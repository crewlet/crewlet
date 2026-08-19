"""A2AService — agent-to-agent channels over the durable queue.

Two things about this used to be process-local, and both of them were
load-bearing.

**The message content.** It rode an ``asyncio.Queue`` per channel while
the *wake* rode the seat's durable inbox. The target of an ask wakes on
the node that owns its seat, which is rarely the node that opened the
channel — so cross-node the wake arrived and the queue was empty, and
the agent was told *"they opened a channel but hasn't sent a message
yet"*. Content now rides the wake event itself, which is durable,
ordered per conversation, owner-routed and covered by the completion
ledger. There is no second delivery path to disagree with it.

**The channel's participants and state.** They were a dict, and every
authorization decision reads them: may this sender post, may this closer
close, who is the other party. Cross-node that dict is empty on the node
that has to answer, so a legitimate reply raised "channel is not open or
does not exist". They live in :mod:`crewlet.a2a.channels` now.

What did NOT need to become durable is the reply mechanism, because
there wasn't one. ``a2a_ask`` promises *"they reply on the same
channel"* and the wake prompt told the agent to call
``send_a2a_message`` — a tool that does not exist, and which
``tests/test_tools/test_builtin.py`` asserts is not registered. The
answering turn's final text is the reply now: no new LLM-facing tool, no
channel lifecycle for a model to manage, and the promise is kept.
"""

from __future__ import annotations

from uuid import uuid4

from crewlet._logging import get_logger
from crewlet.a2a.channels import A2AChannelStore, MemoryA2AChannelStore
from crewlet.a2a.messages import ChannelMessage
from crewlet.events.types import (
    A2AChannelClosed,
    A2AChannelOpened,
    A2AMessageSent,
    Event,
)
from crewlet.queue.protocol import EventQueue
from crewlet.queue.topics import agent_inbox_topic

logger = get_logger("a2a.service")


