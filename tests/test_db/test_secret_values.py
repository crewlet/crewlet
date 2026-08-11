"""The encrypted ``secret_values`` store.

Mirrors ``test_company_config.py``: no real DB — the asyncpg surface is
mocked and the assertions inspect the SQL and args. The cipher, by
contrast, is REAL (a keyring over a fixed key), because the properties
worth pinning here are cryptographic: the value never hits SQL in the
clear, and the row's name is bound as associated data.
"""

from __future__ import annotations

import base64
from datetime import UTC, datetime
from unittest.mock import AsyncMock

import pytest

from crewlet.db.secret_values import (
    DatabaseSecretSource,
    SecretStoreError,
    SecretValueStore,
    load_secret_source,
)
from crewlet.secrets.cipher import KeyringCipher, SecretDecryptError

_KEY_A = base64.b64encode(b"A" * 32).decode()
_KEY_B = base64.b64encode(b"B" * 32).decode()


def _cipher(active: str = "key-1") -> KeyringCipher:
    return KeyringCipher(
        keys={
            "key-1": base64.b64decode(_KEY_A),
            "key-2": base64.b64decode(_KEY_B),
        },
        active_key_id=active,
    )


@pytest.fixture
def mock_db():
    db = AsyncMock()
    db.execute = AsyncMock(return_value=[])
    db.fetchrow = AsyncMock(return_value=None)
    return db


@pytest.fixture
def store(mock_db):
    return SecretValueStore(mock_db, _cipher())


# ── put ──


async def test_put_stores_ciphertext_never_the_plaintext(store, mock_db):
    await store.put("SLACK_BOT_TOKEN_CEO", "xoxb-real", updated_by="cli", source="cli")
    sql, *args = mock_db.execute.call_args[0]
    assert "INSERT INTO secret_values" in sql
    assert "ON CONFLICT (name) DO UPDATE" in sql
    assert args[0] == "SLACK_BOT_TOKEN_CEO"
    assert args[1].startswith("enc:v1:key-1:")
    assert args[2] == "key-1"
    # The plaintext must appear nowhere in what reaches the database.
    assert "xoxb-real" not in str(mock_db.execute.call_args)


async def test_put_rejects_a_name_no_reference_could_read(store):
    # A name outside the ${VAR} grammar would be write-only.
    for bad in ("1LEADING_DIGIT", "has-dash", "has.dot", ""):
        with pytest.raises(SecretStoreError, match="not a valid environment"):
            await store.put(bad, "v", updated_by="cli", source="cli")


async def test_put_without_a_keyring_refuses(mock_db):
    # Unlike company_config there is NO plaintext mode: a dedicated secret
    # store that can hold unencrypted secrets is a footgun with no upside.
    with pytest.raises(SecretStoreError, match="encryption keyring"):
        await SecretValueStore(mock_db, None).put(
            "T", "v", updated_by="cli", source="cli"
        )


# ── get / round-trip ──


async def test_round_trip(store, mock_db):
    await store.put("TOKEN", "s3cret", updated_by="cli", source="cli")
    sealed = mock_db.execute.call_args[0][2]
    mock_db.fetchrow = AsyncMock(return_value={"value": sealed})
    assert await store.get("TOKEN") == "s3cret"


async def test_get_missing_returns_none(store):
    assert await store.get("ABSENT") is None


async def test_ciphertext_is_bound_to_its_row_name(store, mock_db):
    """A ciphertext lifted into a different row must fail, not impersonate.

    Without per-name associated data, anyone able to write the table could
    swap a low-value secret's ciphertext into a high-value row and the
    engine would serve it happily.
    """
    await store.put("LOW_VALUE", "harmless", updated_by="cli", source="cli")
    sealed = mock_db.execute.call_args[0][2]
    mock_db.fetchrow = AsyncMock(return_value={"value": sealed})
    with pytest.raises(SecretDecryptError):
        await store.get("ADMIN_TOKEN")


# ── delete / list ──


async def test_delete_reports_whether_a_row_went(store, mock_db):
    mock_db.execute = AsyncMock(return_value=[{"name": "T"}])
    assert await store.delete("T") is True
    mock_db.execute = AsyncMock(return_value=[])
    assert await store.delete("T") is False


