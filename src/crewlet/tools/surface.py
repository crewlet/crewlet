"""Phase-specific tool surfaces for the turn engine.

MCP integration can register 50-150 tools per role. Sending every
schema into every LLM call bloats payloads and slows the call. The
turn engine handles this by giving each phase a different view over
the same underlying registry:

- **Plan** — a *slim* catalogue in the system prompt (builtin tool
  names + MCP server names only; MCP tool names hidden behind the
  ``list_mcp_server_tools`` meta-tool), plus the
  ``activate_tool`` / ``list_mcp_server_tools`` / ``submit_plan`` /
  ``load_tool_skill`` meta-tools. The planner discovers MCP tools
  on demand, activates the ones it needs for in-Plan recon, and
  names the rest in ``submit_plan.tools_needed`` for Execute.
- **Execute** — the tools named in ``plan.tools_needed`` plus a
  small always-on set (default ``["load_tool_skill"]``), plus the
  same ``activate_tool`` / ``list_mcp_server_tools`` discovery
  meta-tools so Execute can fill mid-run gaps the planner missed.
  Carries the same slim catalogue (builtins + MCP server names)
  so the executor knows what discovery surface is available.
- **Review** — no domain tools; a structured-output tool forces the
  decision enum.
- **Sub-agent** — only the tools the parent named in
  ``tool_names``, plus the always-included ``load_tool_skill``
  loader. Same rule as Execute otherwise (no discovery — sub-agents
  run with a fixed parent-chosen surface).

``ToolSurface`` is the authoritative gate for tool dispatch within
a phase.  :func:`crewlet.agent.llm_loop.execute_tool` resolves every
tool call via :meth:`ToolSurface.lookup`; a name that isn't in the
surface gets an ``Unknown tool: <name>`` result rather than
reaching the underlying registry.  This is the enforcement point
for the per-phase invariants (Execute starts from plan.tools_needed
and may grow it via ``activate_tool``; sub-agents only see
parent-allowed tools minus the denylist, with no discovery).  The
surface also carries the phase's required-skill guard
(``skill_guard`` — see :mod:`crewlet.agent.skills.guard`), consulted
by ``execute_tool`` before dispatching each call.
"""

from __future__ import annotations

from typing import Any, Literal

from crewlet._logging import get_logger
from crewlet.providers.llm.protocol import ToolDef
from crewlet.tools.capabilities import resolve_annotations, writes_to_shared_surface
from crewlet.tools.protocol import Tool
from crewlet.tools.registry import ToolRegistry

logger = get_logger("tools.surface")

Phase = Literal["plan", "execute", "review", "subagent", "judge", "onboarding"]

# Phases whose surface carries the discover-then-activate meta-tools: they
# expose the slim catalogue (``catalogue_text`` / ``catalogue_names``) and
# support :meth:`ToolSurface.activate`. ``onboarding`` is one of them — the
# dedicated pre-Plan onboarding pass must reach its knowledge-base MCP tools
# the same way Plan/Execute do. Omitting it here is the "availability gate"
# trap: the agent can SEE a tool via ``list_mcp_server_tools`` (phase-
# independent) but ``activate`` silently refuses, so onboarding can never read
# its pages, never marks, and re-fires every turn.
_DISCOVERY_PHASES = ("plan", "execute", "subagent", "onboarding")

