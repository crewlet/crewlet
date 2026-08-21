"""Per-entity convenience CRUD on top of ``/config``.

Each handler is a thin wrapper that builds a single-field mutation
callback and hands it to
:meth:`RevisionDispatcher.write_entity_patch`.  The dispatcher
re-validates the merged result against the full :class:`CompanyConfig`
schema, so cross-field invariants (e.g. ``role.llm`` references a
configured provider) cannot drift between the full-doc and per-entity
paths.

The most useful entity routes are implemented; additional ones
follow the same pattern.

All routes return 409 when the engine is unconfigured (no active
revision to patch).  Validation errors return 400 with the Pydantic
detail.  No-such-entity returns 404.
"""

from __future__ import annotations

import json
from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse
from starlette.routing import Route

from crewlet._logging import get_logger
from crewlet.api.config_routes import (
    RevisionDispatcher,
    _cipher,
    _event_queue,
    _store,
)
from crewlet.org.models import slugify

logger = get_logger("api.config_entity_routes")


# ── helpers ───────────────────────────────────────────────────────────


async def _read_json_body(request: Request) -> dict[str, Any]:
    body = await request.body()
    if not body:
        raise ValueError("empty body")
    parsed = json.loads(body)
    if not isinstance(parsed, dict):
        raise ValueError("body must be a JSON object")
    return parsed


async def _parse_body_or_400(
    request: Request,
) -> tuple[dict[str, Any] | None, JSONResponse | None]:
    """Parse the JSON request body or return a ready-made 400 response.

    Every entity route did the same try/except :func:`_read_json_body`
    block; this collapses the 6 lines of boilerplate per route into one
    line plus an ``if error: return error`` guard.  Returns
    ``(body, None)`` on success and ``(None, JSONResponse)`` on failure
    so the caller doesn't have to know the error shape.
    """
    try:
        return await _read_json_body(request), None
    except (ValueError, json.JSONDecodeError) as exc:
        return None, JSONResponse(
            {"error": "invalid_body", "detail": str(exc)},
            status_code=400,
        )


def _readable_payload(request: Request, rev: Any) -> dict[str, Any]:
    """Display-safe, structure-visible view of a revision's payload.

    Every entity read (list + get) goes through this: it decrypts the
    stored blob (the key is required to see any structure) and masks the
    secret leaves. So a fragment returned to a client — or a name walked
    by a list route — never carries ciphertext or a plaintext secret.
    """
    from crewlet.secrets import redact_config

    return redact_config(rev.payload, _cipher(request))


def _operator_id(request: Request) -> str:
    return getattr(request.state, "operator_id", "unknown")


def _summary_header(request: Request) -> str | None:
    return request.headers.get("x-summary")


async def _dispatch(request: Request, mutate: Any) -> JSONResponse:
    dispatcher = RevisionDispatcher(
        _store(request), _event_queue(request), _cipher(request)
    )
    new_id, error, auto_summary = await dispatcher.write_entity_patch(
        mutate,
        operator_id=_operator_id(request),
        summary=_summary_header(request),
    )
    if error == "no_active_revision":
        return JSONResponse(
            {
                "error": "company_not_initialised",
                "hint": "PUT /config first to bootstrap the company.",
            },
            status_code=409,
        )
    if error == "revision_advanced":
        return JSONResponse(
            {
                "error": "revision_advanced",
                "hint": (
                    "A concurrent writer activated a new revision. "
                    "Re-read GET /config and retry the entity write."
                ),
            },
            status_code=409,
        )
    if error and error.startswith("not_found"):
        return JSONResponse(
            {"error": "not_found", "detail": error[len("not_found: ") :]},
            status_code=404,
        )
    if error and error.startswith("validation_error"):
        return JSONResponse(
            {
                "error": "validation_error",
                "detail": error[len("validation_error: ") :],
            },
            status_code=400,
        )
    return JSONResponse(
        {"revision_id": str(new_id), "summary": auto_summary},
        status_code=201,
    )


# ── /config/identity (PUT) ────────────────────────────────────────────


async def put_identity(request: Request) -> JSONResponse:
    """PUT /config/identity — replace {name, mission, vision, policies}."""
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        for key in ("name", "mission", "vision", "policies"):
            if key in body:
                payload[key] = body[key]
        return payload

    return await _dispatch(request, mutate)


# ── /config/turn-engine (PUT) ─────────────────────────────────────────


async def put_turn_engine(request: Request) -> JSONResponse:
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        payload["turn_engine"] = body
        return payload

    return await _dispatch(request, mutate)


# ── /config/budgets (PUT) ─────────────────────────────────────────────


