"""End-to-end turn-engine test: TurnCompleted event + episode write.

Runs a full Plan/Execute/Review turn with a scripted LLM provider and
asserts that (a) a ``TurnCompleted`` event is published with the right
shape, and (b) a single ``Episode`` is written to the stub store.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.turn import TurnEngine
from crewlet.events.types import TurnCompleted
from crewlet.learning.models import Episode
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, ToolCall
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))

    async def subscribe(self, topic, group, handler):  # pragma: no cover
        pass


class _EpisodeStoreStub:
    def __init__(self) -> None:
        self.written: list[Episode] = []

    async def write(self, episode: Episode) -> None:
        self.written.append(episode)

    async def query(self, **kwargs):  # pragma: no cover - not used here
        return []


class _PhaseScriptedProvider:
    """Routes completions by matching PLAN/EXECUTE/REVIEW in the prompt."""

    def __init__(
        self,
        *,
        plan: list[Completion],
        execute: list[Completion],
        review: list[Completion],
        model: str = "phased",
    ) -> None:
        self._plan = list(plan)
        self._execute = list(execute)
        self._review = list(review)
        self.model = model

    async def complete(
        self, messages, tools=None, temperature=0.7, max_tokens=None, tool_choice=None
    ):
        sys_text = ""
        for m in messages:
            if m.role == "system":
                sys_text = m.content
                break
        if "PLAN phase" in sys_text:
            return self._plan.pop(0) if self._plan else Completion(content="plan done")
        if "EXECUTE phase" in sys_text:
            return (
                self._execute.pop(0)
                if self._execute
                else Completion(content="exec done")
            )
        if "REVIEW phase" in sys_text:
            return (
                self._review.pop(0)
                if self._review
                else Completion(content="review done")
            )
        return Completion(content="?", tool_calls=[])


async def _noop(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    return ToolResult(success=True, output="noop")


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    role2 = org.get_role("Engineer")
    assert role2 is not None
    defn = AgentDefinition(role=role2, org=org)
    agent = AgentInstance(definition=defn, handle="eng", email="e@acme.com")
    agent.activate()
    return agent


def _mk_registry() -> ToolRegistry:
    r = ToolRegistry()
    r.register(
        SimpleTool(
            name="search",
            description="Search.",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    r.register(
        SimpleTool(
            name="query_knowledge",
            description="query",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    return r


def _plan_submission(tools_needed: list[str]) -> list[Completion]:
    return [
        Completion(
            content="",
            tool_calls=[
                ToolCall(
                    id="cp",
                    name="submit_plan",
                    arguments={
                        "decision": "plan",
                        "reasoning": "Because.",
                        "steps": [{"intent": "search"}],
                        "tools_needed": tools_needed,
                        "success_criteria": ["done"],
                        "needs_review": True,
                    },
                )
            ],
        ),
    ]


def _review_submission(decision: str, **kwargs) -> list[Completion]:
    args = {"decision": decision, "notes": ""}
    args.update(kwargs)
    return [
        Completion(
            content="",
            tool_calls=[ToolCall(id="cr", name="submit_review", arguments=args)],
        ),
    ]


async def test_turn_completed_published_with_plan_and_tool_sequence() -> None:
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["search"]),
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="search", arguments={"q": "x"})],
            ),
            Completion(content="found X", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="Final"),
    )
    queue = _QueueStub()
    store = _EpisodeStoreStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
        episode_store=store,
    )

    await engine.run_turn(
        agent, task_id="t1", task_description="Find X", org=agent.definition.org
    )

    # TurnCompleted event was published, exactly once.
    tc = [e for _, e in queue.published if isinstance(e, TurnCompleted)]
    assert len(tc) == 1
    event = tc[0]
    assert event.agent_handle == "eng"
    assert event.role == "Engineer"
    assert event.task_id == "t1"
    assert event.review_outcome == "done"
    assert "search" in event.tool_sequence
    assert event.iterations == 1
    assert event.task_summary.startswith("Find X")
    assert event.plan_summary  # non-empty
    assert event.duration_ms >= 0

    # Exactly one episode was written to the store.
    assert len(store.written) == 1
    ep = store.written[0]
    assert ep.agent_handle == "eng"
    assert ep.agent_role == "Engineer"
    assert ep.review_outcome == "done"
    assert ep.tool_sequence == event.tool_sequence
    assert ep.plan_summary == event.plan_summary


async def _suspend_tool(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    return ToolResult(
        success=True,
        output="(launched)",
        suspend=True,
        suspend_payload={"sandbox_id": "sbx-1", "turn_id": ""},
    )


async def test_suspended_turn_skips_learning_emit_and_episode() -> None:
    """A turn that only SUSPENDS for a detached sandbox run must not publish
    TurnCompleted or write an episode. Reflection runs once — on the resumed
    completion (same turn_id). Emitting here would reflect on an incomplete
    turn AND mark the turn_id seen in the ReflectEngine dedup so the genuine
    completion is dropped as a duplicate."""
    agent = _mk_agent()
    registry = _mk_registry()
    registry.register(
        SimpleTool(
            name="run_sandbox",
            description="Run code in a sandbox.",
            parameters={"type": "object"},
            fn=_suspend_tool,
        )
    )
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["run_sandbox"]),
        execute=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="run_sandbox", arguments={})],
            ),
        ],
        review=_review_submission("done", final_artifact="F"),
    )
    queue = _QueueStub()
    store = _EpisodeStoreStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=registry,
        event_queue=queue,
        episode_store=store,
    )

    await engine.run_turn(
        agent, task_id="t", task_description="fix CI", org=agent.definition.org
    )

    # The dashboard per-turn summary still fires (the kick-off is visible)…
    assert any(
        getattr(e, "type", "") == "agent_turn_completed" for _, e in queue.published
    )
    # …but the learning TurnCompleted + episode are suppressed on suspend.
    assert not any(isinstance(e, TurnCompleted) for _, e in queue.published)
    assert store.written == []


async def test_episode_not_written_when_store_is_none() -> None:
    """Engine must tolerate a missing episode store (in-memory mode)."""
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["search"]),
        execute=[Completion(content="done", tool_calls=[])],
        review=_review_submission("done", final_artifact="F"),
    )
    queue = _QueueStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=queue,
        # episode_store intentionally omitted
    )

    # Must not raise.
    await engine.run_turn(
        agent, task_id="t", task_description="x", org=agent.definition.org
    )

    # TurnCompleted still emitted even without a store.
    assert any(isinstance(e, TurnCompleted) for _, e in queue.published)


async def test_skills_used_derived_from_use_skill_tool_calls() -> None:
    """If Execute calls ``use_skill`` with a ``skill_name`` arg, it
    should surface in both the event and the episode's skills_used.
    """
    agent = _mk_agent()
    # Add use_skill to the registry so ToolSurface accepts it.
    registry = _mk_registry()
    registry.register(
        SimpleTool(
            name="use_skill",
            description="Load a skill.",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    provider = _PhaseScriptedProvider(
        plan=_plan_submission(["use_skill"]),
        execute=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(
                        id="cu",
                        name="use_skill",
                        arguments={"skill_name": "code-review"},
                    )
                ],
            ),
            Completion(content="done", tool_calls=[]),
        ],
        review=_review_submission("done", final_artifact="F"),
    )
    queue = _QueueStub()
    store = _EpisodeStoreStub()
    engine = TurnEngine(
        llm_providers={"default": provider},
        tool_registry=registry,
        event_queue=queue,
        episode_store=store,
    )

    await engine.run_turn(
        agent, task_id="t", task_description="x", org=agent.definition.org
    )

    event = next(e for _, e in queue.published if isinstance(e, TurnCompleted))
    assert event.skills_used == ["code-review"]
    assert store.written[0].skills_used == ["code-review"]
