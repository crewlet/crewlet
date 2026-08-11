"""Tests for ``crewlet confluence import`` — knowledge-doc path + routing.

Knowledge docs use a **directory-based convention**: pure prose with no
frontmatter, the target space is the file's parent directory name, and
the title is the file's first ``# H1`` (stripped from the published
body). They are published as clean prose (no YAML code macro), carry the
``crewlet-doc`` marker label, and are idempotent by ``(space, title)``.
These tests also cover the frontmatter ``parent`` override and the
per-space pre-flight that spans the skill space plus every doc
directory.  The backend-neutral parsing/classification behaviour lives
in ``tests/test_knowledge/test_markdown_docs.py``.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import httpx
import pytest

from crewlet.confluence import import_cli, pages
from crewlet.confluence.import_cli import import_one, run_import
from crewlet.confluence.knowledge import DOC_MARKER_LABEL

# A plain-prose knowledge doc: a leading H1 (→ the page title) and body,
# NO frontmatter. The space comes from the parent directory at publish
# time.
_DOC_MD = """# Manager 1:1

## What a 1:1 is

A short private session. See `query_episodes`.

- prepare
- converge
"""

_SKILL_MD = """---
key: tool:reflect_and_persist
trigger:
  tool: reflect_and_persist
phases: [plan]
title: Persisting durable facts
summary: |
  Persist declarative facts.
---

