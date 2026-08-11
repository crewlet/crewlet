"""The process-local secret source and its precedence over ``os.environ``."""

from __future__ import annotations

import pytest

from crewlet.config import _resolve_env_recursive, _resolve_env_value, resolve_env_vars
from crewlet.secrets.resolver import (
    active_secret_source,
    install_secret_source,
    lookup_secret,
    refresh_secret_snapshot,
    resolve_env,
)


class FakeSource:
    """Minimal :class:`SecretSource` with the optional ``names`` hook."""

    def __init__(self, values: dict[str, str]) -> None:
        self.values = dict(values)
        self.refreshes = 0

    def get(self, name: str) -> str | None:
        return self.values.get(name)

    def names(self) -> list[str]:
        return sorted(self.values)

    async def refresh(self) -> None:
        self.refreshes += 1


@pytest.fixture(autouse=True)
def _uninstall_after_each():
    """No test may leak a source into the next — it is a process global."""
    yield
    install_secret_source(None)


# ── inert by default ──


def test_no_source_installed_is_byte_identical_to_os_environ(monkeypatch):
    # The whole feature must be a no-op for every deployment that has
    # never stored a secret.
    install_secret_source(None)
    monkeypatch.setenv("PROBE_A", "from-env")
    monkeypatch.delenv("PROBE_MISSING", raising=False)
    assert _resolve_env_value("${PROBE_A}") == "from-env"
    assert _resolve_env_value("${PROBE_MISSING}") == ""
    assert lookup_secret("PROBE_A") is None
    assert active_secret_source() is None


# ── precedence ──


def test_store_wins_over_environment(monkeypatch):
    # Env-first would let a stale .env shadow a freshly rotated secret —
    # exactly the failure the store exists to remove.
    monkeypatch.setenv("SHARED", "stale-from-env")
    install_secret_source(FakeSource({"SHARED": "fresh-from-store"}))
    assert _resolve_env_value("${SHARED}") == "fresh-from-store"


def test_environment_is_the_fallback(monkeypatch):
    monkeypatch.setenv("ONLY_ENV", "env-value")
    install_secret_source(FakeSource({"OTHER": "x"}))
    assert _resolve_env_value("${ONLY_ENV}") == "env-value"


def test_missing_everywhere_still_resolves_to_empty(monkeypatch):
    monkeypatch.delenv("NOWHERE", raising=False)
    install_secret_source(FakeSource({"OTHER": "x"}))
    assert _resolve_env_value("${NOWHERE}") == ""


def test_stored_empty_value_is_authoritative(monkeypatch):
    # "" from the store means "this is deliberately empty", not a miss —
    # otherwise clearing a secret would silently fall back to a stale
    # environment value.
    monkeypatch.setenv("CLEARED", "stale-from-env")
    install_secret_source(FakeSource({"CLEARED": ""}))
    assert _resolve_env_value("${CLEARED}") == ""


def test_embedded_reference_resolves_from_store(monkeypatch):
    monkeypatch.delenv("HDR", raising=False)
    install_secret_source(FakeSource({"HDR": "abc123"}))
    assert _resolve_env_value("Bearer ${HDR}") == "Bearer abc123"


def test_wrappers_all_see_the_store(monkeypatch):
    # Every wrapper funnels through _resolve_env_value, so installing one
    # source covers env dicts, scalars and nested structures alike.
    monkeypatch.delenv("W", raising=False)
    install_secret_source(FakeSource({"W": "stored"}))
    assert resolve_env_vars({"K": "${W}"}) == {"K": "stored"}
    assert _resolve_env_recursive({"a": ["${W}"]}) == {"a": ["stored"]}


# ── the Tier A root-of-trust boundary ──


def test_use_store_false_bypasses_the_store(monkeypatch):
    # Tier A carries the DSN and the keyring — i.e. what is needed to
    # OPEN the store — so it can never source a value from it.
    monkeypatch.setenv("DSN_VAR", "postgresql://from-env/db")
    install_secret_source(FakeSource({"DSN_VAR": "postgresql://from-store/db"}))
    assert _resolve_env_value("${DSN_VAR}", use_store=False) == (
        "postgresql://from-env/db"
    )
    assert _resolve_env_recursive({"dsn": "${DSN_VAR}"}, use_store=False) == {
        "dsn": "postgresql://from-env/db"
    }


