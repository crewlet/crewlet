"""The provisioner's ``.env`` store — one file, read AND written.

``crewlet slack provision`` persists every secret it obtains (bot
tokens, signing secrets, the rotated config-token pair) into a single
``.env`` file — the same one ``crewlet run`` loads — under the exact
``${VAR}`` names the company YAML references.  :class:`EnvStore` is the
only way provisioning touches that file, and it fixes two whole classes
of bugs the earlier split design had:

- **One path for read and write.**  The store is constructed with the
  resolved env path once; lookups and upserts use the same file, so a
  value persisted by run 1 is always visible to run 2 (previously the
  read side used a different fallback chain than the write side, which
  could strand a freshly rotated — and therefore *only valid* — config
  refresh token in a file no later run ever loaded).
- **The file wins over the shell.**  For any key present in the file,
  ``get`` returns the file's value; the process environment is only the
  bootstrap fallback for keys the file doesn't have yet.  A stale
  ``export SLACK_CONFIG_REFRESH_TOKEN=…`` left in the shell must never
  shadow the rotated pair the previous run persisted — with Slack's
  rotation semantics that shadowing bricks provisioning once the old
  access token expires.  (Note this is deliberately the OPPOSITE
  precedence from :func:`crewlet._env.load_env_file`, which serves
  engine boot; here the file is the provisioner's durable store and
  shell vars are one-time bootstrap input.)

Reading uses ``dotenv_values`` from python-dotenv — the exact parser
``crewlet run``'s ``load_env_file`` uses — and the writer only emits
representations that parser reads back byte-identically (see
:func:`_format_assignment`), so the provisioner and the engine can
never disagree about a stored value.

Writes are atomic (temp file + ``os.replace``) and the file is created
with owner-only permissions from the first byte — there is no window
where credentials sit world-readable, and a crash mid-write can never
truncate the previous contents.
"""

from __future__ import annotations

import contextlib
import os
import re
from pathlib import Path
from typing import Any, Protocol

from dotenv import dotenv_values

from crewlet._logging import get_logger

logger = get_logger("slack.envfile")

#: Matches ``KEY=…`` / ``export KEY=…`` assignment lines.
_ASSIGNMENT_RE = re.compile(r"^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=")

#: Characters that force quoting.  ``$`` is included because python-dotenv
#: interpolates ``${VAR}`` inside unquoted and double-quoted values —
#: only single quotes disable it, so ``$`` routes to the single-quoted
#: form below.
_NEEDS_QUOTING_RE = re.compile(r"[\s#'\"\\$]")


def _format_assignment(key: str, value: str) -> str:
    """Render one ``KEY=value`` line that python-dotenv reads back verbatim.

    Forms, chosen by content:

    - plain ``KEY=value`` when no character needs quoting;
    - ``KEY='value'`` when the value contains a bare ``$`` (quoting
      keeps it out of the double-quote escape rules);
    - ``KEY="value"`` with backslash escapes otherwise.

    Two value shapes have NO faithful python-dotenv representation and
    are refused loudly rather than persisted as something the engine
    would read back differently: ``${…}`` sequences (python-dotenv
    interpolates them in EVERY quoting form, single quotes included),
    and a bare ``$`` combined with a single quote (the only safe quoting
    for ``$`` can't contain ``'``).  Real Slack credentials are
    URL-safe strings, so neither shape occurs in practice.
    """
    if value and not _NEEDS_QUOTING_RE.search(value):
        return f"{key}={value}"
    if "${" in value:
        raise ValueError(
            f"cannot faithfully store {key}: python-dotenv interpolates "
            "${...} sequences in every quoting form, so the value would "
            "read back changed"
        )
    if "$" in value:
        if "'" in value:
            raise ValueError(
                f"cannot faithfully store {key}: the value combines '$' "
                "and a single quote, which python-dotenv cannot round-trip"
            )
        return f"{key}='{value}'"
    escaped = value.replace("\\", "\\\\").replace('"', '\\"')
    return f'{key}="{escaped}"'