async def put_budgets(request: Request) -> JSONResponse:
    """PUT /config/budgets — replace org token_budget."""
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        if "token_budget" in body:
            payload["token_budget"] = body["token_budget"]
        return payload

    return await _dispatch(request, mutate)


# ── /config/llm-providers (CRUD) ──────────────────────────────────────


# ---------------------------------------------------------------------
# Reading entities without a Request
# ---------------------------------------------------------------------
# The dashboard's Configuration room reads these collections, and every
# read it makes goes over the websocket — a view with its own HTTP client
# has a failure mode the shell knows nothing about, which is exactly how
# the Fleet view once shipped dead. The route handlers below need a
# ``Request`` (path params, headers); the query channel has none, so the
# extraction each one performs lives here and both call it.


def _entity_names(payload: dict[str, Any], kind: str) -> list[str]:
    """The ids in one collection, sorted."""
    if kind == "roles":
        from crewlet.api.config_summaries import _flat_role_handles

        return sorted(_flat_role_handles(payload))
    if kind == "units":
        names: list[str] = []

        def walk(units: list[dict[str, Any]]) -> None:
            for unit in units or []:
                name = str(unit.get("name") or "")
                if name:
                    names.append(name)
                walk(unit.get("children") or [])

        walk(payload.get("units") or [])
        return sorted(names)
    if kind == "llm-providers":
        llm = (payload.get("providers") or {}).get("llm", {}) or {}
        return sorted(llm.keys())
    if kind == "mcp-servers":
        return sorted(
            str(m.get("name") or "")
            for m in (payload.get("mcp_servers") or [])
            if m.get("name")
        )
    raise KeyError(kind)


def _entity_body(payload: dict[str, Any], kind: str, entity_id: str) -> Any:
    """One entity's fragment, as the editor round-trips it."""
    if kind == "roles":
        container, idx = _find_role_index(payload, entity_id)
        return container[idx]
    if kind == "units":
        found = _find_unit(payload, entity_id)
        if found is None:
            raise KeyError(entity_id)
        container, idx = found
        return container[idx]
    if kind == "llm-providers":
        llm = (payload.get("providers") or {}).get("llm", {}) or {}
        if entity_id not in llm:
            raise KeyError(entity_id)
        return llm[entity_id]
    if kind == "mcp-servers":
        for server in payload.get("mcp_servers") or []:
            if str(server.get("name") or "") == entity_id:
                return server
        raise KeyError(entity_id)
    raise KeyError(kind)


async def config_entities(app: Any, kind: str, entity_id: str = "") -> dict[str, Any]:
    """A collection's ids, or one entity's body, from the active revision.

    Raises :class:`KeyError` for an unknown kind or a missing entity, and
    :class:`LookupError` when no revision is active.
    """
    from crewlet.secrets import redact_config

    store = getattr(app.state, "company_config_store", None)
    if store is None:
        raise LookupError("no_config_store")
    rev = await store.get_active()
    if rev is None:
        raise LookupError("no_active_revision")
    payload = redact_config(rev.payload, getattr(app.state, "secret_cipher", None))
    if entity_id:
        return {
            "kind": kind,
            "id": entity_id,
            "entity": _entity_body(payload, kind, entity_id),
        }
    return {"kind": kind, "ids": _entity_names(payload, kind)}


async def list_llm_providers(request: Request) -> JSONResponse:
    """GET /config/llm-providers — list keys in the active config."""
    rev = await _store(request).get_active()
    if rev is None:
        return JSONResponse({"error": "no_active_revision"}, status_code=404)
    llm = (_readable_payload(request, rev).get("providers") or {}).get("llm", {})
    return JSONResponse(sorted(llm.keys()))


async def get_llm_provider(request: Request) -> JSONResponse:
    """GET /config/llm-providers/{key}."""
    key = request.path_params["key"]
    rev = await _store(request).get_active()
    if rev is None:
        return JSONResponse({"error": "no_active_revision"}, status_code=404)
    llm = (_readable_payload(request, rev).get("providers") or {}).get("llm", {})
    if key not in llm:
        return JSONResponse({"error": "not_found"}, status_code=404)
    return JSONResponse(llm[key])


async def put_llm_provider(request: Request) -> JSONResponse:
    """POST /config/llm-providers/{key} or PUT — upsert one provider."""
    key = request.path_params["key"]
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        providers = payload.setdefault("providers", {})
        llm = providers.setdefault("llm", {})
        llm[key] = body
        return payload

    return await _dispatch(request, mutate)


