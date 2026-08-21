"""Profile registry invariants for the ``cli-agent`` LLM backend."""

from __future__ import annotations

import pytest

from crewlet.providers.llm.cli_profiles import (
    CLI_AGENT_NAMES,
    MODEL_PLACEHOLDER,
    PROMPT_PLACEHOLDER,
    CLIAgentProfile,
    get_profile,
    resolve_profile,
)


class TestRegistry:
    def test_every_declared_name_resolves(self):
        for name in CLI_AGENT_NAMES:
            assert get_profile(name).name == name

    def test_unknown_name_lists_the_valid_ones(self):
        with pytest.raises(KeyError) as excinfo:
            get_profile("not-a-cli")
        assert "claude-code" in str(excinfo.value)

    def test_config_literal_matches_the_registry(self):
        """``providers.llm.<key>.cli.agent`` must accept exactly the
        profiles that exist — a name in one and not the other is a
        backend an operator can select but the engine cannot build."""
        from typing import get_args

        from crewlet.config import CLIAgentConfig

        field = CLIAgentConfig.model_fields["agent"]
        assert set(get_args(field.annotation)) == set(CLI_AGENT_NAMES)

    def test_only_custom_ships_without_a_binary(self):
        missing = [n for n in CLI_AGENT_NAMES if not get_profile(n).binary]
        assert missing == ["custom"]

    def test_built_ins_declare_credentials_or_a_token(self):
        """Every real profile must tell the workspace layer what to sync;
        a profile with neither cannot survive an engine restart."""
        for name in CLI_AGENT_NAMES:
            if name == "custom":
                continue
            profile = get_profile(name)
            assert profile.credential_paths or profile.token_env, name

    def test_built_ins_wipe_their_conversation_state(self):
        """The isolation guarantee is only as good as this list."""
        for name in CLI_AGENT_NAMES:
            if name == "custom":
                continue
            assert get_profile(name).volatile_paths, name

    def test_no_credential_path_is_also_volatile(self):
        """Pruning runs after the credential sync-back; a path in both
        lists would delete the login the run just refreshed."""
        for name in CLI_AGENT_NAMES:
            profile = get_profile(name)
            for credential in profile.credential_paths:
                for volatile in profile.volatile_paths:
                    assert not credential.startswith(volatile.rstrip("/") + "/")
                    assert credential != volatile

    def test_model_args_carry_the_placeholder(self):
        for name in CLI_AGENT_NAMES:
            profile = get_profile(name)
            if profile.model_args:
                assert MODEL_PLACEHOLDER in " ".join(profile.model_args), name

    def test_argv_prompt_profiles_place_the_placeholder(self):
        for name in CLI_AGENT_NAMES:
            profile = get_profile(name)
            if profile.prompt_via == "argv" and profile.complete_args:
                assert PROMPT_PLACEHOLDER in " ".join(profile.complete_args), name

    def test_failure_patterns_compile(self):
        import re

        for name in CLI_AGENT_NAMES:
            profile = get_profile(name)
            for pattern in (
                *profile.rate_limit_patterns,
                *profile.auth_error_patterns,
            ):
                re.compile(pattern)


class TestOverrides:
    def test_override_replaces_a_field(self):
        merged = resolve_profile("claude-code", {"binary": "/opt/bin/claude"})
        assert merged.binary == "/opt/bin/claude"
        # Untouched fields survive.
        assert merged.token_env == "CLAUDE_CODE_OAUTH_TOKEN"

    def test_list_override_replaces_wholesale(self):
        merged = resolve_profile("codex", {"complete_args": ["exec", "-"]})
        assert merged.complete_args == ["exec", "-"]

    def test_none_values_are_dropped(self):
        merged = resolve_profile("codex", {"binary": None})
        assert merged.binary == "codex"

    def test_unknown_override_key_is_rejected(self):
        from pydantic import ValidationError

        with pytest.raises(ValidationError):
            resolve_profile("codex", {"nonsense": 1})

    def test_custom_profile_is_fully_expressible(self):
        merged = resolve_profile(
            "custom",
            {
                "binary": "my-llm",
                "complete_args": ["--json"],
                "text_paths": [["answer"]],
                "output_format": "json",
            },
        )
        assert isinstance(merged, CLIAgentProfile)
        assert merged.binary == "my-llm"
        assert merged.text_paths == [["answer"]]


def test_a_credential_may_not_ride_in_on_passthrough_env():
    """`passthrough_env` forwards from the ENGINE's own environment.

    It is applied before `auth.mode` is consulted, so a key named there
    reaches every seat whatever the mode says — the metered bill on a
    flat-rate plan the mode exists to prevent. Two shipped profiles did
    exactly this (`grok` forwarding `XAI_API_KEY`, `copilot` forwarding
    the org's `GITHUB_TOKEN`), which is why it is checked rather than
    documented.
    """
    import pytest

    from crewlet.providers.llm.cli_profiles import CLIAgentProfile

    with pytest.raises(ValueError, match="passthrough_env"):
        CLIAgentProfile(
            name="leaky",
            binary="leaky",
            api_key_env="VENDOR_API_KEY",
            passthrough_env=["VENDOR_API_KEY"],
        )
    with pytest.raises(ValueError, match="passthrough_env"):
        CLIAgentProfile(
            name="leaky",
            binary="leaky",
            token_env="VENDOR_OAUTH_TOKEN",
            passthrough_env=["VENDOR_OAUTH_TOKEN"],
        )
    # Non-credential config is exactly what the field is for.
    CLIAgentProfile(
        name="fine",
        binary="fine",
        api_key_env="VENDOR_API_KEY",
        passthrough_env=["VENDOR_REGION"],
    )


def test_no_shipped_profile_forwards_its_own_credential():
    """The invariant, on the profiles actually shipped."""
    from crewlet.providers.llm.cli_profiles import _BUILTIN

    assert _BUILTIN, "no profiles to check"
    for name, profile in _BUILTIN.items():
        credentials = {n for n in (profile.api_key_env, profile.token_env) if n}
        assert credentials.isdisjoint(profile.passthrough_env), name
