"""Tests for ConfluenceTransport — outbound + webhook parsing."""

from unittest.mock import AsyncMock, MagicMock

import pytest

from crewlet.config import ConfluenceConfig
from crewlet.notifications.protocol import InboundNotification
from crewlet.notifications.transports.confluence import (
    ConfluenceTransport,
    _extract_mention_ids,
    parse_confluence_webhook,
)

# Default admin config used across tests
_CFG = ConfluenceConfig(
    url="https://test.atlassian.net/wiki",
    token="test-admin-token",
)

_CFG_WITH_SECRET = ConfluenceConfig(
    url="https://test.atlassian.net/wiki",
    token="test-admin-token",
    webhook_secret="test-secret",
)


@pytest.fixture
def transport() -> ConfluenceTransport:
    return ConfluenceTransport(_CFG)


# --- Lifecycle ---


async def test_start_stop(transport: ConfluenceTransport) -> None:
    await transport.start()
    assert transport._http_client is not None

    await transport.stop()
    assert transport._http_client is None


async def test_name() -> None:
    t = ConfluenceTransport(_CFG)
    assert t.name == "confluence"


async def test_send_not_supported() -> None:
    transport = ConfluenceTransport(_CFG)
    await transport.start()

    from crewlet.notifications.protocol import OutboundMessage

    msg = OutboundMessage(
        transport="confluence", sender_handle="eng", recipient="12345"
    )
    result = await transport.send(msg)
    assert result is False

    await transport.stop()


async def test_dedupe_defaults_to_a_process_local_store() -> None:
    """Correct for one process, and what the engine replaces with a
    shared store the moment a database is configured — a retry landing on
    a peer node would otherwise be a fresh delivery there."""
    from crewlet.db.deliveries import MemoryDeliveryDedupeStore

    t = ConfluenceTransport(_CFG)
    assert isinstance(t._dedupe, MemoryDeliveryDedupeStore)

    shared = MemoryDeliveryDedupeStore()
    t.set_delivery_dedupe(shared)
    assert t._dedupe is shared


# --- Webhook signature verification ---


def test_verify_webhook_no_secret() -> None:
    """No webhook_secret configured → always passes."""
    t = ConfluenceTransport(_CFG)
    assert t.verify_webhook(b'{"test": true}', {"x-hub-signature": "anything"})


def test_verify_webhook_valid_signature() -> None:
    import hashlib
    import hmac

    t = ConfluenceTransport(_CFG_WITH_SECRET)
    body = b'{"event": "page_updated"}'
    sig = "sha256=" + hmac.new(b"test-secret", body, hashlib.sha256).hexdigest()
    assert t.verify_webhook(body, {"x-hub-signature": sig})


def test_verify_webhook_invalid_signature() -> None:
    t = ConfluenceTransport(_CFG_WITH_SECRET)
    assert not t.verify_webhook(b'{"test": true}', {"x-hub-signature": "sha256=bad"})


def test_verify_webhook_missing_header() -> None:
    t = ConfluenceTransport(_CFG_WITH_SECRET)
    assert not t.verify_webhook(b'{"test": true}', {})


# --- Event deduplication ---


async def test_dedup_first_event_not_duplicate() -> None:
    t = ConfluenceTransport(_CFG)
    body = {"timestamp": 123, "page": {"id": "456"}, "event": "page_updated"}
    assert not await t._dedup_event(body)


async def test_dedup_same_event_is_duplicate() -> None:
    t = ConfluenceTransport(_CFG)
    body = {"timestamp": 123, "page": {"id": "456"}, "event": "page_updated"}
    assert not await t._dedup_event(body)
    assert await t._dedup_event(body)


async def test_dedup_different_events_not_duplicate() -> None:
    t = ConfluenceTransport(_CFG)
    body1 = {"timestamp": 123, "page": {"id": "456"}, "event": "page_updated"}
    body2 = {"timestamp": 789, "page": {"id": "012"}, "event": "page_created"}
    assert not await t._dedup_event(body1)
    assert not await t._dedup_event(body2)


# --- parse_confluence_webhook ---


