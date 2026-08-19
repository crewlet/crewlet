"""Tests for ``crewlet mattermost doctor``.

The command exists for the failures that produce no error anywhere, so
what these assert is that each of those is *named*, in terms an operator
can act on, rather than merely reflected in an exit code.
"""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.mattermost import doctor as doctor_mod
from crewlet.mattermost.client import MattermostError
from crewlet.mattermost.doctor import DoctorReport, SeatCheck, format_report, run_doctor

BOT_ID = "botuserid00000000000000000"


class _FakeClient:
    """Stands in for MattermostClient across the whole doctor run."""

    #: Per-token scripted behaviour, set by each test.
    scripts: dict[str, dict[str, Any]] = {}
    #: What the anonymous (tokenless) client reports.
    anon: dict[str, Any] = {}

    def __init__(self, base_url: str, token: str, **_: Any) -> None:
        self.base_url = base_url
        self.token = token
        self.closed = False

    @property
    def _script(self) -> dict[str, Any]:
        return self.scripts.get(self.token, {}) if self.token else self.anon

    async def ping(self) -> dict[str, Any]:
        if self._script.get("unreachable"):
            raise MattermostError(
                "connection refused", method="GET", path="/system/ping"
            )
        return {"status": "OK", "version": "10.5.1"}

    async def client_config(self) -> dict[str, Any]:
        return {"SiteURL": self._script.get("site_url", "https://chat.example")}

    async def me(self) -> dict[str, Any]:
        if self._script.get("token_bad"):
            raise MattermostError("Invalid session", method="GET", path="/users/me")
        return {
            "id": BOT_ID,
            "username": self._script.get("username", "agent-swe"),
            "is_bot": True,
            "delete_at": self._script.get("delete_at", 0),
        }

    async def get_team_by_name(self, name: str) -> dict[str, Any] | None:
        return None if self._script.get("no_team") else {"id": "team1", "name": name}

    async def list_channels_for_user(
        self, user_id: str, team_id: str
    ) -> list[dict[str, Any]]:
        return [{"name": n} for n in self._script.get("channels", ["engineering"])]

    async def close(self) -> None:
        self.closed = True


@pytest.fixture
def fake_mattermost(monkeypatch):
    """Swap the client and both websocket probes for scripted stand-ins."""
    _FakeClient.scripts = {"tok": {}}
    _FakeClient.anon = {}
    monkeypatch.setattr(doctor_mod, "MattermostClient", _FakeClient)

    state = {"browser_ws": (True, "upgraded"), "seat_ws": ""}

    async def _browser(base_url: str) -> tuple[bool, str]:
        return state["browser_ws"]

    async def _seat(base_url: str, token: str, handle: str) -> str:
        return state["seat_ws"]

    monkeypatch.setattr(doctor_mod, "_probe_browser_websocket", _browser)
    monkeypatch.setattr(doctor_mod, "_probe_seat_websocket", _seat)
    return state


async def _run(**kwargs: Any) -> DoctorReport:
    kwargs.setdefault("url", "https://chat.example")
    kwargs.setdefault("team", "nimbus")
    kwargs.setdefault("seat_tokens", {"engineer": "tok"})
    return await run_doctor(**kwargs)


class TestHealthyInstall:
    @pytest.mark.asyncio
    async def test_everything_green(self, fake_mattermost):
        report = await _run()
        assert report.ok
        assert report.reachable
        assert report.site_url_ok
        assert report.browser_websocket_ok
        assert report.team_ok
        assert [s.handle for s in report.seats] == ["engineer"]
        assert report.seats[0].websocket_ok
        assert "problems      : none" in format_report(report)


