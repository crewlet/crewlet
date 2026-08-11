"""Tests for the GitLab webhook parser, prompt, and config plumbing."""

from __future__ import annotations

import pytest

from crewlet.config import GitLabConfig, gitlab_token_from_mcp_env
from crewlet.notifications.notification_prompts import (
    build_notification_prompt,
    get_prompt,
    notification_requires_recon,
)
from crewlet.notifications.notification_prompts.gitlab import GitLabNotificationPrompt
from crewlet.notifications.protocol import InboundNotification
from crewlet.notifications.sources import (
    extract_gitlab_mentions,
    parse_gitlab_webhook,
)


def _project() -> dict:
    return {"path_with_namespace": "nimbus-hq/nimbuscore"}


class TestMentionExtraction:
    def test_extracts_multiple(self):
        assert extract_gitlab_mentions("hi @agent-swe and @tech-lead!") == [
            "agent-swe",
            "tech-lead",
        ]

    def test_ignores_email_and_paths(self):
        assert extract_gitlab_mentions("mail foo@bar.com or see a/@b") == []

    def test_lowercases_and_dedupes(self):
        assert extract_gitlab_mentions("@Agent-SWE @agent-swe") == ["agent-swe"]

    def test_empty(self):
        assert extract_gitlab_mentions("") == []
        assert extract_gitlab_mentions(None) == []


class TestMergeRequestHook:
    async def test_reviewer_added_on_update(self):
        body = {
            "object_kind": "merge_request",
            "user": {"username": "alice"},
            "project": _project(),
            "object_attributes": {
                "iid": 42,
                "title": "Add feature",
                "action": "update",
                "url": "http://gl/mr/42",
                "description": "desc",
            },
            "changes": {
                "reviewers": {
                    "previous": [{"username": "bob"}],
                    "current": [{"username": "bob"}, {"username": "agent-swe"}],
                }
            },
        }
        out = await parse_gitlab_webhook("gitlab", body, {})
        assert len(out) == 1
        n = out[0]
        assert n.metadata["gitlab_username"] == "agent-swe"
        assert n.metadata["event_type"] == "merge_request.review_requested"
        assert n.metadata["mr_iid"] == "42"
        assert n.metadata["project"] == "nimbus-hq/nimbuscore"

    async def test_removed_reviewer_not_notified(self):
        body = {
            "object_kind": "merge_request",
            "user": {"username": "alice"},
            "project": _project(),
            "object_attributes": {"iid": 1, "action": "update"},
            "changes": {
                "reviewers": {
                    "previous": [{"username": "agent-swe"}],
                    "current": [],
                }
            },
        }
        assert await parse_gitlab_webhook("gitlab", body, {}) == []

    async def test_open_notifies_reviewers_and_assignees(self):
        body = {
            "object_kind": "merge_request",
            "user": {"username": "alice"},
            "project": _project(),
            "object_attributes": {"iid": 5, "action": "open"},
            "reviewers": [{"username": "reviewer-bot"}],
            "assignees": [{"username": "assignee-bot"}],
        }
        out = await parse_gitlab_webhook("gitlab", body, {})
        reasons = {n.metadata["gitlab_username"]: n.metadata["event_type"] for n in out}
        assert reasons == {
            "reviewer-bot": "merge_request.review_requested",
            "assignee-bot": "merge_request.assigned",
        }

    async def test_approval_routes_to_assignee(self):
        body = {
            "object_kind": "merge_request",
            "user": {"username": "alice"},
            "project": _project(),
            "object_attributes": {"iid": 9, "action": "approved"},
            "assignees": [{"username": "agent-swe"}],
        }
        out = await parse_gitlab_webhook("gitlab", body, {})
        assert [n.metadata["event_type"] for n in out] == ["merge_request.approved"]


class TestIssueHook:
    async def test_assigned_on_update(self):
        body = {
            "object_kind": "issue",
            "user": {"username": "alice"},
            "project": _project(),
            "object_attributes": {"iid": 7, "title": "Bug", "action": "update"},
            "changes": {
                "assignees": {"previous": [], "current": [{"username": "agent-swe"}]}
            },
        }
        out = await parse_gitlab_webhook("gitlab", body, {})
        assert len(out) == 1
        assert out[0].metadata["event_type"] == "issue.assigned"
        assert out[0].metadata["issue_iid"] == "7"


