"""Agent working memory — per-agent context across turns."""

from __future__ import annotations

from pydantic import BaseModel


class MemoryEntry(BaseModel):
    """A single entry in agent memory."""

    role: str  # "user", "assistant", "system", "tool"
    content: str


class AgentMemory:
    """Working memory for an agent instance.

    Stores recent interactions to provide context across turns.
    Oldest entries are dropped when max_size is exceeded.
    """

    def __init__(self, max_size: int = 50) -> None:
        self.max_size = max_size
        self._entries: list[MemoryEntry] = []

    @property
    def entries(self) -> list[MemoryEntry]:
        return list(self._entries)

    def add(self, role: str, content: str) -> None:
        """Add an entry to memory."""
        self._entries.append(MemoryEntry(role=role, content=content))
        if len(self._entries) > self.max_size:
            self._entries = self._entries[-self.max_size :]

    def get_recent(self, n: int = 10) -> list[MemoryEntry]:
        """Get the most recent n entries."""
        return self._entries[-n:]

    def clear(self) -> None:
        """Clear all memory."""
        self._entries.clear()

    def to_messages(self) -> list[dict[str, str]]:
        """Convert memory to a list of message dicts for LLM context."""
        return [{"role": e.role, "content": e.content} for e in self._entries]
