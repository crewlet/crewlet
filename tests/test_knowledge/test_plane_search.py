"""Tests for query-time Plane page search (``PlaneSearcher``).

Covers the ``KnowledgeSearcher`` protocol surface (``can_search``
truth table, org-derived scope, ``KnowledgeHit`` mapping), per-agent vs
engine client resolution, the fail-closed identifier→UUID rules
(zero-resolved never widens, partial searches the subset), the two
result exclusions (skills project wholesale; auto-drafts by parent id
with the title-prefix backstop on lookup failure), and the best-effort
failure paths.
"""

from __future__ import annotations

from typing import Any

from crewlet.knowledge.plane_search import PlaneSearcher, authenticates_as_self
from crewlet.knowledge.protocol import (
    AUTO_DRAFT_TITLE_PREFIX,
    AUTO_DRAFTED_PARENT,
    KnowledgeHit,
)
from crewlet.org.models import Organization, Role
from crewlet.plane.client import PlaneProvisionError

ENG_UUID = "facefeed-0000-0000-0000-000000000001"
PROD_UUID = "facefeed-0000-0000-0000-000000000002"
TS_UUID = "facefeed-0000-0000-0000-00000000000e"
DRAFT_PARENT_ID = "9a9a9a9a-0000-0000-0000-0000000000dd"

# ---------------------------------------------------------------------------
# Fakes (transport client seam + Plane client)
# ---------------------------------------------------------------------------


class _FakeClient:
    """Stand-in for ``PlaneClient`` (search + page list + close)."""

    def __init__(
        self,
        *,
        rows: list[dict[str, Any]] | None = None,
        rows_sequence: list[list[dict[str, Any]]] | None = None,
        pages_by_project: dict[str, list[dict[str, Any]]] | None = None,
        raise_search: bool = False,
        raise_list: bool = False,
    ) -> None:
        self.search_calls: list[dict[str, Any]] = []
        self.list_calls: list[dict[str, Any]] = []
        self.closed = False
        self._rows = rows if rows is not None else []
        # Per-call result sets for the AND-relaxation ladder tests: call
        # N returns rows_sequence[N] (empty once exhausted). ``rows``
        # is ignored when a sequence is given.
        self._rows_sequence = rows_sequence
        self._pages = pages_by_project or {}
        self._raise_search = raise_search
        self._raise_list = raise_list

    async def search_pages(
        self,
        query: str,
        *,
        projects: list[str] | None = None,
        limit: int = 100,
    ) -> list[dict[str, Any]]:
        self.search_calls.append({"query": query, "projects": projects, "limit": limit})
        if self._raise_search:
            raise PlaneProvisionError("search down")
        if self._rows_sequence is not None:
            call_index = len(self.search_calls) - 1
            if call_index < len(self._rows_sequence):
                return self._rows_sequence[call_index]
            return []
        return self._rows

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
        if self._raise_list:
            raise PlaneProvisionError("incomplete walk")
        return self._pages.get(project_id, [])

    async def close(self) -> None:
        self.closed = True


class _FakeTransport:
    """Minimal stand-in for the ``PlaneTransport`` public client seam."""

    def __init__(
        self,
        *,
        engine_client: _FakeClient | None = None,
        user_client: _FakeClient | None = None,
        ids: dict[str, str] | None = None,
        base_url: str = "https://plane.test",
        workspace: str = "testco",
    ) -> None:
        self._engine_client = engine_client
        self._user_client = user_client
        self._ids = {k.strip().upper(): v for k, v in (ids or {}).items()}
        self._names = {v.lower(): k for k, v in self._ids.items()}
        self.base_url = base_url
        self.workspace = workspace
        self.new_user_tokens: list[str] = []
        self.resolve_calls: list[tuple[tuple[str, ...], Any]] = []

    @property
    def engine_client(self) -> _FakeClient | None:
        return self._engine_client

    def new_user_client(self, token: str) -> _FakeClient:
        self.new_user_tokens.append(token)
        assert self._user_client is not None, "no per-agent client configured"
        return self._user_client

    async def resolve_project_ids(
        self, identifiers: Any, *, client: Any = None
    ) -> dict[str, str]:
        self.resolve_calls.append((tuple(identifiers), client))
        wanted = {i.strip().upper() for i in identifiers if i and i.strip()}
        return {i: self._ids[i] for i in sorted(wanted) if i in self._ids}

    def cached_project_identifier(self, project_id: str) -> str:
        return self._names.get((project_id or "").lower(), "")


