"""HandleRegistry — maps seat handles to parties and external identities.

Every org seat gets a deterministic handle derived from its role name
(e.g. ``"senior-engineer"``, ``"sarah-chen"``). The handle is the
canonical identity key used for:

- Email plus-addressing: ``notif+{handle}@company.com`` (agent seats)
- Slack user mapping: handle → bot_user_id (agents) / member ID (humans)
- Webhook path routing: ``/webhooks/{source}/{handle}``

Agent seats resolve to live :class:`AgentInstance` objects from the
pool; human seats resolve to their :class:`~crewlet.org.models.Role`
straight from the live ``Organization`` (no index — org walks are
cheap at org scale and stay correct under in-place hot reloads).
The party-level API (:meth:`HandleRegistry.resolve_party` and
friends) returns a :class:`ResolvedParty` covering both kinds;
the agent-only methods keep their narrow signatures for callers
that route to inboxes.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from typing import TYPE_CHECKING
from uuid import UUID

from crewlet._logging import get_logger
from crewlet.org.models import HANDLE_RE, Role, RoleKind
from crewlet.queue.topics import agent_inbox_topic

if TYPE_CHECKING:
    from crewlet.agent.instance import AgentInstance
    from crewlet.agent.pool import AgentPool
    from crewlet.org.models import Organization

logger = get_logger("notifications.handle")

#: Companion namespaces to consult when a transport's own namespace does
#: not resolve an external id.
#:
#: Chat backends register an agent's identity under TWO keys, because the
#: two halves of the system see different identifiers: inbound payloads
#: name a poster by *bot/user id*, while humans and MCP tools address the
#: same seat by *member id* or *username*.  Registering both and falling
#: back here is what lets a fellow agent's message be annotated the same
#: way a human's is.
#:
#: A per-backend list rather than a hardcoded ``if transport == "slack"``:
#: every chat backend needs the same split, and a second ``if`` is how
#: this silently stops covering the third one.
_BOT_ID_NAMESPACES: dict[str, tuple[str, ...]] = {
    "slack": ("slack_bot",),
    "mattermost": ("mattermost_bot",),
}

_VALID_HANDLE_RE = HANDLE_RE


@dataclass(frozen=True)
class ResolvedParty:
    """A resolved org-chart party — an agent seat or a human seat.

    ``role`` is the org seat; it is ``None`` only when the registry has
    no org reference at all (pool-only construction, e.g. in tests).

    ``agent`` is the live :class:`AgentInstance` — and it is ``None``
    for two different reasons, which callers must not conflate:

    - the party is a **human seat**, which never has one; or
    - the party is an **agent seat that is not running in this
      process**.

    The second case is the important one, and it is why resolution is
    org-derived rather than pool-derived. Routing an event to a seat
    needs only its handle and its agent id, both of which every process
    can derive from the org. Needing the *instance* to route made
    delivery depend on where the agent happened to be running — which is
    correct only while every agent runs everywhere.
    """

    kind: RoleKind
    handle: str
    name: str
    role: Role | None
    agent: AgentInstance | None
    org_name: str = ""

    @property
    def is_human(self) -> bool:
        return self.kind == RoleKind.HUMAN

    @property
    def is_local(self) -> bool:
        """Whether this seat is running in THIS process.

        The execution question, kept separate from the routing one. A
        caller that wants to *run* something needs this; a caller that
        wants to *deliver* something does not.
        """
        return self.agent is not None

    @property
    def agent_id(self) -> UUID | None:
        """The seat's stable agent id, derived rather than looked up.

        ``derive_agent_id`` is a ``uuid5`` over ``(org name, handle)``,
        so every process computes the same value for the same seat with
        no database and no live instance. That is what lets an event be
        addressed to a seat this process is not running.
        """
        if self.kind == RoleKind.HUMAN or not self.handle:
            return None
        if self.agent is not None:
            return self.agent.id
        if not self.org_name:
            return None
        from crewlet.db.agents import derive_agent_id

        return derive_agent_id(self.org_name, self.handle)

    @property
    def inbox_topic(self) -> str:
        """The seat's inbox subject. Empty for human seats, which have none."""
        if self.kind == RoleKind.HUMAN:
            return ""
        return agent_inbox_topic(self.handle)


