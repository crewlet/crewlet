# Configure Nimbus via the `/config/*` API

End-to-end recipe for bootstrapping the [`examples/nimbus.company.yaml`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) company against a running engine — first the one-shot `PUT /config` (recommended), then per-entity edits you'd run afterwards to evolve the company live.

Every request below assumes:

```bash
export CREWLET_URL="http://localhost"   # the example Tier A file binds the embedded API on port 80
export TOKEN="$CREWLET_API_TOKEN_FOUNDER"   # matches api.auth.tokens[].token in config.yaml
export AUTH="Authorization: Bearer $TOKEN"
```

`/health` should report `{"status":"unconfigured","configured":false}` before you start (still HTTP **200** — the status code is liveness, and an engine waiting for a configuration is alive). After the first PUT it flips to `{"status":"ok","configured":true}` and stays that way for the engine's lifetime.

```bash
curl -s $CREWLET_URL/health
```

See the [Configuration concept doc](../concepts/configuration.md) for the two-tier split and the rationale behind live config management, and the [API endpoints reference](../reference/api-endpoints.md) for status codes.

---

## Option 1 — Single full-document PUT (recommended for bootstrap)

The simplest path. Send the whole `company.yaml` in one request; the engine validates, persists as a new revision, appends an activation epoch, and spawns the whole company. Every node in the deployment converges on that epoch — see [Control Plane](../concepts/control-plane.md).

```bash
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: bootstrap Nimbus" \
  --data-binary @examples/nimbus.company.yaml
```

`X-Summary` is required on full PUT. Response is `201 Created` with `{"revision_id": "..."}`.

JSON body works too:

```bash
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" \
  -H "Content-Type: application/json" \
  -H "X-Summary: bootstrap Nimbus" \
  --data-binary @nimbus.company.json
```

Verify:

```bash
curl -s $CREWLET_URL/health $AUTH                                 # configured: true
curl -s $CREWLET_URL/config -H "$AUTH" | jq '.name'               # "Nimbus"
curl -s $CREWLET_URL/config/revisions -H "$AUTH" | jq '.[0]'      # newest first
curl -s $CREWLET_URL/agents | jq 'length'                         # 7 agent seats spawned
```

If anything else has touched `/config` since you last read it, supply `If-Match`:

```bash
REV=$(curl -s $CREWLET_URL/config/revisions -H "$AUTH" | jq -r '.[0].revision_id')
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" -H "If-Match: $REV" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: bootstrap Nimbus" \
  --data-binary @examples/nimbus.company.yaml
# 409 revision_advanced if the active revision moved past $REV between read + write
```

---

## Option 2 — Build the company piece by piece via per-entity routes

Useful when you want a self-documenting script, or when you're evolving an already-active company. The order below is the natural one for Nimbus. You **must** PUT `/config` at least once first — the per-entity routes return `409 company_not_initialised` while unconfigured.

If you don't have a baseline yet, post a minimal stub:

```bash
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" -H "Content-Type: application/json" -H "X-Summary: minimal stub" \
  -d '{"name":"Nimbus"}'
```

`X-Summary` is auto-generated on per-entity writes (`"Added role 'Agent CEO'"`, `"Updated identity (mission, vision)"`). Override with `-H "X-Summary: ..."` when you want a custom note.

### 2.1 Identity

