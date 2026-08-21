"""Tests for JiraTransport — outbound + webhook parsing."""

from unittest.mock import AsyncMock, MagicMock

import pytest

from crewlet.config import JiraConfig
from crewlet.notifications.protocol import InboundNotification
from crewlet.notifications.transports.jira import JiraTransport

# Default admin config used across tests
_CFG = JiraConfig(
    url="https://test.atlassian.net",
    token="test-admin-token",
)

_CFG_WITH_SECRET = JiraConfig(
    url="https://test.atlassian.net",
    token="test-admin-token",
    webhook_secret="test-secret",
)


@pytest.fixture
def transport() -> JiraTransport:
    return JiraTransport(_CFG)


# --- Lifecycle ---


async def test_start_stop(transport: JiraTransport) -> None:
    await transport.start()
    assert transport._http_client is not None

    await transport.stop()
    assert transport._http_client is None


async def test_name() -> None:
    t = JiraTransport(_CFG)
    assert t.name == "jira"


async def test_send_not_supported() -> None:
    transport = JiraTransport(_CFG)
    await transport.start()

    from crewlet.notifications.protocol import OutboundMessage

    msg = OutboundMessage(transport="jira", sender_handle="eng", recipient="PROJ-1")
    result = await transport.send(msg)
    assert result is False

    await transport.stop()


async def test_dedupe_defaults_to_a_process_local_store() -> None:
    """Correct for one process, and what the engine replaces with a
    shared store the moment a database is configured — a retry landing on
    a peer node would otherwise be a fresh delivery there."""
    from crewlet.db.deliveries import MemoryDeliveryDedupeStore

    t = JiraTransport(_CFG)
    assert isinstance(t._dedupe, MemoryDeliveryDedupeStore)

    shared = MemoryDeliveryDedupeStore()
    t.set_delivery_dedupe(shared)
    assert t._dedupe is shared


# --- fetch_watchers ---


async def test_fetch_watchers_returns_account_ids() -> None:
    t = JiraTransport(_CFG)
    mock_client = AsyncMock()
    mock_response = MagicMock()
    mock_response.json.return_value = {
        "watchers": [
            {"accountId": "user-1", "displayName": "Alice"},
            {"accountId": "user-2", "displayName": "Bob"},
        ]
    }
    mock_response.raise_for_status = MagicMock()
    mock_client.get = AsyncMock(return_value=mock_response)
    t._http_client = mock_client

    result = await t.fetch_watchers("PROJ-1")
    assert result == ["user-1", "user-2"]
    mock_client.get.assert_awaited_once_with(
        "https://test.atlassian.net/rest/api/3/issue/PROJ-1/watchers"
    )


async def test_fetch_watchers_cloud_id_url() -> None:
    """cloud_id config produces correct full URL for watcher fetch."""
    cfg = JiraConfig(cloud_id="abc-cloud-123", token="tok")
    t = JiraTransport(cfg)
    mock_client = AsyncMock()
    mock_response = MagicMock()
    mock_response.json.return_value = {"watchers": [{"accountId": "u1"}]}
    mock_response.raise_for_status = MagicMock()
    mock_client.get = AsyncMock(return_value=mock_response)
    t._http_client = mock_client

    result = await t.fetch_watchers("PROJ-1")
    assert result == ["u1"]
    mock_client.get.assert_awaited_once_with(
        "https://api.atlassian.com/ex/jira/abc-cloud-123"
        "/rest/api/3/issue/PROJ-1/watchers"
    )


async def test_fetch_watchers_ignores_entries_without_account_id() -> None:
    """Entries missing accountId are skipped."""
    t = JiraTransport(_CFG)
    mock_client = AsyncMock()
    mock_response = MagicMock()
    mock_response.json.return_value = {
        "watchers": [
            {"accountId": "user-1"},
            {"key": "JIRAUSER10001", "name": "alice", "displayName": "Alice"},
        ]
    }
    mock_response.raise_for_status = MagicMock()
    mock_client.get = AsyncMock(return_value=mock_response)
    t._http_client = mock_client

    result = await t.fetch_watchers("PROJ-1")
    assert result == ["user-1"]


async def test_fetch_watchers_before_start() -> None:
    """fetch_watchers before start() returns empty (no client yet)."""
    t = JiraTransport(_CFG)
    result = await t.fetch_watchers("PROJ-1")
    assert result == []


async def test_fetch_watchers_api_error() -> None:
    t = JiraTransport(_CFG)
    mock_client = AsyncMock()
    mock_client.get = AsyncMock(side_effect=RuntimeError("connection error"))
    t._http_client = mock_client

    result = await t.fetch_watchers("PROJ-1")
    assert result == []


async def test_fetch_watchers_empty_list() -> None:
    t = JiraTransport(_CFG)
    mock_client = AsyncMock()
    mock_response = MagicMock()
    mock_response.json.return_value = {"watchers": []}
    mock_response.raise_for_status = MagicMock()
    mock_client.get = AsyncMock(return_value=mock_response)
    t._http_client = mock_client

    result = await t.fetch_watchers("PROJ-1")
    assert result == []


# --- Webhook signature verification ---


