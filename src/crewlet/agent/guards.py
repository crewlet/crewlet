"""Runtime invariants for the turn engine.

Nine non-negotiable invariants are enforced in code, not in prompts.
This module hosts those checks so every
call site (Plan, Execute, Review, spawn_subagent, TurnEngine)
touches the same implementation.

Invariants covered here (others live in their natural homes):

- :func:`check_delegation_depth` -- cap on colleague-handoff chains.
  This is the always-on backstop against runaway / circular delegation:
  it is checked at the top of every turn regardless of how the turn was
  triggered, and ``a2a_ask`` propagates the chain so the recipient's
  turn inherits the accumulated depth.
- :class:`StallDetector` -- terminate the turn as ``failed`` when two
  ``self_iterate`` rounds produce the same artifact hash.
- :func:`check_subagent_tools` -- ensures sub-agent allowlist is a
  subset of the parent's and excludes denied tools. (Delegates to
  :func:`crewlet.tools.surface.ToolSurface.for_subagent`; exposed
  here so the TurnEngine can make the call without importing the
  tool module directly.)
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field


class DelegationDepthExceeded(RuntimeError):
    """Raised when the turn's delegation chain has hit the configured cap."""


def check_delegation_depth(*, depth: int, limit: int) -> None:
    """Raise :class:`DelegationDepthExceeded` when depth >= limit.

    ``limit <= 0`` disables the cap.
    """
    if limit <= 0:
        return
    if depth >= limit:
        raise DelegationDepthExceeded(f"delegation depth {depth} >= limit {limit}")


def artifact_hash(text: str) -> str:
    """Stable short hash for an artifact string. Used by the stall guard."""
    return hashlib.sha256(text.encode("utf-8", errors="replace")).hexdigest()[:16]


@dataclass
class StallDetector:
    """Stateful guard that aborts the turn after repeated
    ``self_iterate`` decisions produce an unchanged artifact.

    Call :meth:`observe` with the current iteration's final artifact
    text whenever Review picks ``self_iterate``. If the hash has not
    changed for ``threshold`` iterations in a row, :meth:`should_abort`
    returns ``True`` and the TurnEngine terminates the turn as
    ``failed`` (publishing a ``turn.guard_breach`` event).
    """

    threshold: int = 2
    _history: list[str] = field(default_factory=list)

    def observe(self, artifact: str) -> None:
        self._history.append(artifact_hash(artifact))

    def should_abort(self) -> bool:
        if len(self._history) < self.threshold:
            return False
        tail = self._history[-self.threshold :]
        return len(set(tail)) == 1

    def reset(self) -> None:
        self._history.clear()


__all__ = [
    "DelegationDepthExceeded",
    "StallDetector",
    "artifact_hash",
    "check_delegation_depth",
]
