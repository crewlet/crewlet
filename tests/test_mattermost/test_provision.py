"""Tests for the Mattermost seat provisioner."""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.config import CompanyConfig, config_to_organization
from crewlet.mattermost.client import MattermostError
from crewlet.mattermost.provision import (
    MattermostProvisionAborted,
    provision,
    seat_token_vars,
)

TEAM_ID = "teamid00000000000000000000"


class FakeSink:
    """TokenSink capturing what the reconcile minted."""

    def __init__(self, preset: dict[str, str] | None = None) -> None:
        self.values: dict[str, str] = dict(preset or {})
        self.recorded: list[tuple[str, str]] = []
        self.flushed = False

    def existing(self, var: str) -> str:
        return self.values.get(var, "")

    async def record(self, var: str, token: str) -> None:
        self.values[var] = token
        self.recorded.append((var, token))

    async def flush(self) -> None:
        self.flushed = True


class FakeClient:
    """Scriptable stand-in for MattermostClient."""

    def __init__(
        self,
        *,
        roles: str = "system_admin system_user",
        team_found: bool = True,
        bots: list[dict[str, Any]] | None = None,
        channels: dict[str, str] | None = None,
    ) -> None:
        self._roles = roles
        self._team_found = team_found
        self._bots = list(bots or [])
        self._channels = dict(channels or {})
        self.created_bots: list[dict[str, Any]] = []
        self.enabled: list[str] = []
        self.patched: list[tuple[str, dict[str, Any]]] = []
        self.team_adds: list[str] = []
        self.channel_adds: list[tuple[str, str]] = []
        self.tokens_created: list[str] = []
        self._next_token = 0

    async def me(self) -> dict[str, Any]:
        return {"id": "adminid", "roles": self._roles}

    async def get_team_by_name(self, name: str) -> dict[str, Any] | None:
        return {"id": TEAM_ID, "name": name} if self._team_found else None

    async def server_limits(self) -> dict[str, Any]:
        return {"maxUsersLimit": 250, "activeUserCount": 12}

    async def list_bots(self) -> list[dict[str, Any]]:
        return list(self._bots)

    async def create_bot(
        self, username: str, display_name: str, description: str = ""
    ) -> dict[str, Any]:
        user_id = f"user-{username}"
        self.created_bots.append({"username": username, "display_name": display_name})
        return {"user_id": user_id, "username": username}

    async def patch_bot(self, bot_user_id: str, **fields: Any) -> dict[str, Any]:
        self.patched.append((bot_user_id, fields))
        return {"user_id": bot_user_id}

    async def enable_bot(self, bot_user_id: str) -> dict[str, Any]:
        self.enabled.append(bot_user_id)
        return {"user_id": bot_user_id}

    async def add_team_member(self, team_id: str, user_id: str) -> dict[str, Any]:
        self.team_adds.append(user_id)
        return {"user_id": user_id}

    async def get_channel_by_name(
        self, team_id: str, name: str
    ) -> dict[str, Any] | None:
        channel_id = self._channels.get(name)
        return {"id": channel_id, "name": name} if channel_id else None

    async def add_channel_member(self, channel_id: str, user_id: str) -> dict[str, Any]:
        self.channel_adds.append((channel_id, user_id))
        return {"user_id": user_id}

    async def create_user_access_token(
        self, user_id: str, description: str
    ) -> dict[str, Any]:
        self._next_token += 1
        self.tokens_created.append(user_id)
        return {
            "id": f"tok-id-{self._next_token}",
            "token": f"minted-{self._next_token}",
        }


def _org(**identity: Any) -> Any:
    cfg = CompanyConfig.model_validate(
        {
            "name": "Acme",
            "roles": [
                {
                    "name": "Engineer",
                    "handle": "engineer",
                    "integrations": {"mattermost": identity or {}},
                    "mcp_env": {
                        "mattermost": {"MATTERMOST_TOKEN": "${MM_TOKEN_ENGINEER}"}
                    },
                }
            ],
        }
    )
    return config_to_organization(cfg)


# --- seat scan ------------------------------------------------------------


class TestSeatTokenVars:
    def test_scans_both_consumers(self):
        """One credential, two readers — the transport identity and the
        MCP server — so a seat carrying only one still provisions."""
        org = _org(bot_token="${MM_TOKEN_ENGINEER}")
        role = org.all_roles()[0]
        assert seat_token_vars(role) == ["MM_TOKEN_ENGINEER"]

    def test_dedupes_the_shared_var(self):
        org = _org(bot_token="${MM_TOKEN_ENGINEER}")
        assert len(seat_token_vars(org.all_roles()[0])) == 1

    def test_literal_credentials_yield_nothing_to_mint(self):
        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "roles": [
                    {
                        "name": "Engineer",
                        "handle": "engineer",
                        "integrations": {"mattermost": {"bot_token": "literal-tok"}},
                    }
                ],
            }
        )
        role = config_to_organization(cfg).all_roles()[0]
        assert seat_token_vars(role) == []


