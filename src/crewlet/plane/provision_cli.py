"""``crewlet plane provision`` — CLI entry for Plane provisioning.

Loads the company YAML, resolves the ``integrations.plane`` connection
coordinates, and runs the :func:`crewlet.plane.provision.provision`
reconcile with an operator credential (a workspace-admin personal API
token — Plane's provisioning surface is entirely
workspace-owner-gated, so there is no instance-admin mode and no
``--mode`` flag).  Token values are written to an env file (default)
or printed for shell ``eval``.  See ``docs/integrations/plane.md``.

Config handling deliberately diverges from the GitLab CLI's
whole-model re-validation (:func:`_resolve_gitlab_config`): a
first-run ``${PLANE_WEBHOOK_SECRET}`` is *unset* — Plane generates the
webhook secret server-side and THIS command captures it — so
re-validating a resolved :class:`~crewlet.config.PlaneConfig` would
trip its required-``webhook_secret`` validator before the provisioner
ever runs (the chicken-and-egg trap ``plane/import_cli.py`` documents
and avoids).  Instead only ``url`` and ``workspace`` are resolved
per-field; ``token`` and ``webhook_secret`` stay RAW because their
``${VAR}`` references ARE the minting contract.
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
from crewlet.plane.client import PlaneClient, PlaneProvisionError
from crewlet.plane.provision import (
    PlaneProvisionAborted,
    ProvisionReport,
    provision,
)
from crewlet.provisioning import TokenSink, add_sink_arguments, open_token_sink

logger = get_logger("plane.provision_cli")

#: Default token lifetime when neither ``--token-expiry-days`` nor a
#: ``provisioning`` block supplies one — mirrors
#: ``PlaneProvisioningConfig.token_expiry_days`` (364: just under a
#: year, so an annual ``--rotate`` cadence renews the credential
#: before it lapses — the same rationale as the GitLab provisioner's
#: 364-day default under GitLab.com's 365-day PAT cap).  ``0`` omits
#: ``expired_at`` entirely, which in Plane means the token NEVER
#: expires (not GitLab's "instance default applies").
_DEFAULT_TOKEN_EXPIRY_DAYS = 364


def _nonnegative_days(value: str) -> int:
    """Argparse type for ``--token-expiry-days``: an int >= 0.

    A negative value would silently mean "never expires" (``_expiry``
    returns ``None`` for anything <= 0, and ``None`` on the Plane path
    omits/nulls ``expired_at``) — the exact inverse of the operator's
    intent, so it is rejected at the parser.  ``0`` is the deliberate,
    documented never-expires spelling.
    """
    days = int(value)
    if days < 0:
        raise argparse.ArgumentTypeError(
            f"must be >= 0 (got {days}); 0 is the explicit never-expires "
            "spelling — a negative value would silently mean the same thing"
        )
    return days


def add_provision_parser(plane_sub: argparse._SubParsersAction) -> None:
    """Register the ``provision`` subcommand on the existing ``plane`` group.

    Unlike :func:`crewlet.gitlab.provision_cli.add_gitlab_parser` (which
    owns its whole top-level group), the ``plane`` group already exists
    in ``cli.py`` for ``import`` / ``resync`` — this only adds the
    ``provision`` sub-parser to it.
    """
    prov = plane_sub.add_parser(
        "provision",
        help=(
            "Reconcile the company config into Plane: one service account "
            "per agent seat, project membership, per-agent API tokens, the "
            "crewlet-engine read account, and the workspace webhook"
        ),
    )
    prov.add_argument("config", type=Path, help="Path to the Tier B company YAML")
    prov.add_argument(
        "--provision-token",
        default=None,
        help=(
            "Operator credential: a workspace-admin personal API token "
            "(never stored in config). Falls back to $PLANE_PROVISION_TOKEN."
        ),
    )
    prov.add_argument(
        "--webhook-url",
        default=None,
        help=(
            "Engine webhook endpoint to register as the one workspace "
            "webhook (e.g. https://engine.example.com/webhooks/plane). "
            "Omit to skip the webhook step."
        ),
    )
    add_sink_arguments(prov, default_env_file=".env.plane")
    prov.add_argument(
        "--rotate",
        action="store_true",
        help=(
            "Rotate every managed credential: each seat's managed token, "
            "the engine token, and — only together with --webhook-url — "
            "the webhook secret (delete + recreate; Plane secrets are "
            "immutable and single-show)"
        ),
    )
    prov.add_argument(
        "--decommission-removed",
        action="store_true",
        help=(
            "Delete service accounts whose seats left the config (uses the "
            "fork's service-account management API; irreversible for the "
            "username). Requires provisioning.username_prefix to scope "
            "managed accounts safely."
        ),
    )
    prov.add_argument(
        "--recreate-webhook",
        action="store_true",
        help=(
            "When the workspace webhook exists but its secret is not "
            "recorded here (lost env file, second machine): delete + "
            "recreate it to mint a fresh secret. Destructive — invalidates "
            "the secret every other deployment holds."
        ),
    )
    prov.add_argument(
        "--create-projects",
        action="store_true",
        help=(
            "Create declared provisioning.projects that don't exist yet "
            "(name = identifier; rename in the Plane UI at will). "
            "`crewlet plane import` never creates projects — this does."
        ),
    )
    prov.add_argument(
        "--token-expiry-days",
        type=_nonnegative_days,
        default=None,
        help=(
            "One-off override of provisioning.token_expiry_days (the "
            "standing policy; config default 364). Must be >= 0: 0 omits "
            "expired_at, which in Plane means the token NEVER expires (not "
            "GitLab's 'instance default applies')."
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


class PlaneConfigError(Exception):
    """Raised when the ``integrations.plane`` block cannot be used.

    Carries a human-readable, actionable message (built by
    :func:`_resolve_plane_provision_config`) naming any unset ``${VAR}``
    references rather than surfacing an empty-field symptom.
    """


class _ProvisionTarget(NamedTuple):
    """Resolved connection coordinates for the provisioning run."""

    url: str
    workspace: str


def _resolve_plane_provision_config(plane_raw: Any) -> _ProvisionTarget:
    """Per-field resolution of the ``integrations.plane`` connection.

    Resolves ONLY ``url`` and ``workspace`` (both required) — the block
    already validated at load time with its ``${VAR}`` references
    verbatim, and re-validating a resolved :class:`PlaneConfig` would
    demand a ``webhook_secret`` value that does not exist yet on a
    first run (Plane generates it; this CLI captures it).  ``token``
    and ``webhook_secret`` stay RAW: their references are the minting
    contract consumed by :func:`crewlet.plane.provision.provision`.

    Raises :class:`PlaneConfigError` with an actionable message naming
    unset ``${VAR}`` references when either field resolves empty.
    """
    from crewlet.config import resolve_env_scalar

    url = resolve_env_scalar(str(plane_raw.url or "")).strip()
    workspace = resolve_env_scalar(str(plane_raw.workspace or "")).strip()
    if url and workspace:
        return _ProvisionTarget(url=url, workspace=workspace)

    missing = [
        name for name, value in (("url", url), ("workspace", workspace)) if not value
    ]
    unset = _unset_env_refs(
        {"url": str(plane_raw.url or ""), "workspace": str(plane_raw.workspace or "")}
    )
    hint = ""
    if unset:
        hint = (
            "\nUnset or empty environment variables referenced by "
            "integrations.plane:\n"
            + "\n".join(f"  - {name}" for name in unset)
            + "\nExport these (e.g. in your .env) before provisioning."
        )
    raise PlaneConfigError(
        "integrations.plane must resolve non-empty "
        f"{' and '.join(missing)} value(s) to provision.{hint}"
    )


def _first_run_secret_hint() -> str:
    """The M10 first-run chicken-and-egg remediation message.

    ``PlaneConfig`` requires a non-empty ``webhook_secret`` whenever the
    integration is enabled, and that validator runs at
    ``load_company_config`` — BEFORE the provisioner that would supply
    the secret.  The way out is a ``${VAR}`` *reference* (verbatim refs
    pass load-time validation): Plane generates the secret server-side
    at webhook creation and the provisioner captures it into that var.
    """
    return (
        "integrations.plane.webhook_secret is empty. On a first run set it "
        "to a ${VAR} REFERENCE, not a value — e.g.\n"
        '  webhook_secret: "${PLANE_WEBHOOK_SECRET}"\n'
        "Plane generates the webhook secret server-side when the provisioner "
        "creates the workspace webhook, and `crewlet plane provision "
        "--webhook-url ...` captures it into that variable for you."
    )


def cmd_plane_provision(args: Any) -> int:
    """Run the Plane provisioning reconcile."""
    config_path: Path = args.config
    load_env_file(config_path)
    if not config_path.exists():
        logger.error("plane_provision_config_missing", path=str(config_path))
        print(f"Company config file not found: {config_path}")
        return 1

    from crewlet.config import config_to_organization, load_company_config

    try:
        cfg = load_company_config(config_path)
    except ValueError as exc:  # incl. pydantic ValidationError
        logger.error("plane_provision_config_invalid", error=str(exc))
        if "plane.webhook_secret is required" in str(exc):
            print(_first_run_secret_hint())
        else:
            print(f"Company config is invalid: {exc}")
        return 1

    plane_raw = getattr(cfg.integrations, "plane", None)
    if plane_raw is None or not getattr(plane_raw, "enabled", False):
        logger.error("plane_provision_not_enabled")
        print("integrations.plane is not enabled in this config.")
        return 1

    try:
        target = _resolve_plane_provision_config(plane_raw)
    except PlaneConfigError as exc:
        logger.error("plane_provision_config_invalid", error=str(exc))
        print(str(exc))
        return 1

    token = args.provision_token or os.environ.get("PLANE_PROVISION_TOKEN") or ""
    if not token:
        print(
            "No operator credential. Pass --provision-token or set "
            "$PLANE_PROVISION_TOKEN (a workspace-admin personal API token)."
        )
        return 1

    # The flag is the one-off override; the config field is the standing
    # policy (unlike GitLab, whose config has no expiry field).
    if args.token_expiry_days is not None:
        expiry_days = int(args.token_expiry_days)
    else:
        provisioning = getattr(plane_raw, "provisioning", None)
        expiry_days = (
            int(provisioning.token_expiry_days)
            if provisioning is not None
            else _DEFAULT_TOKEN_EXPIRY_DAYS
        )

    org = config_to_organization(cfg)

    async def _run() -> ProvisionReport:
        # No CLI-side probing (the GitLab pattern): provision()'s
        # pre-mutation preflight IS the whole decision procedure — the
        # /users/me credential probe, the workspace-slug probe, then the
        # capability probes for the surfaces this run actually touches.
        sink: TokenSink
        sink, db = await open_token_sink(args, source="plane-provision")
        client = PlaneClient(target.url, token, target.workspace)
        try:
            return await provision(
                client,
                org,
                plane_raw,
                webhook_url=args.webhook_url or "",
                sink=sink,
                rotate=bool(args.rotate),
                decommission_removed=bool(args.decommission_removed),
                create_projects=bool(args.create_projects),
                recreate_webhook=bool(args.recreate_webhook),
                token_expiry_days=expiry_days,
                # RAW (unresolved) values — their ${VAR} references are
                # the minting contract for the engine's read account and
                # the captured webhook secret (see provision()).
                engine_token_ref=str(plane_raw.token or ""),
                webhook_secret_ref=str(plane_raw.webhook_secret or ""),
            )
        finally:
            await client.close()
            if db is not None:
                await db.close()

    try:
        report = asyncio.run(_run())
    except SecretStoreError as exc:
        logger.error("plane_provision_secret_store_unavailable", error=str(exc))
        print(f"Cannot use the secret store: {exc}")
        return 1
    except PlaneProvisionAborted as exc:
        logger.error("plane_provision_aborted", error=str(exc))
        print(f"Provisioning cannot proceed: {exc}")
        return 1
    except PlaneProvisionError as exc:
        logger.error("plane_provision_failed", error=str(exc))
        print(f"Provisioning failed: {exc}")
        return 1

    _print_report(report, printed_tokens=bool(args.print_tokens))
    # `error` (not account_action) drives the exit code: a seat whose
    # account was created but whose later step failed keeps
    # account_action="created" — truthful state — with the failure in
    # `error`.
    return 0 if all(not s.error for s in report.seats) else 1


def _print_report(report: ProvisionReport, *, printed_tokens: bool) -> None:
    print("\nPlane provisioning report")
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
            # Source AND restart: nothing re-resolves the config block in
            # a running engine, so new values only apply on a fresh start.
            print(
                f"\n  {len(minted)} token(s) written to the env file. Source "
                "it and restart the engine so the new values take effect."
            )
    if report.members:
        # The member table ends the report so founders can copy the human
        # UUIDs into contact.plane_user_id.
        print("\n  Workspace members (copy human UUIDs into contact.plane_user_id):")
        for row in report.members:
            marker = "  [agent]" if row.get("managed") else ""
            print(
                f"    {row.get('display_name', ''):<24} "
                f"{row.get('username', ''):<24} "
                f"{row.get('id', ''):<38} {row.get('role', '')}{marker}"
            )
