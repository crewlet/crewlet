"""Unit tests for the platform-agnostic ``InboundInteraction`` factory."""

from __future__ import annotations

from crewlet.events.types import (
    A2AMessageSent,
    CoalescedMessage,
    ExternalNotification,
    TaskAssigned,
)
from crewlet.learning.interaction import (
    CanonicalIdentity,
    InboundInteraction,
    interactions_require_recon,
    merge_interactions_by_sender,
    salient_task_text,
)


def _single(event: object, **kwargs: object) -> InboundInteraction:
    """Unwrap the one interaction a plain (non-coalesced) trigger yields."""
    interactions = InboundInteraction.list_from_trigger_event(event, **kwargs)
    assert len(interactions) == 1
    return interactions[0]


def test_factory_returns_empty_list_for_none() -> None:
    assert InboundInteraction.list_from_trigger_event(None) == []


def test_factory_returns_empty_for_internal_task_assigned() -> None:
    event = TaskAssigned(task_id="t-1", agent_id="a-1", role="Engineer")
    interaction = _single(event)
    assert not interaction.has_sender
    # Raw type is captured even for unsupported triggers so audits can
    # tell "we considered this event" from "we got nothing".
    assert interaction.raw_event_type == "task_assigned"


def test_factory_extracts_slack_user_from_metadata() -> None:
    event = ExternalNotification(
        notification_source="slack",
        sender="Sam Founder",
        body="please always commit semantically",
        metadata={"slack_user_id": "U0TESTUSER1", "channel_type": "im"},
    )
    interaction = _single(event)
    assert interaction.has_sender
    assert interaction.sender.platform == "slack"
    assert interaction.sender.external_id == "U0TESTUSER1"
    assert interaction.sender.display_name == "Sam Founder"
    assert interaction.sender.handle == ""
    assert interaction.body == "please always commit semantically"
    assert interaction.channel_kind == "dm"


def test_factory_extracts_jira_actor_account_id() -> None:
    """The counterparty id is the trigger ACTOR (``actor_account_id``,
    the key ``parse_jira_webhook`` stamps on every routed copy) — never
    the recipient-side routing key ``assignee_account_id``, which names
    the receiving agent itself."""
    event = ExternalNotification(
        notification_source="jira",
        sender="Sarah Chen",
        body="please review the rollout plan",
        metadata={"actor_account_id": "abc123", "assignee_account_id": "agent-self"},
    )
    interaction = _single(event)
    assert interaction.has_sender
    assert interaction.sender.external_id == "abc123"
    assert interaction.sender.platform == "jira"
    assert interaction.channel_kind == "public"


def test_factory_jira_recipient_key_never_used_as_sender() -> None:
    """Without an actor key, the sender display name is the honest
    fallback — the routed recipient's ``assignee_account_id`` (or an
    ``accountId`` key production never emits) must not become
    the counterparty identity."""
    event = ExternalNotification(
        notification_source="jira",
        sender="Sarah Chen",
        body="assigned you POC-1",
        metadata={"assignee_account_id": "agent-self", "accountId": "ignored"},
    )
    interaction = _single(event)
    assert interaction.sender.external_id == "Sarah Chen"


def test_factory_extracts_confluence_actor_account_id() -> None:
    """Confluence copies stamp the trigger actor as ``actor_account_id``
    (the only actor key ``transports/confluence.py`` ever emits — no
    ``account_id``/``accountId`` key appears in any payload), so
    the actor id is the counterparty identity, mirroring jira/plane."""
    event = ExternalNotification(
        notification_source="confluence",
        sender="Sarah Chen",
        body="commented on the rollout page",
        metadata={"actor_account_id": "5b10ac8d-real-account", "space_key": "ENG"},
    )
    interaction = _single(event)
    assert interaction.has_sender
    assert interaction.sender.external_id == "5b10ac8d-real-account"
    assert interaction.sender.platform == "confluence"
    assert interaction.channel_kind == "public"


