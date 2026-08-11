"""Tests for ExecutionTracker — thin orchestration layer."""

import pytest

from crewlet.task.tracker import ExecutionTracker


@pytest.fixture
def tracker():
    return ExecutionTracker()


class TestTrackUntrack:
    def test_track_and_get_agent(self, tracker: ExecutionTracker):
        tracker.track("BACK-1", agent_id="agent-1")
        assert tracker.get_agent("BACK-1") == "agent-1"

    def test_get_agent_unknown_issue(self, tracker: ExecutionTracker):
        assert tracker.get_agent("NOPE-1") == ""

    def test_get_issues(self, tracker: ExecutionTracker):
        tracker.track("BACK-1", agent_id="agent-1")
        tracker.track("BACK-2", agent_id="agent-1")
        tracker.track("BACK-3", agent_id="agent-2")
        assert sorted(tracker.get_issues("agent-1")) == ["BACK-1", "BACK-2"]
        assert tracker.get_issues("agent-2") == ["BACK-3"]
        assert tracker.get_issues("agent-3") == []

    def test_untrack_returns_issue(self, tracker: ExecutionTracker):
        tracker.track("BACK-1", agent_id="agent-1")
        removed = tracker.untrack("BACK-1")
        assert removed is not None
        assert removed.issue_key == "BACK-1"
        assert tracker.get_agent("BACK-1") == ""

    def test_untrack_unknown_returns_none(self, tracker: ExecutionTracker):
        assert tracker.untrack("NOPE-1") is None

    def test_tracked_count(self, tracker: ExecutionTracker):
        assert tracker.tracked_count == 0
        tracker.track("A-1")
        tracker.track("A-2")
        assert tracker.tracked_count == 2
        tracker.untrack("A-1")
        assert tracker.tracked_count == 1

    def test_tracked_property_is_copy(self, tracker: ExecutionTracker):
        tracker.track("A-1")
        snapshot = tracker.tracked
        tracker.track("A-2")
        assert "A-2" not in snapshot


class TestDependencies:
    def test_no_dependencies_met(self, tracker: ExecutionTracker):
        tracker.track("BACK-1")
        assert tracker.dependencies_met("BACK-1") is True

    def test_untracked_issue_met(self, tracker: ExecutionTracker):
        assert tracker.dependencies_met("NOPE") is True

    def test_dependency_not_met(self, tracker: ExecutionTracker):
        tracker.track("BACK-1")
        tracker.track("BACK-2")
        tracker.add_dependency("BACK-2", blocked_by="BACK-1")
        assert tracker.dependencies_met("BACK-2") is False

    def test_dependency_met_after_untrack(self, tracker: ExecutionTracker):
        tracker.track("BACK-1")
        tracker.track("BACK-2")
        tracker.add_dependency("BACK-2", blocked_by="BACK-1")
        tracker.untrack("BACK-1")
        assert tracker.dependencies_met("BACK-2") is True

    def test_multiple_dependencies(self, tracker: ExecutionTracker):
        tracker.track("A")
        tracker.track("B")
        tracker.track("C")
        tracker.add_dependency("C", blocked_by="A")
        tracker.add_dependency("C", blocked_by="B")
        assert tracker.dependencies_met("C") is False
        tracker.untrack("A")
        assert tracker.dependencies_met("C") is False
        tracker.untrack("B")
        assert tracker.dependencies_met("C") is True

    def test_add_dependency_creates_tracking_entry(self, tracker: ExecutionTracker):
        tracker.track("BACK-1")
        tracker.add_dependency("BACK-2", blocked_by="BACK-1")
        assert tracker.get_issue("BACK-2") is not None

    def test_duplicate_dependency_not_added(self, tracker: ExecutionTracker):
        tracker.track("A")
        tracker.track("B")
        tracker.add_dependency("B", blocked_by="A")
        tracker.add_dependency("B", blocked_by="A")
        issue = tracker.get_issue("B")
        assert issue is not None
        assert issue.blocked_by.count("A") == 1

    def test_track_after_add_dependency_preserves_deps(self, tracker: ExecutionTracker):
        """Deps added before track() must survive the track() call."""
        tracker.track("BLOCKER")
        tracker.add_dependency("CHILD", blocked_by="BLOCKER")
        # Now the assignment webhook arrives — track with agent_id
        tracker.track("CHILD", agent_id="agent-1")
        assert tracker.get_agent("CHILD") == "agent-1"
        assert tracker.dependencies_met("CHILD") is False
        tracker.untrack("BLOCKER")
        assert tracker.dependencies_met("CHILD") is True


class TestTrackUpdateInPlace:
    """Tests for track() updating existing entries."""

    def test_retrack_updates_agent_id(self, tracker: ExecutionTracker):
        tracker.track("X", agent_id="a1")
        tracker.track("X", agent_id="a2")
        assert tracker.get_agent("X") == "a2"

    def test_get_issues_empty_after_untrack_all(self, tracker: ExecutionTracker):
        tracker.track("A", agent_id="ag")
        tracker.track("B", agent_id="ag")
        tracker.untrack("A")
        tracker.untrack("B")
        assert tracker.get_issues("ag") == []
