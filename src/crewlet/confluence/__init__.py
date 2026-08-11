"""Confluence page publishing — generic page ops + the unified import CLI.

This package owns everything Crewlet pushes *into* Confluence:

- :mod:`crewlet.confluence.pages` — generic, content-agnostic page
  operations (find / create / update / label / space ops) over a
  :class:`ConfluenceTransport`'s authenticated HTTP client.
- :mod:`crewlet.confluence.knowledge` — knowledge-doc specifics (clean
  prose encoding, ``crewlet-doc`` labelling; the directory convention —
  space = parent directory, title = first ``# H1``, optional frontmatter
  for ``title``/``parent``/``labels`` overrides only — is parsed by the
  backend-neutral :mod:`crewlet.knowledge.markdown_docs`).
- :mod:`crewlet.confluence.import_cli` — the unified ``crewlet confluence
  import`` / ``resync`` orchestrator that routes each markdown file to a
  Tool Skill page (frontmatter ``trigger``) or, otherwise, a knowledge
  doc in its parent-directory space.
- :mod:`crewlet.confluence.promotion` — the Confluence
  ``PromotionPageWriter`` (skill-promotion drafts under the unit's
  ``Auto-Drafted Skills`` parent), built on the :mod:`pages` helpers.

Reading Confluence at query time (the agent-facing search) lives
separately in :mod:`crewlet.knowledge`; this package is the write side.
"""

from __future__ import annotations

from crewlet.confluence.knowledge import (
    DOC_MARKER_LABEL,
    doc_key_label,
    encode_doc,
)
from crewlet.confluence.pages import (
    ConfluencePublishError,
    attach_labels,
    build_transport,
    check_space_exists,
    create_page,
    create_space,
    find_by_label,
    find_by_title,
    safe_label,
    update_page,
)

__all__ = [
    "DOC_MARKER_LABEL",
    "ConfluencePublishError",
    "attach_labels",
    "build_transport",
    "check_space_exists",
    "create_page",
    "create_space",
    "doc_key_label",
    "encode_doc",
    "find_by_label",
    "find_by_title",
    "safe_label",
    "update_page",
]