```bash
curl -X PUT $CREWLET_URL/config/identity \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Nimbus",
    "mission": "Turn any hardware into cloud-native infrastructure — and give AI teams one open framework to build on top of it",
    "vision": "Phase 1 (shipped): a cloud-native control plane that turns bare metal, on-prem, cloud, and edge hardware into seamless Kubernetes clusters ready to serve AI/ML pipelines and applications at enterprise scale. Phase 2 (in progress): an open, Python-first framework — analogous in spirit to Ray — that runs on Nimbus clusters and unifies how teams develop, schedule, and serve distributed AI workloads (training, fine-tuning, batch inference, online serving). The framework owns the developer surface (SDK, CLI, examples, docs); the control plane (nimbuscore) owns the cluster surface (provisioning, multi-region, GPU scheduling primitives). The two are designed together.",
    "policies": [
      "Communicate decisions in writing",
      "Capture personal-context facts you need to recall later (delegation context, stakeholder preferences, ongoing operational state) via `reflect_and_persist` — pass `ttl_days` for facts that age out naturally (e.g. delegation context lasts the MR lifetime)",
      "Document team-shared knowledge (decisions, conventions, runbooks) as Plane pages so other agents can read it — `reflect_and_persist` writes are personal-only",
      "Use Plane work items to manage all product backlog items — never track tasks only in memory",
      "Use Plane pages to document architecture decisions, runbooks, and meeting notes — search before creating new docs",
      "Each engineering role owns specific projects (see the `Repo Ownership` page in the LEAD Plane project); do not push to projects you do not own without an explicit handoff from their owner",
      "The Phase-2 AI framework is developed in a dedicated project owned by AI Systems Engineering; the control plane (nimbuscore, nimbusk0s) stays focused on cluster-level primitives that the framework depends on"
    ]
  }'
```

### 2.2 LLM providers

```bash
curl -X PUT $CREWLET_URL/config/llm-providers/default \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "type": "openai-compatible",
    "model": "your-model-id",
    "base_url": "https://api.your-llm-provider.example/v1/",
    "timeout_seconds": 300,
    "api_keys": ["${LLM_API_KEY}"]
  }'

# List / inspect
curl -s $CREWLET_URL/config/llm-providers -H "$AUTH" | jq
curl -s $CREWLET_URL/config/llm-providers/default -H "$AUTH" | jq

# Delete (rotates the role to a different provider on next turn)
# curl -X DELETE $CREWLET_URL/config/llm-providers/default -H "$AUTH"
```

Any OpenAI-compatible endpoint works here (OpenAI itself, OpenRouter, Together, a local vLLM/LiteLLM gateway, …) — the shipped example keeps `model` and `base_url` as `${LLM_MODEL}` / `${LLM_BASE_URL}` env references so one `.env` points the whole config. Prefer Anthropic? PUT `{"type": "anthropic", "model": "claude-sonnet-5", "api_keys": ["${ANTHROPIC_API_KEY}"]}` instead.

`${LLM_API_KEY}` stays as a literal reference string in the DB; the engine resolves it from its env at provider-construction time. Never inline raw secrets — exports leak otherwise.

### 2.3 Embeddings

```bash
curl -X PUT $CREWLET_URL/config/embeddings \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "type": "openai",
    "model": "text-embedding-3-small",
    "api_key": "${OPENAI_API_KEY}",
    "dimensions": 1536
  }'
```

### 2.4 Turn engine

```bash
curl -X PUT $CREWLET_URL/config/turn-engine \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "extension_enabled": true,
    "plan_max_tool_rounds_ceiling": 32,
    "execute_max_tool_rounds_ceiling": 40,
    "extension_round_step": 8
  }'
```

Settings are read through a `TurnEngineSettings` cell so the next turn picks them up; in-flight turns finish on the prior snapshot.

### 2.5 Learning

```bash
curl -X PUT $CREWLET_URL/config/learning \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "enabled": true,
    "reflect": {
      "enabled": true,
      "summarize_episodes": true
    },
    "counterparty": {
      "enabled": true
    },
    "skill_synthesis": {
      "enabled": true,
      "scheduler_enabled": true
    },
    "skill_refinement": {
      "enabled": true
    },
    "skill_promotion": {
      "enabled": true
    },
    "personal_memory": {
      "max_refreshes_per_turn": 3
    },
    "episode_lifecycle": {
      "max_raw_episodes_per_agent": 500,
      "non_terminal_max_age_days": 14,
      "consolidated_grace_days": 30,
      "compaction_min_age_days": 30,
      "compaction_min_cluster_size": 3,
      "compacted_max_age_days": 0
    }
  }'
```

