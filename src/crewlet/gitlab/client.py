"""Async REST client for the subset of the GitLab API the provisioner uses.

Thin wrapper over ``httpx`` with ``PRIVATE-TOKEN`` auth: service accounts,
their personal access tokens, group/project membership, and project/group
webhooks — plus ``GET /user`` for credential probing. No SDK dependency;
the endpoint surface here is exactly what :mod:`crewlet.gitlab.provision`
calls. See ``docs/integrations/gitlab.md``.
"""

from __future__ import annotations

from typing import Any
from urllib.parse import quote

import httpx

from crewlet._logging import get_logger

logger = get_logger("gitlab.client")

#: GitLab membership access levels (``POST /groups|projects/:id/members``).
ACCESS_LEVELS: dict[str, int] = {
    "guest": 10,
    "reporter": 20,
    "developer": 30,
    "maintainer": 40,
    "owner": 50,
}


class GitLabProvisionError(RuntimeError):
    """A GitLab API call the provisioner depends on failed.

    Carries the method + path + status so the CLI can report exactly
    which endpoint rejected the operator credential (e.g. a group-Owner
    PAT lacking admin rights for an instance-level create).
    """

    def __init__(self, method: str, path: str, status: int, body: str) -> None:
        self.method = method
        self.path = path
        self.status = status
        self.body = body
        super().__init__(f"{method} {path} → {status}: {body[:300]}")


