"""Isolation guarantees of the ``cli-agent`` workspace layer.

These are the tests that matter for the feature's core promise: many
seats on one subscription must not read each other's CLI memory, and no
turn may inherit the last one's session.
"""

from __future__ import annotations

import os

import pytest

from crewlet.providers.llm.cli_profiles import CLIAgentProfile
from crewlet.providers.llm.cli_workspace import (
    STATE_DIR_ENV,
    CLIWorkspaceManager,
    default_state_dir,
)


def make_profile(**kwargs) -> CLIAgentProfile:
    base = {
        "name": "test-cli",
        "binary": "testcli",
        "complete_args": ["--json"],
        "dir_env": {"TESTCLI_HOME": ".testcli"},
        "credential_paths": [".testcli/auth.json"],
        "volatile_paths": [".testcli/sessions", ".testcli/history.jsonl"],
        "seed_files": {".testcli/settings.json": '{"telemetry": false}'},
        "env": {"TESTCLI_QUIET": "1"},
    }
    base.update(kwargs)
    return CLIAgentProfile(**base)


def make_manager(tmp_path, **profile_kwargs) -> CLIWorkspaceManager:
    return CLIWorkspaceManager(
        provider_key="default",
        profile=make_profile(**profile_kwargs),
        state_dir=tmp_path / "state",
    )


class TestLayout:
    def test_default_state_dir_honours_the_env_override(self, tmp_path, monkeypatch):
        monkeypatch.setenv(STATE_DIR_ENV, str(tmp_path / "vol"))
        assert default_state_dir("default") == tmp_path / "vol" / "default"

    def test_provider_key_cannot_traverse_out_of_the_root(self, tmp_path, monkeypatch):
        monkeypatch.setenv(STATE_DIR_ENV, str(tmp_path))
        resolved = default_state_dir("../escape")
        assert resolved.parent == tmp_path
        assert ".." not in resolved.parts

    def test_directories_are_owner_only(self, tmp_path):
        manager = make_manager(tmp_path)
        manager.ensure_layout()
        mode = manager.credential_dir.stat().st_mode & 0o777
        assert mode == 0o700


class TestSeatIsolation:
    async def test_seats_get_distinct_homes(self, tmp_path):
        manager = make_manager(tmp_path)
        async with (
            manager.acquire("sarah-chen") as a,
            manager.acquire("marcus-rivera") as b,
        ):
            assert a.home != b.home
            assert a.work != b.work

    async def test_one_seats_session_is_invisible_to_another(self, tmp_path):
        manager = make_manager(tmp_path)
        async with manager.acquire("sarah-chen") as a:
            (a.home / ".testcli" / "sessions").mkdir(parents=True, exist_ok=True)
            (a.home / ".testcli" / "sessions" / "s1.json").write_text("secret plan")
        async with manager.acquire("marcus-rivera") as b:
            assert not (b.home / ".testcli" / "sessions" / "s1.json").exists()

    async def test_a_seat_does_not_inherit_its_own_previous_session(self, tmp_path):
        manager = make_manager(tmp_path)
        async with manager.acquire("sarah-chen") as a:
            sessions = a.home / ".testcli" / "sessions"
            sessions.mkdir(parents=True, exist_ok=True)
            (sessions / "s1.json").write_text("turn one")
            (a.home / ".testcli" / "history.jsonl").write_text("turn one")
        async with manager.acquire("sarah-chen") as a2:
            assert not (a2.home / ".testcli" / "sessions" / "s1.json").exists()
            assert not (a2.home / ".testcli" / "history.jsonl").exists()

    async def test_work_dir_is_removed_after_the_call(self, tmp_path):
        manager = make_manager(tmp_path)
        async with manager.acquire("sarah-chen") as ws:
            (ws.work / "scratch.txt").write_text("x")
            work = ws.work
        assert not work.exists()

    async def test_seed_files_are_written_every_generation(self, tmp_path):
        manager = make_manager(tmp_path)
        async with manager.acquire("sarah-chen") as ws:
            settings = ws.home / ".testcli" / "settings.json"
            assert settings.read_text() == '{"telemetry": false}'
            settings.write_text("tampered")
        async with manager.acquire("sarah-chen") as ws:
            assert (ws.home / ".testcli" / "settings.json").read_text() != "tampered"

    async def test_purge_seat_removes_everything(self, tmp_path):
        manager = make_manager(tmp_path)
        async with manager.acquire("sarah-chen"):
            pass
        assert manager.seat_root("sarah-chen").exists()
        manager.purge_seat("sarah-chen")
        assert not manager.seat_root("sarah-chen").exists()


