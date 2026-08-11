"""Tool Skill data models.

Pure data + a pure trigger evaluator. No engine wiring; no Confluence;
no DB. Keeps the foundation testable in isolation.
"""

from __future__ import annotations

import re
from collections.abc import Mapping
from enum import StrEnum

from pydantic import BaseModel, ConfigDict, Field, model_validator

MAX_SKILL_BODY_BYTES = 32_768
"""Hard cap on rendered skill body size.

Skills are loaded into the LLM's conversation context on demand via
the ``load_tool_skill`` builtin (always available in Plan, Execute,
and Sub-agent), NOT inlined into every prompt prefix. The body cap
is therefore looser than it would be for prompt-prefix prose — it
just defends against runaway page content. Skills bigger than this
almost
always want to be split into multiple skills.
"""

MAX_SKILL_SUMMARY_BYTES = 240
"""Hard cap on the catalogue summary line.

The summary is the one line that ALWAYS appears in the per-phase prompt
catalogue, regardless of whether the LLM ends up loading the body.  Keep
it tight so a role with 20 MCP servers doesn't bloat the prompt prefix.

The cap bounds the **source** bytes (validated at model construction,
before ``${variable}`` substitution).  A summary that references skill
variables can render slightly longer; keep variable values short.
"""

SKILL_REQUIRED_DEFAULT = True
"""Single source of truth for :attr:`PromptSkill.required`'s default.

Referenced by the pydantic field below and by the import CLI
(``crewlet confluence import`` stamps the effective value into the
Confluence page's YAML box when the source file omits the key) so the
two can never disagree.  The parser deliberately does NOT restate the
default — it forwards ``required`` only when the frontmatter carries
it, letting the model default apply.
"""


_SKILL_VAR_PATTERN = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")
"""Matches the braced ``${identifier}`` substitution form ONLY.

Bare ``$name``, literal ``$$``, single-brace ``{name}`` (the agent-facing
placeholders skill bodies use, e.g. ``{page_id}``), and non-identifier
keys (``${a.b}``, ``${../x}``) are deliberately NOT matched — see
:func:`substitute_variables`.  The identifier grammar mirrors what
``CompanyConfig`` validates ``skill_variables`` keys against, so the
operator key-space and the render key-space are provably identical.
"""

SKILL_VARIABLE_KEY_PATTERN = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
"""Allowed shape of a ``skill_variables`` key (a substitution identifier)."""


def substitute_variables(text: str, variables: Mapping[str, str]) -> str:
    """Substitute ``${name}`` references in ``text`` from ``variables``.

    Only the **braced identifier** form ``${name}`` is recognised, and
    only when ``name`` is a key in ``variables``; every other byte is left
    verbatim.  In particular a bare ``$name``, a literal ``$$``, a
    single-brace ``{name}``, and an unknown ``${other}`` all pass through
    unchanged.  This keeps substitution a byte-for-byte identity on skill
    prose that legitimately contains shell / regex / Make / currency
    ``$`` — and keeps catalogue summaries byte-stable across turns for
    prompt prefix caching whenever the variable map is unchanged.

    Returns ``text`` unchanged when there are no variables or no ``${``
    sequence, so the common no-variable path costs nothing.

    This is a deliberately narrower engine than :class:`string.Template`,
    which also substitutes bare ``$name`` and collapses ``$$`` → ``$`` —
    both of which would silently corrupt skill bodies (which are
    tool-usage docs dense with command examples).
    """
    if not variables or "${" not in text:
        return text

    def _sub(match: re.Match[str]) -> str:
        name = match.group(1)
        replacement = variables.get(name)
        # Unknown key → leave the literal ``${name}`` so a missing
        # variable is visibly un-substituted (debuggable) rather than
        # silently blanked into a malformed value.
        return replacement if replacement is not None else match.group(0)

    return _SKILL_VAR_PATTERN.sub(_sub, text)


def collect_tool_leaves(expr: TriggerExpr) -> set[str]:
    """Every exact tool name a trigger expression references."""
    if expr.tool is not None:
        return {expr.tool}
    out: set[str] = set()
    for sub in expr.any_of or expr.all_of:
        out |= collect_tool_leaves(sub)
    return out


def collect_server_leaves(expr: TriggerExpr) -> set[str]:
    """Every MCP server name a trigger expression references."""
    if expr.mcp_server is not None:
        return {expr.mcp_server}
    out: set[str] = set()
    for sub in expr.any_of or expr.all_of:
        out |= collect_server_leaves(sub)
    return out


