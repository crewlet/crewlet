"""Canonical, platform-agnostic boundary type for inbound messages.

Every learning worker consumes :class:`InboundInteraction`, never the
raw platform-specific event.  The single platform-aware function in the
subsystem is :meth:`InboundInteraction.list_from_trigger_event` -- the
one place platform-specific trigger metadata (Slack/Jira/Confluence/
GitHub/email sender and channel keys) is extracted.

A turn is triggered by a *list* of inbound messages: usually one, but a
coalesced trigger (several same-conversation notifications merged into
one digest — see :mod:`crewlet.notifications.coalesce`) carries one
interaction per constituent message, possibly from several different
senders.  Workers that reason about *text* join the bodies
(:func:`salient_task_text`); workers that reason about *people* iterate
per distinct sender (:func:`merge_interactions_by_sender`).

Adding a new transport (Discord, Teams, Linear, …) means adding one
branch to ``list_from_trigger_event`` -- nothing else in the learning
code changes.

See ``docs/concepts/agent-learning.md`` for the design rationale.
"""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

ChannelKind = Literal["dm", "group", "public", "internal", "unknown"]


class CanonicalIdentity(BaseModel):
    """A platform-agnostic identifier for an inbound sender.

    Either ``handle`` (resolved Crewlet agent handle) or
    ``external_id`` (platform-specific id for an unresolved external
    human / agent) identifies the sender.  ``platform`` carries
    provenance for downstream lookup; ``display_name`` is best-effort
    for prompt rendering.

    An empty :class:`CanonicalIdentity` (no handle, no external_id)
    means "no identifiable sender" -- e.g. an internal ``TaskAssigned``
    trigger or a system-issued event.  Workers short-circuit on this.
    """

    handle: str = ""
    external_id: str = ""
    platform: str = ""
    display_name: str = ""

    model_config = ConfigDict(frozen=True)

    @property
    def is_resolved(self) -> bool:
        """True when the identity points at a known Crewlet agent."""
        return bool(self.handle)

    @property
    def is_identifiable(self) -> bool:
        """True when *some* identifier is set (handle or external_id)."""
        return bool(self.handle or self.external_id)

    @property
    def label(self) -> str:
        """Best human-readable name: display name > handle > external id."""
        return self.display_name or self.handle or self.external_id

    def describe(self) -> str:
        """Render this identity for an aux-LLM prompt.

        ``Label (platform:external_id)`` with graceful degradation —
        the ONE formatting of a counterparty every learning prompt
        uses (the persist decider's ``Requester:`` lines, the
        personal-memory filter's ``Current sender:`` lines).  Two
        prompts describing the same sender differently would silently
        desynchronize the facts one persists from the lines the other
        filters against.  Empty string when nothing identifies the
        sender.
        """
        label = self.display_name or self.handle
        if label and self.external_id:
            suffix = (
                f" ({self.platform}:{self.external_id})"
                if self.platform
                else f" ({self.external_id})"
            )
            return f"{label}{suffix}"
        if label:
            return label
        if self.external_id:
            return f"{self.platform or 'unknown'}:{self.external_id}"
        return ""


#: Cap on one interaction body, and on any text joined from several.
#:
#: A single body is clipped here at construction; a COALESCED trigger
#: can carry ``max_batch`` of them, so anything that joins bodies has to
#: re-apply the bound or it hands a consumer up to ``max_batch`` times
#: this much. 4000 characters is roughly a thousand tokens — enough for
#: any real chat message plus its quoted context, and small enough that
#: a batch of them still leaves the prompt room for the rest of the
#: turn.
INTERACTION_BODY_LIMIT = 4000


