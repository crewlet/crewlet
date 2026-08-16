"""The in-sandbox ``ask`` shim + MCP scoping.

A headless coding agent can't pause to ask a person — ``claude -p`` runs
to completion. So when it is blocked on something only a human can
answer, it runs one shim command, ``crewlet-ask``, which records the
question + audience to a file the runner reads on ``collect``
(:func:`~crewlet.sandbox.coding_agents.claude_code.ClaudeCodeRunner.collect`)
and surfaces as ``CodingAgentResult.needs_input``. The shim is
**signal-only**: it never posts anything itself — the engine routes the
question on its audited per-role surface, so identity attribution,
capability guards, and delegation telemetry stay on the engine.

This module also renders the **scoped** MCP config the coding agent gets:
only the servers named in ``role.sandbox.mcp.servers``, rendered with the
role's per-server credentials (see :mod:`crewlet.sandbox.mcp_render`) —
never the role's full MCP surface by default.
"""

from __future__ import annotations

import json
from collections.abc import Sequence
from typing import Any

from crewlet.sandbox.protocol import DEFAULT_SANDBOX_HOME

# Where the shim lives and where it drops its signal, for a sandbox at
# the default home. These are DEFAULTS: the runner installs the shim (and
# points it at an output path) derived from the box's own
# ``sandbox.home`` — see
# :class:`~crewlet.sandbox.coding_agents._detached.RunPaths`. A local
# backend runs many boxes on one filesystem, where a single system-wide
# ``/usr/local/bin/crewlet-ask`` would be both unwritable (the engine user
# is unprivileged) and shared between boxes.
ASK_SHIM_PATH = f"{DEFAULT_SANDBOX_HOME}/.crewlet/bin/crewlet-ask"
ASK_OUTPUT_PATH = f"{DEFAULT_SANDBOX_HOME}/.crewlet/ask.json"


def build_ask_shim(output_path: str = ASK_OUTPUT_PATH) -> str:
    """Return the ``crewlet-ask`` shim script.

    Usage inside the sandbox: ``crewlet-ask "the question" --to team``.
    It writes ``{"question", "to"}`` to ``output_path`` (JSON) and exits 0.
    Pure shell + python3 (always present in E2B's templates) so it has no
    install-time dependency.
    """
    return f"""#!/usr/bin/env python3
import json, sys, argparse, os
p = argparse.ArgumentParser()
p.add_argument("question")
p.add_argument("--to", default="requester")
a = p.parse_args()
os.makedirs(os.path.dirname({output_path!r}), exist_ok=True)
with open({output_path!r}, "w") as f:
    json.dump({{"question": a.question, "to": a.to}}, f)
print("Question recorded; stop now — a person will answer and your work "
      "will resume with their reply.")
"""


def ask_brief_instruction(roster: Sequence[str] = (), manager: str = "") -> str:
    """The brief addendum telling the agent how + when to ask.

    ``roster`` / ``manager`` give the agent real names to target so a
    ``--to`` decision can name a person, not guess.
    """
    lines = [
        "\n## If you get blocked on a human decision",
        "You are running autonomously and CANNOT pause to ask interactively. "
        "If you hit something only a person can answer — an ambiguous spec, a "
        "missing detail, a design/framework decision above your remit — do "
        "NOT guess:",
        "1. Commit and push your work-in-progress branch.",
        '2. Run: crewlet-ask "<a specific, self-contained question>" --to <audience>',
        "   where <audience> is `requester` (the person who asked, for a spec "
        "clarification), `team` (a design/technical decision), `manager`, or a "
        "teammate's name.",
        "3. Stop. Your work resumes automatically once they reply.",
    ]
    if roster:
        lines.append("Teammates you can name: " + ", ".join(roster) + ".")
    if manager:
        lines.append(f"Your manager: {manager}.")
    return "\n".join(lines)


def findings_brief_instruction(findings_path: str) -> str:
    """Brief addendum: the agent's report is handed back to continue the turn.

    The coding agent is **not** the last step — its findings are returned to
    the Crewlet agent, which continues the same task (replies to the requester,
    acts on what was found). The streamed final message can be lost when the
    agent finishes but never exits (OpenCode's known bug), and a tool-only run
    leaves no parsed text at all, so we require a **durable** structured report
    written to a known file the runner always reads on collect
    (:meth:`DetachedFileRunner.collect`).
    """
    return "\n".join(
        [
            "\n## Before you finish — write your report",
            "You are NOT the last step. Your output is handed back to the "
            "Crewlet agent, which continues this task (e.g. replies to the "
            "requester, acts on what you found). Before you stop, write your "
            f"final structured report to `{findings_path}`:",
            "- Outcome: succeeded / partial / blocked.",
            "- What you did and verified (tests run + their results).",
            "- The PR or branch you opened, if any (full URL).",
            "- What remains and what the Crewlet agent should do next.",
            "Write that file even if you also print a summary — it is the "
            "authoritative report that gets read back to continue the task.",
        ]
    )


def render_mcp_config(
    mcp_servers: dict[str, dict[str, Any]],
    servers: Sequence[str],
) -> dict[str, Any]:
    """Build the coding agent's ``.mcp.json`` for the scoped server set.

    ``mcp_servers`` maps server name → an already-resolved launch spec
    (``{"type": "http", "url": ..., "headers": {...}}`` or
    ``{"command": ..., "args": [...], "env": {...}}``). Only the
    ``servers`` named in ``role.sandbox.mcp.servers`` are included, so the
    in-sandbox agent never sees a server the role didn't scope to it.
    Returns the Claude Code / OpenCode ``{"mcpServers": {...}}`` shape.
    """
    rendered: dict[str, Any] = {}
    for name in servers:
        spec = mcp_servers.get(name)
        if spec:
            rendered[name] = spec
    return {"mcpServers": rendered}


def mcp_config_json(
    mcp_servers: dict[str, dict[str, Any]], servers: Sequence[str]
) -> str:
    """``render_mcp_config`` serialised for writing into the sandbox."""
    return json.dumps(render_mcp_config(mcp_servers, servers), indent=2)


__all__ = [
    "ASK_OUTPUT_PATH",
    "ASK_SHIM_PATH",
    "ask_brief_instruction",
    "build_ask_shim",
    "findings_brief_instruction",
    "mcp_config_json",
    "render_mcp_config",
]
