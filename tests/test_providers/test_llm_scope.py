"""The seat scope stateful LLM backends resolve their workspace from."""

from __future__ import annotations

import asyncio

import pytest

from crewlet.providers.llm.scope import (
    ENGINE_SEAT,
    bind_llm_scope,
    current_scope,
    current_seat,
    sanitize_seat,
)


class TestBinding:
    def test_default_is_the_engine_seat(self):
        assert current_scope().seat == ENGINE_SEAT

    def test_binding_and_restoring(self):
        with bind_llm_scope("sarah-chen", phase="plan"):
            assert current_scope().seat == "sarah-chen"
            assert current_scope().phase == "plan"
        assert current_scope().seat == ENGINE_SEAT

    def test_restores_on_exception(self):
        with pytest.raises(RuntimeError), bind_llm_scope("sarah-chen"):
            raise RuntimeError("turn failed")
        assert current_scope().seat == ENGINE_SEAT

    def test_nesting(self):
        with bind_llm_scope("a"):
            with bind_llm_scope("b"):
                assert current_seat() == "b"
            assert current_seat() == "a"

    def test_empty_seat_binds_the_engine_seat(self):
        with bind_llm_scope(""):
            assert current_seat() == ENGINE_SEAT

    async def test_child_tasks_inherit_the_seat(self):
        """Batched sub-agents run as child tasks and belong to the seat
        that spawned them."""
        seen: list[str] = []

        async def child() -> None:
            seen.append(current_seat())

        with bind_llm_scope("sarah-chen"):
            await asyncio.gather(child(), child())
        assert seen == ["sarah-chen", "sarah-chen"]

    async def test_concurrent_turns_do_not_see_each_other(self):
        """Two seats' turns interleave on one event loop; each LLM call
        must resolve to its own seat or their CLI homes would cross."""
        observed: dict[str, str] = {}

        async def turn(seat: str, delay: float) -> None:
            with bind_llm_scope(seat):
                await asyncio.sleep(delay)
                observed[seat] = current_seat()

        await asyncio.gather(turn("alpha", 0.02), turn("beta", 0.01))
        assert observed == {"alpha": "alpha", "beta": "beta"}


class TestSanitize:
    @pytest.mark.parametrize(
        "raw,expected",
        [
            ("sarah-chen", "sarah-chen"),
            ("Sarah Chen", "sarah-chen"),
            ("VP/Engineering", "vp-engineering"),
            ("  spaced  ", "spaced"),
            ("under_score.dot", "under_score.dot"),
        ],
    )
    def test_normal_handles(self, raw, expected):
        assert sanitize_seat(raw) == expected

    @pytest.mark.parametrize("raw", ["", "..", "/", "///", "-", "."])
    def test_degenerate_input_falls_back(self, raw):
        """Returning "" would put the seat home at the workspace root,
        where every other seat could read it."""
        assert sanitize_seat(raw) == ENGINE_SEAT

    @pytest.mark.parametrize("raw", ["../../etc", "..%2f..", "a/../../b"])
    def test_traversal_cannot_survive(self, raw):
        cleaned = sanitize_seat(raw)
        assert "/" not in cleaned
        assert not cleaned.startswith(".")

    def test_long_handles_are_truncated(self):
        assert len(sanitize_seat("x" * 500)) == 64
