"""Tests for SlackNotificationPrompt mention formatting."""

from __future__ import annotations

from crewlet.notifications.notification_prompts.slack import SlackNotificationPrompt
from crewlet.notifications.protocol import InboundNotification


class TestSlackMentionFormat:
    """Slack prompt instructs agents to use <@USER_ID> mention format."""

    def _make_notification(self, **meta_overrides: str) -> InboundNotification:
        metadata: dict[str, str] = {
            "channel": "C001",
            "ts": "1234567890.000100",
            "thread_ts": "",
            "bot_user_id": "U_BOT_ENG",
        }
        metadata.update(meta_overrides)
        return InboundNotification(
            source="slack",
            source_event_type="message",
            recipient_handle="engineer",
            sender="U12345",
            subject="Slack message",
            body="Hey engineer, check this out",
            metadata=metadata,
        )

    def test_mention_format_instructions_present(self):
        """Prompt includes mention format instructions."""
        notification = self._make_notification()
        result = SlackNotificationPrompt().build(notification)

        assert "<@USER_ID>" in result
        assert "angle brackets" in result

    def test_mention_format_shows_wrong_example(self):
        """Prompt shows the wrong format so agents know what to avoid."""
        notification = self._make_notification()
        result = SlackNotificationPrompt().build(notification)

        assert "@U0ABC123" in result

    def test_mention_format_without_bot_user_id(self):
        """Mention format instructions appear even without bot_user_id."""
        notification = self._make_notification(bot_user_id="")
        result = SlackNotificationPrompt().build(notification)

        assert "<@USER_ID>" in result


class TestSlackSenderFallback:
    """A senderless notification renders an explicit 'unknown', never a
    blank From line or 'posted by ****' (bold markup around an empty
    sender).  The transport always writes the ``user`` metadata key, so
    the fallback must be an ``or`` chain, not a ``.get`` default."""

    def test_empty_sender_renders_unknown(self) -> None:
        notification = InboundNotification(
            source="slack",
            source_event_type="message",
            recipient_handle="engineer",
            sender="",
            subject="Slack message",
            body="hello",
            metadata={
                "channel": "C001",
                "ts": "1234567890.000100",
                "thread_ts": "",
                "user": "",
                "bot_user_id": "U_BOT_ENG",
            },
        )
        result = SlackNotificationPrompt().build(notification)

        assert "posted by **unknown**" in result
        assert "**From:** unknown" in result
        assert "****" not in result

    def test_metadata_user_still_wins_over_fallback(self) -> None:
        notification = InboundNotification(
            source="slack",
            source_event_type="message",
            recipient_handle="engineer",
            sender="",
            subject="Slack message",
            body="hello",
            metadata={
                "channel": "C001",
                "ts": "1234567890.000100",
                "user": "U_HUMAN",
                "bot_user_id": "U_BOT_ENG",
            },
        )
        result = SlackNotificationPrompt().build(notification)

        assert "posted by **U_HUMAN**" in result
        assert "**From:** U_HUMAN" in result


class TestSlackVerboseDecline:
    """When the message names the agent SPECIFICALLY (not
    just as part of an addressed group) and they are declining, the
    prompt must require a brief explanation instead of a silent skip.
    Silence on a group-addressed message with nothing substantive is
    fine (rule 2 carries that); silence on a 1:1 ping is not.
    """

    def _make_notification(self, **meta_overrides: str) -> InboundNotification:
        metadata: dict[str, str] = {
            "channel": "C001",
            "ts": "1234567890.000100",
            "thread_ts": "",
            "bot_user_id": "U_BOT_ENG",
        }
        metadata.update(meta_overrides)
        # Body intentionally non-addressed -- the triage prompt is
        # static, so the test verifies the rule is rendered regardless
        # of body content (the LLM decides at runtime whether it is
        # the addressee).
        return InboundNotification(
            source="slack",
            source_event_type="message",
            recipient_handle="engineer",
            sender="U12345",
            subject="Slack message",
            body="FYI deploy is going out at 5pm",
            metadata=metadata,
        )

    def test_silence_default_still_applies_to_non_addressed(self) -> None:
        """Rule 3 (silence for non-addressed messages) must remain
        explicit.  Pre-existing wording 'silence beats noise' was a
        bare assertion; this test pins the scoped version."""
        result = SlackNotificationPrompt().build(self._make_notification())
        assert "Default for non-addressed messages" in result
        assert "silence beats noise" in result.lower()

    def test_personal_addressee_must_verbose_decline(self) -> None:
        """When the message names the agent specifically (rule 4) and
        they are declining, the prompt must say so explicitly -- a
        one-liner reply, not a silent skip."""
        result = SlackNotificationPrompt().build(self._make_notification())
        # Specifically-personal qualifier -- distinguishes rule 4
        # from rule 2 (group addressee, silence allowed).
        assert "names you SPECIFICALLY" in result
        # Actionable instruction.
        assert "do NOT skip silently" in result
        # Unified justification phrase across all four prompts --
        # changing this should also update the jira / confluence /
        # github / base tests so the wording stays in sync.
        assert "looks like the message was lost" in result

    def test_group_addressee_carve_out_present(self) -> None:
        """The rule explicitly carves out group addressees so rule 4
        does not contradict rule 2 (which allows silence in an
        addressed group when nothing substantive to add)."""
        result = SlackNotificationPrompt().build(self._make_notification())
        assert "Group pings (rule 2)" in result


class TestSlackRequiresRecon:
    """A Slack thread reply is a pointer -- the triggering message is
    usually thin ("yes", "+1") and the thread is the real context.
    A top-level message / DM carries its own body."""

    def _make(self, **meta_overrides: str) -> InboundNotification:
        metadata: dict[str, str] = {
            "channel": "C001",
            "ts": "1234567890.000100",
            "thread_ts": "",
        }
        metadata.update(meta_overrides)
        return InboundNotification(
            source="slack",
            source_event_type="message",
            sender="U12345",
            subject="Slack message",
            body="hey",
            metadata=metadata,
        )

    def test_requires_recon_true_for_thread_reply(self) -> None:
        prompt = SlackNotificationPrompt()
        assert prompt.requires_recon(self._make(thread_ts="1700000000.0001")) is True

    def test_requires_recon_false_for_top_level_message(self) -> None:
        prompt = SlackNotificationPrompt()
        assert prompt.requires_recon(self._make()) is False