def _row(
    *,
    page_id: str,
    name: str,
    project_id: str = ENG_UUID,
    parent_id: str | None = None,
    snippet: str = "",
    updated_at: str = "2026-07-26T10:00:00Z",
) -> dict[str, Any]:
    """One pinned page-search row: {id, name, project_id, parent_id,
    updated_at, snippet} — ordered by ``-updated_at`` server-side."""
    return {
        "id": page_id,
        "name": name,
        "project_id": project_id,
        "parent_id": parent_id,
        "updated_at": updated_at,
        "snippet": snippet,
    }


def _org(projects: list[str] | None = None) -> Organization:
    return Organization(name="Acme", plane_projects=list(projects or []))


def _self_auth_role(key: str = "agent-key") -> Role:
    return Role(name="Engineer", mcp_env={"plane": {"PLANE_API_KEY": key}})


def _setup(
    *,
    rows: list[dict[str, Any]] | None = None,
    rows_sequence: list[list[dict[str, Any]]] | None = None,
    ids: dict[str, str] | None = None,
    skills_project: str = "",
    pages_by_project: dict[str, list[dict[str, Any]]] | None = None,
    engine: bool = True,
    user: bool = False,
    raise_search: bool = False,
    raise_list: bool = False,
) -> tuple[PlaneSearcher, _FakeTransport, _FakeClient]:
    """One shared client wired as engine and/or per-agent."""
    client = _FakeClient(
        rows=rows,
        rows_sequence=rows_sequence,
        pages_by_project=pages_by_project,
        raise_search=raise_search,
        raise_list=raise_list,
    )
    transport = _FakeTransport(
        engine_client=client if engine else None,
        user_client=client if user else None,
        ids=ids if ids is not None else {"ENG": ENG_UUID},
    )
    searcher = PlaneSearcher(transport=transport, skills_project=skills_project)
    return searcher, transport, client


# ---------------------------------------------------------------------------
# authenticates_as_self
# ---------------------------------------------------------------------------


def test_authenticates_as_self_with_key():
    assert authenticates_as_self(_self_auth_role()) is True


def test_authenticates_as_self_false_without_key():
    assert authenticates_as_self(None) is False
    assert authenticates_as_self(Role(name="E")) is False
    assert authenticates_as_self(Role(name="E", mcp_env={"plane": {}})) is False
    blank = Role(name="E", mcp_env={"plane": {"PLANE_API_KEY": "   "}})
    assert authenticates_as_self(blank) is False


def test_authenticates_as_self_unresolvable_env_ref_is_false(monkeypatch):
    """A ``${VAR}`` reference that resolves to nothing is no credential."""
    monkeypatch.delenv("PLANE_KEY_NOT_SET_ANYWHERE", raising=False)
    role = Role(
        name="E",
        mcp_env={"plane": {"PLANE_API_KEY": "${PLANE_KEY_NOT_SET_ANYWHERE}"}},
    )
    assert authenticates_as_self(role) is False


def test_authenticates_as_self_resolves_env_refs(monkeypatch):
    monkeypatch.setenv("PLANE_AGENT_KEY", "resolved-secret")
    role = Role(name="E", mcp_env={"plane": {"PLANE_API_KEY": "${PLANE_AGENT_KEY}"}})
    assert authenticates_as_self(role) is True


# ---------------------------------------------------------------------------
# can_search — the cheap pre-gate
# ---------------------------------------------------------------------------


def test_can_search_true_with_configured_scope():
    searcher, _, _ = _setup()
    assert searcher.can_search(role=Role(name="Eng"), org=_org(["ENG"])) is True
    assert searcher.can_search(role=None, org=_org(["ENG"])) is True
    assert searcher.can_search(role=_self_auth_role(), org=_org(["ENG"])) is True


