"""The work-key grammar — one identity for the unit of work a turn did.

The bug this closes is the least visible one the design admits to. A
doubled Slack post is obvious and a human deletes it; a doubled
*episode* is retrieved by every later turn's recall and feeds skill
synthesis, quietly weighting an agent's behaviour with something that
happened once.

Two paths produce that duplicate, and neither is a defect on its own:
a zombie whose loop stalls past its lease and finishes between fence
checks, and a genuine re-run after the completion ledger fails open —
which it does deliberately, because not knowing whether work was done
has exactly one safe answer.
"""

from __future__ import annotations

import asyncio
import re
from pathlib import Path

from crewlet.work_key import bind_work_key, current_work_key, derive_work_key

_SRC = Path(__file__).resolve().parents[1] / "src" / "crewlet"


# ── the grammar ──────────────────────────────────────────────────────


def test_the_same_triggers_give_the_same_key() -> None:
    """The whole point: two nodes running one trigger agree."""
    assert derive_work_key(["a", "b"]) == derive_work_key(["a", "b"])


def test_delivery_order_does_not_change_the_key() -> None:
    """Two nodes draining one partition can see a different order for
    the same set, and an order-sensitive key would then differ between
    exactly the two writers it exists to reconcile."""
    assert derive_work_key(["b", "a", "c"]) == derive_work_key(["a", "c", "b"])


def test_a_repeated_id_does_not_change_the_key() -> None:
    """At-least-once delivery can hand the same id over twice."""
    assert derive_work_key(["a", "a", "b"]) == derive_work_key(["a", "b"])


def test_different_trigger_sets_give_different_keys() -> None:
    """The partial-overlap case the ledger produces: a redelivery of
    A+B then A+B+C skips A and B and runs C alone. Two different units
    of work, so two episodes — not one."""
    assert derive_work_key(["a", "b"]) != derive_work_key(["c"])


def test_no_triggers_is_an_empty_key_not_a_hash_of_nothing() -> None:
    """A turn with no ledgerable trigger — a scheduled fire, a
    sub-agent, a sandbox resume. Hashing the empty set would give every
    one of them the SAME key, and they would then deduplicate against
    each other: a scheduled job would write one episode ever."""
    assert derive_work_key([]) == ""
    assert derive_work_key(["", ""]) == ""


def test_the_key_is_short_enough_to_read_and_long_enough_to_trust() -> None:
    key = derive_work_key(["a"])
    assert re.fullmatch(r"[0-9a-f]{32}", key), key


# ── the contextvar ───────────────────────────────────────────────────


def test_unbound_is_empty_rather_than_an_error() -> None:
    """Every write path reads this, including ones that legitimately run
    outside a dispatch. Raising there would turn a missing optimisation
    into a broken turn."""
    assert current_work_key() == ""


def test_binding_scopes_to_the_block() -> None:
    with bind_work_key("abc"):
        assert current_work_key() == "abc"
    assert current_work_key() == ""


def test_binding_restores_the_outer_value_not_the_empty_one() -> None:
    with bind_work_key("outer"), bind_work_key("inner"):
        assert current_work_key() == "inner"
    with bind_work_key("outer"):
        assert current_work_key() == "outer"


async def test_the_key_reaches_a_task_started_inside_the_block() -> None:
    """The property the whole transport depends on: the episode write
    happens several frames and at least one task down from the dispatch
    that bound it."""
    seen: list[str] = []

    async def _deep() -> None:
        await asyncio.sleep(0)
        seen.append(current_work_key())

    with bind_work_key("k"):
        await asyncio.create_task(_deep())
    assert seen == ["k"]


# ── one implementation ───────────────────────────────────────────────


def test_nothing_else_hashes_a_trigger_set_into_a_key() -> None:
    """A second implementation of this grammar would not raise — the two
    writers would just quietly record two of everything, which is the
    exact bug being fixed. Same guard ``tests/test_queue/test_topics``
    and ``tests/test_env_refs`` apply to their own grammars.
    """
    offenders: list[str] = []
    for path in _SRC.rglob("*.py"):
        if path.name == "work_key.py":
            continue
        text = path.read_text()
        # A sha over a joined id list is what this module is for.
        for match in re.finditer(r"hashlib\.sha256\((.{0,120})", text, re.S):
            snippet = match.group(1)
            if "join" in snippet and ("event" in snippet or "trigger" in snippet):
                offenders.append(f"{path.relative_to(_SRC)}: {snippet[:60]}")
    assert not offenders, (
        "these hash a trigger set into a key of their own; call "
        "crewlet.work_key.derive_work_key instead:\n" + "\n".join(offenders)
    )


