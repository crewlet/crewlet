"""Unit tests for the in-memory live-state projection.

These cover the two things the projection exists for: keeping agent
state current without a per-read DB scan, and holding the *in-flight*
LLM call so it survives a dashboard refresh.
"""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.api import live_state
from crewlet.api.live_state import LiveState
from crewlet.events.types import FAILURE_EVENT_TYPES


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

    def test_feed_row_carries_the_failure_flag(self) -> None:
        """A feed row says whether the work it reports failed.

        The dashboard renders failed rows in red and filters on them, and
        the buffer is payload-free -- so without this the only way to
        tell a dead turn from a live one was to read the prose.
        """
        live = LiveState()
        live.apply_event(
            _env(
                "agent_phase_completed",
                {"role": "Lead", "phase": "execute", "failed": True},
                event_id="dead",
            )
        )
        live.apply_event(
            _env(
                "agent_phase_completed",
                {"role": "Lead", "phase": "review"},
                event_id="ok",
                ts="2026-06-14T12:01:00+00:00",
            )
        )
        flags = {e["id"]: e["failed"] for e in live.recent_events()}
        assert flags == {"dead": True, "ok": False}

    def test_failure_by_event_type_needs_no_payload_flag(self) -> None:
        """Some events ARE the failure; they carry no ``failed`` field."""
        live = LiveState()
        for i, etype in enumerate(sorted(FAILURE_EVENT_TYPES)):
            live.apply_event(
                _env(
                    etype,
                    {"role": "Lead"},
                    event_id=f"f{i}",
                    ts=f"2026-06-14T12:0{i}:00+00:00",
                )
            )
        assert all(e["failed"] for e in live.recent_events())


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
            {
                "id": "e0",
                "type": "agent_phase_completed",
                "timestamp": "2026-06-14T11:59:00",
                "category": "system",
                "summary": "execute failed",
                # Every store row decides its own ``failed`` -- from the
                # tag the writer stamps, since ``list_events`` never
                # selects the payload column.
                "failed": True,
            },
        ]

    async def get_agent_states(self, roles: list[str]) -> dict[str, Any]:
        return {r: self.states[r] for r in roles if r in self.states}

    async def list_token_usage_events(self, **_: Any) -> list[dict[str, Any]]:
        return self.token_events

    async def list_events(self, **_: Any) -> list[dict[str, Any]]:
        return self.events