def test_can_search_true_for_self_auth_role_without_scope():
    searcher, _, _ = _setup()
    assert searcher.can_search(role=_self_auth_role(), org=_org()) is True


def test_can_search_false_for_credential_less_role_without_scope():
    searcher, _, _ = _setup()
    assert searcher.can_search(role=Role(name="Eng"), org=_org()) is False
    assert searcher.can_search(role=None, org=_org()) is False


def test_can_search_missing_org_treated_as_empty_scope():
    searcher, _, _ = _setup()
    assert searcher.can_search(role=Role(name="Eng"), org=None) is False
    assert searcher.can_search(role=_self_auth_role(), org=None) is True


class _BrokenOrg:
    """Org stand-in whose scope list is unreadable."""

    @property
    def plane_projects(self) -> list[str]:
        raise RuntimeError("boom")


async def test_scope_resolution_failure_fails_closed():
    """An unreadable org scope must not fall through to an unscoped
    search: ``can_search`` gates off and ``search`` returns [] without
    issuing a request — even for a self-authenticating role."""
    searcher, transport, client = _setup(rows=[_row(page_id="1", name="X")], user=True)
    broken = _BrokenOrg()
    assert searcher.can_search(role=_self_auth_role(), org=broken) is False  # type: ignore[arg-type]
    assert await searcher.search(query="q", role=_self_auth_role(), org=broken) == []  # type: ignore[arg-type]
    assert transport.new_user_tokens == []
    assert client.search_calls == []


# ---------------------------------------------------------------------------
# Client resolution (per-agent vs engine)
# ---------------------------------------------------------------------------


async def test_per_agent_key_builds_owned_client_and_closes_it():
    engine = _FakeClient()
    agent = _FakeClient(rows=[_row(page_id="1", name="Doc")])
    transport = _FakeTransport(
        engine_client=engine, user_client=agent, ids={"ENG": ENG_UUID}
    )
    searcher = PlaneSearcher(transport=transport)
    hits = await searcher.search(
        query="q", role=_self_auth_role("agent-key"), org=_org(["ENG"])
    )
    assert [h.page_id for h in hits] == ["1"]
    assert transport.new_user_tokens == ["agent-key"]
    assert agent.search_calls  # ran as the agent
    assert engine.search_calls == []
    assert agent.closed is True  # per-agent client is owned + closed
    assert engine.closed is False


async def test_env_var_refs_resolved_for_per_agent_key(monkeypatch):
    monkeypatch.setenv("PLANE_AGENT_KEY", "resolved-secret")
    searcher, transport, _ = _setup(rows=[], user=True, engine=False)
    role = Role(name="E", mcp_env={"plane": {"PLANE_API_KEY": "${PLANE_AGENT_KEY}"}})
    await searcher.search(query="q", role=role, org=_org(["ENG"]))
    assert transport.new_user_tokens == ["resolved-secret"]


async def test_credential_less_role_uses_engine_client_not_closed():
    searcher, transport, client = _setup(rows=[_row(page_id="1", name="Doc")])
    hits = await searcher.search(query="q", role=Role(name="Eng"), org=_org(["ENG"]))
    assert len(hits) == 1
    assert transport.new_user_tokens == []
    assert client.search_calls  # engine client was used
    assert client.closed is False  # shared client must never be closed


async def test_no_client_at_all_returns_empty():
    """A credential-less role on a token-less engine searches nothing —
    the Confluence admin-client-missing path."""
    transport = _FakeTransport(engine_client=None, ids={"ENG": ENG_UUID})
    searcher = PlaneSearcher(transport=transport)
    assert await searcher.search(query="q", role=None, org=_org(["ENG"])) == []


# ---------------------------------------------------------------------------
# Scope: unscoped-vs-nothing + identifier→UUID resolution
# ---------------------------------------------------------------------------


async def test_scoped_search_sends_resolved_uuids_sorted():
    searcher, transport, client = _setup(
        rows=[], ids={"ENG": ENG_UUID, "PROD": PROD_UUID}
    )
    await searcher.search(query="q", role=None, org=_org(["prod", "eng"]))
    # Scope is normalised + sorted before resolution; the request
    # carries the resolved UUIDs in that order.
    assert transport.resolve_calls[0][0] == ("ENG", "PROD")
    assert client.search_calls[0]["projects"] == [ENG_UUID, PROD_UUID]


