"""Structural tests for the shipped Nimbus example org.

The reference org is Plane-shaped (docs/integrations/plane.md): the
tracker + knowledge backend is the self-hosted Plane fork, the code host
is GitLab, and no Atlassian (Jira / Confluence / Forge) config appears
anywhere. These tests pin that shape so a stray Atlassian remnant fails
CI rather than shipping a broken example.
"""

from __future__ import annotations

import re
from collections.abc import Iterator
from pathlib import Path
from typing import Any

from crewlet.agent.skills.parser import parse_skill
from crewlet.config import (
    OrgUnitConfig,
    RoleConfig,
    config_to_organization,
    load_company_config,
)
from crewlet.engine_builders import resolve_skill_variables
from crewlet.knowledge.markdown_docs import (
    classify,
    parse_doc_file,
    split_optional_frontmatter,
)
from crewlet.org.models import RoleKind
from crewlet.sandbox.mcp_render import to_launch_spec

_REPO_ROOT = Path(__file__).resolve().parents[2]
_COMPANY_YAML = _REPO_ROOT / "examples" / "nimbus.company.yaml"
_TOOL_SKILLS_DIR = _REPO_ROOT / "examples" / "tool-skills"
_DOCS_DIR = _REPO_ROOT / "examples" / "nimbus-docs"

# The provisioner's seat-token contract: one ${PLANE_TOKEN_<SEAT>} per
# agent seat.
_EXPECTED_SEAT_TOKEN_VARS = {
    "${PLANE_TOKEN_CEO}",
    "${PLANE_TOKEN_CTO}",
    "${PLANE_TOKEN_PM}",
    "${PLANE_TOKEN_DEVREL}",
    "${PLANE_TOKEN_SWE}",
    "${PLANE_TOKEN_FE}",
    "${PLANE_TOKEN_AI}",
}

_ENV_REF_RE = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")


def _walk_units(units: list[OrgUnitConfig]) -> Iterator[OrgUnitConfig]:
    for unit in units:
        yield unit
        yield from _walk_units(unit.children)


def _agent_seats(cfg: Any) -> list[RoleConfig]:
    """Every agent-kind seat, from root roles and all nested units."""
    seats: list[RoleConfig] = [r for r in cfg.roles if r.kind == RoleKind.AGENT]
    for unit in _walk_units(cfg.units):
        seats.extend(r for r in unit.roles if r.kind == RoleKind.AGENT)
    return seats


