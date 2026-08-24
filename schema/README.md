# Generated config schemas

These are **generated**, never hand-edited: `crewlet schema <tier> -o <path>`
emits them from the Go types in `internal/config`, and
`cmd/crewlet/schema_test.go` regenerates and compares on every build. A
config field added without a schema entry is a failing test rather than a
stale file nobody opens.

To update after a config change:

```sh
go run ./cmd/crewlet schema company   -o schema/company.schema.json
go run ./cmd/crewlet schema bootstrap -o schema/bootstrap.schema.json
```

The schema is a **subset** of the validator and must stay one. It can
express structure — key spaces, types, closed sets, ranges, patterns — and
not the cross-field rules `Company.Validate` enforces. The invariant is
one-directional: everything the schema rejects, the validator also rejects.
An editor that red-underlines a config the engine would happily run teaches
authors to ignore it.

These differ from the repository-root `schema/` pair, which the Python
engine generates. The difference is real and intended: per the rewrite
plan's D11 the Python in-process extension system is not ported, so the
company schema here has no `extensions` block. The root pair is replaced by
this one when the Go tree moves to the root.
