"""Tests for the provisioning orchestration (plans + run_provision)."""

from __future__ import annotations

import os
import time
from pathlib import Path
from typing import Any

import pytest

from crewlet.config import load_company_config
from crewlet.slack.api import (
    AppCredentials,
    ConfigTokenPair,
    InstallResult,
    SlackApiError,
)
from crewlet.slack.envfile import EnvStore
from crewlet.slack.manifest import build_agent_manifest
from crewlet.slack.provision import (
    CONFIG_REFRESH_TOKEN_VAR,
    CONFIG_TOKEN_EXPIRES_VAR,
    CONFIG_TOKEN_VAR,
    SlackAgentPlan,
    SlackProvisionError,
    build_agent_plans,
    manifest_fingerprint,
    parse_install_code,
    resolve_config_token,
    run_provision,
)
from crewlet.slack.state import ProvisionState, SlackAppRecord, load_state

BASE = "https://crewlet.example.com"

COMPANY_YAML = """
name: "Acme AI"
roles:
  - name: "Jane Founder"
    kind: human
    manages: ["Agent CEO"]
    contact: { slack_user_id: U0FOUNDER }
  - name: "Agent CEO"
    goal: "Lead"
    manages: ["Agent Engineer"]
    integrations:
      slack:
        bot_token: "${SLACK_BOT_TOKEN_CEO}"
        signing_secret: "${SLACK_SIGNING_SECRET_CEO}"
    mcp_env:
      slack:
        SLACK_MCP_XOXB_TOKEN: "${SLACK_BOT_TOKEN_CEO}"
units:
  - name: Core
    type: team
    lead: "Agent Engineer"
    roles:
      - name: "Agent Engineer"
        goal: "Build"
        integrations:
          slack:
            bot_token: "${SLACK_BOT_TOKEN_ENG}"
            signing_secret: "${SLACK_SIGNING_SECRET_ENG}"
      - name: "Agent Writer"
        goal: "Write"
        integrations:
          slack:
            bot_token: "xoxb-literal-token"
            signing_secret: "literal-secret"
      - name: "Agent Offline"
        goal: "No slack"
"""

_ENV_VARS = (
    "SLACK_BOT_TOKEN_CEO",
    "SLACK_SIGNING_SECRET_CEO",
    "SLACK_BOT_TOKEN_ENG",
    "SLACK_SIGNING_SECRET_ENG",
    CONFIG_TOKEN_VAR,
    CONFIG_REFRESH_TOKEN_VAR,
    CONFIG_TOKEN_EXPIRES_VAR,
)


@pytest.fixture
def company(tmp_path: Path):
    path = tmp_path / "company.yaml"
    path.write_text(COMPANY_YAML)
    return load_company_config(path)


@pytest.fixture(autouse=True)
def _clean_slack_env():
    """Shield the EnvStore's process-env fallback from ambient vars."""
    saved = {var: os.environ.pop(var, None) for var in _ENV_VARS}
    yield
    for var, value in saved.items():
        if value is None:
            os.environ.pop(var, None)
        else:
            os.environ[var] = value


class FakeSlackClient:
    """Duck-typed stand-in for SlackManifestClient."""

    def __init__(self) -> None:
        self.created: list[dict[str, Any]] = []
        self.updated: list[tuple[str, dict[str, Any]]] = []
        self.exchanged: list[str] = []
        self.rotations: list[str] = []
        self.update_error: str = ""
        self.exchange_error: dict[str, str] = {}
        self.exchange_app_id: str = ""
        self.rotate_error: str = ""
        self._counter = 0

    async def rotate_config_token(self, refresh_token: str) -> ConfigTokenPair:
        self.rotations.append(refresh_token)
        if self.rotate_error:
            raise SlackApiError("tooling.tokens.rotate", self.rotate_error)
        return ConfigTokenPair(
            token="xoxe.rotated",
            refresh_token="xoxe-1-next",
            expires_at=int(time.time()) + 43200,
        )

    async def create_app(
        self, config_token: str, manifest: dict[str, Any]
    ) -> AppCredentials:
        self._counter += 1
        self.created.append(manifest)
        n = self._counter
        return AppCredentials(
            app_id=f"A000{n}",
            client_id=f"cid-{n}",
            client_secret=f"csec-{n}",
            signing_secret=f"ssec-{n}",
        )

    async def update_app(
        self, config_token: str, app_id: str, manifest: dict[str, Any]
    ) -> None:
        if self.update_error:
            raise SlackApiError("apps.manifest.update", self.update_error)
        self.updated.append((app_id, manifest))

    async def exchange_oauth_code(
        self, *, code: str, client_id: str, client_secret: str, redirect_url: str
    ) -> InstallResult:
        handle = code.removeprefix("code-")
        if handle in self.exchange_error:
            raise SlackApiError("oauth.v2.access", self.exchange_error[handle])
        self.exchanged.append(code)
        return InstallResult(
            bot_token=f"xoxb-{code}",
            app_id=self.exchange_app_id,
            bot_user_id="U0BOT",
            team_id="T01",
        )


