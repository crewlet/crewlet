"""Tests for ``crewlet schema`` and the machine-readable
``crewlet validate --json``.

Together these are the contract an editor, a CI job, or an AI-assisted
authoring loop drives: the schema says what the fields are, and the JSON
validator says exactly which one is wrong.  See
``docs/getting-started/ai-authoring.md``.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from crewlet.cli import build_config_schema, main

_REPO_ROOT = Path(__file__).resolve().parents[2]
_SCHEMA_DIR = _REPO_ROOT / "schema"
FIXTURES = Path(__file__).parent.parent / "fixtures"
MINIMAL_YAML = FIXTURES / "minimal.yaml"


class TestSchemaCommand:
    def test_company_schema_to_stdout(self, capsys: pytest.CaptureFixture[str]) -> None:
        assert main(["schema"]) == 0
        schema = json.loads(capsys.readouterr().out)
        assert schema["$schema"] == "https://json-schema.org/draft/2020-12/schema"
        assert schema["$id"].endswith("company.schema.json")
        assert "name" in schema["required"]

    def test_bootstrap_schema_to_stdout(
        self, capsys: pytest.CaptureFixture[str]
    ) -> None:
        assert main(["schema", "bootstrap"]) == 0
        schema = json.loads(capsys.readouterr().out)
        assert schema["$id"].endswith("bootstrap.schema.json")
        assert "providers" in schema["properties"]

    def test_output_flag_writes_file(self, tmp_path: Path) -> None:
        out = tmp_path / "nested" / "company.json"
        assert main(["schema", "company", "-o", str(out)]) == 0
        assert json.loads(out.read_text())["$id"].endswith("company.schema.json")

    def test_org_chart_is_fully_described(self) -> None:
        """The schema's whole point is describing what a founder actually
        authors.  ``roles`` / ``units`` / ``mcp_servers`` used to be
        ``list[dict[str, Any]]``, which produced a permissive
        ``{"type": "object"}`` — no field list, no typo detection.
        """
        schema = build_config_schema("company")
        assert schema["properties"]["units"]["items"]["$ref"].endswith("OrgUnitConfig")
        assert schema["properties"]["roles"]["items"]["$ref"].endswith("RoleConfig")
        assert schema["properties"]["mcp_servers"]["items"]["$ref"].endswith(
            "MCPServerConfig"
        )

        role = schema["$defs"]["RoleConfig"]
        assert role["additionalProperties"] is False
        for field in ("name", "goal", "backstory", "manages", "handle", "mcp_env"):
            assert field in role["properties"], field

        unit = schema["$defs"]["OrgUnitConfig"]
        assert unit["additionalProperties"] is False
        # Recursion: a unit's children are units.
        assert unit["properties"]["children"]["items"]["$ref"].endswith("OrgUnitConfig")


class TestCheckedInSchemasAreCurrent:
    """The files under ``schema/`` are generated artifacts.  A stale one
    is worse than none — an editor or agent would validate against a
    surface the loader no longer accepts — so CI regenerates and compares
    rather than trusting whoever last ran the command.
    """

    @pytest.mark.parametrize("tier", ["company", "bootstrap"])
    def test_checked_in_schema_matches_models(self, tier: str) -> None:
        path = _SCHEMA_DIR / f"{tier}.schema.json"
        assert path.exists(), f"missing {path}; run: crewlet schema {tier} -o {path}"
        assert json.loads(path.read_text()) == build_config_schema(tier), (
            f"{path} is stale — regenerate with: crewlet schema {tier} -o {path}"
        )


class TestSchemasWorkInRealTooling:
    """``crewlet schema`` exists to be consumed by editors (a
    ``# yaml-language-server: $schema=…`` modeline), CI, and AI authoring
    agents — none of which run Pydantic.  "Pydantic emitted something"
    is therefore not the guarantee that matters; these drive the output
    through a standards-compliant validator instead.
    """

    @pytest.mark.parametrize("tier", ["company", "bootstrap"])
    def test_output_is_valid_json_schema(self, tier: str) -> None:
        from jsonschema import Draft202012Validator

        Draft202012Validator.check_schema(build_config_schema(tier))

    def test_shipped_examples_validate_against_the_schema(self) -> None:
        import yaml
        from jsonschema import Draft202012Validator

        validator = Draft202012Validator(build_config_schema("company"))
        example = yaml.safe_load(
            (_REPO_ROOT / "examples" / "nimbus.company.yaml").read_text()
        )
        errors = [
            f"{list(e.path)}: {e.message}" for e in validator.iter_errors(example)
        ]
        assert errors == []

    def test_bootstrap_example_validates_against_the_schema(self) -> None:
        import yaml
        from jsonschema import Draft202012Validator

        validator = Draft202012Validator(build_config_schema("bootstrap"))
        example = yaml.safe_load(
            (_REPO_ROOT / "examples" / "nimbus.config.yaml").read_text()
        )
        errors = [
            f"{list(e.path)}: {e.message}" for e in validator.iter_errors(example)
        ]
        assert errors == []

    def test_editor_tooling_flags_a_role_typo(self) -> None:
        """What a founder actually sees in their editor before ever
        running the CLI."""
        import yaml
        from jsonschema import Draft202012Validator

        validator = Draft202012Validator(build_config_schema("company"))
        draft = yaml.safe_load("name: Acme\nroles:\n  - name: CEO\n    backstroy: x\n")
        messages = [e.message for e in validator.iter_errors(draft)]
        assert any("backstroy" in m for m in messages), messages


class TestValidateJson:
    def test_valid_company_reports_summary(
        self, capsys: pytest.CaptureFixture[str]
    ) -> None:
        assert main(["validate", str(MINIMAL_YAML), "--json"]) == 0
        result = json.loads(capsys.readouterr().out)
        assert result["valid"] is True
        assert result["tier"] == "company"
        assert result["errors"] == []
        assert result["summary"]["name"] == "Minimal Corp"
        assert result["summary"]["roles"] >= 1

    def test_field_typo_reports_exact_path(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        """The property that makes an automated fix loop converge: the
        error names the offending path, not just 'config invalid'."""
        bad = tmp_path / "company.yaml"
        bad.write_text("name: Acme\nroles:\n  - name: CEO\n    backstroy: oops\n")
        assert main(["validate", str(bad), "--json"]) == 1
        result = json.loads(capsys.readouterr().out)
        assert result["valid"] is False
        assert result["errors"] == [
            {
                "path": "roles.0.backstroy",
                "message": "Extra inputs are not permitted",
                "type": "extra_forbidden",
            }
        ]

    def test_multiple_errors_are_all_reported(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        """One round-trip per fix would be needlessly slow; every
        offending field is reported at once."""
        bad = tmp_path / "company.yaml"
        bad.write_text(
            "name: Acme\n"
            "units:\n"
            "  - name: Eng\n"
            "    leed: CTO\n"
            "    roles:\n"
            "      - name: Dev\n"
            "        lmm: fast\n"
        )
        assert main(["validate", str(bad), "--json"]) == 1
        paths = {e["path"] for e in json.loads(capsys.readouterr().out)["errors"]}
        assert paths == {"units.0.leed", "units.0.roles.0.lmm"}

    def test_org_model_error_reports_pathless_record(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        """Errors raised by the org models (not Pydantic field
        validation) still come back in the same shape."""
        bad = tmp_path / "company.yaml"
        bad.write_text(
            "name: Acme\n"
            "roles:\n"
            "  - name: Jane\n"
            "    kind: human\n"  # human seat with no contact identity
        )
        assert main(["validate", str(bad), "--json"]) == 1
        errors = json.loads(capsys.readouterr().out)["errors"]
        assert len(errors) == 1
        assert "contact" in errors[0]["message"]

    def test_missing_file_reports_json_not_traceback(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        assert main(["validate", str(tmp_path / "nope.yaml"), "--json"]) == 1
        result = json.loads(capsys.readouterr().out)
        assert result["valid"] is False
        assert "not found" in result["errors"][0]["message"]

    def test_yaml_syntax_error_is_reported(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        bad = tmp_path / "company.yaml"
        bad.write_text("name: Acme\nroles: [unclosed\n")
        assert main(["validate", str(bad), "--json"]) == 1
        assert json.loads(capsys.readouterr().out)["valid"] is False

    def test_validates_with_no_env_vars_set(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys
    ) -> None:
        """The property that makes offline authoring work: Tier B keeps
        ``${VAR}`` references verbatim, so a config validates fully
        before a single secret exists.
        """
        monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
        monkeypatch.delenv("OPENAI_API_KEY", raising=False)
        draft = tmp_path / "company.yaml"
        draft.write_text(
            "name: Acme\n"
            "providers:\n"
            "  llm:\n"
            "    default:\n"
            "      type: anthropic\n"
            "      model: claude-sonnet-5\n"
            '      api_keys: ["${ANTHROPIC_API_KEY}"]\n'
            "roles:\n"
            "  - name: CEO\n"
        )
        assert main(["validate", str(draft), "--json"]) == 0
        assert json.loads(capsys.readouterr().out)["valid"] is True


class TestValidateTierDetection:
    def test_bootstrap_file_autodetected(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        """Tier A has no ``name``; Tier B requires it. The split is
        exact, not a heuristic."""
        boot = tmp_path / "config.yaml"
        boot.write_text(
            "debug: true\n"
            "providers:\n"
            "  queue:\n"
            "    type: pulsar\n"
            '    url: "pulsar://localhost:6650"\n'
            "api:\n"
            "  port: 8000\n"
        )
        assert main(["validate", str(boot), "--json"]) == 0
        result = json.loads(capsys.readouterr().out)
        assert result["tier"] == "bootstrap"
        assert result["summary"]["api_port"] == 8000

    def test_explicit_tier_overrides_detection(
        self, capsys: pytest.CaptureFixture[str]
    ) -> None:
        """Forcing the wrong tier fails loudly rather than silently
        validating against the other model."""
        assert (
            main(["validate", str(MINIMAL_YAML), "--tier", "bootstrap", "--json"]) == 1
        )
        assert json.loads(capsys.readouterr().out)["tier"] == "bootstrap"

    def test_text_mode_reports_tier_and_paths(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        bad = tmp_path / "company.yaml"
        bad.write_text("name: Acme\nroles:\n  - name: CEO\n    backstroy: oops\n")
        assert main(["validate", str(bad)]) == 1
        err = capsys.readouterr().err
        assert "company tier" in err
        assert "roles.0.backstroy" in err
