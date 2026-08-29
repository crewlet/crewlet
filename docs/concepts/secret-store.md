# Secret Store

The **secret store** is an encrypted table (`secret_values`) that answers `${VAR}` references, consulted ahead of the process environment. It gives Crewlet a place to *keep* a secret that the engine can read back — so a provisioner that mints a credential can hand it straight to the engine instead of writing a file a human must source.

It is optional and inert until used. With nothing stored, `${VAR}` resolution is byte-for-byte what it has always been: the process environment.

> Related: [Configuration § Secrets](configuration.md#secrets) covers whole-config encryption at rest — a *different* mechanism with the same keyring. See [Which one do I want?](#which-one-do-i-want) below.

---

## The problem

`${VAR}` substitution has exactly one choke point in the engine (`config._resolve_env_value`), and its only source used to be `os.getenv`. The database stores the *reference* (`"${GITLAB_TOKEN_SWE}"`); only the process environment could answer it.

That is why the provisioning CLIs write `.env` files: the environment was the only address the reader knew. It leaves an awkward seam in an otherwise automated flow —

```
crewlet gitlab provision company.yaml    # mints PATs → .env.gitlab
source .env.gitlab                       # ← a human, in the right shell
crewlet run                              # ← restarted, in that same shell
```

— and every step in the middle is a way to get it wrong. The failure is silent: a `${VAR}` nothing answers resolves to `""`, which produces an empty Bearer token that looks live to every layer until the provider rejects it.

## The design

One row per environment-variable name, each value sealed with the Tier A keyring:

```
secret_values
  name        TEXT PRIMARY KEY   -- "GITLAB_TOKEN_SWE"
  value       TEXT               -- "enc:v1:<key_id>:<base64>"
  key_id      TEXT               -- which keyring entry sealed it
  updated_at  TIMESTAMPTZ
  updated_by  TEXT
  source      TEXT               -- "cli" | "gitlab-provision" | ...
```

At boot the engine loads every row into a process-local snapshot and installs it as the **secret source**. From then on `_resolve_env_value` asks the store first and falls back to the process environment:

```mermaid
flowchart LR
    REF["<b>${GITLAB_TOKEN_SWE}</b><br/>config._resolve_env_value"]
    STORE{"secret store<br/>(boot snapshot)"}
    ENV{"process env"}
    VAL["the value"]
    EMPTY["<b>&quot;&quot;</b><br/>(silently empty)"]
    REF --> STORE
    STORE -->|hit| VAL
    STORE -->|miss| ENV
    ENV -->|hit| VAL
    ENV -->|miss| EMPTY
```

Because substitution has a single choke point, that one change covers everything downstream: LLM API keys, per-role `mcp_env`, sandbox env, webhook secrets, contact identities, knowledge-search tokens.

### Store wins; the environment is the fallback

Deliberately, and not negotiable: env-first would let a stale `.env` shadow a freshly rotated secret, which is exactly the failure this removes. A rotation would appear to succeed and then quietly not take effect, surfacing days later as an auth error far from its cause.

When a name exists in both with **different** values, boot logs `secret_shadowed_env` at WARNING with the names (never the values). That is a cue to delete the stale export, not a problem in itself.

### A keyring is required

Unlike `company_config` — which supports a plaintext mode so pre-encryption deployments keep working — `secret_values` has **no plaintext mode**. There is no legacy corpus to stay compatible with, and a store whose whole purpose is holding secrets should not be able to hold them in the clear.

Each value is sealed with AES-256-GCM and the row's own `name` bound in as associated data, so a ciphertext moved to another row fails to decrypt rather than silently impersonating a different secret.

### Two things that can never live in the store

The **DSN** and the **keyring** itself. Tier A carries exactly what is needed to *open and decrypt* the store, so it cannot source values from it. That boundary is explicit in the code (`_resolve_env_recursive(raw, use_store=False)` on the bootstrap path), not an accident of boot ordering.

```yaml
# config.yaml (Tier A) — always env/file-sourced, never from the store
providers:
  database:
    dsn: "${CREWLET_DSN}"
secrets:
  active_key_id: "2026-01"
  keys:
    - id: "2026-01"
      material: "${CREWLET_SECRET_KEY_2026_01}"
```

---

## Using it

### From the CLI

```bash
crewlet secrets keygen                           # a fresh key for the keyring
echo "$TOKEN" | crewlet secrets set GITLAB_TOKEN_SWE
crewlet secrets set SLACK_BOT_TOKEN_CEO          # reads stdin
crewlet secrets list                             # names + metadata, never values
crewlet secrets unset STALE_TOKEN
crewlet secrets get TOKEN -reveal                # break-glass; logged
crewlet secrets rekey                            # after a keyring rotation
```

The value is read from **stdin** by default rather than from `-value`, because an argv value is visible in `ps` output and lands in shell history. Exactly one trailing newline is stripped, so `echo "$TOKEN" | ...` does the right thing — and no more than one, because a secret may legitimately end in whitespace and altering it silently is a failure nobody can see.

There is **no HTTP route that returns a secret value**, by design. `crewlet secrets get -reveal` is the only read-back: it refuses without the explicit flag and logs the access by name. The refusal points at `list`, because the common need is "is X set and when did it last change" — which the listing answers without putting a credential into a terminal, a scrollback buffer and a screen-share.

`crewlet secrets keygen` needs no config and no store: it is what an operator runs *before* either exists, and it prints the base64 form the keyring's `material` field takes.

Every other command reads the **Tier A** config for its keyring, never the company document. The store holds only ciphertext; the key material lives in the bootstrap file — on disk or in the environment, never in the database it opens.

### From a provisioner

Every provisioning CLI takes `-secret-store` in place of `-env-file`:

```bash
GITLAB_ADMIN_TOKEN="$GITLAB_ADMIN_TOKEN" crewlet gitlab provision company.yaml \
  -public-url https://engine.example.com \
  -secret-store
```

Minted PATs and the generated webhook signing secret go straight into the encrypted table under the same `${VAR}` names the config already references. The three-step dance collapses to one command — no file to source, no shell to be in.

`crewlet plane provision -secret-store`, `crewlet slack provision -secret-store`
and `crewlet mattermost provision -secret-store` work identically. Slack's own
app-configuration token pair is the one credential that does **not** go here:
it is the operator's, not the company's, and it lives in the
[app ledger](../integrations/slack.md#the-app-ledger) beside the company file.

Where it pays off most is a credential the vendor shows **once**. Plane
generates a webhook's secret at creation and returns it exactly once, and every
vendor here returns a token's value once and never again — so the recorded copy
*is* the credential. A file someone has to remember to source is a worse home
for that than an encrypted table the engine reads back itself.

### Propagation

| When | What picks up a new value |
|---|---|
| `crewlet run` boot (every node role) | Reads the whole table before resolving any Tier B `${VAR}` |
| A config revision activates (`PUT /config`, `crewlet config import`) | Each node re-reads **its own** store as it converges on the new activation epoch — engine and API halves alike |
| Otherwise | The running process keeps its snapshot |

**The store is the node's own**, like the database it lives in, and a fleet is where that shows. `crewlet secrets set` writes the rows of the one node whose `crewlet.yaml` it was pointed at; nothing propagates them. On more than one node a rotated credential has to be set on **every** node — run the command once per node's Tier A file, or supply the value through the process environment, which every node's resolver falls back to. The activation epoch propagates; the value it re-resolves does not.

> **Stop the node's engine before running `crewlet secrets`.** The store is
> **one file, one process**, and the engine takes an exclusive lock on it for
> as long as it runs — so `crewlet secrets` against a live node is **refused**,
> naming the process holding the file and what to do about it. It is not a
> caution you can run past: the driver does not support two writers and does
> not reliably refuse the second one, so before the lock existed this corrupted
> the database silently.
>
> The lock is released by the kernel when the engine exits, however it exits —
> a crash, a `kill -9` and an OOM all free it, and there is no stale lock to
> clear by hand.
>
> On a fleet the nodes are independent, so a rotation is a rolling one: stop a
> node, set the value, start it, move on. Where that downtime is not
> acceptable, supply the value through the process environment instead — the
> resolver falls back to it — and restart nodes on your own schedule.

So `crewlet secrets set` takes effect at the next config activation or restart — the CLI says so after each write. Re-activating the *current* revision is a valid way to ask a running engine to pick up a rotated credential; the refresh happens before the no-op check precisely so that gesture works, and the activation log is append-only precisely so a re-activation still moves the pointer every node is watching (see [Control Plane](control-plane.md)).

**What "picks up" means.** A rotated value is not useful until the things that *captured* it are rebuilt — an MCP child baked the resolved value into its spawn environment, an LLM provider holds it inside a constructed client, a transport holds it in a header. So re-activating an unchanged revision has to rebuild them, even though its payload is byte-identical.

It does, and the reason is the shape of the control plane rather than a comparison. The activation table is **append-only**, so re-activating a revision mints a new epoch; a node's reconciler skips on the *epoch* it has applied, never on the payload, so a re-activation always applies. `${VAR}` references stay verbatim in the stored revision and are resolved where a provider is **constructed** — and the secret snapshot is re-read immediately before that. There is no payload-equality shortcut for a rotation to slip through.

> One surface this cannot reach: a **running code sandbox** received its credentials in the box's environment at launch, and no engine-side refresh reaches a live box. There the effective bound is the run's duration plus any clarification pause, not seconds. Tear the run down if a rotation is a revocation.

---

## Which one do I want?

Two mechanisms share the Tier A keyring and are easy to confuse:

| | [Whole-config encryption](configuration.md#secrets) | Secret store (this page) |
|---|---|---|
| **What is encrypted** | The entire `company_config` payload as one blob | One value per row |
| **Keyed by** | Revision | Environment-variable name |
| **Where the secret lives** | Inline in the config document | In `secret_values`, referenced by `${VAR}` |
| **Rotation** | New revision (a full immutable copy) | `UPDATE` of one row |
| **Written by** | `PUT /config`, `crewlet config import` | `crewlet secrets set`, provisioners |

They compose: encrypt the config document *and* keep credentials in the store. That is the recommended shape for a provisioned deployment **on one node**. On a fleet, see the next section — the store is per node, so credentials belong in the environment and the store goes unused.

**Why credentials belong in the store rather than inlined as literals in the config**, even though the config is itself encrypted:

- **Rotation would archive the old secret forever.** Every revision is an immutable full copy and revisions are never scrubbed, so each rotation leaves the superseded credential readable in history.
- **One credential, several pointers.** `role.integrations.slack.bot_token` and `role.mcp_env.slack.SLACK_MCP_XOXB_TOKEN` reference the *same* variable — one credential with two readers. Inlining literals duplicates it across pointers that must then update atomically or the identity split-brains. Keying by variable name keeps it one row.
- **Blast radius.** Losing the keyring with inlined literals loses your credentials, not just your config.

---

## How a node gets its secrets in the first place

The store is an **overlay**, not the source of truth, and knowing which is
which answers most operational questions about it.

Resolution order on any node, for any `${VAR}`:

1. **Tier A** (`crewlet.yaml`) resolves from the **process environment alone**.
   It has to: this file holds the keyring that opens the store, so a resolver
   reaching into the store would have Tier A reading from the thing it
   describes.
2. The engine opens its database and takes a **snapshot** of `secret_values`.
3. **Tier B** (the company document) resolves **store first, environment
   second**.

So a node with an empty store — or no keyring at all — resolves everything
from the environment and runs normally. That is not a degraded mode: *"no
keyring is a supported deployment; secrets come from the environment and the
store is simply not in use"* is the engine's own comment at the point it
decides. **A brand-new node always starts from the environment.** Nothing has
to be copied to it, and there is no first-boot step that reads a peer.

### Which one to use

| | Where credentials live | Rotation | New node starts |
|---|---|---|---|
| **One node** | Store (env as the fallback) | `crewlet secrets set`, then re-activate | From the env, then the store overlays it |
| **A fleet** | **The environment** | Update the env source, restart nodes | From the env, like every other node |

**On a fleet, put credentials where your platform already puts secrets** — a
Kubernetes `Secret` projected as env, systemd's `EnvironmentFile=`, Compose's
`env_file:`, `source .env` in a wrapper script. Every node then resolves the
same values, a node that scales up at 3am gets them the same way as the ones
that were already running, and the store stays empty with nothing to
propagate.

The store is worth using on a fleet only for a value you are willing to set on
every node, once per node — which is the same work as updating the environment
and one more place to forget.

> **Provisioner-minted credentials are the case that catches people.**
> `crewlet gitlab provision`, `crewlet slack provision` and the rest MINT
> credentials, and `-secret-store` writes them to **the one node whose Tier A
> file you passed**. On a fleet use `-env-file PATH` or `-print` instead and
> feed the result into your secret manager; `-secret-store` there produces a
> company where one node can authenticate and the others cannot, with nothing
> failing until a seat lands on the wrong node.

The store being per node is a gap rather than a design — the company config,
which may itself carry credentials, already replicates fleet-wide through the
coordination store under the same encryption. Closing it is
[d-203](https://github.com/crewlet/crewlet/blob/main/decisions/203-secrets-follow-the-config-path.md);
until then the environment is the fleet-wide path.

---

## What still has to be in the environment

Most `${VAR}` resolution funnels through one function, so the store covers it. A handful of places read a variable by name instead, and each was decided deliberately:

| Site | Source | Why |
|---|---|---|
| `providers.llm.*` / embeddings conventional-key fallback (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`) | **Store, then env** | Otherwise `crewlet secrets set OPENAI_API_KEY` would work through a config reference but not through the fallback |
| Everything a provisioning command reads out of the company document (`integrations.*.url`, `.workspace`, `.token`, `.signing_secret`) | **Store, then env** | The same chain the engine resolves through. A command that saw only the environment read an empty string for every value already rotated into the store — and for the GitLab signing secret, empty is the signal to *mint*, so a re-run replaced a working webhook secret at the vendor. Each command takes `-config` for this |
| Sandbox launch credential check | **Store, then env** | A seat whose token lives only in the store must not read as unresolved |
| Tier A bootstrap (`providers.database.dsn`, `secrets.keys[].material`) | **Env/file only** | Root of trust — this is what opens and decrypts the store |
| Operator provisioning credentials (`GITLAB_ADMIN_TOKEN`, `PLANE_ADMIN_TOKEN`, `MATTERMOST_ADMIN_TOKEN`) | **Env only** | Human operator credentials, never persisted by Crewlet **and never read back from the store**: a GitLab admin PAT carries `api` scope over the whole group, and the store is replicated to every node holding the keyring. Reading one from it would imply it may be kept there |
| OTLP endpoint / protocol / headers, `CREWLET_SANDBOX_OTEL_RECEIVER_URL` | **Env only** | Deployment-environment settings that belong to the host, not the company; several are read before the store loads |
| `CREWLET_TOOL_SKILLS_SPACE` / `_PROJECT` | **Env only** | Not secrets — flag defaults for the import/resync commands, read by nothing else |
| MCP stdio subprocess environment | **Env only, plus declared creds** | Servers read undeclared conventional variables (`PATH`, proxy vars, vendor SDK keys), so the host env is inherited. Store values are **not** poured in — each server gets exactly the credentials its `mcp_env` declares, already resolved. Injecting the whole store would hand every seat's token to every subprocess |

Nothing writes a minted value back into the process environment. **The sink is the only durability path**, and a value minted this run is read back through the sink — which is why a run must name one before it touches the vendor, and why `-print` reports itself as holding nothing rather than pretending otherwise.

A `-print` run that ROLLS BACK emits `unset VAR` for every value it printed,
followed by the comment saying why. The stream is meant to be sourced — that
is the whole reason it emits `export` lines — and a comment is a no-op to a
shell, so an operator who piped the output into `source` and then hit a
rollback would keep a revoked token exported in their session. Sourcing the
stream to its end now leaves the environment as it started.

**No sink feeds the engine directly.** `-env-file` writes a file to `source`
before the engine starts; the engine itself reads `${VAR}` from this store and
then from the process environment, and from nowhere else. `-secret-store` is
the one that needs no file and no restart — see [`crewlet config
activate`](../reference/cli.md#crewlet-config-activate).

---

## Operational notes

- **A missing value still resolves to `""`.** The store removes the most common cause, not the failure mode itself. `sandbox_env_unresolved` warns (names only) when a sandbox launch references something nothing answers, and the sandbox credential check refuses to launch a coding agent on an empty credential.
- **Backups.** The table holds only ciphertext; the keyring is the sole root of trust and lives in Tier A. Back them up separately — a database backup alone is unrecoverable, which is the point.
- **Key rotation.** Add the new key to `secrets.keys`, set `active_key_id`, then run **both** `crewlet config rekey` and `crewlet secrets rekey` before dropping the old key. Each row's envelope names the key that sealed it, so mixed-key states are readable throughout.
- **Migrations.** `secret_values` is created in phase 1 of the two-phase migrate, alongside `company_config` — the snapshot is installed there, before the first Tier B `${VAR}` is resolved.

---

## See also

- [Configuration](configuration.md) — the two-tier split and whole-config encryption
- [CLI reference](../reference/cli.md#crewlet-secrets) — every `crewlet secrets` subcommand
- [Environment variables](../reference/environment-variables.md) — what still has to be in the environment
- [GitLab](../integrations/gitlab.md#provisioning) / [Plane](../integrations/plane.md) — the provisioning CLIs and their sinks
