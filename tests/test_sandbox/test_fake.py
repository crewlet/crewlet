"""Tests for the sandbox fakes + ``SandboxManager``."""

from __future__ import annotations

import pytest

from crewlet.sandbox import (
    CodingAgentResult,
    FakeCodingAgentRunner,
    FakeSandboxProvider,
    RunLimits,
    SandboxManager,
    SandboxSpec,
)


async def test_fake_sandbox_exec_and_files() -> None:
    provider = FakeSandboxProvider()
    sb = await provider.create(SandboxSpec())
    res = await sb.exec("echo hi", env={"X": "1"})
    assert res.exit_code == 0
    assert sb.commands == ["echo hi"]  # type: ignore[attr-defined]
    await sb.write_file("/work/a.txt", "hello")
    assert await sb.read_file("/work/a.txt") == b"hello"
    await sb.close()
    assert sb.closed is True  # type: ignore[attr-defined]
    assert provider.created[0].coding_agent == "claude-code"


async def test_fake_runner_default_result() -> None:
    runner = FakeCodingAgentRunner()
    provider = FakeSandboxProvider()
    sb = await provider.create(SandboxSpec())
    await runner.install(sb)
    out = await runner.run(sb, brief="do X", env={"K": "v"}, limits=RunLimits())
    assert out.success is True
    assert out.delivered_refs
    assert runner.runs[0][0] == "do X"
    assert runner.installed == [sb]


async def test_manager_acquire_installs_runner() -> None:
    provider = FakeSandboxProvider()
    runner = FakeCodingAgentRunner(name="claude-code")
    mgr = SandboxManager(provider=provider, runners={"claude-code": runner})
    spec = mgr.build_spec(coding_agent="claude-code", env={"ANTHROPIC_API_KEY": "x"})
    assert spec.coding_agent == "claude-code"
    assert spec.env == {"ANTHROPIC_API_KEY": "x"}
    sb, r = await mgr.acquire(spec)
    assert r is runner
    assert runner.installed == [sb]
    assert provider.created == [spec]
    assert mgr.provider_kind == "fake"


async def test_manager_acquire_runs_setup_with_spec_env() -> None:
    # acquire threads spec.env into apply_setup, so provisioning commands
    # execute WITH the run env — a config recipe reads $CREWLET_AGENT_* /
    # its declared tokens at provisioning time. This is the unit pin on the
    # manager wiring itself (the launch test pins it end-to-end).
    from crewlet.sandbox.setup import SandboxSetupStep

    provider = FakeSandboxProvider()
    mgr = SandboxManager(
        provider=provider,
        runners={"claude-code": FakeCodingAgentRunner(name="claude-code")},
    )
    run_env = {"CREWLET_AGENT_HANDLE": "eng", "GITHUB_TOKEN": "ghp_x"}
    step = SandboxSetupStep(
        name="identity",
        commands=['git config --global user.name "$CREWLET_AGENT_HANDLE"'],
    )
    await mgr.acquire(mgr.build_spec(env=run_env), setup=[step])
    box = provider.sandboxes[0]
    assert box.exec_envs[box.commands.index(step.commands[0])] == run_env


async def test_manager_build_spec_defaults_and_pause_ttl() -> None:
    mgr = SandboxManager(
        provider=FakeSandboxProvider(),
        runners={"claude-code": FakeCodingAgentRunner()},
        default_timeout_s=600.0,
        default_pause_ttl_s=1200.0,
    )
    # pause_ttl_s < 0 -> provider default; == 0 -> disabled (never pause)
    assert mgr.build_spec().pause_ttl_s == 1200.0
    assert mgr.build_spec(pause_ttl_s=0.0).pause_ttl_s == 0.0
    assert mgr.build_spec(timeout_s=30.0).timeout_s == 30.0
    assert mgr.build_spec().timeout_s == 600.0


async def test_fake_provider_kill_by_id_does_not_resume() -> None:
    # Mirrors the real provider: kill-by-id destroys the box without going
    # through connect() (which auto-resumes a paused sandbox).
    provider = FakeSandboxProvider()
    box = await provider.create(SandboxSpec())
    await box.pause()

    await provider.kill(box.id)

    assert box.closed is True
    assert box.paused is True
    # An unknown / already-gone id is a no-op, not an error.
    await provider.kill("never-existed")


def test_manager_runner_for_unknown_raises() -> None:
    mgr = SandboxManager(provider=FakeSandboxProvider(), runners={})
    with pytest.raises(KeyError):
        mgr.runner_for("nope")


async def test_manager_acquire_teardown_on_install_failure() -> None:
    class _BoomRunner:
        name = "claude-code"

        async def install(self, sandbox: object) -> None:
            raise RuntimeError("boom")

        async def run(
            self, sandbox: object, *, brief: str, env: dict, limits: RunLimits
        ) -> CodingAgentResult:
            return CodingAgentResult()

    provider = FakeSandboxProvider()
    mgr = SandboxManager(provider=provider, runners={"claude-code": _BoomRunner()})
    with pytest.raises(RuntimeError):
        await mgr.acquire(mgr.build_spec())
    # the sandbox was created then torn down so it is never leaked
    assert provider.sandboxes[0].closed is True


async def test_manager_acquire_teardown_on_setup_failure() -> None:
    # A failing SETUP step (operator-configured provisioning) must also tear
    # the box down and propagate — a half-provisioned box must never reach a
    # run whose brief promises the full environment.
    from crewlet.sandbox.fake import FakeSandbox
    from crewlet.sandbox.protocol import ExecResult
    from crewlet.sandbox.setup import SandboxSetupError, SandboxSetupStep

    class _FailingSandbox(FakeSandbox):
        async def exec(self, cmd: str, **kw: object) -> ExecResult:
            self.commands.append(cmd)
            return ExecResult(exit_code=1, stdout="", stderr="broken tool")

    class _Provider(FakeSandboxProvider):
        async def create(self, spec: SandboxSpec) -> _FailingSandbox:
            self.created.append(spec)
            sb = _FailingSandbox(sandbox_id=f"fake-{len(self.sandboxes) + 1}")
            self.sandboxes.append(sb)
            self._by_id[sb.id] = sb
            return sb

    provider = _Provider()
    mgr = SandboxManager(
        provider=provider,
        runners={"claude-code": FakeCodingAgentRunner(name="claude-code")},
    )
    step = SandboxSetupStep(name="broken", commands=["definitely-not-a-tool"])
    with pytest.raises(SandboxSetupError, match="broken"):
        await mgr.acquire(mgr.build_spec(), setup=[step])
    assert provider.sandboxes[0].closed is True
