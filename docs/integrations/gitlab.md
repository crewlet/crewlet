# GitLab Integration

Crewlet integrates with GitLab as a first-class code host, independent of the [GitHub integration](github.md); an org can enable either or both. As with GitHub, GitLab tools **read, review and track** code (diffs, comments, reviews, approvals, MR/issue state, pipelines); **authoring** goes through the [code sandbox](../concepts/code-sandbox.md).

GitLab adds **automated, per-agent identity provisioning**. Each agent seat gets its own **service account**: a user that can be assigned issues and MRs, @-mentioned and requested as reviewer, costs no billable seat and cannot sign in through the UI. `crewlet gitlab provision` reconciles the company config into GitLab in one command, which GitHub's API cannot do (see [GitHub vs GitLab identity](#github-vs-gitlab-identity)). The top-level `integrations.gitlab` block carries the inbound-webhook and identity-resolution config.

> **Prerequisites.** The operator creates the **top-level GitLab group** by hand and mints the **operator credential**: a group-Owner PAT with the `api` scope on GitLab.com, or an instance-admin PAT on self-managed (see the [permission matrix](#permission-matrix--the-operator-credential)). Provisioning automates everything below that. The same configuration covers **gitlab.com and self-hosted instances**: point `integrations.gitlab.url` at yours.

---

## Setting it up from the dashboard

Connect GitLab in Company › Integrations with the instance address, the group and a group Owner token. The engine generates the signing secret and the reconcile loop registers the webhook on its next tick, running the same company-level pass `crewlet gitlab provision` runs. Each agent turned on in the Agents tab gets its own account through [per-agent access](#per-agent-access).

The **group Owner token is sealed in the fleet's secret store and kept**. It creates accounts and mints their tokens (a standing power), but removing an account needs the authority that created it, so without it the integration could not be disconnected with its accounts.

Rotating a seat's token and removing its account are Agents-tab actions, one seat at a time: a rotation seals the new token before revoking the old one, and removal is allowed only once the seat is off. Neither comes from a document edit or a timer.

See [Running the provisioning pass](../reference/api-endpoints.md#running-the-provisioning-pass).

**The signing secret is generated, and its shape matters.** GitLab computes the HMAC over the *decoded* bytes of a `whsec_` value and accepts no other form; a secret of another shape cannot match a delivery and nothing downstream catches it (config validation sees only the `${VAR}` reference). The dashboard, `crewlet gitlab provision` and GitLab's Generate button all mint the same shape.

## Configuration

The top-level `integrations.gitlab` block is **non-tool config**: inbound webhook handling and boot-time identity registration:

```yaml
integrations:
  gitlab:
    enabled: true
    url: "https://gitlab.com"                   # instance base URL — REQUIRED
    signing_secret: "${GITLAB_SIGNING_SECRET}"  # whsec_<base64 of 32 bytes>, the hook's signing token, REQUIRED
    token: "${GITLAB_ROUTING_TOKEN}"             # optional read credential → participants-based routing
    webhook_name: crewlet                       # which hooks on the instance are this deployment's
    provisioning:                # read by the engine's reconcile loop AND by `crewlet gitlab provision`
      group: nimbus-hq           # top-level group the agent service accounts join
      username_prefix: ""        # crewlet when empty; the username is <prefix>-<handle>
      projects: []               # the projects an agent may be scoped to (+ hooks only when group_webhook: false / falls back); empty: any project in the group
      group_webhook: auto        # auto (group hook, else per-project) | true (group only) | false (per-project only)
      mode: group                # where accounts are OWNED: group (default) | instance (self-managed only)
```

Each agent's role and projects are set per seat: see [Per-agent access](#per-agent-access).

Four fields differ from the [hosted code host's](github.md) block:

- **`url` is required** when enabled: needed for webhook links, boot-time identity resolution (`GET {url}/api/v4/user`) and provisioning.
- **`signing_secret` is required** when enabled: the HMAC key every inbound delivery is verified against and the route's **only** credential (see [Verification](#verification)). It must be `whsec_` plus standard base64 of a 32-byte key, the only shape GitLab signs with; anything else is refused at validation, and an unresolved `${VAR}` stops the code host at boot rather than 503-ing every delivery. A GitLab older than **19.1** cannot sign, so cannot deliver to this engine. Point it at a `${VAR}` and you need not invent a value: when `crewlet gitlab provision -public-url …` runs and the var is unset, the provisioner **generates a `whsec_…` secret**, stamps it on the hook and writes it back to the token sink (see [Provisioning](#what-a-run-does)). *Unset* means held by neither the company's secret store nor the environment behind it; the store is the fleet's however the run reaches it (the running node, the node `-api` names, or with this node's engine stopped, directly through the node's own broker or the external one it dials). A node that holds no data, or a cluster member with its engine stopped, cannot reach the store alone, so the run is refused there before it reads or mints anything, naming `-api`: it never treats a secret the fleet holds as unset and replaces it ([where a command reads the company's values](../reference/cli.md#where-a-command-reads-the-companys-values)). See [Webhooks](#webhooks).
- **`webhook_name` says which hooks on the instance are this deployment's.** The reconcile converges the hooks carrying that name wherever they point, so a changed public base leaves no live orphans (one group hook and one per project per change, enabled, signed, delivering nowhere). A hook with that name but a path other than `/webhooks/gitlab`, or with **no name**, was not registered by this engine and is left alone. The engine names every hook it registers, so a GitLab older than **17.1**, which drops the name, is refused and the hook it created is removed. Defaults to `crewlet`. **Two deployments watching one instance must set two names**: with one name (staging and production sharing a document) each pass repoints the other's hooks. Disconnecting removes hooks by the same name.
- **`token` (optional)** enables **participants-based routing**: comments and state changes fan out to everyone participating in the issue/MR (GitLab's notification reach) instead of only assignees and mentioned users. Payloads lack the participants list, so this costs one `GET …/participants` call per comment/state-change event, made with this credential: any group member's PAT with `read_api`, which **you supply** (nothing provisions it). Without it, routing falls back to payload-derived targets; directed events are unaffected. Mirrors `integrations.jira`'s admin token (watcher lookups). See [Event routing](#event-routing). The atlas reads the group with it where set ([what the atlas reads](#what-the-atlas-reads)).

The `provisioning:` sub-block drives the reconcile under [Provisioning](#provisioning); the CLI and the engine's reconcile loop both read it.

**`mode` says where a service account is owned**: which endpoint creates it, mints its tokens and deletes it. The group delete answers `404` as success ("unknown or already removed; both are the state the caller asked for"), so an instance-owned account sent down the group route would report deleted and stay live with every credential. Per-agent access reads it from the document on every pass.

---

## Per-agent access

Every agent seat carries its own GitLab block, edited in the integration's Agents tab. It stores GitLab's own terms:

```yaml
roles:
  - name: SRE
    integrations:
      gitlab:
        enabled: true            # off until turned on; off blocks the account and keeps it
        access_level: 20         # GitLab's integer: 10 Guest, 15 Planner, 20 Reporter, 30 Developer, 40 Maintainer, 50 Owner
        member_role_id: 1007     # or a custom role on GitLab Ultimate, instead of access_level
        projects: [nimbus-hq/infra]   # empty: a member of the whole group
```

| What the seat holds | How the engine applies it |
|---|---|
| The role | A direct membership at that `access_level` (or custom role) in each listed project, or in the group where `projects` is empty. Security Manager (a beta, disabled by default outside GitLab.com) and Minimal Access (grants nothing alone) are not offered. |
| The token | One personal access token named `crewlet-<handle>`; scopes follow the role: `read_api` and `read_repository` below Developer, `api` from Developer up. A role change that changes scopes mints a new token, seals it, then revokes the old one. |
| Off | Account blocked where the provisioning token is an instance administrator's; with a group Owner token (every GitLab.com deployment) its memberships are removed instead. Either way every token the engine minted is revoked; the account and history are kept. |
| Back on | Unblocked (only an account the engine blocked; one blocked by somebody else waits for a person to choose Turn back on), memberships restored, new token minted. |
| Remove | Separate, confirmed, allowed only while the seat is off. The account is deleted through the route matching its owner; GitLab keeps its contributions under the Ghost User. |
| Rotate | New token minted and sealed, then the previous revoked. A token an administrator made by hand on the account is never touched. |

The token is sealed as `GITLAB_TOKEN_<HANDLE>` (for example `GITLAB_TOKEN_SRE`), a name the engine derives; no document names it. The engine binds it to the seat's GitLab tool server under `GITLAB_TOKEN` (what `glab mcp serve` reads) or the `Private-Token` header (a shared HTTP server in remote-authorization mode).

Every pass reads each account back: state, memberships in the group and every project the company or a seat names, and the tokens the engine minted. A role or project changed by hand at GitLab is restored to the document's setting and reported; the Agents tab offers Keep the vendor's setting, which writes GitLab's value into the document. Maintainer and Owner are administration roles: a raise to either carries a warning and asks for a fresh step-up.

Custom roles are read live (Ultimate only): from the group on GitLab.com, from the instance on self-managed. A custom role GitLab no longer has is reported on the seat, never replaced by a default role.

---

## Per-role wiring

Declare the GitLab **MCP tool server** once in `mcp_servers` as a `shared: false` server. Each agent that is on for GitLab gets its own service-account token from the engine, bound to the server under the key it reads and put in its coding box as `GITLAB_TOKEN` for the git-auth recipe and `glab`, with no line in the company document ([An agent's own integration credentials](../concepts/code-sandbox.md#an-agents-own-integration-credentials)):

```yaml
mcp_servers:
  - name: gitlab                             # official glab CLI MCP server (stdio)
    shared: false
    command: glab
    args: ["mcp", "serve"]
    env:                                     # every seat's instance; never the host's environment
      GLAB_CHECK_UPDATE: "false"             # no daily update notice (or failed check) in a tool result
      GLAB_SHOW_WHATS_NEW: "false"           # no "What's new" banner after an upgrade, which the first leaves on
      GLAB_SEND_TELEMETRY: "false"           # no usage event to the instance per tool call

roles:
  - name: Agent SWE
    integrations:
      gitlab:
        enabled: true
        access_level: 30                         # Developer
    mcp_env:
      gitlab:
        GITLAB_HOST: gitlab.com                  # config only; the engine binds the seat's own token
```

`mcp_env.gitlab` does not carry the credential: the seat's token is the engine's (see [Per-agent access](#per-agent-access)), and a credential key there (`GITLAB_TOKEN`, `GITLAB_PERSONAL_ACCESS_TOKEN`, `Private-Token`, `Authorization`) is refused. Everything else in the block (`GITLAB_HOST`, an API url) is config, forwarded verbatim to the seat's instance.

The default is the official **`glab` stdio MCP server** (`glab mcp serve`): the engine's MCP bridge spawns one `glab` process per role with that role's `mcp_env.gitlab` env, so the per-agent PAT IS the per-agent identity, with no separate server to run. Boot-time identity resolution reads the token from whichever key is present (`GITLAB_TOKEN`, `GITLAB_PERSONAL_ACCESS_TOKEN`, a `Private-Token` header, or `Authorization: Bearer <pat>`), so the http alternative below works too. The engine adds the seat's token to the role's MCP instance under `GITLAB_TOKEN` and puts the same token in the seat's box as `GITLAB_TOKEN` while it provisions GitLab; a line of the seat's own under that name is ignored and reported as an advisory at that line. Where the agents' accounts are made outside Crewlet, the engine holds no token, and a seat declares its own `GITLAB_TOKEN` in `role.sandbox.env`. A seat that is off for GitLab gets no GitLab tools and no token in its box.

The entry's own `env` keeps what `glab` prints on its own out of tool results and stops a usage report to the instance on every call; see [MCP tool server](#mcp-tool-server).

---

## MCP tool server

The default tool server is the **official [`glab` CLI](https://gitlab.com/gitlab-org/cli)** running its built-in MCP server, `glab mcp serve` (stdio). It is declared like any stdio server and the bridge spawns **one `glab` process per role** with that role's `mcp_env.gitlab` env (the same shape as the `atlassian` (`uvx mcp-atlassian==0.23.1`) server), so **no separate MCP server runs or is hosted**. Each process authenticates as its own service account via `GITLAB_TOKEN` (and `GITLAB_HOST` for self-managed, with `GLAB_API_PROTOCOL: http` beside it when the instance serves plain HTTP: glab takes the API scheme from its `api_protocol` setting, `https` unless that variable says otherwise, never from the URL in `GITLAB_HOST`). The served surface is the full `glab` command tree as tools: MR approve (`glab_mr_approve`), diff (`glab_mr_diff`), notes and diff discussions (`glab_mr_note`, `glab_mr_note_create`), full issue CRUD, CI/pipelines, and `glab_api` for any raw authenticated call (including `glab_api /user` for identity). Interactive commands are excluded; `--output json` is added automatically.

- **Requirement:** the `glab` binary must be on the `PATH` of **every engine host that runs seats**: a seat's instance starts on the node holding its lease. Install per [GitLab's harness docs](https://gitlab.com/gitlab-org/cli); Homebrew (`brew install glab`) is the officially supported cross-platform method.
- **Status:** GitLab flags `glab mcp serve` **experimental** ("This feature is an experiment and is not ready for production use. It might be unstable or removed at any time."). It is official and actively developed; pin a known-good `glab` version if that risk matters.
- **What glab prints on its own is switched off in the entry's `env`** (read against glab 1.123.0). `glab mcp serve` answers each call with everything the `glab` command wrote, standard error included, so anything glab writes unasked lands in the agent's tool result behind the command's JSON; a result that no longer parses as JSON loses the parsed `data` glab otherwise puts in its structured content. glab writes two things unasked:
  - **An update check.** Once a day (timestamp kept under the engine user's `HOME`) glab asks gitlab.com for its newest release and, when newer, appends `A new version of glab is available` and a release-notes link. Where gitlab.com is unreachable it appends the failure (`ERROR: failed checking for glab updates: …`), only after the connection gives up: with gitlab.com's traffic dropped the call took 42 seconds, against under a tenth of a second with the check off. `GLAB_CHECK_UPDATE: "false"` turns it off.
  - **A "What's new" banner.** The first command after every glab upgrade, and the first in a home glab never ran in, appends `What's new in glab v<version>` and `Run: glab whatsnew`. `GLAB_CHECK_UPDATE` does not cover it; `GLAB_SHOW_WHATS_NEW: "false"` does.
- **glab reports every command to the instance it talks to**, and the entry switches that off too. After each tool call it posts a usage event to the instance's `/api/v4/usage_data/track_event` under the seat's token (the command, the project path, glab's version and platform, `invocation_source: mcp`, and for `glab_api` the route template), and one more when the server stops, waiting up to two seconds for each answer. None reaches the agent, but each call costs the instance a request under the seat's token, and against a slow instance every tool call, and stopping the server, takes up to two seconds longer (two seconds is how long the engine waits for a server to exit after closing its input before it signals it). `GLAB_SEND_TELEMETRY: "false"` stops the reports. Leave it out only if you want GitLab's [usage statistics](https://docs.gitlab.com/administration/settings/usage_statistics/) to count your agents' commands.

**The three switches go in the entry's `env`, nowhere else.** None is a credential, so they belong in the server entry every seat's instance starts with, not each seat's `mcp_env`. Never put them in the engine's own environment (a sourced `.env` or a container image's `ENV`): a stdio server gets only [the host variables the engine passes on](../guides/tools-and-mcp.md#what-a-stdio-servers-environment-is) and what its configuration declares, so a value set elsewhere never reaches glab. Spell each `"false"`: glab reads `GLAB_CHECK_UPDATE` and `GLAB_SEND_TELEMETRY` with Go's `strconv.ParseBool`, and a value that does not parse (`"no"`, `"off"`) puts `WARNING: Could not parse …` into every tool result.

**Alternative: one shared server.** [`@zereight/mcp-gitlab`](https://github.com/zereight/gitlab-mcp) (community, MIT; docker image `zereight050/gitlab-mcp`) in **streamable-HTTP + remote-authorization** mode (`STREAMABLE_HTTP=true`, `REMOTE_AUTHORIZATION=true`) is one process for the fleet, each request authenticated by its own `Private-Token` header. Declare it as a `shared: false` `http` server (`url: http://…/mcp`) and put the PAT in `mcp_env.gitlab` as `Private-Token`. Choose it to run one hosted server rather than a `glab` binary on the engine host; guardrails (`GITLAB_PERMISSION_MODE`, `GITLAB_TOOLSETS`, `GITLAB_DENIED_TOOLS_REGEX`) trim the catalogue, and `GITLAB_API_URL` targets self-managed.

GitLab's **built-in server-side MCP endpoint** (`POST /api/v4/mcp`, Free tier since 19.2) is **not used**: its authentication is OAuth-only (dynamic client registration with interactive consent per identity), unworkable for headless per-agent identities (PAT auth is an open request, [gitlab-org/gitlab#586184](https://gitlab.com/gitlab-org/gitlab/-/issues/586184)). When PAT auth lands, adopting it is a single-server swap.

The engine hardcodes no tool names: a bundled `mcp:gitlab` [Tool Skill](../concepts/tool-skills.md) frames the tools as read/review/track, with authoring in the run's own checkout with its shell and git, mirroring the `mcp:github` skill.

---

## How code authoring works

Agents author code through the **code sandbox**, not a GitLab tool. Every seat's executor is its harness's [own run in a box](../concepts/llm-backends.md#the-executors-run-in-a-box) (Claude Code, Codex, Gemini CLI, Qwen Code, OpenCode, Cursor Agent, GitHub Copilot CLI, Grok Build, Muse Code, Kimi Code, Hermes Agent or Pi), in a container or a box on the engine host, and opens a merge request **as the agent's own GitLab identity**: the token in its box as `GITLAB_TOKEN`, the same one its GitLab tool server reads ([Per-role wiring](#per-role-wiring)). The run is detached: the turn suspends at launch and resumes with the run's report, so the agent reports the MR in the same turn. Design: [Code Sandbox](../concepts/code-sandbox.md).

The GitLab **git-auth recipe** is config you write; the engine ships no git-auth (same as GitHub). Without it a headless `git clone https://gitlab.com/...` dies with `could not read Username`. The GitLab form of the [recipe on the sandbox page](../concepts/code-sandbox.md#the-git-auth-recipe):

```yaml
providers:
  sandbox:
    setup:
      - name: git-auth
        commands:
          - mkdir -p "$HOME/.local/bin"
          - |
            cat > "$HOME/.local/bin/git-credential-crewlet" <<'SH'
            #!/bin/sh
            # Supply this seat's GitLab token for gitlab.com credential
            # requests ONLY. Reads $GITLAB_TOKEN from the environment
            # (never persisted to disk).
            [ "$1" = "get" ] || exit 0
            [ -n "$GITLAB_TOKEN" ] || exit 0
            ok=""
            while IFS= read -r line; do
                [ -z "$line" ] && break
                [ "$line" = "host=gitlab.com" ] && ok=1
            done
            [ -n "$ok" ] || exit 0
            echo "username=oauth2"
            echo "password=$GITLAB_TOKEN"
            SH
          - chmod +x "$HOME/.local/bin/git-credential-crewlet"
          - 'git config --global credential."https://gitlab.com".helper "!''$HOME/.local/bin/git-credential-crewlet''"'
          - 'git config --global --add url."https://gitlab.com/".insteadOf "git@gitlab.com:"'
          - 'git config --global --add url."https://gitlab.com/".insteadOf "ssh://git@gitlab.com/"'
          - 'git config --global user.name "$CREWLET_AGENT_HANDLE"'
          - 'git config --global user.email "$CREWLET_AGENT_EMAIL"'
        env:
          GIT_TERMINAL_PROMPT: "0"
        brief: >-
          When $GITLAB_TOKEN is in your environment it is this seat's own
          GitLab PAT, for all GitLab work: git authenticates to gitlab.com
          with it automatically, so just clone, fetch, and push over plain
          HTTPS. SSH-style remotes (`git@gitlab.com:...`) are rewritten to
          HTTPS for you. Never embed the token in a URL. To open a merge
          request, use GitLab push options — no harness needed:
          `git push -o merge_request.create -o merge_request.target=main
          -o merge_request.title="<title>" origin HEAD:<your-branch>`
          creates the MR under your own identity. Without $GITLAB_TOKEN
          this seat has no GitLab identity: public projects still clone
          anonymously, but nothing you push is accepted, so hand a code
          change to the seat that owns the project instead. Your git
          commit identity is preconfigured.
```

The GitHub form's security properties hold here: the helper is scoped to the host at **both** layers (the `credential."https://…".helper` key *and* the `host=` check in the script), stays silent with no token so public clones fall through to anonymous, the `insteadOf` rewrites use `--add` (multi-valued key), and the commit identity comes from the generic `$CREWLET_AGENT_*` facts. The helper is written into the box's home by a command and registered as a quoted shell snippet: git needs its absolute path, which only the box's shell knows (`"$HOME"`), and git runs a helper's value through the shell, so the path is quoted for a home with a space ([Local sandboxes](../concepts/code-sandbox.md#local-sandboxes)). Two GitLab specifics: the basic-auth **username is arbitrary** for PAT auth (the token is the password; `oauth2` is a conventional placeholder), and the helper must match the host **including a non-standard port** (the dev compose serves `gitlab.local:8929`), so template the host from your `integrations.gitlab.url` rather than copying `gitlab.com`.

Every commit the seat makes as itself also credits Crewlet, with `Co-authored-by: Crewlet <noreply@crewlet.com>` and the run's `Crewlet-Run:`. Git adds them through a hook the box is given; a person's commit the seat amends or rebases is left alone; `providers.sandbox.commit_attribution: false` turns it off ([Commit attribution](../concepts/code-sandbox.md#commit-attribution)).

Once an MR exists, GitLab tools stay on the **read/review/track** side: agents read its diff, comment, approve, and follow its webhooks to report back to the requester. As with GitHub, an agent that starts async work whose result returns in a later turn (an MR awaiting review) should capture the context with `reflect_and_persist(ttl_days=30)`, so the original ask, project and MR number surface in its `## Personal memory` block on the review-notification turn.

---

## Provisioning

`crewlet gitlab provision <company.yaml>` is a one-shot, **idempotent reconcile** of the company-level half of GitLab, runnable any number of times: it checks the group, warns when the provisioning token is about to expire, and registers the webhook with its signing secret. It reads the company's `${VAR}`s (the instance URL and signing secret) and mints nothing but a signing secret. Each agent's account, role and token are [per-agent access](#per-agent-access), applied by the engine.

```bash
GITLAB_ADMIN_TOKEN="$GITLAB_ADMIN_TOKEN" crewlet gitlab provision company.yaml \
  -public-url https://engine.example.com \
  -secret-store
```

### Flags

| Flag | Description |
|------|-------------|
| `company.yaml` (positional) | Path to the Tier B company YAML |
| `-admin-token` | The provisioning credential (see the permission matrix below). Empty reads `$GITLAB_ADMIN_TOKEN` |
| `-public-url URL` | The engine's **public base address**, for example `https://engine.example.com`, not a webhook path: the engine derives `/webhooks/gitlab`. **Omit to skip webhook registration**, which the run reports |
| `-secret-store` | Write a minted signing secret into the encrypted [secret store](../concepts/secret-store.md), where every fleet node reads it |
| `-env-file PATH` | Env file to write a minted signing secret into. Exactly one of the three sinks: naming two is refused |
| `-print` | Print `export VAR=value` lines to stdout and persist nothing |
| `-config PATH` | Tier A config naming this node's store and secret keyring (default `crewlet.yaml`) |
| `-api URL` | A running node to read the company's stored values and record through |
| `-rotate` | Mint a fresh signing secret into the variable `signing_secret` names, wherever the hook is registered. Not what a re-run does: the hook stops verifying against the old key the moment it runs, until the engine reads the new one |
| `-dry-run` | Print what the run would do and touch nothing |

### What a run does

1. **Checks the group.** `provisioning.group` must exist: every agent's access is granted within it. This first call is also where a refused token shows up, named as a rejected credential.
2. **Warns about the provisioning token.** Where GitLab publishes the token's expiry and it falls inside the warning window, the run reports it: nothing rotates this credential, and the day it lapses every pass is refused.
3. **Ensures webhooks** (only when a public address is known). It registers the events the router acts on (`issues_events`, `merge_requests_events`, `note_events`, `pipeline_events`) with **every other event explicitly off**, pointing at the engine's `/webhooks/gitlab`.

   > **The hook's description records which key it carries, as a digest.** GitLab never returns a hook's signing token, so the digest is how a later pass tells a hook holding the current key from one holding a key rotated elsewhere.

   > **Auto-generated signing secret.** If `signing_secret` points at a `${VAR}` that is unset, the run generates a valid `whsec_<base64-of-32-bytes>` secret, stamps it on the hook, and records it to the chosen sink.

   > **Off is stated, never omitted.** GitLab defaults `push_events` to true, so a body that does not mention push subscribes to it.

   See [Where the webhook lands](#where-the-webhook-lands) for which level it goes on.
4. **Nothing is provisioned for `integrations.gitlab.token`.** It holds the read-only credential [participants-based routing](#event-routing) uses, and **you supply it**: a personal access token with the `read_api` scope.

The webhook is registered whatever the seats: a company whose agents are all off GitLab still has its hook.

### Where the webhook lands

**One level, never both.** A **group** hook fires for every `issues`/`merge_requests`/`note`/`pipeline` event in *every* project of the group and its subgroups, and a group hook and a project hook on the same events **both** fire: double delivery, which the engine's completion ledger deduplicates and its inbox does not. `provisioning.group_webhook` picks the level:

| Mode | Behaviour |
|------|-----------|
| **`auto`** (default) | Try one **group hook**; on success stop (it covers every project, including later ones). If the instance does not serve the group hooks API, fall back to **one hook per `provisioning.projects` entry** and record a note saying so |
| **`true`** | Group hook only. **Fail** if the group hooks API is unavailable: no silent fallback, so an operator needing the group-level guarantee does not learn of it the day a new repository goes unwatched |
| **`false`** | Per-project hooks only, one per listed `projects` entry, without touching the group |

> **On gitlab.com Free the group hooks API is worse than absent.** A free group **accepts** the registration (`POST /groups/:id/hooks` answers `201`, the hook appears in settings) and its event log stays empty for ever. So `auto` **reads the group's tier** rather than waiting for a refusal, from **both** places that state it: `GET /groups/:path` omits `plan` for a free group *exactly as a self-managed instance does*, while `/groups/<path>` gave no plan and `/namespaces/<path>` answered `plan: "free"` for the same path in the same second. Asking only the group endpoint made a free group read as paid, the fallback never ran, and nothing was delivered.

The namespace is asked **only when the group says nothing**, so a self-managed instance answers unknown at both and keeps the group hook it serves. A tier the instance could not answer for is **not** free: the pass stops rather than conclude one, because moving a working group hook to per-project hooks over a blip, and back on the next pass, rewrites somebody's webhooks on a timer.
>
> The refusal path still exists, because the API is also absent from **Community Edition**, and GitLab **hides an unavailable endpoint as a `404`** rather than `402`. `auto` treats a `403`/`404` from that endpoint as the tier gate. Any other refusal (`401`, `5xx`, transport failure) is a real problem and aborts, since falling back would paper over a broken credential with project hooks nobody asked for.
>
> The **unlicensed `gitlab-ee`** image this repository's `docker compose --profile gitlab` stack runs (19.3.0, no license) *does* serve `GET /groups/:id/hooks`, so the local loop takes the group path. Set `group_webhook: false` to exercise the per-project path there.

Per-project mode needs `provisioning.projects` to list something; a run with none **refuses** rather than register nothing, since a healthy-looking integration that delivers to nobody is the failure the skip-rather-than-guess rule exists to prevent.

The report names the level, `on the group` or `on N project(s)`: they are not interchangeable, and the difference shows the day somebody adds a repository.

**A disconnect sweeps every project in the group, not only those the config names.** The config does not record where hooks are: per-project hooks were written on the projects named *at the time*, so a project since dropped from `provisioning.projects`, or a group-only company, would leave hooks a current-list teardown never visits. Such hooks may point at dead tunnel hostnames (`trycloudflare`) that get re-issued to others: deliveries cannot be forged *into* the engine (signed with a secret the stranger lacks), but the payloads still leave your GitLab. So the teardown enumerates the group's projects (subgroups and archived included), unions them with whatever `provisioning.projects` names (a project there need not be in the group), and removes this engine's hooks from all. Only hooks carrying your `webhook_name` at a `/webhooks/gitlab` path are touched; a nameless hook is somebody else's and stays.

The steady-state reconcile does **not** walk the group: it runs every few minutes and is already O(seats × projects) over the projects you named, so a thousand-project group would turn each tick into a thousand hook listings. A disconnect happens once, when somebody presses the button, so the whole group is worth reading. Above 2000 projects it refuses and names the hooks to remove by hand.

**The level a pass stops writing at is swept, in both directions.** `group_webhook` is live and the `auto` answer depends on the group's plan, and a hook left at the abandoned level keeps delivering every event the new level already delivers. A run that establishes a **group** hook removes this engine's hooks from each declared `provisioning.projects` entry; a run that registers **per-project** hooks removes this engine's group hook. Only hooks carrying your `webhook_name` at a `/webhooks/gitlab` path are touched. A project dropped from `provisioning.projects` keeps its hook, as a departed seat keeps its account: a company mid-edit looks exactly like one that removed a project.

### Token sinks

Three sinks, chosen by flag, for the one value this command can mint, the signing secret:

- **`-secret-store`**: write it into the encrypted [secret store](../concepts/secret-store.md) under the name the config references. The engine consults the store ahead of the environment, so there is no file to source and every node reads it.
- **`-env-file PATH`**: append or update a `VAR=value` line, written atomically and left `0600`.
- **`-print`**: emit an `export VAR=value` line to stdout and persist nothing.

**The store outranks an env file and stdout.** The engine resolves every `${VAR}` from the secret store before its environment, so a secret minted into `-env-file` or `-print` under a name the store already holds would never be read; such a run is refused before it reaches GitLab.

### Rotation

- **`-rotate`** replaces the [signing secret](#verification), minted into the variable `signing_secret` names, wherever the hook is registered. It is a flag, not what a run does, because the hook stops verifying against the old key the moment it runs.
- **A seat's token** is rotated from the Agents tab: the new token is sealed before the old one is revoked.

### Permission matrix — the operator credential

The provisioner's own credential is an **admin credential**. On the command line it is passed by `-admin-token` or `$GITLAB_ADMIN_TOKEN` and read from the environment only. From the dashboard it is `integrations.gitlab.provisioning.admin_token`: a `${VAR}` in the document whose value is sealed in the fleet secret store.

**It is the one credential nothing rotates**, so the engine warns before it lapses: once it expires every pass is refused and no agent can be provisioned or repaired. Each pass reads its `expires_at` (`GET /personal_access_tokens/self`, GitLab 15.5 and later) and, inside **14 days** of it, reports a [`credential_expiring`](../concepts/integration-reconcile.md#what-a-pass-reports) finding naming `integrations.gitlab.provisioning.admin_token` and the date. The integration stays ready, and GitLab's page reads **Credential expiring** with **Replace key** as its one action until a new token is stored. A GitLab that cannot say (an older instance, or a credential that is not an access token) leaves a note on the run and no finding.

It is **held** rather than asked for each time because removing a service account needs the authority that created it; a credential dropped after every pass would leave every account the engine created outliving the integration. Disconnecting names the secret in `orphaned_secrets`, so revoking it afterwards is one command.

| Target | Required credential |
|--------|---------------------|
| **GitLab.com** (primary) | A **top-level group Owner PAT with the `api` scope** — no instance admin. Everything the provisioner touches (service accounts, their PATs, memberships, hooks) is group-Owner-callable on GitLab.com |
| **Self-managed**, `mode: group` (default) | An **instance admin PAT**, **or** a group Owner PAT with the instance setting `allow_top_level_group_owners_to_create_service_accounts` enabled |
| **Self-managed**, `mode: instance` | An **instance admin PAT**, always: a group Owner cannot create an account the instance owns. A `403` on this route says so by name, because the same status has a different remedy in each mode |

**Every token operation goes through the group that owns the account**, which
is what makes the GitLab.com row above true:

| Operation | Group route (a group Owner may call) | Instance route (admin only) |
|---|---|---|
| Mint | `POST /groups/:id/service_accounts/:uid/personal_access_tokens` | `POST /users/:uid/personal_access_tokens` |
| List | `GET /groups/:id/service_accounts/:uid/personal_access_tokens` | `GET /personal_access_tokens?user_id=` |
| Revoke | `DELETE …/personal_access_tokens/:token_id` under the group | `DELETE /personal_access_tokens/:token_id` |

**Both list routes are paged to exhaustion.** Each is read for a *destructive* decision (retiring earlier tokens, emptying an account being decommissioned), so a truncated read would leave a live `api`-scoped credential behind while the run reports success. GitLab's default page is twenty rows, the oldest, which may all be revoked already.

On GitLab.com nobody is an instance admin, so a run using the right column is refused at three moments: a mint `403`s, a list `401`s on the *next* pass, and a revoke fails inside a rollback whose message says a live credential was left behind. Instance mode uses the right column, because there no group owns the account.

On the GitLab.com Free tier, **annual token rotation is the norm** — every new PAT expires within 365 days (non-expiring service-account tokens require the Premium group setting). Wire `crewlet gitlab provision -rotate` into a yearly cron.

### Prerequisites (GitLab.com)

Two GitLab.com conditions must hold before the service-accounts API answers, or provisioning aborts on the first preflight with a `403`:

1. **The group Owner's identity is verified.** GitLab.com blocks the service-accounts API until the top-level group Owner (the account whose PAT you pass) has completed [identity verification](https://gitlab.com/-/identity_verification): a credit card and/or phone number (no charge). It is an anti-abuse gate unrelated to token scope, and the most common cause of a `403` on an otherwise-correct setup.
2. **`provisioning.group` is a top-level group.** Service accounts are owned by the **top-level** group (they can then be invited into subgroups and projects). A personal namespace or subgroup path yields a `403`. Free tier allows up to 100 service accounts per top-level group.

Both surface as `Provisioning cannot proceed: GitLab denied the service-accounts API for group '…' (403) …` with the fix inline. Service accounts are generally available on the Free tier (GitLab ≥ 18.11).

> **Declared projects must already exist.** The provisioner reconciles seats, tokens, memberships, and webhooks onto projects listed in `provisioning.projects`; it does **not create them**. Create each in the group first (or leave `projects` empty and rely on the group hook + group membership). A listed project that doesn't exist is dropped with a note.

---

## What the atlas reads

The engine reads the group for the company's [atlas](../concepts/atlas.md#what-integrations-read) once an hour unless `atlas.refresh_interval_seconds` says otherwise. Every call is a `GET`; nothing at GitLab is changed.

**Which credential.** The read uses `integrations.gitlab.token`, the read credential [participants-based routing](#event-routing) uses, where it resolves; else `provisioning.admin_token`. One read uses one credential: what it is refused is never retried under the other. To keep the group owner's token off this path, give the engine a `read_api` token whose owner holds the Reporter role on the group.

| Part | Calls | Role | What the atlas gains |
|---|---|---|---|
| Group | `GET /groups/{path}` | a member of the group | proof that the credential works and sees `provisioning.group` |
| Projects | `GET /groups/{id}/projects` with subgroups, 100 a page, and `GET /projects/{path}` for each project `provisioning.projects` names that the group does not hold | a member of the group or of the project | each project as a repository belonging to the group's account, with default branch, archived state, and GitLab visibility (`public`, `internal` or `private`) |
| Pipelines | `GET /projects/{id}/pipelines` for the newest finished pipeline on the default branch | Reporter, on a private project | the project's pipeline, built from the repository, with its result |
| Environments | `GET /projects/{id}/environments` for the available ones, then `GET /projects/{id}/environments/{id}` for each one read | Reporter, on a private project | each environment as a pipeline built from the repository, with its name, the state of its newest deployment, the environment its deployment tier names, and what it serves: the address and its host name |

**A project is one pipeline.** A GitLab project has one CI configuration, so the atlas shows one pipeline named for the project, once a pipeline has finished on the default branch. Its result is the newest finished pipeline's: `passed`, `failed`, `cancelled` or `skipped`. A merge-request pipeline runs on the MR's own ref and is never the branch's.

**A project Gitlet mirrors is the same entry** as the one GitLab lists: they meet on the exact address the mirror pulls from. Each host's visibility is shown ("Visibility on GitLab", "Visibility in Gitlet") and never compared.

**Keys are exact.** A repository is the instance's host and the project's whole path, `gitlab.com/acme/platform/api`, under the path the instance is served at where it has one. The project's pipeline is its repository and `#pipeline`; an environment is its repository, `#environment:` and its name, where a character outside letters, digits and `. _ + -` is written as `~` and its two hexadecimal digits, so `review/eu west` is `review~2feu~20west`. An environment's deployment tier (`production`, `staging`, `testing`, `development`) suggests that environment for the entry; a person confirms it.

**A failing pipeline is the health of what it deploys.** A project whose newest finished default-branch pipeline failed, and an environment whose newest deployment failed, each show as Needs a look, and so does every address and name the environment serves.

**A refusal is a finding, never a failed read.** Where GitLab refuses a project's pipelines or environments (owner below Reporter), that part is left out for the project, the source's standing names the part with the role to grant, and what an earlier read recorded stays. A group GitLab does not show to the credential, and a project in `provisioning.projects` it does not show, are findings the same way and retire nothing. A credential GitLab rejects at the group fails the read as a rejected credential.

**What is never read.** Who did anything: a pipeline's or deployment's user, a commit's author, every username, name and email. A project's description, topics, runner token, import address, service desk address and CI configuration path. A pipeline's name, commit and variables, a deployment's job, ref and commit. CI/CD variables, deploy keys, deploy tokens and protected environment rules. An address is kept without user information, query and fragment. A value shaped like a credential is refused and counted on the source's standing, never stored.

**What is left out on purpose.** The pipeline and environments of a project that is archived, empty, or has the feature switched off. A stopped environment (what a review environment becomes when its branch merges). Projects other groups share with the group. Of a project's available environments, the twenty nearest production are read (by tier, then name) and the rest counted.

A read costs two calls for the group, one more per hundred projects, one per named project outside the group, and per project that runs pipelines two calls plus one per environment read. On GitLab's rate limit, or any listing failing part way, the read stops, records nothing, and runs again five minutes later. A group holding more than 1,000 projects is not read: the standing says so, and nothing is retired. A repository, pipeline or environment that two complete reads at least a day apart both miss is retired from the atlas.

### The files the atlas reads

Where Gitlet does not mirror a project, the engine also reads its deploy and dependency files through GitLab, for [what a repository's files declare](../concepts/atlas.md#what-a-repositorys-files-declare). The closed list of files, what is decoded and what is never read are the same as for Gitlet's repositories. It uses the group's credential, with `GET` alone.

| Call | Role | Why |
|---|---|---|
| `GET /projects/{id}/repository/branches/{branch}` | Reporter, on a private project | where the default branch is, once a project a pass |
| `GET /projects/{id}/repository/tree?recursive=true`, by keyset, 100 a page | Reporter, on a private project | the tree at that commit, asked only when the branch moved |
| `GET /projects/{id}/repository/blobs/{sha}/raw` | Reporter, on a private project | one listed file whose content changed, by the object the tree names |

**An `.env` file is never asked for.** Neither is a values file, a variable or state file, a manifest named for secrets, `.gitlab-ci.yml`, or anything else off the list. A `Dockerfile` and a `Procfile` are not read (their presence says it). A symbolic link is never read. GitLab's tree carries no sizes, so a listed file is read up to 64 KiB and a larger one is counted and dropped.

**Bounds.** One pass reads at most 50 files and a mebibyte of them and lists at most 100 trees; the rest is read five minutes later. A tree past 10,000 entries has none of its files read. On GitLab's rate limit the pass stops and runs again five minutes later.

**What is not read.** An archived project, an empty one, one whose repository is switched off, and a project Gitlet mirrors (Gitlet's own scan reads it). A project whose code GitLab refuses the credential (owner below Reporter) is left as it was. While the group or a named project is not shown to the credential, nothing its files declared is withdrawn.

---

## Webhooks

Inbound GitLab events arrive at **`POST /webhooks/gitlab`**.

### Verification

A delivery is authenticated by its **signature**, and by nothing else.


`webhook-signature` is verified as a Standard-Webhooks HMAC-SHA256 over
`{webhook-id}.{webhook-timestamp}.{body}`, keyed on the `whsec_…` secret's
base64 payload, compared in constant time against any of the header's
space-separated `v1,…` entries, with a ±5-minute timestamp tolerance in both
directions.

- **No signature, or a wrong one → `401 invalid_signature`.** No fallback: GitLab's plaintext `X-Gitlab-Token` is not a credential here.
- **A delivery before `signing_secret` is configured → `503
  no_webhook_secret`, and one whose value is not a usable `whsec_` key →
  `503 unusable_webhook_secret`**, each with a `Retry-After`, so the delivery is held for retry rather than discarded. The fault is on this side, and the codes differ because the fixes do: set the secret, or fix the one set.

> **GitLab signs when the hook has a signing token, not when the version is
> new enough.** Two different secrets sit on the same hook:
>
> | Field | What GitLab does with it |
> |---|---|
> | **`signing_token`** | Signs every delivery, and sends `webhook-id`, `webhook-timestamp`, `webhook-signature`. Never returned by the API. |
> | `token` | Echoes it back verbatim as `X-Gitlab-Token`. GitLab's own docs call this "not recommended" and "weaker". |
>
> Given the signing key in `token`, the instance never signs and echoes a
> 32-byte HMAC key in cleartext on every delivery. Signing is supported from
> **19.1** (19.0 behind the `webhook_signing_token` flag).
>
> A GitLab older than 19.1 therefore cannot deliver to this engine at all;
> the alternative is accepting a plaintext bearer token on a public endpoint.

`crewlet gitlab provision` sets the hook's `signing_token`, mints a secret when the `${VAR}` is unset, and reads the hook back to confirm the instance kept its name and reports `signing_token_present`, so a too-old GitLab fails the run instead of delivering unsigned.

**If the key may have leaked**, re-run with `-rotate`, which replaces the signing secret as well as the seat tokens.

GitLab **does not auto-retry** failed deliveries (and auto-disables a hook after 4 consecutive failures), so use the manual resend endpoint; the engine carries the delivery UUID / `Idempotency-Key` into event metadata so resends are idempotent.

### Event routing

The parser turns a payload into a **list** of per-recipient notifications (one comment can @-mention several agents; one update can add several assignees/reviewers). Each names exactly one GitLab username, which the inbound service resolves to an agent or human seat, and carries `project`, `mr_iid`/`issue_iid`, `url`, `actor_external_id` (who caused the event) and an `event_type` of `"{object_kind}.{action}"`. The MR or issue is the **conversation** — `nimbus/api!42`, `nimbus/api#42` — project-qualified because an iid is unique only within its project. That string is what the prompt prints, the coalescer partitions on and the seat's sessions are kept under.

Routing mirrors GitLab's own notification semantics, in **two layers**:

- **Directed events** target exactly the named party from the payload — an assignment, a review request, a mention, a failed pipeline. No extra lookup.
- **Thread activity** — comments and state changes — fans out to the issue/MR **participants** (author + assignees + reviewers + commenters + previously-mentioned users), as GitLab would notify. Participants are not in webhook payloads, so this layer needs the `integrations.gitlab.token` read credential (one `GET …/participants` call per event); without it, or when the lookup fails, routing degrades to the payload-derived assignees.

| Hook (`object_kind`) | Routed to | `event_type` |
|---|---|---|
| `issue` | On `update`: newly-added assignees (diff of `changes.assignees`) + newly-added description `@mentions` (diff of `changes.description`). On open/reopen: assignees + description mentions. On `close`: **participants** (a human closing an agent's issue must reach the agent), assignees as fallback | `issue.assigned`, `issue.mention`, `issue.close` |
| `merge_request` | On `update`: newly-added reviewers (`changes.reviewers`), assignees (`changes.assignees`), and description mentions. On `approval`/`approved`/`unapproval`/`unapproved`/`merge`/`close`: **participants**, assignees as fallback. On open: reviewers + assignees + description mentions. On reopen: same, plus a participants fan-out | `merge_request.review_requested`, `merge_request.assigned`, `merge_request.mention`, `merge_request.{approval,approved,unapproval,unapproved,merge,close,reopen}` |
| `note` (comment) | Every **@-mentioned** registered username (directed), then **participants**, noteable assignees as fallback | `note.mention`, `note.comment` |
| `pipeline` | Only when `object_attributes.status == failed`: the actor who triggered it — the owner who must fix the build, and the one GitLab event allowed past the self-action rule | `pipeline.failed` |
| `emoji` | Parsed but **not** routed (no reliable target on award events) | — |

Each recipient gets **one** notification per event — the first, highest-signal reason wins, so a mentioned participant pings as a mention.

The parser does not filter the event's **actor**; the [self-action rule](../concepts/agent-runtime.md) suppresses it centrally, resolving an actor across identity namespaces (an agent's bot id and member id are one seat). `pipeline.failed` states its exception in the prompt.

Mentions are extracted from text, not inferred from participation: a mention is a *directed ask* with a tailored prompt, and GitLab adds mentions to participants in a background job that can race the webhook. GitLab sends **raw markdown with no parsed mention array**, so the parser extracts word-boundary `@username` tokens from note bodies *and* issue/MR descriptions (on `update`, only mentions *added* by the edit count).

Mention and participant fan-out are both **intersected with the registered GitLab usernames** (agents ∪ human seats), so outsiders (or `@here`, `a@b.com`) never produce undeliverable notifications. Bursts on the same MR/issue collapse into one digest turn via **inbox coalescing**, as for GitHub PR events.

### Notification prompts

The prompt dispatches on the **routing reason**, not the event type, because one merge-request event reaches a reviewer, an assignee and a watcher and asks each something different:

| `event_type` | Prompt behaviour |
|---|---|
| `merge_request.review_requested` | The request and the MR's description, and how a review is declined here: request review from the right reviewer and remove yourself |
| `merge_request.assigned` / `issue.assigned` | The item, and how an assignment is declined here: reassign it. For an issue it also says the agent itself makes the change, in its own checkout (its harness runs in its box with its own shell and git), opening the MR under its own identity |
| `note.mention`, `issue.mention`, `merge_request.mention` | Who named the agent and what they wrote. A mention is addressed, so the turn engine holds it to an answer and the prompt does not ask whether the agent was really asked |
| `issue.close` | **Stop** any in-flight work: a closed issue's deliverable has no taker |
| `pipeline.failed` | The pipeline and who triggered it. The prompt states that the agent is deliberately told about its own action, so a seat does not read its own name as a routing mistake |
| Everything else (approvals, non-mention comments, merges, participant thread activity, and any reason a later release adds) | You are informed because you take part in this thread, not asked to act; act only if it asks something of you or changes work you have in hand |

The prompt states only what the engine knows (why the agent is reading this, the item by the reference a person would paste, the product facts and decline gestures above); that a direct ask may not end in silence is the executor's contract, which the [turn engine enforces](../concepts/turn-engine.md#who-is-waiting-and-how-they-get-an-answer).

Review requests, assignments, and failed pipelines are **pointer events**: they name a diff, thread or job log to fetch, so the turn-start relevance prefetches skip their aux-LLM call and the executor searches once it has context. A comment is not one — its body *is* what was said.

---

## Identity registration

A GitLab webhook names people by **username**, and nothing in the org model says which account a seat holds. Without that mapping the routing gate drops every target and the integration is silently inert.

At engine startup the engine calls **`GET {integrations.gitlab.url}/api/v4/user`** with each seat's own credential from `mcp_env.gitlab` and registers the returned `(gitlab, username) → agent handle` mapping. The username is **derived from the credential**, never declared beside it: a declaration that disagrees with the token is an undetectable misroute. It is REST (`GET /user`), not MCP, because the official MCP server has no `whoami`.

Lookups run **concurrently** and are **cached by token**: a config revision touching something else re-registers every seat from the cache with **no requests**; a rotated token is a miss and costs one (it may be a different account). Boot is one round trip per *distinct* credential, in parallel.

A seat whose lookup **fails** is left unresolved rather than failing the boot (the instance may be briefly down), and the next apply retries it. Until then that seat's inbound routing is lost, reported as `gitlab_seat_identity_unresolved`. A seat whose `${VAR}` credential does not resolve is skipped (no MCP instance was started for it either), and if two seats resolve to the same username the second is refused with a warning.

The credential is read from whichever key the seat's tool stack names it under (`GITLAB_TOKEN`, `GITLAB_PERSONAL_ACCESS_TOKEN`, `Private-Token`, or `Authorization: Bearer …`).

**Human seats** register their `contact.gitlab_username` through the same contact reconciliation every backend's human identities use, so their GitLab activity is attributed by name in agent prompts and webhook sender resolution. Human identities are declared, not resolved against the instance, so that pass needs no credential and no network.

---

## GitHub vs GitLab identity

The two integrations differ in one place — **how a per-agent identity comes to exist**:

- **GitHub** has *no API to create assignable user identities.* An assignable, mentionable identity must be a real user account, and github.com offers no API to create accounts or mint their tokens (2FA is mandatory), so machine users and their PATs are hand-made. Full automation exists only on GitHub Enterprise.
- **GitLab** provides **service accounts** created *via API* — Free tier, no billable seat, full user semantics (assignable, mentionable, reviewer-able), custom username/display-name/email, and API-managed tokens. That is why `crewlet gitlab provision` goes from "role in `company.yaml`" to "agent with working credentials" with zero UI clicks.

---

## Local testing

A profile-gated GitLab lives in `docker-compose.yml` so the loop is testable without a real gitlab.com group. It stays out of a plain `docker compose up` (GitLab is heavy) and opts in with a profile:

```bash
docker compose --profile gitlab up -d
scripts/gitlab-dev-bootstrap.sh          # mint a root token, open the SSRF allowlist, seed a group, provision
```

The profile ships one service, **`gitlab`**:

- **`gitlab`** — `gitlab/gitlab-ee`, at the release `docker-compose.yml` pins, served at `http://gitlab.local:8929`. The EE image is deliberate: service accounts are a **Free-*tier*** feature in **EE-*edition*** code, so the FOSS `gitlab-ce` image 404s on the `/service_accounts` API, while an *unlicensed* `gitlab-ee` runs as Free tier and serves it.

There is no MCP-server sidecar: the GitLab tool surface is `glab mcp serve`, which the engine spawns per-role (see [MCP tool server](#mcp-tool-server)).

`examples/nimbus.company.yaml` runs no vendor: its code is on [Gitlet](../concepts/gitlet.md), the engine's own git host. The walkthrough below **adds** this local GitLab to a copy of it, which is most of what a real self-hosted GitLab takes. A code host runs **beside** the engine's own, so the copy keeps Gitlet for what it does not move.

### Walkthrough (Nimbus against local GitLab)

1. **Resolve `gitlab.local`.** The instance's `external_url` is `http://gitlab.local:8929`, so the engine/CLI must resolve it to localhost. Add `127.0.0.1 gitlab.local` to `/etc/hosts`:
   ```
   127.0.0.1 gitlab.local
   ```
2. **Bring up GitLab and seed it.** The `gitlab` profile brings up GitLab alone; the engine needs no other service:
   ```bash
   docker compose --profile gitlab up -d      # first boot of GitLab takes 3–6 min
   scripts/gitlab-dev-bootstrap.sh            # waits, mints a root PAT, opens the webhook SSRF allowlist, seeds nimbus-hq/nimbuscore
   ```
   The script prints the root PAT (`glpat-crewlet-dev-bootstrap`) and the UI login (`root` / `$GITLAB_ROOT_PASSWORD`). Local unlicensed `gitlab-ee` runs as **Free tier with no identity-verification gate**, so the service-accounts API works immediately; none of the gitlab.com [identity-verification](#prerequisites-gitlabcom) friction applies.

   > **`curl http://localhost:8929/-/readiness` returns `404` from the host — expected, not a failure.** GitLab's monitoring endpoints (`/-/readiness`, `/-/liveness`, `/-/health`, `/-/metrics`) are restricted to `127.0.0.0/8`/`::1/128`, and a host-side curl arrives with the Docker gateway's IP. `docker ps` showing the container `(healthy)` is the real signal. To check from the host, run `docker exec <gitlab> curl -sf http://localhost:8929/-/readiness` or hit a non-restricted route like `/users/sign_in`. The REST API (`/api/v4/…`) is not restricted, so provisioning works from the host.
3. **Make a GitLab-wired copy of the config.** The example carries none of what this page documents, so a local run adds it to a copy (the repo `.gitignore` covers `*.local.company.yaml`, so a copy with real contact IDs can't be committed by accident):
   ```bash
   cp examples/nimbus.company.yaml nimbus.local.company.yaml
   ```
   Then edit `nimbus.local.company.yaml`:
   - **one home per project.** The example hosts `nimbuscore`, `nimbusk0s`, `console` and `website` on Gitlet. Delete those four from `code.repos`, so each lives on GitLab alone; `nimbusflow`, `docs` and `infra` stay on Gitlet.
   - **`integrations.gitlab`**, under a new `integrations:` key:
     ```yaml
     integrations:
       gitlab:
         enabled: true
         url: "http://gitlab.local:8929"
         signing_secret: "${GITLAB_SIGNING_SECRET}"   # the provision run mints it, step 4
         token: "${GITLAB_ROUTING_TOKEN}"              # optional read_api token, below
         provisioning:
           group: nimbus-hq
           projects: [nimbus-hq/nimbuscore, nimbus-hq/nimbusk0s, nimbus-hq/console, nimbus-hq/website]
           group_webhook: auto
     ```
     Nothing mints the `token` it names: give it a `read_api` token on the local instance, or leave it unresolved and comments reach only the people a payload names (see [Event routing](#event-routing)).
   - **the `gitlab` MCP server**, under a new `mcp_servers:` key — the `shared: false` `glab mcp serve` entry under [Per-role wiring](#per-role-wiring), which names no host, so its `env` is the same against any instance.
   - **each GitLab seat** — `Agent DevRel`, `Agent SWE`, `Agent Frontend SWE` and `Agent AI Systems Engineer` — gets `integrations.gitlab: {enabled: true, access_level: 40}` (Maintainer: an agent must merge its own reviewed MR, GitLab's default protected `main` lets only maintainers merge, and a Developer would stall the loop on a human) and an `mcp_env.gitlab` of `GITLAB_HOST: http://gitlab.local:8929` and `GLAB_API_PROTOCOL: http`. The second is required: glab takes the API scheme from `api_protocol`, which is `https` unless `GLAB_API_PROTOCOL` says otherwise, not from `GITLAB_HOST`, so without it every glab call fails with `http: server gave HTTP response to HTTPS client`.
   - **the git-auth recipe** of [How code authoring works](#how-code-authoring-works), as a step under `providers.sandbox.setup` beside the example's `nimbus-conventions`, pointed at the local instance: the helper's check becomes `host=gitlab.local:8929` (git sends the port), its registration `'git config --global credential."http://gitlab.local:8929".helper "!''$HOME/.local/bin/git-credential-crewlet''"'` (the quoted helper path kept as it is), and the two `insteadOf` rewrites map `git@gitlab.local:` and `ssh://git@gitlab.local:2424/` (the compose instance's SSH port) onto `http://gitlab.local:8929/`; its brief says `gitlab.local:8929` and HTTP wherever the recipe says `gitlab.com` and HTTPS. The helper lives in the box's home, the only place a box writes a setup file (see [Local sandboxes](../concepts/code-sandbox.md#local-sandboxes)); see the caveat at the end of this section. Leave out the recipe's two `user.name`/`user.email` lines: the engine already sets the box's commit identity for Gitlet, and its settings win.
   - **every brief that says where a project lives** — the `nimbus-conventions` step's, which says every repository lives on Gitlet, and the `go` and `node` steps', which name the repositories their seats own — now says the four moved projects are at `http://gitlab.local:8929/nimbus-hq/<project>` and are proposed as merge requests. Every step's brief is folded into the harness's `## Your environment` block, so a brief still saying Gitlet sends the run to `code_checkout` for a repository Gitlet no longer holds.

   Each seat's token is the engine's, sealed as `GITLAB_TOKEN_<HANDLE>` once the seat is on, and put in its box as `GITLAB_TOKEN`; the copy names no token of its own.
4. **Register the webhook.** The root PAT is the provisioning credential; `-public-url` is the engine's base address (its **embedded API**, on the `api.port` your Tier A file binds, `8000` in `examples/nimbus.config.yaml`, reachable from the GitLab container via `host.docker.internal`), and the run appends `/webhooks/gitlab` itself. No `GITLAB_SIGNING_SECRET` is needed: the run [generates one](#what-a-run-does) and writes it to the env file. The run loads `examples/nimbus.config.yaml`, the Tier A file step 5 runs the engine with, so export what that file reads first: the keyring (`crewlet secrets keygen -key-id 2026-01` prints the line) and this node's own API token:
   ```bash
   export CREWLET_SECRET_KEY_2026_01="<the base64 key it printed>"
   export CREWLET_API_TOKEN_FOUNDER="$(openssl rand -hex 32)"
   GITLAB_ADMIN_TOKEN=glpat-crewlet-dev-bootstrap \
     crewlet gitlab provision nimbus.local.company.yaml \
       -public-url http://host.docker.internal:8000 \
       -env-file .env.gitlab -config examples/nimbus.config.yaml
   ```
   This is before that node's first start, so the run starts the node's own broker on its `stream.store_dir`, creates the fleet's store there as the engine's first boot would, and reads it, saying so on its first line. The bootstrap seeds only `nimbus-hq/nimbuscore`, so the other projects (`nimbusk0s`, `console`, `website`) are checked, **dropped with a note, and everything else still reconciles** — create them in the UI if you want them, then re-run (the reconcile is idempotent). A re-run once step 5 has started the engine goes through it, so export one of its Tier A tokens first — `export CREWLET_API_TOKEN="$CREWLET_API_TOKEN_FOUNDER"` — and a re-run with that engine stopped reaches the same store itself ([where a command reads the company's values](../reference/cli.md#where-a-command-reads-the-companys-values)). The compose instance serves group hooks, so the webhook lands on the group; see [Where the webhook lands](#where-the-webhook-lands) for instances where it does not.
5. **Run the engine**, which creates each agent's account and seals its token on its first pass. `examples/nimbus.config.yaml` works as the Tier A file as-is: with an `api.port` above 0 the **embedded API** both receives the GitLab webhooks and serves the dashboard, so one process is the whole stack. The port only has to match the `-public-url` you registered the hook with. (Do *not* also start a second, ingress-only node here: the two would fight over the port, and splitting ingress off is for [fleets](../guides/fleet.md) only):
   ```bash
   source .env.gitlab
   export CREWLET_BOX_GIT_URL="http://localhost:8000/box/git"   # the repositories the copy keeps on Gitlet
   crewlet run -config examples/nimbus.config.yaml -company nimbus.local.company.yaml
   ```
6. **Drive the loop in the UI** (`http://gitlab.local:8929`): create an issue in `nimbus-hq/nimbuscore` and **assign** it to an agent's service account (their handle appears in the assignee list). The assignment webhook wakes that agent, which reads the issue via its own `glab` MCP tools and acts as itself.

The full loop this validates: **provision** (service accounts appear with the agents' handles) → **assign/mention** → webhook wakes the agent → it reads/comments as itself → (where the sandbox can reach the instance) opens an MR as itself → the reviewer-added webhook wakes the reviewer → the review lands under the reviewer's identity.

> **Sandbox code-authoring reaches a laptop's instance only from a box that resolves it to the laptop.** The example's seats run `direct`, as processes on the machine running `crewlet run`, so their boxes resolve `gitlab.local` through the same `/etc/hosts` line and push to it. A seat whose entry places its runs `run_in: container` does not: a container has its own `/etc/hosts` and loopback. Map the name to the host in `local.run_args`, `["--add-host=gitlab.local:host-gateway"]` (the compose service publishes `8929` on every host interface), and the box reaches the instance as a `direct` one does. Provisioning, webhooks, identity resolution, and all `glab` MCP read/review/track work fine against local compose.

---

## Limitations

- **The default MCP tool server is experimental.** `glab mcp serve` is GitLab-official but flagged experimental; pin a known-good `glab` version if that matters. The community `@zereight/mcp-gitlab` is the supported alternative for a single shared server. GitLab's built-in `/api/v4/mcp` endpoint stays unused until it gains PAT authentication (OAuth-only today). See [MCP tool server](#mcp-tool-server).
- **A single group webhook is not available on every tier** — Premium on gitlab.com, absent from Community Edition, and on gitlab.com Free accepted but silently never delivered. `group_webhook: auto` reads the tier from the group and, where the group states none, from its namespace; it uses a group hook where the tier serves one and registers **per-project** hooks otherwise; see [Where the webhook lands](#where-the-webhook-lands). Per-project hooks cover exactly the declared `provisioning.projects`, so a repository added later needs another run.
- **No composite identity.** GitLab's dual-attribution token mechanism (agent + triggering human) has no public API, so seats are plain service accounts. Every action is attributed to the agent that took it.
