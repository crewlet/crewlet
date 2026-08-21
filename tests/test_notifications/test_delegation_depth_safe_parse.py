"""Regression test for the safe-parse of ``delegation_depth`` in
``NotificationService._handle_inbound_inner``.

A bare ``int(notification.metadata["delegation_depth"])``
would raise ``ValueError`` on any non-numeric string (common for
webhook metadata), aborting the inbound notification handling for
that message.  The safe parse falls back to ``0`` and logs the
malformed value.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet.events.types import Event, ExternalNotification
from crewlet.queue.topics import agent_inbox_topic


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


class _PartyStub:
    """Minimal stand-in for a ResolvedParty.

    Deliberately carries no ``agent``: the inbound path resolves seats
    from the org and must route on the handle and the derived id alone,
    so a stub with no live instance is the honest shape here.
    """

    def __init__(self, handle: str = "bob", agent_id: str = "id-bob") -> None:
        self.handle = handle
        self.name = handle
        self.agent_id = agent_id
        self.is_human = False
        self.is_local = False
        self.inbox_topic = agent_inbox_topic(handle)


class _HandleRegistryStub:
    """Matches the real HandleRegistry surface used by the inbound
    path: ``resolve_party*`` returns a party (or None)."""

    def __init__(self) -> None:
        self._party = _PartyStub()

    def resolve_party(self, handle: str) -> _PartyStub | None:
        return self._party if handle == "bob" else None

    def resolve_party_email(self, email: str) -> _PartyStub | None:
        return None

    def resolve_party_external(self, source: str, ext_key: str) -> _PartyStub | None:
        return None

    def resolve_handle(self, handle: str) -> None:
        return None

    def all_transports(self) -> list[str]:
        return []

    def all_handles(self) -> list[str]:
        return ["bob"]


def _build_inbound_event(metadata: dict[str, Any]) -> Event:
    """Construct an ``inbound_notification`` event carrying the given
    metadata, shaped like the API process would publish it."""
    return Event(
        type="inbound_notification",
        source="slack",
        payload={
            "source": "slack",
            "source_event_type": "message",
            "recipient_handle": "bob",
            "recipient_email": "",
            "sender": "alice",
            "subject": "",
            "body": "hello",
            "metadata": metadata,
        },
    )


async def _run(metadata: dict[str, Any]) -> list[tuple[str, Any]]:
    """Invoke ``_handle_inbound_inner`` and return the list of events
    published to the event queue."""
    from crewlet.notifications.service import NotificationService

    queue = _QueueStub()
    service = NotificationService(
        event_queue=queue,
        transports={},
        handle_registry=_HandleRegistryStub(),
    )
    await service._handle_inbound_inner(_build_inbound_event(metadata))
    return queue.published


async def test_non_numeric_delegation_depth_falls_back_to_zero():
    """A malformed depth string should not crash the route."""
    published = await _run({"delegation_depth": "not-a-number"})
    inbox = [e for _, e in published if isinstance(e, ExternalNotification)]
    assert inbox, "inbound notification routing aborted on malformed depth"
    assert inbox[0].delegation_depth == 0


async def test_numeric_string_delegation_depth_is_parsed():
    """A stringified integer should still be honored."""
    published = await _run({"delegation_depth": "2"})
    inbox = [e for _, e in published if isinstance(e, ExternalNotification)]
    assert inbox and inbox[0].delegation_depth == 2


async def test_missing_delegation_depth_defaults_to_zero():
    published = await _run({})
    inbox = [e for _, e in published if isinstance(e, ExternalNotification)]
    assert inbox and inbox[0].delegation_depth == 0


async def test_negative_delegation_depth_parses_through():
    """A negative stringified integer is still parsable (the cap
    check handles out-of-range values downstream)."""
    published = await _run({"delegation_depth": "-1"})
    inbox = [e for _, e in published if isinstance(e, ExternalNotification)]
    assert inbox and inbox[0].delegation_depth == -1


# ---------------------------------------------------------------------------
# delegation_chain parsing
# ---------------------------------------------------------------------------
#
# ``InboundNotification.metadata`` is typed ``dict[str, str]`` -- Pydantic
# coerces / rejects anything else at construction time -- so a producer
# carrying ``delegation_chain`` across a webhook boundary MUST encode it
# as a JSON array string.  Anything that doesn't decode to a list falls
# back to an empty chain rather than aborting routing; the
# delegation-depth cap still enforces the invariant downstream.


async def test_delegation_chain_parsed_from_json_string():
    """Webhook-shape metadata: JSON-encoded string is decoded."""
    published = await _run({"delegation_chain": '["ceo", "alice"]'})
    inbox = [e for _, e in published if isinstance(e, ExternalNotification)]
    assert inbox and inbox[0].delegation_chain == ["ceo", "alice"]


async def test_delegation_chain_missing_is_empty():
    published = await _run({})
    inbox = [e for _, e in published if isinstance(e, ExternalNotification)]
    assert inbox and inbox[0].delegation_chain == []


async def test_delegation_chain_malformed_json_falls_back_to_empty():
    """Malformed JSON must not crash the route; falls back to empty."""
    published = await _run({"delegation_chain": "not-json[["})
    inbox = [e for _, e in published if isinstance(e, ExternalNotification)]
    assert inbox, "routing aborted on malformed chain"
    assert inbox[0].delegation_chain == []


async def test_delegation_chain_non_list_json_falls_back_to_empty():
    """Well-formed JSON that isn't an array is still rejected."""
    published = await _run({"delegation_chain": '{"not":"a-list"}'})
    inbox = [e for _, e in published if isinstance(e, ExternalNotification)]
    assert inbox and inbox[0].delegation_chain == []


async def test_delegation_chain_coerces_non_string_entries():
    """JSON with numeric entries (e.g. ``[1, "alice", 42]``) gets
    stringified so downstream ``str`` consumers don't blow up;
    ``null`` and empty-string entries are dropped.
    """
    published = await _run({"delegation_chain": '[1, "alice", "", null, 42]'})
    inbox = [e for _, e in published if isinstance(e, ExternalNotification)]
    assert inbox and inbox[0].delegation_chain == ["1", "alice", "42"]


async def test_delegation_chain_preserves_falsy_but_valid_zero():
    """A producer that encodes
    IDs as numbers can legitimately emit ``0``; the parser must not
    filter it out with a blanket ``if x`` truthiness check.
    """
    published = await _run({"delegation_chain": "[0, 1, 2]"})
    inbox = [e for _, e in published if isinstance(e, ExternalNotification)]
    assert inbox and inbox[0].delegation_chain == ["0", "1", "2"]
