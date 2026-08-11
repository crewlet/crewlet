"""Tests for the Plane webhook transport, parser fallback, and prompt."""

from __future__ import annotations

import pytest
import pytest_asyncio

from crewlet.agent.pool import AgentPool
from crewlet.config import PlaneConfig
from crewlet.notifications.handle import HandleRegistry
from crewlet.notifications.notification_prompts import get_prompt
from crewlet.notifications.notification_prompts.plane import PlaneNotificationPrompt
from crewlet.notifications.protocol import InboundNotification
from crewlet.notifications.transports.plane import (
    PlaneTransport,
    _subscriber_ids,
    extract_mention_ids,
    parse_plane_webhook,
)
from crewlet.org.models import Organization, OrgUnit, Role
from crewlet.plane.client import PlaneClient
from crewlet.queue.memory import MemoryEventQueue

ACTOR = "aaaaaaaa-0000-0000-0000-00000000000a"
SWE = "11111111-1111-1111-1111-111111111111"
LEAD = "22222222-2222-2222-2222-222222222222"
OUTSIDER = "99999999-9999-9999-9999-999999999999"
PROJECT = "facefeed-0000-0000-0000-000000000001"
ISSUE = "beefbeef-0000-0000-0000-000000000002"
COMMENT = "cafecafe-0000-0000-0000-000000000003"
PAGE = "0badf00d-0000-0000-0000-000000000004"

_CFG = PlaneConfig(
    enabled=True,
    url="https://plane.test",
    workspace="testco",
    webhook_secret="whsec",
    token="plane_api_enginetoken",
)


class FakeClient:
    """In-process stand-in for PlaneClient (subscribers + projects)."""

    def __init__(
        self,
        subscribers: list | None = None,
        projects: list | None = None,
        raise_subscribers: bool = False,
    ) -> None:
        self.subscriber_calls: list[tuple[str, str]] = []
        self.project_calls = 0
        self._subscribers = subscribers or []
        self._projects = projects or []
        self._raise = raise_subscribers

    async def list_issue_subscribers(self, project_id: str, issue_id: str) -> list:
        self.subscriber_calls.append((project_id, issue_id))
        if self._raise:
            raise RuntimeError("boom")
        return self._subscribers

    async def list_projects(self) -> list:
        self.project_calls += 1
        return self._projects

    async def close(self) -> None:
        pass


def _subscriber_row(user_id: str) -> dict:
    """One pinned member-embedded subscriber row: the row's own
    ``id`` is the subscription-row UUID, never a user id."""
    return {
        "id": f"0e0e0e0e-0000-0000-0000-{user_id[:12]}",
        "subscriber": user_id,
        "member": {"id": user_id, "display_name": "Someone"},
        "issue": ISSUE,
        "project": PROJECT,
    }


def _mention(user_id: str) -> str:
    return (
        f'<mention-component id="node-1" entity_identifier="{user_id}" '
        f'entity_name="user_mention"></mention-component>'
    )


def _activity(**over) -> dict:
    activity = {
        "field": None,
        "old_value": None,
        "new_value": None,
        "actor": {"id": ACTOR, "display_name": "Alice"},
        "old_identifier": None,
        "new_identifier": None,
    }
    activity.update(over)
    return activity


def _body(event: str, action: str, data: dict, activity: dict | None = None) -> dict:
    return {
        "event": event,
        "action": action,
        "webhook_id": "wh-1",
        "workspace_id": "ws-uuid",
        "workspace_slug": "testco",
        "data": data,
        "activity": activity if activity is not None else _activity(),
    }


def _issue_data(**over) -> dict:
    data = {
        "id": ISSUE,
        "name": "Fix login flow",
        "project": PROJECT,
        "sequence_id": 42,
        "created_at": "2026-07-25T10:00:00Z",
        "updated_at": "2026-07-26T10:00:00Z",
        "description_html": "<p>Login is broken</p>",
        "assignees": [],
    }
    data.update(over)
    return data


def _comment_data(comment_html: str = "", **over) -> dict:
    data = {
        "id": COMMENT,
        "issue": ISSUE,
        "project": PROJECT,
        "comment_html": comment_html,
        "updated_at": "2026-07-26T11:00:00Z",
    }
    data.update(over)
    return data


def _page_data(**over) -> dict:
    data = {
        "id": PAGE,
        "name": "Deploy Runbook",
        "project": PROJECT,
        "updated_at": "2026-07-26T12:00:00Z",
    }
    data.update(over)
    return data


@pytest_asyncio.fixture
async def registry():
    org = Organization(
        name="TestCo",
        units=[
            OrgUnit(
                name="Core",
                type="team",
                lead="Tech Lead",
                roles=[
                    Role(name="Tech Lead", email="lead@test.com"),
                    Role(name="Agent SWE", email="swe@test.com"),
                    Role(name="Actor Agent", email="actor@test.com"),
                ],
            )
        ],
    )
    queue = MemoryEventQueue()
    await queue.start()
    pool = AgentPool(queue)
    await pool.spawn_from_org(org)
    reg = HandleRegistry(pool, org_provider=lambda: org)
    reg.register_external_id("plane", LEAD, "tech-lead")
    reg.register_external_id("plane", SWE, "agent-swe")
    reg.register_external_id("plane", ACTOR, "actor-agent")
    yield reg
    await queue.stop()


