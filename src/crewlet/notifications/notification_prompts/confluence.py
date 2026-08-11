"""Confluence notification prompt — tool-agnostic wiki event descriptions."""

from __future__ import annotations

from typing import TYPE_CHECKING

from crewlet.notifications.notification_prompts.base import NotificationPrompt

if TYPE_CHECKING:
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.notifications.protocol import InboundNotification


class ConfluenceNotificationPrompt(NotificationPrompt):
    """Builds prompts for Confluence-originated events.

    Instructions describe *intent* (e.g. "review the updated page")
    rather than specific tool names.
    """

    def build(
        self,
        notification: InboundNotification,
        *,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        event_type = meta.get("event_type", "")
        page_title = meta.get("page_title", "")
        page_id = meta.get("page_id", "")
        space_key = meta.get("space_key", "")
        actor = meta.get("actor_name", "")
        timestamp = meta.get("timestamp", "")

        # ── Event Summary ──
        description = (
            f"A Confluence event has been routed to you.\n\n"
            f"**Title:** {notification.subject}\n"
        )

        # Event metadata
        meta_lines = []
        if event_type:
            meta_lines.append(f"- **Event:** {event_type}")
        if actor:
            meta_lines.append(f"- **Actor:** {actor}")
        if space_key:
            meta_lines.append(f"- **Space:** {space_key}")
        if page_id:
            meta_lines.append(f"- **Page ID:** {page_id}")
        if page_title:
            meta_lines.append(f"- **Page Title:** {page_title}")
        comment_id = meta.get("comment_id", "")
        if comment_id:
            meta_lines.append(f"- **Comment ID:** {comment_id}")
        if timestamp:
            meta_lines.append(f"- **Timestamp:** {timestamp}")
        routed_via = meta.get("routed_via", "")
        if routed_via:
            meta_lines.append(f"- **Routed via:** {routed_via}")

        if meta_lines:
            description += "\n## Event Metadata\n" + "\n".join(meta_lines) + "\n"

        if notification.body:
            description += f"\n**Details:**\n{notification.body}\n"

        # ── Get Full Context ──
        page_url = meta.get("page_url", "")
        if page_id:
            description += (
                f"\n## Get Full Context"
                f"\nUse your Confluence tools to fetch the full "
                f"content of page ID `{page_id}`"
            )
            if page_title:
                description += f" (**{page_title}**)"
            description += " before deciding on next steps.\n"
            if page_url:
                description += f"Page URL: {page_url}\n"
            if comment_id:
                description += (
                    f"The triggering comment has ID `{comment_id}`. "
                    f"Fetch the page comments to see the full "
                    f"discussion thread.\n"
                )

        # ── Lead-fallback framing ──
        # When the only reason this lead received the event is that
        # the page had no specific watcher or @mention, the lead is
        # the default owner -- but only by fallback.  Surface that so
        # the lead delegates rather than silently absorbing the work.
        description += self._why_routed_block(meta)

        # ── Triage & Action Guidance ──
        description += self._handling_guidance(meta)

        return description

    @staticmethod
    def _handling_guidance(meta: dict[str, str]) -> str:
        """The static "How to Handle This" block.

        The verbose-decline rule fires only for routings that
        represent a direct ask (``mention`` or an empty reason).
        Watchers receive events because they once interacted, not
        because someone is asking them now -- the rule is omitted
        to avoid noise.  Space-lead-fallback routings already get
        the dedicated ``Why You Received This`` block laying out
        delegate / act / escalate, so the rule is skipped there
        too.
        """
        routed_via = meta.get("routed_via", "")
        direct_ask = routed_via in ("", "mention")

        base = (
            "\n## How to Handle This"
            "\nSkip silently if this page or comment is unrelated to"
            " your work or team and you weren't directly @mentioned."
            " If relevant, read the change and decide whether your"
            " plans need adjustment; reply with substance to direct"
            " questions; otherwise observe silently."
            " Acknowledge-only comments are noise."
        )
        if not direct_ask:
            return base + "\n"
        return base + (
            "\n\nIf you were directly @mentioned but have decided not"
            " to act (out of scope, wrong owner, already covered):"
            " do NOT skip silently. Post a brief reply @mentioning"
            " the right owner or pointing at where the topic is being"
            " handled. If you can't identify the right owner,"
            " @mention your own manager (named in your identity"
            " prompt) so they can route it. Leaving a direct @mention"
            " unanswered looks like the message was lost.\n"
        )

    @staticmethod
    def _why_routed_block(meta: dict[str, str]) -> str:
        """Return a "Why You Received This" section when the lead got
        this event by routing fallback rather than personal involvement.

        Returned empty for ``watcher`` / ``mention`` routings -- those
        carry their own signal of why the recipient is involved.  The
        block is only added for ``space_lead`` (see
        ``ConfluenceTransport.handle_webhook``), where the only reason
        the lead received the event is that no specific watcher or
        @mention applied.
        """
        if meta.get("routed_via", "") != "space_lead":
            return ""
        space = meta.get("space_key", "") or meta.get("space_name", "")
        space_phrase = (
            f"the **{space}** Confluence space" if space else "this Confluence space"
        )
        return (
            "\n## Why You Received This"
            f"\nThis event was routed to you because you lead a unit"
            f" that owns {space_phrase}, and no specific watcher or"
            " @mention applied. You are the default owner only by"
            " fallback, not because the page is yours. Since"
            " space-lead fallback fires only when nobody else is"
            " involved, no one else is watching this page — if you"
            " walk away silently, it goes unattended."
            "\n\nDecide one of:"
            "\n- **Delegate** — @mention the right teammate in a"
            " comment on the page. They will be added as a watcher"
            " and pick up future updates on this page."
            "\n- **Act yourself** — if the page concerns your own work"
            " or needs a lead-level response, reply directly."
            "\n- **Escalate** — if the page is outside your team's"
            " scope or you can't identify the right reviewer,"
            " @mention your own manager (named in your identity"
            " prompt) in a comment so they are added as a watcher and"
            " can decide where the page belongs.\n"
        )

    def requires_recon(self, notification: InboundNotification) -> bool:
        """A Confluence webhook is a pointer: it names the page that
        changed, and ``build()`` emits a ``## Get Full Context`` block
        telling the agent to fetch the page content before acting.
        Same condition ``build()`` gates that block on -- a page id."""
        return bool(notification.metadata.get("page_id"))

    def conversation_key(self, metadata: dict[str, str], *, subject: str = "") -> str:
        """The page is the conversation -- comments and edits on one
        page coalesce together."""
        return metadata.get("page_id", "")
