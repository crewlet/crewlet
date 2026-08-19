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
from crewlet.mattermost.client import (
    MattermostClient,
    MattermostError,
    site_url_origin_matches,
)
from crewlet.provisioning import TokenSink, referenced_env_vars, sole_env_var

logger = get_logger("mattermost.provision")

#: Description stamped on every token this provisioner mints, so an
#: operator reading a bot's token list in the Mattermost console can tell
#: engine-managed credentials from ones a human created by hand.
TOKEN_DESCRIPTION = "crewlet-engine"

#: Description stamped on bot accounts the provisioner creates, for the
#: same reason.
BOT_DESCRIPTION = "Crewlet agent seat"

#: Keys in ``role.mcp_env.mattermost`` that name the seat's CREDENTIAL.
#: Everything else there (a URL, a team) is configuration, and minting a
#: token into it would replace an operator's value with a secret.
#: ``MATTERMOST_TOKEN`` is what ``mcp-server-mattermost`` reads and what
#: the docs use; the others are the names its forks answer to.
_TOKEN_ENV_KEYS = frozenset(
    {
        "MATTERMOST_TOKEN",
        "MATTERMOST_API_TOKEN",
        "MATTERMOST_ACCESS_TOKEN",
        "MATTERMOST_BOT_TOKEN",
        "MATTERMOST_PERSONAL_ACCESS_TOKEN",
    }
)


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

    Only the **credential** keys are scanned, not every value in
    ``mcp_env.mattermost``.  That block legitimately carries other
    references — a ``MATTERMOST_URL``, a team name — and minting a bot's
    personal access token into one of those would overwrite an operator's
    URL with a secret, in the env file, silently.
    """
    scan: dict[str, str] = {}
    identity = dict(getattr(role, "mattermost", None) or {})
    if identity.get("bot_token"):
        scan["__identity_bot_token__"] = identity["bot_token"]
    mcp_env = dict((getattr(role, "mcp_env", None) or {}).get("mattermost") or {})
    for key, value in mcp_env.items():
        if key.upper() in _TOKEN_ENV_KEYS:
            scan[key] = value
    return referenced_env_vars(scan)


def seat_username(role: Any, username_prefix: str = "") -> str:
    """The Mattermost account name for *role*'s bot.

    ONE derivation, shared by provisioning and decommissioning — they
    disagreed before, so a seat with an explicit ``username`` could be
    created and then never found again by ``--decommission``, which
    reported it ``absent`` while the bot stayed enabled with a live
    token.

    ``${VAR}`` references are resolved here, the same way the engine
    resolves them: only ``bot_token`` stays raw, because its reference
    *is* the minting contract.  An unresolved one would otherwise create
    a bot literally named ``${MM_USERNAME_SWE}``.
    """
    from crewlet.config import resolve_env_scalar

    identity = dict(getattr(role, "mattermost", None) or {})
    explicit = resolve_env_scalar(str(identity.get("username") or "")).strip()
    if explicit:
        return explicit
    return f"{username_prefix}{role.get_handle()}"


def seat_channel(role: Any) -> str:
    """The seat's configured default channel name, ``${VAR}`` resolved."""
    from crewlet.config import resolve_env_scalar

    identity = dict(getattr(role, "mattermost", None) or {})
    return resolve_env_scalar(str(identity.get("channel") or "")).strip()


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

    await _check_required_settings(client)
    await _check_site_url(client, report)

    # Bots are excluded from the active-user cap, so an agent fleet never
    # consumes it.  Report the human headroom anyway: an operator whose
    # workspace is near the wall wants to know before they invite people,
    # not after the server refuses.
    try:
        limits = await client.server_limits()
    except MattermostError as exc:
        # Informational only. A reconcile that refused to run because a
        # headroom NOTE could not be fetched would be refusing over
        # nothing.
        logger.debug("mattermost_limits_read_failed", error=str(exc))
        limits = {}
    max_users = limits.get("maxUsersLimit") or limits.get("max_users_limit")
    active = limits.get("activeUserCount") or limits.get("active_user_count")
    if max_users:
        report.notes.append(
            f"server user limit: {active or '?'}/{max_users} active human "
            "users. Bot accounts are excluded from this count, so agent "
            "seats do not consume it."
        )

    return team_id


