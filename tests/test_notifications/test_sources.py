"""Tests for built-in notification sources."""

import asyncio
import json

import pytest

from crewlet.notifications.protocol import InboundNotification
from crewlet.notifications.sources import (
    PollingSource,
    WebhookSource,
    parse_github_webhook,
    parse_jira_webhook,
)  # noqa: I001

# --- WebhookSource tests ---


class TestWebhookSource:
    @pytest.mark.asyncio
    async def test_handle_webhook_with_parser(self):
        source = WebhookSource()
        received: list[InboundNotification] = []

        async def parser(name, body, headers):
            return InboundNotification(
                source=name,
                recipient_email=body.get("email", ""),
                subject=body.get("subject", ""),
            )

        source.register_parser("test", parser)

        async def callback(n: InboundNotification):
            received.append(n)

        await source.start(callback)
        result = await source.handle_webhook(
            "test",
            {"email": "alice@test.com", "subject": "Hello"},
        )

        assert result is True
        assert len(received) == 1
        assert received[0].recipient_email == "alice@test.com"

    @pytest.mark.asyncio
    async def test_handle_webhook_no_parser(self):
        source = WebhookSource()

        async def callback(n: InboundNotification):
            pass

        await source.start(callback)
        result = await source.handle_webhook("unknown", {})
        assert result is False

    @pytest.mark.asyncio
    async def test_handle_webhook_before_start_queues(self):
        source = WebhookSource()
        received: list[InboundNotification] = []

        async def parser(name, body, headers):
            return InboundNotification(source=name, subject="queued")

        source.register_parser("test", parser)

        # Handle before start — should be queued
        await source.handle_webhook("test", {})

        async def callback(n: InboundNotification):
            received.append(n)

        await source.start(callback)
        assert len(received) == 1
        assert received[0].subject == "queued"

    @pytest.mark.asyncio
    async def test_handle_webhook_parser_returns_none(self):
        source = WebhookSource()
        received: list[InboundNotification] = []

        async def parser(name, body, headers):
            return None

        source.register_parser("test", parser)

        async def callback(n: InboundNotification):
            received.append(n)

        await source.start(callback)
        result = await source.handle_webhook("test", {})
        assert result is False
        assert len(received) == 0

    @pytest.mark.asyncio
    async def test_stop(self):
        source = WebhookSource()

        async def callback(n: InboundNotification):
            pass

        await source.start(callback)
        await source.stop()
        # Should not crash if called after stop

    @pytest.mark.asyncio
    async def test_handle_webhook_parser_exception(self):
        """A parser that raises is caught and returns False."""
        source = WebhookSource()

        async def bad_parser(name, body, headers):
            raise ValueError("parse error")

        source.register_parser("bad", bad_parser)

        async def callback(n: InboundNotification):
            pass

        await source.start(callback)
        result = await source.handle_webhook("bad", {"data": "test"})
        assert result is False

    @pytest.mark.asyncio
    async def test_handle_webhook_callback_exception(self):
        """Callback exception is caught and returns False (delivery failed)."""
        source = WebhookSource()

        async def parser(name, body, headers):
            return InboundNotification(source=name, subject="boom")

        source.register_parser("test", parser)

        async def bad_callback(n: InboundNotification):
            raise RuntimeError("callback failed")

        await source.start(bad_callback)
        # Should not raise, but should return False to signal failure
        result = await source.handle_webhook("test", {})
        assert result is False

    @pytest.mark.asyncio
    async def test_handle_webhook_after_stop_drops(self):
        """Webhooks after stop() are dropped, not queued."""
        source = WebhookSource()

        async def parser(name, body, headers):
            return InboundNotification(source=name, subject="dropped")

        source.register_parser("test", parser)

        async def callback(n: InboundNotification):
            pass

        await source.start(callback)
        await source.stop()

        result = await source.handle_webhook("test", {})
        assert result is True  # parsing succeeded
        assert len(source._pending) == 0  # but not queued

    @pytest.mark.asyncio
    async def test_pending_queue_capped(self):
        """Pending queue drops oldest when exceeding max size."""
        source = WebhookSource()

        async def parser(name, body, headers):
            return InboundNotification(
                source=name,
                subject=f"item-{body.get('i', 0)}",
            )

        source.register_parser("test", parser)

        # Fill beyond cap
        for i in range(source._MAX_PENDING + 5):
            await source.handle_webhook("test", {"i": i})

        assert len(source._pending) == source._MAX_PENDING
        # Oldest items should have been dropped
        assert source._pending[0].subject == "item-5"
        assert source._pending[-1].subject == f"item-{source._MAX_PENDING + 4}"

    @pytest.mark.asyncio
    async def test_handle_webhook_none_headers(self):
        """Passing headers=None defaults to empty dict for parser."""
        source = WebhookSource()
        received_headers: list[dict] = []

        async def parser(name, body, headers):
            received_headers.append(headers)
            return InboundNotification(source=name, subject="test")

        source.register_parser("test", parser)

        async def callback(n: InboundNotification):
            pass

        await source.start(callback)
        result = await source.handle_webhook("test", {"key": "val"}, headers=None)
        assert result is True
        assert received_headers[0] == {}


