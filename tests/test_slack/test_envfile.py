"""Tests for the provisioner's .env store (EnvStore + upsert writer)."""

from __future__ import annotations

import os
import stat
from pathlib import Path

import pytest
from dotenv import dotenv_values

from crewlet.slack.envfile import EnvStore, upsert_env_values, write_secret_file

# ---------------------------------------------------------------------------
# upsert_env_values — line surgery
# ---------------------------------------------------------------------------


def test_creates_file_with_values(tmp_path: Path) -> None:
    path = tmp_path / ".env"
    upsert_env_values(path, {"A": "1", "B": "two"})
    assert path.read_text() == "A=1\nB=two\n"


def test_owner_only_permissions_from_creation(tmp_path: Path) -> None:
    path = tmp_path / ".env"
    upsert_env_values(path, {"A": "1"})
    assert stat.S_IMODE(path.stat().st_mode) == 0o600


def test_replaces_in_place_preserving_rest(tmp_path: Path) -> None:
    path = tmp_path / ".env"
    path.write_text(
        "# comment stays\nKEEP=untouched\nexport TOKEN=old\nOTHER=x # trailing\n"
    )
    upsert_env_values(path, {"TOKEN": "new"})
    assert path.read_text() == (
        "# comment stays\nKEEP=untouched\nTOKEN=new\nOTHER=x # trailing\n"
    )


def test_appends_missing_keys(tmp_path: Path) -> None:
    path = tmp_path / ".env"
    path.write_text("EXISTING=1\n")
    upsert_env_values(path, {"NEW": "v"})
    assert path.read_text() == "EXISTING=1\nNEW=v\n"


def test_duplicate_assignments_collapse_to_one(tmp_path: Path) -> None:
    path = tmp_path / ".env"
    path.write_text("TOKEN=a\nMIDDLE=1\nTOKEN=b\n")
    upsert_env_values(path, {"TOKEN": "final"})
    assert path.read_text() == "TOKEN=final\nMIDDLE=1\n"


def test_prefix_key_not_confused(tmp_path: Path) -> None:
    """Updating TOKEN must not touch TOKEN_SUFFIX."""
    path = tmp_path / ".env"
    path.write_text("TOKEN_SUFFIX=keep\nTOKEN=old\n")
    upsert_env_values(path, {"TOKEN": "new"})
    assert path.read_text() == "TOKEN_SUFFIX=keep\nTOKEN=new\n"


def test_noop_on_empty_values(tmp_path: Path) -> None:
    path = tmp_path / ".env"
    upsert_env_values(path, {})
    assert not path.exists()


# ---------------------------------------------------------------------------
# Writer ↔ python-dotenv reader agreement (the engine reads with dotenv)
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "value",
    [
        "plain-token-123",
        "has space",
        'quo"te',
        "back\\slash",
        "hash#inside",
        "",
        "dollar$sign",
        "trailing$",
    ],
)
def test_round_trips_through_dotenv(tmp_path: Path, value: str) -> None:
    """Whatever we write, load_dotenv-style parsing must read back
    byte-identically — especially ``$`` content, which dotenv treats
    as interpolation syntax in most quoting forms."""
    path = tmp_path / ".env"
    upsert_env_values(path, {"KEY": value})
    assert dotenv_values(path)["KEY"] == value


@pytest.mark.parametrize("value", ["abc${USER}def", "both'$kinds"])
def test_unrepresentable_values_refused(tmp_path: Path, value: str) -> None:
    """python-dotenv interpolates ``${…}`` in EVERY quoting form (single
    quotes included), and a bare ``$`` next to a single quote defeats the
    one safe quoting — refuse loudly instead of persisting something the
    engine would read back differently."""
    path = tmp_path / ".env"
    with pytest.raises(ValueError, match="cannot faithfully store"):
        upsert_env_values(path, {"KEY": value})


# ---------------------------------------------------------------------------
# write_secret_file — atomicity
# ---------------------------------------------------------------------------


def test_write_secret_file_atomic_no_temp_left(tmp_path: Path) -> None:
    path = tmp_path / "ledger.json"
    write_secret_file(path, "one")
    write_secret_file(path, "two")
    assert path.read_text() == "two"
    assert list(tmp_path.iterdir()) == [path]
    assert stat.S_IMODE(path.stat().st_mode) == 0o600


# ---------------------------------------------------------------------------
# EnvStore — one path, file-wins precedence
# ---------------------------------------------------------------------------