def _transport(
    registry: HandleRegistry | None = None,
    leads: dict[str, str] | None = None,
    client: FakeClient | None = None,
    excluded: list[str] | None = None,
    seed: bool = True,
) -> PlaneTransport:
    t = PlaneTransport(_CFG)
    if registry is not None:
        t.set_handle_registry(registry)
    t.set_project_leads(leads if leads is not None else {"ENG": "tech-lead"})
    if excluded:
        t.set_notification_excluded_projects(excluded)
    t._client = client
    if seed:
        t._project_names[PROJECT] = "ENG"
    return t


# --- Mention extraction ---


class TestMentionExtraction:
    def test_extracts_multiple_in_order(self):
        html = f"hi {_mention(SWE)} and {_mention(LEAD)}"
        assert extract_mention_ids(html) == [SWE, LEAD]

    def test_lowercases_and_dedupes(self):
        html = f"{_mention(SWE.upper())} {_mention(SWE)}"
        assert extract_mention_ids(html) == [SWE]

    def test_empty(self):
        assert extract_mention_ids("") == []

    def test_no_entity_identifier_attribute(self):
        html = '<mention-component id="n1"></mention-component>'
        assert extract_mention_ids(html) == []


# --- issue create ---


class TestIssueCreate:
    async def test_assignees_routed(self):
        t = _transport()
        out = await t.handle_webhook(
            _body("issue", "created", _issue_data(assignees=[SWE, LEAD]))
        )
        assert len(out) == 2
        assert {n.metadata["plane_user_id"] for n in out} == {SWE, LEAD}
        assert all(n.metadata["routed_via"] == "assignee" for n in out)
        assert all(n.recipient_handle == "" for n in out)
        n = out[0]
        assert n.metadata["event_type"] == "issue.created"
        assert n.metadata["work_item_key"] == "ENG-42"
        assert n.metadata["project"] == "ENG"
        assert n.metadata["project_id"] == PROJECT
        assert n.metadata["issue_id"] == ISSUE
        assert n.metadata["actor_account_id"] == ACTOR
        assert n.metadata["workspace"] == "testco"
        assert n.subject == "[ENG-42] Fix login flow"
        assert n.body == "Login is broken"
        assert (
            n.metadata["url"]
            == f"https://plane.test/testco/projects/{PROJECT}/issues/{ISSUE}"
        )

    async def test_actor_excluded_from_assignees(self):
        t = _transport()
        out = await t.handle_webhook(
            _body("issue", "created", _issue_data(assignees=[ACTOR, SWE]))
        )
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]

    async def test_self_assigned_create_returns_empty(self):
        t = _transport()
        out = await t.handle_webhook(
            _body("issue", "created", _issue_data(assignees=[ACTOR]))
        )
        assert out == []

    async def test_no_assignees_routes_to_lead(self):
        t = _transport()
        out = await t.handle_webhook(_body("issue", "created", _issue_data()))
        assert len(out) == 1
        n = out[0]
        assert n.recipient_handle == "tech-lead"
        assert n.metadata["routed_via"] == "project_lead_fallback"
        assert "plane_user_id" not in n.metadata

    async def test_unresolvable_project_identifier_returns_empty(self):
        t = _transport(seed=False)
        out = await t.handle_webhook(_body("issue", "created", _issue_data()))
        assert out == []

    async def test_project_id_fk_shape(self):
        data = _issue_data(assignees=[SWE])
        del data["project"]
        data["project_id"] = PROJECT
        t = _transport()
        out = await t.handle_webhook(_body("issue", "created", data))
        assert out[0].metadata["project_id"] == PROJECT
        assert out[0].metadata["work_item_key"] == "ENG-42"

    async def test_expanded_project_dict_seeds_cache(self):
        data = _issue_data(
            assignees=[SWE], project={"id": PROJECT, "identifier": "ENG"}
        )
        t = _transport(seed=False)
        out = await t.handle_webhook(_body("issue", "created", data))
        assert out[0].metadata["project_id"] == PROJECT
        assert out[0].metadata["project"] == "ENG"
        assert out[0].metadata["work_item_key"] == "ENG-42"

    async def test_lead_not_notified_of_own_action(self, registry):
        t = _transport(registry=registry)
        body = _body(
            "issue",
            "created",
            _issue_data(),
            activity=_activity(actor={"id": LEAD, "display_name": "Lead"}),
        )
        assert await t.handle_webhook(body) == []


# --- assignee diff (issue update on the assignees field) ---


