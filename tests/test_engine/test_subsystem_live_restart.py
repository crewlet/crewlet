"""Tests for per-subsystem live restart in ``Engine.apply_config``."""

from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import MagicMock

sys.path.insert(0, str(Path(__file__).parent.parent))
from conftest import make_engine  # noqa: E402
from crewlet.config import CompanyConfig  # noqa: E402

# ── extensions ─────────────────────────────────────────────────────


class _MockExtension:
    def __init__(self, name: str) -> None:
        self.name = name
        self.version = "0.0.1"
        self.registered = False
        self.started = False
        self.stopped = False

    async def on_register(self, ctx) -> None:
        self.registered = True

    async def on_engine_start(self, ctx) -> None:
        self.started = True

    async def on_engine_stop(self, ctx) -> None:
        self.stopped = True


async def test_extensions_diff_live_unregister_and_register() -> None:
    """Adding / removing extensions live calls the lifecycle hooks."""
    from unittest.mock import patch

    engine = await make_engine(
        company=CompanyConfig(name="Acme"),
    )
    # Simulate post-cascade state.
    engine._tier_b_done = True

    # Mock the build_extensions builder for the test: when the config
    # specifies a list of extensions, return our mocks instead of
    # actually importing real packages.
    ext_old = _MockExtension("ext_old")
    ext_new = _MockExtension("ext_new")
    engine._extension_manager._extensions = [ext_old]

    def _build_from_config(cfg):
        if not cfg.extensions:
            return []
        return [ext_new]

    with patch(
        "crewlet.engine_builders.build_extensions",
        _build_from_config,
    ):
        await engine.apply_config(
            CompanyConfig(
                name="Acme",
                extensions=[{"new_ext": {}}],
            )
        )

    assert ext_old.stopped is True
    assert ext_new.registered is True
    assert ext_new.started is True


async def test_extensions_diff_pre_cascade_only_stores_config() -> None:
    """Before the spawn cascade runs, extension diff just updates
    pending_extensions; lifecycle hooks don't fire yet."""
    from unittest.mock import patch

    engine = await make_engine(
        company=CompanyConfig(name="Acme"),
    )
    assert getattr(engine, "_tier_b_done", False) is False

    ext = _MockExtension("ext_pending")
    with patch(
        "crewlet.engine_builders.build_extensions",
        lambda cfg: [ext] if cfg.extensions else [],
    ):
        await engine.apply_config(
            CompanyConfig(
                name="Acme",
                extensions=[{"new_ext": {}}],
            )
        )
    assert ext.started is False  # cascade not run; no live wire
    assert engine._pending_extensions == [ext]


async def test_extensions_unchanged_neighbour_not_restarted() -> None:
    """When one extension's settings change, ONLY that extension
    restarts.  An unrelated extension in the same list whose YAML is
    unchanged must keep its live instance (no unregister, no
    re-register)."""
    from unittest.mock import patch

    engine = await make_engine(
        company=CompanyConfig(
            name="Acme",
            extensions=[{"ext_a": {"setting": "v1"}}, {"ext_b": {"keep": True}}],
        ),
    )
    engine._tier_b_done = True

    # Live instances reflect the prior apply.
    live_a = _MockExtension("ext_a")
    live_b = _MockExtension("ext_b")
    engine._extension_manager._extensions = [live_a, live_b]

    # Build returns fresh instances on each apply (real builders do this).
    fresh_a = _MockExtension("ext_a")
    fresh_b = _MockExtension("ext_b")
    with patch(
        "crewlet.engine_builders.build_extensions",
        lambda cfg: [fresh_a, fresh_b] if cfg.extensions else [],
    ):
        await engine.apply_config(
            CompanyConfig(
                name="Acme",
                extensions=[
                    {"ext_a": {"setting": "v2"}},  # changed
                    {"ext_b": {"keep": True}},  # unchanged
                ],
            )
        )

    # ext_a (the one whose settings changed) restarted.
    assert live_a.stopped is True
    assert fresh_a.started is True

    # ext_b's live instance was NOT touched.
    assert live_b.stopped is False
    assert fresh_b.started is False


# ── notification transports ────────────────────────────────────────


async def test_notification_transports_diff_swaps_service_dict() -> None:
    """When an integration change rebuilds the transports and a
    NotificationService is running, the swap happens in place AND the
    newly-installed transport gets ``start()`` called.  Without start() the transport
    sits with ``_running=False`` and rejects every inbound webhook
    with ``handle_event_after_stop``.
    """
    from unittest.mock import AsyncMock as A
    from unittest.mock import patch

    engine = await make_engine(
        company=CompanyConfig(name="Acme"),
    )
    engine._tier_b_done = True
    fake_service = MagicMock()
    fake_service.transports = {}
    engine.notification_service = fake_service

    new_transport = MagicMock()
    new_transport.name = "transport_a"
    new_transport.start = A()
    new_transport.stop = A()

    with patch(
        "crewlet.engine_builders.build_notification_transports",
        lambda cfg, storage=None: [new_transport] if cfg.integrations.jira else [],
    ):
        await engine.apply_config(
            CompanyConfig(
                name="Acme",
                integrations={"jira": {"url": "https://x.atlassian.net", "token": "t"}},
            )
        )

    assert "transport_a" in fake_service.transports
    new_transport.start.assert_awaited_once()
    new_transport.stop.assert_not_awaited()


async def test_refresh_slack_apps_seeds_registry_from_org() -> None:
    """``_refresh_slack_apps`` re-runs ``register_app`` on the running
    SlackTransport for every role with a Slack token.  Without it, a
    SlackTransport rebuilt by a live integrations PUT (or one that
    survived from boot while roles arrived later) starts with an empty
    ``_apps`` registry and drops every inbound webhook with
    ``no_app_for_handle``.
    """
    from crewlet.notifications.transports.slack import SlackTransport

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    engine.handle_registry = MagicMock()

    captured: list[tuple[str, str]] = []

    class _FakeSlack(SlackTransport):
        def __init__(self):  # type: ignore[no-untyped-def]
            self.name = "slack"
            self._apps = {}
            self._running = False

        def set_handle_registry(self, registry):  # type: ignore[no-untyped-def]
            pass

        def register_app(self, handle, config):  # type: ignore[no-untyped-def]
            captured.append((handle, config.bot_token))
            self._apps[handle] = config

    fake_slack = _FakeSlack()
    fake_service = MagicMock()
    fake_service.transports = {"slack": fake_slack}
    engine.notification_service = fake_service

    engine.org = MagicMock()
    engine.org.all_roles = lambda: [
        MagicMock(
            slack={
                "bot_token": "xoxb-ceo",
                "signing_secret": "ss",
                "channel": "",
            },
            get_handle=lambda: "agent-ceo",
            name="Agent CEO",
        ),
        MagicMock(
            slack={
                "bot_token": "xoxb-pm",
                "signing_secret": "ss",
                "channel": "",
            },
            get_handle=lambda: "agent-pm",
            name="Agent PM",
        ),
        MagicMock(slack={}, get_handle=lambda: "agent-loner", name="Agent Loner"),
    ]

    engine._refresh_slack_apps()

    handles = {h for h, _ in captured}
    assert handles == {"agent-ceo", "agent-pm"}, (
        f"unexpected handle set; captured={captured}"
    )
    assert ("agent-ceo", "xoxb-ceo") in captured
    assert ("agent-pm", "xoxb-pm") in captured