class TestNoteHook:
    async def test_mention_intersected_with_known(self):
        body = {
            "object_kind": "note",
            "user": {"username": "bob"},
            "project": _project(),
            "object_attributes": {
                "note": "cc @agent-swe and @outsider",
                "noteable_type": "MergeRequest",
                "url": "http://gl/mr/7#note_1",
            },
            "merge_request": {"iid": 7, "title": "T", "assignees": []},
        }
        out = await parse_gitlab_webhook(
            "gitlab", body, {}, known_usernames={"agent-swe"}
        )
        assert [n.metadata["gitlab_username"] for n in out] == ["agent-swe"]
        assert out[0].metadata["event_type"] == "note.mention"

    async def test_comment_to_assignee_and_mention_dedup(self):
        body = {
            "object_kind": "note",
            "user": {"username": "bob"},
            "project": _project(),
            "object_attributes": {
                "note": "@agent-swe take a look",
                "noteable_type": "Issue",
                "url": "u",
            },
            "issue": {"iid": 3, "title": "T", "assignees": [{"username": "agent-swe"}]},
        }
        out = await parse_gitlab_webhook(
            "gitlab", body, {}, known_usernames={"agent-swe"}
        )
        # mentioned AND assignee → one notification, mention reason wins.
        assert len(out) == 1
        assert out[0].metadata["event_type"] == "note.mention"

    async def test_author_not_self_notified(self):
        body = {
            "object_kind": "note",
            "user": {"username": "agent-swe"},
            "project": _project(),
            "object_attributes": {
                "note": "note to self @agent-swe",
                "noteable_type": "Issue",
                "url": "u",
            },
            "issue": {"iid": 3, "title": "T", "assignees": [{"username": "agent-swe"}]},
        }
        out = await parse_gitlab_webhook(
            "gitlab", body, {}, known_usernames={"agent-swe"}
        )
        assert out == []


class TestPipelineHook:
    async def test_failed_notifies_actor(self):
        body = {
            "object_kind": "pipeline",
            "user": {"username": "Agent-SWE"},
            "project": _project(),
            "object_attributes": {"status": "failed"},
            "merge_request": {"iid": 9},
        }
        out = await parse_gitlab_webhook("gitlab", body, {})
        assert len(out) == 1
        assert out[0].metadata["gitlab_username"] == "agent-swe"
        assert out[0].metadata["event_type"] == "pipeline.failed"
        assert out[0].metadata["mr_iid"] == "9"

    async def test_success_ignored(self):
        body = {
            "object_kind": "pipeline",
            "user": {"username": "agent-swe"},
            "project": _project(),
            "object_attributes": {"status": "success"},
        }
        assert await parse_gitlab_webhook("gitlab", body, {}) == []


class TestUnknownAndEmoji:
    async def test_emoji_no_fan_out(self):
        body = {
            "object_kind": "emoji",
            "user": {"username": "bob"},
            "project": _project(),
            "object_attributes": {"name": "thumbsup"},
        }
        assert await parse_gitlab_webhook("gitlab", body, {}) == []

    async def test_unknown_kind(self):
        assert (
            await parse_gitlab_webhook("gitlab", {"object_kind": "wiki_page"}, {}) == []
        )


class TestGitLabPrompt:
    def _notif(self, event_type: str, **meta) -> InboundNotification:
        base = {"event_type": event_type, "project": "nimbus-hq/x"}
        base.update(meta)
        return InboundNotification(
            source="gitlab",
            source_event_type=event_type,
            sender="alice",
            subject="Thing",
            body="the body",
            metadata=base,
        )

    def test_registered(self):
        assert isinstance(get_prompt("gitlab"), GitLabNotificationPrompt)

    def test_review_requested_prompt(self):
        n = self._notif("merge_request.review_requested", mr_iid="42", url="http://x")
        text = build_notification_prompt(n)
        assert "review a merge request" in text.lower()
        assert "nimbus-hq/x!42" in text

    def test_assigned_prompt(self):
        n = self._notif("issue.assigned", issue_iid="7")
        text = build_notification_prompt(n)
        assert "assigned" in text.lower()
        assert "nimbus-hq/x#7" in text

    def test_mention_prompt(self):
        n = self._notif("note.mention", mr_iid="5")
        text = build_notification_prompt(n)
        assert "mentioned" in text.lower()

    def test_conversation_key_mr(self):
        prompt = get_prompt("gitlab")
        assert prompt.conversation_key({"project": "p", "mr_iid": "3"}) == "p!3"
        assert prompt.conversation_key({"project": "p", "issue_iid": "8"}) == "p#8"
        assert prompt.conversation_key({"project": "p"}) == ""

    def test_requires_recon(self):
        assert notification_requires_recon(
            self._notif("merge_request.review_requested", mr_iid="1")
        )
        assert notification_requires_recon(self._notif("pipeline.failed", mr_iid="1"))
        assert not notification_requires_recon(self._notif("note.mention", mr_iid="1"))