class A2AService:
    """Manages A2A channel lifecycle within the Engine."""

    def __init__(
        self,
        queue: EventQueue,
        channels: A2AChannelStore | None = None,
    ) -> None:
        self._queue = queue
        self._channels: A2AChannelStore = channels or MemoryA2AChannelStore()
        self._handle_registry = None

    def set_channel_store(self, channels: A2AChannelStore) -> None:
        """Swap in the durable store once the database is known."""
        self._channels = channels

    @property
    def channels(self) -> A2AChannelStore:
        return self._channels

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
        brief: str = "",
        sender_role: str = "",
        delegation_depth: int = 0,
        delegation_chain: list[str] | None = None,
        parent_turn_id: str = "",
    ) -> str:
        """Open a channel and wake the target, carrying the brief.

        The brief travels ON the wake event. It used to be pushed into a
        per-channel ``asyncio.Queue`` while only the wake was durable,
        which meant the content existed on exactly one node and the wake
        was delivered to whichever node owned the target's seat. Those
        are the same node only by luck.

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
        if requester and requester == target:
            # A channel to yourself has no responder. ``_handle_a2a``
            # decides who answers by comparing the woken seat against
            # the channel's requester, so a self-channel wakes the
            # asker, is read as an incoming ANSWER, and is never
            # replied to — a turn spent on a question nobody was asked,
            # and a row the idle sweep eventually closes. The agent
            # wanted to think, not to ask; say so.
            raise ValueError(
                f"a2a target '{target}' is you — a channel has two "
                f"parties. Reason it through in this turn instead of "
                f"asking yourself."
            )
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
        await self._channels.open(channel_id, requester=requester, target=target)

        # Announce the channel and record the brief BEFORE the wake.
        #
        # The wake is what runs the other agent's turn, and on the
        # in-process queue that happens inline: publishing it first put
        # the whole conversation — the answer, and the close — on the
        # observability topics ahead of the question that caused it, so
        # a dashboard trace read backwards. The order here is the order
        # things happened.
        opened_event = A2AChannelOpened(
            source=requester,
            channel_id=channel_id,
            requester=requester,
            target=target,
            participants=participants,
        )
        await self._queue.publish("crewlet.events.a2a_channel_opened", opened_event)

        if brief:
            await self._channels.count_message(channel_id)
            await self._publish_sent(channel_id, requester, target, brief, sender_role)

        chain = list(delegation_chain or [])
        if requester and requester not in chain:
            chain = [*chain, requester]
        wake_event = Event(
            type="a2a_request",
            source=requester,
            payload={
                "channel_id": channel_id,
                "requester": requester,
                "content": brief,
                "sender_role": sender_role,
            },
            delegation_depth=delegation_depth + 1,
            delegation_chain=chain,
            parent_turn_id=parent_turn_id,
        )
        await self._queue.publish(agent_inbox_topic(target), wake_event)

        logger.info(
            "channel_requested",
            channel_id=channel_id,
            requester=requester,
            target=target,
            participants=participants,
        )
        return channel_id

    async def reply(
        self,
        channel_id: str,
        sender: str,
        content: str,
        sender_role: str = "",
        *,
        delegation_depth: int = 0,
        delegation_chain: list[str] | None = None,
        parent_turn_id: str = "",
    ) -> None:
        """Answer on a channel and wake the other party.

        The reply carries the ask's delegation depth UNCHANGED. The ask
        is the delegation; the answer is that same hop completing, and
        the cap (default 3) exists to bound how deep a chain of *asks*
        goes. Charging the return leg as well halves the budget in the
        one direction nobody meant to spend it: a scheduled 1:1 costs
        depth 1 to ask and 2 to answer, so the report's first follow-up
        question lands at 3 and the manager's turn dies on a
        ``guard_breach`` — a legitimate second exchange ending as an
        engine failure event. Counting only asks bounds the
        conversation at ``delegation_depth_limit`` exchanges, which is
        what the limit reads as.

        The CHAIN still grows on every hop: it is provenance, not a
        gate, and "alice → bob → alice" is exactly what happened.

        Raises:
            ValueError: the channel is unknown, already closed, or the
                sender is not a participant. All three used to be one
                silent drop; a reply that goes nowhere is the failure
                the requester experiences as "they never answered".
        """
        channel = await self._channels.get(channel_id)
        if channel is None:
            logger.error(
                "send_on_unknown_channel", channel_id=channel_id, sender=sender
            )
            raise ValueError(f"A2A channel '{channel_id}' does not exist")
        if not channel.is_open:
            logger.error("send_on_closed_channel", channel_id=channel_id, sender=sender)
            raise ValueError(f"A2A channel '{channel_id}' is closed")
        recipient = channel.other_party(sender)
        if not recipient:
            logger.error(
                "send_by_non_participant",
                channel_id=channel_id,
                sender=sender,
                participants=channel.participants,
            )
            raise ValueError(
                f"Sender '{sender}' is not a participant of channel '{channel_id}'"
            )

        await self._channels.count_message(channel_id)
        await self._publish_sent(channel_id, sender, recipient, content, sender_role)

        chain = list(delegation_chain or [])
        if sender and sender not in chain:
            chain = [*chain, sender]
        wake_event = Event(
            type="a2a_message",
            source=sender,
            payload={
                "channel_id": channel_id,
                "sender": sender,
                "content": content,
                "sender_role": sender_role,
            },
            delegation_depth=delegation_depth,
            delegation_chain=chain,
            parent_turn_id=parent_turn_id,
        )
        await self._queue.publish(agent_inbox_topic(recipient), wake_event)

        logger.info(
            "message_sent",
            channel_id=channel_id,
            sender=sender,
            sender_role=sender_role,
            recipient=recipient,
            content_length=len(content),
        )

    async def _publish_sent(
        self,
        channel_id: str,
        sender: str,
        recipient: str,
        content: str,
        sender_role: str,
    ) -> None:
        msg = ChannelMessage(
            channel=channel_id,
            sender=sender,
            sender_role=sender_role,
            content=content,
        )
        await self._queue.publish(
            "crewlet.events.a2a_message_sent",
            A2AMessageSent(
                source=sender,
                channel_id=channel_id,
                sender=sender,
                sender_role=sender_role,
                recipient=recipient,
                message_id=str(msg.id),
                content=content,
            ),
        )

    async def close_channel(self, channel_id: str, closer: str = "") -> None:
        """Close a channel and record it.

        Args:
            channel_id: The channel to close.
            closer: Handle of the agent requesting the close. When
                provided, must be a participant of the channel.

        Raises:
            ValueError: If *closer* is provided and is not a
                participant of the channel.
        """
        channel = await self._channels.get(channel_id)
        if closer and channel is not None and not channel.other_party(closer):
            logger.error(
                "close_by_non_participant",
                channel_id=channel_id,
                closer=closer,
                participants=channel.participants,
            )
            raise ValueError(
                f"'{closer}' is not a participant of channel '{channel_id}'"
            )

        participants = channel.participants if channel else []
        message_count = channel.message_count if channel else 0
        if not await self._channels.close(channel_id):
            # Already closed, or never existed. Closing is idempotent —
            # both sides may reasonably try — so this is not an error,
            # but it must not publish a second closed event either.
            logger.debug("channel_close_noop", channel_id=channel_id)
            return

        closed_event = A2AChannelClosed(
            source=closer or "system",
            channel_id=channel_id,
            closed_by=closer,
            participants=participants,
            message_count=message_count,
            duration_ms=0.0,
        )
        await self._queue.publish("crewlet.events.a2a_channel_closed", closed_event)

        logger.info(
            "channel_closed",
            channel_id=channel_id,
            closed_by=closer or "system",
            participants=participants,
            message_count=message_count,
        )

    async def close_idle_channels(self, older_than_seconds: float) -> int:
        """Close channels nothing has finished. Returns how many.

        A channel is closed by the answering turn, so the ones that
        reach here are the turns that never got to finish: a crash, or a
        node that died between the wake and the answer. Left open they
        are a row that never goes away and a promise to the requester
        that a reply may still arrive.
        """
        closed = await self._channels.close_idle(older_than_seconds)
        for channel_id in closed:
            with_event = A2AChannelClosed(
                source="system",
                channel_id=channel_id,
                closed_by="",
                participants=[],
                message_count=0,
                duration_ms=0.0,
            )
            await self._queue.publish("crewlet.events.a2a_channel_closed", with_event)
        if closed:
            logger.info("a2a_channels_closed_idle", count=len(closed))
        return len(closed)
