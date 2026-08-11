"""Regression tests for live-config-management edge cases.

Each test pins a subtle live-apply bug class so a future change can't
silently reintroduce it.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parent.parent))
from conftest import make_company, make_engine  # noqa: E402
from crewlet.config import CompanyConfig, config_to_organization  # noqa: E402
from crewlet.queue.memory import MemoryEventQueue  # noqa: E402


async def _engine_with_started_queue(roles: list[dict]):
    """Engine whose event queue is started — the org-diff add/remove
    branches publish AgentSpawned / RoleUpdated, which a stopped
    MemoryEventQueue rejects."""
    eq = MemoryEventQueue()
    await eq.start()
    return await make_engine(
        company=make_company(name="Acme", roles=roles),
        event_queue=eq,
    )


# ── _dispatch_org_diff covers every role field ─────────────────────────


async def test_dispatch_org_diff_detects_single_field_role_edits() -> None:
    """A live edit to ANY single role field must fire the org diff —
    the prior signature sampled only handle/name/goal/backstory/manages/
    responsibilities/guidelines, so mcp_env / slack / llm /
    token_budget / email / unit edits were silently dropped."""
    engine = await make_engine(
        company=make_company(name="Acme", roles=[{"name": "Eng", "token_budget": 1000}])
    )
    old = engine._active_config

    cases: list[tuple[str, object]] = [
        ("token_budget", 2000),
        ("email", "eng@acme.com"),
        ("llm", "gpt-4o"),
        ("mcp_env", {"atlassian": {"JIRA_USERNAME": "eng"}}),
        # Authored under integrations.slack; materialises into role.slack.
        ("integrations", {"slack": {"bot_token": "${SLACK_ENG}"}}),
        ("learning_enabled", False),
    ]
    for field, value in cases:
        role: dict[str, object] = {"name": "Eng", "token_budget": 1000, field: value}
        new = make_company(name="Acme", roles=[role])
        new_org = config_to_organization(new)
        assert engine._dispatch_org_diff(old, new, new_org) is True, field


async def test_dispatch_org_diff_ignores_pure_reorder() -> None:
    """Reordering roles in YAML is a no-op (no spawn cascade pass)."""
    engine = await make_engine(
        company=make_company(name="Acme", roles=[{"name": "A"}, {"name": "B"}])
    )
    old = engine._active_config
    new = make_company(name="Acme", roles=[{"name": "B"}, {"name": "A"}])
    assert engine._dispatch_org_diff(old, new, config_to_organization(new)) is False


async def test_single_field_role_edit_reaches_running_org() -> None:
    """End-to-end: a token_budget-only edit propagates into engine.org."""
    engine = await make_engine(
        company=make_company(name="Acme", roles=[{"name": "Eng", "token_budget": 1000}])
    )
    await engine.apply_config(
        make_company(name="Acme", roles=[{"name": "Eng", "token_budget": 7777}])
    )
    assert engine.org.get_role("Eng").token_budget == 7777


async def test_knowledge_spaces_change_swaps_org() -> None:
    """A ``knowledge.confluence_spaces`` edit must update the running org
    so the Plan-phase Confluence search scope changes live, even with no
    role/unit change (the ``_dispatch_org_diff`` confluence_spaces gate)."""
    engine = await make_engine(
        company=make_company(name="Acme", knowledge={"confluence_spaces": ["OLD"]})
    )
    assert engine.org.confluence_spaces == ["OLD"]
    await engine.apply_config(
        make_company(name="Acme", knowledge={"confluence_spaces": ["NEW", "SHARED"]})
    )
    assert engine.org.confluence_spaces == ["NEW", "SHARED"]


# ── first activation builds the embeddings provider ────────────────────


async def test_first_activation_builds_embeddings(monkeypatch) -> None:
    """An engine that boots unconfigured must build the embeddings
    provider on first activation so the spawn cascade can wire the
    learning subsystem (diary / episodes / reflect)."""
    sentinel = object()
    monkeypatch.setattr(
        "crewlet.engine_builders.build_embedding_provider",
        lambda cfg: sentinel,
    )
    engine = await make_engine(company=make_company(name="Acme"))
    assert engine._embeddings is sentinel


# ── per-role token budgets apply live ──────────────────────────────────


async def _spawn_role_live(engine, name: str, **role_fields):
    """Apply a config that adds ``name`` to the existing org (diff path)."""
    existing = [{"name": r.name} for r in engine.org.all_roles() if r.name != name]
    role = {"name": name, **role_fields}
    await engine.apply_config(
        make_company(name=engine.org.name, roles=[*existing, role])
    )


async def test_added_role_gets_token_budget_live() -> None:
    engine = await _engine_with_started_queue([{"name": "CEO"}])
    await _spawn_role_live(engine, "Eng", token_budget=5000)

    agents = engine.agent_pool.get_all_for_role("Eng")
    assert len(agents) == 1
    budgets = engine.budget_manager._agent_budgets
    assert agents[0].id_str in budgets
    assert budgets[agents[0].id_str].max_tokens == 5000


async def test_changed_role_budget_updates_and_drops_live() -> None:
    engine = await _engine_with_started_queue([{"name": "CEO"}])
    await _spawn_role_live(engine, "Eng", token_budget=5000)
    agent_id = engine.agent_pool.get_all_for_role("Eng")[0].id_str

    # Raise the cap — preserved-in-place update.
    await engine.apply_config(
        make_company(
            name="Acme",
            roles=[{"name": "CEO"}, {"name": "Eng", "token_budget": 9000}],
        )
    )
    assert engine.budget_manager._agent_budgets[agent_id].max_tokens == 9000

    # Drop to 0 (= unlimited) removes the per-agent cap entirely.
    await engine.apply_config(
        make_company(
            name="Acme",
            roles=[{"name": "CEO"}, {"name": "Eng", "token_budget": 0}],
        )
    )
    assert agent_id not in engine.budget_manager._agent_budgets


async def test_removed_role_drops_budget_live() -> None:
    engine = await _engine_with_started_queue([{"name": "CEO"}])
    await _spawn_role_live(engine, "Eng", token_budget=5000)
    agent_id = engine.agent_pool.get_all_for_role("Eng")[0].id_str
    assert agent_id in engine.budget_manager._agent_budgets

    await engine.apply_config(make_company(name="Acme", roles=[{"name": "CEO"}]))
    assert agent_id not in engine.budget_manager._agent_budgets


# ── removed role tears down per-role MCP state ─────────────────────────


async def test_removed_role_pops_role_mcp_tools() -> None:
    engine = await make_engine(
        company=make_company(name="Acme", roles=[{"name": "CEO"}, {"name": "Eng"}])
    )
    # Simulate the post-cascade state with a cached per-role tool list.
    engine._tier_b_done = True
    engine._role_mcp_tools["Eng"] = ["fake_tool"]

    await engine.apply_config(make_company(name="Acme", roles=[{"name": "CEO"}]))
    assert "Eng" not in engine._role_mcp_tools


# ── a github-only role edit respawns per-role MCP ──────────────────────


async def test_role_mcp_env_change_triggers_role_mcp_respawn() -> None:
    """Rotating a per-agent MCP credential (here the GitHub Copilot MCP
    auth header in ``mcp_env.github``) respawns the role's MCP."""
    from unittest.mock import AsyncMock

    engine = await make_engine(
        company=make_company(
            name="Acme",
            roles=[
                {
                    "name": "Eng",
                    "mcp_env": {"github": {"Authorization": "Bearer ${GH_A}"}},
                }
            ],
        )
    )
    engine._tier_b_done = True
    engine._respawn_role_mcp = AsyncMock()

    await engine.apply_config(
        make_company(
            name="Acme",
            roles=[
                {
                    "name": "Eng",
                    "mcp_env": {"github": {"Authorization": "Bearer ${GH_B}"}},
                }
            ],
        )
    )
    engine._respawn_role_mcp.assert_awaited_once()


