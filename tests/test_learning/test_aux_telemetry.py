"""Tests for the auxiliary-LLM telemetry wrapper.

Each learning worker that calls an aux LLM (PersistDecider,
CounterpartyProfiler, SkillSynthesizer, SkillRefiner,
PromotionSynthesizer, ``summarize_episodes``) routes through
``complete_with_telemetry``.  The wrapper must emit an
``AgentPhaseCompleted`` event with ``phase="auxiliary"`` and the
worker name so the dashboard's per-agent "LLM Invocations" view
shows the call alongside the main Plan / Execute / Review phases.
"""

from __future__ import annotations

import contextlib
from typing import Any

from crewlet.events.types import AgentPhaseCompleted
from crewlet.learning._aux_telemetry import complete_with_telemetry
from crewlet.providers.llm.protocol import Completion, Message


class _ScriptedProvider:
    model = "aux-cheap"

    def __init__(self, *, content: str, input_tokens: int, output_tokens: int) -> None:
        self._content = content
        self._input = input_tokens
        self._output = output_tokens
        self.calls: list[dict[str, Any]] = []

    async def complete(self, messages, **kwargs):
        self.calls.append({"messages": messages, **kwargs})
        return Completion(
            content=self._content,
            input_tokens=self._input,
            output_tokens=self._output,
        )


class _QueueStub:
    def __init__(self) -> None:
        self.published: list[tuple[str, Any]] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


async def test_aux_telemetry_publishes_phase_event_with_worker_name() -> None:
    queue = _QueueStub()
    provider = _ScriptedProvider(
        content="patched fact", input_tokens=100, output_tokens=20
    )

    completion = await complete_with_telemetry(
        provider=provider,  # type: ignore[arg-type]
        messages=[
            Message(role="system", content="You are a tester."),
            Message(role="user", content="Observe the subject."),
        ],
        worker="counterparty_profiler",
        role_name="Engineer",
        provider_key="default",
        event_queue=queue,
        agent_id="alice-id",
        turn_id="t1",
        temperature=0.2,
    )

    assert completion.content == "patched fact"
    # Provider received the temperature kwarg unchanged.
    assert provider.calls[0]["temperature"] == 0.2
    # One event went to the queue, on the AgentPhaseCompleted topic.
    assert len(queue.published) == 1
    topic, event = queue.published[0]
    assert topic == "crewlet.events.agent_phase_completed"
    assert isinstance(event, AgentPhaseCompleted)
    assert event.phase == "auxiliary"
    assert event.worker == "counterparty_profiler"
    assert event.agent_id == "alice-id"
    assert event.role == "Engineer"
    assert event.turn_id == "t1"
    assert event.model == "aux-cheap"
    assert event.input_tokens == 100
    assert event.output_tokens == 20
    assert event.total_tokens == 120
    # System / user prompts captured for the dashboard drill-down.
    assert "tester" in event.system_prompt
    assert "Observe the subject" in event.user_prompt
    assert event.response == "patched fact"


async def test_aux_telemetry_skips_publish_without_event_queue() -> None:
    """``event_queue=None`` means in-memory / test mode — the LLM call
    still runs but no event fires.  Workers boot this way before the
    engine wires the real queue."""
    provider = _ScriptedProvider(content="ok", input_tokens=1, output_tokens=1)

    completion = await complete_with_telemetry(
        provider=provider,  # type: ignore[arg-type]
        messages=[Message(role="system", content="x")],
        worker="persist_decider",
        role_name="Engineer",
        provider_key="default",
        event_queue=None,
    )
    assert completion.content == "ok"


async def test_aux_telemetry_skips_publish_when_provider_raises() -> None:
    """When the provider call itself raises, no event publishes — a
    half-populated event would mislead the dashboard."""

    class _BoomProvider:
        model = "aux"

        async def complete(self, messages, **_):
            raise RuntimeError("rate limit")

    queue = _QueueStub()
    with contextlib.suppress(RuntimeError):
        await complete_with_telemetry(
            provider=_BoomProvider(),  # type: ignore[arg-type]
            messages=[Message(role="system", content="x")],
            worker="skill_synthesizer",
            role_name="Engineer",
            provider_key="default",
            event_queue=queue,
        )
    assert queue.published == []


class _ReasoningProvider:
    """Returns a completion with reasoning_content + visible content
    so we can verify the dashboard's <think>...</think> wrapping."""

    model = "aux-thinking"

    def __init__(self, *, content: str, reasoning: str) -> None:
        self._content = content
        self._reasoning = reasoning

    async def complete(self, messages, **_):
        return Completion(
            content=self._content,
            reasoning_content=self._reasoning,
            input_tokens=50,
            output_tokens=200,
        )


async def test_aux_telemetry_wraps_reasoning_in_think_tags() -> None:
    """Reasoning text from extended-thinking models must reach the
    dashboard wrapped in ``<think>...</think>`` tags so the existing
    renderer surfaces it inline -- mirrors
    ``llm_loop.assistant_text_with_reasoning``."""
    queue = _QueueStub()
    provider = _ReasoningProvider(
        content='{"kind": "NOOP"}',
        reasoning="Let me think... the turn was about backend reviews.",
    )
    await complete_with_telemetry(
        provider=provider,  # type: ignore[arg-type]
        messages=[
            Message(role="system", content="sys"),
            Message(role="user", content="usr"),
        ],
        worker="persist_decider",
        role_name="Engineer",
        provider_key="default",
        event_queue=queue,
    )
    _, event = queue.published[0]
    assert "<think>Let me think... the turn was about backend reviews.</think>" in (
        event.response
    )
    # Visible content sits AFTER the think block, separated by a
    # blank line (mirrors main-loop format).
    assert event.response.endswith('{"kind": "NOOP"}')
    assert event.response.index("<think>") < event.response.index('{"kind"')


