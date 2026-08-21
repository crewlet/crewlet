"""Tests for the structural secret-path registry: pointers, redact, restore.

The whole config is encrypted as one blob at rest (see
:mod:`crewlet.secrets.document`), so this module only ever operates on the
*decrypted* structure: it locates secrets (:func:`secret_pointers`), masks
them for display (:func:`redact_payload`), and restores masked markers to
their stored values on write-back (:func:`restore_redacted`).
"""

from __future__ import annotations

import copy

import pytest

from crewlet.config import CompanyConfig
from crewlet.config_yaml import company_config_to_dict
from crewlet.secrets.registry import (
    SecretLeakError,
    is_redaction_marker,
    pointer_get,
    pointer_set,
    redact_payload,
    restore_redacted,
    secret_pointers,
)

# Every secret-bearing surface, non-empty so the pointer set is exact.
SAMPLE_INPUT = {
    "name": "Acme",
    "providers": {
        "llm": {
            "default": {
                "type": "openai",
                "model": "gpt-4o",
                "api_keys": ["sk-a", "sk-b"],
            },
            "cheap": {"type": "openai", "model": "gpt-4o-mini", "api_keys": ["sk-c"]},
        },
        "embeddings": {
            "type": "openai",
            "model": "text-embedding-3-small",
            "api_key": "sk-emb",
        },
        "sandbox": {"type": "e2b", "api_key": "e2b-key"},
    },
    "integrations": {
        "jira": {
            "url": "https://x.atlassian.net",
            "token": "jira-tok",
            "webhook_secret": "jira-wh",
        },
        "confluence": {
            "url": "https://x.atlassian.net/wiki",
            "token": "conf-tok",
            "webhook_secret": "conf-wh",
        },
        "github": {"enabled": True, "webhook_secret": "gh-wh"},
        "gitlab": {
            "enabled": True,
            "url": "https://gitlab.com",
            "signing_secret": "whsec_gl",
            "token": "glpat-engine",
        },
        # Disabled so it validates alongside the confluence block above
        # (knowledge-backend exclusivity); pointer emission keys on
        # presence, not on ``enabled``.
        "plane": {
            "enabled": False,
            "url": "https://plane.example",
            "workspace": "acme",
            "webhook_secret": "plane-wh",
            "token": "plane-tok",
        },
    },
    "roles": [
        {"name": "CEO", "mcp_env": {"github": {"Authorization": "Bearer ghp_ceo"}}},
    ],
    "units": [
        {
            "name": "Eng",
            "type": "team",
            "lead": "CTO",
            "mcp_env": {"atlassian": {"JIRA_API_TOKEN": "unit-jira"}},
            "roles": [
                {
                    "name": "CTO",
                    "mcp_env": {"github": {"Authorization": "Bearer ghp_cto"}},
                    "integrations": {
                        "slack": {
                            "bot_token": "xoxb-cto",
                            "signing_secret": "sign-cto",
                            "channel": "C123",
                        }
                    },
                },
            ],
            "children": [
                {
                    "name": "Backend",
                    "type": "team",
                    "roles": [
                        {
                            "name": "Dev",
                            "mcp_env": {"slack": {"SLACK_MCP_XOXB_TOKEN": "xoxb-dev"}},
                            "integrations": {
                                "slack": {
                                    "bot_token": "xoxb-dev-tr",
                                    "signing_secret": "sign-dev",
                                }
                            },
                        },
                    ],
                },
            ],
        },
    ],
}

