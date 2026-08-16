"""Tests for the sandbox launch plumbing: run-env
assembly (``build_sandbox_env``), the brief
composer, ``launch_sandbox_run`` / ``collect_sandbox_run`` /
``teardown_sandbox_run``, and the OTel + completion-phase helpers, driven
against the fakes."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from crewlet.agent.definition import AgentDefinition
from crewlet.agent.execute_sandbox import (
    collect_sandbox_run,
    compose_brief,
    launch_sandbox_run,
)
from crewlet.agent.instance import AgentInstance
from crewlet.agent.plan import ExecutionPlan, Step
from crewlet.agent.turn_context import TurnContext
from crewlet.config import LLMProviderConfig
from crewlet.org.models import (
    Organization,
    OrgUnit,
    Role,
    RoleSandboxConfig,
)
from crewlet.sandbox import (
    CodingAgentResult,
    FakeCodingAgentRunner,
    FakeSandboxProvider,
    SandboxManager,
)
from crewlet.sandbox.credentials import (
    SandboxCredentialError,
    build_sandbox_env,
)
from crewlet.sandbox.pending_store import (
    MemoryPendingSandboxRunStore,
    PendingSandboxRun,
)

# ---------------------------------------------------------------------------
# stubs
# ---------------------------------------------------------------------------


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


@dataclass
class _Budget:
    max_tokens: int
    used_tokens: int = 0


@dataclass
class _BudgetManager:
    budget: _Budget
    consumed: list[tuple[str, int]] = field(default_factory=list)

    def get_agent_budget(self, agent_id: str) -> _Budget:
        return self.budget

    async def consume(self, agent_id: str, tokens: int) -> bool:
        self.consumed.append((agent_id, tokens))
        self.budget.used_tokens += tokens
        return True


@dataclass
class _TokenUsageRepo:
    increments: list[tuple[str, int]] = field(default_factory=list)

    async def increment(self, handle: str, tokens: int) -> None:
        self.increments.append((handle, tokens))


def _mk_agent(sandbox: RoleSandboxConfig | None, *, llm_sandbox=None) -> AgentInstance:
    # The seat's GitHub PAT is EXPLICIT config (role.sandbox.env) — the
    # engine does not extract it from mcp_env.github; mcp_env stays the
    # MCP servers' credential surface only.
    if sandbox is not None and "GITHUB_TOKEN" not in sandbox.env:
        sandbox.env["GITHUB_TOKEN"] = "${GH_PAT}"
    role = Role(
        name="Engineer",
        handle="eng",
        llm="default",
        llm_execute="exec-prov",
        llm_sandbox=llm_sandbox,
        sandbox=sandbox,
        mcp_env={"github": {"Authorization": "Bearer ${GH_PAT}"}},
    )
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    org = Organization(name="Acme", units=[unit])
    defn = AgentDefinition(role=org.get_role("Engineer"), org=org)
    return AgentInstance(definition=defn, handle="eng", email="e@acme.com")


def _mk_manager(runner: FakeCodingAgentRunner | None = None) -> SandboxManager:
    runner = runner or FakeCodingAgentRunner(name="claude-code")
    return SandboxManager(
        provider=FakeSandboxProvider(),
        runners={runner.name: runner},
    )


# ---------------------------------------------------------------------------
# build_sandbox_env
# ---------------------------------------------------------------------------


def test_claude_code_uses_anthropic_provider() -> None:
    cfg = LLMProviderConfig(
        type="anthropic", api_keys=["sk-ant-xyz"], base_url="https://api.anthropic.com"
    )
    env = build_sandbox_env(coding_agent="claude-code", llm_config=cfg)
    assert env["ANTHROPIC_API_KEY"] == "sk-ant-xyz"
    assert env["ANTHROPIC_BASE_URL"] == "https://api.anthropic.com"


def test_claude_code_without_anthropic_creds_raises() -> None:
    cfg = LLMProviderConfig(type="openai", api_keys=["sk-openai"])
    with pytest.raises(SandboxCredentialError):
        build_sandbox_env(coding_agent="claude-code", llm_config=cfg)


def test_claude_code_with_unresolvable_key_reference_raises(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """An unset ``${VAR}`` credential must raise, not launch empty.

    ``resolved_keys()`` returns config values verbatim, so the key here is
    the literal string ``"${ANTHROPIC_KEY_1}"`` — which is truthy. The
    guard used to run against that pre-resolution dict, so it passed, the
    reference then flattened to ``""``, and Claude Code launched with an
    empty credential instead of failing here.
    """
    monkeypatch.delenv("ANTHROPIC_KEY_1", raising=False)
    cfg = LLMProviderConfig(type="anthropic", api_keys=["${ANTHROPIC_KEY_1}"])
    with pytest.raises(SandboxCredentialError):
        build_sandbox_env(coding_agent="claude-code", llm_config=cfg)


def test_claude_code_with_resolvable_key_reference_succeeds(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("ANTHROPIC_KEY_1", "sk-ant-from-env")
    cfg = LLMProviderConfig(type="anthropic", api_keys=["${ANTHROPIC_KEY_1}"])
    env = build_sandbox_env(coding_agent="claude-code", llm_config=cfg)
    assert env["ANTHROPIC_API_KEY"] == "sk-ant-from-env"


def test_claude_code_credential_may_come_from_the_secret_store(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A credential stored only in the DB counts as reachable.

    The resolvability check asks the engine's resolver, not ``os.environ``
    directly — otherwise a fully-provisioned seat whose token lives in the
    secret store would be reported as unresolved and refused.
    """
    from crewlet.secrets.resolver import install_secret_source

    class Source:
        def get(self, name: str) -> str | None:
            return "sk-ant-from-store" if name == "ANTHROPIC_KEY_2" else None

    monkeypatch.delenv("ANTHROPIC_KEY_2", raising=False)
    install_secret_source(Source())
    try:
        cfg = LLMProviderConfig(type="anthropic", api_keys=["${ANTHROPIC_KEY_2}"])
        env = build_sandbox_env(coding_agent="claude-code", llm_config=cfg)
        assert env["ANTHROPIC_API_KEY"] == "sk-ant-from-store"
    finally:
        install_secret_source(None)