async def test_aux_telemetry_surfaces_reasoning_when_no_visible_content() -> None:
    """Cap-hit shape: thinking model exhausted the output budget
    before emitting visible text.  Without surfacing the reasoning,
    the dashboard would show ``response=""`` and the operator would
    have no signal that thinking happened.  Reasoning alone must
    render inside ``<think>`` tags."""
    queue = _QueueStub()
    provider = _ReasoningProvider(content="", reasoning="trying to decide...")
    await complete_with_telemetry(
        provider=provider,  # type: ignore[arg-type]
        messages=[Message(role="system", content="x")],
        worker="memory_filter",
        role_name="Engineer",
        provider_key="default",
        event_queue=queue,
    )
    _, event = queue.published[0]
    assert event.response == "<think>trying to decide...</think>"


async def test_aux_telemetry_omits_think_tags_when_no_reasoning() -> None:
    """Non-thinking models still work unchanged: the response field
    is the visible content with no wrapper.  Preserves the
    pre-existing event shape so dashboards / consumers that didn't
    opt into the new format see no behavioural difference."""
    queue = _QueueStub()
    provider = _ScriptedProvider(
        content="just a string", input_tokens=1, output_tokens=1
    )
    await complete_with_telemetry(
        provider=provider,  # type: ignore[arg-type]
        messages=[Message(role="system", content="x")],
        worker="persist_decider",
        role_name="Engineer",
        provider_key="default",
        event_queue=queue,
    )
    _, event = queue.published[0]
    assert event.response == "just a string"
    assert "<think>" not in event.response


async def test_response_formatter_hook_overrides_default_format() -> None:
    """Workers that want to enrich the dashboard view (e.g. the
    personal-memory filter appending a "→ Resolved selection:" block)
    pass a custom ``response_formatter`` -- the wrapper uses it
    instead of the default ``format_response_with_reasoning``."""
    queue = _QueueStub()
    provider = _ScriptedProvider(content="[0]", input_tokens=10, output_tokens=10)

    def _custom(comp):
        return f"[CUSTOM] {comp.content}"

    await complete_with_telemetry(
        provider=provider,  # type: ignore[arg-type]
        messages=[Message(role="system", content="x")],
        worker="memory_filter",
        role_name="Engineer",
        provider_key="default",
        event_queue=queue,
        response_formatter=_custom,
    )
    _, event = queue.published[0]
    assert event.response == "[CUSTOM] [0]"


async def test_response_formatter_exception_falls_back_to_default() -> None:
    """A buggy worker formatter must not lose the event entirely --
    fall back to the default so the dashboard still sees the LLM
    output."""
    queue = _QueueStub()
    provider = _ScriptedProvider(content="hello", input_tokens=1, output_tokens=1)

    def _broken(comp):
        raise ValueError("formatter crashed")

    await complete_with_telemetry(
        provider=provider,  # type: ignore[arg-type]
        messages=[Message(role="system", content="x")],
        worker="memory_filter",
        role_name="Engineer",
        provider_key="default",
        event_queue=queue,
        response_formatter=_broken,
    )
    _, event = queue.published[0]
    assert event.response == "hello"  # default format unchanged


async def test_response_formatter_does_not_affect_returned_completion() -> None:
    """The formatter only changes what's stamped on the published
    event.  The Completion returned to the caller is whatever the
    provider produced -- worker logic that parses the raw response
    must keep working."""
    queue = _QueueStub()
    provider = _ScriptedProvider(content="[0]", input_tokens=1, output_tokens=1)

    completion = await complete_with_telemetry(
        provider=provider,  # type: ignore[arg-type]
        messages=[Message(role="system", content="x")],
        worker="memory_filter",
        role_name="Engineer",
        provider_key="default",
        event_queue=queue,
        response_formatter=lambda c: "[ANNOTATED] " + (c.content or ""),
    )
    # Caller still gets the raw completion; only the event is
    # decorated.
    assert completion.content == "[0]"
    assert queue.published[0][1].response == "[ANNOTATED] [0]"


async def test_aux_telemetry_stores_full_payloads_untruncated() -> None:
    queue = _QueueStub()
    huge_input = "x" * 10_000
    huge_output = "y" * 20_000
    provider = _ScriptedProvider(content=huge_output, input_tokens=10, output_tokens=10)

    await complete_with_telemetry(
        provider=provider,  # type: ignore[arg-type]
        messages=[
            Message(role="system", content="sys"),
            Message(role="user", content=huge_input),
        ],
        worker="skill_synthesizer",
        role_name="Engineer",
        provider_key="default",
        event_queue=queue,
    )
    _, event = queue.published[0]
    # Full payloads stamped on the event verbatim -- no truncation.
    assert event.user_prompt == huge_input
    assert event.response == huge_output
    assert "[truncated]" not in event.user_prompt
    assert "[truncated]" not in event.response
