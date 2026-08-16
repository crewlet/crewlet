"""Authentication helpers for the ``cli-agent`` backend.

Covers the two paths an operator actually hits — capturing a headless
token, and moving a login onto another host — plus the honesty
guarantee: a profile with no credential login must say so rather than
pretend.
"""

from __future__ import annotations

import base64
import io
import stat
import sys
import tarfile

import pytest

from crewlet.providers.llm import cli_login
from crewlet.providers.llm.cli_profiles import CLIAgentProfile, StdinLoginSpec
from crewlet.providers.llm.cli_workspace import CLIWorkspaceManager


def make_workspace(tmp_path, **profile_kwargs):
    base = {
        "name": "fake",
        "binary": "fakecli",
        "complete_args": ["--json"],
        "credential_paths": [".fake/auth.json"],
        "volatile_paths": [".fake/sessions"],
        "token_env": "FAKE_OAUTH",
        "token_capture_args": ["setup-token"],
        "login_args": ["login"],
        "logout_args": ["logout"],
        "status_args": ["whoami"],
    }
    base.update(profile_kwargs)
    profile = CLIAgentProfile(**base)
    workspace = CLIWorkspaceManager(
        provider_key="default", profile=profile, state_dir=tmp_path / "state"
    )
    workspace.ensure_layout()
    return workspace, profile


def install(tmp_path, body: str, monkeypatch, name: str = "fakecli") -> None:
    bindir = tmp_path / "bin"
    bindir.mkdir(exist_ok=True)
    script = bindir / name
    script.write_text(f"#!{sys.executable}\n{body}")
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("PATH", str(bindir))


class TestBundleName:
    @pytest.mark.parametrize(
        "key,expected",
        [
            ("default", "CREWLET_LLM_CLI_DEFAULT_CREDENTIALS"),
            ("claude-cli", "CREWLET_LLM_CLI_CLAUDE_CLI_CREDENTIALS"),
            ("--", "CREWLET_LLM_CLI_DEFAULT_CREDENTIALS"),
        ],
    )
    def test_slug(self, key, expected):
        assert cli_login.bundle_env_name(key) == expected


class TestTokenCapture:
    def test_pattern_extracts_the_token(self):
        out = "Paste this token:\n\nsk-ant-oat01-" + "A" * 40 + "\n\nDone.\n"
        token = cli_login.extract_token(out, r"(sk-ant-oat[0-9]{2}-[A-Za-z0-9_-]{20,})")
        assert token == "sk-ant-oat01-" + "A" * 40

    def test_no_pattern_takes_the_last_line(self):
        assert cli_login.extract_token("banner\n\ntok-123\n", "") == "tok-123"

    def test_no_match_returns_empty(self):
        assert cli_login.extract_token("nothing here", r"(tok-\w+)") == ""

    def test_capture_runs_the_command(self, tmp_path, monkeypatch):
        install(tmp_path, "print('tok-abc')", monkeypatch)
        workspace, profile = make_workspace(tmp_path)
        result, token = cli_login.capture_token(workspace, profile)
        assert result.ok
        assert token == "tok-abc"

    def test_capture_on_a_profile_without_one_explains(self, tmp_path, monkeypatch):
        install(tmp_path, "pass", monkeypatch)
        workspace, profile = make_workspace(tmp_path, token_capture_args=[])
        result, token = cli_login.capture_token(workspace, profile)
        assert not result.ok
        assert token == ""
        assert "no token-minting command" in result.output


