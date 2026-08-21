"""``LocalSandboxProvider`` — coding agents on the engine host.

The third sandbox backend, alongside E2B and the in-process fake. It
exists so a role can do real code work with the **coding CLI you already
logged in to** (see
``docs/concepts/subscription-llm-backends.md``) — no E2B account, no API
key, no token minting: the run reads the same credential directory
``crewlet llm login`` wrote.

Two containment modes, both satisfying the same :class:`Sandbox`
protocol, so the existing ``ClaudeCodeRunner`` / ``OpenCodeRunner`` drive
either one unchanged:

``direct``
    A process tree on the engine host, rooted in a per-box directory
    with ``HOME`` and the XDG variables pointed at it and an allowlisted
    environment. **This isolates state, not the host.** The coding agent
    runs as the engine user with its own tools enabled, so it can read
    anything that user can read and reach anything its credentials
    reach. It is the right mode for a workstation or a dedicated VM, and
    the wrong one for a shared host. Because there is no filesystem
    virtualisation, :meth:`DirectSandbox.write_file` refuses paths
    outside the box — a setup step that provisions ``/usr/local/bin``
    fails loudly here instead of writing to the host's real one.

``container``
    A long-lived container (Docker or Podman) per box, with the box
    directory bind-mounted at ``/home/user`` — the same home E2B uses, so
    setup steps that provision system paths work exactly as they do
    remotely. Real host isolation, at the cost of an image that has the
    coding CLI installed (there is no default: ``image`` is required, and
    validated at config time).

Both modes are engine-host-local, so a box survives an engine restart
only as far as its processes do: ``connect`` reattaches by id because the
box directory is derived from it, and the detached job is started in its
own session so it is reparented to init rather than dying with the
engine.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import os
import shutil
import signal
import time
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

from crewlet._logging import get_logger
from crewlet.providers.llm.cli_workspace import (
    BASE_PASSTHROUGH_ENV,
    copy_file_atomic,
    file_digest,
    safe_join,
)
from crewlet.sandbox.protocol import (
    DEFAULT_SANDBOX_HOME,
    ExecResult,
    Sandbox,
    SandboxSpec,
)

logger = get_logger("sandbox.local")

Containment = Literal["direct", "container"]
ContainerRuntime = Literal["auto", "docker", "podman"]

#: Where a box's checkout lives, relative to its home. The coding agent's
#: working directory; kept under the home so container mode needs exactly
#: one bind mount.
WORKSPACE_SUBDIR = "workspace"

#: Prefix for container names, so a stray box is identifiable (and
#: greppable) in ``docker ps`` without consulting Crewlet.
CONTAINER_PREFIX = "crewlet-sbx-"

#: Seconds a host/container command gets before it is abandoned. This
#: covers the *control-plane* calls only — `mkdir`, `docker exec`, the
#: `kill -0` liveness probe — never the coding job itself, which is
#: detached and deliberately uncapped. Sized for a container runtime
#: under load, not for work.
_CONTROL_TIMEOUT = 120.0

#: Grace between SIGTERM and SIGKILL when tearing a direct box down.
_TERM_GRACE_SECONDS = 5.0

#: Slack allowed when comparing a process's start time against the box's
#: creation time. A box's own job cannot predate the box, so a pid whose
#: process started EARLIER is a recycled one pointing at some unrelated
#: long-lived process — which, without this, would keep a dead box's
#: directory alive forever. The grace absorbs only clock and filesystem
#: timestamp granularity between the two readings (``st_mtime`` on the
#: directory versus on ``/proc/<pid>``), which is sub-second; a second is
#: ample and far below any interval over which a pid could wrap.
_PID_REUSE_GRACE_SECONDS = 1.0


class LocalSandboxError(RuntimeError):
    """A local sandbox could not be created, reached, or torn down."""


async def _run_host(
    argv: list[str],
    *,
    stdin: bytes | None = None,
    timeout: float = _CONTROL_TIMEOUT,
    cwd: str | None = None,
    env: dict[str, str] | None = None,
) -> ExecResult:
    """Run one command on the engine host and capture it."""
    proc = await asyncio.create_subprocess_exec(
        *argv,
        stdin=(
            asyncio.subprocess.PIPE if stdin is not None else asyncio.subprocess.DEVNULL
        ),
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        cwd=cwd,
        env=env,
        start_new_session=True,
    )
    try:
        out, err = await asyncio.wait_for(
            proc.communicate(input=stdin), timeout=timeout
        )
    except TimeoutError:
        with contextlib.suppress(ProcessLookupError, PermissionError, OSError):
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        with contextlib.suppress(Exception):
            await proc.wait()
        return ExecResult(exit_code=124, stderr=f"timed out after {timeout:g}s")
    return ExecResult(
        exit_code=proc.returncode or 0,
        stdout=out.decode("utf-8", errors="replace"),
        stderr=err.decode("utf-8", errors="replace"),
    )


def _pid_from(output: str) -> str:
    """Last numeric line of ``output`` — the backgrounded job's pid."""
    for line in reversed(output.strip().splitlines()):
        candidate = line.strip()
        if candidate.isdigit():
            return candidate
    return ""


