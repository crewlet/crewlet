"""``crewlet mattermost doctor`` — prove a Mattermost install actually works.

Mirrors ``crewlet llm doctor``'s job for the other backend an operator
has to stand up themselves: check the things that are *silently* wrong,
not the things that raise.

Two of those dominate, and neither shows up as an error anywhere:

**The Site URL.** ``ServiceSettings.SiteURL`` is not cosmetic. Mattermost
accepts a websocket upgrade only from a browser whose ``Origin`` matches
it *exactly* — host and scheme — so a server that answers every REST call
correctly on ``http://10.0.0.7:8065`` will refuse the web app's live
connection when its Site URL still says ``http://localhost:8065``. The
web app then falls back to fetching on navigation, which reads as "I have
to refresh to see replies" rather than as an outage. Plugins compound it:
they build their URLs from the same value, so their requests go to a host
the reader's own machine has to resolve. The engine is *exempt* — a
non-browser client sends no ``Origin`` header and Mattermost lets those
through — which is exactly why this can run for weeks: the agents answer,
and only the humans' clients are blind.

**A seat's websocket.** A bot token can be syntactically fine, belong to
a real account, and still not open a socket — the bot was disabled, the
token was revoked, personal access tokens were turned off server-wide.
The engine's *only* inbound path is that socket, so the failure mode is
an agent that never hears anything while every other check passes. So
this command opens the real handshake and authenticates, the way
``llm doctor`` insists on a real completion.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field

from crewlet._logging import get_logger
from crewlet.mattermost.client import (
    MattermostClient,
    MattermostError,
    normalize_base_url,
    site_urls_match,
    websocket_url,
)

logger = get_logger("mattermost.doctor")

#: How long the browser-shaped upgrade probe and each seat's
#: authentication round trip may take.  Both are a single round trip on a
#: server that has already answered ``/system/ping``, so this only has to
#: survive a loaded server; it is deliberately short enough that a doctor
#: run against an unreachable host fails in seconds rather than minutes.
PROBE_TIMEOUT_SECONDS = 15.0


@dataclass
class SeatCheck:
    """One agent seat's Mattermost health."""

    handle: str
    username: str = ""
    user_id: str = ""
    token_ok: bool = False
    is_bot: bool = False
    disabled: bool = False
    websocket_ok: bool = False
    channels: list[str] = field(default_factory=list)
    problems: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.problems


@dataclass
class DoctorReport:
    """Everything ``crewlet mattermost doctor`` learned about one install."""

    url: str
    team: str
    reachable: bool = False
    server_version: str = ""
    site_url: str = ""
    site_url_ok: bool = False
    browser_websocket_ok: bool = False
    browser_websocket_detail: str = ""
    team_ok: bool = False
    seats: list[SeatCheck] = field(default_factory=list)
    problems: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.problems and all(seat.ok for seat in self.seats)


async def _probe_browser_websocket(base_url: str) -> tuple[bool, str]:
    """Open the websocket the way a BROWSER would, and report the outcome.

    The ``origin`` argument is the whole point: it is the one header a
    browser always sends and a server-side client never does, and it is
    the only input to Mattermost's websocket origin check.  Probing
    without it would prove the endpoint is reachable while telling us
    nothing about whether a human's web app can use it — which is
    precisely the blind spot this command exists to remove.
    """
    import websockets

    url = websocket_url(base_url)
    origin = normalize_base_url(base_url)
    try:
        async with asyncio.timeout(PROBE_TIMEOUT_SECONDS):
            async with websockets.connect(url, origin=origin, open_timeout=None):
                return True, f"upgraded with Origin: {origin}"
    except TimeoutError:
        return False, f"timed out after {PROBE_TIMEOUT_SECONDS:.0f}s"
    except Exception as exc:  # websockets raises a family of these
        return False, f"{type(exc).__name__}: {exc}"


