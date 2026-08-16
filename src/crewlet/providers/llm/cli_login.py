"""Authenticating a CLI-agent backend, and moving that login around.

Subscription CLIs authenticate the operator, not the machine, through
the vendor's browser OAuth (PKCE) flow: open a URL, approve in a
session that may involve SSO, a one-time code, or a passkey, paste the
result back.  There is no password grant behind any of them, and no
supported headless equivalent for most.

So Crewlet does not re-implement vendor auth.  It does four things
around it, which together cover every deployment shape:

1. **Broker the vendor's own login** (:func:`interactive_login`) — run
   the real ``claude /login`` / ``codex login`` / ``opencode auth
   login`` attached to the operator's terminal, but inside the
   provider's *isolated* credential home, so the resulting token is
   Crewlet's to distribute and does not collide with the operator's
   personal CLI login on the same machine.
2. **Capture a reusable headless token** (:func:`capture_token`) where
   the vendor offers one — ``claude setup-token`` mints a long-lived
   subscription token for exactly this purpose.  Stored in the
   encrypted secret store, it needs no files, survives an ephemeral
   container, and sidesteps refresh-token rotation entirely.  Prefer
   this whenever the CLI supports it.
3. **Drive a credential login where one genuinely exists**
   (:func:`credential_login`) — the ``optionally ask for user/pass``
   path, declared per profile as
   :class:`~crewlet.providers.llm.cli_profiles.StdinLoginSpec`.  It is
   wired for CLIs that accept a credential on stdin or in the
   environment (``opencode auth login``, an operator's own wrapper, a
   self-hosted gateway) and deliberately *absent* from the Claude /
   Codex / Gemini profiles, because scripting a browser to type a
   password into them would break on the vendor's next login-page
   change and would fail outright against MFA.  Crewlet refuses to
   pretend a credential login exists where it does not.
4. **Adopt a login the machine already has**
   (:func:`adopt_host_login`) — the common starting state is an
   operator who has been using ``claude`` on this box for months.
   Crewlet does not read ``$HOME`` (the child process never sees it,
   which is the isolation the whole backend rests on), so that login is
   invisible until it is explicitly copied in. One command does the
   copy; nothing happens implicitly.
5. **Move the login between hosts** (:func:`export_bundle` /
   :func:`import_bundle`) — the credential directory as one encrypted
   blob in the secret store, so a fresh container, a second engine
   host, or a rebuilt VM comes up already authenticated.

Every function here is synchronous and terminal-attached: this is CLI
territory (``crewlet llm …``), not engine runtime.
"""

from __future__ import annotations

import base64
import io
import re
import subprocess
import tarfile
from dataclasses import dataclass, field
from pathlib import Path

from crewlet._logging import get_logger
from crewlet.providers.llm.cli_profiles import (
    HOME_PLACEHOLDER,
    MODEL_PLACEHOLDER,
    CLIAgentProfile,
)
from crewlet.providers.llm.cli_workspace import (
    CLIWorkspaceManager,
    copy_file_atomic,
    safe_join,
)

logger = get_logger("llm.cli.login")

#: Conventional secret-store / environment name holding a provider's
#: exported credential bundle.  ``{key}`` is the ``providers.llm`` key
#: upper-cased with non-alphanumerics folded to ``_``.  Mirrors the
#: conventional-env-var fallback the API-key providers already use, so
#: an operator who runs ``crewlet llm export`` gets pickup at the next
#: boot without editing YAML.
CREDENTIAL_BUNDLE_ENV_TEMPLATE = "CREWLET_LLM_CLI_{key}_CREDENTIALS"

#: Seconds a non-interactive auth helper gets before it is abandoned.
#: `claude setup-token` and friends complete in seconds once the
#: operator finishes in the browser; the wide budget is for the human,
#: not the process.
_AUTH_COMMAND_TIMEOUT = 600.0

