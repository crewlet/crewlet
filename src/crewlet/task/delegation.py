"""Delegation handler — subtask creation through the org hierarchy."""

from __future__ import annotations

from crewlet._logging import get_logger
from crewlet.agent.pool import AgentPool
from crewlet.events.types import TaskCompleted, TaskDelegated
from crewlet.org.models import Organization
from crewlet.queue.protocol import EventQueue
from crewlet.task.models import Task, TaskResult, TaskStatus

logger = get_logger("task.delegation")


class DelegationHandler:
    """Handles task delegation through the org hierarchy.

    Creates subtasks that the team lead agent assigns to specific
    team members based on their skills and expertise.
    """

    def __init__(
        self,
        org: Organization,
        agent_pool: AgentPool,
        event_queue: EventQueue,
    ) -> None:
        self._org = org
        self._pool = agent_pool
        self._queue = event_queue

    async def delegate(
        self,
        parent_task: Task,
        subtask: Task,
        tasks: dict[str, Task],
    ) -> bool:
        """Create a subtask delegated from a parent task.

        The subtask is queued for the team lead to assign to a
        specific team member. Returns True if delegation succeeded.
        """
        logger.info(
            "subtask_delegating",
            title=subtask.title,
            parent_task_id=parent_task.id_str,
            target_role=subtask.target_role,
        )
        if parent_task.is_terminal():
            logger.warning(
                "delegation_failed_terminal",
                parent_task_id=parent_task.id_str,
                status=parent_task.status,
            )
            return False

        logger.debug(
            "task_linking_parent_child",
            parent_task_id=parent_task.id_str,
            child_task_id=subtask.id_str,
        )
        subtask.parent_task_id = parent_task.id_str
        parent_task.children_ids.append(subtask.id_str)
        parent_task.status = TaskStatus.DELEGATED

        tasks[subtask.id_str] = subtask
        subtask.status = TaskStatus.QUEUED

        await self._queue.publish(
            "crewlet.events.task_delegated",
            TaskDelegated(
                source="delegation",
                parent_task_id=parent_task.id_str,
                child_task_id=subtask.id_str,
                target_role=subtask.target_role,
            ),
        )

        return True

    async def check_parent_completion(
        self, parent_task: Task, tasks: dict[str, Task]
    ) -> bool:
        """Check if all children are completed, and if so, complete parent.

        Returns True if parent was completed.
        """
        if not parent_task.children_ids:
            return False

        logger.debug(
            "parent_completion_checking",
            parent_task_id=parent_task.id_str,
            children_count=len(parent_task.children_ids),
        )

        child_results: list[str] = []
        for cid in parent_task.children_ids:
            child = tasks.get(cid)
            if child is None or child.status != TaskStatus.COMPLETED:
                logger.debug(
                    "parent_incomplete_child",
                    parent_task_id=parent_task.id_str,
                    child_task_id=cid,
                    status=child.status if child else "NOT_FOUND",
                )
                return False
            if child.result:
                child_results.append(child.result.output)

        combined = (
            "; ".join(child_results) if child_results else "All subtasks completed"
        )
        parent_task.result = TaskResult(output=combined)
        parent_task.status = TaskStatus.COMPLETED

        logger.info(
            "parent_task_completed",
            parent_task_id=parent_task.id_str,
            children_count=len(parent_task.children_ids),
        )

        await self._queue.publish(
            "crewlet.events.task_completed",
            TaskCompleted(
                # No ``role``, deliberately. This is the task engine
                # rolling a parent up from its children, not a seat
                # finishing a turn — ``assignee`` is an agent id, and
                # naming a role here would flip that agent to idle on
                # the dashboard while its own turn is still running.
                source="delegation",
                task_id=parent_task.id_str,
                agent_id=parent_task.assignee,
                result=combined,
            ),
        )
        return True
