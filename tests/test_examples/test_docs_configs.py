"""The YAML in the getting-started docs must actually load.

The quickstart's config is the canonical minimal company — the one a
founder types out by hand on their first day, and the shape the
`company-architect` skill is told to produce for a first pass. It lives
inline in markdown so it can be read in place, which means nothing would
otherwise stop it from drifting away from what the loader accepts.

This is a docs test on purpose: a broken example in the first page
someone reads costs more than a broken example in `examples/`.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest
import yaml

from crewlet.config import (
    BootstrapConfig,
    CompanyConfig,
    config_to_organization,
)
from crewlet.org.models import RoleKind

_REPO_ROOT = Path(__file__).resolve().parents[2]
_QUICKSTART = _REPO_ROOT / "docs" / "getting-started" / "quickstart.md"

_YAML_BLOCK_RE = re.compile(r"```yaml\n(.*?)```", re.DOTALL)
_ENV_REF_RE = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")


def _yaml_blocks(path: Path) -> list[str]:
    return _YAML_BLOCK_RE.findall(path.read_text())


@pytest.fixture(scope="module")
def quickstart_company() -> CompanyConfig:
    """The Tier B block — the only one with a top-level ``name``, which
    is exactly what distinguishes the tiers."""
    block = next(b for b in _yaml_blocks(_QUICKSTART) if b.startswith("name:"))
    return CompanyConfig.model_validate(yaml.safe_load(block))


@pytest.fixture(scope="module")
def quickstart_bootstrap() -> BootstrapConfig:
    block = next(
        b for b in _yaml_blocks(_QUICKSTART) if b.lstrip().startswith("debug:")
    )
    return BootstrapConfig.model_validate(yaml.safe_load(block))


def test_quickstart_company_validates(quickstart_company: CompanyConfig) -> None:
    config_to_organization(quickstart_company)


def test_quickstart_bootstrap_validates(quickstart_bootstrap: BootstrapConfig) -> None:
    assert quickstart_bootstrap.api.port > 0, (
        "the page promises the dashboard comes up with the engine, which "
        "needs an embedded API (api.port > 0)"
    )


def test_quickstart_has_four_agent_seats(quickstart_company: CompanyConfig) -> None:
    """The page title says four agents. The founder human seat is not an
    agent — it is never spawned — so adding it must not change the count."""
    org = config_to_organization(quickstart_company)
    agents = [r for r in org.all_roles() if r.kind == RoleKind.AGENT]
    assert len(agents) == 4


def test_quickstart_seats_are_reachable_from_the_top(
    quickstart_company: CompanyConfig,
) -> None:
    """Every agent seat should be managed by someone, or the hierarchy
    the page is teaching has a hole in it."""
    org = config_to_organization(quickstart_company)
    managed = {name for r in org.all_roles() for name in r.manages}
    unmanaged = [
        r.name
        for r in org.all_roles()
        if r.kind == RoleKind.AGENT and r.name not in managed
    ]
    assert unmanaged == [], f"agent seats nobody manages: {unmanaged}"


def test_quickstart_schedule_can_fire(quickstart_company: CompanyConfig) -> None:
    """With no integrations there is no inbound work; the page promises a
    turn within five minutes, and a schedule is the only thing that can
    deliver one."""
    org = config_to_organization(quickstart_company)
    schedules = [s for r in org.all_roles() for s in r.schedules]
    assert schedules, "no seat carries a schedule — nothing would ever run"
    assert any(s.enabled for s in schedules)


def test_quickstart_founder_seat_is_human_and_managed_by_nobody(
    quickstart_company: CompanyConfig,
) -> None:
    """Escalation has to terminate at a person: the founder seat holds
    the top of the chart and is addressable, not executable."""
    org = config_to_organization(quickstart_company)
    humans = [r for r in org.all_roles() if r.kind == RoleKind.HUMAN]
    assert len(humans) == 1
    founder = humans[0]
    assert founder.contact is not None, "a human seat needs a contact identity"
    assert founder.manages, "the founder seat must manage the top agent"


def test_quickstart_agent_seats_pin_their_handles(
    quickstart_company: CompanyConfig,
) -> None:
    """The page tells the reader handles are effectively permanent and to
    set them explicitly — the example has to model that, not rely on
    auto-derivation from a name they may rename."""
    org = config_to_organization(quickstart_company)
    missing = [
        r.name for r in org.all_roles() if r.kind == RoleKind.AGENT and not r.handle
    ]
    assert missing == [], f"agent seats without an explicit handle: {missing}"


def test_quickstart_holds_no_literal_secrets(
    quickstart_company: CompanyConfig,
) -> None:
    values = [k for p in quickstart_company.providers.llm.values() for k in p.api_keys]
    assert quickstart_company.providers.embeddings is not None
    values.append(quickstart_company.providers.embeddings.api_key)
    for value in values:
        assert _ENV_REF_RE.fullmatch(value), f"literal secret in the docs: {value!r}"


def test_quickstart_exports_every_env_var_its_config_references(
    quickstart_company: CompanyConfig,
) -> None:
    """A reader follows the page top to bottom. Every ``${VAR}`` in the
    config must appear in an ``export`` line somewhere on the page —
    an unset one resolves to an empty string and fails much later.
    """
    text = _QUICKSTART.read_text()
    referenced = set(_ENV_REF_RE.findall(str(quickstart_company.model_dump())))
    exported = set(re.findall(r"export\s+([A-Za-z_][A-Za-z0-9_]*)=", text))
    missing = referenced - exported
    assert missing == set(), f"config references vars the page never exports: {missing}"
