"""Turn-engine ↔ Slack working-status wiring.

The TurnEngine owns the indicator's lifecycle: it goes up when a
Slack-triggered turn starts and comes down when the turn ends — whether
the agent replied, decided to stay silent, or failed. A turn that
SUSPENDS for a detached sandbox run holds it, because the agent has
neither replied nor given up.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.execute import ExecuteResult
from crewlet.agent.instance import AgentInstance, AgentState
from crewlet.agent.turn import TurnEngine
from crewlet.events.types import ExternalNotification
from crewlet.notifications.typing_status import (
    PHASE_PHRASES,
    WorkingStatusDriver,
    WorkingStatusMode,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, ToolCall
from crewlet.tools.registry import ToolRegistry

SLACK_META = {
    "transport": "slack",
    "channel": "C900",
    "channel_type": "channel",
    "ts": "1700000000.000100",
    "thread_ts": "",
    "thread_follow_reason": "mention",
    "user": "U1",
}


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


class _FakePoster:
    status_backend = "slack"
    supports_status_text = True
    status_refresh_interval = 45.0
    dm_channel_id_prefix = "D"

    def __init__(self) -> None:
        self.calls: list[tuple[str, str, str, str]] = []

    async def set_status(
        self, handle: str, channel: str, thread_id: str, status: str
    ) -> bool:
        self.calls.append((handle, channel, thread_id, status))
        return True

    @property
    def statuses(self) -> list[str]:
        return [c[3] for c in self.calls]


class _SlackTransportStub:
    """Just enough transport for the engine's ``typing_status`` lookup."""

    name = "slack"

    def __init__(self, mode: WorkingStatusMode = WorkingStatusMode.ADDRESSED):
        self.poster = _FakePoster()
        self.typing_status = WorkingStatusDriver(self.poster, mode)


class _ServiceStub:
    def __init__(self, transport: Any = None) -> None:
        self.transports = {"slack": transport} if transport is not None else {}


class _SkipPlanner:
    """Shortest legal turn: Plan submits ``skip`` and the turn ends."""

    model = "stub"

    def __init__(self) -> None:
        self.calls = 0

    async def complete(
        self, messages, tools=None, temperature=0.7, max_tokens=None, tool_choice=None
    ):
        self.calls += 1
        return Completion(
            content="",
            tool_calls=[
                ToolCall(
                    id="p",
                    name="submit_plan",
                    arguments={"decision": "skip", "reasoning": "not for me"},
                )
            ],
        )


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    agent = AgentInstance(definition=defn, handle="eng", email="e@acme.com")
    agent.activate()
    return agent


def _mk_engine(transport: Any, provider: Any = None) -> TurnEngine:
    return TurnEngine(
        llm_providers={"default": provider or _SkipPlanner()},
        tool_registry=ToolRegistry(),
        event_queue=_QueueStub(),
        notification_service=_ServiceStub(transport),
        onboarding_max_tool_rounds=0,
    )


def _slack_event(metadata: dict[str, str] | None = None) -> ExternalNotification:
    return ExternalNotification(
        source="notification_service.slack",
        notification_source="slack",
        source_event_type="app_mention",
        sender="U1",
        subject="Slack message",
        body="hey eng, can you look at this?",
        metadata=dict(metadata if metadata is not None else SLACK_META),
    )


async def test_slack_turn_raises_then_clears_the_indicator() -> None:
    agent = _mk_agent()
    transport = _SlackTransportStub()
    engine = _mk_engine(transport)

    await engine.run_turn(agent, event=_slack_event(), org=agent.definition.org)

    statuses = transport.poster.statuses
    assert statuses[0] in PHASE_PHRASES["plan"]
    assert statuses[-1] == ""  # "gave up" (skip) still clears
    assert transport.typing_status.active_conversations == []
    assert agent.state == AgentState.IDLE
    # It was set on the thread the agent would reply under.
    assert transport.poster.calls[0][1:3] == ("C900", "1700000000.000100")


async def test_onboarding_status_does_not_flash_for_an_onboarded_agent() -> None:
    """The onboarding pass self-skips for every turn after the first, so its
    status must ride the pass's ``on_start`` hook — announcing it up front
    would show "is getting up to speed…" on every turn forever."""
    agent = _mk_agent()
    transport = _SlackTransportStub()
    engine = TurnEngine(
        llm_providers={"default": _SkipPlanner()},
        tool_registry=ToolRegistry(),
        event_queue=_QueueStub(),
        notification_service=_ServiceStub(transport),
        # Onboarding enabled, but no marker store is wired → the pass
        # self-skips exactly as it does for an already-onboarded agent.
        onboarding_max_tool_rounds=10,
    )

    await engine.run_turn(agent, event=_slack_event(), org=agent.definition.org)

    statuses = transport.poster.statuses
    assert not any(s in PHASE_PHRASES["onboarding"] for s in statuses)
    assert len(statuses) == 2
    assert statuses[0] in PHASE_PHRASES["plan"]
    assert statuses[1] == ""


