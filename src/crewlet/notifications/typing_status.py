"""Working-status indicator — "is thinking…" while an agent reasons.

An agent turn takes minutes.  Without a signal, the human who posted sees
nothing until the reply lands and cannot tell "the bot is working" from
"the bot is dead".  Every chat backend offers *some* way to close that
gap, but they differ in one way that matters enough to be modelled
explicitly: **whether the indicator carries text.**

- **Slack** renders free text via `assistant.threads.setStatus
  <https://docs.slack.dev/reference/methods/assistant.threads.setStatus/>`_
  ("*Agent SWE is crewleting…*").  Slack has no public ``user_typing`` API
  for granular apps (`slackapi/bolt-js#885
  <https://github.com/slackapi/bolt-js/issues/885>`_), and since `March
  2026 <https://docs.slack.dev/changelog/2026/03/05/set-status-scope-update/>`_
  ``setStatus`` accepts the plain ``chat:write`` scope every Slack-enabled
  agent already holds.
- **Mattermost** offers only the composer typing indicator, whose wording
  is fixed by the client.  The engine can raise it, but not say anything
  with it.

A backend advertises which it is through
:attr:`StatusPoster.supports_status_text`.  When text is unsupported the
phrase machinery below is inert — the session still runs, keeping the
indicator alive, but phase changes stop costing a request, because there
is nothing about them the reader could see.

Two properties of every backend's mechanism shape the lifecycle:

- **A raised status expires.**  Slack drops one after 2 minutes;
  Mattermost's is shorter still.  So a session keeps a heartbeat task
  that re-asserts well inside that window, at the poster's own
  :attr:`~StatusPoster.status_refresh_interval`.
- **Posting into the conversation clears it.**  That covers the "agent
  replied" half of the lifecycle for free.  The "agent gave up" half —
  the planner decided the message wasn't addressed to it, the turn
  failed, the budget ran out — has no backend-side signal, so the session
  always clears explicitly when the turn ends.

Lifecycle owner is the :class:`~crewlet.agent.turn.TurnEngine`: it opens a
session when a chat-triggered turn starts and closes it when the turn
ends, updating the text at each phase boundary.  A turn that SUSPENDS for
a detached sandbox run holds its session open — the same ``turn_id``
resumes when the coding job completes, and the human is still waiting.

Sessions are keyed by ``(handle, channel, thread_id)`` and
reference-counted by ``turn_id``, so two turns for the same agent in the
same thread (a suspend/resume pair, or a queued follow-up) share one
heartbeat and the status clears only when the last one finishes.

The words themselves come from :class:`StatusPhrases` — a pool per phase,
drawn from deterministically so a turn's line is stable while it holds a
phase and changes when it moves on.  Companies override the pools via
``integrations.slack.status_phrases``; backends that cannot render text
expose no such setting.
"""

from __future__ import annotations

import asyncio
import contextlib
import hashlib
import time
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any, Protocol, runtime_checkable

from crewlet._logging import get_logger

logger = get_logger("notifications.typing_status")


# Slack expires a status set via ``assistant.threads.setStatus`` after two
# minutes.  Re-asserting every 45 s leaves room for one call to be lost or
# rate-limited (t+45 fails → t+90 still lands 30 s before the t+120
# expiry).  Cost is ~1.3 requests/minute per in-flight conversation against
# Slack's 600/minute per-app-per-team limit, so even a fully saturated
# engine (dozens of concurrent turns) stays two orders of magnitude clear.
#
# This is the DEFAULT and Slack's value; a poster whose backend expires
# faster advertises its own via ``StatusPoster.status_refresh_interval``.
REFRESH_INTERVAL_SECONDS = 45.0

