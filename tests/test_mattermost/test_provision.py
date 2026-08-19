"""Tests for the Mattermost seat provisioner."""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.config import CompanyConfig, config_to_organization
from crewlet.mattermost.client import MattermostError
from crewlet.mattermost.provision import (
    TOKEN_DESCRIPTION,
    MattermostProvisionAborted,
    decommission,
    provision,
    seat_token_vars,
)

TEAM_ID = "teamid00000000000000000000"


class FakeSink:
    """TokenSink capturing what the reconcile minted."""

    def __init__(self, preset: dict[str, str] | None = None) -> None:
        self.values: dict[str, str] = dict(preset or {})
        self.recorded: list[tuple[str, str]] = []
        self.discarded: list[str] = []
        self.flushed = False

    def existing(self, var: str) -> str:
        return self.values.get(var, "")

    async def record(self, var: str, token: str) -> None:
        self.values[var] = token
        self.recorded.append((var, token))

    async def discard(self, var: str) -> None:
        self.values.pop(var, None)
        self.discarded.append(var)

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
        self.disabled: list[str] = []
        self.patched: list[tuple[str, dict[str, Any]]] = []
        self.team_adds: list[str] = []
        self.channel_adds: list[tuple[str, str]] = []
        self.tokens_created: list[str] = []
        self.revoked: list[str] = []
        self._next_token = 0
        #: Tokens the bot already holds, per user id.
        self.existing_tokens: dict[str, list[dict[str, Any]]] = {}
        #: Server settings the preflight reads.
        self.service_settings: dict[str, Any] = {
            "EnableBotAccountCreation": True,
            "EnableUserAccessTokens": True,
            "SiteURL": "https://chat.example",
        }
        self.base_url = "https://chat.example"

    async def me(self) -> dict[str, Any]:
        return {"id": "adminid", "roles": self._roles}

    async def get_team_by_name(self, name: str) -> dict[str, Any] | None:
        return {"id": TEAM_ID, "name": name} if self._team_found else None

    async def server_limits(self) -> dict[str, Any]:
        return {"maxUsersLimit": 250, "activeUserCount": 12}

    async def server_config(self) -> dict[str, Any]:
        return {"ServiceSettings": dict(self.service_settings)}

    async def site_url(self) -> str:
        return str(self.service_settings.get("SiteURL") or "")

    async def get_team_member(
        self, team_id: str, user_id: str
    ) -> dict[str, Any] | None:
        return {"user_id": user_id} if user_id in self.team_adds else None

    async def get_channel_member(
        self, channel_id: str, user_id: str
    ) -> dict[str, Any] | None:
        return (
            {"user_id": user_id} if (channel_id, user_id) in self.channel_adds else None
        )

    async def list_user_access_tokens(self, user_id: str) -> list[dict[str, Any]]:
        return list(self.existing_tokens.get(user_id, []))

    async def revoke_user_access_token(self, token_id: str) -> None:
        self.revoked.append(token_id)

    async def disable_bot(self, bot_user_id: str) -> dict[str, Any]:
        self.disabled.append(bot_user_id)
        return {"user_id": bot_user_id}

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
        client.existing_tokens["user-engineer"] = [
            {"id": "t1", "description": TOKEN_DESCRIPTION, "is_active": True}
        ]
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
    async def test_a_missing_channel_fails_the_seat(self):
        """Channel membership is what makes the websocket deliver
        anything, so a configured channel that does not exist is a seat
        that will be deaf where the operator expects it to listen — not a
        note under a clean, zero-exit report."""
        client = FakeClient(channels={})
        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}", channel="nope"),
            team="nimbus",
            sink=FakeSink(),
        )
        assert not report.ok
        assert "does not exist" in (report.seats[0].error or "")

    @pytest.mark.asyncio
    async def test_a_revoked_token_is_reminted(self):
        """`--decommission` revokes the token but leaves the ${VAR}
        holding its dead value, so the documented recovery — "re-enabled
        by a later provision run" — used to restore a bot that could not
        authenticate, silently."""
        client = FakeClient(
            bots=[
                {
                    "username": "engineer",
                    "user_id": "user-engineer",
                    "display_name": "Engineer",
                    "delete_at": 1700000000000,
                }
            ]
        )
        client.existing_tokens["user-engineer"] = []
        sink = FakeSink({"MM_TOKEN_ENGINEER": "revoked-value"})

        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}"),
            team="nimbus",
            sink=sink,
        )

        assert report.ok
        assert report.seats[0].bot_action == "re-enabled"
        assert report.seats[0].token_action == "minted"
        assert sink.values["MM_TOKEN_ENGINEER"] != "revoked-value"

    @pytest.mark.asyncio
    async def test_an_inactive_token_does_not_count_as_provisioned(self):
        client = FakeClient()
        client.existing_tokens["user-engineer"] = [
            {"id": "t1", "description": TOKEN_DESCRIPTION, "is_active": False}
        ]
        sink = FakeSink({"MM_TOKEN_ENGINEER": "stale"})

        report = await provision(
            client, _org(bot_token="${MM_TOKEN_ENGINEER}"), team="nimbus", sink=sink
        )

        assert report.seats[0].token_action == "minted"

    @pytest.mark.asyncio
    async def test_a_token_that_cannot_be_persisted_is_revoked(self):
        """A minted token whose value is lost is a live, unknown,
        non-expiring credential on the account — and the next run mints
        another one indistinguishable from it."""

        class _FailingSink(FakeSink):
            async def record(self, var: str, token: str) -> None:
                raise OSError("read-only file system")

        client = FakeClient()
        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}"),
            team="nimbus",
            sink=_FailingSink(),
        )

        assert not report.ok
        assert client.revoked == ["tok-id-1"]

    @pytest.mark.asyncio
    async def test_a_membership_failure_is_verified_before_it_is_believed(self):
        """Mattermost answers an add for an existing member with success,
        so a 4xx is a real failure — reading it as "already joined"
        reported bots into channels they could not hear."""

        class _RefusingClient(FakeClient):
            async def add_channel_member(self, channel_id, user_id):
                raise MattermostError("archived channel", status=400)

        client = _RefusingClient(channels={"engineering": "chan-eng"})
        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}"),
            team="nimbus",
            sink=FakeSink(),
            default_channels=["engineering"],
        )

        assert not report.ok
        assert "engineering" in (report.seats[0].error or "")


