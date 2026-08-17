"""Slack's mention grammar, plus its thread-tracker factory.

The follow *model* — who follows which thread, and why — is
backend-neutral and lives in
:mod:`crewlet.notifications.transports.chat_threads`.  What is Slack-specific
is only how a mention is written on the wire: Slack rewrites every mention
into a structured token (``<@U123>``, ``<!channel>``) before the message
reaches a webhook, so the grammar is a pair of regexes over that markup
rather than a literal ``@name`` match.
"""

from __future__ import annotations

import re

from crewlet.db.chat_thread_follows import ChatThreadFollowRepository
from crewlet.notifications.transports.chat_threads import (
    ChatThreadTracker,
    ThreadFollowReason,
)

#: The chat backend key Slack follow rows are stored under.
SLACK_BACKEND = "slack"

# Matches Slack user mentions like <@U12345> or <@W12345>
_USER_MENTION_RE = re.compile(r"<@([UW][A-Z0-9_]+)>")

# Matches Slack special mentions <!channel> and <!here>
_COLLECTIVE_RE = re.compile(r"<!(?:channel|here)(?:\|[^>]*)?>")


def detect_follow_trigger(text: str, bot_user_id: str) -> ThreadFollowReason | None:
    """Detect if message text contains a follow trigger for the agent.

    Returns the reason if a trigger is found, ``None`` otherwise.  A
    direct mention outranks a collective address: being named personally
    is a stronger signal than being in the room when someone shouted.
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


class SlackMentionGrammar:
    """:class:`~crewlet.notifications.transports.chat_threads.MentionGrammar`
    for Slack's structured mention markup."""

    def detect(self, text: str, self_identity: str) -> ThreadFollowReason | None:
        return detect_follow_trigger(text, self_identity)


def build_slack_thread_tracker(
    repo: ChatThreadFollowRepository,
) -> ChatThreadTracker:
    """A thread tracker bound to Slack's namespace and grammar."""
    return ChatThreadTracker(SLACK_BACKEND, repo, SlackMentionGrammar())


__all__ = [
    "SLACK_BACKEND",
    "SlackMentionGrammar",
    "build_slack_thread_tracker",
    "detect_follow_trigger",
]
