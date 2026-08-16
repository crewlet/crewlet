"""Tests for config loading."""

from pathlib import Path
from typing import get_args
from unittest.mock import patch

import pytest
import yaml
from pydantic import ValidationError

from crewlet.config import (
    CompanyConfig,
    JiraConfig,
    LLMProviderType,
    build_plane_project_lead_map,
    build_space_key_lead_map,
    config_to_organization,
    load_company_config,
    load_organization,
)
from crewlet.org.models import Organization, OrgUnit, Role

FIXTURES = Path(__file__).parent / "fixtures"


def test_load_full_config():
    config = load_company_config(FIXTURES / "company.yaml")
    assert config.name == "Acme AI Corp"
    assert config.mission == "Build intelligent products that help people"
    assert len(config.policies) == 2
    assert len(config.units) == 2


def test_load_minimal_config():
    config = load_company_config(FIXTURES / "minimal.yaml")
    assert config.name == "Minimal Corp"
    assert len(config.units) == 1


def test_load_config_file_not_found():
    with pytest.raises(FileNotFoundError):
        load_company_config("/nonexistent/path.yaml")


def test_load_config_missing_name(tmp_path: Path):
    bad = tmp_path / "bad.yaml"
    bad.write_text("mission: test\n")
    with pytest.raises(ValueError, match="must have a 'name' field"):
        load_company_config(bad)


def test_load_config_invalid_yaml(tmp_path: Path):
    bad = tmp_path / "bad.yaml"
    bad.write_text("- just a list\n")
    with pytest.raises(ValueError, match="YAML mapping"):
        load_company_config(bad)


def test_config_to_organization():
    config = load_company_config(FIXTURES / "company.yaml")
    org = config_to_organization(config)
    assert org.name == "Acme AI Corp"
    assert len(org.units) == 2

    eng = org.get_unit("Engineering")
    assert eng is not None
    backend = eng.get_child("Backend")
    assert backend is not None
    assert len(backend.roles) == 3
    assert backend.lead == "Tech Lead"
    assert backend.get_lead() is not None
    assert backend.get_lead().name == "Tech Lead"

    lead = backend.get_role("Tech Lead")
    assert lead is not None
    assert lead.manages == ["Senior Engineer", "Junior Engineer"]
    assert lead.llm == "default"

    senior = backend.get_role("Senior Engineer")
    assert senior is not None

    junior = backend.get_role("Junior Engineer")
    assert junior is not None
    assert junior.llm == "budget"


def test_config_providers():
    config = load_company_config(FIXTURES / "company.yaml")
    assert "default" in config.providers.llm
    assert config.providers.llm["default"].model == "gpt-4o"
    assert "budget" in config.providers.llm
    assert config.providers.llm["budget"].model == "gpt-4o-mini"
    # Tier A providers (queue / database / knowledge) live on
    # BootstrapProvidersConfig — see test_bootstrap_config.


def test_config_extensions():
    config = load_company_config(FIXTURES / "company.yaml")
    assert len(config.extensions) == 2


def test_load_organization_convenience():
    org = load_organization(FIXTURES / "company.yaml")
    assert org.name == "Acme AI Corp"
    roles = org.all_roles()
    names = [r.name for r in roles]
    assert "Tech Lead" in names
    assert "Product Manager" in names


def test_organization_hierarchy_from_config():
    from crewlet.org.hierarchy import get_manager, get_reports

    org = load_organization(FIXTURES / "company.yaml")
    lead = org.get_role("Tech Lead")
    assert lead is not None
    reports = get_reports(lead, org)
    assert len(reports) == 2

    senior = org.get_role("Senior Engineer")
    assert senior is not None
    manager = get_manager(senior, org)
    assert manager is not None
    assert manager.name == "Tech Lead"


def test_unit_lead_from_yaml(tmp_path: Path):
    """Unit lead field is parsed from YAML config."""
    yaml_content = """\
name: Test Corp
units:
  - name: Core
    type: team
    lead: Lead Dev
    roles:
      - name: Lead Dev
        manages: [Dev]
      - name: Dev
"""
    config_file = tmp_path / "config.yaml"
    config_file.write_text(yaml_content)
    org = load_organization(config_file)

    core = org.get_unit("Core")
    assert core is not None
    assert core.lead == "Lead Dev"
    lead = core.get_lead()
    assert lead is not None
    assert lead.name == "Lead Dev"


def test_root_role_with_unit_ref_is_auto_managed_by_lead():
    """A root role carrying ``unit:`` must be moved into the unit BEFORE
    lead auto-management runs, so the unit lead ends up managing it.

    Regression guard: if the move happened after ``_validate_lead`` ran,
    the role would be orphaned from the hierarchy (the lead would never
    gain a ``manages`` entry)."""
    cfg = CompanyConfig(
        name="Acme",
        units=[{"name": "Eng", "lead": "Lead", "roles": [{"name": "Lead"}]}],
        roles=[{"name": "Dev", "unit": "Eng"}],
    )
    org = config_to_organization(cfg)

    eng = org.get_unit("Eng")
    assert eng is not None
    assert eng.get_role("Dev") is not None
    assert "Dev" in org.get_role("Lead").manages


def test_root_role_with_unit_ref_inherits_unit_mcp_env():
    """The moved role inherits the unit's mcp_env (unit base, role
    override) — without the move it would miss per-role MCP launch."""
    cfg = CompanyConfig(
        name="Acme",
        units=[
            {
                "name": "Eng",
                "mcp_env": {"atlassian": {"JIRA_URL": "https://x"}},
                "roles": [],
            }
        ],
        roles=[
            {
                "name": "Dev",
                "unit": "Eng",
                "mcp_env": {"atlassian": {"JIRA_USERNAME": "dev"}},
            }
        ],
    )
    org = config_to_organization(cfg)
    dev = org.get_unit("Eng").get_role("Dev")
    assert dev.mcp_env["atlassian"] == {
        "JIRA_URL": "https://x",
        "JIRA_USERNAME": "dev",
    }


def test_config_to_organization_does_not_mutate_source():
    """Re-materialising a CompanyConfig must be idempotent — the unit
    reference resolution works on copies, never the stored payload."""
    cfg = CompanyConfig(
        name="Acme",
        units=[{"name": "Eng", "roles": []}],
        roles=[{"name": "Dev", "unit": "Eng"}],
    )
    config_to_organization(cfg)
    config_to_organization(cfg)  # a second pass must not double-move

    # Source payload untouched: Dev still at root, unit still empty.
    assert cfg.units[0].roles == []
    assert [r.name for r in cfg.roles] == ["Dev"]


def test_per_role_learning_enabled_is_parsed():
    """``learning_enabled`` on a role must survive parsing — it was
    silently dropped by ``_parse_role`` before."""
    cfg = CompanyConfig(
        name="Acme",
        roles=[{"name": "Noisy", "learning_enabled": False}],
    )
    org = config_to_organization(cfg)
    assert org.get_role("Noisy").learning_enabled is False


class TestTokenBudgetConfig:
    """Tests for token budget configuration."""

    def test_company_config_default_budget(self):
        config = CompanyConfig(name="X")
        assert config.token_budget == 0

    def test_company_config_with_budget(self):
        config = CompanyConfig(name="X", token_budget=100000)
        assert config.token_budget == 100000

    def test_role_token_budget_from_yaml(self, tmp_path: Path):
        yaml_content = """\
name: Test Corp
token_budget: 500000
units:
  - name: Core
    type: team
    lead: Lead
    roles:
      - name: Engineer
        token_budget: 50000
      - name: Intern
        token_budget: 10000
      - name: Lead
"""
        config_file = tmp_path / "config.yaml"
        config_file.write_text(yaml_content)
        config = load_company_config(config_file)
        assert config.token_budget == 500000

        org = config_to_organization(config)
        engineer = org.get_role("Engineer")
        assert engineer is not None
        assert engineer.token_budget == 50000

        intern = org.get_role("Intern")
        assert intern is not None
        assert intern.token_budget == 10000

        lead = org.get_role("Lead")
        assert lead is not None
        assert lead.token_budget == 0


