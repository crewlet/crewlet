"""Reconcile company config → Plane state for ``crewlet plane provision``.

Idempotent: given the org and the ``integrations.plane`` block, ensure
one service account per agent seat, project
membership, one API token per seat (minted from the config's own
``${VAR}`` references), the engine's ``crewlet-engine`` read account,
and the workspace webhook — capturing Plane's server-generated
webhook secret into the ``${VAR}`` behind
``integrations.plane.webhook_secret``.  Re-runs no-op; ``rotate``
re-mints every managed credential (seat tokens, engine token, webhook
secret); ``decommission_removed`` retires prefixed accounts whose seats
left the config.  See ``docs/integrations/plane.md``.

Token *values* leave this module only through a
:class:`~crewlet.provisioning.TokenSink` — Plane returns every
credential exactly once (service-account tokens at mint/rotate, the
webhook ``secret_key`` at hook creation), so a ``${VAR}`` that already
has a value is skipped, which is what makes minting idempotent.

Capability model (fork-gated, detected pre-mutation): service-account
*create* is the upstream service-accounts route; the token lifecycle
(list/mint/rotate/revoke) and account DELETE are the fork's
**token-lifecycle** routes; the members rows'
``username``/``is_bot``/``bot_type`` fields are the fork's
**member-identity** fields (hard-required — the reconcile keys every
create-vs-exists decision on usernames); the webhook ``page`` toggle is
the fork's **page-webhook** support (detected from the create/update
echo, degraded with a note).  Without the token-lifecycle routes the
reconcile still runs in a degraded service-account-only mode —
creation itself mints token #1 — but cannot rotate, re-mint on an
existing account, or decommission.
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from urllib.parse import urlsplit, urlunsplit

from crewlet._logging import get_logger
from crewlet.config import resolve_env_scalar
from crewlet.plane.client import PlaneClient, PlaneProvisionError
from crewlet.provisioning import TokenSink, referenced_env_vars, sole_env_var

logger = get_logger("plane.provision")


class PlaneProvisionAborted(RuntimeError):
    """Provisioning cannot proceed for a reason the operator must resolve.

    Carries a plain, actionable message (missing fork capability,
    operator not a workspace admin, a literal webhook secret, ...) —
    distinct from :class:`~crewlet.plane.client.PlaneProvisionError`'s
    raw ``method path → status: body`` format.  Raised by preflight
    checks so the CLI can print one clear next step instead of N
    identical API failures buried in the seat report.
    """


#: Workspace-role name → ``WorkspaceMember.role`` int, pinned from the
#: fork's ``plane/utils/service_account.py`` ``SERVICE_ACCOUNT_ROLES``
#: (which mirrors CE's ROLE_CHOICES).  The create endpoint takes the
#: STRING name; project-member writes take the INT.
SERVICE_ACCOUNT_ROLES: dict[str, int] = {"admin": 20, "member": 15, "guest": 5}

_ROLE_NAME_BY_INT = {value: name for name, value in SERVICE_ACCOUNT_ROLES.items()}

#: Handle/username stem of the engine's routing/read account (the
#: ``integrations.plane.token`` credential).
ENGINE_ACCOUNT_HANDLE = "crewlet-engine"

# Workspace-webhook entity toggles the engine's parser acts on.  The
# router consumes project / issue / issue_comment / page events (page
# feeds the Tool Skills sync worker); cycle and module are inbox noise
# the engine never routes, so they stay off.  ``intake_issue`` has no
# toggle — Plane delivers intake events to every active workspace hook
# unconditionally.  ``page`` requires the fork's page-webhook support on
# the public serializer; without it DRF silently drops the field, which
# ``_check_page_echo`` detects from the response.
_HOOK_ENTITY_TOGGLES: dict[str, bool] = {
    "project": True,
    "issue": True,
    "issue_comment": True,
    "page": True,
    "cycle": False,
    "module": False,
}

#: Expiry-warning horizon for the token-expiry report.
#: The default token lifetime is 364 days (``provisioning.
#: token_expiry_days``); 30 days gives the operator a full monthly
#: ops/re-provision cycle of lead time to schedule a ``--rotate``
#: before the credential lapses, while staying silent for >90% of the
#: token's life.
_TOKEN_EXPIRY_WARNING_DAYS = 30


def seat_token_vars(role: object) -> list[str]:
    """The ``${VAR}`` token references to mint for one seat, deduped.

    ``mcp_env.plane`` ONLY — deliberately narrower than GitLab's scan:
    Plane is not a code host, so there is no git-auth sandbox recipe and
    no conventional ``sandbox.env`` token key; sandboxed MCP credentials
    flow from the same ``mcp_env`` via ``resolve_sandbox_mcp``, so a
    sandbox-using seat already gets the same var.
    """
    return referenced_env_vars(dict(role.mcp_env.get("plane") or {}))


@dataclass
class SeatResult:
    """Outcome of provisioning one agent seat.

    ``account_action`` keeps the FURTHEST truthful state — a seat whose
    account was created but whose later step failed stays ``created``
    (the account exists!) with ``error`` carrying the failure; ``error``
    non-empty is what makes the run exit 1.  ``skipped`` is the seat
    whose account was deliberately not created (no ``${VAR}`` to record
    the single-show creation token into).  ``blocked`` is the degraded
    service-account-only mode's could-not-mint (distinct from ``skipped`` =
    already provisioned); ``needs_rotate`` is an unrecorded var whose
    recovery would invalidate a live token and therefore waits for
    ``--rotate``.
    """

    handle: str
    username: str
    account_action: str = "pending"  # created | exists | skipped | error
    user_id: str | None = None  # Plane user UUID (str, never an int)
    membership: str = "pending"  # added | exists | inactive | skipped
    token_vars: list[str] = field(default_factory=list)
    token_action: str = (
        "none"  # minted | rotated | skipped | needs_rotate | blocked | none
    )
    error: str = ""


@dataclass
class ProvisionReport:
    seats: list[SeatResult] = field(default_factory=list)
    hooks: list[str] = field(default_factory=list)
    decommissioned: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    #: The member table: one row per workspace member
    #: (``display_name`` / ``username`` / ``id`` / ``role`` /
    #: ``managed``), rendered at the END of the report so founders can
    #: copy the human UUIDs into ``contact.plane_user_id``.
    members: list[dict] = field(default_factory=list)


def _expiry(days: int) -> datetime | None:
    """UTC expiry ``days`` ahead, or ``None`` for 0.

    ``None`` means the token NEVER expires (Plane semantics: an omitted
    ``expired_at`` on mint, an explicit ``null`` on rotate) — a
    deliberate delta from GitLab, where omission means "instance
    default applies".
    """
    if days <= 0:
        return None
    return datetime.now(UTC) + timedelta(days=days)


def _managed_label(handle: str) -> str:
    """The provisioner-managed token label for a seat.

    One convention shared with the GitLab provisioner's PAT naming; the
    create call's ``name`` is this same string because the
    service-accounts API pins ``name`` as the first token's label.
    (Plane also stores ``name`` as the user's ``first_name`` — cosmetic, since
    ``display_name`` is set separately and drives the members UI, and
    renaming would break rotate targeting for existing accounts.)
    """
    return f"crewlet-provision:{handle}"


def _validated_role(name: object, where: str) -> str:
    """Normalize + validate a workspace-role name (M11).

    ``PlaneProvisioningConfig`` accepts free strings, but the fork's
    serializer is a strict ``ChoiceField`` — a GitLab copy-paste like
    ``developer`` would 400 on every seat, so it aborts here instead.
    """
    normalized = str(name or "").strip().lower()
    if normalized not in SERVICE_ACCOUNT_ROLES:
        raise PlaneProvisionAborted(
            f"invalid workspace role '{name}' in {where}: Plane service "
            f"accounts accept {' | '.join(sorted(SERVICE_ACCOUNT_ROLES))}"
        )
    return normalized


async def _preflight_capabilities(
    client: PlaneClient, *, rotate: bool, decommission_removed: bool
) -> bool:
    """Pre-mutation capability preflight.  Returns whether the fork's
    token-lifecycle routes are present.

    Decision procedure (the capability probes use a deliberately
    disallowed HTTP method, so nothing here can mutate anything):

    0. ``GET /users/me/`` — the credential probe: a 401 OR 403 here
       means the operator token itself is invalid/expired/revoked.  The
       fork's own contract tests deliberately do NOT pin which of the
       two a bad credential produces (DRF turns ``AuthenticationFailed``
       into 403 when the authenticator supplies no ``WWW-Authenticate``
       header), so both statuses map to the same message.
    1. ``GET /workspaces/{slug}/projects/`` — the workspace probe: with
       the credential proven, a 403/404 here means the configured slug
       does not name a workspace this credential can see (Plane's
       slug-filtered permission classes reject an unknown slug exactly
       like a non-member).
    2. Service-account create route absent (404) → abort: stock CE.
    3. Probe 401/403 → abort: route present but the operator is not a
       workspace admin (every provisioning endpoint requires it) — the
       credential and slug were already proven good, so permission is
       the only remaining cause.
    4. Token-lifecycle routes absent (404) + ``--rotate`` or
       ``--decommission-removed`` → abort BEFORE any mutation.
    5. Token-lifecycle routes absent on a plain run → proceed in the
       degraded service-account-only mode (creation itself mints token
       #1); seats needing a re-mint get one collective note, never N
       failures.
    """
    try:
        await client.get_current_user()
    except PlaneProvisionError as exc:
        if exc.status in (401, 403):
            raise PlaneProvisionAborted(
                "Plane rejected the operator credential (HTTP "
                f"{exc.status} from /users/me/): the token is invalid, "
                "expired, or revoked — pass a valid workspace-admin API "
                "token via --provision-token / $PLANE_PROVISION_TOKEN"
            ) from exc
        raise
    workspace_status = await client.probe_workspace()
    if workspace_status in (401, 403, 404):
        raise PlaneProvisionAborted(
            f"workspace '{client.workspace}' not found or not visible to the "
            "operator credential (the token authenticates, but every "
            "workspace-scoped route rejects the slug) — check "
            "integrations.plane.workspace, and that the token's account "
            "belongs to that workspace"
        )
    create_status = await client.probe_service_accounts()
    if create_status == 404:
        raise PlaneProvisionAborted(
            "no service-accounts API at this Plane instance (HTTP 404) — "
            "provisioning requires the crewlet/plane fork (`preview`); stock "
            "Plane CE cannot provision service accounts"
        )
    if create_status in (401, 403):
        raise PlaneProvisionAborted(
            "operator token is not a workspace admin (every provisioning "
            "endpoint requires Plane's workspace-owner permission) — create "
            "the token from a workspace-admin account"
        )
    token_status = await client.probe_service_account_tokens()
    if token_status in (401, 403):
        raise PlaneProvisionAborted(
            "operator token is not a workspace admin (every provisioning "
            "endpoint requires Plane's workspace-owner permission) — create "
            "the token from a workspace-admin account"
        )
    f10 = token_status != 404
    if not f10 and (rotate or decommission_removed):
        raise PlaneProvisionAborted(
            "this Plane instance lacks the fork's service-account "
            "token-lifecycle routes — stock CE cannot rotate, re-mint, or "
            "decommission; deploy the crewlet/plane fork (`preview`) or drop "
            "--rotate/--decommission-removed"
        )
    if not f10:
        logger.warning("plane_provision_f10_absent")
    return f10


async def _fetch_members(client: PlaneClient) -> list[dict]:
    """Strict members enumeration + the member-identity hard requirement.

    Stock CE's members rows are ``UserLiteSerializer`` dicts with NO
    ``username`` — and the reconcile keys every create-vs-exists
    decision (and decommission targeting) on usernames, so without the
    fork's member-identity fields (``username``/``is_bot``) provisioning
    is unimplementable, not merely degraded.
    """
    members = await client.list_workspace_members()
    if members and "username" not in members[0]:
        raise PlaneProvisionAborted(
            "the workspace members API serves rows without a `username` "
            "field — the reconcile keys every create-vs-exists decision on "
            "usernames, so provisioning requires the fork's member-identity "
            "fields; deploy the crewlet/plane fork (`preview`)"
        )
    return members


async def _resolve_projects(
    client: PlaneClient,
    identifiers: list[str],
    *,
    create_projects: bool,
    report: ProvisionReport,
) -> list[tuple[str, str]]:
    """Resolve project identifiers → ``(identifier, uuid)`` pairs.

    Case-insensitive identifier match (the import-CLI convention).
    Missing projects are created only with ``--create-projects``
    (``name=identifier`` — config carries only identifiers; the
    operator renames in the UI at will), otherwise dropped with a note
    so accounts, tokens, and the projects that DO exist still
    reconcile.  A FAILED create is a per-project note + drop, never a
    whole-run abort: CE's project list is membership-scoped, so a
    project that exists but is invisible to the operator classifies as
    "missing" and its create then 409s on the taken identifier — the
    other seats and projects must still reconcile (a duplicate
    create surfaces as a note, self-healing).
    """
    rows = await client.list_projects(strict=True)
    by_identifier = {
        str(row.get("identifier", "")).lower(): row for row in rows if row.get("id")
    }
    resolved: list[tuple[str, str]] = []
    missing: list[str] = []
    for identifier in identifiers:
        row = by_identifier.get(identifier.lower())
        if row is not None:
            resolved.append((identifier, str(row["id"])))
            continue
        if create_projects:
            try:
                created = await client.create_project(
                    name=identifier, identifier=identifier
                )
            except PlaneProvisionError as exc:
                if 400 <= exc.status < 500:
                    report.notes.append(
                        f"could not create project '{identifier}' ({exc}) — a "
                        "conflict means it already exists but is not visible "
                        "to the operator credential: add the operator to the "
                        "project, or remove it from provisioning.projects; "
                        "the project was dropped from this run"
                    )
                    logger.warning(
                        "plane_provision_project_create_failed",
                        identifier=identifier,
                        status=exc.status,
                    )
                    continue
                raise
            resolved.append((identifier, str(created.get("id", ""))))
            report.notes.append(f"created project '{identifier}'")
            logger.info("plane_provision_project_created", identifier=identifier)
        else:
            missing.append(identifier)
            logger.warning("plane_provision_project_missing", identifier=identifier)
    if missing:
        report.notes.append(
            "declared projects not found (or not visible to the operator "
            "credential) — create them with --create-projects or remove them "
            "from integrations.plane.provisioning.projects: "
            f"{', '.join(missing)}"
        )
    return resolved


async def _ensure_account(
    client: PlaneClient,
    *,
    members_by_username: dict[str, dict],
    username: str,
    display_name: str,
    workspace_role: str,
    handle: str,
    result: SeatResult,
    report: ProvisionReport,
) -> str:
    """Ensure the seat's service account; return its creation token.

    Returns the create response's single-show ``token`` when the
    account was created this run, ``""`` when it already existed.  A
    matched members row that is NOT a service account (per the fork's
    ``is_bot``/``bot_type`` member-identity fields) is a seat ERROR raised BEFORE any
    membership write: adopting it would stamp project memberships onto
    a human (or a non-service bot) and then fail opaquely at the token
    step — the misdiagnosis those fields exist to prevent.  On an
    existing service account, display-name and workspace-role drift are
    reported as notes — the pinned fork has no service-account update
    endpoint and the members API is read-only, so repair would need a
    fork patch (delete + recreate is forbidden: it rotates the user
    UUID and orphans HandleRegistry + attribution).
    """
    row = members_by_username.get(username.lower())
    if row is not None:
        if not (row.get("is_bot") and row.get("bot_type") == "SERVICE"):
            raise PlaneProvisionError(
                f"username '{username}' is held by an existing workspace "
                "member that is NOT a service account (a human or non-service "
                "bot) — the provisioner refuses to adopt it; set "
                "provisioning.username_prefix so managed usernames cannot "
                "collide with human ones"
            )
        result.account_action = "exists"
        result.user_id = str(row["id"])
        have_name = str(row.get("display_name", ""))
        if have_name != display_name:
            report.notes.append(
                f"{username}: display name drifted: '{have_name}' → wants "
                f"'{display_name}'; no update endpoint (would need a fork patch)"
            )
        want_int = SERVICE_ACCOUNT_ROLES[workspace_role]
        have_int = row.get("role")
        if have_int != want_int:
            have_role = _ROLE_NAME_BY_INT.get(have_int, str(have_int))
            report.notes.append(
                f"{username}: workspace role drifted (has {have_role}, config "
                f"wants {workspace_role}); the members API is read-only — no "
                "update endpoint (would need a fork patch)"
            )
        return ""

    try:
        created = await client.create_service_account(
            name=_managed_label(handle),
            # Always explicit: the API default is `admin`, silently
            # privilege-escalating for an omitting caller.
            role=workspace_role,
            username=username,
            display_name=display_name,
        )
    except PlaneProvisionError as exc:
        if exc.status == 409:
            # The 409 covers both a foreign holder AND a previously
            # decommissioned service account — decommission keeps the
            # (deactivated) user row, so the username is terminally
            # taken and lookup-before-create can never adopt it.
            # (PlaneProvisionError's raw-body cap applies only at the
            # client boundary, so this crafted remediation is never
            # truncated, whatever the username's length.)
            raise PlaneProvisionError(
                f"username '{username}' already taken: a foreign user holds "
                "it (adjust provisioning.username_prefix) or a decommissioned "
                "service account owned it (decommission is irreversible for "
                "the username) — pick a new handle/prefix or reactivate "
                "the account server-side",
                method=exc.method,
                path=exc.path,
                status=409,
            ) from exc
        raise

    echoed = str(created.get("username", ""))
    if echoed.lower() != username.lower():
        # A service-accounts-only instance silently ignores the
        # caller-chosen identity fields and generates svc_<uuid> values —
        # undetected, every run would create a fresh account per seat,
        # forever.  Abort naming the orphan (its DELETE needs the fork's
        # token-lifecycle routes, so it cannot be cleaned up from here).
        raise PlaneProvisionAborted(
            f"created service account echoed username '{echoed}' instead of "
            f"the requested '{username}' — this instance ignores caller-chosen "
            "identity fields (a service-accounts API without the fork's "
            f"identity support); the just-created orphan account {created.get('id')} "
            "must be removed server-side; deploy the crewlet/plane fork "
            "(`preview`)"
        )
    result.account_action = "created"
    result.user_id = str(created.get("id"))
    return str(created.get("token", ""))


async def _project_role_map(
    client: PlaneClient,
    project_id: str,
    cache: dict[str, dict[str, dict] | None],
) -> dict[str, dict] | None:
    """Per-project ``user-uuid → {role, is_active}`` map, fetched once per run.

    Both fields come from the same ``ProjectMemberLiteAPISerializer``
    rows: ``role`` feeds drift detection, ``is_active`` catches a
    membership deactivated in the Plane UI (the detail DELETE sets
    ``is_active=False`` and KEEPS the row) — the seat then has no
    project access even though a re-add 400s as a duplicate.

    ``None`` (cached) when the listing failed — e.g. a workspace-admin
    operator who is not a member of the project gets 403 from the
    membership-scoped lite endpoint (M4); drift detection is then
    skipped for that project rather than failing the seat.
    """
    if project_id in cache:
        return cache[project_id]
    try:
        rows = await client.list_project_members_lite(project_id)
    except PlaneProvisionError as exc:
        logger.warning(
            "plane_provision_project_members_unreadable",
            project_id=project_id,
            status=exc.status,
        )
        cache[project_id] = None
        return None
    mapping = {
        str(row.get("id", "")).lower(): {
            "role": int(row["role"]),
            "is_active": bool(row.get("is_active", True)),
        }
        for row in rows
        if row.get("id") is not None and row.get("role") is not None
    }
    cache[project_id] = mapping
    return mapping


async def _ensure_memberships(
    client: PlaneClient,
    projects: list[tuple[str, str]],
    *,
    user_id: str,
    username: str,
    role_int: int,
    result: SeatResult,
    report: ProvisionReport,
    project_roles_cache: dict[str, dict[str, dict] | None],
) -> None:
    """Ensure project membership for every resolved project.

    An UNAMBIGUOUS duplicate add is "exists"; CE's GENERIC 400
    (``"The payload is not valid"`` — the ``BaseAPIView`` mapping for
    *any* ``IntegrityError`` from the view, not only the duplicate
    constraint) counts as a duplicate ONLY after a lite row (active or
    inactive) confirms the membership actually exists — otherwise the
    error surfaces, because trusting the body would report "exists" for
    an add that did not happen.

    A pre-existing membership deactivated in the Plane UI reports
    ``membership="inactive"`` with a loud note: the seat has NO project
    access, and the public API cannot reactivate the row — the pinned
    ``ProjectMemberSerializer`` writes only ``member`` + ``role``
    (``is_active`` is not a serializer field), and a re-add trips the
    (project, member) unique constraint.  Reactivation is a Plane-UI
    action.

    Project-role drift on a pre-existing membership is reported as a
    note, not repaired — a KNOWN deviation from the errata's "REPAIR
    for project-role via the pinned PATCH" ruling, because the pinned
    surface cannot key the repair: ``ProjectMemberDetailAPIEndpoint``'s
    PATCH does write ``role``, but it is keyed by the ``ProjectMember``
    ROW pk, and NO pinned read serves that pk for a pre-existing row
    (``ProjectMemberLiteAPISerializer`` overrides ``id`` with
    ``source="member.id"`` — the USER uuid; the list endpoint serves
    ``UserLiteSerializer`` rows; the pk appears only in the create
    echo).  A fork patch exposing the row pk on the lite listing would
    make the repair implementable.
    """
    added = False
    exists = False
    inactive = False
    for identifier, project_id in projects:
        try:
            resp = await client.add_project_member(
                project_id, member_id=user_id, role=role_int
            )
        except PlaneProvisionError as exc:
            if exc.status != 400 or "payload is not valid" not in str(exc).lower():
                raise
            roles = await _project_role_map(client, project_id, project_roles_cache)
            if roles is None or user_id.lower() not in roles:
                # No row backs the "duplicate" reading — the generic 400
                # came from some OTHER IntegrityError; surface it.
                raise
            resp = None
        if resp is not None:
            added = True
            continue
        exists = True
        roles = await _project_role_map(client, project_id, project_roles_cache)
        if roles is None:
            continue
        row = roles.get(user_id.lower())
        if row is None:
            continue
        if not row["is_active"]:
            inactive = True
            report.notes.append(
                f"{username}: project '{identifier}' membership is "
                "DEACTIVATED (removed in the Plane UI) — the seat has no "
                "access to the project, and the public API cannot reactivate "
                "a ProjectMember row (is_active is not writable; a re-add "
                "trips the duplicate constraint): re-activate the member in "
                "Plane's project settings"
            )
            logger.warning(
                "plane_provision_membership_inactive",
                username=username,
                project=identifier,
            )
            continue
        have = row["role"]
        if have != role_int:
            have_name = _ROLE_NAME_BY_INT.get(have, str(have))
            want_name = _ROLE_NAME_BY_INT.get(role_int, str(role_int))
            report.notes.append(
                f"{username}: project '{identifier}' role drifted (has "
                f"{have_name}, config wants {want_name}) — the public API "
                "cannot update a pre-existing project membership (PATCH is "
                "keyed by the ProjectMember row pk, which no list endpoint "
                "exposes; would need a fork patch)"
            )
    result.membership = (
        "inactive"
        if inactive
        else ("added" if added else ("exists" if exists else "skipped"))
    )


async def _revoke_other_managed_tokens(
    client: PlaneClient,
    *,
    user_id: str,
    username: str,
    label: str,
    tokens: list[dict],
    keep_id: object,
    report: ProvisionReport,
) -> None:
    """Revoke every OTHER active managed-label token (B7 cleanup).

    Historic double-minting leaves parallel active tokens whose values
    are unrecoverable; after a rotate/mint the survivors are dead
    weight and an unnecessary credential surface, so they are revoked.
    """
    others = [
        t
        for t in tokens
        if t.get("label") == label and t.get("is_active") and t.get("id") != keep_id
    ]
    for token in others:
        await client.revoke_service_account_token(user_id, str(token["id"]))
    if others:
        report.notes.append(
            f"{username}: revoked {len(others)} stale managed token(s) "
            f"(label '{label}') left over from earlier runs"
        )


def _note_expiring_tokens(
    tokens: list[dict], *, username: str, label: str, report: ProvisionReport
) -> None:
    """The token-expiry report for a skipped seat (needs the fork's
    token-lifecycle routes).

    A recorded var whose token expired would otherwise stay ``skipped``
    forever with no signal; Plane hands us ``expired_at`` +
    ``is_active`` for free, so warn on active managed tokens already
    expired or expiring within :data:`_TOKEN_EXPIRY_WARNING_DAYS`.
    """
    now = datetime.now(UTC)
    horizon = now + timedelta(days=_TOKEN_EXPIRY_WARNING_DAYS)
    for token in tokens:
        if token.get("label") != label or not token.get("is_active"):
            continue
        raw = token.get("expired_at")
        if not raw:
            continue
        try:
            expires = datetime.fromisoformat(str(raw))
        except ValueError:
            continue
        if expires.tzinfo is None:
            expires = expires.replace(tzinfo=UTC)
        if expires <= now:
            report.notes.append(
                f"{username}: managed token expired {expires.date().isoformat()}"
                " — re-run with --rotate"
            )
        elif expires <= horizon:
            report.notes.append(
                f"{username}: managed token expires "
                f"{expires.date().isoformat()} (within "
                f"{_TOKEN_EXPIRY_WARNING_DAYS} days) — re-run with --rotate"
            )


async def _ensure_token(
    client: PlaneClient,
    *,
    user_id: str,
    username: str,
    handle: str,
    token_vars: list[str],
    sink: TokenSink,
    rotate: bool,
    expiry_days: int,
    f10: bool,
    degraded_seats: list[str],
    result: SeatResult,
    report: ProvisionReport,
) -> None:
    """Mint/rotate an EXISTING account's token into its ``${VAR}``s.

    (A freshly created account never reaches here — its single-show
    creation token is recorded by ``_reconcile_seat`` the moment the
    create returns, BEFORE memberships, so no later failure can discard
    it.)  Everything here needs the fork's token-lifecycle routes:
    with ``rotate``, the active
    managed-label token is rotated with an EXPLICIT fresh expiry window
    (never the omit/inherit form — inheriting would reproduce GitLab's
    bare-rotate trap in mirror image), and stale parallel managed
    tokens are revoked.  An unrecorded var whose account
    holds an ACTIVE managed token is ``needs_rotate`` + a loud note on
    a plain run — the only recovery is rotating the live token, which
    invalidates the value the running engine holds, so it is exactly as
    destructive as the webhook recreate and gated the same way
    (behind ``--rotate``).  A genuine mint, when NO active
    managed token exists, stays automatic — nothing live can be
    invalidated.  A rotate 409 (someone else rotated/revoked
    concurrently) falls through to mint; an explicit expiry also
    makes the rotate 400 ``SOURCE_TOKEN_EXPIRY_ELAPSED`` unreachable.
    In degraded service-account-only mode (no token-lifecycle routes)
    an unrecorded var is ``blocked`` — could not provision, distinct
    from ``skipped`` = already provisioned.
    """
    result.token_vars = token_vars
    if not token_vars:
        result.token_action = "none"
        return

    label = _managed_label(handle)

    async def _rotate_managed(tokens: list[dict]) -> bool:
        """Rotate the active managed token; record into ALL vars.

        Returns False on a 409 (token deactivated concurrently) so the
        caller falls through to minting.  Any previously recorded var
        value dies with the source token, so every referencing var is
        overwritten, and stale parallel managed tokens are revoked.
        """
        managed = [t for t in tokens if t.get("label") == label and t.get("is_active")]
        if not managed:
            return False
        source = managed[0]
        try:
            rotated = await client.rotate_service_account_token(
                user_id, str(source["id"]), expired_at=_expiry(expiry_days)
            )
        except PlaneProvisionError as exc:
            if exc.status != 409:
                raise
            return False
        for var in token_vars:
            await sink.record(var, str(rotated.get("token", "")))
        await _revoke_other_managed_tokens(
            client,
            user_id=user_id,
            username=username,
            label=label,
            tokens=tokens,
            keep_id=source.get("id"),
            report=report,
        )
        result.token_action = "rotated"
        return True

    async def _mint(record_vars: list[str]) -> None:
        minted = await client.create_service_account_token(
            user_id, label=label, expired_at=_expiry(expiry_days)
        )
        for var in record_vars:
            await sink.record(var, str(minted.get("token", "")))
        result.token_action = "minted"

    if rotate:
        # Token-lifecycle routes are guaranteed here: the preflight
        # aborts a --rotate run pre-mutation when they are absent.
        tokens = await client.list_service_account_tokens(user_id)
        if await _rotate_managed(tokens):
            return
        # No active managed token (or it was deactivated concurrently):
        # the rotation intent still wants a fresh value everywhere.
        await _mint(token_vars)
        return

    needs = [var for var in token_vars if not sink.existing(var)]
    if not needs:
        result.token_action = "skipped"
        if f10:
            tokens = await client.list_service_account_tokens(user_id)
            _note_expiring_tokens(tokens, username=username, label=label, report=report)
        return

    # Mint-on-existing (lost env file, second machine) — needs the
    # fork's token-lifecycle routes.
    if not f10:
        result.token_action = "blocked"
        degraded_seats.append(username)
        return
    tokens = await client.list_service_account_tokens(user_id)
    if any(t.get("label") == label and t.get("is_active") for t in tokens):
        # B6 symmetry: the only recovery is rotating the LIVE managed
        # token — destructive, so a plain run never does it.
        result.token_action = "needs_rotate"
        report.notes.append(
            f"{username}: "
            + ", ".join(f"${{{var}}}" for var in needs)
            + " not recorded here, but an active managed token exists — pass "
            "--rotate to mint a fresh value; this invalidates the value the "
            "running engine holds"
        )
        logger.warning(
            "plane_provision_token_needs_rotate", username=username, vars=needs
        )
        return
    await _mint(needs)


#: Scheme → default port, for :func:`_normalize_hook_url`'s port
#: equivalence (``https://host:443/…`` ≡ ``https://host/…``).
_DEFAULT_PORTS = {"http": 80, "https": 443}


def _normalize_hook_url(url: str) -> str:
    """Normalized webhook-URL form for matching (M12).

    Plane's duplicate-URL constraint is byte-exact, so
    ``https://engine/webhooks/plane`` and ``…/plane/`` are two distinct
    hooks that BOTH fire.  Matching normalizes: the trailing slash;
    scheme + HOST case (case-insensitive per RFC 3986 — userinfo is
    case-SENSITIVE and is preserved verbatim); an explicit default port
    (``:443`` on https, ``:80`` on http — equivalent to no port);
    and duplicate path slashes (``//webhooks//plane`` serves the same
    resource) — so the reconcile finds its hook regardless of spelling
    drift.  A syntactically invalid netloc (unparseable port) falls
    back to verbatim netloc matching: it cannot equal any well-formed
    spelling anyway.
    """
    parts = urlsplit(url.strip())
    scheme = parts.scheme.lower()
    try:
        port = parts.port
    except ValueError:
        netloc = parts.netloc
    else:
        host = (parts.hostname or "").lower()
        if port is not None and port != _DEFAULT_PORTS.get(scheme):
            host = f"{host}:{port}"
        userinfo, sep, _ = parts.netloc.rpartition("@")
        netloc = f"{userinfo}{sep}{host}"
    path = re.sub(r"/{2,}", "/", parts.path).rstrip("/")
    return urlunsplit((scheme, netloc, path, parts.query, ""))


def _check_page_echo(resp: dict, report: ProvisionReport) -> None:
    """Page-webhook runtime detection: the fork must echo ``page: true``.

    A server without page-webhook support silently drops the unknown
    ``page`` field (DRF
    ignores it, no 400) and the hook is created with ``page=False`` —
    zero page events delivered, killing the Tool Skills sync and page
    routing.  The only reliable signal is the real create/update echo.
    """
    if resp.get("page") is True:
        return
    note = (
        "webhook `page` entity toggle not supported by this Plane instance "
        "— page events will NOT be delivered, breaking the "
        "Tool Skills sync and page routing; deploy the crewlet/plane fork "
        "(`preview`)"
    )
    if note not in report.notes:
        report.notes.append(note)
    logger.warning("plane_provision_webhook_page_unsupported")


async def _ensure_webhook(
    client: PlaneClient,
    *,
    webhook_url: str,
    secret_vars: list[str],
    sink: TokenSink,
    rotate: bool,
    recreate_webhook: bool,
    report: ProvisionReport,
) -> None:
    """Ensure the one workspace webhook, capturing its secret.

    Plane generates ``secret_key`` server-side and returns it exactly
    once at creation (the inversion of GitLab, where crewlet supplies
    the secret and can re-stamp it on update), so:

    - no hook → create, capture the secret into the ``${VAR}`` behind
      ``integrations.plane.webhook_secret``;
    - hook exists + secret recorded → PATCH toggles + ``is_active``
      (re-enable after Plane's 5-retry auto-disable — the transport's
      repair duty);
    - hook exists + secret UNRECORDED (lost env file, second machine)
      → a dead end (update-by-URL can never re-emit the secret; the
      engine 401s every delivery until Plane auto-disables the hook).
      The destructive recovery — delete + recreate for a fresh secret
      — is gated behind ``--recreate-webhook`` because it
      invalidates the secret every OTHER deployment holds; without the
      flag the run emits a loud note instead;
    - ``--rotate`` → delete + recreate (secret rotation).

    The destructive paths delete only an EXACT-URL match, and log the
    deletion BEFORE issuing it; a hook matching only after
    normalization is a near-duplicate the provisioner did not create
    (M12) — it is never deleted, and no replacement is created either
    (two near-duplicate hooks would both fire, double-delivering every
    event).  A create failure AFTER a successful delete aborts with an
    explicit message: the workspace hook is gone, deliveries are down,
    and a re-run restores it.

    A create 409 (the URL raced into existence) re-lists and adopts.
    Multiple hooks matching the normalized URL are noted loudly and
    never auto-deleted (M12 — foreign hooks are not ours to remove).
    """
    hooks = await client.list_webhooks()
    target = _normalize_hook_url(webhook_url)
    matches = [h for h in hooks if _normalize_hook_url(str(h.get("url", ""))) == target]
    if len(matches) > 1:
        report.notes.append(
            f"{len(matches)} webhooks target {webhook_url} (near-duplicate "
            "URLs — Plane's uniqueness is byte-exact, so ALL of them fire, "
            "double-delivering every event): "
            f"{', '.join(str(h.get('url', '')) for h in matches)} — remove "
            "the extras by hand; the provisioner never deletes a hook it "
            "did not create"
        )
    exact = next((h for h in matches if str(h.get("url", "")) == webhook_url), None)
    match = exact if exact is not None else (matches[0] if matches else None)

    async def _capture(secret: str) -> None:
        for var in secret_vars:
            await sink.record(var, secret)
            # Exported for symmetry with the GitLab CLI's signing-secret
            # env export; nothing in this process re-resolves the raw
            # config block, but a same-shell engine start after the CLI
            # exits picks the value up from the flushed sink — the report
            # tells the operator to source the env file and restart.
            os.environ[var] = secret
        logger.info("plane_provision_secret_captured", vars=secret_vars)
        report.notes.append(
            "webhook secret captured into "
            + ", ".join(f"${{{var}}}" for var in secret_vars)
            + " — source the env file and restart the engine so deliveries "
            "verify"
        )

    async def _create(description: str) -> None:
        try:
            created = await client.create_webhook(
                url=webhook_url, is_active=True, **_HOOK_ENTITY_TOGGLES
            )
        except PlaneProvisionError as exc:
            if exc.status != 409:
                raise
            # Idempotency backstop: the URL raced into existence between
            # the list and the create — adopt it (its secret is not ours).
            adopted = next(
                (
                    h
                    for h in await client.list_webhooks()
                    if _normalize_hook_url(str(h.get("url", ""))) == target
                ),
                None,
            )
            if adopted is None:
                raise
            report.hooks.append(f"{webhook_url} (adopted)")
            report.notes.append(
                "webhook appeared concurrently (409 on create); adopted it — "
                "its secret is NOT recorded here; re-run with "
                "--recreate-webhook if this deployment must verify deliveries"
            )
            return
        await _capture(str(created.get("secret_key", "")))
        _check_page_echo(created, report)
        report.hooks.append(f"{webhook_url} ({description})")

    def _near_duplicate_only(flag: str) -> bool:
        """The M12 guard for the destructive paths.

        True when the only match is a near-duplicate (normalized-equal,
        not byte-equal): it is never deleted — the provisioner did not
        create it — and no replacement is created either, since both
        would fire and double-deliver every event.
        """
        if match is None or exact is not None:
            return False
        report.hooks.append(f"{webhook_url} (near-duplicate only, untouched)")
        report.notes.append(
            f"{flag} matched only a near-duplicate hook "
            f"({match.get('url', '')}) whose URL is not byte-identical to "
            f"{webhook_url} — it was NOT deleted (the provisioner never "
            "deletes a hook it did not create) and no replacement was "
            "created (both would fire, double-delivering every event); "
            "align the hook URL by hand and re-run"
        )
        return True

    async def _delete_then_create(description: str) -> None:
        """Delete the EXACT match, then recreate — abort loudly on failure.

        The deletion is logged BEFORE it is issued so a trace exists
        even if the process dies mid-recreate; a create failure after
        the successful delete leaves the workspace with NO hook, so it
        aborts with a message saying exactly that.
        """
        logger.warning(
            "plane_provision_webhook_deleting",
            url=str(exact.get("url", "")),
            webhook_id=str(exact["id"]),
        )
        await client.delete_webhook(str(exact["id"]))
        try:
            await _create(description)
        except PlaneProvisionError as exc:
            raise PlaneProvisionAborted(
                "the existing workspace webhook was DELETED but the "
                f"replacement could not be created ({exc}) — Plane "
                "deliveries are DOWN until the hook is restored; fix the "
                "cause and re-run `crewlet plane provision --webhook-url "
                "...` to recreate it"
            ) from exc

    if rotate:
        if _near_duplicate_only("--rotate"):
            return
        if exact is not None:
            await _delete_then_create("recreated, secret rotated")
        else:
            await _create("recreated, secret rotated")
        return
    if match is None:
        await _create("created, secret captured")
        return

    unrecorded = [var for var in secret_vars if not sink.existing(var)]
    if unrecorded:
        if recreate_webhook:
            if _near_duplicate_only("--recreate-webhook"):
                return
            await _delete_then_create("recreated, secret captured")
            report.notes.append(
                "webhook recreated to mint a fresh secret (brief delivery gap)"
            )
        else:
            report.hooks.append(f"{webhook_url} (exists, secret UNRECORDED)")
            report.notes.append(
                "webhook exists but its secret is not recorded here (Plane "
                "secrets are single-show; the env file — not the shell — is "
                "the source of truth) — pass --recreate-webhook to mint a "
                "fresh one; note this invalidates the secret every other "
                "deployment holds"
            )
        return

    was_active = bool(match.get("is_active", True))
    updated = await client.update_webhook(
        str(match["id"]), is_active=True, **_HOOK_ENTITY_TOGGLES
    )
    _check_page_echo(updated, report)
    report.hooks.append(
        f"{webhook_url} " + ("(updated)" if was_active else "(re-enabled)")
    )


async def _decommission(
    client: PlaneClient,
    members: list[dict],
    *,
    prefix: str,
    desired_usernames: set[str],
    report: ProvisionReport,
) -> None:
    """Retire prefixed accounts whose seats left the config (needs the
    fork's token-lifecycle routes).

    Requires ``provisioning.username_prefix`` so managed accounts can
    be identified safely.  The endpoint's own ``NOT_A_SERVICE_ACCOUNT``
    400 guard means a prefixed HUMAN can never be deleted — a 400 is
    recorded as a note, not an error; a 404 is an already-decommissioned
    account (idempotent re-run).  Decommission is IRREVERSIBLE for the
    username: the deactivated user row keeps it, so re-adding the seat
    later 409s (see the create-path 409 copy).
    """
    if not prefix:
        report.notes.append(
            "decommission skipped: set provisioning.username_prefix so managed "
            "accounts can be identified safely (refusing to delete un-prefixed "
            "service accounts)"
        )
        return
    for row in members:
        username = str(row.get("username", ""))
        if not username.startswith(prefix) or username.lower() in desired_usernames:
            continue
        try:
            await client.delete_service_account(str(row["id"]))
        except PlaneProvisionError as exc:
            if exc.status == 400:
                report.notes.append(
                    f"decommission refused for '{username}': not a service "
                    "account (Plane's guard — humans are never deleted)"
                )
                continue
            if exc.status == 404:
                continue  # already decommissioned — idempotent re-run
            raise
        report.decommissioned.append(username)
        logger.info("plane_provision_decommissioned", username=username)


def _human_seat_notes(
    org: object, members: list[dict], report: ProvisionReport
) -> None:
    """Validate human seats' ``contact.plane_user_id`` against members.

    Humans are validated, never created.  Humans without the field are
    silent — the member table at the end of the report is the fill-in
    aid.
    """
    member_ids = {str(row.get("id", "")).lower() for row in members}
    for role in org.all_roles():
        if getattr(role, "kind", "agent") != "human":
            continue
        contact = getattr(role, "contact", None)
        raw = getattr(contact, "plane_user_id", "") if contact is not None else ""
        if not raw:
            continue
        resolved = resolve_env_scalar(raw).strip().lower()
        handle = role.get_handle()
        if not resolved:
            report.notes.append(
                f"human seat '{handle}': contact.plane_user_id references an "
                f"unset variable ({raw}) — cannot validate workspace membership"
            )
            continue
        if resolved not in member_ids:
            report.notes.append(
                f"human seat '{handle}': plane_user_id {resolved} is not a "
                "workspace member — invite them in Plane or fix "
                "contact.plane_user_id (see the member table below)"
            )


def _member_table(members: list[dict], desired_usernames: set[str]) -> list[dict]:
    """The member table rows the report ends with.

    Every workspace member as display name / username / user UUID /
    role, with provisioner-managed rows marked — the table exists so
    founders can copy the human UUIDs into ``contact.plane_user_id``.
    """
    rows: list[dict] = []
    for row in members:
        username = str(row.get("username", ""))
        role_int = row.get("role")
        rows.append(
            {
                "display_name": str(row.get("display_name", "")),
                "username": username,
                "id": str(row.get("id", "")),
                "role": _ROLE_NAME_BY_INT.get(role_int, str(role_int)),
                "managed": username.lower() in desired_usernames,
            }
        )
    return rows


async def provision(
    client: PlaneClient,
    org: object,
    plane_config: object,
    *,
    webhook_url: str,
    sink: TokenSink,
    rotate: bool = False,
    decommission_removed: bool = False,
    create_projects: bool = False,
    recreate_webhook: bool = False,
    token_expiry_days: int = 364,
    engine_token_ref: str = "",
    webhook_secret_ref: str = "",
) -> ProvisionReport:
    """Reconcile the org's agent seats into Plane.  See module docstring.

    ``engine_token_ref`` / ``webhook_secret_ref`` are the RAW
    (unresolved) values of ``integrations.plane.token`` /
    ``integrations.plane.webhook_secret`` — their ``${VAR}`` references
    ARE the minting contract.  When ``engine_token_ref`` references a
    ``${VAR}``, a dedicated ``crewlet-engine`` service account is
    provisioned at workspace role ``member`` and made a project member
    everywhere; ``member`` (not ``guest``) because Plane has no
    Reporter-style read-only role and guest visibility is restricted to
    assigned items under common project settings, which would break
    subscriber fan-out, member/project resolution, and page fetch —
    ``member`` is the least role that guarantees the engine's read
    paths.  A literal (non-``${VAR}``) engine token leaves the engine
    account untouched (GitLab parity).
    """
    provisioning = getattr(plane_config, "provisioning", None)
    prefix = str(getattr(provisioning, "username_prefix", "") or "")
    project_identifiers = list(getattr(provisioning, "projects", []) or [])
    default_role = _validated_role(
        getattr(provisioning, "role", "member") or "member", "provisioning.role"
    )
    role_overrides = {
        handle: _validated_role(name, f"provisioning.roles['{handle}']")
        for handle, name in (getattr(provisioning, "roles", {}) or {}).items()
    }

    # The webhook secret must be EXACTLY one whole-value ${VAR} reference:
    # an embedded ("wh-${VAR}") or multi-reference ("${A}${B}") form passes
    # a mere contains-a-ref check but resolves to a CONCATENATION that can
    # never equal the captured secret — every delivery would fail HMAC
    # verification until Plane auto-disables the hook.
    secret_var = sole_env_var(webhook_secret_ref)
    if webhook_url and not secret_var:
        raise PlaneProvisionAborted(
            "integrations.plane.webhook_secret is not exactly one ${VAR} "
            "reference — Plane generates the webhook secret server-side and "
            "returns it exactly once, so a literal, embedded "
            '("wh-${VAR}") or multi-reference ("${A}${B}") value can never '
            "match a hook this CLI creates; set webhook_secret to a single "
            'whole-value reference (e.g. "${PLANE_WEBHOOK_SECRET}") so the '
            "provisioner can capture the generated secret into it"
        )
    secret_vars = [secret_var] if secret_var else []

    report = ProvisionReport()
    desired_usernames: set[str] = set()
    degraded_seats: list[str] = []
    project_roles_cache: dict[str, dict[str, dict] | None] = {}
    #: ``${VAR}`` → first claiming handle: one variable cannot record two
    #: seats' tokens (the seats would silently share one identity — the
    #: later write wins and the earlier seat's token is orphaned).
    var_claims: dict[str, str] = {}

    try:
        # Preflight 1 — capabilities, BEFORE any mutation: stock CE
        # aborts with one actionable message, a non-admin operator aborts
        # with the permission fix, and --rotate/--decommission-removed
        # abort when the token-lifecycle routes are absent instead of
        # failing N times mid-run.
        f10 = await _preflight_capabilities(
            client, rotate=rotate, decommission_removed=decommission_removed
        )
        if not f10:
            report.notes.append(
                "service-account token-lifecycle routes absent: running in "
                "degraded service-account-only mode "
                "(account creation mints token #1; no rotate / re-mint / "
                "decommission)"
            )

        # Preflight 2 — webhook API, only when this run touches it.
        if webhook_url:
            try:
                await client.list_webhooks()
            except PlaneProvisionError as exc:
                if exc.status == 404:
                    raise PlaneProvisionAborted(
                        "no webhook API at this Plane instance (HTTP 404) — "
                        "the workspace-webhook surface is provided by the "
                        "crewlet/plane fork; deploy it (`preview`) or drop "
                        "--webhook-url"
                    ) from exc
                if exc.status == 403:
                    raise PlaneProvisionAborted(
                        "operator token is not a workspace admin (the webhook "
                        "API requires Plane's workspace-owner permission)"
                    ) from exc
                raise

        # Preflight 3 — members enumeration (strict) + the
        # member-identity hard requirement; also the lookup anchor for
        # every seat.
        members = await _fetch_members(client)
        members_by_username = {
            str(row.get("username", "")).lower(): row for row in members
        }

        # Preflight 4 — declared projects must exist (or be created).
        projects = await _resolve_projects(
            client,
            project_identifiers,
            create_projects=create_projects,
            report=report,
        )

        def _shared_var_error(
            handle: str, token_vars: list[str], result: SeatResult
        ) -> bool:
            """Seat error when a ``${VAR}`` is already claimed by another seat."""
            shared = next(
                (
                    var
                    for var in token_vars
                    if var in var_claims and var_claims[var] != handle
                ),
                None,
            )
            if shared is None:
                for var in token_vars:
                    var_claims.setdefault(var, handle)
                return False
            result.account_action = "error"
            result.membership = "skipped"
            result.token_vars = token_vars
            result.error = (
                f"${{{shared}}} is referenced by both '{var_claims[shared]}' "
                f"and '{handle}' — one variable cannot record two seats' "
                "tokens (the seats would share one identity, and one token "
                "would be silently discarded); give each seat its own ${VAR}"
            )
            logger.error(
                "plane_provision_shared_token_var",
                var=shared,
                seats=[var_claims[shared], handle],
            )
            return True

        async def _reconcile_seat(
            *,
            handle: str,
            username: str,
            display_name: str,
            workspace_role: str,
            token_vars: list[str],
            result: SeatResult,
        ) -> None:
            if not token_vars and username.lower() not in members_by_username:
                # A create would mint the account's single-show token #1
                # with NOWHERE to record it — the credential would be
                # orphaned invisibly.  An EXISTING account still gets its
                # membership reconcile below.
                result.account_action = "skipped"
                result.membership = "skipped"
                report.notes.append(
                    f"{username}: mcp_env.plane declares no ${{VAR}} "
                    "reference, so the account was NOT created (the create "
                    "response's single-show token could not be recorded "
                    "anywhere and the credential would be orphaned) — add a "
                    'reference (e.g. PLANE_API_KEY: "${PLANE_TOKEN_X}") and '
                    "re-run"
                )
                logger.warning("plane_provision_seat_no_token_vars", username=username)
                return
            creation_token = await _ensure_account(
                client,
                members_by_username=members_by_username,
                username=username,
                display_name=display_name,
                workspace_role=workspace_role,
                handle=handle,
                result=result,
                report=report,
            )
            if result.user_id is None:
                return
            result.token_vars = token_vars
            if creation_token:
                # The single-show creation token is recorded the moment it
                # exists — BEFORE memberships, so a later per-seat failure
                # (e.g. the project-admin-gated member POST 403ing) can
                # never discard the brand-new account's only credential.
                # All referencing vars are overwritten, stale values
                # included: a brand-new account's old var value cannot
                # possibly be valid (L2).
                for var in token_vars:
                    await sink.record(var, creation_token)
                result.token_action = "minted"
            await _ensure_memberships(
                client,
                projects,
                user_id=result.user_id,
                username=username,
                role_int=SERVICE_ACCOUNT_ROLES[workspace_role],
                result=result,
                report=report,
                project_roles_cache=project_roles_cache,
            )
            if not creation_token:
                await _ensure_token(
                    client,
                    user_id=result.user_id,
                    username=username,
                    handle=handle,
                    token_vars=token_vars,
                    sink=sink,
                    rotate=rotate,
                    expiry_days=token_expiry_days,
                    f10=f10,
                    degraded_seats=degraded_seats,
                    result=result,
                    report=report,
                )

        for role in org.all_roles():
            if getattr(role, "kind", "agent") != "agent":
                continue
            if not role.mcp_env.get("plane"):
                continue
            handle = role.get_handle()
            username = f"{prefix}{handle}"
            desired_usernames.add(username.lower())
            result = SeatResult(handle=handle, username=username)
            token_vars = seat_token_vars(role)
            if _shared_var_error(handle, token_vars, result):
                report.seats.append(result)
                continue
            try:
                await _reconcile_seat(
                    handle=handle,
                    username=username,
                    display_name=role.name,
                    workspace_role=role_overrides.get(handle, default_role),
                    token_vars=token_vars,
                    result=result,
                )
            except PlaneProvisionError as exc:
                # The account state stays truthful: a seat whose account
                # was created (or existed) before the failing step keeps
                # that state — the error itself lives in `error`, which
                # is what drives the exit code.
                if result.account_action == "pending":
                    result.account_action = "error"
                result.error = str(exc)
                logger.error(
                    "plane_provision_seat_failed", handle=handle, error=str(exc)
                )
            report.seats.append(result)

        # Engine routing/read account: when integrations.plane.token
        # references a ${VAR}, provision the read-path identity too.
        engine_vars = referenced_env_vars({"token": engine_token_ref})
        if engine_vars:
            engine_username = f"{prefix}{ENGINE_ACCOUNT_HANDLE}"
            desired_usernames.add(engine_username.lower())
            result = SeatResult(handle=ENGINE_ACCOUNT_HANDLE, username=engine_username)
            if _shared_var_error(ENGINE_ACCOUNT_HANDLE, engine_vars, result):
                report.seats.append(result)
            else:
                try:
                    await _reconcile_seat(
                        handle=ENGINE_ACCOUNT_HANDLE,
                        username=engine_username,
                        display_name="Crewlet Engine (routing)",
                        # `member`, never guest and never the config default —
                        # see the provision() docstring for the rationale.
                        workspace_role="member",
                        token_vars=engine_vars,
                        result=result,
                    )
                except PlaneProvisionError as exc:
                    if result.account_action == "pending":
                        result.account_action = "error"
                    result.error = str(exc)
                    logger.error(
                        "plane_provision_engine_account_failed",
                        username=engine_username,
                        error=str(exc),
                    )
                report.seats.append(result)

        if degraded_seats:
            report.notes.append(
                f"{len(degraded_seats)} seat(s) need a token minted on an "
                "existing account, which requires the fork's service-account "
                "token-lifecycle routes: "
                f"{', '.join(degraded_seats)} — deploy the crewlet/plane fork "
                "(`preview`) or restore the recorded env values"
            )

        if webhook_url:
            await _ensure_webhook(
                client,
                webhook_url=webhook_url,
                secret_vars=secret_vars,
                sink=sink,
                rotate=rotate,
                recreate_webhook=recreate_webhook,
                report=report,
            )
        elif rotate:
            # M5: --rotate promises all three credential classes, but the
            # webhook secret only rotates through the webhook step.
            report.notes.append(
                "the webhook secret was NOT rotated: --rotate covers it only "
                "together with --webhook-url (the webhook step is skipped "
                "without it)"
            )

        if decommission_removed:
            await _decommission(
                client,
                members,
                prefix=prefix,
                desired_usernames=desired_usernames,
                report=report,
            )

        _human_seat_notes(org, members, report)

        # The member table reflects FINAL state (accounts created this
        # run appear, decommissioned ones are gone).
        report.members = _member_table(
            await client.list_workspace_members(), desired_usernames
        )

    except BaseException:
        # Credential values recorded before a mid-run abort are
        # unretrievable from Plane (single-show) — flush them before
        # propagating.  A flush failure here is logged, never raised, so
        # the abort's original cause is what the operator sees.
        try:
            await sink.flush()
        except Exception:
            logger.exception("plane_provision_flush_failed")
        raise

    await sink.flush()
    return report
