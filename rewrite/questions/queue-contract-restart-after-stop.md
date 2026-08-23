# q — can a stopped queue be started again?

Raised by: the conformance suite running against two backends that answer
differently. Low stakes next to the `Detach` question, but the same shape: the
contract is silent, so the suite was about to render a verdict it has no
authority for.

## The disagreement

`queuetest` `EventQueue/stop_clears_pause` (ported from the Python
`test_stop_clears_pause`, "a fresh start() after stop() resumes normal
delivery") does `PauseDelivery` → `Stop` → `Start` → `Subscribe` → expect
delivery.

- **memory** — passes. `Stop` is a client disconnect: the broker and its mail
  outlive it, holds and quiesce flags are cleared (deliberately — "a hold that
  outlived a stop left a reused queue silently deaf"), so `Start` serves again.
- **jetstream** — fails: `Subscribe(topic.r, grp): ensure stream
  CREWLET_NS_TOPIC: nats: connection closed`. `Stop` closes the NATS connection
  and `Start` does not re-establish it.

## Why the contract does not settle it

Both readings are defensible from `queue.go` as written:

- **Restartable.** `Start` "connects the backend and begins consuming", `Stop`
  "closes the connection". Nothing says `Start` may only be called once, and the
  plain reading of a connect/close pair is that it can be reopened.
- **Terminal.** `PauseDelivery` is explicitly one-way — "once paused, the engine
  is shutting down" — and `Stop` is the end of that sequence. On that reading a
  stopped queue is spent and a restart means a new queue.

The Python spec asserts restartable, but the memory twin is also the only
backend it ever ran against, so that is weak evidence: it may be recording the
twin's implementation strategy rather than a decision. That is the trap a
teammate flagged today on their own suite — *a conformance suite must not
require the happy path of the backend it was written against* — and this looks
like an instance of it.

It also matters very little in production either way: nothing in the engine
restarts a queue in-process today. Which is an argument for deciding it cheaply
rather than for leaving it ambiguous, since an undecided lifecycle point is
exactly what a future config-reload path will trip over.

## What I did meanwhile (suite only)

Gated behind `Capabilities.Restartable`, default off. memory sets it true, so
the property cannot rot on the backend that has it; jetstream skips with a named
reason rather than failing for a promise the contract never made.

If the answer is "restartable", jetstream has a real bug and the flag should be
deleted in favour of a plain requirement. If it is "terminal", the flag should
stay and `Stop` should say so in `queue.go`.