def classify_trigger_liveness(
    expr: TriggerExpr,
    *,
    known_tools: set[str] | frozenset[str],
    known_servers: set[str] | frozenset[str],
) -> tuple[list[str], bool]:
    """Classify a trigger against the deployment's actual tool surface.

    Returns ``(dangling_tool_leaves, skill_is_live)`` where
    ``dangling_tool_leaves`` are the trigger's exact tool names that
    match **no** tool registered anywhere in the engine, and
    ``skill_is_live`` is whether any other part of the trigger still
    matches the deployment (a tool leaf naming a real tool, or an
    ``mcp_server`` leaf naming a configured server).

    Trigger matching is exact-string with no validation anywhere else,
    so an upstream MCP server renaming a tool silently disables both
    the skill's catalogue entry and — for required skills — the
    load-before-use guard.  The live/dead split separates the two
    interpretations of a dangling name: a *partially live* skill whose
    other leaves match is almost certainly name drift (warn); a skill
    whose entire trigger matches nothing is plausibly authored for a
    stack this org doesn't run (informational).
    """
    tool_leaves = collect_tool_leaves(expr)
    dangling = sorted(name for name in tool_leaves if name not in known_tools)
    live = bool(tool_leaves - set(dangling)) or bool(
        collect_server_leaves(expr) & known_servers
    )
    return dangling, live


def find_variable_references(text: str) -> set[str]:
    """Return every ``${identifier}`` name referenced in ``text``.

    Same grammar as :func:`substitute_variables` — bare ``$name``,
    ``$$``, and single-brace ``{name}`` are not references.  Used by the
    registry to warn operators when a skill references a variable that
    is missing from ``skill_variables`` (which would otherwise ship the
    literal ``${name}`` to the LLM with no signal anywhere).
    """
    return {match.group(1) for match in _SKILL_VAR_PATTERN.finditer(text)}


class Phase(StrEnum):
    """Per-phase scopes a skill can declare."""

    PLAN = "plan"
    EXECUTE = "execute"
    REVIEW = "review"
    SUBAGENT = "subagent"


class TriggerExpr(BaseModel):
    """Discriminated trigger expression.

    Exactly one of ``mcp_server``, ``tool``, ``any_of``, ``all_of`` must
    be set. The presence-by-field shape matches the YAML frontmatter
    operators author directly:

    .. code-block:: yaml

        trigger:
          mcp_server: github

        trigger:
          any_of:
            - tool: query_episodes
            - tool: confluence_search
    """

    model_config = ConfigDict(extra="forbid")

    mcp_server: str | None = None
    tool: str | None = None
    any_of: list[TriggerExpr] = Field(default_factory=list)
    all_of: list[TriggerExpr] = Field(default_factory=list)

    @model_validator(mode="after")
    def _exactly_one(self) -> TriggerExpr:
        kinds = (
            self.mcp_server is not None,
            self.tool is not None,
            bool(self.any_of),
            bool(self.all_of),
        )
        if sum(kinds) != 1:
            raise ValueError(
                "TriggerExpr must set exactly one of mcp_server, tool, any_of, all_of"
            )
        return self


class TriggerContext(BaseModel):
    """Facts a trigger evaluates against.

    Built once per phase by the prompt builder from the active
    ``ToolSurface`` and the role's ``mcp_env``.
    """

    model_config = ConfigDict(frozen=True)

    tools: frozenset[str] = Field(default_factory=frozenset)
    mcp_servers: frozenset[str] = Field(default_factory=frozenset)


def evaluate_trigger(expr: TriggerExpr, ctx: TriggerContext) -> bool:
    """Recursive boolean evaluation of ``expr`` against ``ctx``."""
    if expr.mcp_server is not None:
        return expr.mcp_server in ctx.mcp_servers
    if expr.tool is not None:
        return expr.tool in ctx.tools
    if expr.any_of:
        return any(evaluate_trigger(sub, ctx) for sub in expr.any_of)
    if expr.all_of:
        return all(evaluate_trigger(sub, ctx) for sub in expr.all_of)
    return False


