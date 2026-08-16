"""``crewlet llm`` — operate the subscription-authenticated CLI backends.

Everything an operator does to a ``providers.llm`` entry of type
``cli-agent`` that is *not* engine runtime: log the CLI in, mint or
store a headless token, check what the engine will see at boot, and move
a login onto another host.

The commands read the Tier B company YAML for the provider's ``cli``
block, and (only where a secret is written or read) the Tier A bootstrap
for the database DSN + keyring, exactly like ``crewlet secrets``.

See ``docs/concepts/subscription-llm-backends.md`` and
``docs/reference/cli.md``.
"""

from __future__ import annotations

import argparse
import asyncio
import sys
from pathlib import Path
from typing import Any

from crewlet._logging import get_logger

logger = get_logger("cli.llm")


# ---------------------------------------------------------------------
# Parser
# ---------------------------------------------------------------------


def add_llm_parser(sub: argparse._SubParsersAction) -> None:
    """Register the ``crewlet llm`` command group."""
    llm_p = sub.add_parser(
        "llm",
        help=(
            "Manage subscription-authenticated CLI LLM backends "
            "(providers.llm entries of type 'cli-agent')"
        ),
    )
    llm_sub = llm_p.add_subparsers(dest="llm_command")

    def _company(p: argparse.ArgumentParser) -> None:
        p.add_argument(
            "--company",
            type=Path,
            default=Path("company.yaml"),
            help="Path to the Tier B company YAML (default: company.yaml)",
        )

    def _store(p: argparse.ArgumentParser) -> None:
        p.add_argument(
            "--bootstrap",
            type=Path,
            default=Path("config.yaml"),
            help="Path to the Tier A bootstrap YAML (supplies DSN + keyring)",
        )
        p.add_argument("--dsn", type=str, default=None)

    list_p = llm_sub.add_parser(
        "list", help="List the cli-agent providers in a company config"
    )
    _company(list_p)

    login_p = llm_sub.add_parser(
        "login",
        help=(
            "Authenticate a cli-agent provider: broker the vendor's own "
            "login, mint a headless token, or drive a credential login"
        ),
    )
    login_p.add_argument("provider", help="providers.llm key, e.g. 'default'")
    _company(login_p)
    _store(login_p)
    login_p.add_argument(
        "--capture-token",
        action="store_true",
        help=(
            "Run the CLI's token-minting command (e.g. `claude setup-token`) "
            "and store the result in the encrypted secret store. The best "
            "option where the vendor offers one: no credential files, and it "
            "survives an ephemeral container"
        ),
    )
    login_p.add_argument(
        "--token-stdin",
        action="store_true",
        help=(
            "Read an already-minted subscription token from stdin and store "
            "it, instead of running the CLI at all"
        ),
    )
    login_p.add_argument(
        "--username",
        type=str,
        default="",
        help=(
            "Drive the CLI's credential login as this user. Only available "
            "for a profile that declares one — the Claude / Codex / Gemini "
            "subscriptions authenticate through browser OAuth and have no "
            "username/password grant to drive"
        ),
    )
    login_p.add_argument(
        "--password-stdin",
        action="store_true",
        help=(
            "Read the password for --username from stdin (never from argv, "
            "which is visible in `ps` and shell history). Omit on a terminal "
            "to be prompted without echo"
        ),
    )
    login_p.add_argument(
        "--print-token",
        action="store_true",
        help=(
            "Print a captured token instead of storing it — for piping into "
            "your own secret manager. Refuses to run on a terminal"
        ),
    )

    logout_p = llm_sub.add_parser(
        "logout", help="Log a cli-agent provider out and delete its credentials"
    )
    logout_p.add_argument("provider")
    _company(logout_p)

    status_p = llm_sub.add_parser("status", help="Ask the CLI who it is logged in as")
    status_p.add_argument("provider")
    _company(status_p)

    doctor_p = llm_sub.add_parser(
        "doctor",
        help=(
            "Verify a cli-agent provider end to end: binary, version, login, "
            "token accounting, and a real smoke completion"
        ),
    )
    doctor_p.add_argument(
        "provider", nargs="?", default="", help="Provider key (default: all)"
    )
    _company(doctor_p)
    _store(doctor_p)
    doctor_p.add_argument(
        "--no-smoke",
        action="store_true",
        help="Skip the smoke completion (does not spend subscription quota)",
    )

    export_p = llm_sub.add_parser(
        "export",
        help=(
            "Pack a provider's credential files into one portable blob so "
            "another host or a rebuilt container comes up logged in"
        ),
    )
    export_p.add_argument("provider")
    _company(export_p)
    _store(export_p)
    export_p.add_argument(
        "--secret-store",
        action="store_true",
        help=(
            "Write the bundle into the encrypted secret store under "
            "CREWLET_LLM_CLI_<KEY>_CREDENTIALS, where the engine reads it at "
            "boot. Without this the blob is printed to stdout"
        ),
    )

    import_p = llm_sub.add_parser(
        "import", help="Restore a credential bundle onto this host"
    )
    import_p.add_argument("provider")
    _company(import_p)
    _store(import_p)
    import_p.add_argument(
        "--secret-store",
        action="store_true",
        help="Read the bundle from the encrypted secret store instead of stdin",
    )