EXPECTED_POINTERS = {
    "/providers/llm/default/api_keys/0",
    "/providers/llm/default/api_keys/1",
    "/providers/llm/cheap/api_keys/0",
    "/providers/embeddings/api_key",
    "/providers/sandbox/api_key",
    "/integrations/jira/token",
    "/integrations/jira/webhook_secret",
    "/integrations/confluence/token",
    "/integrations/confluence/webhook_secret",
    "/integrations/github/webhook_secret",
    "/integrations/gitlab/token",
    "/integrations/gitlab/signing_secret",
    "/integrations/plane/token",
    "/integrations/plane/webhook_secret",
    "/roles/0/mcp_env/github/Authorization",
    "/units/0/mcp_env/atlassian/JIRA_API_TOKEN",
    "/units/0/roles/0/mcp_env/github/Authorization",
    "/units/0/roles/0/integrations/slack/bot_token",
    "/units/0/roles/0/integrations/slack/signing_secret",
    "/units/0/children/0/roles/0/mcp_env/slack/SLACK_MCP_XOXB_TOKEN",
    "/units/0/children/0/roles/0/integrations/slack/bot_token",
    "/units/0/children/0/roles/0/integrations/slack/signing_secret",
}


def normalized_payload() -> dict:
    """The stored form: validate through CompanyConfig, then serialize —
    exactly what ``RevisionDispatcher`` persists."""
    return company_config_to_dict(CompanyConfig.model_validate(SAMPLE_INPUT))


# ── pointer discovery ─────────────────────────────────────────────────


def test_secret_pointers_exact_set():
    assert set(secret_pointers(normalized_payload())) == EXPECTED_POINTERS


def test_pointers_track_present_keys_only():
    # A minimal config yields no org/provider secret pointers.
    payload = company_config_to_dict(CompanyConfig.model_validate({"name": "Empty"}))
    assert secret_pointers(payload) == []


def test_slack_channel_is_not_secret():
    pointers = secret_pointers(normalized_payload())
    assert not any(p.endswith("/channel") for p in pointers)


def test_legacy_role_level_slack_block_is_still_masked():
    """A role-level ``slack`` block is no longer an authorable field —
    ``RoleConfig`` forbids it — but revisions stored before that change
    can still carry one, and ``config export --redact`` / ``config diff``
    read those payloads structurally without re-validating them.  The
    registry must keep covering the shape so an old revision's bot token
    is never displayed in the clear.
    """
    legacy = {
        "name": "Acme",
        "roles": [
            {
                "name": "Dev",
                "slack": {"bot_token": "xoxb-old", "signing_secret": "sign-old"},
            }
        ],
    }
    assert set(secret_pointers(legacy)) == {
        "/roles/0/slack/bot_token",
        "/roles/0/slack/signing_secret",
    }


# ── JSON-pointer escaping ─────────────────────────────────────────────


def test_pointer_escaping_for_special_server_names():
    payload = {
        "name": "X",
        "roles": [{"name": "R", "mcp_env": {"a/b~c": {"TOKEN": "v"}}}],
    }
    (pointer,) = secret_pointers(payload)
    assert pointer == "/roles/0/mcp_env/a~1b~0c/TOKEN"
    assert pointer_get(payload, pointer) == "v"


def test_pointer_set_round_trips():
    payload = {"name": "X", "integrations": {"jira": {"url": "u", "token": "t"}}}
    pointer_set(payload, "/integrations/jira/token", "new")
    assert pointer_get(payload, "/integrations/jira/token") == "new"


# ── free-form / org-wide secret surfaces ──────────────────────────────


def test_mcp_server_secrets_are_masked():
    # Shared MCP-server env/header credentials must be masked; non-secret
    # config (hosts, ports, URLs, flags) stays visible.
    payload = {
        "name": "X",
        "mcp_servers": [
            {
                "name": "atlassian",
                "url": "https://mcp.example.com",
                "env": {"CONFLUENCE_URL": "https://x", "API_TOKEN": "tok-secret"},
                "headers": {"Authorization": "Bearer abc", "X-Trace": "on"},
            },
        ],
    }
    pointers = set(secret_pointers(payload))
    assert pointers == {
        "/mcp_servers/0/env/API_TOKEN",
        "/mcp_servers/0/headers/Authorization",
    }
    redacted = redact_payload(payload)
    for secret in ("tok-secret", "Bearer abc"):
        assert secret not in str(redacted)
    # Non-secret structural values stay visible.
    assert redacted["mcp_servers"][0]["url"] == "https://mcp.example.com"
    assert redacted["mcp_servers"][0]["env"]["CONFLUENCE_URL"] == "https://x"
    assert redacted["mcp_servers"][0]["headers"]["X-Trace"] == "on"


