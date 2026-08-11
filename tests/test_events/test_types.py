"""Tests for event type models."""

from datetime import UTC, datetime
from uuid import UUID

from crewlet.events.types import (
    AgentSpawned,
    AgentTerminated,
    AgentTurnCompleted,
    CoalescedMessage,
    Event,
    ExternalNotification,
    LLMUnavailable,
    MessageSent,
    OrgStarted,
    TaskCompleted,
    TaskCreated,
    TaskFailed,
    describe_trigger,
)


def test_base_event_defaults():
    event = Event(type="test")
    assert isinstance(event.id, UUID)
    assert event.type == "test"
    assert isinstance(event.timestamp, datetime)
    assert event.timestamp.tzinfo is not None
    assert event.source == ""
    assert event.payload == {}


def test_base_event_custom_fields():
    event = Event(type="custom", source="agent-1", payload={"key": "value"})
    assert event.source == "agent-1"
    assert event.payload == {"key": "value"}


def test_org_started():
    event = OrgStarted(org_name="Acme")
    assert event.type == "org_started"
    assert event.org_name == "Acme"


def test_agent_spawned():
    event = AgentSpawned(agent_id="a1", role="Engineer")
    assert event.type == "agent_spawned"
    assert event.agent_id == "a1"
    assert event.role == "Engineer"


def test_task_created():
    event = TaskCreated(task_id="t1", title="Build API", target_role="Engineer")
    assert event.type == "task_created"
    assert event.task_id == "t1"
    assert event.title == "Build API"
    assert event.target_role == "Engineer"


def test_task_completed():
    event = TaskCompleted(task_id="t1", agent_id="a1", result="Done")
    assert event.type == "task_completed"
    assert event.result == "Done"


def test_message_sent():
    event = MessageSent(channel="team-backend", sender="a1", content="Hello")
    assert event.type == "message_sent"
    assert event.channel == "team-backend"
    assert event.content == "Hello"


def test_event_serialization():
    event = TaskCreated(task_id="t1", title="Test", source="engine")
    data = event.model_dump()
    assert data["type"] == "task_created"
    assert data["task_id"] == "t1"
    restored = TaskCreated.model_validate(data)
    assert restored.task_id == event.task_id
    assert restored.id == event.id


def test_events_have_unique_ids():
    e1 = Event(type="a")
    e2 = Event(type="a")
    assert e1.id != e2.id


def test_event_timestamps_are_utc():
    event = Event(type="test")
    assert event.timestamp.tzinfo == UTC


def test_event_trace_context_empty_without_span():
    """Without an active OTel span, trace fields are empty."""
    event = Event(type="test")
    assert event.trace_id == ""
    assert event.span_id == ""
    assert event.parent_span_id == ""


def test_event_trace_context_from_otel_span():
    """Events created inside an OTel span inherit its trace/span IDs."""
    from crewlet.telemetry import tracer

    with tracer.start_as_current_span("test-span"):
        event = Event(type="test")
        assert len(event.trace_id) == 32
        assert len(event.span_id) == 16
        assert all(c in "0123456789abcdef" for c in event.trace_id)


def test_event_trace_context_custom():
    event = Event(
        type="test",
        trace_id="abcd1234abcd1234abcd1234abcd1234",
        span_id="1234abcd1234abcd",
        parent_span_id="eeee1111eeee1111",
    )
    assert event.trace_id == "abcd1234abcd1234abcd1234abcd1234"
    assert event.span_id == "1234abcd1234abcd"
    assert event.parent_span_id == "eeee1111eeee1111"


def test_event_trace_context_serialization():
    event = Event(type="test", trace_id="a" * 32, span_id="b" * 16)
    data = event.model_dump()
    assert data["trace_id"] == "a" * 32
    assert data["span_id"] == "b" * 16
    restored = Event.model_validate(data)
    assert restored.trace_id == event.trace_id


def test_events_in_same_span_share_trace():
    """Multiple events created in the same span share the same trace_id."""
    from crewlet.telemetry import tracer

    with tracer.start_as_current_span("test-span"):
        e1 = Event(type="a")
        e2 = Event(type="b")
        assert e1.trace_id == e2.trace_id
        assert e1.trace_id != ""


# --- Summary property tests ---


def test_base_event_summary_fallback():
    event = Event(type="some_custom_event")
    assert event.summary == "Some Custom Event"


def test_base_event_actor_fallback():
    event = Event(type="test", source="engine")
    assert event.actor == "engine"

    event2 = Event(type="test")
    assert event2.actor == "system"


def test_org_started_summary():
    event = OrgStarted(org_name="Acme")
    assert event.summary == "Organization 'Acme' started"


def test_agent_spawned_summary():
    event = AgentSpawned(role="Engineer", source="pool")
    assert event.summary == "Engineer joined the organization"
    assert event.actor == "Engineer"


def test_agent_terminated_summary():
    event = AgentTerminated(role="Dev", reason="shutdown")
    assert "Dev" in event.summary
    assert "shutdown" in event.summary


def test_task_created_summary():
    event = TaskCreated(source="PM", title="Build API", target_role="Engineer")
    assert "PM" in event.summary
    assert "Build API" in event.summary
    assert "Engineer" in event.summary