# ---------------------------------------------------------------------
# Shared resolution
# ---------------------------------------------------------------------


class _LLMCliError(RuntimeError):
    """An operator-facing problem; printed without a traceback."""


def _load_cli_entries(path: Path) -> dict[str, Any]:
    """Every ``cli-agent`` entry in the company YAML, keyed by name."""
    from crewlet.config import load_company_config

    if not path.exists():
        raise _LLMCliError(
            f"{path} not found — pass --company with the path to your Tier B "
            "company YAML."
        )
    config = load_company_config(path)
    return {
        key: cfg
        for key, cfg in config.providers.llm.items()
        if cfg.type == "cli-agent" and cfg.cli is not None
    }


def _open_entry(key: str, cfg: Any) -> tuple[str, Any, Any, Any]:
    """Return ``(key, llm_config, profile, workspace)`` for one entry."""
    from crewlet.config import _resolve_env_value
    from crewlet.providers.llm.cli_workspace import CLIWorkspaceManager

    profile = cfg.cli.profile()
    state_dir = str(_resolve_env_value(cfg.cli.state_dir)) if cfg.cli.state_dir else ""
    workspace = CLIWorkspaceManager(
        provider_key=key,
        profile=profile,
        state_dir=Path(state_dir).expanduser() if state_dir else None,
        extra_env={str(k): str(_resolve_env_value(v)) for k, v in cfg.cli.env.items()},
    )
    workspace.ensure_layout()
    return key, cfg, profile, workspace


def _pick(
    entries: dict[str, Any], company: Path, key: str
) -> tuple[str, Any, Any, Any]:
    """Look ``key`` up in already-loaded ``entries``, or explain."""
    if key not in entries:
        known = ", ".join(sorted(entries)) or "(none)"
        raise _LLMCliError(
            f"{company} has no cli-agent provider named {key!r}. "
            f"cli-agent providers in this config: {known}"
        )
    return _open_entry(key, entries[key])


def _resolve_one(args: argparse.Namespace) -> tuple[str, Any, Any, Any]:
    """Return ``(key, llm_config, profile, workspace)`` for ``args.provider``."""
    return _pick(_load_cli_entries(args.company), args.company, args.provider)


def _with_secret_store(args: argparse.Namespace, body: Any) -> int:
    """Run ``body(store)`` against the encrypted secret store."""
    from crewlet.cli_config_handlers import _connect, _secret_store
    from crewlet.db.secret_values import SecretStoreError
    from crewlet.secrets import SecretCipherError

    async def _run() -> int:
        db = await _connect(args)
        try:
            return await body(_secret_store(args, db))
        finally:
            await db.close()

    try:
        return asyncio.run(_run())
    except (
        SecretStoreError,
        SecretCipherError,
        RuntimeError,
        FileNotFoundError,
    ) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


def _guard(fn: Any) -> Any:
    """Turn an ``_LLMCliError`` into a clean message + exit code."""

    def _wrapped(args: argparse.Namespace) -> int:
        try:
            return fn(args)
        except _LLMCliError as exc:
            print(f"Error: {exc}", file=sys.stderr)
            return 1

    return _wrapped


