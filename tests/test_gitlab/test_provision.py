"""Tests for the GitLab provisioning reconcile."""

from __future__ import annotations

import json
from urllib.parse import unquote

import httpx
import pytest

from crewlet.config import GitLabConfig, GitLabProvisioningConfig
from crewlet.gitlab.client import GitLabClient, GitLabProvisionError
from crewlet.gitlab.provision import (
    GitLabProvisionAborted,
    provision,
    seat_token_vars,
)
from crewlet.provisioning import EnvFileSink, PrintSink


class _Sandbox:
    def __init__(self, env):
        self.env = env


class FakeRole:
    def __init__(self, name, handle, mcp_env, *, kind="agent", email="", sandbox=None):
        self.name = name
        self._handle = handle
        self.mcp_env = mcp_env
        self.kind = kind
        self.email = email
        self.sandbox = sandbox

    def get_handle(self) -> str:
        return self._handle


class FakeOrg:
    def __init__(self, roles):
        self._roles = roles

    def all_roles(self):
        return self._roles


class FakeGitLab:
    """Stateful in-memory GitLab for the provision reconcile."""

    def __init__(
        self,
        *,
        group_hooks_forbidden: bool = True,
        service_accounts_forbidden: bool = False,
        existing_projects: set[str] | None = None,
    ):
        self.accounts: dict[str, dict] = {}  # username -> account
        self.tokens: dict[int, list[dict]] = {}  # user_id -> tokens
        self.members: list[tuple[str, int]] = []
        self.project_hooks: dict[str, list[dict]] = {}
        self.group_hooks: dict[str, list[dict]] = {}
        self._next_id = 100
        self._next_token_id = 1
        self._group_hooks_forbidden = group_hooks_forbidden
        self._service_accounts_forbidden = service_accounts_forbidden
        # None → every project exists (default); a set → only those exist.
        self._existing_projects = existing_projects
        self.minted_tokens: list[str] = []

    def _alloc(self) -> int:
        self._next_id += 1
        return self._next_id

    def handler(self, request: httpx.Request) -> httpx.Response:
        method = request.method
        # Use raw_path so a %2F-encoded group/project path stays one
        # segment, then decode each segment individually.
        raw = request.url.raw_path.decode().split("?", 1)[0]
        segs = [unquote(p) for p in raw.split("/") if p]
        parts = segs[2:]  # drop api, v4
        body = json.loads(request.content) if request.content else {}

        # /service_accounts under a group
        if parts[:1] == ["groups"] and "service_accounts" in parts:
            return self._service_accounts(method, parts, body)
        if parts[:1] == ["groups"] and parts[2:3] == ["members"]:
            self.members.append((parts[1], body["user_id"]))
            return httpx.Response(201, json={"id": body["user_id"]})
        if parts[:1] == ["groups"] and parts[2:3] == ["hooks"]:
            return self._group_hooks(method, parts, body)
        if parts[:1] == ["projects"] and len(parts) == 2:
            return self._get_project(parts[1])
        if parts[:1] == ["projects"] and parts[2:3] == ["members"]:
            return httpx.Response(201, json={"id": body["user_id"]})
        if parts[:1] == ["projects"] and parts[2:3] == ["hooks"]:
            return self._project_hooks(method, parts, body)
        return httpx.Response(404, text=f"unhandled {method} {'/'.join(parts)}")

    def _get_project(self, path: str) -> httpx.Response:
        if self._existing_projects is None or path in self._existing_projects:
            return httpx.Response(
                200, json={"id": self._alloc(), "path_with_namespace": path}
            )
        return httpx.Response(404, text='{"message":"404 Project Not Found"}')

    def _service_accounts(self, method, parts, body) -> httpx.Response:
        # parts: groups/<g>/service_accounts[/<uid>[/tokens...]]
        if len(parts) == 3:  # list or create
            if method == "GET":
                if self._service_accounts_forbidden:
                    return httpx.Response(403, text='{"message":"403 Forbidden"}')
                return httpx.Response(
                    200, json=list(self.accounts.values()), headers={}
                )
            uid = self._alloc()
            acct = {"id": uid, "username": body["username"], "name": body.get("name")}
            self.accounts[body["username"]] = acct
            self.tokens[uid] = []
            return httpx.Response(201, json=acct)
        uid = int(parts[3])
        if len(parts) >= 5 and parts[4] == "personal_access_tokens":
            if len(parts) == 5:
                if method == "GET":
                    return httpx.Response(
                        200, json=self.tokens.get(uid, []), headers={}
                    )
                tid = self._next_token_id
                self._next_token_id += 1
                value = f"glpat-minted-{tid}"
                tok = {
                    "id": tid,
                    "name": body["name"],
                    "active": True,
                    "token": value,
                }
                self.tokens.setdefault(uid, []).append(tok)
                self.minted_tokens.append(value)
                return httpx.Response(201, json=tok)
            if parts[6] == "rotate":
                tid = int(parts[5])
                value = f"glpat-rotated-{tid}"
                self.minted_tokens.append(value)
                return httpx.Response(200, json={"id": tid, "token": value})
        if method == "DELETE":
            # decommission
            for uname, acct in list(self.accounts.items()):
                if acct["id"] == uid:
                    del self.accounts[uname]
            return httpx.Response(204)
        return httpx.Response(404, text="sa route")

    def _group_hooks(self, method, parts, body) -> httpx.Response:
        if self._group_hooks_forbidden:
            return httpx.Response(403, text="group hooks are Premium")
        g = parts[1]
        if method == "GET":
            return httpx.Response(200, json=self.group_hooks.get(g, []), headers={})
        if method == "PUT":  # update existing group hook (no new row)
            return httpx.Response(200, json={"id": int(parts[3])})
        hook = {"id": self._alloc(), "url": body["url"]}
        self.group_hooks.setdefault(g, []).append(hook)
        return httpx.Response(201, json=hook)

    def _project_hooks(self, method, parts, body) -> httpx.Response:
        p = parts[1]
        if method == "GET":
            return httpx.Response(200, json=self.project_hooks.get(p, []), headers={})
        if method == "POST":
            hook = {"id": self._alloc(), "url": body["url"]}
            self.project_hooks.setdefault(p, []).append(hook)
            return httpx.Response(201, json=hook)
        return httpx.Response(200, json={"id": 1})


