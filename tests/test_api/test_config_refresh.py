"""Tests for the standalone-API config-refresh helpers."""

from __future__ import annotations

import types

from crewlet.api.config_refresh import _apply_payload_to_app, _serialize_agent_roles


def _app() -> types.SimpleNamespace:
    return types.SimpleNamespace(state=types.SimpleNamespace())


# ── ${VAR} references resolve for the HMAC / JWT secrets ──────────────


def test_apply_payload_resolves_github_webhook_secret(monkeypatch) -> None:
    """The webhook HMAC check compares against the resolved secret — a
    literal ``${VAR}`` would 401 every signed GitHub webhook."""
    monkeypatch.setenv("GH_HOOK", "supersecret")
    app = _app()
    _apply_payload_to_app(
        app,
        {"name": "Acme", "integrations": {"github": {"webhook_secret": "${GH_HOOK}"}}},
    )
    assert app.state.github_webhook_secret == "supersecret"


def test_apply_payload_resolves_forge_app_id(monkeypatch) -> None:
    monkeypatch.setenv("FORGE_ID", "ari:cloud:ecosystem::app/123")
    app = _app()
    _apply_payload_to_app(
        app, {"name": "Acme", "integrations": {"forge_app_id": "${FORGE_ID}"}}
    )
    assert app.state.forge_app_id == "ari:cloud:ecosystem::app/123"


def test_apply_payload_unset_var_resolves_to_empty(monkeypatch) -> None:
    monkeypatch.delenv("MISSING_HOOK", raising=False)
    app = _app()
    _apply_payload_to_app(
        app,
        {
            "name": "Acme",
            "integrations": {"github": {"webhook_secret": "${MISSING_HOOK}"}},
        },
    )
    assert app.state.github_webhook_secret == ""


def test_apply_payload_plain_secret_passthrough() -> None:
    app = _app()
    _apply_payload_to_app(
        app,
        {
            "name": "Acme",
            "integrations": {"github": {"webhook_secret": "literal-secret"}},
        },
    )
    assert app.state.github_webhook_secret == "literal-secret"


def test_apply_payload_resolves_plane_webhook_secret(monkeypatch) -> None:
    """The Plane webhook HMAC check compares against the resolved
    secret — a literal ``${VAR}`` would 401 every signed delivery."""
    monkeypatch.setenv("PLANE_HOOK", "plane-hook-secret")
    app = _app()
    _apply_payload_to_app(
        app,
        {
            "name": "Acme",
            "integrations": {"plane": {"webhook_secret": "${PLANE_HOOK}"}},
        },
    )
    assert app.state.plane_webhook_secret == "plane-hook-secret"


def test_apply_payload_plane_secret_plain_passthrough() -> None:
    app = _app()
    _apply_payload_to_app(
        app,
        {
            "name": "Acme",
            "integrations": {"plane": {"webhook_secret": "plane-literal"}},
        },
    )
    assert app.state.plane_webhook_secret == "plane-literal"


def test_apply_payload_plane_secret_unset_is_none() -> None:
    """No plane block → ``None``, so the webhook endpoint 500s instead
    of verifying against an empty-string key."""
    app = _app()
    _apply_payload_to_app(app, {"name": "Acme"})
    assert app.state.plane_webhook_secret is None


def test_apply_payload_plane_unset_var_resolves_to_none(monkeypatch) -> None:
    monkeypatch.delenv("MISSING_PLANE_HOOK", raising=False)
    app = _app()
    _apply_payload_to_app(
        app,
        {
            "name": "Acme",
            "integrations": {"plane": {"webhook_secret": "${MISSING_PLANE_HOOK}"}},
        },
    )
    assert app.state.plane_webhook_secret is None


def test_apply_payload_encrypted_without_keyring_fails_gracefully() -> None:
    # A standalone API with no keyring pointed at an encrypted DB must fail
    # closed (configured=False) — not crash the process at boot/refresh.
    from crewlet.secrets import KeyringCipher
    from crewlet.secrets.cipher import decode_key_material
    from crewlet.secrets.document import encrypt_document
    from crewlet.secrets.keygen import generate_key

    cipher = KeyringCipher(
        keys={"k1": decode_key_material(generate_key())}, active_key_id="k1"
    )
    encrypted = encrypt_document({"name": "Acme"}, cipher)

    app = _app()
    app.state.secret_cipher = None  # this process has no keyring
    # Must not raise.
    _apply_payload_to_app(app, encrypted)
    assert app.state.configured is False


# ── the API derives handles with the canonical slugify ────────────────


def test_serialize_agent_roles_uses_canonical_slug() -> None:
    """Handle must match the engine's ``slugify`` (punctuation → dash),
    so ``derive_agent_id`` lines up with the spawned agent's UUID."""
    rows = _serialize_agent_roles({"name": "Acme", "roles": [{"name": "QA/Test Lead"}]})
    assert rows[0]["handle"] == "qa-test-lead"


def test_serialize_agent_roles_skips_human_seats() -> None:
    rows = _serialize_agent_roles(
        {
            "name": "Acme",
            "roles": [
                {"name": "Engineer"},
                {
                    "name": "Sarah Chen",
                    "kind": "human",
                    "email": "sarah@acme.com",
                    "contact": {"slack_user_id": "U0HUMAN"},
                },
            ],
            "units": [
                {
                    "name": "Core",
                    "roles": [
                        {"name": "Dev"},
                        {"name": "Pat Ops", "kind": "human"},
                    ],
                }
            ],
        }
    )
    assert [r["name"] for r in rows] == ["Engineer", "Dev"]
