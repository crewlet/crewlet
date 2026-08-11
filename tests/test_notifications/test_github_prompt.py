"""Tests for GitHubNotificationPrompt."""

from __future__ import annotations

from crewlet.notifications.notification_prompts import build_notification_prompt
from crewlet.notifications.notification_prompts.github import GitHubNotificationPrompt
from crewlet.notifications.protocol import InboundNotification


class TestGitHubPromptReviewRequested:
    """review_requested gets an actionable prompt, not a generic one."""

    def _make_notification(self, **meta_overrides: str) -> InboundNotification:
        metadata: dict[str, str] = {
            "event_type": "pull_request.review_requested",
            "pr_number": "42",
            "repo": "org/repo",
            "github_login": "agent-swe-user",
        }
        metadata.update(meta_overrides)
        return InboundNotification(
            source="github",
            source_event_type="pull_request.review_requested",
            recipient_email="swe@test.com",
            sender="copilot-agent",
            subject="Add hello world",
            body="Adds main.py with hello world",
            metadata=metadata,
        )

    def test_review_requested_contains_action_instructions(self):
        notification = self._make_notification()
        result = GitHubNotificationPrompt().build(notification)

        assert "requested to review" in result
        assert "Review the PR" in result
        assert "Notify the requester" in result
        assert "query_knowledge" not in result
        # No Copilot-delegation recall step appears (no steering).
        assert "Personal memory" not in result
        assert "Copilot" not in result

    def test_review_requested_contains_pr_info(self):
        notification = self._make_notification()
        result = GitHubNotificationPrompt().build(notification)

        assert "org/repo#42" in result
        assert "https://github.com/org/repo/pull/42" in result
        assert "Add hello world" in result
        assert "copilot-agent" in result

    def test_review_requested_no_generic_evaluate_section(self):
        """review_requested should NOT have the generic evaluate prompt."""
        notification = self._make_notification()
        result = GitHubNotificationPrompt().build(notification)

        assert "Evaluate Before Acting" not in result

    def test_review_requested_via_registry(self):
        """build_notification_prompt dispatches github to GitHubNotificationPrompt."""
        notification = self._make_notification()
        result = build_notification_prompt(notification)

        assert "requested to review" in result

    def test_review_requested_has_verbose_decline_guidance(self):
        """A review request is a direct ping.  When the
        agent decides not to review (out of scope / wrong reviewer /
        conflict of interest), the prompt must require a brief
        comment or re-request rather than letting the open review
        request silently sit in the requester's UI."""
        result = GitHubNotificationPrompt().build(self._make_notification())
        # Decline branch -- the new fourth instruction step.
        assert "decided not to review" in result
        # Unified anchor across all four prompts.
        assert "do NOT skip silently" in result
        # The actionable path -- re-request or comment, not just
        # 'consider replying'.
        assert "re-request review" in result


class TestGitHubPromptGenericFallback:
    """All non-review_requested events use the generic fallback."""

    def _make_pr_notification(self, event_type: str) -> InboundNotification:
        return InboundNotification(
            source="github",
            source_event_type=event_type,
            sender="copilot-agent",
            subject="Add feature",
            body="PR description",
            metadata={
                "event_type": event_type,
                "pr_number": "1",
                "repo": "org/repo",
            },
        )

    def test_pr_edited_uses_generic(self):
        result = GitHubNotificationPrompt().build(
            self._make_pr_notification("pull_request.edited")
        )
        assert "Evaluate Before Acting" in result

    def test_pr_synchronize_uses_generic(self):
        result = GitHubNotificationPrompt().build(
            self._make_pr_notification("pull_request.synchronize")
        )
        assert "Evaluate Before Acting" in result

    def test_pr_opened_uses_generic(self):
        result = GitHubNotificationPrompt().build(
            self._make_pr_notification("pull_request.opened")
        )
        assert "Evaluate Before Acting" in result

    def test_pr_assigned_uses_generic(self):
        result = GitHubNotificationPrompt().build(
            self._make_pr_notification("pull_request.assigned")
        )
        assert "Evaluate Before Acting" in result

    def test_issue_comment_uses_generic(self):
        notification = InboundNotification(
            source="github",
            source_event_type="issue_comment.created",
            sender="reviewer",
            subject="Feature request",
            body="What about edge cases?",
            metadata={
                "event_type": "issue_comment.created",
                "issue_number": "5",
                "repo": "org/repo",
            },
        )
        result = GitHubNotificationPrompt().build(notification)

        assert "Evaluate Before Acting" in result
        assert "What about edge cases?" in result

    def test_issues_assigned_uses_generic(self):
        notification = InboundNotification(
            source="github",
            source_event_type="issues.assigned",
            sender="pm",
            subject="Fix bug",
            body="It's broken",
            metadata={
                "event_type": "issues.assigned",
                "issue_number": "99",
                "repo": "org/repo",
            },
        )
        result = GitHubNotificationPrompt().build(notification)

        assert "Evaluate Before Acting" in result


class TestGitHubRequiresRecon:
    """``review_requested`` is a pointer (the agent must pull the diff);
    every other GitHub event uses the generic fallback whose body
    carries its own context."""

    def test_requires_recon_true_for_review_requested(self) -> None:
        prompt = GitHubNotificationPrompt()
        notification = InboundNotification(
            source="github",
            source_event_type="pull_request.review_requested",
            sender="copilot-agent",
            subject="Add hello world",
            body="Adds main.py",
            metadata={"event_type": "pull_request.review_requested"},
        )
        assert prompt.requires_recon(notification) is True

    def test_requires_recon_false_for_generic_events(self) -> None:
        prompt = GitHubNotificationPrompt()
        notification = InboundNotification(
            source="github",
            source_event_type="issues.assigned",
            sender="someone",
            subject="An issue",
            body="body",
            metadata={"event_type": "issues.assigned"},
        )
        assert prompt.requires_recon(notification) is False
