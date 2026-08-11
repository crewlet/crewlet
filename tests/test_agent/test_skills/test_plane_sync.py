"""Tests for ``crewlet.agent.skills.plane_sync.PlaneSkillSyncWorker``.

Drives the worker against in-memory fakes of the ``PlaneTransport``
public client seam (``engine_client`` + ``resolve_project_ids``) and
the ``PlaneClient`` page surface. No real network.
"""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.agent.skills.plane_sync import (
    SYNC_PAGE_FIELDS,
    PlaneSkillSyncWorker,
    page_to_skill,
)
from crewlet.agent.skills.registry import PromptSkillRegistry
from crewlet.plane.client import PlaneProvisionError
from crewlet.plane.plane_codec import encode_page

TS_UUID = "6f000000-0000-4000-8000-00000000c0de"


def _skill_frontmatter(key: str = "tool:jira_search") -> dict[str, Any]:
    return {
        "key": key,
        "trigger": {"tool": "jira_search"},
        "phases": ["plan"],
        "title": f"Skill {key}",
        "summary": f"How to use {key}.",
    }


def _skill_page(
    page_id: str,
    key: str = "tool:jira_search",
    *,
    external: bool = True,
    archived_at: str | None = None,
) -> dict[str, Any]:
    page: dict[str, Any] = {
        "id": page_id,
        "name": f"Skill {key}",
        "description_html": encode_page(_skill_frontmatter(key), f"Body of {key}."),
        "external_id": f"skill:{key}" if external else None,
        "external_source": "crewlet" if external else None,
        "archived_at": archived_at,
    }
    return page


def _home_page(page_id: str = "home-1") -> dict[str, Any]:
    return {
        "id": page_id,
        "name": "Tool Skills",
        "description_html": "<p>Welcome to the Tool Skills project.</p>",
        "external_id": None,
        "external_source": None,
        "archived_at": None,
    }


class _FakePlaneClient:
    """Duck-typed ``PlaneClient`` covering the worker's read surface."""

    def __init__(
        self,
        *,
        list_rows: list[dict[str, Any]] | None = None,
        get_pages: dict[str, dict[str, Any]] | None = None,
        list_error: Exception | None = None,
        get_error: Exception | None = None,
    ) -> None:
        self.list_rows = list_rows or []
        self.get_pages = get_pages or {}
        self.list_error = list_error
        self.get_error = get_error
        self.list_calls: list[dict[str, Any]] = []
        self.get_calls: list[tuple[str, str]] = []

    async def list_pages(
        self,
        project_id: str,
        *,
        search: str | None = None,
        fields: list[str] | None = None,
        page_type: str | None = None,
    ) -> list[dict[str, Any]]:
        self.list_calls.append(
            {"project_id": project_id, "search": search, "fields": fields}
        )
        if self.list_error is not None:
            raise self.list_error
        return list(self.list_rows)

    async def get_page(self, project_id: str, page_id: str) -> dict[str, Any]:
        self.get_calls.append((project_id, page_id))
        if self.get_error is not None:
            raise self.get_error
        page = self.get_pages.get(page_id)
        if page is None:
            raise PlaneProvisionError(
                "not found",
                method="GET",
                path=f"/pages/{page_id}/",
                status=404,
            )
        return page


class _FakeTransport:
    """Duck-typed ``PlaneTransport`` public client seam."""

    def __init__(
        self,
        client: _FakePlaneClient | None,
        mapping: dict[str, str] | None = None,
        resolve_error: Exception | None = None,
    ) -> None:
        self._client = client
        self._mapping = {"TS": TS_UUID} if mapping is None else mapping
        self._resolve_error = resolve_error

    @property
    def engine_client(self) -> _FakePlaneClient | None:
        return self._client

    async def resolve_project_ids(
        self, identifiers: Any, *, client: Any = None
    ) -> dict[str, str]:
        if self._resolve_error is not None:
            # The real transport RAISES when the refresh request itself
            # fails (distinct from "identifier absent").
            raise self._resolve_error
        wanted = {str(i).strip().upper() for i in identifiers if i}
        return {k: v for k, v in self._mapping.items() if k in wanted}


