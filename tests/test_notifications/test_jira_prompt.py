"""Tests for JiraNotificationPrompt changelog rendering."""

from __future__ import annotations

import json

from crewlet.notifications.handle import ResolvedParty
from crewlet.notifications.notification_prompts.jira import JiraNotificationPrompt
from crewlet.notifications.protocol import InboundNotification
from crewlet.org.models import RoleKind


class _FakeRegistry:
    """Stand-in for :class:`HandleRegistry` exposing only the surface
    the prompt builder consumes (``resolve_party_external``).  Returns
    a real :class:`ResolvedParty` so kind-dependent rendering is
    exercised."""

    def __init__(self, mapping: dict[str, tuple[str, ...]]):
        # mapping: account_id -> (handle, role_name[, kind])
        self._mapping = mapping

    def resolve_party_external(self, transport: str, external_id: str):
        if transport != "jira":
            return None
        if external_id not in self._mapping:
            return None
        entry = self._mapping[external_id]
        handle, role = entry[0], entry[1]
        kind = RoleKind(entry[2]) if len(entry) > 2 else RoleKind.AGENT
        return ResolvedParty(kind=kind, handle=handle, name=role, role=None, agent=None)


class TestJiraPromptChangelog:
    def _make_notification(
        self, changelog_items: list[dict] | None = None, **meta_overrides: str
    ) -> InboundNotification:
        metadata: dict[str, str] = {
            "issue_key": "PROJ-1",
            "event_type": "jira:issue_updated",
            "project": "PROJ",
            "assignee_account_id": "abc",
            "actor_name": "Manager",
            "timestamp": "1711756800000",
        }
        if changelog_items is not None:
            metadata["changelog"] = json.dumps(changelog_items)
        metadata.update(meta_overrides)
        return InboundNotification(
            source="jira",
            source_event_type="jira:issue_updated",
            sender="Manager",
            subject="[PROJ-1] Fix bug",
            body="",
            metadata=metadata,
        )

    def test_changelog_rendered_in_prompt(self):
        items = [
            {
                "field": "status",
                "fromString": "To Do",
                "toString": "In Progress",
            },
            {
                "field": "priority",
                "fromString": "Medium",
                "toString": "High",
            },
        ]
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(changelog_items=items))

        assert "## What Changed" in result
        assert "**status**: To Do → In Progress" in result
        assert "**priority**: Medium → High" in result

    def test_changelog_none_values(self):
        """Null fromString with a non-null toString renders as (none)."""
        items = [
            {
                "field": "assignee",
                "fromString": None,
                "toString": "Alice",
            },
        ]
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(changelog_items=items))

        assert "**assignee**: (none) → Alice" in result

    def test_changelog_falls_back_to_raw_ids_when_strings_null(self):
        """When the bridge drops ``fromString``/``toString`` (e.g.
        Forge sometimes only relays the raw account-id ``from``/``to``
        fields) we render the IDs rather than the ``(none) → (none)``
        placeholder that masked the real change in production."""
        items = [
            {
                "field": "assignee",
                "from": None,
                "fromString": None,
                "to": "712020:abc-def",
                "toString": None,
            },
        ]
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(changelog_items=items))

        assert "**assignee**: (none) → 712020:abc-def" in result
        # Crucial: a degenerate "(none) → (none)" rendering MUST NOT appear.
        assert "(none) → (none)" not in result

    def test_changelog_drops_empty_lines(self):
        """When *both* sides resolve to empty after the fall-back,
        the line carries no signal and is dropped -- otherwise every
        no-op item would render as ``(none) → (none)``."""
        items = [
            {
                "field": "assignee",
                "from": None,
                "fromString": None,
                "to": None,
                "toString": None,
            },
            {
                "field": "status",
                "fromString": "To Do",
                "toString": "In Progress",
            },
        ]
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(changelog_items=items))

        assert "(none) → (none)" not in result
        assert "**assignee**" not in result
        assert "**status**: To Do → In Progress" in result

    def test_changelog_description_summarised_not_diffed(self):
        """Description edits would otherwise dump both the full pre-
        and post-edit body — a wall of text on every issue update.
        Render a one-line "was updated" summary instead."""
        items = [
            {
                "field": "description",
                "fromString": "Original long description ..." * 50,
                "toString": "Updated long description ..." * 50,
            },
        ]
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(changelog_items=items))

        assert "**description**: was updated" in result
        assert "Original long description" not in result
        assert "Updated long description" not in result

    def test_event_lead_names_comment_add(self):
        """A ``comment_created`` event surfaces a one-line lead naming
        the event kind so the agent doesn't have to infer it from the
        changelog shape."""
        prompt = JiraNotificationPrompt()
        notif = self._make_notification(
            changelog_items=None,
            event_type="comment_created",
        )
        result = prompt.build(notif)
        assert "A comment was added by Manager." in result

    def test_event_lead_names_issue_update(self):
        prompt = JiraNotificationPrompt()
        notif = self._make_notification(
            changelog_items=None,
            event_type="jira:issue_updated",
        )
        result = prompt.build(notif)
        assert "The issue was updated by Manager." in result

    def test_comment_body_included(self):
        """Comment-add events surface the comment body as ``Details:``
        — that text is the actual signal of what was said."""
        prompt = JiraNotificationPrompt()
        notif = self._make_notification(
            changelog_items=None,
            event_type="comment_created",
        )
        notif.body = "Hey team, can we sync on this?"
        result = prompt.build(notif)
        assert "Details:" in result
        assert "Hey team, can we sync on this?" in result

    def test_issue_updated_body_suppressed(self):
        """For ``issue_updated`` the body is the full description.
        The changelog already says "description was updated"; dumping
        the full text again would duplicate it as a wall of text."""
        prompt = JiraNotificationPrompt()
        notif = self._make_notification(
            changelog_items=[
                {
                    "field": "description",
                    "fromString": "old",
                    "toString": "new full description body",
                }
            ],
            event_type="jira:issue_updated",
        )
        notif.body = "new full description body"
        result = prompt.build(notif)
        assert "Details:" not in result
        assert "new full description body" not in result

    def test_no_changelog_no_section(self):
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(changelog_items=None))

        assert "## What Changed" not in result

    def test_empty_changelog_no_section(self):
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(changelog_items=[]))

        assert "## What Changed" not in result

    def test_assignee_account_id_resolved_via_registry(self):
        """Forge's bare ``{to: "712020:..."}`` for an assignee change
        should render with the friendly handle when the registry
        knows the account ID — saves the agent a ``lookup_colleague``
        round-trip on every notification.  This is the exact shape
        Forge sends in production."""
        items = [
            {
                "to": "712020:00000000-0000-0000-0000-000000000001",
                "field": "assignee",
                "fieldId": "assignee",
                "fieldtype": "jira",
            }
        ]
        registry = _FakeRegistry(
            {"712020:00000000-0000-0000-0000-000000000001": ("agent-cto", "Agent CTO")}
        )
        prompt = JiraNotificationPrompt()
        result = prompt.build(
            self._make_notification(changelog_items=items),
            handle_registry=registry,
        )
        assert "**assignee**: (none) → Agent CTO (agent-cto)" in result
        # Raw account ID must NOT appear once it's resolved.
        assert "712020:00000000" not in result

    def test_assignee_unresolved_falls_back_to_raw_id(self):
        """An account ID the registry doesn't know stays as the raw
        ID — strictly more informative than ``(none)``."""
        items = [
            {
                "to": "712020:unknown-account",
                "field": "assignee",
                "fieldtype": "jira",
            }
        ]
        registry = _FakeRegistry({})  # empty registry
        prompt = JiraNotificationPrompt()
        result = prompt.build(
            self._make_notification(changelog_items=items),
            handle_registry=registry,
        )
        assert "**assignee**: (none) → 712020:unknown-account" in result

    def test_actor_resolved_via_registry_when_displayname_empty(self):
        """Forge bodies carry only ``{accountId: ...}`` for users (no
        displayName), so the parser routinely emits an empty
        ``actor_name``.  When ``actor_account_id`` is set we should
        recover the actor's identity through the registry rather
        than rendering a leading ``A comment was added by .`` line."""
        registry = _FakeRegistry(
            {"712020:00000000-0000-0000-0000-000000000002": ("agent-pm", "Agent PM")}
        )
        prompt = JiraNotificationPrompt()
        notif = self._make_notification(
            changelog_items=None,
            event_type="comment_created",
            actor_name="",  # Forge gave no displayName
            actor_account_id="712020:00000000-0000-0000-0000-000000000002",
        )
        notif.sender = ""  # no fallback from notification.sender either
        result = prompt.build(notif, handle_registry=registry)
        assert "A comment was added by Agent PM (agent-pm)." in result

    def test_string_fields_preserve_human_value_over_registry_lookup(self):
        """When ``fromString``/``toString`` ARE populated (Atlassian
        rendered them), we must NOT overwrite them with a registry
        lookup -- the human display name from Atlassian is the
        canonical label and may differ from the agent handle."""
        items = [
            {
                "field": "assignee",
                "fromString": "Old Person",
                "toString": "Real Display Name",
                "to": "712020:00000000-0000-0000-0000-000000000001",
            }
        ]
        registry = _FakeRegistry(
            {"712020:00000000-0000-0000-0000-000000000001": ("agent-cto", "Agent CTO")}
        )
        prompt = JiraNotificationPrompt()
        result = prompt.build(
            self._make_notification(changelog_items=items),
            handle_registry=registry,
        )
        assert "**assignee**: Old Person → Real Display Name" in result
        # Registry-resolved label must NOT clobber the rendered string.
        assert "Agent CTO (agent-cto)" not in result

    def test_handling_guidance_points_at_lookup_colleague_not_query_knowledge(self):
        """Resolving a teammate's Jira account ID is a roster lookup --
        ``lookup_colleague``'s job.  ``query_knowledge`` only searches
        Confluence + the agent's diary, so directing the agent there
        sent it on a wild goose chase (3 wasted calls in a real trace).
        The guidance must name the right tool."""
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification())
        # The acting-guidance step names lookup_colleague for ID resolution.
        assert "lookup_colleague" in result
        assert "Jira account ID" in result
        # And no misdirection toward a knowledge lookup ("use
        # `query_knowledge` for teammate Jira usernames") appears.
        assert "`query_knowledge` for teammate" not in result


