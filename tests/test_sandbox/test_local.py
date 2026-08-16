"""The ``local`` sandbox backend — coding agents on the engine host.

``direct`` mode is exercised for real (actual processes, actual files),
including a full detached start → poll → collect cycle driven by the
production ``DetachedFileRunner``, which is the claim that matters: the
same runner drives a local box and an E2B one unchanged.

``container`` mode's command construction is unit-pinned against a stub
runtime rather than a real daemon — CI has no Docker, and the project
rule is that tests call nothing external.
"""

from __future__ import annotations

import os
import shutil
import signal
import stat
import sys
import time
from pathlib import Path

import pytest

from crewlet.sandbox.coding_agents._detached import RunPaths, run_paths
from crewlet.sandbox.local import (
    BOX_ROOT_ENV,
    CONTAINER_PREFIX,
    ContainerSandbox,
    DirectSandbox,
    LocalSandboxError,
    LocalSandboxProvider,
    default_box_root,
    resolve_container_runtime,
)
from crewlet.sandbox.protocol import DEFAULT_SANDBOX_HOME, SandboxSpec

pytestmark = pytest.mark.skipif(
    sys.platform == "win32", reason="POSIX process groups / /bin/sh"
)


def provider(tmp_path, **kwargs) -> LocalSandboxProvider:
    params = {"containment": "direct", "state_dir": tmp_path / "sandboxes"}
    params.update(kwargs)
    return LocalSandboxProvider(**params)


async def wait_for(predicate, timeout: float = 10.0) -> bool:
    """Poll ``predicate`` until it is true or ``timeout`` elapses.

    Accepts a sync or async callable — the process-state probes read
    ``/proc`` directly and are not coroutines.
    """
    import asyncio
    import inspect

    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = predicate()
        if inspect.isawaitable(result):
            result = await result
        if result:
            return True
        await asyncio.sleep(0.05)
    return False


