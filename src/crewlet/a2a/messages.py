"""Message model for agent-to-agent communication."""

from __future__ import annotations

from datetime import UTC, datetime
from uuid import UUID, uuid4

from pydantic import BaseModel, Field


class ChannelMessage(BaseModel):
    """A message exchanged on an A2A channel."""

    id: UUID = Field(default_factory=uuid4)
    channel: str
    sender: str
    sender_role: str = ""
    content: str
    timestamp: datetime = Field(default_factory=lambda: datetime.now(UTC))