#: Cap on an imported bundle after decompression.  A credential
#: directory is a few kilobytes of JSON; anything approaching this is a
#: wrong or hostile blob, and expanding it unbounded would be a zip
#: bomb against the engine host.
_MAX_BUNDLE_BYTES = 4 * 1024 * 1024


def bundle_env_name(provider_key: str) -> str:
    """Conventional bundle variable name for ``provider_key``."""
    slug = re.sub(r"[^A-Za-z0-9]+", "_", provider_key).strip("_").upper()
    return CREDENTIAL_BUNDLE_ENV_TEMPLATE.format(key=slug or "DEFAULT")


@dataclass
class CommandResult:
    """Outcome of one auth helper invocation."""

    exit_code: int
    stdout: str = ""
    stderr: str = ""

    @property
    def ok(self) -> bool:
        return self.exit_code == 0

    @property
    def output(self) -> str:
        return f"{self.stdout}\n{self.stderr}".strip()


@dataclass
class DoctorReport:
    """What ``crewlet llm doctor`` found about one provider."""

    provider_key: str
    agent: str
    binary: str
    binary_path: str = ""
    version: str = ""
    verified_against: str = ""
    state_dir: str = ""
    has_credentials: bool = False
    has_token: bool = False
    reports_usage: bool = False
    status_output: str = ""
    host_login: list[str] = field(default_factory=list)
    """Credential files found under the engine user's own home — a login
    the operator established by running the CLI themselves. Crewlet does
    not use these until they are adopted; reported so "not logged in" on
    a machine where the CLI plainly works is explained rather than
    baffling."""
    smoke_ok: bool | None = None
    smoke_detail: str = ""
    problems: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.problems


def host_login_files(
    profile: CLIAgentProfile, home: Path | None = None
) -> dict[str, Path]:
    """The profile's credential files as they exist under ``home``.

    ``home`` defaults to the engine user's real home — the login an
    operator established by running the CLI themselves. Only paths that
    actually exist are returned, so an empty result means "this machine
    has no login for this CLI".
    """
    root = (home or Path.home()).expanduser()
    found: dict[str, Path] = {}
    for relative in profile.credential_paths:
        candidate = root / relative
        if candidate.is_file():
            found[relative] = candidate
    return found


def adopt_host_login(
    workspace: CLIWorkspaceManager,
    profile: CLIAgentProfile,
    home: Path | None = None,
) -> list[str]:
    """Copy the machine's existing login into Crewlet's credential dir.

    Explicit rather than automatic, and a copy rather than a redirect,
    for two different reasons.

    *Explicit*, because the alternative is an engine that silently
    borrows a human's personal credentials the moment its own directory
    happens to be empty — the opposite of the isolation every other part
    of this backend maintains.

    *A copy*, because seats must not write into the operator's own
    ``~/.claude``: a fleet refreshing that file mid-session is a
    surprise the human never opted into. The cost is that the two
    credentials become independent lineages from one refresh token, so a
    vendor that rotates refresh tokens can invalidate whichever side
    refreshes second. Where the CLI mints a headless token
    (:func:`capture_token`) that is the better answer and avoids the
    fork entirely.

    Returns the relative paths copied.
    """
    files = host_login_files(profile, home)
    target = _login_home(workspace)
    copied: list[str] = []
    for relative, source in files.items():
        destination = safe_join(target, relative)
        if destination is None:
            continue
        destination.parent.mkdir(parents=True, exist_ok=True)
        if copy_file_atomic(source, destination):
            copied.append(relative)
    if copied:
        logger.info(
            "cli_host_login_adopted",
            provider=workspace.provider_key,
            agent=profile.name,
            files=len(copied),
        )
    return copied


def _login_home(workspace: CLIWorkspaceManager) -> Path:
    """The home the auth helpers run in.

    Deliberately the *same* directory the seats seed their credentials
    from, so a successful login is immediately live for every seat with
    no copy step that could be skipped or go stale.
    """
    workspace.ensure_layout()
    return workspace.credential_dir


