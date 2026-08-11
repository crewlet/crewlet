"""Tests for E2BSandboxProvider's SDK-independent logic.

The real provisioning path needs the optional ``e2b`` SDK + network, so
only the pure mapping logic is unit-tested here (per the project testing
rules — no real sandbox / network).
"""

from __future__ import annotations

import pytest

from crewlet.sandbox.e2b import E2BSandboxProvider
from crewlet.sandbox.protocol import SandboxSpec


def test_provider_kind() -> None:
    assert E2BSandboxProvider().kind == "e2b"


def test_template_selection_precedence() -> None:
    # explicit spec template wins
    p = E2BSandboxProvider(template="provider-tpl")
    assert p._template_for(SandboxSpec(template="spec-tpl")) == "spec-tpl"
    # else provider default
    assert p._template_for(SandboxSpec(coding_agent="claude-code")) == "provider-tpl"
    # else E2B's prebuilt per coding agent
    bare = E2BSandboxProvider()
    assert bare._template_for(SandboxSpec(coding_agent="claude-code")) == "claude"
    assert bare._template_for(SandboxSpec(coding_agent="opencode")) == "opencode"


def test_connect_kwargs_only_include_set_fields() -> None:
    assert E2BSandboxProvider()._kwargs() == {}
    full = E2BSandboxProvider(api_key="k", domain="local.example")
    assert full._kwargs() == {"api_key": "k", "domain": "local.example"}


async def test_sandbox_set_timeout_resets_box_ttl() -> None:
    # The waiter's keepalive: set_timeout int-coerces and forwards to the SDK
    # so a running box's TTL is refreshed (no run-time limit). A non-positive
    # value is a no-op — never reset the box to an immediate kill.
    from crewlet.sandbox.e2b import E2BSandbox

    class _FakeSbx:
        def __init__(self) -> None:
            self.timeouts: list[int] = []

        async def set_timeout(self, t: int) -> None:
            self.timeouts.append(t)

    sbx = _FakeSbx()
    box = E2BSandbox(sbx)
    await box.set_timeout(900.0)
    assert sbx.timeouts == [900]
    await box.set_timeout(0.0)
    assert sbx.timeouts == [900]


async def test_pause_prefers_the_stable_method_over_the_deprecated_alias() -> None:
    # `beta_pause` is deprecated in favour of `pause`. Probing only for the
    # deprecated name means that the day it is removed, pausing silently
    # becomes a no-op — every "paused" box keeps running and billing until
    # its TTL, and the pause reaper has nothing to reclaim.
    from crewlet.sandbox.e2b import E2BSandbox

    class _Both:
        def __init__(self) -> None:
            self.called: list[str] = []

        async def pause(self) -> None:
            self.called.append("pause")

        async def beta_pause(self) -> None:
            self.called.append("beta_pause")

    both = _Both()
    await E2BSandbox(both).pause()
    assert both.called == ["pause"]

    # An SDK that only carries the old alias still pauses.
    class _OnlyBeta:
        def __init__(self) -> None:
            self.called: list[str] = []

        async def beta_pause(self) -> None:
            self.called.append("beta_pause")

    only_beta = _OnlyBeta()
    await E2BSandbox(only_beta).pause()
    assert only_beta.called == ["beta_pause"]


async def test_provider_kill_uses_the_by_id_variant(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # The reaper must destroy a PAUSED box without resuming it: connect()
    # auto-resumes, which would boot the VM back up purely to shut it down.
    # The SDK's static kill(sandbox_id) does it over the API instead.
    import crewlet.sandbox.e2b as e2b_mod

    killed: list[tuple[str, dict]] = []

    class _SDK:
        @staticmethod
        async def kill(sandbox_id: str, **kw: object) -> bool:
            killed.append((sandbox_id, dict(kw)))
            return True

    monkeypatch.setattr(e2b_mod, "_require_e2b", lambda: _SDK)
    await E2BSandboxProvider(api_key="k", domain="local.example").kill("sbx-9")

    assert killed == [("sbx-9", {"api_key": "k", "domain": "local.example"})]


async def test_exec_maps_nonzero_exit_to_result_not_exception() -> None:
    # The e2b SDK RAISES CommandExitException on any non-zero exit, but the
    # Sandbox protocol reports the exit code in ExecResult — callers depend
    # on it: the `kill -0` liveness probe reads exit 1 as "process gone" and
    # apply_setup turns a failed provisioning command into SandboxSetupError.
    # Without this mapping a dead detached job would RAISE out of poll()
    # instead of completing the run.
    # The optional `sandbox` extra provides the SDK; without it there is
    # no real exception type to map, so skip (the rest of this module
    # stays SDK-independent — see the module docstring). CI installs
    # `.[dev,all]`, which includes e2b, so this still runs there.
    e2b = pytest.importorskip("e2b")
    CommandExitException = e2b.CommandExitException

    from crewlet.sandbox.e2b import E2BSandbox

    class _FakeCommands:
        async def run(self, cmd: str, **kw: object) -> object:
            raise CommandExitException(
                stdout="", stderr="no such process", exit_code=1, error=""
            )

    class _FakeSbx:
        commands = _FakeCommands()

    res = await E2BSandbox(_FakeSbx()).exec("kill -0 12345 2>/dev/null")
    assert res.exit_code == 1
    assert res.stderr == "no such process"


async def test_create_without_sdk_raises_actionable_error(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # Force the lazy import to fail regardless of whether e2b is installed.
    import builtins

    real_import = builtins.__import__

    def _blocked(name, *args, **kwargs):
        if name == "e2b" or name.startswith("e2b."):
            raise ImportError("no e2b")
        return real_import(name, *args, **kwargs)

    monkeypatch.setattr(builtins, "__import__", _blocked)
    with pytest.raises(RuntimeError, match="requires the 'e2b' package"):
        await E2BSandboxProvider().create(SandboxSpec())