> **Note:** Learning is the one subsystem that does NOT live-restart. The new config is stored for the next engine restart; running `ReflectEngine` / `EpisodeLifecycleWorker` / `SkillCuratorWorker` keep the prior config until then. A WARNING is logged.

### 2.6 Integrations (Plane, Mattermost, GitLab)

Each route sets `integrations.<kind>` (the route does the nesting; the body is the bare config block). These carry only **non-tool** config — the MCP tool servers are declared under `mcp_servers` ([section 2.7](#27-mcp-servers)), and the org-wide knowledge read scope lives in the `knowledge` block (set via the full PUT; Nimbus deliberately omits it — reads are membership-bound, see the comment in the example YAML).

> **Running on Atlassian instead?** The same route also accepts `integrations/jira` and `integrations/confluence` bodies — but `integrations.confluence` and `integrations.plane` are mutually exclusive (`400 validation_error`): the knowledge backend is single-homed, and a Confluence↔Plane switch is a cut-over (disable one before enabling the other).

```bash
# Plane (self-hosted crewlet/plane fork) — webhook + engine-read side.
# The webhook secret is generated BY Plane at hook creation and captured
# by `crewlet plane provision` into ${PLANE_WEBHOOK_SECRET}; keep it and
# the engine token as ${VAR} references, never inline values.
curl -X PUT $CREWLET_URL/config/integrations/plane \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "enabled": true,
    "url": "https://plane.nimbus.example",
    "workspace": "nimbus",
    "webhook_secret": "${PLANE_WEBHOOK_SECRET}",
    "token": "${PLANE_ENGINE_TOKEN}",
    "provisioning": {
      "role": "member",
      "username_prefix": "",
      "projects": ["LEAD", "ENG", "PROD", "TS"],
      "token_expiry_days": 364
    }
  }'

# Mattermost — enables BOTH the outbound transport and the inbound
# websocket fleet (one connection per agent seat). Per-agent bot tokens
# live on each role's `integrations.mattermost` block + `mcp_env.mattermost`,
# see section 2.9. The `provisioning` sub-block is read only by
# `crewlet mattermost provision`, never the engine.
curl -X PUT $CREWLET_URL/config/integrations/mattermost \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "enabled": true,
    "url": "https://chat.nimbus.example",
    "team": "nimbus",
    "typing_status": "addressed",
    "provisioning": {
      "username_prefix": "",
      "channels": ["town-square", "engineering", "product"],
      "display_name_suffix": " (AI)"
    }
  }'

# GitLab (the code host) — webhook + engine-read side; the `provisioning`
# sub-block is read only by `crewlet gitlab provision`, never the engine.
curl -X PUT $CREWLET_URL/config/integrations/gitlab \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "enabled": true,
    "url": "https://gitlab.com",
    "signing_secret": "${GITLAB_SIGNING_SECRET}",
    "token": "${GITLAB_ENGINE_TOKEN}",
    "provisioning": {
      "group": "nimbus-hq",
      "access_level": "maintainer",
      "projects": [
        "nimbus-hq/nimbuscore",
        "nimbus-hq/nimbusk0s",
        "nimbus-hq/console",
        "nimbus-hq/website"
      ],
      "group_webhook": "auto",
      "token_scopes": ["api"]
    }
  }'
```

The Plane/Mattermost/GitLab MCP *tool* servers are declared under `mcp_servers` ([section 2.7](#27-mcp-servers)), not here — each is a per-agent stdio template (`shared: false`) whose credentials come from role `mcp_env`. Changing one server's env there (e.g. a new `PLANE_BASE_URL`) restarts only that server via `_respawn_role_mcp`, not unrelated ones like `tavily`.

### 2.7 MCP servers

The roles in [section 2.9](#29-roles) reference three per-agent stdio templates (`plane`, `mattermost`, `gitlab`) plus the shared `tavily` — declare them all, or the role `mcp_env` blocks point at servers the engine doesn't know:

```bash
# List
curl -s $CREWLET_URL/config/mcp-servers -H "$AUTH" | jq

# Plane — official plane-mcp-server, version-pinned (the enforced
# platform-mentions skill names its tools exactly). PLANE_BASE_URL is
# mandatory for self-hosted; the per-agent PLANE_API_KEY comes from each
# role's mcp_env.plane.
curl -X POST $CREWLET_URL/config/mcp-servers \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "plane",
    "shared": false,
    "command": "uvx",
    "args": ["plane-mcp-server@0.2.10", "stdio"],
    "env": {
      "PLANE_BASE_URL": "https://plane.nimbus.example",
      "PLANE_WORKSPACE_SLUG": "nimbus"
    }
  }'

# Mattermost — per-agent token from role mcp_env.mattermost
curl -X POST $CREWLET_URL/config/mcp-servers \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "mattermost",
    "shared": false,
    "command": "uvx",
    "args": ["mcp-server-mattermost==0.5.1"],
    "tool_prefix": "mattermost_",
    "env": {
      "MATTERMOST_URL": "${MATTERMOST_URL}"
    }
  }'

# GitLab — official `glab mcp serve` (stdio); one process per role with
# that role's GITLAB_TOKEN from mcp_env.gitlab. Requires the glab CLI on
# the engine host.
curl -X POST $CREWLET_URL/config/mcp-servers \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "gitlab",
    "shared": false,
    "command": "glab",
    "args": ["mcp", "serve"]
  }'

# Tavily web search (shared backend the PM and DevRel call)
curl -X POST $CREWLET_URL/config/mcp-servers \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "tavily",
    "command": "npm",
    "args": ["exec", "--yes", "--", "tavily-mcp@latest"],
    "env": {"TAVILY_API_KEY": "${TAVILY_API_KEY}"},
    "shared": true
  }'

# Update one server
curl -X PUT $CREWLET_URL/config/mcp-servers/tavily \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "tavily",
    "command": "npm",
    "args": ["exec", "--yes", "--", "tavily-mcp@latest"],
    "env": {"TAVILY_API_KEY": "${TAVILY_API_KEY}"},
    "shared": true
  }'

# Remove
curl -X DELETE $CREWLET_URL/config/mcp-servers/tavily -H "$AUTH"
```

Each add/remove/update triggers `MCPToolBridge.restart_server` for that one server. Affected agents drop their cached MCP tool list on the next turn.

### 2.8 Org units (Leadership / Product / Engineering)

```bash
# List
curl -s $CREWLET_URL/config/units -H "$AUTH" | jq
```

Post each department as one unit (children + each team's `integrations.plane.project` identity — the unit's webhook-routing target and write home, **not** a tool credential and **not** a read scope). Roles get added separately via `/config/roles` in [section 2.9](#29-roles); a role posted with a `unit:` reference inherits any unit-level `mcp_env` (Nimbus keeps tool credentials per-role instead).

```bash
# Leadership / Executives
curl -X POST $CREWLET_URL/config/units \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Leadership",
    "type": "department",
    "lead": "Agent CEO",
    "children": [
      {
        "name": "Executives",
        "type": "team",
        "lead": "Agent CEO",
        "integrations": {
          "plane": { "project": "LEAD" }
        }
      }
    ]
  }'

# Product / Product Management + Developer Relations (both map PROD →
# their lead: PM and DevRel collaborate tightly, so they share the
# project and PROD webhooks reach Agent PM)
curl -X POST $CREWLET_URL/config/units \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Product",
    "type": "department",
    "lead": "Agent PM",
    "children": [
      {
        "name": "Product Management",
        "type": "team",
        "lead": "Agent PM",
        "integrations": {
          "plane": { "project": "PROD" }
        }
      },
      {
        "name": "Developer Relations",
        "type": "team",
        "integrations": {
          "plane": { "project": "PROD" }
        }
      }
    ]
  }'

# Engineering / Core
curl -X POST $CREWLET_URL/config/units \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Engineering",
    "type": "department",
    "lead": "Agent CTO",
    "children": [
      {
        "name": "Core",
        "type": "team",
        "lead": "Agent CTO",
        "integrations": {
          "plane": { "project": "ENG" }
        }
      }
    ]
  }'
```

Developer Relations omits its own `lead` so it inherits `Agent PM` from its parent department. Per-unit POSTs are best for adding *new* units to an already-bootstrapped company; use Option 1 (full-document PUT) when you want the whole tree in a single request.

```bash
# Inspect / replace / delete
curl -s $CREWLET_URL/config/units/Leadership -H "$AUTH" | jq
# curl -X PUT $CREWLET_URL/config/units/Leadership -H "$AUTH" -H "Content-Type: application/json" -d @leadership.json
# curl -X DELETE $CREWLET_URL/config/units/Leadership -H "$AUTH"
```

### 2.9 Roles

```bash
# List all roles (root + nested across units)
curl -s $CREWLET_URL/config/roles -H "$AUTH" | jq
```

Each `POST /config/roles` body carries a `unit: <name>` reference; the engine resolves it at build time, drops the role inside the named unit, and inherits any unit-level `mcp_env` so per-agent MCP instances spawn with the right credentials. (The unit's Plane project *identity* — for webhook routing and as the team write home — lives on the unit's `integrations.plane.project`, not on the role, and is **not** what scopes knowledge reads.) Per-agent tool credentials go in the role's `mcp_env` (keyed by server name): every seat carries `mcp_env.plane.PLANE_API_KEY` (its provisioned Plane service-account token) and `mcp_env.mattermost.MATTERMOST_TOKEN`; the GitLab-facing seats add `mcp_env.gitlab.GITLAB_TOKEN` for the `glab` stdio server. The Mattermost **transport** identity goes in the role's `integrations.mattermost` block. A Mattermost-enabled role names the same bot token in both `integrations.mattermost.bot_token` and `mcp_env.mattermost.MATTERMOST_TOKEN` — three consumers (websocket, REST, MCP), one secret. Bodies below mirror `examples/nimbus.company.yaml` — keep them verbatim or trim long responsibilities / behavioural-guidelines lists to taste. (For brevity the bodies below omit the example's role `schedules:` and the three engineering seats' `sandbox:` blocks — take those verbatim from the YAML if you want the scheduled reviews and the code-runtime sandbox.)

**Agent CEO** — Leadership / Executives:

```bash
curl -X POST $CREWLET_URL/config/roles \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Agent CEO",
    "unit": "Executives",
    "goal": "Define Nimbus'"'"'s product vision and strategic direction, prioritize high-impact initiatives across product and engineering, make final calls on roadmap trade-offs, and ensure the team stays aligned on building an open, composable infrastructure control plane that wins in the AI/ML and enterprise market",
    "backstory": "Founder-minded CEO with deep roots in cloud infrastructure and platform engineering. Built and scaled developer-facing infrastructure products from zero to thousands of enterprise deployments. You live at the intersection of technology and business — fluent in Kubernetes, bare-metal provisioning, and GPU orchestration, but equally sharp on go-to-market strategy, competitive positioning, and fundraising narratives.",
    "manages": ["Agent CTO", "Agent PM"],
    "responsibilities": [
      "Set and communicate Nimbus'"'"'s product vision and quarterly OKRs",
      "Prioritize initiatives across product and engineering based on strategic impact, customer demand, and competitive pressure",
      "Make final go/no-go decisions on features, partnerships, and architectural bets",
      "Review and approve high-impact epics and stories proposed by the PM",
      "Unblock cross-team dependencies and resolve escalated disagreements between CTO and PM",
      "Monitor overall project health in Plane — flag stalled epics, scope creep, or misaligned priorities",
      "Ensure strategic decisions and rationale are documented in Plane comments or knowledge base for team recall",
      "If you are distributing/assigning any unassigned story/task/work item/... to any team member including yourself, always make sure to actually set the assignee of the story/task/work item/... in Plane with the correct tool"
    ],
    "behavioral_guidelines": [
      "Before making a prioritization call, review the current state of the Plane backlog and any open escalations from the CTO or PM.",
      "When setting direction or changing priorities, create or update a Plane work item with clear rationale so the team understands the \"why\" behind the decision.",
      "When asked for a decision, respond with a clear verdict and reasoning — avoid ambiguous or deferred answers.",
      "Delegate execution details to the CTO (technical) and PM (product backlog) — focus on strategic alignment, not implementation specifics.",
      "Regularly check in on progress by reviewing the Plane boards and team updates rather than requesting ad-hoc status reports.",
      "Store strategic insights, competitive intelligence, and key decisions in your personal knowledge base for future reference.",
      "When documenting strategic decisions or company-wide announcements, publish them as Plane pages in the LEAD project so all team members can reference them — every agent'"'"'s `## Relevant knowledge` search runs as their own Plane user, so anyone with access to the LEAD project will surface it."
    ],
    "integrations": {
      "mattermost": {
        "bot_token": "${MATTERMOST_TOKEN_CEO}"
      }
    },
    "mcp_env": {
      "plane": {
        "PLANE_API_KEY": "${PLANE_TOKEN_CEO}"
      },
      "mattermost": {
        "MATTERMOST_TOKEN": "${MATTERMOST_TOKEN_CEO}"
      }
    }
  }'
```

**Agent CTO** — Leadership / Executives:

```bash
curl -X POST $CREWLET_URL/config/roles \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Agent CTO",
    "unit": "Executives",
    "goal": "Own Nimbus'"'"'s technical architecture and engineering execution — make architecture decisions, break down epics into actionable engineering tasks, assign work to the SWE team, manage sprints, and ensure the platform is built with the right patterns for scale, observability, and extensibility",
    "manages": ["Agent SWE", "Agent Frontend SWE", "Agent AI Systems Engineer"],
    "integrations": {
      "mattermost": {
        "bot_token": "${MATTERMOST_TOKEN_CTO}"
      }
    },
    "mcp_env": {
      "plane": {
        "PLANE_API_KEY": "${PLANE_TOKEN_CTO}"
      },
      "mattermost": {
        "MATTERMOST_TOKEN": "${MATTERMOST_TOKEN_CTO}"
      }
    }
  }'
```

**Agent PM** — Product / Product Management:

```bash
curl -X POST $CREWLET_URL/config/roles \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Agent PM",
    "unit": "Product Management",
    "manages": ["Agent DevRel"],
    "goal": "Drive Nimbus'"'"'s product strategy by researching the composable infrastructure and AI-framework markets, proposing high-impact user stories across both the control plane and the Phase-2 framework, and maintaining a prioritized Plane backlog aligned with Nimbus'"'"'s two-phase vision",
    "integrations": {
      "mattermost": {
        "bot_token": "${MATTERMOST_TOKEN_PM}"
      }
    },
    "mcp_env": {
      "plane": {
        "PLANE_API_KEY": "${PLANE_TOKEN_PM}"
      },
      "mattermost": {
        "MATTERMOST_TOKEN": "${MATTERMOST_TOKEN_PM}"
      }
    }
  }'
```

**Agent DevRel** — Product / Developer Relations (with a GitLab PAT for the docs work on the `nimbus-hq` projects):

```bash
curl -X POST $CREWLET_URL/config/roles \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Agent DevRel",
    "unit": "Developer Relations",
    "goal": "Make the Nimbus platform — both the control plane and the Phase-2 AI framework — discoverable, learnable, and adoptable. Own developer-facing documentation, tutorials, runnable examples, and the public narrative on the website blog so an external developer can go from \"never heard of Nimbus\" to \"running a distributed job in 10 minutes\"",
    "integrations": {
      "mattermost": {
        "bot_token": "${MATTERMOST_TOKEN_DEVREL}"
      }
    },
    "mcp_env": {
      "plane": {
        "PLANE_API_KEY": "${PLANE_TOKEN_DEVREL}"
      },
      "mattermost": {
        "MATTERMOST_TOKEN": "${MATTERMOST_TOKEN_DEVREL}"
      },
      "gitlab": {
        "GITLAB_TOKEN": "${GITLAB_TOKEN_DEVREL}",
        "GITLAB_HOST": "gitlab.com"
      }
    }
  }'
```

**Agent SWE** — Engineering / Core (the agent's `email` is set so external notifications route to the right inbox; plus a GitLab PAT for the `nimbuscore`/`nimbusk0s` projects):

```bash
curl -X POST $CREWLET_URL/config/roles \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Agent SWE",
    "unit": "Core",
    "email": "agent-swe@nimbus.example",
    "goal": "Pick up the assigned Plane work items and implement production-grade Go features for Nimbus'"'"'s control plane (nimbuscore, nimbusk0s), write comprehensive tests, and ship clean, reviewable code that meets acceptance criteria — while keeping work-item status, worklogs, and comments up to date throughout the lifecycle of every task",
    "integrations": {
      "mattermost": {
        "bot_token": "${MATTERMOST_TOKEN_SWE}"
      }
    },
    "mcp_env": {
      "plane": {
        "PLANE_API_KEY": "${PLANE_TOKEN_SWE}"
      },
      "mattermost": {
        "MATTERMOST_TOKEN": "${MATTERMOST_TOKEN_SWE}"
      },
      "gitlab": {
        "GITLAB_TOKEN": "${GITLAB_TOKEN_SWE}",
        "GITLAB_HOST": "gitlab.com"
      }
    }
  }'
