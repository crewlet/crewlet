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
        #: Recorded values the server REJECTS (401) when used.
        self.dead_tokens: set[str] = set()
        #: Recorded values that authenticate as somebody else.
        self.foreign_tokens: dict[str, str] = {}
        #: When set, every verification probe fails with this — the
        #: "cannot tell" answer, distinct from "the token is bad".
        self.probe_error: MattermostError | None = None
        #: Who a recorded value authenticates as by default — the bot the
        #: seat resolved to, i.e. "the recorded token is this seat's".
        self.probe_user_id: str = ""
        #: Server settings the preflight reads.
        self.service_settings: dict[str, Any] = {
            "EnableBotAccountCreation": True,
            "EnableUserAccessTokens": True,
            "SiteURL": "https://chat.example",
        }
        self.base_url = "https://chat.example"

    async def me(self) -> dict[str, Any]:
        return {"id": "adminid", "roles": self._roles}

    def with_token(self, token: str) -> Any:
        """Stand-in for the real client's verification seam."""
        return _TokenProbe(self, token)

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
        # Remember who is being asked about, so a verification probe with
        # no explicit script answers "yes, this is that bot's token".
        self.probe_user_id = self.probe_user_id or user_id
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
        self.probe_user_id = user_id
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
        self.probe_user_id = user_id
        return {
            "id": f"tok-id-{self._next_token}",
            "token": f"minted-{self._next_token}",
        }


class _TokenProbe:
    """What `client.with_token(value).me()` answers in tests."""

    def __init__(self, parent: FakeClient, token: str) -> None:
        self._parent = parent
        self._token = token
        self.closed = False

    async def me(self) -> dict[str, Any]:
        if self._parent.probe_error is not None:
            raise self._parent.probe_error
        if self._token in self._parent.dead_tokens:
            raise MattermostError(
                "Invalid or expired session", method="GET", path="/users/me", status=401
            )
        other = self._parent.foreign_tokens.get(self._token)
        return {"id": other or self._parent.probe_user_id}

    async def close(self) -> None:
        self.closed = True


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
        client.dead_tokens = {"revoked-value"}
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
        client.dead_tokens = {"stale"}
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


#: A bot account that already existed before the run.
_EXISTING_BOT = {
    "username": "engineer",
    "user_id": "existing-id",
    "display_name": "Engineer",
}


