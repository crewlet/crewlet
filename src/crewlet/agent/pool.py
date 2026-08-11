"""AgentPool — registry of all agent instances for an organization."""

from __future__ import annotations

from crewlet._logging import get_logger
from crewlet.agent.definition import AgentDefinition
from crewlet.agent.instance import AgentInstance, AgentState
from crewlet.db.agents import derive_agent_id
from crewlet.events.types import AgentSpawned, AgentTerminated
from crewlet.org.models import Organization, Role, RoleKind
from crewlet.queue.protocol import EventQueue

logger = get_logger("agent.pool")


class AgentPool:
    """Registry of all agent instances in the organization.

    Each agent seat (``Role.kind == "agent"``) maps 1:1 to an
    AgentInstance. Human seats are never spawned — they live only in
    the ``Organization`` and resolve through the party-level
    ``HandleRegistry`` API. The pool provides lookup by ID, email, or
    handle — not role-based routing. Task assignment is a team lead
    decision, not a pool concern.
    """

    def __init__(
        self,
        event_queue: EventQueue,
    ) -> None:
        self._event_queue = event_queue
        self._agents: list[AgentInstance] = []
        self._definitions: dict[str, AgentDefinition] = {}
        self._version: int = 0
        self._org_name: str = ""

    @property
    def agents(self) -> list[AgentInstance]:
        return list(self._agents)

    @property
    def version(self) -> int:
        """Monotonically increasing counter, bumped on any pool mutation."""
        return self._version

    @property
    def active_agents(self) -> list[AgentInstance]:
        return [a for a in self._agents if a.state != AgentState.TERMINATED]

    async def spawn_role(
        self, role: Role, org: Organization, *, source: str = "pool"
    ) -> AgentInstance:
        """Spawn one agent instance for ``role`` and publish ``AgentSpawned``.

        Single-role analogue of :meth:`spawn_from_org`.  The engine's
        live add-branch (``apply_config`` adding a role on a running
        instance) calls this so the AgentDefinition / AgentInstance
        construction + activate + publish stay in one canonical place
        instead of being copy-pasted in :meth:`Engine._apply_org_diff`.
        ``source`` is forwarded to the ``AgentSpawned`` event so the
        audit trail distinguishes boot-time spawns from live additions.

        Raises ``ValueError`` for human seats — callers must filter on
        ``role.kind`` (a silently-skipped spawn would hide a routing
        bug; an accidentally-spawned human would shadow the seat with
        a zombie agent).
        """
        if role.kind == RoleKind.HUMAN:
            raise ValueError(
                f"cannot spawn an agent for human seat '{role.name}' — "
                f"human seats are addressable only"
            )
        definition = AgentDefinition(
            role=role,
            org=org,
        )
        self._definitions[role.name] = definition

        handle = role.get_handle()
        agent = AgentInstance(
            definition,
            email=role.email,
            handle=handle,
            id=derive_agent_id(org.name, handle),
        )
        agent.activate()
        self._agents.append(agent)
        self._version += 1
        logger.debug("agent_spawned", agent_name=role.name, agent_id=agent.id_str)

        await self._event_queue.publish(
            "crewlet.events.agent_spawned",
            AgentSpawned(
                source=source,
                agent_id=agent.id_str,
                role=role.name,
            ),
        )
        return agent

    async def spawn_from_org(self, org: Organization) -> list[AgentInstance]:
        """Spawn one agent instance per *agent* seat in the organization.

        Human seats are skipped — they participate in the hierarchy
        but have no runtime instance.
        """
        all_roles = org.all_roles()
        agent_roles = [r for r in all_roles if r.kind == RoleKind.AGENT]
        logger.info(
            "spawning_agents",
            role_count=len(agent_roles),
            human_seats=len(all_roles) - len(agent_roles),
        )
        # Stash the org name so post-spawn paths (``handle_failure``,
        # hot-reload) can re-derive deterministic agent ids without
        # taking ``Organization`` as another constructor argument.
        self._org_name = org.name
        spawned = [await self.spawn_role(role, org) for role in agent_roles]
        logger.info("spawning_complete", count=len(spawned))
        return spawned

    def add_agent(self, agent: AgentInstance) -> None:
        """Add an externally created agent to the pool."""
        logger.debug("agent_added", agent_id=agent.id_str, agent_name=agent.role_name)
        self._agents.append(agent)
        self._version += 1

    def get_by_id(self, agent_id: str) -> AgentInstance | None:
        """Get an agent by its ID."""
        for agent in self._agents:
            if agent.id_str == agent_id:
                return agent
        logger.debug("get_by_id_miss", agent_id=agent_id)
        return None

    def get_by_email(self, email: str) -> AgentInstance | None:
        """Get an active agent by its email address."""
        for agent in self._agents:
            if (
                agent.email
                and agent.email.lower() == email.lower()
                and agent.state != AgentState.TERMINATED
            ):
                return agent
        logger.debug("get_by_email_miss", email=email)
        return None

    def get_by_handle(self, handle: str) -> AgentInstance | None:
        """Get an active agent by its handle."""
        for agent in self._agents:
            if (
                agent.handle
                and agent.handle == handle
                and agent.state != AgentState.TERMINATED
            ):
                return agent
        logger.debug("get_by_handle_miss", handle=handle)
        return None

    def get_all_for_role(self, role_name: str) -> list[AgentInstance]:
        """Get the agent for a given role (returns list for API consistency).

        With the 1:1 Role→Agent model, this returns at most one agent.
        """
        return [
            a
            for a in self._agents
            if a.role_name == role_name and a.state != AgentState.TERMINATED
        ]

    async def handle_failure(
        self, agent: AgentInstance, task_id: str, error: str
    ) -> str:
        """Handle agent failure — restart with fresh instance.

        Returns the replacement agent's ID.
        """
        logger.warning(
            "agent_failure",
            agent_id=agent.id_str,
            agent_name=agent.role_name,
            task_id=task_id,
            error=error,
        )
        # Terminate and remove the failed agent to avoid zombie duplicates
        agent.terminate()
        self._agents = [a for a in self._agents if a is not agent]

        # Create replacement from same definition, preserving identity
        # via the deterministic ``derive_agent_id`` -- restart-equivalent
        # reuse, not a fresh agent.  Personal memory + onboarding
        # markers (keyed by ``agent.id`` at AGENT scope) follow the
        # replacement.  Falls back to ``agent.id`` directly when the
        # pool was bootstrapped without ``spawn_from_org`` (e.g. tests
        # using ``add_agent``) so the path stays correct in either case.
        derived_id = (
            derive_agent_id(self._org_name, agent.handle)
            if self._org_name
            else agent.id
        )
        replacement = AgentInstance(
            agent.definition,
            email=agent.email,
            handle=agent.handle,
            id=derived_id,
        )
        replacement.activate()
        self._agents.append(replacement)
        self._version += 1

        logger.info(
            "agent_replaced",
            new_id=replacement.id_str,
            agent_name=replacement.role_name,
            replaced_id=agent.id_str,
        )

        await self._event_queue.publish(
            "crewlet.events.agent_spawned",
            AgentSpawned(
                source="pool.failure_recovery",
                agent_id=replacement.id_str,
                role=replacement.role_name,
            ),
        )

        return replacement.id_str

    async def terminate(self, agent: AgentInstance) -> None:
        """Terminate an agent instance."""
        logger.info(
            "agent_terminating",
            agent_id=agent.id_str,
            agent_name=agent.role_name,
        )
        agent.terminate()
        self._version += 1
        await self._event_queue.publish(
            "crewlet.events.agent_terminated",
            AgentTerminated(
                source="pool",
                agent_id=agent.id_str,
                role=agent.role_name,
                reason="terminated by pool",
            ),
        )
