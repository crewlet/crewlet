"""Shared token-minting plumbing for the provisioning CLIs.

The integration-agnostic ``${VAR}``-minting contract — the config's own
references are the contract; sinks are where minted values leave the
process. Each provisioner (GitLab, Plane, ...) scans its integration's
config block for ``${VAR}`` references via :func:`referenced_env_vars`,
mints credentials into those vars, and hands the values to a
:class:`TokenSink`. A var that already has a value is considered
provisioned — the APIs never return a credential after creation, which is
what makes minting idempotent.

Three sinks ship:

- :class:`SecretStoreSink` — the encrypted ``secret_values`` table. The
  engine reads it back directly, so nothing has to be sourced into a
  shell or restarted into place.
- :class:`EnvFileSink` — an env file the operator sources.
- :class:`PrintSink` — ``export VAR=token`` lines on stdout.

``record`` and ``flush`` are **async** because a sink may persist
remotely. That also makes write-through affordable: a minted credential
is unretrievable from the upstream API, so the window between "minted"
and "persisted" is a window in which a crash orphans a live credential.
Every sink here persists inside ``record``; ``flush`` is the closing
safety net, not the moment the write happens.

Integration-specific seat scans (e.g. GitLab's conventional
``sandbox.env.GITLAB_TOKEN`` key) stay in the integration's own
``provision`` module; only the generic pieces live here.
"""

from __future__ import annotations

import os
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.env_refs import env_var_reference
from crewlet.env_refs import referenced_env_vars as _referenced_env_vars

logger = get_logger("provisioning")

#: ``export ``-prefixed assignment lines — the exact shape ``--print``
#: emits — are valid env-file syntax and must round-trip through
#: :class:`EnvFileSink`.
_EXPORT_PREFIX = "export "

#: Owner-only. A minted PAT written with the default umask is
#: world-readable on a normal Linux box, so the mode is applied when the
#: file is *created*, never chmod'd after the first byte is on disk.
_SECRET_FILE_MODE = 0o600


def referenced_env_vars(mapping: dict[str, str]) -> list[str]:
    """``${VAR}`` names referenced in a config block's values, in order."""
    return _referenced_env_vars(mapping.values())


def sole_env_var(value: str) -> str:
    """The ``${VAR}`` name when *value* is EXACTLY one whole reference.

    Returns ``""`` for a literal, an embedded reference
    (``"prefix-${VAR}"``) or a multi-reference value (``"${A}${B}"``) —
    the shapes a capture-into-one-var contract (e.g. Plane's
    server-generated webhook secret) can never satisfy, because the
    resolved value would be a concatenation, not the captured secret.
    """
    return env_var_reference(value)


class TokenSink(Protocol):
    """Where minted token values are written."""

    def existing(self, var: str) -> str:
        """Current value of ``var`` (empty ⇒ needs minting).

        Synchronous: every sink pre-loads what it knows at construction,
        so the minting loop can ask without awaiting.
        """
        ...

    async def record(self, var: str, token: str) -> None:
        """Persist a freshly minted/rotated token value.

        Write-through — when this returns, the value has survived the
        process.
        """
        ...

    async def flush(self) -> None:
        """Closing safety net; persisting already happened in `record`."""
        ...


class EnvFileSink:
    """Append/update ``VAR=value`` lines in an env file.

    A seat's ``${VAR}`` is considered already-provisioned when the file
    (or the process env) already carries a non-empty value for it, so
    re-runs never re-mint — the API won't return the token again anyway.

    ``export VAR=value`` lines (the shape ``--print`` emits, so pasted
    ``--print`` output round-trips) are understood: the key is indexed
    without the prefix, values may be single- or double-quoted, and an
    update rewrites the line in place *keeping* the ``export`` prefix.

    The file is created ``0600`` and rewritten on every ``record``, so a
    crash mid-run can never leave a minted-but-unrecorded credential.
    """

    def __init__(self, path: str) -> None:
        self._path = path
        self._lines: list[str] = []
        self._index: dict[str, int] = {}
        self._exported: set[str] = set()
        self._existed = os.path.exists(path)
        self._dirty = False
        if self._existed:
            with open(path, encoding="utf-8") as fh:
                self._lines = fh.read().splitlines()
            for i, line in enumerate(self._lines):
                stripped = line.strip()
                if not stripped or stripped.startswith("#") or "=" not in stripped:
                    continue
                key = stripped.split("=", 1)[0].strip()
                if key.startswith(_EXPORT_PREFIX):
                    key = key[len(_EXPORT_PREFIX) :].strip()
                    self._exported.add(key)
                self._index[key] = i

    def existing(self, var: str) -> str:
        if var in self._index:
            value = self._lines[self._index[var]].split("=", 1)[1].strip()
            if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
                value = value[1:-1]
            if value:
                return value
        return os.environ.get(var, "")

    async def record(self, var: str, token: str) -> None:
        prefix = _EXPORT_PREFIX if var in self._exported else ""
        line = f"{prefix}{var}={token}"
        if var in self._index:
            self._lines[self._index[var]] = line
        else:
            self._index[var] = len(self._lines)
            self._lines.append(line)
        self._dirty = True
        # Write-through: a minted credential is unretrievable from the
        # upstream API, so it must not live only in this list.
        self._write()

    async def flush(self) -> None:
        # A run that recorded nothing must not conjure a file into
        # existence (the flush-on-abort wrapper reaches here on every
        # preflight abort); an already-existing file is rewritten as-is.
        if not self._dirty and not self._existed:
            return
        self._write()

    def _write(self) -> None:
        """Rewrite the file, creating it ``0600`` when it is new.

        ``os.open`` with the mode applies it at creation; a plain
        ``open()`` + later ``chmod`` would leave the secrets
        world-readable for the window in between.
        """
        fd = os.open(
            self._path,
            os.O_WRONLY | os.O_CREAT | os.O_TRUNC,
            _SECRET_FILE_MODE,
        )
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write("\n".join(self._lines) + "\n")


