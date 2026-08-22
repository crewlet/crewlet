"""Engine._handle_a2a — the wake, the turn it runs, and the reply.

The reply is the part that did not exist. ``a2a_ask``'s own description
promises *"they reply on the same channel"*, and the wake prompt told the
agent to call ``send_a2a_message`` — a tool that is not registered and
that ``tests/test_tools/test_builtin.py`` asserts is absent. So every ask
delivered a brief and every answer went nowhere.

The answering turn's final text is the reply now, which is why these
tests care so much about *which* party is answering: the requester's own
turn is itself triggered by that reply, so a rule that let it answer too
would have two agents volleying until the delegation cap stopped them.
"""

from __future__ import annotations

from collections.abc import AsyncIterator
from typing import Any
from unittest.mock import AsyncMock

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance
from crewlet.engine import Engine
from crewlet.events.types import Event
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, CompletionChunk, Message, ToolDef


class _MockLLM:
    """Returns a fixed response for every completion request."""

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice: str | None = None,
    ) -> Completion:
        return Completion(content="Acknowledged via A2A")

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]:
        yield CompletionChunk(content="streamed")


def _make_org() -> Organization:
    return Organization(
        name="Test Corp",
        mission="Test A2A",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="CEO",
                roles=[
                    Role(name="CEO", manages=["CTO"]),
                    Role(name="CTO"),
                ],
            )
        ],
    )


def _make_agent(org: Organization, role_name: str) -> AgentInstance:
    role = next(r for r in org.all_roles() if r.name == role_name)
    defn = AgentDefinition(role=role, org=org)
    return AgentInstance(definition=defn, handle=role_name.lower())


@pytest.fixture
def org() -> Organization:
    return _make_org()


@pytest.fixture
async def engine(org: Organization) -> AsyncIterator[Engine]:
    """A started engine whose seats do not consume their own inboxes.

    Detached deliberately: these tests drive ``_handle_a2a`` by hand to
    assert what it does with one wake, and an attached engine consumes
    the wake first — running a real turn, replying and closing the
    channel before the test gets to look at it. Detaching leaves the
    durable subscription (so a published wake is retained, not dropped)
    and the test's own observer groups get their own copy regardless.

    ``test_the_whole_round_trip_runs_without_help`` keeps them attached,
    because that is the one thing worth asserting end to end.
    """
    from crewlet.queue.topics import agent_inbox_group, agent_inbox_topic

    e = Engine(organization=org, llm_providers={"default": _MockLLM()})
    await e.start()
    for handle in ("ceo", "cto"):
        await e.event_queue.detach(agent_inbox_topic(handle), agent_inbox_group(handle))
    yield e
    await e.stop()


async def _open(engine: Engine, requester: str, target: str, brief: str) -> str:
    assert engine.a2a_service is not None
    return await engine.a2a_service.request_channel(requester, target, brief=brief)


def _wake(channel_id: str, requester: str, content: str) -> Event:
    return Event(
        type="a2a_request",
        source=requester,
        payload={
            "channel_id": channel_id,
            "requester": requester,
            "content": content,
        },
    )


# ── the brief reaches the turn ───────────────────────────────────────


async def test_the_brief_on_the_event_reaches_the_prompt(engine: Engine) -> None:
    """Read from the payload, not from a per-channel queue.

    The queue lived on the node that opened the channel; the wake is
    delivered to the node that owns the target's seat. Cross-node those
    differ, and the agent was told nobody had said anything yet.
    """
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None
    channel_id = await _open(engine, "ceo", "cto", "how big is the migration?")

    captured: dict[str, Any] = {}

    async def _capture(agent: Any, **kwargs: Any) -> str:
        captured.update(kwargs)
        return "about two weeks"

    engine.turn_engine.run_turn = _capture  # type: ignore[method-assign]
    await engine._handle_a2a(cto, _wake(channel_id, "ceo", "how big is the migration?"))

    assert "how big is the migration?" in captured["task_description"]
    assert captured["a2a_context"]["channel_id"] == channel_id
    assert captured["a2a_context"]["responder"] is True


# ── the reply ────────────────────────────────────────────────────────


async def test_the_answering_turns_text_is_delivered_as_the_reply(
    engine: Engine,
) -> None:
    """The whole point, and the thing that was missing."""
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None
    channel_id = await _open(engine, "ceo", "cto", "how big?")

    replies: list[Event] = []

    async def handler(event: Event) -> None:
        replies.append(event)

    await engine.event_queue.subscribe("crewlet.agent.ceo.inbox", "test-reply", handler)

    async def _answer(agent: Any, **kwargs: Any) -> str:
        return "about two weeks"

    engine.turn_engine.run_turn = _answer  # type: ignore[method-assign]
    await engine._handle_a2a(cto, _wake(channel_id, "ceo", "how big?"))

    assert [e.payload["content"] for e in replies] == ["about two weeks"]
    assert replies[0].payload["sender"] == "cto"


