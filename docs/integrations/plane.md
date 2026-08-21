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
| [#9469](https://github.com/makeplane/plane/pull/9469) (F8) | Public workspace **page search** (`GET /workspaces/{slug}/pages/search/`) honoring page access + project membership, with a match snippet per row | the query-time `PlaneSearcher` behind the Plan-phase `## Relevant knowledge` block |
| F8.1 | **Tokenised** page search: the query is whitespace-split and matched AND-across-tokens (each token case-insensitively against page name and stripped body text), with the snippet anchored on the first matching token. The fork caps a query at **16 distinct (case-folded) tokens — above the cap it returns 400, it never truncates**; the searcher pre-trims to the cap so an over-long query still searches on its leading tokens. Without F8.1, F8 matches the whole query string as one literal substring, and the multi-keyword queries the aux LLM generates would match nothing | same — and because AND is a strict conjunction, the searcher **relaxes on zero hits**: full aux-LLM query first, then its 4-token and 2-token leading prefixes (≤3 requests), first non-empty result set wins |
| [#9398](https://github.com/makeplane/plane/pull/9398) | Webhook **CRUD** on the public token API — server-generated `secret_key` returned exactly once at creation, SSRF-guarded URLs, entity toggles | [`crewlet plane provision --webhook-url`](#provisioning--crewlet-plane-provision) self-registers (and repairs) the engine's workspace webhook |
| [#9399](https://github.com/makeplane/plane/pull/9399) + F10 | API-provisionable **service accounts**: `is_bot` user + workspace membership + first API token in one admin-scoped `POST`, token redacted from activity logs. F10 adds the lifecycle a reconcile needs: caller-chosen username/display name, token list/mint/rotate/revoke, and the account DELETE cascade | the provisioner's identity keystone — one service account per agent seat; `--rotate` / `--decommission-removed` are F10-gated |

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
    provisioning:                # consumed ONLY by `crewlet plane provision`
      role: member               #   — the engine never reads this block
      roles: { tech-lead: admin }
      username_prefix: ""
      projects: [LEAD, ENG, PROD, TS]
      token_expiry_days: 364
```

- **`url` and `workspace` are required** when enabled. Every resource path the integration touches is workspace-scoped (only `GET /api/v1/users/me/` isn't); the URL also builds shareable work-item/page links in notification metadata.
- **`webhook_secret` is required** when enabled — it is the `X-Plane-Signature` HMAC-SHA256 key, the only verification mode Plane offers. Plane **generates the secret at webhook creation** and shows it once. Keep the field **exactly one whole-value `${VAR}` reference** (a literal can never match a hook the CLI creates, and an embedded `"wh-${VAR}"` or multi-reference `"${A}${B}"` form resolves to a concatenation that never matches either — the provisioner aborts on all three): [`crewlet plane provision --webhook-url …`](#provisioning--crewlet-plane-provision) captures the generated secret into that var for you, or capture it by hand (see [Webhooks](#webhooks)).
- **`token` (optional, but effectively required)** — a read credential for a workspace member with access to the routed projects (the [provisioner](#provisioning--crewlet-plane-provision) mints a dedicated `crewlet-engine` service account for it). It enables **subscriber fan-out** (comments and field changes reach everyone subscribed to the work item, via fork PR #9397) and the project **UUID → identifier** resolution behind lead-fallback routing and `ENG-42` display keys — and the knowledge half leans on it too: the [tool-skill sync worker](#the-tool-skills-project) and [skill promotion](#skill-promotion-on-plane) run entirely on the engine client (no token ⇒ no sync, no promotion), and the [searcher](#query-time-knowledge-search) uses it as the fallback for roles without their own `PLANE_API_KEY`. Without it, thread activity degrades to payload assignees, and the project map can only learn from `project` webhook payloads — after an engine restart that cache is empty, so lead-fallback routing and `ENG-42` keys silently stay empty until the next `project` event. The engine warns `plane_engine_token_missing` at boot and at transport start when the token is unset. This mirrors `integrations.gitlab.token` (participants lookups) and `integrations.jira`'s admin token (watcher lookups).
- **`provisioning:`** is CLI-only input, ignored by the engine — it is read by [`crewlet plane provision`](#provisioning--crewlet-plane-provision). `role` is the default workspace role for agent service accounts (`admin` | `member` | `guest` — Plane's only roles; validated up front, so a GitLab copy-paste like `developer` aborts with the valid list instead of 400ing every seat), `roles` holds per-handle overrides, `username_prefix` prefixes every managed username (and safety-scopes decommission), `projects` lists the project identifiers every seat joins, and `token_expiry_days` is the standing token-lifetime policy (default 364; must be `>= 0`; `0` = the token never expires — Plane semantics for an omitted expiry, not GitLab's "instance default applies"; negative values are rejected by both the config model and the CLI flag, since they would silently mean never-expires too).

> **Unset `${VAR}`s fail the whole revision.** The engine re-validates the config *after* `${VAR}` resolution, so an unset `${PLANE_WEBHOOK_SECRET}` on the engine host makes `plane.webhook_secret is required when plane is enabled` fire while the revision is being applied — the entire revision fails and rolls back. Same behaviour as GitLab; export the vars before applying.

### Exclusivity rules

Validated on every config parse:

- `integrations.confluence` + an **enabled** `integrations.plane` → rejected (single-homed knowledge backend; also rules out Confluence *notifications* running alongside Plane). A `plane` block with `enabled: false` alongside Confluence is allowed (inert).
- `knowledge.confluence_spaces` alongside an enabled Plane → rejected (scope list for the disabled backend); `knowledge.plane_projects` alongside Confluence → rejected; `knowledge.plane_projects` without an enabled Plane → rejected.
- **Jira + Plane may coexist** — the exclusivity is Confluence-shaped, not tracker-shaped.

### Knowledge scope

```yaml
knowledge:
  plane_projects: []        # project identifiers; empty = unscoped (Plane ACLs bound the search)
```

`knowledge.plane_projects` is the `confluence_spaces` analog, materialised onto `Organization.plane_projects` and consumed by the [query-time `PlaneSearcher`](#query-time-knowledge-search). As with `confluence_spaces`, it is org-wide read scope, role- and unit-independent, and a unit's `integrations.plane.project` identity does **not** feed it. Set it only to *narrow* reads to a curated floor; leave it empty to let Plane's own membership/access rules do the scoping (empty + a per-agent token ⇒ unscoped search; empty + no per-agent token ⇒ no search).

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

The paved road is [`crewlet plane provision --webhook-url https://engine.example.com/webhooks/plane`](#provisioning--crewlet-plane-provision): it registers the one workspace webhook (#9398), sets the entity toggles, captures Plane's server-generated secret into the `${VAR}` behind `webhook_secret`, and re-enables an auto-disabled hook on every run. To create the hook by hand instead — in the fork UI (workspace settings → webhooks) or via the public webhook API:

1. Point the webhook at the engine: `https://engine.example.com/webhooks/plane`.
2. Enable the **project**, **issue**, and **issue-comment** entity toggles (plus **page** on the fork — #9401/F13). Leave **cycle** and **module** off — the router drops them anyway, and they are inbox noise. Intake events are delivered to every active workspace webhook unconditionally (CE has no toggle for them).
3. Plane **generates the secret** at creation and shows it once — capture it into the `${PLANE_WEBHOOK_SECRET}` var that `integrations.plane.webhook_secret` references.

> **Auto-disable.** Plane retries a failed delivery 5× with exponential backoff, then **auto-disables** the webhook (`is_active=False`). A [provisioning re-run](#provisioning--crewlet-plane-provision) repairs it (`is_active=True` is re-asserted on every update); by hand, flip it back on in the workspace settings. The engine's unconfigured-drop returns 200 precisely so an engine that is up but not yet configured never poisons the retry counter.

### Verification

The `X-Plane-Signature` header carries the HMAC-SHA256 **hexdigest of the raw body** keyed with `webhook_secret` (Plane CE's only scheme), compared constant-time. Invalid or missing signature → **401**; no secret configured → **503** with `Retry-After` (the request is fine; what is missing is on this side, so the delivery is held for retry rather than discarded as a 4xx would tell the sender to do); malformed JSON → **400**; engine unconfigured → **200** `{"status": "dropped"}` — verified *first*, so forgeries never earn a 200.

CE payloads carry no stable delivery id (`X-Plane-Delivery` is per-attempt), so the transport deduplicates on the event coordinates *plus* the activity discriminator (Plane fires one webhook per changed field with an identical `data` snapshot — a bulk edit is N deliveries differing only in `activity`), with a 5-minute TTL covering queue redelivery and operator replay.

### Event routing

`PlaneTransport` turns one payload into a **list** of per-recipient notifications, following the GitLab two-layer model: **directed targets** come straight from the payload; **thread activity** fans out to the work item's **subscribers** (fork PR #9397, via the engine `token`), degrading to payload assignees when the client is missing, the lookup fails, or nothing is extractable. The trigger actor is excluded from every fan-out, each recipient gets one copy per event (first, highest-signal reason wins), and every copy carries `metadata.actor_account_id` so the generic self-action guard catches any fall-through.

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

Plane webhooks use `PlaneNotificationPrompt` (`src/crewlet/notifications/notification_prompts/plane.py`), which dispatches on `routed_via` — *why* the recipient was woken:

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

With Plane enabled, the engine constructs a **`PlaneSearcher`** (`crewlet.knowledge.plane_search`) — the Plane backend of the [`KnowledgeSearcher` seam](../concepts/knowledge-system.md#the-knowledgesearcher-seam) behind the Plan-phase `## Relevant knowledge` block. Once per turn, the role's auxiliary LLM turns the trigger context into a short keyword query; the searcher sends it **verbatim** to the fork's workspace page-search endpoint (F8 — patch F8.1 tokenises it server-side, AND-across-tokens) and renders the hits as title + snippet bullets. Agents follow up with their own `plane` MCP tools (page read/search) — the block prose describes the capability, never a tool name.

- **Authentication** mirrors the Confluence searcher: the search runs as the **agent's own Plane user** when the role carries `mcp_env.plane.PLANE_API_KEY` — Plane then enforces project membership and page access natively — falling back to the engine's `integrations.plane.token` read client. A credential-less role on a token-less engine searches nothing.
- **Scope** is the org-wide [`knowledge.plane_projects`](#knowledge-scope) list, resolved from identifiers to project UUIDs through the transport's shared cache (case-insensitive). A non-empty scope that resolves to **zero** projects searches nothing, with a warning — it never silently widens to everything the credential can see. Empty scope: unscoped for self-authenticating roles, nothing for credential-less ones. Remember the [membership precondition](#knowledge-scope).
- **Ordering is recency, not relevance.** The fork's page search (F8) orders results by `-updated_at`, so the top hit is the most recently *edited* match rather than the closest one. The searcher preserves the server order, which means a strongly-matching but stale page can be outranked by a recently touched one.
- **Two result exclusions**: pages in the [Tool Skills project](#the-tool-skills-project) are dropped wholesale (skill content reaches agents through the skill catalogue / `load_tool_skill`, and a skill page's leading YAML block would dominate any snippet), and unreviewed [auto-drafts](#skill-promotion-on-plane) are hidden by parent — rows whose parent is the project's `Auto-Drafted Skills` page. When that parent lookup fails, a fail-closed backstop hides rows whose title starts with `[Auto-draft] ` instead, so an outage hides drafts rather than leaking them. The draft filter is depth-1 (direct children) rather than Confluence's full ancestor chain — drafts are flat leaf pages by construction.
- **Both exclusions fail closed**, like the transport's page routing: a hit whose `project_id` is absent is dropped, and when the configured skills project can't be resolved to a UUID, only hits positively attributable to a *different* project survive (with a `plane_search_skills_project_unresolved` warning) — an outage hides content rather than leaking skill YAML or drafts into prompts. A failed project-resolution *request* fails the whole search closed to no hits. The scope and the skills project resolve in **one** `resolve_project_ids` call per search, so a token-less engine's per-agent fallback pays a single `list_projects` walk per call.
- **Best-effort**: any failure returns no hits and the block renders empty; archived pages are excluded server-side, and a private page is visible only to its owner.

Hit URLs use the shareable shape `{url}/{workspace}/projects/{project_uuid}/pages/{page_id}` — the same one notification metadata carries (and the one to mirror via the [`plane_base_url` + `plane_workspace_slug` skill variables](#per-role-wiring)).

---

## Publishing docs + skills — `crewlet plane import`

`crewlet plane import <company.yaml> [PATH]` is the **unified publisher** — the [`crewlet confluence import`](../reference/cli.md#crewlet-confluence-import) analog. The positional config is the Tier B company YAML (credentials from its `integrations.plane` block; a `.env` next to it is loaded like `crewlet run` does). It walks every `.md` under `PATH` (default `examples/`, recursive) and routes each file by frontmatter:

- **`trigger:` present ⇒ a [Tool Skill](../concepts/tool-skills.md)** — published into the [Tool Skills project](#the-tool-skills-project) with a leading YAML code block the engine parses back out. Its directory is ignored. Target project: `--project` > `$CREWLET_TOOL_SKILLS_PROJECT` > `TS` — the same env var the engine's sync worker reads, so import and sync always agree.
- **otherwise ⇒ a knowledge doc** — published as clean prose (no YAML block) to the project whose **identifier is the file's immediate parent directory name** (`<root>/ENG/onboarding.md` → project `ENG`), titled by its first `# H1` (frontmatter `title:` overrides; a doc with no determinable title is skipped with a warning). Docs surface through the [query-time search](#query-time-knowledge-search) — they are never loaded into a registry.

**Idempotency is the fork's `external_id` contract.** Every page the importer publishes is stamped `external_source="crewlet"` with `external_id="skill:<key>"` (skills, keyed by frontmatter `key`) or `external_id="doc:<title>"` (docs, keyed by the authored H1). The fork enforces project-wide uniqueness of the pair and 409s on a duplicate — the Plane analog of the Confluence `crewlet-skill-key-*` / `crewlet-doc` marker labels. Consequences worth knowing:

- Re-running matches by external identity — one narrow page enumeration per target project, no per-file lookups — and **retitling a page in Plane's UI never orphans it** (the identity, not the title, is the match key). Existing pages are skipped unless `--update`.
- Pages that predate the marker are adopted by an **exact-title fallback**, and `--update` stamps the external identity onto them (self-healing).
- A 409 means another page — possibly an **archived** one, which the importer's enumeration doesn't see — already owns the identity; the conflict is logged with a remediation hint and the run continues.
- A frontmatter `parent:` (a page title in the same project) nests a **newly created** doc under that parent — including a parent page published **earlier in the same run** (the importer indexes every page it creates); a missing parent falls back to the project root with a hint, and an *existing* page is never re-parented — operators own page positions. A frontmatter `labels:` is ignored with a one-time log: Plane pages carry no free-form labels; the external pair **is** the marker.
- Every page is created with `access` **public**, explicitly — a private Plane page is invisible to every non-owner on both read paths.
- **Per-page failure isolation**: a non-409 write failure on one page — a **locked** page (a normal Plane UI gesture; the fork rejects writes with `400 "Page is locked"`) or a page-level 403 — logs `plane_page_write_failed` with a status-specific remediation and the run keeps publishing the remaining files, then **exits non-zero naming the failed files**. Only an invalid credential (401) or a pre-write enumeration failure aborts the run outright.

**Pre-flight**: every distinct target project (`skills project` ∪ doc parent-directory names) must already exist — matched case-insensitively against the workspace's project identifiers. Any missing project fails the run before a single page is written, naming the identifiers to create; **the importer never creates projects** — that is the provisioner's job ([`crewlet plane provision --create-projects`](#provisioning--crewlet-plane-provision)).

**Flags**: `--update` (overwrite existing pages), `--dry-run` (log what would happen, no page writes), `--project` (skills project override), and `--prune`:

- `--prune` deletes **orphaned skill pages only**, on a positive-marker predicate: `external_source == "crewlet"` **and** a `skill:` external id whose key no local file publishes. Unmarked (user-authored) pages, `doc:` pages, and knowledge docs are structurally out of reach. Deletion follows the fork's **archive-then-delete** precondition (a live page 400s on `DELETE`) with per-page failure isolation — a failure on one page logs and continues, never aborting the run — and the two steps fail independently: a failed archive leaves the page untouched (`skill_prune_failed`), while a failed **delete** (deletion is owner-or-project-admin only, so a 403 on a human-owned page is the expected case) **rolls the archive back** via unarchive, logging `plane_prune_delete_failed` with the page left visible. Left archived, the page would be hidden from users and from every later enumeration while its `external_id` — which the fork's uniqueness check matches regardless of archived state — 409s every future republish of that skill; if even the unarchive fails, the log says the page was left ARCHIVED and names the manual unarchive as the remediation. The prune candidate set is the import's own strict enumeration; if that enumeration failed, the run aborted before pruning — an incomplete listing deletes nothing.

For the combined deploy (`crewlet run --import-plane … --import-company …`) and `crewlet plane resync`, see the [CLI reference](../reference/cli.md#crewlet-plane-import).

### Onboarding convention

As on Confluence, each unit's project (plus the org root's) can host one page titled exactly **`Onboarding`** that fresh agents are nudged to read on their first turn — name the file `Onboarding.md` and the title falls out of the H1. Note that Plane does **not** enforce per-project title uniqueness (Confluence 400s on a duplicate title; Plane doesn't), so "one `Onboarding` page per project" is convention: the importer itself is idempotent by external id, but nothing stops a human from creating a second page with the same title.

---

## The Tool Skills project

`CREWLET_TOOL_SKILLS_PROJECT` (env var, default `TS`, empty string disables) names the Plane project where [Tool Skill](../concepts/tool-skills.md) pages live — the `CREWLET_TOOL_SKILLS_SPACE` analog. Don't add it to `knowledge.plane_projects`; the [searcher](#query-time-knowledge-search) drops its pages from results regardless, and the engine **excludes it from notification routing** (page edits there have no human or agent recipient by design).

The **`PlaneSkillSyncWorker`** (`crewlet.agent.skills.plane_sync`) keeps the in-memory skill registry in step with that project:

- **Boot** — ONE strict, paginated enumeration of the project carrying `description_html` (the fork's list rows serialize the body, so there is no per-page fetch fan-out). The walk is all-or-nothing: an incomplete enumeration (pagination cap, cursor stall, transport error) **never seeds the registry** — a wholesale replace from partial rows would silently delete skills. A failed walk **retries with exponential backoff** (5 attempts, from 5 s — sized for the compose boot race where Plane's API comes up seconds after the engine); the failure is logged as `tool_skill_sync_list_failed` (a *reachability* problem, retryable) as distinct from `tool_skill_project_not_found` (the identifier is genuinely absent — a configuration problem). When every attempt fails, `tool_skill_resync_exhausted` (error) says so explicitly: the registry keeps whatever it currently holds; fix the backend and re-apply the integrations config (or restart) to re-walk. The same bounded retry covers the re-seed after a live backend cut-over, so a cut-over whose first walk fails cannot silently keep serving the old backend's skills without telling the operator. Archived pages are excluded by the server default.
- **Runtime** — the transport's index callback fires for **every** `page` webhook workspace-wide; the worker scopes it via the fork's project-scoped page GET, which 404s for pages in other projects (404 ⇒ evict, a no-op when the registry never held the page). A `deleted` action evicts by page id; **any other action** — `created`, `updated` (including the 60 s-debounced content-persist flush), or an unknown future one — fetches the page and re-runs the admission predicate.
- **Admission predicate** (identical for boot and webhooks): the page lives in the skills project **and** its body decodes as a skill (a leading YAML code block with a `trigger`). A fetched page failing it is **evicted, not skipped**: an **archived** page (non-null `archived_at` — archive is the operator's "remove" gesture, since delete requires archive first), a page moved to another project, or a page edited into a non-skill must not keep serving its last-good body. Pages carrying the importer's `external_source="crewlet"` marker log their decode failures at `info` (they *should* decode); stray operator pages and the project home page skip quietly.

Editing a skill is therefore a Plane-native workflow: open the page, edit, save — the content-persist webhook lands within ~60 s and the registry updates, no restart. `crewlet plane resync` is the drift-recovery diagnostic (see the [CLI reference](../reference/cli.md#crewlet-plane-resync)). The page representation (YAML code block + prose body) is documented in [Tool Skills](../concepts/tool-skills.md#page-representation).

---

## Skill promotion on Plane

The [cross-agent skill promotion](../concepts/agent-learning.md) write path works on Plane through the `PlanePromotionWriter` (`crewlet.plane.promotion`), using the engine's read client — an engine `token` is required (without one the promotion pass soft-skips and retries next tick):

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
    --provision-token <workspace-admin API token> \
    --webhook-url https://engine.example.com/webhooks/plane \
    --create-projects
```

A one-shot, **idempotent reconcile** from company config to Plane state — the [GitLab provisioner](gitlab.md#provisioning)'s contract ported to Plane. For every **agent** seat that declares `mcp_env.plane` it ensures a service account, project memberships, and an API token minted into the seat's own `${VAR}` references; it also provisions the engine's `crewlet-engine` read account and self-registers the [workspace webhook](#webhooks). Re-runs no-op. Per-seat failures are isolated — the loop continues and the command exits 1 at the end — while operator-actionable conditions (stock CE, a non-admin token, a missing fork capability) abort up front with one clear message. Flags and defaults are in the [CLI reference](../reference/cli.md#crewlet-plane-provision).

> **First run: `webhook_secret` must be exactly one `${VAR}` reference.** Config validation requires a non-empty `webhook_secret` whenever Plane is enabled, and it runs *before* the provisioner that supplies the secret. The way out of the chicken-and-egg is `webhook_secret: "${PLANE_WEBHOOK_SECRET}"` — a reference passes load-time validation with the var still unset, Plane generates the secret server-side when the CLI creates the hook, and the CLI captures it into that var. The command prints exactly this remediation when it meets an empty value, and aborts when the value is not **exactly one whole-value reference**: a literal can never match a hook it creates, and an embedded (`"wh-${VAR}"`) or multi-reference (`"${A}${B}"`) form resolves to a concatenation that never matches the captured secret either — every delivery would fail HMAC verification until Plane auto-disables the hook.

### Operator credential

A **workspace-admin personal API token**, passed via `--provision-token` or `$PLANE_PROVISION_TOKEN` — created once in the Plane UI by the first human, never stored in company config. Every provisioning surface (service accounts, tokens, webhooks, the members list) is gated on Plane's workspace-owner permission, so there is no instance-admin mode and no `--mode` flag — Plane provisioning is workspace-scoped, full stop. Two project-level caveats:

- **`--create-projects`** makes the *operator* each created project's admin (fine), with `name = identifier` — config carries only identifiers; rename in the Plane UI at will.
- **Pre-existing projects** are visible to the operator only through membership, and project-member writes are project-admin-gated — so a declared project the operator cannot see is reported as "not found **(or not visible to the operator credential)**". Make the operator a member/admin of every pre-existing `provisioning.projects` entry, or let `--create-projects` create them fresh. When `--create-projects` then hits the invisible-but-exists conflict (the create 409s on the taken identifier — or any other 4xx), that is a **per-project note** and the project is dropped from the run's targets; the seats and the visible projects still reconcile. A membership write 403ing after an account was created is a per-seat error that **cannot lose the credential**: the create response's single-show token is recorded before memberships, the seat keeps `account=created` with the failure in its error column, and the run exits 1.

### Capability preflight and degraded modes

Nothing mutates until the CLI has probed what the instance supports. The preflight opens with two cheap read probes that pin the *cause* of a rejection — the fork's own contract tests deliberately do **not** pin whether a bad credential produces 401 or 403, so status codes alone cannot separate "bad token" from "not an admin": first `GET /users/me/` (any auth failure there = the credential itself), then the cheapest membership-scoped workspace route (`GET …/projects/` — rejects a wrong slug whatever the token's permissions are). Only then run the capability probes, which use deliberately disallowed HTTP methods against the fork's routes — 404 means the route family is absent, 405/403 proves presence — so they can never write:

| Instance | Behaviour |
|---|---|
| `GET /users/me/` rejects the token (401 **or** 403 — Plane does not pin which) | **Abort**: the operator token is invalid, expired, or revoked |
| Credential good, workspace slug unknown or invisible | **Abort** naming `integrations.plane.workspace` ("workspace not found or not visible to the operator credential") |
| No service-accounts route at all (stock Plane CE) | **Abort**: provisioning requires the crewlet/plane fork (`preview`) |
| Routes present, operator not a workspace admin (403 — credential and slug already proven good) | **Abort**: create the token from a workspace-admin account |
| #9399 present, **F10 absent** — plain run | **Degraded #9399-only mode**, with a note: accounts are created and *creation itself mints token #1*, memberships and the webhook still reconcile — but nothing can be rotated, re-minted on an existing account, or decommissioned; seats needing a re-mint report `token=blocked` (could **not** provision — distinct from `skipped` = already provisioned) with **one collective note**, never N failures, and the run still exits 0 — the degraded mode is a usable mode by design, not an error |
| #9399 present, **F10 absent** — `--rotate` / `--decommission-removed` | **Abort before any mutation** — and no simulated fallback: delete-and-recreate "rotation" would change user UUIDs and orphan HandleRegistry + attribution |
| Members rows lack `username` (**F15 absent**) | **Abort** — the reconcile keys every create-vs-exists decision and decommission targeting on usernames, so F15 is hard-required |
| Create response echoes a synthetic `svc_<uuid>` username (upstream #9399 without the fork's identity support) | **Abort**, naming the just-created orphan account — its DELETE is F10-only, so it must be removed server-side; without this check every run would silently create fresh accounts per seat, forever |
| Webhook create/update does not echo `page: true` (**F13 absent**) | Proceed with a loud note: page events will **not** be delivered — the [Tool Skills sync](#the-tool-skills-project) and page routing stay dark until F13 lands |
| No webhook API (#9398 absent) with `--webhook-url` | **Abort** (deploy the fork or drop the flag) — the webhook surface is probed only when the run touches it |

### What a run does

1. **Ensure the service account.** Look up `{username_prefix}{handle}` case-insensitively in the strict workspace-members walk; create if missing via #9399, passing the caller-chosen username, `display_name = role.name`, an **explicit** workspace role (the API default is `admin` — silently privilege-escalating for an omitting caller — so `provisioning.roles[handle]` / `provisioning.role` is always sent), and `name = crewlet-provision:<handle>`, which #9399 pins as the first token's label (Plane also stores `name` as the user's `first_name` — cosmetic; `display_name` drives the members UI). A matched members row that is **not a service account** (a human — or a non-service bot — holding the derived username) is a **seat error** raised before any membership write, naming `provisioning.username_prefix` as the remedy: a human squatter is never adopted. A seat whose `mcp_env.plane` declares **no `${VAR}` reference never creates an account** (`account=skipped`, with a note) — the create response's single-show token would have nowhere to be recorded and the credential would be orphaned; an *existing* account for such a seat still gets its membership reconcile. A username **409** means a foreign holder (adjust `username_prefix`) *or* a previously decommissioned service account — see step 6. **Drift is noted, not repaired**: a drifted display name or workspace role on an existing account becomes a report note — the fork has no service-account update endpoint and the members API is read-only, and delete + recreate is forbidden (UUID churn orphans HandleRegistry + attribution).
2. **Ensure memberships.** `provisioning.projects` identifiers resolve case-insensitively against the workspace's projects; missing ones are created only with `--create-projects` — a failed create (the invisible-but-exists identifier conflict, or any 4xx) is a **per-project note + drop**, never a whole-run abort — otherwise dropped with a note (accounts, tokens, and the projects that *do* exist still reconcile — and the created projects are what unblock [`crewlet plane import`](#publishing-docs--skills--crewlet-plane-import), whose pre-flight hard-fails on missing projects and never creates them). Each seat is added to every resolved project (existing-workspace-member user UUID + role int). A duplicate add is "exists" — but CE's *generic* duplicate 400 (`"The payload is not valid"`, the mapping for **any** IntegrityError from the member view) counts as a duplicate **only after a lite row confirms the membership exists**; otherwise the error surfaces. A confirmed membership that was **deactivated in the Plane UI** (the row is kept with `is_active=False`, so the seat has *no* project access while every re-add reads as a duplicate) reports `member=inactive` with a loud note — the public API cannot reactivate a `ProjectMember` row (`is_active` is not a writable field on the pinned serializer), so re-activating in Plane's project settings is the remediation. **Project-role drift is likewise a note**: the pinned membership PATCH does write `role`, but it is keyed by the `ProjectMember` row pk, which no public read exposes for a pre-existing row (the lite rows serve the *user* UUID; the pk appears only in the create echo).
3. **Ensure a token.** Scan the seat's `mcp_env.plane` values for `${VAR}` references — the config's own references ARE the contract; the provisioner never invents names, and a var that already has a recorded value is skipped (Plane returns every credential exactly once, which is what makes re-runs no-op). One `${VAR}` referenced by **two seats** is a seat error on the later one, naming both — one variable cannot record two identities. On a **freshly created** account, the create response's single-show token is recorded into **all** referencing vars *the moment the create returns, before memberships* — a later per-seat failure can never discard the account's only credential. On an existing account with an unrecorded var (lost env file, second machine) whose account still holds an **active managed token** (label `crewlet-provision:<handle>`), a plain run reports `token=needs_rotate` with a loud note: the only recovery is rotating that live token, which **invalidates the value the running engine holds**, so — exactly like the webhook recreate — it happens only under the explicit flag: `--rotate` rotates it into every referencing var and revokes stale parallel managed-label tokens. A **genuine mint** — no active managed token exists, so nothing live can be invalidated — stays automatic. Expiry: `provisioning.token_expiry_days` (default 364; `>= 0`) or the `--token-expiry-days` one-off override; `0` omits `expired_at`, which in Plane means the token **never expires**. Active managed tokens already expired or expiring within 30 days get a "re-run with `--rotate`" note.
4. **Ensure the engine account.** Iff `integrations.plane.token` is a `${VAR}` reference: service account `{username_prefix}crewlet-engine`, display name "Crewlet Engine (routing)", workspace role **`member`** — never `guest`, and never the config default: Plane has no Reporter-style read-only role, and guest visibility is restricted to own/assigned items under common project settings, which would break subscriber fan-out, member/project resolution, and page fetch; `member` is the least role that guarantees the engine's read paths — **and a project member of every configured project** (the engine's page reads and search fallback are membership-scoped like any seat's). It survives decommission and rotates with `--rotate`; a literal (non-`${VAR}`) token leaves the engine account untouched.
5. **Ensure the workspace webhook** (with `--webhook-url` only). One hook, matched against the existing hooks by **normalized** URL (trailing slash stripped, scheme + host lowercased — userinfo case preserved, it is case-sensitive — explicit default ports dropped (`:443`/`:80`), duplicate path slashes collapsed): Plane's own duplicate-URL constraint is byte-exact, so `…/plane` and `…/plane/` are two hooks that would **both fire**, double-delivering every event — near-duplicates matching the engine path are reported loudly and **never auto-deleted** (foreign hooks are not the provisioner's to remove). Entity toggles: `project` / `issue` / `issue_comment` / `page` on, `cycle` / `module` off. The secret is **generated by Plane and returned exactly once** — the inversion of GitLab, where crewlet supplies the secret and can re-stamp it — so: no hook ⇒ create + capture the secret into the `webhook_secret` `${VAR}`; hook exists + secret recorded ⇒ PATCH toggles + `is_active=True` (re-enabling an [auto-disabled](#webhooks) hook); hook exists + secret **unrecorded** ⇒ a dead end (the update path can never re-emit the secret), and the destructive recovery — delete + recreate for a fresh secret — is gated behind **`--recreate-webhook`**, because it invalidates the secret every *other* deployment holds; without the flag the run emits a loud note instead. The destructive paths (`--rotate`, `--recreate-webhook`) delete **only an exact-URL match** and log the deletion before issuing it; when the only match is a near-duplicate, nothing is deleted *and* nothing is created (a byte-exact sibling would double-deliver) — a note names the spelling difference. If the replacement create **fails after a successful delete**, the run aborts stating exactly that: the hook was deleted, Plane deliveries are **down**, and a re-run restores it. A create 409 (the URL raced into existence) is adopted, with a note that its secret is not recorded here. The **env file — not the shell — is the source of truth** for "recorded".
6. **Decommission** (`--decommission-removed`, explicit, never default). Refuses without `provisioning.username_prefix` (managed accounts must be identifiable — un-prefixed service accounts are never deleted). Prefixed accounts whose seats left the config get the F10 DELETE cascade: every token deactivated, project + workspace memberships removed, user deactivated — with the row **kept** for attribution. Plane's own `NOT_A_SERVICE_ACCOUNT` guard makes a prefixed *human* structurally undeletable (recorded as a note, never an error); a 404 is an already-decommissioned account (idempotent re-run). **Irreversible for the username on a build without F14** (create-reactivates — see [the fork](#the-fork)): the kept row owns it terminally, so re-adding the seat later 409s at create — pick a new handle or `username_prefix`, reactivate the account server-side, or deploy a fork build carrying F14.

`--rotate` covers **all three credential classes**: each seat's managed token (rotated with a fresh **explicit** expiry window — never the inherit form, whose expiry would silently shrink to the source token's; a concurrent-rotation 409 falls through to minting), the engine token (same path), and — only together with `--webhook-url` — the webhook secret (delete + recreate, since the secret is immutable and single-show). `--rotate` *without* `--webhook-url` leaves the webhook secret un-rotated and says so in a note.

Human seats are **validated, never created**: a `contact.plane_user_id` that is not a workspace member (or references an unset `${VAR}`) becomes a note naming the seat; humans without the field stay silent — the member table below is the fill-in aid.

### The report

Per-seat lines (`username  account=created|exists|skipped|error  member=added|exists|inactive|skipped  token=minted|rotated|skipped|needs_rotate|blocked|none`), then hooks / decommissioned accounts / notes (drift, expiring tokens, degraded-mode collectives, webhook warnings), then — when tokens were written — the reminder to **source the env file and restart the engine** (a running engine never re-resolves the config block, so new values only apply on a fresh start). The report **ends with the workspace member table** (display name, username, user UUID, role; provisioner-managed rows marked `[agent]`) so founders can copy the human UUIDs straight into [`contact.plane_user_id`](#identity-registration). Exit code: 1 when any seat carries an error — the account column stays truthful (a created-then-failed seat still reads `account=created`), so the error column, not the account state, drives the exit code. `member=inactive` = a deactivated membership needing Plane-UI reactivation; `token=needs_rotate` = an unrecorded var recoverable only by `--rotate`; `token=blocked` = the degraded-mode could-not-mint (exit stays 0); `account=skipped` = a seat with no `${VAR}` to record a creation token into.

Minted values leave the process through one of three sinks: the encrypted [secret store](../concepts/secret-store.md) (`--secret-store` — the engine reads it back directly, so nothing has to be sourced or restarted into place), the env file (`--env-file`, default `.env.plane`, written atomically and left `0600` even when you created it yourself), or `--print` (`export VAR=token` lines to stdout). Every credential Plane returns is unretrievable after the response that carried it, so each sink persists write-through as it mints rather than buffering to the end of the run.

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

The bootstrap is idempotent (every step re-runnable) and does, in order: **(1)** poll the API healthy; **(2)** ensure the **founder** — instance-admin user `founder@nimbus.local` / `crewlet-dev-password` (override via `PLANE_FOUNDER_EMAIL` / `PLANE_FOUNDER_PASSWORD`) plus a personal API token, via one idempotent django-shell pass in the api container, then write `PLANE_FOUNDER_USER_ID` **and `PLANE_URL`** into `.env.plane` (the shipped example references exactly those vars); **(3)** ensure the **`nimbus` workspace** through the public API (#9400 — the founder token becomes a workspace-owner credential, which is precisely what [provisioning](#operator-credential) needs); **(4)** **archive the demo project** Plane's async seed task creates in every fresh workspace — it is named after the workspace itself with only the founder as a member, so left alone it is a decoy agents wander into and 403 against; **(5–6)** with `COMPANY=<config>` set, run [`crewlet plane provision … --create-projects --webhook-url …`](#provisioning--crewlet-plane-provision) and then [`crewlet plane import <config> examples/`](#publishing-docs--skills--crewlet-plane-import) (one walk publishes the Tool Skills *and* the Nimbus knowledge docs); **(7)** print the next steps. Steps 2–3 each end with a `manage.py clear_cache` — Plane caches the `/api/instances/` payload for 2 hours and the shell/public-API writes don't invalidate it, so without the flush the web UI would keep dropping you into a stale "not set up" flow.

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
   (Or run `scripts/plane-dev-bootstrap.sh` without `COMPANY` — it then only creates the founder + workspace and prints the `crewlet plane provision` / `crewlet plane import` commands to run yourself.) The founder token is the operator credential; `--create-projects` creates `LEAD` / `ENG` / `PROD` / `TS`; the `--webhook-url` targets the engine's **embedded API** on **port 80** (`api.port: 80` in the bundled Tier A file) at `http://host.docker.internal:80/webhooks/plane`; Plane's server-generated webhook secret is captured into `${PLANE_WEBHOOK_SECRET}` in `.env.plane`.
3. **Run the engine** with the minted tokens sourced. The bundled `examples/nimbus.config.yaml` is the matching Tier A file — its `api.port: 80` starts the **embedded API inside the engine process**, which receives the Plane webhooks and serves the dashboard, so this single command is the whole stack. Binding port 80 as a non-root user needs privileged-port access on Linux — `sudo sysctl net.ipv4.ip_unprivileged_port_start=80` (persist in `/etc/sysctl.d/`) or `CAP_NET_BIND_SERVICE`. (Do *not* also start a second, ingress-only node here — the two processes would fight over the port; splitting ingress off is for [fleets](../guides/fleet.md) only):
   ```bash
   source .env.plane
   crewlet run examples/nimbus.config.yaml --import-company examples/nimbus.company.yaml
   ```
4. **Drive the loop in the UI** (`http://localhost:8091`, log in as the founder — email+password auth is on by default): create a work item in a routed project (say `ENG`) and leave it **unassigned** → the webhook wakes the unit's **lead**, which reads it via its own `plane` MCP tools and assigns it → the assignment webhook wakes the **assignee** → the agent works, comments, and @-mentions the founder ([#9403](https://github.com/makeplane/plane/pull/9403) — you get a real Plane notification) → you @-mention the agent back (F11) and the mention webhook wakes it → edit a skill page in the `TS` project and the change propagates into the live skill registry within ~60 s (F9, the content-persist debounce) → comments and field changes keep every **subscriber** informed ([#9397](https://github.com/makeplane/plane/pull/9397)).

The provision report ends with the workspace **member table** — copy any other human's UUID into their seat's `contact.plane_user_id` (the founder's is already in `.env.plane` as `${PLANE_FOUNDER_USER_ID}`). If deliveries fail 5× while the engine is down, Plane [auto-disables the webhook](#webhooks) — re-run `crewlet plane provision` to repair it.

> **Sandbox code-authoring is the one part that won't work against a laptop.** Plane is not a code host, so nothing here changes the sandbox story — but the same caveat as [local GitLab](gitlab.md#local-testing) applies to a role listing `plane` in `role.sandbox.mcp.servers`: a cloud E2B sandbox cannot reach your machine's `localhost:8091`, so in-sandbox Plane reads need a *reachable* instance (a self-hosted E2B domain on the same network, a tunnel, or a hosted fork deployment). Provisioning, webhooks, identity resolution, knowledge search, skill sync, and all engine-side MCP tool use work fine against local compose.
