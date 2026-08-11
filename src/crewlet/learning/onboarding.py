"""Onboarding hint + ``mark_onboarded`` builtin.

The onboarding subsystem nudges a fresh agent to read the relevant
``Onboarding`` pages in the team knowledge base once, then stays out
of the way until the org structure changes.  The marker that
suppresses the hint lives in
:class:`~crewlet.learning.onboarding_markers.OnboardingMarkerStore`
(the ``agent_onboarding_markers`` table).

Public surface:

* :func:`compute_chain_hash` -- stable hash over the agent's
  org chain; used by both the prefetch hook (decides whether to
  render the hint) and ``mark_onboarded`` (stamped into the marker
  so a chain change invalidates it).
* :func:`build_onboarding_hint` -- the per-turn Plan-prompt block
  shown to unmarked agents, listing every ``Onboarding`` page on
  their unit chain.
* :func:`register_mark_onboarded_tool` -- registers the LLM-facing
  builtin that writes the marker after the agent has done the
  reading.
"""

from __future__ import annotations

import hashlib
from typing import Any

from crewlet._logging import get_logger
from crewlet.learning.onboarding_markers import OnboardingMarkerStore
from crewlet.org.hierarchy import get_unit_chain_for_role
from crewlet.org.models import Organization, Role
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry

logger = get_logger("learning.onboarding")

ONBOARDING_PAGE_TITLE = "Onboarding"
MARK_ONBOARDED_TOOL = "mark_onboarded"


def compute_chain_hash(role: Role, organization: Organization) -> str:
    """Stable hash over the agent's org chain.

    Used to invalidate an existing onboarding marker when the org
    structure changes (role moved between units, ancestor renamed, new
    unit inserted).  Agent-content-driven re-reads (a knowledge-base
    page edited) are deliberately NOT in the hash -- those are the
    agent's responsibility to track via watchers / chat / their own
    judgement.
    """
    chain = get_unit_chain_for_role(role, organization)
    parts = [organization.name]
    parts.extend(unit.name for unit in chain)
    parts.append(role.name)
    return hashlib.sha256("\n".join(parts).encode("utf-8")).hexdigest()


async def is_onboarded(
    *,
    marker_store: OnboardingMarkerStore | None,
    agent_id: str,
    expected_chain_hash: str,
) -> bool | None:
    """Tri-state onboarding check for this agent's current org chain.

    ``True``: a marker exists and matches the current chain hash.
    ``False``: definitively no matching marker (never marked, or the
    chain changed — the agent needs to (re-)onboard).
    ``None``: the lookup FAILED — state unknown. Callers must err toward
    *skipping* onboarding on unknown (retry the check next turn): a
    spurious repeat pass for an already-marked agent is strictly worse
    than a one-turn delay of the hint, and collapsing failures into
    ``False`` would do exactly that.
    """
    if marker_store is None or not agent_id:
        return False
    return await marker_store.is_onboarded(
        agent_id=agent_id, chain_hash=expected_chain_hash
    )


def build_onboarding_hint(role: Role, organization: Organization) -> str:
    """Render the per-turn onboarding hint shown to unmarked agents.

    The hint enumerates the units on the agent's chain (org root +
    each ancestor + own unit) and the convention page name; the agent
    is expected to use its knowledge-base search tool to locate each
    page in the appropriate area of the knowledge base.
    """
    chain = get_unit_chain_for_role(role, organization)
    bullets = [
        f"- '{ONBOARDING_PAGE_TITLE}' page in the **{organization.name}** "
        "area of the knowledge base"
    ]
    for unit in chain:
        bullets.append(
            f"- '{ONBOARDING_PAGE_TITLE}' page in the **{unit.name}** "
            "area of the knowledge base"
        )
    return (
        "You have not yet completed onboarding for this team configuration.\n\n"
        "Before doing other work, read the following pages (each is a "
        "page in your team knowledge base, literally titled "
        f"'{ONBOARDING_PAGE_TITLE}'):\n" + "\n".join(bullets) + "\n\n"
        "How:\n"
        "- Use your knowledge-base search tool (or a get-page tool if "
        "you know the id) to locate each page.  If a page is missing "
        "for some scope, that's fine -- skip it.\n"
        "- For each page, capture conventions you should apply going "
        "forward via `reflect_and_persist` (scope=agent).  Write "
        "declarative facts, not instructions to yourself.\n"
        "- When you have read all available pages and persisted what "
        "matters, call `mark_onboarded` with a one-line summary.  After "
        "that, this section disappears from your prompt.\n"
        "\n"
        "If a page changes later you can re-read it any time -- the "
        "onboarding marker only re-fires when the org structure itself "
        "changes."
    )


