"""Built-in event types for the Crewlet engine."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any
from uuid import UUID, uuid4

from pydantic import BaseModel, Field

from crewlet.learning.interaction import InboundInteraction
from crewlet.telemetry import current_parent_span_id, current_span_id, current_trace_id


class Event(BaseModel):
    """Base event model. All events in the system inherit from this.

    Trace context is automatically captured from the active
    OpenTelemetry span at creation time.  When events are created
    inside an OTel span, they inherit its trace/span IDs — no manual
    propagation needed.

    Subclasses override ``summary`` to provide a human-readable
    description used in the dashboard trace view.
    """

    id: UUID = Field(default_factory=uuid4)
    type: str
    timestamp: datetime = Field(default_factory=lambda: datetime.now(UTC))
    source: str = ""
    payload: dict[str, Any] = Field(default_factory=dict)

    # Automatically captured from the active OTel span.
    trace_id: str = Field(default_factory=current_trace_id)
    span_id: str = Field(default_factory=current_span_id)
    parent_span_id: str = Field(default_factory=current_parent_span_id)

    # Turn-engine bookkeeping. Populated by the
    # TurnEngine on outbound events and forwarded by the
    # NotificationService / A2AService so an agent woken by a
    # colleague handoff inherits the correct depth / chain.
    delegation_depth: int = 0
    parent_turn_id: str = ""
    delegation_chain: list[str] = Field(default_factory=list)

    @property
    def summary(self) -> str:
        """Human-readable 'who did what' description for the dashboard."""
        return self.type.replace("_", " ").title()

    @property
    def actor(self) -> str:
        """Human-readable actor name — prefers role over source over ID."""
        for field in ("role", "source"):
            val = getattr(self, field, "")
            if val:
                return val
        return getattr(self, "agent_id", "") or "system"


# Event types that ARE a failure by their very type, independent of any
# payload flag.  Named here, beside the events themselves, because three
# layers need the same answer -- the live projection stamping its feed,
# the event store reading a row back from history, and the API
# serializing a push -- and a second copy is how the same turn ends up
# red on one surface and not on another.
FAILURE_EVENT_TYPES = frozenset(
    {
        "task_failed",
        "llm_unavailable",
        "budget_exhausted",
        "turn.guard_breach",
    }
)


def event_failed(
    event_type: str, *, payload_failed: bool = False, tag_failed: bool = False
) -> bool:
    """Whether the work an event reports failed.

    ``payload_failed`` is the event's own ``failed`` field, available
    live; ``tag_failed`` is the ``failed`` tag the event-store writer
    stamps, which is all that survives into history (``list_events``
    never selects the payload column).  Either one, or a type that is
    itself a failure, means failed.
    """
    return bool(payload_failed) or bool(tag_failed) or event_type in FAILURE_EVENT_TYPES


def describe_trigger(event: Event | None) -> dict[str, Any]:
    """Compact descriptor of the event that triggered an agent turn.

    Threaded onto the per-phase telemetry events
    (:class:`AgentPhaseStarted`, :class:`AgentPhaseCompleted`,
    :class:`AgentTurnProgress`, :class:`AgentTurnCompleted`) so the
    dashboard can show *what caused* each LLM invocation -- the task
    assignment, notification, A2A request, or schedule tick that woke
    the agent -- as the source of the turn.

    Returns an empty dict when there is no trigger (e.g. an
    engine-internal turn), which the dashboard renders as "no source".
    The ``id`` lets the dashboard link to the full event detail when
    the trigger event was persisted.

    External-notification triggers additionally carry the originating
    ``integration`` (slack / jira / github / …), the human ``sender``,
    and the ``source_event_type`` so the dashboard can label the
    invocation's source with the actual integration -- a branded
    Slack/Jira badge with the sender -- instead of a generic "external
    notification". These keys are absent on every other trigger type,
    which the dashboard renders with its plain type label.
    """
    if event is None:
        return {}
    timestamp = ""
    ts = getattr(event, "timestamp", None)
    if ts is not None:
        timestamp = ts.isoformat() if hasattr(ts, "isoformat") else str(ts)
    descriptor: dict[str, Any] = {
        "id": str(event.id),
        "type": event.type,
        "summary": event.summary,
        "actor": event.actor,
        "timestamp": timestamp,
    }
    if integration := getattr(event, "notification_source", ""):
        descriptor["integration"] = integration
        if sender := getattr(event, "sender", ""):
            descriptor["sender"] = sender
        if source_event_type := getattr(event, "source_event_type", ""):
            descriptor["source_event_type"] = source_event_type
    return descriptor


# --- Lifecycle events ---


class OrgStarted(Event):
    type: str = "org_started"
    org_name: str = ""

    @property
    def summary(self) -> str:
        return f"Organization '{self.org_name or self.source}' started"


class OrgStopped(Event):
    type: str = "org_stopped"
    org_name: str = ""

    @property
    def summary(self) -> str:
        return f"Organization '{self.org_name or self.source}' stopped"


class AgentSpawned(Event):
    type: str = "agent_spawned"
    agent_id: str = ""
    role: str = ""

    @property
    def summary(self) -> str:
        return f"{self.actor} joined the organization"


class AgentTerminated(Event):
    type: str = "agent_terminated"
    agent_id: str = ""
    role: str = ""
    reason: str = ""

    @property
    def summary(self) -> str:
        msg = f"{self.actor} was terminated"
        return f"{msg}: {self.reason}" if self.reason else msg


class AgentReassigned(Event):
    type: str = "agent_reassigned"
    agent_id: str = ""
    old_role: str = ""
    new_role: str = ""
    old_manager: str = ""
    new_manager: str = ""

    @property
    def summary(self) -> str:
        return f"Agent reassigned from {self.old_role} to {self.new_role}"


class RoleUpdated(Event):
    """Emitted when an existing role's definition changes during config reload."""

    type: str = "role_updated"
    role: str = ""
    agent_id: str = ""
    changed_fields: list[str] = Field(default_factory=list)

    @property
    def summary(self) -> str:
        if self.changed_fields:
            return f"{self.actor}'s role updated ({', '.join(self.changed_fields)})"
        return f"{self.actor}'s role updated"


# --- Live config-management events ---


class ConfigRevisionActivated(Event):
    """Published by the API when a new ``company_config`` revision is
    activated (``PUT /config``, per-entity write, revert, or CLI import).

    The engine subscribes to this topic, fetches the payload from the
    DB, and calls :meth:`Engine.apply_config` to bring the live state
    in line.  The API process also subscribes (distinct consumer
    group) to rebuild its in-memory ``app.state.org_data`` / webhook
    routing / auth-bound metadata.
    """

    type: str = "config_revision_activated"
    revision_id: str = ""
    revision_summary: str = ""
    source: str = ""
    created_by: str = ""

    @property
    def summary(self) -> str:
        if self.revision_summary:
            return f"Config revision activated: {self.revision_summary}"
        return f"Config revision {self.revision_id} activated"


class ConfigRevisionApplied(Event):
    """Published by the engine after :meth:`Engine.apply_config` runs.

    ``status="ok"`` means the diff applied cleanly and the engine is
    now serving the new config.  ``status="error"`` means the diff
    failed mid-apply; the engine rolled back to the prior state.  The
    DB row stays ``is_active=TRUE`` either way — divergence is
    surfaced via this event for the dashboard / ops to act on.
    """

    type: str = "config_revision_applied"
    revision_id: str = ""
    status: str = "ok"
    applied_subsystems: list[str] = Field(default_factory=list)
    error: str = ""

    @property
    def summary(self) -> str:
        if self.status == "ok":
            return f"Config revision {self.revision_id} applied"
        return (
            f"Config revision {self.revision_id} failed: "
            f"{self.error or 'unknown error'}"
        )


# --- Task events ---


class TaskCreated(Event):
    type: str = "task_created"
    task_id: str = ""
    title: str = ""
    target_role: str = ""

    @property
    def summary(self) -> str:
        a = self.actor
        if self.title and self.target_role:
            return f"{a} created task '{self.title}' for {self.target_role}"
        if self.title:
            return f"{a} created task '{self.title}'"
        return f"{a} created a task"


