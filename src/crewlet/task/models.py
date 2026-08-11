"""Task models — data structures for work items."""

from __future__ import annotations

from datetime import UTC, datetime
from enum import StrEnum
from typing import Any
from uuid import UUID, uuid4

from pydantic import BaseModel, Field


class TaskStatus(StrEnum):
    CREATED = "created"
    QUEUED = "queued"
    ASSIGNED = "assigned"
    IN_PROGRESS = "in_progress"
    REVIEW = "review"
    COMPLETED = "completed"
    FAILED = "failed"
    DELEGATED = "delegated"


class Priority(StrEnum):
    LOW = "low"
    MEDIUM = "medium"
    HIGH = "high"
    CRITICAL = "critical"


class TaskResult(BaseModel):
    """Result of a completed task."""

    output: str = ""
    artifacts: dict[str, Any] = Field(default_factory=dict)


class Task(BaseModel):
    """A work item in the system."""

    id: UUID = Field(default_factory=uuid4)
    title: str
    description: str = ""
    status: TaskStatus = TaskStatus.CREATED
    priority: Priority = Priority.MEDIUM
    creator: str = ""  # agent_id or "founder"
    assignee: str = ""  # agent_id
    target_role: str = ""
    parent_task_id: str = ""
    children_ids: list[str] = Field(default_factory=list)
    dependencies: list[str] = Field(default_factory=list)
    result: TaskResult | None = None
    deadline: datetime | None = None
    created_at: datetime = Field(default_factory=lambda: datetime.now(UTC))
    updated_at: datetime = Field(default_factory=lambda: datetime.now(UTC))

    @property
    def id_str(self) -> str:
        return str(self.id)

    def is_terminal(self) -> bool:
        """Whether the task is in a terminal state."""
        return self.status in (TaskStatus.COMPLETED, TaskStatus.FAILED)