def _login_env(
    workspace: CLIWorkspaceManager, *, extra: dict[str, str] | None = None
) -> dict[str, str]:
    home = _login_home(workspace)
    work = workspace.state_dir / "login-work"
    work.mkdir(parents=True, exist_ok=True)
    (work / ".tmp").mkdir(parents=True, exist_ok=True)
    env = workspace.build_env(home, work)
    if extra:
        env.update(extra)
    return env


def _argv(profile: CLIAgentProfile, args: list[str], home: Path) -> list[str]:
    rendered = [
        a.replace(HOME_PLACEHOLDER, str(home)).replace(MODEL_PLACEHOLDER, "")
        for a in args
    ]
    return [profile.binary, *rendered]


def interactive_login(
    workspace: CLIWorkspaceManager, profile: CLIAgentProfile
) -> CommandResult:
    """Run the vendor's own login attached to the operator's terminal.

    stdio is inherited, so the CLI's device-code prompt, its browser
    hand-off and its paste-back all behave exactly as they do when a
    human runs it — which is the point: Crewlet stays out of the flow
    and only controls *where* the credential lands.
    """
    home = _login_home(workspace)
    argv = _argv(profile, profile.login_args, home)
    logger.info("cli_login_starting", agent=profile.name, binary=profile.binary)
    try:
        completed = subprocess.run(argv, env=_login_env(workspace), check=False)
    except FileNotFoundError:
        return CommandResult(127, stderr=f"{profile.binary}: command not found")
    except KeyboardInterrupt:  # pragma: no cover — operator-driven
        return CommandResult(130, stderr="login cancelled")
    workspace.prune(home)
    return CommandResult(completed.returncode)


def _run_captured(
    workspace: CLIWorkspaceManager,
    profile: CLIAgentProfile,
    args: list[str],
    *,
    stdin_text: str = "",
    extra_env: dict[str, str] | None = None,
) -> CommandResult:
    home = _login_home(workspace)
    argv = _argv(profile, args, home)
    try:
        completed = subprocess.run(
            argv,
            env=_login_env(workspace, extra=extra_env),
            input=stdin_text,
            capture_output=True,
            text=True,
            timeout=_AUTH_COMMAND_TIMEOUT,
            check=False,
        )
    except FileNotFoundError:
        return CommandResult(127, stderr=f"{profile.binary}: command not found")
    except subprocess.TimeoutExpired:
        return CommandResult(124, stderr=f"{profile.binary} timed out")
    return CommandResult(completed.returncode, completed.stdout, completed.stderr)


def capture_token(
    workspace: CLIWorkspaceManager, profile: CLIAgentProfile
) -> tuple[CommandResult, str]:
    """Mint and extract a long-lived subscription token.

    Returns ``(result, token)``; ``token`` is empty when the command
    failed or printed nothing matching the profile's pattern.  The
    caller stores it in the encrypted secret store under the profile's
    ``token_env`` — never on disk in plaintext.
    """
    if not profile.token_capture_args:
        return (
            CommandResult(
                2,
                stderr=(
                    f"the {profile.name!r} CLI has no token-minting command; "
                    "use `crewlet llm login` for its interactive flow"
                ),
            ),
            "",
        )
    result = _run_captured(workspace, profile, profile.token_capture_args)
    workspace.prune(_login_home(workspace))
    if not result.ok:
        return result, ""
    return result, extract_token(result.stdout, profile.token_capture_pattern)


def extract_token(output: str, pattern: str) -> str:
    """Pull a token out of a capture command's stdout.

    With a pattern, the first capturing group of the first match.
    Without one, the last non-empty line — the convention for a command
    whose entire job is to print a credential.
    """
    text = output or ""
    if pattern:
        match = re.search(pattern, text)
        if not match:
            return ""
        return (match.group(1) if match.groups() else match.group(0)).strip()
    lines = [line.strip() for line in text.splitlines() if line.strip()]
    return lines[-1] if lines else ""


