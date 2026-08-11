"""Hierarchy traversal and permission resolution utilities."""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from crewlet.org.models import Organization, OrgUnit, Role


def get_manager(role: Role, org: Organization) -> Role | None:
    """Find the manager of a role (the role that lists this role in its manages)."""
    for candidate in org.all_roles():
        if role.name in candidate.manages:
            return candidate
    return None


def get_reports(role: Role, org: Organization) -> list[Role]:
    """Find all direct reports of a role."""
    reports = []
    for name in role.manages:
        found = org.get_role(name)
        if found is not None:
            reports.append(found)
    return reports


def get_ancestors(role: Role, org: Organization) -> list[Role]:
    """Get the management chain from this role up to the top.

    Returns a list starting with the direct manager and ending with
    the topmost manager (who has no manager).
    """
    ancestors = []
    current = role
    seen = {current.name}
    while True:
        manager = get_manager(current, org)
        if manager is None or manager.name in seen:
            break
        ancestors.append(manager)
        seen.add(manager.name)
        current = manager
    return ancestors


def get_unit_for_role(role: Role, org: Organization) -> OrgUnit | None:
    """Find the OrgUnit that directly contains this role."""
    for unit in org.units:
        found = _find_unit_containing_role(unit, role.name)
        if found is not None:
            return found
    return None


def get_unit_chain_for_role(role: Role, org: Organization) -> list[OrgUnit]:
    """Find the chain of OrgUnits from outermost to the unit containing the role.

    Returns ``[grandparent_unit, parent_unit, direct_unit]`` — ordered
    from the top-level structure entry down to the unit that directly
    contains the role. Returns an empty list if the role is not found.
    """
    for unit in org.units:
        chain: list[OrgUnit] = []
        if _build_unit_chain(unit, role.name, chain):
            chain.reverse()  # built innermost-first, reverse to outermost-first
            return chain
    return []


def is_unit_lead(role: Role, org: Organization) -> bool:
    """Check whether *role* is the designated lead of any unit in the org.

    A role can be lead of a parent unit even if it physically lives in
    a child unit (e.g. a department lead whose role is in a team).
    """
    return get_lead_depth(role, org) >= 0


def get_effective_lead(unit: OrgUnit, org: Organization) -> Role | None:
    """Return the effective lead for a unit, including inherited leads.

    When a unit's lead is a direct role or descendant role,
    ``unit.get_lead()`` resolves it.  When the lead was inherited from
    a parent unit (and therefore lives outside this unit's subtree),
    this function falls back to an org-wide role lookup.
    """
    if not unit.lead:
        return None
    # Fast path: lead is in this unit's own roles / descendants.
    lead = unit.get_lead()
    if lead is not None:
        return lead
    # Inherited lead — find the role anywhere in the org.
    return org.get_role(unit.lead)


def is_lead_of_unit(role: Role, unit: OrgUnit) -> bool:
    """Check whether *role* is the designated lead of a specific unit."""
    return unit.lead == role.name


def get_lead_depth(role: Role, org: Organization) -> int:
    """Return the depth at which this role is a lead, or -1 if not a lead.

    Depth 0 = lead of a top-level structure entry (like a division/dept).
    Depth 1 = lead of a child unit. And so on.
    A lower depth means higher authority.
    """
    for unit in org.units:
        depth = _find_lead_depth(unit, role.name, 0)
        if depth >= 0:
            return depth
    return -1


# --- Private helpers ---


def _find_unit_containing_role(unit: OrgUnit, role_name: str) -> OrgUnit | None:
    """Find the unit whose direct ``roles`` list contains *role_name*."""
    for r in unit.roles:
        if r.name == role_name:
            return unit
    for child in unit.children:
        found = _find_unit_containing_role(child, role_name)
        if found is not None:
            return found
    return None


def _build_unit_chain(unit: OrgUnit, role_name: str, chain: list[OrgUnit]) -> bool:
    """Build the chain of units from *unit* down to the one containing *role_name*.

    Appends to *chain* in reverse order (innermost first) during recursion,
    then the caller reverses once at the end. This avoids O(n) inserts.
    Returns True if found.
    """
    # Check if role is directly in this unit
    for r in unit.roles:
        if r.name == role_name:
            chain.append(unit)
            return True
    # Recurse into children
    for child in unit.children:
        if _build_unit_chain(child, role_name, chain):
            chain.append(unit)  # append parent after child (reversed later)
            return True
    return False


def _find_lead_depth(unit: OrgUnit, role_name: str, depth: int) -> int:
    """Find the depth at which *role_name* is a lead. Returns -1 if not found."""
    if unit.lead == role_name:
        return depth
    for child in unit.children:
        result = _find_lead_depth(child, role_name, depth + 1)
        if result >= 0:
            return result
    return -1
