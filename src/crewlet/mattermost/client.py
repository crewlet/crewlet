"""Async REST client for the subset of Mattermost's API Crewlet uses.

Thin wrapper over ``httpx`` with bearer-token auth.  Covers three
consumers:

- the **transport** — posting messages, resolving a bot's own user id,
  reading the server's typing-throttle setting, raising the typing
  indicator;
- the **websocket fleet** — the per-channel ``since=`` backfill that
  covers a reconnect, because Mattermost's websocket replays nothing;
- the **provisioner** — bots, tokens, teams and channel membership.

No SDK dependency.  See ``docs/integrations/mattermost.md``.
"""

from __future__ import annotations

from datetime import UTC
from email.utils import parsedate_to_datetime
from typing import Any

import httpx

from crewlet._logging import get_logger

logger = get_logger("mattermost.client")

#: Mattermost paginates with ``page`` / ``per_page``; 200 is the
#: server's documented ceiling for the endpoints used here.
_MAX_PER_PAGE = 200

#: Hard ceiling on pagination walks.  50 pages × 200 rows = 10 000, far
#: above any real team's channel or member list.  The backfill walk runs
#: on the reconnect path, where an unbounded loop against a
#: malfunctioning server would stall the fleet, so a finite cap is the
#: backstop behind the empty-page check.
_MAX_LIST_PAGES = 50

#: Channel types, as Mattermost's single-letter ``type`` field.
CHANNEL_OPEN = "O"
CHANNEL_PRIVATE = "P"
CHANNEL_DIRECT = "D"
CHANNEL_GROUP = "G"

#: Channel types that are a direct conversation with the bot.  Mirrors
#: the ``DIRECT_CHANNEL_TYPES`` half of
#: :mod:`crewlet.notifications.typing_status` for this backend.
DIRECT_CHANNEL_TYPES = frozenset({CHANNEL_DIRECT, CHANNEL_GROUP})

#: Fallback for ``TimeBetweenUserTypingUpdatesMilliseconds`` when the
#: server's client config cannot be read.  5 000 ms is Mattermost's own
#: default; the transport derives its heartbeat from whatever the server
#: actually reports (see ``MattermostTransport.start``), so this only
#: covers a server that refuses the config read.
DEFAULT_TYPING_THROTTLE_MS = 5000


#: The websocket endpoint every Mattermost server exposes, appended to
#: the instance URL with the scheme swapped for its ws(s) counterpart.
WEBSOCKET_PATH = "/api/v4/websocket"


def normalize_base_url(url: str) -> str:
    """The instance URL in the one shape every consumer compares against.

    Trailing slashes are stripped so ``https://chat.example/`` and
    ``https://chat.example`` are the same address — which matters because
    this value is both string-compared against the server's own
    ``SiteURL`` and concatenated with API paths.
    """
    return str(url or "").strip().rstrip("/")


def websocket_url(base_url: str) -> str:
    """The ``/api/v4/websocket`` endpoint for *base_url*.

    ONE derivation, shared by the config model, the transport that builds
    the fleet and ``crewlet mattermost doctor``.  It was written out three
    times before, and a divergence here is invisible until an
    ``https://`` instance silently gets a plaintext ``ws://`` socket.
    """
    base = normalize_base_url(base_url)
    if base.startswith("https://"):
        base = "wss://" + base[len("https://") :]
    elif base.startswith("http://"):
        base = "ws://" + base[len("http://") :]
    return f"{base}{WEBSOCKET_PATH}"


def typing_throttle_from(client_config: dict[str, Any]) -> int:
    """``TimeBetweenUserTypingUpdatesMilliseconds`` out of a client config.

    Split from the read so a caller that already holds the client config
    — the transport, which reads it once at start for both this and the
    Site URL check — does not fetch it twice.
    """
    try:
        value = int(client_config.get("TimeBetweenUserTypingUpdatesMilliseconds"))
    except (TypeError, ValueError):
        return DEFAULT_TYPING_THROTTLE_MS
    return value if value > 0 else DEFAULT_TYPING_THROTTLE_MS


def site_urls_match(configured: str, reported: str) -> bool:
    """Whether the server's own ``SiteURL`` names the configured address.

    Mattermost hands ``SiteURL`` to every browser and every plugin, which
    build absolute URLs from it; a value that does not match the address
    people actually reach the server on produces requests to a host the
    reader's machine cannot resolve.  Compared after normalisation
    because the server trims its own trailing slash and an operator's
    config may not.
    """
    return normalize_base_url(configured) == normalize_base_url(reported)


class MattermostError(Exception):
    """A Mattermost API call failed.

    Carries the method, path and status so a failure is actionable
    without turning on request logging.
    """

    def __init__(
        self,
        message: str,
        *,
        method: str = "",
        path: str = "",
        status: int = 0,
    ) -> None:
        self.method = method
        self.path = path
        self.status = status
        prefix = f"{method} {path} → {status}: " if method else ""
        super().__init__(f"{prefix}{message}")