class TaskAssigned(Event):
    type: str = "task_assigned"
    task_id: str = ""
    agent_id: str = ""
    role: str = ""

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} was assigned task {self.task_id}"
            if self.task_id
            else f"{self.actor} was assigned a task"
        )


class TaskStarted(Event):
    type: str = "task_started"
    task_id: str = ""
    agent_id: str = ""
    role: str = ""
    """The agent's role name — its seat, and the key every projection
    groups an agent by.

    Load-bearing, not decorative: the event store tags a row's
    ``agent_role`` from this field and the dashboard's live projection
    keys on it, so a task event published without one is invisible to
    both.  That is exactly what happened — an agent's current task never
    appeared on a dashboard, and a seat stayed "working" past the end of
    its turn, because the task lifecycle events carried the role only in
    ``source``, which neither consumer reads."""

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} started working on {self.task_id}"
            if self.task_id
            else f"{self.actor} started a task"
        )


class TaskCompleted(Event):
    type: str = "task_completed"
    task_id: str = ""
    agent_id: str = ""
    result: str = ""
    role: str = ""
    """The agent's role name — its seat, and the key every projection
    groups an agent by.

    Load-bearing, not decorative: the event store tags a row's
    ``agent_role`` from this field and the dashboard's live projection
    keys on it, so a task event published without one is invisible to
    both.  That is exactly what happened — an agent's current task never
    appeared on a dashboard, and a seat stayed "working" past the end of
    its turn, because the task lifecycle events carried the role only in
    ``source``, which neither consumer reads."""

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} completed task {self.task_id}"
            if self.task_id
            else f"{self.actor} completed a task"
        )


class TaskFailed(Event):
    type: str = "task_failed"
    task_id: str = ""
    agent_id: str = ""
    error: str = ""
    role: str = ""
    """The agent's role name — its seat, and the key every projection
    groups an agent by.

    Load-bearing, not decorative: the event store tags a row's
    ``agent_role`` from this field and the dashboard's live projection
    keys on it, so a task event published without one is invisible to
    both.  That is exactly what happened — an agent's current task never
    appeared on a dashboard, and a seat stayed "working" past the end of
    its turn, because the task lifecycle events carried the role only in
    ``source``, which neither consumer reads."""

    @property
    def summary(self) -> str:
        msg = (
            f"{self.actor} failed task {self.task_id}"
            if self.task_id
            else f"{self.actor} failed a task"
        )
        return f"{msg}: {self.error}" if self.error else msg


class TaskDelegated(Event):
    type: str = "task_delegated"
    parent_task_id: str = ""
    child_task_id: str = ""
    target_role: str = ""

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} delegated a subtask to {self.target_role}"
            if self.target_role
            else f"{self.actor} delegated a subtask"
        )


class ScheduledTaskFired(Event):
    """Observability event emitted when the Scheduler dispatches a run.

    The agent wake itself is a ``TaskAssigned`` published to the runner's
    inbox; this event records the dispatch for the dashboard / event
    store.  ``scope_type`` is ``"role"`` or ``"unit"``.
    """

    type: str = "scheduled_task_fired"
    scope_type: str = ""
    scope_id: str = ""
    schedule_name: str = ""
    target_handle: str = ""
    scheduled_at: str = ""

    @property
    def summary(self) -> str:
        who = self.target_handle or self.scope_id
        return f"Scheduled '{self.schedule_name}' fired for {who}"


# --- Communication events ---


class MessageSent(Event):
    type: str = "message_sent"
    channel: str = ""
    sender: str = ""
    content: str = ""

    @property
    def summary(self) -> str:
        who = self.sender or self.actor
        return (
            f"{who} sent a message to {self.channel}"
            if self.channel
            else f"{who} sent a message"
        )


# --- A2A (agent-to-agent) events ---


class A2AChannelOpened(Event):
    """Emitted when an A2A channel is created between two agents."""

    type: str = "a2a_channel_opened"
    channel_id: str = ""
    requester: str = ""
    target: str = ""
    participants: list[str] = Field(default_factory=list)

    @property
    def summary(self) -> str:
        return f"{self.requester} opened A2A channel with {self.target}"


class A2AMessageSent(Event):
    """Emitted when a message is sent on an A2A channel."""

    type: str = "a2a_message_sent"
    channel_id: str = ""
    sender: str = ""
    sender_role: str = ""
    recipient: str = ""
    message_id: str = ""
    content: str = ""

    @property
    def summary(self) -> str:
        who = self.sender or self.actor
        target = f" → {self.recipient}" if self.recipient else ""
        return f"{who} sent A2A message on {self.channel_id}{target}"


class A2AMessageDelivered(Event):
    """Emitted when A2A messages are read/delivered to the target agent."""

    type: str = "a2a_message_delivered"
    channel_id: str = ""
    recipient: str = ""
    sender: str = ""
    message_count: int = 0
    total_content_length: int = 0

    @property
    def summary(self) -> str:
        return (
            f"{self.recipient} received {self.message_count} A2A"
            f" message(s) from {self.sender} on {self.channel_id}"
        )


class A2AChannelClosed(Event):
    """Emitted when an A2A channel is closed."""

    type: str = "a2a_channel_closed"
    channel_id: str = ""
    closed_by: str = ""
    participants: list[str] = Field(default_factory=list)
    message_count: int = 0
    duration_ms: float = 0

    @property
    def summary(self) -> str:
        who = self.closed_by or "system"
        return (
            f"{who} closed A2A channel {self.channel_id}"
            f" ({self.message_count} messages)"
        )


# --- Knowledge events ---


class DocumentCreated(Event):
    type: str = "document_created"
    document_id: str = ""
    scope_kind: str = ""
    title: str = ""

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} created document '{self.title}'"
            if self.title
            else f"{self.actor} created a document"
        )


class DocumentUpdated(Event):
    type: str = "document_updated"
    document_id: str = ""
    scope_kind: str = ""

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} updated document {self.document_id}"
            if self.document_id
            else f"{self.actor} updated a document"
        )


# --- Notification events ---


class CoalescedMessage(BaseModel):
    """One constituent inbound message inside a coalesced notification.

    When several same-conversation notifications are merged into one
    trigger (see :mod:`crewlet.notifications.coalesce`), the merged
    :class:`ExternalNotification` keeps each original message here in
    chronological order — full fidelity for the learning workers
    (per-sender counterparty profiling, salient-text filters), while
    rendering decisions (supersede rules, digest layout) live in the
    merged ``body``.

    ``body`` is the constituent's *salient* body (the raw inbound
    message, no prompt scaffolding); ``metadata`` is the constituent's
    full notification metadata so per-message sender identity stays
    extractable (Slack user id, Jira account id, …).

    ``context_requires_recon`` is the constituent's OWN recon flag —
    the merged event's flat ``context_requires_recon`` is the
    conservative ``any()`` across constituents (it gates whole-turn
    prefetch logic), so without this per-message copy the original
    flags would be unrecoverable after the merge and any worker that
    later reasons per message would silently inherit whole-turn
    semantics.
    """

    sender: str = ""
    body: str = ""
    timestamp: datetime = Field(default_factory=lambda: datetime.now(UTC))
    source_event_type: str = ""
    metadata: dict[str, str] = Field(default_factory=dict)
    context_requires_recon: bool = False


