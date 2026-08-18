"""``crewlet mattermost provision`` — CLI entry for Mattermost provisioning.

Loads the company YAML, resolves the ``integrations.mattermost``
connection coordinates, and runs the
:func:`crewlet.mattermost.provision.provision` reconcile with a
system-admin personal access token.  Minted bot tokens are written to an
env file (default), the encrypted secret store, or stdout — the same
sink flags every other provisioning CLI takes.

Config handling follows the Plane CLI's per-field resolution rather than
GitLab's whole-model re-validation: ``role.integrations.mattermost.bot_token``
values stay RAW because their ``${VAR}`` references ARE the minting
contract.  Only ``url`` and ``team`` are resolved.
"""

from __future__ import annotations

import argparse
import asyncio
import os
from pathlib import Path
from typing import Any, NamedTuple

from crewlet._env import load_env_file
from crewlet._logging import get_logger
from crewlet.db.secret_values import SecretStoreError
from crewlet.mattermost.client import MattermostClient, MattermostError
from crewlet.mattermost.provision import (
    MattermostProvisionAborted,
    ProvisionReport,
    decommission,
    provision,
)
from crewlet.provisioning import TokenSink, add_sink_arguments, open_token_sink

logger = get_logger("mattermost.provision_cli")

#: Environment variable holding the system-admin token the reconcile
#: authenticates with.  Never read from the company config: it is an
#: operator credential, not part of the company's own identity.
ADMIN_TOKEN_ENV = "MATTERMOST_ADMIN_TOKEN"


class MattermostConfigError(Exception):
    """The ``integrations.mattermost`` block cannot be used.

    Carries an actionable message naming unset ``${VAR}`` references
    rather than surfacing an empty-field symptom.
    """


class _ProvisionTarget(NamedTuple):
    """Resolved connection coordinates for the provisioning run."""

    url: str
    team: str


def _unset_env_refs(data: Any) -> list[str]:
    """Sorted ``${VAR}`` names in *data* that resolve to nothing.

    Resolution is asked of the ENGINE (secret store, then environment),
    not ``os.environ`` directly: with ``--secret-store`` a var minted on
    a previous run is provisioned, and naming it here would send the
    operator chasing an export they do not need.
    """
    from crewlet.config import _ENV_VAR_PATTERN
    from crewlet.secrets.resolver import resolve_env

    found: set[str] = set()

    def _walk(node: Any) -> None:
        if isinstance(node, dict):
            for value in node.values():
                _walk(value)
        elif isinstance(node, list):
            for item in node:
                _walk(item)
        elif isinstance(node, str):
            for match in _ENV_VAR_PATTERN.finditer(node):
                if not resolve_env(match.group(1)):
                    found.add(match.group(1))

    _walk(data)
    return sorted(found)


def _resolve_target(raw: Any) -> _ProvisionTarget:
    """Per-field resolution of the ``integrations.mattermost`` connection."""
    from crewlet.config import resolve_env_scalar

    url = resolve_env_scalar(str(raw.url or "")).strip()
    team = resolve_env_scalar(str(raw.team or "")).strip()
    if url and team:
        return _ProvisionTarget(url=url, team=team)

    missing = [name for name, value in (("url", url), ("team", team)) if not value]
    unset = _unset_env_refs({"url": str(raw.url or ""), "team": str(raw.team or "")})
    hint = ""
    if unset:
        hint = (
            "\nUnset or empty environment variables referenced by "
            "integrations.mattermost:\n"
            + "\n".join(f"  - {name}" for name in unset)
            + "\nExport these (e.g. in your .env) before provisioning."
        )
    raise MattermostConfigError(
        "integrations.mattermost must resolve non-empty "
        f"{' and '.join(missing)} value(s) to provision.{hint}"
    )