def write_secret_file(path: Path, content: str) -> None:
    """Atomically write *content* to *path* with 0600 permissions.

    The temp file is created with owner-only permissions before any
    byte is written (no umask window), fsynced, then ``os.replace``d
    over the target — a crash at any point leaves either the old file
    or the new one, never a truncated hybrid.  Used for both ``.env``
    and the ``slack-apps.json`` ledger, whose client secrets Slack
    hands out exactly once.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f".{path.name}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, path)
    except BaseException:
        with contextlib.suppress(OSError):
            os.unlink(tmp)
        raise


def upsert_env_values(path: Path, values: dict[str, str]) -> None:
    """Set *values* in the ``.env`` at *path*, creating it if needed.

    Existing assignments for the given keys are replaced where they
    stand (extra duplicate assignments of the same key are removed so
    the written value is unambiguous); everything else in the file is
    left byte-identical.  Missing keys are appended at the end.
    """
    if not values:
        return
    lines = path.read_text(encoding="utf-8").splitlines() if path.exists() else []
    remaining = dict(values)
    out: list[str] = []
    for line in lines:
        match = _ASSIGNMENT_RE.match(line)
        key = match.group(1) if match else None
        if key is not None and key in values:
            if key in remaining:
                out.append(_format_assignment(key, remaining.pop(key)))
            # else: duplicate assignment of an updated key — drop it.
            continue
        out.append(line)
    for key, value in remaining.items():
        out.append(_format_assignment(key, value))
    write_secret_file(path, "\n".join(out) + "\n")
    logger.debug("env_file_updated", path=str(path), keys=sorted(values))


class EnvStore:
    """Read/write view over the provisioner's single ``.env`` file.

    See the module docstring for the two invariants this type enforces
    (one path for read+write; file values win over shell exports).
    ``os.environ`` is never mutated — state flows through the store,
    not through process-global side channels.
    """

    def __init__(self, path: Path) -> None:
        self.path = path
        self._file_values: dict[str, str] = self._load()

    def _load(self) -> dict[str, str]:
        if not self.path.exists():
            return {}
        return {
            key: value
            for key, value in dotenv_values(self.path).items()
            if value is not None
        }

    def get(self, key: str) -> str:
        """File value if the file has *key*, else the process env, else ``""``."""
        if key in self._file_values:
            return self._file_values[key]
        return os.environ.get(key, "")

    async def set(self, values: dict[str, str]) -> None:
        """Persist *values* to the file (atomic, 0600) and the live view.

        Async to match :class:`CredentialStore`; the file write itself is
        synchronous and cheap.
        """
        if not values:
            return
        upsert_env_values(self.path, values)
        self._file_values.update(values)


class CredentialStore(Protocol):
    """Where the Slack provisioner reads and persists its secrets.

    Two implementations: :class:`EnvStore` (a ``.env`` file the operator
    sources) and :class:`SecretValueEnvStore` (the encrypted
    ``secret_values`` table the engine reads back directly). Both satisfy
    the invariants in the module docstring — one place for read and
    write, and that place wins over a stale shell export.

    ``get`` is synchronous because every implementation pre-loads its
    contents; ``set`` is async because persisting may be a database
    round-trip.
    """

    def get(self, key: str) -> str: ...

    async def set(self, values: dict[str, str]) -> None: ...


class SecretValueEnvStore:
    """:class:`CredentialStore` over the encrypted ``secret_values`` table.

    The endpoint of the provisioning loop: the engine resolves ``${VAR}``
    from this same table, so a bot token minted here is live without an
    env file to source or a shell to be in — which also removes the
    *reason* the config-token rotation bug existed, since there is no
    second copy to go stale.

    Same precedence as :class:`EnvStore`: the store wins for keys it
    holds, and the process environment is only the bootstrap fallback for
    keys it does not — that is what lets an operator seed the first
    ``SLACK_CONFIG_TOKEN`` from their shell and have every rotation
    thereafter persist here.
    """

    def __init__(self, store: Any, *, source: str = "slack-provision") -> None:
        self._store = store
        self._source = source
        self._values: dict[str, str] = {}

    async def prime(self) -> None:
        """Load what the store already holds, so :meth:`get` stays sync."""
        self._values = await self._store.load_all()

    def get(self, key: str) -> str:
        if key in self._values:
            return self._values[key]
        return os.environ.get(key, "")

    async def set(self, values: dict[str, str]) -> None:
        for key, value in values.items():
            await self._store.put(
                key, value, updated_by="cli:slack-provision", source=self._source
            )
            self._values[key] = value
        if values:
            logger.info("slack_secrets_stored", keys=sorted(values))
