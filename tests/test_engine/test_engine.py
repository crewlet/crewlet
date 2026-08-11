"""Tests for the Engine core."""

import asyncio
import sys
from collections.abc import AsyncIterator
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parent.parent))
from conftest import make_engine_from_yaml  # noqa: E402
from crewlet.config import load_company_config  # noqa: E402
from crewlet.engine import Engine  # noqa: E402
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.providers.llm.protocol import Completion, CompletionChunk, Message, ToolDef


class MockLLM:
    """Mock LLM for engine tests."""

    def __init__(self) -> None:
        self.calls: list[list[Message]] = []

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice: str | None = None,
    ) -> Completion:
        self.calls.append(messages)
        return Completion(content="Task completed by agent")

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]:
        yield CompletionChunk(content="streamed")


def make_org() -> Organization:
    return Organization(
        name="Test Corp",
        mission="Test things",
        units=[
            OrgUnit(
                name="Engineering",
                type="department",
                children=[
                    OrgUnit(
                        name="Core",
                        type="team",
                        lead="Tech Lead",
                        roles=[
                            Role(
                                name="Tech Lead",
                                manages=["Engineer A", "Engineer B"],
                            ),
                            Role(name="Engineer A"),
                            Role(name="Engineer B"),
                        ],
                    )
                ],
            )
        ],
    )


@pytest.mark.asyncio
async def test_engine_start_stop():
    org = make_org()
    engine = Engine(organization=org)

    await engine.start()
    assert engine.is_running
    assert len(engine.agent_pool.agents) == 3  # 1 lead + 2 engineers

    await engine.stop()
    assert not engine.is_running


@pytest.mark.asyncio
async def test_engine_start_emits_events():
    org = make_org()
    engine = Engine(organization=org)

    await engine.start()
    assert engine.is_running
    assert len(engine.agent_pool.agents) == 3  # Agents were spawned

    await engine.stop()
    assert not engine.is_running


@pytest.mark.asyncio
async def test_engine_from_config(monkeypatch):
    monkeypatch.setenv("OPENAI_API_KEY", "test-fake-key")
    fixtures = Path(__file__).parent.parent / "fixtures"
    engine = await make_engine_from_yaml(fixtures / "company.yaml")
    assert engine.org.name == "Acme AI Corp"
    assert len(engine.org.all_roles()) == 4


@pytest.mark.asyncio
async def test_engine_builtin_tools_registered():
    org = make_org()
    engine = Engine(organization=org)
    tools = engine.tool_registry.list_tools()
    names = [t.name for t in tools]
    # Builtins + the lone colleague-surface tool (a2a_ask) are
    # registered at engine init.
    assert "a2a_ask" in names
    for removed in (
        "slack_message",
        "jira_comment",
        "jira_assign",
        "confluence_comment",
        "confluence_mention",
        "github_request_review",
    ):
        assert removed not in names
    # Task management tools should NOT be registered (external PM tool)
    assert "create_task" not in names
    assert "delegate" not in names
    # Channel-based A2A tools are not registered; a2a_ask is the only
    # colleague tool.
    assert "send_a2a_message" not in names
    assert "request_a2a_channel" not in names
    assert "close_a2a_channel" not in names


@pytest.mark.asyncio
async def test_engine_observability_hooks():
    """on_task_state_change should fire when task events are published."""
    org = make_org()
    mock_llm = MockLLM()
    engine = Engine(organization=org, llm_providers={"default": mock_llm})

    events_received = []

    async def capture(event):
        events_received.append(event)

    await engine.on_task_state_change(capture)

    await engine.start()

    # Simulate a Jira webhook by publishing a TaskCreated event directly
    from crewlet.events.types import TaskCreated

    await engine.event_queue.publish(
        "crewlet.events.task_created",
        TaskCreated(source="test", task_id="EXT-1", title="Test task"),
    )

    assert len(events_received) > 0
    await engine.stop()


@pytest.mark.asyncio
async def test_on_agent_spawn_hook():
    """on_agent_spawn should fire when agents are spawned."""
    org = make_org()
    engine = Engine(organization=org)

    spawned = []

    async def on_spawn(event):
        spawned.append(event)

    await engine.on_agent_spawn(on_spawn)

    await engine.start()
    assert len(spawned) == 3  # 1 Tech Lead + 2 Engineers

    await engine.stop()


@pytest.mark.asyncio
async def test_executor_gets_concurrency_and_budget():
    """Executor created by engine should have concurrency and budget refs."""
    org = make_org()
    mock_llm = MockLLM()
    engine = Engine(organization=org, llm_providers={"default": mock_llm})

    await engine.start()
    assert engine.turn_engine is not None
    assert engine.turn_engine._concurrency is engine.concurrency
    assert engine.turn_engine._budget_manager is engine.budget_manager
    assert engine.turn_engine._observability is engine.observability

    await engine.stop()


