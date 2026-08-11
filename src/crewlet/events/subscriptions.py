"""Event subscriptions — route task lifecycle events to agent inboxes.

Instead of calling execute_turn() directly, this module publishes events
to the target agent's inbox topic on the EventQueue.  The Engine's
per-agent handler (subscribed to that inbox topic) decides what to do.

Routing rules:
- TaskCreated     → team lead's inbox
- TaskAssigned    → assigned agent's inbox
- TaskCompleted   → manager's inbox
- TaskDelegated   → target agent's inbox
- ExternalNotification → resolved agent's inbox

These internal events route only to **agent** seats — they exist to
wake an agent into a turn, and a human has no turn to wake.  When a
resolved recipient is a human seat the event is skipped gracefully:
the human is already notified natively by the PM tool / Slack where
the work actually lives (a Jira assignment emails the assignee; a
Slack mention pings them).  The engine never sends as itself — agents
reach humans through their own colleague-surface tools with an
@-mention, called directly during a turn's Execute phase.

Handlers read the org through ``org_provider`` on every event so a
hot reload that swaps ``engine.org`` (including seat-kind flips) is
respected immediately.

Manager handoffs when an agent is stuck are NOT routed through this
module — they use the colleague-surface tools (Slack mention, Jira
comment, A2A) directly.

DACI decisions happen naturally in Slack — agents use their team's
Slack channel and MCP tools to discuss and decide.
"""

from __future__ import annotations

from collections.abc import Callable
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import (
    Event,
    ExternalNotification,
    TaskAssigned,
    TaskCompleted,
    TaskCreated,
    TaskDelegated,
)
from crewlet.org.hierarchy import get_manager
from crewlet.org.models import Organization
from crewlet.queue.protocol import EventQueue

logger = get_logger("events.subscriptions")


def _inbox_topic(agent: Any) -> str:
    """Return the inbox topic for an agent instance."""
    handle = agent.handle or agent.id_str
    return f"crewlet.agent.{handle}.inbox"