# --- preflight ------------------------------------------------------------


class TestPreflight:
    @pytest.mark.asyncio
    async def test_non_admin_aborts_before_mutating(self):
        client = FakeClient(roles="system_user")
        with pytest.raises(MattermostProvisionAborted, match="not a system admin"):
            await provision(
                client,
                _org(bot_token="${MM_TOKEN_ENGINEER}"),
                team="nimbus",
                sink=FakeSink(),
            )
        assert client.created_bots == []

    @pytest.mark.asyncio
    async def test_missing_team_aborts(self):
        client = FakeClient(team_found=False)
        with pytest.raises(MattermostProvisionAborted, match="not found"):
            await provision(
                client,
                _org(bot_token="${MM_TOKEN_ENGINEER}"),
                team="nimbus",
                sink=FakeSink(),
            )
        assert client.created_bots == []

    @pytest.mark.asyncio
    async def test_bad_credential_aborts(self):
        class Rejecting(FakeClient):
            async def me(self) -> dict[str, Any]:
                raise MattermostError("unauthorized", status=401)

        with pytest.raises(MattermostProvisionAborted, match="rejected"):
            await provision(
                Rejecting(),
                _org(bot_token="${MM_TOKEN_ENGINEER}"),
                team="nimbus",
                sink=FakeSink(),
            )

    @pytest.mark.asyncio
    async def test_reports_human_seat_headroom(self):
        """Bots do not consume the cap, but an operator still wants to
        know how close the humans are to it."""
        client = FakeClient()
        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}"),
            team="nimbus",
            sink=FakeSink(),
        )
        assert any("12/250" in note for note in report.notes)
        assert any("Bot accounts are excluded" in note for note in report.notes)


# --- reconcile ------------------------------------------------------------


