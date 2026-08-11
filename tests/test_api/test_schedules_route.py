"""Tests for the GET /schedules dashboard endpoint."""

from __future__ import annotations

from collections.abc import Awaitable, Callable

import pytest
from starlette.testclient import TestClient

from crewlet.api.app import create_app
from crewlet.events.types import Event
from crewlet.org.models import Organization, OrgUnit, Role, Schedule
from crewlet.schedule import describe_schedules


class MockEventQueue:
    async def publish(self, topic: str, event: Event) -> None:
        pass

    async def subscribe(
        self, topic: str, group: str, handler: Callable[[Event], Awaitable[None]]
    ) -> None:
        pass


def _org() -> Organization:
    return Organization(
        name="Acme",
        units=[
            OrgUnit(
                name="Quality",
                type="team",
                lead="QA Lead",
                roles=[
                    Role(name="QA Lead", handle="qa-lead"),
                    Role(name="QA Dev", handle="qa-dev"),
                ],
                schedules=[
                    Schedule(name="standup", cron="30 9 * * 1-5", task="standup"),
                    Schedule(name="off", cron="0 9 * * *", task="x", enabled=False),
                ],
            )
        ],
    )


@pytest.fixture
def client() -> TestClient:
    app = create_app(
        event_queue=MockEventQueue(),
        schedules_data=describe_schedules(_org(), default_timezone="UTC"),
    )
    return TestClient(app)


def test_schedules_endpoint_lists_configured(client: TestClient):
    resp = client.get("/schedules")
    assert resp.status_code == 200
    body = resp.json()
    assert "schedules" in body and "recent_runs" in body
    by_name = {s["name"]: s for s in body["schedules"]}
    assert set(by_name) == {"standup", "off"}

    standup = by_name["standup"]
    assert standup["scope_type"] == "unit"
    assert standup["target"] == "each"
    assert set(standup["runners"]) == {"qa-lead", "qa-dev"}
    # Enabled schedule has a computed next_run; disabled one does not.
    assert standup["next_run"]  # non-empty ISO timestamp
    assert by_name["off"]["next_run"] == ""

    # No database wired → empty run history (but the key is present).
    assert body["recent_runs"] == []


def test_schedules_endpoint_empty_without_config():
    app = create_app(event_queue=MockEventQueue())
    resp = TestClient(app).get("/schedules")
    assert resp.status_code == 200
    assert resp.json() == {"schedules": [], "recent_runs": []}