class TestJiraLeadFallbackHint:
    """When the only reason a lead received the event is that the issue
    had no specific recipient, the prompt must surface that explicitly
    and lay out the delegate / take-it / skip decision -- otherwise the
    lead silently absorbs work that should be delegated."""

    @staticmethod
    def _make_notification(**meta_overrides: str) -> InboundNotification:
        metadata: dict[str, str] = {
            "issue_key": "PROJ-1",
            "event_type": "jira:issue_created",
            "project": "PROJ",
            "actor_name": "Creator",
            "timestamp": "1711756800000",
        }
        metadata.update(meta_overrides)
        return InboundNotification(
            source="jira",
            source_event_type="jira:issue_created",
            sender="Creator",
            subject="[PROJ-1] New ticket",
            body="",
            metadata=metadata,
        )

    def test_routed_via_rendered_in_metadata(self) -> None:
        """``routed_via`` is set by the transport for every routing
        decision; it must appear in the Event Metadata block so the
        lead can tell at a glance whether they're a personal recipient
        (watcher / assignee / mention) or a fallback recipient."""
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(routed_via="watcher"))
        assert "**Routed via:** watcher" in result

    def test_lead_fallback_block_present_for_project_lead_fallback(self) -> None:
        prompt = JiraNotificationPrompt()
        result = prompt.build(
            self._make_notification(routed_via="project_lead_fallback")
        )
        assert "## Why You Received This" in result
        # Names the project so the lead knows which board fell back.
        assert "**PROJ**" in result
        # Names the three explicit decisions.
        assert "**Delegate**" in result
        assert "**Take it yourself**" in result
        assert "**Escalate**" in result
        # ``lookup_colleague`` is the stable builtin for resolving teammates;
        # the prompt names it explicitly.  Specific MCP action tool names
        # (``jira_update_issue`` etc.) are intentionally NOT hard-coded
        # because the colleague-wrapper layer was removed and MCP tools
        # change names upstream -- the prompt describes intent instead.
        assert "lookup_colleague" in result
        # The third decision must steer the lead to surface this up
        # rather than walking away silently, since nobody else is
        # watching a lead-fallback-routed issue.
        assert "manager" in result
        assert "no one else is watching" in result

    def test_lead_fallback_block_omits_project_when_missing(self) -> None:
        """A malformed payload missing the project key must not crash
        the hint -- fall back to a generic phrasing rather than"""
        """ rendering ``the **** Jira project``."""
        prompt = JiraNotificationPrompt()
        result = prompt.build(
            self._make_notification(routed_via="project_lead_fallback", project="")
        )
        assert "## Why You Received This" in result
        assert "this Jira project" in result
        # No malformed empty-bold marker from a missing project key.
        assert "**** Jira project" not in result

    def test_lead_fallback_block_absent_for_watcher(self) -> None:
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(routed_via="watcher"))
        assert "## Why You Received This" not in result

    def test_lead_fallback_block_absent_for_assignee(self) -> None:
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(routed_via="assignee"))
        assert "## Why You Received This" not in result

    def test_lead_fallback_block_absent_for_mention(self) -> None:
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(routed_via="mention"))
        assert "## Why You Received This" not in result

    def test_lead_fallback_block_absent_when_routed_via_missing(self) -> None:
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification())
        assert "## Why You Received This" not in result


