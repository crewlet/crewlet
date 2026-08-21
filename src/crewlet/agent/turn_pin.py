"""The config a turn runs under, pinned for its whole duration.

A live config apply mutates engine state **in place** so in-flight work
keeps working: the LLM provider map is `clear()` + `update()`d (identity
preserved on purpose), `_role_mcp_tools` is rewritten per role, an
`AgentDefinition` is reassigned on the running `AgentInstance`, and the
`TurnEngineSettings` cell hands out a new model in one shot.

In-place is right — the alternative is a turn holding a reference to a
dict nobody updates any more. But "keeps working" is not the same as
"stays coherent", and every one of those is read *repeatedly* within a
single turn:

- the ~18 `TurnEngine` settings accessors re-read the cell on **every
  access**, so a config swap mid-turn can run Plan under one round cap
  and Execute under another, or size a sub-agent's budget from a
  fraction the parent never saw;
- `_role_mcp_tools` is read twice per turn from two different places —
  once to build the availability set, once to render the tool catalogue;
- the agent's `definition` (role prompt, model, tools) is read from
  roughly twenty places across the phases.

So a turn could plan against one company and execute against another.

The fix is a **pin**: capture those four things once, at the top of the
turn, and read through the capture for the rest of it. The pin lives in
a :class:`~contextvars.ContextVar`, which means it propagates into every
task the turn spawns (sub-agents inherit a copy of the context at
`create_task` time) without threading a parameter through every phase
signature — and it means the *reads* stay written exactly as they were.

**What a pin does not buy.** It pins a catalogue, not a capability. A
pinned MCP tool wrapper whose client was stopped mid-turn still fails —
it just fails as a dead tool rather than as a name that vanished. That
is precisely why draining a seat before mutating it
(:meth:`TurnEngine.drain_seat`) is load-bearing rather than a nicety:
pinning makes a turn *consistent*, draining is what makes it *possible*.

The pin is keyed by owner and seat so it can only ever apply to the turn
that took it: a concurrent turn for a different agent, or a second
engine in the same process, reads live state.
"""

from __future__ import annotations

import contextlib
from collections.abc import Iterator
from contextvars import ContextVar
from dataclasses import dataclass, field
from typing import Any

_CURRENT: ContextVar[TurnPin | None] = ContextVar("crewlet_turn_pin", default=None)


@dataclass(frozen=True, slots=True)
class TurnPin:
    """One turn's frozen view of the config it started under."""

    owner: int
    """``id()`` of the TurnEngine that took the pin.  A second engine in
    the same process must read its own live state, not this."""

    agent_id: str
    """The seat this pin was taken for.  A concurrent turn for another
    seat shares the context only if it was spawned from this one, and
    even then must not inherit this seat's definition."""

    settings: Any = None
    llm_providers: dict[str, Any] = field(default_factory=dict)
    role_mcp_tools: list[Any] = field(default_factory=list)
    definition: Any = None


def current_pin() -> TurnPin | None:
    """The pin in force for this task, if any."""
    return _CURRENT.get()


def pin_for(owner: object, agent_id: str) -> TurnPin | None:
    """The pin in force, but only if it belongs to ``owner`` and ``agent_id``."""
    pin = _CURRENT.get()
    if pin is None:
        return None
    if pin.owner != id(owner) or pin.agent_id != agent_id:
        return None
    return pin


@contextlib.contextmanager
def pinned(pin: TurnPin) -> Iterator[TurnPin]:
    """Hold ``pin`` for the duration of the block."""
    token = _CURRENT.set(pin)
    try:
        yield pin
    finally:
        _CURRENT.reset(token)
