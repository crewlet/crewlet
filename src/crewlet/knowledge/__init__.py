"""Knowledge retrieval -- the query-time knowledge-base search seam.

:mod:`crewlet.knowledge.protocol` defines the backend-neutral
``KnowledgeSearcher`` protocol and ``KnowledgeHit`` model the
``## Relevant knowledge`` prefetch consumes.  The backends implement
it: :mod:`crewlet.knowledge.confluence_search` (Confluence CQL) and
:mod:`crewlet.knowledge.plane_search` (Plane fork page search).
:mod:`crewlet.knowledge.accessibility` holds the org-wide read scope
both backends resolve behind the seam (``accessible_spaces`` /
``accessible_projects``).

:mod:`crewlet.knowledge.markdown_docs` is the write-side counterpart:
the backend-neutral markdown-document helpers (frontmatter/H1 parsing,
skill-vs-doc classification, markdown rendering, HTML flattening, the
frontmatter YAML dump) shared by the Confluence and Plane publishers.
"""
