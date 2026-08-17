"""Tests for the ``/config/*`` HTTP routes."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from datetime import UTC, datetime
from typing import Any
from unittest.mock import AsyncMock, MagicMock
from uuid import UUID, uuid4

import pytest
import yaml
from starlette.testclient import TestClient

from crewlet.config import ApiAuthConfig, ApiAuthTokenConfig, ApiConfig, BootstrapConfig
from crewlet.db.company_config import CompanyConfigRevision
from crewlet.events.types import ConfigRevisionActivated, Event
from tests.test_api.helpers import create_app


class _MockQueue:
    def __init__(self) -> None:
        self.published: list[tuple[str, Event]] = []

    async def publish(self, topic: str, event: Event) -> None:
        self.published.append((topic, event))

    async def subscribe(
        self,
        topic: str,
        group: str,
        handler: Callable[[Event], Awaitable[None]],
    ) -> None: ...

    async def subscribe_stream(
        self,
        topic_pattern: str,
        handler: Callable[[Event], Awaitable[None]],
    ) -> Callable[[], Awaitable[None]]:
        async def _unsubscribe() -> None: ...

        return _unsubscribe

    async def start(self) -> None: ...
    async def stop(self) -> None: ...


def _bootstrap() -> BootstrapConfig:
    return BootstrapConfig(
        api=ApiConfig(
            auth=ApiAuthConfig(tokens=[ApiAuthTokenConfig(id="t1", token="secret-1")])
        ),
    )


def _make_revision(
    payload: dict | None = None,
    *,
    is_active: bool = True,
    rev_id: UUID | None = None,
    parent: UUID | None = None,
) -> CompanyConfigRevision:
    return CompanyConfigRevision(
        revision_id=rev_id or uuid4(),
        parent_revision_id=parent,
        created_at=datetime.now(UTC),
        created_by="alice",
        source="api",
        summary="test",
        payload=payload or {"name": "Test"},
        is_active=is_active,
        activated_at=datetime.now(UTC) if is_active else None,
    )


@pytest.fixture
def queue() -> _MockQueue:
    return _MockQueue()


@pytest.fixture
def store() -> MagicMock:
    s = MagicMock()
    s.get_active = AsyncMock(return_value=None)
    s.list_revisions = AsyncMock(return_value=[])
    s.get_revision = AsyncMock(return_value=None)
    s.has_any = AsyncMock(return_value=False)
    s.insert_active = AsyncMock(return_value=uuid4())
    return s


@pytest.fixture
def client(queue: _MockQueue, store: MagicMock) -> TestClient:
    app = create_app(
        event_queue=queue,
        bootstrap=_bootstrap(),
        company_config_store=store,
    )
    return TestClient(app)


_AUTH = {"Authorization": "Bearer secret-1"}
# PUT /config requires a summary.  Most tests don't care what summary
# lands; we attach a
# default via this header dict so each test can override when needed.
_AUTH_PUT = {**_AUTH, "X-Summary": "test"}


# ── GET /config ─────────────────────────────────────────────────────


def test_get_config_unconfigured(client: TestClient) -> None:
    resp = client.get("/config", headers=_AUTH)
    assert resp.status_code == 404
    assert resp.json() == {"error": "no_active_revision"}


def test_get_config_returns_active_payload(
    client: TestClient, store: MagicMock
) -> None:
    payload = {"name": "Acme", "mission": "ship"}
    store.get_active.return_value = _make_revision(payload)
    resp = client.get("/config", headers=_AUTH)
    assert resp.status_code == 200
    assert resp.json() == payload


def test_get_config_yaml_format(client: TestClient, store: MagicMock) -> None:
    payload = {"name": "Acme", "policies": ["a", "b"]}
    store.get_active.return_value = _make_revision(payload)
    resp = client.get("/config?format=yaml", headers=_AUTH)
    assert resp.status_code == 200
    assert "yaml" in resp.headers["content-type"]
    parsed = yaml.safe_load(resp.text)
    assert parsed == payload


# ── GET /config/revisions ───────────────────────────────────────────


def test_get_revisions_empty(client: TestClient) -> None:
    resp = client.get("/config/revisions", headers=_AUTH)
    assert resp.status_code == 200
    assert resp.json() == []


def test_get_revisions_returns_metadata_only(
    client: TestClient, store: MagicMock
) -> None:
    revs = [_make_revision(is_active=True), _make_revision(is_active=False)]
    store.list_revisions.return_value = revs
    resp = client.get("/config/revisions", headers=_AUTH)
    assert resp.status_code == 200
    body = resp.json()
    assert len(body) == 2
    assert "revision_id" in body[0]
    # Payload is intentionally absent from list view.
    assert "payload" not in body[0]


# ── GET /config/revisions/{id} ──────────────────────────────────────


def test_get_revision_404_when_invalid_uuid(client: TestClient) -> None:
    resp = client.get("/config/revisions/not-a-uuid", headers=_AUTH)
    assert resp.status_code == 400


def test_get_revision_404_when_not_found(client: TestClient) -> None:
    resp = client.get(f"/config/revisions/{uuid4()}", headers=_AUTH)
    assert resp.status_code == 404


def test_get_revision_returns_payload(client: TestClient, store: MagicMock) -> None:
    payload = {"name": "Historical"}
    rev = _make_revision(payload)
    store.get_revision.return_value = rev
    resp = client.get(f"/config/revisions/{rev.revision_id}", headers=_AUTH)
    assert resp.status_code == 200
    body = resp.json()
    assert body["payload"] == payload
    assert body["revision_id"] == str(rev.revision_id)


# ── GET /config/revisions/{id}/diff ─────────────────────────────────


def test_diff_against_active(client: TestClient, store: MagicMock) -> None:
    active = _make_revision({"name": "Acme", "mission": "old"})
    target = _make_revision(
        {"name": "Acme", "mission": "new", "vision": "yes"},
        is_active=False,
    )
    store.get_active.return_value = active
    store.get_revision.return_value = target
    resp = client.get(f"/config/revisions/{target.revision_id}/diff", headers=_AUTH)
    assert resp.status_code == 200
    body = resp.json()
    paths = {c["path"] for c in body["changes"]}
    assert "/mission" in paths
    assert "/vision" in paths


def test_diff_against_explicit_uuid(client: TestClient, store: MagicMock) -> None:
    other = _make_revision({"name": "Acme"})
    target = _make_revision({"name": "Other"}, is_active=False)

    async def _get_rev(rev_id):
        return target if rev_id == target.revision_id else other

    store.get_revision = AsyncMock(side_effect=_get_rev)
    resp = client.get(
        f"/config/revisions/{target.revision_id}/diff?against={other.revision_id}",
        headers=_AUTH,
    )
    assert resp.status_code == 200
    body = resp.json()
    assert any(c["path"] == "/name" for c in body["changes"])


def test_diff_no_active_returns_404(client: TestClient, store: MagicMock) -> None:
    target = _make_revision({"name": "X"}, is_active=False)
    store.get_revision.return_value = target
    store.get_active.return_value = None
    resp = client.get(f"/config/revisions/{target.revision_id}/diff", headers=_AUTH)
    assert resp.status_code == 404


# ── PUT /config ─────────────────────────────────────────────────────


def test_put_config_unconfigured_creates_first_revision(
    client: TestClient, store: MagicMock, queue: _MockQueue
) -> None:
    resp = client.put(
        "/config",
        json={"name": "Acme AI", "mission": "ship"},
        headers=_AUTH_PUT,
    )
    assert resp.status_code == 201
    body = resp.json()
    assert "revision_id" in body
    store.insert_active.assert_called_once()
    insert_kwargs = store.insert_active.call_args.kwargs
    assert insert_kwargs["created_by"] == "t1"
    assert insert_kwargs["source"] == "api"
    assert insert_kwargs["parent_revision_id"] is None
    # Activation event was published.
    assert any(isinstance(e, ConfigRevisionActivated) for _, e in queue.published)


def test_put_config_validation_error_returns_400(
    client: TestClient,
) -> None:
    # Missing required `name` field.
    resp = client.put(
        "/config",
        json={"mission": "no name"},
        headers=_AUTH_PUT,
    )
    assert resp.status_code == 400
    assert resp.json()["error"] == "validation_error"


def test_put_config_yaml_body(client: TestClient, store: MagicMock) -> None:
    resp = client.put(
        "/config",
        content="name: Acme YAML\nmission: yamlship\n",
        headers={**_AUTH_PUT, "Content-Type": "application/yaml"},
    )
    assert resp.status_code == 201
    payload_arg = store.insert_active.call_args.args[0]
    assert payload_arg["name"] == "Acme YAML"


def test_put_config_invalid_body_returns_400(client: TestClient) -> None:
    resp = client.put(
        "/config",
        content="- a\n- b\n",
        headers={**_AUTH_PUT, "Content-Type": "application/yaml"},
    )
    assert resp.status_code == 400


def test_put_config_unsupported_content_type_returns_400(client: TestClient) -> None:
    """Bodies with an unsupported Content-Type get a clear 400 instead
    of a confusing JSON parse error.  Only ``application/yaml`` and
    ``application/json`` (or empty/missing header) are accepted."""
    resp = client.put(
        "/config",
        content="<config><name>Acme</name></config>",
        headers={**_AUTH_PUT, "Content-Type": "text/xml"},
    )
    assert resp.status_code == 400
    body = resp.json()
    assert body["error"] == "invalid_body"
    assert "text/xml" in body["detail"]


def test_put_config_no_content_type_defaults_to_json(
    client: TestClient, store: MagicMock
) -> None:
    """Missing Content-Type header is accepted as JSON (curl + small
    scripts often omit it)."""
    resp = client.put(
        "/config",
        content='{"name": "NoHeader"}',
        headers=_AUTH_PUT,
    )
    # No "Content-Type" override — TestClient may set a default; we
    # only assert the request succeeded.
    assert resp.status_code == 201
    assert store.insert_active.call_args.args[0]["name"] == "NoHeader"


def test_put_config_missing_summary_returns_400(client: TestClient) -> None:
    """Full-doc PUT without X-Summary header (or body _summary) is rejected."""
    resp = client.put(
        "/config",
        json={"name": "Acme"},
        headers=_AUTH,
    )
    assert resp.status_code == 400
    body = resp.json()
    assert body["error"] == "summary_required"


def test_put_config_summary_via_body_key(client: TestClient, store: MagicMock) -> None:
    """``_summary`` in the body works as an alternative to the header."""
    resp = client.put(
        "/config",
        json={"name": "Acme", "_summary": "via body"},
        headers=_AUTH,
    )
    assert resp.status_code == 201
    assert store.insert_active.call_args.kwargs["summary"] == "via body"


def test_put_config_whitespace_only_summary_rejected(
    client: TestClient,
) -> None:
    resp = client.put(
        "/config",
        json={"name": "Acme"},
        headers={**_AUTH, "X-Summary": "   "},
    )
    assert resp.status_code == 400


def test_put_config_if_match_mismatch_returns_409(
    client: TestClient, store: MagicMock
) -> None:
    existing = _make_revision({"name": "Old"})
    store.get_active.return_value = existing
    resp = client.put(
        "/config",
        json={"name": "New"},
        headers={**_AUTH_PUT, "If-Match": str(uuid4())},
    )
    assert resp.status_code == 409
    body = resp.json()
    assert body["current_revision_id"] == str(existing.revision_id)


def test_put_config_if_match_matches_succeeds(
    client: TestClient, store: MagicMock
) -> None:
    existing = _make_revision({"name": "Old"})
    store.get_active.return_value = existing
    resp = client.put(
        "/config",
        json={"name": "New"},
        headers={**_AUTH_PUT, "If-Match": str(existing.revision_id)},
    )
    assert resp.status_code == 201


def test_put_config_if_match_none_when_unconfigured(
    client: TestClient,
) -> None:
    # 'none' is the allowed sentinel value when no revision exists.
    resp = client.put(
        "/config",
        json={"name": "First"},
        headers={**_AUTH_PUT, "If-Match": "none"},
    )
    assert resp.status_code == 201


def test_put_config_if_match_set_when_unconfigured_returns_412(
    client: TestClient,
) -> None:
    resp = client.put(
        "/config",
        json={"name": "First"},
        headers={**_AUTH_PUT, "If-Match": str(uuid4())},
    )
    assert resp.status_code == 412


# ── POST /config/revisions/{id}/revert ──────────────────────────────


def test_revert_revision_not_found(client: TestClient) -> None:
    resp = client.post(f"/config/revisions/{uuid4()}/revert", headers=_AUTH)
    assert resp.status_code == 404


def test_revert_creates_new_revision_with_historical_payload(
    client: TestClient, store: MagicMock, queue: _MockQueue
) -> None:
    historical = _make_revision({"name": "OldShape"}, is_active=False)
    current = _make_revision({"name": "Current"})
    store.get_revision.return_value = historical
    store.get_active.return_value = current
    new_id = uuid4()
    store.insert_active.return_value = new_id

    resp = client.post(
        f"/config/revisions/{historical.revision_id}/revert", headers=_AUTH
    )
    assert resp.status_code == 201
    assert resp.json()["revision_id"] == str(new_id)
    payload_arg = store.insert_active.call_args.args[0]
    insert_kwargs = store.insert_active.call_args.kwargs
    assert payload_arg["name"] == "OldShape"
    assert insert_kwargs["source"] == "api.revert"
    assert insert_kwargs["parent_revision_id"] == current.revision_id
    assert any(isinstance(e, ConfigRevisionActivated) for _, e in queue.published)


def test_revert_to_dropped_key_revision_returns_409(
    queue: _MockQueue, store: MagicMock
) -> None:
    # The target revision is sealed under a key no longer in the keyring —
    # revert must return a clean 409, not a 500 from an uncaught decrypt.
    from crewlet.config import SecretKeyConfig, SecretsConfig
    from crewlet.secrets import KeyringCipher
    from crewlet.secrets.cipher import decode_key_material
    from crewlet.secrets.document import encrypt_document
    from crewlet.secrets.keygen import generate_key

    old_mat, new_mat = generate_key(), generate_key()
    old_cipher = KeyringCipher(
        keys={"old": decode_key_material(old_mat)}, active_key_id="old"
    )
    historical = _make_revision(
        encrypt_document({"name": "OldShape"}, old_cipher), is_active=False
    )
    store.get_revision.return_value = historical
    store.get_active.return_value = _make_revision({"name": "Current"})

    boot = _bootstrap()
    # Keyring holds only the NEW key; "old" was dropped.
    boot = boot.model_copy(
        update={
            "secrets": SecretsConfig(
                active_key_id="new",
                keys=[SecretKeyConfig(id="new", material=new_mat)],
            )
        }
    )
    app = create_app(event_queue=queue, bootstrap=boot, company_config_store=store)
    resp = TestClient(app).post(
        f"/config/revisions/{historical.revision_id}/revert", headers=_AUTH
    )
    assert resp.status_code == 409
    assert resp.json()["error"] == "decrypt_failed"
    store.insert_active.assert_not_called()


# ── Operator id attribution ─────────────────────────────────────────


def test_put_config_attributes_revision_to_token_id(
    client: TestClient, store: MagicMock
) -> None:
    """`created_by` on the new revision == the bearer token's `id`."""
    resp = client.put(
        "/config",
        json={"name": "Acme"},
        headers={"Authorization": "Bearer secret-1", "X-Summary": "attribution"},
    )
    assert resp.status_code == 201
    insert_kwargs = store.insert_active.call_args.kwargs
    assert insert_kwargs["created_by"] == "t1"