# --- Jira parser tests ---


class TestJiraParser:
    @pytest.mark.asyncio
    async def test_parse_issue_assigned(self):
        body = {
            "webhookEvent": "jira:issue_updated",
            "user": {"displayName": "Manager"},
            "issue": {
                "key": "PROJ-42",
                "id": "10042",
                "fields": {
                    "summary": "Fix login bug",
                    "description": "Users can't log in",
                    "assignee": {"emailAddress": "alice@test.com"},
                    "project": {"key": "PROJ"},
                },
            },
        }
        result = await parse_jira_webhook("jira", body, {})
        assert result is not None
        assert result.recipient_email == "alice@test.com"
        assert result.sender == "Manager"
        assert "PROJ-42" in result.subject
        assert result.metadata["issue_key"] == "PROJ-42"
        assert result.metadata["project"] == "PROJ"

    @pytest.mark.asyncio
    async def test_parse_assignee_account_id(self):
        """Jira Cloud: emailAddress missing, accountId present."""
        body = {
            "webhookEvent": "jira:issue_updated",
            "user": {"displayName": "Manager"},
            "issue": {
                "key": "PROJ-99",
                "id": "10099",
                "fields": {
                    "summary": "Cloud task",
                    "description": "No email in payload",
                    "assignee": {
                        "accountId": "712020:00000000-0000-0000-0000-000000000003",
                        "displayName": "Alice",
                    },
                    "project": {"key": "PROJ"},
                },
            },
        }
        result = await parse_jira_webhook("jira", body, {})
        assert result is not None
        assert result.recipient_email == ""
        assert (
            result.metadata["assignee_account_id"]
            == "712020:00000000-0000-0000-0000-000000000003"
        )

    @pytest.mark.asyncio
    async def test_parse_assignee_with_email_and_account_id(self):
        """Both emailAddress and accountId present — both captured."""
        body = {
            "webhookEvent": "jira:issue_updated",
            "user": {"displayName": "Manager"},
            "issue": {
                "key": "PROJ-50",
                "id": "10050",
                "fields": {
                    "summary": "Dual fields",
                    "assignee": {
                        "emailAddress": "alice@test.com",
                        "accountId": "712020:abc",
                    },
                    "project": {"key": "PROJ"},
                },
            },
        }
        result = await parse_jira_webhook("jira", body, {})
        assert result is not None
        assert result.recipient_email == "alice@test.com"
        assert result.metadata["assignee_account_id"] == "712020:abc"

    @pytest.mark.asyncio
    async def test_parse_comment(self):
        body = {
            "webhookEvent": "comment_created",
            "user": {"displayName": "Reviewer"},
            "issue": {
                "key": "PROJ-10",
                "id": "10010",
                "fields": {
                    "summary": "Add feature",
                    "assignee": {"emailAddress": "bob@test.com"},
                    "project": {"key": "PROJ"},
                },
            },
            "comment": {"body": "Looks good, but needs tests"},
        }
        result = await parse_jira_webhook("jira", body, {})
        assert result is not None
        assert result.recipient_email == "bob@test.com"
        assert result.body == "Looks good, but needs tests"

    @pytest.mark.asyncio
    async def test_parse_comment_adf_body_is_flattened(self):
        """Jira Cloud sends ``comment.body`` as ADF (a JSON document
        tree), not a string.  The parser must flatten it to plain
        text -- otherwise Pydantic rejects the assignment to
        ``InboundNotification.body: str`` and the entire webhook is
        dropped, making comment webhooks
        invisible end-to-end."""
        adf_body = {
            "type": "doc",
            "version": 1,
            "content": [
                {
                    "type": "paragraph",
                    "content": [
                        {"type": "text", "text": "Hi "},
                        {
                            "type": "mention",
                            "attrs": {
                                "id": "712020:abc",
                                "text": "@Alice",
                                "displayName": "Alice",
                            },
                        },
                        {"type": "text", "text": " — please review."},
                    ],
                },
                {
                    "type": "paragraph",
                    "content": [
                        {"type": "text", "text": "Thanks!"},
                    ],
                },
            ],
        }
        body = {
            "webhookEvent": "comment_created",
            "issue": {
                "key": "PROJ-12",
                "id": "10012",
                "fields": {
                    "summary": "Needs review",
                    "assignee": {"emailAddress": "bob@test.com"},
                    "project": {"key": "PROJ"},
                },
            },
            "comment": {
                "body": adf_body,
                "author": {"accountId": "712020:abc", "displayName": "Alice"},
            },
        }
        result = await parse_jira_webhook("jira", body, {})
        assert result is not None
        assert "Hi @Alice — please review." in result.body
        assert "Thanks!" in result.body

    @pytest.mark.asyncio
    async def test_parse_description_adf_is_flattened(self):
        """The same ADF coercion must apply to ``fields.description``
        on issue events; older code passed the raw dict through and
        Pydantic rejected the notification."""
        body = {
            "webhookEvent": "jira:issue_updated",
            "issue": {
                "key": "PROJ-13",
                "id": "10013",
                "fields": {
                    "summary": "Some issue",
                    "assignee": {"emailAddress": "bob@test.com"},
                    "project": {"key": "PROJ"},
                    "description": {
                        "type": "doc",
                        "version": 1,
                        "content": [
                            {
                                "type": "paragraph",
                                "content": [{"type": "text", "text": "Body line."}],
                            }
                        ],
                    },
                },
            },
        }
        result = await parse_jira_webhook("jira", body, {})
        assert result is not None
        assert "Body line." in result.body

    @pytest.mark.asyncio
    async def test_parse_comment_created_no_top_level_user(self):
        """comment_created with no top-level user falls back to comment.author."""
        body = {
            "webhookEvent": "comment_created",
            "issue": {
                "key": "PROJ-11",
                "id": "10011",
                "fields": {
                    "summary": "Needs review",
                    "assignee": {"emailAddress": "bob@test.com"},
                    "project": {"key": "PROJ"},
                },
            },
            "comment": {
                "body": "Done, please check",
                "author": {
                    "accountId": "712020:abc",
                    "displayName": "Alice",
                },
            },
        }
        result = await parse_jira_webhook("jira", body, {})
        assert result is not None
        assert result.sender == "Alice"
        assert result.metadata["actor_name"] == "Alice"
        assert result.body == "Done, please check"

    @pytest.mark.asyncio
    async def test_parse_changelog_items(self):
        """issue_updated with changelog — items stored in metadata."""
        body = {
            "webhookEvent": "jira:issue_updated",
            "user": {"displayName": "Manager"},
            "issue": {
                "key": "PROJ-77",
                "id": "10077",
                "fields": {
                    "summary": "Status change",
                    "assignee": {"emailAddress": "alice@test.com"},
                    "project": {"key": "PROJ"},
                },
            },
            "changelog": {
                "items": [
                    {
                        "field": "status",
                        "fieldtype": "jira",
                        "from": "10000",
                        "fromString": "To Do",
                        "to": "10001",
                        "toString": "In Progress",
                    },
                    {
                        "field": "assignee",
                        "fieldtype": "jira",
                        "from": None,
                        "fromString": None,
                        "to": "alice123",
                        "toString": "Alice",
                    },
                ]
            },
        }
        result = await parse_jira_webhook("jira", body, {})
        assert result is not None
        assert "changelog" in result.metadata
        items = json.loads(result.metadata["changelog"])
        assert len(items) == 2
        assert items[0]["field"] == "status"
        assert items[0]["fromString"] == "To Do"
        assert items[0]["toString"] == "In Progress"
        assert items[1]["field"] == "assignee"

    @pytest.mark.asyncio
    async def test_parse_no_changelog(self):
        """Webhook without changelog — metadata has no changelog key."""
        body = {
            "webhookEvent": "jira:issue_created",
            "user": {"displayName": "Manager"},
            "issue": {
                "key": "PROJ-78",
                "id": "10078",
                "fields": {
                    "summary": "New task",
                    "assignee": {"emailAddress": "alice@test.com"},
                    "project": {"key": "PROJ"},
                },
            },
        }
        result = await parse_jira_webhook("jira", body, {})
        assert result is not None
        assert "changelog" not in result.metadata


