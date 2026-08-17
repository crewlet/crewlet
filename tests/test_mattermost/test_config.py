"""Tests for the Mattermost config models and their materialisation."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from crewlet.config import (
    CompanyConfig,
    MattermostConfig,
    RoleMattermostIdentity,
    config_to_organization,
)
from crewlet.notifications.typing_status import WorkingStatusMode


class TestMattermostConfig:
    def test_url_and_team_required_when_enabled(self):
        with pytest.raises(ValidationError, match="mattermost.url is required"):
            MattermostConfig(enabled=True, team="nimbus")
        with pytest.raises(ValidationError, match="mattermost.team is required"):
            MattermostConfig(enabled=True, url="https://chat.example")

    def test_disabled_needs_nothing(self):
        assert MattermostConfig().enabled is False

    def test_api_and_websocket_bases_are_derived(self):
        cfg = MattermostConfig(enabled=True, url="https://chat.example/", team="nimbus")
        assert cfg.api_base == "https://chat.example/api/v4"
        assert cfg.websocket_url == "wss://chat.example/api/v4/websocket"

    def test_plain_http_maps_to_ws(self):
        cfg = MattermostConfig(enabled=True, url="http://localhost:8065", team="nimbus")
        assert cfg.websocket_url == "ws://localhost:8065/api/v4/websocket"

    def test_typing_status_defaults_off(self):
        """Mattermost's indicator is fixed-vocabulary and costs a request
        every few seconds — unlike Slack it is opt-in."""
        assert MattermostConfig().typing_status is WorkingStatusMode.OFF

    def test_no_status_phrases_field(self):
        """The backend cannot render text, so the setting must not exist
        rather than exist and be silently ignored."""
        with pytest.raises(ValidationError):
            MattermostConfig.model_validate(
                {
                    "enabled": True,
                    "url": "https://chat.example",
                    "team": "n",
                    "status_phrases": {"plan": ["is thinking..."]},
                }
            )


class TestRoleMattermostIdentity:
    def test_accepts_a_placeholder_token(self):
        identity = RoleMattermostIdentity(bot_token="${MM_TOKEN_SWE}")
        assert identity.bot_token == "${MM_TOKEN_SWE}"

    @pytest.mark.parametrize(
        "username", ["Agent-SWE", "_swe", "swe!", "agent swe", "ÄGENT"]
    )
    def test_rejects_usernames_mattermost_would_refuse(self, username):
        """Caught at config load, on the line that authored it — not
        midway through a provisioning run that already created half the
        fleet."""
        with pytest.raises(ValidationError, match="invalid Mattermost username"):
            RoleMattermostIdentity(username=username)

    @pytest.mark.parametrize("username", ["agent-swe", "swe", "a.b_c-1", "0swe"])
    def test_accepts_valid_usernames(self, username):
        assert RoleMattermostIdentity(username=username).username == username

    def test_env_reference_username_passes_through(self):
        """Resolved later — rejecting the unresolved form would forbid
        configuring the username from the environment."""
        identity = RoleMattermostIdentity(username="${MM_USERNAME_SWE}")
        assert identity.username == "${MM_USERNAME_SWE}"


class TestMaterialisation:
    def _org(self, identity: dict) -> object:
        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "roles": [
                    {
                        "name": "Engineer",
                        "handle": "engineer",
                        "integrations": {"mattermost": identity},
                    }
                ],
            }
        )
        return config_to_organization(cfg)

    def test_identity_lands_on_the_runtime_role(self):
        org = self._org(
            {
                "bot_token": "${MM_TOKEN_ENGINEER}",
                "username": "agent-engineer",
                "channel": "engineering",
            }
        )
        role = org.all_roles()[0]
        assert role.mattermost == {
            "bot_token": "${MM_TOKEN_ENGINEER}",
            "username": "agent-engineer",
            "channel": "engineering",
        }

    def test_empty_fields_are_omitted(self):
        org = self._org({"bot_token": "${MM_TOKEN_ENGINEER}"})
        assert org.all_roles()[0].mattermost == {"bot_token": "${MM_TOKEN_ENGINEER}"}

    def test_no_fan_out_into_mcp_env(self):
        """The loader never writes one block from the other, so an
        operator who deliberately splits them keeps that split."""
        org = self._org({"bot_token": "${MM_TOKEN_ENGINEER}"})
        assert org.all_roles()[0].mcp_env.get("mattermost") is None

    def test_human_seat_cannot_hold_a_bot_identity(self):
        """Human seats are addressable, never spawned — a bot credential
        on one is dead config.  Caught where the Slack equivalent is, on
        conversion to the runtime Role."""
        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "roles": [
                    {
                        "name": "Founder",
                        "kind": "human",
                        "contact": {"mattermost_user_id": "jane"},
                        "integrations": {"mattermost": {"bot_token": "${MM_TOKEN}"}},
                    }
                ],
            }
        )
        with pytest.raises(ValidationError, match="mattermost"):
            config_to_organization(cfg)

    def test_human_contact_accepts_a_mattermost_username(self):
        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "roles": [
                    {
                        "name": "Founder",
                        "kind": "human",
                        "contact": {"mattermost_user_id": "jane"},
                    }
                ],
            }
        )
        role = config_to_organization(cfg).all_roles()[0]
        assert ("mattermost", "jane") in role.contact.resolved_identities()