def test_parse_page_created() -> None:
    body = {
        "event": "page_created",
        "timestamp": 1680000000,
        "userAccountId": "user-abc",
        "page": {
            "id": "12345",
            "title": "Architecture Decision: Auth",
            "space": {"key": "ENG", "name": "Engineering"},
            "version": {"number": 1},
        },
    }
    result = parse_confluence_webhook(body)
    assert result is not None
    assert result.source == "confluence"
    assert result.source_event_type == "page_created"
    assert result.subject == "[ENG] Architecture Decision: Auth"
    assert result.metadata["page_id"] == "12345"
    assert result.metadata["space_key"] == "ENG"
    assert result.metadata["actor_account_id"] == "user-abc"


def test_parse_page_updated_with_user_dict() -> None:
    """Data Center style with user dict instead of userAccountId."""
    body = {
        "webhookEvent": "page_updated",
        "timestamp": 1680000001,
        "user": {
            "name": "alice",
            "displayName": "Alice Smith",
            "accountId": "user-alice",
        },
        "page": {
            "id": "67890",
            "title": "Runbook: Deployment",
            "space": {"key": "OPS", "name": "Operations"},
            "version": {"number": 3, "message": "Updated deploy steps"},
        },
    }
    result = parse_confluence_webhook(body)
    assert result is not None
    assert result.source_event_type == "page_updated"
    assert result.sender == "Alice Smith"
    assert result.body == "Updated deploy steps"
    assert result.metadata["actor_account_id"] == "user-alice"


def test_parse_comment_created() -> None:
    body = {
        "event": "comment_created",
        "timestamp": 1680000002,
        "userAccountId": "commenter-id",
        "page": {
            "id": "12345",
            "title": "Design Doc",
            "space": {"key": "ENG"},
        },
        "comment": {
            "id": "99",
            "body": {
                "storage": {"value": "<p>Looks good!</p>"},
            },
        },
    }
    result = parse_confluence_webhook(body)
    assert result is not None
    assert result.source_event_type == "comment_created"
    assert result.body == "<p>Looks good!</p>"
    assert result.metadata["comment_id"] == "99"


def test_parse_no_event_type() -> None:
    """Missing event type returns None."""
    body = {"page": {"id": "123"}}
    assert parse_confluence_webhook(body) is None


def test_parse_label_added() -> None:
    body = {
        "event": "label_added",
        "timestamp": 1680000003,
        "userAccountId": "user-xyz",
        "page": {
            "id": "55555",
            "title": "API Reference",
            "space": {"key": "DOCS"},
        },
    }
    result = parse_confluence_webhook(body)
    assert result is not None
    assert result.source_event_type == "label_added"
    assert result.metadata["space_key"] == "DOCS"


# --- mention extraction ---


def test_extract_mention_ids_cloud() -> None:
    html = '<ac:link><ri:user ri:account-id="user-abc" /></ac:link> hello'
    assert _extract_mention_ids(html) == ["user-abc"]


def test_extract_mention_ids_data_center() -> None:
    html = '<ri:user ri:userkey="JIRAUSER10001" /> hi'
    assert _extract_mention_ids(html) == ["JIRAUSER10001"]


def test_extract_mention_ids_multiple() -> None:
    html = '<ri:user ri:account-id="a1" /> and <ri:user ri:account-id="a2" />'
    assert _extract_mention_ids(html) == ["a1", "a2"]


def test_extract_mention_ids_none() -> None:
    assert _extract_mention_ids("<p>No mentions here</p>") == []


def test_parse_comment_with_mentions() -> None:
    """Comment body with @mentions populates mention_ids metadata."""
    body = {
        "event": "comment_created",
        "timestamp": 1680000010,
        "userAccountId": "commenter",
        "page": {
            "id": "100",
            "title": "Design",
            "space": {"key": "ENG"},
            "version": {"by": {"accountId": "page-author"}},
        },
        "comment": {
            "id": "55",
            "body": {
                "storage": {
                    "value": '<p>Hey <ri:user ri:account-id="cto-id" /> check this</p>'
                },
            },
        },
    }
    result = parse_confluence_webhook(body)
    assert result is not None
    assert result.metadata["mention_ids"] == "cto-id"


# --- fetch_watchers ---


async def test_fetch_watchers_returns_account_ids() -> None:
    t = ConfluenceTransport(_CFG)
    mock_client = AsyncMock()

    def _make_response(account_ids: list[str]) -> MagicMock:
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {
            "results": [{"watcher": {"accountId": aid}} for aid in account_ids]
        }
        return resp

    mock_client.get = AsyncMock(
        side_effect=[
            _make_response(["user-1"]),  # /notification/created
            _make_response(["user-2"]),  # /notification/child-created
        ]
    )
    t._http_client = mock_client

    result = await t.fetch_watchers("12345")
    assert result == ["user-1", "user-2"]


