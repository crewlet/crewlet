"""Tests for the engine-fronted OTLP receiver: per-run token
store, the receiver façade, and the _otel_env receiver path."""

from __future__ import annotations

from typing import Any

from crewlet.sandbox.otel import SandboxOtelReceiver, SandboxOtelTokens


class _Clock:
    def __init__(self) -> None:
        self.t = 1000.0

    def __call__(self) -> float:
        return self.t


# --- token store ----------------------------------------------------------


def test_mint_and_validate_returns_trace_id() -> None:
    tokens = SandboxOtelTokens()
    tok = tokens.mint("trace-abc", ttl_seconds=60)
    assert tokens.validate(tok) == "trace-abc"
    assert tokens.validate("nope") is None


def test_token_expires() -> None:
    clk = _Clock()
    tokens = SandboxOtelTokens(now=clk)
    tok = tokens.mint("t", ttl_seconds=30)
    clk.t += 29
    assert tokens.validate(tok) == "t"
    clk.t += 2  # now past the 30s ttl
    assert tokens.validate(tok) is None
    # expired token is reaped on the failed validate
    assert tokens.sweep() == 0


def test_revoke_and_sweep() -> None:
    clk = _Clock()
    tokens = SandboxOtelTokens(now=clk)
    a = tokens.mint("a", ttl_seconds=10)
    tokens.mint("b", ttl_seconds=10)
    tokens.revoke(a)
    assert tokens.validate(a) is None
    clk.t += 11
    assert tokens.sweep() == 1  # 'b' expired


# --- receiver -------------------------------------------------------------


def test_endpoint_for_embeds_token() -> None:
    r = SandboxOtelReceiver(base_url="https://engine.internal/")
    url, token = r.endpoint_for("trace-1", ttl_seconds=60)
    assert url == f"https://engine.internal/otlp/{token}"
    # the minted token validates against the receiver's own store
    assert r.tokens.validate(token) == "trace-1"


async def test_forward_noop_without_upstream() -> None:
    calls: list[Any] = []

    async def _post(url, body, headers):
        calls.append((url, body, headers))

    r = SandboxOtelReceiver(base_url="https://e", post=_post)  # no upstream
    await r.forward("traces", b"payload", "application/x-protobuf")
    assert calls == []


async def test_forward_posts_to_upstream_with_auth() -> None:
    calls: list[Any] = []

    async def _post(url, body, headers):
        calls.append((url, body, headers))

    r = SandboxOtelReceiver(
        base_url="https://e",
        upstream_endpoint="https://backend/v",
        upstream_headers={"Authorization": "Bearer real-token"},
        post=_post,
    )
    await r.forward("metrics", b"data", "application/json")
    assert len(calls) == 1
    url, body, headers = calls[0]
    assert url == "https://backend/v/v1/metrics"
    assert body == b"data"
    # the BACKEND token is added here, engine-side
    assert headers["Authorization"] == "Bearer real-token"
    assert headers["content-type"] == "application/json"


async def test_forward_swallows_errors() -> None:
    async def _boom(url, body, headers):
        raise RuntimeError("backend down")

    r = SandboxOtelReceiver(
        base_url="https://e", upstream_endpoint="https://backend", post=_boom
    )
    # must not raise — telemetry failures never break the exporter
    await r.forward("traces", b"x", "")


# --- _otel_env receiver path ----------------------------------------------


def test_otel_env_uses_receiver_endpoint_no_headers() -> None:
    from crewlet.agent.definition import AgentDefinition
    from crewlet.agent.execute_sandbox import _otel_env
    from crewlet.agent.instance import AgentInstance
    from crewlet.agent.turn_context import TurnContext
    from crewlet.org.models import Organization, OrgUnit, Role

    role = Role(name="Eng", handle="eng")
    org = Organization(
        name="A", units=[OrgUnit(name="U", type="team", lead="Eng", roles=[role])]
    )
    agent = AgentInstance(
        definition=AgentDefinition(role=org.get_role("Eng"), org=org), handle="eng"
    )
    turn = TurnContext(agent=agent, org=org, task_description="x")

    receiver = SandboxOtelReceiver(base_url="https://engine")
    env = _otel_env(turn, receiver, 120.0)
    assert env["OTEL_EXPORTER_OTLP_ENDPOINT"].startswith("https://engine/otlp/")
    # the ingest token is NEVER set as a header in the sandbox
    assert "OTEL_EXPORTER_OTLP_HEADERS" not in env
    # a per-run token was minted and validates
    token = env["OTEL_EXPORTER_OTLP_ENDPOINT"].rsplit("/", 1)[-1]
    assert receiver.tokens.validate(token) == turn.turn_id
