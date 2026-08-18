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

import asyncio
import uuid
from datetime import UTC
from email.utils import parsedate_to_datetime
from typing import Any

import httpx

from crewlet._logging import get_logger
from crewlet.providers.errors import parse_retry_after

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

#: Status codes worth retrying.  429 is the rate limiter (Mattermost
#: sends ``Retry-After`` with it); 502/503/504 are a proxy or a server
#: mid-restart, which is the docker-compose case where the container is
#: up but still migrating.  Every other 4xx is the caller's fault and
#: every other 5xx is not going to change on a second try.
_RETRY_STATUSES = frozenset({429, 502, 503, 504})

#: Total wall clock one call may spend waiting out retries, mirroring
#: ``slack/api.py``'s budgeted wait rather than inventing a second
#: pattern.  Ten seconds absorbs a proxy hiccup, a brief 429 and a
#: server finishing a restart, and deliberately stops short of waiting
#: out an outage: every caller here is already on a retry loop of its own
#: (the fleet's reconnect backoff, the scheduler, an operator re-running
#: a command), so a longer budget would not rescue anything — it would
#: only turn a server that is simply down into a stall repeated once per
#: seat, and a reconnect backfill walks every channel serially.
_RETRY_BUDGET_SECONDS = 10.0

#: Delay used when a retryable response carries no ``Retry-After``.
#: Doubles per attempt, capped by the budget above.
_RETRY_BASE_DELAY_SECONDS = 1.0

#: Default per-phase timeouts.  A single scalar would give a cosmetic
#: typing POST the same deadline as a bulk backfill read: connect is the
#: fast failure an unreachable host should produce, read is sized to a
#: backfill page, and pool acquisition must not silently absorb
#: contention between the fleet's serial reads.
_CONNECT_TIMEOUT_SECONDS = 5.0
_READ_TIMEOUT_SECONDS = 15.0
_WRITE_TIMEOUT_SECONDS = 10.0
_POOL_TIMEOUT_SECONDS = 5.0

#: Deadline for the typing indicator's POST.  It is re-asserted every
#: ``TimeBetweenUserTypingUpdatesMilliseconds`` (5 s by default), so a
#: call that outlives the next heartbeat is raising an indicator that is
#: already stale — better to drop it than to hold a pool connection the
#: same client's real work needs.
TYPING_TIMEOUT_SECONDS = 5.0

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


def browser_origin(base_url: str) -> str:
    """The ``Origin`` header a browser at *base_url* would send.

    Scheme and authority only, lower-cased — an ``Origin`` never carries
    a path, which matters twice: it is what Mattermost's websocket check
    compares, and it is the exact string ``AllowCorsFrom`` is matched
    against, so a probe that invents a path-bearing Origin fails a check
    every real browser passes.
    """
    from urllib.parse import urlsplit

    parts = urlsplit(normalize_base_url(base_url))
    if not parts.scheme or not parts.netloc:
        return ""
    return f"{parts.scheme.lower()}://{parts.netloc.lower()}"


def site_url_origin_matches(configured: str, reported: str) -> bool:
    """Whether a browser at *configured* passes the websocket origin check.

    Models the server's own rule rather than approximating it:
    ``EqualFold(origin.Host, siteURL.Host) && EqualFold(origin.Scheme,
    siteURL.Scheme)`` (``App.OriginChecker``).  So the comparison is
    case-insensitive and ignores the path entirely — a whole-URL string
    compare reports a mismatch for ``https://Chat.Example.com`` and for
    every subpath deployment, on installs whose live feed works
    perfectly.
    """
    left, right = browser_origin(configured), browser_origin(reported)
    return bool(left) and left == right


