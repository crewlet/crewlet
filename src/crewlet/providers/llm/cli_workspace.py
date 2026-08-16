"""Per-seat filesystem isolation for CLI-agent LLM backends.

A coding CLI is not a stateless HTTP endpoint.  Run ``claude`` twice and
the second run can see the first one's session transcript, its todo
list, its project notes, and the ``CLAUDE.md`` sitting in the working
directory.  That is the whole point of the tool when a human drives it,
and exactly wrong when an engine drives it on behalf of *seven different
agents* who each have their own memory, their own manager, and their own
confidentiality boundary.  Without isolation, the CTO seat's Plan phase
can read what the Engineer seat said an hour ago.

This module is the boundary.  Layout under the provider's state
directory::

    <state_dir>/
      credentials/                 # the subscription login, ONE per provider
      seats/<seat>/
        cache/                     # XDG_CACHE_HOME — warm, never memory
        home/                      # HOME + XDG config/data/state + vendor dirs
        work/<call-id>/            # cwd for one call, then removed

Three separations, each load-bearing:

**Between seats.**  Every seat gets its own ``home``; nothing in a CLI's
state layout is reachable across that boundary.

**Between calls.**  The profile's ``volatile_paths`` — sessions,
history, todos, transcripts — are deleted at the start and end of each
*generation* (see below), so a seat's own turn N+1 does not inherit turn
N's conversation either.  Crewlet's memory model is the agent diary and
the episode store; the CLI is a text model, and giving it a second,
invisible memory would make turns non-reproducible.

**Between the seat and the host.**  The child process gets an
*allowlisted* environment, not ``os.environ``.  Inheriting the engine's
environment would hand every seat the org's ``ANTHROPIC_API_KEY``,
``SLACK_BOT_TOKEN`` and database DSN — and, for a subscription backend,
an inherited API key silently bills the metered account instead of using
the subscription the operator configured.

**Generations, not per-call locks.**  Sub-agents spawned by one seat run
concurrently and belong to the same agent, so sharing a home between
them is harmless by definition.  Pruning is therefore keyed to the
seat's in-flight count crossing zero: the *first* concurrent call wipes
and seeds, the *last* one to finish wipes again and syncs any refreshed
credential back.  Concurrency inside a seat is preserved; nothing leaks
across seats or across turns.
"""

from __future__ import annotations

import asyncio
import contextlib
import hashlib
import os
import shutil
import sys
import uuid
from collections.abc import Iterator
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from pathlib import Path

from crewlet._logging import get_logger
from crewlet.providers.llm.cli_profiles import HOME_PLACEHOLDER, CLIAgentProfile
from crewlet.providers.llm.scope import sanitize_seat

logger = get_logger("llm.cli.workspace")

#: Host environment variables forwarded into every CLI process.
#:
#: An allowlist, not a denylist: a new secret added to the engine's
#: environment must not become reachable from an agent's CLI just
#: because nobody remembered to deny it.  These entries are the ones a
#: subprocess genuinely cannot work without — the executable search
#: path, locale, TLS trust, and proxy configuration (a corporate proxy
#: is exactly the deployment where the CLI cannot reach the vendor
#: without them).
BASE_PASSTHROUGH_ENV: tuple[str, ...] = (
    "PATH",
    "LANG",
    "LC_ALL",
    "LC_CTYPE",
    "LC_NUMERIC",
    "TERM",
    "TZ",
    "SSL_CERT_FILE",
    "SSL_CERT_DIR",
    "REQUESTS_CA_BUNDLE",
    "CURL_CA_BUNDLE",
    "NODE_EXTRA_CA_CERTS",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "ALL_PROXY",
    "NO_PROXY",
    "http_proxy",
    "https_proxy",
    "all_proxy",
    "no_proxy",
    # Windows cannot start a process without these.
    "SYSTEMROOT",
    "COMSPEC",
    "PATHEXT",
    "NUMBER_OF_PROCESSORS",
)

#: Directory mode for everything this module creates.  Credentials and
#: prompt transcripts both land under here, on a host that may run
#: other services.
_DIR_MODE = 0o700

#: Environment variable naming the root of every provider's state,
#: overriding the ``~/.crewlet/llm-cli`` default.  Deployments that run
#: the engine with a read-only or ephemeral home point this at a
#: persistent volume so the subscription login survives a restart.
STATE_DIR_ENV = "CREWLET_LLM_CLI_HOME"

#: Fallback root when neither config nor :data:`STATE_DIR_ENV` says
#: otherwise.
DEFAULT_STATE_ROOT = Path.home() / ".crewlet" / "llm-cli"


