"""Mattermost notification prompt.

Only the Mattermost-shaped facts live here; the triage guidance, thread
guidance and conversation keying are shared with every other chat
backend in :mod:`.chat`.

Two differences from Slack are worth the agent knowing:

- Mentions are written **literally** (``@agent-swe``), not as an id in
  markup — so the format hint is the inverse of Slack's.
- ``@all`` is a third collective address alongside ``@channel`` and
  ``@here``.
"""

from __future__ import annotations

from typing import ClassVar

from crewlet.mattermost.client import DIRECT_CHANNEL_TYPES
from crewlet.notifications.notification_prompts.chat import ChatNotificationPrompt


class MattermostNotificationPrompt(ChatNotificationPrompt):
    """Builds prompts for Mattermost-originated events."""

    backend_label: ClassVar[str] = "Mattermost"
    backend: ClassVar[str] = "mattermost"
    dm_channel_types: ClassVar[frozenset[str]] = DIRECT_CHANNEL_TYPES
    #: Empty deliberately — Mattermost channel ids are opaque 26-char
    #: strings, so a prefix test would misclassify public channels.
    #: ``channel_type`` is server-stamped and always present.
    dm_channel_id_prefix: ClassVar[str] = ""
    collective_addresses: ClassVar[str] = "`@all` / `@channel` / `@here`"

    def self_reference(self, metadata: dict[str, str]) -> str:
        username = metadata.get("bot_username", "")
        return f"`@{username}`" if username else ""

    def identity_note(self, metadata: dict[str, str]) -> str:
        user_id = metadata.get("bot_user_id", "")
        username = metadata.get("bot_username", "")
        if not user_id:
            return ""
        addressed_as = f" You are addressed as `@{username}`." if username else ""
        return (
            f"Your Mattermost user ID is `{user_id}`;"
            f" messages from `{user_id}` in the thread are YOUR"
            f" previous replies.{addressed_as}"
        )

    def mention_format_hint(self, metadata: dict[str, str]) -> str:
        return (
            "Write mentions as a plain `@username` (Mattermost resolves"
            " the name itself) — not as a user ID, and not wrapped in"
            " angle brackets."
        )
