"""A turn runs under the config it started with, and a seat drains
before that config is taken away.

Live config apply mutates engine state **in place** so in-flight work
keeps working — the LLM provider map is `clear()` + `update()`d, the
`TurnEngineSettings` cell hands out a new model, an `AgentDefinition` is
reassigned on the running instance, `_role_mcp_tools` is rewritten per
role. Every one of those is read *repeatedly* within one turn, so
"keeps working" was not the same as "stays coherent": a turn could plan
under one round cap and execute under another, or plan as one role and
execute as its replacement.

Two mechanisms, and the split matters. The **pin** makes a turn
internally consistent; it holds a catalogue, not a capability, so it
cannot keep an MCP client alive. **Draining** is what moves the rewire
to a point between turns — which is why it is load-bearing rather than
a nicety.
"""

from __future__ import annotations

import asyncio

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.turn import SEAT_DRAIN_TIMEOUT_SECONDS, TurnEngine
from crewlet.agent.turn_pin import TurnPin, current_pin, pin_for, pinned
from crewlet.agent.turn_settings import TurnEngineSettings
from crewlet.config import TurnEngineConfig
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.queue.memory import MemoryEventQueue
from crewlet.tools.registry import ToolRegistry


def _org(name: str = "Acme") -> Organization:
    return Organization(
        name=name,
        mission="ship",
        units=[OrgUnit(name="Core", type="team", lead="Dev", roles=[Role(name="Dev")])],
    )


def _agent(org: Organization | None = None) -> AgentInstance:
    org = org or _org()
    return AgentInstance(
        definition=AgentDefinition(role=org.get_role("Dev"), org=org),
        handle="dev",
    )


def _engine(**kwargs) -> TurnEngine:
    return TurnEngine(
        tool_registry=kwargs.pop("tool_registry", ToolRegistry()),
        llm_providers=kwargs.pop("llm_providers", {}),
        event_queue=kwargs.pop("event_queue", MemoryEventQueue()),
        **kwargs,
    )


# ── the pin primitive ────────────────────────────────────────────────


def test_no_pin_by_default() -> None:
    assert current_pin() is None


def test_pin_is_scoped_to_its_block() -> None:
    pin = TurnPin(owner=1, agent_id="a")
    with pinned(pin):
        assert current_pin() is pin
    assert current_pin() is None


def test_pin_only_matches_its_own_owner_and_seat() -> None:
    """A pin must never leak across engines or across seats.

    Sub-agents inherit a copy of the spawning turn's context, so a pin
    that matched on presence alone would hand one seat's definition to
    another's turn.
    """
    owner = object()
    other = object()
    pin = TurnPin(owner=id(owner), agent_id="seat-a")
    with pinned(pin):
        assert pin_for(owner, "seat-a") is pin
        assert pin_for(owner, "seat-b") is None
        assert pin_for(other, "seat-a") is None


async def test_a_pin_propagates_into_spawned_tasks() -> None:
    """Sub-agents run as their own tasks and must see the parent's pin.

    ``contextvars`` copy at ``create_task`` time, which is exactly the
    semantics wanted: a sub-agent inherits the turn that spawned it.
    """
    pin = TurnPin(owner=1, agent_id="a")
    seen: list[TurnPin | None] = []

    async def _child() -> None:
        seen.append(current_pin())

    with pinned(pin):
        await asyncio.create_task(_child())
    assert seen == [pin]


# ── the settings cell ────────────────────────────────────────────────


def test_settings_read_through_the_pin() -> None:
    cell = TurnEngineSettings(TurnEngineConfig(max_tool_rounds=5))
    pinned_cfg = TurnEngineConfig(max_tool_rounds=99)
    with pinned(TurnPin(owner=1, agent_id="a", settings=pinned_cfg)):
        assert cell.get().max_tool_rounds == 99
        # ``live()`` is the escape hatch the apply path uses.
        assert cell.live().max_tool_rounds == 5
    assert cell.get().max_tool_rounds == 5


def test_a_config_swap_mid_pin_does_not_move_the_turn() -> None:
    """The bug this exists for: ~18 accessors re-read the cell on EVERY
    access, so a swap between phases changed the limits under a turn."""
    cell = TurnEngineSettings(TurnEngineConfig(max_tool_rounds=5))
    with pinned(TurnPin(owner=1, agent_id="a", settings=cell.live())):
        cell.set(TurnEngineConfig(max_tool_rounds=99))
        assert cell.get().max_tool_rounds == 5
    # The next turn takes the new value.
    assert cell.get().max_tool_rounds == 99


# ── the agent definition ─────────────────────────────────────────────


def test_definition_reads_through_the_pin() -> None:
    agent = _agent()
    original = agent.definition
    replacement = AgentDefinition(role=_org("Other").get_role("Dev"), org=_org("Other"))

    with pinned(TurnPin(owner=1, agent_id=agent.id_str, definition=original)):
        agent.definition = replacement
        assert agent.definition is original
        # The apply path diffs against what is actually installed.
        assert agent.live_definition is replacement
    assert agent.definition is replacement


