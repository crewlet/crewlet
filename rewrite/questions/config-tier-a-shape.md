# config — Tier A no longer has the Python shape, and the shipped example is now invalid

Status: **decision made, needs ratification + a follow-up edit outside `go/internal/config/`**
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

## What this leaves broken, outside my remit

1. **`examples/nimbus.config.yaml` no longer loads.** Its `providers:` key is
   now an unknown field, which is a clean single error rather than a silent
   half-load — `TestPythonEraBootstrapIsRefusedByName` pins that on purpose,
   so the break is a decision with a name on it rather than a surprise. The
   file needs rewriting to the shape above (a worked example is in
   `TestBootstrapExampleShapeLoads`).
2. **The quickstart's Tier A block** is the same shape and needs the same edit.
   Its Tier B block is unaffected and is covered by `TestQuickstartCompanyLoads`.
3. **`schema/bootstrap.schema.json`** is generated from the Python models.
   `config.Schema(config.TierBootstrap)` now emits the Go one; whoever owns
   the `crewlet schema` command should regenerate both files.

## The question for the architect

Only whether the **key names** are right, since they become a public surface
the moment a doc references them. Specifically: top-level `store` / `stream` /
`coordination` rather than keeping a `providers:` envelope. I chose flat
because "providers" in Tier B means *model* providers, and one word meaning
two things across the tiers is exactly the kind of drift the tier split exists
to avoid.

Nothing about the validation rules is in question; those follow from D5/D6.
