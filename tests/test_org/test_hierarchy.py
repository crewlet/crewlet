"""Tests for hierarchy traversal utilities."""

from crewlet.org.hierarchy import (
    get_ancestors,
    get_effective_lead,
    get_lead_depth,
    get_manager,
    get_reports,
    get_unit_chain_for_role,
    get_unit_for_role,
    is_unit_lead,
)
from crewlet.org.models import Organization, OrgUnit, Role


def make_org() -> Organization:
    """Create a sample org for testing."""
    return Organization(
        name="Acme AI",
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                lead="VP Engineering",
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        lead="VP Engineering",
                        roles=[
                            Role(
                                name="VP Engineering",
                                manages=["Tech Lead"],
                            ),
                            Role(
                                name="Tech Lead",
                                manages=[
                                    "Senior Engineer A",
                                    "Senior Engineer B",
                                    "Junior Engineer",
                                ],
                            ),
                            Role(name="Senior Engineer A"),
                            Role(name="Senior Engineer B"),
                            Role(name="Junior Engineer"),
                        ],
                    ),
                ],
            ),
            OrgUnit(
                name="Product",
                type="department",
                children=[
                    OrgUnit(
                        name="PM Team",
                        type="team",
                        lead="Product Manager",
                        roles=[Role(name="Product Manager")],
                    ),
                ],
            ),
        ],
    )


def test_get_manager():
    org = make_org()
    senior = org.get_role("Senior Engineer A")
    assert senior is not None
    manager = get_manager(senior, org)
    assert manager is not None
    assert manager.name == "Tech Lead"


def test_get_manager_top_level():
    org = make_org()
    vp = org.get_role("VP Engineering")
    assert vp is not None
    manager = get_manager(vp, org)
    assert manager is None


def test_get_manager_no_manager():
    org = make_org()
    pm = org.get_role("Product Manager")
    assert pm is not None
    manager = get_manager(pm, org)
    assert manager is None


def test_get_reports():
    org = make_org()
    lead = org.get_role("Tech Lead")
    assert lead is not None
    reports = get_reports(lead, org)
    names = [r.name for r in reports]
    assert "Senior Engineer A" in names
    assert "Senior Engineer B" in names
    assert "Junior Engineer" in names
    assert len(reports) == 3


def test_get_reports_no_reports():
    org = make_org()
    junior = org.get_role("Junior Engineer")
    assert junior is not None
    reports = get_reports(junior, org)
    assert reports == []


def test_get_ancestors():
    org = make_org()
    junior = org.get_role("Junior Engineer")
    assert junior is not None
    ancestors = get_ancestors(junior, org)
    names = [r.name for r in ancestors]
    assert names == ["Tech Lead", "VP Engineering"]


def test_get_ancestors_top_level():
    org = make_org()
    vp = org.get_role("VP Engineering")
    assert vp is not None
    ancestors = get_ancestors(vp, org)
    assert ancestors == []


def test_get_unit_for_role():
    org = make_org()
    senior = org.get_role("Senior Engineer A")
    assert senior is not None
    unit = get_unit_for_role(senior, org)
    assert unit is not None
    assert unit.name == "Backend"


def test_get_unit_for_role_different_unit():
    org = make_org()
    pm = org.get_role("Product Manager")
    assert pm is not None
    unit = get_unit_for_role(pm, org)
    assert unit is not None
    assert unit.name == "PM Team"


def test_get_unit_chain_for_role():
    org = make_org()
    senior = org.get_role("Senior Engineer A")
    assert senior is not None
    chain = get_unit_chain_for_role(senior, org)
    names = [u.name for u in chain]
    assert names == ["Engineering", "Backend"]


def test_get_unit_chain_for_role_different_unit():
    org = make_org()
    pm = org.get_role("Product Manager")
    assert pm is not None
    chain = get_unit_chain_for_role(pm, org)
    names = [u.name for u in chain]
    assert names == ["Product", "PM Team"]


def test_is_unit_lead_true():
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Lead",
                roles=[
                    Role(name="Lead"),
                    Role(name="Dev"),
                ],
            ),
        ],
    )
    lead_role = org.get_role("Lead")
    assert lead_role is not None
    assert is_unit_lead(lead_role, org) is True


