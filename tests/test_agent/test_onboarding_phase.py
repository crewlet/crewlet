"""Tests for the dedicated first-turn onboarding phase (runs before Plan).

Onboarding (read the team Onboarding pages → persist conventions →
``mark_onboarded``) runs as its own phase with its own round budget so it
never competes with the Plan round budget.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.onboarding_phase import run_onboarding_phase
from crewlet.agent.plan import _fetch_onboarding_hint_block
from crewlet.agent.turn_context import TurnContext
from crewlet.learning.onboarding import (
    compute_chain_hash,
    register_mark_onboarded_tool,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, ToolCall
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


@dataclass
class _ProviderStub:
    completions: list[Completion]
    calls: int = 0
    model: str = "stub"

    async def complete(
        self, messages, tools=None, temperature=0.7, max_tokens=None, tool_choice=None
    ):
        idx = self.calls
        self.calls += 1
        if idx >= len(self.completions):
            return Completion(content="done", tool_calls=[])
        return self.completions[idx]


class _MarkerStore:
    """Minimal in-memory onboarding marker store (tri-state read contract)."""

    def __init__(self) -> None:
        self.rows: dict[str, str] = {}
        # Cross-process pass lease (scriptable): deny_claims makes
        # try_claim_pass lose, as if another engine holds the lease.
        self.deny_claims = False
        self.claims: list[str] = []
        self.releases: list[str] = []

    async def is_onboarded(self, *, agent_id: str, chain_hash: str) -> bool | None:
        return self.rows.get(agent_id) == chain_hash

    async def try_claim_pass(self, *, agent_id: str, ttl_seconds: float) -> bool:
        self.claims.append(agent_id)
        return not self.deny_claims

    async def release_claim(self, agent_id: str) -> None:
        self.releases.append(agent_id)

    async def mark(
        self,
        *,
        agent_id: str,
        chain_hash: str,
        agent_handle: str = "",
        role: str = "",
        summary: str = "",
    ) -> None:
        self.rows[agent_id] = chain_hash


async def _noop(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    return ToolResult(success=True, output="ok")


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    return AgentInstance(definition=defn, handle="eng", email="e@acme.com")


def _registry(marker_store: _MarkerStore, org: Organization) -> ToolRegistry:
    r = ToolRegistry()
    for name in ("reflect_and_persist", "load_tool_skill"):
        r.register(
            SimpleTool(
                name=name, description=name, parameters={"type": "object"}, fn=_noop
            )
        )
    register_mark_onboarded_tool(r, marker_store, org)
    return r


def _ctx(agent: AgentInstance, queue: _QueueStub, marker: Any) -> AgentContext:
    return AgentContext(
        agent_id=agent.id_str,
        agent_handle="eng",
        role="Engineer",
        org=agent.definition.org,
        event_queue=queue,
        onboarding_marker_store=marker,
    )


async def test_onboarding_runs_and_marks() -> None:
    agent = _mk_agent()
    org = agent.definition.org
    marker = _MarkerStore()
    registry = _registry(marker, org)
    queue = _QueueStub()
    turn = TurnContext(agent=agent, org=org, task_description="do a thing")
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="m",
                        name="mark_onboarded",
                        arguments={"summary": "learned the team conventions"},
                    )
                ],
            ),
        ]
    )

    ran = await run_onboarding_phase(
        turn=turn,
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=5,
    )

    assert ran is True
    # The marker was written for this agent's current org chain.
    expected = compute_chain_hash(org.get_role("Engineer"), org)
    assert marker.rows.get(agent.id_str) == expected
    # An onboarding phase event was published (dashboard visibility).
    assert any(getattr(e, "phase", "") == "onboarding" for _, e in queue.published)
    # Tokens accounted under an "onboarding" model key, not Plan's.
    assert "onboarding" in turn.model_keys


async def test_onboarding_extends_via_judge_then_marks() -> None:
    # The round-cap extension judge governs onboarding. The base cap (1)
    # exhausts before the agent marks; the judge grants more rounds and the
    # agent then completes onboarding — instead of being cut off mid-pass.
    agent = _mk_agent()
    org = agent.definition.org
    marker = _MarkerStore()
    registry = _registry(marker, org)
    queue = _QueueStub()
    turn = TurnContext(agent=agent, org=org, task_description="x")

    # Round 1 burns a non-terminating tool (base cap = 1 → exhausts without
    # mark_onboarded); after the judge extends, the next round marks.
    main = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="r",
                        name="reflect_and_persist",
                        arguments={"fact": "team uses trunk-based dev"},
                    )
                ],
            ),
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="m",
                        name="mark_onboarded",
                        arguments={"summary": "read the pages"},
                    )
                ],
            ),
        ]
    )

    class _Judge:
        model = "judge"

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            return Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="j",
                        name="submit_extension_decision",
                        arguments={
                            "decision": "extend",
                            "additional_rounds": 3,
                            "reason": "still reading pages",
                        },
                    )
                ],
            )

    ran = await run_onboarding_phase(
        turn=turn,
        provider=main,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=1,
        judge_provider=_Judge(),
        judge_provider_key="judge",
        extension_enabled=True,
        extension_ceiling=5,
        extension_round_step=3,
    )

    assert ran is True
    # The judge extended the pass, so the agent got a second round and marked.
    assert marker.rows.get(agent.id_str) == compute_chain_hash(
        org.get_role("Engineer"), org
    )
    # Main provider was called twice: the base round + the extended round.
    assert main.calls == 2


async def test_onboarding_skips_when_already_onboarded() -> None:
    agent = _mk_agent()
    org = agent.definition.org
    marker = _MarkerStore()
    marker.rows[agent.id_str] = compute_chain_hash(org.get_role("Engineer"), org)
    registry = _registry(marker, org)
    queue = _QueueStub()
    provider = _ProviderStub(completions=[])

    ran = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=5,
    )

    assert ran is False
    assert provider.calls == 0  # no LLM call when already onboarded
    # A confirmed read latches the process-local state for this chain.
    assert agent.onboarded_chain_hash == compute_chain_hash(
        org.get_role("Engineer"), org
    )


class _FlakyMarkerStore(_MarkerStore):
    """Marker store whose reads fail (return unknown) on demand."""

    def __init__(self) -> None:
        super().__init__()
        self.fail_reads = False
        self.reads = 0

    async def is_onboarded(self, *, agent_id: str, chain_hash: str) -> bool | None:
        self.reads += 1
        if self.fail_reads:
            return None  # lookup failed — state unknown
        return await super().is_onboarded(agent_id=agent_id, chain_hash=chain_hash)


async def test_onboarding_unknown_marker_state_skips_pass() -> None:
    # A failed marker lookup (unknown state) must NOT re-run onboarding —
    # the agent may already be marked; the check retries next turn.
    # Collapsing lookup failures into "not onboarded" would re-onboard
    # an already-marked agent ("marked but onboarded again").
    agent = _mk_agent()
    org = agent.definition.org
    marker = _FlakyMarkerStore()
    marker.fail_reads = True
    provider = _ProviderStub(completions=[])
    queue = _QueueStub()

    ran = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=provider,
        provider_key="stub",
        registry=_registry(marker, org),
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=5,
    )

    assert ran is False
    assert provider.calls == 0  # no pass ran on unknown state


async def test_onboarding_latch_survives_store_read_failures() -> None:
    # Once a pass MARKS, the in-process latch guarantees no later read flake
    # can re-fire onboarding for the same chain: turn 2's store reads fail
    # (unknown) yet the pass is skipped without even consulting the store.
    agent = _mk_agent()
    org = agent.definition.org
    marker = _FlakyMarkerStore()
    registry = _registry(marker, org)
    queue = _QueueStub()
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="m",
                        name="mark_onboarded",
                        arguments={"summary": "done"},
                    )
                ],
            ),
        ]
    )

    # Turn 1: runs and marks → latch set.
    ran = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=5,
    )
    assert ran is True
    expected = compute_chain_hash(org.get_role("Engineer"), org)
    assert agent.onboarded_chain_hash == expected

    # Turn 2: the store starts failing reads — the latch still skips the
    # pass, without a single store read.
    marker.fail_reads = True
    marker.reads = 0
    ran2 = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=5,
    )
    assert ran2 is False
    assert marker.reads == 0
    assert provider.calls == 1  # only turn 1's single mark call


async def test_onboarding_concurrent_turns_run_single_pass() -> None:
    # Turns for one agent can overlap when the sandbox busy-state transitions
    # free the agent mid-turn. The single-flight lock must hold the second
    # turn at the gate while the first pass runs, then let it skip via the
    # latch — ONE pass total, not two concurrent duplicates.
    import asyncio

    agent = _mk_agent()
    org = agent.definition.org
    marker = _MarkerStore()
    registry = _registry(marker, org)
    queue = _QueueStub()
    gate = asyncio.Event()

    class _GatedProvider:
        model = "stub"
        calls = 0

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            self.calls += 1
            await gate.wait()  # hold the pass open so the turns overlap
            return Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="m",
                        name="mark_onboarded",
                        arguments={"summary": "read the pages"},
                    )
                ],
            )

    provider = _GatedProvider()

    def _run() -> Any:
        return run_onboarding_phase(
            turn=TurnContext(agent=agent, org=org),
            provider=provider,
            provider_key="stub",
            registry=registry,
            role_mcp_tools=[],
            event_queue=queue,
            agent_context=_ctx(agent, queue, marker),
            max_rounds=5,
        )

    t1 = asyncio.create_task(_run())
    await asyncio.sleep(0)  # t1 acquires the lock and blocks on the gate
    t2 = asyncio.create_task(_run())
    await asyncio.sleep(0)  # t2 parks on the lock
    gate.set()
    ran1, ran2 = await t1, await t2

    assert ran1 is True
    assert ran2 is False  # waited, then skipped via the latch
    assert provider.calls == 1  # exactly one LLM pass
    assert marker.rows.get(agent.id_str) == compute_chain_hash(
        org.get_role("Engineer"), org
    )


async def test_onboarding_skips_when_lease_held_elsewhere() -> None:
    # Cross-process single-flight: another engine holds the DB pass lease —
    # this process must skip its pass (the winner's mark suppresses later
    # attempts; if the winner crashes the lease TTL expires and we retry).
    agent = _mk_agent()
    org = agent.definition.org
    marker = _MarkerStore()
    marker.deny_claims = True
    provider = _ProviderStub(completions=[])
    queue = _QueueStub()

    ran = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=provider,
        provider_key="stub",
        registry=_registry(marker, org),
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=5,
    )

    assert ran is False
    assert provider.calls == 0  # no pass ran
    assert marker.claims  # the lease was attempted
    assert not marker.releases  # nothing to release — we never held it


async def test_onboarding_releases_lease_when_pass_ends_unmarked() -> None:
    # An unmarked pass (budget exhausted) must release the lease so the
    # retry next turn isn't blocked until the TTL.
    agent = _mk_agent()
    org = agent.definition.org
    marker = _MarkerStore()
    registry = _registry(marker, org)
    queue = _QueueStub()
    # One non-terminating tool call, budget 1 round → ends unmarked.
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="r",
                        name="reflect_and_persist",
                        arguments={"fact": "reading"},
                    )
                ],
            ),
        ]
    )

    ran = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=provider,
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=1,
    )

    assert ran is True  # the pass ran (and will retry next turn)
    assert marker.claims == [agent.id_str]
    assert marker.releases == [agent.id_str]


async def test_onboarding_latch_invalidated_by_chain_change() -> None:
    # The latch is keyed by chain hash: a live org restructure (different
    # hash) must still re-fire onboarding by design.
    agent = _mk_agent()
    org = agent.definition.org
    agent.onboarded_chain_hash = "stale-hash-from-old-org-structure"
    marker = _MarkerStore()  # unmarked for the current chain
    queue = _QueueStub()
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="m",
                        name="mark_onboarded",
                        arguments={"summary": "re-read for the new structure"},
                    )
                ],
            ),
        ]
    )

    ran = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=provider,
        provider_key="stub",
        registry=_registry(marker, org),
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=5,
    )

    assert ran is True  # stale latch did not suppress the re-onboard
    assert agent.onboarded_chain_hash == compute_chain_hash(
        org.get_role("Engineer"), org
    )


async def test_onboarding_skips_without_marker_store() -> None:
    # No marker store → onboarding can't be completed, so the pass must not run
    # (otherwise it would fire every turn forever).
    agent = _mk_agent()
    org = agent.definition.org
    queue = _QueueStub()
    provider = _ProviderStub(completions=[])

    ran = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=provider,
        provider_key="stub",
        registry=ToolRegistry(),
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, None),
        max_rounds=5,
    )

    assert ran is False
    assert provider.calls == 0


async def test_onboarding_disabled_with_zero_budget() -> None:
    agent = _mk_agent()
    org = agent.definition.org
    marker = _MarkerStore()
    provider = _ProviderStub(completions=[])

    ran = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=provider,
        provider_key="stub",
        registry=_registry(marker, org),
        role_mcp_tools=[],
        event_queue=_QueueStub(),
        agent_context=_ctx(agent, _QueueStub(), marker),
        max_rounds=0,
    )

    assert ran is False
    assert provider.calls == 0


async def test_turn_runs_onboarding_phase_before_plan() -> None:
    # End-to-end: a real turn for an unmarked agent runs the onboarding phase
    # (its own budget) BEFORE Plan, marks the agent, and the onboarding phase
    # event precedes the plan phase event.
    from crewlet.agent.turn import TurnEngine

    agent = _mk_agent()
    agent.activate()  # run_turn requires an idle (activated) agent
    org = agent.definition.org
    marker = _MarkerStore()
    registry = _registry(marker, org)
    queue = _QueueStub()

    class _Router:
        model = "router"

        async def complete(
            self,
            messages,
            tools=None,
            temperature=0.7,
            max_tokens=None,
            tool_choice=None,
        ):
            sys = next((m.content for m in messages if m.role == "system"), "")
            if "ONBOARDING phase" in sys:
                return Completion(
                    content="",
                    tool_calls=[
                        ToolCall(
                            id="o",
                            name="mark_onboarded",
                            arguments={"summary": "learned the conventions"},
                        )
                    ],
                )
            if "PLAN phase" in sys:
                return Completion(
                    content="",
                    tool_calls=[
                        ToolCall(
                            id="p",
                            name="submit_plan",
                            arguments={
                                "decision": "skip",
                                "reasoning": "nothing to do",
                            },
                        )
                    ],
                )
            return Completion(content="ok", tool_calls=[])

    engine = TurnEngine(
        llm_providers={"default": _Router()},
        tool_registry=registry,
        event_queue=queue,
        onboarding_marker_store=marker,
    )
    await engine.run_turn(agent, task_id="t", task_description="hi", org=org)

    # The agent was onboarded during the turn …
    assert marker.rows.get(agent.id_str) == compute_chain_hash(
        org.get_role("Engineer"), org
    )
    # … and the onboarding phase event preceded the plan phase event.
    phases = [
        getattr(e, "phase", "")
        for _, e in queue.published
        if getattr(e, "type", "") == "agent_phase_completed"
    ]
    assert "onboarding" in phases
    assert "plan" in phases
    assert phases.index("onboarding") < phases.index("plan")


async def test_plan_hint_suppressed_after_onboarding_ran() -> None:
    # Once the dedicated pass has run this turn, the Plan-prompt onboarding hint
    # must be suppressed so onboarding can't re-run inside Plan and re-spend the
    # Plan round budget.
    agent = _mk_agent()
    org = agent.definition.org
    marker = _MarkerStore()  # deliberately NOT onboarded
    ctx = _ctx(agent, _QueueStub(), marker)

    # Without the flag the hint renders (the agent is unmarked).
    fresh = TurnContext(agent=agent, org=org)
    assert await _fetch_onboarding_hint_block(turn=fresh, agent_context=ctx)

    # With the flag set, it is suppressed.
    ran = TurnContext(agent=agent, org=org)
    ran.onboarding_ran = True
    assert await _fetch_onboarding_hint_block(turn=ran, agent_context=ctx) == ""


async def test_plan_hint_suppressed_on_unknown_marker_state() -> None:
    # A failed marker lookup (unknown) must NOT render the hint — showing it
    # to a possibly already-marked agent would re-run onboarding inside Plan.
    agent = _mk_agent()
    org = agent.definition.org
    marker = _FlakyMarkerStore()
    marker.fail_reads = True
    ctx = _ctx(agent, _QueueStub(), marker)

    turn = TurnContext(agent=agent, org=org)
    assert await _fetch_onboarding_hint_block(turn=turn, agent_context=ctx) == ""


async def test_plan_hint_suppressed_by_latch_without_store_read() -> None:
    # The process-local latch suppresses the hint outright — no DB read that
    # could flake.
    agent = _mk_agent()
    org = agent.definition.org
    agent.onboarded_chain_hash = compute_chain_hash(org.get_role("Engineer"), org)
    marker = _FlakyMarkerStore()
    marker.fail_reads = True  # would return unknown if consulted
    ctx = _ctx(agent, _QueueStub(), marker)

    turn = TurnContext(agent=agent, org=org)
    assert await _fetch_onboarding_hint_block(turn=turn, agent_context=ctx) == ""
    assert marker.reads == 0


async def test_on_start_fires_only_when_the_pass_actually_runs() -> None:
    # ``on_start`` is the "the pass cleared every skip gate" hook the turn
    # engine uses to swap the Slack working status to "is getting up to
    # speed…".  Firing it before the call would flash that text on every
    # turn after the first, since the overwhelmingly common outcome is a
    # skip for an already-onboarded agent.
    agent = _mk_agent()
    org = agent.definition.org
    marker = _MarkerStore()
    registry = _registry(marker, org)
    queue = _QueueStub()
    fired: list[str] = []

    async def _on_start() -> None:
        fired.append("started")

    def _provider() -> _ProviderStub:
        return _ProviderStub(
            completions=[
                Completion(
                    content="",
                    tool_calls=[
                        ToolCall(
                            id="m",
                            name="mark_onboarded",
                            arguments={"summary": "learned the conventions"},
                        )
                    ],
                ),
            ]
        )

    ran = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=_provider(),
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=5,
        on_start=_on_start,
    )
    assert ran is True
    assert fired == ["started"]

    # Second turn: already marked → skipped, and the hook stays silent.
    agent.onboarded_chain_hash = ""  # force the store read, not the latch
    ran_again = await run_onboarding_phase(
        turn=TurnContext(agent=agent, org=org),
        provider=_provider(),
        provider_key="stub",
        registry=registry,
        role_mcp_tools=[],
        event_queue=queue,
        agent_context=_ctx(agent, queue, marker),
        max_rounds=5,
        on_start=_on_start,
    )
    assert ran_again is False
    assert fired == ["started"]