async def test_fetch_watchers_deduplicates() -> None:
    t = ConfluenceTransport(_CFG)
    mock_client = AsyncMock()

    def _make_response(account_ids: list[str]) -> MagicMock:
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {
            "results": [{"watcher": {"accountId": aid}} for aid in account_ids]
        }
        return resp

    mock_client.get = AsyncMock(
        side_effect=[
            _make_response(["user-1"]),
            _make_response(["user-1"]),  # duplicate
        ]
    )
    t._http_client = mock_client

    result = await t.fetch_watchers("12345")
    assert result == ["user-1"]


async def test_fetch_watchers_before_start() -> None:
    t = ConfluenceTransport(_CFG)
    result = await t.fetch_watchers("12345")
    assert result == []


async def test_fetch_watchers_api_error() -> None:
    t = ConfluenceTransport(_CFG)
    mock_client = AsyncMock()
    mock_client.get = AsyncMock(side_effect=RuntimeError("connection error"))
    t._http_client = mock_client

    result = await t.fetch_watchers("12345")
    assert result == []


# --- handle_webhook ---


def _make_confluence_body(
    *,
    event: str = "page_updated",
    page_id: str = "12345",
    page_title: str = "Test Page",
    space_key: str = "ENG",
    trigger_account_id: str = "",
) -> dict:
    """Build a minimal Confluence webhook body."""
    body: dict = {
        "event": event,
        "timestamp": 1234567890,
        "page": {
            "id": page_id,
            "title": page_title,
            "space": {"key": space_key, "name": "Test Space"},
        },
    }
    if trigger_account_id:
        body["userAccountId"] = trigger_account_id
    return body


async def test_handle_webhook_basic() -> None:
    """Basic webhook produces at least one notification."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    body = _make_confluence_body()

    results = await t.handle_webhook(body)
    assert len(results) >= 1
    assert all(isinstance(n, InboundNotification) for n in results)


async def test_handle_webhook_signature_rejected() -> None:
    """Invalid signature returns empty list."""
    t = ConfluenceTransport(_CFG_WITH_SECRET)
    body = _make_confluence_body()
    results = await t.handle_webhook(body, headers={"x-hub-signature": "bad"})
    assert results == []


async def test_handle_webhook_signature_no_headers() -> None:
    """Missing headers when secret configured returns empty list."""
    t = ConfluenceTransport(_CFG_WITH_SECRET)
    body = _make_confluence_body()
    results = await t.handle_webhook(body, headers=None)
    assert results == []


async def test_handle_webhook_dedup() -> None:
    """Duplicate event returns empty list."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    body = _make_confluence_body()

    r1 = await t.handle_webhook(body)
    assert len(r1) >= 1

    r2 = await t.handle_webhook(body)
    assert r2 == []


async def test_handle_webhook_trigger_user_excluded() -> None:
    """Trigger user excluded from routing, other watchers still get it."""
    t = ConfluenceTransport(_CFG)
    # Agent triggers the event. Another agent is a watcher.
    t.fetch_watchers = AsyncMock(return_value=["our-agent-id", "other-watcher-id"])

    registry = MagicMock()
    our_agent = MagicMock()
    our_agent.handle = "agent-architect"
    other_agent = MagicMock()
    other_agent.handle = "agent-ceo"
    registry.resolve_external_id.side_effect = lambda transport, ext_id: {
        "our-agent-id": our_agent,
        "other-watcher-id": other_agent,
    }.get(ext_id)
    t.set_handle_registry(registry)

    body = _make_confluence_body(trigger_account_id="our-agent-id")
    results = await t.handle_webhook(body)

    # Trigger user excluded, other watcher gets it
    handles = [n.recipient_handle for n in results]
    assert "agent-architect" not in handles
    assert "agent-ceo" in handles


