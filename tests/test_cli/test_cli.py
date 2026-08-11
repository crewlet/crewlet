"""Tests for the crewlet CLI."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import pytest

from crewlet.cli import main

FIXTURES = Path(__file__).parent.parent / "fixtures"
MINIMAL_YAML = FIXTURES / "minimal.yaml"
COMPANY_YAML = FIXTURES / "company.yaml"


class TestVersion:
    def test_version_flag(self, capsys: pytest.CaptureFixture[str]) -> None:
        with pytest.raises(SystemExit, match="0"):
            main(["--version"])
        assert "crewlet" in capsys.readouterr().out

    def test_short_version_flag(self, capsys: pytest.CaptureFixture[str]) -> None:
        with pytest.raises(SystemExit, match="0"):
            main(["-V"])


class TestNoCommand:
    def test_no_command_prints_help(self, capsys: pytest.CaptureFixture[str]) -> None:
        result = main([])
        assert result == 0
        out = capsys.readouterr().out
        assert "usage" in out.lower() or "crewlet" in out.lower()


class TestValidate:
    def test_validate_minimal(self, capsys: pytest.CaptureFixture[str]) -> None:
        result = main(["validate", str(MINIMAL_YAML)])
        assert result == 0
        out = capsys.readouterr().out
        assert "Config OK" in out

    def test_validate_company(self, capsys: pytest.CaptureFixture[str]) -> None:
        result = main(["validate", str(COMPANY_YAML)])
        assert result == 0
        out = capsys.readouterr().out
        assert "Config OK" in out
        assert "Roles" in out

    def test_validate_missing_file(self, capsys: pytest.CaptureFixture[str]) -> None:
        result = main(["validate", "/nonexistent/config.yaml"])
        assert result == 1

    def test_validate_bad_yaml(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        bad = tmp_path / "bad.yaml"
        bad.write_text("departments: not_a_list")
        result = main(["validate", str(bad)])
        assert result == 1


class TestRun:
    def test_run_missing_file(self, capsys: pytest.CaptureFixture[str]) -> None:
        result = main(["run", "/nonexistent/config.yaml"])
        assert result == 1
        assert "not found" in capsys.readouterr().err

    def test_run_bad_yaml(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        bad = tmp_path / "bad.yaml"
        bad.write_text("departments: not_a_list")
        result = main(["run", str(bad)])
        assert result == 1
        assert "invalid bootstrap config" in capsys.readouterr().err.lower()

    def test_run_import_confluence_requires_import_company(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        """--import-confluence needs the Tier B company config (for the
        Confluence integration); without --import-company it errors
        clearly instead of mis-loading the Tier A bootstrap."""
        bootstrap = tmp_path / "config.yaml"
        bootstrap.write_text("{}\n")
        docs = tmp_path / "docs"
        docs.mkdir()
        (docs / "a.md").write_text("# a\n")
        result = main(["run", str(bootstrap), "--import-confluence", str(docs)])
        assert result == 1
        assert "requires --import-company" in capsys.readouterr().err

    def test_run_import_confluence_uses_company_config_not_bootstrap(
        self,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        """The bundled import is handed the --import-company file
        (Tier B, has the Confluence block), not the Tier A bootstrap."""
        import crewlet.confluence.import_cli as ic

        recorded: dict[str, Path] = {}

        async def fake_run_import(*, config_path: Path, **_kw: object) -> int:
            recorded["config_path"] = config_path
            raise RuntimeError("stop-after-record")

        monkeypatch.setattr(ic, "run_import", fake_run_import)
        bootstrap = tmp_path / "config.yaml"
        bootstrap.write_text("{}\n")
        docs = tmp_path / "docs"
        docs.mkdir()
        (docs / "a.md").write_text("# a\n")
        result = main(
            [
                "run",
                str(bootstrap),
                "--import-confluence",
                str(docs),
                "--import-company",
                str(COMPANY_YAML),
            ]
        )
        assert result == 1
        assert recorded["config_path"] == COMPANY_YAML
        assert "stop-after-record" in capsys.readouterr().err

    def test_run_import_confluence_runs_without_tier_a_config(
        self,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        """The bundled Confluence import is self-contained — its creds come
        from --import-company, not the Tier A bootstrap — so it must run
        even when ``config.yaml`` is absent. If the import sat
        behind the bootstrap existence/validity gate, a missing or
        invalid Tier A config would silently skip the import (and thus
        --update-confluence) entirely."""
        import crewlet.confluence.import_cli as ic

        recorded: dict[str, object] = {}

        async def fake_run_import(
            *, config_path: Path, update: bool = False, **_kw: object
        ) -> int:
            recorded["config_path"] = config_path
            recorded["update"] = update
            return 0

        monkeypatch.setattr(ic, "run_import", fake_run_import)
        missing_bootstrap = tmp_path / "config.yaml"  # never created
        docs = tmp_path / "docs"
        docs.mkdir()
        (docs / "a.md").write_text("# a\n")
        result = main(
            [
                "run",
                str(missing_bootstrap),
                "--import-company",
                str(COMPANY_YAML),
                "--import-confluence",
                str(docs),
                "--update-confluence",
            ]
        )
        # The import ran (not gated) and --update-confluence threaded through.
        assert recorded["config_path"] == COMPANY_YAML
        assert recorded["update"] is True
        # The engine still needs a Tier A bootstrap to actually run, so the
        # command exits non-zero with that error — but only *after* the
        # import has already published.
        assert result == 1
        assert "bootstrap config not found" in capsys.readouterr().err

    def test_run_import_plane_requires_import_company(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        """--import-plane needs the Tier B company config (for the Plane
        integration); without --import-company it errors clearly instead
        of mis-loading the Tier A bootstrap."""
        bootstrap = tmp_path / "config.yaml"
        bootstrap.write_text("{}\n")
        docs = tmp_path / "docs"
        docs.mkdir()
        (docs / "a.md").write_text("# a\n")
        result = main(["run", str(bootstrap), "--import-plane", str(docs)])
        assert result == 1
        assert "requires --import-company" in capsys.readouterr().err

    def test_run_import_plane_threads_company_config_and_flags(
        self,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        """The bundled Plane import is handed the --import-company file
        and the --update-plane / --prune-plane flags thread through."""
        import crewlet.plane.import_cli as pic

        recorded: dict[str, object] = {}

        async def fake_run_import(
            *,
            config_path: Path,
            update: bool = False,
            prune: bool = False,
            **_kw: object,
        ) -> int:
            recorded["config_path"] = config_path
            recorded["update"] = update
            recorded["prune"] = prune
            return 0

        monkeypatch.setattr(pic, "run_import", fake_run_import)
        missing_bootstrap = tmp_path / "config.yaml"  # never created
        docs = tmp_path / "docs"
        docs.mkdir()
        (docs / "a.md").write_text("# a\n")
        result = main(
            [
                "run",
                str(missing_bootstrap),
                "--import-company",
                str(COMPANY_YAML),
                "--import-plane",
                str(docs),
                "--update-plane",
                "--prune-plane",
            ]
        )
        # The import ran (self-contained, before the Tier A gate) with
        # the flags threaded through; the engine then still needs Tier A.
        assert recorded["config_path"] == COMPANY_YAML
        assert recorded["update"] is True
        assert recorded["prune"] is True
        assert result == 1
        assert "bootstrap config not found" in capsys.readouterr().err

    def test_run_import_confluence_and_plane_are_mutually_exclusive(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        """One knowledge backend per run: argparse rejects the pair
        BEFORE either import gets to publish anything."""
        docs = tmp_path / "docs"
        docs.mkdir()
        (docs / "a.md").write_text("# a\n")
        with pytest.raises(SystemExit, match="2"):
            main(
                [
                    "run",
                    str(tmp_path / "config.yaml"),
                    "--import-confluence",
                    str(docs),
                    "--import-plane",
                    str(docs),
                ]
            )
        assert "not allowed with argument" in capsys.readouterr().err


class TestPlaneCommands:
    """`crewlet plane import|resync` — parse + dispatch."""

    def test_plane_no_subcommand_prints_help(
        self, capsys: pytest.CaptureFixture[str]
    ) -> None:
        result = main(["plane"])
        assert result == 0
        assert "usage" in capsys.readouterr().out.lower()

    def test_plane_import_dispatches_with_parsed_args(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        import crewlet.plane.import_cli as pic

        recorded: dict[str, object] = {}

        def fake_cmd(args: object) -> int:
            recorded["config"] = args.config  # type: ignore[attr-defined]
            recorded["path"] = args.path  # type: ignore[attr-defined]
            recorded["project"] = args.project  # type: ignore[attr-defined]
            recorded["update"] = args.update  # type: ignore[attr-defined]
            recorded["dry_run"] = args.dry_run  # type: ignore[attr-defined]
            recorded["prune"] = args.prune  # type: ignore[attr-defined]
            return 0

        monkeypatch.setattr(pic, "cmd_plane_import", fake_cmd)
        cfg = tmp_path / "company.yaml"
        docs = tmp_path / "docs"
        result = main(
            [
                "plane",
                "import",
                str(cfg),
                str(docs),
                "--project",
                "SKILLS",
                "--update",
                "--dry-run",
                "--prune",
            ]
        )
        assert result == 0
        assert recorded == {
            "config": cfg,
            "path": docs,
            "project": "SKILLS",
            "update": True,
            "dry_run": True,
            "prune": True,
        }

    def test_plane_resync_dispatches_with_parsed_args(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        import crewlet.plane.import_cli as pic

        recorded: dict[str, object] = {}

        def fake_cmd(args: object) -> int:
            recorded["config"] = args.config  # type: ignore[attr-defined]
            recorded["project"] = args.project  # type: ignore[attr-defined]
            return 0

        monkeypatch.setattr(pic, "cmd_plane_resync", fake_cmd)
        cfg = tmp_path / "company.yaml"
        result = main(["plane", "resync", str(cfg), "--project", "TS"])
        assert result == 0
        assert recorded == {"config": cfg, "project": "TS"}


class TestPythonM:
    def test_python_m_crewlet(self) -> None:
        result = subprocess.run(
            [sys.executable, "-m", "crewlet", "--version"],
            capture_output=True,
            text=True,
        )
        assert result.returncode == 0
        assert "crewlet" in result.stdout


class TestSecretsKeygen:
    """`crewlet secrets keygen` — key generation CLI."""

    def test_keygen_prints_snippet(self, capsys) -> None:
        from crewlet.cli import main

        result = main(["secrets", "keygen", "--key-id", "2026-01"])
        assert result == 0
        out = capsys.readouterr().out
        assert "secrets:" in out
        assert "active_key_id: 2026-01" in out
        # The printed material must be a usable 32-byte base64 key.
        import base64

        for line in out.splitlines():
            if "export CREWLET_SECRET_KEY" in line:
                material = line.split("=", 1)[1].strip()
                assert len(base64.b64decode(material)) == 32
                break
        else:
            raise AssertionError("no export line in keygen output")

    def test_secrets_no_subcommand_prints_help(self, capsys) -> None:
        from crewlet.cli import main

        assert main(["secrets"]) == 0
