"""Colleague-surface tool: ``a2a_ask``.

Pre-cleanup this module also exposed thin MCP-forwarding wrappers
(``slack_message``, ``jira_assign``, ``jira_comment``,
``confluence_comment``, ``confluence_mention``,
``github_request_review``).  Each one was a 1:1 alias for an upstream
MCP tool with the wrong arg-shape baked in; the layer accumulated
maintenance debt against the upstream MCP servers' changes and gave
us nothing in exchange except a ``DelegationEdge`` event nothing read.
Deleted -- agents now call the upstream MCP tools directly (the chat /
issue-tracker / wiki / code-host tools their configured MCP servers
expose), choosing them by description rather than any engine-hardcoded
name.

What survives:

- :func:`a2a_ask` -- private agent-to-agent channels.  Pure engine
  builtin (no MCP forwarding); the only path the LLM has for tight-
  loop / mechanical sync between agents that should NOT show up on
  the team's chat or issue tracker.  It forwards the caller's
  delegation chain so the
  recipient's turn inherits the accumulated depth; the always-on
  delegation-depth cap (checked at the top of every turn) bounds the
  chain.
"""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry

logger = get_logger("tools.colleague")


async def _a2a_ask(params: dict[str, Any], context: AgentContext) -> ToolResult:
    """Open an A2A channel with a colleague and post a brief.

    Uses the existing :class:`~crewlet.a2a.service.A2AService` which
    also publishes ``a2a_channel_opened`` and wakes the target via
    ``crewlet.agent.{target}.inbox``.
    """
    from crewlet.tools.builtin import resolve_colleague_party

    role_urn = params.get("role_urn") or params.get("target_handle") or ""
    brief = params.get("brief", "")
    if not role_urn or not brief:
        return ToolResult(
            success=False,
            error="role_urn and brief are required",
        )
    if context.a2a_service is None:
        return ToolResult(success=False, error="A2A service not available")

    # One resolution pass yields both the canonical handle and the
    # seat kind; fall back to the raw query when nothing matches so
    # the service-side guard produces the error.
    party = resolve_colleague_party(role_urn, context)
    target = party.handle if party is not None else role_urn
    requester = context.agent_handle or context.agent_id

    # Human seats are not on A2A — there is no agent behind
    # the inbox topic, so the channel would wait forever.  Fail with
    # actionable guidance instead.  (The A2AService enforces the same
    # invariant at the chokepoint; this earlier check gives the LLM
    # the richer, person-specific message.)
    if party is not None and party.is_human:
        availability = ""
        if party.role is not None and party.role.availability:
            availability = f" Availability: {party.role.availability}."
        return ToolResult(
            success=False,
            error=(
                f"{party.name} ({party.handle}) is a human teammate — "
                f"they are not on A2A. Reach them where humans "
                f"read: mention them on Slack or comment on the "
                f"relevant Jira issue, leave the state they need in "
                f"the message, and end your turn — they reply "
                f"asynchronously and their reply will re-trigger "
                f"you.{availability}"
            ),
        )

    # Pull delegation info from the current turn context if attached
    # to the AgentContext.  The TurnEngine attaches
    # ``turn_context`` via __dict__ so tools can read it without
    # widening the AgentContext schema.  We forward the chain so the
    # recipient's turn inherits the accumulated depth; the delegation-
    # depth cap (checked at the top of every turn) bounds it.
    turn_ctx = getattr(context, "turn_context", None)
    depth = getattr(turn_ctx, "delegation_depth", 0) if turn_ctx else 0
    chain = getattr(turn_ctx, "delegation_chain", []) if turn_ctx else []
    parent_turn_id = getattr(turn_ctx, "turn_id", "") if turn_ctx else ""

    try:
        # The brief rides the wake event. It used to be a second call
        # into a per-channel in-memory queue, which meant the content
        # lived on the opening node while the wake was delivered to
        # whichever node owned the target's seat.
        channel_id = await context.a2a_service.request_channel(
            requester,
            target,
            brief=brief,
            sender_role=context.role,
            delegation_depth=depth,
            delegation_chain=list(chain),
            parent_turn_id=parent_turn_id,
        )
    except ValueError as exc:
        # The service refused the target (not a live agent) — surface
        # as a tool failure so the LLM sees the error and can re-plan
        # (or reach the target on a human surface) instead of posting
        # into a void.
        return ToolResult(success=False, error=str(exc))

    return ToolResult(
        output=(
            f"Opened A2A channel {channel_id} with {target} and delivered "
            f"the brief. Their answer arrives as a new message on this "
            f"channel — end your turn; it will wake you."
        )
    )


def register_colleague_tools(registry: ToolRegistry) -> None:
    """Register the colleague-surface tools with a ``ToolRegistry``.

    Intended to be called alongside ``register_builtin_tools`` during
    engine startup.  After the wrapper-cleanup the only colleague-
    surface tool is ``a2a_ask`` -- agents reach out to humans /
    cross-agent via the upstream MCP tools directly.
    """
    registry.register(
        SimpleTool(
            name="a2a_ask",
            description=(
                "Ask a colleague a question on a private A2A channel. "
                "Opens an ephemeral 1:1 channel and delivers the "
                "brief; their answer comes back on the same channel "
                "and wakes you, then the channel closes.\n\n"
                "ONE ask, ONE answer -- so put everything you need "
                "into the brief. There is no tool to send a follow-up "
                "on the channel; if you genuinely need another round, "
                "call a2a_ask again.\n\n"
                "Use a2a_ask ONLY for tight-loop / mechanical sync "
                "(high-frequency coordination that would spam a chat "
                "channel, or internal state exchange a human teammate "
                "would not have written down).  For everything else -- "
                "review requests, design questions, status updates, "
                "handoffs (including manager handoffs when stuck) -- "
                "use your colleague-surface tools instead: a chat post "
                "or @-mention, an issue comment / reassignment, a doc "
                "comment, or a code-review request.  If a human "
                "colleague on the same team would reasonably want to "
                "see this message, it does not belong on A2A."
            ),
            parameters={
                "type": "object",
                "properties": {
                    "role_urn": {
                        "type": "string",
                        "description": (
                            "Handle or role identifier of the "
                            "colleague to ask. Use lookup_colleague "
                            "first if unsure."
                        ),
                    },
                    "brief": {
                        "type": "string",
                        "description": "The question / task brief.",
                    },
                    "budget": {
                        "type": "integer",
                        "description": (
                            "Optional token budget hint for the colleague's reply."
                        ),
                    },
                    "deadline": {
                        "type": "string",
                        "description": (
                            "Optional ISO-8601 deadline by which a reply is expected."
                        ),
                    },
                },
                "required": ["role_urn", "brief"],
            },
            fn=_a2a_ask,
        )
    )
    logger.info("colleague_tools_registering")