# ── /health configured flag ─────────────────────────────────────────


def test_health_reports_configured_flag(client: TestClient) -> None:
    """`configured` starts False when a Tier B store is wired but no
    revision has been primed yet (the canonical unconfigured state).

    ``status`` says so too.  It used to read ``"ok"`` here, which meant
    an engine with no active company revision -- one dropping every
    inbound webhook it receives -- was indistinguishable from a healthy
    idle one, except that all its screens were empty.  A boolean nobody
    renders is not a signal.

    The HTTP status stays 200 regardless: liveness is the status code,
    and an unconfigured process is very much alive.
    """
    resp = client.get("/health")
    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "unconfigured"
    assert body["configured"] is False


def test_health_flips_to_true_after_priming(
    queue: _MockQueue, store: MagicMock
) -> None:
    """``attach_config_refresh`` primes ``configured`` from the active
    revision; once primed, /health reports True."""
    import asyncio

    from crewlet.api.app import attach_config_refresh
    from tests.test_api.helpers import create_app

    store.get_active.return_value = _make_revision({"name": "Acme"})
    app = create_app(
        event_queue=queue,
        bootstrap=_bootstrap(),
        company_config_store=store,
    )
    asyncio.new_event_loop().run_until_complete(attach_config_refresh(app))
    resp = TestClient(app).get("/health")
    assert resp.json()["configured"] is True