async def test_the_reply_carries_the_original_question(engine: Engine) -> None:
    """The asker's turn ended when it asked, and nothing rehydrates it.

    Without the echo the woken turn gets an answer with no record of the
    question — and this is the one wake path with no external surface to
    re-read it from.
    """
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None
    channel_id = await _open(engine, "ceo", "cto", "how big is the migration?")

    replies: list[Event] = []

    async def handler(event: Event) -> None:
        replies.append(event)

    await engine.event_queue.subscribe("crewlet.agent.ceo.inbox", "test-echo", handler)

    async def _answer(agent: Any, **kwargs: Any) -> str:
        return "about two weeks"

    engine.turn_engine.run_turn = _answer  # type: ignore[method-assign]
    await engine._handle_a2a(cto, _wake(channel_id, "ceo", "how big is the migration?"))

    assert replies[0].payload["question"] == "how big is the migration?"


async def test_the_reply_stays_in_the_asks_trace(engine: Engine) -> None:
    """One exchange, one trace.

    An A2A event is not an LLM call and never renders one — the detail
    view dispatches on type, and the only bridge from an A2A event to
    the turns around it is its trace link. The reply is published from
    ``_answer_a2a``, which runs after the answering turn's phase spans
    have closed, and the engine opens no span of its own — so the
    ambient trace is empty and the reply used to carry none at all,
    leaving that link pointing nowhere.
    """
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None
    channel_id = await _open(engine, "ceo", "cto", "how big?")

    replies: list[Event] = []

    async def handler(event: Event) -> None:
        replies.append(event)

    await engine.event_queue.subscribe("crewlet.agent.ceo.inbox", "test-trace", handler)

    async def _answer(agent: Any, **kwargs: Any) -> str:
        return "about two weeks"

    engine.turn_engine.run_turn = _answer  # type: ignore[method-assign]

    ask = _wake(channel_id, "ceo", "how big?")
    # A real ask is published from inside the asker's Execute phase, so
    # it carries that turn's trace. Nothing downstream can invent one.
    ask.trace_id = "0af7651916cd43dd8448eb211c80319c"
    ask.span_id = "b7ad6b7169203331"

    await engine._handle_a2a(cto, ask)

    assert replies, "the reply never reached the asker"
    assert replies[0].trace_id == ask.trace_id
    # Verbatim, so ``restore_context`` makes the woken turn a child of
    # the span that caused it rather than a sibling of it.
    assert replies[0].span_id == ask.span_id


async def test_the_asker_is_shown_what_it_asked(engine: Engine) -> None:
    """The echo has to reach the PROMPT, not just the payload."""
    ceo = engine.agent_pool.get_by_handle("ceo")
    assert ceo is not None
    channel_id = await _open(engine, "ceo", "cto", "how big is the migration?")

    captured: dict[str, Any] = {}

    async def _capture(agent: Any, **kwargs: Any) -> str:
        captured.update(kwargs)
        return ""

    engine.turn_engine.run_turn = _capture  # type: ignore[method-assign]
    await engine._handle_a2a(
        ceo,
        Event(
            type="a2a_message",
            source="cto",
            payload={
                "channel_id": channel_id,
                "sender": "cto",
                "content": "about two weeks",
                "question": "how big is the migration?",
            },
        ),
    )

    task = captured["task_description"]
    assert "You asked:" in task
    assert "how big is the migration?" in task
    assert "about two weeks" in task


async def test_the_responder_is_not_shown_an_echo(engine: Engine) -> None:
    """The responder is reading the ask itself — quoting it back would
    print the same text twice."""
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None
    channel_id = await _open(engine, "ceo", "cto", "how big?")

    captured: dict[str, Any] = {}

    async def _capture(agent: Any, **kwargs: Any) -> str:
        captured.update(kwargs)
        return "two weeks"

    engine.turn_engine.run_turn = _capture  # type: ignore[method-assign]
    await engine._handle_a2a(cto, _wake(channel_id, "ceo", "how big?"))

    assert "You asked:" not in captured["task_description"]


