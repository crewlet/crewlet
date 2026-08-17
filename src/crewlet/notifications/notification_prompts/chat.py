"""Shared base for chat-backend notification prompts.

Slack and Mattermost deliver the same *kind* of event — a person said
something in a channel, a DM or a thread — and the hard part of the
prompt is the same for both: deciding whether the message was actually
addressed to this agent, and staying silent when it was not.  That triage
guidance is the bulk of the prompt and is backend-neutral, so it lives
here once.

What genuinely differs per backend is small and mechanical:

- how a mention is written (Slack rewrites them to ``<@U123>``;
  Mattermost keeps a literal ``@username``),
- which collective addresses exist (``@channel`` / ``@here``, plus
  ``@all`` on Mattermost),
- how a direct conversation is recognised in metadata.

Subclasses supply those through the hooks below.  Nothing here names a
tool: per ``docs/concepts/tool-capabilities.md`` the prompt describes the
capability and lets the LLM pick the matching tool out of its own
catalogue, because the deployed MCP server's tool names are not
knowable by the engine.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, ClassVar

from crewlet.notifications.notification_prompts.base import NotificationPrompt

if TYPE_CHECKING:
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.notifications.protocol import InboundNotification


class ChatNotificationPrompt(NotificationPrompt):
    """Triage + thread guidance shared by every chat backend."""

    #: Human-readable backend name, used in the prompt's prose.
    backend_label: ClassVar[str] = "chat"

    #: Metadata ``transport`` key this prompt is registered for.
    backend: ClassVar[str] = ""

    #: ``channel_type`` values that mean a direct conversation.
    dm_channel_types: ClassVar[frozenset[str]] = frozenset()

    #: Channel-id prefix that marks a DM, or ``""`` when ids are opaque.
    dm_channel_id_prefix: ClassVar[str] = ""

    #: The collective addresses this backend understands, rendered.
    collective_addresses: ClassVar[str] = "`@channel` / `@here`"

    # ----- per-backend hooks --------------------------------------------

    def self_reference(self, metadata: dict[str, str]) -> str:
        """How this agent is addressed on this backend, or ``""``.

        Rendered into the triage block so the agent recognises a mention
        of itself, and into the thread block so it recognises its own
        previous replies.
        """
        return ""

    def mention_format_hint(self, metadata: dict[str, str]) -> str:
        """One line telling the agent how to write a mention, or ``""``."""
        return ""

    def identity_note(self, metadata: dict[str, str]) -> str:
        """Optional line naming the agent's own id on this backend."""
        return ""

    # ----- shared rendering ----------------------------------------------

    def build(
        self,
        notification: InboundNotification,
        *,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata or {}
        channel_id = meta.get("channel", "")
        thread_ts = meta.get("thread_ts", "")
        msg_ts = meta.get("ts", "")
        # ``or`` chain (not a ``.get`` default): the transport always
        # writes the ``user`` metadata key, so an absent sender arrives
        # as ``""`` — a plain default would never fire and the prompt
        # would render "posted by ****" / a blank From line.
        user = notification.sender or meta.get("user") or "unknown"
        # Annotate known colleagues — especially human seats, so the
        # agent treats the counterparty as a person (async replies,
        # no a2a_ask) rather than an opaque platform ID.
        sender_label = self._party_label(
            handle_registry, self.backend, meta.get("user", "")
        )
        if sender_label:
            user = f"{sender_label} — `{meta.get('user', '')}`"
        recipient_handle = notification.recipient_handle or "your handle"

        parts: list[str] = [
            f"A {self.backend_label} message was posted by **{user}**."
            f" Your handle is `{recipient_handle}`."
        ]
        identity = self.identity_note(meta)
        if identity:
            parts.append(identity)

        personal_ids: list[str] = []
        self_ref = self.self_reference(meta)
        if self_ref:
            personal_ids.append(self_ref)
        personal_ids.append(f"`{recipient_handle}`")
        personal_marker = " or ".join(personal_ids)
        parts.append(self._triage_block(personal_marker))

        if thread_ts:
            parts.append(self._thread_block(self_ref))

        parts.append("")
        parts.append(f"**Message:** {notification.body or '(empty)'}")
        parts.append(f"**From:** {user}")
        if channel_id:
            parts.append(f"**Channel:** {channel_id}")
        if thread_ts:
            parts.append(f"**Thread:** {thread_ts} (existing thread)")
        elif msg_ts:
            parts.append(
                f"**Thread:** {msg_ts} (top-level message — reply as a thread)"
            )
        parts.extend(self._message_id_lines(meta))

        return "\n".join(parts)

    def _message_id_lines(self, metadata: dict[str, str]) -> list[str]:
        """The trailing per-message facts (id + mention formatting)."""
        lines: list[str] = []
        msg_ts = metadata.get("ts", "")
        hint = self.mention_format_hint(metadata)
        if msg_ts:
            lines.append(
                f"**Message id:** {msg_ts} — the reference for acting on"
                " *this* message rather than replying to it (for example"
                " reacting to it) with your chat tools."
            )
        if hint:
            lines.append(hint)
        return lines

    def _triage_block(self, personal_marker: str) -> str:
        """The addressee-triage guidance — identical on every backend."""
        return (
            "\n## Triage — decide BEFORE replying"
            "\n"
            "\n**1. Find the addressee — who is the message asking to act?**"
            f"\n- **Personal:** the message names you — {personal_marker},"
            ' or your role-name (e.g. "PM", "the CTO") in plain text.'
            "\n- **A group you belong to:** "
            f"{self.collective_addresses} /"
            " 'everyone' / 'team', or a role-group like \"engineers\","
            ' "leadership", "PMs".'
            "\n- **Someone else:** a specific other person/role the message"
            " is directing."
            "\n- **No one (informational):** announcements, FYIs, status"
            " updates, welcomes / kudos *about* someone."
            "\n"
            "\n**Distinguish addressee from subject.** Mentions can be the"
            " addressee or the subject — read the verb, not just the `@`."
            '\n- "Engineers, welcome @newhire" → addressee = engineers;'
            " @newhire is the subject (the new hire)."
            '\n- "@PM open a ticket for @SWE" → addressee = PM;'
            " @SWE is the subject (assignee mentioned for context)."
            '\n- "Thanks @SWE for the fix!" → no addressee; @SWE is'
            " the subject of kudos. Don't reply."
            "\n"
            "\n**2. Decide.**"
            "\n- Personal addressee → respond."
            "\n- In an addressed group → respond only if you have a specific,"
            " substantive contribution. For *action requests*, the narrowest-"
            'matching role should answer ("engineers" + auth question →'
            " the eng with auth ownership, not every eng). For *social*"
            " messages (welcomes, kudos), a brief reply from anyone in the"
            " group is fine."
            "\n- Addressee is someone else, or message is informational →"
            " stay silent unless you hold decision-changing information they"
            " cannot see."
            "\n"
            "\n**3. Default for non-addressed messages: silence beats"
            " noise.** When you weren't being asked (informational,"
            " passing reference, addressee is clearly someone else),"
            " stay silent."
            "\n"
            "\n**4. When the message names you SPECIFICALLY (not just"
            " as part of an addressed group) and you have decided not"
            " to act** (out of scope, already handled, deferring to"
            " someone else) → do NOT skip silently. Post a brief"
            " reply in the same thread/channel — one sentence is"
            ' enough (e.g. "Not my area — try someone who owns auth."'
            ' or "Already handled in <link>."). Group pings (rule 2)'
            " still allow silence when you have nothing substantive;"
            " this rule covers personal pings only — leaving a direct"
            " 1:1 ping unanswered looks like the message was lost."
        )

    def _thread_block(self, self_ref: str) -> str:
        """Thread-reply guidance — identical on every backend."""
        reference = self_ref or "your own account"
        return (
            "\n## Thread context"
            "\nThis is a thread reply. Read the thread with your chat"
            " tools before responding; focus on the triggering message,"
            " treat the rest as background."
            "\n"
            f"\n**Self-check before replying.** Messages from {reference}"
            " in this thread are YOUR previous replies."
            "\n- If you already gave your take on this question, do not"
            " repeat it."
            "\n- If the new message is acknowledgement / status /"
            " agreement and asks you nothing new, stay silent."
            "\n- If another agent is updating their own progress (not"
            " asking you), stay silent — let them finish."
            "\nEach fresh reply must answer a NEW question or make a"
            " NEW decision."
        )

    def requires_recon(self, notification: InboundNotification) -> bool:
        """A thread reply is a pointer: ``build()`` tells the agent to
        read the thread before replying, because the triggering
        message is usually thin ("yes", "+1") and the thread is the
        context.  A top-level message / DM carries its own body, so
        it is not recon-requiring."""
        return bool((notification.metadata or {}).get("thread_ts"))

    def is_direct(self, metadata: dict[str, str]) -> bool:
        """Whether this event happened in a direct conversation."""
        if metadata.get("channel_type", "") in self.dm_channel_types:
            return True
        prefix = self.dm_channel_id_prefix
        return bool(prefix) and metadata.get("channel", "").startswith(prefix)

    def conversation_key(self, metadata: dict[str, str], *, subject: str = "") -> str:
        """The conversation: the DM channel for top-level DM messages,
        the thread otherwise.

        In a DM or group DM a human's consecutive TOP-LEVEL messages are
        one conversation — they key on the channel alone, so a typing
        burst coalesces into one digest turn (the feature's headline
        case).  A DM *thread reply* keeps its thread key: merging it with
        unrelated top-level pings would hand the turn a single merged
        metadata whose thread pointer names only one of the two reply
        targets, steering the digest reply to the wrong place.  In shared
        channels two unrelated top-level asks must NOT merge, so the key
        stays thread-grained throughout: replies carry the root's id and
        a top-level message keys on its own id so its later replies land
        in the same partition.  Without a channel there is no
        conversation identity."""
        channel = metadata.get("channel", "")
        if not channel:
            return ""
        thread = metadata.get("thread_ts", "")
        if self.is_direct(metadata) and not thread:
            return channel
        anchor = thread or metadata.get("ts", "")
        if anchor:
            return f"{channel}:{anchor}"
        return ""


__all__ = ["ChatNotificationPrompt"]