# ── webhook unconfigured early-out ───────────────────────────────────


def test_jira_webhook_rejected_when_unconfigured(client: TestClient) -> None:
    """An unconfigured process answers 503 so the sender RETRIES.

    The old 200-and-drop told Jira the delivery had been accepted while
    discarding it — survivable when "unconfigured" meant the whole
    deployment, and silent unrecoverable loss the moment one process of
    several has simply not caught up yet.
    """
    resp = client.post(
        "/webhooks/jira",
        json={"webhookEvent": "jira:issue_created", "issue": {}},
    )
    assert resp.status_code == 503
    body = resp.json()
    assert body["status"] == "unavailable"
    assert body["reason"] == "unconfigured"
    assert resp.headers["retry-after"]


def test_webhook_passes_through_after_configured(
    queue: _MockQueue, store: MagicMock
) -> None:
    """Once primed, webhook routes process normally (no drop)."""
    import asyncio

    from crewlet.api.app import attach_config_refresh
    from tests.test_api.helpers import create_app

    store.get_active.return_value = _make_revision({"name": "Acme"})
    app = create_app(
        event_queue=queue,
        bootstrap=_bootstrap(),
        company_config_store=store,
    )
    asyncio.new_event_loop().run_until_complete(attach_config_refresh(app))
    resp = TestClient(app).post(
        "/webhooks/jira",
        json={"webhookEvent": "jira:issue_created", "issue": {}},
    )
    assert resp.status_code == 200
    assert resp.json().get("status") != "dropped"


