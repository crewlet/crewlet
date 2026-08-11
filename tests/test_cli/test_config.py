"""Tests for the ``crewlet config`` subcommand tree.

Each test stubs the asyncpg-backed Database + CompanyConfigStore so the
handlers can be exercised end-to-end without a real PostgreSQL.
"""

from __future__ import annotations

from pathlib import Path
from unittest.mock import AsyncMock, MagicMock
from uuid import uuid4

import pytest

from crewlet.cli import main


@pytest.fixture
def bootstrap_yaml(tmp_path: Path) -> Path:
    """A minimal Tier A bootstrap YAML pointing at a fake DSN."""
    path = tmp_path / "config.yaml"
    path.write_text(
        "providers:\n  database:\n    dsn: postgresql://test@localhost/crewlet\n",
    )
    return path


@pytest.fixture
def company_yaml(tmp_path: Path) -> Path:
    """A minimal Tier B company YAML."""
    path = tmp_path / "company.yaml"
    path.write_text(
        "name: Test Co\nmission: build cool stuff\n",
    )
    return path


@pytest.fixture
def mock_db(monkeypatch: pytest.MonkeyPatch):
    """Patch Database.connect + migrate so handlers run against mocks."""
    db = AsyncMock()
    db.close = AsyncMock()
    db._require_pool = MagicMock()
    # A bare AsyncMock returns a truthy mock for every query, which the
    # secret-store snapshot load would read as "rows exist". Empty is the
    # honest default for a handler test that stores no secrets.
    db.execute = AsyncMock(return_value=[])

    async def _connect(_dsn):
        return db

    async def _migrate(_db, *, template_vars=None, only=None):
        return None

    monkeypatch.setattr("crewlet.db.client.Database.connect", _connect)
    monkeypatch.setattr("crewlet.db.migrator.migrate", _migrate)
    return db


@pytest.fixture
def patch_store(monkeypatch: pytest.MonkeyPatch):
    """Patch the CompanyConfigStore constructor to return a mock."""
    store_instances = []

    def _maker():
        s = MagicMock()
        s.insert_active = AsyncMock(return_value=uuid4())
        s.get_active = AsyncMock(return_value=None)
        s.get_revision = AsyncMock(return_value=None)
        s.list_revisions = AsyncMock(return_value=[])
        store_instances.append(s)
        return s

    def _patch_constructor(_db):
        return _maker()

    monkeypatch.setattr(
        "crewlet.db.company_config.CompanyConfigStore", _patch_constructor
    )
    return store_instances


# -- import ----------------------------------------------------------


def test_config_import_dry_run(
    bootstrap_yaml: Path,
    company_yaml: Path,
    capsys: pytest.CaptureFixture[str],
) -> None:
    rc = main(
        [
            "config",
            "import",
            str(company_yaml),
            "--bootstrap",
            str(bootstrap_yaml),
            "--dry-run",
        ]
    )
    assert rc == 0
    out = capsys.readouterr().out
    assert "Test Co" in out
    assert "dry-run" in out


def test_config_import_unconfigured_creates_first_revision(
    bootstrap_yaml: Path,
    company_yaml: Path,
    mock_db,  # noqa: ARG001
    patch_store,
    capsys: pytest.CaptureFixture[str],
) -> None:
    rc = main(
        [
            "config",
            "import",
            str(company_yaml),
            "--bootstrap",
            str(bootstrap_yaml),
        ]
    )
    assert rc == 0
    assert len(patch_store) == 1
    store = patch_store[0]
    store.insert_active.assert_called_once()
    insert_call = store.insert_active.call_args
    assert insert_call.kwargs["created_by"] == "cli"
    assert insert_call.kwargs["source"] == "cli"
    assert insert_call.kwargs["parent_revision_id"] is None
    assert "Activated revision" in capsys.readouterr().out


