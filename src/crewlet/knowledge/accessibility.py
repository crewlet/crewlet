"""Accessibility -- the optional org-wide knowledge read scope.

The read scope for the query-time knowledge search (the ``## Relevant
knowledge`` prefetch) is an org-wide container list and nothing else:
``Organization.confluence_spaces`` for the Confluence backend,
``Organization.plane_projects`` for the Plane backend.  It is an
**optional narrowing**: when empty, a role that authenticates as its
own backend user searches *unscoped* and the backend's own ACLs bound
the hits (a credential-less role, on the shared engine credential,
searches nothing rather than the whole instance).

Per-team container *identity* -- the space / project a unit owns for
webhook routing and as its write / skill-promotion home -- is a
separate concern (``OrgUnit.confluence_space`` / ``Role.confluence_space``
and ``OrgUnit.plane_project`` / ``Role.plane_project``, authored under
``integrations.confluence.space`` / ``integrations.plane.project``).
It deliberately does **not** narrow reads: an agent searches across
everything its own backend account can read, not just its team's.
"""

from __future__ import annotations

from crewlet.org.models import Organization


def accessible_spaces(org: Organization) -> set[str]:
    """Return the org-wide Confluence read-scope set (uppercased, deduped).

    This is ``Organization.confluence_spaces`` normalised -- the optional
    ``space IN (...)`` narrowing for the query-time search.  The set is
    intentionally **role-independent**: per-team space identity is
    integration config, not read scope.

    An empty set means "no explicit scope": a self-authenticating role
    then searches unscoped (ACL-bound), while a credential-less role
    searches nothing (see :mod:`crewlet.knowledge.confluence_search`).
    """
    return {k.strip().upper() for k in org.confluence_spaces if k and k.strip()}


def accessible_projects(org: Organization) -> set[str]:
    """Return the org-wide Plane read-scope set (uppercased, deduped).

    This is ``Organization.plane_projects`` normalised -- the optional
    project narrowing for the query-time Plane page search.  Identical
    contract to :func:`accessible_spaces`: intentionally
    **role-independent** (per-team project identity is integration
    config, not read scope).

    An empty set means "no explicit scope": a self-authenticating role
    then searches unscoped (Plane's own project membership bounds the
    hits), while a credential-less role searches nothing.
    """
    return {p.strip().upper() for p in org.plane_projects if p and p.strip()}