def default_state_dir(provider_key: str) -> Path:
    """Where provider ``provider_key`` keeps its CLI state by default."""
    root = os.environ.get(STATE_DIR_ENV, "").strip()
    base = Path(root).expanduser() if root else DEFAULT_STATE_ROOT
    return base / sanitize_seat(provider_key)


def _mkdir(path: Path) -> None:
    path.mkdir(parents=True, exist_ok=True)
    try:
        path.chmod(_DIR_MODE)
    except OSError:  # pragma: no cover — exotic filesystems (FAT, some NFS)
        logger.debug("cli_workspace_chmod_skipped", path=str(path))


def _remove(path: Path) -> None:
    """Delete ``path`` whether it is a file, a symlink or a directory.

    Never raises: pruning runs on the completion path of every call, and
    a stale lock file or a race with a still-exiting CLI process must
    not fail the agent's turn.
    """
    try:
        if path.is_symlink() or path.is_file():
            path.unlink()
        elif path.is_dir():
            shutil.rmtree(path, ignore_errors=True)
    except OSError:
        logger.debug("cli_workspace_remove_failed", path=str(path))


def file_digest(path: Path) -> str:
    """Content hash of ``path``, or ``""`` when it does not exist.

    Shared with the local sandbox provider, which uses the same
    changed-or-not test to decide whether a run refreshed a credential.
    """
    try:
        return hashlib.sha256(path.read_bytes()).hexdigest()
    except (OSError, ValueError):
        return ""


def _safe_join(base: Path, relative: str) -> Path | None:
    """Resolve ``relative`` under ``base``, refusing to escape it.

    Profile paths are operator-overridable config; a ``../../..`` entry
    in ``volatile_paths`` would otherwise delete outside the workspace.
    Returns ``None`` for anything that does not land under ``base``.
    """
    candidate = (base / relative).resolve()
    root = base.resolve()
    if candidate == root or root not in candidate.parents:
        logger.warning(
            "cli_workspace_path_escape_refused", base=str(root), relative=relative
        )
        return None
    return candidate


def copy_file_atomic(src: Path, dst: Path) -> bool:
    """Copy ``src`` onto ``dst`` atomically.  Returns whether it ran.

    Public because the local sandbox provider seeds and writes back the
    same credential files through the same guarantees (0600, temp +
    rename, never a partial file a concurrent reader could see).
    """
    if not src.is_file():
        return False
    _mkdir(dst.parent)
    tmp = dst.with_name(f".{dst.name}.{uuid.uuid4().hex}.tmp")
    try:
        shutil.copyfile(src, tmp)
        with contextlib.suppress(OSError):  # exotic filesystems (FAT, NFS)
            tmp.chmod(0o600)
        os.replace(tmp, dst)
        return True
    except OSError:
        logger.warning("cli_credential_copy_failed", src=str(src), dst=str(dst))
        _remove(tmp)
        return False


@dataclass(frozen=True, slots=True)
class SeatWorkspace:
    """One prepared, isolated place for a single CLI invocation."""

    seat: str
    home: Path
    work: Path
    env: dict[str, str]


@dataclass
class _SeatState:
    """Per-seat bookkeeping for the generation protocol."""

    lock: asyncio.Lock = field(default_factory=asyncio.Lock)
    in_flight: int = 0


@dataclass
class _SharedState:
    """Generation bookkeeping for ONE state directory.

    Keyed by the resolved directory rather than held per manager,
    because "which seat generation is open" is a property of the files
    on disk, not of the Python object that happens to be looking at
    them. Two managers over one directory arise in two ordinary
    situations, and both corrupt a live run without this:

    * **Per-phase models.** Running Plan on ``opus`` and Execute on
      ``sonnet`` means two ``providers.llm`` entries. Pointing both at
      one ``state_dir`` is how an operator shares a single login
      between them — and with independent counters, one entry's first
      call would prune the other's in-flight session.
    * **Hot reload.** ``apply_config`` rebuilds every provider. A fresh
      manager starting at ``in_flight = 0`` would prune the seat home
      of a call the outgoing provider is still running.
    """

    seats: dict[str, _SeatState] = field(default_factory=dict)
    credential_lock: asyncio.Lock = field(default_factory=asyncio.Lock)


#: Shared state per resolved state directory. A process global for the
#: same reason the secret source is: the thing being guarded is
#: process-wide (a directory), and every would-be holder is constructed
#: independently from config.
_SHARED: dict[Path, _SharedState] = {}


