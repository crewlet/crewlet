"""Tests for the Plane provisioning reconcile."""

from __future__ import annotations

import json
import os
from datetime import UTC, datetime, timedelta

import httpx
import pytest

from crewlet.config import PlaneConfig, PlaneProvisioningConfig
from crewlet.plane.client import PlaneClient, PlaneProvisionError
from crewlet.plane.provision import (
    ENGINE_ACCOUNT_HANDLE,
    PlaneProvisionAborted,
    provision,
    seat_token_vars,
)
from crewlet.provisioning import EnvFileSink, PrintSink

_WS_UUID = "abcdabcd-0000-0000-0000-000000000001"
FOUNDER_ID = "11111111-1111-1111-1111-111111111111"
_HOOK_URL = "https://engine.test/webhooks/plane"


@pytest.fixture(autouse=True)
def _plane_env_isolation():
    """Keep PLANE_* env vars out of (and cleaned up after) every test.

    The reconcile exports the captured webhook secret into ``os.environ``
    and the sinks fall back to the environment, so leakage between tests
    would silently flip mint decisions.
    """
    saved = {k: v for k, v in os.environ.items() if k.startswith("PLANE_")}
    for key in saved:
        del os.environ[key]
    yield
    for key in [k for k in os.environ if k.startswith("PLANE_")]:
        del os.environ[key]
    os.environ.update(saved)


class _Sandbox:
    def __init__(self, env):
        self.env = env


class FakeContact:
    def __init__(self, plane_user_id=""):
        self.plane_user_id = plane_user_id


class FakeRole:
    def __init__(
        self, name, handle, mcp_env, *, kind="agent", contact=None, sandbox=None
    ):
        self.name = name
        self._handle = handle
        self.mcp_env = mcp_env
        self.kind = kind
        self.contact = contact
        self.sandbox = sandbox

    def get_handle(self) -> str:
        return self._handle


class FakeOrg:
    def __init__(self, roles):
        self._roles = roles

    def all_roles(self):
        return self._roles