# ── redaction (display) ───────────────────────────────────────────────


def test_redact_masks_every_secret_with_structured_marker():
    redacted = redact_payload(normalized_payload())
    # Every secret leaf is masked — no plaintext value survives.
    for pointer in EXPECTED_POINTERS:
        assert pointer_get(redacted, pointer) == {"encrypted": True, "key_id": None}
    for plaintext in ("sk-a", "sk-emb", "jira-tok", "xoxb-cto", "ghp_ceo", "plane-tok"):
        assert plaintext not in str(redacted)
    # non-secret fields survive redaction
    assert redacted["integrations"]["jira"]["url"] == "https://x.atlassian.net"
    assert redacted["providers"]["llm"]["default"]["model"] == "gpt-4o"
    slack = redacted["units"][0]["roles"][0]["integrations"]["slack"]
    assert slack["channel"] == "C123"


def test_redact_does_not_mutate_input():
    payload = normalized_payload()
    redact_payload(payload)
    # original secret untouched
    assert payload["integrations"]["jira"]["token"] == "jira-tok"


def test_redact_skips_empty_secret():
    payload = {
        "name": "X",
        "integrations": {"github": {"enabled": False, "webhook_secret": ""}},
    }
    redacted = redact_payload(payload)
    assert redacted["integrations"]["github"]["webhook_secret"] == ""


def test_redact_leaves_var_references_visible():
    # A ${VAR} reference is an inert env-var pointer, not a secret at rest —
    # it stays visible (masking it would falsely mark it "encrypted" and
    # hide the useful var name). Embedded refs ("Bearer ${X}") too.
    payload = {
        "name": "X",
        "providers": {"embeddings": {"type": "openai", "api_key": "${EMB_KEY}"}},
        "roles": [
            {"name": "R", "mcp_env": {"gh": {"Authorization": "Bearer ${GH_TOKEN}"}}}
        ],
    }
    redacted = redact_payload(payload)
    assert redacted["providers"]["embeddings"]["api_key"] == "${EMB_KEY}"
    assert (
        redacted["roles"][0]["mcp_env"]["gh"]["Authorization"] == "Bearer ${GH_TOKEN}"
    )
    # A literal secret in the same shape is still masked.
    payload["providers"]["embeddings"]["api_key"] = "sk-literal"
    assert redact_payload(payload)["providers"]["embeddings"]["api_key"] == {
        "encrypted": True,
        "key_id": None,
    }


def test_plane_var_references_stay_visible():
    # The authored form of a plane block keeps its ${VAR} secrets as inert
    # env-var pointers — never masked, never treated as secrets-at-rest.
    payload = {
        "name": "X",
        "integrations": {
            "plane": {
                "enabled": True,
                "url": "https://plane.example",
                "workspace": "acme",
                "webhook_secret": "${PLANE_WEBHOOK_SECRET}",
                "token": "${PLANE_ENGINE_TOKEN}",
            },
        },
    }
    redacted = redact_payload(payload)
    plane = redacted["integrations"]["plane"]
    assert plane["webhook_secret"] == "${PLANE_WEBHOOK_SECRET}"
    assert plane["token"] == "${PLANE_ENGINE_TOKEN}"
    # A literal value in the same shape is masked.
    payload["integrations"]["plane"]["token"] = "plane_api_literal"
    assert redact_payload(payload)["integrations"]["plane"]["token"] == {
        "encrypted": True,
        "key_id": None,
    }


# ── redaction round-trip (restore_redacted) ───────────────────────────


def test_is_redaction_marker():
    assert is_redaction_marker({"encrypted": True, "key_id": "k1"})
    assert is_redaction_marker({"encrypted": True, "key_id": None})
    assert not is_redaction_marker({"encrypted": False, "key_id": "k1"})
    assert not is_redaction_marker("enc:v1:k1:blah")
    assert not is_redaction_marker({"encrypted": True})  # no key_id


