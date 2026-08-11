"""GitHub notification prompt — source-specific prompt formatting.

Gives ``review_requested`` events an actionable recon-and-review prompt.
All other GitHub events use the generic fallback.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from crewlet.notifications.notification_prompts.base import NotificationPrompt

if TYPE_CHECKING:
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.notifications.protocol import InboundNotification


class GitHubNotificationPrompt(NotificationPrompt):
    """Builds prompts for GitHub-originated events.

    ``review_requested`` gets a tailored prompt; everything else
    (noisy PR lifecycle events, issue comments, etc.) uses the
    generic fallback from the base class.
    """

    def build(
        self,
        notification: InboundNotification,
        *,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        event_type = notification.metadata.get("event_type", "")

        if event_type == "pull_request.review_requested":
            return self._review_requested(notification, handle_registry)

        return self._generic_description(notification)

    def requires_recon(self, notification: InboundNotification) -> bool:
        """``review_requested`` is a pointer: ``build()`` tells the
        agent to use its GitHub tools to read the diff and files
        before reviewing.  Every other GitHub event falls through to
        the generic fallback, whose body carries its own context --
        not recon-requiring."""
        return (
            notification.metadata.get("event_type", "")
            == "pull_request.review_requested"
        )

    def conversation_key(self, metadata: dict[str, str], *, subject: str = "") -> str:
        """The PR / issue is the conversation.  The webhook parser sets
        ``pr_number`` for pull-request events and ``issue_number`` for
        issue events, always alongside ``repo``."""
        repo = metadata.get("repo", "")
        number = metadata.get("pr_number", "") or metadata.get("issue_number", "")
        if repo and number:
            return f"{repo}#{number}"
        return ""

    @classmethod
    def _review_requested(
        cls,
        notification: InboundNotification,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        pr_number = meta.get("pr_number", "?")
        repo = meta.get("repo", "unknown")
        pr_url = f"https://github.com/{repo}/pull/{pr_number}"
        sender = notification.sender or "someone"
        # Annotate known colleagues (agents and human seats) so the
        # agent knows who is asking and how to reach them back.
        sender_label = cls._party_label(handle_registry, "github", sender)
        if sender_label:
            sender = f"{sender_label} — `{notification.sender}`"
        subject = notification.subject or "Pull Request"

        return (
            f"You have been requested to review a pull request.\n\n"
            f"**PR:** {repo}#{pr_number} — {subject}\n"
            f"**PR Link:** {pr_url}\n"
            f"**Requested by:** {sender}\n\n"
            f"**Description:**\n{notification.body or '(no description)'}\n\n"
            f"## What you should do\n\n"
            f"1. **Review the PR.** Use your GitHub tools to read "
            f"the diff and files. If the changes look correct, "
            f"approve the PR. If there are issues, leave review "
            f"comments.\n"
            f"2. **Notify the requester** in the original "
            f"conversation channel that the PR is ready for their "
            f"review or has been approved.\n"
            f"3. **If you have decided not to review** (out of "
            f"scope, wrong reviewer, conflict of interest): do NOT "
            f"skip silently. A review request is a direct ping — "
            f"leaving it open looks like the request was lost. "
            f"Either re-request review from the right person via "
            f"GitHub, or post a brief PR comment naming who should "
            f"review and (if you can) dismissing your own review "
            f"request so the requester knows to re-assign.\n"
        )
