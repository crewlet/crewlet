"""Assemble the sandbox run env — TOOL-AGNOSTIC engine facts only.

The engine contributes exactly two kinds of env, neither specific to any
external tool: the LLM creds the role already uses (its resolved
``providers.llm`` entry — the engine chose the model, so it threads the
credential) and the agent's IDENTITY (``CREWLET_AGENT_HANDLE`` /
``CREWLET_AGENT_EMAIL`` — per-launch facts static config cannot know, which
setup-step recipes map into whatever their tool needs, e.g. the git-auth
recipe's ``git config user.name "$CREWLET_AGENT_HANDLE"``).

Everything tool-specific comes from config: external-service tokens
(``GITHUB_TOKEN`` and friends) are declared in ``role.sandbox.env`` /
setup-step ``env`` and merely ``${VAR}``-resolved here — the engine never
extracts, names, or special-cases them. The OTLP ingest token is **never**
injected; only non-secret OTel values are.
"""

from __future__ import annotations

from pathlib import Path

from crewlet._logging import get_logger
from crewlet.config import (
    LLMProviderConfig,
    _resolve_env_value,
    env_reference_is_resolvable,
    resolve_env_vars,
)

logger = get_logger("sandbox.credentials")

# Anthropic-compatible signals that satisfy Claude Code's auth requirement.
# ``CLAUDE_CODE_OAUTH_TOKEN`` is the SUBSCRIPTION credential (Claude
# Pro/Max, minted by ``crewlet llm login --capture-token``): it is a
# first-class headless auth path, not a lesser one, so a role backed by a
# ``cli-agent`` provider is fully credentialled without any API key.
_CLAUDE_AUTH_KEYS = (
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
    "CLAUDE_CODE_OAUTH_TOKEN",
    "CLAUDE_CODE_USE_BEDROCK",
    "CLAUDE_CODE_USE_VERTEX",
    "CLAUDE_CODE_USE_FOUNDRY",
)


class SandboxCredentialError(ValueError):
    """The role's config can't satisfy the chosen coding agent's creds."""


def _cli_agent_env(llm_config: LLMProviderConfig) -> dict[str, str]:
    """Credential env for a ``cli-agent`` provider, for the sandbox.

    A subscription backend has no API key — but the vendors that offer a
    **headless token** (Claude Code's ``CLAUDE_CODE_OAUTH_TOKEN``, minted
    by ``crewlet llm login --capture-token``) hand out exactly the kind
    of credential that travels: one variable, scoped, revocable. Exported
    here, the coding agent inside the sandbox authenticates on the same
    subscription the role's LLM already uses, with no API key anywhere.

    The credential *files* deliberately do not travel. They carry a
    refresh token whose rotation is shared fleet state, and pushing that
    into a remote VM is a materially bigger trust step than a scoped
    token. A local sandbox (``providers.sandbox.type: local``) runs on
    the engine host and reads the same credential directory directly, so
    it needs neither.
    """
    cli = llm_config.cli
    if cli is None:  # pragma: no cover — validation guarantees the block
        return {}
    profile = cli.profile()
    env: dict[str, str] = {}
    if cli.auth.mode == "api-key":
        keys = llm_config.resolved_keys()
        if profile.api_key_env and keys:
            env[profile.api_key_env] = keys[0]
        return env
    if not profile.token_env:
        return env
    # Same two sources, same precedence, as the LLM provider itself: the
    # explicit ${VAR} reference if the operator wrote one, else the
    # profile's conventional variable from the secret store / environment.
    token = cli.auth.token or f"${{{profile.token_env}}}"
    env[profile.token_env] = token
    return env


def cli_credential_files(
    provider_key: str, llm_config: LLMProviderConfig
) -> dict[str, str]:
    """The ``cli-agent`` login's files: box-relative path → host path.

    Handed to the sandbox provider on the :class:`SandboxSpec` so a
    **local** box runs the coding agent against the very login
    ``crewlet llm login`` established — the point of "code with the same
    agent". Returns ``{}`` for any other provider type, and for a CLI
    whose auth is a token rather than files.

    Deliberately *not* consumed by the E2B provider: see
    :attr:`crewlet.sandbox.protocol.SandboxSpec.credential_files`.
    """
    if llm_config.type != "cli-agent" or llm_config.cli is None:
        return {}
    from crewlet.providers.llm.cli_workspace import default_state_dir

    cli = llm_config.cli
    profile = cli.profile()
    if not profile.credential_paths:
        return {}
    state_dir = str(_resolve_env_value(cli.state_dir)) if cli.state_dir else ""
    root = (
        Path(state_dir).expanduser() if state_dir else default_state_dir(provider_key)
    ) / "credentials"
    return {relative: str(root / relative) for relative in profile.credential_paths}


def _llm_env(coding_agent: str, llm_config: LLMProviderConfig) -> dict[str, str]:
    """Env vars that point the coding agent at the role's LLM provider.

    Claude Code only speaks the Anthropic API, so a non-Anthropic provider
    yields nothing here -- the caller's validation then requires an explicit
    key in ``role.sandbox.env``. OpenCode is provider-agnostic.
    """
    if llm_config.type == "cli-agent":
        return _cli_agent_env(llm_config)
    keys = llm_config.resolved_keys()
    api_key = keys[0] if keys else ""
    env: dict[str, str] = {}
    if coding_agent == "claude-code":
        if llm_config.type == "anthropic":
            if api_key:
                env["ANTHROPIC_API_KEY"] = api_key
            if llm_config.base_url:
                env["ANTHROPIC_BASE_URL"] = llm_config.base_url
        # non-anthropic → nothing; role.sandbox.env must supply the key
    else:  # opencode (provider-agnostic)
        anthropic = llm_config.type == "anthropic"
        if api_key:
            # The OpenCode custom provider references this via {env:...}
            # rather than inlining the secret (see opencode.to_opencode_provider).
            env["ANTHROPIC_API_KEY" if anthropic else "OPENAI_API_KEY"] = api_key
        # Also export the endpoint. NOTE: for OpenCode a *_BASE_URL env does
        # NOT redirect a built-in provider — the real redirect is the custom
        # provider the runner writes into opencode.json. This is
        # kept for parity / co-located collectors; it's harmless when the
        # custom provider's baseURL is what OpenCode actually uses.
        if llm_config.base_url:
            env["ANTHROPIC_BASE_URL" if anthropic else "OPENAI_BASE_URL"] = (
                llm_config.base_url
            )
    return env


