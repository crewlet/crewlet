# CLI Reference

Crewlet ships one command, `crewlet`, a single self-contained binary. Every
subcommand below is served by it.

---

## Commands

| Command | Description |
|---------|-------------|
| `crewlet run [config.yaml]` | Read Tier A bootstrap (positional, or `-config`; default `./crewlet.yaml`), connect to DB, run engine; falls into unconfigured state if its store boots on no revision |
| `crewlet validate [file.yaml]` | Validate a Tier A or Tier B YAML, or a repository's `.crewlet/checks.yaml`, and print a summary (`-json` for located, classified problems and warnings); with no positional it checks both tiers via `-config` and `-company` |
| `crewlet migrate [config.yaml]` | Apply pending schema migrations (Tier A file, default `./crewlet.yaml`) without starting a process (every process migrates on open, so a CI step may run `crewlet migrate` alone). `-check` reports pending work, exits non-zero, applies nothing |
| `crewlet budgets show [config]` | Each scope's day, ISO week and month on the company clock: spend, ceiling (`unlimited` where none), `STATE` (`ok`, `near`, `refusing`), when the window turns over, `LAST REFUSED`. Read from a running node (the counters are the fleet's); any token reads it. No reset: an allowance returns when the window turns over |
| `crewlet backup -dir PATH [config]` | Copy a running node's store files **and** stream estate into one verified directory on the *engine's* host, the only way to copy either (the store is locked to that process; the embedded broker binds no socket). Token takes `admin`. See [Backups & Restore](../guides/backup.md) |
| `crewlet retention status [config]` | Every `retention` command that talks to a node takes `admin`, reads included. Shows what each domain's log holds, the trim's conclusion and which of the six terms stops it, every data node's position, and this node's replacement cost. **Exits non-zero when any alarm is active**, printing each one's measurement and remedy on stderr (a cron hook) |
| `crewlet retention snapshots [config]` | Per-node snapshot inventory: what each machine holds per domain, how old and large, or why none. Use it when a join fails |
| `crewlet retention ack -stream NAME -position N -taken-at TIME` | Publish an operator backup floor, for `backup_floor: operator`, with when the copy was taken (the engine cannot see a copy that left the host) |
| `crewlet retention evict <node> -confirm <node> [-op-id ID] [-force]` | Stop a node's records applying on every log the trim counts nodes on (tracker, knowledge base, identity estate, artifacts log, artifact datastore, Ask Crewlet, chat, executor log, code log), so the trim can pass a floor an absent machine pins. Refused, writing nothing, while the node holds a live presence lease or its lease cannot be read (`-force` overrides both). Prints per log its outcome and the watermark before and after; **exits non-zero when not every log holds the record**, printing under each unfinished log what finishes it (the command with `-op-id` where a rerun can, else what to do) |
| `crewlet retention readmit <node> -confirm <node> [-op-id ID]` | The inverse, on the same logs. Refused, printing the numbers and writing nothing, while the node has not applied every record up to the one before the higher of the trim floor and any of those logs' first surviving sequence. A partial readmission is finished like an eviction |
| `crewlet retention set-capacity <stream> <bytes> -confirm <bytes>` | Change a log's byte ceiling, up or down, in a fleet-wide maintenance window costing three restarts (a Tier A ceiling is only the value the stream is created with). A raise the broker has no room for is refused **before** the window opens; one refused at the apply is reported as a refusal, not an unknown outcome |
| `crewlet retention maintenance status\|abandon\|exclude -stream NAME` | Where that window stands, who has not acknowledged, and the two gestures that act on it |
| `crewlet retention maintenance retire -node ID -confirm ID` | Record that no process will run under a node id again, so no capacity window counts it while it runs nothing and stores no row; refused while it runs (a live presence lease, or a broker the metadata group hears) |
| `crewlet retention reanchor -stream NAME -confirm <created_at> [-force] [-discard]` | Adopt a recreated stream or a broker restored from an older copy: move that log to its next generation (every position below it comparable and safely stale), resuming its applier with no restart. A recreated log is followed from its first surviving record, a restored one from its end; one continuing in a generation only an evicted peer held, from this node's own checkpoint, that generation's records void. A restored log with post-restore records this node's rows lack is refused unless `-discard` accepts they are applied on no node |
| `crewlet retention verify --restore -dir DIR` | Restore the newest artefact and open the copy. **Exits non-zero past its cadence** (a cron hook for a lapsed restore test). Talks to no node |
| `crewlet worklet purge <item> -reason TEXT -confirm <item-key> [-op-id ID]` | Destroy a task and every row it produced, on every data node. No inverse; `admin` credential only, never a seat. Its children move onto its own parent |
| `crewlet objects migrate -to s3 -target <path> [-final]` | Move the company's files from the fleet's NATS bucket to an S3-compatible bucket through a running data node: a copy pass on a live fleet, then `-final` with every node in maintenance mode, sealing the NATS bucket and moving the fleet's record. See [Moving the files to a bucket](../concepts/object-store.md#moving-the-files-to-a-bucket). Token takes `admin` |
| `crewlet objects status [config] [-json]` | Where the company's files are kept ([object store's](../concepts/object-store.md) backend, `nats` or an S3 bucket), from a running node: the node that ran the collector's last passes and what each found (objects listed and deleted, unfinished uploads abandoned); files **missing**, **damaged** and **unanswered** by the last finished audit, listed. `-json` prints the fleet view's `objects` block, every field and number as answered, text as written. Token takes `admin` |
| `crewlet gitlet status [config] [-repo NAME] [-json]` | [Gitlet's](../concepts/gitlet.md) repositories as a running node reads them: status, owner, default branch and head, refs, packs, size, last push; with `-repo`, one repository with its newest pushes (who, and what became of every ref) |
| `crewlet gitlet verify [config] [-repo NAME]… [-repair] [-json]` | Check **this node's** cache of every (or the named) repository against its rows; `-repair` fixes in place where only the copy's directory is out of place, else rebuilds. Each Gitlet node keeps its own cache, so verify each. **Exits non-zero when any copy is not clean**, after checking all (a per-node cron hook) |
| `crewlet gitlet export <name> <file.bundle> [config] [-git PATH]` | Write one repository (every ref and object) to a git bundle on this machine through your git; `git clone <file.bundle>` restores it |
| `crewlet gitlet import <source-url> <name> [config] -owner unit:KEY\|seat:HANDLE [-credential VAR] [-mirror] [-op-id ID]` | Create a project repository the node fills from another host's https URL, fetched by its home Gitlet node with the token the node holds as `VAR`, a variable `code.remote_credentials` declares for the source's host; `-mirror` keeps it in step as a pull mirror. **A person's alone**, through your token; answered once created, `importing` until the home has fetched the source |
| `crewlet gitlet import <source-url> <name> -local [config] [-git PATH] [-dry-run]` | Push another host's repository (GitHub, GitLab, any URL your git clones) into an existing one, as you, in steps each within `code.limits.max_push_bytes`; your git clones and pushes, the engine dials nothing. Refuses up front what it sees in the source (a blob past the blob limit, a history already past the repository limit as one pack), stops before a step that would pass that limit, skips a forge's pull refs, and **resumes** a failed run when rerun |
| `crewlet gitlet purge-unreachable <name> [config] [-op-id ID]` | Ask a repository's maintainer to drop every object no branch or tag reaches (a force-pushed secret, a deleted branch's leftovers), with no grace. **A person's alone**, through your token; answered once the request is recorded, with the serving node and what still holds the objects |
| `crewlet gitlet credential [-token-env NAME] get` | git's credential helper for Gitlet: answers with the API token in `$CREWLET_API_TOKEN` as password, over HTTPS (or plain http to this machine) only, storing nothing |
| `crewlet fleet broker list [config] [-json]` | Each live node's broker kind (`member`, `leaf`, `client`, or `unknown` for a kind this build does not know) beside how the JetStream metadata group counts it (read through a member), and every disagreement in words: a member gone for good that the group still counts in every election, with the command that removes it. Token takes `admin` |
| `crewlet fleet broker remove <node> -confirm <node> [-force]`, or `-peer <peer> -confirm <peer>` | Stop the metadata group counting a member gone for good, through a live member's system account, by node id or by the peer id `list` shows for a voter no member can name. Refused while the node holds a live presence lease as a member; `-force` is for a member wedged yet still renewing it. Token takes `admin` |
| `crewlet fleet broker credential [config] -o FILE` | Write the operator key a clustered member's broker admits on its loopback client listener, derived from this host's `secrets.keys`, to a `0600` file (never the terminal), and print the `nats` invocation that reaches the broker with it, from this host only. Token takes `admin` |
| `crewlet feeds dead-letters [config] [-json]` | The wake feeds' dead letters (records no node could read in the hour each was handed back), from a running node: per entry, by the id `discard` names, its feed, why no node read it, where its record was delivered from, how long ago kept and how long left. A feed the node could not list is named with its error and **exits non-zero**. Token takes `admin` |
| `crewlet feeds discard <id> -confirm <id>` | Give up one dead letter's wake for good (an entry no build will read, such as a record sealed under a key retired from every node); no node will derive it. Token takes `admin` |
| `crewlet doctor [config] [-json] [-words]` | The [Advisor](../guides/advisor.md)'s open findings from a running node: per class its severity, who acts, class, every subject as a kind and ordinal (`seat#1`), and evidence as counts. Subjects' handles and each finding's title and reason (the company's own names) only with `-words`. **Exits 2** while a finding blocks the company, 1 when it could not ask, the latest run could not read a source, or the Advisor has not run. Token takes `admin` |
| `crewlet seats pause <handle> [-stop] [-reason TEXT] [-op-id ID]` | Pause an agent seat: no new turn, mail waits in order, scheduled runs skipped. `-stop` also ends running work (the turn at its next round, its executor in its box at once). Taken by the seat's holder, whoever leads it, or an administrator; recorded under the caller's name, kind and credential |
| `crewlet seats resume <handle> [-op-id ID]` | Lift the pause; what waited is delivered first, in order. Same authority as `pause` |
| `crewlet artifacts list\|get\|versions\|assets\|usage` | Read artifacts through a running node as the token's principal: the listing it may see (`-scope`, `-kind`, `-q`, `-status`, `-work-item`), one artifact's head (runtime contract and capabilities included) and audience, versions with contract and capabilities, assets, and company usage against quota (an administrator's). See [`crewlet artifacts`](#crewlet-artifacts) |
| `crewlet artifacts publish <dir\|file> [-artifact ID] [-op-id ID]` | Publish a directory or file as a new artifact or the next version: an upload intent, then only files the artifact lacks, then the version against the head it read |
| `crewlet artifacts update\|restore\|label\|share\|unshare\|trash\|untrash\|purge` | Change title, description or icon; roll back to a version; label; share with a seat, a unit or the company; withdraw a share; trash and untrash; purge for good (an administrator's, with a person present; the id is asked twice) |
| `crewlet chatlet channels [-archived]` | Rooms of the company's own chat your seat is in, most recently active first, each with the id every other chat verb takes and your unread. Every `crewlet chatlet` verb acts as the seat `CREWLET_API_TOKEN` acts as; see [`crewlet chatlet`](#crewlet-chatlet) |
| `crewlet chatlet read <room-id> [-thread MESSAGE-ID] [-limit N] [-before CURSOR \| -after CURSOR]` | A room's transcript, oldest first, each message with its id and thread reply count; with `-thread`, that thread's replies |
| `crewlet chatlet post <room> -body TEXT [-link URL]... [-op-id ID]` | Post in a room (id or name) as your seat, via the `post_message` the dashboard composer calls |
| `crewlet chatlet search <text> [-channel ROOM-ID] [-author HANDLE] [-limit N]` | Rank every room your seat may read, each hit with its message id |
| `crewlet chatlet erase <room-id> -message ID... -reason TEXT -confirm N [-op-id ID]` | Destroy the named messages on every data node (e.g. a pasted credential). No inverse: `admin`, a recent proof, never a seat |
| `crewlet chatlet prune <room-id> -cutoff TIME -confirm TIME [-op-id ID]` | Destroy everything a room said before an instant, on every data node, now rather than at its [retention](../concepts/chatlet.md#retention) horizon. Same authority as `erase` |
| `crewlet chatlet retention <room-id> -days N \| -company -confirm N\|company [-op-id ID]` | Set how long a room keeps messages (`0` for ever) or return it to the company's `chat.chatlet.message_retention_days`; `-confirm` repeats the horizon. A shorter horizon is a delayed prune: same authority as `erase` |
| `crewlet schema [company\|bootstrap\|issue-report-catalogue]` | Print the JSON Schema for a config tier, or the [issue report catalogue](../guides/engine-issue-reports.md#what-is-reported) (editor autocomplete, CI, [AI-assisted authoring](../getting-started/ai-authoring.md)) |
| `crewlet config import <company.yaml>` | Load a company file as a new `company_config` revision: through a running node's `PUT /config` (activates fleet-wide), or offline as what this node boots on (published at its next start only while the fleet still runs the revision the import was built on). The printed line says which |
| `crewlet config export [--revision <UUID>]` | Dump the company this node knows (or the given revision) as YAML to stdout |
| `crewlet iam <command>` | People, seats, invitations, permissions, sessions and tokens, through a running node. See [`crewlet iam`](#crewlet-iam) |
| `crewlet config show` | The company this node knows, as redacted YAML |
| `crewlet config revisions [--limit N]` | Recent revisions, newest first, each with its `STATE`: what this node boots on, the fleet's revision as last seen, an offline change not yet published |
| `crewlet config diff <UUID> [-against <UUID\|active>]` | Structural diff of two revisions (paths and values), always redacted |
| `crewlet config activate <UUID>` | Make a stored revision what this node boots on, published to the fleet at its next start only while the fleet still runs the revision this node knew. Activating the fleet's own revision mints a new epoch, which is how a rotated secret takes effect. A running fleet: `POST /config/revisions/<UUID>/revert` or `POST /config/reload` |
| `crewlet config rekey [-dry-run]` | Re-encrypt the config document this node boots on under the active key (master-key rotation) |
| `crewlet secrets keygen [-key-id ID]` | Generate a fresh encryption-keyring key and the `crewlet.yaml` snippet to install it |
| `crewlet secrets set <NAME>...` | Store one encrypted secret, or several as one write, in the [secret store](../concepts/secret-store.md); the engine resolves `${NAME}` from it ahead of the environment and a running node re-activates the revision that reads it. `admin` through a node |
| `crewlet secrets list` | List stored secret names and metadata (never values). `admin` through a node |
| `crewlet secrets unset <NAME>` | Remove a stored secret. `admin` through a node |
| `crewlet secrets get <NAME> -reveal` | Print one stored value to stdout: break-glass, audited, CLI-only. `admin` through a node and a person present; a machine token is refused whatever it carries, so the token read is a Tier A one |
| `crewlet secrets rekey [-dry-run]` | Re-encrypt stored secrets under the active keyring key. `admin` through a node |
| `crewlet search eval [-store PATH]` | Measure the two-stage semantic search against the exact scan on the vectors a store file holds. Ground truth is the exact scan's own top-K, so no judgement is authored; exits non-zero below the floor for that corpus size |
| `crewlet assistant eval -suite DIR` | Run a directory of evaluation cases through Ask Crewlet's orchestrator and catalogue in process: each case's scripted rounds by default (deterministic, no model called), or a candidate `providers.llm` entry with `-model KEY` to measure a model before switching to it. Exits non-zero when any case fails |
| `crewlet llm list` | Every `providers.llm` entry the company declares: harness, model, how signed in |
| `crewlet llm doctor [KEY]` | Verify `providers.llm` entries end to end: CLI installed, something signs it in, a real run calls a tool over the seat's tool route, the CLI's own shell is refused and its web tool reaches the network (`-no-smoke` stops before every real completion); for an entry a seat's executor launches on, also whether its CLI has an in-box runner and which tool bridge its box dials. Exits non-zero when any entry has a problem |
| `crewlet llm login <KEY>` | Establish the vendor's own login: brokered interactively, `-from-host` to adopt one this machine has, `-capture-token` to mint a headless token into the [secret store](../concepts/secret-store.md) (`-print-token` sends it to stdout and stores nothing), `-token-stdin` for one you hold |
| `crewlet llm status <KEY>` | Ask the CLI whether it holds a login and as whom: one line as the engine reads it (`signed in`, `signed out` or `unknown`, with method and account where printed), then the CLI's own words; exits non-zero unless signed in |
| `crewlet llm logout <KEY>` | Revoke locally and delete the provider's login: credential files, or the login document a CLI like OpenCode 2 exports |
| `crewlet llm export <KEY> [-secret-store]` | Pack the login into one portable blob: stdout, or the secret store under the name the engine restores from on a fresh host |
| `crewlet llm import <KEY>` | Restore a bundle from **stdin** onto this host; refuses to overwrite an existing login |
| `crewlet confluence import <company.yaml> <directory>` | Publish authored markdown into [Confluence](../integrations/confluence.md) spaces (one per directory), plus the tool skills the files declare. Every target space is checked before a page is written |
| `crewlet confluence provision <company.yaml>` | Register the inbound [Confluence](../integrations/confluence.md) webhooks and mint each one's credential: Cloud one token-bearing hook per event, Data Center one signed hook for all. A rerun converges |
| `crewlet confluence resync <company.yaml>` | Re-run the engine's tool-skill walk of the Confluence skills space against a throwaway registry and print what loads: read-only diagnostic, changes no running engine |
| `crewlet jira provision <company.yaml>` | Report a [Jira](../integrations/jira.md) instance against the config: which account each seat's credential authenticates as, whether every org-chart project exists and agrees about its lead, and on Data Center register the inbound webhook with a minted secret. Jira issues no credentials for a provisioner, so it reports far more than it changes |
| `crewlet gitlab provision <company.yaml>` | Reconcile GitLab's company-level half: group check, the provisioning token's expiry warning, the webhook with its signing secret. Each agent's account, role and token are per-agent access, applied by the engine. `-dry-run` reports without touching anything. |
| `crewlet github provision <company.yaml>` | Report a [GitHub](../integrations/github.md) deployment against the config (which account each seat's credential authenticates as) and register the inbound webhooks with a minted secret: one on the organization where the credential may, else one per named repository. GitHub issues no credentials for a provisioner, so it reports more than it changes |
| `crewlet mattermost doctor <company.yaml>` | Check a [Mattermost](../integrations/mattermost.md) install end to end: reachability, the Site URL every browser inherits (against the address people browse, given `-browser-url`), a browser-shaped websocket upgrade, one real authenticated socket per agent seat |
| `crewlet mcp-relay <socket>` | Not for people: the MCP server a seat's harness starts, relaying the harness's stdio to the engine's tool socket and back; see [`crewlet mcp-relay`](#crewlet-mcp-relay) |
| `crewlet version` | Show the installed version (`--version` and `-v` too) |
| `crewlet help` | Show the usage message (`--help` and `-h` too) |

---

> **Every command except `crewlet run` and `crewlet mcp-relay` logs at `warn`**: they open a store, whose
> migration and open lines are noise on a one-shot command whose stdout is piped or diffed.
> Export `CREWLET_LOG_LEVEL=debug` (or `info` / `error`) to turn it up and
> `CREWLET_LOG_FORMAT=json` for a collector. `CREWLET_LOG_FILE` appends what the
> command *logs* to a file beside stderr (never what it *prints*). They are
> environment variables because they belong to the *invocation*. An unrecognised
> value resolves to the default (`warn`, `console`), so a bad level never blocks a
> migration; a log file that cannot be opened fails the command. `crewlet run` has its own
> `-log-level` / `-log-format` / `-log-file` / `-debug` flags, defaults to `info`,
> reads the `logging:` block from its Tier A file, and ignores all three variables.
> Colour is `CREWLET_LOG_COLOR` / `NO_COLOR` everywhere; see
> [Environment Variables](environment-variables.md#logging).

> **A command that talks to a running node prints a refusal as the node
> answered it**: its [code](api-endpoints.md#every-refusal-is-one-envelope),
> what it says, and what to do (its `hint`). The text is the node's `detail`
> where it worded one, else the code's `message`, never both. A
> `401`, and a `403 unauthorized` naming the rule that refused, concern the
> token, not the request, and say why the node didn't
> take it, or what it lacks and what to use instead.

### Where a command reads the company's values

**Every command that reads the Tier B company document** (`crewlet llm`, every
`crewlet <vendor> provision`, `mattermost doctor`, `confluence import` and
`confluence resync`) resolves `${VAR}` as the engine does:
**the secret store first, the process environment behind it**. The environment
alone would give an empty string for every value rotated into the store, and for
`integrations.gitlab.signing_secret` empty means *mint*, so a rerun would replace a
working webhook secret at the third-party app. Each takes `-config PATH` (default
`./crewlet.yaml`), the Tier A file naming this node's store and keyring, and
`-api URL`, a node to read through instead, as [`crewlet secrets`](#crewlet-secrets) does.

A run decides **once** how it reaches the fleet's one store, and everything goes that
way: values read, what a `-secret-store` sink records, seat tokens read back, and what
`crewlet llm login` and `crewlet llm export -secret-store` store. Its first line says which.

| Where the run is | What it reads |
|---|---|
| **This node's engine is running** (it holds its database's lock, from the read its start decides on, so a node still starting answers `503 starting`), or **`-api URL`** names a node | The fleet's store through that node's `/secrets` API: the node `-api` names, else the `api.host:port` in `-config`. The bearer is `CREWLET_API_TOKEN` only, one of Tier A's `api.auth.tokens`: every value read is a reveal, taking `admin` and a person present, so a machine token is refused. A run through `-api` needs and opens no Tier A file, so it works from a non-node machine. Where this node's engine holds its database and the run cannot use its API (no `CREWLET_API_TOKEN`, or an `api.port` of 0), it is refused with both facts, naming `-api URL` with a node whose `api.auth.admin` is `serve`; only `crewlet secrets` also offers stopping the engine, since taking a node down to read a value is an outage to save a token |
| **This node's engine is stopped**, on a solo node (an embedded broker naming no `stream.cluster`) or a client of an external broker (`stream.type: nats`) | The fleet's store itself. The command checks the database lock without claiming it, starts this node's own broker on `stream.store_dir` with no listener and stops it when done (on `stream.type: nats`, it dials the external broker with Tier A's credentials). One invocation starts one broker for all its reads and writes; while it holds the store directory an engine started there is refused, naming the command's pid. On a never-booted node it creates the fleet's buckets as the engine's first boot would, and the engine adopts them. No node logs these reads: this node's Tier A (keyring, broker files or credentials) opens every value without an API. The first line names the broker, so a run from the wrong directory shows |
| **This node's engine is stopped, on a member of a cluster** (`stream.cluster`) | **Refused** before the command reads, plans, prints or changes anything, naming `-api URL` with a running member's Tier A token in `CREWLET_API_TOKEN`, or starting this node's engine and rerunning: alone its broker never reaches the quorum the store needs, and beside running peers it would rejoin the cluster as this node for one command |
| **Another process holds `stream.store_dir`**: another `crewlet` command reaching the store, or an engine part way through a start with `-import-company` (any other start holds the database from the read it decides on, so the run goes through its API) | **Refused**, naming the holder (`pid N (PROGRAM) on HOST since T`): rerun once it has finished or the engine is up. Two brokers on one store directory would corrupt it |
| **No Tier A file at `-config`, and no `-api`** | The environment alone, said on the first line: how these commands run on a non-node machine |

A command that reads no stored value decides no route, runs anywhere (a stopped
cluster member included) and starts no broker: `confluence import` with `-dry-run` or nothing to do.

**A run reads its command's values and nothing else.**
Through a node it lists the node's names (`GET /secrets`, revealing nothing), then
asks `POST /secrets/reveal` for the variables the command reads that the node lists,
at most 64 per request: one reveal or a few, never one per name, and a name the node
lacks costs no request. A listed name gone when asked falls to the environment as an
unheld one does, and that is **this shell's** environment, not the engine's.
A name no store can hold (an `overrides.token_env` outside the reference
grammar) is never sent. The node logs every name asked against the caller, and
the run's first line names the node and the count: `resolving ${VAR} through the running node at <url>: N of
the M stored values this command reads revealed there in K requests, each
logged by name`. With the engine stopped it takes the fleet's listing the
same way, then reads the listed names in one store read, opening
no row it was not asked for; its first line: `resolving
${VAR} from the fleet's store, reached through this node's broker on
<stream.store_dir> (this node's engine is stopped): N of the stored values this
command reads held there` (`the external broker at <host:port>` on
`stream.type: nats`). What each command reads:

- **`crewlet llm`** the entries it works on: `login`, `status`, `logout`,
  `export`, `import` and `doctor KEY` the one named only, whatever the
  other keys look like; `list` and a bare `doctor` every one.
- **`gitlab provision`** `integrations.gitlab.url` and `.signing_secret`, and
  with `-secret-store` each planned seat's token variable, which the sink reads
  back to decide whether to keep the seat's token (`-rotate` and
  `-dry-run` read none back).
- **`mattermost doctor`** `integrations.mattermost.url`, `.team` and each
  agent seat's `mattermost.bot_token` that is one whole `${VAR}`.
- **`github provision`** `integrations.github.url`, `.token` and
  `.webhook_secret`, and the token spellings its identity scan tries in each
  agent seat's `mcp_env.github` (its own, or its unit's).
- **`jira provision`** `integrations.jira`'s address, account and
  `webhook_secret`, and the Jira token spellings and address keys in each
  agent seat's `mcp_env.jira` and `mcp_env.atlassian`, never a
  Confluence-only credential.
- **`confluence import`** and **`resync`** `integrations.confluence`'s address
  and account, and **`confluence provision`** those, `.webhook_secret` and
  `.webhook_token`.

A signing or webhook secret's whole `${VAR}`, which a provisioner's sink reads
back before minting, is among those names. They are read **before
the command does anything**, so a refused reveal or a value the keyring cannot
open stops it there (a stored value read as unset would make a provisioning
run mint over a working secret). A held name the command resolves beyond them is
read as it comes up, and the run says so (through a node, one name per
request, revealed and logged by name); a seat token its sink reads
back beyond them likewise; a failed read stops the run. Nothing is filled from the environment in place of a value the store
could not be asked for. The error names the token only when the node
refused it (401 or 403): a node that is down answers the same for any token,
and a draining one answers 503; name another with `-api`.

A Tier A file that exists and does not load (one declaring no `secrets.keys`
among them) is refused, not sent to the environment, so a configured store
never silently resolves from a stale export. A node that holds no data (`store.scratch`) has no store of the
company's, so a command there without `-api` is refused, naming the flag
for a node that does. The one exception is the operator's own
credential (`-admin-token` / `$GITLAB_ADMIN_TOKEN` and its siblings), read
from the environment only; see [the secret
store](../concepts/secret-store.md#what-still-has-to-be-in-the-environment).

#### A sink the store outranks

`-env-file` and `-print` hand you what a provisioning run mints, for the
engine to read from its environment, but the engine resolves every `${VAR}`
from the secret store **first**. Where the store holds a name the
run mints into, the new value is never read: the third-party app holds it and
the engine keeps the stored one. A seat's token locks the seat
out (GitLab retires earlier tokens once a new one is recorded); a
webhook secret re-keys a hook with a key the engine lacks, so
every delivery is refused: `gitlab provision -rotate -env-file`, the remedy for
a leaked key, would leave the engine verifying with the leaked one.

So a real run with `-env-file` or `-print` that has a store to ask (a
running node, the node `-api` names, or the fleet's store reached directly
with this node's engine stopped) is
**refused before it reaches the third-party app** where the store holds any
name it can mint into. The refusal names the store, the names and the sink, and
the two ways forward: record with `-secret-store`, which reads each stored value
back and writes into that store, or remove the stored value first (`crewlet
secrets unset NAME`). The store is asked by its listing alone, revealing
nothing.

| Command | The names it can mint into |
|---|---|
| `gitlab provision` | Every planned seat's token variable, and `integrations.gitlab.signing_secret`'s `${VAR}` where the run mints the secret or, with `-rotate`, rotates it |
| `github provision`, `jira provision` | `webhook_secret`'s `${VAR}`, where the run registers a hook (it has a webhook address to register at, and for GitHub an `integrations.github.provisioning` block naming where) and the secret resolves to nothing, or `-recreate-webhooks` (GitHub) or `-recreate-webhook` (Jira) asks for a fresh one |
| `confluence provision` | `webhook_secret`'s and `webhook_token`'s `${VAR}`s, on the same terms with `-recreate-webhooks`; which of the two the instance's hooks use is the instance's answer, not yet asked |

Every planned seat counts, not only those the sink holds nothing for: whether a
seat is kept or re-minted is settled against the third-party app mid-run,
and an env file's value for a name the store also holds
is not what the engine runs on, so keeping it because it still authenticates
would report a seat healthy on a token the engine does not use. A webhook secret
that resolves is registered as is, so it counts only where the run would mint
one. A run with no store (no Tier A file, no `-api`) has nothing to be
outranked by, and a `-dry-run` mints nothing, so neither asks. The question is a
store read like any other, so a run that cannot reach the store (a stopped
cluster member, a no-data node without `-api`) is refused as every reader is.
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
so a Tier B file on this command line only gets a document *into* the store
(what `-company` and `-import-company` are for). The engine never starts on the
file itself. It starts on the revision the node's store **boots on** (the last
one its engine applied, at the instant of the activation it applied, or one an
offline [`crewlet config`](#crewlet-config) command chose), or with no company
where the file is to be imported; the import and first reconcile bring the
file's company in before any seat is claimed.

The file is read against the rules a running company depends on. The
[admission rules](../concepts/configuration.md#what-a-stored-revision-is-held-to)
apply only when it is written as a new revision (`-company` where no company
exists yet, `-import-company` over a different company); it is then also built
as the node's apply would build it before anything is stored, so a file the
node cannot build refuses the boot with nothing written. A file that is already
the revision this node boots on, or a bootstrap an existing company outranks,
starts the node even if it carries a duplicate unit key stored before the rule
existed.

A file that is a **redacted read** (`crewlet config show`, `GET /config`, the
dashboard's Copy as YAML, each masking an inline credential as `__redacted__`)
has every mask restored from [the company this node
knows](#what-a-nodes-store-knows) before it is judged, by the identity of what
holds it (a seat by its handle, a unit by its key), as
[`crewlet config import`](#crewlet-config-import) restores one. A mask nothing
there answers, or a store that knows no company, refuses the start naming the
field.

To change a **running** fleet with no restart, use
[`crewlet config import`](#crewlet-config-import), which goes through the
node's API.

The path comes from the **positional argument** or from `-config`, defaulting
to `./crewlet.yaml`. Naming it both ways is refused (nothing checks they
agree). A leftover positional is refused too: Go's flag parser stops at the
first non-flag token, so accepting it would silently boot from the default.

If the default is missing and a `config.yaml` sits beside it, the error says
so (older guides and `examples/nimbus.config.yaml` used that name). It is a
**hint, not a fallback**: silently loading a file nobody asked for boots a node
from the wrong document on a machine that has both. Tier B is read from the `company_config` table in the store. If it names no revision this node boots on, the engine boots **unconfigured** with the API still serving, so an operator can bootstrap via `crewlet config import` or `PUT /config`. See [Configuration concept doc](../concepts/configuration.md).

**What a start publishes.** Before its first reconcile, a node that imports no
file compares what it boots on, and the fleet's revision as it last saw it,
with the fleet's pointer, and publishes at most one revision. Every publish is
a compare-and-set against the activation it read (create-only where there is
none), deciding again if another write moves the pointer first. Each says
whether it **restores** a company the fleet lost or **chooses** one; only a
restore is ever replaced by a peer:

- **An offline change** (what `crewlet config import`, `activate` or `rekey`
  chose with the engine stopped) is published only over the revision it was
  built on, or on an offline change an earlier start published and could not
  record: while the pointer still names that revision (and is not another
  node's restore of one this node saw the fleet move past), or, with no
  pointer, while the fleet's revision this node last saw is that one, or it saw
  none.
  An offline change the fleet has moved on from is **withheld**, never
  published over the fleet: the node boots on it, logs
  `offline_revision_not_published` at warn on every start (naming the
  revision, what it was built on and what the fleet runs), and runs it until it
  applies the fleet's revision.
- **A lost pointer** (a coordination store rebuilt under a running fleet) is
  restored, create-only, from what this node knows: the fleet's revision as it
  last saw it, even one this build refused, rather than the older one it runs;
  where it never saw one, the revision it boots on.
- **A pointer restored behind the fleet** (another node's restore of a
  revision this node holds, activated before the fleet's revision as this node
  last saw it) is moved on to that revision, expecting that very restore. A
  pointer somebody *chose* is never moved, however old its instant reads:
  instants come from writers' clocks.
- **Anything else** publishes nothing: the pointer is the company, and the
  node's reconciler converges on it.

A node whose engine refuses the fleet's revision keeps running the one it
served. At its next start it boots on what its boot row names, which is that
one, except while a write to the row is being retried (a running fleet shows
this as [`GET /fleet`](api-endpoints.md#get-fleet)'s `config_boot_revision_id`
differing from `config_serving_revision_id`), and on a node whose store is
discarded at every start (`store.scratch`) there is nothing to boot on. The
refused revision is recorded as the fleet's, never as what the node boots on,
and the one it runs is never published over the fleet. The whole decision is
[What a booting node
publishes](../concepts/control-plane.md#what-a-booting-node-publishes).

| Flag | Description |
|------|-------------|
| `-config PATH` | Tier A: this node's broker, store and API (default `./crewlet.yaml`) |
| `-company PATH` | Tier B **bootstrap seed** (default `./company.yaml`): imported only when no company exists yet (none this node boots on, none it has seen the fleet run, none the fleet runs, the last asked create-only once the node's broker is up), so a fresh node joining a fleet, or a node without `data`, never replaces the fleet's company. Once one exists the file is **ignored**, loudly (`company_seed_ignored` at warn, naming the revision that outranks it; silent when the file is that revision), so a stale file never reverts a live change. The hint names `crewlet config import <file>` first (offline, the next start publishes it only while the fleet still runs the revision it is built on) and `-import-company <file>` second, the override that replaces whatever the fleet runs. Absent at its default is fine: the node boots on the revision its store boots on, or unconfigured. An ignored file is never run or built. An imported file is first built as its apply would build it, so one it cannot build (a `${VAR}` resolving to nothing where a backend refuses an empty value, say) refuses the boot with nothing stored and the fleet not pointed at it. A revision the seed writes is the node's own: `created_by` is the node's id and `created_by_kind` is `system`, since no credential made the write. Where Tier A names [`api.auth.company_writers`](../concepts/configuration.md#managed-configuration) the seed is not read at all (never imported, never run, empty store or not), so a stray file neither becomes the company nor fails the boot when it no longer parses; the node says so the same way, and the managing system writes the company. |
| `-import-company PATH` | Tier B to make the company **now**, over whatever the fleet is running: the one activation this build makes unconditionally, the deliberate "this file is the company again" gesture. The node starts with no company and imports the file before it claims a seat, without opening the revision its store boots on, so a revision this build cannot run does not refuse the boot that replaces it. The file is built as the node's apply would build it before anything is written, so one the node cannot build refuses the boot and leaves the fleet on its company. The file is compared with the revision the **fleet** runs, so a stale local copy still replaces what a peer changed since; a file that is the fleet's revision imports nothing. With no pointer it is compared with [the company this node knows](#what-a-nodes-store-knows), and a file that is that revision is published as it is, as a choice made now at this node's clock, so a peer remembering a newer revision does not take it for a stale restore. A revision it cannot open to compare (the fleet's, or with no pointer the one this node knows, stored unsealed or under a key the keyring no longer holds) is replaced all the same, said at warn as `company_import_uncompared`, since that is the state the flag exists to repair. The new revision is built on the one it replaces. Mutually exclusive with `-company` (both is refused: opposite asks). **Refused** before the node starts when Tier A names [`api.auth.company_writers`](../concepts/configuration.md#managed-configuration): the file presents no credential, and the managing system would replace it at its next reconcile. |
| `-log-level LEVEL` | `debug`, `info` (default), `warn` or `error`. Overrides `logging.level` in Tier A, only when given. A typo resolves to `info`: a bad log level must never be why a company will not boot. |
| `-log-format FORMAT` | `console` (default), `text` or `json`. Overrides `logging.format` in Tier A, only when given. `console` is columns and colour for a person; `text` is slog's `key=value`; `json` is one object per line for a shipper. A typo resolves to `console`. |
| `-log-file PATH` | Also write the log to this file, overriding `logging.file.path` in Tier A, only when given. It is a **second** destination: stderr keeps every line. An explicit `-log-file ""` writes no file for one run, unless Tier A also set `logging.stderr: false` (the node would log nowhere), which is refused by name. It moves the path only; the file's shape, level and rotation caps stay Tier A's. A path that cannot be opened **fails the command** rather than defaulting. |
| `-debug` | Shorthand for `-log-level debug`; wins if both are given. It only *raises*: to quieten a file that sets `logging.level: debug`, pass `-log-level info`. It makes the **engine** verbose, not the embedded broker: nats-server's debug output is `stream.debug` in Tier A, off by default (it is per internal-client and the engine's coordination reads produce a constant stream). The broker's warnings and errors reach the log at either setting. |
| `-api-host HOST` | Bind address, overriding `api.host` |
| `-api-port PORT` | Bind port, overriding `api.port`. With the `ingress` role it carries the whole HTTP surface (less the routes `api.public.port` moves to its own listener); without it, only the [probes](api-endpoints.md#probes-on-a-node-without-ingress) `/health` and `/ready`, since an executor run's tool calls reach a seat on such a node through an `ingress` node's bridge. `0` serves **no HTTP at all** (no dashboard, REST, webhook endpoint or probe), so every integration goes deaf and no orchestrator can probe the node; leaving the flag off is not the same as `0`. Any other port is bound as the node starts, before it opens its store, so a port somebody holds fails the run at once; until the node serves, it answers what a [starting node](api-endpoints.md#while-a-node-is-starting) answers. Either flag is refused when it contradicts the file's other listeners: `0` beneath an `api.public.port`, or a port another listener binds on the same address. Also refused: a port on an ingress node whose file has no `api:` block (a node serving the API must state `api.external_url`, `api.auth.admin` and a token). |
| `-mode MODE` | `maintenance` or `seal`: boot for a [capacity window](../guides/retention.md#changing-a-logs-ceiling) rather than for service. Both start the broker and **no publisher**: no seats, duties, schedulers, writer on any surface, or barrier (a `linearizable` read is refused `maintenance`), no object collection or audit (each pins the estate with a barrier), and no eviction, readmission or reanchor. The node keeps its presence lease. `maintenance` may write stream configuration and `seal` may not, which is what makes a `seal`-mode acknowledgement evidence. Leave it off for a node in service; a node in either mode refuses to run a company. |
| `-roles ROLE[,ROLE...]` | What this node runs, overriding `node.roles`: `data` (hold the company's durable state), `gitlet` (serve the company's git repositories where it sets `code.backend: gitlet`; needs `ingress`), `ingress` (serve the HTTP API and its webhooks), `seats` (claim seat leases and run agents), `workers` (the company-wide singleton duties). Default: all five, one process running a whole company. An unknown name is **rejected rather than dropped** (a typo would give a node that runs nothing and reports itself healthy). The flag applies after the file validates, so what the roles require of the file is checked again: dropping `data` from a node whose store is not `store.scratch` is refused, naming the setting. Roles decide what a node serves and which duties it holds; `data` alone decides which state logs it applies: every data node applies every state log (identity directory, chat and code log included), and a node without `data` applies none and reaches the estate through one that does; see [what a satellite holds](../guides/satellite-nodes.md#what-a-satellite-holds). See [Running a Fleet](../guides/fleet.md#nodes-that-hold-no-data). |
| `-dev-principal LOGIN` | **Development only.** Resolve every request that presents no credential to `dev:LOGIN`, carrying every app, and `admin` only where `api.auth.admin` is `serve`. It is what `api.auth.disabled` should have been, and it is **refused** unless *both* hold: `api.host` binds a loopback address, and the binary is a development build. See below. |

The logging flags override the Tier A `logging:` block **only when actually
given**: a flag carries its default whether or not anyone typed it, and
`-log-file`'s default (the empty string) is also how an operator says "no file
for this run". Every flag that overrides Tier A is **folded into the document**
the node runs, and verbosity, the log file and the [advisories the start
logs](#every-start-says-what-its-tier-a-is-worth-knowing) all read that one
document. The run holds it to every Tier A rule once the flags are in, and a
refusal names the flags given (the file passed as it loaded, but a flag reaches
every rule that reads what it moved); the engine asks again before it opens
anything.

`-roles`, `-api-host`, `-api-port` and `-log-file` are the four overrides: the
fields whose right value depends on *where the process is running* (the node's
job, its HTTP bind, its log path on this host). Everything else in Tier A
belongs in the file, where it can be reviewed, including the log file's own
shape and rotation caps.

**How it exits says why it stopped**, which is what a supervisor's restart policy reads:

| Status | Why |
|---|---|
| `0` | A signal (`SIGINT`, `SIGTERM`) stopped it, after the drain and the teardown |
| `1` | It could not start: a refused config, a store or broker it could not open (a database or `stream.store_dir` another process holds, named by its pid: another engine, or a `crewlet` command run while this node was stopped, such as an offline `config` or `migrate` or one reaching the fleet's store, which holds them only until it finishes), or a broker connection closed for good while it booted, whose last stderr line names the cause and what to change as the next case's does. **Or it stopped itself** because a broker connection it cannot run without was [closed for good](../concepts/seat-ownership.md#the-node-whose-broker-is-gone-and-why-it-leaves-too): the last stderr line, `crewlet: the node stopped itself: …`, names the cause and what to change, after the drain and teardown in a signal's order; the drain cancels running turns rather than waiting for them when the lost connection was the stream's own, since none can be acknowledged |
| `69` | This node's [embedded broker declared itself dead](../concepts/seat-ownership.md#a-broker-that-declares-itself-dead) after the boot; the watchdog's exit leaves with no drain, after an `embedded_broker_died` log line and with the broker's reason on stderr. A broker declaring itself dead *during* the boot fails the start with status `1` |
| `75` | The [watchdog](../concepts/seat-ownership.md#the-wedged-node-and-why-it-leaves) ended a wedged process, with no shutdown at all |

Every non-zero status is a node that wants restarting, so run it under something that restarts a process that exits non-zero (`Restart=on-failure`, a container restart policy).

### Every start says what its Tier A is worth knowing

Every warning [`crewlet validate`](#crewlet-validate) prints for a Tier A
document, `crewlet run` logs too: once per start, at `WARN`, one
`config_advisory` line each, in the order `validate` lists them.

```json
{"level":"WARN","msg":"config_advisory","component":"cli","config":"bootstrap","path":"retention.backup_owner","detail":"nobody is named as this deployment's backup owner. The trim stops when the newest backup ages past `stream.tracker_retention.backup_max_age`, and the alarm that says so has nobody to name"}
```

Most deployments never run `validate` (an image, Helm chart or operator renders
the document and starts the node), but every deployment has the node's log, and
each advisory describes what the node does for as long as it runs. Tier A
changes only on a restart, so once per start is the whole record.

| Field | Meaning |
|---|---|
| `config` | `bootstrap`: which tier. `config_reference_unresolved` uses the same key for the same question, so one query finds both |
| `path` | Where in the document the remedy is written, exactly as `validate` locates it |
| `detail` | The whole message: what will happen, then what to do instead |

**After every Tier A refusal, and into the log file.** The lines are written
once the document, with every flag folded in, has passed every Tier A rule (a
refusal names the flags given) and once `-dev-principal` and `-mode` have been
checked, so no warning is printed beside a refusal. They are written after the
log file named by `logging.file.path` or `-log-file` is open, so they are in
it, unlike `config_reference_unresolved`, which comes out while the document is
still being read, before any file exists (see
[Deployment → Logging](../guides/deployment.md#logging)). What can still fail
after them is not Tier A's to refuse: a `CREWLET_NODE_ID` no node may carry, a
port somebody else holds, a seed company file held to the
[admission rules](../concepts/configuration.md#what-a-stored-revision-is-held-to)
when written, a store or broker that will not open.

**Judged on the document the run applies.** Flags are folded in first, so a
fleet node started with `-roles` is not told its roles are undeclared, and one
started with `-debug` is not told the embedded broker's debug output has
nowhere to go, unless `logging.stderr: false` hands the stream to a log file
with a quieter `logging.file.level` that `-debug` does not raise; the line then
names that level, never `-debug`. A start with nothing to say logs no such
line; a warning never stops a node or changes its exit.

**And the company's, at every apply.** The advisories a Tier B document raises
(a human seat nobody can message, a company nobody can join, an idle token
ceiling) are logged under the same event with `config=company`, by every node,
once per epoch it applies, with the revision, the epoch and the seat or unit
each is about (see
[What an applied company is worth knowing](../concepts/configuration.md#what-an-applied-company-is-worth-knowing)).
That reports them for a company `-company` or `-import-company` seeds, which no
write answers to anybody.

**One fact, one warning.** A node with `api.port` 0 logs `api_disabled`, `WARN`
only on a node whose `node.roles` is undeclared (every role, `ingress` among
them, which the `api.port` advisory leaves out on purpose). A node whose
declared roles name `ingress` was already warned by that advisory, so its
`api_disabled` is `INFO`, like a node without `ingress`.

### `-dev-principal`, and why it is a flag

Tier A's `api.auth.disabled` is retired: it authenticated the **empty**
credential into full operator authority with no check on the bind address, so
one unset environment variable bypassed every gate on any deployment. A file
still carrying it is refused at that line, saying the key was removed and that
local development uses `crewlet run -dev-principal` instead.

`-dev-principal` has two guards that field never had:

- **`api.host` must bind a loopback address** (`127.0.0.1`, `::1`,
  `localhost`): the only check that physically stops another machine reaching
  it. It judges the *bind*, not `api.external_url` (unlike
  `api.auth.accept_insecure`'s rule, which is about a session cookie crossing
  plaintext through a proxy, where the external address is the truth; this is
  about who can open a socket to the process). It runs **after** `-api-host` is
  applied, so moving the bind on the command line moves the decision.
- **The binary must be a development build.** A setting reaches production by
  being copied (a compose file, a Helm value, a forked base image). The release
  pipeline stamps a version at link time; anything the engine cannot positively
  recognise as a development build refuses the flag, whatever its
  configuration. That is why this is a flag and not a config field: a field
  gets copied.

It grants every app, and `admin` only where `api.auth.admin` is `serve`, so a
node refusing admin refuses it here too. Every boot that enables it logs
`api_dev_principal_enabled` at **warn** naming the login and the bind.

A credential that IS presented still wins: a Tier A token resolves as itself,
and a *wrong* token is still refused with a `401` rather than served as the
development principal, which would hide the typo.

For anything reachable from another machine, use an `api.auth.tokens` entry:
listable, revocable without a restart, and in the audit log.

Publishing knowledge and tool skills is its own command, not a flag on `run`: an engine publishing on every boot would rewrite a company's knowledge base from whatever tree the deploying machine had. Use [`crewlet confluence import`](#crewlet-confluence-import), and [`crewlet config import`](#crewlet-config-import) to load the first company revision.

Press `Ctrl+C` for graceful shutdown; signals escalate in two tiers:

1. **First `Ctrl+C`**: graceful. Pressed while the node is still starting, it ends the boot (once an embedded broker's store recovery in progress has finished, which cannot be cut short), the listener answering a drain's answers, as far as a [starting node](api-endpoints.md#while-a-node-is-starting) can, until the boot has unwound. Once the node serves, every held seat quiesces, running LLM turns finish their rounds (no internal timeout; `drain_in_progress` logs the in-flight count every 10 s), and turns queued behind the concurrency limit return to the broker for prompt redelivery. The HTTP surface stays up **for the whole drain** and closes only when it completes: `/health` stays `200`, `/ready` answers `503` naming the drain, and the dashboard and every read keep answering. From the first moment it stops taking work: every webhook, config, secret and setup write, operator write and backup answers `503` with `{"error": "draining"}` and a `Retry-After` (the drain waits on in-flight turns, and accepting deliveries would make more), and so does a new git fetch or push, in git's own `text/plain` form. A git request already running is finished: once the turns are done, the listener stays up until the node has served none for five seconds (`git_draining`, `git_drain_complete`).
2. **Second `Ctrl+C`**: the process exits immediately. The first signal hands signal handling back to the operating system so this works: in-flight turns are killed with their triggers unacknowledged, and the broker redelivers them once its ack window elapses. A node started with SIGINT ignored, as a background job of a non-interactive shell is (`crewlet run &` in a script), has no default to hand back; its second SIGINT still exits at once, with status 130 rather than by the signal, which a shell reports the same way (see [Force stop](../concepts/agent-runtime.md#graceful-shutdown)). A node started with SIGHUP ignored (`nohup crewlet run`) keeps ignoring a hangup, but starts every child, a box's run script among them, with SIGHUP's default.

There is no third tier: the second is an unconditional exit.

Under an orchestrator, SIGTERM follows the same tiers; the host's grace period (k8s `terminationGracePeriodSeconds`, systemd `TimeoutStopSec`, Docker `--stop-timeout` / Compose `stop_grace_period`) is the SIGKILL backstop. See [Graceful shutdown](../concepts/agent-runtime.md#graceful-shutdown).

When piping output, prefer `tee -i`: Ctrl+C reaches the whole pipeline, and a plain `tee` dies on the first press, taking the drain logs with it (the shutdown itself is unaffected).

---

## `crewlet config`

Manage the Tier B company configuration in the store. Every subcommand opens the store named by the Tier A bootstrap (`-config`, default `./crewlet.yaml`), and decrypts what it holds with that file's keyring.

**Revisions are immutable and the activation pointer is append-only.** Importing writes a new revision; an activation (through a running node, or the next start's publish of an offline one) appends to the pointer. So re-activating an *unchanged* revision is meaningful: it mints a new epoch every node watches, which is how a [rotated secret](../concepts/secret-store.md) reaches a running fleet without a restart.

### What a node's store knows

The company is the revision the fleet's [activation
pointer](../concepts/control-plane.md) names, and the pointer lives in the
coordination store (inside the running engine, on an embedded broker), which
these commands do not open. They read this node's own record, which keeps three
facts apart ([A node's own store keeps three facts
apart](../concepts/control-plane.md#a-nodes-own-store-keeps-three-facts-apart)):

- the revision this node **boots** on: the last one its engine applied, or one
  an offline command chose;
- the **fleet's** revision as this node last saw it, whether this build could
  run it or not;
- an **offer**: an offline choice of what to boot on that the fleet has not
  taken yet, with the revision it was built on.

A command that takes no revision id (`show`, `export`, `diff -against
active`, and the masks an offline `import` restores) answers **the company
this node knows**: an offer built on the fleet's revision this node last saw
(or built while it had seen none), otherwise that fleet revision, otherwise the
revision it boots on. On a node that refused the fleet's revision that is the
fleet's revision, not the one it runs, and `show` and `export` say so on
stderr: `this node boots on revision <id>: it runs that revision until it
applies <id>`. [`crewlet config revisions`](#crewlet-config-revisions) marks
each fact on its row. With the engine stopped, the fleet's revision is the one
this node last saw; `GET /config` on a running node answers the pointer itself.

**Offline, `import`, `activate` and `rekey` make an offer.** Each chooses what
this node boots on at its next start and records the revision the change is
built on, its **base**. For `import` and `activate` the base is the company
this node knows, or, where that is an unpublished offline change, the revision
that change was built on. For `rekey`, which re-seals the revision this node
boots on, it is that revision, or that change's base where the revision is an
unpublished offline change. Where the company this node knows is an offline
change a start already published and could not record (it stopped between the
publish and the record), whether the fleet took it is unknown offline, so the
new offer is built on that change **or** its base, and the note names both.
The next start publishes the offer by compare-and-set against what it was built
on: only while the pointer still names that revision, or, built on no company,
only while the fleet has none. A change the fleet has moved on from is
**withheld**, never published over the fleet: the node boots on it, logs
`offline_revision_not_published` at warn on every start, and runs it until it
applies the fleet's revision, which then becomes what it boots on, the offline
change staying in its history ([What a start publishes](#crewlet-run)).
Each command's note names the revision its change is built on:

```
This node boots on it at its next start and publishes it to the fleet only while the fleet still runs <base>, the revision it was built on; if the fleet has moved on, it runs the fleet's revision once it can and says it did not publish this one.
```

A change built on no company says `only if the fleet has no company yet`
instead; one built on a change a start published and could not record adds
`, or <id>, the offline change it replaced, which an earlier start of this node
published and could not confirm`. A withheld change put up as it stands would
undo what the fleet changed since, so the warning's hint names the change
itself: `GET /config/revisions/<revision>/diff?against=<built_on>` on that node
shows what it did, to make again on the fleet's document (through the
dashboard, or `crewlet config import -api`); `POST /config/revisions/<revision>/revert` there puts it up as it stands, for an
operator who means that.
### `crewlet config import`

```
crewlet config import <company.yaml> [-config PATH] [-api URL] [-summary STR]
```

Validates the Tier B YAML and writes it as a new revision with the replaced one as `parent_revision_id`: through a running node the fleet's revision, replaced fleet-wide; offline, [the company this node knows](#what-a-nodes-store-knows), and the new revision becomes this node's offer.

**A redacted read imports over the company it was read from.** `crewlet config
show`, `GET /config` and the dashboard's Copy as YAML mask every inline credential as `__redacted__`. On import each mask is restored from the revision being replaced, by the identity of what holds it (a seat by handle, a unit by key, an entry by key), and only then is the document validated (by `PUT /config` through a node, against the company this node knows offline). A mask nothing there answers (a new or renamed member, or a store that knows no company) is refused naming its field; nothing is stored.

**It reaches a running node.** The store is exclusive to one process, so against a live engine the command detects the held store and goes through that node's `PUT /config`, which stores the revision **and activates it fleet-wide** with no restart. A node still **starting** holds its store too, from the read its start decides on ([step 6](../concepts/configuration.md#bootstrap-sequence) of the boot), and answers `503 starting` until it serves: run again then. It is never written behind the start's back, under the revision the node is about to publish. `-api URL` names a node explicitly, also how it works from a machine that is not the node. [`crewlet secrets`](#crewlet-secrets) makes the same test. With the engine stopped the two part ways: a revision goes to this node's own store, a secret to the fleet's.

Through a node the write is a [compare-and-set](api-endpoints.md#concurrent-writes) on the revision the fleet's pointer names (every node answers it alike, one that refused the fleet's revision included), so it loses only to a write landing between the node's pointer read and its own.
- Coordination store unreadable before anything is stored: `503 unavailable`, nothing stored; run again.
- Store stops answering after the revision is stored, during activation: the import names the stored revision and says the outcome is unknown. See `GET /config` for whether it is live, and the node's `revert` route to make it live if not; running the import again makes another revision, over this one if it landed.
- Another write activated first: the import says so and names the winner. If the node kept the document as an inactive revision it names that too, with the routes to compare and activate it (`GET /config/revisions/<UUID>/diff`, `POST /config/revisions/<UUID>/revert`), because `crewlet config diff` and `crewlet config activate` open the store the running engine holds. It names the node that stored the revision, whose history alone holds it: where `-api` is shared by several nodes (a load balancer, a Service), send diff and revert to that node's own address, or get `404`.

With the engine **stopped** it writes this node's own store, as the [offer](#what-a-nodes-store-knows) this node boots on at its next start, built on the company this node knows. That start publishes it only while the fleet still runs the offer's base (or, where this node's last start published an offline change and could not record it, that change), which the printed note names; if the fleet has moved on, the import is this node's alone until it applies the fleet's revision. The printed line says which route was taken.

**It never refuses a file for its charter.** A company file whose mission or policy breaks the [charter's rules](../concepts/configuration.md#mission-vision-and-policies) (past its length, or on two lines) ran under an earlier build, and restore or migration must work: each fault is printed as a `warning (admission): …` line and the file is imported, both ways. Through a node it sends `PUT /config?import=true`, which judges the charter warn-only (an edit is judged on what it changes). That mode is `admin`'s: `CREWLET_API_TOKEN` must be a Tier A token, or a machine token whose owner holds `admin`. The revision records `source: file`, as offline does, and the credential in `operator_id`. Every other rule holds as for any write.

`-summary` is the audit note (default `imported from <path>`, the path cut to 512 bytes); one past the 4,096 bytes the activation pointer carries, as the pointer encodes it, is refused before anything is stored, naming the flag. Through the API the revision records the author the node resolved: `created_by` is `token:<id>` for a Tier A token, a person's seat for a person (every person holds one), the owner for a person's machine token; `created_by_kind` is `operator` or `human`; `operator_id` names the credential. Offline it is the invoking operator (`$CREWLET_OPERATOR`, else `$USER`, else `$LOGNAME`, kind `operator`, no credential) and `source` is `file`; neither is settable from the command line. The author travels with the revision to every node.

Through the API an import is `admin`'s alone: `PUT /config?import=true` is refused `403` to a [unit's lead](api-endpoints.md#a-lead-edits-their-own-team) before the body is read, even for a file changing only their units; leads use `PUT` or `PATCH /config` or the entity routes. Offline anyone who can open the store on the host can write it.

It does **not** refuse because the company already exists, and no flag forces it: the pointer is append-only, so an import *chains* a revision, and `crewlet config activate` makes any earlier id the offer again.

**Idempotent by content**, like the [boot seed](#crewlet-run): offline, a file matching the company this node knows imports nothing and says so (naming the revision this node boots on where that is another); an edited one imports once.
- A revision stored **without a seal** is never compared and the import replaces it, which is why every command refusing such a revision names this one. Through a node `PUT /config` does the same (see [A whole document replaces an unsealed revision](api-endpoints.md#a-whole-document-replaces-an-unsealed-revision)).
- A revision **sealed in a form this build does not read** (likely a newer build) is not unsealed: the import and every other command refuse it, naming the build that sealed it.

Like `activate`, an offline import writes **this node's** store; the note names the revision it is built on and what reaches a running fleet without a restart: `PUT /config` to a node that is up, or this command run while a node is up.

**On a managed document** (Tier A names [`api.auth.company_writers`](../concepts/configuration.md#managed-configuration)) the offline route presents no credential and is refused, naming the writers. Through a node, the token judged is `CREWLET_API_TOKEN` only (`api.auth.tokens` is what a node accepts, never a wallet the CLI uses): a writer imports as before, any other gets `config_managed`, printed naming `CREWLET_API_TOKEN` as the credential sent.

### `crewlet config export`

```
crewlet config export [-config PATH] [-revision UUID] [-redact]
```

Dumps [the company this node knows](#what-a-nodes-store-knows) (or `-revision <UUID>`) as YAML to stdout, with a stderr line where, without `-revision`, this node boots on another revision. Not the stored bytes: the payload is decrypted with the Tier A keyring and re-encoded, so an [encrypted](../concepts/configuration.md#secrets) revision exports readable, not as its `{__encrypted__: "enc:v1:…"}` blob.

> **Without `-redact` this prints secrets in the clear.** `${VAR}` references export as themselves, but a config that *inlines* a literal key exports it. This is the one command that does, deliberately: a genuine restore or migration needs it.
>
> **`-redact` is what you want for anything you share.** It masks every field the config types declare secret-bearing (structurally, not by text pattern). `crewlet config show` and `crewlet config diff` are always redacted, with no flag to turn it off.

### `crewlet config show`

```
crewlet config show [-config PATH]
```

Prints [the company this node knows](#what-a-nodes-store-knows) as **redacted** YAML, exactly `crewlet config export -redact`, never unredacted. On a node that refused the fleet's revision it prints that revision, with a stderr line naming the one the node boots on. `GET /config` on a running node answers what the fleet runs now; `crewlet config revisions` gives id, author and audit note.

Errors with ``this node knows no company; run `crewlet config import` `` when it knows none.

### `crewlet config revisions`

```
crewlet config revisions [-config PATH] [-limit 20]
```

Lists recent revisions, newest first: columns `REVISION`, `STATE`, `CREATED`, `BY`, `SOURCE`, `SUMMARY`. `STATE` is what this node's store [knows each revision to be](#what-a-nodes-store-knows):

| `STATE` | Means |
|---|---|
| `boots` | This node starts on it: the last revision its engine applied, or one an offline command chose |
| `fleet's` | The fleet's revision as this node last saw it, whether this build could run it or not |
| `offered over <id>` | An offline change the fleet has not taken; the next start publishes it only while the fleet still runs `<id>`. `offered over (no company)`: built on no company, published only while the fleet has none. `offered over <id> or the unconfirmed <offer>`: made on an offline change an earlier start published but could not confirm; published while the fleet runs either |
| `-` | History |

A row can carry several, comma-separated:
- `boots, fleet's`: the node runs the fleet's revision as it last saw it.
- `boots` on one row, `fleet's` on another: the node refused the fleet's revision or has not applied it yet; it boots on the first.
- `boots, offered over <id>`: an offline change published at next start while the fleet still runs `<id>`, withheld once it does not.

With the engine stopped, `fleet's` is the revision last seen; the pointer may have moved. For a running fleet [`GET /fleet`](api-endpoints.md#get-fleet) gives per node `config_serving_revision_id` (what it runs), `config_boot_revision_id` (what it would boot on: the `boots` row, equal to what it runs except while a write to that row is retried) and `config_revision_id` (the last one attempted), with how that went.

`BY` is the author, with the credential beside them where it names something the author does not: `jane.doe (through pat:…)` for a person's machine token. A Tier A token's `token:<id>` is both, said once.

### `crewlet config diff`

```
crewlet config diff <UUID> [-against <UUID|active>] [-config PATH]
```

Prints a **structural** diff of two revisions: one line per path that moved, with the value on each side. `-against active` (default) compares against [the company this node knows](#what-a-nodes-store-knows); the direction reads "what `-against` became", so `crewlet config diff <UUID>` answers "what would reverting to this change".

```
--- active
+++ 35ecd5ab-96ff-4d6d-b101-9de41d644832
~ providers.llm.main.model: "claude-opus-5" -> "claude-sonnet-5"
+ mcp_servers[1].url = "https://mcp.example.com/sse"
- integrations.github.enabled (was true)
```

**Paths and values, not lines**: the stored form is marshalled JSON, where reordering a map or adding a defaulted field rewrites meaningless lines. It is the differ [`GET /config/revisions/{id}/diff`](api-endpoints.md) serves. Strings are quoted and other values not, so a `"true"` to `true` change is visible.

**Every change, however many.** `GET /config/revisions/{id}/diff` cuts its listing at 500 entries (body and dashboard socket frame size budget) and reports `changes_total`; a terminal has no such budget, so this prints everything.

**Both sides are always redacted, with no flag to turn that off**: a diff gets pasted into tickets and chats, the likeliest way a credential leaves the machine. `crewlet config export -revision <UUID>` is the deliberate route to real values. A changed credential still shows: revisions compare as stored, so a rotated key is a change from `"__redacted__"` to `"__redacted__"` at its path (the mask is `__redacted__`), with neither value.

### `crewlet config activate`

```
crewlet config activate <UUID> [-config PATH]
```

Makes a stored revision what **this node** boots on, as an [offer](#what-a-nodes-store-knows) built on the company this node knows: the next start publishes it to the fleet only while the fleet still runs the offer's base, and every node then applies it on its next reconcile. It opens the store, so it runs while the engine is stopped. It prints `marked <UUID> to boot on this node; it is offered to the fleet over <id>`, then the note. On a **running** fleet, `POST /config/revisions/<UUID>/revert` to a node that is up does the same without a restart (stores that revision's document again, built on the fleet's revision, and activates it); the note names it.

**Held to the runnable rules first**, since the next start boots on it: a revision this build cannot run is refused, nothing written, as `revision <UUID> cannot run on this build, so this node could not start on it`; so is one this node's keyring does not open, naming what brings it back.

**Activating the fleet's own revision is not a no-op.** The offer is built on that revision, so the next start publishes it over itself at a later instant than the pointer's, and the append-only pointer mints a new epoch. A node's reconciler skips on the *epoch* it has applied, never the payload, so the apply always runs: it re-reads the [secret store](../concepts/secret-store.md) and rebuilds every provider, transport and MCP child that captured a resolved value. This is the documented way to make a rotated credential take effect; on a running fleet the same gesture is `POST /config/reload`. Activating an earlier revision is a rollback by explicit choice, landing only while the fleet is where this node last saw it; on a node that was behind, either is withheld like any offer.

On a [managed document](../concepts/configuration.md#managed-configuration) activating the company this node knows stays open (it changes nothing); any **other** revision is refused (the one this node boots on while it refuses the fleet's revision included), because it presents no credential and replaces the document the managing system wrote.

### `crewlet config rekey`

```
crewlet config rekey [-dry-run] [-config PATH]
```

Rotates the master key: re-encrypts the config document this node **boots on** (not necessarily the fleet's, on a node that refused it) under the current `secrets.active_key_id`, writing a new revision this node boots on. It decrypts with whatever key sealed it (that key **must still be in** `secrets.keys`). A store that boots on no revision has nothing to re-seal: refused, naming `crewlet config import`.

**Run this and [`crewlet secrets rekey`](#crewlet-secrets-rekey) together.** They are the two halves of one rotation (document and secret store share one keyring); doing one, then dropping the retired key, makes whatever the other holds unreadable.

It opens the store directly, so it runs on a **stopped** node. The re-sealed copy is an [offer](#what-a-nodes-store-knows) whose base is the revision it re-seals (or, if that is an unpublished offline change, the revision that change was built on), published at next start only while the fleet still runs that base. A copy of a revision the fleet moved past is this node's alone, withheld with `offline_revision_not_published` on every start until the node applies the fleet's revision, and never replaces the company that superseded it. While the engine holds the store it refuses and names the node's equivalent, `POST /config/reload`, which stores the fleet's document again under that node's active key. A reload re-seals the fleet's revision, which a node that refused it does not boot on: there, run this command too before the old key goes, or its next start cannot open the revision it boots on. Workflow: `crewlet secrets keygen -key-id <new>` → add the new key to `secrets.keys` on every node and restart → set `active_key_id: <new>` and restart again → `crewlet secrets rekey` **and** this command, or a reload → drop the old key only once [the rotation runbook](../concepts/secret-store.md#rotating-the-keyring-with-nothing-in-flight-lost)'s conditions hold.

`-dry-run` reports what would move by reading the key id off the envelope, decrypting nothing. Idempotent: a document already under the active key is skipped, and says so. Refused: a **plaintext** revision (no key to rotate, nothing vouches for its writer; the refusal names `crewlet config import`, which stores it sealed again from your own copy); a document sealed under a key no longer in the keyring (names the key); one sealed in a form this build does not read (names the build).

---

## `crewlet iam`

```
crewlet iam <command> [SUBJECT] [-config PATH] [-api URL] [-json] [-reason TEXT]
```

The company's [people, credentials and sessions](../concepts/identity-and-access.md), from the command line.

**Every one of these goes through a running node**: the directory is a state-log domain, a change is a RECORD arbitrated at the broker, and the broker is embedded in the engine with no listener. A second process cannot publish one, and an offline read would be a second applier of the exclusive store file.

**Authentication is `CREWLET_API_TOKEN` and nothing else.** Tier A `api.auth.tokens` is what a node *accepts*, not a wallet this command uses. Export one of its values, or a machine token minted with `crewlet iam token`: it acts as the person or service account it was minted for, refused only `iam token` itself and the gestures about how its owner signs in. The exception is `iam token -login`, which mints **your own** token, signing in as you without reading the variable.

**A person holds a human seat for as long as they are here.** They are invited onto a vacant one (`invite -seat`) or created on one (`create -seat`) (the dashboard's Company › Structure **Invite** and **Add person** do the same), `bind` moves them, `remove` frees the seat. Nothing clears a person's seat; `unbind` is for service accounts, whose seat is optional. `seats` lists which are free.

| Command | What it does |
|---|---|
| `people` | The directory, paged. `-q` narrows on login and seat; `-stage` on enrolment stage; `-limit` on the page; `-after ID` starts the next page, as the listing's last line spells it |
| `show ID` | One person, in full, with name and address opened |
| `seats` | The company's **human seats** and what holds each, in declared order: `HANDLE`, `NAME`, `UNIT`, `HOLDER` (marked `(service account)` for one), `STAGE`, the open `INVITATION` (its id and the address sent to) and when it `EXPIRES`. **What a token reads depends on whose it is**: for an admin, and the holder themselves, `HOLDER` is the login; for anybody else the name they go by (`(name unavailable)` where this node's keyring did not open it, `(held)` for somebody enrolled with no name), and `STAGE`, `INVITATION`, `EXPIRES` are an admin's alone, so a seat with an open invitation reads as unheld. `-unheld` (an admin's) keeps only **vacant** seats (nobody holds them, no open invitation is for them), the ones `invite` and `create` take. An empty listing says why: the company declares no `kind: human` seat, or every seat is held or invited (an empty `-unheld` tells these apart by asking the node for every seat once, never under `-json`); the remedy names what frees a seat: `remove` for a person, `unbind` for a service account, `cancel-invite` for an invitation, or a new `kind: human` seat. A node running no company yet refuses it, naming a company declaring a human seat as the step before. See [`GET /iam/seats`](api-endpoints.md#get-iamseats-lists-the-human-seats-and-who-holds-each) |
| `invite EMAIL -seat SEAT` | Invite someone to a vacant human seat (no person, no open invitation). **`-seat` is required**, refused before any config is read, naming `seats -unheld`. Prints where the issue stands (`applied` or `pending`, with its op id, as every write) and the link **once** (nothing stores it, no route reads it back; good for a week): the dashboard's invitation screen, `<api.external_url>/dashboard#/invite/<id>.<secret>`; redeeming it creates the person on that seat. While open the invitation **holds the seat** as it holds the address: a create, move or other invitation naming the seat is refused, and so is a company write removing the seat; see [An invitation names its seat, and holds it](../concepts/identity-and-access.md#an-invitation-names-its-seat-and-holds-it). Under a Tier A token on an empty estate it is how the company's **first person** arrives, onto a human seat the company declares (declare one first), with `-grants admin`; see [How the first person exists](../concepts/identity-and-access.md#how-the-first-person-exists) |
| `invitations` | Invitations not redeemed and still good, paged like `people` (`-limit`, `-after ID`); `-all` adds expired and redeemed ones the estate still holds, with state. Each row: for whom, seat bound, what it confers, issuer, until when; **never the link** (shown once): an inviter who lost it cancels and reissues |
| `cancel-invite ID` | Withdraw an unredeemed invitation: its link opens nothing, and the address and seat it held are free at once. A redeemed one is refused naming the person it created; `remove` undoes that. See [A person arrives on their seat: invited, or created](../concepts/identity-and-access.md#a-person-arrives-on-their-seat-invited-or-created) |
| `create` | Create somebody directly, active at once. **A person** (`create -email E -seat SEAT [-login L] [-name N] [-grants G,...]`, every app but `atlas` when `-grants` is left out) needs the sign-in address and a vacant human seat; either missing is refused before any config is read. Without `-login` the node proposes one from the address (`jane.doe@example.com` → `jane.doe`) and the command prints it. It prints id, login, seat and outcome, then their **first password link, once** (`<api.external_url>/dashboard#/reset/<id>.<secret>`, good for a week), which sets the first password and signs nobody in: they sign in afterwards, enrolling a second factor where the deployment requires one. Send it yourself; the engine sends no mail. See [A person created directly gets their first password link](../concepts/identity-and-access.md#a-person-created-directly-gets-their-first-password-link). **A service account** (`create -kind machine -login L -grants G,... [-seat SEAT] [-name N]`) needs its login (no address to find it by) and `-grants` (nobody behind it to mean a default; `-grants none` for the overview alone; missing is refused before any config is read); `-seat` is optional and binds it to act as that human seat (`-login token:<id> -seat SEAT -grants none` binds a Tier A token in one step; the row carries nothing itself, the token's permissions being its Tier A entry's). No link (no password); the command prints the `iam token -person ID` that mints its token. **Every principal enrols with a login** in its kind's grammar: a person's dotted (`jane.doe`), a machine's coloned (`ci:release`, or `token:<id>`), at most 64 characters; anything else is refused |
| `bind ID SEAT` | **Move a person to another vacant human seat**, or bind a service account to one, so they act as it on the engine's own surface (inbox, day, lead relations). `SEAT` is a **human** seat's handle that no person and no open invitation holds. A Tier A token acts under the login `token:<id>`, so binding one is a service account's row under it: `create -kind machine -login token:<id> -seat SEAT -grants none`. See [Humans in the Org Chart](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api) |
| `unbind ID` | Take a **service account's** seat back. A person always holds a human seat, so the node refuses (`seat_required`) and the command names what frees it: `bind ID SEAT` moves them, `remove ID` frees it |
| `grant ID` | Change permissions: `-grants`, the whole set: `admin` (which stores every app beside it), `worklet`, `doclet`, `artifacts`, `chatlet`, `gitlet`, `atlas`, or `none` |
| `suspend ID` / `activate ID` | Stop somebody acting, or let them again. The row stays; a suspension also ends every session, token and password reset link they hold, which activating does not bring back |
| `remove ID` | Tombstone them (freeing their seat) and erase every sealed value of theirs (name, address, second factor) from every data node's rows, the invitation and trail rows that outlive them included. The identity log keeps the records that wrote those values, sealed under the keyring, until the retention trim passes them, and a backup keeps what it held until deleted; their login is not sealed and outlives removal in the clear. See [Removing somebody erases what is theirs from every data node's rows](../concepts/identity-and-access.md#removing-somebody-erases-what-is-theirs-from-every-data-nodes-rows) |
| `revoke ID` | End every session, token and password reset link they hold, by raising their sign-out count |
| `sessions ID` | Their sessions, newest first, ended ones included, each with why and when it ended (a password change, a reset link, signing out everywhere, a suspension; a later gesture never rewrites it), or, for one ended with every session in the company, that |
| `credentials` | List someone's credentials. `-person` names them; without it, yourself |
| `token` | Mint a machine token for exactly one owner. **`-login L` mints your own**: a person's token is theirs alone, so the command signs in as `L` for the one request (password read from the terminal without echo or the first line piped in, then a second-factor code if the account holds one, asked or piped the same way; each piped line refused past 4 KiB without its line ending, the most a sign-in carries, so a pipe at the wrong file is refused, not read whole), mints from that session, and signs out, reading no `CREWLET_API_TOKEN`. **`-person ID` mints a service account's**, takes `admin`, and is refused for a person's id, whoever asks. The value is printed **once**, with where the mint stands (`applied` or `pending`, with its op id, as every write; a `pending` token works on every node that has applied its record, and one that has not answers it `503` rather than refusing it); the estate holds a hash. It acts as that owner with `-grants` (default: everything the owner holds that the minting session may confer, `admin` included, except on a node whose `api.auth.admin` is `refuse`, where it gets the owner's apps) cut to what they still hold on every request, for `-days` (90, at most 365). A token may carry `admin`; the authority table's presence rows and the identity domain's refusal of an administrative record written through a token keep it off gestures needing a person present. See [Machine tokens](../concepts/identity-and-access.md#machine-tokens-a-persons-own-and-a-service-accounts) |
| `revoke-credential ID` | Withdraw one credential, naming its owner with `-person`. With a machine token, machine tokens only |
| `reset-mfa ID` | Clear the second factor **and** end every session (clearing alone leaves sessions opened with it live), and with them every token and reset link they hold; issue any link they need after this |
| `reset-password ID\|LOGIN` | Issue a **one-time password reset link**, printed **once** with where the issue stands (`applied` or `pending`, with its op id), as `<api.external_url>/dashboard#/reset/<id>.<secret>`, good for a day; send it yourself. The person is named by id or login, resolved to the one row holding it exactly. Spending it sets a new password, ends every session and token they held, and signs nobody in: they sign in afterwards, where a second factor they hold still applies. Issuing another revokes the one before (a created person's first password link included, so this is also how somebody whose first link lapsed gets a new one; their screen then says it sets their first password); `suspend`, `revoke`, `reset-mfa` and `invalidate-all` each end it, so issue it after any of those it is needed beside. Refused: a machine, somebody suspended or retired, somebody holding a permission you do not (you are shown the link, so the rule is an invitation's). Takes no `-op-id`: a retry could not hand back a secret its first attempt never showed, so an unknown issue is issued again. A person changes their **own** password signed in, through `POST /auth/password`, which needs a person present (no token this command reads is one), so no command exists for it. See [A forgotten password is a one-time link from an administrator](../concepts/identity-and-access.md#a-forgotten-password-is-a-one-time-link-from-an-administrator) |
| `invalidate-all` | Invalidate every session, machine token and password reset link in the company. The restore runbook's last step: it ends bearers and nothing else, so a removal, suspension, withdrawn credential or reduced permission the restore rolled back is re-applied by hand before it, and a reset link the restore calls for is issued after ([Backups & Restore](../guides/backup.md#the-last-step-is-crewlet-iam-invalidate-all)). Takes `admin` on a Tier A token (a machine token is refused, whatever it carries); the config file's Tier A tokens are untouched |
| `check` | Find problems with the company's access: no administrator, people with no way in, dangling bindings (a seat the running company no longer holds as a human seat), an administrator this node clamps because it refuses admin. A binding a node running no company yet cannot judge is counted and said first rather than reported either way. See [below](#crewlet-iam-check) |
| `audit` | Show the identity audit trail. `-person`, `-event`, `-since POSITION`, `-at TIME`, `-before POSITION` (the next page, as the listing's last line spells it), `-limit`. The actor column names the credential beside them where recorded: `ana.admin (through pat:…)` for something their machine token did |

### Flags

| Flag | Default | What it is |
|---|---|---|
| `-config PATH` | `crewlet.yaml` | The node config file naming the node to reach |
| `-api URL` | the `api.host:port` in `-config` | The running node to talk to |
| `-json` | off | Print the node's answer as JSON, not a table: fields and numbers as sent, text as written (`<`, `>` and `&` as themselves), keys sorted |
| `-reason TEXT` | the surface's own | Saved with the change for the audit trail |
| `-grants G,...` | see below | Comma-separated `admin`, `worklet`, `doclet`, `artifacts`, `chatlet`, `gitlet` and `atlas`, or `none` for an empty set; anything else is refused before any config is read, naming the set. `admin` also sends every app, as the dashboard's picker stores it, so taking admin away later leaves the apps. Left out: on a person's `invite` or `create`, **every app but `atlas`**, which is [given on purpose](../concepts/identity-and-access.md#atlas-is-given-on-purpose), and not admin; on a service account's `create`, refused (no default); on `grant`, nothing changes; on `token`, everything the owner holds that the minting session may confer. `none` stands alone (beside a permission it is refused), and the flag is refused on every command but `invite`, `create`, `grant` and `token` |
| `-person ID` | yourself | Whose credentials or sessions, and on `token` the **service account** a minted token is for; a person's id is refused there, whoever asks |
| `-login L` | — | On `create`, the login to enrol under: required for a service account, proposed from the address for a person given none. On `token`, **your own** login: the command signs in as you for the one request, mints, and signs out. The password and any second-factor code are read from the terminal or piped in, and **no flag takes either**: a recovery code on a command line lands in shell history and `ps` and stays good there (a sign-in refused for a mistyped password spends nothing) |
| `-email E` | — | On `create`, the address a person signs in with: required for a person, optional for a service account |
| `-name N` | — | On `create`, their own name |
| `-kind K` | `person` | On `create`, `person` or `machine` (a service account) |
| `-label L` | — | What to call a minted token |
| `-days N` | 90 | How long a minted token lasts, at most 365; "forever" is deliberately unexpressible |
| `-q TERM` | — | Narrow the directory on a login or a seat |
| `-stage S` | every stage | `invited`, `enrolling`, `active`, `suspended` or `retired` |
| `-since POSITION` / `-at TIME` | — | Where the trail starts: `-since` is a log position; `-at` is an RFC 3339 instant the route resolves to one |
| `-limit N` | 50 (100 for the trail) | How many rows |
| `-after ID` / `-before POSITION` | the first page | Where the next page starts: `-after` on `people` and `invitations`, `-before` on `audit`. Each listing ends with the command for its next page, repeating every filter |
| `-all` | open ones only | On `invitations` only: expired and redeemed invitations too. Refused elsewhere |
| `-seat SEAT` | — | On `invite` and `create`: the human seat they will hold, by handle, one nobody holds and no open invitation is for (`seats -unheld` lists them). **Required** on `invite` and a person's `create`, optional on a service account's. Refused elsewhere; somebody who already exists is moved with `bind` |
| `-unheld` | every seat | On `seats` only: the vacant ones. Refused elsewhere |
| `-op-id OP` | a new operation, minted before the command asks | Retry a write whose outcome was unknown **as the same operation**: the op id its refusal named, or the one an unanswered write printed. Refused on a read, on `token` and on `reset-password` (a retry would hand back a record whose value was shown to nobody, so an unknown mint or issue is made again) |

**`-grants none` strips; an omitted `-grants` means what the command
states.** On `grant` these are opposite intentions an empty flag value cannot
carry, so the word is explicit (and could never be a permission). `-grants
none` on a person leaves the
[overview](../concepts/identity-and-access.md#everybodys-overview) alone: they
see the org chart, what the agents are doing and the spend, and use no app,
the company's code included, which takes `gitlet`.

**A value shown once is shown once.** `invite`, a person's `create`,
`reset-password` and `token` each print something nothing can read back; the
estate holds only the invitation's id and a SHA-256 of the secret its link
carries after the id (same for a first password link and a reset link), and a
SHA-256 of the token. A lost one is re-issued: an invitation cancelled and
issued again, a first password link replaced by `reset-password`. The
exception: an `invite` or `create` retried with `-op-id <op id>` is the same
operation and hands back the very link its first attempt issued while it still
opens (both are derived from the operation under the company's own key). A
first password link that no longer opens is not printed: the command says so
and names the `reset-password` that issues another.

**Every write prints its outcome rather than "done".** `applied` means this
node has the change; `pending` means it is durable, not yet applied here, and
carries the position to read at; each with the op id it was published under. A
link is printed on `pending` too: it opens on every node that has applied the
record. `unknown` means nothing can be established from here; it fails naming
the op id and the one safe retry: the **same** command with `-op-id <op id>`.
These fail the same way, naming the op id: a write the node never answered
(connection dropped, or a proxy answered in its place; every write that takes
`-op-id` is sent under one the command mints first, so a lost answer never
loses the operation), and a write answered successfully with an outcome this
build cannot read (a newer node may) or none. For the latter nobody can vouch
for it, so the command **exits non-zero** naming the op id and prints nothing
of the answer, under `-json` too: no invitation, first password link, reset
link or token. The retry under `-op-id` hands back an invitation's or a
create's link; a token or reset link is made again (below). A fresh attempt
would be a second operation: for `create` and `invite` a second person or
invitation, which the first attempt's person or invitation refuses as a
conflict, since it holds the address and the seat. `token` and
`reset-password` are the exceptions because their routes read no key: a mint
or an issue answered `unknown`, with an unreadable outcome, or with none,
names the op id the node answered for the trail and is run again as a new
one. A token that may have landed is one nobody holds and expires; a reset
link issued again revokes the one before.

**An edit refused partway says what landed.** `bind`, `suspend` and the rest
are one `PATCH`, which moves the login and the seat in one record before the
stage and the grants. A login or seat somebody else (or an open invitation)
holds refuses that first record with nothing changed; everything else the node
can judge is refused before it; what only a later record can decide fails
listing the changes that did land.

**A refusal names the command that clears it.** A seat or address an open
invitation holds is refused naming the invitation, and the command adds the
`cancel-invite ID` that withdraws it. A person's seat left out is
`seat_required`, and the command adds what to do: for `invite` or `create`,
`-seat` with a seat `seats -unheld` lists; for `unbind` on a person, `bind ID
SEAT` to move them or `remove ID` to free the seat.

### `crewlet iam check`

One walk of the directory as the node you reach holds it, printed as a
`FINDING`, a `WHO` and the node's sentence saying what to do, in the table's
order. It reports and never repairs: each finding is somebody's decision. A
seat binding the node could not judge (it runs no company yet) is counted and
said first, because it qualifies everything below.

| Finding | What it means | What to do |
|---|---|---|
| `no_admin_holder` | No active **person** holding a credential they have **used** (password, second factor, token) holds `admin`, so nobody can invite, grant or revoke except through a Tier A token. An unspent first password link does not count, nor does a service account holding `admin` (no machine token writes an administrative record) | `grant ID -grants admin` for somebody, with a Tier A token |
| `person_without_credential` | Somebody active with no live way in: a person whose first password link lapsed unspent or was revoked, or whose every credential has been; a service account with no live token. A live link counts, so somebody created a minute ago is not reported until theirs lapses; an invitation creates nobody until redeemed, so is never the cause | A person: `reset-password ID`. A service account: `token -person ID`. Or `remove` them |
| `binding_dangling` | Somebody bound to a seat the org chart no longer holds as a human seat (removed, or made an agent's) | A person: `bind` them to another human seat, or `remove` them. A service account: `unbind` it, or `bind` it elsewhere |
| `admin_clamped_by_node` | Somebody's row holds `admin` and this node's `api.auth.admin` is `refuse`, so through this node they administer nothing (and keep every app) | Expected on a node published to the people who use the company; otherwise administer through a node that serves admin, or set `api.auth.admin: serve` here |

A login, address or seat two people hold is not a finding: each is decided on
one subject against every row, so ordinary traffic never produces one. A
record the node cannot decode, or a restore, can leave two rows holding one. A
lookup or seat listing that meets them answers `503` rather than pick either;
a sign-in is refused like any failed one and the node logs
`api_sign_in_lookup_failed`. Repair a duplicated login or seat by changing it
on all but one: a login through `PATCH /iam/people/{id}`, a seat with `bind`
(for a service account, `unbind`). A duplicated address, which no gesture
changes, is repaired with `remove` on the extra person.

## `crewlet secrets`

### `crewlet secrets keygen`

```
crewlet secrets keygen [-key-id ID]
```

Prints a fresh base64 32-byte encryption key plus a copy-pasteable `crewlet.yaml` `secrets:` snippet referencing it via an environment variable (keeping the raw key out of the file). `--key-id` (default `key-1`) names the key; the id is stamped into every envelope the key seals, so keep it stable across restarts and pick a new one only when rotating. Generation is always explicit: a silently generated key that isn't captured makes every backup unrecoverable. See [Secrets](../concepts/configuration.md#secrets).

The other subcommands operate on the [secret store](../concepts/secret-store.md): the company's encrypted credentials, one sealed value per `${VAR}` name, consulted **ahead of** the process environment. Without `-api` they read the Tier A bootstrap (`-config`, default `./crewlet.yaml`) for the way to the store and its keyring; the file must carry one, since the store has no plaintext mode: a config declaring no `secrets.keys` is refused with a pointer at `keygen`. With no file and no `-api` they are refused naming both (`rekey` names only `-config`). Through `-api` a machine needs no file for any but `rekey` (below).

**There is one store; whether this node's engine runs decides only the way in**, and the command says which it used. The rows live on the coordination KV so every node reads them. While the engine **runs**, that KV is behind it (on the default topology inside its own process, no socket), so the command goes through the node's authenticated `/secrets` API. While it is **stopped**, the command reaches the store itself: on a solo node (embedded broker, no `stream.cluster`) it starts the node's own broker on `stream.store_dir`, with no listener, and stops it when done; on `stream.type: nats` it dials the external broker with Tier A's credentials. On a node that has never booted it creates the fleet's buckets as the engine's first boot would, and the engine adopts them, so a value set before the first `crewlet run` is the one the engine reads. The engine's exclusive database lock (with a pid attached) tells the two apart, held from the moment a start reads the store to decide what to boot on ([step 6](../concepts/configuration.md#bootstrap-sequence)), before its broker starts, for as long as the node runs. So a command started while an engine is starting takes the API route, and the listener answers `503 starting` until the node serves: re-run then. The command only asks the lock; what it holds while working is the lock of the broker it started on `stream.store_dir`, so an engine started meanwhile is refused there, naming the command's pid. Only a start with `-import-company`, which reads no store first, leaves the database unlocked until its engine opens it; a command started in that stretch meets the engine's broker on `stream.store_dir` instead and is refused, told to re-run once the engine is up. While the engine holds the database, a command that cannot use its API (no `CREWLET_API_TOKEN`, or an `api.port` of 0) is refused with both facts, the lock and why the API could not be used, and offered the ways past: name a node that serves admin with `-api`, or stop `crewlet run` and re-run, reaching the store itself (any node but a cluster member). See [Which store the CLI writes](../concepts/secret-store.md#which-store-the-cli-writes).

Two kinds of node cannot reach the store on their own and are refused; `-api` is the way round for both. A **member of a cluster** (`stream.cluster`) with its engine stopped: alone, its broker never reaches the quorum the store needs, and beside running peers it would rejoin the cluster as this node. Name a running member, or start this node's engine and re-run. And a **node that holds no data** (`store.scratch`), engine running or not: it holds none of the store (on the embedded topology its broker is a leaf of the members'), so go through one of them.

`-api URL` names the node to go through, for running from a machine that is not the node or from either of those; that machine needs no Tier A file: the node seals and opens every value under its own keyring, so with no file at `-config` the command opens and creates nothing locally. `rekey` alone still needs the file and is refused without one, naming `-config`, before it asks the node anything: it judges rows against the `secrets.active_key_id` it reads there (`-dry-run` lists what is sealed under any other key; the pass sends that id for the node to check against its own). The bearer token comes from `CREWLET_API_TOKEN` and nowhere else (`api.auth.tokens` is what a node **accepts**, not a wallet the CLI takes from), and the credential's own identity is recorded as the write's author.

### `crewlet secrets set`

```
crewlet secrets set <NAME> [-value V] [-config PATH] [-api URL]
crewlet secrets set <NAME> <NAME>... [-config PATH] [-api URL] < values.env
```

Stores one encrypted secret under `NAME`, which must be a valid environment-variable name (a name outside the `${VAR}` grammar could never be read back, so it is rejected). Existing names are replaced.

**Several names are one write.** Given more than one name, the command reads one `NAME=value` line per name from stdin (blank lines skipped, a value keeps every `=` after the first) and refuses the whole set, storing nothing, when a given name is missing, repeated, or joined by one it was not: a rotation file meant for another model is refused rather than half applied, and a list read by position could swap two credentials silently. `-value` takes one name only. Through a running node the set is one `PUT /secrets`, which re-activates the revision that reads any of them **once**; a value holding a newline (a PEM key) is stored on its own.

The value comes from **stdin** by default (`echo "$TOKEN" | crewlet secrets set GITLAB_TOKEN_SWE`), or an interactive prompt on a terminal. `-value` is for scripted use, but an argv value is visible in `ps` and lands in shell history, so prefer stdin. A value is at most **64 KiB** on either route ([the size every store keeps](../concepts/secret-store.md#the-design)); a longer one is refused before it is sent or stored, and a pipe is read no further, so pointing one at the wrong file is refused rather than read whole. The row's **source** is the write path's own word and no flag sets it: through a running node it is `api`, whichever command wrote; written directly with the engine stopped it is the command's own: `cli` here, `provision` for a provisioning command's `-secret-store`, and `llm-login` for `crewlet llm login` and `export` (see [Secret Store](../concepts/secret-store.md#the-design)). Through a running node the **author** is the node's to decide: whoever the credential in `CREWLET_API_TOKEN` resolves to, `token:<id>` for a Tier A token or the owner with the token beside them for a machine token; the line printed after the write is what the node recorded, never the shell's user. Written directly it is the shell's user, `$CREWLET_OPERATOR`/`$USER`, with no credential.

After each write the command says which way it went and when an engine reads the value. Through a running node: `written to the fleet's secret store, through <url> (every node reads it)`, and what the node's [re-activation](../concepts/secret-store.md#a-write-through-a-node-re-activates-what-reads-it) did: `The running fleet reads it now: the node re-activated the company as revision <id>, epoch <n>.`; or, for a name nothing the running company reads, that nothing was re-activated; or that it was stored and **not** re-activated, with the node's reason, and that `POST /config/reload` does it once that is fixed. With the engine stopped: `written to the fleet's store, reached through this node's broker on <stream.store_dir> (this node's engine is stopped)` (or `the external broker at <host:port>`), and that this node's engine reads it at start, while a node already running on the same store reads it at its next configuration apply, which `POST /config/reload` to it brings forward.

### `crewlet secrets list`

```
crewlet secrets list [-config PATH] [-api URL]
```

Prints one row per stored secret: name, sealing `key_id`, last-updated timestamp, who wrote it (with the credential beside them where one made the write and names something they do not: `jane.doe (through pat:…)`), and source (`api`, `setup`, `provision`, `cli`, `llm-login`, `models` or `mcp`), marked `(write-only)` for a value no reveal answers, such as an MCP server's header stored from Settings › MCP & tools ([write-only values](../concepts/secret-store.md#write-only-values)). It names first the way it reached the store (the running node, or the broker reached with the engine stopped), so an operator never reads it as the fleet they meant when it was not. **Never** prints a value: the listing drops the envelope and the route has no value field. It never opens a value, so a row sealed under a key this keyring no longer holds (dropped before its rekey finished) still lists, and an operator locked out of that key can still take inventory.

The engine's own key (the identity directory's blind-index key) shares the store and is **counted, never named**, on a line after the table; so, through a running node (which holds the identity estate), are the identity estate's sealed personal values (names, addresses, second factors), which the same keyring seals and a rekey moves. No `crewlet secrets` command reads, writes or removes an engine key: `get`, `set` and `unset` refuse a name in the engine's namespace (`iam/…`) before opening anything, and the store refuses it again, through a node as `403 reserved_name`. See [the secret store](../concepts/secret-store.md#the-engines-own-keys-share-the-bucket-and-never-the-namespace).

### `crewlet secrets unset`

```
crewlet secrets unset <NAME> [-config PATH] [-api URL]
```

Removes the record and says whether one was there; "was not set" is the outcome a cleanup script wanted on its second run, not a failure. Like [a write](#crewlet-secrets-set), it says which store it reached (`written to …`, or `was not set in …`) and when the value stops resolving, since the engine resolves a `${VAR}` through the snapshot of its last apply: through a running node, the running fleet at its next configuration apply; with the engine stopped, this node's engine starts without it and a node already running on the same store stops at its next apply. `POST /config/reload` brings either forward. Afterwards `${NAME}` falls back to the environment.

### `crewlet secrets get`

```
crewlet secrets get <NAME> -reveal [-config PATH] [-api URL]
```

Prints one decrypted value to stdout, with no trailing newline so it pipes. `-reveal` is **required**; without it the command refuses and reads nothing. Through a running node the route (`GET /secrets/{name}`) is gated the same way, by an explicit `?reveal=true`, answers `Cache-Control: no-store`, and logs the access against the operator its guard authenticated. A reveal takes `admin` and a person present, so `CREWLET_API_TOKEN` must hold one of Tier A's tokens: a machine token is refused whatever it carries. With the engine stopped the value is read from the fleet's store directly and no node logs it. Either way the command logs `secret_revealed` with the name and the shell's user on its own log. It is break-glass for recovering a credential the upstream API will never show again, not a scripting interface. A write-only value is refused whichever way the command reaches the store, naming why: through a node as `403 write_only`, with the engine stopped by the store itself. So is a command that resolves its company document and reads one ([write-only values](../concepts/secret-store.md#write-only-values)).

### `crewlet secrets rekey`

```
crewlet secrets rekey [-dry-run] [-config PATH] [-api URL]
```

Re-encrypts every stored secret not already sealed under `secrets.active_key_id`, and prints the names it moved (the engine's own key **counted**, not named). **Through a running node it also re-seals every person's values in the identity estate**: rows derive from the identity log, so the node publishes one record per person still under an old key and prints how many values of how many people it moved (every running node holds the identity estate from boot, company or none). **With the engine stopped** it re-seals every row of the fleet's store (your names and the engine's own `iam/` keys) but not the identity estate's personal values, which only the engine holding the estate re-seals, through its own log: the command says so; run it again through a running node for those. It is the per-record counterpart of [`crewlet config rekey`](#crewlet-config-rekey): run **both** before dropping a retired key from `secrets.keys`, or records still sealed under it become unreadable (for the engine's key and the identity estate, the blind-index key and every person's name and address at once). Left under the old key, and counted by a run through a running node, so keep that key on the ring until none remain:

- An **outstanding invitation**'s address (only a re-issue could carry it again): until each is redeemed or expires, at most a week after issue.
- **Ask Crewlet's conversations**' values (each leaves with its conversation): until each conversation started before the flip is untouched for 30 days or cleared by its owner.
- **The seats' executor sessions**: each leaves with its row, when a later turn of its conversation ends, when its seat's `turn_engine.conversation_session.max_bytes_per_seat` releases it, or once untouched for `turn_engine.conversation_session.retention_days`; one whose key is dropped sooner reads as no session, so its conversation's next turn starts fresh.

Each envelope names its sealing key, so mixed-key states decrypt correctly throughout. `-dry-run` lists what would re-encrypt without decrypting anything, reading the key id off each envelope, and counts the engine's key and, through a running node, the identity estate's values, Ask Crewlet's and the executor sessions' still under another key beside your names.

The store is shared, so run this **once for the fleet**, not per node. A run through a node's API sends the key id it expects (`-config`'s `secrets.active_key_id`, which `-dry-run` also judges against, which is why a rekey needs the Tier A file even through `-api`) and is **refused with a 409** if that node seals under a different `active_key_id`: a silent success would report a rotation the fleet did not make. It aborts rather than half-completing if any record cannot be opened with the keyring in hand, because retiring the old key on a partial pass makes a secret unreadable for ever. It never undoes a write that lands while it runs: each record is re-sealed only at the version it read, so a record rotated meanwhile is judged again on what it holds now, and a deleted record stays deleted. A person whose re-seal nobody could confirm fails the run with what did move; running again moves only what is still under an old key.

---

## `crewlet validate`

```
crewlet validate [<file.yaml>] [-tier auto|company|bootstrap|checks] [-json]
crewlet validate [-config <tier-a.yaml>] [-company <tier-b.yaml>] [-json]
```

Validates a config, or a repository's [checks
file](../concepts/gitlet.md#the-checks-file), and prints a summary, reaching
nothing: no broker, no store, no provider is dialled.

**Two forms.** Name **one file** and it validates that document. Name
**neither** and it validates both tiers together, from `-config` and
`-company` (what a CI step wants), reporting *both* tiers' problems rather
than stopping at the first.

A **leftover positional is refused**, not ignored: Go's flag package stops at
the first non-flag token, so a command that kept going would silently validate
the defaults and print a success line about files it never opened.

Validation is **deep**: it builds the `Organization`, so duplicate handles and
unit keys, bad cron expressions, invalid timezones, two seats declaring one
contact identity, and a knowledge scope with no backend all fail here rather
than at run time. A human seat with **no** contact identity is not among them
(a person who works only through the dashboard has no chat account to name):
validation admits it, and on a company that talks on Mattermost or Slack
prints a warning at its `contact` when it names no account on that workspace.
On the engine's own chat, or no chat, there is no such warning. It reads **no
environment**: Tier B keeps `${VAR}` references verbatim, so a config validates
fully before any secret exists. For the same reason a company with no
`providers.llm` validates, and its summary reports `0 LLM providers`: every
node applies it, and its seats hold their work until a provider is added (see
[A Company With No Model Provider](../concepts/configuration.md#a-company-with-no-model-provider)).

| Flag | Description |
|------|-------------|
| `-tier` | Which document a positional file is: `company`, `bootstrap` or `checks`. `auto` (default) reads the document's **keys**, not its filename. The tiers share no top-level key: `name`/`roles`/`units`/`providers` mean Tier B, `node`/`stream`/`store`/`coordination` mean Tier A; a document with `checks:` and no key of either tier is a checks file (a typo'd key beside `checks:` is refused at its own line by the checks file's reading). A document with none of them, or an equal count of both tiers', is **refused naming this flag** rather than guessed at, since a wrong guess reports every field as invalid. |
| `-json` | Emit a machine-readable result on stdout instead of prose, text as written: a problem naming `token:<id>` says so, never `token:\u003cid\u003e`. |
| `-config` / `-company` | The two-tier form. Ignored when a positional file is given. |

A Tier A document's prose summary names what it resolved to, and a checks
file's names its checks:

```
crewlet.yaml: stream "nats", coordination "embedded-kv", store "…", roles [seats]
.crewlet/checks.yaml: 2 checks (unit, lint)
```

A **checks file** is read strictly by the one parser Gitlet uses: every unknown
key, key written twice, value of the wrong shape and value past its bound is a
problem located at its field and line, as a config document's is: the first
128, and one more problem, at no field, counting the rest ([The checks
file](../concepts/gitlet.md#the-checks-file)). It reaches nothing a repository
holds: a check's secrets are named, never resolved.

With `-json`, the payload is `{"valid": bool, "tier": str, "file": str,
"problems": [...], "warnings": [...], "summary": {...}}`. `summary` is what the
document resolved to: a Tier B file's `company`, `seats` and `llm_providers`; a
Tier A file's `stream`, `coordination`, `store` and `roles`; a checks file's
`checks` (how many it defines) and `names` (theirs, in order); for the
two-tier form the company's three beside `stream` and `coordination`. Each
problem is located and classified, so an editor, CI job, or [AI authoring
loop](../getting-started/ai-authoring.md) can fix everything in one pass:

```json
{
  "valid": false,
  "tier": "company",
  "file": "company.yaml",
  "problems": [
    { "path": "roles[1].llm", "segments": ["roles", 1, "llm"],
      "kind": "unknown_value", "seat": "cto",
      "message": "roles[1].llm: value not in the allowed set: \"nonexistent\" is not a configured provider. Use one of providers.llm's: \"primary\". A key that misses is not an error at run time: the seat falls back to another model and bills against it, so this is the only place the typo can be seen" }
  ],
  "warnings": []
}
```

| Problem field | Meaning |
|---|---|
| `path` | The authored path. Empty only for a failure that belongs to no place in the document, such as a file that is not YAML. |
| `segments` | The path as an array of keys (strings) and list indexes (numbers); a map key can hold a dot, so read these rather than splitting `path`. `null` when `path` is empty. |
| `kind` | `missing`, `out_of_range`, `conflict`, `shape`, `unknown_field`, `unknown_value`, or `invalid` for anything this build does not classify: a closed set with a fallback, so a consumer never receives an empty string. |
| `message` | The full line, exactly as the prose output prints it. |
| `seat` / `unit` | The handle of the seat, or name of the unit, it is about. Omitted when neither. |
| `line` | The line in the file, for a failure the parser found (unknown key, wrong shape). Omitted otherwise. |
| `range` | For a value outside its setting's range, that range: `{min, min_exclusive, max}`, the last two omitted when false or unbounded. Omitted otherwise. |
| `said` | The problem in a person's words, without a path, for a rule that writes them. Omitted otherwise. |

A rule several seats or units break together, such as two seats sharing a
name, is one message and one problem beside each, so `problems` can hold more
entries than the prose has lines; the prose prints that message once, led by
every path it applies to.

Each warning is `{"kind", "ref", "path", "segments", "seat", "unit", "from",
"to", "message"}`, every field always present, with `title`, `person` and
`item` (what it is about in a few words, the same said for a person, and the
one thing within its seat or unit) and an advisory's `class` where set, as a
[write's answer](api-endpoints.md#what-a-write-answers) carries them. It is one
of two kinds here:

- **`dangling_reference`**: a reference that resolves to nothing: a unit
  `lead`, a root seat's `unit`, a `manages` entry. `ref` names which (`lead`,
  `unit` or `manages`), and `from` and `to` are what holds it and what it names.
- **`advisory`**: a valid setting with a consequence: a declined fsync, a
  backup nobody owns, a human seat nobody can message, a token ceiling another
  one leaves idle, a repository whose default branch no protection rule
  covers. Located at the field to change; `ref`, `from` and `to` are empty. The
  only kind a Tier A document raises.

The third kind a `/config` answer can carry, `admission` (a rule a stored
revision breaks), cannot appear here: a document validated here gets a problem
instead. The engine runs a document with either kind, so a warning never fails
validation; prose prints each on its own line led by its kind: `warning (dangling reference): …` or `warning (advisory): …`. A company document that
parses is checked for dangling references whether or not it is valid;
advisories are reported only for a document that validates, since a warning
beside a refusal is noise. Both lists are always arrays.

Exit code is `0` when valid and `1` otherwise, **in both output modes**
(a `-json` run printing `{"valid": false}` and exiting zero would pass every CI gate built on `crewlet validate x.yaml -json || exit 1`).
In `-json` mode nothing is echoed on stderr, since the payload already carries
every problem.

---

## `crewlet migrate`

Applies pending schema migrations. Every process that opens the store migrates
it, so this is never *required*; it is a way to migrate without starting a node.

```bash
crewlet migrate                          # uses ./crewlet.yaml
crewlet migrate /etc/crewlet/crewlet.yaml
crewlet migrate -check                   # report pending work, apply nothing
```

| Flag | Description |
|------|-------------|
| `config` | Tier A YAML (positional, or `-config`; default `./crewlet.yaml`). Name it **once**: a second positional, or a positional alongside `-config`, is refused rather than resolved. |
| `-check` | List pending migrations and exit **1** if there are any; applies nothing. For a deploy gate, which must not exit 0 on pending work. |

**A node without `data` has nothing to migrate.** Its store is `store.scratch`,
created fresh at the binary's own schema every time `crewlet run` starts, so
`migrate` refuses it. The offline `config` and `search eval` commands refuse a
scratch store too, since what they wrote would be gone at the next boot.
`secrets`, and every command that resolves the company's `${VAR}`s, refuses one
for another reason: a node without `data` holds none of the fleet's store (on
the embedded topology its broker is a leaf of the members'). Name a running
node that holds data with `-api` instead.

Rolling out N nodes at once means N processes racing to apply the same files.
That is safe (one transaction per file, version row written inside it, so a
file is fully applied and recorded or neither), but a failure mid-rollout is a
fleet in two schema states. Migrating once, deliberately, before anything
starts makes the outcome one thing that worked or did not.

Applying is done by **opening the store**, not a second code path, so the
migrator cannot disagree with the engine about what "applied" means. `-check`
is the exception: it reads `schema_migrations` and creates nothing. A database
with no `schema_migrations` table has applied nothing, as a fresh install does;
that is not an error.

---

## `crewlet budgets`

Token-budget usage lives in the fleet's [coordination store](../concepts/coordination.md#token-budgets-are-windows):
one counter per scope for the whole company, with a slot per calendar window
(day, ISO week, month, on the company's clock), surviving restarts. Per-node
counters would turn an org cap of 500k into N × 500k.

**This command talks to a running node**, not a file: on the default topology
the coordination store is the engine's embedded broker, and a second broker on
its `stream.store_dir` is refused, naming the holding process (two writers
would corrupt it).

```bash
crewlet budgets show                       # usage per scope
```

| Flag | Default | What it does |
|---|---|---|
| `-api URL` | the `api` block of the config named on the command line | The running node's base URL. A wildcard bind (`0.0.0.0`, `::`) becomes the loopback address, since a wildcard can't be dialled |

**The credential is `CREWLET_API_TOKEN` and nothing else**, on every command
that talks to a running node (each names it with `-api`). Every guarded route
needs one, reads included. It is never a flag (it would land in shell history,
`ps` and CI logs), so `-token` is refused as an unknown flag. It is never read
from the config's `api.auth.tokens`, which is what the node *accepts*; a write
authored by the first entry landed under a name nobody chose. The one command
that reads no token is `iam token -login`, which mints your own and signs in as
you for that request. A `401` therefore has one thing to check: the value in
`CREWLET_API_TOKEN`. The config is read for the node's address alone; with
`-api` none is read, so a laptop or CI job with no Tier A file reaches a node.

The **ceilings** are not stored here: they are the company document's
`token_budget`s (the company's and each seat's), so every process derives the
same numbers. Only usage is shared.

`show` names the company clock, then prints **one row per window**: the
company's day, week and month, then each seat's.

```
Windows on the company clock: Europe/Berlin

SCOPE  PERIOD  WINDOW      USED     LIMIT      STATE     RESETS AT             LAST REFUSED
org    day     2026-09-23  2710450  3000000    near      2026-09-23T22:00:00Z  -
org    week    2026-W39    9120045  unlimited  ok        2026-09-27T22:00:00Z  -
org    month   2026-09     31004188 unlimited  ok        2026-09-30T22:00:00Z  -
eng    day     2026-09-23  102120   100000     refusing  2026-09-23T22:00:00Z  2026-09-23T07:29:51Z
…
```

A window no ceiling caps still shows its spend, with `LIMIT` `unlimited`.
`STATE` is the engine's own judgement, shown the same on every surface:
`refusing` when no charge fits (every window that refused a round reaches it,
since the refused round is counted), `near` at nine tenths of the ceiling, `ok`
otherwise. `LAST REFUSED` is when the window last turned a call away: a round
whose charge it refused, or work turned away unsent because the window was
already full (a turn's next call, a parked delivery, a person's question, a
reflection pass). It prints only while `STATE` is `refusing`, else `-`: a window
whose ceiling was raised after refusing reads `ok` or `near` with `-`, though it
keeps the stamp (`refused_at` on `GET /budgets`) until its next admitted charge
clears it. The window turning over clears it too. A refused round is counted
like any other because the vendor billed it, so `USED` can pass `LIMIT` by that
round (the `eng` day above refused a 3 000-token round at 99 120), and every
later charge is refused against that figure.

`show` refuses rather than printing zeros when the node could not read the
counter (`durable: false` on the query surface): zeros would show 0% of budget
exactly when nothing is known. Seats with no ceiling and no spend in any window
are left out, since permanent zero rows bury the seats that matter.

**There is no `reset`.** A window's allowance returns when it turns over (local
midnight, Monday, the 1st), rolled inside the charge that crosses the boundary.
Room before then comes from raising the ceiling through `/config` (a seat's with
`PUT /config/roles/{handle}`), a write with an author; a hand-zeroed counter
records nobody.

---

## `crewlet backup`

```
crewlet backup [<config.yaml>] -dir <absolute path> [-api URL] [-wait DURATION]
```

Copies what a running node holds (its own store file, the replicated estate's
file on a node with the `data` role, and every JetStream stream and coordination
bucket) into one directory, and verifies each store copy before calling it a
backup.

**It writes to the engine's host, not yours.** `-dir` is resolved where the node
runs; nothing is downloaded. A relative path is refused, and so is a destination
that already holds something (no merging), so each run needs its own directory.

Like [`budgets`](#crewlet-budgets), it goes through the node, for a doubled
reason. The store is locked to the engine's process and the driver supports no
second process on a database file; copying it anyway is torn, since committed
data lives in the file and its `-wal` together. And on the default topology the
broker is embedded and **binds no socket**, so the `nats` CLI has no address.
Only the engine reaches both.

The report names what it captured per estate: the node's own store and the
replicated estate beside it. Every backup is a data node's: only a node with
`ingress` serves the route, `ingress` requires `data`, and a node without `data`
keeps nothing of its own (its store is scratch; its events are in the data node
in custody). On the embedded topology the fleet's streams come too, snapshotted
from the node's own broker, of which a data node is always a member. A node that
dialled an external NATS cluster copies its store files alone and says so,
naming the cluster as where the stream half is backed up.

**The company's files come too**, as an `objects` line. On the default `nats`
object store they are in the `OBJ_crewlet_files` stream, snapshotted after the
rest; the line counts the objects the backup owes that the stream holds, asked
thirty-two at a time (lost ones listed beneath). On `s3` each owed object is read
from the bucket into `objects/`, thirty-two at a time, checked against its
file's size and SHA-256 as written; the line gives count and size. Owed is every
object a live row of the copy names and every object a record the restore
replays from the copy names; a file written while the backup ran counts as
"(N written during the copy)". A file whose object the store says it does not
hold, or holds wrong, does not fail the backup: it is listed beneath as
`PROJECT/path` and the object's key, to restore from an earlier backup or upload
again. The first object the store could not answer about at all fails the
backup, and nothing more is asked.

`-wait` (default 12 hours) bounds how long the command waits for the answer, not
the copy: the engine finishes what it started, so an expired wait would leave a
good backup called a failure. It is set far past any plausible duration.

One backup runs at a time on a node: asked while another runs, the node answers
`409 backup_running`, writes nothing, and the command says so. Refusals caused
by the fleet's state print the node's hint:

- `409 record_unreadable`: a log the backup replays holds a newer build's record; take it from a node running that build.
- `409 record_unverified`: a record signed under a key this node does not hold; add the key to its `secrets.keys` and restart, or take it from a node that holds it.
- `409 log_recreated`: this node's copy is keyed to a log since recreated; take it once the node has re-keyed, or from one that has.
- `503 log_gap` or `503
objects_unreachable` (take it again: the node's `Retry-After` is one second
  for `log_gap`, since the next copy stands past the gap at once, and four for
  `objects_unreachable`, the broker's own election timeout).

See [Backups & Restore](../guides/backup.md) for the directory's contents, what
the copy is a copy *of*, and how a restore uses it.

---

## `crewlet worklet`

```
crewlet worklet purge <item> -reason TEXT -confirm <item-key>
    [-op-id ID] [<config.yaml>] [-api URL]
```

The gestures on work items that belong on a **shell**. Everything else is done
by seats through their tools, or by a person through the [write surface](api-endpoints.md#the-human-write-surface)
the dashboard uses. This one is the operation no seat may perform, run by an
operator acting on an erasure request, through that surface's own route
`POST /worklet/items/{key}/purge`, so the same authority table decides it:
`admin`, never a seat.

### `crewlet worklet purge`

Destroys a task and every row it produced: its own row, comments, body
revisions, checklist, field values, watchers, relations, dependencies, every
inbound reference and the key directory row that made it resolvable. A marker is
written in their place and every later record about that task is dropped for
ever, which stops a redelivery months later resurrecting any of it.

`remove` hides a task and `delete` stops later records about it; neither takes
the body out of a single database. An erasure request, or a credential pasted
into a task description, needs the rows gone.

`<item>` is the task's key or id. **The confirmation is the task's key**, checked
against the task `<item>` resolves to; a mismatch destroys nothing (the id may
be what you typed, so repeating it confirms nothing). **There is no `-project`**:
the task's stored row says which project the record is filed under, while a
key's prefix names only where it was first filed. **The reason is required**: it
is the only thing that survives, the whole account of what was at that key.

**Its children are moved, not destroyed.** Each direct child re-parents onto the
purged task's parent, or becomes a root if the purged task was one. Destroying
the subtree would destroy unconfirmed work; leaving it would leave children
pointing at an id that resolves to nothing.

The outcome is three-valued, like every write here:

- `applied`: the rows are gone on this node and the record is durable.
- `pending`: the record is durable, this node has not applied it yet. **Do not run it again**: a second gesture appends a second purge.
- `unknown`: exits non-zero naming the operation id. Pass that id back in `-op-id`, sent as the request's `Idempotency-Key`, so the retry is the same operation and appends nothing. A retry whose first purge landed answers `applied`, at the position that purge landed at, rather than finding the task gone.

The command mints the id **before** it asks and waits thirty seconds (past the
three five-second waits a write can make), so a purge the node never answered,
which may have landed, still prints the `-op-id` to retry with. Pass the id
exactly as printed: it carries the instant it was minted, and a node whose
operation ledger may have lost the first purge's row since (the ledger's
thirty-day sweep) judges the retry by it, answering `unknown` again rather than
purging twice. An id the engine did not print is refused (`op_id_invalid`), as
is one altered on the way back (trimmed, spaced or over 128 bytes), since the
broker would carry it as a different id.

It does **not** reach a node that is offline or evicted: that keeps its copy
until it replays, adopts a snapshot, is replaced or is destroyed. No duration can
be stated; `crewlet retention status` names those nodes.

## `crewlet objects`

```
crewlet objects status [-json] [<config.yaml>] [-api URL]
crewlet objects migrate -to s3 -target <path> [-final] [-wait D] [-json] [<config.yaml>] [-api URL]
```

Where the company's files are kept, and what the
[object store's](../concepts/object-store.md) collector last found. It talks to
a running node, for `backup`'s reason: the collector's report is a coordination
store record, held by the embedded broker that binds no socket. `status` reads
the fleet view's `objects` block ([`GET /fleet`](api-endpoints.md#get-fleet)),
which every node answers from that one record.

### `crewlet objects status`

It prints the store the files are in (the fleet's NATS bucket `OBJ_crewlet_files`,
or an S3 bucket with endpoint, bucket and prefix), the node that ran the last
passes of the `object-collector` duty, and one line per pass:

- **Last collection**: objects listed in the store, objects deleted for being older than a day with no row naming them, and uploads begun over a day ago and never finished, which it abandoned. A collection that stopped judging says why (`stopped judging: …`: the node's view of the estate was incomplete, so an unreadable row might name any object) beside what it had deleted before stopping; one that stopped says what stopped it; one that did not finish says `incomplete`. A sweep of unfinished uploads the store refused (on S3, an identity without `s3:ListBucketMultipartUploads` or `s3:AbortMultipartUpload`) is a line of its own beneath.
- **Last audit**: from the last audit to run to its end: files the company's rows name, files the store does not hold (**missing**), files held at the wrong size or digest or as nothing readable (**damaged**), and files it failed twice to answer for while answering for the rest (**unanswered**). An audit that failed after it is said beneath, never in its place. When any file cannot be read it lists them (the file, which of the three was found, its object's key; the first hundred by name, and how many more) to restore from a [backup](../guides/backup.md) or upload again.

Before the collector has finished a pass (on a new fleet, in the minute or so
before a data node first claims the duty) it says so. `-json` prints the block
as JSON for a script: every field and number as the node answered, text as
written, keys sorted. Exit is non-zero when the node cannot say (its
coordination store did not answer, it runs no object store, or it answered a
state this build does not know) and zero otherwise, unreadable files included: a
store that lost bytes is an answer, and the [`objects_missing`](alarms.md) alarm
is what pages for it.

### `crewlet objects migrate`

Moves the company's files from the fleet's NATS bucket to an S3-compatible
bucket ([Moving the files to a
bucket](../concepts/object-store.md#moving-the-files-to-a-bucket) is the whole
procedure). It asks a **data node**
([`POST /objects/migrate`](api-endpoints.md#post-objectsmigrate)), because only
the engine reads the objects, the records naming them and the coordination
record the move ends with.

- `-to s3` names where the files go; an S3-compatible bucket is the one other store.
- `-target` is an absolute path **on the engine's host** to the Tier A file every node will run with after the move. Its `store.objects` block names the bucket, and its `${VAR}` references resolve from the engine's environment, so the bucket's credential never crosses the API.
- Without `-final` it runs a **copy pass** on a live fleet: every object a file names that the bucket lacks is read from the fleet's store, checked against its row and written to the bucket, as is every object a [derived](../concepts/object-store.md#live-retired-and-derived-references) row names (which the engine would otherwise rebuild on every node). Run it as often as you like; it copies only what is missing.
- `-final` runs the pass that moves the fleet. Every node must be running `crewlet run -mode maintenance`, so no record naming a new file can be written meanwhile. It copies what is left, seals the NATS bucket so an unrestarted node can store nothing there, and moves the fleet's record to the bucket. It refuses while the node cannot read every row that may name a file (a data node running a state log this build does not, or this node's copy of the estate migrated by a newer build; [What a build cannot see](../concepts/object-store.md#what-a-build-cannot-see)): run it from a build that knows every log the fleet runs.

It prints what the pass found: objects the files name, how many the bucket
already held and how many were copied, the same for derived objects where there
are any, whether the bucket offers direct uploads, any file the fleet's store
could not give back (no copy can restore those), how many derived objects it
could not give back (the engine rebuilds them, so nothing is lost), and what to
do next. `-wait` (six hours) is how long it waits for the answer; the node
finishes the pass either way, and running the command again reports where the
copy stands. `-json` prints the report as JSON, as for `status`. The token takes
`admin`.

## `crewlet gitlet`

```
crewlet gitlet status [<config.yaml>] [-api URL] [-repo NAME] [-pushes N] [-json]
crewlet gitlet verify [<config.yaml>] [-api URL] [-repo NAME]... [-repair] [-json]
crewlet gitlet export <name> <file.bundle> [<config.yaml>] [-api URL] [-git PATH]
crewlet gitlet import <source-url> <name> [<config.yaml>] [-api URL] -owner unit:KEY|seat:HANDLE
                    [-credential VAR] [-mirror] [-op-id ID]
crewlet gitlet import <source-url> <name> -local [<config.yaml>] [-api URL] [-git PATH] [-dry-run]
crewlet gitlet purge-unreachable <name> [<config.yaml>] [-api URL] [-op-id ID]
crewlet gitlet credential [-token-env NAME] get|store|erase
```

[Gitlet](../concepts/gitlet.md) is the engine's own git host. `status`,
`verify`, `import` and `purge-unreachable` ask a **running node**, as whoever the
token resolves to: `status` and `verify` through its `/gitlet/` routes
([API Endpoints](api-endpoints.md#gitlet-git-and-gitlet)), the other two through
the act transport. Repositories are rows the data nodes keep, and a Gitlet node's
cache is files only its own process touches, so nothing here opens either. Like
every node client they take the token from `$CREWLET_API_TOKEN` alone and find
the node from the Tier A file's `api` block; `-api` overrides it, and with `-api`
no Tier A file is read. `export`, `import -local` and `credential` are
**client-side**: your own git, with your own environment, talks to the node's
git routes as anyone's git does. A repository's git URL is
`<the node's API base>/git/<name>.git`. `status`, `import` and
`purge-unreachable` need the token to carry the Gitlet permission (`gitlet`), as
does the git that `export`, `import -local` and `credential` serve
([ADR-0110](../../adr/0110-reaching-a-repository-takes-the-gitlet-permission.md)):
without it the node refuses before any repository is read. `verify` is the
deployment's own control and takes `admin`.

`crewlet gitlet hook` is the engine's own: git starts it inside every push. Run
by hand it says so and exits 2.

### `crewlet gitlet status`

With no `-repo`: every importing, active and archived repository, read at the
node's default level (`stale`), as many a page as the node serves (500), every
page after the first at or past the first one's position, and on stderr the
position the listing was read at, so two runs can be compared:

```
NAME        STATUS    OWNER               BRANCH  HEAD     REFS  PACKS  SIZE      LAST PUSH
infra       archived  seat:platform-lead  main    77c0d2e  3     9      12.4 MiB  2026-09-30 08:11 UTC
nimbus-web  active    unit:engineering    main    3f2a91c  14    212    1.9 GiB   2026-10-10 14:02 UTC
read at CREWLET_CODE_LOG@0:4127
```

`HEAD` is the default branch's commit, `-` while it has none; `SIZE` is what its
live packs hold. A company that declares none says `no repositories:
code.repos declares none`, never an empty table.

With `-repo NAME`: that repository in detail and its newest pushes (`-pushes N`,
10 by default, `0` for none). The repository is **read at `linearizable`** (as
it is now), and every page of its history at or past that position, which the
node serves with no barrier of its own, so the command costs the code log one
barrier however many pages it reads, and asks for no more pushes than it prints:

```
web: active, owned by unit:engineering, default branch main
  managed by:   code.repos
  description:  The customer-facing web app.
  visibility:   unit (only the seats the chart relates to its owner read it)
  search:       indexed (read from an index of its default branch, by a search across the company's repositories as well as one naming it)
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

`BY` is the credential that pushed: `pat:<credential id>` for a machine token,
`token:<id>` for a Tier A token, then in parentheses the human seat its
principal holds (a person's own, or the one the identity directory binds a token
to). A seat's own push shows the seat's handle and, in parentheses, the turn that
made it and the work item it was charged to (`docs-writer (turn <turn id>, native ENG-12)`);
for a push the seat's coding box made, led by the box's harness and launch
(`swe (claude-code run <launch id>, turn <turn id>, native ENG-42)`), all as the
engine stamped them at push time. Each ref's result is `ok`, `noop` or `ng`,
with git's `..` for a fast forward, `...` for a forced move, and an `ng`'s
reason. `limits` are the serving node's `code.limits`. `managed by` is who
changes the repository: `code.repos` for a declared one, or the code tools for
one a [tool made](../concepts/gitlet.md#repositories-made-by-tools) (a project,
or a seat's scratch repository, shown with when it was last active, the clock
its idle expiry reads). A deleted repository's header says when it was deleted
and how it comes back until `code.deleted_retention_days` have passed: by
declaring it again in `code.repos`, or for a tool-made one by a restore through
the code tools. A repository asked for by a name a rename left redirecting is
printed under its current name, with a `renamed` line saying until when the old
name redirects.

`-json` prints the node's answers, indented, one JSON document a page (the
repository's first with `-repo`). The command prints what it holds while
reading: a page at a time with `-json`, otherwise each repository's or push's
row of text. A repository nobody declared is refused in the node's words (`no repository named "web" is declared in
code.repos`), and a company that hosts no code says `code.backend is not gitlet`;
either exits non-zero.

### `crewlet gitlet verify`

Checks **this node's** copy of each repository against its rows: every pack's
bytes against the digest its row names, the refs against what was written, and
git's own connectivity check. With `-repair` it repairs what it finds and checks
again.

- Where everything found is the copy's **own directory out of place** (a file the engine does not write there, a fixed file edited or replaced by a link, a pack no row names, a shape the node refuses to run git in), the copy is put right **in place**: every pack, the multi-pack-index, its bitmap and the commit-graph kept, nothing downloaded. A pack no row names leaves as the copy moves into its next directory, taking the multi-pack-index and commit-graph built over it, still with nothing downloaded ([When packs leave a
copy](../concepts/gitlet.md#the-cache-on-a-gitlet-node)).
- A pack missing or damaged, or an index failing its checksum, is **rebuilt** into a new copy (every pack found whole is linked, not downloaded again), as is whatever a repair in place leaves.

Every Gitlet node keeps its own cache, so **verify each** (`-api` names the
node); with requests routed by repository, a repository is cached on one or two
of them. A verify takes **`admin`**, so the token in `CREWLET_API_TOKEN` must
carry it: any other is refused naming it, before anything is checked.

With no `-repo`: every importing, active and archived repository, listed at
`linearizable` so one declared a moment ago is not missed. `-repo NAME`, as often
as you like, verifies those; a name nobody declared is refused in the node's
words, naming `code.repos`. Each is verified in turn **at the position the
listing was read at** (`min_position`): the node brings its copy there with no
barrier of its own (a check at the log's head appends one), so a sweep costs the
code log one barrier rather than one per repository:

```
web: ok (212 packs, 41 refs)
docs: not held on this node — nothing here to check (verify the Gitlet nodes that serve it)
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
writes them again (minutes to hours for a large repository, its clones slower
meanwhile), which is why nothing a repair in place can end is rebuilt.

Damage the node had **already repaired** when the check began (its sync before
the check held a file to its checksum and dropped or refetched it in place) is
told first and leaves the copy clean: it is neither a problem nor a reason to
`-repair`, though a disk that damaged one file may hold others damaged:

```
web: the sync before the check found damage on disk and repaired it in place:
  the sync before the check found objects/pack/multi-pack-index-3f2a….bitmap failing its checksum and dropped it to be written again
web: ok (212 packs, 41 refs)
```

A repository the node holds **no copy of** is passed over: nothing there can be
damaged, and the node builds no copy just to check it. A rebuild that leaves a
problem names where it is (the object store or the rows), since a copy rebuilt
from them can be no better.

**The check is the node's operation, not the request's.** It reads every byte of
the repository (about nine seconds a gigabyte, nearly all git's connectivity
check), so the node runs it on its own time and a proxy's read timeout ends
nothing. A check ending within a quarter of a second (a small repository's) is
answered in the request; otherwise the command reads it a quarter of a second
later and then at doubling waits up to five seconds, until it ends. A refusal
the node says clears by waiting (its verify slot or the check's memory held, a
node stopping) is retried after the node's `Retry-After`, for up to five
minutes, then reported. While checking, the node says when it will end the check
itself, a bound from what the copy holds (about 1.1 seconds a mebibyte), and the
command waits by it. A node still checking well past that bound, or silent for
half a minute, is given up on, with how to read the outcome later; asking again
hands back a check still running. A node that serves no verify at all is said
once, naming `-api`, and the sweep stops: the address is wrong, not a repository.

**Exits non-zero when any copy is not clean**, after verifying every one (a
problem found, a check that failed, a name nobody declared), and zero when all
are clean or not held: the hook for a cron on each Gitlet node. `-json` prints
each verification as the node answered it on stdout, and its words on stderr (a
company that declares none among them).

### `crewlet gitlet export`

Writes one repository, every ref and every object behind them, to a [git
bundle](https://git-scm.com/docs/git-bundle) on this machine:

```
$ crewlet gitlet export web web.bundle
exported web: 42 refs, 1.9 GiB to web.bundle
$ git clone web.bundle web        # restores it, anywhere
```

It runs **your** git, with your environment and configuration: the first on
`PATH`, or `-git PATH` (a name is looked up on `PATH`, a relative path taken from
where you run the command). It must be 2.40 or newer, the client floor
`gitproc.ClientMinVersion`, below which it is refused before anything runs, as
`gitproc: <path> is git
<version>, and a client of Gitlet needs git 2.40.0 or newer
(gitproc.ClientMinVersion, the client floor): install a newer git — on macOS,
Homebrew's: Apple's bundled git is 2.39; or name one with -git`. It runs a `git clone
--mirror` through the node's git routes, then bundles the mirror, naming the
mirror to every command (`--git-dir`), so `safe.bareRepository=explicit` in your
configuration refuses none of them. Your token reaches git only through
`crewlet gitlet credential`, in the environment of that one clone and never on a
command line, and only for the node's own URL: every credential helper your
configuration lists is set aside for that clone (a stored password for the same
host is never sent instead), and a host a redirect sends git to is offered
nothing. The bundle is written as `<file.bundle>.partial` and **checked whole
before it is renamed into place**: its header must be a bundle's with no
prerequisite, and its pack is read start to end and hashed against git's
trailer. A pack cut short or with a byte changed (which `git bundle verify`,
reading only the header, calls okay, and a clone refuses) is refused with
nothing left at the path. An interrupted export leaves nothing that looks
finished, and an interrupt during the check stops it at its next read. The
mirror is made in a directory **beside the bundle** and removed when the export
ends, so the bundle's volume needs room for the repository twice while it runs.
A repository with no refs yet, or a deleted one, is refused with nothing written.

### `crewlet gitlet import`

Creates a project repository and has Gitlet fill it from a repository at another
host, once or for good as a **pull mirror** (`-mirror`). It is the
`code_import_repo` tool your assistant and the dashboard's **Import a
repository** call, over the same route
([`POST /operator/act/{tool}`](api-endpoints.md#operatoract--the-dashboards-write-surface)),
so the create is one record however it was asked.

```
$ crewlet gitlet import https://github.com/acme/web.git web -owner unit:eng -credential GITHUB_TOKEN
importing https://github.com/acme/web.git into web — importing, owned by unit:eng (at CREWLET_CODE_LOG@1:4821)
  its home Gitlet node fetches the source now; `crewlet gitlet status -repo web` shows it importing until the fetch has landed
```

**The node fetches, not this machine.** The repository's home Gitlet node
fetches the source's branches and tags over HTTPS (every address the host
resolves to must be public unless Tier A's `gitlet.import_allow_private` says
otherwise; redirects are not followed) and lands them in chunks, packs before
the refs that need them, so a history of any size goes in whatever
`code.limits.max_push_bytes` says. The repository is `importing` until the last
chunk lands, taking no push meanwhile; a restarted home fetches again from the
refs already landed. A blob past `code.limits.max_blob_bytes`, or a history past
`code.limits.repo_max_bytes`, ends the import early: the repository turns
`active` holding what landed, and the import is not retried. `crewlet gitlet status -repo <name>` shows `importing` until the import ends, and an `import:`
line naming its source and how many chunks have landed:
`still running`, `done on` when it landed whole, or `ended short on` with the
sentence it ended on. A
repository's mirrors print as `mirror from:` (the source it follows, what it
fetches with, its last sync) and `mirror to:` (the target, as which seat, and
which refs).

- `-owner` is required: the project's owner, `unit:<key>` or `seat:<handle>`, as `code_create_repo` takes one.
- `-credential VAR` names the variable holding the source's token, in upper case such as `GITHUB_TOKEN`, which the node resolves from the [secret store](../concepts/secret-store.md) and then its environment when it dials (`crewlet secrets set GITHUB_TOKEN` stores one). It must be a variable the company's [`code.remote_credentials`](../getting-started/configuration.md#coderemote_credentials) declares for the source's host (an administrator's to declare), or the node refuses the import `not_permitted`, naming the host and the variables declared for it: Gitlet sends no other value it can resolve to a host you chose. The repository records the reference `${GITHUB_TOKEN}`, never the token, and a token given in its place is refused before anything is sent. Left out, the source is fetched anonymously.
- `-mirror` makes the repository a pull mirror: imported first, then moved only by its home's syncs from the source; every push to it is refused.

**A person's alone.** `$CREWLET_API_TOKEN` must be your own token (`crewlet
iam token -login` mints one), bound to the human seat you hold: an import has
Gitlet dial another host for the company, so a token bound to nobody is refused,
naming why, and so is a seat's tool call.

**A retry is the same create.** The command mints an operation id and sends it as
the `Idempotency-Key`; the repository's id derives from it. When the outcome is
unknown or there is no answer, it prints the id, and `-op-id <id>` resends it,
answered as the first create rather than refused for a name already taken.

A source the node cannot reach (a forge on your own network, behind your VPN or
your SSH key) is imported from this machine instead, with `-local`, into a
repository that exists.

### `crewlet gitlet import -local`

Pushes another host's repository (anything your git clones: HTTPS or SSH URL, another server, a local path) into one Gitlet serves, **as you**: every step is an ordinary push by the person your token acts as, held to every rule a push is (`code.limits`, protections, the blob limit, git's object checks), and the push history names you for each. The engine dials nothing: your git reaches the source with your own credentials, proxy and network.

```
$ crewlet gitlet import https://github.com/acme/web.git web -local
skipping 412 refs outside refs/heads/ and refs/tags/ (refs/pull/*): a forge's, not the repository's
cloning https://github.com/acme/web.git into /tmp/crewlet-import-2861
cloned https://github.com/acme/web.git: 63 refs (4 branches, 59 tags), 2.1 GiB packed
importing 63 refs in steps of at most 1.0 GiB (code.limits.max_push_bytes), each measured before it is pushed
pushing step 1: refs/heads/main → 3f2a91c (1009.2 MiB)
imported step 1: refs/heads/main → 3f2a91c (1009.2 MiB)
pushing step 2: refs/heads/main → 9e8d7c6 (1016.7 MiB)
…
imported step 4: 62 refs, refs/heads/develop … refs/tags/v3.2.0 (48.0 MiB)
imported web: 63 refs, 3.4 GiB sent in 4 steps, 3.4 GiB stored with the packs' indexes
```

In order:

1. **The repository.** It must exist and be active (declared in `code.repos` or made by a tool; read at `linearizable`, so one declared a moment ago is found); its `code.limits` bind the pushes. An archived or deleted repository is refused saying how to declare it, an undeclared name is refused naming `code.repos`, and a company without `code.backend: gitlet` is refused saying so. The import asks the node who the token is (`GET /viewer`): every push's record names that person, so it counts toward what one push may carry (step 6).
2. **Before anything is downloaded**, your git (2.40 or newer, the client floor, as `export`; the first on `PATH`, or `-git PATH`) reads the source's refs, and the import refuses, naming them (rename them in the source): a ref Gitlet takes no push of (git's ref rules, at most 255 bytes of UTF-8), a branch Gitlet keeps for its seats (`refs/heads/seats`, or `refs/heads/seats/<name>` as a branch itself; [who may read and push](../concepts/gitlet.md#who-may-read-and-push)), and more refs than one repository holds (100 000). Where the source wants a password no credential helper holds, or ssh a host key or passphrase, your git **asks you on the terminal**, here and in the clone; with no terminal (CI) it fails and the import says so. It then dry-runs a push's first request, so a credential bound to nobody, a fetch-only one, or a node taking no push here is refused now, in git's words, not after the download.
3. **The clone**: a bare clone of the source's branches and tags into the system temporary directory. Set `TMPDIR` to a volume with room for the source plus two steps' packs (step 6: up to three times `code.limits.max_push_bytes`). Refs outside `refs/heads/` and `refs/tags/` (`refs/pull/*`, `refs/merge-requests/*`, notes, remotes) are skipped and counted: Gitlet takes no push of them and history only they reach is never downloaded.
4. **Every refusal the source shows, before any push**, listed together: a shallow source (import from a full clone), a SHA-256 source (Gitlet keeps SHA-1), every blob past `code.limits.max_blob_bytes` (by id, path and size, anywhere in history; large binaries belong in Worklet's project files, and a history rewritten without them imports), and a source whose history is past `code.limits.repo_max_bytes` already as **one pack** (raise it, or import less): an import stores more than that (step 6).
5. **What the repository already holds.** A ref at the source's value is done. A branch at an earlier commit of the source's branch is an import that stopped part way (or a source that moved on) and continues through the commits descending from it, so every step is a fast-forward even where that commit reached the branch through a merge. A ref at any other value is refused, naming the first such ref in the source's order, both values, and that this is not a resumed import of the same history. A ref the repository holds that the source lacks is left and named.
6. **The steps.** The default branch first (the source's branch named by `default_branch` in `code.repos`, else the source's own default under its own name, said, so you can push the repository's default afterwards), advanced along its first-parent history, each step a fast-forward of the branch itself, so no temporary ref exists. Then every other branch and tag in name order, in batches of at most 1 000 refs (fewer where refs are long and spelled long in the code log: `&`, `<` and `>` take six bytes each there), each batch holding only what one push's record has room for, as the person the token acts as, which the node refuses past before anything runs ([Limits](../concepts/gitlet.md#limits)). Every step is **sent whole** (`--no-thin`) and **measured before it is pushed**: its pack is built once, against what the repository will hold and under your pack settings, in a repository of the step's own in the working directory, and pushed from there as a copy of that very pack (`pack.window=0`, no delta search of `git push`'s own), so what is measured is what is sent. A pack built twice differs (delta search splits work across threads differently each run; one range came to 895 KiB and 1 057 KiB). A step is held to `code.limits.max_push_bytes` for what it sends, to the 1.25 GiB one push may store with its indexes, and, added to what the repository holds and earlier steps stored, to `code.limits.repo_max_bytes`. A history stored as a forge stores it (newest version of each file whole, older ones as deltas) packs a step to far more than the source's pack holds of it, so a step past what it may carry is cut shorter before anything is sent, and the summary says how many were. A branch no batch can carry is stepped along its own history. A **merge** whose other side is more than one step (a long-lived branch merged back, a subtree, an unrelated history) has that side stepped first, to the merged-in commit and no further, on a branch of the source still to be pushed that reaches it (the branch whose history it is, else any that reaches it, such as one created after the merge). That branch is taken out of its turn, every step a fast-forward, and goes on to its own value in its turn; a branch the repository holds carries it only where held at an earlier commit of that history. The merge, measured once, then goes in a step of what is left. Where no other branch can carry it, the import is refused naming the merge, the merged-in commit and what its history weighs, with the remedy: a branch on that commit in a mirror of the source (`git clone --mirror`, then `git branch`), imported from the mirror. One commit, or one tag no branch reaches, that alone packs past the limit cannot be split and is refused naming it: raise `code.limits.max_push_bytes` (at most 1 GiB). The next step is built while one is pushed, so delta search overlaps upload and node work (at most two packs in the working directory); a failed push ends the next build at once.
7. **The pushes**: each step is one `git push --porcelain --atomic
   --no-verify --no-thin` from the step's own repository, through `crewlet gitlet
   credential` exactly as `export`'s clone is. `--atomic`: a step lands whole or
   not at all. `--no-verify`: your own pre-push hooks are for your own pushes,
   and one that uploads elsewhere — Git LFS's — would refuse every step. And
   each is **pinned against your own push settings**, which would change what a
   step carries or have git refuse it: `--no-signed` (`push.gpgSign` asks for a
   push certificate Gitlet does not take), `--no-recurse-submodules`,
   push options cleared (`push.pushOption`: a `change` kept there would open a
   [change](../guides/gitlet.md#open-a-change-from-a-push) from every step that
   pushes one branch, and an import is history, not a proposal),
   `push.negotiate` off, and the copy's own settings (`pack.window=0`, and no
   multi-pack-index, bitmap or whole-pack reuse of the clone's, so every object
   comes from the step's pack). `push.followTags` needs no pin: a step's
   repository holds no ref, so it has no tag to add.
8. **The proof**: every branch and tag of the source, read back from the node as git lists it, at the source's value; any that differs exits 1 naming it.

**A stepped history stores more than one pack of it**: every step keeps whole the files its deltas cannot reach in earlier steps. This project's history, 34.0 MB as one pack, came to 68.5 MB in eleven steps and 78.6 MB in forty-two. What the repository stores is what the object store holds and every serving node caches, so run a large import with `code.limits.max_push_bytes` at its 1 GiB ceiling (fewest, largest steps) and size `code.limits.repo_max_bytes` for the steps, not the source's pack. A step that would take the repository past it is refused **before it is sent**, saying what the repository would hold, what is left of the source, and to raise the limit and run again.

**A failed step is safe to run again**: the import stops at the first step git refuses, printing git's own line for every ref refused (`! <id>:refs/heads/frozen [remote rejected] (protected_direct: …)`) with git's words for why, which a ref's line does not always carry (an object the node's checks refuse is `(unpacker error)` there, its reason git's `remote: error: object …: badTimezone: …`), and how many steps landed. Fix the cause and run the same command again: step 5 finds what landed and pushes the rest. Protections apply as to any push, so a branch whose rule says `direct_push: none` refuses its own import: lift the rule for the import, restore it after.

`-dry-run` does steps 1–5 and prints the plan: every step sized from how the source stores its objects (`≈`), nothing measured or pushed, a step marked where its estimate passes the push limit or takes the repository past `code.limits.repo_max_bytes`. A merge whose estimate passes the push limit has its other side planned first, as the run does (`stepping … first`). A real run's steps can differ, since a step is cut where its measurement says.

Git LFS: the import carries the pointer files a history holds, never the large files an LFS server keeps; Gitlet serves no LFS.

Exits 0 once every ref is proven, 1 on any refusal, a stopped step or a ref that differs.

### `crewlet gitlet purge-unreachable`

Asks the repository's maintainer (the Gitlet node that compacts and publishes it) to drop every object no branch or tag reaches: the commit a leaked secret was force-pushed away from, the history a deleted branch left. It is the `code_purge_unreachable` tool your assistant and a repository's Settings tab call, over the same route ([`POST /operator/act/{tool}`](api-endpoints.md#operatoract--the-dashboards-write-surface)).

```
$ crewlet gitlet purge-unreachable web
purge of web requested (0192f6c1-…, at CREWLET_CODE_LOG@1:4821)
  served by git-2, the repository's maintainer
  until it is served and after, the purged objects are still held by:
    the caches of git-2, git-1, until each syncs past the purge
    every backup taken before the purge commits keeps the purged objects for as long as the backup is kept, and on the nats object store every backup taken before the store deletes them, since its snapshot of the bucket carries every byte the bucket holds. The repository's detail names the instant the purge committed.
    the object store keeps them for 24 hours after the purge commits, until the retired packs' sweep and the store's next collection take them, and a versioned S3 bucket for as long as its own rules keep a deleted object's noncurrent version.
  `crewlet gitlet status -repo web` shows the request until it is served
```

**A person's alone.** `$CREWLET_API_TOKEN` must be your own token (`crewlet iam token -login` mints one) bound to the human seat you hold: a token bound to nobody is refused, naming why, and so is a seat's tool call. Your grant on the repository must be `maintain`, which every person holds. The fresh proof of identity a dashboard session gives (`sign_in.step_up_seconds`) is not asked of a token, which is stepped up by construction, so keep a token that can purge as carefully as your password.

**No grace.** A ref's drop is otherwise fetchable for two weeks; a purge keeps only what a ref reaches. The command returns once the request is recorded, long before it is served: `crewlet gitlet status -repo <name>` prints an `expunge:` line naming the request until served, then when the purge committed and what it dropped. Asked while another request is open, it widens that one: the repository holds one purge, served once nothing from before the latest request is left.

**A retry is the same request.** The command mints an operation id and sends it as the `Idempotency-Key`; an answer whose outcome is unknown, or none, prints it, and `-op-id <id>` sends it again, which the repository's committer answers as the first request.

### `crewlet gitlet credential`

git's [credential helper](https://git-scm.com/docs/gitcredentials) for Gitlet. Configured for the node's address:

```sh
export CREWLET_API_TOKEN=…
git config --global credential.https://code.example.com.helper ''
git config --global --add credential.https://code.example.com.helper '!crewlet gitlet credential'
```

The empty value comes first so the address has this helper alone: git hands a credential to **every** helper to store, and a storing helper your system configures (osxkeychain, Git for Windows' credential manager) would keep the token and keep sending a rotated token's old value until a refusal erased it. Configured so, it answers git's `get` with `username=crewlet` and the token in `$CREWLET_API_TOKEN` (`-token-env NAME` reads another variable) as the password; the user name is ignored. It answers **only over HTTPS**, or plain http to this machine (`localhost`, `127.0.0.1`, `::1`): over plain http to another host it says it will not send a token in the clear and answers nothing, and git asks its next helper. An empty variable answers nothing too. `store` and `erase` do nothing: the token lives in the variable, never a file.

## `crewlet fleet broker`

```
crewlet fleet broker list [-json] [<config.yaml>] [-api URL]
crewlet fleet broker remove <node> -confirm <node> [-force] [<config.yaml>] [-api URL]
crewlet fleet broker remove -peer <peer> -confirm <peer> [-force] [<config.yaml>] [-api URL]
crewlet fleet broker credential [<config.yaml>] -o FILE [-api URL]
```

The fleet broker's membership. Two records say who its members are: every node advertises its **broker kind** on its presence lease (derived from its `stream` block, never its roles; see [Configuration](../concepts/configuration.md#noderoles)), and the broker's **metadata group**, the raft group placing every stream and consumer, counts its voters itself. `list` and `remove` talk to a running node, which asks a member where it is not one: only a member holds the group, and a removal is answered only on its system account, unreachable from outside its process. They are clients of the routes in the [API reference](api-endpoints.md#the-brokers-membership). `credential` is for broker gestures no verb performs, made with the `nats` tool on a member's own host.

### `crewlet fleet broker list`

It answers **does the broker still count a member I have lost?** One row per live node: what it advertises, its roles, how the group counts it (`leader`, `current`, `offline, last heard 5m ago`, or `-` for a non-voter) and its **peer id** where it is a voter. Then one row per voter no live node is, and under the table every disagreement in words.

Voters are counted by raft peer id, derived from the node id. A member hears another's name only from that server, so a voter whose survivors restarted since it died is listed as `(name unknown)` by peer id alone; a voter is matched to its node by peer id, never by a name the answering member heard.

- **DEAD MEMBER**: a voter no live node is, or whose node came back as a leaf or a client. Every election and create goes on counting it; the line prints the `remove` command for once it is not coming back, by `-peer` for a voter nobody can name.
- **NOT COUNTED**: a live node advertising a member the group does not count: still joining, or removed while it ran.
- **UNKNOWN**: a node whose presence does not say what its broker is, which a capacity seal counts as a member.

A group no member could report is said to be unread, never shown empty. On a fleet whose broker is an external cluster it says so and lists nothing: that membership is the cluster operator's. On a clustered member it also prints the answering node's **client listener**, on loopback, reachable from that host alone, where `credential`'s key reaches the broker. `-json` prints the node's answer as JSON: every field and number as sent, text as written, keys sorted.

### `crewlet fleet broker remove`

Stops the metadata group counting a member once the group has committed the change, printing which member's system account carried it and the voters that remain. Name it by **node id** (reaches it by the peer id the id hashes to, whether or not any member remembers its name), or by **`-peer`** with the peer id `list` shows, for a voter nobody can name. **Refused while its node holds a live presence lease as a member** (or without saying what its broker is): a running member removed rejoins as a voter at its next restart, so stop it first. `-force` removes it anyway, for a member wedged in a way that still renews its lease. A node alive under the voter's name as a **leaf** or **client** is removed without `-force`: the member it was is gone for good. The member being removed never carries its own removal; if it is the only live member, the command says so.

One wait bounds the removal: the carrying member's commit budget and a round trip. A carrying member that does not answer, or an answer the node cannot believe, ends it as an outcome nobody knows (it may have committed) rather than another member proposing again; `list` reads the group. Requests and answers are signed under the fleet keyring and bound to that one request, because the member carries a removal out through its system account and the broker's admission vouches for a connection, never a request. A member holding no key the request is signed under runs nothing and the next member is asked.

Only the group's leader answers, and a member removed because its host died was often the leader, so the removal asks again every second while the survivors elect another, and waits out an election rather than failing. A group that answers nobody for the whole wait (two minutes) has lost its quorum and can change nothing about itself; the refusal says so: bring enough members back for a quorum, then ask again. A removal the node did not answer may still have been committed; `list` reads the group, and asking again for a member already removed is refused as not a member.

### `crewlet fleet broker credential`

Writes the **operator key** an embedded fleet's broker admits on a clustered member's loopback client listener, for the `nats` tool: gestures no engine verb performs, above all resizing every stream and bucket before a raised `stream.replicas` lets a node start ([Deployment § Reaching an embedded fleet's broker with the nats tool](../guides/deployment.md#reaching-an-embedded-fleets-broker-with-the-nats-tool)). Run it on the member's own host:

```bash
crewlet fleet broker credential -o ./operator.nk
nats --server nats://127.0.0.1:41234 --nkey ./operator.nk stream ls
```

The key is derived from the active key of THIS host's `secrets.keys` (read from its own Tier A, never fetched) and written to `-o FILE`, created `0600` and refused where anything is already there; it never reaches the terminal. The address is asked of this host's running node (`GET /fleet/broker`, the token in `CREWLET_API_TOKEN` taking `admin`): its port is the OS's at every start, and a loopback listener is its host's own, so an answer from another node (a `-api` naming a balancer or peer) is refused and nothing is written. A solo member, a member with only a leaf listener, and a leaf open no client listener, and say so; a node on an external cluster is reached with that cluster's credentials. A member serving no API (`api.port: 0`, no `-api`) is asked nothing: the invocation is printed with the address left for you, and the node's `broker_keys` line names it.

The key reaches every stream and bucket and never the system account (membership changes go through `remove`), and grants nothing its holder lacks: whoever reads the node's Tier A holds the keyring. It is admitted as long as its key is in `secrets.keys`; write it again after a rotation drops it.

## `crewlet feeds`

```
crewlet feeds dead-letters [-json] [<config.yaml>] [-api URL]
crewlet feeds discard <id> -confirm <id> [<config.yaml>] [-api URL]
```

The wake feeds' dead letters. A domain's wake feed hands a record it cannot read back for a node that can, and one no node has read within an hour of being stored is kept as a dead letter in `CREWLET_DLQ`, its sealed frame inside, and raises `feed_dead_lettered`; the first node whose feed starts able to read an entry replays it, and the stream's age bound lets one go after `stream.event_retention_hours` ([Dead letters](../concepts/event-system.md#dead-letters)). Both commands talk to a running node (the entries are in its broker); every node serving the API answers alike. They are clients of the routes in the [API reference](api-endpoints.md#the-wake-feeds-dead-letters), and the token in `CREWLET_API_TOKEN` takes `admin`.

A wake the notification service's inbound group passed round during a roll and no node took is that subscription's dead letter, not a wake feed's: neither command lists or discards it. Every node able to read it replays it: one already running within a few seconds of its keep, else the next such node to start; `inbound_dead_lettered` is raised while it waits ([Dead letters](../concepts/event-system.md#dead-letters)).

### `crewlet feeds dead-letters`

One row per entry, every feed's in turn: its **id** (the one `discard` names); its **feed** (`work`, `page`, `chat` or `code`); the **reason** no node read it (`newer_record`, `newer_envelope`, `unknown_key`, `untranslatable`, or `(unreadable)` for an entry a newer build kept, unreadable here past its id); where its **record** was delivered from, as every operator surface writes a position (`CREWLET_TRACKER_LOG@1:1234`); its **key**; how long ago it was **kept** and when it **expires**, by the answering node's clock. Under the table, each entry's own words: what the node that gave up met, or why this build cannot read it. Feeds holding nothing say so.

A feed whose dead letters could not be listed is named on stderr with its error, and the command **exits non-zero**: an unread feed is not an empty one. `-json` prints the node's answer as JSON, keys sorted, exiting the same way.

### `crewlet feeds discard`

Drops the entry `<id>` from whichever feed holds it, and prints it. **Its wake is lost for good: no node will ever derive it**, which is why `-confirm` must repeat the id. Discard only once you have decided its wake can be lost: one no build will ever read, such as a record sealed under a key retired from every node's `secrets.keys`, which would otherwise keep `feed_dead_lettered` raised until retention lets it go. An entry a newer build will read is not one: finish the roll, and the first node on that build replays it.

The node takes the claim on the entry's going that its replay and the alarm's expiry count take, so no node counts a discarded entry as a wake the retention lost; it logs `changefeed_dead_letter_discarded` naming who pressed it and counts `crewlet.tracker.feed.dead_letter_discarded`. A keep of the same record landing within fifteen minutes of its entry (a node whose keep was still crossing a link) stores nothing while the broker runs; after a broker restart it is stored and listed again, and must be discarded again. An id no feed holds (never an entry, or already replayed, expired or discarded, or a subscription's poison under another `dlq.` subject, which this never reaches) is refused as `no_dead_letter` and removes nothing. Where a feed could not be listed and no other holds the id, the node says so rather than "no such entry": run it again. A discard the node did not answer may have been made; `dead-letters` lists what the feeds still hold.

## `crewlet doctor`

```
crewlet doctor [-json] [-words] [<config.yaml>] [-api URL]
```

The [Advisor](../guides/advisor.md)'s open findings about the running node's company, most costly first: what its document costs it, what its integrations' last check reported, and the patterns the engine read in its last 7 days of work, as its latest run left them, neither snoozed nor applied. It is a client of [`GET /advisor/findings`](api-endpoints.md), and the token in `CREWLET_API_TOKEN` takes `admin`.

Each row is a class of finding: its number, its **severity** (`blocks`, `costs`, `note`), its **actor** (`owner` for a change you send, `engine` for a pattern the engine is reducing), its **class**, every **subject** it is raised for, and each subject's **evidence** as counts, separated by `;`. A subject is `company`, or a seat, unit, schedule, repository or integration as its kind and the order the answer first names it in (`seat#1`, `schedule#2`; one subject keeps one ordinal), never a handle. Below the table, `Not read:` names what the node could not read (its integrations' state), so a short or empty list never reads as all clear. That is all it prints by default, so the output can be pasted into an issue or chat without the company's names; `-words` prints subjects by handle (`schedule:sre:health-check`, an integration by its product, `integration:GitHub`) and, under the table, each class's title and why once, with each subject's item under it. `-json` prints the node's answer as JSON, keys sorted, with every name in it.

The command **exits 2** while any finding is `blocks`, so a script can hold a go-live or configuration change on it, and **1** when it could not ask (no node, a refused token, an unreadable answer), the latest run could not read a source (`Not read:`) or the Advisor has not run yet, since a finding that blocks may be missing; a listed block still exits 2. `costs` and `note` findings exit zero. It sends nothing: a fix is previewed and sent from the dashboard, or with `PUT /config` as the advisor's guide shows.

## `crewlet seats`

```
crewlet seats pause <handle> [-stop] [-reason TEXT]
    [-op-id ID] [<config.yaml>] [-api URL]
crewlet seats resume <handle>
    [-op-id ID] [<config.yaml>] [-api URL]
```

Pause an agent seat, or resume it: the same `pause_seat` and `resume_seat` the dashboard's buttons call, over the same route ([`POST /operator/act/{tool}`](api-endpoints.md#operatoract--the-dashboards-write-surface)), one gesture with one record. The node decides it on the principal `CREWLET_API_TOKEN` resolves to: the seat's holder, whoever leads it, or an administrator. The write is recorded under the caller's own name, kind and credential; nobody is refused for being bound to no seat, and a token with the authority acts under its own login.

A paused seat starts no new turn, its incoming mail waits on its inbox in order, and its scheduled runs are recorded `skipped_paused` rather than sent. The turn it is on finishes first unless `-stop` is given, which ends that turn at its next round and its executor in its box at once, the box reclaimed and what it had spent charged; what was stopped is not run again. `-reason` is one line (at most 500 characters), shown on the seat and in the feed. Pausing a paused seat changes nothing (except that `-stop` adds the stop), and resuming a seat that is not paused changes nothing. See [Agent Runtime § Pausing a seat](../concepts/agent-runtime.md#pausing-a-seat).

The outcome is `applied` (the pause is the fleet's record; the node holding the seat carries it out from its own copy within about a second) or `unknown`, when the node could not confirm the write. Every invocation sends an operation id as the request's `Idempotency-Key`, which the act route requires: `-op-id` takes any id in the engine's operation-id grammar; without it the command mints `<uuidv7>.pause` or `<uuidv7>.resume`. An `unknown` outcome, or no answer, prints that id; send it again with `-op-id` so the retry is the same operation. Pass it exactly as printed: it carries the instant it was minted, which a node reads to decide whether it can still vouch for a retry.

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
    ... [<config.yaml>] [-api URL]
```

The company's artifacts from a shell, through the routes the dashboard is built on ([Artifacts](api-endpoints.md#artifacts)), as whoever `CREWLET_API_TOKEN` resolves to. The node decides every gesture on that principal exactly as it decides the dashboard's: publishing, renaming and labelling are an editor's and ask the artifacts app; sharing and withdrawing a share are the owner's; trashing and restoring are the owner's and a person's; a purge is an administrator's with a fresh proof of identity and a person present, which a machine token never is.

**`publish` moves only the bytes the artifact does not hold.** It declares every file first (path, SHA-256, size; a directory is walked, any name starting with a dot is left out with everything beneath it, and a symbolic link inside is never followed), and the node answers which files the artifact already holds (carried by digest, no byte moved) and how each of the rest is sent: straight to the bucket on a presigned put where the object store offers one, through the node otherwise. Then the version is published.

`-exclude PATTERN` leaves files out of a directory, once per pattern (`-exclude '*.map' -exclude drafts`). A pattern with no slash matches a file's or folder's name at any depth; one with a slash matches its path from the directory published (`notes/*.md`, where `*` stops at a slash). A matched folder is left out with everything beneath it; a leading `./` or trailing `/` is ignored. The grammar is Go's `path.Match`: `*` and `?` within one name, `[...]` for a class, `\` to escape. A malformed pattern, one reaching outside the directory, or `-exclude` beside a single file is refused before anything is sent. Quote a pattern so the shell does not expand it.

A new artifact opens on the one default entry it holds, `index.html`, `index.md`, `doc.json`, `deck.json`, `dashboard.json` or `canvas.json`, else the only file, unless `-entry` names another; a directory holding two default entries is refused until `-entry` or `-kind` settles which. Its kind is the one whose entry that file is: `html` for a page, `doc` for a `.md` file or a tabbed doc's `doc.json`, `slides` for `deck.json`, `dashboard` for `dashboard.json`, `design` for `canvas.json` (an index is told by its path, never by JSON alone), and `-kind` names it outright. Its title defaults to the entry's name, and `-description` and `-icon` set the gallery card's subtitle and the tab's icon word. Those four describe a new artifact only: naming `-title`, `-description`, `-icon` or `-kind` beside `-artifact` is refused before any byte moves, since a version keeps the artifact's kind and `update` changes the rest. A new version of `-artifact` names the head it read as its base, so a version somebody else published meanwhile is refused rather than overwritten; `-force` publishes over it, a person's gesture. A new version keeps the head's runtime contract and capabilities unless `-contract` or `-capabilities` names others: `-capabilities db,state` declares exactly that set, `-capabilities none` declares none, and a name this build does not serve is refused before any byte moves. The result line counts files two ways: how many this command sent, and how many of the version's files are new and how many the artifact already held, as the node counted the manifest that landed. They differ for an unchanged page: a page is stored in its linted form, so its own digest never matches and it is sent, and the node then finds the linted page already held and carries it. The publish policy's warnings are printed beneath the result.

**`get` and `versions` show each version's runtime contract and capabilities**: `get` the head version's on its `contract` and `can` lines, `versions` one `contract` and `can` per version. Either reads `none` where there is none (a Markdown document or a sheet pins no contract; a version publishing `-capabilities none` declares none).

**`update` changes the title, the description or the icon** (`PATCH /artifacts/{id}`, an editor's) and sends exactly the flags it names: an empty value clears the description or the icon, an unnamed field is left as is, and a call naming none is refused before anything is sent.

Every write sends an operation id as the request's `Idempotency-Key`: `-op-id` takes one in the engine's operation-id grammar; without it the command mints one. An `unknown` outcome, no answer, or a success whose outcome this build cannot read **exits non-zero** and prints it; send it again with `-op-id` so the retry is the same operation, through another node with `-api` where the answering node cannot vouch for it. A `503` refusal that wrote nothing says so, and the retry once the node can take it is under the same id. A publish keys its upload intent with the id and its version with a step of it, so a retried publish reuses the upload and lands one version.

**`sources` lists the dashboard source catalogue** the node answers: each engine source a `dashboard`'s datasets may read, what it reads, who may read it, its parameters (with choices and defaults), its columns and the push it refreshes on, or one source whole when named. A source is answered as each board's viewer, so the list holds no row; see the [Dashboard Spec](dashboard-spec.md#the-engine-source-catalogue).
## `crewlet chatlet`

```
crewlet chatlet channels|read|post|search|erase|prune|retention
    [<config.yaml>] [-api URL] [-op-id ID]
```

The company's own chat from a shell: the
[`chat.backend: chatlet`](../concepts/chatlet.md) axis. On a company whose chat is
a vendor's or `none`, chat's routes and tools are never registered, so every verb is refused and names `chat.backend`. A node in a maintenance mode (`crewlet run -mode`) refuses the writes the same way; a node without the `ingress` role serves no chat route.

**Every verb talks to a running node**, as `crewlet budgets` and
[`crewlet worklet purge`](#crewlet-worklet-purge) do: rooms and messages are the state log's, so a write is a record the broker arbitrates and every data node applies, and on the default topology that broker is in the engine's process with no listener. Opening the store instead hits a locked database or writes rows the log never carried.

**No verb names an author.** The node decides every call on the
principal `CREWLET_API_TOKEN` resolves to, and a chat verb is made **as a
seat**: your machine token (`crewlet iam token -login <login>`) acts as the seat you hold, and a service account or Tier A token's row the
identity directory binds to a human seat (`crewlet iam bind`) acts as that one.
A credential acting as itself (the deployment's Tier A token, a service account bound to no seat) is refused
every room, reads included, `unseated`, and the command prints those two
remedies. `erase`, `prune` and `retention` are an administrator's, said in
nobody's voice, on the terms of
[`crewlet worklet purge`](#crewlet-worklet-purge) (`admin`, a recent proof, never a
seat), made under the caller's own name, a Tier A token holding `admin`
included; none reads the room it acts on. See
[Chatlet § People](../concepts/chatlet.md#people).

Every read prints the [level it was served at](../guides/consistency.md), the
position it answered at and how far behind the log the answering copy is (this node's while it serves, a data node's otherwise); a copy that could not
present every row says `incomplete`.
Every write is three-valued: `applied` is on the
answering node's rows, `pending` is durable on the log and not yet applied
there (**do not run it again**), `unknown` is the one to retry. Every
write sends an operation id as the `Idempotency-Key`: `-op-id` takes
an id the engine printed; without it the command mints one **before** it
asks. An `unknown` outcome, an unreadable answer, or no answer
(dropped connection, timeout; the write may have landed) exits non-zero printing that id; resend with `-op-id` so the
retry is the same operation, not a second message, erase or prune. Pass
it exactly as printed: it carries its mint instant, which a node
reads to decide whether it can still vouch for a retry.

### `crewlet chatlet channels`

```
crewlet chatlet channels [-archived]
```

The rooms this seat is in, most recently active first, as `ID  NAME  KIND
UNREAD  LAST`. The **id** is the value `read`, `post`, `search`, `erase`,
`prune` and `retention` take. A direct conversation has no name; its row carries the participants' handles. An unread count
at its ceiling prints `N+`; a muted room says so. When
the node could not read where this seat has read to (read state lives in the
coordination store), every count prints `?` and a line says
why. `-archived` includes rooms
that take no new messages and stay readable.

### `crewlet chatlet read`

```
crewlet chatlet read <room-id> [-limit N] [-before CURSOR]
crewlet chatlet read <room-id> -thread MESSAGE-ID [-limit N] [-after CURSOR]
```

One room's transcript, oldest first, a page at a time: each line carries the
message's number in the room, when it was said, **its id** (what
[`chatlet erase`](#crewlet-chatlet-erase) takes) and who said it. A room's
page holds only what was said in the room; a message with replies says how many and the flag
that reads them (`[3 replies: -thread <id>]`). `-thread` prints
the root message, then its replies oldest first, each marked `↳<id>` with its
root, and the `-after` cursor for the replies
after them. A room pages back with `-before`, a thread forwards with
`-after`; each refuses the other's. Extra lines of a message stay under
its own heading, its links print beneath it, an edited one says so and
a taken-back one reads `(deleted)`. A [room event](../concepts/chatlet.md#a-room-says-what-changed-in-it)
prints what changed in brackets, under whoever changed it (`(joined: ana, ravi)`,
`(set the topic: …)`); a room with nothing else on the page is said to have
no messages. The page ends where the room does or at
`-limit`, and prints the `-before` cursor for the page before. A
room the seat may not read answers exactly as one that does not exist.

### `crewlet chatlet post`

```
crewlet chatlet post <room> -body TEXT [-link URL]... [-op-id ID]
```

Says something in a room, by id or name, as your seat: the same
`post_message` the dashboard's composer calls, over the same route
([`POST /operator/act/{tool}`](api-endpoints.md#operatoract--the-dashboards-write-surface)),
so a terminal post and a dashboard post are one gesture with one
record and one `operator_acted` audit row. `-link` repeats, for what the
message points at (a message links to a file, never carries one); each
is an absolute `http`, `https` or `mailto` address in full, and a
post carrying any other is refused with nothing said. It
prints the message's id and outcome, the seats it mentions, and, when
`@channel` addressed the whole room, that it wakes the agents in
the room (the people are told, never woken).

### `crewlet chatlet search`

```
crewlet chatlet search <text> [-channel ROOM-ID] [-author HANDLE] [-limit N]
```

Ranks every room this seat may read, or one with `-channel`, as `SCORE
ROOM MESSAGE WHEN WHO SNIPPET`, and says how many rooms it searched. A node
whose index is still being built says so. The query is at most 400 bytes (the bound every ranked search shares); a
longer one is refused, not cut.

### `crewlet chatlet erase`

```
crewlet chatlet erase <room-id> -message ID... -reason TEXT -confirm N [-op-id ID]
```

Destroys the named messages on every data node, with the reactions, mentions
and thread rows hanging off them: for a credential pasted into one message,
keeping the conversation around it. No inverse, so
`-confirm` repeats how many messages it destroys (a repeated id counts once), and `-reason` is required: the erase
record's reason is the only account of them that survives. The ids are those
[`chatlet read`](#crewlet-chatlet-read) and [`chatlet search`](#crewlet-chatlet-search)
print. Like every destruction it reads nothing from the room first, so it
reaches a private room the administrator is not in. See
[`POST /chatlet/channels/{channel_id}/erase`](api-endpoints.md#a-chat-erase-prune-or-horizon).

### `crewlet chatlet prune`

```
crewlet chatlet prune <room-id> -cutoff TIME -confirm TIME [-op-id ID]
```

Destroys everything a room said before an RFC 3339 instant, on every data
node, now rather than at the room's [retention](../concepts/chatlet.md#retention)
horizon: for an erasure request covering a room's history. No
inverse, so the cutoff is typed twice and compared as an instant (two zones
naming one moment are one cutoff); a future cutoff is refused, since it
would empty the room rather than enforce a horizon.

### `crewlet chatlet retention`

```
crewlet chatlet retention <room-id> -days N | -company -confirm N|company [-op-id ID]
```

Sets how long one room keeps what is said in it: `-days N` (`0` keeps it
for ever) or `-company`, which returns it to the company's
`chat.chatlet.message_retention_days`. Exactly one. The change destroys nothing itself; the retention duty prunes what the new
horizon no longer covers on its next pass, on every data node, with no
inverse, so a shorter horizon is confirmed as a prune is: `-confirm` repeats the horizon, the same number of days
(`0` included) or `company`, and a command whose two disagree sends nothing.
Every horizon takes it, a longer one too, since whether it shortens the
room's depends on what the room had, which the command does not read.

## `crewlet retention`

```
crewlet retention status|snapshots|ack|evict|readmit|set-capacity|maintenance|reanchor|verify
    [<config.yaml>] [-api URL]
```

What the state log is holding, why it is not shrinking, and the gestures that
change it. A **group** because several
stop a machine writing and should not sit beside
`version`.

Every verb but one talks to a running node, for `backup`'s reason: the register
is a coordination bucket on a broker embedded in the
engine, which binds no socket. The exception is `verify --restore`, which reads an artefact off disk.

### `crewlet retention status`

A domain this node **refuses** leads with the
refusal's own sentence, and for a `wrong_stream` the finding behind it
(`recreated`, `ahead_of_log`, `log_diverged`, `generation_passed`):

```
NOT READY pages: wrong_stream (recreated): pages's rows are keyed to the stream created at … and the broker's CREWLET_PAGES_LOG was created at …
WRITES REFUSED tracker: log_truncated: peer node-4 stands at sequence … of CREWLET_TRACKER_LOG, which ends at …
```

While a capacity window is open on any log it leads with that window
(`MAINTENANCE IS OPEN on <log>: no node starts in service until it closes.`),
naming what it waits on or saying it couldn't read that; a window that needs a person says why in the node's words
(`BLOCKED`, never the code a script reads from `blocked`) and who abandoned it.
`crewlet retention maintenance status`, `set-capacity` and the gestures print
that sentence on its own line under the table, as the window's next step.
A log whose window the node couldn't read gets its own line:

```
This node couldn't read whether a capacity window is open on "CREWLET_PAGES_LOG". A data node restarted now won't start in service until it can read it. `crewlet retention maintenance status -stream CREWLET_PAGES_LOG` asks again.
```

Then the blocking term, **in prose**:

```
Nothing is being trimmed on tracker: the newest backup is 72h0m0s old, past the 24h0m0s this deployment accepts.
```

Then one row per registered domain (stream, generation, replay protocol,
both ends, bytes, what it took in over the last day (`PER DAY`), ceiling,
reserve, headroom, trim floor), the six terms with state and detail,
one row per node per domain, and this node's replica line. `TRIM FLOOR` is
`-` where the trim has concluded nothing about the domain's generation yet
(after a reanchor, until its first tick on the adopted stream) and
`unreadable` where the floor register could not be read; the watermark block
says which instead of listing terms. Each node row carries its position's
generation (`GEN`); a position from a generation the log has left prints
`left gen N` (or `ahead gen N`) in place of a lag; a node that reported its log diverged at its
checkpoint is marked `LOG DIVERGED`. `RESERVE` is the top of the ceiling every
identity-claiming log (tracker, knowledge base, identity estate, artifacts log, artifact datastore,
Ask Crewlet, chat, executor log, code log)
keeps for
gate records ([the gate reserve](../guides/retention.md#the-gate-reserve)); `-`
on the vector changelog and the usage log, which keep none. `HEADROOM` is what
is left of the rest, the ceiling ordinary writes are refused at, so a log at
`0%` still takes an eviction.

A term that does not apply prints `n/a`, not `0`: the
vector log, identity estate, artifacts log, artifact
datastore, Ask Crewlet, executor log and usage log have no wake
feed, and an absent term differs from one that permits nothing. Where a log has
one, its `feed_ack_floor` is that log's own feed. `PER DAY` prints `-` where nothing was measured (a
compacted log, a log in its first two days, a node whose trim has not ticked)
and `0 B` only for a log that took in nothing; see
[Retention](../guides/retention.md#the-one-rate-and-what-it-is-held-against).

**It exits non-zero exactly when this node has an active alarm**, on the
report's own rule. Each alarm's measurement and remedy go to **stderr**, so a cron
capturing stdout still gets the reason. An alarm
that stands on logs names them: one measurement in its headline with the logs
it was taken on (`log_headroom on usage: 3% of the log's byte ceiling for
ordinary writes is left`, or a fleet with no backup yet as `backup_age on
tracker, vectors, pages, iam, usage, …: no verified backup has been
recorded, …`, once, since every log measured it in the same words), several as
a line each with the term holding the log's trim (`  tracker (backup_floor): the
trim has not advanced for 46h 33m, and the log is keeping up to 1840 record(s)
older than its 24h replay window`).

`-domain <name>` narrows the log and watermark blocks to one domain.

### `crewlet retention snapshots`

Its own verb because the repository is per
node: *which of my machines can donate, and how old is what they hold*. A node with no artefact still gets a row carrying the reason:
`sole_node`, `lagging`, `unhydrated`, `deferred`, `insufficient_space`,
`ahead_of_log`, `log_diverged` or `failed`. A
node holding a current artefact carries no reason (`recent` is the
skip a healthy node takes, never published).

### `crewlet retention ack`

```
crewlet retention ack -stream CREWLET_TRACKER_LOG -position 918100000 -taken-at 2026-11-17T03:00:00Z
```

Publishes an operator backup floor. Under `backup_floor: operator` the trim
follows this, not the engine's own copies, because a backup is not a
backup until it leaves the host and the engine cannot see that it has. All
three flags are required: the floor is what the trim deletes
against, so there is no value to guess.

`-taken-at` is when the copy was taken, an RFC 3339 instant; `backup_age` and the backup check age the copy
from it, never from when you acknowledged. An instant later than the
node's clock is refused (`taken_at_in_future`, naming what the node's clock
reads).

One acknowledgement covers the one log it names, kept per log. Under `operator`,
acknowledge every log once its copy is off the host:
a log never acknowledged is never trimmed, and `backup_age` names it.

### `crewlet retention evict` / `readmit`

```
crewlet retention evict node-4 -confirm node-4
```

An absent node pins the `applied` term for ever: its position never advances,
so nothing above it can be deleted. Right for a node that is
coming back; eviction is for one that is not.

`-confirm` repeats the node id. The trim counts nodes per log, so the eviction is a record on every log that
counts nodes (tracker, knowledge base, identity estate,
artifacts log, artifact datastore, Ask Crewlet,
chat, executor log, code log), and each answers on
its own line with its
[three-valued outcome](../guides/replication.md#a-write-has-three-outcomes), or
`not written` and the reason:

```
$ crewlet retention evict node-4 -confirm node-4
evict node-4 (operation 01a0cd85-735a-7294-9d3e-38998abd698c.evict-node-4)
  tracker: applied at CREWLET_TRACKER_LOG 918280002
  pages: applied at CREWLET_PAGES_LOG 4410
  iam: applied at CREWLET_IAM_LOG 58311
  artifacts: applied at CREWLET_ARTIFACTS_LOG 30552
  artifactdata: applied at CREWLET_ARTIFACT_DATA_LOG 112940
  assistant: applied at CREWLET_ASSISTANT_LOG 15734
  chat: applied at CREWLET_CHAT_LOG 207714
  executor: applied at CREWLET_EXECUTOR_LOG 30870
  code: applied at CREWLET_CODE_LOG 2207
  access: applied at CREWLET_ACCESS_LOG 4120
  atlas: applied at CREWLET_ATLAS_LOG 310
  advisor: applied at CREWLET_ADVISOR_LOG 5140
  node-4 stays COUNTED for about a minute, so a live node is certain to have read its own tombstone before the trim passes it.
  tracker trim floor 918100000 → 918100000
  pages trim floor 4100 → 4100
  iam trim floor 57000 → 57000
  artifacts trim floor 30100 → 30100
  artifactdata trim floor 112000 → 112000
  assistant trim floor 15200 → 15200
  chat trim floor 206900 → 206900
  executor trim floor 30400 → 30400
  code trim floor 2100 → 2100
```

It prints the watermark before and after and the instant the
eviction takes effect: **the node stays counted for about a minute**, so a live
one has read its own tombstone before the trim passes it.
The node you ran it on writes every log itself (every data node holds the
whole estate).

The gesture is **judged once, before any log is written**. A node that still
holds a live presence lease is refused with `409 eviction_refused`: it is
probably running, and eviction would
drop everything it writes and take its log copies out of service. Stop it and wait
for its `LIVE` column in `crewlet retention status` to read `no`, or pass
`-force` for a node wedged in a way that still renews its lease; the refusal
prints both. A node
that cannot read the presence leases answers `503 eviction_unjudged`;
`-force` takes it past that too, and the refusal says so. A run that already passed `-force`
is never advised it again. A
node id no node could run under is refused `400 invalid_gate` before anything
is judged.

When **not every log holds the record**, the command exits non-zero, and under
each unfinished log prints what to do. Where the same command can
finish that log (an `unknown` outcome, a node still catching up) it names it,
with `-force` carried over from a forced run:

```
  pages: unknown (the record may or may not be on the log)
    its outcome is unknown: the same gesture under the same operation id answers from this log's own ledger if the record landed, and writes it if it did not
  The gesture has not reached every log. Run it again with -op-id 01a0cd85-… to finish it: a log that already holds the record answers from its own rows and is not written twice.
```

Run it again with that `-op-id`. Each log's record is published under an id
derived from it, the sign, the log and the node, and each log's snapshot reads
that id's ledger row first, so only the missing
log is written, however long after the first run. A fresh id
would be a second eviction, and an id not in the engine's grammar is refused. Where the same command **cannot**
finish a log straight away it prints what has to happen first, then the
same `-op-id`. A `log_full` log (full past even the reserve its gate records may use; one full only for
ordinary writes still takes them) needs
[`crewlet retention set-capacity`](#crewlet-retention-set-capacity):

```
  pages: not written (log_full): pages: statelog: unavailable (log_full): …
    CREWLET_PAGES_LOG is full to its broker ceiling, past even the reserve kept there for gate records: raise its ceiling, which is the only thing that makes room for it. Then the same gesture under the same operation id finishes it, where a fresh one would write every log that already holds the record again
    crewlet retention set-capacity CREWLET_PAGES_LOG <bytes> -confirm <bytes>, then run this again with -op-id 01a0cd85-…
```

The same command is refused until the ceiling moves, and **a fresh one afterwards is wrong**: once there is room, this gesture's own
`-op-id` finishes it, because a fresh id is a second eviction that
rewrites every log already holding the record (here all but the
knowledge base's) and re-dates the eviction. Other cases: a node
itself `evicted` cannot write, so it prints `-api <that node>` with the same
`-op-id`; a `wrong_stream` log needs a [reanchor](#crewlet-retention-reanchor)
first, then the same `-op-id`; a `superseded` operation (an eviction
retried after a readmission took the node back) is a new gesture, without
`-op-id`. The indented sentence is the node's own and names no flag,
because the dashboard renders it too; the line under it is this
command's rendering of the node's `actions` (see
[the gate answer](api-endpoints.md#the-three-retention-gestures-that-write)).

The command mints the operation id **before** it asks and waits a minute,
past the fifty seconds the node takes at most to answer one
gesture (twenty to judge it, thirty to write every log), which it finishes
even if the connection drops. So a gesture with
no answer still prints the `-op-id` that finishes it, and so does one
whose answer the node did not write: a reverse proxy's 504 page, any status
without an engine error code, or a 200 cut off part way. Pass it back
exactly as printed: the node refuses (`op_id_invalid`) an id not in
the engine's grammar (a UUIDv7 carrying its mint instant), and one over 128
bytes or holding anything but visible ASCII, since the broker would carry that
as a different id.

`readmit` is the inverse commit, not a delete, so the eviction's history
survives a replay. It is **refused** while the node is below a trim
floor, and the refusal prints its position beside the floor:

```
$ crewlet retention readmit node-4 -confirm node-4
crewlet: the node answered 409: readmission_refused
  node-4 may not be readmitted: its last position in tracker (reported 2031-04-02T03:14:00Z) is 1200 and records below 9000 may already be gone from the tracker log (published floor 9000, first surviving sequence 8800). It has to catch up before the fleet counts it again.
  Start node-4 if it isn't running: it catches up on its own, …
  its SEQ in `crewlet retention status`, at the domain's own GEN, says when it has caught up, and `crewlet retention snapshots` whether a peer can donate one; then run this again
```

The comparison is the node's own write fence's: its **last
published position** in each identity-claiming log (tracker, knowledge base, identity estate, artifacts log, artifact
datastore, Ask Crewlet, chat, executor log, code log) against the
**higher of
the published floor and the log's first
surviving sequence**, and the node must have applied every record up to the
one just before it. A node that never published a position is judged as
holding nothing; one whose position is from a generation the log has since
left is refused. The judgement is made once, for every log, before any write,
and a refusal writes nothing. A readmission that reaches
some logs and not others is finished with `-op-id`, as an eviction
is.

Let the node catch up on its own: start it if it
is not running, and it replays what the log still holds or adopts a peer's
snapshot (`crewlet retention snapshots` says whether any peer
can donate). Readmit it once its `SEQ` in `crewlet retention status` has
reached one less than the higher of that domain's `TRIM FLOOR` and `FIRST`; the
refusal's own `floor`, `first_seq` and `generation` are the numbers it
compared, and after a reanchor the domain shows `-` in `TRIM FLOOR` until
the trim's first tick on the adopted stream, so the bound is `FIRST` alone. The
`GEN` column says whether the position is at the log's current
generation: one from a generation the log has left prints `left gen N` in place
of a lag and is refused whatever its `SEQ` reads. The
position is a heartbeat old, so a node that has only just caught up can be
refused once more; run the command again.

A readmission that cannot be **judged** is refused too, writing nothing:
`503 readmission_unjudged`, when the positions register, a published
floor or a log's first surviving sequence could not be read. The node
is not told to catch up; ask
again, on this node or through another (`-api <that node>`).
Either gesture on a node in a
[capacity window](../guides/retention.md#changing-a-logs-ceiling) is refused `409 not_publishing`
before anything is judged; its line points at `crewlet retention status`, which
leads with the window while it is open (nothing is written to any log
until the fleet is back in normal mode).

The refusal protects the trim: readmitting a node below the floor puts back the
pin the eviction had lifted, so the trim stops
advancing. The node's writes were never the danger; its own fence refuses any write
that assumes a history it does not hold.

### `crewlet retention set-capacity`

```
crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 -confirm 8589934592
```

Changes a log's byte ceiling, raising or lowering it. A log's Tier A ceiling
(`stream.tracker_log_max_bytes`, `stream.tracker_vectors_max_bytes`,
`stream.pages_log_max_bytes`, `stream.iam_log_max_bytes`,
`stream.usage_log_max_bytes`, `stream.artifacts_log_max_bytes`,
`stream.artifact_data_log_max_bytes`, `stream.assistant_log_max_bytes`,
`stream.chat_log_max_bytes`, `stream.executor_log_max_bytes`,
`stream.code_log_max_bytes`, `stream.access_log_max_bytes`,
`stream.atlas_log_max_bytes`, `stream.advisor_log_max_bytes`) is not a live
setting, only the value its stream is created with. A resize is decided
against usage, which a
publisher keeps moving, so this runs with the whole fleet in a
maintenance mode and costs **three fleet-wide restarts**, two more per retry. The
[procedure is documented once](../guides/retention.md#changing-a-logs-ceiling);
this command's help prints the five steps.

`-confirm` repeats the byte count; the target is then fixed for the operation, since a verify compares the observed ceiling against it.

A raise the broker has no room for is refused **before** the maintenance window
opens, naming what it would reserve, what the broker has left, the limit
it is held to and the field that sets it, so you learn it in one
round trip, not after three restarts. A raise the broker
refuses at the apply is reported as a refusal, not an unknown
outcome, with the same numbers read at that moment. A limit this node could not read is **not** a
refusal, and the operation
goes ahead for the broker to decide.

`-i-have-excluded-all-publishers` is required only on `stream.type: nats`:
the engine does not run that broker and cannot establish who else holds a
connection to it, so the assertion is yours.

A target the log's ceiling already is opens no window (the verb run again after its own window closed with its
answer lost). A target whose ordinary ceiling is at or below
what the log already holds is refused before the window opens, naming both numbers and the least target that
would do, since it would refuse every ordinary append. On identity-claiming logs the ordinary ceiling is the target less
its [gate reserve](../guides/retention.md#the-gate-reserve), the larger of a
sixteenth of it and seven of the log's largest records with a mebibyte beside
them; on the vector changelog and the usage log, which hold no gate, it is the
whole target. A target under the floor Tier A holds the log's own field to is
refused too, naming the field: a gibibyte on the tracker's, knowledge
base's, code log's, vector changelog's, artifacts log's, artifact datastore's
and chat's, 64 MiB on the identity estate's, usage log's,
Ask Crewlet's and executor log's. A target under the
current ceiling and above the usage is accepted, and is how a log gives a
reservation back. A raise the broker cannot
reserve is refused before the window opens too, wherever the node can read that limit (a lone
embedded node, or a NATS account's own limit); a clustered member cannot, and
there the broker refuses it when the window applies it.

A window names at most 16 participants (every live member and data node, and
every node with a position row that isn't
[retired](../guides/retention.md#who-has-to-acknowledge)). On a fleet that
counts more it opens no window, naming each node counted only for its row, with
`crewlet retention maintenance retire` to retire one gone for good; only where
the live nodes are themselves more than 16 does it say a window cannot serve
the fleet.

Run from a node in `normal` mode it refuses outright, naming the restart. Run
from `maintenance` it opens, baselines and applies, and on a window already
baselined it records again who is running before it sends its request; run
from `seal` it collects
the barrier, seals, verifies and confirms. It prints the phase it reached and
the next gesture, with the full command. Once its window closes it sends the fleet back to service only when no
log has a window open and the node could read every log's: it names each window still open with the
command that finishes it, and each log whose window it couldn't read. A node
holding an **admission** (one that may be publishing) stops it in
one of two ways. If the process that wrote the admission is still running, the
window is closed again as it opens and nothing is sent: restart that node with
`-mode maintenance` and run the verb again. If it isn't running, the window
stays at `opened`, which has sent nothing for this attempt, and the first
request is refused naming the node: restart it with `-mode maintenance`, or
`exclude` one that will never boot again. While it waits the window refuses
every node that starts in service, so that refusal names `abandon` too. An
admission whose node now runs another process is withdrawn by the window
itself. A later attempt back at `opened` says what the last one left the
ceiling at.

### `crewlet retention maintenance`

```
crewlet retention maintenance status  -stream CREWLET_TRACKER_LOG
crewlet retention maintenance exclude -stream CREWLET_TRACKER_LOG -node node-4 -confirm node-4
crewlet retention maintenance abandon -stream CREWLET_TRACKER_LOG -confirm capacity-01J...
crewlet retention maintenance retire  -node data-17 -confirm data-17
```

`status` shows why a fleet is still excluded: the
operation, its phase and attempt, the process each participant was running as
the newest request left (RUNNING AT REQUEST) against what it acknowledged
for this log (its ATTEMPT reads `no window`
for a node that restarted before a window opened on the log, and `another
window` for one that last acknowledged an earlier window), any
unresolved request and when it was sent, and what the
window's next step waits on. At `opened` that is the baseline's: the
participants with no restart into a maintenance mode recorded, and any
admission still stopping the first request. From the baseline on it is
the seal's: which acknowledgements are missing. On a fleet with no window it
says so (*no window* and *a window in phase `opened`* have different next steps) and, on a node in a maintenance mode, names any node
still holding an admission before you open one. A window the node couldn't read is a
third answer: the command says so and exits
non-zero; ask again once coordination answers. When the node read the window but couldn't read its
acknowledgements or the admissions beside it, the page says so in their place
(`couldn't read` in the acknowledgement column, and a line saying what it can't
tell), never a dash or an empty list. When it couldn't judge which participants
are retired, it prints none as retired and says it can't tell what the window
waits on.

`exclude` is your assertion that a participant's **process is stopped** and
holds no outstanding request. It is the only thing that waives an
acknowledgement; an eviction does not (that is about whose records
apply, this about whose process is running). The window records it as
yours (`status` prints the participant as `excluded by` you, with the
credential you used), and an `abandon` that leaves the window open is recorded
and printed (`ABANDONED`) the same way. It also withdraws the node's
admission, which gets a window past a node that crashed and will never
boot again (a node that boots again withdraws its last process's admission
itself). Any node can be excluded, participant or not; `-node` names it. An
excluded node is no longer one of the window's participants, and `status`
prints it after them. A running node is refused (one holding a live presence
lease, one whose broker the fleet's metadata group hears, or the node you ask),
as is a window's last participant once it has sent a request, which only that
node's restart or retirement seals, an exclusion past the 16 a window keeps,
and one the window moved under. An exclusion lapses if the node is running,
judged the same way, when the window's next request leaves: the request can
reach it, so the window records it again and its seal waits for it.

`retire` is your assertion, for the whole fleet and every window to come, that
**no process will run under a node id again**: a fleet scaled down, a host
rebuilt under a new id. No window counts the id from then on while it holds no
presence and writes no position row; the node running again under its id
takes part again. A running node (holding a live presence lease, or whose
broker the metadata group hears) is refused, as is the node you ask. One
running only because the group still hears its broker is told so, since a
member that died without stopping is marked offline within four minutes. It
names no log, and is never written into a window: an open
window judges it every time it is read, waiving a participant (printed
`retired`) only while the retirement stands and waiting for it again the
moment the node runs.

`abandon` changes what the operation is trying to reach, never the barrier
it must cross. From `opened` it clears outright, since no request was issued,
and says what's left before service as set-capacity does once it closes a
window. From anywhere else it still crosses the seal, because a paused
coordinator's request is outstanding whether or not a person has read a status
page: every node restarted into `seal` since the request left, which a window
abandoned after it sealed already has. Once the seal holds, set-capacity run
again closes the window at whatever ceiling the log has then, rather than
retrying the target, and the window's `BLOCKED` sentence says which of the
two it is. `-confirm` repeats the operation id from `status`, and the node checks
it against the open operation first: one naming
another operation is refused and nothing changes.

### `crewlet retention reanchor`

```
crewlet retention reanchor -stream CREWLET_TRACKER_LOG
crewlet retention reanchor -stream CREWLET_TRACKER_LOG -confirm 2031-04-02T03:00:00.418226517Z
```

The recovery for a stream that was genuinely recreated, or for a broker
restored from an older copy. Run without `-confirm` it **prints the live
stream's own `created_at`, and which case it is, and refuses**: the
confirmation means *I looked at the thing I am re-anchoring*, which a verb feeding the value straight back would not. Paste the instant back exactly as printed; it is the same one a
`wrong_stream` refusal names as the live stream's: the creation instant
stamped into its configuration (`crewlet.created_at`), which a snapshot
restore keeps.

The three cases are where the log is followed **from**:

- **recreated**: the stream is not the one this node's rows are keyed to, so it
  is followed from its first surviving record;
- **restored**: it is the same stream, ending below this node's checkpoint or
  written past it since, so the rows already hold every record the copy kept
  and it is followed from its end (one below the generation record the verb
  appends, so a record written while it ran is not applied past it either);
  none is applied again. If the log
  also holds records the rows do **not** (written after the restore, by a node
  whose rows were the copy's age), they would be applied on no node, so the
  verb names the newest and refuses unless you pass `-discard`. The
  alternative is to keep them: do not re-anchor this node,
  replace its rows with a peer's instead;
- **abandoned**: it is the same stream and holds every record the rows are
  missing, but it continues in a generation only a peer the fleet has since
  **evicted** held; it is followed from this node's own checkpoint, in the
  generation after the evicted peer's, and every record of the generation it
  skips is void.

A same-stream log that reaches the checkpoint, holding there the record the
checkpoint names, and continues in no abandoned generation is none of these:
the verb prints that there is nothing to re-anchor. The answer names the case and the sequence the checkpoint went to, and,
with `-discard`, the newest record it discarded.

`-discard` is separate from `-force`: `-force` says the
fleet cannot be asked who is most caught up, which says nothing about what a
log holds, and forcing past an unreadable register must not discard writes on
the same keystroke.

It moves **only the log you name**: that domain's checkpoint goes to the next
generation on the live stream (after every generation the domain has
used, an evicted peer's included), every other domain's stays, and
the domain's applier resumes with no restart.

For every log that keeps a generation record (tracker, knowledge
base, identity estate, artifacts log, artifact datastore,
Ask Crewlet, chat, executor log, code log) it refuses while any peer
has already re-anchored the stream, **naming the peer**: that peer's rows are the fleet's
history in the new generation, and this node adopts its snapshot instead. A
peer gone for good is released by evicting it first
([`crewlet retention evict`](../guides/retention.md#eviction)): an evicted peer counts
toward neither this rule nor the next. Otherwise only the most caught-up node
on the stream its rows came from may run it, among the peers holding history
the log does not (a peer whose checkpoint record the log holds is not weighed), and the refusal names the peer further along; `-force`
overrides that rule, and
an unreadable positions register, and never a re-anchored peer. It never
opens a generation another node already opened: if a peer got there first,
the verb reads its record back, names it, and commits nothing.

For a recreated log it does not recover records that were on the old stream
and never applied here, and the refusal says so. The generation record it
publishes names you (your login and the credential you ran it through) on
every node. See
[Re-anchoring a recreated or restored log](../guides/retention.md#re-anchoring-a-recreated-or-restored-log).
### `crewlet retention verify --restore`

```
crewlet retention verify --restore -dir /var/backups/crewlet [-cadence 720h]
```

**The one verb here that talks to no node.** It restores the newest artefact
under `-dir` and opens the copy, to establish that the artefact alone is enough; a running engine
would be the thing under test testing itself.
It writes nothing to the live store and takes no lock on it, so it is safe
beside a running node.

It prints what the artefact holds and **exits non-zero past its cadence**
(default 30 days). Put it in cron.

A directory with no manifest is **debris**, not a partial backup (the manifest
is written last). An artefact naming no domain position cannot be verified,
because a restore replays from that sequence.

See [Retention](../guides/retention.md) for the six terms, the snapshot
repository and the join runbook.

---

## `crewlet schema`

```
crewlet schema [company|bootstrap|issue-report-catalogue] [-o PATH]
```

Prints the JSON Schema for a config tier, `company` (Tier B, the default) or
`bootstrap` (Tier A), to stdout, or to `PATH` with `-o/--output`.

The schema is **generated from the config types the loader uses**, so it cannot
drift from what the engine accepts. Every config type forbids unknown keys, so
it carries `additionalProperties: false` throughout and flags a mistyped field.

It is a **subset** of the validator, never a superset: it expresses structure
(key spaces, types, closed sets, ranges, patterns), not cross-field rules.
Tested invariant: everything the schema rejects, the validator also rejects.

It accepts what the decoder reads, not merely what a field's type suggests; a
test offers every field of both tiers each such value to hold the two to the
same verdict:

- **An empty value** (`~` or `""`) anywhere the engine reads it as unset, which
  is everywhere but the company document's root.
- **A number or a boolean in a text field**, read as the text it was written as
  (`name: 2024`, `org_webhook: false`).
- **YAML 1.1's switch words** in a boolean field: `yes`, `no`, `on`, `off`, `y`
  and `n`, each in three casings (`yes`, `Yes`, `YES`). A quoted `"true"` is
  text, and a boolean field refuses it.
- **A `${VAR}` reference in Tier A** wherever the engine resolves one into a
  value the field can take: anywhere in any text field (a patterned or
  closed-set one included, since the value is judged once resolved), and as the
  **whole** value of a number or boolean field. A reference with other text
  around it in a number or boolean is refused, by schema and engine alike.
- **A Tier B reference as the literal text it is.** The company keeps a
  reference verbatim and resolves it only where a provider or transport is
  built, so a field with a pattern or closed set (an enum, a handle, a unit id)
  refuses one as it would any text that is not a value of it. Exception: a
  field whose consumer resolves a **whole** reference takes exactly one beside
  its own rule; today that is a Mattermost seat's `username`. See
  [Environment Variable References](../getting-started/configuration.md#environment-variable-references).

### Credential positions: `x-crewlet-secret`

Every position that holds a credential carries `"x-crewlet-secret": true`: on
the field for a single value (`integrations.github.webhook_secret`, a
`providers.llm` entry's `auth.api_key`), and on `additionalProperties` for each
value of a map (`mcp_servers[].headers`, a `providers.llm` entry's `env`, a
seat's `sandbox.env`, and both levels down in `mcp_env`, whose values are
per-server maps of variables). A map's **keys** are never marked: they are names
(`GITHUB_TOKEN`, `Authorization`). A position without the keyword holds no
credential; its only value is `true`.

It comes from the same declaration on the config types that drives
[redaction](../concepts/configuration.md#reads-and-export), and a test holds
the two to one set in both directions: in the company schema, a position is
marked exactly when `GET /config`, `crewlet config show` and
`crewlet config export -redact` mask it. The Tier A schema marks its own by the
same rule (the keyring's `secrets.keys[].material`, `api.auth.tokens[].token`,
`stream.token` and `store.objects.s3.secret_access_key`), though no read surface
serves Tier A to redact. `store.objects.s3.access_key_id` is not marked: AWS
treats an access key id as an identifier (it travels in every signed request's
`Authorization` header and appears in CloudTrail records and error messages);
only the secret access key proves possession. A consumer may still write a
`${VAR}` there; the mark only says where a literal is wrong.

It is an **annotation, not a rule**. The engine runs a literal credential in a
marked position (sealed at rest, masked on every read), so the schema accepts
one, and a validator that does not know the keyword ignores it (JSON Schema
2020-12). The policy the engine is built for:

- **Write a whole `${VAR}` reference there, never the value.** In the company
  document the reference is stored verbatim and resolved where the provider or
  transport is built, from the [secret store](../concepts/secret-store.md)
  first, then the environment. A tool that renders a company (a Kubernetes
  operator from its `Secret`s, a CI job from its vault) pushes the value with
  `PUT /secrets/{name}` and writes `${NAME}` into the marked position. In Tier A
  the reference resolves from the node's environment.
- **Treat a literal in a marked position as a finding**: an editor warning, a
  linter failure, or a refusal in tooling that must never hold a credential in a
  versioned document.
- **Mask it when displaying a document**, as the engine's reads do, unless the
  value is exactly one `${VAR}`, which names a credential and carries none.

The mark does not survive being copied into a Kubernetes
CustomResourceDefinition: a CRD's schema accepts only `x-kubernetes-*`
extensions (and no `$ref`), so a generator building one from fragments of these
files must strip `x-crewlet-secret` and keep the policy in its own code.

Both documents are checked into [`schema/`](../../schema/); a test regenerates
and compares them, so a config field added without a schema entry fails the
build.

Point your editor at it for completion, inline field docs, and typo squiggles:

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

Measures the semantic half of knowledge search (the two-stage 1-bit retrieval in
[Knowledge System](../concepts/knowledge-system.md)) against the exact scan it
approximates, **on your own vectors**. The engine's own quality gate measures
the arithmetic on a seeded fixture and claims nothing about recall on your
documents; over embedding-shaped generators the same arithmetic spans 0.29 to
0.98 recall.

**Ground truth is the exact scan's own top-K**, so no judgement is authored. The
queries are held-out documents from the corpus, spread deterministically so two
runs are comparable.

**It reads a file, not a running node.** The running engine exclusively owns the
replicated estate, so pass the copy inside a [backup](../guides/backup.md)
(needs nothing stopped, same rows) or the node's own file with the engine
stopped. A file a running engine holds is refused, naming that way around it.
With neither `-store` nor a reachable Tier A file it has nothing to open. A path
holding no file is refused rather than created, since an empty estate would
report a corpus of nothing.

```console
$ crewlet search eval -store /var/backups/crewlet/2026-09-01/store-replicated.db
corpus       118432 sources, text-embedding-3-large at 3072 dimensions
measured     25 queries at depth 150 from 1200 candidates
first stage  the semantic index: 128 of 1024 lists probed (generation 1099511744562)
index        measured on 118432 sources: 0.9812 against a 0.9800 floor in its worst shape (container:task), 0 head miss(es)
recall       0.9761  (floor 0.9312 for this corpus size)
worst query  0.9467
head misses  0  (documents dropped from the exact top ten)
scan recall  0.9803 with 0 head miss(es), from the same search with the full scan as its first stage
narrowed     source:page      recall 0.9950  floor 0.9800  head misses 0  (25 of 25 scanned)  (scan 0.9950, 0 head miss(es))
narrowed     source:task      recall 0.9772  floor 0.9368  head misses 0  (scan 0.9810, 0 head miss(es))
narrowed     container:task   recall 0.9967  floor 0.9800  head misses 0  (19 of 21 scanned)  (scan 0.9967, 0 head miss(es))
narrowed     container:page   recall 1.0000  floor 0.9800  head misses 0  (4 of 4 scanned)  (scan 1.0000, 0 head miss(es))
verdict      the two-stage search recovers the exact ranking at the shipped depth, in every shape
window       8192 bytes a source (the corpus's opening): a semantic search sees each source's title and body up to it, a keyword search the whole body
past window  task  2110 of 104208 sources (2.0%), 7.4 MiB of 196.0 MiB of text (3.8%)
past window  page  9874 of 14224 sources (69.4%), 402.1 MiB of 518.6 MiB of text (77.5%)
```

The last three lines print on every run: per corpus, how many sources and how
much text lie past the window each vector was computed from, which a search by
meaning cannot see and a keyword search still reads
([Knowledge System](../concepts/knowledge-system.md#what-the-quality-of-this-can-and-cannot-be-promised)).
That part reads every source's whole body.

| Flag | Default | What it does |
|---|---|---|
| `-store PATH` | from `-config` | The replicated database to measure. Overrides the Tier A file |
| `-config PATH` | `./crewlet.yaml` | Tier A, read only for the replicated store's path |
| `-queries N` | `25` | Held-out documents to measure over; each is a full exact scan, which bounds the run |
| `-limit N` | `150` | The depth recall is measured at (the shipped returned depth) |
| `-candidates N` | `1200` | The stage-1 candidate depth (the shipped pair) |
| `-model NAME` | most populated | The embedding model to measure |
| `-dimensions N` | the model's | The width to measure |
| `-probes N` | the index's own | Lists of the semantic index to probe. `0` takes the count the index's own training measured; larger shows what probing more would recall |
| `-window BYTES` | the smaller of 8 192 and the model's own per-input bound | Bytes of each source the vectors were computed from, for the past-window report. The store does not carry the company's configuration, so state it when `providers.embeddings.max_input_tokens` or `max_batch_tokens` lowers the bound below the model's own (the embedding duty embeds the smaller of 8 192 bytes and that bound) |
| `-fit` | off | Also print the corpus's mean pairwise cosine, the parameter the seeded fixture is fitted from |
| `-metrics` | off | Print one `key value` line per metric instead of a report, window report included, as `search_eval_window_bytes` and, labelled by `source`, `search_eval_window_sources`, `search_eval_window_beyond_sources`, `search_eval_window_text_bytes` and `search_eval_window_beyond_bytes` |

**It exits non-zero** when recall is below the floor for that corpus size, or
when any document was dropped from the exact top ten, in any shape, so it can go
in a schedule. Both matter: an aggregate of 0.98 can hide losing exactly the
documents that mattered, and a semantic-only document the first stage drops
leaves the fused answer entirely.

Run it **monthly, and after any change to `providers.embeddings.model` or
`providers.embeddings.dimensions`**. A report naming more than one embedding
space is a refill in progress: until it finishes, documents still on the old
model are not in the candidate pool, because the scan filters on the
model/width pair.

Below the floor, the remedy, in order: raise the quantization over-fetch
(free in latency: stage one is a full scan whose cost does not depend on how
many candidates it keeps), then an 8-bit first stage, which ships in the same
release that moves the model default.

---

## `crewlet assistant eval`

```
crewlet assistant eval -suite DIR [-model KEY -paid] [-trials N] [-company PATH] [-config PATH] [-api URL]
```

Runs a directory of evaluation cases through
[Ask Crewlet](../concepts/assistant.md)'s own orchestrator and tool catalogue,
in this process, and reports which cases pass. No case is asked of a running
node: each describes the company it is asked in, as the answers its requests
receive.

**Two ways to run the same cases.**

- With no `-model`, every case plays its own `script` (the rounds a scripted
  model answers with): no model is called and the run is deterministic. The
  engine's tests run the shipped suite this way, so a change that breaks a case
  fails CI.
- With `-model KEY -paid`, the script is ignored and the question is put to that
  `providers.llm` entry of the company at `-company`, through its CLI, as a turn
  would run on it. This calls a paid model once per round of every trial, so
  `-model` without `-paid` is refused before anything is read, and the run says
  on stderr which model it is asking. This is how a model is measured before
  it is assigned as `assistant.model`
  ([Running Ask Crewlet](../guides/assistant.md#choosing-the-model)). The entry
  runs as Ask Crewlet would run it, the harness's own web tools off and no tool
  beside the engine's; an entry on a harness that cannot be run that way is
  refused (`crewlet llm doctor KEY` probes that run first). Only the entry's own
  `${VAR}` references resolve, as `crewlet llm` resolves them: through the store
  `-config` names or the node `-api` names, the environment behind it (see
  [where a command reads the company's values](#where-a-command-reads-the-companys-values)).

**What it does not measure.** Cases run against seeded answers, not the
engine's API, so they say nothing about permissions (that Ask Crewlet reads and
is refused exactly what the person is, and writes nothing they did not approve,
is held by the engine's test suites). An evaluation measures what a model does
with the tools it is offered: which it calls, with what arguments, what it
proposes, and what it says.

```console
$ crewlet assistant eval -suite examples/assistant-evals -model assistant -paid -trials 3
case                  pass  rounds  tokens  p50 round
file-a-task           3/3   1       3988    5.8s
find-a-task           3/3   2       4210    6.1s
injected-instruction  2/3   2       4410    6.4s
injected-instruction, trial 2: never: update_task called
prompt assistant.prompt.v1, model assistant (sonnet), pass^3 2/3 cases
```

Each line gives the trials that passed and the mean rounds and tokens and median
round latency across them; under the table, each failed trial is listed with its
case and the expectation it missed. The footer names the prompt version, since a
result belongs to the prompt as much as to the model.

It **exits non-zero** when any case fails any trial.

| Flag | Default | What it does |
|---|---|---|
| `-suite DIR` | required | The directory of cases: every `*.yaml` file in it, in name order |
| `-model KEY` | none: each case's script | The `providers.llm` entry to ask instead of the scripts |
| `-paid` | off | Confirms that `-model` calls a paid model; required with it, refused without it |
| `-trials N` | `1` | Times each case is run. A case passes only when every trial passes (pass^k), since a person meets every trial |
| `-company PATH` | `company.yaml` | Tier B, read with `-model` for the entry to ask, which must satisfy Ask Crewlet's rules |
| `-config PATH` | `crewlet.yaml` | Tier A, read with `-model` to resolve the entry's `${VAR}` references as every command does |

### The case format

One case per file:

```yaml
name: spend-by-agent              # default: the file's name
person:
  apps: [worklet]                 # admin, worklet, doclet, artifacts, chatlet, gitlet, atlas
  acts: [create_work_item]        # the act tools they may propose; default none
page:                             # optional: the page chip, as the dashboard sends it
  screen: spend
  title: Spend
  source_revision: spend@1
question: Who spent the most tokens this week?
search: true                      # optional: the model can search the web (search_web)
mode: ask                         # ask (default) or brief, which needs the page
answers:                          # what each request Ask Crewlet sends answers
  - route: GET /tokens/breakdown  # a route pattern, {name} matching one segment
    status: 200                   # default 200
    body: {rows: [{seat: ada, tokens: 51200}, {seat: grace, tokens: 1200}]}
script:                           # the scripted model's rounds; ignored with -model
  - call: get_spend
    args: {window: week}
  - say: Ada spent the most this week, 51 200 tokens.
expect:
  calls: [get_spend]              # tools called, in this order (others may come between)
  never: [create_task]            # tools that must not be called
  proposals: 0                    # how many proposals the turn ends with
  state: completed                # how the turn ends
  says: [ada]                     # text the answer contains, ignoring case
  never_says: ["http"]            # text it must not contain, ignoring case
  shows: [Spend by agent]         # text the turn's cards carry, ignoring case
  never_shows: [Budgets]          # text no card of the turn carries, ignoring case
  brief:                          # a brief turn's card (mode: brief only)
    needs: [ENG-42, qa]           # the records its rows cite, in order; [] is "Nothing needs you"
    tones: [warning, danger]      # optional: each row's tone, in the same order
    verdict: 2 things need you    # optional: the verdict the engine counted from the rows it kept
  refusals:                       # what each refused call's answer names, in order
    - [about, "needs[0].do"]      # judged against the scripted model, which hears every answer
  taint: none                     # the outside surface every proposal names (executor, page_body, ...), or none
```

- A case is read by the configuration's strict reader: an unknown member, or a
  tool the catalogue lacks, is refused naming the file, path and line, so a
  misspelling never leaves an expectation unchecked. The page chip takes the
  wire's member names (`source_revision`). The runner is
  `internal/assistant/evals`, the harness the engine's suites drive.
- A request no `answers` entry matches is answered `404 not_found` in the
  engine's error envelope.
- Every `expect` field is optional, but a case with none is refused: a case that
  cannot fail measures nothing.
- A write the model calls is never sent: it becomes a proposal, as in the
  dashboard, which `proposals` counts. `step_up` holds every proposal to
  whether the engine asks the person to confirm it is them before approval
  (approving an agent's grant request does), failing a turn that ends with none.
- `shows` reads the cards the turn drew (a table's title and cells, a
  proposal's rows and note), so a case can hold what the person was shown, such
  as the place a page is proposed in or the reason a read card gives;
  `never_shows` holds what no card may carry, such as an option row on an answer
  given in the person's own words.
- A `mode: brief` case runs a brief turn (the drawer's "Brief me on this
  page"), whose script ends with an `answer_brief` call. `brief` holds the card
  the engine drew from it, after the engine dropped every row whose record the
  turn did not read or the person may not open, and drew a danger tone only
  where the record's own read carried the fact. Its `verdict` is counted from
  the surviving rows, never written by the model.

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
`providers.llm` entry drives a vendor's own harness — on the operator's
subscription plan, or on an API key under `auth.mode: api-key` — and the login
that makes a subscription work is established here rather than in the config
document. `KEY` is the `providers.llm` key; commands that take one and are
given none act on the only entry when there is exactly one, and `doctor` given
none examines every entry, in the order the document declares them.

**`list`** is the inventory — provider key, harness, model and how it is signed
in. **`doctor`** is the one to run before a company's first turn: it
checks the harness is installed, that something signs it in, and — unless you pass
`-no-smoke` — that a real run reaches the seat's tools: one tool,
`crewlet_smoke`, served on a real tool session as a phase's are, which the harness
is told of by its profile's `mcp` block, starts this binary as
[`mcp-relay`](#crewlet-mcp-relay) to reach, and is asked to call the way a phase
asks, nothing forcing the call. It passes when the session recorded the call
and the harness's record showed the request that made it, and says how the call was
joined to that record and which tools the harness
offered its model — where a confinement that stopped holding shows first. It is
the only check that catches a plan whose quota is exhausted, or a profile whose
tool route, record or `text_paths` no longer match what the harness does. The same run measures two claims the
profile makes rather than trusting them: that the harness's own **shell is
refused** (every phase but a seat's executor runs on the engine host — the
reviewer, a worker, a side pass — so a denial that stopped working is such a
phase reading whatever the engine user can read) and that its **web tool
reaches the network** (the one local tool every profile deliberately keeps
on). Both are believed only on evidence a model cannot invent — the current
clock, read by the tool. Its `system prompt` lines say where a seat's system
prompt goes and where a side pass's does — added to the harness's own, in place of
it, or in the transcript under it ([which channel a call
takes](../concepts/llm-backends.md#which-channel-a-call-takes)) — and every probe
is sent as a seat's call, so it runs under the prompt a seat does.

For an entry some agent seat's executor chain holds, `doctor` also prints
`executor` lines: the seats that launch on it; whether its harness has an in-box
runner — only `claude-code`, `codex`, `gemini-cli`, `qwen-code`, `opencode`, `copilot`, `grok`, `muse-code`, `kimi-code`, `hermes` and `pi` do, and another harness is skipped at
launch and serves those seats' phases on this host only, which is a line and
not a problem; and the tool bridge its box dials — for a `direct` cell this
node's own listener on loopback where the node binds `api.port`, whatever its
roles, and for any other cell `CREWLET_MCP_BRIDGE_URL`, whose absence there is
a problem, since
every launch on that entry would be refused
([The executor's run in a box](../concepts/llm-backends.md#the-executors-run-in-a-box)).
Where its runner names a release floor (Codex's 0.171.0), a `direct` cell's
harness, this host's own, below it, naming no release (a pre-release, which a
run refuses too), or failing its runner's probe (`codex app-server --help`), is
a problem, and a newer one a line saying its runs start fresh sessions; any other cell's box holds its own harness to the floor at each
run, which is a line.

Both commands answer "signed in" the same way, from the environment the harness is
actually given rather than from the configuration: credential files a login
wrote (or, for OpenCode 2, which keeps its login in its own database, the
document its export printed), a headless token, the key `auth.mode: api-key`
puts in the harness's key variable (`auth.api_key`, or that variable itself —
`ANTHROPIC_API_KEY` for Claude Code, `CODEX_API_KEY` for Codex; Qwen Code's
`OPENAI_API_KEY`, which other vendors' keys share, only through `auth.api_key`), a
variable `auth.mode: inherit-env` forwards, or — for a harness whose profile reads
its provider's key from its own environment (`hermes`, `pi`, `opencode`) — a
[credential-named](../concepts/llm-backends.md#6-a-provider-key-in-env-opencode-hermes-pi)
variable in the entry's `env`. `doctor`'s `sign-in` line names every one it found, and
an entry with none is a `no sign-in` problem naming the routes its `auth.mode`
reads ([how `doctor` judges a
sign-in](../concepts/llm-backends.md#how-doctor-judges-a-sign-in)): a login or a
headless token under `subscription`, only the key under `api-key`, and the
variables it forwards or a login under `inherit-env` — unless
the smoke test was answered, which proves the harness authenticates some way the
engine does not hand it (an endpoint that takes no key, a credential in its
own configuration). A token `auth.mode: api-key` removes from the child is not
counted. A token or key variable the entry's `overrides` names outside the
reference grammar (`token_env: MY-TOKEN`) is a problem of its own, naming
`providers.llm.<key>.overrides.token_env` or `.api_key_env`, and no route
through it is named: no `-capture-token`, and no key or token to store or
export under it. `${VAR}` references, and the conventional variables an entry
falls back to, are resolved in the process running `crewlet llm` — the secret
store first, then that shell's environment — while the variables `inherit-env`
forwards come from that shell's environment alone, as the engine forwards its
own and never the store's; so run it with the engine's environment to get the
engine's answer. Beside a running engine the store's values are read through
it, as every command that reads the company reads them ([Where a command reads
the company's values](#where-a-command-reads-the-companys-values)), so `crewlet
llm` runs while the seats do — revealing, at most 64 to a request, only what
the entries it works on read that the node holds (the one entry a key names, or
every entry for `list` and a bare `doctor`), and never a variable no store can
hold, such as an `overrides.token_env` outside the reference grammar — with one
of Tier A's tokens in `CREWLET_API_TOKEN` when one of those is stored. With the
engine stopped it
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
by `auth.mode: inherit-env`), `env` (the entry's `env` under `auth.mode: env`: a
cloud provider's switch, a gateway's token, or a provider's key), `environment`
(a provider's key in the entry's `env` under any other mode), or `none`,
and `unknown` where nothing else signs the entry in and its credential
directory holds a [login write that cannot be
finished](../concepts/llm-backends.md#a-login-of-several-files-is-written-whole),
which `doctor` reports as a problem naming why.

`doctor` exits non-zero when any entry has a problem, and an entry that does
not build at all (a `${VAR}` model that resolved to nothing, say) is reported
as one beside the others rather than ending the run. A configured credential
bundle that does not restore is a problem of its own, naming `crewlet llm
export` as the fix, since the engine would run that entry signed out. It also
reads the URLs a box dials back to the engine (`CREWLET_MCP_BRIDGE_URL`,
`CREWLET_SANDBOX_OTEL_RECEIVER_URL`) exactly as `crewlet run` reads them at
boot: one the engine would refuse to boot on is reported first, as the node's,
and fails the run whatever cell (`run_in`) each entry runs in, since a node
that does not boot runs no seat.

**`login`** has four shapes because the vendors do:

| Shape | When |
|---|---|
| *(no flag)* | Broker the vendor's own interactive login and keep the result — written **whole** over whatever landed while it ran, so a file the sign-in left as it was goes back as it was and a file it never held is removed rather than kept beside the new token |
| `-from-host` | Adopt a login this machine already has, e.g. from running the harness by hand — copied for a login kept in files, exported by the harness itself (`opencode auth export --standalone`) for one kept in its own database. The adopted login replaces the old one **whole**: a file your home does not hold is removed rather than left beside the adopted ones |
| `-capture-token` | Mint a **headless token** into the secret store: one scoped, revocable variable every node of the fleet reads from the shared store, with no refresh token for them to share ([Capture a headless token](../concepts/llm-backends.md#2-capture-a-headless-token-best-where-it-exists)). Stored through a running engine, it is read by the fleet at its next configuration apply, and the command says so, naming `POST /config/reload`; stored with the engine stopped, it goes into the fleet's store directly, where this node's engine reads it when it starts and a node already running on that store at its next configuration apply. It is read under the profile's token variable by convention, and the command prints the `auth.token` snippet for naming it in the company document instead |
| `-capture-token -print-token` | The same mint, written to **stdout** and stored nowhere — for an operator whose secrets live in somebody else's manager. It **refuses to run on a terminal**: this is a credential, and a token in a scrollback outlives the command, while a screen-share or a shell history outlives the scrollback. Pipe it or redirect it. The two-step alternative (`-capture-token`, then `secrets get -reveal`) writes the token into the store on the way past, which is precisely what this avoids. |
| `-token-stdin` / `-username U -password-stdin` | Store a token you already hold (`-token-stdin`), or sign the harness in with a username and password (`-password-stdin`), each where the harness genuinely has that kind of login. A token piped to `-token-stdin` is trimmed of the whitespace around it and held to the [64 KiB every store keeps](../concepts/secret-store.md#the-design): the pipe is read no further than that, so one pointed at the wrong file, or one that never ends, is refused rather than read whole. A password piped to `-password-stdin` is never stored, so it is held to a password's bound instead, 4 KiB without its line ending — the most the engine's own sign-in carries, and an order of magnitude above any passphrase — and a longer one is refused the same way, before the harness's login runs |

A headless token is read only under `auth.mode: subscription`, so `-capture-token`
and `-token-stdin` are **refused before anything is minted, read or stored** for
an entry whose mode would never read one, naming what that mode reads instead —
the words `doctor` uses. Under `api-key` the harness's token variable is removed from
its environment, so the error names the key: `auth.api_key`, else the harness's key
variable, stored with `crewlet secrets set` or exported. Under `inherit-env` the
token variable is forwarded from the engine's own environment and the secret
store is never read, so storing a token is refused while `-capture-token
-print-token` is not: export what it prints as that variable in the engine's
environment, which is what the line it ends with says. `-capture-token` and
`-token-stdin` are also refused before anything is minted, read or stored, in
every mode and with `-print-token` too, for an entry no token variable hands
a token to: one whose harness names none (every built-in profile but
`claude-code`), and one whose `overrides.token_env` is not an environment
variable name (`MY-TOKEN`), under which no store, `${VAR}` or export can carry
a token. The error names `providers.llm.<key>.overrides.token_env` — except
under `api-key`, where it is that mode's refusal above, naming the key, with the
field beside it for a name outside the grammar and without `auth.mode:
subscription`, under which such an entry could read no token either — except
an entry whose harness names no key variable, a pairing every write refuses: it is
told that the mode puts its key in no variable, and to set `auth.mode:
subscription` and sign the harness in with a login. Every write of the company
document refuses such an override, but `crewlet llm` loads the document as a
run does, without the rules only a write applies. `-from-host` recommends
`-capture-token` after it runs only under `subscription`, and only where the
harness has a minting command and a token variable a store can hold.

The brokered and `-username` shapes **fail when the harness left no login** where
the entry reads one — exiting 0 having stored nothing, as backing out of the
vendor's sign-in does — rather than printing that a login was written. `login`,
`status` and a vendor's own `logout` run the harness one at a time per state
directory and harness (entries sharing a `state_dir` and driving one harness share one
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
`login` likewise replaces a shared login the harness refuses to import (OpenCode's
document from another build, say), saying so, where `status` keeps refusing it.

**`export`** packs a login into one portable blob so another host can come up
already authenticated: the entry's credential files, read whole so a bundle
never packs a write that stopped part way, and a manifest naming the harness they
were exported from (`crewlet-bundle.json`). `-secret-store` writes it to the
[secret store](../concepts/secret-store.md) instead of stdout, under
`CREWLET_LLM_CLI_<KEY>_CREDENTIALS` — through the running engine, where every
node restores it when it next builds the company (at boot and at every apply;
the command says so) — and, with the engine stopped, into the fleet's store
directly, by the route `export` read the company through. A bundle is stored only if it fits the [64 KiB every store
keeps](../concepts/secret-store.md#the-design) a value to: a longer one is
refused before anything is written, and the error names what carries it
instead — the bundle on stdout, kept in your own secret manager and piped to
`crewlet llm import` on each host, or a login on each host. Without it the
blob goes to stdout in the clear, which is what you want when piping into your
own secret manager and never what you want in a shell history.
Every node restores a stored bundle when it builds a company to run, at boot
and at every configuration apply, into a credentials directory that is still
empty.

A token `login` stores and a bundle `export -secret-store` stores each say,
as [`crewlet secrets set`](#crewlet-secrets) does, which store took the value
and who reads it when: through a running node, the running fleet at its next
configuration apply; with this node's engine stopped, that engine when it
starts and a node already running on the same store at its next configuration
apply.

**`import`** is the other half, and it reads the bundle from **stdin** — a
credential on argv is visible in `ps` and lands in shell history, and the
natural spelling is a pipe anyway:

```bash
crewlet llm export claude | ssh other-host crewlet llm import claude
```

It **refuses to overwrite an existing login**, loudly: a host that has been
running holds the fresher refresh token, and restoring a boot-time blob over
it is how a fleet logs itself out. `crewlet llm logout <KEY>` first if you
mean to replace it. And it refuses a bundle that does not name the entry's harness
as the one it was exported from (below), writing nothing.

**`logout`** removes the login (and revokes it with the vendor where the harness
has a logout of its own) in one write of the login less every file of it that
holds one — so a logout killed part way never leaves one file of the login it
was removing, and a settings file the harness keeps beside it, holding none, stays
— and
**records the logout** in the harness's share of the state directory
(`<state_dir>/<harness>/logged-out`), so the configured bundle below is not
restored into the directory it emptied —
not by the engine's next boot or apply, and not by `list`, `status`, `export`
or `doctor`, which each say why instead — until a login is written into that
directory again: `login` (brokered or `-username`), `-from-host` or `import`. A
headless token (`-token-stdin`, or the one `-capture-token` mints, unless its
sign-in also left a login file) signs the entry in without ending the logout,
where the entry takes one — and `logout` names one only there: under a token
variable a store can hold, stored under `subscription` or exported in the
engine's environment under `inherit-env`, never under `api-key`, and
`-capture-token` only where the harness mints it. A
fresh state directory holds no such record and restores the bundle as before,
so `logout` names the field or secret that retires it when the entry has one —
even when the harness's own logout fails, since the login is removed and the logout
recorded either way. For the conventional variable that is `crewlet secrets
unset CREWLET_LLM_CLI_<KEY>_CREDENTIALS`, with the variable removed from the
engine's environment too ([`crewlet secrets`](#crewlet-secrets)).

The engine also restores a bundle **itself**, at boot and at every apply it
publishes (never for a revision it refuses), from
`auth.credential_bundle` or the conventional
`CREWLET_LLM_CLI_<KEY>_CREDENTIALS` variable: into a credential directory
holding no login, or over a login installed before the bundle's, since the
newest login wins by the install record a bundle carries ([the newest login
wins](../concepts/llm-backends.md#the-newest-login-wins)), and never after a
`logout` until a login is written again. `import` never replaces a login, as
above. `list`, `status`, `export` and `doctor` make
the same restore before they look, so they report the login the engine would
run with; `validate` never writes one. That path authenticates a fresh
container before its first turn rather than after an operator remembers a
command; `import` is for the hosts that are already up. See [environment
variables](environment-variables.md).

A bundle **belongs to the harness it was exported from**, and names it, though it
is keyed by the entry: one exported before the entry moved to another harness
names the old harness, so a restore into the new harness's empty directory refuses it
and writes nothing — even where the two harnesses keep a login file under one name,
as six keep an `auth.json` — at boot and apply as
`harness_bundle_restore_failed`, in `list`, `status`, `export` and `doctor`,
and in `import`, until the new harness is signed in and its own bundle exported in
its place. A bundle that names no harness is refused the same way: see [an entry
moved to another harness](../concepts/llm-backends.md#an-entry-moved-to-another-harness).

---

## `crewlet mcp-relay`

```
crewlet mcp-relay <absolute path of a socket>
```

**You never run this.** It is the one MCP server a seat's harness is told to
start, by the engine that runs the harness: the harness speaks MCP over the relay's
stdin and stdout, and the relay copies those bytes to a unix socket the engine
opened for that one harness run, and the engine's answers back. Every tool the seat
calls goes through it to the engine's own dispatch, with the permission model
and the record a tool call always has. See [LLM Backends](../concepts/llm-backends.md).

- **It reads its one argument and nothing else** — no environment, no config
  file, no logging setting. A harness decides what environment its MCP servers start
  in, and the harnesses disagree, so nothing the relay depended on there could be
  relied on. It logs nothing either: its stdout is the protocol stream.
- **The socket path is not a credential.** The socket sits in a directory the
  engine created for itself with mode `0700`, so only the engine's own user can
  connect, and the engine removes it when the harness run ends.
- **It ends when the engine closes the socket**, and when the harness closes its
  stdin it half-closes, so the answer to a call already sent still arrives.
- It cannot reach a socket? It prints one line on stderr naming the path, and
  exits 1 — which the harness reports as an MCP server that failed to start.

## `crewlet mattermost provision` (retired)

Each agent's Mattermost bot, its roles, its channels and its token are managed
from the Mattermost integration's **Agents** tab and applied by the reconcile
loop, which seals each bot's token under the name the engine derives for the
seat. The command answers with that and changes nothing. To check an install,
use [`crewlet mattermost doctor`](#crewlet-mattermost-doctor); see
[Each agent's access](../integrations/mattermost.md#each-agents-access-roles-and-channels).

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
| `-api URL` | The node to read the company's stored values through — a node that holds data, from a `seats` node, a member of a cluster whose engine is stopped, or a machine that is not a node. Default: this node's engine when it runs, and the fleet's store itself when it is stopped ([Where a command reads the company's values](#where-a-command-reads-the-companys-values)). |

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
                                        [-public-url URL] [-rotate] [-dry-run]
                                        [-config PATH] [-api URL]
```

Idempotent reconcile of the company-level half of GitLab: it checks `provisioning.group`, warns when the provisioning token is about to expire, and registers the [webhook](../integrations/gitlab.md#webhooks) with its signing secret. It mints nothing but a signing secret. Each agent's service account, role and token are [per-agent access](../integrations/gitlab.md#per-agent-access), set on the integration's Agents tab and applied by the engine.

**A dry run says what it would do to the signing secret.** Replacing the key a working hook signs with fails every delivery in flight until the new value reaches the engine, so the plan states which of *untouched* / *reused* / *minted* / *rotated* will happen, and into which `${VAR}`. That decision is made by the same function the real run uses, on the stored value, so the two cannot disagree.

**A run that recorded something says what is left to do**, on a `Next:` line: `-env-file` needs sourcing and a restart, `-print` needs the value moved before the terminal closes, and `-secret-store` needs the current revision re-activated on a running node (`POST /config/reload`). A run that changed nothing prints no follow-up.

| Flag | Description |
|------|-------------|
| `-admin-token` | The provisioning credential: a top-level group **Owner** PAT with `api` scope on GitLab.com, or an administrator's PAT on a self-managed instance. Falls back to `$GITLAB_ADMIN_TOKEN` |
| `-secret-store` / `-env-file PATH` / `-print` | Where a minted signing secret goes: exactly one, and there is no default |
| `-public-url` | This deployment's public base URL; the webhook is registered at `<url>/webhooks/gitlab`. Overrides the Tier A webhook address. Omitted with no Tier A address, no hook is registered and the run says so |
| `-rotate` | Mint a fresh webhook signing secret into the variable `signing_secret` names, wherever the hook is registered. A seat's token is rotated from the Agents tab |
| `-dry-run` | Print the plan and touch nothing. It is the **same** plan the run uses |
| `-config PATH` | Tier A config naming this node's store and keyring (default `./crewlet.yaml`) |
| `-api URL` | The node to read the company's values through and record into |

See [GitLab Integration: Provisioning](../integrations/gitlab.md#provisioning) for the permission matrix and the full walkthrough.

## `crewlet jira provision`

```
crewlet jira provision <company.yaml> [-secret-store | -env-file PATH | -print]
                                      [-public-url URL]
                                      [-recreate-webhook] [-dry-run]
                                      [-config PATH] [-api URL]
```

The Atlassian tracker's reconcile, a different shape from its peers: **Jira issues no credentials on a provisioner's behalf.** A Cloud API token is created by its owner at Atlassian's account site, and a Data Center personal access token can only be minted for the calling user. The command does the three things Jira allows, each answering a question otherwise invisible until an issue reaches nobody:

1. **Which account each seat's credential authenticates as.** It calls `/myself` with every seat's own token, read from `mcp_env.atlassian` or `mcp_env.jira`, and reports the seats that resolved and those that did not. A seat with no account id receives **no Jira events at all** (the routing gate drops every target naming it) and nothing else in the engine says so. Human seats are never probed: they hold no tool credential and are reached by `contact.atlassian_account_id` or by email.
2. **Whether every project the org chart names exists**, and whether Jira's project lead agrees with the org chart's. A typo in a key is a routing gap with no error anywhere; a lead disagreement is reported and never failed, since a human manager owning the project while an agent triages it is ordinary.
3. **The inbound webhook**, on Data Center only, at `<public-url>/webhooks/jira`, subscribed to exactly the events the parser routes, delivering the whole body, signed with an HMAC secret. On **Cloud** the step is skipped with a note: a dynamic webhook there belongs to an app, so the endpoint refuses an API token however privileged, and reporting that 403 would send an operator to rotate a fine credential. Cloud events arrive through the [Forge app](../integrations/jira.md#jira-cloud--the-forge-app-optional) instead.

**A working webhook secret is never replaced.** If `integrations.jira.webhook_secret` already resolves, that value is registered as-is; minting every run would be an outage, since the engine holds the *old* secret and the instance would sign every delivery with a key nothing can verify. A secret that resolves to nothing is minted into the `${VAR}` the config points at and recorded in the sink you chose. `-recreate-webhook` forces a rotation, which invalidates the secret every other deployment of this company holds.

| Flag | Description |
|------|-------------|
| `-secret-store` / `-env-file PATH` / `-print` | Where a minted webhook secret goes: exactly one, no default. See [the secret store](../concepts/secret-store.md). With `-env-file` or `-print`, a real run that would mint the secret (it resolves to nothing, or `-recreate-webhook` asks for a fresh one) is refused before it reaches Jira where the company's store holds its `${VAR}` ([A sink the store outranks](#a-sink-the-store-outranks)). |
| `-public-url` | This deployment's public base URL; the webhook is registered at `<url>/webhooks/jira`. **Overrides the Tier A webhook address (`api.public.external_url` when the file sets a [public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api), `api.external_url` otherwise) for this run.** Registration is skipped only when both are empty: a hook pointing at the wrong host is worse than none, since the instance reports a healthy integration that delivers into the void. |
| `-recreate-webhook` | Delete and remake the hook to mint a fresh secret. The only recovery for a lost secret, since the value cannot be read back off the hook, and destructive for every other deployment holding the old one. A literal `webhook_secret` has no variable to mint into, so the run is refused: point the field at a whole `${VAR}`, or run without the flag to register the literal as it is. It reads the company's values by the same route as any run. |
| `-dry-run` | Read the instance and report; register nothing. No sink is opened, so it needs none of `-secret-store`, `-env-file` and `-print`, and an env file it names that does not exist is not created. It reads the company's values as a real run does. |
| `-config PATH` | Tier A config naming this node's store and keyring (default `./crewlet.yaml`): where the run reads the company's values and where `-secret-store` records ([Where a command reads the company's values](#where-a-command-reads-the-companys-values)). |
| `-api URL` | The node to read the company's values through and record into: a node that holds data, named from a `seats` node, a member of a cluster whose engine is stopped, or a machine that is not a node. Default: this node's engine when it runs, the fleet's store itself when stopped ([Where a command reads the company's values](#where-a-command-reads-the-companys-values)). |

A hook somebody else registered is reported and never touched.

The run refuses outright when the org credential in `integrations.jira.token` is rejected, since nothing else it reported would be trustworthy.

See [Jira Integration — Provisioning](../integrations/jira.md#provisioning) for the full walkthrough.
## `crewlet github provision`

```
crewlet github provision <company.yaml> [-secret-store | -env-file PATH | -print]
                                        [-public-url URL]
                                        [-recreate-webhooks] [-dry-run]
                                        [-config PATH] [-api URL]
```

Same shape as Jira's reconcile, for the same reason: **GitHub issues no credentials on a provisioner's behalf** (no API creates a user; the token-minting API was withdrawn in 2020). It does the two things GitHub allows:

1. **Which account each seat's credential authenticates as.** It calls `GET /user` with every seat's own token, read from `mcp_env.github` under whichever key the seat's tools use (`GITHUB_TOKEN`, `GITHUB_PERSONAL_ACCESS_TOKEN`, `GH_TOKEN`, or an `Authorization` header), and reports the seats that resolved and those that did not. A seat with no login receives **no GitHub events at all** (the routing gate drops every target naming it), and nothing else says so. Human seats are never probed: they hold no tool credential and are reached by `contact.github_login`.
2. **The inbound webhooks**, at `<public-url>/webhooks/github`, subscribed to exactly the events the parser routes and delivering JSON (GitHub's default is form-encoded, which nothing here decodes). One hook on the **organization** where the credential may register it, covering every repository including later ones; otherwise one per repository in `provisioning.repos`. `org_webhook: true` makes a credential that cannot register an org hook a failed run, not a silent fallback. The scope is `admin:org_hook`, which a fine-grained token cannot carry.

**A working webhook secret is never replaced.** If `integrations.github.webhook_secret` already resolves, that value is registered as-is: minting every run would be an outage, as the engine holds the *old* secret and GitHub would sign deliveries with a key nothing can verify. A secret that resolves to nothing is minted into the `${VAR}` the config points at and recorded in the chosen sink. `-recreate-webhooks` forces a rotation, which invalidates the secret every other deployment of this company holds.

**One unhookable repository does not stop the rest.** A repository that was renamed, archived, or made private to a team the credential is not in is reported with what is wrong; the others are still hooked. GitHub answers **404 for both "absent" and "invisible to this credential"**, so the report says both.

| Flag | Description |
|------|-------------|
| `-secret-store` / `-env-file PATH` / `-print` | Where a minted webhook secret goes: exactly one, no default. See [the secret store](../concepts/secret-store.md). With `-env-file` or `-print`, a real run that would mint the secret (it resolves to nothing, or `-recreate-webhooks` asks for a fresh one) is refused before reaching GitHub where the company's store holds its `${VAR}` ([A sink the store outranks](#a-sink-the-store-outranks)). |
| `-public-url` | This deployment's public base URL; hooks register at `<url>/webhooks/github`. **Overrides the Tier A webhook address (`api.public.external_url` when the file sets a [public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api), `api.external_url` otherwise) for this run**; registration is skipped only when both are empty, since a hook pointing at the wrong host reports a healthy integration that delivers into the void. |
| `-recreate-webhooks` | Delete and remake every hook to mint a fresh secret. The only recovery for a lost secret (it cannot be read back off a hook), and destructive for every other deployment holding the old one. A literal `webhook_secret` has no variable to mint into, so the run is refused: point the field at a whole `${VAR}`, or run without the flag to register the literal as is. Reads the company's values like any other run, by the same route. |
| `-dry-run` | Read GitHub and report; register nothing. No sink is opened, so none of `-secret-store`, `-env-file` and `-print` is needed, and an env file it names that does not exist is not created. Reads the company's values as a real run does. |
| `-config PATH` | Tier A config naming this node's store and keyring (default `./crewlet.yaml`): where the run reads the company's values and where `-secret-store` records ([Where a command reads the company's values](#where-a-command-reads-the-companys-values)). |
| `-api URL` | The node to read the company's values through and record into: a node that holds data, named from a `seats` node, a member of a cluster whose engine is stopped, or a machine that is not a node. Default: this node's engine when it runs, the fleet's store itself when it is stopped ([Where a command reads the company's values](#where-a-command-reads-the-companys-values)). |

A hook somebody else registered is left alone: matching is on the **delivery URL**, never a name (GitHub webhooks have none, and an organization carries other integrations' hooks).

The run refuses outright when `integrations.github.token` is rejected, as nothing else it reported would be trustworthy. That token is optional for the *engine* (without it, thread activity degrades to the payload's author and assignees) but required here: registering a webhook has no degraded form.

See [GitHub Integration — Provisioning](../integrations/github.md#provisioning--crewlet-github-provision) for the full walkthrough.

## `crewlet slack provision` (retired)

The reconcile loop creates and maintains each agent's Slack app, with the app configuration token entered once on Slack's connect form. A person approves each install with **Install** on the Slack integration's **Agents** tab, after which the engine seals the bot token under the name it derives for the seat. The command says this and changes nothing.

An app the command made is adopted by its working bot token, not created again. Its OAuth client stayed unsealed in the command's ledger (`slack-apps.json`), so to let such an app be installed again (after it is turned off, or for a rotate), copy its `client_id` and `client_secret` into the secret store with `crewlet secrets set SLACK_CLIENT_ID_<HANDLE>` and `crewlet secrets set SLACK_CLIENT_SECRET_<HANDLE>`, each value piped on stdin rather than passed with `-value`, then delete `slack-apps.json`. See [Upgrading from `crewlet slack provision`](../integrations/slack.md#upgrading-from-crewlet-slack-provision).

## `crewlet confluence import`

```
crewlet confluence import <company.yaml> <directory> [-space KEY] [-prune]
    [-config PATH] [-api URL] [-dry-run]
```

Publishes a tree of authored markdown into Confluence. **One walk, two destinations, decided by the file**: a file whose frontmatter declares a `trigger:` is a [tool skill](../concepts/tool-skills.md) and goes to `knowledge.skills_container` with the leading code block the engine parses back out; everything else is a knowledge doc, published as prose into the space its parent directory names, titled by its first `# H1`. Routing follows the file, not the directory: a skill filed under `ENG/` is still a skill, and publishing it as prose would put an instruction meant for one phase of one turn into every seat's context.

**Every target space is checked before a single page is written**, so a typo in a directory name cannot leave a half-published tree. The importer never *creates* a space: that names a container the whole company works in.

**A page that already exists is updated in place**, matched by title within its space. Confluence has no external-id field, so a page renamed in the UI is orphaned and a re-import creates a second one. This is the backend's limitation, reported rather than worked around: a marker page or label used as identity would disagree with Confluence the first time somebody moved a page.

**Nesting and labels come from frontmatter.** It may declare a `parent:` (the **title** of a page in the same space to nest under) and `labels:` (the author's own page labels).
- The plan is ordered parents-first, so a `parent:` naming a page **published by the same run** resolves.
- A cycle stops the walk naming the files.
- A parent nobody publishes is a note, and the page goes at the space root.
- **An existing page is never re-parented**: people move pages deliberately.
- Labels are attached on every run, not only on create (the call is idempotent, so a label added later reaches the page). A label that will not attach is a note, not a page failure.

**Provenance is recorded.** Every skill page this command writes gets the global label `crewlet-skill`, which says only that the importer wrote it. `-prune` needs that: an orphaned page is deleted only if this tool published it, so a skill a lead authored by hand in the wiki (no local `.md`) is never lost. A label that cannot be written is a note, not a page failure; the page is correct, but it can no longer be pruned.

**Page failures are isolated**: a restricted page or one 403 does not cost the other forty. The run reports what failed and exits non-zero.

| Flag | Description |
|------|-------------|
| `-space KEY` | Publish tool skills into this space instead of `knowledge.skills_container`. Empty reads `$CREWLET_TOOL_SKILLS_SPACE`, then the config field. Skill files only: a knowledge doc takes its space from its parent directory. |
| — | Frontmatter on a knowledge doc may declare `parent:` (the title of a page in the same space to nest under) and `labels:` (the author's own, lower-cased and de-duplicated, as Confluence stores them). See above. |
| `-prune` | After publishing, delete skill pages in the skills space that carry the `crewlet-skill` label and whose key no local file publishes any more. **Three conditions, all required**: in the skills space, labelled, and parsing as a skill whose key this run's tree does not publish. The label protects a hand-authored page, the parse protects an ordinary page in the same space, and the key comparison makes a renamed skill a delete-and-create rather than a silent duplicate. **A prune that cannot enumerate the space deletes nothing** and fails the run, as a partial read would enlarge the orphan set and delete live pages. The set is taken from the *plan*, not from the writes that landed, so a page whose update 403'd is never deleted as its own orphan. A page that declares a trigger and does not parse has an unknown key and is reported, not deleted. |
| `-config` | Tier A config naming this node's store and keyring, for resolving the `${VAR}`s in the company's `confluence:` block. |
| `-api URL` | As in [`crewlet github provision`](#crewlet-github-provision), ([Where a command reads the company's values](#where-a-command-reads-the-companys-values)). |
| `-dry-run` | Print the plan and write or delete nothing. |

A company with tool skills turned off (`knowledge.skills_container: ""`) has no space for a skill file: a tree containing one **stops the walk** naming both the setting and `-space`.

See [Confluence Integration](../integrations/confluence.md#publishing-local-pages-from-your-machine-cli).

---

## `crewlet confluence provision`

```
crewlet confluence provision <company.yaml> [-secret-store|-env-file PATH|-print]
                             [-public-url URL] [-recreate-webhooks] [-dry-run]
                             [-config PATH] [-api URL]
```

Registers the inbound webhooks that make Confluence push page and comment events to this deployment, and mints the credential each carries into the `${VAR}` the company document already points at.

**The two deployments authenticate differently, so they register differently.**
- **Cloud**: one hook per event at `<public-url>/webhooks/confluence/<event>?token=…`. Cloud signs nothing and honours no registration field for a header, so a shared token in the URL is the whole authentication; one hook per event is how the engine knows which event fired (a Cloud payload does not say).
- **Data Center**: one signed hook at `<public-url>/webhooks/confluence` covering every event, verified by HMAC over the body against `integrations.confluence.webhook_secret`.

A re-run **converges**: hooks are found by the `crewlet:` name prefix this engine registers under, so a hook whose address moved is re-pointed, not duplicated, and one registered by hand is never touched. A correct hook is left as is, and an event this instance refuses is reported without stopping the rest.

| Flag | Meaning |
|---|---|
| `-secret-store` / `-env-file PATH` / `-print` | Where a minted token or secret goes. The same three sinks every provisioning command takes; a run with none refuses rather than mint a credential it cannot record. With `-env-file` or `-print`, a real run that would mint either value (it resolves to nothing, or `-recreate-webhooks` asks for a fresh one) is refused before reaching Confluence where the company's store holds its `${VAR}` ([A sink the store outranks](#a-sink-the-store-outranks)). |
| `-public-url URL` | This deployment's public base URL, which the hooks are registered against. **Overrides the Tier A webhook address (`api.public.external_url` when the file sets a [public listener](../guides/deployment.md#exposing-webhooks-without-the-admin-api), `api.external_url` otherwise) for this run**; registration is skipped only when both are empty. |
| `-recreate-webhooks` | Delete and remake every hook with a fresh token or secret. **Destructive across deployments**: the previous value stops working everywhere else this company runs, so use it to recover a leaked or lost credential, not in an ordinary run. A literal `webhook_token` or `webhook_secret` has no variable to mint into, so the run is refused: point the field at a whole `${VAR}`, or run without the flag to register the literal as is. Reads the company's values like any other run, by the same route. |
| `-dry-run` | Read and report; register nothing. Reads the company's values as a real run does. |
| `-config PATH` | Tier A config naming this node's store and keyring (default `./crewlet.yaml`); where the run reads the company's values and `-secret-store` records. |
| `-api URL` | As in [`crewlet github provision`](#crewlet-github-provision), ([Where a command reads the company's values](#where-a-command-reads-the-companys-values)). |

See [Confluence Integration — Webhooks](../integrations/confluence.md#webhooks-confluence-pushes-to-agents).

---

## `crewlet confluence resync`

```
crewlet confluence resync <company.yaml> [-space KEY] [-config PATH] [-api URL]
```

Read-only diagnostic beside the importer. Runs the engine's **own** [tool-skill](../concepts/tool-skills.md) walk of the Confluence skills space against a throwaway registry and prints what admitted, showing what a running node's next walk will see. It exists because a page that fails to admit is **invisible**: the only symptom is guidance that never appears in an executor prompt.

```
TS holds 12 page(s): 9 skill(s), 3 ordinary page(s).
  deploy                       Cutting a release
  incident-review              Running a post-incident review
```

A page that declares a `trigger:` and does **not** parse is printed separately and exits non-zero: somebody wrote a trigger and got the rest wrong, and counting it as ordinary is how a skill goes missing unnoticed.

`-space` overrides `knowledge.skills_container`, for checking a space the company document does not name yet.

**Skills only.** Knowledge docs are searched live at query time and never loaded into a registry.

**It does not reach into a running engine, and a running engine does not need it.** A page edit reaches every node through the page webhook, and every node also walks its skills space every 10 minutes (give or take a fifth, so a fleet does not walk in lockstep), so a change the webhook missed applies within 12 minutes without a restart. See [Keeping every node current](../concepts/tool-skills.md#keeping-every-node-current).