# ---------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------


@_guard
def cmd_llm_list(args: argparse.Namespace) -> int:
    """List the cli-agent providers and whether each looks logged in."""
    entries = _load_cli_entries(args.company)
    if not entries:
        print(
            f"{args.company} declares no cli-agent LLM providers.\n"
            "Add one with `type: cli-agent` and a `cli:` block — see "
            "docs/concepts/subscription-llm-backends.md"
        )
        return 0
    width = max(len(k) for k in entries)
    print(f"{'PROVIDER'.ljust(width)}  AGENT         MODEL                LOGIN")
    for key in sorted(entries):
        _, cfg, profile, workspace = _open_entry(key, entries[key])
        state = "credentials" if workspace.has_credentials() else "-"
        if profile.token_env:
            from crewlet.secrets.resolver import resolve_env

            if resolve_env(profile.token_env):
                state = f"token ({profile.token_env})"
        print(
            f"{key.ljust(width)}  {profile.name:<12}  "
            f"{(cfg.model or '(cli default)')[:19]:<19}  {state}"
        )
    return 0


@_guard
def cmd_llm_login(args: argparse.Namespace) -> int:
    """Authenticate a cli-agent provider."""
    from crewlet.providers.llm import cli_login

    key, _cfg, profile, workspace = _resolve_one(args)

    if args.token_stdin:
        token = cli_login.read_password(f"{profile.token_env or 'Token'}: ")
        if not token:
            raise _LLMCliError("no token read from stdin")
        return _store_token(args, profile, key, token)

    if args.capture_token:
        result, token = cli_login.capture_token(workspace, profile)
        if not result.ok:
            print(result.output or f"exit {result.exit_code}", file=sys.stderr)
            return result.exit_code or 1
        if not token:
            raise _LLMCliError(
                f"`{profile.binary} {' '.join(profile.token_capture_args)}` "
                "printed no token matching the profile's pattern. Run it by "
                "hand to see its output, then store the token with "
                f"`crewlet llm login {key} --token-stdin`."
            )
        if args.print_token:
            if sys.stdout.isatty():
                raise _LLMCliError(
                    "refusing to print a credential to a terminal — redirect "
                    "stdout, or drop --print-token to store it encrypted"
                )
            print(token)
            return 0
        return _store_token(args, profile, key, token)

    if args.username:
        password = (
            cli_login.read_password()
            if (args.password_stdin or sys.stdin.isatty())
            else ""
        )
        if not password:
            raise _LLMCliError("no password supplied for --username")
        result = cli_login.credential_login(
            workspace, profile, username=args.username, password=password
        )
        print(result.output or ("Logged in." if result.ok else "Login failed."))
        return 0 if result.ok else (result.exit_code or 1)

    print(
        f"Starting {profile.binary} login for provider {key!r}.\n"
        f"  state dir: {workspace.state_dir}\n"
        "This runs the vendor's own login flow — follow its prompts. The "
        "credential lands in Crewlet's isolated directory, separate from "
        "your personal CLI login on this machine.\n"
    )
    result = cli_login.interactive_login(workspace, profile)
    if not result.ok:
        print(result.output or f"exit {result.exit_code}", file=sys.stderr)
        return result.exit_code or 1
    if not workspace.has_credentials():
        print(
            "The login command exited cleanly but wrote no credential file "
            f"under {workspace.credential_dir}. If this CLI keeps its login "
            "elsewhere, add the path to "
            f"providers.llm.{key}.cli.overrides.credential_paths.",
            file=sys.stderr,
        )
        return 1
    print(f"Logged in. Verify with `crewlet llm doctor {key}`.")
    return 0


def _store_token(args: argparse.Namespace, profile: Any, key: str, token: str) -> int:
    """Put a captured subscription token in the encrypted secret store."""
    name = profile.token_env
    if not name:
        raise _LLMCliError(
            f"the {profile.name!r} profile declares no token variable, so "
            "there is nowhere to store a token. Use the interactive login."
        )

    async def _body(store: Any) -> int:
        await store.put(name, token, updated_by="cli", source=f"llm login {key}")
        print(
            f"Stored {name} (encrypted) for provider {key!r}.\n"
            "The engine resolves it ahead of the environment at its next "
            "boot or config activation."
        )
        return 0

    return _with_secret_store(args, _body)


