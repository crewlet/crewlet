"""In-turn seat fencing.

The honest property this buys is **bounded duplication**, not none.
Epoch-fenced database writes protect database state and nothing else: by
the time an agent has posted to Slack or commented on a Jira issue the
effect is outside any transaction, and no fence can recall it.
``run_sandbox`` makes the limit vivid — it acquires a real, billed E2B
box before its row is written.

What the in-turn check buys is a bound on how far a node that lost the
seat gets before it notices: one round, or one tool call. Without it,
nothing bounds that at all — a zombie runs a whole turn beside the
seat's new owner.
"""

from __future__ import annotations

import pytest

from crewlet.agent.llm_loop import _is_known_read, run_tool_loop
from crewlet.agent.turn import SeatLost
from crewlet.providers.llm.protocol import Completion, Message, ToolCall
from crewlet.queue.memory import MemoryEventQueue
from crewlet.tools.capabilities import ToolAnnotations
from crewlet.tools.protocol import AgentContext, ToolResult


class _Tool:
    def __init__(self, name: str, annotations: ToolAnnotations | None = None) -> None:
        self.name = name
        self.description = name
        self.parameters: dict = {"type": "object", "properties": {}}
        if annotations is not None:
            self.annotations = annotations

    async def execute(self, arguments: dict, context) -> ToolResult:
        return ToolResult(output="ok")


class _Surface:
    phase = "execute"

    def __init__(self, tools: list[_Tool]) -> None:
        self._tools = {t.name: t for t in tools}
        self._registry = None
        self.calls: list[str] = []

    def to_tool_defs(self) -> list[dict]:
        return [
            {"name": t.name, "description": t.description, "parameters": t.parameters}
            for t in self._tools.values()
        ]

    def lookup(self, name: str):
        return self._tools.get(name)


class _Call:
    def __init__(self, name: str) -> None:
        self.id = f"call-{name}"
        self.name = name
        self.arguments = {}


def _completion(tool_calls: list[_Call], content: str = "") -> Completion:
    return Completion(
        content=content,
        tool_calls=[
            ToolCall(id=c.id, name=c.name, arguments=c.arguments) for c in tool_calls
        ],
    )


class _Provider:
    model = "test-model"

    def __init__(self, script: list[Completion]) -> None:
        self._script = list(script)
        self.rounds = 0

    async def complete(self, **kwargs) -> Completion:
        self.rounds += 1
        return self._script.pop(0) if self._script else _completion([], "done")


class _Agent:
    id_str = "aid"
    handle = "eng"
    role_name = "Engineer"
    input_tokens = 0
    output_tokens = 0


async def _run(provider, surface, fence, *, max_rounds: int = 4):
    queue = MemoryEventQueue()
    await queue.start()
    try:
        return await run_tool_loop(
            provider=provider,
            messages=[Message(role="user", content="go")],
            surface=surface,
            context=AgentContext(agent_handle="eng", seat_fence=fence),
            agent=_Agent(),
            max_rounds=max_rounds,
            event_queue=queue,
        )
    finally:
        await queue.stop()


# ── the round fence ──────────────────────────────────────────────────


async def test_a_lost_seat_stops_the_loop_before_spending_a_token() -> None:
    """The cheapest possible point: no LLM call has been made this round
    and no tool has fired."""

    def _lost() -> None:
        raise SeatLost("lease moved")

    provider = _Provider([])
    with pytest.raises(SeatLost):
        await _run(provider, _Surface([]), _lost)
    assert provider.rounds == 0, "an LLM round ran after the seat was lost"


async def test_the_fence_is_re_checked_every_round() -> None:
    """One round is the bound. A check taken once at turn start would
    let a turn that started legitimately run to completion beside its
    successor."""
    state = {"rounds": 0}

    def _lost_after_one() -> None:
        state["rounds"] += 1
        if state["rounds"] > 1:
            raise SeatLost("lease moved mid-turn")

    surface = _Surface([_Tool("read_thing", ToolAnnotations(read_only=True))])
    provider = _Provider(
        [_completion([_Call("read_thing")]), _completion([_Call("read_thing")])]
    )
    with pytest.raises(SeatLost):
        await _run(provider, surface, _lost_after_one)
    assert provider.rounds == 1


async def test_no_fence_configured_runs_normally() -> None:
    """Single-node embeds and tests have no seat to lose."""
    surface = _Surface([_Tool("thing")])
    provider = _Provider([_completion([_Call("thing")]), _completion([], "done")])
    result = await _run(provider, surface, None)
    assert result.rounds_used == 2


# ── the per-tool fence ───────────────────────────────────────────────


async def test_a_write_tool_is_fenced_within_the_round() -> None:
    """The finest-grained gate, and the last point before an external
    side effect."""
    calls = {"n": 0}

    def _lost_at_the_tool() -> None:
        calls["n"] += 1
        if calls["n"] > 1:  # round check passes, tool check fails
            raise SeatLost("lease moved")

    fired: list[str] = []

    class _Recording(_Tool):
        async def execute(self, arguments, context):
            fired.append(self.name)
            return ToolResult(output="ok")

    surface = _Surface([_Recording("post_message")])
    provider = _Provider([_completion([_Call("post_message")])])
    with pytest.raises(SeatLost):
        await _run(provider, surface, _lost_at_the_tool)
    assert fired == [], "a write fired after the seat was lost"


async def test_a_known_read_is_not_fenced_per_call() -> None:
    """Refusing a read buys nothing — it produces no duplicate effect —
    and costs the turn."""
    seen: list[int] = []

    def _count() -> None:
        seen.append(1)

    surface = _Surface([_Tool("search", ToolAnnotations(read_only=True))])
    provider = _Provider([_completion([_Call("search")]), _completion([], "done")])
    await _run(provider, surface, _count)
    # Two rounds, two round-fences, and NO extra check for the read.
    assert len(seen) == 2


async def test_an_unannotated_tool_is_fenced() -> None:
    """Unknown is fenced, deliberately.

    ``writes_to_shared_surface`` returns False for an all-unknown
    annotation set — right for its own callers, exactly wrong here.
    Most builtins carry no annotations at all, so treating
    "not classified" as "safe" would leave the majority of the outbound
    surface unfenced.
    """
    seen: list[int] = []

    def _count() -> None:
        seen.append(1)

    surface = _Surface([_Tool("mystery")])
    provider = _Provider([_completion([_Call("mystery")]), _completion([], "done")])
    await _run(provider, surface, _count)
    # Round, tool, round = 3.
    assert len(seen) == 3


def test_is_known_read_requires_an_explicit_annotation() -> None:
    surface = _Surface(
        [
            _Tool("reader", ToolAnnotations(read_only=True)),
            _Tool("writer", ToolAnnotations(read_only=False)),
            _Tool("unknown"),
        ]
    )
    assert _is_known_read("reader", surface) is True
    assert _is_known_read("writer", surface) is False
    assert _is_known_read("unknown", surface) is False
    assert _is_known_read("absent", surface) is False
