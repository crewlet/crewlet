"""Tests for ``build_sandbox_manager``."""

from __future__ import annotations

from crewlet.config import CompanyConfig, ProvidersConfig, SandboxProviderConfig
from crewlet.engine_builders import build_sandbox_manager
from crewlet.sandbox import SandboxManager


def _cfg(sandbox: SandboxProviderConfig | None) -> CompanyConfig:
    return CompanyConfig(name="Acme", providers=ProvidersConfig(sandbox=sandbox))


def test_no_sandbox_config_returns_none() -> None:
    assert build_sandbox_manager(_cfg(None)) is None


def test_type_none_returns_none() -> None:
    assert build_sandbox_manager(_cfg(SandboxProviderConfig(type="none"))) is None


def test_type_fake_builds_manager_with_defaults() -> None:
    cfg = _cfg(
        SandboxProviderConfig(
            type="fake",
            default_coding_agent="opencode",
            default_timeout_seconds=123.0,
            default_pause_ttl_seconds=456.0,
        )
    )
    mgr = build_sandbox_manager(cfg)
    assert isinstance(mgr, SandboxManager)
    assert mgr.provider_kind == "fake"
    # both coding-agent runners are registered
    assert mgr.runner_for("claude-code").name == "claude-code"
    assert mgr.runner_for("opencode").name == "opencode"
    # provider defaults flow into the spec
    spec = mgr.build_spec()
    assert spec.coding_agent == "opencode"
    assert spec.timeout_s == 123.0
    assert spec.pause_ttl_s == 456.0


def test_type_e2b_builds_real_provider() -> None:
    # The e2b SDK is imported lazily (only on create/connect), so building
    # the manager succeeds without the optional dependency installed.
    mgr = build_sandbox_manager(
        _cfg(SandboxProviderConfig(type="e2b", domain="local.example"))
    )
    assert isinstance(mgr, SandboxManager)
    assert mgr.provider_kind == "e2b"
    assert mgr.runner_for("claude-code").name == "claude-code"
