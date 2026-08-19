"""``/config/*`` routes — versioned company-config CRUD over the API.

All routes auth-gated by :class:`ApiAuthMiddleware`.  Reads return
the current active revision (or its history); writes funnel through
:class:`RevisionDispatcher` which validates the merged payload, stores
a new revision, marks it active, and publishes
``ConfigRevisionActivated`` on Pulsar so the engine + API both pick up
the change.

Behaviour while unconfigured (no active revision):

- ``GET /config`` → 404 ``no_active_revision``
- ``GET /config/revisions`` → 200 ``[]``
- ``PUT /config`` → accepted; creates the first active revision
- ``POST /config/revisions/{id}/revert`` → 404
- ``GET /config/revisions/{id}/diff?against=active`` → 404
"""

from __future__ import annotations

import json
from typing import Any
from uuid import UUID

import yaml
from starlette.requests import Request
from starlette.responses import JSONResponse, Response
from starlette.routing import Route

from crewlet._logging import get_logger

# Body parsing + structural-diff helpers live in
# :mod:`crewlet.api.config_yaml_io`.  Re-exported under their
# pre-existing private names so call sites in this module are stable.
from crewlet.api.config_yaml_io import parse_request_body as _parse_request_body
from crewlet.api.config_yaml_io import structural_diff as _structural_diff
from crewlet.events.types import ConfigRevisionActivated

logger = get_logger("api.config_routes")


# ── helpers ───────────────────────────────────────────────────────────


def _is_concurrency_conflict(exc: BaseException) -> bool:
    """Return True for asyncpg errors that indicate a concurrent writer
    won the race to insert the next active revision.

    Either path produces this:

    - ``UniqueViolationError`` on the ``company_config_one_active``
      partial index when two transactions both INSERT with
      ``is_active=TRUE`` after their respective deactivate pass.
    - ``SerializationError`` under SERIALIZABLE isolation when the DB
      detects a write-write conflict.

    Imported lazily so we don't take a hard dep on asyncpg in test
    suites that swap in an in-memory storage backend.
    """
    try:
        from asyncpg.exceptions import (
            SerializationError,
            UniqueViolationError,
        )
    except ImportError:
        return False
    return isinstance(exc, (UniqueViolationError, SerializationError))


def _store(request: Request) -> Any:
    """Return the CompanyConfigStore from app.state."""
    store = getattr(request.app.state, "company_config_store", None)
    if store is None:
        raise RuntimeError(
            "company_config_store not configured on the API app — "
            "Tier A setup is incomplete."
        )
    return store


def _event_queue(request: Request) -> Any:
    return request.app.state.event_queue


def _cipher(request: Request) -> Any:
    """Return the secret-encryption keyring (or ``None`` when disabled)."""
    return getattr(request.app.state, "secret_cipher", None)


def _serialize_revision_meta(rev: Any) -> dict[str, Any]:
    """Return the metadata view of a revision (no payload)."""
    return {
        "revision_id": str(rev.revision_id),
        "parent_revision_id": (
            str(rev.parent_revision_id) if rev.parent_revision_id else None
        ),
        "created_at": rev.created_at.isoformat() if rev.created_at else None,
        "created_by": rev.created_by,
        "source": rev.source,
        "summary": rev.summary,
        "is_active": rev.is_active,
        "activated_at": (rev.activated_at.isoformat() if rev.activated_at else None),
    }


# ── RevisionDispatcher ────────────────────────────────────────────────