async def _auto_installer(plan: SlackAgentPlan, authorize_url: str) -> str:
    assert authorize_url.startswith("https://slack.com/oauth/v2/authorize")
    return f"code-{plan.handle}"


def _ledgered_record(app_id: str, *, manifest: dict[str, Any]) -> SlackAppRecord:
    return SlackAppRecord(
        app_id=app_id,
        client_id="c",
        client_secret="s",
        signing_secret="ss",
        manifest_hash=manifest_fingerprint(manifest),
    )


def _current_state(company, tmp_path: Path) -> ProvisionState:
    """A ledger whose two seats are fully provisioned with the CURRENT
    manifest fingerprints (so re-runs are no-ops unless a test says
    otherwise)."""
    plans, _ = build_agent_plans(company, base_url=BASE)
    return ProvisionState(
        apps={
            plan.handle: _ledgered_record(f"A-{plan.handle}", manifest=plan.manifest)
            for plan in plans
        }
    )


# ---------------------------------------------------------------------------
# build_agent_plans
# ---------------------------------------------------------------------------


def test_plans_cover_placeholder_seats_and_skip_literal_ones(company) -> None:
    plans, skipped = build_agent_plans(company, base_url=BASE)
    assert [p.handle for p in plans] == ["agent-ceo", "agent-engineer"]
    ceo = plans[0]
    assert ceo.bot_token_var == "SLACK_BOT_TOKEN_CEO"
    assert ceo.signing_secret_var == "SLACK_SIGNING_SECRET_CEO"
    assert (
        ceo.manifest["settings"]["event_subscriptions"]["request_url"]
        == f"{BASE}/webhooks/slack/agent-ceo"
    )
    # Literal credentials = a manually managed app — surfaced, not silent.
    assert [handle for handle, _ in skipped] == ["agent-writer"]
    assert "not provisionable" in skipped[0][1]


def test_plans_handle_filter(company) -> None:
    plans, _ = build_agent_plans(
        company, base_url=BASE, only_handles={"agent-engineer"}
    )
    assert [p.handle for p in plans] == ["agent-engineer"]


def test_plans_unknown_handle_raises(company) -> None:
    with pytest.raises(SlackProvisionError, match="agent-nope"):
        build_agent_plans(company, base_url=BASE, only_handles={"agent-nope"})


def test_mixed_placeholder_literal_rejected_at_validation(tmp_path: Path) -> None:
    """A half-placeholder pair is an authoring mistake — rejected by
    RoleSlackIdentity when the org is built (`crewlet validate`, engine
    apply, and provisioning all pass through it), never a silent skip."""
    path = tmp_path / "company.yaml"
    path.write_text(
        'name: "Acme"\n'
        "roles:\n"
        "  - name: Agent X\n"
        "    goal: g\n"
        "    integrations:\n"
        "      slack:\n"
        '        bot_token: "${SLACK_BOT_TOKEN_X}"\n'
        '        signing_secret: "pasted-literal"\n'
    )
    with pytest.raises(ValueError, match="mixed"):
        build_agent_plans(load_company_config(path), base_url=BASE)


def test_signing_secret_without_bot_token_rejected(tmp_path: Path) -> None:
    """Secret-only is dead config: registration skips tokenless roles,
    so the secret would verify nothing."""
    path = tmp_path / "company.yaml"
    path.write_text(
        'name: "Acme"\n'
        "roles:\n"
        "  - name: Agent X\n"
        "    goal: g\n"
        "    integrations:\n"
        "      slack:\n"
        '        signing_secret: "${SLACK_SIGNING_SECRET_X}"\n'
    )
    with pytest.raises(ValueError, match="no bot_token"):
        build_agent_plans(load_company_config(path), base_url=BASE)


