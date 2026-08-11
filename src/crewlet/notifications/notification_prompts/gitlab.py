"""GitLab notification prompt — source-specific prompt formatting.

Gives ``review_requested`` / ``assigned`` / ``mention`` events tailored,
actionable prompts. Everything else uses the generic fallback.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from crewlet.notifications.notification_prompts.base import NotificationPrompt

if TYPE_CHECKING:
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.notifications.protocol import InboundNotification

# Event types (set by ``parse_gitlab_webhook`` as ``metadata.event_type``)
# that are *pointers* — the agent must read the diff / thread / job logs
# before it has the real context, so the Plan-phase relevance prefetches
# skip their aux-LLM call (see ``NotificationPrompt.requires_recon``).
_RECON_EVENTS = frozenset(
    {
        "merge_request.review_requested",
        "merge_request.assigned",
        "issue.assigned",
        "pipeline.failed",
    }
)


class GitLabNotificationPrompt(NotificationPrompt):
    """Builds prompts for GitLab-originated events.

    ``merge_request.review_requested``, ``*.assigned`` and
    ``note.mention`` get tailored prompts; every other GitLab event
    (approvals, non-mention comments, MR merges/closes) uses the generic
    fallback from the base class.
    """

    def build(
        self,
        notification: InboundNotification,
        *,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        event_type = notification.metadata.get("event_type", "")

        if event_type == "merge_request.review_requested":
            return self._review_requested(notification, handle_registry)
        if event_type in ("merge_request.assigned", "issue.assigned"):
            return self._assigned(notification, handle_registry)
        if event_type.endswith(".mention"):
            # note.mention (comment) plus issue.mention /
            # merge_request.mention (description pings) — same directed
            # "you were named" shape.
            return self._mention(notification, handle_registry)

        return self._generic_description(notification)

    def requires_recon(self, notification: InboundNotification) -> bool:
        """Pointer events need a diff/thread/log fetch before acting.

        Review requests, assignments and failed pipelines name a thing
        to look at rather than carrying its full content; ``note.mention``
        and non-mention comments carry the comment body itself, so they
        fall through to the generic (body-is-context) path.
        """
        return notification.metadata.get("event_type", "") in _RECON_EVENTS

    def conversation_key(self, metadata: dict[str, str], *, subject: str = "") -> str:
        """The MR / issue is the conversation.

        The parser always sets ``project`` plus one of ``mr_iid`` /
        ``issue_iid``; events sharing a key coalesce into one digest turn.
        """
        project = metadata.get("project", "")
        mr = metadata.get("mr_iid", "")
        issue = metadata.get("issue_iid", "")
        if project and mr:
            return f"{project}!{mr}"
        if project and issue:
            return f"{project}#{issue}"
        return ""

    # ── Tailored builders ──

    @classmethod
    def _review_requested(
        cls,
        notification: InboundNotification,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        project = meta.get("project", "unknown")
        mr_iid = meta.get("mr_iid", "?")
        url = meta.get("url", "")
        sender = cls._sender_label(notification, handle_registry)
        subject = notification.subject or "Merge Request"
        link = f"\n**MR Link:** {url}" if url else ""

        return (
            f"You have been requested to review a merge request.\n\n"
            f"**MR:** {project}!{mr_iid} — {subject}{link}\n"
            f"**Requested by:** {sender}\n\n"
            f"**Description:**\n{notification.body or '(no description)'}\n\n"
            f"## What you should do\n\n"
            f"1. **Review the MR.** Use your GitLab tools to read the "
            f"diff and changed files. If the changes look correct, approve "
            f"the MR. If there are issues, leave review comments on the "
            f"diff.\n"
            f"2. **Notify the requester** in the original conversation "
            f"channel that the MR is ready for their review or has been "
            f"approved.\n"
            f"3. **If you have decided not to review** (out of scope, wrong "
            f"reviewer, conflict of interest): do NOT skip silently. A "
            f"review request is a direct ping — either re-request review "
            f"from the right person, or post a brief MR comment naming who "
            f"should review and removing yourself as a reviewer so the "
            f"requester knows to re-assign.\n"
        )

    @classmethod
    def _assigned(
        cls,
        notification: InboundNotification,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        event_type = meta.get("event_type", "")
        is_mr = event_type == "merge_request.assigned"
        kind = "merge request" if is_mr else "issue"
        project = meta.get("project", "unknown")
        iid = meta.get("mr_iid" if is_mr else "issue_iid", "?")
        sep = "!" if is_mr else "#"
        url = meta.get("url", "")
        sender = cls._sender_label(notification, handle_registry)
        subject = notification.subject or kind.title()
        link = f"\n**Link:** {url}" if url else ""
        do_step = (
            ", then push and update the MR.\n"
            if is_mr
            else ". Code changes go through your sandbox coding runtime, "
            "which opens an MR under your own GitLab identity.\n"
        )

        return (
            f"You have been assigned a {kind}.\n\n"
            f"**{kind.title()}:** {project}{sep}{iid} — {subject}{link}\n"
            f"**Assigned by:** {sender}\n\n"
            f"**Description:**\n{notification.body or '(no description)'}\n\n"
            f"## What you should do\n\n"
            f"1. **Read the {kind}.** Use your GitLab tools to read the "
            f"full {kind} and any discussion before starting.\n"
            f"2. **Do the work** the {kind} describes"
            f"{do_step}"
            f"3. **Report back** to the assigner in the originating "
            f"channel when done, or if you have decided not to act (out of "
            f"scope, wrong person) say so — don't leave a direct assignment "
            f"unanswered.\n"
        )

    @classmethod
    def _mention(
        cls,
        notification: InboundNotification,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        mr_iid = meta.get("mr_iid", "")
        issue_iid = meta.get("issue_iid", "")
        project = meta.get("project", "unknown")
        if mr_iid:
            ref = f"{project}!{mr_iid}"
        elif issue_iid:
            ref = f"{project}#{issue_iid}"
        else:
            ref = project
        url = meta.get("url", "")
        sender = cls._sender_label(notification, handle_registry)
        link = f"\n**Link:** {url}" if url else ""
        is_note = meta.get("event_type", "") == "note.mention"
        surface = "comment" if is_note else "issue/merge-request description"
        body_label = "Comment" if is_note else "Description"

        return (
            f"You were mentioned in a GitLab {surface}.\n\n"
            f"**On:** {ref}{link}\n"
            f"**By:** {sender}\n\n"
            f"**{body_label}:**\n{notification.body or '(no text)'}\n\n"
            f"## Evaluate Before Acting\n"
            f"1. **Were you actually asked to do something?** Read the "
            f"verb, not just whether your name appears. A passing mention "
            f'("cc @you") is not a request directed at you.\n'
            f"2. **If you were asked**, read the surrounding thread and the "
            f"MR/issue with your GitLab tools before replying, then respond "
            f"on the same thread.\n"
            f"3. **If you were not asked** (informational, FYI) → stay "
            f"silent. If you were asked but decline (out of scope, wrong "
            f"person) → say so briefly rather than going quiet.\n"
        )

    @staticmethod
    def _sender_label(
        notification: InboundNotification,
        handle_registry: HandleRegistry | None,
    ) -> str:
        """Render the actor as ``Label — \\`username\\``` when known."""
        sender = notification.sender or "someone"
        label = GitLabNotificationPrompt._party_label(
            handle_registry, "gitlab", notification.sender
        )
        if label:
            return f"{label} — `{notification.sender}`"
        return sender