def _detach_script(cmd: str) -> str:
    """Background ``cmd`` inside a container and print its pid.

    Container mode only. ``docker exec`` would otherwise block for the
    whole length of a coding job; backgrounding and echoing ``$!`` lets
    the exec return at once while the job keeps running under the
    container's PID 1. That PID 1 is started with ``--init`` precisely so
    it reaps the job when it finishes — an unreaped process becomes a
    zombie, and ``kill -0`` reports a zombie as *alive*, which would hang
    the runner's process-liveness completion check on a job that had
    already died.

    Direct mode does not use this: it spawns the job itself with its own
    session (see :meth:`DirectSandbox.start_background`), which gets both
    properties without a shell trick.
    """
    return f"{cmd} & echo $!"


# ---------------------------------------------------------------------
# Shared box layout
# ---------------------------------------------------------------------


@dataclass(frozen=True)
class BoxLayout:
    """Where one box's files live on the engine host."""

    box_id: str
    root: Path

    @property
    def workspace(self) -> Path:
        return self.root / WORKSPACE_SUBDIR

    @property
    def pid_file(self) -> Path:
        """Records the detached job's pid so teardown can reach its group
        even in a fresh engine that never held the handle."""
        return self.root / ".crewlet" / "box.pid"

    @property
    def alive_file(self) -> Path:
        """Keepalive stamp — the local counterpart of E2B's box TTL.

        A directory's mtime does NOT move when files are written inside
        its subdirectories, so the box root's own timestamp is frozen at
        creation for the box's entire life.  Anything reading it as
        "recently used" is reading a constant.  This file is touched on
        every ``set_timeout``, which the waiter calls once per poll for
        exactly the box it is keeping alive.
        """
        return self.root / ".crewlet" / "alive"

    @property
    def credentials_file(self) -> Path:
        """The credential map this box was seeded with.

        Recorded so the box is SELF-DESCRIBING: ``connect`` is handed
        nothing but an id — possibly in a different process from the one
        that created the box — and without this it cannot know which
        files to sync back, so a refreshed login is silently dropped on
        every reconnect-then-close.  Same reason ``pid_file`` exists.
        """
        return self.root / ".crewlet" / "credentials.json"


def _seed_credentials(layout: BoxLayout, credential_files: dict[str, str]) -> None:
    """Copy the CLI login into the box before the coding agent runs.

    The keys are relative paths from an operator-overridable profile
    (``cli.overrides.credential_paths``), so they are joined through
    :func:`safe_join`: an entry like ``../../etc/cron.d/x`` would
    otherwise write a host file every time a box was created.
    """
    for relative, source in credential_files.items():
        src = Path(source)
        dst = safe_join(layout.root, relative)
        if dst is None or not src.is_file():
            continue
        dst.parent.mkdir(parents=True, exist_ok=True)
        copy_file_atomic(src, dst)


def _collect_credentials(layout: BoxLayout, credential_files: dict[str, str]) -> None:
    """Write a credential the run refreshed back to the shared login.

    Same reason the LLM backend does it: OAuth access tokens expire in
    hours and most vendors rotate the refresh token alongside them, so a
    box that quietly discards the rewritten file logs the whole fleet out
    at the next expiry. Only writes back over a file that already exists
    in the shared store, so a torn-down box can never *create* a login
    the operator has since removed.

    Read through :func:`safe_join` for the same reason the seed is
    written through it: a ``../../`` key would otherwise copy an
    arbitrary host file out of the box's directory and over the
    credential store.
    """
    for relative, source in credential_files.items():
        src = safe_join(layout.root, relative)
        dst = Path(source)
        if src is None or not src.is_file() or not dst.is_file():
            continue
        if file_digest(src) == file_digest(dst):
            continue
        if copy_file_atomic(src, dst):
            logger.info("local_sandbox_credential_refreshed", file=relative)


def _record_credential_map(layout: BoxLayout, credential_files: dict[str, str]) -> None:
    """Persist the box's credential map so ``connect`` can sync it back."""
    if not credential_files:
        return
    target = layout.credentials_file
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(json.dumps(credential_files), encoding="utf-8")
        target.chmod(0o600)
    except OSError:
        # The map names paths, not secrets, but losing it means a
        # refreshed login is not written back — worth saying out loud.
        logger.warning(
            "local_sandbox_credential_map_unwritable", sandbox_id=layout.box_id
        )


