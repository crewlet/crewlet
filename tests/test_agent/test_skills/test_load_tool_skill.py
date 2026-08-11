"""Tests for the ``load_tool_skill`` builtin tool."""

from __future__ import annotations

import pytest

from crewlet.agent.skills import (
    Phase,
    PromptSkill,
    PromptSkillRegistry,
    TriggerExpr,
)
from crewlet.tools.builtin import _load_tool_skill
from crewlet.tools.protocol import AgentContext


def _skill(key: str, *, body: str = "BODY") -> PromptSkill:
    return PromptSkill(
        key=key,
        trigger=TriggerExpr(tool=key),
        phases={Phase.PLAN},
        title=key.upper(),
        summary=f"summary of {key}",
        body=body,
    )


def _ctx(registry: PromptSkillRegistry | None) -> AgentContext:
    return AgentContext(prompt_skill_registry=registry)


@pytest.mark.asyncio
async def test_returns_body_for_existing_key() -> None:
    reg = PromptSkillRegistry()
    reg.seed([_skill("mcp:github", body="THE GITHUB BODY")])
    result = await _load_tool_skill({"key": "mcp:github"}, _ctx(reg))
    assert result.success
    assert "MCP:GITHUB" in result.output  # title
    assert "THE GITHUB BODY" in result.output


@pytest.mark.asyncio
async def test_renders_skill_variables_in_title_and_body() -> None:
    reg = PromptSkillRegistry()
    reg.seed(
        [
            _skill(
                "skill:platform_mentions",
                body="share ${confluence_base_url}/spaces/{space_key}/pages/{page_id}",
            )
        ]
    )
    reg.set_variables({"confluence_base_url": "https://acme.atlassian.net/wiki"})
    result = await _load_tool_skill({"key": "skill:platform_mentions"}, _ctx(reg))
    assert result.success
    # ${var} substituted, single-brace placeholders preserved, no gateway URL.
    assert "https://acme.atlassian.net/wiki/spaces/{space_key}/pages/{page_id}" in (
        result.output or ""
    )
    assert "${confluence_base_url}" not in (result.output or "")


@pytest.mark.asyncio
async def test_returns_error_for_missing_key() -> None:
    reg = PromptSkillRegistry()
    reg.seed([_skill("mcp:github")])
    result = await _load_tool_skill({"key": "mcp:nope"}, _ctx(reg))
    assert not result.success
    assert "No tool skill registered" in (result.error or "")
    # The error includes available keys so the LLM can recover.
    assert "mcp:github" in (result.error or "")


@pytest.mark.asyncio
async def test_returns_error_when_registry_unconfigured() -> None:
    result = await _load_tool_skill({"key": "mcp:github"}, _ctx(None))
    assert not result.success
    assert "not configured" in (result.error or "")


@pytest.mark.asyncio
async def test_returns_error_when_key_blank() -> None:
    reg = PromptSkillRegistry()
    result = await _load_tool_skill({"key": ""}, _ctx(reg))
    assert not result.success
    assert "required" in (result.error or "")


@pytest.mark.asyncio
async def test_returns_error_when_key_missing_from_params() -> None:
    reg = PromptSkillRegistry()
    result = await _load_tool_skill({}, _ctx(reg))
    assert not result.success
