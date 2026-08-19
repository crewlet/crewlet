"""Tests for the NotificationService (event-queue-driven inbound + outbound)."""

import pytest
import pytest_asyncio

from crewlet.agent.pool import AgentPool
from crewlet.events.types import Event, ExternalNotification
from crewlet.notifications.handle import (
    HandleRegistry,
    register_human_contacts_from_org,
)
from crewlet.notifications.protocol import (
    InboundNotification,
    OutboundMessage,
)
from crewlet.notifications.service import (
    INBOUND_TOPIC,
    OUTBOUND_TOPIC,
    NotificationService,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.queue.memory import MemoryEventQueue
from crewlet.queue.topics import agent_inbox_topic


def _make_org(*roles: Role) -> Organization:
    return Organization(
        name="TestCo",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead=roles[0].name if roles else "",
                roles=list(roles),
            )
        ],
    )


@pytest.fixture
def org():
    return _make_org(
        Role(name="Engineer", email="alice@test.com"),
        Role(name="Designer", email="bob@test.com"),
    )


@pytest_asyncio.fixture
async def bus():
    q = MemoryEventQueue()
    await q.start()
    yield q
    await q.stop()


@pytest_asyncio.fixture
async def pool(bus, org):
    pool = AgentPool(bus)
    await pool.spawn_from_org(org)
    return pool


class MockTransport:
    """A mock transport that records sent messages."""

    def __init__(self, name: str = "test", fail: bool = False):
        self.name = name
        self.started = False
        self.stopped = False
        self.sent: list[OutboundMessage] = []
        self._fail = fail

    async def start(self) -> None:
        self.started = True

    async def stop(self) -> None:
        self.stopped = True

    async def send(self, message: OutboundMessage) -> bool:
        if self._fail:
            raise ConnectionError("transport down")
        self.sent.append(message)
        return True


class FailStartTransport:
    """A transport that fails on start()."""

    name: str = "failing"

    async def start(self) -> None:
        raise RuntimeError("connection refused")

    async def stop(self) -> None:
        pass

    async def send(self, message: OutboundMessage) -> bool:
        return True


def _make_service(
    bus: MemoryEventQueue,
    pool: AgentPool,
    transports: dict | None = None,
    rate_limit: int = 0,
) -> NotificationService:
    registry = HandleRegistry(pool)
    return NotificationService(
        event_queue=bus,
        transports=transports or {},
        handle_registry=registry,
        rate_limit=rate_limit,
    )


# --- Inbound: publish to notifications.inbound → routes to agent inbox ---


@pytest.mark.asyncio
async def test_inbound_routes_to_agent_inbox(bus, pool):
    service = _make_service(bus, pool)
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    inbound_event = Event(
        type="inbound_notification",
        payload=InboundNotification(
            source="jira",
            # Use a comment-add event so ``body`` is the comment text
            # — the only event class where the prompt builder still
            # includes ``body`` verbatim (issue-update bodies are the
            # full description and would duplicate the changelog).
            source_event_type="comment_created",
            recipient_handle="engineer",
            sender="manager",
            subject="PROJ-123: Fix bug",
            body="Please fix the login bug",
            # A real Jira webhook carries the issue key in metadata --
            # the parser extracts it.  It drives both the prompt's
            # ``## Get Full Context`` block and ``requires_recon``.
            metadata={"event_type": "comment_created", "issue_key": "PROJ-123"},
        ).model_dump(),
    )
    await bus.publish(INBOUND_TOPIC, inbound_event)

    assert len(received) == 1
    event = received[0]
    assert isinstance(event, ExternalNotification)
    assert event.notification_source == "jira"
    assert event.subject == "PROJ-123: Fix bug"
    assert event.agent_id == agent.id_str
    # Body should be enriched by the notification prompt builder, not raw
    assert "Please fix the login bug" in event.body
    assert "PROJ-123" in event.body
    # A Jira webhook with an issue key is a pointer -- the routed event
    # carries the recon flag so the Plan-phase prefetches can skip
    # aux-LLM filtering on a trigger too thin to filter against.
    assert event.context_requires_recon is True