def _make_client(fake: FakeGitLab) -> GitLabClient:
    c = GitLabClient("https://gl/api/v4", "op")
    c._client = httpx.AsyncClient(
        base_url="https://gl/api/v4",
        headers={"PRIVATE-TOKEN": "op"},
        transport=httpx.MockTransport(fake.handler),
    )
    return c


def _config(**prov) -> GitLabConfig:
    return GitLabConfig(
        enabled=True,
        url="https://gl",
        signing_secret="whsec_x",
        provisioning=GitLabProvisioningConfig(group="nimbus-hq", **prov),
    )


def _org() -> FakeOrg:
    return FakeOrg(
        [
            FakeRole(
                "Agent SWE",
                "agent-swe",
                {"gitlab": {"Private-Token": "${GITLAB_TOKEN_SWE}"}},
                email="agent-swe@x.co",
            ),
            FakeRole(
                "Agent FE",
                "agent-fe",
                {"gitlab": {"Private-Token": "${GITLAB_TOKEN_FE}"}},
            ),
            FakeRole("Tech Lead", "tech-lead", {}),  # no gitlab creds → skipped
            FakeRole("Founder", "founder", {"gitlab": {"x": "y"}}, kind="human"),
        ]
    )


def test_seat_token_vars_unions_mcp_and_sandbox():
    # Same var in both places → deduped to one.
    role = FakeRole(
        "A",
        "a",
        {"gitlab": {"Private-Token": "${GITLAB_TOKEN_A}"}},
        sandbox=_Sandbox({"GITLAB_TOKEN": "${GITLAB_TOKEN_A}"}),
    )
    assert seat_token_vars(role) == ["GITLAB_TOKEN_A"]