class TestJiraVerboseDecline:
    """When the agent was directly assigned or @mentioned
    but is declining to act, the prompt must require a brief comment
    (naming the right owner, with manager escalation when the owner
    is unknown) instead of a silent skip.  The rule is gated to
    routings that represent a direct ask -- watcher events get the
    routine guidance only, and project_lead_fallback uses its own
    dedicated block."""

    @staticmethod
    def _make_notification(**meta_overrides: str) -> InboundNotification:
        metadata: dict[str, str] = {
            "issue_key": "PROJ-1",
            "event_type": "jira:issue_assigned",
            "project": "PROJ",
            "actor_name": "Manager",
            "timestamp": "1711756800000",
            "routed_via": "assignee",
        }
        metadata.update(meta_overrides)
        return InboundNotification(
            source="jira",
            source_event_type="jira:issue_assigned",
            sender="Manager",
            subject="[PROJ-1] Fix bug",
            body="",
            metadata=metadata,
        )

    def test_verbose_decline_guidance_present_for_assignee(self) -> None:
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(routed_via="assignee"))
        # Anchor phrase from the rule.
        assert "directly assigned or @mentioned" in result
        # Actionable instruction.
        assert "do NOT skip silently" in result
        # Unified justification phrase across all four prompts.
        assert "looks like the message was lost" in result

    def test_verbose_decline_guidance_present_for_mention(self) -> None:
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(routed_via="mention"))
        assert "directly assigned or @mentioned" in result

    def test_verbose_decline_includes_manager_escalation_fallback(self) -> None:
        """When the agent can't identify the right owner, the rule
        must offer a manager-escalation path -- without it the agent
        is forced to invent an owner or post a vague non-answer."""
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(routed_via="assignee"))
        assert "can't identify the right owner" in result
        assert "your own manager" in result

    def test_verbose_decline_omitted_for_watcher_routing(self) -> None:
        """Watcher routings are NOT direct asks -- the agent receives
        the event because they once interacted, not because someone
        is asking now.  Step 5 (verbose-decline) must be omitted so
        watcher events don't trigger 'this isn't mine, ask X'
        running-commentary noise on every routine update."""
        prompt = JiraNotificationPrompt()
        result = prompt.build(self._make_notification(routed_via="watcher"))
        # The rule's anchor phrase must NOT appear for watchers.
        assert "directly assigned or @mentioned" not in result
        # And the unified anchor must not appear either.
        assert "do NOT skip silently" not in result

    def test_verbose_decline_omitted_for_project_lead_fallback(self) -> None:
        """project_lead_fallback already gets the dedicated 'Why You
        Received This' block (with delegate / take / escalate), so
        the generic verbose-decline rule would be redundant."""
        prompt = JiraNotificationPrompt()
        result = prompt.build(
            self._make_notification(routed_via="project_lead_fallback")
        )
        # The dedicated block is present.
        assert "## Why You Received This" in result
        # The generic verbose-decline rule is NOT.
        assert "directly assigned or @mentioned" not in result