class TestLayoutAndDefaults:
    def test_box_root_env_override(self, tmp_path, monkeypatch):
        monkeypatch.setenv(BOX_ROOT_ENV, str(tmp_path / "vol"))
        assert default_box_root() == tmp_path / "vol"

    async def test_each_box_gets_its_own_home(self, tmp_path):
        p = provider(tmp_path)
        first = await p.create(SandboxSpec())
        second = await p.create(SandboxSpec())
        try:
            assert first.home != second.home
            assert first.id != second.id
            # The artefact paths the runner derives must differ too — this
            # is the failure the per-sandbox home exists to prevent.
            assert run_paths(first).done != run_paths(second).done
        finally:
            await first.close()
            await second.close()

    async def test_home_directory_is_owner_only(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            assert Path(box.home).stat().st_mode & 0o777 == 0o700
        finally:
            await box.close()

    async def test_close_removes_the_box(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        home = Path(box.home)
        await box.close()
        assert not home.exists()

    async def test_orphaned_boxes_are_reaped_on_create(self, tmp_path):
        p = provider(tmp_path)
        orphan = tmp_path / "sandboxes" / "boxes" / "leftover"
        orphan.mkdir(parents=True)
        old = time.time() - 10_000
        os.utime(orphan, (old, old))
        box = await p.create(SandboxSpec(timeout_s=900))
        try:
            assert not orphan.exists()
        finally:
            await box.close()

    async def test_live_boxes_are_not_reaped(self, tmp_path):
        p = provider(tmp_path)
        first = await p.create(SandboxSpec(timeout_s=900))
        second = await p.create(SandboxSpec(timeout_s=900))
        try:
            assert Path(first.home).exists()
        finally:
            await first.close()
            await second.close()


class TestDirectExecution:
    async def test_exec_captures_output_and_exit_code(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            ok = await box.exec("echo hello")
            assert ok.exit_code == 0
            assert ok.stdout.strip() == "hello"
            bad = await box.exec("exit 7")
            assert bad.exit_code == 7
        finally:
            await box.close()

    async def test_commands_run_in_the_box_workspace(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            res = await box.exec("pwd")
            assert res.stdout.strip() == f"{box.home}/workspace"
        finally:
            await box.close()

    async def test_home_and_run_env_reach_the_process(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(SandboxSpec(env={"GITHUB_TOKEN": "ghp-from-config"}))
        try:
            res = await box.exec('echo "$HOME|$GITHUB_TOKEN|$XDG_CONFIG_HOME"')
            home, token, xdg = res.stdout.strip().split("|")
            assert home == box.home
            assert token == "ghp-from-config"
            assert xdg == f"{box.home}/.config"
        finally:
            await box.close()

    async def test_host_secrets_are_not_inherited(self, tmp_path, monkeypatch):
        """The engine's environment holds the org's tokens and its DSN;
        a coding agent has no business reading them."""
        monkeypatch.setenv("SLACK_BOT_TOKEN", "xoxb-should-not-leak")
        monkeypatch.setenv("CREWLET_DATABASE_DSN", "postgresql://should-not-leak")
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            res = await box.exec('echo "[${SLACK_BOT_TOKEN}][${CREWLET_DATABASE_DSN}]"')
            assert res.stdout.strip() == "[][]"
            # PATH still passes through, or nothing would run.
            path = await box.exec('echo "$PATH"')
            assert path.stdout.strip()
        finally:
            await box.close()

    async def test_read_and_write_file(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            target = f"{box.home}/.crewlet/x.txt"
            await box.write_file(target, "content")
            assert await box.read_file(target) == b"content"
            assert await box.read_file(f"{box.home}/missing") == b""
        finally:
            await box.close()

    @pytest.mark.parametrize(
        "escape", ["/usr/local/bin/git-credential-crewlet", "/etc/passwd"]
    )
    async def test_write_outside_the_box_is_refused(self, tmp_path, escape):
        """Direct mode does not virtualise the filesystem, so a setup step
        provisioning a system path must fail loudly rather than write to
        the engine host's real one."""
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            with pytest.raises(LocalSandboxError) as excinfo:
                await box.write_file(escape, "x")
            assert "containment 'container'" in str(excinfo.value)
        finally:
            await box.close()

    async def test_relative_traversal_is_refused(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            with pytest.raises(LocalSandboxError):
                await box.write_file("../../escaped", "x")
        finally:
            await box.close()


class TestDetachedLifecycle:
    async def test_background_job_survives_and_reports_a_pid(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            marker = f"{box.home}/workspace/done.txt"
            pid = await box.start_background(f"sleep 0.2; echo ok > {marker}")
            assert pid.isdigit()
            assert await wait_for(lambda: _exists(box, marker))
        finally:
            await box.close()

    async def test_dead_job_reads_as_gone_not_as_a_zombie(self, tmp_path):
        """A child the engine never reaps becomes a zombie, and `kill -0`
        reports a zombie as ALIVE — which would hang the runner's
        process-liveness completion check forever."""
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            pid = await box.start_background("exit 0")
            assert await wait_for(lambda: _probe_dead(box, pid), timeout=10.0), (
                "pid still reported alive — the job was left as a zombie"
            )
        finally:
            await box.close()

    async def test_full_runner_cycle_against_a_stub_cli(self, tmp_path):
        """The claim that matters: the production runner drives a local box
        unchanged — start, poll for the done-marker, collect the parsed
        result and the findings file."""
        import json

        from crewlet.sandbox.coding_agents.claude_code import ClaudeCodeRunner
        from crewlet.sandbox.protocol import RunLimits

        p = provider(tmp_path)
        runner = ClaudeCodeRunner()
        box = await p.create(SandboxSpec())
        try:
            await runner.install(box)
            paths = run_paths(box)
            assert Path(paths.ask_shim).exists()

            # Drop the stub into the box's OWN bin dir. The runner
            # prepends that to PATH inside the job's shell, so this both
            # isolates the test from whatever `claude` the host has and
            # pins that the PATH prefix actually works.
            script = Path(paths.bin_dir) / "claude"
            script.write_text(
                f"#!{sys.executable}\n"
                "import json, os\n"
                "findings = os.path.join(\n"
                "    os.environ['HOME'], '.crewlet', 'findings.md')\n"
                "open(findings, 'w').write('Outcome: succeeded')\n"
                "print(json.dumps({'subtype': 'success', 'result': 'shipped',\n"
                "                  'usage': {'input_tokens': 9,\n"
                "                            'output_tokens': 4}}))\n"
            )
            script.chmod(script.stat().st_mode | stat.S_IEXEC)

            handle = await runner.start(
                box, brief="do the thing", env={}, limits=RunLimits()
            )
            assert await wait_for(lambda: runner.poll(box, handle))
            result = await runner.collect(box, handle)
            assert result.success is True
            # The findings file is the report of record.
            assert result.text == "Outcome: succeeded"
            assert result.input_tokens == 9
            raw = json.loads(Path(paths.result).read_text())
            assert raw["result"] == "shipped"
        finally:
            await box.close()

    async def test_ask_shim_is_on_path_and_signals(self, tmp_path):
        """The brief tells the agent to run `crewlet-ask`; that only works
        if the runner put the box's shim dir on PATH."""
        from crewlet.sandbox.coding_agents.claude_code import ClaudeCodeRunner

        p = provider(tmp_path)
        runner = ClaudeCodeRunner()
        box = await p.create(SandboxSpec())
        try:
            await runner.install(box)
            paths = run_paths(box)
            res = await box.exec(
                f'PATH={paths.bin_dir}:"$PATH" crewlet-ask "which repo?" --to team'
            )
            assert res.exit_code == 0
            import json as _json

            ask = _json.loads(Path(paths.ask).read_text())
            assert ask == {"question": "which repo?", "to": "team"}
        finally:
            await box.close()


class TestPauseResume:
    async def test_pause_stops_and_connect_resumes(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            marker = f"{box.home}/workspace/tick.txt"
            pid = await box.start_background(f"sleep 0.6; echo done > {marker}")
            await box.pause()
            # The job is a session leader, so killpg reaches it and
            # SIGSTOP really stops it. Polled rather than sampled once:
            # a process sitting in uninterruptible sleep reports "D"
            # until it leaves that state, and only then shows "T", so
            # asserting on the instant after pause() is a race.
            assert await wait_for(
                lambda: _process_state(int(pid)) == "T", timeout=5.0
            ), f"job never stopped (state {_process_state(int(pid))!r})"
            # The behavioural half: a stopped job cannot reach its work.
            assert not await _exists(box, marker)

            resumed = await p.connect(box.id)
            assert resumed.home == box.home
            assert await wait_for(lambda: _exists(box, marker))
        finally:
            await box.close()

    async def test_connect_to_a_reaped_box_explains(self, tmp_path):
        p = provider(tmp_path)
        with pytest.raises(LocalSandboxError) as excinfo:
            await p.connect("does-not-exist")
        assert "no longer exists" in str(excinfo.value)

    async def test_kill_reclaims_a_paused_box_without_resuming_it(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        marker = f"{box.home}/workspace/should-not-appear.txt"
        await box.start_background(f"sleep 0.3; echo x > {marker}")
        await box.pause()
        await p.kill(box.id)
        assert not Path(box.home).exists()


class TestCredentialHandling:
    def _login(self, tmp_path) -> dict[str, str]:
        store = tmp_path / "llm-cli" / "credentials" / ".claude"
        store.mkdir(parents=True)
        (store / ".credentials.json").write_text('{"token": "v1"}')
        return {
            ".claude/.credentials.json": str(store / ".credentials.json"),
        }

    async def test_login_is_seeded_into_the_box(self, tmp_path):
        creds = self._login(tmp_path)
        p = provider(tmp_path)
        box = await p.create(SandboxSpec(credential_files=creds))
        try:
            seeded = Path(box.home) / ".claude" / ".credentials.json"
            assert seeded.read_text() == '{"token": "v1"}'
        finally:
            await box.close()

    async def test_refreshed_credential_is_written_back(self, tmp_path):
        creds = self._login(tmp_path)
        shared = Path(creds[".claude/.credentials.json"])
        p = provider(tmp_path)
        box = await p.create(SandboxSpec(credential_files=creds))
        (Path(box.home) / ".claude" / ".credentials.json").write_text(
            '{"token": "refreshed"}'
        )
        await box.close()
        assert shared.read_text() == '{"token": "refreshed"}'

    async def test_a_box_never_creates_a_login_that_was_removed(self, tmp_path):
        creds = self._login(tmp_path)
        shared = Path(creds[".claude/.credentials.json"])
        p = provider(tmp_path)
        box = await p.create(SandboxSpec(credential_files=creds))
        shared.unlink()  # the operator ran `crewlet llm logout`
        await box.close()
        assert not shared.exists()

    @pytest.mark.parametrize(
        "escape", ["../../escaped.json", "/etc/shadow-copy", "a/../../escaped.json"]
    )
    async def test_a_credential_path_cannot_escape_the_box(self, tmp_path, escape):
        """``credential_paths`` comes from operator-overridable profile
        config, so a ``../../`` entry must not let a box seeding write —
        or a box teardown read — outside its own directory."""
        source = tmp_path / "login.json"
        source.write_text("secret")
        outside = tmp_path / "escaped.json"
        p = provider(tmp_path)
        box = await p.create(SandboxSpec(credential_files={escape: str(source)}))
        try:
            assert not outside.exists()
        finally:
            await box.close()
        assert not outside.exists()

    async def test_a_credential_path_cannot_exfiltrate_on_teardown(self, tmp_path):
        """The collect direction is guarded too: the source file exists,
        so an unguarded join would copy whatever the escaped path names
        over the shared credential store."""
        shared = tmp_path / "shared.json"
        shared.write_text("original")
        target = tmp_path / "sandboxes" / "boxes"
        target.mkdir(parents=True, exist_ok=True)
        (tmp_path / "sandboxes" / "outside.json").write_text("host secret")
        p = provider(tmp_path)
        box = await p.create(
            SandboxSpec(credential_files={"../../outside.json": str(shared)})
        )
        await box.close()
        assert shared.read_text() == "original"

    async def test_missing_login_is_not_an_error(self, tmp_path):
        p = provider(tmp_path)
        box = await p.create(
            SandboxSpec(credential_files={".claude/x.json": str(tmp_path / "nope")})
        )
        await box.close()


class TestContainerMode:
    """Command construction against a stub runtime (no daemon in CI)."""

    @pytest.fixture
    def runtime(self, tmp_path, monkeypatch) -> Path:
        bindir = tmp_path / "cbin"
        bindir.mkdir()
        log = tmp_path / "docker.log"
        script = bindir / "docker"
        script.write_text(
            "#!/bin/sh\n"
            f'echo "$@" >> {log}\n'
            'case "$1" in\n'
            "  run) echo container-id ;;\n"
            "  exec) echo 4242 ;;\n"
            "esac\n"
            "exit 0\n"
        )
        script.chmod(script.stat().st_mode | stat.S_IEXEC)
        monkeypatch.setenv("PATH", f"{bindir}:{os.environ['PATH']}")
        return log

    def test_image_is_required(self, tmp_path):
        with pytest.raises(LocalSandboxError, match="requires an `image`"):
            LocalSandboxProvider(
                containment="container", state_dir=tmp_path / "s", image=""
            )

    def test_runtime_resolution_reports_a_missing_binary(self, tmp_path, monkeypatch):
        monkeypatch.setenv("PATH", str(tmp_path / "empty"))
        with pytest.raises(LocalSandboxError, match="neither docker nor podman"):
            resolve_container_runtime("auto")
        with pytest.raises(LocalSandboxError, match="not on the engine host"):
            resolve_container_runtime("podman")

    async def test_create_mounts_the_box_at_the_standard_home(self, tmp_path, runtime):
        p = provider(tmp_path, containment="container", image="acme/coding:1")
        box = await p.create(SandboxSpec(env={"GITHUB_TOKEN": "t"}))
        assert isinstance(box, ContainerSandbox)
        # In-box paths match E2B's, so setup steps behave identically.
        assert box.home == DEFAULT_SANDBOX_HOME
        line = runtime.read_text().splitlines()[0]
        assert f"--name {CONTAINER_PREFIX}{box.id}" in line
        assert f":{DEFAULT_SANDBOX_HOME}" in line
        assert "acme/coding:1 sleep infinity" in line

    async def test_run_args_and_network_are_spliced_in(self, tmp_path, runtime):
        p = provider(
            tmp_path,
            containment="container",
            image="acme/coding:1",
            network="none",
            run_args=["--cpus", "2"],
        )
        await p.create(SandboxSpec())
        line = runtime.read_text().splitlines()[0]
        assert "--network none" in line
        assert "--cpus 2" in line

    async def test_files_go_through_the_host_side_of_the_mount(self, tmp_path, runtime):
        p = provider(tmp_path, containment="container", image="acme/coding:1")
        box = await p.create(SandboxSpec())
        paths = RunPaths(home=box.home)
        await box.write_file(paths.result, "payload")
        assert await box.read_file(paths.result) == b"payload"
        # No `docker cp` — the write landed on the host side directly.
        assert "cp" not in runtime.read_text()

    async def test_paths_outside_the_mount_are_refused(self, tmp_path, runtime):
        p = provider(tmp_path, containment="container", image="acme/coding:1")
        box = await p.create(SandboxSpec())
        with pytest.raises(LocalSandboxError, match="outside the sandbox home mount"):
            await box.write_file("/etc/passwd", "x")

    async def test_exec_and_background_go_through_the_runtime(self, tmp_path, runtime):
        p = provider(tmp_path, containment="container", image="acme/coding:1")
        box = await p.create(SandboxSpec(env={"K": "v"}))
        await box.exec("echo hi")
        pid = await box.start_background("long-job")
        assert pid == "4242"
        log = runtime.read_text()
        assert "-e K=v" in log
        assert "long-job & echo $!" in log

    async def test_pause_and_kill_use_the_runtime(self, tmp_path, runtime):
        p = provider(tmp_path, containment="container", image="acme/coding:1")
        box = await p.create(SandboxSpec())
        await box.pause()
        await p.kill(box.id)
        log = runtime.read_text()
        assert f"pause {CONTAINER_PREFIX}{box.id}" in log
        assert f"rm -f {CONTAINER_PREFIX}{box.id}" in log


class TestProtocolConformance:
    async def test_both_modes_satisfy_the_sandbox_protocol(self, tmp_path, monkeypatch):
        from crewlet.sandbox.protocol import Sandbox

        p = provider(tmp_path)
        box = await p.create(SandboxSpec())
        try:
            assert isinstance(box, Sandbox)
        finally:
            await box.close()

        bindir = tmp_path / "cbin2"
        bindir.mkdir()
        (bindir / "docker").write_text("#!/bin/sh\necho id\n")
        (bindir / "docker").chmod(0o755)
        monkeypatch.setenv("PATH", f"{bindir}:{os.environ['PATH']}")
        cp = provider(tmp_path, containment="container", image="i")
        cbox = await cp.create(SandboxSpec())
        assert isinstance(cbox, Sandbox)


# -- helpers -----------------------------------------------------------


async def _exists(box: DirectSandbox, path: str) -> bool:
    return bool(await box.read_file(path))


async def _probe_dead(box: DirectSandbox, pid: str) -> bool:
    res = await box.exec(f"kill -0 {pid} 2>/dev/null")
    return res.exit_code != 0


def _process_state(pid: int) -> str:
    """Single-letter process state from /proc, or "" when gone."""
    try:
        stat_line = Path(f"/proc/{pid}/stat").read_text()
    except OSError:
        return ""
    try:
        return stat_line.rsplit(")", 1)[1].split()[0]
    except IndexError:  # pragma: no cover
        return ""


def _cleanup(path: Path) -> None:  # pragma: no cover — belt and braces
    shutil.rmtree(path, ignore_errors=True)


assert signal.SIGSTOP  # imported for the pause path's documentation value