async def test_non_slack_turn_shows_nothing() -> None:
    agent = _mk_agent()
    transport = _SlackTransportStub()
    engine = _mk_engine(transport)

    await engine.run_turn(
        agent, task_id="t1", task_description="do a thing", org=agent.definition.org
    )

    assert transport.poster.calls == []


async def test_passive_channel_message_shows_nothing_in_addressed_mode() -> None:
    agent = _mk_agent()
    transport = _SlackTransportStub()
    engine = _mk_engine(transport)
    passive = {k: v for k, v in SLACK_META.items() if k != "thread_follow_reason"}

    await engine.run_turn(agent, event=_slack_event(passive), org=agent.definition.org)

    assert transport.poster.calls == []


async def test_always_mode_covers_a_passive_channel_message() -> None:
    agent = _mk_agent()
    transport = _SlackTransportStub(WorkingStatusMode.ALWAYS)
    engine = _mk_engine(transport)
    passive = {k: v for k, v in SLACK_META.items() if k != "thread_follow_reason"}

    await engine.run_turn(agent, event=_slack_event(passive), org=agent.definition.org)

    assert transport.poster.statuses[0] in PHASE_PHRASES["plan"]
    assert transport.poster.statuses[-1] == ""


async def test_detached_suspend_holds_the_indicator_until_the_resume_ends(
    monkeypatch,
) -> None:
    agent = _mk_agent()
    transport = _SlackTransportStub()
    engine = _mk_engine(transport)

    async def _detached(turn, typing=None):
        if typing is not None:
            await typing.set_phase("execute")
        turn.last_execute_result = ExecuteResult(
            status="detached", sandbox_id="sbx-1", text=""
        )
        return "(suspended)", "done"

    monkeypatch.setattr(engine, "_drive_phases", _detached)
    await engine.run_turn(
        agent, event=_slack_event(), org=agent.definition.org, turn_id="turn-1"
    )

    assert agent.state == AgentState.AWAITING_SANDBOX
    assert transport.poster.statuses[-1] in PHASE_PHRASES["execute"]
    assert len(transport.typing_status.active_conversations) == 1

    # The coordinator resumes the SAME turn_id when the coding job lands.
    agent.resume_from_sandbox()
    monkeypatch.undo()
    await engine.run_turn(
        agent, event=_slack_event(), org=agent.definition.org, turn_id="turn-1"
    )

    assert transport.poster.statuses[-1] == ""
    assert transport.typing_status.active_conversations == []


async def test_failed_turn_still_clears_the_indicator(monkeypatch) -> None:
    agent = _mk_agent()
    transport = _SlackTransportStub()
    engine = _mk_engine(transport)

    async def _boom(turn, typing=None):
        raise RuntimeError("phase exploded")

    monkeypatch.setattr(engine, "_drive_phases", _boom)
    with pytest.raises(RuntimeError, match="phase exploded"):
        await engine.run_turn(agent, event=_slack_event(), org=agent.definition.org)

    assert transport.poster.statuses[-1] == ""
    assert transport.typing_status.active_conversations == []


async def test_turn_refused_during_shutdown_clears_the_indicator() -> None:
    import asyncio

    from crewlet.agent.turn import ShutdownDraining

    agent = _mk_agent()
    transport = _SlackTransportStub()
    engine = _mk_engine(transport)

    # Park the turn behind a busy agent, then start draining: the turn
    # aborts at the gate, BEFORE the main try/finally is entered.
    assert agent.start_working("earlier-turn")
    task = asyncio.create_task(
        engine.run_turn(agent, event=_slack_event(), org=agent.definition.org)
    )
    await asyncio.sleep(0)
    assert len(transport.poster.statuses) == 1
    assert transport.poster.statuses[0] in PHASE_PHRASES["plan"]

    engine.begin_shutdown()
    with pytest.raises(ShutdownDraining):
        await asyncio.wait_for(task, timeout=5)

    assert transport.poster.statuses[-1] == ""
    assert transport.typing_status.active_conversations == []


async def test_no_slack_transport_is_a_no_op() -> None:
    agent = _mk_agent()
    engine = _mk_engine(None)
    result = await engine.run_turn(
        agent, event=_slack_event(), org=agent.definition.org
    )
    assert isinstance(result, str)
    assert agent.state == AgentState.IDLE
