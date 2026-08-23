# d-403 — Config models, validation and schema generation

Status: decided (largely by what is already built; the residue is named below)
Related: `404` (a validated config becomes an immutable epoch), `000`

## What was decided, and what built it

`internal/config` is the answer to this decision item, and it was built before
the decision was written down. Recording it here so the next reader does not
re-litigate settled choices:

- **Hand-written structs and imperative validators, not codegen.** ~100 Pydantic
  models became Go structs with `Validate(path string) error` methods that
  return `errors.Join` of every problem with its field path, rather than
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
- **The resolution FINGERPRINT is preserved** (`fingerprint.go`): an opaque,
  keyed, per-process digest of what a payload's `${VAR}` references currently
  resolve to. It exists because re-activating an unchanged revision is the
  documented credential-rotation gesture, so a payload-only comparison rebuilds
  nothing. It renders as `fingerprint(redacted)` and is never persisted or
  logged.

## The residue, and why the plan's version of it was wrong

The plan asked for "a parity test against `schema/*.json`". Taken literally
that is the wrong test now: those files are the PYTHON generator's output, and
this is a clean break where Tier A is a new shape
(`rewrite/questions/config-tier-a-shape.md` is still open on exactly that).
Asserting the Go schema matches them would pin the Go models to a shape the
rewrite has already decided against.

The guarantee the plan actually wanted is that the COMMITTED artifact matches
what the current models generate — the same rule the Python repo has, against
the new source of truth. So the build item is: regenerate `schema/*.json` from
the Go models, commit them, and add the test that regenerates and compares.
That lands with the CLI command that emits them, not before, because a
committed artifact nothing can regenerate is worse than no artifact.

Until then the schema files in the tree are the old generator's and are
STALE. That is stated here rather than left to be discovered.