class TestJiraConfig:
    """Tests for top-level jira configuration."""

    def test_jira_config_none_by_default(self):
        config = CompanyConfig(name="X")
        assert config.integrations.jira is None

    def test_jira_config_from_dict(self):
        config = CompanyConfig(
            name="X",
            integrations={
                "jira": {
                    "url": "https://test.atlassian.net",
                    "token": "admin-tok",
                    "email": "bot@test.com",
                    "webhook_secret": "sec",
                }
            },
        )
        assert config.integrations.jira is not None
        assert config.integrations.jira.url == "https://test.atlassian.net"
        assert config.integrations.jira.token == "admin-tok"
        assert config.integrations.jira.email == "bot@test.com"
        assert config.integrations.jira.webhook_secret == "sec"

    def test_jira_config_requires_token(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError):
            CompanyConfig(name="X", integrations={"jira": {"url": "https://x.com"}})

    def test_jira_config_requires_url_or_cloud_id(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError, match="url.*cloud_id"):
            CompanyConfig(name="X", integrations={"jira": {"token": "tok"}})

    def test_jira_config_rejects_both_url_and_cloud_id(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError, match="not both"):
            JiraConfig(
                url="https://x.com",
                cloud_id="abc-123",
                token="tok",
            )

    def test_jira_config_email_optional(self):
        config = CompanyConfig(
            name="X",
            integrations={"jira": {"url": "https://x.com", "token": "tok"}},
        )
        assert config.integrations.jira is not None
        assert config.integrations.jira.email == ""

    def test_jira_config_cloud_id(self):
        """cloud_id constructs the gateway base URL."""
        cfg = JiraConfig(cloud_id="ac0da8ad-1234", token="tok")
        assert cfg.base_url == ("https://api.atlassian.com/ex/jira/ac0da8ad-1234")
        assert cfg.url == ""

    def test_jira_config_url_base_url(self):
        """url is returned directly as base_url."""
        cfg = JiraConfig(url="https://my.atlassian.net", token="tok")
        assert cfg.base_url == "https://my.atlassian.net"
        assert cfg.cloud_id == ""

    def test_jira_config_env_var_preserved_at_load(self, tmp_path: Path):
        """``${VAR}`` references are preserved verbatim by the loader.

        Resolution happens at provider / transport construction time
        in the engine builders — keeps the loaded CompanyConfig free
        of resolved secrets for safe DB storage and YAML export.
        """
        import os

        os.environ["TEST_JIRA_URL"] = "https://resolved.atlassian.net"
        os.environ["TEST_JIRA_TOKEN"] = "resolved-token"
        try:
            yaml_content = """\
name: Test Corp
integrations:
  jira:
    url: "${TEST_JIRA_URL}"
    token: "${TEST_JIRA_TOKEN}"
"""
            config_file = tmp_path / "config.yaml"
            config_file.write_text(yaml_content)
            config = load_company_config(config_file)
            assert config.integrations.jira is not None
            assert config.integrations.jira.url == "${TEST_JIRA_URL}"
            assert config.integrations.jira.token == "${TEST_JIRA_TOKEN}"
        finally:
            del os.environ["TEST_JIRA_URL"]
            del os.environ["TEST_JIRA_TOKEN"]

    def test_jira_config_cloud_id_env_var_preserved(self, tmp_path: Path):
        """``${VAR}`` references in ``cloud_id`` are preserved at load.

        ``base_url`` interpolation against the unresolved cloud_id is
        the expected behaviour — engine builders call
        ``_resolved_for_runtime(cfg)`` before constructing transports.
        """
        import os

        os.environ["TEST_CLOUD_ID"] = "abc-cloud-id"
        os.environ["TEST_JIRA_TOKEN2"] = "tok"
        try:
            yaml_content = """\
name: Test Corp
integrations:
  jira:
    cloud_id: "${TEST_CLOUD_ID}"
    token: "${TEST_JIRA_TOKEN2}"
"""
            config_file = tmp_path / "config.yaml"
            config_file.write_text(yaml_content)
            config = load_company_config(config_file)
            assert config.integrations.jira is not None
            assert config.integrations.jira.cloud_id == "${TEST_CLOUD_ID}"
            assert config.integrations.jira.token == "${TEST_JIRA_TOKEN2}"
        finally:
            del os.environ["TEST_CLOUD_ID"]
            del os.environ["TEST_JIRA_TOKEN2"]

    def test_jira_config_creates_transport(self):
        """JiraConfig can be used to construct a JiraTransport."""
        cfg = JiraConfig(
            url="https://test.atlassian.net",
            token="tok",
            webhook_secret="sec",
        )
        from crewlet.notifications.transports.jira import (
            JiraTransport,
        )

        t = JiraTransport(cfg)
        assert t._jira_url == "https://test.atlassian.net"
        assert t._jira_token == "tok"
        assert t._webhook_secret == "sec"

    def test_jira_config_cloud_id_creates_transport(self):
        """cloud_id-based config produces correct URL in transport."""
        cfg = JiraConfig(cloud_id="my-cloud-id", token="tok")
        from crewlet.notifications.transports.jira import (
            JiraTransport,
        )

        t = JiraTransport(cfg)
        assert t._jira_url == ("https://api.atlassian.com/ex/jira/my-cloud-id")

    def test_jira_config_rejects_mcp_block(self):
        """A ``jira.mcp`` sub-block is a hard error — MCP tool servers
        are configured under ``mcp_servers``, not inside ``jira``."""
        from pydantic import ValidationError

        with pytest.raises(ValidationError):
            JiraConfig(
                url="https://test.atlassian.net",
                token="tok",
                mcp={"command": "uvx", "args": ["mcp-atlassian"]},
            )

    def test_jira_config_env_var_preserved(self, tmp_path: Path):
        """``${VAR}`` refs in jira.* survive the loader unresolved."""
        import os

        os.environ["TEST_JIRA_URL_MCP"] = "https://resolved.atlassian.net"
        os.environ["TEST_JIRA_TOKEN_MCP"] = "resolved-token"
        try:
            yaml_content = """\
name: Test Corp
integrations:
  jira:
    url: "${TEST_JIRA_URL_MCP}"
    token: "${TEST_JIRA_TOKEN_MCP}"
"""
            config_file = tmp_path / "config.yaml"
            config_file.write_text(yaml_content)
            config = load_company_config(config_file)
            assert config.integrations.jira is not None
            assert config.integrations.jira.url == "${TEST_JIRA_URL_MCP}"
        finally:
            del os.environ["TEST_JIRA_URL_MCP"]
            del os.environ["TEST_JIRA_TOKEN_MCP"]