def test_seat_token_vars_sandbox_only():
    # A sandbox-authoring seat with no MCP creds still gets its token var.
    role = FakeRole(
        "B", "b", {}, sandbox=_Sandbox({"GITLAB_TOKEN": "${GITLAB_TOKEN_B}"})
    )
    assert seat_token_vars(role) == ["GITLAB_TOKEN_B"]


def test_seat_token_vars_ignores_unrelated_sandbox_env():
    # Only the conventional GITLAB_TOKEN key is scanned, never other secrets.
    role = FakeRole(
        "C",
        "c",
        {"gitlab": {"Private-Token": "${GITLAB_TOKEN_C}"}},
        sandbox=_Sandbox({"OTHER_SECRET": "${SOME_OTHER}"}),
    )
    assert seat_token_vars(role) == ["GITLAB_TOKEN_C"]


async def test_fresh_provision(tmp_path):
    fake = FakeGitLab()
    sink = EnvFileSink(str(tmp_path / ".env.gitlab"))
    async with _make_client(fake) as client:
        report = await provision(
            client,
            _org(),
            _config(projects=["nimbus-hq/nimbuscore"]),
            webhook_url="https://engine/webhooks/gitlab",
            sink=sink,
        )
    # Two agent seats provisioned; human + credential-less skipped.
    provisioned = {s.username for s in report.seats}
    assert provisioned == {"agent-swe", "agent-fe"}
    assert all(s.account_action == "created" for s in report.seats)
    assert all(s.membership == "added" for s in report.seats)
    assert all(s.token_action == "minted" for s in report.seats)
    # Tokens landed in the env file under the referenced var names.
    content = (tmp_path / ".env.gitlab").read_text()
    assert "GITLAB_TOKEN_SWE=glpat-minted" in content
    assert "GITLAB_TOKEN_FE=glpat-minted" in content
    # Group hooks 403 (Premium) → per-project fallback, with a note.
    assert any("nimbus-hq/nimbuscore" in h for h in report.hooks)
    assert any("group webhook unavailable" in n for n in report.notes)


async def test_auto_group_hook_available_skips_per_project(tmp_path):
    # When the group-hooks API is available, `auto` creates ONE group hook
    # (which already covers every project in the group) and does NOT also
    # create per-project hooks — doing both would double every delivery.
    fake = FakeGitLab(group_hooks_forbidden=False)
    async with _make_client(fake) as client:
        report = await provision(
            client,
            _org(),
            _config(projects=["nimbus-hq/nimbuscore"], group_webhook="auto"),
            webhook_url="https://engine/webhooks/gitlab",
            sink=EnvFileSink(str(tmp_path / ".env.gitlab")),
        )
    assert any(h == "group:nimbus-hq (created)" for h in report.hooks)
    # No per-project hook was created and none is reported.
    assert fake.project_hooks == {}
    assert not any("nimbus-hq/nimbuscore" in h for h in report.hooks)
    # Exactly one group hook exists (not duplicated).
    assert len(fake.group_hooks.get("nimbus-hq", [])) == 1


async def test_group_hook_updated_not_duplicated_on_rerun(tmp_path):
    # Re-running refreshes the existing group hook (PUT) rather than
    # creating a second one — so a rotated signing_secret still propagates.
    fake = FakeGitLab(group_hooks_forbidden=False)
    env = str(tmp_path / ".env.gitlab")
    cfg = _config(projects=["nimbus-hq/nimbuscore"])
    async with _make_client(fake) as client:
        r1 = await provision(
            client,
            _org(),
            cfg,
            webhook_url="https://e/webhooks/gitlab",
            sink=EnvFileSink(env),
        )
        r2 = await provision(
            client,
            _org(),
            cfg,
            webhook_url="https://e/webhooks/gitlab",
            sink=EnvFileSink(env),
        )
    assert any(h == "group:nimbus-hq (created)" for h in r1.hooks)
    assert any(h == "group:nimbus-hq (updated)" for h in r2.hooks)
    assert len(fake.group_hooks.get("nimbus-hq", [])) == 1


