"""``crewlet slack provision`` CLI handler.

Creates (or updates) one Slack app per Slack-enabled agent in a Tier B
company YAML via Slack's App Manifest APIs, walks the operator through
the per-app OAuth install click, and writes every obtained secret into
one ``.env`` file under the exact ``${VAR}`` names the YAML references.

Two destinations, one contract
(:class:`~crewlet.slack.envfile.CredentialStore`).  By default that is
an env file — ``--env-file``, defaulting to whatever
:func:`crewlet._env.env_file_for` says ``crewlet run`` will load — read
and written through a single
:class:`~crewlet.slack.envfile.EnvStore`, so secrets persisted by one
run are always visible to the next, and file values win over stale
shell exports (see the envfile module docstring for why).  With
``--secret-store`` it is instead the encrypted ``secret_values`` table
the engine resolves ``${VAR}`` from directly, which removes the
source-and-restart step entirely.

Self-contained by default: it needs the company YAML and network access
to slack.com — no engine or queue.  ``--secret-store`` additionally
needs the database and the Tier A keyring.  See
``docs/integrations/slack.md`` for the operator flow.
"""

from __future__ import annotations

import argparse
import asyncio
import sys
import threading
from pathlib import Path
from typing import Any

from crewlet._env import env_file_for
from crewlet._logging import get_logger
from crewlet.db.secret_values import SecretStoreError
from crewlet.slack.api import SlackApiError, SlackManifestClient
from crewlet.slack.envfile import CredentialStore, EnvStore, SecretValueEnvStore
from crewlet.slack.provision import (
    CONFIG_REFRESH_TOKEN_VAR,
    FATAL_TOKEN_ERRORS,
    ProvisionOutcome,
    SlackAgentPlan,
    SlackProvisionError,
    build_agent_plans,
    resolve_config_token,
    run_provision,
)
from crewlet.slack.state import ProvisionState, load_state

logger = get_logger("slack.provision_cli")

#: Default name of the provisioning ledger, created next to the company
#: YAML.  Holds per-app credentials — gitignore it like ``.env``.
STATE_FILE_NAME = "slack-apps.json"


async def _read_stdin_line(prompt: str) -> str:
    """Read one line from stdin without wedging Ctrl+C teardown.

    A plain ``asyncio.to_thread(input, …)`` leaves the executor's
    non-daemon worker blocked in ``input()`` after a KeyboardInterrupt;
    ``asyncio.run``'s cleanup then joins that executor (and the
    interpreter joins the thread again at exit), so the process hangs at
    the prompt until the operator presses Enter.  A daemon reader thread
    hands the line back via ``call_soon_threadsafe`` and simply dies
    with the process on interrupt.
    """
    loop = asyncio.get_running_loop()
    future: asyncio.Future[str] = loop.create_future()

    def _deliver(setter, value) -> None:
        if not future.done():
            setter(value)

    def _reader() -> None:
        try:
            line = input(prompt)
        except (EOFError, KeyboardInterrupt):
            loop.call_soon_threadsafe(
                _deliver,
                future.set_exception,
                SlackProvisionError("stdin closed while waiting for the code"),
            )
        else:
            loop.call_soon_threadsafe(_deliver, future.set_result, line)

    threading.Thread(target=_reader, daemon=True, name="slack-provision-stdin").start()
    return await future


async def _prompt_install_code(plan: SlackAgentPlan, authorize_url: str) -> str:
    """Interactive installer: print the authorize URL, read the code."""
    print(f"\n→ Install “{plan.role_name}” (@{plan.handle})")
    print("  1. Open this URL in a browser and click Allow:")
    print(f"     {authorize_url}")
    print("  2. You land on the Crewlet OAuth page (or see ?code=… in the")
    print("     address bar if the API server isn't running yet).")
    return await _read_stdin_line(
        "  3. Paste the code (or the full redirect URL) here: "
    )