def test_bot_token_only_is_valid_but_not_provisionable(tmp_path: Path) -> None:
    """An outbound-only identity (bot token, no webhooks) stays legal —
    it just can't be provisioned, and says so."""
    path = tmp_path / "company.yaml"
    path.write_text(
        'name: "Acme"\n'
        "roles:\n"
        "  - name: Agent X\n"
        "    goal: g\n"
        "    integrations:\n"
        "      slack:\n"
        '        bot_token: "${SLACK_BOT_TOKEN_X}"\n'
    )
    plans, skipped = build_agent_plans(load_company_config(path), base_url=BASE)
    assert not plans
    assert [handle for handle, _ in skipped] == ["agent-x"]
    assert "not provisionable" in skipped[0][1]


# ---------------------------------------------------------------------------
# parse_install_code
# ---------------------------------------------------------------------------


async def test_parse_install_code_variants() -> None:
    assert parse_install_code("  abc.def  ") == "abc.def"
    assert (
        parse_install_code(
            "https://x.example/webhooks/slack-oauth?code=abc.def&state=ceo"
        )
        == "abc.def"
    )
    assert parse_install_code("code=abc.def&state=x") == "abc.def"
    with pytest.raises(SlackProvisionError):
        parse_install_code("   ")
    with pytest.raises(SlackProvisionError):
        parse_install_code("https://x.example/cb?code=&state=x")


# ---------------------------------------------------------------------------
# run_provision
# ---------------------------------------------------------------------------


async def _provision(company, tmp_path: Path, client, state=None, **kwargs):
    plans, _ = build_agent_plans(company, base_url=BASE)
    state = state or ProvisionState()
    env = kwargs.pop("env", None) or EnvStore(tmp_path / ".env")
    kwargs.setdefault("request_install_code", _auto_installer)
    outcomes = await run_provision(
        plans,
        client=client,
        config_token="xoxe.config",
        state=state,
        state_path=tmp_path / "slack-apps.json",
        env=env,
        base_url=BASE,
        **kwargs,
    )
    return outcomes, state, env


async def test_fresh_run_creates_installs_and_writes_secrets(
    company, tmp_path: Path
) -> None:
    client = FakeSlackClient()
    outcomes, state, env = await _provision(company, tmp_path, client)

    assert [(o.handle, o.action, o.installed) for o in outcomes] == [
        ("agent-ceo", "created", True),
        ("agent-engineer", "created", True),
    ]
    assert len(client.created) == 2 and not client.updated
    assert client.exchanged == ["code-agent-ceo", "code-agent-engineer"]

    env_text = (tmp_path / ".env").read_text()
    assert "SLACK_SIGNING_SECRET_CEO=ssec-1" in env_text
    assert "SLACK_BOT_TOKEN_CEO=xoxb-code-agent-ceo" in env_text
    assert "SLACK_SIGNING_SECRET_ENG=ssec-2" in env_text
    assert "SLACK_BOT_TOKEN_ENG=xoxb-code-agent-engineer" in env_text

    ledger = load_state(tmp_path / "slack-apps.json")
    assert ledger.apps["agent-ceo"].app_id == "A0001"
    assert ledger.apps["agent-ceo"].client_secret == "csec-1"
    assert ledger.apps["agent-ceo"].bot_user_id == "U0BOT"
    assert ledger.apps["agent-ceo"].manifest_hash
    assert ledger.apps["agent-engineer"].app_id == "A0002"


async def test_rerun_with_unchanged_manifest_skips_update(
    company, tmp_path: Path
) -> None:
    """Byte-identical manifests are not re-pushed — the endpoints are
    Tier 1 rate-limited and the push would be a no-op."""
    client = FakeSlackClient()
    state = _current_state(company, tmp_path)
    env = EnvStore(tmp_path / ".env")
    await env.set(
        {
            "SLACK_BOT_TOKEN_CEO": "xoxb-existing",
            "SLACK_SIGNING_SECRET_CEO": "ss",
            "SLACK_BOT_TOKEN_ENG": "xoxb-existing",
            "SLACK_SIGNING_SECRET_ENG": "ss",
        }
    )

    outcomes, _, _ = await _provision(company, tmp_path, client, state=state, env=env)

    assert [(o.action, o.installed) for o in outcomes] == [
        ("unchanged", False),
        ("unchanged", False),
    ]
    assert not client.updated and not client.created and not client.exchanged