def _org_two_vars() -> Any:
    """A seat whose two consumers name DIFFERENT vars.

    Legal and occasionally deliberate — the transport identity and the
    MCP server are separate readers — and it is the shape that makes a
    partial write observable.
    """
    cfg = CompanyConfig.model_validate(
        {
            "name": "Acme",
            "roles": [
                {
                    "name": "Engineer",
                    "handle": "engineer",
                    "integrations": {"mattermost": {"bot_token": "${MM_IDENTITY}"}},
                    "mcp_env": {"mattermost": {"MATTERMOST_TOKEN": "${MM_MCP}"}},
                }
            ],
        }
    )
    return config_to_organization(cfg)


class TestTokenMintIsAllOrNothing:
    """One credential, several ${VAR}s, persisted one at a time. Whatever
    happens, a seat must never end up with some vars naming a token and
    others naming a different — or dead — one: the engine cannot tell
    them apart, so the failure is a socket that never opens."""

    @pytest.mark.asyncio
    async def test_a_mint_lands_in_every_var_not_only_the_empty_ones(self):
        sink = FakeSink({"MM_IDENTITY": "older-token"})
        client = FakeClient()

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.seats[0].token_action == "minted"
        assert sink.values["MM_IDENTITY"] == sink.values["MM_MCP"]
        assert sink.values["MM_IDENTITY"] != "older-token"

    @pytest.mark.asyncio
    async def test_the_superseded_token_is_revoked(self):
        sink = FakeSink({"MM_IDENTITY": "older-token"})
        client = FakeClient()
        client.existing_tokens["user-engineer"] = [
            {"id": "tok-old", "description": "crewlet-engine", "is_active": True}
        ]

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert client.revoked == ["tok-old"]
        assert any("revoked 1 superseded" in n for n in report.seats[0].notes)

    @pytest.mark.asyncio
    async def test_a_foreign_token_is_never_revoked(self):
        """Only tokens carrying this tool's description are ours."""
        sink = FakeSink({"MM_IDENTITY": "older-token"})
        client = FakeClient()
        client.existing_tokens["user-engineer"] = [
            {"id": "tok-theirs", "description": "someone-else", "is_active": True}
        ]

        await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert client.revoked == []

    @pytest.mark.asyncio
    async def test_a_partial_write_leaves_no_var_holding_a_dead_token(self):
        """The sharp one: the rollback revokes, so any var already
        written carries a value that no longer authenticates — and
        nothing downstream can tell that from a good one."""

        class _HalfFailingSink(FakeSink):
            async def record(self, var: str, token: str) -> None:
                if self.recorded:
                    raise OSError("read-only file system")
                await super().record(var, token)

        sink = _HalfFailingSink()
        client = FakeClient()

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert not report.ok
        assert client.revoked == ["tok-id-1"]
        assert len(sink.recorded) == 1  # one var was written before the failure
        assert sink.discarded == [sink.recorded[0][0]]
        assert sink.values == {}  # ...and nothing is left claiming a value

    @pytest.mark.asyncio
    async def test_a_failed_discard_does_not_stop_the_revoke(self):
        """A live orphaned credential is the worse of the two to leave."""

        class _StubbornSink(FakeSink):
            async def record(self, var: str, token: str) -> None:
                if self.recorded:
                    raise OSError("read-only file system")
                await super().record(var, token)

            async def discard(self, var: str) -> None:
                raise OSError("read-only file system")

        client = FakeClient()
        report = await provision(
            client, _org_two_vars(), team="nimbus", sink=_StubbornSink()
        )

        assert not report.ok
        assert client.revoked == ["tok-id-1"]

    @pytest.mark.asyncio
    async def test_an_unreadable_token_list_neither_mints_nor_revokes(self):
        """Unknowable is not absent: minting would strand the credential
        the config carries, and revoking would tear down a working seat."""

        class _BlindClient(FakeClient):
            async def list_user_access_tokens(self, user_id: str) -> list[Any]:
                raise MattermostError("forbidden", status=403)

        sink = FakeSink({"MM_IDENTITY": "live", "MM_MCP": "live"})
        client = _BlindClient()

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.seats[0].token_action == "exists"
        assert client.tokens_created == []
        assert client.revoked == []
        assert sink.values == {"MM_IDENTITY": "live", "MM_MCP": "live"}


