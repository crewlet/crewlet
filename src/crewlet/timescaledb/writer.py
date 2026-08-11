"""Event store writer — persists engine events to the event store.

Designed to be registered as a publish listener on the EventQueue so
events are written to the backing store synchronously at publish time —
no queue round-trip, consumer groups, or race conditions.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from crewlet._logging import get_logger

if TYPE_CHECKING:
    from crewlet.events.types import Event
    from crewlet.timescaledb.protocol import EventStore

logger = get_logger("timescaledb.writer")

# Maps event type string → category label.
_CATEGORY_MAP: dict[str, str] = {
    "org_started": "lifecycle",
    "org_stopped": "lifecycle",
    "agent_spawned": "lifecycle",
    "agent_terminated": "lifecycle",
    "agent_reassigned": "lifecycle",
    "role_updated": "lifecycle",
    "task_created": "task",
    "task_assigned": "task",
    "task_started": "task",
    "task_completed": "task",
    "task_failed": "task",
    "task_delegated": "task",
    "message_sent": "communication",
    "a2a_channel_opened": "a2a",
    "a2a_message_sent": "a2a",
    "a2a_message_delivered": "a2a",
    "a2a_channel_closed": "a2a",
    "decision_requested": "decision",
    "decision_resolved": "decision",
    "contribution_requested": "decision",
    "contribution_received": "decision",
    "document_created": "knowledge",
    "document_updated": "knowledge",
    "external_notification": "notification",
    "notification_skipped": "notification",
    # Inbox coalescing: N same-conversation events merged into one
    # digest trigger.  The event exists FOR the event store / dashboard
    # (operators watch when and how hard batching kicks in) — without
    # this entry the writer silently drops it.
    "notifications_coalesced": "notification",
    "budget_exhausted": "system",
    "llm_unavailable": "system",
    "agent_turn_completed": "system",
    "agent_phase_started": "system",
    "agent_phase_completed": "system",
    "execute.missing_tool": "system",
    "phase.tool_activated": "system",
    "prompt.size": "system",
    "turn.guard_breach": "system",
    # Learning subsystem (phases 1-5).  All grouped under the
    # ``learning`` category so the dashboard's category filter can
    # include / exclude reflection traffic in one toggle.
    "turn_completed": "learning",
    "episode_written": "learning",
    "persist_decider_completed": "learning",
    "counterparty_profile_updated": "learning",
    "skill_synthesized": "learning",
    "skill_refined": "learning",
    "skill_promoted": "learning",
    "reflection_completed": "learning",
    # Plan-prefetch telemetry: the once-per-turn prefetch summary
    # (carries the thin-trigger gate decision) and the post-Plan
    # relevant-knowledge re-fetch.  Without these entries the writer
    # silently drops both, and the gate / re-fetch are invisible in
    # the dashboard despite being emitted.
    "plan_prefetch_summary": "learning",
    "relevant_knowledge_refetched": "learning",
}


class EventStoreWriter:
    """Writes engine events directly to an EventStore.

    Register as a publish listener via
    ``event_queue.add_publish_listener(writer.on_publish)``.
    """

    def __init__(self, event_store: EventStore) -> None:
        self._event_store = event_store

    async def on_publish(self, topic: str, event: Event) -> None:
        """Publish listener callback — called on every event_queue.publish()."""
        category = _CATEGORY_MAP.get(event.type)
        if category is None:
            return  # Not a tracked event type
        summary = event.summary
        actor = event.actor
        tags = self._extract_tags(event)
        try:
            await self._event_store.write_event(
                event_id=str(event.id),
                event_type=event.type,
                source=event.source,
                timestamp=event.timestamp,
                category=category,
                payload=event.model_dump(mode="json"),
                summary=summary,
                actor=actor,
                trace_id=getattr(event, "trace_id", ""),
                span_id=getattr(event, "span_id", ""),
                parent_span_id=getattr(event, "parent_span_id", ""),
                tags=tags,
            )
        except Exception as exc:
            # Observability writes are fire-and-forget; failures must
            # not disrupt the event pipeline.
            logger.exception(
                "event_write_failed",
                event_type=event.type,
                event_id=str(event.id),
                error=str(exc),
            )

    @staticmethod
    def _extract_tags(event: Event) -> dict[str, str]:
        """Extract filterable dimensions from the event."""
        tags: dict[str, str] = {}
        if agent_id := getattr(event, "agent_id", ""):
            tags["agent_id"] = agent_id
        if role := getattr(event, "role", ""):
            tags["agent_role"] = role
        if task_id := getattr(event, "task_id", ""):
            tags["task_id"] = task_id
        if channel_id := getattr(event, "channel_id", ""):
            tags["channel_id"] = channel_id
        if sender := getattr(event, "sender", ""):
            tags["sender"] = sender
        # A2A-specific tags for richer filtering.
        if requester := getattr(event, "requester", ""):
            tags["requester"] = requester
        if target := getattr(event, "target", ""):
            tags["target"] = target
        if recipient := getattr(event, "recipient", ""):
            tags["recipient"] = recipient
        if closed_by := getattr(event, "closed_by", ""):
            tags["closed_by"] = closed_by
        # Tag turns triggered by A2A for cross-referencing.
        if (a2a_ctx := getattr(event, "a2a_context", None)) and (
            a2a_ch := a2a_ctx.get("channel_id", "")
        ):
            tags["a2a_channel_id"] = a2a_ch
        return tags