def test_store_get_falls_back_to_process_env(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("BOOTSTRAP_ONLY", "from-shell")
    store = EnvStore(tmp_path / ".env")
    assert store.get("BOOTSTRAP_ONLY") == "from-shell"
    assert store.get("MISSING") == ""


def test_store_file_wins_over_stale_shell_export(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A rotated token persisted to the file must never be shadowed by a
    stale shell export — that shadowing bricked provisioning after the
    old access token's 12 h expiry."""
    path = tmp_path / ".env"
    path.write_text("SLACK_CONFIG_REFRESH_TOKEN=xoxe-1-fresh\n")
    monkeypatch.setenv("SLACK_CONFIG_REFRESH_TOKEN", "xoxe-1-stale-export")
    store = EnvStore(path)
    assert store.get("SLACK_CONFIG_REFRESH_TOKEN") == "xoxe-1-fresh"


async def test_store_set_persists_and_updates_view(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("KEY", "shell-value")
    store = EnvStore(tmp_path / ".env")
    await store.set({"KEY": "file-value"})
    assert store.get("KEY") == "file-value"
    # A fresh store over the same file sees the same value.
    assert EnvStore(tmp_path / ".env").get("KEY") == "file-value"
    # And os.environ was never mutated.
    assert os.environ["KEY"] == "shell-value"


def test_store_reads_existing_dotenv_forms(tmp_path: Path) -> None:
    path = tmp_path / ".env"
    path.write_text("PLAIN=x\nQUOTED=\"a b\"\nSINGLE='c$d'\nexport EXPORTED=y\n")
    store = EnvStore(path)
    assert store.get("PLAIN") == "x"
    assert store.get("QUOTED") == "a b"
    assert store.get("SINGLE") == "c$d"
    assert store.get("EXPORTED") == "y"


# ── SecretValueEnvStore — the DB-backed CredentialStore ──


class _FakeSecretStore:
    """Stands in for SecretValueStore — plaintext, in memory."""

    def __init__(self, initial: dict[str, str] | None = None) -> None:
        self.values = dict(initial or {})
        self.puts: list[tuple[str, str, str]] = []

    async def load_all(self) -> dict[str, str]:
        return dict(self.values)

    async def put(self, name, value, *, updated_by, source) -> None:
        self.values[name] = value
        self.puts.append((name, value, source))


async def test_secret_value_env_store_round_trips():
    from crewlet.slack.envfile import SecretValueEnvStore

    backing = _FakeSecretStore()
    store = SecretValueEnvStore(backing)
    await store.prime()
    assert store.get("SLACK_BOT_TOKEN_CEO") == ""

    await store.set({"SLACK_BOT_TOKEN_CEO": "xoxb-1"})
    # Visible immediately to this run, and committed to the backing store —
    # the whole point is that nothing has to be sourced afterwards.
    assert store.get("SLACK_BOT_TOKEN_CEO") == "xoxb-1"
    assert backing.values["SLACK_BOT_TOKEN_CEO"] == "xoxb-1"
    assert backing.puts == [("SLACK_BOT_TOKEN_CEO", "xoxb-1", "slack-provision")]


async def test_secret_value_env_store_wins_over_stale_shell_export(monkeypatch):
    """Same precedence rule as EnvStore, for the same reason.

    A stale `export SLACK_CONFIG_REFRESH_TOKEN=…` must never shadow the
    rotated pair a previous run persisted — with Slack's rotation
    semantics that shadowing bricks provisioning once the old access
    token expires.
    """
    from crewlet.slack.envfile import SecretValueEnvStore

    monkeypatch.setenv("SLACK_CONFIG_REFRESH_TOKEN", "stale-from-shell")
    monkeypatch.setenv("SLACK_CONFIG_TOKEN", "bootstrap-from-shell")
    store = SecretValueEnvStore(
        _FakeSecretStore({"SLACK_CONFIG_REFRESH_TOKEN": "rotated"})
    )
    await store.prime()
    assert store.get("SLACK_CONFIG_REFRESH_TOKEN") == "rotated"
    # ...but the shell is still the bootstrap fallback for keys the store
    # has never held, which is how the operator seeds the first token.
    assert store.get("SLACK_CONFIG_TOKEN") == "bootstrap-from-shell"


async def test_secret_value_env_store_empty_set_is_a_noop():
    from crewlet.slack.envfile import SecretValueEnvStore

    backing = _FakeSecretStore()
    store = SecretValueEnvStore(backing)
    await store.prime()
    await store.set({})
    assert backing.puts == []
