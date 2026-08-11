"""Tests for the agent-driven onboarding marker + hint."""

from __future__ import annotations

from typing import Any

from crewlet.learning.onboarding import (
    MARK_ONBOARDED_TOOL,
    build_onboarding_hint,
    compute_chain_hash,
    is_onboarded,
    register_mark_onboarded_tool,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry


def _build_org() -> Organization:
    backend = OrgUnit(
        name="Backend",
        type="team",
        lead="Backend Lead",
        roles=[Role(name="Backend Lead"), Role(name="Backend Engineer")],
    )
    engineering = OrgUnit(
        name="Engineering",
        type="department",
        lead="VP Engineering",
        roles=[Role(name="VP Engineering")],
        children=[backend],
    )
    return Organization(name="Acme", units=[engineering])


class _MarkerStoreStub:
    """In-memory stand-in for :class:`OnboardingMarkerStore`.

    Mirrors the real store's ``mark`` UPSERT semantics so a re-mark
    overwrites the existing row.  ``is_onboarded`` does the same
    chain-hash equality check the real store does.
    """

    def __init__(self) -> None:
        self.rows: dict[str, dict[str, Any]] = {}
        self.marks: list[dict[str, Any]] = []

    async def is_onboarded(self, *, agent_id: str, chain_hash: str) -> bool | None:
        row = self.rows.get(agent_id)
        if row is None:
            return False
        return str(row.get("chain_hash") or "") == chain_hash

    async def try_claim_pass(self, *, agent_id: str, ttl_seconds: float) -> bool:
        return True

    async def release_claim(self, agent_id: str) -> None:
        return None

    async def mark(
        self,
        *,
        agent_id: str,
        chain_hash: str,
        agent_handle: str = "",
        role: str = "",
        summary: str = "",
    ) -> None:
        self.marks.append(
            {
                "agent_id": agent_id,
                "chain_hash": chain_hash,
                "agent_handle": agent_handle,
                "role": role,
                "summary": summary,
            }
        )
        self.rows[agent_id] = {
            "chain_hash": chain_hash,
            "agent_handle": agent_handle,
            "role": role,
            "summary": summary,
        }


def test_chain_hash_changes_on_role_move() -> None:
    org = _build_org()
    role = Role(name="Backend Engineer")
    h1 = compute_chain_hash(role, org)

    other_unit = OrgUnit(
        name="Frontend", type="team", roles=[Role(name="Backend Engineer")]
    )
    org2 = Organization(name="Acme", units=[other_unit])
    h2 = compute_chain_hash(role, org2)
    assert h1 != h2


def test_build_onboarding_hint_lists_chain_pages() -> None:
    org = _build_org()
    hint = build_onboarding_hint(Role(name="Backend Engineer"), org)
    assert "Onboarding" in hint
    assert "Acme" in hint
    assert "Engineering" in hint
    assert "Backend" in hint
    assert "mark_onboarded" in hint
    assert "reflect_and_persist" in hint


def test_build_onboarding_hint_is_backend_neutral() -> None:
    """Page locations render as areas of the knowledge base -- the hint
    names no backend and no backend container vocabulary (spaces /
    projects), so it reads correctly on Confluence and Plane alike."""
    org = _build_org()
    hint = build_onboarding_hint(Role(name="Backend Engineer"), org)
    assert "- 'Onboarding' page in the **Acme** area of the knowledge base" in hint
    assert (
        "- 'Onboarding' page in the **Engineering** area of the knowledge base" in hint
    )
    lowered = hint.lower()
    assert "confluence" not in lowered
    assert "atlassian" not in lowered
    assert "space" not in lowered
    assert "plane" not in lowered


async def test_is_onboarded_false_when_no_marker() -> None:
    store = _MarkerStoreStub()
    assert (
        await is_onboarded(marker_store=store, agent_id="a-1", expected_chain_hash="x")
        is False
    )


async def test_is_onboarded_false_when_store_unwired() -> None:
    """Test / in-memory mode -- no marker store, hint always renders."""
    assert (
        await is_onboarded(marker_store=None, agent_id="a-1", expected_chain_hash="x")
        is False
    )


async def test_is_onboarded_true_when_hash_matches() -> None:
    store = _MarkerStoreStub()
    await store.mark(agent_id="a-1", chain_hash="abc")
    assert (
        await is_onboarded(
            marker_store=store, agent_id="a-1", expected_chain_hash="abc"
        )
        is True
    )


async def test_is_onboarded_false_when_hash_stale() -> None:
    store = _MarkerStoreStub()
    await store.mark(agent_id="a-1", chain_hash="old-hash")
    assert (
        await is_onboarded(
            marker_store=store, agent_id="a-1", expected_chain_hash="new-hash"
        )
        is False
    )


async def test_db_store_claim_failure_returns_false() -> None:
    # A claim that ERRORS must lose (False), never win: running a possibly
    # duplicate pass on unknown lease state defeats the single-flight.
    from crewlet.learning.onboarding_markers import OnboardingMarkerStore

    class _BrokenDb:
        async def fetchrow(self, *a: Any, **k: Any) -> dict[str, Any] | None:
            raise RuntimeError("connection reset")

        async def execute(self, *a: Any, **k: Any) -> list[dict[str, Any]]:
            raise RuntimeError("connection reset")

    store = OnboardingMarkerStore(_BrokenDb())  # type: ignore[arg-type]
    assert await store.try_claim_pass(agent_id="a-1", ttl_seconds=900.0) is False
    # And release is best-effort — a broken DB must not raise.
    await store.release_claim("a-1")


async def test_db_store_lookup_failure_is_unknown_not_false() -> None:
    # A read failure must surface as UNKNOWN (None), never "not onboarded" —
    # collapsing it into False re-ran whole onboarding passes for
    # already-marked agents on transient DB errors.
    from crewlet.learning.onboarding_markers import OnboardingMarkerStore

    class _BrokenDb:
        async def execute(self, *a: Any, **k: Any) -> list[dict[str, Any]]:
            raise RuntimeError("connection reset")

    store = OnboardingMarkerStore(_BrokenDb())  # type: ignore[arg-type]
    assert await store.is_onboarded(agent_id="a-1", chain_hash="x") is None


async def test_mark_onboarded_hashes_against_context_org() -> None:
    # The mark tool must hash the LIVE org on the context (what the check
    # paths hash), not the org captured at tool registration — after a live
    # config reload those can diverge and the stored marker would never
    # match, re-onboarding forever.
    registration_org = _build_org()
    live_org = Organization(
        name="Acme",
        units=[
            OrgUnit(
                name="Platform",  # Backend team renamed → different chain hash
                type="team",
                lead="Backend Lead",
                roles=[Role(name="Backend Lead"), Role(name="Backend Engineer")],
            )
        ],
    )
    store = _MarkerStoreStub()
    registry = ToolRegistry()
    register_mark_onboarded_tool(registry, store, registration_org)
    tool = registry.get(MARK_ONBOARDED_TOOL)
    assert tool is not None

    ctx = AgentContext(
        agent_id="a-9",
        agent_handle="be",
        role="Backend Engineer",
        org=live_org,
    )
    result = await tool.execute({"summary": "read the pages"}, ctx)
    assert result.success is True
    expected = compute_chain_hash(live_org.get_role("Backend Engineer"), live_org)
    assert store.rows["a-9"]["chain_hash"] == expected


async def test_register_mark_onboarded_tool_writes_marker() -> None:
    org = _build_org()
    store = _MarkerStoreStub()
    registry = ToolRegistry()
    register_mark_onboarded_tool(registry, store, org)

    tool = registry.get(MARK_ONBOARDED_TOOL)
    assert tool is not None
    ctx = AgentContext(
        agent_id="agent-1",
        agent_handle="backend-eng",
        role="Backend Engineer",
    )
    result = await tool.execute({"summary": "Read engineering + backend pages."}, ctx)
    assert result.success is True
    assert len(store.marks) == 1
    mark = store.marks[0]
    assert mark["agent_id"] == "agent-1"
    assert mark["chain_hash"] == compute_chain_hash(Role(name="Backend Engineer"), org)
    assert mark["agent_handle"] == "backend-eng"
    assert mark["role"] == "Backend Engineer"
    assert mark["summary"] == "Read engineering + backend pages."


async def test_mark_onboarded_requires_summary() -> None:
    org = _build_org()
    store = _MarkerStoreStub()
    registry = ToolRegistry()
    register_mark_onboarded_tool(registry, store, org)
    tool = registry.get(MARK_ONBOARDED_TOOL)
    ctx = AgentContext(
        agent_id="agent-1",
        agent_handle="x",
        role="Backend Engineer",
    )
    result = await tool.execute({}, ctx)
    assert result.success is False
    assert "summary is required" in (result.error or "")


async def test_mark_onboarded_rejects_unknown_role() -> None:
    org = _build_org()
    store = _MarkerStoreStub()
    registry = ToolRegistry()
    register_mark_onboarded_tool(registry, store, org)
    tool = registry.get(MARK_ONBOARDED_TOOL)
    ctx = AgentContext(
        agent_id="agent-1",
        agent_handle="x",
        role="Phantom Role",
    )
    result = await tool.execute({"summary": "x"}, ctx)
    assert result.success is False
    assert "not found" in (result.error or "")


async def test_mark_onboarded_overwrites_prior_marker_via_upsert() -> None:
    """The marker is keyed by ``agent_id`` so a fresh ``mark`` UPSERTs
    the existing row -- stale chain hashes never accumulate."""
    org = _build_org()
    store = _MarkerStoreStub()
    await store.mark(agent_id="agent-1", chain_hash="STALE")
    registry = ToolRegistry()
    register_mark_onboarded_tool(registry, store, org)
    tool = registry.get(MARK_ONBOARDED_TOOL)
    ctx = AgentContext(
        agent_id="agent-1",
        agent_handle="backend-eng",
        role="Backend Engineer",
    )
    result = await tool.execute({"summary": "fresh onboarding"}, ctx)
    assert result.success is True
    assert len(store.rows) == 1  # one row, overwritten
    assert store.rows["agent-1"]["chain_hash"] != "STALE"


async def test_full_onboarding_cycle_suppresses_hint_after_mark() -> None:
    """End-to-end: empty store -> hint fires, mark_onboarded -> hint
    suppressed for the same chain hash."""
    org = _build_org()
    store = _MarkerStoreStub()
    role = Role(name="Backend Engineer")
    chain_hash = compute_chain_hash(role, org)

    assert (
        await is_onboarded(
            marker_store=store,
            agent_id="agent-1",
            expected_chain_hash=chain_hash,
        )
        is False
    )

    registry = ToolRegistry()
    register_mark_onboarded_tool(registry, store, org)
    ctx = AgentContext(
        agent_id="agent-1",
        agent_handle="backend-eng",
        role="Backend Engineer",
    )
    result = await registry.get(MARK_ONBOARDED_TOOL).execute(
        {"summary": "internalised conventions"}, ctx
    )
    assert result.success is True

    assert (
        await is_onboarded(
            marker_store=store,
            agent_id="agent-1",
            expected_chain_hash=chain_hash,
        )
        is True
    )
