"""The three seams the new dashboard rooms read: budgets, integrations
and sandbox runs.

Each is one payload function with two callers — a REST route and a
``/ws/stream`` query — for the same reason every other pair in this
package is: two surfaces that answer the same question from two
implementations eventually answer it differently, and nothing notices
until an operator is comparing them.

The rest of what is asserted here is honesty. All three of these rooms
report on things that can be *unknown*, and every one of them had a
tempting wrong answer available:

* a budget counter that cannot be read is not a counter that reads zero;
* an integration that has seen no traffic is not an integration that is
  healthy;
* a sandbox run parked on a question is not a run that has finished.
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

import pytest
from starlette.testclient import TestClient

from crewlet.api.routes.budgets import budgets_payload
from crewlet.api.routes.conversations import conversations_payload
from crewlet.api.routes.integrations import (
    INBOUND,
    TRAFFIC_WINDOW_HOURS,
    integrations_payload,
)
from crewlet.api.routes.sandbox_runs import (
    PendingStoreUnavailable,
    sandbox_runs_payload,
)
from crewlet.db.client import Database
from tests.test_api.helpers import create_app


class _Queue:
    async def publish(self, topic: str, event: Any) -> None:
        return None

    async def subscribe_stream(self, pattern: str, handler: Any) -> Any:
        async def _unsub() -> None:
            return None

        return _unsub


ROLES = [
    {
        "id": "11111111-1111-1111-1111-111111111111",
        "name": "Engineer",
        "handle": "eng",
        "token_budget": 100_000,
    },
    {
        "id": "22222222-2222-2222-2222-222222222222",
        "name": "Analyst",
        "handle": "analyst",
        "token_budget": 50_000,
    },
]


def _app(**kwargs: Any) -> Any:
    return create_app(_Queue(), agent_roles=ROLES, **kwargs)


# ---------------------------------------------------------------------------
# Budgets
# ---------------------------------------------------------------------------


class TestBudgets:
    async def test_an_unreadable_counter_is_not_a_counter_that_reads_zero(
        self,
    ) -> None:
        """``durable`` is the flag that separates the two.

        Without it a database blip renders every seat at the bottom of
        its cap — the most reassuring possible picture, drawn at exactly
        the moment nothing is known.
        """
        payload = await budgets_payload(_app(org_data={"token_budget": 1_000_000}))
        assert payload["durable"] is False
        assert payload["org"]["durable_used"] == 0

    async def test_a_cap_with_no_live_meter_reports_no_meter_not_zero_spend(
        self,
    ) -> None:
        """``live_used`` is ``None``, never ``0``.

        The live meter is per engine RUN and a standalone API has none.
        Zero would let a client draw a full-width empty bar and call it
        "nothing spent this run", which is a claim about a run that is
        not happening.
        """
        payload = await budgets_payload(_app(org_data={"token_budget": 900}))
        assert payload["org"]["live_used"] is None
        assert payload["org"]["live_max"] is None
        for seat in payload["seats"]:
            assert seat["live_used"] is None, seat

    async def test_caps_come_from_the_shared_agent_projection(self) -> None:
        """Same ids, same handles, same human-seat exclusion as /agents.

        Deriving them here instead is how a cap ends up attached to a
        seat id that nothing else in the product uses.
        """
        payload = await budgets_payload(_app())
        by_id = {seat["agent_id"]: seat for seat in payload["seats"]}
        assert set(by_id) == {role["id"] for role in ROLES}
        assert by_id[ROLES[0]["id"]]["max_tokens"] == 100_000
        assert by_id[ROLES[0]["id"]]["handle"] == "eng"

    async def test_exhaustion_is_the_refusal_not_the_ratio(self) -> None:
        """``refused_at`` is carried through verbatim.

        ``TokenBudget`` refuses a charge that would exceed the cap and
        increments nothing, so a seat charged in 3k rounds against a 100k
        cap stalls near 99k and never compares equal to its own maximum.
        A surface deriving exhaustion from ``used >= max`` shows a
        permanently blocked seat at 99% and calls it healthy.
        """
        app = _app()
        app.state.stream = _StreamWithMeter(
            org={"used": 99_000, "max": 100_000, "refused_at": "2026-08-21T09:14:00Z"}
        )
        payload = await budgets_payload(app)
        assert payload["org"]["refused_at"] == "2026-08-21T09:14:00Z"
        assert payload["org"]["live_used"] < payload["org"]["live_max"]

    def test_the_rest_route_and_the_socket_query_are_one_function(self) -> None:
        from crewlet.api.queries import _QUERIES
        from crewlet.api.routes import budgets as module

        handler, _needs_operator = _QUERIES["budgets"]
        assert "budgets_payload" in handler.__code__.co_names
        assert module.get_budgets.__doc__


class TestConversations:
    """The conversation ledger, served the way the prompt reads it.

    An operator who cannot read what the engine feeds a turn is looking
    at the same invisible second memory the CLI workspace deletes on
    every call to avoid — so the honesty rule here is the one that
    matters: no ledger and an empty ledger must not render alike.
    """

    async def test_no_database_is_not_an_empty_ledger(self) -> None:
        payload = await conversations_payload(_app(), handle="eng")
        assert payload["available"] is False
        assert payload["conversations"] == []

    async def test_it_lists_what_a_seat_has_worked(self) -> None:
        app = _app(database=_FakeDatabase())
        payload = await conversations_payload(app, handle="eng")
        assert payload["available"] is True
        assert [c["conversation_key"] for c in payload["conversations"]] == [
            "jira:POC-7"
        ]
        assert payload["conversations"][0]["entries"] == 2

    async def test_one_conversation_returns_its_entries(self) -> None:
        app = _app(database=_FakeDatabase())
        payload = await conversations_payload(app, handle="eng", key="jira:POC-7")
        assert [e["reply"] for e in payload["entries"]] == ["first", "second"]

    async def test_an_entry_shows_the_timestamp_the_prompt_renders(self) -> None:
        """The Threads tab's contract is that it shows what the PROMPT
        shows, and the prompt renders ``SessionEntry.at``. Building the
        payload by spreading the entry LAST made the answer depend on
        dict-merge order and quietly dropped the row-timestamp fallback
        the same line was there to provide."""
        app = _app(database=_FakeDatabase(stored_at=True))
        payload = await conversations_payload(app, handle="eng", key="jira:POC-7")
        # Entries come back oldest-first, the order the prompt renders.
        assert [e["at"] for e in payload["entries"]] == ["", "2026-08-19T07:00"]

    async def test_an_entry_with_no_recorded_time_falls_back_to_the_row(self) -> None:
        """An entry written by an older engine still landed at a knowable
        moment; a blank timestamp would read as an entry from nowhere."""
        app = _app(database=_FakeDatabase())
        assert [e["at"] for e in (await _entries(app))] == [
            "2026-08-20T09:00:00+00:00",
            "2026-08-20T09:30:00+00:00",
        ]

    async def test_an_unreadable_ledger_reports_unavailable(self) -> None:
        """Same rule as the budget counter: a store that cannot be read
        is not a seat that has said nothing."""
        app = _app(database=_FakeDatabase(fail=True))
        payload = await conversations_payload(app, handle="eng")
        assert payload["available"] is False
        assert payload["conversations"] == []

    async def test_no_handle_asks_nothing(self) -> None:
        payload = await conversations_payload(_app(database=_FakeDatabase()))
        assert payload["conversations"] == []

    def test_the_rest_route_and_the_socket_query_are_one_function(self) -> None:
        from crewlet.api.queries import _QUERIES
        from crewlet.api.routes import conversations as module

        handler, _needs_operator = _QUERIES["conversations"]
        assert "conversations_payload" in handler.__code__.co_names
        assert module.get_conversations.__doc__


async def _entries(app: Any) -> list[dict[str, Any]]:
    payload = await conversations_payload(app, handle="eng", key="jira:POC-7")
    return payload["entries"]


class _FakeDatabase(Database):
    """A real ``Database`` with its execute stubbed.

    Subclassed rather than duck-typed because the payload selects its
    store with ``isinstance``: a stand-in that skipped that check would
    pass here while the production path returned no store at all.
    """

    def __init__(self, *, fail: bool = False, stored_at: bool = False) -> None:
        super().__init__()
        self._fail = fail
        self._stored_at = stored_at

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        if self._fail:
            raise RuntimeError("database is down")
        if "GROUP BY" in query:
            return [
                {
                    "conversation_key": "jira:POC-7",
                    "entries": 2,
                    "last_at": datetime(2026, 8, 20, 9, 30, tzinfo=UTC),
                }
            ]
        # ``SessionEntry.to_dict`` always emits every key, empty or
        # not — which is exactly what made the row-timestamp fallback
        # dead code when the entry was spread last.
        second: dict[str, Any] = {"reply": "second", "at": ""}
        first: dict[str, Any] = {"reply": "first", "at": ""}
        if self._stored_at:
            # Only the newer entry carries its own time, so one row
            # exercises the entry value and the other the fallback.
            second["at"] = "2026-08-19T07:00"
            return [
                # A row time that DISAGREES with the entry's own, so the
                # assertion proves which one reaches the screen.
                {
                    "turn_id": "t2",
                    "work_key": "w2",
                    "created_at": datetime(2026, 8, 20, 9, 30, tzinfo=UTC),
                    "entry": second,
                },
                {"turn_id": "t1", "work_key": "w1", "created_at": None, "entry": first},
            ]
        return [
            {
                "turn_id": "t2",
                "work_key": "w2",
                "created_at": datetime(2026, 8, 20, 9, 30, tzinfo=UTC),
                "entry": second,
            },
            {
                "turn_id": "t1",
                "work_key": "w1",
                "created_at": datetime(2026, 8, 20, 9, 0, tzinfo=UTC),
                "entry": first,
            },
        ]


class _StreamWithMeter:
    def __init__(self, *, org: dict[str, Any]) -> None:
        self.live = _Live(org)


class _Live:
    def __init__(self, org: dict[str, Any]) -> None:
        self._org = org

    def budget(self) -> dict[str, Any]:
        return {"org": self._org}

    def merge_agents(self, roles: list[dict[str, Any]]) -> list[dict[str, Any]]:
        return [dict(role) for role in roles]


# ---------------------------------------------------------------------------
# Integrations
# ---------------------------------------------------------------------------


class TestIntegrations:
    async def test_silence_is_reported_as_silence_never_as_health(self) -> None:
        """No status field, in either direction.

        An idle Slack and a 401-ing Slack are indistinguishable in the
        event store — verification runs *before* a delivery row is
        written, so a rejected one leaves no trace at all. Neither
        "healthy" nor "down" would be a fact, so neither is offered.
        """
        payload = await integrations_payload(_app())
        for row in payload["integrations"]:
            assert "status" not in row, row
            assert "healthy" not in row, row

    async def test_a_missing_signing_secret_is_false_and_an_absent_one_is_none(
        self,
    ) -> None:
        """Three-valued, because the two mean opposite things.

        ``None`` — this surface does not use one (Mattermost
        authenticates its websocket with the bot's own token).
        ``False`` — it does, and none is configured, which means the
        webhook route answers 503 to every delivery. That is a real,
        silent outage and it must not render the same as "not
        applicable".
        """
        payload = await integrations_payload(_app())
        by_key = {row["key"]: row for row in payload["integrations"]}
        assert by_key["mattermost"]["secret_present"] is None
        assert by_key["github"]["secret_present"] is False

        app = _app()
        app.state.github_webhook_secret = "s3cret"  # noqa: S105 - test literal
        payload = await integrations_payload(app)
        by_key = {row["key"]: row for row in payload["integrations"]}
        assert by_key["github"]["secret_present"] is True

    async def test_no_secret_value_ever_reaches_the_payload(self) -> None:
        """Only the boolean. The room shows whether one is set, never what.

        Nothing else in the API is allowed to emit a credential and this
        is not the exception — the payload is JSON on a page an operator
        may well screen-share.
        """
        app = _app()
        app.state.github_webhook_secret = "sup3r-s3cret-value"  # noqa: S105
        app.state.jira_webhook_secret = "another-s3cret"  # noqa: S105
        text = repr(await integrations_payload(app))
        assert "sup3r-s3cret-value" not in text
        assert "another-s3cret" not in text

    async def test_traffic_counts_are_grouped_by_the_store_not_the_browser(
        self,
    ) -> None:
        """And routing rows are folded onto the integration they routed for.

        A webhook row's ``source`` is the bare key; a routing row's is
        ``notification_service.<key>``. Counting them as two different
        integrations is how a funnel ends up wider at the bottom than at
        the top.
        """
        app = _app()
        app.state.event_store = _Store(
            [
                {
                    "source": "slack",
                    "category": "webhook",
                    "event_type": "raw_webhook",
                    "count": 12,
                    "last_at": datetime(2026, 8, 21, 9, tzinfo=UTC),
                },
                {
                    "source": "notification_service.slack",
                    "category": "notification",
                    "event_type": "external_notification",
                    "count": 7,
                    "last_at": datetime(2026, 8, 21, 10, tzinfo=UTC),
                },
                {
                    "source": "notification_service.slack",
                    "category": "notification",
                    "event_type": "notification_skipped",
                    "count": 5,
                    "last_at": datetime(2026, 8, 21, 10, tzinfo=UTC),
                },
            ]
        )
        payload = await integrations_payload(app)
        slack = next(r for r in payload["integrations"] if r["key"] == "slack")
        assert slack["inbound"] == 12
        assert slack["routed"] == 7
        assert slack["skipped"] == 5
        assert app.state.event_store.since_hours == TRAFFIC_WINDOW_HOURS

    async def test_an_event_store_that_cannot_count_degrades_to_no_traffic(
        self,
    ) -> None:
        """Not to zeros presented as measurements.

        The memory store predates ``count_events_by_source``; a
        deployment on it must say the figures are unavailable rather than
        report a quiet company.
        """
        payload = await integrations_payload(_app())
        assert payload["traffic_known"] is False

    def test_every_inbound_surface_names_a_route_that_exists(self) -> None:
        """Except the one that deliberately has none.

        A path here that no route serves would render an operator a URL
        to paste into a provider's webhook config that answers 404 for
        ever.
        """
        app = _app()
        served = {
            getattr(route, "path", "")
            for route in app.routes
            if getattr(route, "path", "")
        }
        for key, spec in INBOUND.items():
            if not spec["path"]:
                assert spec["kind"] != "webhook", key
                continue
            assert spec["path"] in served, f"{key} names an unserved path"


class _Store:
    def __init__(self, rows: list[dict[str, Any]]) -> None:
        self._rows = rows
        self.since_hours: int | None = None

    async def count_events_by_source(self, *, since_hours: int) -> list[dict[str, Any]]:
        self.since_hours = since_hours
        return list(self._rows)


# ---------------------------------------------------------------------------
# Sandbox runs
# ---------------------------------------------------------------------------


class TestSandboxRuns:
    async def test_without_a_database_the_payload_says_so(self) -> None:
        with pytest.raises(PendingStoreUnavailable):
            await sandbox_runs_payload(_app())

    def test_the_rest_twin_answers_none_rather_than_failing(self) -> None:
        """Without a database the engine cannot park a run at all, so
        "there are none" is the true answer for that deployment — but it
        is labelled, because an empty board and an unreadable one look
        identical."""
        with TestClient(_app()) as client:
            body = client.get("/sandbox-runs").json()
        assert body["runs"] == []
        assert "no database" in body["degraded"]

    def test_a_run_that_no_chat_reply_can_reach_is_marked_as_such(self) -> None:
        """The resume path matches an inbound notification's conversation
        key by exact string equality. A run started by a schedule tick or
        an A2A wake stored ``event:{id}``, which no inbound message can
        reproduce — so telling somebody to "reply in the thread" would
        send them to a thread that does not exist."""
        from crewlet.api.routes.sandbox_runs import _answerable_in_chat

        assert _answerable_in_chat("slack:C123:1700000000.1") is True
        assert _answerable_in_chat("event:9f2c") is False
        assert _answerable_in_chat("") is False

    def test_the_execute_conversation_is_never_shipped(self) -> None:
        """``execute_state`` is the largest column in the row and every
        prompt in it is already reachable through the event store. A
        board rendering one line per run does not need megabytes of
        serialised conversation to draw it."""
        from crewlet.api.routes.sandbox_runs import _serialise

        row = _serialise(_Run())
        assert "execute_state" not in row
        assert "sandbox_id" not in row
        assert row["box_exists"] is True

    def test_the_rest_route_and_the_socket_query_are_one_function(self) -> None:
        from crewlet.api.queries import _QUERIES

        handler, _needs_operator = _QUERIES["sandbox_runs"]
        assert "sandbox_runs_payload" in handler.__code__.co_names


class _Run:
    turn_id = "t-1"
    agent_handle = "eng"
    role = "Engineer"
    status = "awaiting_clarification"
    coding_agent = "claude"
    task_description = "Fix the flaky test"
    question = "Which branch?"
    audience = "eng"
    branch = "feat/x"
    trace_id = "tr-1"
    owner = "node-a"
    sandbox_id = "sb-1"
    paused_at = None
    pause_ttl_seconds = 3600
    conversation_key = "slack:C1:1.2"
    created_at = datetime(2026, 8, 21, 8, tzinfo=UTC)
    updated_at = datetime(2026, 8, 21, 9, tzinfo=UTC)
    execute_state = {"messages": ["a" * 10_000]}