def test_config_import_refuses_when_active_without_force(
    bootstrap_yaml: Path,
    company_yaml: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from crewlet.db.company_config import CompanyConfigRevision

    existing_id = uuid4()
    existing = CompanyConfigRevision(
        revision_id=existing_id,
        parent_revision_id=None,
        created_at=__import__("datetime").datetime.now(__import__("datetime").UTC),
        created_by="x",
        source="cli",
        summary="prior",
        payload={"name": "Prior"},
        is_active=True,
        activated_at=None,
    )

    store = MagicMock()
    store.get_active = AsyncMock(return_value=existing)
    store.insert_active = AsyncMock()

    monkeypatch.setattr(
        "crewlet.db.company_config.CompanyConfigStore",
        lambda _db: store,
    )

    rc = main(
        [
            "config",
            "import",
            str(company_yaml),
            "--bootstrap",
            str(bootstrap_yaml),
        ]
    )
    assert rc == 1
    err = capsys.readouterr().err
    assert "already exists" in err
    store.insert_active.assert_not_called()


def test_config_import_force_overrides_active(
    bootstrap_yaml: Path,
    company_yaml: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from crewlet.db.company_config import CompanyConfigRevision

    existing_id = uuid4()
    existing = CompanyConfigRevision(
        revision_id=existing_id,
        parent_revision_id=None,
        created_at=__import__("datetime").datetime.now(__import__("datetime").UTC),
        created_by="x",
        source="cli",
        summary="prior",
        payload={"name": "Prior"},
        is_active=True,
        activated_at=None,
    )

    new_id = uuid4()
    store = MagicMock()
    store.get_active = AsyncMock(return_value=existing)
    store.insert_active = AsyncMock(return_value=new_id)

    monkeypatch.setattr(
        "crewlet.db.company_config.CompanyConfigStore",
        lambda _db: store,
    )

    rc = main(
        [
            "config",
            "import",
            str(company_yaml),
            "--bootstrap",
            str(bootstrap_yaml),
            "--force",
            "--summary",
            "promotion test",
        ]
    )
    assert rc == 0
    insert_call = store.insert_active.call_args
    assert insert_call.kwargs["parent_revision_id"] == existing_id
    assert insert_call.kwargs["summary"] == "promotion test"
    assert "Activated revision" in capsys.readouterr().out


# -- show ------------------------------------------------------------


def test_config_show_unconfigured(
    bootstrap_yaml: Path,
    mock_db,  # noqa: ARG001
    patch_store,  # noqa: ARG001
    capsys: pytest.CaptureFixture[str],
) -> None:
    rc = main(["config", "show", "--bootstrap", str(bootstrap_yaml)])
    assert rc == 0
    assert "unconfigured" in capsys.readouterr().out


# -- revisions -------------------------------------------------------


def test_config_revisions_empty(
    bootstrap_yaml: Path,
    mock_db,  # noqa: ARG001
    patch_store,  # noqa: ARG001
    capsys: pytest.CaptureFixture[str],
) -> None:
    rc = main(["config", "revisions", "--bootstrap", str(bootstrap_yaml)])
    assert rc == 0
    assert "No revisions yet" in capsys.readouterr().out


# -- export ----------------------------------------------------------


def test_config_export_no_active(
    bootstrap_yaml: Path,
    mock_db,  # noqa: ARG001
    patch_store,  # noqa: ARG001
    capsys: pytest.CaptureFixture[str],
) -> None:
    rc = main(["config", "export", "--bootstrap", str(bootstrap_yaml)])
    assert rc == 1
    assert "no revision" in capsys.readouterr().err


def test_config_export_dumps_active_as_yaml(
    bootstrap_yaml: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from crewlet.db.company_config import CompanyConfigRevision

    rev = CompanyConfigRevision(
        revision_id=uuid4(),
        parent_revision_id=None,
        created_at=__import__("datetime").datetime.now(__import__("datetime").UTC),
        created_by="cli",
        source="cli",
        summary="x",
        payload={"name": "Acme"},
        is_active=True,
        activated_at=None,
    )

    store = MagicMock()
    store.get_active = AsyncMock(return_value=rev)
    monkeypatch.setattr(
        "crewlet.db.company_config.CompanyConfigStore",
        lambda _db: store,
    )

    rc = main(["config", "export", "--bootstrap", str(bootstrap_yaml)])
    assert rc == 0
    out = capsys.readouterr().out
    assert "name: Acme" in out


# -- diff ------------------------------------------------------------


def test_config_diff_against_active(
    bootstrap_yaml: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from datetime import UTC, datetime

    from crewlet.db.company_config import CompanyConfigRevision

    rev_a = CompanyConfigRevision(
        revision_id=uuid4(),
        parent_revision_id=None,
        created_at=datetime.now(UTC),
        created_by="cli",
        source="cli",
        summary="a",
        payload={"name": "Acme", "mission": "old"},
        is_active=True,
        activated_at=None,
    )
    rev_b = CompanyConfigRevision(
        revision_id=uuid4(),
        parent_revision_id=rev_a.revision_id,
        created_at=datetime.now(UTC),
        created_by="cli",
        source="cli",
        summary="b",
        payload={"name": "Acme", "mission": "new", "vision": "yes"},
        is_active=False,
        activated_at=None,
    )

    store = MagicMock()
    store.get_revision = AsyncMock(return_value=rev_b)
    store.get_active = AsyncMock(return_value=rev_a)
    monkeypatch.setattr(
        "crewlet.db.company_config.CompanyConfigStore",
        lambda _db: store,
    )

    rc = main(
        [
            "config",
            "diff",
            str(rev_b.revision_id),
            "--bootstrap",
            str(bootstrap_yaml),
        ]
    )
    assert rc == 0
    out = capsys.readouterr().out
    assert "mission" in out
    assert "vision" in out
    assert "old" in out
    assert "new" in out


# -- run default config path ----------------------------------------


def test_run_default_config_path(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    """``crewlet run`` with no arg defaults to ``./config.yaml``."""
    monkeypatch.chdir(tmp_path)
    # No config.yaml present → expect the missing-file error.
    rc = main(["run"])
    assert rc == 1
    err = capsys.readouterr().err
    assert "config.yaml" in err


def test_import_company_for_run_inserts_when_unconfigured(
    tmp_path: Path,
) -> None:
    """``_import_company_for_run`` writes the pre-loaded config as a
    fresh revision and returns the activated revision."""
    import asyncio
    from datetime import UTC, datetime

    from crewlet.cli import _import_company_for_run
    from crewlet.config import CompanyConfig
    from crewlet.db.company_config import CompanyConfigRevision

    company_path = tmp_path / "company.yaml"
    company = CompanyConfig(name="Acme")

    new_rev = CompanyConfigRevision(
        revision_id=uuid4(),
        parent_revision_id=None,
        created_at=datetime.now(UTC),
        created_by="cli",
        source="cli.run.--import-company",
        summary=f"bootstrap from {company_path.name}",
        payload={"name": "Acme"},
        is_active=True,
        activated_at=datetime.now(UTC),
    )

    store = MagicMock()
    store.insert_active = AsyncMock(return_value=new_rev.revision_id)
    store.get_active = AsyncMock(return_value=new_rev)

    result = asyncio.run(_import_company_for_run(store, company, company_path))

    store.insert_active.assert_awaited_once()
    payload, kwargs = (
        store.insert_active.await_args.args,
        store.insert_active.await_args.kwargs,
    )
    assert payload[0]["name"] == "Acme"
    assert kwargs["source"] == "cli.run.--import-company"
    assert kwargs["parent_revision_id"] is None
    assert result is new_rev


def test_run_import_company_missing_file_errors_when_unconfigured(
    tmp_path: Path,
    bootstrap_yaml: Path,
    mock_db,  # noqa: ARG001
    patch_store,  # noqa: ARG001 (get_active → None ⇒ unconfigured)
    capsys: pytest.CaptureFixture[str],
) -> None:
    """A missing --import-company file on an UNCONFIGURED engine errors
    cleanly (rc 1) instead of crashing — the file is loaded because it
    would actually be imported."""
    rc = main(
        [
            "run",
            str(bootstrap_yaml),
            "--import-company",
            str(tmp_path / "missing.yaml"),
        ]
    )
    assert rc == 1
    assert "not found" in capsys.readouterr().err


def test_run_import_company_ignored_when_already_configured(
    tmp_path: Path,
    bootstrap_yaml: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A configured engine must treat --import-company as a true no-op —
    a missing/invalid file is NOT loaded, so it can't break the restart.

    Drives only the connect+migrate helper (the engine spawn needs real
    infra) and asserts it returns ``import_company=None`` without raising
    despite the path not existing."""
    import asyncio
    from datetime import UTC, datetime

    from crewlet.cli import _connect_and_migrate_from_db
    from crewlet.db.company_config import CompanyConfigRevision

    active = CompanyConfigRevision(
        revision_id=uuid4(),
        parent_revision_id=None,
        created_at=datetime.now(UTC),
        created_by="x",
        source="cli",
        summary="prior",
        payload={"name": "Configured"},
        is_active=True,
        activated_at=None,
    )
    store = MagicMock()
    store.get_active = AsyncMock(return_value=active)
    monkeypatch.setattr(
        "crewlet.db.company_config.CompanyConfigStore", lambda _db: store
    )

    from crewlet.config import SecretsConfig

    bootstrap = MagicMock()
    bootstrap.providers.database.dsn = "postgresql://x/y"
    bootstrap.secrets = SecretsConfig()  # encryption disabled

    _db, _store, returned_active, import_company = asyncio.run(
        _connect_and_migrate_from_db(
            bootstrap, import_company_path=tmp_path / "missing.yaml"
        )
    )
    assert returned_active is active
    assert import_company is None  # never loaded → restart can't break


def test_connect_and_migrate_sizes_pgvector_from_import_company(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The core bug: a first-boot --import-company with a non-1536
    embedding model must size the pgvector migrations from THAT model,
    not the 1536 default (the migrations are forward-only — re-running
    after the import is a no-op)."""
    import asyncio

    from crewlet.cli import _connect_and_migrate_from_db

    company_path = tmp_path / "company.yaml"
    company_path.write_text(
        "name: Acme\n"
        "providers:\n"
        "  embeddings:\n"
        "    type: openai\n"
        "    model: text-embedding-3-large\n"
        "    dimensions: 3072\n"
    )

    migrate_calls: list[dict | None] = []

    async def _connect(_dsn):
        db = AsyncMock()
        # No stored secrets — the snapshot load must see an empty table,
        # not a truthy bare mock.
        db.execute = AsyncMock(return_value=[])
        return db

    async def _migrate(_db, *, template_vars=None, only=None):
        # Phase 1 (only=...) carries no dims; phase 2 carries the dims.
        if only is None:
            migrate_calls.append(template_vars)

    store = MagicMock()
    store.get_active = AsyncMock(return_value=None)  # unconfigured

    monkeypatch.setattr("crewlet.db.client.Database.connect", _connect)
    monkeypatch.setattr("crewlet.db.migrator.migrate", _migrate)
    monkeypatch.setattr(
        "crewlet.db.company_config.CompanyConfigStore", lambda _db: store
    )

    from crewlet.config import SecretsConfig

    bootstrap = MagicMock()
    bootstrap.providers.database.dsn = "postgresql://x/y"
    bootstrap.secrets = SecretsConfig()  # encryption disabled

    _db, _store, _active, import_company = asyncio.run(
        _connect_and_migrate_from_db(bootstrap, import_company_path=company_path)
    )

    assert migrate_calls == [{"embedding_dimensions": "3072"}]
    assert import_company is not None
    assert import_company.name == "Acme"


# -- seal / rekey ----------------------------------------------------


def _keyring_bootstrap(tmp_path: Path, keys, active: str) -> Path:
    """Write a Tier A YAML with a DSN + an encryption keyring (literal keys)."""
    lines = [
        "providers:",
        "  database:",
        "    dsn: postgresql://test@localhost/crewlet",
        "secrets:",
        f"  active_key_id: {active}",
        "  keys:",
    ]
    for kid, material in keys:
        lines.append(f"    - id: {kid}")
        lines.append(f'      material: "{material}"')
    path = tmp_path / "config.yaml"
    path.write_text("\n".join(lines) + "\n")
    return path


def _active_revision(payload: dict):
    from datetime import UTC, datetime

    from crewlet.db.company_config import CompanyConfigRevision

    return CompanyConfigRevision(
        revision_id=uuid4(),
        parent_revision_id=None,
        created_at=datetime.now(UTC),
        created_by="x",
        source="api",
        summary="s",
        payload=payload,
        is_active=True,
        activated_at=datetime.now(UTC),
    )


def _encrypted_company(cipher, api_key="sk-x") -> dict:
    """The stored form: a whole config encrypted as one opaque document."""
    from crewlet.config import CompanyConfig
    from crewlet.config_yaml import company_config_to_dict
    from crewlet.secrets.document import encrypt_document

    cfg = CompanyConfig.model_validate(
        {
            "name": "C",
            "providers": {"embeddings": {"type": "openai", "api_key": api_key}},
        }
    )
    return encrypt_document(company_config_to_dict(cfg), cipher)


def _install_store(monkeypatch, store) -> None:
    monkeypatch.setattr(
        "crewlet.db.company_config.CompanyConfigStore", lambda _db: store
    )


# -- seal (migration) -----------------------------------------------


def test_config_seal_wraps_plaintext_document(
    tmp_path: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from crewlet.secrets import KeyringCipher, is_encrypted_document, load_config
    from crewlet.secrets.cipher import decode_key_material
    from crewlet.secrets.keygen import generate_key

    mat = generate_key()
    boot = _keyring_bootstrap(tmp_path, [("k1", mat)], "k1")
    # Active revision is a plaintext (unencrypted) payload. The
    # ${VAR} reference is kept verbatim — it resolves at construction time.
    payload = {
        "name": "C",
        "providers": {"embeddings": {"type": "openai", "api_key": "${EMB_KEY}"}},
    }
    store = MagicMock()
    store.get_active = AsyncMock(return_value=_active_revision(payload))
    store.insert_active = AsyncMock(return_value=uuid4())
    _install_store(monkeypatch, store)

    rc = main(["config", "seal", "--bootstrap", str(boot)])
    assert rc == 0
    stored = store.insert_active.call_args.args[0]
    assert store.insert_active.call_args.kwargs["source"] == "cli.seal"
    # The whole document is one opaque blob at rest...
    assert is_encrypted_document(stored)
    assert "EMB_KEY" not in str(stored)
    # ...and decrypts back to the original plaintext (${VAR} intact).
    ring = KeyringCipher(keys={"k1": decode_key_material(mat)}, active_key_id="k1")
    assert load_config(stored, ring)["providers"]["embeddings"]["api_key"] == (
        "${EMB_KEY}"
    )


def test_config_seal_idempotent_when_already_sealed(
    tmp_path: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from crewlet.secrets import KeyringCipher
    from crewlet.secrets.cipher import decode_key_material
    from crewlet.secrets.keygen import generate_key

    mat = generate_key()
    boot = _keyring_bootstrap(tmp_path, [("k1", mat)], "k1")
    cipher = KeyringCipher(keys={"k1": decode_key_material(mat)}, active_key_id="k1")
    store = MagicMock()
    store.get_active = AsyncMock(
        return_value=_active_revision(_encrypted_company(cipher))
    )
    store.insert_active = AsyncMock(return_value=uuid4())
    _install_store(monkeypatch, store)

    rc = main(["config", "seal", "--bootstrap", str(boot)])
    assert rc == 0
    assert "already sealed" in capsys.readouterr().out
    store.insert_active.assert_not_called()


def test_config_seal_dropped_key_errors_cleanly(
    tmp_path: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    # Active revision sealed under an OLD key that is no longer in the
    # keyring: seal must surface a clean error, not an uncaught traceback.
    from crewlet.secrets import KeyringCipher
    from crewlet.secrets.cipher import decode_key_material
    from crewlet.secrets.keygen import generate_key

    old_mat, new_mat = generate_key(), generate_key()
    old_cipher = KeyringCipher(
        keys={"old": decode_key_material(old_mat)}, active_key_id="old"
    )
    # Bootstrap keyring holds only the NEW key — "old" was dropped.
    boot = _keyring_bootstrap(tmp_path, [("new", new_mat)], "new")
    store = MagicMock()
    store.get_active = AsyncMock(
        return_value=_active_revision(_encrypted_company(old_cipher))
    )
    store.insert_active = AsyncMock(return_value=uuid4())
    _install_store(monkeypatch, store)

    rc = main(["config", "seal", "--bootstrap", str(boot)])
    assert rc == 1
    assert "no longer in the keyring" in capsys.readouterr().err
    store.insert_active.assert_not_called()


# -- rekey (master-key rotation) ------------------------------------


def test_config_rekey_no_keyring_errors(
    bootstrap_yaml: Path,
    mock_db,  # noqa: ARG001
    capsys: pytest.CaptureFixture[str],
) -> None:
    rc = main(["config", "rekey", "--bootstrap", str(bootstrap_yaml)])
    assert rc == 1
    assert "no encryption keyring" in capsys.readouterr().err


def test_config_rekey_dry_run(
    tmp_path: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from crewlet.secrets import KeyringCipher
    from crewlet.secrets.cipher import decode_key_material
    from crewlet.secrets.keygen import generate_key

    old_mat, new_mat = generate_key(), generate_key()
    boot = _keyring_bootstrap(tmp_path, [("old", old_mat), ("new", new_mat)], "new")
    old_cipher = KeyringCipher(
        keys={"old": decode_key_material(old_mat)}, active_key_id="old"
    )
    store = MagicMock()
    store.get_active = AsyncMock(
        return_value=_active_revision(_encrypted_company(old_cipher))
    )
    store.insert_active = AsyncMock(return_value=uuid4())
    _install_store(monkeypatch, store)

    rc = main(["config", "rekey", "--bootstrap", str(boot), "--dry-run"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "config document" in out
    assert "dry-run" in out
    store.insert_active.assert_not_called()


def test_config_rekey_writes_reencrypted_revision(
    tmp_path: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from crewlet.secrets import KeyringCipher, load_config, parse_key_id
    from crewlet.secrets.cipher import decode_key_material
    from crewlet.secrets.document import DOCUMENT_WRAPPER_KEY
    from crewlet.secrets.keygen import generate_key

    old_mat, new_mat = generate_key(), generate_key()
    boot = _keyring_bootstrap(tmp_path, [("old", old_mat), ("new", new_mat)], "new")
    old_cipher = KeyringCipher(
        keys={"old": decode_key_material(old_mat)}, active_key_id="old"
    )
    store = MagicMock()
    store.get_active = AsyncMock(
        return_value=_active_revision(_encrypted_company(old_cipher))
    )
    store.insert_active = AsyncMock(return_value=uuid4())
    _install_store(monkeypatch, store)

    rc = main(["config", "rekey", "--bootstrap", str(boot)])
    assert rc == 0

    stored = store.insert_active.call_args.args[0]
    assert store.insert_active.call_args.kwargs["source"] == "cli.rekey"
    # The whole document is re-encrypted under the new active key...
    assert parse_key_id(stored[DOCUMENT_WRAPPER_KEY]) == "new"
    # ...and still decrypts to the original secret.
    ring = KeyringCipher(
        keys={
            "old": decode_key_material(old_mat),
            "new": decode_key_material(new_mat),
        },
        active_key_id="new",
    )
    assert load_config(stored, ring)["providers"]["embeddings"]["api_key"] == "sk-x"


def test_config_rekey_plaintext_revision_points_at_seal(
    tmp_path: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    # A keyring is configured but the active revision was never sealed —
    # rekey must guide the operator to `config seal`, not claim there is
    # nothing to rotate.
    from crewlet.secrets.keygen import generate_key

    boot = _keyring_bootstrap(tmp_path, [("k1", generate_key())], "k1")
    store = MagicMock()
    store.get_active = AsyncMock(
        return_value=_active_revision({"name": "C", "mission": "plaintext"})
    )
    store.insert_active = AsyncMock(return_value=uuid4())
    _install_store(monkeypatch, store)

    rc = main(["config", "rekey", "--bootstrap", str(boot)])
    assert rc == 1
    assert "config seal" in capsys.readouterr().err
    store.insert_active.assert_not_called()


# -- import / export round-trip -------------------------------------


def test_config_import_stores_encrypted_document(
    tmp_path: Path,
    mock_db,  # noqa: ARG001
    patch_store,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from crewlet.secrets import is_encrypted_document
    from crewlet.secrets.keygen import generate_key

    boot = _keyring_bootstrap(tmp_path, [("k1", generate_key())], "k1")
    company = tmp_path / "company.yaml"
    company.write_text("name: Acme\nmission: ship\n")

    rc = main(["config", "import", str(company), "--bootstrap", str(boot)])
    assert rc == 0
    stored = patch_store[0].insert_active.call_args.args[0]
    assert is_encrypted_document(stored)
    assert "Acme" not in str(stored)  # structure hidden at rest


def test_config_export_roundtrips_via_reimport(
    tmp_path: Path,
    mock_db,  # noqa: ARG001
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from crewlet.config import CompanyConfig
    from crewlet.config_yaml import company_config_to_dict
    from crewlet.secrets import KeyringCipher, is_encrypted_document, load_config
    from crewlet.secrets.cipher import decode_key_material
    from crewlet.secrets.document import encrypt_document
    from crewlet.secrets.keygen import generate_key

    mat = generate_key()
    boot = _keyring_bootstrap(tmp_path, [("k1", mat)], "k1")
    cipher = KeyringCipher(keys={"k1": decode_key_material(mat)}, active_key_id="k1")
    original = company_config_to_dict(
        CompanyConfig.model_validate({"name": "Acme", "mission": "ship"})
    )
    stored = encrypt_document(original, cipher)

    store = MagicMock()
    store.get_active = AsyncMock(return_value=_active_revision(stored))
    store.insert_active = AsyncMock(return_value=uuid4())
    _install_store(monkeypatch, store)

    # export (default) → the opaque wrapper
    rc = main(["config", "export", "--bootstrap", str(boot)])
    assert rc == 0
    exported = capsys.readouterr().out
    import yaml

    assert is_encrypted_document(yaml.safe_load(exported))

    # re-import that export → decrypts, re-stores as an encrypted document
    export_path = tmp_path / "export.yaml"
    export_path.write_text(exported)
    rc = main(
        ["config", "import", str(export_path), "--bootstrap", str(boot), "--force"]
    )
    assert rc == 0
    reimported = store.insert_active.call_args.args[0]
    assert is_encrypted_document(reimported)
    assert load_config(reimported, cipher)["name"] == "Acme"


# ---------------------------------------------------------------------------
# crewlet secrets set / list / unset / get / rekey
# ---------------------------------------------------------------------------


def _store_bootstrap(tmp_path: Path) -> Path:
    """A Tier A file carrying a real keyring, for the store commands."""
    import base64

    material = base64.b64encode(b"K" * 32).decode()
    path = tmp_path / "config.yaml"
    path.write_text(
        "providers:\n"
        "  database:\n"
        "    dsn: postgresql://x/y\n"
        "secrets:\n"
        "  active_key_id: key-1\n"
        "  keys:\n"
        "    - id: key-1\n"
        f"      material: {material}\n"
    )
    return path


class TestSecretsStoreCommands:
    """The DB-backed half of ``crewlet secrets``."""

    def test_set_stores_ciphertext(self, mock_db, tmp_path, capsys) -> None:
        from crewlet.cli import main

        bootstrap = _store_bootstrap(tmp_path)
        rc = main(
            [
                "secrets",
                "set",
                "SLACK_BOT_TOKEN_CEO",
                "--value",
                "xoxb-real",
                "--bootstrap",
                str(bootstrap),
            ]
        )
        assert rc == 0
        assert "Stored SLACK_BOT_TOKEN_CEO" in capsys.readouterr().out
        sql, *args = mock_db.execute.call_args[0]
        assert "INSERT INTO secret_values" in sql
        assert args[0] == "SLACK_BOT_TOKEN_CEO"
        assert args[1].startswith("enc:v1:key-1:")
        # The plaintext must never reach the database.
        assert "xoxb-real" not in str(mock_db.execute.call_args)

    def test_set_without_a_keyring_fails(self, mock_db, tmp_path, capsys) -> None:
        from crewlet.cli import main

        bootstrap = tmp_path / "config.yaml"
        bootstrap.write_text("providers:\n  database:\n    dsn: postgresql://x/y\n")
        rc = main(
            ["secrets", "set", "T", "--value", "v", "--bootstrap", str(bootstrap)]
        )
        assert rc == 1
        assert "keyring" in capsys.readouterr().err

    def test_set_reads_stdin_when_no_value_flag(
        self, mock_db, tmp_path, monkeypatch
    ) -> None:
        """stdin is the default so the secret stays out of argv/`ps`."""
        import io

        from crewlet.cli import main

        monkeypatch.setattr("sys.stdin", io.StringIO("piped-secret\n"))
        rc = main(
            ["secrets", "set", "T", "--bootstrap", str(_store_bootstrap(tmp_path))]
        )
        assert rc == 0
        # The single trailing newline `echo` adds must not be stored.
        sealed = mock_db.execute.call_args[0][2]
        from crewlet.config import load_bootstrap_config
        from crewlet.secrets import KeyringCipher

        cipher = KeyringCipher.from_config(
            load_bootstrap_config(_store_bootstrap(tmp_path)).secrets
        )
        assert cipher.decrypt(sealed, aad="secret_values/T") == "piped-secret"

    def test_list_shows_names_and_never_values(self, mock_db, tmp_path, capsys) -> None:
        from datetime import UTC, datetime

        from crewlet.cli import main

        mock_db.execute = AsyncMock(
            return_value=[
                {
                    "name": "GITLAB_TOKEN_SWE",
                    "key_id": "key-1",
                    "updated_at": datetime(2026, 8, 3, 12, 0, tzinfo=UTC),
                    "updated_by": "cli",
                    "source": "gitlab-provision",
                }
            ]
        )
        assert main(["secrets", "list", "--dsn", "postgresql://x/y"]) == 0
        out = capsys.readouterr().out
        assert "GITLAB_TOKEN_SWE" in out
        assert "gitlab-provision" in out
        assert "enc:v1" not in out

    def test_list_empty(self, mock_db, capsys) -> None:
        from crewlet.cli import main

        assert main(["secrets", "list", "--dsn", "postgresql://x/y"]) == 0
        assert "No secrets stored." in capsys.readouterr().out

    def test_unset_reports_missing(self, mock_db, capsys) -> None:
        from crewlet.cli import main

        assert main(["secrets", "unset", "GONE", "--dsn", "postgresql://x/y"]) == 1
        assert "not stored" in capsys.readouterr().err

    def test_unset_removes(self, mock_db, capsys) -> None:
        from crewlet.cli import main

        mock_db.execute = AsyncMock(return_value=[{"name": "T"}])
        assert main(["secrets", "unset", "T", "--dsn", "postgresql://x/y"]) == 0
        assert "Removed T." in capsys.readouterr().out

    def test_get_refuses_without_reveal(self, mock_db, capsys) -> None:
        """Printing plaintext must be an explicit, deliberate act."""
        from crewlet.cli import main

        assert main(["secrets", "get", "T", "--dsn", "postgresql://x/y"]) == 1
        err = capsys.readouterr().err
        assert "--reveal" in err
        # Nothing was even queried.
        mock_db.fetchrow.assert_not_called()

    def test_get_with_reveal_prints_the_value(self, mock_db, tmp_path, capsys) -> None:
        from crewlet.cli import main
        from crewlet.config import load_bootstrap_config
        from crewlet.secrets import KeyringCipher

        bootstrap = _store_bootstrap(tmp_path)
        cipher = KeyringCipher.from_config(load_bootstrap_config(bootstrap).secrets)
        mock_db.fetchrow = AsyncMock(
            return_value={"value": cipher.encrypt("v", aad="secret_values/T")}
        )
        rc = main(["secrets", "get", "T", "--reveal", "--bootstrap", str(bootstrap)])
        assert rc == 0
        assert capsys.readouterr().out == "v\n"
