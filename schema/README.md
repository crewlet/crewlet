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
express structure — key spaces, types, closed sets, ranges, patterns, and
the few Tier A fleet rules a JSON Schema condition can state — and not the
cross-field rules `Company.Validate` enforces. The invariant is
one-directional: everything the schema rejects, the validator also rejects.
An editor that red-underlines a config the engine would happily run teaches
authors to ignore it.

So a field admits what the engine's **decoder** reads there, not merely what
its Go type spells, and `internal/config/schemaleaves_test.go` offers every
field of both tiers each such value to hold the schema to the decoder's
verdict:

- **An empty value** (`~`, or `""`) is unset, and admitted everywhere except
  the company document's root, which the engine refuses when empty.
- **A number or a boolean in a text field** is that field's text, as written
  (`name: 2024`, `org_webhook: false`), and a closed set lists the forms YAML
  turns its words into.
- **YAML 1.1's switch words** — `yes`, `no`, `on`, `off`, `y`, `n`, in each of
  YAML's three casings — are admitted in a boolean field, which is where the
  decoder reads them.
- **A Tier A `${VAR}`** is resolved before the file is decoded, so it is
  judged by what it could resolve to: admitted anywhere in a text field, a
  patterned or closed-set one included, and as the WHOLE value of a number or
  a boolean, whose resolved text is read as if written there. A reference with
  other text around it in a number or a boolean is refused, as the engine
  refuses it.
- **A Tier B `${VAR}`** is stored verbatim and resolved only where a provider
  or transport is built, so it is judged as the literal text it is — a field
  with a pattern or a closed set refuses it — except in a field tagged
  `pointer`, whose consumer resolves exactly one WHOLE reference: that field
  admits one beside its own rule. The Mattermost seat `username` is the one
  such field today.

A fraction in a whole-number field is refused by the loader rather than
truncated, so `integer` in these files is exactly what the engine reads.

There is no `extensions` block in the company schema, and the absence is
deliberate: this engine has no in-process extension system for a schema to
describe. What a company extends it extends through MCP servers and the
knowledge base, both of which are ordinary config.
