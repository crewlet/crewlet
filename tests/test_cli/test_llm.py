"""``crewlet llm`` — the operator surface for subscription CLI backends.

Drives the real ``main([...])`` entry point against a company YAML and a
scripted fake CLI, so the argument wiring, the company-config lookup,
and the operator-facing messages are all covered.  See
``docs/concepts/subscription-llm-backends.md``.
"""

from __future__ import annotations

import stat
import sys
from pathlib import Path

import pytest
import yaml

from crewlet.cli import main

COMPANY = {
    "name": "Acme",
    "providers": {
        "llm": {
            "metered": {
                "type": "anthropic",
                "model": "claude-sonnet-5",
                "api_keys": ["sk-ant-x"],
            },
            "subscription": {
                "type": "cli-agent",
                "model": "sonnet",
                "cli": {
                    "agent": "custom",
                    "overrides": {
                        "binary": "fakecli",
                        "complete_args": ["--json"],
                        "output_format": "json",
                        "text_paths": [["result"]],
                        "input_token_paths": [["usage", "input_tokens"]],
                        "output_token_paths": [["usage", "output_tokens"]],
                        "credential_paths": [".fake/auth.json"],
                        "volatile_paths": [".fake/sessions"],
                        "version_args": ["--version"],
                        "token_env": "FAKE_OAUTH",
                        "token_capture_args": ["setup-token"],
                        "status_args": ["whoami"],
                    },
                },
            },
        }
    },
    "roles": [{"name": "CEO", "handle": "ceo"}],
}

SMOKE_OK = """
import json, sys
sys.stdin.read()
envelope = {"message": "", "tool_calls": [
    {"name": "crewlet_smoke_check", "arguments": {"ok": True}}]}
print(json.dumps({
    "result": "```json\\n" + json.dumps(envelope) + "\\n```",
    "usage": {"input_tokens": 5, "output_tokens": 2},
}))
"""


@pytest.fixture
def company(tmp_path: Path) -> Path:
    payload = yaml.safe_load(yaml.safe_dump(COMPANY))
    payload["providers"]["llm"]["subscription"]["cli"]["state_dir"] = str(
        tmp_path / "state"
    )
    path = tmp_path / "company.yaml"
    path.write_text(yaml.safe_dump(payload))
    return path


def install(tmp_path: Path, body: str, monkeypatch) -> None:
    bindir = tmp_path / "bin"
    bindir.mkdir(exist_ok=True)
    script = bindir / "fakecli"
    script.write_text(f"#!{sys.executable}\n{body}")
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("PATH", str(bindir))


class TestList:
    def test_lists_only_cli_agent_providers(self, company, capsys):
        assert main(["llm", "list", "--company", str(company)]) == 0
        out = capsys.readouterr().out
        assert "subscription" in out
        # The API-key provider is not this command's business.
        assert "metered" not in out

    def test_empty_config_explains(self, tmp_path, capsys):
        path = tmp_path / "c.yaml"
        path.write_text(yaml.safe_dump({"name": "Acme"}))
        assert main(["llm", "list", "--company", str(path)]) == 0
        assert "no cli-agent LLM providers" in capsys.readouterr().out

    def test_missing_file_is_a_clean_error(self, tmp_path, capsys):
        assert main(["llm", "list", "--company", str(tmp_path / "nope.yaml")]) == 1
        assert "not found" in capsys.readouterr().err


class TestUnknownProvider:
    def test_names_the_ones_that_do_exist(self, company, capsys):
        assert main(["llm", "status", "typo", "--company", str(company)]) == 1
        err = capsys.readouterr().err
        assert "no cli-agent provider named 'typo'" in err
        assert "subscription" in err


class TestStatus:
    def test_forwards_to_the_cli(self, company, tmp_path, monkeypatch, capsys):
        install(tmp_path, "print('logged in as ops@example.com')", monkeypatch)
        assert main(["llm", "status", "subscription", "--company", str(company)]) == 0
        assert "ops@example.com" in capsys.readouterr().out


class TestDoctor:
    def test_reports_a_missing_binary_without_running_a_smoke(
        self, company, tmp_path, monkeypatch, capsys
    ):
        monkeypatch.setenv("PATH", str(tmp_path / "empty"))
        assert main(["llm", "doctor", "subscription", "--company", str(company)]) == 1
        out = capsys.readouterr().out
        assert "not on PATH" in out
        assert "smoke test" not in out

    def test_healthy_provider_passes_including_the_smoke_call(
        self, company, tmp_path, monkeypatch, capsys
    ):
        install(tmp_path, SMOKE_OK, monkeypatch)
        monkeypatch.setenv("FAKE_OAUTH", "tok-123")
        assert main(["llm", "doctor", "subscription", "--company", str(company)]) == 0
        out = capsys.readouterr().out
        assert "smoke test    : ok" in out
        assert "problems      : none" in out

    def test_a_model_that_ignores_the_contract_fails_the_smoke(
        self, company, tmp_path, monkeypatch, capsys
    ):
        """The whole reason the smoke test exists: flags and login can be
        perfect while the model still refuses to emit the envelope."""
        install(
            tmp_path,
            "import json; print(json.dumps({'result': 'Sure, I can help!'}))",
            monkeypatch,
        )
        monkeypatch.setenv("FAKE_OAUTH", "tok-123")
        assert main(["llm", "doctor", "subscription", "--company", str(company)]) == 1
        assert "no parseable tool call" in capsys.readouterr().out

    def test_no_smoke_skips_the_call(self, company, tmp_path, monkeypatch, capsys):
        install(tmp_path, "import sys; sys.exit(7)", monkeypatch)
        monkeypatch.setenv("FAKE_OAUTH", "tok-123")
        main(
            [
                "llm",
                "doctor",
                "subscription",
                "--company",
                str(company),
                "--no-smoke",
            ]
        )
        assert "smoke test" not in capsys.readouterr().out

    def test_checks_every_provider_when_none_is_named(
        self, company, tmp_path, monkeypatch, capsys
    ):
        install(tmp_path, SMOKE_OK, monkeypatch)
        monkeypatch.setenv("FAKE_OAUTH", "tok-123")
        assert main(["llm", "doctor", "--company", str(company)]) == 0
        assert capsys.readouterr().out.count("provider      :") == 1


