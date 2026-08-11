"""Tests for the provisioning ledger (slack-apps.json)."""

from __future__ import annotations

import json
import stat

from crewlet.slack.state import (
    ProvisionState,
    SlackAppRecord,
    load_state,
    save_state,
)


def test_missing_file_is_empty_ledger(tmp_path) -> None:
    assert load_state(tmp_path / "slack-apps.json").apps == {}


def test_round_trip(tmp_path) -> None:
    path = tmp_path / "slack-apps.json"
    state = ProvisionState(
        apps={
            "agent-ceo": SlackAppRecord(
                app_id="A0001",
                client_id="1.2",
                client_secret="sec",
                signing_secret="sign",
                bot_user_id="U1",
                team_id="T1",
                manifest_hash="abc123",
            )
        }
    )
    save_state(path, state)
    loaded = load_state(path)
    assert loaded == state


def test_saved_owner_only_permissions(tmp_path) -> None:
    path = tmp_path / "slack-apps.json"
    save_state(path, ProvisionState())
    mode = stat.S_IMODE(path.stat().st_mode)
    assert mode == 0o600


def test_save_is_atomic_no_temp_left(tmp_path) -> None:
    """The ledger holds once-only client secrets — writes must never
    truncate-then-fail. Rewrites go through a temp file + os.replace."""
    path = tmp_path / "slack-apps.json"
    save_state(path, ProvisionState(apps={"a": SlackAppRecord(app_id="A1")}))
    save_state(path, ProvisionState(apps={"a": SlackAppRecord(app_id="A2")}))
    assert load_state(path).apps["a"].app_id == "A2"
    assert list(tmp_path.iterdir()) == [path]


def test_unknown_keys_ignored_on_load(tmp_path) -> None:
    """Forward-compat: a ledger written by a newer version still loads."""
    path = tmp_path / "slack-apps.json"
    path.write_text(
        json.dumps(
            {
                "apps": {"dev": {"app_id": "A9", "future_field": "x"}},
                "some_new_section": {},
            }
        )
    )
    loaded = load_state(path)
    assert loaded.apps["dev"].app_id == "A9"
