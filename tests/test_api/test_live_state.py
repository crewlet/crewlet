"""Unit tests for the in-memory live-state projection.

These cover the two things the projection exists for: keeping agent
state current without a per-read DB scan, and holding the *in-flight*
LLM call so it survives a dashboard refresh.
"""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.api.live_state import LiveState


def _env(
    etype: str,
    payload: dict[str, Any] | None = None,
    *,
    ts: str = "2026-06-14T12:00:00+00:00",
    event_id: str = "e1",
    category: str = "system",
) -> dict[str, Any]:
    """Build a serialized-event envelope (the shape ``apply_event`` reads)."""
    return {
        "id": event_id,
        "type": etype,
        "timestamp": ts,
        "category": category,
        "payload": payload or {},
    }


class TestAgentStateTransitions:
    def test_spawn_marks_idle_and_records_runtime_id(self) -> None:
        live = LiveState()
        live.apply_event(_env("agent_spawned", {"role": "Lead", "agent_id": "rt-1"}))
        overlay = live.agent_overlay("Lead")
        assert overlay is not None
        assert overlay["state"] == "idle"
        assert overlay["runtime_id"] == "rt-1"
        assert live.runtime_id_for("Lead") == "rt-1"

    def test_task_started_then_completed(self) -> None:
        live = LiveState()
        live.apply_event(
            _env("task_started", {"role": "Lead", "agent_id": "rt-1", "task_id": "T-1"})
        )
        overlay = live.agent_overlay("Lead")
        assert overlay["state"] == "working"
        assert overlay["current_task"] == "T-1"

        live.apply_event(
            _env(
                "task_completed",
                {"role": "Lead", "agent_id": "rt-1", "task_id": "T-1"},
                ts="2026-06-14T12:01:00+00:00",
            )
        )
        overlay = live.agent_overlay("Lead")
        assert overlay["state"] == "idle"
        assert overlay["current_task"] is None
        assert overlay["current_phase"] is None
        assert overlay["live_call"] is None

    def test_afk_reason_from_guard_breach_kind(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "turn.guard_breach",
                {"role": "Dev", "agent_id": "rt-2", "kind": "stall"},
            )
        )
        overlay = live.agent_overlay("Dev")
        assert overlay["state"] == "afk"
        assert overlay["afk_reason"] == "stall"

    def test_afk_reason_defaults_to_event_type(self) -> None:
        live = LiveState()
        live.apply_event(_env("llm_unavailable", {"role": "Dev", "agent_id": "rt-2"}))
        assert live.agent_overlay("Dev")["afk_reason"] == "llm_unavailable"

    def test_reflection_completed_returns_to_idle(self) -> None:
        live = LiveState()
        live.apply_event(_env("agent_phase_started", {"role": "Lead", "phase": "plan"}))
        live.apply_event(
            _env(
                "reflection_completed",
                {"role": "Lead"},
                ts="2026-06-14T12:05:00+00:00",
            )
        )
        overlay = live.agent_overlay("Lead")
        assert overlay["state"] == "idle"
        assert overlay["current_phase"] is None
        assert overlay["live_call"] is None

    def test_terminated(self) -> None:
        live = LiveState()
        live.apply_event(_env("agent_terminated", {"role": "Lead", "agent_id": "rt-1"}))
        assert live.agent_overlay("Lead")["state"] == "terminated"

    def test_reorder_guard_ignores_older_state_event(self) -> None:
        """A stale (older-timestamp) event must not clobber newer state.

        The standalone API receives events over Pulsar where ordering is
        only per-topic, so a ``task_completed`` can arrive after a newer
        ``task_started`` from a different topic.
        """
        live = LiveState()
        live.apply_event(
            _env(
                "task_started",
                {"role": "Lead", "agent_id": "rt-1", "task_id": "T-2"},
                ts="2026-06-14T12:10:00+00:00",
            )
        )
        # An OLDER task_completed (earlier timestamp) must be ignored.
        live.apply_event(
            _env(
                "task_completed",
                {"role": "Lead", "agent_id": "rt-1", "task_id": "T-1"},
                ts="2026-06-14T12:00:00+00:00",
            )
        )
        overlay = live.agent_overlay("Lead")
        assert overlay["state"] == "working"
        assert overlay["current_task"] == "T-2"


