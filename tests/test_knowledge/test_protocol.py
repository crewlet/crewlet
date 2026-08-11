"""Tests for the backend-neutral ``KnowledgeSearcher`` seam.

Covers the ``KnowledgeHit`` model shape, the canonical auto-draft
constants (single-sourced here for the prefetch exclusion, the
promotion synthesizer, and the Plane searcher's title backstop), and
protocol conformance of the concrete backends.
"""

from __future__ import annotations

import inspect

from crewlet.knowledge.confluence_search import ConfluenceSearcher
from crewlet.knowledge.plane_search import PlaneSearcher
from crewlet.knowledge.protocol import (
    AUTO_DRAFT_TITLE_PREFIX,
    AUTO_DRAFTED_PARENT,
    KnowledgeHit,
    KnowledgeSearcher,
)

# ---------------------------------------------------------------------------
# Canonical constants
# ---------------------------------------------------------------------------


def test_auto_drafted_parent_literal_pinned() -> None:
    """The parent title is a live-content contract: existing Confluence
    ``Auto-Drafted Skills`` pages must keep matching the exclusion."""
    assert AUTO_DRAFTED_PARENT == "Auto-Drafted Skills"


def test_auto_draft_title_prefix_literal_pinned() -> None:
    """The stamped title prefix is a live-content contract too: pages
    drafted by older engine versions must keep matching the backstop."""
    assert AUTO_DRAFT_TITLE_PREFIX == "[Auto-draft] "


def test_constants_are_single_sourced() -> None:
    """The prefetch and the promotion synthesizer import the canonical
    constants rather than re-defining them (the defined-twice defect)."""
    from crewlet.learning import relevant_knowledge, skill_synthesizer

    assert relevant_knowledge.AUTO_DRAFTED_PARENT is AUTO_DRAFTED_PARENT
    assert skill_synthesizer.AUTO_DRAFTED_PARENT is AUTO_DRAFTED_PARENT
    assert (
        skill_synthesizer.PromotionSynthesizer.AUTO_DRAFTED_PARENT
        is AUTO_DRAFTED_PARENT
    )
    assert skill_synthesizer.AUTO_DRAFT_TITLE_PREFIX is AUTO_DRAFT_TITLE_PREFIX


# ---------------------------------------------------------------------------
# KnowledgeHit
# ---------------------------------------------------------------------------


def test_knowledge_hit_defaults() -> None:
    hit = KnowledgeHit()
    assert hit.title == ""
    assert hit.url == ""
    assert hit.container == ""
    assert hit.page_id == ""
    assert hit.snippet == ""
    assert hit.ancestors == []


def test_knowledge_hit_ancestors_default_is_per_instance() -> None:
    a, b = KnowledgeHit(), KnowledgeHit()
    a.ancestors.append("Runbooks")
    assert b.ancestors == []


def test_knowledge_hit_full_shape() -> None:
    hit = KnowledgeHit(
        title="Deploy Runbook",
        url="https://wiki.example/wiki/spaces/ENG/pages/1",
        container="ENG",
        page_id="1",
        snippet="How to deploy",
        ancestors=["Runbooks"],
    )
    assert hit.container == "ENG"
    assert hit.ancestors == ["Runbooks"]


# ---------------------------------------------------------------------------
# Protocol conformance
# ---------------------------------------------------------------------------


class _Transport:
    """Bare transport stand-in -- conformance needs construction only."""

    _rest_base = ""
    _shareable_base_url = ""
    _http_client = None

    def new_user_client(self, username: str, token: str) -> object:
        raise AssertionError("not used in conformance tests")


def test_confluence_backend_satisfies_protocol() -> None:
    # Static conformance: assignment to the protocol type must
    # type-check without a type-ignore.
    searcher: KnowledgeSearcher = ConfluenceSearcher(transport=_Transport())
    # Runtime conformance (structural): the protocol is
    # ``runtime_checkable``.
    assert isinstance(searcher, KnowledgeSearcher)


def test_confluence_backend_search_signature_matches_protocol() -> None:
    """The concrete ``search`` keeps the protocol's keyword-only
    surface and defaults -- callers bind by name across backends."""
    sig = inspect.signature(ConfluenceSearcher.search)
    params = dict(sig.parameters)
    params.pop("self")
    assert list(params) == ["query", "role", "org", "limit", "exclude_ancestors"]
    assert all(p.kind is inspect.Parameter.KEYWORD_ONLY for p in params.values())
    assert params["limit"].default == 8
    assert params["exclude_ancestors"].default is None


def test_confluence_backend_can_search_signature_matches_protocol() -> None:
    sig = inspect.signature(ConfluenceSearcher.can_search)
    params = dict(sig.parameters)
    params.pop("self")
    assert list(params) == ["role", "org"]
    assert all(p.kind is inspect.Parameter.KEYWORD_ONLY for p in params.values())


class _PlaneTransportStub:
    """Bare transport stand-in -- conformance needs construction only."""


def test_plane_backend_satisfies_protocol() -> None:
    # Static conformance: assignment to the protocol type must
    # type-check without a type-ignore.
    searcher: KnowledgeSearcher = PlaneSearcher(transport=_PlaneTransportStub())
    # Runtime conformance (structural): the protocol is
    # ``runtime_checkable``.
    assert isinstance(searcher, KnowledgeSearcher)


def test_plane_backend_search_signature_matches_protocol() -> None:
    """The concrete ``search`` keeps the protocol's keyword-only
    surface and defaults -- callers bind by name across backends."""
    sig = inspect.signature(PlaneSearcher.search)
    params = dict(sig.parameters)
    params.pop("self")
    assert list(params) == ["query", "role", "org", "limit", "exclude_ancestors"]
    assert all(p.kind is inspect.Parameter.KEYWORD_ONLY for p in params.values())
    assert params["limit"].default == 8
    assert params["exclude_ancestors"].default is None


def test_plane_backend_can_search_signature_matches_protocol() -> None:
    sig = inspect.signature(PlaneSearcher.can_search)
    params = dict(sig.parameters)
    params.pop("self")
    assert list(params) == ["role", "org"]
    assert all(p.kind is inspect.Parameter.KEYWORD_ONLY for p in params.values())