# ── the plumbing ─────────────────────────────────────────────────────


class TestTheEngineBindsIt:
    """A key that is never bound disables the whole mechanism silently.

    Nothing raises: every episode simply lands with ``work_key = ''``,
    the guard skips, and duplicates come back — with the store's own
    tests still green, because they set the key themselves. So the bind
    is asserted here, at the seam where it actually happens.
    """

    @staticmethod
    async def _engine(monkeypatch):
        import sys

        sys.path.insert(0, str(Path(__file__).resolve().parent))
        from conftest import make_company, make_engine
        from crewlet.config import LLMProviderConfig, ProvidersConfig
        from crewlet.queue.memory import MemoryEventQueue

        # A provider has to exist for the engine to build a TurnEngine
        # at all; it is never called (run_turn is replaced below).
        monkeypatch.setenv("OPENAI_API_KEY", "test-key-for-construction")
        engine = await make_engine(
            company=make_company(
                roles=[{"name": "CEO", "handle": "ceo", "goal": "lead"}],
                providers=ProvidersConfig(
                    llm={
                        "default": LLMProviderConfig(type="openai", model="gpt-4o-mini")
                    }
                ),
            ),
            event_queue=MemoryEventQueue(),
        )
        await engine.start()
        return engine

    async def test_a_dispatched_trigger_binds_a_key(self, monkeypatch) -> None:
        from crewlet.events.types import Event

        engine = await self._engine(monkeypatch)
        seen: list[str] = []
        try:
            agent = engine.agent_pool.get_by_handle("ceo")
            assert agent is not None

            async def _capture(*_a, **kw) -> str:
                seen.append(current_work_key())
                return "ok"

            engine.turn_engine.run_turn = _capture  # type: ignore[method-assign]
            event = Event(
                type="task_assigned",
                source="test",
                payload={"task_id": "t1", "task_description": "do it"},
            )
            await engine._make_agent_handler(agent)([event])
        finally:
            await engine.stop()

        assert seen, "the turn never ran"
        assert seen[0] == derive_work_key([str(event.id)]), (
            "the dispatch did not bind the trigger's work key, so every "
            "episode it writes is unkeyed and never deduplicated"
        )

    async def test_the_key_is_unbound_again_after_the_dispatch(
        self, monkeypatch
    ) -> None:
        """It is ambient state; leaking it would tag the NEXT turn's
        episode with the previous turn's identity, and the guard would
        then throw that episode away as a duplicate."""
        from crewlet.events.types import Event

        engine = await self._engine(monkeypatch)
        try:
            agent = engine.agent_pool.get_by_handle("ceo")
            engine.turn_engine.run_turn = lambda *a, **kw: _done()  # type: ignore
            await engine._make_agent_handler(agent)(
                [Event(type="task_assigned", source="t", payload={"task_id": "x"})]
            )
            assert current_work_key() == ""
        finally:
            await engine.stop()


async def _done() -> str:
    return "ok"


def test_every_deferral_records_the_edge_that_resumes_it() -> None:
    """A `DeferDelivery` quiesces the seat's consumer, and the resume
    hook is edge-triggered on a set the deferral must enter.

    Miss the call on one path and that path's seats go permanently deaf
    on a healthy node — silently, with the lease still held and still
    renewed. It shipped that way on the ownership branch, and again on
    the `SeatLost` branch after the first was fixed, because nothing
    tied the two together. This does.

    Structural rather than behavioural on purpose: a new defer path is
    exactly the thing a behavioural test cannot know to cover.
    """
    engine_src = (_SRC / "engine.py").read_text().splitlines()
    misses: list[str] = []
    for i, line in enumerate(engine_src):
        if "raise DeferDelivery(" not in line:
            continue
        # The bookkeeping sits just above the raise, inside the same
        # branch. Twelve lines is generous for a comment plus the call.
        window = "\n".join(engine_src[max(0, i - 12) : i])
        if "note_delivery_deferred" not in window:
            misses.append(f"engine.py:{i + 1}: {line.strip()}")
    assert not misses, (
        "these deferrals quiesce a consumer without recording the edge "
        "that resumes it, so the seat goes deaf on a healthy node:\n"
        + "\n".join(misses)
    )