class ExternalNotification(Event):
    """An inbound notification from an external system."""

    type: str = "external_notification"
    notification_source: str = ""
    source_event_type: str = ""
    recipient_email: str = ""
    agent_id: str = ""
    sender: str = ""
    subject: str = ""
    body: str = ""
    salient_body: str | None = None
    """The raw inbound message, stripped of notification-builder
    scaffolding.

    ``body`` is the *enriched* planner prompt -- the output of
    :func:`~crewlet.notifications.notification_prompts.build_notification_prompt`,
    which front-loads triage / how-to boilerplate (for Slack, ~1.5k
    chars of ``## Triage`` instructions) before the actual message.
    ``salient_body`` is the raw ``InboundNotification.body`` -- the
    message itself.  Learning workers (the Plan-phase relevance
    filters, the counterparty profiler, the PersistDecider) need the
    message, not the scaffolding: a filter keyed on a prefix of the
    enriched ``body`` never sees the message at all.  Set by the
    NotificationService alongside ``body``.

    ``None`` -- the default -- means "this producer emitted no distinct
    raw message" (an extension event, or a producer that doesn't set it); learning
    workers fall back to ``body``.  An *empty string* is distinct: it
    means the producer set a salient body and it was genuinely empty,
    so workers must NOT fall back to the scaffolding-laden ``body``."""
    metadata: dict[str, str] = Field(default_factory=dict)
    context_requires_recon: bool = False
    """True when ``body`` is a *pointer* (a webhook naming a
    thing-that-changed) rather than the context itself -- the agent
    must fetch the issue / read the thread / pull the diff before it
    has anything substantive.  Set by the NotificationService from
    :func:`~crewlet.notifications.notification_prompts.notification_requires_recon`.
    The Plan-phase relevance-filter prefetches read this (via
    :class:`~crewlet.learning.interaction.InboundInteraction`) and
    skip their aux-LLM call when it is ``True`` -- filtering memory /
    docs against a bare pointer is near-guaranteed low-value, and the
    planner is already told to re-query after recon."""
    messages: list[CoalescedMessage] = Field(default_factory=list)
    """Constituent messages when this event is a *coalesced* trigger
    (several same-conversation notifications merged into one digest by
    :func:`crewlet.notifications.coalesce.coalesce_notifications`).

    Empty -- the overwhelmingly common case -- means a plain
    single-webhook notification: the flat ``sender`` / ``salient_body``
    / ``metadata`` fields are the message.  Non-empty means ``body`` is
    a digest and the flat fields mirror the *latest* constituent;
    learning workers iterate this list (via
    ``InboundInteraction.list_from_trigger_event``) so every sender in
    the merged conversation is observed, not just the last one."""

    @property
    def summary(self) -> str:
        # The integration (slack / jira / github / …) is surfaced by the
        # dashboard as a branded badge built from ``notification_source``,
        # so the summary leads with the sender + subject rather than
        # repeating the source name.
        if self.messages:
            head = f"{len(self.messages)} messages"
            if self.sender:
                head += f" from {self.sender}"
        elif self.sender:
            head = f"Message from {self.sender}"
        else:
            head = "Notification"
        return f"{head}: {self.subject}" if self.subject else head

    @property
    def actor(self) -> str:
        # The person who sent it, for the same reason ``summary`` leads
        # with them: this event carries no ``role``, so the base
        # property falls through to ``source`` and reports the actor of
        # an inbound Slack message as ``notification_service.slack`` --
        # a machine name, sitting in a column of human ones, directly
        # beside the branded badge already built from the same string.
        # Falls back to the base chain when the integration gave us no
        # sender, so nothing loses a value it had before.
        return self.sender or super().actor


class NotificationsCoalesced(Event):
    """Emitted when several same-conversation inbox events are merged
    into one digest trigger before an agent turn.

    Observability counterpart of the merged :class:`ExternalNotification`
    itself: operators watch this stream to see when and how hard inbox
    batching kicks in (a busy agent draining a thread's backlog as one
    turn, or a linger window absorbing a webhook burst).  ``count`` is
    the number of constituent notifications; ``first_at`` / ``last_at``
    bound the span they arrived in (ISO 8601).
    """

    type: str = "notifications_coalesced"
    agent_handle: str = ""
    conversation_key: str = ""
    notification_source: str = ""
    count: int = 0
    first_at: str = ""
    last_at: str = ""

    @property
    def summary(self) -> str:
        return (
            f"{self.count} notifications coalesced for {self.agent_handle}"
            f" ({self.conversation_key})"
        )


class NotificationSkipped(Event):
    """Emitted when a notification is dropped with a reason."""

    type: str = "notification_skipped"
    handle: str = ""
    reason: str = ""
    notification_source: str = ""

    @property
    def summary(self) -> str:
        if self.handle:
            return f"Skipped for {self.handle}: {self.reason}"
        return f"Skipped: {self.reason}"


# --- Budget events ---


class BudgetExhausted(Event):
    """Emitted when an agent or the org exceeds its token budget."""

    type: str = "budget_exhausted"
    agent_id: str = ""
    role: str = ""
    budget_type: str = ""  # "agent" or "org"
    used_tokens: int = 0
    max_tokens: int = 0

    @property
    def summary(self) -> str:
        return f"{self.actor} exhausted {self.budget_type} token budget"


class BudgetReported(Event):
    """A snapshot of every live token meter, for the dashboard.

    Published on a fixed tick by the engine whenever a counter moved.
    It exists because the counters live nowhere but in the engine's own
    :class:`~crewlet.concurrency.BudgetManager`: they are not derivable
    from any other event, and the two figures a dashboard already has --
    the 7-day per-agent total and the 24-hour spend rollup -- cover
    different spans and cannot substitute.

    Deliberately NOT persisted (it is absent from the event store's
    category map, like ``agent_turn_progress``).  It is a *live meter*,
    so replaying one from history would show a dead engine's counters as
    the current ones.  Being an ordinary event is nonetheless what makes
    it work on both deployment shapes: the embedded API sees it through
    the publish listener and the standalone API through its broadcast
    subscription, with no second transport.
    """

    type: str = "budget_reported"
    meter_id: str = ""
    """Identity of the reporting meter's run.

    ``used_tokens`` is comparable only within one ``meter_id``.  A new
    id means the engine restarted and every prior figure is dead --
    consumers must REPLACE what they hold, never merge or take a
    maximum, or a restart pins a phantom high-water mark forever.
    """
    seq: int = 0
    """Monotonic within ``meter_id``; the reorder guard.

    Broker ordering holds only within a topic, and the standalone API
    reads a broadcast subscription across all of them, so an older
    report can arrive after a newer one and walk the meter backwards.
    """
    org_used_tokens: int = 0
    org_max_tokens: int = 0
    org_refused_at: str = ""
    agents: list[dict[str, Any]] = Field(default_factory=list)
    """One entry per METERED seat: ``agent_id``, ``role``,
    ``used_tokens``, ``max_tokens``, ``refused_at``.

    Only seats with a per-agent budget appear: the engine seeds one
    solely for a non-zero ``Role.token_budget``, so absence means "no
    cap and no meter" -- a different fact from a cap of zero, and one a
    consumer must not draw as an empty bar.

    ``role`` rides along because the engine already knows it and every
    consumer is keyed by role; re-deriving it from ``agent_id`` would be
    a second identity path, and the two diverge after a live handle
    edit.

    ``refused_at`` is when the cap last turned a charge away.  That, not
    ``used_tokens >= max_tokens``, is what "exhausted" means: a refused
    charge increments nothing, so the counter stops short of the cap by
    the size of the round that would not fit.
    """

    @property
    def summary(self) -> str:
        return f"Token meters reported for {len(self.agents)} agents"


class LLMUnavailable(Event):
    """Emitted when the LLM fallback chain is exhausted.

    Distinct from ``ProviderFallback`` (per-attempt failure that's
    still in-progress).  ``LLMUnavailable`` means no provider in the
    role's chain succeeded — the agent is effectively AFK and the
    turn terminates as ``failed``.
    """

    type: str = "llm_unavailable"
    agent_id: str = ""
    role: str = ""
    provider_chain: list[str] = Field(default_factory=list)
    attempt_count: int = 0
    last_error_kind: str = ""
    last_error: str = ""
    turn_id: str = ""

    @property
    def summary(self) -> str:
        return (
            f"LLM unavailable for {self.actor} "
            f"({len(self.provider_chain)} providers tried)"
        )


# --- System events ---