def add_mattermost_parser(sub: argparse._SubParsersAction) -> None:
    """Register the top-level ``mattermost`` command group."""
    group = sub.add_parser(
        "mattermost",
        help="Mattermost integration commands (bot seat provisioning)",
    )
    group_sub = group.add_subparsers(dest="mattermost_command")

    prov = group_sub.add_parser(
        "provision",
        help=(
            "Reconcile the company config into Mattermost: one bot account "
            "per agent seat, team + channel membership, and a per-agent "
            "access token minted into the config's own ${VAR}s"
        ),
    )
    prov.add_argument("config", type=Path, help="Path to the Tier B company YAML")
    prov.add_argument(
        "--admin-token",
        type=str,
        default=None,
        help=(
            "System-admin personal access token (default: read from "
            f"${ADMIN_TOKEN_ENV}). Creating bot accounts and minting their "
            "tokens both require system-admin rights."
        ),
    )
    prov.add_argument(
        "--handles",
        type=str,
        default="",
        help="Only provision these agent handles (comma-separated)",
    )
    prov.add_argument(
        "--decommission",
        type=str,
        default="",
        help=(
            "Disable the bot accounts for these handles (comma-separated) "
            "and revoke their tokens, instead of provisioning. The accounts "
            "keep their message history and can be re-enabled by a later "
            "provision run — but the revoked token is gone, so a restored "
            "seat is minted a fresh one."
        ),
    )
    prov.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Print the plan without creating or modifying anything. "
            "Applies to --decommission too."
        ),
    )
    add_sink_arguments(prov, default_env_file=".env")

    doc = group_sub.add_parser(
        "doctor",
        help=(
            "Check a Mattermost install end to end: reachability, the "
            "Site URL every browser inherits, a browser-shaped websocket "
            "upgrade, and one real authenticated socket per agent seat"
        ),
    )
    doc.add_argument("config", type=Path, help="Path to the Tier B company YAML")


def _render(report: ProvisionReport) -> None:
    """Print the per-seat table the operator reads the run from."""
    if report.notes:
        for note in report.notes:
            print(f"note: {note}")
        print()

    if report.seats:
        print(
            f"{'HANDLE':<18} {'USERNAME':<22} {'BOT':<12} {'TEAM':<8} "
            f"{'TOKEN':<8} CHANNELS"
        )
        for seat in report.seats:
            channels = ",".join(seat.channels) or "-"
            print(
                f"{seat.handle:<18} {seat.username:<22} {seat.bot_action:<12} "
                f"{seat.team:<8} {seat.token_action:<8} {channels}"
            )
            for note in seat.notes:
                print(f"    note: {note}")
            if seat.error:
                print(f"    FAILED: {seat.error}")

    for handle, reason in report.skipped:
        print(f"skipped {handle}: {reason}")

    if report.failed:
        print()
        print(f"{len(report.failed)} seat(s) failed — re-run to resume.")


async def _run(args: Any, target: _ProvisionTarget, org: Any, raw: Any) -> int:
    admin_token = args.admin_token or os.environ.get(ADMIN_TOKEN_ENV, "")
    if not admin_token:
        print(
            "No admin token supplied. Pass --admin-token or export "
            f"{ADMIN_TOKEN_ENV}. Generate one in Mattermost under "
            "Profile → Security → Personal Access Tokens on a system-admin "
            "account (an admin must first enable them in "
            "System Console → Integrations)."
        )
        return 1

    client = MattermostClient(target.url, admin_token)
    sink: TokenSink | None = None
    db: Any = None
    try:
        if args.decommission:
            handles = [h.strip() for h in args.decommission.split(",") if h.strip()]
            # --dry-run is checked HERE, not in the branch below: this
            # path used to run to completion before the dry-run branch was
            # ever reached, so rehearsing a decommission with the flag
            # documented as "create and modify nothing" revoked every
            # token irreversibly and disabled every named bot.
            outcomes = await decommission(
                client,
                handles,
                org=org,
                username_prefix=_username_prefix(raw),
                dry_run=bool(args.dry_run),
            )
            for handle, outcome in outcomes:
                print(f"{handle:<18} {outcome}")
            if not args.dry_run:
                print(
                    "\nDecommissioned seats keep their history and can be "
                    "re-enabled by a later provision run, which mints a fresh "
                    "token (the old one is revoked)."
                )
            failed = [h for h, outcome in outcomes if outcome.startswith("error")]
            if failed:
                print(f"\n{len(failed)} handle(s) failed — re-run to resume.")
            return 1 if failed else 0

        sink, db = await open_token_sink(args, source="mattermost-provision")

        if args.dry_run:
            print(f"Would provision against {target.url} (team {target.team}).")
            from crewlet.mattermost.provision import seat_token_vars, seat_username

            for role in org.all_roles():
                identity = dict(getattr(role, "mattermost", None) or {})
                if not identity or getattr(role, "is_human", False):
                    continue
                token_vars = seat_token_vars(role)
                state = (
                    "already set"
                    if token_vars and all(sink.existing(v) for v in token_vars)
                    else "would mint"
                )
                username = seat_username(role, _username_prefix(raw))
                print(f"  {role.get_handle():<18} {username:<22} {state}: {token_vars}")
            return 0

        handles = (
            {h.strip() for h in args.handles.split(",") if h.strip()}
            if args.handles
            else None
        )
        provisioning = getattr(raw, "provisioning", None)
        report = await provision(
            client,
            org,
            team=target.team,
            sink=sink,
            username_prefix=_username_prefix(raw),
            display_name_suffix=getattr(provisioning, "display_name_suffix", "") or "",
            default_channels=list(getattr(provisioning, "channels", []) or []),
            handles=handles,
        )
        _render(report)
        return 0 if report.ok else 1
    finally:
        await client.close()
        if db is not None:
            await db.close()