@pytest.mark.asyncio
async def test_reload_config(tmp_path):
    """reload_config should spawn new roles and terminate removed ones."""
    # Write initial config
    initial_yaml = tmp_path / "initial.yaml"
    initial_yaml.write_text(
        """
name: Reload Corp
units:
  - name: Core
    type: team
    lead: Engineer
    roles:
          - name: Engineer
"""
    )

    engine = await make_engine_from_yaml(initial_yaml)
    await engine.start()
    assert len(engine.agent_pool.get_all_for_role("Engineer")) == 1
    assert len(engine.agent_pool.get_all_for_role("Designer")) == 0

    # Write updated config with Designer added, Engineer kept
    updated_yaml = tmp_path / "updated.yaml"
    updated_yaml.write_text(
        """
name: Reload Corp
units:
  - name: Core
    type: team
    lead: Engineer
    roles:
          - name: Engineer
          - name: Designer
"""
    )

    await engine.apply_config(load_company_config(updated_yaml))
    assert len(engine.agent_pool.get_all_for_role("Designer")) >= 1

    await engine.stop()


@pytest.mark.asyncio
async def test_reload_config_updates_role_properties(tmp_path):
    """reload_config should update agents in place when role properties change."""
    from crewlet.events.types import RoleUpdated

    initial_yaml = tmp_path / "initial.yaml"
    initial_yaml.write_text(
        """
name: Reload Corp
units:
  - name: Core
    type: team
    lead: Engineer
    roles:
          - name: Engineer
            goal: Build features
            responsibilities:
              - Write code
            behavioral_guidelines:
              - Be thorough
"""
    )

    engine = await make_engine_from_yaml(initial_yaml)
    await engine.start()

    agents = engine.agent_pool.get_all_for_role("Engineer")
    assert len(agents) == 1
    agent = agents[0]
    agent_id = agent.id_str
    old_prompt = agent.definition.system_prompt

    # Capture events
    events: list[RoleUpdated] = []

    async def capture(event):
        if isinstance(event, RoleUpdated):
            events.append(event)

    await engine.event_queue.subscribe("crewlet.events.role_updated", "test", capture)

    # Update goal, tools, responsibilities, and behavioral_guidelines
    updated_yaml = tmp_path / "updated.yaml"
    updated_yaml.write_text(
        """
name: Reload Corp
units:
  - name: Core
    type: team
    lead: Engineer
    roles:
          - name: Engineer
            goal: Build amazing features
            responsibilities:
              - Write code
              - Review PRs
            behavioral_guidelines:
              - Be thorough
              - Be collaborative
"""
    )

    await engine.apply_config(load_company_config(updated_yaml))

    # Agent should still exist (not recreated)
    agents_after = engine.agent_pool.get_all_for_role("Engineer")
    assert len(agents_after) == 1
    assert agents_after[0].id_str == agent_id  # Same agent, not recreated

    # Definition should be rebuilt with new properties
    new_def = agents_after[0].definition
    assert new_def.role.goal == "Build amazing features"
    assert "Review PRs" in new_def.role.responsibilities
    assert "Be collaborative" in new_def.role.behavioral_guidelines

    # System prompt should have changed
    assert new_def.system_prompt != old_prompt
    assert "Build amazing features" in new_def.system_prompt

    # RoleUpdated event should have been emitted
    assert len(events) == 1
    assert events[0].role == "Engineer"
    assert events[0].agent_id == agent_id
    assert set(events[0].changed_fields) == {
        "goal",
        "responsibilities",
        "behavioral_guidelines",
    }

    await engine.stop()


@pytest.mark.asyncio
async def test_reload_config_updates_manages(tmp_path):
    """reload_config should update manages when a new role is added."""
    initial_yaml = tmp_path / "initial.yaml"
    initial_yaml.write_text(
        """
name: Reload Corp
units:
  - name: Core
    type: team
    lead: Manager
    roles:
          - name: Manager
          - name: Engineer
"""
    )

    engine = await make_engine_from_yaml(initial_yaml)
    await engine.start()

    manager_agents = engine.agent_pool.get_all_for_role("Manager")
    assert len(manager_agents) == 1
    old_manages = manager_agents[0].definition.role.manages
    # Lead auto-manages all other roles in the team
    assert old_manages == ["Engineer"]

    # Add Designer to the team — Manager should auto-manage them too
    updated_yaml = tmp_path / "updated.yaml"
    updated_yaml.write_text(
        """
name: Reload Corp
units:
  - name: Core
    type: team
    lead: Manager
    roles:
          - name: Manager
          - name: Engineer
          - name: Designer
"""
    )

    await engine.apply_config(load_company_config(updated_yaml))

    manager_agents = engine.agent_pool.get_all_for_role("Manager")
    assert len(manager_agents) == 1
    new_manages = manager_agents[0].definition.role.manages
    assert sorted(new_manages) == ["Designer", "Engineer"]

    # System prompt should reflect new reports
    prompt = manager_agents[0].definition.system_prompt
    assert "Designer" in prompt

    await engine.stop()


