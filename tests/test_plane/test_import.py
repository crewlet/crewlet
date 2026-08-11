"""Tests for ``crewlet plane import`` / ``resync``.

Drive the unified Plane importer against a fake ``PlaneClient`` that
records every write.  Idempotency is the fork's external-identity
contract: pages are matched by ``external_id`` (``skill:<key>`` /
``doc:<title>``) with ``external_source="crewlet"``; prune is gated on
that positive marker; deletion is archive-then-delete with per-page
failure isolation.
"""

from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from crewlet.agent.skills.registry import PromptSkillRegistry
from crewlet.plane import import_cli
from crewlet.plane.client import PlaneProvisionError
from crewlet.plane.import_cli import (
    EXTERNAL_SOURCE,
    PlanePublishError,
    build_client_from_config,
    doc_external_id,
    run_import,
    skill_external_id,
    sync_skills_registry,
)
from crewlet.plane.plane_codec import encode_page

_TS_UUID = "facefeed-0000-0000-0000-0000000000ts"
_ENG_UUID = "facefeed-0000-0000-0000-000000000e19"

_PROJECTS = [
    {"id": _TS_UUID, "identifier": "TS", "name": "Tool Skills"},
    {"id": _ENG_UUID, "identifier": "ENG", "name": "Engineering"},
]

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

_DOC_MD = """# Manager 1:1

## What a 1:1 is

A short private session.

- prepare
- converge
"""


class FakePlaneClient:
    """Recording stand-in for the Plane page surface the importer consumes."""

    def __init__(
        self,
        *,
        projects: list[dict[str, Any]] | None = None,
        pages: dict[str, list[dict[str, Any]]] | None = None,
    ) -> None:
        self.projects = projects if projects is not None else list(_PROJECTS)
        self.pages = pages or {}
        self.created: list[tuple[str, dict[str, Any]]] = []
        self.updated: list[tuple[str, str, dict[str, Any]]] = []
        self.archived: list[tuple[str, str]] = []
        self.unarchived: list[tuple[str, str]] = []
        self.deleted: list[tuple[str, str]] = []
        self.list_pages_calls: list[tuple[str, dict[str, Any]]] = []
        self.closed = False
        # Failure injection knobs.
        self.create_conflict_names: set[str] = set()
        self.create_errors: dict[str, PlaneProvisionError] = {}  # by page name
        self.update_errors: dict[str, PlaneProvisionError] = {}  # by page id
        self.delete_errors: dict[str, PlaneProvisionError] = {}
        self.unarchive_errors: dict[str, PlaneProvisionError] = {}
        self.list_pages_error: Exception | None = None

    async def close(self) -> None:
        self.closed = True

    async def list_projects(self) -> list[dict[str, Any]]:
        return list(self.projects)

    async def list_pages(
        self,
        project_id: str,
        *,
        search: str | None = None,
        fields: list[str] | None = None,
        page_type: str | None = None,
    ) -> list[dict[str, Any]]:
        self.list_pages_calls.append(
            (project_id, {"search": search, "fields": fields, "page_type": page_type})
        )
        if self.list_pages_error is not None:
            raise self.list_pages_error
        return [dict(p) for p in self.pages.get(project_id, [])]

    async def create_page(
        self,
        project_id: str,
        *,
        name: str,
        description_html: str,
        parent_id: str | None = None,
        external_id: str | None = None,
        external_source: str | None = None,
        access: int = 0,
    ) -> dict[str, Any]:
        if name in self.create_conflict_names:
            raise PlaneProvisionError(
                "Page with the same external id and external source already exists",
                method="POST",
                path="/pages/",
                status=409,
            )
        if name in self.create_errors:
            raise self.create_errors[name]
        page = {
            "id": f"new-{len(self.created) + 1}",
            "name": name,
            "description_html": description_html,
            "parent": parent_id,
            "external_id": external_id,
            "external_source": external_source,
            "access": access,
        }
        self.created.append((project_id, dict(page)))
        return page

    async def update_page(
        self, project_id: str, page_id: str, **fields: Any
    ) -> dict[str, Any]:
        err = self.update_errors.get(page_id)
        if err is not None:
            raise err
        self.updated.append((project_id, page_id, dict(fields)))
        return {"id": page_id, **fields}

    async def archive_page(self, project_id: str, page_id: str) -> dict[str, Any]:
        self.archived.append((project_id, page_id))
        return {"archived_at": "2026-07-27"}

    async def unarchive_page(self, project_id: str, page_id: str) -> None:
        err = self.unarchive_errors.get(page_id)
        if err is not None:
            raise err
        self.unarchived.append((project_id, page_id))

    async def delete_page(self, project_id: str, page_id: str) -> None:
        err = self.delete_errors.get(page_id)
        if err is not None:
            raise err
        self.deleted.append((project_id, page_id))


