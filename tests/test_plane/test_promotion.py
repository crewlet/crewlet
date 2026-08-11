"""Tests for ``crewlet.plane.promotion.PlanePromotionWriter``."""

from __future__ import annotations

from typing import Any

from crewlet.knowledge.markdown_docs import render_markdown
from crewlet.learning.skill_synthesizer import PromotionPageWriter
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.plane.client import PAGE_ACCESS_PUBLIC, PlaneProvisionError
from crewlet.plane.promotion import PlanePromotionWriter, draft_external_id

ENG_UUID = "aa000000-0000-4000-8000-0000000000e9"
_BODY_MD = "**Team routine.**\n\n1. Ship\n2. Verify"


class _FakePlaneClient:
    """Recording Plane pages fake whose ``list_pages`` honours the ``search``
    name-``icontains`` semantics — the writer issues one lookup for the
    draft title (dedup) and one for the parent title, and the fake must
    answer each with only the matching rows, like the fork does."""

    def __init__(
        self,
        *,
        pages: list[dict[str, Any]] | None = None,
        list_error: Exception | None = None,
        create_error: Exception | None = None,
        create_parent_error: Exception | None = None,
        conflict_names: set[str] | None = None,
    ) -> None:
        self.pages = pages or []
        self.list_error = list_error
        self.create_error = create_error
        self.create_parent_error = create_parent_error
        self.conflict_names = conflict_names or set()
        self.list_calls: list[dict[str, Any]] = []
        self.creates: list[dict[str, Any]] = []
        self._next_id = 0

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
        needle = (search or "").lower()
        return [dict(p) for p in self.pages if needle in str(p.get("name", "")).lower()]

    async def create_page(
        self,
        project_id: str,
        *,
        name: str,
        description_html: str,
        parent_id: str | None = None,
        external_id: str | None = None,
        external_source: str | None = None,
        access: int = PAGE_ACCESS_PUBLIC,
    ) -> dict[str, Any]:
        is_parent = name == "Auto-Drafted Skills"
        if is_parent and self.create_parent_error is not None:
            raise self.create_parent_error
        if not is_parent and self.create_error is not None:
            raise self.create_error
        if name in self.conflict_names:
            raise PlaneProvisionError(
                "Page with the same external id and external source already exists",
                method="POST",
                path="/pages/",
                status=409,
            )
        self._next_id += 1
        page_id = f"NEW-{self._next_id}"
        page = {
            "id": page_id,
            "project_id": project_id,
            "name": name,
            "description_html": description_html,
            "parent_id": parent_id,
            "external_id": external_id,
            "external_source": external_source,
            "access": access,
        }
        self.creates.append(dict(page))
        # Created pages become visible to later lookups — the fork's
        # enumeration would return them, and the two-tick dedup test
        # depends on it.
        self.pages.append(page)
        return {"id": page_id, "name": name}


class _FakeTransport:
    def __init__(
        self,
        client: _FakePlaneClient | None,
        mapping: dict[str, str] | None = None,
    ) -> None:
        self._client = client
        self._mapping = {"ENG": ENG_UUID} if mapping is None else mapping

    @property
    def engine_client(self) -> _FakePlaneClient | None:
        return self._client

    async def resolve_project_ids(
        self, identifiers: Any, *, client: Any = None
    ) -> dict[str, str]:
        wanted = {str(i).strip().upper() for i in identifiers if i}
        return {k: v for k, v in self._mapping.items() if k in wanted}


def _mk_org(plane_project: str = "ENG") -> Organization:
    unit = OrgUnit(
        name="Eng",
        type="team",
        lead="Alice",
        roles=[Role(name="Alice", handle="alice")],
        plane_project=plane_project,
    )
    return Organization(name="Acme", units=[unit])


def _writer(
    client: _FakePlaneClient | None,
    mapping: dict[str, str] | None = None,
) -> PlanePromotionWriter:
    return PlanePromotionWriter(
        transport=_FakeTransport(client, mapping)  # type: ignore[arg-type]
    )


async def _draft(
    writer: PlanePromotionWriter,
    *,
    container: str = "ENG",
    title: str = "[Auto-draft] ship-routine",
    name: str = "ship-routine",
    body: str = _BODY_MD,
) -> str | None:
    return await writer.create_draft_page(
        container=container,
        parent_title="Auto-Drafted Skills",
        title=title,
        name=name,
        body_markdown=body,
    )