class TestSiteURL:
    @pytest.mark.asyncio
    async def test_a_mismatch_is_a_problem_that_explains_itself(self, fake_mattermost):
        """This is the failure with no error message anywhere: REST works,
        the engine works, and only the humans' browsers go blind."""
        _FakeClient.anon = {"site_url": "http://localhost:8065"}

        report = await _run()

        assert not report.ok
        assert not report.site_url_ok
        problem = " ".join(report.problems)
        assert "http://localhost:8065" in problem
        assert "Origin" in problem
        assert "refresh" in problem
        assert "MATTERMOST_PUBLIC_URL" in problem

    @pytest.mark.asyncio
    async def test_a_trailing_slash_is_not_a_mismatch(self, fake_mattermost):
        _FakeClient.anon = {"site_url": "https://chat.example/"}
        report = await _run()
        assert report.site_url_ok

    @pytest.mark.asyncio
    async def test_an_unreported_site_url_is_flagged(self, fake_mattermost):
        _FakeClient.anon = {"site_url": ""}
        report = await _run()
        assert not report.ok
        assert any("did not report a SiteURL" in p for p in report.problems)


class TestReachability:
    @pytest.mark.asyncio
    async def test_an_unreachable_server_stops_the_run(self, fake_mattermost):
        """No point probing seats against a server nobody can reach."""
        _FakeClient.anon = {"unreachable": True}

        report = await _run()

        assert not report.ok
        assert not report.reachable
        assert report.seats == []
        assert any("cannot reach" in p for p in report.problems)


class TestBrowserWebsocket:
    @pytest.mark.asyncio
    async def test_a_failed_upgrade_is_reported_as_the_live_feed(self, fake_mattermost):
        fake_mattermost["browser_ws"] = (False, "InvalidStatus: HTTP 403")

        report = await _run()

        assert not report.ok
        assert any("live feed is dead" in p for p in report.problems)
        assert "403" in format_report(report)


class TestSeats:
    @pytest.mark.asyncio
    async def test_a_revoked_token_names_the_remedy(self, fake_mattermost):
        _FakeClient.scripts = {"tok": {"token_bad": True}}

        report = await _run()

        assert not report.ok
        seat = report.seats[0]
        assert not seat.token_ok
        assert any("provision" in p for p in seat.problems)

    @pytest.mark.asyncio
    async def test_a_seat_in_no_channel_is_a_problem(self, fake_mattermost):
        """A bot only receives messages from channels it has joined, so
        this one would look alive and hear nothing but DMs."""
        _FakeClient.scripts = {"tok": {"channels": []}}

        report = await _run()

        assert not report.ok
        assert any("no channel" in p for p in report.seats[0].problems)

    @pytest.mark.asyncio
    async def test_a_disabled_account_is_a_problem(self, fake_mattermost):
        _FakeClient.scripts = {"tok": {"delete_at": 1700000000000}}
        report = await _run()
        assert any("disabled" in p for p in report.seats[0].problems)

    @pytest.mark.asyncio
    async def test_a_failed_websocket_auth_is_a_problem(self, fake_mattermost):
        """The check that separates "the token is valid" from "this seat
        will actually hear anything" — the engine's only inbound path."""
        fake_mattermost["seat_ws"] = "websocket authentication rejected"

        report = await _run()

        assert not report.ok
        assert not report.seats[0].websocket_ok
        assert any("websocket authentication" in p for p in report.seats[0].problems)

    @pytest.mark.asyncio
    async def test_an_unresolved_token_var_is_named(self, fake_mattermost):
        report = await _run(seat_tokens={"engineer": ""})
        assert not report.ok
        assert any("${VAR}" in p for p in report.seats[0].problems)

    @pytest.mark.asyncio
    async def test_no_seats_at_all_is_a_problem(self, fake_mattermost):
        report = await _run(seat_tokens={})
        assert not report.ok
        assert any("no Mattermost-enabled agent seats" in p for p in report.problems)


class TestTeam:
    @pytest.mark.asyncio
    async def test_a_missing_team_is_a_problem(self, fake_mattermost):
        _FakeClient.scripts = {"tok": {"no_team": True}}

        report = await _run()

        assert not report.ok
        assert not report.team_ok
        assert any("team" in p for p in report.problems)


class TestFormatting:
    def test_the_report_renders_without_a_server(self):
        report = DoctorReport(url="https://chat.example", team="nimbus")
        report.seats.append(SeatCheck(handle="engineer", problems=["nope"]))
        rendered = format_report(report)
        assert "https://chat.example" in rendered
        assert "engineer" in rendered
        assert "nope" in rendered


