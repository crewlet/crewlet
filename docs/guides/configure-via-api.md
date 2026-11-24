# Configure Nimbus via the `/config/*` API

Recipe for bootstrapping the [`examples/nimbus.company.yaml`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) company against a running engine: first the one-shot import of the whole file (recommended), then the per-entity edits that evolve the company live, its org chart included.

Every request below assumes:

```bash
export CREWLET_URL="http://localhost:8000"   # examples/nimbus.config.yaml binds the embedded API on port 8000
export TOKEN="$CREWLET_API_TOKEN_FOUNDER"   # matches api.auth.tokens[].token in crewlet.yaml
export AUTH="Authorization: Bearer $TOKEN"
```

Before you start, `/health` should report `{"status":"unconfigured","configured":false}` (still HTTP **200**: the code is liveness). A booting node says `"status":"starting"` (`503` while it recovers what it holds, then `200`), and every other route answers `503 starting` until it serves: wait for `unconfigured`, as [While a node is starting](../reference/api-endpoints.md#while-a-node-is-starting) describes. `/ready` is no signal: it answers `503` throughout the boot and `503 unconfigured` until the first PUT. After the first PUT `/health` reports `{"status":"ok","configured":true}` for the engine's lifetime.

```bash
curl -s $CREWLET_URL/health
```

See the [Configuration concept doc](../concepts/configuration.md) for the two-tier split and the rationale, and the [API endpoints reference](../reference/api-endpoints.md) for status codes.

---

## Option 1 — Import the whole file (recommended for bootstrap)

`crewlet config import` validates the whole `company.yaml` and sends it to the node's `PUT /config`, which persists a new revision and activates it; every node converges on that epoch and spawns the company (see [Control Plane](../concepts/control-plane.md)). It authenticates with `CREWLET_API_TOKEN`, like every command that talks to a node:

```bash
export CREWLET_API_TOKEN="$TOKEN"
crewlet config import examples/nimbus.company.yaml \
  -config examples/nimbus.config.yaml -api "$CREWLET_URL" \
  -summary "bootstrap Nimbus"
```

Verify:

```bash
curl -s $CREWLET_URL/health                                       # configured: true
curl -s $CREWLET_URL/config -H "$AUTH" | jq '.name'               # "Nimbus"
curl -s $CREWLET_URL/config/revisions -H "$AUTH" | jq '.[0]'      # newest first
curl -s $CREWLET_URL/agents -H "$AUTH" | jq 'length'              # 7 agent seats spawned
```

### The same write, over the API alone

The command is a `PUT /config` of the whole file:

```bash
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: bootstrap Nimbus" \
  --data-binary @examples/nimbus.company.yaml
```

Every write needs a revision summary, in the `X-Summary` header or a top-level `_summary` key in the body:

```bash
curl -X PUT http://localhost:8000/config \
  -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  -H "Content-Type: application/yaml" \
  --data-binary $'_summary: bootstrap Nimbus\n'"$(cat nimbus.company.yaml)"
```

The body key is for callers that control only the body (a form post, a header-stripping proxy, a CI step piping a document). It is **removed before the document is parsed**, so it never trips Tier B's unknown-field check. When both are present the **header wins**: it is the more explicit channel, and a stale `_summary` can linger in a versioned document.

Response is `201 Created` with the new `revision_id` and `epoch`, the `warnings` about the document (a lead or `manages` entry naming nobody, for example) and the `derived` hierarchy it will run. See [What a write answers](../reference/api-endpoints.md#what-a-write-answers).

**Every seat's handle and every unit's key is written into the stored document.** A seat with no `handle` gets the one its `name` derives, and a unit with no `id` gets one minted from its name, before the revision is stored. So correcting a name by editing the stored document (`GET` then `PUT`, `PATCH`, the per-entity routes below) keeps the identity, and the seat its agent id, mailbox and memory. A file you import is minted afresh each time: correct a name in one that omits `handle` or `id` and the import replaces that seat or unit. Declare both in any file you keep, or start from `crewlet config export`, which writes all of them.

To check a document without writing it, send the same request with `?dry_run=true`. Nothing is stored or activated and no summary is needed. The answer is `200 {"valid": true, "base_revision_id", "warnings", "derived"}`, each warning marked `"introduced"` when this write brings it rather than the active revision already carrying it, or the refusal the write would get:

```bash
curl -X PUT "$CREWLET_URL/config?dry_run=true" \
  -H "$AUTH" \
  --data-binary @examples/nimbus.company.yaml
```

A refusal names each failure in `detail` and again in `problems`, one located, classified entry per failure with its `path`, `segments` and `kind` (and the `line` when the parser found it). See [Refusals carry located problems](../reference/api-endpoints.md#refusals-carry-located-problems).

The body is read as YAML (JSON is a subset), whatever `Content-Type` says. A JSON body works too:

```bash
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" \
  -H "Content-Type: application/json" \
  -H "X-Summary: bootstrap Nimbus" \
  --data-binary @nimbus.company.json
```

**The active revision is the fleet's.** `GET /config` and its `ETag`, the revision list's `is_active`, and the base every write is built on are the revision the fleet's activation pointer names, the same on every node. A node still applying the last write, or one that refused it, answers the last write all the same, so an `ETag` read from one node is a good `If-Match` on any other. What each node runs is separate: [`GET /fleet`](../reference/api-endpoints.md#get-fleet) reports it per node as `config_serving_revision_id`. While the coordination store cannot be read, these requests answer `503 unavailable` with a `Retry-After` (never anything from the node's own store); send them again, **unless the body carries `"outcome": "unknown"`**. That is a write that stored its revision and got no answer to its activation, which may have landed: the body names the revision as `stored_revision_id`, the one it was built on as `your_base` and the storing node as `node`. Sent again, it makes a second revision over the first if that one landed. Instead, once the store answers, read `GET /config`: if its `ETag` names `stored_revision_id`, the write landed; if not, `POST /config/revisions/{stored_revision_id}/revert` makes it the company, sent to that node (by its own address where several share one), because the revision is in that node's history alone and any other answers `404`. See [Status codes](../reference/api-endpoints.md#status-codes) and [Control Plane](../concepts/control-plane.md#every-config-answer-is-the-pointers).

To have the write refused if anything else has touched `/config` since you read it, supply that read's `ETag` as `If-Match`:

```bash
REV=$(curl -s -o /dev/null -D - $CREWLET_URL/config -H "$AUTH" | awk -F'"' '/^[Ee][Tt]ag:/ {print $2}')
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" -H "If-Match: $REV" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: bootstrap Nimbus" \
  --data-binary @examples/nimbus.company.yaml
# 412 revision_advanced if the fleet's revision moved past $REV between read + write
```

Take the tag from `GET /config`, not from the top of `/config/revisions`: that list is this node's history, and its newest row need not be the fleet's (a write that lost a race is kept there, inert). Without `If-Match` a write is built on the fleet's revision as the node reads it, and refused `409 revision_advanced` if another write lands before its activation. With `If-Match` the same race is `412`, and `If-Match` also refuses an edit of a revision that had moved before you sent it, before anything is built.

---

## Option 2 — Evolve a live company one entity at a time

Five collections are addressable on their own: **roles**, **units**, **llm-providers**, **llm-fleets** and **mcp-servers**. Use them to change one thing in an active company; use Option 1 to bootstrap, and for anything the five do not cover (the identity block, integrations, the turn engine, the knowledge scope).

```
PUT /config/roles/{handle}
PUT /config/units/{id}
PUT /config/llm-providers/{key}
PUT /config/llm-fleets/{key}
PUT /config/mcp-servers/{name}
```

Each is addressed by what the *document* resolves it by, never its display name: a seat by its handle, a unit by its key (`id`), a provider by its key under `providers.llm`, a [fleet](../concepts/llm-fleets.md) by its key under `providers.llm_fleets` (`default-fleet` included while synthesised), a server by its `name`. `GET` on the collection lists exactly those.

Why not `PUT /config`? That makes every edit company-wide: changing one seat's goal means sending a document carrying every other seat, provider and integration, and a concurrent edit anywhere in it is yours to lose. A per-entity write narrows what you claim to have changed, which makes the revision summary meaningful.

### The loop

Read the entity, edit it, send it back. The read is the redacted document `GET /config` serves, sliced:

```bash
# What the collection holds
curl -s "$CREWLET_URL/query/config_entities?kind=roles" -H "$AUTH" | jq

# One entity. The response IS the entity, so it goes straight back.
curl -s -D headers.txt "$CREWLET_URL/config/roles/ceo" -H "$AUTH" > ceo.json

# Edit ceo.json, then send it back — quoting the ETag the read returned, so a
# concurrent activation is refused rather than silently overwritten.
curl -X PUT $CREWLET_URL/config/roles/ceo \
  -H "$AUTH" -H "Content-Type: application/json" \
  -H "If-Match: $(awk -F'"' '/^[Ee][Tt]ag:/ {print $2}' headers.txt)" \
  -H "X-Summary: give the CEO a quarterly goal" \
  -d @ceo.json
```

The `config_entities` query lists a collection and answers a `{kind, id, entity}` envelope; the dashboard reads it. For one entity prefer `GET /config/{kind}/{id}`, whose body is exactly what `PUT` takes.

The response is `201 Created` with the new `revision_id`, `epoch`, `warnings` and `derived` hierarchy, as a full PUT would give: one entity changed, one revision created.

### What a write actually does

It is not a patch protocol. The engine opens the active revision, splices your entity in, restores the credential masks the read showed you against that same revision, **validates the whole document**, and stores the result. Consequences:

- **The whole company is validated, not just your entity.** A seat naming an `llm` provider that no longer exists is fine on its own and breaks the company; you get `400 validation_error` naming the field, though nothing in your body is about it. Executor rules apply too: a company's first `providers.llm` entry, added while it has an agent seat, is refused at `providers.sandbox` unless the document already configures one (every agent seat's executor runs in a box from that block, which is no collection of its own, so add the two together with `PUT /config`). An entry or a seat's `llm` that leaves a seat's executor chain with no `claude-code`, `codex`, `gemini-cli`, `qwen-code`, `opencode`, `copilot`, `grok`, `muse-code`, `kimi-code`, `hermes` or `pi` member is refused, at the seat's `llm` or at the `harness` of the entry a seat with none falls to.
- **An unknown field is refused, not dropped.** A body carrying `heders` for `headers` is `400 invalid_body` naming the field, as the whole-document parser does. Ignoring it would answer `201` and store a server that sends no credentials.
- **A plain `PUT` never creates.** An id the active revision does not carry is `404 no_such_entity` (far more often a typo than an intent to add). To ADD an MCP server or an LLM provider, send the same `PUT` with `If-None-Match: *`, the create-only condition at the new entity's own address. It is added (an MCP server after every server already declared, so no seat's tool block moves) and the whole company validated; a name already taken is `412 entity_exists`. Do not send `If-Match` beside it (`400 conflicting_preconditions`): the create lands on the revision active when it commits, compare-and-set. A seat and a unit are not created by address (`400 not_creatable`), since each has a place in the chart the path does not name: add one by replacing the unit it sits in, or through `PUT /config`; see [Adding, moving and removing seats and units](#adding-moving-and-removing-seats-and-units).

  ```bash
  curl -X PUT http://localhost:8000/config/mcp-servers/linear \
    -H "Authorization: Bearer $CREWLET_API_TOKEN" \
    -H "If-None-Match: *" \
    -H "X-Summary: add the linear server" \
    -d '{"name":"linear","transport":"http","url":"https://mcp.example.com",
         "headers":{"Authorization":"${LINEAR_TOKEN}"}}'
  ```
- **The path is the identity, and a `PUT` never renames.** A body whose handle differs from the path's is `400 identity_mismatch`. `PUT
  /config/roles/ceo` replaces whatever is at `ceo`. A seat's handle and a unit's key are permanent (the seat's durable id derives from its handle, so a different handle is a different seat with an empty mailbox and no memory), and a server's name is the key every seat declares its credentials under, so nothing that references the old name travels with the splice. A body that omits `handle` (or a unit's `id`) keeps the path's, so editing a display name is never a rename.
- **`PUT` is the only verb, but for removing a model, a fleet or an MCP server.** A seat or unit is removed by a whole-document edit (export, edit, `PUT /config`): unlike a create, a delete leaves every `manages:`, `lead:` and `unit:` that names it pointing at nothing, so it should land as one reviewable revision of a document you looked at. A model and a server are the exceptions, because what names each has a shape the route can ask for. `DELETE /config/llm-providers/{key}` takes the repoint of every seat job and Ask Crewlet target that names the entry, by seat and job, and refuses with the list of them while one is unanswered, so removal and repoints land as one revision ([the route](../reference/api-endpoints.md#delete-configllm-providersid-remove-a-model-and-repoint-what-names-it)); a fleet listing the entry is answered by `{"fleet": "<key>"}`. `DELETE /config/llm-fleets/{key}` repoints every seat job and helper on the fleet the same way, and `default-fleet` is never removed ([the route](../reference/api-endpoints.md#delete-configllm-fleetsid-remove-a-fleet-and-repoint-what-runs-on-it)). `DELETE /config/mcp-servers/{name}` with `"clear": true` takes every `mcp_env` block and box entry that names the server with it, refuses with the list of them otherwise, and can delete the stored values only the server read ([the route](../reference/api-endpoints.md#delete-configmcp-serversid-remove-an-mcp-server-and-clear-what-names-it)).

A write keeps what the node's own build cannot represent: during a rolling upgrade a node may hold a document a newer node wrote, with settings its `GET` cannot show, and whatever you send through it, those settings survive on every seat, unit and MCP server matched by identity. See [Fields a newer build wrote survive every write](../reference/api-endpoints.md#fields-a-newer-build-wrote-survive-every-write).

### `X-Summary` and `If-Match`

Both work as on the full PUT, and `X-Summary` is **required** (the history is what someone reads at 3am to find the breaking change). Before the company exists (no activation, no company known to the node) a per-entity write answers `409 no_active_revision`: there is nothing to splice into.

---

### Adding, moving and removing seats and units

A seat or unit has a **place** (the unit it sits in) that the entity path does not name, so neither is created or moved by address. Change the unit that holds it: read it with `GET /config/units/{id}`, add, move or drop the seat in its `roles`, or the unit in its `children`, and `PUT` it back, or make the change in the whole document with `PUT` or `PATCH /config`. A seat at the company root is added through the whole document. The answer's `derived` hierarchy shows where everything landed.

**A human seat something holds cannot be taken away.** A write that removes one, or turns it into an agent seat, is refused `409 seat_held` while the [identity directory](../concepts/identity-and-access.md) binds somebody to it at any stage short of their removal, or while an open invitation names it (an invitation holds its seat until redeemed, cancelled or lapsed). The answer names the one thing holding each seat under `held`: a holder by `person`, `kind`, `login` and `stage`, or an invitation by its `invitation` id and `expires_at` (never the invitee's address). The remedy follows the holder, each a gesture for an administrator:

| What holds the seat | Free it with |
|---|---|
| A person | Move them to another human seat (`PATCH /iam/people/{person}` with `seat`, or `crewlet iam bind <person> <seat>`) or remove them. A person always holds a seat, so unbinding one is refused |
| A service account | Unbind it (`PATCH /iam/people/{person}` with `seat: ""`, or `crewlet iam unbind <person>`) or move it |
| An open invitation | Cancel it (`DELETE /iam/invitations/{invitation}`, or `crewlet iam cancel-invite <invitation>`) |

Then send the write again. A node that cannot read the directory refuses such a write `503 seat_holders_unavailable`. An offline `crewlet config import` has no directory to ask, so the binding it strands is reported by `crewlet iam check`, and an invitation to a seat it removed is refused when somebody tries to redeem it.

### A lead editing their own team

A person who leads a unit needs no `admin` to change what is inside it. Signed in as themselves, they read and write the seats and units of their team (the unit they lead and every unit beneath it) through the same entity routes, with the same `If-Match` and `X-Summary`:

```bash
# Read one seat of the team, and its ETag
curl -si $CREWLET_URL/config/roles/sre -H "$AUTH"

# Check the edit first: a dry run judges it exactly as the write will
curl -X PUT "$CREWLET_URL/config/roles/sre?dry_run=true" -H "$AUTH" \
  -H 'Content-Type: application/json' -H "If-Match: \"$REV\"" --data-binary @sre.json

curl -X PUT $CREWLET_URL/config/roles/sre -H "$AUTH" \
  -H 'Content-Type: application/json' -H "If-Match: \"$REV\"" \
  -H "X-Summary: give the SRE its on-call goal" --data-binary @sre.json
```

Everything the write changes must be inside their team, where it was and where it lands: a seat added, edited, moved between two of their teams or removed, a sub-team added or removed, the seats and sub-teams inside their unit reordered, their own unit's name or purpose. These take `admin`: handing their unit to another `lead:`, removing, moving or reordering it among its siblings, naming somebody outside it in a `lead:` or `manages:` entry, setting or changing a project, space, channel, address or contact id, changing a credential, naming a `${VAR}` in any field, and any setting. A write that reaches past them is refused whole: `403 unauthorized` with a `refused` list naming every part, the place it reaches, why, and the `reason` and `grants` of its own, beside the write's `reason` and the permission that would admit all of it; see [A lead edits their own team](../reference/api-endpoints.md#a-lead-edits-their-own-team). A seat or unit outside their team, and a name the company does not have, is refused `403` at its own address before the body is read, in the same words either way. The whole document (`GET /config`) stays `admin`'s, so a lead uses the entity routes. While the document is [managed by another system](#when-another-system-manages-the-document), a lead's edit is refused `403 config_managed` like everybody else's.

## Read paths

```bash
# The active revision, the fleet's on every node (JSON or YAML)
curl -s $CREWLET_URL/config -H "$AUTH" | jq
curl -s "$CREWLET_URL/config?format=yaml" -H "$AUTH"

# Revision history (newest first)
curl -s "$CREWLET_URL/config/revisions?limit=20&offset=0" -H "$AUTH" | jq

# Single revision incl. payload
curl -s $CREWLET_URL/config/revisions/$REV -H "$AUTH" | jq

# Structural diff between two revisions (or against active)
curl -s "$CREWLET_URL/config/revisions/$REV/diff" -H "$AUTH" | jq
curl -s "$CREWLET_URL/config/revisions/$REV/diff?against=$BASE_REV" -H "$AUTH" | jq

# Revision metadata for ops scraping (no payloads) — a query, not a REST route
curl -s "$CREWLET_URL/query/config_audit?limit=50" -H "$AUTH" | jq
```

---

## Revert

Re-activate any historical revision as a new active revision (the audit chain stays intact via `parent_revision_id`):

```bash
curl -X POST $CREWLET_URL/config/revisions/$REV/revert \
  -H "$AUTH" -H "X-Summary: revert — bootstrap was missing role X"
```

The dashboard sends the same request from a revision's page in Settings › Configuration (**Revert to this revision**), after showing what it changes via `GET /config/revisions/$REV/diff` (the comparison against `active`). Its **Reload** is `POST /config/reload`; its **Edit** reads the document as YAML, checks a draft with `PUT /config?dry_run=true` under the read's `ETag` and saves it with `PUT /config`. A revert and a reload read no body, so their summary travels in `X-Summary` alone, as UTF-8 bytes (how the engine reads every header's text).

---

## When another system manages the document

When something else is the source of the company (a GitOps pipeline applying `company.yaml` from a repository, or tooling such as a Kubernetes operator rendering it from custom resources), it runs these same requests with its own token and replaces whatever is active each time it reconciles. A person's edit in between lasts until then, with no sign it was lost.

Give the managing system its own token and name it as the document's only writer in every node's Tier A:

```yaml
api:
  auth:
    tokens:
      - id: gitops
        token: "${CREWLET_API_TOKEN_GITOPS}"
        grants: [admin]
      - id: founder
        token: "${CREWLET_API_TOKEN_FOUNDER}"
        grants: [admin]
    company_writers: [gitops]
```

The list names a credential rather than granting one: the pipeline's token still needs `admin`, and the list is asked only once that permission has admitted it.

The pipeline writes as above, with `Authorization: Bearer
$CREWLET_API_TOKEN_GITOPS` and an `If-Match` on the revision it last wrote, so a person's reload in between is a `409` it re-reads rather than overwrites. The checked revision is the fleet's on every node, so the next write lands on whichever node it reaches (one still applying the last write, or one that refused it, included) unless somebody else changed the document. Everyone else still reads (`GET /config`, the history, the diffs) and is refused a change: every credential the list does not name, `admin` holders included, whether a person signed in to the dashboard, a machine token acting as its owner, or a [lead editing their own team](#a-lead-editing-their-own-team), whose edit is exactly what the next reconcile would overwrite:

```bash
curl -X PATCH $CREWLET_URL/config -H "$AUTH" \
  -H "Content-Type: application/merge-patch+json" -H "X-Summary: a tweak" \
  -d '{"mission":"ship it"}'
# 403 {"error":"config_managed","managed_by":["gitops"],"detail":"…","hint":"…"}
```

The dashboard shows the document as managed by `gitops`: the org builder opens read-only and every control that writes the document is disabled with that sentence. What stays open is the break-glass for a leaked credential, which the managing system cannot know about: write the new value and re-publish the unchanged document (Settings › Configuration's **Reload** sends the same request):

```bash
printf %s "$NEW_VALUE" | curl -X PUT $CREWLET_URL/secrets/GITLAB_TOKEN -H "$AUTH" --data-binary @-
curl -X POST $CREWLET_URL/config/reload -H "$AUTH" -H "X-Summary: rotate the leaked GitLab token"
```

An MCP server's header or variable stored from its page ([`PUT /mcp-servers/{name}/values/{key}`](../reference/api-endpoints.md#put-mcp-serversnamevalueskey)) is refused too (`403 config_managed`), since it writes the key's `${VAR}` into the document: the managing system names the variable, and a value rotation goes through `/secrets` as above.

What is refused and what stays open, and why: [Managed configuration](../concepts/configuration.md#managed-configuration).

---

## Common error responses

Every `400` about the document carries `problems` (the failures located and classified) beside the `detail` that renders them.

| Status | Error | Meaning |
|--------|-------|---------|
| `400` | `invalid_body` | The body is not YAML or JSON, or its shape is not a company's (an unknown key, a list where a mapping belongs). `Content-Type` does not decide it |
| `400` | `invalid_patch` | A `PATCH` body that could not be merged, or that names a key the document does not have |
| `400` | `validation_error` | The whole resulting document failed validation; `detail` carries the message and `problems` locates each failure |
| `400` | `summary_required` | No `X-Summary` header and no top-level `_summary` key; asked after admission and document checks, so a write you may not make is refused for that first |
| `400` | `summary_too_long` | The summary is past the 4,096 bytes the activation pointer carries, as the pointer encodes it (`summary_bytes`, `max_summary_bytes`); refused whole, never cut |
| `400` | `origin_too_long` | The name the write was made under leaves the activation pointer past its 8 KiB (`pointer_bytes`, `max_pointer_bytes`); make it through a credential with a shorter name |
| `400` | `invalid_query` | `dry_run` given as anything but `true` or `false` |
| `400` | `conflicting_preconditions` | A write carrying both `If-None-Match` and `If-Match`, to `/config` or a per-entity `PUT`; send one |
| `400` | `identity_mismatch` | A per-entity `PUT` whose body names another id than its path; never renames. A server or provider is renamed in the whole document, with everything that names it |
| `400` | `not_creatable` | A create-only `PUT` (`If-None-Match: *`) of a seat or a unit: add it by replacing the unit it sits in, or through `PUT /config` |
| `401` | `invalid_token` | Bearer missing / wrong / wrong scheme |
| `403` | `unauthorized` | The credential holds neither `admin`, which the route takes, nor, on the org chart, the lead relation; `reason` names the rule. A lead's write reaching outside their team names every part under `refused` |
| `403` | `config_managed` | The document is [managed by another system](#when-another-system-manages-the-document) and this credential is not in `api.auth.company_writers`; `managed_by` names the writers. Applies to a write and its dry run, after the grant admits the caller and before the body is read |
| `403` | `step_up_required` | The proof of identity was older than `step_up` when the write arrived: confirm who you are and resend. A proof fresh on arrival is not judged again part-way |
| `404` | `no_active_revision` | Reading `/config` before the company exists: no fleet activation, no company known to this node. The `detail` says how to start one |
| `404` | `no_such_entity` | A per-entity `PUT` naming an id the active revision lacks; send `If-None-Match: *` to add an MCP server or an LLM provider. For a seat or unit only `admin` is told; a lead gets `403` as for one outside their team |
| `404` | `no_route` | A path under `/config` this surface does not serve |
| `405` | `method_not_allowed` | A `/config` path under a method it does not take; `Allow` names the ones it does |
| `409` | `no_active_revision` | A per-entity write before the company exists: there is nothing to splice into |
| `409` | `unsealed_revision`, `unsealed_fleet_revision` | The active revision is stored unsealed, so it can't be shown, edited or reverted to. An admin replaces it with the whole document via `PUT /config` (or `crewlet config import`); see [A whole document replaces an unsealed revision](../reference/api-endpoints.md#a-whole-document-replaces-an-unsealed-revision) |
| `409` | `unreadable_revision`, `unreadable_fleet_revision` | Sealed under a key this node's keyring lacks, so no write reads or replaces it. Put the key in `secrets.keys` and restart the node |
| `409` | `unknown_seal_revision` | Sealed in a form this build does not read (likely a newer build). Upgrade the node to the sealing build |
| `409` | `revision_advanced` | The fleet's activation moved between the write's read and its activation, for a write naming no revision in `If-Match` (or `If-Match: *`, another resource's tag, or none, as a reload and a revert do): a concurrent writer won, the activation was removed, or a write built while the fleet had none lost to one published meanwhile. A write that lost at activation keeps its revision, inert, as `stored_revision_id` on the node named `node`; its diff and revert go to that node (by its own address where several share one; any other answers `404`). Re-read `/config` and resend |
| `409` | `seat_held` | A write removing a human seat or making it an agent's while the directory binds somebody to it or an open invitation names it, or a whole document replacing an unsealed revision that drops such a seat; `held` names what holds each. Free the seat first, or, for a replacement, keep it as a human seat |
| `412` | `revision_advanced` | `If-Match: <revision>` names a revision that is not the fleet's, on arrival or because another write activated first; one that lost at activation keeps and names its revision as the `409` does. Re-read `/config` and resend |
| `412` | `no_active_revision` | `If-Match: <revision>` sent before the company exists; retry without `If-Match`, or send `If-None-Match: *` |
| `412` | `already_configured` | `If-None-Match: *` on `PUT /config` while the fleet runs a company (every node alike, one yet to apply it included) or, with no activation, while this node knows one. Also a create-only write that read no activation and lost to one published first: it keeps its revision, inert, as `stored_revision_id` in the history of `node` alone, as a `409` does |
| `412` | `entity_exists` | A per-entity create (`If-None-Match: *`) naming an MCP server or LLM provider the active revision already has |
| `415` | `unsupported_patch_media_type` | A `PATCH` in a patch format other than a JSON Merge Patch, such as `application/json-patch+json` |
| `503` | `draining` | The node was told to stop; nothing was written. `Retry-After` says when to retry, against a peer or this node once restarted; see [During a drain](../reference/api-endpoints.md#during-a-drain) |
| `503` | `seat_holders_unavailable` | A write removing a human seat, on a node that could not read the identity directory, or a whole document replacing an unsealed revision (which always asks and names no `seats`). Nothing was written; retry after `Retry-After`, and with none, waiting won't help |
| `503` | `unavailable` | With no `outcome` field: the node could not read the fleet's activation from the coordination store, so it cannot say which revision is the company. Affects any write and any read that answers or compares with the active revision (`GET /config`, the revision list and a single revision, each marked `is_active`, the reference index, a diff against `active`). Nothing was written or answered from the node's own store; resend after `Retry-After` (2 seconds; a store electing a leader is out for a few). A [`/setup`](../reference/api-endpoints.md#supplying-them) submission is the exception: its company document was unchanged, but it names the credentials it sealed first as `wrote_secrets`, and one whose own write went unanswered, which may have landed, as `unanswered_secret`. Resend all the same: that rewrites every credential in it by name. With no `outcome` and a `detail` instead, any other node condition (a store too busy for the write's revision, one closed while the node adopts a snapshot or shuts down, a request out of time): `detail` says so, and `Retry-After` comes exactly where waiting clears it |
| `503` | `unavailable`, `"outcome": "unknown"` | A write (`PUT`, `PATCH`, a per-entity or charter write, a reload, a revert, or a [`/setup`](../reference/api-endpoints.md#supplying-them) submission) stored its revision as `stored_revision_id`, built on `your_base`, on the node named `node`, and the coordination store did not answer its activation, so it may have landed. **Do not send it again**: once the store answers, `GET /config`; if the `ETag` names `stored_revision_id` the write landed, and otherwise `POST /config/revisions/{stored_revision_id}/revert` makes it the company, sent to that node (the revision is in its history alone; any other node answers `404`) |

The full reference is in [API endpoints](../reference/api-endpoints.md).
