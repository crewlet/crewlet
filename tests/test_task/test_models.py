"""Tests for task models."""

from datetime import UTC, datetime
from uuid import UUID

from crewlet.task.models import Priority, Task, TaskResult, TaskStatus


def test_task_defaults():
    task = Task(title="Test")
    assert isinstance(task.id, UUID)
    assert task.title == "Test"
    assert task.status == TaskStatus.CREATED
    assert task.priority == Priority.MEDIUM
    assert task.creator == ""
    assert task.assignee == ""
    assert not task.is_terminal()


def test_task_terminal_states():
    task = Task(title="A", status=TaskStatus.COMPLETED)
    assert task.is_terminal()

    task2 = Task(title="B", status=TaskStatus.FAILED)
    assert task2.is_terminal()


def test_task_non_terminal_states():
    for status in [
        TaskStatus.CREATED,
        TaskStatus.QUEUED,
        TaskStatus.ASSIGNED,
        TaskStatus.IN_PROGRESS,
    ]:
        task = Task(title="T", status=status)
        assert not task.is_terminal()


def test_task_result():
    result = TaskResult(output="Done", artifacts={"code": "hello.py"})
    assert result.output == "Done"
    assert result.artifacts["code"] == "hello.py"


def test_task_serialization():
    task = Task(
        title="Build API",
        description="Design REST endpoints",
        priority=Priority.HIGH,
        target_role="Engineer",
    )
    data = task.model_dump()
    restored = Task.model_validate(data)
    assert restored.title == task.title
    assert restored.id == task.id


def test_task_id_str():
    task = Task(title="T")
    assert isinstance(task.id_str, str)
    assert len(task.id_str) > 0


def test_task_deadline():
    dl = datetime(2026, 6, 1, tzinfo=UTC)
    task = Task(title="Deadline task", deadline=dl)
    assert task.deadline == dl


def test_task_deadline_default_none():
    task = Task(title="No deadline")
    assert task.deadline is None
