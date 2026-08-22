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


def test_no_configured_secrets_is_refused_not_deferred():
    """An empty secret map means "cannot verify", never "nothing to verify".

    This used to be the one deliberate fail-open, on the reasoning that a
    deployment which never populated the map must keep working and that
    the transport re-verifies anyway. Both halves were checked against
    the code, and neither holds:

    - The transport does refuse: ``verify_signature`` returns False on an
      empty secret, so no agent is ever woken. But an agent wake was
      never the whole of what an unsigned POST bought. Everything BETWEEN
      the route and the transport still ran — the event-store row, the
      Pulsar publish, the fan-out to every connected dashboard socket —
      which is precisely the pollution this module's own docstring says
      these tests exist to stop. Attacker-controlled text reached durable
      storage and rendered in the operator's console under a Slack badge.
    - "Must keep working" assumed those deliveries worked. They did not:
      with no secret the transport dropped every one of them at the far
      end. The fail-open preserved no functioning path — only the
      pollution — so refusing costs a real deployment nothing and makes a
      mis-wired one visible instead of silently lossy.

    503, not 401, for the same reason the other six use it: the sender's
    request is fine, this side has nothing to check it with, and a 4xx
    would make Slack discard a delivery nobody else holds a copy of.
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
    assert resp.status_code == 503
    assert resp.headers.get("Retry-After"), "a held delivery must say when to retry"
    assert queue.published == [], "an unverifiable payload reached the queue"


def test_every_webhook_fails_closed_with_no_secret():
    """The seven inbound routes answer a missing secret the same way.

    Slack was the one that did not, and nothing compared them — each
    provider's tests live in their own class and assert their own
    handler. A rule that is only ever checked one route at a time is a
    rule the seventh route can quietly opt out of.
    """
    queue = _QueueStub()
    app = create_app(event_queue=queue, event_store=_EventStoreStub())
    app.state.configured = True
    client = TestClient(app)

    posts = {
        "/webhooks/github": {"action": "opened"},
        "/webhooks/gitlab": {"object_kind": "push"},
        "/webhooks/jira": {"webhookEvent": "jira:issue_created"},
        "/webhooks/confluence": {"webhookEvent": "page_created"},
        "/webhooks/plane": {"event": "issue", "action": "created"},
        "/webhooks/forge": {"eventType": "x"},
        "/webhooks/slack/engineer": _EVENT,
    }
    served = {}
    for path, body in posts.items():
        queue.published.clear()
        resp = client.post(path, json=body)
        if resp.status_code < 400 or queue.published:
            served[path] = (resp.status_code, len(queue.published))
    assert served == {}, f"these accepted an unauthenticated delivery: {served}"