class TestAssigneeDiff:
    async def test_added_assignee_directed(self):
        t = _transport()
        body = _body(
            "issue",
            "updated",
            _issue_data(),
            activity=_activity(field="assignees", new_identifier=SWE),
        )
        out = await t.handle_webhook(body)
        assert len(out) == 1
        assert out[0].metadata["plane_user_id"] == SWE
        assert out[0].metadata["routed_via"] == "assignee_added"

    async def test_removal_returns_empty(self):
        t = _transport()
        body = _body(
            "issue",
            "updated",
            _issue_data(),
            activity=_activity(field="assignees", old_identifier=SWE),
        )
        assert await t.handle_webhook(body) == []

    async def test_self_assign_returns_empty(self):
        t = _transport()
        body = _body(
            "issue",
            "updated",
            _issue_data(),
            activity=_activity(field="assignees", new_identifier=ACTOR),
        )
        assert await t.handle_webhook(body) == []

    async def test_new_identifier_lowercased(self):
        t = _transport()
        body = _body(
            "issue",
            "updated",
            _issue_data(),
            activity=_activity(field="assignees", new_identifier=SWE.upper()),
        )
        out = await t.handle_webhook(body)
        assert out[0].metadata["plane_user_id"] == SWE

    async def test_self_assign_case_insensitive(self):
        t = _transport()
        body = _body(
            "issue",
            "updated",
            _issue_data(),
            activity=_activity(field="assignees", new_identifier=ACTOR.upper()),
        )
        assert await t.handle_webhook(body) == []


# --- subscriber fan-out (issue update on other fields, issue delete) ---


class TestSubscriberFanOut:
    async def test_update_fans_to_subscribers_minus_actor(self):
        client = FakeClient(
            subscribers=[
                _subscriber_row(SWE),
                _subscriber_row(LEAD),
                _subscriber_row(ACTOR),
            ]
        )
        t = _transport(client=client)
        body = _body(
            "issue", "updated", _issue_data(), activity=_activity(field="state")
        )
        out = await t.handle_webhook(body)
        assert {n.metadata["plane_user_id"] for n in out} == {SWE, LEAD}
        assert all(n.metadata["routed_via"] == "subscriber" for n in out)
        assert client.subscriber_calls == [(PROJECT, ISSUE)]

    async def test_no_client_degrades_to_assignees(self):
        t = _transport(client=None)
        body = _body(
            "issue",
            "updated",
            _issue_data(assignees=[SWE]),
            activity=_activity(field="state"),
        )
        out = await t.handle_webhook(body)
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]
        assert out[0].metadata["routed_via"] == "assignee"

    async def test_client_error_degrades_to_assignees(self):
        client = FakeClient(raise_subscribers=True)
        t = _transport(client=client)
        body = _body(
            "issue",
            "updated",
            _issue_data(assignees=[SWE]),
            activity=_activity(field="priority"),
        )
        out = await t.handle_webhook(body)
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]
        assert out[0].metadata["routed_via"] == "assignee"

    async def test_empty_subscribers_degrades_to_assignees(self):
        client = FakeClient(subscribers=[])
        t = _transport(client=client)
        body = _body(
            "issue",
            "updated",
            _issue_data(assignees=[SWE]),
            activity=_activity(field="state"),
        )
        out = await t.handle_webhook(body)
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]

    async def test_unextractable_rows_degrade_to_assignees(self):
        # A non-empty response whose rows carry no recognisable user
        # reference is a FAILED lookup — the row's own ``id`` must
        # never be used (it is the subscription row, not a user).
        client = FakeClient(subscribers=[{"id": "sub-row-uuid", "weird": True}])
        t = _transport(client=client)
        body = _body(
            "issue",
            "updated",
            _issue_data(assignees=[SWE]),
            activity=_activity(field="state"),
        )
        out = await t.handle_webhook(body)
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]
        assert out[0].metadata["routed_via"] == "assignee"

    async def test_delete_routes_straight_to_assignees(self):
        client = FakeClient(subscribers=[_subscriber_row(LEAD)])
        t = _transport(client=client)
        out = await t.handle_webhook(
            _body("issue", "deleted", _issue_data(assignees=[SWE]))
        )
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]
        assert out[0].metadata["routed_via"] == "assignee"
        # Deletion never hits the subscribers endpoint — it 404s.
        assert client.subscriber_calls == []

    def test_subscriber_ids_shapes(self):
        # Bare UUID strings, member-embedded rows, subscriber FK only.
        rows = [
            SWE.upper(),
            _subscriber_row(LEAD),
            {"id": "row-uuid", "subscriber": OUTSIDER},
        ]
        assert _subscriber_ids(rows) == [SWE, LEAD, OUTSIDER]


# --- registry intersection (outsiders never fan out) ---


