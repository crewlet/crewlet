# CLI Reference

Crewlet ships one command, `crewlet` — a single static binary. Every
subcommand below is served by it.

---

## Commands

| Command | Description |
|---------|-------------|
| `crewlet run [config.yaml]` | Read Tier A bootstrap (positional, or `-config`; default `./crewlet.yaml`), connect to DB, run engine; falls into unconfigured state if no active revision |
| `crewlet validate [file.yaml]` | Validate a Tier A or Tier B YAML and print a summary (`-json` for machine-readable errors); with no positional it checks both tiers via `-config` and `-company` |
| `crewlet migrate [config.yaml]` | Apply pending schema migrations (Tier A file, default `./crewlet.yaml`). Every process migrates on open, so this is a way to do it *without* starting one — `-check` reports pending work and exits non-zero without applying it |
| `crewlet budgets show [config]` | Print token usage per scope (`org`, `agent:<id>`), read from a running node — the counter is the fleet's, not this file's |
| `crewlet budgets reset [config]` | Zero token usage on a running node — durable across restarts, so resetting is deliberate. `-scope` limits it to one scope, and the report names what it cleared |
| `crewlet schema [company\|bootstrap]` | Print the JSON Schema for a config tier (editor autocomplete, CI, [AI-assisted authoring](../getting-started/ai-authoring.md)) |
| `crewlet config import <company.yaml>` | Load Tier B YAML, activate as a new `company_config` revision |
| `crewlet config export [--revision <UUID>]` | Dump the active (or specified) revision as YAML to stdout |
| `crewlet config show` | One-line summary of the active revision |
| `crewlet config revisions [--limit N]` | List recent revisions (newest first) |
| `crewlet config diff <UUID> [-against <UUID\|active>]` | Structural diff of two revisions — paths and values, always redacted on both sides |
| `crewlet config activate <UUID>` | Re-point the fleet at a revision; re-activating the current one mints a new epoch, which is how a rotated secret takes effect |
| `crewlet config seal` | Encrypt the active revision as one document under the Tier A keyring (one-time migration off plaintext-at-rest) — see [Secrets](../concepts/configuration.md#secrets) |
| `crewlet config rekey [-dry-run]` | Re-encrypt the active revision's config document under the active key (master-key rotation) |
| `crewlet secrets keygen [-key-id ID]` | Generate a fresh encryption-keyring key + the `crewlet.yaml` snippet to install it |
| `crewlet secrets set <NAME>` | Store an encrypted secret in the [secret store](../concepts/secret-store.md); the engine resolves `${NAME}` from it ahead of the environment |
| `crewlet secrets list` | List stored secret names + metadata (never values) |
| `crewlet secrets unset <NAME>` | Remove a stored secret |
| `crewlet secrets get <NAME> -reveal` | Print one stored value to stdout — break-glass, audited, CLI-only |
| `crewlet secrets rekey [-dry-run]` | Re-encrypt stored secrets under the active keyring key |
| `crewlet llm list` | Every `cli-agent` provider the company declares, with its CLI, model and login state |
| `crewlet llm doctor [KEY]` | Verify a subscription backend end to end — the CLI is installed, the login answers, a real completion returns (`-no-smoke` stops before the completion) |
| `crewlet llm login <KEY>` | Establish the vendor's own login for a provider: brokered interactively, `-from-host` to adopt one this machine already has, `-capture-token` to mint a headless token into the [secret store](../concepts/secret-store.md) (add `-print-token` to send it to stdout and store nothing), `-token-stdin` for one you already hold |
| `crewlet llm status <KEY>` | Ask the CLI who it is currently logged in as |
| `crewlet llm logout <KEY>` | Revoke locally and delete the provider's credential files |
| `crewlet llm export <KEY> [-secret-store]` | Pack the login into one portable blob — stdout, or the secret store under the name the engine restores from on a fresh host |
| `crewlet llm import <KEY>` | Restore a bundle from **stdin** onto this host; refuses to overwrite a login that is already there |
| `crewlet plane import <company.yaml> <directory>` | Publish local [Tool Skill](../concepts/tool-skills.md) + [knowledge-doc](../concepts/knowledge-system.md#publishing-knowledge-docs) markdown into [Plane](../integrations/plane.md) — `trigger:` ⇒ skill in the Tool Skills project, otherwise ⇒ doc in its parent-directory project. Idempotent by `external_id`; `-prune` removes orphaned skill pages. |
| `crewlet plane resync <company.yaml>` | Re-run the engine's own skills walk against a throwaway registry and print what loads — a read-only diagnostic, not a way to change a running engine |
| `crewlet plane provision <company.yaml>` | Reconcile the config into [Plane](../integrations/plane.md): one service account per agent seat, project memberships, per-agent API tokens (minted from the config's `${VAR}` references), the `crewlet-engine` read account, and the workspace webhook (secret captured) — idempotent, with rotation and decommission paths |
| `crewlet confluence import <company.yaml> <directory>` | Publish a directory of authored markdown into [Confluence](../integrations/confluence.md) spaces — one space per directory, plus the tool skills the files themselves declare. Every target space is checked before a single page is written |
| `crewlet confluence resync <company.yaml>` | Re-run the engine's own tool-skill walk of the Confluence skills space against a throwaway registry and print what loads — a read-only diagnostic, not a way to change a running engine |
| `crewlet slack provision <company.yaml>` | Create, update and install one [Slack](../integrations/slack.md) app per agent seat from the canonical manifest, minting each seat's bot token and signing secret into the `${VAR}`s its config points at. The install itself is an OAuth grant, so the run hands the operator one authorize URL per seat and takes the code back |
| `crewlet jira provision <company.yaml>` | Report a [Jira](../integrations/jira.md) instance against the config: which account each seat's own credential authenticates as, whether every project the org chart names exists and agrees about its lead, and — on Data Center — register the inbound webhook with a minted secret. Jira issues no credentials on a provisioner's behalf, so this run reports far more than it changes |
| `crewlet gitlab provision <company.yaml>` | Reconcile the config into GitLab: one service account per agent seat, membership, per-agent PATs minted into the config's own `${VAR}` references, and the group webhook. A re-run leaves a working token alone; `-dry-run` reports without touching anything, and a run that cannot record what it minted revokes it. |
| `crewlet github provision <company.yaml>` | Report a [GitHub](../integrations/github.md) deployment against the config — which account each seat's own credential authenticates as — and register the inbound webhooks with a minted secret: one on the organization where the credential may, otherwise one per named repository. GitHub issues no credentials on a provisioner's behalf, so this run reports more than it changes |
| `crewlet mattermost provision <company.yaml>` | Create/update one Mattermost bot account per Mattermost-enabled agent, add it to the team + channels, mint its access token into an env file or the [secret store](../concepts/secret-store.md). Keeps each bot's display name in step with the company document. A re-run leaves a working token alone; `-rotate` mints fresh ones, `-handles a,b` narrows the run, `-decommission` disables departed seats' bots. Runs a preflight first: system-admin role, `EnableBotAccountCreation`, `EnableUserAccessTokens`. |
| `crewlet mattermost doctor <company.yaml>` | Check a [Mattermost](../integrations/mattermost.md) install end to end: reachability, the Site URL every browser inherits, a browser-shaped websocket upgrade, and one real authenticated socket per agent seat |
| `crewlet --version` | Show the installed version |

---

> **Every command that reads the Tier B company document takes `-config`** (default `./crewlet.yaml`), and resolves its `${VAR}` references the way the engine does: **the secret store first, the process environment behind it**. A command that read the environment alone would see an empty string for every value already rotated into the store — and for `integrations.gitlab.signing_secret`, empty is the signal to *mint*, so a re-run would replace a working webhook secret at the vendor. With no bootstrap at that path, or one declaring no `secrets.keys`, the run resolves from the environment alone and says so on its first line. The one exception is the operator's own credential (`-admin-token` / `$GITLAB_ADMIN_TOKEN` and its siblings), which is read from the environment only — see [the secret store](../concepts/secret-store.md#what-still-has-to-be-in-the-environment).


> **Every command except `crewlet run` logs at `warn`.** They open a store,
> which logs a migration line per schema file and an open line per call —
> noise on a one-shot command whose stdout is meant to be piped, read or
> diffed. That is a default, not a ceiling: export `CREWLET_LOG_LEVEL=debug`
> (or `info` / `error`) to turn it up, which is exactly what a half-applied
> migration or a failing deploy gate needs. It is an environment variable
> rather than a flag on a dozen commands because it belongs to the
> *invocation* — a CI step exports it once and everything it runs answers. A
> value this build does not recognise resolves to `warn`: a bad log level must
> never be why an operator cannot run a migration, and it must not quietly
> change the default either. `crewlet run` has its own `-log-level` / `-debug`
> flags and its own default of `info`.
## `crewlet run`

```
crewlet run [<config.yaml>] [-company PATH] [-debug]
            [-log-level LEVEL] [-log-format FORMAT]
            [-roles ROLE[,ROLE...]] [-api-host HOST] [-api-port PORT]
```

Reads Tier A bootstrap and starts the agent engine. The path comes from the
**positional argument**, or from `-config`, defaulting to `./crewlet.yaml`.
Naming it both ways is refused: the two would have to agree and nothing checks
that they do. A leftover positional is refused too, rather than ignored —
Go's flag parser stops at the first non-flag token, so a command that took the
path and kept going would silently boot from the default without ever
mentioning the file the operator named.

If the default is missing and a `config.yaml` sits beside it, the error says
so. This repository's own quickstart, its example file and much of its
documentation have called the Tier A document `config.yaml` while the binary's
default is `crewlet.yaml`, and an operator who followed the guide otherwise
gets "no such file" about a name they never typed. It is a **hint, not a
fallback**: silently loading a file nobody asked for is how a node boots from
the wrong document on a machine that has both. Tier B is read from the `company_config` table in the store — if no active revision exists, the engine boots in the **unconfigured** state with the API still serving so an operator can bootstrap via `crewlet config import` or `PUT /config`. See [Configuration concept doc](../concepts/configuration.md).

| Flag | Description |
|------|-------------|
| `-config PATH` | Tier A: this node's broker, store and API (default `./crewlet.yaml`) |
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

Manage the Tier B company configuration in the store. Every subcommand opens the store named by the Tier A bootstrap (`-config`, default `./crewlet.yaml`), and decrypts what it holds with that file's keyring.

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

Prints a **structural** diff of two revisions — one line per path that moved,
with the value on each side. `-against active` (default) compares against the
currently-active revision, and the direction reads as "what `-against` became",
so `crewlet config diff <UUID>` answers "what would reverting to this change".

```
--- active
+++ 35ecd5ab-96ff-4d6d-b101-9de41d644832
~ providers.llm.main.model: "claude-opus-5" -> "claude-sonnet-5"
+ agents.roles[3].handle = "qa"
- integrations.github.enabled (was true)
```

**Paths and values, not lines.** The stored form is JSON produced by
marshalling a struct, so re-ordering a map or adding a field with a default
rewrites lines that mean nothing to a reader. The question an operator asks is
"what changed about the company", and paths answer it — this is the same
differ [`GET /config/revisions/{id}/diff`](api-endpoints.md) serves, and the
shape [d-505](design-decisions.md) records. A string value is quoted and other
values are not, because `"true"` and `true` are different settings and a
renderer that printed both bare would show a type change as no change at all.
A diff longer than the cap reports its own truncation rather than stopping
silently.

**Both sides are always redacted, and there is no flag to turn that off.** A diff is what an operator pastes into a ticket or a chat thread to ask a colleague whether a change looks right, which is the single most likely way a credential leaves the machine. `crewlet config export -revision <UUID>` is there for the rare case that needs the real values, and it takes a deliberate act.

### `crewlet config activate`

```
crewlet config activate <UUID> [-config PATH]
```

Re-points the fleet at a revision. Every node applies it on its next reconcile.

**Re-activating the revision that is already active is not a no-op**, and that is the point: the pointer is append-only, so it mints a new epoch. A node's reconciler skips on the *epoch* it has applied, never on the payload, so the apply always runs — re-reading the [secret store](../concepts/secret-store.md) and rebuilding every provider, transport and MCP child that captured a resolved value. It is the documented way to make a rotated credential take effect on a running fleet.

### `crewlet config seal`

```
crewlet config seal [-config PATH]
```

Encrypts the active revision under the Tier A keyring and writes a new active revision holding the whole config as one opaque `{"__encrypted__": "enc:v1:…"}` document — the one-time migration off plaintext-at-rest. `${VAR}` references inside are kept verbatim and resolve at construction time. A no-op when the active revision is already sealed. Requires a keyring in `crewlet.yaml` (`crewlet secrets keygen`), and says so if there is none.

Like `import`, this writes the revision to **this node's** store; the note it prints says what publishes it to a running fleet.

### `crewlet config rekey`

```
crewlet config rekey [-dry-run] [-config PATH]
```

Rotates the master key: re-encrypts the active revision's config document under the current `secrets.active_key_id`, writing a new revision. The document is decrypted with whatever key sealed it (that key **must still be in** `secrets.keys`) and re-encrypted under the active key.

**Run this and [`crewlet secrets rekey`](#crewlet-secrets-rekey) together.** They are the two halves of one rotation — the company document and the secret store are sealed with the same keyring — and doing only one, then dropping the retired key, makes whatever the other still holds unreadable.

Workflow: `crewlet secrets keygen -key-id <new>` → add the new key to `secrets.keys` and set `active_key_id: <new>` while keeping the old key → `crewlet config rekey` **and** `crewlet secrets rekey` → once both succeed, drop the old key from `crewlet.yaml`.

`-dry-run` reports what would move by reading the key id off the envelope, decrypting nothing. Idempotent: a document already under the active key is skipped and says so. A **plaintext** revision is refused rather than silently sealed — "rotate the key this is under" and "start encrypting this at all" are different decisions, and the refusal points at `config seal`. Fails clearly, naming the key, if the document is sealed under one no longer in the keyring.

---

## `crewlet secrets`

### `crewlet secrets keygen`

```
crewlet secrets keygen [-key-id ID]
```

Prints a fresh base64 32-byte encryption key plus a copy-pasteable `crewlet.yaml` `secrets:` snippet that references it via an environment variable (keeping the raw key out of the file). `--key-id` (default `key-1`) names the key; the id is stamped into every envelope the key seals, so keep it stable across restarts and pick a new id only when rotating. Key generation is always explicit — Crewlet never auto-generates a key, because a silently-generated key that isn't captured makes every backup unrecoverable. See [Secrets](../concepts/configuration.md#secrets).

The remaining subcommands operate on the [secret store](../concepts/secret-store.md) — the company's encrypted credentials, one sealed value per `${VAR}` name, which the engine consults **ahead of** the process environment. All of them read the Tier A bootstrap (`-config`, default `./crewlet.yaml`) for the keyring, and all of them need one: the store has no plaintext mode, so a config declaring no `secrets.keys` is refused with a pointer at `keygen` rather than silently storing plaintext.

**Which store they reach depends on whether the engine is running**, and the command says which it used. The rows live on the coordination KV so every node reads them, and on the default topology that KV is inside the engine's own process — so a running node is written through its authenticated `/secrets` API, and a stopped one falls back to its own local table, which the engine migrates onto the fleet at its next start. The engine's exclusive database lock is what tells the two apart, with a pid attached.

`-api URL` names the node to write through, for running the command from a machine that is not the node. The bearer token comes from `CREWLET_API_TOKEN` when set, and otherwise from the first `api.auth.tokens` entry in the Tier A config; the token's id is recorded as the author of the write.

### `crewlet secrets set`

```
crewlet secrets set <NAME> [-value V] [-source STR] [-config PATH] [-api URL]
```

Stores one encrypted secret under `NAME`, which must be a valid environment-variable name — a name outside the `${VAR}` grammar could never be read back, so it is rejected. Existing names are replaced.

The value comes from **stdin** by default (`echo "$TOKEN" | crewlet secrets set GITLAB_TOKEN_SWE`), or from an interactive prompt on a terminal. `-value` exists for scripted use, but an argv value is visible in `ps` and lands in shell history, so prefer stdin. `-source` records provenance (default `cli`); the provisioning CLIs stamp their own. The **author** is the Tier A token's id when the write goes through a running node, and `$CREWLET_OPERATOR`/`$USER` when it goes to the local table.

A running engine picks the new value up at its next config activation or restart, not immediately — the command says so after each write, along with which of the two stores it wrote.

### `crewlet secrets list`

```
crewlet secrets list [-config PATH] [-api URL]
```

Prints one row per stored secret: name, sealing `key_id`, last-updated timestamp, who wrote it, and source. It names the store it read first, because a stopped node's own empty table and a fleet with nothing in it look identical otherwise. **Never** prints a value — the listing drops the envelope on the way out and the route behind it has no value field at all. Works without a keyring on the fleet's store, so an operator locked out of the key can still take inventory.

### `crewlet secrets unset`

```
crewlet secrets unset <NAME> [-config PATH] [-api URL]
```

Removes the record and says whether one was there — "was not set" is the outcome a cleanup script wanted on its second run, not a failure. Afterwards `${NAME}` falls back to the environment.

### `crewlet secrets get`

```
crewlet secrets get <NAME> -reveal [-config PATH] [-api URL]
```

Prints one decrypted value to stdout, with no trailing newline so it can be piped. `-reveal` is **required** — without it the command refuses and reads nothing. The route behind it (`GET /secrets/{name}`) is gated the same way, by an explicit `?reveal=true` no crawl reaches by accident, answers `Cache-Control: no-store`, and logs the access against the operator its guard authenticated. Intended as break-glass for recovering a credential the upstream API will never show again, not as a scripting interface.

### `crewlet secrets rekey`

```
crewlet secrets rekey [-dry-run] [-config PATH] [-api URL]
```

Re-encrypts every stored secret not already sealed under `secrets.active_key_id`, and prints the names it moved. The per-record counterpart of [`crewlet config rekey`](#crewlet-config-rekey) — run **both** before dropping a retired key from `secrets.keys`, or records still sealed under it become unreadable. Each envelope names its sealing key, so mixed-key states decrypt correctly throughout. `-dry-run` lists what would re-encrypt without decrypting anything, reading the denormalised `key_id` instead.

The store is shared, so this is run **once for the fleet**, not once per node. A run through a node's API sends the key id it expects and is **refused with a 409** if that node seals under a different `active_key_id` — a silent success there would report a rotation the fleet did not make. It aborts rather than half-completing if any record cannot be opened with the keyring in hand, because retiring the old key on the strength of a partial pass is what makes a secret unreadable for ever.

---

## `crewlet validate`

```
crewlet validate [<file.yaml>] [-tier auto|company|bootstrap] [-json]
crewlet validate [-config <tier-a.yaml>] [-company <tier-b.yaml>] [-json]
```

Validates a config and prints a summary, reaching nothing: no broker, no
store, no provider is dialled.

**Two forms, because it has two jobs.** Name **one file** and it validates
that document. Name **neither** and it validates both tiers together, from
`-config` and `-company` — which is what a CI step wants, and which reports
*both* tiers' problems rather than stopping at the first: an operator fixing a
broker URL only to be told about their org chart on the next boot has been
made to pay twice for one edit.

A **leftover positional is refused**, not ignored. Go's flag package stops at
the first non-flag token, so a command that took the file and kept going would
silently validate the defaults instead and print a success line about files it
never opened.

Validation is **deep** — it builds the `Organization`, so unknown unit
leads, bad cron expressions, invalid timezones, human seats missing a
contact identity, and the Confluence-XOR-Plane rule all fail here rather
than at run time. It reads **no environment**: Tier B keeps `${VAR}`
references verbatim, so a config validates fully before any secret
exists.

| Flag | Description |
|------|-------------|
| `-tier` | Which tier a positional file is. `auto` (default) reads the document's **keys**, not its filename — the two tiers share no top-level key, so `name`/`agents`/`providers` mean Tier B and `node`/`stream`/`store`/`coordination` mean Tier A. A document that carries neither, or an equal count of both, is **refused naming this flag** rather than guessed at: guessing wrong reports every field of the file as invalid, and an operator reading that cannot tell it from a genuinely broken document. |
| `-json` | Emit a machine-readable result on stdout instead of prose. |
| `-config` / `-company` | The two-tier form. Ignored when a positional file is given. |

With `-json`, the payload is `{"valid": bool, "tier": str, "file": str,
"errors": [{"path", "type", "message"}], "summary": {...}}` — one record per
offending field, with its exact path, so an editor, CI job, or [AI authoring
loop](../getting-started/ai-authoring.md) can fix everything in one pass:

```json
{
  "valid": false,
  "tier": "company",
  "file": "company.yaml",
  "errors": [
    { "path": "agents.roles[0].llm", "type": "unknown_value",
      "message": "no provider named \"nonexistent\" is configured" }
  ]
}
```

`type` is one of `missing`, `out_of_range`, `conflict`, `shape`,
`unknown_field`, `unknown_value`, or `invalid` for anything this build does
not classify — a closed set with a fallback, because a consumer branching on
it must never receive an empty string and read it as a field somebody forgot.

Exit code is `0` when valid and `1` otherwise, **in both output modes**: a
`-json` run that printed `{"valid": false}` and exited zero would pass every
CI gate built on `crewlet validate x.yaml -json || exit 1`. In `-json` mode
nothing is echoed on stderr, because the payload already carries every problem
and a second copy is what makes a machine consumer's log unreadable.

---

## `crewlet migrate`

Applies pending schema migrations. Every process that opens the store
migrates it, so this is never *required* — what it is, is a way to do it
without starting a node.

```bash
crewlet migrate                          # uses ./crewlet.yaml
crewlet migrate /etc/crewlet/crewlet.yaml
crewlet migrate -check                   # report pending work, apply nothing
```

| Flag | Description |
|------|-------------|
| `config` | Tier A YAML (positional, or `-config`; default `./crewlet.yaml`) |
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

Token-budget usage lives in the fleet's [coordination store](../concepts/coordination.md):
one counter for the whole company, surviving restarts. A counter each node
kept privately would make an org cap of 500k into N × 500k.

**These commands talk to a running node**, not to a file. That follows from
where the counter lives: on the default topology the coordination store is the
engine's own embedded broker, so there is nothing on disk to open — and opening
it anyway would be worse than useless, because a second broker on the same
store directory is accepted rather than refused, and two writers on one store
is corruption rather than contention.

```bash
crewlet budgets show                       # usage per scope
crewlet budgets reset                      # zero every scope
crewlet budgets reset -scope org           # just the org
crewlet budgets reset -scope agent:<id>    # just one seat
```

| Flag | Default | What it does |
|---|---|---|
| `-url` | the `api` block of the config named on the command line | The running node's base URL. A wildcard bind (`0.0.0.0`, `::`) becomes the loopback address, because a wildcard is not something anything can dial |
| `-token` | `$CREWLET_API_TOKEN`, then the config's first `api.auth.tokens` entry | The bearer token. `reset` is a write, so it always needs one — `allow_anonymous_read` opens reads and nothing else |

The environment wins over the config so an operator who exported a token
deliberately gets that one. There is no token *default* on the command line:
a token typed as an argument is in the shell history, in `ps`, and in any CI
log that echoes the command.

The **caps** are not stored here — they come from the active company config
(`token_budget` on the org, `role.token_budget` on a seat), so every process
derives the same numbers without coordinating. Only the usage is shared.

`show` refuses rather than printing zeros when the node reports it could not
read the counter (`durable: false` on the query surface). A counter nobody
could look at is not a counter that reads zero, and a table of zeros draws a
company at 0% of its budget at exactly the moment nothing is known. Seats with
no cap and no spend are left out for the same reason in reverse: a permanent
zero row per seat buries the seats that matter.

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

## `crewlet llm`

```
crewlet llm list
crewlet llm doctor [KEY] [-no-smoke]
crewlet llm login  <KEY> [-from-host | -capture-token | -token-stdin |
                          -username U -password-stdin] [-home PATH]
                          [-print-token]
crewlet llm status <KEY>
crewlet llm logout <KEY>
crewlet llm export <KEY> [-secret-store]
crewlet llm import <KEY>          # bundle on stdin
```

The operator side of a [subscription LLM backend](../concepts/subscription-llm-backends.md):
a `providers.llm` entry of `type: cli-agent` drives a vendor's own CLI under
the operator's Pro/Max plan instead of an API key, and the login that makes
that work is established here rather than in the config document. `KEY` is the
`providers.llm` key; commands that take one and are given none act on the only
`cli-agent` provider when there is exactly one.

**`list`** is the inventory — provider key, CLI, model and whether it is
logged in. **`doctor`** is the one to run before a company's first turn: it
checks the CLI is installed, the credentials answer, and — unless you pass
`-no-smoke` — that a real completion comes back, which is the only check that
catches a plan whose quota is exhausted.

**`login`** has four shapes because the vendors do:

| Shape | When |
|---|---|
| *(no flag)* | Broker the vendor's own interactive login and keep the result |
| `-from-host` | Adopt a login this machine already has, e.g. from running the CLI by hand |
| `-capture-token` | Mint a **headless token** into the secret store — the only shape a remote [code sandbox](../concepts/code-sandbox.md) can use, because a token is one scoped revocable variable and credential *files* never leave the engine host |
| `-capture-token -print-token` | The same mint, written to **stdout** and stored nowhere — for an operator whose secrets live in somebody else's manager. It **refuses to run on a terminal**: this is a credential, and a token in a scrollback outlives the command, while a screen-share or a shell history outlives the scrollback. Pipe it or redirect it. The two-step alternative (`-capture-token`, then `secrets get -reveal`) writes the token into the store on the way past, which is precisely what this avoids. |
| `-token-stdin` / `-username U -password-stdin` | Store a credential you already hold, where the CLI genuinely has one |

**`export`** packs a login into one portable blob so another host can come up
already authenticated. `-secret-store` writes it to the [secret
store](../concepts/secret-store.md) instead of stdout; without it the blob goes
to stdout in the clear, which is what you want when piping into your own
secret manager and never what you want in a shell history.

**`import`** is the other half, and it reads the bundle from **stdin** — a
credential on argv is visible in `ps` and lands in shell history, and the
natural spelling is a pipe anyway:

```bash
crewlet llm export claude | ssh other-host crewlet llm import claude
```

It **refuses to overwrite an existing login**, loudly: a host that has been
running holds the fresher refresh token, and restoring a boot-time blob over
it is how a fleet logs itself out. `crewlet llm logout <KEY>` first if you
mean to replace it.

The engine also restores a bundle **itself**, at boot, from
`cli.auth.credential_bundle` or the conventional
`CREWLET_LLM_CLI_<KEY>_CREDENTIALS` variable — and only into an empty
credential directory, by the same rule. That path authenticates a fresh
container before its first turn rather than after an operator remembers a
command; `import` is for the hosts that are already up. See [environment
variables](environment-variables.md).

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
| `-secret-store` / `-env-file PATH` / `-print` | Where minted credentials go — exactly one, and there is no default. See [the secret store](../concepts/secret-store.md). `-print` writes `export VAR=…` lines and, when a run rolls back, `unset VAR` for each — the stream is meant to be sourced, and a comment is a no-op to a shell, so an operator who piped it into `source` would otherwise keep a revoked token exported. |
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

The title comes from the H1 rather than the filename because it is the page name *and* half the idempotency key — a rename would orphan the published page and leave a second one beside it. Frontmatter may override the title (`title:`) and the container (`project:` / `space:`), and nothing else — **Plane pages have no parent chain and no labels**, so a `parent:` or `labels:` written for the other backend is reported once per run naming the files, and the pages are published at the project root without them. A note rather than a refusal: the content is right and the pages belong in the workspace; only their position and their labels are lost. A file with no determinable title, or two files that would publish as one page, **stop the walk** naming the fix: both are things an operator corrects in their editor, and a run that skipped them would report success with a skill silently unpublished.

**Idempotency is the fork's `external_id` contract**: every published page is stamped `external_source="crewlet"` and `external_id="skill:<key>"` / `"doc:<title>"`, so re-runs match by identity and retitling a page in Plane never orphans it. A re-import always writes — this is a publisher, and skipping existing pages would mean an edited file never reaching the workspace. A page created by hand under the same title is adopted and stamped, but **only** if it carries no external identity at all: one that does belongs to whoever set it. Where two unclaimed pages share a title the lowest page id wins, because Plane guarantees no enumeration order and the alternative is a coin flip.

Every distinct target project must already exist; a missing one fails the run **before any page is written**, naming what the workspace has. The importer never creates projects — that is [`crewlet plane provision -create-projects`](#crewlet-plane-provision). Page-write failures are isolated per page: the rest of the run publishes, then the command **exits non-zero naming the failures**.

| Flag | Description |
|------|-------------|
| `-token KEY` | A Plane API key that may write the target projects. Empty reads `$PLANE_TOKEN`, then `integrations.plane.token`. The account must be a member of every target project. |
| `-project ID` | Publish tool skills into this project instead of `integrations.plane.skills_project`. Empty reads `$CREWLET_TOOL_SKILLS_PROJECT`, then the config field. Skill files only — a knowledge doc takes its project from its parent directory. A company that has turned tool skills off (`skills_project: ""`) and has a skill file in the tree **stops the walk** naming both the setting and this flag. |
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
| `-secret-store` / `-env-file PATH` / `-print` | Where minted credentials go — exactly one, and there is no default: a run with nowhere to put what it mints creates live credentials at the vendor and prints none of them. See [the secret store](../concepts/secret-store.md). `-print` writes `export VAR=…` lines and, when a run rolls back, `unset VAR` for each — the stream is meant to be sourced, and a comment is a no-op to a shell, so an operator who piped it into `source` would otherwise keep a revoked token exported. |
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
                                        [-public-url URL] [-mode group|instance]
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
| `-decommission` | Delete service accounts whose seats have left the config. Scoped **twice**: the username must start with `provisioning.username_prefix` (never empty — it defaults to `crewlet-`) *and* the account must be a member of this company's group, because either alone is too broad. That group scan is used in **both** modes and it is deliberate: every seat is made a member of `provisioning.group` whatever created it, so a managed account of this company is always in the group — while the instance's own service-account listing also holds every *other* company's accounts on the box, which a shared prefix would then sweep. Only the DELETE route differs by mode; sending one down the wrong route answers "no such account" for one that is still live. An account the instance refuses to delete because it is not a service account is reported rather than aborting — that refusal is GitLab catching what the scan should not have proposed, so it is a signal about the prefix. |
| `-mode group\|instance` | Where service accounts are **owned**. `group` (the default, and all GitLab.com offers) creates them under `provisioning.group`; `instance` creates them on the instance itself, which needs an **instance-administrator** PAT and buys an account that is a member of nothing until this run adds it — so one company's seats can span several top-level groups, and an account survives its group being deleted. Everything downstream is identical: memberships are added the same way, and the token API is user-scoped on both. An unknown value is refused before the config is loaded. |
| `-token-expiry-days` | Lifetime minted onto each token. Omitted, no `expires_at` is sent and the instance's own policy applies — GitLab.com caps personal access tokens at a year regardless, which is the instance enforcing its policy rather than this tool choosing one. Nothing in Crewlet renews a credential on a schedule, so a lifetime nobody renews is an outage with a date on it. |
| `-dry-run` | Print the plan and touch nothing. It is the **same** plan the run uses. |

An account this run created is rolled back by revoking every token on it — nothing else has ever minted there. On an account that already existed, only the token this run minted is revoked: sweeping it would take an administrator's own token with no way to tell that it had. Both go through a detached context, because the failure is often the cancellation itself.

See [GitLab Integration — Provisioning](../integrations/gitlab.md#provisioning) for the permission matrix and the full walkthrough.

## `crewlet jira provision`

```
crewlet jira provision <company.yaml> [-secret-store | -env-file PATH | -print]
                                      [-public-url URL]
                                      [-recreate-webhook] [-dry-run]
```

The Atlassian tracker's reconcile, and it is a different shape from its peers: **Jira issues no credentials on a provisioner's behalf.** A Cloud API token is created by the person it belongs to at Atlassian's own account site, and a Data Center personal access token can only be minted for the calling user — so a command that offered to provision accounts would be printing instructions dressed as actions. What it does instead is the three things Jira genuinely allows, each answering a question that is otherwise invisible until an issue reaches nobody:

1. **Which account each seat's credential authenticates as.** It calls `/myself` with every seat's own token, read from `mcp_env.atlassian` or `mcp_env.jira`, and reports the seats that resolved and the seats that did not. A seat with no account id receives **no Jira events at all** — the routing gate drops every target that names it — and nothing else in the engine says so. Human seats are never probed: they hold no tool credential and are reached by `contact.atlassian_account_id` or by email.
2. **Whether every project the org chart names exists**, and whether Jira's own project lead agrees with the org chart's. A key with a typo in it is a routing gap that produces no error anywhere; a disagreement about the lead is reported and never failed, because a human manager owning the project while an agent triages it is an ordinary arrangement.
3. **The inbound webhook**, on Data Center only, at `<public-url>/webhooks/jira` — subscribed to exactly the events the parser routes, delivering the whole body, signed with an HMAC secret. On **Cloud** the step is skipped with a note rather than attempted: a dynamic webhook there belongs to an app, so the endpoint refuses an API token however privileged it is, and reporting that 403 would send an operator to rotate a credential that is fine. Cloud events arrive through the [Forge app](../integrations/jira.md#jira-cloud--forge-app) instead.

**A working webhook secret is never replaced.** If `integrations.jira.webhook_secret` already resolves, that value is registered as-is. Minting on every run would be an outage: the engine holds the *old* secret, so the instance would start signing every delivery with a key nothing can verify. A secret that resolves to nothing is minted into the `${VAR}` the config points at and recorded in the sink you chose; `-recreate-webhook` forces a rotation, which invalidates the secret every other deployment of this company holds.

| Flag | Description |
|------|-------------|
| `-secret-store` / `-env-file PATH` / `-print` | Where a minted webhook secret goes — exactly one, and there is no default. See [the secret store](../concepts/secret-store.md). |
| `-public-url` | This deployment's public base URL; the webhook is registered at `<url>/webhooks/jira`. Omit to skip registration — a hook pointing at the wrong host is worse than none, because the instance then reports a healthy integration that delivers into the void. |
| `-recreate-webhook` | Delete and remake the hook to mint a fresh secret. The only recovery for a secret that was lost, because the value cannot be read back off the hook — and destructive for every other deployment holding the old one. |
| `-dry-run` | Read the instance and report; register nothing. The sink is not opened, so it prompts for no passphrase. |

A hook somebody else registered is reported and never touched: an instance may carry integrations that are none of this run's business, and taking over the first one found by name would break one.

The run refuses outright when the org credential in `integrations.jira.token` is rejected — nothing else it reported would be trustworthy.

See [Jira Integration — Provisioning](../integrations/jira.md#provisioning) for the full walkthrough.

## `crewlet github provision`

```
crewlet github provision <company.yaml> [-secret-store | -env-file PATH | -print]
                                        [-public-url URL]
                                        [-recreate-webhooks] [-dry-run]
```

The hosted code host's reconcile, and it is the same shape as Jira's for the same reason: **GitHub issues no credentials on a provisioner's behalf.** There is no API that creates a user, and the API that once minted a token for somebody else was withdrawn in 2020 — so a command that offered to provision accounts would be printing instructions dressed as actions. What it does instead is the two things GitHub genuinely allows:

1. **Which account each seat's credential authenticates as.** It calls `GET /user` with every seat's own token, read from `mcp_env.github` under whichever key that seat's tools use (`GITHUB_TOKEN`, `GITHUB_PERSONAL_ACCESS_TOKEN`, `GH_TOKEN`, or an `Authorization` header), and reports the seats that resolved and the seats that did not. A seat with no login receives **no GitHub events at all** — the routing gate drops every target that names it — and nothing else in the engine says so. Human seats are never probed: they hold no tool credential and are reached by `contact.github_login`.
2. **The inbound webhooks**, at `<public-url>/webhooks/github`, subscribed to exactly the events the parser routes and delivering JSON — GitHub's own default is form-encoded, which nothing here can decode. One hook on the **organization** where the credential may register it, covering every repository in the org including ones created later; otherwise one per repository named in `provisioning.repos`. `org_webhook: true` turns a credential that cannot into a failed run rather than a silent fallback, because an operator who asked for the org arrangement must not quietly get the other one. `admin:org_hook` is the scope, which a fine-grained token cannot carry at all.

**A working webhook secret is never replaced.** If `integrations.github.webhook_secret` already resolves, that value is registered as-is. Minting on every run would be an outage: the engine holds the *old* secret, so GitHub would start signing every delivery with a key nothing can verify. A secret that resolves to nothing is minted into the `${VAR}` the config points at and recorded in the sink you chose; `-recreate-webhooks` forces a rotation, which invalidates the secret every other deployment of this company holds.

**One unhookable repository does not stop the rest.** A list will contain one that was renamed, archived, or made private to a team this credential is not in; each is reported with what is wrong and the others are still hooked. GitHub answers **404 for both "absent" and "invisible to this credential"** — deliberately, so a probe cannot enumerate what exists — so the report says both rather than sending an operator to look for a repository that is right there.

| Flag | Description |
|------|-------------|
| `-secret-store` / `-env-file PATH` / `-print` | Where a minted webhook secret goes — exactly one, and there is no default. See [the secret store](../concepts/secret-store.md). |
| `-public-url` | This deployment's public base URL; the hooks are registered at `<url>/webhooks/github`. Omit to skip registration — a hook pointing at the wrong host is worse than none, because GitHub then reports a healthy integration that delivers into the void. |
| `-recreate-webhooks` | Delete and remake every hook to mint a fresh secret. The only recovery for a secret that was lost, because the value cannot be read back off a hook — and destructive for every other deployment holding the old one. |
| `-dry-run` | Read GitHub and report; register nothing. The sink is not opened, so it prompts for no passphrase. |

A hook somebody else registered is left alone: matching is on the **delivery URL**, never on a name, because GitHub gives a webhook no name at all and an organization carries hooks other integrations put there.

The run refuses outright when the credential in `integrations.github.token` is rejected — nothing else it reported would be trustworthy. That token is optional for the *engine* (without it, thread activity degrades to the payload's author and assignees) and required here, because there is no degraded form of registering a webhook.

See [GitHub Integration — Provisioning](../integrations/github.md#provisioning--crewlet-github-provision) for the full walkthrough.

## `crewlet slack provision`

```
crewlet slack provision <company.yaml> [-secret-store | -env-file PATH | -print]
                                       -public-url URL
                                       [-config-token TOKEN] [-ledger PATH]
                                       [-handles a,b] [-reinstall]
                                       [-no-install] [-dry-run]
```

Every other vendor's provisioning runs unattended. **Slack cannot**, and that is not an implementation gap: installing an app into a workspace is an OAuth grant, and OAuth exists precisely so that a person decides. So the run creates and updates the apps by itself, then hands the operator one authorize URL per seat and takes the code back — and where there is nobody to ask (`-no-install`, `-dry-run`), it prints the URLs and stops rather than pretending.

For each seat whose `integrations.slack` credentials are whole `${VAR}` references it builds the canonical manifest ([`internal/slack`](../integrations/slack.md#bot-scopes-and-events) is the single source of truth for the scopes and events), creates or updates the app, records the **signing secret** into the variable `signing_secret` points at, and — after the click — the **bot token** into the one `bot_token` points at. A seat whose credentials are literals is one an operator manages by hand: it is reported and skipped, never rewritten.

**An unchanged manifest is not pushed.** The manifest methods are Slack's slowest rate class, roughly one request a minute, so a company of seven re-running would otherwise spend minutes achieving nothing. The ledger holds a fingerprint of the last manifest Slack accepted, and a re-run compares against it.

**One seat's failure does not cost the others.** A mistyped code paste or a refused manifest is recorded against that handle, the remaining seats still provision, and the command exits non-zero naming what failed. Everything completed is durable — the ledger is written after every mutation — so a re-run resumes.

**The app-configuration token is persisted before it is used.** Slack's rotation is single-use in both directions: the call that returns a new refresh token invalidates the one it was given. A run that rotated and then failed to record the result would lock the operator out of their own apps, so the pair is written to the ledger first, and a still-valid access token is reused rather than rotated again. For the same reason **the ledger's pair beats `-config-token` and `$SLACK_CONFIG_REFRESH_TOKEN`**, which is the reverse of the usual rule: a value left in a shell export is dead the moment the first run used it, and preferring it would trade the only live pair for a retired one on every run after. The flag and the variable are a bootstrap for a ledger that holds nothing.

**A recorded app that no longer exists is replaced, not reported kept.** Its manifest fingerprint still matches, so without a probe the seat reads as healthy while the bot token in its `${VAR}` authenticates as nothing. Every run validates each recorded app id first — one call, no write — and reads `app_not_found` / `invalid_app_id` / `invalid_app` as gone; a permission refusal is not an absence and never triggers a replacement, because an app this credential may not touch still exists and replacing it would leave two.

**A code that belongs to another app is refused and records nothing.** One authorize URL is printed per seat and they look alike; pasting the wrong one would mint a colleague's bot token into this seat's variable, and the seat would post as them with nothing reporting it.

| Flag | Description |
|------|-------------|
| `-public-url` | Public HTTPS base URL of this deployment. **Required**: every app's Events API request URL and OAuth redirect URL are built from it, so an app created without one delivers nowhere and cannot be installed. |
| `-secret-store` / `-env-file PATH` / `-print` | Where the minted bot token and signing secret go — exactly one, and there is no default. See [the secret store](../concepts/secret-store.md). |
| `-config-token` | The operator's app-configuration **refresh** token, from [api.slack.com/apps](https://api.slack.com/apps) → Your App Configuration Tokens. Falls back to `$SLACK_CONFIG_REFRESH_TOKEN`. Read from the environment alone, never from the secret store: it is the operator's credential rather than the company's. |
| `-ledger` | The app ledger (default `slack-apps.json` beside the company document). It holds the client secrets Slack returns only at creation, so it is written `0600` — gitignore it like `.env`. |
| `-handles a,b` | Only provision these seats. Worth having against a method that allows about one request a minute. |
| `-reinstall` | Redo the OAuth install even where a token is already recorded. **Required for a scope change to take effect** — a bot token carries only the scopes it was minted with — and destructive: the new install revokes the token every running node is authenticating with. |
| `-no-install` | Create and update the apps and record the signing secrets, then print the authorize URLs instead of asking for codes. For a non-interactive run. |
| `-dry-run` | Print the plan and **check every manifest** through `apps.manifest.validate`, which writes nothing: no app created, no manifest pushed, no install run, and the sink is not opened either, so it prompts for no passphrase. That check is why a dry run touches the network at all — `apps.manifest.create` is Tier 1, roughly one request a minute, so a malformed manifest discovered from the create costs a minute per seat and leaves the seats before the bad one already created. Validating needs a config token, so a dry run that cannot get one prints the plan and says the manifests were not checked; getting that token may rotate it, which is the one write a dry run makes and has to. |

Run the API server first, publicly reachable at `-public-url`: Slack verifies each app's request URL with a `url_verification` challenge, which the edge answers unconditionally — it has to, because during provisioning the signing secret does not exist yet and a verified handshake would be impossible.

See [Slack Integration](../integrations/slack.md#automated-setup-crewlet-slack-provision) for the full walkthrough.

## `crewlet confluence import`

```
crewlet confluence import <company.yaml> <directory> [-space KEY] [-prune]
    [-config PATH] [-dry-run]
```

Publishes a tree of authored markdown into Confluence. **One walk, two destinations, decided by the file**: a file whose frontmatter declares a `trigger:` is a [tool skill](../concepts/tool-skills.md) and goes to `integrations.confluence.skills_space` with the leading code block the engine parses back out; everything else is a knowledge doc, published as prose into the space its parent directory names, titled by its first `# H1`.

The routing is the FILE'S, not the directory's, because a skill is identified by what it declares — an operator who files one under `ENG/` still means a skill, and publishing it there as prose would put an instruction meant for one phase of one turn into every planner's context.

**Every target space is checked before a single page is written.** A typo in a directory name would otherwise be discovered half way through, leaving an operator to work out which pages landed. The importer never *creates* a space: that names a container the whole company then works in, and it is not this command's guess to make.

**A page that already exists is updated in place**, matched by title within its space. Confluence has no external-id field, so a page somebody renamed in the UI is orphaned and a re-import creates a second one. That is the backend's limitation, reported rather than worked around: a marker page or a label pressed into service as an *identity* would be this tool inventing a second answer to a question Confluence already answers, and the two would disagree the first time somebody moved a page.

**Nesting and labels come from frontmatter.** Frontmatter may also declare a `parent:` — the **title** of a page in the same space to nest this one under, which is the one thing a flat directory of files cannot say about a wiki that has trees in it — and `labels:`, the author's own page labels. The plan is ordered parents-first so a `parent:` naming a page **published by the same run** resolves; a cycle stops the walk naming the files, and a parent nobody publishes is a note and a page at the space root, because a doc nobody can read is worse than a doc in the wrong place. **An existing page is never re-parented** — where a page sits is something people move deliberately, and a run that dragged it back every time would be fighting them with no way to say so. Labels are attached on every run, not only on create, because the server call is idempotent and a label an author adds to a file that already publishes has to reach the page somehow; a label that will not attach is a note, not a page failure.

**Provenance is a different question, and it is recorded.** Every skill page this command writes gets the global label `crewlet-skill`. That says nothing about *which* page a file belongs to — it says only that the importer wrote it, which is a fact no field on the page carries and which only the writer can know. `-prune` is the one caller that needs it: an orphaned page is deleted only if this tool published it, because a lead who authored a skill by hand in the wiki has no local `.md` and would otherwise lose their work on the next import. A label that cannot be written is a note, not a page failure — the page is published and correct, and what is lost is the ability to prune it later.

**Page failures are isolated**: a restricted page or one 403 does not cost the other forty. The run reports what failed and exits non-zero.

| Flag | Description |
|------|-------------|
| `-space KEY` | Publish tool skills into this space instead of `integrations.confluence.skills_space`. Empty reads `$CREWLET_TOOL_SKILLS_SPACE`, then the config field. Skill files only — a knowledge doc takes its space from its parent directory. |
| — | Frontmatter on a knowledge doc may declare `parent:` (the title of a page in the same space to nest under) and `labels:` (the author's own, lower-cased and de-duplicated because that is what Confluence stores). See below. |
| `-prune` | After publishing, delete skill pages in the skills space that carry the `crewlet-skill` label and whose key no local file publishes any more. **Three conditions, all required**: in the skills space, labelled, and parsing as a skill whose key this run's tree does not publish — the label protects a hand-authored page, the parse protects an ordinary page filed in the same space, and the key comparison is what makes a renamed skill a delete-and-create rather than a silent duplicate. The orphan set is derived by subtraction, so **a prune that cannot enumerate the space deletes nothing** and fails the run: a partial read would make the orphan set larger and delete live pages. The set is taken from the *plan*, not from the writes that landed, so a page whose update happened to 403 is never deleted as an orphan of itself. A page that declares a trigger and does not parse has an unknown key and is reported rather than deleted. |
| `-config` | Tier A config naming this node's store and keyring, for resolving the `${VAR}`s in the company's `confluence:` block. |
| `-dry-run` | Print the plan and write or delete nothing. |

A company that has turned tool skills off (`integrations.confluence.skills_space: ""`) has no space for a skill file to go to: a tree containing one **stops the walk** naming both the setting and `-space`, rather than filing an instruction meant for one phase of one turn into a space every planner searches.

See [Confluence Integration](../integrations/confluence.md#publishing-local-pages-from-your-machine-cli).

---

## `crewlet confluence resync`

```
crewlet confluence resync <company.yaml> [-space KEY] [-config PATH]
```

Runs the engine's **own** [tool-skill](../concepts/tool-skills.md) walk of the
Confluence skills space against a throwaway registry and prints what admitted,
so you can see what the next boot will see. The counterpart of
[`crewlet plane resync`](#crewlet-plane-resync), and it exists for the same
reason: the registry is populated by one walk at boot, so a page that fails to
admit is **invisible** — the only symptom is guidance that never appears in a
Plan prompt.

```
TS holds 12 page(s): 9 skill(s), 3 ordinary page(s).
  deploy                       Cutting a release
  incident-review              Running a post-incident review
```

A page that declares a `trigger:` and does **not** parse is printed separately
and exits non-zero. That is the case worth failing on: somebody wrote a trigger
and got the rest wrong, and counting it as an ordinary page is exactly how a
skill goes missing unnoticed.

`-space` overrides `integrations.confluence.skills_space`, for checking a space
the company document does not name yet.

**Skills only.** Knowledge docs are searched live at query time and never
loaded into a registry, so for them there is nothing to resync.

**It does not reach into a running engine.** Restart it, or wait for the next
webhook, to apply what you changed.
