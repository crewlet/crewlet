# Jira Integration

> **v1 status — `integrations.jira` is REFUSED by this build**, along with
> `integrations.forge_app_id` (whose only consumers are Jira and Confluence
> Cloud) and the per-seat and per-unit identities `role.integrations.jira`
> and `unit.integrations.jira`. No parser turns a Jira delivery into a
> notification, so the block configured webhooks that verified events and
> reached nobody, and the project identities recorded where those events
> would have routed. Config validation now rejects all of them by name and
> says what serves the role instead: the tracker this build routes end to
> end is [Plane](plane.md), whose `role.integrations.plane` and
> `unit.integrations.plane` are the identities that do get consulted.
>
> `POST /webhooks/jira` and `POST /webhooks/forge` still exist and fail
> closed at 503, and come alive in the same change that ships the parser.
> **Agents still reach Jira through MCP**, which is unaffected — that is
> `mcp_servers` plus each seat's credentials, and has nothing to do with the
> refused block. Everything below describes the intended contract.

Crewlet integrates with Jira in two directions: agents control Jira via MCP tools, and Jira pushes events to agents via webhooks.

> **Prerequisites — the Atlassian side is set up by hand.** Atlassian offers no API for provisioning users, so the operator creates the Atlassian site (Cloud or Data Center) and each agent's Atlassian account and API token manually, then wires the tokens into `mcp_env` as shown below. Webhooks differ by deployment: **Cloud** events arrive via the [Crewlet Forge app](https://github.com/crewlet/forge); **Data Center** uses direct webhook registration (see [Webhooks](#webhooks-jira-pushes-to-agents)).

---

## Configuration

The `integrations.jira` block is **non-tool config** — the admin/service account for org-level REST calls (watcher lookups) and the inbound webhook secret. The Jira MCP *tool* server is a separate `mcp_servers` entry shared with Confluence (name it `atlassian`):

```yaml
integrations:
  jira:
    url: "${JIRA_URL}"                    # Jira instance URL
    token: "${JIRA_API_TOKEN}"            # API token (admin/service account)
    email: "${JIRA_EMAIL}"                # Cloud only — admin email for Basic Auth
    webhook_secret: "${JIRA_WEBHOOK_SECRET}"  # Data Center: required, HMAC-SHA256

mcp_servers:
  - name: atlassian                     # shared by Jira + Confluence (one mcp-atlassian)
    shared: false                       # per-agent: each role supplies its own token
    command: uvx
    args: ["mcp-atlassian"]
    env:
      JIRA_URL: "${JIRA_URL}"           # declare explicitly — the engine does not inject it
```

> **Human-clickable links agents share:** with `cloud_id`, the `mcp-atlassian` tools return `api.atlassian.com/ex/jira/{cloud_id}/...` gateway URLs, which colleagues can't open. To have agents share a clickable `…atlassian.net/browse/{ISSUE-KEY}` link, set a [skill variable](../concepts/tool-skills.md#skill-variables) — `skill_variables.jira_base_url: "https://mycompany.atlassian.net"` — for your mention/link Tool Skill to reference. (The bundled `examples/tool-skills/platform-mentions.md` ships Plane-shaped, since the reference org runs on Plane; a Jira org adapts its link-shape section to this variable.) Note Jira `browse` links are always *composed* by the agent (Jira tool results carry only REST self-links), so this prompt-layer variable is the primary fix here, not a fallback.