def test_is_unit_lead_false():
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Lead",
                roles=[
                    Role(name="Lead"),
                    Role(name="Dev"),
                ],
            ),
        ],
    )
    dev_role = org.get_role("Dev")
    assert dev_role is not None
    assert is_unit_lead(dev_role, org) is False


def test_get_lead_depth():
    org = make_org()
    vp = org.get_role("VP Engineering")
    assert vp is not None
    # VP Engineering is lead at depth 0 (Engineering) and depth 1 (Backend)
    depth = get_lead_depth(vp, org)
    assert depth == 0  # returns shallowest


def test_get_lead_depth_not_lead():
    org = make_org()
    junior = org.get_role("Junior Engineer")
    assert junior is not None
    depth = get_lead_depth(junior, org)
    assert depth == -1


def test_lead_manages_auto_inferred_flat_unit():
    """Unit lead auto-manages all roles when manages is not specified."""
    unit = OrgUnit(
        name="Backend",
        type="team",
        lead="Lead",
        roles=[
            Role(name="Lead"),
            Role(name="Dev A"),
            Role(name="Dev B"),
        ],
    )
    lead = unit.get_lead()
    assert lead is not None
    assert sorted(lead.manages) == ["Dev A", "Dev B"]


def test_lead_manages_auto_inferred_skips_already_managed():
    """Auto-inference doesn't claim roles already managed by someone else."""
    unit = OrgUnit(
        name="Backend",
        type="team",
        lead="VP",
        roles=[
            Role(name="VP"),
            Role(name="Tech Lead", manages=["Dev A", "Dev B"]),
            Role(name="Dev A"),
            Role(name="Dev B"),
        ],
    )
    vp = unit.get_lead()
    assert vp is not None
    # VP should only auto-manage Tech Lead (Dev A/B are managed by Tech Lead)
    assert vp.manages == ["Tech Lead"]


def test_lead_manages_explicit_not_overwritten():
    """Explicit manages on the lead role is preserved."""
    unit = OrgUnit(
        name="Backend",
        type="team",
        lead="Lead",
        roles=[
            Role(name="Lead", manages=["Dev A"]),
            Role(name="Dev A"),
            Role(name="Dev B"),
        ],
    )
    lead = unit.get_lead()
    assert lead is not None
    # Dev A was explicit, Dev B was auto-inferred
    assert sorted(lead.manages) == ["Dev A", "Dev B"]


def test_lead_manages_single_role_unit():
    """A unit with only the lead has no one to manage."""
    unit = OrgUnit(
        name="Solo",
        type="team",
        lead="PM",
        roles=[Role(name="PM")],
    )
    lead = unit.get_lead()
    assert lead is not None
    assert lead.manages == []


def test_lead_manages_no_cycle_when_role_manages_lead():
    """Don't auto-add a role that already manages the lead."""
    unit = OrgUnit(
        name="Backend",
        type="team",
        lead="Senior Engineer A",
        roles=[
            Role(name="Tech Lead", manages=["Senior Engineer A"]),
            Role(name="Senior Engineer A"),
        ],
    )
    lead = unit.get_lead()
    assert lead is not None
    # Tech Lead manages the lead, so it must NOT be auto-added
    assert lead.manages == []


def test_deep_nesting():
    """Test org with 3+ levels of nesting."""
    org = Organization(
        name="BigCorp",
        units=[
            OrgUnit(
                name="Technology",
                type="division",
                children=[
                    OrgUnit(
                        name="Engineering",
                        type="department",
                        children=[
                            OrgUnit(
                                name="Platform",
                                type="team",
                                lead="Platform Lead",
                                roles=[
                                    Role(name="Platform Lead"),
                                    Role(name="Platform Dev"),
                                ],
                            ),
                        ],
                    ),
                ],
            ),
        ],
    )
    dev = org.get_role("Platform Dev")
    assert dev is not None
    chain = get_unit_chain_for_role(dev, org)
    names = [u.name for u in chain]
    assert names == ["Technology", "Engineering", "Platform"]

    unit = get_unit_for_role(dev, org)
    assert unit is not None
    assert unit.name == "Platform"


def test_root_level_role_no_unit():
    """Root-level roles return None for get_unit_for_role."""
    org = Organization(
        name="Test",
        roles=[Role(name="CEO", manages=["Dev"])],
        units=[
            OrgUnit(
                name="Team",
                type="team",
                lead="Dev",
                roles=[Role(name="Dev")],
            ),
        ],
    )
    ceo = org.get_role("CEO")
    assert ceo is not None
    unit = get_unit_for_role(ceo, org)
    assert unit is None