class InboundInteraction(BaseModel):
    """A canonical record of one inbound message that triggered a turn.

    Constructed once at the edge via
    :meth:`list_from_trigger_event`; all learning workers consume this
    type instead of branching on ``event.type`` /
    ``event.notification_source``.

    ``raw_event_type`` is the string event-type tag captured from the
    original event for audit/debugging only -- learning workers must
    never branch on it.  Carrying it lets dashboards / operator-facing
    tools resolve back to which kind of trigger produced this
    interaction without re-fetching the original event.

    ``requires_recon`` is ``True`` when the trigger is a *pointer*
    (a webhook naming a thing-that-changed) rather than self-contained
    context -- the agent must fetch the issue / read the thread / pull
    the diff before it has anything substantive.  The Plan-phase
    relevance-filter prefetches read this and skip their aux-LLM call
    when it is set: filtering against a bare pointer is
    near-guaranteed low-value, and the planner is already told to
    re-query after recon.  This is the one field workers *may* branch
    on -- it is a normalized, platform-agnostic property, not an
    ``event.type`` check.
    """

    sender: CanonicalIdentity = Field(default_factory=CanonicalIdentity)
    body: str = ""
    channel_kind: ChannelKind = "unknown"
    raw_event_type: str = ""
    requires_recon: bool = False

    model_config = ConfigDict(frozen=True)

    @property
    def has_sender(self) -> bool:
        """True when an identifiable counterparty is present."""
        return self.sender.is_identifiable

    @classmethod
    def list_from_trigger_event(
        cls, event: Any, *, body_limit: int = INTERACTION_BODY_LIMIT
    ) -> list[InboundInteraction]:
        """Normalize any platform event into canonical interactions.

        Returns one interaction per inbound message on the trigger:

        - a **coalesced** external notification (``event.messages``
          non-empty) yields one interaction per constituent message,
          in chronological order — each with its own sender identity
          extracted from the constituent's metadata;
        - every other non-``None`` trigger yields exactly one
          interaction, which is *empty* (no sender, no body) when the
          event has no identifiable counterparty -- internal
          ``TaskAssigned`` triggers, scheduled ticks, system events.
          Workers filter on ``has_sender`` instead of on the original
          event's type;
        - ``event is None`` yields ``[]``.

        ``body_limit`` clips each body to a safe size for downstream
        prompt insertion; the full text remains in the original event.
        """
        if event is None:
            return []
        event_type = getattr(event, "type", "") or ""
        if not event_type:
            return []

        if event_type == "external_notification":
            messages = getattr(event, "messages", None) or []
            if messages:
                return [
                    cls._from_coalesced_message(event, message, body_limit=body_limit)
                    for message in messages
                ]
            return [cls._from_external_notification(event, body_limit=body_limit)]
        if event_type == "a2a_message_sent":
            return [cls._from_a2a_message(event, body_limit=body_limit)]
        return [cls(raw_event_type=event_type)]

    # ------------------------------------------------------------------
    # Per-platform factories.  Adding a new transport means adding one
    # branch above and one private classmethod below.  All extraction
    # quirks live here -- nothing else in the codebase touches platform
    # metadata keys for the purpose of building an interaction.
    # ------------------------------------------------------------------

    @classmethod
    def _from_external_notification(
        cls, event: Any, *, body_limit: int
    ) -> InboundInteraction:
        platform = getattr(event, "notification_source", "") or ""
        sender_name = getattr(event, "sender", "") or ""
        # Prefer ``salient_body`` -- the raw inbound message -- over
        # ``body``, which is the enriched planner prompt (the
        # notification builder front-loads ~1.5k chars of triage / how-to
        # scaffolding before the actual message).  Learning workers
        # consume ``InboundInteraction.body`` to reason about *what the
        # sender said*; the scaffolding is identical on every turn and
        # pure noise for that.
        #
        # Distinguish "no ``salient_body`` attribute at all" (an
        # extension-emitted event whose only message is ``body``) from
        # "``salient_body`` is present but empty" (a genuinely
        # contentless message).  An ``a or b`` fallback collapses the
        # two -- and for the empty-salient-body case it would wrongly
        # feed the triage scaffolding to every relevance filter.
        salient = getattr(event, "salient_body", None)
        if salient is not None:
            raw_body = salient or ""
        else:
            raw_body = getattr(event, "body", "") or ""
        return cls._from_notification_fields(
            platform=platform,
            sender_name=sender_name,
            raw_body=raw_body,
            metadata=getattr(event, "metadata", None) or {},
            # Set by the NotificationService from the source prompt
            # builder -- a webhook that names a thing-that-changed
            # without carrying its content (Jira/Confluence page
            # events, GitHub review requests, Slack thread replies).
            requires_recon=bool(getattr(event, "context_requires_recon", False)),
            body_limit=body_limit,
            # Long-standing single-webhook behaviour: a senderless
            # notification contributes no interaction body (its
            # ``salient_task_text`` falls back to the task
            # description).
            keep_body_when_senderless=False,
        )

    @classmethod
    def _from_coalesced_message(
        cls, event: Any, message: Any, *, body_limit: int
    ) -> InboundInteraction:
        """One interaction for one constituent of a coalesced trigger.

        Sender identity is extracted from the *constituent's* metadata
        (each original webhook's keys ride along on the
        ``CoalescedMessage``) so a multi-human thread yields one
        identifiable sender per message — the merged event's flat
        fields only mirror the latest constituent.  ``requires_recon``
        is the constituent's own flag (carried on the
        ``CoalescedMessage`` precisely so the merge doesn't destroy
        it); the whole-turn gate is its ``any()`` via
        :func:`interactions_require_recon`, matching the merged
        event-level flag.
        """
        return cls._from_notification_fields(
            platform=getattr(event, "notification_source", "") or "",
            sender_name=getattr(message, "sender", "") or "",
            raw_body=getattr(message, "body", "") or "",
            metadata=getattr(message, "metadata", None) or {},
            requires_recon=bool(getattr(message, "context_requires_recon", False)),
            body_limit=body_limit,
            # A senderless constituent's text is still part of the
            # conversation the digest presents — keep it for the
            # salient-text join.
            keep_body_when_senderless=True,
        )

    @classmethod
    def _from_notification_fields(
        cls,
        *,
        platform: str,
        sender_name: str,
        raw_body: str,
        metadata: dict[str, Any],
        requires_recon: bool,
        body_limit: int,
        keep_body_when_senderless: bool,
    ) -> InboundInteraction:
        """Shared constructor tail for notification-shaped sources.

        One copy of the id-extraction / identity-construction rules so
        single-webhook and coalesced-constituent interactions can never
        drift; the two factories above only differ in where the fields
        come from and in the (explicit) senderless-body policy.
        """
        body = raw_body[:body_limit]
        channel_kind = _channel_kind_for_external(platform, metadata)
        external_id = _extract_external_id(platform, metadata, sender_name)
        if not external_id and not sender_name:
            return cls(
                body=body if keep_body_when_senderless else "",
                channel_kind=channel_kind,
                raw_event_type="external_notification",
                requires_recon=requires_recon,
            )
        # External notifications carry external humans, not internal
        # agents -- handle stays empty.  Cross-platform identity
        # resolution (platform external_id -> Crewlet handle) is left
        # to callers that need it; the boundary type itself stays
        # platform-agnostic and pure.
        sender = CanonicalIdentity(
            handle="",
            external_id=external_id or sender_name,
            platform=platform,
            display_name=sender_name,
        )
        return cls(
            sender=sender,
            body=body,
            channel_kind=channel_kind,
            raw_event_type="external_notification",
            requires_recon=requires_recon,
        )

    @classmethod
    def _from_a2a_message(cls, event: Any, *, body_limit: int) -> InboundInteraction:
        sender_handle = getattr(event, "sender", "") or ""
        body = (getattr(event, "content", "") or "")[:body_limit]
        if not sender_handle:
            return cls(raw_event_type="a2a_message_sent")
        sender = CanonicalIdentity(
            handle=sender_handle,
            external_id="",
            platform="a2a",
            display_name=sender_handle,
        )
        return cls(
            sender=sender,
            body=body,
            channel_kind="internal",
            raw_event_type="a2a_message_sent",
        )


