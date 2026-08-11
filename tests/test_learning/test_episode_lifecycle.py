"""Unit tests for :class:`EpisodeLifecycleWorker` + helpers.

Covers:
- Action ordering (drops first, then compaction, then optional eviction).
- Per-agent semaphore: concurrent CompactionRequested for the same
  agent serialise (no double-fan-out).
- Compaction LLM happy path: cluster summarised, compacted row inserted,
  originals deleted (except exemplars).
- LLM failure / unparseable output: cluster left intact, no row written.
- Below-min-cluster-size candidates: nothing compacted.
- Pure helpers: clustering, prompt rendering, JSON extraction,
  compacted-episode construction, success_rate computed from data
  (not from LLM-asserted value).
"""

from __future__ import annotations

import asyncio
import json
from datetime import UTC, datetime, timedelta
from typing import Any
from uuid import uuid4

from crewlet.events.types import CompactionCompleted, CompactionRequested
from crewlet.learning.episode_lifecycle import (
    EpisodeLifecycleWorker,
    _build_compacted_episode,
    _build_compactor_user_prompt,
    _cluster_by_jaccard,
    _extract_json_object,
    _jaccard,
)
from crewlet.learning.models import Episode
from crewlet.org.models import Organization, Role
from crewlet.providers.llm.protocol import Completion


def _ep(
    *,
    tool_sequence: list[str],
    outcome: str = "done",
    minutes_ago: int = 60,
    agent_handle: str = "alice",
    task: str = "do a thing",
    plan: str = "do it",
) -> Episode:
    now = datetime.now(UTC)
    ended = now - timedelta(minutes=minutes_ago)
    started = ended - timedelta(seconds=30)
    return Episode(
        id=uuid4(),
        agent_handle=agent_handle,
        agent_role="Engineer",
        turn_id=uuid4(),
        started_at=started,
        ended_at=ended,
        plan_summary=plan,
        task_summary=task,
        tool_sequence=tool_sequence,
        review_outcome=outcome,  # type: ignore[arg-type]
        duration_ms=30_000,
    )


# ---------------------------------------------------------------------
# Pure helpers (no worker / no LLM)
# ---------------------------------------------------------------------


def test_jaccard_basic() -> None:
    assert _jaccard(["a", "b"], ["a", "b"]) == 1.0
    assert _jaccard(["a", "b"], ["c", "d"]) == 0.0
    assert _jaccard(["a", "b"], ["a"]) == 0.5
    assert _jaccard([], ["a"]) == 0.0
    assert _jaccard(["a"], []) == 0.0


def test_cluster_by_jaccard_groups_similar_episodes() -> None:
    """Three pairs of similar episodes -> three clusters of two."""
    eps = [
        _ep(tool_sequence=["slack_message", "slack_reactions_add"]),
        _ep(tool_sequence=["slack_message", "slack_reactions_add"]),
        _ep(tool_sequence=["jira_comment", "jira_assign"]),
        _ep(tool_sequence=["jira_comment", "jira_assign"]),
        _ep(tool_sequence=["github_request_review"]),
        _ep(tool_sequence=["github_request_review"]),
    ]
    clusters = _cluster_by_jaccard(eps, threshold=0.7)
    assert len(clusters) == 3
    assert all(len(c) == 2 for c in clusters)


def test_cluster_skips_episodes_with_empty_tool_sequence() -> None:
    """An episode with no tool calls has no signal to cluster on; drop it."""
    eps = [
        _ep(tool_sequence=[]),
        _ep(tool_sequence=["slack_message"]),
        _ep(tool_sequence=["slack_message"]),
    ]
    clusters = _cluster_by_jaccard(eps, threshold=0.6)
    # The empty-tool episode is excluded; the two slack ones cluster.
    assert len(clusters) == 1
    assert len(clusters[0]) == 2


def test_extract_json_object_handles_prose_wrapping() -> None:
    text = 'Here you go: {"common_task_pattern": "x"} done.'
    obj = _extract_json_object(text)
    assert obj == {"common_task_pattern": "x"}


def test_extract_json_object_returns_none_on_garbage() -> None:
    assert _extract_json_object("not json") is None
    assert _extract_json_object("") is None