# ── Plane integration (transport + routing re-seed) ────────────────


def _plane_block(**overrides: object) -> dict[str, object]:
    block: dict[str, object] = {
        "enabled": True,
        "url": "https://plane.example.com",
        "workspace": "acme",
        "webhook_secret": "wh-secret",
    }
    block.update(overrides)
    return block


def _fake_plane_transport():
    """A ``PlaneTransport`` subclass whose setters record instead of
    wiring — passes the ``isinstance`` guard in the engine refreshers."""
    from crewlet.notifications.transports.plane import PlaneTransport

    class _FakePlane(PlaneTransport):
        def __init__(self):  # type: ignore[no-untyped-def]
            self.name = "plane"
            self.registries: list[object] = []
            self.lead_maps: list[dict[str, str]] = []
            self.excluded: list[list[str]] = []

        def set_handle_registry(self, registry):  # type: ignore[no-untyped-def]
            self.registries.append(registry)

        def set_project_leads(self, mapping):  # type: ignore[no-untyped-def]
            self.lead_maps.append(dict(mapping))

        def set_notification_excluded_projects(self, keys):  # type: ignore[no-untyped-def]
            self.excluded.append(list(keys))

    return _FakePlane()


def _fake_jira_transport():
    """A ``JiraTransport`` subclass whose setters record instead of
    wiring — passes the ``isinstance`` guard in the engine refreshers."""
    from crewlet.notifications.transports.jira import JiraTransport

    class _FakeJira(JiraTransport):
        def __init__(self):  # type: ignore[no-untyped-def]
            self.name = "jira"
            self.registries: list[object] = []
            self.lead_maps: list[dict[str, str]] = []

        def set_handle_registry(self, registry):  # type: ignore[no-untyped-def]
            self.registries.append(registry)

        def set_project_key_leads(self, mapping):  # type: ignore[no-untyped-def]
            self.lead_maps.append(dict(mapping))

    return _FakeJira()


async def test_plane_integrations_diff_rebuilds_transport_live() -> None:
    """A live ``integrations.plane`` PUT rebuilds the transport set
    (the new dict carries a started ``PlaneTransport``) and updates
    ``self._plane_config`` from the new revision."""
    from crewlet.notifications.transports.plane import PlaneTransport

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    engine._tier_b_done = True
    fake_service = MagicMock()
    fake_service.transports = {}
    engine.notification_service = fake_service

    applied = await engine.apply_config(
        CompanyConfig(name="Acme", integrations={"plane": _plane_block()})
    )

    assert "integrations_plane" in applied
    assert engine._plane_config is not None
    assert engine._plane_config.enabled is True
    assert engine._plane_config.url == "https://plane.example.com"
    transport = fake_service.transports.get("plane")
    assert isinstance(transport, PlaneTransport)


async def test_unit_plane_project_edit_reseeds_lead_map_live() -> None:
    """An org-only edit to a unit's ``integrations.plane.project`` must
    (1) fire the org diff at all — the unit signature covers the
    per-unit integration identities — and (2) re-seed the RUNNING
    transports' lead maps via ``_reseed_notification_routing``: an org
    edit never rebuilds a transport, so without the org-diff re-seed
    the running transport keeps routing on the stale map."""
    base_unit = {
        "name": "Core",
        "type": "team",
        "lead": "Agent Lead",
        "roles": [{"name": "Agent Lead"}],
    }
    engine = await make_engine(company=CompanyConfig(name="Acme", units=[base_unit]))
    engine._tier_b_done = True

    fake_plane = _fake_plane_transport()
    fake_service = MagicMock()
    fake_service.transports = {"plane": fake_plane}
    engine.notification_service = fake_service

    updated_unit = dict(base_unit)
    updated_unit["integrations"] = {"plane": {"project": "eng"}}
    applied = await engine.apply_config(
        CompanyConfig(name="Acme", units=[updated_unit])
    )

    assert "org" in applied, "unit plane_project edit did not dispatch the org diff"
    assert fake_plane.lead_maps, "running PlaneTransport lead map was not re-seeded"
    assert fake_plane.lead_maps[-1] == {"ENG": "agent-lead"}


async def test_unit_plane_project_removal_clears_lead_map_live() -> None:
    """Removing the last ``integrations.plane.project`` from the org
    must push an EMPTY map onto the running transport.  The setter
    replaces the dict wholesale, so ``{}`` is the only way to clear —
    a truthiness guard around the setter would leave the transport
    routing every unassigned/intake/page event to the removed
    project's lead until restart."""
    unit_with = {
        "name": "Core",
        "type": "team",
        "lead": "Agent Lead",
        "roles": [{"name": "Agent Lead"}],
        "integrations": {"plane": {"project": "eng"}},
    }
    engine = await make_engine(company=CompanyConfig(name="Acme", units=[unit_with]))
    engine._tier_b_done = True

    fake_plane = _fake_plane_transport()
    fake_service = MagicMock()
    fake_service.transports = {"plane": fake_plane}
    engine.notification_service = fake_service

    unit_without = {k: v for k, v in unit_with.items() if k != "integrations"}
    applied = await engine.apply_config(
        CompanyConfig(name="Acme", units=[unit_without])
    )

    assert "org" in applied, "removing the plane identity did not fire the org diff"
    assert fake_plane.lead_maps, "running PlaneTransport lead map was never pushed"
    assert fake_plane.lead_maps[-1] == {}