class TestOutsiderIntersection:
    """All directed fan-out is intersected with ``known_external_ids``
    at the ``_directed_copies`` choke point — an ordinary workspace
    human must never produce an undeliverable copy."""

    async def test_outsider_subscriber_dropped_silently(self, registry):
        client = FakeClient(
            subscribers=[_subscriber_row(SWE), _subscriber_row(OUTSIDER)]
        )
        t = _transport(registry=registry, client=client)
        body = _body(
            "issue", "updated", _issue_data(), activity=_activity(field="state")
        )
        out = await t.handle_webhook(body)
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]
        assert out[0].metadata["routed_via"] == "subscriber"

    async def test_all_outsider_subscribers_do_not_degrade_to_assignees(self, registry):
        # The degrade decision is made on the PRE-intersection list: a
        # subscriber list that is entirely outsiders is a SUCCESSFUL
        # lookup, so the assignee fallback must not fire — nothing
        # routes.
        client = FakeClient(subscribers=[_subscriber_row(OUTSIDER)])
        t = _transport(registry=registry, client=client)
        body = _body(
            "issue",
            "updated",
            _issue_data(assignees=[SWE]),
            activity=_activity(field="state"),
        )
        assert await t.handle_webhook(body) == []

    async def test_registered_assignees_unaffected_on_create(self, registry):
        t = _transport(registry=registry)
        out = await t.handle_webhook(
            _body("issue", "created", _issue_data(assignees=[SWE, OUTSIDER]))
        )
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]
        assert out[0].metadata["routed_via"] == "assignee"
        assert all(n.recipient_handle == "" for n in out)

    async def test_all_outsider_assignees_on_create_fall_to_lead(self, registry):
        # A ticket handed entirely to humans outside the org chart must
        # still wake the owning lead rather than vanish.
        t = _transport(registry=registry)
        out = await t.handle_webhook(
            _body("issue", "created", _issue_data(assignees=[OUTSIDER]))
        )
        assert len(out) == 1
        assert out[0].recipient_handle == "tech-lead"
        assert out[0].metadata["routed_via"] == "project_lead_fallback"
        assert "plane_user_id" not in out[0].metadata

    async def test_self_assigned_create_stays_silent_with_registry(self, registry):
        # The actor claimed the work — no outsider fallback, no lead.
        t = _transport(registry=registry)
        out = await t.handle_webhook(
            _body("issue", "created", _issue_data(assignees=[ACTOR]))
        )
        assert out == []

    async def test_outsider_added_assignee_dropped(self, registry):
        t = _transport(registry=registry)
        body = _body(
            "issue",
            "updated",
            _issue_data(),
            activity=_activity(field="assignees", new_identifier=OUTSIDER),
        )
        assert await t.handle_webhook(body) == []

    async def test_outsider_delete_assignee_dropped(self, registry):
        t = _transport(registry=registry)
        out = await t.handle_webhook(
            _body("issue", "deleted", _issue_data(assignees=[SWE, OUTSIDER]))
        )
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]

    async def test_no_registry_passes_directed_targets_through(self):
        # Without a registry the choke point deliberately passes
        # subscribers/assignees through — the degraded bare transport
        # must not go silent (engine wiring always sets a registry).
        client = FakeClient(subscribers=[_subscriber_row(SWE)])
        t = _transport(client=client)
        body = _body(
            "issue", "updated", _issue_data(), activity=_activity(field="state")
        )
        out = await t.handle_webhook(body)
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]


# --- comments ---


class TestCommentHook:
    async def test_mention_wins_over_subscriber(self, registry):
        client = FakeClient(subscribers=[_subscriber_row(SWE), _subscriber_row(LEAD)])
        t = _transport(registry=registry, client=client)
        body = _body(
            "issue_comment",
            "created",
            _comment_data(f"{_mention(SWE)} please review"),
        )
        out = await t.handle_webhook(body)
        reasons = {n.metadata["plane_user_id"]: n.metadata["routed_via"] for n in out}
        # Mentioned AND subscribed → one copy, mention reason wins.
        assert reasons == {SWE: "mention", LEAD: "subscriber"}

    async def test_mentions_intersected_with_known(self, registry):
        t = _transport(registry=registry)
        body = _body(
            "issue_comment",
            "created",
            _comment_data(f"cc {_mention(SWE)} and {_mention(OUTSIDER)}"),
        )
        out = await t.handle_webhook(body)
        assert [n.metadata["plane_user_id"] for n in out] == [SWE]
        assert out[0].metadata["routed_via"] == "mention"

    async def test_body_flattened_with_party_label(self, registry):
        t = _transport(registry=registry)
        body = _body(
            "issue_comment",
            "created",
            _comment_data(f"{_mention(SWE)} please review <b>this</b> &amp; that"),
        )
        out = await t.handle_webhook(body)
        assert out[0].body == "@Agent SWE please review this & that"

    async def test_issue_id_is_not_comment_id(self, registry):
        t = _transport(registry=registry)
        body = _body("issue_comment", "created", _comment_data(f"{_mention(SWE)} hi"))
        out = await t.handle_webhook(body)
        meta = out[0].metadata
        assert meta["issue_id"] == ISSUE
        assert meta["comment_id"] == COMMENT
        assert meta["issue_id"] != meta["comment_id"]
        # The shareable URL points at the WORK ITEM, not the comment row.
        assert (
            meta["url"]
            == f"https://plane.test/testco/projects/{PROJECT}/issues/{ISSUE}"
        )

    async def test_actor_never_self_notified(self, registry):
        t = _transport(registry=registry)
        body = _body(
            "issue_comment",
            "created",
            _comment_data(f"note to self {_mention(ACTOR)}"),
        )
        assert await t.handle_webhook(body) == []

    async def test_comment_delete_returns_empty(self, registry):
        client = FakeClient(subscribers=[_subscriber_row(SWE)])
        t = _transport(registry=registry, client=client)
        body = _body("issue_comment", "deleted", _comment_data())
        assert await t.handle_webhook(body) == []
        assert client.subscriber_calls == []

    async def test_no_registry_routes_no_mentions(self):
        # Mentions are intersected with the registered Plane
        # UUIDs unconditionally — with no registry the known set is
        # empty, so mentions fail CLOSED (even for a party that would
        # be registered under normal wiring).
        t = _transport()  # registry=None
        body = _body(
            "issue_comment",
            "created",
            _comment_data(f"{_mention(SWE)} please review"),
        )
        assert await t.handle_webhook(body) == []


