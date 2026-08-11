"""GitLab integration — per-agent service-account provisioning.

The engine-side GitLab integration (webhook routing, identity
registration, MCP tool surface) lives in ``crewlet.config`` /
``crewlet.notifications`` / ``crewlet.api``; this package is the
**provisioning** half: an idempotent reconcile that makes GitLab match
the company config (one service account per agent seat, membership,
per-agent PATs, webhooks). Driven by ``crewlet gitlab provision``.

See ``docs/integrations/gitlab.md``.
"""

from __future__ import annotations

from crewlet.gitlab.client import (
    ACCESS_LEVELS,
    GitLabClient,
    GitLabProvisionError,
    build_participants_fetcher,
)
from crewlet.gitlab.provision import (
    ENGINE_ACCOUNT_HANDLE,
    GitLabProvisionAborted,
    ProvisionReport,
    SeatResult,
    provision,
    seat_token_vars,
)

__all__ = [
    "ACCESS_LEVELS",
    "ENGINE_ACCOUNT_HANDLE",
    "GitLabClient",
    "GitLabProvisionAborted",
    "GitLabProvisionError",
    "build_participants_fetcher",
    "ProvisionReport",
    "SeatResult",
    "provision",
    "seat_token_vars",
]