def test_root_level_role_empty_unit_chain():
    """Root-level roles return empty unit chain."""
    org = Organization(
        name="Test",
        roles=[Role(name="CEO")],
    )
    ceo = org.get_role("CEO")
    assert ceo is not None
    chain = get_unit_chain_for_role(ceo, org)
    assert chain == []


def test_root_level_role_is_not_unit_lead():
    """Root-level roles are not unit leads."""
    org = Organization(
        name="Test",
        roles=[Role(name="CEO")],
    )
    ceo = org.get_role("CEO")
    assert ceo is not None
    assert is_unit_lead(ceo, org) is False
    assert get_lead_depth(ceo, org) == -1


def test_root_level_role_as_manager():
    """Root-level role can be a manager discovered via get_manager."""
    org = Organization(
        name="Test",
        roles=[Role(name="CEO", manages=["VP"])],
        units=[
            OrgUnit(
                name="Eng",
                type="team",
                lead="VP",
                roles=[Role(name="VP", manages=["Dev"]), Role(name="Dev")],
            ),
        ],
    )
    vp = org.get_role("VP")
    assert vp is not None
    manager = get_manager(vp, org)
    assert manager is not None
    assert manager.name == "CEO"


def test_root_level_role_ancestors():
    """get_ancestors from a unit role climbs through root-level manager."""
    org = Organization(
        name="Test",
        roles=[Role(name="CEO", manages=["VP"])],
        units=[
            OrgUnit(
                name="Eng",
                type="team",
                lead="VP",
                roles=[Role(name="VP", manages=["Dev"]), Role(name="Dev")],
            ),
        ],
    )
    dev = org.get_role("Dev")
    assert dev is not None
    ancestors = get_ancestors(dev, org)
    names = [r.name for r in ancestors]
    assert names == ["VP", "CEO"]


def test_flat_org():
    """Test org with a single flat unit (no nesting)."""
    org = Organization(
        name="Startup",
        units=[
            OrgUnit(
                name="Team",
                type="team",
                lead="Founder",
                roles=[
                    Role(name="Founder", manages=["Dev 1", "Dev 2"]),
                    Role(name="Dev 1"),
                    Role(name="Dev 2"),
                ],
            ),
        ],
    )
    assert len(org.all_roles()) == 3
    dev = org.get_role("Dev 1")
    assert dev is not None
    manager = get_manager(dev, org)
    assert manager is not None
    assert manager.name == "Founder"

    chain = get_unit_chain_for_role(dev, org)
    assert len(chain) == 1
    assert chain[0].name == "Team"


# --- Lead inheritance from parent unit ---


def test_child_inherits_lead_from_parent():
    """A child unit with no lead inherits the parent's lead."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                lead="VP Eng",
                roles=[Role(name="VP Eng")],
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        roles=[Role(name="Dev A"), Role(name="Dev B")],
                    ),
                ],
            ),
        ],
    )
    backend = org.get_unit("Backend")
    assert backend is not None
    assert backend.lead == "VP Eng"


def test_child_explicit_lead_not_overwritten():
    """A child with its own lead is not overwritten by the parent."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                lead="VP Eng",
                roles=[Role(name="VP Eng")],
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        lead="Tech Lead",
                        roles=[Role(name="Tech Lead"), Role(name="Dev")],
                    ),
                ],
            ),
        ],
    )
    backend = org.get_unit("Backend")
    assert backend is not None
    assert backend.lead == "Tech Lead"


def test_inherited_lead_auto_manages_child_roles():
    """An inherited lead auto-manages unmanaged roles in the child unit."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                lead="VP Eng",
                roles=[Role(name="VP Eng")],
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        roles=[Role(name="Dev A"), Role(name="Dev B")],
                    ),
                ],
            ),
        ],
    )
    vp = org.get_role("VP Eng")
    assert vp is not None
    assert "Dev A" in vp.manages
    assert "Dev B" in vp.manages


def test_inherited_lead_skips_already_managed():
    """Inherited lead auto-management skips roles managed by someone else."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                lead="VP Eng",
                roles=[Role(name="VP Eng")],
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        roles=[
                            Role(name="Tech Lead", manages=["Dev A"]),
                            Role(name="Dev A"),
                            Role(name="Dev B"),
                        ],
                    ),
                ],
            ),
        ],
    )
    vp = org.get_role("VP Eng")
    assert vp is not None
    # Dev A is managed by Tech Lead, so VP should only auto-manage Tech Lead and Dev B
    assert "Tech Lead" in vp.manages
    assert "Dev B" in vp.manages
    assert "Dev A" not in vp.manages