class FakePlane:
    """Stateful in-memory Plane fork for the provision reconcile.

    Implements the pinned fork surface: service-account create
    (+ token lifecycle + DELETE cascade), members rows with identity
    fields, CE
    project members (duplicate add → the BaseAPIView 400 body), and
    workspace webhooks (single-show ``secret_key``).  Knobs turn
    individual capabilities off to exercise the capability preflight
    and the degraded modes.
    """

    def __init__(
        self,
        *,
        service_accounts_route: bool = True,
        token_lifecycle: bool = True,
        page_webhooks: bool = True,
        member_identity_fields: bool = True,
        admin: bool = True,
        token_valid: bool = True,
        workspace_visible: bool = True,
        honor_username: bool = True,
        webhooks_route: bool = True,
        existing_projects: tuple[str, ...] = ("ENG",),
        hidden_projects: tuple[str, ...] = (),
        force_rotate_409: bool = False,
        webhook_race: bool = False,
        webhook_create_error: bool = False,
        member_post_forbidden: bool = False,
        member_post_integrity_400: bool = False,
    ):
        self.service_accounts_route = service_accounts_route
        self.token_lifecycle = token_lifecycle
        self.page_webhooks = page_webhooks
        self.member_identity_fields = member_identity_fields
        self.admin = admin
        self.token_valid = token_valid
        self.workspace_visible = workspace_visible
        self.honor_username = honor_username
        self.webhooks_route = webhooks_route
        # Projects that EXIST (409 on create) but are invisible to the
        # operator's membership-scoped listing.
        self.hidden_projects = {p.lower() for p in hidden_projects}
        self.force_rotate_409 = force_rotate_409
        self.webhook_race = webhook_race
        self.webhook_create_error = webhook_create_error
        # Project-member POST failure knobs: a 403 models the pinned
        # ProjectAdminPermission gate (workspace admin but not project
        # admin); the integrity-400 models a NON-duplicate IntegrityError
        # (e.g. ProjectUserProperty) mapped to the same generic body.
        self.member_post_forbidden = member_post_forbidden
        self.member_post_integrity_400 = member_post_integrity_400

        self.members: list[dict] = [
            self._human_row(FOUNDER_ID, "founder", "Founding Human", 20)
        ]
        self.tokens: dict[str, list[dict]] = {}
        self.minted_tokens: list[str] = []
        self.create_bodies: list[dict] = []
        self.rotate_bodies: list[dict] = []
        self.decommissioned_usernames: set[str] = set()
        self.delete_404_uids: set[str] = set()
        self.projects: dict[str, dict] = {}
        for i, ident in enumerate(existing_projects):
            pid = f"facefeed-0000-0000-0000-{i + 1:012d}"
            self.projects[ident] = {"id": pid, "identifier": ident, "name": ident}
        self.project_members: dict[str, dict[str, dict]] = {}
        self.webhooks: list[dict] = []
        self._n = 0

    # ── helpers ──

    def _alloc(self) -> str:
        self._n += 1
        return f"aaaaaaaa-0000-0000-0000-{self._n:012d}"

    @staticmethod
    def _human_row(uid, username, display_name, role):
        return {
            "id": uid,
            "first_name": display_name,
            "last_name": "",
            "email": f"{username}@plane.test",
            "avatar": "",
            "avatar_url": None,
            "display_name": display_name,
            "role": role,
            "username": username,
            "is_bot": False,
            "bot_type": None,
        }

    @staticmethod
    def _bot_row(uid, username, display_name, role):
        return {
            "id": uid,
            "first_name": display_name,
            "last_name": "",
            "email": f"{username}@service.plane.local",
            "avatar": "",
            "avatar_url": None,
            "display_name": display_name,
            "role": role,
            "username": username,
            "is_bot": True,
            "bot_type": "SERVICE",
        }

    def uid(self, username: str) -> str:
        return next(m["id"] for m in self.members if m["username"] == username)

    def usernames(self) -> set[str]:
        return {m["username"] for m in self.members}

    def add_stale_bot(self, username: str) -> str:
        uid = self._alloc()
        self.members.append(self._bot_row(uid, username, username, 15))
        self.tokens[uid] = []
        return uid

    def add_human(self, username: str) -> str:
        uid = self._alloc()
        self.members.append(self._human_row(uid, username, username, 15))
        return uid

    def mint_extra_token(self, uid: str, label: str) -> None:
        self.tokens.setdefault(uid, []).append(
            {
                "id": self._alloc(),
                "label": label,
                "is_active": True,
                "expired_at": None,
                "value": f"plane_api_extra_{self._n}",
                "revoked": False,
            }
        )

    # ── HTTP handler ──

    def handler(self, request: httpx.Request) -> httpx.Response:
        method = request.method
        segs = [p for p in request.url.path.split("/") if p][2:]  # drop api/v1
        body = json.loads(request.content) if request.content else {}
        if not self.token_valid:
            # A bad credential fails BEFORE any permission/slug check.
            return httpx.Response(401, json={"detail": "Invalid token."})
        if segs == ["users", "me"]:
            return httpx.Response(200, json={"id": "0e0e0e0e-op", "email": "op@x"})
        if segs[:1] == ["workspaces"] and not self.workspace_visible:
            # Plane's slug-filtered permission classes reject an unknown
            # slug exactly like a non-member — 403 on EVERY route.
            return httpx.Response(403, json={"detail": "forbidden"})
        if segs[:1] != ["workspaces"] or len(segs) < 3:
            return httpx.Response(404, text="unhandled")
        rest = segs[2:]
        if rest == ["members"]:
            return httpx.Response(200, json=self._member_rows())
        if rest[0] == "service-accounts":
            return self._service_accounts(method, rest, body)
        if rest[0] == "projects":
            return self._projects_route(method, rest, body)
        if rest[0] == "webhooks":
            return self._webhooks_route(method, rest, body)
        return httpx.Response(404, text=f"unhandled {method} {'/'.join(rest)}")

    def _member_rows(self) -> list[dict]:
        if self.member_identity_fields:
            return [dict(m) for m in self.members]
        # Stock CE: UserLiteSerializer rows — no username/is_bot/bot_type.
        return [
            {k: v for k, v in m.items() if k not in ("username", "is_bot", "bot_type")}
            for m in self.members
        ]

    # ── service accounts ──

    def _service_accounts(self, method, rest, body) -> httpx.Response:
        if not self.service_accounts_route:
            return httpx.Response(404, text="Not Found")
        if not self.admin:
            return httpx.Response(403, json={"detail": "You do not have permission."})
        if len(rest) == 1:
            if method == "POST":
                return self._create_account(body)
            return httpx.Response(405, text="Method Not Allowed")
        uid = rest[1]
        if len(rest) == 2:
            if not self.token_lifecycle:
                return httpx.Response(404, text="Not Found")
            if method == "DELETE":
                return self._delete_account(uid)
            return httpx.Response(405, text="Method Not Allowed")
        if rest[2] != "tokens" or not self.token_lifecycle:
            return httpx.Response(404, text="Not Found")
        if len(rest) == 3:
            if method not in ("GET", "POST"):
                return httpx.Response(405, text="Method Not Allowed")
            if self._service_account(uid) is None:
                return httpx.Response(404, text="Not Found")
            if method == "GET":
                rows = [
                    self._token_row(t)
                    for t in self.tokens.get(uid, [])
                    if not t["revoked"]
                ]
                return httpx.Response(200, json=rows)
            return self._mint(uid, body)
        if self._service_account(uid) is None:
            return httpx.Response(404, text="Not Found")
        if len(rest) == 4 and method == "DELETE":
            return self._revoke(uid, rest[3])
        if len(rest) == 5 and rest[4] == "rotate" and method == "POST":
            return self._rotate(uid, rest[3], body)
        return httpx.Response(405, text="Method Not Allowed")

    def _service_account(self, uid) -> dict | None:
        row = next((m for m in self.members if m["id"] == uid), None)
        if row is None or not (row["is_bot"] and row["bot_type"] == "SERVICE"):
            return None
        return row

    def _create_account(self, body) -> httpx.Response:
        self.create_bodies.append(dict(body))
        requested = body.get("username")
        username = (
            requested
            if (requested and self.honor_username)
            else f"svc_{self._alloc()[-12:]}"
        )
        if username in self.usernames() or username in self.decommissioned_usernames:
            return httpx.Response(
                409,
                json={
                    "error": "A user with this username already exists.",
                    "code": "USERNAME_ALREADY_EXISTS",
                },
            )
        role_int = {"admin": 20, "member": 15, "guest": 5}[body["role"]]
        uid = self._alloc()
        display_name = body.get("display_name") or body["name"]
        self.members.append(self._bot_row(uid, username, display_name, role_int))
        value = f"plane_api_minted_{self._n}"
        self.minted_tokens.append(value)
        self.tokens[uid] = [
            {
                "id": self._alloc(),
                "label": body["name"],
                "is_active": True,
                "expired_at": None,
                "value": value,
                "revoked": False,
            }
        ]
        return httpx.Response(
            201,
            json={
                "id": uid,
                "username": username,
                "email": f"{username}@service.plane.local",
                "display_name": display_name,
                "role": role_int,
                "workspace": _WS_UUID,
                "token": value,
            },
        )

    def _delete_account(self, uid) -> httpx.Response:
        if uid in self.delete_404_uids:
            return httpx.Response(404, text="Not Found")
        row = next((m for m in self.members if m["id"] == uid), None)
        if row is None:
            return httpx.Response(404, text="Not Found")
        if not (row["is_bot"] and row["bot_type"] == "SERVICE"):
            return httpx.Response(
                400,
                json={
                    "error": "This user is not a service account.",
                    "code": "NOT_A_SERVICE_ACCOUNT",
                },
            )
        self.members.remove(row)
        self.decommissioned_usernames.add(row["username"])
        for token in self.tokens.get(uid, []):
            token["is_active"] = False
        for memberships in self.project_members.values():
            memberships.pop(uid, None)
        return httpx.Response(204)

    @staticmethod
    def _token_row(token) -> dict:
        # List rows NEVER carry the value (pinned).
        return {
            "id": token["id"],
            "label": token["label"],
            "description": "",
            "is_active": token["is_active"],
            "is_service": True,
            "user_type": 1,
            "created_at": "2026-07-27T00:00:00Z",
            "updated_at": "2026-07-27T00:00:00Z",
            "expired_at": token["expired_at"],
            "last_used": None,
        }

    def _mint(self, uid, body) -> httpx.Response:
        value = f"plane_api_minted_{self._n + 1}"
        token = {
            "id": self._alloc(),
            "label": body.get("label", ""),
            "is_active": True,
            "expired_at": body.get("expired_at"),
            "value": value,
            "revoked": False,
        }
        self.minted_tokens.append(value)
        self.tokens.setdefault(uid, []).append(token)
        return httpx.Response(
            201,
            json={
                "id": token["id"],
                "label": token["label"],
                "is_active": True,
                "created_at": "2026-07-27T00:00:00Z",
                "expired_at": token["expired_at"],
                "token": value,
            },
        )

    def _revoke(self, uid, tid) -> httpx.Response:
        token = next(
            (
                t
                for t in self.tokens.get(uid, [])
                if t["id"] == tid and not t["revoked"]
            ),
            None,
        )
        if token is None:
            return httpx.Response(404, text="Not Found")
        token["revoked"] = True
        token["is_active"] = False
        return httpx.Response(204)

    def _rotate(self, uid, tid, body) -> httpx.Response:
        self.rotate_bodies.append(dict(body))
        token = next(
            (
                t
                for t in self.tokens.get(uid, [])
                if t["id"] == tid and not t["revoked"]
            ),
            None,
        )
        if token is None:
            return httpx.Response(404, text="Not Found")
        if self.force_rotate_409 or not token["is_active"]:
            return httpx.Response(
                409,
                json={
                    "error": "The token is not active and cannot be rotated.",
                    "code": "TOKEN_NOT_ACTIVE",
                },
            )
        token["is_active"] = False
        value = f"plane_api_rotated_{self._n + 1}"
        replacement = {
            "id": self._alloc(),
            "label": token["label"],
            "is_active": True,
            "expired_at": body.get("expired_at", token["expired_at"]),
            "value": value,
            "revoked": False,
        }
        self.minted_tokens.append(value)
        self.tokens[uid].append(replacement)
        return httpx.Response(
            201,
            json={
                "id": replacement["id"],
                "label": replacement["label"],
                "is_active": True,
                "created_at": "2026-07-27T00:00:00Z",
                "expired_at": replacement["expired_at"],
                "token": value,
            },
        )

    # ── projects + members ──

    def deactivate_project_member(self, pid: str, uid: str) -> None:
        """Model the pinned detail DELETE: ``is_active=False``, row KEPT."""
        self.project_members[pid][uid]["is_active"] = False

    def _projects_route(self, method, rest, body) -> httpx.Response:
        if len(rest) == 1:
            if method == "GET":
                # Membership-scoped listing: hidden projects exist but the
                # operator cannot see them.
                return httpx.Response(
                    200,
                    json=[
                        dict(p)
                        for ident, p in self.projects.items()
                        if ident.lower() not in self.hidden_projects
                    ],
                )
            if method == "POST":
                ident = body["identifier"]
                if ident.lower() in {k.lower() for k in self.projects}:
                    return httpx.Response(
                        409, json={"error": "identifier already taken"}
                    )
                pid = self._alloc()
                self.projects[ident] = {
                    "id": pid,
                    "identifier": ident,
                    "name": body["name"],
                }
                return httpx.Response(201, json=dict(self.projects[ident]))
        pid = rest[1]
        if rest[2:3] == ["members"] and method == "POST":
            if self.member_post_forbidden:
                # Pinned ProjectAdminPermission gate: a workspace admin who
                # is not a project admin 403s on every member write.
                return httpx.Response(403, json={"detail": "forbidden"})
            if self.member_post_integrity_400:
                # A NON-duplicate IntegrityError from the same view maps to
                # the identical generic body — no membership is created.
                return httpx.Response(400, json={"error": "The payload is not valid"})
            member = body["member"]
            if member not in {m["id"] for m in self.members}:
                return httpx.Response(
                    400, json={"member": ["Member not found in workspace"]}
                )
            memberships = self.project_members.setdefault(pid, {})
            if member in memberships:
                # Pinned CE duplicate semantics: unique-constraint
                # IntegrityError → BaseAPIView's generic 400 — fires for a
                # deactivated (is_active=False) row too, since the
                # constraint is conditioned on deleted_at, not is_active.
                return httpx.Response(400, json={"error": "The payload is not valid"})
            pk = self._alloc()
            memberships[member] = {"pk": pk, "role": body["role"], "is_active": True}
            return httpx.Response(
                201, json={"id": pk, "member": member, "role": body["role"]}
            )
        if rest[2:3] == ["project-members-lite"] and method == "GET":
            rows = [
                {
                    "id": uid,
                    "display_name": uid,
                    "role": info["role"],
                    "is_active": info.get("is_active", True),
                    "is_bot": True,
                }
                for uid, info in self.project_members.get(pid, {}).items()
            ]
            return httpx.Response(200, json={"results": rows})
        return httpx.Response(404, text="unhandled project route")

    # ── webhooks ──

    def _webhooks_route(self, method, rest, body) -> httpx.Response:
        if not self.webhooks_route:
            return httpx.Response(404, text="Not Found")
        if not self.admin:
            return httpx.Response(403, json={"detail": "forbidden"})
        if len(rest) == 1:
            if method == "GET":
                return httpx.Response(200, json=[self._lite(h) for h in self.webhooks])
            if method == "POST":
                if self.webhook_create_error:
                    return httpx.Response(500, text="boom")
                if any(h["url"] == body["url"] for h in self.webhooks):
                    return httpx.Response(
                        409,
                        json={"error": "URL already exists for the workspace"},
                    )
                if self.webhook_race:
                    # Simulate a hook racing into existence between the
                    # provisioner's list and its create.
                    self.webhook_race = False
                    self.webhooks.append(self._make_hook({"url": body["url"]}))
                    return httpx.Response(
                        409,
                        json={"error": "URL already exists for the workspace"},
                    )
                hook = self._make_hook(body)
                self.webhooks.append(hook)
                return httpx.Response(201, json=dict(hook))
        pk = rest[1]
        hook = next((h for h in self.webhooks if h["id"] == pk), None)
        if hook is None:
            return httpx.Response(404, text="Not Found")
        if method == "PATCH":
            fields = {k: v for k, v in body.items() if k != "secret_key"}
            if not self.page_webhooks:
                fields.pop("page", None)
            hook.update(fields)
            return httpx.Response(200, json=self._lite(hook))
        if method == "DELETE":
            self.webhooks.remove(hook)
            return httpx.Response(204)
        return httpx.Response(405, text="Method Not Allowed")

    def _make_hook(self, body) -> dict:
        hook = {
            "id": self._alloc(),
            "url": body["url"],
            "is_active": bool(body.get("is_active", True)),
            "secret_key": f"plane_wh_secret_{self._n}",
        }
        for toggle in ("project", "issue", "module", "cycle", "issue_comment"):
            hook[toggle] = bool(body.get(toggle, False))
        if self.page_webhooks:
            hook["page"] = bool(body.get("page", False))
        # A server without the page-webhook capability silently DROPS the
        # unknown `page` field — the hook is created page=False and the
        # echo has no `page` key.
        return hook

    @staticmethod
    def _lite(hook) -> dict:
        return {k: v for k, v in hook.items() if k != "secret_key"}


