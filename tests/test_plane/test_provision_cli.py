"""Tests for the ``crewlet plane provision`` CLI wiring.

Mirrors ``tests/test_gitlab/test_provision_cli.py`` plus the Plane-only
concerns: registration on the EXISTING ``plane`` group (import / resync
/ provision all dispatch), the first-run webhook-secret
chicken-and-egg message, per-field url/workspace resolution naming
unset ``${VAR}``s, the operator-token fallback chain, the
``--token-expiry-days`` flag-overrides-config precedence, and the
report printer (seat lines, hooks, notes, member table, the
source-AND-restart reminder).
"""

from __future__ import annotations

import os
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from crewlet.cli import _build_parser, main
from crewlet.plane.client import PlaneProvisionError
from crewlet.plane.provision import (
    PlaneProvisionAborted,
    ProvisionReport,
    SeatResult,
)
from crewlet.plane.provision_cli import (
    PlaneConfigError,
    _print_report,
    _resolve_plane_provision_config,
    _unset_env_refs,
    cmd_plane_provision,
)
from crewlet.provisioning import EnvFileSink, PrintSink

FOUNDER_ID = "11111111-1111-1111-1111-111111111111"


@pytest.fixture(autouse=True)
def _plane_env_isolation():
    """Keep PLANE_* env vars out of (and cleaned up after) every test.

    The CLI reads ``$PLANE_PROVISION_TOKEN`` and ``load_env_file`` pulls
    a sibling ``.env`` into the process environment, so leakage between
    tests would silently flip credential-resolution decisions.
    """
    saved = {k: v for k, v in os.environ.items() if k.startswith("PLANE_")}
    for key in saved:
        del os.environ[key]
    yield
    for key in [k for k in os.environ if k.startswith("PLANE_")]:
        del os.environ[key]
    os.environ.update(saved)


# ── config fixtures ──


_PLANE_BLOCK = (
    "integrations:\n"
    "  plane:\n"
    "    enabled: true\n"
    '    url: "https://plane.test"\n'
    "    workspace: testco\n"
    '    webhook_secret: "${PLANE_WEBHOOK_SECRET}"\n'
    '    token: "${PLANE_ENGINE_TOKEN}"\n'
)

_ROLES_BLOCK = (
    "roles:\n"
    "  - name: Engineer\n"
    "    mcp_env:\n"
    "      plane:\n"
    '        PLANE_API_KEY: "${PLANE_TOKEN_ENG}"\n'
)


def _write_company(tmp_path: Path, body: str) -> Path:
    path = tmp_path / "company.yaml"
    path.write_text(f"name: T\n{body}")
    return path


def _args(config: Path, *argv: str) -> Any:
    """Build args through the real parser — registration coverage too."""
    return _build_parser().parse_args(["plane", "provision", str(config), *argv])


class _Capture:
    """Records what ``cmd_plane_provision`` handed to ``provision()``."""

    def __init__(self, report: ProvisionReport | None = None) -> None:
        self.report = report or ProvisionReport()
        self.client: Any = None
        self.org: Any = None
        self.plane: Any = None
        self.kwargs: dict[str, Any] = {}
        self.calls = 0

    def fake_provision(self):
        async def _fake(client: Any, org: Any, plane_config: Any, **kwargs: Any):
            self.calls += 1
            self.client = client
            self.org = org
            self.plane = plane_config
            self.kwargs = kwargs
            return self.report

        return _fake


@pytest.fixture
def capture(monkeypatch: pytest.MonkeyPatch) -> _Capture:
    cap = _Capture()
    monkeypatch.setattr("crewlet.plane.provision_cli.provision", cap.fake_provision())
    return cap


# ── parser registration on the existing plane group ──


def test_parser_defaults(tmp_path: Path) -> None:
    args = _args(tmp_path / "c.yaml")
    assert args.command == "plane"
    assert args.plane_command == "provision"
    assert args.provision_token is None
    assert args.webhook_url is None
    assert args.env_file == Path(".env.plane")
    assert args.print_tokens is False
    assert args.rotate is False
    assert args.decommission_removed is False
    assert args.recreate_webhook is False
    assert args.create_projects is False
    assert args.token_expiry_days is None  # None ⇒ config default applies


