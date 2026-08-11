"""Typed queries against the ``secret_values`` table — the secret store.

One row per ``${VAR}`` name, each value sealed with the Tier A keyring.
:class:`SecretValueStore` is the read/write surface;
:class:`DatabaseSecretSource` is the boot-loaded snapshot that
``crewlet.config._resolve_env_value`` consults ahead of ``os.environ``.

Unlike :class:`~crewlet.db.company_config.CompanyConfigStore`, this store
takes the cipher itself rather than accepting pre-sealed payloads.  That
is deliberate: each row binds its own ``name`` as AEAD associated data, so
leaving the sealing to callers would spread one easily-mismatched
convention across every write site.  One place owns the AAD, and a
ciphertext lifted into another row simply fails to decrypt.

See ``docs/concepts/secret-store.md``.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Any

from crewlet._logging import get_logger
from crewlet.db.client import Database
from crewlet.env_refs import is_env_var_name
from crewlet.secrets.cipher import SecretCipher, parse_key_id

logger = get_logger("db.secret_values")

#: Associated-data domain for a stored secret. The row's ``name`` is bound
#: in, so a ciphertext moved between rows fails to decrypt instead of
#: silently answering as a different secret.
_AAD_PREFIX = "secret_values/"


def _aad(name: str) -> str:
    return f"{_AAD_PREFIX}{name}"


class SecretStoreError(Exception):
    """The secret store cannot serve a request."""


@dataclass
class SecretValueRecord:
    """Metadata for one stored secret — deliberately **without** the value.

    Listing secrets is an inventory operation; a record type that cannot
    carry a value is what keeps ``crewlet secrets list`` and any future
    read path structurally incapable of leaking one.
    """

    name: str
    key_id: str
    updated_at: datetime
    updated_by: str
    source: str


def _row_to_record(row: dict[str, Any]) -> SecretValueRecord:
    return SecretValueRecord(
        name=row["name"],
        key_id=row["key_id"],
        updated_at=row["updated_at"],
        updated_by=row["updated_by"],
        source=row["source"],
    )


class SecretValueStore:
    """Persist and query encrypted secret values keyed by var name.

    A keyring is **required**: every method that touches a value raises
    :class:`SecretStoreError` when ``cipher is None``.  Failing loudly
    beats the alternative — a missing keyring silently yielding ``""``
    produces an empty Bearer token that looks live to every layer until
    the provider rejects it, hours later and far from the cause.
    """

    def __init__(self, db: Database, cipher: SecretCipher | None) -> None:
        self._db = db
        self._cipher = cipher

    @property
    def enabled(self) -> bool:
        """True when a keyring is configured, so values can be sealed."""
        return self._cipher is not None

    def _require_cipher(self) -> SecretCipher:
        if self._cipher is None:
            raise SecretStoreError(
                "the secret store needs an encryption keyring — set Tier A "
                "`secrets.keys` in config.yaml (generate one with "
                "`crewlet secrets keygen`). Secrets are never stored "
                "unencrypted."
            )
        return self._cipher

    async def put(
        self,
        name: str,
        value: str,
        *,
        updated_by: str,
        source: str,
    ) -> None:
        """Seal ``value`` under ``name``, inserting or replacing the row."""
        if not is_env_var_name(name):
            raise SecretStoreError(
                f"{name!r} is not a valid environment-variable name, so no "
                f"${{...}} reference could ever read it "
                f"(expected [A-Za-z_][A-Za-z0-9_]*)"
            )
        cipher = self._require_cipher()
        sealed = cipher.encrypt(value, aad=_aad(name))
        await self._db.execute(
            """
            INSERT INTO secret_values (name, value, key_id, updated_by, source)
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (name) DO UPDATE
               SET value = EXCLUDED.value,
                   key_id = EXCLUDED.key_id,
                   updated_at = now(),
                   updated_by = EXCLUDED.updated_by,
                   source = EXCLUDED.source
            """,
            name,
            sealed,
            parse_key_id(sealed) or "",
            updated_by,
            source,
        )
        # Name and provenance only — the value never reaches a log line.
        logger.info("secret_value_stored", name=name, source=source)

    async def get(self, name: str) -> str | None:
        """The decrypted value for ``name``, or ``None`` when not stored."""
        row = await self._db.fetchrow(
            "SELECT value FROM secret_values WHERE name = $1", name
        )
        if row is None:
            return None
        cipher = self._require_cipher()
        return cipher.decrypt(row["value"], aad=_aad(name))

    async def delete(self, name: str) -> bool:
        """Remove ``name``. Returns whether a row was actually removed."""
        rows = await self._db.execute(
            "DELETE FROM secret_values WHERE name = $1 RETURNING name", name
        )
        removed = bool(rows)
        if removed:
            logger.info("secret_value_deleted", name=name)
        return removed

    async def list_records(self) -> list[SecretValueRecord]:
        """Metadata for every stored secret, newest write first.

        Needs no keyring — an operator locked out of the key can still see
        what is stored and under which ``key_id``, which is exactly the
        inventory a key-loss postmortem needs.
        """
        rows = await self._db.execute(
            """
            SELECT name, key_id, updated_at, updated_by, source
            FROM secret_values
            ORDER BY updated_at DESC
            """
        )
        return [_row_to_record(row) for row in rows]

    async def load_all(self) -> dict[str, str]:
        """Every stored secret, decrypted — the process snapshot.

        Returns ``{}`` for an empty table even with no keyring, so a
        deployment that has never used the store stays inert.  Rows with
        no keyring raise instead: answering ``""`` for a name someone
        deliberately stored is the failure mode this store exists to
        prevent.
        """
        rows = await self._db.execute("SELECT name, value FROM secret_values")
        if not rows:
            return {}
        cipher = self._require_cipher()
        return {
            row["name"]: cipher.decrypt(row["value"], aad=_aad(row["name"]))
            for row in rows
        }

    async def rekey(self) -> list[str]:
        """Re-seal every row not already under the active key.

        The per-row counterpart of ``crewlet config rekey``: without it a
        retired key could never be dropped from the keyring, because rows
        sealed under it would become unreadable.  Returns the names
        re-sealed.
        """
        cipher = self._require_cipher()
        active = getattr(cipher, "active_key_id", "")
        rows = await self._db.execute("SELECT name, value, key_id FROM secret_values")
        rotated: list[str] = []
        for row in rows:
            if row["key_id"] == active:
                continue
            name = row["name"]
            plaintext = cipher.decrypt(row["value"], aad=_aad(name))
            sealed = cipher.encrypt(plaintext, aad=_aad(name))
            await self._db.execute(
                """
                UPDATE secret_values
                   SET value = $2, key_id = $3, updated_at = now()
                 WHERE name = $1
                """,
                name,
                sealed,
                parse_key_id(sealed) or "",
            )
            rotated.append(name)
        if rotated:
            logger.info("secret_values_rekeyed", names=rotated, key_id=active)
        return rotated


class DatabaseSecretSource:
    """A refreshable in-memory snapshot of :class:`SecretValueStore`.

    Implements :class:`crewlet.secrets.resolver.SecretSource`.  Resolution
    is synchronous at ~25 scattered call sites, so the values are read
    once into a dict and swapped wholesale on refresh — never queried
    per lookup.

    The swap is atomic (one rebind of ``_values``), so a refresh racing a
    resolution yields either the old map or the new one, never a
    half-updated one.
    """

    def __init__(self, store: SecretValueStore) -> None:
        self._store = store
        self._values: dict[str, str] = {}

    def get(self, name: str) -> str | None:
        return self._values.get(name)

    def names(self) -> list[str]:
        """Stored names — used to report what the store shadows in env."""
        return sorted(self._values)

    async def refresh(self) -> None:
        """Re-read the table and swap the snapshot in."""
        loaded = await self._store.load_all()
        previous = set(self._values)
        self._values = loaded
        if set(loaded) != previous:
            logger.info("secret_snapshot_refreshed", count=len(loaded))


async def load_secret_source(
    db: Database, cipher: SecretCipher | None
) -> DatabaseSecretSource | None:
    """Build and prime a snapshot, or ``None`` when the store is unused.

    ``None`` for an empty table keeps every existing deployment on the
    exact ``os.environ`` path it has today — the feature engages only once
    a secret is actually stored.  A populated table with no keyring raises
    :class:`SecretStoreError` rather than booting with secrets it cannot
    read.
    """
    source = DatabaseSecretSource(SecretValueStore(db, cipher))
    await source.refresh()
    if not source.names():
        return None
    logger.info("secret_store_loaded", count=len(source.names()))
    return source


__all__ = [
    "DatabaseSecretSource",
    "SecretStoreError",
    "SecretValueRecord",
    "SecretValueStore",
    "load_secret_source",
]