async def test_search_passes_active_client_as_resolution_fallback():
    agent = _FakeClient(rows=[])
    transport = _FakeTransport(user_client=agent, ids={"ENG": ENG_UUID})
    searcher = PlaneSearcher(transport=transport)
    await searcher.search(query="q", role=_self_auth_role(), org=_org(["ENG"]))
    # The per-agent client rides along so a token-less engine can still
    # resolve — the transport decides whether to use it.
    assert transport.resolve_calls[0][1] is agent


async def test_unscoped_for_self_auth_role_without_projects():
    searcher, transport, client = _setup(
        rows=[_row(page_id="1", name="Doc")], user=True, engine=False
    )
    hits = await searcher.search(query="deploy", role=_self_auth_role(), org=_org())
    assert len(hits) == 1
    # No scope → no resolution call, no ``projects=`` param.
    assert transport.resolve_calls == []
    assert client.search_calls[0]["projects"] is None


async def test_empty_scope_credential_less_returns_empty():
    searcher, _, client = _setup(rows=[_row(page_id="1", name="Doc")])
    assert await searcher.search(query="q", role=Role(name="Eng"), org=_org()) == []
    assert client.search_calls == []


async def test_zero_resolved_from_non_empty_scope_never_widens(caplog):
    """A configured scope resolving to NOTHING searches nothing — never
    silently widens to everything the credential can see."""
    searcher, _, client = _setup(
        rows=[_row(page_id="1", name="Doc")], ids={}, user=True
    )
    hits = await searcher.search(query="q", role=_self_auth_role(), org=_org(["GONE"]))
    assert hits == []
    assert client.search_calls == []
    assert "plane_search_projects_unresolved" in caplog.text


async def test_partial_resolution_searches_the_subset():
    searcher, _, client = _setup(rows=[], ids={"ENG": ENG_UUID})
    await searcher.search(query="q", role=None, org=_org(["ENG", "GONE"]))
    assert client.search_calls[0]["projects"] == [ENG_UUID]


# ---------------------------------------------------------------------------
# Query handling
# ---------------------------------------------------------------------------


async def test_query_sent_verbatim_with_overfetch():
    """The first attempt carries the raw multi-keyword aux query — the
    server tokenises (AND-across-tokens); relaxation only
    kicks in when that attempt returns nothing."""
    searcher, _, client = _setup(rows=[])
    await searcher.search(
        query="latency spike rollback incident",
        role=None,
        org=_org(["ENG"]),
        limit=6,
    )
    call = client.search_calls[0]
    assert call["query"] == "latency spike rollback incident"
    assert call["limit"] == 11  # limit + overfetch (post-filters still fill)


# ---------------------------------------------------------------------------
# AND-relaxation ladder (server-side strict conjunction)
# ---------------------------------------------------------------------------


async def test_relaxation_walks_prefixes_until_hits():
    """A many-keyword conjunction that matches nothing relaxes to the
    4-token then 2-token leading prefix — e.g. a 7-keyword query can
    yield zero over a corpus its prefix hits."""
    hit = _row(page_id="p1", name="Phase two kickoff")
    searcher, _, client = _setup(rows_sequence=[[], [], [hit]])
    out = await searcher.search(
        query="draft ticket phase two nimbus implementation template",
        role=None,
        org=_org(["ENG"]),
    )
    queries = [c["query"] for c in client.search_calls]
    assert queries == [
        "draft ticket phase two nimbus implementation template",
        "draft ticket phase two",
        "draft ticket",
    ]
    assert [h.page_id for h in out] == ["p1"]


async def test_relaxation_stops_at_first_non_empty_attempt():
    hit = _row(page_id="p1", name="Rollback runbook")
    searcher, _, client = _setup(rows_sequence=[[], [hit]])
    out = await searcher.search(
        query="latency spike rollback incident response",
        role=None,
        org=_org(["ENG"]),
    )
    assert [c["query"] for c in client.search_calls] == [
        "latency spike rollback incident response",
        "latency spike rollback incident",
    ]
    assert [h.page_id for h in out] == ["p1"]


