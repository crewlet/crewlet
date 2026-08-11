"""Unit tests for ``CounterpartyStore``.

Uses a stub ``Database`` so these are pure unit tests — no Postgres
required.  The stub captures executed SQL + args and returns canned
rows from upsert RETURNING clauses.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any

import pytest

from crewlet.events.types import ExternalNotification
from crewlet.learning.counterparty_store import CounterpartyStore
from crewlet.learning.interaction import CanonicalIdentity, InboundInteraction


class _DBStub:
    """Minimal ``Database``-shaped stub for store-level tests."""

    def __init__(
        self,
        *,
        upsert_rows: list[dict[str, Any]] | None = None,
        select_rows: list[dict[str, Any]] | None = None,
    ) -> None:
        self.executed: list[tuple[str, tuple[Any, ...]]] = []
        self._upsert_rows = upsert_rows or []
        self._select_rows = select_rows or []

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        self.executed.append((query, args))
        if "INSERT INTO counterparty_profiles" in query:
            if not self._upsert_rows:
                raise RuntimeError("no canned upsert rows")
            return [self._upsert_rows.pop(0)]
        if "SELECT" in query:
            return list(self._select_rows)
        return []


def _canned_row(
    *,
    observer="alice",
    subject_handle="bob",
    interaction_count=1,
    traits: dict[str, Any] | None = None,
) -> dict[str, Any]:
    return {
        "observer_handle": observer,
        "subject_handle": subject_handle,
        "subject_external_id": "",
        "subject_platform": "",
        "subject_name": "Bob",
        "traits": traits if traits is not None else {"communication_style": "terse"},
        "first_seen_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        "last_updated_at": datetime(2026, 4, 23, 9, 5, tzinfo=UTC),
        "last_corroborated_at": datetime(2026, 4, 23, 9, 5, tzinfo=UTC),
        "interaction_count": interaction_count,
    }


async def test_upsert_requires_observer_handle() -> None:
    store = CounterpartyStore(_DBStub())  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        await store.upsert(
            observer_handle="",
            subject_handle="bob",
            traits_patch={"x": 1},
        )


async def test_upsert_requires_a_subject_identifier() -> None:
    store = CounterpartyStore(_DBStub())  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        await store.upsert(
            observer_handle="alice",
            subject_handle="",
            subject_external_id="",
            traits_patch={"x": 1},
        )


async def test_upsert_sends_jsonb_patch_and_increments() -> None:
    db = _DBStub(upsert_rows=[_canned_row(interaction_count=1)])
    store = CounterpartyStore(db)  # type: ignore[arg-type]
    profile = await store.upsert(
        observer_handle="alice",
        subject_handle="bob",
        subject_name="Bob",
        traits_patch={"communication_style": "terse"},
    )
    assert profile.observer_handle == "alice"
    assert profile.interaction_count == 1
    assert profile.traits == {"communication_style": "terse"}
    sql, args = db.executed[0]
    assert "INSERT INTO counterparty_profiles" in sql
    assert "ON CONFLICT" in sql
    assert "traits || EXCLUDED.traits" in sql
    # args order: observer, subject_handle, subject_external_id,
    # subject_platform, subject_name, traits_json, increment, increment
    assert args[0] == "alice"
    assert args[1] == "bob"
    assert args[4] == "Bob"
    assert json.loads(args[5]) == {"communication_style": "terse"}


async def test_upsert_without_increment_keeps_interaction_count() -> None:
    db = _DBStub(upsert_rows=[_canned_row(interaction_count=0)])
    store = CounterpartyStore(db)  # type: ignore[arg-type]
    await store.upsert(
        observer_handle="alice",
        subject_handle="bob",
        traits_patch={},
        increment_interactions=False,
    )
    _, args = db.executed[0]
    assert args[6] == 0  # increment arg passed into VALUES
    assert args[7] == 0  # increment arg used in ON CONFLICT update
    # traits_patch was empty, so traits_changed=0 and last_corroborated_at
    # is preserved.
    assert args[8] == 0


async def test_upsert_marks_corroborated_when_traits_change() -> None:
    db = _DBStub(upsert_rows=[_canned_row(interaction_count=1)])
    store = CounterpartyStore(db)  # type: ignore[arg-type]
    await store.upsert(
        observer_handle="alice",
        subject_handle="bob",
        traits_patch={"communication_style": "terse"},
    )
    _, args = db.executed[0]
    assert args[8] == 1


async def test_fetch_by_subject_handle() -> None:
    row = _canned_row()
    db = _DBStub(select_rows=[row])
    store = CounterpartyStore(db)  # type: ignore[arg-type]
    profile = await store.fetch(observer_handle="alice", subject_handle="bob")
    assert profile is not None
    assert profile.subject_handle == "bob"
    sql, args = db.executed[0]
    assert "WHERE observer_handle = $1" in sql
    assert args == ("alice", "bob", "", "")


async def test_fetch_returns_none_when_missing_observer() -> None:
    store = CounterpartyStore(_DBStub())  # type: ignore[arg-type]
    assert await store.fetch(observer_handle="", subject_handle="bob") is None


async def test_fetch_returns_none_when_no_subject_identifier() -> None:
    store = CounterpartyStore(_DBStub())  # type: ignore[arg-type]
    assert await store.fetch(observer_handle="alice", subject_handle="") is None


async def test_fetch_for_senders_returns_stored_profile() -> None:
    row = _canned_row(subject_handle="")
    row.update(
        subject_external_id="U12345",
        subject_platform="slack",
        subject_name="External Stakeholder",
    )
    db = _DBStub(select_rows=[row])
    store = CounterpartyStore(db)  # type: ignore[arg-type]

    sender = CanonicalIdentity(
        external_id="U12345", platform="slack", display_name="External Stakeholder"
    )
    profiles = await store.fetch_for_senders("alice", [sender])
    assert len(profiles) == 1
    assert profiles[0].subject_external_id == "U12345"
    assert profiles[0].subject_platform == "slack"


async def test_fetch_for_senders_one_profile_per_sender_in_order() -> None:
    """One concurrent fetch per sender; result order follows input
    order (the turn path passes distinct senders first-seen-first)."""
    alice_row = _canned_row(subject_handle="")
    alice_row.update(subject_external_id="U-A", subject_platform="slack")
    bob_row = _canned_row(subject_handle="")
    bob_row.update(subject_external_id="U-B", subject_platform="slack")

    class _PerSenderDB(_DBStub):
        async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
            self.executed.append((query, args))
            if "SELECT" in query:
                return [alice_row] if args[2] == "U-A" else [bob_row]
            return []

    store = CounterpartyStore(_PerSenderDB())  # type: ignore[arg-type]
    senders = [
        CanonicalIdentity(external_id="U-A", platform="slack"),
        CanonicalIdentity(external_id="U-B", platform="slack"),
    ]
    profiles = await store.fetch_for_senders("alice", senders)
    assert [p.subject_external_id for p in profiles] == ["U-A", "U-B"]


async def test_fetch_for_senders_empty_input_returns_empty() -> None:
    store = CounterpartyStore(_DBStub())  # type: ignore[arg-type]
    assert await store.fetch_for_senders("alice", []) == []


def test_inbound_interaction_from_external_notification() -> None:
    event = ExternalNotification(
        notification_source="slack",
        sender="External User",
        body="hello",
        metadata={"slack_user_id": "U99"},
    )
    (interaction,) = InboundInteraction.list_from_trigger_event(event)
    assert interaction.has_sender
    assert interaction.sender.external_id == "U99"
    assert interaction.sender.platform == "slack"
    assert interaction.sender.display_name == "External User"
    assert interaction.body == "hello"


def test_inbound_interaction_falls_back_to_sender_name() -> None:
    # No metadata id present — fall back to sender display name.
    event = ExternalNotification(
        notification_source="email",
        sender="alice@example.com",
        body="hi",
        metadata={},
    )
    (interaction,) = InboundInteraction.list_from_trigger_event(event)
    assert interaction.has_sender
    assert interaction.sender.external_id == "alice@example.com"


def test_inbound_interaction_from_a2a_uses_sender_handle() -> None:
    class _StubA2A:
        type = "a2a_message_sent"
        sender = "bob"
        content = "hey"

    (interaction,) = InboundInteraction.list_from_trigger_event(_StubA2A())
    assert interaction.has_sender
    assert interaction.sender.handle == "bob"
    assert interaction.sender.platform == "a2a"
    assert interaction.body == "hey"


def test_inbound_interaction_returns_empty_for_unknown_event() -> None:
    class _Stub:
        type = "task_assigned"

    (interaction,) = InboundInteraction.list_from_trigger_event(_Stub())
    assert not interaction.has_sender
    assert InboundInteraction.list_from_trigger_event(None) == []