def test_writer_satisfies_protocol_and_resolves_container() -> None:
    writer: PromotionPageWriter = _writer(_FakePlaneClient())
    assert writer.backend == "plane"
    assert writer.resolve_unit_container(_mk_org(), "Eng") == "ENG"
    assert writer.resolve_unit_container(_mk_org(plane_project=""), "Eng") == ""
    assert "integrations.plane.project" in writer.missing_container_hint()


def test_draft_external_id_shape() -> None:
    assert draft_external_id("ship-routine") == "draft:ship-routine"


async def test_create_draft_under_existing_parent() -> None:
    # The icontains lookup returns a near-miss alongside the exact
    # parent — only the exact name matches.
    client = _FakePlaneClient(
        pages=[
            {"id": "NEAR-1", "name": "Auto-Drafted Skills (old)"},
            {"id": "PARENT-1", "name": "Auto-Drafted Skills"},
        ]
    )
    writer = _writer(client)

    page_id = await _draft(writer, container="eng")  # case-insensitive identifier

    assert page_id == "NEW-1"
    # First lookup: the draft-title dedup; second: the parent.
    assert client.list_calls[0]["search"] == "[Auto-draft] ship-routine"
    assert client.list_calls[1]["search"] == "Auto-Drafted Skills"
    assert all(c["project_id"] == ENG_UUID for c in client.list_calls)
    assert len(client.creates) == 1
    draft = client.creates[0]
    assert draft["project_id"] == ENG_UUID
    assert draft["name"] == "[Auto-draft] ship-routine"
    assert draft["parent_id"] == "PARENT-1"
    assert draft["access"] == PAGE_ACCESS_PUBLIC
    assert draft["description_html"] == render_markdown(_BODY_MD)
    # The fork's idempotency identity is stamped — cross-tick dedup.
    assert draft["external_id"] == "draft:ship-routine"
    assert draft["external_source"] == "crewlet"


async def test_second_tick_returns_existing_draft_no_second_create() -> None:
    """The scheduler re-clusters the same rows every tick; the writer
    must not pile up one identical draft per tick.  Two ticks ⇒ ONE
    page — the second call finds the stamped draft and returns its id."""
    client = _FakePlaneClient(pages=[{"id": "PARENT-1", "name": "Auto-Drafted Skills"}])
    writer = _writer(client)

    first = await _draft(writer)
    second = await _draft(writer)

    assert first == "NEW-1"
    assert second == "NEW-1"  # the existing page, not a duplicate
    assert len(client.creates) == 1


async def test_dedup_matches_external_id_even_when_retitle_kept_title() -> None:
    """External identity wins over the title match: a page carrying
    this draft's ``draft:<name>`` id is THE draft, whatever its name
    row says."""
    client = _FakePlaneClient(
        pages=[
            {"id": "PARENT-1", "name": "Auto-Drafted Skills"},
            {
                "id": "OLD-DRAFT",
                "name": "[Auto-draft] ship-routine",
                "external_id": draft_external_id("ship-routine"),
                "external_source": "crewlet",
            },
        ]
    )
    writer = _writer(client)
    assert await _draft(writer) == "OLD-DRAFT"
    assert client.creates == []


async def test_dedup_title_fallback_adopts_prestamp_legacy_draft() -> None:
    """Drafts created before the identity stamp existed carry no
    external id — the exact-title fallback still finds them."""
    client = _FakePlaneClient(
        pages=[
            {"id": "PARENT-1", "name": "Auto-Drafted Skills"},
            {"id": "LEGACY", "name": "[Auto-draft] ship-routine"},
        ]
    )
    writer = _writer(client)
    assert await _draft(writer) == "LEGACY"
    assert client.creates == []


async def test_create_conflict_409_returns_none() -> None:
    """The fork's project-wide 409 on the duplicate external pair is
    the hard dedup backstop (a retitled draft the title lookup missed,
    or a race) — mapped to None so the tick no-ops exactly like
    Confluence's duplicate-title 400."""
    client = _FakePlaneClient(
        pages=[{"id": "PARENT-1", "name": "Auto-Drafted Skills"}],
        conflict_names={"[Auto-draft] ship-routine"},
    )
    writer = _writer(client)
    assert await _draft(writer) is None
    assert [c["name"] for c in client.creates] == []


