"""Tests for the relevant-knowledge digest used by the Plan-phase prompt.

The digest is backed by the query-time ``KnowledgeSearcher`` seam: one
aux-LLM call turns the trigger into a text query, the searcher runs it
against the team knowledge base, and the top hits render as bullets.

Covers: empty / unwired returns, the ``can_search`` pre-gate, the
thin-trigger gate, the aux-LLM query-generation call (happy path +
failure modes), the no-results hint, char-budget truncation, and
``KnowledgeHit`` bullet rendering.
"""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.knowledge.protocol import KnowledgeHit
from crewlet.learning.relevant_knowledge import (
    AUTO_DRAFTED_PARENT,
    EMPTY_FILTER_HINT,
    _render_bullet,
    fetch_relevant_knowledge_block,
)
from crewlet.org.models import Organization, Role
from crewlet.providers.llm.protocol import Completion


class _ScriptedProvider:
    """LLM stub returning queued :class:`Completion` objects."""

    model = "scripted"

    def __init__(self, completions: list[Completion]) -> None:
        self._completions = list(completions)
        self.calls: list[dict[str, Any]] = []

    async def complete(
        self,
        messages,
        tools=None,
        temperature=0.7,
        max_tokens=None,
        tool_choice=None,
    ):
        self.calls.append({"messages": messages})
        if self._completions:
            return self._completions.pop(0)
        return Completion(content="")


class _RaisingProvider:
    """LLM stub that raises on every ``complete`` call."""

    model = "raising"

    async def complete(
        self,
        messages,
        tools=None,
        temperature=0.7,
        max_tokens=None,
        tool_choice=None,
    ):
        raise RuntimeError("aux LLM unavailable")


class _SearcherStub:
    """Protocol-shaped stand-in for a ``KnowledgeSearcher`` backend.

    Records each ``search`` call so tests can assert the aux-generated
    query, the pass-through role/org, and the auto-draft exclusion
    reached it.  ``can_search_result`` scripts the pre-gate.
    """

    def __init__(
        self, results: list[KnowledgeHit], *, can_search_result: bool = True
    ) -> None:
        self.results = list(results)
        self.calls: list[dict[str, Any]] = []
        self.can_search_calls: list[dict[str, Any]] = []
        self.can_search_result = can_search_result
        self.raise_on_search = False

    def can_search(self, *, role: Any, org: Any) -> bool:
        self.can_search_calls.append({"role": role, "org": org})
        return self.can_search_result

    async def search(
        self,
        *,
        query: str,
        role: Any,
        org: Any,
        limit: int = 8,
        exclude_ancestors: list[str] | None = None,
    ) -> list[KnowledgeHit]:
        self.calls.append(
            {
                "query": query,
                "role": role,
                "org": org,
                "limit": limit,
                "exclude_ancestors": list(exclude_ancestors or []),
            }
        )
        if self.raise_on_search:
            raise RuntimeError("knowledge search outage")
        return list(self.results)


def _hit(title: str, snippet: str = "", container: str = "ENG") -> KnowledgeHit:
    return KnowledgeHit(
        title=title,
        url=f"https://wiki.example/{title}",
        container=container,
        page_id=title,
        snippet=snippet,
    )


def _org_role(space: str | None = "ENG") -> tuple[Organization, Role]:
    """A root-level role + an org-wide Confluence read scope (or none).

    Read scope comes from ``org.confluence_spaces`` -- a unit's /
    role's own space identity does not narrow reads.
    """
    role = Role(name="Engineer")
    org = Organization(
        name="Acme",
        roles=[role],
        confluence_spaces=[space] if space else [],
    )
    return org, role


@pytest.mark.asyncio
async def test_empty_task_returns_empty():
    org, role = _org_role()
    out = await fetch_relevant_knowledge_block(
        searcher=_SearcherStub([]),
        role=role,
        org=org,
        task_description="   ",
    )
    assert out == ""


@pytest.mark.asyncio
async def test_missing_searcher_returns_empty():
    org, role = _org_role()
    out = await fetch_relevant_knowledge_block(
        searcher=None,
        role=role,
        org=org,
        task_description="ship the thing",
        llm_providers={"default": _ScriptedProvider([Completion(content="q")])},
    )
    assert out == ""


@pytest.mark.asyncio
async def test_can_search_false_returns_empty_without_aux_call():
    """The searcher's cheap pre-gate short-circuits the whole fetch --
    no aux-LLM query-generation call, no search."""
    org, role = _org_role(space=None)
    provider = _ScriptedProvider([Completion(content="q")])
    searcher = _SearcherStub([_hit("Runbook")], can_search_result=False)
    out = await fetch_relevant_knowledge_block(
        searcher=searcher,
        role=role,
        org=org,
        task_description="ship the thing",
        llm_providers={"default": provider},
    )
    assert out == ""
    assert provider.calls == []  # aux call skipped
    assert searcher.calls == []  # search skipped
    # The gate was consulted with the per-call role/org.
    assert searcher.can_search_calls == [{"role": role, "org": org}]


@pytest.mark.asyncio
async def test_can_search_true_flows_role_and_org_to_search():
    """``role`` / ``org`` are per-call parameters -- the searcher
    resolves scope (and unscoped-vs-nothing) behind the seam."""
    org, role = _org_role()
    searcher = _SearcherStub([_hit("Runbook", snippet="how to deploy")])
    out = await fetch_relevant_knowledge_block(
        searcher=searcher,
        role=role,
        org=org,
        task_description="ship the thing",
        llm_providers={"default": _ScriptedProvider([Completion(content="q")])},
    )
    assert "Runbook" in out
    call = searcher.calls[-1]
    assert call["role"] is role
    assert call["org"] is org