@_guard
def cmd_llm_logout(args: argparse.Namespace) -> int:
    from crewlet.providers.llm import cli_login

    key, _cfg, profile, workspace = _resolve_one(args)
    result = cli_login.logout(workspace, profile)
    print(
        f"Removed {key!r} credentials from {workspace.credential_dir}.\n"
        + (result.output if result.output else "")
    )
    if profile.token_env:
        print(
            f"A stored {profile.token_env} is a separate credential — remove "
            f"it with `crewlet secrets unset {profile.token_env}`."
        )
    return 0


@_guard
def cmd_llm_status(args: argparse.Namespace) -> int:
    from crewlet.providers.llm import cli_login

    _key, _cfg, profile, workspace = _resolve_one(args)
    result = cli_login.status(workspace, profile)
    print(result.output or "(no output)")
    return 0 if result.ok else (result.exit_code or 1)


@_guard
def cmd_llm_doctor(args: argparse.Namespace) -> int:
    """Verify one or every cli-agent provider, end to end."""
    from crewlet.providers.llm import cli_login

    entries = _load_cli_entries(args.company)
    keys = [args.provider] if args.provider else sorted(entries)
    if not keys:
        print(f"{args.company} declares no cli-agent LLM providers.")
        return 0

    failures = 0
    for index, key in enumerate(keys):
        _key, cfg, profile, workspace = _pick(entries, args.company, key)
        token = _resolve_token(cfg, profile)
        report = cli_login.build_report(
            provider_key=key,
            profile=profile,
            workspace=workspace,
            subscription_token=token,
        )
        if not args.no_smoke and report.binary_path:
            ok, detail = _smoke(key, cfg, profile, token)
            report.smoke_ok = ok
            report.smoke_detail = detail
            if not ok:
                report.problems.append(f"smoke completion failed: {detail}")
        if index:
            print()
        print(cli_login.format_report(report))
        failures += 0 if report.ok else 1
    return 1 if failures else 0


def _resolve_token(cfg: Any, profile: Any) -> str:
    from crewlet.config import _resolve_env_value
    from crewlet.secrets.resolver import resolve_env

    if cfg.cli.auth.token:
        return str(_resolve_env_value(cfg.cli.auth.token))
    return resolve_env(profile.token_env) if profile.token_env else ""


def _smoke(key: str, cfg: Any, profile: Any, token: str) -> tuple[bool, str]:
    """Run one real completion with one tool and check the round trip.

    This is the only check that exercises what actually breaks in
    practice — the CLI's flags, its output shape, and whether the model
    honours the JSON envelope the tool loop depends on. A profile can
    look perfect and still not produce a parseable tool call.
    """
    from crewlet.config import _resolve_env_value
    from crewlet.providers.llm.cli_agent import CLIAgentConfigError, CLIAgentProvider
    from crewlet.providers.llm.protocol import Message, ToolDef
    from crewlet.providers.llm.scope import bind_llm_scope

    # Resolve the API key the same way the engine does. Passing the raw
    # "${VAR}" reference through would make an `auth.mode: api-key`
    # provider smoke-test with a literal placeholder as its credential —
    # a confusing auth failure in the one command whose job is to
    # explain auth failures.
    keys = [str(_resolve_env_value(k)) for k in cfg.resolved_keys()]
    try:
        provider = CLIAgentProvider(
            provider_key=key,
            profile=profile,
            model=cfg.model,
            state_dir=(
                str(_resolve_env_value(cfg.cli.state_dir)) if cfg.cli.state_dir else ""
            ),
            timeout=cfg.cli.timeout_seconds,
            max_concurrent=1,
            auth_mode=cfg.cli.auth.mode,
            api_key=next((k for k in keys if k), ""),
            subscription_token=token,
            env={str(k): str(_resolve_env_value(v)) for k, v in cfg.cli.env.items()},
        )
    except CLIAgentConfigError as exc:
        # The engine would refuse this provider at boot too; report it
        # here rather than as a traceback out of the doctor.
        return False, str(exc)
    tool = ToolDef(
        name="crewlet_smoke_check",
        description="Report that the response protocol works.",
        parameters={
            "type": "object",
            "properties": {"ok": {"type": "boolean"}},
            "required": ["ok"],
        },
    )
    messages = [
        Message(
            role="system",
            content=(
                "You are validating an automation harness. Do exactly what "
                "the response format section asks and nothing more."
            ),
        ),
        Message(
            role="user",
            content="Call crewlet_smoke_check with ok=true.",
        ),
    ]

    async def _run() -> tuple[bool, str]:
        try:
            with bind_llm_scope("_doctor", phase="doctor"):
                completion = await provider.complete(
                    messages, [tool], tool_choice="required"
                )
        except Exception as exc:  # surfaced to the operator, never raised
            return False, str(exc)[:400]
        finally:
            await provider.aclose()
        names = [c.name for c in completion.tool_calls]
        if tool.name in names:
            return True, (
                f"{completion.input_tokens} in / {completion.output_tokens} out"
            )
        if completion.tool_calls:
            return False, f"model called {names} instead of {tool.name!r}"
        return False, (
            "model returned prose with no parseable tool call: "
            f"{completion.content.strip()[:200]!r}"
        )

    return asyncio.run(_run())