def test_parser_all_flags(tmp_path: Path) -> None:
    args = _args(
        tmp_path / "c.yaml",
        "--provision-token",
        "tok",
        "--webhook-url",
        "https://engine.test/webhooks/plane",
        "--env-file",
        str(tmp_path / "custom.env"),
        "--print",
        "--rotate",
        "--decommission-removed",
        "--recreate-webhook",
        "--create-projects",
        "--token-expiry-days",
        "30",
    )
    assert args.provision_token == "tok"
    assert args.webhook_url == "https://engine.test/webhooks/plane"
    assert args.env_file == tmp_path / "custom.env"
    assert args.print_tokens is True
    assert args.rotate is True
    assert args.decommission_removed is True
    assert args.recreate_webhook is True
    assert args.create_projects is True
    assert args.token_expiry_days == 30


def test_plane_group_help_mentions_provisioning() -> None:
    # Normalize whitespace — argparse wraps help text at terminal width.
    help_text = " ".join(_build_parser().format_help().split())
    assert "provision service accounts" in help_text


def test_plane_group_dispatches_all_three(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """import / resync / provision all route through the plane group."""
    monkeypatch.setattr(
        "crewlet.plane.provision_cli.cmd_plane_provision", lambda args: 7
    )
    monkeypatch.setattr("crewlet.plane.import_cli.cmd_plane_import", lambda args: 8)
    monkeypatch.setattr("crewlet.plane.import_cli.cmd_plane_resync", lambda args: 9)
    cfg = str(tmp_path / "c.yaml")
    assert main(["plane", "provision", cfg]) == 7
    assert main(["plane", "import", cfg]) == 8
    assert main(["plane", "resync", cfg]) == 9


# ── _unset_env_refs / _resolve_plane_provision_config ──


def test_unset_env_refs_finds_empty_and_missing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv("PLANE_URL_UNSET", raising=False)
    monkeypatch.setenv("PLANE_WS_EMPTY", "")  # set-but-empty counts
    monkeypatch.setenv("PLANE_SET", "value")
    data = {
        "url": "${PLANE_URL_UNSET}",
        "workspace": "${PLANE_WS_EMPTY}",
        "nested": [{"k": "${PLANE_SET}"}],
    }
    assert _unset_env_refs(data) == ["PLANE_URL_UNSET", "PLANE_WS_EMPTY"]


def test_resolve_returns_resolved_url_and_workspace(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("PLANE_URL", "https://plane.test")
    raw = SimpleNamespace(url="${PLANE_URL}", workspace="testco")
    target = _resolve_plane_provision_config(raw)
    assert target.url == "https://plane.test"
    assert target.workspace == "testco"


def test_resolve_names_unset_vars() -> None:
    raw = SimpleNamespace(url="${PLANE_URL}", workspace="${PLANE_WS}")
    with pytest.raises(PlaneConfigError) as excinfo:
        _resolve_plane_provision_config(raw)
    msg = str(excinfo.value)
    assert "url and workspace" in msg
    assert "PLANE_URL" in msg
    assert "PLANE_WS" in msg


# ── config validation paths ──


def test_config_file_missing(tmp_path: Path, capsys: pytest.CaptureFixture) -> None:
    assert cmd_plane_provision(_args(tmp_path / "nope.yaml")) == 1
    assert "not found" in capsys.readouterr().out


def test_not_enabled(tmp_path: Path, capsys: pytest.CaptureFixture) -> None:
    cfg = _write_company(tmp_path, "integrations:\n  plane:\n    enabled: false\n")
    assert cmd_plane_provision(_args(cfg)) == 1
    assert "not enabled" in capsys.readouterr().out


def test_no_plane_block(tmp_path: Path, capsys: pytest.CaptureFixture) -> None:
    cfg = _write_company(tmp_path, "")
    assert cmd_plane_provision(_args(cfg)) == 1
    assert "not enabled" in capsys.readouterr().out


def test_first_run_empty_webhook_secret_prints_reference_remediation(
    tmp_path: Path, capsys: pytest.CaptureFixture
) -> None:
    """An enabled block with NO webhook_secret trips PlaneConfig's
    required-field validator at load — before the provisioner that would
    supply the secret ever runs.  The CLI must translate that into the
    set-a-${VAR}-REFERENCE remediation, not a pydantic traceback."""
    cfg = _write_company(
        tmp_path,
        "integrations:\n"
        "  plane:\n"
        "    enabled: true\n"
        '    url: "https://plane.test"\n'
        "    workspace: testco\n",
    )
    assert cmd_plane_provision(_args(cfg)) == 1
    out = capsys.readouterr().out
    assert "PLANE_WEBHOOK_SECRET" in out
    assert "REFERENCE" in out
    assert "provision" in out


def test_other_config_error_prints_plainly(
    tmp_path: Path, capsys: pytest.CaptureFixture
) -> None:
    cfg = tmp_path / "company.yaml"
    cfg.write_text("mission: no name field\n")
    assert cmd_plane_provision(_args(cfg)) == 1
    assert "Company config is invalid" in capsys.readouterr().out


def test_unset_url_workspace_vars_named(
    tmp_path: Path, capsys: pytest.CaptureFixture
) -> None:
    cfg = _write_company(
        tmp_path,
        "integrations:\n"
        "  plane:\n"
        "    enabled: true\n"
        '    url: "${PLANE_URL}"\n'
        '    workspace: "${PLANE_WS}"\n'
        '    webhook_secret: "${PLANE_WEBHOOK_SECRET}"\n',
    )
    assert cmd_plane_provision(_args(cfg)) == 1
    out = capsys.readouterr().out
    assert "PLANE_URL" in out
    assert "PLANE_WS" in out


# ── operator-token fallback chain ──


def test_provision_token_flag_wins(
    tmp_path: Path, capture: _Capture, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("PLANE_PROVISION_TOKEN", "env-tok")
    cfg = _write_company(tmp_path, _PLANE_BLOCK + _ROLES_BLOCK)
    assert cmd_plane_provision(_args(cfg, "--provision-token", "flag-tok")) == 0
    assert capture.client._token == "flag-tok"
    assert capture.client._base_url == "https://plane.test"
    assert capture.client._workspace == "testco"


def test_provision_token_env_fallback(
    tmp_path: Path, capture: _Capture, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("PLANE_PROVISION_TOKEN", "env-tok")
    cfg = _write_company(tmp_path, _PLANE_BLOCK)
    assert cmd_plane_provision(_args(cfg)) == 0
    assert capture.client._token == "env-tok"


def test_provision_token_from_sibling_dotenv(tmp_path: Path, capture: _Capture) -> None:
    """The ``_env`` contract: a .env beside the config is honoured."""
    (tmp_path / ".env").write_text("PLANE_PROVISION_TOKEN=dotenv-tok\n")
    cfg = _write_company(tmp_path, _PLANE_BLOCK)
    assert cmd_plane_provision(_args(cfg)) == 0
    assert capture.client._token == "dotenv-tok"


def test_no_credential_errors(
    tmp_path: Path, capture: _Capture, capsys: pytest.CaptureFixture
) -> None:
    cfg = _write_company(tmp_path, _PLANE_BLOCK)
    assert cmd_plane_provision(_args(cfg)) == 1
    out = capsys.readouterr().out
    assert "PLANE_PROVISION_TOKEN" in out
    assert "workspace-admin" in out
    assert capture.calls == 0


# ── argument passthrough into provision() ──


def test_kwargs_passthrough_and_raw_refs(tmp_path: Path, capture: _Capture) -> None:
    cfg = _write_company(tmp_path, _PLANE_BLOCK + _ROLES_BLOCK)
    ret = cmd_plane_provision(
        _args(
            cfg,
            "--provision-token",
            "tok",
            "--webhook-url",
            "https://engine.test/webhooks/plane",
            "--rotate",
            "--decommission-removed",
            "--create-projects",
            "--recreate-webhook",
        )
    )
    assert ret == 0
    kwargs = capture.kwargs
    assert kwargs["webhook_url"] == "https://engine.test/webhooks/plane"
    assert kwargs["rotate"] is True
    assert kwargs["decommission_removed"] is True
    assert kwargs["create_projects"] is True
    assert kwargs["recreate_webhook"] is True
    # token + webhook_secret stay RAW — their ${VAR} references ARE the
    # minting contract.
    assert kwargs["engine_token_ref"] == "${PLANE_ENGINE_TOKEN}"
    assert kwargs["webhook_secret_ref"] == "${PLANE_WEBHOOK_SECRET}"
    # The org and the raw plane block flow through unresolved.
    assert capture.plane.provisioning is None
    handles = [r.get_handle() for r in capture.org.all_roles()]
    assert "engineer" in handles


def test_webhook_url_omitted_is_empty_string(tmp_path: Path, capture: _Capture) -> None:
    cfg = _write_company(tmp_path, _PLANE_BLOCK)
    assert cmd_plane_provision(_args(cfg, "--provision-token", "tok")) == 0
    assert capture.kwargs["webhook_url"] == ""


# ── sink selection ──


def test_default_sink_is_env_file(tmp_path: Path, capture: _Capture) -> None:
    cfg = _write_company(tmp_path, _PLANE_BLOCK)
    env_file = tmp_path / ".env.plane"
    ret = cmd_plane_provision(
        _args(cfg, "--provision-token", "tok", "--env-file", str(env_file))
    )
    assert ret == 0
    sink = capture.kwargs["sink"]
    assert isinstance(sink, EnvFileSink)
    assert sink._path == str(env_file)


def test_print_flag_selects_print_sink(tmp_path: Path, capture: _Capture) -> None:
    cfg = _write_company(tmp_path, _PLANE_BLOCK)
    assert cmd_plane_provision(_args(cfg, "--provision-token", "tok", "--print")) == 0
    assert isinstance(capture.kwargs["sink"], PrintSink)


# ── --token-expiry-days precedence ──


def test_expiry_defaults_to_config_field(tmp_path: Path, capture: _Capture) -> None:
    cfg = _write_company(
        tmp_path, _PLANE_BLOCK + "    provisioning:\n      token_expiry_days: 30\n"
    )
    assert cmd_plane_provision(_args(cfg, "--provision-token", "tok")) == 0
    assert capture.kwargs["token_expiry_days"] == 30


def test_expiry_flag_overrides_config(tmp_path: Path, capture: _Capture) -> None:
    cfg = _write_company(
        tmp_path, _PLANE_BLOCK + "    provisioning:\n      token_expiry_days: 30\n"
    )
    ret = cmd_plane_provision(
        _args(cfg, "--provision-token", "tok", "--token-expiry-days", "7")
    )
    assert ret == 0
    assert capture.kwargs["token_expiry_days"] == 7


def test_expiry_flag_zero_is_a_real_override(tmp_path: Path, capture: _Capture) -> None:
    """0 means never-expires (Plane semantics) — it must not be read as
    'unset' and fall back to the config value."""
    cfg = _write_company(
        tmp_path, _PLANE_BLOCK + "    provisioning:\n      token_expiry_days: 30\n"
    )
    ret = cmd_plane_provision(
        _args(cfg, "--provision-token", "tok", "--token-expiry-days", "0")
    )
    assert ret == 0
    assert capture.kwargs["token_expiry_days"] == 0


def test_negative_expiry_days_rejected_by_the_parser(tmp_path: Path) -> None:
    """A negative --token-expiry-days would silently mean "never expires"
    (the exact inverse of the operator's intent), so argparse rejects it
    before any code runs."""
    with pytest.raises(SystemExit):
        _args(tmp_path / "c.yaml", "--token-expiry-days", "-7")


def test_missing_provisioning_block_tolerated(
    tmp_path: Path, capture: _Capture
) -> None:
    """Plane has NO required provisioning field — unlike GitLab, whose
    CLI hard-requires provisioning.group.  The block may be absent
    entirely; the 364-day config default still applies."""
    cfg = _write_company(tmp_path, _PLANE_BLOCK + _ROLES_BLOCK)
    assert cmd_plane_provision(_args(cfg, "--provision-token", "tok")) == 0
    assert capture.kwargs["token_expiry_days"] == 364


# ── abort / failure surfaces ──


def test_aborted_prints_cannot_proceed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture
) -> None:
    async def _raise(*a: Any, **k: Any):
        raise PlaneProvisionAborted(
            "no service-accounts API at this Plane instance (HTTP 404)"
        )

    monkeypatch.setattr("crewlet.plane.provision_cli.provision", _raise)
    cfg = _write_company(tmp_path, _PLANE_BLOCK)
    assert cmd_plane_provision(_args(cfg, "--provision-token", "tok")) == 1
    out = capsys.readouterr().out
    assert "Provisioning cannot proceed" in out
    assert "no service-accounts API" in out


def test_provision_error_prints_failed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture
) -> None:
    async def _raise(*a: Any, **k: Any):
        raise PlaneProvisionError("boom", method="GET", path="/x", status=500)

    monkeypatch.setattr("crewlet.plane.provision_cli.provision", _raise)
    cfg = _write_company(tmp_path, _PLANE_BLOCK)
    assert cmd_plane_provision(_args(cfg, "--provision-token", "tok")) == 1
    out = capsys.readouterr().out
    assert "Provisioning failed" in out
    assert "500" in out


def test_invalid_role_aborts_listing_valid_choices(
    tmp_path: Path, capsys: pytest.CaptureFixture
) -> None:
    """End-to-end: a GitLab copy-paste role like ``developer`` aborts
    in provision()'s preflight (before any HTTP call — no mocking
    needed) and the CLI surfaces the valid choices."""
    cfg = _write_company(
        tmp_path, _PLANE_BLOCK + "    provisioning:\n      role: developer\n"
    )
    assert cmd_plane_provision(_args(cfg, "--provision-token", "tok")) == 1
    out = capsys.readouterr().out
    assert "Provisioning cannot proceed" in out
    assert "invalid workspace role 'developer'" in out
    assert "admin | guest | member" in out


def test_seat_error_exits_nonzero(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture
) -> None:
    report = ProvisionReport(
        seats=[
            SeatResult(
                handle="eng",
                username="agent-eng",
                account_action="created",
                membership="added",
                token_action="minted",
            ),
            SeatResult(
                handle="bad",
                username="agent-bad",
                account_action="error",
                error="POST → 500",
            ),
        ]
    )
    cap = _Capture(report)
    monkeypatch.setattr("crewlet.plane.provision_cli.provision", cap.fake_provision())
    cfg = _write_company(tmp_path, _PLANE_BLOCK)
    assert cmd_plane_provision(_args(cfg, "--provision-token", "tok")) == 1
    assert "ERROR: POST → 500" in capsys.readouterr().out


def test_created_seat_with_error_still_exits_nonzero(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture
) -> None:
    """`error` (not account_action) drives the exit code: a seat whose
    account was created before its membership step 403'd keeps the
    truthful account=created, and the run must still exit 1."""
    report = ProvisionReport(
        seats=[
            SeatResult(
                handle="eng",
                username="agent-eng",
                account_action="created",
                membership="pending",
                token_action="minted",
                error="POST …/members/ → 403",
            ),
        ]
    )
    cap = _Capture(report)
    monkeypatch.setattr("crewlet.plane.provision_cli.provision", cap.fake_provision())
    cfg = _write_company(tmp_path, _PLANE_BLOCK)
    assert cmd_plane_provision(_args(cfg, "--provision-token", "tok")) == 1
    out = capsys.readouterr().out
    assert "account=created" in out
    assert "ERROR: POST …/members/ → 403" in out


# ── report printer ──


def _full_report() -> ProvisionReport:
    return ProvisionReport(
        seats=[
            SeatResult(
                handle="eng",
                username="agent-eng",
                account_action="created",
                membership="added",
                token_action="minted",
            ),
            SeatResult(
                handle="bad",
                username="agent-bad",
                account_action="error",
                error="boom",
            ),
        ],
        hooks=["https://engine.test/webhooks/plane (created, secret captured)"],
        decommissioned=["agent-old"],
        notes=[
            "created project 'ENG'",
            "agent-eng: managed token expires 2026-08-01 (within 30 days) "
            "— re-run with --rotate",
        ],
        members=[
            {
                "display_name": "Founding Human",
                "username": "founder",
                "id": FOUNDER_ID,
                "role": "admin",
                "managed": False,
            },
            {
                "display_name": "Engineer",
                "username": "agent-eng",
                "id": "22222222-2222-2222-2222-222222222222",
                "role": "member",
                "managed": True,
            },
        ],
    )


def test_print_report_full(capsys: pytest.CaptureFixture) -> None:
    _print_report(_full_report(), printed_tokens=False)
    out = capsys.readouterr().out
    assert "Plane provisioning report" in out
    assert "agent-eng" in out
    assert "account=created" in out
    assert "member=added" in out
    assert "token=minted" in out
    assert "ERROR: boom" in out
    assert (
        "webhooks: https://engine.test/webhooks/plane (created, secret captured)" in out
    )
    assert "decommissioned: agent-old" in out
    assert "note: created project 'ENG'" in out
    # Expiry notes ride the generic notes channel.
    assert "re-run with --rotate" in out
    # The reminder covers both: source the env file AND restart the engine.
    assert "1 token(s) written to the env file" in out
    assert "restart the engine" in out
    # The member table ends the report: humans unmarked, managed rows
    # marked [agent], UUIDs visible for contact.plane_user_id copying.
    assert "contact.plane_user_id" in out
    founder_line = next(line for line in out.splitlines() if "founder" in line)
    assert FOUNDER_ID in founder_line
    assert "[agent]" not in founder_line
    last_line = out.rstrip().splitlines()[-1]
    assert "agent-eng" in last_line
    assert "[agent]" in last_line


def test_print_report_no_env_reminder_when_printed(
    capsys: pytest.CaptureFixture,
) -> None:
    _print_report(_full_report(), printed_tokens=True)
    out = capsys.readouterr().out
    assert "written to the env file" not in out


def test_print_report_no_reminder_when_nothing_minted(
    capsys: pytest.CaptureFixture,
) -> None:
    report = ProvisionReport(
        seats=[
            SeatResult(
                handle="eng",
                username="agent-eng",
                account_action="exists",
                membership="exists",
                token_action="skipped",
            )
        ]
    )
    _print_report(report, printed_tokens=False)
    out = capsys.readouterr().out
    assert "written to the env file" not in out
    assert "token=skipped" in out