async def test_handle_webhook_watcher_routing() -> None:
    """Watchers each get their own notification."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=["watcher-1", "watcher-2"])

    registry = MagicMock()
    agent_1 = MagicMock()
    agent_1.handle = "agent-cto"
    agent_2 = MagicMock()
    agent_2.handle = "agent-swe"
    registry.resolve_external_id.side_effect = lambda transport, ext_id: {
        "watcher-1": agent_1,
        "watcher-2": agent_2,
    }.get(ext_id)
    t.set_handle_registry(registry)

    body = _make_confluence_body()
    results = await t.handle_webhook(body)

    assert len(results) == 2
    handles = {n.recipient_handle for n in results}
    assert handles == {"agent-cto", "agent-swe"}
    assert all(n.metadata.get("routed_via") == "watcher" for n in results)


async def test_handle_webhook_mention_routing() -> None:
    """Comment with @mention routes to the mentioned agent."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])  # no watchers
    t.add_watcher = AsyncMock(return_value=True)

    registry = MagicMock()
    agent_mock = MagicMock()
    agent_mock.handle = "agent-cto"
    registry.resolve_external_id.side_effect = lambda transport, ext_id: (
        agent_mock if ext_id == "cto-id" else None
    )
    t.set_handle_registry(registry)

    body = {
        "event": "comment_created",
        "timestamp": 9999,
        "userAccountId": "human-user",
        "page": {
            "id": "100",
            "title": "Design Doc",
            "space": {"key": "ENG"},
        },
        "comment": {
            "id": "55",
            "body": {
                "storage": {
                    "value": '<p><ri:user ri:account-id="cto-id" /> please review</p>'
                },
            },
        },
    }
    results = await t.handle_webhook(body)
    assert len(results) == 1
    assert results[0].recipient_handle == "agent-cto"
    assert results[0].metadata["routed_via"] == "mention"

    # Verify auto-follow: mentioned agent should be added as watcher
    t.add_watcher.assert_awaited_once_with("100", "cto-id")


async def test_handle_webhook_mention_auto_follow_multiple() -> None:
    """Multiple mentions all get routed and auto-followed."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    t.add_watcher = AsyncMock(return_value=True)

    registry = MagicMock()
    cto = MagicMock()
    cto.handle = "agent-cto"
    swe = MagicMock()
    swe.handle = "agent-swe"
    registry.resolve_external_id.side_effect = lambda transport, ext_id: {
        "cto-id": cto,
        "swe-id": swe,
    }.get(ext_id)
    t.set_handle_registry(registry)

    body = {
        "event": "comment_created",
        "timestamp": 9999,
        "userAccountId": "human-user",
        "page": {"id": "200", "title": "RFC", "space": {"key": "ENG"}},
        "comment": {
            "id": "66",
            "body": {
                "storage": {
                    "value": (
                        '<p><ri:user ri:account-id="cto-id" /> '
                        '<ri:user ri:account-id="swe-id" /> thoughts?</p>'
                    )
                },
            },
        },
    }
    results = await t.handle_webhook(body)
    assert len(results) == 2
    handles = {n.recipient_handle for n in results}
    assert handles == {"agent-cto", "agent-swe"}

    # Both should be auto-followed
    assert t.add_watcher.await_count == 2


async def test_handle_webhook_watcher_skips_space_leads() -> None:
    """When watchers are found, space leads are NOT notified."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=["watcher-1"])

    registry = MagicMock()
    agent_mock = MagicMock()
    agent_mock.handle = "agent-swe"
    registry.resolve_external_id.side_effect = lambda transport, ext_id: (
        agent_mock if ext_id == "watcher-1" else None
    )
    t.set_handle_registry(registry)
    t.set_space_key_leads({"ENG": ["agent-ceo", "agent-cto"]})

    body = _make_confluence_body(space_key="ENG")
    results = await t.handle_webhook(body)

    # Only watcher gets it, NOT the 2 space leads
    assert len(results) == 1
    assert results[0].recipient_handle == "agent-swe"
    assert results[0].metadata["routed_via"] == "watcher"


