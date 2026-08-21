"""GET /integrations — the external surfaces, and what has come through them.

Integrations had close to no surface at all. The dashboard branded an
event once it had already been accepted and routed, which means every
failure mode an operator actually hits was invisible: a Mattermost
SiteURL that blinds every browser while agents keep working, a revoked
bot token, a mis-pasted webhook secret. Rejected deliveries are
deliberately never written to the event store — verification runs before
the row is logged, which is correct — so a signature mismatch left no
trace anywhere except the provider's own delivery UI.

Three things are answerable today and are answered here:

* **how each integration is wired** — from the active config revision,
  including whether a signing secret is present (the boolean, never the
  value);
* **what has arrived** — per-source counts and a last-seen timestamp,
  grouped by the database rather than tallied in a browser that holds
  only the last 400 events;
* **what the routing did with it** — the funnel of ``external_notification``
  against ``notification_skipped`` and ``notifications_coalesced``.

One thing is deliberately NOT inferred: health. An idle Slack and a
401-ing Slack look identical in the event store, so silence is reported
as "no traffic seen", never as "healthy" and never as "down". That is
the same three-valued discipline the health surface keeps for engine
facts.
"""

from __future__ import annotations

from typing import Any

from starlette.requests import Request
from starlette.responses import JSONResponse

from crewlet._logging import get_logger

logger = get_logger("api.routes.integrations")

# The surfaces the engine can receive from, and how each one arrives.
# `secret` names the `app.state` attribute holding its verification
# secret, where one exists.
INBOUND: dict[str, dict[str, Any]] = {
    "slack": {
        "path": "/webhooks/slack/{handle}",
        "kind": "webhook",
        "secret": "slack_signing_secrets",
        "per_seat_secret": True,
    },
    "mattermost": {
        # No usable inbound webhook: outgoing webhooks fire only in
        # public channels and carry no thread id, channel type or
        # mentions, so the engine holds one websocket per seat instead.
        "path": "",
        "kind": "websocket",
        "secret": "",
    },
    "jira": {
        "path": "/webhooks/jira",
        "kind": "webhook",
        "secret": "jira_webhook_secret",
    },
    "confluence": {
        "path": "/webhooks/confluence",
        "kind": "webhook",
        "secret": "confluence_webhook_secret",
    },
    "github": {
        "path": "/webhooks/github",
        "kind": "webhook",
        "secret": "github_webhook_secret",
    },
    "gitlab": {
        "path": "/webhooks/gitlab",
        "kind": "webhook",
        "secret": "gitlab_signing_secret",
    },
    "plane": {
        "path": "/webhooks/plane",
        "kind": "webhook",
        "secret": "plane_webhook_secret",
    },
}

# How long a window the traffic figures cover.
TRAFFIC_WINDOW_HOURS = 24

# The routing outcomes, as event types.
_ROUTED = "external_notification"
_SKIPPED = "notification_skipped"
_COALESCED = "notifications_coalesced"


def _iso(value: Any) -> str:
    try:
        return value.isoformat() if value is not None else ""
    except Exception:
        return str(value or "")


def _secret_present(app: Any, spec: dict[str, Any]) -> bool | None:
    """Whether a verification secret is configured.

    Three-valued on purpose: ``None`` means this surface does not use
    one (Mattermost authenticates its websocket with the bot's own
    token), which is a different statement from "configured without a
    secret" — and the latter is a real, silent failure, because the
    webhook route answers 503 to every delivery until one is set.
    """
    attr = spec.get("secret") or ""
    if not attr:
        return None
    value = getattr(app.state, attr, None)
    if spec.get("per_seat_secret"):
        return bool(value)
    return bool(value)


def _configured(app: Any) -> dict[str, dict[str, Any]]:
    """Which integrations the active revision actually configures.

    Read from the redacted org payload the API already holds, so this
    cannot show a credential and cannot disagree with the Configuration
    room about what is set up.
    """
    integrations = getattr(app.state, "integrations_data", None) or {}
    return {key: block for key, block in integrations.items() if key in INBOUND}