def test_nimbus_company_is_plane_shaped() -> None:
    cfg = load_company_config(_COMPANY_YAML)

    # Integrations: Plane in, Atlassian out.
    plane = cfg.integrations.plane
    assert plane is not None and plane.enabled
    # The instance address is the ${PLANE_URL} reference everywhere (the
    # bootstrap writes the var into .env.plane) — Tier B stores it raw.
    assert plane.url == "${PLANE_URL}"
    assert plane.workspace == "nimbus"
    assert plane.webhook_secret == "${PLANE_WEBHOOK_SECRET}"
    assert plane.token == "${PLANE_ENGINE_TOKEN}"
    assert plane.provisioning is not None
    assert plane.provisioning.projects == ["LEAD", "ENG", "PROD", "TS"]
    assert cfg.integrations.jira is None
    assert cfg.integrations.confluence is None
    assert cfg.integrations.forge_app_id == ""

    # Unit Plane project identities.
    unit_projects = {
        unit.integrations.plane.project
        for unit in _walk_units(cfg.units)
        if unit.integrations.plane is not None
    }
    assert unit_projects == {"LEAD", "PROD", "ENG"}
    for unit in _walk_units(cfg.units):
        assert unit.integrations.jira is None
        assert unit.integrations.confluence is None

    # All seven agent seats carry mcp_env.plane.PLANE_API_KEY and no
    # atlassian block.
    seats = _agent_seats(cfg)
    assert len(seats) == 7
    seat_tokens: set[str] = set()
    for role in seats:
        assert "atlassian" not in role.mcp_env, role.name
        token = role.mcp_env["plane"]["PLANE_API_KEY"]
        assert re.fullmatch(r"\$\{PLANE_TOKEN_[A-Z]+\}", token), role.name
        seat_tokens.add(token)
    assert seat_tokens == _EXPECTED_SEAT_TOKEN_VARS

    # Founder human seat closes the mention loop via a ${VAR} contact.
    humans = [r for r in cfg.roles if r.kind == RoleKind.HUMAN]
    assert len(humans) == 1
    assert humans[0].contact is not None
    assert humans[0].contact.plane_user_id == "${PLANE_FOUNDER_USER_ID}"

    # Sandbox seats read work items via the plane MCP server in-box.
    sandbox_seats = [r for r in seats if r.sandbox is not None and r.sandbox.enabled]
    assert len(sandbox_seats) == 3
    for role in sandbox_seats:
        assert role.sandbox is not None
        servers = role.sandbox.mcp.servers
        assert "plane" in servers, role.name
        assert "atlassian" not in servers, role.name

    # The plane MCP tool server, pinned per the fork's release.
    servers_by_name = {s.name: s for s in cfg.mcp_servers}
    assert "atlassian" not in servers_by_name
    plane_server = servers_by_name["plane"]
    assert plane_server.shared is False
    assert plane_server.command == "uvx"
    assert plane_server.args == ["plane-mcp-server@0.2.10", "stdio"]
    # Mandatory for self-hosted: without it the server talks to the
    # api.plane.so cloud gateway.  The ${PLANE_URL} ref resolves at spawn.
    assert plane_server.env["PLANE_BASE_URL"] == "${PLANE_URL}"
    assert plane_server.env["PLANE_WORKSPACE_SLUG"] == "nimbus"

    # Skill variables backing the platform-mentions link templates — the
    # base url resolves from the same ${PLANE_URL} at render time.
    assert cfg.skill_variables["plane_base_url"] == "${PLANE_URL}"
    assert cfg.skill_variables["plane_workspace_slug"] == "nimbus"


def test_nimbus_company_first_run_paths_tolerate_unset_vars(monkeypatch) -> None:
    """First-run condition: with NO ${VAR} exported, nothing may raise.

    ``load_company_config`` never reads the environment (Tier B stores
    ``${VAR}`` references verbatim), so a bare load cannot regress on env
    state — resolution happens at construction time inside the engine
    builders.  This drives the construction paths that DO resolve: the
    org build (the founder's ``${PLANE_FOUNDER_USER_ID}`` contact), the
    skill variables, and the per-role plane MCP launch spec.  The webhook
    secret, founder UUID, and seat tokens only exist *after* the
    provisioner runs, so all of these must tolerate every reference
    being unset.
    """
    raw = _COMPANY_YAML.read_text()
    for var in set(_ENV_REF_RE.findall(raw)):
        monkeypatch.delenv(var, raising=False)
    cfg = load_company_config(_COMPANY_YAML)
    assert cfg.name == "Nimbus"

    # Org build + contact resolution: the unresolved plane reference is
    # omitted (never emitted raw); literal identities still resolve.
    org = config_to_organization(cfg)
    founder = org.get_role("Jane Founder")
    assert founder is not None and founder.contact is not None
    resolved = dict(founder.contact.resolved_identities())
    assert "plane" not in resolved
    assert resolved["gitlab"] == "jane-founder"

    # Skill-variable resolution (values support ${ENV}) must not raise.
    assert resolve_skill_variables(cfg)["plane_workspace_slug"] == "nimbus"

    # The per-role plane MCP launch spec renders with the seat token
    # unset — the ${VAR} resolves to "" rather than exploding at spawn.
    plane_server = next(s for s in cfg.mcp_servers if s.name == "plane")
    spec = to_launch_spec(plane_server, {"PLANE_API_KEY": "${PLANE_TOKEN_SWE}"})
    assert spec["env"]["PLANE_API_KEY"] == ""
    # PLANE_URL was deleted above with every other referenced var, so the
    # base URL resolves to "" too — spawn still must not explode.
    assert spec["env"]["PLANE_BASE_URL"] == ""