class TestGenerations:
    async def test_concurrent_calls_on_one_seat_share_the_generation(self, tmp_path):
        """Batched sub-agents belong to the same agent, so they may share
        a home — and the first one's prune must not delete the second
        one's live session."""
        manager = make_manager(tmp_path)
        async with manager.acquire("sarah-chen") as first:
            marker = first.home / ".testcli" / "sessions" / "live.json"
            marker.parent.mkdir(parents=True, exist_ok=True)
            marker.write_text("in flight")
            async with manager.acquire("sarah-chen") as second:
                assert second.home == first.home
                assert marker.exists()
            # Inner call finished; the generation is still open.
            assert marker.exists()
        assert not marker.exists()

    async def test_credentials_seed_in_and_refresh_back(self, tmp_path):
        manager = make_manager(tmp_path)
        manager.ensure_layout()
        shared = manager.credential_dir / ".testcli" / "auth.json"
        shared.parent.mkdir(parents=True, exist_ok=True)
        shared.write_text('{"token": "v1"}')

        async with manager.acquire("sarah-chen") as ws:
            seat_copy = ws.home / ".testcli" / "auth.json"
            assert seat_copy.read_text() == '{"token": "v1"}'
            # The CLI refreshes its OAuth token mid-run.
            seat_copy.write_text('{"token": "v2"}')
        assert shared.read_text() == '{"token": "v2"}'

    async def test_refreshed_credential_reaches_another_seat(self, tmp_path):
        manager = make_manager(tmp_path)
        manager.ensure_layout()
        shared = manager.credential_dir / ".testcli" / "auth.json"
        shared.parent.mkdir(parents=True, exist_ok=True)
        shared.write_text('{"token": "v1"}')
        async with manager.acquire("a") as ws:
            (ws.home / ".testcli" / "auth.json").write_text('{"token": "v2"}')
        async with manager.acquire("b") as ws:
            assert (ws.home / ".testcli" / "auth.json").read_text() == '{"token": "v2"}'

    async def test_credentials_survive_the_prune(self, tmp_path):
        manager = make_manager(tmp_path)
        manager.ensure_layout()
        shared = manager.credential_dir / ".testcli" / "auth.json"
        shared.parent.mkdir(parents=True, exist_ok=True)
        shared.write_text("{}")
        async with manager.acquire("a"):
            pass
        assert shared.exists()

    async def test_a_logged_out_provider_is_not_re_adopted_from_a_seat(self, tmp_path):
        """`crewlet llm logout` deletes the shared login. A leftover seat
        copy must not put it back, and must not survive the next
        generation."""
        manager = make_manager(tmp_path)
        async with manager.acquire("a") as ws:
            (ws.home / ".testcli").mkdir(parents=True, exist_ok=True)
            (ws.home / ".testcli" / "auth.json").write_text("revoked")
        assert not (manager.credential_dir / ".testcli" / "auth.json").exists()
        async with manager.acquire("a") as ws:
            assert not (ws.home / ".testcli" / "auth.json").exists()


class TestEnvironment:
    async def test_host_secrets_are_not_inherited(self, tmp_path, monkeypatch):
        monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
        monkeypatch.setenv("SLACK_BOT_TOKEN", "xoxb-should-not-leak")
        manager = make_manager(tmp_path)
        async with manager.acquire("a") as ws:
            assert "ANTHROPIC_API_KEY" not in ws.env
            assert "SLACK_BOT_TOKEN" not in ws.env

    async def test_allowlisted_host_vars_pass_through(self, tmp_path, monkeypatch):
        monkeypatch.setenv("HTTPS_PROXY", "http://proxy.example:8080")
        manager = make_manager(tmp_path)
        async with manager.acquire("a") as ws:
            assert ws.env["PATH"] == os.environ["PATH"]
            assert ws.env["HTTPS_PROXY"] == "http://proxy.example:8080"

    async def test_profile_passthrough_extends_the_allowlist(
        self, tmp_path, monkeypatch
    ):
        monkeypatch.setenv("GOOGLE_CLOUD_PROJECT", "acme-ai")
        manager = make_manager(tmp_path, passthrough_env=["GOOGLE_CLOUD_PROJECT"])
        async with manager.acquire("a") as ws:
            assert ws.env["GOOGLE_CLOUD_PROJECT"] == "acme-ai"

    async def test_home_and_xdg_point_into_the_seat(self, tmp_path):
        manager = make_manager(tmp_path)
        async with manager.acquire("sarah-chen") as ws:
            assert ws.env["HOME"] == str(ws.home)
            assert ws.env["XDG_CONFIG_HOME"] == str(ws.home / ".config")
            assert ws.env["TESTCLI_HOME"] == str(ws.home / ".testcli")
            assert ws.env["TMPDIR"].startswith(str(ws.work))

    async def test_cache_is_shared_per_seat_not_per_generation(self, tmp_path):
        manager = make_manager(tmp_path)
        async with manager.acquire("sarah-chen") as ws:
            assert ws.env["XDG_CACHE_HOME"] == str(
                manager.seat_root("sarah-chen") / "cache"
            )

    async def test_operator_env_wins_over_the_profile(self, tmp_path):
        manager = CLIWorkspaceManager(
            provider_key="default",
            profile=make_profile(),
            state_dir=tmp_path / "state",
            extra_env={"TESTCLI_QUIET": "0"},
        )
        async with manager.acquire("a") as ws:
            assert ws.env["TESTCLI_QUIET"] == "0"


class TestPathSafety:
    @pytest.mark.parametrize(
        "escape", ["../../evil", "/etc/passwd", ".testcli/../../evil"]
    )
    async def test_volatile_path_cannot_escape_the_home(self, tmp_path, escape):
        outside = tmp_path / "evil"
        outside.write_text("keep me")
        manager = make_manager(tmp_path, volatile_paths=[escape])
        async with manager.acquire("a"):
            pass
        assert outside.exists()

    async def test_credential_path_cannot_escape_the_home(self, tmp_path):
        outside = tmp_path / "state" / "evil.json"
        outside.parent.mkdir(parents=True, exist_ok=True)
        outside.write_text("keep me")
        manager = make_manager(tmp_path, credential_paths=["../../evil.json"])
        async with manager.acquire("a"):
            pass
        assert outside.read_text() == "keep me"