async def test_create_draft_ensures_parent_when_absent() -> None:
    """The parent page is load-bearing (the search exclusion matches on
    parent id), so a missing parent is CREATED, then the draft nests
    under it.  The parent carries the importer's doc identity so its
    own lookup/create race is closed by the fork's 409."""
    client = _FakePlaneClient(pages=[])
    writer = _writer(client)

    page_id = await _draft(writer, title="[Auto-draft] x", name="x", body="body")

    assert len(client.creates) == 2
    parent, draft = client.creates
    assert parent["name"] == "Auto-Drafted Skills"
    assert parent["parent_id"] is None
    assert parent["access"] == PAGE_ACCESS_PUBLIC
    assert parent["external_id"] == "doc:Auto-Drafted Skills"
    assert parent["external_source"] == "crewlet"
    assert draft["parent_id"] == parent["id"]
    assert page_id == draft["id"]


async def test_parent_create_conflict_relooks_up() -> None:
    """A 409 on the parent create means the identity already exists
    (a concurrent tick / importer run) — the writer re-looks it up and
    nests under the found page instead of degrading to the root."""

    class _RacyClient(_FakePlaneClient):
        async def create_page(self, project_id: str, **kw: Any) -> dict[str, Any]:
            if kw.get("name") == "Auto-Drafted Skills":
                # Simulate a concurrent create winning the race: the
                # page now exists for the retry lookup, and our create
                # 409s.
                self.pages.append({"id": "RACED-1", "name": "Auto-Drafted Skills"})
                raise PlaneProvisionError(
                    "duplicate external id", method="POST", path="/pages/", status=409
                )
            return await super().create_page(project_id, **kw)

    client = _RacyClient(pages=[])
    writer = _writer(client)
    page_id = await _draft(writer, title="[Auto-draft] x", name="x", body="body")
    assert page_id == "NEW-1"
    assert client.creates[0]["parent_id"] == "RACED-1"


async def test_create_draft_parent_lookup_failure_lands_at_root() -> None:
    """A FAILED parent lookup never creates a parent (it could
    duplicate one) — the draft lands at the root.  Honest coverage:
    the searcher's [Auto-draft] title backstop hides it only while the
    searcher's OWN lookup is failing too; once the backend recovers a
    root-level draft is served until reviewed (the stamped external id
    still prevents duplicates meanwhile)."""
    client = _FakePlaneClient(
        list_error=PlaneProvisionError("boom", method="GET", path="/pages/")
    )
    writer = _writer(client)

    page_id = await _draft(writer, title="[Auto-draft] x", name="x", body="body")

    assert page_id == "NEW-1"
    assert len(client.creates) == 1
    assert client.creates[0]["parent_id"] is None
    assert client.creates[0]["external_id"] == "draft:x"


async def test_create_draft_parent_create_failure_lands_at_root() -> None:
    client = _FakePlaneClient(
        pages=[],
        create_parent_error=PlaneProvisionError(
            "denied", method="POST", path="/pages/", status=403
        ),
    )
    writer = _writer(client)

    page_id = await _draft(writer, title="[Auto-draft] x", name="x", body="body")

    assert page_id == "NEW-1"
    assert len(client.creates) == 1
    assert client.creates[0]["parent_id"] is None


async def test_create_draft_without_engine_client_returns_none() -> None:
    writer = _writer(None)
    assert await _draft(writer, title="t", name="t") is None


async def test_create_draft_unresolved_container_returns_none() -> None:
    client = _FakePlaneClient()
    writer = _writer(client, mapping={})
    assert await _draft(writer, container="NOPE", title="t", name="t") is None
    assert client.creates == []


async def test_create_draft_returns_none_on_create_failure() -> None:
    client = _FakePlaneClient(
        pages=[{"id": "PARENT-1", "name": "Auto-Drafted Skills"}],
        create_error=PlaneProvisionError(
            "denied", method="POST", path="/pages/", status=403
        ),
    )
    writer = _writer(client)
    assert await _draft(writer, title="t", name="t") is None