def register_mark_onboarded_tool(
    registry: ToolRegistry,
    marker_store: OnboardingMarkerStore,
    organization: Organization,
) -> None:
    """Register the LLM-facing ``mark_onboarded`` builtin.

    UPSERTs a row in ``agent_onboarding_markers`` keyed by the
    agent's runtime ``agent_id``, stamped with the current chain hash
    so the marker invalidates itself when the org structure changes.
    The summary the agent supplies is persisted so an operator
    inspecting the table can see what each agent took away from
    onboarding.
    """

    async def _mark_onboarded(
        params: dict[str, Any], context: AgentContext
    ) -> ToolResult:
        if marker_store is None:
            return ToolResult(success=False, error="onboarding store unavailable")
        if not context.agent_id:
            return ToolResult(
                success=False, error="agent_id not set on context — cannot mark"
            )
        summary = str(params.get("summary", "")).strip()
        if not summary:
            return ToolResult(
                success=False,
                error="summary is required (one or two sentences on what you learned)",
            )

        # Compute the hash from the LIVE org on the context when present.
        # The check paths (onboarding phase / Plan hint) hash
        # ``agent_context.org``; hashing the org captured at tool
        # registration here could diverge after a live config reload and
        # store a marker the check never matches — re-onboarding forever.
        org = getattr(context, "org", None) or organization
        role = org.get_role(context.role)
        if role is None:
            return ToolResult(
                success=False, error=f"role {context.role!r} not found in org config"
            )
        chain_hash = compute_chain_hash(role, org)
        try:
            await marker_store.mark(
                agent_id=context.agent_id,
                chain_hash=chain_hash,
                agent_handle=context.agent_handle,
                role=context.role,
                summary=summary,
            )
        except Exception as exc:
            logger.exception("mark_onboarded_store_failed")
            return ToolResult(success=False, error=f"store failed: {exc}")
        logger.info(
            "mark_onboarded",
            agent_handle=context.agent_handle,
            role=context.role,
            agent_id=context.agent_id,
        )
        return ToolResult(
            success=True,
            output=(
                "Onboarding marker stored.  This onboarding hint will not "
                "appear in future plans unless the org structure changes "
                "(in which case you'll re-read for the new context)."
            ),
        )

    registry.register(
        SimpleTool(
            name=MARK_ONBOARDED_TOOL,
            description=(
                "Call this AFTER you have read the relevant 'Onboarding' "
                "pages in your team knowledge base for your team and "
                "ancestor units AND captured durable conventions via "
                "reflect_and_persist.  Records a marker so the first-turn "
                "onboarding hint stops appearing in your plans.  "
                "Re-onboarding fires automatically only when the org "
                "structure changes; you can always re-read those pages "
                "on your own."
            ),
            parameters={
                "type": "object",
                "properties": {
                    "summary": {
                        "type": "string",
                        "description": (
                            "One or two sentences naming the most important "
                            "conventions you internalised during onboarding."
                        ),
                    },
                },
                "required": ["summary"],
            },
            fn=_mark_onboarded,
        )
    )
    logger.info("mark_onboarded_tool_registered")