async def test_service_accounts_403_aborts_with_identity_hint(tmp_path):
    # GitLab.com forbids the service-accounts API until the group Owner's
    # identity is verified — one clear abort, not N per-seat 403s.
    fake = FakeGitLab(service_accounts_forbidden=True)
    async with _make_client(fake) as client:
        with pytest.raises(GitLabProvisionAborted) as excinfo:
            await provision(
                client,
                _org(),
                _config(projects=["nimbus-hq/nimbuscore"]),
                webhook_url="https://engine/webhooks/gitlab",
                sink=EnvFileSink(str(tmp_path / ".env.gitlab")),
            )
    msg = str(excinfo.value)
    assert "identity" in msg.lower()
    assert "top-level group" in msg
    # Nothing was created — we aborted before touching seats/webhooks.
    assert fake.accounts == {}
    assert fake.project_hooks == {}


async def test_missing_projects_are_dropped_with_note(tmp_path):
    # Only one of the two declared projects exists; the missing one is
    # noted and skipped, the existing one still reconciles.
    fake = FakeGitLab(existing_projects={"nimbus-hq/nimbuscore"})
    sink = EnvFileSink(str(tmp_path / ".env.gitlab"))
    async with _make_client(fake) as client:
        report = await provision(
            client,
            _org(),
            _config(projects=["nimbus-hq/nimbuscore", "nimbus-hq/ghost"]),
            webhook_url="https://engine/webhooks/gitlab",
            sink=sink,
        )
    # Seats still provisioned; the reconcile did not abort.
    assert {s.username for s in report.seats} == {"agent-swe", "agent-fe"}
    assert all(s.account_action == "created" for s in report.seats)
    # The missing project is named in a note and got no hook; the real one did.
    assert any("nimbus-hq/ghost" in n for n in report.notes)
    assert any("nimbus-hq/nimbuscore" in h for h in report.hooks)
    assert "nimbus-hq/ghost" not in fake.project_hooks


async def test_rerun_is_idempotent(tmp_path):
    fake = FakeGitLab()
    env = str(tmp_path / ".env.gitlab")
    cfg = _config(projects=["nimbus-hq/nimbuscore"])
    async with _make_client(fake) as client:
        await provision(
            client,
            _org(),
            cfg,
            webhook_url="https://e/webhooks/gitlab",
            sink=EnvFileSink(env),
        )
        minted_first = len(fake.minted_tokens)
        report2 = await provision(
            client,
            _org(),
            cfg,
            webhook_url="https://e/webhooks/gitlab",
            sink=EnvFileSink(env),
        )
    # No new accounts, no new tokens (env file already has values).
    assert all(s.account_action == "exists" for s in report2.seats)
    assert all(s.token_action == "skipped" for s in report2.seats)
    assert len(fake.minted_tokens) == minted_first
    # Hook already present → updated, not duplicated.
    assert len(fake.project_hooks["nimbus-hq/nimbuscore"]) == 1


async def test_rotate_remints(tmp_path):
    fake = FakeGitLab()
    env = str(tmp_path / ".env.gitlab")
    cfg = _config()
    async with _make_client(fake) as client:
        await provision(client, _org(), cfg, webhook_url="", sink=EnvFileSink(env))
        report = await provision(
            client, _org(), cfg, webhook_url="", sink=EnvFileSink(env), rotate=True
        )
    assert all(s.token_action == "rotated" for s in report.seats)
    assert any(t.startswith("glpat-rotated") for t in fake.minted_tokens)


