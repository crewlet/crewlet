# Atlas

The atlas is the map and catalogue of everything a company runs on: its vendor accounts, domains and host names, servers, databases, repositories, pipelines, monitoring, tool servers and models, and how they depend on each other. The engine fills it from what it already knows, and every fact in it says where it came from, when it was seen, and how far it can be trusted.

This page describes the model: what the atlas holds, where each fact comes from, how one thing seen by several sources becomes one entry, and what the atlas never holds. The decision behind it is [ADR-0126](../../adr/0126-the-atlas-computes-what-the-engine-knows-and-records-what-it-is-told.md).

## What the atlas holds

An **entry** is one thing the company runs on. Its kind is one of a closed set, and each kind sits in one lane of the map, read left to right the way a request travels, with the code, monitoring, tools and models in a band beneath.

| Kind | On screen | Lane | Example |
|---|---|---|---|
| `domain` | Domain | Entry | `acme.example` |
| `hostname` | Hostname | Entry | `api.acme.example` |
| `endpoint` | Endpoint | Entry | `https://api.acme.example/v1` |
| `service` | Service | Services | `checkout-api` |
| `host` | Server | Runtime | `web-1` |
| `datastore` | Database | Data | `orders-db` |
| `queue` | Queue | Data | `orders` |
| `bucket` | Bucket | Data | `acme-assets` |
| `vendor_account` | Account | Platforms | the Cloudflare account, the GitHub organisation |
| `repository` | Repository | Code and delivery | `acme/api` |
| `pipeline` | Pipeline | Code and delivery | a repository's `deploy` check |
| `monitor` | Monitor | Observability | "API 5xx" |
| `tool_server` | Tool server | Tools and models | an MCP server |
| `model_provider` | Model | Tools and models | a model entry and its vendor |

What a vendor does for the company (chat, tracking, code hosting, CI, monitoring, DNS, CDN, compute, a model API) is an account's **capabilities**, not a kind per product, so "which CDN do we use" is answered by the account that carries the `cdn` capability. A vendor the engine does not know yet is an account with the vendor `other` and a name.

**Relations** always point from the thing that relies to the thing relied on, so "what depends on X" is one walk against the arrows: `runs_on`, `depends_on`, `exposes`, `resolves_to`, `in_zone` and `fronted_by` are drawn on the map at rest; `served_by`, `built_from`, `deployed_by`, `monitored_by`, `belongs_to`, `documented_in` (a Doclet page) and `owned_by` (a seat or a unit) appear when an entry is selected.

**Facts** are a closed list per kind, each with a value type: a host name, an IP address, an address without credentials, a port, a version, a region, an account's own id, a branch, a short label, a word from a closed set. Facts of **meaning** (purpose, environment, importance, and the owner) are decided only by people and the settings; an integration or an agent can only suggest them.

## Where each fact comes from

Every fact is a claim from one source, and the atlas is one fold over all of them.

```mermaid
flowchart LR
    S["Company settings<br/>(the redacted copy)"] --> C["Claims, computed<br/>when the atlas is read"]
    R["The engine's own records<br/>(Gitlet's repositories and checks)"] --> C
    P["People, agents, integration reads<br/>and repositories' files"] --> L["Claims, recorded<br/>on the atlas's own log"]
    C --> F["One fold"]
    L --> F
    F --> V["The atlas: entries, facts,<br/>relations, sources"]
```