class TestGitLabConfig:
    def test_api_base(self):
        c = GitLabConfig(enabled=True, url="https://gitlab.com/", signing_secret="s")
        assert c.api_base == "https://gitlab.com/api/v4"

    def test_requires_url(self):
        with pytest.raises(ValueError, match="url is required"):
            GitLabConfig(enabled=True, signing_secret="s")

    def test_requires_signing_secret(self):
        with pytest.raises(ValueError, match="signing_secret is required"):
            GitLabConfig(enabled=True, url="https://gitlab.com")

    def test_disabled_needs_nothing(self):
        assert GitLabConfig().enabled is False

    def test_token_from_private_token_header(self):
        assert gitlab_token_from_mcp_env({"Private-Token": "glpat-a"}) == "glpat-a"

    def test_token_from_bearer(self):
        assert (
            gitlab_token_from_mcp_env({"Authorization": "Bearer glpat-b"}) == "glpat-b"
        )

    def test_token_from_stdio_env(self):
        assert (
            gitlab_token_from_mcp_env({"GITLAB_PERSONAL_ACCESS_TOKEN": "glpat-c"})
            == "glpat-c"
        )

    def test_token_from_glab_env(self):
        # glab's stdio env var (the adopted default MCP server).
        assert (
            gitlab_token_from_mcp_env(
                {"GITLAB_TOKEN": "glpat-d", "GITLAB_HOST": "gitlab.com"}
            )
            == "glpat-d"
        )

    def test_token_absent(self):
        assert gitlab_token_from_mcp_env({"Other": "x"}) == ""


