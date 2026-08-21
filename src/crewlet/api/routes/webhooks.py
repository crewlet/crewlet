"""Inbound webhook endpoints (Jira/Slack/GitHub/GitLab/Plane/Confluence/Forge).

These are the API's external contract — webhook senders POST here on a
fixed retry schedule.  Each handler verifies the relevant signature,
persists the event for the dashboard, surfaces it on the live stream,
and republishes a ``raw_webhook`` event onto the EventQueue for the
engine's transport pipeline.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import html
import json
import re
from datetime import UTC, datetime
from typing import Any

from starlette.requests import Request
from starlette.responses import HTMLResponse, JSONResponse

from crewlet._logging import get_logger
from crewlet.events.types import Event

# Imported, not restated: three independent copies of this string is
# three chances for a publisher to write onto a topic nothing consumes.
from crewlet.notifications.service import INBOUND_TOPIC
from crewlet.telemetry import tracer

logger = get_logger("api.routes")

# ``Retry-After`` on the unconfigured 503. Matched to the control plane's
# reconcile cadence: a node that missed an activation event picks the
# revision up on its next poll, so telling a sender to come back sooner
# just burns deliveries against a node that cannot have converged yet.
WEBHOOK_UNCONFIGURED_RETRY_AFTER_SECONDS = 15

# ``Retry-After`` on the 503 a route answers when its HMAC secret is
# missing. Deliberately longer than the unconfigured one above: that
# case resolves itself on the next reconcile poll, this one waits on a
# human editing config, and a sender hammering every 15 s in the
# meantime buys nothing.
WEBHOOK_NO_SECRET_RETRY_AFTER_SECONDS = 300


def _no_secret_response(source: str) -> JSONResponse:
    """The answer when a webhook route has no secret to verify against.

    **5xx, never 4xx.** A 4xx says "your request is malformed, do not
    send it again" — and the request is fine. What is wrong is on this
    side, so a 4xx would make the sender discard a delivery nobody has
    any other copy of. That is the silent, unretried, unrecoverable loss
    ``_webhook_unconfigured_response`` was rewritten to stop; a missing
    secret has exactly the same shape as a missing config, and deserves
    exactly the same answer.

    503 rather than 500 for the same reason it is a 503 there: nothing
    crashed. This node cannot serve this delivery *yet*, which is what
    503 means and what ``Retry-After`` is for. The delivery waits at the
    provider and flows the moment somebody sets the secret — so a
    deployment that has not set one is stalled, not damaged.

    A signature that does not MATCH is the other case and stays 401:
    there the credential really was presented and really was wrong.
    """
    logger.error(
        "webhook_no_secret_configured",
        source=source,
        hint=(
            "this route verifies a provider HMAC and has no secret to "
            "verify against, so it cannot accept deliveries. Answering "
            "503 so the sender retries rather than discards them; set "
            "the integration's webhook_secret to clear it"
        ),
    )
    return JSONResponse(
        {"status": "unavailable", "reason": "no_webhook_secret"},
        status_code=503,
        headers={"Retry-After": str(WEBHOOK_NO_SECRET_RETRY_AFTER_SECONDS)},
    )


_SENSITIVE_HEADERS = frozenset({"authorization", "cookie"})


def _event_queue(request: Request) -> Any:
    return request.app.state.event_queue


def _parse_json_object(body_raw: bytes) -> dict[str, Any] | None:
    """Parse a webhook body, accepting only a JSON *object*.

    ``json.loads`` happily returns lists and scalars, and every handler
    immediately calls ``.get`` on the result — a correctly signed list
    body must produce a 400, not an ``AttributeError`` 500.
    """
    try:
        body = json.loads(body_raw)
    except (json.JSONDecodeError, ValueError):
        return None
    return body if isinstance(body, dict) else None


def _safe_headers(request: Request) -> dict[str, str]:
    """Return request headers with sensitive values redacted."""
    return {
        k: ("REDACTED" if k in _SENSITIVE_HEADERS else v)
        for k, v in request.headers.items()
    }


async def _log_event(
    request: Request,
    event_type: str,
    source: str,
    payload: dict[str, Any] | None = None,
    summary: str = "",
) -> None:
    """Persist a webhook event via the EventStore + surface it on the stream."""
    from uuid import uuid4

    from crewlet.telemetry import (
        current_parent_span_id,
        current_span_id,
        current_trace_id,
    )

    store = request.app.state.event_store

    trace_id = current_trace_id()
    span_id = current_span_id()
    parent_span_id = current_parent_span_id()

    enriched_payload = dict(payload or {})
    if trace_id:
        enriched_payload["trace_id"] = trace_id
    if span_id:
        enriched_payload["span_id"] = span_id
    if parent_span_id:
        enriched_payload["parent_span_id"] = parent_span_id

    event_id = str(uuid4())
    timestamp = datetime.now(UTC)
    final_summary = summary or f"Webhook from {source}: {event_type}"

    if store is not None:
        try:
            await store.write_event(
                event_id=event_id,
                event_type=event_type,
                source=source,
                timestamp=timestamp,
                category="webhook",
                payload=enriched_payload,
                summary=final_summary,
                trace_id=trace_id,
                span_id=span_id,
                parent_span_id=parent_span_id,
            )
        except Exception as exc:
            logger.exception(
                "event_store_write_failed", event_type=event_type, error=str(exc)
            )

    # Surface the webhook on the live stream so the dashboard's activity
    # feed reflects it in real time (the engine never publishes these on
    # ``crewlet.events.*``, so the stream service would otherwise miss
    # them).
    stream = getattr(request.app.state, "stream", None)
    if stream is not None:
        envelope_data = {
            "id": event_id,
            "type": event_type,
            "timestamp": timestamp.isoformat(),
            "source": source,
            "actor": source,
            "summary": final_summary,
            "category": "webhook",
            "trace_id": trace_id,
            "span_id": span_id,
            "parent_span_id": parent_span_id,
            "topic": f"crewlet.webhooks.{source}",
            "payload": enriched_payload,
        }
        try:
            await stream.emit_event(envelope_data)
        except Exception as exc:
            logger.exception(
                "stream_broadcast_failed", event_type=event_type, error=str(exc)
            )


def _webhook_unconfigured_response(
    request: Request, *, source: str, event_type: str = ""
) -> JSONResponse | None:
    """Return a 503 + WARNING log when this process has no active config.

    **Fail closed, not quiet.** This used to answer 200 to avoid provoking
    retries — which told the sender the delivery had been accepted when
    nothing had been done with it. That trade only made sense while a
    missing config meant the whole deployment was unconfigured. It stops
    being true the moment more than one process serves webhooks: a node
    that missed a config activation would silently discard real events
    that its peers were handling fine, and the sender, having been told
    "200", would never retry. Silent, unretried, unrecoverable loss.

    A 503 is the honest answer — nothing here can handle this delivery
    right now — and it is precisely what every provider's retry schedule
    exists for. Slack disabling an endpoint after sustained 5xx is the
    correct pressure toward fixing a genuinely-unconfigured deployment,
    not a reason to lie about the outcome.

    ``Retry-After`` keeps well-behaved senders from hot-looping while a
    node is still converging on its first revision.
    """
    if getattr(request.app.state, "configured", False):
        return None
    logger.warning(
        "webhook_rejected_unconfigured",
        source=source,
        event_type=event_type,
        remote=request.client.host if request.client else "",
    )
    return JSONResponse(
        {"status": "unavailable", "reason": "unconfigured"},
        status_code=503,
        headers={"Retry-After": str(WEBHOOK_UNCONFIGURED_RETRY_AFTER_SECONDS)},
    )


# ---------------------------------------------------------------------------
# Summary builders
# ---------------------------------------------------------------------------


def _build_slack_summary(handle: str, body: dict[str, Any]) -> str:
    """Build a human-readable summary for a Slack webhook."""
    event = body.get("event", {})
    event_type = event.get("type", body.get("type", ""))
    user = event.get("user", "")
    channel = event.get("channel", "")
    text = event.get("text", "")

    parts = [f"Slack → {handle}"]
    if event_type == "message":
        sender = user or "someone"
        if text:
            preview = text[:80] + ("..." if len(text) > 80 else "")
            parts.append(f'{sender} said "{preview}"')
        else:
            parts.append(f"{sender} sent a message")
        if channel:
            parts.append(f"in #{channel}")
    elif event_type == "app_mention":
        parts.append(f"mentioned by {user}" if user else "app mentioned")
    elif event_type == "reaction_added":
        reaction = event.get("reaction", "")
        parts.append(f":{reaction}: by {user}" if reaction else "reaction added")
    elif event_type:
        parts.append(event_type.replace("_", " "))

    return " ".join(parts)


def _build_jira_summary(body: dict[str, Any]) -> str:
    """Build a human-readable summary for a Jira webhook."""
    webhook_event = body.get("webhookEvent", "")
    issue = body.get("issue", {})
    issue_key = issue.get("key", "")
    summary = issue.get("fields", {}).get("summary", "")
    user = body.get("user", {}).get("displayName", "")

    action = webhook_event.replace("jira:", "").replace("_", " ")
    parts = ["Jira"]
    if user:
        parts.append(user)
    parts.append(action)
    if issue_key:
        parts.append(issue_key)
    if summary:
        preview = summary[:60] + ("..." if len(summary) > 60 else "")
        parts.append(f'"{preview}"')

    return " ".join(parts)


def _build_github_summary(gh_event: str, body: dict[str, Any]) -> str:
    """Build a human-readable summary for a GitHub webhook."""
    action = body.get("action", "")
    sender = body.get("sender", {}).get("login", "")
    repo = body.get("repository", {}).get("full_name", "")

    parts = ["GitHub"]
    if sender:
        parts.append(sender)

    if gh_event == "push":
        ref = body.get("ref", "").replace("refs/heads/", "")
        commits = body.get("commits", [])
        parts.append(f"pushed {len(commits)} commit(s) to {ref}")
    elif gh_event == "pull_request":
        pr = body.get("pull_request", {})
        title = pr.get("title", "")
        number = pr.get("number", "")
        parts.append(f"{action} PR #{number}")
        if title:
            parts.append(f'"{title[:60]}"')
    elif gh_event == "issues":
        issue = body.get("issue", {})
        title = issue.get("title", "")
        number = issue.get("number", "")
        parts.append(f"{action} issue #{number}")
        if title:
            parts.append(f'"{title[:60]}"')
    else:
        desc = f"{gh_event}"
        if action:
            desc += f" {action}"
        parts.append(desc)

    if repo:
        parts.append(f"on {repo}")

    return " ".join(parts)


def _build_gitlab_summary(gl_event: str, body: dict[str, Any]) -> str:
    """Build a human-readable summary for a GitLab webhook."""
    attrs = body.get("object_attributes", {}) or {}
    actor = (body.get("user", {}) or {}).get("username", "")
    project = body.get("project", {}) or {}
    path = project.get("path_with_namespace", "")
    kind = body.get("object_kind", gl_event or "event")
    action = attrs.get("action", "")

    parts = ["GitLab"]
    if actor:
        parts.append(actor)

    if kind == "merge_request":
        parts.append(f"{action or 'updated'} MR !{attrs.get('iid', '')}")
        title = attrs.get("title", "")
        if title:
            parts.append(f'"{title[:60]}"')
    elif kind == "issue":
        parts.append(f"{action or 'updated'} issue #{attrs.get('iid', '')}")
        title = attrs.get("title", "")
        if title:
            parts.append(f'"{title[:60]}"')
    elif kind == "note":
        parts.append(f"commented on {attrs.get('noteable_type', 'item')}")
    elif kind == "pipeline":
        parts.append(f"pipeline {attrs.get('status', '')}")
    else:
        parts.append(f"{kind} {action}".strip())

    if path:
        parts.append(f"on {path}")

    return " ".join(p for p in parts if p)


def _build_plane_summary(body: dict[str, Any]) -> str:
    """Build a human-readable summary for a Plane webhook."""
    event = body.get("event", "")
    action = body.get("action", "")
    data = body.get("data") or {}
    name = data.get("name", "") if isinstance(data, dict) else ""

    activity = body.get("activity") or {}
    actor = activity.get("actor") if isinstance(activity, dict) else None
    if isinstance(actor, dict):
        actor_name = (
            actor.get("display_name") or actor.get("first_name") or actor.get("id", "")
        )
    elif isinstance(actor, str):
        actor_name = actor
    else:
        actor_name = ""

    parts = ["Plane"]
    if actor_name:
        parts.append(str(actor_name))
    if event:
        parts.append(str(event))
    if action:
        parts.append(str(action))
    if name:
        preview = name[:60] + ("..." if len(name) > 60 else "")
        parts.append(f'"{preview}"')

    return " ".join(parts)


def _build_confluence_summary(body: dict[str, Any]) -> str:
    """Build a human-readable summary for a Confluence webhook."""
    event_type = (
        body.get("event") or body.get("webhookEvent") or body.get("eventType", "")
    )
    page = body.get("page") or body.get("content") or {}
    page_title = page.get("title", "")
    space = page.get("space") or body.get("space") or {}
    space_key = space.get("key", "")

    user_data = body.get("userAccountId") or body.get("user") or {}
    if isinstance(user_data, dict):
        user = user_data.get("displayName") or user_data.get("name", "")
    else:
        user = ""

    action = event_type.replace("_", " ")
    parts = ["Confluence"]
    if user:
        parts.append(user)
    parts.append(action)
    if space_key:
        parts.append(f"[{space_key}]")
    if page_title:
        preview = page_title[:60] + ("..." if len(page_title) > 60 else "")
        parts.append(f'"{preview}"')

    return " ".join(parts)


# ---------------------------------------------------------------------------
# Native webhook handlers
# ---------------------------------------------------------------------------


async def jira_webhook(request: Request) -> JSONResponse:
    """POST /webhooks/jira — verify then publish to EventQueue.

    Verified at the route for the same reason its Confluence twin is:
    this path is exempt from the API's bearer token because it
    authenticates by provider HMAC, so the check has to run before the
    delivery is recorded and published. The ``JiraTransport`` keeps its
    own check on the consume side as defence in depth.
    """
    body_raw = await request.body()

    refused = _atlassian_signature_failure(
        request, body_raw, source="jira", state_attr="jira_webhook_secret"
    )
    if refused is not None:
        return refused

    body_data = _parse_json_object(body_raw)
    if body_data is None:
        return JSONResponse({"error": "invalid JSON"}, status_code=400)

    dropped = _webhook_unconfigured_response(
        request, source="jira", event_type=body_data.get("webhookEvent", "")
    )
    if dropped is not None:
        return dropped

    with tracer.start_as_current_span(
        "webhook.jira", attributes={"webhook.source": "jira"}
    ):
        logger.info(
            "jira_webhook_received",
            webhook_event=body_data.get("webhookEvent", ""),
            issue_key=body_data.get("issue", {}).get("key", ""),
        )
        jira_summary = _build_jira_summary(body_data)
        await _log_event(
            request,
            f"webhook:{body_data.get('webhookEvent', 'unknown')}",
            "jira",
            payload=body_data,
            summary=jira_summary,
        )

        eq = _event_queue(request)
        await eq.publish(
            INBOUND_TOPIC,
            Event(
                type="raw_webhook",
                source="jira",
                payload={
                    "body": body_data,
                    "headers": _safe_headers(request),
                    "body_raw_b64": base64.b64encode(body_raw).decode(),
                },
            ),
        )
    return JSONResponse({"status": "ok"})


def _verify_slack_signature(
    body_raw: bytes,
    headers: dict[str, str],
    signing_secret: str,
) -> bool:
    """Verify Slack's ``x-slack-signature`` over the raw request body.

    Delegates to the transport's implementation so the edge check and the
    engine-side check can never diverge on the wire format or the replay
    window — two independent HMAC implementations for one signature is
    how one of them quietly stops matching.
    """
    from crewlet.notifications.transports.slack import SlackTransport

    return SlackTransport.verify_signature(body_raw, headers, signing_secret)


async def slack_webhook(request: Request) -> JSONResponse:
    """POST /webhooks/slack/{handle} — verify, then publish to EventQueue."""
    handle = request.path_params["handle"]
    body_raw = await request.body()
    body_data = _parse_json_object(body_raw)
    if body_data is None:
        return JSONResponse({"error": "invalid JSON"}, status_code=400)

    # Slack URL verification challenge — always answer (no engine needed).
    if body_data.get("type") == "url_verification":
        return JSONResponse({"challenge": body_data.get("challenge", "")})

    dropped = _webhook_unconfigured_response(
        request, source="slack", event_type=body_data.get("type", "")
    )
    if dropped is not None:
        return dropped

    # Verify BEFORE anything is persisted or broadcast.  The transport
    # re-verifies later (it is the component that must not act on an
    # unverified payload), but everything between here and there —
    # writing the event store row, fanning the payload out to every
    # connected dashboard websocket, enqueueing on Pulsar — happened
    # unconditionally, so an unauthenticated POST could pollute the
    # event log and inject content into the dashboard without ever
    # waking an agent.  github / gitlab / plane all 401 at the route;
    # Slack now does too.
    #
    # It is a MAP rather than a scalar because Slack's key is per-agent
    # (one app per seat), and the handle comes from the URL path.
    signing_secrets = getattr(request.app.state, "slack_signing_secrets", None)
    if signing_secrets:
        secret = signing_secrets.get(handle, "")
        if not secret:
            logger.warning("slack_webhook_unknown_handle", handle=handle)
            return JSONResponse({"error": "unknown handle"}, status_code=401)
        if not _verify_slack_signature(body_raw, dict(request.headers), secret):
            logger.warning("slack_webhook_signature_invalid", handle=handle)
            return JSONResponse({"error": "invalid signature"}, status_code=401)

    with tracer.start_as_current_span(
        "webhook.slack",
        attributes={"webhook.source": "slack", "webhook.handle": handle},
    ):
        logger.info(
            "slack_webhook_received",
            handle=handle,
            event_type=body_data.get("type", "unknown"),
        )
        slack_summary = _build_slack_summary(handle, body_data)
        await _log_event(
            request,
            f"webhook:{body_data.get('type', 'unknown')}",
            "slack",
            payload=body_data,
            summary=slack_summary,
        )

        eq = _event_queue(request)
        await eq.publish(
            INBOUND_TOPIC,
            Event(
                type="raw_webhook",
                source="slack",
                payload={
                    "body": body_data,
                    "handle": handle,
                    "headers": _safe_headers(request),
                    "body_raw_b64": base64.b64encode(body_raw).decode(),
                },
            ),
        )
    return JSONResponse({"ok": True})


_SLACK_OAUTH_PAGE = """<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Crewlet — Slack app install</title>
    <style>
      body {{ font-family: system-ui, sans-serif; max-width: 40rem;
             margin: 4rem auto; padding: 0 1rem; line-height: 1.5; }}
      code {{ background: rgba(127, 127, 127, .15); padding: .35rem .6rem;
             border-radius: .35rem; font-size: 1.05rem;
             word-break: break-all; display: inline-block; }}
      .muted {{ opacity: .7; font-size: .9rem; }}
    </style>
  </head>
  <body>
    <h1>Slack app install {heading}</h1>
    {body}
    <p class="muted">This page is served by the Crewlet API for
    <code>crewlet slack provision</code>. The code expires after 10
    minutes and is useless without the app's client secret.</p>
  </body>
