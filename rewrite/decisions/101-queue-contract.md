# d-101 — The Go queue contract

Status: **decided** · Phase: 1 · Spec: `src/crewlet/queue/protocol.py`, `memory.py`,
`topics.py`; suites `tests/test_queue/test_protocol.py`, `test_topics.py`

The contract lives in `internal/queue/queue.go`. This note records the choices a
reader of that file should not have to re-derive.

## 1. Three outcomes, one return value

Python gives a handler two ordinary outcomes (return = ack, raise = nak) and smuggles
the third in as a control-flow exception (`DeferDelivery`). Go makes all three
explicit:

```go
type Result struct { Outcome Outcome; Reason string; Err error }
func Ack() Result; func Nak(err error) Result; func Defer(reason string) Result
```

`Ack` is the zero value, so a handler that returns `queue.Result{}` acknowledges —
the same default Python gets from a bare `return`. A handler that panics is recovered
by the backend and treated as `Nak`, preserving "an unexpected failure redelivers".

**Defer means: leave it unacked and stop consuming.** Not ack (that claims work this
process will not do), not nak (that spends dead-letter budget on a healthy event).
The closing consumer returns the message to the next attacher in order and at zero
redelivery cost. Never substitute a republish: that sends the event to the topic tail
while its prefetched siblings replay from the head, reordering the conversation.

## 2. Live-mutable batch options, made safe

Python's `BatchOptions` is a deliberately mutable dataclass the consume loop re-reads
every cycle, so a hot config reload changes linger/max-batch with no re-subscription.
Go keeps the behaviour and removes the data race: `BatchOptions` carries a mutex, is
read through `EffectiveLinger()` / `EffectiveMaxBatch()` (which apply the clamping
rules once, here, not per backend), and is written through `Set`. The
`MaxLingerSeconds = 60` ceiling stays in this package because the linger counts
against the broker's ack-timeout budget, and programmatic construction bypasses
config validation.

## 3. What every backend must provide

The interface is the same shape as the Python protocol, with the four attachment
verbs kept under their own names because each is differently destructive:

- `Quiesce` — stop taking new work, stay attached, leave fetched work unacked.
- `Unquiesce` — its reversible inverse. Required, not optional: a node whose lease
  store blipped quiesces and must be able to come back, or it is owned, attached and
  permanently deaf. On a prefetching backend this must also reclaim the prefetch.
- `Detach` — close consumers; subscription, cursor and retained mail all survive.
  Releases this attachment's pause holds.
- `DeleteSubscription` — destructive, and must work with **no local consumer**, so a
  role decommission never depends on which node ran the seat.

Plus: `EnsureSubscription` (create with no consumer, positioned at the earliest
message — one created at "latest" exists and still drops everything published before
its first consumer), `SubscribeStream` (ephemeral per-caller broadcast, `*` and `>`
wildcards), `PauseTopic`/`ResumeTopic` (reason-scoped holds keyed by the
`(topic, group)` **pair** — two subsystems gate one inbox and must not release each
other), `PauseDelivery`/`WaitForHandlers` (the one-way graceful-drain protocol), and
`Publish` that must persist before returning.

### The in-flight count is one implementation, not three

`queue.Inflight` — the counter behind `InFlightCount` and `WaitForHandlers` — lives
in the contract package and every backend embeds it. That is not tidiness. Two of
the three counted with a `sync.WaitGroup`, whose contract forbids exactly what a
dispatch loop does: *"calls with a positive delta that start when the counter is
zero must happen before a Wait."* A message arrives when it arrives, so a handler
can start while a drain is already waiting on an empty queue — and `Wait` may then
return on a momentary zero while that handler is starting, reporting a clean drain
through a running turn.

The memory twin never had it: a count under a mutex with a channel closed on the
transition to zero, which is what the shared type is. That the **twin** was right
and both real backends were wrong is the argument for one implementation — the
certified suite exercised the contract identically for all three and could not see
the difference, because every drain case waited on a quiet queue. It has one that
does not now (`a_handler_may_start_while_a_drain_is_waiting`), and its real
assertion is the race detector, which CI runs on everything.

## 4. Ordering policy lives in the contract package, not the backends

`PartitionByKey` and `OrderPartitionsOldestFirst` are exported pure functions shared
by every backend. Oldest-constituent-first dispatch is a fairness *policy* — a quiet
conversation whose requeued copies re-enter behind a hot conversation's fresh
arrivals would starve forever under receive-ordered dispatch. Both functions fall
back to arrival order rather than failing: ordering must never block delivery.

## 5. Topic grammar has exactly one definition

`internal/queue/topics` owns `crewlet.agent.{handle}.inbox`, its `.control` sibling,
and the `agent-{handle}` group names. An empty handle yields an empty subject, which
callers must treat as "not routable" — publishing to `crewlet.agent..inbox` is a real
topic nobody reads. A test greps the Go tree and fails the build on any hand-built
`crewlet.agent.` string outside this package, mirroring
`tests/test_queue/test_topics.py`.

## 6. Deliberately not ported

The asyncio-over-C++ bridging that is ~30% of `pulsar.py` (per-consumer thread
executors, `to_thread` wrapping every create/close, `call_soon_threadsafe` future
marshalling) has no Go equivalent and disappears. Go goroutines are the concurrency
model the Python code was emulating.