@pytest.mark.asyncio
async def test_inbound_resolves_by_email(bus, pool):
    service = _make_service(bus, pool)
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    inbound_event = Event(
        type="inbound_notification",
        payload=InboundNotification(
            source="email",
            recipient_email="notif+engineer@test.com",
            subject="Hello via plus-address",
        ).model_dump(),
    )
    await bus.publish(INBOUND_TOPIC, inbound_event)

    assert len(received) == 1
    assert received[0].agent_id == agent.id_str


@pytest.mark.asyncio
async def test_inbound_unresolvable_logs_warning(bus, pool):
    """Unresolvable notifications don't publish to any inbox."""
    service = _make_service(bus, pool)
    await service.start()

    received: list[Event] = []

    async def catch_all(event: Event):
        received.append(event)

    # Subscribe to both agent inboxes to make sure nothing arrives
    for agent in pool.agents:
        await bus.subscribe(agent_inbox_topic(agent.handle), "test", catch_all)

    inbound_event = Event(
        type="inbound_notification",
        payload=InboundNotification(
            source="email",
            recipient_email="nobody@test.com",
            subject="Hello",
        ).model_dump(),
    )
    await bus.publish(INBOUND_TOPIC, inbound_event)

    assert len(received) == 0


@pytest.mark.asyncio
async def test_inbound_resolves_by_external_id(bus, pool):
    """Inbound notification resolves via external ID (e.g. Jira accountId)."""
    service = _make_service(bus, pool)
    # Register an external ID mapping
    jira_account_id = "712020:00000000-test"
    service.handle_registry.register_external_id("jira", jira_account_id, "engineer")
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    # No handle, no email — only metadata with assignee_account_id
    inbound_event = Event(
        type="inbound_notification",
        payload=InboundNotification(
            source="jira",
            recipient_handle="",
            recipient_email="",
            subject="Jira ticket via external ID",
            metadata={"assignee_account_id": jira_account_id},
        ).model_dump(),
    )
    await bus.publish(INBOUND_TOPIC, inbound_event)

    assert len(received) == 1
    assert received[0].agent_id == agent.id_str


@pytest.mark.asyncio
async def test_inbound_skips_self_action(bus, pool):
    """An agent must not be woken by a webhook describing its own
    action.  Without this guard, an agent assigned to its own issue
    keeps re-receiving the comment-add webhooks IT just posted and
    re-comments forever (observed: 28× same comment loop).

    The trigger-user filter inside ``JiraTransport`` already covers
    the watcher-routed path; this test exercises the standard
    resolution path (``assignee_account_id`` lookup), which had no
    self-check before.
    """
    service = _make_service(bus, pool)
    pm_account_id = "712020:pm-account-id"
    service.handle_registry.register_external_id("jira", pm_account_id, "engineer")
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    # Webhook resolves to the engineer agent via ``assignee_account_id``;
    # ``actor_account_id`` is the same agent — i.e. the agent's own
    # comment-add webhook coming back to it.
    inbound_event = Event(
        type="inbound_notification",
        payload=InboundNotification(
            source="jira",
            source_event_type="comment_created",
            recipient_handle="",
            recipient_email="",
            subject="POC-1 self-comment loop trigger",
            metadata={
                "assignee_account_id": pm_account_id,
                "actor_account_id": pm_account_id,
                "event_type": "comment_created",
            },
        ).model_dump(),
    )
    await bus.publish(INBOUND_TOPIC, inbound_event)

    assert received == [], "self-action should not reach the agent's inbox"