def _make_client(fake: FakePlane) -> PlaneClient:
    c = PlaneClient("https://plane.test", "op-token", "testco")
    c._client = httpx.AsyncClient(
        base_url="https://plane.test/api/v1",
        headers={"X-API-Key": "op-token"},
        transport=httpx.MockTransport(fake.handler),
    )
    return c


def _config(**prov) -> PlaneConfig:
    return PlaneConfig(
        enabled=True,
        url="https://plane.test",
        workspace="testco",
        webhook_secret="${PLANE_WEBHOOK_SECRET}",
        provisioning=PlaneProvisioningConfig(**prov),
    )


def _org() -> FakeOrg:
    return FakeOrg(
        [
            FakeRole(
                "Agent SWE",
                "agent-swe",
                {"plane": {"PLANE_API_KEY": "${PLANE_TOKEN_SWE}"}},
            ),
            FakeRole(
                "Agent FE",
                "agent-fe",
                {"plane": {"PLANE_API_KEY": "${PLANE_TOKEN_FE}"}},
            ),
            FakeRole("Tech Lead", "tech-lead", {}),  # no plane creds → skipped
            FakeRole(
                "Founder",
                "founder",
                {"plane": {"x": "y"}},
                kind="human",
                contact=FakeContact(FOUNDER_ID),
            ),
        ]
    )


async def _run(fake: FakePlane, *, org=None, cfg=None, sink, **kwargs):
    kwargs.setdefault("webhook_url", _HOOK_URL)
    kwargs.setdefault("webhook_secret_ref", "${PLANE_WEBHOOK_SECRET}")
    client = _make_client(fake)
    try:
        return await provision(
            client,
            org if org is not None else _org(),
            cfg if cfg is not None else _config(projects=["ENG"]),
            sink=sink,
            **kwargs,
        )
    finally:
        await client.close()


# ---------------------------------------------------------------------------
# seat_token_vars — the Plane ${VAR} scan
# ---------------------------------------------------------------------------


def test_seat_token_vars_scans_only_the_credential_keys():
    """The block is forwarded verbatim, so most of it is config.

    Treating every reference in it as a token to mint is what made two
    seats sharing an ordinary var — a workspace slug, a base url — look
    like two seats claiming one credential, which failed every seat
    after the first.
    """
    role = FakeRole(
        "A",
        "a",
        {
            "plane": {
                "PLANE_API_KEY": "${PLANE_TOKEN_A}",
                "PLANE_WORKSPACE": "${PLANE_WORKSPACE}",
            }
        },
    )
    assert seat_token_vars(role) == ["PLANE_TOKEN_A"]


def test_seat_token_vars_accepts_the_header_spelling():
    """A remote MCP server carries the token as `X-API-Key`.

    Which keys hold one is boot-time identity resolution's question, and
    the answer is shared — a spelling missing here means a seat's token
    is silently never minted.
    """
    from crewlet.config import PLANE_TOKEN_KEYS

    for key in PLANE_TOKEN_KEYS:
        role = FakeRole("A", "a", {"plane": {key: "${PLANE_TOKEN_A}"}})
        assert seat_token_vars(role) == ["PLANE_TOKEN_A"], key


def test_two_seats_may_share_a_non_credential_var():
    """The shared-var check is about credentials, not about the block.

    Each seat gets its own token, so two seats pointing at one token var
    is a genuine config error — but two seats naming the same workspace
    is the ordinary case, and it used to abort the second seat.
    """
    a = FakeRole(
        "A",
        "a",
        {"plane": {"PLANE_API_KEY": "${PLANE_TOKEN_A}", "W": "${PLANE_WORKSPACE}"}},
    )
    b = FakeRole(
        "B",
        "b",
        {"plane": {"PLANE_API_KEY": "${PLANE_TOKEN_B}", "W": "${PLANE_WORKSPACE}"}},
    )
    assert set(seat_token_vars(a)) & set(seat_token_vars(b)) == set()


def test_seat_token_vars_empty_without_plane_block():
    assert seat_token_vars(FakeRole("B", "b", {})) == []
    assert seat_token_vars(FakeRole("C", "c", {"plane": {}})) == []


def test_seat_token_vars_ignores_sandbox_env():
    # Deliberate divergence from GitLab: Plane is not a code host — no
    # git-auth sandbox recipe, no conventional sandbox token key.  A
    # sandbox PLANE_API_KEY var must NOT be turned into a minted token.
    role = FakeRole(
        "D",
        "d",
        {"plane": {"PLANE_API_KEY": "${PLANE_TOKEN_D}"}},
        sandbox=_Sandbox({"PLANE_API_KEY": "${PLANE_SANDBOX_ONLY}"}),
    )
    assert seat_token_vars(role) == ["PLANE_TOKEN_D"]


# ---------------------------------------------------------------------------
# Fresh provision end-to-end + idempotent re-run
# ---------------------------------------------------------------------------


async def test_fresh_provision(tmp_path):
    fake = FakePlane()
    sink = EnvFileSink(str(tmp_path / ".env.plane"))
    report = await _run(fake, sink=sink)

    # Two agent seats provisioned; human + credential-less skipped.
    assert {s.username for s in report.seats} == {"agent-swe", "agent-fe"}
    assert all(s.account_action == "created" for s in report.seats)
    assert all(s.membership == "added" for s in report.seats)
    # Creation itself mints token #1 — the create response's single-show
    # token, with NO separate token call.
    assert all(s.token_action == "minted" for s in report.seats)
    content = (tmp_path / ".env.plane").read_text()
    assert "PLANE_TOKEN_SWE=plane_api_minted" in content
    assert "PLANE_TOKEN_FE=plane_api_minted" in content
    # The workspace role is ALWAYS passed explicitly (API default is
    # `admin` — omission would silently privilege-escalate).
    assert all(body["role"] == "member" for body in fake.create_bodies)
    # Username + display name are caller-chosen; name is the token label.
    assert fake.create_bodies[0]["username"] == "agent-swe"
    assert fake.create_bodies[0]["display_name"] == "Agent SWE"
    assert fake.create_bodies[0]["name"] == "crewlet-provision:agent-swe"
    # Webhook created with the pinned toggles; secret captured.
    assert len(fake.webhooks) == 1
    hook = fake.webhooks[0]
    assert hook["project"] and hook["issue"] and hook["issue_comment"]
    assert hook["page"] is True
    assert not hook["cycle"] and not hook["module"]
    assert any("(created, secret captured)" in h for h in report.hooks)
    assert "PLANE_WEBHOOK_SECRET=plane_wh_secret_" in content
    assert os.environ["PLANE_WEBHOOK_SECRET"].startswith("plane_wh_secret_")
    # The report ends with the member table (founder + both agents),
    # managed rows marked so founders can spot the human UUIDs.
    by_username = {m["username"]: m for m in report.members}
    assert by_username["founder"]["id"] == FOUNDER_ID
    assert by_username["founder"]["managed"] is False
    assert by_username["agent-swe"]["managed"] is True
    assert by_username["founder"]["role"] == "admin"
    # Human seat validated against the members list — no note.
    assert not any("founder" in n for n in report.notes)


async def test_missing_provisioning_block_is_tolerated(tmp_path):
    # Unlike GitLab (which requires provisioning.group), Plane has no
    # required provisioning field — everything defaults (role `member`,
    # no prefix, no projects).
    fake = FakePlane()
    cfg = PlaneConfig(
        enabled=True,
        url="https://plane.test",
        workspace="testco",
        webhook_secret="${PLANE_WEBHOOK_SECRET}",
    )
    assert cfg.provisioning is None
    report = await _run(
        fake, cfg=cfg, sink=EnvFileSink(str(tmp_path / ".env.plane")), webhook_url=""
    )
    assert all(s.account_action == "created" for s in report.seats)
    assert all(s.membership == "skipped" for s in report.seats)  # no projects
    assert all(body["role"] == "member" for body in fake.create_bodies)


async def test_rerun_is_idempotent(tmp_path):
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    await _run(fake, sink=EnvFileSink(env))
    minted_first = len(fake.minted_tokens)
    report2 = await _run(fake, sink=EnvFileSink(env))
    assert all(s.account_action == "exists" for s in report2.seats)
    assert all(s.membership == "exists" for s in report2.seats)
    assert all(s.token_action == "skipped" for s in report2.seats)
    assert len(fake.minted_tokens) == minted_first
    # Hook matched by URL → updated, never duplicated.
    assert len(fake.webhooks) == 1
    assert any("(updated)" in h for h in report2.hooks)
    # No drift notes on a faithful re-run.
    assert not any("drifted" in n for n in report2.notes)