def _crewlet_page(
    page_id: str, name: str, external_id: str, *, source: str = EXTERNAL_SOURCE
) -> dict[str, Any]:
    return {
        "id": page_id,
        "name": name,
        "external_id": external_id,
        "external_source": source,
    }


def _install(monkeypatch: pytest.MonkeyPatch, fake: FakePlaneClient) -> None:
    async def _fake_build(config_path: Path) -> Any:
        return fake

    monkeypatch.setattr(import_cli, "build_client_from_config", _fake_build)


def _write(tmp_path: Path, rel: str, text: str) -> Path:
    path = tmp_path / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    return path


async def _run(tmp_path: Path, **kw: Any) -> int:
    kw.setdefault("skills_project", "TS")
    return await run_import(
        config_path=tmp_path / "company.yaml", paths=[tmp_path], **kw
    )


# ---------------------------------------------------------------------------
# Routing + wire format
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_routes_skill_and_doc_to_right_projects(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Skill (trigger frontmatter) → skills project with the YAML code
    block + skill: external id; doc → parent-directory project as clean
    prose with the doc: external id."""
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    _write(tmp_path, "ENG/y.md", _DOC_MD)
    fake = FakePlaneClient()
    _install(monkeypatch, fake)

    rc = await _run(tmp_path)
    assert rc == 0
    assert fake.closed
    by_project = {proj: page for proj, page in fake.created}
    skill_page = by_project[_TS_UUID]
    assert skill_page["external_id"] == skill_external_id("tool:reflect_and_persist")
    assert skill_page["external_source"] == EXTERNAL_SOURCE
    assert skill_page["name"] == "Persisting durable facts"
    assert skill_page["description_html"].startswith(
        '<pre><code class="language-yaml">'
    )
    doc_page = by_project[_ENG_UUID]
    assert doc_page["external_id"] == doc_external_id("Manager 1:1")
    assert doc_page["name"] == "Manager 1:1"
    assert "<pre>" not in doc_page["description_html"]
    assert "<h2>" in doc_page["description_html"]


@pytest.mark.asyncio
async def test_one_narrow_enumeration_per_project(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """The importer does ONE list_pages walk per target project with the
    narrow fields= projection — never a per-file search."""
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    _write(tmp_path, "ENG/a.md", _DOC_MD)
    _write(tmp_path, "ENG/b.md", _DOC_MD.replace("Manager 1:1", "Other Doc"))
    fake = FakePlaneClient()
    _install(monkeypatch, fake)

    await _run(tmp_path)
    assert len(fake.list_pages_calls) == 2  # TS + ENG, once each
    for _project, params in fake.list_pages_calls:
        assert params["fields"] == ["id", "name", "external_id", "external_source"]
        assert params["search"] is None


@pytest.mark.asyncio
async def test_required_default_stamped_into_page_yaml(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    assert "required" not in _SKILL_MD.split("---")[1]
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    fake = FakePlaneClient()
    _install(monkeypatch, fake)

    await _run(tmp_path)
    _, page = fake.created[0]
    assert "required: true" in page["description_html"]


@pytest.mark.asyncio
async def test_skill_without_key_is_skipped(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(
        tmp_path,
        "tool-skills/bad.md",
        "---\ntrigger:\n  tool: x\n---\n\nBody.\n",
    )
    fake = FakePlaneClient()
    _install(monkeypatch, fake)

    rc = await _run(tmp_path)
    assert rc == 0
    assert fake.created == []


@pytest.mark.asyncio
async def test_doc_with_labels_frontmatter_is_published_without_them(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Plane pages carry no free-form labels — the frontmatter is
    accepted (logged once) and the doc still publishes."""
    _write(
        tmp_path,
        "ENG/l.md",
        "---\nlabels: [playbook, leadership]\n---\n\n# Labelled Doc\n\nBody.\n",
    )
    fake = FakePlaneClient()
    _install(monkeypatch, fake)

    rc = await _run(tmp_path)
    assert rc == 0
    assert len(fake.created) == 1


# ---------------------------------------------------------------------------
# external_id idempotency
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_skill_matched_by_external_id_skipped_without_update(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    fake = FakePlaneClient(
        pages={
            _TS_UUID: [
                _crewlet_page(
                    "p1",
                    "Persisting durable facts",
                    skill_external_id("tool:reflect_and_persist"),
                )
            ]
        }
    )
    _install(monkeypatch, fake)

    await _run(tmp_path)
    assert fake.created == []
    assert fake.updated == []


@pytest.mark.asyncio
async def test_skill_update_with_flag_rewrites_and_restamps(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    fake = FakePlaneClient(
        pages={
            _TS_UUID: [
                _crewlet_page(
                    "p1",
                    "Persisting durable facts",
                    skill_external_id("tool:reflect_and_persist"),
                )
            ]
        }
    )
    _install(monkeypatch, fake)

    await _run(tmp_path, update=True)
    assert fake.created == []
    assert len(fake.updated) == 1
    project, page_id, fields = fake.updated[0]
    assert (project, page_id) == (_TS_UUID, "p1")
    assert fields["name"] == "Persisting durable facts"
    assert fields["external_id"] == skill_external_id("tool:reflect_and_persist")
    assert fields["external_source"] == EXTERNAL_SOURCE
    assert fields["description_html"].startswith('<pre><code class="language-yaml">')


@pytest.mark.asyncio
async def test_skill_retitled_in_plane_still_matched_by_external_id(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Retitling a page in Plane's UI never orphans it — the external
    identity, not the title, is the match key (the update renames it
    back to the authored title)."""
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    fake = FakePlaneClient(
        pages={
            _TS_UUID: [
                _crewlet_page(
                    "p1",
                    "Operator Renamed This",
                    skill_external_id("tool:reflect_and_persist"),
                )
            ]
        }
    )
    _install(monkeypatch, fake)

    await _run(tmp_path, update=True)
    assert fake.created == []
    _project, page_id, fields = fake.updated[0]
    assert page_id == "p1"
    assert fields["name"] == "Persisting durable facts"


@pytest.mark.asyncio
async def test_title_fallback_adopts_legacy_page_and_stamps_marker(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A page with the exact title but NO crewlet marker (published by an
    older path or created manually) is adopted instead of duplicated;
    --update stamps the external identity onto it."""
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    fake = FakePlaneClient(
        pages={
            _TS_UUID: [
                {"id": "legacy", "name": "Persisting durable facts"},
            ]
        }
    )
    _install(monkeypatch, fake)

    await _run(tmp_path, update=True)
    assert fake.created == []
    _project, page_id, fields = fake.updated[0]
    assert page_id == "legacy"
    assert fields["external_id"] == skill_external_id("tool:reflect_and_persist")
    assert fields["external_source"] == EXTERNAL_SOURCE


@pytest.mark.asyncio
async def test_title_fallback_never_adopts_foreign_crewlet_page(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A crewlet page with a DIFFERENT external id sharing the title is a
    different record — the importer creates a fresh page rather than
    hijacking it."""
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    fake = FakePlaneClient(
        pages={
            _TS_UUID: [
                _crewlet_page(
                    "other",
                    "Persisting durable facts",
                    skill_external_id("tool:other_skill"),
                )
            ]
        }
    )
    _install(monkeypatch, fake)

    await _run(tmp_path, update=True)
    assert fake.updated == []
    assert len(fake.created) == 1


@pytest.mark.asyncio
async def test_doc_matched_by_external_id_skipped_without_update(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(tmp_path, "ENG/y.md", _DOC_MD)
    fake = FakePlaneClient(
        pages={
            _ENG_UUID: [
                _crewlet_page("d1", "Manager 1:1", doc_external_id("Manager 1:1"))
            ]
        }
    )
    _install(monkeypatch, fake)

    await _run(tmp_path)
    assert fake.created == []
    assert fake.updated == []


@pytest.mark.asyncio
async def test_create_conflict_409_logs_and_continues(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A 409 external-identity conflict (e.g. an ARCHIVED page invisible
    to the enumeration still owns the pair) skips that file and lets the
    rest of the run publish."""
    _write(tmp_path, "ENG/a.md", _DOC_MD)
    _write(tmp_path, "ENG/b.md", _DOC_MD.replace("Manager 1:1", "Second Doc"))
    fake = FakePlaneClient()
    fake.create_conflict_names = {"Manager 1:1"}
    _install(monkeypatch, fake)

    rc = await _run(tmp_path)
    assert rc == 0
    assert [page["name"] for _p, page in fake.created] == ["Second Doc"]


# ---------------------------------------------------------------------------
# Frontmatter ``parent`` resolution
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_doc_parent_resolved_from_enumeration(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(
        tmp_path,
        "ENG/n.md",
        "---\nparent: Team Handbook\n---\n\n# Nested Doc\n\nBody.\n",
    )
    fake = FakePlaneClient(pages={_ENG_UUID: [{"id": "hb-1", "name": "Team Handbook"}]})
    _install(monkeypatch, fake)

    await _run(tmp_path)
    _project, page = fake.created[0]
    assert page["parent"] == "hb-1"


@pytest.mark.asyncio
async def test_doc_parent_missing_creates_at_root(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(
        tmp_path,
        "ENG/n.md",
        "---\nparent: No Such Page\n---\n\n# Orphan Doc\n\nBody.\n",
    )
    fake = FakePlaneClient()
    _install(monkeypatch, fake)

    await _run(tmp_path)
    _project, page = fake.created[0]
    assert page["parent"] is None


@pytest.mark.asyncio
async def test_doc_parent_published_in_same_run_nests_child(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A frontmatter ``parent`` naming a page published EARLIER IN THE
    SAME RUN resolves: every successful create is added to the project
    index, so the child's POST carries the just-created parent's id
    (files publish in ``collect_md_files``' sorted order)."""
    _write(tmp_path, "ENG/handbook.md", "# Engineering Handbook\n\nBody.\n")
    _write(
        tmp_path,
        "ENG/onboarding.md",
        "---\nparent: Engineering Handbook\n---\n\n# Onboarding\n\nBody.\n",
    )
    fake = FakePlaneClient()
    _install(monkeypatch, fake)

    rc = await _run(tmp_path)
    assert rc == 0
    by_name = {page["name"]: page for _p, page in fake.created}
    parent_page = by_name["Engineering Handbook"]
    child_page = by_name["Onboarding"]
    assert parent_page["parent"] is None
    assert child_page["parent"] == parent_page["id"]


# ---------------------------------------------------------------------------
# Per-page write-failure isolation
# ---------------------------------------------------------------------------


def _locked_error() -> PlaneProvisionError:
    """The fork's locked-page rejection (pr9470 PATCH/POST): 400 with
    ``{"error": "Page is locked"}``."""
    return PlaneProvisionError(
        '{"error": "Page is locked"}', method="POST", path="/pages/", status=400
    )


@pytest.mark.asyncio
async def test_locked_page_isolated_run_continues_and_exits_nonzero(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """One locked page (a normal Plane UI gesture on a reviewed skill
    page) must not abort the run: the remaining files still publish —
    including files sorted AFTER the failed one — and the run then
    exits non-zero naming the failed file."""
    _write(tmp_path, "ENG/a.md", _DOC_MD)  # a.md sorts first and fails
    _write(tmp_path, "ENG/b.md", _DOC_MD.replace("Manager 1:1", "Second Doc"))
    fake = FakePlaneClient()
    fake.create_errors["Manager 1:1"] = _locked_error()
    _install(monkeypatch, fake)

    with pytest.raises(PlanePublishError, match=r"a\.md"):
        await _run(tmp_path)
    assert [page["name"] for _p, page in fake.created] == ["Second Doc"]


@pytest.mark.asyncio
async def test_locked_page_on_update_isolated(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    _write(tmp_path, "ENG/b.md", _DOC_MD)
    fake = FakePlaneClient(
        pages={
            _TS_UUID: [
                _crewlet_page(
                    "p-locked",
                    "Persisting durable facts",
                    skill_external_id("tool:reflect_and_persist"),
                )
            ]
        }
    )
    fake.update_errors["p-locked"] = _locked_error()
    _install(monkeypatch, fake)

    with pytest.raises(PlanePublishError, match=r"x\.md"):
        await _run(tmp_path, update=True)
    # The doc after the locked skill still published.
    assert [page["name"] for _p, page in fake.created] == ["Manager 1:1"]


@pytest.mark.asyncio
async def test_credential_401_still_aborts_run(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """An invalid credential fails every request identically — no point
    churning through the remaining files page by page."""
    _write(tmp_path, "ENG/a.md", _DOC_MD)
    _write(tmp_path, "ENG/b.md", _DOC_MD.replace("Manager 1:1", "Second Doc"))
    fake = FakePlaneClient()
    fake.create_errors["Manager 1:1"] = PlaneProvisionError(
        "authentication failed", method="POST", path="/pages/", status=401
    )
    _install(monkeypatch, fake)

    with pytest.raises(PlanePublishError, match="check the Plane token"):
        await _run(tmp_path)
    # Aborted at the first write — the second file never published.
    assert fake.created == []


# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_missing_project_fails_before_writes(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(tmp_path, "NOPE/y.md", _DOC_MD)
    fake = FakePlaneClient()
    _install(monkeypatch, fake)

    with pytest.raises(PlanePublishError, match="NOPE"):
        await _run(tmp_path)
    assert fake.created == []
    assert fake.list_pages_calls == []


@pytest.mark.asyncio
async def test_project_identifier_matched_case_insensitively(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(tmp_path, "eng/y.md", _DOC_MD)
    fake = FakePlaneClient()
    _install(monkeypatch, fake)

    rc = await _run(tmp_path)
    assert rc == 0
    assert fake.created[0][0] == _ENG_UUID


@pytest.mark.asyncio
async def test_dry_run_makes_no_writes(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    _write(tmp_path, "ENG/y.md", _DOC_MD)
    fake = FakePlaneClient(
        pages={
            _TS_UUID: [_crewlet_page("orphan", "Old", skill_external_id("skill:gone"))]
        }
    )
    _install(monkeypatch, fake)

    rc = await _run(tmp_path, update=True, dry_run=True, prune=True)
    assert rc == 0
    assert fake.created == []
    assert fake.updated == []
    assert fake.archived == []
    assert fake.deleted == []


# ---------------------------------------------------------------------------
# Prune
# ---------------------------------------------------------------------------


def _prune_fixture_pages() -> list[dict[str, Any]]:
    return [
        # Orphan: crewlet skill whose key is no longer local.
        _crewlet_page("orphan", "Copilot", skill_external_id("skill:copilot")),
        # Local: crewlet skill still published by a local file.
        _crewlet_page(
            "keep-local",
            "Persisting durable facts",
            skill_external_id("tool:reflect_and_persist"),
        ),
        # Unmarked user-authored page: never touched.
        {"id": "keep-human", "name": "Team notes"},
        # crewlet doc-keyed page (not a skill): never pruned.
        _crewlet_page("keep-doc", "Stray Doc", doc_external_id("Stray Doc")),
        # Foreign integration's page: not our marker.
        _crewlet_page(
            "keep-foreign", "Import", skill_external_id("skill:x"), source="jira"
        ),
    ]


@pytest.mark.asyncio
async def test_prune_archives_then_deletes_only_orphan_marker_pages(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    fake = FakePlaneClient(pages={_TS_UUID: _prune_fixture_pages()})
    _install(monkeypatch, fake)

    rc = await _run(tmp_path, update=True, prune=True)
    assert rc == 0
    assert fake.archived == [(_TS_UUID, "orphan")]
    assert fake.deleted == [(_TS_UUID, "orphan")]


@pytest.mark.asyncio
async def test_prune_dry_run_deletes_nothing(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    fake = FakePlaneClient(pages={_TS_UUID: _prune_fixture_pages()})
    _install(monkeypatch, fake)

    await _run(tmp_path, prune=True, dry_run=True)
    assert fake.archived == []
    assert fake.deleted == []


@pytest.mark.asyncio
async def test_prune_isolates_per_page_failures_and_rolls_back_archive(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A 403 on one orphan's DELETE (human-owned page — deletion is
    owner-or-admin only) logs and continues; the other orphan is still
    deleted and the run succeeds.  Crucially the failed page's archive
    is ROLLED BACK: left archived it would be invisible to users and
    to every later enumeration while its external_id 409s any future
    republish — a permanent-conflict trap."""
    fake = FakePlaneClient(
        pages={
            _TS_UUID: [
                _crewlet_page("a-orphan", "A", skill_external_id("skill:a")),
                _crewlet_page("b-orphan", "B", skill_external_id("skill:b")),
            ]
        }
    )
    fake.delete_errors["a-orphan"] = PlaneProvisionError(
        "forbidden", method="DELETE", path="/pages/a-orphan/", status=403
    )
    _install(monkeypatch, fake)

    rc = await _run(tmp_path, prune=True)
    assert rc == 0
    assert (_TS_UUID, "b-orphan") in fake.deleted
    assert (_TS_UUID, "a-orphan") not in fake.deleted
    # Both were archived (archive precedes delete)…
    assert (_TS_UUID, "a-orphan") in fake.archived
    # …but the failed one was unarchived again — a true no-op prune.
    assert fake.unarchived == [(_TS_UUID, "a-orphan")]


@pytest.mark.asyncio
async def test_prune_unarchive_failure_logs_left_archived(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    """When even the unarchive rollback fails, the half-applied state
    (page left ARCHIVED, external_id permanently 409ing) is called out
    explicitly — never silent."""
    fake = FakePlaneClient(
        pages={_TS_UUID: [_crewlet_page("a-orphan", "A", skill_external_id("skill:a"))]}
    )
    fake.delete_errors["a-orphan"] = PlaneProvisionError(
        "forbidden", method="DELETE", path="/pages/a-orphan/", status=403
    )
    fake.unarchive_errors["a-orphan"] = PlaneProvisionError(
        "forbidden", method="DELETE", path="/pages/a-orphan/archive/", status=403
    )
    _install(monkeypatch, fake)

    rc = await _run(tmp_path, prune=True)
    assert rc == 0
    assert fake.unarchived == []
    assert "plane_prune_delete_failed" in caplog.text
    assert "left ARCHIVED" in caplog.text


@pytest.mark.asyncio
async def test_prune_enumeration_failure_deletes_nothing(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """An incomplete enumeration must delete nothing — the strict
    list_pages walk raising aborts the whole run before prune."""
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    fake = FakePlaneClient()
    fake.list_pages_error = PlaneProvisionError(
        "pagination cursor stalled", method="GET", path="/pages/", status=0
    )
    _install(monkeypatch, fake)

    with pytest.raises(PlanePublishError, match="Plane API error"):
        await _run(tmp_path, prune=True)
    assert fake.archived == []
    assert fake.deleted == []


@pytest.mark.asyncio
async def test_skills_without_configured_project_fails_clearly(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _write(tmp_path, "tool-skills/x.md", _SKILL_MD)
    fake = FakePlaneClient()
    _install(monkeypatch, fake)

    with pytest.raises(PlanePublishError, match="CREWLET_TOOL_SKILLS_PROJECT"):
        await _run(tmp_path, skills_project="")


# ---------------------------------------------------------------------------
# build_client_from_config
# ---------------------------------------------------------------------------

_PLANE_COMPANY_YAML = (
    "name: T\n"
    "integrations:\n"
    "  plane:\n"
    "    enabled: true\n"
    '    url: "https://plane.test"\n'
    "    workspace: testco\n"
    '    webhook_secret: "${PLANE_WEBHOOK_SECRET}"\n'
    '    token: "${PLANE_ENGINE_TOKEN}"\n'
)


@pytest.mark.asyncio
async def test_build_client_resolves_env_and_returns_client(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setenv("PLANE_ENGINE_TOKEN", "secret-token")
    company = tmp_path / "company.yaml"
    company.write_text(_PLANE_COMPANY_YAML)

    client = await build_client_from_config(company)
    try:
        assert client._base_url == "https://plane.test"
        assert client._workspace == "testco"
        assert client._token == "secret-token"
    finally:
        await client.close()


@pytest.mark.asyncio
async def test_build_client_errors_when_token_unset(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """An unset ${VAR} token fails fast naming the variable — not an
    opaque 401 on every request."""
    monkeypatch.delenv("PLANE_ENGINE_TOKEN", raising=False)
    company = tmp_path / "company.yaml"
    company.write_text(_PLANE_COMPANY_YAML)
    with pytest.raises(PlanePublishError, match="PLANE_ENGINE_TOKEN"):
        await build_client_from_config(company)


@pytest.mark.asyncio
async def test_build_client_errors_without_enabled_plane_block(
    tmp_path: Path,
) -> None:
    company = tmp_path / "company.yaml"
    company.write_text("name: T\n")
    with pytest.raises(PlanePublishError, match="integrations.plane"):
        await build_client_from_config(company)

    disabled = tmp_path / "disabled.yaml"
    disabled.write_text("name: T\nintegrations:\n  plane:\n    enabled: false\n")
    with pytest.raises(PlanePublishError, match="enabled"):
        await build_client_from_config(disabled)


# ---------------------------------------------------------------------------
# Command handlers
# ---------------------------------------------------------------------------


def test_cmd_plane_import_errors_when_config_missing(tmp_path: Path) -> None:
    args = SimpleNamespace(
        config=tmp_path / "nope.yaml",
        path=tmp_path,
        project=None,
        update=False,
        dry_run=False,
        prune=False,
    )
    assert import_cli.cmd_plane_import(args) == 1


def test_cmd_plane_import_errors_when_path_missing(tmp_path: Path) -> None:
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("name: x\n")
    args = SimpleNamespace(
        config=cfg,
        path=tmp_path / "no-such-dir",
        project=None,
        update=False,
        dry_run=False,
        prune=False,
    )
    assert import_cli.cmd_plane_import(args) == 1


def test_cmd_plane_import_reports_publish_error(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("name: x\n")
    docs = tmp_path / "docs"
    docs.mkdir()
    (docs / "a.md").write_text("# A\n\nbody\n")

    async def _boom(**_kw: Any) -> int:
        raise PlanePublishError("no project")

    monkeypatch.setattr(import_cli, "run_import", _boom)
    args = SimpleNamespace(
        config=cfg, path=docs, project=None, update=False, dry_run=False, prune=False
    )
    assert import_cli.cmd_plane_import(args) == 1


# ---------------------------------------------------------------------------
# Resync
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_sync_skills_registry_seeds_only_decodable_trigger_pages() -> None:
    """The boot-walk logic: pages that decode AND carry a trigger seed
    the registry; the project home page (no code block) and a decodable
    page without a trigger are skipped silently."""
    skill_html = encode_page(
        {
            "key": "mcp:plane",
            "trigger": {"mcp_server": "plane"},
            "phases": ["plan"],
            "title": "Plane tools",
            "summary": "How to Plane.",
        },
        "Body.",
    )
    no_trigger_html = encode_page({"key": "x", "note": "not a skill"}, "Body.")
    fake = FakePlaneClient(
        pages={
            _TS_UUID: [
                {"id": "s1", "name": "Plane tools", "description_html": skill_html},
                {
                    "id": "home",
                    "name": "Tool Skills",
                    "description_html": "<p>Home</p>",
                },
                {"id": "n1", "name": "Note", "description_html": no_trigger_html},
            ]
        }
    )
    registry = PromptSkillRegistry()
    count = await sync_skills_registry(fake, registry, "TS")  # type: ignore[arg-type]
    assert count == 1
    assert registry.keys() == ["mcp:plane"]
    # The enumeration carried bodies — one walk, no per-page fetch.
    assert len(fake.list_pages_calls) == 1
    assert "description_html" in (fake.list_pages_calls[0][1]["fields"] or [])


@pytest.mark.asyncio
async def test_sync_skills_registry_missing_project_loads_nothing() -> None:
    fake = FakePlaneClient(projects=[])
    registry = PromptSkillRegistry()
    count = await sync_skills_registry(fake, registry, "TS")  # type: ignore[arg-type]
    assert count == 0
    assert registry.keys() == []
    assert fake.list_pages_calls == []


def test_cmd_plane_resync_reports_loaded_keys(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("name: x\n")
    skill_html = encode_page(
        {
            "key": "mcp:plane",
            "trigger": {"mcp_server": "plane"},
            "phases": ["plan"],
            "title": "Plane tools",
            "summary": "s",
        },
        "Body.",
    )
    fake = FakePlaneClient(
        pages={
            _TS_UUID: [
                {"id": "s1", "name": "Plane tools", "description_html": skill_html}
            ]
        }
    )
    _install(monkeypatch, fake)
    args = SimpleNamespace(config=cfg, project="TS")
    assert import_cli.cmd_plane_resync(args) == 0
    assert fake.closed