class _BlindClient(FakeClient):
    """Answers everything except the token list, which it refuses."""

    async def list_user_access_tokens(self, user_id: str) -> list[Any]:
        raise MattermostError("forbidden", status=403)


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
    async def test_a_superseded_token_that_cannot_be_revoked_is_named(self):
        """It is live, unreferenced, and carries the same description as
        the good one — so the report is the only thing that can point at
        it. The mint itself succeeded, so the seat is not failed."""

        class _StuckRevoke(FakeClient):
            async def revoke_user_access_token(self, token_id: str) -> None:
                raise MattermostError("gateway timeout", status=504)

        sink = FakeSink({"MM_IDENTITY": "older-token"})  # MM_MCP empty -> mint
        client = _StuckRevoke(bots=[_EXISTING_BOT])
        client.probe_user_id = "existing-id"
        client.existing_tokens["existing-id"] = [
            {"id": "tok-old", "description": "crewlet-engine", "is_active": True}
        ]

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.seats[0].token_action == "minted"
        assert any(
            "could not revoke the superseded token tok-old" in n
            for n in report.seats[0].notes
        )

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
    async def test_a_working_recorded_token_settles_it_without_the_list(self):
        """The recorded VALUE is the thing that matters. Proving it works
        answers the question the token listing was only ever a proxy
        for — so an unreadable list is no longer a reason to do anything."""
        sink = FakeSink({"MM_IDENTITY": "live", "MM_MCP": "live"})
        client = _BlindClient()

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.ok
        assert report.seats[0].token_action == "exists"
        assert client.tokens_created == []
        assert client.revoked == []
        assert sink.values == {"MM_IDENTITY": "live", "MM_MCP": "live"}

    @pytest.mark.asyncio
    async def test_a_live_token_is_not_proof_the_recorded_one_works(self):
        """The two questions come apart exactly where it hurts: a run
        whose mint reached the server but whose response was lost leaves
        a live token the sink never saw. Reading THAT as proof the
        recorded value is good reports `exists` on a seat holding a dead
        credential — on every run after, since nothing revisits it."""
        sink = FakeSink({"MM_IDENTITY": "orphaned-run", "MM_MCP": "orphaned-run"})
        client = FakeClient(bots=[_EXISTING_BOT])
        client.probe_user_id = "existing-id"
        client.dead_tokens = {"orphaned-run"}
        client.existing_tokens["existing-id"] = [
            {"id": "tok-live", "description": "crewlet-engine", "is_active": True}
        ]

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.seats[0].token_action == "minted"
        assert sink.values["MM_IDENTITY"] not in ("", "orphaned-run")
        assert client.revoked == ["tok-live"]
        assert any("no longer authenticates" in n for n in report.seats[0].notes)

    @pytest.mark.asyncio
    async def test_a_token_that_belongs_to_another_account_is_replaced(self):
        sink = FakeSink({"MM_IDENTITY": "someone-elses", "MM_MCP": "someone-elses"})
        client = FakeClient(bots=[_EXISTING_BOT])
        client.probe_user_id = "existing-id"
        client.foreign_tokens = {"someone-elses": "a-different-account"}

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.seats[0].token_action == "minted"
        assert any(
            "authenticates as a-different-account" in n for n in report.seats[0].notes
        )

    @pytest.mark.asyncio
    async def test_an_unverifiable_token_is_left_alone(self):
        """ "Cannot tell" is not "is bad": re-minting on a 5xx would
        destroy a credential that works."""
        sink = FakeSink({"MM_IDENTITY": "live", "MM_MCP": "live"})
        client = FakeClient(bots=[_EXISTING_BOT])
        client.probe_error = MattermostError("bad gateway", status=502)

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.ok
        assert report.seats[0].token_action == "exists"
        assert client.tokens_created == []
        assert client.revoked == []
        assert any("could not verify" in n for n in report.seats[0].notes)

    @pytest.mark.asyncio
    async def test_vars_holding_different_values_are_replaced_with_one(self):
        sink = FakeSink({"MM_IDENTITY": "one-token", "MM_MCP": "another-token"})
        client = FakeClient(bots=[_EXISTING_BOT])
        client.probe_user_id = "existing-id"

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.seats[0].token_action == "minted"
        assert sink.values["MM_IDENTITY"] == sink.values["MM_MCP"]
        assert any("hold different values" in n for n in report.seats[0].notes)

    @pytest.mark.asyncio
    async def test_a_mint_that_fails_after_the_server_created_it_is_reclaimed(self):
        """create_user_access_token can fail AFTER the server minted the
        token — a read timeout on the response — and the value is then
        live on the account and readable by nobody, forever."""

        class _LosesTheResponse(FakeClient):
            async def create_user_access_token(self, user_id, description):
                self.existing_tokens.setdefault(user_id, []).append(
                    {"id": "tok-orphan", "description": description, "is_active": True}
                )
                raise MattermostError("transport error: ReadTimeout", status=0)

        client = _LosesTheResponse()
        report = await provision(
            client, _org_two_vars(), team="nimbus", sink=FakeSink()
        )

        assert not report.ok
        assert client.revoked == ["tok-orphan"]
        assert any(
            "value was never readable, so it was revoked" in n
            for n in report.seats[0].notes
        )

    @pytest.mark.asyncio
    async def test_an_unreadable_list_refuses_to_mint_onto_an_existing_bot(self):
        """A mint that cannot enumerate what is already there cannot
        revoke what it supersedes, so it leaves a live, non-expiring
        token referenced by no ${VAR} — carrying the same description as
        the good one, invisible to `doctor`, and never revisited, because
        the next run finds every var populated and returns early."""
        sink = FakeSink({"MM_IDENTITY": "live"})  # MM_MCP is empty
        client = _BlindClient(bots=[_EXISTING_BOT])

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert not report.ok
        assert client.tokens_created == []
        assert client.revoked == []
        assert sink.values == {"MM_IDENTITY": "live"}  # nothing was touched
        seat = report.seats[0]
        assert seat.token_action == "skipped"
        assert "forbidden" in seat.error  # the underlying cause, not a shrug
        assert "Refusing to mint one blind" in seat.error

    @pytest.mark.asyncio
    async def test_the_refused_seat_resumes_on_the_next_run(self):
        """A refusal must cost one re-run, not a hand-repaired install."""
        sink = FakeSink({"MM_IDENTITY": "live"})
        blind = await provision(
            _BlindClient(bots=[_EXISTING_BOT]),
            _org_two_vars(),
            team="nimbus",
            sink=sink,
        )
        assert not blind.ok

        client = FakeClient(bots=[_EXISTING_BOT])
        client.existing_tokens["existing-id"] = [
            {"id": "tok-old", "description": "crewlet-engine", "is_active": True}
        ]
        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.ok
        assert report.seats[0].token_action == "minted"
        assert sink.values["MM_IDENTITY"] == sink.values["MM_MCP"]
        assert client.revoked == ["tok-old"]

    @pytest.mark.asyncio
    async def test_an_unreadable_list_still_mints_for_a_bot_this_run_created(self):
        """The one exemption. A just-created account's token list is
        empty by construction, so nothing can be stranded — and refusing
        here would abort a first-ever provision over a hazard that
        cannot exist."""
        sink = FakeSink()
        client = _BlindClient()  # no bots → _ensure_bot creates one

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.ok
        seat = report.seats[0]
        assert seat.bot_action == "created"
        assert seat.token_action == "minted"
        assert sink.values["MM_IDENTITY"] == sink.values["MM_MCP"]
        assert client.revoked == []
        assert any("this run created the account" in n for n in seat.notes)

    @pytest.mark.asyncio
    async def test_surplus_tokens_on_a_provisioned_seat_are_named(self):
        """A fully provisioned seat returns before the supersede loop, so
        surplus tokens of ours survive every future run. The provisioner
        can see them; staying quiet is how an operator never learns."""
        sink = FakeSink({"MM_IDENTITY": "live", "MM_MCP": "live"})
        client = FakeClient(bots=[_EXISTING_BOT])
        client.existing_tokens["existing-id"] = [
            {"id": "tok-a", "description": "crewlet-engine", "is_active": True},
            {"id": "tok-b", "description": "crewlet-engine", "is_active": True},
        ]

        report = await provision(client, _org_two_vars(), team="nimbus", sink=sink)

        assert report.seats[0].token_action == "exists"
        assert client.revoked == []  # naming them is not the same as guessing
        assert any("2 crewlet-engine tokens" in n for n in report.seats[0].notes)

    @pytest.mark.asyncio
    async def test_a_rollback_that_cannot_revoke_names_the_live_token(self):
        """Neither persisted nor revoked is the worst state there is: a
        live credential whose id exists nowhere but a log line."""

        class _StuckClient(FakeClient):
            async def revoke_user_access_token(self, token_id: str) -> None:
                raise MattermostError("gateway timeout", status=504)

        class _FailingSink(FakeSink):
            async def record(self, var: str, token: str) -> None:
                raise OSError("read-only file system")

        report = await provision(
            _StuckClient(), _org_two_vars(), team="nimbus", sink=_FailingSink()
        )

        assert not report.ok
        assert any(
            "tok-id-1" in n and "could not be persisted OR revoked" in n
            for n in report.seats[0].notes
        )


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