async def test_the_channel_closes_after_one_round_trip(engine: Engine) -> None:
    """Ask, answer, done. Leaving it open would be a promise of a second
    reply that nothing in the design ever sends."""
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None
    channel_id = await _open(engine, "ceo", "cto", "how big?")

    engine.turn_engine.run_turn = AsyncMock(return_value="two weeks")  # type: ignore[assignment]
    await engine._handle_a2a(cto, _wake(channel_id, "ceo", "how big?"))

    assert engine.a2a_service is not None
    channel = await engine.a2a_service.channels.get(channel_id)
    assert channel is not None and not channel.is_open


async def test_the_requester_does_not_answer_the_answer(engine: Engine) -> None:
    """The guard against a volley.

    The requester's turn is itself triggered by the reply. If it
    answered too, two agents would bounce a conversation between them
    until the delegation cap stopped it — burning a turn's worth of
    tokens on each hop.
    """
    ceo = engine.agent_pool.get_by_handle("ceo")
    assert ceo is not None
    channel_id = await _open(engine, "ceo", "cto", "how big?")

    sent: list[Event] = []

    async def handler(event: Event) -> None:
        sent.append(event)

    await engine.event_queue.subscribe(
        "crewlet.agent.cto.inbox", "test-volley", handler
    )
    sent.clear()

    engine.turn_engine.run_turn = AsyncMock(return_value="thanks!")  # type: ignore[assignment]
    await engine._handle_a2a(
        ceo,
        Event(
            type="a2a_message",
            source="cto",
            payload={
                "channel_id": channel_id,
                "sender": "cto",
                "content": "about two weeks",
            },
        ),
    )

    assert sent == [], "the requester answered its own answer"


async def test_an_empty_answer_closes_without_sending(engine: Engine) -> None:
    """A turn that produced nothing has nothing to deliver, but the
    channel must not be left open on the strength of it."""
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None
    channel_id = await _open(engine, "ceo", "cto", "how big?")

    replies: list[Event] = []

    async def handler(event: Event) -> None:
        replies.append(event)

    await engine.event_queue.subscribe("crewlet.agent.ceo.inbox", "test-empty", handler)

    engine.turn_engine.run_turn = AsyncMock(return_value="   ")  # type: ignore[assignment]
    await engine._handle_a2a(cto, _wake(channel_id, "ceo", "how big?"))

    assert replies == []
    assert engine.a2a_service is not None
    channel = await engine.a2a_service.channels.get(channel_id)
    assert channel is not None and not channel.is_open


# ── refusals ─────────────────────────────────────────────────────────


async def test_a_wake_for_a_closed_channel_runs_no_turn(engine: Engine) -> None:
    """For the RESPONDER. Its turn would produce an answer with nowhere
    to go: closed means the conversation ended — the reply already
    shipped, or the idle sweep gave up on it."""
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None
    channel_id = await _open(engine, "ceo", "cto", "how big?")
    assert engine.a2a_service is not None
    await engine.a2a_service.close_channel(channel_id)

    engine.turn_engine.run_turn = AsyncMock()  # type: ignore[assignment]
    await engine._handle_a2a(cto, _wake(channel_id, "ceo", "how big?"))
    engine.turn_engine.run_turn.assert_not_called()


async def test_the_answer_still_reaches_the_asker_after_the_close(
    engine: Engine,
) -> None:
    """The requester's hop is NOT gated on the channel being open.

    The responder closes the channel immediately after replying, so by
    the time the reply reaches the asker the channel is closed — every
    single time. Refusing it there drops the answer on the floor, which
    is the exact failure A2A had when it had no reply path at all: the
    asker ends its turn expecting to be woken, and never is.

    In-process the close raced the delivery and lost, so the round trip
    looked fine on the memory twin; on a real broker the consume loop is
    a separate task and the close wins.
    """
    ceo = engine.agent_pool.get_by_handle("ceo")
    assert ceo is not None
    channel_id = await _open(engine, "ceo", "cto", "how big?")
    assert engine.a2a_service is not None
    await engine.a2a_service.close_channel(channel_id, closer="cto")

    engine.turn_engine.run_turn = AsyncMock(return_value="noted")  # type: ignore[assignment]
    await engine._handle_a2a(
        ceo,
        Event(
            type="a2a_message",
            source="cto",
            payload={
                "channel_id": channel_id,
                "sender": "cto",
                "content": "about two weeks",
            },
        ),
    )
    engine.turn_engine.run_turn.assert_called_once()
    prompt = engine.turn_engine.run_turn.call_args.kwargs["task_description"]
    assert "about two weeks" in prompt


async def test_a_wake_for_an_unknown_channel_runs_no_turn(engine: Engine) -> None:
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None
    engine.turn_engine.run_turn = AsyncMock()  # type: ignore[assignment]
    await engine._handle_a2a(cto, _wake("a2a-never-opened", "ceo", "hi"))
    engine.turn_engine.run_turn.assert_not_called()


