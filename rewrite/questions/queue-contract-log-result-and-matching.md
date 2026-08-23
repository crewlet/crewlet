# q — `LogResult` and the subject matcher: two contract-package gaps

Raised by: the memory-backend + conformance-suite port (`internal/queue/memory`,
`internal/queue/queuetest`). Neither is a correctness problem in `queue.go`; both
are places where a shared thing is *almost* shared, which is the shape the topic
grammar was in before it got its own package.

## 1. `LogResult` cannot express a batch failure, so backends will not use it

`queue.LogResult` emits `handler_failed` / `delivery_deferred` with
`topic, group, event_type, reason|error`. That is exactly right for a
single-event delivery, and I use those field sets.

It cannot be used for a **batch partition**, for two reasons:

- The Python engine deliberately emits a *different* event name there —
  `batch_handler_failed` — and both backends emit it, with the comment that "log
  consumers must not see different names per backend"
  (`src/crewlet/queue/memory.py`, `_deliver_batch`). `LogResult` hardcodes
  `handler_failed`.
- A batch line carries `batch_key` and `event_count`. `LogResult` has nowhere to
  put them, and those are the two fields that make a batch log line readable.

So the memory backend logs both outcomes itself (`dispatch.go`, `invoke`'s
`failureEvent`/`attrs` parameters) and `LogResult` is currently called by
nothing. Every other backend will hit the same wall and grow its own copy —
which is precisely how the two Python backends came to need a comment reminding
them to agree on a string.

Suggested shape, if this is worth fixing (contract owner's call):

```go
func LogResult(l *slog.Logger, r Result, failureEvent string, attrs ...any)
```

i.e. the *name* and the *fields* come from the caller, and the contract package
owns only the outcome→level→"which fields are added for a nak vs a defer"
mapping, which is the part that must not diverge. The alternative — two
functions, `LogResult` and `LogBatchResult` — also works and keeps the two event
names in the contract package where they can be grepped together.

I did not change it: the signature is already published and another backend may
be depending on it.

## 2. The `*` / `>` subject grammar is documented in the contract and implemented nowhere

> **Resolved.** `topics.Match` now exists and says exactly this in its own doc
> comment. The memory backend calls it and its private `matching.go` is deleted,
> so there is one implementation again. The rest of this section is kept only as
> the record of why the function was added.

`EventQueue.SubscribeStream`'s doc defines the wildcard language (`*` = one
segment, `>` = trailing segments), and `queuetest` now certifies it
(`Stream/stream_star_matches_exactly_one_segment`). But there is no shared
implementation, so:

- the memory backend has a private `topicMatches` (`memory/matching.go`,
  ported from `src/crewlet/queue/matching.py`);
- NATS matches these subjects natively, so JetStream needs none;
- Pulsar translated the same grammar to a topic regex in Python
  (`_pattern_to_regex`) and will need to again.

That is two hand-written interpretations of one documented grammar, which is the
`crewlet.agent.{handle}.inbox` hazard in a different costume: a producer and a
consumer that disagree about a pattern never raise, they just stop matching. It
is *milder* — the conformance suite catches a divergence, whereas a hand-built
topic string had nothing checking it — so I left the matcher private and said so
in its doc comment rather than creating `internal/queue/matching` outside my
allowed scope.

Worth deciding deliberately: either promote it beside `topics` (one definition,
one test, backends that have native matching simply ignore it), or state in
`queue.go` that the grammar is per-backend and the suite is the only contract.
Right now it reads as shared but is not.
