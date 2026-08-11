"""Tests for query-time Confluence CQL search (``ConfluenceSearcher``).

Covers the CQL builder, the storage-format HTML strip, the
``KnowledgeSearcher`` protocol surface (``can_search`` truth table,
org-derived scope, ``KnowledgeHit`` mapping including the auto-draft
ancestor post-filter and ``limit``), the best-effort failure paths,
and per-agent vs admin auth selection.
"""

from __future__ import annotations

from typing import Any
from urllib.parse import unquote

import pytest

from crewlet.knowledge.confluence_search import (
    ConfluenceSearcher,
    _strip_storage_format,
    authenticates_as_self,
    build_cql,
)
from crewlet.knowledge.protocol import AUTO_DRAFTED_PARENT, KnowledgeHit
from crewlet.org.models import Organization, Role

# ---------------------------------------------------------------------------
# Transport / HTTP stubs
# ---------------------------------------------------------------------------


class _FakeResponse:
    def __init__(
        self, *, status_code: int = 200, payload: dict[str, Any] | None = None
    ) -> None:
        self.status_code = status_code
        self._payload = payload if payload is not None else {}
        self.text = ""

    def json(self) -> dict[str, Any]:
        return self._payload


class _FakeClient:
    def __init__(self, response: _FakeResponse | None, raise_on_get: bool) -> None:
        self._response = response
        self._raise = raise_on_get
        self.get_urls: list[str] = []
        self.closed = False

    async def get(self, url: str) -> _FakeResponse:
        self.get_urls.append(url)
        if self._raise:
            raise RuntimeError("network down")
        assert self._response is not None
        return self._response

    async def aclose(self) -> None:
        self.closed = True


class _FakeTransport:
    """Minimal stand-in for ``ConfluenceTransport``."""

    def __init__(
        self,
        *,
        response: _FakeResponse | None = None,
        raise_on_get: bool = False,
        has_admin: bool = True,
        rest_base: str = "https://wiki.example/wiki",
        shareable: str = "https://wiki.example/wiki",
    ) -> None:
        self._rest_base = rest_base
        self._shareable_base_url = shareable
        self._response = response
        self._raise = raise_on_get
        self._http_client = _FakeClient(response, raise_on_get) if has_admin else None
        self.new_user_calls: list[tuple[str, str]] = []
        self.user_clients: list[_FakeClient] = []

    def new_user_client(self, username: str, token: str) -> _FakeClient:
        self.new_user_calls.append((username, token))
        client = _FakeClient(self._response, self._raise)
        self.user_clients.append(client)
        return client


def _page(
    *,
    page_id: str,
    title: str,
    body: str = "<p>Body text.</p>",
    space_key: str = "ENG",
    ancestors: list[str] | None = None,
) -> dict[str, Any]:
    return {
        "id": page_id,
        "title": title,
        "space": {"key": space_key},
        "body": {"storage": {"value": body}},
        "ancestors": [{"title": a} for a in (ancestors or [])],
    }


def _response(pages: list[dict[str, Any]]) -> _FakeResponse:
    return _FakeResponse(payload={"results": pages})


def _org(spaces: list[str] | None = None) -> Organization:
    """An org whose only knowledge config is the read-scope list."""
    return Organization(name="Acme", confluence_spaces=list(spaces or []))


def _self_auth_role() -> Role:
    return Role(
        name="Engineer",
        mcp_env={
            "atlassian": {
                "CONFLUENCE_USERNAME": "e@a.com",
                "CONFLUENCE_API_TOKEN": "t",
            }
        },
    )


# ---------------------------------------------------------------------------
# build_cql
# ---------------------------------------------------------------------------


def test_build_cql_basic_shape():
    cql = build_cql("deploy hotfix", ["ENG", "GENERAL"])
    assert cql == (
        'space IN ("ENG","GENERAL") AND type = page AND text ~ "deploy hotfix"'
    )


def test_build_cql_uppercases_spaces():
    assert 'space IN ("ENG")' in build_cql("q", ["eng"])


