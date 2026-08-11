"""Tests for the sandbox setup-step framework (``crewlet.sandbox.setup``).

Provisioning is declarative: ordered ``SandboxSetupStep``s each contribute
files, commands, env, and a brief paragraph. The engine ships NO steps of
its own — git auth included: the documented recipe lives in
``examples/nimbus.company.yaml`` (``providers.sandbox.setup``), and the
git-specific tests here run against THAT config-authored step so the
security properties (host-scoped helper, ``--add`` rewrites, token
hygiene) can't silently regress in the example everyone copies.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from crewlet.sandbox.fake import FakeSandbox
from crewlet.sandbox.protocol import ExecResult
from crewlet.sandbox.setup import (
    SandboxSetupError,
    SandboxSetupStep,
    apply_setup,
    environment_brief,
    setup_env,
)

_EXAMPLE = Path(__file__).resolve().parents[2] / "examples" / "nimbus.company.yaml"


def _example_git_step() -> SandboxSetupStep:
    """The ``git-auth`` recipe as shipped in the nimbus example config."""
    from crewlet.config import load_company_config

    cfg = load_company_config(str(_EXAMPLE))
    assert cfg.providers.sandbox is not None
    for step in cfg.providers.sandbox.setup:
        if step.name == "git-auth":
            return step
    raise AssertionError("nimbus example does not ship the git-auth step")


# ---------------------------------------------------------------------------
# the git-auth CONFIG RECIPE (examples/nimbus.company.yaml)
# ---------------------------------------------------------------------------


def test_example_git_step_wires_scoped_helper_and_rewrites_ssh() -> None:
    step = _example_git_step()
    joined = "\n".join(step.commands)
    # The helper is SCOPED to gitlab.com — an unscoped helper would be
    # consulted for every https host and hand the PAT to any server that
    # challenges.
    assert 'credential."https://gitlab.com".helper' in joined
    assert "git config --global credential.helper " not in joined
    # SSH GitLab remotes are rewritten to HTTPS so the token covers them too.
    # insteadOf is MULTI-VALUED: both commands must use --add, else the
    # second silently replaces the first and scp-style remotes stay SSH.
    assert '--add url."https://gitlab.com/".insteadOf "git@gitlab.com:"' in joined
    assert '--add url."https://gitlab.com/".insteadOf "ssh://git@gitlab.com/"' in joined
    # Commit identity maps the ENGINE's generic facts into git shape — the
    # recipe, not the engine, owns the tool-specific wiring (setup commands
    # run WITH the run env, so $CREWLET_* resolves at provisioning time).
    assert 'git config --global user.name "$CREWLET_AGENT_HANDLE"' in joined
    assert 'git config --global user.email "$CREWLET_AGENT_EMAIL"' in joined
    # Headless git must fail fast on any credential the helper won't answer,
    # never hang on an interactive prompt — recipe env, not engine env.
    assert step.env.get("GIT_TERMINAL_PROMPT") == "0"
    # The hint side: the step's brief tells the coding agent to use the
    # injected token — the original failure was the agent not reasoning
    # about it.
    assert "$GITLAB_TOKEN" in step.brief
    assert "https://gitlab.com" in step.brief


def test_example_git_helper_script_host_gating_behaviour() -> None:
    # Execute the actual script shipped in the example: it must answer for
    # gitlab.com, and stay silent for foreign hosts, lookalike subdomains,
    # missing tokens, and non-get operations — the PAT must never be
    # offered anywhere else.
    import os
    import subprocess
    import tempfile

    step = _example_git_step()
    (script,) = step.files.values()
    assert "$GITLAB_TOKEN" in script  # read from env at git-runtime, not disk
    with tempfile.NamedTemporaryFile("w", suffix=".sh", delete=False) as f:
        f.write(script)
        path = f.name
    try:
        env = dict(os.environ, GITLAB_TOKEN="tok123")

        def run(op: str, stdin: str, e: dict[str, str]) -> str:
            return subprocess.run(
                ["sh", path, op], input=stdin, capture_output=True, text=True, env=e
            ).stdout

        # PAT basic-auth: GitLab ignores the username, so the recipe uses
        # the conventional "oauth2" placeholder.
        gitlab = "protocol=https\nhost=gitlab.com\n\n"
        assert run("get", gitlab, env) == ("username=oauth2\npassword=tok123\n")
        assert run("get", "protocol=https\nhost=github.com\n\n", env) == ""
        assert run("get", "protocol=https\nhost=gitlab.com.evil.io\n\n", env) == ""
        no_token = {k: v for k, v in env.items() if k != "GITLAB_TOKEN"}
        assert run("get", gitlab, no_token) == ""
        assert run("store", gitlab, env) == ""
    finally:
        os.unlink(path)


def test_example_git_helper_survives_config_env_resolution() -> None:
    # ${VAR} interpolation runs over step files/commands when the manager is
    # built; the shipped script must come through byte-meaningful — shell
    # syntax intact and no config-time substitution of its runtime
    # references (the resolver only touches ${NAME}-shaped tokens, and the
    # script deliberately uses none).
    from crewlet.config import resolve_setup_step_content

    step = _example_git_step()
    resolved = resolve_setup_step_content(step)
    assert resolved.files == step.files
    assert resolved.commands == step.commands


# ---------------------------------------------------------------------------
# apply_setup
# ---------------------------------------------------------------------------


async def test_apply_setup_writes_files_then_runs_commands() -> None:
    sb = FakeSandbox()
    git = _example_git_step()
    step = SandboxSetupStep(
        name="custom",
        files={"/etc/thing.conf": "key=value"},
        commands=["chmod 600 /etc/thing.conf", "thing --init"],
    )
    await apply_setup(sb, [git, step])
    (helper_path,) = git.files
    assert sb._files[helper_path].decode() == git.files[helper_path]
    assert sb._files["/etc/thing.conf"] == b"key=value"
    for cmd in [*git.commands, *step.commands]:
        assert cmd in sb.commands


async def test_apply_setup_runs_commands_with_run_env() -> None:
    # Setup commands execute WITH the run env, so a recipe can map the
    # engine's generic identity facts into tool shape at provisioning time
    # (e.g. git config user.name "$CREWLET_AGENT_HANDLE").
    sb = FakeSandbox()
    step = SandboxSetupStep(
        name="identity",
        commands=['git config --global user.name "$CREWLET_AGENT_HANDLE"'],
    )
    run_env = {"CREWLET_AGENT_HANDLE": "eng", "GITHUB_TOKEN": "tok"}
    await apply_setup(sb, [step], env=run_env)
    assert sb.exec_envs[-1] == run_env


async def test_apply_setup_failure_raises() -> None:
    class _FailingSandbox(FakeSandbox):
        async def exec(self, cmd: str, **kw: object) -> ExecResult:
            self.commands.append(cmd)
            return ExecResult(exit_code=1, stdout="", stderr="no such tool")

    step = SandboxSetupStep(name="broken", commands=["definitely-not-a-tool"])
    with pytest.raises(SandboxSetupError, match="broken"):
        await apply_setup(_FailingSandbox(), [step])


# ---------------------------------------------------------------------------
# env merge + brief composition
# ---------------------------------------------------------------------------


def test_setup_env_merges_in_step_order() -> None:
    a = SandboxSetupStep(name="a", env={"X": "1", "SHARED": "a"})
    b = SandboxSetupStep(name="b", env={"Y": "2", "SHARED": "b"})
    assert setup_env([a, b]) == {"X": "1", "Y": "2", "SHARED": "b"}


def test_environment_brief_composes_step_briefs_and_mcp() -> None:
    # The config-authored git-auth step's brief is what tells the coding
    # agent to use $GITLAB_TOKEN — composed with every other step's brief.
    git = _example_git_step()
    node = SandboxSetupStep(name="node", brief="Node 22 + pnpm are preinstalled.")
    silent = SandboxSetupStep(name="quiet", commands=["true"])  # no brief
    brief = environment_brief([git, node, silent], mcp_servers=["gitlab", "jira"])
    assert "## Your environment" in brief
    assert "isolated sandbox" in brief
    assert "$GITLAB_TOKEN" in brief
    assert "Node 22 + pnpm" in brief
    assert "MCP tool servers connected: gitlab, jira" in brief


def test_environment_brief_without_steps_or_servers_is_generic() -> None:
    brief = environment_brief([])
    assert "GITLAB_TOKEN" not in brief
    assert "MCP tool servers" not in brief
    assert "isolated sandbox" in brief


def test_setup_step_rejects_unknown_fields() -> None:
    # Config-loadable model: a typo'd key must fail loudly, not be ignored.
    from pydantic import ValidationError

    with pytest.raises(ValidationError):
        SandboxSetupStep(name="x", command=["oops"])  # type: ignore[call-arg]