def test_redact_then_restore_round_trips():
    stored = normalized_payload()
    redacted = redact_payload(stored)
    # A client edits a non-secret field on the redacted document...
    redacted["mission"] = "new mission"
    # ...and writes it back: markers restore to the stored plaintext.
    restored = restore_redacted(redacted, stored)
    assert restored["mission"] == "new mission"
    # Every secret is byte-identical to what was stored — kept, not
    # clobbered by the marker.
    for pointer in EXPECTED_POINTERS:
        assert pointer_get(restored, pointer) == pointer_get(stored, pointer)
    assert restored["providers"]["llm"]["default"]["api_keys"] == ["sk-a", "sk-b"]


def test_restore_redacted_raises_when_no_stored_value():
    # A marker with no counterpart in the old payload cannot be "kept".
    payload = {
        "name": "X",
        "providers": {"embeddings": {"api_key": {"encrypted": True, "key_id": "k1"}}},
    }
    with pytest.raises(SecretLeakError):
        restore_redacted(payload, {"name": "X"})


def test_restore_redacted_ignores_real_values():
    # Non-marker secret values (real value / ${VAR}) pass through.
    payload = {"name": "X", "integrations": {"jira": {"url": "u", "token": "${T}"}}}
    assert restore_redacted(payload, {}) == payload


# ---------------------------------------------------------------------------
# Leak regressions
# ---------------------------------------------------------------------------


def test_literal_secret_containing_brace_syntax_is_masked():
    """Redaction skips values holding a ``${VAR}`` reference, so "is a
    reference" must use the ENGINE's grammar. A looser ``\\$\\{[^}]+\\}``
    also matched braces the resolver ignores (``${line#host=}``), leaving
    a literal secret that contains them displayed in full."""
    payload = {
        "name": "X",
        "integrations": {"jira": {"token": "pat-${line#host=}-literal"}},
    }
    redacted = redact_payload(payload)
    assert is_redaction_marker(redacted["integrations"]["jira"]["token"])


def test_genuine_env_reference_still_left_visible():
    """A real ``${VAR}`` pointer is not a secret at rest — masking it
    would hide the useful var name and falsely mark it encrypted."""
    payload = {
        "name": "X",
        "integrations": {
            "jira": {"token": "${JIRA_TOKEN}"},
            "gitlab": {"token": "${GITLAB_ENGINE_TOKEN}"},
        },
    }
    redacted = redact_payload(payload)
    assert redacted["integrations"]["jira"]["token"] == "${JIRA_TOKEN}"
    assert redacted["integrations"]["gitlab"]["token"] == "${GITLAB_ENGINE_TOKEN}"


def test_embedded_env_reference_left_visible():
    """``Bearer ${TOKEN}`` still resolves at runtime, so it is a pointer."""
    payload = {"name": "X", "integrations": {"jira": {"token": "Bearer ${T}"}}}
    assert redact_payload(payload)["integrations"]["jira"]["token"] == "Bearer ${T}"


def test_reference_with_a_leading_digit_is_masked():
    """``${1}`` is not a name the resolver substitutes, so a secret
    containing it is a literal and must be masked."""
    payload = {"name": "X", "integrations": {"jira": {"token": "pat-${1}-literal"}}}
    assert is_redaction_marker(redact_payload(payload)["integrations"]["jira"]["token"])


# ── cli-agent credentials ──────────────────────────────────────────


CLI_AGENT_PAYLOAD = {
    "providers": {
        "llm": {
            "default": {
                "type": "cli-agent",
                "api_keys": ["sk-metered"],
                "cli": {
                    "agent": "claude",
                    "auth": {
                        "mode": "subscription",
                        "token": "sk-ant-oat01-EXAMPLE",
                        "credential_bundle": "H4sIAAAABUNDLE",
                    },
                    "env": {
                        "VENDOR_API_KEY": "literal-key",
                        "VENDOR_REGION": "eu-west-1",
                    },
                    "overrides": {"env": {"OTHER_TOKEN": "literal-2"}},
                },
            }
        }
    }
}


