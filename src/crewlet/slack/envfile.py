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
:func:`crewlet.env_file.format_assignment`), so the provisioner and
the engine can never disagree about a stored value.

Writes are atomic (temp file + ``os.replace``) and the file is created
with owner-only permissions from the first byte — there is no window
where credentials sit world-readable, and a crash mid-write can never
truncate the previous contents.
"""

from __future__ import annotations

import os
from pathlib import Path
from typing import Any, Protocol

from dotenv import dotenv_values

from crewlet._logging import get_logger
from crewlet.env_file import (
    ASSIGNMENT_RE,
    format_assignment,
    write_secret_file,
)

logger = get_logger("slack.envfile")


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
        match = ASSIGNMENT_RE.match(line)
        key = match.group(1) if match else None
        if key is not None and key in values:
            if key in remaining:
                out.append(format_assignment(key, remaining.pop(key)))
            # else: duplicate assignment of an updated key — drop it.
            continue
        out.append(line)
    for key, value in remaining.items():
        out.append(format_assignment(key, value))
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