class TestBudgetMeter:
    """The live token meter, and the two guards it cannot work without."""

    @staticmethod
    def _report(seq: int, *, meter: str = "m1", used: int = 10, org: int = 100):
        return _env(
            "budget_reported",
            {
                "meter_id": meter,
                "seq": seq,
                "org_used_tokens": org,
                "org_max_tokens": 1000,
                "org_refused_at": "",
                "agents": [
                    {
                        "agent_id": "rt-1",
                        "role": "Lead",
                        "used_tokens": used,
                        "max_tokens": 500,
                        "refused_at": "",
                    }
                ],
            },
            category="",
        )

    def test_a_report_lands_on_the_seat_and_the_org(self) -> None:
        live = LiveState()
        change = live.apply_event(self._report(1))
        assert change.budget is True
        assert change.agents == {"Lead"}
        assert live.agent_overlay("Lead")["budget"] == {
            "used": 10,
            "max": 500,
            "refused_at": "",
        }
        assert live.budget()["org"] == {
            "used": 100,
            "max": 1000,
            "refused_at": "",
        }

    def test_the_meter_never_enters_the_activity_feed(self) -> None:
        """It is a live meter: a persisted copy replayed from history
        would show a dead process's counters as the current ones."""
        live = LiveState()
        live.apply_event(self._report(1))
        assert live.recent_events() == []

    def test_an_older_report_cannot_walk_the_meter_backwards(self) -> None:
        """Broker ordering holds only within a topic, and the standalone
        API reads a broadcast subscription across all of them."""
        live = LiveState()
        live.apply_event(self._report(2, used=90, org=900))
        live.apply_event(self._report(1, used=10, org=100))
        assert live.agent_overlay("Lead")["budget"]["used"] == 90
        assert live.budget()["org"]["used"] == 900

    def test_a_repeated_seq_is_dropped(self) -> None:
        live = LiveState()
        live.apply_event(self._report(3, used=50))
        assert live.apply_event(self._report(3, used=999)).budget is False
        assert live.agent_overlay("Lead")["budget"]["used"] == 50

    def test_a_new_meter_replaces_rather_than_merges(self) -> None:
        """A restart legitimately zeroes every counter.

        Merging -- or taking a maximum -- would pin a high-water mark
        from a dead run that no later report could ever clear.
        """
        live = LiveState()
        live.apply_event(self._report(9, used=400, org=900))
        # The new meter starts its sequence over, and at a lower value.
        live.apply_event(self._report(1, meter="m2", used=5, org=7))
        assert live.agent_overlay("Lead")["budget"]["used"] == 5
        assert live.budget()["org"]["used"] == 7
        assert live.budget()["meter_id"] == "m2"

    def test_a_seat_that_lost_its_meter_loses_its_bar(self) -> None:
        """A cap edited to zero, or a role decommissioned, stops being
        reported -- the seat must not keep the last figure it had."""
        live = LiveState()
        live.apply_event(self._report(1))
        assert live.agent_overlay("Lead")["budget"] is not None
        empty = _env(
            "budget_reported",
            {"meter_id": "m1", "seq": 2, "org_used_tokens": 5, "agents": []},
            category="",
        )
        change = live.apply_event(empty)
        assert live.agent_overlay("Lead")["budget"] is None
        assert "Lead" in change.agents

    def test_no_meter_reports_as_no_meter(self) -> None:
        """An API with no engine behind it has nothing to report, and a
        zero there would claim a measurement nobody took."""
        live = LiveState()
        assert live.budget() == {}
        live.apply_event(_env("task_started", {"role": "Lead", "agent_id": "rt-1"}))
        assert live.agent_overlay("Lead")["budget"] is None


class TestHydration:
    async def test_hydrate_seeds_state_tokens_and_events(self) -> None:
        live = LiveState()
        await live.hydrate(_FakeStore(), ["Lead"])
        overlay = live.agent_overlay("Lead")
        assert overlay["state"] == "working"
        assert overlay["current_phase"] == "execute"
        assert overlay["total_tokens"] == 150
        # list_events is newest-first; the buffer returns it newest-first.
        assert [e["id"] for e in live.recent_events()] == ["e2", "e1", "e0"]

    async def test_hydrated_failures_survive_a_restart(self) -> None:
        """A failure read back from history is still a failure.

        The feed row is payload-free and the store's ``list_events`` does
        not fetch payloads, so the flag rides the ``failed`` tag -- lose
        it and every historical failure renders as a success the moment
        the API restarts.
        """
        live = LiveState()
        await live.hydrate(_FakeStore(), ["Lead"])
        flags = {e["id"]: e["failed"] for e in live.recent_events()}
        assert flags == {"e2": False, "e1": False, "e0": True}

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


