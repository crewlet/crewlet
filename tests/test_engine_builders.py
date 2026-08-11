"""Tests for pure engine builders (``crewlet.engine_builders``)."""

from __future__ import annotations

import os

from crewlet.config import CompanyConfig, config_to_organization
from crewlet.engine_builders import (
    build_notification_transports,
    build_plane_integration,
    resolve_skill_variables,
)


def test_resolve_skill_variables_empty_by_default() -> None:
    assert resolve_skill_variables(CompanyConfig(name="X")) == {}


def test_resolve_skill_variables_passes_literals_through() -> None:
    cfg = CompanyConfig(
        name="X",
        skill_variables={
            "confluence_base_url": "https://acme.atlassian.net/wiki",
            "jira_base_url": "https://acme.atlassian.net",
        },
    )
    assert resolve_skill_variables(cfg) == {
        "confluence_base_url": "https://acme.atlassian.net/wiki",
        "jira_base_url": "https://acme.atlassian.net",
    }


def test_resolve_skill_variables_resolves_env_refs() -> None:
    os.environ["TEST_CONF_BASE"] = "https://resolved.atlassian.net/wiki"
    try:
        cfg = CompanyConfig(
            name="X",
            skill_variables={"confluence_base_url": "${TEST_CONF_BASE}"},
        )
        assert resolve_skill_variables(cfg) == {
            "confluence_base_url": "https://resolved.atlassian.net/wiki"
        }
    finally:
        del os.environ["TEST_CONF_BASE"]


def test_resolve_skill_variables_drops_empty_values() -> None:
    """An unset ${ENV} (or blank value) is dropped so the ${name}
    reference renders as a visible literal, not a malformed value."""
    cfg = CompanyConfig(
        name="X",
        skill_variables={
            "jira_base_url": "https://acme.atlassian.net",
            "confluence_base_url": "${DEFINITELY_UNSET_ENV_XYZ}",
            "blank": "",
        },
    )
    assert resolve_skill_variables(cfg) == {
        "jira_base_url": "https://acme.atlassian.net"
    }


# ── build_plane_integration ────────────────────────────────────────


def _plane_block(**overrides: object) -> dict[str, object]:
    block: dict[str, object] = {
        "enabled": True,
        "url": "https://plane.example.com",
        "workspace": "acme",
        "webhook_secret": "wh-secret",
    }
    block.update(overrides)
    return block


def test_build_plane_integration_none_when_absent() -> None:
    cfg = CompanyConfig(name="X")
    assert build_plane_integration(cfg, config_to_organization(cfg)) is None


def test_build_plane_integration_none_when_disabled() -> None:
    cfg = CompanyConfig(name="X", integrations={"plane": {"enabled": False}})
    assert build_plane_integration(cfg, config_to_organization(cfg)) is None


def test_build_plane_integration_resolves_env_refs(monkeypatch) -> None:
    monkeypatch.setenv("TEST_PLANE_WH_SECRET", "real-secret")
    monkeypatch.setenv("TEST_PLANE_ENGINE_TOKEN", "real-token")
    cfg = CompanyConfig(
        name="X",
        integrations={
            "plane": _plane_block(
                webhook_secret="${TEST_PLANE_WH_SECRET}",
                token="${TEST_PLANE_ENGINE_TOKEN}",
            )
        },
    )
    result = build_plane_integration(cfg, config_to_organization(cfg))
    assert result is not None
    assert result.enabled is True
    assert result.url == "https://plane.example.com"
    assert result.workspace == "acme"
    assert result.webhook_secret == "real-secret"
    assert result.token == "real-token"


def test_build_plane_integration_warns_when_engine_token_missing(
    monkeypatch,
) -> None:
    """An empty engine read token degrades routing (no subscriber
    fan-out, cold project cache) — surfaced at boot, next to
    ``plane_integration_enabled``, not just at transport start."""
    import crewlet.engine_builders as builders_mod

    warnings: list[str] = []
    real_warning = builders_mod.logger.warning

    def _capture(event, **kwargs):  # type: ignore[no-untyped-def]
        warnings.append(event)
        return real_warning(event, **kwargs)

    monkeypatch.setattr(builders_mod.logger, "warning", _capture)

    cfg = CompanyConfig(name="X", integrations={"plane": _plane_block()})
    result = build_plane_integration(cfg, config_to_organization(cfg))
    assert result is not None
    assert "plane_engine_token_missing" in warnings

    warnings.clear()
    cfg = CompanyConfig(name="X", integrations={"plane": _plane_block(token="tok")})
    build_plane_integration(cfg, config_to_organization(cfg))
    assert "plane_engine_token_missing" not in warnings


# ── build_notification_transports: Plane ───────────────────────────


def test_build_notification_transports_appends_plane_when_enabled() -> None:
    from crewlet.notifications.transports.plane import PlaneTransport

    cfg = CompanyConfig(name="X", integrations={"plane": _plane_block()})
    transports = build_notification_transports(cfg, storage=None)
    planes = [t for t in transports if isinstance(t, PlaneTransport)]
    assert len(planes) == 1
    assert planes[0].name == "plane"


def test_build_notification_transports_skips_disabled_plane() -> None:
    from crewlet.notifications.transports.plane import PlaneTransport

    cfg = CompanyConfig(name="X", integrations={"plane": {"enabled": False}})
    transports = build_notification_transports(cfg, storage=None)
    assert not any(isinstance(t, PlaneTransport) for t in transports)

    cfg = CompanyConfig(name="X")
    transports = build_notification_transports(cfg, storage=None)
    assert not any(isinstance(t, PlaneTransport) for t in transports)
