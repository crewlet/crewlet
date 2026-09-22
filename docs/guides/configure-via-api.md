# Configure Nimbus over the API

End-to-end recipe for bootstrapping the [`examples/nimbus.company.yaml`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) company against a running engine — first the one-shot `PUT /config` (recommended), then the per-entity settings edits and the [org chart's own routes](#evolving-the-org-chart) you'd run afterwards to evolve the company live.

A running company is **two things**: a settings revision (`/config`) and an org chart (`/chart`). They have different lifetimes, different write paths and different authority, and this guide covers both.

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

> **This surface writes the company's settings, not its org chart.** A stored
> revision holds the providers, the integrations, the turn engine and the
> scheduling defaults. The **units and the seats** are a domain of their own,
> with their own records, their own per-object arbitration and their own
> history — see [The org chart domain](../concepts/chart-domain.md). A `PUT` or
> a `PATCH` here carrying a top-level `roles:` or `units:` is refused in full
> with `400 chart_not_writable_here`, and so is a write to
> `/config/roles/{handle}` or `/config/units/{key}`. Both stay **readable**.
>
> The chart has its own routes: `GET`/`PATCH /chart/units/{key}` and
> `/chart/seats/{handle}` for content, `POST /chart/batch` for structure, and
> `POST /chart/import` for a whole revision's authored placement. See
> [Evolving the org chart](#evolving-the-org-chart) below and the
> [`/chart/*` reference](../reference/api-endpoints.md#chart--the-org-chart-auth-gated).
> A node's first chart is also seeded from the company file at boot — `crewlet
> run -company company.yaml`, which seeds only while the chart is empty.
>
> It is refused rather than ignored on purpose. A write that quietly kept half
> of what you sent would answer `201`, activate, and leave the new seat
> nowhere — with your own document saying it exists.
>
> The **authoring file keeps both halves.** You write one `company.yaml`
> describing a company, `crewlet validate` reads it whole, and `crewlet config
> import` is what divides it: the settings to a revision, the chart to its log.

---

## Option 1 — Single full-document PUT (recommended for bootstrap)

The simplest path. Send the whole `company.yaml` in one request; the engine validates, persists as a new revision, appends an activation epoch, and spawns the whole company. Every node in the deployment converges on that epoch — see [Control Plane](../concepts/control-plane.md).

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

The document you send here is the **settings half** — everything in
`company.yaml` except `roles:` and `units:`. To load a whole authored file, use
`crewlet config import`, which divides it and publishes both halves.

Response is `201 Created` with the new `revision_id`, `epoch` and the
`warnings` the engine has about the document. See
[What a write answers](../reference/api-endpoints.md#what-a-write-answers).

There is **no `derived` hierarchy in the answer** any more, and that is
deliberate rather than an omission: the hierarchy came from `roles:` and
`units:`, so a settings write answering one would answer an *empty* org chart
for every company — which reads as "your chart is gone" rather than as "that
field moved". Read the chart from the chart.

To check a document without writing it, send the same request with
`?dry_run=true`. Nothing is stored or activated, no summary is needed, and the
answer is `200 {"valid": true, "base_revision_id", "warnings"}`, or the
refusal the write would get — including the chart refusal, so a check tells
you what the save will do:

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

Verify:

```bash
curl -s $CREWLET_URL/health $AUTH                                 # configured: true
curl -s $CREWLET_URL/config -H "$AUTH" | jq '.name'               # "Nimbus"
curl -s $CREWLET_URL/config/revisions -H "$AUTH" | jq '.[0]'      # newest first
curl -s $CREWLET_URL/agents | jq 'length'                         # 7 agent seats spawned
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

Two collections are **writable** here: **llm-providers** and **mcp-servers**.
Use these to change one thing about an already-active company; use Option 1 to
bootstrap it, and for anything they do not cover (the identity block,
integrations, the turn engine, the knowledge scope).

```
PUT /config/llm-providers/{key}
PUT /config/mcp-servers/{name}
```

`roles` and `units` are still **readable** at `/config/roles/{handle}` and
`/config/units/{key}` — a revision written before the chart's split still
carries both inside it, and you have to be able to see one you are repairing.
There is no write route for either: the chart's own surface is
[below](#evolving-the-org-chart).

Each is addressed by the thing the *document* resolves it by, never by its
display name: a provider by its key under `providers.llm`, a server by its
`name`. `GET` on the collection lists exactly those, which is the list to
address from.

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
curl -s "$CREWLET_URL/query/config_entities?kind=mcp-servers" -H "$AUTH" | jq

# One entity. The response IS the entity, so it goes straight back.
curl -s -D headers.txt "$CREWLET_URL/config/mcp-servers/tracker" -H "$AUTH" > tracker.json

# Edit tracker.json, then send it back — quoting the ETag the read returned,
# so a concurrent activation is refused rather than silently overwritten.
curl -X PUT $CREWLET_URL/config/mcp-servers/tracker \
  -H "$AUTH" -H "Content-Type: application/json" \
  -H "If-Match: $(awk -F'"' '/^[Ee][Tt]ag:/ {print $2}' headers.txt)" \
  -H "X-Summary: point the tracker server at the new endpoint" \
  -d @tracker.json
```

The `config_entities` query still lists a collection and still answers a
`{kind, id, entity}` envelope — it is what the dashboard reads. For one entity
prefer `GET /config/{kind}/{id}`, whose body is exactly what `PUT` takes.

The response is `201 Created` with the new `revision_id`, `epoch` and
`warnings`, exactly as a full PUT would be: the write changed one entity and
created one revision.

### What a write actually does

It is not a patch protocol. The engine opens the active revision, splices your
entity in, restores the credential masks the read showed you against that same
revision, **validates the whole document**, and stores the result. Three
consequences worth knowing before you script against it:

- **The whole company is validated, not just your entity.** A delegate
  template naming an `llm` provider that no longer exists is fine on its own
  and breaks the company; you get `400 validation_error` naming the field,
  even though nothing in the body you sent is about it. This is the point of
  validating whole — you never see the rest of the document, so it is the one
  place that break can be caught.
- **An unknown field is refused, not dropped.** A body carrying `gaol` where
  you meant `goal` is `400 invalid_body` naming the field, exactly as the
  whole-document parser refuses an unknown key. A decoder that ignored what it
  did not recognise would answer `201` and store a seat with no goal, and this
  is the surface most likely to be hand-edited in a hurry.
- **A `PUT` never creates.** An id the active revision does not carry is
  `404 no_such_entity`. Naming one that is not there is far more often a typo
  than an intent to add a seat, and adding through this route would grow the
  company without you ever seeing the document you changed. Add through
  `PUT /config`.
- **The path is the identity, and a `PUT` never renames.** `PUT
  /config/mcp-servers/tracker` replaces whatever is at `tracker`; a body
  carrying a different `name` is `400 identity_mismatch` rather than a move.
  A server's name is the key every seat declares its credentials under and the
  prefix its tools carry, so a rename here silently unhooks everything that
  named it, and nothing that references the old name travels with the splice.
  Keep the identity in the body and change whatever else you like.
- **`PUT` is the only verb.** There is no `DELETE /config/mcp-servers/tracker`;
  the path answers `405`. Removal is a full-document edit for the same reason
  creation is, only more so — deleting a provider silently repoints everything
  that named it. If that is going to happen, it should happen in a document you
  looked at, and land as one reviewable revision. Export, edit, `PUT /config`.

A write keeps what the node's own build cannot represent. During a rolling
upgrade a node may hold a document a newer node wrote, with settings its
`GET` cannot show you; whatever you send back through it, those settings
survive on every member of a list matched by its identity. See
[Fields a newer build wrote survive every write](../reference/api-endpoints.md#fields-a-newer-build-wrote-survive-every-write).

### `X-Summary` and `If-Match`

Both work exactly as they do on the full PUT, and `X-Summary` is **required**:
the revision history is what someone reads at 3am to find the change that
broke something, and a per-entity write is the one most likely to be made in a
hurry. A node with no active revision answers `409 no_active_revision` — there
is nothing to splice into.

---

## Evolving the org chart

The chart is a domain of its own, so it has its own verbs. What decides them is
**which half of an object you are writing**, and the payload picks the
question: the public half is whoever leads that object, and anything under
`runtime` — a seat's model chain, its credentials, its sandbox cell, its
`mcp_env` — is the company's own `config:write` grant, because a stdio MCP
server is `exec.Command` with the config's command.

### Edit one seat's goal

Read it, edit it, send it back. Reads are **stripped by default**: ask for the
runtime half with `?runtime=true`, and you get it only if you also hold
`config:read`.

```bash
curl -s "$CREWLET_URL/chart/seats/sre" -H "$AUTH" | jq .seat

curl -X PATCH $CREWLET_URL/chart/seats/sre \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{"kind":"agent","unit":"engineering","name":"SRE",
       "goal":"keep the platform boring"}'
```

A content write is **full post-state**, like the record it becomes: a field you
leave out is a field you set to empty. That is also why omitting `runtime` is
itself a privileged write — it clears the half you did not send.

### Hire, move, dissolve

Structure goes through one batch, and **one batch is one record**. A caller
that means to move three seats sends three operations in one request:

```bash
curl -X POST $CREWLET_URL/chart/batch \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d '{"operations":[
        {"kind":"create_unit","object":{"kind":"unit","id":"platform"},"lead":"sre"},
        {"kind":"move","object":{"kind":"seat","id":"sre"},"parent":"platform"}
      ]}'
```

Removals go in a batch of their own and carry a `reason`, which rides into the
tombstone so somebody asking where their team went reads "merged into
infrastructure" rather than an absence.

### Rename

```bash
curl -X POST $CREWLET_URL/chart/units/engineering/rename \
  -H "$AUTH" -d '{"to":"platform"}'
```

The former address goes on resolving: a key is an ADDRESS and the row is the
identity, so a `manages:` entry somebody wrote last year still finds what it
named. Reads carry `former_keys` / `former_handles` so a client can say why a
stale reference still works.

### What a `200` means, and what a `202` does not

`200` means the record is durable **and this node has applied it**, so your next
read here sees it. `202` means durable but not yet applied here — read at the
position in the body. `503` with an `op_id` means this node cannot say what
happened: retry with `Idempotency-Key: <that op_id>`, never a fresh one, or a
change that did land is written twice.

### Check the two halves agree

Nothing refuses a settings edit that strands a seat, because the two halves are
written by different people at different times. `GET /chart/check` is the
report over the pair:

```bash
curl -s "$CREWLET_URL/chart/check" -H "$AUTH" | jq '.report.findings'
```

The same evaluation is summarised on `/health` under `consistency`. Check
`evaluated` before the count — `findings: 0` from a node holding no chart is
not a clean bill.

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
| `401` | `invalid_token` | Bearer missing / wrong / wrong scheme |
| `404` | `no_active_revision` | Reading `/config` before the first PUT |
| `404` | `no_such_entity` | A per-entity `PUT` naming an id the active revision does not carry — this route never creates |
| `409` | `no_active_revision` | A per-entity write before the first PUT: there is nothing to splice into |
| `409` | `revision_advanced` | Stale `If-Match`, a concurrent writer won the race, or the write was built on an empty store while the fleet is running a company |
| `412` | `no_active_revision` | `If-Match: <revision>` sent while the node has no active revision; retry without `If-Match`, or send `If-None-Match: *` |
| `412` | `already_configured` | `If-None-Match: *` sent while a revision is active on this node or anywhere in the fleet |
| `415` | `unsupported_patch_media_type` | A `PATCH` in a patch format other than a JSON Merge Patch, such as `application/json-patch+json` |
| `503` | `draining` | The node has been told to stop. Nothing was written; `Retry-After` says when to try again, against a peer or against this node once it has restarted — see [During a drain](../reference/api-endpoints.md#during-a-drain) |

The full reference is in [API endpoints](../reference/api-endpoints.md).