class TestInFlightCall:
    """The in-flight LLM call — must survive a tab refresh via the snapshot."""

    def test_phase_started_seeds_placeholder_live_call(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "agent_phase_started",
                {
                    "role": "Lead",
                    "agent_id": "rt-1",
                    "turn_id": "tn-1",
                    "phase": "execute",
                    "iteration": 0,
                },
            )
        )
        call = live.agent_overlay("Lead")["live_call"]
        assert call is not None
        assert call["phase"] == "execute"
        assert call["in_progress"] is True
        assert call["response"] == ""

    def test_live_call_carries_trigger_source(self) -> None:
        """The triggering event rides through ``agent_phase_started`` into
        the live_call, and survives the round-by-round progress overwrites
        (falling back to the placeholder's trigger when a round omits it)
        so a refresh mid-call shows the live row's source."""
        live = LiveState()
        trigger = {"id": "ev-1", "type": "task_assigned", "summary": "Do it"}
        live.apply_event(
            _env(
                "agent_phase_started",
                {
                    "role": "Lead",
                    "turn_id": "tn-1",
                    "phase": "execute",
                    "iteration": 0,
                    "trigger": trigger,
                },
            )
        )
        assert live.agent_overlay("Lead")["live_call"]["trigger"] == trigger
        # A progress round without its own trigger keeps the placeholder's.
        live.apply_event(
            _env(
                "agent_turn_progress",
                {
                    "role": "Lead",
                    "turn_id": "tn-1",
                    "phase": "execute",
                    "iteration": 0,
                    "response": "partial",
                    "round_num": 1,
                },
                category="",
            )
        )
        assert live.agent_overlay("Lead")["live_call"]["trigger"] == trigger

    def test_progress_updates_live_call(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "agent_phase_started",
                {"role": "Lead", "turn_id": "tn-1", "phase": "execute", "iteration": 0},
            )
        )
        live.apply_event(
            _env(
                "agent_turn_progress",
                {
                    "role": "Lead",
                    "agent_id": "rt-1",
                    "turn_id": "tn-1",
                    "phase": "execute",
                    "iteration": 0,
                    "model": "claude-sonnet-4-6",
                    "response": "partial answer so far",
                    "round_num": 1,
                    "tool_executions": [{"name": "jira_create_issue"}],
                    "input_tokens": 100,
                    "output_tokens": 20,
                    "total_tokens": 120,
                },
                category="",  # progress is never persisted / buffered
            )
        )
        call = live.agent_overlay("Lead")["live_call"]
        assert call["response"] == "partial answer so far"
        assert call["model"] == "claude-sonnet-4-6"
        assert call["rounds"] == 2  # round_num + 1
        assert call["tool_executions"] == [{"name": "jira_create_issue"}]
        # Progress is stream-only: it must NOT land in the events buffer
        # (the persisted agent_phase_started before it does).
        assert all(e["type"] != "agent_turn_progress" for e in live.recent_events())

    def test_phase_completed_clears_matching_live_call(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "agent_phase_started",
                {"role": "Lead", "turn_id": "tn-1", "phase": "plan", "iteration": 0},
            )
        )
        assert live.agent_overlay("Lead")["live_call"] is not None
        live.apply_event(
            _env(
                "agent_phase_completed",
                {"role": "Lead", "turn_id": "tn-1", "phase": "plan", "iteration": 0},
                ts="2026-06-14T12:00:05+00:00",
            )
        )
        assert live.agent_overlay("Lead")["live_call"] is None

    def test_phase_completed_for_other_phase_keeps_live_call(self) -> None:
        """A late completion for a *prior* phase must not wipe the row of
        the phase currently in flight."""
        live = LiveState()
        live.apply_event(
            _env(
                "agent_phase_started",
                {"role": "Lead", "turn_id": "tn-1", "phase": "execute", "iteration": 1},
            )
        )
        # Completion arrives for an earlier (plan) phase — different key.
        live.apply_event(
            _env(
                "agent_phase_completed",
                {"role": "Lead", "turn_id": "tn-1", "phase": "plan", "iteration": 0},
            )
        )
        assert live.agent_overlay("Lead")["live_call"] is not None
        assert live.agent_overlay("Lead")["live_call"]["phase"] == "execute"

    def test_stale_round_is_ignored(self) -> None:
        live = LiveState()
        base = {"role": "Lead", "turn_id": "tn-1", "phase": "execute", "iteration": 0}
        live.apply_event(
            _env(
                "agent_turn_progress",
                {**base, "round_num": 2, "response": "r2"},
                category="",
            )
        )
        live.apply_event(
            _env(
                "agent_turn_progress",
                {**base, "round_num": 1, "response": "r1"},
                category="",
            )
        )
        # The newer round (2) must win over the stale round (1).
        assert live.agent_overlay("Lead")["live_call"]["response"] == "r2"


