# GitHub Integration

GitHub is a **served code host**: a delivery from github.com or a GitHub
Enterprise Server wakes the seat it concerns, and `crewlet github provision`
registers the webhooks that carry it. A company can run it beside
[GitLab](gitlab.md) — they are two hosts with different repositories on them,
which is what a migration and an open-source presence both look like.

Two surfaces, and they are deliberately separate:

- **Inbound** — `integrations.github` plus `POST /webhooks/github`. This is
  how a review request, an assignment, a comment or a red workflow run
  reaches an agent.
- **Tools** — the [GitHub MCP server](https://github.com/github/github-mcp-server),
  a `shared: false` entry in `mcp_servers` with each agent's token in
  `role.mcp_env.github`. This is how an agent reads, reviews and tracks.

GitHub tools are for **reading, reviewing and tracking** code — diffs,
comments, reviews, run status. **Authoring** code changes goes through the
[code sandbox](../concepts/code-sandbox.md), which opens a pull request under
the agent's own identity.

---

## Configuration

```yaml
integrations:
  github:
    enabled: true
    # Omit url for github.com. An Enterprise Server names itself here.
    url: "https://github.example.com"
    webhook_secret: "${GITHUB_WEBHOOK_SECRET}"   # required when enabled
    token: "${GITHUB_ENGINE_TOKEN}"              # read credential; see below
    provisioning:                                # CLI-only, ignored by the engine
      org: acme
      repos: [acme/api, acme/web]
      org_webhook: auto
```

- **`url` is optional and its absence is meaningful.** Leave it unset for
  github.com, whose API lives on a different *host* (`api.github.com`) rather
  than a path on the web UI. An Enterprise Server names itself and the REST
  base is derived as `<url>/api/v3` — there is no second address to keep in
  step, and pasting the API base in instead of the instance URL is accepted
  rather than doubled.
- **`webhook_secret` is required** when enabled. Every delivery to
  `POST /webhooks/github` is verified as HMAC-SHA256 over the raw body
  against `X-Hub-Signature-256`; a route with nothing to verify with answers
  **503** rather than accepting the delivery. Unlike GitLab's, this secret
  has no required shape — GitHub takes any string and signs with it verbatim
  — so there is no wrong *shape* to catch, only a wrong value.
- **`token` (optional, but effectively required)** is a read credential for
  **participant fan-out**. A webhook payload carries the author, the
  assignees and the requested reviewers; it does not carry who has
  *commented* or *reviewed*, which is most of the set GitHub itself would
  notify. See [Participants](#participants-are-computed-not-read). Without
  it, thread activity degrades to the payload's author and assignees;
  directed events are unaffected. It is also the credential
  [`crewlet github provision`](#provisioning--crewlet-github-provision)
  registers webhooks with, and there it is **required**.
- **`provisioning:`** is read only by the CLI. `org` is the GitHub
  organization holding the repositories, `repos` are `owner/repo` entries to
  hook individually, and `org_webhook` is `auto` (default) / `true` /
  `false` — see [Webhooks](#webhooks).

### Seat identity is derived, never declared

A GitHub delivery names people by **login**, and nothing in the org chart
says which account a seat holds. At boot the engine calls `GET /user` with
each seat's own credential and registers whatever account answers.

A declared login beside the token would be cheaper and is the wrong shape: a
declaration that disagrees with the credential is a misroute nothing can
detect, and it would make the engine name a variable the seat's own tools do
not read. The credential is read from `role.mcp_env.github` under whichever
key that seat's tool stack uses — `GITHUB_TOKEN`,
`GITHUB_PERSONAL_ACCESS_TOKEN`, `GH_TOKEN`, or an `Authorization` header
(the `Bearer` and `token` schemes are both stripped).

The lookup is **cached against the credential**, not the seat: identity is a
function of the token, so a config apply that changed something else costs no
requests, and a rotated token costs exactly one.

A seat whose lookup fails is left unresolved rather than failing the boot —
GitHub may be rate-limiting — and the engine says so per seat
(`github_seat_identity_unresolved`). That seat receives no GitHub events
until the next apply re-resolves it. A company with **no** resolved seats
logs `github_has_no_seat_identities`, because the integration is completely
inert in that state and nothing else would say so.

### Human seats

A human seat holds no tool credential. It is addressed by
`contact.github_login` in the org chart, which the party registry registers
directly — so a person can be mentioned in a comment and reached by the
engine's notification spine without ever holding a token here.

---

## Webhooks

`POST /webhooks/github` verifies HMAC-SHA256 over the raw body against
`X-Hub-Signature-256`, and dedupes on `X-GitHub-Delivery` — GitHub sends a
stable per-delivery uuid, the same on every retry and on a redelivery an
operator triggers by hand, so a redelivery does not wake the seat again.

The event name arrives in the **`X-GitHub-Event` header**, not the body.
GitHub puts only the action in the payload, so `{"action": "created"}` is the
whole discriminator a body-only reader gets — created *what* is not in there.

### The subscribed events

The hooks the provisioner registers subscribe to exactly what the parser
reads, never `*`:

`issues` · `pull_request` · `issue_comment` · `pull_request_review` ·
`pull_request_review_comment` · `workflow_run`

A wildcard hook delivers every push, star and fork — thousands a day on a
busy repository, each one verified, stored, deduped and routed to nobody.
`check_run` is deliberately excluded: it reports the same failing Actions run
as `workflow_run`, once per job, so subscribing to both would wake one seat
as many times as the workflow has jobs.

### One organization hook, or one per repository

An **organization** hook covers every repository in the org, including ones
created after the run — the difference between a new repository routing on
day one and routing whenever somebody remembers to re-run the provisioner.
It needs the `admin:org_hook` scope, which a fine-grained token cannot carry
at all and a classic token carries only if whoever minted it ticked the box.

| `org_webhook` | Behaviour |
|---|---|
| `auto` (default) | Try one org hook; fall back to per-repository hooks if the credential may not, saying so in the run's notes |
| `true` | Demand the org hook. A credential that cannot register it **fails the run** — an operator who asked for this arrangement must not silently get the other one |
| `false` | Always register per-repository hooks |

A working org hook means the `repos` list is **not** hooked separately: two
hooks on one repository deliver every event twice.

---

## Routing

Two layers, and the difference between them is what a seat is being asked
for.

**Directed** events name their recipient in the payload. They route from the
payload alone, need no reads, and survive a lapsed credential:

| Event | Reaches | Reason stamped |
|---|---|---|
| `pull_request` `review_requested` | The requested reviewer | `pull_request.review_requested` |
| `pull_request` `opened` | Every reviewer the pull request already requests, every assignee, and anyone the body `@`-mentions — each under its own reason, so an opener who assigned and mentioned one person wakes them once | `pull_request.review_requested` / `.assigned` / `.mention` |
| `pull_request` / `issues` `assigned` | The named assignee | `pull_request.assigned` / `issue.assigned` |
| `pull_request_review` `submitted`, changes requested | The pull request's author | `pull_request.changes_requested` |
| `pull_request_review` `submitted`, approved | The author | `pull_request.approved` |
| `pull_request` `closed` with `merged: true` | The author and assignees | `pull_request.merged` |
| `pull_request` `closed` without it | The author and assignees | `pull_request.close` |
| `pull_request` `reopened` | The author and assignees | `pull_request.reopened` |
| `pull_request` `ready_for_review` | The author and assignees | `pull_request.ready_for_review` |
| `pull_request` `converted_to_draft` | The author and assignees | `pull_request.converted_to_draft` |
| `issues` `closed` | The assignees | `issue.close` |
| A `@login` in any body | Whoever was named | `…mention` |
| `workflow_run` `completed`, conclusion `failure` | **The run's own actor** | `workflow_run.failed` |

The four state changes — `closed`, `reopened`, `ready_for_review`,
`converted_to_draft` — take the **author first**. A pull request's outcome is
news to whoever opened it before it is news to anyone else, and GitHub gives
the login rather than an opaque id, so it needs no lookup and works with no
credential at all. `closed` splits on `merged` because to the author those are
opposite outcomes: one means the work landed and the other means somebody
decided it would not.

**Thread activity** — a comment, a close, a merge — concerns everyone taking
part, which GitHub does not put in the payload. It costs one read per issue
event and two per pull request, and without a `token` it degrades to the
author and assignees the payload does carry. That degradation can only ever
cost reach on the watching layer, never on the directed one.

Where several reasons name one person, **the first wins**, and the list
arrives in priority order — so a mentioned author is woken once, as a
mention, which is the stronger claim on their attention and the one the
prompt renders differently.

### What deliberately does not route

- **Bookkeeping.** A label, a milestone, a `synchronize` (new commits
  pushed), an auto-merge toggle. Each changes the item without asking anyone
  for anything, and routing them produces turns triaging "somebody added a
  label".
- **An edit, beyond the names it added.** GitHub's own rule: re-saving a body
  does not re-notify the people it already named. Only newly-added mentions
  route, so a typo fix pings nobody.
- **A deleted comment.** Whatever it said is gone, and a notification
  pointing at it sends the recipient to a 404.
- **A green, cancelled or timed-out run.** A cancel is somebody deciding the
  run was unnecessary; a timeout is usually the runner rather than the diff.
- **A team review request.** `@acme/reviewers` names no person, and the
  `acme` half is not one either — reading it as a login wakes whichever seat
  happens to share the organization's name on every team ping. Expanding the
  team would mean a members lookup on the inbound path to produce a fan-out
  GitHub itself treats as weaker than a direct request. It is logged
  (`github_team_review_request_not_routed`) rather than dropped silently.
- **Anyone who is not a seat here.** A repository has contributors who are
  not in this company; the registry is the single gate every fan-out passes
  through.

### The one event addressed to its own actor

A seat is never told about its own actions — with one exception in the whole
engine. A **failed workflow run** names the person whose push triggered it,
and when it goes red they are the only one who can fix it. A build runs
asynchronously, minutes after the push, and reports a result nobody could
have predicted, so suppressing it means the person who can act never learns.

The prompt says so out loud: a seat that has learned "I am not told about my
own actions" reads its own name as a routing mistake otherwise.

### Participants are computed, not read

GitHub has no participants endpoint. Its own subscription rule is that you
are subscribed once you author, are assigned, are mentioned, comment or
review — and of those five, three are in the webhook payload and one is in
the text. What is left, and what the engine reads, is the two that are only
in the API:

- `GET /repos/{owner}/{repo}/issues/{n}/comments` — who has commented.
- `GET /repos/{owner}/{repo}/pulls/{n}/reviews` — who has reviewed, on a
  pull request. A reviewer who approved without writing anything appears in
  neither the other, and is exactly the person who should hear that the
  author pushed again.

Both are one page of 100, read concurrently, never a cursor walk: a thread
with more than a hundred commenters is one where notifying all of them is the
wrong behaviour anyway, and the call sits on the inbound consumer's hot path.

A pull request's *conversation* comments arrive as `issue_comment`, because
GitHub models a pull request as an issue with a diff. The engine reads that
from the payload rather than the event name, so a pull-request comment is
never filed as an issue — which would ask the wrong collection for its
participants and lose every reviewer.

---

## Provisioning — `crewlet github provision`

```bash
crewlet github provision company.yaml \
  -public-url https://crewlet.example.com \
  -env-file .env
```

**It reports more than it changes, and that is GitHub's shape.** GitHub
issues no user account and no personal access token on a provisioner's
behalf: there is no API that creates a user, and the API that once minted a
token for somebody else was withdrawn in 2020. A command that offered to
provision accounts would print instructions dressed as actions.

So it does the two things GitHub genuinely allows:

1. **Reports which account each seat's credential authenticates as** — the
   finding an operator acts on, because a seat with no login receives nothing
   and its inbound routing is simply silent.
2. **Registers the webhooks**, on the organization where the credential may
   and on each named repository where it may not.

| Flag | Effect |
|---|---|
| `-public-url URL` | This deployment's public base. Without it no webhook is registered — a hook pointing at the wrong host is worse than no hook, because GitHub then reports a healthy integration delivering into the void |
| `-secret-store` / `-env-file PATH` / `-print` | Where a minted webhook secret goes. **Required for a real run** — a run with nowhere to put what it mints creates a live secret and prints none of it |
| `-recreate-webhooks` | Delete and remake every hook to mint a fresh secret. **Destructive**: it invalidates the secret every other deployment of this company holds |
| `-dry-run` | Read and report; register nothing, and do not open the secret store |

**A working secret is never reminted.** The engine is running with the old
one, so re-registering with a fresh secret would have GitHub sign every
delivery with a key the running engine does not hold — every webhook refused
at the edge, from a command whose whole promise is that it is safe to re-run.
A secret that already resolves is used as it is; one that resolves to nothing
is minted into the `${VAR}` the config already points at, and the run says
where it went.

**A repository that cannot be hooked is reported, not raised.** A company's
list will contain one that was renamed, archived, or made private to a team
this credential is not in. Failing the whole run over it would leave every
other repository unhooked to punish one typo. Note that GitHub answers **404
for both "absent" and "invisible to this credential"** — deliberately, so a
probe cannot enumerate what exists — so the report says both.

---

## MCP tools

Declare the GitHub MCP server once as a `shared: false` `http` server; each
agent supplies its own token:

```yaml
mcp_servers:
  - name: github
    transport: http
    shared: false
    url: "https://api.githubcopilot.com/mcp/"

roles:
  - name: Senior Engineer
    mcp_env:
      github:
        Authorization: "Bearer ${GITHUB_TOKEN_SENIOR}"   # per-agent PAT
    goal: "Implement backend features"
  - name: Tech Lead
    goal: "Coordinate the team"           # no github creds → no GitHub tools
```

The `Authorization` header is the same credential the engine resolves the
seat's login from — one secret, named once. See
[Tools & MCP](../guides/tools-and-mcp.md#per-agent-identity).

| Category | Tools |
|----------|-------|
| **Issues** | `issue_read`, `issue_write`, `add_issue_comment`, `list_issues`, `search_issues` |
| **Pull Requests** | `create_pull_request`, `list_pull_requests`, `pull_request_read`, `merge_pull_request`, `update_pull_request` |
| **Repositories** | `get_file_contents`, `create_or_update_file`, `push_files`, `search_code`, `list_branches` |
| **Actions** | `actions_list`, `actions_run_trigger`, `get_job_logs` |
| **Code Security** | `list_code_scanning_alerts`, `list_secret_scanning_alerts` |

The remote server also exposes GitHub Copilot tools
(`create_pull_request_with_copilot`, `assign_copilot_to_issue`,
`request_copilot_review`). They stay reachable and an agent may call any of
them; Crewlet's own code-authoring path is the
[code sandbox](../concepts/code-sandbox.md), which is what `run_sandbox`
drives. The Copilot tools are only on the remote server, not a self-hosted
one.

---

## How code authoring works

A seat the founder has gated with `role.sandbox.enabled` authors code through
the [code sandbox](../concepts/code-sandbox.md). `run_sandbox` is on the
executor's surface; it calls it, and a coding agent runs in an isolated box
and opens a pull request **as the agent's own GitHub identity** — the token
the role declares in `role.sandbox.env`, by convention the same one as its
`mcp_env.github` header. The call is detached: the executor loop suspends and
resumes with the result, so the agent reports the pull request in the same
turn.

GitHub stays in the picture on the read/review/track side. Once the pull
request exists, its `review_requested` event wakes the reviewer through
exactly the path above — there is nothing special about a pull request an
agent opened.

### Tracking context across async work

An agent that kicks off async work whose result returns later should capture
the context with `reflect_and_persist(ttl_days=30)`: what was kicked off, the
repository and number, and where the original request came from. That is the
SHORT-tier personal memory shape — see
[agent-learning.md](../concepts/agent-learning.md#2-agentdiary--reflect_and_persist--in-flight-personal-memory).

When the review request arrives later, the turn-start prefetch filters that
diary against the incoming trigger, so the original ask shows up in the
agent's `## Personal memory` block and it can report back to whoever asked
rather than silently reviewing.

Roles with GitHub credentials also see the bundled `mcp:github`
[Tool Skill](../concepts/tool-skills.md) in their executor prompt, which frames
the GitHub tools as read/review/track tools with authoring pointed at the
sandbox.

For team-shared conventions — "Engineering uses semantic commits" — edit the
knowledge-base page instead. The knowledge base is the single source of truth
for shared procedural content; a diary entry is private to one seat.

---

## Example

```yaml
integrations:
  github:
    enabled: true
    webhook_secret: "${GITHUB_WEBHOOK_SECRET}"
    token: "${GITHUB_ENGINE_TOKEN}"
    provisioning:
      org: acme
      repos: [acme/api]
      org_webhook: auto

mcp_servers:
  - name: github
    transport: http
    shared: false
    url: "https://api.githubcopilot.com/mcp/"

units:
  - name: Backend
    type: team
    lead: Tech Lead
    roles:
      - name: Tech Lead
        goal: "Ship backend features on time with high quality"
        manages: ["Senior Engineer", "Junior Engineer"]
      - name: Senior Engineer
        mcp_env:
          github: { Authorization: "Bearer ${GITHUB_TOKEN_SENIOR}" }
        goal: "Implement complex backend features"
      - name: Junior Engineer
        mcp_env:
          github: { Authorization: "Bearer ${GITHUB_TOKEN_JUNIOR}" }
        goal: "Implement straightforward features and write tests"
```

---

## Limitations

- **Team mentions and team review requests reach nobody.** Both name a team
  rather than a person; see [What deliberately does not route](#what-deliberately-does-not-route).
- **The provisioner creates no accounts.** GitHub has no API for it. Machine
  users and their tokens are created by hand, and the command reports which
  account each one turned out to be.
- **An installation token cannot hold a seat's identity.** It authenticates
  as an app rather than a person, so `GET /user` names nobody and the seat is
  reported unresolved.
- **Code authoring is the sandbox's job.** A role without
  `role.sandbox.enabled` and an engine-level `providers.sandbox` can still
  read, review and track through the GitHub tools; it has no engine-supported
  path to author a pull request.