async def test_unit_jira_project_removal_clears_lead_map_live() -> None:
    """Same non-empty→empty transition for the pre-existing
    ``JiraTransport`` — the guard removal covers all three refreshers."""
    unit_with = {
        "name": "Core",
        "type": "team",
        "lead": "Agent Lead",
        "roles": [{"name": "Agent Lead"}],
        "integrations": {"jira": {"project": "eng"}},
    }
    engine = await make_engine(company=CompanyConfig(name="Acme", units=[unit_with]))
    engine._tier_b_done = True

    fake_jira = _fake_jira_transport()
    fake_service = MagicMock()
    fake_service.transports = {"jira": fake_jira}
    engine.notification_service = fake_service

    # Seed the running transport from the current org, then remove the
    # identity live.
    engine._reseed_notification_routing()
    assert fake_jira.lead_maps[-1] == {"ENG": "agent-lead"}

    unit_without = {k: v for k, v in unit_with.items() if k != "integrations"}
    applied = await engine.apply_config(
        CompanyConfig(name="Acme", units=[unit_without])
    )

    assert "org" in applied, "removing the jira identity did not fire the org diff"
    assert fake_jira.lead_maps[-1] == {}


async def test_knowledge_plane_projects_edit_swaps_org() -> None:
    """A ``knowledge.plane_projects``-only edit must dispatch the org
    diff (the scope materialises onto ``Organization.plane_projects``)."""
    engine = await make_engine(
        company=CompanyConfig(
            name="Acme",
            integrations={"plane": _plane_block()},
            knowledge={"plane_projects": ["ENG"]},
        )
    )
    engine._tier_b_done = True
    assert engine.org.plane_projects == ["ENG"]

    applied = await engine.apply_config(
        CompanyConfig(
            name="Acme",
            integrations={"plane": _plane_block()},
            knowledge={"plane_projects": ["ENG", "PROD"]},
        )
    )

    assert "org" in applied
    assert engine.org.plane_projects == ["ENG", "PROD"]


async def test_refresh_plane_routing_seeds_leads_and_exclusions() -> None:
    """``_refresh_plane_routing`` re-seeds handle registry, lead map,
    and the tool-skills exclusion — and must not raise when the spawn
    cascade never assigned ``_tool_skill_project_key`` (getattr
    guard)."""
    engine = await make_engine(
        company=CompanyConfig(
            name="Acme",
            units=[
                {
                    "name": "Core",
                    "type": "team",
                    "lead": "Agent Lead",
                    "roles": [{"name": "Agent Lead"}],
                    "integrations": {"plane": {"project": "ENG"}},
                }
            ],
        )
    )
    engine.handle_registry = MagicMock()
    assert not hasattr(engine, "_tool_skill_project_key")

    fake_plane = _fake_plane_transport()
    engine._refresh_plane_routing(fake_plane)

    assert fake_plane.registries == [engine.handle_registry]
    assert fake_plane.lead_maps == [{"ENG": "agent-lead"}]
    assert fake_plane.excluded == []  # no skills project key set yet

    engine._tool_skill_project_key = "TS"
    engine._refresh_plane_routing(fake_plane)
    assert fake_plane.excluded == [["TS"]]

    # A foreign-typed transport under the "plane" key no-ops.
    engine._refresh_plane_routing(object())


async def test_embedded_api_resolves_the_plane_webhook_secret(monkeypatch) -> None:
    """The embedded API ends up with the resolved Plane webhook secret —
    the seam ``/webhooks/plane`` verifies against.

    Asserted on the app's state rather than on a ``create_app`` argument:
    the secret is now DERIVED from the active config by the same
    ``attach_config_refresh`` path the standalone API uses, so there is no
    parameter to observe and nothing left that only one deployment does.
    """
    import uvicorn

    async def _noop_serve(self, sockets=None):  # type: ignore[no-untyped-def]
        return None

    monkeypatch.setattr(uvicorn.Server, "serve", _noop_serve)

    engine = await make_engine(
        company=CompanyConfig(
            name="Acme",
            integrations={"plane": _plane_block(webhook_secret="wh-secret-123")},
        )
    )
    await engine._start_embedded_api()
    try:
        assert engine._api_app.state.plane_webhook_secret == "wh-secret-123"
    finally:
        if engine._api_serve_task is not None:
            await engine._api_serve_task


async def test_embedded_api_plane_secret_none_when_unconfigured(
    monkeypatch,
) -> None:
    """Without a Plane integration the secret stays falsy (the route then
    answers 500 ``webhook verification not configured``)."""
    import uvicorn

    async def _noop_serve(self, sockets=None):  # type: ignore[no-untyped-def]
        return None

    monkeypatch.setattr(uvicorn.Server, "serve", _noop_serve)

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    await engine._start_embedded_api()
    try:
        assert not engine._api_app.state.plane_webhook_secret
    finally:
        if engine._api_serve_task is not None:
            await engine._api_serve_task


def _mk_prompt_skill(key: str = "tool:stale", page_id: str = "P-STALE"):
    from crewlet.agent.skills.models import PromptSkill, TriggerExpr

    return PromptSkill(
        key=key,
        trigger=TriggerExpr(tool="x"),
        title=key,
        summary="s",
        body="b",
        source_page_id=page_id,
    )


def _real_confluence_transport():
    from crewlet.config import ConfluenceConfig
    from crewlet.notifications.transports.confluence import ConfluenceTransport

    return ConfluenceTransport(
        ConfluenceConfig(url="https://x.atlassian.net/wiki", token="t")
    )


async def test_knowledge_backend_cutover_confluence_to_plane(monkeypatch) -> None:
    """A live confluence→plane cut-over must swap the WHOLE knowledge
    stack: searcher (re-pointed on the TurnEngine — it captures the
    reference at construction), sync worker, index callback on the new
    transport, and a registry re-seed off the new backend (M3 + M4)."""

    from crewlet.agent.skills import PlaneSkillSyncWorker
    from crewlet.knowledge.confluence_search import ConfluenceSearcher
    from crewlet.knowledge.plane_search import PlaneSearcher
    from crewlet.notifications.transports.plane import PlaneTransport

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    engine._tier_b_done = True
    engine._tool_skill_space_key = "TS"
    engine._tool_skill_project_key = "TS"

    # Simulate the boot-time confluence wiring.
    conf = _real_confluence_transport()
    fake_service = MagicMock()
    fake_service.transports = {"confluence": conf}
    engine.notification_service = fake_service
    engine._confluence_transport = conf
    engine._plane_transport = None
    engine._knowledge_searcher = ConfluenceSearcher(transport=conf)
    engine._wire_confluence_skill_sync(conf)
    old_worker = engine._tool_skill_sync_worker
    assert old_worker is not None
    engine._prompt_skill_registry.seed([_mk_prompt_skill()])
    turn_engine = MagicMock()
    engine.turn_engine = turn_engine

    # Record the plane boot walk instead of hitting REST — and mimic a
    # SUCCESSFUL walk's semantics (wholesale seed), so the test can
    # assert the old backend's skills actually drop out.
    walked: list[object] = []

    async def _fake_sync(self):  # type: ignore[no-untyped-def]
        walked.append(self)
        self._registry.seed([_mk_prompt_skill("tool:plane-era", "PLANE-1")])
        return 1

    monkeypatch.setattr(PlaneSkillSyncWorker, "run_initial_sync", _fake_sync)

    await engine.apply_config(
        CompanyConfig(name="Acme", integrations={"plane": _plane_block()})
    )
    assert engine._tool_skill_resync_task is not None
    await engine._tool_skill_resync_task  # let the kicked re-seed run

    plane = fake_service.transports.get("plane")
    assert isinstance(plane, PlaneTransport)
    # Searcher swapped and re-pointed on the TurnEngine.
    assert isinstance(engine._knowledge_searcher, PlaneSearcher)
    turn_engine.set_knowledge_searcher.assert_called_with(engine._knowledge_searcher)
    # Worker swapped (single attribute — never both backends at once)
    # and the index callback registered on the NEW transport.
    assert isinstance(engine._tool_skill_sync_worker, PlaneSkillSyncWorker)
    assert engine._tool_skill_sync_worker._transport is plane
    assert plane._index_callback is not None
    assert engine._confluence_transport is None
    assert engine._plane_transport is plane
    # M4: the re-wire path kicked the full re-populate, and the seed
    # replaced the Confluence-era skills wholesale.
    assert walked == [engine._tool_skill_sync_worker]
    assert engine._prompt_skill_registry.keys() == ["tool:plane-era"]


