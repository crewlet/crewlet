"""Tests for ``crewlet.agent.skills.sync.ToolSkillSyncWorker``.

Drives the worker against an in-memory fake of ``ConfluenceTransport``
(just enough surface for ``_http_client`` + ``_rest_base``) and an
``httpx.MockTransport`` for the HTTP layer. No real network.
"""

from __future__ import annotations

import json
from pathlib import Path

import httpx
import pytest

from crewlet.agent.skills import (
    PromptSkillRegistry,
    ToolSkillSyncWorker,
)
from crewlet.agent.skills.confluence_codec import encode_page
from crewlet.agent.skills.parser import parse_skill_file

EXAMPLES_DIR = Path(__file__).resolve().parents[3] / "examples" / "tool-skills"


class _FakeTransport:
    """Bare-minimum stand-in for ConfluenceTransport.

    Carries an httpx async client and a REST base. The sync worker
    consumes only these two attributes via getattr / properties.
    """

    def __init__(self, handler: httpx.MockTransport) -> None:
        self._http_client = httpx.AsyncClient(transport=handler)
        self._rest_base = "https://confluence.example.com/wiki"


def _page_payload(
    page_id: str,
    frontmatter: dict,
    body: str,
    version: int = 1,
    *,
    space_key: str = "TS",
    labels: tuple[str, ...] = ("crewlet-skill",),
) -> dict:
    return {
        "id": page_id,
        "title": frontmatter.get("title", "Skill"),
        "version": {"number": version},
        "body": {"storage": {"value": encode_page(frontmatter, body)}},
        # The webhook fetch expands space + labels for the admission
        # predicate; the boot walk's CQL pre-filters them server-side.
        "space": {"key": space_key},
        "metadata": {"labels": {"results": [{"name": name} for name in labels]}},
    }


def _bundled_payloads() -> list[dict]:
    payloads = []
    for i, path in enumerate(sorted(EXAMPLES_DIR.glob("*.md")), start=1):
        frontmatter, body = parse_skill_file(path.read_text())
        payloads.append(_page_payload(str(100 + i), frontmatter, body, version=i))
    return payloads


@pytest.mark.asyncio
async def test_initial_sync_loads_every_skill_page_via_cql_label_filter() -> None:
    pages = _bundled_payloads()

    def handler(request: httpx.Request) -> httpx.Response:
        # The sync uses the CQL search endpoint (not the bare list
        # endpoint) so it can scope to pages bearing the shared
        # ``crewlet-skill`` marker label and skip the space's home
        # page + any other non-skill content.
        assert request.url.path.endswith("/rest/api/content/search")
        cql = request.url.params.get("cql", "")
        assert 'space = "TS"' in cql
        assert 'label = "crewlet-skill"' in cql
        start = int(request.url.params.get("start", "0"))
        limit = int(request.url.params.get("limit", "50"))
        page_slice = pages[start : start + limit]
        return httpx.Response(
            200,
            json={"results": page_slice, "size": len(page_slice)},
        )

    fake = _FakeTransport(httpx.MockTransport(handler))
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")
    loaded = await worker.run_initial_sync()
    assert loaded == len(pages)
    # Every example file ends up registered, keyed by its declared key.
    keys = reg.keys()
    assert "mcp:github" in keys
    assert "tool:reflect_and_persist" in keys
    assert "skill:platform_mentions" in keys


@pytest.mark.asyncio
async def test_initial_sync_skips_malformed_pages_without_aborting() -> None:
    good = _bundled_payloads()[0]
    bad_no_macro = {
        "id": "999",
        "title": "bad",
        "version": {"number": 1},
        "body": {"storage": {"value": "<p>no code macro here</p>"}},
    }
    pages = [good, bad_no_macro]

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"results": pages, "size": len(pages)})

    fake = _FakeTransport(httpx.MockTransport(handler))
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")
    loaded = await worker.run_initial_sync()
    assert loaded == 1  # only the good page made it through


@pytest.mark.asyncio
async def test_initial_sync_ignores_space_home_page_via_label_filter() -> None:
    """The CQL ``label = "crewlet-skill"`` filter means the space's
    auto-generated home page (no label) never reaches the decoder, so
    there's no ``tool_skill_sync_decode_failed`` log on every boot.
    Modelled here by the mock returning *only* labelled pages — what
    Confluence would actually do given the CQL — and asserting the
    sync builds a normal registry from them.
    """
    skill_page = _bundled_payloads()[0]

    captured: dict[str, str] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        # Confluence applies the CQL server-side; we just record it
        # and return the post-filter set (home page absent).
        captured["cql"] = request.url.params.get("cql", "")
        return httpx.Response(200, json={"results": [skill_page], "size": 1})

    fake = _FakeTransport(httpx.MockTransport(handler))
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")
    loaded = await worker.run_initial_sync()
    # CQL was issued with the marker-label filter, so Confluence (real
    # or, here, mocked) never hands the sync the home page in the
    # first place.
    assert 'label = "crewlet-skill"' in captured["cql"]
    assert loaded == 1


