"""Provisioning orchestration — company YAML → live Slack apps.

The flow (driven by ``crewlet slack provision``):

1. :func:`build_agent_plans` reads the Tier B company config and turns
   every Slack-enabled agent seat (``role.integrations.slack`` with
   ``${VAR}`` credential placeholders) into a :class:`SlackAgentPlan`:
   the handle, the env-var names the YAML references, and the canonical
   app manifest.
2. :func:`resolve_config_token` obtains a usable app-configuration
   token.  Rotation is expiry-aware: the persisted access token is
   reused while fresh; otherwise ``SLACK_CONFIG_REFRESH_TOKEN`` is
   rotated and the new pair (plus its expiry) is persisted immediately —
   rotation invalidates the old refresh token, so the write happens
   before any other work.
3. :func:`run_provision` executes the plans: ``apps.manifest.update``
   for ledgered apps whose manifest changed (byte-identical manifests
   are skipped — the endpoints are Tier 1 rate-limited),
   ``apps.manifest.create`` for new ones (recording the once-only
   credentials), then — unless skipped — the one manual step Slack
   keeps: an authorize click per app, whose code is exchanged for the
   ``xoxb-`` bot token.  A failure on one agent is recorded as a failed
   outcome and the remaining agents still run.  Every secret lands in
   the :class:`~crewlet.slack.envfile.EnvStore` under the exact
   ``${VAR}`` names the YAML references, so ``crewlet run`` picks them
   up with zero copy-pasting.

All environment state flows through the :class:`EnvStore` (one file,
read and written; file values win over stale shell exports) — this
module never reads or mutates ``os.environ``.

The interactive part (printing the authorize URL, reading the pasted
code) is injected as the ``request_install_code`` callback so this
module stays testable and print-free.
"""

from __future__ import annotations

import hashlib
import json
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlsplit

from crewlet._logging import get_logger
from crewlet.config import CompanyConfig, config_to_organization, env_var_reference
from crewlet.slack.api import SlackApiError, SlackManifestClient
from crewlet.slack.envfile import EnvStore
from crewlet.slack.manifest import (
    build_agent_manifest,
    build_authorize_url,
    oauth_redirect_url,
)
from crewlet.slack.state import ProvisionState, SlackAppRecord, save_state

logger = get_logger("slack.provision")

#: Env vars carrying the app-configuration token pair (generated once at
#: api.slack.com/apps → "Your App Configuration Tokens", then rotated
#: automatically when near expiry) plus the recorded expiry timestamp.
CONFIG_TOKEN_VAR = "SLACK_CONFIG_TOKEN"
CONFIG_REFRESH_TOKEN_VAR = "SLACK_CONFIG_REFRESH_TOKEN"
CONFIG_TOKEN_EXPIRES_VAR = "SLACK_CONFIG_TOKEN_EXPIRES_AT"

#: Rotate this many seconds before the recorded expiry.  One full
#: provisioning run — rate-limit waits included (the API client's
#: per-call budget is 180 s) — fits comfortably inside, so a token
#: considered fresh here cannot expire mid-run.
_TOKEN_EXPIRY_MARGIN_SECONDS = 900

#: ``apps.manifest.update`` errors meaning the recorded app no longer
#: exists in Slack (deleted from the console) — fall back to create.
_APP_GONE_ERRORS = frozenset({"app_not_found", "invalid_app_id", "invalid_app"})

#: Config-token-level errors: every subsequent call would fail the same
#: way, so these abort the run (the CLI force-rotates and retries once)
#: instead of being recorded as a single agent's failure.
FATAL_TOKEN_ERRORS = frozenset({"token_expired", "invalid_auth", "not_authed"})


class SlackProvisionError(RuntimeError):
    """A provisioning-level failure with an operator-actionable message."""


@dataclass
class SlackAgentPlan:
    """Everything needed to provision one agent's Slack app."""

    handle: str
    role_name: str
    bot_token_var: str
    signing_secret_var: str
    manifest: dict[str, Any]


@dataclass
class ProvisionOutcome:
    """What happened for one agent during a provisioning run."""

    handle: str
    app_id: str
    action: str
    """``created``, ``updated``, ``unchanged``, or ``failed``."""
    installed: bool
    """Whether a fresh OAuth install ran (a new bot token was written)."""
    notes: list[str] = field(default_factory=list)
    error: str = ""
    """The failure message when ``action == "failed"`` (other agents
    still ran; re-running resumes this one)."""


