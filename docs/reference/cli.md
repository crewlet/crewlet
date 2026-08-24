# CLI Reference

Crewlet ships a `crewlet` command (also available as `python -m crewlet`).

---

## Commands

| Command | Description |
|---------|-------------|
| `crewlet run [config]` | Read Tier A bootstrap (default `./config.yaml`), connect to DB, run engine; falls into unconfigured state if no active revision |
| `crewlet validate <config.yaml>` | Validate a Tier A or Tier B YAML and print a summary (`--json` for machine-readable errors) |
| `crewlet migrate [config]` | Apply pending schema migrations (Tier A file, default `./config.yaml`). Every process migrates on open, so this is a way to do it *without* starting one — `-check` reports pending work and exits non-zero without applying it |
| `crewlet budgets show [config]` | Print token usage per scope (`org`, `agent:<id>`) — nothing rather than zeros when no scope has spent anything |
| `crewlet budgets reset [config]` | Zero token usage — durable across restarts, so resetting is deliberate. `-scope` limits it to one scope, and the report names what it cleared |
| `crewlet schema [company\|bootstrap]` | Print the JSON Schema for a config tier (editor autocomplete, CI, [AI-assisted authoring](../getting-started/ai-authoring.md)) |
| `crewlet config import <company.yaml>` | Load Tier B YAML, activate as a new `company_config` revision |
| `crewlet config export [--revision <UUID>]` | Dump the active (or specified) revision as YAML to stdout |
| `crewlet config show` | One-line summary of the active revision |
| `crewlet config revisions [--limit N]` | List recent revisions (newest first) |
| `crewlet config diff <UUID> [-against <UUID\|active>]` | Line diff of two revisions, always redacted on both sides |
| `crewlet config activate <UUID>` | Re-point the fleet at a revision; re-activating the current one mints a new epoch, which is how a rotated secret takes effect |
| `crewlet config seal` | Encrypt the active revision as one document under the Tier A keyring (one-time migration off plaintext-at-rest) — see [Secrets](../concepts/configuration.md#secrets) |
| `crewlet config rekey [--dry-run]` | Re-encrypt the active revision's config document under the active key (master-key rotation) |
| `crewlet secrets keygen [-key-id ID]` | Generate a fresh encryption-keyring key + the `config.yaml` snippet to install it |
| `crewlet secrets set <NAME>` | Store an encrypted secret in the [secret store](../concepts/secret-store.md); the engine resolves `${NAME}` from it ahead of the environment |
| `crewlet secrets list` | List stored secret names + metadata (never values) |
| `crewlet secrets unset <NAME>` | Remove a stored secret |
| `crewlet secrets get <NAME> -reveal` | Print one stored value to stdout — break-glass, audited, CLI-only |
| `crewlet secrets rekey [-dry-run]` | Re-encrypt stored secrets under the active keyring key |
| `crewlet plane import <company.yaml> <directory>` | Publish local [Tool Skill](../concepts/tool-skills.md) + [knowledge-doc](../concepts/knowledge-system.md#publishing-knowledge-docs) markdown into [Plane](../integrations/plane.md) — `trigger:` ⇒ skill in the Tool Skills project, otherwise ⇒ doc in its parent-directory project. Idempotent by `external_id`; `-prune` removes orphaned skill pages. |
| `crewlet plane resync <company.yaml>` | Re-run the engine's own skills walk against a throwaway registry and print what loads — a read-only diagnostic, not a way to change a running engine |
| `crewlet plane provision <company.yaml>` | Reconcile the config into [Plane](../integrations/plane.md): one service account per agent seat, project memberships, per-agent API tokens (minted from the config's `${VAR}` references), the `crewlet-engine` read account, and the workspace webhook (secret captured) — idempotent, with rotation and decommission paths |
| `crewlet gitlab provision <company.yaml>` | Reconcile the config into GitLab: one service account per agent seat, membership, per-agent PATs minted into the config's own `${VAR}` references, and the group webhook. A re-run leaves a working token alone; `-dry-run` reports without touching anything, and a run that cannot record what it minted revokes it. |
| `crewlet mattermost provision <company.yaml>` | Create/update one Mattermost bot account per Mattermost-enabled agent, add it to the team + channels, mint its access token into an env file or the [secret store](../concepts/secret-store.md). A re-run leaves a working token alone; `-rotate` mints fresh ones. |
| `crewlet mattermost doctor <company.yaml>` | Check a [Mattermost](../integrations/mattermost.md) install end to end: reachability, the Site URL every browser inherits, a browser-shaped websocket upgrade, and one real authenticated socket per agent seat |
| `crewlet --version` | Show the installed version |

---

> **Every command that reads the Tier B company document takes `-config`** (default `./config.yaml`), and resolves its `${VAR}` references the way the engine does: **this node's secret store first, the process environment behind it**. A command that read the environment alone would see an empty string for every value already rotated into the store — and for `integrations.gitlab.signing_secret`, empty is the signal to *mint*, so a re-run would replace a working webhook secret at the vendor. With no bootstrap at that path, or one declaring no `secrets.keys`, the run resolves from the environment alone and says so on its first line. The one exception is the operator's own credential (`-admin-token` / `$GITLAB_ADMIN_TOKEN` and its siblings), which is read from the environment only — see [the secret store](../concepts/secret-store.md#what-still-has-to-be-in-the-environment).

## `crewlet run`

```
crewlet run [-config PATH] [-company PATH] [-debug]
            [-log-level LEVEL] [-log-format FORMAT]
            [-roles ROLE[,ROLE...]] [-api-host HOST] [-api-port PORT]
```

Reads Tier A bootstrap (`./config.yaml` by default) and starts the agent engine. Tier B is read from the `company_config` table in the store — if no active revision exists, the engine boots in the **unconfigured** state with the API still serving so an operator can bootstrap via `crewlet config import` or `PUT /config`. See [Configuration concept doc](../concepts/configuration.md).

| Flag | Description |
|------|-------------|
| `-config PATH` | Tier A: this node's broker, store and API (default `./config.yaml`) |
| `-company PATH` | Tier B **seed**: imported into the store when the store does not already hold it. A running node serves the store, not this file. |
| `-log-level LEVEL` | `debug`, `info` (default), `warn` or `error`. A typo resolves to `info` — a bad log level must never be why a company will not boot. |
| `-log-format FORMAT` | `text` (default) or `json` |
| `-debug` | Shorthand for `-log-level debug`; wins if both are given |
| `-api-host HOST` | Bind address, overriding `api.host` |
| `-api-port PORT` | Bind port, overriding `api.port`. `0` serves **no HTTP at all** — no dashboard, no REST, no webhook endpoint, so every integration goes deaf. That is why leaving the flag off is not the same as passing `0`. |
| `-roles ROLE[,ROLE...]` | What this node runs, overriding `node.roles`: `ingress` (serve the HTTP API and its webhooks), `seats` (claim seat leases and run agents), `workers` (the company-wide singleton duties). Default: all three — one process running a whole company. An unknown name is **rejected rather than dropped**, because a typo would otherwise produce a node that runs nothing and reports itself healthy. See [Running a Fleet](../guides/fleet.md). |

The three overrides are the fields whose right value depends on *where the process is running* rather than on what the company is. Everything else in Tier A belongs in the file, where it can be reviewed.

Publishing knowledge and tool skills is its own command rather than a flag on `run`: an engine that published on every boot would rewrite a company's knowledge base from whatever tree the deploying machine happened to have. Use [`crewlet plane import`](#crewlet-plane-import), and [`crewlet config import`](#crewlet-config-import) to load the first company revision.

Press `Ctrl+C` for graceful shutdown — signals escalate in two tiers:

1. **First `Ctrl+C`** — graceful: every held seat quiesces, running LLM turns finish their rounds (no internal timeout — `drain_in_progress` logs the in-flight count every 10 s), turns still queued behind the concurrency limit are returned to the broker for prompt redelivery, and the embedded dashboard stays up through the drain so you can watch the in-flight pill converge to 0.
2. **Second `Ctrl+C`** — the process exits immediately. The first signal hands signal handling back to the operating system precisely so this works: in-flight turns are killed with their triggers unacknowledged, and the broker redelivers them once its ack window elapses.

There is no third tier, because the second is already an unconditional exit rather than something the engine has to be well enough to perform.

Under an orchestrator, SIGTERM follows the same tiers; the host's grace period (k8s `terminationGracePeriodSeconds`, systemd `TimeoutStopSec`) is the SIGKILL backstop. See [Graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown).

When piping output, prefer `tee -i` — Ctrl+C reaches the whole pipeline, and a plain `tee` dies on the first press, taking the drain logs with it (the shutdown itself is unaffected).

---

## `crewlet config`

Manage the Tier B company configuration in the store. Every subcommand opens the store named by the Tier A bootstrap (`-config`, default `./config.yaml`), and decrypts what it holds with that file's keyring.

**Revisions are immutable and the activation pointer is append-only.** Nothing here edits a revision: importing writes a new one, activating appends to the pointer. That is what makes re-activating an *unchanged* revision meaningful — it mints a new epoch every node is watching, which is how a [rotated secret](../concepts/secret-store.md) reaches a running fleet without a restart.

### `crewlet config import`

```
crewlet config import <company.yaml> [-config PATH]
                                     [--force] [--dry-run] [--summary STR]
```

Validates the Tier B YAML and writes it as a new active revision. Refuses when an active revision already exists unless `--force` (which records the prior revision as `parent_revision_id`). `--dry-run` validates without writing. `--summary` records the audit note (default: `"cli import"`).

### `crewlet config export`

```
crewlet config export [-config PATH] [-revision UUID] [-redact]
```

Dumps the active revision (or `--revision <UUID>`) as YAML to stdout. Emits the stored payload verbatim — a plaintext `${VAR}` config when unencrypted, or the inert `{__encrypted__: "enc:v1:…"}` document blob when [encrypted](../concepts/configuration.md#secrets) (round-trippable: re-importing decrypts and re-stores it). `--redact` decrypts the structure but masks every secret as `{encrypted: true, …}` markers for a share-safe dump. Never emits a plaintext secret.

### `crewlet config show`

```
crewlet config show [-config PATH]
```

Prints a short summary of the active revision (id, activated_at, created_by, source, summary, company name) — or `"No active revision (engine is unconfigured)"` when nothing is active.

### `crewlet config revisions`

```
crewlet config revisions [-config PATH] [-limit 20]
```

Lists recent revisions. The active revision is marked with `*`.

### `crewlet config diff`

```
crewlet config diff <UUID> [-against <UUID|active>] [-config PATH]
```

Prints a line diff of two revisions rendered as YAML, with the unchanged bulk elided — a config is a long document and one line usually moved. `-against active` (default) compares against the currently-active revision.

**Both sides are always redacted, and there is no flag to turn that off.** A diff is what an operator pastes into a ticket or a chat thread to ask a colleague whether a change looks right, which is the single most likely way a credential leaves the machine. `crewlet config export -revision <UUID>` is there for the rare case that needs the real values, and it takes a deliberate act.

### `crewlet config activate`

```
crewlet config activate <UUID> [-config PATH]
```

Re-points the fleet at a revision. Every node applies it on its next reconcile.

**Re-activating the revision that is already active is not a no-op**, and that is the point: the pointer is append-only, so it mints a new epoch. A node's reconciler skips on the *epoch* it has applied, never on the payload, so the apply always runs — re-reading the [secret store](../concepts/secret-store.md) and rebuilding every provider, transport and MCP child that captured a resolved value. It is the documented way to make a rotated credential take effect on a running fleet.

### `crewlet config seal`

```
crewlet config seal [--bootstrap PATH] [--dsn DSN] [--summary TEXT]
```

Encrypts the active revision under the Tier A keyring and writes a new active revision holding the whole config as one opaque `{"__encrypted__": "enc:v1:…"}` document — the one-time migration off plaintext-at-rest. Reads the active revision and encrypts the entire payload (AES-256-GCM); `${VAR}` references inside are kept verbatim and resolve at construction time. A no-op when the active revision is already a document under the active key. Requires a keyring in `config.yaml` (`crewlet secrets keygen`).

### `crewlet config rekey`

```
crewlet config rekey [--bootstrap PATH] [--dsn DSN] [--summary TEXT] [--dry-run]
```

Rotates the master key: re-encrypts the active revision's config document under the current `secrets.active_key_id`, writing a new revision. The document is decrypted with whatever key sealed it (that key **must still be in** `secrets.keys`) and re-encrypted under the active key. Workflow: `crewlet secrets keygen --key-id <new>` → add the new key to `secrets.keys` and set `active_key_id: <new>` while keeping the old key → `crewlet config rekey` → once it succeeds, drop the old key from `config.yaml`. `--dry-run` reports whether it would re-encrypt without writing. Idempotent (a document already under the active key is skipped); a no-op when nothing needs rotating. Fails clearly if the document is sealed under a key no longer in the keyring.

---

## `crewlet secrets`

### `crewlet secrets keygen`

```
crewlet secrets keygen [-key-id ID]
```

Prints a fresh base64 32-byte encryption key plus a copy-pasteable `config.yaml` `secrets:` snippet that references it via an environment variable (keeping the raw key out of the file). `--key-id` (default `key-1`) names the key; the id is stamped into every envelope the key seals, so keep it stable across restarts and pick a new id only when rotating. Key generation is always explicit — Crewlet never auto-generates a key, because a silently-generated key that isn't captured makes every backup unrecoverable. See [Secrets](../concepts/configuration.md#secrets).

The remaining subcommands operate on the [secret store](../concepts/secret-store.md) — the encrypted `secret_values` table the engine consults **ahead of** the process environment when resolving `${VAR}`. All of them open the store named by the Tier A bootstrap (`--config`, default `./config.yaml`), and all of them need a Tier A keyring: the store has no plaintext mode, so a config declaring no `secrets.keys` is refused with a pointer at `keygen` rather than silently storing plaintext.

### `crewlet secrets set`

```
crewlet secrets set <NAME> [-value V] [-source STR] [-config PATH]
```

Stores one encrypted secret under `NAME`, which must be a valid environment-variable name — a name outside the `${VAR}` grammar could never be read back, so it is rejected. Existing names are replaced.

The value comes from **stdin** by default (`echo "$TOKEN" | crewlet secrets set GITLAB_TOKEN_SWE`), or from an interactive prompt on a terminal. `--value` exists for scripted use, but an argv value is visible in `ps` and lands in shell history, so prefer stdin. `--source` records provenance (default `cli`); the provisioning CLIs stamp their own.

A running engine picks the new value up at its next config activation or restart, not immediately — the command says so after each write.

### `crewlet secrets list`

```
crewlet secrets list [-config PATH]
```

Prints one row per stored secret: name, sealing `key_id`, last-updated timestamp, and source. **Never** prints a value — the record type has no value field, so it cannot. Works without a keyring, so an operator locked out of the key can still take inventory.

### `crewlet secrets unset`

```
crewlet secrets unset <NAME> [-config PATH]
```

Removes the row. Exits 1 if the name was not stored. Afterwards `${NAME}` falls back to the environment.

### `crewlet secrets get`

```
crewlet secrets get <NAME> -reveal [-config PATH]
```

Prints one decrypted value to stdout. `--reveal` is **required** — without it the command refuses and reads nothing. This is the only read-back path in the entire system; there is deliberately no HTTP route that returns a secret value. Access is logged by name. Intended as break-glass for recovering a credential the upstream API will never show again, not as a scripting interface.

### `crewlet secrets rekey`

```
crewlet secrets rekey [-dry-run] [-config PATH]
```

Re-encrypts every stored secret not already sealed under `secrets.active_key_id`. The per-row counterpart of [`crewlet config rekey`](#crewlet-config-rekey) — run **both** before dropping a retired key from `secrets.keys`, or rows still sealed under it become unreadable. Each row's envelope names its sealing key, so mixed-key states decrypt correctly throughout. `--dry-run` lists what would re-encrypt without writing.

---

## `crewlet validate`

```
crewlet validate <config> [--tier auto|company|bootstrap] [--json]
```

Validates a config file and prints a summary. Useful for catching errors before running.

Validation is **deep** — it builds the `Organization`, so unknown unit
leads, bad cron expressions, invalid timezones, human seats missing a
contact identity, and the Confluence-XOR-Plane rule all fail here rather
than at run time. It reads **no environment**: Tier B keeps `${VAR}`
references verbatim, so a config validates fully before any secret
exists.

| Flag | Description |
|------|-------------|
| `--tier` | Which tier the file is. `auto` (default) reads a top-level `name` key as Tier B and anything else as Tier A — exact, not a heuristic, since `name` is required in Tier B and forbidden in Tier A. |
| `--json` | Emit a machine-readable result on stdout instead of prose. |

With `--json`, the payload is `{"valid": bool, "tier": str, "errors": [{"path", "message", "type"}], "summary": {...}}` — one record per offending field, with its exact path, so an editor, CI job, or [AI authoring loop](../getting-started/ai-authoring.md) can fix everything in one pass:

```json
{
  "valid": false,
  "tier": "company",
  "errors": [
    { "path": "roles.0.backstroy", "message": "Extra inputs are not permitted", "type": "extra_forbidden" }
  ]
}
```

Exit code is `0` when valid, `1` otherwise (in both output modes).

---

## `crewlet migrate`

Applies pending schema migrations. Every process that opens the store
migrates it, so this is never *required* — what it is, is a way to do it
without starting a node.

```bash
crewlet migrate                          # uses ./config.yaml
crewlet migrate /etc/crewlet/config.yaml
crewlet migrate -check                   # report pending work, apply nothing
```

| Flag | Description |
|------|-------------|
| `config` | Tier A YAML (positional, or `-config`; default `./config.yaml`) |
| `-check` | List pending migrations and exit **1** if there are any; applies nothing. This is what a deploy gate calls, and a gate that reported pending work and exited 0 would stop nothing. |

Rolling out N nodes at once means N processes opening the same database and
racing to apply the same files. That race is safe — one transaction per
file, with the version row written inside it, so a file is either fully
applied and recorded or neither — but it is not what an operator wants to
watch during a deploy, and a failure mid-rollout is a fleet in two schema
states. Migrating once, deliberately, before anything starts makes the
outcome one thing that either worked or did not.

Applying is done by **opening the store**, not by a second code path: a
migrator the engine does not use is one that can disagree with it about what
"applied" means. `-check` is the exception and has to be — it reads
`schema_migrations` and creates nothing, because a command that migrated
while answering "what would you migrate" could never answer it. A database
with no `schema_migrations` table has applied nothing, which is what a fresh
install looks like rather than an error.

---

## `crewlet budgets`

Token-budget usage is stored in the database: it is shared by every process
running the company (an in-memory counter would make an org cap of 500k into
N × 500k) and it survives restarts.

```bash
crewlet budgets show                       # usage per scope
crewlet budgets reset                      # zero every scope
crewlet budgets reset -scope org           # just the org
crewlet budgets reset -scope agent:<id>    # just one seat
```

The **caps** are not stored here — they come from the active company config
(`token_budget` on the org, `role.token_budget` on a seat), so every process
derives the same numbers without coordinating. Only the usage is shared.

`show` prints nothing rather than zeros when no scope has spent anything: a
company that has spent nothing and one whose counters were reset are the
same row-less state, and printing `0` for scopes that do not exist would
invent seats.

`reset` is an operator action and never a schedule — a budget is a ceiling
for the life of a deployment, and a counter that rolled itself over would
silently re-arm a company somebody had stopped on purpose. It **names the
scopes it cleared**, because a count alone leaves you unable to tell "reset
the seat I meant" from "reset a scope that was already empty", and a scoped
reset names only its own scope.

---

## `crewlet schema`

```
crewlet schema [company|bootstrap] [-o PATH]
```

Prints the JSON Schema for a config tier — `company` (Tier B, the
default) or `bootstrap` (Tier A) — to stdout, or to `PATH` with
`-o/--output`.

The schema is **generated from the config types the loader itself uses**,
so it cannot drift from what the engine accepts, and because every config
type forbids unknown keys it carries `additionalProperties: false`
throughout — a mistyped field is flagged rather than silently ignored.

It is deliberately a **subset** of the validator, never a superset. A
schema can express structure — key spaces, types, closed sets, ranges,
patterns — and not the cross-field rules the validator enforces. The
invariant is one-directional and tested: everything the schema rejects,
the validator also rejects. An editor that red-underlines a config the
engine would happily run teaches authors to ignore it.

Both documents are checked into [`schema/`](../../schema/); a test
regenerates and compares them, so a config field added without a schema
entry fails the build rather than leaving a stale file nobody opens.

Point your editor at it for completion, inline field docs, and typo
squiggles:

```yaml
# yaml-language-server: $schema=https://docs.crewlet.ai/schema/company.schema.json
name: "Acme AI"
```

See [Authoring with an AI assistant](../getting-started/ai-authoring.md).

---

## `crewlet mattermost provision`

```
crewlet mattermost provision <company.yaml> [-admin-token TOKEN]
                                           [-secret-store | -env-file PATH | -print]
                                           [-rotate] [-dry-run]
```

Creates (or updates) **one Mattermost bot account per Mattermost-enabled agent seat** — a role whose `mattermost.bot_token` is a whole-value `${VAR}` reference — adds it to the configured team and channels, and mints its personal access token into the exact `${VAR}` the YAML references.

Unlike the Slack provisioner there is **no app manifest, no local ledger and no OAuth click**: Mattermost is its own directory, so a seat is found by looking up a deterministic username and the reconcile is stateless. The one manual prerequisite is a **system-admin personal access token** (`-admin-token`, or `$MATTERMOST_ADMIN_TOKEN`) — creating bot accounts and minting their tokens both require system-admin rights, and an admin must first enable personal access tokens in System Console → Integrations.

**A bot hears only what it has joined**, so the channel list is not a convenience: a bot that exists, authenticates and is in no channel is an agent that never wakes, and the failure is silent on both sides. A channel that does not exist is a note rather than an abort — half a fleet joined and the run stopped is a worse state than every bot joined to the channels that do exist and a line saying which did not.

**A plain re-run does not rotate a working token.** Mattermost returns an access token's value once, so minting every run would revoke the credential every bot's websocket is currently authenticated with — an operator adding a tenth seat would take the other nine down. A bot is left alone when the variable holding its token still has a value **and** the account still has a token under this tool's description (`crewlet-<handle>`). `-rotate` mints for every bot regardless, retiring the previous one after recording the new.

| Flag | Description |
|------|-------------|
| `-admin-token` | System-admin personal access token. Falls back to `$MATTERMOST_ADMIN_TOKEN`. The bots' own tokens are what this run mints, so it cannot bootstrap itself from them. |
| `-secret-store` / `-env-file PATH` / `-print` | Where minted credentials go — exactly one, and there is no default. See [the secret store](../concepts/secret-store.md). |
| `-rotate` | Mint a fresh token for every bot, including bots whose current one still works. **Restart the engine afterwards.** |
| `-dry-run` | Print the plan and touch nothing. It is the **same** plan the run uses. |

A bot this run created is rolled back by revoking every token on it — nothing else has ever minted there. On a bot that already existed, only the token this run minted is revoked. See [Mattermost integration](../integrations/mattermost.md#automated-setup-crewlet-mattermost-provision).

---

## `crewlet mattermost doctor`

```
crewlet mattermost doctor <company.yaml> [-admin-token TOKEN] [-config PATH]
```

Checks a Mattermost install by exercising what actually breaks, in the order it breaks — and **no operator credential is required**: the seat tokens already in the config are what the engine authenticates with, so they are the honest thing to check with, and minting an admin token to find out whether a company works is a step that exists only to be skipped. Pass `-admin-token` (or export `MATTERMOST_ADMIN_TOKEN`) to run the shared checks as somebody else.

| Check | What it catches |
|---|---|
| `GET /system/ping`, **unauthenticated** | Wrong URL, no route from here. First and without a credential, because a bad token must not make a healthy server look dead — the two have completely different remedies. |
| `ServiceSettings.SiteURL` vs `integrations.mattermost.url` | The setting with no error message: Mattermost accepts a websocket only from a client whose `Origin` matches SiteURL, so a mismatch silently costs every human live updates while agents keep working. See [The Site URL](../integrations/mattermost.md#the-site-url). A path-only difference is reported separately — the socket is fine, but the server builds its absolute links from its own value. |
| A websocket upgrade sent **with** a browser's `Origin` | What every human's live feed does, including a reverse proxy that drops `Upgrade`. The Origin carries scheme and host only, because that is the exact string the server compares. |
| The configured team | Channels are team-scoped, so a team that does not resolve is a company where no bot can be placed. |
| Per seat: its own credential, a real socket, its channels | A revoked token, a disabled bot, a `${VAR}` that never reached this deployment, or a bot in **no channel** — which hears only direct messages while its account looks perfectly healthy. |

Each seat is checked with **its own** credential, because "the server accepts sockets" and "this bot wakes" are different questions and only the second delivers a message. A seat that fails early is not asked the later ones: one whose token did not resolve is never dialled, and one whose credential is refused is never asked about its channels — reporting those as separate failures would send an operator after faults nobody observed. The same rule governs the run as a whole: an unreachable server, an unreadable server config or a missing credential **stops** the checks and says so, because one failing line with nothing after it otherwise reads as "one thing is wrong" when it means "nothing else was even asked".

Read-only. Exits non-zero when any check fails, so it drops into a deploy script.

---

## `crewlet plane import`

```
crewlet plane import <company.yaml> <directory> [-token KEY] [-config PATH]
                                               [-prune] [-dry-run]
```

The unified publisher. The first positional is the **Tier B company YAML** — the Plane credentials come from its `integrations.plane` block — and the second is the directory to walk. Every `.md` beneath it is routed **by what the file declares**:

- `trigger:` ⇒ a [Tool Skill](../concepts/tool-skills.md) page (a leading YAML code block the engine parses back out) in the Tool Skills project, `integrations.plane.skills_project`. **Its directory is ignored**: a skill is what it declares, and publishing one as prose would put an instruction meant for one phase of one turn into a planner's context.
- otherwise ⇒ a [knowledge doc](../concepts/knowledge-system.md#publishing-knowledge-docs) as clean prose, in the project named by its **parent directory**, titled by its first `# H1`.

The title comes from the H1 rather than the filename because it is the page name *and* half the idempotency key — a rename would orphan the published page and leave a second one beside it. Frontmatter may override the title (`title:`) and the container (`project:` / `space:`), and nothing else. A file with no determinable title, or two files that would publish as one page, **stop the walk** naming the fix: both are things an operator corrects in their editor, and a run that skipped them would report success with a skill silently unpublished.

**Idempotency is the fork's `external_id` contract**: every published page is stamped `external_source="crewlet"` and `external_id="skill:<key>"` / `"doc:<title>"`, so re-runs match by identity and retitling a page in Plane never orphans it. A re-import always writes — this is a publisher, and skipping existing pages would mean an edited file never reaching the workspace. A page created by hand under the same title is adopted and stamped, but **only** if it carries no external identity at all: one that does belongs to whoever set it. Where two unclaimed pages share a title the lowest page id wins, because Plane guarantees no enumeration order and the alternative is a coin flip.

Every distinct target project must already exist; a missing one fails the run **before any page is written**, naming what the workspace has. The importer never creates projects — that is [`crewlet plane provision -create-projects`](#crewlet-plane-provision). Page-write failures are isolated per page: the rest of the run publishes, then the command **exits non-zero naming the failures**.

| Flag | Description |
|------|-------------|
| `-token KEY` | A Plane API key that may write the target projects. Empty reads `$PLANE_TOKEN`, then `integrations.plane.token`. The account must be a member of every target project. |
| `-prune` | Delete import-managed **skill** pages whose key no local file publishes. Positive-marker predicate — `external_source="crewlet"` **and** a `skill:` external id — so unmarked pages, `doc:` pages and knowledge docs are structurally out of reach: a doc absent from this run is far more likely to have moved than to be dead. Deletion follows the fork's archive-then-delete precondition, per page. When the archive lands and the delete is refused (deletion is owner-or-project-admin only), the archive is **rolled back**: left archived, the page is invisible to every agent while its external id keeps 409ing every future republish of that skill. A failed prune has to be a no-op, not a half-removal. |
| `-dry-run` | Print the routed plan and write nothing. It is the **same** plan the run uses. |

---

## `crewlet plane resync`

```
crewlet plane resync <company.yaml> [-token KEY] [-project ID] [-config PATH]
```

The read-only diagnostic. It runs the **same** walk and the **same** admission the engine's boot sync runs — one strict enumeration of the Tool Skills project — against a throwaway registry, and prints the keys that loaded plus any page that declares a trigger and does not parse. That last case is the one worth printing: somebody wrote a trigger and got the rest wrong, and the only other symptom is guidance that never appears, so the command exits non-zero when it finds one.

It does **not** reach into a running engine: a live engine receives Plane page webhooks directly (create / content update / delete), so this answers "why is this skill not being applied", not "make it apply". Restart the engine, or wait for the next webhook, to change what it holds. `-project` targets a project other than the configured one, for checking a container before pointing the company at it.

---

## `crewlet plane provision`

```
crewlet plane provision <company.yaml> [-admin-token TOKEN]
                                       [-secret-store | -env-file PATH | -print]
                                       [-public-url URL]
                                       [-rotate] [-decommission]
                                       [-create-projects] [-recreate-webhook]
                                       [-token-expiry-days N] [-dry-run]
```

Idempotent reconcile from company config to Plane state — the [`crewlet gitlab provision`](#crewlet-gitlab-provision) analog, targeting the [crewlet/plane fork](../integrations/plane.md#the-fork). For each **agent** seat whose `mcp_env.plane.PLANE_API_KEY` is a whole `${VAR}` reference it ensures a [service account](../integrations/plane.md#provisioning--crewlet-plane-provision) exists (username `<username_prefix><handle>`, display name = role name, explicit workspace role), adds it to every `provisioning.projects` project, and mints that seat's API key into the variable the config already points at. When `integrations.plane.token` is a `${VAR}` reference it also provisions the engine's own `crewlet-engine` read account — always workspace role `member`, whatever the company chose for its agents, because a guest cannot read the subscriber and member lists routing is built on and the engine writes nothing. With `-public-url` it registers the one workspace webhook and **captures Plane's server-generated secret** into the `${VAR}` behind `integrations.plane.webhook_secret`, which must therefore be a reference rather than a literal. Human seats are validated, never created, and the report ends with the workspace member table so founders can fill `contact.plane_user_id`.

**A plain re-run does not rotate a working credential.** Plane serves a token's value once, so a provisioner cannot verify that what it recorded last time still matches — and minting every run is an outage, because the engine is running with the *old* value: an operator adding a tenth seat would revoke the nine credentials the other agents are authenticating with. So a seat is left alone when the variable holding its key still has a value **and** the account still has a usable token under this tool's label, and both halves are checked because either alone is wrong (a recorded value whose token was revoked leaves an agent 401ing for ever; a live token nobody wrote down cannot be deployed). Everything else is minted, and a seat whose token was live but unrecorded is reported as needing an engine restart. `-rotate` mints for every seat regardless — the operator asking, having planned the restart.

| Flag | Description |
|------|-------------|
| `-admin-token` | Operator credential — a **workspace-admin** API key, never stored in config. Falls back to `$PLANE_ADMIN_TOKEN`. The seats' own keys are what this run mints, so it cannot bootstrap itself from them. |
| `-secret-store` / `-env-file PATH` / `-print` | Where minted credentials go — exactly one, and there is no default: a run with nowhere to put what it mints creates live credentials at the vendor and prints none of them. See [the secret store](../concepts/secret-store.md). |
| `-public-url` | This deployment's public base URL; the webhook is registered at `<url>/webhooks/plane`. Omitted, no webhook is registered and the report says so — a hook pointing at the wrong host is worse than none, because the workspace then reports a healthy integration. |
| `-rotate` | Mint a fresh credential for every seat, including seats whose current one still works. **Restart the engine afterwards** — the old values are revoked. |
| `-decommission` | Delete managed service accounts whose seats have left the config. Scoped by `provisioning.username_prefix` (never empty — it defaults to `crewlet-`) and to service accounts only; a person whose name matches the prefix is left alone and reported. The instance's delete cascades tokens, memberships and the account. |
| `-create-projects` | Create configured `provisioning.projects` the workspace does not have, named after the identifier (rename in the Plane UI at will). Without it an unknown identifier aborts the run *before* anything is created, naming what the workspace does have. |
| `-recreate-webhook` | Delete and remake the workspace webhook to mint a fresh secret — the only recovery when the existing secret was never recorded, because Plane will not serve it again. Destructive: it invalidates the secret every other deployment of this company holds. |
| `-token-expiry-days` | Override `provisioning.token_expiry_days` for this run. `0` omits `expired_at`, which in Plane means the token **never expires** (not GitLab's "instance default applies"), and never-expires is also the default: nothing in Crewlet renews a credential on a schedule, so an expiry nobody renews is an outage with a date on it. A company whose policy requires one sets the field and owns the re-run. |
| `-dry-run` | Print the plan and touch nothing. It is the **same** plan the run uses, not a second derivation that could disagree with it. |

A pre-mutation **capability preflight** decides what the instance supports before anything is written, because a run that found out halfway would leave some accounts created, some tokens live, and an operator working out which. It opens with `GET /users/me/` — a credential that does not authenticate is named as such, and without that first call every later 403 is unreadable — then probes the workspace slug (a non-admin route, so a mistyped `workspace:` is told apart from a permission problem) and each capability with a request the route is expected to *refuse*: a `GET` against the POST-only service-accounts route, a `PATCH` against a token collection under the zero UUID. **404 is the only absence**; a 405 is the route rejecting the method and a 403 is its permission class refusing this credential, and both prove the route is there. Stock Plane Community (no service-accounts API), a non-admin credential, an unresolvable workspace and a missing webhook API each abort naming the remedy. An instance with service accounts but **no token-lifecycle API** runs in a degraded mode: new seats still work (an account's creation mints its first token), and a seat that already has an account is named as un-rotatable. A workspace whose member rows carry no `username` aborts — an account created for a seat could never be found again, so every run would create another.

Every mutation is undone when the run cannot finish: minted tokens are revoked, accounts this run created are deleted, a webhook it registered is removed, and the sink is cleared — through a detached context, because the failure is often the cancellation itself. The original error is reported with the cleanup's own problems appended, never replaced by them.

---

## `crewlet gitlab provision`

```
crewlet gitlab provision <company.yaml> [-admin-token TOKEN]
                                        [-secret-store | -env-file PATH | -print]
                                        [-public-url URL]
                                        [-rotate] [-decommission]
                                        [-token-expiry-days N] [-dry-run]
```

Idempotent reconcile from company config to GitLab state. For each **agent** seat that declares a GitLab credential under `mcp_env.gitlab` as a whole `${VAR}` reference, it ensures a [service account](../integrations/gitlab.md#provisioning) exists (username `<username_prefix><handle>`), adds group and project membership, mints a personal access token into that variable, and — with `-public-url` — registers the group webhook at `<url>/webhooks/gitlab`. The positional argument is the Tier B company YAML; its `integrations.gitlab.provisioning` block supplies the group, access levels, and token scopes. Human seats are resolved, never created.

**A dry run says what it would do to the signing secret.** It is the most
consequential thing a run can do — replacing the key a working hook signs with
fails every delivery in flight until the new value reaches the engine — so the
plan states which of *untouched* / *reused* / *minted* / *rotated* will happen,
and into which `${VAR}`. That decision is made by the same function the real
run uses, so the two cannot disagree.

**A run that recorded something says what is left to do.** Where the values
went and what still has to happen are different questions, and only one of the
three sinks answers "source a file": `-env-file` needs sourcing and a restart,
`-print` needs the values moved before the terminal closes, and `-secret-store`
needs the current revision re-activated ([`crewlet config activate`](#crewlet-config-activate))
so the running engine rebuilds its secret snapshot — it needs no file, which is
exactly why a report that stopped at "recorded in the encrypted secret store"
read as finished. A run that changed nothing prints no follow-up.

**A plain re-run does not rotate a working token.** GitLab returns a personal access token's value once, so a provisioner cannot verify that what it recorded last time still matches — and minting every run is an outage, because the engine is running with the *old* value: an operator adding a tenth seat would revoke the nine credentials the other agents are authenticating with. A seat is left alone when the variable holding its token still has a value **and** the account still has a usable token under this tool's name (`crewlet-<handle>`), and both halves are checked because either alone is wrong — a recorded value whose token was revoked leaves an agent 401ing for ever, and a live token nobody wrote down cannot be deployed. `-rotate` mints for every seat regardless, retiring the previous one **after** recording the new: never before, or a failed record leaves the seat with nothing.

| Flag | Description |
|------|-------------|
| `-admin-token` | Operator credential — a top-level group **Owner** PAT with `api` scope on GitLab.com, or an admin PAT self-managed. Falls back to `$GITLAB_ADMIN_TOKEN`. The seats' own tokens are what this run mints, so it cannot bootstrap itself from them. |
| `-secret-store` / `-env-file PATH` / `-print` | Where minted credentials go — exactly one, and there is no default: a run with nowhere to put what it mints creates live credentials at the vendor and prints none of them. See [the secret store](../concepts/secret-store.md). |
| `-public-url` | This deployment's public base URL; the group webhook is registered at `<url>/webhooks/gitlab`. Omit to skip webhook registration — a hook pointing at the wrong host is worse than none, because the instance then reports a healthy integration. |
| `-rotate` | Mint a fresh token for every seat, including seats whose current one still works. **Restart the engine afterwards.** |
| `-decommission` | Delete service accounts whose seats have left the config. Scoped **twice**: the username must start with `provisioning.username_prefix` (never empty — it defaults to `crewlet-`) *and* the account must be a member of this company's group, because either alone is too broad. An account the instance refuses to delete because it is not a service account is reported rather than aborting — that refusal is GitLab catching what the scan should not have proposed, so it is a signal about the prefix. |
| `-token-expiry-days` | Lifetime minted onto each token. Omitted, no `expires_at` is sent and the instance's own policy applies — GitLab.com caps personal access tokens at a year regardless, which is the instance enforcing its policy rather than this tool choosing one. Nothing in Crewlet renews a credential on a schedule, so a lifetime nobody renews is an outage with a date on it. |
| `-dry-run` | Print the plan and touch nothing. It is the **same** plan the run uses. |

An account this run created is rolled back by revoking every token on it — nothing else has ever minted there. On an account that already existed, only the token this run minted is revoked: sweeping it would take an administrator's own token with no way to tell that it had. Both go through a detached context, because the failure is often the cancellation itself.

See [GitLab Integration — Provisioning](../integrations/gitlab.md#provisioning) for the permission matrix and the full walkthrough.