# --- GitHub parser tests ---


class TestGitHubParser:
    @pytest.mark.asyncio
    async def test_parse_pr_review_requested(self):
        body = {
            "action": "review_requested",
            "sender": {"login": "maintainer"},
            "pull_request": {
                "title": "Add auth",
                "body": "JWT implementation",
                "number": 42,
            },
            "requested_reviewer": {"email": "alice@test.com"},
            "repository": {"full_name": "org/repo"},
        }
        headers = {"x-github-event": "pull_request"}
        result = await parse_github_webhook("github", body, headers)
        assert result is not None
        assert result.recipient_email == "alice@test.com"
        assert result.sender == "maintainer"
        assert result.subject == "Add auth"
        assert result.source_event_type == "pull_request.review_requested"
        assert result.metadata["pr_number"] == "42"

    @pytest.mark.asyncio
    async def test_parse_issue_assigned(self):
        body = {
            "action": "assigned",
            "sender": {"login": "pm"},
            "issue": {
                "title": "Fix bug",
                "body": "It's broken",
                "number": 99,
            },
            "assignee": {"email": "bob@test.com"},
            "repository": {"full_name": "org/repo"},
        }
        headers = {"x-github-event": "issues"}
        result = await parse_github_webhook("github", body, headers)
        assert result is not None
        assert result.recipient_email == "bob@test.com"
        assert result.subject == "Fix bug"
        assert result.source_event_type == "issues.assigned"

    @pytest.mark.asyncio
    async def test_parse_issue_comment(self):
        body = {
            "action": "created",
            "sender": {"login": "reviewer"},
            "issue": {
                "title": "Feature request",
                "number": 5,
                "assignee": {"email": "alice@test.com"},
            },
            "comment": {"body": "What about edge cases?"},
            "repository": {"full_name": "org/repo"},
        }
        headers = {"x-github-event": "issue_comment"}
        result = await parse_github_webhook("github", body, headers)
        assert result is not None
        assert result.recipient_email == "alice@test.com"
        assert result.body == "What about edge cases?"

    @pytest.mark.asyncio
    async def test_parse_pr_opened_falls_back_to_assignee(self):
        """PR opened events should extract assignee from pull_request.assignees."""
        body = {
            "action": "opened",
            "sender": {"login": "copilot-swe-agent"},
            "pull_request": {
                "title": "Add hello world",
                "body": "Hello world script",
                "number": 1,
                "assignees": [{"login": "agent-swe-user", "email": "swe@test.com"}],
                "requested_reviewers": [],
            },
            "repository": {"full_name": "org/test15"},
        }
        headers = {"x-github-event": "pull_request"}
        result = await parse_github_webhook("github", body, headers)
        assert result is not None
        assert result.recipient_email == "swe@test.com"
        assert result.metadata["github_login"] == "agent-swe-user"
        assert result.source_event_type == "pull_request.opened"

    @pytest.mark.asyncio
    async def test_parse_pr_opened_login_only(self):
        """PR opened with assignee login but no email still captures login."""
        body = {
            "action": "opened",
            "sender": {"login": "copilot-swe-agent"},
            "pull_request": {
                "title": "Add feature",
                "body": "",
                "number": 2,
                "assignees": [{"login": "agent-swe-user"}],
                "requested_reviewers": [],
            },
            "repository": {"full_name": "org/repo"},
        }
        headers = {"x-github-event": "pull_request"}
        result = await parse_github_webhook("github", body, headers)
        assert result is not None
        assert result.recipient_email == ""
        assert result.metadata["github_login"] == "agent-swe-user"

    @pytest.mark.asyncio
    async def test_parse_pr_ready_for_review_falls_back_to_reviewer(self):
        """PR ready_for_review with no assignees falls back to requested_reviewers."""
        body = {
            "action": "ready_for_review",
            "sender": {"login": "copilot"},
            "pull_request": {
                "title": "Feature PR",
                "body": "",
                "number": 3,
                "assignees": [],
                "requested_reviewers": [{"login": "reviewer-user"}],
            },
            "repository": {"full_name": "org/repo"},
        }
        headers = {"x-github-event": "pull_request"}
        result = await parse_github_webhook("github", body, headers)
        assert result is not None
        assert result.metadata["github_login"] == "reviewer-user"

    @pytest.mark.asyncio
    async def test_parse_pr_review_requested_sets_login(self):
        """review_requested action should also capture the reviewer login."""
        body = {
            "action": "review_requested",
            "sender": {"login": "maintainer"},
            "pull_request": {
                "title": "Add auth",
                "body": "JWT",
                "number": 42,
            },
            "requested_reviewer": {"email": "alice@test.com", "login": "alice"},
            "repository": {"full_name": "org/repo"},
        }
        headers = {"x-github-event": "pull_request"}
        result = await parse_github_webhook("github", body, headers)
        assert result is not None
        assert result.recipient_email == "alice@test.com"
        assert result.metadata["github_login"] == "alice"

    @pytest.mark.asyncio
    async def test_parse_pr_assigned_login_without_email(self):
        """PR assigned with login but no email (typical GitHub payload)."""
        body = {
            "action": "assigned",
            "sender": {"login": "copilot"},
            "pull_request": {
                "title": "Add hello world",
                "body": "",
                "number": 1,
            },
            "assignee": {"login": "ali-swe", "id": 12345},
            "repository": {"full_name": "nimbus-hq/test15"},
        }
        headers = {"x-github-event": "pull_request"}
        result = await parse_github_webhook("github", body, headers)
        assert result is not None
        assert result.recipient_email == ""
        assert result.metadata["github_login"] == "ali-swe"
        assert result.source_event_type == "pull_request.assigned"

    @pytest.mark.asyncio
    async def test_parse_pr_opened_skips_useless_assignees(self):
        """Fallback skips assignees without email or login."""
        body = {
            "action": "opened",
            "sender": {"login": "copilot"},
            "pull_request": {
                "title": "Feature",
                "body": "",
                "number": 5,
                "assignees": [
                    {"id": 111},
                    {"login": "real-user", "id": 222},
                ],
                "requested_reviewers": [],
            },
            "repository": {"full_name": "org/repo"},
        }
        headers = {"x-github-event": "pull_request"}
        result = await parse_github_webhook("github", body, headers)
        assert result is not None
        assert result.metadata["github_login"] == "real-user"