class RevisionDispatcher:
    """Centralised write path used by every config-mutation route.

    Loads the active revision (if any), applies a caller-supplied
    mutation to a copy of the payload, validates the merged result as
    :class:`CompanyConfig`, persists as a new active revision, then
    publishes ``ConfigRevisionActivated`` on Pulsar.
    """

    def __init__(self, store: Any, event_queue: Any, cipher: Any = None) -> None:
        self._store = store
        self._event_queue = event_queue
        self._cipher = cipher

    async def _publish_activated(
        self, revision_id: UUID, summary: str, source: str, created_by: str
    ) -> None:
        await self._event_queue.publish(
            "crewlet.config.revision_activated",
            ConfigRevisionActivated(
                source="api.config_routes",
                revision_id=str(revision_id),
                revision_summary=summary,
                created_by=created_by,
            ),
        )

    async def write_full(
        self,
        payload: dict[str, Any],
        *,
        operator_id: str,
        summary: str,
        if_match: str | None,
    ) -> tuple[UUID, str | None]:
        """Validate + persist a full replacement payload.

        Returns ``(revision_id, error)``.  ``error`` is set when
        ``If-Match`` conflicts or validation fails — the caller turns
        it into the appropriate HTTP status.
        """
        from crewlet.config import CompanyConfig
        from crewlet.config_yaml import company_config_to_dict

        existing = await self._store.get_active()
        if existing is not None:
            # ``If-Match`` is enforced when supplied.  Unconfigured-
            # state semantics: header absent OR literally "none".
            if if_match is not None and if_match != str(existing.revision_id):
                return existing.revision_id, "revision_advanced"
        else:
            if if_match is not None and if_match.lower() != "none":
                return None, "no_active_revision_but_if_match_set"

        from crewlet.secrets import (
            SecretLeakError,
            load_config,
            restore_redacted,
            store_config,
        )

        # Keep-existing: a full-doc PUT of a redacted GET carries markers
        # where secrets belong; swap each back to the stored (decrypted)
        # value before validation so round-trips never clobber a secret.
        restore_source = (
            load_config(existing.payload, self._cipher) if existing is not None else {}
        )
        try:
            payload = restore_redacted(payload, restore_source)
        except SecretLeakError as exc:
            return None, f"validation_error: {exc}"

        try:
            cfg = CompanyConfig.model_validate(payload)
        except Exception as exc:
            return None, f"validation_error: {exc}"

        normalised = store_config(company_config_to_dict(cfg), self._cipher)
        parent = existing.revision_id if existing is not None else None
        try:
            new_id = await self._store.insert_active(
                normalised,
                created_by=operator_id,
                source="api",
                summary=summary,
                parent_revision_id=parent,
            )
        except Exception as exc:
            # TOCTOU race: between ``get_active()`` and ``insert_active()``
            # another writer activated a new revision.  The partial
            # unique index ``company_config_one_active`` (or the
            # serialization failure on a concurrent tx) surfaces here.
            # Translate to ``409 revision_advanced`` so the caller can
            # rebase + retry instead of seeing an opaque 500.  The
            # follow-up ``get_active()`` reports the winning revision
            # so the client knows which id to rebase against — yet
            # another writer slipping in between makes that id slightly
            # stale, but the client will discover it on retry and the
            # outcome (rebase + re-PUT) is unchanged.
            if _is_concurrency_conflict(exc):
                current = await self._store.get_active()
                return (
                    current.revision_id if current is not None else None,
                    "revision_advanced",
                )
            raise
        await self._publish_activated(
            new_id, summary, source="api", created_by=operator_id
        )
        return new_id, None

    async def write_entity_patch(
        self,
        mutate: Any,
        *,
        operator_id: str,
        summary: str | None,
    ) -> tuple[UUID | None, str | None, str | None]:
        """Apply a per-entity mutation and persist as a new revision.

        ``mutate`` is a callable ``(payload: dict) -> dict`` that
        returns the mutated payload (or raises ``ValueError`` /
        ``KeyError`` to signal a 4xx).  The dispatcher validates the
        merged result as :class:`CompanyConfig`, persists, and
        publishes ``ConfigRevisionActivated``.

        Returns ``(revision_id, error_code, auto_summary)``.
        ``error_code`` is set on 4xx-style failures; the caller maps
        it to the appropriate HTTP status.  ``auto_summary`` is the
        auto-generated change description (used when ``summary`` is
        not provided).
        """
        from crewlet.api.config_summaries import summarize_change
        from crewlet.config import CompanyConfig
        from crewlet.config_yaml import company_config_to_dict

        existing = await self._store.get_active()
        if existing is None:
            return None, "no_active_revision", None

        import copy

        from crewlet.secrets import (
            SecretLeakError,
            load_config,
            restore_redacted,
            store_config,
        )

        # The stored payload is one encrypted blob — decrypt it to the
        # plaintext structure the mutate + summary operate on.
        old_base = load_config(existing.payload, self._cipher)
        try:
            new_payload = mutate(copy.deepcopy(old_base))
        except KeyError as exc:
            return None, f"not_found: {exc}", None
        except ValueError as exc:
            return None, f"validation_error: {exc}", None

        # Keep-existing for a patch whose body carried redaction markers
        # (e.g. a client that read a redacted provider and edited one
        # field): swap markers back to the base's values.
        try:
            new_payload = restore_redacted(new_payload, old_base)
        except SecretLeakError as exc:
            return None, f"validation_error: {exc}", None

        try:
            cfg = CompanyConfig.model_validate(new_payload)
        except Exception as exc:
            return None, f"validation_error: {exc}", None

        # Summarise on the normalised structures (field names only — never
        # secret values), then encrypt the whole document for storage.
        new_normalised = company_config_to_dict(cfg)
        try:
            old_normalised = company_config_to_dict(
                CompanyConfig.model_validate(old_base)
            )
        except Exception:
            old_normalised = old_base
        auto_summary = summarize_change(old_normalised, new_normalised)
        actual_summary = summary or auto_summary
        stored = store_config(new_normalised, self._cipher)

        try:
            new_id = await self._store.insert_active(
                stored,
                created_by=operator_id,
                source="api.entity",
                summary=actual_summary,
                parent_revision_id=existing.revision_id,
            )
        except Exception as exc:
            if _is_concurrency_conflict(exc):
                return None, "revision_advanced", auto_summary
            raise
        await self._publish_activated(
            new_id, actual_summary, source="api.entity", created_by=operator_id
        )
        return new_id, None, auto_summary

    async def revert(
        self,
        revision_id: UUID,
        *,
        operator_id: str,
        summary: str,
    ) -> tuple[UUID | None, str | None]:
        """Re-activate an existing revision's payload as a new revision.

        Creates a new row (not just `activate(revision_id)`) so the
        audit chain stays intact: ``parent_revision_id`` points at the
        currently-active revision, ``summary`` records the revert
        intent, ``created_by`` is the operator who triggered it.
        """
        target = await self._store.get_revision(revision_id)
        if target is None:
            return None, "revision_not_found"

        from crewlet.secrets import SecretDecryptError, load_config, store_config

        existing = await self._store.get_active()
        parent = existing.revision_id if existing is not None else None
        # Re-store the historical payload under the current key: decrypt
        # it (whatever key it was saved under) then re-encrypt, so a revert
        # never re-introduces a plaintext secret and lands under the
        # active key.  A target sealed under a key dropped from the keyring
        # can't be decrypted — surface it as a 4xx, not a 500.
        try:
            payload = store_config(
                load_config(target.payload, self._cipher), self._cipher
            )
        except SecretDecryptError as exc:
            return None, f"decrypt_failed: {exc}"
        try:
            new_id = await self._store.insert_active(
                payload,
                created_by=operator_id,
                source="api.revert",
                summary=summary,
                parent_revision_id=parent,
            )
        except Exception as exc:
            if _is_concurrency_conflict(exc):
                return None, "revision_advanced"
            raise
        await self._publish_activated(
            new_id, summary, source="api.revert", created_by=operator_id
        )
        return new_id, None