@pytest.mark.asyncio
async def test_reload_config_no_event_when_unchanged(tmp_path):
    """reload_config should NOT emit RoleUpdated for roles that didn't change."""
    from crewlet.events.types import RoleUpdated

    yaml_content = """
name: Reload Corp
units:
  - name: Core
    type: team
    lead: Engineer
    roles:
          - name: Engineer
            goal: Build features
"""
    initial_yaml = tmp_path / "initial.yaml"
    initial_yaml.write_text(yaml_content)
    updated_yaml = tmp_path / "updated.yaml"
    updated_yaml.write_text(yaml_content)

    engine = await make_engine_from_yaml(initial_yaml)
    await engine.start()

    events: list[RoleUpdated] = []

    async def capture(event):
        if isinstance(event, RoleUpdated):
            events.append(event)

    await engine.event_queue.subscribe("crewlet.events.role_updated", "test", capture)

    await engine.apply_config(load_company_config(updated_yaml))

    assert len(events) == 0

    await engine.stop()


@pytest.mark.asyncio
async def test_reload_config_propagates_policy_change_to_org(tmp_path):
    """Policy changes via ``reload_config`` reach the in-memory
    Organization (which is what the Plan-prompt builder reads from).

    Policies render directly from ``engine.org`` into the Plan
    prompt's policies section, so the test asserts on ``engine.org``.
    """
    initial_yaml = tmp_path / "initial.yaml"
    initial_yaml.write_text(
        """
name: Reload Corp
policies:
  - Original policy only
units:
  - name: Core
    type: team
    lead: Engineer
    roles:
      - name: Engineer
"""
    )
    updated_yaml = tmp_path / "updated.yaml"
    updated_yaml.write_text(
        """
name: Reload Corp
policies:
  - Original policy only
  - Newly added policy from reload
units:
  - name: Core
    type: team
    lead: Engineer
    roles:
      - name: Engineer
"""
    )
    engine = await make_engine_from_yaml(initial_yaml)
    await engine.start()

    assert "Newly added policy from reload" not in engine.org.policies

    await engine.apply_config(load_company_config(updated_yaml))

    assert "Newly added policy from reload" in engine.org.policies

    await engine.stop()


@pytest.mark.asyncio
async def test_engine_run_blocks_until_signal():
    """engine.run() should block until a shutdown signal and then stop."""
    import signal

    org = make_org()
    engine = Engine(organization=org)

    async def send_signal_after_delay():
        await asyncio.sleep(0.1)
        signal.raise_signal(signal.SIGINT)

    # Schedule the signal sender concurrently
    asyncio.create_task(send_signal_after_delay())
    await engine.run()

    # Engine should be stopped after run() returns
    assert not engine.is_running


@pytest.mark.asyncio
async def test_engine_run_force_exit_on_second_signal():
    """A second SIGINT during shutdown should force-exit without hanging."""
    import signal

    org = make_org()
    engine = Engine(organization=org)

    # Patch stop() to simulate a slow shutdown that would hang
    original_stop = engine.stop

    async def _slow_stop() -> None:
        await asyncio.sleep(60)  # Would hang without force-exit
        await original_stop()

    engine.stop = _slow_stop  # type: ignore[assignment]

    async def send_signals():
        await asyncio.sleep(0.1)
        # First signal: triggers graceful shutdown
        signal.raise_signal(signal.SIGINT)
        await asyncio.sleep(0.1)
        # Second signal: forces immediate exit
        signal.raise_signal(signal.SIGINT)

    asyncio.create_task(send_signals())

    # run() should return quickly after the second signal instead of
    # hanging for 60s in _slow_stop
    await asyncio.wait_for(engine.run(), timeout=5.0)

    # Force-stop cleanup should have marked the engine as not running
    assert not engine.is_running


@pytest.mark.asyncio
async def test_engine_run_ctrl_c_during_startup():
    """SIGINT during start() should abort startup and exit promptly."""
    import signal

    org = make_org()
    engine = Engine(organization=org)

    # Patch start() to simulate a slow startup (e.g. Pulsar/DB hanging)
    original_start = engine.start

    async def _slow_start() -> None:
        await asyncio.sleep(60)  # Hangs unless interrupted by the signal
        await original_start()

    engine.start = _slow_start  # type: ignore[assignment]

    async def send_signal():
        await asyncio.sleep(0.1)
        signal.raise_signal(signal.SIGINT)

    asyncio.create_task(send_signal())

    # run() should return quickly instead of hanging for 60s in start()
    await asyncio.wait_for(engine.run(), timeout=5.0)

    # Engine never fully started, so is_running should be False
    assert not engine.is_running


# --- Graceful shutdown drain ---


@pytest.mark.asyncio
async def test_stop_pauses_event_delivery_before_draining() -> None:
    """stop() must pause the event queue before waiting for handlers.

    Without this ordering, new task_assigned events keep starting new
    turns while we're trying to drain the in-flight ones -- the drain
    never converges and shutdown waits the full timeout for nothing.
    """
    org = make_org()
    engine = Engine(organization=org)
    await engine.start()

    pause_called = False
    original_pause = engine.event_queue.pause_delivery

    async def tracking_pause() -> None:
        nonlocal pause_called
        pause_called = True
        await original_pause()

    engine.event_queue.pause_delivery = tracking_pause  # type: ignore[method-assign]

    await engine.stop()
    assert pause_called, "stop() must call event_queue.pause_delivery()"
    assert not engine.is_running