async def test_changed_manifest_updates_and_records_hash(
    company, tmp_path: Path
) -> None:
    client = FakeSlackClient()
    state = _current_state(company, tmp_path)
    for record in state.apps.values():
        record.manifest_hash = "stale-fingerprint"
    env = EnvStore(tmp_path / ".env")
    await env.set(
        {
            "SLACK_BOT_TOKEN_CEO": "xoxb-existing",
            "SLACK_SIGNING_SECRET_CEO": "ss",
            "SLACK_BOT_TOKEN_ENG": "xoxb-existing",
            "SLACK_SIGNING_SECRET_ENG": "ss",
        }
    )

    outcomes, state, _ = await _provision(
        company, tmp_path, client, state=state, env=env
    )

    assert [o.action for o in outcomes] == ["updated", "updated"]
    assert [app_id for app_id, _ in client.updated] == [
        "A-agent-ceo",
        "A-agent-engineer",
    ]
    assert not client.created and not client.exchanged
    plans, _ = build_agent_plans(company, base_url=BASE)
    assert state.apps["agent-ceo"].manifest_hash == manifest_fingerprint(
        plans[0].manifest
    )


async def test_vanished_app_recreated_and_reinstalled(company, tmp_path: Path) -> None:
    """A console-deleted app is replaced AND force-installed — the stale
    bot token in the env belongs to the dead app and must not satisfy
    the presence check."""
    client = FakeSlackClient()
    client.update_error = "app_not_found"
    state = _current_state(company, tmp_path)
    for record in state.apps.values():
        record.manifest_hash = "stale-fingerprint"  # force the update attempt
    env = EnvStore(tmp_path / ".env")
    await env.set(
        {
            "SLACK_BOT_TOKEN_CEO": "xoxb-dead-token",
            "SLACK_SIGNING_SECRET_CEO": "ss",
            "SLACK_BOT_TOKEN_ENG": "xoxb-dead-token",
            "SLACK_SIGNING_SECRET_ENG": "ss",
        }
    )

    outcomes, state, env = await _provision(
        company, tmp_path, client, state=state, env=env
    )

    assert all(o.action == "created" for o in outcomes)
    assert all(o.installed for o in outcomes)
    assert state.apps["agent-ceo"].app_id == "A0001"
    assert env.get("SLACK_BOT_TOKEN_CEO") == "xoxb-code-agent-ceo"
    assert any("no longer exists" in note for note in outcomes[0].notes)
    assert any("stale" in note for note in outcomes[0].notes)


async def test_one_agent_failure_does_not_abort_the_rest(
    company, tmp_path: Path
) -> None:
    """A bad code paste on one agent records a failed outcome; the
    remaining agents still provision, and the failed agent's signing
    secret is already durable in the env file (written before the
    interactive step)."""
    client = FakeSlackClient()
    client.exchange_error = {"agent-ceo": "invalid_code"}

    outcomes, _, env = await _provision(company, tmp_path, client)

    assert [(o.handle, o.action) for o in outcomes] == [
        ("agent-ceo", "failed"),
        ("agent-engineer", "created"),
    ]
    assert "invalid_code" in outcomes[0].error
    # Engineer completed fully.
    assert env.get("SLACK_BOT_TOKEN_ENG") == "xoxb-code-agent-engineer"
    # CEO's signing secret was flushed BEFORE the failed exchange.
    assert env.get("SLACK_SIGNING_SECRET_CEO") == "ssec-1"
    assert env.get("SLACK_BOT_TOKEN_CEO") == ""


async def test_fatal_token_error_aborts_run(company, tmp_path: Path) -> None:
    """token_expired would fail every remaining call identically — it
    aborts (for the caller to rotate and retry) instead of burning every
    agent into a failed outcome."""
    client = FakeSlackClient()
    client.exchange_error = {"agent-ceo": "token_expired"}
    with pytest.raises(SlackApiError, match="token_expired"):
        await _provision(company, tmp_path, client)


async def test_other_update_errors_propagate_as_failed_outcome(
    company, tmp_path: Path
) -> None:
    client = FakeSlackClient()
    client.update_error = "invalid_manifest"
    state = _current_state(company, tmp_path)
    for record in state.apps.values():
        record.manifest_hash = "stale-fingerprint"

    outcomes, _, _ = await _provision(
        company,
        tmp_path,
        client,
        state=state,
        skip_install=True,
        request_install_code=None,
    )
    assert all(o.action == "failed" for o in outcomes)
    assert all("invalid_manifest" in o.error for o in outcomes)


