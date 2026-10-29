# CLI Reference

Crewlet ships one command, `crewlet` — a single self-contained binary. Every
subcommand below is served by it.

---

## Commands

| Command | Description |
|---------|-------------|
| `crewlet run [config.yaml]` | Read Tier A bootstrap (positional, or `-config`; default `./crewlet.yaml`), connect to DB, run engine; falls into unconfigured state if no active revision |
| `crewlet validate [file.yaml]` | Validate a Tier A or Tier B YAML, or a repository's `.crewlet/checks.yaml`, and print a summary (`-json` for located, classified problems and warnings); with no positional it checks both tiers via `-config` and `-company` |
| `crewlet migrate [config.yaml]` | Apply pending schema migrations (Tier A file, default `./crewlet.yaml`). Every process migrates on open, so this is a way to do it *without* starting one — `-check` reports pending work and exits non-zero without applying it |
| `crewlet budgets show [config]` | Print each scope's day, ISO week and month on the company clock — spend, ceiling (`unlimited` where none), the engine's `STATE` (`ok`, `near`, `refusing`), when the window turns over and `LAST REFUSED` — read from a running node, because the counters are the fleet's and not this file's. Any token reads it — budgets are everybody's overview. There is no reset: a window's allowance comes back when the window turns over |
| `crewlet backup -dir PATH [config]` | Copy a running node's store files **and** its stream estate into one verified directory on the *engine's* host — the only way to copy either, since the store is locked to that process and the embedded broker binds no socket. The token takes `admin`. See [Backups & Restore](../guides/backup.md) |
| `crewlet retention status [config]` | Every `retention` command that talks to a node takes `admin`, the reads included. What each domain's log is holding, what the trim concluded and which of the six terms is stopping it, every node's position, and what this node costs to replace. **Exits non-zero when any alarm is active**, printing each one's measurement and remedy on stderr — the hook for your own cron |
| `crewlet retention snapshots [config]` | The per-node snapshot inventory: what each machine holds, per domain, how old and how large — or why it holds none. The question you ask when a join fails |
| `crewlet retention ack -stream NAME -position N` | Publish an operator backup floor, for `backup_floor: operator`. It exists because the engine cannot see a copy that has left the host |
| `crewlet retention evict <node> -confirm <node> [-op-id ID] [-force]` | Stop a node's records applying on every log the trim counts nodes on — the tracker's, the knowledge base's, the identity estate's and the code log — so the trim can pass a floor an absent machine is pinning. Refused, with nothing written, while the node still holds a live presence lease, or while its lease cannot be read (`-force` overrides both). Prints one line per log with its outcome and the watermark before and after; **exits non-zero when not every log holds the record**, printing under each unfinished log what finishes it — the command with `-op-id` where running it again can, and what to do instead where it cannot |
| `crewlet retention readmit <node> -confirm <node> [-op-id ID]` | The inverse commit, on the same logs. Refused while the node has not applied every record up to the one just before the higher of the trim floor and the first surviving sequence of any one of those logs, and the refusal prints the numbers. Nothing is written to any log on a refusal; a partial readmission is finished like an eviction |
| `crewlet retention set-capacity <stream> <bytes> -confirm <bytes>` | Change a log's byte ceiling, up or down. Runs inside a fleet-wide maintenance window and costs three restarts, because a log's Tier A ceiling is only the value its stream is created with. A raise the broker has no room for is refused **before** the window opens, and one it refuses at the apply is reported as a refusal rather than as an unknown outcome |
| `crewlet retention maintenance status\|abandon\|exclude -stream NAME` | Where that window stands, who has not acknowledged, and the two gestures that act on it |
| `crewlet retention reanchor -stream NAME -confirm <created_at> [-force] [-discard]` | Adopt a recreated stream, or a broker restored from an older copy: move that one log to its next generation, declaring every position below it comparable and safely stale, and resume its applier with no restart. A recreated log is followed from its first surviving record, a restored one from its end, and one continuing in a generation only an evicted peer held from this node's own checkpoint, that generation's records void. A restored log holding records written after the restore that this node's rows do not hold is refused unless `-discard` accepts that they are applied on no node |
| `crewlet retention verify --restore -dir DIR` | Restore the newest artefact and open the copy. **Exits non-zero past its cadence** — the cron hook that turns a lapsed restore test into a failing check. Talks to no node |
| `crewlet work purge <item> -reason TEXT -confirm <item-key> [-op-id ID]` | Destroy a task and every row it produced, on every node. The one operation with no inverse, restricted to a credential holding `admin`, and never a seat. Its children move onto its own parent rather than being destroyed with it |
| `crewlet objects migrate -to s3 -target <path> [-final]` | Moves the company's files from the fleet's NATS bucket to an S3-compatible bucket, through a running data node: a copy pass on a live fleet, then `-final` with every node in maintenance mode, which seals the NATS bucket and moves the fleet's record. See [Moving the files to a bucket](../concepts/object-store.md#moving-the-files-to-a-bucket). The token takes `admin` |
| `crewlet objects status [config] [-json]` | Where the company's files are kept — the [object store's](../concepts/object-store.md) backend, `nats` or an S3 bucket — read from a running node, the node that ran the collector's last passes, and what each found: objects listed and deleted and unfinished uploads abandoned by the last collection; files named, **missing**, **damaged** and **unanswered** by the last audit to finish, with those files listed. `-json` prints the fleet view's `objects` block as the node answered it. The token takes `admin` |
| `crewlet code status [config] [-repo NAME] [-json]` | The [code host's](../concepts/code-host.md) repositories as a running node reads them — status, owner, default branch and its head, refs, packs, size and last push — or with `-repo` one repository in detail with its newest pushes: who made each and what became of every ref |
| `crewlet code verify [config] [-repo NAME]… [-repair] [-json]` | Check **this node's** cache of every repository — or the named ones — against their rows, and with `-repair` repair what it finds — in place where only the copy's own directory is out of place, by a rebuild otherwise. Every git node keeps its own cache, so verify each. **Exits non-zero when any copy is not clean**, after checking every one — the hook for a cron on each git node |
| `crewlet code export <name> <file.bundle> [config] [-git PATH]` | Write one repository — every ref and every object — to a git bundle on this machine, through your own git; `git clone <file.bundle>` restores it |
| `crewlet code import <source-url> <name> [config] [-git PATH] [-dry-run]` | Push another host's repository — GitHub, GitLab, any URL your git clones — into a declared one, as you, in steps that each fit `code.limits.max_push_bytes`: your own git clones the source and pushes it, and the engine dials nothing. Refuses before pushing anything what it can see in the source (a blob past the blob limit, a history already past the repository limit as one pack), stops before any step that would take the repository past that limit, skips a forge's pull refs, and **resumes** where a failed run stopped when run again |
| `crewlet code credential [-token-env NAME] get` | git's credential helper for the code host: answers git with the API token in `$CREWLET_API_TOKEN` as the password, over HTTPS (or plain http to this machine) only, and stores nothing |
| `crewlet fleet broker list [config] [-json]` | The fleet broker's membership: each live node's broker kind (`member`, `leaf`, `client`, or `unknown` for a kind this build does not know) beside how the JetStream metadata group counts it — read through a member — and, in words, every disagreement: a member gone for good that the group still counts in every election, with the command that removes it. The token takes `admin` |
| `crewlet fleet broker remove <node> -confirm <node> [-force]`, or `-peer <peer> -confirm <peer>` | Stop the metadata group counting a member that is gone for good, through a live member's system account — by node id, or by the peer id `list` shows for a voter no member can name. Refused while the node holds a live presence lease as a member; `-force` is for a member wedged in a way that still renews it. The token takes `admin` |
| `crewlet fleet broker credential [config] -o FILE` | Write the operator key a clustered member's broker admits on its loopback client listener — derived from this host's `secrets.keys`, to a file created `0600` and never to the terminal — and print the `nats` invocation that reaches the broker with it, from this host only. The token takes `admin` |
| `crewlet seats pause <handle> [-stop] [-reason TEXT] [-op-id ID]` | Pause an agent seat: it starts no new turn, its mail waits in order and its scheduled runs are skipped. `-stop` also ends the turn it is on at its next round and refuses every change its coding runs make — their boxes' pushes and their calls of its tools that write — until it is resumed. Taken by the seat's holder, whoever leads it, or an administrator, and recorded under the caller's own name, kind and credential |
| `crewlet seats resume <handle> [-op-id ID]` | Lift the pause; what waited is delivered first, in order. The same authority as `pause` |
| `crewlet artifacts list\|get\|versions\|assets\|usage` | Read the company's artifacts through a running node, as the token's principal: the listing it may see (`-scope`, `-kind`, `-q`, `-status`, `-work-item`), one artifact's head (its runtime contract and capabilities among it) and audience, its versions, each with its contract and capabilities, its assets, and the company's usage against its quota (an administrator's). See [`crewlet artifacts`](#crewlet-artifacts) |
| `crewlet artifacts publish <dir\|file> [-artifact ID] [-op-id ID]` | Publish a directory or a file as a new artifact, or as the next version of one: an upload intent first, then only the files the artifact does not already hold are sent, then the version is published against the head it read |
| `crewlet artifacts update\|restore\|label\|share\|unshare\|trash\|untrash\|purge` | Change the title, description or icon, roll back to a version, label one, share with a seat, a unit or the company, withdraw a share, move to and from the trash, and purge for good (an administrator's, with a person present; the id is asked twice) |
| `crewlet schema [company\|bootstrap]` | Print the JSON Schema for a config tier (editor autocomplete, CI, [AI-assisted authoring](../getting-started/ai-authoring.md)) |
| `crewlet config import <company.yaml>` | Load a company file as a new active `company_config` revision — through a running node's `PUT /config`, which activates it fleet-wide, or offline into this node's store, which the node publishes at its next start; the line it prints says which it did |
| `crewlet config export [--revision <UUID>]` | Dump the active (or specified) revision as YAML to stdout |
| `crewlet iam <command>` | People, seats, invitations, permissions, sessions and tokens, through a running node. See [`crewlet iam`](#crewlet-iam) |
| `crewlet config show` | One-line summary of the active revision |
| `crewlet config revisions [--limit N]` | List recent revisions (newest first) |
| `crewlet config diff <UUID> [-against <UUID\|active>]` | Structural diff of two revisions — paths and values, always redacted on both sides |
| `crewlet config activate <UUID>` | Mark a revision active in this node's store, published to the fleet at its next start; re-activating the current one mints a new epoch, which is how a rotated secret takes effect. A running fleet does the same through `POST /config/revisions/<UUID>/revert` or `POST /config/reload` |
| `crewlet config rekey [-dry-run]` | Re-encrypt the active revision's config document under the active key (master-key rotation) |
| `crewlet secrets keygen [-key-id ID]` | Generate a fresh encryption-keyring key + the `crewlet.yaml` snippet to install it |
| `crewlet secrets set <NAME>` | Store an encrypted secret in the [secret store](../concepts/secret-store.md); the engine resolves `${NAME}` from it ahead of the environment. Takes `admin` through a node |
| `crewlet secrets list` | List stored secret names + metadata (never values). Takes `admin` through a node |
| `crewlet secrets unset <NAME>` | Remove a stored secret. Takes `admin` through a node |
| `crewlet secrets get <NAME> -reveal` | Print one stored value to stdout — break-glass, audited, CLI-only. Takes `admin` through a node, and a person present: a machine token is refused it whatever it carries, so the token this command reads is a Tier A one |
| `crewlet secrets rekey [-dry-run]` | Re-encrypt stored secrets under the active keyring key. Takes `admin` through a node |
| `crewlet search eval [-store PATH]` | Measure the two-stage semantic search against the exact scan, on the vectors a store file actually holds. Ground truth is the exact scan's own top-K, so nobody authors a judgement; exits non-zero below the floor for that corpus size |
| `crewlet llm list` | Every `providers.llm` entry the company declares, with its CLI, model and how it is signed in |
| `crewlet llm doctor [KEY]` | Verify `providers.llm` entries end to end — the CLI is installed, something signs it in, a real completion returns with a tool call, the CLI's own shell is refused and its web tool reaches the network (`-no-smoke` stops before every real completion). Exits non-zero when any entry has a problem |
| `crewlet llm login <KEY>` | Establish the vendor's own login for a provider: brokered interactively, `-from-host` to adopt one this machine already has, `-capture-token` to mint a headless token into the [secret store](../concepts/secret-store.md) (add `-print-token` to send it to stdout and store nothing), `-token-stdin` for one you already hold |
| `crewlet llm status <KEY>` | Ask the CLI who it is currently logged in as |
| `crewlet llm logout <KEY>` | Revoke locally and delete the provider's login — its credential files, or the login document a CLI like OpenCode 2 exports |
| `crewlet llm export <KEY> [-secret-store]` | Pack the login into one portable blob — stdout, or the secret store under the name the engine restores from on a fresh host |
| `crewlet llm import <KEY>` | Restore a bundle from **stdin** onto this host; refuses to overwrite a login that is already there |
| `crewlet confluence import <company.yaml> <directory>` | Publish a directory of authored markdown into [Confluence](../integrations/confluence.md) spaces — one space per directory, plus the tool skills the files themselves declare. Every target space is checked before a single page is written |
| `crewlet confluence provision <company.yaml>` | Register the inbound [Confluence](../integrations/confluence.md) webhooks and mint the credential each one carries. Cloud gets one token-bearing hook per event, Data Center one signed hook for all of them, and a re-run converges what is there rather than adding to it |
| `crewlet confluence resync <company.yaml>` | Re-run the engine's own tool-skill walk of the Confluence skills space against a throwaway registry and print what loads — a read-only diagnostic, not a way to change a running engine |
| `crewlet slack provision <company.yaml>` | Create, update and install one [Slack](../integrations/slack.md) app per agent seat from the canonical manifest, minting each seat's bot token and signing secret into the `${VAR}`s its config points at. The install itself is an OAuth grant, so the run hands the operator one authorize URL per seat and takes the code back |
| `crewlet jira provision <company.yaml>` | Report a [Jira](../integrations/jira.md) instance against the config: which account each seat's own credential authenticates as, whether every project the org chart names exists and agrees about its lead, and — on Data Center — register the inbound webhook with a minted secret. Jira issues no credentials on a provisioner's behalf, so this run reports far more than it changes |
| `crewlet gitlab provision <company.yaml>` | Reconcile the config into GitLab: one service account per agent seat, membership, per-agent PATs minted into the config's own `${VAR}` references, and the group webhook. A re-run leaves a working token alone; `-dry-run` reports without touching anything, and a run that cannot record what it minted revokes it. |
| `crewlet github provision <company.yaml>` | Report a [GitHub](../integrations/github.md) deployment against the config — which account each seat's own credential authenticates as — and register the inbound webhooks with a minted secret: one on the organization where the credential may, otherwise one per named repository. GitHub issues no credentials on a provisioner's behalf, so this run reports more than it changes |
| `crewlet mattermost provision <company.yaml>` | Create/update one Mattermost bot account per Mattermost-enabled agent, add it to the team + channels, mint its access token into an env file or the [secret store](../concepts/secret-store.md). Keeps each bot's display name in step with the company document. A re-run leaves a working token alone; `-rotate` mints fresh ones, `-handles a,b` narrows the run, `-decommission` disables departed seats' bots. Runs a preflight first: system-admin role, `EnableBotAccountCreation`, `EnableUserAccessTokens`. |
| `crewlet mattermost doctor <company.yaml>` | Check a [Mattermost](../integrations/mattermost.md) install end to end: reachability, the Site URL every browser inherits (against the address people browse, given `-browser-url`), a browser-shaped websocket upgrade, and one real authenticated socket per agent seat |
| `crewlet version` | Show the installed version (`--version` and `-v` too) |
| `crewlet help` | Show the usage message (`--help` and `-h` too) |

---

> **Every command that reads the Tier B company document takes `-config`** (default `./crewlet.yaml`), and resolves its `${VAR}` references the way the engine does: **the secret store first, the process environment behind it**. A command that read the environment alone would see an empty string for every value already rotated into the store — and for `integrations.gitlab.signing_secret`, empty is the signal to *mint*, so a re-run would replace a working webhook secret at the third-party app. **The store read is the fleet's whether or not this node's engine runs, and the engine decides only the way in**, as it does for a write ([`crewlet secrets`](#crewlet-secrets)). A **running** engine holds this node's database, and the values are read through its API, at the `api.host:port` in `-config`; `-api`, which every one of these commands takes, names the node to read through instead, whether or not this one runs. A **stopped** one leaves the store to the command, which reaches it itself: having asked the node's database lock whether the engine runs — and kept no claim on it — it starts this node's own broker on `stream.store_dir` with no listener at all — or, on `stream.type: nats`, dials the external broker with Tier A's credentials — and stops it when it is done. A member of a cluster (`stream.cluster`) is refused with its engine stopped, because started alone it cannot reach the quorum the store needs, and started beside running peers it would rejoin the cluster as this node: name a running member with `-api`, or start this node's engine and re-run. A named node is read whether or not a Tier A is at `-config`, so a command run from a machine that is not a node reads the fleet's values rather than the environment alone. A node that holds no data (`store.scratch`) holds none of the fleet's store — on the embedded topology its broker is a leaf of the members', with no store of its own to start — and is refused unless `-api` names one that does. Only the variables **that command** reads are fetched, either way in: through a node each with a reveal (`GET /secrets/{name}?reveal=true`), which the node logs by name against the caller and which takes `admin` and a person present, so `CREWLET_API_TOKEN` must hold one of Tier A's tokens, since a machine token is refused a reveal; read directly, with no line in any node's log, because the command holds this node's Tier A — its keyring, and the broker's own files or credentials — which opens every value without any API. `crewlet llm` reads the entries it works on (`login`, `status`, `logout`, `export`, `import` and `doctor KEY` the one named — and only it, whatever the other keys look like — `list` and a bare `doctor` every one); `gitlab provision` reads `integrations.gitlab.url` and `.signing_secret`; `mattermost provision` `integrations.mattermost.url`, and `mattermost doctor` that, `.team` and each agent seat's `mattermost.bot_token` that is one whole `${VAR}`; `github provision` `integrations.github.url`, `.token` and `.webhook_secret`, and the token spellings its identity scan tries in each agent seat's `mcp_env.github` (its own, or the one its unit gives it); `jira provision` `integrations.jira`'s address, account and `webhook_secret`, and the Jira token spellings and address keys in each agent seat's `mcp_env.jira` and `mcp_env.atlassian` — never a Confluence-only credential beside them; `confluence import` and `resync` `integrations.confluence`'s address and account, and `confluence provision` those, `.webhook_secret` and `.webhook_token`. With `-secret-store`, `gitlab provision` and `mattermost provision` also read back each planned seat's token through the sink, to check the credential the seat holds — the same way in as the others, and with the engine stopped over the same broker: one invocation starts one, shared by everything it reads and writes. They are read **before the command does anything**, so a refused reveal, or a value the keyring cannot open, stops it there: a stored value read as unset is what makes a provisioning run mint over a working secret. A value a command reads beyond that list is still read from the fleet's store, as it is read, and the run says so; one that cannot be read stops the run where it is. Through a node the error names the token only when the node refused it (401 or 403): a node that is down or draining answers the same for any token. The run says on its first line which way it read and how many values: `resolving ${VAR} through the running node at <url>: N of the stored values this command reads revealed there, each logged by name`, or with the engine stopped `resolving ${VAR} from the fleet's store, reached through this node's broker on <stream.store_dir> — this node's engine is stopped: N of the stored values this command reads held there` (`the external broker at <host:port>` on `stream.type: nats`). With no bootstrap at that path the run resolves from the environment alone and says so on its first line; a bootstrap that exists and declares no `secrets.keys` is refused, because every node's Tier A carries a keyring and resolving a company's values from whatever the shell holds on the strength of one that does not would hide the misconfiguration. The one exception is the operator's own credential (`-admin-token` / `$GITLAB_ADMIN_TOKEN` and its siblings), which is read from the environment only — see [the secret store](../concepts/secret-store.md#what-still-has-to-be-in-the-environment).


> **Every command except `crewlet run` logs at `warn`.** They open a store,
> which logs a migration line per schema file and an open line per call —
> noise on a one-shot command whose stdout is meant to be piped, read or
> diffed. That is a default, not a ceiling: export `CREWLET_LOG_LEVEL=debug`
> (or `info` / `error`) to turn it up, which is exactly what a half-applied
> migration or a failing deploy gate needs, and `CREWLET_LOG_FORMAT=json` to
> change its shape for a collector. `CREWLET_LOG_FILE` is the third of them:
> it appends what the command *logs* to a file, beside stderr, so a CI step
> can keep a `crewlet migrate` in the same durable record as the node it is
> migrating for. It never touches what a command *prints* — stdout is still
> yours to pipe or diff. They are environment variables rather
> than flags on a dozen commands because they belong to the *invocation* — a
> CI step exports them once and everything it runs answers. A value this
> build does not recognise resolves to the default (`warn`, `console`): a bad
> log level must never be why an operator cannot run a migration, and it must
> not quietly change the default either. A log file is the exception, because
> a path is not an enum with a default: one that cannot be opened fails the
> command. `crewlet run` has its own
> `-log-level` / `-log-format` / `-log-file` / `-debug` flags, its own default
> of `info`, and reads the `logging:` block from its Tier A file — it ignores
> all three variables. Colour is
> `CREWLET_LOG_COLOR` / `NO_COLOR` everywhere — see
> [Environment Variables](environment-variables.md#logging).
## `crewlet run`

```
crewlet run [<config.yaml>] [-company PATH | -import-company PATH] [-debug]
            [-log-level LEVEL] [-log-format FORMAT] [-log-file PATH]
            [-mode MODE]
            [-roles ROLE[,ROLE...]] [-api-host HOST] [-api-port PORT]
            [-dev-principal LOGIN]
```

Reads Tier A bootstrap and starts the agent engine.

**Tier B is not read from a file at runtime.** A running node serves the
revision the fleet's [activation pointer](../concepts/control-plane.md) names,
so a Tier B file on this command line is only ever a way of getting a document
*into* the store — and the two flags above are the two reasons to want that.
The file is read against the rules a running company depends on, and against
the [admission rules](../concepts/configuration.md#what-a-stored-revision-is-held-to)
only when it is actually written as a new revision (`-company` into an empty
store, `-import-company` over a different company): a file that is already the
active revision, or a bootstrap the store's own company outranks, starts the
node even when it carries a duplicate unit key stored before the rule existed.
A file that is a **redacted read** — `crewlet config show`, `GET /config`, the
dashboard's Copy as YAML, each of which masks an inline credential as
`__redacted__` — has every mask restored from this node's active revision
before it is judged, by the identity of what holds it (a seat by its handle, a
unit by its key), as [`crewlet config import`](#crewlet-config-import) restores
one; a mask nothing there answers, or a store holding no revision, refuses the
start naming the field.
To change a **running** fleet with no restart at all, use
[`crewlet config import`](#crewlet-config-import), which goes through the
node's API. The path comes from the
**positional argument**, or from `-config`, defaulting to `./crewlet.yaml`.
Naming it both ways is refused: the two would have to agree and nothing checks
that they do. A leftover positional is refused too, rather than ignored —
Go's flag parser stops at the first non-flag token, so a command that took the
path and kept going would silently boot from the default without ever
mentioning the file the operator named.

If the default is missing and a `config.yaml` sits beside it, the error says
so. `config.yaml` is the name this project's own guides used to give the Tier A
document, and the bundled example still carries it in
`examples/nimbus.config.yaml`, so an operator with a file written against that
guidance gets "no such file" about a name they never typed. It is a **hint, not
a fallback**: silently loading a file nobody asked for is how a node boots from
the wrong document on a machine that has both. Tier B is read from the `company_config` table in the store — if no active revision exists, the engine boots in the **unconfigured** state with the API still serving so an operator can bootstrap via `crewlet config import` or `PUT /config`. See [Configuration concept doc](../concepts/configuration.md).

| Flag | Description |
|------|-------------|
| `-config PATH` | Tier A: this node's broker, store and API (default `./crewlet.yaml`) |
| `-company PATH` | Tier B **bootstrap seed** (default `./company.yaml`): imported only when no company exists yet — neither in this node's store nor in the fleet it joins, so a node whose store is empty (one [without `data`](../guides/fleet.md#nodes-that-hold-no-data), at every boot, or a data node joining a running fleet) never replaces the fleet's company with its file. Once one exists this file is **ignored**, loudly (`company_seed_ignored` at warn, unless the file is the fleet's own document), so a restart with a stale file never reverts a live change, and the node boots on the store's company rather than the file's; a node without `data` boots unconfigured whatever its file, and takes the fleet's company (or the one its file just bootstrapped) before it claims a seat. Absent at its default is fine — the node boots on whatever the store holds. A revision the seed writes is the node's own: its `created_by` is the node's id and its `created_by_kind` is `node`. On a deployment whose Tier A names [`api.auth.company_writers`](../concepts/configuration.md#managed-configuration) the seed is never imported, empty store or not, and says so the same way: the managing system writes the company, and an empty store boots unconfigured until it does. |
| `-import-company PATH` | Tier B to make the active revision **now**, over whatever the fleet is running. The deliberate "this file is the company again" gesture. Mutually exclusive with `-company`; both together is refused, because they ask for opposite things. **Refused** before the node starts when Tier A names [`api.auth.company_writers`](../concepts/configuration.md#managed-configuration): the file presents no credential, and the managing system would replace it at its next reconcile. |
| `-log-level LEVEL` | `debug`, `info` (default), `warn` or `error`. Overrides `logging.level` in Tier A, and only when actually given. A typo resolves to `info` — a bad log level must never be why a company will not boot. |
| `-log-format FORMAT` | `console` (default), `text` or `json`. Overrides `logging.format` in Tier A, and only when actually given. `console` is columns and colour for a person; `text` is slog's `key=value`; `json` is one object per line for a shipper. A typo resolves to `console`. |
| `-log-file PATH` | Also write the log to this file, overriding `logging.file.path` in Tier A, and only when actually given. It is a **second** destination: stderr keeps every line. An explicit `-log-file ""` writes no file for one run, whatever the Tier A document says — unless that document also set `logging.stderr: false`, which would leave the node writing its log nowhere; that combination is refused by name. It moves the path only — the file's shape, level and rotation caps stay Tier A's. A path that cannot be opened **fails the command** rather than resolving to a default: a durable record an operator did not get, with nothing saying why, is worse than not starting. |
| `-debug` | Shorthand for `-log-level debug`; wins if both are given. It only ever *raises* — to quieten a file that sets `logging.level: debug`, pass `-log-level info`. It makes the **engine** verbose and not the embedded broker: nats-server's own debug output is `stream.debug` in Tier A, off by default, because it is per internal-client rather than per event and the engine's coordination reads produce a constant stream of it. The broker's warnings and errors reach the log at either setting. |
| `-api-host HOST` | Bind address, overriding `api.host` |
| `-api-port PORT` | Bind port, overriding `api.port`. With the `ingress` role it carries the whole HTTP surface (less the routes `api.public.port` moves to its own listener); without it, only the [probes](api-endpoints.md#probes-on-a-node-without-ingress) — `/health` and `/ready` — and a seats node's agent-mode tool bridge where `api.public.port` is unset. `0` serves **no HTTP at all** — no dashboard, no REST, no webhook endpoint and no probe, so every integration goes deaf and no orchestrator can probe the node. That is why leaving the flag off is not the same as passing `0`. Any other port is bound as the node starts, before it opens its store, so a port somebody holds fails the run at once; until the node serves, it answers what a [starting node](api-endpoints.md#while-a-node-is-starting) answers. Either flag is refused when it contradicts the file's other listeners: `0` beneath an `api.public.port`, or a port another listener in the file binds on the same address. So is a port on an ingress node whose file has no `api:` block: a node serving the API must state `api.external_url`, `api.auth.admin` and a token. |
| `-mode MODE` | `maintenance` or `seal`: boot for a [capacity window](../guides/retention.md#changing-a-logs-ceiling) rather than for service. Both start the broker and **no publisher** — no seats, no duties, no schedulers, no writer on any surface, no barrier (a `linearizable` read is refused `maintenance`), no object collection or audit (each pins the estate with a barrier), and no eviction, readmission or reanchor — while the node keeps its presence lease, and the difference is that `maintenance` may write stream configuration while `seal` may not, which is exactly what makes a `seal`-mode acknowledgement evidence. Leave it off for a node in service; a node in either mode refuses to run a company. |
| `-roles ROLE[,ROLE...]` | What this node runs, overriding `node.roles`: `data` (hold the company's durable state), `git` (serve the company's git repositories where it sets `code.native`; needs `ingress`), `ingress` (serve the HTTP API and its webhooks), `seats` (claim seat leases and run agents), `workers` (the company-wide singleton duties). Default: all five — one process running a whole company. An unknown name is **rejected rather than dropped**, because a typo would otherwise produce a node that runs nothing and reports itself healthy. The flag is applied after the file validates, so what the roles require of the file is checked again here: dropping `data` from a node whose store is not `store.scratch` is refused, naming the setting. Roles decide what a node serves and which duties it holds, and `data` alone decides which state logs it applies: every data node applies every one of them, the identity directory and the code log included, and a node without `data` applies none and reaches the estate through one that does — see [what a satellite holds](../guides/satellite-nodes.md#what-a-satellite-holds). See [Running a Fleet](../guides/fleet.md#nodes-that-hold-no-data). |
| `-dev-principal LOGIN` | **Development only.** Resolve every request that presents no credential to `dev:LOGIN`, carrying every app, and `admin` only where `api.auth.admin` is `serve`. It is what `api.auth.disabled` should have been, and it is **refused** unless *both* hold: `api.host` binds a loopback address, and the binary is a development build. See below. |

The logging flags override the Tier A `logging:` block **only when they are actually given**: a flag carries its default whether or not anyone typed it, so applying them unconditionally would pin every node at `info` and make the file's own setting dead on arrival. `-log-file` needs that distinction in both directions — its default *is* the empty string, which is also how an operator says "no file for this run". Every flag that overrides Tier A is **folded into the document** the node runs, the logging flags as much as `-roles` and the API's: the verbosity, the log file and the [advisories the start logs](#every-start-says-what-its-tier-a-is-worth-knowing) all read that one document. The run holds that document to every Tier A rule once the flags are in, and a refusal names the flags given — the file passed as it loaded, but a flag reaches every rule that reads what it moved — and the engine asks the rules again before it opens anything.

The four overrides — `-roles`, `-api-host`, `-api-port` and `-log-file` — are the fields whose right value depends on *where the process is running* rather than on what the company is: which job this node does, where its HTTP surface binds, and which path on this host its log lands on. Everything else in Tier A belongs in the file, where it can be reviewed — including the log file's own shape and rotation caps, which describe the disk rather than the invocation.

**How it exits says why it stopped**, which is what a supervisor's restart policy reads:

| Status | Why |
|---|---|
| `0` | A signal (`SIGINT`, `SIGTERM`) stopped it, after the drain and the teardown |
| `1` | It could not start — a refused config, a store or broker it could not open (a database or `stream.store_dir` another process holds among them, named by its pid: another engine, or a `crewlet` command run while this node was stopped — an offline `config` or `migrate`, or one reaching the fleet's store — which holds them only until it finishes), or a broker connection closed for good while it booted, whose last stderr line names the cause and what to change exactly as the next case's does — **or it stopped itself** because a broker connection it cannot run without was [closed for good](../concepts/seat-ownership.md#the-node-whose-broker-is-gone-and-why-it-leaves-too): the last stderr line, `crewlet: the node stopped itself: …`, names the cause and what to change, after the drain and teardown in the order a signal takes them — a drain that cancels the turns still running rather than waiting for them when the connection lost was the stream's own, since none of them can be acknowledged |
| `69` | This node's [embedded broker declared itself dead](../concepts/seat-ownership.md#a-broker-that-declares-itself-dead) after the boot, and the node left by the watchdog's exit, with no drain, after an `embedded_broker_died` line in the log and with the broker's own reason on stderr. A broker that declares itself dead *during* the boot fails the start instead, with status `1` |
| `75` | The [watchdog](../concepts/seat-ownership.md#the-wedged-node-and-why-it-leaves) ended a wedged process, with no shutdown at all |

Every non-zero status is a node that wants restarting, so run it under something that restarts a process that exits non-zero (`Restart=on-failure`, a container restart policy).

### Every start says what its Tier A is worth knowing

Every warning [`crewlet validate`](#crewlet-validate) prints for a Tier A
document, `crewlet run` logs too: once per start, at `WARN`, one
`config_advisory` line each, in the order `validate` lists them.

```json
{"level":"WARN","msg":"config_advisory","component":"cli","config":"bootstrap","path":"retention.backup_owner","detail":"nobody is named as this deployment's backup owner. The trim stops when the newest backup ages past `stream.tracker_retention.backup_max_age`, and the alarm that says so has nobody to name"}
```

`validate` is the moment before a deployment where a consequence can still
change the decision, and most deployments never have it: an image, a Helm chart
or an operator renders the document and starts the node, and nobody runs a
command against the file on the way. The node's own log is the one place every
deployment has, and each of these — a declined fsync, a trim nobody
acknowledges, a peer entry no rule counts, an acknowledged insecure sign-in
posture — describes what the node does for as long as it runs. Tier A changes
only on a restart, so once per start is the whole record.

| Field | Meaning |
|---|---|
| `config` | `bootstrap`: which tier. `config_reference_unresolved` uses the same key for the same question, so one query finds both |
| `path` | Where in the document the remedy is written, exactly as `validate` locates it |
| `detail` | The whole message — what will happen, then what to do instead |

**After every Tier A refusal, and into the log file.** The lines are written
once the document, with every flag folded in, has passed every Tier A rule —
the run holds it to them itself, and a refusal names the flags given — and
once `-dev-principal` and `-mode` have been checked, because a warning printed
beside a refusal is noise at the moment somebody reads carefully; and after the
log file named by `logging.file.path` or `-log-file` is open, so they are in it
— unlike `config_reference_unresolved`, which comes out while the document is
still being read, before any file exists (see
[Deployment → Logging](../guides/deployment.md#logging)). What can still fail
after them is not Tier A's to refuse: a `CREWLET_NODE_ID` no node may carry, a
port somebody else holds, a company file the seed writes into the store and
holds to the
[admission rules](../concepts/configuration.md#what-a-stored-revision-is-held-to)
then, a store or a broker that will not open.

**Judged on the document the run applies.** The flags are folded in first, so
a fleet node started with `-roles` is not told its roles are undeclared, and
one started with `-debug` is not told the embedded broker's debug output has
nowhere to go — unless `logging.stderr: false` hands the stream to a log file
that keeps a quieter `logging.file.level` of its own, which `-debug` does not
raise, and then the line names that level and never `-debug`. A start with
nothing to say logs no such line; a warning never stops a node or changes how
it exits.

**And the company's, at every apply.** The advisories a Tier B document
raises — a human seat nobody can message, a company nobody can join, an idle
token ceiling — are logged under the same event with `config=company`, by every
node, once for each epoch it applies, with the revision, the epoch and the seat
or unit each is about (see
[What an applied company is worth knowing](../concepts/configuration.md#what-an-applied-company-is-worth-knowing)).
That is what reports them for a company `-company` or `-import-company` seeds,
which no write answers to anybody.

**One fact, one warning.** A node with `api.port` 0 logs `api_disabled` to say
what it gave up, and that line is `WARN` only on a node whose `node.roles` is
undeclared — every role, `ingress` among them, which the `api.port` advisory
leaves out on purpose. A node whose declared roles name `ingress` has been
warned by that advisory already, and its `api_disabled` is `INFO`, like that of
a node without `ingress`.

### `-dev-principal`, and why it is a flag

Opening the dashboard on a laptop means having a token, and the reflex is to
look for a switch that turns authentication off. There was one — Tier A's
`api.auth.disabled` — and it authenticated the **empty** credential into full
operator authority with no check on the bind address anywhere, so one unset
environment variable was a total bypass of every gate, on any deployment,
reachable by anyone who could reach the port. It is retired; a file still
carrying it is refused at that line, and the refusal says the key was removed
and that local development uses `crewlet run -dev-principal` instead.

`-dev-principal` is the thing somebody actually wanted, with the two guards
that field never had:

- **`api.host` must bind a loopback address** (`127.0.0.1`, `::1`,
  `localhost`). That is the only check that physically stops another machine
  reaching this. It judges the *bind* rather than `api.external_url`,
  deliberately and unlike `api.auth.accept_insecure`'s rule: that one
  is about whether a session cookie crosses plaintext through a proxy, where
  the external address is the truth, and this is about who can open a socket
  to the process, where the bind is. The check runs **after** `-api-host` is
  applied, so moving the bind on the command line moves the decision with it.
- **The binary must be a development build.** A bind is a setting, and a
  setting reaches production by being copied — a compose file, a Helm value, a
  base image somebody forked. The release pipeline stamps a version at link
  time; anything the engine cannot positively recognise as a development build
  refuses the flag, whatever its configuration says. That is why this is a
  flag and not a config field: a field is the thing that gets copied.

It grants every app, and `admin` only where `api.auth.admin` is `serve`, so an
operator who has made a node refuse admin has refused it here too, and every boot that enables it logs
`api_dev_principal_enabled` at **warn** naming the login and the bind.

A credential that IS presented still wins — a Tier A token resolves as itself,
and a *wrong* token is still refused with a `401` rather than quietly served
as the development principal, which would hide exactly the typo you are about
to spend an afternoon on.

For anything reachable from another machine, the answer is an
`api.auth.tokens` entry: listable, revocable without a restart, and present in
the audit log.

Publishing knowledge and tool skills is its own command rather than a flag on `run`: an engine that published on every boot would rewrite a company's knowledge base from whatever tree the deploying machine happened to have. Use [`crewlet confluence import`](#crewlet-confluence-import), and [`crewlet config import`](#crewlet-config-import) to load the first company revision.

Press `Ctrl+C` for graceful shutdown — signals escalate in two tiers:

1. **First `Ctrl+C`**: graceful. Pressed while the node is still starting, it ends the boot — once an embedded broker's store recovery in progress has finished, which cannot be cut short — with the listener answering a drain's answers, as far as a [starting node](api-endpoints.md#while-a-node-is-starting) can give them, until the boot has unwound. Once the node serves, every held seat quiesces, running LLM turns finish their rounds (there is no internal timeout, and `drain_in_progress` logs the in-flight count every 10 s), and turns still queued behind the concurrency limit are returned to the broker for prompt redelivery. The HTTP surface stays up **for the whole drain** and closes only once it completes: `/health` stays `200`, `/ready` answers `503` naming the drain, and the dashboard and every read keep answering, so the drain can be watched there as well as in the logs. What it stops doing, from the first moment, is taking work: every webhook, config, secret and setup write, operator write and backup answers `503` with `{"error": "draining"}` and a `Retry-After`, because the drain waits on in-flight turns and a surface still accepting deliveries would keep making more — and so does a new git fetch or push, in git's own `text/plain` form. A git request already running is finished rather than cut: once the turns are done, the listener stays up until the node has served none for five seconds (`git_draining`, `git_drain_complete`).
2. **Second `Ctrl+C`**: the process exits immediately. The first signal hands signal handling back to the operating system precisely so this works: in-flight turns are killed with their triggers unacknowledged, and the broker redelivers them once its ack window elapses.

There is no third tier, because the second is already an unconditional exit rather than something the engine has to be well enough to perform.

Under an orchestrator, SIGTERM follows the same tiers; the host's grace period (k8s `terminationGracePeriodSeconds`, systemd `TimeoutStopSec`, Docker `--stop-timeout` / Compose `stop_grace_period`) is the SIGKILL backstop. See [Graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown).

When piping output, prefer `tee -i` — Ctrl+C reaches the whole pipeline, and a plain `tee` dies on the first press, taking the drain logs with it (the shutdown itself is unaffected).

---

## `crewlet config`

Manage the Tier B company configuration in the store. Every subcommand opens the store named by the Tier A bootstrap (`-config`, default `./crewlet.yaml`), and decrypts what it holds with that file's keyring.

**Revisions are immutable and the activation pointer is append-only.** Nothing here edits a revision: importing writes a new one, activating appends to the pointer. That is what makes re-activating an *unchanged* revision meaningful — it mints a new epoch every node is watching, which is how a [rotated secret](../concepts/secret-store.md) reaches a running fleet without a restart.

### `crewlet config import`

```
crewlet config import <company.yaml> [-config PATH] [-api URL] [-summary STR]
```

Validates the Tier B YAML and writes it as a new active revision, recording the
previously-active revision as its `parent_revision_id`.

**A redacted read imports over the company it was read from.** `crewlet config
show`, `GET /config` and the dashboard's Copy as YAML mask every inline
credential as `__redacted__`, and a file kept in step from them is written back
with this command: each mask is restored from the revision the import replaces,
by the identity of what holds it (a seat by its handle, a unit by its key, an
entry by its key), and only then is the document validated — by the node's
`PUT /config` through a running node, and here, against this node's active
revision, offline. A mask nothing there answers — a member that is new or was
renamed, or a store holding no revision — is refused naming its field, and
nothing is stored.

**It reaches a running node.** The store is exclusive to one process, so
against a live engine this cannot open the database — and it no longer needs
to: it detects the held store and goes through that node's `PUT /config`
instead, which stores the revision **and activates it fleet-wide**, so every node converges
with no restart. `-api URL` names a node explicitly, which is
also how this works from a machine that is not the node at all. This is the
same test [`crewlet secrets`](#crewlet-secrets) makes for the fleet's secret
store, and for the same reason: while the engine runs, both are behind it. With
it stopped the two part ways: a revision is written to this node's own store,
and a secret to the fleet's.

Through a node, the write is a [compare-and-set](api-endpoints.md#concurrent-writes)
like every other. If another write activated first, the import says so and
names the revision that won; when the node kept the document as an inactive
revision it names that too, with the node's own routes that compare it with
what is live and make it live (`GET /config/revisions/<UUID>/diff` and
`POST /config/revisions/<UUID>/revert`), because `crewlet config diff` and
`crewlet config activate` open the store the running engine holds.

With the engine **stopped** it writes to this node's own store and marks the
revision active there, which the node publishes to the fleet at its next start.
The line it prints says which of the two happened.

**It never refuses a file for its charter.** A company file whose mission or
policy breaks the [charter's rules](../concepts/configuration.md#mission-vision-and-policies)
(past its length, or on two lines) is a file that ran under an earlier build,
and restoring or migrating it must work: the import prints each fault as a
`warning (admission): …` line and imports it, both ways. Through a running
node it sends `PUT /config?import=true`, which judges the charter warn-only
where an edit would be judged on what it changes; that mode is `admin`'s, so
the `CREWLET_API_TOKEN` it presents must be a Tier A token, or a machine token
whose owner holds `admin`, and the revision records `source: file` and the
credential in `operator_id`, as an offline import does. Every other rule holds
as for any write.

`-summary` is the audit note recorded with the revision (default
`imported from <path>`). The revision history is the record of who changed what
and why, so a fleet-wide write is worth a sentence. Through the API the
revision records the author the node resolved the request to — `created_by`
is `token:<id>` for a Tier A token, a person's seat for a person (every person
holds one), and the owner for a person's machine token — with
`created_by_kind` saying which (`operator` or `human`) and `operator_id` naming the credential beside them; written offline it is the
invoking operator. Either way that author travels with the revision to every
node in the fleet.

Through the API it is decided as any `PUT /config` is: `admin`
writes anything, and a [unit's lead](api-endpoints.md#a-lead-edits-their-own-team)
may import a file whose every change is inside the units they lead. Offline
there is nobody to decide on: the store is written by whoever can open it on
the host, which is already more than any permission.

It does **not** refuse because a revision is already active, and there is no
flag to force it past one: the pointer is append-only, so an import *chains* a
revision rather than overwriting one, and `crewlet config activate` takes you
back to any earlier id.

**Idempotent by content**, the same rule the [boot seed](#crewlet-run) follows:
a file that already matches the active revision imports nothing and says so, an
edited one imports once. An active revision stored **without a seal** is never
compared — nothing vouches for what it says — and the import replaces it, which
is why every command that refuses such a revision names this one. The audit note is `imported from <path>`, `created_by`
is the invoking operator (of the `operator` kind, with no credential beside
them: none made the write) and `source` is `file` — all three are recorded for
you and none is settable from the command line. (`PUT /config` takes an
`X-Summary` header; the CLI has no equivalent.)

Like `activate`, this writes the revision to **this node's** store;
the note it prints says what publishes it to a running fleet — for an import,
the same command run again while a node is up.

**On a managed document** — Tier A names
[`api.auth.company_writers`](../concepts/configuration.md#managed-configuration)
— the offline route presents no credential and is refused, naming the writers.
Through a node, the node judges the token this command sends —
`CREWLET_API_TOKEN`, and nothing else (`api.auth.tokens` is what a node
accepts, never a wallet the CLI helps itself from): one of the writers imports
as before, and any other is refused with `config_managed`, which the command
prints naming `CREWLET_API_TOKEN` as the credential it sent.

### `crewlet config export`

```
crewlet config export [-config PATH] [-revision UUID] [-redact]
```

Dumps the active revision (or `-revision <UUID>`) as YAML to stdout. It does **not** emit the stored bytes verbatim: the payload is decrypted with the Tier A keyring, decoded, and re-encoded as YAML, so an [encrypted](../concepts/configuration.md#secrets) revision exports as the readable document rather than as its `{__encrypted__: "enc:v1:…"}` blob.

> **Without `-redact` this prints secrets in the clear.** `${VAR}` references export as themselves — they name a credential rather than being one — but a config that *inlines* a literal key exports that key. This is the one command that will do that, deliberately: it is what a genuine restore or migration needs.
>
> **`-redact` is what you want for anything you are going to share.** It masks every field the config types declare as secret-bearing (structurally, not by pattern-matching the text), which is the share-safe dump. `crewlet config show` and `crewlet config diff` are always redacted and have no flag to turn it off.

### `crewlet config show`

```
crewlet config show [-config PATH]
```

Prints the whole active revision as **redacted** YAML — exactly `crewlet config export -redact`, with no way to ask for the unredacted form. Use it to read what the fleet is actually running; use `crewlet config revisions` for the id, author and audit note.

Errors with ``no revision is active; run `crewlet config import` `` when nothing is active.

### `crewlet config revisions`

```
crewlet config revisions [-config PATH] [-limit 20]
```

Lists recent revisions. The active revision is marked with `*`. `BY` is the
revision's author, with the credential it was written through beside them where
that names something the author does not — `jane.doe (through pat:…)` for a
person's machine token; a Tier A token's `token:<id>` is both, and says so once.

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
+ mcp_servers[1].url = "https://mcp.example.com/sse"
- integrations.github.enabled (was true)
```

**Paths and values, not lines.** The stored form is JSON produced by
marshalling a struct, so re-ordering a map or adding a field with a default
rewrites lines that mean nothing to a reader. The question an operator asks is
"what changed about the company", and paths answer it — this is the same
differ [`GET /config/revisions/{id}/diff`](api-endpoints.md) serves. A string
value is quoted and other values are not, because `"true"`
and `true` are different settings and a renderer that printed both bare would
show a type change as no change at all.
**Every change, however many there are.** `GET /config/revisions/{id}/diff`
cuts its listing at 500 entries because a response body and a dashboard socket
frame have a size budget — it reports `changes_total` beside the listing, so a
reader knows what was left out. A terminal has no such budget and does have a
pager, so this command prints the whole comparison: the reader best equipped
to read a long diff was the one a shared cap kept it from.

**Both sides are always redacted, and there is no flag to turn that off.** A diff is what an operator pastes into a ticket or a chat thread to ask a colleague whether a change looks right, which is the single most likely way a credential leaves the machine. `crewlet config export -revision <UUID>` is there for the rare case that needs the real values, and it takes a deliberate act. A changed credential still appears: the revisions are compared as stored, so a rotated key is reported at its path as a change from `"__redacted__"` to `"__redacted__"`, never with either value.

### `crewlet config activate`

```
crewlet config activate <UUID> [-config PATH]
```

Marks a revision active in **this node's** store, which the node publishes to the fleet at its next start; every node then applies it on its next reconcile. It opens the store, so it runs while the engine is stopped. On a **running** fleet, `POST /config/revisions/<UUID>/revert` to a node that is up does the same without a restart — it stores that revision's document again and activates it — and the note this command prints names it.

**Re-activating the revision that is already active is not a no-op**, and that is the point: the pointer is append-only, so it mints a new epoch. A node's reconciler skips on the *epoch* it has applied, never on the payload, so the apply always runs — re-reading the [secret store](../concepts/secret-store.md) and rebuilding every provider, transport and MCP child that captured a resolved value. It is the documented way to make a rotated credential take effect; on a running fleet the same gesture is `POST /config/reload`.

On a [managed document](../concepts/configuration.md#managed-configuration)
that gesture stays open — it changes nothing in the company — and activating
any **other** revision is refused, because it presents no credential and
replaces the document the managing system wrote.

### `crewlet config rekey`

```
crewlet config rekey [-dry-run] [-config PATH]
```

Rotates the master key: re-encrypts the active revision's config document under the current `secrets.active_key_id`, writing a new revision. The document is decrypted with whatever key sealed it (that key **must still be in** `secrets.keys`) and re-encrypted under the active key.

**Run this and [`crewlet secrets rekey`](#crewlet-secrets-rekey) together.** They are the two halves of one rotation — the company document and the secret store are sealed with the same keyring — and doing only one, then dropping the retired key, makes whatever the other still holds unreadable.

It opens the store directly, so it runs on a **stopped** node, and the node publishes the re-sealed document at its next start. While the engine holds the store it refuses and names the running node's equivalent, `POST /config/reload`, which stores the active document again under that node's active key. Workflow, in outline: `crewlet secrets keygen -key-id <new>` → add the new key to `secrets.keys` on every node and restart → set `active_key_id: <new>` and restart again → `crewlet secrets rekey` **and** this command, or a reload → drop the old key only once [the rotation runbook](../concepts/secret-store.md#rotating-the-keyring-with-nothing-in-flight-lost)'s conditions hold.

`-dry-run` reports what would move by reading the key id off the envelope, decrypting nothing. Idempotent: a document already under the active key is skipped and says so. A **plaintext** revision is refused like every other reader refuses one: it has no key to rotate, and nothing vouches for who wrote it, so the refusal names `crewlet config import`, which stores the document sealed again from your own copy. Fails clearly, naming the key, if the document is sealed under one no longer in the keyring.

---

## `crewlet iam`

```
crewlet iam <command> [SUBJECT] [-config PATH] [-api URL] [-json] [-reason TEXT]
```

The company's [people, credentials and sessions](../concepts/identity-and-access.md),
from the command line.

**Every one of these goes through a running node**, and that is not a
limitation to route around: the directory is a state-log domain, a change is a
RECORD arbitrated at the broker, and the broker is embedded in the engine's
own process with no listener. A second process cannot publish one, and the
store file is exclusive to whichever process holds it, so an offline read
would be a second applier.

**Authentication is `CREWLET_API_TOKEN` and nothing else.** The Tier A
`api.auth.tokens` list is what a node *accepts*; it is not a wallet this
command helps itself from. Export one of its values, or a machine token
minted with `crewlet iam token` — which acts as the person or service account
it was minted for, and is refused only `iam token` itself and the gestures
about how its owner signs in. The one exception is `iam token -login`, which
mints **your own** token and therefore signs in as you instead of reading the
variable at all.

**A person holds a human seat for as long as they are here.** They are invited
onto a vacant one (`invite -seat`) or created on one (`create -seat`) — the
seat's **Invite** and **Add person** in the dashboard's Company › Org chart do
the same — `bind` moves them to another, and `remove` frees it. Nothing
clears a person's seat: `unbind` is for service accounts, whose seat is
optional. `seats` lists which seats are free.

| Command | What it does |
|---|---|
| `people` | The directory, paged. `-q` narrows on the login and the seat; `-stage` on the enrolment stage; `-limit` on the page; `-after ID` starts the next page, as the listing's last line spells it |
| `show ID` | One person, in full, with their name and address opened |
| `seats` | The company's **human seats** and what holds each, in the order the company declares them: `HANDLE`, `NAME`, `UNIT`, the `HOLDER` (marked `(service account)` for one) and their `STAGE`, the open `INVITATION` for the seat (its id, and the address it was sent to) and when that invitation `EXPIRES`. **What a token reads depends on whose it is**, because the listing is everybody's and only the overview's half of it is: for an admin, and for the holder themselves, the `HOLDER` is their login; for anybody else it is the name they go by (`(name unavailable)` where this node's keyring did not open it, `(held)` for somebody enrolled with no name), and `STAGE`, `INVITATION` and `EXPIRES` are an admin's alone — so to anybody else a seat with an open invitation reads as unheld. `-unheld` keeps only the **vacant** ones — nobody holds them and no open invitation is for them — which are the seats `invite` and `create` take, and it is an admin's. An empty listing says why: a company that declares no `kind: human` seat, which can admit nobody until it declares one, or one whose every seat is held or invited — which an empty `-unheld` listing tells apart by asking the node for every seat once (never under `-json`), and whose remedy names what frees a seat: `remove` for a person, `unbind` for a service account, `cancel-invite` for an invitation, or a new `kind: human` seat. A node that runs no company yet refuses it, naming the step before: a company declaring a human seat. See [`GET /iam/seats`](api-endpoints.md#get-iamseats-lists-the-human-seats-and-who-holds-each) |
| `invite EMAIL -seat SEAT` | Invite someone to a vacant human seat. **`-seat` is required**: a person holds a human seat for as long as they are here, so nobody is invited onto none — the command refuses a missing one before it reads any config, naming `seats -unheld`, which lists the seats that are free. The seat must be a human seat that no person and no open invitation holds. Prints the link **once** — nothing stores it and no route reads one back — good for a week. The link is the dashboard's invitation screen, `<api.external_url>/dashboard#/invite/<id>.<secret>`, and redeeming it creates the person on that seat. While it is open the invitation **holds the seat** as it holds the address: a create, a move or another invitation naming the seat is refused naming it, and so is a company write that would remove the seat — see [An invitation names its seat, and holds it](../concepts/identity-and-access.md#an-invitation-names-its-seat-and-holds-it). Run under a Tier A token on an empty estate, it is how the company's **first person** arrives — onto a human seat the company declares, so a company that declares none declares one first: give them `-grants admin` — see [How the first person exists](../concepts/identity-and-access.md#how-the-first-person-exists) |
| `invitations` | The invitations nobody has redeemed and that are still good, paged like `people` (`-limit`, `-after ID`); `-all` adds the expired and redeemed ones the estate still holds, each with its state. Every row says for whom, which seat it binds, what it confers, who issued it and until when — and **never the link**, which was shown once: an inviter who lost it cancels and issues another |
| `cancel-invite ID` | Withdraw an invitation nobody redeemed: its link opens nothing from then on, and the address and the seat it held are free at once — for a new invitation, a create or a move. A redeemed one is refused naming the person it created — what undoes that is `remove`. See [A person arrives on their seat: invited, or created](../concepts/identity-and-access.md#a-person-arrives-on-their-seat-invited-or-created) |
| `create` | Create somebody directly, active from the moment it lands. **A person** — `create -email E -seat SEAT [-login L] [-name N] [-grants G,...]`, every app when `-grants` is left out — needs the address they sign in with and the vacant human seat they will hold, and the command refuses either missing before it reads any config. `-login` may be left out: the node proposes one from the address (`jane.doe@example.com` → `jane.doe`), as an invitation's screen does, and the command prints the login it took. It prints their id, login and seat and the outcome, and then their **first password link, once** — `<api.external_url>/dashboard#/reset/<id>.<secret>`, good for a week — which sets their first password once and signs nobody in: they sign in afterwards, enrolling a second factor there where the deployment requires one. Send it to them yourself; this engine sends no mail. See [A person created directly gets their first password link](../concepts/identity-and-access.md#a-person-created-directly-gets-their-first-password-link). **A service account** — `create -kind machine -login L -grants G,... [-seat SEAT] [-name N]` — needs its login, because it has no address to be found by, and `-grants`, because nobody is behind it to have meant a default (`-grants none` for the overview alone; a missing one is refused before any config is read); `-seat` is optional and binds it to act as that human seat (`-login token:<id> -seat SEAT -grants none` binds a Tier A token in one step — the row carries nothing itself, since the token's permissions are its Tier A entry's). It gets no link — it has no password — and the command prints the `iam token -person ID` that mints its token. **Every principal enrols with a login** in its kind's grammar — a person's is dotted (`jane.doe`), a machine's coloned (`ci:release`, or `token:<id>`), at most 64 characters either way — and anything else is refused |
| `bind ID SEAT` | **Move a person to another vacant human seat**, or bind a service account to one, so they act as it on the engine's own surface — its inbox, its day, the lead relations it holds. `SEAT` is the seat's handle, and it must be a **human** seat that no person and no open invitation holds. A Tier A token acts under the login `token:<id>`, so binding one is a service account's row under that login — `create -kind machine -login token:<id> -seat SEAT -grants none` makes it bound. See [Humans in the Org Chart](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api) |
| `unbind ID` | Take a **service account's** seat back. A person always holds a human seat, so the node refuses to unbind one (`seat_required`) and the command names what does free it: `bind ID SEAT` moves them to another seat, `remove ID` frees it |
| `grant ID` | Change someone's permissions: `-grants`, the whole set: `admin` (which stores every app beside it), `tracker`, `knowledge`, `artifacts`, or `none` |
| `suspend ID` / `activate ID` | Stop somebody acting, or let them again. The row stays either way; a suspension also ends every session, token and password reset link they hold, which activating does not bring back |
| `remove ID` | Tombstone them — which frees the seat they held — and erase every sealed value of theirs — name, address, second factor — from every node's rows, the invitation and trail rows that outlive them included. The identity log keeps the records that wrote those values, sealed under the keyring, until the retention trim passes them, and a backup keeps what it held until you delete it; their login is not sealed and outlives the removal in the clear. See [Removing somebody erases what is theirs from every node's rows](../concepts/identity-and-access.md#removing-somebody-erases-what-is-theirs-from-every-nodes-rows) |
| `revoke ID` | End every session, token and password reset link they hold, by raising their sign-out count |
| `sessions ID` | Their sessions, newest first, ended ones included — each ended one with why and when: the sign-out or gesture that ended it — a password change, a reset link, signing out everywhere, a suspension — which a later gesture never rewrites; or, for one ended with every session in the company, that |
| `credentials` | List someone's credentials. `-person` names them; without it, yourself |
| `token` | Mint a machine token, naming exactly one owner. **`-login L` mints your own**: a person's token is theirs alone to mint, so the command signs in as `L` for the one request — the password read from the terminal without echo, or the first line piped in, and a second-factor code — when your account holds one — asked for the same way, or piped as the line after it — mints from that session, and signs out, reading no `CREWLET_API_TOKEN`. **`-person ID` mints a service account's**, and takes `admin`; it is refused for a person's id, whoever asks. Either way the value is printed **once** and the estate holds a hash. It acts as that owner, carrying `-grants` (default: everything the owner holds, `admin` included) cut to what they still hold on every request, for `-days` (90, at most 365). A token may carry `admin`; what keeps it off the gestures that need a person present is the authority table's presence rows and the identity domain's refusal of an administrative record written through a token. See [Machine tokens](../concepts/identity-and-access.md#machine-tokens-a-persons-own-and-a-service-accounts) |
| `revoke-credential ID` | Withdraw one credential, naming its owner with `-person`. Run with a machine token, it withdraws machine tokens only |
| `reset-mfa ID` | Clear the second factor **and** end every session, because clearing alone leaves the ones opened with it live — and with them every token and password reset link they hold, so a link they need as well is issued after this |
| `reset-password ID\|LOGIN` | Issue somebody a **one-time password reset link**, printed **once** — `<api.external_url>/dashboard#/reset/<id>.<secret>` — and good for a day; send it to them yourself, as an invitation's. The person is named by their id or by their login, resolved to the one row holding it exactly. Spending it sets a new password, ends every session and token they held, and signs nobody in: they sign in afterwards, where a second factor they hold still applies. Issuing another revokes the one before — a created person's first password link included, so this is also how somebody whose first link lapsed or went astray gets a new one, and their screen then says it sets their first password — and `suspend`, `revoke`, `reset-mfa` and `invalidate-all` each end it, so issue it after any of those it is needed beside; a machine, somebody suspended or retired, and somebody holding a permission you do not hold are refused — you are shown the link, so it is held to the rule an invitation is. Takes no `-idempotency-key`: a retry could not hand back a secret its first attempt never showed, so an unknown issue is issued again. A person changes their **own** password signed in, through `POST /auth/password`, which needs a person present — no token this command reads is one — so there is no command for it. See [A forgotten password is a one-time link from an administrator](../concepts/identity-and-access.md#a-forgotten-password-is-a-one-time-link-from-an-administrator) |
| `invalidate-all` | Invalidate every session, every machine token and every password reset link in the company. The restore runbook's last step — it ends bearers and nothing else, so a removal, a suspension, a withdrawn credential or a reduced permission the restore rolled back is re-applied by hand before it, and a reset link the restore calls for is issued after it ([Backups & Restore](../guides/backup.md#the-last-step-is-crewlet-iam-invalidate-all)) — and the token takes `admin`, a Tier A token's (a machine token is refused it, whatever it carries); the Tier A tokens in the config file are untouched |
| `check` | Find problems with the company's access: no administrator, people with no way in, dangling bindings (a seat the running company no longer holds as a human seat), an administrator this node clamps because it refuses admin. A binding a node that runs no company yet cannot judge is counted and said first rather than reported either way. See [below](#crewlet-iam-check) |
| `audit` | Show the identity audit trail. `-person`, `-event`, `-since POSITION`, `-at TIME`, `-before POSITION` (the next page, as the listing's last line spells it), `-limit`. The actor column names the credential beside them where the entry records one — `ana.admin (through pat:…)` for something their machine token did |

### Flags

| Flag | Default | What it is |
|---|---|---|
| `-config PATH` | `crewlet.yaml` | The node config file naming the node to reach |
| `-api URL` | the `api.host:port` in `-config` | The running node to talk to |
| `-json` | off | Print the raw answer rather than a table |
| `-reason TEXT` | the surface's own | Saved with the change for the audit trail |
| `-grants G,...` | see below | A comma-separated list of `admin`, `tracker`, `knowledge` and `artifacts`, or the word `none` for an empty one — anything else is refused before any config is read, naming the set. `admin` also sends every app, as the dashboard's picker stores it, so taking admin away later leaves the apps. Left out: on a person's `invite` or `create`, **every app** and not admin; on a service account's `create`, refused — it has no default; on `grant`, nothing is changed; on `token`, everything the owner holds. `none` stands alone — beside a permission it is refused, since the two say opposite things — and the flag is refused on every command but `invite`, `create`, `grant` and `token` rather than ignored |
| `-person ID` | yourself | Whose credentials or sessions, and — on `token` — the **service account** a minted token is for; a person's id is refused there, whoever asks |
| `-login L` | — | On `create`, the login to enrol under — required for a service account, and proposed from the address for a person given none. On `token`, **your own** login: the command signs in as you for the one request, mints, and signs out. The password and any second-factor code are read from the terminal or piped in, and **no flag takes either**: a recovery code on a command line lands in shell history and `ps`, and stays good there — a sign-in refused for a mistyped password spends nothing |
| `-email E` | — | On `create`, the address a person signs in with — required for a person, optional for a service account |
| `-name N` | — | On `create`, their own name |
| `-kind K` | `person` | On `create`, `person` or `machine` (a service account) |
| `-label L` | — | What to call a minted token, for somebody holding four |
| `-days N` | 90 | How long a minted token lasts, at most 365 — "forever" is deliberately unexpressible |
| `-q TERM` | — | Narrow the directory on a login or a seat |
| `-stage S` | every stage | `invited`, `enrolling`, `active`, `suspended` or `retired` |
| `-since POSITION` / `-at TIME` | — | Where the trail starts. `-since` is a log position; `-at` is an RFC 3339 instant the route resolves to one |
| `-limit N` | 50 (100 for the trail) | How many rows |
| `-after ID` / `-before POSITION` | the first page | Where the next page starts: `-after` on `people` and `invitations`, `-before` on `audit` — each listing ends with the command for its next page, which repeats every filter the page was asked with |
| `-all` | open ones only | On `invitations` only: the expired and redeemed invitations too. Refused on every other command rather than ignored |
| `-seat SEAT` | — | On `invite` and `create`: the human seat they will hold, by its handle — one nobody holds and no open invitation is for, which `seats -unheld` lists. **Required** on `invite` and on a person's `create`, optional on a service account's. Refused on every other command rather than ignored — somebody who already exists is moved with `bind` |
| `-unheld` | every seat | On `seats` only: the vacant ones. Refused on every other command rather than ignored |
| `-idempotency-key OP` | a new operation | Retry a write whose outcome was unknown **as the same operation** — the op id its refusal named. Refused on a read, on `token` and on `reset-password` (a retry of either would hand back a record whose value was shown to nobody, so an unknown mint or issue is made again) |

**`-grants none` strips and an omitted `-grants` means what the command
states.** On `grant` they are opposite intentions and an empty flag value
cannot carry both, so the word is explicit — and it is the one spelling that
could never be a permission. `-grants none` on a person is the
[overview](../concepts/identity-and-access.md#everybodys-overview) alone: they
see the org chart, what the agents are doing and the spend, and use neither
app.

**A value shown once is shown once.** `invite`, a person's `create`,
`reset-password` and `token` each print something nothing can read back: what
the estate holds is the invitation's id and a SHA-256 of the secret its link
carries after the id, the same for a first password link and a reset link, and
a SHA-256 of the token. Lost one is re-issued rather than recovered — an
invitation cancelled and issued again, a first password link replaced by
`reset-password` — with one exception: an `invite` or a `create` retried with
`-idempotency-key <op id>` is the same operation, and hands back the very link
its first attempt issued while that link still opens, because both are derived
from the operation under the company's own key. A first password link that no
longer opens by then is not printed: the command says so and names the
`reset-password` that issues another.

**Every write prints its outcome rather than "done".** `applied` means this
node has the change; `pending` means it is durable and this node has not
applied it yet, and carries the position to read at — each with the op id it
was published under. A link is printed on `pending` too: the record is durable,
and the link opens on every node that has applied it. `unknown` means nothing
can be established from here, and it fails naming the op id and the one safe
retry: the **same** command with `-idempotency-key <op id>`. A fresh attempt
would be a second operation — for `create` and `invite` a second person or a
second invitation, which the first attempt's person or invitation refuses as a
conflict, since it holds the address and the seat. `token` and
`reset-password` are the exceptions, because their routes read no key: an
unknown mint or issue names its op id for the trail and is run again as a new
one — a token that may have landed is one nobody holds, which expires, and a
reset link issued again revokes the one before it.

**An edit refused partway says what landed.** `bind`, `suspend` and the rest
are one `PATCH`, which moves the login and the seat in one record before the
stage and the grants; a login or a seat somebody else holds — or an open
invitation holds — refuses that first record with nothing changed, everything
else the node can judge is refused before it, and what only a later record can
decide fails listing the changes that did land before it.

**A refusal names the command that clears it.** A seat or an address an open
invitation holds is refused naming the invitation, and the command adds the
`cancel-invite ID` that withdraws it. A person's seat left out is
`seat_required`, and the command adds what to do: for `invite` or `create`,
`-seat` with a seat `seats -unheld` lists; for `unbind` on a person, `bind ID
SEAT` to move them or `remove ID` to free the seat.

### `crewlet iam check`

One walk of the directory as the node you reach holds it, printed as a
`FINDING`, a `WHO` and the node's sentence saying what to do, in the order of
the table below. It reports and never repairs: each finding is somebody's
decision. A seat binding the node could not judge — it runs no company yet —
is counted and said first, because it qualifies everything below it.

| Finding | What it means | What to do |
|---|---|---|
| `no_admin_holder` | No active **person** holding a credential they have **used** — a password, a second factor, a token — holds `admin`, so nobody can invite, grant or revoke except through a Tier A token. A first password link nobody has spent does not count, and neither does a service account holding `admin`, since no machine token writes an administrative record | `grant ID -grants admin` for somebody, with a Tier A token |
| `person_without_credential` | Somebody active with no live way in: a person whose first password link lapsed unspent or was revoked, or whose every credential has been; a service account holding no live token. A live link counts, so somebody created a minute ago is not reported until theirs lapses — and an invitation creates nobody until it is redeemed, so it is never the cause | A person: `reset-password ID`. A service account: `token -person ID`. Or `remove` them |
| `binding_dangling` | Somebody bound to a seat the org chart no longer holds as a human seat — removed, or made an agent's | A person: `bind` them to another human seat, or `remove` them. A service account: `unbind` it, or `bind` it elsewhere |
| `admin_clamped_by_node` | Somebody's row holds `admin` and this node's `api.auth.admin` is `refuse`, so through this node they administer nothing (and keep every app) | Expected on a node published to the people who use the company; otherwise administer through a node that serves admin, or set `api.auth.admin: serve` here |

A login, an address or a seat two people hold is not a finding: every one is
decided on one subject against every row, so ordinary traffic never produces
one. A node holding a record it cannot decode, or a restore, can leave two rows
holding one. A lookup or a seat listing that meets them answers `503` rather
than picking either; a sign-in is refused like any failed sign-in and the node
logs `api_sign_in_lookup_failed`. A duplicated login or seat is repaired by
changing it on all but one of them — a login through `PATCH /iam/people/{id}`,
a seat with `bind` (or, for a service account, `unbind`) — and a duplicated
address, which no gesture changes, with `remove` on the extra person.

## `crewlet secrets`

### `crewlet secrets keygen`

```
crewlet secrets keygen [-key-id ID]
```

Prints a fresh base64 32-byte encryption key plus a copy-pasteable `crewlet.yaml` `secrets:` snippet that references it via an environment variable (keeping the raw key out of the file). `--key-id` (default `key-1`) names the key; the id is stamped into every envelope the key seals, so keep it stable across restarts and pick a new id only when rotating. Key generation is always explicit — Crewlet never auto-generates a key, because a silently-generated key that isn't captured makes every backup unrecoverable. See [Secrets](../concepts/configuration.md#secrets).

The remaining subcommands operate on the [secret store](../concepts/secret-store.md) — the company's encrypted credentials, one sealed value per `${VAR}` name, which the engine consults **ahead of** the process environment. All of them read the Tier A bootstrap (`-config`, default `./crewlet.yaml`) for the keyring, and all of them need one: the store has no plaintext mode, so a config declaring no `secrets.keys` is refused with a pointer at `keygen` rather than silently storing plaintext.

**There is one store, and whether this node's engine is running decides only the way in** — the command says which it used. The rows live on the coordination KV so every node reads them, and nowhere else. While the engine **runs**, that KV is behind it — on the default topology inside its own process, listening on no socket — so the command goes through the node's authenticated `/secrets` API. While it is **stopped**, the command reaches the same store itself: on a solo node (the embedded broker, no `stream.cluster`) it starts the node's own broker on `stream.store_dir`, with no listener at all, and stops it when done; on `stream.type: nats` it dials the external broker, with Tier A's credentials. On a node that has never booted it creates the fleet's buckets exactly as the engine's first boot would, and the engine adopts them, so a value set before the first `crewlet run` is the one the engine reads. The engine's exclusive database lock is what tells the two apart, with a pid attached — and the command holds that lock for as long as it works, so an engine started meanwhile is refused, naming the command's pid, rather than opening the store beside it; a command started while an engine is still starting is refused the same way and told to re-run once it is up. See [Which store the CLI writes](../concepts/secret-store.md#which-store-the-cli-writes).

Two kinds of node cannot reach the store on their own, and are refused; `-api` (below) is the way round for both. A **member of a cluster** (`stream.cluster`) with its engine stopped: started alone its broker never reaches the quorum the store needs, and started beside running peers it would rejoin the cluster as this node — so name a running member, or start this node's engine and re-run, which then goes through its API. And a **node that holds no data** (`store.scratch`), whether its engine runs or not: it holds none of the store — on the embedded topology its broker is a leaf of the members', which run it — so the store is reached through one of them.

`-api URL` names the node to go through, for running the command from a machine that is not the node, or from either of those. The bearer token comes from `CREWLET_API_TOKEN` and from nowhere else — `api.auth.tokens` is what a node **accepts** rather than a wallet the CLI helps itself from — and the credential's own identity is recorded as the author of the write.

### `crewlet secrets set`

```
crewlet secrets set <NAME> [-value V] [-config PATH] [-api URL]
```

Stores one encrypted secret under `NAME`, which must be a valid environment-variable name — a name outside the `${VAR}` grammar could never be read back, so it is rejected. Existing names are replaced.

The value comes from **stdin** by default (`echo "$TOKEN" | crewlet secrets set GITLAB_TOKEN_SWE`), or from an interactive prompt on a terminal. `-value` exists for scripted use, but an argv value is visible in `ps` and lands in shell history, so prefer stdin. The row's **source** is the write path's own word and no flag sets it: through a running node it is the route's `api`, whichever command made the write; written directly, with the engine stopped, it is the command's own — `cli` here, `provision` for a provisioning command's `-secret-store`, and `llm-login` for `crewlet llm login` and `export` — see [Secret Store](../concepts/secret-store.md#the-design). The **author** is the node's to decide when the write goes through a running node — whoever the credential in `CREWLET_API_TOKEN` resolves to, `token:<id>` for a Tier A token or the owner with the token beside them for a machine token — and the line printed after the write is what the node recorded, never the shell's own user; written directly it is the shell's user, `$CREWLET_OPERATOR`/`$USER`, with no credential, since none made the write.

An engine reads the new value at its next config activation or start, not immediately — the command says so after each write, along with which way it went. Through a running node: `written to the fleet's secret store, through <url> (every node reads it)`, and that the running fleet reads it at its next configuration apply, which `POST /config/reload` to a node that is up brings forward. With the engine stopped: `written to the fleet's store, reached through this node's broker on <stream.store_dir> — this node's engine is stopped` (or `the external broker at <host:port>`), and that this node's engine reads it when it starts, while a node already running on the same store reads it at its next configuration apply, which `POST /config/reload` to it brings forward.

### `crewlet secrets list`

```
crewlet secrets list [-config PATH] [-api URL]
```

Prints one row per stored secret: name, sealing `key_id`, last-updated timestamp, who wrote it — with the credential beside them where one made the write and names something they do not (`jane.doe (through pat:…)`) — and source (`api`, `setup`, `provision`, `cli` or `llm-login`). It names the way it reached the store first — the running node it went through, or the broker it reached with this node's engine stopped — because a listing that did not say so is one an operator can read as the fleet they meant when it was not. **Never** prints a value — the listing drops the envelope on the way out and the route behind it has no value field at all. It never opens a value, so a row sealed under a key this keyring no longer holds — one dropped before its rekey finished — still lists, and an operator locked out of that key can still take inventory.

The engine's own key — the identity directory's blind-index key — shares the store and is **counted, never named**, on a line after the table, and so — through a running node, which holds the identity estate — are the identity estate's sealed personal values (names, addresses, second factors), which the same keyring seals and a rekey moves. No `crewlet secrets` command reads, writes or removes an engine key: `get`, `set` and `unset` refuse a name in the engine's namespace (`iam/…`) before they open anything, and the store refuses it again — through a node, as `403 reserved_name`. See [the secret store](../concepts/secret-store.md#the-engines-own-keys-share-the-bucket-and-never-the-namespace).

### `crewlet secrets unset`

```
crewlet secrets unset <NAME> [-config PATH] [-api URL]
```

Removes the record and says whether one was there — "was not set" is the outcome a cleanup script wanted on its second run, not a failure. Afterwards `${NAME}` falls back to the environment.

### `crewlet secrets get`

```
crewlet secrets get <NAME> -reveal [-config PATH] [-api URL]
```

Prints one decrypted value to stdout, with no trailing newline so it can be piped. `-reveal` is **required** — without it the command refuses and reads nothing. Through a running node, the route behind it (`GET /secrets/{name}`) is gated the same way, by an explicit `?reveal=true` no crawl reaches by accident, answers `Cache-Control: no-store`, and logs the access against the operator its guard authenticated; with the engine stopped the value is read from the fleet's store directly, and no node logs it. Either way the command logs `secret_revealed` with the name and the shell's user on its own log. Intended as break-glass for recovering a credential the upstream API will never show again, not as a scripting interface.

### `crewlet secrets rekey`

```
crewlet secrets rekey [-dry-run] [-config PATH] [-api URL]
```

Re-encrypts every stored secret not already sealed under `secrets.active_key_id`, and prints the names it moved — and the engine's own key with them, which it **counts** rather than names. **Through a running node it also re-seals every person's values in the identity estate** — the rows are derived from the identity log, so the node publishes one record per person still under an old key — and prints how many values of how many people it moved — every running node holds the identity estate from boot, company or none. **With the engine stopped** it re-seals every row of the fleet's store — your names and the engine's own `iam/` keys — but not the identity estate's personal values, which only the engine holding the estate re-seals, through its own log: the command says so, and is run again through a running node for those. The per-record counterpart of [`crewlet config rekey`](#crewlet-config-rekey) — run **both** before dropping a retired key from `secrets.keys`, or records still sealed under it become unreadable; for the engine's key and the identity estate that would be the blind-index key and every person's name and address at once. An **outstanding invitation**'s address is not re-sealed — only a re-issue could carry it again — so a run through a running node says how many are still under another key: keep that key on the ring until each is redeemed or expires, at most a week after it was issued. Each envelope names its sealing key, so mixed-key states decrypt correctly throughout. `-dry-run` lists what would re-encrypt without decrypting anything, reading the key id off each envelope instead, and counts the engine's key — and, through a running node, the identity estate's values — still under another key beside your names.

The store is shared, so this is run **once for the fleet**, not once per node. A run through a node's API sends the key id it expects and is **refused with a 409** if that node seals under a different `active_key_id` — a silent success there would report a rotation the fleet did not make. It aborts rather than half-completing if any record cannot be opened with the keyring in hand, because retiring the old key on the strength of a partial pass is what makes a secret unreadable for ever. And it never undoes a write that lands while it runs: each record is re-sealed only at the version it read, so a record somebody rotated meanwhile is judged again on what it holds now, and a record that was deleted stays deleted rather than coming back as the value the pass read before it went. A person whose re-seal nobody could confirm fails the run with what did move; running it again moves only what is still under an old key.

---

## `crewlet validate`

```
crewlet validate [<file.yaml>] [-tier auto|company|bootstrap|checks] [-json]
crewlet validate [-config <tier-a.yaml>] [-company <tier-b.yaml>] [-json]
```

Validates a config — or a repository's [checks
file](../concepts/code-host.md#the-checks-file) — and prints a summary,
reaching nothing: no broker, no store, no provider is dialled.

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

Validation is **deep**: it builds the `Organization`, so duplicate handles and
unit keys, bad cron expressions, invalid timezones, two seats declaring one
contact identity, and a knowledge scope with no backend behind it all fail here
rather than at run time. A human seat with **no** contact identity is not among
them: a person who works only through the dashboard has no chat account to
name, so validation admits the seat and prints a warning at its `contact`
instead. It reads
**no environment**: Tier B keeps `${VAR}` references verbatim, so a config
validates fully before any secret exists.
For the same reason a company with no `providers.llm` at all validates, and
its summary reports `0 LLM providers`: every node applies it, and its seats
hold their work until a provider is added (see
[A Company With No Model Provider](../concepts/configuration.md#a-company-with-no-model-provider)).

| Flag | Description |
|------|-------------|
| `-tier` | Which document a positional file is: `company`, `bootstrap` or `checks`. `auto` (default) reads the document's **keys**, not its filename. The two tiers share no top-level key, so `name`/`roles`/`units`/`providers` mean Tier B and `node`/`stream`/`store`/`coordination` mean Tier A; a document carrying `checks:` and no key of either tier is a repository's checks file — whatever else it carries, so a typo'd key beside `checks:` is refused at its own line by the checks file's reading. A document that carries none of them, or an equal count of both tiers', is **refused naming this flag** rather than guessed at: guessing wrong reports every field of the file as invalid, and an operator reading that cannot tell it from a genuinely broken document. |
| `-json` | Emit a machine-readable result on stdout instead of prose. |
| `-config` / `-company` | The two-tier form. Ignored when a positional file is given. |

A Tier A document's prose summary names what it resolved to, and a checks
file's names its checks:

```
crewlet.yaml: stream "nats", coordination "embedded-kv", store "…", roles [seats]
.crewlet/checks.yaml: 2 checks (unit, lint)
```

A **checks file** is read by the one parser the code host reads it with,
strictly: every key it does not know, every key written twice, a value of the
wrong shape and every value past its bound is a problem located at its field
and line, as a config document's is — the first 128 of them, and one more
problem, at no field, counting the rest ([The checks
file](../concepts/code-host.md#the-checks-file)). It reaches nothing a
repository holds: a check's secrets are named, never resolved.

With `-json`, the payload is `{"valid": bool, "tier": str, "file": str,
"problems": [...], "warnings": [...], "summary": {...}}`. `summary` is what the
document resolved to: a Tier B file's `company`, `seats` and `llm_providers`;
a Tier A file's `stream`, `coordination`, `store` and `roles`; a checks file's
`checks`, how many it defines, and `names`, theirs in its order; and for the
two-tier form the company's three beside `stream` and `coordination`. Each
problem is located and classified, so an editor,
CI job, or [AI authoring loop](../getting-started/ai-authoring.md) can fix
everything in one pass:

```json
{
  "valid": false,
  "tier": "company",
  "file": "company.yaml",
  "problems": [
    { "path": "roles[1].llm", "segments": ["roles", 1, "llm"],
      "kind": "unknown_value", "seat": "cto",
      "message": "roles[1].llm: value not in the allowed set: \"nonexistent\" is not a configured provider: providers.llm has primary. A key that misses is not an error at run time: the seat falls back to another model and bills against it, so this is the only place the typo can be seen" }
  ],
  "warnings": []
}
```

| Problem field | Meaning |
|---|---|
| `path` | The authored path. Empty only for a failure that belongs to no place in the document, such as a file that is not YAML. |
| `segments` | The same path as an array of keys (strings) and list indexes (numbers). A map key can hold a dot, so read these rather than splitting `path`. `null` when `path` is empty. |
| `kind` | `missing`, `out_of_range`, `conflict`, `shape`, `unknown_field`, `unknown_value`, or `invalid` for anything this build does not classify: a closed set with a fallback, because a consumer branching on it must never receive an empty string and read it as a field somebody forgot. |
| `message` | The full line, exactly as the prose output prints it. |
| `seat` / `unit` | The handle of the seat, or the name of the unit, it is about. Omitted when neither. |
| `line` | The line in the file, for a failure the parser found (an unknown key, a value of the wrong shape). Omitted otherwise. |

A rule that several seats or units break together, such as two seats sharing a
name, is one message and one problem beside each of them, so `problems` can
hold more entries than the prose output has lines. The prose output prints
that message once, led by every path it applies to.

Each warning is `{"kind", "ref", "path", "segments", "seat", "unit", "from",
"to", "message"}`, every field always present, and it is one of two kinds
here:

- **`dangling_reference`** — a reference that resolves to nothing: a unit
  `lead`, a root seat's `unit`, a `manages` entry, a GitLab access level.
  `ref` names which (`lead`, `unit`, `manages` or `gitlab_access_level`), and
  `from` and `to` are what holds it and what it names.
- **`advisory`** — a setting that is valid and carries a consequence: a
  declined fsync, a backup nobody owns, a human seat nobody can message, a
  token ceiling another one leaves idle, a repository whose default branch no
  protection rule covers. It is located at the field to change, and `ref`,
  `from` and `to` are empty. It is the only kind a Tier A document raises.

The third kind a `/config` answer can carry, `admission`, is a rule a stored
revision breaks, and a document validated here cannot carry one: it is a
problem instead. The engine runs a document with either kind, so a warning
never fails validation; prose output prints each on its own line, led by its
kind — `warning (dangling reference): …` or `warning (advisory): …`. A company
document that parses is checked for dangling references whether or not it is
valid; advisories are reported only for a document that validates, because a
warning printed beside a refusal is noise. Both lists are always arrays.

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
| `config` | Tier A YAML (positional, or `-config`; default `./crewlet.yaml`). Name it **once**: a second positional, or a positional alongside `-config`, is refused rather than resolved — the two would have to agree and nothing checks that they do. |
| `-check` | List pending migrations and exit **1** if there are any; applies nothing. This is what a deploy gate calls, and a gate that reported pending work and exited 0 would stop nothing. |

**A node without `data` has nothing to migrate.** Its store is `store.scratch`
— created fresh, at the binary's own schema, every time `crewlet run` starts —
so `migrate` refuses it and says so. The offline `config` and `search eval`
commands refuse a scratch store for the same reason: what they wrote into it
would be gone at the next boot. `secrets`, and every command that resolves the
company's `${VAR}`s, refuses one too, for another reason: a node without `data`
holds none of the fleet's store — on the embedded topology its broker is a leaf
of the members', which run it. Name a running node that holds data with `-api`
instead.

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

Token-budget usage lives in the fleet's [coordination store](../concepts/coordination.md#token-budgets-are-windows):
one counter per scope for the whole company, with a slot for each calendar
window — the day, the ISO week and the month on the company's clock — and
surviving restarts. A counter each node kept privately would make an org cap of
500k into N × 500k.

**This command talks to a running node**, not to a file. That follows from
where the counters live: on the default topology the coordination store is the
engine's own embedded broker, held by the running engine — a second broker on
its `stream.store_dir` is refused, naming the process that holds it, because
two writers on one store would be corruption rather than contention — so there
is nothing this command could open beside it. And a budget is a question about
a company that is running, which the running node answers.

```bash
crewlet budgets show                       # usage per scope
```

| Flag | Default | What it does |
|---|---|---|
| `-url` | the `api` block of the config named on the command line | The running node's base URL. A wildcard bind (`0.0.0.0`, `::`) becomes the loopback address, because a wildcard is not something anything can dial |

**The credential is `CREWLET_API_TOKEN` and nothing else**, here and on every
command that talks to a running node — `backup`, `retention`, `work`,
`objects`, `fleet`, `seats`, `secrets`, `config`, `iam` and `code` too. Every guarded route needs one,
reads included. It is never a flag: a token typed as an argument is in the
shell history, in `ps`, and in any CI log that echoes the command, so
`-token` is refused as an unknown flag. And it is never read out of the
config's own `api.auth.tokens`: that list is what the node *accepts*, and a
write authored by whichever entry came first landed under a name nobody
chose. The one command that reads no token is `iam token -login`, which mints
your own and so signs in as you for that request instead — a person's token is
theirs alone to mint. A `401` therefore has one thing to check — the value exported in
`CREWLET_API_TOKEN`. And the config is read for the node's address alone: with
`-url` given none is read at all, which is how a laptop or a CI job that holds
no Tier A file reaches a node.

The **ceilings** are not stored here: they are the company document's
`token_budget`s — the company's and each seat's — so every process derives the
same numbers without coordinating. Only the usage is shared.

`show` names the company clock the windows are cut on, then prints **one row
per window**: the company's day, week and month, then each seat's.

```
Windows on the company clock: Europe/Berlin

SCOPE  PERIOD  WINDOW      USED     LIMIT      STATE     RESETS AT             LAST REFUSED
org    day     2026-09-23  2710450  3000000    near      2026-09-23T22:00:00Z  -
org    week    2026-W39    9120045  unlimited  ok        2026-09-27T22:00:00Z  -
org    month   2026-09     31004188 unlimited  ok        2026-09-30T22:00:00Z  -
eng    day     2026-09-23  102120   100000     refusing  2026-09-23T22:00:00Z  2026-09-23T07:29:51Z
…
```

A window no ceiling caps still shows its spend, with a `LIMIT` of `unlimited`.
`STATE` is the engine's own judgement, the one every surface shows: `refusing`
when no charge fits — which every window that refused a round reaches, since
the refused round is counted — `near`
at nine tenths of the ceiling, `ok` otherwise. `LAST REFUSED` is when that
window last turned a call away — a round whose charge it refused, or work turned
away unsent because the window was already full (a turn's next call, a parked
delivery, a person's question, a reflection pass), which is recorded as the
gate's refusal too — printed only while its `STATE` is
`refusing`, and `-` otherwise: a window whose ceiling was raised after it
refused reads `ok` or `near` and shows `-` there, although it keeps the
refusal's stamp (`refused_at` on `GET /budgets`) until its next admitted
charge clears it. A refused round is
counted like any other, because the vendor billed it, so a window that refused
one reads `USED` past `LIMIT` by that round — the `eng` day above refused a
3 000-token round at 99 120 — and every later charge is refused against that
figure. The next charge the scope admits clears the stamp, and so does the
window turning over.

`show` refuses rather than printing zeros when the node reports it could not
read the counter (`durable: false` on the query surface). A counter nobody
could look at is not a counter that reads zero, and a table of zeros draws a
company at 0% of its budget at exactly the moment nothing is known. Seats with
no ceiling and no spend in any window are left out for the same reason in
reverse: three permanent zero rows per seat bury the seats that matter.

**There is no `reset`.** A window's allowance comes back when the window
turns over — at local midnight, on Monday, on the 1st — rolled inside the
charge that crosses the boundary, and room before then is made by raising the
ceiling through `/config` — a seat's with `PUT /config/roles/{handle}` — which
is a write with an author,
where a counter zeroed by hand left no record of who made the room or why.

---

## `crewlet backup`

```
crewlet backup [<config.yaml>] -dir <absolute path> [-url URL] [-wait DURATION]
```

Copies what a running node holds — its own store file, the replicated
estate's file on a node with the `data` role, and every JetStream stream and
coordination bucket — into one directory, and verifies each store copy before
calling it a backup.

**It writes to the engine's host, not yours.** `-dir` is resolved where the
node runs; nothing is downloaded. A relative path is refused rather than
guessed at, and a destination that already holds something is refused rather
than merged, so each run gets a directory of its own.

Like [`budgets`](#crewlet-budgets), this goes through the node rather than
opening files, and here the reason is doubled. The store is locked to the
engine's process for the life of the handle and the driver does not support a
second process on a database file — so no outside tool can read it, and
copying it anyway is a torn copy, because committed data lives in the file and
its `-wal` together. The stream estate is worse: on the default topology the
broker is embedded in the engine and **binds no socket**, so there is no
address to give the `nats` CLI. The one process that can reach both is the
engine, and this asks it to.

The report names what it captured, per estate. Every node holds its own store
whatever its `node.roles`, and a data node the replicated estate beside it. On
the embedded topology the fleet's streams come too: a member snapshots them
from its own broker, and a leaf, whose broker keeps no stream, snapshots the
same streams from the members across its leaf link. A node that dialled an
external NATS cluster copies its store files alone and says so, naming the
cluster as where the stream half is backed up, rather than presenting a
partial copy as a backup.

**The company's files come too**, as an `objects` line. On the default `nats`
object store their objects are in the `OBJ_crewlet_files` stream the backup
snapshots after the rest, and the line counts the objects the backup owes that
the stream holds, asked thirty-two at a time (lost ones are listed beneath); on
`s3` each object it owes is read from the bucket into `objects/`, thirty-two at
a time, and checked against its file's size and SHA-256 as it is written, and the
line gives their count and size. What it owes is every object a live row of the
copy names and every object a record the restore replays from the copy names —
a file written while the backup ran, counted as "(N written during the copy)".
A file
whose object the store answered it does not hold, or holds wrong, does not
fail the backup: it is listed beneath, as `PROJECT/path` and the object's key,
to restore from an earlier backup or upload again. The first one the store
could not answer about at all fails it, and nothing more is asked.

`-wait` (default 12 hours) bounds how long the command waits for the answer,
not the copy: the engine finishes what it started, so a wait that expires
leaves a good backup this command has already called a failure. It is set far
past any plausible duration for that reason.

One backup runs at a time on a node: asked for while another runs there, the
node answers `409 backup_running` and writes nothing, and the command says so.
A refusal the fleet's own state causes is printed with the node's hint saying
what to do: `409 record_unreadable` (a log the backup replays holds a newer
build's record — take it from a node running that build), `409
record_unverified` (a record signed under a key this node does not hold — add
the key to its `secrets.keys` and restart it, or take it from a node that
holds the key), `409 log_recreated` (this node's copy is keyed to a log since recreated — take it
once the node has re-keyed, or from one that has) and `503 log_gap` or `503
objects_unreachable` (take it again: the node's `Retry-After` is one second
for `log_gap`, since the next copy stands past the gap at once, and four for
`objects_unreachable`, the broker's own election timeout).

See [Backups & Restore](../guides/backup.md) for what the directory contains,
what the copy is a copy *of*, and how a restore uses it.

---

## `crewlet work`

```
crewlet work purge <item> -reason TEXT -confirm <item-key>
    [-op-id ID] [<config.yaml>] [-url URL]
```

The gestures on work items that belong on a **shell**. Everything else a
company does to its tasks is done by seats through their own tools, or by a
person through the [write surface](api-endpoints.md#the-human-write-surface)
the dashboard is built on. The one here is the operation no seat may perform,
which an operator acting on an erasure request runs from a terminal and a
runbook — and it goes through that same surface's own route,
`POST /work/items/{key}/purge`, so it is decided by the same authority table:
`admin`, and never a seat.

### `crewlet work purge`

Destroys a task and every row it produced: its own row, its comments, its body
revisions, its checklist, its field values, its watchers, its relations, its
dependencies, and every inbound
reference and the key directory row that made it resolvable. A marker is written in their
place, and every later record about that task is dropped for ever — which is
what stops a redelivery months afterwards resurrecting any of it.

`remove` hides a task and `delete` stops later records about it. Neither takes
the body out of a single database, which is why this verb exists: an erasure
request and a credential pasted into a task description both need the rows
gone.

`<item>` is the task's key or its id. **The confirmation is the task's key**,
and the node checks it against the task `<item>` resolves to — a mismatch
destroys nothing. The id may be what is on the command line, so repeating it
would confirm nothing; the key has to be looked up. **There is no `-project`**:
the task's own stored row says which project the record is filed under, where
a key's prefix names only where it was first filed.
**The reason is required** because it is the only thing that survives — the
marker's reason is the entire account of what used to be at that key for
whoever reads it a year later.

**Its children are moved, not destroyed.** Each direct child re-parents onto
the purged task's own parent, or becomes a root when the purged task was one.
Destroying the subtree would destroy work nobody confirmed; leaving it would
leave every child pointing at an id that resolves to nothing.

The outcome is three-valued, like every write here. `applied` means the rows
are gone on this node and the record is durable. `pending` means the record is
durable and this node has not applied it yet — **do not run it again**, because
a second gesture appends a second purge. `unknown` exits non-zero naming the
operation id; that id goes back in `-op-id`, which the command sends as the
request's `Idempotency-Key`, so the retry is the same operation and cannot
append a second record. A retry whose first purge did land answers `applied`,
at the position that purge landed at, rather than finding the task already
gone. The command mints that id **before** it asks and waits thirty seconds —
past the three five-second waits a write can make — so a purge the node never
answered, which may well have landed, still prints the `-op-id` to run it again
with. Pass the id exactly as printed: it carries the instant it was minted, and
a node whose operation ledger may have lost the first purge's row since — to
the ledger's thirty-day sweep — judges the retry by it, answering `unknown`
again rather than purging twice. An id the engine did not print is refused
(`op_id_invalid`), and so is one altered on the way back — trimmed, spaced or
over 128 bytes — since the broker would carry it as a different id.

What it does **not** reach: a node that is offline or evicted keeps its copy
until it replays, adopts a snapshot, is replaced or is destroyed. There is no
duration to state, and `crewlet retention status` names which nodes those are.

## `crewlet objects`

```
crewlet objects status [-json] [<config.yaml>] [-url URL]
crewlet objects migrate -to s3 -target <path> [-final] [-wait D] [-json] [<config.yaml>] [-url URL]
```

Where the company's files are kept, and what the
[object store's](../concepts/object-store.md) collector last found. It talks to
a running node, for `backup`'s reason: the collector's report is a record in
the coordination store, which on the default topology is the engine's own
embedded broker and binds no socket. `status` reads the fleet view's `objects`
block ([`GET /fleet`](api-endpoints.md#get-fleet)), which every node answers
from that one record.

### `crewlet objects status`

It prints the store the fleet's files are in — the fleet's own NATS bucket
(`OBJ_crewlet_files`) or an S3 bucket with its endpoint, bucket and prefix —
the node that ran the last passes of the `object-collector` duty, and one line
for each pass:

- **Last collection** — how many objects it listed in the store, how many it
  deleted for being older than a day with no row naming them, and how many
  uploads begun more than a day ago and never finished it abandoned.
  A collection that stopped judging says why (`stopped judging: …` — the
  node's view of the estate was incomplete, so a row it could not read might
  name any object) beside what it had deleted before it stopped; one that
  stopped says what stopped it, and one that did not finish says
  `incomplete`. A sweep of unfinished uploads the store refused — on S3, an
  identity without `s3:ListBucketMultipartUploads` or
  `s3:AbortMultipartUpload` — is a line of its own beneath.
- **Last audit** — from the last audit to run to its end: how many files the
  company's rows name, how many the store does not hold (**missing**), how
  many it holds at the wrong size or digest or as nothing it can read
  (**damaged**) and how many it failed twice to answer for while it answered
  for the rest (**unanswered**). An audit that failed after it is said
  beneath, never in its place. When any file cannot be read it lists them —
  the file, which of the three was found of it, and its object's key; the
  first hundred by name, and how many more — to restore from a
  [backup](../guides/backup.md) or upload again.

Before the collector has finished a pass — on a new fleet, in the minute or so
before a data node first claims the duty — it says so. `-json` prints the block exactly as the node answered it,
for a script. The exit status is non-zero when the node cannot say — its
coordination store did not answer, the node runs no object store, or it
answered a state this build does not know — and zero otherwise, files that
cannot be read included: a store that lost bytes is an answer, and the
[`objects_missing`](alarms.md) alarm is what pages for it.

### `crewlet objects migrate`

Moves the company's files from the fleet's own NATS bucket to an
S3-compatible bucket ([Moving the files to a
bucket](../concepts/object-store.md#moving-the-files-to-a-bucket) is the whole
procedure). It asks a **data node** to do it
([`POST /objects/migrate`](api-endpoints.md#post-objectsmigrate)), because only
the engine reads the objects, the records naming them and the coordination
record the move ends with.

- `-to s3` names where the files go; an S3-compatible bucket is the one other
  store there is.
- `-target` is an absolute path **on the engine's host** to the Tier A file
  every node will run with once the move is made: its `store.objects` block
  names the bucket, and its `${VAR}` references resolve from the engine's
  environment, so the bucket's credential never crosses the API.
- Without `-final` it runs a **copy pass** on a live fleet: every object a
  file names that the bucket does not hold is read from the fleet's store,
  checked against its row, and written to the bucket — and so is every object
  a [derived](../concepts/object-store.md#live-retired-and-derived-references)
  row names, which the engine would otherwise build again on every node. Run
  it as often as you like; it copies only what is missing.
- `-final` runs the pass that moves the fleet. Every node must be running
  with `crewlet run -mode maintenance`, so that no record naming a new file
  can be written while it runs. It copies what is left, seals the NATS
  bucket so a node not yet restarted cannot store anything there, and moves
  the fleet's record to the bucket.

It prints what the pass found: the objects the files name, how many the
bucket already held and how many were copied, the same for the derived
objects where there are any, whether the bucket offers direct uploads, any
file the fleet's own store could not give back (no copy can bring those back),
how many derived objects it could not give back (the engine builds those
again, so nothing is lost), and what to do next. `-wait` (six hours) is how long
it waits for the answer; the node finishes the pass either way, and running
the command again reports where the copy stands. `-json` prints the node's
report as it answered it. The token takes `admin`.

## `crewlet code`

```
crewlet code status [<config.yaml>] [-url URL] [-repo NAME] [-pushes N] [-json]
crewlet code verify [<config.yaml>] [-url URL] [-repo NAME]... [-repair] [-json]
crewlet code export <name> <file.bundle> [<config.yaml>] [-url URL] [-git PATH]
crewlet code import <source-url> <name> [<config.yaml>] [-url URL] [-git PATH] [-dry-run]
crewlet code credential [-token-env NAME] get|store|erase
```

The engine's own [git host](../concepts/code-host.md) from the command line.
`status` and `verify` ask a **running node** through its `/code/` routes
([API Endpoints](api-endpoints.md#the-code-host-git-and-code)), as whoever the
token resolves to: the repositories are rows the data nodes keep, and a git
node's cache is files only its own process touches, so nothing here opens
either. Like every node client they take the token from `$CREWLET_API_TOKEN`
alone and find the node from the Tier A file's `api` block — `-url` overrides
it, and with `-url` given no Tier A file is read. `export`, `import` and `credential` are **client-side**: your own git,
with your own environment, talks to the node's git routes exactly as anyone's
git does. The git URL of a repository is `<the node's API base>/git/<name>.git`.

`crewlet code hook` is the engine's own: git starts it inside every push. Run
by hand it says so and exits 2.

### `crewlet code status`

With no `-repo`, every importing, active and archived repository, read at the node's
default level (`stale`), as many a page as the node serves (500), every page
after the first at or past the first one's position, and on stderr the
position the listing was read at, so two runs can be compared:

```
NAME        STATUS    OWNER               BRANCH  HEAD     REFS  PACKS  SIZE      LAST PUSH
infra       archived  seat:platform-lead  main    77c0d2e  3     9      12.4 MiB  2026-09-30 08:11 UTC
nimbus-web  active    unit:engineering    main    3f2a91c  14    212    1.9 GiB   2026-10-10 14:02 UTC
read at CREWLET_CODE_LOG@0:4127
```

`HEAD` is the default branch's commit, `-` while it has none; `SIZE` is what
its live packs hold. A company that declares none says `no repositories:
code.repos declares none`, never an empty table.

With `-repo NAME`, that repository in detail and its newest pushes
(`-pushes N`, 10 by default, `0` for none). The repository is **read at
`linearizable`** — as it is now — and every page of its history at or past the
position it was read at, which the node serves with no barrier of its own, so
the command costs the code log one barrier however many pages it reads, and
asks for no more pushes than it prints:

```
web — active, owned by unit:engineering, default branch main
  managed by:   code.repos
  description:  The customer-facing web app.
  visibility:   unit — only the seats the chart relates to its owner read it
  search:       indexed — read from an index of its default branch, by a search across the company's repositories as well as one naming it
  protections:  refs/heads/main (no force, no delete), refs/heads/release/** (no direct push)
  refs:         12 branches, 30 tags; main at 3f2a91c0e4d1b5a7f8c2e9d0a1b2c3d4e5f60718
  packs:        212 live, 1.9 GiB, 1843021 objects
  limits:       push 1.0 GiB, file 50.0 MiB, repository 20.0 GiB
  pushes:       340 ever, the last 2026-10-10 14:02 UTC
  created:      2026-09-01 10:00 UTC

PUSHED AT             BY                                                  NODE    REFS  RESULT
2026-10-10 14:02 UTC  pat:019a2f4e-5c1d-7a3b-8e2f-6d4c3b2a1908 (founder)  data-b  1     ok refs/heads/main 1a2b3c4..3f2a91c
2026-10-10 13:40 UTC  token:ci (release-desk)                             data-a  2     ok refs/heads/x 0000000..9e8d7c6; ng refs/heads/main 1a2b3c4..77aa001 (stale)
```

`BY` is the credential that pushed, as every record a write leaves names it —
`pat:<credential id>` for a machine token, `token:<id>` for a Tier A token —
and, in parentheses, the human seat its principal holds: a person's own, or the
one the identity directory binds a token to; a seat's own push is the seat's
handle, and in parentheses the turn that made it and the work item it was
charged to (`docs-writer (turn <turn id>, native ENG-12)`) — led, for a push
the seat's coding box made, by the box's coding agent and its run
(`swe (claude-code run <launch id>, turn <turn id>, native ENG-42)`) — as the
engine stamped them when the push was made;
each ref's result is `ok`, `noop` or `ng`, with git's own `..` for a fast
forward and `...` for a forced move, and an `ng`'s reason. `limits` are the
serving node's `code.limits`. `managed by` is who changes the repository:
`code.repos` for a declared one, or the code tools for one a
[tool made](../concepts/code-host.md#repositories-made-by-tools) — a project,
or a seat's own scratch repository with when it was last active, the clock its
idle expiry reads. A deleted repository's header says when it was deleted and
how it comes back until `code.deleted_retention_days` have passed: by
declaring it again in `code.repos`, or for one a tool made, by a restore
through the code tools. A repository asked for by a name a rename left
redirecting to it is printed under its current name, with a `renamed` line
saying until when the old name redirects.

`-json` prints the node's answers as they came — one JSON document a page, the
repository's first with `-repo` — for a script. What the command holds while it
reads is what it prints: a page at a time with `-json`, and otherwise each
repository's or each push's row of text. A repository nobody declared is
refused in the node's words (`no repository named "web" is declared in
code.repos`), and a company that hosts no code says `code.native is not set`;
either exits non-zero.

### `crewlet code verify`

Checks **this node's** copy of each repository against its rows — every pack's
bytes against the digest its row names, the refs against what was written, and
git's own connectivity check — and with `-repair` repairs what it finds and
checks the copy again. Where everything it found is the copy's **own directory
out of place** — a file the engine does not write there, a fixed file edited
or replaced by a link, a pack no row names, a shape the node refuses to run
git in — the copy is put right **in place**: every pack, the
multi-pack-index, its bitmap and the commit-graph kept, nothing downloaded —
but a pack no row names, which leaves as the copy moves into its next
directory, taking the multi-pack-index and the commit-graph built over it
along, still with nothing downloaded ([When packs leave a
copy](../concepts/code-host.md#the-cache-on-a-git-node)).
A pack missing or damaged, or an index failing its checksum, is **rebuilt**
into a new copy (every pack found whole linked rather than downloaded again),
and so is whatever a repair in place leaves. Every git node keeps its own cache, so **verify each** (`-url`
names the node); with requests routed by repository, a repository is cached on
one or two of them. A verify is one of the deployment's own controls and takes
**`admin`**, so the token in `CREWLET_API_TOKEN` must carry it: any other is
refused naming it, before the node checks anything.

With no `-repo`, every importing, active and archived repository, listed at
`linearizable` so one declared a moment ago is not missed; `-repo NAME`, as
often as you like, verifies those — a name nobody declared is refused in the
node's words, naming `code.repos`. Each is verified in turn, **at the position
the listing was read at** (`min_position`): the node brings its copy there with
no barrier of its own, where a check at the log's head appends one, so a sweep
of every repository costs the code log one barrier rather than one a
repository:

```
web: ok (212 packs, 41 refs)
docs: not held on this node — nothing here to check (verify the git nodes that serve it)
api: 2 problem(s) (14 packs, 9 refs):
  pack 01a13ca4-… (pack-166cc99…) hashes to c953e5ea…; its row says b5999284…
  fsck: error: inflate: data stream error (incorrect data check) (and 2 more)
  run it again with -repair to repair this node's copy
```

and with `-repair`:

```
api: damaged, and rebuilt (14 packs, 9 refs); the check before the rebuild found:
  pack 01a13ca4-… (pack-166cc99…) hashes to c953e5ea…; its row says b5999284…
api: the rebuilt copy is whole
```

or, for a copy whose directory alone was out of place:

```
web: out of place, and repaired in place (212 packs, 41 refs); the check before the repair found:
  unexpected description
web: the repaired copy is whole
```

A rebuilt copy has no multi-pack-index, bitmap or commit-graph until the node
writes them again — minutes to hours for a large repository, its clones slower
meanwhile — which is why nothing a repair in place can end is rebuilt.

Damage the node had **already repaired** when the check began — its sync
before the check held a file to its checksum and dropped or fetched it again in
place — is told first and leaves the copy clean: the copy checked no longer
holds it, so it is neither a problem nor a reason to `-repair`, though a disk
that damaged one file may hold others damaged:

```
web: the sync before the check found damage on disk and repaired it in place:
  the sync before the check found objects/pack/multi-pack-index-3f2a….bitmap failing its checksum and dropped it to be written again
web: ok (212 packs, 41 refs)
```

A repository the node holds **no copy of** is passed over: nothing there can be
damaged, and the node does not build a copy just to check it. A rebuild that
leaves a problem names where it is — the object store or the rows — since a
copy rebuilt from them can be no better than they are.

**The check is the node's operation, not the request's.** It reads every byte
of the repository — measured at about nine seconds a gigabyte, nearly all of it
git's connectivity check — so the node runs it on its own time and a proxy's
read timeout ends nothing. The node answers a check that ends within a quarter
of a second — a small repository's — in the request that asked for it, and
otherwise the command reads it, a quarter of a second later and then at
doubling waits up to five seconds, until it ends. A refusal the node says
clears by waiting (its verify slot or the check's memory held, a node stopping)
is asked again after the node's own `Retry-After`, for up to five minutes, and
then reported. While the node checks the copy it says when it ends that check
by — a bound from what the copy holds, about 1.1 seconds a mebibyte, past
which it ends the check itself — and the command waits by it; a node still
saying it is checking well past that bound, or that stops answering for half a
minute, is given up on, saying how to read the check's outcome later, and
asking again hands back a check still running. A node that serves no verify at
all is said once, naming `-url`, and the sweep stops: it is the address that is
wrong, not a repository.

**Exits non-zero when any copy is not clean**, after verifying every one — a
problem found, a check that failed, a name nobody declared — and zero when all
are clean or not held: the hook for a cron on each git node. `-json` prints
each verification as the node answered it on stdout, and what it says in words
on stderr — a company that declares none among them.

### `crewlet code export`

Writes one repository — every ref and every object behind them — to a [git
bundle](https://git-scm.com/docs/git-bundle) on this machine:

```
$ crewlet code export web web.bundle
exported web: 42 refs, 1.9 GiB to web.bundle
$ git clone web.bundle web        # restores it, anywhere
```

It runs **your** git — the first on `PATH`, or `-git PATH` (a name is looked
up on `PATH`, a relative path taken from where you run the command), which
must be 2.40 or newer: the client floor, `gitproc.ClientMinVersion`, below
which it is refused before anything runs, as `gitproc: <path> is git
<version>, and a client of the code host needs git 2.40.0 or newer
(gitproc.ClientMinVersion, the client floor): install a newer git — on macOS,
Homebrew's: Apple's bundled git is 2.39; or name one with -git`. It runs it
with your environment and your configuration, as a `git clone
--mirror` through the node's git routes, then bundles the mirror, naming the
mirror to every command (`--git-dir`), so `safe.bareRepository=explicit` in
your configuration refuses none of them. Your token reaches git only through
`crewlet code credential`, in the environment of that one clone and never on a
command line, and only for the node's own URL: every credential helper your
configuration lists is set aside for that clone, so a stored password for the
same host is never sent instead, and a host a redirect sends git to is offered
nothing. The bundle is written as `<file.bundle>.partial` and **checked whole
before it is renamed into place**: its header is a bundle's with no
prerequisite, and its pack is read from start to end and its bytes hashed
against the trailer git wrote, so a pack cut short or with a byte changed —
which `git bundle verify`, reading only the header, calls okay, and a clone of
it refuses — is refused with nothing left at the path. An export that was
interrupted leaves nothing that looks finished either, and an interrupt
during the check stops it at its next read rather than after the rest of the
bundle; the mirror is
made in a directory **beside the bundle** and removed when the export ends, so
the bundle's volume needs room for the repository twice while it runs. A
repository with no refs yet, or one deleted, is refused with nothing written.

### `crewlet code import`

Pushes another host's repository — anything your git clones: an HTTPS or SSH
URL on GitHub or GitLab, another server, a path on this machine — into one the
code host serves, **as you**: every step is an ordinary push by the person your
token acts as, held to every rule a push is held to (`code.limits`, the
repository's protections, the blob limit, git's own object checks), and the
repository's push history names you for each. The engine dials nothing: your
git reaches the source with your own credentials, proxy and network, so there
is no fetch of a URL on the node to guard.

```
$ crewlet code import https://github.com/acme/web.git web
skipping 412 refs outside refs/heads/ and refs/tags/ (refs/pull/*): a forge's, not the repository's
cloning https://github.com/acme/web.git into /tmp/crewlet-import-2861
cloned https://github.com/acme/web.git: 63 refs (4 branches, 59 tags), 2.1 GiB packed
importing 63 refs in steps of at most 921.6 MiB (code.limits.max_push_bytes less a tenth), each measured before it is pushed
pushing step 1: refs/heads/main → 3f2a91c (905.1 MiB)
imported step 1: refs/heads/main → 3f2a91c (905.1 MiB)
pushing step 2: refs/heads/main → 9e8d7c6 (911.4 MiB)
…
imported step 5: 62 refs, refs/heads/develop … refs/tags/v3.2.0 (48.0 MiB)
imported web: 63 refs, 3.6 GiB sent in 5 steps, 3.6 GiB stored with the packs' indexes
```

In order:

1. **The repository.** It must be declared in `code.repos` and active — read at
   `linearizable`, so one declared a moment ago is found — and its
   `code.limits` are the ones the node holds its pushes to. An archived or
   deleted repository takes no push, and is refused saying how to declare it;
   a name nobody declared is refused naming `code.repos`, and a company that
   does not set `code.native` is refused saying so, in the node's words. The
   import also asks the node who the token is (`GET /viewer`): every push's
   record names that person, so it is part of what one push may carry
   (step 6).
2. **Before anything is downloaded**, your git (2.40 or newer, the client
   floor, refused below it as `export` refuses it — the first on `PATH`, or
   `-git PATH`) reads the source's own list of refs, and the import
   refuses a ref the code host takes no push of (git's ref rules, at most 255
   bytes of UTF-8), a branch the code host keeps for its seats' own namespaces
   (`refs/heads/seats`, or `refs/heads/seats/<name>` as a branch itself —
   [who may read and push](../concepts/code-host.md#who-may-read-and-push)),
   and more refs than one repository holds (100 000), naming the refs it
   refuses: rename them in the source. Where the
   source asks for a user name and password none of your credential helpers
   holds — or ssh for a host key on a first connection, or a key's passphrase
   with no agent — your git **asks you on the terminal**, here and in the clone,
   as a git you ran yourself would; with no terminal (a CI job) the question
   fails and the import says so. It then asks the node what a push's first
   request asks — a dry run of one — so a credential bound to nobody, which
   fetches only, or a node that takes no push here, is refused now, in git's words,
   rather than after the source has been downloaded.
3. **The clone**: a bare clone of the source's branches and tags, into the
   system's temporary directory — set `TMPDIR` to a volume with room for the
   source, since a temporary directory is often small or held in memory. Refs
   outside `refs/heads/` and `refs/tags/` — a forge's `refs/pull/*` and
   `refs/merge-requests/*`, notes, remotes — are skipped and counted: they are
   the forge's rather than the repository's, the code host takes no push of
   them, and the history only they reach is never downloaded.
4. **Every refusal the source shows, before any push**, listed together: a
   shallow source (import from a full clone of it), a source whose objects are
   SHA-256 (the code host keeps SHA-1 repositories), every blob past
   `code.limits.max_blob_bytes` — by its id, its path and its size, wherever
   it is in the history (large binaries belong in tracker files; a history
   rewritten without them imports) — and a source whose history is past
   `code.limits.repo_max_bytes` already as **one pack** (raise it, or import
   less): an import stores more than that (step 6).
5. **What the repository already holds.** A ref it holds at the source's value
   is done; a branch it holds at an earlier commit of the source's branch is an
   import that stopped part way — or a source that has moved on since — and is
   continued from there, through the commits that descend from the one it
   holds, so every step is a fast-forward even where that commit reached the
   source's branch through a merge. A ref at any other value — something else
   was pushed there — is refused naming the first such ref in the source's
   order, both values and that this is not a resumed import of the same
   history. A ref the repository holds that the source does not is left as it
   is, and named.
6. **The steps.** The default branch first — the source's branch of the name
   `code.repos` gives as `default_branch`, or, where the source has none, the
   source's own default branch under its own name (said, so you can push the
   repository's default afterwards) — advanced along its first-parent history,
   every step a fast-forward of the branch itself, so no temporary ref ever
   exists. Then every other branch and tag, in name order, in batches of at
   most 1 000 refs — fewer where the refs are long and spelled long in the
   code log (`&`, `<` and `>` take six bytes each there): each batch holds
   only as many as one push's record has room for, as the person the token
   acts as, which the node refuses past before anything of the push runs
   ([Limits](../concepts/code-host.md#limits)). Every step is **sent whole**
   (`--no-thin`), so the pack it sends is the pack the node stores, and is
   **measured before it is pushed**:
   its pack is built exactly as `git push` builds it, against what the
   repository will hold, its bytes counted and thrown away. A step is held to
   `code.limits.max_push_bytes` less a tenth for what it sends, to the
   1.25 GiB one push may store with its indexes, and — added to what the
   repository holds and what the steps before it stored — to
   `code.limits.repo_max_bytes`. Where a history is stored as a forge stores it
   — each file's newest version whole and its older ones as deltas against it —
   a step packs to far more than the source's own pack holds of it, so a step
   past what it may carry is cut shorter before anything is sent, and the
   summary says how many were. A branch no batch can carry is stepped along its
   own history. A **merge** whose other side is more than one step — a
   long-lived branch merged back, a subtree added, an unrelated history merged
   in — has that side's own history stepped first, to the merged-in commit and
   no further, on a branch of the source still to be pushed that reaches it:
   the branch whose history it is where the source keeps one, and otherwise
   any that reaches it, such as a branch created after the merge. That branch
   is taken out of its turn, every step a fast-forward of it, and goes on to
   its own value in its turn; a branch the repository holds carries it only
   where it holds it at an earlier commit of that history. The merge, measured
   once, then goes in a step of what is left. Where no other branch can carry
   it, the import is refused naming the merge, the merged-in commit and what
   its history weighs, with the remedy: a branch on that commit in a mirror of
   the source (`git clone --mirror`, then `git branch`), imported from the
   mirror. One commit, or one tag no branch reaches, that alone packs past the
   limit cannot be split, and is refused naming it — raise
   `code.limits.max_push_bytes` (at most 1 GiB). The next
   step is measured while one is pushed, so the second build of each pack —
   `git push` sends no pack it did not build — overlaps the upload and the
   node's work rather than adding to them, and a push that fails ends that
   measurement at once.
7. **The pushes**: each step is one `git push --porcelain --atomic
   --no-verify --no-thin`, through `crewlet code credential` exactly as
   `export`'s clone is. `--atomic`: a step lands whole or not at all.
   `--no-verify`: your own pre-push hooks are for your own pushes, and one that
   uploads elsewhere — Git LFS's — would refuse every step. And each is
   **pinned against your own push settings**, which would change what a step
   carries or have git refuse it: `--no-follow-tags` (`push.followTags` adds
   tags the step never planned), `--no-signed` (`push.gpgSign` asks for a push
   certificate the code host does not take), `--no-recurse-submodules`, push
   options cleared (`push.pushOption`: a `change` kept there would open a
   [change](../guides/code.md#open-a-change-from-a-push) from every step that
   pushes one branch, and an import is history, not a proposal) and
   `push.negotiate` off.
8. **The proof**: every branch and tag of the source, read back from the node
   as git lists it, at the source's value; any that differs exits 1 naming it.

**A stepped history stores more than one pack of it**: every step keeps whole
the files its deltas cannot reach in the steps before it, so each file a step
touches is stored again in full. Measured on this project's own history,
34.0 MB as one pack: 68.5 MB in eleven steps and 78.6 MB in forty-two. What
the repository stores is what the object store holds and what every git node
serving it caches, so run a large import with `code.limits.max_push_bytes` at
its 1 GiB ceiling — the fewest, largest steps — and size
`code.limits.repo_max_bytes` for the steps rather than for the source's own
pack. A step that would take the repository past it is refused **before it is
sent**, saying what the repository would hold, what is left of the source, and
to raise the limit and run again.

**A failed step is safe to run again**: the import stops at the first step git
refuses, printing git's own line for every ref it refused — `!
<id>:refs/heads/frozen [remote rejected] (protected_direct: …)` — with git's own
words for why, which a ref's line does not always carry (an object the node's
object checks refuse is `(unpacker error)` there, and its reason is git's
`remote: error: object …: badTimezone: …`), and saying how many steps of the
run landed before it. Fix the cause and run the same
command again: step 5 finds what landed and pushes the rest. The protections
apply to an import as to any push, so a branch whose rule says `direct_push:
none` refuses its own import: lift the rule for the import and restore it
after.

`-dry-run` does steps 1–5 and prints the plan — every step, sized from how the
source stores its objects (`≈`), with nothing measured and nothing pushed, and
a step marked where its estimate passes the push limit or takes the repository
past `code.limits.repo_max_bytes`. A merge whose estimate passes the push limit
has its other side's history planned first, on the branch the run would carry
it on, and the plan says so as the run does (`stepping … first`). A real run's
steps can differ from it, since a step is cut where its measurement says.

Git LFS: the import carries the pointer files a history holds, never the large
files an LFS server keeps beside it; the code host serves no LFS.

Exits 0 once every ref is proven, and 1 on any refusal, a stopped step or a
ref that differs.

### `crewlet code credential`

git's [credential helper](https://git-scm.com/docs/gitcredentials) for the code
host. Configured for the node's address:

```sh
export CREWLET_API_TOKEN=…
git config --global credential.https://code.example.com.helper ''
git config --global --add credential.https://code.example.com.helper '!crewlet code credential'
```

— the empty value first, so the node's address has this helper alone: git
hands a credential it was answered to **every** helper it lists to store, and
a storing helper your system configures (osxkeychain on macOS, the credential
manager with Git for Windows) would otherwise keep the token, answer every
later request with it before this helper is asked, and send a rotated token's
old value until a refusal erased it. Configured so, it answers git's `get`
with `username=crewlet` and the token in
`$CREWLET_API_TOKEN` (`-token-env NAME` reads another variable) as the
password — the user name is ignored, the token is the credential. It answers
**only over HTTPS**, or plain http to this machine (`localhost`, `127.0.0.1`,
`::1`): over plain http to another host it says it will not send a token in
the clear and answers nothing, and git asks its next helper. An empty variable
answers nothing too. `store` and `erase` read git's request and do nothing —
the token's home is the variable, never a file this helper writes.

## `crewlet fleet broker`

```
crewlet fleet broker list [-json] [<config.yaml>] [-url URL]
crewlet fleet broker remove <node> -confirm <node> [-force] [<config.yaml>] [-url URL]
crewlet fleet broker remove -peer <peer> -confirm <peer> [-force] [<config.yaml>] [-url URL]
crewlet fleet broker credential [<config.yaml>] -o FILE [-url URL]
```

The fleet broker's membership. Two records say who its members are: every node
advertises its **broker kind** on its presence lease — derived from its
`stream` block, never from its roles (see
[Configuration](../concepts/configuration.md#noderoles)) — and the broker's
**metadata group**, the raft group that places every stream and consumer,
counts its voters for itself. `list` and `remove` talk to a running node, and
the node asks a member where it is not one: only a member holds the group, and
a removal is answered only on its system account, which nothing outside its
process can reach. They are clients of the routes in the
[API reference](api-endpoints.md#the-brokers-membership). `credential` is for
the broker gestures no verb performs, made with the `nats` tool on a member's
own host.

### `crewlet fleet broker list`

The question it answers is **does the broker still count a member I have
lost?** One row per live node — what it advertises, its roles, how the group
counts it (`leader`, `current`, `offline, last heard 5m ago`, or `-` for a node
that is no voter) and its **peer id** where it is a voter — then one row per
voter no live node is, and under the table every disagreement in words.

The group counts each voter by its raft peer id, which is derived from the
node id. A member hears another's name only from that server, so a voter whose
survivors have restarted since it died is listed as `(name unknown)` by its
peer id alone; a voter is matched to its node by peer id, never by the name the
answering member happens to have heard.

- **DEAD MEMBER** — a voter no live node is, or whose node came back as a leaf
  or a client. Every election and every create goes on counting it; the line
  prints the `remove` command for once it is not coming back — by `-peer` for a
  voter nobody can name, since it has no node id to give.
- **NOT COUNTED** — a live node advertising a member the group does not count:
  still joining, or removed while it ran.
- **UNKNOWN** — a node whose presence does not say what its broker is, which a
  capacity seal counts as a member.

A group no member could report is said to be unread, never shown as empty. On a
fleet whose broker is an external cluster it says so and lists nothing: that
membership is the cluster's own operator's. On a clustered member it also
prints the answering node's **client listener** — on loopback, reachable from
that node's own host alone, where `credential`'s key reaches the broker.
`-json` prints the node's answer as it came.

### `crewlet fleet broker remove`

Stops the metadata group counting a member once the group has committed the
change — printing which member's system account carried it and the voters that
remain. Name it by **node id**, which reaches it by the peer id the id hashes
to whether or not any member still remembers its name, or by **`-peer`** with
the peer id `list` shows, for a voter nobody can name. **Refused while its node
holds a live presence lease as a member** (or without saying what its broker
is): it is still running, and a running member removed from the group rejoins
it as a voter at its next restart, so stop it first. `-force` removes it anyway,
for a member wedged in a way that still renews its lease. A node alive under
the voter's name as a **leaf** or a **client** is removed without `-force`: the
member it was is gone for good. The member being removed never carries its own
removal — if it is the only live member, the command says so.

The whole removal is bounded by one wait: the carrying member's commit budget
and a round trip. A carrying member that does not answer ends it as an outcome
nobody knows — it may have committed the change first — rather than another
member proposing it again; `list` reads the group. So does an answer the node
cannot believe: what it asks a member is signed under the fleet keyring and
bound to that one request, and so is the member's answer, because the member
carries a removal out through its system account and the broker's admission
vouches for a connection, never a request. A member that holds no key the
request is signed under runs nothing and the next member is asked.

Only the group's leader answers a removal, and a member removed because its
host died was often that leader — so the removal asks again every second while
the survivors elect another, and a removal made during an election waits it
out rather than failing. A group that answers nobody for the whole wait (two
minutes) has lost its quorum and can change nothing about itself, and the
refusal says so: bring enough members back for a quorum, then ask again. A
removal the node did not answer may still have been committed; `list` reads
the group, and asking again for a member already removed is refused as not a
member.

### `crewlet fleet broker credential`

Writes the **operator key** an embedded fleet's broker admits on a clustered
member's loopback client listener, for the `nats` tool: the gestures no engine
verb performs — above all resizing every stream and bucket before a raised
`stream.replicas` lets a node start
([Deployment § Reaching an embedded fleet's broker with the nats tool](../guides/deployment.md#reaching-an-embedded-fleets-broker-with-the-nats-tool)).
Run it on the member's own host:

```bash
crewlet fleet broker credential -o ./operator.nk
nats --server nats://127.0.0.1:41234 --nkey ./operator.nk stream ls
```

The key is derived from the active key of THIS host's `secrets.keys` — read
from its own Tier A, never fetched — and written to `-o FILE`, created `0600`
and refused where anything is already there; it never reaches the terminal. The
address is asked of this host's running node (`GET /fleet/broker`, the token in
`CREWLET_API_TOKEN` taking `admin`): its port is the OS's at every start, and a
listener on loopback is its own host's, so an answer from another node — a
`-url` naming a balancer or a peer — is refused and nothing is written. A solo
member, a member with only a leaf listener and a leaf open no client listener,
and say so; a node on an external cluster is reached with that cluster's own
credentials. A member serving no API (`api.port: 0`, no `-url`) is asked
nothing: the invocation is printed with the address left for you, and the
node's `broker_keys` line names it.

The key reaches every stream and bucket and never the system account —
membership changes go through `remove` — and it grants nothing its holder
lacks: whoever reads the node's Tier A holds the keyring. It is admitted for as
long as its key is in `secrets.keys`; write it again after a rotation drops it.

## `crewlet seats`

```
crewlet seats pause <handle> [-stop] [-reason TEXT]
    [-op-id ID] [<config.yaml>] [-url URL]
crewlet seats resume <handle>
    [-op-id ID] [<config.yaml>] [-url URL]
```

Pause an agent seat, or resume it — the same `pause_seat` and `resume_seat`
the dashboard's buttons call, over the same route
([`POST /operator/act/{tool}`](api-endpoints.md#operatoract--the-dashboards-write-surface)), so a pause from a
terminal and one from a profile screen are one gesture with one record. The
node decides it on the principal `CREWLET_API_TOKEN` resolves to: the seat's
holder, whoever leads it, or an administrator. The write is recorded
under the caller's own name, kind and credential, as every other write is —
nobody is refused for being bound to no seat; a token with the authority acts
under its own login.

A paused seat starts no new turn, its incoming mail waits on its inbox in
order, and its scheduled runs are recorded `skipped_paused` rather than sent.
The turn it is on finishes first unless `-stop` is given, which ends that turn
at its next round; a stopped turn is not run again. `-stop` also refuses every
change a detached coding run of the seat makes until the seat is resumed: a push
from its box, and a call through the [tool
bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)
of any of the seat's tools not marked read-only. The run itself runs on: its box
may still fetch, and its bridged reads still answer. `-reason` is one line
(at most 500 characters), shown on the seat and in the feed. Pausing a paused
seat changes nothing — except that `-stop` adds the stop — and resuming a seat
that is not paused changes nothing. See
[Agent Runtime § Pausing a seat](../concepts/agent-runtime.md#pausing-a-seat).

The outcome is `applied` — the pause is the fleet's record, and the node
holding the seat carries it out from its own copy within about a second — or
`unknown`, when the node could not confirm the write. Every invocation sends
an operation id as the request's `Idempotency-Key`, which the act route
requires: `-op-id` takes any id in the engine's operation-id grammar, and
without it the command mints one, `<uuidv7>.pause` or `<uuidv7>.resume`. An
`unknown` outcome, or no answer at all, prints that id; send it again with
`-op-id` so the retry is the same operation rather than a second one. Pass it
exactly as printed: it carries the instant it was minted, which a node reads
to decide whether it can still vouch for a retry.

## `crewlet artifacts`

```
crewlet artifacts list [-scope mine|shared|company|agents|all] [-kind html|doc|slides|dashboard|sheet|design]
    [-q TEXT] [-status live|trashed] [-work-item KEY] [-limit N]
crewlet artifacts get <id>
crewlet artifacts publish <dir|file> [-artifact ID] [-title TEXT] [-description TEXT]
    [-icon NAME] [-kind html|doc|slides|dashboard|sheet|design] [-entry PATH] [-exclude PATTERN]...
    [-label TEXT] [-contract VERSION] [-capabilities a,b] [-work-item KEY] [-force]
    [-op-id ID]
crewlet artifacts update <id> [-title TEXT] [-description TEXT] [-icon NAME] [-op-id ID]
crewlet artifacts versions <id>
crewlet artifacts restore <id> <version> [-label TEXT] [-op-id ID]
crewlet artifacts label <id> <version> <label> [-op-id ID]
crewlet artifacts share <id> <seat:HANDLE|unit:KEY|company> <role> [-op-id ID]
crewlet artifacts unshare <id> <seat:HANDLE|unit:KEY|company> [-op-id ID]
crewlet artifacts trash <id> [-op-id ID]
crewlet artifacts untrash <id> [-op-id ID]
crewlet artifacts purge <id> -confirm <id> [-op-id ID]
crewlet artifacts assets <id>
crewlet artifacts sources [<name>]
crewlet artifacts usage
    ... [<config.yaml>] [-url URL]
```

The company's artifacts from a shell, through the same routes the dashboard is
built on ([Artifacts](api-endpoints.md#artifacts)), as whoever
`CREWLET_API_TOKEN` resolves to. The node decides every gesture on that
principal exactly as it decides the dashboard's: publishing, renaming and
labelling are an editor's and ask the artifacts app, sharing and withdrawing a
share are the owner's, a move to the trash and back is the owner's and a
person's, and a purge is an administrator's with a fresh proof of identity
and a person present, which a machine token never is.

**`publish` moves only the bytes the artifact does not hold.** It declares
every file first (its path, its SHA-256 and its size; a directory is walked,
any name starting with a dot is left out with everything beneath it, and a
symbolic link inside it is never followed), and
the node answers which files the artifact already holds, which are carried by
their digest and move no byte, and how each of the rest is sent: straight to
the bucket on a presigned put where the object store offers one, through the
node otherwise. Then the version is published.

`-exclude PATTERN` leaves files out of a directory, once per pattern
(`-exclude '*.map' -exclude drafts`). A pattern with no slash matches a file's
or a folder's name at any depth; one with a slash matches its path from the
directory published (`notes/*.md`, where `*` stops at a slash). A folder a
pattern matches is left out with everything beneath it, and a leading `./` or
a trailing `/` is ignored. The grammar is Go's `path.Match`: `*` and `?`
within one name, `[...]` for a class, `\` to escape. A malformed pattern, one
reaching outside the directory, or `-exclude` beside a single file is refused
before anything is sent. Quote a pattern so the shell does not expand it.

A new artifact opens on the one default entry it holds, `index.html`,
`index.md`, `doc.json`, `deck.json`, `dashboard.json` or `canvas.json`, else the only file, unless `-entry`
names another; a directory holding two default entries is refused until
`-entry` or `-kind` settles which it is. Its kind is the one whose entry that
file is: `html` for a page, `doc` for a `.md` file or a tabbed doc's
`doc.json`, `slides` for `deck.json`, `dashboard` for `dashboard.json`,
`design` for `canvas.json` (an index is told by its path, never by JSON
alone), and `-kind` names it outright. Its title defaults to the entry's
name, and `-description` and `-icon` set the gallery card's subtitle and the
tab's icon word. Those four describe a new artifact only: naming `-title`,
`-description`, `-icon` or `-kind` beside `-artifact` is refused before any
byte moves, since a version keeps the artifact's kind and `update` is what
changes the rest. A new version of `-artifact` names the head it read as its
base, so a version somebody else published meanwhile is refused rather than
overwritten; `-force` publishes over it, a person's gesture. A new version
keeps the head's runtime contract and capabilities unless `-contract` or
`-capabilities` names others: `-capabilities db,state` declares exactly that
set, `-capabilities none` declares none, and a name this build does not serve
is refused before any byte moves. The result line counts the files two ways:
how many this command sent, and how many of the version's files are new and how
many the artifact already held, as the node counted the manifest that landed.
The two differ for an unchanged page: a page is stored in its linted form, so
its own digest never matches what the artifact holds and it is sent, and the
node then finds the linted page already held and carries it. The publish
policy's warnings are printed beneath the result.

**`get` and `versions` show each version's runtime contract and
capabilities**: `get` the head version's on its `contract` and `can` lines,
`versions` one `contract` and `can` per version. Either reads `none` where
there is none (a Markdown document or a sheet pins no contract; a version
publishing `-capabilities none` declares none), so a version whose
capabilities were stripped shows as such.

**`update` changes the title, the description or the icon** (`PATCH
/artifacts/{id}`, an editor's) and sends exactly the flags it names: an
empty value clears the description or the icon, a field it does not name is
left as it is, and a call that names none is refused before anything is sent.

Every write sends an operation id as the request's `Idempotency-Key`: `-op-id`
takes one in the engine's operation-id grammar, and without it the command
mints one. An `unknown` outcome, or no answer at all, prints it; send it again
with `-op-id` so the retry is the same operation rather than a second one. A
publish keys its upload intent with the id and its version with a step of it,
so a retried publish reuses the upload and lands one version.

**`sources` lists the dashboard source catalogue** the node answers: each
engine source a `dashboard`'s datasets may read, what it reads, who may read
it, its parameters (with their choices and defaults), its columns and the push
it refreshes on, or one source whole when named. A source is answered as each
board's viewer, so the list holds no row; see the
[Dashboard Spec](dashboard-spec.md#the-engine-source-catalogue).

## `crewlet retention`

```
crewlet retention status|snapshots|ack|evict|readmit|set-capacity|maintenance|reanchor|verify
    [<config.yaml>] [-url URL]
```

What the state log is holding, why it is not shrinking, and the gestures that
change it. A **group** rather than nine top-level verbs, because several of
them stop a machine writing and that should not sit at the same level as
`version`.

Every verb but one talks to a running node, for `backup`'s reason: the register
they read and write is a coordination bucket on a broker embedded in the
engine, which binds no socket, so there is no address any other tool could be
given. The exception is `verify --restore`, which reads an artefact off disk on
purpose — see below.

### `crewlet retention status`

A domain this node **refuses** leads, before anything else about it, with the
refusal's own sentence — and for a `wrong_stream`, which finding is behind it
(`recreated`, `ahead_of_log`, `log_diverged`, `generation_passed`):

```
NOT READY pages: wrong_stream (recreated): pages's rows are keyed to the stream created at … and the broker's CREWLET_PAGES_LOG was created at …
WRITES REFUSED tracker: log_truncated: peer node-4 stands at sequence … of CREWLET_TRACKER_LOG, which ends at …
```

Then the blocking term, **in prose**, because it is the answer to the only
question anybody runs this for:

```
Nothing is being trimmed on tracker: the newest backup is 72h0m0s old, past the 24h0m0s this deployment accepts.
```

Then one row per registered domain — its stream, generation, replay protocol,
both ends, bytes, what it took in over the last day (`PER DAY`), ceiling,
reserve, headroom and trim floor — the six terms with their state and detail,
one row per node per domain, and this node's own replica line. `TRIM FLOOR` is
`-` where the trim has concluded nothing about the domain's generation yet —
right after a reanchor, until its first tick on the adopted stream — and
`unreadable` where the floor register could not be read; the watermark block
says which instead of listing terms. Each node row carries its position's
generation (`GEN`), and a position from a generation the log has left prints
`left gen N` (or `ahead gen N`) in place of a lag, since its sequence compares
with nothing the log holds now; a node that reported its log diverged at its
checkpoint is marked `LOG DIVERGED`. `RESERVE` is the top of the ceiling every
identity-claiming log — the tracker's, the knowledge base's, the identity
estate's and the code log — keeps for gate records ([the gate
reserve](../guides/retention.md#the-gate-reserve)), `-` on the vector
changelog and the usage log, which keep none; `HEADROOM` is what is left of the rest, the
ceiling ordinary writes are refused at — so a log at `0%` still takes an
eviction.

A term that does not apply to a domain prints `n/a` rather than `0`: the
vector and usage logs and the identity estate's have no wake feed, and an
absent term is a different fact from one that permits nothing. Where a log does
have one, its `feed_ack_floor` is that log's own feed — never another domain's.
`PER DAY` prints `-` for the same reason where nothing was measured — a
compacted log, a log in its first two days, a node whose trim has not ticked —
and `0 B` only for a log that took in nothing; see
[Retention](../guides/retention.md#the-one-rate-and-what-it-is-held-against).

**It exits non-zero exactly when this node has an active alarm**, on the
report's own rule rather than a second one in the CLI — the shell script
watching this exit code would otherwise be watching the definition nobody
maintained. Each alarm's measurement and remedy go to **stderr**, so a cron
capturing stdout for a dashboard still gets the reason in its own mail. An alarm
that stands on logs names them: one log in its headline (`log_headroom on
usage: 3% of the log's byte ceiling for ordinary writes is left`), several as a
line each with the term holding the log's trim (`  tracker (backup_floor): the
trim has not advanced for 46h 33m, and the log is keeping up to 1840 record(s)
older than its 24h replay window`).

`-domain <name>` narrows the log and watermark blocks to one domain.

### `crewlet retention snapshots`

Its own verb rather than a block of `status`, because the repository is per
node: *which of my machines can donate, and how old is what they hold* is a
disk question. A node with no artefact still gets a row, carrying the reason —
`sole_node`, `lagging`, `unhydrated`, `deferred`, `insufficient_space`,
`ahead_of_log`, `log_diverged` or `failed` — because the absence is the answer to "why did the join fail". A
node that holds a current artefact carries no reason at all: `recent` is the
skip a healthy node takes, so it is never published as one.

### `crewlet retention ack`

```
crewlet retention ack -stream CREWLET_TRACKER_LOG -position 918100000
```

Publishes an operator backup floor. Under `backup_floor: operator` the trim
follows this rather than the engine's own copies, because a backup is not a
backup until it leaves the host and the engine cannot see that it has. Both
flags are required: an acknowledgement moves the floor the trim deletes
against, so there is no value to guess.

### `crewlet retention evict` / `readmit`

```
crewlet retention evict node-4 -confirm node-4
```

An absent node pins the `applied` term for ever — its position never advances,
so nothing above it can be deleted. That is deliberate for a node that is
coming back; eviction is the gesture for one that is not.

`-confirm` repeats the node id, the same shape the other destructive gestures
use. The trim counts nodes per log, so the eviction is a record on every log it
counts nodes on — the tracker's, the knowledge base's, the identity estate's
and the code log — and each answers on its
own line with its [three-valued outcome](../guides/replication.md#a-write-has-three-outcomes),
or `not written` and the reason that stopped it:

```
$ crewlet retention evict node-4 -confirm node-4
evict node-4 (operation 01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4)
  tracker: applied at CREWLET_TRACKER_LOG 918280002
  pages: applied at CREWLET_PAGES_LOG 4410
  iam: applied at CREWLET_IAM_LOG 58311
  code: applied at CREWLET_CODE_LOG 2207
  node-4 stays COUNTED for about a minute, so a live node is certain to have read its own tombstone before the trim passes it.
  tracker trim floor 918100000 → 918100000
  pages trim floor 4100 → 4100
  iam trim floor 57000 → 57000
  code trim floor 2100 → 2100
```

The command prints the watermark before and after and the instant the
eviction takes effect: **the node stays counted for about a minute**, so a live
one is certain to have read its own tombstone before the trim passes it.

The node you ran it on writes every log itself: every data node holds the
whole estate, so one command reaches every log.

The gesture is **judged once, before any log is written**. A node that still
holds a live presence lease is refused with `409 eviction_refused` — it is
still reaching the fleet and almost certainly running, and an eviction would
drop everything it writes and take its copies of the logs it is evicted from
out of service. Stop it and wait
for its `LIVE` column in `crewlet retention status` to read `no`, or pass
`-force` for a node wedged in a way that still renews its lease — the refusal
prints both. A node
that cannot read the presence leases at all answers `503 eviction_unjudged`;
`-force` takes the eviction past that as well, since the leases are all the
judgement reads, and the refusal says so. A run that already passed `-force`
is never advised it again. A
node id no node could run under is refused `400 invalid_gate` before anything
is judged.

When **not every log holds the record**, the command exits non-zero, and under
each log it did not finish it prints what to do. Where the same command can
finish that log — an `unknown` outcome, a node still catching up — it names it,
with `-force` carried over from a forced run:

```
  pages: unknown (the record may or may not be on the log)
    its outcome is unknown: the same gesture under the same operation id answers from this log's own ledger if the record landed, and writes it if it did not
  The gesture has not reached every log. Run it again with -op-id 01a0cd85-… to finish it: a log that already holds the record answers from its own rows and is not written twice.
```

Run it again with that `-op-id`. Each log's record is published under an id
derived from it, the sign, the log and the node, and each log's snapshot reads
that id's ledger row before anything is decided — so a log whose record is
already the gate in force answers at the position it has, and only the missing
log is written, however long after the first run the retry comes. A fresh id
would be a second eviction rather than this one finished, and an id that
is not in the engine's grammar is refused. Where the same command **cannot**
finish a log straight away it prints what has to happen first, and then the
same `-op-id` that finishes the gesture once that is done: a `log_full` log —
full past even the reserve its gate records may use, since a log full only for
ordinary writes still takes them — needs
[`crewlet retention set-capacity`](#crewlet-retention-set-capacity):

```
  pages: not written (log_full): pages: statelog: unavailable (log_full): …
    CREWLET_PAGES_LOG is full to its broker ceiling, past even the reserve kept there for gate records: raise its ceiling, which is the only thing that makes room for it — then the same gesture under the same operation id finishes it, where a fresh one would write every log that already holds the record again
    crewlet retention set-capacity CREWLET_PAGES_LOG <bytes> -confirm <bytes>, then run this again with -op-id 01a0cd85-…
```

Not the same command now — it is refused the same way until the ceiling moves
— and **not a fresh one afterwards**: once there is room, this gesture's own
`-op-id` is what finishes it, because a fresh id is a second eviction that
writes every log already holding the record again — here the tracker's and the
identity estate's — and re-dates the eviction there. A node that is
itself `evicted` cannot write, so it prints `-url <that node>` with the same
`-op-id`; a `wrong_stream` log needs a [reanchor](#crewlet-retention-reanchor)
first, and then the same `-op-id`; and a `superseded` operation — an eviction
retried after a readmission took the node back — is a new gesture, without
`-op-id`. The indented sentence is the node's own and names no flag,
because the dashboard renders the same answer; the line under it is this
command's rendering of the node's `actions` (see
[the gate answer](api-endpoints.md#the-three-retention-gestures-that-write)).

The command mints the operation id **before** it asks, and waits a minute for
the answer — past the fifty seconds the node takes at most to answer one
gesture (twenty to judge it and thirty to write every log), which it finishes
even if the connection drops. So a gesture that got
no answer at all still prints the `-op-id` that finishes it — and so does one
whose answer the node did not write: a reverse proxy's 504 page, any status
carrying no engine error code, or a 200 cut off part way through, each of
which says nothing about what the node did. Pass it back
exactly as printed: the node refuses (`op_id_invalid`) an id that is not in
the engine's grammar — a UUIDv7 carrying its mint instant — and one over 128
bytes or holding anything but visible ASCII, since the broker would carry that
as a different id.

`readmit` is the inverse commit rather than a delete, so the eviction's whole
history survives a replay. It is **refused** while the node is below a trim
floor — and the refusal prints its position beside the floor, because that
inequality is the reason:

```
$ crewlet retention readmit node-4 -confirm node-4
crewlet: the node answered 409: readmission_refused
  statelog: node-4 may not be readmitted: its last position in tracker (reported 2031-04-02T03:14:00Z) is 1200 and records below 9000 may already be gone from the tracker log (published floor 9000, first surviving sequence 8800) — it has to catch up before the fleet counts it again.
  Start node-4 if it is not running: it catches up on its own, …
  its SEQ in `crewlet retention status`, at the domain's own GEN, says when it has caught up, and `crewlet retention snapshots` whether a peer can donate one; then run this again
```

The comparison is the one the node's own write fence makes: its **last
published position** in each log that claims identity — the tracker's, the
knowledge base's, the identity estate's and the code log — against the **higher of the published floor and the log's first
surviving sequence**, and the node must have applied every record up to the
one just before it. A node that has never published a position is judged as
holding nothing; one whose position is from a generation the log has since
left is refused, since nothing it holds compares with anything the log still
has. The judgement is made once, for every log, before any is written, and
nothing is written to any of them on a refusal. A readmission that reaches
some logs and not others is finished with `-op-id`, exactly as an eviction
is.

What to do is let the node catch up, which it does on its own: start it if it
is not running, and it replays what the log still holds or adopts a peer's
snapshot where it does not (`crewlet retention snapshots` says whether any peer
can donate). Readmit it once its `SEQ` in `crewlet retention status` has
reached one less than the higher of that domain's `TRIM FLOOR` and `FIRST` — the
refusal's own `floor`, `first_seq` and `generation` are the numbers it
compared, and right after a reanchor the domain shows `-` in `TRIM FLOOR` until
the trim's first tick on the adopted stream, so the bound is `FIRST` alone. The
node's `GEN` column says whether its position is at the log's current
generation: one from a generation the log has left prints `left gen N` in place
of a lag and is refused whatever its `SEQ` reads. The
position is a heartbeat old, so a node that has only just caught up can be
refused once more; run the command again.

A readmission that cannot be **judged** is refused as well, with nothing
written: `503 readmission_unjudged`, when the positions register, a published
floor or a log's first surviving sequence could not be read. The node's
position is not what is wrong there, so it is not told to catch up: the
command is asked again, on this node or through another (`-url <that node>`).
And either gesture on a node in a
[capacity window](../guides/retention.md#changing-a-logs-ceiling) is refused `409 not_publishing`
before anything is judged; its line points at `crewlet retention status`, which
leads with the window while it is open, since nothing is written to any log
until the fleet is back in normal mode.

The refusal is the truth about the node rather than the thing keeping your data
safe. Readmitting a node below the floor used to succeed, and it put back the
pin the eviction had lifted: the trim, counting that node again, stops
advancing. Its writes were never the danger — its own fence refuses any write
that assumes a history it does not hold.

### `crewlet retention set-capacity`

```
crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 -confirm 8589934592
```

Changes a log's byte ceiling, raising or lowering it. A log's Tier A ceiling
(`stream.tracker_log_max_bytes`, `stream.tracker_vectors_max_bytes`,
`stream.pages_log_max_bytes`,
`stream.iam_log_max_bytes`, `stream.usage_log_max_bytes`,
`stream.code_log_max_bytes`) is not a live setting, only the value its stream is created with. A resize is decided
against the usage the log is at, and a
publisher makes that a moving quantity, so this runs with the whole fleet in a
maintenance mode and costs **three fleet-wide restarts**, two more per retry. The
[procedure is documented once](../guides/retention.md#changing-a-logs-ceiling),
in the retention guide; this command's help prints the five steps.

`-confirm` repeats the byte count, and the target is then fixed for the life of
the operation: a verify compares the observed ceiling against it, so a target
that could move would make a mismatch unreadable.

A raise the broker has no room for is refused **before** the maintenance window
opens, naming what the raise would reserve, what the broker has left, the limit
it is held to and the field that sets it — so an operator learns it in one
round trip rather than after three fleet-wide restarts. A raise the broker
refuses at the apply is reported as a refusal rather than as an unknown
outcome, with the same numbers, read at that moment rather than carried from
the window's opening: an operation spans restarts, and what matters is what the
broker had when it said no. A limit this node could not read is **not** a
refusal — a node that could not hear has not been told no — and the operation
goes ahead for the broker to decide.

`-i-have-excluded-all-publishers` is required only on `stream.type: nats`.
There the engine does not run the broker and cannot establish who else holds a
connection to it, so the assertion is yours in your own words rather than a
check that quietly proves nothing.

A target whose ordinary ceiling is at or below what the log already holds is
refused before the window opens, naming both numbers and the least target that
would do: that ceiling would refuse every ordinary append the moment it
applied. On every identity-claiming log the ordinary ceiling is the target less
its [gate reserve](../guides/retention.md#the-gate-reserve) — the larger of a
sixteenth of it and seven of the log's largest records with a mebibyte beside
them; on the vector changelog and the usage log, which hold no gate, it is the
whole target. A target under the floor Tier A holds the log's own field to is
refused too, naming the field — a gibibyte on the tracker's, the knowledge
base's, the code log's and the vector changelog's, 64 MiB on the identity
estate's and the usage log's. A target under the
current ceiling and above the usage is accepted, and is how a log gives a
reservation back. A raise the broker cannot
reserve is refused before the window opens too, naming what it reserves and
what the broker has left, wherever the node can read that limit: a lone
embedded node, or a NATS account's own limit. A clustered member cannot, and
there the broker refuses such a raise when the window applies it.

Run from a node in `normal` mode it refuses outright, naming the restart. Run
from `maintenance` it opens, baselines and applies; run from `seal` it collects
the barrier, seals, verifies and confirms. It prints the phase it reached and
the next gesture, never the transition table.

### `crewlet retention maintenance`

```
crewlet retention maintenance status  -stream CREWLET_TRACKER_LOG
crewlet retention maintenance exclude -stream CREWLET_TRACKER_LOG -node node-4 -confirm node-4
crewlet retention maintenance abandon -stream CREWLET_TRACKER_LOG -confirm capacity-01J...
```

`status` is the one page an operator can see why a fleet is still excluded: the
operation, its phase and attempt, every participant's baselined incarnation
against what it acknowledged as, which acknowledgements are missing, any
unresolved write attempt, and any admission still blocking activation. On a
fleet with no window it says so — *no window* and *a window in phase `opened`*
are different facts with different next steps.

`exclude` is your assertion that a participant's **process is stopped** and
holds no outstanding request. It is the only thing that waives an
acknowledgement; an eviction does not, because that is about whose records
apply and this is about whose process is running. The window records it as
yours — `status` prints the participant as `excluded by` you, with the
credential you used — and an `abandon` that leaves the window open is recorded
and printed (`ABANDONED`) the same way.

`abandon` changes what the operation is trying to reach and never the barrier
it must cross. From `opened` it clears outright — no request was issued. From
anywhere else the fleet still has to restart into `seal`, because a paused
coordinator's request is outstanding whether or not a person has read a status
page. `-confirm` repeats the operation id from `status`.

### `crewlet retention reanchor`

```
crewlet retention reanchor -stream CREWLET_TRACKER_LOG
crewlet retention reanchor -stream CREWLET_TRACKER_LOG -confirm 2031-04-02T03:00:00.418226517Z
```

The recovery for a stream that was genuinely recreated, or for a broker
restored from an older copy. Run without `-confirm` it **prints the live
stream's own `created_at`, and which case it is, and refuses**: the
confirmation means *I looked at the thing I am re-anchoring*, and a verb that
read the value and fed it straight back would be confirming against its own
output. Paste the instant back exactly as printed — it is the same one a
`wrong_stream` refusal names as the live stream's: the creation instant
stamped into its configuration (`crewlet.created_at`), which a snapshot
restore keeps.

The three cases are where the log is followed **from**:

- **recreated** — the stream is not the one this node's rows are keyed to, so it
  is followed from its first surviving record;
- **restored** — it is the same stream, ending below this node's checkpoint or
  written past it since, so the rows already hold every record the copy kept
  and it is followed from its end — one below the generation record the verb
  appends, so a record written while it ran is not applied past it either —
  and none of them is applied again. If the log
  also holds records the rows do **not** — written after the restore, by a node
  whose rows were the copy's age — they would be applied on no node, so the
  verb names the newest of them and refuses unless you pass `-discard`. The
  alternative to discarding them is to keep them: do not re-anchor this node,
  replace its rows with a peer's instead;
- **abandoned** — it is the same stream and holds every record the rows are
  missing, but it continues in a generation only a peer the fleet has since
  **evicted** held; it is followed from this node's own checkpoint, in the
  generation after the evicted peer's, and every record of the generation it
  skips is void.

A same-stream log that reaches the checkpoint, holding there the record the
checkpoint names, and continues in no abandoned generation is none of these,
and the verb prints that there is nothing to re-anchor rather than a command to
run. The answer names the case and the sequence the checkpoint went to — and,
with `-discard`, the newest record it discarded.

`-discard` is its own flag rather than part of `-force`: `-force` says the
fleet cannot be asked who is most caught up, which says nothing about what a
log holds, and forcing past an unreadable register must not discard writes on
the same keystroke.

It moves **only the log you name**: that domain's checkpoint goes to the next
generation on the live stream — the one after every generation the domain has
used, an evicted peer's included — every other domain's stays where it is, and
the domain's applier resumes on the node with no restart.

For every log that keeps a generation record — the tracker's, the knowledge
base's, the identity estate's and the code log — it refuses while any peer
has already re-anchored the stream, **naming the peer** — that peer's rows are the fleet's
history in the new generation, and this node adopts its snapshot instead. A
peer that is gone for good is released by evicting it first
([`crewlet retention evict`](../guides/retention.md#eviction)): an evicted peer counts
toward neither this rule nor the next. Otherwise only the most caught-up node
on the stream its rows came from may run it, among the peers holding history
the log does not — a peer whose checkpoint record the log holds is on the log
and is not weighed — and the refusal names the peer further along; `-force`
overrides that rule, and
an unreadable positions register, and never a re-anchored peer. Nor does it
ever open a generation another node already opened: if a peer got there first,
the verb reads its record back, names it, and commits nothing.

For a recreated log it does not recover records that were on the old stream
and were never applied here, and the refusal says so. The generation record it
publishes names you — your login and the credential you ran it through — on
every node. See
[Re-anchoring a recreated or restored log](../guides/retention.md#re-anchoring-a-recreated-or-restored-log).

### `crewlet retention verify --restore`

```
crewlet retention verify --restore -dir /var/backups/crewlet [-cadence 720h]
```

**The one verb here that talks to no node.** It restores the newest artefact
under `-dir` and opens the copy — the point is to establish that the artefact
alone is enough, and running it through a running engine would be asking the
thing under test to test itself. It writes nothing to the live store and takes
no lock on it, so it is safe beside a running node.

It prints what the artefact holds and **exits non-zero past its cadence**,
which defaults to 30 days. Put it in cron: a lapsed restore test that exits
zero is a paragraph in a runbook nobody read.

A directory with no manifest is **debris** rather than a partial backup — the
manifest is written last — and an artefact naming no domain position cannot be
verified whatever else it contains, because a restore replays from that
sequence.

See [Retention](../guides/retention.md) for the six terms, the snapshot
repository and the join runbook.

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

So it accepts what the decoder reads, not merely what a field's type
suggests, and a test offers every field of both tiers each such value to
hold the two to the same verdict:

- **An empty value** — `~` or `""` — anywhere the engine reads it as unset,
  which is everywhere but the company document's root.
- **A number or a boolean in a text field**, read as the text it was
  written as (`name: 2024`, `org_webhook: false`).
- **YAML 1.1's switch words** in a boolean field — `yes`, `no`, `on`, `off`,
  `y` and `n`, each in YAML's three casings (`yes`, `Yes`, `YES`) — because
  the decoder reads them there. A quoted `"true"` is not one of them: it is
  text, and a boolean field refuses it.
- **A `${VAR}` reference in Tier A** wherever the engine resolves one into
  a value the field can take: anywhere in any text field, a patterned or
  closed-set one included, since the value is judged only once it has
  resolved; and as the **whole** value of a number or a boolean field, whose
  resolved text is read as if it were written there. A reference with other
  text around it in a number or a boolean is refused, by the schema and the
  engine alike.
- **A Tier B reference as the literal text it is.** The company keeps a
  reference verbatim and resolves it only where a provider or transport is
  built, so a field with a pattern or a closed set — an enum, a handle, a unit
  id — refuses one as it would any other text that is not a value of it. The
  exception is a field whose consumer resolves a **whole** reference, which
  takes exactly one beside its own rule: today that is a Mattermost seat's
  `username`. See
  [Environment Variable References](../getting-started/configuration.md#environment-variable-references).

### Credential positions: `x-crewlet-secret`

Every position that holds a credential carries the annotation
`"x-crewlet-secret": true` — on the field itself for a single value
(`integrations.github.webhook_secret`, a `providers.llm` entry's
`auth.api_key`), and on `additionalProperties` for each value of a map
(`mcp_servers[].headers`, a `providers.llm` entry's `env`, a seat's
`sandbox.env`, and both levels down in `mcp_env`, whose values are per-server
maps of variables).
A map's **keys** are never marked: they are names (`GITHUB_TOKEN`,
`Authorization`), and only what they map to is a credential. A position
without the keyword holds none, and the keyword's only value is `true`.

It is read from the same declaration on the config types that drives
[redaction](../concepts/configuration.md#reads-and-export), and a test
holds the two to one set in both directions: in the company schema, a
position is marked exactly when `GET /config`, `crewlet config show` and
`crewlet config export -redact` mask it. The Tier A schema marks its own by
the same rule — the keyring's `secrets.keys[].material`,
`api.auth.tokens[].token`, `stream.token` and
`store.objects.s3.secret_access_key` — though no read surface serves Tier A
to redact. `store.objects.s3.access_key_id` is not marked: AWS treats an
access key id as an identifier rather than a secret — it travels in every
signed request's `Authorization` header and appears in CloudTrail records
and error messages — and only the secret access key proves possession. A
consumer that prefers every value of that block to arrive by reference
may still write a `${VAR}` there; the mark only says where a literal is
wrong.

It is an **annotation, not a rule**. The engine runs a literal credential
in a marked position (it seals it at rest and masks it on every read), so
the schema accepts one there, and a validator that does not know the
keyword ignores it, as JSON Schema 2020-12 requires. What a consumer does
with it is its own policy; the one the engine is built for:

- **Write a whole `${VAR}` reference there, never the value.** In the company
  document the reference is stored verbatim and resolved where the provider
  or transport is built — from the [secret store](../concepts/secret-store.md)
  first, then the environment — so a tool that renders a company (a
  Kubernetes operator from its `Secret`s, a CI job from its vault) pushes the
  value to the secret store with `PUT /secrets/{name}` and writes
  `${NAME}` into the marked position. In Tier A the reference resolves from
  the node's environment.
- **Treat a literal in a marked position as a finding** — an editor warning,
  a linter failure, or a refusal in tooling that must never hold a credential
  in a document it versions.
- **Mask it when displaying a document**, as the engine's own reads do,
  unless the value is exactly one `${VAR}`, which names a credential and
  carries none.

The mark lives in the published schema, and a consumer acts on it there.
It does not survive being copied into a Kubernetes CustomResourceDefinition:
a CRD's schema accepts only its own `x-kubernetes-*` extensions (and no
`$ref`), so a generator that builds one from fragments of these files has
to strip `x-crewlet-secret` and keep the policy in its own code.

Both documents are checked into [`schema/`](../../schema/); a test
regenerates and compares them, so a config field added without a schema
entry fails the build rather than leaving a stale file nobody opens.

Point your editor at it for completion, inline field docs, and typo
squiggles:

```yaml
# yaml-language-server: $schema=https://docs.crewlet.ai/engine/latest/schema/company.schema.json
name: "Acme AI"
```

See [Authoring with an AI assistant](../getting-started/ai-authoring.md).

---

## `crewlet search eval`

```
crewlet search eval [-store PATH] [-config crewlet.yaml] [flags]
```

Measures the semantic half of knowledge search — the two-stage 1-bit retrieval
described in [Knowledge System](../concepts/knowledge-system.md) — against the
exact scan it approximates, **on your own vectors**.

There is no other number that answers this. The engine's own quality gate
measures the arithmetic on a seeded fixture and deliberately makes no claim
about recall on a particular company's documents: a 1-bit code keeps only each
vector's orthant, and how much that says about ranking is a property of your
corpus's distribution. Over a family of embedding-shaped generators the same
arithmetic spans 0.29 to 0.98 recall.

**Ground truth is the exact scan's own top-K**, so nobody authors a judgement —
which is the step that otherwise makes an evaluation stop being run. The
queries are held-out documents from the corpus itself, spread deterministically
across it so two runs are comparable.

**It reads a file, not a running node.** The replicated estate is exclusively
owned by the running engine, so pass the copy inside a
[backup](../guides/backup.md) — which needs nothing stopped and measures the
same rows — or the node's own file with the engine stopped. With neither
`-store` nor a reachable Tier A file it has nothing to open.

```console
$ crewlet search eval -store /var/backups/crewlet/2026-09-01/store-replicated.db
corpus       118432 sources, text-embedding-3-large at 3072 dimensions
measured     25 queries at depth 150 from 1200 candidates
first stage  the semantic index: 128 of 1024 lists probed (generation 1099511744562)
index        measured on 118432 sources: 0.9812 against a 0.9800 floor in its worst shape (container:task), 0 head miss(es)
recall       0.9761  (floor 0.9312 for this corpus size)
worst query  0.9467
head misses  0  (documents dropped from the exact top ten)
scan recall  0.9803 with 0 head miss(es) — the same search with the full scan as its first stage
narrowed     source:page      recall 0.9950  floor 0.9800  head misses 0  (25 of 25 scanned)  — scan 0.9950, 0 head miss(es)
narrowed     source:task      recall 0.9772  floor 0.9368  head misses 0  — scan 0.9810, 0 head miss(es)
narrowed     container:task   recall 0.9967  floor 0.9800  head misses 0  (19 of 21 scanned)  — scan 0.9967, 0 head miss(es)
narrowed     container:page   recall 1.0000  floor 0.9800  head misses 0  (4 of 4 scanned)  — scan 1.0000, 0 head miss(es)
verdict      the two-stage search recovers the exact ranking at the shipped depth, in every shape
window       8192 bytes a source (the corpus's opening): a semantic search sees each source's title and body up to it, a keyword search the whole body
past window  task  2110 of 104208 sources (2.0%), 7.4 MiB of 196.0 MiB of text (3.8%)
past window  page  9874 of 14224 sources (69.4%), 402.1 MiB of 518.6 MiB of text (77.5%)
```

The last three lines are printed on every run: for each corpus, how many
sources and how much of their text lie past the window each vector was
computed from — what a search by meaning cannot see, which a keyword search
still reads ([Knowledge System](../concepts/knowledge-system.md#what-the-quality-of-this-can-and-cannot-be-promised)).
That part of the report reads every source's whole body.

| Flag | Default | What it does |
|---|---|---|
| `-store PATH` | from `-config` | The replicated database to measure. Overrides the Tier A file |
| `-config PATH` | `./crewlet.yaml` | Tier A, read only for the replicated store's path |
| `-queries N` | `25` | How many held-out documents to measure over. Each one is a full exact scan, which is what bounds the run |
| `-limit N` | `150` | The depth recall is measured at — the shipped returned depth |
| `-candidates N` | `1200` | The stage-1 candidate depth — the shipped pair |
| `-model NAME` | most populated | The embedding model to measure |
| `-dimensions N` | the model's | The width to measure |
| `-probes N` | the index's own | How many lists of the semantic index to probe. `0` takes the count the index's own training measured; a larger one shows what probing more lists would recall |
| `-window BYTES` | the smaller of 8 192 and the model's own per-input bound | The bytes of each source the vectors were computed from, for the report of what lies past them. The store does not carry the company's configuration, so state it when `providers.embeddings.max_input_tokens` or `max_batch_tokens` lowers the per-input bound below the model's own — the embedding duty embeds the smaller of 8 192 bytes and that bound |
| `-fit` | off | Also print the corpus's own mean pairwise cosine, which is the parameter the engine's seeded fixture is fitted from |
| `-metrics` | off | Print one `key value` line per metric instead of a report, for a collector or a shell — the window report included, as `search_eval_window_bytes` and, labelled by `source`, `search_eval_window_sources`, `search_eval_window_beyond_sources`, `search_eval_window_text_bytes` and `search_eval_window_beyond_bytes` |

**It exits non-zero** when the recall is below the floor for that corpus size,
or when any document was dropped from the exact top ten, in any shape — so it
can go in a schedule. Both conditions matter: an aggregate of 0.98 is compatible with
losing exactly the documents that mattered, and a semantic-only document the
first stage drops leaves the fused answer entirely.

Run it **monthly, and after any change to `providers.embeddings.model` or
`providers.embeddings.dimensions`** — those are the two inputs that move the
answer. A report naming more than one embedding space is a refill in progress:
until it finishes, documents still on the old model are not in the candidate
pool at all, because the scan filters on the model/width pair.

Below the floor, the remedy is decided in advance and in this order: raise the
quantization over-fetch (measured free in latency — stage one is a full scan
whose cost does not depend on how many candidates it keeps), then an 8-bit
first stage, which ships in the same release that moves the model default.

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

# every command takes -company PATH, -config PATH and -api URL
```

The operator side of the [LLM backends](../concepts/llm-backends.md): every
`providers.llm` entry drives a vendor's own coding CLI — on the operator's
subscription plan, or on an API key under `auth.mode: api-key` — and the login
that makes a subscription work is established here rather than in the config
document. `KEY` is the `providers.llm` key; commands that take one and are
given none act on the only entry when there is exactly one, and `doctor` given
none examines every entry, in the order the document declares them.

**`list`** is the inventory — provider key, CLI, model and how it is signed
in. **`doctor`** is the one to run before a company's first turn: it
checks the CLI is installed, that something signs it in, and — unless you pass
`-no-smoke` — that a real completion comes back, which is the only check that
catches a plan whose quota is exhausted, or a profile whose `text_paths` no
longer find the answer in what the CLI prints. The same run measures two claims the
profile makes rather than trusting them: that the CLI's own **shell is
refused** (it runs on the engine host, so a denial that stopped working is a
seat reading whatever the engine user can read) and that its **web tool
reaches the network** (the one local tool every profile deliberately keeps
on). Both are believed only on evidence a model cannot invent — the current
clock, read by the tool. Its `system prompt` lines say where a seat's system
prompt goes and where a side pass's does — added to the CLI's own, in place of
it, or in the transcript under it ([which channel a call
takes](../concepts/llm-backends.md#which-channel-a-call-takes)) — and every probe
is sent as a seat's call, so it runs under the prompt a seat does.

Both commands answer "signed in" the same way, from the environment the CLI is
actually given rather than from the configuration: credential files a login
wrote (or, for OpenCode 2, which keeps its login in its own database, the
document its export printed), a headless token, the key `auth.mode: api-key`
puts in the CLI's key variable (`auth.api_key`, or that variable itself —
`ANTHROPIC_API_KEY` for Claude Code, `CODEX_API_KEY` for Codex; Qwen Code's
`OPENAI_API_KEY`, which other vendors' keys share, only through `auth.api_key`), a
variable `auth.mode: inherit-env` forwards, or — for a CLI whose profile reads
its provider's key from its own environment (`hermes`, `pi`, `opencode`) — a
[credential-named](../concepts/llm-backends.md#6-a-provider-key-in-env-opencode-hermes-pi)
variable in the entry's `env`. `doctor`'s `sign-in` line names every one it found, and
an entry with none is a `no sign-in` problem naming the routes its `auth.mode`
reads ([how `doctor` judges a
sign-in](../concepts/llm-backends.md#how-doctor-judges-a-sign-in)): a login or a
headless token under `subscription`, only the key under `api-key`, and the
variables it forwards or a login under `inherit-env` — unless
the smoke test was answered, which proves the CLI authenticates some way the
engine does not hand it (an endpoint that takes no key, a credential in its
own configuration). A token `auth.mode: api-key` removes from the child is not
counted. `${VAR}` references and the variables `inherit-env` forwards are
resolved in the process running `crewlet llm` — the secret store first, then
that shell's environment — so run it with the engine's environment to get the
engine's answer. Beside a running engine the store's values are read through
it, as every command that reads the company reads them
([Commands](#commands)), so `crewlet llm` runs while the seats do —
revealing only what the entries it works on read (the one entry a key names,
or every entry for `list` and a bare `doctor`), with one of Tier A's tokens
in `CREWLET_API_TOKEN` when one of those is stored. With the engine stopped it
reads the same store itself, through this node's own broker or the external
one, and a captured token or an exported bundle is written there — so a login
made before the first `crewlet run` is in the store the engine reads when it
starts. **On a `seats` node, which holds no data**, the command does not
reach the company's store on its own, and the logins of the seats it runs are made there:
`-api URL` names a node that holds data, which the stored values are read
through and a captured token or an exported bundle is stored in. Without it
the command refuses, naming the flag — as it does on a member of a cluster
whose engine is stopped, where `-api` names a running member.

`llm list`'s `SIGN-IN` column is the first route `doctor` would name — both
restore the entry's configured bundle first, as the engine does:
`credentials` (files a login wrote), `token` (a headless token), `api key`
(under `auth.mode: api-key`), `inherited token` or `inherited key` (forwarded
by `auth.mode: inherit-env`), `environment` (a key in the entry's `env`), or `none`.

`doctor` exits non-zero when any entry has a problem, and an entry that does
not build at all (a `${VAR}` model that resolved to nothing, say) is reported
as one beside the others rather than ending the run. A configured credential
bundle that does not restore is a problem of its own, naming `crewlet llm
export` as the fix, since the engine would run that entry signed out. It also
reads the URLs a box dials back to the engine (`CREWLET_MCP_BRIDGE_URL`,
`CREWLET_SANDBOX_OTEL_RECEIVER_URL`) exactly as `crewlet run` reads them at
boot: one the engine would refuse to boot on is reported first, as the node's,
and fails the run whatever modes the entries run in, since a node that does
not boot runs no seat.

**`login`** has four shapes because the vendors do:

| Shape | When |
|---|---|
| *(no flag)* | Broker the vendor's own interactive login and keep the result |
| `-from-host` | Adopt a login this machine already has, e.g. from running the CLI by hand — copied for a login kept in files, exported by the CLI itself (`opencode auth export --standalone`) for one kept in its own database |
| `-capture-token` | Mint a **headless token** into the secret store — the only shape a remote [code sandbox](../concepts/code-sandbox.md) can use, because a token is one scoped revocable variable and a login (credential files, an exported login) never leaves the engine host |
| `-capture-token -print-token` | The same mint, written to **stdout** and stored nowhere — for an operator whose secrets live in somebody else's manager. It **refuses to run on a terminal**: this is a credential, and a token in a scrollback outlives the command, while a screen-share or a shell history outlives the scrollback. Pipe it or redirect it. The two-step alternative (`-capture-token`, then `secrets get -reveal`) writes the token into the store on the way past, which is precisely what this avoids. |
| `-token-stdin` / `-username U -password-stdin` | Store a credential you already hold, where the CLI genuinely has one |

A headless token is read only under `auth.mode: subscription`, so `-capture-token`
and `-token-stdin` are **refused before anything is minted, read or stored** for
an entry whose mode would never read one, naming what that mode reads instead —
the words `doctor` uses. Under `api-key` the CLI's token variable is removed from
its environment, so the error names the key: `auth.api_key`, else the CLI's key
variable, stored with `crewlet secrets set` or exported. Under `inherit-env` the
token variable is forwarded from the engine's own environment and the secret
store is never read, so storing a token is refused while `-capture-token
-print-token` is not: export what it prints as that variable in the engine's
environment, which is what the line it ends with says. `-from-host` recommends
`-capture-token` after it runs only under `subscription`.

The brokered and `-username` shapes **fail when the CLI left no login** where
the entry reads one — exiting 0 having stored nothing, as backing out of the
vendor's sign-in does — rather than printing that a login was written. `login`,
`status` and a vendor's own `logout` run the CLI one at a time per state
directory and CLI (entries sharing a `state_dir` and driving one CLI share one
login home): a second started while the first is running is refused, saying
so. Only the commands
that make a login create the shared one; `status` and a seat's call write back
only a refresh of the very login they started from, so neither reverts a newer
login nor brings back one `logout` removed — and the bundle restore `status`
makes first declines after a `logout` (below). A login whose export failed
after you signed in is kept and written by the next command — unless you replaced
the login since with `-from-host`, `import` or `logout`; a `login` that cannot
carry such a kept login out replaces it, saying so — see [token refresh across
seats](../concepts/llm-backends.md#token-refresh-across-seats). A
`login` likewise replaces a shared login the CLI refuses to import (OpenCode's
document from another build, say), saying so, where `status` keeps refusing it.

**`export`** packs a login into one portable blob so another host can come up
already authenticated: the entry's credential files, and a manifest naming the
CLI they were exported from (`crewlet-bundle.json`). `-secret-store` writes it
to the [secret store](../concepts/secret-store.md) instead of stdout; without
it the blob goes to stdout in the clear, which is what you want when piping
into your own secret manager and never what you want in a shell history.

**`import`** is the other half, and it reads the bundle from **stdin** — a
credential on argv is visible in `ps` and lands in shell history, and the
natural spelling is a pipe anyway:

```bash
crewlet llm export claude | ssh other-host crewlet llm import claude
```

It **refuses to overwrite an existing login**, loudly: a host that has been
running holds the fresher refresh token, and restoring a boot-time blob over
it is how a fleet logs itself out. `crewlet llm logout <KEY>` first if you
mean to replace it. And it refuses a bundle that does not name the entry's CLI
as the one it was exported from (below), writing nothing.

**`logout`** removes the login (and revokes it with the vendor where the CLI
has a logout of its own), and **records the logout** in the CLI's share of the
state directory (`<state_dir>/<agent>/logged-out`), so the configured bundle below is not restored into the directory it emptied —
not by the engine's next boot or apply, and not by `list`, `status`, `export`
or `doctor`, which each say why instead — until a login is written into that
directory again: `login` (brokered or `-username`), `-from-host` or `import`. A
headless token (`-token-stdin`, or the one `-capture-token` mints, unless its
sign-in also left a login file) signs the entry in without ending the logout. A
fresh state directory holds no such record and restores the bundle as before,
so `logout` names the field or secret that retires it when the entry has one —
even when the CLI's own logout fails, since the login is removed and the logout
recorded either way.

The engine also restores a bundle **itself**, at boot and at every apply it
publishes (never for a revision it refuses), from
`auth.credential_bundle` or the conventional
`CREWLET_LLM_CLI_<KEY>_CREDENTIALS` variable — and only into an empty
credential directory, by the same rule, and never after a `logout` until a
login is written again. `list`, `status`, `export` and `doctor` make
the same restore before they look, so they report the login the engine would
run with; `validate` never writes one. That path authenticates a fresh
container before its first turn rather than after an operator remembers a
command; `import` is for the hosts that are already up. See [environment
variables](environment-variables.md).

A bundle **belongs to the CLI it was exported from**, and names it, though it
is keyed by the entry: one exported before the entry moved to another CLI
names the old CLI, so a restore into the new CLI's empty directory refuses it
and writes nothing — even where the two CLIs keep a login file under one name,
as six keep an `auth.json` — at boot and apply as
`cli_agent_bundle_restore_failed`, in `list`, `status`, `export` and `doctor`,
and in `import`, until the new CLI is signed in and its own bundle exported in
its place. A bundle that names no CLI is refused the same way: see [an entry
moved to another CLI](../concepts/llm-backends.md#an-entry-moved-to-another-cli).

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
crewlet mattermost doctor <company.yaml> [-browser-url URL] [-admin-token TOKEN]
                                         [-config PATH] [-api URL]
```

Checks a Mattermost install by exercising what actually breaks, in the order it breaks — and **no operator credential is required**: the seat tokens already in the config are what the engine authenticates with, so they are the honest thing to check with, and minting an admin token to find out whether a company works is a step that exists only to be skipped. Pass `-admin-token` (or export `MATTERMOST_ADMIN_TOKEN`) to run the shared checks as somebody else.

**Where people browse is an input.** The server reports where it *believes* it is browsed — its Site URL — and `integrations.mattermost.url` may be any route to it (an in-cluster Service, say), so neither tells the doctor the address people actually type. `-browser-url` does, and falls back to `$MATTERMOST_PUBLIC_URL`, the variable the compose stack sets the Site URL from. Given it, the Site URL is held to it and the browser probe is sent there; without it, the Site URL is judged on its own, the probe can stand in only for a browser at the Site URL, and the `site url` line says what was not asked.

| Flag | Description |
|------|-------------|
| `-browser-url` | The address people type into their browser to reach Mattermost. Falls back to `$MATTERMOST_PUBLIC_URL`; must be an absolute `http` or `https` address. |
| `-admin-token` | A Mattermost token to run the shared checks as. Falls back to `$MATTERMOST_ADMIN_TOKEN`, and failing that borrows a seat's own. |
| `-config` | Tier A config naming this node's store and keyring, for resolving the company's `${VAR}`s. |
| `-api URL` | The node to read the company's stored values through — a node that holds data, from a `seats` node, a member of a cluster whose engine is stopped, or a machine that is not a node. Default: this node's engine when it runs, and the fleet's store itself when it is stopped ([Commands](#commands)). |

| Check | What it catches |
|---|---|
| `GET /system/ping`, **unauthenticated** | Wrong URL, no route from here — and a `url` the server redirects, or answers with its web app's page, which names the `url` to set (the engine follows no redirect; see [The address the engine dials](../integrations/mattermost.md#the-address-the-engine-dials)). First and without a credential, because a bad token must not make a healthy server look dead — the two have completely different remedies. |
| `ServiceSettings.SiteURL` | The setting with no error message: unset, not an absolute `http(s)` address, a default port spelled out, or — given `-browser-url` — not the address people type. Mattermost accepts a **browser's** websocket only when its `Origin` matches the Site URL's scheme and host, so a wrong one silently costs every human live updates while the agents, whose sockets carry no `Origin`, keep working. The Site URL is never compared with `integrations.mattermost.url`: people browse the one and the engine dials the other. See [The Site URL](../integrations/mattermost.md#the-site-url). |
| A websocket upgrade sent **with** a browser's `Origin` | What every human's live feed does, `AllowCorsFrom` included. Given `-browser-url`, a browser there: sent to that address under the Site URL's path, so a reverse proxy that drops `Upgrade` fails it too. Without it, it carries the Site URL's own `Origin` to `url` and sees nothing in front of the server. The `Origin` is what a browser sends: scheme and host, no path, no default port. |
| The configured team | Channels are team-scoped, so a team that does not resolve is a company where no bot can be placed. |
| Per seat: its own credential, a real socket, its channels | A revoked token, a disabled bot, a `${VAR}` that never reached this deployment, or a bot in **no channel** — which hears only direct messages while its account looks perfectly healthy. |

Each seat is checked with **its own** credential, because "the server accepts sockets" and "this bot wakes" are different questions and only the second delivers a message. A seat that fails early is not asked the later ones: one whose token did not resolve is never dialled, and one whose credential is refused is never asked about its channels — reporting those as separate failures would send an operator after faults nobody observed. The same rule governs the run as a whole: an unreachable server, an unreadable server config or a missing credential **stops** the checks and says so, because one failing line with nothing after it otherwise reads as "one thing is wrong" when it means "nothing else was even asked".

Read-only. Exits non-zero when any check fails, so it drops into a deploy script — and without `-browser-url` or `$MATTERMOST_PUBLIC_URL`, a pass has not asked whether people browse the Site URL.

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
needs the current revision re-activated on a running node (`POST /config/reload`)
so the engine rebuilds its secret snapshot — an engine that starts afterwards
reads it on its own — and it needs no file, which is exactly why a report that stopped at "recorded in the encrypted secret store"
read as finished. A run that changed nothing prints no follow-up.

**A plain re-run does not rotate a working token.** GitLab returns a personal access token's value once, so a provisioner cannot verify that what it recorded last time still matches — and minting every run is an outage, because the engine is running with the *old* value: an operator adding a tenth seat would revoke the nine credentials the other agents are authenticating with. A seat is left alone when the variable holding its token still has a value **and** the account still has a usable token under this tool's name (`crewlet-<handle>`), and both halves are checked because either alone is wrong — a recorded value whose token was revoked leaves an agent 401ing for ever, and a live token nobody wrote down cannot be deployed. `-rotate` mints for every seat regardless, retiring the previous one **after** recording the new: never before, or a failed record leaves the seat with nothing.

| Flag | Description |
|------|-------------|
| `-admin-token` | Operator credential — a top-level group **Owner** PAT with `api` scope on GitLab.com, or an admin PAT self-managed. Falls back to `$GITLAB_ADMIN_TOKEN`. The seats' own tokens are what this run mints, so it cannot bootstrap itself from them. |
| `-secret-store` / `-env-file PATH` / `-print` | Where minted credentials go — exactly one, and there is no default: a run with nowhere to put what it mints creates live credentials at the third-party app and prints none of them. See [the secret store](../concepts/secret-store.md). |
| `-public-url` | This deployment's public base URL; the group webhook is registered at `<url>/webhooks/gitlab`. **Overrides the Tier A webhook address (`api.public.external_url` when the file sets a [public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api), `api.external_url` otherwise) for this run**, and registration is skipped only when both are empty — a hook pointing at the wrong host is worse than none, because the instance then reports a healthy integration. |
| `-rotate` | Mint a fresh token for every seat, including seats whose current one still works. **Restart the engine afterwards.** |
| `-decommission` | Delete service accounts whose seats have left the config. Scoped **twice**: the username must start with `provisioning.username_prefix` (never empty — it defaults to `crewlet-`) *and* the account must be a member of this company's group, because either alone is too broad. That group scan is used in **both** modes and it is deliberate: every seat is made a member of `provisioning.group` whatever created it, so a managed account of this company is always in the group — while the instance's own service-account listing also holds every *other* company's accounts on the box, which a shared prefix would then sweep. Only the DELETE route differs by mode; sending one down the wrong route answers "no such account" for one that is still live. An account the instance refuses to delete because it is not a service account is reported rather than aborting — that refusal is GitLab catching what the scan should not have proposed, so it is a signal about the prefix. |
| `-mode group\|instance` | Where service accounts are **owned**, for this run only; it defaults to `integrations.gitlab.provisioning.mode`, which is what the engine's own reconcile reads. `group` (the default, and all GitLab.com offers) creates them under `provisioning.group`; `instance` creates them on the instance itself, which needs an **instance-administrator** PAT and buys an account that is a member of nothing until this run adds it — so one company's seats can span several top-level groups, and an account survives its group being deleted. Everything downstream is identical: memberships are added the same way, and the token API is user-scoped on both. An unknown value is refused before the config is loaded. |
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
3. **The inbound webhook**, on Data Center only, at `<public-url>/webhooks/jira` — subscribed to exactly the events the parser routes, delivering the whole body, signed with an HMAC secret. On **Cloud** the step is skipped with a note rather than attempted: a dynamic webhook there belongs to an app, so the endpoint refuses an API token however privileged it is, and reporting that 403 would send an operator to rotate a credential that is fine. Cloud events arrive through the [Forge app](../integrations/jira.md#jira-cloud--the-forge-app-optional) instead.

**A working webhook secret is never replaced.** If `integrations.jira.webhook_secret` already resolves, that value is registered as-is. Minting on every run would be an outage: the engine holds the *old* secret, so the instance would start signing every delivery with a key nothing can verify. A secret that resolves to nothing is minted into the `${VAR}` the config points at and recorded in the sink you chose; `-recreate-webhook` forces a rotation, which invalidates the secret every other deployment of this company holds.

| Flag | Description |
|------|-------------|
| `-secret-store` / `-env-file PATH` / `-print` | Where a minted webhook secret goes — exactly one, and there is no default. See [the secret store](../concepts/secret-store.md). |
| `-public-url` | This deployment's public base URL; the webhook is registered at `<url>/webhooks/jira`. **Overrides the Tier A webhook address (`api.public.external_url` when the file sets a [public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api), `api.external_url` otherwise) for this run**, and registration is skipped only when both are empty — a hook pointing at the wrong host is worse than none, because the instance then reports a healthy integration that delivers into the void. |
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
| `-public-url` | This deployment's public base URL; the hooks are registered at `<url>/webhooks/github`. **Overrides the Tier A webhook address (`api.public.external_url` when the file sets a [public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api), `api.external_url` otherwise) for this run**, and registration is skipped only when both are empty — a hook pointing at the wrong host is worse than none, because GitHub then reports a healthy integration that delivers into the void. |
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

Every other third-party app's provisioning runs unattended. **Slack cannot**, and that is not an implementation gap: installing an app into a workspace is an OAuth grant, and OAuth exists precisely so that a person decides. So the run creates and updates the apps by itself, then hands the operator one authorize URL per seat and takes the code back — and where there is nobody to ask (`-no-install`, `-dry-run`), it prints the URLs and stops rather than pretending.

For each seat whose `integrations.slack` credentials are whole `${VAR}` references it builds the canonical manifest ([`internal/slack`](../integrations/slack.md#bot-scopes-and-events) is the single source of truth for the scopes and events), creates or updates the app, records the **signing secret** into the variable `signing_secret` points at, and — after the click — the **bot token** into the one `bot_token` points at. A seat whose credentials are literals is one an operator manages by hand: it is reported and skipped, never rewritten.

**An unchanged manifest is not pushed.** The manifest methods are Slack's slowest rate class, roughly one request a minute, so a company of seven re-running would otherwise spend minutes achieving nothing. The ledger holds a fingerprint of the last manifest Slack accepted, and a re-run compares against it.

**One seat's failure does not cost the others.** A mistyped code paste or a refused manifest is recorded against that handle, the remaining seats still provision, and the command exits non-zero naming what failed. Everything completed is durable — the ledger is written after every mutation — so a re-run resumes.

**The app-configuration token is persisted before it is used.** Slack's rotation is single-use in both directions: the call that returns a new refresh token invalidates the one it was given. A run that rotated and then failed to record the result would lock the operator out of their own apps, so the pair is written to the ledger first, and a still-valid access token is reused rather than rotated again. For the same reason **the ledger's pair beats `-config-token` and `$SLACK_CONFIG_REFRESH_TOKEN`**, which is the reverse of the usual rule: a value left in a shell export is dead the moment the first run used it, and preferring it would trade the only live pair for a retired one on every run after. The flag and the variable are a bootstrap for a ledger that holds nothing.

**A recorded app that no longer exists is replaced, not reported kept.** Its manifest fingerprint still matches, so without a probe the seat reads as healthy while the bot token in its `${VAR}` authenticates as nothing. Every run validates each recorded app id first — one call, no write — and reads `app_not_found` / `invalid_app_id` / `invalid_app` as gone; a permission refusal is not an absence and never triggers a replacement, because an app this credential may not touch still exists and replacing it would leave two.

**A code that belongs to another app is refused and records nothing.** One authorize URL is printed per seat and they look alike; pasting the wrong one would mint a colleague's bot token into this seat's variable, and the seat would post as them with nothing reporting it.

| Flag | Description |
|------|-------------|
| `-public-url` | Public HTTPS base URL of this deployment. **Required unless the Tier A webhook address (`api.public.external_url` when the file sets a [public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api), `api.external_url` otherwise) is set**, which it overrides for this run: every app's Events API request URL and OAuth redirect URL are built from it, so an app created without one delivers nowhere and cannot be installed. |
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
    [-config PATH] [-api URL] [-dry-run]
```

Publishes a tree of authored markdown into Confluence. **One walk, two destinations, decided by the file**: a file whose frontmatter declares a `trigger:` is a [tool skill](../concepts/tool-skills.md) and goes to `knowledge.skills_container` with the leading code block the engine parses back out; everything else is a knowledge doc, published as prose into the space its parent directory names, titled by its first `# H1`.

The routing is the FILE'S, not the directory's, because a skill is identified by what it declares — an operator who files one under `ENG/` still means a skill, and publishing it there as prose would put an instruction meant for one phase of one turn into every seat's context.

**Every target space is checked before a single page is written.** A typo in a directory name would otherwise be discovered half way through, leaving an operator to work out which pages landed. The importer never *creates* a space: that names a container the whole company then works in, and it is not this command's guess to make.

**A page that already exists is updated in place**, matched by title within its space. Confluence has no external-id field, so a page somebody renamed in the UI is orphaned and a re-import creates a second one. That is the backend's limitation, reported rather than worked around: a marker page or a label pressed into service as an *identity* would be this tool inventing a second answer to a question Confluence already answers, and the two would disagree the first time somebody moved a page.

**Nesting and labels come from frontmatter.** Frontmatter may also declare a `parent:` — the **title** of a page in the same space to nest this one under, which is the one thing a flat directory of files cannot say about a wiki that has trees in it — and `labels:`, the author's own page labels. The plan is ordered parents-first so a `parent:` naming a page **published by the same run** resolves; a cycle stops the walk naming the files, and a parent nobody publishes is a note and a page at the space root, because a doc nobody can read is worse than a doc in the wrong place. **An existing page is never re-parented** — where a page sits is something people move deliberately, and a run that dragged it back every time would be fighting them with no way to say so. Labels are attached on every run, not only on create, because the server call is idempotent and a label an author adds to a file that already publishes has to reach the page somehow; a label that will not attach is a note, not a page failure.

**Provenance is a different question, and it is recorded.** Every skill page this command writes gets the global label `crewlet-skill`. That says nothing about *which* page a file belongs to — it says only that the importer wrote it, which is a fact no field on the page carries and which only the writer can know. `-prune` is the one caller that needs it: an orphaned page is deleted only if this tool published it, because a lead who authored a skill by hand in the wiki has no local `.md` and would otherwise lose their work on the next import. A label that cannot be written is a note, not a page failure — the page is published and correct, and what is lost is the ability to prune it later.

**Page failures are isolated**: a restricted page or one 403 does not cost the other forty. The run reports what failed and exits non-zero.

| Flag | Description |
|------|-------------|
| `-space KEY` | Publish tool skills into this space instead of `knowledge.skills_container`. Empty reads `$CREWLET_TOOL_SKILLS_SPACE`, then the config field. Skill files only — a knowledge doc takes its space from its parent directory. |
| — | Frontmatter on a knowledge doc may declare `parent:` (the title of a page in the same space to nest under) and `labels:` (the author's own, lower-cased and de-duplicated because that is what Confluence stores). See below. |
| `-prune` | After publishing, delete skill pages in the skills space that carry the `crewlet-skill` label and whose key no local file publishes any more. **Three conditions, all required**: in the skills space, labelled, and parsing as a skill whose key this run's tree does not publish — the label protects a hand-authored page, the parse protects an ordinary page filed in the same space, and the key comparison is what makes a renamed skill a delete-and-create rather than a silent duplicate. The orphan set is derived by subtraction, so **a prune that cannot enumerate the space deletes nothing** and fails the run: a partial read would make the orphan set larger and delete live pages. The set is taken from the *plan*, not from the writes that landed, so a page whose update happened to 403 is never deleted as an orphan of itself. A page that declares a trigger and does not parse has an unknown key and is reported rather than deleted. |
| `-config` | Tier A config naming this node's store and keyring, for resolving the `${VAR}`s in the company's `confluence:` block. |
| `-api URL` | The node to read the company's stored values through — a node that holds data, from a `seats` node, a member of a cluster whose engine is stopped, or a machine that is not a node. Default: this node's engine when it runs, and the fleet's store itself when it is stopped ([Commands](#commands)). |
| `-dry-run` | Print the plan and write or delete nothing. |

A company that has turned tool skills off (`knowledge.skills_container: ""`) has no space for a skill file to go to: a tree containing one **stops the walk** naming both the setting and `-space`, rather than filing an instruction meant for one phase of one turn into a space every seat searches.

See [Confluence Integration](../integrations/confluence.md#publishing-local-pages-from-your-machine-cli).

---

## `crewlet confluence provision`

```
crewlet confluence provision <company.yaml> [-secret-store|-env-file PATH|-print]
                             [-public-url URL] [-recreate-webhooks] [-dry-run]
```

Registers the inbound webhooks that make Confluence push page and comment
events to this deployment, and mints the credential each one carries into the
`${VAR}` the company document already points at.

**The two deployments are registered differently, because they authenticate
differently.** On **Cloud** the run creates one hook per event at
`<public-url>/webhooks/confluence/<event>?token=…`: Cloud signs nothing and
honours no registration field for a header, so a shared token in the URL is
the whole authentication, and one hook per event is how the engine knows which
event fired at all (a Cloud payload does not say). On **Data Center** it
creates one signed hook at `<public-url>/webhooks/confluence` covering every
event, verified by HMAC over the body against
`integrations.confluence.webhook_secret`.

A re-run **converges** rather than adding: hooks are found by the
`crewlet:` name prefix this engine registers under, so one whose address has
moved is re-pointed rather than duplicated, and one an operator registered by
hand is never touched. A hook that is already correct is left exactly as it
is, and one event this instance refuses is reported without stopping the rest.

| Flag | Meaning |
|---|---|
| `-secret-store` / `-env-file PATH` / `-print` | Where a minted token or secret goes. The same three sinks every provisioning command takes; a run with none of them refuses rather than minting a credential it cannot record. |
| `-public-url URL` | This deployment's public base URL, which the hooks are registered against. **Overrides the Tier A webhook address (`api.public.external_url` when the file sets a [public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api), `api.external_url` otherwise) for this run**, and registration is skipped only when both are empty. |
| `-recreate-webhooks` | Delete and remake every hook with a fresh token or secret. **Destructive across deployments**: the previous value stops working everywhere else this company runs, so it is the recovery for a leaked or lost credential rather than part of an ordinary run. |
| `-dry-run` | Read and report; register nothing. |

See [Confluence Integration — Webhooks](../integrations/confluence.md#webhooks-confluence-pushes-to-agents).

---

## `crewlet confluence resync`

```
crewlet confluence resync <company.yaml> [-space KEY] [-config PATH] [-api URL]
```

Runs the engine's **own** [tool-skill](../concepts/tool-skills.md) walk of the
Confluence skills space against a throwaway registry and prints what admitted,
so you can see what a running node's next walk will see. The read-only
diagnostic beside the importer, and it exists because a page that fails to
admit is **invisible**: the only symptom is guidance that never appears in an
executor prompt.

```
TS holds 12 page(s): 9 skill(s), 3 ordinary page(s).
  deploy                       Cutting a release
  incident-review              Running a post-incident review
```

A page that declares a `trigger:` and does **not** parse is printed separately
and exits non-zero. That is the case worth failing on: somebody wrote a trigger
and got the rest wrong, and counting it as an ordinary page is exactly how a
skill goes missing unnoticed.

`-space` overrides `knowledge.skills_container`, for checking a space
the company document does not name yet.

**Skills only.** Knowledge docs are searched live at query time and never
loaded into a registry, so for them there is nothing to resync.

**It does not reach into a running engine, and a running engine does not need
it to.** A page edit reaches every node through the page webhook, and every
node also walks its skills space every 10 minutes (give or take a fifth, so a
fleet does not walk in lockstep), so a change the webhook path missed is
applied within 12 minutes without a restart. See
[Keeping every node current](../concepts/tool-skills.md#keeping-every-node-current).