def test_factory_confluence_cloud_shape_not_senderless() -> None:
    """Atlassian Cloud webhooks can arrive with a bare ``userAccountId``
    and no display name, so ``sender`` is empty — the actor id alone
    must keep the interaction identifiable (before the
    ``actor_account_id`` fix this path produced a senderless
    interaction and no counterparty profile at all)."""
    event = ExternalNotification(
        notification_source="confluence",
        sender="",
        body="page updated",
        metadata={"actor_account_id": "5b10ac8d-real-account"},
    )
    interaction = _single(event)
    assert interaction.has_sender
    assert interaction.sender.external_id == "5b10ac8d-real-account"


def test_factory_extracts_plane_actor_id() -> None:
    """Plane copies carry the routed target as ``plane_user_id`` and
    the trigger actor as ``actor_account_id`` — the actor is the
    counterparty."""
    event = ExternalNotification(
        notification_source="plane",
        sender="Priya Patel",
        body="assigned you a work item",
        metadata={
            "actor_account_id": "7f0c6a2e-1b3d-4e5f-8a9b-0c1d2e3f4a5b",
            "plane_user_id": "recipient-uuid",
        },
    )
    interaction = _single(event)
    assert interaction.has_sender
    assert interaction.sender.external_id == "7f0c6a2e-1b3d-4e5f-8a9b-0c1d2e3f4a5b"
    assert interaction.sender.platform == "plane"
    assert interaction.channel_kind == "public"


def test_factory_gitlab_sender_fallback_and_public_channel() -> None:
    """GitLab metadata carries only the routed recipient
    (``gitlab_username``); the actor arrives as the event sender, so
    the fallback is already correct — and the channel kind is
    ``public``, not ``unknown``."""
    event = ExternalNotification(
        notification_source="gitlab",
        sender="dev1",
        body="mentioned you in MR !42",
        metadata={"gitlab_username": "agent-swe"},
    )
    interaction = _single(event)
    assert interaction.has_sender
    assert interaction.sender.external_id == "dev1"
    assert interaction.sender.platform == "gitlab"
    assert interaction.channel_kind == "public"


def test_factory_falls_back_to_sender_name_when_metadata_empty() -> None:
    event = ExternalNotification(
        notification_source="email",
        sender="alice@example.com",
        body="hello",
        metadata={},
    )
    interaction = _single(event)
    assert interaction.has_sender
    assert interaction.sender.external_id == "alice@example.com"
    assert interaction.channel_kind == "dm"


def test_factory_returns_empty_when_no_sender_or_metadata() -> None:
    event = ExternalNotification(
        notification_source="slack",
        sender="",
        body="",
        metadata={},
    )
    interaction = _single(event)
    assert not interaction.has_sender


def test_factory_handles_a2a_message() -> None:
    event = A2AMessageSent(
        channel_id="ch-1",
        sender="bob",
        sender_role="Engineer",
        recipient="alice",
        content="ping",
    )
    interaction = _single(event)
    assert interaction.has_sender
    assert interaction.sender.handle == "bob"
    assert interaction.sender.platform == "a2a"
    assert interaction.body == "ping"
    assert interaction.channel_kind == "internal"


def test_factory_clips_body_to_limit() -> None:
    event = ExternalNotification(
        notification_source="slack",
        sender="sam",
        body="x" * 9000,
        metadata={"slack_user_id": "U1"},
    )
    interaction = _single(event, body_limit=128)
    assert len(interaction.body) == 128


def test_requires_recon_threaded_from_external_notification() -> None:
    """``ExternalNotification.context_requires_recon`` (set by the
    NotificationService from the source prompt builder) flows onto the
    canonical interaction so the Plan-phase prefetches can read it
    without branching on ``event.type``."""
    event = ExternalNotification(
        notification_source="jira",
        sender="Sarah Chen",
        body="A Jira event has been routed to you. POC-518 ...",
        metadata={"actor_account_id": "abc123"},
        context_requires_recon=True,
    )
    interaction = _single(event)
    assert interaction.requires_recon is True
    assert interactions_require_recon([interaction]) is True


def test_requires_recon_defaults_false_for_self_contained_trigger() -> None:
    event = ExternalNotification(
        notification_source="slack",
        sender="Sam",
        body="here is the full question with all the context",
        metadata={"slack_user_id": "U1"},
        context_requires_recon=False,
    )
    interaction = _single(event)
    assert interaction.requires_recon is False


