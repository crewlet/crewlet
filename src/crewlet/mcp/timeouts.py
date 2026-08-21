"""Deadlines for talking to an MCP server.

An MCP server is another program — a subprocess over stdio, or a remote
HTTP endpoint — and the failure mode that matters is not an error but a
*silence*.  A server that launches and never completes the handshake,
or answers ``tools/list`` and then never returns from a tool call,
raises nothing at all.  Every ``try/except`` around it is dead code
while it hangs.

That is why these are here rather than left to the caller.  On the
stdio path there is no other ceiling: the SDK's HTTP transport applies
its own read timeout, but a subprocess has nothing between the engine
and an ``await`` that never returns.  The engine starts MCP servers on
the seat-acquisition path, so one silent server does not merely lose
its own tools — it holds up every seat behind it, for the life of the
process.

This module imports nothing from Crewlet so that both the config layer
(which exposes these as per-server overrides) and the two clients
(which enforce them) can share one definition.  Two copies of a
deadline is how a transport ends up with a ceiling its config says it
does not have.
"""

from __future__ import annotations

#: Ceiling on connect + handshake + first ``tools/list``.
#:
#: Sized for the slow *healthy* case rather than the fast one: a
#: ``uvx`` / ``npx`` server whose package is not in the local cache
#: downloads it on first launch, which on a modest link is tens of
#: seconds for a Python package with compiled dependencies.  Two
#: minutes clears that comfortably while still being a deadline a
#: booting operator will actually see, instead of a boot that appears
#: to have finished and quietly never did.
DEFAULT_MCP_STARTUP_TIMEOUT_SECONDS = 120.0

#: Ceiling on one ``tools/call``.
#:
#: Matched deliberately to the MCP SDK's own SSE-friendly HTTP read
#: default (300 s), so a tool behaves the same whether its server is
#: reached over stdio or over HTTP.  Picking a different number here
#: would mean the same tool has two different ceilings depending on a
#: transport the agent calling it cannot see.
DEFAULT_MCP_REQUEST_TIMEOUT_SECONDS = 300.0

__all__ = [
    "DEFAULT_MCP_REQUEST_TIMEOUT_SECONDS",
    "DEFAULT_MCP_STARTUP_TIMEOUT_SECONDS",
]
