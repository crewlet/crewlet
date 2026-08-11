"""Tests for subject-wildcard topic pattern matching."""

from __future__ import annotations

import pytest

from crewlet.queue.matching import topic_matches


@pytest.mark.parametrize(
    "pattern,topic,expected",
    [
        # Exact match
        ("crewlet.events.task_created", "crewlet.events.task_created", True),
        ("crewlet.events.task_created", "crewlet.events.task_failed", False),
        # Single-segment wildcard
        ("crewlet.events.*", "crewlet.events.task_created", True),
        ("crewlet.events.*", "crewlet.events.task.created", False),
        ("crewlet.*.task_created", "crewlet.events.task_created", True),
        # Trailing-segments wildcard
        ("crewlet.events.>", "crewlet.events.task_created", True),
        ("crewlet.events.>", "crewlet.events.deep.nested.topic", True),
        ("crewlet.events.>", "crewlet.events", False),
        (">", "crewlet.events.task_created", True),
        # Mixed
        ("crewlet.*.>", "crewlet.events.task_created", True),
        ("crewlet.*.>", "crewlet.events.task.created", True),
        ("crewlet.*.>", "crewlet", False),
        # Different prefix
        ("crewlet.events.>", "crewlet.agent.foo.inbox", False),
        ("crewlet.notifications.inbound", "crewlet.events.task_created", False),
    ],
)
def test_topic_matches(pattern: str, topic: str, expected: bool) -> None:
    assert topic_matches(pattern, topic) is expected