def test_build_cql_empty_inputs_return_empty():
    assert build_cql("", ["ENG"]) == ""
    assert build_cql("q", []) == ""
    assert build_cql("   ", ["ENG"]) == ""


def test_build_cql_unscoped_when_allowed():
    # Empty spaces + allow_unscoped drops the space filter (ACLs scope).
    assert build_cql("deploy", [], allow_unscoped=True) == (
        'type = page AND text ~ "deploy"'
    )
    # Without the flag, empty spaces still short-circuit.
    assert build_cql("deploy", [], allow_unscoped=False) == ""
    # Empty terms never search, flag or not.
    assert build_cql("", [], allow_unscoped=True) == ""
    # Configured spaces win even when unscoped is allowed.
    assert "space IN" in build_cql("q", ["ENG"], allow_unscoped=True)


# ---------------------------------------------------------------------------
# authenticates_as_self
# ---------------------------------------------------------------------------


def test_authenticates_as_self_cloud_and_dc_creds():
    cloud = Role(
        name="E",
        mcp_env={
            "atlassian": {
                "CONFLUENCE_USERNAME": "e@a.com",
                "CONFLUENCE_API_TOKEN": "t",
            }
        },
    )
    dc = Role(name="E", mcp_env={"atlassian": {"CONFLUENCE_PERSONAL_TOKEN": "pat"}})
    assert authenticates_as_self(cloud) is True
    assert authenticates_as_self(dc) is True


def test_authenticates_as_self_false_without_full_creds():
    assert authenticates_as_self(None) is False
    assert authenticates_as_self(Role(name="E")) is False
    # Username without token is not enough.
    half = Role(name="E", mcp_env={"atlassian": {"CONFLUENCE_USERNAME": "e@a.com"}})
    assert authenticates_as_self(half) is False


def test_build_cql_escapes_quotes_and_backslashes():
    cql = build_cql('say "hi"\\done', ["ENG"])
    # Embedded quotes / backslashes are backslash-escaped so the CQL
    # string literal stays well-formed.
    assert r'text ~ "say \"hi\"\\done"' in cql


def test_build_cql_caps_term_length():
    # ``z`` does not appear in any CQL keyword, so counting it isolates
    # the (capped) term fragment.
    cql = build_cql("z" * 500, ["ENG"])
    assert cql.count("z") <= 200


# ---------------------------------------------------------------------------
# _strip_storage_format
# ---------------------------------------------------------------------------


def test_strip_storage_format_removes_tags():
    assert _strip_storage_format("<p>Hello <b>world</b></p>") == "Hello world"


def test_strip_storage_format_collapses_whitespace():
    assert _strip_storage_format("<p>a</p>\n\n   <p>b</p>") == "a b"


def test_strip_storage_format_empty():
    assert _strip_storage_format("") == ""


# ---------------------------------------------------------------------------
# ConfluenceSearcher.can_search -- the cheap pre-gate
# ---------------------------------------------------------------------------


def test_can_search_true_with_configured_scope():
    searcher = ConfluenceSearcher(transport=_FakeTransport())
    # Scope alone is enough, whatever the role's credentials.
    assert searcher.can_search(role=Role(name="Eng"), org=_org(["ENG"])) is True
    assert searcher.can_search(role=None, org=_org(["ENG"])) is True
    assert searcher.can_search(role=_self_auth_role(), org=_org(["ENG"])) is True


def test_can_search_true_for_self_auth_role_without_scope():
    searcher = ConfluenceSearcher(transport=_FakeTransport())
    assert searcher.can_search(role=_self_auth_role(), org=_org()) is True


def test_can_search_false_for_credential_less_role_without_scope():
    searcher = ConfluenceSearcher(transport=_FakeTransport())
    assert searcher.can_search(role=Role(name="Eng"), org=_org()) is False
    assert searcher.can_search(role=None, org=_org()) is False


def test_can_search_missing_org_treated_as_empty_scope():
    searcher = ConfluenceSearcher(transport=_FakeTransport())
    assert searcher.can_search(role=Role(name="Eng"), org=None) is False
    assert searcher.can_search(role=_self_auth_role(), org=None) is True


