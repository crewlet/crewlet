"""GET /conversations — what a seat already did in each thread it works.

The inspectable half of the per-conversation session ledger. The engine
renders these entries back into a conversation's next turn, and a
context source an operator cannot read is exactly the "second, invisible
memory" the CLI-agent workspace deletes on every call to avoid. So the
same rows the prompt is built from are served here verbatim.

Two questions, one payload function: without ``key`` it lists the
conversations a seat has worked, most recent first; with one it returns
that conversation's entries, oldest first — the order the prompt block
renders them in.
"""

from __future__ import annotations

from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet._logging import get_logger

logger = get_logger("api.routes.conversations")

# Conversations listed for one seat. Enough to cover every thread a busy
# seat touched in a retention window without paging; the store caps its
# own per-conversation depth separately.
_CONVERSATION_LIST_LIMIT = 50


def _store(app: Any) -> Any:
    """The durable ledger, or ``None``.

    Built from the database rather than taken from a co-located engine,
    so the standalone API answers this identically — and so an engine
    running on the memory twin reports "cannot see it" rather than
    serving a process-local half-view that would disagree with the next
    node to own the seat.
    """
    from crewlet.db.client import Database
    from crewlet.db.conversation_sessions import PostgresConversationSessionStore

    db = getattr(app.state, "database", None)
    return PostgresConversationSessionStore(db) if isinstance(db, Database) else None


async def conversations_payload(
    app: Any, *, handle: str = "", key: str = ""
) -> dict[str, Any]:
    """List a seat's conversations, or one conversation's entries."""
    store = _store(app)
    if store is None or not handle:
        # No store is not an empty ledger, and the difference matters on
        # a screen: ``available`` false means "this node cannot see it",
        # which a caller must not draw as "this seat has said nothing".
        return {
            "available": store is not None,
            "handle": handle,
            "conversations": [],
            "entries": [],
        }

    if key:
        try:
            rows = await store.recent(
                agent_handle=handle,
                conversation_key=key,
                limit=_CONVERSATION_LIST_LIMIT,
            )
        except Exception:
            logger.exception("conversation_entries_read_failed", agent=handle)
            return {
                "available": False,
                "handle": handle,
                "conversations": [],
                "entries": [],
            }
        return {
            "available": True,
            "handle": handle,
            "conversation_key": key,
            "conversations": [],
            "entries": [
                {
                    "turn_id": row.turn_id,
                    "at": row.created_at.isoformat() if row.created_at else "",
                    **row.entry,
                }
                for row in rows
            ],
        }

    try:
        listed = await store.conversations(
            agent_handle=handle, limit=_CONVERSATION_LIST_LIMIT
        )
    except Exception:
        logger.exception("conversations_read_failed", agent=handle)
        return {
            "available": False,
            "handle": handle,
            "conversations": [],
            "entries": [],
        }
    return {
        "available": True,
        "handle": handle,
        "conversations": [
            {
                "conversation_key": row.get("conversation_key", ""),
                "entries": int(row.get("entries", 0) or 0),
                "last_at": (row["last_at"].isoformat() if row.get("last_at") else ""),
            }
            for row in listed
        ],
        "entries": [],
    }


async def get_conversations(request: Request) -> JSONResponse:
    """GET /conversations — the REST twin of the ``conversations`` query."""
    return JSONResponse(
        await conversations_payload(
            request.app,
            handle=request.query_params.get("handle", ""),
            key=request.query_params.get("key", ""),
        )
    )