def manifest_fingerprint(manifest: dict[str, Any]) -> str:
    """Stable SHA-256 over a manifest, for change detection."""
    canonical = json.dumps(manifest, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def build_agent_plans(
    config: CompanyConfig,
    *,
    base_url: str,
    only_handles: set[str] | None = None,
) -> tuple[list[SlackAgentPlan], list[tuple[str, str]]]:
    """Turn a :class:`~crewlet.config.CompanyConfig` into per-agent plans.

    Returns ``(plans, skipped)`` where ``skipped`` pairs each
    Slack-enabled seat that is NOT provisionable with the reason —
    literal credentials (a manually managed app) or a bot-token-only
    outbound-only identity.  (A mixed placeholder/literal pair can't
    reach here: ``RoleSlackIdentity`` rejects it at validation.)  Seats
    with no ``integrations.slack`` at all are not Slack-enabled and
    don't appear in either list.

    Raises :class:`SlackProvisionError` when ``only_handles`` names a
    handle that doesn't exist or isn't Slack-enabled.
    """
    org = config_to_organization(config)
    plans: list[SlackAgentPlan] = []
    skipped: list[tuple[str, str]] = []
    slack_handles: set[str] = set()

    for role in org.all_roles():
        if role.is_human or not role.slack:
            continue
        handle = role.get_handle()
        slack_handles.add(handle)
        if only_handles is not None and handle not in only_handles:
            continue

        # The org projection keeps role.slack values verbatim (the
        # engine resolves ${VAR} only at transport construction), so the
        # placeholder names are still readable here.  env_var_reference
        # is config.py's own resolution grammar — the two sides cannot
        # disagree about what counts as a placeholder.
        bot_token_var = env_var_reference(role.slack.get("bot_token", ""))
        signing_secret_var = env_var_reference(role.slack.get("signing_secret", ""))
        if not bot_token_var or not signing_secret_var:
            skipped.append(
                (
                    handle,
                    "not provisionable — provisioning needs BOTH bot_token "
                    "and signing_secret as whole-value ${VAR} placeholders; "
                    "literal values (a manually managed app) and "
                    "bot-token-only outbound-only identities are left "
                    "untouched",
                )
            )
            continue

        plans.append(
            SlackAgentPlan(
                handle=handle,
                role_name=role.name,
                bot_token_var=bot_token_var,
                signing_secret_var=signing_secret_var,
                manifest=build_agent_manifest(
                    role_name=role.name, handle=handle, base_url=base_url
                ),
            )
        )

    if only_handles is not None:
        unknown = only_handles - slack_handles
        if unknown:
            raise SlackProvisionError(
                f"unknown or non-Slack-enabled handle(s): {', '.join(sorted(unknown))}"
                f" — Slack-enabled seats: {', '.join(sorted(slack_handles)) or 'none'}"
            )
    return plans, skipped


async def resolve_config_token(
    client: SlackManifestClient,
    env: EnvStore,
    *,
    force_rotate: bool = False,
) -> str:
    """Obtain a usable app-configuration token, rotating only when needed.

    Config access tokens live 12 hours.  The persisted token is reused
    while its recorded expiry (``SLACK_CONFIG_TOKEN_EXPIRES_AT``) is
    comfortably in the future — so previews and quick re-runs don't
    consume a rotation.  Otherwise the refresh token mints a fresh pair
    via ``tooling.tokens.rotate`` and the pair + expiry is persisted
    immediately: rotation invalidates the old refresh token, so the
    write must land before any other work.

    A token with *unknown* expiry (the manually generated bootstrap
    pair) is used as-is; if it turns out to be expired, the caller
    retries once with ``force_rotate=True`` on a
    :data:`FATAL_TOKEN_ERRORS` failure.
    """
    token = env.get(CONFIG_TOKEN_VAR)
    if token and not force_rotate:
        expires_raw = env.get(CONFIG_TOKEN_EXPIRES_VAR)
        try:
            expires_at = int(expires_raw)
        except (TypeError, ValueError):
            expires_at = 0
        if not expires_at or time.time() < expires_at - _TOKEN_EXPIRY_MARGIN_SECONDS:
            return token

    refresh = env.get(CONFIG_REFRESH_TOKEN_VAR)
    if refresh:
        try:
            pair = await client.rotate_config_token(refresh)
        except SlackApiError as exc:
            if token and not force_rotate:
                logger.warning(
                    "slack_config_token_rotate_failed_using_access_token",
                    error=exc.error,
                )
                return token
            raise SlackProvisionError(
                f"could not rotate {CONFIG_REFRESH_TOKEN_VAR} ({exc.error}) — "
                "generate a new configuration token at "
                "https://api.slack.com/apps (“Your App Configuration "
                "Tokens”) and set both values"
            ) from exc
        await env.set(
            {
                CONFIG_TOKEN_VAR: pair.token,
                CONFIG_REFRESH_TOKEN_VAR: pair.refresh_token,
                CONFIG_TOKEN_EXPIRES_VAR: str(pair.expires_at),
            }
        )
        return pair.token

    if token:
        return token

    raise SlackProvisionError(
        f"no {CONFIG_REFRESH_TOKEN_VAR} or {CONFIG_TOKEN_VAR} found in "
        f"{env.path} or the environment — generate an app configuration "
        "token at https://api.slack.com/apps (“Your App Configuration "
        "Tokens”, any workspace member can) and set both values; "
        "provisioning rotates and persists them automatically from then on"
    )


def parse_install_code(pasted: str) -> str:
    """Extract the OAuth code from whatever the operator pasted.

    Accepts the bare code, the full redirect URL from the browser's
    address bar, or a ``code=…`` query fragment.
    """
    text = (pasted or "").strip().strip("'\"")
    if not text:
        raise SlackProvisionError("empty install code")
    if "code=" in text:
        query = urlsplit(text).query or text
        codes = parse_qs(query).get("code", [])
        if not codes or not codes[0]:
            raise SlackProvisionError(f"no code= value found in: {text!r}")
        return codes[0]
    return text


async def _provision_agent(
    plan: SlackAgentPlan,
    *,
    client: SlackManifestClient,
    config_token: str,
    state: ProvisionState,
    state_path: Path,
    env: EnvStore,
    pending_env: dict[str, str],
    redirect_url: str,
    skip_install: bool,
    reinstall: bool,
    request_install_code: Callable[[SlackAgentPlan, str], Awaitable[str]] | None,
) -> ProvisionOutcome:
    """Provision one agent; raises on failure (caller records it)."""
    notes: list[str] = []
    record = state.apps.get(plan.handle)
    fingerprint = manifest_fingerprint(plan.manifest)

    # ── Manifest: skip if unchanged, update in place, or create ──────
    action = "unchanged"
    if record is not None and record.manifest_hash != fingerprint:
        try:
            await client.update_app(config_token, record.app_id, plan.manifest)
            record.manifest_hash = fingerprint
            save_state(state_path, state)
            action = "updated"
        except SlackApiError as exc:
            if exc.error not in _APP_GONE_ERRORS:
                raise
            notes.append(
                f"recorded app {record.app_id} no longer exists in Slack — "
                "created a replacement"
            )
            record = None
    if record is None:
        creds = await client.create_app(config_token, plan.manifest)
        record = SlackAppRecord(
            app_id=creds.app_id,
            client_id=creds.client_id,
            client_secret=creds.client_secret,
            signing_secret=creds.signing_secret,
            manifest_hash=fingerprint,
        )
        state.apps[plan.handle] = record
        save_state(state_path, state)
        action = "created"

    # ── Signing secret → env store ───────────────────────────────────
    if record.signing_secret:
        if env.get(plan.signing_secret_var) != record.signing_secret:
            pending_env[plan.signing_secret_var] = record.signing_secret
    elif not env.get(plan.signing_secret_var):
        notes.append(
            f"signing secret unknown for app {record.app_id} — copy it from "
            f"the app's Basic Information page into {plan.signing_secret_var}"
        )

    # ── OAuth install → bot token ────────────────────────────────────
    # A freshly created app ALWAYS installs: any existing env value is a
    # leftover token from a previous (deleted/replaced) app and satisfying
    # the presence check with it would report success while the engine
    # runs on a dead credential.
    installed = False
    has_bot_token = bool(env.get(plan.bot_token_var))
    needs_install = action == "created" or reinstall or not has_bot_token
    if skip_install:
        if needs_install:
            notes.append(
                f"install skipped — {plan.bot_token_var} "
                + ("needs a fresh token" if has_bot_token else "is still unset")
            )
    elif needs_install:
        if not record.client_id or not record.client_secret:
            raise SlackProvisionError(
                f"ledger entry for {plan.handle} (app {record.app_id}) has no "
                "client_id/client_secret, so the OAuth install cannot run — "
                "for a hand-adopted app, copy both from the app's Basic "
                "Information page into slack-apps.json"
            )
        # Make the staged signing secret durable BEFORE the interactive
        # wait: if the exchange fails or is abandoned, the engine can
        # still verify this agent's webhooks.
        await env.set(pending_env)
        pending_env.clear()
        authorize_url = build_authorize_url(
            client_id=record.client_id,
            redirect_url=redirect_url,
            state=plan.handle,
        )
        assert request_install_code is not None  # caller guarantees
        pasted = await request_install_code(plan, authorize_url)
        code = parse_install_code(pasted)
        result = await client.exchange_oauth_code(
            code=code,
            client_id=record.client_id,
            client_secret=record.client_secret,
            redirect_url=redirect_url,
        )
        if result.app_id and result.app_id != record.app_id:
            raise SlackProvisionError(
                f"OAuth code for {plan.handle} belongs to app "
                f"{result.app_id}, expected {record.app_id} — the approve "
                "click used a different app's URL; re-run and use the URL "
                "printed for this agent"
            )
        if has_bot_token and action == "created":
            notes.append(
                f"replaced a stale {plan.bot_token_var} left over from a previous app"
            )
        record.bot_user_id = result.bot_user_id
        record.team_id = result.team_id
        save_state(state_path, state)
        # A freshly minted token is written immediately — it exists
        # nowhere else.
        await env.set({plan.bot_token_var: result.bot_token})
        installed = True

    logger.info(
        "slack_agent_provisioned",
        handle=plan.handle,
        app_id=record.app_id,
        action=action,
        installed=installed,
    )
    return ProvisionOutcome(
        handle=plan.handle,
        app_id=record.app_id,
        action=action,
        installed=installed,
        notes=notes,
    )


async def run_provision(
    plans: list[SlackAgentPlan],
    *,
    client: SlackManifestClient,
    config_token: str,
    state: ProvisionState,
    state_path: Path,
    env: EnvStore,
    base_url: str,
    skip_install: bool = False,
    reinstall: bool = False,
    request_install_code: Callable[[SlackAgentPlan, str], Awaitable[str]] | None = None,
) -> list[ProvisionOutcome]:
    """Execute *plans* against Slack, persisting state + secrets as it goes.

    The ledger (*state*/*state_path*) and the env store are saved after
    every mutation that would be unrecoverable (app creation, a freshly
    minted bot token), so an interrupted run resumes where it stopped.
    Signing-secret-only writes are batched into one flush at the end —
    they are already durable in the ledger.

    One agent's failure does not abort the rest: it is recorded as a
    ``failed`` outcome and the loop continues.  The exception is a
    config-token-level error (:data:`FATAL_TOKEN_ERRORS`), which would
    fail every remaining call identically and is re-raised for the
    caller to rotate and retry.
    """
    if not skip_install and request_install_code is None:
        raise SlackProvisionError(
            "request_install_code is required unless skip_install is set"
        )

    redirect_url = oauth_redirect_url(base_url)
    outcomes: list[ProvisionOutcome] = []
    pending_env: dict[str, str] = {}

    try:
        for plan in plans:
            try:
                outcome = await _provision_agent(
                    plan,
                    client=client,
                    config_token=config_token,
                    state=state,
                    state_path=state_path,
                    env=env,
                    pending_env=pending_env,
                    redirect_url=redirect_url,
                    skip_install=skip_install,
                    reinstall=reinstall,
                    request_install_code=request_install_code,
                )
            except (SlackApiError, SlackProvisionError) as exc:
                if isinstance(exc, SlackApiError) and exc.error in FATAL_TOKEN_ERRORS:
                    raise
                logger.error(
                    "slack_agent_provision_failed",
                    handle=plan.handle,
                    error=str(exc),
                )
                existing = state.apps.get(plan.handle)
                outcomes.append(
                    ProvisionOutcome(
                        handle=plan.handle,
                        app_id=existing.app_id if existing else "",
                        action="failed",
                        installed=False,
                        error=str(exc),
                    )
                )
                continue
            outcomes.append(outcome)
    finally:
        # Batched signing-secret writes (already durable in the ledger)
        # land in one flush — including on the fatal-token-error path.
        await env.set(pending_env)
        pending_env.clear()

    return outcomes
