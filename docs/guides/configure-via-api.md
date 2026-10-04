# Configure Nimbus via the `/config/*` API

End-to-end recipe for bootstrapping the [`examples/nimbus.company.yaml`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) company against a running engine — first the one-shot import of the whole file (recommended), then the per-entity edits you'd run afterwards to evolve the company live, its org chart included.

Every request below assumes:

```bash
export CREWLET_URL="http://localhost"   # examples/nimbus.config.yaml binds the embedded API on port 80
export TOKEN="$CREWLET_API_TOKEN_FOUNDER"   # matches api.auth.tokens[].token in crewlet.yaml
export AUTH="Authorization: Bearer $TOKEN"
```

`/health` should report `{"status":"unconfigured","configured":false}` before you start (still HTTP **200** — the status code is liveness, and an engine waiting for a configuration is alive). After the first PUT it flips to `{"status":"ok","configured":true}` and stays that way for the engine's lifetime.

```bash
curl -s $CREWLET_URL/health
```

See the [Configuration concept doc](../concepts/configuration.md) for the two-tier split and the rationale behind live config management, and the [API endpoints reference](../reference/api-endpoints.md) for status codes.

---

## Option 1 — Import the whole file (recommended for bootstrap)

The simplest path. `crewlet config import` validates the whole `company.yaml`
and sends it to the running node's `PUT /config`, which persists it as a new
revision and activates it; every node converges on that epoch and spawns the
company (see [Control Plane](../concepts/control-plane.md)). It authenticates
the way every command that talks to a node does, with `CREWLET_API_TOKEN`:

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

The command is a `PUT /config` of the whole file, and a script can send it
itself:

```bash
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: bootstrap Nimbus" \
  --data-binary @examples/nimbus.company.yaml
```

A revision summary is required on every write. It travels in the `X-Summary`
header, or as a top-level `_summary` key in the body:

```bash
curl -X PUT http://localhost:8000/config \
  -H "Authorization: Bearer $CREWLET_API_TOKEN" \
  -H "Content-Type: application/yaml" \
  --data-binary $'_summary: bootstrap Nimbus\n'"$(cat nimbus.company.yaml)"
```

The body key exists because the body is often the only thing a caller
controls — a form post, a proxy that strips unknown headers, a CI step piping
a document through a tool that takes no header arguments. It is **removed
before the document is parsed**, so it never trips the unknown-field check
that Tier B applies deliberately. When both are present the **header wins**:
it is the more explicit channel, and a `_summary` can survive in a document
somebody keeps in version control long after it stopped describing the write.