class HandleRegistry:
    """Central registry mapping handles to agents and external identities.

    Built automatically from the agent pool during engine start.
    Uses dict-based indexes for O(1) lookups by handle and email.
    External identities (Slack bot user IDs, etc.) are registered
    at runtime by transports.
    """

    def __init__(
        self,
        agent_pool: AgentPool,
        org_provider: Callable[[], Organization | None] | None = None,
    ) -> None:
        self._pool = agent_pool
        # Live org accessor for human-seat resolution.  A callable
        # (not a snapshot) so hot reloads that swap ``engine.org``
        # are picked up without re-wiring the registry.
        self._org_provider = org_provider
        # External identity mappings: {transport_name: {external_id: handle}}
        self._external_to_handle: dict[str, dict[str, str]] = {}
        # Reverse: {transport_name: {handle: external_id}}
        self._handle_to_external: dict[str, dict[str, str]] = {}
        # Indexes (rebuilt on demand)
        self._handle_index: dict[str, AgentInstance] = {}
        self._email_index: dict[str, AgentInstance] = {}
        self._pool_version: int = -1
        # Provenance for human-contact reconciliation: the
        # (transport, external_id) → handle pairs the last
        # ``register_human_contacts_from_org`` run owned.  Lets the
        # reconcile remove exactly its own stale pairs without ever
        # touching agent identities.
        self.human_registered_pairs: dict[tuple[str, str], str] = {}

    def _rebuild_indexes(self) -> None:
        """Rebuild handle and email indexes from the agent pool.

        Only rebuilds if the pool has changed since the last rebuild.
        Uses the pool's monotonic version counter for O(1) staleness
        detection that correctly handles agent replacements.
        """
        from crewlet.agent.instance import AgentState

        if self._pool.version == self._pool_version:
            return

        self._handle_index.clear()
        self._email_index.clear()
        for agent in self._pool.agents:
            if agent.state == AgentState.TERMINATED:
                continue
            if agent.handle:
                self._handle_index[agent.handle] = agent
            if agent.email:
                self._email_index[agent.email.lower()] = agent
        self._pool_version = self._pool.version

    def resolve_handle(self, handle: str) -> AgentInstance | None:
        """Look up the agent instance for a given handle."""
        self._rebuild_indexes()
        return self._handle_index.get(handle)

    def resolve_email_address(self, email: str) -> AgentInstance | None:
        """Resolve a plus-addressed email to an agent.

        Parses ``notif+{handle}@domain`` and looks up the handle.
        Falls back to direct email lookup if not plus-addressed.
        """
        handle = self.parse_plus_address(email)
        if handle:
            agent = self.resolve_handle(handle)
            if agent is not None:
                return agent
        # Fallback: try direct email match
        self._rebuild_indexes()
        return self._email_index.get(email.lower())

    @staticmethod
    def parse_plus_address(email: str) -> str:
        """Extract handle from a plus-addressed email.

        ``notif+engineer@company.com`` → ``"engineer"``
        ``alice@company.com`` → ``""``
        """
        if not email or "@" not in email:
            return ""
        local_part = email.split("@")[0]
        if "+" in local_part:
            return local_part.split("+", 1)[1].lower()
        return ""

    @staticmethod
    def validate_handle(handle: str) -> bool:
        """Check if a handle is valid (lowercase alphanumeric + hyphens)."""
        return bool(_VALID_HANDLE_RE.match(handle))

    def agent_email(self, handle: str, domain: str, prefix: str = "notif") -> str:
        """Generate the plus-addressed email for an agent handle.

        ``agent_email("engineer", "company.com")``
        → ``"notif+engineer@company.com"``

        Raises ``ValueError`` if the handle contains invalid characters.
        """
        if not self.validate_handle(handle):
            raise ValueError(
                f"Invalid handle '{handle}': must match [a-z0-9][a-z0-9-]*"
            )
        return f"{prefix}+{handle}@{domain}"

    def register_external_id(
        self,
        transport_name: str,
        external_id: str,
        handle: str,
    ) -> None:
        """Map an external identity to an agent handle.

        E.g. a Slack bot user ID → agent handle.

        Raises ``ValueError`` if the handle contains invalid characters.
        """
        if not self.validate_handle(handle):
            raise ValueError(
                f"Invalid handle '{handle}': must match [a-z0-9][a-z0-9-]*"
            )
        if transport_name not in self._external_to_handle:
            self._external_to_handle[transport_name] = {}
            self._handle_to_external[transport_name] = {}

        self._external_to_handle[transport_name][external_id] = handle
        self._handle_to_external[transport_name][handle] = external_id
        logger.debug(
            "external_id_registered",
            external_id=external_id,
            transport=transport_name,
            handle=handle,
        )

    def unregister_external_id(
        self, transport_name: str, external_id: str, *, expected: str = ""
    ) -> bool:
        """Remove an external-identity mapping.

        With ``expected`` set, the mapping is only removed when it
        still points at that handle — protects against clearing a
        mapping someone else has since claimed.  The reverse
        (handle → external) entry is cleared only when it points at
        the removed ID.  Returns ``True`` when a mapping was removed.
        """
        mapping = self._external_to_handle.get(transport_name)
        if mapping is None:
            return False
        handle = mapping.get(external_id, "")
        if not handle or (expected and handle != expected):
            return False
        del mapping[external_id]
        reverse = self._handle_to_external.get(transport_name, {})
        if reverse.get(handle) == external_id:
            del reverse[handle]
        logger.debug(
            "external_id_unregistered",
            external_id=external_id,
            transport=transport_name,
            handle=handle,
        )
        return True

    def resolve_external_id(
        self, transport_name: str, external_id: str
    ) -> AgentInstance | None:
        """Resolve an external identity to an agent.

        Used by transports to map e.g. a Slack bot_user_id to the
        correct agent.
        """
        mapping = self._external_to_handle.get(transport_name, {})
        handle = mapping.get(external_id, "")
        if handle:
            return self.resolve_handle(handle)
        return None

    def get_external_id(self, transport_name: str, handle: str) -> str:
        """Get the external ID for an agent on a specific transport.

        Used for outbound: look up which Slack bot user to post as.
        """
        mapping = self._handle_to_external.get(transport_name, {})
        return mapping.get(handle, "")

    def resolve_role(self, role_name: str) -> AgentInstance | None:
        """Resolve a role name to the corresponding agent instance."""
        from crewlet.agent.instance import AgentState

        for agent in self._pool.agents:
            if agent.role_name == role_name and agent.state != AgentState.TERMINATED:
                return agent
        return None

    def all_transports(self) -> list[str]:
        """List all transport names that have registered external IDs."""
        return sorted(self._external_to_handle.keys())

    def known_external_ids(self, transport_name: str) -> set[str]:
        """External IDs registered for a transport (agents ∪ human seats).

        Both agent identity registration and human-contact reconcile
        write into the same ``transport → external_id → handle`` map, so
        this is every party the engine can route *transport* events to.
        The GitLab webhook parser intersects note ``@mentions`` against
        this set so a comment mentioning outsiders never fans out.
        """
        return set(self._external_to_handle.get(transport_name, {}).keys())

    def all_handles(self) -> list[str]:
        """List all registered handles from active agents."""
        self._rebuild_indexes()
        return list(self._handle_index.keys())

    # ------------------------------------------------------------- #
    # Party-level resolution (agents ∪ human seats)
    # ------------------------------------------------------------- #

    def _org(self) -> Organization | None:
        if self._org_provider is None:
            return None
        try:
            return self._org_provider()
        except Exception:
            logger.exception("org_provider_failed")
            return None

    def human_seats(self) -> list[Role]:
        """All human seats in the live org (empty without an org ref)."""
        org = self._org()
        if org is None:
            return []
        return [r for r in org.all_roles() if r.kind == RoleKind.HUMAN]

    def agent_seats(self) -> list[Role]:
        """All agent seats in the live org (empty without an org ref)."""
        org = self._org()
        if org is None:
            return []
        return [r for r in org.all_roles() if r.kind == RoleKind.AGENT]

    def _org_name(self) -> str:
        org = self._org()
        return org.name if org is not None else ""

    def _party_from_agent(self, agent: AgentInstance) -> ResolvedParty:
        org = self._org()
        role = org.get_role(agent.role_name) if org is not None else None
        return ResolvedParty(
            kind=RoleKind.AGENT,
            handle=agent.handle,
            name=agent.role_name,
            role=role,
            agent=agent,
            org_name=self._org_name(),
        )

    def _party_from_agent_seat(self, seat: Role) -> ResolvedParty:
        """An agent seat that is NOT running in this process.

        Addressable but not runnable: it has a handle, a role and a
        derivable agent id, and no instance. This is the party a fleet
        member resolves for a seat another node owns — and, today, for a
        seat that simply has not been spawned yet.
        """
        return ResolvedParty(
            kind=RoleKind.AGENT,
            handle=seat.get_handle(),
            name=seat.name,
            role=seat,
            agent=None,
            org_name=self._org_name(),
        )

    def _party_from_human(self, seat: Role) -> ResolvedParty:
        return ResolvedParty(
            kind=RoleKind.HUMAN,
            handle=seat.get_handle(),
            name=seat.name,
            role=seat,
            agent=None,
            org_name=self._org_name(),
        )

    def resolve_party(self, handle: str) -> ResolvedParty | None:
        """Resolve a handle to a party.

        Local agent instances first (so callers that can run the seat
        get it), then agent seats from the org, then human seats. The
        middle tier is what makes resolution independent of where an
        agent happens to be running.
        """
        agent = self.resolve_handle(handle)
        if agent is not None:
            return self._party_from_agent(agent)
        for seat in self.agent_seats():
            if seat.get_handle() == handle:
                return self._party_from_agent_seat(seat)
        for seat in self.human_seats():
            if seat.get_handle() == handle:
                return self._party_from_human(seat)
        return None

    def resolve_party_role_name(self, role_name: str) -> ResolvedParty | None:
        """Resolve an exact role name to a party."""
        agent = self.resolve_role(role_name)
        if agent is not None:
            return self._party_from_agent(agent)
        for seat in self.agent_seats():
            if seat.name == role_name:
                return self._party_from_agent_seat(seat)
        for seat in self.human_seats():
            if seat.name == role_name:
                return self._party_from_human(seat)
        return None

    def resolve_party_agent_id(self, agent_id: str) -> ResolvedParty | None:
        """Resolve an agent id to a party.

        The inverse of :func:`~crewlet.db.agents.derive_agent_id`: that
        is a ``uuid5`` over ``(org name, handle)``, so every process can
        recompute the id of every agent seat and answer this with no
        database and no live instance.  Internal events carry an agent
        id rather than a handle (``TaskAssigned``, ``ExternalNotification``),
        and looking that id up in the local pool made routing depend on
        where the seat happened to be running.

        Human seats have no agent id, so they never match here — a
        caller that needs to distinguish "human recipient" from
        "unknown recipient" has to ask the org by role or handle.
        """
        if not agent_id:
            return None
        from crewlet.agent.instance import AgentState

        wanted = str(agent_id)
        for agent in self._pool.agents:
            if agent.id_str == wanted and agent.state != AgentState.TERMINATED:
                return self._party_from_agent(agent)
        org = self._org()
        if org is None:
            return None
        seat = org.agent_seat_by_id(wanted)
        return self._party_from_agent_seat(seat) if seat is not None else None

    def resolve_party_email(self, email: str) -> ResolvedParty | None:
        """Resolve an email to a party.

        Agent seats match plus-addresses and their configured email;
        human seats match their real address.
        """
        agent = self.resolve_email_address(email)
        if agent is not None:
            return self._party_from_agent(agent)
        lowered = email.lower()
        # A plus-address names a seat directly, so it resolves even when
        # that seat is not running here.
        handle = self.parse_plus_address(email)
        if handle:
            for seat in self.agent_seats():
                if seat.get_handle() == handle:
                    return self._party_from_agent_seat(seat)
        for seat in self.agent_seats():
            if seat.email and seat.email.lower() == lowered:
                return self._party_from_agent_seat(seat)
        for seat in self.human_seats():
            if seat.email and seat.email.lower() == lowered:
                return self._party_from_human(seat)
        return None

    def resolve_party_external(
        self, transport_name: str, external_id: str
    ) -> ResolvedParty | None:
        """Resolve an external identity to a party.

        Pure dict lookups — the registry IS the maintained index for
        external IDs (agent IDs land via transports/MCP resolution at
        boot; human contact IDs via :func:`register_human_contacts_from_org`
        at boot and on every org swap).  This runs on the inbound
        webhook hot path (sender attribution per changelog line), so
        no org walks here.

        Chat identity spans two namespaces per backend: human member IDs
        under the transport's own name, agent *bot* IDs under its
        ``_bot`` companion (registered by each transport's identity
        resolution at start).  Both are consulted so a fellow agent's
        message annotates the same way a human's does — see
        :data:`_BOT_ID_NAMESPACES`.
        """
        if not external_id:
            return None
        handle = self._external_to_handle.get(transport_name, {}).get(external_id, "")
        if not handle:
            for alias in _BOT_ID_NAMESPACES.get(transport_name, ()):
                handle = self._external_to_handle.get(alias, {}).get(external_id, "")
                if handle:
                    break
        if handle:
            return self.resolve_party(handle)
        return None

    def all_parties(self) -> list[ResolvedParty]:
        """Every addressable party — every ORG seat, agent and human.

        The one enumeration callers should build corpora from (e.g. the
        ``lookup_colleague`` fuzzy tiers), so the party mapping lives
        here rather than being re-derived at call sites.

        Enumerated from the **org**, with live instances attached where
        this process has them. Enumerating the pool instead would shrink
        an agent's view of its own company to whatever happens to be
        running locally — it would stop seeing colleagues that exist,
        and a fuzzy match could then land on the wrong one because the
        right one was missing from the corpus.

        Falls back to the pool when there is no org reference at all
        (pool-only construction in tests).
        """
        seats = self.agent_seats()
        if not seats and self._org() is None:
            parties = [
                self._party_from_agent(agent)
                for handle in self.all_handles()
                if (agent := self.resolve_handle(handle)) is not None
            ]
            parties.extend(self._party_from_human(seat) for seat in self.human_seats())
            return parties

        parties: list[ResolvedParty] = []
        for seat in seats:
            handle = seat.get_handle()
            agent = self.resolve_handle(handle) if handle else None
            parties.append(
                self._party_from_agent(agent)
                if agent is not None
                else self._party_from_agent_seat(seat)
            )
        parties.extend(self._party_from_human(seat) for seat in self.human_seats())
        return parties