class AgentTurnCompleted(Event):
    """Emitted when an agent completes a full reasoning/tool-call cycle.

    The ``plan_model`` / ``execute_model`` / ``review_model`` /
    ``subagent_count`` / ``subagent_tokens`` / ``iterations`` /
    ``decision`` / ``turn_id`` fields summarise the three-phase loop.
    """

    type: str = "agent_turn_completed"
    agent_id: str = ""
    role: str = ""
    """The agent's role name — the seat this turn's tokens belong to.

    Same load-bearing field as on the task events: the event store tags
    ``agent_role`` from it and the live projection keys on it. Without
    one, a turn's token totals were attributed to nobody — every agent
    card read zero tokens no matter how much the seat had spent, live and
    after a reload alike."""
    model: str = ""
    # Compact descriptor of the event that triggered this turn (task
    # assignment, notification, A2A request, schedule tick). Built by
    # ``describe_trigger`` so the dashboard can show the turn's source.
    trigger: dict[str, Any] = Field(default_factory=dict)
    prompt: str = ""
    prompt_messages: list[dict[str, str]] = Field(default_factory=list)
    response: str = ""
    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    tool_executions: list[dict] = Field(default_factory=list)
    a2a_context: dict[str, Any] | None = None

    # Turn-engine summary fields.
    turn_id: str = ""
    plan_model: str = ""
    execute_model: str = ""
    review_model: str = ""
    subagent_count: int = 0
    subagent_tokens: int = 0
    iterations: int = 0
    decision: str = ""
    failed: bool = False
    """True when the turn ended on a failure path rather than finishing.

    ``decision`` is already ``"failed"`` in that case, but the *reason*
    lived only on the separate ``LLMUnavailable`` / ``turn.guard_breach``
    events, which the agent's LLM-history view does not read — so the
    turn that a dashboard shows had no way to say why it stopped."""
    error: str = ""
    """The failure's message (truncated).  Empty unless ``failed``."""
    error_kind: str = ""
    """Machine-readable failure class — the classified provider error
    (``rate_limit`` / ``auth`` / ...) or the guard-breach kind."""

    @property
    def summary(self) -> str:
        a2a_tag = ""
        if self.a2a_context:
            ch = self.a2a_context.get("channel_id", "")
            a2a_tag = f" [A2A:{ch}]" if ch else " [A2A]"
        if self.failed:
            reason = self.error_kind or "error"
            return f"{self.actor} turn failed ({reason}){a2a_tag}"
        if self.model:
            return (
                f"{self.actor} completed LLM turn"
                f" ({self.model}, {self.total_tokens} tokens){a2a_tag}"
            )
        return f"{self.actor} completed a turn{a2a_tag}"


class TurnCompleted(Event):
    """Emitted by the Turn Engine at the end of every agent turn.

    Carries the Plan/Execute/Review-shaped payload the agent-learning
    subsystem consumes to build an :class:`~crewlet.learning.models.Episode`
    and to update counterparty profiles.  Distinct from
    :class:`AgentTurnCompleted`, which carries the single-phase
    summary surface for the dashboard.
    """

    type: str = "turn_completed"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    task_id: str = ""
    started_at: datetime = Field(default_factory=lambda: datetime.now(UTC))
    ended_at: datetime = Field(default_factory=lambda: datetime.now(UTC))
    duration_ms: int = 0
    task_summary: str = ""
    plan_summary: str = ""
    tool_sequence: list[str] = Field(default_factory=list)
    """Tools called during the Execute phase of the final iteration.
    Execute-scoped by design -- the ReflectEngine's ``no_action`` gate
    and the single-turn skill-synthesis trigger both reason about
    Execute work specifically."""

    plan_tool_sequence: list[str] = Field(default_factory=list)
    """Tools called during the Plan phase(s) of this turn, accumulated
    across ``self_iterate`` loops.  Plan-only builtins
    (``reflect_and_persist``, ``query_episodes``, ``refresh_memory``,
    ``refine_skill``) never appear in ``tool_sequence``; the
    ReflectEngine reads this to skip the post-turn ``PersistDecider``
    when the LLM already self-persisted in-flight."""

    skills_used: list[str] = Field(default_factory=list)
    review_outcome: str = ""
    iterations: int = 0

    plan_decision: str = ""
    """Top-level Plan decision: ``plan`` | ``direct`` | ``skip``.
    Empty when the turn never produced a Plan artifact
    (e.g. infrastructure-level failure before Plan ran).  Surfaced on
    this event so the ReflectEngine can short-circuit learning on
    ``skip`` turns -- the planner explicitly opted out, so there's
    nothing the agent itself engaged with to learn from, and persisting
    facts read off the trigger would teach the agent things directed at
    *someone else*."""

    # Universal learning path — the platform-agnostic
    # boundary type carries each trigger message's sender and body
    # when identifiable.  Usually one entry; a coalesced trigger
    # (several same-conversation notifications merged into one digest)
    # carries one per constituent message, possibly from several
    # senders.  Empty for internal triggers (TaskAssigned, scheduled
    # ticks, system events).  See
    # ``crewlet.learning.interaction.InboundInteraction``.
    interactions: list[InboundInteraction] = Field(default_factory=list)

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} completed turn ({self.review_outcome})"
            if self.review_outcome
            else f"{self.actor} completed turn"
        )


# --- Learning lifecycle events ------------------------------------------
#
# Each fires from a learning worker after one outcome.  Trace context is
# inherited from the active OTel span (``learning.reflect`` for the four
# reflection workers, ``agent.turn`` for episode writes,
# ``learning.skill_scheduler.tick`` for scheduled work) so the dashboard's
# trace-grouped event view shows them under the same card as the parent
# turn or scheduled tick.


class EpisodeWritten(Event):
    """Emitted after one episode row lands in the episode store.

    Fires synchronously at turn end from inside the parent ``agent.turn``
    span so the dashboard shows it grouped with the turn.  Skipped when
    no ``episode_store`` is wired (in-memory / test mode).
    """

    type: str = "episode_written"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    review_outcome: str = ""
    duration_ms: int = 0
    tool_count: int = 0

    @property
    def summary(self) -> str:
        return f"{self.actor} wrote episode ({self.review_outcome})"


class PersistDeciderCompleted(Event):
    """Emitted by ``PersistDecider`` after one reflection pass.

    ``persisted=False`` (NOOP) is the common case — the decider is
    deliberately conservative.  When it does write, ``doc_id`` and
    ``scope`` carry the result so operators can navigate from a turn
    to the persisted reflection in the knowledge store.

    ``classification`` distinguishes the four decider tiers
    (``LONG`` / ``SHORT`` / ``DOC`` / ``NOOP``) so dashboards can
    plot the per-agent distribution -- the headline learning-write-rate
    signal.  ``ttl_until`` is populated only for ``SHORT`` rows.
    """

    type: str = "persist_decider_completed"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    persisted: bool = False
    doc_id: str = ""
    scope: str = ""  # "agent" | "unit" — empty on NOOP
    classification: str = ""  # "LONG" | "SHORT" | "DOC" | "NOOP"
    ttl_until: str = ""  # ISO timestamp; populated only for SHORT
    review_outcome: str = ""

    @property
    def summary(self) -> str:
        if self.persisted:
            return f"{self.actor} persisted reflection ({self.classification})"
        if self.classification == "DOC":
            return f"{self.actor} observed standing rule (not memorised)"
        return f"{self.actor} reviewed turn — nothing to persist"


class SkillUsed(Event):
    """Emitted when an agent loads a skill via ``use_skill``.

    Fires for both synthesized skills (per-agent rows in
    ``synthesized_skills``) and registry-loaded skills (operator-curated
    via ``skill_sources`` config).  The Berlot-Attwell measurement
    requires this event: without it, we cannot tell whether the skills
    SkillSynthesizer drafts are ever loaded -- the apparent benefit of
    skill induction may come from extra LLM sampling rather than reuse.

    For synthesized skills, the same load path bumps ``use_count`` and
    ``last_used_at`` on the row directly; this event is the dashboard /
    audit-trail counterpart.  For registry skills the row counter
    doesn't exist (the registry is in-memory), so the event is the only
    record of load.
    """

    type: str = "skill_used"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    skill_name: str = ""
    skill_id: str = ""  # empty for registry-loaded
    source_kind: str = ""  # "synthesized" | "registry"
    file_loaded: str = ""  # bundled file path; empty when loading the body

    @property
    def summary(self) -> str:
        return f"{self.actor} used skill '{self.skill_name}' ({self.source_kind})"