def _read_credential_map(layout: BoxLayout) -> dict[str, str]:
    """The credential map recorded at ``create``; empty when absent."""
    try:
        loaded = json.loads(layout.credentials_file.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    if not isinstance(loaded, dict):
        return {}
    return {str(k): str(v) for k, v in loaded.items()}


def _touch_alive(layout: BoxLayout) -> None:
    """Refresh the box's keepalive stamp.  Never raises."""
    try:
        layout.alive_file.parent.mkdir(parents=True, exist_ok=True)
        layout.alive_file.touch()
        os.utime(layout.alive_file, None)
    except OSError:  # pragma: no cover — the box is being torn down
        logger.debug("local_sandbox_keepalive_unwritable", sandbox_id=layout.box_id)


def _process_group_alive(pid: int, *, box_created: float) -> bool:
    """Whether ``pid`` is a live process group that could be this box's.

    A pid on its own is not proof: pids are reused, and a recycled one
    pointing at some unrelated daemon would keep a dead box's directory
    alive forever.  So the probe also requires the process to have
    started at or after the box was created — a box's own job cannot
    predate its box, and a recycled pid almost always belongs to
    something older.

    ``box_created`` is the box root's own mtime, which is the one thing
    that timestamp genuinely means: entries in the root are all made at
    ``create`` and never again, so it is the box's birth time, not its
    last use.  ``/proc/<pid>``'s mtime is the process start time on
    Linux; where that is unavailable the pid probe stands alone, which
    fails towards keeping a directory rather than deleting live work.
    """
    if pid <= 0:
        return False
    try:
        os.killpg(pid, 0)
    except (ProcessLookupError, OSError):
        return False
    try:
        started = os.stat(f"/proc/{pid}").st_mtime
    except OSError:
        return True
    return started >= box_created - _PID_REUSE_GRACE_SECONDS


# ---------------------------------------------------------------------
# direct
# ---------------------------------------------------------------------


class DirectSandbox:
    """A :class:`Sandbox` that is a process tree on the engine host."""

    def __init__(
        self,
        layout: BoxLayout,
        *,
        env: dict[str, str] | None = None,
        credential_files: dict[str, str] | None = None,
    ) -> None:
        self._layout = layout
        self._spec_env = dict(env or {})
        self._credential_files = dict(credential_files or {})
        self._paused = False
        # Held so asyncio's child watcher keeps reaping the detached job
        # (see start_background) and the transport is not GC'd mid-run.
        self._job: asyncio.subprocess.Process | None = None

    @property
    def id(self) -> str:
        return self._layout.box_id

    @property
    def home(self) -> str:
        return str(self._layout.root)

    # -- env ---------------------------------------------------------

    def _env(self, extra: dict[str, str] | None = None) -> dict[str, str]:
        """Allowlisted host env + the box's home + the run env.

        Allowlisted for the same reason the CLI LLM backend allowlists:
        the engine's environment holds the org's Slack token, its
        database DSN and possibly a metered API key, none of which the
        coding agent has any business reading. The run env
        (``spec.env``) is what config deliberately put there.
        """
        env = {k: v for k in BASE_PASSTHROUGH_ENV if (v := os.environ.get(k))}
        home = str(self._layout.root)
        env["HOME"] = home
        env["XDG_CONFIG_HOME"] = f"{home}/.config"
        env["XDG_DATA_HOME"] = f"{home}/.local/share"
        env["XDG_STATE_HOME"] = f"{home}/.local/state"
        env["XDG_CACHE_HOME"] = f"{home}/.cache"
        env["TMPDIR"] = f"{home}/.tmp"
        env.update(self._spec_env)
        env.update(extra or {})
        return env

    def _cwd(self, cwd: str) -> str:
        target = Path(cwd) if cwd else self._layout.workspace
        target.mkdir(parents=True, exist_ok=True)
        return str(target)

    # -- Sandbox protocol --------------------------------------------

    async def exec(
        self,
        cmd: str,
        *,
        env: dict[str, str] | None = None,
        cwd: str = "",
        timeout_s: float = 0.0,
    ) -> ExecResult:
        return await _run_host(
            ["/bin/sh", "-c", cmd],
            timeout=timeout_s or _CONTROL_TIMEOUT,
            cwd=self._cwd(cwd),
            env=self._env(env),
        )

    async def start_background(
        self,
        cmd: str,
        *,
        env: dict[str, str] | None = None,
        cwd: str = "",
    ) -> str:
        """Spawn the detached coding job in its own session.

        Spawned directly rather than backgrounded inside a throwaway
        shell, because the pid has to be usable for three different
        things and only a direct spawn gives all three:

        * ``start_new_session`` makes the job a session and process-group
          leader, so :meth:`pause` and teardown can signal the *whole*
          tree with one ``killpg``. A job backgrounded inside a shell
          inherits that shell's group and is reachable only individually
          — leaving the coding agent's own children running.
        * asyncio's child watcher reaps it when it exits, so the
          runner's ``kill -0`` liveness probe reports it gone. An
          unreaped process lingers as a zombie, which ``kill -0`` reports
          as *alive* — the job would look like it was still working
          forever.
        * If the engine dies the job is reparented to init and keeps
          going, which is what makes the detached lifecycle survive a
          restart at all.

        Its stdio is ``/dev/null``: the runner's script already redirects
        the agent's output into the box's result and error files.
        """
        proc = await asyncio.create_subprocess_exec(
            "/bin/sh",
            "-c",
            cmd,
            stdin=asyncio.subprocess.DEVNULL,
            stdout=asyncio.subprocess.DEVNULL,
            stderr=asyncio.subprocess.DEVNULL,
            cwd=self._cwd(cwd),
            env=self._env(env),
            start_new_session=True,
        )
        self._job = proc
        pid = str(proc.pid)
        self._layout.pid_file.parent.mkdir(parents=True, exist_ok=True)
        self._layout.pid_file.write_text(pid)
        logger.info("local_sandbox_job_started", sandbox_id=self.id, pid=pid)
        return pid

    def _resolve(self, path: str) -> Path:
        """Map an in-box absolute path onto the host, refusing escapes.

        Direct mode does **not** virtualise the filesystem: there is no
        chroot and no mount namespace, so an in-box path IS a host path.
        Writing outside the box would therefore hit the engine host's
        real ``/usr/local/bin`` (or worse), which is why a setup step
        that provisions a system path is rejected here rather than
        silently doing it. Container mode is the answer for those.
        """
        root = self._layout.root.resolve()
        candidate = Path(path)
        if not candidate.is_absolute():
            candidate = self._layout.root / path
        resolved = candidate.resolve()
        if resolved != root and root not in resolved.parents:
            raise LocalSandboxError(
                f"local sandbox (containment 'direct') refuses to touch "
                f"{path!r}: it is outside the box at {root}. Direct mode has "
                "no filesystem virtualisation, so this would write to the "
                "engine host itself. Put the file under the box's home, or "
                "use providers.sandbox.local.containment 'container'."
            )
        return resolved

    async def write_file(self, path: str, content: bytes | str) -> None:
        target = self._resolve(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        data = content.encode("utf-8") if isinstance(content, str) else content
        target.write_bytes(data)

    async def read_file(self, path: str) -> bytes:
        # Empty-on-missing: the detached runner polls for marker / result
        # files that do not exist until the job finishes.
        try:
            return self._resolve(path).read_bytes()
        except (OSError, LocalSandboxError):
            return b""

    async def set_timeout(self, seconds: float) -> None:
        """Refresh the box's keepalive stamp.

        This DOES have a counterpart here, and missing it was the bug:
        the orphan reaper reclaims local boxes on a clock, so a running
        box that never says it is alive is one the next ``create`` on
        this host deletes. The waiter calls this once per poll for
        exactly the boxes it is keeping alive, which is the same
        contract E2B's TTL refresh has. ``seconds`` is unused: a local
        box has no provider-side deadline to extend, only a last-seen
        time to move forward.
        """
        _touch_alive(self._layout)

    async def pause(self) -> None:
        """SIGSTOP the job's process group.

        The local analogue of an E2B snapshot: the run holds its exact
        state (open files, the checkout, the agent's memory) and resumes
        on :meth:`LocalSandboxProvider.connect`. It also holds RAM, which
        is why the waiter's pause reaper bounds it exactly as it bounds a
        billed snapshot.
        """
        pid = self._read_pid()
        if not pid:
            return
        with contextlib.suppress(ProcessLookupError, PermissionError, OSError):
            os.killpg(pid, signal.SIGSTOP)
            self._paused = True
            logger.debug("local_sandbox_paused", sandbox_id=self.id, pid=pid)

    def resume(self) -> None:
        """SIGCONT a paused box (the ``connect`` auto-resume)."""
        pid = self._read_pid()
        if not pid:
            return
        with contextlib.suppress(ProcessLookupError, PermissionError, OSError):
            os.killpg(pid, signal.SIGCONT)
            self._paused = False

    def _read_pid(self) -> int:
        try:
            return int(self._layout.pid_file.read_text().strip())
        except (OSError, ValueError):
            return 0

    async def close(self) -> None:
        """Kill the job's process group, sync credentials, remove the box."""
        pid = self._read_pid()
        if pid:
            # SIGCONT first: a stopped process never sees SIGTERM, so
            # tearing down a paused box without it would leave the tree
            # alive and the directory undeletable-but-in-use.
            with contextlib.suppress(ProcessLookupError, PermissionError, OSError):
                os.killpg(pid, signal.SIGCONT)
                os.killpg(pid, signal.SIGTERM)
            deadline = time.monotonic() + _TERM_GRACE_SECONDS
            while time.monotonic() < deadline:
                try:
                    os.killpg(pid, 0)
                except OSError:
                    break
                await asyncio.sleep(0.1)
            with contextlib.suppress(ProcessLookupError, PermissionError, OSError):
                os.killpg(pid, signal.SIGKILL)
        _collect_credentials(self._layout, self._credential_files)
        shutil.rmtree(self._layout.root, ignore_errors=True)
        logger.debug("local_sandbox_closed", sandbox_id=self.id)


# ---------------------------------------------------------------------
# container
# ---------------------------------------------------------------------


class ContainerSandbox:
    """A :class:`Sandbox` backed by a long-lived container.

    The box directory is bind-mounted at :data:`DEFAULT_SANDBOX_HOME`, so
    in-box paths are identical to E2B's and file reads/writes are done on
    the host side of the mount (no ``docker cp`` round trip).
    """

    def __init__(
        self,
        layout: BoxLayout,
        *,
        runtime: str,
        container: str,
        env: dict[str, str] | None = None,
        credential_files: dict[str, str] | None = None,
    ) -> None:
        self._layout = layout
        self._runtime = runtime
        self._container = container
        self._spec_env = dict(env or {})
        self._credential_files = dict(credential_files or {})

    @property
    def id(self) -> str:
        return self._layout.box_id

    @property
    def home(self) -> str:
        return DEFAULT_SANDBOX_HOME

    def _host_path(self, path: str) -> Path:
        """Map an in-container path onto its host side of the mount.

        The result is a HOST path — this is the host side of a bind
        mount, so an escape here writes to the engine host, not to the
        container. A prefix test alone does not prevent that:
        ``/home/user/../../etc/cron.d/x`` starts with the mount point
        and still resolves outside it, and setup-step file paths are
        operator config. The final check is the same one direct mode
        makes, for the same reason.
        """
        relative = path
        prefix = DEFAULT_SANDBOX_HOME.rstrip("/") + "/"
        if path == DEFAULT_SANDBOX_HOME:
            return self._layout.root
        if path.startswith(prefix):
            relative = path[len(prefix) :]
        elif path.startswith("/"):
            # Outside the mount — reachable only from inside the
            # container, which is exactly what container mode is for.
            raise LocalSandboxError(
                f"{path!r} is outside the sandbox home mount; write it with a "
                "setup-step command instead of a file entry"
            )
        root = self._layout.root.resolve()
        resolved = (root / relative).resolve()
        if resolved != root and root not in resolved.parents:
            raise LocalSandboxError(
                f"{path!r} resolves outside the sandbox home mount at "
                f"{root} — it would be written to the engine host itself."
            )
        return resolved

    def _env_args(self, extra: dict[str, str] | None = None) -> list[str]:
        """Render the run env as ``--env-file`` arguments.

        NOT ``-e KEY=value``: a process's argv is world-readable on a
        normal Linux box (``/proc/<pid>/cmdline``, and every ``ps`` on
        the host), and this env carries the seat's LLM key and whatever
        code-host token ``role.sandbox.env`` declares. A file the
        runtime reads keeps them off the command line; it lives inside
        the box, which is already 0700, and is written 0600.

        Rewritten per call rather than kept: ``extra`` differs between
        the setup steps and the coding job, and a stale file would hand
        one phase another's environment.
        """
        merged = {**self._spec_env, **(extra or {})}
        if not merged:
            return []
        env_file = self._layout.root / ".crewlet" / "env"
        try:
            env_file.parent.mkdir(parents=True, exist_ok=True)
            # `--env-file` is line-oriented KEY=value with no quoting,
            # so a newline in a value would forge an extra variable.
            # Values that cannot be represented are dropped loudly
            # rather than silently truncated into a different env.
            lines = []
            for key, value in merged.items():
                if "\n" in value or "\r" in value:
                    logger.warning(
                        "local_sandbox_env_var_unrepresentable",
                        sandbox_id=self.id,
                        var=key,
                    )
                    continue
                lines.append(f"{key}={value}")
            env_file.write_text("\n".join(lines) + "\n", encoding="utf-8")
            env_file.chmod(0o600)
        except OSError as exc:
            raise LocalSandboxError(
                f"container sandbox {self.id} could not write its env file: {exc}"
            ) from exc
        return ["--env-file", str(env_file)]

    async def exec(
        self,
        cmd: str,
        *,
        env: dict[str, str] | None = None,
        cwd: str = "",
        timeout_s: float = 0.0,
    ) -> ExecResult:
        argv = [self._runtime, "exec", "-w", cwd or self._workdir()]
        argv += self._env_args(env)
        argv += [self._container, "/bin/sh", "-c", cmd]
        return await _run_host(argv, timeout=timeout_s or _CONTROL_TIMEOUT)

    def _workdir(self) -> str:
        return f"{DEFAULT_SANDBOX_HOME}/{WORKSPACE_SUBDIR}"

    async def start_background(
        self,
        cmd: str,
        *,
        env: dict[str, str] | None = None,
        cwd: str = "",
    ) -> str:
        argv = [self._runtime, "exec", "-w", cwd or self._workdir()]
        argv += self._env_args(env)
        argv += [self._container, "/bin/sh", "-c", _detach_script(cmd)]
        result = await _run_host(argv)
        pid = _pid_from(result.stdout)
        if not pid:
            raise LocalSandboxError(
                f"container sandbox {self.id} could not start a background "
                f"job: {(result.stderr or result.stdout).strip()[:400]}"
            )
        logger.info(
            "local_sandbox_job_started",
            sandbox_id=self.id,
            pid=pid,
            container=self._container,
        )
        return pid

    async def write_file(self, path: str, content: bytes | str) -> None:
        target = self._host_path(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        data = content.encode("utf-8") if isinstance(content, str) else content
        target.write_bytes(data)

    async def read_file(self, path: str) -> bytes:
        try:
            return self._host_path(path).read_bytes()
        except (OSError, LocalSandboxError):
            return b""

    async def set_timeout(self, seconds: float) -> None:
        """Refresh the box's keepalive stamp — see DirectSandbox."""
        _touch_alive(self._layout)

    async def pause(self) -> None:
        result = await _run_host([self._runtime, "pause", self._container])
        if result.exit_code:
            logger.warning(
                "local_sandbox_pause_failed",
                sandbox_id=self.id,
                error=result.stderr.strip()[:200],
            )

    async def unpause(self) -> None:
        # Best-effort: an unpaused container reports an error we ignore,
        # which keeps ``connect`` a single unconditional call.
        await _run_host([self._runtime, "unpause", self._container])

    async def close(self) -> None:
        await _run_host([self._runtime, "rm", "-f", self._container])
        _collect_credentials(self._layout, self._credential_files)
        shutil.rmtree(self._layout.root, ignore_errors=True)
        logger.debug("local_sandbox_closed", sandbox_id=self.id)


# ---------------------------------------------------------------------
# provider
# ---------------------------------------------------------------------


def resolve_container_runtime(preference: str) -> str:
    """Pick the container CLI to drive.

    ``auto`` prefers Docker and falls back to Podman — Docker because it
    is the overwhelmingly common one, Podman because it is the rootless
    default on Fedora/RHEL and takes the same subcommands.
    """
    if preference in ("docker", "podman"):
        found = shutil.which(preference)
        if not found:
            raise LocalSandboxError(
                f"providers.sandbox.local.runtime is {preference!r} but that "
                "command is not on the engine host's PATH."
            )
        return found
    for candidate in ("docker", "podman"):
        found = shutil.which(candidate)
        if found:
            return found
    raise LocalSandboxError(
        "providers.sandbox.local.containment is 'container' but neither "
        "docker nor podman is on the engine host's PATH. Install one, set "
        "providers.sandbox.local.runtime explicitly, or use containment "
        "'direct'."
    )


class LocalSandboxProvider:
    """Mints sandboxes on the engine host, directly or in a container."""

    kind = "local"

    def __init__(
        self,
        *,
        containment: Containment = "direct",
        state_dir: Path | str = "",
        image: str = "",
        runtime: ContainerRuntime = "auto",
        network: str = "",
        run_args: list[str] | None = None,
    ) -> None:
        self.containment = containment
        self._image = image
        self._runtime_pref = runtime
        self._network = network
        self._run_args = list(run_args or [])
        root = Path(state_dir).expanduser() if state_dir else default_box_root()
        self._root = root
        if containment == "container" and not image:
            raise LocalSandboxError(
                "providers.sandbox.local.containment 'container' requires an "
                "`image` that has the coding-agent CLI installed — there is "
                "no sensible default, and a box without the CLI fails only "
                "once an agent tries to use it."
            )

    # -- layout ------------------------------------------------------

    def _layout(self, box_id: str) -> BoxLayout:
        return BoxLayout(box_id=box_id, root=self._root / "boxes" / box_id)

    def _container_name(self, box_id: str) -> str:
        return f"{CONTAINER_PREFIX}{box_id}"

    async def _live_container_names(self) -> set[str]:
        """Names of every crewlet container this runtime still has.

        One listing per reap, rather than an inspect per box: the reap
        runs on the `create` path, which an agent is waiting on.
        """
        try:
            runtime = resolve_container_runtime(self._runtime_pref)
        except LocalSandboxError:  # pragma: no cover — checked at create
            return set()
        result = await _run_host(
            [
                runtime,
                "ps",
                "-a",
                "--filter",
                f"name={CONTAINER_PREFIX}",
                "--format",
                "{{.Names}}",
            ]
        )
        if result.exit_code:
            # Cannot tell what is alive, so reap nothing: deleting a
            # live box's checkout is unrecoverable, a lingering
            # directory is not.
            logger.warning(
                "local_sandbox_reap_listing_failed",
                error=result.stderr.strip()[:200],
            )
            raise LocalSandboxError("container listing failed")
        return {line.strip() for line in result.stdout.splitlines() if line.strip()}

    def _box_last_seen(self, entry: Path) -> float:
        """When this box was last known to be in use.

        The keepalive stamp when there is one, else the directory's own
        mtime — which is the box's CREATION time and nothing else, since
        a directory's mtime does not move when files are written inside
        its subdirectories.
        """
        try:
            return (entry / ".crewlet" / "alive").stat().st_mtime
        except OSError:
            pass
        try:
            return entry.stat().st_mtime
        except OSError:  # pragma: no cover — vanished mid-scan
            return time.time()

    def _box_is_alive(self, entry: Path, live_containers: set[str]) -> bool:
        """Whether this box still has something running in it."""
        if self.containment == "container":
            return self._container_name(entry.name) in live_containers
        try:
            pid = int((entry / ".crewlet" / "box.pid").read_text().strip())
        except (OSError, ValueError):
            return False
        try:
            created = entry.stat().st_mtime
        except OSError:  # pragma: no cover — vanished mid-scan
            return False
        return _process_group_alive(pid, box_created=created)

    async def _reap_orphans(self, older_than_s: float) -> None:
        """Remove box directories an engine crash left behind.

        Teardown normally deletes a box, so anything here outlived the
        engine that made it. Bounded by the box TTL the caller already
        configures rather than a knob of its own — that value is exactly
        "how long a box may outlive its engine".

        **Age alone does not mean orphaned**, and reading it off the
        directory's mtime does not even mean age. A directory's mtime
        moves only when an entry is added or removed *directly in it*,
        and a box's root entries are all made at ``create`` — so the
        root mtime is the box's birth time, frozen, however busy the
        coding agent inside ``workspace/`` is. Reaping on it deleted the
        checkout, the seeded credentials and ``.crewlet/box.pid`` of
        every run that lasted longer than the TTL, while its process
        tree kept going: without the pid file nothing could ever kill
        it, so the job became an unkillable orphan writing into a
        directory that no longer existed.

        So the two questions are asked separately. *Is it in use?* — a
        live process group (direct) or an existing container, checked
        against the OS rather than inferred. *Has it been abandoned?* —
        the keepalive stamp the waiter refreshes on every poll, which
        is what ``set_timeout`` means on this backend. A box is reaped
        only when it fails BOTH: nothing running, and nothing has
        touched it for a whole TTL.

        A paused box is deliberately covered by the first: SIGSTOPped
        processes stop being heartbeated but stay alive, and bounding
        THAT wait belongs to the clarification pause TTL, which already
        owns it.
        """
        boxes = self._root / "boxes"
        if not boxes.is_dir():
            return
        cutoff = time.time() - max(60.0, older_than_s)
        try:
            candidates = [
                entry
                for entry in boxes.iterdir()
                if entry.is_dir() and self._box_last_seen(entry) < cutoff
            ]
        except OSError:  # pragma: no cover — racing another engine
            return
        if not candidates:
            # The common case, and the reason age is tested first: this
            # runs on `create`, which an agent is waiting on, so a fleet
            # with nothing stale pays no liveness call at all.
            return
        try:
            live = (
                await self._live_container_names()
                if self.containment == "container"
                else set()
            )
        except LocalSandboxError:
            return
        for entry in candidates:
            try:
                if self._box_is_alive(entry, live):
                    continue
                layout = BoxLayout(box_id=entry.name, root=entry)
                _collect_credentials(layout, _read_credential_map(layout))
                shutil.rmtree(entry, ignore_errors=True)
                logger.info("local_sandbox_orphan_reaped", sandbox_id=entry.name)
            except OSError:  # pragma: no cover — racing another engine
                continue

    # -- SandboxProvider protocol ------------------------------------

    async def create(self, spec: SandboxSpec) -> Sandbox:
        box_id = uuid.uuid4().hex[:16]
        layout = self._layout(box_id)
        layout.workspace.mkdir(parents=True, exist_ok=True)
        layout.root.chmod(0o700)
        (layout.root / ".tmp").mkdir(exist_ok=True)
        await self._reap_orphans(spec.timeout_s)
        _seed_credentials(layout, spec.credential_files)
        # The box records what it was seeded with and stamps itself
        # alive before anyone can reap it: `connect` is handed only an
        # id, possibly by a different process, so anything it needs to
        # know has to live in the box.
        _record_credential_map(layout, spec.credential_files)
        _touch_alive(layout)

        if self.containment == "direct":
            logger.info(
                "local_sandbox_created",
                sandbox_id=box_id,
                containment="direct",
                home=str(layout.root),
            )
            return DirectSandbox(
                layout, env=spec.env, credential_files=spec.credential_files
            )

        runtime = resolve_container_runtime(self._runtime_pref)
        name = self._container_name(box_id)
        argv = [
            runtime,
            "run",
            "-d",
            "--name",
            name,
            "-v",
            f"{layout.root}:{DEFAULT_SANDBOX_HOME}",
            "-w",
            f"{DEFAULT_SANDBOX_HOME}/{WORKSPACE_SUBDIR}",
            "-e",
            f"HOME={DEFAULT_SANDBOX_HOME}",
            # A reaping PID 1. The detached coding job is backgrounded
            # inside an `exec`, so without an init that reaps it, a
            # finished job lingers as a zombie — which `kill -0` reports
            # as alive, hanging the runner's completion check.
            "--init",
        ]
        if self._network:
            argv += ["--network", self._network]
        argv += self._run_args
        # PID 1 exists only to hold the namespace open; every command is
        # an `exec` into it, which is what makes a detached coding job
        # survive the turn that started it.
        argv += [self._image, "sleep", "infinity"]
        result = await _run_host(argv)
        if result.exit_code:
            shutil.rmtree(layout.root, ignore_errors=True)
            raise LocalSandboxError(
                f"could not start sandbox container from image "
                f"{self._image!r}: {(result.stderr or result.stdout).strip()[:400]}"
            )
        logger.info(
            "local_sandbox_created",
            sandbox_id=box_id,
            containment="container",
            image=self._image,
            container=name,
        )
        return ContainerSandbox(
            layout,
            runtime=runtime,
            container=name,
            env=spec.env,
            credential_files=spec.credential_files,
        )

    async def connect(self, sandbox_id: str) -> Sandbox:
        layout = self._layout(sandbox_id)
        if not layout.root.is_dir():
            raise LocalSandboxError(
                f"local sandbox {sandbox_id!r} is gone (its box directory "
                f"{layout.root} no longer exists) — the engine host was "
                "rebuilt, or the box was reaped."
            )
        # Rebuild the box's credential map from its own record. Without
        # it a reconnected box closes with an empty map, so the login
        # the coding agent refreshed mid-run is discarded — and EVERY
        # production teardown goes through `connect` (collect) or `kill`,
        # never the object `create` returned, so that was every run.
        credential_files = _read_credential_map(layout)
        if self.containment == "direct":
            box = DirectSandbox(layout, credential_files=credential_files)
            # `connect` auto-resumes, matching E2B: a paused box the
            # coordinator reattaches to must be runnable immediately.
            box.resume()
            return box
        runtime = resolve_container_runtime(self._runtime_pref)
        name = self._container_name(sandbox_id)
        box = ContainerSandbox(
            layout,
            runtime=runtime,
            container=name,
            credential_files=credential_files,
        )
        await box.unpause()
        return box

    async def kill(self, sandbox_id: str) -> None:
        """Terminate a box by id without resuming it.

        The pause reaper's primitive: ``connect`` resumes, so reclaiming
        a paused box through it would restart the work purely to stop it.
        Best-effort — a box that is already gone is not an error.
        """
        layout = self._layout(sandbox_id)
        if self.containment == "container":
            with contextlib.suppress(LocalSandboxError):
                runtime = resolve_container_runtime(self._runtime_pref)
                await _run_host([runtime, "rm", "-f", self._container_name(sandbox_id)])
        else:
            try:
                pid = int(layout.pid_file.read_text().strip())
            except (OSError, ValueError):
                pid = 0
            if pid:
                with contextlib.suppress(ProcessLookupError, PermissionError, OSError):
                    # SIGCONT first — a SIGSTOPped group never sees SIGKILL's
                    # predecessor signals, and a paused box is the main
                    # thing this method exists to reclaim.
                    os.killpg(pid, signal.SIGCONT)
                    os.killpg(pid, signal.SIGKILL)
        # Kill is a teardown like any other, and the box on disk may
        # hold a login the run refreshed before it was reclaimed. The
        # files are already there — collecting them needs no resume,
        # which is the one thing this method must not do.
        _collect_credentials(layout, _read_credential_map(layout))
        shutil.rmtree(layout.root, ignore_errors=True)
        logger.debug("local_sandbox_killed", sandbox_id=sandbox_id)


#: Environment variable overriding where local boxes are created.
BOX_ROOT_ENV = "CREWLET_SANDBOX_LOCAL_HOME"


def default_box_root() -> Path:
    """Default parent directory for local sandbox boxes."""
    override = os.environ.get(BOX_ROOT_ENV, "").strip()
    if override:
        return Path(override).expanduser()
    return Path.home() / ".crewlet" / "sandboxes"


__all__ = [
    "BOX_ROOT_ENV",
    "CONTAINER_PREFIX",
    "WORKSPACE_SUBDIR",
    "BoxLayout",
    "ContainerSandbox",
    "DirectSandbox",
    "LocalSandboxError",
    "LocalSandboxProvider",
    "default_box_root",
    "resolve_container_runtime",
]