def test_task_completed_summary():
    event = TaskCompleted(source="Dev", task_id="T-42")
    assert "Dev" in event.summary
    assert "T-42" in event.summary
    assert "completed" in event.summary


def test_task_failed_summary_with_error():
    event = TaskFailed(source="Dev", task_id="T-42", error="timeout")
    assert "failed" in event.summary
    assert "timeout" in event.summary


def test_message_sent_summary():
    event = MessageSent(sender="PM", channel="#backend")
    assert "PM" in event.summary
    assert "#backend" in event.summary


def test_turn_completed_summary():
    event = AgentTurnCompleted(source="CTO", model="gpt-4o", total_tokens=500)
    assert "CTO" in event.summary
    assert "gpt-4o" in event.summary
    assert "500" in event.summary


def test_llm_unavailable_defaults():
    """``LLMUnavailable`` is the chain-exhausted signal.  Default
    values produce a sensible summary even when fields are unset."""
    event = LLMUnavailable()
    assert event.type == "llm_unavailable"
    assert event.agent_id == ""
    assert event.role == ""
    assert event.provider_chain == []
    assert event.attempt_count == 0
    assert event.last_error_kind == ""
    assert event.last_error == ""
    assert event.turn_id == ""


# --- Trigger descriptor tests (turn source) ---


def test_describe_trigger_none_is_empty():
    """No triggering event → empty dict the dashboard renders as 'no source'."""
    assert describe_trigger(None) == {}


def test_describe_trigger_compact_descriptor():
    """The descriptor carries the fields the dashboard renders as the
    LLM invocation's source: id, type, summary, actor, timestamp."""
    event = TaskCreated(source="PM", title="Build API", target_role="Engineer")
    desc = describe_trigger(event)
    assert desc["id"] == str(event.id)
    assert desc["type"] == "task_created"
    assert desc["summary"] == event.summary
    assert desc["actor"] == "PM"
    assert desc["timestamp"] == event.timestamp.isoformat()


def test_describe_trigger_non_notification_omits_integration():
    """Only notification triggers carry integration metadata; every other
    trigger leaves it absent so the dashboard uses its plain type label."""
    desc = describe_trigger(TaskCreated(source="PM", title="x"))
    assert "integration" not in desc
    assert "sender" not in desc
    assert "source_event_type" not in desc


def test_describe_trigger_notification_carries_integration():
    """An external-notification trigger names the originating integration,
    sender, and source event type so the dashboard can render a branded
    source badge instead of a generic 'external notification'."""
    event = ExternalNotification(
        notification_source="slack",
        source_event_type="message",
        sender="alice",
        subject="Need a hand",
    )
    desc = describe_trigger(event)
    assert desc["type"] == "external_notification"
    assert desc["integration"] == "slack"
    assert desc["sender"] == "alice"
    assert desc["source_event_type"] == "message"


def test_external_notification_summary_leads_with_sender_and_subject():
    """The summary is sender/subject-forward (the source is shown by the
    dashboard badge), so it never repeats the integration name."""
    event = ExternalNotification(
        notification_source="slack", sender="alice", subject="Need a hand"
    )
    assert event.summary == "Message from alice: Need a hand"
    assert "slack" not in event.summary


def test_external_notification_summary_without_sender_or_subject():
    bare = ExternalNotification(notification_source="jira")
    assert bare.summary == "Notification"
    subject_only = ExternalNotification(notification_source="jira", subject="ACME-1")
    assert subject_only.summary == "Notification: ACME-1"


def test_external_notification_summary_coalesced():
    """A coalesced digest counts its constituent messages."""
    event = ExternalNotification(
        notification_source="slack",
        sender="alice",
        subject="Thread",
        messages=[
            CoalescedMessage(sender="alice", body="one"),
            CoalescedMessage(sender="bob", body="two"),
        ],
    )
    assert event.summary == "2 messages from alice: Thread"


def test_describe_trigger_round_trips_into_phase_event():
    """A descriptor built from a trigger event slots onto the phase
    events the dashboard reads as the turn source."""
    from crewlet.events.types import AgentPhaseCompleted

    trigger = describe_trigger(TaskCreated(source="PM", title="x"))
    phase = AgentPhaseCompleted(phase="plan", trigger=trigger)
    assert phase.trigger["type"] == "task_created"
    # Survives serialization (the event-store / dashboard wire path).
    restored = AgentPhaseCompleted.model_validate(phase.model_dump())
    assert restored.trigger == trigger


def test_phase_events_default_trigger_empty():
    """The trigger field defaults to an empty dict on every carrier event."""
    from crewlet.events.types import (
        AgentPhaseCompleted,
        AgentPhaseStarted,
        AgentTurnProgress,
    )

    assert AgentPhaseStarted().trigger == {}
    assert AgentPhaseCompleted().trigger == {}
    assert AgentTurnProgress().trigger == {}
    assert AgentTurnCompleted().trigger == {}


def test_llm_unavailable_summary():
    event = LLMUnavailable(
        source="Engineer",
        agent_id="abc",
        role="Engineer",
        provider_chain=["openai", "anthropic"],
        attempt_count=2,
        last_error_kind="rate_limit",
        last_error="429 from anthropic",
    )
    assert event.type == "llm_unavailable"
    assert "Engineer" in event.summary
    assert "2 providers tried" in event.summary