def test_verify_webhook_no_secret() -> None:
    """No webhook_secret configured → always passes."""
    t = JiraTransport(_CFG)
    assert t.verify_webhook(b'{"test": true}', {"x-hub-signature": "anything"})


def test_verify_webhook_valid_signature() -> None:
    import hashlib
    import hmac

    t = JiraTransport(_CFG_WITH_SECRET)
    body = b'{"webhookEvent": "jira:issue_updated"}'
    sig = "sha256=" + hmac.new(b"test-secret", body, hashlib.sha256).hexdigest()
    assert t.verify_webhook(body, {"x-hub-signature": sig})


def test_verify_webhook_invalid_signature() -> None:
    t = JiraTransport(_CFG_WITH_SECRET)
    assert not t.verify_webhook(b'{"test": true}', {"x-hub-signature": "sha256=bad"})


def test_verify_webhook_missing_header() -> None:
    t = JiraTransport(_CFG_WITH_SECRET)
    assert not t.verify_webhook(b'{"test": true}', {})


# --- Event deduplication ---


async def test_dedup_first_event_not_duplicate() -> None:
    t = JiraTransport(_CFG)
    body = {"timestamp": 123, "issue": {"key": "PROJ-1"}, "webhookEvent": "updated"}
    assert not await t._dedup_event(body)


async def test_dedup_same_event_is_duplicate() -> None:
    t = JiraTransport(_CFG)
    body = {"timestamp": 123, "issue": {"key": "PROJ-1"}, "webhookEvent": "updated"}
    assert not await t._dedup_event(body)
    assert await t._dedup_event(body)


async def test_dedup_different_events_not_duplicate() -> None:
    t = JiraTransport(_CFG)
    body1 = {"timestamp": 123, "issue": {"key": "PROJ-1"}, "webhookEvent": "updated"}
    body2 = {"timestamp": 456, "issue": {"key": "PROJ-2"}, "webhookEvent": "created"}
    assert not await t._dedup_event(body1)
    assert not await t._dedup_event(body2)


# --- handle_webhook ---


def _make_jira_body(
    *,
    event: str = "jira:issue_updated",
    issue_key: str = "PROJ-1",
    assignee_id: str = "",
    project: str = "PROJ",
    trigger_account_id: str = "",
    comment_author_account_id: str = "",
    comment_author_display_name: str = "",
) -> dict:
    """Build a minimal Jira webhook body."""
    assignee = None
    if assignee_id:
        assignee = {"accountId": assignee_id, "displayName": "Test User"}
    body: dict = {
        "webhookEvent": event,
        "timestamp": 1234567890,
        "issue": {
            "key": issue_key,
            "fields": {
                "summary": f"Test issue {issue_key}",
                "assignee": assignee,
                "project": {"key": project},
            },
        },
    }
    if trigger_account_id:
        body["user"] = {"accountId": trigger_account_id}
    if comment_author_account_id or comment_author_display_name:
        body["comment"] = {
            "body": "a comment",
            "author": {
                "accountId": comment_author_account_id,
                "displayName": comment_author_display_name or "Commenter",
            },
        }
    return body


async def test_handle_webhook_basic() -> None:
    """Basic webhook produces at least one notification."""
    t = JiraTransport(_CFG)
    # Stub fetch_watchers to return empty
    t.fetch_watchers = AsyncMock(return_value=[])
    body = _make_jira_body(assignee_id="user-1")

    results = await t.handle_webhook(body)
    assert len(results) >= 1
    assert all(isinstance(n, InboundNotification) for n in results)


async def test_handle_webhook_signature_rejected() -> None:
    """Invalid signature returns empty list."""
    t = JiraTransport(_CFG_WITH_SECRET)
    body = _make_jira_body()
    results = await t.handle_webhook(body, headers={"x-hub-signature": "bad"})
    assert results == []


async def test_handle_webhook_signature_no_headers() -> None:
    """Missing headers when secret configured returns empty list."""
    t = JiraTransport(_CFG_WITH_SECRET)
    body = _make_jira_body()
    results = await t.handle_webhook(body, headers=None)
    assert results == []


async def test_handle_webhook_dedup() -> None:
    """Duplicate event returns empty list."""
    t = JiraTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    body = _make_jira_body()

    r1 = await t.handle_webhook(body)
    assert len(r1) >= 1

    r2 = await t.handle_webhook(body)
    assert r2 == []


async def test_handle_webhook_trigger_user_excluded_from_watchers() -> None:
    """Trigger user is excluded from watcher routing but others still get it."""
    t = JiraTransport(_CFG)
    # Agent triggers the event and is also a watcher.
    # Another agent is also a watcher — they should still get it.
    t.fetch_watchers = AsyncMock(return_value=["our-agent-id", "other-watcher-id"])

    results = await t.handle_webhook(_make_jira_body(trigger_account_id="our-agent-id"))
    # Only other-watcher-id should be in results (trigger excluded)
    assert len(results) == 1
    assert results[0].metadata["assignee_account_id"] == "other-watcher-id"
    assert results[0].metadata["routed_via"] == "watcher"