async def test_no_relaxation_when_first_attempt_hits():
    hit = _row(page_id="p1", name="Incident response")
    searcher, _, client = _setup(rows_sequence=[[hit]])
    out = await searcher.search(
        query="latency spike rollback incident response",
        role=None,
        org=_org(["ENG"]),
    )
    assert len(client.search_calls) == 1
    assert [h.page_id for h in out] == ["p1"]


async def test_relaxation_dry_run_returns_empty():
    searcher, _, client = _setup(rows_sequence=[[], [], []])
    out = await searcher.search(
        query="six tokens of pure conjunction noise",
        role=None,
        org=_org(["ENG"]),
    )
    assert len(client.search_calls) == 3
    assert out == []


async def test_relaxation_skips_prefixes_no_shorter_than_the_query():
    """A 3-token query has no 4-token prefix to fall to — the ladder is
    full then the 2-token prefix, two calls total."""
    searcher, _, client = _setup(rows_sequence=[[], []])
    await searcher.search(
        query="alpha beta gamma",
        role=None,
        org=_org(["ENG"]),
    )
    assert [c["query"] for c in client.search_calls] == [
        "alpha beta gamma",
        "alpha beta",
    ]


async def test_query_pretrimmed_to_distinct_token_cap():
    """The search endpoint 400s (never truncates) above 16 distinct
    case-folded tokens;
    the searcher trims to the cap so the first attempt is a real search
    instead of a guaranteed error → empty block."""
    query = " ".join(f"tok{i}" for i in range(20))
    searcher, _, client = _setup(rows_sequence=[[_row(page_id="p1", name="n")]])
    await searcher.search(query=query, role=None, org=_org(["ENG"]))
    sent = client.search_calls[0]["query"].split()
    assert len(sent) == 16
    assert sent == [f"tok{i}" for i in range(16)]


async def test_query_truncated_to_200_chars():
    searcher, _, client = _setup(rows=[])
    await searcher.search(query="z" * 500, role=None, org=_org(["ENG"]))
    assert client.search_calls[0]["query"] == "z" * 200


async def test_empty_query_searches_nothing():
    searcher, _, client = _setup(rows=[])
    assert await searcher.search(query="   ", role=None, org=_org(["ENG"])) == []
    assert client.search_calls == []


# ---------------------------------------------------------------------------
# Hit mapping
# ---------------------------------------------------------------------------


async def test_maps_rows_to_knowledge_hits():
    searcher, _, _ = _setup(
        rows=[
            _row(
                page_id="p1",
                name="Deploy Runbook",
                snippet="…how to deploy the payments service…",
            )
        ]
    )
    hits = await searcher.search(query="deploy", role=None, org=_org(["ENG"]))
    assert len(hits) == 1
    hit = hits[0]
    assert isinstance(hit, KnowledgeHit)
    assert hit.title == "Deploy Runbook"
    assert hit.page_id == "p1"
    assert hit.container == "ENG"  # reverse-resolved from the row's UUID
    assert hit.snippet == "…how to deploy the payments service…"
    # The exact URL shape the transport builds for webhook links.
    assert hit.url == f"https://plane.test/testco/projects/{ENG_UUID}/pages/p1"
    assert hit.ancestors == []  # Plane rows carry no ancestor chain


async def test_unknown_project_degrades_container_but_keeps_url():
    unknown = "facefeed-0000-0000-0000-00000000ffff"
    searcher, _, _ = _setup(
        rows=[_row(page_id="p1", name="Doc", project_id=unknown)],
        user=True,
        engine=False,
    )
    hits = await searcher.search(query="q", role=_self_auth_role(), org=_org())
    assert hits[0].container == ""  # cache miss → empty label, no lookup
    assert hits[0].url == f"https://plane.test/testco/projects/{unknown}/pages/p1"


async def test_preserves_server_recency_order_and_truncates_tail():
    """Page search orders by ``-updated_at`` (recency, NOT relevance).  The
    searcher trusts that order and truncation drops the OLDEST rows of
    the overfetch window."""
    rows = [
        _row(page_id=f"p{i}", name=f"Doc {i}", updated_at=f"2026-07-{26 - i:02d}")
        for i in range(5)
    ]
    searcher, _, _ = _setup(rows=rows)
    hits = await searcher.search(query="q", role=None, org=_org(["ENG"]), limit=3)
    assert [h.page_id for h in hits] == ["p0", "p1", "p2"]


