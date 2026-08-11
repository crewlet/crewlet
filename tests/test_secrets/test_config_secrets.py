"""Tests for the Tier A ``secrets`` config block and the cipher factory."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from crewlet.config import BootstrapConfig, SecretsConfig
from crewlet.secrets.cipher import KeyringCipher
from crewlet.secrets.keygen import generate_key


def _keys(*ids: str) -> list[dict]:
    return [{"id": i, "material": generate_key()} for i in ids]


# ── SecretsConfig validation ──────────────────────────────────────────


def test_empty_secrets_is_valid_and_default():
    cfg = SecretsConfig()
    assert cfg.keys == []
    assert cfg.active_key_id == ""


def test_valid_single_key():
    cfg = SecretsConfig.model_validate({"active_key_id": "k1", "keys": _keys("k1")})
    assert cfg.active_key_id == "k1"


def test_active_key_required_when_keys_present():
    with pytest.raises(ValidationError):
        SecretsConfig.model_validate({"keys": _keys("k1")})


def test_active_key_must_match_a_key():
    with pytest.raises(ValidationError):
        SecretsConfig.model_validate({"active_key_id": "nope", "keys": _keys("k1")})


def test_duplicate_key_ids_rejected():
    with pytest.raises(ValidationError):
        SecretsConfig.model_validate(
            {"active_key_id": "k1", "keys": _keys("k1") + _keys("k1")}
        )


def test_key_id_pattern_rejects_colon():
    # A colon would break envelope parsing (enc:v1:<id>:<blob>).
    with pytest.raises(ValidationError):
        SecretsConfig.model_validate(
            {
                "active_key_id": "a:b",
                "keys": [{"id": "a:b", "material": generate_key()}],
            }
        )


def test_rotation_keyring_two_keys():
    cfg = SecretsConfig.model_validate(
        {"active_key_id": "new", "keys": _keys("old", "new")}
    )
    assert [k.id for k in cfg.keys] == ["old", "new"]


# ── BootstrapConfig integration ───────────────────────────────────────


def test_bootstrap_accepts_secrets_block():
    boot = BootstrapConfig.model_validate(
        {"secrets": {"active_key_id": "k1", "keys": _keys("k1")}}
    )
    assert boot.secrets.active_key_id == "k1"


def test_bootstrap_defaults_to_empty_secrets():
    assert BootstrapConfig().secrets.keys == []


def test_bootstrap_rejects_unknown_secret_field():
    with pytest.raises(ValidationError):
        BootstrapConfig.model_validate({"secrets": {"nope": 1}})


# ── KeyringCipher.from_config factory ─────────────────────────────────


def test_from_config_returns_none_when_disabled():
    assert KeyringCipher.from_config(SecretsConfig()) is None


def test_from_config_builds_cipher():
    cfg = SecretsConfig.model_validate({"active_key_id": "k1", "keys": _keys("k1")})
    cipher = KeyringCipher.from_config(cfg)
    assert cipher is not None
    assert cipher.active_key_id == "k1"
    token = cipher.encrypt("v", aad="/x")
    assert cipher.decrypt(token, aad="/x") == "v"


def test_from_config_multi_key_decrypts_across_rotation():
    cfg = SecretsConfig.model_validate(
        {"active_key_id": "new", "keys": _keys("old", "new")}
    )
    cipher = KeyringCipher.from_config(cfg)
    assert cipher is not None
    assert cipher.active_key_id == "new"