class PrintSink:
    """Print ``export VAR=token`` for each minted token; env-only reads."""

    def __init__(self) -> None:
        self._recorded: list[tuple[str, str]] = []

    def existing(self, var: str) -> str:
        return os.environ.get(var, "")

    async def record(self, var: str, token: str) -> None:
        self._recorded.append((var, token))

    async def flush(self) -> None:
        for var, token in self._recorded:
            print(f"export {var}={token}")


class SecretStoreSink:
    """Write minted values into the encrypted ``secret_values`` table.

    The sink that closes the provisioning loop: the engine resolves
    ``${VAR}`` from this same table, so a minted credential is live
    without an env file to source or a shell to re-export. Values are
    sealed with the Tier A keyring on the way in.

    Existing values are pre-loaded once at construction (via
    :meth:`prime`) so :meth:`existing` stays synchronous — and the
    already-provisioned check keeps working across sinks, since a var
    already carrying a value in the environment still counts as minted.
    """

    def __init__(self, store: Any, *, updated_by: str, source: str) -> None:
        self._store = store
        self._updated_by = updated_by
        self._source = source
        self._known: dict[str, str] = {}

    async def prime(self) -> None:
        """Load what the store already holds, so re-runs never re-mint."""
        self._known = await self._store.load_all()

    def existing(self, var: str) -> str:
        stored = self._known.get(var, "")
        if stored:
            return stored
        return os.environ.get(var, "")

    async def record(self, var: str, token: str) -> None:
        await self._store.put(
            var, token, updated_by=self._updated_by, source=self._source
        )
        self._known[var] = token
        logger.info("secret_store_sink_recorded", var=var, source=self._source)

    async def flush(self) -> None:
        """No-op — :meth:`record` already committed each value."""
        return


def add_sink_arguments(parser: Any, *, default_env_file: str) -> None:
    """Register the sink-selection flags shared by every provisioning CLI.

    One definition so ``gitlab provision`` and ``plane provision`` cannot
    drift into offering different destinations for the same kind of
    minted credential.
    """
    from pathlib import Path

    parser.add_argument(
        "--secret-store",
        action="store_true",
        help=(
            "Write minted credentials into the encrypted secret_values "
            "table instead of an env file. The engine resolves ${VAR} from "
            "there directly, so nothing has to be sourced into a shell. "
            "Requires a Tier A keyring and DB DSN (--bootstrap / --dsn)."
        ),
    )
    parser.add_argument(
        "--env-file",
        type=Path,
        default=Path(default_env_file),
        help=(
            f"Env file to append/update minted tokens into "
            f"(default: {default_env_file}). Ignored with --secret-store."
        ),
    )
    parser.add_argument(
        "--print",
        dest="print_tokens",
        action="store_true",
        help="Print 'export VAR=token' lines to stdout instead of writing an env file",
    )
    parser.add_argument(
        "--bootstrap",
        type=Path,
        default=Path("config.yaml"),
        help=(
            "Path to the Tier A bootstrap YAML (supplies the DB DSN + "
            "encryption keyring for --secret-store)"
        ),
    )
    parser.add_argument(
        "--dsn",
        type=str,
        default=None,
        help="Override DB DSN instead of reading from --bootstrap",
    )


async def open_token_sink(args: Any, *, source: str) -> tuple[TokenSink, Any | None]:
    """Build the sink the CLI flags select, plus a DB handle to close.

    Returns ``(sink, db)``; ``db`` is ``None`` for the file and stdout
    sinks. ``--print`` wins over ``--secret-store`` so a dry inspection
    never writes anywhere.
    """
    if getattr(args, "print_tokens", False):
        return PrintSink(), None
    if not getattr(args, "secret_store", False):
        return EnvFileSink(str(args.env_file)), None

    from pathlib import Path

    from crewlet.config import load_bootstrap_config
    from crewlet.db.client import Database
    from crewlet.db.secret_values import SecretStoreError, SecretValueStore
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
    from crewlet.db.secret_values import load_secret_source
    from crewlet.secrets.resolver import install_secret_source

    db = await Database.connect(dsn)
    sink = SecretStoreSink(
        SecretValueStore(db, cipher), updated_by=f"cli:{source}", source=source
    )
    await sink.prime()
    # Install the snapshot for THIS process too. The provisioners resolve
    # their integration block's ${VAR}s to validate it, and a re-run must
    # see what a previous run stored — otherwise a signing secret already
    # in the table reads as unset and the config fails validation for a
    # credential that is, in fact, provisioned.
    install_secret_source(await load_secret_source(db, cipher))
    return sink, db


__all__ = [
    "EnvFileSink",
    "PrintSink",
    "SecretStoreSink",
    "TokenSink",
    "add_sink_arguments",
    "open_token_sink",
    "referenced_env_vars",
    "sole_env_var",
]