def test_compactor_user_prompt_clamps_per_turn_detail() -> None:
    """Per-turn task/plan strings are truncated so a 100-turn cluster
    doesn't blow the model's context."""
    big = "x" * 5000
    eps = [_ep(tool_sequence=["a"], task=big, plan=big) for _ in range(3)]
    rendered = _build_compactor_user_prompt(eps)
    # Each turn's detail capped well below the 5000-char inputs.
    assert "..." in rendered
    # Three turns appear with their numbered prefixes.
    assert "0." in rendered
    assert "1." in rendered
    assert "2." in rendered
    # Header reflects the cluster size.
    assert "Cluster of 3" in rendered


def test_build_compacted_episode_uses_actual_success_rate() -> None:
    """``success_rate`` is computed from the cluster's actual outcomes
    -- the LLM's value can drift, so we ignore it and trust the data."""
    cluster = [
        _ep(tool_sequence=["slack_message"], outcome="done"),
        _ep(tool_sequence=["slack_message"], outcome="done"),
        _ep(tool_sequence=["slack_message"], outcome="failed"),
    ]
    parsed = {
        "common_task_pattern": "Reply to slack",
        "common_outcome": "done",
        "success_rate": 0.99,  # LLM lied; should be ignored
        "subjects_involved": ["Miles"],
        "notable_patterns": "1 failed",
    }
    out = _build_compacted_episode(
        agent_handle="alice",
        agent_role="Engineer",
        cluster=cluster,
        exemplar_ids=[],
        parsed=parsed,
    )
    assert out.kind == "compacted"
    assert out.count == 3
    # Ground truth: 2 done out of 3 = 0.666...
    assert abs(out.success_rate - (2 / 3)) < 0.01
    assert out.common_task_pattern == "Reply to slack"
    assert out.subjects_involved == ["Miles"]
    # Aggregate window covers the cluster's full time range.
    assert out.started_at == min(e.started_at for e in cluster)
    assert out.ended_at == max(e.ended_at for e in cluster)
    # Tool sequence inherits from the cluster's first (representative) member.
    assert out.tool_sequence == cluster[0].tool_sequence


def test_build_compacted_episode_handles_missing_llm_fields() -> None:
    """A bare ``{"common_task_pattern": "..."}`` should still produce a
    valid Episode with sane defaults."""
    cluster = [_ep(tool_sequence=["x"], outcome="done") for _ in range(2)]
    out = _build_compacted_episode(
        agent_handle="alice",
        agent_role="Engineer",
        cluster=cluster,
        exemplar_ids=[],
        parsed={"common_task_pattern": "minimal"},
    )
    assert out.common_task_pattern == "minimal"
    assert out.subjects_involved == []
    assert out.notable_patterns == ""
    assert out.success_rate == 1.0  # both done


# ---------------------------------------------------------------------
# Worker: store / queue / role / provider stubs
# ---------------------------------------------------------------------


class _StoreStub:
    """In-memory EpisodeStore-compatible stub for worker tests."""

    def __init__(self) -> None:
        self.compaction_candidates: list[Episode] = []
        self.deleted_ids: list[str] = []
        self.inserted_compacted: list[Episode] = []
        self.non_terminal_dropped = 0
        self.consolidated_dropped = 0
        self.compacted_evicted = 0
        # Counters to verify call sequencing:
        self.calls: list[str] = []

    async def fetch_raw_for_compaction(
        self, *, agent_handle: str, older_than_days: int, limit: int
    ) -> list[Episode]:
        self.calls.append("fetch")
        return list(self.compaction_candidates)

    async def insert_compacted(self, episode: Episode) -> None:
        self.calls.append("insert")
        self.inserted_compacted.append(episode)

    async def delete_episodes_by_ids(self, ids: list[str]) -> int:
        self.calls.append("delete")
        self.deleted_ids.extend(ids)
        return len(ids)

    async def delete_non_terminal_older_than_days(
        self, *, agent_handle: str, days: int
    ) -> int:
        self.calls.append("drop_non_terminal")
        return self.non_terminal_dropped

    async def delete_consolidated_older_than_days(
        self, *, agent_handle: str, days: int
    ) -> int:
        self.calls.append("drop_consolidated")
        return self.consolidated_dropped

    async def delete_compacted_older_than_days(
        self, *, agent_handle: str, days: int
    ) -> int:
        self.calls.append("evict_compacted")
        return self.compacted_evicted


class _QueueStub:
    def __init__(self) -> None:
        self.published: list[tuple[str, Any]] = []
        self.subscriptions: list[tuple[str, str]] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))

    async def subscribe(self, topic: str, group: str, handler: Any) -> None:
        self.subscriptions.append((topic, group))