async def delete_llm_provider(request: Request) -> JSONResponse:
    key = request.path_params["key"]

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        providers = payload.get("providers") or {}
        llm = providers.get("llm") or {}
        if key not in llm:
            raise KeyError(f"llm provider {key!r}")
        del llm[key]
        return payload

    return await _dispatch(request, mutate)


# ── /config/units (CRUD) ──────────────────────────────────────────────


def _find_unit(payload: dict[str, Any], name: str) -> tuple[list, int] | None:
    """Return (container, index) for the unit by name at any depth."""

    def _scan(units: list[dict[str, Any]]) -> tuple[list, int] | None:
        for i, u in enumerate(units):
            if u.get("name") == name:
                return units, i
            found = _scan(u.get("children", []) or [])
            if found is not None:
                return found
        return None

    return _scan(payload.get("units", []) or [])


async def list_units(request: Request) -> JSONResponse:
    """GET /config/units — flat list of unit names (any depth)."""
    rev = await _store(request).get_active()
    if rev is None:
        return JSONResponse({"error": "no_active_revision"}, status_code=404)

    names: list[str] = []

    def _walk(units: list[dict[str, Any]]) -> None:
        for u in units:
            if u.get("name"):
                names.append(u["name"])
            _walk(u.get("children", []) or [])

    _walk(_readable_payload(request, rev).get("units", []) or [])
    return JSONResponse(sorted(names))


async def get_unit(request: Request) -> JSONResponse:
    name = request.path_params["name"]
    rev = await _store(request).get_active()
    if rev is None:
        return JSONResponse({"error": "no_active_revision"}, status_code=404)
    found = _find_unit(_readable_payload(request, rev), name)
    if found is None:
        return JSONResponse({"error": "not_found"}, status_code=404)
    container, idx = found
    return JSONResponse(container[idx])


async def post_unit(request: Request) -> JSONResponse:
    """POST /config/units — append a root-level unit.

    Nested unit additions go via PUT /config (full doc) or
    PUT /config/units/{parent} which can include ``children``.
    """
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker
    if "name" not in body:
        return JSONResponse(
            {"error": "validation_error", "detail": "unit requires 'name'"},
            status_code=400,
        )

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        units = payload.setdefault("units", [])
        if any(u.get("name") == body["name"] for u in units):
            raise ValueError(f"unit {body['name']!r} already exists at root")
        units.append(body)
        return payload

    return await _dispatch(request, mutate)


async def put_unit(request: Request) -> JSONResponse:
    """PUT /config/units/{name} — merge update of an existing unit
    (at any depth).  Body fields override; nested ``children`` /
    ``roles`` lists replace the existing ones (deep merge is too
    surprising)."""
    name = request.path_params["name"]
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        found = _find_unit(payload, name)
        if found is None:
            raise KeyError(f"unit {name!r}")
        container, idx = found
        container[idx] = {**container[idx], **body, "name": name}
        return payload

    return await _dispatch(request, mutate)


async def delete_unit(request: Request) -> JSONResponse:
    name = request.path_params["name"]

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        found = _find_unit(payload, name)
        if found is None:
            raise KeyError(f"unit {name!r}")
        container, idx = found
        container.pop(idx)
        return payload

    return await _dispatch(request, mutate)


# ── /config/roles (CRUD) ──────────────────────────────────────────────


def _find_role_index(payload: dict[str, Any], handle: str) -> tuple[list, int]:
    """Return (container_list, index) for the role with this handle, raising
    KeyError if not found.  Walks root roles + units + nested children.
    """

    def _scan_unit(unit: dict[str, Any]):
        for i, r in enumerate(unit.get("roles", []) or []):
            if r.get("handle") == handle or slugify(r.get("name", "")) == handle:
                return unit["roles"], i
        for child in unit.get("children", []) or []:
            found = _scan_unit(child)
            if found is not None:
                return found
        return None

    for i, r in enumerate(payload.get("roles", []) or []):
        if r.get("handle") == handle or slugify(r.get("name", "")) == handle:
            return payload["roles"], i
    for u in payload.get("units", []) or []:
        found = _scan_unit(u)
        if found is not None:
            return found
    raise KeyError(handle)


async def list_roles(request: Request) -> JSONResponse:
    rev = await _store(request).get_active()
    if rev is None:
        return JSONResponse({"error": "no_active_revision"}, status_code=404)
    from crewlet.api.config_summaries import _flat_role_handles

    return JSONResponse(sorted(_flat_role_handles(_readable_payload(request, rev))))