def test_bootstrap_load_does_not_consult_the_store(monkeypatch, tmp_path):
    """The boundary end-to-end: ``load_bootstrap_config`` ignores the store."""
    from crewlet.config import load_bootstrap_config

    monkeypatch.setenv("BOOT_DSN", "postgresql://env/db")
    install_secret_source(FakeSource({"BOOT_DSN": "postgresql://store/db"}))
    path = tmp_path / "config.yaml"
    path.write_text("providers:\n  database:\n    dsn: '${BOOT_DSN}'\n")
    assert load_bootstrap_config(path).providers.database.dsn == "postgresql://env/db"


# ── shadow reporting ──


def test_install_warns_when_store_shadows_env(monkeypatch, caplog):
    import logging

    monkeypatch.setenv("SHADOWED", "old")
    monkeypatch.setenv("AGREES", "same")
    with caplog.at_level(logging.WARNING):
        install_secret_source(
            FakeSource({"SHADOWED": "new", "AGREES": "same", "STORE_ONLY": "x"})
        )
    text = caplog.text
    assert "secret_shadowed_env" in text
    assert "SHADOWED" in text
    # Only genuine disagreements, and never a value.
    assert "AGREES" not in text
    assert "STORE_ONLY" not in text
    assert "old" not in text
    assert "new" not in text


def test_install_is_quiet_when_nothing_is_shadowed(monkeypatch, caplog):
    import logging

    monkeypatch.delenv("QUIET_ONE", raising=False)
    with caplog.at_level(logging.WARNING):
        install_secret_source(FakeSource({"QUIET_ONE": "v"}))
    assert "secret_shadowed_env" not in caplog.text


# ── refresh ──


async def test_refresh_delegates_to_the_source():
    source = FakeSource({"A": "1"})
    install_secret_source(source)
    await refresh_secret_snapshot()
    assert source.refreshes == 1


async def test_refresh_without_a_source_is_a_noop():
    install_secret_source(None)
    await refresh_secret_snapshot()  # must not raise


async def test_refresh_tolerates_a_source_without_refresh():
    class Opaque:
        def get(self, name: str) -> str | None:
            return None

    install_secret_source(Opaque())
    await refresh_secret_snapshot()  # must not raise


# ── resolve_env: the by-name read used by conventional-key fallbacks ──


def test_resolve_env_prefers_store_then_environment(monkeypatch):
    monkeypatch.setenv("BY_NAME", "env")
    install_secret_source(FakeSource({"BY_NAME": "store"}))
    assert resolve_env("BY_NAME") == "store"

    install_secret_source(FakeSource({}))
    assert resolve_env("BY_NAME") == "env"


def test_resolve_env_default_when_nowhere(monkeypatch):
    monkeypatch.delenv("ABSENT_NAME", raising=False)
    install_secret_source(FakeSource({}))
    assert resolve_env("ABSENT_NAME") == ""
    assert resolve_env("ABSENT_NAME", "fallback") == "fallback"


def test_llm_provider_conventional_key_may_come_from_the_store(monkeypatch):
    """A store-only credential must satisfy the conventional-var fallback.

    Otherwise ``crewlet secrets set OPENAI_API_KEY`` would work through a
    ``${VAR}`` reference but not through the bare fallback — the kind of
    inconsistency that reads as a bug.
    """
    from crewlet.config import (
        LLMProviderConfig,
        ProvidersConfig,
        create_llm_providers,
    )

    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    cfg = ProvidersConfig(
        llm={"default": LLMProviderConfig(type="openai", api_keys=[])}
    )

    # Nothing anywhere -> the clear failure stays a failure.
    install_secret_source(FakeSource({}))
    with pytest.raises(ValueError, match="has no API key"):
        create_llm_providers(cfg)

    # Stored -> accepted, and the provider actually picks the key up.
    install_secret_source(FakeSource({"OPENAI_API_KEY": "sk-from-store"}))
    providers = create_llm_providers(cfg)
    assert providers["default"].api_key == "sk-from-store"


def test_embedding_provider_conventional_key_may_come_from_the_store(monkeypatch):
    from crewlet.providers.embeddings.openai import OpenAIEmbeddingProvider

    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    install_secret_source(FakeSource({"OPENAI_API_KEY": "sk-emb-store"}))
    assert OpenAIEmbeddingProvider().api_key == "sk-emb-store"