class PlanPrefetchSummary(Event):
    """Emitted once per turn after the Plan-phase pre-fetches resolve.

    Records hit / miss + rendered-byte size for each of the six
    parallel prefetches (counterparty profile, synthesized skills,
    episode recall, onboarding hint, personal memory, relevant
    knowledge).  Operators plot per-block hit rate over time to
    validate that the prefetch pipeline is producing useful signal --
    a block stuck at 0% hit rate for an agent is a configuration /
    data problem, not a turn problem.
    """

    type: str = "plan_prefetch_summary"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    counterparty_hit: bool = False
    counterparty_bytes: int = 0
    synthesized_skills_hit: bool = False
    synthesized_skills_bytes: int = 0
    episode_recall_hit: bool = False
    episode_recall_bytes: int = 0
    onboarding_hint_hit: bool = False
    onboarding_hint_bytes: int = 0
    personal_memory_hit: bool = False
    personal_memory_bytes: int = 0
    relevant_knowledge_hit: bool = False
    relevant_knowledge_bytes: int = 0
    relevant_knowledge_selection_count: int = 0
    """Number of doc bullets the relevant-knowledge aux filter
    actually picked.

    Distinguishes the two ``relevant_knowledge_hit=True`` paths: a
    non-zero count means the filter rendered real picks; zero with
    ``hit=True`` means the ``EMPTY_FILTER_HINT`` was rendered (filter
    ran, found nothing relevant, but vector candidates existed).
    Operators investigating low effectiveness pivot on this field to
    tell "no signal" from "hint nudge only"."""
    trigger_requires_recon: bool = False
    """Whether the trigger was a bare pointer (a recon-required
    webhook -- Jira/Confluence page event, GitHub review request,
    Slack thread reply).

    When ``True``, the personal-memory / relevant-knowledge /
    episode-recall prefetches skipped their aux-LLM call entirely via
    the thin-trigger gate -- so their ``*_hit`` / ``*_bytes`` reflect
    the gate, not a filter that ran and found nothing.  This is the
    field that disambiguates "the block is empty because it was
    *gated*" from "the filter ran and selected nothing".  Decided by
    pure logic (notification metadata: ``issue_key`` / ``thread_ts`` /
    ``event_type``) -- no LLM call."""

    @property
    def summary(self) -> str:
        hits = sum(
            (
                self.counterparty_hit,
                self.synthesized_skills_hit,
                self.episode_recall_hit,
                self.onboarding_hint_hit,
                self.personal_memory_hit,
                self.relevant_knowledge_hit,
            )
        )
        gated = " (thin trigger — filters gated)" if self.trigger_requires_recon else ""
        return f"{self.actor} plan prefetch: {hits}/6 hits{gated}"


class RelevantKnowledgeRefetched(Event):
    """Emitted when the post-Plan relevant-knowledge re-fetch fires.

    On a thin trigger (a recon-required webhook) the Plan-phase
    ``## Relevant knowledge`` prefetch is gated off -- embedding-
    searching a bare pointer is noise.  Once Plan has done its recon,
    :func:`crewlet.agent.plan.fetch_post_plan_relevant_knowledge`
    re-runs the fetch keyed on the plan summary and hands the result
    to the Execute phase.  This event records that it ran and what it
    produced, so the gated Plan prefetch and the recovered Execute
    block can be correlated in the dashboard.

    Emitted only when the re-fetch actually takes its active path
    (thin trigger + ``plan`` / ``direct`` decision + non-empty plan
    summary); the overwhelmingly common non-thin-trigger turn emits
    nothing.  Decided by pure logic plus one aux-LLM filter call --
    the filter call itself shows up separately as an aux invocation.
    """

    type: str = "relevant_knowledge_refetched"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    iteration: int = 0
    """Turn iteration the re-fetch ran in.  Plan re-runs on
    ``self_iterate``, and the re-fetch runs again each time keyed on
    the (changed) plan summary -- so multiple events per turn are
    expected and legitimate."""
    plan_decision: str = ""
    """The plan decision that routed into Execute -- ``plan`` or
    ``direct``.  ``skip`` never reaches Execute, so the re-fetch
    short-circuits and emits nothing for it."""
    block_bytes: int = 0
    """Rendered size of the block injected into the Execute prompt.
    Non-zero with ``selection_count == 0`` means the aux filter ran
    and rendered ``EMPTY_FILTER_HINT`` (candidates existed, none
    matched); zero means the fetch produced nothing at all."""
    selection_count: int = 0
    """Number of doc bullets the aux filter picked for the Execute
    block.  Mirrors ``PlanPrefetchSummary.relevant_knowledge_selection_count``."""

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} re-fetched relevant knowledge post-Plan: "
            f"{self.selection_count} doc(s), {self.block_bytes}B"
        )


class CounterpartyProfileUpdated(Event):
    """Emitted by ``CounterpartyProfiler`` after one observation pass.

    Fires whenever the turn had an identifiable counterparty —
    even on NOOP traits patches we still bump ``interaction_count`` so
    the cadence is visible.
    """

    type: str = "counterparty_profile_updated"
    observer_handle: str = ""
    role: str = ""
    turn_id: str = ""
    subject_handle: str = ""
    subject_external_id: str = ""
    subject_platform: str = ""
    subject_name: str = ""
    traits_patched: int = 0

    @property
    def summary(self) -> str:
        subject = self.subject_name or self.subject_handle or "(unknown)"
        if self.traits_patched:
            return f"{self.actor} updated {self.traits_patched} trait(s) on {subject}"
        return f"{self.actor} observed {subject}"


class SkillSynthesized(Event):
    """Emitted when ``SkillSynthesizer`` writes a new agent-scope skill.

    Fires from both the single-turn path (in ``ReflectEngine``) and the
    scheduled clustered path (in ``SkillClusteringScheduler``).
    ``trigger`` distinguishes the two on the dashboard so operators can
    tell inline learning from periodic consolidation.
    """

    type: str = "skill_synthesized"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""  # empty for clustered (no single triggering turn)
    skill_name: str = ""
    skill_id: str = ""
    trigger: str = ""  # "single_turn" | "clustered"
    cluster_size: int = 0  # 1 for single-turn, N for clustered
    tool_count: int = 0

    @property
    def summary(self) -> str:
        return f"{self.actor} synthesised skill '{self.skill_name}' ({self.trigger})"


class SkillRefined(Event):
    """Emitted by ``SkillRefiner`` when a synthesized skill picks up a
    new observation or counter-example.

    Most turns NOOP — only fires when the refiner actually appended a
    note and bumped ``version``.  ``refinement_kind`` captures the
    intent (``observed_in_practice`` / ``counter_example`` / etc.) so
    the dashboard can show success vs failure annotations.
    """

    type: str = "skill_refined"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    skill_name: str = ""
    skill_id: str = ""
    skill_version: int = 0
    refinement_kind: str = ""

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} refined '{self.skill_name}' "
            f"v{self.skill_version} ({self.refinement_kind})"
        )


class SkillPromoted(Event):
    """Emitted by ``PromotionSynthesizer`` when sibling agent-scope
    skills are distilled into a knowledge-base draft page.

    The promotion target is a draft page in the team knowledge base
    (the active backend — a Confluence space or a Plane project) under
    the unit's ``Auto-Drafted Skills`` parent.  ``page_id`` /
    ``page_title`` / ``container_key`` let the dashboard link straight
    to the new draft so a unit lead can review it.  ``skill_name`` is
    the LLM-picked kebab name embedded in the title -- still useful
    for the dashboard's text view.
    """

    type: str = "skill_promoted"
    role: str = ""  # representative role for the unit
    unit_id: str = ""
    skill_name: str = ""
    page_id: str = ""
    page_title: str = ""
    container_key: str = ""
    """The unit's configured ``integrations.confluence.space`` /
    ``integrations.plane.project`` the draft landed in."""
    sibling_count: int = 0  # contributing agent-scope skills
    distinct_agents: int = 0  # distinct owners across the cluster

    @property
    def summary(self) -> str:
        return (
            f"unit '{self.unit_id}' drafted '{self.skill_name}' in the "
            f"team knowledge base from {self.distinct_agents} agent(s)"
        )