```

**Agent Frontend SWE** — Engineering / Core (owns the `console` + `website` projects via GitLab PAT):

```bash
curl -X POST $CREWLET_URL/config/roles \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Agent Frontend SWE",
    "unit": "Core",
    "goal": "Build and evolve the Nimbus Console and the marketing website — deliver the AI-services management UI (models, fine-tuning, datasets, jobs) the README promises today, and keep the console in lock-step with new nimbuscore endpoints",
    "integrations": {
      "mattermost": {
        "bot_token": "${MATTERMOST_TOKEN_FE}"
      }
    },
    "mcp_env": {
      "plane": {
        "PLANE_API_KEY": "${PLANE_TOKEN_FE}"
      },
      "mattermost": {
        "MATTERMOST_TOKEN": "${MATTERMOST_TOKEN_FE}"
      },
      "gitlab": {
        "GITLAB_TOKEN": "${GITLAB_TOKEN_FE}",
        "GITLAB_HOST": "gitlab.com"
      }
    }
  }'
```

**Agent AI Systems Engineer** — Engineering / Core (owns the Phase-2 framework project via GitLab PAT):

```bash
curl -X POST $CREWLET_URL/config/roles \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{
    "name": "Agent AI Systems Engineer",
    "unit": "Core",
    "goal": "Design and build the Phase-2 Nimbus AI framework — a Python-first, open-source framework that runs on Nimbus clusters and unifies distributed training, fine-tuning, batch inference, and online serving for AI/ML teams. Own the framework'"'"'s runtime, SDK, CLI, and cluster-side integration with nimbuscore",
    "integrations": {
      "mattermost": {
        "bot_token": "${MATTERMOST_TOKEN_AI}"
      }
    },
    "mcp_env": {
      "plane": {
        "PLANE_API_KEY": "${PLANE_TOKEN_AI}"
      },
      "mattermost": {
        "MATTERMOST_TOKEN": "${MATTERMOST_TOKEN_AI}"
      },
      "gitlab": {
        "GITLAB_TOKEN": "${GITLAB_TOKEN_AI}",
        "GITLAB_HOST": "gitlab.com"
      }
    }
  }'