# --- intake ---


class TestIntake:
    def _intake_data(self, **over) -> dict:
        data = {
            "id": "1n7ake00-0000-0000-0000-000000000005",
            "issue": ISSUE,
            "project": PROJECT,
            "name": "Support request",
            "created_at": "2026-07-26T13:00:00Z",
        }
        data.update(over)
        return data

    async def test_routes_to_lead_with_intake_triage(self):
        t = _transport()
        out = await t.handle_webhook(
            _body("intake_issue", "created", self._intake_data())
        )
        assert len(out) == 1
        n = out[0]
        assert n.recipient_handle == "tech-lead"
        assert n.metadata["routed_via"] == "intake_triage"
        assert n.metadata["issue_id"] == ISSUE
        assert n.metadata["event_type"] == "intake_issue.created"

    async def test_no_lead_returns_empty(self):
        t = _transport(leads={})
        assert (
            await t.handle_webhook(
                _body("intake_issue", "created", self._intake_data())
            )
            == []
        )

    async def test_missing_issue_fk_omits_pointer(self):
        # No ``data.id`` fallback: the intake ROW id in ``issue_id``
        # would produce a URL that 404s and a doomed subscriber call.
        data = self._intake_data()
        del data["issue"]
        t = _transport()
        out = await t.handle_webhook(_body("intake_issue", "created", data))
        assert len(out) == 1
        assert "issue_id" not in out[0].metadata
        assert "url" not in out[0].metadata


# --- pages ---


class TestPageEvents:
    @staticmethod
    def _recorder():
        calls: list[tuple[str, str]] = []

        async def cb(event_type: str, page_id: str) -> None:
            calls.append((event_type, page_id))

        return calls, cb

    async def test_index_callback_fires_and_lead_routed(self):
        calls, cb = self._recorder()
        t = _transport()
        t.set_index_callback(cb)
        out = await t.handle_webhook(_body("page", "created", _page_data()))
        assert calls == [("page.created", PAGE)]
        assert len(out) == 1
        n = out[0]
        assert n.recipient_handle == "tech-lead"
        assert n.metadata["routed_via"] == "page_project_lead"
        assert n.metadata["page_id"] == PAGE
        assert (
            n.metadata["url"]
            == f"https://plane.test/testco/projects/{PROJECT}/pages/{PAGE}"
        )

    async def test_update_and_delete_route_to_lead(self):
        t = _transport()
        for action in ("updated", "deleted"):
            out = await t.handle_webhook(_body("page", action, _page_data()))
            assert [n.metadata["routed_via"] for n in out] == ["page_project_lead"]

    async def test_callback_fires_for_excluded_skills_project(self):
        calls, cb = self._recorder()
        t = _transport(leads={"TS": "tech-lead"}, excluded=["TS"], seed=False)
        t._project_names[PROJECT] = "TS"
        t.set_index_callback(cb)
        out = await t.handle_webhook(_body("page", "updated", _page_data()))
        assert calls == [("page.updated", PAGE)]
        assert out == []

    async def test_callback_fires_when_actor_is_the_lead(self, registry):
        calls, cb = self._recorder()
        t = _transport(registry=registry)
        t.set_index_callback(cb)
        body = _body(
            "page",
            "created",
            _page_data(),
            activity=_activity(actor={"id": LEAD, "display_name": "Lead"}),
        )
        out = await t.handle_webhook(body)
        assert calls == [("page.created", PAGE)]
        assert out == []

    async def test_callback_exception_swallowed(self):
        async def cb(event_type: str, page_id: str) -> None:
            raise RuntimeError("index boom")

        t = _transport()
        t.set_index_callback(cb)
        out = await t.handle_webhook(_body("page", "created", _page_data()))
        assert len(out) == 1

    async def test_unresolvable_project_fails_closed(self):
        # The webhook PageSerializer carries no scalar project FK — a
        # page with no project ref must index but never route.
        calls, cb = self._recorder()
        data = _page_data()
        del data["project"]
        t = _transport()
        t.set_index_callback(cb)
        out = await t.handle_webhook(_body("page", "deleted", data))
        assert calls == [("page.deleted", PAGE)]
        assert out == []

    async def test_projects_list_shape_accepted(self):
        data = _page_data()
        del data["project"]
        data["projects"] = [PROJECT]
        t = _transport()
        out = await t.handle_webhook(_body("page", "created", data))
        assert [n.metadata["project_id"] for n in out] == [PROJECT]