# ── routes ─────────────────────────────────────────────────────────────


async def config_document(app: Any) -> dict[str, Any] | None:
    """The active revision's payload with every secret redacted.

    Shared by ``GET /config`` and the WebSocket ``config`` query so the
    two cannot diverge on what they mask.  ``None`` means there is no
    active revision.
    """
    from crewlet.secrets import redact_config

    store = getattr(app.state, "company_config_store", None)
    if store is None:
        return None
    rev = await store.get_active()
    if rev is None:
        return None
    cipher = getattr(app.state, "secret_cipher", None)
    return redact_config(rev.payload, cipher)


async def revision_diff(
    app: Any, revision_id: str, *, against: str = "active"
) -> dict[str, Any] | None:
    """Structural diff between one revision and another (default: active).

    Both sides are redacted first, so a rotated secret shows as a marker
    change and never as ciphertext or plaintext.  ``None`` means either
    side could not be resolved.
    """
    from crewlet.secrets import redact_config

    store = getattr(app.state, "company_config_store", None)
    if store is None:
        return None
    try:
        target = await store.get_revision(UUID(revision_id))
    except ValueError:
        return None
    if target is None:
        return None
    if against == "active":
        base = await store.get_active()
    else:
        try:
            base = await store.get_revision(UUID(against))
        except ValueError:
            return None
    if base is None:
        return None
    cipher = getattr(app.state, "secret_cipher", None)
    return {
        "from": str(base.revision_id),
        "to": str(target.revision_id),
        "changes": _structural_diff(
            redact_config(base.payload, cipher),
            redact_config(target.payload, cipher),
        ),
    }