def credential_login(
    workspace: CLIWorkspaceManager,
    profile: CLIAgentProfile,
    *,
    username: str,
    password: str,
) -> CommandResult:
    """Drive a CLI's non-interactive credential login.

    Only available for a profile that declares ``stdin_login``.  The
    password reaches the child through stdin (or a declared env var) and
    never through argv, which would expose it in ``ps`` and in the
    shell history of whoever ran the command.
    """
    spec = profile.stdin_login
    if spec is None:
        return CommandResult(
            2,
            stderr=(
                f"the {profile.name!r} CLI authenticates through the vendor's "
                "browser OAuth flow — there is no username/password login to "
                "drive. Run `crewlet llm login` (which brokers that flow), or "
                "`crewlet llm login --capture-token` where the vendor mints a "
                "headless token. If your build of this CLI does accept a "
                "credential, declare it under "
                "providers.llm.<key>.cli.overrides.stdin_login."
            ),
        )
    args = [a.replace("{username}", username) for a in spec.args]
    stdin_text = spec.stdin_template.replace("{username}", username).replace(
        "{password}", password
    )
    extra = {
        name: value.replace("{username}", username).replace("{password}", password)
        for name, value in spec.env.items()
    }
    result = _run_captured(
        workspace, profile, args, stdin_text=stdin_text, extra_env=extra
    )
    workspace.prune(_login_home(workspace))
    return result


def logout(workspace: CLIWorkspaceManager, profile: CLIAgentProfile) -> CommandResult:
    """Run the CLI's logout, then remove every credential file.

    The file removal is not belt-and-braces: several CLIs clear only the
    entry for the *active* profile and leave the rest of the credential
    document behind, which would keep a revoked login seeding into every
    seat.
    """
    result = (
        _run_captured(workspace, profile, profile.logout_args)
        if profile.logout_args
        else CommandResult(0)
    )
    home = _login_home(workspace)
    for relative in profile.credential_paths:
        target = home / relative
        try:
            if target.is_file():
                target.unlink()
        except OSError:
            logger.warning("cli_logout_unlink_failed", path=str(target))
    workspace.prune(home)
    return result


def status(workspace: CLIWorkspaceManager, profile: CLIAgentProfile) -> CommandResult:
    """Ask the CLI who it is logged in as, when it can answer."""
    if not profile.status_args:
        return CommandResult(0, stdout="(this CLI has no status command)")
    return _run_captured(workspace, profile, profile.status_args)


# ---------------------------------------------------------------------
# Moving a login between hosts
# ---------------------------------------------------------------------


def export_bundle(
    workspace: CLIWorkspaceManager, profile: CLIAgentProfile
) -> tuple[str, list[str]]:
    """Pack the provider's credential files into one portable blob.

    Returns ``(base64_gzip_tar, included_relative_paths)``.  Only the
    profile's declared ``credential_paths`` go in — never sessions,
    history or caches, which would carry one host's conversations onto
    another.
    """
    home = _login_home(workspace)
    included: list[str] = []
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w:gz") as tar:
        for relative in profile.credential_paths:
            source = home / relative
            if not source.is_file():
                continue
            info = tar.gettarinfo(str(source), arcname=relative)
            info.mode = 0o600
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            # Fixed mtime: the blob is stored in the secret store and
            # compared for change on write-back, so byte-identical
            # credentials must produce a byte-identical bundle.
            info.mtime = 0
            with source.open("rb") as handle:
                tar.addfile(info, handle)
            included.append(relative)
    return base64.b64encode(raw.getvalue()).decode("ascii"), included


class BundleError(ValueError):
    """An exported credential bundle could not be restored."""


def _normalise_member(name: str) -> str:
    """Canonical form of an archive entry name for the allowlist check.

    Strips a leading ``./`` (some tar writers add one) and trailing
    slashes, and nothing else. Notably *not* ``lstrip("./")``, which
    strips dot *characters* — that would turn ``.claude/.credentials``
    into ``claude/.credentials`` and reject every real credential path,
    since all of them are dotfiles.
    """
    cleaned = name.strip()
    while cleaned.startswith("./"):
        cleaned = cleaned[2:]
    return cleaned.rstrip("/")