def _shared_state(state_dir: Path) -> _SharedState:
    """The :class:`_SharedState` for ``state_dir``, creating it once.

    Resolved first so ``~/x``, ``./x`` and ``/home/u/x`` are recognised
    as one directory — a near-miss would silently reinstate the very
    race this exists to remove.
    """
    try:
        key = state_dir.expanduser().resolve()
    except OSError:  # pragma: no cover — unresolvable path
        key = state_dir
    shared = _SHARED.get(key)
    if shared is None:
        shared = _SharedState()
        _SHARED[key] = shared
    return shared


def reset_shared_state() -> None:
    """Drop every cached :class:`_SharedState` (tests only)."""
    _SHARED.clear()


class CLIWorkspaceManager:
    """Owns one provider's state directory and hands out seat workspaces.

    One instance per ``providers.llm`` entry of type ``cli-agent``; the
    provider is shared across every seat, and this object is what keeps
    their state apart.
    """

    def __init__(
        self,
        *,
        provider_key: str,
        profile: CLIAgentProfile,
        state_dir: Path | None = None,
        extra_env: dict[str, str] | None = None,
    ) -> None:
        self.provider_key = provider_key
        self.profile = profile
        self.state_dir = (
            Path(state_dir) if state_dir else default_state_dir(provider_key)
        )
        self._extra_env = dict(extra_env or {})
        # Generation bookkeeping and the credential lock belong to the
        # DIRECTORY, not to this object: several providers can point at
        # one state dir to share a login, and a hot reload replaces this
        # object while calls are still in flight. See _SharedState.
        self._shared = _shared_state(self.state_dir)

    # -- layout ------------------------------------------------------

    @property
    def credential_dir(self) -> Path:
        """The provider's single subscription login, shared by all seats."""
        return self.state_dir / "credentials"

    def seat_root(self, seat: str) -> Path:
        return self.state_dir / "seats" / sanitize_seat(seat)

    def seat_home(self, seat: str) -> Path:
        return self.seat_root(seat) / "home"

    def ensure_layout(self) -> None:
        """Create the provider-level directories (idempotent)."""
        _mkdir(self.state_dir)
        _mkdir(self.credential_dir)

    def credential_files(self) -> list[Path]:
        """Existing files in the shared credential directory."""
        if not self.credential_dir.is_dir():
            return []
        return sorted(p for p in self.credential_dir.rglob("*") if p.is_file())

    def has_credentials(self) -> bool:
        return bool(self.credential_files())

    # -- environment -------------------------------------------------

    def build_env(self, home: Path, work: Path) -> dict[str, str]:
        """The child process environment: allowlist + isolation + profile.

        Precedence, later wins: host allowlist → home/XDG relocation →
        vendor ``dir_env`` → profile ``env`` → operator ``cli.env``.
        The operator's block is last so it can override anything, which
        is what makes an unforeseen CLI quirk fixable without a release.
        """
        env: dict[str, str] = {}
        names = (*BASE_PASSTHROUGH_ENV, *self.profile.passthrough_env)
        for name in names:
            value = os.environ.get(name)
            if value is not None:
                env[name] = value

        home_str = str(home)
        for name in self.profile.home_env or ["HOME"]:
            env[name] = home_str
        if sys.platform == "win32":  # pragma: no cover — POSIX in CI
            env.setdefault("USERPROFILE", home_str)
            env["APPDATA"] = str(home / "AppData" / "Roaming")
            env["LOCALAPPDATA"] = str(home / "AppData" / "Local")

        if self.profile.xdg:
            env["XDG_CONFIG_HOME"] = str(home / ".config")
            env["XDG_DATA_HOME"] = str(home / ".local" / "share")
            env["XDG_STATE_HOME"] = str(home / ".local" / "state")
            # Cache lives at the SEAT root, not under the per-generation
            # home: it holds no conversation, and re-downloading a
            # model catalogue on every turn is pure latency.
            env["XDG_CACHE_HOME"] = str(home.parent / "cache")

        # A CLI that shells out (or its Node runtime) must not spill
        # into the host's /tmp, where another seat could read it.
        env["TMPDIR"] = str(work / ".tmp")

        for name, relative in self.profile.dir_env.items():
            env[name] = str(home / relative)

        for name, value in self.profile.env.items():
            env[name] = value.replace(HOME_PLACEHOLDER, home_str)
        env.update(self._extra_env)
        return env

    # -- credential sync ---------------------------------------------

    async def seed_credentials(self, home: Path) -> None:
        """Copy the shared login into ``home`` before a generation runs."""
        paths = self.profile.credential_paths
        if not paths:
            return
        async with self._shared.credential_lock:
            for relative in paths:
                src = _safe_join(self.credential_dir, relative)
                dst = _safe_join(home, relative)
                if src is None or dst is None:
                    continue
                if src.is_file():
                    copy_file_atomic(src, dst)
                elif dst.is_file():
                    # The shared store has no copy of this file — a
                    # leftover in the seat home is a stale login from a
                    # previous configuration, not a credential.
                    _remove(dst)

    async def collect_credentials(self, home: Path) -> bool:
        """Sync a refreshed login back to the shared store.

        OAuth access tokens expire in hours and the CLI refreshes them
        mid-run.  Most vendors rotate the *refresh* token at the same
        time, so a run that silently discards the rewritten file logs
        the whole fleet out at the next expiry.  Copies back only when
        the content actually changed; returns whether anything did.

        Strictly a *refresh*: a seat file with no counterpart in the
        shared store is never adopted.  Adopting one would make
        ``crewlet llm logout`` unreliable — the operator deletes the
        shared login, and the next seat generation quietly puts its own
        stale copy back.  Those leftovers are removed by
        :meth:`seed_credentials` at the next acquire instead.
        """
        paths = self.profile.credential_paths
        if not paths:
            return False
        changed = False
        async with self._shared.credential_lock:
            for relative in paths:
                src = _safe_join(home, relative)
                dst = _safe_join(self.credential_dir, relative)
                if src is None or dst is None or not src.is_file():
                    continue
                if not dst.is_file():
                    continue
                if file_digest(src) == file_digest(dst):
                    continue
                if copy_file_atomic(src, dst):
                    changed = True
                    logger.info(
                        "cli_credential_refreshed",
                        provider=self.provider_key,
                        file=relative,
                    )
        return changed

    # -- generation lifecycle ----------------------------------------

    def _volatile_targets(self, home: Path) -> Iterator[Path]:
        for relative in self.profile.volatile_paths:
            target = _safe_join(home, relative)
            if target is not None:
                yield target

    def prune(self, home: Path) -> None:
        """Delete every conversation artefact the profile knows about."""
        for target in self._volatile_targets(home):
            _remove(target)

    def _seed_files(self, home: Path) -> None:
        for relative, content in self.profile.seed_files.items():
            target = _safe_join(home, relative)
            if target is None:
                continue
            _mkdir(target.parent)
            try:
                target.write_text(content, encoding="utf-8")
                target.chmod(0o600)
            except OSError:
                logger.warning("cli_seed_file_failed", path=str(target))

    @asynccontextmanager
    async def acquire(self, seat: str):
        """Prepare an isolated workspace for one CLI call on ``seat``.

        Yields a :class:`SeatWorkspace`.  The first concurrent call on a
        seat wipes the seat's conversation state and seeds the shared
        login; the last one to leave wipes again and syncs a refreshed
        login back.  Concurrent calls *within* one seat (batched
        sub-agents) share the generation and run in parallel.
        """
        seat = sanitize_seat(seat)
        state = self._shared.seats.setdefault(seat, _SeatState())
        home = self.seat_home(seat)
        call_id = uuid.uuid4().hex[:12]
        work = self.seat_root(seat) / "work" / call_id

        async with state.lock:
            if state.in_flight == 0:
                self.ensure_layout()
                _mkdir(home)
                _mkdir(self.seat_root(seat) / "cache")
                self.prune(home)
                self._seed_files(home)
                await self.seed_credentials(home)
            state.in_flight += 1
        _mkdir(work)
        _mkdir(work / ".tmp")

        try:
            yield SeatWorkspace(
                seat=seat, home=home, work=work, env=self.build_env(home, work)
            )
        finally:
            _remove(work)
            async with state.lock:
                state.in_flight -= 1
                if state.in_flight <= 0:
                    state.in_flight = 0
                    await self.collect_credentials(home)
                    self.prune(home)

    def purge_seat(self, seat: str) -> None:
        """Remove a seat's workspace entirely.

        Used when a role is decommissioned: the seat is gone, so its CLI
        home — which may hold a refreshed credential copy — should not
        linger on disk.
        """
        _remove(self.seat_root(seat))


__all__ = [
    "BASE_PASSTHROUGH_ENV",
    "DEFAULT_STATE_ROOT",
    "STATE_DIR_ENV",
    "CLIWorkspaceManager",
    "SeatWorkspace",
    "copy_file_atomic",
    "default_state_dir",
    "file_digest",
    "reset_shared_state",
]
