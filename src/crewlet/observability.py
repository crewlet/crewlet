"""Observability — structured logging, tracing hooks, and metrics."""

from __future__ import annotations

import asyncio
from collections import defaultdict
from collections.abc import Callable
from datetime import UTC, datetime
from typing import Any

from pydantic import BaseModel, Field

from crewlet._logging import get_logger
from crewlet.events.types import Event
from crewlet.queue.protocol import EventQueue

logger = get_logger("observability")


class TokenUsage(BaseModel):
    """Token usage tracking for an agent or the entire org."""

    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    call_count: int = 0

    def add(
        self,
        input_tokens: int = 0,
        output_tokens: int = 0,
    ) -> None:
        self.input_tokens += input_tokens
        self.output_tokens += output_tokens
        self.total_tokens += input_tokens + output_tokens
        self.call_count += 1


class LLMInvocationRecord(BaseModel):
    """A record of one LLM loop (a phase's tool loop, or a whole turn).

    ``turn_id`` / ``phase`` / ``iteration`` mirror the coordinates on
    :class:`~crewlet.events.types.AgentTurnProgress` — per-round
    progress updates for the same loop collapse into a single record.
    Whole-turn aggregates (``agent_turn_completed``) use
    ``phase="turn"``.
    """

    timestamp: datetime
    model: str
    turn_id: str = ""
    phase: str = ""
    iteration: int = 0
    prompt: str
    response: str
    input_tokens: int
    output_tokens: int
    total_tokens: int
    tool_executions: list[dict]


class AgentMetrics(BaseModel):
    """Metrics for a single agent."""

    agent_id: str
    role: str
    turns_completed: int = 0
    turns_failed: int = 0
    tokens: TokenUsage = Field(default_factory=TokenUsage)
    total_duration_ms: float = 0
    llm_history: list[LLMInvocationRecord] = Field(default_factory=list)


class A2AChannelMetrics(BaseModel):
    """Metrics for a single A2A channel."""

    channel_id: str
    requester: str = ""
    target: str = ""
    participants: list[str] = Field(default_factory=list)
    messages_sent: int = 0
    messages_delivered: int = 0
    total_content_length: int = 0
    opened_at: datetime | None = None
    closed_at: datetime | None = None
    duration_ms: float = 0


class EngineMetrics(BaseModel):
    """Aggregate metrics for the entire engine."""

    started_at: datetime | None = None
    events_processed: int = 0
    tasks_created: int = 0
    tasks_completed: int = 0
    tasks_failed: int = 0
    messages_sent: int = 0
    a2a_channels_opened: int = 0
    a2a_messages_sent: int = 0
    a2a_messages_delivered: int = 0
    a2a_channels_closed: int = 0
    total_tokens: TokenUsage = Field(default_factory=TokenUsage)


