"""The Slack webhook route must verify BEFORE it persists or broadcasts.

The transport verifies too, and that is what stops an unverified payload
waking an agent.  But everything between the route and the transport —
the event-store row, the fan-out to every connected dashboard websocket,
the Pulsar publish — used to happen unconditionally, so an
unauthenticated POST could pollute the event log and inject content into
the dashboard without ever reaching an agent.  GitHub, GitLab and Plane
all 401 at the route; these tests hold Slack to the same line.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import time
from typing import Any

import pytest
from starlette.testclient import TestClient

from crewlet.api.app import create_app

SECRET = "test-signing-secret"


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


def _signed_headers(body: bytes, secret: str = SECRET) -> dict[str, str]:
    ts = str(int(time.time()))
    digest = hmac.new(
        secret.encode(), b"v0:" + ts.encode() + b":" + body, hashlib.sha256
    ).hexdigest()
    return {
        "x-slack-request-timestamp": ts,
        "x-slack-signature": f"v0={digest}",
        "content-type": "application/json",
    }


@pytest.fixture
def parts() -> tuple[TestClient, _QueueStub, _EventStoreStub]:
    queue = _QueueStub()
    store = _EventStoreStub()
    app = create_app(
        event_queue=queue,
        event_store=store,
        slack_signing_secrets={"engineer": SECRET},
    )
    app.state.configured = True
    return TestClient(app), queue, store


_EVENT = {
    "type": "event_callback",
    "api_app_id": "A1",
    "team_id": "T1",
    "event": {
        "type": "message",
        "user": "U_HUMAN",
        "text": "hello",
        "channel": "C1",
        "ts": "1700000000.000100",
    },
}


def test_valid_signature_is_accepted_and_published(parts):
    client, queue, store = parts
    body = json.dumps(_EVENT).encode()
    resp = client.post(
        "/webhooks/slack/engineer", content=body, headers=_signed_headers(body)
    )
    assert resp.status_code == 200
    assert queue.published, "a verified event must still reach the queue"


def test_bad_signature_is_rejected_without_persisting_or_publishing(parts):
    client, queue, store = parts
    body = json.dumps(_EVENT).encode()
    headers = _signed_headers(body, secret="wrong-secret")

    resp = client.post("/webhooks/slack/engineer", content=body, headers=headers)

    assert resp.status_code == 401
    assert queue.published == [], "unverified payload must not be enqueued"
    assert store.records == [], "unverified payload must not be persisted"


def test_missing_signature_headers_are_rejected(parts):
    client, queue, store = parts
    body = json.dumps(_EVENT).encode()
    resp = client.post(
        "/webhooks/slack/engineer",
        content=body,
        headers={"content-type": "application/json"},
    )
    assert resp.status_code == 401
    assert queue.published == []
    assert store.records == []


def test_unknown_handle_is_rejected(parts):
    """A handle with no registered secret cannot be verified, so it must
    not be treated as unverifiable-therefore-allowed."""
    client, queue, store = parts
    body = json.dumps(_EVENT).encode()
    resp = client.post(
        "/webhooks/slack/ghost", content=body, headers=_signed_headers(body)
    )
    assert resp.status_code == 401
    assert queue.published == []
    assert store.records == []


def test_url_verification_challenge_needs_no_signature():
    """Slack sends the challenge before the app has ever been installed,
    and the API answers it unconditionally by design — it carries no
    payload to persist and reaches no agent."""
    app = create_app(
        event_queue=_QueueStub(),
        event_store=_EventStoreStub(),
        slack_signing_secrets={"engineer": SECRET},
    )
    app.state.configured = True
    client = TestClient(app)

    resp = client.post(
        "/webhooks/slack/engineer",
        json={"type": "url_verification", "challenge": "abc123"},
    )
    assert resp.status_code == 200
    assert resp.json()["challenge"] == "abc123"


def test_no_configured_secrets_leaves_verification_to_the_transport():
    """An engine that never populated the map (an older embedded start, a
    company with no Slack seats) must keep working: the route defers, and
    the transport still refuses to act on an unverified payload.

    This is the one deliberate fail-open, and it is scoped to "the edge
    has nothing to check with" — never to "the check failed".
    """
    queue = _QueueStub()
    app = create_app(event_queue=queue, event_store=_EventStoreStub())
    app.state.configured = True
    client = TestClient(app)

    body = json.dumps(_EVENT).encode()
    resp = client.post(
        "/webhooks/slack/engineer",
        content=body,
        headers={"content-type": "application/json"},
    )
    assert resp.status_code == 200
    assert queue.published, "without edge secrets the payload still flows"
