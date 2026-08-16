"""Tests for sandbox config wiring: provider/role config,
``_parse_role``, and the ``sandbox`` phase resolution."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.config import (
    ProvidersConfig,
    SandboxProviderConfig,
    TurnEngineConfig,
    _parse_role,
)
from crewlet.org.models import Role, RoleSandboxConfig


class _StubProvider:
    """Minimal LLMProvider-shaped stub (resolution never calls it)."""

    def __init__(self, model: str) -> None:
        self.model = model


# --- provider / turn-engine config ----------------------------------------


def test_sandbox_provider_config_defaults() -> None:
    c = SandboxProviderConfig()
    assert c.type == "e2b"
    assert c.default_coding_agent == "claude-code"
    assert c.default_pause_ttl_seconds == 1800.0


def test_sandbox_provider_config_extra_forbid() -> None:
    with pytest.raises(ValidationError):
        SandboxProviderConfig(nope=1)  # type: ignore[call-arg]


def test_sandbox_provider_config_rejects_unbounded_pause() -> None:
    # A negative pause TTL would flow through build_spec onto the run row and
    # leave the reaper with no deadline to enforce — an eternally paused,
    # eternally billed box, which is the exact leak the knob prevents. A zero
    # box TTL is likewise nonsense (build_spec reads 0 as "inherit").
    with pytest.raises(ValidationError):
        SandboxProviderConfig(default_pause_ttl_seconds=-1.0)
    with pytest.raises(ValidationError):
        SandboxProviderConfig(default_timeout_seconds=0.0)
    # 0 is a real, documented choice: never pause, always re-seed.
    assert SandboxProviderConfig(default_pause_ttl_seconds=0.0)


def test_sandbox_provider_config_rejects_resource_limits() -> None:
    # Sandbox resources are a property of the TEMPLATE (fixed at template
    # build time); the create API takes none, so an engine-side limits knob
    # could only be parsed and dropped. `extra="forbid"` makes a config that
    # still carries the old block fail loudly instead of silently ignoring it.
    with pytest.raises(ValidationError):
        SandboxProviderConfig(  # type: ignore[call-arg]
            default_limits={"vcpu": 8, "memory_mb": 16384}
        )


def test_providers_config_sandbox_optional() -> None:
    assert ProvidersConfig().sandbox is None
    p = ProvidersConfig(sandbox={"type": "fake"})  # type: ignore[arg-type]
    assert p.sandbox is not None
    assert p.sandbox.type == "fake"


def test_turn_engine_sandbox_knobs() -> None:
    te = TurnEngineConfig()
    assert te.sandbox_budget_fraction == 0.5
    assert te.sandbox_min_budget_tokens == 2000


# --- role parsing ---------------------------------------------------------


def test_parse_role_sandbox_and_llm_sandbox() -> None:
    role = _parse_role(
        {
            "name": "Senior Engineer",
            "llm_sandbox": "claude-prov",
            "sandbox": {
                "enabled": True,
                "coding_agent": "opencode",
                "env": {"NPM_TOKEN": "${NPM_TOKEN}"},
                "mcp": {"servers": ["github"]},
            },
        }
    )
    assert role.llm_sandbox == "claude-prov"
    assert isinstance(role.sandbox, RoleSandboxConfig)
    assert role.sandbox.enabled is True
    assert role.sandbox.coding_agent == "opencode"
    assert role.sandbox.env == {"NPM_TOKEN": "${NPM_TOKEN}"}
    assert role.sandbox.mcp.servers == ["github"]


def test_parse_role_sandbox_rejects_dropped_allow_field() -> None:
    # `mcp.allow` is not a valid
    # field (RoleSandboxMCPConfig forbids extras).
    import pytest
    from pydantic import ValidationError

    with pytest.raises(ValidationError):
        _parse_role(
            {
                "name": "Senior Engineer",
                "sandbox": {
                    "enabled": True,
                    "mcp": {"servers": ["github"], "allow": ["mcp__github__*"]},
                },
            }
        )


def test_parse_role_no_sandbox() -> None:
    role = _parse_role({"name": "PM"})
    assert role.sandbox is None
    assert role.llm_sandbox is None


def test_role_sandbox_coding_agent_defaults_to_inherit() -> None:
    # Empty (the default) means "inherit providers.sandbox.default_coding_agent"
    # at launch — NOT a hardcoded "claude-code", so setting the provider
    # default to "opencode" actually reaches roles that don't override it.
    assert RoleSandboxConfig().coding_agent == ""
    role = _parse_role({"name": "Eng", "sandbox": {"enabled": True}})
    assert role.sandbox is not None
    assert role.sandbox.coding_agent == ""


def test_parse_role_llm_dict_form_sandbox() -> None:
    role = _parse_role({"name": "Eng", "llm": {"default": "d", "sandbox": "s"}})
    assert role.llm_sandbox == "s"


def test_role_sandbox_extra_forbid() -> None:
    with pytest.raises(ValidationError):
        RoleSandboxConfig(nope=1)  # type: ignore[call-arg]


# --- setup steps (providers.sandbox.setup / role.sandbox.setup) ------------


def test_provider_config_parses_setup_steps() -> None:
    c = SandboxProviderConfig(
        type="fake",
        setup=[  # type: ignore[list-item]
            {
                "name": "ca",
                "files": {"/etc/ca.pem": "cert"},
                "commands": ["update-ca-certificates"],
                "env": {"SSL_CERT_FILE": "/etc/ca.pem"},
                "brief": "Internal CA installed.",
            }
        ],
    )
    from crewlet.sandbox.setup import SandboxSetupStep

    assert isinstance(c.setup[0], SandboxSetupStep)
    assert c.setup[0].name == "ca"
    assert c.setup[0].commands == ["update-ca-certificates"]
    # Default: no engine-wide steps.
    assert SandboxProviderConfig().setup == []


def test_parse_role_sandbox_setup_steps() -> None:
    role = _parse_role(
        {
            "name": "Frontend Engineer",
            "sandbox": {
                "enabled": True,
                "setup": [
                    {
                        "name": "node",
                        "commands": ["corepack enable"],
                        "env": {"NPM_TOKEN": "${NPM_TOKEN}"},
                        "brief": "Node 22 + pnpm preinstalled.",
                    }
                ],
            },
        }
    )
    assert role.sandbox is not None
    step = role.sandbox.setup[0]
    assert step.name == "node"
    # ${VAR} stays verbatim at parse time (resolved once, at launch).
    assert step.env == {"NPM_TOKEN": "${NPM_TOKEN}"}


def test_parse_role_sandbox_setup_rejects_typo_field() -> None:
    # extra=forbid must hold at the config surface too — a typo'd step key
    # (e.g. `command` for `commands`) fails loudly instead of silently
    # skipping the provisioning.
    with pytest.raises(ValidationError):
        _parse_role(
            {
                "name": "Eng",
                "sandbox": {
                    "enabled": True,
                    "setup": [{"name": "node", "command": ["corepack enable"]}],
                },
            }
        )


def test_resolve_setup_step_content_resolves_files_commands_not_env(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # files + commands resolve at step load; env stays VERBATIM so it is
    # resolved exactly once later in build_sandbox_env (double resolution
    # would mangle a secret containing a literal ${...}). brief/name are
    # never resolved.
    from crewlet.config import resolve_setup_step_content
    from crewlet.sandbox.setup import SandboxSetupStep

    monkeypatch.setenv("CA_PEM", "cert-body")
    step = SandboxSetupStep(
        name="ca-${CA_PEM}",
        files={"/etc/ca.pem": "${CA_PEM}"},
        commands=["echo ${CA_PEM}", "echo $stays"],
        env={"SSL_CERT": "${CA_PEM}"},
        brief="uses ${CA_PEM}",
    )
    resolved = resolve_setup_step_content(step)
    assert resolved.files == {"/etc/ca.pem": "cert-body"}
    assert resolved.commands == ["echo cert-body", "echo $stays"]
    assert resolved.env == {"SSL_CERT": "${CA_PEM}"}  # verbatim
    assert resolved.brief == "uses ${CA_PEM}"  # never resolved
    assert resolved.name == "ca-${CA_PEM}"  # never resolved


# --- the sandbox phase resolves to execute by default ---------------------


def test_sandbox_phase_defaults_to_execute() -> None:
    role = Role(name="Eng", llm="default", llm_execute="exec-prov")
    providers = {"default": _StubProvider("d"), "exec-prov": _StubProvider("e")}
    key, _ = resolve_phase_provider(role, "sandbox", providers)  # type: ignore[arg-type]
    assert key == "exec-prov"


def test_sandbox_phase_override() -> None:
    role = Role(
        name="Eng", llm="default", llm_execute="exec-prov", llm_sandbox="sb-prov"
    )
    providers = {
        "default": _StubProvider("d"),
        "exec-prov": _StubProvider("e"),
        "sb-prov": _StubProvider("s"),
    }
    key, _ = resolve_phase_provider(role, "sandbox", providers)  # type: ignore[arg-type]
    assert key == "sb-prov"


def test_sandbox_phase_falls_back_to_llm() -> None:
    role = Role(name="Eng", llm="default")
    providers = {"default": _StubProvider("d")}
    key, _ = resolve_phase_provider(role, "sandbox", providers)  # type: ignore[arg-type]
    assert key == "default"


# Code work is the run_sandbox Execute tool now, not an ExecutionPlan
# backend field — see tests/test_tools/test_run_sandbox_tool.py.


class TestLocalSandboxConfig:
    """``providers.sandbox.type: local`` — the engine-host backend.

    See ``docs/concepts/code-sandbox.md#local-sandboxes``.
    """

    def test_direct_is_the_default_containment(self) -> None:
        from crewlet.config import SandboxProviderConfig

        cfg = SandboxProviderConfig(type="local", local={})
        assert cfg.local is not None
        assert cfg.local.containment == "direct"

    def test_type_requires_the_block(self) -> None:
        """'direct' runs the coding agent as the engine user with no host
        isolation — never assumed from a bare type."""
        from crewlet.config import SandboxProviderConfig

        with pytest.raises(ValidationError, match="needs a `local:` block"):
            SandboxProviderConfig(type="local")

    def test_block_on_another_type_is_rejected(self) -> None:
        from crewlet.config import SandboxProviderConfig

        with pytest.raises(ValidationError, match="only applies to"):
            SandboxProviderConfig(type="e2b", local={"containment": "direct"})

    def test_container_requires_an_image(self) -> None:
        from crewlet.config import SandboxProviderConfig

        with pytest.raises(ValidationError, match="requires `image`"):
            SandboxProviderConfig(type="local", local={"containment": "container"})

    def test_image_on_direct_is_rejected(self) -> None:
        from crewlet.config import SandboxProviderConfig

        with pytest.raises(ValidationError, match="only applies to containment"):
            SandboxProviderConfig(
                type="local", local={"containment": "direct", "image": "acme/x"}
            )

    def test_container_block_round_trips(self) -> None:
        from crewlet.config import SandboxProviderConfig

        cfg = SandboxProviderConfig(
            type="local",
            local={
                "containment": "container",
                "image": "acme/coding:1",
                "runtime": "podman",
                "network": "none",
                "run_args": ["--cpus", "2"],
            },
        )
        assert cfg.local is not None
        assert cfg.local.runtime == "podman"
        assert cfg.local.run_args == ["--cpus", "2"]

    def test_extra_keys_rejected(self) -> None:
        from crewlet.config import SandboxProviderConfig

        with pytest.raises(ValidationError):
            SandboxProviderConfig(type="local", local={"containmnet": "direct"})

    def test_builder_constructs_the_local_provider(self, tmp_path) -> None:
        from crewlet.config import CompanyConfig
        from crewlet.engine_builders import build_sandbox_manager
        from crewlet.sandbox.local import LocalSandboxProvider

        cfg = CompanyConfig.model_validate(
            {
                "name": "A",
                "providers": {
                    "sandbox": {
                        "type": "local",
                        "local": {
                            "containment": "direct",
                            "state_dir": str(tmp_path / "boxes"),
                        },
                    }
                },
            }
        )
        manager = build_sandbox_manager(cfg)
        assert manager is not None
        assert isinstance(manager.provider, LocalSandboxProvider)
        assert manager.provider_kind == "local"
        # The SAME runners as E2B — the Sandbox protocol is what makes the
        # backend swap free.
        assert manager.runner_for("claude-code").name == "claude-code"
        assert manager.runner_for("opencode").name == "opencode"