def _worker(
    client: _FakePlaneClient | None,
    *,
    registry: PromptSkillRegistry | None = None,
    mapping: dict[str, str] | None = None,
    project: str = "TS",
    resolve_error: Exception | None = None,
) -> tuple[PlaneSkillSyncWorker, PromptSkillRegistry]:
    reg = registry if registry is not None else PromptSkillRegistry()
    worker = PlaneSkillSyncWorker(
        transport=_FakeTransport(client, mapping, resolve_error),  # type: ignore[arg-type]
        registry=reg,
        project=project,
    )
    return worker, reg


# ----------------------------------------------------------------------
# Boot walk
# ----------------------------------------------------------------------


@pytest.mark.asyncio
async def test_initial_sync_seeds_only_decodable_trigger_pages() -> None:
    """The home page (no YAML block) and a non-crewlet stray page skip
    silently; skill pages land keyed by their declared key."""
    client = _FakePlaneClient(
        list_rows=[
            _skill_page("P-1", "tool:jira_search"),
            _skill_page("P-2", "mcp:github"),
            _home_page(),
        ]
    )
    worker, reg = _worker(client)
    assert await worker.run_initial_sync() == 2
    assert sorted(reg.keys()) == ["mcp:github", "tool:jira_search"]
    skill = reg.get("mcp:github")
    assert skill is not None
    assert skill.source_page_id == "P-2"
    assert skill.source_page_version == 0


@pytest.mark.asyncio
async def test_initial_sync_is_one_enumeration_no_page_fetches() -> None:
    """Plane page-list rows carry ``description_html`` via ``fields=`` —
    the boot walk is ONE enumeration, never a per-page GET fan-out."""
    client = _FakePlaneClient(list_rows=[_skill_page("P-1")])
    worker, _reg = _worker(client)
    await worker.run_initial_sync()
    assert len(client.list_calls) == 1
    assert client.list_calls[0]["project_id"] == TS_UUID
    assert client.list_calls[0]["fields"] == SYNC_PAGE_FIELDS
    assert "description_html" in client.list_calls[0]["fields"]
    assert client.get_calls == []


@pytest.mark.asyncio
async def test_initial_sync_replaces_stale_registry_entries() -> None:
    """seed() is a wholesale replace: a page deleted while the engine
    was down drops out on the next walk."""
    client = _FakePlaneClient(list_rows=[_skill_page("P-1", "tool:kept")])
    reg = PromptSkillRegistry()
    reg.seed(
        [
            page_to_skill(_skill_page("P-9", "tool:stale")),  # type: ignore[list-item]
        ]
    )
    worker, reg = _worker(client, registry=reg)
    assert await worker.run_initial_sync() == 1
    assert reg.keys() == ["tool:kept"]


@pytest.mark.asyncio
async def test_initial_sync_no_project_configured_returns_zero() -> None:
    client = _FakePlaneClient(list_rows=[_skill_page("P-1")])
    worker, reg = _worker(client, project="")
    assert await worker.run_initial_sync() == 0
    assert len(reg) == 0
    assert client.list_calls == []


@pytest.mark.asyncio
async def test_initial_sync_without_engine_client_returns_failure() -> None:
    """No engine client = the walk cannot run — a FAILURE (``None``,
    registry untouched, engine retry loop keeps trying / goes loud),
    never a silent 0 that reads like an empty project."""
    worker, reg = _worker(None)
    assert await worker.run_initial_sync() is None
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_initial_sync_unresolved_project_returns_failure() -> None:
    """Identifier genuinely absent after a SUCCESSFUL resolution —
    logged as a configuration error, and still a failed walk: the
    registry keeps its contents."""
    stale = page_to_skill(_skill_page("P-9", "tool:survivor"))
    assert stale is not None
    reg = PromptSkillRegistry()
    reg.seed([stale])
    client = _FakePlaneClient(list_rows=[_skill_page("P-1")])
    worker, reg = _worker(client, mapping={}, registry=reg)
    assert await worker.run_initial_sync() is None
    assert reg.keys() == ["tool:survivor"]
    assert client.list_calls == []