class ProviderFallback(Event):
    """Emitted by the ``FallbackLLMProvider`` wrapper each time
    the chain falls through from one provider to the next.

    ``from_provider_key`` / ``to_provider_key`` identify the move;
    ``error_kind`` is the classified error from ``classify`` (e.g.
    ``"rate_limit"``, ``"auth"``, ``"server"``, ``"timeout"``, or
    ``"pool_exhausted"`` for the special ``AllCredentialsExhausted``
    case from the credential pool). Dashboards count these to spot unstable
    providers."""

    type: str = "provider_fallback"
    agent_handle: str = ""
    phase: str = ""
    from_provider_key: str = ""
    to_provider_key: str = ""
    error_kind: str = ""

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} fallback {self.from_provider_key} → "
            f"{self.to_provider_key} ({self.error_kind})"
        )


class SkillStaled(Event):
    """Emitted by ``SkillCuratorWorker`` when an active synthesized
    skill transitions to ``stale`` (idle past ``stale_after_days``).
    Operators watching this stream see which skills are aging out and
    can ``pinned=true`` ones they want preserved."""

    type: str = "skill_staled"
    agent_handle: str = ""
    skill_id: str = ""
    skill_name: str = ""
    last_used_at: str = ""  # ISO 8601, empty when None
    transitioned_at: str = ""


class SkillArchived(Event):
    """Emitted by ``SkillCuratorWorker`` when a stale synthesized
    skill transitions to ``archived`` (idle past ``archive_after_days``).
    Archived skills are still in the table -- ``_use_skill`` refuses
    to load them and the prefetch filter hides them. Operators can
    restore a row by setting ``state='active'`` manually."""

    type: str = "skill_archived"
    agent_handle: str = ""
    skill_id: str = ""
    skill_name: str = ""
    last_used_at: str = ""
    transitioned_at: str = ""


class SkillRevived(Event):
    """Emitted by ``SkillCuratorWorker`` when a previously-stale
    skill is used again and transitions back to ``active``. Distinct
    from ``SkillStaled`` / ``SkillArchived`` so operators can see
    the bidirectional state churn."""

    type: str = "skill_revived"
    agent_handle: str = ""
    skill_id: str = ""
    skill_name: str = ""
    prior_state: str = ""  # "stale" or "archived" if operator restored
    transitioned_at: str = ""


class SkillTelemetryWriteFailed(Event):
    """Emitted by ``synthesized_skill_store.mark_used`` when
    bumping ``use_count`` / ``last_used_at`` failed
    twice. A persistent failure means ``last_used_at`` is stale and
    the curator may wrongly archive a hot skill. Operators watching
    this event can investigate before that happens."""

    type: str = "skill_telemetry_write_failed"
    skill_id: str = ""
    skill_name: str = ""
    agent_handle: str = ""
    kind: str = ""  # "mark_used", "transition_state", etc.
    error: str = ""


class CompactionRequested(Event):
    """Published by :class:`EpisodeStore` when an agent's raw episode
    count crosses ``max_raw_episodes_per_agent``.

    Consumed by :class:`EpisodeLifecycleWorker`, which runs the full
    episode-lifecycle pass for that agent: drop non-terminal mid-states,
    drop skill-consolidated episodes past grace, cluster + LLM-compact
    the rest, optionally evict ancient compacted entries.

    Trigger is write-side and demand-proportional -- busy agents fire
    it more often (when they accumulate episodes faster), idle agents
    fire it not at all.

    The worker dedups concurrent requests for the same agent via a
    per-agent semaphore, so a burst of writes doesn't fan out
    overlapping compaction passes.
    """

    type: str = "compaction_requested"
    agent_handle: str = ""
    raw_count: int = 0
    """Approximate raw-episode count at trigger time (for telemetry)."""
    threshold: int = 0
    """The configured ``max_raw_episodes_per_agent`` that was crossed."""

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} requested episode compaction "
            f"(raw_count={self.raw_count} > threshold={self.threshold})"
        )


class CompactionCompleted(Event):
    """Published by :class:`EpisodeLifecycleWorker` after one lifecycle
    pass finishes (or short-circuits).  Carries per-action counts so
    operators can audit what happened without reading structlog.
    """

    type: str = "compaction_completed"
    agent_handle: str = ""
    non_terminal_dropped: int = 0
    consolidated_dropped: int = 0
    clusters_compacted: int = 0
    raw_replaced_by_compaction: int = 0
    compacted_evicted: int = 0
    skipped_reason: str = ""
    """Empty when the pass ran; ``"no_database"`` / ``"no_aux_provider"``
    / ``"already_running"`` when it short-circuited."""

    @property
    def summary(self) -> str:
        if self.skipped_reason:
            return f"{self.actor} compaction skipped ({self.skipped_reason})"
        return (
            f"{self.actor} compacted episodes: "
            f"-{self.non_terminal_dropped} non-terminal, "
            f"-{self.consolidated_dropped} consolidated, "
            f"{self.clusters_compacted} clusters "
            f"({self.raw_replaced_by_compaction} raw → compacted), "
            f"-{self.compacted_evicted} ancient compacted"
        )


class ReflectionCompleted(Event):
    """Emitted by ``ReflectEngine`` when post-turn reflection finishes.

    Fired at the end of every reflection pass that actually dispatched
    workers (any pass that got past the gates: workers wired, role
    resolvable, role learning enabled, not duplicate, budget available).
    Carries no per-worker outcome — those have their own lifecycle
    events (``persist_decider_completed``, ``counterparty_profile_updated``,
    ``skill_synthesized``, ``skill_refined``).

    Purpose: state-derivation sentinel.  ``agent_phase_completed`` events
    fired by aux workers (PersistDecider, CounterpartyProfiler, etc.)
    keep the agent in ``working`` state so the dashboard can show the
    agent as live during learning.  This event flips it back to
    ``idle`` when the whole reflection pass is done.
    """

    type: str = "reflection_completed"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    workers_run: int = 0
    review_outcome: str = ""

    @property
    def summary(self) -> str:
        return f"{self.actor} finished reflecting ({self.workers_run} worker(s))"


class AgentPhaseStarted(Event):
    """Emitted at the top of each Plan / Execute / Review phase.

    Lightweight live-state signal for dashboards: the matching
    :class:`AgentPhaseCompleted` carries the full picture (prompts,
    response, tool calls), but until it fires the dashboard has no
    way to know *which* phase a ``state=working`` agent is currently
    running.  Pair them: ``started`` flips ``current_phase`` in the
    agent-state projection, ``completed`` leaves it (the next phase's
    ``started`` overwrites, ``TaskCompleted``/``TaskFailed`` clears).

    Not emitted for ``subagent``, ``judge``, or ``auxiliary`` phases
    -- those are nested under a host phase that's already reflected
    in ``current_phase``.
    """

    type: str = "agent_phase_started"
    agent_id: str = ""
    role: str = ""
    turn_id: str = ""
    iteration: int = 0
    # One of: "plan" | "execute" | "review"
    phase: str = ""
    # Compact descriptor of the event that triggered this turn (see
    # ``describe_trigger``). Carried on every phase event so the
    # dashboard can show the turn's source even on a live row that has
    # no completed phase yet.
    trigger: dict[str, Any] = Field(default_factory=dict)

    @property
    def summary(self) -> str:
        return f"{self.actor} started {self.phase} (iter {self.iteration})"