# Backstop for a session whose owner never closed it — realistically an
# agent wedged in ``AWAITING_SANDBOX`` on a job that never reports back.
# The liveness probe below is the primary guard (it fires within one
# refresh of the agent going idle); this cap bounds the pathological case
# where the agent stays *busy* forever.  One hour is far longer than any
# real chat-triggered turn (a multi-tool Plan → Execute → Review is
# minutes; a detached coding run is bounded by the agent's token budget)
# and short enough that a leak self-heals inside a working session.
MAX_SESSION_SECONDS = 3600.0

#: Normalised channel kinds that mean "this is a direct conversation".
#: Covers both vocabularies the engine sees: Slack's ``im`` / ``mpim`` and
#: Mattermost's single-letter channel types ``D`` (direct) / ``G`` (group).
#: Membership here is what makes a message count as *addressed* without a
#: mention — in a DM there is nobody else it could be for.
DIRECT_CHANNEL_TYPES = frozenset({"im", "mpim", "D", "G"})

# Per-phase pools of status text.  Rendered suffixed to the agent's name,
# so they read as "Agent SWE is crewleting…" — hence the leading "is".
#
# A *pool* rather than one fixed string per phase, because this line is
# the only thing a waiting human sees for minutes at a time.  Drawing a
# fresh line at every phase boundary makes movement legible: "plan →
# execute → review" reads as three different sentences instead of a
# generic label that could equally mean "wedged", and a turn that loops
# back through Plan via ``self_iterate`` visibly starts over rather than
# re-showing the identical text.  Within a phase the line is stable — the
# heartbeat re-asserts the same words, so nothing flickers.
#
# The ``"default"`` pool covers any phase name not listed here; the
# engine only drives the four above, so it is a guard, not a path.
#
# THE RULE FOR A PHRASE: it may describe the PHASE, never a specific
# action.  The pick is arbitrary — nothing consults what the agent is
# actually doing — so a line naming work the agent is merely *capable*
# of ("is consulting the org chart…", "is reading the handbook…",
# "is looking for typos…") reads as a status report and is wrong most of
# the time it shows.  A human who sees the agent claim a lookup it never
# made learns to distrust the whole indicator.  Phrases are therefore
# either generic to the phase ("is thinking…", "is double-checking…") or
# plainly figurative ("is finding the coffee machine…") — nothing a
# reader could mistake for a fact about this turn.
#
# Operators override any pool from config
# (``integrations.slack.status_phrases``) — the built-ins lean on the
# engine's own name, and a company running Crewlet will want its own.
# The rule above applies to those too.  See ``docs/integrations/slack.md``.
PHASE_PHRASES: dict[str, tuple[str, ...]] = {
    "onboarding": (
        "is getting crewleted in...",
        "is getting up to speed...",
        "is settling in...",
        "is getting its bearings...",
        "is finding its feet...",
        "is learning the secret handshake...",
        "is finding the coffee machine...",
        "is unpacking its desk...",
    ),
    "plan": (
        "is crewleting...",
        "is thinking...",
        "is scheming...",
        "is thinking it through...",
        "is mulling it over...",
        "is assembling a cunning plan...",
        "is putting the pieces together...",
        "is thinking about thinking...",
    ),
    "execute": (
        "is crewing...",
        "is working on it...",
        "is doing the doing...",
        "is rolling up its sleeves...",
        "is executing the cunning plan...",
        "is cracking on...",
        "is making things happen...",
        "is getting it done...",
    ),
    "review": (
        "is re-crewleting...",
        "is double-checking...",
        "is marking its own homework...",
        "is squinting at its own work...",
        "is sanity-checking...",
        "is having one more look...",
        "is asking itself the hard questions...",
        "is doing one last pass...",
    ),
    "default": (
        "is crewleting...",
        "is thinking...",
        "is on it...",
        "is busy being useful...",
    ),
}

# Key of the pool a phase falls back to when it has none of its own.
DEFAULT_PHASE = "default"