def test_claude_code_non_anthropic_but_explicit_key_in_role_env() -> None:
    cfg = LLMProviderConfig(type="openai", api_keys=["sk-openai"])
    env = build_sandbox_env(
        coding_agent="claude-code",
        llm_config=cfg,
        role_sandbox_env={"ANTHROPIC_API_KEY": "sk-ant-explicit"},
    )
    assert env["ANTHROPIC_API_KEY"] == "sk-ant-explicit"


def test_opencode_with_openai_compatible_provider() -> None:
    cfg = LLMProviderConfig(type="openai", api_keys=["sk-openai"])
    env = build_sandbox_env(coding_agent="opencode", llm_config=cfg)
    assert env["OPENAI_API_KEY"] == "sk-openai"
    assert "ANTHROPIC_API_KEY" not in env


def test_opencode_with_anthropic_provider_uses_anthropic_key() -> None:
    cfg = LLMProviderConfig(type="anthropic", api_keys=["sk-ant"])
    env = build_sandbox_env(coding_agent="opencode", llm_config=cfg)
    assert env["ANTHROPIC_API_KEY"] == "sk-ant"


def test_opencode_openai_compatible_base_url_reaches_agent() -> None:
    # A custom openai-compatible gateway must reach OpenCode via the
    # standard *_BASE_URL env, not be silently dropped.
    cfg = LLMProviderConfig(
        type="openai", api_keys=["sk-k"], base_url="https://gw.internal/v1"
    )
    env = build_sandbox_env(coding_agent="opencode", llm_config=cfg)
    assert env["OPENAI_API_KEY"] == "sk-k"
    assert env["OPENAI_BASE_URL"] == "https://gw.internal/v1"


def test_opencode_anthropic_base_url() -> None:
    cfg = LLMProviderConfig(
        type="anthropic", api_keys=["sk-ant"], base_url="https://anthropic.gw"
    )
    env = build_sandbox_env(coding_agent="opencode", llm_config=cfg)
    assert env["ANTHROPIC_BASE_URL"] == "https://anthropic.gw"


def test_role_env_token_and_otel_env_merged() -> None:
    # External-service tokens are ORDINARY role.sandbox.env config — the
    # engine names no tool-specific variables of its own.
    cfg = LLMProviderConfig(type="anthropic", api_keys=["sk-ant"])
    env = build_sandbox_env(
        coding_agent="claude-code",
        llm_config=cfg,
        role_sandbox_env={"GITHUB_TOKEN": "ghp_123"},
        otel_env={"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector"},
    )
    assert env["GITHUB_TOKEN"] == "ghp_123"
    assert env["OTEL_EXPORTER_OTLP_ENDPOINT"] == "https://collector"


