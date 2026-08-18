"""Agent endpoints — list, detail, and durable memory.

``/agents`` and ``/agents/{id}`` read live state from the in-memory
:class:`~crewlet.api.live_state.LiveState` projection (O(1), and carrying
the in-flight ``live_call`` so a refresh re-renders the live LLM row).
The store is consulted only for historical detail: an agent's LLM
invocation history and its durable memory surfaces.
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet._logging import get_logger
from crewlet.api.routes._common import iso, safe_store_query

logger = get_logger("api.routes")

_MEMORY_KIND_LONG = "diary_long"
_MEMORY_KIND_SHORT = "diary_short"


def _stream(request: Request) -> Any:
    return getattr(request.app.state, "stream", None)


def agents_payload(app: Any) -> list[dict[str, Any]]:
    """Every configured seat with its live state overlaid."""
    roles: list[dict[str, Any]] = app.state.agent_roles
    stream = getattr(app.state, "stream", None)
    if stream is None:
        return [dict(r) for r in roles]
    return list(stream.live.merge_agents(roles))


async def agent_detail(app: Any, agent_id: str) -> dict[str, Any] | None:
    """One agent: static config + live state + LLM history, or ``None``.

    Live fields (state, phase, tokens, the in-flight ``live_call``) come
    from the projection; ``llm_history`` is read from the event store for
    the agent's runtime id.  Shared by ``GET /agents/{id}`` and the
    WebSocket ``agent`` query so both answer with the same object.
    """
    roles: list[dict[str, Any]] = app.state.agent_roles
    role = next((r for r in roles if r.get("id") == agent_id), None)
    if role is None:
        return None

    merged = dict(role)
    role_name = role.get("role", "") or ""
    stream = getattr(app.state, "stream", None)
    runtime_id = ""
    if stream is not None:
        overlay = stream.live.agent_overlay(role_name)
        if overlay is not None:
            merged.update(overlay)
        runtime_id = stream.live.runtime_id_for(role_name)

    store = app.state.event_store
    if not runtime_id and store is not None and role_name:
        states = await safe_store_query(store.get_agent_states([role_name]), {})
        runtime_id = states.get(role_name, {}).get("runtime_id", "") or ""
        merged.setdefault("runtime_id", runtime_id)

    if store is not None and runtime_id:
        history = await safe_store_query(store.get_agent_llm_history(runtime_id), [])
        merged["llm_history"] = history
    return merged


async def list_agents(request: Request) -> JSONResponse:
    """GET /agents — static config merged with live projection state."""
    return JSONResponse(agents_payload(request.app))


async def get_agent(request: Request) -> JSONResponse:
    """GET /agents/{id} — one agent: static config + live state + history."""
    merged = await agent_detail(request.app, request.path_params["id"])
    if merged is None:
        return JSONResponse({"error": "Agent not found"}, status_code=404)
    return JSONResponse(merged)


# ---------------------------------------------------------------------------
# Durable memory (learning subsystem)
# ---------------------------------------------------------------------------


def _is_memory_doc(doc: dict[str, Any]) -> bool:
    meta = doc.get("metadata") or {}
    return isinstance(meta, dict) and meta.get("kind") in (
        _MEMORY_KIND_LONG,
        _MEMORY_KIND_SHORT,
    )


def _ttl_expired(metadata: dict[str, Any]) -> bool:
    raw = metadata.get("ttl_until") if isinstance(metadata, dict) else None
    if not raw:
        return False
    try:
        expiry = datetime.fromisoformat(str(raw))
    except ValueError:
        return False
    if expiry.tzinfo is None:
        expiry = expiry.replace(tzinfo=UTC)
    return expiry < datetime.now(UTC)


def _serialize_personal_memories(
    docs: list[dict[str, Any]],
) -> dict[str, list[dict[str, Any]]]:
    """Split agent-scope docs into long / short memory buckets.

    Skips non-memory docs (onboarding markers, agent-scope rows
    without ``kind``).  TTL-expired SHORT entries are returned with
    ``expired=true`` so the dashboard can grey them out rather than hide
    them silently.
    """
    long_mem: list[dict[str, Any]] = []
    short_mem: list[dict[str, Any]] = []
    for doc in docs:
        if not _is_memory_doc(doc):
            continue
        meta = doc.get("metadata") or {}
        kind = meta.get("kind")
        entry = {
            "id": doc.get("id", ""),
            "content": doc.get("content", ""),
            "metadata": meta,
            "ttl_until": meta.get("ttl_until", "") if isinstance(meta, dict) else "",
        }
        if kind == _MEMORY_KIND_SHORT:
            entry["expired"] = _ttl_expired(meta)
            short_mem.append(entry)
        else:
            long_mem.append(entry)
    return {"long": long_mem, "short": short_mem}


async def _list_diary_for_agent(
    database: Any, agent_id: str, *, limit: int = 100
) -> list[dict[str, Any]]:
    """Return recent ``agent_diary`` rows for *agent_id* (dashboard shape)."""
    if not agent_id:
        return []
    try:
        from crewlet.db.client import Database
        from crewlet.learning.diary import AgentDiary
    except ImportError:  # pragma: no cover - defensive
        return []
    if not isinstance(database, Database):
        return []
    diary = AgentDiary(database)
    try:
        return await diary.list_for_agent(
            agent_id, limit=int(limit), include_expired=True
        )
    except Exception:
        logger.exception("diary_dashboard_list_failed", agent_id=agent_id)
        return []


async def _list_episodes_for_agent(
    database: Any, agent_handle: str, *, limit: int = 50
) -> list[dict[str, Any]]:
    """Return recent episode rows for *agent_handle* via direct SQL."""
    if not agent_handle:
        return []
    try:
        from crewlet.db.client import Database
    except ImportError:  # pragma: no cover - defensive
        return []
    if not isinstance(database, Database):
        return []
    try:
        rows = await database.execute(
            """
            SELECT id, agent_handle, agent_role, task_id, turn_id,
                   started_at, ended_at, plan_summary, task_summary,
                   tool_sequence, skills_used, review_outcome,
                   duration_ms
            FROM episodes
            WHERE agent_handle = $1
            ORDER BY ended_at DESC
            LIMIT $2
            """,
            agent_handle,
            int(limit),
        )
    except Exception as exc:
        logger.exception("episodes_query_failed", error=str(exc))
        return []
    from crewlet.db._jsonb import decode_jsonb_str_list

    out: list[dict[str, Any]] = []
    for row in rows:
        out.append(
            {
                "id": str(row.get("id", "")),
                "task_id": row.get("task_id") or "",
                "turn_id": str(row.get("turn_id", "")),
                "task_summary": row.get("task_summary") or "",
                "plan_summary": row.get("plan_summary") or "",
                "tool_sequence": decode_jsonb_str_list(row.get("tool_sequence")),
                "skills_used": decode_jsonb_str_list(row.get("skills_used")),
                "review_outcome": row.get("review_outcome") or "",
                "started_at": iso(row.get("started_at")),
                "ended_at": iso(row.get("ended_at")),
                "duration_ms": int(row.get("duration_ms") or 0),
            }
        )
    return out


async def _list_counterparty_profiles(
    database: Any, observer_handle: str, *, limit: int = 50
) -> list[dict[str, Any]]:
    """Return profiles observed by *observer_handle*."""
    if not observer_handle:
        return []
    try:
        from crewlet.db.client import Database
    except ImportError:  # pragma: no cover - defensive
        return []
    if not isinstance(database, Database):
        return []
    try:
        rows = await database.execute(
            """
            SELECT observer_handle, subject_handle, subject_external_id,
                   subject_platform, subject_name, traits,
                   first_seen_at, last_updated_at, last_corroborated_at,
                   interaction_count
            FROM counterparty_profiles
            WHERE observer_handle = $1
            ORDER BY last_updated_at DESC
            LIMIT $2
            """,
            observer_handle,
            int(limit),
        )
    except Exception as exc:
        logger.exception("counterparty_query_failed", error=str(exc))
        return []
    from crewlet.db._jsonb import decode_jsonb_dict

    out: list[dict[str, Any]] = []
    for row in rows:
        subject_handle = row.get("subject_handle") or ""
        subject_name = row.get("subject_name") or ""
        external_id = row.get("subject_external_id") or ""
        platform = row.get("subject_platform") or ""
        if subject_name:
            label = subject_name
        elif subject_handle:
            label = subject_handle
        elif external_id:
            label = f"{platform}:{external_id}" if platform else external_id
        else:
            label = "(unknown)"
        out.append(
            {
                "subject_label": label,
                "subject_handle": subject_handle,
                "subject_external_id": external_id,
                "subject_platform": platform,
                "subject_name": subject_name,
                "traits": decode_jsonb_dict(row.get("traits")),
                "interaction_count": int(row.get("interaction_count") or 0),
                "first_seen_at": iso(row.get("first_seen_at")),
                "last_updated_at": iso(row.get("last_updated_at")),
                "last_corroborated_at": iso(row.get("last_corroborated_at")),
            }
        )
    return out


async def _list_synthesized_skills(
    database: Any, agent_handle: str
) -> list[dict[str, Any]]:
    """Return agent-scope synthesized skills via the store helper."""
    if not agent_handle:
        return []
    try:
        from crewlet.db.client import Database
        from crewlet.learning.synthesized_skill_store import SynthesizedSkillStore
    except ImportError:  # pragma: no cover - defensive
        return []
    if not isinstance(database, Database):
        return []
    try:
        store = SynthesizedSkillStore(database)
        skills = await store.list_for_agent(agent_handle)
    except Exception as exc:
        logger.exception("synthesized_skills_query_failed", error=str(exc))
        return []
    return [
        {
            "id": str(s.id),
            "name": s.name,
            "description": s.description,
            "content": s.content,
            "tool_sequence": list(s.tool_sequence),
            "version": s.version,
            "created_at": iso(s.created_at),
            "updated_at": iso(s.updated_at),
        }
        for s in skills
    ]


async def agent_memory(app: Any, agent_id: str) -> dict[str, Any] | None:
    """Durable memories for one agent, or ``None`` if no such agent.

    Combines the four learning-subsystem stores (personal memories,
    episodes, counterparty profiles, synthesized skills) into one
    payload.  Each section degrades independently: a missing provider, a
    non-Postgres backend, or a query error yields an empty list for that
    section rather than failing the whole read.
    """
    roles: list[dict[str, Any]] = app.state.agent_roles
    role = next((r for r in roles if r.get("id") == agent_id), None)
    if role is None:
        return None

    handle = role.get("handle", "") or ""
    role_name = role.get("role", "") or ""

    stream = getattr(app.state, "stream", None)
    runtime_id = stream.live.runtime_id_for(role_name) if stream is not None else ""
    if not runtime_id:
        store = app.state.event_store
        if store is not None and role_name:
            states = await safe_store_query(store.get_agent_states([role_name]), {})
            runtime_id = states.get(role_name, {}).get("runtime_id", "") or ""
    # Fall back to the static config id so this still returns
    # personal-memory rows for orgs whose agents haven't emitted state
    # events yet.
    memory_scope_id = runtime_id or agent_id

    database = getattr(app.state, "database", None)
    personal_memories: dict[str, list[dict[str, Any]]] = {"long": [], "short": []}
    if memory_scope_id:
        diary_docs = await _list_diary_for_agent(database, memory_scope_id)
        personal_memories = _serialize_personal_memories(diary_docs)

    return {
        "agent_id": agent_id,
        "handle": handle,
        "role": role_name,
        "runtime_id": runtime_id,
        "personal_memories": personal_memories,
        "episodes": await _list_episodes_for_agent(database, handle),
        "counterparty_profiles": await _list_counterparty_profiles(database, handle),
        "synthesized_skills": await _list_synthesized_skills(database, handle),
    }


async def get_agent_memory(request: Request) -> JSONResponse:
    """GET /agents/{id}/memory — durable memories for one agent."""
    payload = await agent_memory(request.app, request.path_params["id"])
    if payload is None:
        return JSONResponse({"error": "Agent not found"}, status_code=404)
    return JSONResponse(payload)