@pytest.mark.asyncio
async def test_inbound_resolves_by_github_login(bus, pool):
    """Inbound GitHub notification resolves via github_login external ID."""
    service = _make_service(bus, pool)
    service.handle_registry.register_external_id("github", "ali-swe", "engineer")
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    inbound_event = Event(
        type="inbound_notification",
        payload=InboundNotification(
            source="github",
            recipient_handle="",
            recipient_email="",
            subject="Add hello world Python script",
            metadata={
                "event_type": "pull_request.opened",
                "pr_number": "1",
                "repo": "nimbus-hq/test15",
                "github_login": "ali-swe",
            },
        ).model_dump(),
    )
    await bus.publish(INBOUND_TOPIC, inbound_event)

    assert len(received) == 1
    assert received[0].agent_id == agent.id_str


# --- Outbound: publish to notifications.outbound → transport.send() called ---


@pytest.mark.asyncio
async def test_outbound_dispatches_to_transport(bus, pool):
    transport = MockTransport("test")
    service = _make_service(bus, pool, transports={"test": transport})
    await service.start()

    outbound_event = Event(
        type="outbound_message",
        payload=OutboundMessage(
            transport="test",
            sender_handle="engineer",
            recipient="user@external.com",
            subject="Hello",
            body="Test message",
        ).model_dump(),
    )
    await bus.publish(OUTBOUND_TOPIC, outbound_event)

    assert len(transport.sent) == 1
    assert transport.sent[0].sender_handle == "engineer"
    assert transport.sent[0].recipient == "user@external.com"


# --- Unknown transport → warning logged ---


@pytest.mark.asyncio
async def test_outbound_unknown_transport_warns(bus, pool):
    """Publishing to outbound with unknown transport doesn't crash."""
    service = _make_service(bus, pool)
    await service.start()

    outbound_event = Event(
        type="outbound_message",
        payload=OutboundMessage(
            transport="nonexistent",
            sender_handle="engineer",
            recipient="user@test.com",
            body="Hello",
        ).model_dump(),
    )
    # Should not raise
    await bus.publish(OUTBOUND_TOPIC, outbound_event)


# --- Failed send → error logged ---


@pytest.mark.asyncio
async def test_outbound_failed_send_logs_error(bus, pool):
    """Transport that raises on send() is caught and logged."""
    transport = MockTransport("broken", fail=True)
    service = _make_service(bus, pool, transports={"broken": transport})
    await service.start()

    outbound_event = Event(
        type="outbound_message",
        payload=OutboundMessage(
            transport="broken",
            sender_handle="engineer",
            recipient="user@test.com",
            body="Hello",
        ).model_dump(),
    )
    # Should not raise
    await bus.publish(OUTBOUND_TOPIC, outbound_event)
    assert len(transport.sent) == 0


# --- Transport lifecycle tests ---


@pytest.mark.asyncio
async def test_transport_start_and_stop(bus, pool):
    transport = MockTransport("test")
    service = _make_service(bus, pool, transports={"test": transport})
    await service.start()
    assert transport.started
    await service.stop()
    assert transport.stopped


@pytest.mark.asyncio
async def test_start_idempotent(bus, pool):
    """Calling start() twice doesn't double-start transports."""
    transport = MockTransport("test")
    service = _make_service(bus, pool, transports={"test": transport})
    await service.start()
    transport.started = False  # Reset flag
    await service.start()  # Should be no-op
    assert not transport.started


@pytest.mark.asyncio
async def test_transport_start_failure_handled(bus, pool):
    """A transport that raises on start() doesn't prevent other transports."""
    good = MockTransport("good")
    service = _make_service(
        bus, pool, transports={"failing": FailStartTransport(), "good": good}
    )
    await service.start()
    assert good.started


@pytest.mark.asyncio
async def test_transport_stop_failure_handled(bus, pool):
    """A transport that raises on stop() is logged but doesn't crash."""

    class FailStopTransport:
        name = "failing"

        async def start(self) -> None:
            pass

        async def stop(self) -> None:
            raise RuntimeError("cleanup failed")

        async def send(self, message):
            return True

    service = _make_service(bus, pool, transports={"failing": FailStopTransport()})
    await service.start()
    # Should not raise
    await service.stop()