@pytest.mark.asyncio
async def test_stop_drains_inflight_handler_then_tears_down() -> None:
    """An in-flight handler started before stop() runs to completion.

    Mirrors the graceful-drain contract: pause stops new dispatch but
    the running handler finishes naturally and its publish lands.
    """
    org = make_org()
    engine = Engine(organization=org)
    await engine.start()

    handler_finished = asyncio.Event()
    publish_landed = False

    async def slow_handler(event):  # type: ignore[no-untyped-def]
        # Simulate an in-flight LLM turn that takes a moment to finish.
        await asyncio.sleep(0.2)
        handler_finished.set()

    await engine.event_queue.subscribe("crewlet.test.slow", "slow-group", slow_handler)

    async def publish_landing_handler(event):  # type: ignore[no-untyped-def]
        nonlocal publish_landed
        publish_landed = True

    await engine.event_queue.subscribe(
        "crewlet.test.landing", "landing-group", publish_landing_handler
    )

    # Fire the slow handler and start shutdown while it's still running.
    from crewlet.events.types import Event

    publish_task = asyncio.create_task(
        engine.event_queue.publish("crewlet.test.slow", Event(type="slow", source="t"))
    )
    # Let dispatch begin so the handler is mid-await when stop() runs.
    await asyncio.sleep(0.05)

    await engine.stop()
    await publish_task
    assert handler_finished.is_set(), "in-flight handler must run to completion"
    assert not engine.is_running


@pytest.mark.asyncio
async def test_stop_blocks_new_event_delivery_during_shutdown() -> None:
    """Events published after pause are NOT delivered to handlers.

    Inline-dispatch memory queue: paused publishes land in history but
    skip dispatch -- the cross-restart-safe semantics that Pulsar gives
    naturally (unacked messages stay queued for the next process).
    """
    org = make_org()
    engine = Engine(organization=org)
    await engine.start()

    received: list[str] = []

    async def post_pause_handler(event):  # type: ignore[no-untyped-def]
        received.append(event.type)

    await engine.event_queue.subscribe(
        "crewlet.test.late", "late-group", post_pause_handler
    )

    await engine.event_queue.pause_delivery()

    from crewlet.events.types import Event

    await engine.event_queue.publish(
        "crewlet.test.late", Event(type="too_late", source="t")
    )
    await asyncio.sleep(0.05)
    assert received == [], "events published after pause must not be delivered"

    await engine.stop()


@pytest.mark.asyncio
async def test_force_stop_resets_working_agent_state() -> None:
    """``_force_stop`` (second-signal path) resets any WORKING agent
    so a teardown listener that still inspects ``agent.state`` sees a
    truthful picture, not a stale "working" snapshot of a coroutine
    that was just cancelled.
    """
    from crewlet.agent.instance import AgentState

    org = make_org()
    engine = Engine(organization=org)
    await engine.start()

    agent = engine.agent_pool.agents[0]
    agent.state = AgentState.WORKING
    agent.current_task_id = "task-stuck-1"

    await engine._force_stop()

    assert agent.state == AgentState.IDLE
    assert agent.current_task_id is None
    assert not engine.is_running


@pytest.mark.asyncio
async def test_stop_blocks_until_handlers_drain_with_no_timeout() -> None:
    """``stop()`` waits indefinitely for the in-flight counter to hit 0.

    No internal timeout: the operator's second signal (or the host
    orchestrator's SIGKILL) is the only cutoff.  Test verifies the
    drain DOES converge once handlers finish — there is no fixed
    grace-window cutoff.
    """
    org = make_org()
    engine = Engine(organization=org)
    await engine.start()

    # Simulate an in-flight handler.  Mid-stop we'll flip the counter
    # to 0 and signal idle; stop() must wake up and complete teardown.
    engine.event_queue._in_flight = 1  # type: ignore[attr-defined]
    engine.event_queue._idle_event.clear()  # type: ignore[attr-defined]

    stop_task = asyncio.create_task(engine.stop())

    # Give stop() time to reach the drain wait.
    await asyncio.sleep(0.05)
    assert not stop_task.done(), "stop() must wait while handlers are in flight"

    # Handler "completes":
    engine.event_queue._in_flight = 0  # type: ignore[attr-defined]
    engine.event_queue._idle_event.set()  # type: ignore[attr-defined]

    # No internal timeout means stop() should wake up promptly now.
    await asyncio.wait_for(stop_task, timeout=2.0)
    assert not engine.is_running


@pytest.mark.asyncio
async def test_engine_in_flight_count_reflects_queue_state() -> None:
    """``Engine.in_flight_count`` delegates to the queue's counter.

    Surfaced via ``/health`` so the dashboard's drain pill has a
    real-time signal during graceful shutdown.
    """
    engine = Engine(organization=make_org())
    await engine.start()

    # Idle.
    assert engine.in_flight_count == 0

    # Bump the counter directly (no need for a real handler -- we're
    # testing the property delegation).
    engine.event_queue._in_flight = 2  # type: ignore[attr-defined]
    assert engine.in_flight_count == 2

    engine.event_queue._in_flight = 0  # type: ignore[attr-defined]
    await engine.stop()