def test_requires_recon_threaded_even_without_identifiable_sender() -> None:
    """A webhook with no resolvable sender still carries the recon
    flag -- the early-return path must not drop it."""
    event = ExternalNotification(
        notification_source="jira",
        sender="",
        body="POC-518 changed",
        metadata={},
        context_requires_recon=True,
    )
    interaction = _single(event)
    assert not interaction.has_sender
    assert interaction.requires_recon is True


def test_requires_recon_false_for_a2a_and_internal_triggers() -> None:
    """A2A messages and internal TaskAssigned triggers carry their own
    context -- they are never recon-requiring."""
    a2a = A2AMessageSent(
        channel_id="ch-1",
        sender="bob",
        sender_role="Engineer",
        recipient="alice",
        content="ping",
    )
    assert (
        interactions_require_recon(InboundInteraction.list_from_trigger_event(a2a))
        is False
    )
    internal = TaskAssigned(task_id="t-1", agent_id="a-1", role="Engineer")
    assert (
        interactions_require_recon(InboundInteraction.list_from_trigger_event(internal))
        is False
    )
    assert interactions_require_recon(None) is False
    assert interactions_require_recon([]) is False


def test_body_prefers_salient_body_over_enriched_body() -> None:
    """``ExternalNotification.body`` is the enriched planner prompt
    (triage scaffolding + the message); ``salient_body`` is the raw
    message.  ``InboundInteraction.body`` -- what learning workers read
    to reason about *what the sender said* -- must be the raw message,
    not the scaffolding."""
    event = ExternalNotification(
        notification_source="slack",
        sender="Sam",
        body=(
            "A Slack message was posted by **Sam**.\n\n## Triage — decide "
            "BEFORE replying\n" + ("boilerplate " * 200)
        ),
        salient_body="who can review the backend fixes this week?",
        metadata={"slack_user_id": "U1"},
    )
    interaction = _single(event)
    assert interaction.body == "who can review the backend fixes this week?"
    assert "Triage" not in interaction.body


def test_body_falls_back_to_enriched_body_when_no_salient_body() -> None:
    """Events emitted without a distinct ``salient_body`` (extensions,
    older producers) still surface *something* as the body.  ``None``
    -- the field default -- means 'no distinct raw message'."""
    event = ExternalNotification(
        notification_source="slack",
        sender="Sam",
        body="the only body we have",
        metadata={"slack_user_id": "U1"},
    )
    interaction = _single(event)
    assert interaction.body == "the only body we have"


def test_empty_salient_body_is_not_replaced_by_scaffolding() -> None:
    """An *empty* ``salient_body`` (the producer set it, the message
    was genuinely contentless) must NOT fall back to the enriched
    ``body`` -- doing so would feed the triage scaffolding to every
    relevance filter.  Empty-but-present is distinct from missing."""
    event = ExternalNotification(
        notification_source="slack",
        sender="Sam",
        body="## Triage — decide BEFORE replying\n" + ("boilerplate " * 200),
        salient_body="",
        metadata={"slack_user_id": "U1"},
    )
    interaction = _single(event)
    assert interaction.body == ""
    assert "Triage" not in interaction.body


# --- Coalesced triggers --------------------------------------------------


def _coalesced_event() -> ExternalNotification:
    """A merged trigger carrying three constituent messages from two
    senders, mirroring what ``coalesce_notifications`` produces."""
    return ExternalNotification(
        notification_source="slack",
        sender="Bob",  # flat fields mirror the LATEST constituent
        body="## Coalesced updates (3 events ...)\n...digest...",
        salient_body="Alice: first\n\nAlice: second\n\nBob: third",
        metadata={"slack_user_id": "U-BOB", "channel": "C1", "thread_ts": "1.0"},
        context_requires_recon=True,  # any() of the per-message flags
        messages=[
            CoalescedMessage(
                sender="Alice",
                body="first",
                metadata={"slack_user_id": "U-ALICE", "channel_type": "channel"},
                context_requires_recon=True,
            ),
            CoalescedMessage(
                sender="Alice",
                body="second",
                metadata={"slack_user_id": "U-ALICE", "channel_type": "channel"},
                context_requires_recon=True,
            ),
            CoalescedMessage(
                sender="Bob",
                body="third",
                metadata={"slack_user_id": "U-BOB", "channel_type": "channel"},
                context_requires_recon=False,
            ),
        ],
    )