@pytest.mark.asyncio
async def test_handle_page_event_upserts_on_update() -> None:
    page = _bundled_payloads()[0]
    page_id = page["id"]

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path.endswith(f"/rest/api/content/{page_id}")
        # The admission predicate needs the space + marker labels, so
        # the fetch must expand them alongside the body/version.
        expand = request.url.params.get("expand", "")
        assert "space" in expand
        assert "metadata.labels" in expand
        return httpx.Response(200, json=page)

    fake = _FakeTransport(httpx.MockTransport(handler))
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")
    await worker.handle_page_event(page_id=page_id, event_kind="page_updated")
    assert len(reg) == 1


@pytest.mark.asyncio
async def test_handle_page_event_evicts_on_delete() -> None:
    page = _bundled_payloads()[0]
    page_id = page["id"]
    # Seed via initial sync so we have one skill in the registry.
    seed_handler = httpx.MockTransport(
        lambda req: httpx.Response(200, json={"results": [page], "size": 1})
    )
    fake = _FakeTransport(seed_handler)
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")
    await worker.run_initial_sync()
    assert len(reg) == 1
    # Now delete that page.
    await worker.handle_page_event(page_id=page_id, event_kind="page_trashed")
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_handle_page_event_evicts_on_404_fetch() -> None:
    """A page_updated event for a page that's been deleted in the
    interim → 404 → evict instead of crashing."""
    page = _bundled_payloads()[0]
    page_id = page["id"]
    seed_pages = [page]
    state = {"seeded": False}

    def handler(request: httpx.Request) -> httpx.Response:
        if not state["seeded"]:
            state["seeded"] = True
            return httpx.Response(
                200, json={"results": seed_pages, "size": len(seed_pages)}
            )
        # Subsequent request → page is now gone.
        return httpx.Response(404, json={"message": "page not found"})

    fake = _FakeTransport(httpx.MockTransport(handler))
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")
    await worker.run_initial_sync()
    assert len(reg) == 1
    await worker.handle_page_event(page_id=page_id, event_kind="page_updated")
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_handle_page_event_handles_non_200_gracefully() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(503, text="server error")

    fake = _FakeTransport(httpx.MockTransport(handler))
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")
    # Should not raise.
    await worker.handle_page_event(page_id="42", event_kind="page_updated")
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_initial_sync_without_http_client_returns_failure() -> None:
    """Engine boot may call sync before Confluence is reachable; sync
    degrades gracefully rather than raising — and reports the walk as
    FAILED (``None``) so the engine's bounded retry distinguishes it
    from a legitimately empty space (``0``)."""

    class Stub:
        _http_client = None
        _rest_base = ""

    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=Stub(), registry=reg, space_key="TS")
    assert await worker.run_initial_sync() is None
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_initial_sync_walk_failure_returns_none_and_keeps_registry() -> None:
    """A non-200 mid-walk is a FAILED walk: ``None``, and the registry
    keeps its previous contents (never seeded from partial rows)."""
    page = _bundled_payloads()[0]
    seed_handler = httpx.MockTransport(
        lambda req: httpx.Response(200, json={"results": [page], "size": 1})
    )
    fake = _FakeTransport(seed_handler)
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")
    assert await worker.run_initial_sync() == 1

    broken = _FakeTransport(
        httpx.MockTransport(lambda req: httpx.Response(503, text="down"))
    )
    worker = ToolSkillSyncWorker(transport=broken, registry=reg, space_key="TS")
    assert await worker.run_initial_sync() is None
    assert len(reg) == 1


def _seeded_worker_and_registry(
    page: dict, followup: httpx.Response
) -> tuple[ToolSkillSyncWorker, PromptSkillRegistry]:
    """Build a worker whose first request seeds ``page`` and whose
    subsequent requests get ``followup``."""
    state = {"seeded": False}

    def handler(request: httpx.Request) -> httpx.Response:
        if not state["seeded"]:
            state["seeded"] = True
            return httpx.Response(200, json={"results": [page], "size": 1})
        return followup

    fake = _FakeTransport(httpx.MockTransport(handler))
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")
    return worker, reg


