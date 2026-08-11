"""Tests for the ``crewlet slack provision`` CLI command (provision_cli)."""

from __future__ import annotations

import os
import time
from pathlib import Path
from typing import Any

import pytest

from crewlet.cli import main
from crewlet.slack import provision_cli
from crewlet.slack.api import AppCredentials, ConfigTokenPair
from crewlet.slack.provision import (
    CONFIG_REFRESH_TOKEN_VAR,
    CONFIG_TOKEN_EXPIRES_VAR,
    CONFIG_TOKEN_VAR,
)
from crewlet.slack.state import load_state

COMPANY_YAML = """
name: "Acme AI"
roles:
  - name: "Agent CEO"
    goal: "Lead"
    integrations:
      slack:
        bot_token: "${SLACK_BOT_TOKEN_CEO}"
        signing_secret: "${SLACK_SIGNING_SECRET_CEO}"
"""


@pytest.fixture
def company_path(tmp_path: Path) -> Path:
    path = tmp_path / "company.yaml"
    path.write_text(COMPANY_YAML)
    return path


@pytest.fixture(autouse=True)
def _clean_slack_env():
    saved = {
        var: os.environ.pop(var, None)
        for var in (
            "SLACK_BOT_TOKEN_CEO",
            "SLACK_SIGNING_SECRET_CEO",
            CONFIG_TOKEN_VAR,
            CONFIG_REFRESH_TOKEN_VAR,
            CONFIG_TOKEN_EXPIRES_VAR,
        )
    }
    yield
    for var, value in saved.items():
        if value is None:
            os.environ.pop(var, None)
        else:
            os.environ[var] = value


class _FakeManifestClient:
    """Async-context-manager stand-in for SlackManifestClient."""

    validated: list[dict[str, Any]] = []
    created: list[dict[str, Any]] = []
    rotations: list[str] = []
    create_error: str = ""

    def __init__(self) -> None:
        type(self).validated = []
        type(self).created = []
        type(self).rotations = []
        type(self).create_error = ""

    async def __aenter__(self) -> _FakeManifestClient:
        return self

    async def __aexit__(self, *exc: object) -> None:
        return None

    async def rotate_config_token(self, refresh_token: str) -> ConfigTokenPair:
        type(self).rotations.append(refresh_token)
        return ConfigTokenPair(
            token="xoxe.rotated",
            refresh_token="xoxe-1-next",
            expires_at=int(time.time()) + 43200,
        )

    async def validate_manifest(
        self, token: str, manifest: dict[str, Any], app_id: str = ""
    ) -> None:
        type(self).validated.append(manifest)

    async def create_app(self, token: str, manifest: dict[str, Any]) -> AppCredentials:
        from crewlet.slack.api import SlackApiError

        if type(self).create_error:
            raise SlackApiError("apps.manifest.create", type(self).create_error)
        type(self).created.append(manifest)
        n = len(type(self).created)
        return AppCredentials(
            app_id=f"A{n}",
            client_id=f"c{n}",
            client_secret=f"s{n}",
            signing_secret=f"ss{n}",
        )

    async def update_app(
        self, token: str, app_id: str, manifest: dict[str, Any]
    ) -> None:
        pass


def _provision_args(company_path: Path, *extra: str) -> list[str]:
    return [
        "slack",
        "provision",
        str(company_path),
        "--base-url",
        "https://x.example",
        *extra,
    ]


def test_missing_company_config(capsys: pytest.CaptureFixture[str]) -> None:
    result = main(
        ["slack", "provision", "/nonexistent.yaml", "--base-url", "https://x.example"]
    )
    assert result == 1
    assert "not found" in capsys.readouterr().err


