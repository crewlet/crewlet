"""Tests for the Tier A BootstrapConfig loader."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from crewlet.config import (
    ApiAuthConfig,
    ApiAuthTokenConfig,
    ApiConfig,
    BootstrapConfig,
    BootstrapProvidersConfig,
    DatabaseConfig,
    KnowledgeStoreConfig,
    QueueConfig,
    load_bootstrap_config,
)

# -- model defaults --------------------------------------------------


def test_bootstrap_config_defaults() -> None:
    cfg = BootstrapConfig()
    assert cfg.debug is False
    assert cfg.api.host == "0.0.0.0"
    assert cfg.api.port == 0
    assert cfg.api.auth.disabled is False
    assert cfg.api.auth.tokens == []
    assert cfg.providers.queue.type == "pulsar"
    assert cfg.providers.database.dsn == ""
    assert cfg.providers.knowledge.type == "pgvector"


def test_bootstrap_config_extra_keys_rejected() -> None:
    """Stray keys at the top level fail closed."""
    with pytest.raises(ValueError, match="Extra inputs"):
        BootstrapConfig.model_validate({"unknown_key": "x"})


def test_api_auth_token_extra_keys_rejected() -> None:
    with pytest.raises(ValueError, match="Extra inputs"):
        ApiAuthTokenConfig.model_validate(
            {"id": "founder", "token": "x", "extra": True},
        )


# -- YAML loader -----------------------------------------------------


def test_load_bootstrap_config_minimal(tmp_path: Path) -> None:
    path = tmp_path / "config.yaml"
    path.write_text("debug: true\n")
    cfg = load_bootstrap_config(path)
    assert cfg.debug is True


def test_load_bootstrap_config_full(tmp_path: Path) -> None:
    path = tmp_path / "config.yaml"
    path.write_text(
        """
providers:
  queue:
    type: pulsar
    url: pulsar://localhost:6650
  database:
    dsn: postgresql://crewlet@localhost:5432/crewlet
  knowledge:
    type: pgvector
api:
  host: 127.0.0.1
  port: 8080
  auth:
    tokens:
      - id: founder
        token: secret-1
      - id: ops
        token: secret-2
debug: false
""",
    )
    cfg = load_bootstrap_config(path)
    assert cfg.api.host == "127.0.0.1"
    assert cfg.api.port == 8080
    assert len(cfg.api.auth.tokens) == 2
    assert cfg.api.auth.tokens[0].id == "founder"
    assert cfg.api.auth.tokens[0].token == "secret-1"
    assert cfg.providers.queue.url == "pulsar://localhost:6650"
    assert cfg.providers.database.dsn.startswith("postgresql://")


def test_load_bootstrap_config_resolves_env_vars(tmp_path: Path) -> None:
    """Tier A env-var refs (e.g. ``${CREWLET_API_TOKEN}``) resolve at load."""
    os.environ["TEST_DB_DSN"] = "postgresql://test"
    os.environ["TEST_TOKEN"] = "resolved-token"
    try:
        path = tmp_path / "config.yaml"
        path.write_text(
            """
providers:
  database:
    dsn: "${TEST_DB_DSN}"
api:
  auth:
    tokens:
      - id: ci
        token: "${TEST_TOKEN}"
""",
        )
        cfg = load_bootstrap_config(path)
        assert cfg.providers.database.dsn == "postgresql://test"
        assert cfg.api.auth.tokens[0].token == "resolved-token"
    finally:
        del os.environ["TEST_DB_DSN"]
        del os.environ["TEST_TOKEN"]


def test_load_bootstrap_config_file_not_found(tmp_path: Path) -> None:
    with pytest.raises(FileNotFoundError):
        load_bootstrap_config(tmp_path / "nope.yaml")


def test_load_bootstrap_config_rejects_non_mapping(tmp_path: Path) -> None:
    path = tmp_path / "config.yaml"
    path.write_text("- just a list\n")
    with pytest.raises(ValueError, match="mapping"):
        load_bootstrap_config(path)


def test_load_bootstrap_config_rejects_tier_b_keys(tmp_path: Path) -> None:
    """Tier B fields (`name`, `mission`, etc.) at top level should fail."""
    path = tmp_path / "config.yaml"
    path.write_text("name: Acme\nmission: build stuff\n")
    with pytest.raises(ValueError, match="Extra inputs"):
        load_bootstrap_config(path)


# -- composition / structural ---------------------------------------


def test_bootstrap_config_is_composable() -> None:
    """Models compose as expected for programmatic test construction."""
    cfg = BootstrapConfig(
        providers=BootstrapProvidersConfig(
            queue=QueueConfig(url="pulsar://test:6650"),
            database=DatabaseConfig(dsn="postgresql://x"),
            knowledge=KnowledgeStoreConfig(type="memory"),
        ),
        api=ApiConfig(
            host="127.0.0.1",
            port=9000,
            auth=ApiAuthConfig(
                tokens=[ApiAuthTokenConfig(id="t1", token="abc")],
            ),
        ),
        debug=True,
    )
    assert cfg.providers.knowledge.type == "memory"
    assert cfg.api.port == 9000