# ── attach_config_refresh idempotency ───────────────────────────────


def test_attach_config_refresh_is_idempotent(
    queue: _MockQueue, store: MagicMock
) -> None:
    """Calling ``attach_config_refresh`` twice doesn't double-subscribe.

    Both ``Engine._start_embedded_api`` and the standalone ``cmd_api``
    path may end up calling this on the same app (the embedded path
    when running the engine; tests that build an app and then attach
    refresh themselves).  Double-subscription would fire the handler
    twice on every revision activation."""
    import asyncio

    from crewlet.api.app import attach_config_refresh
    from tests.test_api.helpers import create_app

    store.get_active.return_value = None  # unconfigured
    subscribe_calls: list[str] = []

    async def _record_stream(topic_pattern: str, handler):
        subscribe_calls.append(topic_pattern)

        async def _unsubscribe() -> None: ...

        return _unsubscribe

    queue.subscribe_stream = _record_stream  # type: ignore[assignment]

    app = create_app(
        event_queue=queue,
        bootstrap=_bootstrap(),
        company_config_store=store,
    )
    loop = asyncio.new_event_loop()
    first_refresher = loop.run_until_complete(attach_config_refresh(app))
    first = list(subscribe_calls)
    again = loop.run_until_complete(attach_config_refresh(app))
    assert subscribe_calls == first  # no new subscriptions on 2nd call
    assert again is first_refresher