@pytest.mark.asyncio
async def test_initial_sync_resolution_request_failure_is_distinct(caplog) -> None:
    """A transient ``list_projects()`` failure at boot (the compose
    race: Plane not yet accepting connections) must surface as
    ``tool_skill_sync_list_failed`` — a retryable walk failure — and
    leave the registry untouched.  It must NOT be conflated with
    ``tool_skill_project_not_found`` (a configuration error)."""
    stale = page_to_skill(_skill_page("P-9", "tool:survivor"))
    assert stale is not None
    reg = PromptSkillRegistry()
    reg.seed([stale])
    client = _FakePlaneClient(list_rows=[_skill_page("P-1")])
    worker, reg = _worker(
        client,
        registry=reg,
        resolve_error=RuntimeError("connection refused (plane still booting)"),
    )
    assert await worker.run_initial_sync() is None
    assert reg.keys() == ["tool:survivor"]
    assert client.list_calls == []
    assert "tool_skill_sync_list_failed" in caplog.text
    assert "tool_skill_project_not_found" not in caplog.text


@pytest.mark.asyncio
async def test_initial_sync_incomplete_enumeration_never_seeds() -> None:
    """A strict walk that fails (pagination cap, cursor stall,
    transport error) leaves the registry EXACTLY as it was — a
    wholesale seed from partial rows would silently delete skills."""
    stale = page_to_skill(_skill_page("P-9", "tool:survivor"))
    assert stale is not None
    reg = PromptSkillRegistry()
    reg.seed([stale])
    client = _FakePlaneClient(
        list_error=PlaneProvisionError(
            "pagination cursor stalled after 100 rows", method="GET", path="/pages/"
        )
    )
    worker, reg = _worker(client, registry=reg)
    assert await worker.run_initial_sync() is None
    assert reg.keys() == ["tool:survivor"]


# ----------------------------------------------------------------------
# Webhook path
# ----------------------------------------------------------------------


async def _seeded(
    client: _FakePlaneClient,
) -> tuple[PlaneSkillSyncWorker, PromptSkillRegistry]:
    worker, reg = _worker(client)
    skill = page_to_skill(_skill_page("P-1", "tool:jira_search"))
    assert skill is not None
    reg.seed([skill])
    return worker, reg


@pytest.mark.asyncio
async def test_handle_page_event_deleted_evicts() -> None:
    client = _FakePlaneClient()
    worker, reg = await _seeded(client)
    await worker.handle_page_event(page_id="P-1", event_kind="page.deleted")
    assert len(reg) == 0
    assert client.get_calls == []  # delete never fetches


@pytest.mark.asyncio
@pytest.mark.parametrize("event_kind", ["page.created", "page.updated"])
async def test_handle_page_event_upserts_on_non_delete(event_kind: str) -> None:
    """The fork's action vocabulary is created|updated|deleted (a
    duplicate emits created); any non-delete fetches + upserts."""
    client = _FakePlaneClient(get_pages={"P-2": _skill_page("P-2", "mcp:github")})
    worker, reg = _worker(client)
    await worker.handle_page_event(page_id="P-2", event_kind=event_kind)
    assert reg.keys() == ["mcp:github"]
    skill = reg.get("mcp:github")
    assert skill is not None
    assert skill.source_page_id == "P-2"
    assert client.get_calls == [(TS_UUID, "P-2")]


@pytest.mark.asyncio
async def test_handle_page_event_unknown_action_still_upserts() -> None:
    """Forward-compatible rule: an unknown future action degrades to a
    harmless refetch + upsert, never a drop."""
    client = _FakePlaneClient(get_pages={"P-2": _skill_page("P-2", "mcp:github")})
    worker, reg = _worker(client)
    await worker.handle_page_event(page_id="P-2", event_kind="page.locked")
    assert reg.keys() == ["mcp:github"]


@pytest.mark.asyncio
async def test_handle_page_event_404_evicts() -> None:
    """The project-scoped GET 404s for deleted AND foreign-project
    pages — both evict (a no-op when the registry never held it)."""
    client = _FakePlaneClient(get_pages={})
    worker, reg = await _seeded(client)
    await worker.handle_page_event(page_id="P-1", event_kind="page.updated")
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_handle_page_event_foreign_project_is_noop() -> None:
    """A workspace-wide page event for a page in another project 404s
    on the scoped GET and leaves the registry untouched."""
    client = _FakePlaneClient(get_pages={})
    worker, reg = await _seeded(client)
    await worker.handle_page_event(page_id="P-OTHER", event_kind="page.created")
    assert reg.keys() == ["tool:jira_search"]


