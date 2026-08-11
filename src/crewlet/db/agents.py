"""Deterministic agent-identity derivation.

Agent UUIDs must be stable across engine restarts: anything keyed by
them (personal-memory rows, onboarding markers, AGENT-scope knowledge
docs) would otherwise be orphaned on every restart -- the writes would
survive in PostgreSQL but no live agent would reference them anymore.

Per ``docs/concepts/organization-model.md``, **handles are the
canonical stable identity** for an agent.  This module derives each
agent's UUID deterministically from ``(org_name, handle)`` via
:func:`uuid.uuid5`, so the same role in the same org always lands on
the same UUID -- no DB round-trip, no migration, no state to keep in
sync.  This single helper is the entire identity story.
"""

from __future__ import annotations

from uuid import UUID, uuid5

from crewlet._logging import get_logger

logger = get_logger("db.agents")


# Fixed UUIDv4 chosen once and frozen forever.  Changing this value
# would re-derive every existing agent UUID and orphan every memory
# row written before the change -- treat as a load-bearing constant.
AGENT_ID_NAMESPACE = UUID("c1ea9c6e-3f5d-4c9c-9e9f-1d2c3b4a5e6f")


def derive_agent_id(org_name: str, handle: str) -> UUID:
    """Return the deterministic ``UUID`` for ``(org_name, handle)``.

    Uses :func:`uuid.uuid5` against a fixed namespace so the result is
    stable across processes, machines, and restarts.  ``org_name`` is
    included in the input to keep handles namespaced -- two companies
    in the same Postgres can both have an ``agent-ceo`` without
    colliding.
    """
    if not org_name or not handle:
        msg = "org_name and handle are required to derive an agent id"
        raise ValueError(msg)
    return uuid5(AGENT_ID_NAMESPACE, f"{org_name}:{handle}")