async def test_creation_token_recorded_into_all_vars_overwriting_stale(tmp_path):
    # L2: a brand-new account's old var value cannot possibly be valid,
    # and discarding the create token would orphan the account's only
    # credential — so the single-show creation token overwrites ALL
    # referencing vars, recorded or not.
    env = tmp_path / ".env.plane"
    env.write_text("PLANE_TOKEN_SWE=stale-value-from-before\n")
    fake = FakePlane()
    report = await _run(fake, sink=EnvFileSink(str(env)), webhook_url="")
    swe = next(s for s in report.seats if s.handle == "agent-swe")
    assert swe.account_action == "created"
    assert swe.token_action == "minted"
    content = env.read_text()
    assert "stale-value-from-before" not in content
    assert "PLANE_TOKEN_SWE=plane_api_minted" in content


# ---------------------------------------------------------------------------
# Token lifecycle: mint-on-existing, rotate, degraded no-token-lifecycle modes
# ---------------------------------------------------------------------------


async def test_unrecorded_var_with_active_token_needs_rotate_on_plain_run(tmp_path):
    # Symmetry: an unrecorded var whose account holds an ACTIVE managed
    # token can only be recovered by rotating that live token — as
    # destructive as the webhook recreate, so a plain run (--print, a
    # second machine, a lost env file) NEVER does it: it reports
    # needs_rotate with a loud note and mints nothing.
    fake = FakePlane()
    await _run(fake, sink=EnvFileSink(str(tmp_path / ".env.first")), webhook_url="")
    minted_first = len(fake.minted_tokens)
    # Second machine: no env file, nothing exported.
    report = await _run(fake, sink=PrintSink(), webhook_url="")
    assert all(s.token_action == "needs_rotate" for s in report.seats)
    assert len(fake.minted_tokens) == minted_first  # nothing minted/rotated
    assert any(
        "--rotate" in n and "invalidates the value the running engine holds" in n
        for n in report.notes
    )
    # The live tokens are untouched.
    for seat in report.seats:
        active = [t for t in fake.tokens[seat.user_id] if t["is_active"]]
        assert len(active) == 1
    # --rotate is the recovery: the live token is rotated and every
    # referencing var gets the new value.
    report2 = await _run(fake, sink=PrintSink(), webhook_url="", rotate=True)
    assert all(s.token_action == "rotated" for s in report2.seats)
    assert any("plane_api_rotated" in t for t in fake.minted_tokens)


async def test_unrecorded_var_without_active_token_still_mints(tmp_path):
    # The genuine mint — no active managed token exists, so nothing live
    # can be invalidated — stays automatic on a plain run.
    fake = FakePlane()
    await _run(fake, sink=EnvFileSink(str(tmp_path / ".env.first")), webhook_url="")
    for seat_username in ("agent-swe", "agent-fe"):
        for token in fake.tokens[fake.uid(seat_username)]:
            token["is_active"] = False
    report = await _run(fake, sink=PrintSink(), webhook_url="")
    assert all(s.token_action == "minted" for s in report.seats)


async def test_multiple_active_managed_tokens_rotate_one_revoke_others(tmp_path):
    fake = FakePlane()
    await _run(fake, sink=EnvFileSink(str(tmp_path / ".env.first")), webhook_url="")
    uid = fake.uid("agent-swe")
    # Historic double-minting left a second active managed-label token.
    fake.mint_extra_token(uid, "crewlet-provision:agent-swe")
    report = await _run(fake, sink=PrintSink(), webhook_url="", rotate=True)
    swe = next(s for s in report.seats if s.handle == "agent-swe")
    assert swe.token_action == "rotated"
    active = [t for t in fake.tokens[uid] if t["is_active"]]
    revoked = [t for t in fake.tokens[uid] if t["revoked"]]
    assert len(active) == 1  # the rotation replacement
    assert len(revoked) == 1  # the stale parallel token was revoked
    assert any("revoked 1 stale managed token" in n for n in report.notes)


async def test_rotate_remints_with_explicit_expiry(tmp_path):
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    await _run(fake, sink=EnvFileSink(env), webhook_url="")
    report = await _run(fake, sink=EnvFileSink(env), webhook_url="", rotate=True)
    assert all(s.token_action == "rotated" for s in report.seats)
    assert any(t.startswith("plane_api_rotated") for t in fake.minted_tokens)
    # The rotate body ALWAYS carries an explicit expired_at — never the
    # omit/inherit form (which would reproduce GitLab's bare-rotate trap
    # in mirror image) — so SOURCE_TOKEN_EXPIRY_ELAPSED is unreachable.
    assert fake.rotate_bodies
    for body in fake.rotate_bodies:
        assert "expired_at" in body
        assert body["expired_at"] is not None
    # The new value replaced the recorded one in the env file.
    content = (tmp_path / ".env.plane").read_text()
    assert "PLANE_TOKEN_SWE=plane_api_rotated" in content
    # --rotate without --webhook-url leaves the webhook secret alone
    # and says so.
    assert any("webhook secret was NOT rotated" in n for n in report.notes)


async def test_rotate_zero_expiry_days_sends_explicit_null(tmp_path):
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    await _run(fake, sink=EnvFileSink(env), webhook_url="")
    await _run(
        fake,
        sink=EnvFileSink(env),
        webhook_url="",
        rotate=True,
        token_expiry_days=0,
    )
    # 0 ⇒ explicit JSON null (never expire) — the key is present.
    assert fake.rotate_bodies[-1] == {"expired_at": None}


async def test_rotate_409_falls_through_to_mint(tmp_path):
    # A 409 TOKEN_NOT_ACTIVE means someone else rotated/revoked the
    # token concurrently — fall through to minting a fresh one.
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    await _run(fake, sink=EnvFileSink(env), webhook_url="")
    fake.force_rotate_409 = True
    report = await _run(fake, sink=EnvFileSink(env), webhook_url="", rotate=True)
    assert all(s.token_action == "minted" for s in report.seats)
    assert any(t.startswith("plane_api_minted") for t in fake.minted_tokens[-2:])


async def test_expiring_managed_token_reported(tmp_path):
    # (token-lifecycle-gated) a recorded var whose token is expiring would
    # stay `skipped` forever with no signal — warn inside the 30-day
    # horizon and on an already-expired token.
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    await _run(fake, sink=EnvFileSink(env), webhook_url="")
    soon = (datetime.now(UTC) + timedelta(days=10)).isoformat()
    past = (datetime.now(UTC) - timedelta(days=2)).isoformat()
    fake.tokens[fake.uid("agent-swe")][0]["expired_at"] = soon
    fake.tokens[fake.uid("agent-fe")][0]["expired_at"] = past
    report = await _run(fake, sink=EnvFileSink(env), webhook_url="")
    assert all(s.token_action == "skipped" for s in report.seats)
    assert any(
        "agent-swe" in n and "within 30 days" in n and "--rotate" in n
        for n in report.notes
    )
    assert any(
        "agent-fe" in n and "expired" in n and "--rotate" in n for n in report.notes
    )


async def test_token_lifecycle_absent_plain_run_degrades_with_note(tmp_path):
    # Service-accounts-without-token-lifecycle is a legitimately useful
    # degraded mode — creation itself mints token #1, memberships and
    # accounts work.
    fake = FakePlane(token_lifecycle=False)
    report = await _run(
        fake, sink=EnvFileSink(str(tmp_path / ".env.plane")), webhook_url=""
    )
    assert all(s.account_action == "created" for s in report.seats)
    assert all(s.token_action == "minted" for s in report.seats)
    assert any("degraded service-account-only mode" in n for n in report.notes)


async def test_token_lifecycle_absent_mint_on_existing_gets_one_collective_note(
    tmp_path,
):
    fake = FakePlane()
    await _run(fake, sink=EnvFileSink(str(tmp_path / ".env.first")), webhook_url="")
    fake.token_lifecycle = False
    # Second machine, no recorded values: seats need a re-mint, which is
    # Re-minting needs the token-lifecycle routes — token_action is
    # `blocked` (COULD NOT provision — distinct
    # from `skipped` = already provisioned) with ONE collective note, not
    # N per-seat failures; the run still exits 0 (B5: degraded mode is a
    # legitimately useful mode, not an error).
    report = await _run(fake, sink=PrintSink(), webhook_url="")
    assert all(s.token_action == "blocked" for s in report.seats)
    assert all(s.account_action == "exists" for s in report.seats)
    assert all(not s.error for s in report.seats)  # exit code stays 0
    lifecycle_notes = [
        n for n in report.notes if "token-lifecycle" in n and "agent-swe" in n
    ]
    assert len(lifecycle_notes) == 1
    assert "agent-fe" in lifecycle_notes[0]


async def test_token_lifecycle_absent_rotate_aborts_pre_mutation(tmp_path):
    fake = FakePlane(token_lifecycle=False)
    with pytest.raises(PlaneProvisionAborted, match="token-lifecycle"):
        await _run(fake, sink=PrintSink(), webhook_url="", rotate=True)
    # Pre-mutation: nothing was created.
    assert fake.usernames() == {"founder"}


async def test_token_lifecycle_absent_decommission_aborts_pre_mutation(tmp_path):
    fake = FakePlane(token_lifecycle=False)
    with pytest.raises(PlaneProvisionAborted, match="token-lifecycle"):
        await _run(fake, sink=PrintSink(), webhook_url="", decommission_removed=True)
    assert fake.usernames() == {"founder"}


# ---------------------------------------------------------------------------
# Capability preflight aborts
# ---------------------------------------------------------------------------