class TestSlackConfig:
    """Tests for the slack transport-enable marker (integrations.slack)."""

    def test_slack_config_none_by_default(self):
        config = CompanyConfig(name="X")
        assert config.integrations.slack is None

    def test_slack_marker_enables_transport(self):
        """``integrations.slack: {}`` is a presence marker that turns on
        the SlackTransport; the Slack MCP tool server lives in
        mcp_servers."""
        config = CompanyConfig(name="X", integrations={"slack": {}})
        assert config.integrations.slack is not None

    def test_slack_config_rejects_mcp_block(self):
        """A ``slack.mcp`` sub-block is a hard error — MCP tool servers
        are configured under ``mcp_servers``, not inside ``slack``."""
        from pydantic import ValidationError

        with pytest.raises(ValidationError):
            CompanyConfig(
                name="X",
                integrations={
                    "slack": {"mcp": {"command": "npm", "args": ["exec", "slack-mcp"]}}
                },
            )

    def test_status_phrases_default_to_the_builtin_pools(self):
        from crewlet.notifications.typing_status import PHASE_PHRASES

        config = CompanyConfig(name="X", integrations={"slack": {}})
        assert config.integrations.slack is not None
        phrases = config.integrations.slack.status_phrases.as_status_phrases()
        assert phrases.pools == PHASE_PHRASES

    def test_status_phrases_override_only_the_named_phases(self):
        from crewlet.notifications.typing_status import PHASE_PHRASES

        config = CompanyConfig(
            name="X",
            integrations={"slack": {"status_phrases": {"plan": ["is nimbusing..."]}}},
        )
        assert config.integrations.slack is not None
        phrases = config.integrations.slack.status_phrases.as_status_phrases()
        assert phrases.pick("plan", seed="s", rotation=0) == "is nimbusing..."
        assert phrases.pools["execute"] == PHASE_PHRASES["execute"]

    def test_status_phrases_reject_a_blank_entry(self):
        """A blank phrase would post a *cleared* indicator — the opposite
        of what the operator meant."""
        from pydantic import ValidationError

        with pytest.raises(ValidationError):
            CompanyConfig(
                name="X",
                integrations={"slack": {"status_phrases": {"plan": ["  "]}}},
            )

    def test_status_phrases_reject_an_unknown_phase(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError):
            CompanyConfig(
                name="X",
                integrations={"slack": {"status_phrases": {"planning": ["is x..."]}}},
            )


class TestIntegrationsAndKnowledge:
    """The inbound ``integrations:`` block + the ``knowledge:`` block."""

    def test_integrations_default_empty(self):
        cfg = CompanyConfig(name="X")
        assert cfg.integrations.jira is None
        assert cfg.integrations.confluence is None
        assert cfg.integrations.slack is None
        assert cfg.integrations.github is None
        assert cfg.integrations.forge_app_id == ""

    def test_integrations_block_parses(self):
        cfg = CompanyConfig(
            name="X",
            integrations={
                "jira": {"url": "https://x.atlassian.net", "token": "t"},
                "confluence": {"url": "https://x.atlassian.net/wiki", "token": "t"},
                "slack": {},
                "github": {"enabled": True, "webhook_secret": "s"},
                "forge_app_id": "ari:cloud:app/1",
            },
        )
        assert cfg.integrations.jira.base_url == "https://x.atlassian.net"
        assert cfg.integrations.confluence is not None
        assert cfg.integrations.slack is not None
        assert cfg.integrations.github.enabled is True
        assert cfg.integrations.forge_app_id == "ari:cloud:app/1"

    def test_integrations_rejects_unknown_key(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError):
            CompanyConfig(name="X", integrations={"jiraa": {"url": "x", "token": "t"}})

    def test_knowledge_confluence_spaces_flows_to_org(self):
        cfg = CompanyConfig(
            name="X", knowledge={"confluence_spaces": ["HANDBOOK", "ENG"]}
        )
        org = config_to_organization(cfg)
        assert org.confluence_spaces == ["HANDBOOK", "ENG"]

    def test_knowledge_default_empty(self):
        org = config_to_organization(CompanyConfig(name="X"))
        assert org.confluence_spaces == []
        assert org.plane_projects == []


class TestPlaneConfig:
    """The ``integrations.plane`` block, its validators, and the
    knowledge-backend exclusivity rule (Confluence XOR Plane)."""

    def _plane(self, **overrides):
        block = {
            "enabled": True,
            "url": "https://plane.nimbus.example",
            "workspace": "nimbus",
            "webhook_secret": "${PLANE_WEBHOOK_SECRET}",
        }
        block.update(overrides)
        return block

    def test_plane_config_none_by_default(self):
        assert CompanyConfig(name="X").integrations.plane is None

    def test_plane_config_parses(self):
        cfg = CompanyConfig(
            name="X",
            integrations={
                "plane": self._plane(
                    token="${PLANE_ENGINE_TOKEN}",
                    provisioning={
                        "role": "member",
                        "roles": {"tech-lead": "admin"},
                        "username_prefix": "agent-",
                        "projects": ["LEAD", "ENG", "PROD", "TS"],
                        "token_expiry_days": 364,
                    },
                ),
            },
        )
        plane = cfg.integrations.plane
        assert plane is not None
        assert plane.enabled is True
        assert plane.url == "https://plane.nimbus.example"
        assert plane.workspace == "nimbus"
        assert plane.webhook_secret == "${PLANE_WEBHOOK_SECRET}"
        assert plane.token == "${PLANE_ENGINE_TOKEN}"
        assert plane.provisioning is not None
        assert plane.provisioning.role == "member"
        assert plane.provisioning.roles == {"tech-lead": "admin"}
        assert plane.provisioning.username_prefix == "agent-"
        assert plane.provisioning.projects == ["LEAD", "ENG", "PROD", "TS"]
        assert plane.provisioning.token_expiry_days == 364

    def test_plane_negative_token_expiry_days_rejected(self):
        """A negative expiry would silently mean "never expires" on the
        Plane path (``_expiry`` maps <= 0 to no ``expired_at``) — the
        inverse of the operator's intent — so the model rejects it."""
        from pydantic import ValidationError

        with pytest.raises(ValidationError, match="token_expiry_days"):
            CompanyConfig(
                name="X",
                integrations={
                    "plane": self._plane(provisioning={"token_expiry_days": -7})
                },
            )

    def test_plane_requires_url(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError, match="plane.url is required"):
            CompanyConfig(name="X", integrations={"plane": self._plane(url="")})

    def test_plane_requires_workspace(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError, match="plane.workspace is required"):
            CompanyConfig(name="X", integrations={"plane": self._plane(workspace="")})

    def test_plane_requires_webhook_secret(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError, match="plane.webhook_secret is required"):
            CompanyConfig(
                name="X", integrations={"plane": self._plane(webhook_secret=None)}
            )

    def test_plane_disabled_needs_nothing(self):
        cfg = CompanyConfig(name="X", integrations={"plane": {"enabled": False}})
        assert cfg.integrations.plane is not None
        assert cfg.integrations.plane.enabled is False

    def test_plane_api_base(self):
        cfg = CompanyConfig(
            name="X", integrations={"plane": self._plane(url="https://x/")}
        )
        assert cfg.integrations.plane.api_base == "https://x/api/v1"

    def test_confluence_plane_exclusive(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError, match="mutually exclusive"):
            CompanyConfig(
                name="X",
                integrations={
                    "plane": self._plane(),
                    "confluence": {"url": "https://x.atlassian.net/wiki", "token": "t"},
                },
            )

    def test_confluence_with_disabled_plane_ok(self):
        cfg = CompanyConfig(
            name="X",
            integrations={
                "plane": {"enabled": False},
                "confluence": {"url": "https://x.atlassian.net/wiki", "token": "t"},
            },
        )
        assert cfg.integrations.confluence is not None
        assert cfg.integrations.plane.enabled is False

    def test_plane_projects_scope_requires_plane(self):
        from pydantic import ValidationError

        with pytest.raises(
            ValidationError, match="requires integrations.plane enabled"
        ):
            CompanyConfig(name="X", knowledge={"plane_projects": ["ENG"]})
        # A disabled plane block does not activate the backend either.
        with pytest.raises(
            ValidationError, match="requires integrations.plane enabled"
        ):
            CompanyConfig(
                name="X",
                integrations={"plane": {"enabled": False}},
                knowledge={"plane_projects": ["ENG"]},
            )

    def test_confluence_spaces_rejected_when_plane_active(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError, match="knowledge.confluence_spaces"):
            CompanyConfig(
                name="X",
                integrations={"plane": self._plane()},
                knowledge={"confluence_spaces": ["HANDBOOK"]},
            )

    def test_plane_projects_rejected_when_confluence_active(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError, match="knowledge.plane_projects"):
            CompanyConfig(
                name="X",
                integrations={
                    "confluence": {"url": "https://x.atlassian.net/wiki", "token": "t"}
                },
                knowledge={"plane_projects": ["ENG"]},
            )

    def test_plane_identity_extraction(self):
        """Unit + root-role ``integrations.plane.project`` materialise onto
        ``plane_project``; the jira + confluence identities are unaffected
        by the 3-tuple ``_integration_identities`` rename."""
        cfg = CompanyConfig(
            name="X",
            roles=[
                {
                    "name": "CTO",
                    "integrations": {
                        "jira": {"project": "INFRA"},
                        "plane": {"project": "LEAD"},
                    },
                },
            ],
            units=[
                {
                    "name": "Engineering",
                    "type": "team",
                    "lead": "Tech Lead",
                    "roles": [{"name": "Tech Lead"}],
                    "integrations": {
                        "jira": {"project": "ENG"},
                        "confluence": {"space": "ENGDOCS"},
                        "plane": {"project": "ENG"},
                    },
                },
            ],
        )
        org = config_to_organization(cfg)
        unit = org.units[0]
        assert unit.jira_project == "ENG"
        assert unit.confluence_space == "ENGDOCS"
        assert unit.plane_project == "ENG"
        cto = org.get_role("CTO")
        assert cto.jira_project == "INFRA"
        assert cto.confluence_space == ""
        assert cto.plane_project == "LEAD"
        # Unit-nested roles do not inherit the unit's identity.
        assert unit.roles[0].plane_project == ""

    def test_knowledge_plane_projects_flows_to_org(self):
        cfg = CompanyConfig(
            name="X",
            integrations={"plane": self._plane()},
            knowledge={"plane_projects": ["HANDBOOK", "ENG"]},
        )
        org = config_to_organization(cfg)
        assert org.plane_projects == ["HANDBOOK", "ENG"]