def test_nimbus_company_has_no_atlassian_text() -> None:
    raw = _COMPANY_YAML.read_text()
    match = re.search(r"(?i)jira|confluence|atlassian|forge", raw)
    assert match is None, (
        f"Atlassian remnant {match.group(0)!r} in examples/nimbus.company.yaml"
    )


def test_bundled_tool_skills_parse() -> None:
    files = sorted(_TOOL_SKILLS_DIR.glob("*.md"))
    assert files, "expected bundled example skills under examples/tool-skills/"

    skills = {}
    for path in files:
        skill = parse_skill(path.read_text())
        assert skill.key not in skills, f"duplicate skill key {skill.key!r}"
        skills[skill.key] = skill

    # The Plane orientation one-pager: server-wide AND enforced (the
    # mcp:github / mcp:gitlab precedent).
    plane = skills["mcp:plane"]
    assert plane.trigger.mcp_server == "plane"
    assert plane.required is True

    # platform_mentions stays enforced but TOOL-scoped: exactly the
    # write tools (pinned from plane-mcp-server 0.2.10) — a server-wide
    # trigger would gate every Plane read behind the load guard.
    mentions = skills["skill:platform_mentions"]
    assert mentions.required is True
    leaf_tools = {leaf.tool for leaf in mentions.trigger.any_of}
    assert None not in leaf_tools, "platform_mentions must not use server-wide leaves"
    assert leaf_tools == {
        "slack_conversations_add_message",
        "create_work_item_comment",
        "update_work_item_comment",
        "create_work_item",
        "update_work_item",
        "create_page",
    }

    # Advisory practice skills went server-wide on Plane, so they must
    # be required: false — otherwise they'd gate every Plane tool call.
    for key in ("skill:getting_unstuck", "skill:retrieval_research"):
        skill = skills[key]
        assert skill.required is False, key
        assert any(leaf.mcp_server == "plane" for leaf in skill.trigger.any_of), key

    # code_runtime triggers on the org's real code host (GitLab).
    code_runtime = skills["skill:code_runtime"]
    assert code_runtime.trigger.mcp_server == "gitlab"
    assert code_runtime.required is False


def test_nimbus_docs_layout() -> None:
    files = sorted(_DOCS_DIR.rglob("*.md"))
    assert files, "expected knowledge docs under examples/nimbus-docs/"

    dirs_seen: set[str] = set()
    for path in files:
        parent = path.parent.name
        dirs_seen.add(parent)
        frontmatter, body = split_optional_frontmatter(path.read_text())
        # Knowledge docs, never skills: the importer routes by parent dir.
        assert classify(frontmatter) == "doc", path
        _, title, _ = parse_doc_file(path.read_text())
        assert title, path

    # Parent dir = Plane project identifier (the importer's routing
    # convention).
    assert dirs_seen <= {"LEAD", "ENG", "PROD"}
    for project in ("LEAD", "ENG", "PROD"):
        assert (_DOCS_DIR / project / "Onboarding.md").is_file(), (
            f"each project dir needs an Onboarding.md ({project} is missing one)"
        )


def test_nimbus_docs_have_no_atlassian_text() -> None:
    """Standing drift guard: the example org's tracker is Plane and its
    code host is GitLab, so no Atlassian or GitHub reference belongs in
    the knowledge docs.

    ``github.com/crewlet/crewlet`` links are exempt — they point at this
    project's own tracker, not at the example org's code host.
    """
    pattern = re.compile(r"(?i)jira|confluence|atlassian|github")
    for path in sorted(_DOCS_DIR.rglob("*.md")):
        text = path.read_text().replace("github.com/crewlet/crewlet", "")
        match = pattern.search(text)
        assert match is None, f"{path}: remnant {match.group(0)!r}"
