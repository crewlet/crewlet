# Configure Nimbus via the `/config/*` API

End-to-end recipe for bootstrapping the [`examples/nimbus.company.yaml`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) company against a running engine — first the one-shot `PUT /config` (recommended), then per-entity edits you'd run afterwards to evolve the company live.

Every request below assumes:

```bash
export CREWLET_URL="http://localhost"   # the example Tier A file binds the embedded API on port 80
export TOKEN="$CREWLET_API_TOKEN_FOUNDER"   # matches api.auth.tokens[].token in config.yaml
export AUTH="Authorization: Bearer $TOKEN"
```

`/health` should report `{"status":"unconfigured","configured":false}` before you start (still HTTP **200** — the status code is liveness, and an engine waiting for a configuration is alive). After the first PUT it flips to `{"status":"ok","configured":true}` and stays that way for the engine's lifetime.

```bash
curl -s $CREWLET_URL/health
```

See the [Configuration concept doc](../concepts/configuration.md) for the two-tier split and the rationale behind live config management, and the [API endpoints reference](../reference/api-endpoints.md) for status codes.

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
curl -X PUT http://localhost:8080/config \
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

Response is `201 Created` with `{"revision_id": "..."}`.

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

Four collections are addressable on their own: **roles**, **units**,
**llm-providers** and **mcp-servers**. Use these to change one thing about an
already-active company; use Option 1 to bootstrap it, and for anything the
four do not cover (the identity block, integrations, the turn engine, the
knowledge scope).

```
PUT /config/roles/{handle}
PUT /config/units/{name}
PUT /config/llm-providers/{key}
PUT /config/mcp-servers/{name}
```

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

The response is `201 Created` with the new `revision_id` and `epoch`, exactly
as a full PUT would be: the write changed one entity and created one revision.

### What a write actually does

It is not a patch protocol. The engine opens the active revision, splices your
entity in, restores the credential masks the read showed you against that same
revision, **validates the whole document**, and stores the result. Three
consequences worth knowing before you script against it:

- **The whole company is validated, not just your entity.** A seat naming an
  `llm` provider that no longer exists is fine on its own and breaks the
  company; you get `400 validation_error` naming the field. This is the point
  of validating whole — you never see the rest of the document, so it is the
  one place that break can be caught.
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
  /config/roles/ceo` replaces whatever is at `ceo`; a body carrying a
  different handle is `400 identity_mismatch` rather than a move. The handle
  is effectively permanent — the seat's durable id derives from it, so a
  rename orphans that seat's diary, its onboarding marker and its counterparty
  profiles — and nothing that references the old name travels with the splice.
  The check is on the **derived** handle, so `{"name": "Chief Executive"}`
  with no `handle` is refused too: an omitted handle is derived from the name,
  which makes a display-name edit a rename by accident. Keep `handle` in the
  body and change whatever else you like. Renaming is a full-document edit.
- **`PUT` is the only verb.** There is no `DELETE /config/roles/ceo`; the path
  answers `405`. Removal is a full-document edit for the same reason creation
  is, only more so — deleting a seat also strands its mailbox and its in-flight
  work, and deleting a provider silently repoints every role that named it. If
  that is going to happen, it should happen in a document you looked at, and
  land as one reviewable revision. Export, edit, `PUT /config`.

A seat inside a unit is reachable by handle like any other — you do not have
to know which list it lives in, or how deeply the unit is nested.

### `X-Summary` and `If-Match`

Both work exactly as they do on the full PUT, and `X-Summary` is **required**:
the revision history is what someone reads at 3am to find the change that
broke something, and a per-entity write is the one most likely to be made in a
hurry. A node with no active revision answers `409 no_active_revision` — there
is nothing to splice into.

---

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

| Status | Error | Meaning |
|--------|-------|---------|
| `400` | `invalid_body` | Body isn't JSON / YAML, or wrong `Content-Type` |
| `400` | `validation_error` | The merged config failed validation — `detail` carries the message |
| `400` | `summary_required` | Any write with neither an `X-Summary` header nor a top-level `_summary` key in the body |
| `401` | `invalid_token` | Bearer missing / wrong / wrong scheme |
| `404` | `no_active_revision` | Reading `/config` before the first PUT |
| `404` | `no_such_entity` | A per-entity `PUT` naming an id the active revision does not carry — this route never creates |
| `409` | `no_active_revision` | A per-entity write before the first PUT: there is nothing to splice into |
| `409` | `revision_advanced` | Stale `If-Match` or concurrent writer won the race |
| `412` | `if_match_must_be_none_when_unconfigured` | `If-Match: <uuid>` sent while engine is unconfigured |

The full reference is in [API endpoints](../reference/api-endpoints.md).