class TestPlaneHumanSeats:
    """``contact.plane_user_id`` on human seats + the plane identity ban."""

    def test_human_seat_with_only_plane_user_id_valid(self):
        human = Role(
            name="Sarah Chen",
            kind="human",
            contact={"plane_user_id": "ab12cd34-0000-0000-0000-000000000001"},
        )
        assert not human.contact.is_empty()
        assert ("plane", "ab12cd34-0000-0000-0000-000000000001") in (
            human.contact.resolved_identities()
        )

    def test_plane_user_id_normalized_to_lowercase(self):
        human = Role(
            name="Sarah Chen",
            kind="human",
            contact={"plane_user_id": "AB12CD34-0000-0000-0000-000000000001"},
        )
        assert human.contact.plane_user_id == "ab12cd34-0000-0000-0000-000000000001"

    def test_human_seat_rejects_plane_project(self):
        with pytest.raises(ValueError, match="integrations.plane"):
            Role(
                name="Sarah Chen",
                kind="human",
                contact={"slack_user_id": "U0HUMAN"},
                plane_project="ENG",
            )

    def test_human_seat_rejects_plane_project_via_loader(self):
        from pydantic import ValidationError

        cfg = CompanyConfig(
            name="X",
            roles=[
                {
                    "name": "Sarah Chen",
                    "kind": "human",
                    "contact": {"slack_user_id": "U0HUMAN"},
                    "integrations": {"plane": {"project": "ENG"}},
                },
            ],
        )
        with pytest.raises(ValidationError, match="integrations.plane"):
            config_to_organization(cfg)


class TestPlaneProjectLeadMap:
    """build_plane_project_lead_map — project identifier to lead handle."""

    def _make_org(self, teams: list[tuple[str, str, str]]) -> Organization:
        """Build an Organization from (team_name, lead_name, project_key).

        All teams go into a single flat unit list. Each team gets one role
        (the lead). The project identifier is set as the unit's
        ``plane_project`` integration identity.
        """
        org_units = []
        for team_name, lead_name, project_key in teams:
            role = Role(name=lead_name, responsibilities=[])
            org_units.append(
                OrgUnit(
                    name=team_name,
                    type="team",
                    lead=lead_name,
                    roles=[role],
                    plane_project=project_key,
                )
            )
        return Organization(name="TestCo", units=org_units)

    def test_single_team_single_key(self) -> None:
        org = self._make_org([("Backend", "Tech Lead", "BACK")])
        assert build_plane_project_lead_map(org) == {"BACK": "tech-lead"}

    def test_multiple_teams_unique_keys(self) -> None:
        org = self._make_org(
            [("Backend", "Tech Lead", "BACK"), ("Frontend", "UI Lead", "FRONT")]
        )
        assert build_plane_project_lead_map(org) == {
            "BACK": "tech-lead",
            "FRONT": "ui-lead",
        }

    def test_team_without_project_key_skipped(self) -> None:
        org = self._make_org(
            [("Backend", "Tech Lead", "BACK"), ("Design", "Designer", "")]
        )
        assert build_plane_project_lead_map(org) == {"BACK": "tech-lead"}

    def test_duplicate_key_warns_and_picks_first(self, caplog) -> None:
        """Same identifier resolving to DIFFERENT leads warns; first wins."""
        org = self._make_org(
            [("Backend", "Tech Lead", "SHARED"), ("Frontend", "UI Lead", "SHARED")]
        )
        result = build_plane_project_lead_map(org)
        assert result == {"SHARED": "tech-lead"}
        assert "plane_project_key_shared" in caplog.text

    def test_duplicate_key_same_lead_stays_silent(self, caplog) -> None:
        """Units sharing one project under ONE lead (e.g. a leadless child
        unit inheriting its parent's) is the documented Jira-parity
        pattern — first-wins, and NO warning."""
        lead = Role(name="Tech Lead", responsibilities=[])
        child = OrgUnit(
            name="Backend Infra",
            type="team",
            plane_project="SHARED",  # no own lead — inherits Tech Lead
        )
        parent = OrgUnit(
            name="Backend",
            type="team",
            lead="Tech Lead",
            roles=[lead],
            plane_project="SHARED",
            children=[child],
        )
        org = Organization(name="TestCo", units=[parent])
        result = build_plane_project_lead_map(org)
        assert result == {"SHARED": "tech-lead"}
        assert "plane_project_key_shared" not in caplog.text

    def test_empty_org(self) -> None:
        assert build_plane_project_lead_map(Organization(name="Empty", units=[])) == {}

    def test_env_var_resolution(self, monkeypatch) -> None:
        monkeypatch.setenv("CREWLET_TEST_PLANE_PROJECT", "RESOLVED")
        role = Role(name="Lead", responsibilities=[])
        unit = OrgUnit(
            name="Team",
            type="team",
            lead="Lead",
            roles=[role],
            plane_project="${CREWLET_TEST_PLANE_PROJECT}",
        )
        org = Organization(name="Test", units=[unit])
        assert build_plane_project_lead_map(org) == {"RESOLVED": "lead"}

    def test_mixed_case_key_normalised_to_upper(self) -> None:
        org = self._make_org([("Executives", "CEO", "PoC")])
        assert build_plane_project_lead_map(org) == {"POC": "ceo"}

    def test_root_level_role_project_key(self) -> None:
        org = Organization(name="Test", roles=[Role(name="CEO", plane_project="EXEC")])
        assert build_plane_project_lead_map(org) == {"EXEC": "ceo"}

    def test_root_level_role_and_unit_keys_coexist(self) -> None:
        org = Organization(
            name="Test",
            roles=[Role(name="CTO", plane_project="INFRA")],
            units=[
                OrgUnit(
                    name="Backend",
                    type="team",
                    lead="Tech Lead",
                    roles=[Role(name="Tech Lead")],
                    plane_project="BACK",
                ),
            ],
        )
        assert build_plane_project_lead_map(org) == {
            "INFRA": "cto",
            "BACK": "tech-lead",
        }

    def test_inherited_lead_unit_maps(self) -> None:
        """A child unit with an inherited lead (the lead role lives in the
        parent unit) still maps — get_effective_lead resolves it."""
        org = Organization(
            name="Test",
            units=[
                OrgUnit(
                    name="Engineering",
                    type="department",
                    lead="VP Eng",
                    roles=[Role(name="VP Eng")],
                    children=[
                        OrgUnit(name="Backend", type="team", plane_project="BACK"),
                    ],
                ),
            ],
        )
        assert build_plane_project_lead_map(org) == {"BACK": "vp-eng"}