# --- Signal escalation tiers ---


@pytest.mark.asyncio
async def test_engine_run_first_signal_is_graceful_not_forced():
    """ONE Ctrl+C must take the graceful path, never the force path.

    Regression: registering SIGINT with both ``loop.add_signal_handler``
    and ``signal.signal`` made a single press fire both handlers (the
    asyncio wakeup-fd machinery stays live when the Python-level
    handler is swapped), so the second invocation saw ``stop_event``
    already set, counted it as a second press, and cancelled every
    task -- the graceful drain was unreachable in practice.
    """
    import signal

    org = make_org()
    engine = Engine(organization=org)

    force_calls: list[str] = []
    original_force = engine._force_stop

    async def tracking_force_stop() -> None:
        force_calls.append("forced")
        await original_force()

    engine._force_stop = tracking_force_stop  # type: ignore[method-assign]

    async def send_one_signal():
        await asyncio.sleep(0.1)
        signal.raise_signal(signal.SIGINT)

    asyncio.create_task(send_one_signal())
    await asyncio.wait_for(engine.run(), timeout=5.0)

    assert force_calls == [], "a single signal must not take the force path"
    assert not engine.is_running
    assert engine.shutting_down


@pytest.mark.asyncio
async def test_engine_run_third_signal_hard_exits(monkeypatch):
    """The third signal is the deterministic escape hatch: ``os._exit``.

    Without it, extra presses just re-cancelled all tasks, ripping
    ``CancelledError`` through ``_force_stop``'s cleanup at arbitrary
    points -- the process eventually died by accident, not by design.
    """
    import os
    import signal

    org = make_org()
    engine = Engine(organization=org)

    async def _hang_stop() -> None:
        await asyncio.sleep(60)

    engine.stop = _hang_stop  # type: ignore[assignment]
    engine._force_stop = _hang_stop  # type: ignore[method-assign]

    exit_calls: list[int] = []
    monkeypatch.setattr(os, "_exit", lambda code: exit_calls.append(code))

    run_task = asyncio.create_task(engine.run())

    # Deliver the signals from plain loop callbacks, NOT tasks: the
    # second signal cancels every task on the loop, which would kill a
    # task-based sender before it could send the third signal.
    loop = asyncio.get_running_loop()
    loop.call_later(0.05, signal.raise_signal, signal.SIGINT)
    loop.call_later(0.15, signal.raise_signal, signal.SIGINT)
    loop.call_later(0.30, signal.raise_signal, signal.SIGINT)

    deadline = loop.time() + 5.0
    while not exit_calls and loop.time() < deadline:
        try:
            await asyncio.sleep(0.05)
        except asyncio.CancelledError:
            # Signal #2's cancel-all also cancels this test task;
            # absorb the cancellation and keep polling.
            task = asyncio.current_task()
            if task is not None:
                task.uncancel()
    assert exit_calls == [1], "third signal must hard-exit with code 1"

    # The (patched) hard exit returned, so unwedge the hung run() task.
    run_task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await run_task


class _BrokenPipeStream:
    """Stands in for stderr after the pipeline reader died.

    Ctrl+C is delivered to the whole foreground process group, so in
    ``crewlet run 2>&1 | tee out`` the first press also kills ``tee``
    — every subsequent write to stdout/stderr raises BrokenPipeError.
    """

    def write(self, _data: str) -> int:
        raise BrokenPipeError(32, "Broken pipe")

    def flush(self) -> None:
        raise BrokenPipeError(32, "Broken pipe")


def test_handle_shutdown_signal_schedules_action_before_notice(monkeypatch):
    """The shutdown action is scheduled BEFORE the console notice.

    The notice write is the step that can fail (broken pipe, closed
    stream); anything sequenced after it inherits that fragility, so
    the action must come first.
    """
    import signal

    from crewlet.engine import _handle_shutdown_signal

    order: list[str] = []

    class _RecordingStream:
        def write(self, data: str) -> int:
            order.append("notice")
            return len(data)

        def flush(self) -> None:
            pass

    monkeypatch.setattr(sys, "stderr", _RecordingStream())
    _handle_shutdown_signal(
        1,
        signal.SIGINT,
        schedule_graceful=lambda: order.append("graceful"),
        schedule_force_cancel=lambda: order.append("force"),
        hard_exit=lambda code: order.append(f"exit:{code}"),
    )
    assert order[0] == "graceful"
    assert "notice" in order
    assert "force" not in order