For **Cloud** webhooks, install the [Crewlet Forge app](https://github.com/crewlet/forge) which forwards events via Forge Remote to `POST /webhooks/forge`. The `webhook_secret` field is only used for Data Center deployments.

---

## MCP Server (Agents Control Jira)

The `atlassian` MCP server gives agents full Jira capabilities — creating issues, transitioning statuses, adding comments, managing assignees. Set `JIRA_URL` in the server's `env` — the engine does not derive it from `integrations.jira.url`. Naming the server `atlassian` also lets the engine enable the required `jira_users` toolset and scope the [Confluence knowledge search](confluence.md).

### Per-Unit Jira Projects

Declare the unit's Jira project under `integrations.jira.project` (its integration identity), and put each agent's token in `mcp_env.atlassian`:

```yaml
units:
  - name: Core
    type: team
    lead: CTO
    integrations:
      jira:
        project: "ENG"             # the unit's Jira project (integration identity)
    roles:
      - name: CTO
        mcp_env:
          atlassian: { JIRA_USERNAME: "${CTO_JIRA_USER}", JIRA_API_TOKEN: "${CTO_JIRA_TOKEN}" }
      - name: Engineer
        mcp_env:
          atlassian: { JIRA_USERNAME: "${ENG_JIRA_USER}", JIRA_API_TOKEN: "${ENG_JIRA_TOKEN}" }
```

(`mcp_env.atlassian` carries the `mcp-atlassian` server's env vars directly — `JIRA_USERNAME`, `JIRA_API_TOKEN`, the matching Confluence creds, and `JIRA_PROJECTS_FILTER` / `CONFLUENCE_SPACES_FILTER` for scoping — for any var the server reads. The unit's Jira project / Confluence space *identity* lives in the unit's `integrations.jira.project` / `integrations.confluence.space`, not in `mcp_env`.)

The project identity is set once on the unit's `integrations.jira.project` — it is integration identity (webhook routing + write home), not a tool credential, and it does not scope knowledge reads. The per-agent `mcp_env.atlassian` creds inherit `{**unit_mcp_env, **role_mcp_env}` (role-level overrides win), so each agent still authenticates as itself.

---

## Webhooks (Jira Pushes to Agents)

Jira Cloud and Data Center use different webhook models. Cloud uses the **Crewlet Forge app**; Data Center uses direct webhook registration.

### Jira Cloud — Forge App

Install the [Crewlet Forge app](https://github.com/crewlet/forge) from the Atlassian Marketplace (or via a private installation link). The Forge app currently forwards these Jira issue events to the Crewlet backend:

- `avi:jira:created:issue` — new ticket created
- `avi:jira:updated:issue` — ticket field changed (status, assignee, priority, etc.)
- `avi:jira:deleted:issue` — ticket deleted

Events are delivered via Forge Remote to `POST /webhooks/forge`. The Forge platform handles authentication automatically.


### Jira Data Center — Direct Webhook Registration

1. In Jira, go to **Settings** > **System** > **WebHooks**
2. Set URL to `https://your-server.com/webhooks/jira`
3. Select events: Issue created, updated, commented
4. Set a **Secret** for HMAC-SHA256 signature verification

Inbound requests are verified using **HMAC-SHA256** against the `X-Hub-Signature` header, at the route, before the delivery is recorded or published — the same point at which the GitHub, GitLab and Plane webhooks verify theirs. `POST /webhooks/jira` is exempt from the API's bearer token precisely *because* it authenticates by provider HMAC, so the check belongs there. Invalid or missing signatures are rejected with `401`.

`webhook_secret` is therefore **required** for Data Center webhooks: without one the endpoint answers **503** with a `Retry-After`, exactly as its peers do, rather than accepting deliveries it cannot verify. That is deliberately not a 4xx — the sender's request is fine, what is missing is on this side, and a 4xx would tell it to discard a delivery nobody else has a copy of. The delivery waits at Jira and flows once the secret is set. Cloud is unaffected — those events arrive through the Forge app on `/webhooks/forge` and carry a JWT instead.

### Programmatic Transport Setup

```python
from crewlet.config import JiraConfig
from crewlet.notifications.transports.jira import JiraTransport

# Cloud — webhooks via Forge app
jira_config = JiraConfig(
    url="https://your-company.atlassian.net",
    token="your-api-token",
    email="admin@your-company.com",
)

# Data Center — direct webhook with HMAC secret
jira_config = JiraConfig(
    url="https://jira.internal.company.com",
    token="your-pat",
    webhook_secret="your-hmac-secret",
)

jira_transport = JiraTransport(jira_config)
```

### Event Deduplication

The transport deduplicates webhook events using a composite key of timestamp + issue key + event type, with a 5-minute TTL.

### Routing Strategy

Once an event passes signature verification and deduplication, the transport fans it out by specificity. The trigger user (the person whose action fired the webhook) is excluded from every step — they already know about their own change.

1. **Watchers** — fetched from the Jira REST API. Every agent matching a watcher account ID gets a copy with `metadata.routed_via = "watcher"`.
2. **Assignee** — if set and not already delivered as a watcher, the assignee gets a copy with `routed_via = "assignee"`.
3. **@mentions** — any agent named in `<ri:user>` markup inside `body.comment.body` gets a copy with `routed_via = "mention"`. (Jira's watcher list does not auto-include mentioned users, so this step covers non-watchers who got @'d.)
4. **Project lead fallback** — if steps 1-3 produced no *meaningful* recipient (the creator is auto-added by Jira as a watcher, so a delivery to only the creator counts as no recipient), the lead of the unit that owns the project (its `integrations.jira.project`) gets a copy with `routed_via = "project_lead_fallback"`. The mapping is built once at engine start from each unit's `integrations.jira.project` (runtime `OrgUnit.jira_project`) and the effective lead.
5. **Standard resolution** — if none of the above matched (no project key mapping configured), the original notification is returned for generic handle/email resolution.

The `routed_via` value appears in the lead's prompt under **Event Metadata**, so the agent can tell at a glance whether they're a personal recipient or a fallback recipient.

### Lead-fallback prompt hint

When a lead receives an event via `project_lead_fallback`, the Jira notification prompt adds a `## Why You Received This` section that names the project, warns the lead that no one else is watching the issue, and lays out three explicit decisions:

- **Delegate** — use `lookup_colleague` to find the right teammate and resolve their Jira account ID, then set the assignee (future updates route to them, not back to the lead).
- **Take it yourself** — assign the issue to yourself so the routing reflects reality.
- **Escalate** — if the issue is out of scope or the lead can't identify the right owner, hand it off to their own manager (named in the identity prompt) by commenting on the issue with an @mention or reassigning the issue to them. Lead-fallback fires only when nobody else is involved, so silently walking away would leave the issue unwatched and unhandled.

The hint is suppressed for `watcher` / `assignee` / `mention` routings — those carry their own signal of personal involvement and don't need the extra framing. The block deliberately describes intent rather than naming specific MCP action tools (the assignment tool, comment tool, etc.) since those change upstream; only the stable `lookup_colleague` builtin is named explicitly.

---

## How It Works

Task state lives in Jira — the engine mirrors nothing. Webhooks become `ExternalNotification` inbox events for the routed agents (watchers, assignee, @-mentions, project-lead fallback), and every write back to Jira happens through the agents' own MCP tools:

```
Jira ticket created ──webhook──► API ──► EventQueue
                                            │
                                            ▼
                                    Team lead wakes up
                                    Assigns via MCP tools
                                            │
                                            ▼
                            Assignment webhook fires
                                            │
                                            ▼
                                    Assigned agent wakes up
                                    Works on task, transitions
                                    ticket via MCP tools
```

There is no engine-side sync layer, no completion-comment automation, and no reconciliation poller: each MCP-tool action an agent takes fires the next webhook, which wakes the next participant — the same loop a human teammate drives. A webhook delivery that is lost is recovered the way it would be for a human: the issue's next activity (a comment, a transition, a nudge from a colleague) re-notifies the routed agents.

See [Task Engine](../concepts/task-engine.md) for the passive `ExecutionTracker` exposed to extensions.