async def setup_subscriptions(
    queue: EventQueue,
    agent_pool: Any,
    org_provider: Callable[[], Organization],
) -> Callable[[], None]:
    """Set up event subscriptions that route to agent inbox topics.

    All handlers publish to the target agent's inbox topic on the
    EventQueue (or, for human seats, through the outbound notification
    pipeline).  The Engine's per-agent handler (subscribed to that
    inbox) processes the event.

    ``org_provider`` is called per event — never cache the org here,
    or hot reloads would route against a stale hierarchy.

    Returns a no-op cleanup callback (kept for API compatibility).
    """

    # ------------------------------------------------------------------ #
    # 1. TaskCreated → publish to team lead's inbox
    # ------------------------------------------------------------------ #
    async def _on_task_created(event: Event) -> None:
        if not isinstance(event, TaskCreated):
            return

        org = org_provider()
        target_role = event.target_role

        if not target_role:
            # No target role — route to every top-level AGENT manager.
            # Human top-level managers (e.g. a founder seat) are skipped:
            # a no-target task surfaces to them in the PM tool, not as an
            # engine push.
            for role in org.all_roles():
                if role.is_human or get_manager(role, org) is not None:
                    continue
                for agent in agent_pool.get_all_for_role(role.name):
                    logger.debug(
                        "task_created_to_top_manager",
                        task_id=event.task_id,
                        agent=agent.role_name,
                    )
                    await queue.publish(_inbox_topic(agent), event)
            return

        # Find the manager (team lead) for the target role
        role = org.get_role(target_role)
        if role is None:
            logger.warning(
                "task_created_unknown_role",
                task_id=event.task_id,
                target_role=target_role,
            )
            return

        manager_role = get_manager(role, org)
        # The lead that would triage is an agent: route to it.  When the
        # lead is a human (or there is none), fall through to the target
        # role's own agents — there is no agent turn to run for a human
        # lead, and the human sees the task in the PM tool.
        if manager_role is not None and not manager_role.is_human:
            manager_agents = agent_pool.get_all_for_role(manager_role.name)
            if manager_agents:
                for mgr in manager_agents:
                    logger.debug(
                        "task_created_to_lead",
                        task_id=event.task_id,
                        lead=mgr.role_name,
                    )
                    await queue.publish(_inbox_topic(mgr), event)
                return
            logger.warning(
                "task_created_no_agents_for_manager_role",
                task_id=event.task_id,
                manager_role=manager_role.name,
            )

        # No agent lead to triage (human lead, or none) — send directly
        # to the target role's own agents, if it has any.
        for agent in agent_pool.get_all_for_role(target_role):
            logger.debug(
                "task_created_to_self",
                task_id=event.task_id,
                agent=agent.role_name,
            )
            await queue.publish(_inbox_topic(agent), event)

    await queue.subscribe(
        "crewlet.events.task_created", "subscriptions", _on_task_created
    )
    logger.debug("subscription_registered", handler="task_created")

    # ------------------------------------------------------------------ #
    # 2. TaskAssigned → publish to assigned agent's inbox
    # ------------------------------------------------------------------ #
    async def _on_task_assigned(event: Event) -> None:
        if not isinstance(event, TaskAssigned):
            return

        agent = agent_pool.get_by_id(event.agent_id)
        if agent is None:
            # A human seat carries no agent id.  Human assignment is the
            # PM tool's job (and notifies them natively), so an event
            # naming a human is skipped quietly rather than warned.
            org = org_provider()
            seat = org.get_role(event.role) if event.role else None
            if seat is not None and seat.is_human:
                logger.debug(
                    "task_assigned_human_skipped",
                    task_id=event.task_id,
                    seat=seat.name,
                )
                return
            logger.warning(
                "task_assigned_agent_not_found",
                task_id=event.task_id,
                agent_id=event.agent_id,
            )
            return

        logger.debug(
            "task_assigned_to_inbox",
            task_id=event.task_id,
            agent=agent.role_name,
        )
        await queue.publish(_inbox_topic(agent), event)

    await queue.subscribe(
        "crewlet.events.task_assigned", "subscriptions", _on_task_assigned
    )
    logger.debug("subscription_registered", handler="task_assigned")

    # ------------------------------------------------------------------ #
    # 3. TaskCompleted → publish to manager's inbox
    # ------------------------------------------------------------------ #
    async def _on_task_completed(event: Event) -> None:
        if not isinstance(event, TaskCompleted):
            return

        # Resolve the manager to notify from the completing agent's role.
        org = org_provider()
        notified = False
        manager_is_human = False
        agent = agent_pool.get_by_id(event.agent_id)
        if agent is not None:
            role = org.get_role(agent.role_name)
            if role is not None:
                manager_role = get_manager(role, org)
                if manager_role is not None and manager_role.is_human:
                    # A human manager has no inbox; they see the agent's
                    # work natively on Slack/Jira.  Not unroutable —
                    # intentionally not pushed.
                    manager_is_human = True
                    logger.debug(
                        "task_completed_human_manager_skipped",
                        task_id=event.task_id,
                        manager=manager_role.name,
                    )
                elif manager_role is not None:
                    for mgr in agent_pool.get_all_for_role(manager_role.name):
                        logger.debug(
                            "task_completed_to_manager",
                            task_id=event.task_id,
                            manager=mgr.role_name,
                        )
                        await queue.publish(_inbox_topic(mgr), event)
                        notified = True

        if not notified and not manager_is_human and event.agent_id:
            logger.warning(
                "task_completed_unroutable",
                task_id=event.task_id,
                agent_id=event.agent_id,
            )

    await queue.subscribe(
        "crewlet.events.task_completed",
        "subscriptions",
        _on_task_completed,
    )
    logger.debug("subscription_registered", handler="task_completed")

    # ------------------------------------------------------------------ #
    # 5. TaskDelegated → publish to target agent's inbox
    # ------------------------------------------------------------------ #
    async def _on_task_delegated(event: Event) -> None:
        if not isinstance(event, TaskDelegated):
            return

        target_role = event.target_role
        if not target_role:
            return

        org = org_provider()
        seat = org.get_role(target_role)
        if seat is not None and seat.is_human:
            # Delegating to a human is done by the agent on a colleague
            # surface (Jira/Slack) with an @-mention, not by an engine
            # push — skip the internal routing event quietly.
            logger.debug(
                "task_delegated_human_skipped",
                child_task_id=event.child_task_id,
                seat=seat.name,
            )
            return

        agents = agent_pool.get_all_for_role(target_role)
        for agent in agents:
            logger.debug(
                "task_delegated_to_agent",
                child_task_id=event.child_task_id,
                agent=agent.role_name,
            )
            await queue.publish(_inbox_topic(agent), event)

    await queue.subscribe(
        "crewlet.events.task_delegated",
        "subscriptions",
        _on_task_delegated,
    )
    logger.debug("subscription_registered", handler="task_delegated")

    # ------------------------------------------------------------------ #
    # 6. ExternalNotification → publish to resolved agent's inbox
    # ------------------------------------------------------------------ #
    async def _on_external_notification(event: Event) -> None:
        if not isinstance(event, ExternalNotification):
            return

        agent = agent_pool.get_by_id(event.agent_id) if event.agent_id else None
        if agent is None:
            logger.warning(
                "external_notification_no_agent",
                agent_id=event.agent_id,
            )
            return

        logger.debug(
            "external_notification_to_inbox",
            agent=agent.role_name,
            source=event.notification_source,
        )
        await queue.publish(_inbox_topic(agent), event)

    await queue.subscribe(
        "crewlet.events.external_notification",
        "subscriptions",
        _on_external_notification,
    )
    logger.debug("subscription_registered", handler="external_notification")

    def _noop() -> None:
        pass

    return _noop
