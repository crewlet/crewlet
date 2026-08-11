"""Local provisioning ledger — which Slack app belongs to which agent.

``crewlet slack provision`` persists one :class:`SlackAppRecord` per
agent handle in a JSON file (default: ``slack-apps.json`` next to the
company YAML).  The ledger is what makes re-runs idempotent: a handle
with a recorded ``app_id`` gets ``apps.manifest.update`` instead of a
duplicate ``create``.

The file also keeps the per-app credentials Slack only hands out once
(at ``apps.manifest.create``): the client id/secret needed to redo the
OAuth install later (token lost/revoked), and the signing secret so a
lost ``.env`` can be healed on the next run.  It is therefore a secrets
file — it is written ``0600`` and must be gitignored, exactly like
``.env``.
"""

from __future__ import annotations

import json
from pathlib import Path

from pydantic import BaseModel, ConfigDict, Field

from crewlet._logging import get_logger
from crewlet.slack.envfile import write_secret_file

logger = get_logger("slack.state")


class SlackAppRecord(BaseModel):
    """One provisioned Slack app (one agent seat)."""

    model_config = ConfigDict(extra="ignore")

    app_id: str
    client_id: str = ""
    client_secret: str = ""
    signing_secret: str = ""
    bot_user_id: str = ""
    team_id: str = ""
    manifest_hash: str = ""
    """SHA-256 of the last manifest successfully pushed to Slack —
    lets re-runs skip ``apps.manifest.update`` when nothing changed
    (manifest methods are Tier 1 rate-limited, ~1+/min)."""


class ProvisionState(BaseModel):
    """The full ledger: agent handle → provisioned app."""

    model_config = ConfigDict(extra="ignore")

    apps: dict[str, SlackAppRecord] = Field(default_factory=dict)


def load_state(path: Path) -> ProvisionState:
    """Read the ledger at *path*; a missing file is an empty ledger."""
    if not path.exists():
        return ProvisionState()
    data = json.loads(path.read_text(encoding="utf-8"))
    return ProvisionState.model_validate(data)


def save_state(path: Path, state: ProvisionState) -> None:
    """Write the ledger at *path* atomically, owner-only (0600).

    Atomicity matters more here than for most files: the ledger holds
    client secrets Slack returns exactly once at app creation, so a
    truncate-then-write interrupted mid-way would destroy them
    unrecoverably.  ``write_secret_file`` goes through a 0600 temp file
    + ``os.replace``.
    """
    write_secret_file(
        path, json.dumps(state.model_dump(), indent=2, sort_keys=True) + "\n"
    )
    logger.debug("slack_state_saved", path=str(path), apps=len(state.apps))
