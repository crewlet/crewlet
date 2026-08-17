"""Slack notification prompt — produces task descriptions for Slack events.

Only the Slack-shaped facts live here; the triage guidance, thread
guidance and conversation keying are shared with every other chat
backend in :mod:`.chat`.
"""

from __future__ import annotations

from typing import ClassVar

from crewlet.notifications.notification_prompts.chat import ChatNotificationPrompt


class SlackNotificationPrompt(ChatNotificationPrompt):
    """Builds prompts for Slack-originated events.

    Keeps only per-event facts + the shared triage/mention hint. Tool
    mechanics (channel_id / thread_ts args, reaction references) live in
    the tool schemas; agent-general behavioral guidelines live in the
    role's ``behavioral_guidelines``.
    """

    backend_label: ClassVar[str] = "Slack"
    backend: ClassVar[str] = "slack"
    dm_channel_types: ClassVar[frozenset[str]] = frozenset({"im", "mpim"})
    #: A ``D``-prefixed channel id is a DM even when the event variant
    #: omits ``channel_type`` (``app_mention`` does).
    dm_channel_id_prefix: ClassVar[str] = "D"
    collective_addresses: ClassVar[str] = "`@channel` / `@here`"

    def self_reference(self, metadata: dict[str, str]) -> str:
        bot_user_id = metadata.get("bot_user_id", "")
        return f"`<@{bot_user_id}>`" if bot_user_id else ""

    def identity_note(self, metadata: dict[str, str]) -> str:
        bot_user_id = metadata.get("bot_user_id", "")
        if not bot_user_id:
            return ""
        return (
            f"Your bot user ID is `{bot_user_id}`;"
            f" messages from `{bot_user_id}` in the thread are YOUR"
            " previous replies."
        )

    def mention_format_hint(self, metadata: dict[str, str]) -> str:
        return (
            "Format mentions as `<@USER_ID>` with angle brackets,"
            " not `@U0ABC123` — Slack won't render the latter."
        )
