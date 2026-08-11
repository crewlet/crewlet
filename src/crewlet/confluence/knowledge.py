"""Knowledge-doc specifics for the unified Confluence publisher.

A *knowledge doc* is a general Confluence page (onboarding, a runbook, a
playbook) published by ``crewlet confluence import`` and surfaced to
agents through the query-time ``## Relevant knowledge`` search — NOT a
Tool Skill.  The two differ on the wire:

- Tool Skills carry a leading YAML ``code`` macro holding binding
  metadata (``trigger`` / ``phases`` / ``required``), because the engine
  parses that metadata back out of the page.
- Knowledge docs are **clean prose**: the page is read by an agent's
  knowledge search exactly as a human would read it, so there is no
  metadata macro on the page.

Knowledge docs use a **directory-based convention** — they are pure
prose, no frontmatter required:

- the target **space** is the source file's immediate parent directory
  name (a file at ``<root>/LEAD/onboarding.md`` publishes to space
  ``LEAD``), resolved by the import CLI from the path (NOT read here);
- the page **title** is the file's first ATX ``# H1`` heading, and that
  H1 line is stripped from the published body so the page doesn't repeat
  the title Confluence already shows separately.

Optional frontmatter is still supported for overrides only:

- ``title`` — wins over the H1;
- ``parent`` — parent page title, resolved by exact title in the target
  space at create time (a missing parent falls back to the space root
  with a hint log; an existing page is never re-parented);
- ``labels`` — extra labels.

(``space`` is never read from frontmatter — it always comes from the
directory.)

The backend-neutral parsing helpers (frontmatter split, H1 title
extraction, skill-vs-doc classification) live in
:mod:`crewlet.knowledge.markdown_docs` — shared with the Plane
publisher.  This module keeps only what is Confluence-specific: the
marker labels and the clean-prose storage encoding.

These docs are not loaded into any in-memory registry — there is nothing
to resync.  They are searched live (see ``docs/concepts/knowledge-system.md``).
"""

from __future__ import annotations

from crewlet.confluence.pages import safe_label
from crewlet.knowledge.markdown_docs import render_markdown

DOC_MARKER_LABEL = "crewlet-doc"
"""Marker label every published knowledge doc carries.

The doc analogue of ``crewlet-skill``: a convenience tag for operators
browsing a space and a stable filter target.  Also the prefix base for
the per-doc key label produced by :func:`doc_key_label`.
"""


def encode_doc(body_markdown: str) -> str:
    """Render a knowledge-doc body to Confluence storage XHTML.

    Clean prose — no YAML ``code`` macro.  Uses the same markdown
    renderer the skill codec uses so doc pages and skill bodies convert
    identically.
    """
    return render_markdown(body_markdown)


def doc_key_label(space: str, title: str) -> str:
    """Confluence-safe per-doc key label keyed by ``(space, title)``."""
    return safe_label(f"{space}:{title}", prefix=f"{DOC_MARKER_LABEL}-key")


__all__ = [
    "DOC_MARKER_LABEL",
    "doc_key_label",
    "encode_doc",
]