def test_coalesced_trigger_yields_one_interaction_per_message() -> None:
    interactions = InboundInteraction.list_from_trigger_event(_coalesced_event())
    assert len(interactions) == 3
    assert [i.body for i in interactions] == ["first", "second", "third"]
    # Sender identity comes from each CONSTITUENT's metadata, not the
    # merged event's flat fields (which mirror only the latest).
    assert interactions[0].sender.external_id == "U-ALICE"
    assert interactions[2].sender.external_id == "U-BOB"
    assert all(i.sender.platform == "slack" for i in interactions)
    # Per-message recon fidelity: each interaction carries its own
    # constituent's flag, and the whole-turn gate is their any() —
    # matching the merged event-level flag.
    assert [i.requires_recon for i in interactions] == [True, True, False]
    assert interactions_require_recon(interactions) is True


def test_merge_interactions_by_sender_joins_same_sender_bodies() -> None:
    interactions = InboundInteraction.list_from_trigger_event(_coalesced_event())
    merged = merge_interactions_by_sender(interactions)
    assert len(merged) == 2
    assert merged[0].sender.external_id == "U-ALICE"
    assert merged[0].body == "first\n\nsecond"
    assert merged[1].sender.external_id == "U-BOB"
    assert merged[1].body == "third"


def test_merge_interactions_by_sender_drops_senderless() -> None:
    assert merge_interactions_by_sender(None) == []
    assert merge_interactions_by_sender([InboundInteraction(body="orphan")]) == []


def test_salient_task_text_single_interaction_is_verbatim() -> None:
    interaction = InboundInteraction(body="who can review the backend fixes this week?")
    assert (
        salient_task_text([interaction], "ENRICHED BOILERPLATE TASK DESCRIPTION")
        == "who can review the backend fixes this week?"
    )


def test_salient_task_text_joins_multiple_with_sender_attribution() -> None:
    interactions = InboundInteraction.list_from_trigger_event(_coalesced_event())
    text = salient_task_text(interactions, "fallback")
    assert text == "Alice: first\n\nAlice: second\n\nBob: third"


def test_salient_task_text_falls_back_when_no_interaction_body() -> None:
    # No interactions at all (internal TaskAssigned trigger).
    assert salient_task_text(None, "internal task description") == (
        "internal task description"
    )
    assert salient_task_text([], "internal task description") == (
        "internal task description"
    )
    # Interaction present but with an empty body.
    empty = InboundInteraction()
    assert salient_task_text([empty], "internal task description") == (
        "internal task description"
    )
    # Everything empty -> empty string, never None.
    assert salient_task_text(None, "") == ""


def test_canonical_identity_helpers() -> None:
    resolved = CanonicalIdentity(handle="alice", platform="a2a")
    assert resolved.is_resolved
    assert resolved.is_identifiable

    external = CanonicalIdentity(external_id="U1", platform="slack")
    assert not external.is_resolved
    assert external.is_identifiable

    empty = CanonicalIdentity()
    assert not empty.is_identifiable


def test_salient_task_text_clips_joined_output() -> None:
    """The singular API implicitly bounded the salient text to one
    clipped body; the joined multi-message form restores that bound so
    embedding queries / filter prompts never receive max_batch × 4000
    chars."""
    interactions = [
        InboundInteraction(
            sender=CanonicalIdentity(external_id=f"U{i}", platform="slack"),
            body="x" * 3000,
        )
        for i in range(5)
    ]
    text = salient_task_text(interactions, "fallback")
    assert len(text) <= 4000 + len(" … [truncated]")
    assert text.endswith(" … [truncated]")
    # Single-body triggers stay verbatim — no truncation marker.
    single = salient_task_text([interactions[0]], "fallback")
    assert single == "x" * 3000


def test_canonical_identity_describe_and_label() -> None:
    full = CanonicalIdentity(external_id="U1", platform="slack", display_name="Sam")
    assert full.describe() == "Sam (slack:U1)"
    assert full.label == "Sam"
    handle_only = CanonicalIdentity(handle="agent-cto", display_name="CTO Agent")
    assert handle_only.describe() == "CTO Agent"
    id_only = CanonicalIdentity(external_id="U2")
    assert id_only.describe() == "unknown:U2"
    assert CanonicalIdentity().describe() == ""