class TestSpaceKeyLeadMapInheritedLead:
    """Incidental m11 fix: build_space_key_lead_map resolves inherited
    leads via get_effective_lead (``unit.get_lead()`` returned ``None``
    for a unit whose lead cascades down from an ancestor, silently
    dropping its page routing while Jira routing kept working)."""

    def test_inherited_lead_unit_now_maps(self) -> None:
        org = Organization(
            name="Test",
            units=[
                OrgUnit(
                    name="Engineering",
                    type="department",
                    lead="VP Eng",
                    roles=[Role(name="VP Eng")],
                    children=[
                        OrgUnit(name="Backend", type="team", confluence_space="BACK"),
                    ],
                ),
            ],
        )
        assert build_space_key_lead_map(org) == {"BACK": ["vp-eng"]}


class TestSkillVariables:
    """The ``skill_variables:`` map on CompanyConfig."""

    def test_default_empty(self):
        assert CompanyConfig(name="X").skill_variables == {}

    def test_parses_map(self):
        cfg = CompanyConfig(
            name="X",
            skill_variables={
                "confluence_base_url": "https://x.atlassian.net/wiki",
                "jira_base_url": "https://x.atlassian.net",
            },
        )
        assert cfg.skill_variables["jira_base_url"] == "https://x.atlassian.net"

    def test_rejects_non_identifier_keys(self):
        from pydantic import ValidationError

        for bad in ("base-url", "a.b", "9x", "../etc", "has space"):
            with pytest.raises(ValidationError):
                CompanyConfig(name="X", skill_variables={bad: "v"})

    def test_accepts_identifier_keys(self):
        cfg = CompanyConfig(
            name="X", skill_variables={"_x9": "a", "Confluence_Base": "b"}
        )
        assert set(cfg.skill_variables) == {"_x9", "Confluence_Base"}


class TestRootLevelRoles:
    """Tests for root-level roles in config."""

    def test_root_roles_from_yaml(self, tmp_path: Path):
        """Root-level roles are parsed from YAML config."""
        yaml_content = """\
name: Test Corp
roles:
  - name: CEO
    goal: Run the company
    manages: [VP Eng]
units:
  - name: Engineering
    type: team
    lead: VP Eng
    roles:
      - name: VP Eng
        manages: [Dev]
      - name: Dev
"""
        config_file = tmp_path / "config.yaml"
        config_file.write_text(yaml_content)
        config = load_company_config(config_file)
        assert len(config.roles) == 1
        assert config.roles[0].name == "CEO"

        org = config_to_organization(config)
        assert len(org.roles) == 1
        assert org.roles[0].name == "CEO"
        assert org.roles[0].manages == ["VP Eng"]

        # Root role is findable via get_role
        ceo = org.get_role("CEO")
        assert ceo is not None
        assert ceo.goal == "Run the company"

        # Total roles include root + unit roles
        assert len(org.all_roles()) == 3

    def test_root_roles_empty_by_default(self):
        config = CompanyConfig(name="X")
        assert config.roles == []
        org = config_to_organization(config)
        assert org.roles == []

    def test_root_roles_only_no_structure(self, tmp_path: Path):
        """Org can have only root-level roles, no structure."""
        yaml_content = """\
name: Solo Founder
roles:
  - name: Founder
    goal: Build everything
"""
        config_file = tmp_path / "config.yaml"
        config_file.write_text(yaml_content)
        org = load_organization(config_file)
        assert len(org.all_roles()) == 1
        assert org.get_role("Founder") is not None
        assert len(org.units) == 0

    def test_root_role_hierarchy_traversal(self, tmp_path: Path):
        """Root-level roles participate in manages[] hierarchy."""
        from crewlet.org.hierarchy import get_ancestors, get_manager

        yaml_content = """\
name: Test Corp
roles:
  - name: CEO
    manages: [VP Eng]
units:
  - name: Engineering
    type: team
    lead: VP Eng
    roles:
      - name: VP Eng
        manages: [Dev]
      - name: Dev
"""
        config_file = tmp_path / "config.yaml"
        config_file.write_text(yaml_content)
        org = load_organization(config_file)

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


class TestRootRoleUnitReference:
    """Root roles with a ``unit:`` reference must be moved into the
    named unit and inherit its ``mcp_env`` -- otherwise per-role MCP
    instances never spawn and Confluence routing can't find the lead.

    This is the natural shape produced by ``POST /config/roles`` with
    a ``unit:`` field (the role lands at root with the reference set);
    ``config_to_organization`` is the single place where the reference
    is resolved.
    """

    def test_root_role_with_unit_ref_moved_into_unit(self):
        config = CompanyConfig(
            name="Acme",
            roles=[
                {
                    "name": "Agent CEO",
                    "handle": "agent-ceo",
                    "unit": "Executives",
                }
            ],
            units=[
                {
                    "name": "Leadership",
                    "type": "department",
                    "children": [
                        {
                            "name": "Executives",
                            "type": "team",
                            "lead": "Agent CEO",
                            "mcp_env": {
                                "atlassian": {
                                    "JIRA_PROJECT_KEY": "LEAD",
                                    "CONFLUENCE_SPACE_KEY": "LEAD",
                                }
                            },
                        }
                    ],
                }
            ],
        )
        org = config_to_organization(config)

        # Role is no longer at root.
        assert org.roles == []
        # Role is now physically inside Executives.
        executives = None
        for u in org.units:
            for child in u.children:
                if child.name == "Executives":
                    executives = child
        assert executives is not None
        assert [r.name for r in executives.roles] == ["Agent CEO"]
        # mcp_env was inherited from the unit.
        ceo = executives.roles[0]
        assert ceo.mcp_env == {
            "atlassian": {
                "JIRA_PROJECT_KEY": "LEAD",
                "CONFLUENCE_SPACE_KEY": "LEAD",
            }
        }
        # Lead lookup on the unit now resolves correctly.
        assert executives.get_lead() is ceo

    def test_root_role_with_unit_ref_role_env_overrides_unit(self):
        config = CompanyConfig(
            name="Acme",
            roles=[
                {
                    "name": "Agent CEO",
                    "unit": "Executives",
                    "mcp_env": {
                        "atlassian": {"JIRA_PROJECT_KEY": "BOSS"},
                    },
                }
            ],
            units=[
                {
                    "name": "Executives",
                    "mcp_env": {
                        "atlassian": {
                            "JIRA_PROJECT_KEY": "LEAD",
                            "CONFLUENCE_SPACE_KEY": "LEAD",
                        }
                    },
                }
            ],
        )
        org = config_to_organization(config)
        ceo = org.units[0].roles[0]
        # Role's own JIRA_PROJECT_KEY wins; unit's other keys still merge.
        assert ceo.mcp_env == {
            "atlassian": {
                "JIRA_PROJECT_KEY": "BOSS",
                "CONFLUENCE_SPACE_KEY": "LEAD",
            }
        }

    def test_root_role_unit_ref_missing_unit_logs_and_keeps_at_root(self):
        config = CompanyConfig(
            name="Acme",
            roles=[{"name": "Stray", "unit": "Nowhere"}],
            units=[{"name": "Executives"}],
        )
        org = config_to_organization(config)
        # The role stayed at root since the unit didn't exist.
        assert [r.name for r in org.roles] == ["Stray"]
        assert org.units[0].roles == []

    def test_root_role_without_unit_ref_stays_at_root(self):
        config = CompanyConfig(
            name="Acme",
            roles=[{"name": "Solo"}],
            units=[{"name": "Executives"}],
        )
        org = config_to_organization(config)
        assert [r.name for r in org.roles] == ["Solo"]
        assert org.units[0].roles == []