def test_handle_shutdown_signal_never_raises_on_broken_stderr(monkeypatch):
    """Every escalation tier must act despite a dead stderr.

    An exception escaping a signal handler is re-raised inside
    whatever frame the main thread happened to be executing, killing
    an arbitrary task or the event loop itself — so the ladder must
    swallow console-write failures on all three tiers.
    """
    import signal

    from crewlet.engine import _handle_shutdown_signal

    monkeypatch.setattr(sys, "stderr", _BrokenPipeStream())

    calls: list[str] = []
    for count in (1, 2, 3):
        _handle_shutdown_signal(
            count,
            signal.SIGINT,
            schedule_graceful=lambda: calls.append("graceful"),
            schedule_force_cancel=lambda: calls.append("force"),
            hard_exit=lambda code: calls.append(f"exit:{code}"),
        )
    assert calls == ["graceful", "force", "exit:1"]


@pytest.mark.asyncio
async def test_engine_run_shuts_down_when_stderr_is_broken_pipe(monkeypatch):
    """Regression for the ``crewlet run 2>&1 | tee out`` hang.

    The first Ctrl+C kills ``tee`` (same process group), turning
    stderr into a broken pipe.  A handler that prints its notice
    BEFORE scheduling ``stop_event.set`` raises BrokenPipeError
    inside the signal handler, so the shutdown never
    starts — the engine runs on invisibly ("stuck forever") and the
    escaping exception corrupts whatever the main thread is
    executing, which on the second press tears the loop down around
    the live Pulsar client and core-dumps at interpreter exit.  The
    handler must therefore schedule the stop before printing.
    """
    import signal

    org = make_org()
    engine = Engine(organization=org)

    # With the broken behaviour, run() only ever ends via the force
    # path: the shutdown never starts, the test timeout cancels run(),
    # and the CancelledError branch runs _force_stop().  The graceful
    # path must be the one taken — same spy pattern as
    # test_engine_run_first_signal_is_graceful_not_forced.
    force_calls: list[str] = []
    original_force = engine._force_stop

    async def tracking_force_stop() -> None:
        force_calls.append("forced")
        await original_force()

    engine._force_stop = tracking_force_stop  # type: ignore[method-assign]

    monkeypatch.setattr(sys, "stderr", _BrokenPipeStream())

    async def send_signal():
        await asyncio.sleep(0.1)
        signal.raise_signal(signal.SIGINT)

    asyncio.create_task(send_signal())
    loop = asyncio.get_running_loop()
    started = loop.time()
    await asyncio.wait_for(engine.run(), timeout=5.0)
    elapsed = loop.time() - started

    assert force_calls == [], "broken stderr must not divert to the force path"
    assert elapsed < 3.0, f"graceful shutdown should be prompt, took {elapsed:.1f}s"
    assert not engine.is_running
    assert engine.shutting_down


# --- Embedded API lifecycle during shutdown ---


class _FakeUvicornServer:
    """Stands in for uvicorn.Server: a serve loop driven by the same
    ``should_exit`` / ``force_exit`` flags ``_stop_embedded_api`` uses."""

    def __init__(self, *, honor: str = "should_exit") -> None:
        self.should_exit = False
        self.force_exit = False
        self._honor = honor

    async def serve(self) -> None:
        while True:
            if self._honor == "should_exit" and self.should_exit:
                return
            if self._honor == "force_exit" and self.force_exit:
                return
            # honor == "nothing": only task cancellation ends us.
            await asyncio.sleep(0.01)


@pytest.mark.asyncio
async def test_engine_run_keeps_api_alive_until_engine_stopped():
    """The dashboard serves through the whole graceful stop (so the
    drain is observable) and only comes down afterwards."""
    import signal

    org = make_org()
    engine = Engine(organization=org)

    order: list[str] = []

    fake_server = _FakeUvicornServer()
    original_stop = engine.stop

    async def tracking_stop() -> None:
        assert not fake_server.should_exit, "stop() must run with the API still serving"
        order.append("engine_stop")
        await original_stop()

    original_api_stop = engine._stop_embedded_api

    async def tracking_api_stop() -> None:
        order.append("api_stop")
        await original_api_stop()

    engine.stop = tracking_stop  # type: ignore[assignment]
    engine._stop_embedded_api = tracking_api_stop  # type: ignore[method-assign]

    async def send_signal():
        await asyncio.sleep(0.1)
        signal.raise_signal(signal.SIGINT)

    asyncio.create_task(send_signal())

    # Wire the fake API in once start() has run (run() would only start
    # a real uvicorn when _api_port > 0).
    async def attach_fake_api():
        await asyncio.sleep(0.05)
        engine._api_server = fake_server
        engine._api_serve_task = asyncio.create_task(fake_server.serve())

    asyncio.create_task(attach_fake_api())
    await asyncio.wait_for(engine.run(), timeout=5.0)

    assert order == ["engine_stop", "api_stop"]
    assert fake_server.should_exit, "API must be asked to exit after stop()"
    assert engine._api_server is None
    assert engine._api_serve_task is None


