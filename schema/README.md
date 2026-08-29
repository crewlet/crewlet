# Generated config schemas

These are **generated**, never hand-edited: `crewlet schema <tier> -o <path>`
emits them from the Go types in `internal/config`, and
`cmd/crewlet/schema_test.go` regenerates and compares on every build. A
config field added without a schema entry is a failing test rather than a
stale file nobody opens.

To update after a config change:

```sh
make schema   # both tiers, from the repository root

# or one at a time:
go run ./cmd/crewlet schema company   -o schema/company.schema.json
go run ./cmd/crewlet schema bootstrap -o schema/bootstrap.schema.json
```

The schema is a **subset** of the validator and must stay one. It can
express structure — key spaces, types, closed sets, ranges, patterns — and
not the cross-field rules `Company.Validate` enforces. The invariant is
one-directional: everything the schema rejects, the validator also rejects.
An editor that red-underlines a config the engine would happily run teaches
authors to ignore it.

There is no `extensions` block in the company schema, and the absence is
deliberate: this engine has no in-process extension system for a schema to
describe. What a company extends it extends through MCP servers and the
knowledge base, both of which are ordinary config.