# First-party engine-control tools that are never exposed to
# sub-agents, regardless of what the parent names in ``tool_names``.
# These are Crewlet's *own* tools (not third-party integrations), so
# naming them here is not a tool-stack dependency — it encodes the
# sub-agent runtime invariants: a sub-agent must not spawn further
# sub-agents, must not grow its surface mid-run, and must not reach out
# to other agents independently.
#
# Writes to *external shared surfaces* (posting to a channel, commenting
# on an issue, opening a PR) are NOT listed by name here — that would
# couple the engine to one tool stack (Slack/Jira/GitHub) and silently
# fail for any other.  They are denied by deriving each tool's behaviour
# from its MCP annotations; see ``writes_to_shared_surface`` and
# ``ToolSurface.for_subagent``.
_SUBAGENT_CONTROL_DENYLIST: frozenset[str] = frozenset(
    {
        "spawn_subagent",
        # Launching a detached coding run is engine control, not a tool
        # call, and it is keyed to the PARENT: the pending row carries
        # the parent's turn_id and the completion pauses the parent
        # seat's inbox. A sub-agent runs with `allow_suspend=False`, so
        # the loop cannot park for the result — the parent turn then
        # finishes normally, never persisting an `execute_state`, and
        # the seat stays deaf for the whole coding run until the
        # completion arrives with nothing to resume into. It carries no
        # MCP annotations, so the shared-write filter below does not
        # catch it either.
        "run_sandbox",
        # Discovery / activation meta-tools: sub-agents run with a
        # fixed parent-chosen surface and must not grow it mid-run.
        "activate_tool",
        "list_mcp_server_tools",
        # Cross-agent communication: sub-agents are scoped strictly
        # to their parent's task and must not reach out independently.
        "a2a_ask",
        # A2A channel tools are not part of the engine surface; excluded
        # defensively in case an extension registers them.
        "request_a2a_channel",
        "send_a2a_message",
        "close_a2a_channel",
    }
)


def merge_registry_and_mcp(
    registry: ToolRegistry,
    role_mcp_tools: list[Tool],
) -> list[Tool]:
    """Merge global registry tools with per-role MCP tools.

    Precedence rule: per-role MCP tools override globally-registered
    tools with the same name, because role-scoped MCP tools carry
    role-specific credentials (e.g. a Slack bot scoped to the
    Engineering channel).
    """
    mcp_names = {t.name for t in role_mcp_tools}
    global_tools = registry.list_tools()
    merged = [t for t in global_tools if t.name not in mcp_names]
    merged.extend(role_mcp_tools)
    return merged


def subagent_safe_tools(
    registry: ToolRegistry,
    role_mcp_tools: list[Tool],
    *,
    availability_filter: set[str] | None = None,
) -> list[Tool]:
    """The role-tool universe a sub-agent may discover and activate.

    A sub-agent can discover (``list_mcp_server_tools``) and activate
    (``activate_tool``) any tool in the role's catalogue EXCEPT the
    ones that would breach a runtime invariant — exactly the same
    filters :meth:`ToolSurface.for_subagent` applies to an explicitly
    requested tool:

    - first-party engine-control tools (``_SUBAGENT_CONTROL_DENYLIST``:
      ``spawn_subagent``, the discovery meta-tools, ``a2a_ask``),
    - tools whose MCP annotations classify them a write to an external
      shared surface (:func:`writes_to_shared_surface`) — an identity
      leak,
    - tools gated off this turn by ``availability_filter``.

    Because the discovery catalogue is pre-filtered here, the sub-agent
    can never widen itself past a read-only / non-control capability:
    the guard holds whether the tool arrives via the parent's explicit
    grant or the sub-agent's own discovery.
    """
    available = merge_registry_and_mcp(registry, role_mcp_tools)
    safe: list[Tool] = []
    for tool in available:
        if tool.name in _SUBAGENT_CONTROL_DENYLIST:
            continue
        if writes_to_shared_surface(
            resolve_annotations(tool, registry.annotations_for)
        ):
            continue
        if availability_filter is not None and tool.name not in availability_filter:
            continue
        safe.append(tool)
    return safe


def _to_tool_defs(tools: list[Tool]) -> list[ToolDef]:
    """Convert Tool instances to LLM-provider ToolDefs."""
    return [
        ToolDef(
            name=t.name,
            description=t.description,
            parameters=t.parameters,
        )
        for t in tools
    ]


