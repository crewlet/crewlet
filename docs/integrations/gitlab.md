# GitLab Integration

Crewlet integrates with GitLab as a first-class code host, alongside — and independent of — the [GitHub integration](github.md). The two coexist; an org can enable either or both. The split is the same one GitHub uses: GitLab tools are for **reading, reviewing, and tracking** code (diffs, comments, reviews, approvals, MR/issue state, pipelines); **authoring** code changes goes through the [code sandbox](../concepts/code-sandbox.md).

What GitLab adds over GitHub is **automated, per-agent identity provisioning**. Each agent seat gets its own GitLab **service account** — a first-class user that can be assigned issues and merge requests, @-mentioned, and requested as a reviewer, but which costs no billable seat and cannot sign in through the UI. Because GitLab exposes an API to create these accounts and mint their tokens, `crewlet gitlab provision` reconciles the whole company config into GitLab in one command — something GitHub's API cannot do (see [GitHub vs GitLab identity](#github-vs-gitlab-identity)). The top-level `integrations.gitlab` block carries the inbound-webhook and identity-resolution config.

> **Prerequisites.** The operator creates the **top-level GitLab group** by hand and mints the **operator credential** — a group-Owner PAT with the `api` scope on GitLab.com, or an instance-admin PAT on self-managed (see the [permission matrix](#permission-matrix--the-operator-credential)); provisioning automates everything below that. The same configuration covers **gitlab.com and self-hosted instances alike** — point `integrations.gitlab.url` at your instance.

---

## Configuration

The top-level `integrations.gitlab` block is **non-tool config** — it enables inbound webhook handling and boot-time identity registration:

```yaml
integrations:
  gitlab:
    enabled: true
    url: "https://gitlab.com"                   # instance base URL — REQUIRED
    signing_secret: "${GITLAB_SIGNING_SECRET}"  # whsec_… — 19.1+ Standard-Webhooks HMAC — REQUIRED
    token: "${GITLAB_ENGINE_TOKEN}"             # optional read credential → participants-based routing
    provisioning:                # consumed ONLY by `crewlet gitlab provision`, ignored by the engine
      group: nimbus-hq           # top-level group the agent service accounts join
      access_level: developer    # default group membership (developer | maintainer)
      access_levels:             # per-handle overrides
        tech-lead: maintainer
      username_prefix: ""        # e.g. "agent-" when the group namespace is shared with humans
      projects: []               # extra projects to add each account to (+ hooks only when group_webhook: false / falls back)
      group_webhook: auto        # auto (group hook, else per-project) | true (group only) | false (per-project only)
      token_scopes: [api]        # scopes minted on each service-account PAT
```

Unlike `integrations.github`, three fields differ:

- **`url` is required** when GitLab is enabled — the instance address is needed for webhook links, boot-time identity resolution (`GET {url}/api/v4/user`), and provisioning. GitHub's is implied.
- **`signing_secret` is required** when enabled — inbound webhooks are verified by the GitLab 19.1+ Standard-Webhooks HMAC signature (`webhook-signature` header) and nothing else. The weaker plain `X-Gitlab-Token` scheme is intentionally unsupported; gitlab.com always runs ≥ 19.1 and the docker-compose test instance runs `gitlab-ee:latest`, so the signing token is always available. **Self-managed GitLab older than 19.1 is not supported.** Point it at a `${VAR}` and you don't even have to invent a value: when `crewlet gitlab provision -public-url …` runs and that var is unset, the provisioner **generates a `whsec_…` secret**, stamps it on the hook, and writes it back to the token sink — see [Provisioning](#what-a-run-does). See [Webhooks](#webhooks).
- **`token` (optional)** enables **participants-based routing**: comments and state changes fan out to everyone participating in the issue/MR — GitLab's own notification reach — instead of only assignees and mentioned users. Webhook payloads don't carry the participants list, so this costs one `GET …/participants` REST call per comment/state-change event, made with this credential (any group member's PAT with `read_api`; the provisioner mints a dedicated read-only `crewlet-engine` account for the referenced `${VAR}` automatically). Without it, routing degrades to payload-derived targets — directed events are unaffected. This mirrors `integrations.jira`'s admin token, which exists for the same reason (watcher lookups). See [Event routing](#event-routing).

The `provisioning:` sub-block is read **only by the provisioning CLI** — the engine never looks at it. Its fields drive the reconcile described under [Provisioning](#provisioning).

---

## Per-role wiring

Declare the GitLab **MCP tool server** once in `mcp_servers` as a `shared: false` server. Each agent supplies its own service-account PAT in `role.mcp_env.gitlab`; a sandbox-enabled role also declares the same PAT in `role.sandbox.env` for the git-auth recipe:

```yaml
mcp_servers:
  - name: gitlab                             # official glab CLI MCP server (stdio)
    shared: false
    command: glab
    args: ["mcp", "serve"]

roles:
  - name: Agent SWE
    mcp_env:
      gitlab:
        GITLAB_TOKEN: "${GITLAB_TOKEN_SWE}"    # per-agent service-account PAT (glab reads it)
        GITLAB_HOST: gitlab.com
    sandbox:
      enabled: true
      env:
        GITLAB_TOKEN: "${GITLAB_TOKEN_SWE}"     # same PAT for the git-auth recipe
```

Provisioning mints into the `${VAR}` referenced by the block's **credential** key — `GITLAB_TOKEN`, `GITLAB_PERSONAL_ACCESS_TOKEN`, `Private-Token`, or `Authorization: Bearer …`, the same keys boot-time identity resolution reads. Everything else in the block (`GITLAB_HOST`, an API url) is config and is left alone, so two seats may point at one shared `${GITLAB_HOST}` without it being mistaken for a credential they both claim.

The default is the official **`glab` CLI stdio MCP server** (`glab mcp serve`): the engine's MCP bridge spawns one `glab` process per role with that role's `mcp_env.gitlab` env, so the per-agent PAT (`GITLAB_TOKEN`) IS the per-agent identity — no separate server to run. Boot-time identity resolution reads the token from whichever key is present (`GITLAB_TOKEN`, `GITLAB_PERSONAL_ACCESS_TOKEN`, a `Private-Token` header, or `Authorization: Bearer <pat>`), so the http alternative below works too. The engine names **no tool-specific variable** — the whole `mcp_env.gitlab` block is forwarded verbatim to the role's MCP instance — and `GITLAB_TOKEN` is declared in `role.sandbox.env` by the founder exactly as `GITHUB_TOKEN` is (see [Code Sandbox](../concepts/code-sandbox.md)). A role with no `mcp_env.gitlab` gets no GitLab tools.

---

## MCP tool server

The default tool server is the **official [`glab` CLI](https://gitlab.com/gitlab-org/cli)** running its built-in MCP server, `glab mcp serve` (stdio). It's declared like any stdio server and the engine's MCP bridge spawns **one `glab` process per role** with that role's `mcp_env.gitlab` env — the same shape as the `atlassian` (`uvx mcp-atlassian`) server — so **there is no separate MCP server to run or host**. Each process authenticates as its own service account via `GITLAB_TOKEN` (and `GITLAB_HOST` for self-managed). The served surface is the full `glab` command tree exposed as tools — MR approve (`glab_mr_approve`), diff (`glab_mr_diff`), notes and diff discussions (`glab_mr_note`, `glab_mr_note_create`), full issue CRUD, CI/pipelines, and `glab_api` for any raw authenticated call (including `glab_api /user` for identity) — and interactive commands are excluded, with `--output json` added automatically.

- **Requirement:** the `glab` binary must be on the **engine host** (it's spawned there). Install per [GitLab's CLI docs](https://gitlab.com/gitlab-org/cli); Homebrew (`brew install glab`) is the officially supported cross-platform method.
- **Status:** `glab mcp serve` is flagged **experimental** by GitLab ("may be unstable or removed"). It's official and actively developed; pin a known-good `glab` version if that risk matters for your deployment.

**Alternative — one shared server:** [`@zereight/mcp-gitlab`](https://github.com/zereight/gitlab-mcp) (community, MIT; docker image `zereight050/gitlab-mcp`) in **streamable-HTTP + remote-authorization** mode (`STREAMABLE_HTTP=true`, `REMOTE_AUTHORIZATION=true`) is a single process for the whole fleet, each request authenticated by its own `Private-Token` header. Declare it as a `shared: false` `http` server (`url: http://…/mcp`) and put the PAT in `mcp_env.gitlab` as `Private-Token`. Choose this if you'd rather run one hosted server than a `glab` binary on the engine host; guardrails (`GITLAB_PERMISSION_MODE`, `GITLAB_TOOLSETS`, `GITLAB_DENIED_TOOLS_REGEX`) trim the catalogue, and `GITLAB_API_URL` targets self-managed.

GitLab's **built-in server-side MCP endpoint** (`POST /api/v4/mcp`, Free tier since 19.2) is **not used**. Its authentication is OAuth-only — dynamic client registration with interactive consent per identity, unworkable for headless per-agent identities (PAT auth is an open request, [gitlab-org/gitlab#586184](https://gitlab.com/gitlab-org/gitlab/-/issues/586184)). When PAT auth lands, adopting it is a single-server swap.

As with every MCP surface, the engine hardcodes no tool names — a bundled `mcp:gitlab` [Tool Skill](../concepts/tool-skills.md) frames the tools as read/review/track with authoring pointed at the sandbox, mirroring the `mcp:github` skill.

---

## How code authoring works

Agents that the founder has gated with `role.sandbox.enabled` author code through the **code sandbox**, not through any GitLab tool. Execute has a `run_sandbox` tool: the planner lists it in `tools_needed`, a coding agent (Claude Code / OpenCode) runs inside an isolated E2B sandbox and opens a merge request **as the agent's own GitLab identity** (the PAT the role declares as `GITLAB_TOKEN` in `role.sandbox.env` — by convention the same PAT as its `mcp_env.gitlab` header). The call is detached — the Execute loop suspends and resumes with the result when the run completes, so the agent reports the MR in the same turn. The full design is in [Code Sandbox](../concepts/code-sandbox.md).

The GitLab **git-auth recipe** is example config, not engine code — the engine ships no git-auth, the same stance as GitHub. The Nimbus GitLab example (`examples/nimbus.company.yaml`) carries a scoped credential helper reading `$GITLAB_TOKEN` for the GitLab host only, `insteadOf` rewrites for SSH-style remotes, and a brief telling the coding agent to just clone. Two GitLab-specific wrinkles versus the GitHub recipe: the basic-auth **username is arbitrary** for PAT auth (GitLab ignores it — the token is the password), and the helper must match the host **including a non-standard port** when the instance runs on one (the dev compose serves `gitlab.local:8929`), so the recipe templates the host from `integrations.gitlab.url` rather than hardcoding it.

Once an MR exists, GitLab tools stay in the picture on the **read/review/track** side: agents read its diff, comment, approve, and follow the MR's webhooks to report back to the original requester. As with GitHub, capture context via `reflect_and_persist(ttl_days=30)` whenever you kick off async work (a sandbox coding job) so the original ask + repo + MR number surface in your `## Personal memory` block on the review-notification turn.

---

## Provisioning

`crewlet gitlab provision <company.yaml>` is a one-shot, **idempotent reconcile** from company config to GitLab state, runnable any number of times.

```bash
GITLAB_ADMIN_TOKEN="$GITLAB_ADMIN_TOKEN" crewlet gitlab provision company.yaml \
  -public-url https://engine.example.com \
  -env-file .env.gitlab
```

### Flags

| Flag | Description |
|------|-------------|
| `company.yaml` (positional) | Path to the Tier B company YAML |
| `-admin-token` | Operator credential (see the permission matrix below). Empty reads `$GITLAB_ADMIN_TOKEN` |
| `-public-url URL` | The engine's **public base address**, e.g. `https://engine.example.com` — *not* a webhook path. The engine owns its seven webhook routes and derives `/webhooks/gitlab` itself, so there is no path to mistype. **Omit to skip webhook registration** |
| `-secret-store` | Write minted credentials into the encrypted [`secret_values`](../concepts/secret-store.md) table instead of an env file — the engine reads them back directly, so there is nothing to source. Needs a Tier A keyring (`-config`) |
| `-env-file PATH` | Env file to append/update minted tokens into. Ignored with `-secret-store` |
| `-print` | Print `export VAR=token` lines to stdout and persist nothing |
| `-config PATH` | Tier A config naming this node's store and secret keyring (default `crewlet.yaml`). Only `-secret-store` reads it |
| `-rotate` | Mint a fresh token for **every** seat, including seats whose current one still works. Not the default, and not what a re-run does: GitLab returns a token's value once, so minting every run would revoke the credential every agent is currently authenticating with — an operator adding a tenth seat would take the other nine down. **Restart the engine after** |
| `-decommission` | Delete managed service accounts whose seats have left the config. Off by default: it is the one destructive direction, and a company mid-edit looks exactly like a company that removed a seat |
| `-token-expiry-days N` | Lifetime minted onto each token. `0` sends no expiry and lets the instance policy decide |
| `-dry-run` | Print what the run would do and touch nothing |

The CLI probes the operator credential with `GET /user` up front and fails fast with the failing endpoint and status if the token or its scopes are wrong. It then runs two **preflights** so common setup gaps surface as one clear message instead of a stack of API errors:

- **Service-accounts access.** A single list call to the service-accounts API. A `403` here — even with a valid group-Owner `api` token — is GitLab.com's identity-verification gate, not a scope problem; the run aborts with `Provisioning cannot proceed: …` pointing at [identity verification](#prerequisites-gitlabcom).
- **Declared projects exist.** Each `provisioning.projects` entry is checked with `GET /projects/:id`. A project that does **not** exist (`404`) is **dropped and named in a report note** rather than aborting the whole reconcile on the first missing one — create it (or remove it from the config) and re-run. The accounts, tokens, and any projects that do exist still reconcile.

### What a run does

For each **agent** seat that declares GitLab credentials (presence of `mcp_env.gitlab`, the same convention GitHub uses):

1. **Ensure the service account exists.** Look it up by username — `<username_prefix><handle>` — under the configured group; create it if missing. The display name is `role.name`; the email is `role.email` when set.
2. **Ensure membership.** Add the account to the configured top-level group at its access level (`access_level`, with `access_levels` per-handle overrides), plus any explicitly listed `projects`.

   > **Access level and merging.** A **Developer** can push a branch and open an MR, but GitLab's default protected branch (`main`) only permits **Maintainers** to *merge* — so for an autonomous review→merge loop (no human doing the final merge), provision the code-active seats as **`maintainer`**. The trade-off: membership here is **group-wide and uniform**, so group-Maintainer means an agent can merge *any* project in the group; scope that behaviourally with an "own your repos" policy. Hard per-repo scoping (Maintainer only on owned projects, Developer elsewhere) would need per-`(seat, project)` access levels, which the reconcile does not model today — provision the group at `developer` and add per-project `maintainer` memberships out of band if you need it. Alternatively, keep `developer` and relax each project's protected-branch *"Allowed to merge"* to include Developers (a project setting the provisioner does not manage).
3. **Ensure a token.** The provisioner derives the **env-var name from the config itself** — it scans the seat's `mcp_env.gitlab` values *and* its `sandbox.env.GITLAB_TOKEN` for unresolved `${VAR}` references (so a sandbox-authoring seat with no MCP surface still gets its token; other sandbox env keys are never scanned). For each referenced var with no recorded value, it mints a PAT (scopes from `provisioning.token_scopes`, default `[api]`; expiry from `-token-expiry-days`) named `crewlet-provision:<handle>` and writes `VAR=glpat-…` to the sink. So the config's `${GITLAB_TOKEN_SWE}` reference is the *contract* and the provisioner fills it — it never invents its own naming scheme. This is what makes minting idempotent: GitLab never returns a token value after creation, so a seat whose `${VAR}` already carries a value is **skipped**.
4. **Ensure the engine's routing account.** When `integrations.gitlab.token` references a `${VAR}`, a dedicated **`crewlet-engine`** service account (prefixed like the seats) is provisioned with **Reporter** access and a `read_api`-scoped PAT minted into that var — the read-only credential [participants-based routing](#event-routing) uses. It rotates with `-rotate` and is never decommissioned.
5. **Ensure webhooks** (only when `-public-url` is passed). Register the events the router acts on — `issues_events`, `merge_requests_events`, `note_events`, `pipeline_events` — and **every other event explicitly off**, pointing at the engine's `/webhooks/gitlab`, carrying the `signing_secret` as the hook's `signing_token` (the caller-supplied, write-only `whsec_…` value GitLab uses to sign the `webhook-signature` header; it is never returned, so it must come from your side — see [Verification](#verification)). Existing hooks with the same URL are **updated, not duplicated**.

   > **Auto-generated signing secret.** If `signing_secret` points at a `${VAR}` that is unset, the provisioner **generates** a valid `whsec_<base64-of-32-bytes>` secret, stamps it on the hook, and records it to the token sink (env file or `-print`) under that var name — the same mint-into-`${VAR}` contract used for seat tokens. Source the sink into the engine's env and both sides share the value; re-runs reuse the persisted secret rather than regenerating (so a rotated hook and the engine stay in sync). This only happens when a hook is actually being created (`-public-url` given); provide the var yourself to pin a specific secret.

   > **Off is stated, never omitted.** GitLab defaults `push_events` to **true**, so a subscription body that simply does not mention push subscribes to it — every push on every repository delivered to an engine that answers `200` and drops it. So the hook is registered with the full flag set: the four above `true` and the other fifteen `false`. An emoji award is the near miss and is deliberately off: it names a user and a target but no party to notify.

   See [Where the webhook lands](#where-the-webhook-lands) for which level it goes on.

Human seats are never created — they carry `contact.gitlab_username` and are resolved, not provisioned.

> **Same `${VAR}` in both places.** Point `role.sandbox.env.GITLAB_TOKEN` at the *same* `${GITLAB_TOKEN_<SEAT>}` reference as `mcp_env.gitlab.GITLAB_TOKEN` (as the examples do) — one PAT, one identity for both tools and git.

### Where the webhook lands

**One level, never both.** A **group** hook already fires for every `issues`/`merge_requests`/`note`/`pipeline` event in *every* project of the group and its subgroups, and GitLab is explicit that a group hook and a project hook subscribed to the same events **both** fire for an in-project event — double delivery, which the engine's completion ledger deduplicates and its inbox does not. So `provisioning.group_webhook` picks a level:

| Mode | Behaviour |
|------|-----------|
| **`auto`** (default) | Try one **group hook**; on success stop, because it covers every project including ones added later. If the instance does not serve the group hooks API, fall back to **one hook per `provisioning.projects` entry** and record a note saying so |
| **`true`** | Group hook only. **Fail** if the group hooks API is unavailable — no silent fallback, because the mode exists for an operator who needs the group-level guarantee and would otherwise find out the day a new repository went unwatched |
| **`false`** | Per-project hooks only, one per listed `projects` entry, without touching the group |

> **The group hooks API is not everywhere.** It is a **Premium** feature on gitlab.com and it does not exist in **Community Edition** at all, and GitLab **hides an unavailable endpoint as a `404`** rather than answering `402` — so "not found" is what an instance says about a feature its tier does not serve. `auto` treats a `403`/`404` from that endpoint as the tier gate. Any other refusal — a `401`, a `5xx`, a transport failure — is a real problem and aborts, because falling back on it would paper over a broken credential with a set of project hooks nobody asked for.
>
> Measured, because the obvious guess is wrong: the **unlicensed `gitlab-ee`** image this repository's `docker compose --profile gitlab` stack runs (19.3.0, no license) *does* serve `GET /groups/:id/hooks`, so the local loop takes the group path. Set `group_webhook: false` to exercise the per-project path there.

Per-project mode needs `provisioning.projects` to list something. A run with none **refuses** rather than registering nothing: an instance reporting a healthy integration that delivers to nobody is exactly the failure the skip-rather-than-guess rule exists to prevent.

The report names the level — `on the group` or `on N project(s)` — because the two are not interchangeable, and the difference only shows up the day somebody adds a repository.

> **Transition caveat.** If a *prior* run created per-project hooks (group hooks were unavailable then) and a *later* run establishes a group hook (now available), the reconcile does **not** remove the old per-project hooks — you would get double delivery until you delete them. Deleting a redundant project hook is a manual step.

### Token sinks

Three sinks, chosen by flag:

- **`-secret-store`**: write each minted value into the encrypted [`secret_values`](../concepts/secret-store.md) table under the same `${VAR}` name the config references. The engine consults that table ahead of the environment, so the `source` + restart step disappears entirely. This is the recommended sink once a Tier A keyring is configured.
- **`-env-file PATH`**: append/update `VAR=token` lines — the file the operator feeds the engine. Written through on every mint, so a crash mid-run cannot leave a minted-but-unrecorded credential, and each write is atomic and leaves the file `0600` — including when you created it yourself, which under the usual umask means `0644`. A newly minted token is shown once; re-runs never re-print a live token.
- **`-print`**: emit `export VAR=token` lines to stdout for shell `eval` and persist nothing.

### Rotation & decommission

- **`-rotate`** mints a fresh token for **every** seat — including seats whose current one still works — retires the previous `crewlet-provision:<handle>` tokens on that account, and updates the chosen sink. It is a flag rather than what a run does because GitLab returns a token's value exactly once: a provisioner cannot check that what it recorded last time still matches, so minting every run would revoke the credential every agent is currently authenticating with. An operator adding a tenth seat would take the other nine down, from a command whose whole promise is that it is safe to re-run. **The engine has to be restarted after.** On the GitLab.com Free tier — where every PAT expires within 365 days — this is the once-a-year cron candidate.
- **`-decommission`** (explicit, never default) deletes managed service accounts whose seats have left the config. It refuses to act unless `provisioning.username_prefix` is set, so it can identify managed accounts without touching un-prefixed ones. Off by default because it is the one destructive direction, and a company mid-edit looks exactly like a company that removed a seat.

A run that changed nothing still says so: the report names the seats it **kept**, because a report listing only changes reads as a run that did nothing — and the operator's next move would be to reach for `-rotate`, which is exactly the outage above.

### Permission matrix — the operator credential

The provisioner's own credential is an **operator credential**, passed by `-admin-token` or `$GITLAB_ADMIN_TOKEN`, and is **never stored in company config**.

| Target | Required credential |
|--------|---------------------|
| **GitLab.com** (primary) | A **top-level group Owner PAT with the `api` scope** — no instance admin. Everything the provisioner touches (service accounts, their PATs, memberships, hooks) is group-Owner-callable on GitLab.com |
| **Self-managed** | An **instance admin PAT**, **or** a group Owner PAT with the instance setting `allow_top_level_group_owners_to_create_service_accounts` enabled |

On the GitLab.com Free tier, **annual token rotation is the norm** — every new PAT expires within 365 days (non-expiring service-account tokens require the Premium group setting). Wire `crewlet gitlab provision -rotate` into a yearly cron.

### Prerequisites (GitLab.com)

Two GitLab.com-specific conditions must hold before the service-accounts API will answer, or provisioning aborts on the first preflight with a `403`:

1. **The group Owner's identity is verified.** GitLab.com blocks the service-accounts API until the top-level group Owner (the account whose PAT you pass as the operator credential) has completed [identity verification](https://gitlab.com/-/identity_verification) — adding a credit card and/or phone number (no charge). This is an anti-abuse gate, unrelated to token scope: a brand-new automation account with a valid group-Owner `api` PAT still gets a `403` until it verifies. This is the most common cause of a `403` on an otherwise-correct setup.
2. **`provisioning.group` is a top-level group.** Service accounts are owned by, and managed from, the **top-level** group (they can then be invited into descendant subgroups and projects). Pointing `provisioning.group` at a personal namespace or a subgroup path yields a `403`. Free tier allows up to 100 service accounts per top-level group.

Both surface as `Provisioning cannot proceed: GitLab denied the service-accounts API for group '…' (403) …` with the fix inline. Service accounts are generally available on the Free tier (GitLab ≥ 18.11).

> **Declared projects must already exist.** The provisioner reconciles seats, tokens, memberships, and webhooks onto projects listed in `provisioning.projects` — it does **not create the projects themselves**. Create each project in the group first (or leave `projects` empty and rely on the group hook + group membership). A listed project that doesn't exist is dropped with a note, not created.

---

## Webhooks

Inbound GitLab events arrive at **`POST /webhooks/gitlab`**.

### Verification

Verification is the GitLab **signing token only**. The `webhook-signature` header is verified as a 19.1+ Standard-Webhooks HMAC-SHA256 over `{webhook-id}.{webhook-timestamp}.{body}` (constant-time compare, ±5-minute timestamp tolerance; the `whsec_…` secret's base64 payload is the HMAC key). An invalid or missing signature → **401**; a request that arrives before `signing_secret` is configured → **503** with a `Retry-After`, so the delivery is held for retry rather than discarded.

The provisioner sets that secret as each hook's `signing_token` when it registers webhooks. The weaker plain `X-Gitlab-Token` scheme is not supported.

GitLab **does not auto-retry** failed webhook deliveries (and auto-disables a hook after 4 consecutive failures), so operators use the manual resend endpoint; the engine carries the delivery UUID / `Idempotency-Key` into event metadata so resends are idempotent.

### Event routing

The parser turns a payload into a **list** of per-recipient notifications (one comment can @-mention several agents; one update can add several assignees/reviewers). Each names exactly one GitLab username, which the inbound service resolves to an agent or human seat, and carries `project`, `mr_iid`/`issue_iid`, `url`, `actor_external_id` (who caused the event) and an `event_type` of `"{object_kind}.{action}"`. The MR or issue is the **conversation** — `nimbus/api!42`, `nimbus/api#42` — project-qualified because an iid is unique only within its project, and that reference is the same string the prompt prints and the coalescer keys on.

Routing mirrors GitLab's own notification semantics, in **two layers**:

- **Directed events** target exactly the named party from the payload — an assignment, a review request, a mention, a failed pipeline. These never depend on any extra lookup.
- **Thread activity** — comments and state changes — fans out to the issue/MR **participants** (author + assignees + reviewers + commenters + previously-mentioned users): exactly the set GitLab itself would notify. Participants are not in webhook payloads, so this layer needs the `integrations.gitlab.token` read credential (one `GET …/participants` call per event); without it — or when the lookup fails — routing degrades to the payload-derived assignees.

| Hook (`object_kind`) | Routed to | `event_type` |
|---|---|---|
| `issue` | On `update`: newly-added assignees (diff of `changes.assignees`) + newly-added description `@mentions` (diff of `changes.description`). On open/reopen: assignees + description mentions. On `close`: **participants** (a human closing an agent's issue must reach the agent), assignees as fallback | `issue.assigned`, `issue.mention`, `issue.close` |
| `merge_request` | On `update`: newly-added reviewers (`changes.reviewers`), assignees (`changes.assignees`), and description mentions. On `approval`/`approved`/`unapproval`/`unapproved`/`merge`/`close`: **participants**, assignees as fallback. On open: reviewers + assignees + description mentions. On reopen: same, plus a participants fan-out (the whole thread wakes) | `merge_request.review_requested`, `merge_request.assigned`, `merge_request.mention`, `merge_request.{approval,approved,unapproval,unapproved,merge,close,reopen}` |
| `note` (comment) | Every **@-mentioned** registered username (directed), then **participants** (thread activity), noteable assignees as fallback | `note.mention`, `note.comment` |
| `pipeline` | Only when `object_attributes.status == failed`: the actor who triggered it — the owner who needs to fix the build, and the one event in the whole engine allowed past the self-action rule | `pipeline.failed` |
| `emoji` | Parsed but **not** routed (no reliable target on award events) | — |

Each recipient gets **one** notification per event — the first, highest-signal reason wins, so a mentioned participant pings as a mention rather than as thread activity.

The event's **actor** is not filtered by the parser. It is stamped under the one metadata key every integration writes, and the [self-action rule](../concepts/agent-runtime.md) suppresses it centrally — which is what lets the rule resolve an actor across identity namespaces (an agent's bot id and its member id are one seat) and lets `pipeline.failed` state its exception once, in the prompt, instead of as a flag each parser has to remember to set.

Mentions stay explicitly extracted rather than inferred from participation, for two reasons: a mention is a *directed ask* and gets a tailored prompt (participation can't distinguish "this note pings you" from "you once commented"), and GitLab materialises new mentions into the participants list via a background job, so the lookup can race the webhook — text extraction can't. GitLab sends **raw markdown with no parsed mention array**, so the parser extracts word-boundary `@username` tokens from note bodies *and* issue/MR descriptions (on `update`, only mentions *added* by the edit count — re-saving a description doesn't re-notify, matching GitLab's own semantics).

Both mention and participant fan-out are **intersected with the registered GitLab usernames** (agents ∪ human seats) — only parties the engine can route to are targeted, so outsiders (or `@here`, `a@b.com`) never produce undeliverable notifications. Bursts on the same MR/issue collapse into one digest turn via **inbox coalescing**, exactly like GitHub PR events — the participants fan-out raises reach, and coalescing keeps the turn cost bounded.

### Notification prompts

The prompt dispatches on the **routing reason**, not the event type, because one merge-request event reaches a reviewer, an assignee and a watcher and asks each of them for something different:

| `event_type` | Prompt behaviour |
|---|---|
| `merge_request.review_requested` | Read the **diff**, not the description; approve or comment on the diff; tell the requester where the conversation started. Declining is a reply, not silence — a review request is a direct ask |
| `merge_request.assigned` / `issue.assigned` | Read the item in full, do the work (an issue's code changes go through the sandbox, which opens an MR under the agent's own identity), report back |
| `note.mention`, `issue.mention`, `merge_request.mention` | Evaluate whether you were actually asked to do something, then respond on the same thread |
| `issue.close` | **Stop.** An agent that keeps working a closed issue is spending budget on a deliverable nobody will take; say what was already done, and raise a disagreement on the issue rather than reopening it |
| `pipeline.failed` | Read the **job log** — the status says nothing about the cause. The prompt states plainly that the agent is being told about its own action deliberately, or a seat that has learned "I am not notified of what I did" reads its own name as a routing mistake |
| Everything else (approvals, non-mention comments, merges, participant thread activity, and any reason a later release adds) | You are informed because you take part in this thread — which is a reason to be informed, not a request to act |

Review requests, assignments, and failed pipelines are **pointer events**: they name a diff, a thread or a job log to fetch before the agent has real context, so the Plan-phase relevance prefetches skip their aux-LLM call rather than filtering against a pointer. A comment is not one — its body *is* what was said.

---

## Identity registration

A GitLab webhook names people by **username**, and nothing in the org model says which account a seat holds. Without that mapping every event names a stranger, the routing gate drops every target, and the integration is silently inert — so this is the whole integration, not a detail of it.

At engine startup the engine calls **`GET {integrations.gitlab.url}/api/v4/user`** with each seat's own credential from `mcp_env.gitlab` and registers the returned `(gitlab, username) → agent handle` mapping. The username is **derived from the credential**, never declared beside it: a declaration that disagrees with the token is a misroute nothing can detect, and it would make the engine name a variable the seat's actual tools do not read. This is REST rather than an MCP round-trip because the official MCP server has no `whoami` and community servers disagree on its name, whereas `GET /user` is stable core API and needs only the seat's own token.

The lookups run **concurrently** and are **cached by token**. Identity is a function of the credential and credentials change rarely, so a config revision that touched something else re-registers every seat from the cache with **no requests at all**; a rotated token is a cache miss and costs exactly one, which is correct — it may well be a different account. Boot on a company of thirty seats is therefore one round trip per *distinct* credential, in parallel, not thirty in series.

A seat whose lookup **fails** is left unresolved rather than failing the boot — the instance may be briefly down — and the next apply retries it. What that costs is that seat's inbound routing until then, reported as `gitlab_seat_identity_unresolved`. A seat whose `${VAR}` credential does not resolve is skipped (no MCP instance was started for it either), and if two seats resolve to the same username the second is refused with a warning rather than misrouting.

The credential is read from whichever key the seat's tool stack names it under — `GITLAB_TOKEN`, `GITLAB_PERSONAL_ACCESS_TOKEN`, `Private-Token`, or `Authorization: Bearer …` — so the engine still names **no tool-specific variable of its own**; it reads the one the tools already use.

**Human seats** register their `contact.gitlab_username` through the same contact reconciliation every backend's human identities use, so a founder's or teammate's GitLab activity is attributed by name in agent prompts and webhook sender resolution — with no extra plumbing. A human's identities are declared rather than resolved against the instance, so that pass needs no credential and no network.

---

## GitHub vs GitLab identity

The two integrations rhyme deliberately, but they differ in exactly one place — **how a per-agent identity comes to exist**:

- **GitHub** has *no API to create assignable user identities.* An identity that can be @-mentioned, assigned an issue, and requested as a reviewer must be a real user account, and github.com offers no API to create user accounts or mint their tokens (2FA is mandatory). Machine users are therefore hand-created, and every seat rides a hand-minted PAT. Fully automated provisioning exists only on GitHub Enterprise.
- **GitLab** provides **service accounts** created *via API* — Free tier, no billable seat, full user semantics (assignable, mentionable, reviewer-able), custom username/display-name/email, and API-managed tokens. That is why `crewlet gitlab provision` can go from "role in `company.yaml`" to "agent with working credentials" with zero UI clicks, and GitHub cannot.

---

## Local testing

A profile-gated GitLab lives in `docker-compose.yml` so the whole loop is testable locally without touching a real gitlab.com group. It stays out of a plain `docker compose up` (GitLab is heavy) and opts in with a profile:

```bash
docker compose --profile gitlab up -d
scripts/gitlab-dev-bootstrap.sh          # mint a root token, open the SSRF allowlist, seed a group, provision
```

The profile ships one service:

- **`gitlab`** — `gitlab/gitlab-ee:latest` served at `http://gitlab.local:8929`. The EE image is deliberate: service accounts are a **Free-*tier*** feature that lives in **EE-*edition*** code, so the FOSS `gitlab-ce` image 404s on the `/service_accounts` API — an *unlicensed* `gitlab-ee` image runs as Free tier and serves it.

There is no MCP-server sidecar: the GitLab tool surface is `glab mcp serve`, which the engine spawns per-role (see [MCP tool server](#mcp-tool-server)).

`examples/nimbus.company.yaml` is the Nimbus example org on GitLab, and it targets **gitlab.com** as shipped (`url: https://gitlab.com`, `GITLAB_HOST: gitlab.com`, the git-auth recipe scoped to `gitlab.com`). To exercise it against the **local compose** instance instead, point those host references at `http://gitlab.local:8929` — everything else is identical.

### Walkthrough (Nimbus against local GitLab)

1. **Resolve `gitlab.local`.** The instance's `external_url` is `http://gitlab.local:8929`, so the engine/CLI must resolve that name to the published port on localhost. Add to `/etc/hosts`:
   ```
   127.0.0.1 gitlab.local
   ```
2. **Bring up the stack and seed GitLab.** The `gitlab` profile also pulls in Pulsar + Postgres:
   ```bash
   docker compose --profile gitlab up -d      # first boot of GitLab takes 3–6 min
   scripts/gitlab-dev-bootstrap.sh            # waits, mints a root PAT, opens the webhook SSRF allowlist, seeds nimbus-hq/nimbuscore
   ```
   The script prints the root PAT (`glpat-crewlet-dev-bootstrap`) and the UI login (`root` / `$GITLAB_ROOT_PASSWORD`). Local unlicensed `gitlab-ee` runs as **Free tier but with no identity-verification gate**, so the service-accounts API works immediately — none of the gitlab.com [identity-verification](#prerequisites-gitlabcom) friction applies locally.

   > **`curl http://localhost:8929/-/readiness` returns `404` from the host — that's expected, not a failure.** GitLab's monitoring endpoints (`/-/readiness`, `/-/liveness`, `/-/health`, `/-/metrics`) are IP-restricted to `127.0.0.0/8`/`::1/128` by default, and a host-side curl to the published port arrives with the Docker gateway's source IP, so GitLab hides them with a 404. `docker ps` showing the container `(healthy)` is the real signal (its healthcheck runs the same curl *inside* the container, where localhost is allowlisted). To check from the host, exec into the container (`docker exec <gitlab> curl -sf http://localhost:8929/-/readiness`) or hit a non-restricted route like `/users/sign_in`. The REST API (`/api/v4/…`) is not restricted, so provisioning works from the host regardless.
3. **Make a local-pointed copy of the config.** Rewrite the three host references (the default repo `.gitignore` covers `*.local.company.yaml`, so a copy you later personalize with real contact IDs can't be committed by accident):
   ```bash
   sed -e 's#https://gitlab.com#http://gitlab.local:8929#g' \
       -e 's#GITLAB_HOST: gitlab.com#GITLAB_HOST: http://gitlab.local:8929#g' \
       -e 's#gitlab.com#gitlab.local:8929#g' \
       examples/nimbus.company.yaml > nimbus.local.company.yaml
   ```
4. **Provision the agents.** The root PAT is the operator credential; `-public-url` is the engine's base address — its **embedded API** on **port 80** (`api.port: 80` in the quickstart's Tier A file), reachable from the GitLab container via `host.docker.internal` — and the provisioner appends `/webhooks/gitlab` itself. No `GITLAB_SIGNING_SECRET` needed — the provisioner [generates one](#what-a-run-does) and writes it to the env file:
   ```bash
   GITLAB_ADMIN_TOKEN=glpat-crewlet-dev-bootstrap \
     crewlet gitlab provision nimbus.local.company.yaml \
       -public-url http://host.docker.internal:80 \
       -env-file .env.gitlab
   ```
   Only `nimbus-hq/nimbuscore` is seeded by the bootstrap, so the config's other projects (`nimbusk0s`, `console`, `website`) are checked, **dropped with a note, and everything else still reconciles** — create them in the UI if you want them, then re-run (the reconcile is idempotent). The compose instance serves group hooks, so the webhook lands on the group; see [Where the webhook lands](#where-the-webhook-lands) for the instances where it does not.
5. **Run the engine** with the minted tokens sourced (add your base runtime `config.yaml` — providers, queue, DB — per the [quickstart](../getting-started/quickstart.md)). With `api.port: 80` in that Tier A file, the engine's **embedded API** receives the GitLab webhooks and serves the dashboard — one process is the whole stack; binding 80 needs privileged-port access on Linux (see the example config's `api` comment). (Do *not* also start a second, ingress-only node here — the two would fight over the port; splitting ingress off is for [fleets](../guides/fleet.md) only):
   ```bash
   source .env.gitlab
   crewlet run config.yaml --import-company nimbus.local.company.yaml
   ```
6. **Drive the loop in the UI** (`http://gitlab.local:8929`): create an issue in `nimbus-hq/nimbuscore` and **assign** it to an agent's service account (their handle appears in the assignee list). The assignment webhook wakes that agent, which reads the issue via its own `glab` MCP tools and acts as itself.

The full loop this validates: **provision** (service accounts appear with the agents' handles) → **assign/mention** → webhook wakes the agent → it reads/comments as itself → (where the sandbox can reach the instance) opens an MR as itself → the reviewer-added webhook wakes the reviewer → the review lands under the reviewer's identity.

> **Sandbox code-authoring is the one part that won't work against a laptop.** A cloud E2B sandbox cannot reach your machine's `gitlab.local:8929`, so `run_sandbox` MRs need a *reachable* instance — a self-hosted E2B domain on the same network, a tunnel, or gitlab.com. Provisioning, webhooks, identity resolution, and all `glab` MCP read/review/track work fine against local compose.

---

## Limitations

- **The default MCP tool server is experimental.** `glab mcp serve` is GitLab-official but flagged experimental; pin a known-good `glab` version if that matters. The community `@zereight/mcp-gitlab` is the supported alternative for a single shared server. GitLab's built-in `/api/v4/mcp` endpoint stays unused until it gains PAT authentication (it is OAuth-only today). See [MCP tool server](#mcp-tool-server).
- **A single group webhook is not available on every tier** — Premium on gitlab.com, absent from Community Edition. `group_webhook: auto` uses a group hook where the instance serves the API and registers **per-project** hooks where it does not; see [Where the webhook lands](#where-the-webhook-lands). Per-project hooks cover exactly the declared `provisioning.projects`, so a repository added later needs another run.
- **No composite identity.** GitLab's dual-attribution token mechanism (agent + triggering human) has no public API, so Crewlet's seats are plain service accounts. Every action is attributed to the agent that took it.