# --- project cache ---


class TestProjectCache:
    async def test_project_webhook_upserts(self):
        t = _transport(seed=False)
        out = await t.handle_webhook(
            _body("project", "created", {"id": PROJECT, "identifier": "ENG"})
        )
        assert out == []
        out = await t.handle_webhook(_body("issue", "created", _issue_data()))
        assert [n.recipient_handle for n in out] == ["tech-lead"]

    async def test_project_delete_evicts(self):
        t = _transport()
        await t.handle_webhook(
            _body("project", "deleted", {"id": PROJECT, "identifier": "ENG"})
        )
        out = await t.handle_webhook(_body("issue", "created", _issue_data()))
        assert out == []

    async def test_miss_triggers_one_refetch(self):
        client = FakeClient(projects=[{"id": PROJECT, "identifier": "ENG"}])
        t = _transport(client=client, seed=False)
        out = await t.handle_webhook(_body("issue", "created", _issue_data()))
        assert client.project_calls == 1
        assert [n.recipient_handle for n in out] == ["tech-lead"]

    async def test_second_miss_within_floor_does_not_refetch(self):
        # The refetch resolves nothing (unknown project) — the floor
        # must stop a second miss from hammering list_projects().
        client = FakeClient(projects=[{"id": "other", "identifier": "OTHER"}])
        t = _transport(client=client, seed=False)
        assert await t.handle_webhook(_body("issue", "created", _issue_data())) == []
        data = _issue_data(updated_at="2026-07-26T10:00:01Z")
        assert await t.handle_webhook(_body("issue", "created", data)) == []
        assert client.project_calls == 1


# --- dedup ---


class TestDedup:
    async def test_duplicate_within_ttl_skipped(self):
        t = _transport()
        body = _body("issue", "created", _issue_data(assignees=[SWE]))
        first = await t.handle_webhook(body)
        assert len(first) == 1
        assert await t.handle_webhook(body) == []

    async def test_different_updated_at_passes(self):
        t = _transport()
        first = _body("issue", "created", _issue_data(assignees=[SWE]))
        second = _body(
            "issue",
            "created",
            _issue_data(assignees=[SWE], updated_at="2026-07-26T10:00:05Z"),
        )
        assert len(await t.handle_webhook(first)) == 1
        assert len(await t.handle_webhook(second)) == 1

    async def test_bulk_edit_activity_discriminator(self):
        # CE fires one webhook per changed field with an IDENTICAL
        # ``data`` snapshot — the activity discriminator must keep the
        # directed assignees route alive behind a state change.
        t = _transport()
        state_change = _body(
            "issue",
            "updated",
            _issue_data(assignees=[SWE]),
            activity=_activity(field="state", new_value="In Progress"),
        )
        assignee_change = _body(
            "issue",
            "updated",
            _issue_data(assignees=[SWE]),
            activity=_activity(field="assignees", new_identifier=SWE),
        )
        assert len(await t.handle_webhook(state_change)) == 1
        out = await t.handle_webhook(assignee_change)
        assert [n.metadata["routed_via"] for n in out] == ["assignee_added"]


# --- ignored events ---


class TestUnknownEvents:
    async def test_ignored_events_return_empty(self):
        t = _transport()
        for event in ("cycle", "module", "cycle_issue", "module_issue", "user"):
            assert await t.handle_webhook(_body(event, "created", {"id": "x"})) == []

    async def test_unknown_event_returns_empty(self):
        t = _transport()
        assert await t.handle_webhook(_body("milestone", "created", {"id": "x"})) == []


# --- module-level parser fallback ---


class TestParsePlaneWebhook:
    def test_returns_single_unrouted_notification(self):
        n = parse_plane_webhook(_body("issue", "created", _issue_data(assignees=[SWE])))
        assert isinstance(n, InboundNotification)
        assert n.source == "plane"
        assert n.source_event_type == "issue.created"
        assert n.recipient_handle == ""
        assert "routed_via" not in n.metadata
        assert "plane_user_id" not in n.metadata
        # No transport → no id→identifier cache and no shareable URL.
        assert n.metadata["project"] == ""
        assert "url" not in n.metadata
        assert n.metadata["issue_id"] == ISSUE
        assert n.sender == "Alice"

    def test_comment_parses(self):
        n = parse_plane_webhook(
            _body("issue_comment", "created", _comment_data("<p>hello</p>"))
        )
        assert n is not None
        assert n.metadata["issue_id"] == ISSUE
        assert n.metadata["comment_id"] == COMMENT
        assert n.body == "hello"

    def test_malformed_returns_none(self):
        assert parse_plane_webhook({}) is None
        assert parse_plane_webhook({"event": "issue", "data": "not-a-dict"}) is None
        assert parse_plane_webhook({"event": "project", "data": {"id": "x"}}) is None
        assert parse_plane_webhook({"event": "cycle", "data": {"id": "x"}}) is None