async def get_config(request: Request) -> Response:
    """GET /config — return the active revision payload (JSON or YAML).

    Encrypted secrets are redacted to ``{"encrypted": true, "key_id": …}``
    markers — the HTTP read paths never emit ciphertext (nor plaintext).
    Use ``crewlet config export`` on the host for a round-trippable dump.
    """
    # Keep the store check on the request path: a missing store is a
    # broken deployment and must not be reported as "no revision yet",
    # which is what collapsing both onto a 404 would do.
    _store(request)
    payload = await config_document(request.app)
    if payload is None:
        return JSONResponse({"error": "no_active_revision"}, status_code=404)
    fmt = request.query_params.get("format", "json").lower()
    if fmt == "yaml":
        body = yaml.safe_dump(payload, sort_keys=False, default_flow_style=False)
        return Response(body, media_type="application/yaml")
    return JSONResponse(payload)


async def get_config_revisions(request: Request) -> JSONResponse:
    """GET /config/revisions — paginated history (newest first)."""
    store = _store(request)
    try:
        limit = int(request.query_params.get("limit", "50"))
        offset = int(request.query_params.get("offset", "0"))
    except ValueError:
        return JSONResponse({"error": "invalid_pagination"}, status_code=400)
    revs = await store.list_revisions(limit=limit, offset=offset)
    return JSONResponse([_serialize_revision_meta(r) for r in revs])


async def get_config_revision(request: Request) -> JSONResponse:
    """GET /config/revisions/{id} — return a single revision's payload."""
    store = _store(request)
    try:
        rev_id = UUID(request.path_params["id"])
    except ValueError:
        return JSONResponse({"error": "invalid_uuid"}, status_code=400)
    rev = await store.get_revision(rev_id)
    if rev is None:
        return JSONResponse({"error": "not_found"}, status_code=404)
    from crewlet.secrets import redact_config

    body = _serialize_revision_meta(rev)
    body["payload"] = redact_config(rev.payload, _cipher(request))
    return JSONResponse(body)


async def get_config_revision_diff(request: Request) -> JSONResponse:
    """GET /config/revisions/{id}/diff?against=<uuid|active>"""
    store = _store(request)
    try:
        target_id = UUID(request.path_params["id"])
    except ValueError:
        return JSONResponse({"error": "invalid_uuid"}, status_code=400)
    target = await store.get_revision(target_id)
    if target is None:
        return JSONResponse({"error": "not_found"}, status_code=404)

    against_param = request.query_params.get("against", "active")
    if against_param == "active":
        against = await store.get_active()
        if against is None:
            return JSONResponse({"error": "no_active_revision"}, status_code=404)
    else:
        try:
            against = await store.get_revision(UUID(against_param))
        except ValueError:
            return JSONResponse({"error": "invalid_against_uuid"}, status_code=400)
        if against is None:
            return JSONResponse({"error": "against_not_found"}, status_code=404)

    from crewlet.secrets import redact_config

    # Diff over redacted payloads: redact_config decrypts each side's
    # structure but masks every secret, so a rotated secret shows as a
    # marker change, never the ciphertext or the plaintext of either side
    # (the keyring is required to show any structure at all).
    cipher = _cipher(request)
    return JSONResponse(
        {
            "from": str(against.revision_id),
            "to": str(target.revision_id),
            "changes": _structural_diff(
                redact_config(against.payload, cipher),
                redact_config(target.payload, cipher),
            ),
        }
    )