async def _probe_seat_websocket(base_url: str, token: str, handle: str) -> str:
    """Authenticate one seat's websocket; ``""`` when it works.

    No ``origin``: this is the connection the ENGINE makes, and it must
    be checked as the engine makes it.  Sending one here would fold the
    Site URL problem into every seat's result and hide whichever of the
    two is actually broken.
    """
    import websockets

    from crewlet.mattermost.events import (
        MattermostAuthError,
        await_authentication,
        send_authentication_challenge,
    )

    try:
        async with asyncio.timeout(PROBE_TIMEOUT_SECONDS):
            async with websockets.connect(
                websocket_url(base_url), open_timeout=None
            ) as socket:
                await send_authentication_challenge(socket, token)
                await await_authentication(
                    socket, handle=handle, timeout=PROBE_TIMEOUT_SECONDS
                )
        return ""
    except MattermostAuthError as exc:
        return str(exc)
    except TimeoutError:
        return f"no authentication reply within {PROBE_TIMEOUT_SECONDS:.0f}s"
    except Exception as exc:
        return f"{type(exc).__name__}: {exc}"


async def _check_seat(
    base_url: str,
    team_id: str,
    handle: str,
    token: str,
) -> SeatCheck:
    """Token → identity → membership → a real authenticated websocket."""
    seat = SeatCheck(handle=handle)
    if not token:
        seat.problems.append(
            "no bot token resolved — the ${VAR} in "
            "role.integrations.mattermost.bot_token is unset; run "
            "`crewlet mattermost provision`"
        )
        return seat

    client = MattermostClient(base_url, token)
    try:
        try:
            me = await client.me()
        except MattermostError as exc:
            seat.problems.append(
                f"the bot token is not usable ({exc}) — it may have been "
                "revoked, or its bot account disabled; re-run "
                "`crewlet mattermost provision`"
            )
            return seat
        seat.token_ok = True
        seat.user_id = str((me or {}).get("id") or "")
        seat.username = str((me or {}).get("username") or "")
        seat.is_bot = bool((me or {}).get("is_bot"))
        seat.disabled = bool((me or {}).get("delete_at"))
        if seat.disabled:
            seat.problems.append(
                "the account is disabled — a later provision run re-enables it"
            )

        if team_id and seat.user_id:
            try:
                channels = await client.list_channels_for_user(seat.user_id, team_id)
            except MattermostError as exc:
                seat.problems.append(f"cannot list its channels: {exc}")
            else:
                seat.channels = sorted(
                    str(c.get("name") or "") for c in channels if c.get("name")
                )
                if not seat.channels:
                    seat.problems.append(
                        "member of no channel in this team — a bot only "
                        "receives messages from channels it has joined, so "
                        "it would never be woken by anything but a DM"
                    )

        failure = await _probe_seat_websocket(base_url, token, handle)
        seat.websocket_ok = not failure
        if failure:
            seat.problems.append(f"websocket authentication failed: {failure}")
    finally:
        await client.close()
    return seat


