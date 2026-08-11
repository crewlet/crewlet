"""Tests for auxiliary-model summarisation of episode hits."""

from __future__ import annotations

from typing import Any

from crewlet.learning.summarize import summarize_episodes
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import Completion


class _CountingProvider:
    def __init__(self, response: str) -> None:
        self._response = response
        self.calls: list[dict[str, Any]] = []
        self.model = "mock"

    async def complete(
        self,
        messages,
        tools=None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice=None,
    ):
        self.calls.append(
            {
                "system": messages[0].content if messages else "",
                "user": messages[1].content if len(messages) > 1 else "",
                "temperature": temperature,
                "max_tokens": max_tokens,
            }
        )
        return Completion(content=self._response)


class _RaisingProvider:
    def __init__(self) -> None:
        self.model = "rain"

    async def complete(self, *args, **kwargs):
        raise RuntimeError("provider down")


async def test_summarize_uses_auxiliary_provider_and_returns_result() -> None:
    role = Role(name="Engineer", handle="eng", llm="cheap", llm_auxiliary="cheap")
    provider = _CountingProvider(response="- [done] foo -- tools: search")
    out = await summarize_episodes(
        raw_payload='{"episodes": [{"x": 1}]}',
        role=role,
        llm_providers={"cheap": provider},
        max_tokens=100,
    )
    assert out.startswith("- [done]")
    assert len(provider.calls) == 1
    assert "summari" in provider.calls[0]["system"].lower()
    assert provider.calls[0]["max_tokens"] == 100


async def test_summarize_falls_back_to_raw_on_provider_failure() -> None:
    role = Role(name="Engineer", handle="eng", llm="fails", llm_auxiliary="fails")
    raw = '{"episodes": [{"x": 1}]}'
    out = await summarize_episodes(
        raw_payload=raw,
        role=role,
        llm_providers={"fails": _RaisingProvider()},
        max_tokens=100,
    )
    assert out == raw


async def test_summarize_falls_back_when_empty_response() -> None:
    role = Role(name="Engineer", handle="eng", llm="cheap", llm_auxiliary="cheap")
    raw = '{"episodes": []}'
    out = await summarize_episodes(
        raw_payload=raw,
        role=role,
        llm_providers={"cheap": _CountingProvider(response="")},
    )
    assert out == raw


async def test_summarize_is_noop_on_empty_payload() -> None:
    role = Role(name="Engineer", handle="eng")
    out = await summarize_episodes(
        raw_payload="", role=role, llm_providers={"cheap": _CountingProvider("x")}
    )
    assert out == ""


async def test_summarize_falls_back_to_role_llm_when_no_auxiliary() -> None:
    """If ``llm_auxiliary`` is unset, ``resolve_phase_provider`` falls
    back to ``role.llm``; summarisation still works.
    """
    role = Role(name="Engineer", handle="eng", llm="default")
    provider = _CountingProvider(response="- [done] test")
    out = await summarize_episodes(
        raw_payload='{"episodes":[]}',
        role=role,
        llm_providers={"default": provider},
    )
    assert out.startswith("- [done]")