async def test_handle_webhook_injects_page_url() -> None:
    """Shareable page URL is injected into notification metadata."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    body = _make_confluence_body(space_key="ENG", page_id="12345")

    results = await t.handle_webhook(body)
    assert len(results) >= 1
    # _CFG uses url="https://test.atlassian.net/wiki" which becomes
    # the shareable_base_url.
    expected = "https://test.atlassian.net/wiki/spaces/ENG/pages/12345"
    assert results[0].metadata.get("page_url") == expected


async def test_handle_webhook_no_page_url_without_site_url() -> None:
    """No page_url when using cloud_id without site_url."""
    cfg = ConfluenceConfig(cloud_id="abc-123", token="tok")
    t = ConfluenceTransport(cfg)
    t.fetch_watchers = AsyncMock(return_value=[])
    body = _make_confluence_body(space_key="ENG")

    results = await t.handle_webhook(body)
    assert len(results) >= 1
    assert "page_url" not in results[0].metadata


async def test_handle_webhook_trigger_only_watcher_no_fallback() -> None:
    """When trigger user is the only watcher, do NOT fall to space leads.

    The trigger user already knows about their own action. No other
    agent is watching the page, so broadcasting to space leads would
    be incorrect (e.g. SWE getting notified about a page they don't
    watch).
    """
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=["trigger-id"])
    t.set_space_key_leads({"ENG": ["agent-ceo"]})

    registry = MagicMock()
    trigger_agent = MagicMock()
    trigger_agent.handle = "agent-swe"
    registry.resolve_external_id.side_effect = lambda transport, ext_id: (
        trigger_agent if ext_id == "trigger-id" else None
    )
    t.set_handle_registry(registry)

    body = _make_confluence_body(space_key="ENG", trigger_account_id="trigger-id")
    results = await t.handle_webhook(body)

    # Trigger user is the only watcher — event is dropped entirely,
    # NOT routed to space leads.
    assert results == []


async def test_handle_webhook_unresolvable_watchers_fall_to_leads() -> None:
    """Watchers that don't resolve to agents should fall to space leads."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=["unknown-external-user"])
    t.set_space_key_leads({"ENG": ["agent-ceo"]})

    registry = MagicMock()
    registry.resolve_external_id.return_value = None  # can't resolve
    t.set_handle_registry(registry)

    body = _make_confluence_body(space_key="ENG", trigger_account_id="someone-else")
    results = await t.handle_webhook(body)

    # Watcher exists but can't resolve → fall to space leads
    assert len(results) == 1
    assert results[0].recipient_handle == "agent-ceo"
    assert results[0].metadata["routed_via"] == "space_lead"


async def test_handle_webhook_space_lead_routing() -> None:
    """Routes to space leads when no watchers or creators found."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    t.set_space_key_leads({"ENG": ["cto"]})
    body = _make_confluence_body(space_key="ENG")

    results = await t.handle_webhook(body)
    assert len(results) == 1
    assert results[0].recipient_handle == "cto"
    assert results[0].metadata["routed_via"] == "space_lead"


async def test_handle_webhook_space_multiple_leads() -> None:
    """Routes to all leads when multiple units share a space key."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    t.set_space_key_leads({"POC": ["agent-ceo", "agent-cto", "agent-pm"]})
    body = _make_confluence_body(space_key="PoC")

    results = await t.handle_webhook(body)
    assert len(results) == 3
    handles = [n.recipient_handle for n in results]
    assert handles == ["agent-ceo", "agent-cto", "agent-pm"]
    assert all(n.metadata["routed_via"] == "space_lead" for n in results)


async def test_handle_webhook_standard_fallback() -> None:
    """When no watchers, creator, or space mapping, returns for standard resolution."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    body = _make_confluence_body(space_key="UNKNOWN")

    results = await t.handle_webhook(body)
    assert len(results) == 1
    assert "routed_via" not in results[0].metadata


async def test_handle_webhook_space_lead_excludes_trigger() -> None:
    """A space lead who triggered the event is excluded from space-lead
    routing; the remaining leads still receive it.

    Without this, a lead acting in the space it leads would be the
    default recipient for its own webhook and re-trigger itself.
    """
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    t.set_space_key_leads({"LEAD": ["agent-ceo", "agent-cto"]})

    registry = MagicMock()
    ceo = MagicMock()
    ceo.handle = "agent-ceo"
    registry.resolve_external_id.side_effect = lambda transport, ext_id: (
        ceo if ext_id == "ceo-id" else None
    )
    t.set_handle_registry(registry)

    body = _make_confluence_body(space_key="LEAD", trigger_account_id="ceo-id")
    results = await t.handle_webhook(body)

    # The triggering lead (CEO) is excluded; the other lead still gets it.
    handles = [n.recipient_handle for n in results]
    assert handles == ["agent-cto"]
    assert all(n.metadata["routed_via"] == "space_lead" for n in results)


async def test_handle_webhook_space_lead_sole_trigger_dropped() -> None:
    """When the only space lead is the trigger user, the event is dropped.

    This is the self-notification loop reported for a CEO that leads the
    space it acts in: its own comment webhook must NOT route back to it,
    and it must NOT fall through to standard resolution either.
    """
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    t.set_space_key_leads({"LEAD": ["agent-ceo"]})

    registry = MagicMock()
    ceo = MagicMock()
    ceo.handle = "agent-ceo"
    registry.resolve_external_id.side_effect = lambda transport, ext_id: (
        ceo if ext_id == "ceo-id" else None
    )
    t.set_handle_registry(registry)

    body = _make_confluence_body(space_key="LEAD", trigger_account_id="ceo-id")
    results = await t.handle_webhook(body)

    assert results == []


async def test_handle_webhook_space_lead_dedupes_repeated_handle() -> None:
    """A lead handle listed twice for a space is notified only once."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    t.set_space_key_leads({"ENG": ["agent-cto", "agent-cto"]})

    body = _make_confluence_body(space_key="ENG")
    results = await t.handle_webhook(body)

    assert [n.recipient_handle for n in results] == ["agent-cto"]