async def post_role(request: Request) -> JSONResponse:
    """POST /config/roles — append a root-level role.

    To add a role inside a unit, PUT the whole unit via
    /config/units/{name} (which preserves nesting) or PUT /config.
    """
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker
    if "name" not in body:
        return JSONResponse(
            {"error": "validation_error", "detail": "role requires 'name'"},
            status_code=400,
        )

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        roles = payload.setdefault("roles", [])
        # Reject a handle collision (root OR any unit): two roles sharing
        # a handle derive the same ``derive_agent_id`` and produce
        # ambiguous webhook / inbox routing.  Mirrors the duplicate
        # guards on ``post_unit`` / ``post_mcp_server``.
        new_handle = body.get("handle") or slugify(body.get("name", ""))
        try:
            _find_role_index(payload, new_handle)
        except KeyError:
            pass
        else:
            raise ValueError(f"role {new_handle!r} already exists")
        roles.append(body)
        return payload

    return await _dispatch(request, mutate)


async def get_role(request: Request) -> JSONResponse:
    handle = request.path_params["handle"]
    rev = await _store(request).get_active()
    if rev is None:
        return JSONResponse({"error": "no_active_revision"}, status_code=404)
    try:
        container, idx = _find_role_index(_readable_payload(request, rev), handle)
    except KeyError:
        return JSONResponse({"error": "not_found"}, status_code=404)
    return JSONResponse(container[idx])


async def put_role(request: Request) -> JSONResponse:
    handle = request.path_params["handle"]
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        container, idx = _find_role_index(payload, handle)
        container[idx] = {**container[idx], **body}
        return payload

    return await _dispatch(request, mutate)


async def delete_role(request: Request) -> JSONResponse:
    handle = request.path_params["handle"]

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        container, idx = _find_role_index(payload, handle)
        container.pop(idx)
        return payload

    return await _dispatch(request, mutate)


# ── /config/embeddings (PUT) ──────────────────────────────────────────


async def put_embeddings(request: Request) -> JSONResponse:
    """PUT /config/embeddings — replace the single embedding provider config."""
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        providers = payload.setdefault("providers", {})
        providers["embeddings"] = body
        return payload

    return await _dispatch(request, mutate)


# ── /config/learning (PUT) ────────────────────────────────────────────


async def put_learning(request: Request) -> JSONResponse:
    """PUT /config/learning — replace the learning subsystem settings."""
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        payload["learning"] = body
        return payload

    return await _dispatch(request, mutate)


# ── /config/integrations/{kind} (PUT) ─────────────────────────────────


async def put_integration(request: Request) -> JSONResponse:
    """PUT /config/integrations/{kind} — replace one integration block.

    ``kind`` is one of: ``jira``, ``confluence``, ``slack``,
    ``mattermost``, ``github``, ``gitlab``, ``plane``.
    """
    kind = request.path_params["kind"]
    if kind not in {
        "jira",
        "confluence",
        "slack",
        "mattermost",
        "github",
        "gitlab",
        "plane",
    }:
        return JSONResponse(
            {"error": "unknown_integration", "kind": kind}, status_code=404
        )
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        integrations = payload.get("integrations") or {}
        integrations[kind] = body
        payload["integrations"] = integrations
        return payload

    return await _dispatch(request, mutate)


# ── /config/mcp-servers (CRUD) ────────────────────────────────────────


async def list_mcp_servers(request: Request) -> JSONResponse:
    rev = await _store(request).get_active()
    if rev is None:
        return JSONResponse({"error": "no_active_revision"}, status_code=404)
    return JSONResponse(
        sorted(
            m.get("name", "")
            for m in (_readable_payload(request, rev).get("mcp_servers") or [])
            if m.get("name")
        )
    )


async def get_mcp_server(request: Request) -> JSONResponse:
    name = request.path_params["name"]
    rev = await _store(request).get_active()
    if rev is None:
        return JSONResponse({"error": "no_active_revision"}, status_code=404)
    for m in _readable_payload(request, rev).get("mcp_servers") or []:
        if m.get("name") == name:
            return JSONResponse(m)
    return JSONResponse({"error": "not_found"}, status_code=404)


async def post_mcp_server(request: Request) -> JSONResponse:
    """POST /config/mcp-servers — append a server."""
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker
    if "name" not in body:
        return JSONResponse(
            {"error": "validation_error", "detail": "mcp_server requires 'name'"},
            status_code=400,
        )

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        servers = payload.setdefault("mcp_servers", [])
        if any(s.get("name") == body["name"] for s in servers):
            raise ValueError(f"mcp_server {body['name']!r} already exists")
        servers.append(body)
        return payload

    return await _dispatch(request, mutate)