async def test_stock_ce_aborts_with_fork_message():
    fake = FakePlane(service_accounts_route=False)
    with pytest.raises(PlaneProvisionAborted, match="crewlet/plane fork"):
        await _run(fake, sink=PrintSink(), webhook_url="")
    assert fake.usernames() == {"founder"}
    assert fake.webhooks == []


async def test_invalid_operator_token_aborts_naming_the_credential():
    # The /users/me credential probe runs FIRST: a bad token is named as
    # such, never misdiagnosed as "not a workspace admin" (the fork's own
    # contract tests leave 401-vs-403 unpinned for a bad credential, so
    # both statuses map to the credential message).
    fake = FakePlane(token_valid=False)
    with pytest.raises(PlaneProvisionAborted) as excinfo:
        await _run(fake, sink=PrintSink(), webhook_url="")
    message = str(excinfo.value)
    assert "invalid, expired, or revoked" in message
    assert "users/me" in message
    assert fake.usernames() == {"founder"}


async def test_bad_workspace_slug_aborts_naming_the_workspace():
    # With the credential proven by /users/me, a 403 from the cheapest
    # workspace-scoped route means the SLUG is wrong (Plane's permission
    # classes reject an unknown slug exactly like a non-member) — the
    # operator is pointed at integrations.plane.workspace, not at their
    # token's permissions.
    fake = FakePlane(workspace_visible=False)
    with pytest.raises(PlaneProvisionAborted) as excinfo:
        await _run(fake, sink=PrintSink(), webhook_url="")
    message = str(excinfo.value)
    assert "'testco'" in message
    assert "not found or not visible" in message
    assert "integrations.plane.workspace" in message
    assert fake.usernames() == {"founder"}


async def test_non_admin_operator_aborts_with_permission_message():
    # Credential (users/me 200) and slug (projects 200) both prove good,
    # so the service-accounts 403 is attributable to permission alone.
    fake = FakePlane(admin=False)
    with pytest.raises(PlaneProvisionAborted, match="workspace admin"):
        await _run(fake, sink=PrintSink(), webhook_url="")
    assert fake.usernames() == {"founder"}


async def test_members_rows_without_username_abort_naming_capability():
    fake = FakePlane(member_identity_fields=False)
    with pytest.raises(PlaneProvisionAborted, match="member-identity"):
        await _run(fake, sink=PrintSink(), webhook_url="")
    assert fake.usernames() == {"founder"}


async def test_username_echo_mismatch_aborts_naming_the_orphan():
    # An instance whose service-account create ignores caller-chosen
    # identity fields would — undetected — mint fresh svc_<uuid>
    # accounts per seat, forever.  The echo assert aborts, naming the
    # orphan account id (account DELETE needs the token-lifecycle
    # capability — we cannot clean it up).
    fake = FakePlane(honor_username=False)
    with pytest.raises(PlaneProvisionAborted) as excinfo:
        await _run(fake, sink=PrintSink(), webhook_url="")
    message = str(excinfo.value)
    assert "svc_" in message
    assert "orphan" in message
    # The orphan account it created (and named) really exists.
    orphan = next(u for u in fake.usernames() if u.startswith("svc_"))
    assert fake.uid(orphan) in message


async def test_username_conflict_is_a_seat_error_naming_the_remedies(tmp_path):
    # B8: decommission keeps the (deactivated) user row, so the username
    # is terminally taken — the 409 copy names the cause and both real
    # remedies, and the loop continues to the other seats.
    fake = FakePlane()
    fake.decommissioned_usernames.add("agent-swe")
    report = await _run(
        fake, sink=EnvFileSink(str(tmp_path / ".env.plane")), webhook_url=""
    )
    swe = next(s for s in report.seats if s.handle == "agent-swe")
    assert swe.account_action == "error"
    assert "irreversible" in swe.error
    assert "reactivate the account server-side" in swe.error
    # Per-seat isolation: the other seat still provisioned fully.
    fe = next(s for s in report.seats if s.handle == "agent-fe")
    assert fe.account_action == "created"
    assert fe.token_action == "minted"


async def test_long_handle_409_remediation_is_never_truncated(tmp_path):
    # PlaneProvisionError's raw-body cap lives at the client
    # boundary, so the crafted 409 remediation survives intact for ANY
    # username length — the trailing remedies must
    # never be truncated away.
    handle = "agent-platform-infrastructure-reliability-emea-lead"  # 51 chars
    org = FakeOrg(
        [
            FakeRole(
                "Reliability Lead",
                handle,
                {"plane": {"PLANE_API_KEY": "${PLANE_TOKEN_LONG}"}},
            ),
        ]
    )
    fake = FakePlane()
    fake.decommissioned_usernames.add(handle)
    report = await _run(
        fake, org=org, sink=EnvFileSink(str(tmp_path / ".env.plane")), webhook_url=""
    )
    seat = report.seats[0]
    assert seat.account_action == "error"
    assert "reactivate the account server-side" in seat.error  # tail survived


async def test_human_holding_seat_username_is_a_seat_error_before_membership():
    # A human workspace member squatting the derived username must never
    # be adopted as the seat's service account — no membership is written
    # onto the human, and the error names provisioning.username_prefix as
    # the remedy, BEFORE the opaque token-step 404 could fire.
    fake = FakePlane()
    human_uid = fake.add_human("agent-swe")
    report = await _run(fake, sink=PrintSink(), webhook_url="")
    swe = next(s for s in report.seats if s.handle == "agent-swe")
    assert swe.account_action == "error"
    assert "NOT a service account" in swe.error
    assert "provisioning.username_prefix" in swe.error
    # No membership write reached the human.
    for memberships in fake.project_members.values():
        assert human_uid not in memberships
    # Per-seat isolation: the other seat still provisioned fully.
    fe = next(s for s in report.seats if s.handle == "agent-fe")
    assert fe.account_action == "created"
    assert fe.token_action == "minted"


async def test_membership_failure_after_create_still_records_the_token(tmp_path):
    # Regression guard: the create response's single-show token is
    # recorded BEFORE memberships, so the project-admin-gated member POST
    # 403ing cannot discard the brand-new account's only
    # credential — and account_action stays `created` (the account
    # exists!) while the seat records the error, which still exits 1.
    fake = FakePlane(member_post_forbidden=True)
    env = tmp_path / ".env.plane"
    report = await _run(fake, sink=EnvFileSink(str(env)), webhook_url="")
    content = env.read_text()
    assert "PLANE_TOKEN_SWE=plane_api_minted" in content
    assert "PLANE_TOKEN_FE=plane_api_minted" in content
    for seat in report.seats:
        assert seat.account_action == "created"  # never overwritten to error
        assert seat.token_action == "minted"
        assert "403" in seat.error  # the membership failure is recorded


# ---------------------------------------------------------------------------
# Roles: explicit, validated, drift notes
# ---------------------------------------------------------------------------


async def test_workspace_role_overrides_are_sent_explicitly(tmp_path):
    fake = FakePlane()
    await _run(
        fake,
        cfg=_config(projects=["ENG"], role="guest", roles={"agent-swe": "admin"}),
        sink=EnvFileSink(str(tmp_path / ".env.plane")),
        webhook_url="",
    )
    roles_sent = {body["username"]: body["role"] for body in fake.create_bodies}
    assert roles_sent == {"agent-swe": "admin", "agent-fe": "guest"}
    # And the project membership got the mapped INT.
    eng = fake.projects["ENG"]["id"]
    assert fake.project_members[eng][fake.uid("agent-swe")]["role"] == 20
    assert fake.project_members[eng][fake.uid("agent-fe")]["role"] == 5


async def test_invalid_role_string_aborts_with_the_valid_list():
    # The config accepts free strings but the fork's serializer is a
    # strict ChoiceField — a GitLab copy-paste like `developer` would 400
    # on every seat, so it aborts up front instead.
    fake = FakePlane()
    with pytest.raises(PlaneProvisionAborted) as excinfo:
        await _run(
            fake,
            cfg=_config(role="developer"),
            sink=PrintSink(),
            webhook_url="",
        )
    message = str(excinfo.value)
    assert "developer" in message
    assert "admin | guest | member" in message


async def test_drift_notes_for_display_name_workspace_and_project_role(tmp_path):
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    await _run(fake, sink=EnvFileSink(env), webhook_url="")
    # Re-run wanting a different display name and role for agent-swe.
    org2 = FakeOrg(
        [
            FakeRole(
                "Agent SWE v2",
                "agent-swe",
                {"plane": {"PLANE_API_KEY": "${PLANE_TOKEN_SWE}"}},
            ),
        ]
    )
    report = await _run(
        fake,
        org=org2,
        cfg=_config(projects=["ENG"], roles={"agent-swe": "admin"}),
        sink=EnvFileSink(env),
        webhook_url="",
    )
    # Display-name drift is a note — the pinned fork has no
    # service-account update endpoint (delete+recreate would rotate the
    # user UUID and orphan HandleRegistry/attribution).
    assert any(
        "display name drifted" in n and "'Agent SWE'" in n and "'Agent SWE v2'" in n
        for n in report.notes
    )
    # Workspace-role drift is a note — the members API is read-only.
    assert any(
        "workspace role drifted" in n and "has member" in n and "wants admin" in n
        for n in report.notes
    )
    # Pinned outcome: project-role drift is DETECTED (via
    # the lite listing) but cannot be repaired — the public PATCH is
    # keyed by the ProjectMember row pk, which no list endpoint exposes.
    assert any(
        "project 'ENG' role drifted" in n and "fork patch" in n for n in report.notes
    )
    # Nothing was mutated behind the notes.
    eng = fake.projects["ENG"]["id"]
    assert fake.project_members[eng][fake.uid("agent-swe")]["role"] == 15


