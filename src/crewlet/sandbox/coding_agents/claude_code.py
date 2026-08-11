"""``ClaudeCodeRunner`` — Claude Code headless inside an E2B sandbox.

Invokes the Claude Code CLI (preinstalled in E2B's ``claude`` template)
in headless JSON mode and maps its output onto :class:`CodingAgentResult`.
The detached file plumbing (start/poll/collect, the ask overlay) lives in
:class:`DetachedFileRunner`; this module supplies the ``claude -p``
command builder and the result-JSON parser — both pure + unit-pinned.
"""

from __future__ import annotations

import json
import shlex

from crewlet.sandbox.coding_agents._detached import (
    ASK_PATH,
    DONE_MARKER,
    ERR_PATH,
    PR_RE,
    RESULT_PATH,
    WORK_DIR,
    DetachedFileRunner,
)
from crewlet.sandbox.protocol import CodingAgentLLM, CodingAgentResult, RunLimits

# Re-exported for callers / tests that pin the sandbox artifact paths.
__all__ = [
    "ASK_PATH",
    "DONE_MARKER",
    "ERR_PATH",
    "RESULT_PATH",
    "WORK_DIR",
    "ClaudeCodeRunner",
    "build_claude_command",
    "parse_claude_result",
]


def build_claude_command(
    brief: str,
    *,
    model: str = "",
    limits: RunLimits | None = None,
    session_id: str = "",
    mcp_config_path: str = "",
) -> str:
    """Build the headless ``claude -p`` invocation.

    Pure + deterministic so the exact flags are unit-pinned. The brief is
    shell-quoted; budget caps are appended only when set so a minimal call
    stays minimal. Runs ``--permission-mode bypassPermissions``, so the agent
    may use every tool the scoped MCP servers expose — there is no per-tool
    allowlist.
    """
    parts = [
        "claude",
        "-p",
        shlex.quote(brief),
        "--output-format",
        "json",
        "--permission-mode",
        "bypassPermissions",
    ]
    if model:
        parts += ["--model", shlex.quote(model)]
    if session_id:
        parts += ["--resume", shlex.quote(session_id)]
    if limits is not None:
        if limits.max_turns > 0:
            parts += ["--max-turns", str(limits.max_turns)]
        if limits.max_budget_usd > 0:
            parts += ["--max-budget-usd", str(limits.max_budget_usd)]
    if mcp_config_path:
        parts += ["--mcp-config", shlex.quote(mcp_config_path), "--strict-mcp-config"]
    return " ".join(parts)


def parse_claude_result(stdout: str) -> CodingAgentResult:
    """Map Claude Code's ``--output-format json`` output onto a result.

    Tolerant: non-JSON / partial output yields a failed result carrying
    the raw text as ``error`` rather than raising — a coding agent that
    crashed should surface as "didn't deliver", not blow up the turn.
    """
    text = (stdout or "").strip()
    if not text:
        return CodingAgentResult(success=False, error="empty coding-agent output")
    try:
        obj = json.loads(text)
    except (json.JSONDecodeError, ValueError):
        # The CLI sometimes prints a banner before the JSON object; try
        # the last line, then give up gracefully.
        last = text.splitlines()[-1].strip()
        try:
            obj = json.loads(last)
        except (json.JSONDecodeError, ValueError):
            return CodingAgentResult(
                success=False, text=text[:2000], error="unparseable coding-agent output"
            )
    if not isinstance(obj, dict):
        return CodingAgentResult(success=False, error="unexpected output shape")

    result_text = str(obj.get("result", "") or "")
    usage = obj.get("usage") or {}
    input_tokens = int(usage.get("input_tokens", 0) or 0)
    output_tokens = int(usage.get("output_tokens", 0) or 0)
    cost = float(obj.get("total_cost_usd", 0.0) or 0.0)
    is_error = bool(obj.get("is_error", False))
    subtype_ok = obj.get("subtype", "success") == "success"
    success = subtype_ok and not is_error
    refs = PR_RE.findall(result_text)
    return CodingAgentResult(
        text=result_text,
        success=success,
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        cost_usd=cost,
        session_id=str(obj.get("session_id", "") or ""),
        delivered_refs=refs,
        error="" if success else (str(obj.get("error", "")) or result_text[:500]),
    )


class ClaudeCodeRunner(DetachedFileRunner):
    """Runs Claude Code headless in a sandbox; returns a CodingAgentResult."""

    name = "claude-code"

    def _build_command(
        self,
        brief: str,
        limits: RunLimits,
        *,
        llm: CodingAgentLLM | None = None,
        mcp_config_path: str = "",
    ) -> str:
        # Claude Code takes the bare model id; its endpoint + key come from
        # the sandbox env (ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY).
        return build_claude_command(
            brief,
            model=llm.model if llm else "",
            limits=limits,
            mcp_config_path=mcp_config_path,
        )

    def _parse_result(self, stdout: str) -> CodingAgentResult:
        return parse_claude_result(stdout)