async def put_mcp_server(request: Request) -> JSONResponse:
    name = request.path_params["name"]
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        servers = payload.get("mcp_servers") or []
        for i, s in enumerate(servers):
            if s.get("name") == name:
                servers[i] = {**s, **body, "name": name}
                payload["mcp_servers"] = servers
                return payload
        raise KeyError(f"mcp_server {name!r}")

    return await _dispatch(request, mutate)


async def delete_mcp_server(request: Request) -> JSONResponse:
    name = request.path_params["name"]

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        servers = payload.get("mcp_servers") or []
        for i, s in enumerate(servers):
            if s.get("name") == name:
                servers.pop(i)
                payload["mcp_servers"] = servers
                return payload
        raise KeyError(f"mcp_server {name!r}")

    return await _dispatch(request, mutate)


# ── /config/extensions (CRUD) ────────────────────────────────────────


def _ext_name(e: dict[str, Any]) -> str:
    return next(iter(e.keys()), "")


async def list_extensions(request: Request) -> JSONResponse:
    rev = await _store(request).get_active()
    if rev is None:
        return JSONResponse({"error": "no_active_revision"}, status_code=404)
    return JSONResponse(
        sorted(
            _ext_name(e)
            for e in (_readable_payload(request, rev).get("extensions") or [])
        )
    )


async def post_extension(request: Request) -> JSONResponse:
    """POST /config/extensions — append an extension entry."""
    body, error = await _parse_body_or_400(request)
    if error is not None:
        return error
    assert body is not None  # narrow Optional for the type-checker

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        extensions = payload.setdefault("extensions", [])
        extensions.append(body)
        return payload

    return await _dispatch(request, mutate)


async def delete_extension(request: Request) -> JSONResponse:
    name = request.path_params["name"]

    def mutate(payload: dict[str, Any]) -> dict[str, Any]:
        extensions = payload.get("extensions") or []
        for i, e in enumerate(extensions):
            if _ext_name(e) == name:
                extensions.pop(i)
                payload["extensions"] = extensions
                return payload
        raise KeyError(f"extension {name!r}")

    return await _dispatch(request, mutate)


# ── route table ──────────────────────────────────────────────────────


def build_config_entity_routes() -> list[Route]:
    """Return per-entity routes ready for ``Mount`` under the auth
    middleware (alongside :func:`build_config_routes`).
    """
    return [
        # identity
        Route("/config/identity", put_identity, methods=["PUT"]),
        # roles
        Route("/config/roles", list_roles, methods=["GET"]),
        Route("/config/roles", post_role, methods=["POST"]),
        Route("/config/roles/{handle}", get_role, methods=["GET"]),
        Route("/config/roles/{handle}", put_role, methods=["PUT"]),
        Route("/config/roles/{handle}", delete_role, methods=["DELETE"]),
        # units
        Route("/config/units", list_units, methods=["GET"]),
        Route("/config/units", post_unit, methods=["POST"]),
        Route("/config/units/{name}", get_unit, methods=["GET"]),
        Route("/config/units/{name}", put_unit, methods=["PUT"]),
        Route("/config/units/{name}", delete_unit, methods=["DELETE"]),
        # llm-providers
        Route("/config/llm-providers", list_llm_providers, methods=["GET"]),
        Route("/config/llm-providers/{key}", get_llm_provider, methods=["GET"]),
        Route("/config/llm-providers/{key}", put_llm_provider, methods=["PUT"]),
        Route(
            "/config/llm-providers/{key}",
            delete_llm_provider,
            methods=["DELETE"],
        ),
        # embeddings
        Route("/config/embeddings", put_embeddings, methods=["PUT"]),
        # turn-engine
        Route("/config/turn-engine", put_turn_engine, methods=["PUT"]),
        # learning
        Route("/config/learning", put_learning, methods=["PUT"]),
        # budgets
        Route("/config/budgets", put_budgets, methods=["PUT"]),
        # integrations
        Route("/config/integrations/{kind}", put_integration, methods=["PUT"]),
        # mcp-servers
        Route("/config/mcp-servers", list_mcp_servers, methods=["GET"]),
        Route("/config/mcp-servers", post_mcp_server, methods=["POST"]),
        Route("/config/mcp-servers/{name}", get_mcp_server, methods=["GET"]),
        Route("/config/mcp-servers/{name}", put_mcp_server, methods=["PUT"]),
        Route("/config/mcp-servers/{name}", delete_mcp_server, methods=["DELETE"]),
        # extensions
        Route("/config/extensions", list_extensions, methods=["GET"]),
        Route("/config/extensions", post_extension, methods=["POST"]),
        Route("/config/extensions/{name}", delete_extension, methods=["DELETE"]),
    ]
