"""Tests for the ``crewlet gitlab provision`` CLI helpers.

Focused on config resolution: an unset ``${VAR}`` reference (e.g.
``${GITLAB_SIGNING_SECRET}``) must surface as an actionable
:class:`GitLabConfigError`, not a raw pydantic ``ValidationError``.
"""

from __future__ import annotations

import base64
import os
import types

import pytest

from crewlet.config import GitLabConfig, GitLabProvisioningConfig
from crewlet.gitlab.provision_cli import (
    GitLabConfigError,
    _generate_signing_secret,
    _maybe_generate_signing_secret,
    _resolve_gitlab_config,
    _unset_env_refs,
)
from crewlet.provisioning import EnvFileSink, PrintSink


def _cfg(gitlab: GitLabConfig | None) -> types.SimpleNamespace:
    return types.SimpleNamespace(integrations=types.SimpleNamespace(gitlab=gitlab))


def _gitlab_with_ref() -> GitLabConfig:
    # A literal ``${VAR}`` passes construction (non-empty string); it only
    # collapses to "" once resolved against an unset environment.
    return GitLabConfig(
        enabled=True,
        url="https://gitlab.com",
        signing_secret="${GITLAB_SIGNING_SECRET}",
        provisioning=GitLabProvisioningConfig(group="nimbus-hq"),
    )


def test_unset_env_refs_finds_empty_and_missing(monkeypatch):
    monkeypatch.delenv("GITLAB_SIGNING_SECRET", raising=False)
    monkeypatch.setenv("GITLAB_ENGINE_TOKEN", "")  # set-but-empty counts
    monkeypatch.setenv("GITLAB_TOKEN_SET", "glpat-real")
    data = {
        "signing_secret": "${GITLAB_SIGNING_SECRET}",
        "token": "${GITLAB_ENGINE_TOKEN}",
        "nested": [{"k": "${GITLAB_TOKEN_SET}"}],
    }
    assert _unset_env_refs(data) == ["GITLAB_ENGINE_TOKEN", "GITLAB_SIGNING_SECRET"]


def test_resolve_none_when_no_gitlab():
    assert _resolve_gitlab_config(_cfg(None)) is None


def test_resolve_raises_actionable_error_when_secret_unset(monkeypatch):
    monkeypatch.delenv("GITLAB_SIGNING_SECRET", raising=False)
    with pytest.raises(GitLabConfigError) as excinfo:
        _resolve_gitlab_config(_cfg(_gitlab_with_ref()))
    msg = str(excinfo.value)
    assert "signing_secret" in msg
    assert "GITLAB_SIGNING_SECRET" in msg
    assert "whsec_" in msg


def test_resolve_succeeds_when_secret_set(monkeypatch):
    monkeypatch.setenv("GITLAB_SIGNING_SECRET", "whsec_abc123")
    resolved = _resolve_gitlab_config(_cfg(_gitlab_with_ref()))
    assert resolved is not None
    assert resolved.signing_secret == "whsec_abc123"
    assert resolved.enabled is True


# ── auto-generated webhook signing secret ──


def test_generate_signing_secret_is_whsec_of_32_bytes():
    s = _generate_signing_secret()
    assert s.startswith("whsec_")
    assert len(base64.b64decode(s[len("whsec_") :])) == 32
    assert _generate_signing_secret() != s  # CSPRNG, not constant


def _raw(secret: str) -> types.SimpleNamespace:
    return types.SimpleNamespace(signing_secret=secret)


async def test_maybe_generate_mints_when_missing_and_creating_hooks(
    monkeypatch, capsys
):
    monkeypatch.delenv("GITLAB_SIGNING_SECRET", raising=False)
    sink = PrintSink()
    var = await _maybe_generate_signing_secret(
        _raw("${GITLAB_SIGNING_SECRET}"), creating_hooks=True, sink=sink
    )
    assert var == "GITLAB_SIGNING_SECRET"
    # Exported for resolution + recorded to the sink for the engine.
    assert os.environ["GITLAB_SIGNING_SECRET"].startswith("whsec_")
    # Asserted on the sink's actual contract — the line it wrote — rather
    # than on private bookkeeping it has no reason to keep.
    assert "export GITLAB_SIGNING_SECRET=whsec_" in capsys.readouterr().out


async def test_maybe_generate_skips_without_webhook(monkeypatch, capsys):
    monkeypatch.delenv("GITLAB_SIGNING_SECRET", raising=False)
    sink = PrintSink()
    assert (
        await _maybe_generate_signing_secret(
            _raw("${GITLAB_SIGNING_SECRET}"), creating_hooks=False, sink=sink
        )
        is None
    )
    assert "GITLAB_SIGNING_SECRET" not in os.environ
    assert "export GITLAB_SIGNING_SECRET" not in capsys.readouterr().out


async def test_maybe_generate_reuses_env_value(monkeypatch, capsys):
    monkeypatch.setenv("GITLAB_SIGNING_SECRET", "whsec_preset")
    sink = PrintSink()
    assert (
        await _maybe_generate_signing_secret(
            _raw("${GITLAB_SIGNING_SECRET}"), creating_hooks=True, sink=sink
        )
        is None
    )
    assert os.environ["GITLAB_SIGNING_SECRET"] == "whsec_preset"
    # Not regenerated: nothing was minted, so nothing was written.
    assert "export GITLAB_SIGNING_SECRET" not in capsys.readouterr().out


async def test_maybe_generate_reuses_sink_file_value(tmp_path, monkeypatch):
    monkeypatch.delenv("GITLAB_SIGNING_SECRET", raising=False)
    env = tmp_path / ".env.gitlab"
    env.write_text("GITLAB_SIGNING_SECRET=whsec_fromfile\n")
    sink = EnvFileSink(str(env))
    assert (
        await _maybe_generate_signing_secret(
            _raw("${GITLAB_SIGNING_SECRET}"), creating_hooks=True, sink=sink
        )
        is None
    )
    # Pushed the persisted value into the env so resolution + the hook match.
    assert os.environ["GITLAB_SIGNING_SECRET"] == "whsec_fromfile"


async def test_maybe_generate_noop_for_literal_secret(capsys):
    sink = PrintSink()
    assert (
        await _maybe_generate_signing_secret(
            _raw("whsec_literal"), creating_hooks=True, sink=sink
        )
        is None
    )
    assert "export GITLAB_SIGNING_SECRET" not in capsys.readouterr().out