def salient_task_text(
    interactions: list[InboundInteraction] | None,
    fallback: str,
    *,
    limit: int = INTERACTION_BODY_LIMIT,
) -> str:
    """Return the inbound message(s) stripped of notification scaffolding.

    Notification-triggered turns carry a ``task_description`` that
    front-loads ~1.5k chars of triage / how-to boilerplate before the
    actual message -- a relevance filter or vector query keyed on a
    prefix of that string never sees the message, and matches against
    boilerplate that is identical on every turn.
    :attr:`InboundInteraction.body` is the raw inbound message (the
    notification builder's salient body, or the A2A message content),
    so prefer it.  A single-message trigger renders its body verbatim
    (byte-identical to the pre-coalescing behaviour); a coalesced
    trigger joins the bodies chronologically, sender-attributed so the
    filter can tell who said what.  Internal triggers (``TaskAssigned``,
    scheduled ticks) have no interaction body -- fall back to
    ``fallback``, the turn's task description, which for those is
    already scaffold-free.

    ``limit`` clips the *joined* text: each constituent body is already
    clipped at construction, but a coalesced trigger can carry up to
    ``max_batch`` of them, and consumers feed this straight into
    embedding queries and aux-filter prompts that the singular API
    implicitly bounded to one body.  The default restores that bound.
    """
    with_body = [i for i in (interactions or []) if i.body]
    if not with_body:
        return fallback or ""
    if len(with_body) == 1:
        text = with_body[0].body
    else:
        lines: list[str] = []
        for interaction in with_body:
            label = interaction.sender.label
            lines.append(f"{label}: {interaction.body}" if label else interaction.body)
        text = "\n\n".join(lines)
    if limit > 0 and len(text) > limit:
        return text[:limit] + " … [truncated]"
    return text


def interactions_require_recon(interactions: list[InboundInteraction] | None) -> bool:
    """Whether the trigger as a whole is a *pointer* needing recon.

    Conservative merge over the trigger's interactions: one
    recon-requiring constituent gates the whole turn's thin-trigger
    prefetch logic, exactly as a single recon-requiring notification
    did before coalescing.  Empty / ``None`` (internal triggers) is
    ``False``.
    """
    return any(i.requires_recon for i in (interactions or []))


