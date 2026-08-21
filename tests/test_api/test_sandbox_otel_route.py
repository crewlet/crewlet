"""Tests for the engine-fronted OTLP receiver route.

``POST /otlp/{token}/v1/{signal}`` — the in-sandbox coding agent exports
here with a per-run token in the path; the route validates the token and
forwards to the real backend with upstream auth added engine-side.
"""

from __future__ import annotations

from typing import Any

from starlette.testclient import TestClient

from tests.test_api.helpers import create_app


class _MockEventQueue:
    def __init__(self) -> None:
        self.published: list[tuple[str, Any]] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


def _otel_app() -> tuple[TestClient, Any, list]:
    from crewlet.sandbox.otel import SandboxOtelReceiver

    forwarded: list = []

    async def _post(url, body, headers):
        forwarded.append((url, body, headers))

    receiver = SandboxOtelReceiver(
        base_url="https://engine",
        upstream_endpoint="https://backend",
        upstream_headers={"Authorization": "Bearer real"},
        post=_post,
    )
    app = create_app(event_queue=_MockEventQueue(), sandbox_otel_receiver=receiver)
    return TestClient(app), receiver, forwarded


def test_otlp_valid_token_forwards() -> None:
    client, receiver, forwarded = _otel_app()
    token = receiver.tokens.mint("trace-1", ttl_seconds=60)
    resp = client.post(f"/otlp/{token}/v1/traces", content=b"payload")
    assert resp.status_code == 200
    assert len(forwarded) == 1
    url, body, headers = forwarded[0]
    assert url == "https://backend/v1/traces"
    assert body == b"payload"
    assert headers["Authorization"] == "Bearer real"


def test_otlp_invalid_token_401() -> None:
    client, _receiver, forwarded = _otel_app()
    resp = client.post("/otlp/bogus/v1/traces", content=b"x")
    assert resp.status_code == 401
    assert forwarded == []


def test_otlp_unknown_signal_404() -> None:
    client, receiver, _ = _otel_app()
    token = receiver.tokens.mint("t", ttl_seconds=60)
    resp = client.post(f"/otlp/{token}/v1/spans", content=b"x")
    assert resp.status_code == 404


def test_otlp_no_receiver_configured_503() -> None:
    app = create_app(event_queue=_MockEventQueue())  # no receiver
    resp = TestClient(app).post("/otlp/anytoken/v1/traces", content=b"x")
    assert resp.status_code == 503