class TestWithoutTheWebsocketsExtra:
    """A base install cannot run the integration at all — the engine's
    only inbound path is a websocket per seat. That is a *finding* for
    this command, not a crash: the doctor exists to name what is wrong,
    and a bare ModuleNotFoundError would abandon every other check that
    still has an answer."""

    @pytest.fixture
    def without_websockets(self, monkeypatch, fake_mattermost):
        monkeypatch.setattr(doctor_mod, "_websockets_module", lambda: None)
        return fake_mattermost

    @pytest.mark.asyncio
    async def test_the_run_completes_and_names_the_missing_extra(
        self, without_websockets
    ):
        report = await _run()

        assert not report.ok
        assert any("crewlet[mattermost]" in p for p in report.problems)
        # Everything that does not need a socket was still checked.
        assert report.reachable
        assert report.site_url_ok
        assert report.team_ok
        assert report.seats[0].token_ok
        assert report.seats[0].channels == ["engineering"]

    @pytest.mark.asyncio
    async def test_an_unrun_probe_is_not_reported_as_a_failure(
        self, without_websockets
    ):
        report = await _run()

        assert not report.browser_websocket_checked
        assert not report.seats[0].websocket_checked
        # The seat itself is fine; only the install is not.
        assert report.seats[0].problems == []
        rendered = format_report(report)
        assert "browser ws    : ?" in rendered
        assert "FAILED" not in rendered

    @pytest.mark.asyncio
    async def test_the_missing_extra_is_reported_once_not_per_seat(
        self, without_websockets
    ):
        report = await _run(seat_tokens={"engineer": "tok", "designer": "tok"})

        assert sum("crewlet[mattermost]" in p for p in report.problems) == 1
        assert all(seat.problems == [] for seat in report.seats)

    @pytest.mark.asyncio
    async def test_the_probes_themselves_degrade_rather_than_raise(self, monkeypatch):
        """Called directly — nothing above stops someone reusing them."""
        monkeypatch.setattr(doctor_mod, "_websockets_module", lambda: None)

        ok, detail = await doctor_mod._probe_browser_websocket("https://chat.example")
        assert ok is False
        assert "crewlet[mattermost]" in detail
        assert "crewlet[mattermost]" in await doctor_mod._probe_seat_websocket(
            "https://chat.example", "tok", "engineer"
        )


class TestUnrunChecksAreNotFailures:
    """The diff added a third state (`?`) whose whole point is that an
    unrun probe must not read as one that was tried and refused. Every
    early return skipped it, so an unprovisioned seat and an unreachable
    server both rendered FAILED for sockets nobody opened."""

    @pytest.mark.asyncio
    async def test_a_seat_with_no_token_does_not_claim_a_failed_socket(
        self, fake_mattermost
    ):
        report = await _run(seat_tokens={"engineer": ""})

        seat = report.seats[0]
        assert not seat.websocket_checked
        assert "no bot token resolved" in seat.problems[0]
        # TOKEN is legitimately FAILED — there is no token. WS is not:
        # nothing was ever dialled.
        row = [ln for ln in format_report(report).splitlines() if "engineer" in ln][0]
        assert row.split()[-2:] == ["?", "-"]

    @pytest.mark.asyncio
    async def test_a_rejected_token_does_not_claim_a_failed_socket(
        self, fake_mattermost
    ):
        _FakeClient.scripts = {"tok": {"token_bad": True}}
        report = await _run()

        seat = report.seats[0]
        assert not seat.token_ok
        assert not seat.websocket_checked
        assert "not usable" in seat.problems[0]

    @pytest.mark.asyncio
    async def test_an_unreachable_server_checks_nothing_and_says_so(
        self, fake_mattermost
    ):
        _FakeClient.anon = {"unreachable": True}
        report = await _run()

        assert not report.reachable
        assert not report.site_url_checked
        assert not report.browser_websocket_checked
        assert not report.team_checked
        rendered = format_report(report)
        # Three faults that were never observed used to be reported here.
        assert "site url      : (not checked)" in rendered
        assert "browser ws    : ?" in rendered
        assert "<< does not match url" not in rendered
        assert "<< not found" not in rendered
        assert "cannot reach" in rendered
