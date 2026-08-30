# adr-403 — Config models, validation and schema generation

Status: **Accepted**
Related: `404` (a validated config becomes an immutable epoch), `000`

## What was decided

`internal/config` is the answer. Recording it here so the next reader does not
re-litigate settled choices:

- **Hand-written structs and imperative validators, not codegen.** Roughly a
  hundred models, each a Go struct with a `Validate(path string) error` method
  returning `errors.Join` of every problem with its field path, rather than
  failing on the first. A config with four mistakes should report four, because
  the alternative is four edit-validate cycles.
- **`KnownFields(true)`** on decode. An unknown key is a typo the operator
  wants told about, not a field to ignore. The one place this is relaxed is
  where the schema documents a free-form map.
- **The schema is GENERATED from the models**, via `Schema(tier)`, so it cannot
  drift from the validators. `schema_test.go` pins three properties that matter
  more than the generation itself: generation is stable across runs (a
  regenerated artifact that differs byte-for-byte makes the check useless),
  every enum in the schema matches its validator's accepted set, and the schema
  never rejects what the validator accepts — the failure direction that would
  make an editor red-underline a legal config.
- **`${VAR}` resolution order is store-before-env**, and the validators call
  that resolution rather than reimplementing it, so a value that only resolves
  through the secret store validates the same way in both places.
- **There is no resolution fingerprint.** A design that short-circuits on an
  unchanged payload needs one — an opaque, keyed, per-process digest of what a
  payload's `${VAR}` references resolved to — because re-activating an
  unchanged revision is the documented credential-rotation gesture and a
  payload-only comparison would otherwise rebuild nothing. There is no such
  comparison here to defeat: `Apply` is
  straight-line, with no payload equality check and no early return — so there
  is nothing for the digest to guard and no `fingerprint.go` in the tree. See
  [`adr-404`](404-hot-reload-epochs.md) for what makes the rotation gesture work
  without one.

## The committed schema must be regenerable

The guarantee worth having is that the COMMITTED artifact matches what the
current models generate. `schema/*.json` is regenerated from the Go models by
`crewlet schema <tier>` and committed, and `cmd/crewlet/schema_test.go` runs
that command and compares the output byte-for-byte. The artifact and the
command that emits it land together, because a committed artifact nothing can
regenerate is worse than no artifact.