# ---------------------------------------------------------------------------
# Memberships: deactivated rows, the tightened duplicate discriminator,
# shared-${VAR} seats, and no-${VAR} seats
# ---------------------------------------------------------------------------


async def test_deactivated_membership_reports_inactive_with_note(tmp_path):
    # A membership removed in the Plane UI keeps its row with
    # is_active=False (the pinned detail DELETE), so the re-add 400s as a
    # duplicate while the seat has NO project access — the reconcile must
    # say `member=inactive` with the reactivate-in-Plane remediation, not
    # a clean `exists`.
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    await _run(fake, sink=EnvFileSink(env), webhook_url="")
    eng = fake.projects["ENG"]["id"]
    fake.deactivate_project_member(eng, fake.uid("agent-swe"))
    report = await _run(fake, sink=EnvFileSink(env), webhook_url="")
    swe = next(s for s in report.seats if s.handle == "agent-swe")
    assert swe.membership == "inactive"
    assert not swe.error  # a note, not a seat failure
    assert any(
        "agent-swe" in n and "DEACTIVATED" in n and "cannot reactivate" in n
        for n in report.notes
    )
    # The untouched seat still reads `exists`.
    fe = next(s for s in report.seats if s.handle == "agent-fe")
    assert fe.membership == "exists"


async def test_generic_400_without_a_lite_row_surfaces_the_error(tmp_path):
    # The BaseAPIView generic 400 body covers ANY IntegrityError from the
    # member view (e.g. ProjectUserProperty), not only the duplicate
    # constraint — so it counts as "exists" ONLY when a lite row confirms
    # the membership; otherwise the error surfaces instead of reporting
    # an add that did not happen.
    fake = FakePlane(member_post_integrity_400=True)
    env = tmp_path / ".env.plane"
    report = await _run(fake, sink=EnvFileSink(str(env)), webhook_url="")
    for seat in report.seats:
        assert "payload is not valid" in seat.error
        assert seat.account_action == "created"  # truthful account state
    # The creation tokens were recorded before the failing step.
    assert "PLANE_TOKEN_SWE=plane_api_minted" in env.read_text()


async def test_shared_token_var_across_seats_errors_the_second_seat(tmp_path):
    # One ${VAR} referenced by two seats cannot hold two identities — the
    # later write would win and the first seat's token would be silently
    # discarded, its agent authenticating as the other. The second seat
    # errors naming BOTH seats; the first provisions normally.
    org = FakeOrg(
        [
            FakeRole(
                "Agent A", "agent-a", {"plane": {"PLANE_API_KEY": "${PLANE_SHARED}"}}
            ),
            FakeRole(
                "Agent B", "agent-b", {"plane": {"PLANE_API_KEY": "${PLANE_SHARED}"}}
            ),
        ]
    )
    fake = FakePlane()
    report = await _run(
        fake, org=org, sink=EnvFileSink(str(tmp_path / ".env.plane")), webhook_url=""
    )
    a = next(s for s in report.seats if s.handle == "agent-a")
    b = next(s for s in report.seats if s.handle == "agent-b")
    assert a.account_action == "created"
    assert a.token_action == "minted"
    assert b.account_action == "error"
    assert "agent-a" in b.error and "agent-b" in b.error
    assert "${PLANE_SHARED}" in b.error
    # The second seat was never reconciled — no account, no server-side
    # token minted for it.
    assert "agent-b" not in fake.usernames()


async def test_seat_without_token_vars_never_creates_an_account(tmp_path):
    # A create would mint the account's single-show token #1 with nowhere
    # to record it — the credential would be orphaned invisibly. The seat
    # is skipped with a note instead.
    org = FakeOrg(
        [
            FakeRole(
                "Configured Agent",
                "agent-cfg",
                {"plane": {"PLANE_BASE_URL": "https://plane.test"}},  # no ${VAR}
            ),
        ]
    )
    fake = FakePlane()
    report = await _run(
        fake, org=org, sink=EnvFileSink(str(tmp_path / ".env.plane")), webhook_url=""
    )
    seat = report.seats[0]
    assert seat.account_action == "skipped"
    assert seat.token_action == "none"
    assert "agent-cfg" not in fake.usernames()
    assert any(
        "agent-cfg" in n and "orphaned" in n and "${VAR}" in n for n in report.notes
    )


async def test_seat_without_token_vars_still_reconciles_an_existing_account(tmp_path):
    # The no-create rule only guards CREATION; an already-existing account
    # still gets its membership reconcile.
    org = FakeOrg(
        [
            FakeRole(
                "Configured Agent",
                "agent-cfg",
                {"plane": {"PLANE_BASE_URL": "https://plane.test"}},  # no ${VAR}
            ),
        ]
    )
    fake = FakePlane()
    fake.add_stale_bot("agent-cfg")
    report = await _run(
        fake, org=org, sink=EnvFileSink(str(tmp_path / ".env.plane")), webhook_url=""
    )
    seat = report.seats[0]
    assert seat.account_action == "exists"
    assert seat.membership == "added"
    assert seat.token_action == "none"
    eng = fake.projects["ENG"]["id"]
    assert fake.uid("agent-cfg") in fake.project_members[eng]


# ---------------------------------------------------------------------------
# Projects: --create-projects gating + case-insensitive match
# ---------------------------------------------------------------------------


async def test_missing_projects_dropped_with_note_without_flag(tmp_path):
    fake = FakePlane(existing_projects=("ENG",))
    report = await _run(
        fake,
        cfg=_config(projects=["ENG", "PROD"]),
        sink=EnvFileSink(str(tmp_path / ".env.plane")),
        webhook_url="",
    )
    assert "PROD" not in fake.projects
    assert any(
        "PROD" in n and "--create-projects" in n and "not visible" in n
        for n in report.notes
    )
    # Seats still reconciled against the project that DOES exist.
    eng = fake.projects["ENG"]["id"]
    assert fake.uid("agent-swe") in fake.project_members[eng]


async def test_create_projects_flag_creates_missing(tmp_path):
    fake = FakePlane(existing_projects=("ENG",))
    report = await _run(
        fake,
        cfg=_config(projects=["ENG", "PROD"]),
        sink=EnvFileSink(str(tmp_path / ".env.plane")),
        webhook_url="",
        create_projects=True,
    )
    assert "PROD" in fake.projects
    assert any("created project 'PROD'" in n for n in report.notes)
    prod = fake.projects["PROD"]["id"]
    assert fake.uid("agent-swe") in fake.project_members[prod]
    assert all(s.membership == "added" for s in report.seats)


async def test_create_projects_conflict_is_a_note_never_an_abort(tmp_path):
    # An invisible-but-existing project (membership-scoped listing hides
    # it from the operator) classifies as "missing"; --create-projects
    # then 409s on the taken identifier. That is a per-project note + the
    # project dropped from targets — the seats and the projects that ARE
    # visible still reconcile.
    fake = FakePlane(existing_projects=("ENG", "PROD"), hidden_projects=("PROD",))
    report = await _run(
        fake,
        cfg=_config(projects=["ENG", "PROD"]),
        sink=EnvFileSink(str(tmp_path / ".env.plane")),
        webhook_url="",
        create_projects=True,
    )
    assert any(
        "could not create project 'PROD'" in n and "not visible" in n
        for n in report.notes
    )
    # The whole run went through: seats provisioned, ENG reconciled.
    assert all(s.account_action == "created" for s in report.seats)
    eng = fake.projects["ENG"]["id"]
    assert fake.uid("agent-swe") in fake.project_members[eng]
    # PROD was dropped — no membership writes were attempted against it.
    prod = fake.projects["PROD"]["id"]
    assert prod not in fake.project_members


async def test_project_identifier_match_is_case_insensitive(tmp_path):
    fake = FakePlane(existing_projects=("ENG",))
    report = await _run(
        fake,
        cfg=_config(projects=["eng"]),
        sink=EnvFileSink(str(tmp_path / ".env.plane")),
        webhook_url="",
    )
    assert not any("not visible" in n for n in report.notes)
    eng = fake.projects["ENG"]["id"]
    assert fake.uid("agent-swe") in fake.project_members[eng]
    assert not any(s.account_action == "error" for s in report.seats)


# ---------------------------------------------------------------------------
# Engine account
# ---------------------------------------------------------------------------


async def test_engine_account_minted_from_token_ref(tmp_path):
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    report = await _run(
        fake,
        cfg=_config(projects=["ENG"], role="guest"),
        sink=EnvFileSink(env),
        webhook_url="",
        engine_token_ref="${PLANE_ENGINE_TOKEN}",
    )
    engine = next(s for s in report.seats if s.handle == ENGINE_ACCOUNT_HANDLE)
    assert engine.account_action == "created"
    assert engine.token_action == "minted"
    assert engine.token_vars == ["PLANE_ENGINE_TOKEN"]
    assert (
        "PLANE_ENGINE_TOKEN=plane_api_minted" in (tmp_path / ".env.plane").read_text()
    )
    # The engine account is `member` (never guest, never the config
    # default) — guest visibility would break subscriber fan-out and the
    # project map — and a ProjectMember of every configured project.
    engine_body = next(
        b for b in fake.create_bodies if b["username"] == "crewlet-engine"
    )
    assert engine_body["role"] == "member"
    assert engine_body["display_name"] == "Crewlet Engine (routing)"
    eng = fake.projects["ENG"]["id"]
    assert fake.project_members[eng][fake.uid("crewlet-engine")]["role"] == 15