class TestCredentialLogin:
    def test_absent_spec_says_why_rather_than_failing_silently(
        self, tmp_path, monkeypatch
    ):
        """The user-facing promise: where a vendor has no credential
        grant, Crewlet says so instead of faking one."""
        install(tmp_path, "pass", monkeypatch)
        workspace, profile = make_workspace(tmp_path)
        result = cli_login.credential_login(
            workspace, profile, username="a@b.c", password="hunter2"
        )
        assert not result.ok
        assert "browser OAuth flow" in result.output
        assert "stdin_login" in result.output

    def test_declared_spec_is_driven_over_stdin(self, tmp_path, monkeypatch):
        script = """
import sys
creds = sys.stdin.read().strip()
print("args=" + " ".join(sys.argv[1:]) + " creds=" + creds)
"""
        install(tmp_path, script, monkeypatch)
        workspace, profile = make_workspace(
            tmp_path,
            stdin_login=StdinLoginSpec(
                args=["auth", "login", "--user", "{username}"],
                stdin_template="{password}\n",
            ),
        )
        result = cli_login.credential_login(
            workspace, profile, username="a@b.c", password="hunter2"
        )
        assert result.ok
        assert "--user a@b.c" in result.stdout
        assert "creds=hunter2" in result.stdout

    def test_spec_can_pass_the_credential_through_the_environment(
        self, tmp_path, monkeypatch
    ):
        script = """
import os
print(os.environ.get("GATEWAY_PASSWORD", "ABSENT"))
"""
        install(tmp_path, script, monkeypatch)
        workspace, profile = make_workspace(
            tmp_path,
            stdin_login=StdinLoginSpec(
                args=["login"], env={"GATEWAY_PASSWORD": "{password}"}
            ),
        )
        result = cli_login.credential_login(
            workspace, profile, username="u", password="s3cret"
        )
        assert "s3cret" in result.stdout


class TestBundles:
    def _seed(self, workspace, content: bytes = b'{"token": "v1"}'):
        target = workspace.credential_dir / ".fake" / "auth.json"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(content)
        return target

    def test_round_trip(self, tmp_path):
        workspace, profile = make_workspace(tmp_path)
        self._seed(workspace)
        blob, included = cli_login.export_bundle(workspace, profile)
        assert included == [".fake/auth.json"]

        other, _ = make_workspace(tmp_path / "second")
        restored = cli_login.import_bundle(other, profile, blob)
        assert restored == [".fake/auth.json"]
        assert (other.credential_dir / ".fake" / "auth.json").read_bytes() == (
            b'{"token": "v1"}'
        )

    def test_export_is_byte_stable_for_identical_credentials(self, tmp_path):
        workspace, profile = make_workspace(tmp_path)
        self._seed(workspace)
        first, _ = cli_login.export_bundle(workspace, profile)
        second, _ = cli_login.export_bundle(workspace, profile)
        assert first == second

    def test_export_carries_only_credentials(self, tmp_path):
        workspace, profile = make_workspace(tmp_path)
        self._seed(workspace)
        sessions = workspace.credential_dir / ".fake" / "sessions"
        sessions.mkdir(parents=True, exist_ok=True)
        (sessions / "leak.json").write_text("a conversation")
        blob, included = cli_login.export_bundle(workspace, profile)
        assert included == [".fake/auth.json"]
        assert b"a conversation" not in base64.b64decode(blob)

    def test_restored_file_is_owner_only(self, tmp_path):
        workspace, profile = make_workspace(tmp_path)
        self._seed(workspace)
        blob, _ = cli_login.export_bundle(workspace, profile)
        other, _ = make_workspace(tmp_path / "second")
        cli_login.import_bundle(other, profile, blob)
        mode = (other.credential_dir / ".fake" / "auth.json").stat().st_mode
        assert stat.S_IMODE(mode) == 0o600

    @staticmethod
    def _hostile(name: str, payload: bytes = b"x", size: int | None = None) -> str:
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode="w:gz") as tar:
            info = tarfile.TarInfo(name)
            info.size = size if size is not None else len(payload)
            tar.addfile(info, io.BytesIO(payload))
        return base64.b64encode(raw.getvalue()).decode("ascii")

    @pytest.mark.parametrize(
        "name", ["../../etc/passwd", "/etc/passwd", ".ssh/id_rsa", "other/file"]
    )
    def test_paths_outside_the_profile_are_refused(self, tmp_path, name):
        workspace, profile = make_workspace(tmp_path)
        with pytest.raises(cli_login.BundleError, match="not a credential path"):
            cli_login.import_bundle(workspace, profile, self._hostile(name))

    def test_symlink_entries_are_refused(self, tmp_path):
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode="w:gz") as tar:
            info = tarfile.TarInfo(".fake/auth.json")
            info.type = tarfile.SYMTYPE
            info.linkname = "/etc/passwd"
            tar.addfile(info)
        blob = base64.b64encode(raw.getvalue()).decode("ascii")
        workspace, profile = make_workspace(tmp_path)
        with pytest.raises(cli_login.BundleError, match="non-file entry"):
            cli_login.import_bundle(workspace, profile, blob)

    def test_oversized_bundle_is_refused(self, tmp_path):
        workspace, profile = make_workspace(tmp_path)
        blob = self._hostile(".fake/auth.json", payload=b"y" * (5 * 1024 * 1024))
        with pytest.raises(cli_login.BundleError, match="implausibly large"):
            cli_login.import_bundle(workspace, profile, blob)

    def test_garbage_is_refused(self, tmp_path):
        workspace, profile = make_workspace(tmp_path)
        with pytest.raises(cli_login.BundleError, match="not valid base64"):
            cli_login.import_bundle(workspace, profile, "!!! not base64 !!!")
        with pytest.raises(cli_login.BundleError, match="readable archive"):
            cli_login.import_bundle(
                workspace, profile, base64.b64encode(b"nope").decode()
            )

    def test_restore_from_secret_never_raises(self, tmp_path):
        workspace, profile = make_workspace(tmp_path)
        assert cli_login.restore_from_secret(workspace, profile, "garbage") is False
        assert cli_login.restore_from_secret(workspace, profile, "") is False

    def test_restore_from_secret_does_not_clobber_a_live_login(self, tmp_path):
        workspace, profile = make_workspace(tmp_path)
        self._seed(workspace, b'{"token": "live"}')
        blob = self._hostile(".fake/auth.json", payload=b'{"token": "stale"}')
        assert cli_login.restore_from_secret(workspace, profile, blob) is False
        assert (workspace.credential_dir / ".fake" / "auth.json").read_bytes() == (
            b'{"token": "live"}'
        )