async def test_transports_setter_replaces_dict(bus, pool):
    """``transports`` is settable so the engine can swap the map on a
    running service during live config apply / rollback."""

    class _T:
        def __init__(self, name: str) -> None:
            self.name = name

        async def start(self) -> None:
            pass

        async def stop(self) -> None:
            pass

        async def send(self, message):
            return True

    a, b = _T("a"), _T("b")
    service = _make_service(bus, pool, transports={"a": a})
    assert list(service.transports) == ["a"]

    service.transports = {"b": b}
    assert list(service.transports) == ["b"]

    # Outbound dispatch must use the swapped-in transport.
    assert service._transports["b"] is b


async def test_transports_setter_is_defensive_copy(bus, pool):
    """Mutating the dict passed to the setter must not bleed into the
    service's internal storage."""

    class _T:
        def __init__(self, name: str) -> None:
            self.name = name

        async def start(self) -> None:
            pass

        async def stop(self) -> None:
            pass

        async def send(self, message):
            return True

    service = _make_service(bus, pool, transports={})
    incoming = {"x": _T("x")}
    service.transports = incoming
    incoming["y"] = _T("y")  # outside mutation
    assert list(service.transports) == ["x"]


# --- Multiple inbound notifications ---


@pytest.mark.asyncio
async def test_inbound_multiple_agents(bus, pool):
    """Inbound notifications route to the correct agent."""
    service = _make_service(bus, pool)
    await service.start()

    engineer_inbox: list[Event] = []
    designer_inbox: list[Event] = []

    async def engineer_handler(event: Event):
        engineer_inbox.append(event)

    async def designer_handler(event: Event):
        designer_inbox.append(event)

    await bus.subscribe("crewlet.agent.engineer.inbox", "test", engineer_handler)
    await bus.subscribe("crewlet.agent.designer.inbox", "test", designer_handler)

    # Send to engineer
    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="inbound_notification",
            payload=InboundNotification(
                source="slack",
                recipient_handle="engineer",
                subject="For engineer",
            ).model_dump(),
        ),
    )

    # Send to designer
    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="inbound_notification",
            payload=InboundNotification(
                source="slack",
                recipient_handle="designer",
                subject="For designer",
            ).model_dump(),
        ),
    )

    assert len(engineer_inbox) == 1
    assert engineer_inbox[0].subject == "For engineer"
    assert len(designer_inbox) == 1
    assert designer_inbox[0].subject == "For designer"


# --- Notification prompt enrichment ---


@pytest.mark.asyncio
async def test_inbound_slack_body_enriched_with_prompt(bus, pool):
    """Slack notifications should have enriched body with tool instructions."""
    service = _make_service(bus, pool)
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="inbound_notification",
            payload=InboundNotification(
                source="slack",
                source_event_type="message",
                recipient_handle="engineer",
                sender="U12345",
                subject="Slack message",
                body="Hey engineer, can you update the report?",
                metadata={
                    "channel": "C001",
                    "ts": "1234567890.000100",
                    "thread_ts": "",
                    "bot_user_id": "U_BOT",
                },
            ).model_dump(),
        ),
    )

    assert len(received) == 1
    event = received[0]
    assert isinstance(event, ExternalNotification)
    # Body must contain the original message text.
    assert "Hey engineer, can you update the report?" in event.body
    # Body must carry Slack-specific enrichment from the prompt builder
    # (mention format + the message reference to act on).
    assert "<@USER_ID>" in event.body
    assert "Message id:" in event.body
    # ...and must NOT name a third-party tool.  ``docs/concepts/
    # tool-capabilities.md`` is explicit that engine prompts describe the
    # capability and let the LLM map it to whatever its MCP server
    # actually registered — the deployed server's names are not knowable
    # here, and a guess that does not resolve is worse than no name at
    # all (the turn engine's own comments cite this exact string as the
    # canonical wrong guess).
    assert "slack_reactions_add" not in event.body
    # Body must contain channel + timestamp context.
    assert "C001" in event.body
    assert "1234567890.000100" in event.body
    # Raw body should NOT be passed through unchanged.
    assert event.body != "Hey engineer, can you update the report?"
    # ``salient_body`` carries the raw message verbatim, with NONE of
    # the triage scaffolding — this is what the learning workers
    # (relevance filters, counterparty profiler, PersistDecider) read.
    assert event.salient_body == "Hey engineer, can you update the report?"
    assert "<@USER_ID>" not in event.salient_body
    assert "Triage" not in event.salient_body