class ObservabilityManager:
    """Collects metrics, logs events, and tracks token usage.

    Subscribes to the event queue and automatically tracks
    metrics across the engine.
    """

    # Event types the observability manager tracks.
    _TRACKED_EVENTS = [
        "task_created",
        "task_completed",
        "task_failed",
        "task_started",
        "message_sent",
        "a2a_channel_opened",
        "a2a_message_sent",
        "a2a_message_delivered",
        "a2a_channel_closed",
        "agent_spawned",
        "agent_turn_progress",
        "agent_turn_completed",
    ]

    def __init__(self, event_queue: EventQueue) -> None:
        self._event_queue = event_queue
        self.engine_metrics = EngineMetrics()
        self.agent_metrics: dict[str, AgentMetrics] = {}
        self._a2a_channels: dict[str, A2AChannelMetrics] = {}
        self._hooks: dict[str, list[Callable[..., Any]]] = defaultdict(list)

    async def start(self) -> None:
        """Subscribe to events and begin tracking."""
        self.engine_metrics.started_at = datetime.now(UTC)
        for event_type in self._TRACKED_EVENTS:
            await self._event_queue.subscribe(
                f"crewlet.events.{event_type}",
                "observability",
                self._on_event,
            )
        logger.info("observability_started")

    def register_hook(self, hook_name: str, callback: Callable[..., Any]) -> None:
        """Register a callback for a named hook point."""
        logger.debug("hook_registered", hook_name=hook_name)
        self._hooks[hook_name].append(callback)

    async def _fire_hooks(self, hook_name: str, **kwargs: Any) -> None:
        """Fire all callbacks for a hook point (sync or async)."""
        hooks = self._hooks.get(hook_name, [])
        logger.debug("hooks_firing", count=len(hooks), hook_name=hook_name)
        for callback in hooks:
            result = callback(**kwargs)
            if asyncio.iscoroutine(result):
                await result

    async def _on_event(self, event: Event) -> None:
        """Process an event for metrics tracking."""
        self.engine_metrics.events_processed += 1

        match event.type:
            case "task_created":
                self.engine_metrics.tasks_created += 1
            case "task_completed":
                self.engine_metrics.tasks_completed += 1
                agent_id = getattr(event, "agent_id", "")
                if agent_id and agent_id in self.agent_metrics:
                    self.agent_metrics[agent_id].turns_completed += 1
                await self._fire_hooks("on_task_complete", event=event)
            case "task_failed":
                self.engine_metrics.tasks_failed += 1
                agent_id = getattr(event, "agent_id", "")
                if agent_id and agent_id in self.agent_metrics:
                    self.agent_metrics[agent_id].turns_failed += 1
            case "task_started":
                await self._fire_hooks("on_turn_start", event=event)
            case "message_sent":
                self.engine_metrics.messages_sent += 1
            case "a2a_channel_opened":
                self.engine_metrics.a2a_channels_opened += 1
                channel_id = getattr(event, "channel_id", "")
                if channel_id:
                    self._a2a_channels[channel_id] = A2AChannelMetrics(
                        channel_id=channel_id,
                        requester=getattr(event, "requester", ""),
                        target=getattr(event, "target", ""),
                        participants=getattr(event, "participants", []),
                        opened_at=event.timestamp,
                    )
                await self._fire_hooks("on_a2a_channel_opened", event=event)
            case "a2a_message_sent":
                self.engine_metrics.a2a_messages_sent += 1
                channel_id = getattr(event, "channel_id", "")
                if channel_id in self._a2a_channels:
                    ch = self._a2a_channels[channel_id]
                    ch.messages_sent += 1
                    ch.total_content_length += getattr(event, "content_length", 0)
                await self._fire_hooks("on_a2a_message_sent", event=event)
            case "a2a_message_delivered":
                self.engine_metrics.a2a_messages_delivered += 1
                channel_id = getattr(event, "channel_id", "")
                if channel_id in self._a2a_channels:
                    self._a2a_channels[channel_id].messages_delivered += getattr(
                        event, "message_count", 0
                    )
                await self._fire_hooks("on_a2a_message_delivered", event=event)
            case "a2a_channel_closed":
                self.engine_metrics.a2a_channels_closed += 1
                channel_id = getattr(event, "channel_id", "")
                if channel_id in self._a2a_channels:
                    ch = self._a2a_channels[channel_id]
                    ch.closed_at = event.timestamp
                    ch.duration_ms = getattr(event, "duration_ms", 0)
                    ch.messages_sent = max(
                        ch.messages_sent,
                        getattr(event, "message_count", 0),
                    )
                await self._fire_hooks("on_a2a_channel_closed", event=event)
            case "agent_spawned":
                agent_id = getattr(event, "agent_id", "")
                role = getattr(event, "role", "")
                if agent_id:
                    self.agent_metrics[agent_id] = AgentMetrics(
                        agent_id=agent_id, role=role
                    )
            case "agent_turn_progress":
                agent_id = getattr(event, "agent_id", "")
                if agent_id and agent_id in self.agent_metrics:
                    record = LLMInvocationRecord(
                        timestamp=event.timestamp,
                        model=getattr(event, "model", ""),
                        turn_id=getattr(event, "turn_id", ""),
                        phase=getattr(event, "phase", ""),
                        iteration=getattr(event, "iteration", 0),
                        prompt=getattr(event, "prompt", ""),
                        response=getattr(event, "response", ""),
                        input_tokens=getattr(event, "input_tokens", 0),
                        output_tokens=getattr(event, "output_tokens", 0),
                        total_tokens=getattr(event, "total_tokens", 0),
                        tool_executions=getattr(event, "tool_executions", []),
                    )
                    self._upsert_llm_history(agent_id, record)
            case "agent_turn_completed":
                agent_id = getattr(event, "agent_id", "")
                if agent_id and agent_id in self.agent_metrics:
                    record = LLMInvocationRecord(
                        timestamp=event.timestamp,
                        model=getattr(event, "model", ""),
                        turn_id=getattr(event, "turn_id", ""),
                        phase="turn",
                        prompt=getattr(event, "prompt", ""),
                        response=getattr(event, "response", ""),
                        input_tokens=getattr(event, "input_tokens", 0),
                        output_tokens=getattr(event, "output_tokens", 0),
                        total_tokens=getattr(event, "total_tokens", 0),
                        tool_executions=getattr(event, "tool_executions", []),
                    )
                    self._upsert_llm_history(agent_id, record)

        # Structured log — include A2A channel_id when present for traceability.
        extra: dict[str, Any] = {}
        if channel_id := getattr(event, "channel_id", ""):
            extra["channel_id"] = channel_id
        if a2a_ctx := getattr(event, "a2a_context", None):
            extra["a2a_channel_id"] = a2a_ctx.get("channel_id", "")

        logger.info(
            "event_processed",
            event_type=event.type,
            event_id=str(event.id),
            source=event.source,
            **extra,
        )

    def _upsert_llm_history(self, agent_id: str, record: LLMInvocationRecord) -> None:
        """Insert or in-place-update a record in the agent's LLM history.

        Records are keyed by their turn coordinates ``(turn_id, phase,
        iteration)`` — an in-flight loop's per-round progress updates
        collapse into one record that the final round overwrites.
        Records without a ``turn_id`` (events from loop callers outside
        the turn engine) fall back to a heuristic: same prompt
        as the latest record means same loop.
        """
        history = self.agent_metrics[agent_id].llm_history
        if record.turn_id:
            for i in range(len(history) - 1, -1, -1):
                prior = history[i]
                if (
                    prior.turn_id == record.turn_id
                    and prior.phase == record.phase
                    and prior.iteration == record.iteration
                ):
                    history[i] = record
                    return
        elif (
            history and not history[-1].turn_id and history[-1].prompt == record.prompt
        ):
            history[-1] = record
            return
        history.append(record)
        if len(history) > 50:
            history.pop(0)

    def track_tokens(
        self,
        agent_id: str,
        input_tokens: int = 0,
        output_tokens: int = 0,
        total_tokens: int = 0,
    ) -> None:
        """Track token usage for an agent.

        When ``input_tokens`` and ``output_tokens`` are both provided,
        they are used directly.  When only ``total_tokens`` is given
        (provider doesn't report a breakdown), the total is attributed
        to ``input_tokens`` for accounting.
        """
        if input_tokens or output_tokens:
            # Provider gave a breakdown — use it as-is
            effective_input = input_tokens
            effective_output = output_tokens
        else:
            # No breakdown — attribute total to input
            effective_input = total_tokens
            effective_output = 0
        logger.debug(
            "token_tracking",
            agent_id=agent_id,
            input_tokens=effective_input,
            output_tokens=effective_output,
            total_tokens=effective_input + effective_output,
        )
        self.engine_metrics.total_tokens.add(
            effective_input,
            effective_output,
        )
        if agent_id in self.agent_metrics:
            self.agent_metrics[agent_id].tokens.add(
                effective_input,
                effective_output,
            )

    def get_summary(self) -> dict[str, Any]:
        """Get a summary of all metrics."""
        logger.debug("generating_summary", agents_tracked=len(self.agent_metrics))
        return {
            "engine": self.engine_metrics.model_dump(),
            "agents": {aid: m.model_dump() for aid, m in self.agent_metrics.items()},
            "a2a_channels": {
                cid: m.model_dump() for cid, m in self._a2a_channels.items()
            },
        }
