"""Slack thread-follow tracker — controls which thread replies reach each agent.

Architecture:
- Top-level channel messages are always delivered (the agent's bot is in the
  channel, so it should see them).
- Thread replies are only delivered if the agent is **following** that thread.
- An agent starts following a thread when:
  1. They are directly mentioned (``<@BOT_USER_ID>``) in the thread.
  2. A collective address (``<!channel>``, ``<!here>``) is used.
  3. They are explicitly subscribed via ``start_following()``.

Follow state is persisted in PostgreSQL via
:class:`~crewlet.db.slack_thread_follows.SlackThreadFollowRepository` so it
survives engine restarts.

Slack's Events API does not expose per-bot thread subscription state, so
this tracker builds and maintains that state based on message content signals.
"""

from __future__ import annotations

import re
from enum import StrEnum

from crewlet._logging import get_logger
from crewlet.db.slack_thread_follows import SlackThreadFollowRepository

logger = get_logger("notifications.slack_threads")

# Matches Slack user mentions like <@U12345> or <@W12345>
_USER_MENTION_RE = re.compile(r"<@([UW][A-Z0-9_]+)>")

# Matches Slack special mentions <!channel> and <!here>
_COLLECTIVE_RE = re.compile(r"<!(?:channel|here)(?:\|[^>]*)?>")


class ThreadFollowReason(StrEnum):
    """Why an agent started following a thread."""

    MENTION = "mention"
    """Agent was directly mentioned (``<@BOT_USER_ID>``)."""

    COLLECTIVE = "collective"
    """Collective address (``<!channel>``, ``<!here>``)."""

    PARTICIPATED = "participated"
    """Agent sent a message in the thread (outbound tracking)."""

    EXPLICIT = "explicit"
    """Programmatically subscribed via ``start_following()``."""


class SlackThreadTracker:
    """Tracks which agents are following which Slack threads.

    PostgreSQL is the single source of truth.
    """

    def __init__(self, db_repo: SlackThreadFollowRepository) -> None:
        self._db = db_repo

    async def is_following(self, handle: str, channel: str, thread_ts: str) -> bool:
        """Check if an agent is following a thread."""
        reason_str = await self._db.is_following(handle, channel, thread_ts)
        return reason_str is not None

    async def start_following(
        self,
        handle: str,
        channel: str,
        thread_ts: str,
        reason: ThreadFollowReason = ThreadFollowReason.EXPLICIT,
    ) -> None:
        """Subscribe an agent to a thread."""
        await self._db.upsert(handle, channel, thread_ts, reason.value)
        logger.debug(
            "thread_following_started",
            handle=handle,
            thread=f"{channel}:{thread_ts}",
            reason=reason.value,
        )

    async def record_participation(
        self, handle: str, channel: str, thread_ts: str
    ) -> None:
        """Record that an agent sent a message in a thread.

        Auto-follows the thread (like Slack's own behavior when you
        reply to a thread).
        """
        if not thread_ts:
            return
        if not await self.is_following(handle, channel, thread_ts):
            await self.start_following(
                handle, channel, thread_ts, ThreadFollowReason.PARTICIPATED
            )


def detect_follow_trigger(text: str, bot_user_id: str) -> ThreadFollowReason | None:
    """Detect if message text contains a follow trigger for the agent.

    Returns the reason if a trigger is found, ``None`` otherwise.
    """
    # Direct mention: <@U12345>
    if bot_user_id:
        mentions = _USER_MENTION_RE.findall(text)
        if bot_user_id in mentions:
            return ThreadFollowReason.MENTION

    # Collective: <!channel> or <!here>
    if _COLLECTIVE_RE.search(text):
        return ThreadFollowReason.COLLECTIVE

    return None