async def test_rows_without_id_skipped():
    searcher, _, _ = _setup(
        rows=[{"name": "no id"}, "junk", _row(page_id="p1", name="D")]
    )
    hits = await searcher.search(query="q", role=None, org=_org(["ENG"]))
    assert [h.page_id for h in hits] == ["p1"]


# ---------------------------------------------------------------------------
# Skills-project exclusion
# ---------------------------------------------------------------------------


async def test_skills_project_rows_dropped_wholesale():
    searcher, _, _ = _setup(
        rows=[
            _row(page_id="p1", name="Real Doc"),
            _row(page_id="p2", name="deploy-service", project_id=TS_UUID),
        ],
        ids={"ENG": ENG_UUID, "TS": TS_UUID},
        skills_project="ts",  # case-insensitive identifier
    )
    hits = await searcher.search(query="q", role=None, org=_org(["ENG"]))
    assert [h.page_id for h in hits] == ["p1"]


async def test_empty_skills_project_disables_the_exclusion():
    searcher, transport, _ = _setup(
        rows=[_row(page_id="p2", name="deploy-service", project_id=TS_UUID)],
        ids={"ENG": ENG_UUID, "TS": TS_UUID},
        skills_project="",
    )
    hits = await searcher.search(query="q", role=None, org=_org(["ENG"]))
    assert [h.page_id for h in hits] == ["p2"]
    # Only the scope was resolved — no skills-project lookup happened.
    assert transport.resolve_calls == [(("ENG",), transport.engine_client)]


async def test_scope_and_skills_project_resolved_in_one_call():
    """The scope and the skills project share ONE resolve_project_ids
    call per search — on a token-less engine each resolve costs a full
    per-agent list_projects walk, so two would double the hot path."""
    searcher, transport, _ = _setup(
        rows=[_row(page_id="p1", name="Doc")],
        ids={"ENG": ENG_UUID, "TS": TS_UUID},
        skills_project="TS",
    )
    await searcher.search(query="q", role=None, org=_org(["ENG"]))
    assert len(transport.resolve_calls) == 1
    assert transport.resolve_calls[0][0] == ("ENG", "TS")


async def test_token_less_engine_pays_one_fallback_walk_per_search(monkeypatch):
    """Real-transport call count: with no engine token the per-agent
    fallback fetch runs ONCE per search() call and serves both the
    scope resolution and the skills-project lookup (it is never written
    to the shared cache, so it stays one per CALL, not one ever)."""
    from crewlet.config import PlaneConfig
    from crewlet.notifications.transports.plane import PlaneTransport

    class _CountingClient(_FakeClient):
        def __init__(self, **kw: Any) -> None:
            super().__init__(**kw)
            self.project_walks = 0

        async def list_projects(self) -> list[dict[str, Any]]:
            self.project_walks += 1
            return [
                {"id": ENG_UUID, "identifier": "ENG"},
                {"id": TS_UUID, "identifier": "TS"},
            ]

    transport = PlaneTransport(
        PlaneConfig(
            enabled=True,
            url="https://plane.test",
            workspace="testco",
            webhook_secret="whsec",
            token="",  # token-less engine: engine_client is None
        )
    )
    client = _CountingClient(rows=[_row(page_id="p1", name="Doc")])
    monkeypatch.setattr(transport, "new_user_client", lambda token: client)
    searcher = PlaneSearcher(transport=transport, skills_project="TS")

    hits = await searcher.search(query="q", role=_self_auth_role(), org=_org(["ENG"]))
    assert [h.page_id for h in hits] == ["p1"]
    assert client.project_walks == 1  # ONE walk served scope + skills lookup
    await searcher.search(query="q", role=_self_auth_role(), org=_org(["ENG"]))
    assert client.project_walks == 2  # per-call, never cached shared
    assert transport._project_names == {}  # fallback wrote nothing shared


