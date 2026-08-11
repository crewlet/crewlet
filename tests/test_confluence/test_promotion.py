"""Tests for ``crewlet.confluence.promotion.ConfluencePromotionWriter``."""

from __future__ import annotations

from typing import Any

from crewlet.confluence.promotion import ConfluencePromotionWriter
from crewlet.knowledge.markdown_docs import render_markdown
from crewlet.learning.skill_synthesizer import PromotionPageWriter
from crewlet.org.models import Organization, OrgUnit, Role

_BODY_MD = "**Team routine.**\n\n1. Ship\n2. Verify\n\n- `jira_search`"


class _Response:
    def __init__(
        self, *, status_code: int, payload: Any = None, text: str = ""
    ) -> None:
        self.status_code = status_code
        self._payload = payload if payload is not None else {}
        self.text = text

    def json(self) -> Any:
        return self._payload

    def raise_for_status(self) -> None:
        if self.status_code >= 400:
            raise RuntimeError(f"HTTP {self.status_code}")


class _HTTPClientStub:
    """Duck-typed httpx client for the ``pages.py`` helpers."""

    def __init__(
        self,
        *,
        parent_id: str | None = "PARENT-1",
        post_status: int = 200,
        post_response_id: str | None = "DRAFT-42",
        get_status: int = 200,
    ) -> None:
        self.gets: list[dict[str, Any]] = []
        self.posts: list[dict[str, Any]] = []
        self._parent_id = parent_id
        self._post_status = post_status
        self._post_response_id = post_response_id
        self._get_status = get_status

    async def get(self, url: str, params: dict[str, Any] | None = None) -> _Response:
        self.gets.append({"url": url, "params": dict(params or {})})
        results = [{"id": self._parent_id}] if self._parent_id else []
        return _Response(status_code=self._get_status, payload={"results": results})

    async def post(self, url: str, *, json: Any) -> _Response:
        self.posts.append({"url": url, "body": json})
        if self._post_status not in (200, 201):
            return _Response(status_code=self._post_status, text="denied")
        return _Response(
            status_code=self._post_status,
            payload={"id": self._post_response_id or ""},
        )


class _TransportStub:
    def __init__(self, http_client: _HTTPClientStub) -> None:
        self._http_client = http_client
        self._rest_base = "https://example.atlassian.net/wiki"


def _mk_org(space_key: str = "ENG") -> Organization:
    unit = OrgUnit(
        name="Eng",
        type="team",
        lead="Alice",
        roles=[Role(name="Alice", handle="alice")],
        confluence_space=space_key,
    )
    return Organization(name="Acme", units=[unit])


def test_writer_satisfies_protocol_and_resolves_container() -> None:
    writer: PromotionPageWriter = ConfluencePromotionWriter(
        transport=_TransportStub(_HTTPClientStub())
    )
    assert writer.backend == "confluence"
    assert writer.resolve_unit_container(_mk_org(), "Eng") == "ENG"
    assert writer.resolve_unit_container(_mk_org(space_key=""), "Eng") == ""
    assert "integrations.confluence.space" in writer.missing_container_hint()


async def test_create_draft_posts_rendered_xhtml_under_parent() -> None:
    http = _HTTPClientStub(parent_id="PARENT-AUTODRAFT", post_response_id="PAGE-99")
    writer = ConfluencePromotionWriter(transport=_TransportStub(http))

    page_id = await writer.create_draft_page(
        container="ENG",
        parent_title="Auto-Drafted Skills",
        title="[Auto-draft] ship-routine",
        name="ship-routine",
        body_markdown=_BODY_MD,
    )

    assert page_id == "PAGE-99"
    # Parent resolved by exact title in the container space.
    assert http.gets[0]["params"]["spaceKey"] == "ENG"
    assert http.gets[0]["params"]["title"] == "Auto-Drafted Skills"
    # One POST creates the page nested under the parent; the body is
    # REAL rendered XHTML (not <br/>-joined prose), and no label is
    # stamped (label-less drafts; publish = move out of the parent).
    assert len(http.posts) == 1
    body = http.posts[0]["body"]
    assert body["space"] == {"key": "ENG"}
    assert body["title"] == "[Auto-draft] ship-routine"
    assert body["ancestors"] == [{"id": "PARENT-AUTODRAFT"}]
    storage = body["body"]["storage"]["value"]
    assert storage == render_markdown(_BODY_MD)
    assert "<strong>Team routine.</strong>" in storage
    assert "<ol>" in storage  # the numbered steps render as a list
    assert all("/label" not in p["url"] for p in http.posts)


async def test_create_draft_missing_parent_lands_at_root() -> None:
    http = _HTTPClientStub(parent_id=None, post_response_id="PAGE-7")
    writer = ConfluencePromotionWriter(transport=_TransportStub(http))

    page_id = await writer.create_draft_page(
        container="ENG",
        parent_title="Auto-Drafted Skills",
        title="[Auto-draft] x",
        name="x",
        body_markdown="body",
    )

    assert page_id == "PAGE-7"
    assert "ancestors" not in http.posts[0]["body"]


async def test_create_draft_returns_none_on_post_failure() -> None:
    http = _HTTPClientStub(post_status=403)
    writer = ConfluencePromotionWriter(transport=_TransportStub(http))

    page_id = await writer.create_draft_page(
        container="ENG",
        parent_title="Auto-Drafted Skills",
        title="[Auto-draft] x",
        name="x",
        body_markdown="body",
    )

    assert page_id is None


async def test_create_draft_degrades_without_transport() -> None:
    writer = ConfluencePromotionWriter(transport=None)
    assert (
        await writer.create_draft_page(
            container="ENG",
            parent_title="Auto-Drafted Skills",
            title="t",
            name="t",
            body_markdown="b",
        )
        is None
    )