class _Provider:
    model = "aux"

    def __init__(self, *, content: str) -> None:
        self._content = content
        self.calls = 0

    async def complete(self, messages, **_):
        self.calls += 1
        return Completion(content=self._content, input_tokens=10, output_tokens=10)


class _AgentStub:
    def __init__(self, role: Role) -> None:
        self.role_name = role.name

        class _Definition:
            def __init__(self, role: Role) -> None:
                self.role = role

        self.definition = _Definition(role)


class _AgentPoolStub:
    def __init__(self, agents: dict[str, _AgentStub]) -> None:
        self._agents = agents

    def get_by_handle(self, handle: str) -> _AgentStub | None:
        return self._agents.get(handle)


def _build_worker(
    *,
    store: _StoreStub,
    queue: _QueueStub,
    provider_content: str = '{"common_task_pattern": "Routine"}',
    min_cluster_size: int = 3,
    compacted_max_age_days: int = 0,
    exemplar_count: int = 1,
) -> tuple[EpisodeLifecycleWorker, _Provider]:
    role = Role(name="Engineer", handle="eng", llm="cheap", llm_auxiliary="cheap")
    org = Organization(name="Acme", roles=[role])
    pool = _AgentPoolStub({"alice": _AgentStub(role)})
    provider = _Provider(content=provider_content)
    worker = EpisodeLifecycleWorker(
        event_queue=queue,  # type: ignore[arg-type]
        episode_store=store,  # type: ignore[arg-type]
        llm_providers={"cheap": provider},  # type: ignore[arg-type]
        organization=org,
        agent_pool=pool,
        non_terminal_max_age_days=14,
        consolidated_grace_days=30,
        compaction_min_age_days=30,
        compaction_min_cluster_size=min_cluster_size,
        compaction_jaccard_threshold=0.6,
        compaction_batch_size=200,
        compaction_budget_tokens=1000,
        compacted_max_age_days=compacted_max_age_days,
        exemplar_count=exemplar_count,
    )
    return worker, provider


def _cluster(*, n: int = 3, tools: list[str] | None = None) -> list[Episode]:
    tools = tools or ["slack_message", "slack_reactions_add"]
    return [_ep(tool_sequence=tools, outcome="done") for _ in range(n)]


# ---------------------------------------------------------------------
# Worker: end-to-end behaviour
# ---------------------------------------------------------------------


async def test_worker_runs_full_lifecycle_on_compaction_event() -> None:
    """Happy path: drops fire first, then a viable cluster gets
    compacted (LLM call → insert compacted row → delete originals)."""
    store = _StoreStub()
    store.non_terminal_dropped = 5
    store.consolidated_dropped = 7
    store.compaction_candidates = _cluster(n=3)
    queue = _QueueStub()
    worker, provider = _build_worker(store=store, queue=queue)
    await worker.start()

    event = CompactionRequested(
        source="episode_store",
        agent_handle="alice",
        raw_count=600,
        threshold=500,
    )
    await worker._handle(event)

    # Action sequencing: drops -> fetch -> insert compacted -> delete raw.
    assert store.calls[0] == "drop_non_terminal"
    assert store.calls[1] == "drop_consolidated"
    assert store.calls[2] == "fetch"
    assert "insert" in store.calls
    assert "delete" in store.calls

    # One LLM call (one cluster), one compacted row, originals deleted
    # except one exemplar (exemplar_count=1).
    assert provider.calls == 1
    assert len(store.inserted_compacted) == 1
    assert len(store.deleted_ids) == 2  # 3 cluster - 1 exemplar
    compacted = store.inserted_compacted[0]
    assert compacted.kind == "compacted"
    assert compacted.count == 3
    assert compacted.common_task_pattern == "Routine"

    # CompactionCompleted event published with the per-action counts.
    completed = [
        e for topic, e in queue.published if isinstance(e, CompactionCompleted)
    ]
    assert len(completed) == 1
    assert completed[0].non_terminal_dropped == 5
    assert completed[0].consolidated_dropped == 7
    assert completed[0].clusters_compacted == 1
    assert completed[0].raw_replaced_by_compaction == 2


