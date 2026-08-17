"""``crewlet mattermost provision`` — the idempotent seat reconcile.

Follows the Plane / GitLab shape rather than Slack's: Mattermost **is**
its own directory, so a seat is found by looking up a deterministic
username, and reconcile is stateless.  None of Slack's machinery is
needed — there is no app manifest to push, no local ledger to keep
(Mattermost can enumerate its own bots), and no OAuth browser click,
because an admin token can mint a bot's personal access token directly.

What the reconcile does, per Mattermost-enabled agent seat:

1. find-or-create the **bot account** at a deterministic username;
2. re-enable it if a previous run (or a decommission) disabled it — a
   disabled bot still owns its username, so creating over it fails with a
   conflict nothing else would explain;
3. keep its display name and description current;
4. add it to the team and to every configured channel — a bot only
   receives messages from channels it is a member of, so this step is
   what makes the integration work at all;
5. mint its personal access token into the config's own ``${VAR}``,
   write-through, because Mattermost returns the value exactly once.

Drift is reported, never silently repaired (the Plane convention).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet._logging import get_logger
from crewlet.mattermost.client import MattermostClient, MattermostError
from crewlet.provisioning import TokenSink, referenced_env_vars, sole_env_var

logger = get_logger("mattermost.provision")

#: Description stamped on every token this provisioner mints, so an
#: operator reading a bot's token list in the Mattermost console can tell
#: engine-managed credentials from ones a human created by hand.
TOKEN_DESCRIPTION = "crewlet-engine"

#: Description stamped on bot accounts the provisioner creates, for the
#: same reason.
BOT_DESCRIPTION = "Crewlet agent seat"


class MattermostProvisionAborted(RuntimeError):
    """Preflight refused to mutate anything.

    Raised before the first write when the operator credential cannot do
    the job — a half-provisioned fleet is worse than a refusal, because
    the seats that succeeded look healthy while the org silently has no
    coverage for the rest.
    """


def seat_token_vars(role: Any) -> list[str]:
    """The ``${VAR}`` token references to mint for one seat, deduped.

    A Mattermost seat's token is referenced in **two** places for two
    different consumers — the transport identity (authored as
    ``role.integrations.mattermost.bot_token``, materialised by the
    config loader onto ``role.mattermost``) and ``role.mcp_env.mattermost``
    (the MCP tool server) — and by convention they name the same var.
    Scanning both means a seat that has only one of the two still gets
    its token minted.

    Reads the *materialised* ``role.mattermost`` dict, because that is
    what the runtime :class:`~crewlet.org.models.Role` carries; the
    authored ``integrations`` block exists only on the config model.
    """
    scan: dict[str, str] = {}
    identity = dict(getattr(role, "mattermost", None) or {})
    if identity.get("bot_token"):
        scan["__identity_bot_token__"] = identity["bot_token"]
    mcp_env = getattr(role, "mcp_env", None) or {}
    scan.update(dict(mcp_env.get("mattermost") or {}))
    return referenced_env_vars(scan)


@dataclass
class SeatResult:
    """Outcome of provisioning one agent seat."""

    handle: str
    username: str
    bot_action: str = "pending"  # created | exists | re-enabled | error
    user_id: str = ""
    team: str = "pending"  # added | exists | skipped
    channels: list[str] = field(default_factory=list)
    token_vars: list[str] = field(default_factory=list)
    token_action: str = "pending"  # minted | exists | skipped
    error: str = ""
    notes: list[str] = field(default_factory=list)


@dataclass
class ProvisionReport:
    """Everything one reconcile run did, for the CLI to render."""

    seats: list[SeatResult] = field(default_factory=list)
    skipped: list[tuple[str, str]] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    team_id: str = ""

    @property
    def failed(self) -> list[SeatResult]:
        return [s for s in self.seats if s.error]

    @property
    def ok(self) -> bool:
        return not self.failed


async def _preflight(
    client: MattermostClient,
    team: str,
    report: ProvisionReport,
) -> str:
    """Abort-or-degrade checks, before anything is mutated.

    Returns the resolved team id.  Everything checked here is something
    that would otherwise fail midway through the fleet, leaving half of
    it provisioned.
    """
    try:
        me = await client.me()
    except MattermostError as exc:
        raise MattermostProvisionAborted(
            f"admin credential rejected by Mattermost: {exc}"
        ) from exc

    roles = str((me or {}).get("roles") or "")
    if "system_admin" not in roles.split():
        raise MattermostProvisionAborted(
            "the supplied token is not a system admin "
            f"(roles: {roles or 'none'}). Creating bot accounts and minting "
            "their access tokens both require system-admin rights."
        )

    resolved = await client.get_team_by_name(team)
    team_id = str((resolved or {}).get("id") or "")
    if not team_id:
        raise MattermostProvisionAborted(
            f"team {team!r} not found — create it in Mattermost first "
            "(the provisioner never creates top-level tenancy)"
        )

    # Bots are excluded from the active-user cap, so an agent fleet never
    # consumes it.  Report the human headroom anyway: an operator whose
    # workspace is near the wall wants to know before they invite people,
    # not after the server refuses.
    limits = await client.server_limits()
    max_users = limits.get("maxUsersLimit") or limits.get("max_users_limit")
    active = limits.get("activeUserCount") or limits.get("active_user_count")
    if max_users:
        report.notes.append(
            f"server user limit: {active or '?'}/{max_users} active human "
            "users. Bot accounts are excluded from this count, so agent "
            "seats do not consume it."
        )

    return team_id


async def _ensure_bot(
    client: MattermostClient,
    *,
    username: str,
    display_name: str,
    existing: dict[str, dict[str, Any]],
    result: SeatResult,
) -> str:
    """Find-or-create the bot account; returns its user id."""
    bot = existing.get(username)
    if bot is None:
        created = await client.create_bot(username, display_name, BOT_DESCRIPTION)
        result.bot_action = "created"
        return str((created or {}).get("user_id") or "")

    user_id = str(bot.get("user_id") or "")
    result.bot_action = "exists"

    # A disabled bot keeps its username but cannot post or connect. Left
    # alone, the seat would provision "successfully" and then be silent.
    if bot.get("delete_at"):
        await client.enable_bot(user_id)
        result.bot_action = "re-enabled"

    if str(bot.get("display_name") or "") != display_name:
        await client.patch_bot(user_id, display_name=display_name)
        result.notes.append(f"display name updated to {display_name!r}")

    return user_id


async def _ensure_membership(
    client: MattermostClient,
    *,
    team_id: str,
    user_id: str,
    channel_names: list[str],
    result: SeatResult,
) -> None:
    """Put the bot in the team and its channels.

    Membership is what makes the websocket deliver anything: Mattermost
    pushes a post to a connection only when that user is in the channel.
    A seat that provisions cleanly but joins nothing is a bot that never
    hears from anyone.
    """
    try:
        await client.add_team_member(team_id, user_id)
        result.team = "added"
    except MattermostError as exc:
        if exc.status in (400, 409):
            result.team = "exists"
        else:
            raise

    for name in channel_names:
        channel = await client.get_channel_by_name(team_id, name)
        channel_id = str((channel or {}).get("id") or "")
        if not channel_id:
            result.notes.append(f"channel {name!r} not found — skipped")
            continue
        try:
            await client.add_channel_member(channel_id, user_id)
            result.channels.append(name)
        except MattermostError as exc:
            if exc.status in (400, 409):
                result.channels.append(name)
            else:
                result.notes.append(f"channel {name!r}: {exc}")


async def _ensure_token(
    client: MattermostClient,
    *,
    user_id: str,
    token_vars: list[str],
    sink: TokenSink,
    result: SeatResult,
) -> None:
    """Mint the seat's access token into its ``${VAR}``s.

    A var that already carries a value counts as provisioned: Mattermost
    returns a token's value exactly once, so re-minting would strand the
    live credential rather than replace it.
    """
    unset = [var for var in token_vars if not sink.existing(var)]
    if not unset:
        result.token_action = "exists"
        return

    created = await client.create_user_access_token(user_id, TOKEN_DESCRIPTION)
    token = str((created or {}).get("token") or "")
    if not token:
        raise MattermostError("token creation returned no token value")

    # Write-through before anything else can fail: the value is
    # unretrievable from here on, so the window between "minted" and
    # "persisted" is a window in which a crash orphans a live credential.
    for var in unset:
        await sink.record(var, token)
    result.token_action = "minted"


async def provision(
    client: MattermostClient,
    org: Any,
    *,
    team: str,
    sink: TokenSink,
    username_prefix: str = "",
    display_name_suffix: str = "",
    default_channels: list[str] | None = None,
    handles: set[str] | None = None,
) -> ProvisionReport:
    """Reconcile the company config into Mattermost.

    Idempotent and resumable: every step is find-or-create, and a seat
    that fails is recorded and stepped over so one bad seat cannot
    prevent the rest of the fleet from provisioning.
    """
    report = ProvisionReport()
    report.team_id = await _preflight(client, team, report)

    existing_bots = {str(b.get("username") or ""): b for b in await client.list_bots()}

    for role in org.all_roles():
        # get_handle(), NOT .handle: the raw field is EMPTY unless the
        # config pins one explicitly, and the handle is what names the bot
        # account. Reading the field would create every seat as
        # "{prefix}" and collide on the second one.
        handle = role.get_handle()
        if handles is not None and handle not in handles:
            continue
        if getattr(role, "is_human", False):
            continue

        identity = dict(getattr(role, "mattermost", None) or {})
        if not identity:
            continue

        token_vars = seat_token_vars(role)
        if not token_vars:
            report.skipped.append(
                (handle, "credentials are literals — manually managed bot")
            )
            continue
        bot_token_ref = identity.get("bot_token", "")
        if bot_token_ref and not sole_env_var(bot_token_ref):
            report.skipped.append(
                (
                    handle,
                    "bot_token is not a whole-value ${VAR} placeholder, so "
                    "there is nothing to mint into",
                )
            )
            continue

        username = identity.get("username") or f"{username_prefix}{handle}"
        result = SeatResult(handle=handle, username=username, token_vars=token_vars)
        report.seats.append(result)

        try:
            user_id = await _ensure_bot(
                client,
                username=username,
                display_name=f"{role.name}{display_name_suffix}",
                existing=existing_bots,
                result=result,
            )
            if not user_id:
                raise MattermostError("bot account has no user id")
            result.user_id = user_id

            channel_names = list(default_channels or [])
            seat_channel = identity.get("channel", "")
            if seat_channel and seat_channel not in channel_names:
                channel_names.append(seat_channel)
            await _ensure_membership(
                client,
                team_id=report.team_id,
                user_id=user_id,
                channel_names=channel_names,
                result=result,
            )

            await _ensure_token(
                client,
                user_id=user_id,
                token_vars=token_vars,
                sink=sink,
                result=result,
            )
        except MattermostError as exc:
            result.error = str(exc)
            result.bot_action = (
                result.bot_action if result.bot_action != "pending" else "error"
            )
            logger.warning("mattermost_seat_failed", handle=handle, error=str(exc))
        except Exception as exc:  # noqa: BLE001 — one seat must not stop the fleet
            result.error = str(exc)
            logger.exception("mattermost_seat_failed", handle=handle)

    await sink.flush()
    return report


async def decommission(
    client: MattermostClient,
    handles: list[str],
    *,
    username_prefix: str = "",
) -> list[tuple[str, str]]:
    """Disable the bot accounts for *handles*.

    Disable rather than delete: the account keeps its history, so the
    channels it posted in stay readable, and the seat can be brought back
    with a re-enable.  Its access tokens are revoked, so a decommissioned
    seat that is later restored needs a fresh token — the CLI says so.
    """
    outcomes: list[tuple[str, str]] = []
    bots = {str(b.get("username") or ""): b for b in await client.list_bots()}
    for handle in handles:
        username = f"{username_prefix}{handle}"
        bot = bots.get(username)
        if bot is None:
            outcomes.append((handle, "absent"))
            continue
        user_id = str(bot.get("user_id") or "")
        try:
            for token in await client.list_user_access_tokens(user_id):
                token_id = str(token.get("id") or "")
                if token_id:
                    await client.revoke_user_access_token(token_id)
            await client.disable_bot(user_id)
            outcomes.append((handle, "disabled"))
        except MattermostError as exc:
            outcomes.append((handle, f"error: {exc}"))
    return outcomes


__all__ = [
    "BOT_DESCRIPTION",
    "TOKEN_DESCRIPTION",
    "MattermostProvisionAborted",
    "ProvisionReport",
    "SeatResult",
    "decommission",
    "provision",
    "seat_token_vars",
]