# --- prompt ---


class TestPlanePrompt:
    def _notif(self, routed_via: str = "", **meta) -> InboundNotification:
        base = {
            "event_type": "issue.updated",
            "project": "ENG",
            "actor_account_id": ACTOR,
            "actor_name": "Alice",
        }
        if routed_via:
            base["routed_via"] = routed_via
        base.update(meta)
        return InboundNotification(
            source="plane",
            source_event_type=base["event_type"],
            sender="Alice",
            subject="[ENG-42] Fix login flow",
            body="the body",
            metadata=base,
        )

    def test_registered(self):
        assert isinstance(get_prompt("plane"), PlaneNotificationPrompt)

    def test_assigned_prompt(self):
        n = self._notif(
            "assignee_added",
            event_type="issue.updated",
            issue_id=ISSUE,
            work_item_key="ENG-42",
        )
        text = get_prompt("plane").build(n)
        assert "assigned" in text.lower()
        assert "ENG-42" in text
        assert "## Get Full Context" in text

    def test_mention_prompt(self):
        n = self._notif("mention", event_type="issue_comment.created", issue_id=ISSUE)
        text = get_prompt("plane").build(n)
        assert "mentioned" in text.lower()
        assert "## Get Full Context" in text

    def test_intake_prompt(self):
        n = self._notif(
            "intake_triage", event_type="intake_issue.created", issue_id=ISSUE
        )
        text = get_prompt("plane").build(n)
        assert "triage" in text.lower()
        assert "## Why You Received This" in text
        assert "**Delegate**" in text
        assert "lookup_colleague" in text

    def test_page_prompt(self):
        n = self._notif("page_project_lead", event_type="page.updated", page_id=PAGE)
        text = get_prompt("plane").build(n)
        assert "page" in text.lower()
        assert "## Get Full Context" in text

    def test_lead_fallback_gets_why_routed_block(self):
        n = self._notif(
            "project_lead_fallback", event_type="issue.created", issue_id=ISSUE
        )
        text = get_prompt("plane").build(n)
        assert "## Why You Received This" in text
        assert "**Delegate**" in text

    def test_subscriber_gets_generic_without_why_routed(self):
        n = self._notif("subscriber", issue_id=ISSUE)
        text = get_prompt("plane").build(n)
        assert "## Why You Received This" not in text
        assert "## Evaluate Before Acting" in text

    def test_requires_recon_truth_table(self):
        prompt = get_prompt("plane")
        assert prompt.requires_recon(self._notif("assignee", issue_id=ISSUE))
        assert prompt.requires_recon(self._notif("page_project_lead", page_id=PAGE))
        assert not prompt.requires_recon(self._notif("subscriber"))

    def test_conversation_key_is_the_uuid(self):
        prompt = get_prompt("plane")
        # UUID first — ``ENG-42`` is display-only, or comments and
        # updates on one work item would coalesce into two partitions.
        assert (
            prompt.conversation_key({"issue_id": ISSUE, "work_item_key": "ENG-42"})
            == ISSUE
        )
        assert prompt.conversation_key({"page_id": PAGE}) == PAGE
        assert prompt.conversation_key({}) == ""

    def test_digest_entry_body_keeps_comments_only(self):
        prompt = get_prompt("plane")
        assert prompt.digest_entry_body("issue_comment.created", "hi") == "hi"
        assert prompt.digest_entry_body("issue_comment.updated", "hi") == "hi"
        assert prompt.digest_entry_body("issue.updated", "stale description") == ""
        assert prompt.digest_entry_body("issue.created", "description") == ""

    def test_why_routed_block_scope(self):
        block = PlaneNotificationPrompt._why_routed_block
        assert block({"routed_via": "project_lead_fallback", "project": "ENG"})
        assert block({"routed_via": "intake_triage", "project": "ENG"})
        assert block({"routed_via": "assignee"}) == ""
        assert block({"routed_via": "mention"}) == ""
        assert block({"routed_via": "subscriber"}) == ""
        assert block({"routed_via": "page_project_lead"}) == ""


# --- public client seam ---


class TestClientSeam:
    def test_workspace_and_base_url_accessors(self):
        t = _transport()
        assert t.base_url == "https://plane.test"
        assert t.workspace == "testco"

    def test_engine_client_exposes_the_read_client(self):
        client = FakeClient()
        t = _transport(client=client)
        assert t.engine_client is client

    async def test_engine_client_none_without_token(self):
        cfg = PlaneConfig(
            enabled=True,
            url="https://plane.test",
            workspace="testco",
            webhook_secret="whsec",
            token="",
        )
        t = PlaneTransport(cfg)
        await t.start()
        assert t.engine_client is None
        await t.stop()

    def test_new_user_client_builds_per_agent_client(self):
        t = _transport()
        c = t.new_user_client("agent-token")
        assert isinstance(c, PlaneClient)
        # Same instance + workspace as the transport, the agent's key.
        assert c._base_url == "https://plane.test"
        assert c._workspace == "testco"
        assert c._token == "agent-token"