@pytest.mark.asyncio
async def test_handle_page_event_evicts_on_space_mismatch() -> None:
    """The webhook admission predicate mirrors the boot CQL's space
    filter: the index callback fires for page webhooks instance-wide,
    and a page fetched from a foreign space must be evicted (a page
    MOVED out of the space stops being a skill), never upserted."""
    frontmatter, body = parse_skill_file(
        sorted(EXAMPLES_DIR.glob("*.md"))[0].read_text()
    )
    seed_page = _page_payload("101", frontmatter, body)
    moved = _page_payload("101", frontmatter, body, space_key="ENG")
    worker, reg = _seeded_worker_and_registry(
        seed_page, httpx.Response(200, json=moved)
    )
    await worker.run_initial_sync()
    assert len(reg) == 1
    await worker.handle_page_event(page_id="101", event_kind="page_updated")
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_handle_page_event_evicts_on_missing_marker_label() -> None:
    """A TS-space page without the ``crewlet-skill`` marker label fails
    the admission predicate — loading it would create a skill that
    silently vanishes on the next restart's CQL-filtered walk, so the
    webhook path evicts instead."""
    frontmatter, body = parse_skill_file(
        sorted(EXAMPLES_DIR.glob("*.md"))[0].read_text()
    )
    seed_page = _page_payload("101", frontmatter, body)
    unlabelled = _page_payload("101", frontmatter, body, labels=())
    worker, reg = _seeded_worker_and_registry(
        seed_page, httpx.Response(200, json=unlabelled)
    )
    await worker.run_initial_sync()
    assert len(reg) == 1
    await worker.handle_page_event(page_id="101", event_kind="page_updated")
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_handle_page_event_unadmitted_page_never_upserts() -> None:
    """A decodable page from a foreign space that was never in the
    registry stays out of it (evict is a no-op)."""
    frontmatter, body = parse_skill_file(
        sorted(EXAMPLES_DIR.glob("*.md"))[0].read_text()
    )
    foreign = _page_payload("999", frontmatter, body, space_key="ENG")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json=foreign)

    fake = _FakeTransport(httpx.MockTransport(handler))
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")
    await worker.handle_page_event(page_id="999", event_kind="page_created")
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_handle_page_event_evicts_on_decode_failure() -> None:
    """A skill page edited into a non-skill (body no longer decodes)
    must not keep serving its last-good body — the update evicts it."""
    frontmatter, body = parse_skill_file(
        sorted(EXAMPLES_DIR.glob("*.md"))[0].read_text()
    )
    seed_page = _page_payload("101", frontmatter, body)
    broken = dict(seed_page)
    broken["body"] = {"storage": {"value": "<p>someone deleted the macro</p>"}}
    worker, reg = _seeded_worker_and_registry(
        seed_page, httpx.Response(200, json=broken)
    )
    await worker.run_initial_sync()
    assert len(reg) == 1
    await worker.handle_page_event(page_id="101", event_kind="page_updated")
    assert len(reg) == 0


@pytest.mark.asyncio
async def test_lowercased_space_key_upserts_instead_of_evicting() -> None:
    """A case-mismatched CREWLET_TOOL_SKILLS_SPACE (e.g. ``ts`` for a
    space Confluence stores as ``TS``) boots fine — CQL resolves the
    key server-side — so the webhook admission predicate must compare
    case-insensitively too, like every other space-key comparison in
    the codebase.  A case-sensitive comparison would EVICT every skill
    page on its first edit over the casing mismatch."""
    frontmatter, body = parse_skill_file(
        sorted(EXAMPLES_DIR.glob("*.md"))[0].read_text()
    )
    # Confluence returns the stored (uppercased) key on the page.
    page = _page_payload("101", frontmatter, body, space_key="TS")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/rest/api/content/search"):
            return httpx.Response(200, json={"results": [page], "size": 1})
        return httpx.Response(200, json=page)

    fake = _FakeTransport(httpx.MockTransport(handler))
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="ts")
    assert worker.space_key == "TS"  # normalised once, used in CQL too
    assert await worker.run_initial_sync() == 1
    await worker.handle_page_event(page_id="101", event_kind="page_updated")
    assert len(reg) == 1  # upserted, NOT evicted


@pytest.mark.asyncio
async def test_handle_page_event_key_rename_evicts_old_key() -> None:
    """Editing a skill page's ``key`` upserts under the new key and
    drops the old-key entry sourced from the same page — no ghost."""
    frontmatter, body = parse_skill_file(
        sorted(EXAMPLES_DIR.glob("*.md"))[0].read_text()
    )
    seed_page = _page_payload("101", frontmatter, body)
    renamed_fm = dict(frontmatter)
    renamed_fm["key"] = "tool:renamed_key"
    renamed = _page_payload("101", renamed_fm, body, version=2)
    worker, reg = _seeded_worker_and_registry(
        seed_page, httpx.Response(200, json=renamed)
    )
    await worker.run_initial_sync()
    assert len(reg) == 1
    old_key = next(iter(reg.keys()))
    await worker.handle_page_event(page_id="101", event_kind="page_updated")
    assert reg.keys() == ["tool:renamed_key"]
    assert old_key not in reg


@pytest.mark.asyncio
async def test_skill_keyed_by_declared_key_not_page_id() -> None:
    """The skill's identity is its declared ``key`` (from frontmatter),
    not the page id — re-syncing the same page produces only one entry."""
    page = _bundled_payloads()[0]

    def handler(request: httpx.Request) -> httpx.Response:
        # Always return the same single page, but pretend version
        # bumps each call so we can assert the upsert overwrite.
        bumped = json.loads(json.dumps(page))
        bumped["version"]["number"] += 1
        return httpx.Response(200, json=bumped)

    fake = _FakeTransport(httpx.MockTransport(handler))
    reg = PromptSkillRegistry()
    worker = ToolSkillSyncWorker(transport=fake, registry=reg, space_key="TS")

    await worker.handle_page_event(page_id=page["id"], event_kind="page_updated")
    await worker.handle_page_event(page_id=page["id"], event_kind="page_updated")
    assert len(reg) == 1