def site_url_path_matches(configured: str, reported: str) -> bool:
    """Whether the two agree on the subpath the server is served under.

    A separate question from the origin, with a separate consequence: a
    wrong path does not break the websocket, it breaks every absolute
    link and plugin URL the server builds.
    """
    from urllib.parse import urlsplit

    def _path(value: str) -> str:
        return urlsplit(normalize_base_url(value)).path.rstrip("/")

    return _path(configured) == _path(reported)


class MattermostError(Exception):
    """A Mattermost API call failed.

    Carries the method, path and status so a failure is actionable
    without turning on request logging.

    ``status == 0`` means the request never reached the server — a
    connect failure, a timeout, a dropped connection.  Every transport
    failure is funnelled through this type deliberately: callers all
    catch ``MattermostError`` and nothing else, so an escaping
    ``httpx.ConnectError`` did not degrade a single call, it aborted
    whatever sequence the call sat in.  One connect blip while the
    transport resolved bot identities at boot was enough to skip opening
    the websocket fleet for the entire life of the process.
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
        timeout: httpx.Timeout | float | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        self._base_url = normalize_base_url(base_url)
        self._token = token
        self._timeout = timeout or httpx.Timeout(
            connect=_CONNECT_TIMEOUT_SECONDS,
            read=_READ_TIMEOUT_SECONDS,
            write=_WRITE_TIMEOUT_SECONDS,
            pool=_POOL_TIMEOUT_SECONDS,
        )
        self._transport = transport
        self._client: httpx.AsyncClient | None = None
        self._closed = False

    @property
    def base_url(self) -> str:
        """The instance URL, without the ``/api/v4`` suffix."""
        return self._base_url

    def _http(self) -> httpx.AsyncClient:
        if self._closed:
            # Rebuilding here would be worse than failing: the caller
            # believes this client's credentials were released, and a
            # fresh pool nothing will ever close would quietly deliver
            # the request anyway — a message sent by a transport the
            # engine has torn down.
            raise MattermostError("client is closed", status=0)
        if self._client is None:
            headers = {"Content-Type": "application/json"}
            if self._token:
                headers["Authorization"] = f"Bearer {self._token}"
            self._client = httpx.AsyncClient(
                base_url=f"{self._base_url}/api/v4",
                headers=headers,
                timeout=self._timeout,
                transport=self._transport,
            )
        return self._client

    async def close(self) -> None:
        """Release the underlying HTTP client, permanently."""
        self._closed = True
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
        timeout: httpx.Timeout | float | None = None,
    ) -> Any:
        """One API call, with every failure mode funnelled into one type.

        Retries the handful of statuses that mean "ask again" — the rate
        limiter, and a proxy or server mid-restart — honouring
        ``Retry-After`` where the server sends one, within a wall-clock
        budget.  A retry is safe for the reads this client makes, and
        safe for the one write it makes because
        :meth:`create_post` carries the idempotency key Mattermost
        deduplicates on.
        """
        loop = asyncio.get_running_loop()
        deadline = loop.time() + _RETRY_BUDGET_SECONDS
        attempt = 0
        while True:
            attempt += 1
            try:
                resp = await self._http().request(
                    method,
                    path,
                    params=params,
                    json=json,
                    **({"timeout": timeout} if timeout is not None else {}),
                )
            except httpx.HTTPError as exc:
                # status=0: the request never reached the server. Callers
                # only catch MattermostError, so letting httpx through
                # here aborts whole sequences rather than one call.
                delay = self._retry_delay(attempt, None, loop, deadline)
                if delay is None:
                    raise MattermostError(
                        f"transport error: {exc}",
                        method=method,
                        path=path,
                        status=0,
                    ) from exc
                logger.warning(
                    "mattermost_request_retry",
                    method=method,
                    path=path,
                    reason=type(exc).__name__,
                    delay=round(delay, 2),
                )
                await asyncio.sleep(delay)
                continue

            if resp.status_code in _RETRY_STATUSES:
                raw = resp.headers.get("retry-after", "")
                hinted = parse_retry_after(raw) if raw else None
                delay = self._retry_delay(attempt, hinted, loop, deadline)
                if delay is not None:
                    logger.warning(
                        "mattermost_request_retry",
                        method=method,
                        path=path,
                        status=resp.status_code,
                        delay=round(delay, 2),
                    )
                    await asyncio.sleep(delay)
                    continue

            if 300 <= resp.status_code < 400:
                # A redirect usually means the configured URL is not the
                # one the server answers on (http:// in front of a
                # TLS-terminating proxy is the common case). Following it
                # would send the bearer token to whatever host it names,
                # so say what happened instead — the same mistake also
                # gives the websocket a plaintext ws:// URL, and nothing
                # else would name the cause.
                location = resp.headers.get("location", "")
                raise MattermostError(
                    f"redirected to {location or '(no Location header)'} — "
                    "set integrations.mattermost.url to the address the "
                    "server actually answers on, so the REST client and "
                    "the websocket both use it",
                    method=method,
                    path=path,
                    status=resp.status_code,
                )
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

    @staticmethod
    def _retry_delay(
        attempt: int,
        hinted: float | None,
        loop: asyncio.AbstractEventLoop,
        deadline: float,
    ) -> float | None:
        """How long to wait before retry *attempt*; ``None`` to give up.

        The server's own ``Retry-After`` wins when it sends one — it
        knows when its limiter resets — and the exponential fallback
        covers the transport failures and proxies that do not.
        """
        delay = (
            hinted
            if hinted is not None
            else _RETRY_BASE_DELAY_SECONDS * (2 ** (attempt - 1))
        )
        if loop.time() + delay > deadline:
            return None
        return delay

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

        ``/config/client?format=old`` is what the web app is handed, so
        reading it reads the value the BROWSER will act on rather than
        the one an admin API might report.

        Authentication changes what comes back: an unauthenticated read
        gets the **limited** client config (``SiteURL`` and the
        sign-in-page settings), an authenticated one gets the full
        superset.  ``SiteURL`` is in both, which is what lets a health
        check compare it with no credential at all; per-feature settings
        like ``TimeBetweenUserTypingUpdatesMilliseconds`` are only in the
        authenticated form.

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

    async def get_channel_member(
        self, channel_id: str, user_id: str
    ) -> dict[str, Any] | None:
        """The membership row, or ``None`` when the user is not a member.

        Membership is verified rather than inferred from an add's status
        code: Mattermost answers an add for an existing member with
        success, so a 400 or 409 there is a REAL failure — an archived
        channel, a guest restriction, a group-constrained team — and
        reading it as "already joined" reports a bot into a channel it
        cannot hear.
        """
        try:
            return await self._request(
                "GET", f"/channels/{channel_id}/members/{user_id}"
            )
        except MattermostError as exc:
            if exc.status in (403, 404):
                return None
            raise

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

    async def get_team_member(
        self, team_id: str, user_id: str
    ) -> dict[str, Any] | None:
        """The team-membership row, or ``None``.  See
        :meth:`get_channel_member` for why this is verified, not inferred."""
        try:
            return await self._request("GET", f"/teams/{team_id}/members/{user_id}")
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
        pending_post_id: str = "",
    ) -> dict[str, Any]:
        """Post a message, optionally as a reply in *root_id*'s thread.

        Carries a ``pending_post_id``, which is Mattermost's own
        idempotency key: the server deduplicates a create whose pending
        id it has already seen within its caching window
        (``App.deduplicateCreatePost``).  This is the only write this
        client makes, and without the key a retry — of the transport
        retry above, or of an operator re-running something — would
        double-post an agent's reply into a human's thread.  It is
        generated once per call, so every retry of *this* send reuses it;
        pass one in to make a send idempotent across a wider scope.
        """
        payload: dict[str, Any] = {
            "channel_id": channel_id,
            "message": message,
            "pending_post_id": pending_post_id or uuid.uuid4().hex,
        }
        if root_id:
            payload["root_id"] = root_id
        return await self._request("POST", "/posts", json=payload)

    async def posts_since(self, channel_id: str, since_ms: int) -> list[dict[str, Any]]:
        """Posts **created** in a channel since *since_ms*.

        The reconnect-gap primitive.  Mattermost's websocket replays
        nothing after a disconnect, so the fleet re-reads each channel it
        was watching from the last post it saw.  Returned oldest-first,
        which is the order the fleet must replay them in.

        The server's ``since=`` is ``UpdateAt``-based, and ``UpdateAt`` is
        bumped by things that are not new content: adding or removing a
        reaction touches the post, editing one touches it, and deleting a
        reply touches the thread's root.  Replaying those would wake an
        agent to re-answer a message it has already answered — a full LLM
        turn per 👍 landing during a reconnect.  So the server's filter is
        a superset and ``create_at`` is the one that decides, which also
        keeps the gap replay to exactly what the live socket would have
        delivered (the fleet does not forward edits either).  Deleted
        posts are dropped here rather than downstream, since ``since=``
        returns them as tombstones.
        """
        data = await self._request(
            "GET", f"/channels/{channel_id}/posts", params={"since": since_ms}
        )
        order = list((data or {}).get("order") or [])
        posts = (data or {}).get("posts") or {}
        replayable = []
        # ``order`` is newest-first; the caller replays chronologically.
        for post_id in reversed(order):
            post = posts.get(post_id)
            if not isinstance(post, dict) or post.get("delete_at"):
                continue
            if int(post.get("create_at") or 0) > since_ms:
                replayable.append(post)
        return replayable

    # ----- typing indicator --------------------------------------------

    async def publish_typing(
        self,
        channel_id: str,
        *,
        parent_id: str = "",
        timeout: float = TYPING_TIMEOUT_SECONDS,
    ) -> None:
        """Raise the composer typing indicator for this client's user.

        Thread-scoped when *parent_id* is set.  Fixed vocabulary: the
        wording is the client's, so nothing can be said with it beyond
        "this user is typing".

        Given a deadline of its own, shorter than every other call's: it
        is re-asserted on a few-second heartbeat, so a call that outlives
        the next beat is pure backlog — the indicator it would have
        raised is already stale, and holding a connection for it starves
        the pool the same client's real work uses.
        """
        payload: dict[str, Any] = {"channel_id": channel_id}
        if parent_id:
            payload["parent_id"] = parent_id
        await self._request("POST", "/users/me/typing", json=payload, timeout=timeout)

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

    async def server_config(self) -> dict[str, Any]:
        """The full (sanitized) server config — system-admin only.

        Read by the provisioning preflight for the two settings the whole
        run depends on.  Sanitized by the server, so no secret comes
        back; still never logged, because "sanitized" is the server's
        judgement rather than this client's.
        """
        return await self._request("GET", "/config") or {}

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
            return await self._request("GET", "/limits/server") or {}
        except MattermostError as exc:
            # 403: an older build refusing a non-admin. 404/501: a server
            # predating the route. A 401 is NOT swallowed — it means the
            # operator credential is bad, and a provisioner that shrugged
            # at that would go on to make writes it cannot make.
            if exc.status in (403, 404, 501):
                return {}
            raise


__all__ = [
    "CHANNEL_DIRECT",
    "CHANNEL_GROUP",
    "CHANNEL_OPEN",
    "CHANNEL_PRIVATE",
    "DEFAULT_TYPING_THROTTLE_MS",
    "TYPING_TIMEOUT_SECONDS",
    "DIRECT_CHANNEL_TYPES",
    "MattermostClient",
    "MattermostError",
    "WEBSOCKET_PATH",
    "normalize_base_url",
    "browser_origin",
    "site_url_origin_matches",
    "site_url_path_matches",
    "typing_throttle_from",
    "websocket_url",
]
