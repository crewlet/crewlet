"""Tests for the sandbox lifecycle events.

The completion event must round-trip through the queue serialization
registry as its concrete subclass (typed fields survive), since it is
published to a topic and re-read by the engine.
"""

from __future__ import annotations

from crewlet.events.types import (
    AgentPhaseCompleted,
    SandboxClarificationRequested,
    SandboxRunCompleted,
    SandboxRunStarted,
)
from crewlet.queue.serialization import deserialize_event, serialize_event


def test_run_started_summary() -> None:
    e = SandboxRunStarted(source="Engineer", coding_agent="claude-code")
    assert e.type == "sandbox_run_started"
    assert "sandbox job" in e.summary


def test_run_completed_roundtrips_as_subclass() -> None:
    # A pure control signal — the outcome is read at collect time, so the
    # event carries only the identifiers needed to reconnect and claim.
    e = SandboxRunCompleted(
        source="Engineer",
        agent_id="a-1",
        agent_handle="eng",
        role="Engineer",
        turn_id="t-1",
        sandbox_id="sbx-1",
        coding_agent="claude-code",
    )
    restored = deserialize_event(serialize_event(e))
    assert isinstance(restored, SandboxRunCompleted)
    assert restored.turn_id == "t-1"
    assert restored.agent_handle == "eng"
    assert restored.sandbox_id == "sbx-1"
    assert restored.coding_agent == "claude-code"
    assert "completed" in restored.summary


def test_clarification_requested_fields() -> None:
    e = SandboxClarificationRequested(
        source="Engineer",
        turn_id="t-2",
        question="Which framework?",
        audience="team",
        conversation_key="slack:C1:123",
    )
    restored = deserialize_event(serialize_event(e))
    assert isinstance(restored, SandboxClarificationRequested)
    assert restored.audience == "team"
    assert restored.conversation_key == "slack:C1:123"
    assert "team" in restored.summary


def test_phase_completed_sandbox_badge_in_summary() -> None:
    native = AgentPhaseCompleted(phase="execute", model="m", source="Eng")
    assert "sandbox" not in native.summary
    assert native.backend == "native"

    sbx = AgentPhaseCompleted(
        phase="execute",
        model="m",
        source="Eng",
        backend="sandbox",
        coding_agent="opencode",
        sandbox_id="sbx-9",
        delivered_refs=["pr"],
        cost_usd=1.5,
    )
    assert "[sandbox:opencode]" in sbx.summary
    # sandbox fields survive a queue round-trip
    restored = deserialize_event(serialize_event(sbx))
    assert isinstance(restored, AgentPhaseCompleted)
    assert restored.backend == "sandbox"
    assert restored.coding_agent == "opencode"
    assert restored.delivered_refs == ["pr"]
