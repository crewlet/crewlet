"""Secret-path registry — *which* values in a ``company_config`` payload are
secrets, used to mask them on read and to keep them on write.

The whole payload is encrypted at rest (see
:mod:`crewlet.secrets.document`), so this module never sees the ciphertext.
It operates on the *decrypted* structure for two jobs:

- **Display redaction** (:func:`redact_payload`): mask every secret value
  before the structure is shown (``GET /config`` / dashboard / diff).
- **Write-back** (:func:`restore_redacted`): a client that edited a
  redacted ``GET`` sends markers where secrets belong; swap them back to
  the stored values before re-encrypting.

Both drive off :func:`secret_pointers`, the single source of truth for the
secret locations. It is a **structural** spec over the raw JSON dict rather
than a Pydantic type-marker, for two reasons: the secret-bearing leaves are
free-form maps whose *keys* are operator-chosen (``mcp_env.<server>.<var>``,
``mcp_servers[*].env``), so there is no field to mark; and the readers run
over payloads that are never re-validated — historical revisions read back
by ``config export --redact`` / ``config diff`` may predate the current
model (e.g. a role-level ``slack`` block, which
:class:`~crewlet.config.RoleConfig` no longer accepts). Walking the
structure keeps those covered; a type-driven walk would silently skip them.

Secret locations:

- ``providers.llm.<key>.api_keys[*]``, ``providers.embeddings.api_key``,
  ``providers.sandbox.api_key``
- ``integrations.{jira,confluence}.token`` / ``.webhook_secret``,
  ``integrations.github.webhook_secret``,
  ``integrations.gitlab.token`` / ``.signing_secret``,
  ``integrations.plane.token`` / ``.webhook_secret`` (org-level
  ``integrations.slack`` is an empty enable-marker — no secrets there)
- shared ``mcp_servers[*].env`` / ``.headers`` — free-form dicts, so only
  secret-hinted keys (``*_TOKEN``, ``Authorization``) are masked
  (:func:`_looks_secret`)
- every ``mcp_env.<server>.<var>`` and per-role ``integrations.slack`` /
  ``slack`` bot-token / signing-secret, on every root role and every
  role / unit nested to any depth under ``units``

A value that is still a ``${VAR}`` reference is treated as *not* a
secret-at-rest (it is an inert env-var pointer) and is left visible by
:func:`redact_payload`; only pure-literal secret values are masked.
"""

from __future__ import annotations

import copy
from collections.abc import Mapping
from typing import Any

from crewlet.env_refs import has_env_reference

#: Substrings that mark a free-form dict key (an ``mcp_servers`` env/header
#: var, a notification-transport field) as secret-bearing. Structured,
#: typed secrets (LLM keys, Slack creds, integration tokens) are pinned by
#: explicit pointers below and do not rely on this heuristic.
#:
#: The match is a plain substring test, so it deliberately errs toward
#: **over**-masking (``max_tokens`` matches ``token``): for a secrets
#: feature a masked non-secret is safe, an unmasked secret is a leak.
_SECRET_KEY_HINTS = (
    "password",
    "passwd",
    "secret",
    "token",
    "api_key",
    "apikey",
    "authorization",
    "credential",
    "private_key",
    "access_key",
)


def _looks_secret(key: str) -> bool:
    """True when a free-form dict key name signals a secret value."""
    low = key.lower()
    return any(hint in low for hint in _SECRET_KEY_HINTS)


class SecretLeakError(Exception):
    """Raised by :func:`restore_redacted` when a redaction marker cannot be
    resolved to a stored secret value.

    A fail-closed guard on the write path: a ``{"encrypted": true}`` marker
    from a ``GET`` must never be persisted as if it were a real secret.
    """


# ── JSON-pointer helpers (RFC 6901) ───────────────────────────────────

_MISSING = object()


def _escape(token: str) -> str:
    """Escape a single reference token per RFC 6901 (``~`` → ``~0``, ``/`` → ``~1``)."""
    return token.replace("~", "~0").replace("/", "~1")


def _unescape(token: str) -> str:
    return token.replace("~1", "/").replace("~0", "~")


def _split_pointer(pointer: str) -> list[str]:
    return [_unescape(tok) for tok in pointer.split("/")[1:]]


def pointer_get(payload: Any, pointer: str) -> Any:
    """Return the value at ``pointer`` or the ``_MISSING`` sentinel."""
    cur = payload
    for tok in _split_pointer(pointer):
        if isinstance(cur, list):
            try:
                idx = int(tok)
            except ValueError:
                return _MISSING
            if not (0 <= idx < len(cur)):
                return _MISSING
            cur = cur[idx]
        elif isinstance(cur, Mapping):
            if tok not in cur:
                return _MISSING
            cur = cur[tok]
        else:
            return _MISSING
    return cur