@pytest.mark.asyncio
async def test_stop_embedded_api_escalates_to_force_exit():
    """A server that ignores ``should_exit`` (e.g. a WebSocket client
    keeping a connection open) gets ``force_exit`` after the graceful
    window."""
    engine = Engine(organization=make_org())
    engine._api_stop_graceful_timeout = 0.05
    engine._api_stop_force_timeout = 1.0

    server = _FakeUvicornServer(honor="force_exit")
    engine._api_server = server
    engine._api_serve_task = asyncio.create_task(server.serve())

    await asyncio.wait_for(engine._stop_embedded_api(), timeout=3.0)
    assert server.should_exit
    assert server.force_exit


@pytest.mark.asyncio
async def test_stop_embedded_api_cancels_a_wedged_server():
    """A server that ignores both flags is cancelled outright --
    shutdown can never park on the API teardown."""
    engine = Engine(organization=make_org())
    engine._api_stop_graceful_timeout = 0.05
    engine._api_stop_force_timeout = 0.05

    server = _FakeUvicornServer(honor="nothing")
    serve_task = asyncio.create_task(server.serve())
    engine._api_server = server
    engine._api_serve_task = serve_task

    await asyncio.wait_for(engine._stop_embedded_api(), timeout=3.0)
    assert serve_task.done()


# --- Drain visibility & turn-engine gate ---


@pytest.mark.asyncio
async def test_shutting_down_flips_at_drain_start() -> None:
    """``Engine.shutting_down`` (what /health serves) must be True
    DURING the drain, while ``is_running`` is still True -- otherwise
    the dashboard can't show the drain at all."""
    org = make_org()
    engine = Engine(organization=org)
    await engine.start()

    assert not engine.shutting_down

    observed: dict[str, bool] = {}
    original_wait = engine.event_queue.wait_for_handlers

    async def observing_wait(timeout: float | None = None) -> int:
        observed["shutting_down_during_drain"] = engine.shutting_down
        observed["is_running_during_drain"] = engine.is_running
        return await original_wait(timeout)

    engine.event_queue.wait_for_handlers = observing_wait  # type: ignore[method-assign]

    await engine.stop()

    assert observed["shutting_down_during_drain"] is True
    assert observed["is_running_during_drain"] is True
    assert engine.shutting_down
    assert not engine.is_running


@pytest.mark.asyncio
async def test_stop_drain_polls_until_handlers_converge() -> None:
    """The drain loop re-polls ``wait_for_handlers`` on its progress
    interval (logging ``drain_in_progress``) until the count hits 0."""
    org = make_org()
    engine = Engine(organization=org)
    await engine.start()
    engine._drain_log_interval = 0.05

    engine.event_queue._in_flight = 1  # type: ignore[attr-defined]
    engine.event_queue._idle_event.clear()  # type: ignore[attr-defined]

    stop_task = asyncio.create_task(engine.stop())

    # Several progress intervals elapse with the handler still busy.
    await asyncio.sleep(0.3)
    assert not stop_task.done(), "drain must keep waiting while in-flight > 0"

    engine.event_queue._in_flight = 0  # type: ignore[attr-defined]
    engine.event_queue._idle_event.set()  # type: ignore[attr-defined]

    await asyncio.wait_for(stop_task, timeout=2.0)
    assert not engine.is_running


@pytest.mark.asyncio
async def test_stop_flips_turn_engine_shutdown_gate() -> None:
    """``stop()`` must call ``TurnEngine.begin_shutdown`` before the
    drain so turns parked at the concurrency gate are NAK'd instead of
    running full LLM rounds during shutdown."""
    org = make_org()
    engine = Engine(organization=org)
    await engine.start()

    calls: list[str] = []

    class _StubTurnEngine:
        def begin_shutdown(self) -> None:
            calls.append("begin_shutdown")

    engine.turn_engine = _StubTurnEngine()  # type: ignore[assignment]

    original_wait = engine.event_queue.wait_for_handlers

    async def asserting_wait(timeout: float | None = None) -> int:
        assert calls == ["begin_shutdown"], (
            "begin_shutdown must fire before the drain wait"
        )
        return await original_wait(timeout)

    engine.event_queue.wait_for_handlers = asserting_wait  # type: ignore[method-assign]

    await engine.stop()
    assert calls == ["begin_shutdown"]


@pytest.mark.asyncio
async def test_agent_handler_reraises_shutdown_draining() -> None:
    """The per-agent inbox handler must let ``ShutdownDraining``
    propagate so the queue NAKs the message for the next boot, rather
    than swallowing it (which would ack and lose the task)."""
    from crewlet.agent.turn import ShutdownDraining
    from crewlet.events.types import Event

    org = make_org()
    engine = Engine(organization=org)
    await engine.start()

    class _DrainingTurnEngine:
        def begin_shutdown(self) -> None:
            pass

        async def run_turn(self, *args, **kwargs):
            raise ShutdownDraining("engine is draining")

    engine.turn_engine = _DrainingTurnEngine()  # type: ignore[assignment]
    agent = engine.agent_pool.agents[0]
    handler = engine._make_agent_handler(agent)

    with pytest.raises(ShutdownDraining):
        await handler([Event(type="task_assigned", source="test")])

    await engine.stop()


# --- _ensure_atlassian_toolsets ---