class TestReconcile:
    @pytest.mark.asyncio
    async def test_creates_bot_and_mints_token(self):
        client = FakeClient(channels={"engineering": "chan-eng"})
        sink = FakeSink()
        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}", channel="engineering"),
            team="nimbus",
            sink=sink,
        )
        assert report.ok
        seat = report.seats[0]
        assert seat.bot_action == "created"
        assert seat.token_action == "minted"
        assert seat.team == "added"
        assert seat.channels == ["engineering"]
        assert sink.values["MM_TOKEN_ENGINEER"] == "minted-1"
        assert sink.flushed

    @pytest.mark.asyncio
    async def test_auto_derived_handles_name_the_bots(self):
        """A role that does not pin `handle:` still gets its derived one.

        `Role.handle` is EMPTY unless the config sets it explicitly;
        `get_handle()` is what derives `Agent PM` → `agent-pm`. Reading
        the field instead named every bot `{prefix}` — so the first seat
        created `agent-` and the second collided on the duplicate
        username. Every other fixture here pins a handle, which is
        exactly why that shipped unnoticed.
        """
        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "roles": [
                    # No `handle:` — derived from the name, like the
                    # Nimbus example and most real configs.
                    {
                        "name": "Agent PM",
                        "integrations": {"mattermost": {"bot_token": "${MM_TOKEN_PM}"}},
                    },
                    {
                        "name": "Agent SWE",
                        "integrations": {
                            "mattermost": {"bot_token": "${MM_TOKEN_SWE}"}
                        },
                    },
                ],
            }
        )
        client = FakeClient()
        report = await provision(
            client,
            config_to_organization(cfg),
            team="nimbus",
            sink=FakeSink(),
            username_prefix="agent-",
        )
        assert report.ok
        assert [s.handle for s in report.seats] == ["agent-pm", "agent-swe"]
        assert [b["username"] for b in client.created_bots] == [
            "agent-agent-pm",
            "agent-agent-swe",
        ]
        # ...and crucially, two DISTINCT usernames — the collision the
        # raw-field read produced.
        assert len({b["username"] for b in client.created_bots}) == 2

    @pytest.mark.asyncio
    async def test_username_prefix_is_applied(self):
        client = FakeClient()
        await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}"),
            team="nimbus",
            sink=FakeSink(),
            username_prefix="agent-",
        )
        assert client.created_bots[0]["username"] == "agent-engineer"

    @pytest.mark.asyncio
    async def test_explicit_username_wins_over_the_prefix(self):
        client = FakeClient()
        await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}", username="custom-bot"),
            team="nimbus",
            sink=FakeSink(),
            username_prefix="agent-",
        )
        assert client.created_bots[0]["username"] == "custom-bot"

    @pytest.mark.asyncio
    async def test_existing_token_is_not_reminted(self):
        """Mattermost returns a token's value once — re-minting would
        strand the live credential rather than replace it."""
        client = FakeClient()
        sink = FakeSink({"MM_TOKEN_ENGINEER": "already-there"})
        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}"),
            team="nimbus",
            sink=sink,
        )
        assert report.seats[0].token_action == "exists"
        assert client.tokens_created == []
        assert sink.values["MM_TOKEN_ENGINEER"] == "already-there"

    @pytest.mark.asyncio
    async def test_existing_bot_is_reused(self):
        client = FakeClient(
            bots=[
                {
                    "username": "engineer",
                    "user_id": "existing-id",
                    "display_name": "Engineer",
                }
            ]
        )
        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}"),
            team="nimbus",
            sink=FakeSink(),
        )
        assert report.seats[0].bot_action == "exists"
        assert client.created_bots == []

    @pytest.mark.asyncio
    async def test_disabled_bot_is_re_enabled(self):
        """A disabled bot keeps its username but cannot post — left alone
        the seat provisions 'successfully' and is silent."""
        client = FakeClient(
            bots=[
                {
                    "username": "engineer",
                    "user_id": "existing-id",
                    "display_name": "Engineer",
                    "delete_at": 1700000000000,
                }
            ]
        )
        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}"),
            team="nimbus",
            sink=FakeSink(),
        )
        assert report.seats[0].bot_action == "re-enabled"
        assert client.enabled == ["existing-id"]

    @pytest.mark.asyncio
    async def test_stale_display_name_is_updated(self):
        client = FakeClient(
            bots=[
                {
                    "username": "engineer",
                    "user_id": "existing-id",
                    "display_name": "Old Name",
                }
            ]
        )
        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}"),
            team="nimbus",
            sink=FakeSink(),
        )
        assert client.patched[0][1]["display_name"] == "Engineer"
        assert any("display name" in n for n in report.seats[0].notes)

    @pytest.mark.asyncio
    async def test_missing_channel_is_noted_not_fatal(self):
        client = FakeClient(channels={})
        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}", channel="nope"),
            team="nimbus",
            sink=FakeSink(),
        )
        assert report.ok
        assert any("not found" in n for n in report.seats[0].notes)

    @pytest.mark.asyncio
    async def test_literal_credentials_are_skipped_not_failed(self):
        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "roles": [
                    {
                        "name": "Engineer",
                        "handle": "engineer",
                        "integrations": {"mattermost": {"bot_token": "literal"}},
                    }
                ],
            }
        )
        report = await provision(
            FakeClient(),
            config_to_organization(cfg),
            team="nimbus",
            sink=FakeSink(),
        )
        assert report.seats == []
        assert report.skipped[0][0] == "engineer"
        assert report.ok

    @pytest.mark.asyncio
    async def test_one_failing_seat_does_not_stop_the_fleet(self):
        class OneBadBot(FakeClient):
            async def create_bot(self, username, display_name, description=""):
                if username == "engineer":
                    raise MattermostError("boom", status=500)
                return await super().create_bot(username, display_name, description)

        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "roles": [
                    {
                        "name": "Engineer",
                        "handle": "engineer",
                        "integrations": {
                            "mattermost": {"bot_token": "${MM_TOKEN_ENGINEER}"}
                        },
                    },
                    {
                        "name": "Designer",
                        "handle": "designer",
                        "integrations": {
                            "mattermost": {"bot_token": "${MM_TOKEN_DESIGNER}"}
                        },
                    },
                ],
            }
        )
        sink = FakeSink()
        report = await provision(
            OneBadBot(),
            config_to_organization(cfg),
            team="nimbus",
            sink=sink,
        )
        assert not report.ok
        assert len(report.failed) == 1
        assert sink.values["MM_TOKEN_DESIGNER"].startswith("minted-")

    @pytest.mark.asyncio
    async def test_handles_filter_narrows_the_run(self):
        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "roles": [
                    {
                        "name": "Engineer",
                        "handle": "engineer",
                        "integrations": {
                            "mattermost": {"bot_token": "${MM_TOKEN_ENGINEER}"}
                        },
                    },
                    {
                        "name": "Designer",
                        "handle": "designer",
                        "integrations": {
                            "mattermost": {"bot_token": "${MM_TOKEN_DESIGNER}"}
                        },
                    },
                ],
            }
        )
        report = await provision(
            FakeClient(),
            config_to_organization(cfg),
            team="nimbus",
            sink=FakeSink(),
            handles={"designer"},
        )
        assert [s.handle for s in report.seats] == ["designer"]

    @pytest.mark.asyncio
    async def test_human_seats_are_never_provisioned(self):
        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "roles": [
                    {
                        "name": "Founder",
                        "handle": "founder",
                        "kind": "human",
                        "contact": {"mattermost_user_id": "jane"},
                    }
                ],
            }
        )
        report = await provision(
            FakeClient(),
            config_to_organization(cfg),
            team="nimbus",
            sink=FakeSink(),
        )
        assert report.seats == []