class GitLabClient:
    """Minimal async GitLab REST client.

    ``api_base`` is the ``…/api/v4`` root (see
    :attr:`crewlet.config.GitLabConfig.api_base`); ``token`` is the
    operator credential (group Owner on GitLab.com, or instance admin on
    self-managed).
    """

    def __init__(self, api_base: str, token: str, *, timeout: float = 30.0) -> None:
        self._api_base = api_base.rstrip("/")
        self._client = httpx.AsyncClient(
            base_url=self._api_base,
            headers={"PRIVATE-TOKEN": token},
            timeout=timeout,
        )

    async def __aenter__(self) -> GitLabClient:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        await self._client.aclose()

    # ── low-level ──

    @staticmethod
    def _enc(id_or_path: str | int) -> str:
        """URL-encode a group/project id-or-path for a path segment."""
        return quote(str(id_or_path), safe="")

    async def _request(
        self,
        method: str,
        path: str,
        *,
        json: Any = None,
        params: dict[str, Any] | None = None,
        ok: tuple[int, ...] = (200, 201, 204),
    ) -> Any:
        resp = await self._client.request(method, path, json=json, params=params)
        if resp.status_code not in ok:
            raise GitLabProvisionError(method, path, resp.status_code, resp.text)
        if resp.status_code == 204 or not resp.content:
            return None
        try:
            return resp.json()
        except ValueError:
            return None

    async def _paginated(
        self, path: str, *, params: dict[str, Any] | None = None
    ) -> list[dict[str, Any]]:
        """GET every page of a list endpoint (``per_page=100``)."""
        out: list[dict[str, Any]] = []
        page = 1
        base_params = dict(params or {})
        while True:
            page_params = {**base_params, "per_page": 100, "page": page}
            resp = await self._client.get(path, params=page_params)
            if resp.status_code != 200:
                raise GitLabProvisionError("GET", path, resp.status_code, resp.text)
            batch = resp.json()
            if not isinstance(batch, list) or not batch:
                break
            out.extend(batch)
            next_page = resp.headers.get("x-next-page", "")
            if not next_page:
                break
            page = int(next_page)
        return out

    # ── identity ──

    async def get_current_user(self) -> dict[str, Any]:
        """``GET /user`` — probe the operator/agent credential."""
        return await self._request("GET", "/user")

    # ── participants ──

    async def get_issue_participants(
        self, project: str | int, iid: int
    ) -> list[dict[str, Any]]:
        return await self._paginated(
            f"/projects/{self._enc(project)}/issues/{iid}/participants"
        )

    async def get_merge_request_participants(
        self, project: str | int, iid: int
    ) -> list[dict[str, Any]]:
        return await self._paginated(
            f"/projects/{self._enc(project)}/merge_requests/{iid}/participants"
        )

    # ── service accounts ──

    async def list_group_service_accounts(
        self, group: str | int
    ) -> list[dict[str, Any]]:
        return await self._paginated(f"/groups/{self._enc(group)}/service_accounts")

    async def create_group_service_account(
        self,
        group: str | int,
        *,
        name: str,
        username: str,
        email: str = "",
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {"name": name, "username": username}
        if email:
            payload["email"] = email
        return await self._request(
            "POST", f"/groups/{self._enc(group)}/service_accounts", json=payload
        )

    async def update_group_service_account(
        self,
        group: str | int,
        user_id: int,
        *,
        name: str | None = None,
        email: str | None = None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {}
        if name is not None:
            payload["name"] = name
        if email is not None:
            payload["email"] = email
        return await self._request(
            "PATCH",
            f"/groups/{self._enc(group)}/service_accounts/{user_id}",
            json=payload,
        )

    async def create_instance_service_account(
        self, *, name: str, username: str, email: str = ""
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {"name": name, "username": username}
        if email:
            payload["email"] = email
        return await self._request("POST", "/service_accounts", json=payload)

    async def list_instance_service_accounts(self) -> list[dict[str, Any]]:
        return await self._paginated("/service_accounts")

    async def delete_group_service_account(
        self, group: str | int, user_id: int, *, hard_delete: bool = False
    ) -> None:
        await self._request(
            "DELETE",
            f"/groups/{self._enc(group)}/service_accounts/{user_id}",
            params={"hard_delete": str(hard_delete).lower()},
        )

    # ── service-account tokens ──

    async def create_group_service_account_token(
        self,
        group: str | int,
        user_id: int,
        *,
        name: str,
        scopes: list[str],
        expires_at: str | None = None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {"name": name, "scopes": scopes}
        if expires_at:
            payload["expires_at"] = expires_at
        return await self._request(
            "POST",
            f"/groups/{self._enc(group)}/service_accounts/{user_id}"
            "/personal_access_tokens",
            json=payload,
        )

    async def list_group_service_account_tokens(
        self, group: str | int, user_id: int
    ) -> list[dict[str, Any]]:
        return await self._paginated(
            f"/groups/{self._enc(group)}/service_accounts/{user_id}"
            "/personal_access_tokens"
        )

    async def rotate_group_service_account_token(
        self,
        group: str | int,
        user_id: int,
        token_id: int,
        *,
        expires_at: str | None = None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {}
        if expires_at:
            payload["expires_at"] = expires_at
        return await self._request(
            "POST",
            f"/groups/{self._enc(group)}/service_accounts/{user_id}"
            f"/personal_access_tokens/{token_id}/rotate",
            json=payload,
        )

    async def create_user_token(
        self,
        user_id: int,
        *,
        name: str,
        scopes: list[str],
        expires_at: str | None = None,
    ) -> dict[str, Any]:
        """Admin ``POST /users/:id/personal_access_tokens`` (CE-safe path)."""
        payload: dict[str, Any] = {"name": name, "scopes": scopes}
        if expires_at:
            payload["expires_at"] = expires_at
        return await self._request(
            "POST", f"/users/{user_id}/personal_access_tokens", json=payload
        )

    async def list_user_tokens(self, user_id: int) -> list[dict[str, Any]]:
        """Admin ``GET /personal_access_tokens?user_id=`` — any user's PATs.

        The instance-mode counterpart of
        :meth:`list_group_service_account_tokens`: instance-level service
        accounts are not *associated* with a group, so the group-scoped
        token endpoints don't apply to them.
        """
        return await self._paginated(
            "/personal_access_tokens", params={"user_id": user_id}
        )

    async def rotate_token(
        self, token_id: int, *, expires_at: str | None = None
    ) -> dict[str, Any]:
        """Admin ``POST /personal_access_tokens/:id/rotate`` (any user's PAT)."""
        payload: dict[str, Any] = {}
        if expires_at:
            payload["expires_at"] = expires_at
        return await self._request(
            "POST", f"/personal_access_tokens/{token_id}/rotate", json=payload
        )

    # ── membership ──

    async def list_group_members(self, group: str | int) -> list[dict[str, Any]]:
        return await self._paginated(f"/groups/{self._enc(group)}/members")

    async def add_group_member(
        self, group: str | int, user_id: int, access_level: int
    ) -> dict[str, Any]:
        return await self._request(
            "POST",
            f"/groups/{self._enc(group)}/members",
            json={"user_id": user_id, "access_level": access_level},
            ok=(200, 201, 409),
        )

    async def add_project_member(
        self, project: str | int, user_id: int, access_level: int
    ) -> dict[str, Any]:
        return await self._request(
            "POST",
            f"/projects/{self._enc(project)}/members",
            json={"user_id": user_id, "access_level": access_level},
            ok=(200, 201, 409),
        )

    # ── projects ──

    async def get_project(self, project: str | int) -> dict[str, Any]:
        """``GET /projects/:id`` — existence/access probe for a declared project.

        Raises :class:`GitLabProvisionError` with ``status == 404`` when the
        project does not exist (GitLab also returns 404, not 403, for a
        project the token cannot see — but a group-Owner operator sees every
        project in its group, so 404 here means genuinely missing).
        """
        return await self._request("GET", f"/projects/{self._enc(project)}")

    # ── webhooks ──

    async def list_project_hooks(self, project: str | int) -> list[dict[str, Any]]:
        return await self._paginated(f"/projects/{self._enc(project)}/hooks")

    async def create_project_hook(
        self, project: str | int, *, url: str, **flags: Any
    ) -> dict[str, Any]:
        payload = {"url": url, **flags}
        return await self._request(
            "POST", f"/projects/{self._enc(project)}/hooks", json=payload
        )

    async def update_project_hook(
        self, project: str | int, hook_id: int, *, url: str, **flags: Any
    ) -> dict[str, Any]:
        payload = {"url": url, **flags}
        return await self._request(
            "PUT", f"/projects/{self._enc(project)}/hooks/{hook_id}", json=payload
        )

    async def list_group_hooks(self, group: str | int) -> list[dict[str, Any]]:
        return await self._paginated(f"/groups/{self._enc(group)}/hooks")

    async def create_group_hook(
        self, group: str | int, *, url: str, **flags: Any
    ) -> dict[str, Any]:
        payload = {"url": url, **flags}
        return await self._request(
            "POST", f"/groups/{self._enc(group)}/hooks", json=payload
        )

    async def update_group_hook(
        self, group: str | int, hook_id: int, *, url: str, **flags: Any
    ) -> dict[str, Any]:
        payload = {"url": url, **flags}
        return await self._request(
            "PUT", f"/groups/{self._enc(group)}/hooks/{hook_id}", json=payload
        )


def build_participants_fetcher(api_base: str, token: str) -> Any:
    """Build the async ``(project_id, kind, iid) → usernames`` lookup the
    webhook parser uses for participants-based routing.

    ``kind`` is ``"issue"`` or ``"merge_request"``; returns lowercased
    usernames. Opens a short-lived client per call — participant lookups
    happen at comment/state-change rate (well below connection-reuse
    territory), and a per-call client needs no lifecycle management
    across config hot-reloads; the same trade the boot-time ``GET /user``
    identity resolution makes.
    """

    async def fetch(project_id: int, kind: str, iid: int) -> list[str]:
        async with GitLabClient(api_base, token, timeout=10.0) as client:
            if kind == "merge_request":
                users = await client.get_merge_request_participants(project_id, iid)
            else:
                users = await client.get_issue_participants(project_id, iid)
        return [
            str(u.get("username", "")).lower()
            for u in users
            if isinstance(u, dict) and u.get("username")
        ]

    return fetch
