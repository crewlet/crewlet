"""Pydantic models for the agent-learning subsystem."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any, Literal
from uuid import UUID, uuid4

from pydantic import BaseModel, ConfigDict, Field, field_validator

ReviewOutcomeStr = Literal["done", "self_iterate", "failed"]
EpisodeKindStr = Literal["raw", "compacted"]


def _ensure_utc(value: datetime | None) -> datetime | None:
    """Coerce a datetime to UTC-aware.

    Timestamps are UTC-aware by convention, but a value can still
    arrive naive (a caller built it without ``tzinfo``, or a row was
    written by hand).  A naive value is *assumed*
    UTC and stamped as such.  This keeps cross-row arithmetic --
    ``min()`` / ``max()`` over a cluster's timestamps in the lifecycle
    worker -- from raising ``TypeError`` on mixed naive/aware inputs.
    """
    if value is None:
        return None
    if value.tzinfo is None:
        return value.replace(tzinfo=UTC)
    return value


class Episode(BaseModel):
    """One agent-learning row in the ``episodes`` hypertable.

    Two physical shapes share the same model + table (distinguished by
    ``kind``):

    - ``kind="raw"``: the original behaviour -- one row per completed
      turn, written by the TurnEngine.  Per-turn fields
      (``task_summary``, ``plan_summary``, ``tool_sequence``,
      ``review_outcome``) carry the full single-turn detail.
    - ``kind="compacted"``: a cluster summary written by the
      :class:`~crewlet.learning.episode_lifecycle.EpisodeLifecycleWorker`
      after compaction.  ``count`` is the number of original raw
      episodes the row represents; ``common_task_pattern``,
      ``common_outcome``, ``success_rate``, ``subjects_involved``,
      ``notable_patterns`` carry the aggregate signal LLM-summarised
      from the cluster.  ``exemplar_turn_ids`` lists 2-3 originals
      that survive as raw rows alongside the compacted entry for
      drill-down.

    See ``docs/concepts/agent-learning.md`` -- the lifecycle section.
    """

    model_config = ConfigDict(extra="forbid")

    id: UUID = Field(default_factory=uuid4)
    agent_handle: str
    agent_role: str
    task_id: str = ""
    turn_id: UUID
    started_at: datetime
    ended_at: datetime
    plan_summary: str
    task_summary: str
    tool_sequence: list[str] = Field(default_factory=list)
    skills_used: list[str] = Field(default_factory=list)
    review_outcome: ReviewOutcomeStr
    duration_ms: int

    work_key: str = ""
    """Identity of the unit of work this episode records — see
    :mod:`crewlet.work_key`.

    NOT ``turn_id``: two nodes that both complete a turn for one trigger
    mint two turn ids, so a key taken from one records the duplicate
    instead of collapsing it.  Derived from the trigger events instead,
    which are the same across a re-run.

    ``""`` for a turn with no ledgerable trigger (a scheduled fire, a
    sub-agent, a sandbox resume).  Those rows are unconstrained by the
    partial unique index, which is correct: they have no cross-node
    duplicate to collapse."""

    # --- Lifecycle / compaction fields (defaults preserve raw shape) ---

    kind: EpisodeKindStr = "raw"
    """``"raw"`` for one-turn rows, ``"compacted"`` for cluster summaries."""

    count: int = 1
    """How many original episodes this row represents.  Always 1 for raw."""

    exemplar_turn_ids: list[UUID] = Field(default_factory=list)
    """For compacted rows: 2-3 original turn ids preserved as raw rows for
    drill-down.  Empty for raw rows."""

    consolidated_into_skill_id: UUID | None = None
    """Set by ``SkillSynthesizer`` when this raw episode contributed to a
    synthesized skill.  The lifecycle worker drops these episodes after
    a configurable grace -- the skill carries the learning forward."""

    common_task_pattern: str = ""
    """Compacted rows: LLM-summarised one-line pattern across the cluster.
    Empty for raw rows (their ``task_summary`` is the per-turn detail)."""

    common_outcome: str = ""
    """Compacted rows: most common ``review_outcome`` in the cluster."""

    success_rate: float = 0.0
    """Compacted rows: fraction of cluster turns that ended ``done``."""

    subjects_involved: list[str] = Field(default_factory=list)
    """Compacted rows: distinct counterparties observed across the cluster."""

    notable_patterns: str = ""
    """Compacted rows: LLM-noted variations / edge cases worth surfacing."""

    @field_validator("started_at", "ended_at", mode="after")
    @classmethod
    def _utc_timestamps(cls, value: datetime) -> datetime:
        return _ensure_utc(value)  # type: ignore[return-value]

    @property
    def embeddable_text(self) -> str:
        """Text used to generate the row's embedding.

        For raw rows: concatenates task + plan summary so similarity
        search surfaces episodes with similar *goals* and *approaches*.
        For compacted rows: uses ``common_task_pattern`` (the LLM's
        cluster-level summary) so vector search returns the right
        aggregate when the planner asks about a similar topic.
        """
        if self.kind == "compacted" and self.common_task_pattern:
            return self.common_task_pattern
        if self.task_summary and self.plan_summary:
            return f"{self.task_summary} | {self.plan_summary}"
        return self.task_summary or self.plan_summary


class CounterpartyProfile(BaseModel):
    """One agent's observed model of one subject.

    Stored in the ``counterparty_profiles`` table keyed by
    ``(observer_handle, subject_handle, subject_external_id,
    subject_platform)``.  Traits is a free-form JSONB blob whose keys
    are LLM-invented (e.g. ``communication_style``, ``interests``,
    ``sensitivities``, ``decisions``).

    Either ``subject_handle`` or ``subject_external_id`` is set: a
    resolved Crewlet handle for known agents, a transport-scoped
    external identifier for unmapped humans.  ``subject_name`` is a
    display name (best-effort, may be empty).
    """

    model_config = ConfigDict(extra="forbid")

    observer_handle: str
    subject_handle: str = ""
    subject_external_id: str = ""
    subject_platform: str = ""
    subject_name: str = ""
    traits: dict[str, Any] = Field(default_factory=dict)
    first_seen_at: datetime
    last_updated_at: datetime
    last_corroborated_at: datetime | None = None
    interaction_count: int = 0

    @field_validator(
        "first_seen_at", "last_updated_at", "last_corroborated_at", mode="after"
    )
    @classmethod
    def _utc_timestamps(cls, value: datetime | None) -> datetime | None:
        return _ensure_utc(value)

    @property
    def is_resolved_agent(self) -> bool:
        """True when the subject is a known Crewlet agent handle."""
        return bool(self.subject_handle)

    @property
    def subject_label(self) -> str:
        """Human-readable subject identifier for prompts and logs."""
        if self.subject_name:
            return self.subject_name
        if self.subject_handle:
            return self.subject_handle
        if self.subject_external_id:
            return f"{self.subject_platform}:{self.subject_external_id}"
        return "(unknown)"

    def render_observed_traits(self) -> str:
        """Render the "Observed by you:" + traits + provenance block.

        Used by both the Plan-phase prefetch (which prepends a
        ``Subject:`` / ``Platform:`` header) and ``lookup_colleague`` (which
        appends the block to its agent-info output).  Centralising the
        format here keeps the two call sites from drifting on trait
        rendering or the provenance footer.
        """
        traits = dict(self.traits or {})
        if traits:
            lines: list[str] = ["Observed by you:"]
            for key, value in traits.items():
                if isinstance(value, (list, tuple)):
                    rendered = ", ".join(str(v) for v in value)
                else:
                    rendered = str(value)
                lines.append(f"  - {key}: {rendered}")
        else:
            lines = ["Observed by you: (no traits yet)"]
        lines.append(
            f"(interactions: {self.interaction_count}, "
            f"last updated: {self.last_updated_at.isoformat()})"
        )
        return "\n".join(lines)


RefinementKind = Literal[
    "observed_in_practice",
    "counter_example",
    "refine_skill_tool",
    "replace",
    "promotion",
    "rollback",
]


class SynthesizedSkill(BaseModel):
    """An auto-drafted reusable skill, agent-scope only.

    Stored in the ``synthesized_skills`` table, keyed by
    ``(agent_handle, name)``.  Drafted by
    :class:`~crewlet.learning.skill_synthesizer.SkillSynthesizer`
    from one or more successful turns of a single agent; visible
    only to that agent.

    The table holds agent-scope rows only.  Cross-agent promotion
    (the ``PromotionSynthesizer`` pass) drafts a knowledge-base page
    under the unit's ``Auto-Drafted Skills`` parent instead, which a
    unit lead reviews and publishes through the active backend's
    normal tools.  Once published the page reaches all members
    through the backend's query-time search behind the ``## Relevant
    knowledge`` prefetch.

    Shape matches the on-disk ``SKILL.md`` format used by the
    Skills System: ``frontmatter`` is the YAML header dict,
    ``content`` is the Markdown body.  Both are reconstructed into
    ``SKILL.md`` text when ``use_skill`` loads the skill.
    """

    model_config = ConfigDict(extra="forbid")

    id: UUID = Field(default_factory=uuid4)
    agent_handle: str
    """Owning agent.  Always set."""
    name: str
    description: str
    content: str
    frontmatter: dict[str, Any] = Field(default_factory=dict)
    tool_sequence: list[str] = Field(default_factory=list)
    source_episode_ids: list[UUID] = Field(default_factory=list)
    version: int = 1
    created_at: datetime
    updated_at: datetime
    use_count: int = 0
    """Times this skill has been loaded via ``use_skill``.  Bumped by
    :meth:`~crewlet.learning.synthesized_skill_store.SynthesizedSkillStore.mark_used`
    on each successful resolution.  ``0`` means the skill exists but
    has never paid its way -- the Berlot-Attwell warning signal.
    Surfaced in the ``learning_health`` view."""
    last_used_at: datetime | None = None
    """Wall-clock of the most recent ``use_skill`` resolution; ``None``
    until the skill is first loaded."""

    state: str = "active"
    """Curator state machine: ``active`` | ``stale`` | ``archived``.
    The Plan-phase prefetch hides archived rows; ``_use_skill`` refuses
    to load them. Stale skills surface with a ``[stale]`` marker so
    the agent knows they're aging.
    """
    pinned: bool = False
    """When True, the curator never transitions this skill. Operators
    set this manually for skills they want preserved regardless of
    usage; no auto-promotion to pinned."""
    stale_at: datetime | None = None
    """When the skill last transitioned to ``stale``. ``None`` for
    skills that have never been stale."""
    archived_at: datetime | None = None
    """When the skill was archived. ``None`` for active/stale skills."""

    @field_validator(
        "created_at",
        "updated_at",
        "last_used_at",
        "stale_at",
        "archived_at",
        mode="after",
    )
    @classmethod
    def _utc_timestamps(cls, value: datetime | None) -> datetime | None:
        return _ensure_utc(value)

    def render_skill_md(self) -> str:
        """Render the synthesized skill back into ``SKILL.md`` text.

        Level-2 payload for the ``use_skill`` tool.  Writes a minimal
        YAML frontmatter block (name + description + any stored
        frontmatter keys) followed by the Markdown body.
        """
        import json as _json

        header: dict[str, Any] = {
            "name": self.name,
            "description": self.description,
        }
        # Extra frontmatter keys (e.g. a category) override the defaults.
        for key, value in self.frontmatter.items():
            header[key] = value
        # YAML is a superset of JSON for scalar + flat dicts; using
        # JSON-shaped output keeps the render deterministic and avoids
        # pulling PyYAML in.  ``parse_skill_frontmatter`` in the
        # Skills System tolerates JSON-style YAML for these keys.
        lines = ["---"]
        for key, value in header.items():
            lines.append(f"{key}: {_json.dumps(value)}")
        lines.append("---")
        lines.append("")
        lines.append(self.content.strip())
        return "\n".join(lines)


class SynthesizedSkillVersion(BaseModel):
    """One archived version of a synthesized skill.

    Written to ``synthesized_skill_versions`` by
    :meth:`~crewlet.learning.synthesized_skill_store.SynthesizedSkillStore.update`
    before the main row is mutated.  Keyed by ``id``; references the
    live row via ``skill_id``.
    """

    model_config = ConfigDict(extra="forbid")

    id: UUID = Field(default_factory=uuid4)
    skill_id: UUID
    agent_handle: str
    name: str
    description: str
    content: str
    frontmatter: dict[str, Any] = Field(default_factory=dict)
    tool_sequence: list[str] = Field(default_factory=list)
    source_episode_ids: list[UUID] = Field(default_factory=list)
    version: int
    refinement_kind: RefinementKind
    refinement_note: str = ""
    archived_at: datetime

    @field_validator("archived_at", mode="after")
    @classmethod
    def _utc_timestamps(cls, value: datetime) -> datetime:
        return _ensure_utc(value)  # type: ignore[return-value]
