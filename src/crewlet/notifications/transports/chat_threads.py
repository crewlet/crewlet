"""Backend-neutral chat thread-follow tracking.

The rule is the same on every chat backend Crewlet speaks:

- Top-level channel messages are always delivered — the agent's bot is in
  the channel, so it should see them.
- Thread replies are delivered only if the agent is **following** that
  thread.
- An agent starts following when it is directly mentioned, when a
  collective address is used, when it posts in the thread itself, or when
  something subscribes it explicitly.

What differs per backend is only (a) the shape of a thread identifier and
(b) the *grammar* a mention is written in — Slack's ``<@U123>`` versus
Mattermost's literal ``@username``.  Both are abstracted: the tracker is
constructed for one ``backend`` and takes a :class:`MentionGrammar`, so
the follow model itself is written once.

Follow state is persisted via
:class:`~crewlet.db.chat_thread_follows.ChatThreadFollowRepository` so it
survives engine restarts.  No chat backend exposes per-bot thread
subscription state, so this tracker builds and maintains that state from
message-content signals.
"""

from __future__ import annotations

from enum import StrEnum
from typing import Protocol, runtime_checkable

from crewlet._logging import get_logger
from crewlet.db.chat_thread_follows import ChatThreadFollowRepository

logger = get_logger("notifications.chat_threads")


class ThreadFollowReason(StrEnum):
    """Why an agent started following a thread."""

    MENTION = "mention"
    """Agent was directly mentioned."""

    COLLECTIVE = "collective"
    """Collective address (``@channel`` / ``@here`` / ``@all``)."""

    PARTICIPATED = "participated"
    """Agent sent a message in the thread (outbound tracking)."""

    EXPLICIT = "explicit"
    """Programmatically subscribed via ``start_following()``."""


@runtime_checkable
class MentionGrammar(Protocol):
    """How one backend writes a mention.

    ``self_identity`` is whatever that backend addresses a bot BY — a
    user id on Slack (``<@U123>``), a username on Mattermost
    (``@agent-swe``).  Implementations must tolerate an empty
    ``self_identity`` (the bot's identity has not resolved yet) by
    falling through to the collective check rather than matching
    everything.
    """

    def detect(self, text: str, self_identity: str) -> ThreadFollowReason | None:
        """Follow reason triggered by *text*, or ``None``."""
        ...


class ChatThreadTracker:
    """Tracks which agents follow which threads, for one chat backend.

    PostgreSQL is the single source of truth.  The ``backend`` this
    tracker is constructed with scopes every read and write, so two
    backends' thread ids cannot collide.
    """

    def __init__(
        self,
        backend: str,
        db_repo: ChatThreadFollowRepository,
        grammar: MentionGrammar,
    ) -> None:
        self._backend = backend
        self._db = db_repo
        self._grammar = grammar

    @property
    def backend(self) -> str:
        """The chat backend whose namespace this tracker reads and writes."""
        return self._backend

    def detect_follow_trigger(
        self, text: str, self_identity: str
    ) -> ThreadFollowReason | None:
        """Follow reason *text* triggers under this backend's grammar."""
        return self._grammar.detect(text, self_identity)

    async def is_following(self, handle: str, channel: str, thread_id: str) -> bool:
        """Check if an agent is following a thread."""
        reason = await self._db.is_following(self._backend, handle, channel, thread_id)
        return reason is not None

    async def start_following(
        self,
        handle: str,
        channel: str,
        thread_id: str,
        reason: ThreadFollowReason = ThreadFollowReason.EXPLICIT,
    ) -> None:
        """Subscribe an agent to a thread."""
        await self._db.upsert(self._backend, handle, channel, thread_id, reason.value)
        logger.debug(
            "thread_following_started",
            backend=self._backend,
            handle=handle,
            thread=f"{channel}:{thread_id}",
            reason=reason.value,
        )

    async def record_participation(
        self, handle: str, channel: str, thread_id: str
    ) -> None:
        """Record that an agent sent a message in a thread.

        Auto-follows it, mirroring what every chat client does when a
        human replies to a thread.
        """
        if not thread_id:
            return
        if not await self.is_following(handle, channel, thread_id):
            await self.start_following(
                handle, channel, thread_id, ThreadFollowReason.PARTICIPATED
            )


__all__ = [
    "ChatThreadTracker",
    "MentionGrammar",
    "ThreadFollowReason",
]