async def test_handle_a2a_missing_channel_id(engine: Engine) -> None:
    """Bail if channel_id is missing from the payload."""
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None

    engine.turn_engine.run_turn = AsyncMock()  # type: ignore[assignment]
    await engine._handle_a2a(
        cto, Event(type="a2a_request", source="ceo", payload={"requester": "ceo"})
    )
    engine.turn_engine.run_turn.assert_not_called()


async def test_a_failed_reply_does_not_propagate(engine: Engine) -> None:
    """The turn already happened. A delivery failure must not make the
    trigger look unhandled and replay the whole thing."""
    cto = engine.agent_pool.get_by_handle("cto")
    assert cto is not None
    channel_id = await _open(engine, "ceo", "cto", "how big?")

    async def _boom(*_a: Any, **_k: Any) -> None:
        raise RuntimeError("broker is down")

    assert engine.a2a_service is not None
    engine.a2a_service.reply = _boom  # type: ignore[method-assign]
    engine.turn_engine.run_turn = AsyncMock(return_value="two weeks")  # type: ignore[assignment]

    await engine._handle_a2a(cto, _wake(channel_id, "ceo", "how big?"))

    # And the channel is still closed, so it cannot leak.
    channel = await engine.a2a_service.channels.get(channel_id)
    assert channel is not None and not channel.is_open


# ── the completion ledger ────────────────────────────────────────────


async def test_a_redelivered_reply_does_not_run_the_turn_twice(
    org: Organization,
) -> None:
    """A2A is on the completion ledger now, and this hop needs it.

    The exemption existed because ``_handle_a2a`` drained a process-local
    queue: a re-run found it empty and told the agent nobody had said
    anything, so neither branch of short-circuit-or-re-run was honest.
    The content rides the durable wake now, so a redelivery is a genuine
    re-run — which is exactly what has to be suppressed once the turn has
    already acted on the answer.

    The responder's hop is guarded twice over (it closes the channel, and
    a closed channel refuses a second answer). The requester's is not
    guarded at all: it lands on an already-closed channel by design.
    """
    from crewlet.db.turn_completions import MemoryTurnCompletionStore
    from crewlet.queue.topics import agent_inbox_topic

    engine = Engine(
        organization=org,
        llm_providers={"default": _MockLLM()},
        turn_completion_store=MemoryTurnCompletionStore(),
    )
    await engine.start()
    try:
        assert engine.a2a_service is not None
        channel_id = await engine.a2a_service.request_channel(
            "ceo", "cto", brief="how big?"
        )
        await engine.a2a_service.close_channel(channel_id, closer="cto")

        turns: list[str] = []

        async def _count(agent: Any, **kwargs: Any) -> str:
            turns.append(agent.handle)
            return ""

        engine.turn_engine.run_turn = _count  # type: ignore[method-assign]

        reply = Event(
            type="a2a_message",
            source="cto",
            payload={
                "channel_id": channel_id,
                "sender": "cto",
                "content": "about two weeks",
            },
        )
        # The same delivery twice — one message, acked late.
        await engine.event_queue.publish(agent_inbox_topic("ceo"), reply)
        await engine.event_queue.publish(agent_inbox_topic("ceo"), reply)

        assert turns == ["ceo"], "the asker acted on the same answer twice"
    finally:
        await engine.stop()


# ── end to end ───────────────────────────────────────────────────────


async def test_the_whole_round_trip_runs_without_help(org: Organization) -> None:
    """Ask, wake, answer, reply, close — driven by nothing but a publish.

    Every other test here detaches the seat consumers so it can inspect
    one hop. This one leaves them attached and asserts the loop closes
    on its own, which is what an agent calling ``a2a_ask`` actually
    experiences — and what did not happen before, because the answer had
    no way back.
    """
    engine = Engine(organization=org, llm_providers={"default": _MockLLM()})
    await engine.start()
    try:
        answers: list[str] = []

        async def _answer(agent: Any, **kwargs: Any) -> str:
            reply = f"{agent.handle} says: two weeks"
            answers.append(reply)
            return reply

        engine.turn_engine.run_turn = _answer  # type: ignore[method-assign]

        assert engine.a2a_service is not None
        channel_id = await engine.a2a_service.request_channel(
            "ceo", "cto", brief="how big is the migration?"
        )

        # The CTO answered, the CEO was woken with it, and the CEO did
        # NOT answer back — two turns, not an unbounded volley.
        assert answers == ["cto says: two weeks", "ceo says: two weeks"]
        channel = await engine.a2a_service.channels.get(channel_id)
        assert channel is not None and not channel.is_open
    finally:
        await engine.stop()