class _BrokenOrg:
    """Org stand-in whose scope list is unreadable."""

    @property
    def confluence_spaces(self) -> list[str]:
        raise RuntimeError("boom")


@pytest.mark.asyncio
async def test_scope_resolution_failure_fails_closed():
    """An unreadable org scope must not fall through to an unscoped /
    admin search: ``can_search`` gates off and ``search`` returns []
    without issuing a request -- even for a self-authenticating role."""
    transport = _FakeTransport(response=_response([_page(page_id="1", title="X")]))
    searcher = ConfluenceSearcher(transport=transport)
    broken = _BrokenOrg()
    assert searcher.can_search(role=_self_auth_role(), org=broken) is False  # type: ignore[arg-type]
    assert await searcher.search(query="q", role=_self_auth_role(), org=broken) == []  # type: ignore[arg-type]
    assert transport.new_user_calls == []
    assert transport._http_client.get_urls == []


# ---------------------------------------------------------------------------
# ConfluenceSearcher.search -- result mapping
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_search_maps_results_to_knowledge_hits():
    transport = _FakeTransport(
        response=_response(
            [
                _page(
                    page_id="1",
                    title="Deploy Runbook",
                    body="<p>How to deploy. More detail.</p>",
                )
            ]
        )
    )
    searcher = ConfluenceSearcher(transport=transport)
    hits = await searcher.search(query="deploy", role=None, org=_org(["ENG"]))
    assert len(hits) == 1
    hit = hits[0]
    assert isinstance(hit, KnowledgeHit)
    assert hit.title == "Deploy Runbook"
    assert hit.page_id == "1"
    assert hit.container == "ENG"
    # The snippet is the first sentence (the ". " boundary is the cut).
    assert hit.snippet == "How to deploy"
    assert hit.url == "https://wiki.example/wiki/spaces/ENG/pages/1"


@pytest.mark.asyncio
async def test_search_scope_comes_from_org_sorted():
    """The space scope is derived from ``org.confluence_spaces`` inside
    ``search()`` -- normalised, deduped, and sorted."""
    transport = _FakeTransport(response=_response([_page(page_id="1", title="X")]))
    searcher = ConfluenceSearcher(transport=transport)
    await searcher.search(query="q", role=None, org=_org(["general", "eng", " ENG "]))
    decoded = unquote(transport._http_client.get_urls[0])
    assert 'space IN ("ENG","GENERAL")' in decoded


@pytest.mark.asyncio
async def test_search_drops_auto_draft_ancestors():
    transport = _FakeTransport(
        response=_response(
            [
                _page(page_id="1", title="Published", ancestors=["Runbooks"]),
                _page(page_id="2", title="Draft", ancestors=[AUTO_DRAFTED_PARENT]),
            ]
        )
    )
    searcher = ConfluenceSearcher(transport=transport)
    hits = await searcher.search(
        query="q",
        role=None,
        org=_org(["ENG"]),
        exclude_ancestors=[AUTO_DRAFTED_PARENT],
    )
    titles = [h.title for h in hits]
    assert titles == ["Published"]


@pytest.mark.asyncio
async def test_search_honors_limit():
    pages = [_page(page_id=str(i), title=f"Doc {i}") for i in range(8)]
    transport = _FakeTransport(response=_response(pages))
    searcher = ConfluenceSearcher(transport=transport)
    hits = await searcher.search(query="q", role=None, org=_org(["ENG"]), limit=3)
    assert len(hits) == 3


@pytest.mark.asyncio
async def test_search_empty_scope_credential_less_returns_empty():
    transport = _FakeTransport(response=_response([_page(page_id="1", title="X")]))
    searcher = ConfluenceSearcher(transport=transport)
    assert await searcher.search(query="q", role=None, org=_org()) == []


