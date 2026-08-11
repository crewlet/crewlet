"""Confluence label conventions for tool-skill pages.

Every imported skill page is tagged with the shared
:data:`SKILL_MARKER_LABEL` so the sync worker can filter the walk
to skill pages only — the space's auto-generated home page and any
other non-skill content an operator drops in the space get ignored
without triggering a decode-error log.  Imported pages also carry a
per-skill ``{prefix}-key-<safe>`` label that encodes the skill key
into Confluence's restricted label charset for idempotent lookup by
``crewlet confluence import``; the marker label is used for filtering,
the key label is used for lookup.
"""

from __future__ import annotations

SKILL_MARKER_LABEL = "crewlet-skill"
"""Marker label every skill page carries.

Filter target for :class:`~crewlet.agent.skills.sync.ToolSkillSyncWorker`
(via CQL ``label="crewlet-skill"``) and a convenience tag for operators
browsing the Tool Skills space.  Also serves as the prefix for the
per-skill key label produced by
:func:`~crewlet.confluence.import_cli._skill_key_label`.
"""

__all__ = ["SKILL_MARKER_LABEL"]