async def test_sink_flushed_when_webhook_ensure_aborts(tmp_path):
    # A mid-run abort AFTER tokens were minted (here: group_webhook="true"
    # but the group-hooks API 403s) must still flush the sink — minted PAT
    # values are unretrievable from GitLab, so skipping the flush would
    # discard them forever. Regression test for the try/except-flush wrap.
    fake = FakeGitLab()  # group hooks 403 by default
    env = tmp_path / ".env.gitlab"
    async with _make_client(fake) as client:
        with pytest.raises(GitLabProvisionError):
            await provision(
                client,
                _org(),
                _config(group_webhook="true"),
                webhook_url="https://e/webhooks/gitlab",
                sink=EnvFileSink(str(env)),
            )
    # The abort propagated, but the tokens minted before it are persisted.
    content = env.read_text()
    assert "GITLAB_TOKEN_SWE=glpat-minted" in content
    assert "GITLAB_TOKEN_FE=glpat-minted" in content


async def test_flush_failure_on_abort_does_not_mask_original_error(tmp_path):
    # When the reconcile aborts AND the best-effort flush itself fails,
    # the operator must still see the abort's original cause.
    class ExplodingFlushSink(PrintSink):
        async def flush(self) -> None:
            raise OSError("disk full")

    fake = FakeGitLab()
    async with _make_client(fake) as client:
        with pytest.raises(GitLabProvisionError):
            await provision(
                client,
                _org(),
                _config(group_webhook="true"),
                webhook_url="https://e/webhooks/gitlab",
                sink=ExplodingFlushSink(),
            )


async def test_successful_run_flushes_exactly_once(capsys):
    # The except-BaseException wrapper must not double-flush the success
    # path. PrintSink prints inside record() now, so a second flush is
    # merely redundant rather than a duplicate export line — but a sink
    # that persists on flush (the encrypted store, an env file) still
    # pays for every extra one, and a wrapper that flushes twice here is
    # a wrapper that has lost track of its own control flow.
    # (A future editor rewriting the wrapper as try/finally would trip
    # exactly this.)
    class CountingSink(PrintSink):
        flushes = 0

        async def flush(self) -> None:
            type(self).flushes += 1
            await super().flush()

    fake = FakeGitLab()
    sink = CountingSink()
    async with _make_client(fake) as client:
        await provision(client, _org(), _config(), webhook_url="", sink=sink)
    assert CountingSink.flushes == 1
    out = capsys.readouterr().out
    assert out.count("export GITLAB_TOKEN_SWE=") == 1


async def test_decommission_requires_prefix(tmp_path):
    fake = FakeGitLab()
    async with _make_client(fake) as client:
        report = await provision(
            client,
            _org(),
            _config(),  # no username_prefix
            webhook_url="",
            sink=PrintSink(),
            decommission_removed=True,
        )
    assert any("decommission skipped" in n for n in report.notes)
    assert report.decommissioned == []


async def test_decommission_with_prefix_removes_stale(tmp_path):
    fake = FakeGitLab()
    cfg = _config(username_prefix="agent-")
    async with _make_client(fake) as client:
        await provision(client, _org(), cfg, webhook_url="", sink=PrintSink())
        # Inject a stale prefixed account that no seat maps to.
        fake.accounts["agent-ghost"] = {"id": 999, "username": "agent-ghost"}
        report = await provision(
            client,
            _org(),
            cfg,
            webhook_url="",
            sink=PrintSink(),
            decommission_removed=True,
        )
    assert report.decommissioned == ["agent-ghost"]
    assert "agent-ghost" not in fake.accounts


async def test_engine_account_minted_from_token_ref(tmp_path):
    fake = FakeGitLab()
    env = str(tmp_path / ".env.gitlab")
    async with _make_client(fake) as client:
        report = await provision(
            client,
            _org(),
            _config(),
            webhook_url="",
            sink=EnvFileSink(env),
            engine_token_ref="${GITLAB_ENGINE_TOKEN}",
        )
    engine = next(s for s in report.seats if s.handle == "crewlet-engine")
    assert engine.account_action == "created"
    assert engine.token_action == "minted"
    assert engine.token_vars == ["GITLAB_ENGINE_TOKEN"]
    assert "crewlet-engine" in fake.accounts
    content = (tmp_path / ".env.gitlab").read_text()
    assert "GITLAB_ENGINE_TOKEN=glpat-minted" in content


