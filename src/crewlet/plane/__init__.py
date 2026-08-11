"""Plane integration — REST client, page codec, and the import CLI.

The engine-side Plane integration (config models, webhook route,
notification transport, identity registration) lives in
``crewlet.config`` / ``crewlet.notifications`` / ``crewlet.api``; this
package is the **REST + publishing** half:

- :mod:`crewlet.plane.client` — the thin async ``X-API-Key`` client the
  ``PlaneTransport`` drives for webhook enrichment (subscriber lookups,
  the project id→identifier map) plus the fork's pages CRUD / page
  search surface the knowledge machinery consumes and the provisioning
  write endpoints (service accounts + token lifecycle, members, project
  membership, workspace webhooks).
- :mod:`crewlet.plane.provision` — the ``crewlet plane provision``
  reconcile: one service account per agent seat, project membership,
  ``${VAR}``-minted tokens, the ``crewlet-engine`` read account, and
  the workspace webhook with its server-generated secret captured into
  the config's own reference.
- :mod:`crewlet.plane.plane_codec` — markdown ↔ ``description_html``
  round-trip for engine-published pages (the Plane analog of the
  Confluence storage codec).
- :mod:`crewlet.plane.import_cli` — the unified ``crewlet plane
  import`` / ``resync`` orchestrator (skills + knowledge docs,
  ``external_id``-keyed idempotency, positive-marker prune).
- :mod:`crewlet.plane.promotion` — the Plane ``PromotionPageWriter``
  (skill-promotion drafts under the unit project's ensured
  ``Auto-Drafted Skills`` parent page).

See ``docs/integrations/plane.md``.
"""

from __future__ import annotations

from crewlet.plane.client import PlaneClient, PlaneProvisionError
from crewlet.plane.provision import (
    ENGINE_ACCOUNT_HANDLE,
    SERVICE_ACCOUNT_ROLES,
    PlaneProvisionAborted,
    ProvisionReport,
    SeatResult,
    provision,
    seat_token_vars,
)

__all__ = [
    "ENGINE_ACCOUNT_HANDLE",
    "SERVICE_ACCOUNT_ROLES",
    "PlaneClient",
    "PlaneProvisionAborted",
    "PlaneProvisionError",
    "ProvisionReport",
    "SeatResult",
    "provision",
    "seat_token_vars",
]
