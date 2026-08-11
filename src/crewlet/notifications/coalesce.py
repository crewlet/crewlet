"""Inbox coalescing — conversation keys and digest-trigger merging.

Companion of the EventQueue's batch delivery
(:meth:`crewlet.queue.protocol.EventQueue.subscribe_batch`): the engine
partitions an agent's pending inbox events by :func:`conversation_key`,
and a multi-event partition is merged by
:func:`coalesce_notifications` into ONE digest trigger so N comments on
the same Jira issue / Slack thread cost one turn instead of N.

Per-source knowledge (which metadata identifies a conversation, which
constituent bodies are superseded by later state) lives on the
``NotificationPrompt`` classes — the same registry that owns prompt
building and ``requires_recon``.  This module is the source-agnostic
merge algorithm.

See ``docs/concepts/event-system.md`` § Inbox batching for the operator
view.
"""

from __future__ import annotations

from crewlet.events.types import CoalescedMessage, Event, ExternalNotification
from crewlet.notifications.notification_prompts import NotificationPrompt, get_prompt


def conversation_key(event: Event) -> str:
    """Batch-partition key for one agent-inbox event.

    External notifications key on their conversation (Jira issue,
    Slack thread, Confluence page, GitHub PR/issue), namespaced by
    source so two sources can never collide.  Everything else —
    ``task_assigned``, A2A wakes, informational events, notifications
    without a derivable conversation — keys uniquely on the event id
    and is therefore never coalesced: single-event partitions follow
    exactly the pre-batching dispatch path.
    """
    if event.type == "external_notification":
        source = getattr(event, "notification_source", "") or ""
        metadata = getattr(event, "metadata", None) or {}
        subject = getattr(event, "subject", "") or ""
        local = get_prompt(source).conversation_key(metadata, subject=subject)
        if local:
            return f"{source}:{local}"
    return f"event:{event.id}"


def coalesce_notifications(
    events: list[ExternalNotification],
) -> ExternalNotification:
    """Merge ≥2 same-conversation notifications into one digest trigger.

    The merged event is shaped so every existing consumer keeps
    working unchanged:

    - ``body`` — the planner prompt: a chronological digest of the
      earlier messages (supersede rules applied per source via
      ``digest_entry_body``), then the *latest* constituent's full
      enriched body.  Source scaffolding (triage rules, Get Full
      Context) therefore renders exactly once, and its conversation
      pointers come from the most recent state.
    - ``messages`` — every constituent in chronological order, full
      fidelity (sender, salient body, metadata, per-message recon
      flag), for the learning workers.
    - flat fields (``sender`` / ``subject`` / ``metadata`` / trace
      context) — mirror the latest constituent, whose metadata carries
      the issue / thread pointers the turn needs.
    - ``context_requires_recon`` — conservative merge: recon is
      required when any constituent required it.
    - delegation bookkeeping — taken from the maximum-depth
      constituent so batching cannot launder the depth cap.
    """
    if not events:
        raise ValueError("coalesce_notifications requires at least one event")
    ordered = sorted(events, key=lambda e: e.timestamp)
    latest = ordered[-1]
    if len(ordered) == 1:
        return latest

    prompt = get_prompt(latest.notification_source)
    duplicates = _later_duplicates(prompt, ordered)
    digest_lines: list[str] = []
    for index, event in enumerate(ordered[:-1]):
        digest_lines.append(_digest_line(prompt, event, duplicate=index in duplicates))

    body = (
        f"## Coalesced updates ({len(ordered)} events in this conversation)\n"
        "Multiple events for this conversation arrived in quick succession"
        " (or while you were busy). They are listed chronologically below;"
        " the MOST RECENT event is rendered in full afterwards. Handle them"
        " as ONE piece of work — read everything before acting, and reply"
        " at most once.\n\n"
        + "\n".join(digest_lines)
        + "\n\n---\n\n"
        + (latest.body or "")
    )

    messages = [
        CoalescedMessage(
            sender=event.sender,
            body=_raw_body(event),
            timestamp=event.timestamp,
            source_event_type=event.source_event_type,
            metadata=dict(event.metadata),
            context_requires_recon=event.context_requires_recon,
        )
        for event in ordered
    ]
    salient_body = "\n\n".join(
        line
        for index, event in enumerate(ordered)
        for line in (_salient_line(prompt, event),)
        if line and index not in duplicates
    )

    # Depth-cap bookkeeping must survive the merge un-laundered: the
    # most-constrained constituent wins.
    deepest = max(ordered, key=lambda e: e.delegation_depth)

    return ExternalNotification(
        source=latest.source,
        notification_source=latest.notification_source,
        source_event_type=latest.source_event_type,
        recipient_email=latest.recipient_email,
        agent_id=latest.agent_id,
        sender=latest.sender,
        subject=latest.subject,
        body=body,
        salient_body=salient_body,
        metadata=dict(latest.metadata),
        context_requires_recon=any(e.context_requires_recon for e in ordered),
        messages=messages,
        # Continue the latest constituent's trace so the merged turn
        # correlates with the webhook that completed the batch.
        trace_id=latest.trace_id,
        span_id=latest.span_id,
        parent_span_id=latest.parent_span_id,
        delegation_depth=deepest.delegation_depth,
        parent_turn_id=deepest.parent_turn_id,
        delegation_chain=list(deepest.delegation_chain),
    )