async def test_handle_webhook_trigger_only_watcher_falls_to_lead() -> None:
    """When trigger user is the only watcher, falls to project lead."""
    t = JiraTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=["our-agent-id"])
    t.set_project_key_leads({"PROJ": "tech-lead"})

    results = await t.handle_webhook(_make_jira_body(trigger_account_id="our-agent-id"))
    lead_notifs = [
        n for n in results if n.metadata.get("routed_via") == "project_lead_fallback"
    ]
    assert len(lead_notifs) == 1
    assert lead_notifs[0].recipient_handle == "tech-lead"


async def test_handle_webhook_routes_to_adf_mentioned_users() -> None:
    """Comments with ADF mention nodes must fan out to the mentioned
    users even when they are NOT in Jira's watcher list -- Jira does
    not auto-add @-mentioned users as watchers.  Without this fan-out
    the agent who got @'d would never receive the notification."""
    t = JiraTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])

    body = _make_jira_body(
        event="comment_created",
        trigger_account_id="commenter-id",
    )
    body["comment"] = {
        "body": {
            "type": "doc",
            "version": 1,
            "content": [
                {
                    "type": "paragraph",
                    "content": [
                        {"type": "text", "text": "Hey "},
                        {
                            "type": "mention",
                            "attrs": {"id": "mentioned-swe-id", "text": "@SWE"},
                        },
                        {
                            "type": "text",
                            "text": " — could you take a look?",
                        },
                    ],
                }
            ],
        },
        "author": {"accountId": "commenter-id"},
    }

    results = await t.handle_webhook(body)
    mention_notifs = [n for n in results if n.metadata.get("routed_via") == "mention"]
    assert len(mention_notifs) == 1
    assert mention_notifs[0].metadata["assignee_account_id"] == "mentioned-swe-id"


async def test_handle_webhook_mention_skips_trigger_and_dedups() -> None:
    """When the mentioned user is also the comment author (or the
    same person as a watcher), they are not re-notified.  Self-
    mentions and overlapping watcher/mention IDs both collapse to a
    single notification (or zero, if it's a self-mention)."""
    t = JiraTransport(_CFG)
    # ``alice-id`` is in the watcher list AND mentioned in the body.
    t.fetch_watchers = AsyncMock(return_value=["alice-id"])

    body = _make_jira_body(
        event="comment_created",
        trigger_account_id="commenter-id",
    )
    body["comment"] = {
        "body": {
            "type": "doc",
            "version": 1,
            "content": [
                {
                    "type": "paragraph",
                    "content": [
                        # Self-mention -- must be skipped.
                        {
                            "type": "mention",
                            "attrs": {"id": "commenter-id"},
                        },
                        # Already a watcher -- must not get a duplicate.
                        {
                            "type": "mention",
                            "attrs": {"id": "alice-id"},
                        },
                    ],
                }
            ],
        },
        "author": {"accountId": "commenter-id"},
    }

    results = await t.handle_webhook(body)
    routed_ids = [n.metadata.get("assignee_account_id") for n in results]
    # ``alice-id`` shows up exactly once (via watcher), ``commenter-id`` not at all.
    assert routed_ids.count("alice-id") == 1
    assert "commenter-id" not in routed_ids


async def test_handle_webhook_watcher_routing() -> None:
    """Watchers each get their own notification."""
    t = JiraTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=["watcher-1", "watcher-2"])
    body = _make_jira_body(assignee_id="watcher-1")

    results = await t.handle_webhook(body)
    # watcher-1, watcher-2 — assignee is watcher-1 so not duplicated
    assert len(results) == 2
    routed_ids = [n.metadata.get("assignee_account_id") for n in results]
    assert "watcher-1" in routed_ids
    assert "watcher-2" in routed_ids
    assert all(n.metadata.get("routed_via") == "watcher" for n in results)


async def test_handle_webhook_assignee_fallback() -> None:
    """Assignee gets notification if not a watcher."""
    t = JiraTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=["watcher-1"])
    body = _make_jira_body(assignee_id="assignee-1")

    results = await t.handle_webhook(body)
    # watcher-1 (routed_via=watcher) + assignee-1 (original)
    assert len(results) == 2


async def test_handle_webhook_project_lead_fallback() -> None:
    """Falls back to project lead when no meaningful recipients."""
    t = JiraTransport(_CFG)
    # No watchers, no assignee, but trigger user is the creator watcher
    t.fetch_watchers = AsyncMock(return_value=["creator-id"])
    t.set_project_key_leads({"PROJ": "tech-lead"})
    body = _make_jira_body(trigger_account_id="creator-id")

    results = await t.handle_webhook(body)
    lead_notifs = [
        n for n in results if n.metadata.get("routed_via") == "project_lead_fallback"
    ]
    assert len(lead_notifs) == 1
    assert lead_notifs[0].recipient_handle == "tech-lead"


async def test_handle_webhook_standard_fallback() -> None:
    """When no watchers/assignee/lead, returns original notification."""
    t = JiraTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    body = _make_jira_body()

    results = await t.handle_webhook(body)
    assert len(results) == 1
    # No routed_via metadata — standard fallback
    assert "routed_via" not in results[0].metadata