async def _check_required_settings(client: MattermostClient) -> None:
    """Refuse to start when the server cannot do what the run needs.

    Both settings default to **false** on a fresh install, and both fail
    late: with personal access tokens disabled, every seat creates its
    bot, joins its channels, and only then fails at the mint — the
    half-provisioned fleet this preflight exists to prevent, with an
    error naming an API path instead of the toggle to flip.
    """
    try:
        config = await client.server_config()
    except MattermostError as exc:
        # A server that will not hand over its config is not necessarily
        # broken (a proxy, a restricted admin role); the run can still
        # proceed and fail per seat with a real error.
        logger.debug("mattermost_config_read_failed", error=str(exc))
        return
    service = dict(config.get("ServiceSettings") or {})
    required = [
        (
            "EnableBotAccountCreation",
            "creating the bot accounts",
            "System Console → Integrations → Bot Accounts",
        ),
        (
            "EnableUserAccessTokens",
            "minting their access tokens",
            "System Console → Integrations → Integration Management → "
            "Enable Personal Access Tokens",
        ),
    ]
    missing = [
        f"ServiceSettings.{name} is off, which {why} needs — enable it under {where}"
        for name, why, where in required
        if service.get(name) is False
    ]
    if missing:
        raise MattermostProvisionAborted("; ".join(missing))


async def _check_site_url(client: MattermostClient, report: ProvisionReport) -> None:
    """Report — or refuse — a Site URL that will break every browser.

    This command is the only one in the product that runs against the
    server holding a system-admin token, and it runs *first*, so it is
    where a wrong ``ServiceSettings.SiteURL`` should be caught. Mattermost
    accepts a websocket upgrade only from a browser whose ``Origin``
    matches SiteURL exactly, and the engine — which sends no ``Origin`` —
    is exempt: provisioning succeeds, agents reply, and only the humans'
    clients are blind, with nothing anywhere naming the cause.

    A loopback SiteURL on a server reached at a real address is wrong for
    every browser except one sitting on that host, so that combination
    aborts.  Any other mismatch is a note: an operator may legitimately
    reach the server by a second name.
    """
    try:
        reported = await client.site_url()
    except MattermostError as exc:
        logger.debug("mattermost_site_url_read_failed", error=str(exc))
        return
    configured = client.base_url
    if not reported or site_url_origin_matches(configured, reported):
        return

    from urllib.parse import urlparse

    reported_host = (urlparse(reported).hostname or "").lower()
    configured_host = (urlparse(configured).hostname or "").lower()
    loopback = {"localhost", "127.0.0.1", "0.0.0.0", "::1"}
    if reported_host in loopback and configured_host not in loopback:
        raise MattermostProvisionAborted(
            f"the server reports SiteURL {reported}, but it is reached here "
            f"at {configured}. Mattermost accepts a websocket upgrade only "
            "from a browser whose Origin matches SiteURL exactly, so with a "
            "loopback SiteURL every browser loses live updates and has to "
            "refresh to see new messages — and its plugins request "
            f"{reported} from the reader's own machine. Set "
            "MM_SERVICESETTINGS_SITEURL (compose: MATTERMOST_PUBLIC_URL) to "
            "the address people type and recreate the container; an "
            "environment-managed setting cannot be changed from the System "
            "Console or the API. Provisioning would otherwise succeed and "
            "leave you to find this in a browser console."
        )
    report.notes.append(
        f"the server's SiteURL is {reported}, not {configured}. Browsers on "
        "any address other than SiteURL lose live updates; verify with "
        "`crewlet mattermost doctor`."
    )


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
    channel_ids: dict[str, str] | None = None,
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
        # Mattermost answers an add for an EXISTING member with success,
        # so a 4xx here is a real failure — not-in-the-team, a
        # group-constrained team, a guest restriction. Verify instead of
        # guessing: reading it as "already joined" reports a seat into a
        # team it never joined.
        if await client.get_team_member(team_id, user_id):
            result.team = "exists"
        else:
            raise MattermostError(
                f"could not join team: {exc}",
                method=exc.method,
                path=exc.path,
                status=exc.status,
            ) from exc

    failures: list[str] = []
    resolved = channel_ids if channel_ids is not None else {}
    for name in channel_names:
        if name in resolved:
            channel_id = resolved[name]
        else:
            channel = await client.get_channel_by_name(team_id, name)
            channel_id = str((channel or {}).get("id") or "")
            resolved[name] = channel_id
        if not channel_id:
            # Not a note: a bot receives nothing from a channel it is not
            # in, so a configured channel that does not exist is a seat
            # that will be deaf where the operator expects it to listen.
            failures.append(f"channel {name!r} does not exist in this team")
            continue
        try:
            await client.add_channel_member(channel_id, user_id)
            result.channels.append(name)
        except MattermostError as exc:
            if await client.get_channel_member(channel_id, user_id):
                result.channels.append(name)
            else:
                failures.append(f"channel {name!r}: {exc}")
    if failures:
        raise MattermostError("; ".join(failures))