@dataclass(frozen=True)
class StatusPhrases:
    """The lines each turn phase draws its working status from.

    Built-in pools by default; ``with_overrides`` layers an operator's
    ``integrations.slack.status_phrases`` on top, per phase, so a company
    can replace only the phases it cares about.
    """

    pools: Mapping[str, tuple[str, ...]] = field(
        default_factory=lambda: dict(PHASE_PHRASES)
    )

    @classmethod
    def with_overrides(
        cls, overrides: Mapping[str, Sequence[str]] | None
    ) -> StatusPhrases:
        """Built-in pools with each supplied phase's pool replaced.

        An absent or empty list keeps that phase's built-in pool —
        "override nothing" and "override with nothing" are the same
        request, and an empty status is not a rendering, it's a *cleared*
        indicator.
        """
        pools = dict(PHASE_PHRASES)
        for phase, phrases in (overrides or {}).items():
            cleaned = tuple(p.strip() for p in phrases if p and p.strip())
            if cleaned:
                pools[phase] = cleaned
        return cls(pools=pools)

    def pick(self, phase: str, *, seed: str, rotation: int) -> str:
        """Choose *phase*'s line for a session at *rotation* steps in.

        Deterministic in ``(seed, phase, rotation)``: the same turn always
        renders the same words — which is what lets the heartbeat re-assert
        a status without the text drifting under the reader — while
        different turns in the same thread start from different points in
        the pool.  ``blake2b`` rather than ``hash()`` because the latter is
        salted per process, so a restart mid-conversation would jump to an
        unrelated line.
        """
        pool = (
            self.pools.get(phase)
            or self.pools.get(DEFAULT_PHASE)
            or PHASE_PHRASES[DEFAULT_PHASE]
        )
        digest = hashlib.blake2b(f"{seed}|{phase}".encode(), digest_size=8).digest()
        return pool[(int.from_bytes(digest, "big") + rotation) % len(pool)]


class WorkingStatusMode(StrEnum):
    """When an agent shows a working status on a chat backend.

    ``addressed`` (the default) covers exactly the cases where a human is
    plausibly waiting on *this* agent: a DM or group DM, a direct
    ``@mention``, or a thread the agent already follows.  A broadcast
    (``@here`` / ``@channel``) and a passive top-level channel message are
    deliberately excluded — every bot in the channel is woken by those, and
    the chat triage prompt tells most of them to stay silent, so lighting
    up N indicators would be noise rather than signal.

    ``always`` shows it on every chat-triggered turn (useful for a
    single-agent workspace); ``off`` disables the feature entirely.
    """

    OFF = "off"
    ADDRESSED = "addressed"
    ALWAYS = "always"


@dataclass(frozen=True)
class ChatConversation:
    """The thread a status belongs to, on whichever backend raised it."""

    channel: str
    thread_id: str


def _field(metadata: Mapping[str, Any], key: str) -> str:
    """Read one metadata field as a string.

    Notification metadata is ``dict[str, str]`` by contract, but it
    reaches the turn engine as ``dict[str, Any]`` (the TurnContext field
    is untyped so it can also carry A2A / extension payloads), so the
    coercion keeps a stray non-string from raising inside a cosmetic
    side-channel.
    """
    value = metadata.get(key)
    return "" if value is None else str(value)


def chat_conversation(
    metadata: Mapping[str, Any] | None,
    backend: str,
) -> ChatConversation | None:
    """Resolve the thread a turn's trigger metadata points at, for *backend*.

    Returns ``None`` for a trigger that did not come from *backend*.  The
    discriminator is the ``transport`` key every chat transport stamps on
    the notifications it parses — it survives inbox coalescing (the merged
    event mirrors the latest constituent's metadata) and the
    detached-sandbox round trip (the pending run stores the originating
    ``notification_metadata``), so a resumed turn resolves the same
    conversation as its kick-off.

    Raising a status needs a thread anchor.  For a top-level message no
    thread exists yet, so the message's own id is the anchor — the same
    value the chat prompt tells the agent to reply under, which is what
    makes the status appear where the reply will land.
    """
    if not metadata:
        return None
    if _field(metadata, "transport") != backend:
        return None
    channel = _field(metadata, "channel")
    anchor = _field(metadata, "thread_ts") or _field(metadata, "ts")
    if not channel or not anchor:
        return None
    return ChatConversation(channel=channel, thread_id=anchor)