def import_bundle(
    workspace: CLIWorkspaceManager, profile: CLIAgentProfile, blob: str
) -> list[str]:
    """Restore a bundle produced by :func:`export_bundle`.

    Rejects anything that is not a small archive of the profile's own
    credential paths: the blob arrives from the secret store or an
    operator's paste, and an archive is an execution surface (absolute
    paths, ``..`` traversal, symlinks, decompression bombs) if it is
    unpacked on trust.
    """
    try:
        raw = base64.b64decode(blob.strip().encode("ascii"), validate=True)
    except (ValueError, UnicodeEncodeError) as exc:
        raise BundleError("credential bundle is not valid base64") from exc

    allowed = {_normalise_member(p) for p in profile.credential_paths}
    home = _login_home(workspace)
    restored: list[str] = []
    total = 0
    try:
        with tarfile.open(fileobj=io.BytesIO(raw), mode="r:gz") as tar:
            for member in tar.getmembers():
                name = _normalise_member(member.name)
                if not member.isfile():
                    raise BundleError(
                        f"credential bundle contains a non-file entry {name!r}"
                    )
                if name not in allowed:
                    raise BundleError(
                        f"credential bundle contains {name!r}, which is not a "
                        f"credential path of the {profile.name!r} profile"
                    )
                total += member.size
                if total > _MAX_BUNDLE_BYTES:
                    raise BundleError("credential bundle is implausibly large")
                extracted = tar.extractfile(member)
                if extracted is None:  # pragma: no cover — isfile() checked
                    continue
                target = home / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(extracted.read())
                target.chmod(0o600)
                restored.append(name)
    except tarfile.TarError as exc:
        raise BundleError(
            f"credential bundle is not a readable archive: {exc}"
        ) from exc
    return restored


def restore_from_secret(
    workspace: CLIWorkspaceManager, profile: CLIAgentProfile, blob: str
) -> bool:
    """Best-effort bundle restore during engine boot.

    Returns whether anything was written.  Never raises: a malformed
    bundle must surface as "not logged in" (which ``crewlet llm doctor``
    explains) rather than crash the engine's provider construction.
    """
    if not blob.strip():
        return False
    if workspace.has_credentials():
        # A live login on disk wins: it may already carry a refreshed
        # token the stored bundle predates.
        return False
    try:
        restored = import_bundle(workspace, profile, blob)
    except BundleError as exc:
        logger.warning(
            "cli_credential_bundle_invalid",
            provider=workspace.provider_key,
            error=str(exc),
        )
        return False
    if restored:
        logger.info(
            "cli_credential_bundle_restored",
            provider=workspace.provider_key,
            files=len(restored),
        )
    return bool(restored)


# ---------------------------------------------------------------------
# Diagnosis
# ---------------------------------------------------------------------


def probe_version(
    workspace: CLIWorkspaceManager, profile: CLIAgentProfile
) -> CommandResult:
    """Run the CLI's version probe inside the isolated home."""
    return _run_captured(workspace, profile, profile.version_args)


