"""Tests for GET /webhooks/slack-oauth — the OAuth install landing page.

Used by ``crewlet slack provision``: every provisioned app redirects
here after the operator's authorize click, and the page shows the
temporary code to paste back into the CLI.
"""

from __future__ import annotations

import pytest
from starlette.testclient import TestClient

from crewlet.api.app import create_app


class _NoopEventQueue:
    async def publish(self, topic, event):  # pragma: no cover - unused
        pass

    async def subscribe(self, topic, group, handler):  # pragma: no cover
        pass


@pytest.fixture
def client() -> TestClient:
    return TestClient(create_app(event_queue=_NoopEventQueue()))


def test_displays_code_and_agent_handle(client: TestClient) -> None:
    resp = client.get(
        "/webhooks/slack-oauth", params={"code": "12345.abcde", "state": "agent-ceo"}
    )
    assert resp.status_code == 200
    assert "text/html" in resp.headers["content-type"]
    assert "12345.abcde" in resp.text
    assert "agent-ceo" in resp.text
    assert "crewlet slack provision" in resp.text


def test_code_is_html_escaped(client: TestClient) -> None:
    resp = client.get("/webhooks/slack-oauth", params={"code": "<script>x</script>"})
    assert "<script>x</script>" not in resp.text
    assert "&lt;script&gt;" in resp.text


def test_slack_error_reported(client: TestClient) -> None:
    resp = client.get("/webhooks/slack-oauth", params={"error": "access_denied"})
    assert resp.status_code == 400
    assert "access_denied" in resp.text


def test_missing_code_rejected(client: TestClient) -> None:
    resp = client.get("/webhooks/slack-oauth")
    assert resp.status_code == 400


def test_does_not_shadow_the_events_webhook(client: TestClient) -> None:
    """The literal /oauth GET route must leave POST /{handle} intact —
    including for a hypothetical agent whose handle is 'oauth'."""
    resp = client.post(
        "/webhooks/slack/dev",
        json={"type": "url_verification", "challenge": "chal"},
    )
    assert resp.status_code == 200
    assert resp.json() == {"challenge": "chal"}