def test_cli_agent_credentials_are_masked():
    """`${VAR}` is a convention on these fields, not a constraint.

    An operator who pastes the token from `crewlet llm login
    --capture-token --print-token` gets a literal, and nothing pointed
    at it — so it rendered in the clear through `GET /config`, the
    dashboard's config view and `config export --redact`, while the
    `api_keys` entry beside it was masked. The bundle is the worse of
    the two: it is the entire CLI login archive.
    """
    redacted = redact_payload(CLI_AGENT_PAYLOAD)
    cli = redacted["providers"]["llm"]["default"]["cli"]

    assert cli["auth"]["token"] != "sk-ant-oat01-EXAMPLE"
    assert cli["auth"]["credential_bundle"] != "H4sIAAAABUNDLE"
    assert cli["env"]["VENDOR_API_KEY"] != "literal-key"
    assert cli["overrides"]["env"]["OTHER_TOKEN"] != "literal-2"


def test_cli_agent_non_secret_env_stays_readable():
    """`cli.env` mixes config with credentials, like `mcp_servers[].env`.

    Masking it wholesale would hide the region, the model endpoint and
    every other setting an operator reads the config view to check.
    """
    redacted = redact_payload(CLI_AGENT_PAYLOAD)
    cli = redacted["providers"]["llm"]["default"]["cli"]
    assert cli["env"]["VENDOR_REGION"] == "eu-west-1"
    assert cli["agent"] == "claude"
    assert cli["auth"]["mode"] == "subscription"


def test_cli_agent_credentials_survive_a_redacted_write_back():
    """The dashboard writes back what it was shown.

    A masked field the restore does not know about would be written
    back as its mask, destroying the credential.
    """
    redacted = redact_payload(CLI_AGENT_PAYLOAD)
    restored = restore_redacted(redacted, CLI_AGENT_PAYLOAD)
    cli = restored["providers"]["llm"]["default"]["cli"]

    assert cli["auth"]["token"] == "sk-ant-oat01-EXAMPLE"
    assert cli["auth"]["credential_bundle"] == "H4sIAAAABUNDLE"
    assert cli["env"]["VENDOR_API_KEY"] == "literal-key"
    assert cli["overrides"]["env"]["OTHER_TOKEN"] == "literal-2"


def test_a_free_form_secret_survives_a_redacted_write_back():
    """The corruption this module's docstring warns about, on every
    untyped surface it covers.

    `restore_redacted` walks the pointers of the REDACTED payload,
    where each masked value is a marker dict. The free-form emitter
    only emitted a pointer for a `str`, so it emitted nothing there:
    the restore had no pointer to swap back and the marker was stored
    AS the credential. One dashboard save — editing any unrelated
    field — replaced every shared MCP server's API key with
    `{"encrypted": true, "key_id": null}`.
    """
    stored = {
        "mcp_servers": [
            {
                "name": "tavily",
                "env": {"TAVILY_API_KEY": "sk-real", "TAVILY_HOST": "api.example"},
                "headers": {"Authorization": "Bearer sk-hdr"},
            }
        ]
    }
    redacted = redact_payload(stored)
    assert is_redaction_marker(redacted["mcp_servers"][0]["env"]["TAVILY_API_KEY"])

    # The client edits something unrelated and PUTs the document back.
    edited = copy.deepcopy(redacted)
    edited["mcp_servers"][0]["name"] = "tavily-eu"
    restored = restore_redacted(edited, stored)

    server = restored["mcp_servers"][0]
    assert server["env"]["TAVILY_API_KEY"] == "sk-real"
    assert server["headers"]["Authorization"] == "Bearer sk-hdr"
    assert server["env"]["TAVILY_HOST"] == "api.example"
    assert server["name"] == "tavily-eu"