@pytest.mark.asyncio
async def test_thin_trigger_returns_hint_and_skips_search():
    org, role = _org_role()
    searcher = _SearcherStub([_hit("Runbook")])
    out = await fetch_relevant_knowledge_block(
        searcher=searcher,
        role=role,
        org=org,
        task_description="PR #12",
        llm_providers={"default": _ScriptedProvider([Completion(content="q")])},
        trigger_requires_recon=True,
    )
    assert out == EMPTY_FILTER_HINT
    assert searcher.calls == []


@pytest.mark.asyncio
async def test_no_llm_providers_returns_empty():
    org, role = _org_role()
    out = await fetch_relevant_knowledge_block(
        searcher=_SearcherStub([_hit("Runbook")]),
        role=role,
        org=org,
        task_description="ship the thing",
        llm_providers=None,
    )
    assert out == ""


@pytest.mark.asyncio
async def test_happy_path_renders_bullets():
    org, role = _org_role()
    provider = _ScriptedProvider([Completion(content="deployment runbook")])
    searcher = _SearcherStub(
        [
            _hit("Deploy Runbook", "How to deploy hotfixes."),
            _hit("Release Checklist", "Steps before a release."),
        ]
    )
    out = await fetch_relevant_knowledge_block(
        searcher=searcher,
        role=role,
        org=org,
        task_description="deploy a hotfix",
        llm_providers={"default": provider},
    )
    assert "**Deploy Runbook**: How to deploy hotfixes." in out
    assert "**Release Checklist**: Steps before a release." in out
    # The aux-generated query reached the searcher, with auto-drafts
    # excluded by default.
    assert len(searcher.calls) == 1
    call = searcher.calls[0]
    assert call["query"] == "deployment runbook"
    assert AUTO_DRAFTED_PARENT in call["exclude_ancestors"]


@pytest.mark.asyncio
async def test_aux_user_prompt_is_backend_neutral():
    """The query-generation user prompt asks for a knowledge-base
    query, naming no backend."""
    org, role = _org_role()
    provider = _ScriptedProvider([Completion(content="q")])
    await fetch_relevant_knowledge_block(
        searcher=_SearcherStub([_hit("Runbook")]),
        role=role,
        org=org,
        task_description="deploy a hotfix",
        llm_providers={"default": provider},
    )
    user_msgs = [m for m in provider.calls[0]["messages"] if m.role == "user"]
    assert user_msgs
    assert user_msgs[-1].content.endswith("Knowledge-base search query:")
    assert "Confluence" not in user_msgs[-1].content


@pytest.mark.asyncio
async def test_no_results_returns_hint():
    org, role = _org_role()
    out = await fetch_relevant_knowledge_block(
        searcher=_SearcherStub([]),
        role=role,
        org=org,
        task_description="deploy a hotfix",
        llm_providers={"default": _ScriptedProvider([Completion(content="q")])},
    )
    assert out == EMPTY_FILTER_HINT


@pytest.mark.asyncio
async def test_aux_empty_response_returns_empty_and_skips_search():
    org, role = _org_role()
    searcher = _SearcherStub([_hit("Runbook")])
    out = await fetch_relevant_knowledge_block(
        searcher=searcher,
        role=role,
        org=org,
        task_description="deploy a hotfix",
        llm_providers={"default": _ScriptedProvider([Completion(content="   ")])},
    )
    assert out == ""
    assert searcher.calls == []


@pytest.mark.asyncio
async def test_aux_raises_returns_empty():
    org, role = _org_role()
    out = await fetch_relevant_knowledge_block(
        searcher=_SearcherStub([_hit("Runbook")]),
        role=role,
        org=org,
        task_description="deploy a hotfix",
        llm_providers={"default": _RaisingProvider()},
    )
    assert out == ""


@pytest.mark.asyncio
async def test_search_failure_returns_empty():
    org, role = _org_role()
    searcher = _SearcherStub([_hit("Runbook")])
    searcher.raise_on_search = True
    out = await fetch_relevant_knowledge_block(
        searcher=searcher,
        role=role,
        org=org,
        task_description="deploy a hotfix",
        llm_providers={"default": _ScriptedProvider([Completion(content="q")])},
    )
    assert out == ""


@pytest.mark.asyncio
async def test_char_budget_truncates():
    org, role = _org_role()
    hits = [_hit(f"Doc {i}", "x" * 120) for i in range(6)]
    out = await fetch_relevant_knowledge_block(
        searcher=_SearcherStub(hits),
        role=role,
        org=org,
        task_description="deploy a hotfix",
        llm_providers={"default": _ScriptedProvider([Completion(content="q")])},
        char_budget=160,
    )
    # Only the bullets that fit the budget render -- far fewer than 6.
    assert 0 < out.count("**Doc") < 6


def test_render_bullet_title_and_snippet():
    assert (
        _render_bullet(KnowledgeHit(title="Runbook", snippet="Deploy steps."))
        == "- **Runbook**: Deploy steps."
    )


def test_render_bullet_title_only():
    assert _render_bullet(KnowledgeHit(title="Runbook")) == "- **Runbook**"