def merge_interactions_by_sender(
    interactions: list[InboundInteraction] | None,
) -> list[InboundInteraction]:
    """One merged interaction per distinct identifiable sender.

    Workers that reason about *people* (the counterparty profiler, the
    known-counterparty prefetch) need one observation pass per sender,
    not per message: a coalesced thread where one human sent four
    messages is still one counterparty, and a multi-human thread is
    genuinely several.  Bodies of the same sender are joined
    chronologically; senderless interactions are dropped (nothing to
    profile).  First-seen order is preserved.
    """
    # Single pass accumulating per-sender state, one merged interaction
    # constructed per sender at the end — no intermediate model copies
    # or quadratic body re-joins for chatty senders.
    grouped: dict[tuple[str, str, str], list[InboundInteraction]] = {}
    for interaction in interactions or []:
        if not interaction.has_sender:
            continue
        sender = interaction.sender
        key = (sender.handle, sender.external_id, sender.platform)
        grouped.setdefault(key, []).append(interaction)

    merged: list[InboundInteraction] = []
    for group in grouped.values():
        first = group[0]
        if len(group) == 1:
            merged.append(first)
            continue
        # Capped like `interaction_text`, and for the same reason: each
        # constituent body is already clipped at construction, but a
        # coalesced trigger can carry `max_batch` of them from one
        # chatty sender — and this merged body goes straight into the
        # counterparty profiler's prompt.
        joined = "\n\n".join(i.body for i in group if i.body)
        if len(joined) > INTERACTION_BODY_LIMIT:
            joined = joined[:INTERACTION_BODY_LIMIT] + " … [truncated]"
        merged.append(
            first.model_copy(
                update={
                    "body": joined,
                    "requires_recon": any(i.requires_recon for i in group),
                }
            )
        )
    return merged


# ----------------------------------------------------------------------
# Platform-metadata extraction helpers.  These are the only place in the
# learning code that names specific platform keys.  Adding a transport
# means extending the map below.
# ----------------------------------------------------------------------


_EXTERNAL_ID_KEYS_BY_PLATFORM: dict[str, tuple[str, ...]] = {
    "slack": ("slack_user_id", "user_id", "user"),
    "mattermost": ("user_id", "user"),
    # Webhook transports stamp the TRIGGER ACTOR's id on every routed
    # copy as ``actor_account_id`` — that is the counterparty whose
    # profile this feeds.  Recipient-side routing keys
    # (``assignee_account_id``, ``plane_user_id``, ``gitlab_username``)
    # name the receiving agent itself and must never be read here.
    # GitLab carries no actor-id metadata key at all: its actor
    # username arrives as the event sender, which the fallback in
    # ``_extract_external_id`` already returns.
    "jira": ("actor_account_id",),
    "confluence": ("actor_account_id",),
    "plane": ("actor_account_id",),
    "github": ("login", "user_login"),
    "email": ("sender_email", "email"),
}


def _extract_external_id(platform: str, metadata: dict[str, Any], fallback: str) -> str:
    """Best-effort platform-id extraction from notification metadata."""
    for key in _EXTERNAL_ID_KEYS_BY_PLATFORM.get(platform, ()):
        value = metadata.get(key)
        if value:
            return str(value)
    return fallback


#: Per-chat-backend mapping from the platform's own ``channel_type``
#: vocabulary onto the coarse kinds used for prompt flavour.  A map per
#: platform rather than a chain of ``if platform == ...`` branches:
#: every chat backend has its own single-word vocabulary for the same
#: four ideas, and a second branch is how the third backend gets
#: forgotten.  Mattermost's values are the raw single letters the server
#: stamps (``O``pen / ``P``rivate / ``D``irect / ``G``roup), lowercased
#: here like every other platform's.
_CHANNEL_KIND_BY_PLATFORM: dict[str, dict[str, ChannelKind]] = {
    "slack": {
        "im": "dm",
        "dm": "dm",
        "group": "group",
        "mpim": "group",
        "public": "public",
        "channel": "public",
    },
    "mattermost": {
        "d": "dm",
        "g": "group",
        "o": "public",
        "p": "group",
    },
}


def _channel_kind_for_external(platform: str, metadata: dict[str, Any]) -> ChannelKind:
    """Derive a coarse channel category for prompt-context flavour.

    Used only for descriptive context; never branched on for behaviour.
    """
    vocabulary = _CHANNEL_KIND_BY_PLATFORM.get(platform)
    if vocabulary is not None:
        channel_type = str(metadata.get("channel_type", "") or "").lower()
        kind = vocabulary.get(channel_type)
        if kind is not None:
            return kind
    if platform == "email":
        return "dm"
    if platform in ("jira", "confluence", "github", "gitlab", "plane"):
        return "public"
    return "unknown"
