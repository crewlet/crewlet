"""Tests for organization models."""

from crewlet.org.models import Organization, OrgUnit, Role


def make_org() -> Organization:
    """Create a sample org for testing."""
    return Organization(
        name="Acme AI",
        mission="Build the best AI widgets",
        vision="AI for everyone",
        policies=["Code review required", "Communicate in writing"],
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                purpose="Build products",
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        goals=["API reliability above 99.9%"],
                        lead="Tech Lead",
                        roles=[
                            Role(
                                name="Tech Lead",
                                goal="Coordinate backend team",
                                manages=[
                                    "Senior Engineer A",
                                    "Senior Engineer B",
                                    "Junior Engineer",
                                ],
                            ),
                            Role(
                                name="Senior Engineer A",
                                goal="Design and build APIs",
                            ),
                            Role(
                                name="Senior Engineer B",
                                goal="Design and build APIs",
                            ),
                            Role(
                                name="Junior Engineer",
                                goal="Fix bugs",
                            ),
                        ],
                    ),
                ],
            ),
            OrgUnit(
                name="Product",
                type="department",
                children=[
                    OrgUnit(
                        name="Product Management",
                        type="team",
                        lead="Product Manager",
                        roles=[
                            Role(
                                name="Product Manager",
                                goal="Define product strategy",
                            ),
                        ],
                    ),
                ],
            ),
        ],
    )


def test_organization_basic_fields():
    org = make_org()
    assert org.name == "Acme AI"
    assert org.mission == "Build the best AI widgets"
    assert len(org.policies) == 2


def test_organization_get_unit():
    org = make_org()
    eng = org.get_unit("Engineering")
    assert eng is not None
    assert eng.name == "Engineering"
    assert org.get_unit("Nonexistent") is None


def test_unit_get_child():
    org = make_org()
    eng = org.get_unit("Engineering")
    assert eng is not None
    backend = eng.get_child("Backend")
    assert backend is not None
    assert backend.name == "Backend"
    assert eng.get_child("Nonexistent") is None


def test_unit_get_role():
    org = make_org()
    backend = org.get_unit("Backend")
    assert backend is not None
    lead = backend.get_role("Tech Lead")
    assert lead is not None
    assert lead.goal == "Coordinate backend team"
    assert backend.get_role("Nonexistent") is None


def test_all_roles():
    org = make_org()
    roles = org.all_roles()
    names = [r.name for r in roles]
    assert "Tech Lead" in names
    assert "Senior Engineer A" in names
    assert "Senior Engineer B" in names
    assert "Junior Engineer" in names
    assert "Product Manager" in names
    assert len(roles) == 5


def test_get_role():
    org = make_org()
    lead = org.get_role("Tech Lead")
    assert lead is not None
    assert lead.manages == ["Senior Engineer A", "Senior Engineer B", "Junior Engineer"]
    assert org.get_role("Nonexistent") is None


def test_role_defaults():
    role = Role(name="Test")
    assert role.goal == ""
    assert role.backstory == ""
    assert role.responsibilities == []
    assert role.manages == []
    assert role.behavioral_guidelines == []
    assert role.llm == "default"


def test_role_with_all_fields():
    role = Role(
        name="Lead",
        goal="Lead the team",
        backstory="10 years experience",
        responsibilities=["Coordinate", "Review"],
        manages=["Engineer"],
        behavioral_guidelines=["Be concise"],
        llm="gpt-4o",
    )
    assert role.backstory == "10 years experience"
    assert len(role.responsibilities) == 2
    assert role.behavioral_guidelines == ["Be concise"]
    assert role.llm == "gpt-4o"


def test_role_behavioral_guidelines():
    role = Role(
        name="Engineer",
        behavioral_guidelines=["Always write tests", "Be concise in communication"],
    )
    assert len(role.behavioral_guidelines) == 2
    assert "Always write tests" in role.behavioral_guidelines


def test_role_behavioral_guidelines_default_empty():
    role = Role(name="Test")
    assert role.behavioral_guidelines == []


def test_unit_get_lead_explicit():
    """Explicit lead field returns the named role."""
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
    assert lead.name == "Senior Engineer A"


