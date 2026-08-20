"""THE work-key grammar: what identifies the unit of work a turn did.

A turn is not identified by its ``turn_id``. Two nodes that both run the
same trigger mint two turn ids, so anything keyed on one records the
duplicate rather than collapsing it. What *is* stable across a re-run is
the set of trigger events the turn was dispatched for — the same
identity :mod:`crewlet.db.turn_completions` keys the completion ledger
on, derived here so the two cannot disagree.

Two writes carry it:

* ``episodes`` — exactly one row per turn, so a plain unique index makes
  a second writer's insert a no-op.
* ``counterparty_profiles.interaction_count`` — an unconditional ``+ n``
  that a duplicate turn would double.

Why a key rather than an epoch fence. The natural instinct is to guard
these writes the way ``pending_sandbox_run`` is guarded, with
``WHERE owner_epoch = $epoch``. That works for mutations of an existing
row and fails for an insert, which has no row to attach the condition
to; and even done atomically it loses data in the case where nothing
went wrong. A node that completes a turn, acks the delivery and *then*
lapses would have its episode fenced out — the turn happened, the memory
of it is gone. Keying on the work keeps that row and still collapses the
duplicate, which is strictly better on every case that matters:

===========================================  =========  ==========
scenario                                     fence      work key
===========================================  =========  ==========
zombie and owner both complete               one row    one row
owner completes, acks, then lapses           ROW LOST   one row
ledger fails open, turn legitimately re-runs two rows    one row
===========================================  =========  ==========

The key travels as a **contextvar**, not a parameter. It is set once
around an inbox dispatch and read at the bottom of the stack by writers
several frames down (the episode write, the counterparty profiler) that
sit behind functions with no other reason to know about it — the same
shape, and the same reason, as ``bind_llm_scope`` in
:mod:`crewlet.providers.llm.scope`.

An **empty key is the honest default**, not a failure: a turn with no
ledgerable trigger (a scheduled fire, a sub-agent, a sandbox resume that
runs in a later task than the dispatch that bound the key) has no
cross-node duplicate to collapse, and the unique index is partial so
those rows are unconstrained. Only the case the key can actually speak
for is constrained by it.

This module imports nothing from the rest of Crewlet, for the same
reason :mod:`crewlet.queue.topics` and :mod:`crewlet.env_refs` do not: a
producer and a consumer that disagree about an identity never raise,
they just quietly record two of everything.
"""

from __future__ import annotations

import contextlib
import hashlib
from collections.abc import Iterator, Sequence
from contextvars import ContextVar

__all__ = [
    "bind_work_key",
    "current_work_key",
    "derive_work_key",
]

# Length of the hex digest kept. 32 hex chars is 128 bits of a SHA-256 —
# far past any collision concern for the number of turns a company will
# ever run, and short enough to read in a log line or a psql row.
_KEY_CHARS = 32

_WORK_KEY: ContextVar[str] = ContextVar("crewlet_work_key", default="")


def derive_work_key(event_ids: Sequence[str]) -> str:
    """One stable key for the set of triggers a turn was dispatched for.

    **Sorted and deduplicated** before hashing, so the key depends on
    which events the turn covered and not on the order a broker happened
    to deliver them in — two nodes draining the same partition can see a
    different order for the same set.

    Returns ``""`` for an empty set, which is what a turn with no
    ledgerable trigger gets: no duplicate to collapse, and nothing for
    the partial unique index to constrain.

    Never derive this from a coalesced digest's own id. That id is
    minted fresh on every merge, so a key taken from it differs on every
    redelivery and matches nothing — the same trap
    ``_record_worked_triggers`` documents for the completion ledger.
    """
    unique = sorted({str(e) for e in event_ids if str(e)})
    if not unique:
        return ""
    digest = hashlib.sha256("\n".join(unique).encode("utf-8")).hexdigest()
    return digest[:_KEY_CHARS]


def current_work_key() -> str:
    """The work key in scope, or ``""`` outside a bound dispatch."""
    return _WORK_KEY.get()


@contextlib.contextmanager
def bind_work_key(key: str) -> Iterator[None]:
    """Bind ``key`` for the duration of the block, restoring it after.

    Reset through the token rather than by re-setting the previous
    value: a contextvar set inside a task is not visible to its parent,
    and restoring by assignment would paper over that difference in the
    one place it is load-bearing.
    """
    token = _WORK_KEY.set(key or "")
    try:
        yield
    finally:
        _WORK_KEY.reset(token)
