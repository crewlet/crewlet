"""Tests for :class:`CounterpartyProfiler`."""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any

from crewlet.learning.counterparty_profiler import (
    _PROFILER_SYSTEM_PROMPT,
    NOOP_SENTINEL,
    CounterpartyProfiler,
    _sanitise_patch,
)
from crewlet.learning.interaction import CanonicalIdentity, InboundInteraction
from crewlet.learning.models import CounterpartyProfile
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import Completion


def _interaction(
    *,
    handle: str = "",
    external_id: str = "",
    platform: str = "",
    display_name: str = "",
    body: str = "",
) -> InboundInteraction:
    return InboundInteraction(
        sender=CanonicalIdentity(
            handle=handle,
            external_id=external_id,
            platform=platform,
            display_name=display_name,
        ),
        body=body,
    )


class _ScriptedProvider:
    def __init__(self, completions: list[Completion]) -> None:
        self._completions = list(completions)
        self.calls: list[dict[str, Any]] = []
        self.model = "scripted"

    async def complete(
        self,
        messages,
        tools=None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice=None,
    ):
        self.calls.append(
            {"messages": messages, "temperature": temperature, "max_tokens": max_tokens}
        )
        if self._completions:
            return self._completions.pop(0)
        return Completion(content="NOOP")


class _StoreStub:
    def __init__(self) -> None:
        self.upserts: list[dict[str, Any]] = []

    async def upsert(self, **kwargs) -> CounterpartyProfile:
        self.upserts.append(kwargs)
        return CounterpartyProfile(
            observer_handle=kwargs["observer_handle"],
            subject_handle=kwargs.get("subject_handle", ""),
            subject_external_id=kwargs.get("subject_external_id", ""),
            subject_platform=kwargs.get("subject_platform", ""),
            subject_name=kwargs.get("subject_name", ""),
            traits=kwargs.get("traits_patch") or {},
            first_seen_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
            last_updated_at=datetime(2026, 4, 23, 9, 5, tzinfo=UTC),
            interaction_count=1,
        )

    async def fetch(self, **kwargs):  # pragma: no cover - unused here
        return None

    async def fetch_for_senders(self, *args, **kwargs):  # pragma: no cover
        return None


def _mk_role() -> Role:
    return Role(name="Engineer", handle="eng", llm="cheap", llm_auxiliary="cheap")


async def test_noop_still_increments_interactions_with_empty_patch() -> None:
    provider = _ScriptedProvider([Completion(content="NOOP")])
    store = _StoreStub()
    profiler = CounterpartyProfiler(llm_providers={"cheap": provider}, store=store)
    await profiler.update_from_turn(
        role=_mk_role(),
        observer_handle="alice",
        interaction=_interaction(handle="bob", display_name="Bob", body="hi"),
        plan_summary="respond",
        review_outcome="done",
        turn_id="t1",
    )
    assert len(store.upserts) == 1
    assert store.upserts[0]["traits_patch"] == {}
    assert store.upserts[0]["increment_interactions"] is True


async def test_valid_json_patch_forwarded_to_store() -> None:
    provider = _ScriptedProvider(
        [
            Completion(
                content=json.dumps(
                    {
                        "communication_style": "terse",
                        "interests": ["deploy automation"],
                    }
                )
            )
        ]
    )
    store = _StoreStub()
    profiler = CounterpartyProfiler(llm_providers={"cheap": provider}, store=store)
    await profiler.update_from_turn(
        role=_mk_role(),
        observer_handle="alice",
        interaction=_interaction(handle="bob", display_name="Bob", body="ship it"),
        plan_summary="ack",
        review_outcome="done",
        turn_id="t2",
    )
    assert store.upserts[0]["traits_patch"] == {
        "communication_style": "terse",
        "interests": ["deploy automation"],
    }
    # System prompt enforces the observation-only rule.
    sys_msg = provider.calls[0]["messages"][0]
    assert sys_msg.role == "system"
    assert "observations of the SUBJECT" in _PROFILER_SYSTEM_PROMPT
    # The prompt explicitly steers toward capturing explicit user
    # directives (the "always greet me with X" case) — without this,
    # the LLM tends to NOOP on preference statements.
    assert "explicit subject preferences" in _PROFILER_SYSTEM_PROMPT
    assert "preferred_greeting" in _PROFILER_SYSTEM_PROMPT
    assert NOOP_SENTINEL in _PROFILER_SYSTEM_PROMPT


async def test_missing_subject_identifier_skips_everything() -> None:
    provider = _ScriptedProvider([Completion(content="NOOP")])
    store = _StoreStub()
    profiler = CounterpartyProfiler(llm_providers={"cheap": provider}, store=store)
    await profiler.update_from_turn(
        role=_mk_role(),
        observer_handle="alice",
        interaction=_interaction(body="x"),
        plan_summary="x",
        review_outcome="done",
        turn_id="t3",
    )
    assert provider.calls == []
    assert store.upserts == []


