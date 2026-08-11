"""Unit tests for the ``query_episodes`` builtin tool."""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from crewlet.learning.models import Episode
from crewlet.learning.tools import register_episode_tools
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry


class _FakeEpisodeStore:
    """In-memory stand-in for ``EpisodeStore`` used in tool tests."""

    def __init__(self, episodes: list[Episode] | None = None) -> None:
        self._episodes = episodes or []
        self.queries: list[dict[str, Any]] = []

    async def write(self, episode: Episode) -> None:  # pragma: no cover - unused
        self._episodes.append(episode)

    async def query(
        self,
        *,
        agent_handle: str,
        query_text: str,
        limit: int = 5,
        outcome_filter: str | None = None,
    ) -> list[Episode]:
        self.queries.append(
            {
                "agent_handle": agent_handle,
                "query_text": query_text,
                "limit": limit,
                "outcome_filter": outcome_filter,
            }
        )
        out = [e for e in self._episodes if e.agent_handle == agent_handle]
        if outcome_filter is not None:
            out = [e for e in out if e.review_outcome == outcome_filter]
        return out[:limit]


def _mk_episode(**overrides: Any) -> Episode:
    base = {
        "agent_handle": "alice",
        "agent_role": "Engineer",
        "task_id": "",
        "turn_id": uuid4(),
        "started_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        "ended_at": datetime(2026, 4, 23, 9, 2, tzinfo=UTC),
        "plan_summary": "Plan: do X",
        "task_summary": "Do X for Y",
        "tool_sequence": ["jira_comment"],
        "skills_used": [],
        "review_outcome": "done",
        "duration_ms": 60_000,
    }
    base.update(overrides)
    return Episode(**base)


async def test_register_and_invoke_returns_formatted_episodes() -> None:
    registry = ToolRegistry()
    store = _FakeEpisodeStore([_mk_episode()])

    register_episode_tools(registry, store)

    tool = registry.get("query_episodes")
    assert tool is not None
    assert tool.parameters["required"] == ["query"]

    ctx = AgentContext(agent_id="a1", agent_handle="alice")
    result = await tool.execute({"query": "how to do X"}, ctx)

    assert result.success
    payload = json.loads(result.output)
    assert "episodes" in payload
    assert len(payload["episodes"]) == 1
    assert payload["episodes"][0]["tool_sequence"] == ["jira_comment"]
    assert store.queries[0]["agent_handle"] == "alice"
    assert store.queries[0]["limit"] == 5


async def test_empty_query_returns_error() -> None:
    registry = ToolRegistry()
    register_episode_tools(registry, _FakeEpisodeStore())
    tool = registry.get("query_episodes")
    assert tool is not None

    result = await tool.execute(
        {"query": ""}, AgentContext(agent_id="a1", agent_handle="alice")
    )

    assert not result.success
    assert result.error is not None and "query" in result.error.lower()


async def test_missing_agent_handle_returns_error() -> None:
    registry = ToolRegistry()
    register_episode_tools(registry, _FakeEpisodeStore())
    tool = registry.get("query_episodes")
    assert tool is not None

    result = await tool.execute(
        {"query": "anything"}, AgentContext(agent_id="a1", agent_handle="")
    )

    assert not result.success
    assert result.error is not None and "agent_handle" in result.error


async def test_invalid_outcome_filter_rejected() -> None:
    registry = ToolRegistry()
    register_episode_tools(registry, _FakeEpisodeStore())
    tool = registry.get("query_episodes")
    assert tool is not None

    result = await tool.execute(
        {"query": "x", "outcome_filter": "bogus"},
        AgentContext(agent_id="a1", agent_handle="alice"),
    )

    assert not result.success
    assert result.error is not None and "outcome_filter" in result.error


async def test_valid_outcome_filter_forwarded_to_store() -> None:
    registry = ToolRegistry()
    store = _FakeEpisodeStore(
        [
            _mk_episode(review_outcome="done"),
            _mk_episode(review_outcome="failed"),
        ]
    )
    register_episode_tools(registry, store)
    tool = registry.get("query_episodes")
    assert tool is not None

    result = await tool.execute(
        {"query": "x", "outcome_filter": "failed"},
        AgentContext(agent_id="a1", agent_handle="alice"),
    )

    assert result.success
    payload = json.loads(result.output)
    assert len(payload["episodes"]) == 1
    assert payload["episodes"][0]["review_outcome"] == "failed"
    assert store.queries[0]["outcome_filter"] == "failed"


async def test_limit_clamped_to_valid_range() -> None:
    registry = ToolRegistry()
    store = _FakeEpisodeStore()
    register_episode_tools(registry, store)
    tool = registry.get("query_episodes")
    assert tool is not None

    # Above 20 → clamped to 20
    await tool.execute(
        {"query": "x", "limit": 500},
        AgentContext(agent_id="a1", agent_handle="alice"),
    )
    # Below 1 → clamped to 1
    await tool.execute(
        {"query": "x", "limit": 0},
        AgentContext(agent_id="a1", agent_handle="alice"),
    )

    assert store.queries[0]["limit"] == 20
    assert store.queries[1]["limit"] == 1


async def test_no_episodes_returns_friendly_output() -> None:
    registry = ToolRegistry()
    register_episode_tools(registry, _FakeEpisodeStore([]))
    tool = registry.get("query_episodes")
    assert tool is not None

    result = await tool.execute(
        {"query": "nothing"}, AgentContext(agent_id="a1", agent_handle="alice")
    )

    assert result.success
    assert "no prior episodes" in result.output
