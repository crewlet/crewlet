"""Tests for the ``agent.subagent`` OTel span attributes.

Sub-agent spans carry ``tool_names``, ``system_prompt_hash``,
``turns_used``, and ``tokens_used``.
These get set at different points: the first two are known before the
loop starts, the latter two after it finishes.
"""

from __future__ import annotations

from collections.abc import Iterator
from dataclasses import dataclass, field
from typing import Any

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import ReadableSpan, TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
    InMemorySpanExporter,
)

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.subagent import spawn_subagent
from crewlet.agent.turn_context import TurnContext
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


async def _noop(params, ctx):
    return ToolResult(success=True, output="")


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="alice")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    agent = AgentInstance(definition=defn, handle="alice", email="a@acme.com")
    agent.activate()
    return agent


def _mk_registry() -> ToolRegistry:
    r = ToolRegistry()
    r.register(
        SimpleTool(
            name="search",
            description="s",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    return r


def _span_by_name(spans: list[ReadableSpan], name: str) -> ReadableSpan | None:
    for s in spans:
        if s.name == name:
            return s
    return None


@pytest.fixture
def otel_exporter() -> Iterator[InMemorySpanExporter]:
    """Attach an in-memory span exporter to the global OTel provider
    for the duration of one test, detaching cleanly on teardown.

    Attaching via a bare ``add_span_processor(...)`` without cleanup
    would leave the processor attached for the rest of the run: later
    tests would accumulate spans from earlier ones in the same
    exporter, and the provider type could leak into unrelated tests.

    We attach to the existing provider (which ``crewlet.telemetry``
    has already made a real ``TracerProvider`` at import time, so
    ``crewlet.telemetry.tracer`` is bound to it permanently) and
    on teardown remove our processor from the provider's private
    multi-processor list and shut it down.  The private attribute
    is brittle by nature, but OpenTelemetry exposes no public
    "remove processor" API; the alternative is letting state leak
    across tests.
    """
    provider = trace.get_tracer_provider()
    # ``crewlet.telemetry`` guarantees a real ``TracerProvider`` at
    # import time (see module-level setup there); this assertion
    # just documents that guarantee for the reader.
    assert isinstance(provider, TracerProvider)

    exporter = InMemorySpanExporter()
    processor = SimpleSpanProcessor(exporter)
    provider.add_span_processor(processor)
    try:
        yield exporter
    finally:
        processor.shutdown()
        # Remove our processor from the provider's private chain so
        # subsequent tests don't see our shut-down processor firing
        # (``on_end`` is a no-op post-shutdown, but it's cleaner to
        # actually detach).
        multi = provider._active_span_processor  # type: ignore[attr-defined]
        current = list(getattr(multi, "_span_processors", ()))
        if processor in current:
            current.remove(processor)
            multi._span_processors = tuple(current)  # type: ignore[attr-defined]


async def test_subagent_span_records_required_attributes(otel_exporter):
    """A successful spawn_subagent records tool_names,
    system_prompt_hash, turns_used, and tokens_used on its span."""
    exporter = otel_exporter

    class _Provider:
        model = "sub"

        async def complete(self, messages, tools=None, **_):
            return Completion(
                content="ok",
                tool_calls=[],
                input_tokens=7,
                output_tokens=3,
                tokens_used=10,
            )

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = AgentContext(
        agent_id=agent.id_str,
        agent_handle=agent.handle,
        role=agent.role_name,
        event_queue=_QueueStub(),
    )
    result = await spawn_subagent(
        parent_turn=turn,
        task_prompt="research x",
        system_prompt="You are a researcher.",
        tool_names=["search"],
        parent_tool_names=["search"],
        provider=_Provider(),
        provider_key="sub",
        registry=_mk_registry(),
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=ctx.event_queue,
    )
    assert result.timed_out is False
    assert result.turns_used >= 1
    assert result.tokens_used == 10

    # Locate the agent.subagent span and inspect attributes.
    span = _span_by_name(list(exporter.get_finished_spans()), "agent.subagent")
    assert span is not None
    attrs = dict(span.attributes or {})
    # The requested tool plus the always-on discovery meta-tools the
    # sub-agent uses to find and activate more read-only tools.
    tool_names = attrs["subagent.tool_names"].split(",")
    assert "search" in tool_names
    assert "activate_tool" in tool_names
    assert "list_mcp_server_tools" in tool_names
    assert attrs["subagent.max_turns"] >= 1
    # system_prompt_hash is a 16-char hex digest.
    sp_hash = attrs["subagent.system_prompt_hash"]
    assert isinstance(sp_hash, str) and len(sp_hash) == 16
    assert all(c in "0123456789abcdef" for c in sp_hash)
    # Post-run attributes.
    assert attrs["subagent.turns_used"] == result.turns_used
    assert attrs["subagent.tokens_used"] == 10


async def test_subagent_span_attributes_on_timeout(otel_exporter, monkeypatch):
    """Timed-out sub-agents still record turns_used=0 and
    tokens_used=0 on the span, plus a ``timed_out=True`` marker."""
    exporter = otel_exporter

    import asyncio

    class _Provider:
        model = "sub"

        async def complete(self, messages, tools=None, **_):
            await asyncio.sleep(10)
            return Completion(content="never")

    agent = _mk_agent()
    turn = TurnContext(agent=agent, org=agent.definition.org)
    ctx = AgentContext(
        agent_id=agent.id_str,
        agent_handle=agent.handle,
        role=agent.role_name,
        event_queue=_QueueStub(),
    )
    result = await spawn_subagent(
        parent_turn=turn,
        task_prompt="x",
        system_prompt="y",
        tool_names=[],
        parent_tool_names=[],
        provider=_Provider(),
        provider_key="sub",
        registry=_mk_registry(),
        role_mcp_tools=[],
        agent_context=ctx,
        event_queue=ctx.event_queue,
        timeout_seconds=0.01,
    )
    assert result.timed_out is True

    span = _span_by_name(list(exporter.get_finished_spans()), "agent.subagent")
    assert span is not None
    attrs = dict(span.attributes or {})
    assert attrs["subagent.turns_used"] == 0
    assert attrs["subagent.tokens_used"] == 0
    assert attrs["subagent.timed_out"] is True
    # system_prompt_hash is still recorded (known before the loop starts).
    assert "subagent.system_prompt_hash" in attrs
