"""Crewlet's MCP client identity for the protocol handshake."""

from __future__ import annotations

from importlib.metadata import PackageNotFoundError, version

from mcp.types import Implementation


def crewlet_client_info() -> Implementation:
    """The ``Implementation`` Crewlet advertises when connecting to MCP
    servers, so server-side logs attribute sessions to ``crewlet`` rather
    than the SDK's anonymous default client info."""
    try:
        pkg_version = version("crewlet")
    except PackageNotFoundError:  # running from a source tree, not installed
        pkg_version = "0.0.0"
    return Implementation(name="crewlet", version=pkg_version)