async def test_list_records_carries_no_value(store, mock_db):
    mock_db.execute = AsyncMock(
        return_value=[
            {
                "name": "TOKEN",
                "key_id": "key-1",
                "updated_at": datetime(2026, 8, 3, tzinfo=UTC),
                "updated_by": "cli",
                "source": "gitlab-provision",
            }
        ]
    )
    (record,) = await store.list_records()
    assert record.name == "TOKEN"
    assert record.source == "gitlab-provision"
    # Structurally incapable of leaking — there is no value attribute.
    assert not hasattr(record, "value")


async def test_list_records_works_without_a_keyring(mock_db):
    # An operator locked out of the key still needs the inventory (which
    # names exist, under which key_id) for a key-loss postmortem.
    mock_db.execute = AsyncMock(
        return_value=[
            {
                "name": "T",
                "key_id": "lost-key",
                "updated_at": datetime(2026, 8, 3, tzinfo=UTC),
                "updated_by": "cli",
                "source": "cli",
            }
        ]
    )
    (record,) = await SecretValueStore(mock_db, None).list_records()
    assert record.key_id == "lost-key"


# ── load_all ──


async def test_load_all_decrypts_every_row(store, mock_db):
    cipher = _cipher()
    rows = [
        {"name": "A", "value": cipher.encrypt("va", aad="secret_values/A")},
        {"name": "B", "value": cipher.encrypt("vb", aad="secret_values/B")},
    ]
    mock_db.execute = AsyncMock(return_value=rows)
    assert await store.load_all() == {"A": "va", "B": "vb"}


async def test_load_all_empty_table_needs_no_keyring(mock_db):
    # Deployments that never used the store stay completely inert.
    assert await SecretValueStore(mock_db, None).load_all() == {}


async def test_load_all_populated_without_keyring_raises(mock_db):
    # The critical failure mode: answering "" for a name someone
    # deliberately stored yields an empty Bearer token that looks live to
    # every layer until the provider rejects it.
    mock_db.execute = AsyncMock(return_value=[{"name": "A", "value": "enc:v1:k:x"}])
    with pytest.raises(SecretStoreError):
        await SecretValueStore(mock_db, None).load_all()


# ── rekey ──


async def test_rekey_reseals_only_stale_rows(mock_db):
    old = _cipher(active="key-2")
    rows = [
        {
            "name": "STALE",
            "value": old.encrypt("v1", aad="secret_values/STALE"),
            "key_id": "key-2",
        },
        {
            "name": "CURRENT",
            "value": "enc:v1:key-1:unused",
            "key_id": "key-1",
        },
    ]
    mock_db.execute = AsyncMock(return_value=rows)
    store = SecretValueStore(mock_db, _cipher(active="key-1"))
    assert await store.rekey() == ["STALE"]
    updates = [
        c for c in mock_db.execute.call_args_list if "UPDATE secret_values" in c[0][0]
    ]
    assert len(updates) == 1
    assert updates[0][0][1] == "STALE"
    assert updates[0][0][3] == "key-1"


# ── DatabaseSecretSource / load_secret_source ──


async def test_source_refresh_swaps_the_snapshot(mock_db):
    cipher = _cipher()
    store = SecretValueStore(mock_db, cipher)
    source = DatabaseSecretSource(store)
    assert source.get("A") is None

    mock_db.execute = AsyncMock(
        return_value=[
            {"name": "A", "value": cipher.encrypt("v1", aad="secret_values/A")}
        ]
    )
    await source.refresh()
    assert source.get("A") == "v1"
    assert source.names() == ["A"]

    mock_db.execute = AsyncMock(
        return_value=[
            {"name": "A", "value": cipher.encrypt("v2", aad="secret_values/A")}
        ]
    )
    await source.refresh()
    assert source.get("A") == "v2"


async def test_load_secret_source_returns_none_for_an_empty_table(mock_db):
    # None keeps _resolve_env_value on its exact pre-existing os.environ
    # path — the feature engages only once a secret is actually stored.
    assert await load_secret_source(mock_db, _cipher()) is None
    assert await load_secret_source(mock_db, None) is None


async def test_load_secret_source_primes_the_snapshot(mock_db):
    cipher = _cipher()
    mock_db.execute = AsyncMock(
        return_value=[
            {"name": "T", "value": cipher.encrypt("v", aad="secret_values/T")}
        ]
    )
    source = await load_secret_source(mock_db, cipher)
    assert source is not None
    assert source.get("T") == "v"