# --- Rate limiting tests ---


@pytest.mark.asyncio
async def test_rate_limit_drops_excess(bus, pool):
    """Notifications exceeding rate limit are dropped."""
    service = _make_service(bus, pool, rate_limit=2)
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    # Send 5 notifications rapidly to the same agent
    for i in range(5):
        await bus.publish(
            INBOUND_TOPIC,
            Event(
                type="inbound_notification",
                payload=InboundNotification(
                    source="test",
                    recipient_handle="engineer",
                    subject=f"Notification {i}",
                ).model_dump(),
            ),
        )

    # Only first 2 should have been routed to inbox
    assert len(received) == 2


@pytest.mark.asyncio
async def test_no_rate_limit_by_default(bus, pool):
    """Without rate_limit, all notifications are processed."""
    service = _make_service(bus, pool)
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    for i in range(10):
        await bus.publish(
            INBOUND_TOPIC,
            Event(
                type="inbound_notification",
                payload=InboundNotification(
                    source="test",
                    recipient_handle="engineer",
                    subject=f"Notification {i}",
                ).model_dump(),
            ),
        )

    assert len(received) == 10


# --- Human-seat recipient → info skip, not undeliverable warning ---


@pytest.mark.asyncio
async def test_inbound_for_human_seat_skipped_quietly(bus):
    human = Role(
        name="Sarah Chen",
        kind="human",
        email="sarah@acme.com",
        contact={"atlassian_account_id": "5b10-sarah"},
    )
    org = _make_org(Role(name="Engineer", email="alice@test.com"), human)
    pool = AgentPool(bus)
    await pool.spawn_from_org(org)
    registry = HandleRegistry(pool, org_provider=lambda: org)
    # Mirror engine boot: human contact IDs register before webhooks.
    register_human_contacts_from_org(registry, org)
    service = NotificationService(
        event_queue=bus,
        transports={},
        handle_registry=registry,
    )
    await service.start()

    skips: list[Event] = []

    async def capture(event: Event) -> None:
        skips.append(event)

    await bus.subscribe("crewlet.events.notification_skipped", "test", capture)

    inbox_events: list[Event] = []

    async def capture_inbox(event: Event) -> None:
        inbox_events.append(event)

    await bus.subscribe("crewlet.agent.sarah-chen.inbox", "test", capture_inbox)

    # 1. By handle.
    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="inbound_notification",
            payload=InboundNotification(
                source="jira",
                recipient_handle="sarah-chen",
                subject="Issue assigned",
            ).model_dump(),
        ),
    )
    # 2. By external account id (assignee).
    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="inbound_notification",
            payload=InboundNotification(
                source="jira",
                subject="Issue assigned",
                metadata={"assignee_account_id": "5b10-sarah"},
            ).model_dump(),
        ),
    )

    assert len(skips) == 2
    assert all("human seat" in s.reason for s in skips)
    assert all(s.handle == "sarah-chen" for s in skips)
    # Nothing must ever reach a human "inbox" topic.
    assert inbox_events == []


