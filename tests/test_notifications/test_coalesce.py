"""Unit tests for inbox coalescing — conversation keys + digest merging."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta

import pytest

from crewlet.events.types import Event, ExternalNotification, TaskAssigned
from crewlet.notifications.coalesce import (
    coalesce_notifications,
    conversation_key,
)

_T0 = datetime(2026, 6, 10, 12, 0, 0, tzinfo=UTC)


def _slack(
    *,
    sender: str = "Alice",
    body: str = "enriched prompt",
    salient: str | None = "hello",
    thread_ts: str = "1718.001",
    ts: str = "1718.005",
    channel: str = "C42",
    offset_s: int = 0,
    user_id: str = "U-A",
    recon: bool = False,
    depth: int = 0,
    parent_turn_id: str = "",
    chain: list[str] | None = None,
) -> ExternalNotification:
    return ExternalNotification(
        notification_source="slack",
        source_event_type="message",
        sender=sender,
        subject="Slack message",
        body=body,
        salient_body=salient,
        metadata={
            "channel": channel,
            "thread_ts": thread_ts,
            "ts": ts,
            "slack_user_id": user_id,
            "event_type": "message",
        },
        timestamp=_T0 + timedelta(seconds=offset_s),
        context_requires_recon=recon,
        delegation_depth=depth,
        parent_turn_id=parent_turn_id,
        delegation_chain=chain or [],
    )


def _jira(
    *,
    sender: str = "Sarah",
    event_type: str = "comment_created",
    salient: str | None = "please review",
    issue_key: str = "POC-7",
    offset_s: int = 0,
) -> ExternalNotification:
    return ExternalNotification(
        notification_source="jira",
        source_event_type=event_type,
        sender=sender,
        subject=f"{issue_key} updated",
        body=f"enriched jira prompt for {issue_key}",
        salient_body=salient,
        metadata={"issue_key": issue_key, "event_type": event_type},
        timestamp=_T0 + timedelta(seconds=offset_s),
        context_requires_recon=True,
    )


# ---------------------------------------------------------------------------
# conversation_key
# ---------------------------------------------------------------------------


class TestConversationKey:
    def test_jira_keys_on_issue(self) -> None:
        assert conversation_key(_jira(issue_key="POC-7")) == "jira:POC-7"

    def test_jira_falls_back_to_subject_issue_key(self) -> None:
        event = _jira(issue_key="POC-9")
        event.metadata.pop("issue_key")
        assert conversation_key(event) == "jira:POC-9"  # from "POC-9 updated"

    def test_slack_keys_on_channel_and_thread(self) -> None:
        assert (
            conversation_key(_slack(channel="C42", thread_ts="1718.001"))
            == "slack:C42:1718.001"
        )

    def test_slack_top_level_message_keys_on_own_ts(self) -> None:
        event = _slack(thread_ts="", ts="1718.005")
        assert conversation_key(event) == "slack:C42:1718.005"

    def test_slack_without_channel_is_unique(self) -> None:
        event = _slack(channel="")
        assert conversation_key(event) == f"event:{event.id}"

    def test_confluence_keys_on_page(self) -> None:
        event = ExternalNotification(
            notification_source="confluence",
            metadata={"page_id": "12345"},
        )
        assert conversation_key(event) == "confluence:12345"

    def test_github_keys_on_repo_and_number(self) -> None:
        pr = ExternalNotification(
            notification_source="github",
            metadata={"repo": "acme/api", "pr_number": "42"},
        )
        assert conversation_key(pr) == "github:acme/api#42"
        issue = ExternalNotification(
            notification_source="github",
            metadata={"repo": "acme/api", "issue_number": "7"},
        )
        assert conversation_key(issue) == "github:acme/api#7"

    def test_unknown_source_is_unique(self) -> None:
        event = ExternalNotification(notification_source="carrier-pigeon")
        assert conversation_key(event) == f"event:{event.id}"

    def test_non_notification_events_are_unique(self) -> None:
        task = TaskAssigned(task_id="t1", agent_id="a1")
        assert conversation_key(task) == f"event:{task.id}"
        generic = Event(type="a2a_request")
        assert conversation_key(generic) == f"event:{generic.id}"

    def test_same_conversation_same_key_distinct_conversations_differ(self) -> None:
        assert conversation_key(_slack(offset_s=0)) == conversation_key(
            _slack(offset_s=9)
        )
        assert conversation_key(_jira(issue_key="POC-1")) != conversation_key(
            _jira(issue_key="POC-2")
        )


# ---------------------------------------------------------------------------
# coalesce_notifications
# ---------------------------------------------------------------------------


class TestCoalesceNotifications:
    def test_requires_at_least_one_event(self) -> None:
        with pytest.raises(ValueError):
            coalesce_notifications([])

    def test_single_event_passes_through_unchanged(self) -> None:
        event = _slack()
        assert coalesce_notifications([event]) is event

    def test_digest_lists_earlier_messages_and_renders_latest_in_full(self) -> None:
        first = _slack(sender="Alice", salient="first message", offset_s=0)
        second = _slack(sender="Bob", salient="second message", offset_s=5)
        latest = _slack(
            sender="Carol",
            body="FULL ENRICHED PROMPT OF LATEST",
            salient="third message",
            offset_s=9,
        )
        merged = coalesce_notifications([first, second, latest])

        assert "## Coalesced updates (3 events" in merged.body
        assert "**Alice**" in merged.body and "first message" in merged.body
        assert "**Bob**" in merged.body and "second message" in merged.body
        # The latest constituent's enriched body renders once, in full.
        assert merged.body.endswith("FULL ENRICHED PROMPT OF LATEST")
        assert merged.body.count("FULL ENRICHED PROMPT") == 1

    def test_chronological_order_independent_of_input_order(self) -> None:
        first = _slack(sender="Alice", salient="first", offset_s=0)
        latest = _slack(sender="Bob", body="LATEST BODY", salient="last", offset_s=9)
        merged = coalesce_notifications([latest, first])  # reversed input
        assert merged.body.endswith("LATEST BODY")
        assert [m.body for m in merged.messages] == ["first", "last"]

    def test_messages_carry_full_fidelity_per_constituent(self) -> None:
        first = _slack(sender="Alice", salient="first", user_id="U-A", offset_s=0)
        latest = _slack(sender="Bob", salient="second", user_id="U-B", offset_s=5)
        merged = coalesce_notifications([first, latest])

        assert len(merged.messages) == 2
        assert merged.messages[0].sender == "Alice"
        assert merged.messages[0].metadata["slack_user_id"] == "U-A"
        assert merged.messages[1].sender == "Bob"
        assert merged.messages[1].metadata["slack_user_id"] == "U-B"
        # Flat fields mirror the latest constituent.
        assert merged.sender == "Bob"
        assert merged.metadata["slack_user_id"] == "U-B"

    def test_salient_body_is_sender_attributed_join(self) -> None:
        merged = coalesce_notifications(
            [
                _slack(sender="Alice", salient="first", offset_s=0),
                _slack(sender="Bob", salient="second", offset_s=5),
            ]
        )
        assert merged.salient_body == "Alice: first\n\nBob: second"

    def test_recon_merge_is_conservative(self) -> None:
        no_recon = _slack(recon=False, offset_s=0)
        with_recon = _slack(recon=True, offset_s=5)
        assert coalesce_notifications([no_recon, with_recon]).context_requires_recon
        assert not coalesce_notifications(
            [_slack(recon=False, offset_s=0), _slack(recon=False, offset_s=5)]
        ).context_requires_recon

    def test_delegation_bookkeeping_takes_max_depth_constituent(self) -> None:
        shallow = _slack(depth=0, offset_s=9)  # latest but shallow
        deep = _slack(depth=2, parent_turn_id="turn-9", chain=["a", "b"], offset_s=0)
        merged = coalesce_notifications([shallow, deep])
        assert merged.delegation_depth == 2
        assert merged.parent_turn_id == "turn-9"
        assert merged.delegation_chain == ["a", "b"]

    def test_trace_context_continues_latest_constituent(self) -> None:
        first = _slack(offset_s=0)
        latest = _slack(offset_s=9)
        merged = coalesce_notifications([first, latest])
        assert merged.trace_id == latest.trace_id
        assert merged.span_id == latest.span_id

    def test_jira_supersede_drops_update_bodies_keeps_comments(self) -> None:
        """Intermediate ``issue_updated`` bodies (stale full descriptions)
        collapse to their event lead; comment bodies always survive."""
        update = _jira(
            event_type="issue_updated", salient="STALE FULL DESCRIPTION", offset_s=0
        )
        comment = _jira(
            event_type="comment_created", salient="please fix the rollout", offset_s=5
        )
        latest = _jira(event_type="comment_created", salient="ping?", offset_s=9)
        merged = coalesce_notifications([update, comment, latest])

        assert "STALE FULL DESCRIPTION" not in merged.body
        assert "superseded by later state" in merged.body
        assert "please fix the rollout" in merged.body
        # The structured record keeps full fidelity regardless.
        assert merged.messages[0].body == "STALE FULL DESCRIPTION"
        # The merged salient text mirrors the digest's supersede rule.
        assert "STALE FULL DESCRIPTION" not in merged.salient_body
        assert "please fix the rollout" in merged.salient_body

    def test_long_digest_bodies_render_in_full(self) -> None:
        merged = coalesce_notifications(
            [
                _slack(sender="Alice", salient="x" * 5000, offset_s=0),
                _slack(sender="Bob", salient="done", offset_s=5),
            ]
        )
        # Digest bodies are never truncated -- the full text renders.
        assert "x" * 5000 in merged.body
        assert "… [truncated]" not in merged.body
        # Full fidelity is preserved on the structured record too.
        assert merged.messages[0].body == "x" * 5000

    def test_merged_event_round_trips_through_queue_serialization(self) -> None:
        """The merged trigger must survive a Pulsar round-trip with its
        ``messages`` intact — learning workers read them on the consumer
        side."""
        from crewlet.queue.serialization import deserialize_event, serialize_event

        merged = coalesce_notifications(
            [
                _slack(sender="Alice", salient="first", offset_s=0),
                _slack(sender="Bob", salient="second", offset_s=5),
            ]
        )
        restored = deserialize_event(serialize_event(merged))
        assert isinstance(restored, ExternalNotification)
        assert [m.sender for m in restored.messages] == ["Alice", "Bob"]
        assert restored.salient_body == merged.salient_body


class TestConversationKeyDMs:
    """DM channels coalesce at channel granularity (the headline case)."""

    def test_dm_top_level_burst_shares_one_key(self) -> None:
        first = _slack(thread_ts="", ts="1718.001")
        first.metadata["channel_type"] = "im"
        second = _slack(thread_ts="", ts="1718.002")
        second.metadata["channel_type"] = "im"
        assert conversation_key(first) == "slack:C42"
        assert conversation_key(first) == conversation_key(second)

    def test_group_dm_keys_on_channel(self) -> None:
        event = _slack(thread_ts="", ts="1718.001")
        event.metadata["channel_type"] = "mpim"
        assert conversation_key(event) == "slack:C42"

    def test_dm_thread_reply_keeps_its_thread_key(self) -> None:
        """A DM thread reply must NOT merge with unrelated top-level
        pings — the merged trigger would carry only one reply target
        (the latest constituent's ts/thread_ts) and steer the digest
        reply to the wrong place."""
        reply = _slack(thread_ts="1718.001", ts="1718.009")
        reply.metadata["channel_type"] = "im"
        assert conversation_key(reply) == "slack:C42:1718.001"
        top_level = _slack(thread_ts="", ts="1718.010")
        top_level.metadata["channel_type"] = "im"
        assert conversation_key(top_level) == "slack:C42"
        assert conversation_key(reply) != conversation_key(top_level)

    def test_dm_recognised_by_channel_prefix_without_channel_type(self) -> None:
        """``app_mention`` events carry no ``channel_type`` — a
        D-prefixed channel id still keys channel-grained, so the
        mention/message double-delivery dedup race cannot split a DM
        burst into two partitions."""
        event = _slack(thread_ts="", ts="1718.001", channel="D123")
        assert conversation_key(event) == "slack:D123"

    def test_shared_channel_stays_thread_grained(self) -> None:
        """Two unrelated top-level asks in a public channel must NOT
        merge into one digest."""
        first = _slack(thread_ts="", ts="1718.001")
        first.metadata["channel_type"] = "channel"
        second = _slack(thread_ts="", ts="1718.002")
        second.metadata["channel_type"] = "channel"
        assert conversation_key(first) == "slack:C42:1718.001"
        assert conversation_key(first) != conversation_key(second)


class TestDigestBodyResolution:
    def test_salient_none_falls_back_to_body_in_digest(self) -> None:
        """A producer without a prompt builder (salient_body=None, body
        IS the message) must not render as 'superseded' — the
        salient_body contract's None-fallback applies to the digest."""
        first = _slack(body="the actual message", salient=None, offset_s=0)
        latest = _slack(salient="newest", offset_s=5)
        merged = coalesce_notifications([first, latest])
        assert "the actual message" in merged.body
        assert "superseded by later state" not in merged.body
        assert "the actual message" in merged.salient_body
        assert merged.messages[0].body == "the actual message"

    def test_empty_salient_does_not_fall_back(self) -> None:
        """Empty-but-present salient_body means genuinely contentless —
        falling back to the enriched body would inject scaffolding."""
        first = _slack(body="## Triage scaffolding ...", salient="", offset_s=0)
        latest = _slack(salient="newest", offset_s=5)
        merged = coalesce_notifications([first, latest])
        assert "Triage scaffolding" not in merged.salient_body
        assert "(no message body — superseded by later state)" in merged.body

    def test_github_lifecycle_bodies_superseded_comments_kept(self) -> None:
        """GitHub lifecycle webhooks re-emit the full PR description per
        event; only comments carry per-event signal."""

        def _gh(event_type: str, salient: str, offset_s: int) -> ExternalNotification:
            return ExternalNotification(
                notification_source="github",
                source_event_type=event_type,
                sender="octocat",
                subject="PR title",
                body=f"enriched github prompt ({event_type})",
                salient_body=salient,
                metadata={
                    "repo": "acme/api",
                    "pr_number": "42",
                    "event_type": event_type,
                },
                timestamp=_T0 + timedelta(seconds=offset_s),
            )

        merged = coalesce_notifications(
            [
                _gh("pull_request.synchronize", "FULL STALE DESCRIPTION", 0),
                _gh("issue_comment.created", "please fix the lint error", 5),
                _gh("pull_request.labeled", "FULL STALE DESCRIPTION", 9),
            ]
        )
        # The re-emitted (identical, same-sender) copy collapses to a
        # dedupe marker; the comment survives; the description renders
        # exactly once (via the latest constituent).
        assert "FULL STALE DESCRIPTION" not in merged.body.split("---")[0]
        assert "identical to a later update" in merged.body
        assert "please fix the lint error" in merged.body
        assert merged.salient_body.count("FULL STALE DESCRIPTION") == 1
        assert "please fix the lint error" in merged.salient_body
        # Full fidelity survives on the structured record.
        assert merged.messages[0].body == "FULL STALE DESCRIPTION"

    def test_github_description_survives_when_latest_is_a_comment(self) -> None:
        """The dedupe must not destroy content nothing re-renders: when
        the latest constituent is a comment, the earlier lifecycle
        event's description has no later copy and stays in the digest."""

        def _gh(event_type: str, salient: str, offset_s: int) -> ExternalNotification:
            return ExternalNotification(
                notification_source="github",
                source_event_type=event_type,
                sender="octocat",
                subject="PR title",
                body=f"enriched github prompt ({event_type})",
                salient_body=salient,
                metadata={
                    "repo": "acme/api",
                    "pr_number": "42",
                    "event_type": event_type,
                },
                timestamp=_T0 + timedelta(seconds=offset_s),
            )

        merged = coalesce_notifications(
            [
                _gh("pull_request.opened", "THE PR DESCRIPTION", 0),
                _gh("issue_comment.created", "see above, please prioritize", 5),
            ]
        )
        assert "THE PR DESCRIPTION" in merged.body
        assert "THE PR DESCRIPTION" in merged.salient_body
        # The comment is the latest constituent: its enriched body
        # renders below the digest (the fixture's fake enriched body
        # stands in for it) and its salient text joins salient_body.
        assert merged.body.endswith("enriched github prompt (issue_comment.created)")
        assert "see above, please prioritize" in merged.salient_body

    def test_identical_bodies_from_different_senders_both_survive(self) -> None:
        """Two people saying the same thing ("+1") are two signals —
        dedupe is same-sender only."""
        merged = coalesce_notifications(
            [
                _slack(sender="Alice", salient="+1", user_id="U-A", offset_s=0),
                _slack(sender="Bob", salient="+1", user_id="U-B", offset_s=5),
                _slack(sender="Carol", salient="ship it", user_id="U-C", offset_s=9),
            ]
        )
        assert merged.salient_body.count("+1") == 2

    def test_per_message_recon_flags_survive_the_merge(self) -> None:
        no_recon = _slack(recon=False, offset_s=0)
        with_recon = _slack(recon=True, offset_s=5)
        merged = coalesce_notifications([no_recon, with_recon])
        assert [m.context_requires_recon for m in merged.messages] == [False, True]
        assert merged.context_requires_recon is True