class TestLLMProviderConfig:
    """Tests for LLM provider reasoning config validation."""

    def test_reasoning_rejected_for_openai_compatible(self):
        from pydantic import ValidationError

        from crewlet.config import LLMProviderConfig

        with pytest.raises(ValidationError, match="openai-compatible"):
            LLMProviderConfig(
                type="openai-compatible",
                model="example-model",
                reasoning=True,
            )

    def test_reasoning_allowed_for_openai(self):
        from crewlet.config import LLMProviderConfig

        cfg = LLMProviderConfig(type="openai", model="o3", reasoning=True)
        assert cfg.reasoning is True

    def test_reasoning_allowed_for_anthropic(self):
        from crewlet.config import LLMProviderConfig

        cfg = LLMProviderConfig(
            type="anthropic",
            model="claude-sonnet-4-20250514",
            reasoning=True,
        )
        assert cfg.reasoning is True

    def test_no_reasoning_allowed_for_openai_compatible(self):
        from crewlet.config import LLMProviderConfig

        cfg = LLMProviderConfig(
            type="openai-compatible",
            model="example-model",
        )
        assert cfg.reasoning is False

    def test_create_llm_providers_fails_fast_on_missing_key(self):
        """An empty api_keys list with no conventional env var set must
        fail at provider construction with a clear message -- not
        surface as an AuthenticationError deep inside the first turn."""
        from crewlet.config import (
            LLMProviderConfig,
            ProvidersConfig,
            create_llm_providers,
        )

        cfg = ProvidersConfig(
            llm={"default": LLMProviderConfig(type="openai", model="gpt-4o")}
        )
        with patch.dict("os.environ", {}, clear=False) as _env:
            import os

            os.environ.pop("OPENAI_API_KEY", None)
            with pytest.raises(ValueError, match="no API key"):
                create_llm_providers(cfg)

    def test_create_llm_providers_accepts_env_var_fallback(self):
        """An empty api_keys list is fine when the conventional env var
        is set -- the provider resolves the credential at construction."""
        from crewlet.config import (
            LLMProviderConfig,
            ProvidersConfig,
            create_llm_providers,
        )

        cfg = ProvidersConfig(
            llm={"default": LLMProviderConfig(type="openai", model="gpt-4o")}
        )
        with patch.dict("os.environ", {"OPENAI_API_KEY": "sk-test"}):
            providers = create_llm_providers(cfg)
        assert "default" in providers

    def test_create_llm_providers_threads_timeout(self):
        """``timeout_seconds`` reaches the provider's SDK client; the
        default is 120s and an explicit value overrides it."""
        from crewlet.config import (
            LLMProviderConfig,
            ProvidersConfig,
            create_llm_providers,
        )

        cfg = ProvidersConfig(
            llm={
                "default": LLMProviderConfig(
                    type="openai", model="gpt-4o", api_keys=["k"]
                ),
                "slow": LLMProviderConfig(
                    type="anthropic",
                    model="claude-3",
                    api_keys=["k"],
                    timeout_seconds=300.0,
                ),
            }
        )
        providers = create_llm_providers(cfg)
        assert providers["default"]._timeout == 120.0
        assert providers["default"]._client.timeout == 120.0
        assert providers["slow"]._timeout == 300.0
        assert providers["slow"]._client.timeout == 300.0


class TestExtraKeysRejected:
    def test_extra_root_keys_rejected(self):
        """Unknown root-level keys are rejected to catch indentation bugs."""
        from pydantic import ValidationError

        with pytest.raises(ValidationError):
            CompanyConfig.model_validate({"name": "X", "llm": {"default": {}}})


# ---------------------------------------------------------------------------
# Turn engine config
# ---------------------------------------------------------------------------


class TestTurnEngineConfig:
    """Tests for the ``turn_engine`` CompanyConfig section."""

    def test_turn_engine_defaults(self):
        """Unconfigured ``turn_engine`` gets the documented defaults."""
        cfg = CompanyConfig.model_validate({"name": "X"})
        assert cfg.turn_engine.max_iterations == 3
        assert cfg.turn_engine.subagent_max_turns == 20
        # Timeout is a float (wall-clock seconds; fractional allowed).
        assert cfg.turn_engine.subagent_timeout_seconds == 120.0
        assert isinstance(cfg.turn_engine.subagent_timeout_seconds, float)
        assert cfg.turn_engine.subagent_budget_fraction == 0.2
        assert cfg.turn_engine.executor_always_on_tools == ["load_tool_skill"]
        assert cfg.turn_engine.delegation_depth_limit == 3

    def test_turn_engine_accepts_fractional_subagent_timeout(self):
        """``subagent_timeout_seconds`` is typed ``float`` so test configs
        and fine-grained timeouts can use fractional values (e.g. 0.5)
        without silent truncation.
        """
        cfg = CompanyConfig.model_validate(
            {"name": "X", "turn_engine": {"subagent_timeout_seconds": 0.5}}
        )
        assert cfg.turn_engine.subagent_timeout_seconds == 0.5

    def test_turn_engine_override(self):
        """Explicit ``turn_engine`` values are honored."""
        cfg = CompanyConfig.model_validate(
            {
                "name": "X",
                "turn_engine": {
                    "max_iterations": 5,
                    "subagent_max_turns": 10,
                    "subagent_timeout_seconds": 30,
                    "subagent_budget_fraction": 0.5,
                    "executor_always_on_tools": ["query_knowledge", "use_skill"],
                    "delegation_depth_limit": 5,
                },
            }
        )
        assert cfg.turn_engine.max_iterations == 5
        assert cfg.turn_engine.subagent_max_turns == 10
        assert cfg.turn_engine.subagent_timeout_seconds == 30
        assert cfg.turn_engine.subagent_budget_fraction == 0.5
        assert cfg.turn_engine.executor_always_on_tools == [
            "query_knowledge",
            "use_skill",
        ]
        assert cfg.turn_engine.delegation_depth_limit == 5

    def test_turn_engine_rejects_unknown_keys(self):
        """Typos in ``turn_engine`` should be caught by ``extra='forbid'``."""
        from pydantic import ValidationError

        with pytest.raises(ValidationError):
            CompanyConfig.model_validate(
                {"name": "X", "turn_engine": {"bogus_field": 1}}
            )