class ToolSurface:
    """A phase-specific filtered view over the tool registry.

    Construct one per phase via the class-method factories:
    :meth:`for_plan`, :meth:`for_execute`, :meth:`for_review`,
    :meth:`for_subagent`. Each factory encodes the rules for that
    phase.

    The surface exposes:

    - :meth:`to_tool_defs` — the schemas passed to the LLM SDK's
      ``tools=[...]`` parameter.
    - :meth:`catalogue_text` — a compact ``name: description`` listing
      for injection into the Plan-phase system prompt. Empty for
      non-Plan phases.
    - :meth:`lookup` — resolve a Tool by name for dispatch (used by
      ``llm_loop`` when the LLM emits a tool call).
    - :meth:`has` — whether a name is in the surface.
    """

    def __init__(
        self,
        *,
        phase: Phase,
        tools: list[Tool],
        catalogue_tools: list[Tool] | None = None,
        registry: ToolRegistry,
        role_mcp_tools: list[Tool],
    ) -> None:
        self.phase = phase
        self._tools = tools
        self._tool_map: dict[str, Tool] = {t.name: t for t in tools}
        # catalogue_tools is the role's full registry+MCP universe used
        # for activation lookup. Plan and Execute both populate it so
        # the discover-then-activate flow can promote any tool the role
        # is configured to use. Review / Sub-agent leave it empty.
        self._catalogue_tools: list[Tool] = catalogue_tools or []
        self._registry = registry
        self._role_mcp_tools = role_mcp_tools
        # Required-skill guard (crewlet.agent.skills.guard.SkillGuard).
        # Assigned by the phase runners that carry domain tools (Plan /
        # Execute / Sub-agent) after the surface is constructed — the
        # guard's trigger context derives from the surface itself.
        # ``execute_tool`` consults it on every dispatch; ``None``
        # (Review, Judge, rescue/grace surfaces, engines without a
        # skill registry) disables enforcement.
        self.skill_guard: Any = None

    # ----- factories --------------------------------------------------

    @classmethod
    def for_plan(
        cls,
        registry: ToolRegistry,
        role_mcp_tools: list[Tool],
        *,
        meta_tools: list[Tool] | None = None,
        availability_filter: set[str] | None = None,
    ) -> ToolSurface:
        """Planner surface.

        ``tools=[...]`` passed to the LLM is just the meta-tools
        (typically ``submit_plan`` + ``activate_tool`` +
        ``list_mcp_server_tools`` + ``load_tool_skill``). The full
        registry + MCP tool universe is exposed for activation via
        :meth:`activate`; the prompt-side catalogue rendered by
        :meth:`catalogue_text` is *slim* — builtin tools by name
        plus MCP server names, no MCP tool names.

        ``availability_filter``: when provided, restricts the
        activation universe to tool names in the set. Meta-tools are
        never filtered -- if a planner couldn't see ``submit_plan`` /
        ``activate_tool`` it could not complete a plan at all.
        """
        all_tools = merge_registry_and_mcp(registry, role_mcp_tools)
        if availability_filter is not None:
            all_tools = [t for t in all_tools if t.name in availability_filter]
        return cls(
            phase="plan",
            tools=list(meta_tools or []),
            catalogue_tools=all_tools,
            registry=registry,
            role_mcp_tools=role_mcp_tools,
        )

    @classmethod
    def for_execute(
        cls,
        registry: ToolRegistry,
        role_mcp_tools: list[Tool],
        *,
        tools_needed: list[str],
        always_on: list[str],
        meta_tools: list[Tool] | None = None,
        availability_filter: set[str] | None = None,
        expose_catalogue: bool = True,
    ) -> ToolSurface:
        """Executor surface: plan-named tools + always-on + meta-tools.

        Unknown names in ``tools_needed`` or ``always_on`` are dropped
        silently; the executor can't expose a schema for a tool that
        isn't registered. The Plan phase is responsible for naming
        tools that exist.

        ``meta_tools`` (typically ``activate_tool`` +
        ``list_mcp_server_tools``) are appended to ``tools=[...]`` so
        the executor can discover and promote tools the planner did
        not list. The full registry+MCP universe is captured as
        ``catalogue_tools`` so :meth:`activate` can find any tool the
        role is configured to use.

        ``expose_catalogue=False`` disables both discovery and
        activation — a strict "plan is a hard contract" surface.
        Used by the Execute grace / rescue path so the wrap-up call
        sees no surprise tools.

        ``availability_filter``: when provided, names not in
        the set are dropped from the surface. The exec round therefore
        cannot call a tool whose ``check_fn`` returned ``False``.
        """
        available = merge_registry_and_mcp(registry, role_mcp_tools)
        available_map = {t.name: t for t in available}

        surface_names: list[str] = []
        # Pre-populate ``seen`` with meta-tool names so a plan-named tool
        # that happens to collide with a meta-tool (e.g. a planner
        # confusion that names ``activate_tool`` in tools_needed) is
        # skipped here — the meta-tool wins. Without this, a registry
        # entry of the same name would shadow the closure-bearing
        # meta-tool and silently disable the discover-then-activate flow.
        seen: set[str] = {mt.name for mt in (meta_tools or [])}
        # Plan-named first, then always-on (dedup while preserving order).
        for name in [*tools_needed, *always_on]:
            if name in seen:
                continue
            if name not in available_map:
                logger.debug(
                    "execute_tool_not_registered",
                    tool=name,
                    source=(
                        "plan.tools_needed" if name in tools_needed else "always_on"
                    ),
                )
                continue
            if availability_filter is not None and name not in availability_filter:
                logger.debug(
                    "execute_tool_unavailable",
                    tool=name,
                    source=(
                        "plan.tools_needed" if name in tools_needed else "always_on"
                    ),
                )
                continue
            surface_names.append(name)
            seen.add(name)

        tools = [available_map[n] for n in surface_names]
        # Meta-tools land in ``tools=[...]`` after the plan-named set.
        # Order matters for prefix-cache stability: plan tools first,
        # then meta-tools (so the prefix is byte-identical as long as
        # the plan's tool list is stable). Meta-tool precedence over
        # colliding plan-named tools is handled by the ``seen``
        # pre-population above; the loop below just appends.
        appended: set[str] = set()
        for mt in meta_tools or []:
            if mt.name in appended:
                continue
            tools.append(mt)
            appended.add(mt.name)

        catalogue_tools = available if expose_catalogue else []
        if availability_filter is not None:
            catalogue_tools = [
                t for t in catalogue_tools if t.name in availability_filter
            ]
        return cls(
            phase="execute",
            tools=tools,
            catalogue_tools=catalogue_tools,
            registry=registry,
            role_mcp_tools=role_mcp_tools,
        )

    @classmethod
    def for_review(
        cls,
        registry: ToolRegistry,
        role_mcp_tools: list[Tool],
        *,
        decision_tools: list[Tool] | None = None,
    ) -> ToolSurface:
        """Review surface: no domain tools, optional decision meta-tool."""
        return cls(
            phase="review",
            tools=list(decision_tools or []),
            registry=registry,
            role_mcp_tools=role_mcp_tools,
        )

    @classmethod
    def for_judge(
        cls,
        registry: ToolRegistry,
        role_mcp_tools: list[Tool],
        *,
        decision_tools: list[Tool] | None = None,
    ) -> ToolSurface:
        """Extension-judge surface: no domain tools, single decision tool.

        Identical shape to :meth:`for_review` but tagged ``phase="judge"``
        so prompt-size / round / unknown-tool events fired by
        ``run_tool_loop`` during the judge call carry the right phase
        label.  Without this the judge's ``PromptSize`` event would be
        misattributed to ``"review"``, polluting per-phase prompt-size
        dashboards.
        """
        return cls(
            phase="judge",
            tools=list(decision_tools or []),
            registry=registry,
            role_mcp_tools=role_mcp_tools,
        )

    @classmethod
    def for_subagent(
        cls,
        registry: ToolRegistry,
        role_mcp_tools: list[Tool],
        *,
        parent_tool_names: list[str],
        requested_tool_names: list[str],
        availability_filter: set[str] | None = None,
        meta_tools: list[Tool] | None = None,
    ) -> tuple[ToolSurface, list[str]]:
        """Sub-agent surface. Enforces the runtime invariants:

        - Requested names must be a subset of the parent's tool list.
        - First-party engine-control tools (``spawn_subagent``, the
          discovery meta-tools, ``a2a_ask``) are denied regardless of
          what the parent named (``_SUBAGENT_CONTROL_DENYLIST``).
        - Tools that write to an **external shared surface** — derived
          from each tool's MCP annotations, not a hardcoded name list —
          are denied: a sub-agent posting to a channel / commenting on
          an issue / opening a PR would leak the parent agent's identity
          onto a transcript a human reads.  See
          :func:`~crewlet.tools.capabilities.writes_to_shared_surface`.
        - ``load_tool_skill`` is always included when registered, even
          if the parent didn't name it: the sub-agent prompt's skill
          catalogue points at it, and the required-skill guard relies
          on it as the unlock. It is a read-only prompt-fragment
          loader, so it cannot widen the sub-agent's capabilities.

        ``meta_tools`` (the discovery meta-tools ``list_mcp_server_tools``
        / ``activate_tool``) are appended to the active surface and the
        surface is given a catalogue (:func:`subagent_safe_tools`) so the
        sub-agent can discover and activate tools the parent didn't name
        — bounded to read-only / non-control / non-shared-write tools,
        so discovery can never breach the invariants above. Without
        ``meta_tools`` the surface stays frozen to the requested set
        (the single-shot behaviour, used by unit tests).

        ``availability_filter``: when provided, names not in
        the set are rejected -- a sub-agent inherits the parent's
        availability resolution.

        Returns ``(surface, rejected_names)`` so the caller can emit a
        trace event listing names that were filtered out.
        """
        parent_set = set(parent_tool_names)
        available = merge_registry_and_mcp(registry, role_mcp_tools)
        available_map = {t.name: t for t in available}

        allowed: list[Tool] = []
        seen: set[str] = set()
        rejected: list[str] = []
        for name in requested_tool_names:
            if name in _SUBAGENT_CONTROL_DENYLIST:
                rejected.append(name)
                continue
            if name not in parent_set:
                rejected.append(name)
                continue
            if name not in available_map:
                rejected.append(name)
                continue
            # Identity-leak guard, derived from MCP annotations rather
            # than a tool-name list: deny any tool that writes to an
            # external shared surface (channel post, issue comment, PR).
            if writes_to_shared_surface(
                resolve_annotations(available_map[name], registry.annotations_for)
            ):
                rejected.append(name)
                continue
            if availability_filter is not None and name not in availability_filter:
                rejected.append(name)
                continue
            if name in seen:
                # Duplicates would produce ToolDefs with the same
                # name in ``to_tool_defs()``; some provider SDKs
                # reject that.  Drop silently (not a rejection --
                # the first occurrence is already allowed).
                continue
            allowed.append(available_map[name])
            seen.add(name)

        loader = available_map.get("load_tool_skill")
        if loader is not None and "load_tool_skill" not in seen:
            allowed.append(loader)
            seen.add("load_tool_skill")

        # Discovery meta-tools (when provided) ride on top of the
        # requested set and unlock the safety-filtered catalogue so the
        # sub-agent can find tools the parent didn't name.
        catalogue_tools: list[Tool] = []
        for mt in meta_tools or []:
            if mt.name in seen:
                continue
            allowed.append(mt)
            seen.add(mt.name)
        if meta_tools:
            catalogue_tools = subagent_safe_tools(
                registry,
                role_mcp_tools,
                availability_filter=availability_filter,
            )

        surface = cls(
            phase="subagent",
            tools=allowed,
            catalogue_tools=catalogue_tools,
            registry=registry,
            role_mcp_tools=role_mcp_tools,
        )
        return surface, rejected

    # ----- presentation ----------------------------------------------

    def to_tool_defs(self) -> list[ToolDef]:
        """The ``tools=[...]`` list to pass to ``provider.complete()``."""
        return _to_tool_defs(self._tools)

    def catalogue_text(self) -> str:
        """Slim ``name`` catalogue for the system prompt.

        Renders for any phase that carries the discover-then-activate
        meta-tools (Plan, Execute, and discovery-capable Sub-agents).
        Two blocks:

        - ``## Builtin tools`` — every non-MCP tool in the role's
          catalogue listed as ``- name: first-line-of-description``.
        - ``## MCP servers`` — one line per server, advising the LLM
          to call ``list_mcp_server_tools(server)`` to discover what
          the server offers.

        MCP tool *names* are deliberately omitted from the prompt to
        keep the prefix small (listing a role's 100+ MCP tools would
        add 15–25 KB to every system prompt). The LLM walks the
        server → list → activate path explicitly.

        Empty for Review / Judge surfaces and any surface without a
        catalogue (a sub-agent spawned without the discovery meta-tools).
        """
        if self.phase not in _DISCOVERY_PHASES:
            return ""
        if not self._catalogue_tools:
            return ""
        builtins: list[Tool] = []
        mcp_servers: list[str] = []
        seen_servers: set[str] = set()
        for tool in self._catalogue_tools:
            server = getattr(tool, "server_name", "")
            if server:
                if server not in seen_servers:
                    mcp_servers.append(server)
                    seen_servers.add(server)
            else:
                builtins.append(tool)

        sections: list[str] = []
        if builtins:
            lines = [
                f"- {t.name}: {_first_line(t.description)}"
                for t in sorted(builtins, key=lambda t: t.name)
            ]
            sections.append("### Builtin tools")
            sections.append("\n".join(lines))
        if mcp_servers:
            lines = [f"- {s}" for s in sorted(mcp_servers)]
            sections.append("### MCP servers")
            sections.append(
                "Call `list_mcp_server_tools(server=...)` to discover "
                "what tools a server offers, then `activate_tool(name=...)` "
                "to promote a tool into your `tools=[...]`."
            )
            sections.append("\n".join(lines))
        return "\n".join(sections)

    def catalogue_names(self) -> list[str]:
        """All catalogue tool names (Plan, Execute, discovery sub-agents).

        Returns the full set — including MCP tool names that are NOT
        rendered into the slim prompt catalogue — so trigger matching
        (e.g. tool-skill registry) and activation lookups still see
        the complete activation universe.
        """
        if self.phase not in _DISCOVERY_PHASES:
            return []
        return [t.name for t in self._catalogue_tools]

    def catalogue_mcp_servers(self) -> list[str]:
        """Distinct MCP server names in the catalogue, alphabetised."""
        if self.phase not in ("plan", "execute"):
            return []
        seen: list[str] = []
        seen_set: set[str] = set()
        for tool in self._catalogue_tools:
            server = getattr(tool, "server_name", "")
            if server and server not in seen_set:
                seen.append(server)
                seen_set.add(server)
        return sorted(seen)

    def lookup(self, name: str) -> Tool | None:
        """Return the Tool for ``name`` if it's in this surface."""
        return self._tool_map.get(name)

    def has(self, name: str) -> bool:
        return name in self._tool_map

    def activate(self, name: str) -> bool:
        """Promote a catalogue tool into the active ``tools=[...]``.

        Supported in Plan (in-Plan recon) and Execute (filling gaps
        Plan did not predict). The phase's surface starts with the
        plan-named / meta tools; calling ``activate_tool(name)``
        promotes a catalogue tool so the LLM can invoke it in
        subsequent rounds. The loop refreshes ``tool_defs`` each
        round so the new schema is visible on the next
        ``provider.complete()`` call.

        Returns ``True`` if the tool was found in the catalogue and
        added (or was already active); ``False`` if the name is not
        in the catalogue or this surface does not support activation
        (Review, Judge).

        Sub-agent surfaces support activation when they were built with
        the discovery meta-tools: their catalogue is pre-filtered by
        :func:`subagent_safe_tools`, so anything activatable is already
        guaranteed read-only / non-control / non-shared-write.
        """
        if self.phase not in _DISCOVERY_PHASES:
            return False
        if name in self._tool_map:
            return True
        for tool in self._catalogue_tools:
            if tool.name == name:
                self._tools.append(tool)
                self._tool_map[name] = tool
                return True
        return False

    @property
    def names(self) -> list[str]:
        """Names of tools whose schemas the LLM sees this phase."""
        return [t.name for t in self._tools]

    def __len__(self) -> int:
        return len(self._tools)


def _first_line(text: str) -> str:
    """Return the first non-empty line of ``text`` (for catalogue prose)."""
    for line in text.splitlines():
        stripped = line.strip()
        if stripped:
            return stripped
    return ""


# Public re-export for callers that want the control denylist for
# tests / guard logic (e.g. the sub-agent preamble invariants check).
# External-shared-surface writes are NOT in this set — they are denied
# by annotation-derived classification (``writes_to_shared_surface``).
SUBAGENT_CONTROL_DENYLIST = _SUBAGENT_CONTROL_DENYLIST


def phase_tool_context(surface: ToolSurface) -> dict[str, Any]:
    """Return a dict suitable for trace/event attributes."""
    return {
        "phase": surface.phase,
        "tool_count": len(surface),
        "tools": surface.names,
        "catalogue_size": (
            len(surface.catalogue_names())
            if surface.phase in ("plan", "execute")
            else 0
        ),
    }
