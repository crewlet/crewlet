# The packages under `internal/`

**Every package states its own rationale in its package doc.** `go doc ./internal/coord` is the authority on coordination, not this file. What follows is a map, so you know which doc to read: one line per package, saying what it is for. A row that still runs longer holds decisions its package doc has not absorbed yet (#1474 folds them in package by package); read it with the doc, and record a new decision in the doc, never here. A row may not grow (`internal/commentgate`), and a new package gets a one-line row.

## The spine — what everything else is built on

| Package | What it is for |
|---|---|
| `queue` | The EventQueue contract: durable mailboxes, ephemeral ask/broadcast, dead letters; the `memory` and `jetstream` backends certified by one `queuetest` suite. |
| `coord` | Cross-process ownership and fleet agreement: TTL leases with a monotonic fencing epoch, and the contracts for every record the whole company must agree on now (activation pointer, counters, claims, node statuses, sandbox runs), kept by `coord/kv` and the `coord/memory` twin. |
| `store` | The node's local databases: its own file (audit log, learning memory, config revisions) and the replicated estate beside it, which only a state log's applier writes; backup copies, locks and migrations. |
| `statelog` | The replicated state-log framework: one ordered stream per domain is the write-ahead log every data node derives identical SQL rows from, with its write authority, read levels, retention, recovery and alarm table. |
| `textindex` | The engine's own lexical search: the analyzer that turns a document into terms, BM25 ranking over the inverted list, and term search of a single text; pure functions, the SQL is `search`'s. |
| `keyset` | A row's place in an order as the per-depth ARMS that read the rows after or before it, each one index seek, so a later page costs what the first does; NULL is a position, last in both directions |
| `search` | Knowledge search: the BM25 lexical index, the 1-bit semantic first stage (an IVF index replicated on the vector log) with exact rerank and rank fusion, the chat and change indexes, and the cross-node fan-out. |
| `events` | The event envelope and the typed-payload registry that reconstructs events off the wire, with the event-type catalogue and its feed and rate rules. |
| `org` | The organization model: the hierarchy, the seats that hold it, and the derived seat identity every node shares. |
| `seatname` | The one name Crewlet suggests or creates for an agent seat's identity at any vendor, held to each vendor's rule (ADR-0179). |
| `iam` | The engine's identity vocabulary, a leaf of values only: principals, the three login grammars, permissions and proof recency, and the one `OwnerOf` that resolves whose record a name is. |
| `authz` | The one authority table: whether a principal may do a verb to an object, decided by `Decide` over a row per verb, the route policy and refusal envelope, and the artifact, assistant and requester-ceiling rules. |
| `authz/orgchart` | `authz.Chart` over one `org.Organization`: who leads a person, project, unit or page container, who sits in a unit, and which grants a seat's role carries, as pure arithmetic over a tree for the engine and `/config` writes alike. |
| `config` | The two configuration tiers, Tier A (`Bootstrap`, the operator's file) and Tier B (`Company`, the founder's stored document): their decoding, `${VAR}` resolution, validation, warnings and JSON Schema. |
| `advisor` | Turns what the engine knows of a company's setup and work into findings an administrator acts on, each carrying its fix as data the administrator previews and sends. |
| `advisor/duty` | The Advisor's background duty (ADR-0164): one node under a lease runs the rule groups hourly and on a change to the setup, the model pass daily, and a person's Run now, and records what `advisor.Reconcile` decides on the advisor log. |
| `advisor/modelpass` | The Advisor's model pass: one run on `advisor.model` over the week's records as read tools, whose findings the engine grounds before any is kept (ADR-0164). |
| `atlas` | The company atlas's closed vocabulary and the one pure fold that turns typed, source-ranked claims into entries, relations and facts, plus the joins and reads over them (ADR-0126). |
| `atlas/derive` | Pure functions from company settings and the engine's records to atlas claims, computed at every read and never stored. |
| `atlas/gather` | Reads the atlas from the places the engine keeps what it knows, derives claims and folds them, reporting each source's coverage. |
| `atlas/collect` | The atlas-collect duty (ADR-0129): reads each connected integration's vendor on an interval and records on the atlas log only what moved. |
| `atlas/codescan` | Reads a Gitlet or forge repository's deploy and dependency files into LIKELY atlas claims by exact keys only, in the repository's own voice (ADR-0133, ADR-0136). |
| `atlaswake` | Wakes the agent seat that owns an atlas entry when it drifts, at most once a UTC day, bounded by a coordination record claimed before the publish. |
| `atlasdomain` | The state log's thirteenth domain: what people, agents and integrations' reads tell the engine about what the company runs on, as claims on one subject an entry (ADR-0126). |
| `advisordomain` | The state log's fourteenth domain: what the Advisor found and what became of it (each finding key's episodes and their history, runs, weekly reports, alarm episodes), one subject a finding key. |
| `configplane` | The control plane: how a fleet agrees which company configuration it runs, via an append-only activation pointer and per-node apply status, and the posture a node takes when it lacks the current one. |
| `seat` | Which seats this node runs: greedy fair-share claim and give-back by lease, a freshness-based admission gate, the heartbeat and presence row, and the watchdog. |
| `estate` | Routes every node's reads and writes of the replicated estate, signed, to the one data node (or, for git work, the Gitlet node) whose copy serves it, with read-your-writes floors and a failover rule per operation. |
| `rendezvous` | The one order a fleet's nodes are preferred in for a key: rendezvous (highest-random-weight) hashing, so every node computes the same placement from the same member set with no table to agree on |
| `objstore` | The fleet's OBJECT STORE (ADR-0026): the bytes of files too large for a replicated row, one object per upload under a minted `Key`, behind a `Backend` and a verifying `Store`; also the reference-table declarations the collector, audit and backup walk |
| `objstore/natsobj` | The default object backend: a JetStream object store bucket on the fleet's own broker, in NATS's own format, under a byte reservation so a full bucket refuses only uploads; it writes, reads and deletes through the stream, never the library's verbs |
| `objstore/s3obj` | The object store's S3 backend (AWS SDK) for any S3-compatible bucket: bounded, stall-watched, throttle-gated requests, multipart puts and ranged reads; `s3fake` is its in-process test server. |
| `objstore/memobj`, `objstore/objstoretest` | The in-memory object-store backend for tests, and the one conformance suite every backend, the twin included, runs. |
| `objstore/collect` | The object store's one fleet duty (`object-collector`): an hourly lock-free collection of unreferenced objects and a daily audit that counts the objects the store has lost |
| `objstore/migrate` | Copies every object the estate names (Required and Derived, never Retired) from the fleet's object store into another backend, verified, for `crewlet objects migrate --to s3`; resumable, moves no record. |
| `objstore/references` | The one list of replicated tables whose rows name object-store objects, read by the collector, audit, backup and store move, and held against the schema both ways by a gate |
| `artifacts` | The artifacts vocabulary, values only: ids, kinds, capabilities, roles, shares, manifests, limits, authority stamps, upload intents, view tickets and datastore shapes; a leaf `authz` and `config` import |
| `artifacts/manifest` | The grammar of an artifact version's file list: paths, reserved paths, whole and patch publishes, carries and the canonical manifest encoding. |
| `artifacts/mediatype` | Pure: the ONE extension table an artifact may publish (served type, class, compressible, scriptable). The served type is always the table's, never a client's |
| `artifacts/slideformat` | Pure: the slide format a `slides` deck holds (`deck.json` and one `<section>` per slide), checked at publish into one canonical tree the dashboard draws; a closed subset, refused rather than dropped. |
| `artifacts/svgsafe` | Pure: the ONE drawing allowlist an inline slide `<svg>` and a standalone `.svg` logo are both held to, with a strict XML front end and a stored re-serialisation. |
| `artifacts/brandformat` | Pure: the format of a `brand` kit (`brand.json` plus its logo and font files): strict decoding, field rules, stored asset form, contrast, and the one generated brand stylesheet, canonical index and slide theme an adoption makes |
| `artifacts/canvasformat` | Pure: a `design`'s `canvas.json` format (artboard positions, draw order, notes), parsed strictly, with the outline the gallery reads and the comment anchor. |
| `artifacts/commentanchor` | Pure: where a comment on an artifact points, and how its place is found again in each version at read, with no per-version write. |
| `artifacts/dashspec` | Pure: the format of a `dashboard` artifact's `dashboard.json` (datasets, widgets, engine source catalogue, closed deterministic calculations, outline, comment anchor) and its validation |
| `jsonform` | The strict JSON reader the artifact formats hold their indexes to, and the one reference of each index's shape an agent reads, both derived from the format's Go type (ADR-0059) |
| `artifactsdomain` | The artifacts state-log domain (`CREWLET_ARTIFACTS_LOG`): artifacts, versions, the objects they name, assets, shares, folders, the adopted brand, comment threads and their inbox notices |
| `assistantdomain` | The Dashboard Assistant's state-log domain: a person's conversations, turns, cards, proposals and files, sealed and replicated to every data node. |
| `accessdomain` | The state log's record of each seat's access (ADR-0103): what each turn was told of its own access, the SSH certificates signed, the SSH commands run or denied, and the verdicts on delegated requests (ADR-0125). |
| `accessreview` | Turns a change to an agent seat's access into one review turn for that seat (ADR-0111). |
| `executordomain` | The seats' executor sessions: `statelog`'s domain on `CREWLET_EXECUTOR_LOG`, one row per seat's turn naming its sealed session objects, its conversation's head and the seat's own search. |
| `executordomain/executordomaintest` | TEST-ONLY: one data node's copy of the executor log, the shipped applier and reader over a replicated estate of its own, for suites that may not name the state log (`internal/sandbox`) |
| `executorkeep` | The leaf vocabulary of kept executor work that the log's domain and the sandbox share: a turn's `Ending`, the session, harness and id bounds, `Excerpt`, the sealed shapes, `TurnRow` and `ConversationHead` (ADR-0073). |
| `executorread` | One read of a seat's kept executor session for every surface that reads one: the turn's row, the sealed object it names and a page of the transcript, with what the read came to said in one set of words. |
| `artifactdata` | The artifact datastore: shared, durable collections of documents as a strict state-log domain of its own, written by sequenced records and decided per write in the applier. |
| `artifactdata/canon` | Pure: the ONE canonical document encoding (sorted keys, numbers as decimal text) and the store state digest (a modular sum of document digests) |
| `artifactdata/query` | Pure: compiles the datastore query grammar to SQL over the field index, refusing any filter the index does not hold |
| `costlimit` | The engine's one cost-weighted, per-node limiter: two token buckets per charge (a principal on a scope, and the scope), paid together or not at all. |
| `node` | Composes a broker client, a coordination backend and the seat host into a runnable node: attaches a seat's mailbox on acquire, admits every turn (`AdmitTurn`), and drains |
| `engine` | Assembles one process's part in a company (the config, the org, the models, the tools and the turn machinery between them), wiring which concrete thing satisfies which seam. |

