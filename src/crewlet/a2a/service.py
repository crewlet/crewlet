"""A2AService — manages A2A channel lifecycle within the Engine."""

from __future__ import annotations

from datetime import UTC, datetime
from uuid import uuid4

from crewlet._logging import get_logger
from crewlet.a2a.messages import ChannelMessage
from crewlet.a2a.protocol import A2ABus
from crewlet.events.types import (
    A2AChannelClosed,
    A2AChannelOpened,
    A2AMessageSent,
    Event,
)
from crewlet.queue.protocol import EventQueue
from crewlet.queue.topics import agent_inbox_topic

logger = get_logger("a2a.service")


class _ChannelState:
    """Internal bookkeeping for an open A2A channel."""

    __slots__ = ("participants", "created_at", "message_count", "requester")

    def __init__(self, participants: list[str], requester: str) -> None:
        self.participants = participants
        self.created_at = datetime.now(UTC)
        self.message_count = 0
        self.requester = requester


class A2AService:
    """Manages A2A channel lifecycle within the Engine."""

    def __init__(self, bus: A2ABus, queue: EventQueue) -> None:
        self._bus = bus
        self._queue = queue
        self._channels: dict[str, _ChannelState] = {}
        self._handle_registry = None

    def set_handle_registry(self, registry) -> None:
        """Wire the identity registry so the service can defend its
        own contract (the bus connects agent seats — see
        :meth:`request_channel`).  Set by the engine once the
        registry exists; ``None`` (tests, embedded use) skips the
        guard."""
        self._handle_registry = registry

    async def request_channel(
        self,
        requester: str,
        target: str,
        *,
        delegation_depth: int = 0,
        delegation_chain: list[str] | None = None,
        parent_turn_id: str = "",
    ) -> str:
        """Create a channel and wake the target agent.

        1. Create a temporary channel on the A2A Bus
        2. Publish a wake event to the target agent's inbox via the Event Queue
        3. Publish an ``a2a_channel_opened`` event for observability
        4. Return the channel_id for the requester to begin sending

        ``delegation_depth`` / ``delegation_chain`` / ``parent_turn_id``
        are copied onto the wake event so the target's TurnEngine
        inherits depth+1 and can enforce the cap.

        Raises ``ValueError`` when a registry is wired and ``target``
        is not an agent seat in the org.  This is the chokepoint that
        creates the inbox wake — without the guard, a human seat or a
        typo'd handle produces a channel whose wake event lands on a
        subscriber-less topic: the requester reports success and waits
        on a reply that can never come.  Callers (``a2a_ask``) surface
        the error as a tool failure so it stays visible — the agent
        re-plans, or reaches the target on a human surface (Slack /
        Jira) instead.

        The question the guard asks is "does this seat exist and does
        it have an inbox?", NOT "is it running in this process".  A
        colleague owned by another node is a perfectly good A2A target:
        the wake lands on its inbox and that node consumes it.  Asking
        the local pool instead would have made every cross-node ask
        fail as a typo — the more nodes, the fewer colleagues an agent
        appears to have.
        """
        if self._handle_registry is not None:
            party = self._handle_registry.resolve_party(target)
            if party is None or party.is_human:
                raise ValueError(
                    f"a2a target '{target}' is not an agent seat — the A2A "
                    f"bus connects agents only (human seats and unknown "
                    f"handles have no inbox)"
                )
        channel_id = f"a2a-{uuid4().hex[:12]}"
        participants = [requester, target]
        await self._bus.create_channel(channel_id, participants)
        self._channels[channel_id] = _ChannelState(participants, requester=requester)

        chain = list(delegation_chain or [])
        if requester and requester not in chain:
            chain = [*chain, requester]
        wake_event = Event(
            type="a2a_request",
            source=requester,
            payload={"channel_id": channel_id, "requester": requester},
            delegation_depth=delegation_depth + 1,
            delegation_chain=chain,
            parent_turn_id=parent_turn_id,
        )
        await self._queue.publish(agent_inbox_topic(target), wake_event)

        opened_event = A2AChannelOpened(
            source=requester,
            channel_id=channel_id,
            requester=requester,
            target=target,
            participants=participants,
        )
        await self._queue.publish("crewlet.events.a2a_channel_opened", opened_event)

        logger.info(
            "channel_requested",
            channel_id=channel_id,
            requester=requester,
            target=target,
            participants=participants,
        )
        return channel_id

    async def send(
        self,
        channel_id: str,
        sender: str,
        content: str,
        sender_role: str = "",
    ) -> None:
        """Send a message on an open A2A channel.

        Builds the ``ChannelMessage`` internally to prevent
        sender/channel spoofing.

        Raises:
            ValueError: If the channel does not exist or the sender
                is not a participant.
        """
        state = self._channels.get(channel_id)
        if state is None:
            logger.error(
                "send_on_unknown_channel",
                channel_id=channel_id,
                sender=sender,
            )
            raise ValueError(
                f"A2A channel '{channel_id}' is not open or does not exist"
            )

        if sender not in state.participants:
            logger.error(
                "send_by_non_participant",
                channel_id=channel_id,
                sender=sender,
                participants=state.participants,
            )
            raise ValueError(
                f"Sender '{sender}' is not a participant of channel '{channel_id}'"
            )

        msg = ChannelMessage(
            channel=channel_id,
            sender=sender,
            sender_role=sender_role,
            content=content,
        )
        await self._bus.send(channel_id, sender, msg)
        state.message_count += 1

        # Determine recipient (the other participant in a 1:1 channel).
        recipients = [p for p in state.participants if p != sender]
        recipient = recipients[0] if recipients else ""

        sent_event = A2AMessageSent(
            source=sender,
            channel_id=channel_id,
            sender=sender,
            sender_role=sender_role,
            recipient=recipient,
            message_id=str(msg.id),
            content=content,
        )
        await self._queue.publish("crewlet.events.a2a_message_sent", sent_event)

        # Wake the recipient so they pick up the new message.  Skip for the
        # initial message sent by the channel requester — request_channel()
        # already published an ``a2a_request`` wake event for that.
        is_reply = sender != state.requester
        if recipient and is_reply:
            wake_event = Event(
                type="a2a_message",
                source=sender,
                payload={
                    "channel_id": channel_id,
                    "sender": sender,
                },
            )
            await self._queue.publish(agent_inbox_topic(recipient), wake_event)

        logger.info(
            "message_sent",
            channel_id=channel_id,
            sender=sender,
            sender_role=sender_role,
            recipient=recipient,
            content_length=len(content),
            message_number=state.message_count,
        )

    async def close_channel(self, channel_id: str, closer: str = "") -> None:
        """Close a channel on the A2A Bus and clean up state.

        Args:
            channel_id: The channel to close.
            closer: Handle of the agent requesting the close. When
                provided, must be a participant of the channel.

        Raises:
            ValueError: If *closer* is provided and is not a
                participant of the channel.
        """
        state = self._channels.get(channel_id)
        if closer and state is not None and closer not in state.participants:
            logger.error(
                "close_by_non_participant",
                channel_id=channel_id,
                closer=closer,
                participants=state.participants,
            )
            raise ValueError(
                f"'{closer}' is not a participant of channel '{channel_id}'"
            )

        participants = state.participants if state else []
        message_count = state.message_count if state else 0
        duration_ms = 0.0
        if state:
            elapsed = datetime.now(UTC) - state.created_at
            duration_ms = elapsed.total_seconds() * 1000

        self._channels.pop(channel_id, None)
        await self._bus.close_channel(channel_id)

        closed_event = A2AChannelClosed(
            source=closer or "system",
            channel_id=channel_id,
            closed_by=closer,
            participants=participants,
            message_count=message_count,
            duration_ms=duration_ms,
        )
        await self._queue.publish("crewlet.events.a2a_channel_closed", closed_event)

        logger.info(
            "channel_closed",
            channel_id=channel_id,
            closed_by=closer or "system",
            participants=participants,
            message_count=message_count,
            duration_ms=round(duration_ms, 1),
        )