def register_human_contacts_from_org(
    registry: HandleRegistry, org: Organization
) -> int:
    """Reconcile human seats' declared contact IDs into the registry.

    Unlike the agent paths (``register_jira_accounts_from_org`` /
    ``register_github_accounts_from_org``, which resolve identities
    through per-role MCP servers), human IDs are declared directly in
    config — registration is synchronous and needs no integrations.
    Declared ``${VAR}`` references are env-resolved here (via
    :meth:`~crewlet.org.models.HumanContact.resolved_identities`); an
    unresolved reference is simply not registered on this run, and the
    next reconcile (engine start / org swap) picks it up once the
    variable is exported.

    This is a **reconcile**, not an append: pairs this function
    registered on a previous run that are no longer declared (contact
    edits, seat removals, human → agent kind flips) are unregistered
    first.  Without that, a corrected ``slack_user_id`` would keep
    attributing the old ID to the seat forever, and an ID that
    legitimately moved between seats would be blocked by the conflict
    guard until restart.

    An external ID already mapped to a different handle by someone
    *else* (an agent identity) is left alone (warned): a human must
    never silently take over an agent's mapping or vice versa.

    Returns the number of registered (transport, external_id) pairs.
    """
    # Desired state: every (transport, external_id) → handle for the
    # current org's human seats.
    desired: dict[tuple[str, str], str] = {}
    for seat in org.all_roles():
        if seat.kind != RoleKind.HUMAN or seat.contact is None:
            continue
        handle = seat.get_handle()
        for transport, external_id in seat.contact.resolved_identities():
            desired[(transport, external_id)] = handle

    # Drop pairs WE registered previously that are no longer desired
    # (or now belong to a different seat).  Only our own pairs — an
    # agent mapping that happens to collide is never touched.
    previous = registry.human_registered_pairs
    for pair, prev_handle in previous.items():
        if desired.get(pair) != prev_handle:
            registry.unregister_external_id(pair[0], pair[1], expected=prev_handle)

    registered: dict[tuple[str, str], str] = {}
    for (transport, external_id), handle in desired.items():
        existing = registry._external_to_handle.get(transport, {}).get(external_id, "")
        if existing and existing != handle:
            logger.warning(
                "human_contact_id_conflict",
                transport=transport,
                external_id=external_id,
                handle=handle,
                already_mapped_to=existing,
            )
            continue
        registry.register_external_id(transport, external_id, handle)
        registered[(transport, external_id)] = handle

    registry.human_registered_pairs = registered
    if registered:
        logger.info("human_contacts_registered", count=len(registered))
    return len(registered)
