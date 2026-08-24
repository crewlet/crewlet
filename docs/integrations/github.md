# GitHub Integration

> **v1 status — webhook ingestion only.** This build verifies and stores
> GitHub deliveries on `POST /webhooks/github`, and agents reach GitHub
> through the remote MCP server described below. What it does **not** yet
> have is the routing half: no parser turns a delivery into a notification,
> so a GitHub event does not wake a seat. The three vendors that route
> end to end are [Mattermost](mattermost.md) (chat), [Plane](plane.md)
> (tracker and knowledge) and [GitLab](gitlab.md) (code host). Everything
> below describes the intended contract; the parts that are live today are
> the `github:` config block, the webhook endpoint and the MCP surface.

Crewlet integrates with GitHub via the [remote GitHub MCP server](https://github.com/github/github-mcp-server), declared as a `shared: false` `http` server in `mcp_servers`. Each agent that supplies a GitHub token in `mcp_env.github` gets a per-role instance, giving them access to the full GitHub toolset — issues, PRs, repos, code search, actions. GitHub tools are for **reading, reviewing, and tracking** code (diffs, comments, reviews, PR status); **authoring** code changes goes through the [code sandbox](../concepts/code-sandbox.md). The top-level `github:` block carries only the inbound-webhook config.

---

## Configuration

The top-level `integrations.github` block is **non-tool config** — it enables inbound webhook handling:

```yaml
integrations:
  github:
    enabled: true
    webhook_secret: "${GITHUB_WEBHOOK_SECRET}"  # HMAC-SHA256 secret (required)
```

`webhook_secret` is **required** when GitHub is enabled. Inbound webhook requests to `POST /webhooks/github` are verified using **HMAC-SHA256** against the `x-hub-signature-256` header. Requests with invalid or missing signatures are rejected with 401. Set the same secret in your GitHub repository's webhook settings.

Declare the GitHub **MCP tool server** once in `mcp_servers` as a `shared: false` `http` server. Each agent supplies its token in `role.mcp_env.github` as an `Authorization: Bearer <token>` header:

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
  - name: Junior Engineer
    mcp_env:
      github:
        Authorization: "Bearer ${GITHUB_TOKEN_JUNIOR}"
    goal: "Write tests and small features"
  - name: Tech Lead
    goal: "Coordinate the team"           # no github creds → no GitHub tools
```

Each token needs appropriate scopes for the operations the agent will perform. The `Authorization` header is the only place the GitHub PAT lives — the engine forwards it to the role's GitHub MCP instance verbatim. See [Tools & MCP](../guides/tools-and-mcp.md#per-agent-identity).

---

## MCP Tools

For each role with a GitHub token in `mcp_env.github`, the engine launches a per-role instance of the [remote GitHub MCP server](https://github.com/github/github-mcp-server) (`https://api.githubcopilot.com/mcp/`) with the role's token. All tools are discovered via standard MCP and available alongside Jira/Slack MCP tools.

### Standard GitHub Tools

| Category | Tools |
|----------|-------|
| **Issues** | `issue_read`, `issue_write`, `add_issue_comment`, `list_issues`, `search_issues` |
| **Pull Requests** | `create_pull_request`, `list_pull_requests`, `pull_request_read`, `merge_pull_request`, `update_pull_request` |
| **Repositories** | `get_file_contents`, `create_or_update_file`, `push_files`, `search_code`, `list_branches` |
| **Actions** | `actions_list`, `actions_run_trigger`, `get_job_logs` |
| **Code Security** | `list_code_scanning_alerts`, `list_secret_scanning_alerts` |

### Copilot Tools

The remote GitHub MCP server also exposes GitHub Copilot tools. They stay
reachable on the agent's surface and an agent may call any of them. Crewlet's
own code-authoring path is the [code sandbox](../concepts/code-sandbox.md),
which is what `run_sandbox` drives.

| Tool | Description |
|------|-------------|
| `create_pull_request_with_copilot` | Create a PR from a prompt — Copilot writes the code and opens the PR |
| `assign_copilot_to_issue` | Assign Copilot to an existing issue |
| `request_copilot_review` | Request an automated code review from Copilot on an existing PR |

---

## How Code Authoring Works

Agents that the founder has gated with `role.sandbox.enabled` author code through the **code sandbox**. Execute has a `run_sandbox` tool: the planner lists it in `tools_needed`, Execute calls it, and a coding agent (Claude Code / OpenCode) runs inside an isolated E2B sandbox and opens a PR **as the agent's own GitHub identity** (the PAT the role declares as `GITHUB_TOKEN` in `role.sandbox.env` — by convention the same PAT as its `mcp_env.github` header). The call is detached — the Execute loop suspends and resumes with the result when the run completes, so the agent reports the PR in the same turn. The full design — sandbox provider, coding-agent runners, mid-task `crewlet-ask` human-in-the-loop, budget/tracing — is in [Code Sandbox](../concepts/code-sandbox.md).

GitHub tools stay in the picture on the **read/review/track** side: once a PR exists, agents read its diff, comment, review, and follow the PR's webhooks to report back to the original requester. A `pull_request.review_requested` event — whether it comes from a teammate or from an automated author — arrives via `POST /webhooks/github`, is routed through the NotificationService's `_parse_github` path, and wakes the requested reviewer.

### Tracking Context Across Async Work

Every agent is expected to **capture context** via `reflect_and_persist(ttl_days=30)` whenever it kicks off async work whose result returns later — a sandbox coding job, a handoff to another agent, or any out-of-band process.  This context is per-agent (only the originator needs to recall it), ephemeral (the task eventually closes), and naturally has a TTL (the typical PR / async-task lifetime).  That's exactly the SHORT-tier personal memory shape — see [agent-learning.md](../concepts/agent-learning.md#2-agentdiary--reflect_and_persist--in-flight-personal-memory).  Roles with GitHub creds in `mcp_env.github` also see the bundled `mcp:github` [Tool Skill](../concepts/tool-skills.md) in their Plan prompt (sourced from `examples/tool-skills/github.md` via the knowledge backend) that frames the GitHub tools as read/review/track tools, with code authoring pointed at the sandbox.

What to record: what was kicked off, reference details (repo, PR number, issue key), and where the original request came from (Slack channel/thread, Jira issue, the human who asked).  Example: `reflect_and_persist(content="Opened PR foo/bar #123 from sandbox run — original request from Sam in #engineering thread 1777...", ttl_days=30)`.

When the `review_requested` webhook arrives later, the agent receives a **tailored prompt** (`GitHubNotificationPrompt`) that instructs it to:

1. Look at its `## Personal memory` block (which the Plan-phase pre-fetch filtered against the incoming trigger) for the context of the work the PR came from
2. Report back to the original requester in the originating channel
3. Review the PR

This closes the loop — the agent doesn't just silently receive the PR, it actively reports back to whoever asked for the work.

**Use `reflect_and_persist` for that context** — it writes to the agent's private diary so the original ask + repo + PR number show up in the agent's `## Personal memory` Plan-prompt block on the review-notification turn. Pass `ttl_days=30` so the entry ages out automatically once the review work is done.

For team-shared conventions (e.g. "Engineering uses semantic commits") that other agents should also see, edit the relevant knowledge-base page (Confluence or Plane) via the agent's own page tools rather than capturing locally — humans and other agents reach that page through the query-time knowledge search (the `## Relevant knowledge` prefetch). The knowledge base is the single source of truth for shared procedural content.

### Webhook Routing

During engine startup, Crewlet resolves each role's GitHub username by calling the `get_me` MCP tool on the role's per-instance GitHub MCP server and registers it as an external identity in the HandleRegistry (`github` → `login` → `agent handle`). This mapping enables routing for all PR webhook actions:

- **`review_requested`** — routes to the requested reviewer
- **`assigned`** — routes to the assignee
- **All other PR actions** (`opened`, `synchronize`, `ready_for_review`, `closed`, etc.) — falls back to the first PR assignee or requested reviewer

The webhook parser extracts the GitHub login from the PR payload and resolves it against the registered external IDs. This is how PR events — a review request from a teammate, or a completion from an automated author — reach the right agent.

### GitHub Notification Prompts

A GitHub notification prompt would differentiate event types (this build ships none — see the note at the top):

| Event | Prompt behaviour |
|-------|-----------------|
| `review_requested` | Actionable — tells agent to check its context for the originating request, report back, and review |
| Everything else | Generic fallback — agent evaluates relevance and skips if not actionable |

This ensures agents act on meaningful events while quickly skipping noise.

---

## Example

```yaml
integrations:
  github:
    enabled: true
    webhook_secret: "${GITHUB_WEBHOOK_SECRET}"

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

The Senior Engineer can use any GitHub MCP tool — create issues, read code, search repos, review PRs. For coding tasks, a sandbox-enabled role uses the [`run_sandbox` code sandbox](../concepts/code-sandbox.md): the planner lists `run_sandbox` in `tools_needed`, a coding agent writes the change in an isolated sandbox and opens a PR under the engineer's own GitHub identity, and the PR's `review_requested`/`assigned` webhook wakes the engineer via the standard flow so they can review it and update the Jira ticket. (`request_copilot_review` is available as a lightweight automated-review option on an existing PR.)

---

## Limitations

- **Remote MCP server** — the Copilot tools (`create_pull_request_with_copilot`, `assign_copilot_to_issue`, `request_copilot_review`) are only available on the remote GitHub MCP server (`https://api.githubcopilot.com/mcp/`), not the local/self-hosted version. Standard GitHub tools work with both.
- **Code authoring is the sandbox's job** — agents author code through the [code sandbox](../concepts/code-sandbox.md), which requires the role to be gated with `role.sandbox.enabled` and an engine-level `providers.sandbox`. A role without that gate can still read, review, and track code via the GitHub tools; it just has no engine-supported path to author a PR.