Response is `201 Created` with the new `revision_id` and `epoch`, the
`warnings` the engine has about the document (a lead or a `manages` entry that
names nobody, for example) and the `derived` hierarchy it will run. See
[What a write answers](../reference/api-endpoints.md#what-a-write-answers).

**Every seat's handle and every unit's key is written into the stored
document.** A seat that declares no `handle` gets the one its `name` derives,
and a unit with no `id` gets one minted from its name, before the revision is
stored — so a correction to a name made by editing the stored document (`GET`
then `PUT`, `PATCH`, the per-entity routes below) keeps the identity, and the
seat its agent id, mailbox and memory. A file you import is minted afresh each
time: correct a name in one that leaves the `handle` or `id` out and the import
replaces that seat or unit. Declare both in any file you keep, or start it
from `crewlet config export`, which writes every one of them.

To check a document without writing it, send the same request with
`?dry_run=true`. Nothing is stored or activated, no summary is needed, and the
answer is `200 {"valid": true, "base_revision_id", "warnings", "derived"}`, or
the refusal the write would get:

```bash
curl -X PUT "$CREWLET_URL/config?dry_run=true" \
  -H "$AUTH" \
  --data-binary @examples/nimbus.company.yaml
```

A refusal names each failure in `detail` and again in `problems`, one located,
classified entry per failure with its `path`, `segments` and `kind` (and the
`line` when the parser found it), so a script can point at the field rather
than parse the message. See
[Refusals carry located problems](../reference/api-endpoints.md#refusals-carry-located-problems).

The body is read as YAML, which JSON is a subset of, whatever `Content-Type`
says.

JSON body works too:

```bash
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" \
  -H "Content-Type: application/json" \
  -H "X-Summary: bootstrap Nimbus" \
  --data-binary @nimbus.company.json
```

If anything else has touched `/config` since you last read it, supply `If-Match`:

```bash
REV=$(curl -s $CREWLET_URL/config/revisions -H "$AUTH" | jq -r '.[0].revision_id')
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" -H "If-Match: $REV" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: bootstrap Nimbus" \
  --data-binary @examples/nimbus.company.yaml
# 409 revision_advanced if the active revision moved past $REV between read + write
```

---

## Option 2 — Evolve a live company one entity at a time

Four collections are addressable on their own: **roles**, **units**,
**llm-providers** and **mcp-servers**. Use these to change one thing about an
already-active company; use Option 1 to bootstrap it, and for anything the
four do not cover (the identity block, integrations, the turn engine, the
knowledge scope).

```
PUT /config/roles/{handle}
PUT /config/units/{id}
PUT /config/llm-providers/{key}
PUT /config/mcp-servers/{name}
```

Each is addressed by the thing the *document* resolves it by, never by its
display name: a seat by its handle, a unit by its key (`id`), a provider by its
key under `providers.llm`, a server by its `name`. `GET` on the collection
lists exactly those, which is the list to address from.

Why bother, when `PUT /config` already works? Because that write makes every
edit a company-wide one. Changing one seat's goal means sending back a
document carrying every other seat, every provider and every integration — and
a concurrent edit anywhere in it is yours to lose. A per-entity write narrows
what you are claiming to have changed, which is what makes the revision
summary in the history mean something.

### The loop

Read the entity, edit it, send it back. The read is the same redacted
document `GET /config` serves, sliced:

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

The `config_entities` query still lists a collection and still answers a
`{kind, id, entity}` envelope — it is what the dashboard reads. For one entity
prefer `GET /config/{kind}/{id}`, whose body is exactly what `PUT` takes.

The response is `201 Created` with the new `revision_id`, `epoch`, `warnings`
and `derived` hierarchy, exactly as a full PUT would be: the write changed one entity and
created one revision.

### What a write actually does

It is not a patch protocol. The engine opens the active revision, splices your
entity in, restores the credential masks the read showed you against that same
revision, **validates the whole document**, and stores the result. Three
consequences worth knowing before you script against it:

- **The whole company is validated, not just your entity.** A seat naming an
  `llm` provider that no longer exists is fine on its own and breaks the
  company; you get `400 validation_error` naming the field, even though
  nothing in the body you sent is about it. This is the point of
  validating whole — you never see the rest of the document, so it is the one
  place that break can be caught.
- **An unknown field is refused, not dropped.** A body carrying `heders`
  where you meant `headers` is `400 invalid_body` naming the field, exactly as
  the whole-document parser refuses an unknown key. A decoder that ignored what
  it did not recognise would answer `201` and store a server that sends no
  credentials, and this is the surface most likely to be hand-edited in a
  hurry.
- **A plain `PUT` never creates.** An id the active revision does not carry
  is `404 no_such_entity`: naming one that is not there is far more often a
  typo than an intent to add one. To ADD an MCP server or an LLM provider,
  say so — send the same `PUT` with `If-None-Match: *`, the create-only
  condition at the new entity's own address. It is added (an MCP server after
  every server already declared, so no seat's tool block moves) and the whole
  company validated as for any write; a name already taken is
  `412 entity_exists` rather than a replacement of a server you never saw. Do
  not send `If-Match` beside it (`400 conflicting_preconditions`): the create
  lands on the revision active when it commits, compare-and-set. A seat and a
  unit are not created by address (`400 not_creatable`) — each has a place in
  the chart the path does not name — so add one by replacing the unit it sits
  in, or through `PUT /config`; see
  [Adding, moving and removing seats and units](#adding-moving-and-removing-seats-and-units).

  ```bash
  curl -X PUT http://localhost:8000/config/mcp-servers/linear \
    -H "Authorization: Bearer $CREWLET_API_TOKEN" \
    -H "If-None-Match: *" \
    -H "X-Summary: add the linear server" \
    -d '{"name":"linear","transport":"http","url":"https://mcp.example.com",
         "headers":{"Authorization":"${LINEAR_TOKEN}"}}'
  ```
- **The path is the identity, and a `PUT` never renames.** `PUT
  /config/roles/ceo` replaces whatever is at `ceo`; a body carrying a
  different handle is `400 identity_mismatch` rather than a move. A seat's
  handle and a unit's key are permanent — the seat's durable id derives from
  its handle, so a different handle is a different seat with an empty mailbox
  and no memory — and a server's name is the key every seat declares its
  credentials under, so nothing that references the old name travels with the
  splice. A body that leaves `handle` (or a unit's `id`) out keeps the one the
  path names, so editing a display name is never a rename. Keep the identity
  and change whatever else you like.
- **`PUT` is the only verb.** There is no `DELETE /config/mcp-servers/tracker`;
  the path answers `405`. Removal is a whole-document edit, unlike a create:
  adding a server or a provider changes nothing that already names one, while
  deleting a provider silently repoints every seat whose model chain named it,
  and deleting a server leaves every `mcp_env` block keyed on it read by
  nothing. If that is going to happen, it should happen in a document you
  looked at, and land as one reviewable revision: export, edit, `PUT /config`.

A write keeps what the node's own build cannot represent. During a rolling
upgrade a node may hold a document a newer node wrote, with settings its
`GET` cannot show you; whatever you send back through it, those settings
survive on every seat, unit and MCP server matched by its identity. See
[Fields a newer build wrote survive every write](../reference/api-endpoints.md#fields-a-newer-build-wrote-survive-every-write).

### `X-Summary` and `If-Match`

Both work exactly as they do on the full PUT, and `X-Summary` is **required**:
the revision history is what someone reads at 3am to find the change that
broke something, and a per-entity write is the one most likely to be made in a
hurry. A node with no active revision answers `409 no_active_revision` — there
is nothing to splice into.

---

### Adding, moving and removing seats and units

A seat or a unit has a **place** — the unit it sits in — and the entity path
names none, so neither is created or moved by address. Each is a change to the
unit that holds it: read that unit with `GET /config/units/{id}`, add, move or
drop the seat in its `roles`, or the unit in its `children`, and `PUT` it back
— or make the change in the whole document with `PUT` or `PATCH /config`. A
seat at the company root is added through the whole document. The answer's
`derived` hierarchy shows where everything landed.

**A human seat somebody is bound to cannot be taken away.** A write that
removes one — or turns it into an agent seat — while the
[identity directory](../concepts/identity-and-access.md) binds a person to it,
at any stage short of their removal, is refused `409 seat_held` naming them under `held`;
unbind or remove them first. A node that cannot read the directory refuses
such a write `503 identity_unavailable` rather than allowing it. An offline `crewlet config import`
has no directory to ask, so the binding it strands is reported by
`crewlet iam check` and the `iam_binding_dangling` alarm.

## Read paths

```bash
# Active revision (JSON or YAML)
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

---

## Common error responses

Every `400` about the document carries `problems`, the failures located and
classified, beside the `detail` that renders them.

| Status | Error | Meaning |
|--------|-------|---------|
| `400` | `invalid_body` | The body is not YAML or JSON, or its shape is not a company's (an unknown key, a list where a mapping belongs). `Content-Type` is not what decides it |
| `400` | `invalid_patch` | A `PATCH` body that could not be merged, or that names a key the document does not have |
| `400` | `validation_error` | The whole resulting document failed validation; `detail` carries the message and `problems` locates each failure |
| `400` | `summary_required` | Any write with neither an `X-Summary` header nor a top-level `_summary` key in the body |
| `400` | `invalid_query` | `dry_run` given as anything but `true` or `false` |
| `400` | `conflicting_preconditions` | A per-entity `PUT` carrying both `If-None-Match: *` and `If-Match`; send one |
| `400` | `identity_mismatch` | A per-entity `PUT` whose body names another id than its path: this route never renames — a seat's handle and a unit's key are permanent, and a server or provider is renamed in the whole document, with everything that names it |
| `400` | `not_creatable` | A create-only `PUT` (`If-None-Match: *`) of a seat or a unit: each has a place the path cannot name, so add it by replacing the unit it sits in, or through `PUT /config` |
| `401` | `invalid_token` | Bearer missing / wrong / wrong scheme |
| `404` | `no_active_revision` | Reading `/config` before the first PUT |
| `404` | `no_such_entity` | A per-entity `PUT` naming an id the active revision does not carry — a plain `PUT` never creates; send `If-None-Match: *` to add an MCP server or an LLM provider |
| `404` | `no_route` | A path under `/config` this surface does not serve |
| `405` | `method_not_allowed` | A `/config` path under a method it does not take; `Allow` names the ones it does |
| `409` | `no_active_revision` | A per-entity write before the first PUT: there is nothing to splice into |
| `409` | `revision_advanced` | Stale `If-Match`, a concurrent writer won the race, or the write was built on an empty store while the fleet is running a company |
| `409` | `seat_held` | A write that removes a human seat, or makes it an agent's, while the identity directory binds somebody to it; the answer names them. Unbind or remove them first |
| `412` | `no_active_revision` | `If-Match: <revision>` sent while the node has no active revision; retry without `If-Match`, or send `If-None-Match: *` |
| `412` | `already_configured` | `If-None-Match: *` on `PUT /config` sent while a revision is active on this node or anywhere in the fleet |
| `412` | `entity_exists` | A per-entity create (`If-None-Match: *`) naming an MCP server or LLM provider the active revision already has |
| `415` | `unsupported_patch_media_type` | A `PATCH` in a patch format other than a JSON Merge Patch, such as `application/json-patch+json` |
| `503` | `draining` | The node has been told to stop. Nothing was written; `Retry-After` says when to try again, against a peer or against this node once it has restarted — see [During a drain](../reference/api-endpoints.md#during-a-drain) |
| `503` | `identity_unavailable` | A write that removes a human seat, on a node that could not read the identity directory to see whether anybody holds it. Nothing was written; retry after `Retry-After` |

The full reference is in [API endpoints](../reference/api-endpoints.md).