def test_config_refresh_uses_broadcast_not_a_consumer_group(
    queue: _MockQueue, store: MagicMock
) -> None:
    """The API's config subscriptions must be BROADCAST, never a group.

    They used to be durable competing-consumer subscriptions (group
    ``api-config``).  With more than one API process that meant exactly
    one of them ever refreshed its cached state, and the rest served the
    previous company — including its previous webhook signing secret —
    indefinitely.  A regression here is invisible in a single-process
    test run, so it is asserted directly.
    """
    import asyncio

    from crewlet.api.app import attach_config_refresh
    from tests.test_api.helpers import create_app

    store.get_active.return_value = None
    group_subscribes: list[str] = []
    stream_subscribes: list[str] = []

    async def _record_subscribe(topic: str, group: str, handler) -> None:
        group_subscribes.append(topic)

    async def _record_stream(topic_pattern: str, handler):
        stream_subscribes.append(topic_pattern)

        async def _unsubscribe() -> None: ...

        return _unsubscribe

    queue.subscribe = _record_subscribe  # type: ignore[assignment]
    queue.subscribe_stream = _record_stream  # type: ignore[assignment]

    app = create_app(
        event_queue=queue,
        bootstrap=_bootstrap(),
        company_config_store=store,
    )
    asyncio.new_event_loop().run_until_complete(attach_config_refresh(app))

    assert group_subscribes == []
    assert set(stream_subscribes) == {
        "crewlet.config.revision_activated",
        "crewlet.config.revision_applied",
    }


# ── TOCTOU concurrency conflict path ────────────────────────────────


