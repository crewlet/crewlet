"""Tests for deterministic agent-identity derivation.

Per ``docs/concepts/organization-model.md``, handles are the canonical
stable identity.  ``derive_agent_id`` makes the runtime ``UUID``
deterministic in those terms so anything keyed by it (personal
memory, onboarding markers, AGENT-scope knowledge docs) survives
engine restarts without a separate persistence layer.
"""

from __future__ import annotations

from uuid import UUID

import pytest

from crewlet.db.agents import AGENT_ID_NAMESPACE, derive_agent_id


def test_derive_agent_id_deterministic() -> None:
    """Same ``(org_name, handle)`` always yields the same UUID."""
    a = derive_agent_id("Acme", "agent-ceo")
    b = derive_agent_id("Acme", "agent-ceo")
    assert a == b
    assert isinstance(a, UUID)


def test_derive_agent_id_namespaces_by_org() -> None:
    """The same handle in a different org yields a different UUID."""
    a = derive_agent_id("Acme", "agent-ceo")
    b = derive_agent_id("Globex", "agent-ceo")
    assert a != b


def test_derive_agent_id_distinct_handles() -> None:
    """Different handles in the same org yield different UUIDs."""
    a = derive_agent_id("Acme", "agent-ceo")
    b = derive_agent_id("Acme", "agent-engineer")
    assert a != b


def test_derive_agent_id_uses_documented_namespace() -> None:
    """Namespace UUID is load-bearing; changing it would orphan all
    pre-existing rows.  Pin it in a test so accidental changes are
    caught."""
    assert UUID("c1ea9c6e-3f5d-4c9c-9e9f-1d2c3b4a5e6f") == AGENT_ID_NAMESPACE


@pytest.mark.parametrize(
    ("org_name", "handle"),
    [("", "agent-ceo"), ("Acme", ""), ("", "")],
)
def test_derive_agent_id_rejects_empty_inputs(org_name: str, handle: str) -> None:
    with pytest.raises(ValueError, match="org_name and handle"):
        derive_agent_id(org_name, handle)
