"""Tests for the TurnEngine live-reload settings cell."""

from __future__ import annotations

from crewlet.agent.turn_settings import TurnEngineSettings
from crewlet.config import TurnEngineConfig


def test_settings_default_construction() -> None:
    s = TurnEngineSettings()
    assert s.get().max_iterations == 3
    assert s.get().subagent_max_turns == 20


def test_settings_set_swaps_cfg_in_one_step() -> None:
    s = TurnEngineSettings(TurnEngineConfig(max_iterations=3))
    s.set(TurnEngineConfig(max_iterations=7))
    assert s.get().max_iterations == 7


def test_settings_get_returns_live_reference() -> None:
    """``get()`` returns the current config — successive calls see updates."""
    s = TurnEngineSettings()
    first = s.get()
    s.set(TurnEngineConfig(max_iterations=99))
    second = s.get()
    assert first.max_iterations != second.max_iterations
    assert second.max_iterations == 99


def test_turn_engine_reads_through_settings_cell() -> None:
    """TurnEngine.@property accessors delegate to the settings cell."""
    from unittest.mock import MagicMock

    from crewlet.agent.turn import TurnEngine

    settings = TurnEngineSettings(TurnEngineConfig(max_iterations=5))
    engine = TurnEngine(
        llm_providers={},
        tool_registry=MagicMock(),
        event_queue=MagicMock(),
        settings=settings,
    )
    assert engine._max_iterations == 5
    # Live swap: next access returns the new value.
    settings.set(TurnEngineConfig(max_iterations=11))
    assert engine._max_iterations == 11
    assert engine._max_tool_rounds == TurnEngineConfig().max_tool_rounds


def test_turn_engine_accepts_scalar_kwargs() -> None:
    """Scalar kwargs are accepted as an alternative to a settings object."""
    from unittest.mock import MagicMock

    from crewlet.agent.turn import TurnEngine

    engine = TurnEngine(
        llm_providers={},
        tool_registry=MagicMock(),
        event_queue=MagicMock(),
        max_iterations=42,
        subagent_max_turns=99,
    )
    assert engine._max_iterations == 42
    assert engine._subagent_max_turns == 99