def test_put_config_concurrency_conflict_returns_409(
    client: TestClient, store: MagicMock
) -> None:
    """A ``UniqueViolationError`` raised by ``insert_active`` (the
    losing writer in a TOCTOU race against ``company_config_one_active``)
    surfaces as a 409 ``revision_advanced``, not a 500."""
    # Simulate: get_active says unconfigured, but insert_active fails
    # because another writer just activated a revision.
    from asyncpg.exceptions import UniqueViolationError

    winning = _make_revision({"name": "WinnerWroteFirst"})
    # First call (in write_full): no active.  Second call (in
    # error handler): the winner now sits as active.
    store.get_active = AsyncMock(side_effect=[None, winning])
    store.insert_active = AsyncMock(
        side_effect=UniqueViolationError("duplicate active row")
    )
    resp = client.put(
        "/config",
        json={"name": "LoserWroteSecond"},
        headers=_AUTH_PUT,
    )
    assert resp.status_code == 409
    body = resp.json()
    assert body["error"] == "revision_advanced"
    assert body["current_revision_id"] == str(winning.revision_id)


def test_put_config_serialization_failure_also_returns_409(
    client: TestClient, store: MagicMock
) -> None:
    """Under SERIALIZABLE isolation the DB raises ``SerializationError``
    instead of ``UniqueViolationError`` for the same TOCTOU race.  Both
    map to ``409 revision_advanced``."""
    from asyncpg.exceptions import SerializationError

    winning = _make_revision({"name": "Winner"})
    store.get_active = AsyncMock(side_effect=[None, winning])
    store.insert_active = AsyncMock(
        side_effect=SerializationError("could not serialize access")
    )
    resp = client.put(
        "/config",
        json={"name": "Loser"},
        headers=_AUTH_PUT,
    )
    assert resp.status_code == 409
    assert resp.json()["error"] == "revision_advanced"


# ── configured flag flip-back after apply error ─────────────────────


def test_configured_flips_back_to_false_after_apply_error_unconfigured(
    queue: _MockQueue, store: MagicMock
) -> None:
    """When ``revision_applied(status=error)`` fires and the DB still has
    no active revision (i.e. the failed apply was the FIRST one), the
    API process flips ``configured`` back to False so ``/health`` is
    honest and the webhook unconfigured drop keeps firing."""
    import asyncio

    from crewlet.api.app import attach_config_refresh
    from crewlet.events.types import ConfigRevisionApplied
    from tests.test_api.helpers import create_app

    handlers: dict[str, Any] = {}

    async def _record_stream(topic_pattern: str, handler):
        handlers[topic_pattern] = handler

        async def _unsubscribe() -> None: ...

        return _unsubscribe

    queue.subscribe_stream = _record_stream  # type: ignore[assignment]

    # Initial state: configured (we primed it).
    store.get_active = AsyncMock(side_effect=[_make_revision({"name": "X"}), None])
    app = create_app(
        event_queue=queue,
        bootstrap=_bootstrap(),
        company_config_store=store,
    )
    loop = asyncio.new_event_loop()
    loop.run_until_complete(attach_config_refresh(app))
    assert app.state.configured is True

    # Now simulate engine reporting apply error AND DB shows no active
    # (first activation failed).
    applied_handler = handlers["crewlet.config.revision_applied"]
    loop.run_until_complete(
        applied_handler(
            "crewlet.config.revision_applied",
            ConfigRevisionApplied(
                source="engine",
                revision_id=str(uuid4()),
                status="error",
                applied_subsystems=[],
                error="something blew up",
            ),
        )
    )
    assert app.state.configured is False


def test_configured_stays_true_after_apply_error_when_db_still_has_active(
    queue: _MockQueue, store: MagicMock
) -> None:
    """When the failed apply was a 2nd-or-later one (the engine rolled
    back to a prior working state), ``configured`` stays True — the
    DB still has an active revision."""
    import asyncio

    from crewlet.api.app import attach_config_refresh
    from crewlet.events.types import ConfigRevisionApplied
    from tests.test_api.helpers import create_app

    handlers: dict[str, Any] = {}

    async def _record_stream(topic_pattern: str, handler):
        handlers[topic_pattern] = handler

        async def _unsubscribe() -> None: ...

        return _unsubscribe

    queue.subscribe_stream = _record_stream  # type: ignore[assignment]

    rev = _make_revision({"name": "Y"})
    store.get_active = AsyncMock(return_value=rev)
    app = create_app(
        event_queue=queue,
        bootstrap=_bootstrap(),
        company_config_store=store,
    )
    loop = asyncio.new_event_loop()
    loop.run_until_complete(attach_config_refresh(app))
    assert app.state.configured is True

    applied_handler = handlers["crewlet.config.revision_applied"]
    loop.run_until_complete(
        applied_handler(
            "crewlet.config.revision_applied",
            ConfigRevisionApplied(
                source="engine",
                revision_id=str(uuid4()),
                status="error",
                applied_subsystems=["budgets"],
                error="boom",
            ),
        )
    )
    assert app.state.configured is True