class TestSpendWindow:
    """The live spend rollup's window has to stay a window."""

    def _phase(self, live: LiveState, event_id: str, ts: str, tokens: int = 10) -> None:
        live.apply_event(
            _env(
                "agent_phase_completed",
                {
                    "role": "Lead",
                    "agent_id": "rt-1",
                    "turn_id": "t1",
                    "phase": "plan",
                    "model": "m",
                    "input_tokens": tokens,
                    "output_tokens": 0,
                    "total_tokens": tokens,
                },
                ts=ts,
                event_id=event_id,
            )
        )

    def test_records_inside_the_window_are_aggregated(self) -> None:
        live = LiveState()
        self._phase(live, "p1", "2026-06-14T12:00:00+00:00", tokens=10)
        self._phase(live, "p2", "2026-06-14T12:30:00+00:00", tokens=5)
        assert live.token_rollup()["totals"]["total_tokens"] == 15

    def test_a_re_delivered_phase_is_not_counted_twice(self) -> None:
        live = LiveState()
        self._phase(live, "p1", "2026-06-14T12:00:00+00:00", tokens=10)
        self._phase(live, "p1", "2026-06-14T12:00:00+00:00", tokens=10)
        assert live.token_rollup()["totals"]["calls"] == 1

    def test_records_older_than_the_window_are_dropped(self) -> None:
        live = LiveState()
        self._phase(live, "old", "2026-06-13T06:00:00+00:00", tokens=99)
        self._phase(live, "new", "2026-06-14T12:00:00+00:00", tokens=7)
        totals = live.token_rollup()["totals"]
        assert totals["total_tokens"] == 7
        assert totals["calls"] == 1

    def test_pruning_survives_an_out_of_order_head(self) -> None:
        """Hydration lands behind live events, so order is not guaranteed.

        The API subscribes to the stream before it hydrates, so a live
        event can sit ahead of the older records hydration then appends.
        A prune that only pops from the head sees a recent record there,
        exits, and never prunes again — the window silently stops being
        one.
        """
        live = LiveState()
        # A live event arrives first...
        self._phase(live, "live", "2026-06-14T12:00:00+00:00", tokens=7)
        # ...then hydration appends older records behind it.
        for i, ts in enumerate(
            ["2026-06-10T01:00:00+00:00", "2026-06-11T01:00:00+00:00"]
        ):
            live._phase_spend.append(
                {
                    "event_id": f"h{i}",
                    "timestamp": ts,
                    "agent_id": "rt-1",
                    "agent_role": "Lead",
                    "phase": "plan",
                    "model": "m",
                    "turn_id": "t0",
                    "iteration": 1,
                    "input_tokens": 100,
                    "output_tokens": 0,
                    "total_tokens": 100,
                }
            )
        # The next fold must clear everything outside the window.
        self._phase(live, "next", "2026-06-14T12:05:00+00:00", tokens=3)
        totals = live.token_rollup()["totals"]
        assert totals["total_tokens"] == 10, "stale records survived the prune"
        assert totals["calls"] == 2

    def test_the_rollup_reports_the_window_it_covers(self) -> None:
        assert live_state.LIVE_SPEND_WINDOW_HOURS == 24
        assert LiveState().token_rollup()["since_days"] == 1


class TestAfkLifecycle:
    """AFK is sticky, but not immortal."""

    def _afk(self, live: LiveState) -> None:
        live.apply_event(
            _env(
                "llm_unavailable",
                {"role": "Lead", "agent_id": "rt-1", "last_error_kind": "rate_limit"},
                ts="2026-06-14T12:00:00+00:00",
                event_id="a1",
            )
        )

    def test_task_failed_does_not_clear_the_failure_that_caused_it(self) -> None:
        live = LiveState()
        self._afk(live)
        live.apply_event(
            _env(
                "task_failed",
                {"role": "Lead", "agent_id": "rt-1", "task_id": "T-1"},
                ts="2026-06-14T12:00:01+00:00",
                event_id="a2",
            )
        )
        overlay = live.agent_overlay("Lead")
        assert overlay["state"] == "afk"
        assert overlay["afk_reason"] == "llm_unavailable"

    def test_real_work_clears_it(self) -> None:
        live = LiveState()
        self._afk(live)
        live.apply_event(
            _env(
                "task_started",
                {"role": "Lead", "agent_id": "rt-1", "task_id": "T-2"},
                ts="2026-06-14T12:10:00+00:00",
                event_id="a3",
            )
        )
        overlay = live.agent_overlay("Lead")
        assert overlay["state"] == "working"
        assert overlay["afk_reason"] == ""
        assert overlay["last_error"] is None

    def test_a_respawn_clears_it(self) -> None:
        """A spawn is a new instance; it cannot inherit the old one's stop.

        Otherwise the hold outlives an engine restart and a healthy seat
        renders as broken until it happens to do some work.
        """
        live = LiveState()
        self._afk(live)
        live.apply_event(
            _env(
                "agent_spawned",
                {"role": "Lead", "agent_id": "rt-2"},
                ts="2026-06-15T09:00:00+00:00",
                event_id="a4",
            )
        )
        overlay = live.agent_overlay("Lead")
        assert overlay["state"] == "idle"
        assert overlay["afk_reason"] == ""
        assert overlay["last_error"] is None