# --- resolve_project_ids (the shared identifier→UUID primitive) ---


class _RaisingClient:
    async def list_projects(self) -> list:
        raise RuntimeError("down")


class TestResolveProjectIds:
    async def test_cache_hit_costs_no_rest(self):
        client = FakeClient()
        t = _transport(client=client)  # seeds PROJECT→ENG
        assert await t.resolve_project_ids(["ENG"]) == {"ENG": PROJECT}
        assert client.project_calls == 0

    async def test_case_insensitive_keyed_uppercased(self):
        t = _transport(client=FakeClient())
        assert await t.resolve_project_ids([" eng "]) == {"ENG": PROJECT}

    async def test_miss_refetches_once_and_merges(self):
        other = "facefeed-0000-0000-0000-000000000009"
        client = FakeClient(projects=[{"id": other, "identifier": "PROD"}])
        t = _transport(client=client)  # cache already holds ENG
        out = await t.resolve_project_ids(["ENG", "prod"])
        assert out == {"ENG": PROJECT, "PROD": other}
        assert client.project_calls == 1
        # Merge-only: the refresh must not evict the pre-seeded entry.
        assert t._project_names[PROJECT] == "ENG"

    async def test_partial_result_and_refetch_floor(self):
        client = FakeClient(projects=[])
        t = _transport(client=client)
        out = await t.resolve_project_ids(["ENG", "MISSING"])
        assert out == {"ENG": PROJECT}
        assert client.project_calls == 1
        # A second miss within the floor must not hammer list_projects.
        assert await t.resolve_project_ids(["MISSING"]) == {}
        assert client.project_calls == 1

    async def test_fallback_client_resolves_without_writing_shared_cache(self):
        ops_uuid = "facefeed-0000-0000-0000-00000000000c"
        fallback = FakeClient(projects=[{"id": ops_uuid, "identifier": "OPS"}])
        t = _transport(client=None, seed=False)
        out = await t.resolve_project_ids(["ops"], client=fallback)
        assert out == {"OPS": ops_uuid}
        assert fallback.project_calls == 1
        # list_projects is membership-scoped — one agent's view must
        # never poison the shared cache.
        assert t._project_names == {}

    async def test_fallback_client_fetch_failure_raises(self):
        """A failed fallback fetch must RAISE — "Plane unreachable" and
        "identifier absent" are different answers, and returning the
        cache subset conflated them (the boot-walk registry wipe)."""
        t = _transport(client=None)
        with pytest.raises(RuntimeError, match="down"):
            await t.resolve_project_ids(["ENG", "PROD"], client=_RaisingClient())

    async def test_engine_client_fetch_failure_raises_and_keeps_floor_open(self):
        """A failed engine-client refresh raises AND does not burn the
        60 s floor, so the very next resolve retries immediately — the
        compose boot race (Plane up seconds after the engine) self-heals
        instead of serving empty resolutions for a minute."""

        class FlakyClient:
            def __init__(self) -> None:
                self.calls = 0

            async def list_projects(self) -> list:
                self.calls += 1
                if self.calls == 1:
                    raise RuntimeError("connection refused (plane still booting)")
                return [{"id": PROJECT, "identifier": "ENG"}]

        flaky = FlakyClient()
        t = _transport(client=None, seed=False)
        t._client = flaky  # type: ignore[assignment]
        with pytest.raises(RuntimeError, match="still booting"):
            await t.resolve_project_ids(["ENG"])
        # Floor NOT burned by the failure — the retry fetches again.
        assert await t.resolve_project_ids(["ENG"]) == {"ENG": PROJECT}
        assert flaky.calls == 2

    async def test_successful_fetch_burns_the_floor(self):
        """Only a SUCCESSFUL refresh burns the refetch floor (the
        anti-hammer guard for genuinely absent identifiers)."""
        client = FakeClient(projects=[])
        t = _transport(client=client, seed=False)
        assert await t.resolve_project_ids(["ENG"]) == {}
        assert await t.resolve_project_ids(["ENG"]) == {}
        assert client.project_calls == 1

    async def test_no_clients_returns_cache_subset(self):
        t = _transport(client=None)
        assert await t.resolve_project_ids(["ENG", "PROD"]) == {"ENG": PROJECT}

    async def test_empty_identifiers(self):
        client = FakeClient()
        t = _transport(client=client)
        assert await t.resolve_project_ids([]) == {}
        assert await t.resolve_project_ids(["", "   "]) == {}
        assert client.project_calls == 0


class TestCachedProjectIdentifier:
    def test_hit_miss_and_case_fold(self):
        t = _transport()
        assert t.cached_project_identifier(PROJECT) == "ENG"
        assert t.cached_project_identifier(PROJECT.upper()) == "ENG"
        assert t.cached_project_identifier("unknown-uuid") == ""
        assert t.cached_project_identifier("") == ""
