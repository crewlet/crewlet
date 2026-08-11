"""Tests for TurnContext."""

from __future__ import annotations

from unittest.mock import MagicMock

from crewlet.agent.turn_context import PhaseBudget, TurnContext


def _stub_agent(handle: str = "sarah") -> MagicMock:
    agent = MagicMock()
    agent.handle = handle
    return agent


def test_turn_id_auto_generated():
    ctx = TurnContext(agent=_stub_agent(), org=None)
    assert ctx.turn_id  # non-empty
    ctx2 = TurnContext(agent=_stub_agent(), org=None)
    assert ctx.turn_id != ctx2.turn_id


def test_phase_budget_unlimited():
    b = PhaseBudget(limit=0)
    assert b.unlimited is True
    assert b.remaining == 0  # caller must check ``unlimited``


def test_phase_budget_remaining():
    b = PhaseBudget(limit=1000, used=200)
    assert b.unlimited is False
    assert b.remaining == 800


def test_phase_budget_negative_remaining_clamps_to_zero():
    b = PhaseBudget(limit=100, used=500)
    assert b.remaining == 0
