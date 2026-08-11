"""Reconcile company config → GitLab state for ``crewlet gitlab provision``.

Idempotent: given the org and the ``integrations.gitlab.provisioning``
block, ensure one service account per agent seat, group/project
membership, one PAT per seat (minted from the config's own ``${VAR}``
references), and project/group webhooks. Re-runs no-op; ``rotate``
re-mints tokens; ``decommission_removed`` deletes accounts whose seats
left the config. See ``docs/integrations/gitlab.md``.

Token *values* leave this module only through a :class:`TokenSink`
(env-file or stdout) — GitLab never returns a token after creation, so a
seat whose ``${VAR}`` already has a value is skipped, which is what makes
minting idempotent.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta

from crewlet._logging import get_logger
from crewlet.gitlab.client import ACCESS_LEVELS, GitLabClient, GitLabProvisionError
from crewlet.provisioning import TokenSink, referenced_env_vars

logger = get_logger("gitlab.provision")


class GitLabProvisionAborted(RuntimeError):
    """Provisioning cannot proceed for a reason the operator must resolve.

    Carries a plain, actionable message (identity verification not done,
    operator not an instance admin, ...) — distinct from
    :class:`~crewlet.gitlab.client.GitLabProvisionError`'s raw
    ``method path → status: body`` format. Raised by preflight checks so
    the CLI can print one clear next step instead of N identical API
    failures buried in the seat report.
    """


# Project-hook event flags the engine's parser acts on. Push is off by
# default (inbox noise); the router never routes raw pushes.
_HOOK_EVENTS = {
    "issues_events": True,
    "merge_requests_events": True,
    "note_events": True,
    "pipeline_events": True,
    "emoji_events": True,
    "push_events": False,
    "tag_push_events": False,
}


def seat_token_vars(role: object) -> list[str]:
    """The ``${VAR}`` token references to mint for one seat, deduped.

    A seat's GitLab PAT is referenced in ``mcp_env.gitlab`` (the MCP
    tool credential) and, by convention, again as ``GITLAB_TOKEN`` in
    ``role.sandbox.env`` (the git-auth recipe) — usually the *same*
    ``${VAR}``. Scanning both means a sandbox-authoring seat with no MCP
    tool surface still gets its token minted. Only the conventional
    ``GITLAB_TOKEN`` sandbox key is scanned, never the whole sandbox env,
    so an unrelated secret var there is never turned into a GitLab PAT.
    """
    scan: dict[str, str] = dict(role.mcp_env.get("gitlab") or {})
    sandbox = getattr(role, "sandbox", None)
    sandbox_env = getattr(sandbox, "env", None) if sandbox is not None else None
    if isinstance(sandbox_env, dict) and sandbox_env.get("GITLAB_TOKEN"):
        scan["__sandbox_gitlab_token__"] = sandbox_env["GITLAB_TOKEN"]
    return referenced_env_vars(scan)


@dataclass
class SeatResult:
    """Outcome of provisioning one agent seat."""

    handle: str
    username: str
    account_action: str = "pending"  # created | exists | error
    user_id: int | None = None
    membership: str = "pending"  # added | exists | skipped
    token_vars: list[str] = field(default_factory=list)
    # A token-step failure surfaces via `error` (set by the per-seat
    # handler), never as a token_action state.
    token_action: str = "none"  # minted | rotated | skipped | none
    error: str = ""


@dataclass
class ProvisionReport:
    seats: list[SeatResult] = field(default_factory=list)
    hooks: list[str] = field(default_factory=list)
    decommissioned: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)


def _access_level(handle: str, provisioning: object) -> int:
    overrides = getattr(provisioning, "access_levels", {}) or {}
    name = overrides.get(handle) or getattr(provisioning, "access_level", "developer")
    return ACCESS_LEVELS.get(str(name).lower(), ACCESS_LEVELS["developer"])


def _expiry(days: int) -> str | None:
    """Return an ``expires_at`` date ``days`` ahead, or ``None`` for 0."""
    if days <= 0:
        return None
    return (datetime.now(UTC) + timedelta(days=days)).date().isoformat()


async def _find_account(
    client: GitLabClient, group: str, username: str, *, mode: str
) -> dict | None:
    accounts = (
        await client.list_instance_service_accounts()
        if mode == "instance"
        else await client.list_group_service_accounts(group)
    )
    for acct in accounts:
        if str(acct.get("username", "")).lower() == username.lower():
            return acct
    return None


async def _ensure_account(
    client: GitLabClient,
    group: str,
    *,
    mode: str,
    name: str,
    username: str,
    email: str,
    result: SeatResult,
) -> None:
    existing = await _find_account(client, group, username, mode=mode)
    if existing is not None:
        result.account_action = "exists"
        result.user_id = existing.get("id")
        return
    if mode == "instance":
        created = await client.create_instance_service_account(
            name=name, username=username, email=email
        )
    else:
        created = await client.create_group_service_account(
            group, name=name, username=username, email=email
        )
    result.account_action = "created"
    result.user_id = created.get("id")


async def _ensure_membership(
    client: GitLabClient,
    group: str,
    projects: list[str],
    *,
    user_id: int,
    access_level: int,
    result: SeatResult,
) -> None:
    resp = await client.add_group_member(group, user_id, access_level)
    result.membership = "exists" if resp is None else "added"
    for project in projects:
        await client.add_project_member(project, user_id, access_level)


async def _ensure_token(
    client: GitLabClient,
    group: str,
    *,
    user_id: int,
    scopes: list[str],
    token_vars: list[str],
    sink: TokenSink,
    rotate: bool,
    expiry_days: int,
    result: SeatResult,
    mode: str = "group",
) -> None:
    """Mint/rotate one seat's PAT into its referenced ``${VAR}``s.

    Token endpoints are **mode-aware**: group-created service accounts
    use the group-scoped token API, but instance-level service accounts
    (``--mode instance``) are not *associated* with a group, so the
    group endpoints 404 on them — instance mode uses the admin
    Users/personal_access_tokens APIs instead.
    """
    result.token_vars = token_vars
    if not token_vars:
        result.token_action = "none"
        return

    token_name = f"crewlet-provision:{result.handle}"
    expires_at = _expiry(expiry_days)

    if rotate:
        if mode == "instance":
            tokens = await client.list_user_tokens(user_id)
        else:
            tokens = await client.list_group_service_account_tokens(group, user_id)
        managed = [t for t in tokens if t.get("name") == token_name and t.get("active")]
        if managed:
            if mode == "instance":
                rotated = await client.rotate_token(
                    managed[0]["id"], expires_at=expires_at
                )
            else:
                rotated = await client.rotate_group_service_account_token(
                    group, user_id, managed[0]["id"], expires_at=expires_at
                )
            for var in token_vars:
                await sink.record(var, rotated.get("token", ""))
            result.token_action = "rotated"
            return
        # No managed token to rotate → fall through to minting.

    # Mint only when the referenced var has no value yet (idempotent).
    needs = [var for var in token_vars if not sink.existing(var)]
    if not needs:
        result.token_action = "skipped"
        return
    if mode == "instance":
        created = await client.create_user_token(
            user_id, name=token_name, scopes=scopes, expires_at=expires_at
        )
    else:
        created = await client.create_group_service_account_token(
            group, user_id, name=token_name, scopes=scopes, expires_at=expires_at
        )
    token = created.get("token", "")
    for var in needs:
        await sink.record(var, token)
    result.token_action = "minted"


async def _ensure_webhooks(
    client: GitLabClient,
    group: str,
    projects: list[str],
    *,
    webhook_url: str,
    signing_token: str,
    group_webhook: str,
    report: ProvisionReport,
) -> None:
    flags: dict[str, object] = dict(_HOOK_EVENTS)
    flags["enable_ssl_verification"] = webhook_url.startswith("https")
    if signing_token:
        flags["signing_token"] = signing_token

    async def ensure_project_hook(project: str) -> None:
        hooks = await client.list_project_hooks(project)
        match = next((h for h in hooks if h.get("url") == webhook_url), None)
        if match is not None:
            await client.update_project_hook(
                project, match["id"], url=webhook_url, **flags
            )
            report.hooks.append(f"{project} (updated)")
        else:
            await client.create_project_hook(project, url=webhook_url, **flags)
            report.hooks.append(f"{project} (created)")

    # Group hook first when requested. A group hook already fires for every
    # issue/MR/note/pipeline event in EVERY project of the group and its
    # subgroups (GitLab: "identical webhooks in both a group and a project
    # ... are both triggered for events in that project"), so on success we
    # are DONE — also creating per-project hooks would double every
    # delivery. Per-project is strictly the fallback for when the
    # group-hooks API is unavailable (older/Free self-managed); `auto`
    # tolerates that fallback, `true` demands the group hook.
    if group_webhook in ("auto", "true"):
        try:
            hooks = await client.list_group_hooks(group)
            match = next((h for h in hooks if h.get("url") == webhook_url), None)
            if match is None:
                await client.create_group_hook(group, url=webhook_url, **flags)
                report.hooks.append(f"group:{group} (created)")
            else:
                # Refresh flags/signing_token on re-run (idempotent), the
                # same as ensure_project_hook — otherwise rotating
                # signing_secret would never reach an existing group hook.
                await client.update_group_hook(
                    group, match["id"], url=webhook_url, **flags
                )
                report.hooks.append(f"group:{group} (updated)")
            return
        except GitLabProvisionError as exc:
            if group_webhook == "true":
                raise
            report.notes.append(
                f"group webhook unavailable ({exc.status}); using per-project hooks"
            )

    for project in projects:
        await ensure_project_hook(project)


async def _decommission(
    client: GitLabClient,
    group: str,
    *,
    prefix: str,
    desired_usernames: set[str],
    report: ProvisionReport,
) -> None:
    if not prefix:
        report.notes.append(
            "decommission skipped: set provisioning.username_prefix so managed "
            "accounts can be identified safely (refusing to delete un-prefixed "
            "service accounts)"
        )
        return
    accounts = await client.list_group_service_accounts(group)
    for acct in accounts:
        username = str(acct.get("username", ""))
        if username.startswith(prefix) and username.lower() not in desired_usernames:
            await client.delete_group_service_account(group, acct["id"])
            report.decommissioned.append(username)


async def _preflight_service_accounts(
    client: GitLabClient, group: str, mode: str
) -> None:
    """Probe the service-accounts API once; abort with a clear message on 403.

    Listing service accounts needs the same authorization as creating
    them, so a single list call up front turns a plan/verification gate
    into one actionable error instead of an identical 403 on every seat.
    """
    try:
        if mode == "instance":
            await client.list_instance_service_accounts()
        else:
            await client.list_group_service_accounts(group)
    except GitLabProvisionError as exc:
        if exc.status != 403:
            raise
        if mode == "instance":
            raise GitLabProvisionAborted(
                "GitLab denied the instance service-accounts API (403). "
                "--mode instance requires an instance administrator token; "
                "on GitLab.com use the default group mode instead."
            ) from exc
        raise GitLabProvisionAborted(
            f"GitLab denied the service-accounts API for group '{group}' "
            "(403), even though the operator credential is a group Owner "
            "(other group calls succeed). On GitLab.com this gate is almost "
            "always identity verification: the top-level group Owner must "
            "verify their identity (add a credit card / phone at "
            "https://gitlab.com/-/identity_verification — no charge) before "
            "the service-accounts API is allowed. Verify, then re-run. Also "
            f"confirm '{group}' is a top-level group — service accounts "
            "cannot be managed from a personal namespace or below the top "
            "level."
        ) from exc


async def _preflight_projects(
    client: GitLabClient,
    group: str,
    projects: list[str],
    report: ProvisionReport,
) -> list[str]:
    """Return the subset of ``projects`` that exist; note the ones missing.

    Membership and webhook creation both 404 on a project that does not
    exist, aborting the whole reconcile on the first one. Filtering here
    (and recording a note naming the missing projects) keeps the reconcile
    idempotent: create the projects later, re-run, and they are picked up.
    """
    existing: list[str] = []
    missing: list[str] = []
    for project in projects:
        try:
            await client.get_project(project)
            existing.append(project)
        except GitLabProvisionError as exc:
            if exc.status == 404:
                missing.append(project)
                logger.warning("gitlab_provision_project_missing", project=project)
            else:
                raise
    if missing:
        report.notes.append(
            f"declared projects not found in group '{group}' — create them "
            "first or remove them from integrations.gitlab.provisioning."
            f"projects: {', '.join(missing)}"
        )
    return existing


#: Handle/username stem of the engine's read-only routing account (the
#: ``integrations.gitlab.token`` participants credential).
ENGINE_ACCOUNT_HANDLE = "crewlet-engine"


async def provision(
    client: GitLabClient,
    org: object,
    gitlab_config: object,
    *,
    webhook_url: str,
    sink: TokenSink,
    mode: str = "group",
    rotate: bool = False,
    decommission_removed: bool = False,
    token_expiry_days: int = 364,
    engine_token_ref: str = "",
) -> ProvisionReport:
    """Reconcile the org's agent seats into GitLab. See module docstring.

    ``engine_token_ref`` is the RAW (unresolved) value of
    ``integrations.gitlab.token``; when it references a ``${VAR}``, a
    dedicated ``crewlet-engine`` service account is provisioned with
    Reporter access (read-only: enough for participant lookups, nothing
    else) and its token minted into that var — same contract as the
    per-seat tokens.
    """
    provisioning = getattr(gitlab_config, "provisioning", None)
    if provisioning is None or not getattr(provisioning, "group", ""):
        raise ValueError(
            "integrations.gitlab.provisioning.group is required to provision"
        )
    group = provisioning.group
    prefix = getattr(provisioning, "username_prefix", "") or ""
    projects = list(getattr(provisioning, "projects", []) or [])
    scopes = list(getattr(provisioning, "token_scopes", None) or ["api"])
    group_webhook = getattr(provisioning, "group_webhook", "auto")

    report = ProvisionReport()
    desired_usernames: set[str] = set()

    try:
        # Preflight 1 — service-account capability. Every per-seat create hits
        # the same service_accounts API; probe it once so a plan/verification
        # gate surfaces as ONE actionable message, not N identical 403s. On
        # GitLab.com the top-level group Owner's identity must be verified
        # before this API is allowed even for a valid Owner+api token.
        await _preflight_service_accounts(client, group, mode)

        # Preflight 2 — declared projects must exist. A missing one 404s on
        # both membership and webhook creation and would abort the whole
        # reconcile; drop the missing ones (recording a prominent note) so the
        # accounts, tokens, and the projects that DO exist still reconcile.
        projects = await _preflight_projects(client, group, projects, report)

        for role in org.all_roles():
            if getattr(role, "kind", "agent") != "agent":
                continue
            gitlab_env = role.mcp_env.get("gitlab")
            if not gitlab_env:
                continue
            handle = role.get_handle()
            username = f"{prefix}{handle}"
            desired_usernames.add(username.lower())
            result = SeatResult(handle=handle, username=username)
            try:
                await _ensure_account(
                    client,
                    group,
                    mode=mode,
                    name=role.name,
                    username=username,
                    email=getattr(role, "email", "") or "",
                    result=result,
                )
                if result.user_id is not None:
                    await _ensure_membership(
                        client,
                        group,
                        projects,
                        user_id=result.user_id,
                        access_level=_access_level(handle, provisioning),
                        result=result,
                    )
                    await _ensure_token(
                        client,
                        group,
                        user_id=result.user_id,
                        scopes=scopes,
                        token_vars=seat_token_vars(role),
                        sink=sink,
                        rotate=rotate,
                        expiry_days=token_expiry_days,
                        result=result,
                        mode=mode,
                    )
            except GitLabProvisionError as exc:
                result.account_action = "error"
                result.error = str(exc)
                logger.error(
                    "gitlab_provision_seat_failed", handle=handle, error=str(exc)
                )
            report.seats.append(result)

        # Engine routing account: when integrations.gitlab.token references a
        # ${VAR}, provision the read-only participants-lookup identity too.
        engine_vars = referenced_env_vars({"token": engine_token_ref})
        if engine_vars:
            engine_username = f"{prefix}{ENGINE_ACCOUNT_HANDLE}"
            desired_usernames.add(engine_username.lower())
            result = SeatResult(handle=ENGINE_ACCOUNT_HANDLE, username=engine_username)
            try:
                await _ensure_account(
                    client,
                    group,
                    mode=mode,
                    name="Crewlet Engine (routing)",
                    username=engine_username,
                    email="",
                    result=result,
                )
                if result.user_id is not None:
                    # Reporter: read issues/MRs/participants, write nothing.
                    await _ensure_membership(
                        client,
                        group,
                        projects,
                        user_id=result.user_id,
                        access_level=ACCESS_LEVELS["reporter"],
                        result=result,
                    )
                    await _ensure_token(
                        client,
                        group,
                        user_id=result.user_id,
                        scopes=["read_api"],
                        token_vars=engine_vars,
                        sink=sink,
                        rotate=rotate,
                        expiry_days=token_expiry_days,
                        result=result,
                        mode=mode,
                    )
            except GitLabProvisionError as exc:
                result.account_action = "error"
                result.error = str(exc)
                logger.error(
                    "gitlab_provision_engine_account_failed",
                    username=engine_username,
                    error=str(exc),
                )
            report.seats.append(result)

        if webhook_url:
            await _ensure_webhooks(
                client,
                group,
                projects,
                webhook_url=webhook_url,
                signing_token=str(getattr(gitlab_config, "signing_secret", "") or ""),
                group_webhook=group_webhook,
                report=report,
            )

        if decommission_removed:
            await _decommission(
                client,
                group,
                prefix=prefix,
                desired_usernames=desired_usernames,
                report=report,
            )

    except BaseException:
        # Token values recorded before a mid-run abort (a webhook or
        # decommission failure, an unexpected exception) are
        # unretrievable from GitLab — flush them before propagating.
        # A flush failure here is logged, never raised, so the abort's
        # original cause is what the operator sees.
        try:
            await sink.flush()
        except Exception:
            logger.exception("gitlab_provision_flush_failed")
        raise

    await sink.flush()
    return report
