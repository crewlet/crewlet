# q — does `Detach` wait for an in-flight handler, or abandon it?

Raised by: the conformance suite (`queuetest`) running against two backends that
answer differently. Found as a **10-minute deadlock**, not a test failure, which
is the part that makes it worth a contract decision rather than a bug report.

## What happened

`queuetest` `Attachment/re_attaching_clears_a_stale_quiesce` holds a handler
in-flight, calls `Detach`, and only then releases the handler. That ordering is
the incident it reproduces — the Python spec's own words:

> `detach` clears the flag — but an in-flight handler abandoned by that detach
> can still raise `DeferDelivery` afterwards, and `_invoke` puts the key
> straight back.

- **memory** — `Detach` removes this client's consumers and returns. The
  in-flight handler keeps running and lands its deferral afterwards. Test passes.
- **jetstream** — `Detach` → `attachment.stop()` joins the dispatch goroutine
  (`attach.go:92`, reached from `attach.go:337`). The dispatcher is running the
  test's handler, which is waiting to be released *after* `Detach` returns.
  Deadlock; the binary died on the 10-minute test timeout.

`queue.go` does not say which is correct. It says `Detach` "closes this
process's consumers, leaving the subscription", and separately that `Quiesce`
"stops taking NEW work … a running handler finishes". The contrast is suggestive
but never stated, and two backend authors read it two ways.

## Why I believe abandon is the right answer

Not just because it is what the twin does — the reason is in the seat layer:

- `Quiesce` and `Detach` are separate verbs *because* they differ here. If
  `Detach` also waited for the running handler, `Quiesce` would have no distinct
  meaning: the documented voluntary path is quiesce-then-detach precisely so the
  in-flight turn finishes **before** the seat moves.
- `seat/host.go` names the two release paths "voluntary quiesce-then-detach" vs
  "**fenced detach-first-abandon**". The fenced path exists for a node that has
  *lost its lease* — the one moment it must not block. An agent turn can run for
  minutes; a `Detach` that joins its dispatcher would hold a node that no longer
  owns the seat behind work it no longer has the right to do, while the seat's
  new owner waits.
- The deferral in this very test is what an abandoned handler does on its way
  out, which only makes sense if `Detach` returned without it.

So the contract likely wants a sentence on `Detach` along the lines of: *Returns
without waiting for a handler already running; that handler's outcome still
applies when it completes. Use `Quiesce` first if the in-flight work must finish
before the attachment goes away.* And the matching sentence on `Quiesce`.

I have not edited `queue.go` — this is the contract owner's call, and if the
answer is instead "Detach joins", then the suite's test and the `seat` docs both
need changing, which is a bigger decision than a backend fix.

## What I changed meanwhile (suite only)

The suite no longer deadlocks on this. `Detach` runs on its own goroutine with a
bounded wait; on expiry the suite releases the handler so the process can exit
and fails with a message naming this file. A shared conformance suite that hangs
a backend's test binary costs its author ten minutes and a goroutine dump to
learn what one line can tell them — and, worse, the hang looks like it belongs
to whichever package happened to be timed out, which is how this one first got
reported against `memory`.

The same guard now covers every other unbounded channel receive in the suite
(`awaitSignal`).