async def test_row_without_project_id_dropped_fail_closed():
    """A hit whose ``project_id`` is absent can be checked against
    neither exclusion (and has an unbuildable URL) — dropped, exactly
    like a row without an ``id``.  A project-less
    ``[Auto-draft] `` row would otherwise skip both filters and leak."""
    searcher, _, _ = _setup(
        rows=[
            {"id": "leak", "name": f"{AUTO_DRAFT_TITLE_PREFIX}Unvetted Skill"},
            {"id": "leak2", "name": "no project ref", "project_id": None},
            _row(page_id="p1", name="Kept Doc"),
        ],
        skills_project="ts",
        ids={"ENG": ENG_UUID, "TS": TS_UUID},
    )
    hits = await searcher.search(
        query="q",
        role=None,
        org=_org(["ENG"]),
        exclude_ancestors=[AUTO_DRAFTED_PARENT],
    )
    assert [h.page_id for h in hits] == ["p1"]


async def test_unresolvable_skills_project_fails_closed(caplog):
    """When the CONFIGURED skills project cannot be resolved to a UUID,
    the exclusion must not silently become a no-op (Tool Skills YAML in
    the Plan prompt): only rows positively attributable to a DIFFERENT
    project survive; unattributable rows drop, with a warning."""
    searcher, _, _ = _setup(
        rows=[
            _row(page_id="p1", name="Kept Doc"),  # ENG — cached, ≠ TS
            _row(page_id="p2", name="deploy-service", project_id=TS_UUID),
        ],
        ids={"ENG": ENG_UUID},  # TS deliberately unresolvable
        skills_project="TS",
    )
    hits = await searcher.search(query="q", role=None, org=_org(["ENG"]))
    assert [h.page_id for h in hits] == ["p1"]
    assert "plane_search_skills_project_unresolved" in caplog.text


# ---------------------------------------------------------------------------
# Auto-draft exclusion (parent id + title-prefix backstop)
# ---------------------------------------------------------------------------

_DRAFT_PARENT_PAGES = [
    {"id": DRAFT_PARENT_ID, "name": AUTO_DRAFTED_PARENT},
    {"id": "other-page", "name": "Auto-Drafted Skills Archive"},  # icontains noise
]


async def test_drafts_hidden_by_parent_id():
    searcher, _, client = _setup(
        rows=[
            _row(page_id="p1", name="Published Runbook"),
            _row(
                page_id="p2",
                name=f"{AUTO_DRAFT_TITLE_PREFIX}New Skill",
                parent_id=DRAFT_PARENT_ID,
            ),
        ],
        pages_by_project={ENG_UUID: _DRAFT_PARENT_PAGES},
    )
    hits = await searcher.search(
        query="q",
        role=None,
        org=_org(["ENG"]),
        exclude_ancestors=[AUTO_DRAFTED_PARENT],
    )
    assert [h.page_id for h in hits] == ["p1"]
    # The lookup used the exact-name post-filter over ``search=``
    # (icontains) rows.
    assert client.list_calls[0]["project_id"] == ENG_UUID
    assert client.list_calls[0]["search"] == AUTO_DRAFTED_PARENT


async def test_published_draft_moved_out_is_visible_even_unrenamed():
    """The publish gesture is "move the page out of the parent" —
    renaming is optional.  When the parent lookup WORKS, the title
    backstop must not hide a published-but-unrenamed page."""
    searcher, _, _ = _setup(
        rows=[
            _row(
                page_id="p1",
                name=f"{AUTO_DRAFT_TITLE_PREFIX}Deploy Skill",
                parent_id=None,  # moved out of the draft parent
            )
        ],
        pages_by_project={ENG_UUID: _DRAFT_PARENT_PAGES},
    )
    hits = await searcher.search(
        query="q",
        role=None,
        org=_org(["ENG"]),
        exclude_ancestors=[AUTO_DRAFTED_PARENT],
    )
    assert [h.page_id for h in hits] == ["p1"]