class TestPreflightRequirements:
    @pytest.mark.asyncio
    async def test_disabled_access_tokens_abort_before_any_write(self):
        """Both settings default to false on a fresh install and fail
        LATE: every bot gets created and joined, and only then does the
        mint fail — the half-provisioned fleet preflight exists for."""
        client = FakeClient()
        client.service_settings["EnableUserAccessTokens"] = False

        with pytest.raises(MattermostProvisionAborted, match="Personal Access"):
            await provision(
                client,
                _org(bot_token="${MM_TOKEN_ENGINEER}"),
                team="nimbus",
                sink=FakeSink(),
            )
        assert client.created_bots == []

    @pytest.mark.asyncio
    async def test_disabled_bot_creation_aborts(self):
        client = FakeClient()
        client.service_settings["EnableBotAccountCreation"] = False

        with pytest.raises(MattermostProvisionAborted, match="Bot Accounts"):
            await provision(
                client,
                _org(bot_token="${MM_TOKEN_ENGINEER}"),
                team="nimbus",
                sink=FakeSink(),
            )

    @pytest.mark.asyncio
    async def test_a_loopback_site_url_aborts(self):
        """This is the only command that runs against the server with a
        system-admin token, and it runs first — so it is where a Site URL
        that will blind every browser has to be caught."""
        client = FakeClient()
        client.service_settings["SiteURL"] = "http://localhost:8065"

        with pytest.raises(MattermostProvisionAborted, match="SiteURL"):
            await provision(
                client,
                _org(bot_token="${MM_TOKEN_ENGINEER}"),
                team="nimbus",
                sink=FakeSink(),
            )

    @pytest.mark.asyncio
    async def test_another_address_is_a_note_not_an_abort(self):
        """An operator may legitimately reach the server by a second
        name."""
        client = FakeClient()
        client.service_settings["SiteURL"] = "https://chat.internal"

        report = await provision(
            client,
            _org(bot_token="${MM_TOKEN_ENGINEER}"),
            team="nimbus",
            sink=FakeSink(),
        )

        assert report.ok
        assert any("SiteURL" in n for n in report.notes)


