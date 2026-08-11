"""Sandboxed coding-agent runtime (``crewlet.sandbox``).

Lets a sandbox-enabled role do real code work through the
``run_sandbox`` Execute tool: a coding agent (Claude Code / OpenCode)
works in an isolated sandbox (E2B) and the Execute loop suspends until
it finishes. See ``docs/concepts/code-sandbox.md``.
"""

from __future__ import annotations

from crewlet.sandbox.fake import (
    FakeCodingAgentRunner,
    FakeSandbox,
    FakeSandboxProvider,
)
from crewlet.sandbox.manager import SandboxConfigError, SandboxManager
from crewlet.sandbox.protocol import (
    CodingAgentResult,
    CodingAgentRunner,
    ExecResult,
    RunHandle,
    RunLimits,
    Sandbox,
    SandboxProvider,
    SandboxSpec,
)

__all__ = [
    "CodingAgentResult",
    "CodingAgentRunner",
    "ExecResult",
    "FakeCodingAgentRunner",
    "FakeSandbox",
    "FakeSandboxProvider",
    "RunHandle",
    "RunLimits",
    "Sandbox",
    "SandboxConfigError",
    "SandboxManager",
    "SandboxProvider",
    "SandboxSpec",
]
