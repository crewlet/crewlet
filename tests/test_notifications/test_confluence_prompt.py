"""Tests for ConfluenceNotificationPrompt rendering."""

from __future__ import annotations

from crewlet.notifications.notification_prompts.confluence import (
    ConfluenceNotificationPrompt,
)
from crewlet.notifications.protocol import InboundNotification


def _make_notification(**meta_overrides: str) -> InboundNotification:
    metadata: dict[str, str] = {
        "page_id": "22904843",
        "page_title": "Architecture Decision: Auth",
        "space_key": "ENG",
        "space_name": "Engineering",
        "event_type": "comment_created",
        "actor_account_id": "user-abc",
        "actor_name": "Alice",
        "timestamp": "1711756800000",
    }
    metadata.update(meta_overrides)
    return InboundNotification(
        source="confluence",
        source_event_type=metadata["event_type"],
        sender=metadata.get("actor_name", ""),
        subject=f"[{metadata['space_key']}] {metadata['page_title']}",
        body="<p>Looks good!</p>",
        metadata=metadata,
    )


class TestConfluencePromptPageId:
    """Page ID and comment ID must appear in the prompt so agents can
    use them with MCP tools."""

    def test_page_id_in_prompt(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification())
        assert "22904843" in result
        assert "Page ID: 22904843" in result or "`22904843`" in result

    def test_page_title_in_prompt(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification())
        assert "Architecture Decision: Auth" in result

    def test_comment_id_in_prompt(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification(comment_id="99"))
        assert "99" in result
        assert "Comment ID: 99" in result or "`99`" in result

    def test_comment_id_absent_when_not_set(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification())
        assert "Comment ID" not in result

    def test_event_metadata_section(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification())
        assert "comment_created" in result
        assert "Alice" in result
        assert "ENG" in result

    def test_body_included(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification())
        assert "<p>Looks good!</p>" in result

    def test_routed_via_shown(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification(routed_via="mention"))
        assert "mention" in result

    def test_page_details_section_has_page_id(self) -> None:
        """The Page Details section must include the concrete page ID
        so the agent can pass it to MCP tools."""
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification())
        assert "page ID `22904843`" in result

    def test_page_url_in_prompt(self) -> None:
        """When page_url metadata is set, it appears in the prompt."""
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(
            _make_notification(
                page_url="https://myco.atlassian.net/wiki/spaces/ENG/pages/22904843"
            )
        )
        assert "https://myco.atlassian.net/wiki/spaces/ENG/pages/22904843" in result

    def test_page_url_absent_when_not_set(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification())
        assert "Page URL:" not in result

    def test_comment_fetch_instruction(self) -> None:
        """When comment_id is present, prompt should instruct the agent
        to fetch comments."""
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification(comment_id="55"))
        assert "comment" in result.lower()
        assert "`55`" in result


class TestConfluenceLeadFallbackHint:
    """When the only reason a lead received the event is that the page
    had no specific watcher or @mention, the prompt must surface that
    explicitly and lay out the delegate / act / skip decision -- so the
    lead delegates rather than silently absorbing the work."""

    def test_lead_fallback_block_present_for_space_lead(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification(routed_via="space_lead"))
        assert "## Why You Received This" in result
        # Names the space so the lead knows which one fell back.
        assert "**ENG**" in result
        # Names the three explicit decisions.
        assert "**Delegate**" in result
        assert "**Act yourself**" in result
        assert "**Escalate**" in result
        # The third decision must steer the lead to surface this up
        # rather than walking away silently, since nobody else is
        # watching a space-lead-routed page.
        assert "manager" in result
        assert "no one else is watching" in result

    def test_lead_fallback_block_omits_space_when_missing(self) -> None:
        """A payload with no space key still gets a coherent hint --
        fall back to a generic phrasing rather than rendering"""
        """ ``the **** Confluence space``."""
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(
            _make_notification(routed_via="space_lead", space_key="", space_name="")
        )
        assert "## Why You Received This" in result
        assert "this Confluence space" in result
        assert "**** Confluence space" not in result

    def test_lead_fallback_block_absent_for_watcher(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification(routed_via="watcher"))
        assert "## Why You Received This" not in result

    def test_lead_fallback_block_absent_for_mention(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification(routed_via="mention"))
        assert "## Why You Received This" not in result

    def test_lead_fallback_block_absent_when_routed_via_missing(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification())
        assert "## Why You Received This" not in result


class TestConfluenceVerboseDecline:
    """A direct @mention on a Confluence page is a request
    for input.  When the agent decides not to act, the prompt must
    require a brief reply (naming the right owner, with manager
    escalation when the owner is unknown) instead of a silent skip.
    The rule is gated to routings that represent a direct ask --
    watcher events get the routine guidance only, and space_lead
    uses its own dedicated block."""

    def test_verbose_decline_present_for_mention_routing(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification(routed_via="mention"))
        # Anchor phrase from the rule -- distinguish from the
        # earlier "weren't directly @mentioned" line in rule prose
        # by anchoring on the unique "decided not to act" clause.
        assert "directly @mentioned but have decided not" in result
        # Actionable instruction.
        assert "do NOT skip silently" in result
        # Unified justification across all four prompts.
        assert "looks like the message was lost" in result

    def test_verbose_decline_includes_manager_escalation_fallback(self) -> None:
        """When the agent can't identify the right owner, the rule
        must offer a manager-escalation path -- otherwise the agent
        is forced to invent an owner or post a vague non-answer."""
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification(routed_via="mention"))
        assert "can't identify the right owner" in result
        assert "your own manager" in result

    def test_verbose_decline_omitted_for_watcher_routing(self) -> None:
        """Watcher routings are NOT direct asks; verbose-decline
        rule must be omitted so routine updates on watched pages
        don't trigger noise."""
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification(routed_via="watcher"))
        assert "directly @mentioned but have decided not" not in result
        assert "do NOT skip silently" not in result

    def test_verbose_decline_omitted_for_space_lead_routing(self) -> None:
        """space_lead already gets the dedicated 'Why You Received
        This' block, so the generic verbose-decline rule would
        be redundant."""
        prompt = ConfluenceNotificationPrompt()
        result = prompt.build(_make_notification(routed_via="space_lead"))
        assert "## Why You Received This" in result
        assert "directly @mentioned but have decided not" not in result


class TestConfluenceRequiresRecon:
    """A Confluence webhook is a pointer -- it names the page that
    changed; the agent must fetch the page content before it has real
    context.  ``requires_recon`` lets the Plan-phase prefetches skip
    aux-LLM filtering on that thin trigger."""

    def test_requires_recon_true_with_page_id(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        assert prompt.requires_recon(_make_notification()) is True

    def test_requires_recon_false_without_page_id(self) -> None:
        prompt = ConfluenceNotificationPrompt()
        assert prompt.requires_recon(_make_notification(page_id="")) is False
