# Plane Integration

Crewlet integrates with [Plane](https://plane.so) — self-hosted, on **the crewlet/plane fork** — as the work-item tracker **and** the knowledge backend, so one product covers both halves of the stack. All three sides are wired end to end:

- **Tracker** — work-item webhooks with two-layer routing (directed targets from the payload, thread activity fanned out to subscribers), intake triage, project-lead fallback, per-agent MCP tools, boot-time identity registration.
- **Knowledge** — [query-time knowledge search](#query-time-knowledge-search), [`crewlet plane import`](#publishing-docs--skills--crewlet-plane-import) for docs + Tool Skills, [webhook-driven tool-skill sync](#the-tool-skills-project), [skill promotion](#skill-promotion-on-plane).
- **Operations** — [`crewlet plane provision`](#provisioning--crewlet-plane-provision), an idempotent reconcile CLI (service accounts, tokens, project memberships, the workspace webhook), plus a full [docker-compose local loop](#local-testing) (the `plane` profile in `docker-compose.yml` + `scripts/plane-dev-bootstrap.sh`) running the Nimbus example org.

Because Plane replaces Confluence as the knowledge backend, **`integrations.confluence` and an enabled `integrations.plane` are mutually exclusive** — the knowledge backend is single-homed, and a Confluence→Plane migration is a cut-over (disable one). Jira + Plane may coexist freely.

---

## The fork

The integration targets a self-hosted deployment of **`github.com/crewlet/plane`**, pinned to the **`preview`** tag (images are published under the fork's namespace by its CI). The fork is upstream Plane CE plus the patches this integration depends on — all landed on `preview`; the upstream PRs are still pending, so **pin the fork tag, not upstream**:

| Patch | What it adds | Used by |
|---|---|---|
| [#9397](https://github.com/makeplane/plane/pull/9397) | Work-item **subscribers** on the public API | subscriber fan-out for `issue`/`issue_comment` thread activity (needs the engine `token`) |
| [#9401](https://github.com/makeplane/plane/pull/9401) + F9 | `page` webhook events — action vocabulary `created` / `updated` / `deleted` (a duplicate emits `created`) — including the content-persist `page.updated` fired when the live editor flushes a page body, debounced 60 s per page | page-event routing + the tool-skill sync worker's index hook |
| [#9403](https://github.com/makeplane/plane/pull/9403) | `mentions` on work-item comments, rendered as `<mention-component>` markup | @-mention routing (the transport parses the same markup) |
| F11 | service accounts as valid **mention targets** | humans @-mentioning agents (agent→human needs only #9403) |
| [#9470](https://github.com/makeplane/plane/pull/9470) (F7) | Public **pages CRUD** — list/retrieve/create/update/archive/delete with `description_html` exchange, a `fields=` column projection on list, and the `external_id` / `external_source` idempotency identity (project-wide unique; 409 on a duplicate pair) | `crewlet plane import`, the tool-skill sync worker, skill promotion, and the searcher's parent-page lookups |
| [#9469](https://github.com/makeplane/plane/pull/9469) (F8) | Public workspace **page search** (`GET /workspaces/{slug}/pages/search/`) honoring page access + project membership, with a match snippet per row | the query-time the Plane searcher behind the Plan-phase `## Relevant knowledge` block |
| F8.1 | **Tokenised** page search: the query is whitespace-split and matched AND-across-tokens (each token case-insensitively against page name and stripped body text), with the snippet anchored on the first matching token. The fork caps a query at **16 distinct (case-folded) tokens — above the cap it returns 400, it never truncates**; the searcher pre-trims to the cap so an over-long query still searches on its leading tokens. Without F8.1, F8 matches the whole query string as one literal substring, and the multi-keyword queries the aux LLM generates would match nothing | same — and because AND is a strict conjunction, the searcher **relaxes on zero hits**: full aux-LLM query first, then its 4-token and 2-token leading prefixes (≤3 requests), first non-empty result set wins |
| [#9398](https://github.com/makeplane/plane/pull/9398) | Webhook **CRUD** on the public token API — server-generated `secret_key` returned exactly once at creation, SSRF-guarded URLs, entity toggles | [`crewlet plane provision -public-url`](#provisioning--crewlet-plane-provision) self-registers (and repairs) the engine's workspace webhook |
| [#9399](https://github.com/makeplane/plane/pull/9399) + F10 | API-provisionable **service accounts**: `is_bot` user + workspace membership + first API token in one admin-scoped `POST`, token redacted from activity logs. F10 adds the lifecycle a reconcile needs: caller-chosen username/display name, token list/mint/rotate/revoke, and the account DELETE cascade | the provisioner's identity keystone — one service account per agent seat; `-rotate` / `-decommission` need its token lifecycle |

Three further patches are **verified at runtime** by the provisioning CLI ([capability preflight](#capability-preflight-and-degraded-modes)) rather than assumed present on the deployed build:

| Patch | What it adds | Without it |
|---|---|---|
| F13 | the `page` entity toggle on the **public** webhook serializer (#9398's serializer lacks the field #9401 added to the model, and DRF silently drops unknown request fields — no 400) | the provisioner's hook is created with `page=False` and **zero page events are delivered** (no tool-skill sync, no page routing); detected from the real create/update echo and reported as a loud note |
| F14 | service-account **create reactivates** a decommissioned account that owns the requested username (restore membership, `is_active=True`, fresh token) | [decommission is irreversible for the username](#provisioning--crewlet-plane-provision): the deactivated user row keeps it, so re-adding the seat later 409s at create |
| F15 | `username` / `is_bot` / `bot_type` appended to the public workspace-members rows (stock rows are flat `UserLiteSerializer` dicts with **no username**) | **hard-required** — the reconcile keys every create-vs-exists decision and decommission targeting on usernames; the preflight aborts naming the patch |

Against stock Plane CE the integration degrades predictably: work-item / comment / intake routing works, but there are no `page` webhooks, no public pages API or search (so no knowledge search, import, sync, or promotion), humans cannot @-mention agent service accounts, and [`crewlet plane provision`](#provisioning--crewlet-plane-provision) aborts up front (no service-accounts API — accounts are then ordinary users, provisioned [by hand](#manual-alternative)). One further fork patch — the dev toggle allowlisting private webhook URLs (F12, `WEBHOOK_ALLOW_PRIVATE_URLS=1`) — is consumed by the [local compose stack](#local-testing): the compose `plane` profile sets it in the shared app-env anchor so `host.docker.internal` webhook targets survive the SSRF guard at create **and** delivery time (the check runs in the api on create/update and in the worker on Celery delivery).

---

## Configuration

The top-level `integrations.plane` block is **non-tool config** — it enables inbound webhook handling, routing enrichment, and boot-time identity registration:

```yaml
integrations:
  plane:
    enabled: true
    url: "https://plane.nimbus.example"        # fork instance base URL — REQUIRED
    workspace: nimbus                          # workspace slug — REQUIRED
    webhook_secret: "${PLANE_WEBHOOK_SECRET}"  # X-Plane-Signature HMAC key — REQUIRED
    token: "${PLANE_ENGINE_TOKEN}"             # optional engine read credential → subscriber fan-out
    skills_project: TS                         # project holding Tool Skill pages (default TS)
    provisioning:                # consumed ONLY by `crewlet plane provision`
      role: member               #   — the engine never reads this block
      roles: { tech-lead: admin }
      username_prefix: ""
      projects: [LEAD, ENG, PROD, TS]
      token_expiry_days: 364
```

- **`url` and `workspace` are required** when enabled. Every resource path the integration touches is workspace-scoped (only `GET /api/v1/users/me/` isn't); the URL also builds shareable work-item/page links in notification metadata.
- **`webhook_secret` is required** when enabled — it is the `X-Plane-Signature` HMAC-SHA256 key, the only verification mode Plane offers. Plane **generates the secret at webhook creation** and shows it once. Keep the field **exactly one whole-value `${VAR}` reference** (a literal can never match a hook the CLI creates, and an embedded `"wh-${VAR}"` or multi-reference `"${A}${B}"` form resolves to a concatenation that never matches either — the provisioner aborts on all three): [`crewlet plane provision -public-url …`](#provisioning--crewlet-plane-provision) captures the generated secret into that var for you, or capture it by hand (see [Webhooks](#webhooks)).
- **`token` (optional, but effectively required)** — a read credential for a workspace member with access to the routed projects (the [provisioner](#provisioning--crewlet-plane-provision) mints a dedicated `crewlet-engine` service account for it). It enables **subscriber fan-out** (comments and field changes reach everyone subscribed to the work item, via fork PR #9397) and the project **UUID → identifier** resolution behind lead-fallback routing and `ENG-42` display keys — and the knowledge half leans on it too: the [tool-skill sync worker](#the-tool-skills-project) and [skill promotion](#skill-promotion-on-plane) run entirely on the engine client (no token ⇒ no sync, no promotion), and the [searcher](#query-time-knowledge-search) uses it as the fallback for roles without their own `PLANE_API_KEY`. Without it, thread activity degrades to payload assignees, and the project map can only learn from `project` webhook payloads — after an engine restart that cache is empty, so lead-fallback routing and `ENG-42` keys silently stay empty until the next `project` event. The engine warns `plane_engine_token_missing` at boot and at transport start when the token is unset. This mirrors `integrations.gitlab.token` (participants lookups) and `integrations.jira`'s admin token (watcher lookups).
- **`skills_project` (optional, default `TS`)** names the project holding [Tool Skill](../concepts/tool-skills.md) pages — see [The Tool Skills project](#the-tool-skills-project). It is a config field rather than an environment variable because it is a **routing decision**: the engine excludes that project from notification routing and from knowledge search, and a routing decision that lives outside the document describing the company is one an operator cannot see when they read their config.
- **`provisioning:`** is CLI-only input, ignored by the engine — it is read by [`crewlet plane provision`](#provisioning--crewlet-plane-provision). `role` is the default workspace role for agent service accounts (`admin` | `member` | `guest` — Plane's only roles; validated up front, so a GitLab copy-paste like `developer` aborts with the valid list instead of 400ing every seat), `roles` holds per-handle overrides, `username_prefix` prefixes every managed username (and safety-scopes decommission), `projects` lists the project identifiers every seat joins, and `token_expiry_days` is the standing token-lifetime policy (default 364; must be `>= 0`; `0` = the token never expires — Plane semantics for an omitted expiry, not GitLab's "instance default applies"; negative values are rejected by both the config model and the CLI flag, since they would silently mean never-expires too).

> **Unset `${VAR}`s fail the whole revision.** The engine re-validates the config *after* `${VAR}` resolution, so an unset `${PLANE_WEBHOOK_SECRET}` on the engine host makes `plane.webhook_secret is required when plane is enabled` fire while the revision is being applied — the entire revision fails and rolls back. Same behaviour as GitLab; export the vars before applying.

### Exclusivity rules

Validated on every config parse:

- `integrations.confluence` + an **enabled** `integrations.plane` → rejected. Both are served knowledge backends, and the knowledge base is **single-homed**: two searchers would make an agent's answer to "what do we already know about this" depend on which one was asked, and neither would be wrong. A migration between them is a cut-over, not an overlap. (It rules out Confluence *notifications* running alongside Plane as a consequence, not as the reason.) A `plane` block with `enabled: false` alongside Confluence is allowed (inert).
- `knowledge.confluence_spaces` without an `integrations.confluence` block → rejected; `knowledge.plane_projects` without an enabled `integrations.plane` → rejected. A read scope for a backend that is not there narrows nothing while reading as though it does. Since the blocks themselves are exclusive, so are the two scope lists.
- **Jira + Plane may coexist** — the exclusivity is Confluence-shaped, not tracker-shaped.

### Knowledge scope

```yaml
knowledge:
  plane_projects: []        # project identifiers; empty = unscoped (Plane ACLs bound the search)
```

`knowledge.plane_projects` is the `confluence_spaces` analog, materialised onto `org.Organization.PlaneProjects` and consumed by the [query-time the Plane searcher](#query-time-knowledge-search). As with `confluence_spaces`, it is org-wide read scope, role- and unit-independent, and a unit's `integrations.plane.project` identity does **not** feed it. Set it only to *narrow* reads to a curated floor; leave it empty to let Plane's own membership/access rules do the scoping (empty + a per-agent token ⇒ unscoped search; empty + no per-agent token ⇒ no search).

> **Membership is the search's hard precondition.** The fork's page search and pages API scope every result to projects where the *calling account* is an active member — there is no admin bypass. **Every agent seat must be a member of every project listed in `knowledge.plane_projects`** (and of any other project you expect its unscoped search to reach); a seat missing membership in a scoped project silently gets no hits from it, and a seat that is a member of none of them searches **nothing** — which reads as a broken searcher but is a membership gap. The same applies to the engine `token`'s account for credential-less roles. The [provisioner's membership step](#provisioning--crewlet-plane-provision) is where this is earned — list every project in `knowledge.plane_projects` under `provisioning.projects`, and each seat (plus the `crewlet-engine` account) becomes a member on the next run.

---

## Per-role wiring

Declare the Plane **MCP tool server** once in `mcp_servers` as a `shared: false` server; each agent supplies its own service-account token in `role.mcp_env.plane`:

```yaml
mcp_servers:
  - name: plane                       # official plane-mcp-server (stdio), version-pinned
    shared: false
    command: uvx
    args: ["plane-mcp-server@0.2.10", "stdio"]
    env:
      PLANE_BASE_URL: "https://plane.nimbus.example"
      PLANE_WORKSPACE_SLUG: "nimbus"

roles:
  - name: Agent SWE
    mcp_env:
      plane:
        PLANE_API_KEY: "${PLANE_TOKEN_SWE}"   # per-agent service-account token
```

The engine names no tool-specific variable — the whole `mcp_env.plane` block is forwarded verbatim to the role's MCP instance, and every value in it is covered by the secrets registry. Boot-time identity resolution reads the token from whichever key is present (`PLANE_API_KEY` — the official server's variable — or an `X-API-Key` header for an http-shaped server). A role with no `mcp_env.plane` gets no Plane tools and no registered Plane identity. Two wiring facts worth pinning: **`PLANE_BASE_URL` is mandatory for a self-hosted instance** (unset, the server defaults to the `https://api.plane.so` cloud gateway), and the **version pin matters** — the bundled `platform_mentions` skill gates on the server's write tools *by name*, so an unpinned upgrade could silently change the tool surface under the enforcement.

By the `skill_variables` convention, add **`plane_base_url`** and **`plane_workspace_slug`** (next to where `jira_base_url` / `confluence_base_url` would sit on an Atlassian org) so [Tool Skill](../concepts/tool-skills.md) prose can render human-clickable links. The shareable page-URL shape — the exact one the engine builds for webhook notifications and search hits — is `${plane_base_url}/${plane_workspace_slug}/projects/{project_uuid}/pages/{page_id}` (work items likewise, with `issues/{work_item_uuid}`); note the project segment is the project **UUID**, not the `ENG`-style identifier, so a skill's link template must interpolate the UUID an agent's Plane tools return. `plane_base_url` stays the bare instance URL (it must match `integrations.plane.url`); the slug is a separate variable rather than baked in, because the URL template needs them in separate positions.

Plane is not a code host — there is no git-auth recipe; sandbox-coding roles can list `plane` in `role.sandbox.mcp.servers` to read work items from inside the sandbox.

---

## Project identity

A unit (or a root-level role) declares the Plane project it owns:

```yaml
units:
  - name: Backend
    lead: Tech Lead
    integrations:
      plane:
        project: "ENG"        # project identifier → OrgUnit.plane_project
```

This is **integration identity**, exactly like `integrations.jira.project` / `integrations.confluence.space`: it is the webhook fallback-routing target (unassigned work items, intake triage, and page events in that project wake the unit's effective lead) and the project the team's agents file work under. It is **not** an MCP credential and does **not** scope knowledge reads (read scope is the org-wide `knowledge.plane_projects` only). Identifiers are matched case-insensitively (uppercased in the lead map); duplicate identifiers across units are first-wins, warning only when they resolve to **different** leads — several units sharing one project under one lead is the documented Jira-parity pattern and stays silent; human seats may not carry a `plane_project`.

---

## Webhooks

Inbound Plane events arrive at **`POST /webhooks/plane`**.

### Setup

The paved road is [`crewlet plane provision -public-url https://engine.example.com`](#provisioning--crewlet-plane-provision): it registers the one workspace webhook (#9398) at `<public-url>/webhooks/plane`, sets the entity toggles, and captures Plane's server-generated secret into the `${VAR}` behind `webhook_secret`. To create the hook by hand instead — in the fork UI (workspace settings → webhooks) or via the public webhook API:

1. Point the webhook at the engine: `https://engine.example.com/webhooks/plane`.
2. Enable the **project**, **issue**, and **issue-comment** entity toggles (plus **page** on the fork — #9401/F13). Leave **cycle** and **module** off — the router drops them anyway, and they are inbox noise. Intake events are delivered to every active workspace webhook unconditionally (CE has no toggle for them).
3. Plane **generates the secret** at creation and shows it once — capture it into the `${PLANE_WEBHOOK_SECRET}` var that `integrations.plane.webhook_secret` references.

> **Auto-disable.** Plane retries a failed delivery 5× with exponential backoff, then **auto-disables** the webhook (`is_active=False`). A [provisioning re-run](#provisioning--crewlet-plane-provision) repairs it (`is_active=True` is re-asserted on every update); by hand, flip it back on in the workspace settings. The engine's unconfigured-drop returns 200 precisely so an engine that is up but not yet configured never poisons the retry counter.

### Verification

The `X-Plane-Signature` header carries the HMAC-SHA256 **hexdigest of the raw body** keyed with `webhook_secret` (Plane CE's only scheme), compared constant-time. Invalid or missing signature → **401**; no secret configured → **503** with `Retry-After` (the request is fine; what is missing is on this side, so the delivery is held for retry rather than discarded as a 4xx would tell the sender to do); malformed JSON → **400**; engine unconfigured → **200** `{"status": "dropped"}` — verified *first*, so forgeries never earn a 200.

CE payloads carry no stable delivery id (`X-Plane-Delivery` is per-attempt), so the transport deduplicates on the event coordinates *plus* the activity discriminator (Plane fires one webhook per changed field with an identical `data` snapshot — a bulk edit is N deliveries differing only in `activity`), with a 5-minute TTL covering queue redelivery and operator replay.

### Event routing

the Plane transport turns one payload into a **list** of per-recipient notifications, following the GitLab two-layer model: **directed targets** come straight from the payload; **thread activity** fans out to the work item's **subscribers** (fork PR #9397, via the engine `token`), degrading to payload assignees when the client is missing, the lookup fails, or nothing is extractable. The trigger actor is excluded from every fan-out, each recipient gets one copy per event (first, highest-signal reason wins), and every copy carries `metadata.actor_external_id` so the generic self-action guard catches any fall-through.

**Every directed fan-out** (assignee, added assignee, subscriber, mention) is intersected with the **registered** Plane UUIDs (agents ∪ human seats) at a single choke point, so ordinary workspace humans never produce undeliverable copies; the subscriber-vs-assignee degrade decision is made on the *pre-intersection* list — an all-outsider subscriber list is a successful lookup (nothing routes), not a reason to fall back to assignees. Without a handle registry, mentions route to **nobody** (fail closed).

| Event | Routed to | `routed_via` |
|---|---|---|
| `issue.create` (with assignees) | each **registered** assignee ≠ actor; when every non-actor assignee is an unregistered outsider, falls through to the project lead so the ticket still wakes the owner (a purely self-assigned create stays silent) | `assignee` (all-outsider: `project_lead_fallback`) |
| `issue.create` (no assignees) | project lead via the `plane_project` map — the "new ticket wakes the lead" flow; unresolvable identifier or unmapped project → nothing (never a misroute) | `project_lead_fallback` |
| `issue.update`, `activity.field == "assignees"` | the **newly added** assignee (directed); removals wake nobody | `assignee_added` |
| `issue.update` (any other field) | **subscribers** minus actor, degrading to payload assignees | `subscriber` (degraded: `assignee`) |
| `issue.delete` | payload assignees directly — the subscribers endpoint cannot serve a deleted work item, so no REST call | `assignee` |
| `issue_comment.create` / `.update` | 1) @-mentions parsed from `comment_html` `<mention-component entity_identifier="…">` markup; 2) then subscribers, degrading to assignees | `mention`, then `subscriber` |
| `issue_comment.delete` | dropped — the payload carries no content to act on | — |
| `intake_issue` (any action) | project lead — Plane's intake is the unassigned-inbound surface, triage falls to the lead | `intake_triage` |
| `page.created` / `.updated` / `.deleted` (#9401/F9; a duplicate emits `created`) | the tool-skill **index hook fires first, always** (even for excluded projects and self-edits) — it feeds the [`PlaneSkillSyncWorker`](#the-tool-skills-project); then the [Tool Skills project](#the-tool-skills-project) is excluded from routing; a page whose project cannot be resolved **fails closed** (no routing); otherwise the project lead | `page_project_lead` |
| `project` (any action) | no routing — the payload feeds the project UUID→identifier cache (delete evicts) | — |
| `cycle` / `module` / `user` / unknown | dropped at debug | — |

**Page-routing parity, stated honestly:** Confluence routes page events through watchers → mentions → space leads; CE Plane pages have no subscription model and no page comments, so page events wake the **project lead only**, and page discussion happens where Plane puts it — on work items, where the full routing applies.

Directed copies carry the target under `metadata.plane_user_id`; lead copies carry `recipient_handle`. Metadata also includes the project identifier, `issue_id` / `page_id` / `comment_id`, the display key `work_item_key` (`ENG-42`, when the project map is warm), a shareable `url` (`{url}/{workspace}/projects/{project_id}/issues/{issue_id}`, pages likewise), and `event_type` as `"{event}.{action}"`. A human seat resolved via `contact.plane_user_id` is *recognised and skipped* — Plane already notified the human natively.

Bursts on the same work item collapse into one digest turn via **inbox coalescing**. The conversation key is the work item's **UUID** (`plane:<issue-uuid>`; pages key on the page UUID) — deliberately not `ENG-42`, which only rides `issue` payloads and needs a warm project cache; the human key stays display-only so comments and field updates on one work item can never split into two digest partitions.

### Notification prompts

Plane webhooks use the Plane notification prompt (`internal/plane/prompt.go`), which dispatches on `routed_via` — *why* the recipient was woken:

| `routed_via` | Prompt behaviour |
|---|---|
| `assignee` / `assignee_added` | Actionable — read the work item, do the work, post one completion summary; reassign via `lookup_colleague` rather than going silent |
| `mention` | Evaluate — were you actually asked to do something? Respond with substance on the same thread, or stay silent on a passing cc |
| `intake_triage` | Triage — an explicit **delegate / take it yourself / escalate** decision block (the Jira lead-fallback analog) |
| `project_lead_fallback` | Generic description + the same "Why You Received This" delegate/take/escalate block |
| `page_project_lead` | Page change — skip if unrelated, act or flag if relevant |
| `subscriber` / anything else | Generic evaluate-and-skip fallback for thread activity the recipient merely watches |

Every prompt with an issue/page pointer appends a **Get Full Context** block ("use your Plane tools to fetch the work item / page" — never a specific tool name), and those events are pointer events (`requires_recon`). In coalesced digests, comment bodies are kept and stale description snapshots collapse, like Jira's.

---

## Identity registration

`register_plane_accounts_from_org` resolves each seat's Plane identity so webhooks can route to it: for every role with a token in `mcp_env.plane`, it calls **`GET {integrations.plane.url}/api/v1/users/me/`** with that token and registers the returned `(plane, user-UUID) → agent handle` mapping in the HandleRegistry. This is REST, not an MCP round-trip — `GET /users/me/` is stable public API and needs only the role's own token. Roles whose `${VAR}` credential is unresolved are skipped; if two seats resolve to the same UUID, the mapping is dropped with a warning rather than misrouting.

Registration runs at **engine startup**, on live **`integrations.plane` config changes** (the `PUT /config/integrations/plane` diff re-resolves every seat), and when **roles are added live** (only the just-added seats are resolved, and that role-add refresh is additionally gated on a running MCP bridge). Rotating an *existing* seat's `mcp_env.plane` token in place is **not** re-resolved live — an engine restart or an `integrations.plane` PUT is required to pick up the new identity.

**Human seats** register their `contact.plane_user_id` — the workspace-member user UUID, discoverable via `GET /api/v1/workspaces/{slug}/members/` — through the same `CONTACT_FIELD_BY_TRANSPORT` map, so a founder's or teammate's Plane activity is attributed by name and their assignments/mentions route natively. Agent→human @-mentions need no engine plumbing: the mention markup is written by the MCP server, and the seat's `plane_user_id` is discoverable to agents via `lookup_colleague` once registered.

Per-agent tokens are covered end to end: `mcp_env` values are masked by the secrets registry, and transcript redaction scrubs `plane_api_…` tokens and `plane_wh_…` webhook secrets from tool output.

---

## Query-time knowledge search

With Plane enabled, the engine constructs a **the Plane searcher** (`internal/knowledge`) — the Plane backend of the [`knowledge.Searcher` seam](../concepts/knowledge-system.md#the-knowledgesearcher-seam) behind the Plan-phase `## Relevant knowledge` block. Once per turn, the role's auxiliary LLM turns the trigger context into a short keyword query; the searcher sends it **verbatim** to the fork's workspace page-search endpoint (F8 — patch F8.1 tokenises it server-side, AND-across-tokens) and renders the hits as title + snippet bullets. Agents follow up with their own `plane` MCP tools (page read/search) — the block prose describes the capability, never a tool name.

- **Authentication** mirrors the Confluence searcher: the search runs as the **agent's own Plane user** when the role carries `mcp_env.plane.PLANE_API_KEY` — Plane then enforces project membership and page access natively — falling back to the engine's `integrations.plane.token` read client. A credential-less role on a token-less engine searches nothing.
- **Scope** is the org-wide [`knowledge.plane_projects`](#knowledge-scope) list, resolved from identifiers to project UUIDs through the transport's shared cache (case-insensitive). A non-empty scope that resolves to **zero** projects searches nothing, with a warning — it never silently widens to everything the credential can see. Empty scope: unscoped for self-authenticating roles, nothing for credential-less ones. Remember the [membership precondition](#knowledge-scope).
- **Ordering is recency, not relevance.** The fork's page search (F8) orders results by `-updated_at`, so the top hit is the most recently *edited* match rather than the closest one. The searcher preserves the server order, which means a strongly-matching but stale page can be outranked by a recently touched one.
- **Two result exclusions**: pages in the [Tool Skills project](#the-tool-skills-project) are dropped wholesale (skill content reaches agents through the skill catalogue / `load_tool_skill`, and a skill page's leading YAML block would dominate any snippet), and unreviewed [auto-drafts](#skill-promotion-on-plane) are hidden by parent — rows whose parent is the project's `Auto-Drafted Skills` page. When that parent lookup fails, a fail-closed backstop hides rows whose title starts with `[Auto-draft] ` instead, so an outage hides drafts rather than leaking them. The draft filter is depth-1 (direct children) rather than Confluence's full ancestor chain — drafts are flat leaf pages by construction.
- **Both exclusions fail closed**, like the transport's page routing: a hit whose `project_id` is absent is dropped, and when the configured skills project can't be resolved to a UUID, only hits positively attributable to a *different* project survive (with a `plane_search_skills_project_unresolved` warning) — an outage hides content rather than leaking skill YAML or drafts into prompts. A failed project-resolution *request* fails the whole search closed to no hits. The scope and the skills project resolve in **one** `resolve_project_ids` call per search, so a token-less engine's per-agent fallback pays a single `list_projects` walk per call.
- **Best-effort**: any failure returns no hits and the block renders empty; archived pages are excluded server-side, and a private page is visible only to its owner.

Hit URLs use the shareable shape `{url}/{workspace}/projects/{project_uuid}/pages/{page_id}` — the same one notification metadata carries (and the one to mirror via the [`plane_base_url` + `plane_workspace_slug` skill variables](#per-role-wiring)).

---

## Publishing docs + skills — `crewlet plane import`

`crewlet plane import <company.yaml> <directory>` is the **unified publisher**. The first positional is the Tier B company YAML (credentials from its `integrations.plane` block); the second is the directory to walk. It reads every `.md` beneath it, recursively, and routes each file **by what the file declares**:

- **`trigger:` present ⇒ a [Tool Skill](../concepts/tool-skills.md)** — published into the [Tool Skills project](#the-tool-skills-project) (`integrations.plane.skills_project`, default `TS`) with a leading YAML code block the engine parses back out. **Its directory is ignored**: a skill is identified by what it declares, so one filed under `ENG/` is still a skill, and publishing it there as prose would put an instruction meant for one phase of one turn into a planner's context.
- **otherwise ⇒ a knowledge doc** — published as clean prose to the project whose **identifier is the file's immediate parent directory name** (`<root>/ENG/onboarding.md` → project `ENG`), titled by its first `# H1`. Docs surface through the [query-time search](#query-time-knowledge-search); they are never loaded into a registry.

The **title comes from the H1** rather than the filename, because it is the page name *and* half the idempotency key: a filename is the thing an operator renames most casually, and a rename would orphan the published page and leave a second one beside it. Frontmatter may override the title (`title:`) and the container (`project:`, or `space:` — the two backends' words for the same thing), and overrides nothing else: the file's location in the tree is the convention, and a frontmatter that could redirect a doc anywhere would make the tree meaningless. A file with no determinable title **stops the walk** naming the fix — a doc that cannot be titled cannot be found again, and skipping it quietly would report success while leaving it unpublished. So do two files that would publish as the same page: the second would overwrite the first on every run, and which one wins would depend on walk order.

Markdown is rendered as **GitHub-flavoured** — the tables, task lists and autolinks in real knowledge docs render as literal pipes and brackets under plain CommonMark. The title heading is removed from the body, because Plane renders the page title itself and leaving it prints the same words twice.

**Idempotency is the fork's `external_id` contract.** Every page the importer publishes is stamped `external_source="crewlet"` with `external_id="skill:<key>"` (skills, keyed by frontmatter `key`) or `external_id="doc:<title>"` (docs, keyed by the authored H1). Consequences worth knowing:

- Re-running matches by external identity — one narrow page enumeration per target project, no per-file lookups — so **retitling a page in Plane's UI never orphans it**. A re-import always writes: this is a publisher, and an import that skipped existing pages would mean an edited file never reaching the workspace.
- A page an operator created **by hand** under the same title is **adopted** and stamped, so the next run finds it by id. Only a page with *no external identity at all* is adoptable: one carrying any — this tool under a different id, or another tool whose ids happen to look like these — belongs to whoever set it, and adopting it would overwrite somebody else's page and then fight over it on every run.
- Where two unclaimed pages share a title, the **lowest page id** is adopted. Plane guarantees no enumeration order, so without a rule the adopted page would be a coin flip and a later import would write to the other one.
- Every page is created with `access` **public**, explicitly — a private Plane page is invisible to every non-owner on both read paths.
- **Per-page failure isolation**: a write failure on one page — a **locked** page (a normal Plane UI gesture) or a page-level 403 — costs that page. The run keeps publishing the rest and then **exits non-zero naming the failures**, so whatever ran it is not told the import succeeded.

**Pre-flight**: every distinct target project (the skills project ∪ the doc parent-directory names) must already exist, matched case-insensitively against the workspace's project identifiers. Any missing project fails the run **before a single page is written**, naming what the workspace does have — half an import is worse than none, because the half that landed looks like a complete knowledge base with holes in it. **The importer never creates projects**: a container the whole company works in should not be named by a guess. That is the provisioner's job ([`crewlet plane provision -create-projects`](#provisioning--crewlet-plane-provision)).

**Flags**: `-token` (an API key; empty reads `$PLANE_TOKEN`, then `integrations.plane.token`), `-dry-run` (print the routed plan and write nothing — the *same* plan the run uses), and `-prune`:

- `-prune` deletes **orphaned skill pages only**, on a positive-marker predicate: `external_source == "crewlet"` **and** a `skill:` external id whose key no local file publishes. Unmarked pages, `doc:` pages and knowledge docs are structurally out of reach — a doc absent from this run is far more likely to have moved than to be dead, and an unmarked page was somebody's own. Deletion follows the fork's **archive-then-delete** precondition (a live page 400s on `DELETE`), per page, with failure isolation. When the archive lands and the **delete** is refused — deletion is owner-or-project-admin only, so a 403 on a human-owned page is the expected case — the archive is **rolled back**: left archived, the page is hidden from users and from every later enumeration while its `external_id` keeps 409ing every future republish of that skill. A failed prune has to be a no-op, not a half-removal. An orphan somebody already archived is still deleted, for the same reason: it is holding an id. If even the unarchive fails, the failure says the page was left archived and names the manual fix.

`crewlet plane resync <company.yaml>` is the read-only diagnostic: it runs the **same** walk and the **same** admission the engine's boot sync runs, against a throwaway registry, and prints the keys that loaded plus any page that declares a trigger and does not parse. It does **not** reach into a running engine — a live engine receives page webhooks directly, so this answers "why is this skill not being applied", not "make it apply". `-project` targets a project other than the configured one, for checking a container before pointing the company at it. It exits non-zero when a page declares a trigger and fails to decode: the only other symptom is guidance that never appears.

### Onboarding convention

As on Confluence, each unit's project (plus the org root's) can host one page titled exactly **`Onboarding`** that fresh agents are nudged to read on their first turn — name the file `Onboarding.md` and the title falls out of the H1. Note that Plane does **not** enforce per-project title uniqueness (Confluence 400s on a duplicate title; Plane doesn't), so "one `Onboarding` page per project" is convention: the importer itself is idempotent by external id, but nothing stops a human from creating a second page with the same title.

---

## The Tool Skills project

`integrations.plane.skills_project` (default `TS`) names the Plane project where [Tool Skill](../concepts/tool-skills.md) pages live. Don't add it to `knowledge.plane_projects`; the [searcher](#query-time-knowledge-search) drops its pages from results regardless, and the engine **excludes it from notification routing** (page edits there have no human or agent recipient by design, and without the exclusion every skill edit falls through to lead routing as an undeliverable notification).

There is no "off" switch and none is needed: a company that publishes no tool skills has no project by that identifier, so both exclusions match nothing and cost nothing. The one company that must set this field is one whose skills live somewhere other than the reserved default — or one that uses `TS` as an ordinary work project, which the default would otherwise hide from knowledge search.

The **`PlaneSkillSyncWorker`** (`internal/agent/skills`) keeps the in-memory skill registry in step with that project:

- **Boot** — ONE strict, paginated enumeration of the project carrying `description_html` (the fork's list rows serialize the body, so there is no per-page fetch fan-out). The walk is all-or-nothing: an incomplete enumeration (pagination cap, cursor stall, transport error) **never seeds the registry** — a wholesale replace from partial rows would silently delete skills. A failed walk **retries with exponential backoff** (5 attempts, from 5 s — sized for the compose boot race where Plane's API comes up seconds after the engine); the failure is logged as `tool_skill_sync_list_failed` (a *reachability* problem, retryable) as distinct from `tool_skill_project_not_found` (the identifier is genuinely absent — a configuration problem). When every attempt fails, `tool_skill_resync_exhausted` (error) says so explicitly: the registry keeps whatever it currently holds; fix the backend and re-apply the integrations config (or restart) to re-walk. The same bounded retry covers the re-seed after a live backend cut-over, so a cut-over whose first walk fails cannot silently keep serving the old backend's skills without telling the operator. Archived pages are excluded by the server default.
- **Runtime** — the transport's index callback fires for **every** `page` webhook workspace-wide; the worker scopes it via the fork's project-scoped page GET, which 404s for pages in other projects (404 ⇒ evict, a no-op when the registry never held the page). A `deleted` action evicts by page id; **any other action** — `created`, `updated` (including the 60 s-debounced content-persist flush), or an unknown future one — fetches the page and re-runs the admission predicate.
- **Admission predicate** (identical for boot and webhooks): the page lives in the skills project **and** its body decodes as a skill (a leading YAML code block with a `trigger`). A fetched page failing it is **evicted, not skipped**: an **archived** page (non-null `archived_at` — archive is the operator's "remove" gesture, since delete requires archive first), a page moved to another project, or a page edited into a non-skill must not keep serving its last-good body. Pages carrying the importer's `external_source="crewlet"` marker log their decode failures at `info` (they *should* decode); stray operator pages and the project home page skip quietly.

Editing a skill is therefore a Plane-native workflow: open the page, edit, save — the content-persist webhook lands within ~60 s and the registry updates, no restart. `crewlet plane resync` is the drift-recovery diagnostic (see the [CLI reference](../reference/cli.md#crewlet-plane-resync)). The page representation (YAML code block + prose body) is documented in [Tool Skills](../concepts/tool-skills.md#page-representation).

---

## Skill promotion on Plane

The [cross-agent skill promotion](../concepts/agent-learning.md) write path works on Plane through the `PlanePromotionWriter` (`internal/plane`), using the engine's read client — an engine `token` is required (without one the promotion pass soft-skips and retries next tick):

- The draft's target container is the unit's **`integrations.plane.project`** identity; a unit without one soft-skips with the hint `set integrations.plane.project on the unit`.
- **Cross-tick dedup by external identity.** The scheduler re-clusters the same persisted rows every tick, and Plane enforces no title uniqueness — so the writer stamps every draft with `external_source="crewlet"` + `external_id="draft:<name>"` (keyed on the LLM-picked kebab name, which stays stable across title-prefix changes and lead retitles), looks the identity up before creating (exact-title fallback adopts pre-stamp legacy drafts), and **returns the existing page instead of creating another**. The fork's project-wide 409 on the duplicate pair is the hard backstop for anything the lookup missed (a retitled draft, a race) — mapped to a no-op for that tick. One converging cluster therefore produces **one** draft, ever, not one per hourly tick. The `Auto-Drafted Skills` parent itself is stamped `doc:Auto-Drafted Skills`, closing its own lookup/create race the same way.
- Drafts land under the project's **`Auto-Drafted Skills`** parent page, which the writer **ensures exists** (creating it on first use) — the parent is load-bearing, because the [searcher hides drafts by parent-id](#query-time-knowledge-search). A *failed* parent lookup never creates (that could duplicate an existing parent); the draft then lands at the project root, where the `[Auto-draft] ` title-prefix backstop hides it from search **only while the searcher's own parent lookup is failing too** (the plausible correlated outage). Once the backend recovers, a root-level draft is no longer parent-excluded and surfaces in search — move it under the parent (or review it) promptly; the `promotion_no_parent_page` log flags the case.
- Draft titles are prefixed **`[Auto-draft] `**; bodies are rendered markdown; every page is created `access` public.
- **Publish gesture** — Confluence parity, one UI action: a unit lead reviews the draft and **moves the page out of the `Auto-Drafted Skills` parent**. Renaming is optional — dropping the `[Auto-draft] ` prefix is cosmetic, since the title backstop only applies when the parent lookup fails.

---

## Provisioning — `crewlet plane provision`

> **Prerequisites.** Provisioning talks to a **running fork deployment** — the compose `plane` profile for [local testing](#local-testing), or the fork's `ghcr.io/crewlet/plane-*` images (tag `preview`, see [the fork](#the-fork)) deployed on a server — with the **workspace already created** and a **founder (workspace-admin) account** whose personal API token is the operator credential below. Locally, `docker compose --profile plane up -d` plus `scripts/plane-dev-bootstrap.sh` covers all of this; on a server, create the workspace and founder account in the Plane UI first.

```
crewlet plane provision my_company.yaml \
    -admin-token <workspace-admin API key> \
    -secret-store \
    -public-url https://engine.example.com \
    -create-projects
```

A one-shot, **idempotent reconcile** from company config to Plane state — the [GitLab provisioner](gitlab.md#provisioning)'s contract ported to Plane. For every **agent** seat whose `mcp_env.plane.PLANE_API_KEY` is a whole `${VAR}` reference it ensures a service account, project memberships, and an API key minted into that variable; it also provisions the engine's `crewlet-engine` read account and self-registers the [workspace webhook](#webhooks). **Re-runs leave a working credential alone** — see [Why a re-run does not rotate](#why-a-re-run-does-not-rotate). Operator-actionable conditions (stock CE, a non-admin token, an unknown project, a workspace whose member rows carry no username) abort **before anything is created**, and a run that cannot finish undoes what it made. Flags and defaults are in the [CLI reference](../reference/cli.md#crewlet-plane-provision).

> **First run: `webhook_secret` must be exactly one `${VAR}` reference.** Config validation requires a non-empty `webhook_secret` whenever Plane is enabled, and it runs *before* the provisioner that supplies the secret. The way out of the chicken-and-egg is `webhook_secret: "${PLANE_WEBHOOK_SECRET}"` — a reference passes load-time validation with the var still unset, Plane generates the secret server-side when the CLI creates the hook, and the CLI captures it into that var. A literal, an embedded `"wh-${VAR}"`, or a multi-reference `"${A}${B}"` is refused: none of them can hold a secret the CLI captures, so every delivery would fail HMAC verification until Plane auto-disables the hook.

### Operator credential

A **workspace-admin API key**, passed via `-admin-token` or `$PLANE_ADMIN_TOKEN` — created once in the Plane UI by the first human, never stored in company config. The seats' own keys are what this run *mints*, so it cannot bootstrap itself from them. Every provisioning surface (service accounts, tokens, webhooks, the members list) is gated on Plane's workspace-owner permission, so there is no instance-admin mode and no `--mode` flag — Plane provisioning is workspace-scoped, full stop.

- **`-create-projects`** makes the *operator* each created project's admin, with `name = identifier` — config carries only identifiers; rename in the Plane UI at will.
- **Pre-existing projects** are visible to the operator only through membership, and project-member writes are project-admin-gated. A declared project the operator cannot see is indistinguishable from one that does not exist, so the run aborts naming what the workspace *does* have — before anything is created. Make the operator a member of every pre-existing `provisioning.projects` entry, or let `-create-projects` create them fresh.

### Capability preflight and degraded modes

Nothing mutates until the CLI has probed what the instance supports — because a run that discovered a missing capability halfway would leave some accounts created, some tokens live, and an operator working out which.

The preflight opens with two read probes that pin the *cause* of a rejection, since status codes alone cannot separate "bad token" from "not an admin": first `GET /users/me/` (any failure there is the credential itself, and without this call every later 403 is unreadable), then the cheapest membership-scoped workspace route (`GET …/projects/`, which rejects a wrong slug whatever the token's permissions are). Only then the capability probes, which use deliberately **disallowed HTTP methods** against the fork's routes so they can never write: a `GET` against the POST-only service-accounts route, a `PATCH` against a token collection under the zero UUID. **404 is the only absence** — a 405 is the route rejecting the method and a 403 is its permission class refusing this credential, and both prove the route is there. A request that never lands is an error, not an answer: reading a dropped connection as absence would tell an operator their fork lacks a feature because the network blinked.

| Instance | Behaviour |
|---|---|
| `GET /users/me/` rejects the key (401 **or** 403 — Plane does not pin which) | **Abort**: the operator credential is invalid, expired, or revoked |
| Credential good, workspace slug unknown or invisible | **Abort** naming `integrations.plane.workspace` |
| No service-accounts route at all (stock Plane CE) | **Abort**: provisioning requires the crewlet/plane fork (`preview`) |
| Routes present, operator not a workspace admin | **Abort**: create the key from a workspace-admin account |
| No webhook API (#9398 absent) | **Abort** — an agent could not be woken by anything happening in the workspace |
| Service accounts present, **token lifecycle absent** | **Degraded mode**, with a note: a *new* seat still works because creating an account mints its first token, and memberships and the webhook still reconcile — but a seat that already has an account cannot be re-minted or rotated, and is named individually with the remedy (delete the account so the next run creates it afresh) |
| Members rows lack `username` | **Abort** — an account created for a seat could never be found again, so every run would create another one and write its token over the live one |
| Create response echoes a synthetic `svc_<uuid>` username (upstream service accounts without the fork's identity support) | **Abort** — and the just-created account is **deleted first**, so the run is a true no-op rather than leaving an orphan the next run cannot find |
| Webhook create/update does not echo `page: true` | Proceed with a loud note: page events will **not** be delivered — the [Tool Skills sync](#the-tool-skills-project) keeps serving what it read at boot, and no error is ever raised |

### What a run does

1. **Ensure the service account.** Look up `{username_prefix}{handle}` case-insensitively in the workspace-members walk; create if missing, passing the caller-chosen username, `display_name = role.name`, and an **explicit** workspace role — the API default is `admin`, silently privilege-escalating for an omitting caller, so `provisioning.roles[handle]` / `provisioning.role` is always sent and an unmapped value falls to the *least* privilege. A matched members row that is **not a service account** (a human holding the derived username) aborts before any write, naming `provisioning.username_prefix` as the remedy: an agent must never be provisioned onto a person's identity, or everything it does is attributed to them. A seat whose `PLANE_API_KEY` is not a whole `${VAR}` reference is reported and skipped — an operator managing that credential by hand is a supported choice, it just cannot be minted into. **Two seats naming one variable is refused**: it can hold only one identity, so the second would overwrite the first and that agent would authenticate as the second, with nothing anywhere reporting a problem.
2. **Ensure memberships.** `provisioning.projects` identifiers resolve case-insensitively against the workspace's projects, **before any mutation**; an unknown identifier aborts naming what exists, unless `-create-projects` is passed. Each seat is then added to every resolved project. A duplicate add is success — the fork answers 409 on some paths and stock CE maps the same constraint violation to a *generic* 400, and a run that read either as failure could never run twice — but only out of those two statuses: an unhandled integrity error is a 500 whose body also says "duplicate", and the membership it was making may not exist.
3. **Ensure a credential.** A newly created account is normalised to the invariant every later run depends on — **exactly one active token, under this tool's own label** — by minting a labelled token, recording it, and *then* retiring the create-response one, in that order so the recorded value is always live. On an existing account, see [Why a re-run does not rotate](#why-a-re-run-does-not-rotate). Rotation revokes only the tool's own previous token and only rows that are still active: an administrator's hand-made token on the same account is left alone, and a row a previous rotation already retired is not re-revoked (every rotation leaves another, so a run without that check issues one more pointless request than the run before it, for ever). Expiry comes from `provisioning.token_expiry_days` or the `-token-expiry-days` one-off override; `0` omits `expired_at`, which in Plane means the token **never expires**, and never-expires is the default — nothing in Crewlet renews a credential on a schedule, so an expiry nobody renews is an outage with a date on it. An expired token is not a live one whatever `is_active` says: the flag records revocation, not the calendar.
4. **Ensure the engine account.** Iff `integrations.plane.token` is a `${VAR}` reference: service account `{username_prefix}engine`, workspace role **`member`** — never `guest`, and never the config default. Plane has no read-only role, and guest visibility is restricted to own/assigned items, which would break subscriber fan-out, member/project resolution, and page fetch; `member` is the least role that guarantees the engine's read paths, and the engine writes nothing, which is what makes `admin` wrong in the other direction. It joins every configured project (page reads and the search fallback are membership-scoped like any seat's). A `token:` that is unset or a literal leaves the engine account untouched, and the run says what that costs: routing falls back to the targets a payload happens to name.
5. **Ensure the workspace webhook** (with `-public-url` only; without it nothing is guessed, because a hook pointing at the wrong host is worse than no hook — the workspace then reports a healthy integration). One hook at `<public-url>/webhooks/plane`, matched **byte-exact**, because that is what identifies "our" hook and a run that reconfigured the first one it found would take down an unrelated integration. Entity toggles: `project` / `issue` / `issue_comment` / `page` on, `cycle` / `module` off — a delivery for an entity nothing routes is a signed request the engine verifies, stores and then drops. Plane's duplicate check is byte-exact too, so a hook differing only in a trailing slash or an explicit `:443` is a **second** hook that fires on the same events: it is reported loudly and **never deleted**, since a foreign hook is not this run's to remove. The secret is **generated by Plane and returned exactly once** — the inversion of GitLab, where crewlet supplies the secret and can re-stamp it — so: no hook ⇒ create and capture the secret into the `webhook_secret` `${VAR}`; hook exists ⇒ its toggles are brought in line and its secret is *not* re-read, which is said in a note only when the sink does not already hold it. The destructive recovery — delete and recreate for a fresh secret — is gated behind **`-recreate-webhook`**, because it invalidates the secret every *other* deployment holds.
6. **Decommission** (`-decommission`, explicit, never default). Managed accounts whose seats left the config get the DELETE cascade: every token deactivated, project and workspace memberships removed, the user deactivated with the row **kept** for attribution. "Managed" means both halves — the username starts with `provisioning.username_prefix` (which is never empty; a company that clears it gets `crewlet-`, and that default is the safety property) **and** the account is a service account. A person whose name matches the prefix is left alone and reported, because a wrong delete here has no undo. A 404 is an already-decommissioned account, so re-runs are safe.

Every mutation is undone when the run cannot finish: minted tokens are revoked, accounts this run created are deleted, a webhook it registered is removed, and the sink is cleared — through a **detached context**, because the failure is often the cancellation itself and a rollback that inherited it would do nothing at all. The original error is reported with the cleanup's own problems appended, never replaced by them: the reason the run stopped is what has to be fixed.

Human seats are **validated, never created**: a `contact.plane_user_id` that is not a workspace member becomes a note naming the seat — a wrong UUID addresses nobody *silently*, the assignment lands, the mention renders as raw markup, and no notification is ever delivered. A `${VAR}` reference is not checked (it resolves at run time against an environment this command does not have), and human seats with no id at all are named together beside the member table that fills them in.

### Why a re-run does not rotate

Plane serves a token's value **once**, so a provisioner has no way to verify that what it recorded last time still matches. The tempting answer is to mint every run — and that is an outage: the engine is running with the *old* value, so rotating revokes the credential every agent is currently authenticating with. An operator adding a tenth seat would take the other nine down, from a command whose whole promise is that it is safe to re-run.

So a seat is left alone when **both** halves hold:

- the variable holding its key still has a value — answered by the sink the run is writing to, and an *unreadable* sink stops the run rather than being read as empty, because reading it as empty would rotate every live credential in the company because a store blinked; and
- the account still holds a usable token under this tool's label.

Either alone is wrong: a recorded value whose token was revoked leaves an agent 401ing for ever, and a live token nobody wrote down cannot be deployed anywhere. Everything else is minted. A seat whose token was live but **unrecorded** (lost env file, a second machine) is minted into and reported as needing an engine restart — the value cannot be read back, so minting is the only recovery, and it costs the restart.

`-rotate` mints for every seat regardless. It is the operator asking, having planned the restart that follows.

### The report

Created accounts, minted seats, seats **left alone** (said out loud — it is the successful outcome of a re-run, and a silent report reads as a run that did nothing), decommissioned accounts, projects joined, the webhook target, then the notes. The report **ends with the workspace member table** — username, user UUID, kind, email — because the ids are what a founder copies into [`contact.plane_user_id`](#identity-registration) and Plane's own UI does not show them anywhere.

Minted values leave the process through one of three sinks, and exactly one must be named — a run with nowhere to put what it mints would create live credentials at the vendor and print none of them, the worst outcome available: the encrypted [secret store](../concepts/secret-store.md) (`-secret-store` — the engine reads it back directly, so nothing has to be sourced or restarted into place), an env file (`-env-file PATH`, created `0600` at open and rewritten atomically, keeping every credential it did not write), or `-print` (`export VAR=token` lines to stdout, persisting nothing). Every credential Plane returns is unretrievable after the response that carried it, so each sink persists **write-through** as it mints rather than buffering to the end of the run.

### Manual alternative

The CLI is the paved road, but hand-provisioning still works — and is the only road on stock CE, with ordinary users in place of service accounts:

1. Create one service account per agent seat: the fork's `create_service_account` management command, or an admin-scoped `POST /api/v1/workspaces/{slug}/service-accounts/` (#9399).
2. Add each account to the projects it works in — including **every project in `knowledge.plane_projects`** (page search and page reads are membership-scoped; see [Knowledge scope](#knowledge-scope)).
3. Mint an API token per account into the `${VAR}` the seat's `mcp_env.plane.PLANE_API_KEY` references (e.g. `PLANE_TOKEN_SWE`).
4. Mint the engine read token — for a member with access to every routed project — into the `${VAR}` `integrations.plane.token` references.
5. Create the workspace webhook and capture its generated secret into the `webhook_secret` `${VAR}` (see [Setup](#setup)).
6. For each human seat, look up their user UUID via `GET /api/v1/workspaces/{slug}/members/` and set `contact.plane_user_id`.

---

## Local testing

A complete local fork instance ships in the main **`docker-compose.yml`** under the **`plane` profile** (thirteen services: api, worker, beat, one-shot migrator, web, space, admin, live, proxy + postgres, valkey, rabbitmq, rustfs) plus **`scripts/plane-dev-bootstrap.sh`**, so the whole loop — provision, webhooks, knowledge search, skill sync, mentions — is testable on a laptop without a hosted Plane. The images are the [fork](#the-fork)'s own (`ghcr.io/crewlet/plane-*`, tag `preview`); third-party infra tracks the newest compatible stable lines (postgres stays on the 15 major the fork's upstream matrix pins; the S3 store is [RustFS](https://github.com/rustfs/rustfs) — MinIO's community image is de-facto deprecated). The UI lands on **`http://localhost:8091`** (80/8080/8090/8150/8929 are all taken in this repo's dev story), and the whole stack needs **~2–2.5 GB RAM**. The compose file also bakes in the two dev-loop necessities: fork patch **F12** (`WEBHOOK_ALLOW_PRIVATE_URLS=1` on api *and* worker — delivery runs in the worker) so webhooks may target the private `host.docker.internal`, and the `host.docker.internal: host-gateway` mapping itself.

```bash
docker compose --profile plane up -d    # the plane stack (add other services/profiles as needed)
scripts/plane-dev-bootstrap.sh          # wait, founder + token, workspace, demo-project archive; prints next steps
```

> **Remote host?** Set **`PLANE_PUBLIC_URL`** (e.g. `http://<server-ip>:8091`) on *both* commands above. It feeds Plane's `WEB_URL`/CORS — where redirects and shared links come from — and the bootstrap writes it into `.env.plane` as `${PLANE_URL}`, which is how the company config references the instance. Without it, Plane redirects browsers to `localhost`.

> **Don't add `--wait`.** `plane-migrator` is a one-shot job (`restart: on-failure` — it retries a *failed* migration but never restarts after a clean exit) that exits after migrating, which `--wait` treats as a failure. The bootstrap polls `GET /api/instances/` itself — that route only answers once the api serves, and the api entrypoint's `wait_for_migrations` gates serving on the migrator having finished, so a 200 means migrations are done.

The bootstrap is idempotent (every step re-runnable) and does, in order: **(1)** poll the API healthy; **(2)** ensure the **founder** — instance-admin user `founder@nimbus.local` / `crewlet-dev-password` (override via `PLANE_FOUNDER_EMAIL` / `PLANE_FOUNDER_PASSWORD`) plus a personal API token, via one idempotent django-shell pass in the api container, then write `PLANE_FOUNDER_USER_ID` **and `PLANE_URL`** into `.env.plane` (the shipped example references exactly those vars); **(3)** ensure the **`nimbus` workspace** through the public API (#9400 — the founder token becomes a workspace-owner credential, which is precisely what [provisioning](#operator-credential) needs); **(4)** **archive the demo project** Plane's async seed task creates in every fresh workspace — it is named after the workspace itself with only the founder as a member, so left alone it is a decoy agents wander into and 403 against; **(5–6)** with `COMPANY=<config>` set, run [`crewlet plane provision … -create-projects -public-url …`](#provisioning--crewlet-plane-provision) and then [`crewlet plane import <config> examples/`](#publishing-docs--skills--crewlet-plane-import) (one walk publishes the Tool Skills *and* the Nimbus knowledge docs); **(7)** print the next steps. Steps 2–3 each end with a `manage.py clear_cache` — Plane caches the `/api/instances/` payload for 2 hours and the shell/public-API writes don't invalidate it, so without the flush the web UI would keep dropping you into a stale "not set up" flow.

`examples/nimbus.company.yaml` is the Nimbus example org on Plane. Every site that names the instance (`integrations.plane.url`, the `plane` MCP server's `PLANE_BASE_URL`, `skill_variables.plane_base_url`) is the **`${PLANE_URL}`** reference — the bootstrap writes the var into `.env.plane`, so the shipped file works against any instance with no copy or sed.

### Walkthrough (Nimbus against local Plane)

1. **Bring up the stack** (first boot pulls the six fork images and runs migrations — give it a few minutes):
   ```bash
   docker compose --profile plane up -d
   ```
2. **Bootstrap + provision + import in one shot:**
   ```bash
   COMPANY=examples/nimbus.company.yaml scripts/plane-dev-bootstrap.sh
   ```
   (Or run `scripts/plane-dev-bootstrap.sh` without `COMPANY` — it then only creates the founder + workspace and prints the `crewlet plane provision` / `crewlet plane import` commands to run yourself.) The founder token is the operator credential; `-create-projects` creates `LEAD` / `ENG` / `PROD` / `TS`; the `-public-url` targets the engine's **embedded API** on **port 80** (`api.port: 80` in the bundled Tier A file), so the hook lands at `http://host.docker.internal:80/webhooks/plane`; Plane's server-generated webhook secret is captured into `${PLANE_WEBHOOK_SECRET}` in `.env.plane`.
3. **Run the engine** with the minted tokens sourced. The bundled `examples/nimbus.config.yaml` is the matching Tier A file — its `api.port: 80` starts the **embedded API inside the engine process**, which receives the Plane webhooks and serves the dashboard, so this single command is the whole stack. Binding port 80 as a non-root user needs privileged-port access on Linux — `sudo sysctl net.ipv4.ip_unprivileged_port_start=80` (persist in `/etc/sysctl.d/`) or `CAP_NET_BIND_SERVICE`. (Do *not* also start a second, ingress-only node here — the two processes would fight over the port; splitting ingress off is for [fleets](../guides/fleet.md) only):
   ```bash
   source .env.plane
   crewlet run -config examples/nimbus.config.yaml -company examples/nimbus.company.yaml
   ```
4. **Drive the loop in the UI** (`http://localhost:8091`, log in as the founder — email+password auth is on by default): create a work item in a routed project (say `ENG`) and leave it **unassigned** → the webhook wakes the unit's **lead**, which reads it via its own `plane` MCP tools and assigns it → the assignment webhook wakes the **assignee** → the agent works, comments, and @-mentions the founder ([#9403](https://github.com/makeplane/plane/pull/9403) — you get a real Plane notification) → you @-mention the agent back (F11) and the mention webhook wakes it → edit a skill page in the `TS` project and the change propagates into the live skill registry within ~60 s (F9, the content-persist debounce) → comments and field changes keep every **subscriber** informed ([#9397](https://github.com/makeplane/plane/pull/9397)).

The provision report ends with the workspace **member table** — copy any other human's UUID into their seat's `contact.plane_user_id` (the founder's is already in `.env.plane` as `${PLANE_FOUNDER_USER_ID}`). If deliveries fail 5× while the engine is down, Plane [auto-disables the webhook](#webhooks) — re-run `crewlet plane provision` to repair it.

> **Sandbox code-authoring is the one part that won't work against a laptop.** Plane is not a code host, so nothing here changes the sandbox story — but the same caveat as [local GitLab](gitlab.md#local-testing) applies to a role listing `plane` in `role.sandbox.mcp.servers`: a cloud E2B sandbox cannot reach your machine's `localhost:8091`, so in-sandbox Plane reads need a *reachable* instance (a self-hosted E2B domain on the same network, a tunnel, or a hosted fork deployment). Provisioning, webhooks, identity resolution, knowledge search, skill sync, and all engine-side MCP tool use work fine against local compose.
