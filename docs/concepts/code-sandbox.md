# Code Sandbox

> **v1 status.** Both backends run: `type: e2b` (a remote VM per run, on the
> vendor cloud or a self-hosted cluster) and `type: local` (`direct`, a
> process tree, or `container`, Docker or Podman), and the engine-fronted OTLP
> receiver is wired.

Crewlet agents author code through the **`run_sandbox` Execute tool**: the executor calls it with a concrete code task, the engine provisions an isolated sandbox (a real VM with a shell, a filesystem, and a git checkout), and a **coding agent** — Claude Code or OpenCode — works on the task autonomously inside it. The call is **detached**: the Execute tool-loop *suspends* when the job starts, the engine persists the in-flight conversation, and when the job completes — minutes or hours later — the **same loop resumes** with the coding agent's findings spliced in as that tool call's reply. The executor then reports and acts with its own tools (replying on the originating channel, updating the ticket) in the same turn, with full context.

This is how a Crewlet agent implements a feature, makes tests pass, reproduces a bug, or runs a one-off script — anything that needs a shell and a checkout. The sandbox is the isolation boundary: arbitrary, autonomously generated code runs *there*, never on the engine host, which is why the coding agent can run fully permissioned (Claude Code `--permission-mode bypassPermissions`; OpenCode `permission: "allow"`).

The moving parts live in `internal/sandbox/` — the provider, the manager, the coding-agent runners under `codingagent/`, the waiter, the coordinator, the pending store, and `launch.go`, which is the plumbing between the Execute loop and a detached run.

---

## How a coding task runs

For a role gated with `role.sandbox.enabled` (and an engine-level `providers.sandbox`), the [turn engine](turn-engine.md) exposes `run_sandbox` in the Execute tool surface. The planner lists it in `tools_needed` alongside the tool it will report with; the executor calls it with a `brief` — the concrete task, naming the repository and the exact change or investigation.

```
Execute loop                     Engine                         Sandbox
    │                              │                               │
    ├── run_sandbox(brief) ───────▶│ provision box, apply setup    │
    │                              │ start coding agent (detached) ├── clone, code,
    │   ToolResult(suspend=True)   │ persist pending_sandbox_run   │   test, push,
    │◀─────────────────────────────┤ publish SandboxRunStarted     │   open PR/MR …
    │  loop SUSPENDS, turn ends    │                               │
    │  (agent busy, inbox paused)  │  SandboxWaiter polls ─────────┤ keepalive +
    │                              │  … job finishes …             │ completion check
    │                              │ SandboxRunCompleted           │
    │                              │ collect result, pause box     │
    │  loop RESUMES, same turn ◀───┤ splice findings in as the     │
    ├── reply on Slack / update    │   run_sandbox call's reply    │
    │   the ticket / run_sandbox   │                               │
    │   again (reuses the box)     │                               │
    ▼  Execute finishes ──────────▶│ tear the box down             │
```

The details, each load-bearing:

- **Call → suspend.** `run_sandbox` provisions the box, starts the coding agent as a background command, persists a durable `pending_sandbox_run` row (the plan, success criteria, conversation key, trace context, and the serialized Execute conversation including the dangling tool call), and returns `ToolResult(suspend=True)`. The Execute loop leaves that call unanswered, the turn ends, and the agent transitions `WORKING → AWAITING_SANDBOX` in the turn's own `finally` — the state never passes through `IDLE`, so no queued event can slip a turn in between. Nothing is parked in memory; everything the resume needs is in the row.
- **Busy while it runs.** The agent's inbox topic is paused for the duration of the run (new events stay broker backlog; deliveries already in flight are requeued and acked, so nothing is held against a broker ack window during an hours-long job). The agent runs **one coding job at a time** — only the first suspend in a loop is honored, and a busy agent starts no new turns. It is freed only while a run is parked waiting on a human answer (see [Mid-run clarification](#mid-run-clarification-crewlet-ask)).

  That pause is a **hold this subsystem takes and this subsystem releases** — it is reason-scoped, so nothing else in the engine can drop it for you. So every exit that is finished with the box releases it from a `finally`, including the ones where cleanup failed: a store write that errors while tearing a box down is a bad minute, and a seat that is owned, attached and permanently deaf is a bad week. The one path that deliberately keeps it is a resumed executor calling `run_sandbox` again, where a new run owns the box and the seat is legitimately still busy.