class MattermostClient:
    """Minimal async Mattermost REST client.

    ``base_url`` is the instance URL (e.g. ``https://chat.example``, NOT
    the ``/api/v4`` base).  Auth is a bearer token — a bot's personal
    access token for the transport and fleet, an admin's for the
    provisioner; the API does not distinguish them, so one client serves
    both.  An **empty** token sends no ``Authorization`` header at all,
    rather than an empty bearer a server would reject: the endpoints a
    health check reads (``/system/ping``, ``/config/client``) need no
    credential, and offering a bad one turns a working read into a 401.
    The underlying ``httpx.AsyncClient`` is created lazily on first
    request and released via :meth:`close`.
    """

    def __init__(
        self,
        base_url: str,
        token: str,
        *,
        timeout: float = 15.0,
    ) -> None:
        self._base_url = normalize_base_url(base_url)
        self._token = token
        self._timeout = timeout
        self._client: httpx.AsyncClient | None = None

    @property
    def base_url(self) -> str:
        """The instance URL, without the ``/api/v4`` suffix."""
        return self._base_url

    def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            headers = {"Content-Type": "application/json"}
            if self._token:
                headers["Authorization"] = f"Bearer {self._token}"
            self._client = httpx.AsyncClient(
                base_url=f"{self._base_url}/api/v4",
                headers=headers,
                timeout=self._timeout,
            )
        return self._client

    async def close(self) -> None:
        """Release the underlying HTTP client."""
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    def _redact(self, text: str) -> str:
        """Strip the credential out of an error body.

        A proxy echoing request headers into an error page is enough to
        leak a bot token into a log line otherwise.
        """
        if self._token:
            return text.replace(self._token, "[REDACTED]")
        return text

    async def _request(
        self,
        method: str,
        path: str,
        *,
        params: dict[str, Any] | None = None,
        json: Any | None = None,
    ) -> Any:
        resp = await self._http().request(method, path, params=params, json=json)
        if resp.status_code < 200 or resp.status_code >= 300:
            # Cap the UNTRUSTED raw body at the boundary it enters — a
            # proxy can echo megabytes of HTML into an error page.
            raise MattermostError(
                self._redact(resp.text)[:300],
                method=method,
                path=path,
                status=resp.status_code,
            )
        if not resp.content:
            return None
        try:
            return resp.json()
        except ValueError:
            return None

    # ----- liveness ----------------------------------------------------

    async def ping(self) -> dict[str, Any]:
        """``/system/ping`` — reachability, and the server's version.

        Needs no credential, which is what makes it the right first check
        in a health command: a bad operator token must not be able to
        report a healthy server as unreachable.
        """
        return await self._request("GET", "/system/ping") or {}

    # ----- identity ----------------------------------------------------

    async def me(self) -> dict[str, Any]:
        """The user this client's token authenticates as."""
        return await self._request("GET", "/users/me")

    async def get_user_by_username(self, username: str) -> dict[str, Any] | None:
        """Look up a user by username; ``None`` when absent."""
        try:
            return await self._request("GET", f"/users/username/{username}")
        except MattermostError as exc:
            if exc.status == 404:
                return None
            raise

    async def get_user(self, user_id: str) -> dict[str, Any] | None:
        """Look up a user by id; ``None`` when absent."""
        try:
            return await self._request("GET", f"/users/{user_id}")
        except MattermostError as exc:
            if exc.status == 404:
                return None
            raise

    # ----- server settings ---------------------------------------------

    async def server_time_ms(self) -> int:
        """The **server's** clock, in the epoch milliseconds posts carry.

        Every reconnect decision — how far back to replay, whether a gap
        is inside the backfill window — compares a post timestamp the
        SERVER stamped against "now".  Taking that "now" from the engine
        host's clock silently makes the window a function of the skew
        between two machines: an engine running a few minutes fast skips
        a backfill it should have replayed, and one running slow replays
        more than the window allows.

        Read from the ``Date`` response header, which every HTTP response
        carries and which is defined to be the server's own clock.  Its
        one-second resolution is immaterial against a 15-minute window.
        Returns ``0`` when unreadable, so callers can fall back to the
        local clock rather than fail.
        """
        try:
            resp = await self._http().get("/system/ping")
            date = resp.headers.get("Date", "")
        except httpx.HTTPError as exc:
            logger.debug("server_time_read_failed", error=str(exc))
            return 0
        if not date:
            return 0
        try:
            stamp = parsedate_to_datetime(date)
        except (TypeError, ValueError):
            return 0
        if stamp.tzinfo is None:
            stamp = stamp.replace(tzinfo=UTC)
        return int(stamp.timestamp() * 1000)

    async def client_config(self) -> dict[str, Any]:
        """The server's **client** configuration, as a browser sees it.

        ``/config/client?format=old`` needs no authentication and returns
        exactly the settings the web app is handed — including
        ``SiteURL``, which is what every browser and plugin builds its
        absolute URLs from.  Reading the same document the browser reads
        is the point: an admin-side config read can report a value the
        clients never see.

        Returns an empty dict when the server refuses the read, so a
        caller can degrade rather than fail.
        """
        try:
            cfg = await self._request("GET", "/config/client", params={"format": "old"})
        except MattermostError as exc:
            logger.debug("client_config_read_failed", error=str(exc))
            return {}
        return dict(cfg or {})

    async def site_url(self) -> str:
        """The server's own ``ServiceSettings.SiteURL``; ``""`` if unread."""
        return normalize_base_url(
            str((await self.client_config()).get("SiteURL") or "")
        )

    async def typing_throttle_ms(self) -> int:
        """The server's ``TimeBetweenUserTypingUpdatesMilliseconds``.

        The typing indicator's heartbeat is derived from this rather than
        hardcoded: it is the interval the server itself throttles typing
        updates to, so re-asserting faster is silently dropped and
        re-asserting much slower leaves a visible gap.  Reading it means
        an operator who tunes the server setting gets a heartbeat that
        follows, instead of a constant that silently disagrees.

        Falls back to :data:`DEFAULT_TYPING_THROTTLE_MS` when the config
        endpoint is unreadable — it is a cosmetic side-channel and must
        never fail a transport start.
        """
        return typing_throttle_from(await self.client_config())

    # ----- channels ----------------------------------------------------

    async def get_channel(self, channel_id: str) -> dict[str, Any] | None:
        """Fetch one channel; ``None`` when absent or not visible."""
        try:
            return await self._request("GET", f"/channels/{channel_id}")
        except MattermostError as exc:
            if exc.status in (403, 404):
                return None
            raise

    async def get_channel_by_name(
        self, team_id: str, name: str
    ) -> dict[str, Any] | None:
        """Fetch a channel by its URL name within a team."""
        try:
            return await self._request("GET", f"/teams/{team_id}/channels/name/{name}")
        except MattermostError as exc:
            if exc.status in (403, 404):
                return None
            raise

    async def list_channels_for_user(
        self, user_id: str, team_id: str
    ) -> list[dict[str, Any]]:
        """Every channel in *team_id* the user is a member of."""
        result = await self._request(
            "GET", f"/users/{user_id}/teams/{team_id}/channels"
        )
        return list(result or [])

    async def add_channel_member(self, channel_id: str, user_id: str) -> dict[str, Any]:
        """Add a user to a channel (idempotent server-side)."""
        return await self._request(
            "POST", f"/channels/{channel_id}/members", json={"user_id": user_id}
        )

    async def create_direct_channel(
        self, user_id_a: str, user_id_b: str
    ) -> dict[str, Any]:
        """Open (or fetch) the DM channel between two users."""
        return await self._request(
            "POST", "/channels/direct", json=[user_id_a, user_id_b]
        )

    # ----- teams -------------------------------------------------------

    async def get_team_by_name(self, name: str) -> dict[str, Any] | None:
        """Fetch a team by its URL name; ``None`` when absent."""
        try:
            return await self._request("GET", f"/teams/name/{name}")
        except MattermostError as exc:
            if exc.status in (403, 404):
                return None
            raise

    async def add_team_member(self, team_id: str, user_id: str) -> dict[str, Any]:
        """Add a user to a team (idempotent server-side)."""
        return await self._request(
            "POST",
            f"/teams/{team_id}/members",
            json={"team_id": team_id, "user_id": user_id},
        )

    # ----- posts -------------------------------------------------------

    async def create_post(
        self,
        channel_id: str,
        message: str,
        *,
        root_id: str = "",
    ) -> dict[str, Any]:
        """Post a message, optionally as a reply in *root_id*'s thread."""
        payload: dict[str, Any] = {"channel_id": channel_id, "message": message}
        if root_id:
            payload["root_id"] = root_id
        return await self._request("POST", "/posts", json=payload)

    async def posts_since(self, channel_id: str, since_ms: int) -> list[dict[str, Any]]:
        """Posts in a channel created or updated since *since_ms*.

        The reconnect-gap primitive.  Mattermost's websocket replays
        nothing after a disconnect, so the fleet re-reads each channel it
        was watching from the last post it saw.  Returned oldest-first,
        which is the order the fleet must replay them in.
        """
        data = await self._request(
            "GET", f"/channels/{channel_id}/posts", params={"since": since_ms}
        )
        order = list((data or {}).get("order") or [])
        posts = (data or {}).get("posts") or {}
        # ``order`` is newest-first; the caller replays chronologically.
        return [posts[pid] for pid in reversed(order) if pid in posts]

    # ----- typing indicator --------------------------------------------

    async def publish_typing(self, channel_id: str, *, parent_id: str = "") -> None:
        """Raise the composer typing indicator for this client's user.

        Thread-scoped when *parent_id* is set.  Fixed vocabulary: the
        wording is the client's, so nothing can be said with it beyond
        "this user is typing".
        """
        payload: dict[str, Any] = {"channel_id": channel_id}
        if parent_id:
            payload["parent_id"] = parent_id
        await self._request("POST", "/users/me/typing", json=payload)

    # ----- bots + tokens (provisioning) ---------------------------------

    async def list_bots(self) -> list[dict[str, Any]]:
        """Every bot account on the server, including deleted ones.

        ``include_deleted`` matters for reconcile: a disabled bot still
        owns its username, so creating over it fails with a conflict the
        caller could not otherwise explain.
        """
        bots: list[dict[str, Any]] = []
        for page in range(_MAX_LIST_PAGES):
            batch = await self._request(
                "GET",
                "/bots",
                params={
                    "page": page,
                    "per_page": _MAX_PER_PAGE,
                    "include_deleted": "true",
                },
            )
            batch = list(batch or [])
            bots.extend(batch)
            if len(batch) < _MAX_PER_PAGE:
                break
        else:
            logger.warning("bot_list_truncated", pages=_MAX_LIST_PAGES)
        return bots

    async def create_bot(
        self, username: str, display_name: str, description: str = ""
    ) -> dict[str, Any]:
        """Create a bot account."""
        return await self._request(
            "POST",
            "/bots",
            json={
                "username": username,
                "display_name": display_name,
                "description": description,
            },
        )

    async def patch_bot(self, bot_user_id: str, **fields: Any) -> dict[str, Any]:
        """Update a bot's mutable fields (display name, description)."""
        return await self._request("PUT", f"/bots/{bot_user_id}", json=fields)

    async def enable_bot(self, bot_user_id: str) -> dict[str, Any]:
        """Re-enable a previously disabled bot."""
        return await self._request("POST", f"/bots/{bot_user_id}/enable")

    async def disable_bot(self, bot_user_id: str) -> dict[str, Any]:
        """Disable a bot without deleting it (decommission)."""
        return await self._request("POST", f"/bots/{bot_user_id}/disable")

    async def create_user_access_token(
        self, user_id: str, description: str
    ) -> dict[str, Any]:
        """Mint a personal access token for *user_id*.

        The token value is returned **once**, here; afterwards only its
        id and description are readable, so the caller must persist it
        before doing anything else.
        """
        return await self._request(
            "POST",
            f"/users/{user_id}/tokens",
            json={"description": description},
        )

    async def list_user_access_tokens(self, user_id: str) -> list[dict[str, Any]]:
        """Token metadata for *user_id* — ids and descriptions, no values."""
        result = await self._request(
            "GET",
            f"/users/{user_id}/tokens",
            params={"page": 0, "per_page": _MAX_PER_PAGE},
        )
        return list(result or [])

    async def revoke_user_access_token(self, token_id: str) -> None:
        """Revoke a personal access token by id."""
        await self._request("POST", "/users/tokens/revoke", json={"token_id": token_id})

    async def set_user_roles(self, user_id: str, roles: str) -> dict[str, Any]:
        """Replace a user's space-separated system roles."""
        return await self._request(
            "PUT", f"/users/{user_id}/roles", json={"roles": roles}
        )

    # ----- server limits (provisioning preflight) ------------------------

    async def server_limits(self) -> dict[str, Any]:
        """The server's user-count limits, when it reports them.

        Unlicensed Mattermost enforces a hard active-**user** cap.  Bots
        are excluded from that count, so an agent fleet does not consume
        it — but the provisioner reads it anyway so its report can say
        how much human headroom is left rather than leaving an operator
        to discover the wall themselves.  Older servers have no such
        endpoint; an empty dict means "no limit reported".
        """
        try:
            return await self._request("GET", "/limits/users") or {}
        except MattermostError as exc:
            if exc.status in (401, 403, 404, 501):
                return {}
            raise


__all__ = [
    "CHANNEL_DIRECT",
    "CHANNEL_GROUP",
    "CHANNEL_OPEN",
    "CHANNEL_PRIVATE",
    "DEFAULT_TYPING_THROTTLE_MS",
    "DIRECT_CHANNEL_TYPES",
    "MattermostClient",
    "MattermostError",
    "WEBSOCKET_PATH",
    "normalize_base_url",
    "site_urls_match",
    "typing_throttle_from",
    "websocket_url",
]