def _raw_body(event: ExternalNotification) -> str:
    """The constituent's raw inbound message.

    Honors the ``salient_body`` contract (see
    :class:`~crewlet.events.types.ExternalNotification`): ``None``
    means the producer emitted no distinct raw message — fall back to
    ``body`` (for such producers, e.g. extensions without a prompt
    builder, the body IS the message).  An empty string is a genuinely
    contentless message and must NOT fall back.
    """
    if event.salient_body is not None:
        return event.salient_body
    return event.body or ""


def _effective_digest_body(
    prompt: NotificationPrompt, event: ExternalNotification
) -> str:
    """Body to render for *event* in digest / merged-salient text.

    One resolver for both :func:`_digest_line` and
    :func:`_salient_line` so the planner-facing digest and the
    learning-facing salient text can never desynchronize: the raw
    body (per the ``salient_body`` contract) passed through the
    source's supersede hook.
    """
    event_type = event.metadata.get("event_type", "") or event.source_event_type
    return prompt.digest_entry_body(event_type, _raw_body(event))


def _later_duplicates(
    prompt: NotificationPrompt, ordered: list[ExternalNotification]
) -> set[int]:
    """Indexes whose effective body a LATER same-sender constituent repeats.

    Webhook sources re-emit unchanged state per event (GitHub sends the
    full PR description on every lifecycle action); rendering N
    identical copies buries the one actionable line and pollutes the
    merged salient text the learning filters embed.  A constituent is a
    duplicate only when a *later* event from the *same sender* carries
    the byte-identical effective body — the later copy renders (in the
    digest, or in full as the latest constituent), so no content is
    lost, while two different people saying "+1" both survive.
    """
    duplicates: set[int] = set()
    bodies = [_effective_digest_body(prompt, event).strip() for event in ordered]
    for i, event in enumerate(ordered):
        if not bodies[i]:
            continue
        for j in range(i + 1, len(ordered)):
            if bodies[j] == bodies[i] and ordered[j].sender == event.sender:
                duplicates.add(i)
                break
    return duplicates


def _digest_line(
    prompt: NotificationPrompt,
    event: ExternalNotification,
    *,
    duplicate: bool = False,
) -> str:
    """Render one earlier constituent as a digest bullet."""
    sender = event.sender or "(unknown sender)"
    when = event.metadata.get("timestamp", "") or event.timestamp.isoformat(
        timespec="seconds"
    )
    event_type = event.metadata.get("event_type", "") or event.source_event_type
    label = f"- **{sender}** ({when}{', ' + event_type if event_type else ''})"
    if duplicate:
        return f"{label}: (no new content — identical to a later update)"
    body = _effective_digest_body(prompt, event)
    if not body:
        return f"{label}: (no message body — superseded by later state)"
    body = body.strip()
    # Each constituent body renders in full -- no length cap. The agent
    # reads the digest as its trigger, so truncating it would hide
    # content it may need to act on.
    # Indent continuation lines so multi-line bodies stay inside the bullet.
    body = body.replace("\n", "\n  ")
    return f"{label}: {body}"


def _salient_line(prompt: NotificationPrompt, event: ExternalNotification) -> str:
    """Sender-attributed salient body for the merged ``salient_body``.

    Applies the same per-source supersede hook as the digest so the
    merged salient text matches what the planner sees; constituents
    whose body is superseded contribute nothing.
    """
    body = _effective_digest_body(prompt, event)
    if not body:
        return ""
    sender = event.sender or "(unknown sender)"
    return f"{sender}: {body.strip()}"
