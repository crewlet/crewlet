"""``crewlet gitlab provision`` — CLI entry for GitLab provisioning.

Loads the company YAML, resolves the ``integrations.gitlab`` block, and
runs the :func:`crewlet.gitlab.provision.provision` reconcile with an
operator credential. Token values are written to an env file (default) or
printed for shell ``eval``. See ``docs/integrations/gitlab.md``.
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import os
import secrets
from pathlib import Path
from typing import Any

from crewlet._env import load_env_file
from crewlet._logging import get_logger
from crewlet.db.secret_values import SecretStoreError
from crewlet.gitlab.client import GitLabClient, GitLabProvisionError
from crewlet.gitlab.provision import (
    GitLabProvisionAborted,
    ProvisionReport,
    provision,
)
from crewlet.provisioning import (
    TokenSink,
    add_sink_arguments,
    open_token_sink,
    referenced_env_vars,
)

logger = get_logger("gitlab.provision_cli")


def _generate_signing_secret() -> str:
    """A Standard-Webhooks signing secret: ``whsec_`` + base64 of 32 bytes.

    GitLab's webhook ``signing_token`` must be ``whsec_<base64>`` encoding a
    32-byte key (see the group/project webhooks API). ``secrets.token_bytes``
    is the CSPRNG source.
    """
    return "whsec_" + base64.b64encode(secrets.token_bytes(32)).decode("ascii")


async def _maybe_generate_signing_secret(
    gitlab_raw: Any, *, creating_hooks: bool, sink: TokenSink
) -> str | None:
    """Mint a webhook signing secret into the config's ``${VAR}`` if missing.

    Only when the run will actually create webhooks (``creating_hooks``) and
    the configured ``signing_secret`` is an **unset** ``${VAR}``: generate a
    ``whsec_`` value, export it into ``os.environ`` so config resolution +
    the hook's ``signing_token`` both pick it up, and record it to the sink
    so the engine's env file carries the same value the hook is stamped
    with. An already-provisioned value (in the sink file or the environment)
    is reused — never regenerated — so re-runs keep the hook and the engine
    in sync. Returns the var name minted, or ``None`` when nothing was done.
    """
    if gitlab_raw is None or not creating_hooks:
        return None
    raw = str(getattr(gitlab_raw, "signing_secret", "") or "")
    referenced = referenced_env_vars({"signing_secret": raw})
    if not referenced:
        return None  # a literal secret (or none) — nothing to mint into
    var = referenced[0]
    # These os.environ writes are intra-process PRIMING, not persistence:
    # _resolve_gitlab_config runs a few lines later in this same process
    # and must see the value. Durability is the sink's job — the env write
    # dies with the process.
    existing = sink.existing(var)
    if existing:
        # Reuse the persisted/exported value; ensure resolution sees it.
        os.environ[var] = existing
        return None
    value = _generate_signing_secret()
    os.environ[var] = value
    await sink.record(var, value)
    logger.info("gitlab_provision_signing_secret_generated", var=var)
    print(
        f"No {var} was set — generated a webhook signing secret and recorded "
        "it to the token sink. Source that into the engine's env so it "
        "verifies inbound webhooks with the same value the hook is signed with."
    )
    return var


def add_gitlab_parser(sub: argparse._SubParsersAction) -> None:
    """Register the ``gitlab`` subcommand group on the top-level parser."""
    gitlab_p = sub.add_parser(
        "gitlab",
        help="Provision per-agent GitLab service accounts, tokens, and webhooks",
    )
    gitlab_sub = gitlab_p.add_subparsers(dest="gitlab_command")

    prov = gitlab_sub.add_parser(
        "provision",
        help=(
            "Reconcile the company config into GitLab: one service account "
            "per agent seat, membership, per-agent PATs, and project webhooks"
        ),
    )
    prov.add_argument("config", type=Path, help="Path to the Tier B company YAML")
    prov.add_argument(
        "--provision-token",
        default=None,
        help=(
            "Operator credential: a top-level group Owner PAT (GitLab.com) "
            "or admin PAT (self-managed). Falls back to $GITLAB_PROVISION_TOKEN "
            "then $GITLAB_ADMIN_TOKEN."
        ),
    )
    prov.add_argument(
        "--mode",
        choices=["group", "instance"],
        default="group",
        help=(
            "Create accounts under the top-level group (default; group-Owner "
            "callable on GitLab.com) or instance-wide (self-managed admin)"
        ),
    )
    prov.add_argument(
        "--webhook-url",
        default=None,
        help=(
            "Engine webhook endpoint to register on the group/projects "
            "(e.g. https://engine.example.com/webhooks/gitlab). Omit to skip "
            "webhook registration."
        ),
    )
    add_sink_arguments(prov, default_env_file=".env.gitlab")
    prov.add_argument(
        "--rotate",
        action="store_true",
        help="Rotate each managed service-account token (re-mints with a fresh expiry)",
    )
    prov.add_argument(
        "--decommission-removed",
        action="store_true",
        help=(
            "Delete service accounts whose seats left the config (soft delete). "
            "Requires provisioning.username_prefix to scope managed accounts safely."
        ),
    )
    prov.add_argument(
        "--token-expiry-days",
        type=int,
        default=364,
        help=(
            "Expiry for minted/rotated tokens (default 364; GitLab.com Free max "
            "is 365). 0 omits expires_at so the instance default/max applies."
        ),
    )


def _unset_env_refs(data: Any) -> list[str]:
    """Return sorted ``${VAR}`` names in ``data`` that are unset/empty in env.

    Walks the raw (unresolved) config structure and collects every
    ``${VAR}`` reference that resolves to nothing — these are what silently
    become ``""`` and then trip a required-field validator. Used to name
    the culprit for the operator.

    Resolution is asked of the ENGINE (secret store, then environment),
    not of ``os.environ`` directly: with ``--secret-store`` a var minted
    into the table on a previous run is provisioned, and naming it here
    would send the operator chasing an export they do not need.
    """
    from crewlet.config import _ENV_VAR_PATTERN
    from crewlet.secrets.resolver import resolve_env

    found: set[str] = set()

    def _walk(node: Any) -> None:
        if isinstance(node, dict):
            for v in node.values():
                _walk(v)
        elif isinstance(node, list):
            for item in node:
                _walk(item)
        elif isinstance(node, str):
            for match in _ENV_VAR_PATTERN.finditer(node):
                if not resolve_env(match.group(1)):
                    found.add(match.group(1))

    _walk(data)
    return sorted(found)


class GitLabConfigError(Exception):
    """Raised when the resolved ``integrations.gitlab`` block is invalid.

    Carries a human-readable, actionable message (built by
    :func:`_resolve_gitlab_config`) rather than the raw pydantic
    ``ValidationError`` traceback.
    """


def _resolve_gitlab_config(cfg: Any) -> Any:
    """Return the ``integrations.gitlab`` block with ``${VAR}`` resolved.

    Only the integration block is resolved — the org's ``mcp_env`` must
    stay verbatim so ``referenced_env_vars`` can see each seat's
    ``${GITLAB_TOKEN_*}`` reference and mint into it.

    Raises :class:`GitLabConfigError` with an actionable message when the
    resolved block fails validation — most commonly because a ``${VAR}``
    reference (e.g. ``${GITLAB_SIGNING_SECRET}``) resolved to an empty
    string, which then trips a required-field validator.
    """
    from pydantic import ValidationError

    from crewlet.config import GitLabConfig, _resolve_env_recursive

    gitlab = cfg.integrations.gitlab
    if gitlab is None:
        return None
    raw = gitlab.model_dump(exclude_none=True)
    resolved = _resolve_env_recursive(raw)
    try:
        return GitLabConfig.model_validate(resolved)
    except ValidationError as exc:
        reasons = "; ".join(
            f"{'.'.join(str(p) for p in err['loc'])}: {err['msg']}"
            for err in exc.errors()
        )
        unset = _unset_env_refs(raw)
        hint = ""
        if unset:
            hint = (
                "\nUnset or empty environment variables referenced by "
                "integrations.gitlab:\n"
                + "\n".join(f"  - {name}" for name in unset)
                + "\nExport these (e.g. in your --env-file) before provisioning. "
                "GITLAB_SIGNING_SECRET must be the 19.1+ webhook signing token "
                "(a 'whsec_…' value)."
            )
        raise GitLabConfigError(
            f"integrations.gitlab config is invalid — {reasons}.{hint}"
        ) from exc


def cmd_gitlab_provision(args: Any) -> int:
    """Run the GitLab provisioning reconcile."""
    config_path: Path = args.config
    load_env_file(config_path)
    if not config_path.exists():
        logger.error("gitlab_provision_config_missing", path=str(config_path))
        return 1

    from crewlet.config import config_to_organization, load_company_config

    cfg = load_company_config(config_path)
    gitlab_raw = getattr(cfg.integrations, "gitlab", None)
    if gitlab_raw is None or not getattr(gitlab_raw, "enabled", False):
        logger.error("gitlab_provision_not_enabled")
        print("integrations.gitlab is not enabled in this config.")
        return 1

    # Everything from here runs in one event loop: the sink may be
    # DB-backed, and minting the webhook signing secret writes through it
    # before the config block is resolved and validated.
    async def _run() -> ProvisionReport:
        # The sink is opened up front because auto-generating a missing
        # webhook signing secret mints into it, the same contract as seat
        # tokens.
        sink: TokenSink
        sink, db = await open_token_sink(args, source="gitlab-provision")
        try:
            # When we'll create webhooks but no signing secret was
            # provided, mint one so the hook and the engine share it —
            # before resolving/validating.
            await _maybe_generate_signing_secret(
                gitlab_raw, creating_hooks=bool(args.webhook_url), sink=sink
            )

            gitlab_config = _resolve_gitlab_config(cfg)
            if (
                gitlab_config.provisioning is None
                or not gitlab_config.provisioning.group
            ):
                raise SystemExit(
                    "integrations.gitlab.provisioning.group is required to provision."
                )

            token = (
                args.provision_token
                or os.environ.get("GITLAB_PROVISION_TOKEN")
                or os.environ.get("GITLAB_ADMIN_TOKEN")
                or ""
            )
            if not token:
                raise SystemExit(
                    "No operator credential. Pass --provision-token or set "
                    "$GITLAB_PROVISION_TOKEN (a group-Owner PAT on GitLab.com)."
                )

            org = config_to_organization(cfg)

            async with GitLabClient(gitlab_config.api_base, token) as client:
                # Probe the operator credential up front for a clear error.
                try:
                    me = await client.get_current_user()
                    logger.info("gitlab_provision_as", username=me.get("username", ""))
                except GitLabProvisionError as exc:
                    raise SystemExit(
                        f"Operator credential rejected by "
                        f"{gitlab_config.api_base}/user ({exc.status}). Check "
                        f"the token and its scopes (needs 'api')."
                    ) from exc
                return await provision(
                    client,
                    org,
                    gitlab_config,
                    webhook_url=args.webhook_url or "",
                    sink=sink,
                    mode=args.mode,
                    rotate=bool(args.rotate),
                    decommission_removed=bool(args.decommission_removed),
                    token_expiry_days=int(args.token_expiry_days),
                    # RAW (unresolved) token value — its ${VAR} reference is
                    # the contract for minting the engine's read-only
                    # participants-lookup account (see provision()).
                    engine_token_ref=str(cfg.integrations.gitlab.token or ""),
                )
        finally:
            if db is not None:
                await db.close()

    try:
        report = asyncio.run(_run())
    except GitLabConfigError as exc:
        logger.error("gitlab_provision_config_invalid", error=str(exc))
        print(str(exc))
        return 1
    except SecretStoreError as exc:
        logger.error("gitlab_provision_secret_store_unavailable", error=str(exc))
        print(f"Cannot use the secret store: {exc}")
        return 1
    except SystemExit as exc:
        print(str(exc))
        return 1
    except GitLabProvisionAborted as exc:
        logger.error("gitlab_provision_aborted", error=str(exc))
        print(f"Provisioning cannot proceed: {exc}")
        return 1
    except GitLabProvisionError as exc:
        logger.error("gitlab_provision_failed", error=str(exc))
        print(f"Provisioning failed: {exc}")
        return 1

    _print_report(report, printed_tokens=args.print_tokens)
    return 0 if all(s.account_action != "error" for s in report.seats) else 1


def _print_report(report: ProvisionReport, *, printed_tokens: bool) -> None:
    print("\nGitLab provisioning report")
    print("=" * 40)
    for seat in report.seats:
        line = (
            f"  {seat.username:<28} account={seat.account_action:<8} "
            f"member={seat.membership:<8} token={seat.token_action}"
        )
        if seat.error:
            line += f"  ERROR: {seat.error}"
        print(line)
    if report.hooks:
        print(f"\n  webhooks: {', '.join(report.hooks)}")
    if report.decommissioned:
        print(f"  decommissioned: {', '.join(report.decommissioned)}")
    for note in report.notes:
        print(f"  note: {note}")
    if not printed_tokens:
        minted = [s for s in report.seats if s.token_action in ("minted", "rotated")]
        if minted:
            print(
                f"\n  {len(minted)} token(s) written to the env file. Source it "
                "before starting the engine."
            )