def trigger_covers_tool(
    expr: TriggerExpr,
    *,
    tool_name: str,
    server_name: str = "",
) -> bool:
    """Whether ``expr`` expresses interest in one specific tool.

    Distinct from :func:`evaluate_trigger`: that answers "does this
    skill apply to the current surface?" while this answers "is THIS
    tool one of the tools the skill is about?".  The required-skill
    guard (:mod:`crewlet.agent.skills.guard`) needs both — a skill
    only gates the tools its trigger names, not every tool on the
    surface that happened to catalogue it.

    Leaf semantics: a ``tool`` leaf covers the exact tool name; an
    ``mcp_server`` leaf covers every tool served by that MCP server
    (``server_name`` is the tool's ``server_name`` attribute, empty
    for builtins).  Composites use union semantics for BOTH ``any_of``
    and ``all_of`` — an ``all_of`` skill is about the combination of
    its surfaces, so each of those surfaces' tools is covered once
    the full trigger matches.
    """
    if expr.tool is not None:
        return expr.tool == tool_name
    if expr.mcp_server is not None:
        return bool(server_name) and expr.mcp_server == server_name
    subs = expr.any_of or expr.all_of
    return any(
        trigger_covers_tool(sub, tool_name=tool_name, server_name=server_name)
        for sub in subs
    )


class PromptSkill(BaseModel):
    """A tool skill — catalogue summary, rich body, and trigger metadata.

    Lives in the in-memory :class:`PromptSkillRegistry`; no DB row.
    Originates from a page in the Tool Skills container of the active
    knowledge backend (a Confluence space or a Plane project); the
    ``source_page_id`` / ``source_page_version`` fields exist for
    logging and webhook eviction, not for runtime behaviour.

    Two-tier consumption (see ``docs/concepts/tool-skills.md``):

    - ``summary`` always appears in the per-phase prompt catalogue, so
      the planner / executor sees that the skill exists.
    - ``body`` is loaded on demand by the LLM via the
      ``load_tool_skill`` builtin, which is always available in Plan,
      Execute, and Sub-agent.
    """

    model_config = ConfigDict(extra="forbid")

    key: str = Field(min_length=1)
    trigger: TriggerExpr
    phases: set[Phase] = Field(default_factory=lambda: {Phase.PLAN})
    title: str = Field(min_length=1)
    summary: str = Field(min_length=1)
    body: str
    required: bool = SKILL_REQUIRED_DEFAULT
    """Load-before-use safeguard (see ``crewlet.agent.skills.guard``).

    ``True`` (default): the engine enforces the skill.  Within one LLM
    session (one phase's message history), any call to a tool the
    trigger covers is rejected with a "load this skill first" error
    until the LLM has successfully called ``load_tool_skill(key)`` in
    that same session.  Enforcement applies per declared phase, in the
    phases that carry domain tools (Plan / Execute / Sub-agent).

    ``False``: the skill is advisory — the catalogue summary is a hint
    and the LLM decides whether to load the body.  Mark orientation /
    hint-grade content (broad ``mcp_server`` overviews, retrieval
    advice) advisory; a skill is practices for the tools its trigger
    names, so keep triggers narrow (exact tool names) when enforced —
    a server-wide trigger would gate every tool on the server,
    including reads the practices don't apply to.
    """
    source_page_id: str = ""
    """Backend page id the skill was decoded from (``""`` for skills
    constructed directly, e.g. in tests).  The webhook eviction path
    matches on it (:meth:`PromptSkillRegistry.evict_by_page_id`)."""
    source_page_version: int = 0
    """Backend page version at decode time.  Confluence stamps its
    integer version number; Plane has no integer page version (its
    pages API exposes ``updated_at`` only), so Plane-sourced skills
    keep ``0``."""

    @model_validator(mode="after")
    def _body_length_cap(self) -> PromptSkill:
        byte_len = len(self.body.encode("utf-8"))
        if byte_len > MAX_SKILL_BODY_BYTES:
            raise ValueError(
                f"skill body exceeds {MAX_SKILL_BODY_BYTES} byte cap "
                f"(got {byte_len} bytes)"
            )
        summary_len = len(self.summary.encode("utf-8"))
        if summary_len > MAX_SKILL_SUMMARY_BYTES:
            raise ValueError(
                f"skill summary exceeds {MAX_SKILL_SUMMARY_BYTES} byte "
                f"cap (got {summary_len} bytes)"
            )
        return self


TriggerExpr.model_rebuild()

__all__ = [
    "MAX_SKILL_BODY_BYTES",
    "MAX_SKILL_SUMMARY_BYTES",
    "SKILL_REQUIRED_DEFAULT",
    "SKILL_VARIABLE_KEY_PATTERN",
    "Phase",
    "PromptSkill",
    "TriggerContext",
    "TriggerExpr",
    "classify_trigger_liveness",
    "collect_server_leaves",
    "collect_tool_leaves",
    "evaluate_trigger",
    "find_variable_references",
    "substitute_variables",
    "trigger_covers_tool",
]