class TestLogin:
    def test_interactive_login_reports_a_missing_credential_file(
        self, company, tmp_path, monkeypatch, capsys
    ):
        """A login command that exits 0 but writes nothing is the shape
        that would otherwise look successful and fail at the first turn."""
        install(tmp_path, "pass", monkeypatch)
        assert main(["llm", "login", "subscription", "--company", str(company)]) == 1
        assert "wrote no credential file" in capsys.readouterr().err

    def test_capture_token_without_a_pattern_match_explains(
        self, company, tmp_path, monkeypatch, capsys
    ):
        install(tmp_path, "print('')", monkeypatch)
        code = main(
            [
                "llm",
                "login",
                "subscription",
                "--company",
                str(company),
                "--capture-token",
            ]
        )
        assert code == 1
        assert "printed no token" in capsys.readouterr().err

    def test_print_token_refuses_a_terminal(
        self, company, tmp_path, monkeypatch, capsys
    ):
        install(tmp_path, "print('tok-abc')", monkeypatch)
        monkeypatch.setattr("sys.stdout.isatty", lambda: True, raising=False)
        code = main(
            [
                "llm",
                "login",
                "subscription",
                "--company",
                str(company),
                "--capture-token",
                "--print-token",
            ]
        )
        assert code == 1
        assert "refusing to print a credential" in capsys.readouterr().err

    def test_credential_login_on_a_browser_oauth_profile_explains(
        self, company, tmp_path, monkeypatch, capsys
    ):
        install(tmp_path, "pass", monkeypatch)
        monkeypatch.setattr("sys.stdin.isatty", lambda: False, raising=False)
        monkeypatch.setattr(
            "crewlet.providers.llm.cli_login.read_password", lambda *_: "pw"
        )
        code = main(
            [
                "llm",
                "login",
                "subscription",
                "--company",
                str(company),
                "--username",
                "ops@example.com",
                "--password-stdin",
            ]
        )
        assert code != 0
        assert "no username/password login to drive" in capsys.readouterr().out


class TestExportImport:
    def _login(self, company, tmp_path):
        import argparse

        from crewlet.cli_llm import _resolve_one

        _key, _cfg, _profile, workspace = _resolve_one(
            argparse.Namespace(company=company, provider="subscription")
        )
        target = workspace.credential_dir / ".fake" / "auth.json"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text('{"token": "v1"}')
        return workspace

    def test_export_without_a_login_explains(self, company, capsys):
        assert main(["llm", "export", "subscription", "--company", str(company)]) == 1
        assert "no credential files" in capsys.readouterr().err

    def test_export_refuses_a_terminal(self, company, tmp_path, monkeypatch, capsys):
        self._login(company, tmp_path)
        monkeypatch.setattr("sys.stdout.isatty", lambda: True, raising=False)
        assert main(["llm", "export", "subscription", "--company", str(company)]) == 1
        assert "refusing to print a credential bundle" in capsys.readouterr().err

    def test_export_then_import_round_trips(
        self, company, tmp_path, monkeypatch, capsys
    ):
        workspace = self._login(company, tmp_path)
        monkeypatch.setattr("sys.stdout.isatty", lambda: False, raising=False)
        assert main(["llm", "export", "subscription", "--company", str(company)]) == 0
        blob = capsys.readouterr().out.strip()

        credential = workspace.credential_dir / ".fake" / "auth.json"
        credential.unlink()

        import io

        monkeypatch.setattr("sys.stdin", io.StringIO(blob))
        monkeypatch.setattr("sys.stdin.isatty", lambda: False, raising=False)
        assert main(["llm", "import", "subscription", "--company", str(company)]) == 0
        assert credential.read_text() == '{"token": "v1"}'

    def test_import_from_a_terminal_explains(self, company, monkeypatch, capsys):
        monkeypatch.setattr("sys.stdin.isatty", lambda: True, raising=False)
        assert main(["llm", "import", "subscription", "--company", str(company)]) == 1
        assert "pipe the bundle on stdin" in capsys.readouterr().err


class TestLogout:
    def test_removes_the_credential_and_mentions_the_token(
        self, company, tmp_path, monkeypatch, capsys
    ):
        install(tmp_path, "pass", monkeypatch)
        import argparse

        from crewlet.cli_llm import _resolve_one

        _k, _c, _p, workspace = _resolve_one(
            argparse.Namespace(company=company, provider="subscription")
        )
        target = workspace.credential_dir / ".fake" / "auth.json"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("{}")

        assert main(["llm", "logout", "subscription", "--company", str(company)]) == 0
        assert not target.exists()
        assert "crewlet secrets unset FAKE_OAUTH" in capsys.readouterr().out