@pytest.mark.asyncio
async def test_inbound_unknown_recipient_still_undeliverable(bus, pool, org):
    registry = HandleRegistry(pool, org_provider=lambda: org)
    service = NotificationService(
        event_queue=bus, transports={}, handle_registry=registry
    )
    await service.start()

    skips: list[Event] = []

    async def capture(event: Event) -> None:
        skips.append(event)

    await bus.subscribe("crewlet.events.notification_skipped", "test", capture)

    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="inbound_notification",
            payload=InboundNotification(
                source="jira",
                recipient_handle="total-stranger",
            ).model_dump(),
        ),
    )

    assert len(skips) == 1
    assert "no agent found" in skips[0].reason


# --- Plane: webhook dispatch + external-ID resolution ---


_PLANE_ACTOR = "aaaaaaaa-0000-0000-0000-00000000000a"
_PLANE_ENGINEER = "11111111-1111-1111-1111-111111111111"
_PLANE_PROJECT = "facefeed-0000-0000-0000-000000000001"


def _plane_transport():
    from crewlet.config import PlaneConfig
    from crewlet.notifications.transports.plane import PlaneTransport

    transport = PlaneTransport(
        PlaneConfig(url="https://plane.test", workspace="testco")
    )
    transport.set_project_leads({"ENG": "engineer"})
    transport._project_names[_PLANE_PROJECT] = "ENG"
    return transport


def _plane_issue_body(**data_over) -> dict:
    data = {
        "id": "beefbeef-0000-0000-0000-000000000002",
        "name": "Fix login flow",
        "project": _PLANE_PROJECT,
        "sequence_id": 7,
        "updated_at": "2026-07-26T10:00:00Z",
        "assignees": [],
    }
    data.update(data_over)
    return {
        "event": "issue",
        "action": "created",
        "workspace_slug": "testco",
        "data": data,
        "activity": {"field": None, "actor": {"id": _PLANE_ACTOR}},
    }


@pytest.mark.asyncio
async def test_plane_raw_webhook_dispatches_to_transport(bus, pool):
    """A ``raw_webhook`` with source=plane reaches the PlaneTransport's
    routing (here: the unassigned-create lead fallback)."""
    transport = _plane_transport()
    service = _make_service(bus, pool, transports={"plane": transport})
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="raw_webhook",
            source="plane",
            payload={"body": _plane_issue_body(), "headers": {}, "body_raw_b64": ""},
        ),
    )

    assert len(received) == 1
    assert received[0].agent_id == agent.id_str
    assert received[0].metadata["routed_via"] == "project_lead_fallback"


@pytest.mark.asyncio
async def test_plane_no_transport_fallback_uses_parser(bus, pool):
    """Without a PlaneTransport the raw webhook still parses via
    ``parse_plane_webhook`` — the unrouted notification carries no
    target, so it surfaces as an undeliverable skip (not a parse
    failure that drops the event silently)."""
    service = _make_service(bus, pool)
    await service.start()

    skips: list[Event] = []

    async def capture(event: Event) -> None:
        skips.append(event)

    await bus.subscribe("crewlet.events.notification_skipped", "test", capture)

    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="raw_webhook",
            source="plane",
            payload={"body": _plane_issue_body(), "headers": {}, "body_raw_b64": ""},
        ),
    )

    assert len(skips) == 1
    assert skips[0].notification_source == "plane"
    assert "no agent found" in skips[0].reason


@pytest.mark.asyncio
async def test_inbound_resolves_by_plane_user_id(bus, pool):
    """Inbound Plane notification resolves via the plane_user_id
    external-ID candidate."""
    service = _make_service(bus, pool)
    service.handle_registry.register_external_id("plane", _PLANE_ENGINEER, "engineer")
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="inbound_notification",
            payload=InboundNotification(
                source="plane",
                source_event_type="issue.update",
                subject="[ENG-7] Fix login flow",
                metadata={
                    "event_type": "issue.update",
                    "plane_user_id": _PLANE_ENGINEER,
                    "actor_account_id": _PLANE_ACTOR,
                    "routed_via": "subscriber",
                },
            ).model_dump(),
        ),
    )

    assert len(received) == 1
    assert received[0].agent_id == agent.id_str


