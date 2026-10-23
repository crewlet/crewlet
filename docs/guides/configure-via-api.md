# Configure Nimbus via the `/config/*` API

End-to-end recipe for bootstrapping the [`examples/nimbus.company.yaml`](https://github.com/crewlet/crewlet/blob/main/examples/nimbus.company.yaml) company against a running engine — first the one-shot import of the whole file (recommended), then the per-entity edits you'd run afterwards to evolve the company live, its org chart included.

Every request below assumes:

```bash
export CREWLET_URL="http://localhost"   # examples/nimbus.config.yaml binds the embedded API on port 80
export TOKEN="$CREWLET_API_TOKEN_FOUNDER"   # matches api.auth.tokens[].token in crewlet.yaml
export AUTH="Authorization: Bearer $TOKEN"
```

`/health` should report `{"status":"unconfigured","configured":false}` before you start (still HTTP **200** — the status code is liveness, and an engine waiting for a configuration is alive). A node still booting says `"status":"starting"` instead — with a `503` while it recovers what it holds, then a `200` — and every other route answers `503 starting` until it serves: wait until `/health`'s status is `unconfigured`, as [While a node is starting](../reference/api-endpoints.md#while-a-node-is-starting) describes. (`/ready` is no signal here: it answers `503` throughout the boot, and goes on answering `503 unconfigured` until the first PUT.) After the first PUT it flips to `{"status":"ok","configured":true}` and stays that way for the engine's lifetime.

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
answer is `200 {"valid": true, "base_revision_id", "warnings", "derived"}`, each
warning marked `"introduced"` when this write brings it rather than the active
revision already carrying it, or the refusal the write would get:

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

**The active revision is the fleet's.** `GET /config` and its `ETag`, the
revision list's `is_active`, and the base every write is built on are the
revision the fleet's activation pointer names — the same on every node,
whatever that node runs. A node still applying the last write, or one that
refused it and goes on running the revision before, answers the last write all
the same, so an `ETag` read from one node is a good `If-Match` on any other.
What each node runs is a separate fact, which
[`GET /fleet`](../reference/api-endpoints.md#get-fleet) reports per node as
`config_serving_revision_id`. While the coordination store cannot be read,
these requests answer `503 unavailable` with a `Retry-After` rather than
anything from the node's own store; send them again — **unless the body
carries `"outcome": "unknown"`**. That is a write that stored its revision and
got no answer to its activation, which may have landed: the body names the
revision as `stored_revision_id`, the one it was built on as `your_base` and the
node that stored it as `node`. Sent again, it makes a second revision over the
first if that one landed. Instead, once the store answers, read `GET /config`:
if its `ETag` names `stored_revision_id`, the write landed; if not,
`POST /config/revisions/{stored_revision_id}/revert` makes it the company —
sent to that node, by its own address where several share one, because the
revision is in that node's history alone and any other answers `404`. See
[Status codes](../reference/api-endpoints.md#status-codes) and [Control
Plane](../concepts/control-plane.md#every-config-answer-is-the-pointers).

To have the write refused if anything else has touched `/config` since you
read it, supply that read's `ETag` as `If-Match`:

```bash
REV=$(curl -s -o /dev/null -D - $CREWLET_URL/config -H "$AUTH" | awk -F'"' '/^[Ee][Tt]ag:/ {print $2}')
curl -X PUT $CREWLET_URL/config \
  -H "$AUTH" -H "If-Match: $REV" \
  -H "Content-Type: application/yaml" \
  -H "X-Summary: bootstrap Nimbus" \
  --data-binary @examples/nimbus.company.yaml
# 412 revision_advanced if the fleet's revision moved past $REV between read + write
```

Take the tag from `GET /config`, not from the top of `/config/revisions`: that
list is this node's history, newest first, and its newest row need not be the
fleet's — a write that lost a race is kept there, inert. Without `If-Match` a
write is still built on the fleet's revision as the node reads it, and still
refused `409 revision_advanced` if another write lands between that read and
its activation. With `If-Match` the same race is `412`, the precondition you
stated having failed, and `If-Match` also refuses an edit of a revision that
had moved before you sent it, before anything is built.

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
hurry. Before the company exists — the fleet has no activation and the node
knows no company — a per-entity write answers `409 no_active_revision`: there
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

**A human seat something holds cannot be taken away.** A write that removes
one — or turns it into an agent seat — is refused `409 seat_held` while the
[identity directory](../concepts/identity-and-access.md) binds somebody to it
at any stage short of their removal, or while an open invitation names it,
since an invitation holds its seat until it is redeemed, cancelled or lapses.
The answer names the one thing holding each seat under `held` — a holder by
`person`, `kind`, `login` and `stage`, or an invitation by its `invitation` id
and `expires_at`, never the invitee's address — and the remedy follows the
holder, each a gesture for an administrator:

| What holds the seat | Free it with |
|---|---|
| A person | Move them to another human seat (`PATCH /iam/people/{person}` with `seat`, or `crewlet iam bind <person> <seat>`) or remove them. A person always holds a seat, so unbinding one is refused |
| A service account | Unbind it (`PATCH /iam/people/{person}` with `seat: ""`, or `crewlet iam unbind <person>`) or move it |
| An open invitation | Cancel it (`DELETE /iam/invitations/{invitation}`, or `crewlet iam cancel-invite <invitation>`) |

Then send the write again. A node that cannot read the directory refuses such
a write `503 seat_holders_unavailable` rather than allowing it. An offline
`crewlet config import` has no directory to ask, so the binding it strands is
reported by `crewlet iam check`, and an invitation to a seat it removed is
refused when somebody tries to redeem it.

### A lead editing their own team

A person who leads a unit needs no `admin` to change what is inside it.
Signed in as themselves, they read and write the seats and units of their team
— the unit they lead and every unit beneath it — through the same entity
routes, with the same `If-Match` and `X-Summary`:

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

Everything the write changes has to be inside their team, where it was and
where it lands: a seat added, edited, moved between two of their teams or
removed, a sub-team added or removed, the seats and sub-teams inside their
unit reordered, their own unit's name or purpose. Handing their unit to
another `lead:`, removing, moving or reordering it among its siblings, naming somebody
outside it in a `lead:` or `manages:` entry, setting or changing a project,
space, channel, address or contact id, changing a credential, naming a
`${VAR}` in any field, and any setting take `admin`. A write that reaches past them
is refused whole —
`403 unauthorized` with a `refused` list naming every part, the place it
reaches, why, and the `reason` and `grants` of its own, beside the write's
`reason` and the permission that would admit all of it; see [A lead edits their own
team](../reference/api-endpoints.md#a-lead-edits-their-own-team). A seat or a
unit outside their team, and a name the company does not have, is refused
`403` at its own address before the body is read, in the same words either
way. The whole document (`GET /config`) stays `admin`'s, so a lead edits
through the entity routes rather than a `GET` and `PUT` of `/config`. While
the document is [managed by another system](#when-another-system-manages-the-document),
a lead's edit is refused `403 config_managed` like everybody else's.

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

---

## When another system manages the document

Everything above is a person driving the API. When something else is the
source of the company — a GitOps pipeline applying `company.yaml` from a
repository, or external tooling such as a Kubernetes operator rendering it from
custom resources — that system runs exactly these requests with its own token,
and it replaces whatever is active each time it reconciles. A person's edit
made in between lasts until then, and nothing says it was lost.

Give the managing system its own token and name it as the document's only
writer in every node's Tier A:

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

The list names a credential rather than granting one: the pipeline's token
still needs `admin`, as every write of the document does, and the list is
asked only once that permission has admitted it.

The pipeline keeps writing as above, with `Authorization: Bearer
$CREWLET_API_TOKEN_GITOPS` and an `If-Match` on the revision it last wrote, so a
person's reload in between is a `409` it re-reads rather than one it
overwrites. The revision a write is checked against is the fleet's on every
node, so the pipeline's next write lands on whichever node it reaches — one
still applying the last write, or one that refused it, included — unless
somebody else changed the document. Everyone else still reads — `GET
/config`, the history, the diffs — and is refused a change. That is every
credential the list does not name, the ones holding `admin` included: a
person signed in to the dashboard, a machine token acting as its owner, and a
[lead editing their own
team](#a-lead-editing-their-own-team), whose edit is exactly the one the
pipeline's next reconcile would overwrite:

```bash
curl -X PATCH $CREWLET_URL/config -H "$AUTH" \
  -H "Content-Type: application/merge-patch+json" -H "X-Summary: a tweak" \
  -d '{"mission":"ship it"}'
# 403 {"error":"config_managed","managed_by":["gitops"],"detail":"…","hint":"…"}
```

The dashboard shows the document as managed by `gitops`: the org builder opens
read-only and every control that writes the document is disabled with that
sentence. What stays open is the break-glass for a leaked credential, which the
managing system cannot know about — write the new value and re-publish the
unchanged document:

```bash
printf %s "$NEW_VALUE" | curl -X PUT $CREWLET_URL/secrets/GITLAB_TOKEN -H "$AUTH" --data-binary @-
curl -X POST $CREWLET_URL/config/reload -H "$AUTH" -H "X-Summary: rotate the leaked GitLab token"
```

The full list of what is refused and what stays open, and why, is in
[Managed configuration](../concepts/configuration.md#managed-configuration).

---

## Common error responses

Every `400` about the document carries `problems`, the failures located and
classified, beside the `detail` that renders them.

| Status | Error | Meaning |
|--------|-------|---------|
| `400` | `invalid_body` | The body is not YAML or JSON, or its shape is not a company's (an unknown key, a list where a mapping belongs). `Content-Type` is not what decides it |
| `400` | `invalid_patch` | A `PATCH` body that could not be merged, or that names a key the document does not have |
| `400` | `validation_error` | The whole resulting document failed validation; `detail` carries the message and `problems` locates each failure |
| `400` | `summary_required` | Any write with neither an `X-Summary` header nor a top-level `_summary` key in the body — asked once the write is admitted and its document checked, so a write you may not make is refused for that first |
| `400` | `invalid_query` | `dry_run` given as anything but `true` or `false` |
| `400` | `conflicting_preconditions` | A write carrying both `If-None-Match` and `If-Match`, to `/config` or a per-entity `PUT`; send one |
| `400` | `identity_mismatch` | A per-entity `PUT` whose body names another id than its path: this route never renames — a seat's handle and a unit's key are permanent, and a server or provider is renamed in the whole document, with everything that names it |
| `400` | `not_creatable` | A create-only `PUT` (`If-None-Match: *`) of a seat or a unit: each has a place the path cannot name, so add it by replacing the unit it sits in, or through `PUT /config` |
| `401` | `invalid_token` | Bearer missing / wrong / wrong scheme |
| `403` | `unauthorized` | The credential holds neither `admin`, which the route takes, nor, on the org chart, the lead relation; `reason` names the rule that refused the write. A lead's write that reaches outside their team names every part under `refused` |
| `403` | `config_managed` | The document is [managed by another system](#when-another-system-manages-the-document) and this credential is not one of the Tier A tokens `api.auth.company_writers` lists; `managed_by` names the writers. A write and its dry run alike, asked once the grant has admitted the caller and before the body is read |
| `403` | `step_up_required` | A write whose proof of identity is older than `step_up` when it arrived: confirm who you are and send it again. A proof fresh when the write arrived is not judged again part-way through it |
| `404` | `no_active_revision` | Reading `/config` before the company exists: the fleet has no activation, and this node knows no company. The `detail` says how to start one |
| `404` | `no_such_entity` | A per-entity `PUT` naming an id the active revision does not carry — a plain `PUT` never creates; send `If-None-Match: *` to add an MCP server or an LLM provider. Of a seat or a unit, only `admin` is told: a lead is refused `403` as for one outside their team |
| `404` | `no_route` | A path under `/config` this surface does not serve |
| `405` | `method_not_allowed` | A `/config` path under a method it does not take; `Allow` names the ones it does |
| `409` | `no_active_revision` | A per-entity write before the company exists: there is nothing to splice into |
| `409` | `revision_advanced` | The fleet's activation moved between the write's read of it and its own activation, for a write that named no revision in `If-Match` (or sent `If-Match: *`, or another resource's tag, or none at all, as a reload and a revert do): a concurrent writer won the race, the activation was removed under it, or a write built while the fleet had none lost to one published meanwhile. A write that lost at its activation keeps the revision it stored, inert, and names it as `stored_revision_id` and the node holding it as `node`: it is in that node's history alone, so its diff and its revert go to that node, by its own address where several share one — any other answers `404` for it. Re-read `/config` and send the edit again |
| `409` | `seat_held` | A write that removes a human seat, or makes it an agent's, while the identity directory binds somebody to it or an open invitation names it; `held` names the one thing holding each seat. Move or remove a person, unbind a service account, or cancel the invitation first |
| `412` | `revision_advanced` | `If-Match: <revision>` names a revision that is not the fleet's: when the write arrived, or because another write was activated before it. One that lost at its activation keeps and names the revision it stored as the `409` does. Re-read `/config` and send the edit again |
| `412` | `no_active_revision` | `If-Match: <revision>` sent before the company exists; retry without `If-Match`, or send `If-None-Match: *` |
| `412` | `already_configured` | `If-None-Match: *` on `PUT /config` sent while the fleet runs a company — on every node alike, one that has not applied it yet included — or, with no activation, while this node knows one. Also a create-only write that read no activation and lost to one published before its own landed: it keeps the revision it stored, inert, as `stored_revision_id`, in the history of the node named `node` alone, as a `409` does |
| `412` | `entity_exists` | A per-entity create (`If-None-Match: *`) naming an MCP server or LLM provider the active revision already has |
| `415` | `unsupported_patch_media_type` | A `PATCH` in a patch format other than a JSON Merge Patch, such as `application/json-patch+json` |
| `503` | `draining` | The node has been told to stop. Nothing was written; `Retry-After` says when to try again, against a peer or against this node once it has restarted — see [During a drain](../reference/api-endpoints.md#during-a-drain) |
| `503` | `seat_holders_unavailable` | A write that removes a human seat, on a node that could not read the identity directory to see whether anybody holds it. Nothing was written; retry after `Retry-After`, and where there is none, waiting won't change it |
| `503` | `unavailable` | With no `outcome` field: the node could not read the fleet's activation from the coordination store, so it cannot say which revision is the company: any write, and any read that answers or compares with the active revision — `GET /config`, the revision list and a single revision (each marked `is_active` against it), the reference index and a diff against `active`. Nothing was written, and nothing is answered from the node's own store instead; send it again after `Retry-After` (2 seconds — a coordination store electing a leader is out for a few). A [`/setup`](../reference/api-endpoints.md#supplying-them) submission's is the exception to "nothing was written": its company document was not changed, but it names the credentials it sealed first as `wrote_secrets`, and one whose own write the store did not answer, which may have landed, as `unanswered_secret`. Send it again all the same — that writes every credential in it again by name. With no `outcome` and a `detail` in place of that reason, any other condition of the node: a store too busy to take the write's revision, one closed while the node adopts a snapshot or shuts down, a request out of time. The `detail` says so in a sentence for a person, and a `Retry-After` comes exactly where waiting clears it |
| `503` | `unavailable`, `"outcome": "unknown"` | A write — `PUT`, `PATCH`, a per-entity or charter write, a reload, a revert, or a [`/setup`](../reference/api-endpoints.md#supplying-them) submission — stored its revision as `stored_revision_id`, built on `your_base`, on the node named `node`, and the coordination store did not answer its activation, so it may have landed. **Do not send it again**: once the store answers, `GET /config`; if the `ETag` names `stored_revision_id` the write landed, and otherwise `POST /config/revisions/{stored_revision_id}/revert` makes it the company, sent to that node — the revision is in its history alone, and any other node answers `404` |

The full reference is in [API endpoints](../reference/api-endpoints.md).
