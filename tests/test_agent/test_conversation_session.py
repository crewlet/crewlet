"""The conversation ledger, wired into a real turn.

Covers the two halves that only exist once the TurnEngine is in the
picture: which turns get recorded (and which deliberately do not), and
that a later turn of the same conversation is handed what the earlier
one did.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.agent.turn import TurnEngine
from crewlet.config import ConversationSessionConfig, TurnEngineConfig
from crewlet.db.conversation_sessions import MemoryConversationSessionStore
from crewlet.events.types import Event, ExternalNotification
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, ToolCall
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.work_key import bind_work_key


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))

    async def subscribe(self, topic, group, handler):
        pass


class _PhaseScriptedProvider:
    """Canned completions per phase, routed on the system prompt."""

    def __init__(
        self,
        *,
        plan: list[Completion],
        execute: list[Completion],
        review: list[Completion],
    ) -> None:
        self._plan = list(plan)
        self._execute = list(execute)
        self._review = list(review)
        self.model = "phased"
        self.user_prompts: list[str] = []

    async def complete(self, messages, **kwargs) -> Completion:
        system = next((m.content for m in messages if m.role == "system"), "")
        user = next((m.content for m in messages if m.role == "user"), "")
        if "PLAN phase" in system:
            self.user_prompts.append(user)
            queue = self._plan
        elif "EXECUTE phase" in system:
            self.user_prompts.append(user)
            queue = self._execute
        else:
            queue = self._review
        return queue.pop(0) if queue else Completion(content="", tool_calls=[])


async def _noop(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    return ToolResult(success=True, content="ok")


def _mk_agent() -> AgentInstance:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    resolved = org.get_role("Engineer")
    assert resolved is not None
    agent = AgentInstance(
        definition=AgentDefinition(role=resolved, org=org),
        handle="eng",
        email="e@acme.com",
    )
    agent.activate()
    return agent


def _mk_registry() -> ToolRegistry:
    registry = ToolRegistry()
    registry.register(
        SimpleTool(
            name="jira_add_comment",
            description="Comment on an issue.",
            parameters={"type": "object"},
            fn=_noop,
        )
    )
    return registry


def _plan(tools: list[str], reasoning: str = "Because.") -> list[Completion]:
    return [
        Completion(
            content="",
            tool_calls=[
                ToolCall(
                    id="cp",
                    name="submit_plan",
                    arguments={
                        "decision": "plan",
                        "reasoning": reasoning,
                        "steps": [{"intent": "reply"}],
                        "tools_needed": tools,
                        "success_criteria": ["done"],
                    },
                )
            ],
        )
    ]


def _execute(text: str) -> list[Completion]:
    return [
        Completion(
            content="",
            tool_calls=[
                ToolCall(id="c1", name="jira_add_comment", arguments={"key": "POC-7"})
            ],
        ),
        Completion(content=text, tool_calls=[]),
    ]


def _review(decision: str = "done", **kw) -> list[Completion]:
    args = {"decision": decision, "notes": ""}
    args.update(kw)
    return [
        Completion(
            content="",
            tool_calls=[ToolCall(id="cr", name="submit_review", arguments=args)],
        )
    ]


def _jira_trigger(body: str = "please take a look") -> ExternalNotification:
    return ExternalNotification(
        notification_source="jira",
        sender="Dana",
        body=body,
        metadata={"issue_key": "POC-7"},
    )


def _mk_engine(
    provider: Any,
    store: Any,
    *,
    session_cfg: ConversationSessionConfig | None = None,
) -> TurnEngine:
    from crewlet.agent.turn_settings import TurnEngineSettings

    cfg = TurnEngineConfig(
        conversation_session=session_cfg or ConversationSessionConfig()
    )
    return TurnEngine(
        llm_providers={"default": provider},
        tool_registry=_mk_registry(),
        event_queue=_QueueStub(),
        conversation_sessions=store,
        settings=TurnEngineSettings(cfg),
    )


async def _run(engine: TurnEngine, agent: AgentInstance, event: Event, work: str):
    with bind_work_key(work):
        return await engine.run_turn(
            agent, event=event, org=agent.definition.org, task_id=""
        )


# ── what gets recorded ───────────────────────────────────────────────


async def test_a_completed_turn_is_recorded_against_its_conversation() -> None:
    agent = _mk_agent()
    store = MemoryConversationSessionStore()
    engine = _mk_engine(
        _PhaseScriptedProvider(
            plan=_plan(["jira_add_comment"], reasoning="They asked for a review."),
            execute=_execute("Commented on POC-7."),
            review=_review("done", final_artifact="Commented on POC-7."),
        ),
        store,
    )

    await _run(engine, agent, _jira_trigger(), "w1")

    rows = await store.recent(
        agent_handle="eng", conversation_key="jira:POC-7", limit=5
    )
    assert len(rows) == 1
    entry = rows[0].entry
    assert entry["reply"] == "Commented on POC-7."
    assert entry["plan_reasoning"] == "They asked for a review."
    # The write that actually landed is in the record.
    assert "jira_add_comment" in entry["tool_calls"]
    # And who asked, so the next turn can tell what it was answering.
    assert "Dana" in entry["trigger"]


async def test_the_reviewers_verdict_reaches_the_entry() -> None:
    """A ``done`` round appends no IterationRecord, so without the
    engine stashing the ReviewOutcome this prose would be lost."""
    agent = _mk_agent()
    store = MemoryConversationSessionStore()
    engine = _mk_engine(
        _PhaseScriptedProvider(
            plan=_plan(["jira_add_comment"]),
            execute=_execute("done"),
            review=_review(
                "done", final_artifact="done", completed_work="Posted the comment."
            ),
        ),
        store,
    )

    await _run(engine, agent, _jira_trigger(), "w1")

    rows = await store.recent(
        agent_handle="eng", conversation_key="jira:POC-7", limit=5
    )
    assert rows[0].entry["completed_work"] == "Posted the comment."


async def test_a_trigger_with_no_reproducible_conversation_is_not_recorded() -> None:
    """A scheduled fire or task assignment keys as ``event:{uuid}``,
    which no later message can reproduce — a row nothing could read."""
    agent = _mk_agent()
    store = MemoryConversationSessionStore()
    engine = _mk_engine(
        _PhaseScriptedProvider(
            plan=_plan(["jira_add_comment"]),
            execute=_execute("done"),
            review=_review("done", final_artifact="done"),
        ),
        store,
    )

    with bind_work_key("w1"):
        await engine.run_turn(
            agent,
            task_id="t1",
            task_description="Do the scheduled thing",
            org=agent.definition.org,
        )

    assert await store.conversations(agent_handle="eng") == []


async def test_a_turn_with_no_work_key_is_not_recorded() -> None:
    """No work key means nothing to dedupe on: a redelivery would append
    a second copy of the same turn and the next turn would read its own
    reply twice."""
    agent = _mk_agent()
    store = MemoryConversationSessionStore()
    engine = _mk_engine(
        _PhaseScriptedProvider(
            plan=_plan(["jira_add_comment"]),
            execute=_execute("done"),
            review=_review("done", final_artifact="done"),
        ),
        store,
    )

    await engine.run_turn(agent, event=_jira_trigger(), org=agent.definition.org)

    assert await store.conversations(agent_handle="eng") == []


async def test_the_ledger_can_be_turned_off() -> None:
    """A live kill switch: disabling restores the pre-ledger prompt
    exactly, which is why it is safe to leave on by default."""
    agent = _mk_agent()
    store = MemoryConversationSessionStore()
    engine = _mk_engine(
        _PhaseScriptedProvider(
            plan=_plan(["jira_add_comment"]),
            execute=_execute("done"),
            review=_review("done", final_artifact="done"),
        ),
        store,
        session_cfg=ConversationSessionConfig(enabled=False),
    )

    await _run(engine, agent, _jira_trigger(), "w1")

    assert await store.conversations(agent_handle="eng") == []


# ── what a later turn is handed ──────────────────────────────────────


async def test_a_later_turn_is_handed_the_earlier_one() -> None:
    """The whole point. Turn two on the same ticket sees what turn one
    said, without re-reading the ticket."""
    agent = _mk_agent()
    store = MemoryConversationSessionStore()

    first = _PhaseScriptedProvider(
        plan=_plan(["jira_add_comment"]),
        execute=_execute("The failure is in the retry path."),
        review=_review("done", final_artifact="The failure is in the retry path."),
    )
    await _run(engine := _mk_engine(first, store), agent, _jira_trigger(), "w1")

    second = _PhaseScriptedProvider(
        plan=_plan(["jira_add_comment"]),
        execute=_execute("Still the retry path."),
        review=_review("done", final_artifact="Still the retry path."),
    )
    engine = _mk_engine(second, store)
    await _run(engine, agent, _jira_trigger("any update?"), "w2")

    plan_prompt = second.user_prompts[0]
    assert "## Earlier in this conversation" in plan_prompt
    assert "The failure is in the retry path." in plan_prompt
    # Execute gets it too — it is the phase that fires the side effects.
    assert any("## Earlier in this conversation" in p for p in second.user_prompts[1:])


async def test_a_different_conversation_gets_no_history() -> None:
    agent = _mk_agent()
    store = MemoryConversationSessionStore()

    await _run(
        _mk_engine(
            _PhaseScriptedProvider(
                plan=_plan(["jira_add_comment"]),
                execute=_execute("about POC-7"),
                review=_review("done", final_artifact="about POC-7"),
            ),
            store,
        ),
        agent,
        _jira_trigger(),
        "w1",
    )

    other = _PhaseScriptedProvider(
        plan=_plan(["jira_add_comment"]),
        execute=_execute("about POC-9"),
        review=_review("done", final_artifact="about POC-9"),
    )
    trigger = ExternalNotification(
        notification_source="jira",
        sender="Dana",
        body="different ticket",
        metadata={"issue_key": "POC-9"},
    )
    await _run(_mk_engine(other, store), agent, trigger, "w2")

    assert "## Earlier in this conversation" not in other.user_prompts[0]


async def test_the_first_turn_of_a_conversation_sees_no_block() -> None:
    """The common case must keep its exact pre-ledger prompt shape."""
    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan(["jira_add_comment"]),
        execute=_execute("done"),
        review=_review("done", final_artifact="done"),
    )

    await _run(
        _mk_engine(provider, MemoryConversationSessionStore()),
        agent,
        _jira_trigger(),
        "w1",
    )

    assert provider.user_prompts[0].startswith("Task:")


async def test_an_unreadable_store_does_not_stop_the_turn() -> None:
    """Fail open: no history is the pre-ledger prompt, and a ledger
    outage must never cost a turn."""

    class _Broken:
        async def recent(self, **kw):
            raise RuntimeError("database is down")

        async def append(self, **kw):
            raise RuntimeError("database is down")

    agent = _mk_agent()
    provider = _PhaseScriptedProvider(
        plan=_plan(["jira_add_comment"]),
        execute=_execute("done"),
        review=_review("done", final_artifact="delivered anyway"),
    )

    result = await _run(_mk_engine(provider, _Broken()), agent, _jira_trigger(), "w1")

    assert result == "delivered anyway"
    assert provider.user_prompts[0].startswith("Task:")