Persist durable facts. Write declarative facts, not instructions.
"""


class _FakeTransport:
    def __init__(self, handler: httpx.MockTransport) -> None:
        self._http_client = httpx.AsyncClient(transport=handler)
        self._rest_base = "https://confluence.example.com/wiki"

    async def stop(self) -> None:
        await self._http_client.aclose()


def _write_doc(tmp_path: Path, space: str, name: str, text: str) -> Path:
    """Write ``text`` to ``<tmp_path>/<space>/<name>`` so the doc's parent
    directory carries the target Confluence space key."""
    space_dir = tmp_path / space
    space_dir.mkdir(parents=True, exist_ok=True)
    doc = space_dir / name
    doc.write_text(text)
    return doc


# ---------------------------------------------------------------------------
# Doc create / skip / update / dry-run
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_doc_create_clean_storage_title_and_label(tmp_path: Path) -> None:
    """A new doc page (no frontmatter, H1 title, under <root>/LEAD/) →
    publishes to space LEAD, title from the H1, clean storage with the H1
    stripped (no leading <h1>) and no code macro, carries crewlet-doc."""
    doc = _write_doc(tmp_path, "LEAD", "page.md", _DOC_MD)
    create_payloads: list[dict[str, Any]] = []
    label_payloads: list[Any] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":  # find-by-title → none
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            label_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            create_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"id": "555"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, doc, update=False, dry_run=False, skill_space="TS")
    assert len(create_payloads) == 1
    body = create_payloads[0]
    # Space from the parent directory; title from the H1.
    assert body["space"]["key"] == "LEAD"
    assert body["title"] == "Manager 1:1"
    storage = body["body"]["storage"]["value"]
    # Clean prose — NO skill code macro.
    assert '<ac:structured-macro ac:name="code">' not in storage
    assert "ac:structured-macro" not in storage
    # H1 stripped — the page body must NOT repeat the title.
    assert "<h1>" not in storage
    # Body markdown still rendered to XHTML.
    assert "<h2>" in storage
    # Label includes the doc marker + the per-doc key label.
    assert len(label_payloads) == 1
    names = [entry["name"] for entry in label_payloads[0]]
    assert DOC_MARKER_LABEL in names
    assert any(name.startswith("crewlet-doc-key-") for name in names)
    await fake.stop()


@pytest.mark.asyncio
async def test_doc_frontmatter_title_overrides_h1(tmp_path: Path) -> None:
    """A frontmatter ``title:`` wins over the H1, and the H1 is still
    stripped from the body."""
    doc = _write_doc(
        tmp_path,
        "LEAD",
        "page.md",
        "---\ntitle: Override Title\n---\n\n# Ignored H1\n\n## Section\n\nBody.\n",
    )
    create_payloads: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            create_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"id": "1"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, doc, update=False, dry_run=False, skill_space="TS")
    assert len(create_payloads) == 1
    body = create_payloads[0]
    assert body["title"] == "Override Title"
    storage = body["body"]["storage"]["value"]
    assert "<h1>" not in storage  # leading H1 stripped even when overridden
    assert "Ignored H1" not in storage
    await fake.stop()


@pytest.mark.asyncio
async def test_doc_includes_extra_labels_from_frontmatter(tmp_path: Path) -> None:
    doc = _write_doc(
        tmp_path,
        "LEAD",
        "page.md",
        "---\nlabels: [playbook, leadership]\n---\n\n# T\n\nBody.\n",
    )
    label_payloads: list[Any] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            label_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            return httpx.Response(200, json={"id": "1"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, doc, update=False, dry_run=False, skill_space="TS")
    names = [entry["name"] for entry in label_payloads[0]]
    assert "playbook" in names
    assert "leadership" in names
    await fake.stop()


@pytest.mark.asyncio
async def test_doc_frontmatter_parent_resolved_to_ancestor(tmp_path: Path) -> None:
    """A frontmatter ``parent`` naming an existing page nests the new doc
    under it — the create POST carries ``ancestors`` with the resolved
    parent id."""
    doc = _write_doc(
        tmp_path,
        "LEAD",
        "page.md",
        "---\nparent: Team Handbook\n---\n\n# Nested Doc\n\nBody.\n",
    )
    create_payloads: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            query = request.url.query.decode()
            if "Team+Handbook" in query or "Team%20Handbook" in query:
                # The parent lookup finds the page.
                return httpx.Response(
                    200, json={"results": [{"id": "321", "version": {"number": 2}}]}
                )
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            create_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"id": "1"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, doc, update=False, dry_run=False, skill_space="TS")
    assert len(create_payloads) == 1
    assert create_payloads[0]["ancestors"] == [{"id": "321"}]
    await fake.stop()


@pytest.mark.asyncio
async def test_doc_frontmatter_parent_missing_creates_at_root(
    tmp_path: Path,
) -> None:
    """A ``parent`` naming a page that doesn't exist falls back to the
    space root (no ``ancestors`` in the POST) with a hint log — the doc
    still publishes."""
    doc = _write_doc(
        tmp_path,
        "LEAD",
        "page.md",
        "---\nparent: No Such Page\n---\n\n# Orphan Doc\n\nBody.\n",
    )
    create_payloads: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            create_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"id": "1"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, doc, update=False, dry_run=False, skill_space="TS")
    assert len(create_payloads) == 1
    assert "ancestors" not in create_payloads[0]
    await fake.stop()


@pytest.mark.asyncio
async def test_doc_skips_existing_without_update(tmp_path: Path) -> None:
    """Found by title (in the derived space) → skipped unless --update."""
    doc = _write_doc(tmp_path, "LEAD", "page.md", _DOC_MD)
    posts_or_puts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal posts_or_puts
        if request.method == "GET":
            return httpx.Response(
                200, json={"results": [{"id": "9", "version": {"number": 1}}]}
            )
        posts_or_puts += 1
        return httpx.Response(200, json={"id": "9"})

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, doc, update=False, dry_run=False, skill_space="TS")
    assert posts_or_puts == 0
    await fake.stop()


@pytest.mark.asyncio
async def test_doc_updates_existing_with_flag(tmp_path: Path) -> None:
    doc = _write_doc(tmp_path, "LEAD", "page.md", _DOC_MD)
    put_payloads: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(
                200, json={"results": [{"id": "9", "version": {"number": 4}}]}
            )
        if request.method == "POST" and request.url.path.endswith("/label"):
            return httpx.Response(200, json={"results": []})
        if request.method == "PUT":
            put_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"id": "9"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, doc, update=True, dry_run=False, skill_space="TS")
    assert len(put_payloads) == 1
    assert put_payloads[0]["version"]["number"] == 5  # bumped from 4
    storage = put_payloads[0]["body"]["storage"]["value"]
    assert "ac:structured-macro" not in storage  # still clean prose
    await fake.stop()


@pytest.mark.asyncio
async def test_doc_dry_run_makes_no_post_or_put(tmp_path: Path) -> None:
    doc = _write_doc(tmp_path, "LEAD", "page.md", _DOC_MD)
    posts_or_puts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal posts_or_puts
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        posts_or_puts += 1
        return httpx.Response(200, json={"id": "1"})

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, doc, update=False, dry_run=True, skill_space="TS")
    assert posts_or_puts == 0
    await fake.stop()


@pytest.mark.asyncio
async def test_doc_with_no_title_is_skipped(tmp_path: Path) -> None:
    """A doc with neither a leading H1 nor a frontmatter title is skipped,
    not published."""
    doc = _write_doc(tmp_path, "LEAD", "bad.md", "Just prose, no heading.\n")
    posts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal posts
        if request.method == "POST":
            posts += 1
        return httpx.Response(200, json={"results": []})

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, doc, update=False, dry_run=False, skill_space="TS")
    assert posts == 0
    await fake.stop()


# ---------------------------------------------------------------------------
# Routing + per-space pre-flight
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_run_import_routes_skill_and_doc_to_right_spaces(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A skill .md (trigger frontmatter) under tool-skills/ + a doc .md
    (H1, no frontmatter) under LEAD/ → the skill goes to TS with the YAML
    code macro, the doc goes to LEAD as clean prose."""
    (tmp_path / "tool-skills").mkdir()
    (tmp_path / "tool-skills" / "x.md").write_text(_SKILL_MD)
    _write_doc(tmp_path, "LEAD", "y.md", _DOC_MD)

    creates: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and "/content" in request.url.path:
            creates.append(json.loads(request.content))
            return httpx.Response(200, json={"id": str(len(creates))})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))

    async def _fake_build(config_path: Path) -> Any:
        return fake

    monkeypatch.setattr(pages, "build_transport", _fake_build)

    rc = await run_import(
        config_path=tmp_path / "cfg.yaml",
        paths=[tmp_path],
        update=False,
        dry_run=False,
        skill_space="TS",
    )
    assert rc == 0
    assert len(creates) == 2
    by_space = {c["space"]["key"]: c for c in creates}
    # Skill → TS with the YAML code macro.
    assert "TS" in by_space
    skill_storage = by_space["TS"]["body"]["storage"]["value"]
    assert 'ac:structured-macro ac:name="code"' in skill_storage
    # Doc → LEAD with clean prose (no macro), title from the H1.
    assert "LEAD" in by_space
    doc_storage = by_space["LEAD"]["body"]["storage"]["value"]
    assert "ac:structured-macro" not in doc_storage
    assert by_space["LEAD"]["title"] == "Manager 1:1"
    await fake.stop()