class TestTokenAccounting:
    def test_turn_tokens_accumulate(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "agent_turn_completed",
                {
                    "role": "Lead",
                    "input_tokens": 100,
                    "output_tokens": 50,
                    "total_tokens": 150,
                },
                event_id="turn-1",
            )
        )
        live.apply_event(
            _env(
                "agent_turn_completed",
                {
                    "role": "Lead",
                    "input_tokens": 10,
                    "output_tokens": 5,
                    "total_tokens": 15,
                },
                event_id="turn-2",
            )
        )
        overlay = live.agent_overlay("Lead")
        assert overlay["total_tokens"] == 165
        assert overlay["input_tokens"] == 110

    def test_duplicate_turn_event_counted_once(self) -> None:
        live = LiveState()
        ev = _env(
            "agent_turn_completed",
            {
                "role": "Lead",
                "input_tokens": 100,
                "output_tokens": 50,
                "total_tokens": 150,
            },
            event_id="turn-1",
        )
        live.apply_event(ev)
        live.apply_event(ev)  # same event id → deduped
        assert live.agent_overlay("Lead")["total_tokens"] == 150


class TestEventBuffer:
    def test_persisted_events_recorded_newest_first(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "task_started",
                {"role": "Lead"},
                event_id="a",
                ts="2026-06-14T12:00:00+00:00",
                category="task",
            )
        )
        live.apply_event(
            _env(
                "task_completed",
                {"role": "Lead"},
                event_id="b",
                ts="2026-06-14T12:01:00+00:00",
                category="task",
            )
        )
        events = live.recent_events()
        assert [e["id"] for e in events] == ["b", "a"]

    def test_uncategorized_events_not_buffered(self) -> None:
        live = LiveState()
        live.apply_event(_env("provider_fallback", {"role": "Lead"}, category=""))
        assert live.recent_events() == []


class TestMergeAgents:
    def test_overlay_onto_static_rows(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "agent_phase_started",
                {"role": "Lead", "agent_id": "rt-1", "phase": "plan", "iteration": 2},
            )
        )
        static = [
            {"id": "a-1", "role": "Lead", "name": "Lead", "handle": "lead"},
            {"id": "a-2", "role": "Dev", "name": "Dev", "handle": "dev"},
        ]
        merged = {a["role"]: a for a in live.merge_agents(static)}
        assert merged["Lead"]["state"] == "working"
        assert merged["Lead"]["current_phase"] == "plan"
        assert merged["Lead"]["live_call"] is not None
        # Dev has no live entry → static row passes through unchanged.
        assert merged["Dev"]["name"] == "Dev"
        assert "state" not in merged["Dev"]


class _FakeStore:
    def __init__(self) -> None:
        self.states = {
            "Lead": {
                "state": "working",
                "runtime_id": "rt-1",
                "current_task": "T-9",
                "current_phase": "execute",
                "current_iteration": 3,
            },
        }
        self.token_events = [
            {
                "event_id": "t1",
                "agent_role": "Lead",
                "input_tokens": 100,
                "output_tokens": 50,
                "total_tokens": 150,
            },
        ]
        self.events = [
            {
                "id": "e2",
                "type": "task_started",
                "timestamp": "2026-06-14T12:01:00",
                "category": "task",
                "summary": "started",
            },
            {
                "id": "e1",
                "type": "agent_spawned",
                "timestamp": "2026-06-14T12:00:00",
                "category": "lifecycle",
                "summary": "spawned",
            },
        ]

    async def get_agent_states(self, roles: list[str]) -> dict[str, Any]:
        return {r: self.states[r] for r in roles if r in self.states}

    async def list_token_usage_events(self, **_: Any) -> list[dict[str, Any]]:
        return self.token_events

    async def list_events(self, **_: Any) -> list[dict[str, Any]]:
        return self.events