def _print_skipped(skipped: list[tuple[str, str]]) -> None:
    for handle, reason in skipped:
        print(f"  ~ {handle}: skipped — {reason}")


def _print_outcomes(outcomes: list[ProvisionOutcome]) -> None:
    for outcome in outcomes:
        if outcome.action == "failed":
            app = f" ({outcome.app_id})" if outcome.app_id else ""
            print(f"  ✗ {outcome.handle}{app}: FAILED — {outcome.error}")
            continue
        installed = "installed" if outcome.installed else "install unchanged"
        print(f"  ✓ {outcome.handle}: {outcome.action} ({outcome.app_id}), {installed}")
        for note in outcome.notes:
            print(f"      ! {note}")


async def _dry_run(
    client: SlackManifestClient,
    config_token: str,
    plans: list[SlackAgentPlan],
    state: ProvisionState,
) -> int:
    failures = 0
    for plan in plans:
        record = state.apps.get(plan.handle)
        action = "update" if record else "create"
        print(
            f"  • {plan.handle}: would {action} app "
            f"“{plan.manifest['display_information']['name']}”"
        )
        events = plan.manifest["settings"]["event_subscriptions"]
        print(f"      events → {events['request_url']}")
        print(f"      env    → {plan.bot_token_var}, {plan.signing_secret_var}")
        try:
            await client.validate_manifest(
                config_token,
                plan.manifest,
                app_id=record.app_id if record else "",
            )
            print("      manifest OK")
        except SlackApiError as exc:
            failures += 1
            print(f"      manifest INVALID: {exc}")
    return 1 if failures else 0


async def _open_credential_store(
    args: argparse.Namespace, env_path: Path
) -> tuple[CredentialStore, Any | None, str]:
    """Build the store the flags select, plus a DB handle to close.

    Returns ``(store, db, description)``; ``db`` is ``None`` for the env
    file. The description is what the run reports as the destination.
    """
    if not getattr(args, "secret_store", False):
        return EnvStore(env_path), None, str(env_path)

    from crewlet.config import load_bootstrap_config
    from crewlet.db.client import Database
    from crewlet.db.secret_values import SecretValueStore
    from crewlet.secrets import KeyringCipher

    dsn = getattr(args, "dsn", None) or ""
    cipher = None
    bootstrap_path = Path(getattr(args, "bootstrap", "config.yaml"))
    if bootstrap_path.exists():
        bootstrap = load_bootstrap_config(bootstrap_path)
        dsn = dsn or bootstrap.providers.database.dsn
        cipher = KeyringCipher.from_config(bootstrap.secrets)
    if not dsn:
        raise SecretStoreError(
            "--secret-store needs a database DSN — pass --dsn or point "
            f"--bootstrap at a Tier A config (looked at {bootstrap_path})"
        )
    if cipher is None:
        raise SecretStoreError(
            "--secret-store needs an encryption keyring — set Tier A "
            "`secrets.keys` in the bootstrap config (generate one with "
            "`crewlet secrets keygen`). Secrets are never stored unencrypted."
        )
    db = await Database.connect(dsn)
    store = SecretValueEnvStore(SecretValueStore(db, cipher))
    await store.prime()
    return store, db, "the encrypted secret store"


async def _run(
    args: argparse.Namespace,
    plans: list[SlackAgentPlan],
    env_path: Path,
    state_path: Path,
) -> int:
    state = load_state(state_path)
    env, db, destination = await _open_credential_store(args, env_path)
    try:
        return await _run_with_store(args, plans, env, state, state_path, destination)
    finally:
        if db is not None:
            await db.close()


