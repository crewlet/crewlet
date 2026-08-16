"""Which *seat* an LLM call is being made on behalf of.

Every built-in HTTP provider is stateless, so the engine has never
needed to tell a provider which agent it is serving: one shared
``AnthropicProvider`` answers every seat identically, and the seat's
identity only mattered for telemetry (which is threaded explicitly).

The :mod:`~crewlet.providers.llm.cli_agent` backend breaks that
assumption.  It drives a *local CLI process* that keeps state on
disk — sessions, history, project memory, auth — under a home
directory.  Run two seats against one shared home and seat B inherits
seat A's memory.  The provider therefore has to know the caller's
seat to pick an isolated workspace, and the provider map it lives in
is shared across every role (see
:func:`crewlet.config.create_llm_providers`).

This module is that seam: a context variable the turn engine binds
once per turn, and the CLI provider reads.  It is deliberately
*ambient* rather than an extra ``complete()`` argument:

- ``LLMProvider.complete`` is the documented provider contract
  (``docs/concepts/overview.md``); widening it for one backend's
  bookkeeping would force every third-party provider to grow a
  parameter it must ignore.
- The value is genuine turn-scoped ambient context, exactly like the
  OTel span the turn engine already restores around the same block,
  and asyncio copies the context into child tasks — so sub-agents
  spawned with ``asyncio.gather`` inherit their parent's seat for
  free.

**There is no unscoped state.**  Calls made outside a turn (the
learning workers' auxiliary summarisation, an extension, a smoke
test) resolve to :data:`ENGINE_SEAT`, which is a real, separate
workspace — never a shared default that quietly mixes seats
together.
"""

from __future__ import annotations

import re
from collections.abc import Iterator
from contextlib import contextmanager
from contextvars import ContextVar
from dataclasses import dataclass

#: Seat used for LLM calls that are not attributable to an agent —
#: engine-level auxiliary work, extensions, ``crewlet llm doctor``.
#: A real workspace of its own, not a shared fallback.
ENGINE_SEAT = "_engine"

#: Characters allowed in a workspace directory name.  A seat is an
#: agent *handle* (``sarah-chen``), already slug-shaped — but handles
#: come from founder-authored YAML, so the value is sanitised before
#: it is ever joined onto a filesystem path.
_UNSAFE = re.compile(r"[^a-z0-9._-]+")

#: Cap on a sanitised seat directory name.  Long enough for any real
#: handle, short enough that ``<state_dir>/seats/<seat>/home/...``
#: stays clear of ``PATH_MAX`` once a CLI nests its own directories
#: under it.
_MAX_SEAT_LEN = 64


@dataclass(frozen=True, slots=True)
class LLMCallScope:
    """The seat an LLM call belongs to.

    ``seat`` is an agent handle (or :data:`ENGINE_SEAT`).  ``phase``
    is advisory — it is carried for logging only; workspaces are keyed
    by seat alone so a seat's Plan and Execute calls share one
    warmed-up CLI home instead of thrashing two.
    """

    seat: str = ENGINE_SEAT
    phase: str = ""


#: The scope every unbound call resolves to.  A module singleton
#: rather than a ``ContextVar`` default: the value is frozen, so one
#: shared instance is safe, and keeping the ``ContextVar`` default
#: ``None`` sidesteps the mutable-default footgun entirely.
_UNBOUND = LLMCallScope()

_CURRENT: ContextVar[LLMCallScope | None] = ContextVar(
    "crewlet_llm_call_scope", default=None
)


def current_scope() -> LLMCallScope:
    """The scope in effect for the calling task."""
    return _CURRENT.get() or _UNBOUND


def current_seat() -> str:
    """The seat in effect, already sanitised for filesystem use."""
    return sanitize_seat(current_scope().seat)


@contextmanager
def bind_llm_scope(seat: str, *, phase: str = "") -> Iterator[LLMCallScope]:
    """Bind ``seat`` for the duration of the block.

    Restores the previous scope on exit, including on exception, so a
    failed turn cannot leak its seat into whatever runs next on the
    same task.  An empty ``seat`` binds :data:`ENGINE_SEAT` rather
    than an empty directory name.
    """
    scope = LLMCallScope(seat=seat or ENGINE_SEAT, phase=phase)
    token = _CURRENT.set(scope)
    try:
        yield scope
    finally:
        _CURRENT.reset(token)


def sanitize_seat(seat: str) -> str:
    """Map a seat identifier onto a safe single path segment.

    Lower-cases, collapses every character outside ``[a-z0-9._-]`` to
    ``-``, strips leading dots (no hidden directories, and ``.`` /
    ``..`` can never survive), and truncates.  An input that sanitises
    to nothing yields :data:`ENGINE_SEAT` — the workspace layer must
    always get a usable segment, and silently returning ``""`` would
    put a seat's home at the workspace root where every other seat
    could read it.
    """
    cleaned = _UNSAFE.sub("-", (seat or "").strip().lower()).strip("-.")
    cleaned = cleaned[:_MAX_SEAT_LEN].strip("-.")
    return cleaned or ENGINE_SEAT


__all__ = [
    "ENGINE_SEAT",
    "LLMCallScope",
    "bind_llm_scope",
    "current_scope",
    "current_seat",
    "sanitize_seat",
]
