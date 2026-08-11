"""Handlers for the ``crewlet config`` subcommand tree.

Each handler is a thin sync wrapper around an async core that:

1. Resolves the DB DSN (from ``--dsn`` or the Tier A YAML).
2. Connects to PostgreSQL, runs migrations.
3. Performs the per-subcommand work via :class:`CompanyConfigStore`.
4. Closes the DB cleanly.

Kept in a separate module so ``cli.py`` stays focused on argparse
wiring and the long-lived ``run`` / ``api`` commands.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import sys
from pathlib import Path
from typing import Any
from uuid import UUID

from crewlet._logging import get_logger

logger = get_logger("cli.config")


# ── DSN resolution ────────────────────────────────────────────────


def _resolve_dsn(args: argparse.Namespace) -> str:
    """Return the DB DSN from ``--dsn`` or ``--bootstrap``."""
    if getattr(args, "dsn", None):
        return args.dsn
    from crewlet.config import load_bootstrap_config

    bootstrap_path: Path = args.bootstrap
    if not bootstrap_path.exists():
        raise FileNotFoundError(
            f"Bootstrap config not found: {bootstrap_path}. "
            f"Pass --dsn or --bootstrap to override."
        )
    bootstrap = load_bootstrap_config(bootstrap_path)
    dsn = bootstrap.providers.database.dsn
    if not dsn:
        raise RuntimeError(
            "providers.database.dsn missing from bootstrap config "
            "(or pass --dsn explicitly)."
        )
    return dsn


async def _connect(args: argparse.Namespace) -> Any:
    """Open the DB.  Returns the open Database (no migrations)."""
    from crewlet.db.client import Database

    dsn = _resolve_dsn(args)
    return await Database.connect(dsn)


def _resolve_cipher(args: argparse.Namespace) -> Any:
    """Build the secret-encryption keyring from the Tier A bootstrap.

    Returns ``None`` when no bootstrap file is available (e.g. a bare
    ``--dsn`` invocation) or Tier A ``secrets`` is unset — the caller
    then stores payloads unsealed (plaintext ``${VAR}`` mode).
    """
    from crewlet.config import load_bootstrap_config
    from crewlet.secrets import KeyringCipher

    bootstrap_path = getattr(args, "bootstrap", None)
    if bootstrap_path is None or not Path(bootstrap_path).exists():
        return None
    bootstrap = load_bootstrap_config(bootstrap_path)
    return KeyringCipher.from_config(bootstrap.secrets)


async def _connect_and_migrate(
    args: argparse.Namespace, company: Any | None = None
) -> Any:
    """Open the DB and run migrations.  Returns the open Database.

    Only ``crewlet config import`` should ever call this: it's the
    one command that legitimately bootstraps a fresh DB.  Read paths
    (``export`` / ``show`` / ``revisions`` / ``diff``) use
    :func:`_connect` and let the missing-table error surface — they
    should not invent the schema as a side effect.

    ``company`` is the :class:`CompanyConfig` being imported.  Its
    embedding dimensions size the pgvector columns the migrations
    create — a non-1536 model otherwise mismatches the ``vector(N)``
    column width.  Falls back to 1536 only when no config is supplied.
    """
    from crewlet.db.migrator import migrate

    db = await _connect(args)
    dims = "1536"
    if company is not None and company.providers.embeddings is not None:
        dims = str(company.providers.embeddings.dimensions)
    await migrate(db, template_vars={"embedding_dimensions": dims})
    return db


# ── import ─────────────────────────────────────────────────────────


def cmd_config_import(args: argparse.Namespace) -> int:
    """Load a Tier B YAML and write it as the active revision.

    Accepts a plaintext ``company.yaml`` or an encrypted-document export
    (from ``crewlet config export``) — the latter is decrypted with the
    configured keyring first. Either way the payload is re-encrypted under
    the Tier A keyring before storage (or stored plaintext when no keyring
    is configured).
    """
    import yaml

    from crewlet.config import CompanyConfig, load_company_config
    from crewlet.config_yaml import company_config_to_dict
    from crewlet.db.company_config import CompanyConfigStore
    from crewlet.secrets import is_encrypted_document, load_config

    company_path: Path = args.company_config
    if not company_path.exists():
        print(f"Error: company config not found: {company_path}", file=sys.stderr)
        return 1

    raw = yaml.safe_load(company_path.read_text())
    if is_encrypted_document(raw):
        # Re-importing an encrypted-document export: decrypt with the keyring.
        decrypt_cipher = _resolve_cipher(args)
        if decrypt_cipher is None:
            print(
                "Error: file is a fully-encrypted config export but no keyring "
                "is configured — set Tier A `secrets.keys` to import it.",
                file=sys.stderr,
            )
            return 1
        try:
            company = CompanyConfig.model_validate(load_config(raw, decrypt_cipher))
        except Exception as exc:
            print(f"Error: could not decrypt/validate export: {exc}", file=sys.stderr)
            return 1
    else:
        try:
            company = load_company_config(company_path)
        except Exception as exc:
            print(f"Error: invalid company config: {exc}", file=sys.stderr)
            return 1

    payload = company_config_to_dict(company)

    if args.dry_run:
        print(f"Valid Tier B company config: {company.name}")
        print(f"  Roles      : {len(company.roles)}")
        print(f"  Units      : {len(company.units)}")
        print(f"  Providers  : {', '.join(company.providers.llm) or '(none)'}")
        if company.mcp_servers:
            print(f"  MCP servers: {len(company.mcp_servers)}")
        print("(dry-run — nothing written to DB)")
        return 0

    from crewlet.secrets import SecretLeakError, store_config

    cipher = _resolve_cipher(args)

    async def _run() -> int:
        db = await _connect_and_migrate(args, company=company)
        try:
            store = CompanyConfigStore(db)
            existing = await store.get_active()
            if existing is not None and not args.force:
                print(
                    "Error: an active revision already exists "
                    f"({existing.revision_id}). Pass --force to overwrite.",
                    file=sys.stderr,
                )
                return 1
            parent = existing.revision_id if existing is not None else None
            summary = args.summary or "cli import"
            try:
                to_store = store_config(payload, cipher)
            except SecretLeakError as exc:
                print(f"Error: {exc}", file=sys.stderr)
                return 1
            rev_id = await store.insert_active(
                to_store,
                created_by="cli",
                source="cli",
                summary=summary,
                parent_revision_id=parent,
            )
            print(f"Activated revision {rev_id}")
            return 0
        finally:
            await db.close()

    try:
        return asyncio.run(_run())
    except (RuntimeError, FileNotFoundError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


# ── export ─────────────────────────────────────────────────────────


def cmd_config_export(args: argparse.Namespace) -> int:
    """Dump the active (or specified) revision as YAML to stdout.

    By default emits the payload verbatim — a plaintext ``${VAR}`` config
    when no keyring is configured, or the inert
    ``{"__encrypted__": "enc:v1:…"}`` document blob when encrypted. The
    ciphertext form is DR-friendly and round-trippable (re-importing
    decrypts and re-stores it). ``--redact`` decrypts the structure but
    masks every secret to ``{"encrypted": true, ...}`` for a share-safe
    dump.
    """
    import yaml

    from crewlet.db.company_config import CompanyConfigStore

    async def _run() -> int:
        db = await _connect(args)
        try:
            store = CompanyConfigStore(db)
            if args.revision:
                try:
                    rev_uuid = UUID(args.revision)
                except ValueError:
                    print(
                        f"Error: invalid revision UUID: {args.revision}",
                        file=sys.stderr,
                    )
                    return 1
                rev = await store.get_revision(rev_uuid)
            else:
                rev = await store.get_active()
            if rev is None:
                print(
                    "Error: no revision found"
                    + (f" with id {args.revision}" if args.revision else " (active)"),
                    file=sys.stderr,
                )
                return 1
            if getattr(args, "redact", False):
                from crewlet.secrets import redact_config

                dumped = redact_config(rev.payload, _resolve_cipher(args))
            else:
                # Emit the stored payload verbatim: a plaintext ${VAR}
                # config when unencrypted, or the inert __encrypted__
                # document blob otherwise. Round-trippable via
                # `crewlet config import`.
                dumped = rev.payload
            # ``width=inf`` disables line-folding: a long ``enc:v1:`` base64
            # scalar folded across lines re-parses as a broken mapping (the
            # value carries colons), silently breaking the round-trip.
            sys.stdout.write(
                yaml.safe_dump(
                    dumped,
                    sort_keys=False,
                    default_flow_style=False,
                    width=float("inf"),
                )
            )
            return 0
        finally:
            await db.close()

    try:
        return asyncio.run(_run())
    except (RuntimeError, FileNotFoundError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


# ── show ───────────────────────────────────────────────────────────


def cmd_config_show(args: argparse.Namespace) -> int:
    """Print a one-line summary of the active revision."""
    from crewlet.db.company_config import CompanyConfigStore

    async def _run() -> int:
        db = await _connect(args)
        try:
            store = CompanyConfigStore(db)
            rev = await store.get_active()
            if rev is None:
                print("No active revision (engine is unconfigured).")
                return 0
            from crewlet.secrets import load_config

            # Decrypt the document so the company name is readable
            # (plaintext payloads pass through).
            name = load_config(rev.payload, _resolve_cipher(args)).get(
                "name", "(unnamed)"
            )
            print(f"Revision   : {rev.revision_id}")
            print(f"Activated  : {rev.activated_at or rev.created_at}")
            print(f"Created by : {rev.created_by}")
            print(f"Source     : {rev.source}")
            print(f"Summary    : {rev.summary}")
            print(f"Company    : {name}")
            return 0
        finally:
            await db.close()

    try:
        return asyncio.run(_run())
    except (RuntimeError, FileNotFoundError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


# ── revisions ─────────────────────────────────────────────────────


def cmd_config_revisions(args: argparse.Namespace) -> int:
    """List recent revisions (newest first)."""
    from crewlet.db.company_config import CompanyConfigStore

    async def _run() -> int:
        db = await _connect(args)
        try:
            store = CompanyConfigStore(db)
            rows = await store.list_revisions(limit=args.limit)
            if not rows:
                print("No revisions yet.")
                return 0
            for r in rows:
                marker = "*" if r.is_active else " "
                ts = (r.activated_at or r.created_at).strftime("%Y-%m-%d %H:%M:%SZ")
                print(
                    f"{marker} {r.revision_id}  "
                    f"{ts}  by={r.created_by}  "
                    f"source={r.source}  {r.summary}"
                )
            return 0
        finally:
            await db.close()

    try:
        return asyncio.run(_run())
    except (RuntimeError, FileNotFoundError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


# ── diff ─────────────────────────────────────────────────────────


def _structural_diff(
    old: dict[str, Any], new: dict[str, Any], path: str = ""
) -> list[str]:
    """Return a flat list of "+ path : value", "- path", "~ path : old → new"
    lines.  A deliberately simple structural diff — the API's
    ``structural_diff`` provides the richer form.
    """
    lines: list[str] = []
    old_keys = set(old) if isinstance(old, dict) else set()
    new_keys = set(new) if isinstance(new, dict) else set()
    for key in sorted(old_keys | new_keys):
        sub_path = f"{path}.{key}" if path else key
        in_old, in_new = key in old_keys, key in new_keys
        if in_old and not in_new:
            lines.append(f"- {sub_path}")
        elif in_new and not in_old:
            v = json.dumps(new[key], default=str)
            lines.append(f"+ {sub_path} : {v}")
        else:
            ov, nv = old[key], new[key]
            if isinstance(ov, dict) and isinstance(nv, dict):
                lines.extend(_structural_diff(ov, nv, sub_path))
            elif ov != nv:
                lines.append(
                    f"~ {sub_path} : "
                    f"{json.dumps(ov, default=str)} → "
                    f"{json.dumps(nv, default=str)}"
                )
    return lines


def cmd_config_diff(args: argparse.Namespace) -> int:
    """Print a structural diff between two revisions."""
    from crewlet.db.company_config import CompanyConfigStore

    try:
        target_uuid = UUID(args.revision)
    except ValueError:
        print(f"Error: invalid revision UUID: {args.revision}", file=sys.stderr)
        return 1

    async def _run() -> int:
        db = await _connect(args)
        try:
            store = CompanyConfigStore(db)
            target = await store.get_revision(target_uuid)
            if target is None:
                print(f"Error: revision {target_uuid} not found", file=sys.stderr)
                return 1

            if args.against == "active":
                against = await store.get_active()
                if against is None:
                    print(
                        "Error: no active revision to diff against; "
                        "pass --against <uuid>",
                        file=sys.stderr,
                    )
                    return 1
            else:
                try:
                    base_uuid = UUID(args.against)
                except ValueError:
                    print(
                        f"Error: invalid --against UUID: {args.against}",
                        file=sys.stderr,
                    )
                    return 1
                against = await store.get_revision(base_uuid)
                if against is None:
                    print(
                        f"Error: revision {base_uuid} not found",
                        file=sys.stderr,
                    )
                    return 1

            from crewlet.secrets import redact_config

            # Diff over display-safe views: decrypt each side's structure
            # but keep every secret masked — never leak ciphertext or
            # plaintext secrets into the diff.
            cipher = _resolve_cipher(args)
            print(f"Diff: {against.revision_id} → {target.revision_id}")
            diff_lines = _structural_diff(
                redact_config(against.payload, cipher),
                redact_config(target.payload, cipher),
            )
            if not diff_lines:
                print("(no changes)")
            for line in diff_lines:
                print(line)
            return 0
        finally:
            await db.close()

    try:
        return asyncio.run(_run())
    except (RuntimeError, FileNotFoundError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


# ── seal (migration) ──────────────────────────────────────────────


def cmd_config_seal(args: argparse.Namespace) -> int:
    """Encrypt the active revision under the Tier A keyring.

    The one-time migration off plaintext-at-rest: reads the active
    revision and writes a new active revision holding the whole config as
    one opaque ``{"__encrypted__": …}`` blob. Idempotent when the active
    revision is already a document under the active key.
    """
    from crewlet.db.company_config import CompanyConfigStore
    from crewlet.secrets import (
        DOCUMENT_WRAPPER_KEY,
        SecretDecryptError,
        is_encrypted_document,
        load_config,
        parse_key_id,
    )
    from crewlet.secrets.document import encrypt_document

    cipher = _resolve_cipher(args)
    if cipher is None:
        print(
            "Error: no encryption keyring configured. Set Tier A "
            "`secrets.keys` in config.yaml (generate one with "
            "`crewlet secrets keygen`).",
            file=sys.stderr,
        )
        return 1

    async def _run() -> int:
        db = await _connect(args)
        try:
            store = CompanyConfigStore(db)
            active = await store.get_active()
            if active is None:
                print("No active revision to seal.")
                return 0
            # Wrap the whole plaintext config. Idempotent when the active
            # revision is already a document under the active key.
            if (
                is_encrypted_document(active.payload)
                and parse_key_id(active.payload[DOCUMENT_WRAPPER_KEY])
                == cipher.active_key_id
            ):
                print("Active revision already sealed — nothing to do.")
                return 0
            try:
                sealed = encrypt_document(load_config(active.payload, cipher), cipher)
            except SecretDecryptError as exc:
                print(
                    f"Error: {exc}\n"
                    "The active revision is sealed under a key no longer in "
                    "the keyring — keep the old key in config.yaml "
                    "`secrets.keys` so it can be decrypted.",
                    file=sys.stderr,
                )
                return 1
            rev_id = await store.insert_active(
                sealed,
                created_by="cli",
                source="cli.seal",
                summary=args.summary or "seal secrets at rest",
                parent_revision_id=active.revision_id,
            )
            print(f"Sealed secrets into new revision {rev_id}")
            return 0
        finally:
            await db.close()

    try:
        return asyncio.run(_run())
    except (RuntimeError, FileNotFoundError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


# ── rekey (master-key rotation) ───────────────────────────────────


def cmd_config_rekey(args: argparse.Namespace) -> int:
    """Re-encrypt the active revision under the active key.

    Master-key rotation. Set the new key as Tier A ``secrets.active_key_id``
    while keeping the old key in ``secrets.keys`` (so its ciphertext still
    decrypts), then run this: the config document is decrypted with whatever
    key sealed it and re-encrypted under the new active key, written as a new
    revision. Once it succeeds you can drop the old key from ``config.yaml``.
    ``--dry-run`` reports whether it would re-encrypt without writing.
    """
    from crewlet.db.company_config import CompanyConfigStore
    from crewlet.secrets import (
        SecretDecryptError,
        is_encrypted_document,
        rekey_config,
    )

    cipher = _resolve_cipher(args)
    if cipher is None:
        print(
            "Error: no encryption keyring configured. Set Tier A "
            "`secrets.keys` in config.yaml (generate one with "
            "`crewlet secrets keygen`).",
            file=sys.stderr,
        )
        return 1

    async def _run() -> int:
        db = await _connect(args)
        try:
            store = CompanyConfigStore(db)
            active = await store.get_active()
            if active is None:
                print("No active revision to rekey.")
                return 0
            if not is_encrypted_document(active.payload):
                # A keyring is configured but the active revision was never
                # sealed — rekey has nothing to rotate. Point at `seal`
                # rather than the misleading "already under the active key".
                print(
                    "Active revision is stored in plaintext (not encrypted) — "
                    "run `crewlet config seal` first to encrypt it, then rekey "
                    "to rotate the key.",
                    file=sys.stderr,
                )
                return 1
            try:
                # Re-encrypt the whole document blob under the active key.
                rekeyed, changed = rekey_config(
                    active.payload, cipher, cipher.active_key_id
                )
            except SecretDecryptError as exc:
                print(
                    f"Error: {exc}\n"
                    "A secret is sealed under a key that is no longer in the "
                    "keyring — keep the old key in config.yaml `secrets.keys` "
                    "until the rekey completes.",
                    file=sys.stderr,
                )
                return 1
            if not changed:
                print(
                    "Config is already under the active key "
                    f"({cipher.active_key_id!r}) — nothing to rekey."
                )
                return 0
            if args.dry_run:
                print(
                    f"Would re-encrypt the config document under "
                    f"'{cipher.active_key_id}'."
                )
                print("(dry-run — nothing written to DB)")
                return 0
            rev_id = await store.insert_active(
                rekeyed,
                created_by="cli",
                source="cli.rekey",
                summary=args.summary or f"rekey config → {cipher.active_key_id}",
                parent_revision_id=active.revision_id,
            )
            print(
                f"Re-encrypted the config document under "
                f"'{cipher.active_key_id}' in new revision {rev_id}"
            )
            return 0
        finally:
            await db.close()

    try:
        return asyncio.run(_run())
    except (RuntimeError, FileNotFoundError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


# ── secrets keygen ────────────────────────────────────────────────


def cmd_secrets_keygen(args: argparse.Namespace) -> int:
    """Print a fresh keyring key plus the Tier A snippet to install it."""
    from crewlet.secrets import keygen_snippet

    sys.stdout.write(keygen_snippet(args.key_id))
    return 0


# ── secrets store (set / list / unset / get / rekey) ───────────────


def _secret_store(args: argparse.Namespace, db: Any) -> Any:
    """Build a :class:`SecretValueStore` over an open DB handle."""
    from crewlet.db.secret_values import SecretValueStore

    return SecretValueStore(db, _resolve_cipher(args))


def _read_secret_value(args: argparse.Namespace) -> str:
    """The value for ``secrets set`` — flag, stdin, or an interactive prompt.

    Reading from stdin is the default because an argv value is visible in
    ``ps`` output and lands in shell history; ``--value`` stays available
    for scripted use where the caller has already accepted that.
    """
    if args.value is not None:
        return args.value
    if not sys.stdin.isatty():
        # Strip exactly one trailing newline — `echo secret | crewlet ...`
        # must not store the newline, but a secret whose real value ends
        # in whitespace has to survive.
        data = sys.stdin.read()
        return data[:-1] if data.endswith("\n") else data
    import getpass

    return getpass.getpass("Secret value: ")


def _run_secret_command(args: argparse.Namespace, body: Any) -> int:
    """Shared connect / dispatch / close wrapper for the store commands."""
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
    except (SecretStoreError, SecretCipherError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1
    except (RuntimeError, FileNotFoundError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


def cmd_secrets_set(args: argparse.Namespace) -> int:
    """Store one encrypted secret, keyed by environment-variable name."""
    value = _read_secret_value(args)

    async def _body(store: Any) -> int:
        await store.put(
            args.name, value, updated_by="cli", source=str(args.source or "cli")
        )
        print(f"Stored {args.name} (encrypted).")
        print(
            "A running engine picks this up on its next config activation "
            "or restart; a fresh `crewlet run` reads it at boot."
        )
        return 0

    return _run_secret_command(args, _body)


def cmd_secrets_list(args: argparse.Namespace) -> int:
    """List stored secret names and metadata — never values."""

    async def _body(store: Any) -> int:
        records = await store.list_records()
        if not records:
            print("No secrets stored.")
            return 0
        width = max(len(r.name) for r in records)
        print(f"{'NAME'.ljust(width)}  KEY_ID      UPDATED              SOURCE")
        for r in records:
            stamp = r.updated_at.strftime("%Y-%m-%d %H:%M:%S")
            print(f"{r.name.ljust(width)}  {r.key_id:<10}  {stamp}  {r.source}")
        return 0

    return _run_secret_command(args, _body)


def cmd_secrets_unset(args: argparse.Namespace) -> int:
    """Remove one stored secret."""

    async def _body(store: Any) -> int:
        if await store.delete(args.name):
            print(f"Removed {args.name}.")
            return 0
        print(f"Error: {args.name} is not stored.", file=sys.stderr)
        return 1

    return _run_secret_command(args, _body)


def cmd_secrets_get(args: argparse.Namespace) -> int:
    """Print one stored secret value. Deliberately awkward to invoke.

    There is no HTTP route that returns a secret value; this CLI path is
    the only read-back, it requires an explicit ``--reveal``, and it logs
    the access (name only). Break-glass for recovering a credential the
    upstream API will never show again — not a scripting interface.
    """
    if not args.reveal:
        print(
            "Error: refusing to print a secret without --reveal. "
            "Values never appear in `secrets list`, the API, or the "
            "dashboard; pass --reveal to confirm you want plaintext on "
            "stdout.",
            file=sys.stderr,
        )
        return 1

    async def _body(store: Any) -> int:
        value = await store.get(args.name)
        if value is None:
            print(f"Error: {args.name} is not stored.", file=sys.stderr)
            return 1
        logger.warning("secret_value_revealed", name=args.name)
        sys.stdout.write(value + "\n")
        return 0

    return _run_secret_command(args, _body)


def cmd_secrets_rekey(args: argparse.Namespace) -> int:
    """Re-encrypt stored secrets under the active keyring key.

    The per-row counterpart of ``crewlet config rekey``. Without it a
    retired key can never leave the keyring, because rows still sealed
    under it would become unreadable.
    """

    async def _body(store: Any) -> int:
        if not store.enabled:
            print(
                "Error: no encryption keyring configured. Set Tier A "
                "`secrets.keys` in config.yaml (generate one with "
                "`crewlet secrets keygen`).",
                file=sys.stderr,
            )
            return 1
        if args.dry_run:
            cipher = _resolve_cipher(args)
            active = getattr(cipher, "active_key_id", "")
            stale = [r.name for r in await store.list_records() if r.key_id != active]
            if not stale:
                print(f"All stored secrets are already sealed under {active!r}.")
                return 0
            print(f"Would re-encrypt under {active!r}:")
            for name in stale:
                print(f"  {name}")
            return 0
        rotated = await store.rekey()
        if not rotated:
            print("No secrets needed re-encryption.")
            return 0
        print(f"Re-encrypted {len(rotated)} secret(s): {', '.join(rotated)}")
        return 0

    return _run_secret_command(args, _body)