async def test_worker_skips_compaction_when_below_min_cluster_size() -> None:
    """Two episodes can't form a cluster when min size is 3 -- drops
    fire but no LLM call, no compacted row."""
    store = _StoreStub()
    store.compaction_candidates = _cluster(n=2)
    queue = _QueueStub()
    worker, provider = _build_worker(store=store, queue=queue, min_cluster_size=3)
    await worker.start()

    await worker._handle(
        CompactionRequested(
            source="episode_store", agent_handle="alice", raw_count=600, threshold=500
        )
    )

    assert provider.calls == 0
    assert store.inserted_compacted == []
    assert store.deleted_ids == []


async def test_worker_handles_unparseable_llm_response() -> None:
    """LLM emits garbage -> cluster left intact, no row written."""
    store = _StoreStub()
    store.compaction_candidates = _cluster(n=3)
    queue = _QueueStub()
    worker, provider = _build_worker(
        store=store, queue=queue, provider_content="this is not json"
    )
    await worker.start()

    await worker._handle(
        CompactionRequested(
            source="episode_store", agent_handle="alice", raw_count=600, threshold=500
        )
    )

    assert provider.calls == 1  # we did call the LLM
    assert store.inserted_compacted == []  # but parsed nothing
    assert store.deleted_ids == []  # so nothing was deleted


async def test_worker_dedups_concurrent_requests_for_same_agent() -> None:
    """Two CompactionRequested events for the same agent in flight
    simultaneously: the second fires the 'already_running' skip path
    and doesn't re-run the lifecycle."""
    store = _StoreStub()
    store.compaction_candidates = _cluster(n=3)
    queue = _QueueStub()
    worker, _ = _build_worker(store=store, queue=queue)
    await worker.start()

    event = CompactionRequested(
        source="episode_store",
        agent_handle="alice",
        raw_count=600,
        threshold=500,
    )

    # Pre-acquire the agent lock so the first dispatch blocks.
    lock = worker._agent_locks.setdefault("alice", asyncio.Lock())
    await lock.acquire()
    try:
        await worker._handle(event)  # second arrival sees lock.locked()
    finally:
        lock.release()

    # No store interaction happened (we never released to let the pass run);
    # just the skipped-completion event landed.
    skipped = [
        e
        for topic, e in queue.published
        if isinstance(e, CompactionCompleted) and e.skipped_reason
    ]
    assert len(skipped) == 1
    assert skipped[0].skipped_reason == "already_running"


async def test_worker_evicts_old_compacted_when_configured() -> None:
    """``compacted_max_age_days > 0`` triggers the eviction action."""
    store = _StoreStub()
    store.compaction_candidates = []  # nothing to compact
    store.compacted_evicted = 4
    queue = _QueueStub()
    worker, _ = _build_worker(store=store, queue=queue, compacted_max_age_days=365)
    await worker.start()

    await worker._handle(
        CompactionRequested(
            source="episode_store", agent_handle="alice", raw_count=600, threshold=500
        )
    )

    assert "evict_compacted" in store.calls
    completed = [
        e
        for topic, e in queue.published
        if isinstance(e, CompactionCompleted) and not e.skipped_reason
    ]
    assert len(completed) == 1
    assert completed[0].compacted_evicted == 4


async def test_worker_skips_eviction_when_disabled() -> None:
    """``compacted_max_age_days=0`` (default) means no eviction call."""
    store = _StoreStub()
    queue = _QueueStub()
    worker, _ = _build_worker(store=store, queue=queue, compacted_max_age_days=0)
    await worker.start()

    await worker._handle(
        CompactionRequested(
            source="episode_store", agent_handle="alice", raw_count=600, threshold=500
        )
    )

    assert "evict_compacted" not in store.calls


async def test_worker_subscribes_on_start_and_can_be_stopped() -> None:
    """Start binds the subscription; stop flips _running.  Idempotent."""
    store = _StoreStub()
    queue = _QueueStub()
    worker, _ = _build_worker(store=store, queue=queue)

    await worker.start()
    assert ("crewlet.events.compaction_requested", "episode-lifecycle-worker") in (
        queue.subscriptions
    )
    assert worker._running

    # Idempotent start.
    await worker.start()
    assert len(queue.subscriptions) == 1

    await worker.stop()
    assert not worker._running

    # Handler short-circuits when stopped.
    await worker._handle(
        CompactionRequested(
            source="episode_store", agent_handle="alice", raw_count=600, threshold=500
        )
    )
    assert store.calls == []


# Silence unused-import lint for symbols re-imported for IDE help.
_ = json
