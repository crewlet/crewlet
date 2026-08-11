"""ExecutionTracker — thin orchestration layer for external task backends.

When an external PM tool (Jira, Linear, GitHub Issues) is the source of
truth for tasks, the engine does not need its own task lifecycle. It only
needs to track which agent is working on which issue and the dependency
graph between issues — the orchestration concerns the PM tool doesn't
cover.

The ExecutionTracker is a passive data structure — it emits no events and
enforces no transitions. Events originate from webhooks via the
NotificationService. Agents interact with the PM tool via MCP tools.
"""

from __future__ import annotations

from crewlet._logging import get_logger

logger = get_logger("task.tracker")


class TrackedIssue:
    """Lightweight reference to an externally-managed issue.

    Stores only the orchestration metadata the engine needs.
    """

    __slots__ = (
        "issue_key",
        "agent_id",
        "blocked_by",
    )

    def __init__(
        self,
        issue_key: str,
        agent_id: str = "",
    ) -> None:
        self.issue_key = issue_key
        self.agent_id = agent_id
        self.blocked_by: list[str] = []


class ExecutionTracker:
    """Tracks agent ↔ issue mappings and orchestration metadata.

    Thin orchestration layer for external PM tool backends.
    It does **not** track task status, enforce transitions, or store
    task details — all of that lives in the PM tool.

    Usage::

        tracker = ExecutionTracker()

        # Jira "assigned" webhook arrives
        tracker.track("BACK-1234", agent_id="abc-123")

        # Agent is working — executor queries what they're on
        issues = tracker.get_issues("abc-123")  # ["BACK-1234"]

        # Jira "status→Done" webhook arrives
        tracker.untrack("BACK-1234")
    """

    def __init__(self) -> None:
        self._tracked: dict[str, TrackedIssue] = {}

    def track(
        self,
        issue_key: str,
        agent_id: str = "",
    ) -> TrackedIssue:
        """Start tracking an issue.

        Called when a webhook indicates an issue has been assigned
        to an agent, or when the engine needs to track a dependency.

        If the issue is already tracked (e.g. via :meth:`add_dependency`),
        the existing entry is updated in place so that dependency
        metadata is preserved.  ``agent_id`` is always updated.
        """
        existing = self._tracked.get(issue_key)
        if existing is not None:
            logger.debug(
                "tracked_issue_updating",
                issue_key=issue_key,
                old_agent_id=existing.agent_id,
                new_agent_id=agent_id,
            )
            existing.agent_id = agent_id
            logger.info("issue_tracking", issue_key=issue_key, agent_id=agent_id)
            return existing
        issue = TrackedIssue(issue_key=issue_key, agent_id=agent_id)
        self._tracked[issue_key] = issue
        logger.info("issue_tracking", issue_key=issue_key, agent_id=agent_id)
        return issue

    def untrack(self, issue_key: str) -> TrackedIssue | None:
        """Stop tracking an issue.

        Called when a webhook indicates the issue has been resolved.
        Returns the removed issue, or ``None`` if it wasn't tracked.
        """
        removed = self._tracked.pop(issue_key, None)
        if removed is not None:
            logger.info("issue_untracking", issue_key=issue_key)
        else:
            logger.debug(
                "issue_untrack_not_found",
                issue_key=issue_key,
            )
        return removed

    def get_issue(self, issue_key: str) -> TrackedIssue | None:
        """Get tracking info for an issue."""
        return self._tracked.get(issue_key)

    def get_agent(self, issue_key: str) -> str:
        """Get the agent ID assigned to an issue.

        Returns empty string if the issue is not tracked.
        """
        issue = self._tracked.get(issue_key)
        return issue.agent_id if issue else ""

    def get_issues(self, agent_id: str) -> list[str]:
        """Get all issue keys assigned to an agent."""
        return [
            issue.issue_key
            for issue in self._tracked.values()
            if issue.agent_id == agent_id
        ]

    def add_dependency(self, issue_key: str, blocked_by: str) -> None:
        """Record that *issue_key* is blocked by *blocked_by*.

        Creates a tracking entry for *issue_key* if one doesn't exist.
        """
        issue = self._tracked.get(issue_key)
        if issue is None:
            issue = self.track(issue_key)
        if blocked_by not in issue.blocked_by:
            issue.blocked_by.append(blocked_by)
            logger.debug(
                "dependency_added",
                issue_key=issue_key,
                blocked_by=blocked_by,
                all_blockers=issue.blocked_by,
            )

    def dependencies_met(self, issue_key: str) -> bool:
        """Check if all blocking issues have been resolved (untracked).

        An issue with no dependencies always returns ``True``.
        """
        issue = self._tracked.get(issue_key)
        if issue is None:
            return True
        if not issue.blocked_by:
            return True
        # A dependency is met when the blocking issue is no longer tracked
        # (i.e. it has been resolved and untracked via webhook).
        result = all(dep not in self._tracked for dep in issue.blocked_by)
        logger.debug(
            "dependencies_check",
            issue_key=issue_key,
            met=result,
            blocked_by=issue.blocked_by,
        )
        return result

    @property
    def tracked(self) -> dict[str, TrackedIssue]:
        """Read-only view of all tracked issues."""
        return dict(self._tracked)

    @property
    def tracked_count(self) -> int:
        """Number of currently tracked issues."""
        return len(self._tracked)
