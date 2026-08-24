# CLI Reference

Crewlet ships a `crewlet` command (also available as `python -m crewlet`).

---

## Commands

| Command | Description |
|---------|-------------|
| `crewlet run [config]` | Read Tier A bootstrap (default `./config.yaml`), connect to DB, run engine; falls into unconfigured state if no active revision |
| `crewlet run api <config>` | **Deprecated** alias for `crewlet run <config> --roles ingress` — an ingress-only node is the standalone API |
| `crewlet validate <config.yaml>` | Validate a Tier A or Tier B YAML and print a summary (`--json` for machine-readable errors) |
| `crewlet migrate [config]` | Apply pending database migrations (Tier A file, default `./config.yaml`). Run this once before starting any process — `--check` reports pending work without applying it |
| `crewlet budgets show [config]` | Print token usage per scope (`org`, `agent:<id>`) |
| `crewlet budgets reset [config]` | Zero token usage — durable across restarts, so resetting is deliberate. `--scope` limits it to one scope |
| `crewlet schema [company\|bootstrap]` | Print the JSON Schema for a config tier (editor autocomplete, CI, [AI-assisted authoring](../getting-started/ai-authoring.md)) |
| `crewlet config import <company.yaml>` | Load Tier B YAML, activate as a new `company_config` revision |
| `crewlet config export [--revision <UUID>]` | Dump the active (or specified) revision as YAML to stdout |
| `crewlet config show` | One-line summary of the active revision |
| `crewlet config revisions [--limit N]` | List recent revisions (newest first) |
| `crewlet config diff <UUID> [--against <UUID\|active>]` | Structural diff between two revisions |
| `crewlet config seal` | Encrypt the active revision as one document under the Tier A keyring (one-time migration off plaintext-at-rest) — see [Secrets](../concepts/configuration.md#secrets) |
| `crewlet config rekey [--dry-run]` | Re-encrypt the active revision's config document under the active key (master-key rotation) |
| `crewlet secrets keygen [--key-id ID]` | Generate a fresh encryption-keyring key + the `config.yaml` snippet to install it |
| `crewlet secrets set <NAME>` | Store an encrypted secret in the [secret store](../concepts/secret-store.md); the engine resolves `${NAME}` from it ahead of the environment |
| `crewlet secrets list` | List stored secret names + metadata (never values) |
| `crewlet secrets unset <NAME>` | Remove a stored secret |
| `crewlet secrets get <NAME> --reveal` | Print one stored value to stdout — break-glass, audited, CLI-only |
| `crewlet secrets rekey [--dry-run]` | Re-encrypt stored secrets under the active keyring key |
| `crewlet llm list` | List the [`cli-agent`](../concepts/subscription-llm-backends.md) LLM providers in a company config and whether each looks logged in |
| `crewlet llm login <provider>` | Authenticate a subscription CLI backend: adopt the machine's existing login (`--from-host`), broker the vendor's own login, mint a headless token, or drive a credential login |
| `crewlet llm doctor [provider]` | Verify a CLI backend end to end — binary, version, login, token accounting, and a real smoke completion |
| `crewlet llm status <provider>` | Ask the CLI who it is logged in as |
| `crewlet llm logout <provider>` | Run the CLI's logout and delete its stored credentials |
| `crewlet llm export <provider>` | Pack the provider's credential files into one portable blob (stdout, or the [secret store](../concepts/secret-store.md)) |
| `crewlet llm import <provider>` | Restore a credential bundle onto this host |
| `crewlet confluence import <company.yaml> [PATH]` | Publish local [Tool Skill](../concepts/tool-skills.md) + [knowledge-doc](../concepts/knowledge-system.md#publishing-knowledge-docs) markdown into Confluence (`trigger:` ⇒ skill; otherwise ⇒ doc in its parent-directory space) |
| `crewlet confluence resync <company.yaml>` | Re-fetch the Tool Skills space and print loaded keys |
| `crewlet plane import <company.yaml> [PATH]` | Publish local [Tool Skill](../concepts/tool-skills.md) + [knowledge-doc](../concepts/knowledge-system.md#publishing-knowledge-docs) markdown into [Plane](../integrations/plane.md) (`trigger:` ⇒ skill in the Tool Skills project; otherwise ⇒ doc in its parent-directory project) |
| `crewlet plane resync <company.yaml>` | Re-fetch the Tool Skills project and print loaded keys |
| `crewlet plane provision <company.yaml>` | Reconcile the config into [Plane](../integrations/plane.md): one service account per agent seat, project memberships, per-agent API tokens (minted from the config's `${VAR}` references), the `crewlet-engine` read account, and the workspace webhook (secret captured) — idempotent, with rotation and decommission paths |
| `crewlet gitlab provision <company.yaml>` | Reconcile the config into GitLab: one service account per agent seat, membership, per-agent PATs (minted from the config's `${VAR}` references), and project webhooks — idempotent, with rotation and decommission paths |
| `crewlet slack provision <company.yaml> --base-url URL` | Create/update one Slack app per Slack-enabled agent via Slack's App Manifest APIs, run the OAuth installs, write tokens into `.env` or the [secret store](../concepts/secret-store.md) |
| `crewlet mattermost provision <company.yaml>` | Create/update one Mattermost bot account per Mattermost-enabled agent, add it to the team + channels, mint its access token into `.env` or the [secret store](../concepts/secret-store.md) |
| `crewlet mattermost doctor <company.yaml>` | Check a [Mattermost](../integrations/mattermost.md) install end to end: reachability, the Site URL every browser inherits, a browser-shaped websocket upgrade, and one real authenticated socket per agent seat |
| `crewlet --version` | Show the installed version |

---

## `crewlet run`

```
crewlet run [config] [--debug] [--api-port PORT] [--api-host HOST]
            [--roles ROLE[,ROLE...]]
            [--import-company PATH]
            [--import-confluence [PATH]] [--update-confluence]
            [--create-confluence-space] [--prune-confluence]
            [--import-plane [PATH]] [--update-plane] [--prune-plane]
```

Reads Tier A bootstrap (`./config.yaml` by default) and starts the agent engine. Tier B is read from the `company_config` PostgreSQL table — if no active revision exists, the engine boots in the **unconfigured** state with the API still serving so an operator can bootstrap via `crewlet config import` or `PUT /config`. See [Configuration concept doc](../concepts/configuration.md).

| Flag | Description |
|------|-------------|
| `--debug` | Enable DEBUG-level logging |
| `--api-port PORT` | Start an embedded API server on this port (for webhooks). Overrides `api.port` in the bootstrap config. |
| `--api-host HOST` | Bind host for the API server. Overrides `api.host` in the bootstrap config. |
| `--roles ROLE[,ROLE...]` | What this node runs, overriding `node.roles`: `ingress` (serve the HTTP API and its webhooks), `seats` (claim seat leases and run agents), `workers` (the company-wide singleton duties). Default: all three — one process running a whole company. An unknown name is rejected rather than dropped. See [Running a Fleet](../guides/fleet.md). |
| `--import-company PATH` | Before booting, import the Tier B company YAML at `PATH` as the first revision **if no active revision exists** — a one-command bootstrap (`crewlet run config.yaml --import-company company.yaml`). Idempotent: a no-op once a revision is active (use `crewlet config import --force` to overwrite). The file's embedding dimensions size the pgvector columns on first migrate. A missing/invalid file aborts before any DB work. |
| `--import-confluence [PATH]` | Before booting the engine, run the same publish as `crewlet confluence import` against `PATH` (defaults to `examples/` when given without a value; walked recursively). Publishes both [Tool Skills](../concepts/tool-skills.md) and [knowledge docs](../concepts/knowledge-system.md#publishing-knowledge-docs), routed by frontmatter. Requires `--import-company`: the Confluence credentials come from the Tier B company YAML's `confluence:` block, not the Tier A bootstrap. The import runs **before** the Tier A bootstrap is loaded — it depends only on `--import-company`, so it publishes even when the positional `config.yaml` is missing or invalid (the engine still needs a valid Tier A config to actually start afterward). Failures abort engine start. Mutually exclusive with `--import-plane`. |
| `--update-confluence` | With `--import-confluence`: overwrite existing pages instead of skipping them. |
| `--create-confluence-space` | With `--import-confluence`: auto-create any target Confluence space that doesn't exist (needs Confluence space-admin permission on the bot account). |
| `--prune-confluence` | With `--import-confluence`: after publishing, delete import-managed skill pages whose local source `.md` was removed (orphans). Only touches pages the importer published, never user-authored pages or knowledge docs. |
| `--import-plane [PATH]` | Before booting the engine, run the same publish as `crewlet plane import` against `PATH` (defaults to `examples/` when given without a value; walked recursively). Publishes both [Tool Skills](../concepts/tool-skills.md) and [knowledge docs](../concepts/knowledge-system.md#publishing-knowledge-docs), routed by frontmatter. **Requires `--import-company`** — the Plane credentials come from the Tier B company YAML's `integrations.plane` block, not the Tier A bootstrap — and is **mutually exclusive with `--import-confluence`** (argparse rejects both; one knowledge backend per run). Like the Confluence variant, the import runs *before* the Tier A bootstrap is loaded and a failure aborts engine start. |
| `--update-plane` | With `--import-plane`: overwrite existing pages instead of skipping them. |
| `--prune-plane` | With `--import-plane`: after publishing, archive+delete import-managed skill pages whose local source `.md` was removed (orphans). Only touches pages the importer published (`external_source="crewlet"`), never user-authored pages or knowledge docs. |

Press `Ctrl+C` for graceful shutdown — signals escalate in three tiers:

1. **First `Ctrl+C`** — graceful: event delivery pauses, running LLM turns finish their rounds (no internal timeout — `drain_in_progress` logs the in-flight count every 10 s), turns still queued behind the concurrency limit are returned to the broker for the next boot, and the embedded dashboard stays up through the drain so you can watch the in-flight pill converge to 0.
2. **Second `Ctrl+C`** — force-stop: in-flight turns are cancelled and NAK'd for redelivery on the next boot, followed by a fast best-effort cleanup.
3. **Third `Ctrl+C`** — hard exit (`os._exit(1)`), the escape hatch for a wedged process.

Under an orchestrator, SIGTERM follows the same tiers; the host's grace period (k8s `terminationGracePeriodSeconds`, systemd `TimeoutStopSec`) is the SIGKILL backstop. See [Graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown).

The per-tier console notices are best-effort: each press schedules its shutdown action *before* printing, so a dead stderr can never stall the ladder. When piping output, prefer `tee -i` — Ctrl+C reaches the whole pipeline, and a plain `tee` dies on the first press, taking the drain logs with it (the shutdown itself is unaffected).

---

## `crewlet config`

Manage the Tier B company configuration stored in PostgreSQL. Every subcommand connects to the DB using the DSN from the Tier A bootstrap (`--bootstrap`, default `./config.yaml`) or an explicit `--dsn`.

### `crewlet config import`

```
crewlet config import <company.yaml> [--bootstrap PATH] [--dsn DSN]
                                     [--force] [--dry-run] [--summary STR]
```

Validates the Tier B YAML and writes it as a new active revision. Refuses when an active revision already exists unless `--force` (which records the prior revision as `parent_revision_id`). `--dry-run` validates without writing. `--summary` records the audit note (default: `"cli import"`).

### `crewlet config export`

```
crewlet config export [--bootstrap PATH] [--dsn DSN] [--revision UUID] [--redact]
```

Dumps the active revision (or `--revision <UUID>`) as YAML to stdout. Emits the stored payload verbatim — a plaintext `${VAR}` config when unencrypted, or the inert `{__encrypted__: "enc:v1:…"}` document blob when [encrypted](../concepts/configuration.md#secrets) (round-trippable: re-importing decrypts and re-stores it). `--redact` decrypts the structure but masks every secret as `{encrypted: true, …}` markers for a share-safe dump. Never emits a plaintext secret.

### `crewlet config show`

```
crewlet config show [--bootstrap PATH] [--dsn DSN]
```

Prints a short summary of the active revision (id, activated_at, created_by, source, summary, company name) — or `"No active revision (engine is unconfigured)"` when nothing is active.

### `crewlet config revisions`

```
crewlet config revisions [--bootstrap PATH] [--dsn DSN] [--limit 20]
```

Lists recent revisions. The active revision is marked with `*`.

### `crewlet config diff`

```
crewlet config diff <UUID> [--against <UUID|active>] [--bootstrap PATH] [--dsn DSN]
```

Prints a structural diff between two revisions: `+ path : value`, `- path`, `~ path : old → new`. `--against active` (default) compares against the currently-active revision.

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
crewlet secrets keygen [--key-id ID]
```

Prints a fresh base64 32-byte encryption key plus a copy-pasteable `config.yaml` `secrets:` snippet that references it via an environment variable (keeping the raw key out of the file). `--key-id` (default `key-1`) names the key; the id is stamped into every envelope the key seals, so keep it stable across restarts and pick a new id only when rotating. Key generation is always explicit — Crewlet never auto-generates a key, because a silently-generated key that isn't captured makes every backup unrecoverable. See [Secrets](../concepts/configuration.md#secrets).

The remaining subcommands operate on the [secret store](../concepts/secret-store.md) — the encrypted `secret_values` table the engine consults ahead of `os.environ` when resolving `${VAR}`. All of them connect to the DB using the DSN from the Tier A bootstrap (`--bootstrap`, default `./config.yaml`) or an explicit `--dsn`, and all of them need a Tier A keyring: the store has no plaintext mode.

### `crewlet secrets set`

```
crewlet secrets set <NAME> [--value V] [--source STR]
                           [--bootstrap PATH] [--dsn DSN]
```

Stores one encrypted secret under `NAME`, which must be a valid environment-variable name — a name outside the `${VAR}` grammar could never be read back, so it is rejected. Existing names are replaced.

The value comes from **stdin** by default (`echo "$TOKEN" | crewlet secrets set GITLAB_TOKEN_SWE`), or from an interactive prompt on a terminal. `--value` exists for scripted use, but an argv value is visible in `ps` and lands in shell history, so prefer stdin. `--source` records provenance (default `cli`); the provisioning CLIs stamp their own.

A running engine picks the new value up at its next config activation or restart, not immediately — the command says so after each write.

### `crewlet secrets list`

```
crewlet secrets list [--bootstrap PATH] [--dsn DSN]
```

Prints one row per stored secret: name, sealing `key_id`, last-updated timestamp, and source. **Never** prints a value — the record type has no value field, so it cannot. Works without a keyring, so an operator locked out of the key can still take inventory.

### `crewlet secrets unset`

```
crewlet secrets unset <NAME> [--bootstrap PATH] [--dsn DSN]
```

Removes the row. Exits 1 if the name was not stored. Afterwards `${NAME}` falls back to the environment.

### `crewlet secrets get`

```
crewlet secrets get <NAME> --reveal [--bootstrap PATH] [--dsn DSN]
```

Prints one decrypted value to stdout. `--reveal` is **required** — without it the command refuses and reads nothing. This is the only read-back path in the entire system; there is deliberately no HTTP route that returns a secret value. Access is logged by name. Intended as break-glass for recovering a credential the upstream API will never show again, not as a scripting interface.

### `crewlet secrets rekey`

```
crewlet secrets rekey [--dry-run] [--bootstrap PATH] [--dsn DSN]
```

Re-encrypts every stored secret not already sealed under `secrets.active_key_id`. The per-row counterpart of [`crewlet config rekey`](#crewlet-config-rekey) — run **both** before dropping a retired key from `secrets.keys`, or rows still sealed under it become unreadable. Each row's envelope names its sealing key, so mixed-key states decrypt correctly throughout. `--dry-run` lists what would re-encrypt without writing.

---

## `crewlet llm`

Operate the [subscription-authenticated CLI
backends](../concepts/subscription-llm-backends.md) — `providers.llm`
entries of type `cli-agent`, which drive a locally installed coding CLI
(`claude`, `codex`, `gemini`, `opencode`, …) on the operator's
subscription instead of a metered API key.

Every subcommand reads the Tier B company YAML for the provider's `cli`
block (`--company`, default `./company.yaml`). The ones that write or
read a secret also take `--bootstrap` (default `./config.yaml`) or
`--dsn`, exactly like [`crewlet secrets`](#crewlet-secrets).

### `crewlet llm list`

```
crewlet llm list [--company PATH]
```

One row per `cli-agent` provider: key, CLI profile, model, and whether a
login is reachable (credential files on disk, or a subscription token in
the [secret store](../concepts/secret-store.md)).

### `crewlet llm login`

```
crewlet llm login <provider> [--company PATH] [--bootstrap PATH] [--dsn DSN]
                             [--capture-token] [--token-stdin]
                             [--username USER] [--password-stdin]
                             [--print-token]
```

With no flags, runs the vendor's own login command attached to your
terminal, inside the provider's isolated credential directory — so the
device-code prompt and browser hand-off behave exactly as they do by
hand, and the result does not collide with your personal CLI login on
the same machine.

`--from-host` skips logging in at all when the machine already has a
login: it copies the CLI's credential files out of your home directory
into Crewlet's. Crewlet never reads `$HOME` by itself (each call gets
its own), so an existing login is invisible until adopted this way.
A copy, not a redirect — agents never write into your personal
credential file. Both copies then share one refresh token, so prefer
`--capture-token` where the vendor mints one. `--home PATH` reads from a
different user's home.

`--capture-token` runs the CLI's token-minting command (`claude
setup-token`) and stores the result encrypted under the profile's token
variable. **Prefer this where the vendor offers it:** no credential
files to sync, no refresh-token rotation, and it survives an ephemeral
container. `--token-stdin` stores a token you already have
(`pass show … | crewlet llm login default --token-stdin`).
`--print-token` writes a captured token to stdout instead of storing it,
and refuses to run on a terminal.

`--username` / `--password-stdin` drive a CLI's credential login where
one genuinely exists (the `opencode` profile, a custom wrapper, a
self-hosted gateway). The password is read from stdin or a declared
environment variable, never from argv. Vendor subscription logins are
browser OAuth with no password grant; for those the command explains
that and points at the flows above rather than failing obscurely.

### `crewlet llm doctor`

```
crewlet llm doctor [provider] [--company PATH] [--bootstrap PATH] [--dsn DSN]
                              [--no-smoke]
```

The command to run before the first turn, and the one to run when a
vendor ships a breaking CLI release. Checks the binary is on `PATH`,
runs its version probe (printing it next to the version the built-in
profile was written against), reports whether a login is present and
whether token counts will be real or estimated — then runs **a real
completion with a real tool**, because a profile can look correct and
still not produce a parseable tool call. `--no-smoke` skips that last
step so the check spends no subscription quota. Omit *provider* to check
every `cli-agent` entry; exits non-zero if any has a problem.

### `crewlet llm status` / `crewlet llm logout`

```
crewlet llm status <provider> [--company PATH]
crewlet llm logout <provider> [--company PATH]
```

`status` forwards to the CLI's own status command. `logout` runs the
CLI's logout **and** deletes the credential files — several CLIs clear
only the active profile's entry, which would keep a revoked login
seeding into every seat. A stored subscription token is a separate
credential; remove it with `crewlet secrets unset`.

### `crewlet llm export` / `crewlet llm import`

```
crewlet llm export <provider> [--secret-store] [--company PATH] [--bootstrap PATH]
crewlet llm import <provider> [--secret-store] [--company PATH] [--bootstrap PATH]
```

Move one login between hosts. `export --secret-store` writes the
credential files as one encrypted blob under
`CREWLET_LLM_CLI_<KEY>_CREDENTIALS`, which any engine sharing that
database restores at boot when its own credential directory is empty —
so a rebuilt container comes up already authenticated. Without the flag
the blob goes to stdout (refused on a terminal).

Only credential files travel; sessions, history and caches never enter a
bundle. On the way back in the archive is validated — files only, only
the profile's own credential paths, size-capped — because an archive is
an execution surface if unpacked on trust.

---

## `crewlet run api` (deprecated)

```
crewlet run api config.yaml [--host 0.0.0.0] [--port 8000] [--debug]
```

**Deprecated — use `crewlet run <config> --roles ingress` instead.** It is
kept for one minor release and prints a warning to stderr; the flags map
onto `--api-host` / `--api-port`.

There is one node type now, and what it does is a config value. An
ingress-only node *is* the standalone API: an engine that claims no
seats, runs no singleton duties, launches no MCP children, and serves the
routes. It used to be a second process shape with its own wiring — the
app, the stream service and the config refresher built by hand in the
same order as the engine's embedded path, but never provably the same
way, so every fix to one had to be remembered for the other.

The split topology is unchanged in shape: run one node with
`--roles ingress` and `api.port` set, and the others with `api.port: 0`
so nothing else binds it. The two halves talk over Pulsar. See
[Running a Fleet](../guides/fleet.md) and
[API Endpoints](api-endpoints.md).

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

Applies pending database migrations. This is the recommended first step of
any deployment — run it to completion **before** starting the engine or the
API.

```bash
crewlet migrate                          # uses ./config.yaml
crewlet migrate /etc/crewlet/config.yaml
crewlet migrate --check                  # report pending work, apply nothing
crewlet migrate --company company.yaml   # supply the embedding width up front
```

| Flag | Description |
|------|-------------|
| `config` | Tier A YAML (positional, default `./config.yaml`) |
| `--company PATH` | Tier B YAML to read the embedding width from, when no revision is active yet |
| `--check` | List pending migrations and exit `1` if any; applies nothing |
| `--debug` | Verbose logging |

The whole run is serialized behind a PostgreSQL advisory lock and each
migration file applies inside its own transaction, so concurrent callers
wait rather than race, and a file is either fully applied and recorded or
neither.

### The embedding width, and why a run can stop early

The `agent_diary` and `episodes` tables carry `vector(N)` columns whose
width is fixed at creation, and the migration sequence is forward-only —
so the width can never be changed afterwards. It is read from the active
company config's `providers.embeddings.dimensions`.

On a database with **no active revision yet**, that width is unknown, and
`crewlet migrate` stops before those two migrations rather than guessing.
It tells you so and exits `0`; the remaining migrations apply on the next
run, once a company config exists:

```bash
crewlet config import company.yaml   # or: crewlet migrate --company company.yaml
crewlet migrate                      # applies the rest
```

This is deliberate. A guessed width that disagrees with your embedding
model makes every diary and episode write fail permanently, and the
failure is swallowed — the [agent-learning subsystem](../concepts/agent-learning.md)
simply goes quiet with nothing in the logs to explain it.

`crewlet run` still auto-migrates on boot, whatever roles the node has, so the
single-host quickstart stays one command. That is now safe to do
concurrently — the advisory lock serializes them, and neither can bake a
guessed embedding width because the width is either read from the active
revision or deferred. Running `crewlet migrate` first is still the
recommendation for any multi-process deployment: it makes schema changes an
explicit, observable step rather than a side effect of whichever process
happened to start first.

---

## `crewlet budgets`

Token-budget usage is stored in PostgreSQL: it is shared by every process
running the company (an in-memory counter would make an org cap of 500k
into N x 500k) and it survives restarts.

```bash
crewlet budgets show                       # usage per scope
crewlet budgets reset                      # zero every scope
crewlet budgets reset --scope org          # just the org
crewlet budgets reset --scope agent:<uuid> # just one seat
```

The **caps** are not stored here — they come from the active company
config (`token_budget` on the org, `role.token_budget` on a seat), so
every process derives the same numbers without coordinating.

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

## `crewlet confluence import`

```
crewlet confluence import my_company.yaml [PATH]
                                          [--space KEY] [--update] [--dry-run]
                                          [--create-space] [--prune]
```

The positional argument is the **Tier B company YAML** — the Confluence credentials are read from its `confluence:` block (passing the Tier A bootstrap fails with `Company config file must have a 'name' field`). Walks every `.md` file under `PATH` recursively (defaults to `examples/`) and **routes each file by frontmatter**:

- `trigger:` ⇒ a [Tool Skill](../concepts/tool-skills.md) page (YAML-macro encoding) in the Tool Skills space. Idempotent by the `crewlet-skill-key-<key>` label.
- **otherwise** ⇒ a [knowledge doc](../concepts/knowledge-system.md#publishing-knowledge-docs) (clean prose, `crewlet-doc` label). Its **space is the file's parent directory name** and its **title is the first `# H1`** (frontmatter is optional, only for `title` / `parent` / `labels` overrides). Idempotent by `(space, title)`.

A skill file with malformed frontmatter, or a doc with no determinable title (no `# H1` and no frontmatter `title:`), is skipped with a log line.

| Flag | Description |
|------|-------------|
| `--space KEY` | Tool Skills space key for **skill** files. Default: `$CREWLET_TOOL_SKILLS_SPACE` or `TS`. Knowledge docs take their space from their parent directory. |
| `--update` | Overwrite existing pages (Confluence retains the prior version in page history). |
| `--dry-run` | Print what would be created/updated without making API calls. |
| `--create-space` | Auto-create any target Confluence space that doesn't exist. Requires the bot account to have Confluence space-admin permission on the tenant. |
| `--prune` | After publishing, delete import-managed skill pages in the skill space whose source `.md` is gone (e.g. a removed bundled skill). Only touches pages the importer published — identified by the `crewlet-skill` marker plus a per-key label no local file claims — never user-authored pages or knowledge docs. Combine with `--dry-run` to preview deletions. |

> **Credentials.** The company YAML's `confluence:` block typically references environment variables (e.g. `token: "${CONFLUENCE_API_TOKEN}"`). The command resolves them from the process environment, and — like `crewlet run` — first loads a `.env` next to the company YAML (falling back to `./.env`), so credentials kept only in `.env` work too. Real environment variables take precedence over `.env`. Use `--update` to overwrite existing pages; **without it, pages that already exist are skipped** (only new ones are created).

See [Tool Skills](../concepts/tool-skills.md) and [Knowledge System](../concepts/knowledge-system.md#publishing-knowledge-docs) for the file formats and operator workflow.

---

## `crewlet slack provision`

```
crewlet slack provision my_company.yaml --base-url https://your-server.com
                                        [--secret-store] [--env-file PATH]
                                        [--bootstrap PATH] [--dsn DSN]
                                        [--state-file PATH]
                                        [--handles a,b] [--dry-run]
                                        [--skip-install] [--reinstall]
```

Creates (or updates) **one Slack app per Slack-enabled agent seat** — a role whose `integrations.slack` uses whole-value `${VAR}` placeholders — via Slack's App Manifest APIs, then walks through the per-app OAuth install click and writes every obtained secret (`SLACK_BOT_TOKEN_*`, `SLACK_SIGNING_SECRET_*`) under the exact `${VAR}` names the YAML references — into `.env` by default, or into the encrypted [secret store](../concepts/secret-store.md) with `--secret-store`. Self-contained by default: company YAML + network access to slack.com, no engine/DB/queue; `--secret-store` additionally needs the database and the Tier A keyring.

Requires an app **configuration token** (`SLACK_CONFIG_REFRESH_TOKEN`, generated once at [api.slack.com/apps](https://api.slack.com/apps) → *Your App Configuration Tokens*), taken from the env file or the environment; rotation is expiry-aware and the fresh pair (plus `SLACK_CONFIG_TOKEN_EXPIRES_AT`) is persisted back to the env file, which is authoritative from then on. The Crewlet API should already be serving at `--base-url` so Slack's events-URL verification and the OAuth landing page (`GET /webhooks/slack-oauth`) work — see [Slack integration](../integrations/slack.md#automated-setup-crewlet-slack-provision) for the full operator flow.

| Flag | Description |
|------|-------------|
| `--base-url URL` | Public HTTPS base URL of the Crewlet API server (required). Becomes each app's events Request URL (`…/webhooks/slack/{handle}`) and OAuth redirect (`…/webhooks/slack-oauth`). |
| `--secret-store` | Write the obtained secrets into the encrypted [`secret_values`](../concepts/secret-store.md) table instead of an env file. The engine resolves `${VAR}` from there directly, so the source-and-restart step disappears — including for the rotating config-token pair, whose persisted copy is the only valid one after each rotation. Needs a Tier A keyring + DSN (`--bootstrap` / `--dsn`). |
| `--env-file PATH` | The env file secrets are read from **and** written to (default: the `.env` `crewlet run` loads — beside the company YAML if one is there, else `./.env`). For provisioning-managed keys, values in this file win over shell exports. Ignored with `--secret-store`. |
| `--bootstrap PATH` / `--dsn DSN` | Tier A bootstrap YAML (default `./config.yaml`) supplying the DB DSN + keyring for `--secret-store`, or an explicit DSN override. |
| `--state-file PATH` | Provisioning ledger mapping handles to app ids + app credentials + last-pushed manifest fingerprints (default: `slack-apps.json` next to the company YAML). A secrets file — keep it out of version control. |
| `--handles a,b` | Comma-separated agent handles to provision (default: all Slack-enabled seats). |
| `--dry-run` | Validate each manifest via `apps.manifest.validate` and print the plan; no app is created or updated. May refresh the stored config-token pair if missing or near expiry (validation needs a live token). |
| `--skip-install` | Create/update apps and write signing secrets, but skip the interactive OAuth step (no bot tokens written) — for non-interactive runs, e.g. pushing a scope change to every app. |
| `--reinstall` | Redo the OAuth install even for agents whose bot-token env var is already set (mints a fresh `xoxb-` token, e.g. after a revoke or scope change). |

Re-runs are idempotent (ledger-keyed; byte-identical manifests are skipped as `unchanged`) and resumable — state and secrets are persisted after every step, so an interrupted run continues where it stopped. One agent's failure is reported as `FAILED`, the remaining agents still run, and the exit code is non-zero.

---

## `crewlet mattermost provision`

```
crewlet mattermost provision my_company.yaml [--admin-token TOKEN]
                                             [--secret-store] [--env-file PATH]
                                             [--bootstrap PATH] [--dsn DSN]
                                             [--handles a,b] [--dry-run]
                                             [--decommission a,b]
```

Creates (or updates) **one Mattermost bot account per Mattermost-enabled agent seat** — a role whose `integrations.mattermost.bot_token` is a whole-value `${VAR}` placeholder — adds it to the configured team and channels, and mints its personal access token into the exact `${VAR}` the YAML references: into `.env` by default, or the encrypted [secret store](../concepts/secret-store.md) with `--secret-store`.

Unlike the Slack provisioner there is **no app manifest, no local ledger and no OAuth click**: Mattermost is its own directory, so a seat is found by looking up a deterministic username and the reconcile is stateless. The one manual prerequisite is a **system-admin personal access token** (`--admin-token`, or `$MATTERMOST_ADMIN_TOKEN`) — creating bot accounts and minting their tokens both require system-admin rights, and an admin must first enable personal access tokens in System Console → Integrations.

A preflight aborts before touching anything if the run cannot finish: the credential is not a system admin, the team does not exist, `ServiceSettings.EnableBotAccountCreation` or `ServiceSettings.EnableUserAccessTokens` is off (both default to false on a fresh install, and both fail *after* every bot has been created and joined), or the server's [Site URL](../integrations/mattermost.md#the-site-url) is a loopback address while the server is reached at a real one — which would silently cost every browser its live updates. A half-provisioned fleet is worse than a refusal. It also reports the server's active-**human**-user headroom; bot accounts are excluded from that cap, so agent seats do not consume it.

| Flag | Description |
|------|-------------|
| `--admin-token TOKEN` | System-admin personal access token (default: `$MATTERMOST_ADMIN_TOKEN`). |
| `--handles a,b` | Comma-separated agent handles to provision (default: all Mattermost-enabled seats). |
| `--decommission a,b` | Disable these handles' bot accounts and revoke their tokens instead of provisioning. The account is disabled first (that is what stops the seat acting), then each token is revoked on its own, so one failure costs one token rather than the whole seat; anything left over is named in the outcome and exits non-zero. Accounts keep their history and can be re-enabled by a later run, which mints a fresh token. |
| `--dry-run` | Print the plan (which seats would mint which vars); create and modify nothing. Applies to `--decommission` too. |
| `--secret-store` | Write minted tokens into the encrypted [`secret_values`](../concepts/secret-store.md) table instead of an env file. Needs a Tier A keyring + DSN (`--bootstrap` / `--dsn`). |
| `--env-file PATH` | The env file minted tokens are written to (default `.env`). Ignored with `--secret-store`. |
| `--bootstrap PATH` / `--dsn DSN` | Tier A bootstrap YAML (default `./config.yaml`) supplying the DB DSN + keyring for `--secret-store`, or an explicit DSN override. |

Re-runs are idempotent and resumable: every step is find-or-create, a seat whose token var already carries a value is never re-minted (Mattermost returns a token's value exactly once), and one seat's failure is reported as `FAILED` while the rest still provision — the exit code is then non-zero. See [Mattermost integration](../integrations/mattermost.md#automated-setup-crewlet-mattermost-provision).

---

## `crewlet mattermost doctor`

```
crewlet mattermost doctor my_company.yaml
```

Checks a Mattermost install the way [`crewlet llm doctor`](#crewlet-llm) checks a subscription CLI: by exercising what actually breaks, not what raises.

| Check | What it catches |
|---|---|
| `GET /system/ping` (unauthenticated) | Wrong URL, server down, no route from here |
| `ServiceSettings.SiteURL` vs `integrations.mattermost.url` | The setting with no error message: Mattermost accepts a websocket only from a browser whose `Origin` matches SiteURL exactly, so a mismatch silently costs every human live updates while the engine — which sends no `Origin` — keeps working. See [The Site URL](../integrations/mattermost.md#the-site-url). |
| A websocket upgrade sent **with** an `Origin` header | Predicts what a browser gets, including a reverse proxy that drops `Upgrade` |
| Per seat: `/users/me`, account state, channel membership | Revoked token, disabled bot, a bot in no channel (it would only ever hear DMs) |
| Per seat: a real authenticated websocket | The engine's only inbound path — a token valid for REST can still fail to open a socket |

Read-only, and no operator credential: the seat tokens already in the config do the work, resolved the way the engine resolves them ([secret store](../concepts/secret-store.md), then environment). Exits non-zero when any check fails.

---

## `crewlet confluence resync`

```
crewlet confluence resync my_company.yaml [--space KEY]
```

Re-runs the boot-time full populate of the Tool Skills registry against a *temporary* registry and prints the loaded keys. Like `confluence import`, the positional argument is the Tier B company YAML (its `confluence:` block carries the credentials). Used to verify what the engine would see at next boot, or to diagnose drift after a long outage. **Skills-only** — knowledge docs are searched live and never loaded into a registry, so there is nothing to resync for them. **Does not** reach into a running engine — restart the engine (or wait for the next webhook) to apply changes there.

---

## `crewlet plane import`

```
crewlet plane import my_company.yaml [PATH]
                                     [--project ID] [--update] [--dry-run]
                                     [--prune]
```

The Plane analog of [`crewlet confluence import`](#crewlet-confluence-import). The positional argument is the **Tier B company YAML** — the Plane credentials are read from its `integrations.plane` block (the block must be present and enabled). Walks every `.md` file under `PATH` recursively (defaults to `examples/`) and **routes each file by frontmatter**:

- `trigger:` ⇒ a [Tool Skill](../concepts/tool-skills.md) page (leading YAML code block) in the Tool Skills project.
- **otherwise** ⇒ a [knowledge doc](../concepts/knowledge-system.md#publishing-knowledge-docs) (clean prose). Its **project is the file's parent directory name** (identifier, matched case-insensitively) and its **title is the first `# H1`** (frontmatter is optional, only for `title` / `parent` overrides; `labels` is ignored — Plane pages have no free-form labels).

**Idempotency is the fork's `external_id` contract**: every published page is stamped `external_source="crewlet"` + `external_id="skill:<key>"` / `"doc:<title>"`, so re-runs match by external identity (retitling a page in Plane never orphans it) with an exact-title fallback that adopts pre-existing unmarked pages. A frontmatter `parent:` resolves against the project's pages **including those published earlier in the same run**. A skill file with malformed frontmatter, or a doc with no determinable title, is skipped with a log line. Every distinct target project must already exist — a missing project fails the run **before any page is written**, naming the identifiers to create; the importer never creates projects. Page-write failures (a locked page, a page-level 403) are isolated per page: the rest of the run publishes, then the command **exits non-zero naming the failed files** (only a 401 or an enumeration failure aborts outright). See [Plane § Publishing docs + skills](../integrations/plane.md#publishing-docs--skills--crewlet-plane-import) for the full contract.

| Flag | Description |
|------|-------------|
| `--project ID` | Tool Skills project identifier for **skill** files. Default: `$CREWLET_TOOL_SKILLS_PROJECT` or `TS` — the same env var the engine's sync worker reads. Knowledge docs take their project from their parent directory. |
| `--update` | Overwrite existing pages (also stamps the external identity onto pages adopted via the title fallback). |
| `--dry-run` | Log what would be created/updated (and, with `--prune`, deleted) without making page writes. |
| `--prune` | After publishing, **archive + delete** import-managed skill pages in the skills project whose source `.md` is gone. Positive-marker predicate: only pages with `external_source="crewlet"` and a `skill:` external id no local file claims — never user-authored pages, `doc:` pages, or knowledge docs. Per-page failure isolation: a 403 on a page the account doesn't own logs and continues — and when the delete is refused *after* a successful archive, the archive is **rolled back** (unarchive) so the page stays visible and republishable rather than stranded archived behind a permanently-409ing external id. If the project enumeration is incomplete, the run aborts before pruning — an incomplete listing deletes nothing. |

> **Credentials.** The company YAML's `integrations.plane` block typically references environment variables (e.g. `token: "${PLANE_ENGINE_TOKEN}"`). The command resolves them from the process environment, and — like `crewlet run` — first loads a `.env` next to the company YAML (falling back to `./.env`). The token's account must be a member of every target project.

---

## `crewlet plane resync`

```
crewlet plane resync my_company.yaml [--project ID]
```

The Plane analog of [`crewlet confluence resync`](#crewlet-confluence-resync): re-runs the boot-time full populate of the Tool Skills registry — one strict enumeration of the Tool Skills project, the same decode + admission logic the engine's boot walk runs — against a *temporary* registry and prints the loaded keys. **Skills-only**, and **does not** reach into a running engine: a live engine receives Plane page webhooks directly (create / content-persist update / delete), so a manual resync is only needed when you suspect a webhook was missed across a long outage. Restart the engine (or wait for the next webhook) to apply changes there.

---

## `crewlet plane provision`

```
crewlet plane provision <company.yaml> [--provision-token TOKEN]
                                       [--webhook-url URL]
                                       [--env-file PATH] [--print]
                                       [--rotate] [--decommission-removed]
                                       [--recreate-webhook]
                                       [--create-projects]
                                       [--token-expiry-days N]
```

Idempotent reconcile from company config to Plane state — the [`crewlet gitlab provision`](#crewlet-gitlab-provision) analog, targeting the [crewlet/plane fork](../integrations/plane.md#the-fork). For each **agent** seat that declares Plane credentials (`mcp_env.plane`), it ensures a [service account](../integrations/plane.md#provisioning--crewlet-plane-provision) exists (username `<username_prefix><handle>`, display name = role name, explicit workspace role), adds it to every `provisioning.projects` project, and mints a per-agent API token derived from the seat's own `${VAR}` references (skipping any var that already has a value — Plane returns every credential exactly once). When `integrations.plane.token` is a `${VAR}` reference it also provisions the engine's `crewlet-engine` read account (workspace role `member`, member of every configured project), and with `--webhook-url` it registers the one workspace webhook and **captures Plane's server-generated secret** into the `${VAR}` behind `integrations.plane.webhook_secret` — which must therefore be a reference, not a literal. Human seats are validated, never created, and the report ends with the workspace member table so founders can fill `contact.plane_user_id`. There is no `--mode` flag (every provisioning surface is workspace-admin-gated — no instance mode) and no group-webhook variant (one workspace hook).

| Flag | Description |
|------|-------------|
| `--provision-token` | Operator credential — a **workspace-admin** personal API token, never stored in config. Falls back to `$PLANE_PROVISION_TOKEN`. |
| `--webhook-url` | Engine webhook endpoint to register as the one workspace webhook (e.g. `https://engine.example.com/webhooks/plane`). Omit to skip the webhook step (with `--rotate`, that leaves the webhook secret un-rotated — noted in the report). |
| `--secret-store` | Write minted credentials into the encrypted [`secret_values`](../concepts/secret-store.md) table instead of an env file. The engine resolves `${VAR}` from there directly, so nothing has to be sourced into a shell. Needs a Tier A keyring + DSN (`--bootstrap` / `--dsn`). |
| `--env-file` | Env file to append/update minted `VAR=token` lines into (default `.env.plane`). Source it **and restart the engine**; the env file — not the shell — is the source of truth for what has been minted. Ignored with `--secret-store`. |
| `--print` | Print `export VAR=token` lines to stdout instead of writing an env file. |
| `--rotate` | Rotate every managed credential: each seat's managed token and the engine token (fresh explicit expiry; requires the fork's token-lifecycle API), and — only together with `--webhook-url` — the webhook secret (delete + recreate; Plane secrets are immutable and single-show). Also the **only** recovery for a `token=needs_rotate` seat (an unrecorded `${VAR}` whose account holds an active managed token): a plain run never rotates a live token, because rotation invalidates the value the running engine holds. |
| `--decommission-removed` | Delete service accounts whose seats left the config (the fork's account-delete cascade removes tokens, memberships, and the account). Requires `provisioning.username_prefix` to scope managed accounts safely. **Irreversible for the username** on fork builds without the create-reactivate capability (see [the fork](../integrations/plane.md#the-fork)) — the deactivated row keeps the username, so re-adding the seat later 409s at create. |
| `--recreate-webhook` | When the workspace webhook exists but its secret is not recorded here (lost env file, second machine): delete + recreate it to mint a fresh secret. Destructive — invalidates the secret every other deployment holds; without the flag the run emits a note instead. |
| `--create-projects` | Create declared `provisioning.projects` that don't exist yet (name = identifier; rename in the Plane UI at will). [`crewlet plane import`](#crewlet-plane-import) never creates projects — this does. |
| `--token-expiry-days` | One-off override of `provisioning.token_expiry_days` (the standing policy; config default 364). Must be `>= 0` — the parser rejects negative values, which would silently mean never-expires. `0` omits `expired_at`, which in Plane means the token **never expires** (not GitLab's "instance default applies"). |

A pre-mutation **capability preflight** decides what the instance supports before anything is written. It opens with a `GET /users/me/` credential probe (a bad token is named as such — Plane does not pin whether it answers 401 or 403) and a workspace-slug probe (a mistyped `workspace:` is named as such), then probes capabilities: stock CE (no service-accounts route) aborts with the fork message, a non-workspace-admin operator aborts naming the permission, a fork with service accounts but without the token-lifecycle API runs plain reconciles in a **degraded service-accounts-only mode** (creation itself mints the first token; unmintable seats report `token=blocked` and the run exits 0; `--rotate` / `--decommission-removed` abort up front), and a members listing that doesn't expose `username` aborts (the fork's provisioning-identity fields are hard-required) — see [Plane — capability preflight and degraded modes](../integrations/plane.md#capability-preflight-and-degraded-modes) for the full table and [the walkthrough](../integrations/plane.md#provisioning--crewlet-plane-provision) for the six reconcile steps. Exit 1 on any seat error (a seat whose account was created before a later step failed keeps `account=created`; the error column drives the exit code).

---

## `crewlet gitlab provision`

```
crewlet gitlab provision <company.yaml> [--provision-token TOKEN]
                                        [--mode group|instance]
                                        [--webhook-url URL]
                                        [--env-file PATH] [--print]
                                        [--rotate] [--decommission-removed]
                                        [--token-expiry-days N]
```

Idempotent reconcile from company config to GitLab state. For each **agent** seat that declares GitLab credentials (`mcp_env.gitlab`), it ensures a [service account](../integrations/gitlab.md#provisioning) exists (username `<username_prefix><handle>`), adds group/project membership, mints a per-agent PAT derived from the seat's own `${VAR}` references (skipping any var that already has a value — GitLab never returns a token after creation), and — when `--webhook-url` is passed — registers project (or group) webhooks. The positional argument is the Tier B company YAML; its `integrations.gitlab.provisioning` block supplies the group, access levels, and token scopes. Human seats are resolved, never created.

| Flag | Description |
|------|-------------|
| `--provision-token` | Operator credential — a top-level group **Owner** PAT with `api` scope on GitLab.com, or an admin PAT self-managed. Falls back to `$GITLAB_PROVISION_TOKEN`, then `$GITLAB_ADMIN_TOKEN`. |
| `--mode` | `group` (default; group-Owner-callable on GitLab.com) or `instance` (self-managed admin, instance-wide accounts). |
| `--webhook-url` | Engine webhook endpoint to register on the group/projects (e.g. `https://engine.example.com/webhooks/gitlab`). Omit to skip webhook registration. |
| `--secret-store` | Write minted credentials into the encrypted [`secret_values`](../concepts/secret-store.md) table instead of an env file. The engine resolves `${VAR}` from there directly, so nothing has to be sourced into a shell. Needs a Tier A keyring + DSN (`--bootstrap` / `--dsn`). |
| `--env-file` | Env file to append/update minted `VAR=token` lines into (default `.env.gitlab`). Source it before starting the engine. Ignored with `--secret-store`. |
| `--print` | Print `export VAR=token` lines to stdout instead of writing an env file. |
| `--rotate` | Rotate each managed service-account token (re-mints with a fresh expiry). The once-a-year cron path on GitLab.com Free, where every PAT expires within 365 days. |
| `--decommission-removed` | Soft-delete service accounts whose seats left the config. Requires `provisioning.username_prefix` to scope managed accounts safely. |
| `--token-expiry-days` | Expiry for minted/rotated tokens (default `364`; GitLab.com Free max is 365). `0` omits `expires_at` so the instance default/max applies. |

The CLI probes the operator credential with `GET /user` up front and fails fast with the endpoint + status when the token or its scopes are wrong. See [GitLab Integration — Provisioning](../integrations/gitlab.md#provisioning) for the permission matrix and the full walkthrough.