class TestTheAdminVarIsNeverMintedInto:
    """A mint overwrites every var it lands in, populated or not. A
    config pointing a seat's credential key at the OPERATOR's var would
    replace a system-admin token with a bot's — in the same .env the
    bootstrap writes both into."""

    @staticmethod
    def _org_naming_the_admin_var() -> Any:
        cfg = CompanyConfig.model_validate(
            {
                "name": "Acme",
                "roles": [
                    {
                        "name": "Engineer",
                        "handle": "engineer",
                        "integrations": {"mattermost": {"bot_token": "${MM_SWE}"}},
                        "mcp_env": {
                            "mattermost": {
                                "MATTERMOST_TOKEN": "${MATTERMOST_ADMIN_TOKEN}"
                            }
                        },
                    }
                ],
            }
        )
        return config_to_organization(cfg)

    def test_the_scan_excludes_it(self):
        org = self._org_naming_the_admin_var()
        assert seat_token_vars(org.all_roles()[0]) == ["MM_SWE"]

    @pytest.mark.asyncio
    async def test_the_operators_token_survives_a_mint(self):
        sink = FakeSink({"MATTERMOST_ADMIN_TOKEN": "the-operators-admin-pat"})
        client = FakeClient()

        report = await provision(
            client, self._org_naming_the_admin_var(), team="nimbus", sink=sink
        )

        assert report.ok
        assert sink.values["MATTERMOST_ADMIN_TOKEN"] == "the-operators-admin-pat"
        assert sink.values["MM_SWE"] != "the-operators-admin-pat"


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
