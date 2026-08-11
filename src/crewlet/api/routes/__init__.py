"""REST + WebSocket route table for the Crewlet API.

Route handlers receive their dependencies (event queue, event store,
stream service, static config) via Starlette app state — no direct
Engine imports.  The handlers are split by domain into sibling modules;
this package assembles them into the route table and re-exports the few
helpers that callers/tests import by name.
"""

from __future__ import annotations

from starlette.routing import Route, WebSocketRoute

from crewlet.api.routes.agents import get_agent, get_agent_memory, list_agents
from crewlet.api.routes.dashboard import dashboard, root, static_file
from crewlet.api.routes.events import get_event, list_events, list_trace
from crewlet.api.routes.health import health
from crewlet.api.routes.org import get_org, get_schedules, list_tools
from crewlet.api.routes.stream import stream_snapshot, stream_websocket
from crewlet.api.routes.tokens import get_tokens_breakdown
from crewlet.api.routes.webhooks import (
    _log_event,
    confluence_webhook,
    forge_webhook,
    github_webhook,
    gitlab_webhook,
    jira_webhook,
    plane_webhook,
    sandbox_otel,
    slack_oauth_landing,
    slack_webhook,
)

__all__ = ["build_routes", "_log_event"]


def build_routes() -> list[Route | WebSocketRoute]:
    """Return the list of API routes (read, stream, dashboard, webhooks)."""
    return [
        Route("/", root, methods=["GET"]),
        Route("/dashboard", dashboard, methods=["GET"]),
        Route("/static/{path:path}", static_file, methods=["GET"]),
        Route("/health", health, methods=["GET"]),
        Route("/agents", list_agents, methods=["GET"]),
        Route("/agents/{id}", get_agent, methods=["GET"]),
        Route("/agents/{id}/memory", get_agent_memory, methods=["GET"]),
        Route("/org", get_org, methods=["GET"]),
        Route("/schedules", get_schedules, methods=["GET"]),
        Route("/tokens/breakdown", get_tokens_breakdown, methods=["GET"]),
        Route("/events", list_events, methods=["GET"]),
        Route("/events/trace/{trace_id}", list_trace, methods=["GET"]),
        Route("/events/{event_id}", get_event, methods=["GET"]),
        Route("/tools", list_tools, methods=["GET"]),
        Route("/stream/snapshot", stream_snapshot, methods=["GET"]),
        WebSocketRoute("/ws/stream", stream_websocket),
        Route("/webhooks/forge", forge_webhook, methods=["POST"]),
        Route("/webhooks/jira", jira_webhook, methods=["POST"]),
        Route("/webhooks/confluence", confluence_webhook, methods=["POST"]),
        Route("/webhooks/github", github_webhook, methods=["POST"]),
        Route("/webhooks/gitlab", gitlab_webhook, methods=["POST"]),
        Route("/webhooks/plane", plane_webhook, methods=["POST"]),
        Route("/otlp/{token}/v1/{signal}", sandbox_otel, methods=["POST"]),
        # Outside the /webhooks/slack/{handle} namespace on purpose: any
        # agent handle (including a literal "oauth") stays a valid
        # webhook path with no collision to reason about.
        Route("/webhooks/slack-oauth", slack_oauth_landing, methods=["GET"]),
        Route("/webhooks/slack/{handle}", slack_webhook, methods=["POST"]),
    ]
