"""Notification protocol — models and interfaces for outbound transports."""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from pydantic import BaseModel, Field


class InboundNotification(BaseModel):
    """A notification received from an external source.

    This is the normalized representation of any external event:
    a Jira ticket assignment, a Slack message, a GitHub PR review
    request, an email, etc.
    """

    source: str
    """The source system (e.g. 'jira', 'slack', 'github', 'email')."""

    source_event_type: str = ""
    """The original event type from the source (e.g. 'issue_assigned',
    'message', 'pull_request_review_requested')."""

    recipient_email: str = ""
    """Email address of the intended recipient."""

    recipient_handle: str = ""
    """Handle of the intended recipient (parsed from plus-address or
    webhook path)."""

    sender: str = ""
    """Display name or identifier of the sender."""

    subject: str = ""
    """Subject line or title (email subject, ticket title, etc.)."""

    body: str = ""
    """Full content/body of the notification."""

    metadata: dict[str, str] = Field(default_factory=dict)
    """Extra source-specific metadata (ticket ID, PR number, etc.)."""


class OutboundMessage(BaseModel):
    """A message sent by an agent through an external transport.

    Represents the agent's reply or proactive outbound communication
    through a specific transport channel (email, Slack, etc.).
    """

    transport: str
    """Target transport name (e.g. 'email', 'slack')."""

    sender_handle: str
    """Handle of the sending agent."""

    recipient: str
    """Recipient address (email, Slack channel/user, etc.)."""

    subject: str = ""
    """Subject line (for email) or thread reference."""

    body: str = ""
    """Message content."""

    reply_to_metadata: dict[str, str] = Field(default_factory=dict)
    """Metadata from the original notification being replied to.
    Transports use this to thread replies correctly (e.g. email
    In-Reply-To, Slack thread_ts)."""

    metadata: dict[str, str] = Field(default_factory=dict)
    """Extra transport-specific options."""


@runtime_checkable
class Transport(Protocol):
    """Protocol for outbound notification transports.

    Transports handle sending messages to external systems on behalf
    of agents. Inbound notifications arrive via the EventQueue (published
    by API webhook handlers), not through transports directly.

    Every transport has a ``name`` used to route outbound messages
    (e.g. ``OutboundMessage(transport="email", ...)``).
    """

    name: str
    """Transport identifier, used to route outbound messages."""

    async def start(self) -> None:
        """Start the transport (initialize connections, clients, etc.)."""
        ...

    async def stop(self) -> None:
        """Stop and clean up resources."""
        ...

    async def send(self, message: OutboundMessage) -> bool:
        """Send a message through this transport.

        Returns True if the message was sent successfully.
        """
        ...