def test_another_seats_turn_sees_the_live_definition() -> None:
    agent = _agent()
    replacement = AgentDefinition(role=_org("Other").get_role("Dev"), org=_org("Other"))
    stale = agent.definition
    agent.definition = replacement

    with pinned(TurnPin(owner=1, agent_id="some-other-seat", definition=stale)):
        assert agent.definition is replacement


# ── the engine's capture ─────────────────────────────────────────────


def test_capture_snapshots_all_four_surfaces() -> None:
    providers = {"default": object()}
    role_mcp: dict[str, list] = {"Dev": ["tool-a"]}
    engine = _engine(llm_providers=providers, role_mcp_tools=role_mcp)
    agent = _agent()

    pin = engine._capture_pin(agent)
    assert pin.owner == id(engine)
    assert pin.agent_id == agent.id_str
    assert pin.definition is agent.live_definition
    assert pin.llm_providers == providers
    assert pin.role_mcp_tools == ["tool-a"]

    # Snapshots, not aliases — the engine mutates both in place.
    providers.clear()
    role_mcp["Dev"].clear()
    assert pin.llm_providers != providers
    assert pin.role_mcp_tools == ["tool-a"]


def test_provider_map_reads_through_the_pin() -> None:
    live = {"default": "old"}
    engine = _engine(llm_providers=live)
    pinned_map = dict(live)

    with pinned(TurnPin(owner=id(engine), agent_id="a", llm_providers=pinned_map)):
        live.clear()
        live.update({"default": "new"})
        assert engine._llm_providers == {"default": "old"}
    assert engine._llm_providers == {"default": "new"}


def test_role_mcp_tools_read_through_the_pin() -> None:
    live: dict[str, list] = {"Dev": ["old-tool"]}
    engine = _engine(role_mcp_tools=live)

    with pinned(
        TurnPin(owner=id(engine), agent_id="seat", role_mcp_tools=["old-tool"])
    ):
        live["Dev"] = ["new-tool"]
        assert engine._role_mcp_for("Dev", "seat") == ["old-tool"]
        # A different seat is not covered by this pin.
        assert engine._role_mcp_for("Dev", "other") == ["new-tool"]
    assert engine._role_mcp_for("Dev", "seat") == ["new-tool"]


# ── the per-seat drain ───────────────────────────────────────────────


async def test_drain_returns_immediately_when_the_seat_is_idle() -> None:
    engine = _engine()
    assert engine.seat_in_flight("Dev") == 0
    assert await engine.drain_seat("Dev") is True


async def test_drain_waits_for_the_seat_then_returns() -> None:
    engine = _engine()
    engine._seat_enter("Dev")
    assert engine.seat_in_flight("Dev") == 1

    async def _finish() -> None:
        await asyncio.sleep(0)
        engine._seat_exit("Dev")

    finisher = asyncio.create_task(_finish())
    assert await engine.drain_seat("Dev", timeout=5.0) is True
    assert engine.seat_in_flight("Dev") == 0
    await finisher


async def test_drain_waits_for_every_turn_on_the_seat() -> None:
    """Two overlapping turns for one seat — the sandbox transitions can
    free an agent while an earlier turn is still mid-flight."""
    engine = _engine()
    engine._seat_enter("Dev")
    engine._seat_enter("Dev")

    async def _finish() -> None:
        await asyncio.sleep(0)
        engine._seat_exit("Dev")
        await asyncio.sleep(0)
        engine._seat_exit("Dev")

    finisher = asyncio.create_task(_finish())
    assert await engine.drain_seat("Dev", timeout=5.0) is True
    await finisher


async def test_drain_ignores_other_seats() -> None:
    engine = _engine()
    engine._seat_enter("Other")
    assert await engine.drain_seat("Dev", timeout=5.0) is True


async def test_drain_gives_up_rather_than_blocking_the_apply() -> None:
    """The timeout is a cap, not a contract.

    An apply that blocks indefinitely on one busy seat is strictly worse
    than one turn seeing a mid-flight rewire — which is what every turn
    saw before the drain existed, and which the pin makes survivable.
    """
    engine = _engine()
    engine._seat_enter("Dev")
    assert await engine.drain_seat("Dev", timeout=0.05) is False
    assert engine.seat_in_flight("Dev") == 1


async def test_a_late_exit_still_wakes_a_waiting_drain() -> None:
    """The clear-then-recheck ordering, stated as the race it closes.

    Clearing the idle event *after* the in-flight check would drop
    exactly the wakeup being waited for, and the drain would sit out its
    whole timeout with the seat already idle.
    """
    engine = _engine()
    engine._seat_enter("Dev")

    async def _exit_soon() -> None:
        await asyncio.sleep(0.01)
        engine._seat_exit("Dev")

    exiter = asyncio.create_task(_exit_soon())
    # A generous timeout that the test would still hit if the wakeup
    # were lost, so a regression fails on time rather than on assertion.
    drained = await asyncio.wait_for(engine.drain_seat("Dev", timeout=5.0), timeout=1.0)
    assert drained is True
    await exiter


def test_the_drain_cap_is_one_llm_round() -> None:
    """Pinned so a future edit has to justify moving it: the wait is for
    the tail of an LLM round, not for a whole turn."""
    assert pytest.approx(10.0) == SEAT_DRAIN_TIMEOUT_SECONDS