What the engine **already keeps** is computed every time the atlas is read and never stored: it cannot drift from where it lives, and it needs nothing written. What only **somebody can tell** the engine (a person's correction, an agent's observation, an integration's read beyond what the engine keeps, what a repository's files declare) is recorded on the atlas's own log ([What integrations read](#what-integrations-read), [What a repository's files declare](#what-a-repositorys-files-declare)).

Each claim carries a confidence the engine derives from its source and the evidence it checked. Nobody, and no model, chooses it.

| Where the fact came from | Shown as |
|---|---|
| The company settings, or the engine's own records | Confirmed |
| A person | Confirmed |
| An integration's read with the company's credential | Confirmed |
| A repository's deploy and dependency files, read at its default branch | Likely |
| An agent, backed by a server's or a vendor's own answer to a tool call in the same turn | Likely |
| An agent, backed by a repository file it read in the same turn | Likely |
| An agent, backed only by text anybody could have written (a message, a page, a work item) | Unconfirmed |

An agent's fact that nothing else supports is shown as stale after 30 days and no longer counts after 180 ([What agents record](#what-agents-record)).

### What the settings and Gitlet show

The settings are read only through the same redacting copy the configuration surface serves, so a credential cannot reach the atlas even by mistake.

| From | Entries |
|---|---|
| Each connected integration | its account, keyed by the account's own id where the settings carry one (the Cloudflare account id, the AWS account, the GitHub organisation), with its capabilities and whether the integration is on. Jira and Confluence on one site are one account |
| Cloudflare | each declared zone as a domain, served by and belonging to the account |
| GitHub and GitLab | each repository the settings name, belonging to the account |
| SSH | each declared server, with its address (marked internal where no public network routes to it), its port and whether it runs containers |
| `api.external_url` and `api.public.external_url` | the host name the deployment answers at, in its zone where the settings declare that zone |
| `providers.llm` and `providers.embeddings` | each model entry, its vendor, harness, model, sign-in mode and place in the fallback chain |
| `mcp_servers` | each enabled tool server, its transport, its address or the base name of its command (never its arguments) |
| `providers.sandbox` | the E2B account, where the sandbox runs boxes there |
| Gitlet | each repository the reader may read, with its default branch, visibility, status and owner, the hosts it mirrors from and to, and each check its default branch runs, with its latest result |

Gitlet's repositories are read as the person reading the atlas, so a repository only its unit may read appears only to that unit. A value the atlas would refuse (a label shaped like a token, an address with credentials in it, a `${VAR}` reference it does not resolve) is left out, and the read says where it sat and why, never what it was.

### What the engine's records add

The engine already keeps a record of what agents and integrations use. The atlas reads it at every read and stores none of it.

| Record | What it adds |
|---|---|
| Each integration's standing | the base address its registration at the vendor points at, on the integration's account; and, for administrators only, whether it waits on a person or its last pass failed |
| The ledger of agents' accounts | on each integration's account, the agents holding an account there: which seat, on which lane, at which level, and whether it is on |
| What each node's tool servers listed | each tool server's own name, version and number of tools; its instructions and its tools' descriptions are a vendor's text and are never read |
| Each tool server's sign-in | who issued it and when it lapses; a sign-in that lapsed or failed to refresh is a signal, and one lapsing within a week a warning |
| Each node's last beat | a tool server with a failed or waiting instance, and a model entry sitting out a spent plan or whose last call failed with no answer since |
| Connection tests | a server whose last test against its current settings failed, for administrators only; a test of an address or a key since changed says nothing |
| The access log | each server's use: commands agents ran there in the last week and month, the last one, and which seats |
| The usage history | each tool server's calls and each model entry's calls over the week and the month, and which seats made them |
| Gitlet | each repository's last push |

An entry whose use the engine records (a server, a tool server, a model entry, a repository) that nothing used for 30 days is marked **not used recently**: a fact about use, never about existence. It is said only when that use was read in full; when the usage history cannot be read, the use is unknown rather than zero.

A **signal** is something on an entry that needs attention now. It is read from the record that holds it on every read, never copied, so it cannot disagree with its source, and each says who may read it: what only administrators see elsewhere (an integration's standing, a connection test), they alone see here.

Records are read as closed projections: the fields a map needs and nothing else. An email address is never read, whether it is a vendor account's name, the account an agent's lane observed, who signed a tool server in, who ran a test or a model plan's sign-in account; ownership and use name seats.

Every read says which of these sources it read in full and why not where it did not. A source that fails leaves the rest of the atlas answering. A company that keeps no code in Gitlet has nothing there to read, so its Gitlet is read in full for every reader, and no repository of its own is judged by pushes Gitlet never saw.

### What integrations read

Beyond what the settings and the engine's records say, the engine reads each connected integration's vendor **whole** for the atlas, under the access the company's own credential already holds, and records what it found on the atlas's own log in that integration's voice. A fact read this way is **confirmed**, and the entry it is about is **discovered** until a person stands behind it.

| Integration | What its read adds |
|---|---|
| Cloudflare | the account's name; every zone of the account as a domain, served by and belonging to the account; every A, AAAA and CNAME record as a host name in its zone, resolving to a server known by its address (marked internal where no public network routes to it) or to the name its CNAME points at, and fronted by Cloudflare where proxied; and every certificate pack as a certificate in its zone, with the names it covers, its type, validity, issuer and expiry, securing each name of the zone it covers. MX and TXT records name nothing, and a TXT record's content, where verification tokens sit, is never decoded; comments and tags are never read |
| Datadog | the organisation's name and id on the account; every monitor, belonging to the account, with its name, its state (`ok`, `warn`, `alert`, `no_data` or `unknown`) and, while it is not healthy, since when; every service of the service catalogue, watched by the account and built from the repositories its definition links; every host that reported in the last three hours, by its Datadog name, watched by the account, with its environment suggested from its `env` tag; and what each monitor watches: the service its `service:` tags name and the host its `host:` tags name, by those exact keys, and for a running Synthetic test the address it requests and that address's host name or server. A monitor's query and message, a test's request headers, body and credentials, a definition's contacts, every creator and every other tag are never decoded |
| GitHub | every repository an agent's installation covers, belonging to the organisation's account, with its default branch, whether it is archived and its visibility on GitHub; every workflow a repository declares as a pipeline built from it, with its name, whether GitHub still runs it (`active` or `disabled`) and the result of its newest completed run on the default branch; and every lasting deployment environment as a pipeline built from its repository, with its name, the state of its newest deployment, an environment suggested where its name spells one, and the address and the host name that deployment put the code behind, each deployed by the environment by its exact key. It is read through the agents' own installations, under a token minted Read-only for the read and revoked after it ([what the atlas reads](../integrations/github.md#what-the-atlas-reads)). Nobody's login, name or email address, no deployment payload, description or log address, and no secret, variable or protection rule is ever decoded |

The read is the `atlas-collect` duty, one node of the fleet at a time ([singleton duties](seat-ownership.md#singleton-duties)), on the company's `atlas.refresh_interval_seconds`: an hour unless the company says otherwise, and never under five minutes, since a read shares the credential's rate-limit window with the [integration reconcile loop](integration-reconcile.md). It is a duty of its own rather than part of that loop, whose contract forbids a write on a converged pass and whose cadence is tuned to catching access somebody revoked.

**A read records only what changed.** The duty diffs each read against every claim the log already holds in the integration's voice and records one entry at a time: the facts and relations that are new or changed, and the ones the read no longer holds, which move into the entry's history. An unchanged read writes nothing, so an hourly read of a few thousand DNS records costs nothing an hour. Each recorded fact shows the instant of the read that last changed it; the source's own line says when it was last read whole.

**Retirement takes two complete reads a day apart.** A read that does not list an entry the integration holds is one miss, never a removal: a vendor's listing can skip a row while it changes, and a vendor's view can lag. The entry is retired only by a later complete read, at least a day after the first miss, that misses it too; it then leaves the map and stays in history, and a read that lists it again restores it. A read that failed or was cut short retires nothing and forgets no miss. An integration never denies a fact or a relation and never outranks a person: a person's claim on the same fact stands, and the entry goes to Review with both values shown.

**An integration says an entry exists only where the entry lives at its vendor.** A Datadog monitor lives at Datadog; the service it watches does not, so the read relates the service to the monitor without claiming the service. A monitor somebody deletes therefore takes its relations with it after the same two reads, and retires nothing: an entry only its relations named leaves the map, and one another source or a person stands behind stays as it was.

**A listing the credential may not read is a finding, never a failed read.** A vendor puts each listing behind a permission of its own, and the company's credential may hold some and not others. The read completes with the listings it may read, and the source's standing names each one it was refused with the permission that restores it. What an earlier read of a refused listing recorded stays exactly as it was: nothing is withdrawn, nothing is missed and nothing is retired, however long the refusal lasts. Any other failure of a listing fails the whole read.

**Nothing a vendor returns reaches the atlas as text.** Every value a read reports goes through the vocabulary's grammars, which refuse a value or a key shaped like a credential; what is refused is counted on the source, never shown. A source maps a vendor's answer into closed fields and reads no field nobody mapped.

Each source's standing, kept on the coordination store beside the integration statuses, says when it was last read whole, what that read cost in calls and withheld values, whether its last read failed and why, which listings it was refused, and which entries it has stopped seeing since when.

### What a repository's files declare

A repository says what it runs as and what its code calls, often before anything is deployed. The engine reads a **closed list** of deploy and dependency files from each Gitlet repository the whole company may read, at its default branch, and records what they declare in that repository's own voice ([ADR-0133](../../adr/0133-a-repositorys-deploy-and-dependency-files-are-read-from-a-closed-list-and-what-they-declare-is-likely.md)).

**It is a suggestion until a person confirms it.** A file states what somebody meant to run, not what runs, so everything read this way is **likely**, shown with the repository whose files said it, and the entry is **discovered**. It is never confirmed by the files alone, it never outvotes what an integration read or a person said, and it never sends an entry to Review. A person who confirms an entry makes it their own claim. A file sets no purpose, environment, importance or owner, and removes nothing: when a file stops declaring something, what it said is withdrawn, and whoever else stands behind the entry still does.

| File | What it declares |
|---|---|
| `Dockerfile` and `Procfile`, at the repository's root | by being there, never by being read: a service named for the repository, built from it. Their lines are build arguments, environment values and commands |
| `compose.yaml`, `docker-compose.yml` and their overrides | each service, keyed `<project>-<service>` the way compose names its containers (the project is the file's `name`, or the directory it is in, or the repository); a service whose image is a well-known database or queue (Postgres, MySQL, MariaDB, Redis, Valkey, MongoDB, Elasticsearch, OpenSearch, ClickHouse, NATS, Kafka, RabbitMQ) as that database or queue, keyed `<project>/<service>`, with its engine and the version its tag starts with; `built from` this repository where the service has a `build`, or from the forge repository where its image sits in that forge's registry (`ghcr.io`, `registry.gitlab.com`); and `depends on` for each `depends_on` between services of the file |
| Kubernetes manifests under a directory named `k8s`, `kubernetes`, `manifests` or `deploy` | each Deployment, StatefulSet, DaemonSet and CronJob as a service by its name, or as the database or queue its first container's image is known to run; and the database, queue or bucket a well-known operator's resource asks for (CloudNativePG, Zalando Postgres, MySQL, MariaDB, MongoDB, Redis, Elasticsearch, OpenSearch, ClickHouse, Strimzi Kafka, RabbitMQ, object bucket claims and Crossplane buckets), keyed `<repository>/<name>` |
| `Chart.yaml` of a Helm chart | the chart as a service built from this repository, and each dependency that is a well-known database or queue chart as one the service depends on. A values file is never read |
| `*.tf` and `*.tofu` | the vendors its providers name, and what its resource types say each is used for (compute, a database, object storage, DNS, a CDN, email, monitoring). No attribute's value is read, and variable and state files never are |
| `fly.toml`, and `render.yaml` at the root | the application as a service built from this repository and running on the company's Fly.io or Render account; a Render database as a Postgres database, and a key value service as one |
| `package.json`, `go.mod`, `requirements.txt` and `requirements-*.txt`, `pyproject.toml`, `Gemfile`, `Cargo.toml` | the vendors whose SDKs the code depends on, from a closed table of package names per registry. A vendor the engine knows is its account keyed by the vendor alone, since a file names the vendor and never which account; any other is an account with the vendor `other` and its name |

**What is never read.** A file that is not on the list is never read: an `.env` file, a values file, a variable or state file, a manifest named for the secrets it holds, anything in a directory whose name starts with a dot, and anything under a vendored, test or example tree are refused by their path, and Gitlet itself refuses a read of a file off the list. Inside a listed file, only the fields in the table are decoded: an `environment`, `env` or `env_file` block, a command, a build argument, a label, a Secret's or a ConfigMap's data, a dependency's version or address and a development dependency have no field to be read into. What is decoded reaches the atlas only through the vocabulary's grammars, which refuse a key or a value shaped like a credential and count it. A name is matched against closed tables and never guessed from a similar one.

**Relations are by exact keys only.** "This repository builds this service" is said where a file says so (a `build`, a chart, a platform file, an image in a forge's registry), and "this service uses this database" where a compose file's `depends_on` or a chart's dependency says so. Nothing is related by a similar name.

**When, and how much.** A repository is read when its default branch moves, never on a clock: the code log's record of the push asks for the read on the repository's home node, and a listing each minute catches a record that was missed. One read lists the tip's files in one call and reads only the files whose content changed since the last read; a push that touches no listed file reads nothing. A file past 64 KiB is counted and not read, at most 200 listed files of a repository are taken (the rest are counted), a file makes at most 128 claims, and one pass reads at most 50 files, a larger change being read over several passes and recorded only once it has been read whole.

**What is not scanned.** A repository only its unit may read (`visibility: unit`) is never scanned, because what a scan records is on the company's atlas log, whose history every holder of Atlas reads. A seat's scratch repository, an archived repository and one still being imported are not scanned either. What a repository's files declared is withdrawn when it stops being scanned, and when it is renamed its claims move to the new name.

Each scanned repository's standing, on the coordination store, says which commit was last read whole, how many files it listed and read and in how many calls, how many files were past a bound or were not their format, how many values were withheld, and the fault of its last read if it failed.

### What agents record

Agents meet the company's systems while they work: a server over SSH, a vendor through a tool server, a repository. An agent records what it met with `atlas_record`, and says that something the atlas holds is wrong or gone with `atlas_dispute` ([ADR-0132](../../adr/0132-an-agent-records-in-the-atlas-only-what-a-call-of-its-own-turn-showed.md)). Both are on unless the company sets `atlas.agent_records: false`.

**Only what a tool call of the same turn showed.** A record names, in `seen_in`, a tool the agent called in that turn, and the engine reads that call's own answer: a new entry's identifier, every fact's value and the entry a relation points at must stand in the answer, whole, or the call must have been an SSH call made on the server the relation points at. A value the cited answer does not show is refused, so an agent cannot record a host no tool returned. The engine keeps a turn's answers in memory for that turn alone, so a call of an earlier turn cannot be cited: the agent makes the read again.

**The engine decides how far the record is trusted**, from the tool the cited call was made with. The agent passes no confidence.

| The cited call | The record |
|---|---|
| `ssh_read` or `ssh_operate` on a server, or a tool of a tool server that reads the agent's own vendor credential | Likely: seen on the system itself |
| `code_read` or `code_search` | Likely: declared in code |
| Any other tool: a message, a page, a work item, a colleague's answer, a web read, the atlas itself | Unconfirmed, and hidden |

**An unbacked record stays hidden until a person confirms it.** No agent reads it back, the one that recorded it included, and a list or the map leaves it out unless asked. A person who confirms it with `atlas_confirm` makes it their own claim.

**What an agent never does.** It never removes or retires anything: a dispute stands beside what the atlas holds, never replaces it, and sends the entry to Review only where the cited call was a read of a server, a vendor or a repository. It never sets a purpose, an environment, an importance, an owner or what depends on what; those are a person's. It never says two entries are the same. It records nothing in a turn somebody outside the company woke, or a person without Atlas ([ADR-0109](../../adr/0109-a-turn-somebody-woke-is-held-to-the-apps-they-use.md)). A value or a key shaped like a credential is refused, even where a server printed it, and the refusal never repeats it.

**The caps.** A turn records at most 20 times, and an agent at most 200 times in a UTC day across its turns, counted for the whole fleet. A record of what the atlas already holds (the agent's own backed claim from the last week, or the same value from the settings, the engine's records, a person or an integration) writes nothing and counts against neither. After a week the agent's claim is written again, so a fact it keeps meeting does not go stale.

## Reading the atlas

People holding the `atlas` permission read the atlas in the dashboard, under Company › Atlas ([Reading the atlas](../guides/atlas.md)), and through seven questions, each also a REST route: a page of its entries, the map, one entry, a trace from one, the counts a filter menu offers, its own log as lines, and that log counted a day at a time ([The atlas](../reference/api-endpoints.md#the-atlas)). Each reads the atlas as its reader: Gitlet's repositories only for a holder of Gitlet, and an integration's standing or a connection test only for an administrator. A standing alarm on a tool server or a model entry, and the health the nodes beat, are every holder's.

An entry, a fact or a relation only an agent reported, with nothing the engine could check behind it, is **unconfirmed**. A list and the map leave it out unless the read asks for it (`unconfirmed`), and one entry read in full shows it marked as such: it stays hidden until a person confirms it. An agent is never shown it.

Each entry carries a **health**, the one word its open signals add up to: `alerting` where something is broken now, `needs_look` where a signal asks for a person or the sources disagree, `quiet` otherwise. A monitor's state is one of those signals, read off the monitor's own facts on every read: a monitor in `alert` makes itself and every entry a `monitored_by` relation joins to it `alerting`, and one in `warn` or `no_data` makes them `needs_look`, since the instant the vendor says the state began. A monitor that recovers, is retired or stops watching an entry takes its signal with it, and every holder reads these signals, as they read the monitor. A pipeline's last result is another: a pipeline whose last result is `failed` or `errored` makes itself and every entry a `deployed_by` relation joins to it `needs_look`, never `alerting`, since a failed run or deployment leaves what ran before it in place. An entry that is not quiet says why in one sentence, its **reason**, built from the engine's own words for its signals and never from text a vendor, an agent or a person wrote. Every row also says who answers for the entry (the owner a person or the engine's records name, never one only suggested), the one line of purpose a person gave it, and how many entries it relies on and how many rely on it.

Relations are drawn in three **families**: `calls`, what a request travels along (`depends_on`, `exposes`, `served_by`, `fronted_by`); `runs_on`, what an entry stands on (`runs_on`, `resolves_to`); and `around`, everything else (its zone, what builds, deploys, watches and secures it, the account it belongs to). The map answers the relations it draws at rest, or every relation between the entries it draws when asked for all. A **trace** walks against the arrows for what depends on an entry and along them for what it depends on, through the relations the map draws at rest unless asked for all.

### What changed, and when

The atlas's own log is also read as a question: what changed, when and in whose voice, newest first, for the company or for one entry. A **line** is one claim recorded on the log, by a person, an integration's read or an agent: an entry recorded or retired, a fact set or marked wrong, a relation or an owner recorded or removed, two entries joined or kept apart. A claim an integration's read stopped holding is a line of its own, at the read that withdrew it, so a fact that went away is as visible as one that arrived. What the engine computes at every read from the settings and its own records is never recorded, so it leaves no line: activity is a count of the log's lines, and nothing else.

The same log is **counted**: its lines on each of the last 14 days, for the company and for each entry, and each entry's newest line however old. A day is a UTC day, so every node counts the same. That is the whole of what "activity" means in the atlas: there are no request rates, latencies or error budgets in it, because the engine holds none.

A line is shown only to a reader who is shown its entry and the entry it names. A line on a repository or a check of Gitlet's, or naming one, is read only by a reader Gitlet lists that repository to; a line on an unconfirmed entry is left out with the entry; and the counts are of exactly the lines the reader would be shown.

## One thing, one entry

An entry's identity is its kind and its natural key, and its id is derived from the two, so every source that meets the same zone names the same entry. Two entries of one kind that share an **exact** key are one entry when the atlas is read:

- a repository and a Gitlet repository that mirrors it, by the forge address;
- two accounts named by the same account id;
- two servers at the same public address, seen by different sources.

A key one source carries on two entries (two servers behind one load balancer, two copies of one mirror) is a shared address, not an identity, and merges nothing. A private address never merges two servers, which may sit on two networks. An agent's report never merges anything. A person may join two entries no key joins (a declared server and the cloud instance it is) by saying they are the same, and may keep two entries apart by saying they are not; a person's "not the same" always wins, and similar names never merge: they only suggest.

**A merged entry still has a visibility on each host.** Most facts are the entry's: a repository has one default branch, and two sources that report two are a disagreement, shown and sent to Review. A repository's `visibility` is different: it is a fact of each host the repository lives on, in that host's own words (`public`, `private` or `internal` on a forge; `company` or `unit` on Gitlet). A repository that is private on GitHub and mirrored on Gitlet for the whole company carries both, each named for its host, and the two are never held against each other. Two voices on one host that disagree (a person who recorded `public`, GitHub now reading `private`) are still a disagreement. A value in another host's word is refused, and a person corrects a host's visibility by naming that host's repository.

## When sources disagree

For each fact, a source's newest claim stands for it, and across sources the most trusted wins, then the newest. A person's value is shown over every inference. An agent's dispute never wins: it is shown beside the value it disputes. When a newer report from an integration, an agent or the engine contradicts a person, the fact is **contested**; when two sources the engine trusts alike disagree with no person to settle it, it is **disputed**. Either way both values are shown, and the entry waits for a person; nothing is settled in silence.

## Changing the atlas

A person who holds Atlas changes it with eleven tools of the operator catalogue, the same catalogue the dashboard writes through ([`/operator/act`](../reference/api-endpoints.md#operatoract--the-dashboards-write-surface)) and your own assistant reaches over `/operator/mcp`. Each records a claim in the person's own name, which outranks every integration and agent until they report a change, when both values are shown for review:

| Tool | What it records |
|---|---|
| `atlas_add_entry` | an entry no source reports, by its `kind` and `key`, with a `name` where the key is not one |
| `atlas_correct` | one fact's `value` as the person knows it (for a fact that holds many, one more value), or with `wrong`, that a value shown is wrong |
| `atlas_confirm` | what the atlas shows, as the person's own: the entry, one fact (or another value a source reports for it) or one relation |
| `atlas_relate`, `atlas_unrelate` | a relation between two entries, drawn or recorded as not there |
| `atlas_set_owner` | the owning `seat` (by handle) or `unit` (by key) of the chart, in place of the owner a person named before; `clear` removes it. Never free text |
| `atlas_retire`, `atlas_restore` | an entry taken off the map, kept in its history, and brought back |
| `atlas_merge`, `atlas_split` | two entries of one kind said to be the same thing, or not the same |
| `atlas_purge` | an entry or a value erased everywhere ([below](#the-atlass-own-log)) |

**A write is decided on what the person reads.** Each reads the atlas as its caller does, Gitlet only for a holder of `gitlet` and what people recorded at the person's own level, after reading the version of every entry it names; every claim is recorded on that version, so a change somebody made in between refuses the write rather than being written over. A caller who decided from an earlier read names the version it read as `if_match`, and every write answers the entry's new `version`. A write that asks for what already holds records nothing and answers `applied` with `unchanged`. A write that records several claims (an owner replaced, a merge that withdraws a "not the same") records them in an order that leaves the atlas honest if it stops part of the way, and the same call made again finishes it.

**What is declared elsewhere is changed there.** A fact the company's settings, Gitlet or the engine's own records declare is never written over in the atlas: a server the SSH settings declare is not retired here, nor its address corrected, and a tool server, a model, a server keyed `ssh/` and a Gitlet repository, which the engine names, are not added by hand. A Gitlet repository is changed only by a holder of `gitlet` who can read it, since the atlas is never a way round another permission. What the settings leave unsaid (a server's importance, a service's owner) is a person's to say.

**Values go through the vocabulary's typed grammar, keys included.** A key or a value shaped like a credential is refused, never stored masked, and the refusal never repeats it.

**"The same" and "not the same".** A merge joins two entries no exact key joins, and withdraws a "not the same" a person recorded between them first. A split keeps two entries apart whatever key they share, and always wins over a merge nobody made.

**A purge is an administrator's**, after a fresh sign-in, and never an agent's. It names an entry, or a value exactly as the atlas holds it; a value the settings or the engine's records declare is removed there first. The value is never repeated in an answer, and only its digest leaves the node the purge was asked of.

**A node without data writes too**, through a data node that publishes, as it reads. Every write is audited as every operator call is, and **no agent changes the atlas through these tools**: the authority table refuses every one of them to an agent whatever it holds, and no seat's registry holds them. An agent has two tools of its own ([What agents record](#what-agents-record)). Each is made as the seat its person holds, so a credential bound to no seat records nothing in a person's rank.

## The atlas's own log

What people and agents record is kept on a state log of the atlas's own, `CREWLET_ATLAS_LOG`, applied identically on every data node, so whichever node a person reaches answers the same atlas, and every read of the atlas folds it beside what the engine computes. A node that holds no data, such as one that only runs agents, reads and writes it through a data node. Each entry is one subject on it: an edit names the version of the entry it was decided from, and is refused if somebody else changed the entry since. A claim is never overwritten: a newer claim from the same person on the same fact replaces the older one in the view, and the older one stays as the entry's history. An integration's read is one record per entry it changed, in the integration's voice, carrying the claims that are new or changed and withdrawing the ones the read no longer holds into the same history ([What integrations read](#what-integrations-read)).

**History** is kept for the company's `atlas.retention_days`, 400 days unless it says otherwise (90 to 3650): a replaced value, and an entry a person retired that nothing has reported since, go once they are older than it. What the atlas holds now never expires.

**A purge** is for a value that must not be kept: an internal name recorded by mistake, or a value a newer check recognises as a credential. It erases an entry (every claim on it and every claim naming it), or a value (every claim carrying it, and every entry it is the key of), on every node and from the history, and it leaves a mark that keeps anybody, and any later report, from recording it again. The log names a purged value by its digest and never writes the value itself. The log's own records age out with its trim, and a backup taken before the purge keeps what it copied until the backup's own retention. Only an administrator purges, never an agent.

## What the atlas never holds

Secrets, tokens, passwords, private keys, signed addresses, connection strings, environment values, command lines, command output, file contents and raw vendor answers have no value type to go in, and a value shaped like a credential is refused rather than masked. A person's contact details are never read: ownership names a seat or a unit. Addresses on private networks are kept, because they are the company's own map, and marked as internal.

## Who reads it

Reading the atlas takes the **Atlas** permission (`atlas`), the one app a person is given only on purpose: an invitation and a person's create leave it unticked, and an administrator holds it as every app ([ADR-0127](../../adr/0127-reading-the-atlas-takes-the-atlas-permission.md), [Atlas is given on purpose](identity-and-access.md#atlas-is-given-on-purpose)). A person holding it reads every entry, private addresses included. The atlas is never a way round another permission: its repositories are read as the reader, so a holder who does not use Gitlet is shown none, and the answer says Gitlet was not read and why.

A person reads it with three tools on the operator surface (`/operator/mcp`), the ones an agent holds: `atlas_search` for every entry, or those a `query` or a `kind` names; `atlas_get` for one entry in full, by its id or its exact name or key, with what relies on it; and `atlas_trace` for what depends on one entry, or what it depends on. Each answers beside which sources were read in full and how many values were left out for being shaped like a credential, which are counted and never shown ([The company's atlas](../guides/tools-and-mcp.md#the-companys-atlas)).

## Agents read the atlas

An agent seat holds the Atlas permission as it holds every app, and the same three tools, and in a turn somebody woke it reads the atlas only if they hold it too ([ADR-0109](../../adr/0109-a-turn-somebody-woke-is-held-to-the-apps-they-use.md)). Two things differ from a person's read:

- **An internal address only within its own reach.** An agent is shown a private address, or a host name on a private network, only on an entry its own access reaches: a server its SSH access names, a tool server it is granted, a model it calls, a repository it may read, or an account it holds. Any other entry keeps its name and loses its private values, an entry whose key is itself a private address is left out with every relation to it, and the answer counts what it left out as `private_withheld`. The atlas tells an agent nothing about the inside of the network that its own access does not already reach.
- **Nothing only an agent's unbacked report says.** An entry, a fact or a relation an agent recorded with no evidence the engine could check is left out of every agent's read, so one agent's report never reaches another as a fact.
- **Text somebody else wrote.** Every answer carries who wrote what it holds, and is read at the least trust any of them earns: the settings, the engine's records, a person and an agent as a colleague's, and a vendor's listing, which names no writer, as an outside party's.

A turn whose ask names an entry exactly is also handed a few lines about it before it starts, so a seat asked about `orders-db` reads what the company knows before it probes a server ([Systems in this work](agent-runtime.md#systems-in-this-work)).

## Every piece of state, and who has to agree on it

| State | Who has to agree | Where it lives |
|---|---|---|
| What the engine already keeps: the settings, Gitlet's rows, integration standings, agents' accounts, tool server listings and sign-ins, node beats, connection tests, the access log and the usage history | already agreed where it lives | not stored: computed when the atlas is read |
| Entry states, merges on exact keys, contested and disputed facts, signals, use and "not used recently" | nobody: computed on every read | the read itself |
| What people record: entries, facts, relations, owners, "the same" and "not the same", purges, with their history | every data node identically, with history | the atlas log, `CREWLET_ATLAS_LOG`, and its replicated tables |
| What an integration's read found that the log did not hold: a changed fact, a withdrawn one, a retirement | every data node identically, with history | the same log, one `read` record an entry, in the integration's voice |
| What agents record: an entry, a fact, a relation or a dispute, with the turn and the evidence behind it | every data node identically, with history | the same log, one `claim` record each, in the agent's voice |
| What a repository's deploy and dependency files declare, and what they stopped declaring | every data node identically, with history | the same log, one `read` record an entry, in the repository's voice |
| Each scanned repository's standing: the commit last read whole, the files listed and read, the calls, and what was refused | the whole company, current value only | the coordination store, in a bucket of its own |
| What each file declared at the content last read, so an unchanged file is not read again | the repository's home node alone, while it runs | that node's memory; a node that lost it reads the repository's listed files once more at its next move |
| What a turn's tool calls answered, which a record cites | the node running the turn, for that turn | the turn's own memory, never written anywhere |
| An agent's records today, against its allowance of 200 | the whole company, now | the coordination store's shared counter, a UTC day wide |
| The log's lines, its daily counts, an entry's last change, a row's owner, purpose, relation counts and reason, and a relation's family | nobody: computed on every read, from the log's rows and the fold | the read itself |
| Each source's standing: its last complete read and that read's digest, calls and withheld values, its last fault, the listings it was refused, and the entries it has stopped seeing since when | the whole company, current value only | the coordination store, apart from the integration statuses |
| Which node reads the vendors | the fleet, one holder at a time | the `worker:atlas-collect` lease |
| How often: `atlas.refresh_interval_seconds`, and whether agents record: `atlas.agent_records` | the company | the company document |