async def test_handle_webhook_space_lead_non_trigger_still_routes() -> None:
    """A space lead that did NOT trigger the event still receives it.

    Guards against an over-broad exclusion that would drop legitimate
    space-lead notifications (e.g. a human edited the page).
    """
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    t.set_space_key_leads({"LEAD": ["agent-ceo"]})

    registry = MagicMock()
    registry.resolve_external_id.return_value = None  # human trigger, unknown
    t.set_handle_registry(registry)

    body = _make_confluence_body(space_key="LEAD", trigger_account_id="human-user")
    results = await t.handle_webhook(body)

    assert len(results) == 1
    assert results[0].recipient_handle == "agent-ceo"
    assert results[0].metadata["routed_via"] == "space_lead"


# --- fetch_comment_body ---


async def test_fetch_comment_body_success() -> None:
    """Fetches comment body HTML via REST API."""
    t = ConfluenceTransport(_CFG)
    mock_client = AsyncMock()
    resp = MagicMock()
    resp.status_code = 200
    resp.json.return_value = {
        "body": {
            "storage": {
                "value": '<p>Hello <ri:user ri:account-id="pm-id" /></p>',
            },
        },
    }
    mock_client.get = AsyncMock(return_value=resp)
    t._http_client = mock_client

    result = await t.fetch_comment_body("99")
    assert result == '<p>Hello <ri:user ri:account-id="pm-id" /></p>'
    mock_client.get.assert_awaited_once()


async def test_fetch_comment_body_no_client() -> None:
    t = ConfluenceTransport(_CFG)
    result = await t.fetch_comment_body("99")
    assert result == ""


async def test_fetch_comment_body_api_error() -> None:
    t = ConfluenceTransport(_CFG)
    mock_client = AsyncMock()
    mock_client.get = AsyncMock(side_effect=RuntimeError("timeout"))
    t._http_client = mock_client

    result = await t.fetch_comment_body("99")
    assert result == ""


# --- Forge comment body fetch fallback ---


async def test_handle_webhook_forge_fetches_comment_body_for_mentions() -> None:
    """When comment body is missing (Forge), fetch via API and extract mentions."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    t.add_watcher = AsyncMock(return_value=True)

    # Simulate Forge payload — comment has no body key
    body = {
        "event": "comment_created",
        "timestamp": 9999,
        "userAccountId": "human-user",
        "page": {"id": "100", "title": "Design", "space": {"key": "ENG"}},
        "comment": {
            "id": "77",
            "type": "comment",
            "status": "current",
            "title": "Re: Design",
            # No "body" key — this is what Forge sends
        },
    }

    # Mock the comment body fetch
    mock_client = AsyncMock()
    resp = MagicMock()
    resp.status_code = 200
    resp.json.return_value = {
        "body": {
            "storage": {
                "value": '<p>Hey <ri:user ri:account-id="pm-id" /> please review</p>'
            }
        }
    }
    mock_client.get = AsyncMock(return_value=resp)
    t._http_client = mock_client

    # Set up registry to resolve the mention
    registry = MagicMock()
    pm_agent = MagicMock()
    pm_agent.handle = "agent-pm"
    registry.resolve_external_id.side_effect = lambda transport, ext_id: (
        pm_agent if ext_id == "pm-id" else None
    )
    t.set_handle_registry(registry)

    results = await t.handle_webhook(body)

    # Mention should be detected and routed
    assert len(results) == 1
    assert results[0].recipient_handle == "agent-pm"
    assert results[0].metadata["routed_via"] == "mention"

    # Auto-follow should be called
    t.add_watcher.assert_awaited_once_with("100", "pm-id")


async def test_index_callback_fires_for_page_event() -> None:
    """The index callback runs for every parsed page event before
    notification routing, so the indexer reflects every change
    regardless of self-edit suppression downstream."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])

    calls: list[tuple[str, str]] = []

    async def _index_callback(event_type: str, page_id: str) -> None:
        calls.append((event_type, page_id))

    t.set_index_callback(_index_callback)
    body = _make_confluence_body(event="page_updated", page_id="42")
    await t.handle_webhook(body)
    assert calls == [("page_updated", "42")]


