"""Organization model — the foundational data model for the company."""

from crewlet.org.hierarchy import (
    get_ancestors,
    get_effective_lead,
    get_lead_depth,
    get_manager,
    get_reports,
    get_unit_chain_for_role,
    get_unit_for_role,
    is_lead_of_unit,
    is_unit_lead,
)
from crewlet.org.models import (
    HumanContact,
    Organization,
    OrgUnit,
    OrgUnitType,
    Role,
    RoleKind,
)

__all__ = [
    "HumanContact",
    "OrgUnit",
    "OrgUnitType",
    "Organization",
    "Role",
    "RoleKind",
    "get_ancestors",
    "get_effective_lead",
    "get_lead_depth",
    "get_manager",
    "get_reports",
    "get_unit_chain_for_role",
    "get_unit_for_role",
    "is_lead_of_unit",
    "is_unit_lead",
]