class TestRolePerPhaseLLM:
    """Tests for per-phase LLM keys on a Role."""

    def test_llm_as_string_backwards_compatible(self, tmp_path):
        """``llm: mymodel`` still works and sets the global provider key."""
        yaml_content = """\
name: Test Corp
units:
  - name: Core
    type: team
    lead: Lead
    roles:
      - name: Lead
        llm: claude-sonnet
"""
        config_file = tmp_path / "config.yaml"
        config_file.write_text(yaml_content)
        org = load_organization(config_file)
        lead = org.get_role("Lead")
        assert lead is not None
        assert lead.llm == "claude-sonnet"
        assert lead.llm_plan is None
        assert lead.llm_execute is None
        assert lead.llm_review is None
        assert lead.llm_subagent is None

    def test_llm_as_dict_per_phase(self, tmp_path):
        """``llm: {plan: A, execute: B, ...}`` populates the per-phase keys."""
        yaml_content = """\
name: Test Corp
units:
  - name: Core
    type: team
    lead: Lead
    roles:
      - name: Lead
        llm:
          default: claude-sonnet
          plan: claude-sonnet
          execute: claude-haiku
          review: claude-haiku
          subagent: claude-haiku
"""
        config_file = tmp_path / "config.yaml"
        config_file.write_text(yaml_content)
        org = load_organization(config_file)
        lead = org.get_role("Lead")
        assert lead is not None
        assert lead.llm == "claude-sonnet"
        assert lead.llm_plan == "claude-sonnet"
        assert lead.llm_execute == "claude-haiku"
        assert lead.llm_review == "claude-haiku"
        assert lead.llm_subagent == "claude-haiku"

    def test_llm_as_dict_missing_phases_fall_back(self, tmp_path):
        """Phases missing from the dict fall back to ``default`` or None."""
        yaml_content = """\
name: Test Corp
units:
  - name: Core
    type: team
    lead: Lead
    roles:
      - name: Lead
        llm:
          default: claude-sonnet
          plan: claude-opus
"""
        config_file = tmp_path / "config.yaml"
        config_file.write_text(yaml_content)
        org = load_organization(config_file)
        lead = org.get_role("Lead")
        assert lead is not None
        assert lead.llm == "claude-sonnet"
        assert lead.llm_plan == "claude-opus"
        assert lead.llm_execute is None  # falls back to role.llm at resolve time
        assert lead.llm_review is None
        assert lead.llm_subagent is None


# ---------------------------------------------------------------- #
# Human seats — config → org parsing
# ---------------------------------------------------------------- #


def _human_role_dict(**overrides):
    data = {
        "name": "Sarah Chen",
        "kind": "human",
        "email": "sarah@acme.com",
        "contact": {"slack_user_id": "U0123"},
    }
    data.update(overrides)
    return data


def test_config_parses_human_seat():
    config = CompanyConfig(
        name="Acme",
        roles=[
            _human_role_dict(
                contact={"slack_user_id": "U0123", "atlassian_account_id": "abc"},
                availability="CET business hours",
            )
        ],
    )
    org = config_to_organization(config)
    sarah = org.get_role("Sarah Chen")
    assert sarah is not None
    assert sarah.is_human
    assert sarah.contact is not None
    assert sarah.contact.slack_user_id == "U0123"
    assert sarah.availability == "CET business hours"


def test_config_human_seat_requires_contact():
    config = CompanyConfig(
        name="Acme",
        roles=[{"name": "Sarah Chen", "kind": "human", "email": "s@x.com"}],
    )
    with pytest.raises(ValueError, match="contact"):
        config_to_organization(config)


class TestAuthoredModelStrictness:
    """``RoleConfig`` / ``OrgUnitConfig`` / ``MCPServerConfig`` are the
    authored YAML shape and forbid unknown keys.

    Before they were typed, ``roles`` / ``units`` / ``mcp_servers`` were
    ``list[dict[str, Any]]`` and every parser read them with
    ``data.get(...)``: a mistyped field was silently dropped and
    ``crewlet validate`` still reported "Config OK", leaving the seat
    running with an empty backstory (or the MCP server with no command)
    and no diagnostic anywhere.
    """

    def test_role_typo_is_rejected_with_its_path(self):
        with pytest.raises(ValidationError) as exc:
            CompanyConfig.model_validate(
                {"name": "Acme", "roles": [{"name": "CEO", "backstroy": "oops"}]}
            )
        err = exc.value.errors()[0]
        assert err["loc"] == ("roles", 0, "backstroy")
        assert err["type"] == "extra_forbidden"

    def test_nested_unit_role_typo_is_rejected(self):
        with pytest.raises(ValidationError) as exc:
            CompanyConfig.model_validate(
                {
                    "name": "Acme",
                    "units": [
                        {
                            "name": "Eng",
                            "children": [
                                {
                                    "name": "Backend",
                                    "roles": [{"name": "Dev", "lmm": "x"}],
                                }
                            ],
                        }
                    ],
                }
            )
        assert exc.value.errors()[0]["loc"] == (
            "units",
            0,
            "children",
            0,
            "roles",
            0,
            "lmm",
        )

    def test_unit_typo_is_rejected(self):
        with pytest.raises(ValidationError, match="leed"):
            CompanyConfig.model_validate(
                {"name": "Acme", "units": [{"name": "Eng", "leed": "CTO"}]}
            )

    def test_mcp_server_typo_is_rejected(self):
        with pytest.raises(ValidationError, match="commnad"):
            CompanyConfig.model_validate(
                {"name": "Acme", "mcp_servers": [{"name": "x", "commnad": "uvx"}]}
            )

    def test_role_level_slack_block_is_rejected(self):
        """``slack:`` on a role is the *runtime* shape — the parser only
        ever read ``integrations.slack``, so authoring it did nothing."""
        with pytest.raises(ValidationError, match="slack"):
            CompanyConfig.model_validate(
                {
                    "name": "Acme",
                    "roles": [{"name": "Dev", "slack": {"bot_token": "xoxb"}}],
                }
            )


class TestValuelessYamlKeys:
    """A key written with nothing under it parses to ``None``; that is
    ordinary "empty" authoring, not an error."""

    def test_valueless_collections_and_scalars_are_unset(self):
        cfg = CompanyConfig.model_validate(
            yaml.safe_load(
                """
name: Acme
policies:
mcp_servers:
units:
  - name: Eng
    lead:
    goals:
    roles:
    children:
roles:
  - name: Dev
    unit: Eng
    manages:
    mcp_env:
"""
            )
        )
        assert cfg.policies == []
        assert cfg.mcp_servers == []
        assert cfg.units[0].lead == ""
        assert cfg.units[0].roles == []
        assert cfg.roles[0].manages == []
        assert cfg.roles[0].mcp_env == {}

    def test_valueless_unit_roles_still_receives_moved_root_role(self):
        """Regression: a root role's ``unit:`` reference must resolve
        into a unit whose ``roles:`` key was written empty."""
        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "units": [{"name": "Eng", "roles": None}],
                "roles": [{"name": "Dev", "unit": "Eng"}],
            }
        )
        org = config_to_organization(cfg)
        assert org.get_unit("Eng").get_role("Dev") is not None

    def test_required_field_written_valueless_still_errors(self):
        with pytest.raises(ValidationError, match="name"):
            CompanyConfig.model_validate({"name": None})