async def test_skip_install_batches_env_writes(company, tmp_path: Path) -> None:
    """Signing-secret-only writes (already durable in the ledger) land
    in ONE env flush at the end, not one rewrite per agent."""
    client = FakeSlackClient()
    env = EnvStore(tmp_path / ".env")
    writes: list[dict[str, str]] = []
    original_set = env.set

    async def _tracking_set(values: dict[str, str]) -> None:
        if values:
            writes.append(dict(values))
        await original_set(values)

    env.set = _tracking_set  # type: ignore[method-assign]

    outcomes, _, _ = await _provision(
        company,
        tmp_path,
        client,
        env=env,
        skip_install=True,
        request_install_code=None,
    )
    assert not client.exchanged
    assert len(writes) == 1
    assert writes[0] == {
        "SLACK_SIGNING_SECRET_CEO": "ssec-1",
        "SLACK_SIGNING_SECRET_ENG": "ssec-2",
    }
    env_text = (tmp_path / ".env").read_text()
    assert "SLACK_BOT_TOKEN_CEO" not in env_text
    assert any("still unset" in note for o in outcomes for note in o.notes)


async def test_reinstall_forces_oauth(company, tmp_path: Path) -> None:
    client = FakeSlackClient()
    state = _current_state(company, tmp_path)
    env = EnvStore(tmp_path / ".env")
    await env.set(
        {
            "SLACK_BOT_TOKEN_CEO": "xoxb-old",
            "SLACK_SIGNING_SECRET_CEO": "ss",
            "SLACK_BOT_TOKEN_ENG": "xoxb-old",
            "SLACK_SIGNING_SECRET_ENG": "ss",
        }
    )

    outcomes, _, env = await _provision(
        company, tmp_path, client, state=state, env=env, reinstall=True
    )
    assert client.exchanged == ["code-agent-ceo", "code-agent-engineer"]
    assert all(o.installed for o in outcomes)
    assert env.get("SLACK_BOT_TOKEN_CEO") == "xoxb-code-agent-ceo"


async def test_wrong_app_code_recorded_as_failure(company, tmp_path: Path) -> None:
    client = FakeSlackClient()
    client.exchange_app_id = "ANOTHER"
    outcomes, _, _ = await _provision(company, tmp_path, client)
    assert all(o.action == "failed" for o in outcomes)
    assert all("different app" in o.error for o in outcomes)


async def test_hand_adopted_ledger_without_client_creds_is_actionable(
    company, tmp_path: Path
) -> None:
    """Adopting an existing app by hand-seeding only app_id must produce
    an actionable error, not an authorize URL with an empty client_id."""
    client = FakeSlackClient()
    plans, _ = build_agent_plans(company, base_url=BASE)
    state = ProvisionState(
        apps={
            plan.handle: SlackAppRecord(
                app_id=f"A-{plan.handle}",
                manifest_hash=manifest_fingerprint(plan.manifest),
            )
            for plan in plans
        }
    )
    outcomes, _, _ = await _provision(company, tmp_path, client, state=state)
    assert all(o.action == "failed" for o in outcomes)
    assert all("client_id/client_secret" in o.error for o in outcomes)
    assert not client.exchanged