```

```bash
# Inspect / update / delete by handle (the engine auto-derives the
# handle from the role name when not supplied — e.g. "Agent CEO"
# becomes "agent-ceo").
curl -s $CREWLET_URL/config/roles/agent-ceo -H "$AUTH" | jq
# curl -X PUT $CREWLET_URL/config/roles/agent-ceo -H "$AUTH" -H "Content-Type: application/json" -d @ceo.json
# curl -X DELETE $CREWLET_URL/config/roles/agent-ceo -H "$AUTH"
```

`POST /config/roles` spawns one new agent and (when the `unit:` reference resolves) triggers a per-role MCP server spawn for it: a `plane-mcp-server` instance with the role's own `mcp_env.plane.PLANE_API_KEY`, a Mattermost MCP with the per-agent `mcp_env.mattermost.MATTERMOST_TOKEN`, and a `glab mcp serve` instance when `mcp_env.gitlab.GITLAB_TOKEN` is set. `DELETE` terminates the agent and stops its per-role MCP instances. `PUT` swaps the `AgentDefinition` in place — the next turn renders the new prompts.

### 2.10 Budgets

`examples/nimbus.company.yaml` pins a `token_budget` of `10000000` (10M tokens) — a single org-wide cap shared across every agent. Raise, lower, or lift it (`0` = unlimited) live:

```bash
# Raise to 20M tokens org-wide (cap shared across all agents)
curl -X PUT $CREWLET_URL/config/budgets \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{"token_budget": 20000000}'
```

### 2.11 Extensions

Nimbus ships with no extensions enabled. The route exists for op-installed extensions (`crewlet.extensions.*`):

```bash
curl -s $CREWLET_URL/config/extensions -H "$AUTH" | jq          # []