@_guard
def cmd_llm_export(args: argparse.Namespace) -> int:
    from crewlet.providers.llm import cli_login

    key, _cfg, profile, workspace = _resolve_one(args)
    blob, included = cli_login.export_bundle(workspace, profile)
    if not included:
        raise _LLMCliError(
            f"provider {key!r} has no credential files under "
            f"{workspace.credential_dir} — run `crewlet llm login {key}` "
            "first. (A provider authenticated by a stored token has nothing "
            "to export; the token already lives in the secret store.)"
        )
    if not args.secret_store:
        if sys.stdout.isatty():
            raise _LLMCliError(
                "refusing to print a credential bundle to a terminal — "
                "redirect stdout, or pass --secret-store to store it encrypted"
            )
        print(blob)
        return 0

    name = cli_login.bundle_env_name(key)

    async def _body(store: Any) -> int:
        await store.put(name, blob, updated_by="cli", source=f"llm export {key}")
        print(
            f"Stored {name} (encrypted): {', '.join(included)}.\n"
            "Any engine sharing this database now restores the login at boot "
            "when its own credential directory is empty."
        )
        return 0

    return _with_secret_store(args, _body)


@_guard
def cmd_llm_import(args: argparse.Namespace) -> int:
    from crewlet.providers.llm import cli_login

    key, _cfg, profile, workspace = _resolve_one(args)
    if args.secret_store:
        name = cli_login.bundle_env_name(key)
        holder: dict[str, str] = {}

        async def _body(store: Any) -> int:
            value = await store.get(name)
            if value is None:
                print(f"Error: {name} is not stored.", file=sys.stderr)
                return 1
            holder["blob"] = value
            return 0

        code = _with_secret_store(args, _body)
        if code:
            return code
        blob = holder.get("blob", "")
    else:
        if sys.stdin.isatty():
            raise _LLMCliError(
                "pipe the bundle on stdin, or pass --secret-store to read it "
                "from the encrypted store"
            )
        blob = sys.stdin.read()

    try:
        restored = cli_login.import_bundle(workspace, profile, blob)
    except cli_login.BundleError as exc:
        raise _LLMCliError(str(exc)) from exc
    if not restored:
        raise _LLMCliError("the bundle contained no credential files")
    print(
        f"Restored {', '.join(restored)} into {workspace.credential_dir}.\n"
        f"Verify with `crewlet llm doctor {key}`."
    )
    return 0


LLM_COMMANDS = {
    "list": cmd_llm_list,
    "login": cmd_llm_login,
    "logout": cmd_llm_logout,
    "status": cmd_llm_status,
    "doctor": cmd_llm_doctor,
    "export": cmd_llm_export,
    "import": cmd_llm_import,
}


__all__ = ["LLM_COMMANDS", "add_llm_parser"]