class TestPhaseFailureProjection:
    """A failed phase must reach the screen, not vanish."""

    def _fail(self, live: LiveState) -> None:
        live.apply_event(
            _env(
                "agent_phase_started",
                {"role": "Lead", "agent_id": "rt-1", "turn_id": "t1", "phase": "plan"},
                ts="2026-06-14T12:00:00+00:00",
                event_id="s1",
            )
        )
        live.apply_event(
            _env(
                "agent_phase_completed",
                {
                    "role": "Lead",
                    "agent_id": "rt-1",
                    "turn_id": "t1",
                    "phase": "plan",
                    "failed": True,
                    "error": "429 slow down",
                    "error_kind": "rate_limit",
                    "response": "I was part way through",
                },
                ts="2026-06-14T12:00:05+00:00",
                event_id="s2",
            )
        )

    def test_the_failure_lands_on_the_agent(self) -> None:
        live = LiveState()
        self._fail(live)
        error = live.agent_overlay("Lead")["last_error"]
        assert error["kind"] == "rate_limit"
        assert error["message"] == "429 slow down"
        assert error["phase"] == "plan"

    def test_the_failed_call_stays_on_screen(self) -> None:
        live = LiveState()
        self._fail(live)
        call = live.agent_overlay("Lead")["live_call"]
        assert call is not None, "the call the reader needs was cleared"
        assert call["failed"] is True
        assert call["in_progress"] is False
        assert call["response"] == "I was part way through"

    def test_a_following_afk_event_does_not_wipe_it(self) -> None:
        live = LiveState()
        self._fail(live)
        live.apply_event(
            _env(
                "llm_unavailable",
                {"role": "Lead", "agent_id": "rt-1", "last_error_kind": "rate_limit"},
                ts="2026-06-14T12:00:06+00:00",
                event_id="s3",
            )
        )
        assert live.agent_overlay("Lead")["live_call"] is not None

    def test_a_clean_phase_clears_the_call_as_before(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "agent_phase_started",
                {"role": "Lead", "agent_id": "rt-1", "turn_id": "t1", "phase": "plan"},
                event_id="c1",
            )
        )
        live.apply_event(
            _env(
                "agent_phase_completed",
                {"role": "Lead", "agent_id": "rt-1", "turn_id": "t1", "phase": "plan"},
                ts="2026-06-14T12:00:05+00:00",
                event_id="c2",
            )
        )
        overlay = live.agent_overlay("Lead")
        assert overlay["live_call"] is None
        assert overlay["last_error"] is None

    def test_a_failed_task_records_why(self) -> None:
        """``TaskFailed`` is a failure and must say so on the seat.

        It was folded in with ``task_completed`` and recorded nothing, so
        a task that died for a reason the engine does not treat as AFK --
        an unhandled handler exception, a rejected delegation -- left a
        seat rendering as healthy and idle, with the cause visible only
        as one line in the feed.
        """
        live = LiveState()
        live.apply_event(
            _env(
                "task_started",
                {"role": "Lead", "agent_id": "rt-1", "task_id": "T-9"},
                event_id="t1",
            )
        )
        live.apply_event(
            _env(
                "task_failed",
                {
                    "role": "Lead",
                    "agent_id": "rt-1",
                    "task_id": "T-9",
                    "error": "handler raised ValueError",
                },
                ts="2026-06-14T12:00:05+00:00",
                event_id="t2",
            )
        )
        error = live.agent_overlay("Lead")["last_error"]
        assert error is not None, "a failed task left the seat looking healthy"
        assert error["kind"] == "task_failed"
        assert error["message"] == "handler raised ValueError"

    def test_the_next_task_clears_it(self) -> None:
        live = LiveState()
        live.apply_event(
            _env(
                "task_failed",
                {"role": "Lead", "agent_id": "rt-1", "error": "boom"},
                event_id="t1",
            )
        )
        live.apply_event(
            _env(
                "task_started",
                {"role": "Lead", "agent_id": "rt-1", "task_id": "T-10"},
                ts="2026-06-14T12:00:05+00:00",
                event_id="t2",
            )
        )
        assert live.agent_overlay("Lead")["last_error"] is None


if __name__ == "__main__":  # pragma: no cover
    pytest.main([__file__, "-v"])