async def test_knowledge_backend_cutover_failed_walk_retries_then_goes_loud(
    monkeypatch, caplog
) -> None:
    """A cut-over whose walk keeps failing (backend still booting, or
    genuinely broken) retries with backoff and then logs
    ``tool_skill_resync_exhausted`` LOUDLY: the registry keeps the OLD
    backend's skills — better than an empty prompt surface — but the
    operator is told explicitly instead of serving stale prose in
    silence forever."""
    import asyncio

    import crewlet.engine as engine_mod
    from crewlet.agent.skills import PlaneSkillSyncWorker

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    engine._tier_b_done = True
    engine._tool_skill_space_key = "TS"
    engine._tool_skill_project_key = "TS"

    conf = _real_confluence_transport()
    fake_service = MagicMock()
    fake_service.transports = {"confluence": conf}
    engine.notification_service = fake_service
    engine._confluence_transport = conf
    engine._plane_transport = None
    engine._wire_confluence_skill_sync(conf)
    engine._prompt_skill_registry.seed([_mk_prompt_skill()])
    engine.turn_engine = MagicMock()

    attempts: list[int] = []

    async def _failing_sync(self):  # type: ignore[no-untyped-def]
        attempts.append(1)
        return None  # walk failed — registry untouched by contract

    monkeypatch.setattr(PlaneSkillSyncWorker, "run_initial_sync", _failing_sync)
    monkeypatch.setattr(engine_mod, "_TOOL_SKILL_RESYNC_ATTEMPTS", 3)
    monkeypatch.setattr(engine_mod, "_TOOL_SKILL_RESYNC_BASE_DELAY_SECONDS", 0.0)

    await engine.apply_config(
        CompanyConfig(name="Acme", integrations={"plane": _plane_block()})
    )
    assert engine._tool_skill_resync_task is not None
    await engine._tool_skill_resync_task
    await asyncio.sleep(0)

    assert len(attempts) == 3  # bounded retry, then gave up
    # Fail-stale, loudly: the old backend's skills survive…
    assert engine._prompt_skill_registry.keys() == ["tool:stale"]
    # …and the exhaustion is unmissable in the logs.
    assert "tool_skill_resync_exhausted" in caplog.text
    assert "tool_skill_resync_retry" in caplog.text


async def test_select_knowledge_backend_tiebreak_is_confluence_everywhere() -> None:
    """One shared helper decides the active backend, so the searcher /
    sync-worker wiring, the live-refresh reconcile, and the promotion
    writer can never disagree.  Config validation makes both-present
    unreachable; the defensive tiebreak is Confluence-first, matching
    ``start()``'s historical order."""
    from crewlet.confluence.promotion import ConfluencePromotionWriter
    from crewlet.knowledge.confluence_search import ConfluenceSearcher
    from crewlet.notifications.transports.plane import PlaneTransport

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    engine._tool_skill_space_key = "TS"
    engine._tool_skill_project_key = "TS"

    conf = _real_confluence_transport()
    from crewlet.config import PlaneConfig as _PlaneConfig

    plane = PlaneTransport(_PlaneConfig.model_validate(_plane_block()))

    assert engine._select_knowledge_backend(conf, plane) == ("confluence", conf)
    assert engine._select_knowledge_backend(None, plane) == ("plane", plane)
    assert engine._select_knowledge_backend(conf, None) == ("confluence", conf)
    assert engine._select_knowledge_backend(None, None) == ("none", None)

    # The install path follows the same tiebreak: with both transports
    # (unreachable via config validation) Confluence wins for the
    # searcher, the worker, AND the promotion writer — no split-brain.
    engine._confluence_transport = conf
    engine._plane_transport = plane
    backend = engine._install_knowledge_backend()
    assert backend == "confluence"
    assert isinstance(engine._knowledge_searcher, ConfluenceSearcher)
    from crewlet.agent.skills import ToolSkillSyncWorker

    assert isinstance(engine._tool_skill_sync_worker, ToolSkillSyncWorker)
    assert isinstance(engine._build_promotion_page_writer(), ConfluencePromotionWriter)


async def test_knowledge_backend_removal_nulls_searcher_and_worker() -> None:
    """Removing the knowledge integration outright must null the
    searcher (TurnEngine re-pointed to None — M3's removal direction),
    drop the worker, and clear the registry (nothing left to source
    skills from)."""
    from crewlet.knowledge.plane_search import PlaneSearcher
    from crewlet.notifications.transports.plane import PlaneTransport

    engine = await make_engine(
        company=CompanyConfig(name="Acme", integrations={"plane": _plane_block()})
    )
    engine._tier_b_done = True
    engine._tool_skill_project_key = "TS"
    engine._tool_skill_space_key = "TS"

    # Simulate the boot-time plane wiring on a real transport.
    from crewlet.config import PlaneConfig as _PlaneConfig

    plane = PlaneTransport(_PlaneConfig.model_validate(_plane_block()))
    fake_service = MagicMock()
    fake_service.transports = {"plane": plane}
    engine.notification_service = fake_service
    engine._confluence_transport = None
    engine._plane_transport = plane
    engine._knowledge_searcher = PlaneSearcher(transport=plane, skills_project="TS")
    engine._wire_plane_skill_sync(plane)
    assert engine._tool_skill_sync_worker is not None
    engine._prompt_skill_registry.seed([_mk_prompt_skill()])
    turn_engine = MagicMock()
    engine.turn_engine = turn_engine

    await engine.apply_config(CompanyConfig(name="Acme"))

    assert engine._knowledge_searcher is None
    assert engine._tool_skill_sync_worker is None
    assert engine._plane_transport is None
    turn_engine.set_knowledge_searcher.assert_called_with(None)
    assert len(engine._prompt_skill_registry) == 0


