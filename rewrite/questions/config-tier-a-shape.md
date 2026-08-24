# config — Tier A no longer has the Python shape, and the shipped example is now invalid

Status: **RESOLVED.** The key names stand, and all three follow-up edits
are done — the example, the quickstart and both generated schemas.
Raised by: the config-layer executor · Affects: `examples/nimbus.config.yaml`,
`docs/getting-started/quickstart.md`, `schema/bootstrap.schema.json`

## What changed

The Python Tier A file names a Pulsar broker, a PostgreSQL DSN and a pgvector
knowledge store under one `providers:` block:

```yaml
providers:
  queue:    {type: pulsar, url: "${CREWLET_PULSAR_URL}"}
  database: {dsn: "${CREWLET_DATABASE_DSN}"}
  knowledge: {type: pgvector}
api: {...}
secrets: {...}
debug: false
```

None of that is what this engine runs on. Per REWRITE_PLAN D2/D4/D5/D8 the
stream is NATS JetStream (embedded by default), the store is one local
SQLite/Turso **file** this node owns exclusively, and coordination is its own
slot. Porting the old keys would have produced Tier A fields nothing can
consume — dead config that validates.

Tier A is therefore:

```yaml
node:         {id, roles, labels}
store:        {path, driver, max_open_conns, busy_timeout_seconds}
stream:       {type: embedded|nats|pulsar, url, store_dir, cluster{name,port,peers},
               replicas, event_retention_hours, credentials, token, tenant, namespace}
coordination: {type: local|embedded-kv, lease_ttl_seconds}
api:          {host, port, auth{tokens, disabled, allow_anonymous_read, allowed_origins}}
secrets:      {active_key_id, keys[]}
debug:        false
```

`Bootstrap.Validate` also refuses the incoherent slot combinations D5 and D6
call for, each with a named reason: a fleet on `local` coordination, a
**two-node** fleet (no quorum), Pulsar filling the coordination slot, and
`replicas > 1` with no peers. `internal/config/bootstrap_test.go` covers each.

## What was left broken, and how it was closed

All three were found again from the other end, by pointing the binary at the
shipped example and watching it refuse — which is what a gate is for.

1. **`examples/nimbus.config.yaml`** is rewritten to the shape above, and
   `TestPythonEraBootstrapIsRefusedByName` is replaced by
   `TestBootstrapExampleLoads`: what needs pinning is no longer that the old
   shape is refused but that the shipped one keeps working, since it is what
   the quickstart, both bootstrap scripts and every integration walkthrough
   tell a founder to run.
2. **The quickstart's Tier A block** is rewritten with it, along with the
   `crewlet run` invocations across the docs — the Go CLI takes `-config` and
   `-company` rather than a positional path and `--import-company`.
3. **Both generated schemas** are regenerated from the Go models, and
   `TestTheCommittedSchemasMatchTheModels` compares them on every run. They
   matter more than a build artifact usually would: the examples carry a
   `# yaml-language-server:` modeline, so a stale schema flags a correct
   config as wrong on the exact key the author just learned about.

## The question for the architect — settled

Only whether the **key names** are right, since they become a public surface
the moment a doc references them. Specifically: top-level `store` / `stream` /
`coordination` rather than keeping a `providers:` envelope. Flat, because
"providers" in Tier B means *model* providers, and one word meaning two things
across the tiers is exactly the kind of drift the tier split exists to avoid.

**Kept.** The names are now in the shipped example, the quickstart, both
generated schemas and the guides, and reading them back in that setting they
say what they are: `stream` is where the durable log lives, `store` is this
node's own file, `coordination` is where seat leases live. A `providers:`
envelope would have said none of that twice.

Nothing about the validation rules is in question; those follow from D5/D6.