class TestJiraRequiresRecon:
    """A Jira webhook with an identifiable issue key is a pointer --
    ``build()`` tells the agent to fetch the full issue before acting,
    so the Plan-phase prefetches should skip aux-LLM filtering on the
    thin trigger."""

    def test_requires_recon_true_with_issue_key_in_metadata(self) -> None:
        prompt = JiraNotificationPrompt()
        notification = InboundNotification(
            source="jira",
            source_event_type="jira:issue_updated",
            sender="Manager",
            subject="Something changed",
            body="",
            metadata={"issue_key": "PROJ-7", "event_type": "jira:issue_updated"},
        )
        assert prompt.requires_recon(notification) is True

    def test_requires_recon_true_when_key_only_in_subject(self) -> None:
        # ``_extract_issue_key`` is the fallback when metadata carries
        # no ``issue_key``; it scans whitespace-split tokens, so the
        # key must be a bare ``PROJ-9`` token (no brackets / colon).
        prompt = JiraNotificationPrompt()
        notification = InboundNotification(
            source="jira",
            source_event_type="jira:issue_updated",
            sender="Manager",
            subject="PROJ-9 Fix the bug",
            body="",
            metadata={"event_type": "jira:issue_updated"},
        )
        assert prompt.requires_recon(notification) is True

    def test_requires_recon_false_without_resolvable_issue_key(self) -> None:
        prompt = JiraNotificationPrompt()
        notification = InboundNotification(
            source="jira",
            source_event_type="jira:issue_updated",
            sender="Manager",
            subject="a generic subject with no key",
            body="",
            metadata={"event_type": "jira:issue_updated"},
        )
        assert prompt.requires_recon(notification) is False


class TestHumanSenderAttribution:
    def test_human_assignee_renders_human_colleague_label(self):
        items = [
            {
                "to": "5b10-sarah",
                "field": "assignee",
                "fieldId": "assignee",
                "fieldtype": "jira",
            }
        ]
        notification = InboundNotification(
            source="jira",
            subject="[PROJ-1] Fix bug",
            metadata={
                "issue_key": "PROJ-1",
                "event_type": "issue_updated",
                "changelog": json.dumps(items),
            },
        )
        registry = _FakeRegistry({"5b10-sarah": ("sarah-chen", "Sarah Chen", "human")})
        result = JiraNotificationPrompt().build(notification, handle_registry=registry)
        assert "Sarah Chen (sarah-chen, human colleague)" in result