def test_inherited_lead_cascades_multiple_levels():
    """Lead inheritance cascades through multiple levels of nesting."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Technology",
                type="division",
                lead="CTO",
                roles=[Role(name="CTO")],
                children=[
                    OrgUnit(
                        name="Engineering",
                        type="department",
                        children=[
                            OrgUnit(
                                name="Backend",
                                type="team",
                                roles=[Role(name="Dev")],
                            ),
                        ],
                    ),
                ],
            ),
        ],
    )
    eng = org.get_unit("Engineering")
    assert eng is not None
    assert eng.lead == "CTO"

    backend = org.get_unit("Backend")
    assert backend is not None
    assert backend.lead == "CTO"


def test_get_effective_lead_local():
    """get_effective_lead returns local lead when it exists in the unit."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Backend",
                type="team",
                lead="Lead",
                roles=[Role(name="Lead"), Role(name="Dev")],
            ),
        ],
    )
    backend = org.get_unit("Backend")
    assert backend is not None
    lead = get_effective_lead(backend, org)
    assert lead is not None
    assert lead.name == "Lead"


def test_get_effective_lead_inherited():
    """get_effective_lead finds an inherited lead via org-wide lookup."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                lead="VP Eng",
                roles=[Role(name="VP Eng")],
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        roles=[Role(name="Dev A"), Role(name="Dev B")],
                    ),
                ],
            ),
        ],
    )
    backend = org.get_unit("Backend")
    assert backend is not None
    lead = get_effective_lead(backend, org)
    assert lead is not None
    assert lead.name == "VP Eng"


def test_get_effective_lead_no_lead():
    """get_effective_lead returns None when no lead exists anywhere."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Backend",
                type="team",
                roles=[Role(name="Dev")],
            ),
        ],
    )
    backend = org.get_unit("Backend")
    assert backend is not None
    assert get_effective_lead(backend, org) is None


def test_inherited_lead_is_unit_lead():
    """An inherited lead is recognized as lead of the child unit."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                lead="VP Eng",
                roles=[Role(name="VP Eng")],
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        roles=[Role(name="Dev")],
                    ),
                ],
            ),
        ],
    )
    vp = org.get_role("VP Eng")
    assert vp is not None
    assert is_unit_lead(vp, org) is True


def test_inherited_lead_survives_serialization_roundtrip():
    """Inherited lead persists through model_dump / model_validate."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                lead="VP Eng",
                roles=[Role(name="VP Eng")],
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        roles=[Role(name="Dev A"), Role(name="Dev B")],
                    ),
                ],
            ),
        ],
    )
    data = org.model_dump()
    restored = Organization.model_validate(data)

    backend = restored.get_unit("Backend")
    assert backend is not None
    assert backend.lead == "VP Eng"

    lead = get_effective_lead(backend, restored)
    assert lead is not None
    assert lead.name == "VP Eng"


def test_inherited_lead_unresolved_does_not_raise():
    """Organization accepts (with a warning) a unit whose lead doesn't
    yet resolve to a role.  Live config management bootstraps the org
    in pieces via the per-entity API -- ``POST /config/units`` lands
    before ``POST /config/roles`` -- so the engine has to apply the
    intermediate revision and let the next role POST complete the
    picture.  ``get_lead()`` / ``get_effective_lead()`` already treat
    a dangling reference as ``None``; downstream code is defensive.
    """
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                lead="Ghost",
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        roles=[Role(name="Dev")],
                    ),
                ],
            ),
        ],
    )

    # Lead reference is preserved untouched on the unit.
    assert org.units[0].lead == "Ghost"
    # Downstream lookups treat the dangling reference as "no lead".
    assert org.units[0].get_lead() is None
    assert org.get_role("Ghost") is None
    # The real role (Backend / Dev) still resolves.
    backend = org.units[0].children[0]
    assert backend.get_role("Dev") is not None
