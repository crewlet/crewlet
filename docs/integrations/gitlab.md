# GitLab Integration

Crewlet integrates with GitLab as a first-class code host, alongside — and independent of — the [GitHub integration](github.md). The two coexist; an org can enable either or both. The split is the same one GitHub uses: GitLab tools are for **reading, reviewing, and tracking** code (diffs, comments, reviews, approvals, MR/issue state, pipelines); **authoring** code changes goes through the [code sandbox](../concepts/code-sandbox.md).

What GitLab adds over GitHub is **automated, per-agent identity provisioning**. Each agent seat gets its own GitLab **service account** — a first-class user that can be assigned issues and merge requests, @-mentioned, and requested as a reviewer, but which costs no billable seat and cannot sign in through the UI. Because GitLab exposes an API to create these accounts and mint their tokens, `crewlet gitlab provision` reconciles the whole company config into GitLab in one command — something GitHub's API cannot do (see [GitHub vs GitLab identity](#github-vs-gitlab-identity)). The top-level `integrations.gitlab` block carries the inbound-webhook and identity-resolution config.

> **Prerequisites.** The operator creates the **top-level GitLab group** by hand and mints the **operator credential** — a group-Owner PAT with the `api` scope on GitLab.com, or an instance-admin PAT on self-managed (see the [permission matrix](#permission-matrix--the-operator-credential)); provisioning automates everything below that. The same configuration covers **gitlab.com and self-hosted instances alike** — point `integrations.gitlab.url` at your instance.

---

## Setting it up from the dashboard

Connect GitLab in Company › Integrations with the instance address, the group
and a group Owner token. The engine generates the signing secret and the
reconcile loop registers the webhook on its next tick, running the same
company-level pass `crewlet gitlab provision` runs. Each agent turned on in the
integration's Agents tab gets its own account through
[per-agent access](#per-agent-access).

The **group Owner token is sealed in the fleet's secret store and kept**. That
token creates accounts and mints tokens on them, which is a standing power, and
it is held anyway for one reason: removing an account needs the authority that
created it, so with nothing kept there was no way to disconnect the integration
and take the accounts away with it.

Rotating a seat's token and removing its account are actions on the Agents
tab, one seat at a time: a rotation seals the new token before the old one is
revoked, and removal is allowed only once the seat is off, so neither can come
from a document edit or a timer.

See [Running the provisioning pass](../reference/api-endpoints.md#running-the-provisioning-pass).

**The signing secret is generated, and its shape is not a formality.** GitLab
computes the HMAC over the *decoded* bytes of a `whsec_` value and accepts no
other form, so a secret of any other shape cannot match a delivery it will
then refuse. Nothing downstream catches one: the value goes to the secret
store and the document gets a `${VAR}`, which is the one thing config
validation cannot check the shape of, because the reference is all that layer
ever sees. Both the dashboard and `crewlet gitlab provision` mint the same
shape, and GitLab's own Generate button produces it too.

## Configuration

The top-level `integrations.gitlab` block is **non-tool config** — it enables inbound webhook handling and boot-time identity registration:

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

Each agent's role and projects are set per seat, not here: see [Per-agent access](#per-agent-access).

Four fields differ from the [hosted code host's](github.md) block beside it:

- **`url` is required** when GitLab is enabled — the instance address is needed for webhook links, boot-time identity resolution (`GET {url}/api/v4/user`), and provisioning. GitHub's is optional, because github.com serves its API from a different host and needs no address at all.
- **`signing_secret` is required** when enabled — it is the HMAC key every inbound delivery is verified against, and the route's **only** credential (see [Verification](#verification)). It must be `whsec_` followed by standard base64 over a 32-byte key, the only shape GitLab signs with; a value that is not one is refused at validation, and one arriving through an unresolved `${VAR}` stops the code host at boot rather than 503-ing every delivery in silence. A GitLab older than **19.1** cannot sign at all and therefore cannot deliver to this engine. Point it at a `${VAR}` and you don't even have to invent a value: when `crewlet gitlab provision -public-url …` runs and that var is unset, the provisioner **generates a `whsec_…` secret**, stamps it on the hook, and writes it back to the token sink — see [Provisioning](#what-a-run-does). *Unset* means held by neither the company's secret store nor the environment behind it, and the store is the fleet's whichever way the run reaches it: through the running node or the node `-api` names, or, with this node's engine stopped, directly through the node's own broker or the external one it dials. A node that holds no data, or a member of a cluster whose engine is stopped, cannot reach the store alone, so the run is refused there before it reads or mints anything, naming `-api`: it never reads a secret the fleet holds as unset and replaces it ([where a command reads the company's values](../reference/cli.md#where-a-command-reads-the-companys-values)). See [Webhooks](#webhooks).
- **`webhook_name` says which hooks on the instance are this deployment's.** The reconcile converges the hooks carrying that name whatever address they currently point at, which is what stops a change of public base leaving live orphans behind — one group hook and one per project per change, all enabled, all signed, all delivering somewhere that no longer answers. Measured on a deployment behind a restarted tunnel: a group hook and two project hooks, invisible to every pass. A hook that shares the name but points at anything other than a `/webhooks/gitlab` path was not registered by this engine and is left alone, and so is a hook with **no name**: this engine names every hook it registers, and a GitLab older than **17.1**, which drops the name, is refused — the hook it created is removed rather than left for every later pass to create again. It defaults to `crewlet`, and **two deployments watching one instance must set two names**: staging and production of one company share this document, so with a single name each pass would repoint the other's hooks and only the last to run would receive anything. Disconnecting removes the hooks by the same name, so the two halves cannot disagree about which hooks are yours.
- **`token` (optional)** enables **participants-based routing**: comments and state changes fan out to everyone participating in the issue/MR — GitLab's own notification reach — instead of only assignees and mentioned users. Webhook payloads don't carry the participants list, so this costs one `GET …/participants` REST call per comment/state-change event, made with this credential (any group member's PAT with `read_api`, which **you supply** — nothing provisions it). Without it, routing degrades to payload-derived targets — directed events are unaffected. This mirrors `integrations.jira`'s admin token, which exists for the same reason (watcher lookups). See [Event routing](#event-routing).

The `provisioning:` sub-block drives the reconcile described under [Provisioning](#provisioning), and both the CLI and the engine's own reconcile loop read it.

**`mode` says where a service account is owned.** It decides which endpoint creates an account, which one mints its tokens, and which one deletes it, and the group delete answers `404` as success ("unknown or already removed; both are the state the caller asked for"), so an instance-owned account sent down the group route would report itself deleted and stay live with every credential it holds. Per-agent access reads it from the document on every pass.

---

## Per-agent access

Every agent seat carries its own GitLab block, and the Agents tab on the
integration edits it. The block stores GitLab's own terms, never a Crewlet
word:

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
| The role | A direct membership at that `access_level` (or custom role) in each listed project, or in the group where `projects` is empty. Security Manager and Minimal Access are not offered: the first is a beta disabled by default outside GitLab.com, and the second grants an agent nothing by itself. |
| The token | One personal access token, named `crewlet-<handle>`, whose scopes follow the role: `read_api` and `read_repository` below Developer, `api` from Developer up. A role change that changes the scopes mints a new token, seals it, and only then revokes the old one. |
| Off | The account is blocked where the provisioning token is an instance administrator's; with a group Owner token (every GitLab.com deployment) its memberships are removed instead. Either way every token the engine minted on it is revoked, and the account and its history are kept. |
| Back on | Unblocked (only an account the engine blocked; one somebody else blocked waits for a person to choose Turn back on), memberships restored, a new token minted. |
| Remove | A separate, confirmed action, allowed only while the seat is off. The service account is deleted through the route that matches its owner; GitLab keeps its contributions under the Ghost User. |
| Rotate | A new token is minted and sealed, then the previous one is revoked. A token an administrator made by hand on the account is never touched. |

The token is sealed under the name the engine derives for the seat,
`GITLAB_TOKEN_<HANDLE>` (for example `GITLAB_TOKEN_SRE`), and no document
names it. The engine binds it to the seat's GitLab tool server under
`GITLAB_TOKEN` (the variable `glab mcp serve` reads) or the `Private-Token`
header (a shared HTTP server in remote-authorization mode).

Every pass reads each account back: its state, its memberships in the group
and in every project the company or a seat names, and the tokens the engine
minted on it. A role or project changed by hand at GitLab is restored to the
document's setting and reported; the Agents tab offers Keep the vendor's
setting, which writes GitLab's value into the document instead. Maintainer and
Owner are administration roles: a raise to either carries a warning and asks
for a fresh step-up.

Custom roles are read live: from the group on GitLab.com, from the instance on
a self-managed GitLab, and only on Ultimate. A custom role GitLab no longer has
is reported on the seat, never replaced by a default role.

---

## Per-role wiring

Declare the GitLab **MCP tool server** once in `mcp_servers` as a `shared: false` server. Each agent that is on for GitLab gets its own service-account token from the engine, bound to the server under the key it reads; a seat whose executor run pushes code also names the same engine-owned variable in `role.sandbox.env`, for the git-auth recipe its box runs:

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
    sandbox:
      env:
        GITLAB_TOKEN: "${GITLAB_TOKEN_AGENT_SWE}"  # the engine-owned name, for the git-auth recipe
```

`mcp_env.gitlab` no longer carries the credential: a seat's token is the engine's (see [Per-agent access](#per-agent-access)), and a credential key there (`GITLAB_TOKEN`, `GITLAB_PERSONAL_ACCESS_TOKEN`, `Private-Token`, `Authorization`) is refused. Everything else in the block (`GITLAB_HOST`, an API url) is config and is forwarded to the seat's instance as before.

The default is the official **`glab` harness stdio MCP server** (`glab mcp serve`): the engine's MCP bridge spawns one `glab` process per role with that role's `mcp_env.gitlab` env, so the per-agent PAT (`GITLAB_TOKEN`) IS the per-agent identity, no separate server to run. Boot-time identity resolution reads the token from whichever key is present (`GITLAB_TOKEN`, `GITLAB_PERSONAL_ACCESS_TOKEN`, a `Private-Token` header, or `Authorization: Bearer <pat>`), so the http alternative below works too. The engine adds the seat's own token to the role's MCP instance under `GITLAB_TOKEN` and forwards the rest of the `mcp_env.gitlab` block verbatim, and `GITLAB_TOKEN` is declared in `role.sandbox.env` by the founder exactly as `GITHUB_TOKEN` is (see [Code Sandbox](../concepts/code-sandbox.md)). A seat that is off for GitLab gets no GitLab tools.

The entry's own `env` is not tuning: it keeps what `glab` prints on its own out of the tool results an agent reads, and stops a usage report to the instance on every call. [MCP tool server](#mcp-tool-server) says what each switch stops, and why the switches go in the entry rather than in the engine host's environment.

---

## MCP tool server

The default tool server is the **official [`glab` CLI](https://gitlab.com/gitlab-org/cli)** running its built-in MCP server, `glab mcp serve` (stdio). It's declared like any stdio server and the engine's MCP bridge spawns **one `glab` process per role** with that role's `mcp_env.gitlab` env — the same shape as the `atlassian` (`uvx mcp-atlassian==0.23.1`) server — so **there is no separate MCP server to run or host**. Each process authenticates as its own service account via `GITLAB_TOKEN` (and `GITLAB_HOST` for self-managed, with `GLAB_API_PROTOCOL: http` beside it when that instance serves plain HTTP: glab takes the API's scheme from its `api_protocol` setting, `https` unless that variable says otherwise, and never from the URL in `GITLAB_HOST`). The served surface is the full `glab` command tree exposed as tools — MR approve (`glab_mr_approve`), diff (`glab_mr_diff`), notes and diff discussions (`glab_mr_note`, `glab_mr_note_create`), full issue CRUD, CI/pipelines, and `glab_api` for any raw authenticated call (including `glab_api /user` for identity) — and interactive commands are excluded, with `--output json` added automatically.

- **Requirement:** the `glab` binary must be on the `PATH` of **every engine host that runs seats**: each seat's instance is started on the node that holds that seat's lease, so a node without it cannot run the seat's GitLab tools. Install per [GitLab's harness docs](https://gitlab.com/gitlab-org/cli); Homebrew (`brew install glab`) is the officially supported cross-platform method.
- **Status:** `glab mcp serve` is flagged **experimental** by GitLab ("This feature is an experiment and is not ready for production use. It might be unstable or removed at any time."). It's official and actively developed; pin a known-good `glab` version if that risk matters for your deployment.
- **What glab prints on its own is switched off in the entry's `env`** (read against glab 1.123.0). `glab mcp serve` runs each tool as a `glab` command of its own and answers the call with everything that command wrote, its standard error included, whether it succeeded or not. So whatever glab writes there after the command lands in the agent's tool result, behind the JSON the command printed, and a result that no longer parses as JSON is answered without the parsed `data` glab otherwise puts in its structured content. glab writes two things there unasked:
  - **An update check.** Once a day, by a timestamp it keeps under the engine user's `HOME`, glab asks gitlab.com for its newest release and, when that is newer than itself, appends `A new version of glab is available` and a link to the release notes. Where gitlab.com cannot be reached it appends the failure instead (`ERROR: failed checking for glab updates: …`), and only once the connection has given up: measured with gitlab.com's traffic dropped, the call took 42 seconds, against under a tenth of a second with the check off. `GLAB_CHECK_UPDATE: "false"` turns the check off.
  - **A "What's new" banner.** The first command after every upgrade of glab, and the first in a home glab has never run in, appends `What's new in glab v<version>` and `Run: glab whatsnew`. `GLAB_CHECK_UPDATE` does not cover it; `GLAB_SHOW_WHATS_NEW: "false"` does.
- **glab reports every command to the instance it talks to**, and the entry switches that off too. After each tool call it posts a usage event to the instance's `/api/v4/usage_data/track_event` under the seat's own token — the command, the path of the project it ran against, glab's version and platform, `invocation_source: mcp`, and for `glab_api` the route template it called — and it posts one more when the server stops, waiting up to two seconds for each answer. None of it reaches the agent, but every call costs the instance one more request under the seat's token, and against an instance slow to answer, every tool call takes up to two seconds longer and so does stopping the server — two seconds being exactly how long the engine waits for a server to exit after closing its input before it signals it. `GLAB_SEND_TELEMETRY: "false"` stops the reports. Leave it out only if you want GitLab's [usage statistics](https://docs.gitlab.com/administration/settings/usage_statistics/) to count your agents' commands.

**The three switches go in the entry's `env`, and nowhere else.** None of them is a credential, so they belong in the server entry, which every seat's instance starts with, rather than in each seat's `mcp_env`. Never put them in the engine's own environment, whether a sourced `.env` file or a container image's `ENV` set them there: a stdio server is handed only [the host variables the engine passes on](../guides/tools-and-mcp.md#what-a-stdio-servers-environment-is) and what its configuration declares, and glab runs each tool in the environment it was handed, so a value set anywhere else never reaches it. Spell each one `"false"`: glab reads `GLAB_CHECK_UPDATE` and `GLAB_SEND_TELEMETRY` with Go's `strconv.ParseBool`, and a value that does not parse, such as `"no"` or `"off"`, puts `WARNING: Could not parse …` into every tool result instead.

**Alternative — one shared server:** [`@zereight/mcp-gitlab`](https://github.com/zereight/gitlab-mcp) (community, MIT; docker image `zereight050/gitlab-mcp`) in **streamable-HTTP + remote-authorization** mode (`STREAMABLE_HTTP=true`, `REMOTE_AUTHORIZATION=true`) is a single process for the whole fleet, each request authenticated by its own `Private-Token` header. Declare it as a `shared: false` `http` server (`url: http://…/mcp`) and put the PAT in `mcp_env.gitlab` as `Private-Token`. Choose this if you'd rather run one hosted server than a `glab` binary on the engine host; guardrails (`GITLAB_PERMISSION_MODE`, `GITLAB_TOOLSETS`, `GITLAB_DENIED_TOOLS_REGEX`) trim the catalogue, and `GITLAB_API_URL` targets self-managed.

GitLab's **built-in server-side MCP endpoint** (`POST /api/v4/mcp`, Free tier since 19.2) is **not used**. Its authentication is OAuth-only — dynamic client registration with interactive consent per identity, unworkable for headless per-agent identities (PAT auth is an open request, [gitlab-org/gitlab#586184](https://gitlab.com/gitlab-org/gitlab/-/issues/586184)). When PAT auth lands, adopting it is a single-server swap.

As with every MCP surface, the engine hardcodes no tool names — a bundled `mcp:gitlab` [Tool Skill](../concepts/tool-skills.md) frames the tools as read/review/track, with authoring done in the run's own checkout with its shell and git, mirroring the `mcp:github` skill.

---

## How code authoring works

Agents author code through the **code sandbox**, not through any GitLab tool. Every agent seat's executor is its harness's [own run in a box](../concepts/llm-backends.md#the-executors-run-in-a-box) (Claude Code / OpenCode) — a remote E2B VM, a container, or a box on the engine host — and opens a merge request **as the agent's own GitLab identity** (the PAT the role declares as `GITLAB_TOKEN` in `role.sandbox.env` — by convention the same PAT as its `mcp_env.gitlab` header). The run is detached — the turn suspends at launch and resumes with the run's report when it completes, so the agent reports the MR in the same turn. The full design is in [Code Sandbox](../concepts/code-sandbox.md).

The GitLab **git-auth recipe** is config you write, not engine code — the engine ships no git-auth, the same stance as GitHub. Without it a headless `git clone https://gitlab.com/...` dies with `could not read Username`: git has no way to supply the token on its own. The GitLab form of the [recipe on the sandbox page](../concepts/code-sandbox.md#the-git-auth-recipe):

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

Every security property of the GitHub form holds here and for the same reasons — the helper is scoped to the host at **both** layers (the `credential."https://…".helper` key *and* the `host=` check in the script), it stays silent with no token so public clones fall through to anonymous, the `insteadOf` rewrites use `--add` because the key is multi-valued, and the commit identity comes from the engine's generic `$CREWLET_AGENT_*` facts. And the helper is written into the box's home by a command and registered as a quoted shell snippet, as there: git is given the helper's absolute path, which only the box's shell knows (`"$HOME"`, expanded), the helper is made executable, both in commands, and the path is quoted because git runs a helper's value through the shell and a box's home may hold a space ([Local sandboxes](../concepts/code-sandbox.md#local-sandboxes)); this way the one step works in every cell, which is how `examples/nimbus.company.yaml` ships it. Two GitLab-specific wrinkles: the basic-auth **username is arbitrary** for PAT auth (GitLab ignores it — the token is the password, so `oauth2` is a conventional placeholder), and the helper must match the host **including a non-standard port** when the instance runs on one (the dev compose serves `gitlab.local:8929`), so template the host from your `integrations.gitlab.url` rather than copying `gitlab.com` blindly.

Every commit the seat makes as itself also credits Crewlet, with `Co-authored-by: Crewlet <noreply@crewlet.ai>` and the run's `Crewlet-Run:`. Git adds them through a hook the box is given, and a person's commit the seat amends or rebases is left alone; `providers.sandbox.commit_attribution: false` turns it off ([Commit attribution](../concepts/code-sandbox.md#commit-attribution)).

Once an MR exists, GitLab tools stay in the picture on the **read/review/track** side: agents read its diff, comment, approve, and follow the MR's webhooks to report back to the original requester. As with GitHub, an agent that kicks off async work whose result comes back in a later turn — a merge request waiting on its review — should capture the context with `reflect_and_persist(ttl_days=30)`, so the original ask, the project and the MR number surface in its `## Personal memory` block on the review-notification turn.

---

## Provisioning

`crewlet gitlab provision <company.yaml>` is a one-shot, **idempotent reconcile** of the company-level half of GitLab, runnable any number of times: it checks the group, warns when the provisioning token is about to expire, and registers the webhook with its signing secret. It reads the company's `${VAR}`s (the instance URL and the signing secret) and mints nothing but a signing secret. Each agent's account, role and token are [per-agent access](#per-agent-access), applied by the engine.

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
| `-public-url URL` | The engine's **public base address**, for example `https://engine.example.com`, not a webhook path: the engine derives `/webhooks/gitlab` itself. **Omit to skip webhook registration**, which the run then reports |
| `-secret-store` | Write a minted signing secret into the encrypted [secret store](../concepts/secret-store.md), where every node of the fleet reads it |
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
4. **Nothing is provisioned for `integrations.gitlab.token`.** That field holds the read-only credential [participants-based routing](#event-routing) uses, and **you supply it**: a personal access token with the `read_api` scope.

The webhook is registered whatever the seats: a company whose agents are all off GitLab still has its hook.

### Where the webhook lands

**One level, never both.** A **group** hook already fires for every `issues`/`merge_requests`/`note`/`pipeline` event in *every* project of the group and its subgroups, and GitLab is explicit that a group hook and a project hook subscribed to the same events **both** fire for an in-project event — double delivery, which the engine's completion ledger deduplicates and its inbox does not. So `provisioning.group_webhook` picks a level:

| Mode | Behaviour |
|------|-----------|
| **`auto`** (default) | Try one **group hook**; on success stop, because it covers every project including ones added later. If the instance does not serve the group hooks API, fall back to **one hook per `provisioning.projects` entry** and record a note saying so |
| **`true`** | Group hook only. **Fail** if the group hooks API is unavailable — no silent fallback, because the mode exists for an operator who needs the group-level guarantee and would otherwise find out the day a new repository went unwatched |
| **`false`** | Per-project hooks only, one per listed `projects` entry, without touching the group |

> **The group hooks API is not everywhere, and on gitlab.com Free it is worse than absent.** A free group **accepts** the registration: `POST /groups/:id/hooks` answers `201`, the hook appears in the group's settings, and its own event log stays empty for ever. Measured on a live free group, where the pass reported ready and not one delivery had ever arrived. So `auto` **reads the group's tier** rather than waiting for a refusal, and it reads it from **both** places that state it. `GET /groups/:path` omits `plan` for a free group *exactly as a self-managed instance omits it*, so asking only there made the one deployment this protects indistinguishable from the one it must not touch: the group read as paid, the fallback never ran, and the pass registered a group hook that GitLab accepted and never delivered. Measured on a live free group — `/groups/<path>` answered with no plan at all while `/namespaces/<path>` answered `plan: "free"` for the same path in the same second, and neither a comment on an issue nor a new issue fired anything.

The namespace is asked **only when the group says nothing**, so a self-managed instance answers unknown at both endpoints and keeps the group hook it serves. A tier the instance could not answer for is **not** free: the pass stops rather than concluding one, because moving a working group hook to per-project hooks over a blip — and back on the next pass — is somebody's webhooks rewritten on a timer.
>
> The refusal path still exists beside it, because the API is also absent from **Community Edition**, and GitLab **hides an unavailable endpoint as a `404`** rather than answering `402` — so "not found" is what an instance says about a feature its tier does not serve. `auto` treats a `403`/`404` from that endpoint as the tier gate. Any other refusal — a `401`, a `5xx`, a transport failure — is a real problem and aborts, because falling back on it would paper over a broken credential with a set of project hooks nobody asked for.
>
> Measured, because the obvious guess is wrong: the **unlicensed `gitlab-ee`** image this repository's `docker compose --profile gitlab` stack runs (19.3.0, no license) *does* serve `GET /groups/:id/hooks`, so the local loop takes the group path. Set `group_webhook: false` to exercise the per-project path there.

Per-project mode needs `provisioning.projects` to list something. A run with none **refuses** rather than registering nothing: an instance reporting a healthy integration that delivers to nobody is exactly the failure the skip-rather-than-guess rule exists to prevent.

The report names the level — `on the group` or `on N project(s)` — because the two are not interchangeable, and the difference only shows up the day somebody adds a repository.

**A disconnect sweeps every project in the group, not only the ones the config names.** The config is not a record of where the hooks are: a run that established per-project hooks wrote them on the projects named *at the time*, so a project dropped from `provisioning.projects` since — or a company that connected with the group alone and never named one — left them unreachable, and a teardown that visited only the current list reported success having walked past them. Measured on a live disconnect: two hooks still on a project in the group, pointing at dead `trycloudflare` tunnels from earlier runs, which nothing would ever visit again. That matters because such a hostname is re-issued to whoever asks for it next: the deliveries cannot be forged *into* the engine (they are signed with a secret the stranger does not have), and the payloads still leave your GitLab. So the teardown enumerates the group's projects — subgroups and archived projects included — and removes this engine's hooks from all of them, unioned with whatever `provisioning.projects` names, since a project there need not be in the group at all. Only hooks carrying your `webhook_name` at a `/webhooks/gitlab` path are touched; a nameless hook there is somebody else's and is left in place.

The steady-state reconcile deliberately does **not** walk the group like that. It runs every few minutes and is already O(seats × projects) over the projects you named; a group with a thousand projects would turn every tick into a thousand hook listings to converge hooks on the handful the config asks for. A disconnect happens once, when somebody presses the button, and is the one moment the whole group is worth reading. Above 2000 projects it refuses and names the hooks to remove by hand rather than grinding through them.

**The level a pass stops writing at is swept, in both directions.** The level moves on its own — `group_webhook` is a live field and the `auto` answer depends on the group's plan — and a hook left at the level nobody writes any more goes on delivering every event the new level already delivers. So a run that establishes a **group** hook removes this engine's hooks from each declared `provisioning.projects` entry, and a run that registers **per-project** hooks removes this engine's group hook. Only hooks carrying your `webhook_name` at a `/webhooks/gitlab` path are touched; anything else on the group or the project is left where it is. A project dropped from `provisioning.projects` keeps its hook, for the same reason a departed seat keeps its account: a company mid-edit looks exactly like one that removed a project.

### Token sinks

Three sinks, chosen by flag, for the one value this command can mint, the signing secret:

- **`-secret-store`**: write it into the encrypted [secret store](../concepts/secret-store.md) under the name the config references. The engine consults the store ahead of the environment, so there is no file to source and every node reads it.
- **`-env-file PATH`**: append or update a `VAR=value` line, written atomically and left `0600`.
- **`-print`**: emit an `export VAR=value` line to stdout and persist nothing.

**The store outranks an env file and stdout.** The engine resolves every `${VAR}` from the secret store before its environment, so a secret minted into `-env-file` or `-print` under a name the store already holds would never be read; such a run is refused before it reaches GitLab.

### Rotation

- **`-rotate`** replaces the [signing secret](#verification), minted into the variable `signing_secret` names, wherever the hook is registered. It is a flag rather than what a run does because the hook stops verifying against the old key the moment it runs.
- **A seat's token** is rotated from the Agents tab: the new token is sealed before the old one is revoked.

### Permission matrix — the operator credential

The provisioner's own credential is an **admin credential**. On the command line it is passed by `-admin-token` or `$GITLAB_ADMIN_TOKEN` and read from the environment only. From the dashboard it is `integrations.gitlab.provisioning.admin_token`: a `${VAR}` in the document whose value is sealed in the fleet secret store, like every other credential.

**It is the one credential here nothing rotates**, so the engine warns before it lapses. Every seat's token is minted and replaced by the pass; this one was pasted in by a person, and the day it expires every pass is refused and no agent can be provisioned or repaired. Each pass reads the token's own `expires_at` (`GET /personal_access_tokens/self`, GitLab 15.5 and later) and, inside **14 days** of it, reports a [`credential_expiring`](../concepts/integration-reconcile.md#what-a-pass-reports) finding naming `integrations.gitlab.provisioning.admin_token` and the date. The integration stays ready — the token works today — and GitLab's page reads **Credential expiring** with **Replace key** as its one action until a new token is stored. A GitLab that cannot say (an older instance, or a credential that is not an access token) leaves a note on the run and no finding.

It is **held** rather than asked for each time, and the reason is the disconnect. Removing a service account needs the authority that created it, so a credential dropped after every pass left no way to take an account away: every one this engine created outlived the integration. Disconnecting names the secret in `orphaned_secrets`, so revoking it afterwards is one command.

| Target | Required credential |
|--------|---------------------|
| **GitLab.com** (primary) | A **top-level group Owner PAT with the `api` scope** — no instance admin. Everything the provisioner touches (service accounts, their PATs, memberships, hooks) is group-Owner-callable on GitLab.com |
| **Self-managed**, `mode: group` (default) | An **instance admin PAT**, **or** a group Owner PAT with the instance setting `allow_top_level_group_owners_to_create_service_accounts` enabled |
| **Self-managed**, `mode: instance` | An **instance admin PAT**, always: a group Owner cannot create an account the instance owns. A `403` on this route says so by name, because the same status means a different remedy in each mode and "403 Forbidden" alone tells an operator nothing about which |

**Every token operation goes through the group that owns the account**, which
is what makes the GitLab.com row above true:

| Operation | Group route (a group Owner may call) | Instance route (admin only) |
|---|---|---|
| Mint | `POST /groups/:id/service_accounts/:uid/personal_access_tokens` | `POST /users/:uid/personal_access_tokens` |
| List | `GET /groups/:id/service_accounts/:uid/personal_access_tokens` | `GET /personal_access_tokens?user_id=` |
| Revoke | `DELETE …/personal_access_tokens/:token_id` under the group | `DELETE /personal_access_tokens/:token_id` |

**Both list routes are paged to exhaustion**, and that is not a performance
note. Each is read to make a *destructive* decision — retiring the tokens this
tool minted earlier, and emptying an account being decommissioned — so a
truncated read leaves a live `api`-scoped credential behind while the run
reports that it cleaned up. Unpaged, the listing returned GitLab's default
twenty rows; on an account that had accumulated 164 tokens those twenty were
the oldest and every one was already revoked, so each pass retired nothing,
minted another, and called it a successful rotation.

On GitLab.com nobody is an instance admin, so a run reaching for the right
column is refused, and the three refusals arrive at three different moments: a
mint `403`s outright, a list `401`s on the *next* pass after minting has
already succeeded, and a revoke fails inside a rollback whose message says a
live credential was left behind. Instance mode uses the right column, because
there the credential is an admin token and no group owns the account.

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

A delivery is authenticated by its **signature**, and by nothing else.


`webhook-signature` is verified as a Standard-Webhooks HMAC-SHA256 over
`{webhook-id}.{webhook-timestamp}.{body}`, keyed on the `whsec_…` secret's
base64 payload, compared in constant time against any of the header's
space-separated `v1,…` entries, with a ±5-minute timestamp tolerance in both
directions.

- **No signature, or a wrong one → `401 invalid_signature`.** There is no
  fallback. GitLab's other secret — the plaintext `X-Gitlab-Token` — is not a
  credential here.
- **A delivery before `signing_secret` is configured → `503
  no_webhook_secret`, and one whose value is not a usable `whsec_` key →
  `503 unusable_webhook_secret`**, each with a `Retry-After`, so the delivery
  is held for retry rather than discarded. The request was fine; what is
  wrong is on this side — and the two codes differ because the fixes do: one
  says to set the secret, the other that the one set is the problem.

> **GitLab signs when the hook has a signing token, not when the version is
> new enough.** These are two different secrets on the same hook and they are
> not variants of one idea:
>
> | Field | What GitLab does with it |
> |---|---|
> | **`signing_token`** | Signs every delivery, and sends `webhook-id`, `webhook-timestamp`, `webhook-signature`. Never returned by the API. |
> | `token` | Echoes it back verbatim as `X-Gitlab-Token`. GitLab's own docs call this "not recommended" and "weaker". |
>
> Given the signing key in `token`, the instance does exactly as asked — it
> never signs, and it echoes a 32-byte HMAC key back in cleartext on every
> delivery. Signing is supported from **19.1** (19.0 behind the
> `webhook_signing_token` flag).
>
> A GitLab older than 19.1 therefore cannot deliver to this engine at all.
> That is what "mandatory" means; the alternative is accepting a plaintext
> bearer token on a public endpoint.

`crewlet gitlab provision` sets the hook's `signing_token`, mints a secret
when the `${VAR}` is unset, and reads the hook back to confirm the instance
kept its name and reports `signing_token_present` — so a too-old GitLab fails
the run instead of delivering unsigned for ever.

**If the key may have leaked**, re-run with `-rotate`, which replaces the
signing secret as well as the seat tokens.

GitLab **does not auto-retry** failed webhook deliveries (and auto-disables a hook after 4 consecutive failures), so operators use the manual resend endpoint; the engine carries the delivery UUID / `Idempotency-Key` into event metadata so resends are idempotent.

### Event routing

The parser turns a payload into a **list** of per-recipient notifications (one comment can @-mention several agents; one update can add several assignees/reviewers). Each names exactly one GitLab username, which the inbound service resolves to an agent or human seat, and carries `project`, `mr_iid`/`issue_iid`, `url`, `actor_external_id` (who caused the event) and an `event_type` of `"{object_kind}.{action}"`. The MR or issue is the **conversation** — `nimbus/api!42`, `nimbus/api#42` — project-qualified because an iid is unique only within its project, and that reference is the same string the prompt prints, the coalescer partitions on and the conversation ledger files under — GitLab's two keys coincide, because an item is one object that is both the merge unit and the durable thread.

Routing mirrors GitLab's own notification semantics, in **two layers**:

- **Directed events** target exactly the named party from the payload — an assignment, a review request, a mention, a failed pipeline. These never depend on any extra lookup.
- **Thread activity** — comments and state changes — fans out to the issue/MR **participants** (author + assignees + reviewers + commenters + previously-mentioned users): exactly the set GitLab itself would notify. Participants are not in webhook payloads, so this layer needs the `integrations.gitlab.token` read credential (one `GET …/participants` call per event); without it — or when the lookup fails — routing degrades to the payload-derived assignees.

| Hook (`object_kind`) | Routed to | `event_type` |
|---|---|---|
| `issue` | On `update`: newly-added assignees (diff of `changes.assignees`) + newly-added description `@mentions` (diff of `changes.description`). On open/reopen: assignees + description mentions. On `close`: **participants** (a human closing an agent's issue must reach the agent), assignees as fallback | `issue.assigned`, `issue.mention`, `issue.close` |
| `merge_request` | On `update`: newly-added reviewers (`changes.reviewers`), assignees (`changes.assignees`), and description mentions. On `approval`/`approved`/`unapproval`/`unapproved`/`merge`/`close`: **participants**, assignees as fallback. On open: reviewers + assignees + description mentions. On reopen: same, plus a participants fan-out (the whole thread wakes) | `merge_request.review_requested`, `merge_request.assigned`, `merge_request.mention`, `merge_request.{approval,approved,unapproval,unapproved,merge,close,reopen}` |
| `note` (comment) | Every **@-mentioned** registered username (directed), then **participants** (thread activity), noteable assignees as fallback | `note.mention`, `note.comment` |
| `pipeline` | Only when `object_attributes.status == failed`: the actor who triggered it — the owner who needs to fix the build, and the one GitLab event allowed past the self-action rule | `pipeline.failed` |
| `emoji` | Parsed but **not** routed (no reliable target on award events) | — |

Each recipient gets **one** notification per event — the first, highest-signal reason wins, so a mentioned participant pings as a mention rather than as thread activity.

The event's **actor** is not filtered by the parser. It is stamped under the one metadata key every integration writes, and the [self-action rule](../concepts/agent-runtime.md) suppresses it centrally — which is what lets the rule resolve an actor across identity namespaces (an agent's bot id and its member id are one seat) and lets `pipeline.failed` state its exception once, in the prompt, instead of as a flag each parser has to remember to set.

Mentions stay explicitly extracted rather than inferred from participation, for two reasons: a mention is a *directed ask* and gets a tailored prompt (participation can't distinguish "this note pings you" from "you once commented"), and GitLab materialises new mentions into the participants list via a background job, so the lookup can race the webhook — text extraction can't. GitLab sends **raw markdown with no parsed mention array**, so the parser extracts word-boundary `@username` tokens from note bodies *and* issue/MR descriptions (on `update`, only mentions *added* by the edit count — re-saving a description doesn't re-notify, matching GitLab's own semantics).

Both mention and participant fan-out are **intersected with the registered GitLab usernames** (agents ∪ human seats) — only parties the engine can route to are targeted, so outsiders (or `@here`, `a@b.com`) never produce undeliverable notifications. Bursts on the same MR/issue collapse into one digest turn via **inbox coalescing**, exactly like GitHub PR events — the participants fan-out raises reach, and coalescing keeps the turn cost bounded.

### Notification prompts

The prompt dispatches on the **routing reason**, not the event type, because one merge-request event reaches a reviewer, an assignee and a watcher and asks each of them for something different:

| `event_type` | Prompt behaviour |
|---|---|
| `merge_request.review_requested` | The request and the MR's description, and how a review is declined here: request review from the right reviewer and remove yourself |
| `merge_request.assigned` / `issue.assigned` | The item, and how an assignment is declined here: reassign it. For an issue it also says who makes the change and under whose name: the agent itself, in its own checkout — the executor reading the prompt is its harness, running in its box with its own shell and git — opening the MR under its own identity |
| `note.mention`, `issue.mention`, `merge_request.mention` | Who named the agent and what they wrote. A mention is addressed, so the turn engine holds it to an answer and the prompt does not ask whether the agent was really asked |
| `issue.close` | **Stop** any in-flight work: an agent that keeps working a closed issue is spending budget on a deliverable nobody will take |
| `pipeline.failed` | The pipeline and who triggered it. The prompt states plainly that the agent is being told about its own action deliberately, or a seat that has learned "I am not notified of what I did" reads its own name as a routing mistake |
| Everything else (approvals, non-mention comments, merges, participant thread activity, and any reason a later release adds) | You are informed because you take part in this thread — which is a reason to be informed, not a request to act — and you act only if it asks something of you or changes work you have in hand |

The prompt states what only the engine knows — why the agent is reading this, the item by the reference a person would paste, and the product facts and decline gestures above. How to review a diff or fix a red pipeline is the model's own, and that a direct ask may not end in silence is the executor's contract, which the [turn engine enforces](../concepts/turn-engine.md#who-is-waiting-and-how-they-get-an-answer); neither is restated per event.

Review requests, assignments, and failed pipelines are **pointer events**: they name a diff, a thread or a job log to fetch before the agent has real context, so the turn-start relevance prefetches skip their aux-LLM call rather than filtering against a pointer — the executor searches for itself once it has the context. A comment is not one — its body *is* what was said.

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

- **`gitlab`** — `gitlab/gitlab-ee`, at the release `docker-compose.yml` pins, served at `http://gitlab.local:8929`. The EE image is deliberate: service accounts are a **Free-*tier*** feature that lives in **EE-*edition*** code, so the FOSS `gitlab-ce` image 404s on the `/service_accounts` API — an *unlicensed* `gitlab-ee` image runs as Free tier and serves it.

There is no MCP-server sidecar: the GitLab tool surface is `glab mcp serve`, which the engine spawns per-role (see [MCP tool server](#mcp-tool-server)).

`examples/nimbus.company.yaml` is the Nimbus example org, and it targets `gitlab.com` as shipped: an `integrations.gitlab` block, a `gitlab` MCP server per engineering role, and a sandbox those seats push from. The walkthrough below re-points a copy at this local instance, which is also most of what you would do to put a real company on a self-hosted GitLab. Its seven seats, their handles and the ownership split between them (`nimbuscore`/`nimbusk0s`, `console`/`website`, the Phase-2 framework) are already written for this. (Its smaller sibling, `examples/nimbus-claude-code.company.yaml`, has no code host at all — its engineering seats run code on the engine host with nowhere to push it, and the blocks below are exactly what it is missing.)

### Walkthrough (Nimbus against local GitLab)

1. **Resolve `gitlab.local`.** The instance's `external_url` is `http://gitlab.local:8929`, so the engine/CLI must resolve that name to the published port on localhost. Add to `/etc/hosts`:
   ```
   127.0.0.1 gitlab.local
   ```
2. **Bring up GitLab and seed it.** The `gitlab` profile brings up GitLab alone — the engine needs no other service:
   ```bash
   docker compose --profile gitlab up -d      # first boot of GitLab takes 3–6 min
   scripts/gitlab-dev-bootstrap.sh            # waits, mints a root PAT, opens the webhook SSRF allowlist, seeds nimbus-hq/nimbuscore
   ```
   The script prints the root PAT (`glpat-crewlet-dev-bootstrap`) and the UI login (`root` / `$GITLAB_ROOT_PASSWORD`). Local unlicensed `gitlab-ee` runs as **Free tier but with no identity-verification gate**, so the service-accounts API works immediately — none of the gitlab.com [identity-verification](#prerequisites-gitlabcom) friction applies locally.

   > **`curl http://localhost:8929/-/readiness` returns `404` from the host — that's expected, not a failure.** GitLab's monitoring endpoints (`/-/readiness`, `/-/liveness`, `/-/health`, `/-/metrics`) are IP-restricted to `127.0.0.0/8`/`::1/128` by default, and a host-side curl to the published port arrives with the Docker gateway's source IP, so GitLab hides them with a 404. `docker ps` showing the container `(healthy)` is the real signal (its healthcheck runs the same curl *inside* the container, where localhost is allowlisted). To check from the host, exec into the container (`docker exec <gitlab> curl -sf http://localhost:8929/-/readiness`) or hit a non-restricted route like `/users/sign_in`. The REST API (`/api/v4/…`) is not restricted, so provisioning works from the host regardless.
3. **Make a GitLab-wired copy of the config.** The example already carries every block this page documents, pointed at `gitlab.com`: `integrations.gitlab`, the `gitlab` MCP server, each GitLab seat's `mcp_env.gitlab`, the engineering seats' `GITLAB_TOKEN` in `role.sandbox.env`, and the git-auth recipe. A local run therefore re-points a copy rather than adding anything (the default repo `.gitignore` covers `*.local.company.yaml`, so a copy you later personalize with real contact IDs can't be committed by accident):
   ```bash
   cp examples/nimbus.company.yaml nimbus.local.company.yaml
   ```
   Then edit `nimbus.local.company.yaml`:
   - **`integrations.gitlab`**: set `url: http://gitlab.local:8929`. Leave `provisioning.group: nimbus-hq` as shipped, and give each GitLab seat `integrations.gitlab: {enabled: true, access_level: 40}` (Maintainer): an agent has to be able to merge its own reviewed MR, GitLab's default protected `main` lets only maintainers merge, and a Developer would stall the review→merge loop on a human. Nothing mints the `token` it names: give it a `read_api` token on the local instance, or leave it unresolved and comments reach only the people a payload names (see [Event routing](#event-routing)).
   - **the `gitlab` MCP server** — nothing to change: the `shared: false` `glab mcp serve` entry under [Per-role wiring](#per-role-wiring) names no host, and its `env` is the same against any instance.
   - **`mcp_env.gitlab`** on `Agent DevRel`, `Agent SWE`, `Agent Frontend SWE` and `Agent AI Systems Engineer` — set `GITLAB_HOST: http://gitlab.local:8929` and `GLAB_API_PROTOCOL: http` beside each seat's `${GITLAB_TOKEN_*}` placeholder. The second is not optional: glab takes the API's scheme from its `api_protocol` setting, which is `https` unless `GLAB_API_PROTOCOL` says otherwise, and not from the URL in `GITLAB_HOST`, so without it every glab tool call goes to the compose instance over HTTPS and fails with `http: server gave HTTP response to HTTPS client`.
   - **the git-auth recipe** under `providers.sandbox.setup` — point its host at the local instance: the helper's check becomes `host=gitlab.local:8929` (git sends the port), its registration `'git config --global credential."http://gitlab.local:8929".helper "!''$HOME/.local/bin/git-credential-crewlet''"'` (the key re-pointed, the quoted helper path kept as it is), and the two `insteadOf` rewrites map `git@gitlab.local:` and `ssh://git@gitlab.local:2424/` (the compose instance's SSH port) onto `http://gitlab.local:8929/`. The helper already lives in the box's home, the only place a local box writes a setup file (see [Local sandboxes](../concepts/code-sandbox.md#local-sandboxes)); see the caveat at the end of this section before moving a seat to a cell that cannot reach a laptop.
   - **every `gitlab.com` an agent reads**, in every form — in the `brief` of both engine-wide steps (`git-auth` and `nimbus-conventions`, which says every project lives under `gitlab.com/nimbus-hq`), in each engineering seat's own setup `brief` (`go`, `node` and `python` name the projects that seat owns), and in the engineering seats' `backstory`: `https://gitlab.com` becomes `http://gitlab.local:8929`, `git@gitlab.com:` becomes `git@gitlab.local:`, and every other `gitlab.com` — `gitlab.com/nimbus-hq/…`, and the git-auth brief's "authenticate to gitlab.com" — becomes `gitlab.local:8929`; and the git-auth brief's "HTTPS", which it says git pushes over and rewrites SSH remotes to, becomes "HTTP". Every step's brief is folded into the harness's `## Your environment` block, so a mention left behind sends it to gitlab.com, where the re-pointed helper offers no token, or over a scheme the instance does not serve.

   Each seat's token is the engine's, sealed as `GITLAB_TOKEN_<HANDLE>` once the seat is on; the copy names no token of its own, and `role.sandbox.env.GITLAB_TOKEN` references that engine-owned name.
4. **Register the webhook.** The root PAT is the provisioning credential; `-public-url` is the engine's base address (its **embedded API**, on whatever `api.port` your Tier A file binds, `80` in `examples/nimbus.config.yaml`, reachable from the GitLab container via `host.docker.internal`), and the run appends `/webhooks/gitlab` itself. No `GITLAB_SIGNING_SECRET` is needed: the run [generates one](#what-a-run-does) and writes it to the env file. The run loads `examples/nimbus.config.yaml`, the Tier A file step 5 runs the engine with, so export what that file reads first: the keyring (`crewlet secrets keygen -key-id 2026-01` prints the line) and this node's own API token:
   ```bash
   export CREWLET_SECRET_KEY_2026_01="<the base64 key it printed>"
   export CREWLET_API_TOKEN_FOUNDER="$(openssl rand -hex 32)"
   GITLAB_ADMIN_TOKEN=glpat-crewlet-dev-bootstrap \
     crewlet gitlab provision nimbus.local.company.yaml \
       -public-url http://host.docker.internal:80 \
       -env-file .env.gitlab -config examples/nimbus.config.yaml
   ```
   This is before that node's first start, so the run starts the node's own broker on its `stream.store_dir`, creates the fleet's store there as the engine's first boot would, and reads it, saying so on its first line. Only `nimbus-hq/nimbuscore` is seeded by the bootstrap, so the config's other projects (`nimbusk0s`, `console`, `website`) are checked, **dropped with a note, and everything else still reconciles** — create them in the UI if you want them, then re-run (the reconcile is idempotent). A re-run once step 5 has started the engine goes through it, so export one of its Tier A tokens first — `export CREWLET_API_TOKEN="$CREWLET_API_TOKEN_FOUNDER"` — and a re-run with that engine stopped again reaches the same store itself ([where a command reads the company's values](../reference/cli.md#where-a-command-reads-the-companys-values)). The compose instance serves group hooks, so the webhook lands on the group; see [Where the webhook lands](#where-the-webhook-lands) for the instances where it does not.
5. **Run the engine**, which creates each agent's account and seals its token on its first pass. `examples/nimbus.config.yaml` works as the Tier A file as-is: with a `api.port` above 0 the engine's **embedded API** both receives the GitLab webhooks and serves the dashboard, so one process is the whole stack. The port only has to match the `-public-url` you registered the hook with. (Do *not* also start a second, ingress-only node here: the two would fight over the port, and splitting ingress off is for [fleets](../guides/fleet.md) only):
   ```bash
   source .env.gitlab
   crewlet run -config examples/nimbus.config.yaml -company nimbus.local.company.yaml
   ```
6. **Drive the loop in the UI** (`http://gitlab.local:8929`): create an issue in `nimbus-hq/nimbuscore` and **assign** it to an agent's service account (their handle appears in the assignee list). The assignment webhook wakes that agent, which reads the issue via its own `glab` MCP tools and acts as itself.

The full loop this validates: **provision** (service accounts appear with the agents' handles) → **assign/mention** → webhook wakes the agent → it reads/comments as itself → (where the sandbox can reach the instance) opens an MR as itself → the reviewer-added webhook wakes the reviewer → the review lands under the reviewer's identity.

> **Sandbox code-authoring reaches a laptop's instance only from a box that resolves it to the laptop.** The example's seats run `direct`, as processes on the machine running `crewlet run`, so their boxes resolve `gitlab.local` through the same `/etc/hosts` line as the engine and push to it. A seat whose entry places its runs `run_in: container` does not, as it stands: a container has its own `/etc/hosts` and its own loopback, so the host's `127.0.0.1 gitlab.local` line neither resolves nor routes inside it. Map the name to the host in `local.run_args`, `["--add-host=gitlab.local:host-gateway"]` (the compose service publishes `8929` on every host interface), and the box reaches the instance as a `direct` one does. A seat whose entry places its runs `run_in: e2b` cannot reach it at all: a cloud E2B sandbox cannot reach your machine's `gitlab.local:8929`, so its runs' MRs need a *reachable* instance — a self-hosted E2B domain on the same network, a tunnel, or gitlab.com. Provisioning, webhooks, identity resolution, and all `glab` MCP read/review/track work fine against local compose.

---

## Limitations

- **The default MCP tool server is experimental.** `glab mcp serve` is GitLab-official but flagged experimental; pin a known-good `glab` version if that matters. The community `@zereight/mcp-gitlab` is the supported alternative for a single shared server. GitLab's built-in `/api/v4/mcp` endpoint stays unused until it gains PAT authentication (it is OAuth-only today). See [MCP tool server](#mcp-tool-server).
- **A single group webhook is not available on every tier** — Premium on gitlab.com, absent from Community Edition, and on gitlab.com Free it is accepted and silently never delivered. `group_webhook: auto` reads the tier from the group and, where the group states none, from its namespace; it uses a group hook where the tier serves one and registers **per-project** hooks otherwise; see [Where the webhook lands](#where-the-webhook-lands). Per-project hooks cover exactly the declared `provisioning.projects`, so a repository added later needs another run.
- **No composite identity.** GitLab's dual-attribution token mechanism (agent + triggering human) has no public API, so Crewlet's seats are plain service accounts. Every action is attributed to the agent that took it.
