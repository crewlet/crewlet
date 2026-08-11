"""Render the scoped MCP surface the in-sandbox coding agent gets.

The coding agent is itself an MCP client, so the runner hands it a
**scoped** config: only the role's servers named in
``role.sandbox.mcp.servers``, each with the role's credentials. Scoping is
at the server level only — there is no per-tool allowlist, so the agent
gets every tool the scoped servers expose (OpenCode has no allowlist flag
and Claude Code runs ``bypassPermissions`` headless, so a curated allowlist
couldn't be enforced uniformly). This module is the pure translation from
the engine's ``MCPServerConfig`` + ``role.mcp_env`` into the generic
launch-spec shape the runners write into the sandbox (Claude Code's
``.mcp.json`` / OpenCode's ``opencode.json``). Credentials are resolved
here (``${ENV}`` expanded like ``mcp_env``) so the in-sandbox server can
authenticate.
"""

from __future__ import annotations

from collections.abc import Sequence
from typing import Any

from crewlet.config import MCPServerConfig, resolve_env_vars


def to_launch_spec(
    cfg: MCPServerConfig, role_creds: dict[str, str] | None = None
) -> dict[str, Any]:
    """One server's generic launch spec, role creds merged + ``${ENV}`` resolved.

    HTTP servers (e.g. the GitHub remote MCP) render as
    ``{"type": "http", "url", "headers"}`` with the role's ``mcp_env``
    entries as headers; stdio servers render as ``{"command", "args",
    "env"}`` with the role's entries merged into ``env``.
    """
    role_creds = role_creds or {}
    if cfg.transport == "http":
        headers = resolve_env_vars({**cfg.headers, **role_creds})
        spec: dict[str, Any] = {"type": "http", "url": cfg.url}
        if headers:
            spec["headers"] = headers
        return spec
    env = resolve_env_vars({**cfg.env, **role_creds})
    spec = {"command": cfg.command, "args": list(cfg.args)}
    if env:
        spec["env"] = env
    return spec


def resolve_sandbox_mcp(
    server_configs: Sequence[MCPServerConfig],
    role_mcp_env: dict[str, dict[str, str]] | None,
    servers: Sequence[str],
) -> dict[str, dict[str, Any]]:
    """Build the server launch specs for a role's scoped sandbox MCP surface.

    Only the ``servers`` the role scoped to the sandbox are included, and a
    server with no matching ``MCPServerConfig`` is skipped (the agent never
    sees a server the engine doesn't know). Scoping is server-level only —
    the agent gets every tool those servers expose.
    """
    by_name = {c.name: c for c in server_configs}
    role_mcp_env = role_mcp_env or {}
    specs: dict[str, dict[str, Any]] = {}
    for name in servers:
        cfg = by_name.get(name)
        if cfg is None:
            continue
        specs[name] = to_launch_spec(cfg, role_mcp_env.get(name, {}))
    return specs


__all__ = ["resolve_sandbox_mcp", "to_launch_spec"]