async def run_doctor(
    *,
    url: str,
    team: str,
    seat_tokens: dict[str, str],
) -> DoctorReport:
    """Check one Mattermost install and every seat configured against it."""
    base_url = normalize_base_url(url)
    report = DoctorReport(url=base_url, team=team)

    # Unauthenticated on purpose: /system/ping and /config/client are the
    # two reads a browser makes before anyone signs in, so a bad operator
    # credential cannot turn a healthy server into a red report.
    anon = MattermostClient(base_url, "")
    try:
        try:
            ping = await anon.ping()
        except MattermostError as exc:
            report.problems.append(
                f"cannot reach {base_url}: {exc} — check the URL, that the "
                "server is running, and that this host can route to it"
            )
            return report
        report.reachable = True
        report.server_version = str((ping or {}).get("version") or "")

        config = await anon.client_config()
        report.site_url = normalize_base_url(str(config.get("SiteURL") or ""))
        report.site_url_ok = bool(report.site_url) and site_urls_match(
            base_url, report.site_url
        )
        if not report.site_url:
            report.problems.append(
                "the server did not report a SiteURL — every browser-facing "
                "URL it builds will be wrong"
            )
        elif not report.site_url_ok:
            report.problems.append(
                f"the server's SiteURL is {report.site_url}, not {base_url}. "
                "Mattermost accepts a websocket upgrade only from a browser "
                "whose Origin matches SiteURL exactly (host and scheme), so "
                "everyone browsing at another address loses live updates and "
                "has to refresh to see new messages, and plugins request "
                f"{report.site_url} from the reader's own machine. Set "
                "MM_SERVICESETTINGS_SITEURL (compose: MATTERMOST_PUBLIC_URL) "
                "to the address people type, and recreate the container — "
                "environment-managed settings cannot be changed from the "
                "System Console or the API"
            )

        ok, detail = await _probe_browser_websocket(base_url)
        report.browser_websocket_ok = ok
        report.browser_websocket_detail = detail
        if not ok:
            report.problems.append(
                f"a browser-shaped websocket to {base_url} did not open "
                f"({detail}) — the web app's live feed is dead for everyone "
                "until this works"
            )
    finally:
        await anon.close()

    # The team lookup needs a credential; borrow the first seat's, since
    # every seat is a member of it and no operator token is required for
    # a command whose whole point is to be safe to run.
    team_id = ""
    for handle, token in seat_tokens.items():
        if not token:
            continue
        client = MattermostClient(base_url, token)
        try:
            found = await client.get_team_by_name(team)
            team_id = str((found or {}).get("id") or "")
        except MattermostError as exc:
            logger.debug("team_lookup_failed", handle=handle, error=str(exc))
        finally:
            await client.close()
        if team_id:
            break
    report.team_ok = bool(team_id)
    if not team_id and seat_tokens:
        report.problems.append(
            f"team {team!r} was not found, or no seat can see it — channels "
            "are team-scoped, so nothing can be delivered"
        )

    for handle in sorted(seat_tokens):
        report.seats.append(
            await _check_seat(base_url, team_id, handle, seat_tokens[handle])
        )
    if not seat_tokens:
        report.problems.append(
            "no Mattermost-enabled agent seats in this config — add "
            "role.integrations.mattermost.bot_token to at least one role"
        )
    return report


def format_report(report: DoctorReport) -> str:
    """Render a :class:`DoctorReport` for the terminal."""
    lines = [
        f"url           : {report.url}",
        f"reachable     : {'yes' if report.reachable else 'NO'}"
        + (f" (Mattermost {report.server_version})" if report.server_version else ""),
        f"site url      : {report.site_url or '(unreported)'}"
        + ("" if report.site_url_ok else "   << does not match url"),
        f"browser ws    : {'ok' if report.browser_websocket_ok else 'FAILED'}"
        + (
            f" — {report.browser_websocket_detail}"
            if report.browser_websocket_detail
            else ""
        ),
        f"team          : {report.team}{'' if report.team_ok else '   << not found'}",
    ]
    if report.seats:
        lines.append("")
        lines.append(f"{'SEAT':<18} {'USERNAME':<22} {'TOKEN':<7} {'WS':<7} CHANNELS")
        for seat in report.seats:
            lines.append(
                f"{seat.handle:<18} {seat.username or '-':<22} "
                f"{'ok' if seat.token_ok else 'FAILED':<7} "
                f"{'ok' if seat.websocket_ok else 'FAILED':<7} "
                f"{','.join(seat.channels) or '-'}"
            )
            for problem in seat.problems:
                lines.append(f"    - {problem}")
    if report.problems:
        lines.append("")
        lines.append("problems:")
        lines.extend(f"  - {p}" for p in report.problems)
    else:
        lines.append("")
        lines.append("problems      : none")
    return "\n".join(lines)


__all__ = [
    "PROBE_TIMEOUT_SECONDS",
    "DoctorReport",
    "SeatCheck",
    "format_report",
    "run_doctor",
]