# ── a shared http MCP url change restarts via the http path ────────────


async def test_shared_http_mcp_url_change_uses_http_restart() -> None:
    from unittest.mock import AsyncMock

    engine = await make_engine(
        company=make_company(
            name="Acme",
            mcp_servers=[
                {"name": "remote", "transport": "http", "url": "https://a/mcp"}
            ],
        )
    )
    engine._tier_b_done = True
    bridge = AsyncMock()
    bridge.restart_http_server = AsyncMock(return_value=[])
    bridge.restart_server = AsyncMock(return_value=[])
    engine.mcp_bridge = bridge

    await engine.apply_config(
        make_company(
            name="Acme",
            mcp_servers=[
                {"name": "remote", "transport": "http", "url": "https://b/mcp"}
            ],
        )
    )

    bridge.restart_http_server.assert_awaited_once()
    bridge.restart_server.assert_not_awaited()


# ── notification rate_limit propagates to the running service ──────────


async def test_rate_limit_change_propagates_to_service() -> None:
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.notifications.service import NotificationService

    engine = await make_engine(
        company=make_company(name="Acme", notification_rate_limit=5),
        event_queue=MemoryEventQueue(),
    )
    engine.notification_service = NotificationService(
        engine.event_queue,
        {},
        HandleRegistry(engine.agent_pool),
        rate_limit=5,
    )

    await engine.apply_config(make_company(name="Acme", notification_rate_limit=20))
    assert engine.notification_service.rate_limit == 20
    # The dead ``email_domain`` write must not reappear.
    assert not hasattr(engine.notification_service, "email_domain")