@pytest.mark.asyncio
async def test_search_unscoped_for_self_auth_role_without_spaces():
    """A self-authenticating role with no configured spaces searches
    unscoped -- the CQL omits ``space IN`` and runs as the agent, so
    Confluence ACLs bound the hits."""
    transport = _FakeTransport(response=_response([_page(page_id="1", title="X")]))
    searcher = ConfluenceSearcher(transport=transport)
    hits = await searcher.search(query="deploy", role=_self_auth_role(), org=_org())
    assert hits  # results came back
    assert transport.new_user_calls == [("e@a.com", "t")]  # ran as the agent
    decoded = unquote(transport.user_clients[0].get_urls[0])
    assert "space IN" not in decoded
    assert 'text ~ "deploy"' in decoded


@pytest.mark.asyncio
async def test_search_no_spaces_no_creds_searches_nothing():
    """A credential-less role with no spaces issues no request -- it would
    otherwise search the admin's whole instance."""
    transport = _FakeTransport(response=_response([_page(page_id="1", title="X")]))
    searcher = ConfluenceSearcher(transport=transport)
    hits = await searcher.search(query="q", role=Role(name="Eng"), org=_org())
    assert hits == []
    assert transport._http_client.get_urls == []  # never issued


@pytest.mark.asyncio
async def test_search_non_200_returns_empty():
    transport = _FakeTransport(response=_FakeResponse(status_code=503))
    searcher = ConfluenceSearcher(transport=transport)
    assert await searcher.search(query="q", role=None, org=_org(["ENG"])) == []


@pytest.mark.asyncio
async def test_search_transport_exception_returns_empty():
    transport = _FakeTransport(response=_response([]), raise_on_get=True)
    searcher = ConfluenceSearcher(transport=transport)
    assert await searcher.search(query="q", role=None, org=_org(["ENG"])) == []


# ---------------------------------------------------------------------------
# Auth selection
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_search_uses_admin_client_when_role_has_no_creds():
    transport = _FakeTransport(response=_response([_page(page_id="1", title="X")]))
    searcher = ConfluenceSearcher(transport=transport)
    role = Role(name="Engineer")  # no atlassian mcp_env
    await searcher.search(query="q", role=role, org=_org(["ENG"]))
    assert transport.new_user_calls == []
    assert transport._http_client.get_urls  # admin client was used


@pytest.mark.asyncio
async def test_search_uses_per_agent_cloud_token_when_present():
    transport = _FakeTransport(response=_response([_page(page_id="1", title="X")]))
    searcher = ConfluenceSearcher(transport=transport)
    role = Role(
        name="Engineer",
        mcp_env={
            "atlassian": {
                "CONFLUENCE_USERNAME": "eng@acme.com",
                "CONFLUENCE_API_TOKEN": "agent-token",
            }
        },
    )
    await searcher.search(query="q", role=role, org=_org(["ENG"]))
    assert transport.new_user_calls == [("eng@acme.com", "agent-token")]
    # The per-agent client is owned by the searcher and closed after use.
    assert transport.user_clients[0].closed is True


@pytest.mark.asyncio
async def test_search_uses_data_center_personal_token():
    transport = _FakeTransport(response=_response([_page(page_id="1", title="X")]))
    searcher = ConfluenceSearcher(transport=transport)
    role = Role(
        name="Engineer",
        mcp_env={"atlassian": {"CONFLUENCE_PERSONAL_TOKEN": "dc-pat"}},
    )
    await searcher.search(query="q", role=role, org=_org(["ENG"]))
    # No username -> Bearer auth path (the transport branch handles it).
    assert transport.new_user_calls == [("", "dc-pat")]


@pytest.mark.asyncio
async def test_search_resolves_env_var_refs_in_mcp_env(monkeypatch):
    monkeypatch.setenv("CFL_AGENT_TOKEN", "resolved-secret")
    transport = _FakeTransport(response=_response([_page(page_id="1", title="X")]))
    searcher = ConfluenceSearcher(transport=transport)
    role = Role(
        name="Engineer",
        mcp_env={
            "atlassian": {
                "CONFLUENCE_USERNAME": "eng@acme.com",
                "CONFLUENCE_API_TOKEN": "${CFL_AGENT_TOKEN}",
            }
        },
    )
    await searcher.search(query="q", role=role, org=_org(["ENG"]))
    assert transport.new_user_calls == [("eng@acme.com", "resolved-secret")]