class TestDecommission:
    @pytest.mark.asyncio
    async def test_dry_run_changes_nothing(self):
        """`--dry-run` used to be checked AFTER this ran, so rehearsing a
        decommission with the documented safety flag irreversibly revoked
        every token and disabled every bot."""
        client = FakeClient(bots=[{"username": "engineer", "user_id": "user-engineer"}])
        client.existing_tokens["user-engineer"] = [{"id": "t1"}]

        outcomes = await decommission(client, ["engineer"], dry_run=True)

        assert client.disabled == []
        assert client.revoked == []
        assert "would disable" in outcomes[0][1]

    @pytest.mark.asyncio
    async def test_an_explicit_username_is_still_found(self):
        """Provisioning honoured `username:`; decommissioning did not, so
        those seats reported `absent` while the bot stayed enabled."""
        client = FakeClient(bots=[{"username": "custom-bot", "user_id": "user-custom"}])
        org = _org(bot_token="${MM_TOKEN_ENGINEER}", username="custom-bot")

        outcomes = await decommission(client, ["engineer"], org=org)

        assert outcomes == [("engineer", "disabled")]
        assert client.disabled == ["user-custom"]

    @pytest.mark.asyncio
    async def test_the_bot_is_disabled_before_its_tokens_are_revoked(self):
        """Deactivating the account is the fast kill; revoking first meant
        one dead token could abandon the handle with the bot still live."""

        class _PartialClient(FakeClient):
            async def revoke_user_access_token(self, token_id: str) -> None:
                if token_id == "t1":
                    raise MattermostError("already inactive", status=400)
                await super().revoke_user_access_token(token_id)

        client = _PartialClient(
            bots=[{"username": "engineer", "user_id": "user-engineer"}]
        )
        client.existing_tokens["user-engineer"] = [{"id": "t1"}, {"id": "t2"}]

        outcomes = await decommission(client, ["engineer"])

        assert client.disabled == ["user-engineer"]
        assert client.revoked == ["t2"]
        assert "still active" in outcomes[0][1]

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


class TestSeatScanPrecision:
    def test_only_credential_keys_are_minted_into(self):
        """`mcp_env.mattermost` legitimately carries a URL; minting the
        bot's token into it would overwrite an operator's value with a
        secret, in the env file, silently."""
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
                        "mcp_env": {
                            "mattermost": {
                                "MATTERMOST_TOKEN": "${MM_TOKEN_ENGINEER}",
                                "MATTERMOST_URL": "${MATTERMOST_URL}",
                            }
                        },
                    }
                ],
            }
        )
        org = config_to_organization(cfg)
        role = next(iter(org.all_roles()))

        assert seat_token_vars(role) == ["MM_TOKEN_ENGINEER"]


class TestHandleFilter:
    @pytest.mark.asyncio
    async def test_an_unknown_handle_aborts_rather_than_doing_nothing(self):
        """Provisioning nothing looks identical to a clean run, and the
        typo survives into the next one."""
        client = FakeClient()
        with pytest.raises(MattermostProvisionAborted, match="not in this config"):
            await provision(
                client,
                _org(bot_token="${MM_TOKEN_ENGINEER}"),
                team="nimbus",
                sink=FakeSink(),
                handles={"nobody"},
            )