def agent_is_addressed(
    metadata: Mapping[str, Any],
    *,
    dm_channel_id_prefix: str = "",
) -> bool:
    """Is a human plausibly waiting on THIS agent for this message?

    True for a DM / group DM, a direct ``@mention``, and a thread the
    agent already follows.  False for a passive top-level channel message
    and for collective addresses (``@here`` / ``@channel`` / ``@all``) —
    see :class:`WorkingStatusMode`.

    Primarily metadata-driven: every chat transport normalises its own
    channel-kind vocabulary into ``channel_type``
    (:data:`DIRECT_CHANNEL_TYPES` spans them all), so this does not need
    to know any backend's id conventions.

    ``dm_channel_id_prefix`` is the belt-and-braces fallback for the one
    backend that has a meaningful one: a Slack channel id beginning
    ``D`` is always a DM, which still resolves correctly if metadata
    reaches here without ``channel_type`` at all.  It is **opt-in per
    backend** and must stay empty for backends whose channel ids are
    opaque — Mattermost's are 26-char alphanumerics, so a prefix test
    there would mark arbitrary public channels as direct messages and
    raise indicators for traffic nobody addressed to this agent.
    """
    if _field(metadata, "channel_type") in DIRECT_CHANNEL_TYPES:
        return True
    if dm_channel_id_prefix and _field(metadata, "channel").startswith(
        dm_channel_id_prefix
    ):
        return True
    if _field(metadata, "thread_follow_reason") == "mention":
        return True
    return bool(_field(metadata, "thread_following"))


@runtime_checkable
class StatusPoster(Protocol):
    """The backend call this module drives, plus what it can express.

    Implemented by each chat transport, which owns the per-agent
    credentials.  ``set_status`` never raises — a failed call returns
    ``False`` and the status simply expires on the backend's side.
    """

    status_backend: str
    """Transport name (``"slack"``, ``"mattermost"``) — matched against
    the ``transport`` key on a trigger's metadata."""

    supports_status_text: bool
    """Whether ``status`` is rendered.  ``False`` for a fixed-vocabulary
    typing indicator, which makes the phrase pools inert."""

    status_refresh_interval: float
    """Seconds between heartbeats, sized to this backend's expiry."""

    dm_channel_id_prefix: str
    """Channel-id prefix that unambiguously marks a DM on this backend,
    or ``""`` when its ids are opaque.  See :func:`agent_is_addressed`."""

    async def set_status(
        self, handle: str, channel: str, thread_id: str, status: str
    ) -> bool: ...

    async def clear_status(self, handle: str, channel: str, thread_id: str) -> bool:
        """Take the indicator down.

        Separate from ``set_status("")`` because the two are only the
        same operation on a backend that renders text.  Slack clears by
        setting an empty status; a fixed-vocabulary typing indicator has
        no text to empty and no clear operation at all — it lapses on its
        own — so overloading the payload made "raise the indicator" and
        "clear the indicator" indistinguishable, and such a backend had
        to refuse both.  Optional: a poster that does not implement it
        falls back to ``set_status(..., "")``.
        """
        ...


_Key = tuple[str, str, str]


@dataclass
class _Session:
    """One live status indicator, shared by every turn holding it open.

    ``seed`` fixes which lines this conversation draws (see
    :meth:`StatusPhrases.pick`) and ``rotation`` counts the phase changes
    it has been through, so each boundary advances to the next line.
    """

    handle: str
    conversation: ChatConversation
    owners: set[str]
    seed: str
    phase: str
    status: str
    started_at: float
    liveness: Callable[[], bool]
    rotation: int = 0
    task: asyncio.Task[None] | None = field(default=None, repr=False)