def test_role_env_resolves_env_references(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("NPM_TOKEN", "npm-secret")
    cfg = LLMProviderConfig(type="anthropic", api_keys=["sk-ant"])
    env = build_sandbox_env(
        coding_agent="claude-code",
        llm_config=cfg,
        role_sandbox_env={"NPM_TOKEN": "${NPM_TOKEN}"},
    )
    assert env["NPM_TOKEN"] == "npm-secret"


def test_agent_identity_env_is_engine_owned_and_generic() -> None:
    # The engine injects the agent's identity as GENERIC facts
    # (CREWLET_AGENT_*) — per-launch values static config can't know.
    # Tool-specific mapping (e.g. git user.name) is the recipes' job; the
    # engine sets no GIT_*/GITHUB_* variables at all.
    cfg = LLMProviderConfig(type="anthropic", api_keys=["sk-ant"])
    env = build_sandbox_env(
        coding_agent="claude-code",
        llm_config=cfg,
        agent_handle="eng",
        agent_email="eng@acme.com",
    )
    assert env["CREWLET_AGENT_HANDLE"] == "eng"
    assert env["CREWLET_AGENT_EMAIL"] == "eng@acme.com"
    assert not any(k.startswith(("GIT_", "GITHUB_")) for k in env)
    # Identity omitted when unknown.
    bare = build_sandbox_env(coding_agent="claude-code", llm_config=cfg)
    assert "CREWLET_AGENT_HANDLE" not in bare


def test_setup_env_merged_with_correct_precedence() -> None:
    # Setup-step env rides ``setup_env``; role.sandbox.env still overrides
    # it (the explicit operator surface wins over step defaults).
    cfg = LLMProviderConfig(type="anthropic", api_keys=["sk-ant"])
    env = build_sandbox_env(
        coding_agent="claude-code",
        llm_config=cfg,
        setup_env={"TOOLCHAIN": "stable", "REGISTRY": "step-default"},
        role_sandbox_env={"REGISTRY": "role-override"},
    )
    assert env["TOOLCHAIN"] == "stable"
    assert env["REGISTRY"] == "role-override"


def test_setup_env_resolves_env_references(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("REGISTRY_TOKEN", "reg-secret")
    cfg = LLMProviderConfig(type="anthropic", api_keys=["sk-ant"])
    env = build_sandbox_env(
        coding_agent="claude-code",
        llm_config=cfg,
        setup_env={"REGISTRY_TOKEN": "${REGISTRY_TOKEN}"},
    )
    assert env["REGISTRY_TOKEN"] == "reg-secret"


def test_unresolved_env_reference_warns_naming_keys_only(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # The loud-failure contract: a declared ${VAR} whose variable isn't
    # exported warns at launch — INCLUDING the embedded "Bearer ${VAR}"
    # shape, which resolves to a truthy-but-broken value that a whole-value
    # emptiness check would miss. The warning names keys only, never values
    # (a partial value can embed a partial secret). Asserted on the module
    # logger seam: structlog's capture_logs is bypassed by cached loggers
    # (cache_logger_on_first_use=True) once any test configured logging.
    from unittest.mock import MagicMock

    from crewlet.sandbox import credentials

    monkeypatch.delenv("GH_PAT", raising=False)
    monkeypatch.setenv("SET_TOKEN", "s3cret-value")
    mock_logger = MagicMock()
    monkeypatch.setattr(credentials, "logger", mock_logger)
    cfg = LLMProviderConfig(type="anthropic", api_keys=["sk-ant"])
    env = build_sandbox_env(
        coding_agent="claude-code",
        llm_config=cfg,
        role_sandbox_env={
            "GITHUB_TOKEN": "${GH_PAT}",  # whole-value reference
            "AUTH_HEADER": "Bearer ${GH_PAT}",  # embedded reference
            "OK_TOKEN": "${SET_TOKEN}",  # resolves fine
            "LITERAL": "no-refs-here",
        },
    )
    # Keys only — no resolved or partial value may reach the log.
    mock_logger.warning.assert_called_once_with(
        "sandbox_env_unresolved", keys=["AUTH_HEADER", "GITHUB_TOKEN"]
    )
    # Resolution itself is unchanged — the warning is the signal, not a veto.
    assert env["GITHUB_TOKEN"] == ""
    assert env["AUTH_HEADER"] == "Bearer "
    assert env["OK_TOKEN"] == "s3cret-value"


def test_resolved_env_references_do_not_warn(monkeypatch: pytest.MonkeyPatch) -> None:
    from unittest.mock import MagicMock

    from crewlet.sandbox import credentials

    monkeypatch.setenv("GH_PAT", "ghp_ok")
    mock_logger = MagicMock()
    monkeypatch.setattr(credentials, "logger", mock_logger)
    cfg = LLMProviderConfig(type="anthropic", api_keys=["sk-ant"])
    env = build_sandbox_env(
        coding_agent="claude-code",
        llm_config=cfg,
        role_sandbox_env={"GITHUB_TOKEN": "x-access-token:${GH_PAT}"},
    )
    mock_logger.warning.assert_not_called()
    assert env["GITHUB_TOKEN"] == "x-access-token:ghp_ok"


# ---------------------------------------------------------------------------
# _coding_agent_llm
# ---------------------------------------------------------------------------


def test_coding_agent_llm_descriptor() -> None:
    from crewlet.agent.execute_sandbox import _coding_agent_llm

    # The role's resolved LLM becomes a descriptor (raw model + type +
    # endpoint); the runner formats the CLI arg and provider config from it.
    cfg = LLMProviderConfig(
        type="openai-compatible",
        model="acme/model-x-large",
        base_url="https://api.x/v1",
    )
    llm = _coding_agent_llm(cfg)
    assert llm.model == "acme/model-x-large"
    assert llm.provider_type == "openai-compatible"
    assert llm.base_url == "https://api.x/v1"
    # No model pinned → empty (the agent falls back to its own default).
    assert _coding_agent_llm(LLMProviderConfig(type="openai", model="")).model == ""


# ---------------------------------------------------------------------------
# compose_brief
# ---------------------------------------------------------------------------


def test_compose_brief_assembles_from_plan() -> None:
    plan = ExecutionPlan(
        steps=[Step(intent="Add endpoint", approach="edit routes.py")],
        success_criteria=["endpoint returns 200", "tests pass"],
    )
    brief = compose_brief(plan, "Build a health endpoint")
    assert "Acceptance criteria:" in brief
    assert "- endpoint returns 200" in brief
    assert "Build a health endpoint" in brief


def test_compose_brief_falls_back_to_task() -> None:
    # An empty plan (no brief, summary, or criteria) still carries the task.
    plan = ExecutionPlan()
    assert compose_brief(plan, "just do this") == "Task:\njust do this"


def test_compose_brief_empty_everything_is_empty() -> None:
    assert compose_brief(ExecutionPlan(), "") == ""


# ---------------------------------------------------------------------------
# run_sandbox_execute_phase
# ---------------------------------------------------------------------------


def _providers() -> tuple[dict, dict]:
    """Return (llm_providers, llm_provider_configs) for an anthropic exec
    provider keyed ``exec-prov`` (the role's ``llm_execute``)."""

    class _P:
        model = "claude"

    return (
        {"default": _P(), "exec-prov": _P()},
        {"exec-prov": LLMProviderConfig(type="anthropic", api_keys=["sk-ant"])},
    )


def test_otel_env_injects_non_secret_values_only(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from crewlet.agent.execute_sandbox import _otel_env

    agent = _mk_agent(RoleSandboxConfig(enabled=True))
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")

    # No collector endpoint → no telemetry env at all.
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)
    assert _otel_env(turn) == {}

    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.internal")
    env = _otel_env(turn)
    assert env["OTEL_EXPORTER_OTLP_ENDPOINT"] == "https://collector.internal"
    assert env["CLAUDE_CODE_ENABLE_TELEMETRY"] == "1"
    assert f"crewlet.turn_id={turn.turn_id}" in env["OTEL_RESOURCE_ATTRIBUTES"]
    # The OTLP ingest token is NEVER injected into the sandbox.
    assert "OTEL_EXPORTER_OTLP_HEADERS" not in env


def test_sandbox_phase_response_includes_transcript_and_redacts() -> None:
    from crewlet.agent.execute_sandbox import _sandbox_phase_response
    from crewlet.sandbox.protocol import CodingAgentResult

    result = CodingAgentResult(
        text="Ran the tests; all pass.",
        success=True,
        transcript="$ gh auth\nToken: ghp_" + "a" * 36 + "\n[✓] done",
    )
    out = _sandbox_phase_response(result)
    assert "Ran the tests" in out
    assert "Coding-agent transcript" in out
    assert "[✓] done" in out
    # A token echoed in the transcript is redacted at the publish boundary.
    assert "ghp_" + "a" * 36 not in out
    assert "[REDACTED:github-token]" in out


async def test_publish_sandbox_completion_phase_emits_execute_with_transcript() -> None:
    from crewlet.agent.execute_sandbox import publish_sandbox_completion_phase
    from crewlet.sandbox.pending_store import PendingSandboxRun
    from crewlet.sandbox.protocol import CodingAgentResult

    agent = _mk_agent(RoleSandboxConfig(enabled=True))
    run = PendingSandboxRun(
        turn_id="t-9",
        agent_handle="eng",
        coding_agent="opencode",
        task_description="run the tests",
    )
    result = CodingAgentResult(
        text="all pass", success=True, transcript="$ pytest\n2 passed"
    )
    queue = _QueueStub()
    await publish_sandbox_completion_phase(
        event_queue=queue, agent=agent, run=run, result=result
    )

    assert len(queue.published) == 1
    topic, event = queue.published[0]
    assert topic == "crewlet.events.agent_phase_completed"
    assert event.phase == "execute"
    assert event.turn_id == "t-9"
    assert "2 passed" in event.response
    assert "detached(completed)" in event.notes


def test_otel_env_claude_toggles_only_for_claude_code(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from crewlet.agent.execute_sandbox import _otel_env

    agent = _mk_agent(RoleSandboxConfig(enabled=True))
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.internal")

    # Claude Code natively exports OTLP → the enable toggles are set.
    cc = _otel_env(turn, coding_agent="claude-code")
    assert cc["CLAUDE_CODE_ENABLE_TELEMETRY"] == "1"

    # OpenCode does NOT emit OTLP → no Claude-specific toggles (they'd be
    # noise); the standard OTEL_* vars are still present.
    oc = _otel_env(turn, coding_agent="opencode")
    assert "CLAUDE_CODE_ENABLE_TELEMETRY" not in oc
    assert "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA" not in oc
    assert oc["OTEL_EXPORTER_OTLP_ENDPOINT"] == "https://collector.internal"


async def test_collect_sandbox_run_reconnects_collects_and_tears_down() -> None:
    # A sandbox the runner "started" earlier; collect reattaches by id.
    runner = FakeCodingAgentRunner(
        name="claude-code",
        result=CodingAgentResult(
            text="done", success=True, delivered_refs=["pr-1"], input_tokens=10
        ),
    )
    provider = FakeSandboxProvider()
    manager = SandboxManager(provider=provider, runners={"claude-code": runner})
    sandbox = await provider.create(manager.build_spec())  # the still-running box
    store = MemoryPendingSandboxRunStore()
    run = PendingSandboxRun(
        turn_id="t-1",
        agent_handle="eng",
        sandbox_id=sandbox.id,
        coding_agent="claude-code",
        command_id="cmd-1",
    )
    await store.create(run)

    result = await collect_sandbox_run(run=run, manager=manager, pending_store=store)

    assert result.delivered_refs == ["pr-1"]
    assert runner.collected == [sandbox]
    # Reconnected to the SAME box and tore it down.
    assert sandbox.closed is True
    # …and the row no longer points at a box that is gone.
    row = await store.get("t-1")
    assert row is not None and row.sandbox_id == "" and row.paused_at is None


async def test_collect_pauses_box_when_teardown_false() -> None:
    # collect keeps the box for reuse — pauses it rather than closing it —
    # and starts its pause TTL on the row so the reaper can expire a
    # snapshot nothing else would ever free.
    runner = FakeCodingAgentRunner(name="claude-code")
    provider = FakeSandboxProvider()
    manager = SandboxManager(provider=provider, runners={"claude-code": runner})
    sandbox = await provider.create(manager.build_spec())
    store = MemoryPendingSandboxRunStore()
    run = PendingSandboxRun(
        turn_id="t-1",
        agent_handle="eng",
        sandbox_id=sandbox.id,
        coding_agent="claude-code",
        pause_ttl_seconds=1800.0,
    )
    await store.create(run)

    await collect_sandbox_run(
        run=run, manager=manager, pending_store=store, teardown=False
    )

    assert sandbox.closed is False
    assert sandbox.paused is True
    row = await store.get("t-1")
    assert row is not None and row.sandbox_id == sandbox.id
    assert row.paused_at is not None


async def test_collect_still_pauses_for_reuse_when_pause_ttl_is_zero() -> None:
    # ``pause_ttl_seconds: 0`` bounds the open-ended wait a BLOCKED run parks
    # in, not the one-dispatch pause between collecting a completion and
    # resuming the Execute loop. Tearing the box down here instead would cost
    # the turn its checkout on the very next run_sandbox call.
    runner = FakeCodingAgentRunner(name="claude-code")
    provider = FakeSandboxProvider()
    manager = SandboxManager(provider=provider, runners={"claude-code": runner})
    sandbox = await provider.create(manager.build_spec())
    store = MemoryPendingSandboxRunStore()
    run = PendingSandboxRun(
        turn_id="t-1",
        agent_handle="eng",
        sandbox_id=sandbox.id,
        coding_agent="claude-code",
        pause_ttl_seconds=0.0,
    )
    await store.create(run)

    await collect_sandbox_run(
        run=run, manager=manager, pending_store=store, teardown=False
    )

    assert sandbox.closed is False
    assert sandbox.paused is True
    row = await store.get("t-1")
    assert row is not None and row.sandbox_id == sandbox.id


async def test_teardown_sandbox_run_kills_box_without_resuming_it() -> None:
    # Teardown goes through kill-by-id, never connect(): connect auto-resumes
    # a paused sandbox, so tearing one down that way would boot the VM back
    # up purely to shut it off.
    from crewlet.agent.execute_sandbox import teardown_sandbox_run

    provider = FakeSandboxProvider()
    manager = SandboxManager(
        provider=provider, runners={"claude-code": FakeCodingAgentRunner()}
    )
    sandbox = await provider.create(manager.build_spec())
    await sandbox.pause()
    store = MemoryPendingSandboxRunStore()
    run = PendingSandboxRun(turn_id="t-1", agent_handle="eng", sandbox_id=sandbox.id)
    await store.create(run)

    await teardown_sandbox_run(run=run, manager=manager, pending_store=store)

    assert sandbox.closed is True
    assert sandbox.paused is True  # never un-paused to be killed
    row = await store.get("t-1")
    assert row is not None and row.sandbox_id == ""


async def test_teardown_sandbox_run_without_a_box_is_a_noop() -> None:
    # A run whose box was already reaped past its pause TTL has no id left.
    from crewlet.agent.execute_sandbox import teardown_sandbox_run

    provider = FakeSandboxProvider()
    manager = SandboxManager(
        provider=provider, runners={"claude-code": FakeCodingAgentRunner()}
    )
    store = MemoryPendingSandboxRunStore()
    run = PendingSandboxRun(turn_id="t-1", agent_handle="eng", sandbox_id="")

    await teardown_sandbox_run(run=run, manager=manager, pending_store=store)

    assert provider.sandboxes == []


# ---------------------------------------------------------------------------
# launch_sandbox_run (the run_sandbox tool's launch core)
# ---------------------------------------------------------------------------


async def test_launch_sandbox_run_persists_row_and_emits_started(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("GH_PAT", "ghp_x")
    agent = _mk_agent(RoleSandboxConfig(enabled=True, coding_agent="claude-code"))
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    plan = ExecutionPlan(success_criteria=["PR"])
    queue = _QueueStub()
    runner = FakeCodingAgentRunner(name="claude-code")
    manager = _mk_manager(runner)
    providers, configs = _providers()
    store = MemoryPendingSandboxRunStore()

    res = await launch_sandbox_run(
        turn=turn,
        plan=plan,
        manager=manager,
        llm_providers=providers,
        llm_provider_configs=configs,
        event_queue=queue,
        pending_store=store,
        brief_override="clone acme/api and run tests",
    )

    assert res.skipped is False
    assert res.sandbox_id.startswith("fake-sandbox")
    brief = runner.started[0][0]
    assert "clone acme/api" in brief
    # The environment block is appended (steps' briefs compose into it —
    # this manager carries none, so just the generic intro)…
    assert "Your environment" in brief
    # …and the run env carries the CONFIG-declared token (role.sandbox.env,
    # ${VAR}-resolved) plus the engine's generic identity facts — no GIT_*
    # vars from the engine.
    env = runner.started[0][1]
    assert env["GITHUB_TOKEN"] == "ghp_x"
    assert env["CREWLET_AGENT_HANDLE"] == "eng"
    assert env["CREWLET_AGENT_EMAIL"] == "e@acme.com"
    row = await store.get(turn.turn_id)
    assert row is not None and row.status == "running"
    assert any(e.type == "sandbox_run_started" for _, e in queue.published)
    # Not torn down — it runs detached.
    assert manager._provider.sandboxes[0].closed is False


async def test_launch_threads_configured_setup_steps(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # Configured setup steps flow through the whole pipeline: commands
    # applied to the box at acquire, env merged into the run env (with
    # ${VAR} resolution), and briefs folded into the environment block.
    # The git-auth wiring is exactly such a configured step (the example's
    # recipe rides manager.default_setup, i.e. providers.sandbox.setup) —
    # the engine ships no steps of its own.
    from crewlet.sandbox.setup import SandboxSetupStep

    from .test_setup import _example_git_step

    monkeypatch.setenv("GH_PAT", "ghp_x")
    # The secret's real value contains a literal ${x}: single resolution must
    # deliver it intact — double resolution would substitute the embedded
    # ${x} from os.environ (to "") and silently corrupt the credential.
    monkeypatch.setenv("NPM_TOKEN", "npm-${x}-secret")
    monkeypatch.delenv("x", raising=False)
    role_step = SandboxSetupStep(
        name="node",
        commands=["corepack enable"],
        env={"NPM_TOKEN": "${NPM_TOKEN}"},
        brief="Node 22 + pnpm are preinstalled; use pnpm for installs.",
    )
    git_step = _example_git_step()
    provider_step = SandboxSetupStep(name="ca", commands=["update-ca-certificates"])
    agent = _mk_agent(
        RoleSandboxConfig(enabled=True, coding_agent="claude-code", setup=[role_step])
    )
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    runner = FakeCodingAgentRunner(name="claude-code")
    manager = SandboxManager(
        provider=FakeSandboxProvider(),
        runners={"claude-code": runner},
        default_setup=[git_step, provider_step],
    )
    providers, configs = _providers()

    res = await launch_sandbox_run(
        turn=turn,
        plan=ExecutionPlan(),
        manager=manager,
        llm_providers=providers,
        llm_provider_configs=configs,
        event_queue=_QueueStub(),
        pending_store=MemoryPendingSandboxRunStore(),
        brief_override="do the thing",
    )

    assert res.skipped is False
    box = manager._provider.sandboxes[0]
    # Order: engine-wide (git-auth recipe, then extras) → role-level.
    assert any('credential."https://gitlab.com".helper' in c for c in box.commands)
    assert "update-ca-certificates" in box.commands
    assert "corepack enable" in box.commands
    assert box.commands.index("update-ca-certificates") < box.commands.index(
        "corepack enable"
    )
    # Provisioning commands run WITH the run env (manager.acquire threads
    # spec.env into apply_setup) — how the git-auth recipe's identity
    # commands read $CREWLET_AGENT_* and its helper sees the declared token
    # at provisioning time. Assert through the REAL acquire path: dropping
    # env=spec.env in manager.acquire must fail here.
    setup_exec_env = box.exec_envs[box.commands.index("corepack enable")]
    assert setup_exec_env["CREWLET_AGENT_HANDLE"] == "eng"
    assert setup_exec_env["CREWLET_AGENT_EMAIL"] == "e@acme.com"
    assert setup_exec_env["GITHUB_TOKEN"] == "ghp_x"
    # Every provisioning command saw the same resolved run env.
    assert all(e == setup_exec_env for e in box.exec_envs)
    # Step env reaches the run env, ${VAR}-resolved exactly ONCE — the
    # literal ${x} inside the secret's value survives.
    assert runner.started[0][1]["NPM_TOKEN"] == "npm-${x}-secret"
    # Step briefs reach the agent's environment block — including the
    # git-auth recipe's "$GITLAB_TOKEN" hint (the original problem: the
    # coding agent never reasoned to use the injected token).
    assert "Node 22 + pnpm" in runner.started[0][0]
    assert "$GITLAB_TOKEN" in runner.started[0][0]


async def test_launch_sandbox_run_budget_floor_skips(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("GH_PAT", "ghp_x")
    agent = _mk_agent(RoleSandboxConfig(enabled=True))
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    providers, configs = _providers()
    store = MemoryPendingSandboxRunStore()
    budget = _BudgetManager(_Budget(max_tokens=1000, used_tokens=999))

    res = await launch_sandbox_run(
        turn=turn,
        plan=ExecutionPlan(),
        manager=_mk_manager(),
        llm_providers=providers,
        llm_provider_configs=configs,
        event_queue=_QueueStub(),
        pending_store=store,
        budget_manager=budget,
        sandbox_min_budget_tokens=2000,
        brief_override="go",
    )

    assert res.skipped is True
    assert "below the floor" in res.skip_reason
    assert await store.get(turn.turn_id) is None  # no row persisted


async def test_launch_sandbox_run_closes_box_on_start_failure(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("GH_PAT", "ghp_x")

    class _Boom(FakeCodingAgentRunner):
        async def start(self, *a, **k):
            raise RuntimeError("boom")

    agent = _mk_agent(RoleSandboxConfig(enabled=True, coding_agent="claude-code"))
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    providers, configs = _providers()
    manager = _mk_manager(_Boom(name="claude-code"))
    store = MemoryPendingSandboxRunStore()

    with pytest.raises(RuntimeError):
        await launch_sandbox_run(
            turn=turn,
            plan=ExecutionPlan(),
            manager=manager,
            llm_providers=providers,
            llm_provider_configs=configs,
            event_queue=_QueueStub(),
            pending_store=store,
            brief_override="go",
        )

    # A box we provisioned is torn down on start failure; no row persisted.
    assert manager._provider.sandboxes[0].closed is True
    assert await store.get(turn.turn_id) is None


async def test_launch_sandbox_run_reuse_reconnects_without_new_box(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("GH_PAT", "ghp_x")
    agent = _mk_agent(RoleSandboxConfig(enabled=True, coding_agent="claude-code"))
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    providers, configs = _providers()
    manager = _mk_manager()
    store = MemoryPendingSandboxRunStore()

    first = await launch_sandbox_run(
        turn=turn,
        plan=ExecutionPlan(),
        manager=manager,
        llm_providers=providers,
        llm_provider_configs=configs,
        event_queue=_QueueStub(),
        pending_store=store,
        brief_override="first",
    )
    await store.set_status(turn.turn_id, "resumed")  # as the coordinator would
    second = await launch_sandbox_run(
        turn=turn,
        plan=ExecutionPlan(),
        manager=manager,
        llm_providers=providers,
        llm_provider_configs=configs,
        event_queue=_QueueStub(),
        pending_store=store,
        brief_override="second",
        reuse_sandbox_id=first.sandbox_id,
    )

    # Same box reused; no second provision; row flipped back to running.
    assert second.sandbox_id == first.sandbox_id
    assert len(manager._provider.created) == 1
    row = await store.get(turn.turn_id)
    assert row is not None and row.status == "running"
    # The row tracks THIS job's command, not the first one's — otherwise the
    # waiter polls a command that already finished.
    assert row.command_id == "cmd-2"


async def test_launch_after_reap_repoints_the_existing_row_at_the_new_box(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # Re-seed path: the reaper took the paused box and released it on
    # the row, so this launch provisions a FRESH one. The row already exists,
    # and it must be re-pointed at the new sandbox + command — an insert that
    # no-ops on conflict would leave the waiter polling a box that is gone,
    # and the completion would never fire.
    monkeypatch.setenv("GH_PAT", "ghp_x")
    agent = _mk_agent(RoleSandboxConfig(enabled=True, coding_agent="claude-code"))
    turn = TurnContext(agent=agent, org=agent.definition.org, task_description="x")
    providers, configs = _providers()
    manager = _mk_manager()
    store = MemoryPendingSandboxRunStore()

    first = await launch_sandbox_run(
        turn=turn,
        plan=ExecutionPlan(),
        manager=manager,
        llm_providers=providers,
        llm_provider_configs=configs,
        event_queue=_QueueStub(),
        pending_store=store,
        brief_override="first",
    )
    # …blocked on a question, then reaped past pause_ttl.
    await store.mark_awaiting_clarification(
        turn.turn_id, question="Q", audience="team", conversation_key="k"
    )
    await store.release_box(turn.turn_id)
    await store.set_status(turn.turn_id, "reseed")

    second = await launch_sandbox_run(
        turn=turn,
        plan=ExecutionPlan(),
        manager=manager,
        llm_providers=providers,
        llm_provider_configs=configs,
        event_queue=_QueueStub(),
        pending_store=store,
        brief_override="second",
        reuse_sandbox_id="",  # nothing to reattach to
    )

    assert second.sandbox_id != first.sandbox_id
    assert len(manager._provider.created) == 2
    row = await store.get(turn.turn_id)
    assert row is not None
    assert row.status == "running"
    assert row.sandbox_id == second.sandbox_id
    assert row.command_id == "cmd-2"
    assert row.paused_at is None
    # The original framing survives — the row was updated, not replaced.
    assert row.conversation_key == "k"


# ---------------------------------------------------------------------------
# build_sandbox_env — subscription (cli-agent) providers
# ---------------------------------------------------------------------------


def _cli_agent_config(agent: str = "claude-code", **cli) -> LLMProviderConfig:
    block = {"agent": agent}
    block.update(cli)
    return LLMProviderConfig(type="cli-agent", model="sonnet", cli=block)


def test_subscription_token_reaches_the_sandbox(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A role on a subscription CLI backend can still do code work: the
    headless token travels to the box, so Claude Code there bills the
    same Pro/Max plan instead of a metered key."""
    monkeypatch.setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-abc")
    env = build_sandbox_env(coding_agent="claude-code", llm_config=_cli_agent_config())
    assert env["CLAUDE_CODE_OAUTH_TOKEN"] == "sk-ant-oat01-abc"
    assert "ANTHROPIC_API_KEY" not in env


def test_subscription_token_honours_an_explicit_reference(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("MY_OAUTH", "sk-ant-oat01-xyz")
    cfg = _cli_agent_config(auth={"mode": "subscription", "token": "${MY_OAUTH}"})
    env = build_sandbox_env(coding_agent="claude-code", llm_config=cfg)
    assert env["CLAUDE_CODE_OAUTH_TOKEN"] == "sk-ant-oat01-xyz"


def test_missing_subscription_token_points_at_capture_token(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The error must name the actual next step, not send the operator
    hunting for an API key the subscription never had."""
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)
    with pytest.raises(SandboxCredentialError) as excinfo:
        build_sandbox_env(coding_agent="claude-code", llm_config=_cli_agent_config())
    message = str(excinfo.value)
    assert "--capture-token" in message
    assert "providers.sandbox.type 'local'" in message


def test_a_cli_without_a_headless_token_points_at_local(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)
    with pytest.raises(SandboxCredentialError) as excinfo:
        build_sandbox_env(
            coding_agent="claude-code", llm_config=_cli_agent_config("codex")
        )
    assert "mints no headless token" in str(excinfo.value)


def test_cli_agent_api_key_mode_exports_the_key() -> None:
    cfg = LLMProviderConfig(
        type="cli-agent",
        model="sonnet",
        api_keys=["sk-ant-metered"],
        cli={"agent": "claude-code", "auth": {"mode": "api-key"}},
    )
    env = build_sandbox_env(coding_agent="claude-code", llm_config=cfg)
    assert env["ANTHROPIC_API_KEY"] == "sk-ant-metered"


def test_cli_credential_files_point_at_the_login(tmp_path) -> None:
    """A local box seeds the very login `crewlet llm login` wrote."""
    from crewlet.sandbox.credentials import cli_credential_files

    cfg = _cli_agent_config(state_dir=str(tmp_path / "state"))
    files = cli_credential_files("default", cfg)
    assert files == {
        ".claude/.credentials.json": str(
            tmp_path / "state" / "credentials" / ".claude" / ".credentials.json"
        )
    }


def test_cli_credential_files_empty_for_api_key_providers() -> None:
    from crewlet.sandbox.credentials import cli_credential_files

    assert cli_credential_files("default", LLMProviderConfig(type="anthropic")) == {}
