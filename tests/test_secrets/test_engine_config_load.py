"""Engine-side read boundary: decrypt the stored config document on load.

The whole ``company_config`` payload is stored as one encrypted blob. The
read sites (the ``revision_activated`` Pulsar handler, the CLI, the API)
run :func:`crewlet.secrets.load_config` to decrypt it into a plaintext
``CompanyConfig`` *before* calling :meth:`Engine.apply_config`. These tests
cover that boundary: a stored document decrypts to usable credentials, and
an encrypted document with no keyring fails closed rather than booting with
an opaque blob.
"""

from __future__ import annotations

import pytest

from crewlet.config import BootstrapConfig, CompanyConfig
from crewlet.config_yaml import company_config_to_dict
from crewlet.engine import Engine
from crewlet.secrets import (
    KeyringCipher,
    SecretCipherError,
    is_encrypted_document,
    load_config,
    store_config,
)
from crewlet.secrets.keygen import generate_key

pytestmark = pytest.mark.asyncio


def _bootstrap_with_keyring() -> tuple[BootstrapConfig, KeyringCipher]:
    boot = BootstrapConfig.model_validate(
        {
            "secrets": {
                "active_key_id": "k1",
                "keys": [{"id": "k1", "material": generate_key()}],
            }
        }
    )
    cipher = KeyringCipher.from_config(boot.secrets)
    assert cipher is not None
    return boot, cipher


def _company_with_secret() -> CompanyConfig:
    return CompanyConfig.model_validate(
        {
            "name": "Acme",
            "providers": {
                "llm": {
                    "default": {
                        "type": "openai-compatible",
                        "model": "m",
                        "base_url": "http://example.invalid/v1",
                        "api_keys": ["sk-real-secret"],
                    }
                }
            },
        }
    )


async def test_load_document_then_apply_uses_decrypted_secret():
    # The stored payload is one encrypted blob; the read site unwraps it to
    # a plaintext CompanyConfig before apply_config.
    boot, cipher = _bootstrap_with_keyring()
    stored = store_config(company_config_to_dict(_company_with_secret()), cipher)
    # The stored form is an opaque wrapper — not a valid CompanyConfig.
    assert is_encrypted_document(stored)

    engine = Engine.from_bootstrap(boot)
    new = CompanyConfig.model_validate(load_config(stored, engine._cipher))
    await engine.apply_config(new)
    assert engine._active_config.providers.llm["default"].api_keys == ["sk-real-secret"]


async def test_load_config_fails_closed_without_keyring():
    _, cipher = _bootstrap_with_keyring()
    stored = store_config(company_config_to_dict(_company_with_secret()), cipher)

    # A bootstrap with no secrets → engine has no cipher.
    engine = Engine.from_bootstrap(BootstrapConfig())
    assert engine._cipher is None
    with pytest.raises(SecretCipherError):
        load_config(stored, engine._cipher)


async def test_plaintext_config_without_keyring_applies():
    # A plaintext payload (no keyring) passes through load_config and applies
    # fine — encryption is opt-in via Tier A secrets.
    engine = Engine.from_bootstrap(BootstrapConfig())
    assert engine._cipher is None
    plain = company_config_to_dict(CompanyConfig(name="Plain Co"))
    new = CompanyConfig.model_validate(load_config(plain, engine._cipher))
    await engine.apply_config(new)
    assert engine._active_config.name == "Plain Co"