async def test_engine_account_skipped_without_ref(tmp_path):
    fake = FakeGitLab()
    async with _make_client(fake) as client:
        report = await provision(
            client,
            _org(),
            _config(),
            webhook_url="",
            sink=PrintSink(),
            engine_token_ref="glpat-literal-already-resolved",
        )
    assert all(s.handle != "crewlet-engine" for s in report.seats)
    assert "crewlet-engine" not in fake.accounts


async def test_engine_account_survives_decommission(tmp_path):
    fake = FakeGitLab()
    cfg = _config(username_prefix="agent-")
    async with _make_client(fake) as client:
        await provision(
            client,
            _org(),
            cfg,
            webhook_url="",
            sink=PrintSink(),
            engine_token_ref="${GITLAB_ENGINE_TOKEN}",
        )
        report = await provision(
            client,
            _org(),
            cfg,
            webhook_url="",
            sink=PrintSink(),
            engine_token_ref="${GITLAB_ENGINE_TOKEN}",
            decommission_removed=True,
        )
    # The prefixed engine account is desired state, never stale.
    assert "agent-crewlet-engine" in fake.accounts
    assert report.decommissioned == []


class FakeInstanceGitLab(FakeGitLab):
    """FakeGitLab extended with instance-level service-account + admin
    user-token routes (the --mode instance surface)."""

    def handler(self, request: httpx.Request) -> httpx.Response:
        method = request.method
        raw = request.url.raw_path.decode().split("?", 1)[0]
        segs = [unquote(p) for p in raw.split("/") if p]
        parts = segs[2:]
        body = json.loads(request.content) if request.content else {}

        if parts == ["service_accounts"]:
            if method == "GET":
                return httpx.Response(200, json=list(self.accounts.values()))
            uid = self._alloc()
            acct = {"id": uid, "username": body["username"]}
            self.accounts[body["username"]] = acct
            self.tokens[uid] = []
            return httpx.Response(201, json=acct)
        if parts[:1] == ["users"] and parts[2:3] == ["personal_access_tokens"]:
            uid = int(parts[1])
            tid = self._next_token_id
            self._next_token_id += 1
            value = f"glpat-admin-minted-{tid}"
            self.tokens.setdefault(uid, []).append(
                {"id": tid, "name": body["name"], "active": True}
            )
            self.minted_tokens.append(value)
            return httpx.Response(201, json={"id": tid, "token": value})
        if parts[:1] == ["personal_access_tokens"]:
            if method == "GET":
                uid = int(request.url.params.get("user_id", "0"))
                return httpx.Response(200, json=self.tokens.get(uid, []))
            if parts[2:3] == ["rotate"]:
                value = f"glpat-admin-rotated-{parts[1]}"
                self.minted_tokens.append(value)
                return httpx.Response(200, json={"id": int(parts[1]), "token": value})
        return super().handler(request)


async def test_instance_mode_uses_admin_token_endpoints(tmp_path):
    fake = FakeInstanceGitLab()
    env = str(tmp_path / ".env.gitlab")
    async with _make_client(fake) as client:
        report = await provision(
            client,
            _org(),
            _config(),
            webhook_url="",
            sink=EnvFileSink(env),
            mode="instance",
        )
        rotated = await provision(
            client,
            _org(),
            _config(),
            webhook_url="",
            sink=EnvFileSink(env),
            mode="instance",
            rotate=True,
        )
    assert all(s.token_action == "minted" for s in report.seats)
    assert all(s.token_action == "rotated" for s in rotated.seats)
    # Instance mode never touched the group token endpoints.
    assert any(t.startswith("glpat-admin-minted") for t in fake.minted_tokens)
    assert any(t.startswith("glpat-admin-rotated") for t in fake.minted_tokens)