@pytest.mark.asyncio
async def test_handle_page_event_archived_evicts() -> None:
    """Archive is the operator's "remove" gesture (delete requires
    archive first) and fires ``updated`` — a fetched page with non-null
    ``archived_at`` evicts."""
    client = _FakePlaneClient(
        get_pages={
            "P-1": _skill_page("P-1", "tool:jira_search", archived_at="2026-07-27")
        }
    )
    worker, reg = await _seeded(client)
    await worker.handle_page_event(page_id="P-1", event_kind="page.updated")
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_handle_page_event_decode_failure_evicts() -> None:
    """A skill page edited into a non-skill must not keep serving its
    last-good body."""
    broken = _skill_page("P-1", "tool:jira_search")
    broken["description_html"] = "<p>the yaml block is gone</p>"
    client = _FakePlaneClient(get_pages={"P-1": broken})
    worker, reg = await _seeded(client)
    await worker.handle_page_event(page_id="P-1", event_kind="page.updated")
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_handle_page_event_key_rename_evicts_old_key() -> None:
    """Editing a skill page's ``key`` in the UI must not leave a ghost
    under the old key serving the stale trigger and body — after the
    update event exactly one key (the new one) remains."""
    renamed = _skill_page("P-1", "tool:renamed")
    client = _FakePlaneClient(get_pages={"P-1": renamed})
    worker, reg = await _seeded(client)  # holds tool:jira_search from P-1
    await worker.handle_page_event(page_id="P-1", event_kind="page.updated")
    assert reg.keys() == ["tool:renamed"]


@pytest.mark.asyncio
async def test_handle_page_event_server_error_leaves_registry() -> None:
    """A non-404 fetch failure is transient — no eviction, no upsert."""
    client = _FakePlaneClient(
        get_error=PlaneProvisionError(
            "boom", method="GET", path="/pages/P-1/", status=503
        )
    )
    worker, reg = await _seeded(client)
    await worker.handle_page_event(page_id="P-1", event_kind="page.updated")
    assert reg.keys() == ["tool:jira_search"]


@pytest.mark.asyncio
async def test_handle_page_event_without_client_is_noop() -> None:
    worker, reg = await _seeded(_FakePlaneClient())
    worker._transport._client = None  # type: ignore[attr-defined]
    await worker.handle_page_event(page_id="P-1", event_kind="page.updated")
    assert reg.keys() == ["tool:jira_search"]


@pytest.mark.asyncio
async def test_handle_page_event_unresolved_project_is_noop() -> None:
    client = _FakePlaneClient(get_pages={"P-1": _skill_page("P-1")})
    worker, reg = _worker(client, mapping={})
    await worker.handle_page_event(page_id="P-1", event_kind="page.updated")
    assert len(reg) == 0
    assert client.get_calls == []


# ----------------------------------------------------------------------
# page_to_skill
# ----------------------------------------------------------------------


def test_page_to_skill_requires_trigger() -> None:
    page = _skill_page("P-1")
    fm = _skill_frontmatter()
    del fm["trigger"]
    page["description_html"] = encode_page(fm, "body")
    assert page_to_skill(page) is None


def test_page_to_skill_invalid_definition_returns_none() -> None:
    fm = _skill_frontmatter()
    fm["phases"] = ["not-a-phase"]
    page = _skill_page("P-1")
    page["description_html"] = encode_page(fm, "body")
    assert page_to_skill(page) is None


def test_page_to_skill_empty_body_returns_none() -> None:
    page = _skill_page("P-1")
    page["description_html"] = ""
    assert page_to_skill(page) is None


def test_worker_exposes_project() -> None:
    client = _FakePlaneClient()
    worker, _reg = _worker(client, project="TS")
    assert worker.project == "TS"
    # The transport binding is internal — the engine's live-refresh
    # path tracks transports on its own attributes and rebuilds the
    # worker unconditionally, so no public staleness surface exists.
    assert worker._transport.engine_client is client