def _username_prefix(raw: Any) -> str:
    provisioning = getattr(raw, "provisioning", None)
    return getattr(provisioning, "username_prefix", "") or ""


def _seat_tokens(org: Any) -> dict[str, str]:
    """Every Mattermost-enabled agent seat's resolved bot token.

    Resolved through the engine's own ``${VAR}`` path (secret store, then
    environment) so the doctor reads exactly the credential the engine
    would boot with — a token present only in the secret store must not
    read as missing here.
    """
    from crewlet.config import resolve_env_scalar

    tokens: dict[str, str] = {}
    for role in org.all_roles():
        identity = dict(getattr(role, "mattermost", None) or {})
        if not identity or getattr(role, "is_human", False):
            continue
        tokens[role.get_handle()] = resolve_env_scalar(
            str(identity.get("bot_token") or "")
        ).strip()
    return tokens


def cmd_mattermost_doctor(args: Any) -> int:
    """Check one Mattermost install and every seat configured against it."""
    from crewlet.config import config_to_organization
    from crewlet.mattermost.doctor import format_report, run_doctor

    loaded = _load_company(args.config)
    if loaded is None:
        return 1
    cfg, raw = loaded
    try:
        target = _resolve_target(raw)
    except MattermostConfigError as exc:
        logger.error("mattermost_doctor_config_unresolved", error=str(exc))
        print(str(exc))
        return 1

    org = config_to_organization(cfg)
    report = asyncio.run(
        run_doctor(
            url=target.url,
            team=target.team,
            seat_tokens=_seat_tokens(org),
        )
    )
    print(format_report(report))
    return 0 if report.ok else 1


def _load_company(config_path: Path) -> tuple[Any, Any] | None:
    """Load the company YAML and its enabled ``integrations.mattermost``.

    Shared by ``provision`` and ``doctor`` so the two commands cannot
    disagree about what "this config has Mattermost" means.
    """
    from crewlet.config import load_company_config

    load_env_file(config_path)
    if not config_path.exists():
        logger.error("mattermost_config_missing", path=str(config_path))
        print(f"Company config file not found: {config_path}")
        return None
    try:
        cfg = load_company_config(config_path)
    except ValueError as exc:  # incl. pydantic ValidationError
        logger.error("mattermost_config_invalid", error=str(exc))
        print(f"Company config is invalid: {exc}")
        return None
    raw = getattr(cfg.integrations, "mattermost", None)
    if raw is None or not getattr(raw, "enabled", False):
        logger.error("mattermost_not_enabled")
        print("integrations.mattermost is not enabled in this config.")
        return None
    return cfg, raw


def cmd_mattermost_provision(args: Any) -> int:
    """Run the Mattermost provisioning reconcile."""
    from crewlet.config import config_to_organization

    loaded = _load_company(args.config)
    if loaded is None:
        return 1
    cfg, raw = loaded

    try:
        target = _resolve_target(raw)
    except MattermostConfigError as exc:
        logger.error("mattermost_provision_config_unresolved", error=str(exc))
        print(str(exc))
        return 1

    org = config_to_organization(cfg)

    try:
        return asyncio.run(_run(args, target, org, raw))
    except MattermostProvisionAborted as exc:
        logger.error("mattermost_provision_aborted", error=str(exc))
        print(f"Aborted before making any changes: {exc}")
        return 1
    except SecretStoreError as exc:
        logger.error("mattermost_provision_secret_store", error=str(exc))
        print(str(exc))
        return 1
    except MattermostError as exc:
        logger.error("mattermost_provision_failed", error=str(exc))
        print(f"Mattermost API error: {exc}")
        return 1


__all__ = [
    "ADMIN_TOKEN_ENV",
    "MattermostConfigError",
    "add_mattermost_parser",
    "cmd_mattermost_doctor",
    "cmd_mattermost_provision",
]