async def test_org_only_edit_does_not_rebuild_knowledge_stack() -> None:
    """An org-only edit re-seeds routing but must NOT rebuild the
    searcher/worker or kick a registry re-walk — the reconcile's
    transport-identity check keeps it a no-op (scope reads ``org`` per
    call)."""
    from unittest.mock import AsyncMock as A

    from crewlet.knowledge.plane_search import PlaneSearcher
    from crewlet.notifications.transports.plane import PlaneTransport

    engine = await make_engine(
        company=CompanyConfig(name="Acme", integrations={"plane": _plane_block()})
    )
    engine._tier_b_done = True
    engine._tool_skill_project_key = "TS"
    # The org edit spawns the new role; stub the queue like the other
    # role-add tests (the MemoryEventQueue is not started here).
    engine.event_queue = MagicMock()
    engine.event_queue.publish = A()
    engine.event_queue.subscribe = A()
    engine.agent_pool._event_queue = engine.event_queue

    from crewlet.config import PlaneConfig as _PlaneConfig

    plane = PlaneTransport(_PlaneConfig.model_validate(_plane_block()))
    fake_service = MagicMock()
    fake_service.transports = {"plane": plane}
    engine.notification_service = fake_service
    engine._confluence_transport = None
    engine._plane_transport = plane
    searcher = PlaneSearcher(transport=plane, skills_project="TS")
    engine._knowledge_searcher = searcher
    engine._wire_plane_skill_sync(plane)
    worker = engine._tool_skill_sync_worker

    await engine.apply_config(
        CompanyConfig(
            name="Acme",
            integrations={"plane": _plane_block()},
            units=[
                {
                    "name": "Core",
                    "type": "team",
                    "lead": "Agent Lead",
                    "roles": [{"name": "Agent Lead"}],
                }
            ],
        )
    )

    assert engine._knowledge_searcher is searcher  # same object, no rebuild
    assert engine._tool_skill_sync_worker is worker


async def test_wire_helpers_respect_disabled_container_keys() -> None:
    """An empty CREWLET_TOOL_SKILLS_PROJECT / _SPACE disables the sync:
    the wiring helpers construct no worker and register no callback."""
    engine = await make_engine(company=CompanyConfig(name="Acme"))
    engine._tool_skill_sync_worker = None
    engine._tool_skill_project_key = ""
    engine._tool_skill_space_key = ""

    fake_plane = _fake_plane_transport()
    engine._wire_plane_skill_sync(fake_plane)
    assert engine._tool_skill_sync_worker is None

    conf = _real_confluence_transport()
    engine._wire_confluence_skill_sync(conf)
    assert engine._tool_skill_sync_worker is None


async def test_build_promotion_page_writer_follows_active_backend() -> None:
    """The promotion gate generalises from ConfluenceTransport-presence
    to active-knowledge-backend: confluence ⇒ Confluence writer, plane
    ⇒ Plane writer, neither ⇒ None (promotion pass soft-no-ops)."""
    from crewlet.confluence.promotion import ConfluencePromotionWriter
    from crewlet.plane.promotion import PlanePromotionWriter

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    assert engine._build_promotion_page_writer() is None

    engine._confluence_transport = _real_confluence_transport()
    engine._plane_transport = None
    assert isinstance(engine._build_promotion_page_writer(), ConfluencePromotionWriter)

    engine._confluence_transport = None
    from crewlet.config import PlaneConfig as _PlaneConfig
    from crewlet.notifications.transports.plane import PlaneTransport

    engine._plane_transport = PlaneTransport(
        _PlaneConfig.model_validate(_plane_block())
    )
    assert isinstance(engine._build_promotion_page_writer(), PlanePromotionWriter)


# ── MCP server live restart (mocked bridge) ────────────────────────


async def test_mcp_server_added_starts_new_client_live() -> None:
    """Adding an MCP server to the config restarts the bridge for it."""
    from unittest.mock import AsyncMock as A

    engine = await make_engine(
        company=CompanyConfig(name="Acme"),
    )
    engine._tier_b_done = True
    # Mock the MCP bridge.
    bridge = MagicMock()
    bridge.restart_server = A(return_value=[])
    bridge.stop_server = A()
    engine.mcp_bridge = bridge

    await engine.apply_config(
        CompanyConfig(
            name="Acme",
            mcp_servers=[
                {
                    "name": "calc",
                    "command": "uvx",
                    "args": ["mcp-calc"],
                    "shared": True,
                }
            ],
        )
    )
    bridge.restart_server.assert_awaited()
    call_kwargs = bridge.restart_server.await_args.kwargs
    assert call_kwargs["name"] == "calc"


async def test_mcp_server_removed_stops_bridge_client() -> None:
    """Removing an MCP server stops the bridge client for it."""
    from unittest.mock import AsyncMock as A

    engine = await make_engine(
        company=CompanyConfig(
            name="Acme",
            mcp_servers=[
                {
                    "name": "calc",
                    "command": "uvx",
                    "args": ["mcp-calc"],
                    "shared": True,
                }
            ],
        ),
    )
    engine._tier_b_done = True
    bridge = MagicMock()
    bridge.stop_server = A()
    bridge.restart_server = A(return_value=[])
    engine.mcp_bridge = bridge

    await engine.apply_config(CompanyConfig(name="Acme"))
    bridge.stop_server.assert_awaited_with("calc")


