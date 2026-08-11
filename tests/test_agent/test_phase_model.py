"""Tests for per-phase LLM provider resolution."""

from __future__ import annotations

import pytest

from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.org.models import Role


class _StubProvider:
    """Marker stand-in for LLMProvider. Identity-only for assertions."""

    def __init__(self, label: str) -> None:
        self.label = label


def test_raises_when_no_providers():
    role = Role(name="X")
    with pytest.raises(RuntimeError, match="No LLM providers"):
        resolve_phase_provider(role, "plan", {})


def test_per_phase_key_wins_when_present():
    """``role.llm_<phase>`` takes priority over ``role.llm``."""
    role = Role(name="X", llm="fallback", llm_plan="smart")
    providers = {"smart": _StubProvider("smart"), "fallback": _StubProvider("fallback")}
    key, provider = resolve_phase_provider(role, "plan", providers)
    assert key == "smart"
    assert provider.label == "smart"


def test_falls_back_to_role_llm_when_phase_key_missing():
    """If no per-phase key is set, ``role.llm`` is used."""
    role = Role(name="X", llm="fallback")
    providers = {"fallback": _StubProvider("fallback"), "default": _StubProvider("d")}
    key, provider = resolve_phase_provider(role, "execute", providers)
    assert key == "fallback"
    assert provider.label == "fallback"


def test_falls_back_to_default_key_when_role_key_missing():
    """If neither per-phase nor ``role.llm`` match, ``default`` is used."""
    role = Role(name="X", llm="no-such-key")
    providers = {"default": _StubProvider("default"), "other": _StubProvider("other")}
    key, provider = resolve_phase_provider(role, "review", providers)
    assert key == "default"
    assert provider.label == "default"


def test_falls_back_to_first_when_no_default():
    """When neither the requested key nor ``default`` exist, use the first."""
    role = Role(name="X", llm="missing")
    providers = {"only": _StubProvider("only")}
    key, provider = resolve_phase_provider(role, "subagent", providers)
    assert key == "only"
    assert provider.label == "only"


def test_each_phase_key_is_independent():
    """The four per-phase keys resolve independently."""
    role = Role(
        name="X",
        llm="base",
        llm_plan="p",
        llm_execute="e",
        llm_review="r",
        llm_subagent="s",
    )
    providers = {
        "base": _StubProvider("base"),
        "p": _StubProvider("p"),
        "e": _StubProvider("e"),
        "r": _StubProvider("r"),
        "s": _StubProvider("s"),
    }
    assert resolve_phase_provider(role, "plan", providers)[0] == "p"
    assert resolve_phase_provider(role, "execute", providers)[0] == "e"
    assert resolve_phase_provider(role, "review", providers)[0] == "r"
    assert resolve_phase_provider(role, "subagent", providers)[0] == "s"


def test_phase_key_missing_from_providers_falls_back_to_role_llm():
    """A set per-phase key that doesn't exist in providers falls back."""
    role = Role(name="X", llm="base", llm_plan="not-there")
    providers = {"base": _StubProvider("base")}
    key, provider = resolve_phase_provider(role, "plan", providers)
    assert key == "base"
    assert provider.label == "base"