async def test_index_callback_failure_does_not_break_routing() -> None:
    """A raise inside the index callback is logged but never aborts
    the notification path."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])

    async def _boom(event_type: str, page_id: str) -> None:
        raise RuntimeError("indexer down")

    t.set_index_callback(_boom)
    body = _make_confluence_body()
    results = await t.handle_webhook(body)
    # Webhook still produced a (fallback) notification.
    assert len(results) >= 1


async def test_set_index_callback_to_none_disables() -> None:
    """Passing ``None`` clears the callback without otherwise
    affecting webhook handling."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])

    captured: list[str] = []

    async def _callback(event_type: str, page_id: str) -> None:
        captured.append(page_id)

    t.set_index_callback(_callback)
    t.set_index_callback(None)
    body = _make_confluence_body(event="page_updated", page_id="42")
    await t.handle_webhook(body)
    assert captured == []


async def test_notification_excluded_space_short_circuits_routing() -> None:
    """Webhook for an excluded space fires the index callback but
    produces zero notifications — so engine-managed spaces (Tool Skills)
    don't surface as ``notification_undeliverable``.
    """
    t = ConfluenceTransport(_CFG)
    # Watcher fetch must never be reached when the space is excluded;
    # fail loudly if it is.
    t.fetch_watchers = AsyncMock(
        side_effect=AssertionError("excluded space should not fetch watchers")
    )

    calls: list[tuple[str, str]] = []

    async def _index_callback(event_type: str, page_id: str) -> None:
        calls.append((event_type, page_id))

    t.set_index_callback(_index_callback)
    t.set_notification_excluded_spaces(["TS"])

    body = _make_confluence_body(event="page_updated", page_id="42", space_key="TS")
    results = await t.handle_webhook(body)

    assert results == []
    # Index callback still ran — the registry update path is unaffected.
    assert calls == [("page_updated", "42")]


async def test_notification_excluded_space_match_is_case_insensitive() -> None:
    """Space-key matching uses uppercase normalisation, matching
    ``set_space_key_leads`` so config and webhook payloads can disagree
    on case without breaking the suppression."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(
        side_effect=AssertionError("excluded space should not fetch watchers")
    )
    t.set_notification_excluded_spaces(["ts"])  # lowercase in config

    body = _make_confluence_body(space_key="TS")  # uppercase in webhook
    results = await t.handle_webhook(body)
    assert results == []


async def test_notification_excluded_space_does_not_affect_other_spaces() -> None:
    """Other spaces continue to route normally when an excluded space
    is configured."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    t.set_notification_excluded_spaces(["TS"])

    body = _make_confluence_body(space_key="ENG")
    results = await t.handle_webhook(body)
    # ENG isn't excluded → standard fallback notification.
    assert len(results) == 1


async def test_handle_webhook_forge_no_mentions_in_fetched_body() -> None:
    """Forge comment body fetched but has no mentions — falls through normally."""
    t = ConfluenceTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])

    body = {
        "event": "comment_created",
        "timestamp": 8888,
        "page": {"id": "200", "title": "Notes", "space": {"key": "UNKNOWN"}},
        "comment": {"id": "88", "type": "comment"},
    }

    mock_client = AsyncMock()
    resp = MagicMock()
    resp.status_code = 200
    resp.json.return_value = {
        "body": {"storage": {"value": "<p>Just a plain comment</p>"}}
    }
    mock_client.get = AsyncMock(return_value=resp)
    t._http_client = mock_client

    results = await t.handle_webhook(body)

    # No mentions, no watchers, no space leads → standard fallback
    assert len(results) == 1
    assert "routed_via" not in results[0].metadata
    # But body should be populated from the fetch
    assert results[0].body == "<p>Just a plain comment</p>"