async def put_config(request: Request) -> JSONResponse:
    """PUT /config — full-document replacement."""
    operator_id = getattr(request.state, "operator_id", "unknown")

    try:
        payload = await _parse_request_body(request)
    except (ValueError, json.JSONDecodeError, yaml.YAMLError) as exc:
        return JSONResponse(
            {"error": "invalid_body", "detail": str(exc)},
            status_code=400,
        )

    if_match = request.headers.get("if-match")
    # ``summary`` is required on full-doc PUT so revision history
    # stays readable.  Accept via ``X-Summary`` header or via
    # a ``_summary`` key in the payload (popped before validation
    # so it doesn't fail Pydantic's ``extra="forbid"``).  Reject
    # absent or whitespace-only with 400 to keep revision history
    # readable.
    summary = request.headers.get("x-summary") or payload.pop("_summary", "")
    summary = (summary or "").strip()
    if not summary:
        return JSONResponse(
            {
                "error": "summary_required",
                "hint": (
                    "Full-doc PUT /config requires an audit summary. "
                    "Pass via 'X-Summary' header or a top-level "
                    "'_summary' key in the body."
                ),
            },
            status_code=400,
        )

    dispatcher = RevisionDispatcher(
        _store(request), _event_queue(request), _cipher(request)
    )
    new_id, error = await dispatcher.write_full(
        payload,
        operator_id=operator_id,
        summary=summary,
        if_match=if_match,
    )

    if error == "revision_advanced":
        # ``write_full`` already returned the winning revision's id as
        # ``new_id`` when it detected the conflict — no need for a
        # third ``get_active()`` call here.
        return JSONResponse(
            {
                "error": "revision_advanced",
                "current_revision_id": (str(new_id) if new_id is not None else None),
                "your_base": if_match,
            },
            status_code=409,
        )
    if error == "no_active_revision_but_if_match_set":
        return JSONResponse(
            {"error": "if_match_must_be_none_when_unconfigured"},
            status_code=412,
        )
    if error and error.startswith("validation_error"):
        return JSONResponse(
            {"error": "validation_error", "detail": error[len("validation_error: ") :]},
            status_code=400,
        )

    return JSONResponse(
        {"revision_id": str(new_id)},
        status_code=201,
    )


async def revert_config_revision(request: Request) -> JSONResponse:
    """POST /config/revisions/{id}/revert — create a new active revision
    with the historical revision's payload."""
    operator_id = getattr(request.state, "operator_id", "unknown")

    try:
        target_id = UUID(request.path_params["id"])
    except ValueError:
        return JSONResponse({"error": "invalid_uuid"}, status_code=400)

    summary = request.headers.get("x-summary") or f"revert to {target_id}"
    dispatcher = RevisionDispatcher(
        _store(request), _event_queue(request), _cipher(request)
    )
    new_id, error = await dispatcher.revert(
        target_id, operator_id=operator_id, summary=summary
    )
    if error == "revision_not_found":
        return JSONResponse({"error": "not_found"}, status_code=404)
    if error == "revision_advanced":
        return JSONResponse(
            {
                "error": "revision_advanced",
                "hint": (
                    "A concurrent writer activated a new revision. "
                    "Re-read GET /config and retry the revert."
                ),
            },
            status_code=409,
        )
    if error and error.startswith("decrypt_failed"):
        return JSONResponse(
            {
                "error": "decrypt_failed",
                "detail": error[len("decrypt_failed: ") :],
                "hint": (
                    "The target revision is sealed under a key no longer in "
                    "the keyring. Restore the old key to Tier A "
                    "`secrets.keys` before reverting."
                ),
            },
            status_code=409,
        )
    return JSONResponse({"revision_id": str(new_id)}, status_code=201)


def build_config_routes() -> list[Route]:
    """Return the `/config/*` route list ready for `Mount(...)`."""
    return [
        Route("/config", get_config, methods=["GET"]),
        Route("/config", put_config, methods=["PUT"]),
        Route("/config/revisions", get_config_revisions, methods=["GET"]),
        Route(
            "/config/revisions/{id}",
            get_config_revision,
            methods=["GET"],
        ),
        Route(
            "/config/revisions/{id}/diff",
            get_config_revision_diff,
            methods=["GET"],
        ),
        Route(
            "/config/revisions/{id}/revert",
            revert_config_revision,
            methods=["POST"],
        ),
    ]
