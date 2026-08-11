"""Plane notification prompt — tool-agnostic work-item / page descriptions."""

from __future__ import annotations

from typing import TYPE_CHECKING

from crewlet.notifications.notification_prompts.base import NotificationPrompt

if TYPE_CHECKING:
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.notifications.protocol import InboundNotification


class PlaneNotificationPrompt(NotificationPrompt):
    """Builds prompts for Plane-originated events.

    Dispatch keys on ``metadata.routed_via`` — the ``PlaneTransport``
    stamps the routing reason on every copy, so the prompt tailors to
    *why* the recipient was woken: assigned, @-mentioned, intake
    triage, lead fallback, or a page event.  Instructions describe
    intent ("assign the work item", "fetch the page") — never specific
    tool names.
    """

    def build(
        self,
        notification: InboundNotification,
        *,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        routed_via = meta.get("routed_via", "")

        if routed_via in ("assignee", "assignee_added"):
            text = self._assigned(notification, handle_registry)
        elif routed_via == "mention":
            text = self._mention(notification, handle_registry)
        elif routed_via == "intake_triage":
            text = self._intake(notification, handle_registry)
        elif routed_via == "page_project_lead":
            text = self._page(notification, handle_registry)
        elif routed_via == "project_lead_fallback":
            text = self._generic_description(notification) + self._why_routed_block(
                meta
            )
        else:
            # Subscriber fan-out and anything unrecognised: the generic
            # evaluate-and-skip framing is the right default for thread
            # activity the recipient merely watches.
            text = self._generic_description(notification)

        return text + self._recon_block(meta)

    def requires_recon(self, notification: InboundNotification) -> bool:
        """A Plane webhook is always a pointer: it names the work item /
        page that changed, and ``build()`` emits a ``## Get Full
        Context`` block on the same condition — an issue or page id."""
        return bool(
            notification.metadata.get("issue_id")
            or notification.metadata.get("page_id")
        )

    def conversation_key(self, metadata: dict[str, str], *, subject: str = "") -> str:
        """The work item (or page) is the conversation.

        Keyed on the entity UUID, not the human ``ENG-42`` key: the
        UUID rides every issue AND comment payload, while
        ``sequence_id`` (and the id→identifier cache behind the
        project prefix) does not — keying on the display key would
        split one work item's comments and field updates into two
        coalescing partitions.  ``work_item_key`` stays display-only.
        """
        return metadata.get("issue_id", "") or metadata.get("page_id", "")

    def digest_entry_body(self, event_type: str, body: str) -> str:
        """Comments keep their body; description snapshots collapse.

        Jira parity: on ``issue.created``/``issue.updated`` the body is
        the (now stale) description — in a coalesced digest only the
        latest state matters, and it is rendered in full below the
        digest, so intermediate copies collapse to their event lead.
        """
        return body if event_type.startswith("issue_comment") else ""

    # ── tailored builders ──

    @classmethod
    def _assigned(
        cls,
        notification: InboundNotification,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        event_type = meta.get("event_type", "")
        routed_via = meta.get("routed_via", "")
        ref = cls._work_item_ref(meta)
        sender = cls._sender_label(notification, handle_registry)

        if routed_via == "assignee_added":
            lead = "You have been assigned a Plane work item."
        elif event_type == "issue.created":
            lead = "A new Plane work item was created with you as an assignee."
        elif event_type == "issue.deleted":
            lead = "A Plane work item you were assigned to was deleted."
        else:
            lead = "A Plane work item you are assigned to was updated."

        text = (
            f"{lead}\n\n"
            f"**Work item:** {ref} — {notification.subject}\n"
            f"**By:** {sender}\n"
        )
        if notification.body:
            text += f"\n**Description:**\n{notification.body}\n"
        if event_type == "issue.deleted":
            text += (
                "\n## How to Handle This"
                "\nThe work item no longer exists — stop any in-flight"
                " work on it. If you had progress worth keeping, note it"
                " where your team tracks such things; otherwise no"
                " action is needed.\n"
            )
        else:
            text += (
                "\n## How to Handle This"
                "\n1. **Read the work item** in full before starting —"
                " state, priority, description, and existing comments.\n"
                "2. **Do the work** it describes: move it to an active"
                " state, and post ONE substantive completion summary"
                ' when done. Avoid running commentary ("starting work",'
                ' "still working") — those are noise.\n'
                "3. **If you have decided not to act** (out of scope,"
                " wrong owner, already handled): do NOT skip silently."
                " Reassign to the right teammate (use `lookup_colleague`"
                " to find them) or comment naming who should own it —"
                " leaving a direct assignment unanswered looks like the"
                " message was lost.\n"
            )
        return text

    @classmethod
    def _mention(
        cls,
        notification: InboundNotification,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        ref = cls._work_item_ref(meta)
        sender = cls._sender_label(notification, handle_registry)

        return (
            f"You were @-mentioned in a comment on a Plane work item.\n\n"
            f"**Work item:** {ref} — {notification.subject}\n"
            f"**By:** {sender}\n\n"
            f"**Comment:**\n{notification.body or '(no text)'}\n\n"
            f"## Evaluate Before Acting\n"
            f"1. **Were you actually asked to do something?** Read the"
            f" verb, not just whether your name appears. A passing"
            f' mention ("cc @you") is not a request directed at you.\n'
            f"2. **If you were asked**, read the work item and the full"
            f" comment thread before replying, then respond with"
            f" substance on the same thread.\n"
            f"3. **If you were not asked** (informational, FYI) → stay"
            f" silent. If you were asked but decline (out of scope,"
            f" wrong person) → say so briefly rather than going quiet.\n"
        )

    @classmethod
    def _intake(
        cls,
        notification: InboundNotification,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        sender = cls._sender_label(notification, handle_registry)
        text = (
            f"A Plane intake work item needs triage.\n\n"
            f"**Title:** {notification.subject}\n"
            f"**Submitted by:** {sender}\n\n"
            f"Intake is Plane's unassigned-inbound surface: the request"
            f" sits outside the normal backlog until someone accepts,"
            f" declines, or assigns it.\n"
        )
        return text + cls._why_routed_block(meta)

    @classmethod
    def _page(
        cls,
        notification: InboundNotification,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        event_type = meta.get("event_type", "")
        project = meta.get("project", "")
        sender = cls._sender_label(notification, handle_registry)

        if event_type == "page.created":
            lead = "A page was created in your team's Plane project."
        elif event_type == "page.deleted":
            lead = "A page was deleted from your team's Plane project."
        else:
            lead = "A page in your team's Plane project was updated."

        return (
            f"{lead}\n\n"
            f"**Page:** {notification.subject}\n"
            f"**Project:** {project}\n"
            f"**By:** {sender}\n\n"
            f"You received this as the lead of the unit that owns the"
            f" project — Plane pages have no per-page watchers, so page"
            f" activity routes to you by default.\n\n"
            f"## How to Handle This\n"
            f"Skip silently if the page is unrelated to your team's"
            f" work. If relevant, read the change and decide whether"
            f" your plans need adjustment, or flag it to the teammate"
            f" it concerns. Acknowledge-only responses are noise.\n"
        )

    # ── shared blocks ──

    @staticmethod
    def _why_routed_block(meta: dict[str, str]) -> str:
        """Return a "Why You Received This" section for lead routings.

        Rendered only for ``project_lead_fallback`` and
        ``intake_triage`` — the two routings where the recipient is
        involved purely as the owning unit's lead, so the prompt must
        lay out the delegate / take-it / escalate decision explicitly.
        Directed routings (assignee, mention) carry their own signal.
        """
        routed_via = meta.get("routed_via", "")
        if routed_via not in ("project_lead_fallback", "intake_triage"):
            return ""
        project = meta.get("project", "")
        project_phrase = (
            f"the **{project}** Plane project" if project else "this Plane project"
        )
        if routed_via == "intake_triage":
            reason = (
                f"\nThis intake item was routed to you because you lead"
                f" the team that owns {project_phrase} — intake requests"
                f" have no owner until someone triages them, so they"
                f" reach the project lead by default. If you walk away"
                f" silently, the request goes nowhere."
            )
        else:
            reason = (
                f"\nThis event was routed to you because you lead the"
                f" team that owns {project_phrase}, and the work item"
                f" has no assignee — you are the default owner only by"
                f" fallback, not because the work is yours. Since"
                f" lead-fallback fires only when nobody else is"
                f" involved, no one else is watching this item — if you"
                f" walk away silently, it goes nowhere."
            )
        return (
            "\n## Why You Received This" + reason + "\n\nDecide one of:"
            "\n- **Delegate** — assign the work item to the right"
            " teammate. Use `lookup_colleague` to find them on your"
            " team and resolve their Plane user ID, then set the"
            " assignee. Future updates on this item will route to"
            " them, not back to you."
            "\n- **Take it yourself** — only if the work clearly falls"
            " on you. Assign the work item to yourself so the routing"
            " reflects reality from now on."
            "\n- **Escalate** — if the item is out of scope or you"
            " can't identify the right owner, hand it off to your own"
            " manager (named in your identity prompt): comment on the"
            " work item mentioning them, or reassign it to them."
            " Either keeps the trail on the item so the work isn't"
            " lost.\n"
        )

    @staticmethod
    def _recon_block(meta: dict[str, str]) -> str:
        """The ``## Get Full Context`` pointer block.

        Appended by every builder when the event names a work item or
        page — the webhook is a pointer, and the agent must fetch the
        entity before acting.  Same condition ``requires_recon`` keys
        on.
        """
        issue_id = meta.get("issue_id", "")
        page_id = meta.get("page_id", "")
        if not issue_id and not page_id:
            return ""
        url = meta.get("url", "")
        if issue_id:
            work_item_key = meta.get("work_item_key", "")
            ref = f"**{work_item_key}**" if work_item_key else f"work item `{issue_id}`"
            text = (
                f"\n## Get Full Context"
                f"\nUse your Plane tools to fetch the full details for"
                f" {ref} (state, priority, assignees, description,"
                f" comments, linked items) before deciding on next"
                f" steps. Do not act on partial information.\n"
            )
        else:
            text = (
                f"\n## Get Full Context"
                f"\nUse your Plane tools to fetch the full content of"
                f" page `{page_id}` before deciding on next steps.\n"
            )
        if url:
            text += f"Link: {url}\n"
        return text

    @staticmethod
    def _work_item_ref(meta: dict[str, str]) -> str:
        """Display reference for the work item — ``ENG-42`` when the
        key resolved, the UUID otherwise."""
        return meta.get("work_item_key", "") or meta.get("issue_id", "") or "?"

    @staticmethod
    def _sender_label(
        notification: InboundNotification,
        handle_registry: HandleRegistry | None,
    ) -> str:
        """Render the trigger actor as a colleague label.

        Falls back through the registry party label (rewrites the raw
        Plane UUID into ``Role (handle)`` / ``Name (handle, human
        colleague)``) → the payload display name → the raw sender.
        """
        meta = notification.metadata
        actor_id = meta.get("actor_account_id", "")
        label = PlaneNotificationPrompt._party_label(handle_registry, "plane", actor_id)
        if label:
            return label
        return meta.get("actor_name", "") or notification.sender or "someone"
