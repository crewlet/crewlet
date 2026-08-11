"""Tests for the AES-256-GCM secret cipher and its envelope format."""

from __future__ import annotations

import base64

import pytest

from crewlet.secrets.cipher import (
    ENVELOPE_PREFIX,
    KEY_BYTES,
    KeyringCipher,
    SecretCipherError,
    SecretDecryptError,
    decode_key_material,
    is_envelope,
    parse_key_id,
)
from crewlet.secrets.fake import FakeCipher
from crewlet.secrets.keygen import generate_key


def _key() -> str:
    return generate_key()


def _cipher(active: str = "k1", **extra: str) -> KeyringCipher:
    keys = {"k1": decode_key_material(_key()), **extra}
    return KeyringCipher(keys=keys, active_key_id=active)


# ── round-trip + envelope shape ───────────────────────────────────────


def test_round_trip():
    c = _cipher()
    token = c.encrypt("sk-secret", aad="/providers/llm/default/api_keys/0")
    assert token != "sk-secret"
    assert c.decrypt(token, aad="/providers/llm/default/api_keys/0") == "sk-secret"


def test_envelope_shape_and_key_id():
    c = _cipher()
    token = c.encrypt("v", aad="/x")
    assert token.startswith(ENVELOPE_PREFIX)
    assert is_envelope(token)
    assert parse_key_id(token) == "k1"


def test_empty_string_round_trips():
    c = _cipher()
    token = c.encrypt("", aad="/x")
    assert c.decrypt(token, aad="/x") == ""


def test_unicode_round_trips():
    c = _cipher()
    token = c.encrypt("pá$$wörd-🔐", aad="/x")
    assert c.decrypt(token, aad="/x") == "pá$$wörd-🔐"


def test_two_encryptions_differ_by_nonce():
    c = _cipher()
    assert c.encrypt("v", aad="/x") != c.encrypt("v", aad="/x")


# ── is_envelope / parse_key_id ────────────────────────────────────────


@pytest.mark.parametrize(
    "value",
    [
        "sk-plaintext",
        "${VAR}",
        "enc:v1:",
        "enc:v1:kid:",
        "enc:v2:kid:blob",
        "",
        42,
        None,
    ],
)
def test_is_envelope_false(value):
    assert not is_envelope(value)


def test_parse_key_id_non_envelope():
    assert parse_key_id("plaintext") is None


# ── associated-data (field-path) binding ──────────────────────────────


def test_aad_mismatch_fails():
    c = _cipher()
    token = c.encrypt("v", aad="/integrations/jira/token")
    with pytest.raises(SecretDecryptError):
        c.decrypt(token, aad="/integrations/github/webhook_secret")


# ── wrong key / tamper / unknown key_id ───────────────────────────────


def test_wrong_key_fails():
    a = _cipher()
    b = _cipher()  # different random key under the same id "k1"
    token = a.encrypt("v", aad="/x")
    with pytest.raises(SecretDecryptError):
        b.decrypt(token, aad="/x")


def test_unknown_key_id_fails():
    c = _cipher()
    token = c.encrypt("v", aad="/x")
    forged = token.replace("enc:v1:k1:", "enc:v1:nope:")
    with pytest.raises(SecretDecryptError):
        c.decrypt(forged, aad="/x")


def test_tampered_ciphertext_fails():
    c = _cipher()
    token = c.encrypt("v", aad="/x")
    prefix, blob = token.rsplit(":", 1)
    raw = bytearray(base64.b64decode(blob))
    raw[-1] ^= 0x01  # flip a bit in the tag
    tampered = f"{prefix}:{base64.b64encode(bytes(raw)).decode()}"
    with pytest.raises(SecretDecryptError):
        c.decrypt(tampered, aad="/x")


def test_decrypt_non_envelope_fails():
    c = _cipher()
    with pytest.raises(SecretDecryptError):
        c.decrypt("plaintext", aad="/x")


# ── multi-key ring / rotation ─────────────────────────────────────────


def test_rotation_old_ciphertext_still_decrypts():
    old_material = decode_key_material(_key())
    old = KeyringCipher(keys={"old": old_material}, active_key_id="old")
    token = old.encrypt("v", aad="/x")

    # Rotate: new active key, old key retained for decrypt.
    new_material = decode_key_material(_key())
    rotated = KeyringCipher(
        keys={"old": old_material, "new": new_material}, active_key_id="new"
    )
    assert rotated.decrypt(token, aad="/x") == "v"  # old ciphertext
    fresh = rotated.encrypt("w", aad="/y")
    assert parse_key_id(fresh) == "new"  # new writes use the active key


# ── key material validation ───────────────────────────────────────────


def test_decode_key_material_rejects_bad_base64():
    with pytest.raises(SecretCipherError):
        decode_key_material("not base64!!!")


def test_decode_key_material_rejects_wrong_length():
    short = base64.b64encode(b"\x00" * 16).decode()
    with pytest.raises(SecretCipherError):
        decode_key_material(short)


def test_generated_key_decodes_to_32_bytes():
    assert len(decode_key_material(generate_key())) == KEY_BYTES


# ── KeyringCipher construction guards ─────────────────────────────────


def test_empty_keyring_rejected():
    with pytest.raises(SecretCipherError):
        KeyringCipher(keys={}, active_key_id="k1")


def test_active_not_in_keyring_rejected():
    with pytest.raises(SecretCipherError):
        KeyringCipher(keys={"k1": b"\x00" * 32}, active_key_id="missing")


def test_wrong_key_length_rejected():
    with pytest.raises(SecretCipherError):
        KeyringCipher(keys={"k1": b"\x00" * 16}, active_key_id="k1")


# ── FakeCipher parity ─────────────────────────────────────────────────


def test_fake_cipher_round_trip_and_aad():
    c = FakeCipher()
    token = c.encrypt("v", aad="/x")
    assert is_envelope(token)
    assert c.decrypt(token, aad="/x") == "v"
    with pytest.raises(SecretDecryptError):
        c.decrypt(token, aad="/y")
