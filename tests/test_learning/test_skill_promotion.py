"""Tests for the cross-agent promotion pass + PromotionSynthesizer.

The promotion path does not write a unit-scope skill row.
Instead it drafts a knowledge-base page under the unit's
``Auto-Drafted Skills`` parent through the backend-neutral
``PromotionPageWriter`` seam.  These tests drive the synthesizer with
a fake writer to verify the draft flow, the markdown body, the
soft-skip paths, and the ``SkillPromoted`` event the scheduler emits.
The real writers (Confluence / Plane) are covered in
``tests/test_confluence/test_promotion.py`` and
``tests/test_plane/test_promotion.py``.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from crewlet.events.types import SkillPromoted
from crewlet.knowledge.protocol import AUTO_DRAFT_TITLE_PREFIX, AUTO_DRAFTED_PARENT
from crewlet.learning.models import SynthesizedSkill
from crewlet.learning.skill_scheduler import (
    SkillClusteringScheduler,
    _cluster_skills_by_jaccard,
    _member_handles_for_unit,
)
from crewlet.learning.skill_synthesizer import (
    PromotionPageWriter,
    PromotionResult,
    PromotionSynthesizer,
    resolve_unit_container_attr,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion
from crewlet.queue.memory import MemoryEventQueue

# ----------------------------------------------------------------------
# Stubs
# ----------------------------------------------------------------------


class _AgentStub:
    def __init__(self, handle: str, role_name: str = "Engineer") -> None:
        self.handle = handle
        self.role_name = role_name
        role = Role(name=role_name, handle=handle)
        self.definition = type("_D", (), {"role": role})()


class _AgentPoolStub:
    def __init__(self, agents: list[_AgentStub]) -> None:
        self.agents = agents


class _ScriptedProvider:
    def __init__(self, completions: list[Completion]) -> None:
        self._completions = list(completions)
        self.calls: list[dict[str, Any]] = []
        self.model = "scripted"

    async def complete(self, messages, **kwargs):
        self.calls.append({"messages": messages})
        if self._completions:
            return self._completions.pop(0)
        return Completion(content="NOOP")


class _EpisodeStoreStub:
    async def list_recent_by_outcome(self, **kwargs):  # pragma: no cover
        return []

    async def fetch_by_turn_id(self, turn_id: str):  # pragma: no cover
        return None

    async def write(self, episode):  # pragma: no cover
        pass

    async def query(self, **kwargs):  # pragma: no cover
        return []


class _FakePageWriter:
    """In-memory ``PromotionPageWriter`` recording every draft."""

    backend = "fake"

    def __init__(
        self,
        *,
        container: str = "ENG",
        page_id: str | None = "DRAFT-42",
    ) -> None:
        self._container = container
        self._page_id = page_id
        self.drafts: list[dict[str, Any]] = []

    def resolve_unit_container(self, org: Any, unit_id: str) -> str:
        return self._container

    def missing_container_hint(self) -> str:
        return "set the container on the unit"

    async def create_draft_page(
        self,
        *,
        container: str,
        parent_title: str,
        title: str,
        name: str,
        body_markdown: str,
    ) -> str | None:
        self.drafts.append(
            {
                "container": container,
                "parent_title": parent_title,
                "title": title,
                "name": name,
                "body_markdown": body_markdown,
            }
        )
        return self._page_id


class _SynthesizedStoreStub:
    def __init__(
        self,
        *,
        members: dict[str, list[SynthesizedSkill]] | None = None,
    ) -> None:
        self._members = members or {}

    async def list_for_agent_handles(
        self, agent_handles: list[str]
    ) -> list[SynthesizedSkill]:
        out: list[SynthesizedSkill] = []
        for handle in agent_handles:
            out.extend(self._members.get(handle, []))
        return out

    # Protocol members the scheduler doesn't call from _tick_unit but
    # the SynthesizedSkillStoreProtocol surface still requires.
    async def insert(self, skill):  # pragma: no cover
        return skill

    async def fetch(self, *, agent_handle: str, name: str):  # pragma: no cover
        return None

    async def list_for_agent(self, *a, **k):  # pragma: no cover
        return []

    async def count_for_agent(self, *a, **k):  # pragma: no cover
        return 0

    async def existing_tool_sequences(self, *a, **k):  # pragma: no cover
        return []

    async def update(self, *a, **k):  # pragma: no cover
        return None

    async def list_versions(self, *a, **k):  # pragma: no cover
        return []

    async def rollback_to_version(self, **k):  # pragma: no cover
        return None

    async def mark_used(self, *a, **k):  # pragma: no cover
        return None


def _mk_skill(
    *,
    agent_handle: str,
    name: str,
    tool_sequence: list[str],
) -> SynthesizedSkill:
    return SynthesizedSkill(
        id=uuid4(),
        agent_handle=agent_handle,
        name=name,
        description=f"{name} by {agent_handle}",
        content=f"body from {agent_handle}",
        tool_sequence=tool_sequence,
        version=1,
        created_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        updated_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    )


def _mk_org(*, space_key: str = "ENG", plane_project: str = "") -> Organization:
    roles = [Role(name=n, handle=n.lower()) for n in ("Alice", "Bob", "Cara", "Dan")]
    unit = OrgUnit(
        name="Eng",
        type="team",
        lead="Alice",
        roles=roles,
        confluence_space=space_key,
        plane_project=plane_project,
    )
    return Organization(name="Acme", units=[unit])


def _ship_routine_provider() -> _ScriptedProvider:
    return _ScriptedProvider(
        [
            Completion(
                content=json.dumps(
                    {
                        "name": "ship-routine",
                        "description": "Team-wide ship routine.",
                        "body": "1. Ship\n2. Update\n3. Verify",
                    }
                )
            )
        ]
    )


# ----------------------------------------------------------------------
# Pure helper coverage
# ----------------------------------------------------------------------


def test_cluster_skills_by_jaccard_groups_similar() -> None:
    s1 = _mk_skill(agent_handle="alice", name="a", tool_sequence=["x", "y", "z"])
    s2 = _mk_skill(agent_handle="bob", name="b", tool_sequence=["x", "y", "z"])
    s3 = _mk_skill(agent_handle="cara", name="c", tool_sequence=["p", "q"])
    clusters = _cluster_skills_by_jaccard([s1, s2, s3], threshold=0.6)
    sizes = sorted(len(c) for c in clusters)
    assert sizes == [1, 2]


def test_member_handles_for_unit_matches_by_role_name() -> None:
    from crewlet.learning.skill_scheduler import _Seat

    org = _mk_org()
    unit = org.units[0]
    seats = [
        _Seat(handle="alice", role=Role(name="Alice", handle="alice"), agent_id="a"),
        _Seat(handle="bob", role=Role(name="Bob", handle="bob"), agent_id="b"),
        _Seat(
            handle="nobody", role=Role(name="Outsider", handle="nobody"), agent_id="n"
        ),
    ]
    members = _member_handles_for_unit(unit, seats, org)
    assert sorted(members) == ["alice", "bob"]


def test_resolve_unit_container_attr_reads_both_identities() -> None:
    """The shared org walk both writers use: Confluence reads
    ``confluence_space``, Plane reads ``plane_project``; a missing unit
    or unset identity resolves to ""."""
    org = _mk_org(space_key="ENG", plane_project="COREPLANE")
    assert resolve_unit_container_attr(org, "Eng", "confluence_space") == "ENG"
    assert resolve_unit_container_attr(org, "Eng", "plane_project") == "COREPLANE"
    assert resolve_unit_container_attr(org, "Nope", "confluence_space") == ""
    assert resolve_unit_container_attr(None, "Eng", "confluence_space") == ""
    bare = _mk_org(space_key="")
    assert resolve_unit_container_attr(bare, "Eng", "confluence_space") == ""


def test_fake_writer_satisfies_protocol() -> None:
    writer: PromotionPageWriter = _FakePageWriter()
    assert writer.backend == "fake"


# ----------------------------------------------------------------------
# Direct PromotionSynthesizer.synthesize_promotion contract
# ----------------------------------------------------------------------


async def test_synthesize_promotion_drafts_page_via_writer() -> None:
    org = _mk_org(space_key="ENG")
    role = org.get_role("Alice")
    assert role is not None
    siblings = [
        _mk_skill(agent_handle="alice", name="alice-ship", tool_sequence=["x", "y"]),
        _mk_skill(agent_handle="bob", name="bob-ship", tool_sequence=["x", "y"]),
        _mk_skill(agent_handle="cara", name="cara-ship", tool_sequence=["x", "y"]),
    ]
    provider = _ship_routine_provider()
    writer = _FakePageWriter(container="ENG", page_id="PAGE-99")
    promotion_synth = PromotionSynthesizer(
        llm_providers={"default": provider},
        page_writer=writer,
        org=org,
    )

    result = await promotion_synth.synthesize_promotion(
        role=role,
        unit_id="Eng",
        source_skills=siblings,
    )

    assert isinstance(result, PromotionResult)
    assert result.skill_name == "ship-routine"
    assert result.page_id == "PAGE-99"
    assert result.page_title == f"{AUTO_DRAFT_TITLE_PREFIX}ship-routine"
    assert result.container_key == "ENG"

    # One draft, addressed at the unit container under the canonical
    # auto-drafts parent.
    assert len(writer.drafts) == 1
    draft = writer.drafts[0]
    assert draft["container"] == "ENG"
    assert draft["parent_title"] == AUTO_DRAFTED_PARENT
    assert draft["title"] == f"{AUTO_DRAFT_TITLE_PREFIX}ship-routine"

    # Markdown body: bold description, LLM body verbatim, tool bullet
    # list, provenance listing every contributing sibling.
    body = draft["body_markdown"]
    assert "**Team-wide ship routine.**" in body
    assert "1. Ship\n2. Update\n3. Verify" in body
    assert "**Common tool sequence:**" in body
    assert "- `x`" in body
    assert "- `y`" in body
    assert "**Provenance (auto-drafted):**" in body
    for sibling in siblings:
        assert sibling.agent_handle in body
        assert sibling.name in body


async def test_synthesize_promotion_noop_writes_nothing() -> None:
    org = _mk_org()
    role = org.get_role("Alice")
    assert role is not None
    siblings = [
        _mk_skill(agent_handle="alice", name="a", tool_sequence=["x", "y"]),
        _mk_skill(agent_handle="bob", name="b", tool_sequence=["x", "y"]),
    ]
    provider = _ScriptedProvider([Completion(content="NOOP")])
    writer = _FakePageWriter()
    promotion_synth = PromotionSynthesizer(
        llm_providers={"default": provider},
        page_writer=writer,
        org=org,
    )

    result = await promotion_synth.synthesize_promotion(
        role=role,
        unit_id="Eng",
        source_skills=siblings,
    )

    assert result is None
    assert writer.drafts == []


async def test_synthesize_promotion_skips_when_no_container() -> None:
    # Writer resolves no container for the unit — promotion soft-skips
    # before ever calling the LLM.
    org = _mk_org(space_key="")
    role = org.get_role("Alice")
    assert role is not None
    siblings = [
        _mk_skill(agent_handle="alice", name="a", tool_sequence=["x", "y"]),
        _mk_skill(agent_handle="bob", name="b", tool_sequence=["x", "y"]),
    ]
    provider = _ScriptedProvider([])
    writer = _FakePageWriter(container="")
    promotion_synth = PromotionSynthesizer(
        llm_providers={"default": provider},
        page_writer=writer,
        org=org,
    )

    result = await promotion_synth.synthesize_promotion(
        role=role,
        unit_id="Eng",
        source_skills=siblings,
    )

    assert result is None
    assert provider.calls == []  # never reached the LLM
    assert writer.drafts == []


async def test_synthesize_promotion_handles_writer_failure() -> None:
    """A writer returning None (backend down, 4xx, ...) → no result →
    the scheduler retries on its next tick."""
    org = _mk_org()
    role = org.get_role("Alice")
    assert role is not None
    siblings = [
        _mk_skill(agent_handle="alice", name="a", tool_sequence=["x", "y"]),
        _mk_skill(agent_handle="bob", name="b", tool_sequence=["x", "y"]),
    ]
    provider = _ScriptedProvider(
        [
            Completion(
                content=json.dumps(
                    {
                        "name": "ship-routine",
                        "description": "desc",
                        "body": "step",
                    }
                )
            )
        ]
    )
    writer = _FakePageWriter(page_id=None)
    promotion_synth = PromotionSynthesizer(
        llm_providers={"default": provider},
        page_writer=writer,
        org=org,
    )

    result = await promotion_synth.synthesize_promotion(
        role=role,
        unit_id="Eng",
        source_skills=siblings,
    )

    assert result is None
    assert len(writer.drafts) == 1  # the write was attempted


# ----------------------------------------------------------------------
# End-to-end scheduler tick
# ----------------------------------------------------------------------


async def test_tick_once_promotes_cluster_shared_across_siblings() -> None:
    members = {
        "alice": [
            _mk_skill(
                agent_handle="alice", name="alice-ship", tool_sequence=["x", "y", "z"]
            )
        ],
        "bob": [
            _mk_skill(
                agent_handle="bob", name="bob-ship", tool_sequence=["x", "y", "z"]
            )
        ],
        "cara": [
            _mk_skill(
                agent_handle="cara", name="cara-ship", tool_sequence=["x", "y", "z"]
            )
        ],
    }
    store = _SynthesizedStoreStub(members=members)
    provider = _ship_routine_provider()
    org = _mk_org()
    writer = _FakePageWriter(container="ENG", page_id="PAGE-1")
    promotion_synth = PromotionSynthesizer(
        llm_providers={"default": provider},
        page_writer=writer,
        org=org,
    )
    bus = MemoryEventQueue()
    await bus.start()
    try:
        scheduler = SkillClusteringScheduler(
            synthesizer=type("_NoSynth", (), {})(),
            episode_store=_EpisodeStoreStub(),
            agent_pool=_AgentPoolStub(
                [
                    _AgentStub("alice", "Alice"),
                    _AgentStub("bob", "Bob"),
                    _AgentStub("cara", "Cara"),
                ]
            ),
            organization=org,
            promotion_synthesizer=promotion_synth,
            synthesized_skill_store=store,
            promotion_enabled=True,
            promotion_min_sibling_count=3,
            promotion_jaccard_threshold=0.6,
            event_queue=bus,
        )

        made = await scheduler.tick_once()
    finally:
        await bus.stop()

    assert made == 1
    # Backend side-effect: one draft under the auto-drafts parent.
    assert len(writer.drafts) == 1
    assert writer.drafts[0]["container"] == "ENG"
    # SkillPromoted event published with the container field.
    promoted_events = [e for e in bus.history if isinstance(e, SkillPromoted)]
    assert len(promoted_events) == 1
    event = promoted_events[0]
    assert event.skill_name == "ship-routine"
    assert event.page_id == "PAGE-1"
    assert event.container_key == "ENG"
    assert event.unit_id == "Eng"
    assert event.distinct_agents == 3
    assert event.sibling_count == 3
    assert "knowledge base" in event.summary


async def test_tick_once_skips_when_cluster_has_too_few_distinct_agents() -> None:
    # Alice alone has 3 similar skills -- distinct agents = 1.
    members = {
        "alice": [
            _mk_skill(agent_handle="alice", name="s1", tool_sequence=["x", "y"]),
            _mk_skill(agent_handle="alice", name="s2", tool_sequence=["x", "y"]),
            _mk_skill(agent_handle="alice", name="s3", tool_sequence=["x", "y"]),
        ],
        "bob": [],
        "cara": [],
    }
    store = _SynthesizedStoreStub(members=members)
    provider = _ScriptedProvider([])
    org = _mk_org()
    writer = _FakePageWriter()
    promotion_synth = PromotionSynthesizer(
        llm_providers={"default": provider},
        page_writer=writer,
        org=org,
    )
    scheduler = SkillClusteringScheduler(
        synthesizer=type("_NoSynth", (), {})(),
        episode_store=_EpisodeStoreStub(),
        agent_pool=_AgentPoolStub(
            [
                _AgentStub("alice", "Alice"),
                _AgentStub("bob", "Bob"),
                _AgentStub("cara", "Cara"),
            ]
        ),
        organization=org,
        promotion_synthesizer=promotion_synth,
        synthesized_skill_store=store,
        promotion_enabled=True,
        promotion_min_sibling_count=3,
    )

    made = await scheduler.tick_once()

    assert made == 0
    assert provider.calls == []
    assert writer.drafts == []


async def test_promotion_disabled_skips_pass_entirely() -> None:
    members = {
        "alice": [_mk_skill(agent_handle="alice", name="a", tool_sequence=["x", "y"])],
        "bob": [_mk_skill(agent_handle="bob", name="b", tool_sequence=["x", "y"])],
        "cara": [_mk_skill(agent_handle="cara", name="c", tool_sequence=["x", "y"])],
    }
    store = _SynthesizedStoreStub(members=members)
    provider = _ScriptedProvider([])
    org = _mk_org()
    writer = _FakePageWriter()
    promotion_synth = PromotionSynthesizer(
        llm_providers={"default": provider},
        page_writer=writer,
        org=org,
    )
    scheduler = SkillClusteringScheduler(
        synthesizer=type("_NoSynth", (), {})(),
        episode_store=_EpisodeStoreStub(),
        agent_pool=_AgentPoolStub(
            [
                _AgentStub("alice", "Alice"),
                _AgentStub("bob", "Bob"),
                _AgentStub("cara", "Cara"),
            ]
        ),
        organization=org,
        promotion_synthesizer=promotion_synth,
        synthesized_skill_store=store,
        promotion_enabled=False,
        promotion_min_sibling_count=3,
    )

    made = await scheduler.tick_once()

    assert made == 0
    assert provider.calls == []
    assert writer.drafts == []