def test_unit_with_unresolved_lead_accepted():
    """Organization accepts a unit whose lead does not match any role
    (logs a warning instead of raising).  Required for per-entity
    bootstrap via the live-config API where ``POST /config/units``
    can land before the matching ``POST /config/roles``."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Solo",
                type="team",
                lead="Nonexistent",
                roles=[Role(name="Engineer")],
            ),
        ],
    )
    assert org.units[0].lead == "Nonexistent"
    # The dangling reference resolves to None for downstream callers.
    assert org.units[0].get_lead() is None


def test_organization_serialization():
    org = make_org()
    data = org.model_dump()
    restored = Organization.model_validate(data)
    assert restored.name == org.name
    assert len(restored.all_roles()) == len(org.all_roles())


def test_unit_all_roles_includes_descendants():
    """OrgUnit.all_roles() includes roles from children."""
    unit = OrgUnit(
        name="Engineering",
        type="department",
        roles=[Role(name="VP Eng")],
        children=[
            OrgUnit(
                name="Backend",
                type="team",
                lead="Dev",
                roles=[Role(name="Dev")],
            ),
        ],
    )
    names = [r.name for r in unit.all_roles()]
    assert "VP Eng" in names
    assert "Dev" in names


def test_unit_lead_in_descendants():
    """A unit lead can reference a role in descendant units."""
    unit = OrgUnit(
        name="Engineering",
        type="department",
        lead="Dev Lead",
        children=[
            OrgUnit(
                name="Backend",
                type="team",
                lead="Dev Lead",
                roles=[Role(name="Dev Lead"), Role(name="Dev")],
            ),
        ],
    )
    lead = unit.get_lead()
    assert lead is not None
    assert lead.name == "Dev Lead"


def test_root_level_roles_included_in_all_roles():
    """Root-level roles are included in Organization.all_roles()."""
    org = Organization(
        name="Test",
        roles=[
            Role(name="CEO", goal="Run the company"),
            Role(name="CTO", goal="Tech strategy"),
        ],
        units=[
            OrgUnit(
                name="Engineering",
                type="team",
                lead="Dev",
                roles=[Role(name="Dev")],
            ),
        ],
    )
    all_names = [r.name for r in org.all_roles()]
    assert "CEO" in all_names
    assert "CTO" in all_names
    assert "Dev" in all_names
    assert len(org.all_roles()) == 3


def test_root_level_roles_get_role():
    """Organization.get_role() finds root-level roles."""
    org = Organization(
        name="Test",
        roles=[Role(name="CEO", goal="Run the company")],
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
    assert ceo.goal == "Run the company"

    dev = org.get_role("Dev")
    assert dev is not None


def test_root_level_roles_manages_hierarchy():
    """Root-level roles can manage unit roles via manages[]."""
    org = Organization(
        name="Test",
        roles=[
            Role(name="CEO", manages=["VP Eng"]),
        ],
        units=[
            OrgUnit(
                name="Engineering",
                type="team",
                lead="VP Eng",
                roles=[
                    Role(name="VP Eng", manages=["Dev"]),
                    Role(name="Dev"),
                ],
            ),
        ],
    )
    from crewlet.org.hierarchy import get_ancestors, get_manager

    vp = org.get_role("VP Eng")
    assert vp is not None
    manager = get_manager(vp, org)
    assert manager is not None
    assert manager.name == "CEO"

    dev = org.get_role("Dev")
    assert dev is not None
    ancestors = get_ancestors(dev, org)
    names = [r.name for r in ancestors]
    assert names == ["VP Eng", "CEO"]


def test_root_level_roles_only_org():
    """An org can have only root-level roles with no units."""
    org = Organization(
        name="Solo",
        roles=[
            Role(name="Founder", manages=["Assistant"]),
            Role(name="Assistant"),
        ],
    )
    assert len(org.all_roles()) == 2
    assert org.get_role("Founder") is not None
    assert org.get_role("Assistant") is not None
    assert len(org.all_units()) == 0


def test_root_level_roles_serialization():
    """Root-level roles survive model_dump / model_validate round-trip."""
    org = Organization(
        name="Test",
        roles=[Role(name="CEO")],
        units=[
            OrgUnit(
                name="Team",
                type="team",
                lead="Dev",
                roles=[Role(name="Dev")],
            ),
        ],
    )
    data = org.model_dump()
    restored = Organization.model_validate(data)
    assert len(restored.roles) == 1
    assert restored.roles[0].name == "CEO"
    assert len(restored.all_roles()) == 2


def test_root_level_roles_to_api_dict():
    """to_api_dict() includes root-level roles."""
    org = Organization(
        name="Test",
        roles=[Role(name="CEO", goal="Lead")],
    )
    api = org.to_api_dict()
    assert "roles" in api
    assert len(api["roles"]) == 1
    assert api["roles"][0]["name"] == "CEO"


def test_to_api_dict_no_root_roles():
    """to_api_dict() includes empty roles list when no root-level roles."""
    org = Organization(name="Test")
    api = org.to_api_dict()
    assert api["roles"] == []


def test_all_units():
    org = make_org()
    units = org.all_units()
    names = [u.name for u in units]
    assert "Engineering" in names
    assert "Backend" in names
    assert "Product" in names
    assert "Product Management" in names
    assert len(units) == 4


# --- manages: unit-name expansion ---


def test_manages_unit_name_expands_to_roles():
    """A manages entry matching a unit name expands to all roles in that unit."""
    org = Organization(
        name="Test",
        roles=[
            Role(name="CEO", manages=["Backend"]),
        ],
        units=[
            OrgUnit(
                name="Backend",
                type="team",
                lead="Lead",
                roles=[
                    Role(name="Lead", manages=["Dev A", "Dev B"]),
                    Role(name="Dev A"),
                    Role(name="Dev B"),
                ],
            ),
        ],
    )
    ceo = org.get_role("CEO")
    assert ceo is not None
    assert set(ceo.manages) == {"Lead", "Dev A", "Dev B"}


def test_manages_unit_name_excludes_self():
    """When a role lives inside the unit it manages, it is excluded."""
    org = Organization(
        name="Test",
        units=[
            OrgUnit(
                name="Team",
                type="team",
                roles=[
                    Role(name="Lead", manages=["Team"]),
                    Role(name="Dev A"),
                    Role(name="Dev B"),
                ],
            ),
        ],
    )
    lead = org.get_role("Lead")
    assert lead is not None
    assert "Lead" not in lead.manages
    assert set(lead.manages) == {"Dev A", "Dev B"}


def test_manages_unit_name_with_descendants():
    """Unit-name expansion includes roles from nested child units."""
    org = Organization(
        name="Test",
        roles=[
            Role(name="CTO", manages=["Engineering"]),
        ],
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                roles=[Role(name="VP Eng")],
                children=[
                    OrgUnit(
                        name="Backend",
                        type="team",
                        lead="Dev",
                        roles=[Role(name="Dev")],
                    ),
                ],
            ),
        ],
    )
    cto = org.get_role("CTO")
    assert cto is not None
    assert set(cto.manages) == {"VP Eng", "Dev"}


def test_manages_role_name_takes_priority_over_unit():
    """When a name matches both a role and a unit, it is kept as a role reference."""
    org = Organization(
        name="Test",
        roles=[
            # Role named same as a unit
            Role(name="Backend", goal="Cross-cutting backend advisor"),
            Role(name="CEO", manages=["Backend"]),
        ],
        units=[
            OrgUnit(
                name="Backend",
                type="team",
                lead="Dev",
                roles=[Role(name="Dev")],
            ),
        ],
    )
    ceo = org.get_role("CEO")
    assert ceo is not None
    # Should remain as the role reference, not expanded
    assert ceo.manages == ["Backend"]


def test_manages_mixed_roles_and_units():
    """manages can mix explicit role names and unit names."""
    org = Organization(
        name="Test",
        roles=[
            Role(name="CEO", manages=["CTO", "Product"]),
            Role(name="CTO"),
        ],
        units=[
            OrgUnit(
                name="Product",
                type="team",
                lead="PM",
                roles=[
                    Role(name="PM"),
                    Role(name="Designer"),
                ],
            ),
        ],
    )
    ceo = org.get_role("CEO")
    assert ceo is not None
    assert ceo.manages[0] == "CTO"  # explicit role kept first
    assert set(ceo.manages[1:]) == {"PM", "Designer"}


def test_manages_unknown_entry_kept():
    """Unknown manages entries are kept as-is (no silent removal)."""
    org = Organization(
        name="Test",
        roles=[Role(name="CEO", manages=["Ghost"])],
    )
    ceo = org.get_role("CEO")
    assert ceo is not None
    assert ceo.manages == ["Ghost"]


def test_manages_no_duplicates_after_expansion():
    """If a role is already listed explicitly, unit expansion skips it."""
    org = Organization(
        name="Test",
        roles=[
            Role(name="CEO", manages=["Dev", "Backend"]),
        ],
        units=[
            OrgUnit(
                name="Backend",
                type="team",
                lead="Dev",
                roles=[
                    Role(name="Dev", manages=["Junior"]),
                    Role(name="Junior"),
                ],
            ),
        ],
    )
    ceo = org.get_role("CEO")
    assert ceo is not None
    # Dev appears explicitly before unit expansion — should not be duplicated
    assert ceo.manages.count("Dev") == 1
    assert "Junior" in ceo.manages