@pytest.mark.asyncio
async def test_plane_human_seat_skipped_natively(bus):
    """A human seat targeted via plane_user_id is skip-notified-natively
    — Plane already notified the person; the engine never forwards."""
    human = Role(
        name="Sarah Chen",
        kind="human",
        email="sarah@acme.com",
        contact={"plane_user_id": "5b10ac8d-0000-0000-0000-00000000-sara"},
    )
    org = _make_org(Role(name="Engineer", email="alice@test.com"), human)
    pool = AgentPool(bus)
    await pool.spawn_from_org(org)
    registry = HandleRegistry(pool, org_provider=lambda: org)
    register_human_contacts_from_org(registry, org)
    service = NotificationService(
        event_queue=bus, transports={}, handle_registry=registry
    )
    await service.start()

    skips: list[Event] = []

    async def capture(event: Event) -> None:
        skips.append(event)

    await bus.subscribe("crewlet.events.notification_skipped", "test", capture)

    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="inbound_notification",
            payload=InboundNotification(
                source="plane",
                subject="[ENG-7] Fix login flow",
                metadata={"plane_user_id": "5b10ac8d-0000-0000-0000-00000000-sara"},
            ).model_dump(),
        ),
    )

    assert len(skips) == 1
    assert "human seat" in skips[0].reason
    assert skips[0].handle == "sarah-chen"


@pytest.mark.asyncio
async def test_plane_self_action_skipped(bus, pool):
    """The generic self-action guard keys on actor_account_id — an
    agent's own Plane action must never wake it (the transport excludes
    the actor during fan-out; this covers any fall-through)."""
    service = _make_service(bus, pool)
    service.handle_registry.register_external_id("plane", _PLANE_ENGINEER, "engineer")
    await service.start()

    received: list[Event] = []
    agent = pool.get_by_handle("engineer")

    async def inbox_handler(event: Event):
        received.append(event)

    await bus.subscribe(agent_inbox_topic(agent.handle), "test", inbox_handler)

    await bus.publish(
        INBOUND_TOPIC,
        Event(
            type="inbound_notification",
            payload=InboundNotification(
                source="plane",
                source_event_type="issue_comment.create",
                subject="[ENG-7] Fix login flow",
                metadata={
                    "event_type": "issue_comment.create",
                    "plane_user_id": _PLANE_ENGINEER,
                    "actor_account_id": _PLANE_ENGINEER,
                },
            ).model_dump(),
        ),
    )

    assert received == [], "self-action should not reach the agent's inbox"


# --- empty-parse logging ---


@pytest.mark.asyncio
async def test_empty_parse_warns_for_gitlab(bus, pool, caplog):
    """``webhook_produced_no_notifications`` must stay a WARNING for
    sources whose parsers return empty WITHOUT logging —
    ``parse_gitlab_webhook``'s unknown-``object_kind`` branch is
    silent, so this warning is the only signal an entirely unhandled
    GitLab event type is being discarded."""
    import logging

    service = _make_service(bus, pool)
    caplog.set_level(logging.WARNING)

    await service._parse_and_route_webhook(
        Event(
            type="raw_webhook",
            source="gitlab",
            payload={"body": {"object_kind": "wiki_page"}, "headers": {}},
        )
    )

    assert "webhook_produced_no_notifications" in caplog.text


@pytest.mark.asyncio
async def test_empty_parse_quiet_for_plane(bus, pool, caplog):
    """Plane's empty parses are by design (``project`` / ``cycle`` /
    ``module`` / ``user`` events) and its parser logs its own skip
    reasons, so the empty result stays below WARNING."""
    import logging

    service = _make_service(bus, pool)
    caplog.set_level(logging.WARNING)

    await service._parse_and_route_webhook(
        Event(
            type="raw_webhook",
            source="plane",
            payload={
                "body": {"event": "cycle", "action": "created", "data": {"id": "x"}},
                "headers": {},
            },
        )
    )

    assert "webhook_produced_no_notifications" not in caplog.text
