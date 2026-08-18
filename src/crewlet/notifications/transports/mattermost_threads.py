"""Mattermost's mention grammar, plus its thread-tracker factory.

The follow *model* is backend-neutral and lives in
:mod:`crewlet.notifications.transports.chat_threads`.  What is
Mattermost-specific is how a mention is written: unlike Slack, which
rewrites mentions into ``<@U123>`` markup before delivery, Mattermost
stores them literally as ``@username`` in the message text.

The websocket event also carries a **server-computed** ``mentions``
list, but the two answer different questions and the transport uses each
for its own (see
:meth:`~crewlet.notifications.transports.mattermost.MattermostTransport._detect_follow`).
The list says *whether* this bot was a target — authoritatively, and
including group mentions, notification keywords and ``@all`` /
``@channel`` / ``@here`` resolved against real membership, none of which
a regex can see.  It cannot say *why*: the server rewrites the field per
connection, so a bare ``@channel`` and being named personally both
arrive as this bot's own id.  Only the text tells those apart, which is
what the grammar below is for — on every post, not just on the reconnect
backfill, where posts re-read over REST carry no mention list and the
grammar is left doing both jobs alone.
"""

from __future__ import annotations

import re

from crewlet.db.chat_thread_follows import ChatThreadFollowRepository
from crewlet.notifications.transports.chat_threads import (
    ChatThreadTracker,
    ThreadFollowReason,
)

#: The chat backend key Mattermost follow rows are stored under.
MATTERMOST_BACKEND = "mattermost"

#: Mattermost's collective addresses.  ``@all`` and ``@channel`` are
#: synonyms for everyone in the channel; ``@here`` narrows to online
#: members.
_COLLECTIVE_RE = re.compile(r"(?<![\w@])@(?:all|channel|here)\b", re.IGNORECASE)

#: A username mention.  Mattermost usernames are lowercase alphanumerics
#: plus ``.``, ``-`` and ``_``.  Trailing punctuation is excluded so
#: "thanks @agent-swe!" and "@agent-swe." both match the username itself
#: — a trailing ``.`` or ``-`` is valid *inside* a username but is far
#: more often sentence punctuation at the end of one.
_MENTION_TEMPLATE = r"(?<![\w@])@{username}(?![\w.-]*[\w])"


def detect_follow_trigger(text: str, username: str) -> ThreadFollowReason | None:
    """Detect a follow trigger for *username* in Mattermost message text.

    A direct mention outranks a collective address: being named
    personally is a stronger signal than being in the room when someone
    shouted.
    """
    if username:
        pattern = _MENTION_TEMPLATE.format(username=re.escape(username))
        if re.search(pattern, text, re.IGNORECASE):
            return ThreadFollowReason.MENTION

    if _COLLECTIVE_RE.search(text):
        return ThreadFollowReason.COLLECTIVE

    return None


class MattermostMentionGrammar:
    """:class:`~crewlet.notifications.transports.chat_threads.MentionGrammar`
    for Mattermost's literal ``@username`` mentions."""

    def detect(self, text: str, self_identity: str) -> ThreadFollowReason | None:
        return detect_follow_trigger(text, self_identity)


def build_mattermost_thread_tracker(
    repo: ChatThreadFollowRepository,
) -> ChatThreadTracker:
    """A thread tracker bound to Mattermost's namespace and grammar."""
    return ChatThreadTracker(MATTERMOST_BACKEND, repo, MattermostMentionGrammar())


__all__ = [
    "MATTERMOST_BACKEND",
    "MattermostMentionGrammar",
    "build_mattermost_thread_tracker",
    "detect_follow_trigger",
]
