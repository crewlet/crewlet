"""One comparator for event timestamps, shared by every store.

Event timestamps reach this package as ISO-8601 strings in two encodings
-- aware (``2026-04-07T03:25:04+00:00``) and naive
(``2026-04-07T03:25:04``) -- often for the same instant, because
``write_event`` stores whatever its caller passed.  Comparing those
lexicographically is wrong: ``"...04"`` sorts after ``"...04+00:00"``,
so the same moment orders differently depending on who wrote it.

That already caused one duplicate-row bug in the composite store's LLM
history merge, which is why this normalizer existed there first.  Paging
made it load-bearing everywhere: an ordering key that is wrong at the
boundary between two encodings silently skips or repeats rows, and a
reader who scrolled past a gap has no way to know.
"""

from __future__ import annotations

from datetime import UTC, datetime


def ts_key(ts: str) -> str:
    """Normalize an ISO-8601 timestamp to a naive-UTC sort key.

    Unparseable input is returned unchanged rather than raised on: this
    is a read path over data the store may not control, and an ordering
    that degrades to lexicographic for one malformed row is better than
    a listing that fails.
    """
    try:
        dt = datetime.fromisoformat(ts)
    except (TypeError, ValueError):
        return ts
    if dt.tzinfo is not None:
        dt = dt.astimezone(UTC).replace(tzinfo=None)
    return dt.isoformat()


def row_key(row: dict) -> tuple[str, str]:
    """The full ordering key for an event row: ``(instant, id)``.

    The id is the tiebreak, and it is not optional.  Burst writes share
    a timestamp at microsecond resolution routinely, and a keyset cursor
    over a non-unique key skips or repeats whatever collided with it.
    """
    return (ts_key(str(row.get("timestamp", ""))), str(row.get("id", "")))