async def test_llm_exception_swallowed_and_interactions_still_bumped() -> None:
    class _Boom:
        model = "boom"

        async def complete(self, *args, **kwargs):
            raise RuntimeError("provider down")

    store = _StoreStub()
    profiler = CounterpartyProfiler(llm_providers={"cheap": _Boom()}, store=store)
    await profiler.update_from_turn(
        role=_mk_role(),
        observer_handle="alice",
        interaction=_interaction(handle="bob", display_name="Bob", body="x"),
        plan_summary="x",
        review_outcome="done",
        turn_id="t4",
    )
    assert len(store.upserts) == 1
    assert store.upserts[0]["traits_patch"] == {}


async def test_unparseable_response_bumps_interactions_but_no_traits() -> None:
    provider = _ScriptedProvider(
        [Completion(content="I would observe that they're nice")]
    )
    store = _StoreStub()
    profiler = CounterpartyProfiler(llm_providers={"cheap": provider}, store=store)
    await profiler.update_from_turn(
        role=_mk_role(),
        observer_handle="alice",
        interaction=_interaction(handle="bob", display_name="Bob", body="x"),
        plan_summary="x",
        review_outcome="done",
        turn_id="t5",
    )
    assert len(store.upserts) == 1
    assert store.upserts[0]["traits_patch"] == {}


async def test_store_failure_is_swallowed() -> None:
    class _BoomStore:
        async def upsert(self, **kwargs):
            raise RuntimeError("db down")

        async def fetch(self, **kwargs):  # pragma: no cover
            return None

        async def fetch_for_senders(self, *a, **k):  # pragma: no cover
            return None

    provider = _ScriptedProvider([Completion(content="NOOP")])
    profiler = CounterpartyProfiler(
        llm_providers={"cheap": provider}, store=_BoomStore()
    )
    # Must not raise.
    await profiler.update_from_turn(
        role=_mk_role(),
        observer_handle="alice",
        interaction=_interaction(handle="bob", display_name="Bob", body="x"),
        plan_summary="x",
        review_outcome="done",
        turn_id="t6",
    )


def test_sanitise_patch_drops_empty_values_and_caps_key_length() -> None:
    patch = {
        "communication_style": "terse",
        "": "bad",
        ("not", "str"): "bad",
        "empty": "",
        "null": None,
        ("x" * 65): "too long key",
        "interests": ["deploy", ""],
    }
    cleaned = _sanitise_patch(patch)
    assert "communication_style" in cleaned
    assert "interests" in cleaned
    assert cleaned["interests"] == ["deploy"]
    # No empty/oversized/malformed keys survive.
    assert all(k and isinstance(k, str) and len(k) <= 64 for k in cleaned)


def test_sanitise_patch_caps_total_keys() -> None:
    patch = {f"k{i}": f"v{i}" for i in range(30)}
    cleaned = _sanitise_patch(patch)
    assert len(cleaned) == 16


def test_sanitise_patch_truncates_long_string_values() -> None:
    patch = {"style": "x" * 500}
    cleaned = _sanitise_patch(patch)
    assert len(cleaned["style"]) == 200


def test_sanitise_patch_collapses_whitespace_in_string_values() -> None:
    patch = {"style": "writes\nin\tmany\n\nlines"}
    cleaned = _sanitise_patch(patch)
    assert cleaned["style"] == "writes in many lines"
    assert "\n" not in cleaned["style"]
    assert "\t" not in cleaned["style"]


def test_sanitise_patch_truncates_list_items_too() -> None:
    patch = {"interests": ["deploys", "y" * 500]}
    cleaned = _sanitise_patch(patch)
    assert cleaned["interests"][0] == "deploys"
    assert len(cleaned["interests"][1]) == 200


def test_sanitise_patch_recursively_sanitises_nested_dicts() -> None:
    patch = {
        "preferences": {
            "tone": "formal\nwith\tnewlines",
            "verbosity": "z" * 500,
        }
    }
    cleaned = _sanitise_patch(patch)
    nested = cleaned["preferences"]
    assert nested["tone"] == "formal with newlines"
    assert "\n" not in nested["tone"]
    assert len(nested["verbosity"]) == 200


def test_sanitise_patch_drops_dicts_beyond_max_depth() -> None:
    patch = {"a": {"b": {"c": {"d": "too deep"}}}}
    cleaned = _sanitise_patch(patch)
    # Recursion bottoms out at depth 2 — anything deeper is dropped,
    # which cascades up and removes the top-level entry entirely once
    # there's nothing left to keep.
    assert "a" not in cleaned