async def test_stale_shell_export_does_not_suppress_install(
    company, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A leftover shell export must not make the installer believe the
    agent is provisioned — presence is judged against the env FILE."""
    monkeypatch.setenv("SLACK_BOT_TOKEN_CEO", "xoxb-stale-shell-export")
    client = FakeSlackClient()
    outcomes, _, env = await _provision(company, tmp_path, client)
    assert all(o.installed for o in outcomes)
    assert env.get("SLACK_BOT_TOKEN_CEO") == "xoxb-code-agent-ceo"
    # The shell export itself is untouched.
    assert os.environ["SLACK_BOT_TOKEN_CEO"] == "xoxb-stale-shell-export"


async def test_installer_required_unless_skipped(company, tmp_path: Path) -> None:
    client = FakeSlackClient()
    with pytest.raises(SlackProvisionError, match="request_install_code"):
        await _provision(company, tmp_path, client, request_install_code=None)


# ---------------------------------------------------------------------------
# resolve_config_token
# ---------------------------------------------------------------------------


async def test_resolve_rotates_and_persists_pair_with_expiry(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    client = FakeSlackClient()
    monkeypatch.setenv(CONFIG_REFRESH_TOKEN_VAR, "xoxe-1-old")
    env = EnvStore(tmp_path / ".env")

    token = await resolve_config_token(client, env)

    assert token == "xoxe.rotated"
    assert client.rotations == ["xoxe-1-old"]
    env_text = (tmp_path / ".env").read_text()
    assert f"{CONFIG_TOKEN_VAR}=xoxe.rotated" in env_text
    assert f"{CONFIG_REFRESH_TOKEN_VAR}=xoxe-1-next" in env_text
    assert CONFIG_TOKEN_EXPIRES_VAR in env_text
    # os.environ untouched — a later run reads the FILE, so a stale shell
    # export can no longer shadow the fresh pair.
    assert os.environ.get(CONFIG_TOKEN_VAR) is None


async def test_resolve_skips_rotation_while_token_fresh(tmp_path: Path) -> None:
    """Previews and quick re-runs must not consume a rotation while the
    persisted access token is comfortably within its 12 h lifetime."""
    client = FakeSlackClient()
    env = EnvStore(tmp_path / ".env")
    await env.set(
        {
            CONFIG_TOKEN_VAR: "xoxe.fresh",
            CONFIG_REFRESH_TOKEN_VAR: "xoxe-1-current",
            CONFIG_TOKEN_EXPIRES_VAR: str(int(time.time()) + 40000),
        }
    )
    assert await resolve_config_token(client, env) == "xoxe.fresh"
    assert not client.rotations


async def test_resolve_rotates_near_expiry(tmp_path: Path) -> None:
    client = FakeSlackClient()
    env = EnvStore(tmp_path / ".env")
    await env.set(
        {
            CONFIG_TOKEN_VAR: "xoxe.nearly-dead",
            CONFIG_REFRESH_TOKEN_VAR: "xoxe-1-current",
            CONFIG_TOKEN_EXPIRES_VAR: str(int(time.time()) + 60),
        }
    )
    assert await resolve_config_token(client, env) == "xoxe.rotated"
    assert client.rotations == ["xoxe-1-current"]


async def test_resolve_force_rotate_ignores_fresh_token(tmp_path: Path) -> None:
    """The stale-bootstrap-token retry path: force_rotate must rotate
    even when the recorded expiry looks fine."""
    client = FakeSlackClient()
    env = EnvStore(tmp_path / ".env")
    await env.set(
        {
            CONFIG_TOKEN_VAR: "xoxe.claims-fresh",
            CONFIG_REFRESH_TOKEN_VAR: "xoxe-1-current",
            CONFIG_TOKEN_EXPIRES_VAR: str(int(time.time()) + 40000),
        }
    )
    assert await resolve_config_token(client, env, force_rotate=True) == "xoxe.rotated"
    assert client.rotations == ["xoxe-1-current"]


async def test_resolve_uses_unknown_expiry_token_without_rotating(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The manually generated bootstrap token has no recorded expiry —
    use it as-is (the caller retries with force_rotate on
    token_expired) instead of consuming a rotation per run."""
    client = FakeSlackClient()
    monkeypatch.setenv(CONFIG_TOKEN_VAR, "xoxe.bootstrap")
    monkeypatch.setenv(CONFIG_REFRESH_TOKEN_VAR, "xoxe-1-bootstrap")
    env = EnvStore(tmp_path / ".env")
    assert await resolve_config_token(client, env) == "xoxe.bootstrap"
    assert not client.rotations


async def test_resolve_falls_back_to_access_token_on_rotate_failure(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    client = FakeSlackClient()
    client.rotate_error = "invalid_refresh_token"
    env = EnvStore(tmp_path / ".env")
    await env.set(
        {
            CONFIG_TOKEN_VAR: "xoxe.access",
            CONFIG_REFRESH_TOKEN_VAR: "xoxe-1-bad",
            CONFIG_TOKEN_EXPIRES_VAR: str(int(time.time()) + 60),
        }
    )
    assert await resolve_config_token(client, env) == "xoxe.access"


async def test_resolve_without_any_token_raises(tmp_path: Path) -> None:
    client = FakeSlackClient()
    with pytest.raises(SlackProvisionError, match="configuration token"):
        await resolve_config_token(client, EnvStore(tmp_path / ".env"))


# ---------------------------------------------------------------------------
# manifest_fingerprint
# ---------------------------------------------------------------------------


def test_fingerprint_stable_and_content_sensitive() -> None:
    a = build_agent_manifest(role_name="A", handle="a", base_url=BASE)
    assert manifest_fingerprint(a) == manifest_fingerprint(
        build_agent_manifest(role_name="A", handle="a", base_url=BASE)
    )
    b = build_agent_manifest(
        role_name="A", handle="a", base_url="https://other.example"
    )
    assert manifest_fingerprint(a) != manifest_fingerprint(b)