# ── the NotImplementedError diagnostic names the diverged keys ─────────


async def test_unhandled_field_diagnostic_names_keys(monkeypatch) -> None:
    """If a future Tier B field is added without a handler, the guard
    must report WHICH field diverged, not a useless ``'?'``."""
    engine = await make_engine(company=make_company(name="Acme"))

    # Force the guard by pretending ``mission`` is unhandled, then
    # changing it.  The symmetric-set-diff bug always printed '?'.
    real_dump = CompanyConfig.model_dump

    def _dump_excluding_handled(self, *, exclude=None, **kw):
        # Drop ``mission`` from the ``handled`` exclusion so it surfaces
        # in old_dump/new_dump and trips the guard.
        exclude = set(exclude or set())
        exclude.discard("mission")
        return real_dump(self, exclude=exclude, **kw)

    monkeypatch.setattr(CompanyConfig, "model_dump", _dump_excluding_handled)

    with pytest.raises(NotImplementedError, match="mission"):
        await engine.apply_config(make_company(name="Acme", mission="changed"))


# ── first-activation, null-roles, and snapshot edge cases ─────────────


# C1 — rollback restores _scheduling_config
async def test_rollback_restores_scheduling_config() -> None:
    """If apply_config raises after _apply_scheduling_live mutated
    self._scheduling_config, _rollback must restore the prior value."""
    from crewlet.config import SchedulingConfig
    from crewlet.engine import ConfigApplyError

    engine = await _engine_with_started_queue([{"name": "CEO"}])
    # The extensions-live path is gated on the cascade having run.
    engine._tier_b_done = True

    old_sched = SchedulingConfig(tick_seconds=15)
    new_sched = SchedulingConfig(tick_seconds=99)
    engine._scheduling_config = old_sched
    # Force a downstream failure by raising inside _apply_extensions_live
    # (runs AFTER scheduling in _apply_restart_required_diff).
    from unittest.mock import AsyncMock

    engine._apply_extensions_live = AsyncMock(side_effect=RuntimeError("boom"))

    cfg = make_company(name="Acme", roles=[{"name": "CEO"}], scheduling=new_sched)
    cfg.extensions = [{"force_diff": {"x": 1}}]
    # Seed the prior state with mismatched scheduling + no extensions
    # so the diff path enters both subsystems.
    engine._active_config = make_company(
        name="Acme", roles=[{"name": "CEO"}], scheduling=old_sched
    )

    with pytest.raises(ConfigApplyError):
        await engine.apply_config(cfg)
    # The rollback must have restored the prior scheduling_config.
    assert engine._scheduling_config is old_sched