def format_reasoning_and_content(reasoning: str, content: str) -> str:
    """Render one assistant turn's reasoning + visible content as ONE string.

    This is the wire format of the ``response`` field on
    :class:`AgentPhaseCompleted` and :class:`AgentTurnProgress`: a
    model's extended-thinking output is wrapped in ``<think>...</think>``
    and placed immediately before the visible content it produced.  The
    dashboard's ``responseBody`` (``static/dashboard/js/llm.js``) parses
    those tags and renders each block as a collapsible "Reasoning"
    section inline with the tool calls.

    It lives here, beside the events whose field it defines, because
    three call sites build that field and they must not drift: the
    per-round live progress event and the per-phase completed event
    (both in ``crewlet.agent.llm_loop``) and the auxiliary-LLM
    telemetry wrapper (``crewlet.learning._aux_telemetry``).  When the
    live event omitted reasoning and the completed one included it, a
    reasoning model's thinking simply did not exist on the dashboard
    until its phase ended -- the live view streamed tool calls against
    an empty response.

    Empty reasoning yields the visible content unchanged, so a
    non-thinking model's response keeps its plain shape.  Reasoning
    with no visible content (a thinking model that hit its output cap)
    still renders, because the thinking is then the only signal there
    is.
    """
    reasoning = (reasoning or "").strip()
    content = (content or "").strip()
    if not reasoning:
        return content
    if not content:
        return f"<think>{reasoning}</think>"
    return f"<think>{reasoning}</think>\n\n{content}"


class AgentPhaseCompleted(Event):
    """Emitted once at the end of each Plan / Execute / Review / Sub-agent
    phase with full context so dashboards can render a per-phase timeline.

    ``AgentTurnCompleted`` only fires once at turn end with aggregate
    totals; the dashboard had no per-phase record of *what* was sent
    to the LLM (``system_prompt``), *what came back* (``response``),
    *which tools ran* (``tool_executions``), or *which structured
    decision* the phase produced (``decision``).  This event fills
    that gap.

    One event per phase invocation.  For ``self_iterate`` turns the
    Plan / Execute / Review trio fires multiple times (one each per
    iteration); ``iteration`` tags which one.  Sub-agent spawns emit
    ``phase="subagent"`` events as siblings under the parent Execute
    phase.
    """

    type: str = "agent_phase_completed"
    agent_id: str = ""
    role: str = ""
    turn_id: str = ""
    iteration: int = 0
    # One of: "plan" | "execute" | "review" | "subagent" | "auxiliary" | "judge"
    phase: str = ""
    host_phase: str = ""
    """For ``phase="judge"`` events: the host phase that triggered the
    judge (``"plan"`` or ``"execute"``).  Empty for all other phases.
    Dashboards group/order judge events under the host phase rather
    than rendering them as standalone siblings."""
    host_iteration: int = 0
    """For ``phase="judge"`` events: the host phase's turn iteration
    when the judge fired.  Pair with ``host_phase`` for grouping."""
    worker: str = ""
    """Name of the auxiliary learning worker that made the call.
    Set only when ``phase=="auxiliary"`` (e.g. ``persist_decider``,
    ``counterparty_profiler``, ``skill_synthesizer``,
    ``skill_refiner``, ``promotion_synthesizer``,
    ``summarize_episodes``).  Empty for main-phase invocations."""
    model: str = ""
    provider_key: str = ""
    # Compact descriptor of the event that triggered this turn (see
    # ``describe_trigger``) — the dashboard renders it as the LLM
    # invocation's source.
    trigger: dict[str, Any] = Field(default_factory=dict)
    system_prompt: str = ""  # truncated
    user_prompt: str = ""  # truncated (first user message text)
    response: str = ""  # final assistant text / artifact
    tool_executions: list[dict] = Field(default_factory=list)
    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    rounds_used: int = 0
    exhausted_rounds: bool = False
    decision: str = ""  # plan or review structured decision; "" otherwise
    rescue_fired: bool = False
    """True when the phase's submit-tool was not called on the first
    run of the LLM loop, prompting a constrained rescue call. Plan and
    Review can rescue; Execute and sub-agent phases never set this."""
    # Free-text notes: review's notes string, rejected sub-agent
    # tools, missing tool names from execute, etc.  Kept short.
    notes: str = ""
    # Tool names whose JSON schemas were passed in the LLM call's
    # ``tools=[...]`` parameter.  This is what the model can
    # actually invoke this round.  For Plan, this starts as the
    # meta-tools (``submit_plan``, ``activate_tool``,
    # ``load_tool_skill``) -- full schemas for the domain tools are
    # *not* sent to keep the Plan call cheap; any catalogue tool the
    # planner activated via ``activate_tool`` also appears here from
    # that round onward.  For Execute / Review / Sub-agent, this is
    # the phase's ``tools=[...]`` list (domain + always-on).
    tools_available: list[str] = Field(default_factory=list)
    # Tool names listed as ``name: description`` prose in the Plan
    # prompt (no schema).  Planner picks from here; to invoke a
    # specific tool it calls ``activate_tool`` to promote it into
    # ``tools=[...]``.  Empty for every phase other than Plan.
    tool_catalogue: list[str] = Field(default_factory=list)
    # Sandbox Execute backend.  Populated
    # only on a ``phase="execute"`` event whose backend was the
    # sandbox; ``backend`` stays "native" for every other phase so the
    # dashboard renders the sandbox badge precisely where it applies.
    backend: str = "native"  # "native" | "sandbox"
    coding_agent: str = ""  # "claude-code" | "opencode" when backend=sandbox
    sandbox_id: str = ""
    cost_usd: float = 0.0
    delivered_refs: list[str] = Field(default_factory=list)
    failed: bool = False
    """True when the phase died instead of finishing.

    A phase that raises -- the provider chain exhausted, a tool blew up,
    the wall-clock cap fired -- used to publish *nothing*: the only
    durable record was the ``agent_phase_started`` that opened it, so the
    dashboard was left showing an in-flight call with no response and no
    reason.  The phase runners now emit this event on the failure path
    too, carrying whatever the loop managed before it died plus the
    fields below.  ``response`` / ``tool_executions`` / token counts are
    therefore *partial* on a failed event, not absent."""
    error: str = ""
    """The failure's message (truncated).  Empty unless ``failed``."""
    error_kind: str = ""
    """Machine-readable failure class.  For an exhausted provider chain
    this is the classified LLM error (``rate_limit`` / ``auth`` /
    ``timeout`` / ...); otherwise the exception's type name."""

    @property
    def summary(self) -> str:
        parts = [f"{self.actor} {self.phase}"]
        if self.backend == "sandbox":
            parts.append(f"[sandbox:{self.coding_agent or '?'}]")
        if self.failed:
            parts.append(f"✗ failed ({self.error_kind or 'error'})")
        elif self.decision:
            parts.append(f"→ {self.decision}")
        if self.model:
            parts.append(f"({self.model}, {self.total_tokens} tokens)")
        return " ".join(parts)


class SandboxRunStarted(Event):
    """A detached sandbox coding job was kicked off.

    Emitted by the kick-off turn after it starts the background command
    and persists the ``pending_sandbox_run`` row.  The kick-off turn
    then ends; the agent stays busy (``AWAITING_SANDBOX``) until the
    matching :class:`SandboxRunCompleted` arrives.
    """

    type: str = "sandbox_run_started"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    sandbox_id: str = ""
    coding_agent: str = ""
    conversation_key: str = ""
    task_id: str = ""
    task: str = ""
    """Short human-readable summary of the work, for the dashboard's
    running-sandboxes panel; the full brief lives in the pending run."""

    @property
    def summary(self) -> str:
        return f"{self.actor} started a sandbox job ({self.coding_agent})"


class SandboxRunCompleted(Event):
    """A detached sandbox coding job finished.

    A pure control signal: it carries the original ``turn_id`` so the
    engine can load the ``pending_sandbox_run`` row, reconnect, and
    collect the actual result from the sandbox — the outcome (success,
    delivered refs, tokens) is read at collect time, never carried on
    the event. Published by the completion poll
    (:class:`~crewlet.sandbox.waiter.SandboxWaiter`); the engine flips
    the row ``running -> resumed`` atomically so the resume runs
    at-most-once even when the signal is delivered more than once
    (successive poll ticks can both fire before the first claim lands,
    and queue delivery is at-least-once).
    """

    type: str = "sandbox_run_completed"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    sandbox_id: str = ""
    coding_agent: str = ""

    @property
    def summary(self) -> str:
        return f"{self.actor}'s sandbox job completed"