async def _ensure_token(
    client: MattermostClient,
    *,
    user_id: str,
    token_vars: list[str],
    sink: TokenSink,
    result: SeatResult,
) -> None:
    """Mint the seat's access token into its ``${VAR}``s.

    "Already provisioned" is a claim about two places, and the sink is
    only one of them.  A ``${VAR}`` that still carries a value whose
    token has since been REVOKED — by ``--decommission``, by an admin in
    the System Console, by a restore from an older env file — used to
    read as provisioned, so the documented recovery ("a later provision
    run re-enables it") re-enabled a bot and left it holding a dead
    credential: silent, and fixable only by hand-editing the env file.
    So the server is consulted too, and a seat with no live token of ours
    is re-minted regardless of what the sink says.

    Mattermost returns a token's value exactly once, which is why a
    seat that HAS both is never re-minted: that would strand the live
    credential rather than replace it.

    When a mint does happen, it lands in **every** ``${VAR}`` the seat
    references, not only the empty ones.  A fresh token supersedes
    whatever any of them held, and writing a subset leaves the seat split
    across two credentials — one consumer working, the other not, with
    nothing in the report to say which.  The superseded token is then
    revoked, so a seat is never the reason an unreferenced credential
    stays live on a bot account.
    """
    missing = [var for var in token_vars if not sink.existing(var)]
    stale = await _our_token_ids(client, user_id)
    if not missing and stale is None:
        # The list could not be read, and unknowable is not the same as
        # absent: minting here would strand the credential the config
        # already carries.
        result.token_action = "exists"
        return
    if not missing and stale:
        result.token_action = "exists"
        return
    if not missing:
        result.notes.append(
            "the recorded token is no longer active on the server — minting a fresh one"
        )

    created = await client.create_user_access_token(user_id, TOKEN_DESCRIPTION)
    token = str((created or {}).get("token") or "")
    token_id = str((created or {}).get("id") or "")
    if not token:
        raise MattermostError("token creation returned no token value")

    # Write-through before anything else can fail: the value is
    # unretrievable from here on, so the window between "minted" and
    # "persisted" is a window in which a crash orphans a live credential.
    recorded: list[str] = []
    try:
        for var in token_vars:
            await sink.record(var, token)
            recorded.append(var)
    except BaseException:
        await _roll_back_mint(
            client, sink, user_id=user_id, token_id=token_id, recorded=recorded
        )
        raise
    result.token_action = "minted"

    # Every ${VAR} now names the token just minted, so anything of ours
    # that was live before is referenced by nothing: an unknown,
    # non-expiring credential on a bot account, which is precisely what
    # the rest of this function refuses to leave behind.
    superseded = 0
    for old_id in stale or []:
        if old_id == token_id:
            continue
        try:
            await client.revoke_user_access_token(old_id)
        except MattermostError as exc:
            result.notes.append(
                f"could not revoke the superseded token {old_id}: {exc} — "
                "revoke it in Profile > Security > Personal Access Tokens"
            )
            logger.warning(
                "mattermost_superseded_token_revoke_failed",
                user=user_id,
                token_id=old_id,
                error=str(exc),
            )
        else:
            superseded += 1
    if superseded:
        result.notes.append(f"revoked {superseded} superseded token(s)")


