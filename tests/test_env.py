"""Tests for the shared CLI ``.env`` loader (``crewlet._env``)."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from crewlet._env import env_file_for, load_env_file


def test_load_env_file_beside_config(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A ``.env`` sitting next to the config file is loaded."""
    monkeypatch.delenv("CREWLET_ENV_PROBE", raising=False)
    cfg = tmp_path / "company.yaml"
    cfg.write_text("name: x\n")
    (tmp_path / ".env").write_text("CREWLET_ENV_PROBE=beside\n")

    load_env_file(cfg)

    assert os.environ.get("CREWLET_ENV_PROBE") == "beside"


def test_load_env_file_falls_back_to_cwd(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """With no ``.env`` beside the config, the CWD ``.env`` is used."""
    monkeypatch.delenv("CREWLET_ENV_PROBE", raising=False)
    work = tmp_path / "work"
    work.mkdir()
    monkeypatch.chdir(work)
    (work / ".env").write_text("CREWLET_ENV_PROBE=cwd\n")
    # Config lives elsewhere with no sibling .env.
    cfg_dir = tmp_path / "elsewhere"
    cfg_dir.mkdir()
    cfg = cfg_dir / "company.yaml"
    cfg.write_text("name: x\n")

    load_env_file(cfg)

    assert os.environ.get("CREWLET_ENV_PROBE") == "cwd"


def test_load_env_file_does_not_override_existing_env(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Real environment variables win over the ``.env`` file."""
    monkeypatch.setenv("CREWLET_ENV_PROBE", "from_shell")
    cfg = tmp_path / "company.yaml"
    cfg.write_text("name: x\n")
    (tmp_path / ".env").write_text("CREWLET_ENV_PROBE=from_dotenv\n")

    load_env_file(cfg)

    assert os.environ.get("CREWLET_ENV_PROBE") == "from_shell"


def test_load_env_file_missing_is_noop(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """No ``.env`` anywhere is a harmless no-op."""
    monkeypatch.delenv("CREWLET_ENV_PROBE", raising=False)
    work = tmp_path / "work"
    work.mkdir()
    monkeypatch.chdir(work)
    cfg = tmp_path / "company.yaml"
    cfg.write_text("name: x\n")

    load_env_file(cfg)  # must not raise

    assert os.environ.get("CREWLET_ENV_PROBE") is None


# ---------------------------------------------------------------------------
# env_file_for — the single decision of WHICH .env is canonical
# ---------------------------------------------------------------------------


def test_env_file_for_prefers_sibling(tmp_path: Path) -> None:
    cfg = tmp_path / "config.yaml"
    cfg.write_text("name: x\n")
    sibling = tmp_path / ".env"
    sibling.write_text("A=1\n")
    assert env_file_for(cfg) == sibling


def test_env_file_for_falls_back_to_cwd(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    cfg = tmp_path / "config.yaml"
    cfg.write_text("name: x\n")
    monkeypatch.chdir(tmp_path)
    # No sibling .env exists -> the cwd path, returned even though absent
    # so a writer can create exactly the file the loader will read.
    assert env_file_for(cfg) == Path(".env")


def test_env_file_for_matches_what_load_env_file_reads(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """The contract keeping writers and readers in sync.

    Whatever ``env_file_for`` names is exactly what ``load_env_file``
    loads. A provisioner writing to one file while ``crewlet run`` read
    another was invisible — every ``${VAR}`` resolved empty, with no error.
    """
    monkeypatch.delenv("CREWLET_ENV_SYNC_PROBE", raising=False)
    cfg_dir = tmp_path / "conf"
    cfg_dir.mkdir()
    cfg = cfg_dir / "config.yaml"
    cfg.write_text("name: x\n")
    monkeypatch.chdir(tmp_path)

    # A writer resolves the path first, then creates it.
    target = env_file_for(cfg)
    target.write_text("CREWLET_ENV_SYNC_PROBE=written\n")

    load_env_file(cfg)
    assert os.environ.get("CREWLET_ENV_SYNC_PROBE") == "written"
