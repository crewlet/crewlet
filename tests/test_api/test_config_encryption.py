"""API wiring for whole-config encryption at rest.

``PUT /config`` encrypts the whole config document before storing it;
``GET /config`` decrypts the structure for display but masks every secret;
a redacted ``GET`` → edit → ``PUT`` round-trip keeps the stored secret.
"""

from __future__ import annotations

from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock
from uuid import uuid4

from starlette.testclient import TestClient

from crewlet.api.app import create_app
from crewlet.config import (
    ApiAuthConfig,
    ApiAuthTokenConfig,
    ApiConfig,
    BootstrapConfig,
)
from crewlet.secrets import KeyringCipher, is_encrypted_document, load_config
from crewlet.secrets.keygen import generate_key

_AUTH_PUT = {"Authorization": "Bearer secret-1", "X-Summary": "test"}


class _MockQueue:
    def __init__(self) -> None:
        self.published: list = []

    async def publish(self, topic, event) -> None:
        self.published.append((topic, event))

    async def subscribe(self, topic, group, handler) -> None: ...
    async def start(self) -> None: ...
    async def stop(self) -> None: ...


def _bootstrap_with_secrets() -> BootstrapConfig:
    return BootstrapConfig.model_validate(
        {
            "api": {"auth": {"tokens": [{"id": "t1", "token": "secret-1"}]}},
            "secrets": {
                "active_key_id": "k1",
                "keys": [{"id": "k1", "material": generate_key()}],
            },
        }
    )


def _bootstrap_no_secrets() -> BootstrapConfig:
    return BootstrapConfig(
        api=ApiConfig(
            auth=ApiAuthConfig(tokens=[ApiAuthTokenConfig(id="t1", token="secret-1")])
        ),
    )


def _store() -> MagicMock:
    s = MagicMock()
    s.get_active = AsyncMock(return_value=None)
    s.get_revision = AsyncMock(return_value=None)
    s.insert_active = AsyncMock(return_value=uuid4())
    return s


_BODY = {
    "name": "Acme",
    "providers": {
        "llm": {
            "default": {
                "type": "openai-compatible",
                "model": "m",
                "base_url": "http://example.invalid/v1",
                "api_keys": ["sk-secret"],
            }
        }
    },
}


# ── write path: encrypt the whole document ────────────────────────────


def test_put_config_stores_encrypted_document() -> None:
    store = _store()
    app = create_app(
        event_queue=_MockQueue(),
        bootstrap=_bootstrap_with_secrets(),
        company_config_store=store,
    )
    resp = TestClient(app).put("/config", headers=_AUTH_PUT, json=_BODY)
    assert resp.status_code == 201

    stored = store.insert_active.call_args.args[0]
    # The whole payload is one opaque blob — no structure visible at rest.
    assert is_encrypted_document(stored)
    for leak in ("gpt-4o", "Acme", "sk-secret", "providers"):
        assert leak not in str(stored)


def test_put_config_without_keyring_stores_plaintext() -> None:
    # Encryption is opt-in: no Tier A secrets → plaintext storage.
    store = _store()
    app = create_app(
        event_queue=_MockQueue(),
        bootstrap=_bootstrap_no_secrets(),
        company_config_store=store,
    )
    resp = TestClient(app).put("/config", headers=_AUTH_PUT, json=_BODY)
    assert resp.status_code == 201
    stored = store.insert_active.call_args.args[0]
    assert stored["providers"]["llm"]["default"]["api_keys"] == ["sk-secret"]


def test_put_config_encrypts_existing_plaintext_revision() -> None:
    # A deployment that added a keyring to a previously-plaintext DB: the
    # next PUT re-stores the whole config as an encrypted document (the
    # write path migrates it on the spot, no explicit `seal` needed).
    from datetime import UTC, datetime

    from crewlet.db.company_config import CompanyConfigRevision

    plaintext_rev = CompanyConfigRevision(
        revision_id=uuid4(),
        parent_revision_id=None,
        created_at=datetime.now(UTC),
        created_by="alice",
        source="api",
        summary="legacy plaintext",
        payload={"name": "Old", "mission": "unencrypted"},
        is_active=True,
        activated_at=datetime.now(UTC),
    )
    store = _store()
    store.get_active = AsyncMock(return_value=plaintext_rev)
    app = create_app(
        event_queue=_MockQueue(),
        bootstrap=_bootstrap_with_secrets(),
        company_config_store=store,
    )
    # No If-Match header → the write applies without a conflict check.
    resp = TestClient(app).put("/config", headers=_AUTH_PUT, json=_BODY)
    assert resp.status_code == 201
    stored = store.insert_active.call_args.args[0]
    assert is_encrypted_document(stored)


