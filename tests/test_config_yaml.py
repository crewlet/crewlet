"""Round-trip tests for the CompanyConfig YAML serialiser."""

from __future__ import annotations

from crewlet.config import CompanyConfig
from crewlet.config_yaml import (
    company_config_from_yaml,
    company_config_to_dict,
    company_config_to_yaml,
)


def test_round_trip_minimal() -> None:
    cfg = CompanyConfig(name="Acme")
    text = company_config_to_yaml(cfg)
    loaded = company_config_from_yaml(text)
    assert loaded.name == "Acme"


def test_round_trip_with_policies_and_roles() -> None:
    cfg = CompanyConfig(
        name="Acme",
        mission="Build cool stuff",
        policies=["Be kind", "Ship code"],
        roles=[{"name": "Founder", "goal": "set direction"}],
    )
    text = company_config_to_yaml(cfg)
    loaded = company_config_from_yaml(text)
    assert loaded.name == "Acme"
    assert loaded.mission == "Build cool stuff"
    assert loaded.policies == ["Be kind", "Ship code"]
    assert [(r.name, r.goal) for r in loaded.roles] == [("Founder", "set direction")]
    # Data-level round trip: the re-parsed roles equal the originals.
    assert loaded.roles == cfg.roles


def test_yaml_output_excludes_none_fields() -> None:
    cfg = CompanyConfig(name="Acme")
    text = company_config_to_yaml(cfg)
    # Optional integration sections default to None; should NOT appear.
    assert "jira:" not in text
    assert "confluence:" not in text


def test_preserves_env_var_references_verbatim() -> None:
    """``${VAR}`` references survive round-trip without resolution."""
    cfg = CompanyConfig(
        name="Acme",
        integrations={  # type: ignore[arg-type]
            "jira": {
                "url": "${JIRA_URL}",
                "token": "${JIRA_TOKEN}",
                "email": "${JIRA_EMAIL}",
            }
        },
    )
    text = company_config_to_yaml(cfg)
    assert "${JIRA_URL}" in text
    loaded = company_config_from_yaml(text)
    assert loaded.integrations.jira is not None
    assert loaded.integrations.jira.url == "${JIRA_URL}"


def test_from_yaml_rejects_empty_body() -> None:
    import pytest

    with pytest.raises(ValueError, match="empty"):
        company_config_from_yaml("")


def test_from_yaml_rejects_non_mapping() -> None:
    import pytest

    with pytest.raises(ValueError, match="mapping"):
        company_config_from_yaml("- a\n- b\n")


def test_to_dict_round_trip() -> None:
    cfg = CompanyConfig(name="Acme", token_budget=100)
    data = company_config_to_dict(cfg)
    assert data["name"] == "Acme"
    assert data["token_budget"] == 100
    rebuilt = CompanyConfig.model_validate(data)
    assert rebuilt.token_budget == 100


def test_yaml_export_serialises_enum_fields() -> None:
    """``yaml.safe_dump`` resolves representers by exact type, so a
    ``StrEnum`` member reaches it as an un-representable object.  Dumping
    in ``mode="python"`` therefore made ``crewlet config export`` and
    ``GET /config?format=yaml`` raise ``RepresenterError`` outright on
    any config that set ``integrations.slack.typing_status`` (or, once
    roles were typed, on *every* config — ``role.kind`` is an enum too).
    """
    cfg = CompanyConfig.model_validate(
        {
            "name": "Acme",
            "integrations": {"slack": {"typing_status": "always"}},
            "roles": [
                {"name": "Founder", "kind": "human", "contact": {"slack_user_id": "U1"}}
            ],
        }
    )
    text = company_config_to_yaml(cfg)
    assert "typing_status: always" in text
    assert "kind: human" in text
    # And it round-trips back to an equal config.
    assert company_config_from_yaml(text) == cfg