class TestEnsureAtlassianToolsets:
    """Tests for the _ensure_atlassian_toolsets helper."""

    def test_adds_jira_users_when_toolsets_unset(self) -> None:
        from crewlet.engine import _ensure_atlassian_toolsets

        env: dict[str, str] = {}
        _ensure_atlassian_toolsets("atlassian", env)
        tokens = {t.strip() for t in env["TOOLSETS"].split(",")}
        assert "jira_users" in tokens
        assert "all" in tokens

    def test_adds_jira_users_to_existing_toolsets(self) -> None:
        from crewlet.engine import _ensure_atlassian_toolsets

        env = {"TOOLSETS": "jira_issues,jira_comments"}
        _ensure_atlassian_toolsets("atlassian", env)
        tokens = {t.strip() for t in env["TOOLSETS"].split(",")}
        assert "jira_users" in tokens
        assert "jira_issues" in tokens
        assert "jira_comments" in tokens

    def test_noop_when_jira_users_present(self) -> None:
        from crewlet.engine import _ensure_atlassian_toolsets

        env = {"TOOLSETS": "jira_issues,jira_users"}
        _ensure_atlassian_toolsets("atlassian", env)
        assert env["TOOLSETS"] == "jira_issues,jira_users"

    def test_noop_for_non_atlassian(self) -> None:
        from crewlet.engine import _ensure_atlassian_toolsets

        env: dict[str, str] = {}
        _ensure_atlassian_toolsets("slack", env)
        assert "TOOLSETS" not in env

    def test_works_with_instance_name(self) -> None:
        from crewlet.engine import _ensure_atlassian_toolsets

        env: dict[str, str] = {}
        _ensure_atlassian_toolsets("atlassian::Agent_PM", env)
        tokens = {t.strip() for t in env["TOOLSETS"].split(",")}
        assert "jira_users" in tokens


class TestValidateSkillTriggers:
    """Dangling-trigger detection: exact-name trigger matching means an
    upstream MCP tool rename silently disables a skill's catalogue entry
    and (for required skills) its guard — the engine must say so."""

    @staticmethod
    def _engine_with_surface() -> Engine:
        from types import SimpleNamespace

        from crewlet.config import MCPServerConfig
        from crewlet.tools.registry import SimpleTool

        engine = Engine(organization=make_org())
        # One globally registered tool + one per-role MCP tool: the
        # known-tools union must include BOTH registries.
        engine.tool_registry.register(
            SimpleTool(
                name="jira_add_comment",
                description="d",
                parameters={},
                fn=lambda p, c: None,
            )
        )
        engine._role_mcp_tools["Engineer A"] = [
            SimpleNamespace(name="confluence_create_page")
        ]
        engine._mcp_configs = [MCPServerConfig(name="slack", command="echo")]
        return engine

    @staticmethod
    def _skill(key: str, trigger, required: bool = True):
        from crewlet.agent.skills.models import PromptSkill

        return PromptSkill(
            key=key,
            trigger=trigger,
            title=key,
            summary="s",
            body="b",
            required=required,
        )

    def test_drift_warns_inert_informs_clean_is_silent(self) -> None:
        from crewlet.agent.skills.models import TriggerExpr

        engine = self._engine_with_surface()
        engine._prompt_skill_registry.seed(
            [
                # Partially live: confluence tool exists (per-role), the
                # slack tool name matches nothing anywhere → drift.
                self._skill(
                    "skill:drift",
                    TriggerExpr(
                        any_of=[
                            TriggerExpr(tool="confluence_create_page"),
                            TriggerExpr(tool="slack_conversations_add_message"),
                        ]
                    ),
                ),
                # Entirely dead + server not configured → inert (info).
                self._skill(
                    "skill:inert",
                    TriggerExpr(
                        any_of=[
                            TriggerExpr(tool="gh_create_pr"),
                            TriggerExpr(mcp_server="github"),
                        ]
                    ),
                ),
                # All leaves match → no finding at all.
                self._skill("skill:clean", TriggerExpr(tool="jira_add_comment")),
            ]
        )

        findings = engine._validate_skill_triggers()

        # (skill_key, dangling_tools, live) — live=True logs as drift
        # warning, live=False as inert info; clean skills are absent.
        assert findings == [
            ("skill:drift", ["slack_conversations_add_message"], True),
            ("skill:inert", ["gh_create_pr"], False),
        ]

    def test_configured_server_leaf_upgrades_dead_skill_to_drift(self) -> None:
        """A skill whose only tool leaves dangle but whose mcp_server
        leaf names a CONFIGURED server is drift, not absence — the
        server is deployed, the tool name just doesn't exist on it."""
        from crewlet.agent.skills.models import TriggerExpr

        engine = self._engine_with_surface()
        engine._prompt_skill_registry.seed(
            [
                self._skill(
                    "skill:slack",
                    TriggerExpr(
                        any_of=[
                            TriggerExpr(tool="slack_postMsg"),
                            TriggerExpr(mcp_server="slack"),
                        ]
                    ),
                )
            ]
        )
        assert engine._validate_skill_triggers() == [
            ("skill:slack", ["slack_postMsg"], True)
        ]