- **Completion → resume.** The completion event is claimed **at-most-once** (an atomic status flip on the row), the result is collected, tokens are post-accounted, and the suspended loop is rebuilt from the row: the saved messages, the activated tools and loaded skills replayed, and the coding agent's findings appended as the `run_sandbox` call's reply — framed as "the sandbox did the code work; don't redo it; report / act now". The same `turn_id` continues, so the whole sequence renders as one turn on the dashboard. A failed resume dispatch reverts the claim so the redelivered completion can retry — the suspended loop is never lost.
- **Reuse across calls.** The box is a per-Execute-phase resource. On collect it is **paused** (snapshotted), not destroyed; if the resumed executor calls `run_sandbox` again — a follow-up fix, a clarification answer to incorporate — the tool reattaches to the same paused box and checkout (clearing the previous run's result markers, keeping the disk state) and suspends again. When the resumed Execute finishes *without* another call, the box is torn down.
- **Review sees the whole turn.** The tool-execution log from the suspended segment (the `run_sandbox` call and anything before it) is replayed to the front of the resumed loop's log, so Review's evidence check sees the code work happened. The clone/test/push commands run *inside* the box, never as engine-visible tool calls — a successful `run_sandbox` call *is* the code work.

There is deliberately no synchronous "block the turn until the sandbox finishes" mode: a turn that held its concurrency slot and its broker message for a real coding job's runtime would trip the inbox ack window and the shutdown drain. A short job in the detached model simply completes fast.

---

## Enabling it

Two switches, both required: an **engine-wide provider** and a **per-role gate**. Without the provider, no role can use the sandbox regardless of gates; without the gate, a role never sees the `run_sandbox` tool.

### Engine provider — `providers.sandbox`

A sibling of `providers.llm`, live-reloadable, with secrets kept as `${VAR}` references and resolved at construction time:

```yaml
providers:
  sandbox:
    type: e2b                     # e2b | local | fake | none — REQUIRED, no default
    api_key: "${E2B_API_KEY}"     # required for e2b — the API authenticates every call
    domain: ""                    # set → self-hosted / local cluster; empty → the cloud
    template: ""                  # empty → the prebuilt template for the coding agent;
                                  #   ALSO how a box is sized (below)
    default_coding_agent: claude-code   # claude-code | opencode
    default_timeout_seconds: 900  # box TTL / keepalive window — NOT a run cap (below)
    default_pause_ttl_seconds: 1800     # how long a blocked run's paused box is
                                        #   held before the reaper takes it
    setup: []                     # engine-wide provisioning steps (see Setup steps)
```

- **`type` is required and has no default.** Every candidate default is wrong in a way nobody sees. `local` runs the coding agent on the **engine host** — its `direct` containment as the engine's own user — which is a trade an operator makes deliberately for their own machine and must never have made for them. `none` reads as "code work is on" in the document and off in the engine. `e2b` mints billable VMs. A block with no `type` is an operator who has not decided, so `crewlet validate` asks. **Leave the block out entirely** to run with no sandbox at all.
- **`api_key`, `domain` and `template` belong to `type: e2b`** and are refused on any other backend — a `domain:` beside `type: local` reads as a cluster address and configures nothing. The key is **required** for `e2b` including against a self-hosted cluster: `domain` changes *which* API is talked to, never whether it authenticates.

- **`default_timeout_seconds` is not a run-time limit.** It is the box's TTL / keepalive window: the completion poll refreshes a running box's TTL to this value on every tick, so the clock never kills a live job. It is effectively the *orphan-reclaim grace* — how long a box outlives an engine that stopped heart-beating (a crash) before E2B reaps it. A coding job is never force-stopped on a timer.
- **`default_pause_ttl_seconds`** bounds how long a run blocked on a human answer keeps its *paused* box. E2B holds a paused sandbox indefinitely — there is no provider-side TTL — and bills for the snapshot, so expiring it is the engine's job: past this age the completion poll [reaps it](#mid-run-clarification-crewlet-ask) and the run re-seeds from the pushed branch when the answer arrives. 30 minutes trades a bounded snapshot bill against exact resume for the replies that come back quickly; raise it if your teams answer in hours. `0` means never pause — the box is torn down the moment the run blocks, for zero snapshot cost. Negative values are rejected here: an unbounded pause is the leak the knob exists to prevent. A *role* inherits this default by leaving `role.sandbox.pause_ttl_seconds` out entirely — the older spelling, `-1`, still means the same thing and is still accepted. Correctness never depends on the box: the durable state is the pushed branch plus the persisted row.
- **Sizing is `template`, not a limits knob.** There is deliberately no `default_limits`. A box's vCPU and RAM are properties of its **template**, fixed when the template is built; the sandbox-*create* API accepts no resource arguments at all, and disk is not exposed in either place. An engine-side limits field could only ever be parsed and dropped — an operator setting `memory_mb: 16384` would silently get whatever the template had. To give agents bigger boxes, build a template with the resources you want and name it in `template:`; a `container` box is sized with `local.image` and `local.run_args` instead.
- **Hot-reload:** a changed `providers.sandbox` re-instantiates the provider on the next apply; in-flight runs keep their handles, the next launch picks up the new one.

### Per-role gate — `role.sandbox`

Absent → the role never sees the sandbox option. Present with `enabled: true` → the `run_sandbox` tool appears in the role's Execute surface (and the planner is taught when to use it).

```yaml
roles:
  - name: Senior Engineer
    mcp_env:
      github: { Authorization: "Bearer ${GITHUB_TOKEN_SENIOR}" }
    sandbox:
      enabled: true
      coding_agent: ""                  # empty → inherit providers.sandbox.default_coding_agent
      # pause_ttl_seconds:              # leave out → provider default; 0 → never hold a paused box
      env:                              # env injected into this seat's runs, ${VAR}-expanded
        GITHUB_TOKEN: "${GITHUB_TOKEN_SENIOR}"   # this seat's own PAT (git-auth recipe + gh)
        NPM_TOKEN: "${NPM_TOKEN_SENIOR}"
      mcp:
        servers: [github, atlassian]    # which of the role's MCP servers the coding agent gets
      setup:                            # per-role provisioning steps, applied after the
        - name: node                    #   engine-wide providers.sandbox.setup
          commands: ["corepack enable"]
          brief: "Node 22 + pnpm are preinstalled; use pnpm for installs."
```

`role.sandbox.env` is where **external-service tokens are declared** — the engine names no tool-specific variable of its own (see [Credentials and the run environment](#credentials-and-the-run-environment)). By convention a seat points `GITHUB_TOKEN` / `GITLAB_TOKEN` at the *same* `${VAR}` as its `mcp_env` code-host credential, so PRs/MRs land under the agent's own identity — see [GitHub](../integrations/github.md) and [GitLab](../integrations/gitlab.md).

---

## Sandbox backends

The provider layer is pluggable behind the `SandboxProvider` interface (`internal/sandbox/protocol.go`). Three backends run code, and one turns it off:

- **E2B cloud** (`type: e2b`, no `domain`). Sign up at [e2b.dev](https://e2b.dev), export `E2B_API_KEY` from the dashboard. The engine talks to the documented REST API directly — there is no Go SDK to install, and no optional dependency for any of this — and uses E2B's prebuilt [`claude`](https://e2b.dev/docs/agents/claude-code) / [`opencode`](https://e2b.dev/docs/agents/opencode) templates (the coding-agent CLI preinstalled), picked automatically per coding agent when `template` is empty.
- **Self-hosted / local E2B** (`type: e2b` + `domain`). E2B's infrastructure is open source ([e2b-dev/infra](https://github.com/e2b-dev/infra)); set `domain` to your cluster's domain and the **same code path** talks to it — one field is the whole cloud↔self-hosted switch, because the control-plane address and every box's own hostname are both derived from it. The cluster still issues its own key.
- **[Local](#local-sandboxes)** (`type: local`). The engine host itself — `direct` (a process tree) or `container` (Docker/Podman). No remote account and no API key, and the coding agent can use the **subscription login** `crewlet llm login` already established. See below.
- **`fake`** — deterministic in-process stubs (`internal/sandbox/fake.go`): an in-memory filesystem, scripted coding-agent results, no network. The unit-test substrate; it does **not** run real code.
- **`none`** — code work is off. Sandbox-gated seats fall back to the native Execute loop, and `run_sandbox` never appears.

The closed set `type` is checked against is exactly the set the engine can construct — a test walks it, because a value the config accepts and the engine cannot build fails at the first coding run and nowhere earlier.

> **Two planes, and they behave differently on purpose.** The control plane (`api.<domain>`) mints, kills, pauses and re-times boxes under a bounded timeout, because minting a VM is a real allocation with a real cost when it is abandoned. The in-box plane — running commands, reading and writing files — is reached on each box's own hostname and carries **no overall deadline at all**: a coding agent's foreground command legitimately runs for many minutes, so what bounds it is the gap *between* output frames rather than the length of the call.

### Local sandboxes

`type: local` runs the coding agent on the machine running `crewlet run`. It exists for the case the [subscription LLM backends](subscription-llm-backends.md) create: you already logged a coding CLI in on that host, and you want code work to use that same login — no E2B account, no API key, no token to mint.

```yaml
providers:
  sandbox:
    type: local
    default_coding_agent: claude-code
    local:
      containment: direct         # direct | container
      state_dir: ""               # empty → $CREWLET_SANDBOX_LOCAL_HOME,
                                  #   else ~/.crewlet/sandboxes
```

The `local:` block is **required** and has no implied default, because choosing `direct` is choosing to run an autonomous coding agent as the engine user.

| | `direct` | `container` |
|---|---|---|
| Runs as | the engine user, on the host | PID inside a Docker/Podman container |
| Isolates state (per box) | yes | yes |
| Isolates the **host** | **no** | yes |
| Needs an image | no | yes (`image:`, no default) |
| System paths in setup steps | refused | work, as on E2B |
| Sizing | the host | `run_args: ["--cpus", "2", "--memory", "4g"]` |

**Both modes isolate state.** Each box gets its own directory with `HOME`, the XDG variables and `TMPDIR` pointed inside it, an **allowlisted** environment (PATH, locale, TLS trust, proxy — never the engine's own `SLACK_BOT_TOKEN` or database DSN), and an empty per-box workspace as the working directory. In `container` mode the run environment is handed to the runtime through an `--env-file` inside the box rather than as `-e KEY=value`, because a process's argv is world-readable on the host (`/proc/<pid>/cmdline`, every `ps`) and that environment carries the seat's LLM key and whatever token `role.sandbox.env` declares.

**Boxes are removed at teardown, and a crash-orphaned one is reaped on the next create — but only when it is genuinely orphaned.** Two independent questions are asked. *Is anything running in it?* — a live process group (`direct`) or an existing container, read from the operating system rather than inferred. *Has anything touched it?* — the completion poll refreshes a keepalive stamp on every tick, which is what `set_timeout` means on this backend, exactly as it refreshes an E2B box's TTL. A box is reaped only when both say no: nothing running, and nothing has touched it for a whole `default_timeout_seconds`. A run blocked on a human answer is covered by the first (its process group is stopped, not gone) and bounded by `pause_ttl_seconds`, which is the knob that owns that wait. A reaped box still gives back any login the run refreshed before it was abandoned.

**Only `container` isolates the host.** In `direct` mode the coding agent runs with its own tools enabled as the engine user, so it can read what that user can read and reach what its credentials reach. That is the normal bargain of a local mode, and it is the right one on a workstation or a dedicated VM — but it is not a security boundary, and Crewlet will not pretend otherwise. On a shared host, or anywhere the work is untrusted, use `container`:

```yaml
providers:
  sandbox:
    type: local
    local:
      containment: container
      image: ghcr.io/acme/crewlet-coding:1   # must have the coding CLI installed
      runtime: auto                          # auto | docker | podman
      network: ""                            # "none" cuts the box off entirely —
                                             #   including from its own LLM
      run_args: ["--cpus", "2", "--memory", "4g"]
```

The box directory is bind-mounted at `/home/user` — the same home E2B uses — so in-box paths, setup steps and briefs are identical across the two backends. The container is started with `--init` so a finished detached job is reaped rather than left as a zombie (which the completion probe would read as *still running*).

**The container runs as the engine's own user, and the mount is proved before the box is used.** A bind mount is one directory shared by two processes, so both of them have to be able to manage what the other writes. Under a **rootful** Docker or Podman the container's root is the host's root, and every directory the box creates in the mount comes out root-owned — the engine, which is not root, can then neither install the agent's `crewlet-ask` shim into it nor reclaim the box's disk at teardown, so box directories accumulate on the host with no error anywhere. Crewlet therefore passes `--user <uid>:<gid>` on a rootful runtime and **not** on a rootless one, where the user namespace already maps container root onto the invoking user and `--user` would map it into the subuid range instead. The runtime is asked which it is (`docker info` / `podman info`); an unanswerable probe is treated as rootful, because that mistake fails loudly at creation while the other is silent.

If your image genuinely needs its own user — one that installs packages at run time, say — put `--user 0:0` in `run_args`. Operator flags are spliced last and both runtimes take the final occurrence of a repeated flag, so yours wins. Be aware you are taking back the problem above.

Every container box is then proved once, at creation, before anything runs in it: the engine writes a token the box reads back, and the box creates a directory the engine writes into and removes. That catches the ownership case and one other that is otherwise invisible — a `runtime` pointed at a daemon on **another host** (`DOCKER_HOST`, a forwarded socket) bind-mounts *that* machine's filesystem, so the agent finds no brief, no credentials and no shim. A box that fails the proof is torn down and the error names the cause; use the `e2b` provider for a genuinely remote runtime.

**Setup commands get a provisioning budget, not a control one.** Each command may run for `timeout_seconds` (default 600). Real provisioning — a dependency install, a cold image pull, a large clone — takes minutes, and without its own budget these inherited the backend's control-plane timeout and were killed, failing the whole acquisition. Raise it for a step you know is slow; lower it for one that should be instant, so a hung command surfaces as a named setup failure rather than eating the turn. A failure reports the step and the command's position, never the command text: `${VAR}` references in commands are resolved before they run, so the text can carry the credential it was given.

**Setup steps and containment.** `direct` mode has no filesystem virtualisation, so a setup step that writes a *system* path is refused with an error naming the mode — it would otherwise write to the engine host's real `/usr/local/bin`. The shipped [git-auth recipe](#the-git-auth-recipe) is one of these; under `direct`, root it in the box's home instead:

File paths in a setup step are absolute and are **not** shell-expanded, so `$HOME` does not work there — write the helper with a `commands` heredoc, which does run in a shell:

```yaml
setup:
  - name: git-auth-local
    commands:
      - mkdir -p "$HOME/.local/bin"
      - |
        cat > "$HOME/.local/bin/git-credential-crewlet" <<'SH'
        #!/bin/sh
        ...
        SH
      - chmod +x "$HOME/.local/bin/git-credential-crewlet"
      - 'git config --global credential."https://gitlab.com".helper "$HOME/.local/bin/git-credential-crewlet"'
```

`container` mode needs no such change.

**Credentials.** When the role's resolved sandbox LLM provider is a [`cli-agent`](subscription-llm-backends.md) entry, the local backend seeds that provider's credential files into the box before the run and writes a **refreshed** one back afterwards — OAuth access tokens expire in hours and most vendors rotate the refresh token with them, so discarding the rewritten file would log the fleet out at the next expiry. A credential the operator has since removed with `crewlet llm logout` is never re-created from a box.

**Pause / resume works.** A run blocked on a human answer is genuinely suspended: `direct` SIGSTOPs the job's process group, `container` issues `docker pause`. Both hold memory, which is exactly why the same `default_pause_ttl_seconds` reaper bounds them as it bounds a billed E2B snapshot.

**What local mode does not give you:** template-based sizing (use `run_args`, or the host), provider-enforced network egress policy (use `network:` plus your own firewall), and a fresh machine per run — a `direct` box shares the host's installed toolchain, which is usually the point.

---

## Coding agents

Two runners ship behind the `CodingAgentRunner` protocol, selected by `role.sandbox.coding_agent` (falling back to `providers.sandbox.default_coding_agent`). Both are handed the same brief and return the same structured result; the engine treats them interchangeably.

### Claude Code

Runs headless — `claude -p "<brief>" --output-format json --permission-mode bypassPermissions` — and its JSON output maps directly onto the result (final text, session id, token usage, cost).

Claude Code **speaks the Anthropic API only**, so it needs an Anthropic-compatible credential. The sandbox's LLM derives from the role's provider chain: `role.llm_sandbox` → `llm_execute` → `llm` → `default`. Point `role.llm_sandbox` at an `anthropic` `providers.llm` entry (a custom `base_url` on that entry becomes `ANTHROPIC_BASE_URL` — an Anthropic-API gateway works). A role whose resolved provider is *not* Anthropic-compatible cannot launch Claude Code — the launch fails with `SandboxCredentialError` (see [Failure modes](#failure-modes)) unless `ANTHROPIC_API_KEY` (or a Bedrock/Vertex/Foundry toggle) is supplied in `role.sandbox.env`.

**A subscription counts.** When the resolved provider is a [`cli-agent`](subscription-llm-backends.md) entry, the headless subscription token (`CLAUDE_CODE_OAUTH_TOKEN`, minted by `crewlet llm login <key> -capture-token`) is exported into the box and Claude Code there bills your Pro/Max plan — no API key anywhere. This works on **every** backend including remote E2B, because a token is one scoped, revocable variable. The credential *files* deliberately never leave the engine host: they carry a refresh token whose rotation is shared fleet state. A CLI that mints no headless token (Codex, Gemini CLI) therefore needs [`type: local`](#local-sandboxes), where the coding agent reads the login directly.

### OpenCode

Runs `opencode run "<brief>" --format json` and is **provider-agnostic**: it works with any OpenAI-compatible endpoint, so it reuses the LLM provider the role already has — no extra secret. An org whose only provider is OpenAI-compatible should keep `default_coding_agent: opencode` rather than pay for a parallel Anthropic credential.

The runner **writes the provider configuration into the sandbox itself**: when the role's provider pins a custom `base_url`, it declares a custom provider (`crewlet`) in `opencode.json` with an explicit `baseURL` and the exact model, addressed as `crewlet/<model>` — bypassing OpenCode's Models.dev catalog and the vendor default endpoint, both of which would otherwise break a custom gateway (`ProviderModelNotFoundError`, or silently hitting `api.openai.com`). The API key is referenced via OpenCode's `{env:VAR}` interpolation, so the secret rides the sandbox env and is never written into the config payload. The same `opencode.json` sets `permission: "allow"` (headless runs have no human to approve a prompt — a gate left at `ask` is auto-rejected, which notably kills runs whose checkout lives outside OpenCode's cwd), `share: disabled`, and `autoupdate: false`.

OpenCode exposes no stable token/cost envelope, so those fields stay zero on its runs — honest accounting over fabricated numbers.

### Runs are uncapped; the transcript is published

A detached coding job is **never force-stopped on a wall-clock timer**. The waiter refreshes the box's TTL every poll tick (keepalive), so a legitimately long run is free to finish; the TTL only reclaims a box the engine has stopped heart-beating for. Completion is detected by tracking the job itself — see [Durability and completion](#durability-completion-and-observability).

Because the brief instructs the coding agent that it is *not* the last step, it writes a structured **findings report** to a known file before stopping; `collect` prefers that file for the result text (it survives an agent that finishes but never exits, and a tool-only run that streamed no final message). The runner also reconstructs an **activity transcript** — for OpenCode from its line-flushed `--format json` event stream (`[tool] bash: <command>`, errors flagged), for Claude Code from captured stderr — and the engine publishes it, tail-capped and **secret-redacted**, as the run's Execute phase event. The dashboard renders the findings as markdown with the transcript as a collapsed step list, so an operator reads exactly what the sandbox did. A sandbox Execute therefore shows as three phase rows under one turn: the kickoff (the executor's reasoning + the launch), the coding-agent output, and the resume (the executor reporting/acting on it).

---

## Setup steps — provisioning the box

A coding agent's box needs environment wiring beyond the CLI itself: git auth, registry credentials, toolchains. Provisioning is a declarative **setup-step** framework (`internal/sandbox/setup.go`); each `SandboxSetupStep` contributes:

- `files` — content written into the box (helper scripts, config files);
- `commands` — shell commands run after the files land; a non-zero exit **fails the acquisition** and tears the box down, so a half-provisioned box never receives a brief promising an environment it doesn't have;
- `env` — variables merged into the coding agent's run env (`${VAR}`-resolved with the rest);
- `brief` — a paragraph folded into the coding agent's "## Your environment" block, *telling* it what the step made true (an agent that wastes rounds rediscovering how to authenticate is slower than one that knows).

Steps come **entirely from company config** — the engine ships none of its own, git auth included — applied in order: `providers.sandbox.setup` (engine-wide, every sandbox role) then `role.sandbox.setup` (per-role extras). Setup commands execute **with the run env**, so a recipe can read the engine's per-launch identity facts (`$CREWLET_AGENT_HANDLE`, `$CREWLET_AGENT_EMAIL`) and its own configured tokens at provisioning time. A reused box skips re-apply — its provisioning survives with its disk state.

### The git-auth recipe

Injecting a code-host token is necessary but not sufficient: a headless `git clone https://github.com/...` has no way to *supply* it and dies with `could not read Username`. The recommended wiring is a config recipe — a credential helper plus rewrites, shipped as an ordinary engine-wide step:

```yaml
providers:
  sandbox:
    setup:
      - name: git-auth
        files:
          /usr/local/bin/git-credential-crewlet: |
            #!/bin/sh
            [ "$1" = "get" ] || exit 0
            [ -n "$GITHUB_TOKEN" ] || exit 0
            ok=""
            while IFS= read -r line; do
                [ -z "$line" ] && break
                [ "$line" = "host=github.com" ] && ok=1
            done
            [ -n "$ok" ] || exit 0
            echo "username=x-access-token"
            echo "password=$GITHUB_TOKEN"
        commands:
          - chmod +x /usr/local/bin/git-credential-crewlet
          - 'git config --global credential."https://github.com".helper /usr/local/bin/git-credential-crewlet'
          - 'git config --global --add url."https://github.com/".insteadOf "git@github.com:"'
          - 'git config --global --add url."https://github.com/".insteadOf "ssh://git@github.com/"'
          - 'git config --global user.name "$CREWLET_AGENT_HANDLE"'
          - 'git config --global user.email "$CREWLET_AGENT_EMAIL"'
        env:
          GIT_TERMINAL_PROMPT: "0"
        brief: >-
          Use $GITHUB_TOKEN (already in your environment) for all GitHub work:
          git authenticates to github.com with it automatically — clone/fetch/push
          over plain HTTPS; SSH remotes are rewritten; never embed the token in a
          URL. Your git commit identity is preconfigured.
```

Each piece is load-bearing: the helper is **scoped to the code host twice** (the credential config key *and* the `host=` check in the script), so the PAT can never be offered to a foreign host such as a malicious submodule URL; it reads the token from the env at git-runtime (never persisted to disk) and stays silent without one, so public clones fall through to anonymous; the `insteadOf` rewrites use `--add` because the key is multi-valued (without it the second value replaces the first and scp-style `git@…:` remotes stay SSH, failing on host-key verification in a keyless box); and commit identity comes from the engine's generic `$CREWLET_AGENT_*` facts, so commits attribute to the seat without the recipe hardcoding a name.

The GitLab form of this recipe — same shape, `gitlab.com` scoping, `oauth2` username, MRs opened via `git push -o merge_request.create` push options — ships in [`examples/nimbus.company.yaml`](../../examples/nimbus.company.yaml); the GitHub form is documented in [GitHub Integration](../integrations/github.md). The repo to work in is **not** role config — it is task context the executor names in the brief, and the coding agent clones it inside the box with the seat's injected token.

---

## Credentials and the run environment

The run env injected into each job is assembled in `internal/sandbox/launch.go`. The engine contributes only **tool-agnostic** facts:

- **LLM credentials**, derived from the role's resolved `providers.llm` entry (the chain above): `ANTHROPIC_API_KEY` / `ANTHROPIC_BASE_URL` for an Anthropic provider, `OPENAI_API_KEY` / `OPENAI_BASE_URL` otherwise (for OpenCode the real endpoint redirect is the custom provider written into `opencode.json`; the env forms are kept for parity).
- **Agent identity** — `CREWLET_AGENT_HANDLE` and `CREWLET_AGENT_EMAIL`, the per-launch facts static config cannot know, which setup recipes map into tool shape (git commit identity above).
- **A subscription token**, when the resolved provider is a `cli-agent` entry: the profile's own variable (`CLAUDE_CODE_OAUTH_TOKEN`) rather than an API key. Its credential *files* travel only to a [local](#local-sandboxes) box, never to a remote one.

Everything tool-specific comes from config: external-service tokens (`GITHUB_TOKEN`, `GITLAB_TOKEN`, registry tokens, a test `DATABASE_URL`, …) are declared in `role.sandbox.env` or a setup step's `env` and merely `${VAR}`-resolved — the engine never extracts, names, or special-cases them. Precedence, later wins: derived LLM creds → agent identity → setup-step env → `role.sandbox.env` → non-secret OTel values.

Any `${VAR}` reference whose variable is unset or empty — whole (`"${TOKEN}"`) or embedded (`"Bearer ${TOKEN}"`) — logs a **`sandbox_env_unresolved`** warning naming the keys (never the values), so a seat whose token isn't exported fails loudly instead of mysteriously. Resolved values are injected only into that sandbox's process env and die with it; the DB stores only the `${VAR}` references. Shared infrastructure secrets — above all the OTLP backend's ingest token — never enter the sandbox at all (see below).

---

## MCP inside the sandbox

The coding agent is itself an MCP client, so the engine renders the role's servers into the box (`internal/sandbox/mcprender.go` → Claude Code's `.mcp.json` via `--mcp-config --strict-mcp-config`, or OpenCode's `opencode.json`). Scoping is at the **server level only**: `role.sandbox.mcp.servers` names which of the role's `mcp_servers` to expose, and the coding agent gets **every tool those servers expose**. There is no per-tool allowlist — OpenCode has no allowlist flag and Claude Code runs `bypassPermissions` headless, so a curated list couldn't be enforced uniformly and would give a false sense of restriction. A role that shouldn't reach a surface simply doesn't list that server.

Credentials from `role.mcp_env` are resolved into the rendered specs (HTTP servers get them as headers, stdio servers as env), so the in-box server instances authenticate as the seat. The connected server names are also listed in the coding agent's environment brief.

Two practical caveats:

- **A cloud sandbox cannot reach your laptop.** An HTTP MCP server (or a git host) on `localhost` / a private LAN address is unreachable from an E2B cloud box. For laptop-local development stacks, use a self-hosted E2B `domain` on the same network, a tunnel, or scope those servers out of the sandbox.
- **stdio servers must exist in the box.** A stdio spec is *spawned inside* the sandbox, so its command must be present in the template (E2B's prebuilt templates cover common cases; anything else belongs in a custom template or a setup step).

---

## Mid-run clarification (`crewlet-ask`)

Once it is in the code, the coding agent may hit something only a person can answer — an ambiguous spec, a design decision above its remit. Headless coding agents can't pause to ask interactively, so every box gets a shim, **`crewlet-ask`** (`internal/sandbox/codingagent/ask.go`), and the brief instructs: *don't guess — commit and push your WIP branch, run `crewlet-ask "<question>" --to <audience>`, and stop.* The audience is `requester`, `team`, `manager`, or a named teammate — the brief carries the unit roster and lead so the agent can name a real person.

The shim is **signal-only**: it records the question to a file and never posts anything itself. When the run completes with a pending question:

1. The engine (never the sandbox) posts the question on its normal, audited per-role surface — the originating conversation — via a short dispatched turn, keeping identity attribution and capability guards on the engine.
2. The run is parked (`awaiting_clarification`) with the conversation key; the suspended Execute state is preserved untouched; the box is left **paused** so the checkout survives. Unlike a running job, **the agent goes free** — a human reply can take days, so its inbox flows and it can do other work.
3. The next inbound message on that conversation is treated as the answer. The coordinator claims the parked run (at-most-once) and **resumes the same suspended Execute loop**, splicing in the question, the answer, and the WIP branch as the `run_sandbox` call's reply, with instructions to call `run_sandbox` again incorporating the answer — which reattaches to the paused box and checkout. No fresh Plan, no re-investigation.

The durable state is always **git plus the persisted row** — the WIP branch is pushed *before* the agent asks, so the paused box is a fast-resume optimization, never the store. Clarification rounds chain within the same turn, bounded by the turn's existing iteration cap and the budget cascade.

**The pause reaper.** A human reply can take days, and E2B holds a paused box — billing for the snapshot — until something kills it. Nothing else would, so the completion poll enforces `pause_ttl_seconds` on the same tick it uses for running jobs. The box's age is durable (`pending_sandbox_run.paused_at`, stamped when the box is paused), so the deadline survives an engine restart instead of resetting with the process. Past the TTL, a parked run gets:

1. **the box killed by id** — `sandbox.Provider.Kill`, never `connect()`, which auto-resumes a paused sandbox and would boot the VM back up purely to shut it down;
2. **the box released on the row** (`sandbox_id` cleared, `paused_at` cleared) — which is also what makes the next `run_sandbox` provision a fresh box rather than reattach to a dead id;
3. **the run flipped to `reseed`** — still claimable, still matched by conversation key, because the run is not over. The answer can arrive days later and still resumes the same suspended Execute loop; only the spliced reply differs, telling the executor the machine is gone and the brief must re-check-out the pushed branch.

`pause_ttl_seconds: 0` takes the same path with no snapshot at all: the box is torn down the moment the run blocks and the run parks straight into `reseed`.

The reaper is scoped to **clarification waits** on purpose. The lifecycle pauses a box in one other place — between collecting a completion and resuming the Execute loop — but that pause lasts a single dispatch and is settled by the tail that made it, so expiring it would only break in-turn reuse. The one paused box no tail will ever settle is a run whose engine died mid-resume; that one is reaped by the coordinator's recovery pass at boot, the single moment the row is unambiguously abandoned.

---

## Durability, completion, and observability

**Pending runs survive restarts.** Every detached run is a `pending_sandbox_run` row in the node's store (in-memory only for single-process tests). On boot the coordinator's recovery pass re-pauses the inboxes of agents with running jobs and re-enters their busy state; the waiter then drives those jobs to completion. Recovery also reaps a run left in `resumed` — that state can only mean the previous engine died between claiming a completion and settling it, so nothing will ever pick it up again (the at-most-once claim already flipped) and its paused box would leak. A restart never orphans a job, because E2B boxes run server-side, independent of the engine, and any process can reconnect by `sandbox_id`.

**The completion poll is THE completion signal.** The `SandboxWaiter` reconnects to each running box every 15 s and asks the runner whether the job finished. There is deliberately no push callback from inside the sandbox: only a poll can see a job that died before its last step, and the tick has to run anyway because it **doubles as the box keepalive** (the TTL refresh that makes runs uncapped). Three signals cover every way a job can end:

1. **The done-marker** — the wrapper shell wrote the exit code after the agent process returned (clean finish or crash; the marker is non-empty by construction so it can't be confused with "not yet").
2. **A terminal event in the streamed output** — `opencode run` is known to finish its work yet never exit ([opencode#17516](https://github.com/anomalyco/opencode/issues/17516)); its `--format json` events are flushed per line, so the terminal `step_finish` (reason `stop`) / `session.status: idle` / `error` event lands before the hang and the poll reads it. Teardown then reaps the husk.
3. **Process liveness** — a `kill -0` probe on the detached wrapper PID. A dead wrapper with no marker means the whole process group was killed (SIGKILL / OOM); the run completes and `collect` surfaces the partial output and exit code instead of stalling forever.

**A vanished sandbox can't orphan a run.** If reconnecting fails on several consecutive ticks (~1 min at defaults), the waiter declares the box gone and fires completion anyway — E2B reclaimed it, a network partition, or the engine was down past the keepalive grace. The coordinator then frees the agent, marks the run failed, and the resumed executor reports the failure rather than polling a dead box forever.

**Per-run OTel, without handing the sandbox a secret.** The coding agent's telemetry exports to an **engine-fronted OTLP receiver**, enabled by setting `CREWLET_SANDBOX_OTEL_RECEIVER_URL` to the engine API base the sandbox can reach. Each run gets a **per-run, trace-scoped, expiring token embedded in the endpoint path** (`POST /otlp/{token}/v1/{signal}`), so `OTEL_EXPORTER_OTLP_HEADERS` — the backend's ingest credential — never enters the box; the receiver validates the token and forwards upstream, adding the real auth outside the sandbox. That variable is set **empty** in the box rather than left unset, because an exporter reads it from the ambient environment otherwise and a box inherits whatever the engine host exported.

The token is **signed and self-describing** rather than a key into a store, because minting and verifying happen in different processes whenever the API runs on its own host: the engine mints when it launches a run, the API verifies when the box exports. Both derive the signing key from the Tier A keyring, so a split deployment agrees without sharing state — and with no keyring configured the key is per-process, which works for a single process and is warned about loudly for two. Expiry rides in the token, so nothing is reaped and a restart does not invalidate a live run's endpoint.

The receiver **accepts and drops** when no upstream is configured: the engine's own per-turn span still carries the trace, and a deployment with no backend should not have every export fail. A failing backend is never reported to the box either — an exporter that gets a 5xx retries, and retries against a backend that is down turn one outage into two. The run env also carries non-secret resource attributes (`crewlet.turn_id`, `crewlet.agent_handle`) and a `TRACEPARENT`, so Claude Code's spans/metrics nest under the turn (its `CLAUDE_CODE_*` telemetry toggles are injected only for that runner). OpenCode emits no OTLP today — its observability is the published transcript plus the engine-side lifecycle events.

**Dashboard.** The live-state projection maintains an active-sandboxes set from the `SandboxRunStarted` / `SandboxClarificationRequested` / `SandboxRunCompleted` lifecycle events, and the dashboard overview shows a **Running sandboxes** panel whenever a job is in flight — agent, coding agent, task, elapsed, status (running / awaiting input). Completed runs render as the three-row Execute group described above, with the findings and transcript.

---

## Budgets

The coding agent calls the LLM itself, from inside the sandbox, so the engine cannot gate it mid-stream like the native loop. The model is **cap + post-account**:

- **Pre-flight floor.** A launch is refused when the agent's remaining token budget is below `turn_engine.sandbox_min_budget_tokens` (default 2000); the `run_sandbox` call returns a normal failure the executor can act on, without suspending.
- **Self-limits.** A fraction of the remaining budget (`turn_engine.sandbox_budget_fraction`, default 0.5) is translated into the coding agent's own caps — Claude Code's `--max-turns` — so the run self-limits.
- **Post-accounting.** On collect, reported usage is charged to the budget cascade and the `token_usage` ledger, and the phase event carries the real usage and cost so the Tokens view rolls sandbox spend up alongside native phases. Mid-run enforcement is best-effort by design — the price of the capability; OpenCode's unreported usage stays at zero rather than being invented.

---

## Security model

- **The sandbox is the isolation boundary.** Arbitrary, autonomously generated code runs *there*, never on the engine host. That is the whole reason running the coding agent with permissions bypassed is acceptable: the blast radius is one ephemeral, provider-isolated VM that is torn down at phase end. Human gating, where wanted, happens at the merge request — not mid-run.
- **`local` + `containment: direct` moves that boundary, deliberately.** There the coding agent runs as the engine user: per-box state isolation and an allowlisted environment still hold, but the host does not. Pick it for a workstation or a dedicated VM you are willing to hand to an autonomous agent; pick `containment: container` (or E2B) anywhere else. This is the one configuration in Crewlet where the sandbox is not a security boundary, and it is opt-in twice over — `type: local` plus an explicit `containment`.
- **Only the seat's own identity enters the box.** The injected env carries credentials the agent legitimately acts *as* — its LLM key and the external-service tokens its config declares (`role.sandbox.env`). Leaking those from inside the box exposes only what the agent already is. Because the code-host token is the seat's **own** PAT, an MR/PR the coding agent opens is attributed to the agent, not to a shared bot identity.
- **Shared infrastructure secrets never enter the box.** The OTLP backend's ingest token is the canonical example: the sandbox sees only an engine-fronted, path-token-scoped OTLP endpoint (`CREWLET_SANDBOX_OTEL_RECEIVER_URL`); the engine attaches the real backend credential upstream. Apply the same rule to any credential you add — if the agent doesn't act *as* it, don't inject it.
- **Injected values are ephemeral.** `${VAR}` references are resolved at acquire time into that sandbox's process env only; the database stores references, never values, and the values die with the box.
- **Sub-agents cannot launch sandboxes.** `run_sandbox` is on the sub-agent control denylist, alongside `spawn_subagent` and the discovery meta-tools — denied by name whether the parent grants it explicitly or the sub-agent finds it through discovery. The `sandbox_enabled` check does not cover this on its own: it is a per-**role** gate resolved once per turn and shared by every phase, so on a sandbox-enabled role the tool is available during the sub-agent phase too. The reason it must be denied is ownership rather than capability: a detached run is keyed to the **parent** turn (the pending row carries the parent's `turn_id`, and the launch pauses the parent seat's inbox), and a sub-agent cannot suspend to wait for it — so the parent turn would finish without persisting the suspended conversation, leaving the seat deaf for the whole coding run with nothing to resume into.
- **Network egress is the provider's policy.** The engine does not enforce egress rules; use your E2B template's network policy for that, and prefer an allowlist (the LLM endpoint, the git host, the engine's OTLP receiver) in templates for sandbox-enabled production roles.
- **Budgets still bound the run** (previous section), and the collected artifact passes through the same result validators as a native Execute before reaching Review.

---

## Failure modes

What an operator should recognize:

- **`SandboxCredentialError` at launch** — the role chose `claude-code` but its resolved LLM provider isn't Anthropic-compatible. The run never starts; the error says exactly what to do: point `role.llm_sandbox` at an `anthropic` `providers.llm` entry, put `ANTHROPIC_API_KEY` in `role.sandbox.env`, or switch the role (or the provider default) to `opencode`.
  For a [`cli-agent` provider](subscription-llm-backends.md) the error is specific to the two cases: a CLI that mints a headless token says "run `crewlet llm login <key> -capture-token`", and one that does not says to use `providers.sandbox.type: local` instead.
- **`LocalSandboxError: refuses to touch …`** — a setup step tried to write a system path under `containment: direct`, which has no filesystem virtualisation. Root the file under the box's `$HOME`, or switch to `containment: container`.
- **`LocalSandboxError: neither docker nor podman`** — `containment: container` with no container runtime on the engine host's PATH. Install one, name it in `runtime:`, or use `direct`.
- **`cannot see its own box directory`** — the runtime is driving a daemon on another host, so the bind mount is that machine's filesystem rather than this one's. Point `runtime:` at a local daemon, or use `type: e2b` for a remote runtime.
- **`writes its box directory as a user this engine cannot manage`** — the container is running as someone the engine cannot follow into the mount, which happens when `run_args` overrides the `--user` the backend chose. Drop that override, or run the engine as the same user the container does.
- **`sandbox_env_unresolved` warnings** — a declared `${VAR}` resolved empty; the run proceeds but the coding agent will hit auth failures (a clone dying anonymously, a 401 from a registry). Export the variable the config references.
- **Setup-step failure** (`sandbox_setup_failed`) — a provisioning command exited non-zero; the box is torn down before any brief runs. This is an operator config problem (a broken recipe), logged distinctly from a coding-agent install failure.
- **Sandbox lost mid-run** — the waiter's gone-detection above. The run is marked failed and the executor reports what happened; look for `sandbox_connect_failed` streaks preceding it. If the engine was down longer than `default_timeout_seconds`, E2B reaped the box as an orphan — that TTL is the knob to raise for deployments with long maintenance windows.
- **Collect failure** — the box was reachable but the result couldn't be read; the run is failed, the box torn down, the agent freed. The coding agent's exit code and stderr are surfaced where available so the failure is diagnosable, not a silent stall.
- **A run that "finishes" with no report** — the coding agent neither wrote its findings file nor streamed a final message. `collect` surfaces stderr and the exit code as the error; the resumed executor sees a failed run rather than an empty success.

---

## Testing

`providers.sandbox.type: fake` wires the in-process fakes (`FakeSandboxProvider`, `FakeCodingAgentRunner`) — scripted results, an in-memory filesystem, no network, per the project rule that tests never touch real services. The runner tests pin the exact CLI invocations (`claude -p … --output-format json`, `opencode run … --format json`) and output parsers, and `internal/config/examples_test.go` holds the shipped git-auth recipe to its security properties (host scoping at both layers, `--add` rewrites, identity from the engine's agent facts) so an edit cannot quietly widen them.