def pointer_set(payload: Any, pointer: str, value: Any) -> None:
    """Set the value at ``pointer`` (parent containers must already exist)."""
    tokens = _split_pointer(pointer)
    cur = payload
    for tok in tokens[:-1]:
        cur = cur[int(tok)] if isinstance(cur, list) else cur[tok]
    last = tokens[-1]
    if isinstance(cur, list):
        cur[int(last)] = value
    else:
        cur[last] = value


# ── structural secret-path spec ───────────────────────────────────────

_SLACK_SECRET_FIELDS = ("bot_token", "signing_secret")


def _emit_mcp_env(mcp_env: Any, base: str, out: list[str]) -> None:
    """Every ``mcp_env.<server>.<var>`` value is a per-agent tool credential."""
    if not isinstance(mcp_env, Mapping):
        return
    for server, env_map in mcp_env.items():
        if not isinstance(env_map, Mapping):
            continue
        for var in env_map:
            out.append(f"{base}/{_escape(str(server))}/{_escape(str(var))}")


def _emit_slack_creds(container: Mapping[str, Any], base: str, out: list[str]) -> None:
    """Slack transport creds live at ``integrations.slack`` (authored form)
    and ``slack`` (loader-materialised form) — cover both, channel excluded."""
    for parent in (
        container.get("integrations"),
        container,
    ):
        if not isinstance(parent, Mapping):
            continue
        slack = parent.get("slack")
        if not isinstance(slack, Mapping):
            continue
        prefix = (
            f"{base}/integrations/slack" if parent is not container else f"{base}/slack"
        )
        for field in _SLACK_SECRET_FIELDS:
            if field in slack:
                out.append(f"{prefix}/{field}")


def _emit_role(role: Any, base: str, out: list[str]) -> None:
    if not isinstance(role, Mapping):
        return
    _emit_mcp_env(role.get("mcp_env"), f"{base}/mcp_env", out)
    _emit_slack_creds(role, base, out)


def _emit_roles(roles: Any, base: str, out: list[str]) -> None:
    if not isinstance(roles, list):
        return
    for i, role in enumerate(roles):
        _emit_role(role, f"{base}/{i}", out)


def _emit_units(units: Any, base: str, out: list[str]) -> None:
    if not isinstance(units, list):
        return
    for i, unit in enumerate(units):
        if not isinstance(unit, Mapping):
            continue
        ubase = f"{base}/{i}"
        _emit_mcp_env(unit.get("mcp_env"), f"{ubase}/mcp_env", out)
        _emit_roles(unit.get("roles"), f"{ubase}/roles", out)
        _emit_units(unit.get("children"), f"{ubase}/children", out)


def _emit_providers(payload: Mapping[str, Any], out: list[str]) -> None:
    providers = payload.get("providers")
    if not isinstance(providers, Mapping):
        return
    llm = providers.get("llm")
    if isinstance(llm, Mapping):
        for key, provider in llm.items():
            if not isinstance(provider, Mapping):
                continue
            api_keys = provider.get("api_keys")
            if isinstance(api_keys, list):
                for i in range(len(api_keys)):
                    out.append(f"/providers/llm/{_escape(str(key))}/api_keys/{i}")
    for name in ("embeddings", "sandbox"):
        section = providers.get(name)
        if isinstance(section, Mapping) and "api_key" in section:
            out.append(f"/providers/{name}/api_key")


def _emit_free_form_secrets(dct: Any, base: str, out: list[str]) -> None:
    """Emit a pointer for each secret-hinted key of a free-form dict.

    Used for the untyped surfaces — ``mcp_servers[].env`` / ``.headers``
    and ``integrations.transports[]`` — which mix non-secret config
    (URLs, hosts, ports, flags) with credentials. Only keys whose name
    signals a secret (:func:`_looks_secret`) are masked, so the visible
    structure keeps its non-secret values.
    """
    if not isinstance(dct, Mapping):
        return
    for key, value in dct.items():
        if _looks_secret(str(key)) and isinstance(value, str):
            out.append(f"{base}/{_escape(str(key))}")


def _emit_integrations(payload: Mapping[str, Any], out: list[str]) -> None:
    integrations = payload.get("integrations")
    if not isinstance(integrations, Mapping):
        return
    # Note: org-level ``integrations.slack`` carries no secrets — it is the
    # transport enable-marker plus the non-sensitive ``typing_status`` mode
    # (``SlackConfig``, ``extra="forbid"``); every Slack credential is
    # per-role and covered by ``_emit_slack_creds``.
    for section, fields in (
        ("jira", ("token", "webhook_secret")),
        ("confluence", ("token", "webhook_secret")),
        ("github", ("webhook_secret",)),
        ("gitlab", ("token", "signing_secret")),
        ("plane", ("token", "webhook_secret")),
    ):
        block = integrations.get(section)
        if isinstance(block, Mapping):
            for field in fields:
                if field in block:
                    out.append(f"/integrations/{section}/{field}")


