"""Tests for JiraTransport watcher-based routing via handle_webhook().

handle_webhook() returns a list of InboundNotification objects. The API
layer publishes each one to the EventQueue for the NotificationService
to route.
"""

from __future__ import annotations

from unittest.mock import AsyncMock

import pytest

from crewlet.config import JiraConfig
from crewlet.notifications.transports.jira import JiraTransport

_CFG = JiraConfig(
    url="https://test.atlassian.net",
    token="test-admin-token",
)


def _make_body(
    *,
    issue_key: str = "PROJ-1",
    assignee_id: str = "",
    project: str = "PROJ",
    trigger_id: str = "",
) -> dict:
    assignee = {"accountId": assignee_id} if assignee_id else None
    body: dict = {
        "webhookEvent": "jira:issue_updated",
        "timestamp": 9999,
        "issue": {
            "key": issue_key,
            "fields": {
                "summary": f"Issue {issue_key}",
                "assignee": assignee,
                "project": {"key": project},
            },
        },
    }
    if trigger_id:
        body["user"] = {"accountId": trigger_id}
    return body


@pytest.fixture
def transport() -> JiraTransport:
    t = JiraTransport(_CFG)
    t.fetch_watchers = AsyncMock(return_value=[])
    return t


class TestWatcherRouting:
    @pytest.mark.asyncio
    async def test_watchers_each_get_notification(self, transport):
        transport.fetch_watchers = AsyncMock(return_value=["w1", "w2", "w3"])
        results = await transport.handle_webhook(_make_body())
        watcher_notifs = [
            n for n in results if n.metadata.get("routed_via") == "watcher"
        ]
        assert len(watcher_notifs) == 3
        ids = {n.metadata["assignee_account_id"] for n in watcher_notifs}
        assert ids == {"w1", "w2", "w3"}

    @pytest.mark.asyncio
    async def test_assignee_not_duplicated_if_watcher(self, transport):
        """If assignee is also a watcher, only one notification."""
        transport.fetch_watchers = AsyncMock(return_value=["user-1", "user-2"])
        body = _make_body(assignee_id="user-1")
        results = await transport.handle_webhook(body)
        # user-1 as watcher + user-2 as watcher = 2, not 3
        assert len(results) == 2

    @pytest.mark.asyncio
    async def test_assignee_added_when_not_watcher(self, transport):
        transport.fetch_watchers = AsyncMock(return_value=["w1"])
        body = _make_body(assignee_id="assignee-only")
        results = await transport.handle_webhook(body)
        # w1 (watcher) + assignee-only
        assert len(results) == 2

    @pytest.mark.asyncio
    async def test_project_lead_fallback_when_only_creator_watching(self, transport):
        """Creator-as-watcher is not meaningful → fall back to lead."""
        transport.fetch_watchers = AsyncMock(return_value=["creator-id"])
        transport.set_project_key_leads({"PROJ": "lead-handle"})

        body = _make_body(trigger_id="creator-id")
        results = await transport.handle_webhook(body)

        lead = [
            n
            for n in results
            if n.metadata.get("routed_via") == "project_lead_fallback"
        ]
        assert len(lead) == 1
        assert lead[0].recipient_handle == "lead-handle"

    @pytest.mark.asyncio
    async def test_no_fallback_when_meaningful_watcher(self, transport):
        """Real watcher (not just the creator) → no lead fallback."""
        transport.fetch_watchers = AsyncMock(
            return_value=["creator-id", "real-watcher"]
        )
        transport.set_project_key_leads({"PROJ": "lead-handle"})

        body = _make_body(trigger_id="creator-id")
        results = await transport.handle_webhook(body)

        lead = [
            n
            for n in results
            if n.metadata.get("routed_via") == "project_lead_fallback"
        ]
        assert len(lead) == 0

    @pytest.mark.asyncio
    async def test_standard_fallback_when_no_watchers_no_assignee(self, transport):
        """No watchers, no assignee → return original notification."""
        transport.fetch_watchers = AsyncMock(return_value=[])
        body = _make_body()
        results = await transport.handle_webhook(body)
        assert len(results) == 1
        assert "routed_via" not in results[0].metadata

    @pytest.mark.asyncio
    async def test_trigger_user_excluded_from_watchers(self, transport):
        """Trigger user excluded from routing, others still get it."""
        transport.fetch_watchers = AsyncMock(return_value=["our-agent-id", "other-id"])

        body = _make_body(trigger_id="our-agent-id")
        results = await transport.handle_webhook(body)
        # Only other-id should be routed (trigger excluded)
        watcher_ids = [
            n.metadata.get("assignee_account_id")
            for n in results
            if n.metadata.get("routed_via") == "watcher"
        ]
        assert "our-agent-id" not in watcher_ids
        assert "other-id" in watcher_ids

    @pytest.mark.asyncio
    async def test_self_ignore_only_when_registry_set(self, transport):
        """Without handle_registry, self-ignore is skipped."""
        body = _make_body(trigger_id="some-id")
        results = await transport.handle_webhook(body)
        # Should still process (no registry to check against)
        assert len(results) >= 1
