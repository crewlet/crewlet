"""Tool protocol and supporting models."""

from __future__ import annotations

from collections.abc import Callable
from typing import Any, Protocol, runtime_checkable

from pydantic import BaseModel, ConfigDict, Field


class ToolResult(BaseModel):
    """Result of a tool execution."""

    success: bool = True
    output: str = ""
    error: str | None = None

    suspend: bool = False
    """The tool kicked off long-running detached work whose result arrives
    later. The Execute tool-loop, on seeing this, leaves *this* tool call
    unanswered (resolving any sibling calls in the same assistant turn
    inline), persists the conversation, and ends the turn; the engine
    resumes the loop with the real result spliced in once the work
    completes — the detached sandbox tool. Set only by
    tools that participate in the suspend/resume protocol; ignored by the
    Plan / Review loops, which never persist a partial conversation."""
    suspend_payload: dict[str, Any] | None = None
    """Opaque correlation state the suspending tool needs on resume (e.g.
    ``turn_id`` / ``sandbox_id``). Persisted with the pending run."""


class AgentContext(BaseModel):
    """Context passed to tools during execution.

    Provides access to engine subsystems. Fields are Any to avoid
    circular imports — they are typed at runtime by the engine.
    """

    agent_id: str = ""
    agent_handle: str = ""
    role: str = ""
    current_task_id: str = ""
    org_id: str = ""
    unit_ids: list[str] = Field(default_factory=list)
    """Unit names from the role's containing unit to outermost ancestor."""
    org: Any = None
    event_queue: Any = None
    knowledge_searcher: Any = None
    """KnowledgeSearcher for the query-time knowledge-base search
    (Confluence CQL or Plane pages).  Injected by the engine when a
    knowledge-backend transport is configured; read by the Plan-phase
    ``## Relevant knowledge`` prefetch.  ``None`` when no backend is
    wired -- the prefetch auto-disables."""
    storage: Any = None
    notification_service: Any = None
    a2a_service: Any = None
    handle_registry: Any = None
    counterparty_store: Any = None
    """CounterpartyStore for per-(observer, subject) profile lookups.
    Injected by the engine; builtins (``lookup_colleague``) and the Plan
    phase pre-fetch hook use it to surface observed traits."""
    synthesized_skill_store: Any = None
    """SynthesizedSkillStore for per-agent auto-drafted skills.
    Injected by the engine; used by ``use_skill`` and by the Plan-phase
    pre-fetch hook that renders the "Synthesized skills you've learned"
    block."""
    episode_store: Any = None
    """EpisodeStore for per-agent episodic memory.  Injected by the
    engine; read by the Plan-phase pre-fetch hook that renders the
    "Similar prior work" block (frozen-at-turn-start retrieval).  The
    ``query_episodes`` builtin uses a separately-registered store
    reference and does not depend on this field."""
    agent_diary: Any = None
    """AgentDiary for the agent's private diary (LONG/SHORT memories).
    Injected by the engine; used by ``reflect_and_persist`` and
    ``refresh_memory`` for writes, and by the Plan-phase
    ``## Personal memory`` pre-fetch hook for reads.  ``None`` in
    test / in-memory mode -- the surfaces auto-disable."""
    onboarding_marker_store: Any = None
    """OnboardingMarkerStore for per-agent onboarding bookkeeping.
    Injected by the engine; read by the Plan-phase onboarding-hint
    prefetch (``is_onboarded`` short-circuits the hint render) and
    written by the ``mark_onboarded`` builtin.  ``None`` in
    test / in-memory mode -- the hint always renders (no "already
    onboarded" path) and ``mark_onboarded`` returns an error."""
    last_notification_metadata: dict[str, Any] = Field(default_factory=dict)
    """Metadata from the most recent inbound notification that triggered
    this agent turn.  Used by outbound tools to auto-populate
    ``reply_to_metadata`` (channel, ts, transport, etc.) so the LLM
    does not have to reconstruct it from unstructured DM text."""
    prompt_skill_registry: Any = None
    """Engine-wide :class:`PromptSkillRegistry`.  Injected by the engine;
    used by the ``load_tool_skill`` builtin to fetch a skill body by
    exact key.  ``None`` in test paths that don't wire the registry —
    the builtin returns a configuration error in that case."""

    model_config = {"arbitrary_types_allowed": True}


class CheckContext(BaseModel):
    """Context passed to per-tool ``check_fn`` callables.

    A small, single-purpose Pydantic model carrying just what a
    ``check_fn`` needs to decide whether its tool is available *for
    this agent's turn*: the role identity, the resolved MCP-env
    credentials, and a handle on the MCP bridge for liveness checks.

    Built once per turn by ``TurnEngine``; the resolved availability
    set is then cached on the ``TurnContext`` so the same ``check_fn``
    is not invoked four times (Plan / Execute / Review / sub-agent).

    Kept deliberately separate from ``AgentContext`` (which is much
    wider; tools see it during ``execute``). ``check_fn`` should not
    need event-queue / knowledge / notification handles — those are
    runtime services, not preconditions for tool visibility.
    """

    agent_handle: str = ""
    role_name: str = ""
    mcp_env: dict[str, dict[str, str]] = Field(default_factory=dict)
    """Resolved per-server env from the role's ``mcp_env``. Outer
    key = MCP server name, inner = env-var → value. Used by colleague
    check_fns to test e.g. ``mcp_env.get("slack", {}).get("token")``."""
    mcp_bridge: Any = None
    """Reference to the engine's ``MCPToolBridge`` so check_fns can
    call ``is_server_alive(server_name)``. ``None`` in test mode."""
    sandbox_enabled: bool = False
    """True when this role can run the detached sandbox tool — i.e. the
    role has an enabled ``role.sandbox`` AND the engine has a sandbox
    manager wired (``providers.sandbox``). The ``run_sandbox`` builtin's
    ``check_fn`` gates on this so the tool only appears for roles allowed
    to use it."""

    model_config = ConfigDict(arbitrary_types_allowed=True)


CheckFn = Callable[[CheckContext], bool]
"""Signature for per-tool availability check functions.

Convention: returns ``True`` when the tool is usable for the calling
agent / turn, ``False`` when it should be hidden from the catalogue
and rejected at lookup. Exceptions are treated as ``False`` (fail-
safe) with a logged warning.

Caching: the result is memoized per-turn — a ``check_fn`` is invoked
at most once per turn regardless of how many phases consult it.
"""


@runtime_checkable
class ToolResultValidator(Protocol):
    """Protocol for custom tool result validators.

    Extensions can register validators to inspect/transform tool output
    before it is returned to the agent. Validators run in sequence;
    each receives the (possibly modified) output from the previous one.
    """

    @property
    def name(self) -> str: ...

    def validate(self, output: str) -> str:
        """Validate and optionally transform tool output.

        Return the (possibly modified) output string.
        Raise ``ValueError`` to reject the output entirely.
        """
        ...


@runtime_checkable
class Tool(Protocol):
    """Protocol for agent tools."""

    @property
    def name(self) -> str: ...

    @property
    def description(self) -> str: ...

    @property
    def parameters(self) -> dict[str, Any]: ...

    async def execute(
        self, params: dict[str, Any], context: AgentContext
    ) -> ToolResult: ...