async def test_atlassian_change_does_not_restart_unrelated_mcp_servers() -> None:
    """Editing only ``confluence:`` must NOT restart calc / github / etc.

    Regression guard: ``force_atlassian_restart=True`` must pass the
    real ``old_configs`` to ``_apply_mcp_servers_live`` — an empty list
    makes every shared MCP look "added" and triggers a full restart.
    """
    from unittest.mock import AsyncMock as A

    from crewlet.config import ConfluenceConfig

    engine = await make_engine(
        company=CompanyConfig(
            name="Acme",
            mcp_servers=[
                {
                    "name": "calc",
                    "command": "uvx",
                    "args": ["mcp-calc"],
                    "shared": True,
                },
            ],
        ),
    )
    engine._tier_b_done = True
    bridge = MagicMock()
    bridge.restart_server = A(return_value=[])
    bridge.stop_server = A()
    engine.mcp_bridge = bridge

    # Editing confluence (no atlassian MCP in the prior config) should
    # not restart calc.  Even with the atlassian-changed branch firing,
    # the diff inside _apply_mcp_servers_live must see calc as unchanged.
    await engine.apply_config(
        CompanyConfig(
            name="Acme",
            mcp_servers=[
                {
                    "name": "calc",
                    "command": "uvx",
                    "args": ["mcp-calc"],
                    "shared": True,
                },
            ],
            integrations={
                "confluence": ConfluenceConfig(
                    url="https://x.atlassian.net/wiki", token="t"
                )
            },
        )
    )
    # calc must NOT be restarted (unchanged).
    for call in bridge.restart_server.await_args_list:
        assert call.kwargs.get("name") != "calc", (
            f"calc was restarted spuriously: {call}"
        )


# ── per-role MCP respawn on role-add (live) ────────────────────────


async def test_role_added_live_spawns_per_role_mcp() -> None:
    """Adding a new role via ``apply_config`` after first activation
    must spawn that role's per-role MCP instances from the ``shared:
    false`` templates in ``mcp_servers`` (atlassian + slack stdio,
    github http).  Without this, the new agent would boot with only the
    built-in tools -- ``_start_mcp_servers`` only runs at first
    activation, so live role-adds mirror that per-role spawn loop in
    ``_apply_org_diff``.
    """
    from unittest.mock import AsyncMock as A

    from crewlet.config import MCPServerConfig as MCPCfg

    engine = await make_engine(
        company=CompanyConfig(name="Acme"),
    )
    engine._tier_b_done = True

    # Engine's _apply_org_diff publishes agent_spawned + subscribes the
    # new agent's inbox; stub the queue so the test exercises only the
    # per-role MCP spawn we're verifying.
    engine.event_queue = MagicMock()
    engine.event_queue.publish = A()
    engine.event_queue.subscribe = A()
    # ``AgentPool`` captured the engine's queue at construction; mirror
    # the stub onto it so ``spawn_role``'s AgentSpawned publish lands
    # on the same mock instead of the original MemoryEventQueue (which
    # is not started in these focused tests).
    engine.agent_pool._event_queue = engine.event_queue

    bridge = MagicMock()
    bridge.stop_server = A()
    bridge.add_server = A(return_value=[])
    bridge.add_http_server = A(return_value=[])
    engine.mcp_bridge = bridge

    # All tool servers are generic ``mcp_servers`` entries now; the
    # per-role ones are ``shared: false`` templates (stdio + http).
    engine._mcp_configs = [
        MCPCfg(
            name="atlassian",
            command="uvx",
            args=["--from", "x", "mcp-atlassian"],
            env={"JIRA_URL": "https://a.atlassian.com/x/jira/cid"},
            shared=False,
        ),
        MCPCfg(
            name="slack",
            command="npm",
            args=["exec", "slack-mcp-server"],
            shared=False,
            tool_prefix="slack_",
        ),
        MCPCfg(
            name="github",
            transport="http",
            url="https://api.githubcopilot.com/mcp/",
            shared=False,
        ),
    ]

    await engine.apply_config(
        CompanyConfig(
            name="Acme",
            units=[
                {
                    "name": "Core",
                    "type": "team",
                    "roles": [
                        {
                            "name": "Agent SWE",
                            "mcp_env": {
                                "atlassian": {
                                    "JIRA_USERNAME": "swe",
                                    "JIRA_API_TOKEN": "swe-tok",
                                },
                                "slack": {"SLACK_MCP_XOXB_TOKEN": "xoxb-swe"},
                                "github": {"Authorization": "Bearer ghp_swe"},
                            },
                        }
                    ],
                }
            ],
        )
    )

    # Per-role atlassian MCP spawned (stdio).
    atlassian_calls = [
        c
        for c in bridge.add_server.await_args_list
        if "atlassian" in c.kwargs.get("name", "")
    ]
    assert atlassian_calls, "per-role atlassian MCP was not spawned"

    # Per-role slack MCP spawned (stdio, token from mcp_env.slack).
    slack_calls = [
        c
        for c in bridge.add_server.await_args_list
        if "slack" in c.kwargs.get("name", "")
    ]
    assert slack_calls, "per-role slack MCP was not spawned"
    slack_env = slack_calls[0].kwargs.get("env", {})
    assert slack_env.get("SLACK_MCP_XOXB_TOKEN") == "xoxb-swe"

    # Per-role GitHub remote MCP spawned via http, with the role's
    # mcp_env.github applied as an Authorization header.
    github_calls = [
        c
        for c in bridge.add_http_server.await_args_list
        if "github" in c.kwargs.get("name", "")
    ]
    assert github_calls, "per-role github MCP was not spawned"
    gh_headers = github_calls[0].kwargs.get("headers", {})
    assert gh_headers.get("Authorization") == "Bearer ghp_swe"


async def test_role_added_live_without_mcp_creds_is_noop() -> None:
    """A new role without ``mcp_env`` doesn't spin up per-role MCP
    instances -- the engine only spawns what the role declared,
    mirroring first-activation behaviour."""
    from unittest.mock import AsyncMock as A

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    engine._tier_b_done = True

    engine.event_queue = MagicMock()
    engine.event_queue.publish = A()
    engine.event_queue.subscribe = A()
    # ``AgentPool`` captured the engine's queue at construction; mirror
    # the stub onto it so ``spawn_role``'s AgentSpawned publish lands
    # on the same mock instead of the original MemoryEventQueue (which
    # is not started in these focused tests).
    engine.agent_pool._event_queue = engine.event_queue

    bridge = MagicMock()
    bridge.stop_server = A()
    bridge.add_server = A(return_value=[])
    bridge.add_http_server = A(return_value=[])
    engine.mcp_bridge = bridge

    await engine.apply_config(
        CompanyConfig(
            name="Acme",
            units=[
                {
                    "name": "Core",
                    "type": "team",
                    "roles": [{"name": "Plain"}],
                }
            ],
        )
    )

    bridge.add_server.assert_not_awaited()
    bridge.add_http_server.assert_not_awaited()