def _seat_identities(app: Any) -> dict[str, list[str]]:
    """Per integration, the seats carrying credentials for it.

    Derived from the same org projection everything else reads, so a
    seat's handle here is the handle routing will look for.
    """
    org = getattr(app.state, "org_data", None) or {}
    found: dict[str, list[str]] = {key: [] for key in INBOUND}

    def visit(role: dict[str, Any]) -> None:
        handle = str(role.get("handle") or role.get("name") or "")
        if not handle:
            return
        for key in INBOUND:
            block = (role.get("integrations") or {}).get(key)
            env = (role.get("mcp_env") or {}).get(key)
            contact = role.get("contact") or {}
            has_contact = any(
                str(field).startswith(key) or key in str(field)
                for field in contact
                if contact.get(field)
            )
            if block or env or has_contact:
                found[key].append(handle)

    def walk_unit(unit: dict[str, Any]) -> None:
        for role in unit.get("roles") or []:
            visit(role)
        for child in unit.get("children") or []:
            walk_unit(child)

    for role in org.get("roles") or []:
        visit(role)
    for unit in org.get("units") or []:
        walk_unit(unit)
    return found


async def integrations_payload(app: Any) -> dict[str, Any]:
    """Every configured surface, how it is wired, and its recent traffic."""
    configured = _configured(app)
    identities = _seat_identities(app)

    counts: list[dict[str, Any]] = []
    store = getattr(app.state, "event_store", None)
    counter = getattr(store, "count_events_by_source", None) if store else None
    have_traffic = False
    if counter is not None:
        try:
            counts = await counter(since_hours=TRAFFIC_WINDOW_HOURS)
            have_traffic = True
        except Exception:
            logger.exception("integration_traffic_read_failed")

    # `source` is the bare integration key on a webhook row and
    # `notification_service.<key>` on a routing one.
    def _key_of(source: str) -> str:
        prefix = "notification_service."
        return source[len(prefix) :] if source.startswith(prefix) else source

    per_key: dict[str, dict[str, Any]] = {}
    for row in counts:
        key = _key_of(str(row.get("source") or ""))
        if key not in INBOUND:
            continue
        bucket = per_key.setdefault(
            key,
            {"inbound": 0, "routed": 0, "skipped": 0, "coalesced": 0, "last_at": ""},
        )
        event_type = str(row.get("event_type") or "")
        n = int(row.get("count") or 0)
        if str(row.get("category") or "") == "webhook":
            bucket["inbound"] += n
        if event_type == _ROUTED:
            bucket["routed"] += n
        elif event_type == _SKIPPED:
            bucket["skipped"] += n
        elif event_type == _COALESCED:
            bucket["coalesced"] += n
        last = _iso(row.get("last_at"))
        if last > bucket["last_at"]:
            bucket["last_at"] = last

    rows = []
    for key, spec in INBOUND.items():
        config = configured.get(key)
        traffic = per_key.get(key) or {}
        rows.append(
            {
                "key": key,
                "configured": config is not None,
                "enabled": bool(config and config["enabled"]),
                "url": (config or {}).get("url", ""),
                "workspace": (config or {}).get("workspace", ""),
                "inbound_kind": spec["kind"],
                "inbound_path": spec["path"],
                "secret_present": _secret_present(app, spec),
                "seats": sorted(identities.get(key) or []),
                "inbound": traffic.get("inbound", 0),
                "routed": traffic.get("routed", 0),
                "skipped": traffic.get("skipped", 0),
                "coalesced": traffic.get("coalesced", 0),
                "last_at": traffic.get("last_at", ""),
            }
        )

    return {
        # False means the counts below are not evidence about anything —
        # there is no event store to count over.
        "traffic_known": have_traffic,
        "window_hours": TRAFFIC_WINDOW_HOURS,
        "integrations": rows,
    }


async def get_integrations(request: Request) -> JSONResponse:
    """GET /integrations — the REST twin of the ``integrations`` query."""
    return JSONResponse(await integrations_payload(request.app))