def test_activation_refreshes_app_state(queue: _MockQueue, store: MagicMock) -> None:
    """End-to-end: app starts unconfigured, an activation moves the
    pointer, and one reconcile tick refreshes the cached
    ``app.state.org_data`` / ``agent_roles`` / ``configured`` flag from
    the new revision's payload.

    This is the contract a real engine relies on when it PUTs a config
    and expects the API process's dashboard reads to reflect it."""
    import asyncio

    from crewlet.api.config_refresh import ConfigStateRefresher
    from crewlet.db.agents import derive_agent_id
    from crewlet.db.config_plane import MemoryConfigPlaneStore
    from tests.test_api.helpers import create_app

    # Boot unconfigured.
    store.get_active = AsyncMock(return_value=None)
    app = create_app(
        event_queue=queue,
        bootstrap=_bootstrap(),
        company_config_store=store,
    )
    plane = MemoryConfigPlaneStore()
    refresher = ConfigStateRefresher(app, store, plane)
    loop = asyncio.new_event_loop()
    assert loop.run_until_complete(refresher.refresh_if_changed()) is False
    assert app.state.configured is False
    assert app.state.agent_roles == []

    # Operator PUTs a config; the activation appends an epoch.
    activated_rev = _make_revision(
        {
            "name": "Acme",
            "mission": "ship things",
            "roles": [{"name": "Engineer", "handle": "engineer", "goal": "code"}],
        }
    )
    store.get_revision = AsyncMock(return_value=activated_rev)
    loop.run_until_complete(plane.record_activation(activated_rev.revision_id))
    assert loop.run_until_complete(refresher.refresh_if_changed()) is True

    # State refreshed end to end.
    assert app.state.configured is True
    assert app.state.org_data["name"] == "Acme"
    assert app.state.org_data["mission"] == "ship things"
    assert len(app.state.agent_roles) == 1
    role = app.state.agent_roles[0]
    assert role["handle"] == "engineer"
    # ID matches the engine's deterministic derive_agent_id — was a
    # silent bug before Commit F (api used NAMESPACE_DNS, engine used
    # AGENT_ID_NAMESPACE).
    assert role["id"] == str(derive_agent_id("Acme", "engineer"))

    # A second tick with the pointer unmoved is a no-op — the whole
    # point of keying on the epoch rather than re-deriving every tick.
    assert loop.run_until_complete(refresher.refresh_if_changed()) is False


def test_reactivating_the_same_revision_still_refreshes(
    queue: _MockQueue, store: MagicMock
) -> None:
    """Re-activating an UNCHANGED revision must move the pointer.

    That gesture is the documented way to make a running deployment pick
    up a rotated credential (``docs/concepts/secret-store.md``), and the
    webhook signing secrets on ``app.state`` are exactly the kind of
    value it rotates.  A pointer keyed on the revision id could never
    express it — hence an append-only epoch log.
    """
    import asyncio

    from crewlet.api.config_refresh import ConfigStateRefresher
    from crewlet.db.config_plane import MemoryConfigPlaneStore
    from tests.test_api.helpers import create_app

    rev = _make_revision({"name": "Acme"})
    store.get_active = AsyncMock(return_value=rev)
    store.get_revision = AsyncMock(return_value=rev)
    app = create_app(
        event_queue=queue,
        bootstrap=_bootstrap(),
        company_config_store=store,
    )
    plane = MemoryConfigPlaneStore()
    refresher = ConfigStateRefresher(app, store, plane)
    loop = asyncio.new_event_loop()

    loop.run_until_complete(plane.record_activation(rev.revision_id))
    assert loop.run_until_complete(refresher.refresh_if_changed()) is True

    # Same revision, activated again.
    loop.run_until_complete(plane.record_activation(rev.revision_id))
    assert loop.run_until_complete(refresher.refresh_if_changed()) is True