@pytest.mark.asyncio
async def test_run_import_preflights_each_distinct_space(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Pre-flight checks every distinct target space — the skill space
    (TS) plus the doc's parent-directory space (LEAD). A frontmatter-less
    doc must still contribute its space to the pre-flight set."""
    (tmp_path / "tool-skills").mkdir()
    (tmp_path / "tool-skills" / "x.md").write_text(_SKILL_MD)
    _write_doc(tmp_path, "LEAD", "y.md", _DOC_MD)
    checked_spaces: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if request.method == "GET" and "/rest/api/space/" in path:
            checked_spaces.append(path.rsplit("/", 1)[-1])
            return httpx.Response(200, json={"key": "x"})
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and path.endswith("/label"):
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            return httpx.Response(200, json={"id": "1"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))

    async def _fake_build(config_path: Path) -> Any:
        return fake

    monkeypatch.setattr(pages, "build_transport", _fake_build)

    rc = await run_import(
        config_path=tmp_path / "cfg.yaml",
        paths=[tmp_path],
        update=False,
        dry_run=False,
        skill_space="TS",
    )
    assert rc == 0
    assert set(checked_spaces) == {"TS", "LEAD"}
    await fake.stop()


@pytest.mark.asyncio
async def test_run_import_collects_md_recursively(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A nested directory tree is walked recursively and deduped; each
    doc's space is its own parent directory."""
    _write_doc(tmp_path, "LEAD", "top.md", _DOC_MD)
    _write_doc(tmp_path, "ENG", "inner.md", _DOC_MD.replace("Manager 1:1", "Inner"))
    (tmp_path / "LEAD" / "ignore.txt").write_text("not markdown")

    creates: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            creates.append(json.loads(request.content))
            return httpx.Response(200, json={"id": str(len(creates))})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))

    async def _fake_build(config_path: Path) -> Any:
        return fake

    monkeypatch.setattr(pages, "build_transport", _fake_build)

    rc = await run_import(
        config_path=tmp_path / "cfg.yaml",
        paths=[tmp_path],
        update=False,
        dry_run=False,
        skill_space="TS",
    )
    assert rc == 0
    assert len(creates) == 2  # top.md + inner.md, ignore.txt skipped
    assert {c["space"]["key"] for c in creates} == {"LEAD", "ENG"}
    await fake.stop()


@pytest.mark.asyncio
async def test_run_import_doc_space_missing_raises(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A missing doc space (no --create-space) surfaces a friendly error
    naming the space, before any page POST."""
    _write_doc(tmp_path, "LEAD", "a-doc.md", _DOC_MD)
    posts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal posts
        if request.method == "GET" and "/rest/api/space/" in request.url.path:
            return httpx.Response(404, json={"message": "no space"})
        if request.method == "POST":
            posts += 1
        return httpx.Response(200, json={"results": []})

    fake = _FakeTransport(httpx.MockTransport(handler))

    async def _fake_build(config_path: Path) -> Any:
        return fake

    monkeypatch.setattr(pages, "build_transport", _fake_build)

    from crewlet.confluence.pages import ConfluencePublishError

    with pytest.raises(ConfluencePublishError, match="LEAD"):
        await run_import(
            config_path=tmp_path / "cfg.yaml",
            paths=[tmp_path],
            update=False,
            dry_run=False,
            skill_space="TS",
        )
    assert posts == 0
    # import_cli is imported for the module-level monkeypatch target check
    assert hasattr(import_cli, "run_import")
    await fake.stop()