class TestLogout:
    def test_removes_credentials_even_if_the_cli_leaves_them(
        self, tmp_path, monkeypatch
    ):
        install(tmp_path, "print('bye')", monkeypatch)
        workspace, profile = make_workspace(tmp_path)
        target = workspace.credential_dir / ".fake" / "auth.json"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("{}")
        cli_login.logout(workspace, profile)
        assert not target.exists()


class TestDoctorReport:
    def test_missing_binary_is_the_only_problem_reported(self, tmp_path, monkeypatch):
        monkeypatch.setenv("PATH", str(tmp_path / "empty"))
        workspace, profile = make_workspace(tmp_path)
        report = cli_login.build_report(
            provider_key="default", profile=profile, workspace=workspace
        )
        assert not report.ok
        assert "not on PATH" in report.problems[0]
        assert len(report.problems) == 1

    def test_healthy_provider_has_no_problems(self, tmp_path, monkeypatch):
        install(tmp_path, "print('fakecli 1.2.3')", monkeypatch)
        workspace, profile = make_workspace(
            tmp_path, input_token_paths=[["usage", "in"]]
        )
        target = workspace.credential_dir / ".fake" / "auth.json"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("{}")
        report = cli_login.build_report(
            provider_key="default", profile=profile, workspace=workspace
        )
        assert report.ok, report.problems
        assert report.version == "fakecli 1.2.3"
        assert "fakecli 1.2.3" in cli_login.format_report(report)

    def test_estimated_usage_is_flagged(self, tmp_path, monkeypatch):
        install(tmp_path, "print('v1')", monkeypatch)
        workspace, profile = make_workspace(tmp_path)
        target = workspace.credential_dir / ".fake" / "auth.json"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("{}")
        report = cli_login.build_report(
            provider_key="default", profile=profile, workspace=workspace
        )
        assert any("estimate" in p for p in report.problems)

    def test_missing_login_is_reported(self, tmp_path, monkeypatch):
        install(tmp_path, "print('v1')", monkeypatch)
        workspace, profile = make_workspace(tmp_path)
        report = cli_login.build_report(
            provider_key="default", profile=profile, workspace=workspace
        )
        assert any("crewlet llm login default" in p for p in report.problems)

    def test_a_stored_token_counts_as_logged_in(self, tmp_path, monkeypatch):
        install(tmp_path, "print('v1')", monkeypatch)
        workspace, profile = make_workspace(tmp_path)
        report = cli_login.build_report(
            provider_key="default",
            profile=profile,
            workspace=workspace,
            subscription_token="tok",
        )
        assert not any("no login found" in p for p in report.problems)