async def test_role_added_with_unknown_mcp_server_logs_warnings(
    monkeypatch,
) -> None:
    """A role whose ``mcp_env`` names a server absent from ``mcp_servers``
    (a typo, or a server that was never declared) spawns nothing.  Surface
    it at role-add time via ``mcp_env_unknown_server`` +
    ``role_has_no_per_role_mcp_tools`` instead of leaving the operator to
    discover the empty tool surface at turn time.
    """
    from unittest.mock import AsyncMock as A

    import crewlet.engine as engine_mod

    # Spy on every ``logger.warning(event, ...)`` call inside engine.py.
    warnings: list[tuple[str, dict]] = []
    real_warning = engine_mod.logger.warning

    def _capture(event, **kwargs):  # type: ignore[no-untyped-def]
        warnings.append((event, kwargs))
        return real_warning(event, **kwargs)

    monkeypatch.setattr(engine_mod.logger, "warning", _capture)

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    engine._tier_b_done = True

    engine.event_queue = MagicMock()
    engine.event_queue.publish = A()
    engine.event_queue.subscribe = A()
    # ``AgentPool`` captured the engine's queue at construction; mirror
    # the stub onto it so ``spawn_role``'s AgentSpawned publish lands
    # on the same mock instead of the original MemoryEventQueue (which
    # is not started in these focused tests).
    engine.agent_pool._event_queue = engine.event_queue

    bridge = MagicMock()
    bridge.stop_server = A()
    bridge.add_server = A(return_value=[])
    bridge.add_http_server = A(return_value=[])
    engine.mcp_bridge = bridge
    # No ``mcp_servers`` declared, so the role's mcp_env references an
    # unknown server.
    engine._mcp_configs = []

    await engine.apply_config(
        CompanyConfig(
            name="Acme",
            units=[
                {
                    "name": "Core",
                    "type": "team",
                    "roles": [
                        {
                            "name": "Agent CTO",
                            "mcp_env": {"nonexistent": {"TOKEN": "x"}},
                        }
                    ],
                }
            ],
        )
    )

    # Nothing spawned for the unknown server.
    bridge.add_server.assert_not_awaited()
    bridge.add_http_server.assert_not_awaited()
    # The role's bucket is the (none) case downstream consumers see.
    assert "Agent CTO" not in engine._role_mcp_tools

    events = [e for e, _ in warnings]
    assert "mcp_env_unknown_server" in events
    assert "role_has_no_per_role_mcp_tools" in events


async def test_per_entity_bootstrap_lazily_creates_mcp_bridge(monkeypatch) -> None:
    """Per-entity bootstrap from an empty stub omits MCP at first
    activation, so ``_start_mcp_servers`` early-returns and the engine
    starts without an ``MCPToolBridge``.  Later live additions
    (``POST /config/mcp-servers``, ``PUT /config/integrations/jira``,
    ``POST /config/roles`` with ``mcp_env``) must lazy-create the
    bridge instead of silently no-op'ing -- otherwise the documented
    per-entity recipe leaves the engine with zero MCP tools even
    after every integration / server / role is configured.
    """
    from unittest.mock import AsyncMock as A
    from unittest.mock import MagicMock

    import crewlet.engine as engine_mod
    from crewlet.config import MCPServerConfig as MCPCfg

    # Stub MCPToolBridge so add_server / add_http_server are
    # observable without spawning real subprocesses.
    bridge = MagicMock()
    bridge.stop_server = A()
    bridge.add_server = A(return_value=[])
    bridge.add_http_server = A(return_value=[])
    bridge.restart_server = A(return_value=[])
    monkeypatch.setattr(engine_mod, "MCPToolBridge", lambda: bridge)

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    engine._tier_b_done = True
    assert engine.mcp_bridge is None  # stub bootstrap left no MCP

    engine.event_queue = MagicMock()
    engine.event_queue.publish = A()
    engine.event_queue.subscribe = A()
    # ``AgentPool`` captured the engine's queue at construction; mirror
    # the stub onto it so ``spawn_role``'s AgentSpawned publish lands
    # on the same mock instead of the original MemoryEventQueue (which
    # is not started in these focused tests).
    engine.agent_pool._event_queue = engine.event_queue

    # Pretend the mcp-servers PUTs landed earlier and populated
    # ``_mcp_configs`` (atlassian + slack stdio, github http, plus a
    # shared server), but the bridge was not created (the pre-fix
    # behaviour we are guarding against).
    engine._mcp_configs = [
        MCPCfg(
            name="atlassian",
            command="uvx",
            args=["mcp-atlassian"],
            env={"JIRA_URL": "https://x.atlassian.com/x/jira/cid"},
            shared=False,
        ),
        MCPCfg(
            name="slack",
            command="npm",
            args=["exec", "slack-mcp-server"],
            shared=False,
            tool_prefix="slack_",
        ),
        MCPCfg(
            name="github",
            transport="http",
            url="https://api.githubcopilot.com/mcp/",
            shared=False,
        ),
        MCPCfg(
            name="tavily",
            command="npm",
            args=["exec", "tavily-mcp"],
            env={"TAVILY_API_KEY": "k"},
            shared=True,
        ),
    ]

    await engine.apply_config(
        CompanyConfig(
            name="Acme",
            units=[
                {
                    "name": "Core",
                    "type": "team",
                    "roles": [
                        {
                            "name": "Agent CEO",
                            "mcp_env": {
                                "atlassian": {"JIRA_USERNAME": "ceo"},
                                "slack": {"SLACK_MCP_XOXB_TOKEN": "xoxb-ceo"},
                                "github": {"Authorization": "Bearer ghp_ceo"},
                            },
                        }
                    ],
                }
            ],
        )
    )

    # Bridge created on demand.
    assert engine.mcp_bridge is bridge
    # All three per-role surfaces spawned via the lazy-created bridge.
    assert bridge.add_server.await_count >= 2  # atlassian + slack
    assert bridge.add_http_server.await_count >= 1  # github