async def _roll_back_mint(
    client: MattermostClient,
    sink: TokenSink,
    *,
    user_id: str,
    token_id: str,
    recorded: list[str],
) -> None:
    """Undo a mint whose value could not be persisted everywhere.

    Both halves are required.  Revoking alone leaves every ``${VAR}``
    written before the failure holding a token that no longer
    authenticates — indistinguishable, downstream, from a good one, so
    the seat's socket simply never opens.  Discarding alone leaves a
    live, unknown, non-expiring credential on the account that no later
    run can name.

    The revoke goes first, and each step runs whatever the last one did:
    a crash between them should leave the recoverable state (dead values
    in the env file, which the next run re-mints over) rather than the
    unrecoverable one (a live orphan nothing will ever revoke).
    """
    if token_id:
        try:
            await client.revoke_user_access_token(token_id)
            logger.warning("mattermost_unpersisted_token_revoked", user=user_id)
        except Exception:
            logger.exception("mattermost_token_revoke_failed", user=user_id)
    for var in recorded:
        try:
            await sink.discard(var)
        except Exception:
            logger.exception("mattermost_token_discard_failed", user=user_id, var=var)


async def _our_token_ids(client: MattermostClient, user_id: str) -> list[str] | None:
    """Ids of the bot's active tokens that this tool minted.

    ``None`` when the list could not be read — which is *not* the same
    as "there are none": minting on a read failure would strand the
    credential the config already carries, and revoking on one would tear
    down a seat that is working.
    """
    try:
        tokens = await client.list_user_access_tokens(user_id)
    except MattermostError as exc:
        logger.debug("mattermost_token_list_failed", user=user_id, error=str(exc))
        return None
    return [
        str(t.get("id") or "")
        for t in tokens
        if str(t.get("description") or "") == TOKEN_DESCRIPTION
        and t.get("is_active", True)
        and t.get("id")
    ]


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

    try:
        await _reconcile_seats(
            client,
            org,
            report=report,
            sink=sink,
            existing_bots=existing_bots,
            username_prefix=username_prefix,
            display_name_suffix=display_name_suffix,
            default_channels=default_channels,
            handles=handles,
        )
    except BaseException:
        # Flush what was minted before re-raising — the same
        # flush-on-abort wrapper the Plane and GitLab provisioners use.
        # A Ctrl-C after the first mint would otherwise lose credentials
        # that are already live on their accounts and unretrievable.
        try:
            await sink.flush()
        except Exception:
            logger.exception("mattermost_sink_flush_failed")
        raise
    await sink.flush()
    return report


