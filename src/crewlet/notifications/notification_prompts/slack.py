"""Slack notification prompt — produces task descriptions for Slack events."""

from __future__ import annotations

from typing import TYPE_CHECKING

from crewlet.notifications.notification_prompts.base import NotificationPrompt

if TYPE_CHECKING:
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.notifications.protocol import InboundNotification


class SlackNotificationPrompt(NotificationPrompt):
    """Builds prompts for Slack-originated events.

    Keeps only per-event facts + a terse triage/mention hint. Tool
    mechanics (channel_id / thread_ts args, reaction timestamps)
    live in the tool schemas; agent-general behavioral guidelines
    live in the role's ``behavioral_guidelines``.
    """

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
        # no a2a_ask) rather than an opaque Slack ID.
        sender_label = self._party_label(handle_registry, "slack", meta.get("user", ""))
        if sender_label:
            user = f"{sender_label} — `{meta.get('user', '')}`"
        bot_user_id = meta.get("bot_user_id", "")
        recipient_handle = notification.recipient_handle or "your handle"

        parts: list[str] = [
            f"A Slack message was posted by **{user}**."
            f" Your handle is `{recipient_handle}`."
        ]
        if bot_user_id:
            parts.append(
                f"Your bot user ID is `{bot_user_id}`;"
                f" messages from `{bot_user_id}` in the thread are YOUR"
                " previous replies."
            )

        personal_ids: list[str] = []
        if bot_user_id:
            personal_ids.append(f"`<@{bot_user_id}>`")
        personal_ids.append(f"`{recipient_handle}`")
        personal_marker = " or ".join(personal_ids)
        parts.append(
            "\n## Triage — decide BEFORE replying"
            "\n"
            "\n**1. Find the addressee — who is the message asking to act?**"
            f"\n- **Personal:** the message names you — {personal_marker},"
            ' or your role-name (e.g. "PM", "the CTO") in plain text.'
            "\n- **A group you belong to:** `@channel` / `@here` /"
            " 'everyone' / 'team', or a role-group like \"engineers\","
            ' "leadership", "PMs".'
            "\n- **Someone else:** a specific other person/role the message"
            " is directing."
            "\n- **No one (informational):** announcements, FYIs, status"
            " updates, welcomes / kudos *about* someone."
            "\n"
            "\n**Distinguish addressee from subject.** Mentions can be the"
            " addressee or the subject — read the verb, not just the `@`."
            '\n- "Engineers, welcome <@newhire>" → addressee = engineers;'
            " <@newhire> is the subject (the new hire)."
            '\n- "<@PM> open a ticket for <@SWE>" → addressee = PM;'
            " <@SWE> is the subject (assignee mentioned for context)."
            '\n- "Thanks <@SWE> for the fix!" → no addressee; <@SWE> is'
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
            ' enough (e.g. "Not my area — try <@OTHER> who owns auth."'
            ' or "Already handled in <link>."). Group pings (rule 2)'
            " still allow silence when you have nothing substantive;"
            " this rule covers personal pings only — leaving a direct"
            " 1:1 ping unanswered looks like the message was lost."
        )

        if thread_ts:
            self_ref = f"`{bot_user_id}`" if bot_user_id else "your bot ID"
            parts.append(
                "\n## Thread context"
                "\nThis is a thread reply. Read the thread with your Slack"
                " tools before responding; focus on the triggering message,"
                " treat the rest as background."
                "\n"
                f"\n**Self-check before replying.** Messages from {self_ref}"
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
        if msg_ts:
            parts.append(
                f"**Message timestamp:** {msg_ts}"
                " (use for `slack_reactions_add`)."
                " Format mentions as `<@USER_ID>` with angle brackets,"
                " not `@U0ABC123` — Slack won't render the latter."
            )
        else:
            parts.append(
                "Format mentions as `<@USER_ID>` with angle brackets,"
                " not `@U0ABC123` — Slack won't render the latter."
            )

        return "\n".join(parts)

    def requires_recon(self, notification: InboundNotification) -> bool:
        """A thread reply is a pointer: ``build()`` tells the agent to
        read the thread before replying, because the triggering
        message is usually thin ("yes", "+1") and the thread is the
        context.  A top-level message / DM carries its own body, so
        it is not recon-requiring."""
        return bool((notification.metadata or {}).get("thread_ts"))

    def conversation_key(self, metadata: dict[str, str], *, subject: str = "") -> str:
        """The conversation: the DM channel for top-level DM messages,
        the thread otherwise.

        In a DM or group DM (``channel_type`` ``im`` / ``mpim``; a
        ``D``-prefixed channel id is recognised as a DM even when the
        event variant omits ``channel_type``, e.g. ``app_mention``) a
        human's consecutive TOP-LEVEL messages are one conversation —
        they key on the channel alone, so a typing burst coalesces into
        one digest turn (the feature's headline case).  A DM *thread
        reply* keeps its thread key: merging it with unrelated
        top-level pings would hand the turn a single merged metadata
        whose ``thread_ts`` points at only one of the two reply
        targets, steering the digest reply to the wrong place.  In
        shared channels two unrelated top-level asks must NOT merge, so
        the key stays thread-grained throughout: replies carry
        ``thread_ts`` (the root's ts) and a top-level message keys on
        its own ``ts`` so its later replies land in the same partition.
        Without a channel there is no conversation identity."""
        channel = metadata.get("channel", "")
        if not channel:
            return ""
        thread = metadata.get("thread_ts", "")
        is_dm = metadata.get("channel_type", "") in (
            "im",
            "mpim",
        ) or channel.startswith("D")
        if is_dm and not thread:
            return channel
        anchor = thread or metadata.get("ts", "")
        if anchor:
            return f"{channel}:{anchor}"
        return ""