# C2 — Pulsar handler reports applied_before_failure on ConfigApplyError
async def test_revision_handler_publishes_partial_applied_on_failure() -> None:
    """When apply_config raises ConfigApplyError, the Pulsar handler must
    pull the list of subsystems that DID succeed off the exception and
    publish it as ConfigRevisionApplied.applied_subsystems — not []."""
    from unittest.mock import AsyncMock

    from crewlet.engine import ConfigApplyError

    engine = await make_engine(company=make_company(name="Acme"))
    engine.apply_config = AsyncMock(  # type: ignore[method-assign]
        side_effect=ConfigApplyError(
            "providers", RuntimeError("LLM build failed"), ["org", "budgets"]
        )
    )

    # Drive the handler by registering it and emitting a fake event.
    published: list[tuple[str, object]] = []

    class _StubQueue:
        async def subscribe(self, *, topic, group, handler):  # noqa: ARG002
            self._handler = handler

        async def publish(self, topic, event):
            published.append((topic, event))

    queue = _StubQueue()
    engine.event_queue = queue  # type: ignore[assignment]
    engine._company_config_store = AsyncMock()
    revision = AsyncMock()
    revision.payload = {"name": "Acme"}
    engine._company_config_store.get_revision = AsyncMock(return_value=revision)

    await engine._subscribe_revision_activated()
    from uuid import uuid4

    from crewlet.events.types import ConfigRevisionActivated

    await queue._handler(
        ConfigRevisionActivated(
            revision_id=str(uuid4()),
            source="api",
            created_by="test",
            summary="",
        )
    )

    assert published, "handler did not publish a ConfigRevisionApplied event"
    topic, event = published[0]
    assert topic == "crewlet.config.revision_applied"
    assert event.status == "error"
    assert event.applied_subsystems == ["org", "budgets"]
    assert "providers" in event.error


# first-activation applied_subsystems includes "scheduling"
async def test_first_activation_applied_subsystems_includes_scheduling() -> None:
    """The list returned on first activation must report every
    subsystem the diff path can claim — otherwise audit consumers see
    different shapes for fresh-bootstrap vs every subsequent edit."""
    engine = await make_engine()  # unconfigured baseline
    applied = await engine.apply_config(make_company(name="Acme"))
    assert "scheduling" in applied


# _attach_root_role_dicts_to_units survives `roles: null`
def test_unit_with_null_roles_does_not_crash() -> None:
    cfg = CompanyConfig.model_validate(
        {
            "name": "Acme",
            "units": [{"name": "Eng", "roles": None}],
            "roles": [{"name": "Dev", "unit": "Eng"}],
        }
    )
    org = config_to_organization(cfg)
    assert org.get_unit("Eng").get_role("Dev") is not None


# no-op apply short-circuits the snapshot capture
async def test_noop_apply_short_circuits() -> None:
    """An apply_config call whose payload equals the active revision
    must return [] without capturing a snapshot."""
    from unittest.mock import MagicMock

    engine = await make_engine(company=make_company(name="Acme"))
    engine._snapshot_for_rollback = MagicMock(  # type: ignore[method-assign]
        side_effect=AssertionError("snapshot must NOT run on a no-op apply")
    )
    applied = await engine.apply_config(make_company(name="Acme"))
    assert applied == []
    engine._snapshot_for_rollback.assert_not_called()