def test_base_url_must_be_https(
    company_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    result = main(
        ["slack", "provision", str(company_path), "--base-url", "http://x.example"]
    )
    assert result == 1
    assert "HTTPS" in capsys.readouterr().err


def test_no_slack_seats(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    path = tmp_path / "company.yaml"
    path.write_text('name: "Acme"\nroles:\n  - name: Solo\n    goal: g\n')
    result = main(["slack", "provision", str(path), "--base-url", "https://x.example"])
    assert result == 1
    assert "No Slack-enabled agent seats" in capsys.readouterr().err


def test_unknown_handle_filter(
    company_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    result = main(_provision_args(company_path, "--handles", "nope"))
    assert result == 1
    assert "nope" in capsys.readouterr().err


def test_skip_install_provisions_and_writes_env(
    company_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    monkeypatch.setattr(provision_cli, "SlackManifestClient", _FakeManifestClient)
    monkeypatch.setenv(CONFIG_TOKEN_VAR, "xoxe.access")
    # The default destination is whatever `_env.env_file_for` names — the
    # same rule `crewlet run` reads by — so run from the config directory,
    # where a sibling .env and ./.env are the same file.
    monkeypatch.chdir(company_path.parent)

    result = main(_provision_args(company_path, "--skip-install"))
    assert result == 0
    out = capsys.readouterr().out
    assert "agent-ceo: created (A1)" in out

    env_text = (company_path.parent / ".env").read_text()
    assert "SLACK_SIGNING_SECRET_CEO=ss1" in env_text
    assert "SLACK_BOT_TOKEN_CEO" not in env_text

    ledger = load_state(company_path.parent / "slack-apps.json")
    assert ledger.apps["agent-ceo"].app_id == "A1"


def test_env_file_flag_is_read_and_written(
    company_path: Path,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """--env-file is one file for BOTH directions: run 2 must see what
    run 1 persisted there (the old split write-only behaviour stranded
    the rotated config-token pair in a file no run ever read)."""
    monkeypatch.setattr(provision_cli, "SlackManifestClient", _FakeManifestClient)
    custom_env = tmp_path / "secure" / "prod.env"
    monkeypatch.setenv(CONFIG_REFRESH_TOKEN_VAR, "xoxe-1-bootstrap")

    result = main(
        _provision_args(company_path, "--skip-install", "--env-file", str(custom_env))
    )
    assert result == 0
    assert _FakeManifestClient.rotations == ["xoxe-1-bootstrap"]
    env_text = custom_env.read_text()
    assert f"{CONFIG_REFRESH_TOKEN_VAR}=xoxe-1-next" in env_text
    assert "SLACK_SIGNING_SECRET_CEO=ss1" in env_text
    # No sibling .env was created — everything went to --env-file.
    assert not (company_path.parent / ".env").exists()

    # Run 2 reads the SAME file: the fresh (non-expired) token is used,
    # so no second rotation happens even though the shell still exports
    # the stale bootstrap refresh token.
    result = main(
        _provision_args(company_path, "--skip-install", "--env-file", str(custom_env))
    )
    assert result == 0
    # The client is re-constructed inside main(), resetting the class
    # tallies — an empty list proves run 2 performed no rotation.
    assert _FakeManifestClient.rotations == []


def test_dry_run_validates_without_creating_or_rotating(
    company_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    monkeypatch.setattr(provision_cli, "SlackManifestClient", _FakeManifestClient)
    monkeypatch.setenv(CONFIG_TOKEN_VAR, "xoxe.access")
    monkeypatch.setenv(CONFIG_REFRESH_TOKEN_VAR, "xoxe-1-current")

    result = main(_provision_args(company_path, "--dry-run"))
    assert result == 0
    assert len(_FakeManifestClient.validated) == 1
    assert not _FakeManifestClient.created
    # A usable access token means the preview consumed NO rotation and
    # left the credentials untouched.
    assert not _FakeManifestClient.rotations
    assert not (company_path.parent / "slack-apps.json").exists()
    out = capsys.readouterr().out
    assert "would create" in out
    assert "manifest OK" in out


def test_failed_agent_yields_nonzero_exit(
    company_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    class _FailingCreateClient(_FakeManifestClient):
        def __init__(self) -> None:
            super().__init__()
            type(self).create_error = "invalid_manifest"

    monkeypatch.setattr(provision_cli, "SlackManifestClient", _FailingCreateClient)
    monkeypatch.setenv(CONFIG_TOKEN_VAR, "xoxe.access")

    result = main(_provision_args(company_path, "--skip-install"))
    assert result == 1
    out = capsys.readouterr().out
    assert "FAILED" in out
    assert "invalid_manifest" in out


def test_missing_config_token_is_actionable(
    company_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    monkeypatch.setattr(provision_cli, "SlackManifestClient", _FakeManifestClient)
    result = main(_provision_args(company_path, "--skip-install"))
    assert result == 1
    assert "App Configuration Tokens" in capsys.readouterr().err