## The agent runtime

| Package | What it is for |
|---|---|
| `agent/turn` | The turn loop: executor, reviewer and the guards that decide whether a turn reached anybody and whether a broken one may run again. |
| `agent/runner` | Drives a turn's phases against real models and tools: the executor's run in a box, launched, carried on and collected, and every other phase on the engine host. |
| `agent/runner/steersourcetest` | The one contract suite every `runner.SteerSource` runs: the first segment's box and the executor's record of a resumed segment, held to the same take, `Waiting` and brief promises. |
| `agent/structured` | How a phase gives a typed answer: it calls a tool, the ordinary tool loop carries the arguments out, and `Remarshal` maps them to a payload, refusing unknown fields. |
| `agent/phase` | The per-phase vocabulary: which phase is running, which provider chain it runs on, and what a turn concluded. |
| `agent/toolloop` | One phase's model-and-tools conversation: starts harness runs, serves each the phase's tools through a dispatcher that gates every call, and folds the runs back into the phase's record (ADR-0049) |
| `agent/steer` | A person's note to a running turn: the box one turn holds, the signed wire a note crosses, and the record of where it was read. |
| `agent/execstate` | The versioned wire format of a turn parked on its executor's run: the turn's bookkeeping and start-of-turn context, serialized into the pending-run row and held to its share of it. |
| `agent/extension` | The round-cap extension judge: when a phase runs out of tool rounds, a cheap model says whether it is progressing, and the engine grants more rounds or falls through to the rescue path. |
| `agent/subagent` | `delegate`: the short-lived leaf workers an executor hands work to, as a task graph of tool loops with a persona, a slice of the parent's tools, a declared answer and hard caps. |
| `agent/inbox` | What wakes a seat, and what it must not be woken by twice. `ledgered` is the completion side |
| `agent/ledger` | The prior-work ledger: what a seat already did across the `self_iterate` rounds of one turn, structured from what the engine recorded and rendered into each next round. |
| `agent/prefetch` | Renders the context blocks a turn's prompt is built with, frozen at turn start: knowledge, memory, past work, the chat thread, open asks and named systems. |
| `agent/prompts` | The per-phase prompt text and one builder per phase, each returning its outline beside its text, plus the continuation briefs for a kept session, a send-back and a relaunch. |
| `agent/skills` | Tool-skill subsystem: guidance from the knowledge base that the registry keeps by source page and that is offered to a phase, with the load-before-use guard. |
| `agent/skillsync` | The one loop per node that keeps the tool-skill registry current: walks, single-page reads, fleet broadcast, periodic walk and bounded retry, all on one goroutine |
| `agent/builtin` | The tools the engine itself ships to a seat and the operator, and the one authorizer that gates every one of them at registration. |
| `agent/colleague` | Resolving a free-text query to one seat or an honest list of candidates, never a guess: the lookup before an A2A ask, a chat mention or a page mention |
| `agent/turnctx` | What a turn IS, carried as an explicit argument rather than ambient state: work key, run id, holds, lookups, call log and answers seen, shared safely across its goroutines. |
| `tools` | The tool registry and the per-phase surfaces built from it: four kinds of tool under one contract, with where each came from recorded at registration. |
| `tools/argval` | How a first-party tool reads one argument a model sent: one reader per declared type that both decides whether a value is one and returns it, shared by the surface check and the builtin tools (ADR-0059). A leaf over the standard library |
| `tools/toolargs` | A tool call's arguments: the one decode every transport shares, and the canonical form (closed-set members spelt as the set spells them, nulls left out) every step after the entry reads. |
| `tools/toolschema` | A tool's input schema as an MCP client takes it: the one object-schema builder every catalogue uses, and the check the tool session lists a tool by. A leaf over the standard library |
| `tools/toolschema/toolschematest` | Holds a tool's input schema to that check and the JSON Schema meta-schema, for the sweeps over every first-party tool |
| `caveat` | What a landed write or a refusal tells its caller beside the outcome, worded for a model and for a person at once, with the action asked of a person (ADR-0060) |
| `modelnote` | Carries, in process only, the sentence a tool call answered a model with, from the answering route to an in-process caller whose own model made the request (the Dashboard Assistant). |
| `mcp` | MCP client: supervises each server (a stdio child is a process tree on an allowlisted environment), files and levels its tools, and reports why a server failed. |
| `inbox` | A person's inbox as every native domain keeps one: the marks over a domain's notices, the one gesture that moves them, the classifier a page is marked by, and the movement an applier announces. |
| `mcp/oauth` | The OAuth client a remote MCP server is signed in with: PKCE S256 only, resource-bound (RFC 8707), refresh required, renewal, revocation, dynamic registration. Holds nothing; the grant is `engine`'s (ADR-0075) |
| `mcp/catalogue` | The MCP servers this build knows: the curated table, the one recognition of a configured server by address or command, and the search of a community registry |
| `a2a` | Agent-to-agent asking: one ask, one answer, then closed; the channel is an authorization record over the durable seat inbox, with a `Service` that addresses and a `Sweeper` that retires. |
| `decision` | The shape of a structured ask (question, two to four options, recommendation, evidence, role) and its validation, shared by the tracker and executor asks that put a choice to a person. |
| `delegated` | Delegated decisions (ADR-0125): the typed act of a request that waits on a person, how a panel's verdicts combine, and the engine's limits, escalating paths and required checks that bound a delegate; values only. |
| `growth` | The marketing and growth integrations' vocabulary: the six classes, seat levels, each product's catalog of every vendor operation, money in micros, account-local days, report answers and the metrics (ADR-0170, ADR-0171). |
| `spendguard` | Holds an ad product's spend to the company's per-account caps at the ask and again at the apply, under one lease per account; micros only, and `spendguardtest` is the one contract every capacity runs. |
| `metaads` | Meta Ads as a growth product: every Marketing API operation an ads integration reaches, classed in its catalog, called as one system user of the company's own app through the least privileged token, scoped to the seat's ad accounts, spend held to the caps (ADR-0170, ADR-0171). |
| `appstoreconnect` | App Store Connect as a growth product: every operation of Apple's API classed in its catalog, each call signed for itself with the agent's own team key, scoped to its apps, public and admin acts held (ADR-0171, ADR-0179). |
| `appleads` | Apple Ads as a growth product: every Platform API operation classed in its catalog, called as the company's API users with roles read live per ad account, scoped to the seat's ad accounts, spend held to the caps (ADR-0170, ADR-0171). |
| `es256` | Signs a JWT with ES256 (ECDSA P-256 over SHA-256) on the standard library alone, as Apple's APIs take it. |
| `worklet` | Worklet, the engine's own work tracker and `statelog`'s first domain: its records, subjects, writer, applier, reads and the routing of who hears about a change. |
| `changefeed` | Durable, fleet-wide pull consumer over a state log that derives a wake from each committed record, with replay and dead-lettering of records a node cannot read |
| `routing` | Who a committed record concerns and which of them it wakes: the one rule every native domain routes by (one reason per handle, ordered fallback chain, roster-filtered wake, delivery states). |
| `pickup` | The WORK PICKUP (ADR-0112): the one wake derived from state rather than from a change, so an item a turn did not act on is handed to its holder again. |
| `alarmwake` | Turns a node's alarm transitions into wakes (ADR-0095) for every agent seat whose role carries `health_tools`: the notify spine's parser and prompt for the inbound source `alarms`. |
| `doclet` | Doclet, the engine's own knowledge base (containers, pages, revisions, change record) and `statelog`'s third domain; a drop-in for Confluence through the `knowledge` seam. |
| `iamdomain` | The company's identity estate: people, their logins and addresses, credentials, invitations and sessions, as `statelog`'s fourth domain over the `iam` values leaf. |
| `iam/credential` | What somebody proves themselves with: argon2id passwords, TOTP second factors, recovery codes, machine tokens, one-time password links, and the sign-in throttle. Stores verifiers, never secrets. |
| `iam/session` | The signed session cookie a browser holds, and the tables that resolve it to a person and a seat, answering 503 and never 401 on a node that is behind. |
| `iam/authevents` | The one trail authentication facts reach: verified facts publish as events, failed attempts only count per source and publish one `iam_login_failures` per closed minute. |
| `usage` | What each node's seats, schedules and people did each company day, and who read unit-private repositories: one record per node and day, replicated by `statelog` (ADR-0020), summed by the reader. |
| `gitlet` | The code domain of Gitlet, the engine's own git host: repositories, their changes, reviews and merge queue as records on the `CREWLET_CODE_LOG` state log, with the packs' bytes in the object store. |
| `chatlet` | Chatlet, the engine's own chat and `statelog`'s ninth domain: channels, rooms, DMs, threads, messages, reactions, retention, erase and prune, with the routing snapshot a wake derives from. |
| `knowledge` | The backend-neutral seam through which turn-start knowledge prefetch and `search_knowledge` search the one team knowledge base, best effort, plus the one bound on ranked-search query text. |
| `providers/llm` | The model contract a harness implements: a conversation in, an answer and usage out, and for a seat at work the phase's tools as a `ToolHost` the harness calls natively |
| `providers/llm/chain` | The seat's model fallback: an `llm.Provider` over its entries tried in order, per call, with a per-launch `Pick` for executors and `Unready`/`KnownLimit` to answer without a call |
| `providers/llm/fleet` | The LLM fleet balancer: which member of a fleet of entries takes a seat job's next piece of work, from its sign-ins' readings, sit-outs and refusals, and the walk a failed call takes |
| `providers/llm/llmtest` | A scripted-model provider for tests that drives the tool host through its real MCP session, as a harness does |
| `providers/llm/harness` | The only model backend: drives a vendor's own harness as a process, with a seat home per call, the seat's tools served as one MCP server, and a spent sign-in benched on this node. |
| `providers/llm/harness/harnessprofile` | The harness profile table (`profiles.yaml`): what each harness accepts, the merge of an entry's `overrides` and the rules a merged profile is held to; a leaf so `config` can judge entries against it. |
| `providers/embeddings` | The vector backend: the one place a text becomes a vector for the knowledge corpus, knowledge search queries, and diary and episode recall, enforcing each model's input limits. |
| `speech` | Dictation's speech-to-text: a local streaming recognizer (sherpa-onnx, embedded per platform, in a re-executed worker process) behind the `Streamer` interface, its pinned model table and the `Fake`. |
| `sandbox` | The executor runtime: every agent seat's executor is its harness's own run in a box, launched detached, collected and resumed by the engine, its session kept between boxes; the in-box runners are `sandbox/executor`'s. |
| `sandbox/homecache` | A node's cache of provisioned container box homes: a seat's setup run once per key, a new box's home copied from it, no credential ever kept. |
| `notehelper` | A `main` package built per box arch with the engine and written into a box: beside a harness with no hook or plugin, hands it the bridge's notes, caps a turn's model responses and reports skill loads, through an adapter (Codex app-server). |
| `notehelper/helperbuild` | The one recipe that builds the note helper per box arch, for the release, the image, `make notehelper` and the tests (`WithHelpers`). |
| `notehelper/helpersource` | The committed digest of the note helper's source, which the helper carries and the engine checks, so a helper from other source is refused. |
| `hostbox` | The primitives for running somebody else's process on the engine host: one environment allowlist, safe file placement, and journaled shared-login writes, shared by the harness backend, local sandbox and MCP stdio children. |
| `hostbox/hostboxtest` | The kill matrix every writer of a shared login passes: it SIGKILLs the re-executed writer at each named step of its journaled writes and judges what the kill left. |
| `shellword` | The one quoting of a value as a word of a POSIX shell, for every command the engine writes for a shell to read. |
| `skillname` | The one rewrite of a skill's name as a harness lists it (Grok Build's), for the engine and the note helper alike. |
| `flock` | The one exclusive cross-process file lock (`store`, `queue/jetstream`, `hostbox`, `gitlet/cache`), plus a pin: a lock on a file description a child inherits, held while any process holds it. |
| `nativelib` | The native-library cache a library the binary embeds is unpacked into (Turso's, the speech recognizer's), its cross-process lock and the verified archive extraction. |
| `sockcount` | What a TCP socket's kernel holds unacknowledged and has had acknowledged, read from the connection's own descriptor (Linux and darwin). |
| `procgroup` | Signals a child process tree as one process group, tells whether a recorded group is still the same one, and sums a group's CPU |
| `nodememory` | The memory this node's processes may hold, the least of the machine's and every cgroup limit above the process: the one reader for every bound that is a share of it. |
| `nodebudget` | The lines of a node's memory budget beside Gitlet's lanes (the Go runtime's share, the page cache, the read lane), which Gitlet's reserve and the Go memory limit both read. |
| `gitlet/gitproc` | The one wrapper that starts git: every git process the engine runs, with its built environment and configuration, lane slot, process-tree end and classified failure, plus the git version floors (ADR-0087). |
| `gitlet/cache` | Each Gitlet node's disposable bare repositories, one a repository it serves, derived by convergence from the code log's rows and the object store, from which real git upload-pack answers clones and fetches. |
| `gitlet/compact` | The maintainer's three loops over a repository's packs: compaction bounds their count, publication writes its bitmap layers, expunge drops what no ref reaches. |
| `gitlet/host` | Gitlet's smart-HTTP git surface for persons and boxes: admission, fetch and push serving, the one landing every push and engine write takes to an applied row, cache repair, verifies, merge chains and downloads. |
| `gitlet/gitlettest` | The one suite every Gitlet topology passes, real git through a real code log, and the data-node and Gitlet-node blocks every topology is built from |
| `gitlet/gitletfault` | The named fault points of Gitlet's push, sync, maintenance, write, check, expunge and mirror paths, in order, and the injector the crash suites arm at them; nil in production. |
| `gitlet/gitletcluster` | Test-only and solo: Gitlet on three data nodes through the production estate router, running the fleet suite (`gitlettest.RunFleet`) and the commit-chain benchmark. |
| `gitlet/home` | A repository's home Gitlet node at work: the loop that schedules each repository's change, check, merge-queue and registered workers from the rows, and the merge previews beside it. |
| `gitlet/checkrun` | Runs a repository's queued checks on every Gitlet node, each in a box with no harness, and the home's sweep of attempts whose runner is gone. |
| `gitlet/mirror` | A repository's import, pull-mirror syncs and push-mirror pushes, run as a worker of its home Gitlet node's loop. |
| `gitlet/query` | Server-side git reads of a repository by object id on a Gitlet node's pinned cache copy: trees, files, diffs, histories, blames, a change's revisions, code search and find. |
| `gitlet/readaudit` | The ledger that folds a node's reads of `visibility: unit` repositories into one `code_fetched` record per reader a flush, off the read's path. |
| `deployfiles` | The closed list of a repository's deploy and dependency files the atlas may be told about, decided by path alone, shared by Gitlet's listing and the atlas scan. |
| `codeindex` | Gitlet's trigram index as pure functions over bytes: per-blob segments, tip maps, the plan of a regexp over them, and the query syntax, so a code search reads only the few files that can match. |
| `codesearch` | Code search of every repository an asker may read, on the Gitlet nodes that hold them: one repository a git read, more one signed scatter with an exact merge and a coverage of what was not searched (M6-D14). |
| `learning` | What a seat remembers: episodes, diary, counterparty profiles and self-taught skills, all best effort. `memsync` carries the rows across a fleet; `memread` answers a screen from the holder. |
| `schedule` | Role- and unit-scoped cron work: at-most-once delivery, missed-tick catchup, manual runs, and a conversation per schedule per seat. |

## The edges

| Package | What it is for |
|---|---|
| `api` | REST, the sockets and the dashboard, served inside the engine's process over its own store, broker and coordination plane; every request is guarded, and each surface states its authority where it mounts. |
| `api/webhooks` | Inbound vendor webhooks (GitHub, GitLab, Jira, Confluence, Slack, Datadog, Forge) and the OAuth landings, each verified before the delivery is recorded |
| `runtoken` | The signed, self-describing per-run credential (OTLP receiver, tool bridge, Gitlet box git, artifact view ticket) that any node of a fleet can mint and verify; its key derivation also feeds the broker credentials. |
| `recordseal` | A record's seal under the fleet's keyring (ADR-0031): the frame, `Signer`, `Verifier` and three verdicts (`Verified`, `KeyUnknown`, `Tampered`). A leaf that imports nothing of the engine, so every sealing channel can use it. |
| `signedask` | The one rule for a request one node asks another to carry out over the queue's ephemeral verbs, and its reply: sealed with the fleet's record seal, bound to an id and deadline, never replayable (ADR-0031) |
| `signedask/signedasktest` | TEST-ONLY: a signed channel's ledger in memory, so a suite restarts a guard over the ids it kept and makes the keep fail. |
| `api/mcpbridge` | Serves a detached box run's tool surface (a seat's executor) to its harness over MCP on HTTP, dispatching through `tools.Surface`, fleet-addressed and relayed to the seat holder. |
| `mcprelay` | `crewlet mcp-relay <socket>`: the stdio MCP server a harness starts for a seat's tool session, copying stdin to the engine's per-run unix socket and back; reads only its argv |
| `mcpreview` | Decides from the fleet's MCP listings and the accepted tool definitions which tools a server changed since their levels were decided and which ask a person first (ADR-0078); one pure reading shared by every caller |
| `toolsession` | One phase's tools served to one harness run over MCP, on a unix socket per run: the engine's end of `crewlet mcp-relay`. Transport only; the tool loop decides every call. |
| `toolsession/toolsessiontest` | The one contract suite every `toolsession.Dispatcher` (the tool loop's, the bridge's) runs, through a real MCP session, so the gates both hold are written once |
| `whsec` | The Standard Webhooks signing-secret format, decided in one place for config, the webhook edge and integration startup. |
| `atlassianhost` | Decides once whether a Jira or Confluence address is Atlassian Cloud or Data Center (`Of`), and which API gateway a cloud id is read through (`Gateway`), for config validation and both clients |
| `api/authapi` | `/auth`: how a person becomes a principal (sign-in, second factor, step-up, invitation and reset links, password change, token exchange, sign-out); owns the HTTP shape and the refusals. |
| `api/iamapi` | `/iam`: the company's identity directory over HTTP: people, service accounts, seats, invitations, tokens and password links, always guarded. |
| `api/workapi` | The human write surface over Worklet, Doclet and Chatlet: `/worklet/*`, `/doclet/pages`, `/chatlet/channels`, as adapters over the same tools a seat holds. |
| `api/configapi` | `/config`: the versioned company document, its revision history and diffs, entity routes over seats, units and charter, and the one write path onto it (`/setup`, the charter and the engine's own writers use it too). |
| `api/setupapi` | `/setup`: connecting an integration from the dashboard, admin-only; it writes through `/secrets` and `PATCH /config`, never on its own. |
| `api/llmapi` | The model routes Settings › Models writes through: a credential tested before it is stored, model checks run on the node's own time, retries, host-login checks and Codex device sign-ins, each audited |
| `api/mcpapi` | Settings › MCP & tools' own routes: the catalogue, a server's detail, restart, the tests, tool levels, write-only values and the OAuth sign-in, each guarded and audited |
| `api/operator` | The operator catalogue: Worklet, Doclet, Chatlet, code and health tools, built per call and served to the people running the company through one dispatch over MCP, the dashboard act route and the write surface. |
| `assistant` | The Dashboard Assistant's orchestrator: a person's question answered by a model through `agent/toolloop`, every read made as that person and every write a proposal they approve; not a seat (ADR-0050). |
| `assistant/wire` | The Dashboard Assistant's protocol vocabulary, a leaf: event kinds, closed sets, frames, cards and route bodies, and the stream's one encoding, held to the dashboard's TypeScript contract both ways |
| `assistant/catalog` | The Dashboard Assistant's tool catalogue: curated tools with their own schema, card and preview, and the generic API tools that reach any route the person may use, each call a proposal or read. |
| `research` | The web pages Ask Crewlet reads: any public `https` page, `GET` with no credentials, redirects only to public `https` addresses, 2 MiB in 15 s through the guarded dial, text in 12 KiB parts, cached ten minutes per node. |
| `assistant/exec` | The ordinary request an assistant tool call becomes: `Request`, `Response`, and the `Dispatcher` and `Grant` seams, a leaf `api/auth` implements |
| `enginedocs` | The engine's own `docs/` and `adr/` pages, embedded (the module root's `Docs`), searched with `textindex`'s BM25 and read by section; `Anchor` is the one GitHub-slug rule the docs gate also uses. |
| `assistant/cards` | Turns a tool's answer into the engine-built cards the Dashboard Assistant shows (table, entity, open in app, error, notice), never authored by the model |
| `assistant/proposal` | The pure logic of a Dashboard Assistant proposal: the digest binding an approval to the previewed request, the legal state moves, expiry, and the card's rows drawn from the server's preview. No I/O (ADR-0051) |
| `assistant/proposal/approval` | Carries a person's decision on a Dashboard Assistant proposal out: approve, decline, Stop on a parked turn, and a page action's claim and report (ADR-0051) |
| `assistant/live` | A Dashboard Assistant turn's live stream: a bounded in-memory frame log on the running node, and the fleet half that lets another node follow it and stop it |
| `assistant/assistanttest` | The Dashboard Assistant's test-only support: golden files, a turn-frame builder, example args, a sample company and the suite rig over `assistant/evals` |
| `assistant/evals` | The Dashboard Assistant run without a node, as production code: the one scripted provider, the shared rig and the evaluation cases `crewlet assistant eval` loads and judges. |
| `api/assistantapi` | `/assistant/`: a person's assistant conversations, turns, streams, files and approvals over HTTP, guarded and refusing in the engine's envelope. |
| `api/secretsapi` | `/secrets`: the company's credentials, written through a running node because the coordination broker has no socket; admin-only, reveals are break-glass and logged, a write re-activates the revision that reads it. |
| `api/stream` | The dashboard's `/ws/stream` socket: live pushes decided per reader and room, its query channel, and how an open socket's credential is decided again. |
| `api/dictation` | The `/ws/dictation` socket: a person's audio to the recognizer and its partials and finals back, admitted and kept decided as `/ws/stream` is, with its session limits. |
| `observe` | The observability edge: what becomes a durable event-store row, what reaches the live dashboard, and the custody that lets a node without `data` keep its events on the fleet's data nodes. |
| `eventfan` | Answers the fleet's turn-level history (events, turns, traces, tool calls, outcomes) by asking every data node its own store at query time and merging exactly, with a coverage naming nodes that did not answer (ADR-0021) |
| `tokens` | Rolls LLM spend into the one breakdown a dashboard renders (phase, model, provider, worker, seat, time), for the live window and for named windows read from the replicated `usage` domain. |
| `coalesce` | How a node publishes a record it makes many times a second: occurrences folded per key, sealed into one retry-safe event a flush, published off the caller's path (ADR-0008); `auxspend` and `gitlet/readaudit` own the records |
| `auxspend` | The seats' auxiliary-model spend, recorded: the `Use` attribution, the per-turn `Tally` and `Budget` meter, and the `Ledger` coalescing calls into `auxiliary_spend` records |
| `tracing` | OpenTelemetry wiring: one always-installed TracerProvider configured from `OTEL_*`, and the conversion between OTel spans and the event envelope's trace ids |
| `notify` | The backend-neutral notification spine: conversation keys, digest coalescing, party resolution, the rate valve, wake-body fencing, and thread reads and status writes across chat surfaces. |
| `backup` | A restorable copy of a node's store, streams and objects, taken from inside the engine because it alone reaches all three; the manifest, written last, is the claim. |
| `maintenance` | The retention sweep for short-horizon tables, state-log operation ledgers, coordination removal markers, removed seats' chat read state and Home boards, and mailbox retirement; a fleet singleton, never a replicated row |
| `mattermost`, `slack`, `gitlab`, `github`, `jira`, `confluence`, `datadog`, `cloudflare`, `expo`, `digitalocean`, `scaleway`, `googlecloud`, `atlassian`, `aws`, `ssh` | The vendor integrations: each vendor's client, inbound parsing, prompt, seat access and atlas read. |
| `googleauth` | Every way the engine gets a Google token: the company's Google identity by federation, impersonation by scope, the token caches and the one call path to Google. |
| `googlegrowth` | What every Google growth product shares: a request held to its operation's resource, sent as the seat's growth identity, bound to its live target and recorded on the access log. |
| `admob` | AdMob: every method of its API in one catalog, offered as growth tools at the seat's level and publisher accounts, as the company's consent grant. |
| `adsense` | AdSense: every Management and Platform API method in one catalog, offered as growth tools at the seat's level and publisher accounts, as the company's consent grant. |
| `googleanalytics` | Google Analytics 4: every Data, Admin and Measurement Protocol method in one catalog, offered as growth tools at the seat's level and properties, as its Google growth identity. |
| `googleads` | Google Ads: every API method in one catalog, each mutate classed by every operation it holds against live values, spend held to each account's caps, as the seat's Google growth identity. |
| `searchconsole` | Google Search Console, Site Verification and the Indexing API: every method in one catalog, offered as growth tools at the seat's level and sites, as its Google growth identity. |
| `googleplay` | Google Play: every Android Developer, Reporting and reports bucket method in one catalog, edits opened and committed by the engine, writes classed by their target, as the seat's Google growth identity. |
| `origin` | records where a failure left the engine's own code: a deferred stamp names the function a failure passed through. |
| `issuereport` | Opt-in anonymous reports of the engine's own defects, built from typed fields only and filed by a relay as GitHub issues. |
| `panics` | The one way a contained panic is read: every `recover()` that keeps a defect from ending the process calls `Recovered`, which names the panicking function and its kind and tells the observers. |
| `integration` | The app-neutral reconcile spine for external surfaces: the finding/phase vocabulary, the who-acts schedule, the fleet-singleton worker, the disconnect intent and the tool roll-up; `integrationtest` certifies every reconciler. |
| `capability` | What one agent seat can reach, as the seat is told it: a pure snapshot of its access, the closed list of what counts as a change, the difference between two snapshots, and the two prompt sections that say them. |
| `fleetsecrets` | The company's credential store on the coordination KV: this package owns the sealing key, coordination owns the bytes; an operator view and a separate engine view over one bucket. |
| `compact` | Fits text to a size budget by having the seat's auxiliary model rewrite it, never by cutting it; refuses with an error rather than degrade to a cut. |
| `textcut` | Shortening a string on a rune boundary, with one marker and one rule for every cut: the last resort, never a way to fit text a model or person acts on |
| `activityline` | The executor activity transcript's line grammar: a numbered call, its whole input where cut, its timed result; written by `sandbox/executor`, read by `assistant` and the dashboard. |
| `tabular` | CSV and XLSX downloads of a table, one implementation of each, escaping formula-like cells and refusing what a format cannot hold, shared by the datastore export and the assistant's table cards |
| `named` | What a sentence the engine writes NAMES (a work item, a change, a commit, a page, a turn), carried beside its words as typed refs, each a kind, an id and the words that spell it, built by one builder so the two cannot drift. |
| `words` | Says a set in a sentence (`Or`, `And`, `AndMore`, `Said`) instead of Go's bracketed slice, and gates the module against brackets, hand-joined lists and em dashes in person-facing text. |
| `jsoncarry` | How a record round-trips through a build that only half understands it: a decode keeps the fields the struct has no home for and the next encode folds them back in; a carried field loses to a known one. A leaf over `encoding/json` and `reflect`. |
| `httprelay` | Carries one HTTP exchange from the node that received it to the process holding what answers it, over the broker's ephemeral request and reply, as signed, acknowledged frames. |
| `plainjson` | JSON written for a reader who reads it as text: `Marshal` leaves `<`, `>` and `&` unescaped and ends where its value does, and `Unreadable` says a caller's undecodable JSON in field terms. A leaf over the standard library. |
| `jsonscan` | Reads one JSON text a token at a time from an `io.Reader`, never holding a string, name or number whole, so session readers handle lines of any size. |
| `kept` | What a record keeps of a payload too large to keep whole: `Len` weighs a value as the encoder escapes it, and `Record.Text` and `Record.Args` leave out the rest so that it reads as left out. |
| `toolname` | Declares once the engine's own tool names that a package must spell but cannot import from the tool's owner; a leaf, with a suite walking the module's string literals for copies. |
| `toolname/toolnametest` | TEST-ONLY: the set of tool names `toolname`'s walk holds every string literal to, shared with the suites that hold a tool surface to it. |
| `httpx` | The one outbound HTTP transport every client shares, the public-address guard for a URL a remote party named (ADR-0072), and the rate-limit rule a vendor client waits out a 429 by. |
| `browser` | The Chromium floor (`MinChromiumMajor`), the Playwright release a box installs (`Outdated`), a box's ensure values, the browser server's name (`ServerName`), the wire a box's relay posts its screen on, and that no Go but the artifact render's client names a DevTools method. |
| `browser/boxhelper` | The helper written into a seat's box to run its browser: ensure (`EnsureCommand`, `ParseEnsure`), installing the tree locked in `tree/` with the box's npm, a supervisor serving Chromium through Playwright MCP behind a fenced shim (`ShimArgs`), the Node and npm floors. |
| `browser/boxhelper/boxhelpertest` | Test support: a seat's box as the engine writes it (`Start`), ensure run as the engine runs it, an MCP client over the shim (`MCPOver`), the probe, a stand-in registry (`Registry`), each Node with its npm and a Chromium (`Each`) and the suite's tree (`Tree`). |
| `browser/boxhelper/live` | The box helper's live view of a seat's browser: its tabs, its screen while somebody watches, the check-in, and the relay, one per job, that carries them over `relay.sock` to the run's bridge. Decisions are in the package doc. |
| `browser/chromiumtest` | Test support: the two pinned Chromiums a case drives (`Newest`, `Floor`, `Each`), how one is launched, and what its protocol describes (`Protocol`), never the machine's Chrome. |
| `browser/hostreach` | The launch's check that a browser seat's box is off its host's network: the box on the engine's own kernel (`EngineKernel`), and no route of it reaching a listener on the engine's loopback (`Check`), with the box's half (`reach.js`). |
| `browser/screen` | A seat's browser screen on the seat's holder: the per-job hub that bounds what a box's relay posts and tells it, on its one held poll, whether anybody watches. Decisions are in the package doc. |
| `sigv4` | AWS request signing on the standard library alone: Signature Version 4 with an access key, and the IAM Roles Anywhere form that signs the same canonical request with an X.509 key. |
| `modelcatalog` | What a model step suggests: each coding harness vendor's models, read from models.dev's public keyless catalogue and cached in memory on this node, never through a model vendor's API. |
| `api/httpjson` | How every JSON surface answers: the refusal envelope, 503/429 with `Retry-After`, immutable answers, strict body reading, the engine header. |
| `api/opkey` | The HTTP half of an operation id: the `Idempotency-Key` header, the rule a caller's key is held to, and the request digest and canonical encoding that bind a key to the request that asked for it |
| `api/paced` | The per-mebibyte pace a client's connection is held to, both ways, in one package for every streaming surface (project files, artifacts, Gitlet transfers): `Body`, `Writer`, `Send`. |
| `api/pagepolicy` | The browser-facing response policy: Content-Security-Policy, framing, sniffing, referrer and HSTS headers for the API, the dashboard, the artifact origin and repository downloads, plus the one gzip negotiation. |
| `artifactupload` | How bytes become an artifact version: upload intents, reservations, the transfer legs, the verified finalize and the one publish pipeline every surface calls. |
| `artifactupload/edits`, `artifactupload/lint`, `artifactupload/vendor` | Publish-time machinery for an HTML artifact: pure exactly-once edits, the policy linter that rewrites and verifies a page, and the guarded fetch of allowlisted CDN files copied into each version. |
| `artifactserve` | The artifact origin: the `api.artifacts` listener that serves artifact versions' bytes under a path-borne view ticket and re-decides every document request, plus the frame builder the API mints a viewer's frame through. `artifactserve/ticket` mints and opens the tickets |
| `artifactrender` | Renders a published version in an operator-run Chromium as a viewer sees it, for a seat checking its own page (`render_artifact`), and reports what it finds. |
| `artifactrender/internal/cdp` | The artifact render's Chrome DevTools Protocol client: one websocket to a browser, every session flat on it, declaring only the commands and events the render sends and reads. |
| `presence` | The node's presence hub: who is in each room (an artifact's or a chat's) and what they are looking at, told between nodes and never stored, each frame sealed one way under the keyring. |
| `provenance` | The one fence around text a model reads that somebody else wrote: a per-turn boundary, an author label and a trust level, one trust rule per surface, and a walk that holds every model call and every declared field to it. A leaf. |
| `trashreap` | The trash reaper duty: purges what has been trashed past its term, as the system, for every domain that keeps a trash (the tracker's removed work items first, then pages, then artifacts), under derived operation ids so a moved duty never purges twice |
| `objectives` | Service objectives as named constants with their reasons, plus the `Meter` every recording site calls and the `Beacon` that admits the dashboard's ready reports; alarms borrow thresholds from them (ADR-0015). |
| `api/artifactsapi`, `api/artifactdataapi` | The HTTP surfaces for artifacts and for their datastores on the API listener, behind one admission that decides each verb on the artifact before anything is read. |
| `api/datasetsapi` | A dashboard artifact's datasets, answered as their viewer, plus the engine source catalogue |
| `artifactspike` | Test-only: the artifacts capacity spikes as reproducible benchmarks over the shipped components; they gate nothing and run alone |
| `setup` | What an integration needs before it works, in one vocabulary every vendor and the dashboard share: requirements, the secret-then-pointer-then-epoch write order, vendor passes and credential checks |
| `vendortext` | The one form of the words a person follows at a vendor: plain words, the vendor's own UI names and menu paths, and each page linked on the words that name it. |
| `jsprovision` | The one place that decides how long a replicated JetStream create gets, how a forming cluster is re-asked, and how metadata reads are hedged; shared by `queue/jetstream` and `coord/kv`. |
| `jsinflight` | Waits out a conditional JetStream publish refused as in flight (10164), which decides nothing, so it is never read as a lost compare-and-set race (10071). |
| `jsapi` | Which JetStream API a connection speaks (embedded fleet domain vs external cluster), decided once; leader reads, hedged listings, pulls and write budgets for the JetStream clients. |
| `jwks` | The one cached, singleflight reader of a published JSON Web Key Set (RSA keys only): what the Forge relay verifies Atlassian's signed token against, and the one place that decides what a key set is trusted to say. |
| `logging`, `secrets`, `provision`, `envref`, `envfile`, `redact`, `workkey`, `backoff`, `cron`, `period` | The small shared grammars each package's doc describes, importing only the standard library, `words`, `plainjson` and one another, so any layer, `config` included, may use them. |
| `gitref` | The grammar of git names the engine and its config share: repository names, refs, branches, protection patterns, path globs, remote URLs, commit text and tag objects, standard library only (ADR-0008). |
| `regexlit`, `regexlit/regexlittest` | The literal substrings every match of a regular expression contains, derived once from its syntax tree so scans skip non-matching texts, and the generator that certifies it. |
| `skipgate` | Runs `go test -json` for both test pipelines and fails the run on a skip no allowlist entry declares or an `Always` entry that stopped skipping. It also writes the `-timings` file the partition reads and prints the run's CPU and slowest tests. |
| `fixedwait` | The gate that refuses a new fixed wait or elapsed-time reading in test code that no marker, bubble or inventory entry covers. |
| `clocktest` | Forges instants whose wall and monotonic clock readings have parted (a host that slept, a wall clock stepped), for tests of code that measures time across them. Imported only by tests |
| `solo` | Declares a test package must have the runner to itself; `solo/partition` computes `make test`'s and `make test-solo`'s halves and CI shards from the imports, and a roster guard fails both ways |
| `prereqtest` | Test support: the one rule for a program or directory a machine may lack (fail under the make targets, skip in a bare `go test`), plus pin-file reading. |
| `imagetest` | Test support: a container image a case runs, built from its Dockerfile once a process, held by the runtime, or loaded from a cache directory CI keeps between runs (`Provide`). |
| `e2e` | The gates only a whole running node can answer: a real engine, API and scripted-model turn, with the dashboard's own protocol module reading the frames that come out; no exported surface, runs in `make test-solo` |
| `docsgate` | Test-only gates holding the tree's markdown links, anchors, docs index, coordination-bucket inventories and the always-loaded agent instructions to the files and code they name |
| `commentgate` | Test-only: holds every comment block outside a package doc to 12 lines and every row of this file to one line, each against an inventory of older code that only shrinks (#1474) |
| `wiregate` | Test-only: every flag and number a client receives is sent at zero or its field says why it may be left out, and the dashboard's wire types declare each as it is sent, each against an inventory of older code that only shrinks |
| `docslinks` | Test-only guard that every docs-site link this repository carries names a page the site builds under its product and version segment. |
| `clientsource` | Finds a declaration the dashboard makes, by name and syntax rather than path or layout, so a gate can hold it against the engine's own copy |
| `sourcetree` | Where this repository's tree is for everything that reads its own files: the module root and a walk that skips VCS metadata, nested checkouts and `node_modules`; plus the module's one type-check loader for gates. |
| `sourcetree/decltype` | Types a Go expression from the declarations it reaches, read from source with no type checker: the one walk every static gate that judges a value by its type is built on |
| `scenario` | Test-only: synthetic timelines of a company's work, each the shape of a measured operating pattern and nothing else, held to a closed vocabulary so no company data enters; imports no engine package. |
| `conflictmarkers` | A test-only package that fails the build when a file in the tree still carries a merge conflict's markers; its package doc has the reasoning. |
| `adr` | The gate behind `adr/`: checks every decision record's fields, Authority, Enforced-by and citations, and that the index lists each |
| `version` | What the binary calls itself: the stamped release tag, else the module build info, else `dev`; `IsDevelopment` allowlists the unversioned spellings so the credential-free development principal never runs on an unknown one. |
| `testbinary` | Says whether this process is a test binary, for the few rules shipping code holds only a test to, and names the directories a test broker keeps its store in. |