# _tier_b_done is a real attribute, not a getattr fallback
def test_tier_b_done_is_initialised_in_init() -> None:
    """``_tier_b_done`` is initialised in ``__init__`` — never read via
    a defensive ``getattr(..., False)`` — so a typo in any diff handler
    raises AttributeError instead of silently coercing to False."""
    from crewlet.engine import Engine
    from crewlet.org.models import Organization

    engine = Engine(organization=Organization(name=""))
    assert engine._tier_b_done is False
    # The attribute must exist as a real slot, not a hasattr-only ghost.
    assert "_tier_b_done" in engine.__dict__


# start() and the live add-branch share AgentPool.spawn_role
async def test_live_role_add_goes_through_agent_pool() -> None:
    """The live add-branch must drive AgentInstance creation via
    AgentPool.spawn_role (the same path start() uses) so future changes
    to the boot cascade ripple through the live path automatically."""
    from unittest.mock import patch

    engine = await _engine_with_started_queue([{"name": "CEO"}])
    with patch.object(
        engine.agent_pool,
        "spawn_role",
        wraps=engine.agent_pool.spawn_role,
    ) as spy:
        await engine.apply_config(
            make_company(name="Acme", roles=[{"name": "CEO"}, {"name": "Eng"}])
        )
    spy.assert_awaited_once()
    # ``source`` is what the audit trail keys off to distinguish
    # boot-time spawns from live additions.
    assert spy.await_args.kwargs["source"] == "engine.apply_config"


# Jira and GitHub handle refreshes run concurrently
async def test_role_external_handles_refresh_runs_concurrently() -> None:
    import asyncio
    from unittest.mock import MagicMock

    engine = await make_engine(company=make_company(name="Acme"))
    engine.handle_registry = MagicMock()
    engine.mcp_bridge = MagicMock()

    # An ``atlassian`` mcp config + a github config enables both paths.
    engine._mcp_configs = [MagicMock(name="atlassian")]
    engine._mcp_configs[0].name = "atlassian"
    engine._github_config = MagicMock(enabled=True)

    started = asyncio.Event()
    finishing = asyncio.Event()

    async def _slow_jira(*args, **kwargs):
        started.set()
        await finishing.wait()
        return 0

    async def _fast_github(*args, **kwargs):
        # If GitHub waited for Jira this would block on `finishing`.
        # Instead it runs concurrently and releases Jira via `finishing`.
        await started.wait()
        finishing.set()
        return 0

    with (
        patch_register("register_jira_accounts_from_org", _slow_jira),
        patch_register("register_github_accounts_from_org", _fast_github),
    ):
        # Plain timeout: if the calls were sequential, the test would
        # hang on `finishing.wait()` forever and TimeoutError would
        # fire instead of completing.
        await asyncio.wait_for(
            engine._refresh_role_external_handles(only_roles={"Eng"}),
            timeout=1.0,
        )


def patch_register(name: str, replacement):
    from unittest.mock import patch

    return patch(f"crewlet.engine.{name}", replacement)


# entity routes share _parse_body_or_400 helper
async def test_parse_body_or_400_returns_400_on_invalid_json() -> None:
    from starlette.requests import Request

    from crewlet.api.config_entity_routes import _parse_body_or_400

    async def _receive():
        return {"type": "http.request", "body": b"not-json"}

    request = Request(
        scope={"type": "http", "method": "PUT", "path": "/config/identity"},
        receive=_receive,
    )
    body, error = await _parse_body_or_400(request)
    assert body is None
    assert error is not None
    assert error.status_code == 400


async def test_parse_body_or_400_returns_dict_on_valid_json() -> None:
    from starlette.requests import Request

    from crewlet.api.config_entity_routes import _parse_body_or_400

    async def _receive():
        return {"type": "http.request", "body": b'{"mission": "ship pixels"}'}

    request = Request(
        scope={"type": "http", "method": "PUT", "path": "/config/identity"},
        receive=_receive,
    )
    body, error = await _parse_body_or_400(request)
    assert error is None
    assert body == {"mission": "ship pixels"}