async def test_engine_account_skipped_without_ref():
    fake = FakePlane()
    report = await _run(
        fake,
        sink=PrintSink(),
        webhook_url="",
        engine_token_ref="plane_api_literal_already_resolved",
    )
    assert all(s.handle != ENGINE_ACCOUNT_HANDLE for s in report.seats)
    assert "crewlet-engine" not in fake.usernames()


async def test_engine_account_survives_decommission(tmp_path):
    fake = FakePlane()
    cfg = _config(projects=["ENG"], username_prefix="agent-")
    env = str(tmp_path / ".env.plane")
    await _run(
        fake,
        cfg=cfg,
        sink=EnvFileSink(env),
        webhook_url="",
        engine_token_ref="${PLANE_ENGINE_TOKEN}",
    )
    report = await _run(
        fake,
        cfg=cfg,
        sink=EnvFileSink(env),
        webhook_url="",
        engine_token_ref="${PLANE_ENGINE_TOKEN}",
        decommission_removed=True,
    )
    # The prefixed engine account is desired state, never stale.
    assert "agent-crewlet-engine" in fake.usernames()
    assert report.decommissioned == []


# ---------------------------------------------------------------------------
# Webhook ensure
# ---------------------------------------------------------------------------


async def test_webhook_update_and_reenable_when_secret_recorded(tmp_path):
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    await _run(fake, sink=EnvFileSink(env))
    # Plane auto-disabled the hook after delivery failures; the
    # reconcile's repair duty is to re-enable it.
    fake.webhooks[0]["is_active"] = False
    report = await _run(fake, sink=EnvFileSink(env))
    assert len(fake.webhooks) == 1
    assert fake.webhooks[0]["is_active"] is True
    assert any("(re-enabled)" in h for h in report.hooks)


async def test_lost_secret_is_gated_behind_recreate_flag(tmp_path):
    # --print / a fresh env file / a second machine must NOT silently
    # delete + recreate the hook (that invalidates the secret every other
    # deployment holds) — without the flag the run emits a loud note.
    fake = FakePlane()
    await _run(fake, sink=EnvFileSink(str(tmp_path / ".env.first")))
    original_id = fake.webhooks[0]["id"]
    # Second machine: the capture's in-process env export is gone too.
    del os.environ["PLANE_WEBHOOK_SECRET"]
    report = await _run(fake, sink=PrintSink())
    assert fake.webhooks[0]["id"] == original_id  # untouched
    assert any("secret UNRECORDED" in h for h in report.hooks)
    assert any("--recreate-webhook" in n for n in report.notes)


async def test_recreate_flag_mints_a_fresh_secret(tmp_path):
    fake = FakePlane()
    await _run(fake, sink=EnvFileSink(str(tmp_path / ".env.first")))
    original_id = fake.webhooks[0]["id"]
    # Second machine: the capture's in-process env export is gone too.
    del os.environ["PLANE_WEBHOOK_SECRET"]
    env2 = tmp_path / ".env.second"
    report = await _run(fake, sink=EnvFileSink(str(env2)), recreate_webhook=True)
    assert len(fake.webhooks) == 1
    assert fake.webhooks[0]["id"] != original_id
    assert any("recreated to mint a fresh secret" in n for n in report.notes)
    assert "PLANE_WEBHOOK_SECRET=plane_wh_secret_" in env2.read_text()


async def test_rotate_recreates_the_webhook_for_a_fresh_secret(tmp_path):
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    await _run(fake, sink=EnvFileSink(env))
    first_secret = fake.webhooks[0]["secret_key"]
    original_id = fake.webhooks[0]["id"]
    report = await _run(fake, sink=EnvFileSink(env), rotate=True)
    assert len(fake.webhooks) == 1
    assert fake.webhooks[0]["id"] != original_id
    assert fake.webhooks[0]["secret_key"] != first_secret
    assert any("(recreated, secret rotated)" in h for h in report.hooks)
    content = (tmp_path / ".env.plane").read_text()
    assert f"PLANE_WEBHOOK_SECRET={fake.webhooks[0]['secret_key']}" in content


async def test_rotate_create_failure_after_delete_aborts_naming_the_gap(tmp_path):
    # A failed create after the rotate path's delete leaves the workspace
    # with NO webhook — the abort must say the hook was deleted, that
    # deliveries are down, and that a re-run restores it (a raw API error
    # says none of that).
    fake = FakePlane()
    env = str(tmp_path / ".env.plane")
    await _run(fake, sink=EnvFileSink(env))
    fake.webhook_create_error = True
    with pytest.raises(PlaneProvisionAborted) as excinfo:
        await _run(fake, sink=EnvFileSink(env), rotate=True)
    message = str(excinfo.value)
    assert "DELETED" in message
    assert "deliveries are DOWN" in message
    assert "re-run" in message
    assert fake.webhooks == []  # the gap the message describes


async def test_recreate_create_failure_after_delete_aborts_naming_the_gap(tmp_path):
    # Same guarantee on the --recreate-webhook path.
    fake = FakePlane()
    await _run(fake, sink=EnvFileSink(str(tmp_path / ".env.first")))
    del os.environ["PLANE_WEBHOOK_SECRET"]
    fake.webhook_create_error = True
    with pytest.raises(PlaneProvisionAborted) as excinfo:
        await _run(fake, sink=PrintSink(), recreate_webhook=True)
    message = str(excinfo.value)
    assert "DELETED" in message
    assert "deliveries are DOWN" in message
    assert fake.webhooks == []


async def test_rotate_never_deletes_a_normalized_only_match(tmp_path):
    # On the destructive paths: a hook matching only after
    # normalization is a near-duplicate the provisioner did not create —
    # --rotate must neither delete it nor create a byte-exact sibling
    # (both would fire, double-delivering every event).
    fake = FakePlane()
    fake.webhooks.append(fake._make_hook({"url": _HOOK_URL + "/"}))
    original_id = fake.webhooks[0]["id"]
    report = await _run(
        fake, sink=EnvFileSink(str(tmp_path / ".env.plane")), rotate=True
    )
    assert len(fake.webhooks) == 1  # nothing deleted, nothing created
    assert fake.webhooks[0]["id"] == original_id
    assert any(
        "near-duplicate" in n and "NOT deleted" in n and _HOOK_URL + "/" in n
        for n in report.notes
    )


async def test_recreate_never_deletes_a_normalized_only_match(tmp_path):
    fake = FakePlane()
    fake.webhooks.append(fake._make_hook({"url": _HOOK_URL + "/"}))
    original_id = fake.webhooks[0]["id"]
    report = await _run(fake, sink=PrintSink(), recreate_webhook=True)
    assert len(fake.webhooks) == 1
    assert fake.webhooks[0]["id"] == original_id
    assert any("near-duplicate" in n and "NOT deleted" in n for n in report.notes)


async def test_literal_webhook_secret_aborts():
    # Inversion of GitLab: Plane generates the secret server-side, so a
    # literal config value can never match a hook this CLI creates.
    fake = FakePlane()
    with pytest.raises(PlaneProvisionAborted, match="reference"):
        await _run(
            fake,
            sink=PrintSink(),
            webhook_secret_ref="whsec_literal_value",
        )
    assert fake.usernames() == {"founder"}  # aborted pre-mutation


async def test_embedded_webhook_secret_ref_aborts():
    # "wh-${VAR}" resolves to a CONCATENATION that can never equal the
    # captured secret — every delivery would fail HMAC verification. The
    # guard requires the value to be EXACTLY one whole-value reference.
    fake = FakePlane()
    with pytest.raises(PlaneProvisionAborted, match="exactly one"):
        await _run(
            fake,
            sink=PrintSink(),
            webhook_secret_ref="wh-${PLANE_WEBHOOK_SECRET}",
        )
    assert fake.usernames() == {"founder"}
    assert fake.webhooks == []


async def test_multi_ref_webhook_secret_aborts():
    # "${A}${B}" records the same secret into both vars and concatenates
    # them at resolve time — same permanently mismatched HMAC key.
    fake = FakePlane()
    with pytest.raises(PlaneProvisionAborted, match="exactly one"):
        await _run(
            fake,
            sink=PrintSink(),
            webhook_secret_ref="${PLANE_WH_A}${PLANE_WH_B}",
        )
    assert fake.usernames() == {"founder"}


async def test_webhook_create_409_relists_and_adopts(tmp_path):
    fake = FakePlane(webhook_race=True)
    report = await _run(fake, sink=EnvFileSink(str(tmp_path / ".env.plane")))
    assert len(fake.webhooks) == 1
    assert any("(adopted)" in h for h in report.hooks)
    assert any("appeared concurrently" in n for n in report.notes)


async def test_near_duplicate_hook_urls_noted_never_deleted(tmp_path):
    # The uniqueness constraint is byte-exact, so `…/plane` and
    # `…/plane/` are two hooks and BOTH fire — note loudly, never
    # auto-delete a hook the provisioner did not create.
    fake = FakePlane()
    fake.webhooks.append(fake._make_hook({"url": _HOOK_URL}))
    fake.webhooks.append(fake._make_hook({"url": _HOOK_URL + "/"}))
    env = tmp_path / ".env.plane"
    env.write_text(f"PLANE_WEBHOOK_SECRET={fake.webhooks[0]['secret_key']}\n")
    report = await _run(fake, sink=EnvFileSink(str(env)))
    assert len(fake.webhooks) == 2  # nothing deleted
    assert any("double-deliver" in n for n in report.notes)
    # The exact-URL hook was still reconciled (toggles refreshed).
    assert any("(updated)" in h or "(re-enabled)" in h for h in report.hooks)