async def test_title_backstop_fires_only_on_lookup_failure():
    """A parent-lookup outage hides drafts (fail closed) via the title
    prefix — and only then."""
    searcher, _, _ = _setup(
        rows=[
            _row(page_id="p1", name="Published Runbook"),
            _row(page_id="p2", name=f"{AUTO_DRAFT_TITLE_PREFIX}New Skill"),
        ],
        raise_list=True,
    )
    hits = await searcher.search(
        query="q",
        role=None,
        org=_org(["ENG"]),
        exclude_ancestors=[AUTO_DRAFTED_PARENT],
    )
    assert [h.page_id for h in hits] == ["p1"]


async def test_no_parent_page_in_project_excludes_nothing():
    searcher, _, _ = _setup(
        rows=[_row(page_id="p1", name=f"{AUTO_DRAFT_TITLE_PREFIX}Kept")],
        pages_by_project={ENG_UUID: []},  # successful lookup, no parent page
    )
    hits = await searcher.search(
        query="q",
        role=None,
        org=_org(["ENG"]),
        exclude_ancestors=[AUTO_DRAFTED_PARENT],
    )
    assert [h.page_id for h in hits] == ["p1"]


async def test_no_exclude_ancestors_skips_the_parent_lookup():
    searcher, _, client = _setup(
        rows=[_row(page_id="p1", name="Doc", parent_id=DRAFT_PARENT_ID)],
        pages_by_project={ENG_UUID: _DRAFT_PARENT_PAGES},
    )
    hits = await searcher.search(
        query="q", role=None, org=_org(["ENG"]), exclude_ancestors=[]
    )
    assert [h.page_id for h in hits] == ["p1"]
    assert client.list_calls == []


async def test_found_parent_id_cached_across_searches():
    searcher, _, client = _setup(
        rows=[_row(page_id="p1", name="Doc")],
        pages_by_project={ENG_UUID: _DRAFT_PARENT_PAGES},
    )
    for _ in range(2):
        await searcher.search(
            query="q",
            role=None,
            org=_org(["ENG"]),
            exclude_ancestors=[AUTO_DRAFTED_PARENT],
        )
    assert len(client.list_calls) == 1  # lifetime cache for the found id


async def test_failed_lookup_cached_behind_the_miss_floor():
    searcher, _, client = _setup(
        rows=[_row(page_id="p2", name=f"{AUTO_DRAFT_TITLE_PREFIX}New Skill")],
        raise_list=True,
    )
    for _ in range(2):
        hits = await searcher.search(
            query="q",
            role=None,
            org=_org(["ENG"]),
            exclude_ancestors=[AUTO_DRAFTED_PARENT],
        )
        assert hits == []  # backstop keeps applying between retries
    assert len(client.list_calls) == 1  # 60 s floor blocked the retry


async def test_parent_lookup_prefers_engine_client():
    """Per-agent membership must not decide what gets hidden: the
    parent lookup runs on the ENGINE client even when the search itself
    runs per-agent."""
    engine = _FakeClient(pages_by_project={ENG_UUID: _DRAFT_PARENT_PAGES})
    agent = _FakeClient(
        rows=[_row(page_id="p2", name="Draft", parent_id=DRAFT_PARENT_ID)]
    )
    transport = _FakeTransport(
        engine_client=engine, user_client=agent, ids={"ENG": ENG_UUID}
    )
    searcher = PlaneSearcher(transport=transport)
    hits = await searcher.search(
        query="q",
        role=_self_auth_role(),
        org=_org(["ENG"]),
        exclude_ancestors=[AUTO_DRAFTED_PARENT],
    )
    assert hits == []
    assert agent.search_calls and not agent.list_calls
    assert engine.list_calls and not engine.search_calls


# ---------------------------------------------------------------------------
# Best-effort failure paths
# ---------------------------------------------------------------------------


async def test_search_request_failure_returns_empty():
    searcher, _, client = _setup(raise_search=True, user=True, engine=False)
    hits = await searcher.search(query="q", role=_self_auth_role(), org=_org(["ENG"]))
    assert hits == []
    assert client.closed is True  # owned client closed on the error path


async def test_limit_honored():
    rows = [_row(page_id=f"p{i}", name=f"Doc {i}") for i in range(8)]
    searcher, _, _ = _setup(rows=rows)
    hits = await searcher.search(query="q", role=None, org=_org(["ENG"]), limit=3)
    assert len(hits) == 3
