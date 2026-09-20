# ADR-0006 — Event evolution is additive-only and unknown types round-trip

- **Status:** accepted
- **Authority:** `internal/events`
- **Enforced-by:** `internal/events.TestAnUnknownTypesLargeIntegersSurviveARoundTrip`, `internal/events/types.TestUnknownTypeSurvivesIntact`, `internal/events/types.TestPayloadTagsMatchTheWireContract`
- **Cost-when-tried:** a consumer that *ignores* what it does not recognise is indistinguishable from one that drops it, and during a rolling upgrade the older nodes are the ones holding the newer events — so every upgrade would strip the new fields off every event it forwarded.
- **Tag-status:** unreleased

## The decision

An event carries no schema version field, because evolution never needs one:
changes to an event type are **additive only**. New fields arrive with
defaults; existing fields are never removed or repurposed.

**A rename is a removal.** The JSON key is the field's identity on the wire, so
changing one deletes a field and adds another in the same stroke — the older
half of a rolling upgrade stops finding what it reads, and every row already
written keeps a key nothing reads any more. That holds when the *name* is what
was wrong rather than the meaning: `prompt.size` measures bytes under keys that
say `system_chars` / `user_chars`, and the keys stay, because a key is an
identifier and not an assertion. What gets corrected instead is everything that
is not a peer contract — the Go identifier, the doc comment, the dashboard's
label, the published docs. Adding a second key for the same value is the
mirror-image mistake: one fact with two identities, and this ADR means the
first one can never be removed.

A consumer does not ignore what it does not recognise, it **preserves** it. An
event type this build has no payload for still decodes into the envelope, keeps
its unknown fields, and re-publishes them unchanged — including large integers,
which a naive `map[string]any` round trip silently rewrites through `float64`.

## Why the obvious alternative is wrong

The obvious alternative is a version field and a migration per bump. It is a
contract between *peers* rather than between a build and a file, and peers
during a rolling upgrade run in both directions at once: the older node holds
the newer event as often as the reverse. A version field tells the older node
that it does not understand the event; it does not tell it what to do, and
every honest answer is worse than preserving the bytes.

Ignoring an unknown field is the failure that hides: it looks like tolerance
and behaves like deletion. Preserving instead means old and new nodes coexist
on one stream with no migration and no ordering requirement between them.

## What this does not decide

It is the same contract, for the same reason, that
`internal/statelog` holds for a record it cannot decode — kept at its position,
never skipped, reprocessed when a build that can read it arrives — but the
state log has one exception this does not: a record that installs an apply
GATE is a STOP rather than a retain, because a deferred gate does not postpone
one record's effect on one node, it silently licenses every record above it.