class TestHydration:
    async def test_hydrate_seeds_state_tokens_and_events(self) -> None:
        live = LiveState()
        await live.hydrate(_FakeStore(), ["Lead"])
        overlay = live.agent_overlay("Lead")
        assert overlay["state"] == "working"
        assert overlay["current_phase"] == "execute"
        assert overlay["total_tokens"] == 150
        # list_events is newest-first; the buffer returns it newest-first.
        assert [e["id"] for e in live.recent_events()] == ["e2", "e1"]

    async def test_hydrate_only_states_does_not_double_count_tokens(self) -> None:
        live = LiveState()
        store = _FakeStore()
        await live.hydrate(store, ["Lead"])
        await live.hydrate(store, ["Lead"], only_states=True)
        # Tokens hydrated once despite two hydrate calls.
        assert live.agent_overlay("Lead")["total_tokens"] == 150

    async def test_hydrate_tolerates_missing_store(self) -> None:
        live = LiveState()
        await live.hydrate(None, ["Lead"])  # no raise
        assert live.agent_overlay("Lead") is None


class TestSandboxProjection:
    """The in-flight detached-sandbox set behind the dashboard panel."""

    def test_started_then_completed(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "sandbox_run_started",
                {
                    "turn_id": "t1",
                    "role": "Eng",
                    "agent_id": "rt-1",
                    "agent_handle": "eng",
                    "coding_agent": "opencode",
                    "sandbox_id": "sb-1",
                    "task": "Build the thing",
                },
                event_id="s1",
            )
        )
        runs = live.active_sandboxes()
        assert len(runs) == 1
        r = runs[0]
        assert r["turn_id"] == "t1"
        assert r["role"] == "Eng"
        assert r["coding_agent"] == "opencode"
        assert r["task"] == "Build the thing"
        assert r["status"] == "running"
        assert r["started_at"]  # carries the envelope timestamp

        live.apply_event(
            _env("sandbox_run_completed", {"turn_id": "t1"}, event_id="s2")
        )
        assert live.active_sandboxes() == []

    def test_clarification_flips_to_awaiting_input(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "sandbox_run_started",
                {"turn_id": "t1", "role": "Eng", "coding_agent": "opencode"},
                event_id="s1",
            )
        )
        live.apply_event(
            _env(
                "sandbox_clarification_requested",
                {
                    "turn_id": "t1",
                    "role": "Eng",
                    "question": "Which repo?",
                    "audience": "requester",
                },
                event_id="s2",
            )
        )
        r = live.active_sandboxes()[0]
        assert r["status"] == "awaiting_input"
        assert r["question"] == "Which repo?"
        assert r["audience"] == "requester"

    def test_clarification_without_prior_start_synthesizes_entry(self) -> None:
        # The API process came up mid-run and missed the start event.
        live = LiveState()
        live.apply_event(
            _env(
                "sandbox_clarification_requested",
                {"turn_id": "t9", "role": "Eng", "question": "?"},
                event_id="s1",
            )
        )
        runs = live.active_sandboxes()
        assert len(runs) == 1
        assert runs[0]["turn_id"] == "t9"
        assert runs[0]["status"] == "awaiting_input"

    def test_active_sandboxes_oldest_first(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "sandbox_run_started",
                {"turn_id": "a", "role": "E1"},
                ts="2026-06-14T12:00:00+00:00",
                event_id="s1",
            )
        )
        live.apply_event(
            _env(
                "sandbox_run_started",
                {"turn_id": "b", "role": "E2"},
                ts="2026-06-14T12:05:00+00:00",
                event_id="s2",
            )
        )
        assert [r["turn_id"] for r in live.active_sandboxes()] == ["a", "b"]

    def test_started_without_turn_id_ignored(self) -> None:
        live = LiveState()
        live.apply_event(_env("sandbox_run_started", {"role": "Eng"}, event_id="s1"))
        assert live.active_sandboxes() == []


if __name__ == "__main__":  # pragma: no cover
    pytest.main([__file__, "-v"])