async def _run_with_store(
    args: argparse.Namespace,
    plans: list[SlackAgentPlan],
    env: CredentialStore,
    state: ProvisionState,
    state_path: Path,
    destination: str,
) -> int:

    async with SlackManifestClient() as client:
        config_token = await resolve_config_token(client, env)

        async def _execute(token: str) -> int:
            if args.dry_run:
                return await _dry_run(client, token, plans, state)
            outcomes = await run_provision(
                plans,
                client=client,
                config_token=token,
                state=state,
                state_path=state_path,
                env=env,
                base_url=args.base_url,
                skip_install=bool(args.skip_install),
                reinstall=bool(args.reinstall),
                request_install_code=None
                if args.skip_install
                else _prompt_install_code,
            )
            print()
            _print_outcomes(outcomes)
            print(
                f"\nSecrets written to {destination}; app ledger in "
                f"{state_path} (keep both out of version control)."
            )
            print("Next: invite each bot to its team channels (/invite @handle), and")
            print("restart `crewlet run` so the engine reads the new credentials.")
            return 1 if any(o.action == "failed" for o in outcomes) else 0

        try:
            return await _execute(config_token)
        except SlackApiError as exc:
            # A bootstrap token with unknown expiry may turn out to be
            # expired only once we call the API — rotate and retry once
            # (run_provision is resumable, completed work is persisted).
            if exc.error not in FATAL_TOKEN_ERRORS or not env.get(
                CONFIG_REFRESH_TOKEN_VAR
            ):
                raise
            logger.warning("slack_config_token_stale_rotating", error=exc.error)
            config_token = await resolve_config_token(client, env, force_rotate=True)
            return await _execute(config_token)


def cmd_slack_provision(args: argparse.Namespace) -> int:
    """Entry point for ``crewlet slack provision``."""
    config_path: Path = args.company_config
    if not config_path.exists():
        print(f"Error: company config not found: {config_path}", file=sys.stderr)
        return 1

    base_url: str = (args.base_url or "").rstrip("/")
    if not base_url.startswith("https://"):
        print(
            "Error: --base-url must be the public HTTPS URL of the Crewlet "
            "API server (Slack requires HTTPS for webhook and OAuth "
            "redirect URLs — use a tunnel such as ngrok for local runs).",
            file=sys.stderr,
        )
        return 1

    # ONE env path for both reading and writing — and it must be the file
    # a later `crewlet run` actually loads, which is `_env.env_file_for`'s
    # decision (sibling of the YAML if it exists, else ./.env), not a
    # second copy of that rule. Hardcoding the sibling stranded secrets in
    # a file the engine never read whenever no sibling existed yet.
    env_path: Path = args.env_file or env_file_for(config_path)
    state_path: Path = args.state_file or (config_path.parent / STATE_FILE_NAME)

    from crewlet.config import load_company_config

    try:
        config = load_company_config(config_path)
    except Exception as exc:
        print(f"Error: invalid company config: {exc}", file=sys.stderr)
        return 1

    only_handles: set[str] | None = None
    if args.handles:
        only_handles = {h.strip() for h in args.handles.split(",") if h.strip()}

    try:
        plans, skipped = build_agent_plans(
            config, base_url=base_url, only_handles=only_handles
        )
    except SlackProvisionError as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1

    if skipped:
        print("Skipped seats:")
        _print_skipped(skipped)
    if not plans:
        print(
            "No Slack-enabled agent seats to provision — declare "
            "role.integrations.slack with ${VAR} placeholders first "
            "(see docs/integrations/slack.md).",
            file=sys.stderr,
        )
        return 1

    if not args.dry_run and not args.skip_install and not sys.stdin.isatty():
        print(
            "Error: the OAuth install step is interactive (it reads a pasted "
            "code from stdin). Run from a terminal, or pass --skip-install "
            "to only create/update the apps.",
            file=sys.stderr,
        )
        return 1

    print(f"Provisioning {len(plans)} Slack app(s) for {config.name} → {base_url}")
    try:
        return asyncio.run(_run(args, plans, env_path, state_path))
    except KeyboardInterrupt:
        print(
            "\nInterrupted — progress so far is saved; re-run to resume.",
            file=sys.stderr,
        )
        return 130
    except SecretStoreError as exc:
        print(f"Cannot use the secret store: {exc}", file=sys.stderr)
        return 1
    except (SlackApiError, SlackProvisionError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1
