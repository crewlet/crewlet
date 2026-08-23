# q — what Go type does a free-form `Payload` value have after delivery?

Raised by: applying a teammate's finding on `coord` (the same drift, on the
lease store's `meta`) to `queuetest`. Same bug class, same cause — the suite's
fixtures were in the shape its backend produces, so it could not see the
question.

## Measured, this repo's memory twin

```
key       written      delivered
replicas  int          float64
roles     []string     []interface {}
conv      string       string
```

So `ev.Payload["replicas"].(int)` succeeds on the publishing node and fails on
every consumer. Nothing errors; a type assertion without the comma-ok form
panics in a handler, and with it silently reads a zero.

## Why this is a question and not a fix

d-103 is explicit that it is the backend's call:

> **What this does NOT require** — Any particular encoding. […] A backend is
> free to use whatever wire format it likes.

So requiring `float64` / `[]any` of every backend would forbid what a decision
permits, and a gob or in-process store could reasonably preserve `int` and
`[]string`. That is a contract verdict, and `queue.go` says only
`Payload map[string]any`.

**Measured gap:** a backend whose codec preserves Go types passes the whole
suite today — verified by handing consumers a correctly-decoded event whose
free-form `Payload` keeps the publisher's Go types. Both shipped backends agree
(both are JSON), so nothing diverges right now; a third could, and would be
certified.

## What the suite does about it meanwhile

`Wire/a_free_form_payload_value_survives_whatever_type_it_lands_as` asserts the
VALUE survives — compared as canonical JSON, under which `int(3)` and
`float64(3)` are the same value — and never the Go type. Mutation-checked both
ways: it fails a codec that silently drops a value it cannot encode, and it
passes a codec that preserves Go types, which is the behaviour this note leaves
undecided. The string-only fixture assumption is now stated at `newConvEvent`.

## The options

1. **Say the type is JSON's.** `Payload` is documented as the free-form bag on
   an envelope whose canonical form is JSON, so `float64` / `[]any` / `nil` is
   what a consumer gets. Backends using another codec normalise to it. Costs a
   real constraint on a hypothetical backend; buys callers one answer.
2. **Say the type is unspecified**, and that callers must not type-assert a
   free-form value — use `json.Number`-style tolerant reads, or put the field in
   a registered `Data` payload where the decoder gives it a real Go type.
3. Leave it undecided and keep the gap written down.

I would take (1) or (2), because (3) leaves a footgun that fails in the handler
rather than at the boundary. Worth noting that the typed path already has the
right answer — a registered payload decodes into this build's struct, so
`Count int` is an `int` on both sides — which is an argument for (2) plus a
nudge toward registering anything whose type matters.