# --- Slack parser tests ---


# --- PollingSource tests ---


class TestPollingSource:
    @pytest.mark.asyncio
    async def test_polling_delivers_notifications(self):
        received: list[InboundNotification] = []
        poll_count = 0

        async def poll_fn():
            nonlocal poll_count
            poll_count += 1
            if poll_count <= 1:
                return [
                    InboundNotification(
                        source="email",
                        recipient_email="alice@test.com",
                        subject=f"Email #{poll_count}",
                    )
                ]
            return []

        async def callback(n: InboundNotification):
            received.append(n)

        source = PollingSource(name="email", poll_fn=poll_fn, interval=0.05)
        await source.start(callback)

        # Wait for at least one poll
        await asyncio.sleep(0.15)
        await source.stop()

        assert len(received) >= 1
        assert received[0].subject == "Email #1"

    @pytest.mark.asyncio
    async def test_polling_stop(self):
        async def poll_fn():
            return []

        async def callback(n: InboundNotification):
            pass

        source = PollingSource(name="test", poll_fn=poll_fn, interval=0.05)
        await source.start(callback)
        await source.stop()
        # Should not raise

    @pytest.mark.asyncio
    async def test_polling_error_does_not_crash(self):
        """A poll function that raises doesn't kill the loop."""
        poll_count = 0
        received: list[InboundNotification] = []

        async def flaky_poll_fn():
            nonlocal poll_count
            poll_count += 1
            if poll_count == 1:
                raise RuntimeError("transient error")
            return [
                InboundNotification(
                    source="test",
                    subject=f"After error #{poll_count}",
                )
            ]

        async def callback(n: InboundNotification):
            received.append(n)

        source = PollingSource(name="flaky", poll_fn=flaky_poll_fn, interval=0.05)
        await source.start(callback)
        # Wait for at least two polls (first fails, second succeeds)
        await asyncio.sleep(0.2)
        await source.stop()

        assert poll_count >= 2
        assert len(received) >= 1