async def _reconcile_seats(
    client: MattermostClient,
    org: Any,
    *,
    report: ProvisionReport,
    sink: TokenSink,
    existing_bots: dict[str, dict[str, Any]],
    username_prefix: str,
    display_name_suffix: str,
    default_channels: list[str] | None,
    handles: set[str] | None,
) -> None:
    """The per-seat loop, split out so aborting it still flushes."""
    if handles is not None:
        known = {r.get_handle() for r in org.all_roles()}
        unknown = sorted(handles - known)
        if unknown:
            # Silently provisioning nothing looks identical to a clean
            # run, and the operator's typo survives into the next one.
            raise MattermostProvisionAborted(
                "--handles names seats that are not in this config: "
                + ", ".join(unknown)
            )

    # Channel names resolve to the same ids for every seat, so resolve
    # each once: a fleet of twenty seats otherwise re-reads the same
    # channel list twenty times, and a channel that does not exist is
    # rediscovered per seat instead of reported once.
    channel_ids: dict[str, str] = {}

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

        username = seat_username(role, username_prefix)
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
            own_channel = seat_channel(role)
            if own_channel and own_channel not in channel_names:
                channel_names.append(own_channel)
            await _ensure_membership(
                client,
                team_id=report.team_id,
                user_id=user_id,
                channel_names=channel_names,
                result=result,
                channel_ids=channel_ids,
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


async def decommission(
    client: MattermostClient,
    handles: list[str],
    *,
    org: Any = None,
    username_prefix: str = "",
    dry_run: bool = False,
) -> list[tuple[str, str]]:
    """Disable the bot accounts for *handles*.

    Disable rather than delete: the account keeps its history, so the
    channels it posted in stay readable, and the seat can be brought back
    with a re-enable.  Its access tokens are revoked, so a decommissioned
    seat that is later restored is minted a fresh one.

    The account is disabled **first**.  Deactivating it is what actually
    stops the seat acting; revoking tokens first meant one already-dead
    token could abort the handle with the bot still enabled and its other
    tokens live, reported as an error the caller shrugged at.  Each token
    is then revoked on its own, so one failure costs one token rather
    than the whole seat, and the outcome names what is left.

    With *org*, the account name comes from :func:`seat_username`, the
    same derivation ``provision`` used to create it — without it, a seat
    with an explicit ``username`` could never be decommissioned at all
    and was reported ``absent`` while its bot stayed enabled.
    """
    outcomes: list[tuple[str, str]] = []
    bots = {str(b.get("username") or ""): b for b in await client.list_bots()}
    by_handle = {}
    if org is not None:
        by_handle = {r.get_handle(): r for r in org.all_roles()}

    for handle in handles:
        role = by_handle.get(handle)
        if org is not None and role is None:
            outcomes.append((handle, "unknown handle — not in this config"))
            continue
        username = (
            seat_username(role, username_prefix)
            if role is not None
            else f"{username_prefix}{handle}"
        )
        bot = bots.get(username)
        if bot is None:
            outcomes.append((handle, f"absent (no bot named {username!r})"))
            continue
        user_id = str(bot.get("user_id") or "")

        if dry_run:
            try:
                live = len(await client.list_user_access_tokens(user_id))
            except MattermostError as exc:
                outcomes.append((handle, f"error: {exc}"))
                continue
            state = "already disabled" if bot.get("delete_at") else "enabled"
            outcomes.append(
                (handle, f"would disable {username} ({state}, {live} token(s))")
            )
            continue

        problems: list[str] = []
        try:
            await client.disable_bot(user_id)
        except MattermostError as exc:
            problems.append(f"disable failed: {exc}")
        try:
            tokens = await client.list_user_access_tokens(user_id)
        except MattermostError as exc:
            problems.append(f"could not list tokens: {exc}")
            tokens = []
        remaining = 0
        for token in tokens:
            token_id = str(token.get("id") or "")
            if not token_id:
                continue
            try:
                await client.revoke_user_access_token(token_id)
            except MattermostError as exc:
                remaining += 1
                logger.warning(
                    "mattermost_token_revoke_failed", handle=handle, error=str(exc)
                )
        if remaining:
            problems.append(f"{remaining} token(s) still active")
        outcomes.append(
            (handle, "disabled" if not problems else "error: " + "; ".join(problems))
        )
    return outcomes


__all__ = [
    "BOT_DESCRIPTION",
    "TOKEN_DESCRIPTION",
    "MattermostProvisionAborted",
    "ProvisionReport",
    "SeatResult",
    "decommission",
    "provision",
    "seat_channel",
    "seat_token_vars",
    "seat_username",
]
