"""Two things the inbound routes could not previously say.

GitHub delivers to every app installed on a repository, each with its
own delivery id.  A repository five agents work therefore produces five
deliveries of one comment, and they are not duplicates — they are five
agents being told, which is the point of giving each its own identity.
Without a seat in the path the engine cannot say which agent a delivery
was for, and dedupe cannot tell those five apart from a redelivery of
one.

Datadog is the opposite problem: it had no route at all.  It also
cannot sign a body — its webhook attaches headers with fixed values
only — so the strongest check available is a shared token, and these
tests hold the route to actually making it.
"""

from __future__ import annotations

import hashlib
import hmac
import json
from typing import Any

import pytest
from starlette.testclient import TestClient

from crewlet.api.app import create_app

GITHUB_SECRET = "github-webhook-secret"
DATADOG_TOKEN = "datadog-shared-token"


class _QueueStub:
    def __init__(self) -> None:
        self.published: list[tuple[str, Any]] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


class _EventStoreStub:
    def __init__(self) -> None:
        self.records: list[Any] = []

    async def record(self, *args: Any, **kwargs: Any) -> None:
        self.records.append((args, kwargs))


@pytest.fixture
def parts() -> tuple[TestClient, _QueueStub]:
    queue = _QueueStub()
    app = create_app(
        event_queue=queue,
        event_store=_EventStoreStub(),
        github_webhook_secret=GITHUB_SECRET,
        datadog_webhook_secret=DATADOG_TOKEN,
    )
    app.state.configured = True
    return TestClient(app), queue


def _github_headers(body: bytes) -> dict[str, str]:
    digest = hmac.new(GITHUB_SECRET.encode(), body, hashlib.sha256).hexdigest()
    return {
        "x-hub-signature-256": f"sha256={digest}",
        "x-github-event": "issue_comment",
        "x-github-delivery": "d-1",
        "content-type": "application/json",
    }


def test_github_delivery_carries_the_seat_it_was_addressed_to(parts):
    client, queue = parts
    body = json.dumps({"action": "created"}).encode()

    resp = client.post(
        "/webhooks/github/agent-swe", content=body, headers=_github_headers(body)
    )

    assert resp.status_code == 200
    assert queue.published, "a verified delivery must reach the queue"

    _, event = queue.published[0]
    assert event.payload["handle"] == "agent-swe"


def test_github_still_accepts_a_delivery_naming_no_seat(parts):
    client, queue = parts
    body = json.dumps({"action": "opened"}).encode()

    resp = client.post("/webhooks/github", content=body, headers=_github_headers(body))

    # The shared app names no seat, and refusing it would drop the one
    # delivery every tenant already gets.
    assert resp.status_code == 200
    assert queue.published[0][1].payload["handle"] == ""


def test_datadog_accepts_the_configured_token(parts):
    client, queue = parts
    body = json.dumps({"id": "1", "alert_type": "error", "title": "CPU high"}).encode()

    resp = client.post(
        "/webhooks/datadog",
        content=body,
        headers={"x-crewlet-token": DATADOG_TOKEN, "content-type": "application/json"},
    )

    assert resp.status_code == 200
    assert queue.published, "a verified alert must reach the queue"


def test_datadog_refuses_a_wrong_token_without_publishing(parts):
    client, queue = parts
    body = json.dumps({"id": "1", "alert_type": "error"}).encode()

    resp = client.post(
        "/webhooks/datadog",
        content=body,
        headers={"x-crewlet-token": "not-it", "content-type": "application/json"},
    )

    assert resp.status_code == 401
    # The check runs before the row is written, so a forged alert
    # cannot pollute the event store on its way to being refused.
    assert not queue.published


def test_datadog_refuses_a_delivery_carrying_no_token(parts):
    client, queue = parts
    body = json.dumps({"id": "1"}).encode()

    resp = client.post(
        "/webhooks/datadog", content=body, headers={"content-type": "application/json"}
    )

    assert resp.status_code == 401
    assert not queue.published


def test_datadog_holds_a_delivery_when_no_secret_is_configured():
    queue = _QueueStub()
    app = create_app(event_queue=queue, event_store=_EventStoreStub())
    app.state.configured = True
    client = TestClient(app)

    resp = client.post(
        "/webhooks/datadog",
        content=b"{}",
        headers={"x-crewlet-token": "anything", "content-type": "application/json"},
    )

    # Nothing to check against is not the same as a valid delivery: the
    # route holds it for retry rather than accepting or dropping it.
    assert resp.status_code == 503
    assert not queue.published