def build_report(
    *,
    provider_key: str,
    profile: CLIAgentProfile,
    workspace: CLIWorkspaceManager,
    subscription_token: str = "",
) -> DoctorReport:
    """Collect everything ``crewlet llm doctor`` reports, minus the smoke
    completion (which the caller runs, because it needs an event loop)."""
    import shutil as _shutil

    report = DoctorReport(
        provider_key=provider_key,
        agent=profile.name,
        binary=profile.binary,
        binary_path=_shutil.which(profile.binary) or "",
        verified_against=profile.verified_against,
        state_dir=str(workspace.state_dir),
        has_credentials=workspace.has_credentials(),
        has_token=bool(subscription_token),
        reports_usage=bool(profile.input_token_paths or profile.output_token_paths),
    )
    if not report.binary_path:
        report.problems.append(
            f"{profile.binary!r} is not on PATH — install the CLI on the host "
            "running the engine"
        )
        return report

    version = probe_version(workspace, profile)
    version_lines = (version.stdout or version.stderr).strip().splitlines()
    report.version = version_lines[0].strip() if version_lines else ""
    if not version.ok:
        report.problems.append(
            f"`{profile.binary} {' '.join(profile.version_args)}` exited "
            f"{version.exit_code}: {version.output[:200]}"
        )

    if not report.has_credentials and not report.has_token:
        report.host_login = sorted(host_login_files(profile))
        if report.host_login:
            # The CLI works for the operator on this very machine, so
            # "no login found" alone reads as a bug in Crewlet. Name the
            # login we can see and the one command that adopts it.
            report.problems.append(
                "no login of its own, but this machine has one at "
                f"~/{report.host_login[0]} — adopt it with "
                f"`crewlet llm login {provider_key} --from-host`"
                + (
                    f", or mint a headless {profile.token_env} with "
                    "`--capture-token` (preferred: no shared refresh token)"
                    if profile.token_capture_args
                    else ""
                )
            )
        else:
            report.problems.append(
                f"no login found: run `crewlet llm login {provider_key}`"
                + (
                    f" (or `--capture-token` to mint a {profile.token_env})"
                    if profile.token_capture_args
                    else ""
                )
            )

    if profile.status_args:
        result = status(workspace, profile)
        report.status_output = result.output[:500]

    if not report.reports_usage:
        report.problems.append(
            f"the {profile.name!r} profile reports no token usage, so budgets "
            "for seats on this provider run on a 4-chars-per-token estimate"
        )
    return report


def format_report(report: DoctorReport) -> str:
    """Render a :class:`DoctorReport` for the terminal."""
    lines = [
        f"provider      : {report.provider_key}",
        f"cli agent     : {report.agent}",
        f"binary        : {report.binary_path or report.binary + ' (NOT FOUND)'}",
        f"version       : {report.version or '(unknown)'}",
        f"written for   : {report.verified_against or '(operator-defined)'}",
        f"state dir     : {report.state_dir}",
        f"credentials   : {'present' if report.has_credentials else 'none on disk'}",
        f"token env     : {'set' if report.has_token else 'unset'}",
        f"token usage   : {'reported by CLI' if report.reports_usage else 'estimated'}",
    ]
    if report.host_login:
        lines.append(f"host login    : {', '.join(report.host_login)} (not adopted)")
    if report.status_output:
        lines.append(f"cli status    : {report.status_output.splitlines()[0]}")
    if report.smoke_ok is not None:
        lines.append(
            f"smoke test    : {'ok' if report.smoke_ok else 'FAILED'}"
            + (f" — {report.smoke_detail}" if report.smoke_detail else "")
        )
    if report.problems:
        lines.append("problems:")
        lines.extend(f"  - {p}" for p in report.problems)
    else:
        lines.append("problems      : none")
    return "\n".join(lines)


def read_password(prompt: str = "Password: ") -> str:
    """Read a credential without echoing it.

    Prefers stdin when it is a pipe so the value can come from a secret
    manager (``vault read … | crewlet llm login … --password-stdin``)
    rather than a human's keyboard; falls back to ``getpass`` on a
    terminal. Never accepts a password on argv.
    """
    import getpass
    import sys as _sys

    if not _sys.stdin.isatty():
        return _sys.stdin.read().rstrip("\n")
    return getpass.getpass(prompt)


__all__ = [
    "CREDENTIAL_BUNDLE_ENV_TEMPLATE",
    "BundleError",
    "CommandResult",
    "DoctorReport",
    "adopt_host_login",
    "build_report",
    "bundle_env_name",
    "capture_token",
    "credential_login",
    "export_bundle",
    "extract_token",
    "format_report",
    "host_login_files",
    "import_bundle",
    "interactive_login",
    "logout",
    "probe_version",
    "read_password",
    "restore_from_secret",
    "status",
]