class WorkingStatusSession:
    """A single turn's claim on a conversation's status indicator.

    Returned by :meth:`WorkingStatusDriver.begin`; the turn engine holds it
    for the life of the turn.  Every method is best-effort and never
    raises — a broken indicator must not fail an agent turn.
    """

    def __init__(self, manager: WorkingStatusDriver, key: _Key, turn_id: str) -> None:
        self._manager = manager
        self._key = key
        self._turn_id = turn_id

    @property
    def conversation(self) -> ChatConversation:
        return ChatConversation(channel=self._key[1], thread_id=self._key[2])

    async def set_phase(self, phase: str) -> None:
        """Swap the status text as the turn moves between phases."""
        await self._manager._set_phase(self._key, phase)

    async def end(self, *, keep_alive: bool = False) -> None:
        """Release this turn's claim.

        ``keep_alive=True`` keeps the indicator up even though the turn
        ended — used when Execute SUSPENDED for a detached sandbox run:
        the agent has neither replied nor given up, and the *same*
        ``turn_id`` resumes when the job completes.
        """
        await self._manager._end(self._key, self._turn_id, keep_alive=keep_alive)


class WorkingStatusDriver:
    """Drives one chat backend's working indicator for in-flight turns.

    Owned by the transport (which holds the per-agent credentials and the
    HTTP client), so its heartbeats are torn down with the transport on
    shutdown or a live config swap.
    """

    def __init__(
        self,
        poster: StatusPoster,
        mode: WorkingStatusMode = WorkingStatusMode.ADDRESSED,
        *,
        phrases: StatusPhrases | None = None,
        refresh_interval: float | None = None,
        max_session_seconds: float = MAX_SESSION_SECONDS,
    ) -> None:
        self._poster = poster
        self._mode = mode
        self._phrases = phrases if phrases is not None else StatusPhrases()
        # The poster's own cadence wins unless a caller pins one (tests,
        # or an operator working around a backend quirk): the interval is
        # a property of how fast that backend expires a status, and the
        # poster is what knows.
        self._refresh_interval = (
            refresh_interval
            if refresh_interval is not None
            else getattr(poster, "status_refresh_interval", REFRESH_INTERVAL_SECONDS)
        )
        self._max_session_seconds = max_session_seconds
        self._sessions: dict[_Key, _Session] = {}
        self._lock = asyncio.Lock()

    @property
    def backend(self) -> str:
        """The chat backend this driver raises indicators on."""
        return getattr(self._poster, "status_backend", "")

    @property
    def supports_status_text(self) -> bool:
        """Whether this backend renders the phrase text at all."""
        return bool(getattr(self._poster, "supports_status_text", True))

    @property
    def mode(self) -> WorkingStatusMode:
        return self._mode

    @property
    def phrases(self) -> StatusPhrases:
        """The phrase book this driver draws status lines from."""
        return self._phrases

    @property
    def refresh_interval(self) -> float:
        """Seconds between heartbeats for this backend."""
        return self._refresh_interval

    @property
    def active_conversations(self) -> list[ChatConversation]:
        """Conversations currently showing a status (for tests / debug)."""
        return [s.conversation for s in self._sessions.values()]

    def _initial_status(self, phase: str, seed: str) -> str:
        """The text a new session opens with.

        Empty on a backend that cannot render text — there is nothing to
        say, and a phrase drawn but never shown would only mislead a
        reader of the logs.
        """
        if not self.supports_status_text:
            return ""
        return self._phrases.pick(phase, seed=seed, rotation=0)

    async def begin(
        self,
        *,
        handle: str,
        turn_id: str,
        metadata: Mapping[str, Any] | None,
        liveness: Callable[[], bool] | None = None,
        phase: str = "plan",
    ) -> WorkingStatusSession | None:
        """Open (or join) the status session for a turn's conversation.

        Returns ``None`` — meaning "nothing to drive" — when the feature
        is off, the trigger isn't from this backend, or the mode is
        ``addressed`` and nobody addressed this agent.  ``liveness`` is
        polled by the heartbeat so an indicator can't outlive a busy agent
        by more than one refresh interval, even if the owning turn dies
        without calling :meth:`WorkingStatusSession.end`.
        """
        if self._mode is WorkingStatusMode.OFF or not handle:
            return None
        conversation = chat_conversation(metadata, self.backend)
        if conversation is None:
            return None
        assert metadata is not None  # chat_conversation rejects None
        if self._mode is WorkingStatusMode.ADDRESSED and not agent_is_addressed(
            metadata,
            dm_channel_id_prefix=getattr(self._poster, "dm_channel_id_prefix", ""),
        ):
            logger.debug(
                "working_status_not_addressed",
                backend=self.backend,
                handle=handle,
                channel=conversation.channel,
            )
            return None

        key = (handle, conversation.channel, conversation.thread_id)
        async with self._lock:
            session = self._sessions.get(key)
            if session is None:
                # The turn_id is in the seed so two turns in the same
                # thread don't open on the same words — a follow-up that
                # re-renders the identical line reads as a stuck
                # indicator rather than a fresh turn.
                seed = (
                    f"{handle}|{conversation.channel}|"
                    f"{conversation.thread_id}|{turn_id}"
                )
                session = _Session(
                    handle=handle,
                    conversation=conversation,
                    owners={turn_id},
                    seed=seed,
                    phase=phase,
                    status=self._initial_status(phase, seed),
                    started_at=time.monotonic(),
                    liveness=liveness if liveness is not None else (lambda: True),
                )
                self._sessions[key] = session
                session.task = asyncio.create_task(
                    self._refresh_loop(key, session),
                    name=f"{self.backend or 'chat'}-working-status-{handle}",
                )
                logger.info(
                    "working_status_started",
                    backend=self.backend,
                    handle=handle,
                    channel=conversation.channel,
                    thread_id=conversation.thread_id,
                    turn_id=turn_id,
                )
            else:
                session.owners.add(turn_id)
                self._advance(session, phase)
                if liveness is not None:
                    session.liveness = liveness
            current = session.status
        await self._post(session, current)
        return WorkingStatusSession(self, key, turn_id)

    async def stop(self) -> None:
        """Cancel every heartbeat and clear every live status.

        Called by the transport's ``stop()`` so a shutdown (or a live
        config swap that rebuilds the transport) never leaves an agent
        looking like it is still thinking.
        """
        async with self._lock:
            sessions = list(self._sessions.values())
            self._sessions.clear()
        for session in sessions:
            await self._finish(session, cancel=True)

    # ----- internals --------------------------------------------------

    def _advance(self, session: _Session, phase: str) -> bool:
        """Move *session* to *phase*, drawing its next line.

        Returns ``False`` — nothing to post — when the session is already
        in that phase, or when the backend cannot render text at all (the
        heartbeat is already keeping the indicator up, and re-posting on
        a boundary nobody can see is a wasted request against a
        typing-indicator rate limit).

        The guard is on the *phase*, not the rendered text: with a pool
        behind each phase, re-picking for the same phase would swap the
        words for no reason a reader could see.

        Caller holds the lock.
        """
        if session.phase == phase:
            return False
        session.phase = phase
        if not self.supports_status_text:
            return False
        session.rotation += 1
        session.status = self._phrases.pick(
            phase, seed=session.seed, rotation=session.rotation
        )
        return True

    async def _set_phase(self, key: _Key, phase: str) -> None:
        async with self._lock:
            session = self._sessions.get(key)
            if session is None or not self._advance(session, phase):
                return
            text = session.status
        await self._post(session, text)

    async def _end(self, key: _Key, turn_id: str, *, keep_alive: bool) -> None:
        if keep_alive:
            logger.debug("working_status_held", turn_id=turn_id)
            return
        async with self._lock:
            session = self._sessions.get(key)
            if session is None:
                return
            session.owners.discard(turn_id)
            if session.owners:
                return
            del self._sessions[key]
        logger.info(
            "working_status_cleared",
            backend=self.backend,
            handle=session.handle,
            channel=session.conversation.channel,
            thread_id=session.conversation.thread_id,
            turn_id=turn_id,
        )
        await self._finish(session, cancel=True)

    async def _finish(self, session: _Session, *, cancel: bool) -> None:
        """Stop a session's heartbeat and clear its status."""
        task = session.task
        if cancel and task is not None and task is not asyncio.current_task():
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task
        await self._clear(session)

    async def _refresh_loop(self, key: _Key, session: _Session) -> None:
        """Re-assert the status until the session ends.

        Every backend drops a status after a while, and clears it outright
        the moment the agent posts into the thread — so the same loop that
        keeps a long turn's indicator alive is also what brings it back
        after a mid-turn reply while later phases are still running.
        """
        try:
            while True:
                await asyncio.sleep(self._refresh_interval)
                if self._sessions.get(key) is not session:
                    return
                elapsed = time.monotonic() - session.started_at
                if elapsed >= self._max_session_seconds:
                    logger.warning(
                        "working_status_expired",
                        backend=self.backend,
                        handle=session.handle,
                        channel=session.conversation.channel,
                        elapsed_seconds=round(elapsed, 1),
                        owners=sorted(session.owners),
                    )
                    await self._abandon(key, session)
                    return
                if not session.liveness():
                    logger.info(
                        "working_status_abandoned",
                        backend=self.backend,
                        handle=session.handle,
                        channel=session.conversation.channel,
                        owners=sorted(session.owners),
                    )
                    await self._abandon(key, session)
                    return
                await self._post(session, session.status)
        except asyncio.CancelledError:
            raise
        except Exception:
            # A crashed heartbeat must not take the agent down with it;
            # the status simply expires on the backend's side.
            logger.exception(
                "working_status_refresh_failed",
                backend=self.backend,
                handle=session.handle,
                channel=session.conversation.channel,
            )

    async def _abandon(self, key: _Key, session: _Session) -> None:
        """Drop a session from inside its own heartbeat task."""
        async with self._lock:
            if self._sessions.get(key) is session:
                del self._sessions[key]
        await self._finish(session, cancel=False)

    async def _clear(self, session: _Session) -> None:
        """Take a session's indicator down, however this backend does it."""
        clear = getattr(self._poster, "clear_status", None)
        if clear is None:
            # A poster written before the protocol split still clears the
            # only way it knows.
            await self._post(session, "")
            return
        try:
            await clear(
                session.handle,
                session.conversation.channel,
                session.conversation.thread_id,
            )
        except Exception as exc:
            logger.warning(
                "working_status_clear_failed",
                backend=self.backend,
                handle=session.handle,
                channel=session.conversation.channel,
                error=str(exc),
            )

    async def _post(self, session: _Session, status: str) -> None:
        try:
            ok = await self._poster.set_status(
                session.handle,
                session.conversation.channel,
                session.conversation.thread_id,
                status,
            )
        except Exception as exc:
            logger.warning(
                "working_status_post_failed",
                backend=self.backend,
                handle=session.handle,
                channel=session.conversation.channel,
                error=str(exc),
            )
            return
        if not ok:
            logger.debug(
                "working_status_post_rejected",
                backend=self.backend,
                handle=session.handle,
                channel=session.conversation.channel,
            )


__all__ = [
    "DEFAULT_PHASE",
    "DIRECT_CHANNEL_TYPES",
    "MAX_SESSION_SECONDS",
    "PHASE_PHRASES",
    "REFRESH_INTERVAL_SECONDS",
    "ChatConversation",
    "StatusPhrases",
    "StatusPoster",
    "WorkingStatusDriver",
    "WorkingStatusMode",
    "WorkingStatusSession",
    "agent_is_addressed",
    "chat_conversation",
]
