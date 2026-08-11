"""Tests for whole-config encryption + the read/write/redact/rekey helpers."""

from __future__ import annotations

import pytest

from crewlet.secrets import SecretCipherError, is_envelope, parse_key_id
from crewlet.secrets.cipher import KeyringCipher, decode_key_material
from crewlet.secrets.document import (
    DOCUMENT_WRAPPER_KEY,
    decrypt_document,
    encrypt_document,
    is_encrypted_document,
    load_config,
    redact_config,
    rekey_config,
    store_config,
)
from crewlet.secrets.fake import FakeCipher
from crewlet.secrets.keygen import generate_key

SAMPLE = {
    "name": "Acme",
    "mission": "ship",
    "providers": {
        "llm": {"default": {"type": "openai", "model": "gpt-4o", "api_keys": ["sk-x"]}},
        "embeddings": {"type": "openai", "api_key": "sk-emb"},
    },
    "roles": [{"name": "CEO", "mcp_env": {"gh": {"TOKEN": "ghp_ceo"}}}],
}


def _cipher() -> KeyringCipher:
    return KeyringCipher(
        keys={"k1": decode_key_material(generate_key())}, active_key_id="k1"
    )


# ── document encrypt/decrypt ──────────────────────────────────────────


def test_document_round_trip():
    c = _cipher()
    wrapped = encrypt_document(SAMPLE, c)
    assert set(wrapped) == {DOCUMENT_WRAPPER_KEY}
    assert is_envelope(wrapped[DOCUMENT_WRAPPER_KEY])
    assert decrypt_document(wrapped, c) == SAMPLE


def test_is_encrypted_document():
    c = _cipher()
    assert is_encrypted_document(encrypt_document(SAMPLE, c))
    assert not is_encrypted_document(SAMPLE)
    assert not is_encrypted_document({"__encrypted__": "not-an-envelope"})
    assert not is_encrypted_document({"__encrypted__": "enc:v1:k1:x", "name": "y"})


def test_document_hides_all_structure():
    # The blob must not leak field names or non-secret values.
    c = _cipher()
    blob = str(encrypt_document(SAMPLE, c))
    for leak in ("gpt-4o", "Acme", "mission", "sk-x", "ghp_ceo", "providers"):
        assert leak not in blob


# ── store_config (write) ──────────────────────────────────────────────


def test_store_config_wraps_whole_payload():
    c = _cipher()
    stored = store_config(SAMPLE, c)
    assert is_encrypted_document(stored)


def test_store_config_no_cipher_passthrough():
    assert store_config(SAMPLE, None) == SAMPLE


# ── load_config (read) ────────────────────────────────────────────────


def test_load_config_returns_plaintext():
    c = _cipher()
    assert load_config(store_config(SAMPLE, c), c) == SAMPLE
    assert load_config(SAMPLE, None) == SAMPLE  # plaintext passthrough


def test_load_fails_closed_without_keyring():
    c = _cipher()
    with pytest.raises(SecretCipherError):
        load_config(store_config(SAMPLE, c), None)


# ── redact_config (display) ───────────────────────────────────────────


def test_redact_config_shows_structure_masks_secrets():
    c = _cipher()
    stored = store_config(SAMPLE, c)
    view = redact_config(stored, c)
    # structure is visible after decrypt...
    assert view["name"] == "Acme"
    assert view["providers"]["llm"]["default"]["model"] == "gpt-4o"
    # ...but every secret is masked
    assert view["providers"]["embeddings"]["api_key"] == {
        "encrypted": True,
        "key_id": None,
    }
    assert "sk-emb" not in str(view)
    assert "ghp_ceo" not in str(view)


def test_redact_config_plaintext_payload_masks_secrets():
    # A plaintext (un-encrypted) payload is redacted directly.
    view = redact_config(SAMPLE, None)
    assert view["name"] == "Acme"
    assert view["providers"]["embeddings"]["api_key"] == {
        "encrypted": True,
        "key_id": None,
    }


def test_redact_config_without_keyring_is_opaque():
    c = _cipher()
    stored = store_config(SAMPLE, c)
    view = redact_config(stored, None)
    assert view == {DOCUMENT_WRAPPER_KEY: {"encrypted": True, "key_id": None}}
    assert "Acme" not in str(view)


# ── rekey_config (rotation) ───────────────────────────────────────────


def test_rekey_config_reencrypts_document():
    old_mat = decode_key_material(generate_key())
    new_mat = decode_key_material(generate_key())
    old = KeyringCipher(keys={"old": old_mat}, active_key_id="old")
    ring = KeyringCipher(keys={"old": old_mat, "new": new_mat}, active_key_id="new")

    stored = store_config(SAMPLE, old)
    assert parse_key_id(stored[DOCUMENT_WRAPPER_KEY]) == "old"

    rekeyed, changed = rekey_config(stored, ring, "new")
    assert changed == [DOCUMENT_WRAPPER_KEY]
    assert parse_key_id(rekeyed[DOCUMENT_WRAPPER_KEY]) == "new"
    assert decrypt_document(rekeyed, ring) == SAMPLE


def test_rekey_config_noop_when_active():
    c = _cipher()
    stored = store_config(SAMPLE, c)
    rekeyed, changed = rekey_config(stored, c, "k1")
    assert changed == []
    assert rekeyed == stored


def test_rekey_config_plaintext_is_noop():
    c = _cipher()
    rekeyed, changed = rekey_config(SAMPLE, c, "k1")
    assert changed == []
    assert rekeyed == SAMPLE


def test_store_config_with_fake_cipher():
    # FakeCipher satisfies the same protocol; useful for deterministic tests.
    c = FakeCipher()
    stored = store_config(SAMPLE, c)
    assert is_encrypted_document(stored)
    assert load_config(stored, c) == SAMPLE