class SandboxClarificationRequested(Event):
    """The in-sandbox coding agent asked a person a question.

    The sandbox never posts anything itself: it signals via the ``ask``
    tool, the runner surfaces the question + audience, and the engine
    posts it on the audited per-role surface (originating conversation /
    team channel / manager DM / A2A).  This event records that routing
    for telemetry; the agent goes **free** (idle) while it waits.
    """

    type: str = "sandbox_clarification_requested"
    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    turn_id: str = ""
    sandbox_id: str = ""
    question: str = ""
    audience: str = ""  # requester | team | manager | <handle>
    conversation_key: str = ""

    @property
    def summary(self) -> str:
        return f"{self.actor} asked {self.audience or 'someone'} a question"


class SubagentBatched(Event):
    """Emitted once per :func:`spawn_subagent_batch` call.

    Tracks how many parallel children a batched ``spawn_subagent``
    invocation produced, how many succeeded vs failed, and the total
    tokens consumed across the batch. Useful for dashboards to count
    fan-out work and identify pathological batches.
    """

    type: str = "subagent_batched"
    parent_handle: str = ""
    task_count: int = 0
    successes: int = 0
    failures: int = 0
    total_tokens: int = 0

    @property
    def summary(self) -> str:
        return (
            f"{self.actor} batched {self.task_count} sub-agents "
            f"({self.successes} ok, {self.failures} failed, "
            f"{self.total_tokens} tokens)"
        )


class AgentTurnProgress(Event):
    """Emitted after each tool-call round to provide incremental updates.

    Not persisted to the event store — the matching
    :class:`AgentPhaseCompleted` is the durable record.  Live consumers
    (the dashboard's ``/ws/stream`` agent page) correlate these
    per-round updates with the turn-grouped LLM history via ``turn_id``
    / ``phase`` / ``iteration``, which mirror the same fields on
    :class:`AgentPhaseStarted` and :class:`AgentPhaseCompleted`.
    """

    type: str = "agent_turn_progress"
    agent_id: str = ""
    role: str = ""
    turn_id: str = ""
    # Phase whose tool loop produced this round — "plan" | "execute" |
    # "review" | "judge" | "subagent" (or the provider key for callers
    # outside the turn engine).
    phase: str = ""
    iteration: int = 0
    model: str = ""
    # Compact descriptor of the event that triggered this turn (see
    # ``describe_trigger``). Repeated on each progress round so a live
    # row keeps its source across the round-by-round overwrites.
    trigger: dict[str, Any] = Field(default_factory=dict)
    prompt: str = ""
    prompt_messages: list[dict[str, str]] = Field(default_factory=list)
    response: str = ""
    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    # Zero-based index of the round this update reports, or ``-1`` for
    # the opening update a phase publishes before its first provider
    # call -- the one that carries ``prompt_messages`` so the live view
    # can show what the agent was asked while it is still answering.
    # Consumers read ``round_num + 1`` as "rounds so far", which is why
    # the sentinel is -1 rather than 0.
    round_num: int = 0
    tool_executions: list[dict] = Field(default_factory=list)
    a2a_context: dict[str, Any] | None = None

    @property
    def summary(self) -> str:
        a2a_tag = ""
        if self.a2a_context:
            ch = self.a2a_context.get("channel_id", "")
            a2a_tag = f" [A2A:{ch}]" if ch else " [A2A]"
        round_part = f"round {self.round_num}" if self.round_num else ""
        bits = ", ".join(x for x in (self.phase, round_part) if x)
        if bits:
            return f"{self.actor} working ({bits}){a2a_tag}"
        return f"{self.actor} working{a2a_tag}"


# --- Turn engine events -------------------------------------------------


class ExecuteMissingTool(Event):
    """Emitted when the Execute phase's LLM calls a tool name that is
    not in its surface (signals plan incompleteness).
    """

    type: str = "execute.missing_tool"
    agent_id: str = ""
    role: str = ""
    tool_name: str = ""
    plan_tools: list[str] = Field(default_factory=list)

    @property
    def summary(self) -> str:
        return f"{self.actor} asked for unknown tool '{self.tool_name}'"


class PhaseToolActivated(Event):
    """Emitted whenever a Plan or Execute phase successfully promotes
    a catalogue tool into its active surface via ``activate_tool``.

    Per-phase semantics:

    - ``phase="plan"`` — the planner pulled a tool in for in-Plan
      recon (lookups, reads). Expected on most non-trivial turns.
    - ``phase="execute"`` — the executor discovered a tool the plan
      did not list and activated it mid-run. Signals plan
      incompleteness; chronic occurrences indicate the planner needs
      more guidance.

    ``turn_id`` / ``iteration`` mirror the other per-phase events
    (``AgentPhaseStarted`` / ``AgentPhaseCompleted``) so dashboards
    can correlate an activation to a specific turn iteration.
    """

    type: str = "phase.tool_activated"
    agent_id: str = ""
    role: str = ""
    phase: str = ""
    tool_name: str = ""
    turn_id: str = ""
    iteration: int = 0

    @property
    def summary(self) -> str:
        return f"{self.actor} {self.phase} activated '{self.tool_name}'"


class ToolSkillGuardBlocked(Event):
    """Emitted when the required-skill guard rejects a tool call.

    Fired by :class:`crewlet.agent.skills.guard.SkillGuard` whenever a
    Plan / Execute / Sub-agent session calls a tool covered by a
    required tool skill (the default; ``required: false`` opts out)
    before loading that skill's body via ``load_tool_skill``.  The
    call never executes; the LLM receives an instructive error naming
    the key(s) to load and retries after loading.

    Operators read this as "the agent tried to skip required
    practices".  Occasional blocks are the guard doing its job;
    chronic blocks on the same skill suggest its catalogue summary
    isn't landing (rewrite it) or the skill is over-scoped (narrow
    the trigger).
    """

    type: str = "phase.tool_skill_blocked"
    agent_id: str = ""
    role: str = ""
    phase: str = ""
    tool_name: str = ""
    skill_keys: list[str] = Field(default_factory=list)
    turn_id: str = ""
    iteration: int = 0

    @property
    def summary(self) -> str:
        keys = ", ".join(self.skill_keys)
        return (
            f"{self.actor} {self.phase} call to '{self.tool_name}' "
            f"blocked pending required skill(s): {keys}"
        )


class PromptSize(Event):
    """Emitted per phase with the final system + user prompt token
    count, for measuring prompt-slimming progress over time.
    """

    type: str = "prompt.size"
    agent_id: str = ""
    role: str = ""
    phase: str = ""
    approximate_tokens: int = 0
    system_chars: int = 0
    user_chars: int = 0

    @property
    def summary(self) -> str:
        return f"{self.actor} {self.phase} prompt ~{self.approximate_tokens} tokens"


class TurnGuardBreach(Event):
    """Emitted whenever a runtime-invariant guard fires during a turn.

    Surfaces every runtime-invariant guard breach in the dashboard
    events table (the guards themselves also log).

    ``kind`` is one of:

    - ``depth_cap``            -- delegation_depth >= delegation_depth_limit
    - ``stall``                -- two self_iterate rounds with unchanged
      artifact hash; turn terminates as ``failed``
    - ``max_iter``             -- plan/execute/review loop exhausted the
      iteration cap without ``done``; turn terminates as ``failed``
    - ``unhandled_exception``  -- an uncaught exception escaped the turn
      body; published before the exception propagates
    - ``scheduled_timeout``    -- a scheduled turn exceeded its
      wall-clock cap; turn terminates as ``failed``
    """

    type: str = "turn.guard_breach"
    agent_id: str = ""
    role: str = ""
    kind: str = ""
    detail: str = ""
    turn_id: str = ""

    @property
    def summary(self) -> str:
        return f"{self.actor} guard {self.kind}: {self.detail}"
