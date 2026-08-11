"""Notification service — outbound transports + event-queue-driven routing."""

from crewlet.notifications.handle import (
    HandleRegistry,
    ResolvedParty,
    register_human_contacts_from_org,
)
from crewlet.notifications.protocol import (
    InboundNotification,
    OutboundMessage,
    Transport,
)
from crewlet.notifications.service import NotificationService

__all__ = [
    "HandleRegistry",
    "InboundNotification",
    "NotificationService",
    "OutboundMessage",
    "ResolvedParty",
    "Transport",
    "register_human_contacts_from_org",
]