async def test_missing_page_echo_is_detected(tmp_path):
    # A server without the page-webhook capability silently DROPS the
    # `page` toggle (DRF
    # ignores unknown fields — no 400) and zero page events would be
    # delivered; the only reliable signal is the real create echo.
    fake = FakePlane(page_webhooks=False)
    report = await _run(fake, sink=EnvFileSink(str(tmp_path / ".env.plane")))
    assert "page" not in fake.webhooks[0]
    assert any("page events will NOT be delivered" in n for n in report.notes)


def test_normalize_hook_url_default_ports_and_slashes_and_userinfo():
    # Default ports (http:80/https:443) and duplicate
    # path slashes are spelling drift of the SAME hook; userinfo is
    # case-SENSITIVE and must survive verbatim while host case folds.
    from crewlet.plane.provision import _normalize_hook_url as norm

    base = norm("https://engine.test/webhooks/plane")
    assert norm("https://engine.test:443/webhooks/plane") == base
    assert norm("https://engine.test//webhooks//plane") == base
    assert norm("HTTPS://Engine.Test/webhooks/plane/") == base
    assert norm("http://engine.test:80/x") == norm("http://engine.test/x")
    # A NON-default port stays significant.
    assert norm("https://engine.test:8443/x") != norm("https://engine.test/x")
    # Userinfo case preserved, host case folded.
    assert (
        norm("https://User:SeCrEt@Engine.Test/hook")
        == "https://User:SeCrEt@engine.test/hook"
    )
    # An unparseable port can never equal a well-formed spelling — the
    # netloc is kept verbatim rather than crashing.
    assert norm("https://engine.test:bogus/x") != norm("https://engine.test/x")


async def test_no_webhook_api_aborts_when_webhook_url_given(tmp_path):
    fake = FakePlane(webhooks_route=False)
    with pytest.raises(PlaneProvisionAborted, match="webhook"):
        await _run(fake, sink=PrintSink())
    # Without --webhook-url the webhook API is never probed, so the
    # same instance provisions fine.
    report = await _run(
        fake, sink=EnvFileSink(str(tmp_path / ".env.plane")), webhook_url=""
    )
    assert all(s.account_action == "created" for s in report.seats)


# ---------------------------------------------------------------------------
# Decommission
# ---------------------------------------------------------------------------


async def test_decommission_requires_prefix():
    fake = FakePlane()
    report = await _run(
        fake,
        cfg=_config(),  # no username_prefix
        sink=PrintSink(),
        webhook_url="",
        decommission_removed=True,
    )
    assert any("decommission skipped" in n for n in report.notes)
    assert report.decommissioned == []


async def test_decommission_with_prefix_removes_stale(tmp_path):
    fake = FakePlane()
    cfg = _config(projects=["ENG"], username_prefix="agent-")
    env = str(tmp_path / ".env.plane")
    await _run(fake, cfg=cfg, sink=EnvFileSink(env), webhook_url="")
    fake.add_stale_bot("agent-ghost")
    report = await _run(
        fake,
        cfg=cfg,
        sink=EnvFileSink(env),
        webhook_url="",
        decommission_removed=True,
    )
    assert report.decommissioned == ["agent-ghost"]
    assert "agent-ghost" not in fake.usernames()
    # Desired (prefixed) seats survive.
    assert "agent-agent-swe" in fake.usernames()
    # The username is terminally taken — recorded server-side.
    assert "agent-ghost" in fake.decommissioned_usernames


async def test_decommission_never_deletes_a_prefixed_human(tmp_path):
    # The endpoint's own NOT_A_SERVICE_ACCOUNT guard makes decommission
    # human-safe; the 400 becomes a note, not an error.
    fake = FakePlane()
    cfg = _config(username_prefix="agent-")
    env = str(tmp_path / ".env.plane")
    await _run(fake, cfg=cfg, sink=EnvFileSink(env), webhook_url="")
    fake.add_human("agent-doubleagent")
    report = await _run(
        fake,
        cfg=cfg,
        sink=EnvFileSink(env),
        webhook_url="",
        decommission_removed=True,
    )
    assert "agent-doubleagent" in fake.usernames()
    assert report.decommissioned == []
    assert any(
        "agent-doubleagent" in n and "not a service account" in n for n in report.notes
    )


async def test_decommission_404_is_idempotent(tmp_path):
    fake = FakePlane()
    cfg = _config(username_prefix="agent-")
    env = str(tmp_path / ".env.plane")
    await _run(fake, cfg=cfg, sink=EnvFileSink(env), webhook_url="")
    ghost_uid = fake.add_stale_bot("agent-ghost")
    fake.delete_404_uids.add(ghost_uid)  # already decommissioned elsewhere
    report = await _run(
        fake,
        cfg=cfg,
        sink=EnvFileSink(env),
        webhook_url="",
        decommission_removed=True,
    )
    assert report.decommissioned == []
    assert not any(s.account_action == "error" for s in report.seats)


# ---------------------------------------------------------------------------
# Human seats + member table
# ---------------------------------------------------------------------------


async def test_human_seat_validation_notes(tmp_path):
    fake = FakePlane()
    org = FakeOrg(
        [
            FakeRole(
                "Agent SWE",
                "agent-swe",
                {"plane": {"PLANE_API_KEY": "${PLANE_TOKEN_SWE}"}},
            ),
            FakeRole(
                "Founder",
                "founder",
                {},
                kind="human",
                contact=FakeContact(FOUNDER_ID),  # valid member → silent
            ),
            FakeRole(
                "PM",
                "pm",
                {},
                kind="human",
                contact=FakeContact("99999999-9999-9999-9999-999999999999"),
            ),
            FakeRole(
                "CTO",
                "cto",
                {},
                kind="human",
                contact=FakeContact("${PLANE_CTO_ID}"),  # unset ${VAR}
            ),
            FakeRole("Intern", "intern", {}, kind="human"),  # no id → silent
        ]
    )
    report = await _run(
        fake,
        org=org,
        sink=EnvFileSink(str(tmp_path / ".env.plane")),
        webhook_url="",
    )
    assert any("'pm'" in n and "not a workspace member" in n for n in report.notes)
    assert any("'cto'" in n and "unset variable" in n for n in report.notes)
    assert not any("'founder'" in n for n in report.notes)
    assert not any("'intern'" in n for n in report.notes)


async def test_member_table_reflects_final_state(tmp_path):
    fake = FakePlane()
    cfg = _config(projects=["ENG"], username_prefix="agent-")
    env = str(tmp_path / ".env.plane")
    await _run(fake, cfg=cfg, sink=EnvFileSink(env), webhook_url="")
    fake.add_stale_bot("agent-ghost")
    report = await _run(
        fake,
        cfg=cfg,
        sink=EnvFileSink(env),
        webhook_url="",
        decommission_removed=True,
    )
    table = {row["username"]: row for row in report.members}
    # Final state: created accounts present, decommissioned ones gone.
    assert "agent-ghost" not in table
    assert table["agent-agent-swe"]["managed"] is True
    assert table["founder"]["managed"] is False
    assert table["founder"]["role"] == "admin"
    assert table["founder"]["id"] == FOUNDER_ID
    assert set(table["founder"]) == {
        "display_name",
        "username",
        "id",
        "role",
        "managed",
    }


# ---------------------------------------------------------------------------
# Error isolation + flush-on-abort
# ---------------------------------------------------------------------------


async def test_sink_flushed_when_webhook_ensure_aborts(tmp_path):
    # Tokens minted before a mid-run abort are unretrievable — the sink
    # must flush on the error path or the values are lost forever.
    fake = FakePlane(webhook_create_error=True)
    env = tmp_path / ".env.plane"
    with pytest.raises(PlaneProvisionError):
        await _run(fake, sink=EnvFileSink(str(env)))
    content = env.read_text()
    assert "PLANE_TOKEN_SWE=plane_api_minted" in content
    assert "PLANE_TOKEN_FE=plane_api_minted" in content


async def test_flush_failure_on_abort_does_not_mask_original_error():
    class ExplodingFlushSink(PrintSink):
        async def flush(self) -> None:
            raise OSError("disk full")

    fake = FakePlane(webhook_create_error=True)
    with pytest.raises(PlaneProvisionError):
        await _run(fake, sink=ExplodingFlushSink())


async def test_successful_run_flushes_exactly_once(capsys):
    # The except-BaseException wrapper must not double-flush the success
    # path. PrintSink prints inside record() now, so a second flush is
    # merely redundant rather than a duplicate export line — but a sink
    # that persists on flush (the encrypted store, an env file) still
    # pays for every extra one, and a wrapper that flushes twice here is
    # a wrapper that has lost track of its own control flow.
    class CountingSink(PrintSink):
        flushes = 0

        async def flush(self) -> None:
            type(self).flushes += 1
            await super().flush()

    fake = FakePlane()
    sink = CountingSink()
    await _run(fake, sink=sink, webhook_url="")
    assert CountingSink.flushes == 1
    out = capsys.readouterr().out
    assert out.count("export PLANE_TOKEN_SWE=") == 1