async def test_role_add_registers_slack_app_before_org_swap() -> None:
    """In ``_apply_org_diff``, the role-add branch runs BEFORE
    ``self.org = new_org``.  Calling ``_refresh_slack_apps`` here
    (which walks ``self.org``) would iterate the OLD org and miss the
    newly-added role -- the role's Slack app would never be registered
    and subsequent webhooks would drop with ``no_app_for_handle``.
    Register the single new role directly to avoid the ordering trap.
    """
    from unittest.mock import AsyncMock as A

    from crewlet.notifications.transports.slack import (
        SlackAppConfig,
        SlackTransport,
    )

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    engine._tier_b_done = True
    engine.handle_registry = MagicMock()

    # Stub out event queue + agent pool wiring; we only care about
    # whether the slack app gets registered before the org swap.
    engine.event_queue = MagicMock()
    engine.event_queue.publish = A()
    engine.event_queue.subscribe = A()
    # ``AgentPool`` captured the engine's queue at construction; mirror
    # the stub onto it so ``spawn_role``'s AgentSpawned publish lands
    # on the same mock instead of the original MemoryEventQueue (which
    # is not started in these focused tests).
    engine.agent_pool._event_queue = engine.event_queue

    captured: list[tuple[str, SlackAppConfig]] = []

    class _FakeSlack(SlackTransport):
        def __init__(self):  # type: ignore[no-untyped-def]
            self.name = "slack"
            self._apps = {}
            self._running = True

        def set_handle_registry(self, registry):  # type: ignore[no-untyped-def]
            pass

        def register_app(self, handle, config):  # type: ignore[no-untyped-def]
            captured.append((handle, config))
            self._apps[handle] = config

    fake_slack = _FakeSlack()
    fake_service = MagicMock()
    fake_service.transports = {"slack": fake_slack}
    engine.notification_service = fake_service

    # Apply a config that adds a role with a Slack token.  No prior
    # MCP servers, transports, or roles.
    await engine.apply_config(
        CompanyConfig(
            name="Acme",
            units=[
                {
                    "name": "Core",
                    "type": "team",
                    "roles": [
                        {
                            "name": "Agent SWE",
                            "integrations": {
                                "slack": {
                                    "bot_token": "xoxb-swe",
                                    "signing_secret": "ss",
                                    "channel": "",
                                },
                            },
                        }
                    ],
                }
            ],
        )
    )

    # The new role's slack app landed on the running transport even
    # though ``self.org`` was still empty when the role-add branch
    # ran.
    handles = {h for h, _ in captured}
    assert "agent-swe" in handles, (
        f"agent-swe slack app not registered; captured={captured}"
    )
    swe_config = next(cfg for h, cfg in captured if h == "agent-swe")
    assert swe_config.bot_token == "xoxb-swe"


async def test_embedded_api_uses_the_broadcast_stream_not_a_publish_listener(
    monkeypatch,
) -> None:
    """The core of the wiring merge.

    ``add_publish_listener`` fires only on THIS process's publishes, so an
    embedded dashboard fed by it is structurally unable to see a peer's
    events — it would show 1/N of a fleet's activity and look correct.
    ``subscribe_stream`` is the broadcast path the standalone API already
    used; both now use it, which is what makes them one wiring.
    """
    import uvicorn

    async def _noop_serve(self, sockets=None):  # type: ignore[no-untyped-def]
        return None

    monkeypatch.setattr(uvicorn.Server, "serve", _noop_serve)

    engine = await make_engine(company=CompanyConfig(name="Acme"))

    listeners_before = len(getattr(engine.event_queue, "_publish_listeners", []))
    subscribed: list[str] = []
    original = engine.event_queue.subscribe_stream

    async def _spy(pattern, handler):  # type: ignore[no-untyped-def]
        subscribed.append(pattern)
        return await original(pattern, handler)

    monkeypatch.setattr(engine.event_queue, "subscribe_stream", _spy)

    await engine._start_embedded_api()
    try:
        assert subscribed == ["crewlet.events.>"]
        # The event-store writer keeps its own publish listener; the point
        # is that the STREAM no longer adds one.
        assert (
            len(getattr(engine.event_queue, "_publish_listeners", []))
            == listeners_before
        )
    finally:
        if engine._api_serve_task is not None:
            await engine._api_serve_task


async def test_embedded_api_registers_a_runtime_for_live_engine_facts(
    monkeypatch,
) -> None:
    """In-flight count and drain state cannot come from config or the
    event stream — they are properties of a live engine object. That is
    the one seam, and it replaces five embedded-only parameters."""
    import uvicorn

    from crewlet.api.runtime import EngineNodeRuntime

    async def _noop_serve(self, sockets=None):  # type: ignore[no-untyped-def]
        return None

    monkeypatch.setattr(uvicorn.Server, "serve", _noop_serve)

    engine = await make_engine(company=CompanyConfig(name="Acme"))
    await engine._start_embedded_api()
    try:
        runtime = engine._api_app.state.runtime
        assert isinstance(runtime, EngineNodeRuntime)
        assert runtime.engine is engine
    finally:
        if engine._api_serve_task is not None:
            await engine._api_serve_task


# ---------------------------------------------------------------------------
# Credential rotation — re-activating an UNCHANGED revision
# ---------------------------------------------------------------------------


async def test_reactivating_an_unchanged_revision_rebuilds_on_rotation(
    monkeypatch,
) -> None:
    """The documented rotation gesture must actually rebuild something.

    `docs/concepts/secret-store.md` names "re-activate the unchanged
    revision" as how you make a running engine pick up a rotated
    credential. That payload is byte-identical by definition, so the
    no-op early-out fired and the gesture did nothing but swap the secret
    snapshot: MCP children kept the credential baked into their spawn
    env, LLM providers kept the revoked key, transports kept the old
    token — indefinitely.
    """
    monkeypatch.setenv("ROTATED_KEY", "old-secret")

    company = CompanyConfig(
        name="Acme",
        providers={
            "llm": {
                "default": {
                    "type": "anthropic",
                    "model": "claude-sonnet-5",
                    "api_keys": ["${ROTATED_KEY}"],
                }
            }
        },
    )
    engine = await make_engine(company=company)

    rotations: list[str] = []

    async def _spy() -> list[str]:
        rotations.append("rebuilt")
        return ["providers"]

    monkeypatch.setattr(
        engine, "_apply_credential_rotation", lambda _cfg: _spy(), raising=False
    )

    # Same payload, same resolved value: a genuine no-op.
    assert await engine.apply_config(company) == []
    assert rotations == []

    # Same payload, DIFFERENT resolved value: a rotation.
    monkeypatch.setenv("ROTATED_KEY", "new-secret")
    applied = await engine.apply_config(company)

    assert rotations == ["rebuilt"]
    assert applied == ["providers"]


async def test_rotation_records_the_new_fingerprint(monkeypatch) -> None:
    """A rotation applied once must not re-apply on every later
    activation — the node would restart its MCP children forever."""
    monkeypatch.setenv("ROTATED_KEY", "old-secret")
    company = CompanyConfig(
        name="Acme",
        providers={
            "llm": {
                "default": {
                    "type": "anthropic",
                    "model": "claude-sonnet-5",
                    "api_keys": ["${ROTATED_KEY}"],
                }
            }
        },
    )
    engine = await make_engine(company=company)

    calls: list[int] = []

    async def _spy() -> list[str]:
        calls.append(1)
        return []

    monkeypatch.setattr(
        engine, "_apply_credential_rotation", lambda _cfg: _spy(), raising=False
    )

    monkeypatch.setenv("ROTATED_KEY", "new-secret")
    await engine.apply_config(company)
    await engine.apply_config(company)
    await engine.apply_config(company)

    assert len(calls) == 1