# ── webhook secret resolution ─────────────────────────────────────────


def test_webhook_secret_plaintext_resolves_onto_app_state() -> None:
    from crewlet.api.config_refresh import _set_webhook_secrets

    app = SimpleNamespace(state=SimpleNamespace(secret_cipher=None))
    payload = {"integrations": {"github": {"webhook_secret": "hunter2"}}}
    _set_webhook_secrets(app, payload)
    assert app.state.github_webhook_secret == "hunter2"


def test_webhook_secret_var_reference_resolves(monkeypatch) -> None:
    from crewlet.api.config_refresh import _set_webhook_secrets

    monkeypatch.setenv("GH_WEBHOOK", "resolved")
    app = SimpleNamespace(state=SimpleNamespace(secret_cipher=None))
    payload = {"integrations": {"github": {"webhook_secret": "${GH_WEBHOOK}"}}}
    _set_webhook_secrets(app, payload)
    assert app.state.github_webhook_secret == "resolved"


# ── read path: redaction + round-trip ─────────────────────────────────


def _encrypted_store():
    """A store whose active revision holds an encrypted config document."""
    from datetime import UTC, datetime

    from crewlet.config import CompanyConfig
    from crewlet.config_yaml import company_config_to_dict
    from crewlet.db.company_config import CompanyConfigRevision
    from crewlet.secrets import store_config

    boot = _bootstrap_with_secrets()
    cipher = KeyringCipher.from_config(boot.secrets)
    payload = store_config(
        company_config_to_dict(CompanyConfig.model_validate(_BODY)), cipher
    )
    assert is_encrypted_document(payload)

    rev = CompanyConfigRevision(
        revision_id=uuid4(),
        parent_revision_id=None,
        created_at=datetime.now(UTC),
        created_by="alice",
        source="api",
        summary="test",
        payload=payload,
        is_active=True,
        activated_at=datetime.now(UTC),
    )
    store = _store()
    store.get_active = AsyncMock(return_value=rev)
    store.get_revision = AsyncMock(return_value=rev)
    return boot, store, cipher


def test_get_config_redacts_secret_but_shows_structure() -> None:
    boot, store, _cipher = _encrypted_store()
    app = create_app(
        event_queue=_MockQueue(), bootstrap=boot, company_config_store=store
    )
    resp = TestClient(app).get("/config", headers={"Authorization": "Bearer secret-1"})
    assert resp.status_code == 200
    body = resp.json()
    # Structure is visible (decrypted for display)...
    assert body["providers"]["llm"]["default"]["model"] == "m"
    # ...but the secret is masked and no ciphertext/plaintext leaks.
    assert body["providers"]["llm"]["default"]["api_keys"][0] == {
        "encrypted": True,
        "key_id": None,
    }
    assert "sk-secret" not in resp.text
    assert "enc:v1:" not in resp.text


def test_get_config_roundtrip_keeps_secret() -> None:
    # GET (redacted) → edit a non-secret field → PUT the whole doc back:
    # the secret must survive, encrypted, not be clobbered by the marker.
    boot, store, cipher = _encrypted_store()
    app = create_app(
        event_queue=_MockQueue(), bootstrap=boot, company_config_store=store
    )
    client = TestClient(app)
    doc = client.get("/config", headers={"Authorization": "Bearer secret-1"}).json()
    doc["mission"] = "edited"

    resp = client.put("/config", headers=_AUTH_PUT, json=doc)
    assert resp.status_code == 201
    stored = store.insert_active.call_args.args[0]
    assert is_encrypted_document(stored)
    # Decrypt the newly-stored document: the marker was restored to the
    # original secret, and the edited field landed.
    plain = load_config(stored, cipher)
    assert plain["providers"]["llm"]["default"]["api_keys"] == ["sk-secret"]
    assert plain["mission"] == "edited"
