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
    """Poll an async ``predicate`` until true or ``timeout`` elapses."""
    import asyncio

    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if await predicate():
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

    async def test_a_busy_box_older_than_the_ttl_survives(self, tmp_path):
        """The reaper must not read "old" off the directory clock.

        A directory's mtime moves only when an entry is added or removed
        DIRECTLY in it, and every entry in a box root is made at create
        — so the root mtime is the box's birth time and stays frozen
        however busy the coding agent inside `workspace/` is. Reaping on
        it deleted the checkout, the seeded login and `.crewlet/box.pid`
        of every run that outlasted the TTL, while the process tree kept
        running: with the pid file gone nothing could ever kill it.
        """
        p = provider(tmp_path)
        box = await p.create(SandboxSpec(timeout_s=900))
        home = Path(box.home)
        # The agent has been working for hours — inside workspace/,
        # which is what a coding job does and what leaves the root
        # mtime untouched.
        (home / "workspace" / "src").mkdir(parents=True, exist_ok=True)
        (home / "workspace" / "src" / "main.py").write_text("print('work')")
        old = time.time() - 10_000
        os.utime(home, (old, old))
        # The waiter has been keeping it alive on every poll.
        await box.set_timeout(900)

        second = await p.create(SandboxSpec(timeout_s=900))
        try:
            assert home.exists(), "the reaper deleted a box that is in use"
            assert (home / "workspace" / "src" / "main.py").exists()
        finally:
            await box.close()
            await second.close()

    async def test_a_paused_box_survives_on_its_live_process(self, tmp_path):
        """A SIGSTOPped box stops being heartbeated but is still alive.

        That is the clarification pause: the run parks holding its exact
        state while a person answers. Bounding THAT wait belongs to the
        pause TTL, which already owns it — the orphan reaper must not
        also have an opinion, or it deletes the checkout the answer was
        going to resume into.
        """
        import subprocess

        p = provider(tmp_path)
        box = await p.create(SandboxSpec(timeout_s=900))
        home = Path(box.home)
        proc = subprocess.Popen(["sleep", "60"], start_new_session=True)
        try:
            (home / ".crewlet").mkdir(parents=True, exist_ok=True)
            (home / ".crewlet" / "box.pid").write_text(str(proc.pid))
            # No heartbeat since it parked, and the directory clock is
            # long past the TTL.
            old = time.time() - 10_000
            os.utime(home / ".crewlet" / "alive", (old, old))
            os.utime(home, (old, old))

            second = await p.create(SandboxSpec(timeout_s=900))
            try:
                assert home.exists(), "the reaper deleted a paused run's box"
            finally:
                await second.close()
        finally:
            proc.kill()
            proc.wait()

    async def test_a_dead_box_past_the_ttl_is_still_reaped(self, tmp_path):
        """The reaper still does its job — nothing running, nothing
        touching it for a whole TTL."""
        p = provider(tmp_path)
        orphan = tmp_path / "sandboxes" / "boxes" / "leftover"
        (orphan / ".crewlet").mkdir(parents=True)
        # A pid from a process that is long gone.
        (orphan / ".crewlet" / "box.pid").write_text("999999")
        old = time.time() - 10_000
        os.utime(orphan / ".crewlet" / "alive", (old, old)) if (
            orphan / ".crewlet" / "alive"
        ).exists() else None
        os.utime(orphan, (old, old))

        box = await p.create(SandboxSpec(timeout_s=900))
        try:
            assert not orphan.exists()
        finally:
            await box.close()

    async def test_a_reconnected_box_writes_its_refreshed_login_back(self, tmp_path):
        """`connect` is handed an id and nothing else.

        EVERY production teardown goes through a connected box (collect)
        or through `kill` — the object `create` returned is only closed
        when the acquire itself failed, i.e. when no run happened and no
        credential was refreshed. So a box that could not recover its
        own credential map discarded the rotated OAuth token on every
        single run, and the fleet logged out at the next expiry.
        """
        p = provider(tmp_path)
        shared = tmp_path / "store" / "auth.json"
        shared.parent.mkdir(parents=True)
        shared.write_text('{"token": "original"}')

        box = await p.create(
            SandboxSpec(credential_files={".cli/auth.json": str(shared)})
        )
        # The coding agent refreshes its login mid-run.
        (Path(box.home) / ".cli" / "auth.json").write_text('{"token": "refreshed"}')

        # The completion path: reconnect by id, then close.
        reconnected = await p.connect(box.id)
        await reconnected.close()

        assert shared.read_text() == '{"token": "refreshed"}'

    async def test_a_killed_box_writes_its_refreshed_login_back(self, tmp_path):
        """`kill` is the pause reaper's primitive and is a teardown too.

        It must not resume the box — but the files are already on disk,
        so collecting them needs no resume.
        """
        p = provider(tmp_path)
        shared = tmp_path / "store" / "auth.json"
        shared.parent.mkdir(parents=True)
        shared.write_text('{"token": "original"}')

        box = await p.create(
            SandboxSpec(credential_files={".cli/auth.json": str(shared)})
        )
        (Path(box.home) / ".cli" / "auth.json").write_text('{"token": "refreshed"}')

        await p.kill(box.id)

        assert shared.read_text() == '{"token": "refreshed"}'

    async def test_a_reaped_box_gives_its_refreshed_login_back(self, tmp_path):
        """A box the engine abandoned may still hold a rotated token.

        Deleting it without collecting is the same fleet-wide logout as
        never collecting at all — it just happens on the crash path.
        """
        p = provider(tmp_path)
        shared = tmp_path / "store" / "auth.json"
        shared.parent.mkdir(parents=True)
        shared.write_text('{"token": "original"}')

        box = await p.create(
            SandboxSpec(timeout_s=900, credential_files={".cli/auth.json": str(shared)})
        )
        home = Path(box.home)
        (home / ".cli" / "auth.json").write_text('{"token": "refreshed"}')
        old = time.time() - 10_000
        os.utime(home / ".crewlet" / "alive", (old, old))
        os.utime(home, (old, old))

        second = await p.create(SandboxSpec(timeout_s=900))
        try:
            assert not home.exists()
            assert shared.read_text() == '{"token": "refreshed"}'
        finally:
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
            work = f"{box.home}/workspace"
            started, release, done = (
                f"{work}/started.txt",
                f"{work}/release.txt",
                f"{work}/done.txt",
            )
            # The job announces itself, then spins until `release` shows
            # up. Handshaking through files rather than asserting on
            # ``/proc`` scheduler state tests OUR guarantee — a paused box
            # makes no progress — instead of the kernel's bookkeeping: a
            # process with a pending SIGSTOP still reports "D" while it
            # sits in uninterruptible sleep, so the state letter is not a
            # sound proxy for "stopped". `release` is written only AFTER
            # pause(), so the job can reach `done` only if the SIGSTOP
            # never took.
            await box.start_background(
                f"echo x > {started}; "
                f"until [ -f {release} ]; do sleep 0.05; done; "
                f"echo x > {done}"
            )
            assert await wait_for(lambda: _exists(box, started)), "job never started"

            await box.pause()
            await box.write_file(release, "go")
            # A live job picks the release up within one 0.05 s poll of
            # its own loop; 1.5 s is thirty of them, so a pause that did
            # not land fails here instead of hiding behind a slow runner.
            assert not await wait_for(lambda: _exists(box, done), timeout=1.5), (
                "paused job kept working"
            )

            resumed = await p.connect(box.id)
            assert resumed.home == box.home
            assert await wait_for(lambda: _exists(box, done)), (
                "resumed job never finished"
            )
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

    async def test_a_traversal_inside_the_mount_prefix_is_refused(
        self, tmp_path, runtime
    ):
        """A prefix test alone does not keep a write inside the box.

        This is the HOST side of a bind mount, so an escape lands on the
        engine host — and setup-step file paths are operator config, the
        same surface `safe_join` already guards everywhere else.
        """
        p = provider(tmp_path, containment="container", image="acme/coding:1")
        box = await p.create(SandboxSpec())
        outside = tmp_path / "escaped.txt"
        depth = len(Path(box.home).parts)
        traversal = "/home/user/" + "../" * (depth + 4) + str(outside).lstrip("/")

        with pytest.raises(LocalSandboxError, match="outside the sandbox home mount"):
            await box.write_file(traversal, "owned")
        assert not outside.exists()

    async def test_exec_and_background_go_through_the_runtime(self, tmp_path, runtime):
        p = provider(tmp_path, containment="container", image="acme/coding:1")
        box = await p.create(SandboxSpec(env={"K": "v"}))
        await box.exec("echo hi")
        pid = await box.start_background("long-job")
        assert pid == "4242"
        log = runtime.read_text()
        assert "--env-file" in log
        assert "long-job & echo $!" in log

    async def test_the_run_env_never_reaches_the_command_line(self, tmp_path, runtime):
        """A process's argv is world-readable on the engine host.

        `/proc/<pid>/cmdline` and every `ps` show it, and this env
        carries the seat's LLM key plus whatever code-host token
        `role.sandbox.env` declares — so passing them as `-e KEY=value`
        published every seat's credentials to any local user.
        """
        p = provider(tmp_path, containment="container", image="acme/coding:1")
        box = await p.create(SandboxSpec(env={"ANTHROPIC_API_KEY": "sk-secret-value"}))
        await box.exec("echo hi")

        log = runtime.read_text()
        assert "sk-secret-value" not in log, "a secret reached the runtime argv"
        env_file = tmp_path / "sandboxes" / "boxes" / box.id / ".crewlet" / "env"
        assert env_file.read_text() == "ANTHROPIC_API_KEY=sk-secret-value\n"
        assert env_file.stat().st_mode & 0o777 == 0o600

    async def test_an_env_value_with_a_newline_is_dropped_not_forged(
        self, tmp_path, runtime
    ):
        """`--env-file` is line-oriented with no quoting.

        A newline inside a value would declare a second variable the
        config never asked for — so the unrepresentable value is
        dropped and logged rather than silently reinterpreted.
        """
        p = provider(tmp_path, containment="container", image="acme/coding:1")
        box = await p.create(SandboxSpec(env={"BAD": "a\nINJECTED=1", "GOOD": "ok"}))
        await box.exec("echo hi")

        env_file = tmp_path / "sandboxes" / "boxes" / box.id / ".crewlet" / "env"
        assert env_file.read_text() == "GOOD=ok\n"

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


def _cleanup(path: Path) -> None:  # pragma: no cover — belt and braces
    shutil.rmtree(path, ignore_errors=True)
