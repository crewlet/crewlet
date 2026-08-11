"""Slack app provisioning — programmatic one-app-per-agent setup.

Crewlet's Slack integration gives every agent its own Slack app (bot
token + signing secret + per-handle webhook).  This package automates
creating and maintaining those apps through Slack's App Manifest APIs
(``apps.manifest.create`` / ``apps.manifest.update``) so the only manual
steps left are generating one app *configuration token* (ever) and one
authorize click per agent (the OAuth install that mints the ``xoxb-``
bot token — Slack has no API to skip it).

Pieces:

- :mod:`crewlet.slack.manifest` — the canonical bot scopes / event
  subscriptions and the per-agent manifest builder (single source of
  truth for what a Crewlet agent's Slack app looks like).
- :mod:`crewlet.slack.api` — thin async client for the Slack endpoints
  provisioning needs (manifest CRUD, config-token rotation, OAuth code
  exchange).
- :mod:`crewlet.slack.state` — the local provisioning ledger
  (``slack-apps.json``): app ids + per-app credentials so re-runs
  update instead of duplicating.
- :mod:`crewlet.slack.envfile` — minimal ``.env`` upserts for the
  ``${VAR}`` names the company YAML references.
- :mod:`crewlet.slack.provision` — the orchestration: company YAML →
  per-agent plans → create/update apps → OAuth install → secrets
  written where ``crewlet run`` reads them.
- :mod:`crewlet.slack.provision_cli` — the ``crewlet slack provision``
  command.

See ``docs/integrations/slack.md`` for the operator flow.
"""

from crewlet.slack.api import SlackApiError, SlackManifestClient
from crewlet.slack.envfile import EnvStore
from crewlet.slack.manifest import (
    BOT_EVENTS,
    BOT_SCOPES,
    OAUTH_CALLBACK_PATH,
    build_agent_manifest,
    build_authorize_url,
    events_request_url,
    oauth_redirect_url,
)
from crewlet.slack.provision import (
    ProvisionOutcome,
    SlackAgentPlan,
    SlackProvisionError,
    build_agent_plans,
    run_provision,
)
from crewlet.slack.state import ProvisionState, SlackAppRecord, load_state, save_state

__all__ = [
    "BOT_EVENTS",
    "BOT_SCOPES",
    "OAUTH_CALLBACK_PATH",
    "EnvStore",
    "ProvisionOutcome",
    "ProvisionState",
    "SlackAgentPlan",
    "SlackApiError",
    "SlackAppRecord",
    "SlackManifestClient",
    "SlackProvisionError",
    "build_agent_manifest",
    "build_agent_plans",
    "build_authorize_url",
    "events_request_url",
    "load_state",
    "oauth_redirect_url",
    "run_provision",
    "save_state",
]
