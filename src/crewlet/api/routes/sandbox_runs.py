"""GET /sandbox-runs — the detached coding runs the engine is holding.

The live projection already tracks in-flight sandbox runs and pushes them
to the dashboard, and it is the wrong source for this question in two
ways. It is in-memory, so it starts empty after an API restart; and it
sweeps an entry after twelve hours, while a run parked on a question can
legitimately wait days for a human to answer. The states that most need a
person were therefore the ones least likely to be on screen — and a
``reseed`` run (pause expired, box reclaimed, work preserved on a pushed
branch) had no surface anywhere at all. It looked exactly like work that
had finished.

``pending_sandbox_run`` is the durable record, and this reads it.

What it deliberately does not return: ``execute_state``, the serialised
Execute-loop conversation. It is the largest column in the row and every
prompt in it is already reachable through the event store; shipping it to
a board that renders one line per run would be a page-weight problem in
exchange for nothing.
"""

from __future__ import annotations

from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet._logging import get_logger

logger = get_logger("api.routes.sandbox_runs")


class PendingStoreUnavailable(Exception):
    """No durable store to read: the engine has no database configured."""


def _iso(value: Any) -> str:
    try:
        return value.isoformat() if value is not None else ""
    except Exception:
        return ""


def _answerable_in_chat(conversation_key: str) -> bool:
    """Whether a reply on a chat surface could ever reach this run.

    The resume path matches an inbound notification's conversation key
    against the one stored at kick-off, by exact string equality. A run
    started by anything other than an external notification — a schedule
    tick, a task assignment, an A2A wake — stored ``event:{id}``, a key
    no inbound message can reproduce. Such a run is not answerable
    through any chat surface, and telling somebody to "reply in the
    thread" would send them to a thread that does not exist.
    """
    return bool(conversation_key) and not conversation_key.startswith("event:")


def _serialise(run: Any) -> dict[str, Any]:
    return {
        "turn_id": run.turn_id,
        "agent_handle": run.agent_handle,
        "role": run.role,
        "status": run.status,
        "coding_agent": run.coding_agent,
        "task_description": run.task_description,
        "question": run.question,
        "audience": run.audience,
        "branch": run.branch,
        "trace_id": run.trace_id,
        "owner": run.owner,
        # `sandbox_id` non-empty means a box exists; `paused_at` set means
        # it is currently paused and being billed for as a snapshot. The
        # board draws those two facts rather than the ids themselves.
        "box_exists": bool(run.sandbox_id),
        "paused_at": _iso(run.paused_at),
        "pause_ttl_seconds": run.pause_ttl_seconds,
        "started_at": _iso(run.created_at),
        "updated_at": _iso(run.updated_at),
        "answerable_in_chat": _answerable_in_chat(run.conversation_key),
    }


async def sandbox_runs_payload(app: Any) -> dict[str, Any]:
    """Every run the engine still holds, oldest first."""
    from crewlet.db.client import Database

    db = getattr(app.state, "database", None)
    if not isinstance(db, Database):
        raise PendingStoreUnavailable("no database configured")

    from crewlet.sandbox.pending_store import PostgresPendingSandboxRunStore

    store = PostgresPendingSandboxRunStore(db)
    runs = await store.list_active()
    return {"runs": [_serialise(run) for run in runs]}


async def get_sandbox_runs(request: Request) -> JSONResponse:
    """GET /sandbox-runs — the REST twin of the ``sandbox_runs`` query."""
    try:
        return JSONResponse(await sandbox_runs_payload(request.app))
    except PendingStoreUnavailable:
        # Not an error: without a database the engine cannot park a run at
        # all, so "there are none" is the true answer for this deployment.
        return JSONResponse(
            {
                "runs": [],
                "degraded": "no database configured — detached runs are not persisted",
            }
        )
    except Exception:
        logger.exception("sandbox_runs_read_failed")
        return JSONResponse({"error": "pending store unavailable"}, status_code=503)