class TestParticipantsRouting:
    """Thread-activity fan-out via the participants fetcher."""

    @staticmethod
    def _fetcher(usernames, calls=None):
        async def fetch(project_id: int, kind: str, iid: int) -> list[str]:
            if calls is not None:
                calls.append((project_id, kind, iid))
            return list(usernames)

        return fetch

    async def test_note_fans_out_to_participants(self):
        calls: list = []
        body = {
            "object_kind": "note",
            "user": {"username": "human"},
            "project": {"path_with_namespace": "nimbus-hq/x", "id": 17},
            "object_attributes": {
                "note": "status update, no mention",
                "noteable_type": "MergeRequest",
                "url": "u",
            },
            "merge_request": {"iid": 7, "title": "T", "assignees": []},
        }
        out = await parse_gitlab_webhook(
            "gitlab",
            body,
            {},
            known_usernames={"agent-author", "agent-reviewer"},
            participants_fetcher=self._fetcher(
                ["agent-author", "agent-reviewer", "human", "outsider"], calls
            ),
        )
        got = {n.metadata["gitlab_username"]: n.metadata["event_type"] for n in out}
        # Actor excluded, outsider filtered by known set, both agents reached.
        assert got == {
            "agent-author": "note.comment",
            "agent-reviewer": "note.comment",
        }
        assert calls == [(17, "merge_request", 7)]

    async def test_mention_reason_wins_over_participant(self):
        body = {
            "object_kind": "note",
            "user": {"username": "human"},
            "project": {"path_with_namespace": "nimbus-hq/x", "id": 17},
            "object_attributes": {
                "note": "@agent-reviewer please look",
                "noteable_type": "MergeRequest",
                "url": "u",
            },
            "merge_request": {"iid": 7, "title": "T", "assignees": []},
        }
        out = await parse_gitlab_webhook(
            "gitlab",
            body,
            {},
            known_usernames={"agent-reviewer"},
            participants_fetcher=self._fetcher(["agent-reviewer", "human"]),
        )
        assert len(out) == 1
        assert out[0].metadata["event_type"] == "note.mention"

    async def test_fetcher_failure_degrades_to_assignees(self):
        async def broken(project_id: int, kind: str, iid: int) -> list[str]:
            raise RuntimeError("boom")

        body = {
            "object_kind": "note",
            "user": {"username": "human"},
            "project": {"path_with_namespace": "nimbus-hq/x", "id": 17},
            "object_attributes": {
                "note": "no mention here",
                "noteable_type": "Issue",
                "url": "u",
            },
            "issue": {
                "iid": 3,
                "title": "T",
                "assignees": [{"username": "agent-swe"}],
            },
        }
        out = await parse_gitlab_webhook(
            "gitlab",
            body,
            {},
            known_usernames={"agent-swe"},
            participants_fetcher=broken,
        )
        assert [n.metadata["gitlab_username"] for n in out] == ["agent-swe"]
        assert out[0].metadata["event_type"] == "note.comment"

    async def test_mr_merge_fans_out_to_participants(self):
        body = {
            "object_kind": "merge_request",
            "user": {"username": "human"},
            "project": {"path_with_namespace": "nimbus-hq/x", "id": 5},
            "object_attributes": {"iid": 9, "title": "T", "action": "merge"},
            "assignees": [{"username": "agent-author"}],
        }
        out = await parse_gitlab_webhook(
            "gitlab",
            body,
            {},
            known_usernames={"agent-author", "agent-reviewer"},
            participants_fetcher=self._fetcher(
                ["agent-author", "agent-reviewer", "human"]
            ),
        )
        got = {n.metadata["gitlab_username"]: n.metadata["event_type"] for n in out}
        assert got == {
            "agent-author": "merge_request.merge",
            "agent-reviewer": "merge_request.merge",
        }

    async def test_issue_close_reaches_assignee_without_fetcher(self):
        body = {
            "object_kind": "issue",
            "user": {"username": "human"},
            "project": {"path_with_namespace": "nimbus-hq/x", "id": 5},
            "object_attributes": {"iid": 4, "title": "T", "action": "close"},
            "assignees": [{"username": "agent-swe"}],
        }
        out = await parse_gitlab_webhook(
            "gitlab", body, {}, known_usernames={"agent-swe"}
        )
        assert [n.metadata["event_type"] for n in out] == ["issue.close"]

    async def test_no_fan_out_on_open(self):
        calls: list = []
        body = {
            "object_kind": "issue",
            "user": {"username": "human"},
            "project": {"path_with_namespace": "nimbus-hq/x", "id": 5},
            "object_attributes": {
                "iid": 4,
                "title": "T",
                "action": "open",
                "description": "",
            },
            "assignees": [{"username": "agent-swe"}],
        }
        await parse_gitlab_webhook(
            "gitlab",
            body,
            {},
            known_usernames={"agent-swe"},
            participants_fetcher=self._fetcher([], calls),
        )
        # Open needs no lookup: author+assignees+description mentions are
        # all payload-derived already.
        assert calls == []


class TestDescriptionMentions:
    async def test_mention_on_issue_open(self):
        body = {
            "object_kind": "issue",
            "user": {"username": "human"},
            "project": {"path_with_namespace": "nimbus-hq/x", "id": 5},
            "object_attributes": {
                "iid": 4,
                "title": "T",
                "action": "open",
                "description": "please @agent-swe handle this",
            },
            "assignees": [],
        }
        out = await parse_gitlab_webhook(
            "gitlab", body, {}, known_usernames={"agent-swe"}
        )
        assert [n.metadata["event_type"] for n in out] == ["issue.mention"]

    async def test_only_newly_added_mentions_on_update(self):
        body = {
            "object_kind": "merge_request",
            "user": {"username": "human"},
            "project": {"path_with_namespace": "nimbus-hq/x", "id": 5},
            "object_attributes": {"iid": 9, "title": "T", "action": "update"},
            "changes": {
                "description": {
                    "previous": "cc @agent-old",
                    "current": "cc @agent-old and now @agent-new too",
                }
            },
        }
        out = await parse_gitlab_webhook(
            "gitlab", body, {}, known_usernames={"agent-old", "agent-new"}
        )
        assert [n.metadata["gitlab_username"] for n in out] == ["agent-new"]
        assert out[0].metadata["event_type"] == "merge_request.mention"

    async def test_description_mention_prompt_is_tailored(self):
        n = InboundNotification(
            source="gitlab",
            source_event_type="issue.mention",
            sender="human",
            subject="T",
            body="please @agent-swe",
            metadata={
                "event_type": "issue.mention",
                "project": "nimbus-hq/x",
                "issue_iid": "4",
            },
        )
        text = build_notification_prompt(n)
        assert "mentioned" in text.lower()
        assert "description" in text.lower()