</html>
"""


async def slack_oauth_landing(request: Request) -> HTMLResponse:
    """GET /webhooks/slack-oauth — OAuth install landing page.

    Every provisioned Slack app has this as its OAuth redirect URL.
    After the operator clicks Allow, Slack redirects here with a
    temporary ``code`` (and ``state`` carrying the agent handle); the
    page displays the code for pasting back into the waiting
    ``crewlet slack provision`` prompt.  No engine, auth, or queue
    involved — the code alone grants nothing without the client secret,
    which only the provisioning CLI holds.
    """
    error = request.query_params.get("error", "")
    code = request.query_params.get("code", "")
    handle = request.query_params.get("state", "")
    if error:
        body = (
            f"<p>Slack reported an error: <code>{html.escape(error)}</code></p>"
            "<p>Close this tab and re-run the install from the CLI.</p>"
        )
        return HTMLResponse(
            _SLACK_OAUTH_PAGE.format(heading="failed", body=body), status_code=400
        )
    if not code:
        body = (
            "<p>No <code>code</code> query parameter — open this page via the "
            "authorize URL printed by <code>crewlet slack provision</code>.</p>"
        )
        return HTMLResponse(
            _SLACK_OAUTH_PAGE.format(heading="", body=body), status_code=400
        )
    agent = f" for agent <strong>@{html.escape(handle)}</strong>" if handle else ""
    body = (
        f"<p>Approved{agent}. Paste this code into the waiting "
        "<code>crewlet slack provision</code> prompt:</p>"
        f"<p><code>{html.escape(code)}</code></p>"
    )
    logger.info("slack_oauth_code_displayed", handle=handle)
    return HTMLResponse(_SLACK_OAUTH_PAGE.format(heading="approved", body=body))


def _verify_github_signature(
    body_raw: bytes, secret: str, signature_header: str
) -> bool:
    """Verify an HMAC-SHA256 ``sha256=<hex>`` signature header."""
    if not signature_header.startswith("sha256="):
        return False
    expected = (
        "sha256="
        + hmac.new(secret.encode("utf-8"), body_raw, hashlib.sha256).hexdigest()
    )
    return hmac.compare_digest(expected, signature_header)


_OTLP_SIGNALS = frozenset({"traces", "metrics", "logs"})


async def sandbox_otel(request: Request) -> JSONResponse:
    """POST /otlp/{token}/v1/{signal} — engine-fronted OTLP receiver.

    The in-sandbox coding agent exports here (token in the path, no auth
    header). We validate the per-run token and forward the payload to the
    real backend with upstream auth added engine-side, so the backend
    credential never enters the sandbox.
    """
    receiver = getattr(request.app.state, "sandbox_otel_receiver", None)
    if receiver is None:
        return JSONResponse({"error": "otel receiver not configured"}, status_code=503)

    signal = request.path_params.get("signal", "")
    if signal not in _OTLP_SIGNALS:
        return JSONResponse({"error": "unknown signal"}, status_code=404)

    token = request.path_params.get("token", "")
    if receiver.tokens.validate(token) is None:
        logger.warning("sandbox_otel_token_invalid", signal=signal)
        return JSONResponse({"error": "invalid or expired token"}, status_code=401)

    body = await request.body()
    await receiver.forward(signal, body, request.headers.get("content-type", ""))
    return JSONResponse({"status": "ok"})


async def _claim_delivery(request: Request, source: str, key: str) -> bool:
    """Claim an inbound delivery, or report it as already handled.

    GitHub and GitLab had NO dedupe at all — not even the per-process
    ring the other sources kept — so every retry, and every redelivery an
    operator triggered from the provider UI, woke the agent again. Both
    send a stable delivery id, which is exactly the identity this needs.

    Fails open: a dedupe store that cannot be reached must not stop
    inbound work. A duplicate is recoverable noise; a dropped delivery is
    a message nobody ever answers.
    """
    store = getattr(request.app.state, "delivery_dedupe", None)
    if store is None or not key:
        return True
    try:
        return await store.claim(source, key)
    except Exception:
        logger.warning("delivery_dedupe_failed", source=source)
        return True


async def github_webhook(request: Request) -> JSONResponse:
    """POST /webhooks/github — publish to EventQueue."""
    body_raw = await request.body()

    webhook_secret: str | None = getattr(
        request.app.state, "github_webhook_secret", None
    )
    if not webhook_secret:
        return _no_secret_response("github")
    signature = request.headers.get("x-hub-signature-256", "")
    if not signature or not _verify_github_signature(
        body_raw, webhook_secret, signature
    ):
        logger.warning("github_webhook_signature_invalid")
        return JSONResponse({"error": "invalid signature"}, status_code=401)

    body_data = _parse_json_object(body_raw)
    if body_data is None:
        return JSONResponse({"error": "invalid JSON"}, status_code=400)

    gh_event = request.headers.get("x-github-event", "unknown")

    dropped = _webhook_unconfigured_response(
        request, source="github", event_type=gh_event
    )
    if dropped is not None:
        return dropped

    with tracer.start_as_current_span(
        "webhook.github",
        attributes={"webhook.source": "github", "github.event": gh_event},
    ):
        delivery_id = request.headers.get("x-github-delivery", "")
        if not await _claim_delivery(request, "github", delivery_id):
            logger.debug("github_delivery_duplicate", delivery_id=delivery_id)
            return JSONResponse({"status": "duplicate"})

        logger.info("github_webhook_received", github_event=gh_event)
        github_summary = _build_github_summary(gh_event, body_data)
        await _log_event(
            request,
            f"webhook:{gh_event}",
            "github",
            payload=body_data,
            summary=github_summary,
        )

        eq = _event_queue(request)
        await eq.publish(
            INBOUND_TOPIC,
            Event(
                type="raw_webhook",
                source="github",
                payload={
                    "body": body_data,
                    "headers": _safe_headers(request),
                    "body_raw_b64": base64.b64encode(body_raw).decode(),
                },
            ),
        )
    return JSONResponse({"status": "ok"})


_GITLAB_SIGNATURE_TOLERANCE_SECONDS = 300


def _verify_gitlab_signature(
    body_raw: bytes,
    signing_secret: str,
    webhook_id: str,
    webhook_timestamp: str,
    signature_header: str,
) -> bool:
    """Verify a GitLab 19.1+ Standard-Webhooks signature.

    GitLab signs ``{webhook-id}.{webhook-timestamp}.{body}`` with
    HMAC-SHA256 keyed on the base64 payload of a ``whsec_…`` secret and
    sends the base64 signature(s) in the ``webhook-signature`` header as
    space-separated ``v1,<sig>`` tokens. The timestamp is checked against
    a ±5 min window to bound replay.
    """
    if not (webhook_id and webhook_timestamp and signature_header):
        return False
    try:
        ts = int(webhook_timestamp)
    except (TypeError, ValueError):
        return False
    now = int(datetime.now(UTC).timestamp())
    if abs(now - ts) > _GITLAB_SIGNATURE_TOLERANCE_SECONDS:
        logger.warning("gitlab_webhook_timestamp_out_of_window", timestamp=ts)
        return False

    key = signing_secret
    if key.startswith("whsec_"):
        try:
            key_bytes = base64.b64decode(key[len("whsec_") :])
        except (ValueError, TypeError):
            key_bytes = key.encode("utf-8")
    else:
        key_bytes = key.encode("utf-8")

    signed = f"{webhook_id}.{webhook_timestamp}.".encode() + body_raw
    expected = base64.b64encode(
        hmac.new(key_bytes, signed, hashlib.sha256).digest()
    ).decode()
    for token in signature_header.split():
        _, _, sig = token.partition(",")
        if sig and hmac.compare_digest(sig, expected):
            return True
    return False


async def gitlab_webhook(request: Request) -> JSONResponse:
    """POST /webhooks/gitlab — verify then publish to EventQueue.

    Verification is the GitLab 19.1+ signing token only: the
    ``webhook-signature`` HMAC over ``{webhook-id}.{webhook-timestamp}.
    {body}`` must match the configured ``signing_secret``.
    """
    body_raw = await request.body()

    signing_secret: str | None = getattr(
        request.app.state, "gitlab_signing_secret", None
    )
    if not signing_secret:
        return _no_secret_response("gitlab")

    verified = _verify_gitlab_signature(
        body_raw,
        signing_secret,
        request.headers.get("webhook-id", ""),
        request.headers.get("webhook-timestamp", ""),
        request.headers.get("webhook-signature", ""),
    )
    if not verified:
        logger.warning("gitlab_webhook_verification_failed")
        return JSONResponse({"error": "invalid signature"}, status_code=401)

    body_data = _parse_json_object(body_raw)
    if body_data is None:
        return JSONResponse({"error": "invalid JSON"}, status_code=400)

    gl_event = request.headers.get("x-gitlab-event", "unknown")

    dropped = _webhook_unconfigured_response(
        request, source="gitlab", event_type=gl_event
    )
    if dropped is not None:
        return dropped

    with tracer.start_as_current_span(
        "webhook.gitlab",
        attributes={"webhook.source": "gitlab", "gitlab.event": gl_event},
    ):
        # GitLab 19.1+ Standard-Webhooks sends `webhook-id`; older
        # deliveries carry `X-Gitlab-Event-UUID`. Either is a stable
        # per-delivery identity.
        delivery_id = request.headers.get(
            "x-gitlab-event-uuid", ""
        ) or request.headers.get("webhook-id", "")
        if not await _claim_delivery(request, "gitlab", delivery_id):
            logger.debug("gitlab_delivery_duplicate", delivery_id=delivery_id)
            return JSONResponse({"status": "duplicate"})

        logger.info("gitlab_webhook_received", gitlab_event=gl_event)
        gitlab_summary = _build_gitlab_summary(gl_event, body_data)
        await _log_event(
            request,
            f"webhook:{gl_event}",
            "gitlab",
            payload=body_data,
            summary=gitlab_summary,
        )

        eq = _event_queue(request)
        await eq.publish(
            INBOUND_TOPIC,
            Event(
                type="raw_webhook",
                source="gitlab",
                payload={
                    "body": body_data,
                    "headers": _safe_headers(request),
                    "body_raw_b64": base64.b64encode(body_raw).decode(),
                },
            ),
        )
    return JSONResponse({"status": "ok"})


# Plane's scheme is a fixed 64-char SHA-256 hexdigest — anything else
# is a forgery by shape.
_PLANE_SIGNATURE_RE = re.compile(r"[0-9a-fA-F]{64}")


def _verify_plane_signature(
    body_raw: bytes, webhook_secret: str, signature: str
) -> bool:
    """X-Plane-Signature = HMAC-SHA256 hexdigest of the raw body keyed
    with the webhook secret (CE ``bgtasks/webhook_task.py`` scheme).

    The shape prefilter keeps the function total: ``hmac.compare_digest``
    raises ``TypeError`` on non-ASCII ``str`` operands, and Starlette
    decodes raw header bytes with latin-1 — without the prefilter a
    single ``0xFF`` byte in the header would turn an unauthenticated
    request into a 500 instead of a 401.
    """
    if _PLANE_SIGNATURE_RE.fullmatch(signature) is None:
        return False
    expected = hmac.new(
        webhook_secret.encode("utf-8"), body_raw, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)


async def plane_webhook(request: Request) -> JSONResponse:
    """POST /webhooks/plane — verify then publish to EventQueue.

    Verification is the CE webhook scheme: the ``X-Plane-Signature``
    header carries the HMAC-SHA256 hexdigest of the raw body keyed with
    the configured webhook secret.  The unconfigured drop runs AFTER
    verification so forgeries never earn a 200 while Plane's five-retry
    auto-disable counter is still protected for genuine deliveries.
    """
    body_raw = await request.body()

    webhook_secret: str | None = getattr(
        request.app.state, "plane_webhook_secret", None
    )
    if not webhook_secret:
        return _no_secret_response("plane")
    signature = request.headers.get("x-plane-signature", "")
    if not signature or not _verify_plane_signature(
        body_raw, webhook_secret, signature
    ):
        logger.warning("plane_webhook_signature_invalid")
        return JSONResponse({"error": "invalid signature"}, status_code=401)

    body_data = _parse_json_object(body_raw)
    if body_data is None:
        return JSONResponse({"error": "invalid JSON"}, status_code=400)

    pl_event = request.headers.get("x-plane-event", "") or body_data.get(
        "event", "unknown"
    )

    dropped = _webhook_unconfigured_response(
        request, source="plane", event_type=str(pl_event)
    )
    if dropped is not None:
        return dropped

    with tracer.start_as_current_span(
        "webhook.plane",
        attributes={"webhook.source": "plane", "plane.event": pl_event},
    ):
        logger.info("plane_webhook_received", plane_event=pl_event)
        plane_summary = _build_plane_summary(body_data)
        # The dashboard event type carries the action so create / update /
        # delete stay distinguishable in the feed filter.
        action = body_data.get("action", "")
        event_label = (
            f"webhook:{pl_event}.{action}" if action else f"webhook:{pl_event}"
        )
        await _log_event(
            request,
            event_label,
            "plane",
            payload=body_data,
            summary=plane_summary,
        )

        eq = _event_queue(request)
        await eq.publish(
            INBOUND_TOPIC,
            Event(
                type="raw_webhook",
                source="plane",
                payload={
                    "body": body_data,
                    "headers": _safe_headers(request),
                    "body_raw_b64": base64.b64encode(body_raw).decode(),
                },
            ),
        )
    return JSONResponse({"status": "ok"})


# ``sha256=`` + 64 hex, the ``X-Hub-Signature`` shape Atlassian Data
# Center uses for both Jira and Confluence. Same prefilter rationale as
# ``_PLANE_SIGNATURE_RE``: keeps the compare total, so a header the
# client made up cannot turn a 401 into a 500.
_ATLASSIAN_SIGNATURE_RE = re.compile(r"sha256=[0-9a-fA-F]{64}")


def _verify_atlassian_signature(
    body_raw: bytes, webhook_secret: str, signature: str
) -> bool:
    """``X-Hub-Signature`` = ``sha256=`` + HMAC-SHA256 of the raw body.

    One implementation for Jira and Confluence, because it is one
    scheme. Two copies of a signature check is how they come to disagree
    — the same reasoning ``_verify_slack_signature`` gives for delegating
    to the transport rather than reimplementing Slack's.
    """
    if _ATLASSIAN_SIGNATURE_RE.fullmatch(signature) is None:
        return False
    expected = (
        "sha256="
        + hmac.new(webhook_secret.encode("utf-8"), body_raw, hashlib.sha256).hexdigest()
    )
    return hmac.compare_digest(expected, signature)


def _atlassian_signature_failure(
    request: Request, body_raw: bytes, *, source: str, state_attr: str
) -> JSONResponse | None:
    """The shared route-level gate, or ``None`` when the request passes.

    Both Atlassian routes sit in the API's unguarded set, exempt from the
    bearer token on the grounds that they authenticate by provider HMAC —
    so this runs before the delivery is recorded or published, which is
    where GitHub, GitLab and Plane put theirs.
    """
    secret: str | None = getattr(request.app.state, state_attr, None)
    if not secret:
        return _no_secret_response(source)
    signature = request.headers.get("x-hub-signature", "")
    if not signature or not _verify_atlassian_signature(body_raw, secret, signature):
        logger.warning("webhook_signature_invalid", source=source)
        return JSONResponse({"error": "invalid signature"}, status_code=401)
    return None


async def confluence_webhook(request: Request) -> JSONResponse:
    """POST /webhooks/confluence — verify then publish to EventQueue.

    Verified HERE, at the route, like GitHub / GitLab / Plane. The
    ``ConfluenceTransport`` still checks the signature on the consume
    side, but that is defence in depth rather than the gate: this route
    is exempt from bearer auth precisely BECAUSE it authenticates by
    provider HMAC, so the check has to run before the delivery is
    recorded and published, not after.

    Refuses when no secret is configured, again like its peers — an
    unconfigured verifier that answers "valid" is not a verifier. Cloud
    is unaffected: those events arrive through the Forge app on
    ``/webhooks/forge`` with its own JWT, never here.
    """
    body_raw = await request.body()

    refused = _atlassian_signature_failure(
        request, body_raw, source="confluence", state_attr="confluence_webhook_secret"
    )
    if refused is not None:
        return refused

    body_data = _parse_json_object(body_raw)
    if body_data is None:
        return JSONResponse({"error": "invalid JSON"}, status_code=400)

    event_type = (
        body_data.get("event")
        or body_data.get("webhookEvent")
        or body_data.get("eventType", "unknown")
    )

    dropped = _webhook_unconfigured_response(
        request, source="confluence", event_type=str(event_type)
    )
    if dropped is not None:
        return dropped

    with tracer.start_as_current_span(
        "webhook.confluence", attributes={"webhook.source": "confluence"}
    ):
        logger.info(
            "confluence_webhook_received",
            webhook_event=event_type,
            page_title=body_data.get("page", {}).get("title", ""),
        )
        confluence_summary = _build_confluence_summary(body_data)
        await _log_event(
            request,
            f"webhook:{event_type}",
            "confluence",
            payload=body_data,
            summary=confluence_summary,
        )

        eq = _event_queue(request)
        await eq.publish(
            INBOUND_TOPIC,
            Event(
                type="raw_webhook",
                source="confluence",
                payload={
                    "body": body_data,
                    "headers": _safe_headers(request),
                    "body_raw_b64": base64.b64encode(body_raw).decode(),
                },
            ),
        )
    return JSONResponse({"status": "ok"})


# ---------------------------------------------------------------------------
# Forge Remote webhook (Cloud)
# ---------------------------------------------------------------------------

# Map Forge event names → (source, legacy_event_type).
_FORGE_EVENT_MAP: dict[str, tuple[str, str]] = {
    "avi:jira:created:issue": ("jira", "jira:issue_created"),
    "avi:jira:updated:issue": ("jira", "jira:issue_updated"),
    "avi:jira:deleted:issue": ("jira", "jira:issue_deleted"),
    "avi:jira:commented:issue": ("jira", "comment_created"),
    "avi:jira:deleted:comment": ("jira", "comment_deleted"),
    "avi:confluence:created:page": ("confluence", "page_created"),
    "avi:confluence:updated:page": ("confluence", "page_updated"),
    "avi:confluence:trashed:page": ("confluence", "page_trashed"),
    "avi:confluence:deleted:page": ("confluence", "page_removed"),
    "avi:confluence:created:comment": ("confluence", "comment_created"),
    "avi:confluence:updated:comment": ("confluence", "comment_updated"),
    "avi:confluence:created:blogpost": ("confluence", "blog_created"),
    "avi:confluence:updated:blogpost": ("confluence", "blog_updated"),
}


def _transform_forge_payload(
    forge_event: str, raw_payload: dict[str, Any]
) -> dict[str, Any]:
    """Transform a Forge event payload into the format our transports expect.

    Confluence event data lives in ``content``; Jira event data lives at
    the top level.  We normalize into the body format the existing native
    transports parse.
    """
    source, legacy_event = _FORGE_EVENT_MAP.get(forge_event, ("", ""))
    atlassian_id = raw_payload.get("atlassianId", "")

    if source == "confluence":
        content = raw_payload.get("content", {})
        body: dict[str, Any] = {}
        content_type = content.get("type", "")
        if content_type == "comment":
            body["comment"] = content
            container = content.get("container", {})
            if container:
                body["page"] = container
            space = content.get("space") or container.get("space")
            if space and "page" in body and "space" not in body["page"]:
                body["page"]["space"] = space
        else:
            body["page"] = content
        body["event"] = legacy_event
    elif source == "jira":
        body = {
            k: v
            for k, v in raw_payload.items()
            if k
            not in (
                "eventType",
                "atlassianId",
                "selfGenerated",
                "suppressNotifications",
                "encryptedData",
                "permissions",
                "eventCreatedDate",
            )
        }
        body["webhookEvent"] = legacy_event
    else:
        body = {}

    if "userAccountId" not in body and atlassian_id:
        body["userAccountId"] = atlassian_id
    if source == "jira" and atlassian_id:
        if not isinstance(body.get("user"), dict):
            body["user"] = {"accountId": atlassian_id}
        else:
            body["user"].setdefault("accountId", atlassian_id)
        comment = body.get("comment")
        if isinstance(comment, dict):
            author = comment.get("author")
            if not isinstance(author, dict):
                comment["author"] = {"accountId": atlassian_id}
            else:
                author.setdefault("accountId", atlassian_id)

    if "timestamp" not in body:
        event_date = raw_payload.get("eventCreatedDate", "")
        if event_date:
            try:
                dt = datetime.fromisoformat(event_date.replace("Z", "+00:00"))
                body["timestamp"] = int(dt.timestamp() * 1000)
            except (ValueError, AttributeError):
                pass
        if "timestamp" not in body:
            body["timestamp"] = int(datetime.now(UTC).timestamp() * 1000)

    return body


def _build_forge_summary(source: str, legacy_event: str, body: dict[str, Any]) -> str:
    """Build a human-readable summary for a Forge webhook event."""
    parts = ["Forge"]
    if source == "confluence":
        page = body.get("page", {})
        comment = body.get("comment", {})
        space_key = page.get("space", {}).get("key", "")
        title = page.get("title", "")
        if space_key:
            parts.append(f"[{space_key}]")
        parts.append(legacy_event.replace("_", " "))
        if comment:
            parts.append(f'on "{title[:50]}"' if title else "")
        elif title:
            parts.append(f'"{title[:50]}"')
    elif source == "jira":
        issue = body.get("issue", {})
        issue_key = issue.get("key", "")
        summary = issue.get("fields", {}).get("summary", "")
        parts.append(legacy_event.replace("jira:", "").replace("_", " "))
        if issue_key:
            parts.append(issue_key)
        if summary:
            parts.append(f'"{summary[:50]}"')
    else:
        parts.append(legacy_event)
    return " ".join(p for p in parts if p)


async def forge_webhook(request: Request) -> JSONResponse:
    """POST /webhooks/forge — receive FIT-verified events from the Forge app."""
    # Read the body before FIT verification: verify_fit can block on a
    # network JWKS fetch while the sender's delivery deadline runs out,
    # and an aborted sender surfaces as ClientDisconnect at the first
    # body read — drain the socket while the sender is still connected.
    body_raw = await request.body()

    forge_app_id = getattr(request.app.state, "forge_app_id", "")
    if not forge_app_id:
        # Same class as a missing HMAC secret: the app id IS the JWT
        # audience this route verifies against, so without it there is
        # nothing to check and the delivery must be held, not discarded.
        return _no_secret_response("forge")
    auth = request.headers.get("authorization", "")
    if not auth.startswith("Bearer "):
        return JSONResponse({"error": "unauthorized"}, status_code=401)

    from crewlet.api.forge_fit import verify_fit

    try:
        fit_payload = await verify_fit(auth[7:], forge_app_id)
    except RuntimeError as exc:
        return JSONResponse({"error": str(exc)}, status_code=500)
    if fit_payload is None:
        return JSONResponse({"error": "unauthorized"}, status_code=401)

    body_data = _parse_json_object(body_raw)
    if body_data is None:
        return JSONResponse({"error": "invalid JSON"}, status_code=400)

    forge_event = body_data.get("eventType", "")
    atlassian_id = body_data.get("atlassianId", "")
    self_generated = body_data.get("selfGenerated", False)

    if self_generated:
        logger.debug("forge_self_generated_skipped", forge_event=forge_event)
        return JSONResponse({"status": "ignored", "reason": "selfGenerated"})

    dropped = _webhook_unconfigured_response(
        request, source="forge", event_type=forge_event
    )
    if dropped is not None:
        return dropped

    mapping = _FORGE_EVENT_MAP.get(forge_event)
    if not mapping:
        logger.warning(
            "forge_unknown_event",
            forge_event=forge_event,
            payload_keys=list(body_data.keys()),
        )
        return JSONResponse({"status": "ignored", "event": forge_event})

    source, legacy_event = mapping

    with tracer.start_as_current_span(
        "webhook.forge",
        attributes={
            "webhook.source": source,
            "forge.event": forge_event,
            "forge.atlassian_id": atlassian_id,
        },
    ):
        logger.info(
            "forge_webhook_received",
            forge_event=forge_event,
            source=source,
            atlassian_id=atlassian_id,
        )

        transformed = _transform_forge_payload(forge_event, body_data)
        summary = _build_forge_summary(source, legacy_event, transformed)
        await _log_event(
            request,
            f"forge:{forge_event}",
            source,
            payload=body_data,
            summary=summary,
        )

        eq = _event_queue(request)
        await eq.publish(
            INBOUND_TOPIC,
            Event(
                type="raw_webhook",
                source=source,
                payload={
                    "body": transformed,
                    "headers": _safe_headers(request),
                    "body_raw_b64": base64.b64encode(body_raw).decode(),
                    "forge_atlassian_id": atlassian_id,
                },
            ),
        )
    return JSONResponse({"status": "ok"})