def _emit_mcp_servers(payload: Mapping[str, Any], out: list[str]) -> None:
    """Shared MCP-server ``env`` / ``headers`` credentials.

    Unlike a role's ``mcp_env`` (every value is a per-agent credential), a
    shared server's ``env`` is mostly non-secret URLs/flags, so only
    secret-hinted keys (a ``*_TOKEN`` env var, an ``Authorization`` header)
    are masked.
    """
    servers = payload.get("mcp_servers")
    if not isinstance(servers, list):
        return
    for i, server in enumerate(servers):
        if not isinstance(server, Mapping):
            continue
        _emit_free_form_secrets(server.get("env"), f"/mcp_servers/{i}/env", out)
        _emit_free_form_secrets(server.get("headers"), f"/mcp_servers/{i}/headers", out)


def secret_pointers(payload: Mapping[str, Any]) -> list[str]:
    """Return the RFC-6901 pointers of every secret value in ``payload``.

    Pointers are emitted only where the enclosing key is present, so the
    result tracks the actual document. Order is stable (providers →
    integrations → transports → mcp_servers → root roles → units,
    depth-first).
    """
    out: list[str] = []
    _emit_providers(payload, out)
    _emit_integrations(payload, out)
    _emit_mcp_servers(payload, out)
    _emit_roles(payload.get("roles"), "/roles", out)
    _emit_units(payload.get("units"), "/units", out)
    return out


# ── redaction (display) + restore (write-back) ────────────────────────


def redact_payload(payload: Mapping[str, Any]) -> dict[str, Any]:
    """Return a deep copy of a *decrypted* payload with secrets masked.

    Every non-empty **literal** secret leaf becomes the structured marker
    ``{"encrypted": true, "key_id": null}``. Applied to the decrypted
    structure before it is shown (``GET`` / dashboard / diff), so the
    caller sees the config's shape but never a secret value.

    A value that still holds a ``${VAR}`` reference is left visible: it is
    an inert pointer (the real secret lives in the environment or the
    encrypted secret store, not in the config), so masking it would both
    hide the useful var name and falsely mark an un-encrypted reference as
    ``encrypted``.

    "Holds a reference" uses the engine's own grammar
    (:mod:`crewlet.env_refs`), so it means "substitution would actually
    change this value".  A looser rule left literal secrets containing
    brace syntax the resolver ignores — say ``${line#host=}`` inside a
    sandbox setup script — displayed in full.
    """
    result = copy.deepcopy(dict(payload))
    for pointer in secret_pointers(result):
        value = pointer_get(result, pointer)
        if isinstance(value, str) and value != "" and not has_env_reference(value):
            pointer_set(result, pointer, {"encrypted": True, "key_id": None})
    return result


def is_redaction_marker(value: Any) -> bool:
    """True if ``value`` is a :func:`redact_payload` marker object."""
    return (
        isinstance(value, dict) and value.get("encrypted") is True and "key_id" in value
    )


def restore_redacted(
    new_payload: Mapping[str, Any],
    old_payload: Mapping[str, Any] | None,
) -> dict[str, Any]:
    """Swap redaction markers in ``new_payload`` for the stored secret values.

    A ``GET /config`` returns secrets as ``{"encrypted": true, ...}``
    markers (never the value). A client that edits a non-secret field and
    ``PUT``s the whole document back would otherwise send those markers
    where a secret belongs — validating a marker object into a ``str``
    field would 400, and a marker landing in an untyped ``mcp_env`` dict
    would be silently stored, corrupting the credential. This runs on
    every write **before** validation: each marker is replaced with the
    value at the same path in the currently-stored (decrypted) payload
    ("keep the existing secret"), so read → edit → write round-trips
    without ever exposing or clobbering a secret.

    Raises :class:`SecretLeakError` if a marker has no stored counterpart
    to keep (e.g. a first write, or a marker at a brand-new secret path) —
    the caller must supply the real value there.
    """
    result = copy.deepcopy(dict(new_payload))
    old = old_payload or {}
    for pointer in secret_pointers(result):
        value = pointer_get(result, pointer)
        if not is_redaction_marker(value):
            continue
        restored = pointer_get(old, pointer)
        if not isinstance(restored, str):
            raise SecretLeakError(
                f"cannot write a redacted secret at {pointer}: no stored "
                f"value to keep — supply the secret value explicitly"
            )
        pointer_set(result, pointer, restored)
    return result


__all__ = [
    "SecretLeakError",
    "is_redaction_marker",
    "pointer_get",
    "pointer_set",
    "redact_payload",
    "restore_redacted",
    "secret_pointers",
]