class TestProviderTypeIsClosed:
    """``providers.llm[*].type`` / ``providers.embeddings.type`` are
    closed sets.

    They used to be plain ``str``. ``create_llm_providers`` dispatches on
    the value with an if/elif chain that had no ``else``, so an
    unrecognised type fell through every branch and the provider key was
    simply ABSENT from the returned dict — no exception, no log, and
    every role naming that provider left without an LLM. The failure
    surfaced much later as a confusing unrelated error.
    """

    def test_typo_rejected_at_validation(self):
        with pytest.raises(ValidationError) as exc:
            CompanyConfig.model_validate(
                {"name": "A", "providers": {"llm": {"default": {"type": "openaii"}}}}
            )
        err = exc.value.errors()[0]
        assert err["loc"] == ("providers", "llm", "default", "type")

    @pytest.mark.parametrize("kind", get_args(LLMProviderType))
    def test_every_declared_type_constructs(self, kind, monkeypatch, tmp_path):
        """The Literal and the factory dispatch must stay in lockstep —
        a value the config accepts but the factory cannot build would
        reintroduce the silent-drop bug from the other direction.

        Parametrised off the ``Literal`` itself rather than a hand-kept
        list, so adding a provider type without a construction path
        fails here instead of at some operator's first turn.
        """
        from crewlet.config import create_llm_providers

        monkeypatch.setenv("OPENAI_API_KEY", "sk-test")
        monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-test")
        extra: dict = {}
        if kind == "openai-compatible":
            extra["base_url"] = "https://x/v1/"
        if kind == "cli-agent":
            # The backend drives a real local executable, so give it one.
            fake = tmp_path / "bin"
            fake.mkdir()
            (fake / "fakecli").write_text("#!/bin/sh\nexit 0\n")
            (fake / "fakecli").chmod(0o755)
            monkeypatch.setenv("PATH", str(fake))
            extra["cli"] = {
                "agent": "custom",
                "state_dir": str(tmp_path / "state"),
                "overrides": {"binary": "fakecli", "complete_args": ["--json"]},
            }
        cfg = CompanyConfig.model_validate(
            {
                "name": "A",
                "providers": {
                    "llm": {
                        "p": {
                            "type": kind,
                            "model": "m",
                            "api_keys": ["sk-x"],
                            **extra,
                        }
                    }
                },
            }
        )
        assert "p" in create_llm_providers(cfg.providers)

    def test_factory_raises_instead_of_silently_dropping(self):
        """Defense in depth for a hand-built config that bypassed
        validation: the key must never just vanish."""
        from crewlet.config import (
            LLMProviderConfig,
            ProvidersConfig,
            create_llm_providers,
        )

        providers = ProvidersConfig(
            llm={
                "default": LLMProviderConfig.model_construct(
                    type="openaii", model="m", api_keys=["sk-x"]
                )
            }
        )
        with pytest.raises(ValueError, match="Unsupported LLM provider type"):
            create_llm_providers(providers)


class TestCLIAgentProviderConfig:
    """``providers.llm[*]`` of type ``cli-agent`` — the subscription
    backend that drives a locally installed coding CLI.

    See ``docs/concepts/subscription-llm-backends.md``.
    """

    @staticmethod
    def _cfg(**cli) -> dict:
        return {
            "name": "A",
            "providers": {
                "llm": {
                    "default": {
                        "type": "cli-agent",
                        "model": "sonnet",
                        "cli": cli or {"agent": "claude-code"},
                    }
                }
            },
        }

    def test_minimal_block_validates(self):
        cfg = CompanyConfig.model_validate(self._cfg(agent="codex"))
        cli = cfg.providers.llm["default"].cli
        assert cli is not None
        assert cli.agent == "codex"
        assert cli.profile().binary == "codex"

    def test_type_requires_the_block(self):
        """Without it the entry would silently construct the default
        Claude Code profile, which may be an entirely different CLI."""
        with pytest.raises(ValidationError, match="needs a `cli:` block"):
            CompanyConfig.model_validate(
                {
                    "name": "A",
                    "providers": {"llm": {"default": {"type": "cli-agent"}}},
                }
            )

    def test_block_on_an_http_provider_is_rejected(self):
        """A `cli:` block nobody reads is the classic 'I configured it
        and nothing happened' bug."""
        with pytest.raises(ValidationError, match="only applies to type"):
            CompanyConfig.model_validate(
                {
                    "name": "A",
                    "providers": {
                        "llm": {
                            "default": {
                                "type": "anthropic",
                                "api_keys": ["k"],
                                "cli": {"agent": "codex"},
                            }
                        }
                    },
                }
            )

    def test_unknown_agent_is_rejected_with_the_valid_names(self):
        with pytest.raises(ValidationError) as exc:
            CompanyConfig.model_validate(self._cfg(agent="claude-kode"))
        assert exc.value.errors()[0]["loc"][:5] == (
            "providers",
            "llm",
            "default",
            "cli",
            "agent",
        )

    def test_reasoning_is_rejected(self):
        with pytest.raises(ValidationError, match="not a Crewlet setting"):
            CompanyConfig.model_validate(
                {
                    "name": "A",
                    "providers": {
                        "llm": {
                            "default": {
                                "type": "cli-agent",
                                "reasoning": True,
                                "cli": {"agent": "codex"},
                            }
                        }
                    },
                }
            )

    def test_bad_override_fails_validation_not_boot(self):
        """`crewlet validate` must catch a broken override — discovering
        it at the first turn of the first agent is far too late."""
        with pytest.raises(ValidationError, match="not a valid profile"):
            CompanyConfig.model_validate(
                self._cfg(agent="codex", overrides={"no_such_field": 1})
            )

    def test_overrides_reach_the_resolved_profile(self):
        cfg = CompanyConfig.model_validate(
            self._cfg(agent="codex", overrides={"binary": "/opt/codex"})
        )
        assert cfg.providers.llm["default"].cli.profile().binary == "/opt/codex"

    @pytest.mark.parametrize(
        "field,value", [("timeout_seconds", 0), ("max_concurrent", 0)]
    )
    def test_nonsensical_knobs_are_rejected(self, field, value):
        with pytest.raises(ValidationError):
            CompanyConfig.model_validate(self._cfg(agent="codex", **{field: value}))

    def test_no_api_key_required(self, tmp_path, monkeypatch):
        """The whole point: this backend authenticates with the
        operator's subscription, so the missing-key guard that protects
        the HTTP providers must not fire."""
        from crewlet.config import create_llm_providers

        monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
        monkeypatch.delenv("OPENAI_API_KEY", raising=False)
        fake = tmp_path / "bin"
        fake.mkdir()
        (fake / "codex").write_text("#!/bin/sh\nexit 0\n")
        (fake / "codex").chmod(0o755)
        monkeypatch.setenv("PATH", str(fake))
        cfg = CompanyConfig.model_validate(
            self._cfg(agent="codex", state_dir=str(tmp_path / "state"))
        )
        providers = create_llm_providers(cfg.providers)
        assert providers["default"].model == "sonnet"

    def test_state_dir_and_env_resolve_env_references(self, tmp_path, monkeypatch):
        from crewlet.config import create_llm_providers

        fake = tmp_path / "bin"
        fake.mkdir()
        (fake / "codex").write_text("#!/bin/sh\nexit 0\n")
        (fake / "codex").chmod(0o755)
        monkeypatch.setenv("PATH", str(fake))
        monkeypatch.setenv("CLI_STATE", str(tmp_path / "resolved"))
        cfg = CompanyConfig.model_validate(
            self._cfg(agent="codex", state_dir="${CLI_STATE}")
        )
        provider = create_llm_providers(cfg.providers)["default"]
        assert provider.workspace.state_dir == tmp_path / "resolved"

    def test_credential_bundle_is_restored_at_construction(self, tmp_path, monkeypatch):
        """An engine in a fresh container must come up already logged
        in, or every seat fails its first turn."""
        import base64
        import io
        import tarfile

        from crewlet.config import create_llm_providers

        fake = tmp_path / "bin"
        fake.mkdir()
        (fake / "codex").write_text("#!/bin/sh\nexit 0\n")
        (fake / "codex").chmod(0o755)
        monkeypatch.setenv("PATH", str(fake))

        raw = io.BytesIO()
        payload = b'{"token": "from-bundle"}'
        with tarfile.open(fileobj=raw, mode="w:gz") as tar:
            info = tarfile.TarInfo(".codex/auth.json")
            info.size = len(payload)
            tar.addfile(info, io.BytesIO(payload))
        monkeypatch.setenv(
            "CREWLET_LLM_CLI_DEFAULT_CREDENTIALS",
            base64.b64encode(raw.getvalue()).decode("ascii"),
        )

        cfg = CompanyConfig.model_validate(
            self._cfg(agent="codex", state_dir=str(tmp_path / "state"))
        )
        provider = create_llm_providers(cfg.providers)["default"]
        restored = provider.workspace.credential_dir / ".codex" / "auth.json"
        assert restored.read_bytes() == payload