# Add — the key is the extension's registry name; the value is its settings dict.
curl -X POST $CREWLET_URL/config/extensions \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{"my_ext": {"setting": "value"}}'

# Remove
curl -X DELETE $CREWLET_URL/config/extensions/my_ext -H "$AUTH"
```

Same-name re-adds trigger an `unregister` + `register` cycle for that one extension; unchanged neighbours keep their live instance.

---

## Read paths

```bash
# Active revision (JSON or YAML)
curl -s $CREWLET_URL/config -H "$AUTH" | jq
curl -s "$CREWLET_URL/config?format=yaml" -H "$AUTH"

# Revision history (newest first)
curl -s "$CREWLET_URL/config/revisions?limit=20&offset=0" -H "$AUTH" | jq

# Single revision incl. payload
curl -s $CREWLET_URL/config/revisions/$REV -H "$AUTH" | jq

# Structural diff between two revisions (or against active)
curl -s "$CREWLET_URL/config/revisions/$REV/diff" -H "$AUTH" | jq
curl -s "$CREWLET_URL/config/revisions/$REV/diff?against=$BASE_REV" -H "$AUTH" | jq

# Audit feed for ops scraping (metadata only — no payloads)
curl -s "$CREWLET_URL/config/audit?limit=50" -H "$AUTH" | jq
```

---

## Revert

Re-activate any historical revision as a new active revision (the audit chain stays intact via `parent_revision_id`):

```bash
curl -X POST $CREWLET_URL/config/revisions/$REV/revert \
  -H "$AUTH" -H "X-Summary: revert — bootstrap was missing role X"
```

---

## Common error responses

| Status | Error | Meaning |
|--------|-------|---------|
| `400` | `invalid_body` | Body isn't JSON / YAML, or wrong `Content-Type` |
| `400` | `validation_error` | The merged config failed validation — `detail` carries the message |
| `400` | `summary_required` | Full PUT without `X-Summary` header or body `_summary` |
| `401` | `invalid_token` | Bearer missing / wrong / wrong scheme |
| `404` | `no_active_revision` | Reading `/config` before the first PUT |
| `404` | `not_found` | Role/unit/server/provider with that key doesn't exist |
| `409` | `company_not_initialised` | Per-entity write before the first PUT |
| `409` | `revision_advanced` | Stale `If-Match` or concurrent writer won the race |
| `412` | `if_match_must_be_none_when_unconfigured` | `If-Match: <uuid>` sent while engine is unconfigured |

The full reference is in [API endpoints](../reference/api-endpoints.md).