def build_sandbox_env(
    *,
    coding_agent: str,
    llm_config: LLMProviderConfig,
    role_sandbox_env: dict[str, str] | None = None,
    agent_handle: str = "",
    agent_email: str = "",
    setup_env: dict[str, str] | None = None,
    otel_env: dict[str, str] | None = None,
) -> dict[str, str]:
    """Assemble + resolve the full env injected into the sandbox.

    The engine contributes only tool-agnostic facts: the LLM creds and the
    agent's identity as ``CREWLET_AGENT_HANDLE`` / ``CREWLET_AGENT_EMAIL``
    (per-launch values static config cannot know — setup-step recipes map
    them into tool-specific shape, e.g. git commit identity). Every
    tool-specific variable (``GITHUB_TOKEN``, registry tokens, …) comes
    from ``setup_env`` (the merged step contributions,
    :func:`crewlet.sandbox.setup.setup_env`) or ``role.sandbox.env``.

    Precedence (later wins): derived LLM creds → agent identity →
    setup-step env → ``role.sandbox.env`` overrides → non-secret OTel
    values. ``${ENV}`` references are resolved at the end (like
    ``mcp_env``); any reference whose variable is unset or empty — whether
    the whole value (``"${TOKEN}"``) or embedded (``"Bearer ${TOKEN}"``) —
    logs a ``sandbox_env_unresolved`` warning naming the keys (never the
    values), so a seat whose token env var isn't exported fails loudly
    instead of mysteriously.
    Raises :class:`SandboxCredentialError` when Claude Code is chosen but
    no Anthropic-compatible credential is reachable.
    """
    env: dict[str, str] = {}
    env.update(_llm_env(coding_agent, llm_config))
    if agent_handle:
        env["CREWLET_AGENT_HANDLE"] = agent_handle
    if agent_email:
        env["CREWLET_AGENT_EMAIL"] = agent_email
    env.update(setup_env or {})
    env.update(role_sandbox_env or {})
    if otel_env:
        env.update(otel_env)

    # Flag per REFERENCE, not per final value: an embedded form like
    # "Bearer ${TOKEN}" resolves to a truthy-but-broken "Bearer " when the
    # var is unset, so testing the resolved value would miss exactly the
    # composite shapes role.sandbox.env documents. Keys only — never values,
    # which may embed partial secrets. Resolvability is asked of the engine
    # (secret store, then environment), not of os.environ directly, so a
    # credential that lives only in the store does not read as missing.
    unresolved = sorted(
        k
        for k, v in env.items()
        if isinstance(v, str) and not env_reference_is_resolvable(v)
    )
    if unresolved:
        logger.warning("sandbox_env_unresolved", keys=unresolved)

    resolved = resolve_env_vars(env)

    # Validate AFTER resolution. The pre-resolution dict holds credential
    # values verbatim (``resolved_keys()`` returns "${ANTHROPIC_KEY_1}" as
    # written), and a raw reference is truthy — so a key check against it
    # passed for an UNSET variable and Claude Code launched with an empty
    # credential instead of raising here. Requiring a non-empty resolved
    # value is what makes this function's promise ("raises when no
    # Anthropic-compatible credential is reachable") actually true.
    if coding_agent == "claude-code" and not any(
        resolved.get(k) for k in _CLAUDE_AUTH_KEYS
    ):
        extra = ""
        if llm_config.type == "cli-agent":
            # The subscription CAN reach a remote sandbox — but only as a
            # headless TOKEN, and only for a vendor that mints one. Point
            # at the exact next step instead of the generic API-key advice.
            profile = llm_config.cli.profile() if llm_config.cli else None
            token_env = getattr(profile, "token_env", "")
            if token_env:
                extra = (
                    f" This role's LLM is a 'cli-agent' provider whose "
                    f"{token_env} is not set: a remote sandbox authenticates "
                    "on your subscription with that headless token, so mint "
                    "one with `crewlet llm login <key> --capture-token`. "
                    "(Credential FILES stay on the engine host — use "
                    "providers.sandbox.type 'local' to run the coding agent "
                    "there against them directly.)"
                )
            else:
                extra = (
                    " This role's LLM is a 'cli-agent' provider for a CLI "
                    "that mints no headless token, so a remote sandbox has "
                    "no credential to receive. Use providers.sandbox.type "
                    "'local' to run the coding agent on the engine host "
                    "against that login, or give the sandbox its own key in "
                    "role.sandbox.env."
                )
        raise SandboxCredentialError(
            "coding_agent 'claude-code' needs an Anthropic-compatible "
            f"provider (the resolved sandbox provider is type "
            f"{llm_config.type!r}). Point role.llm_sandbox at an 'anthropic' "
            "providers.llm entry, or set ANTHROPIC_API_KEY in role.sandbox.env. "
            "A ${VAR} reference that resolves to nothing counts as missing." + extra
        )
    return resolved


__all__ = [
    "SandboxCredentialError",
    "build_sandbox_env",
    "cli_credential_files",
]
