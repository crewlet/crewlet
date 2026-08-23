# q — what do the attachment and subscription verbs do on a STOPPED queue?

Status: **RESOLVED — consistency is the requirement, not any particular answer**
· Decided by the contract owner · Contract: `go/internal/queue/queue.go`

## The ruling

The contract does NOT say what a stopped queue answers, and it should not: the
two shipped answers are both legitimate. On JetStream, `Open` establishes the
connection and the streams, so `Start` is a no-op and a publish before it works
— that is a real property of that backend, already expressed as
`Capabilities.RejectsPublishBeforeStart`. On the twin, `Start` is the thing that
makes the client live.

What is NOT legitimate is a backend answering DIFFERENTLY PER VERB, which is
what the measurement above found: `Publish` and `Subscribe` refused while
`EnsureSubscription` created durable broker state, `DeleteSubscription`
destroyed it, and `PauseTopic` took a hold. A caller cannot reason about a
lifecycle whose rules change per method, and the hold case was live — it
survived into the next life and left a restarted queue reporting itself running
and silently deaf.

So the rule is: **a backend answers the same way for every verb that is not
`Start` or `Stop`.** Either it requires `Start` and refuses them all with
`ErrNotStarted`, or `Start` is a no-op and none of them require it. That
generalises `RejectsPublishBeforeStart` from one verb to all of them, and it is
checkable rather than a matter of taste.

Clearing the gates in `Start` stays regardless. It is defence at the START of a
life — a started queue serves, and is never silently gated by state from a
previous one — and it holds whichever answer a backend gives.



Raised by: a teammate's lifecycle axis — *enumerate the points in an object's
life at which each operation is sent*, which is a different question from what
inputs a suite sends, and one `queuetest` had never asked.

## The gap

After a `Stop`, this suite sends exactly two things: `Start` and `Publish`. The
other nine verbs — `Subscribe`, `Quiesce`, `Unquiesce`, `Detach`,
`EnsureSubscription`, `DeleteSubscription`, `PauseTopic`, `ResumeTopic`,
`SubscribeStream` — are never sent at that point in the queue's life.

## Measured, on the in-memory twin

```
Quiesce             -> false, nil
Unquiesce           -> false, nil
Detach              -> false, nil
Subscribe           -> ErrNotStarted        <- refuses
Publish             -> ErrNotStarted        <- refuses
EnsureSubscription  -> true,  nil           <- CREATED broker state
DeleteSubscription  -> true,  nil           <- DESTROYED broker state
PauseTopic          -> nil                  <- took a hold, created the sub
ResumeTopic         -> nil
SubscribeStream     -> nil                  <- registered a subscriber
```

`Publish` and `Subscribe` refuse; four other verbs happily mutate broker or
client state on a queue whose connection is closed. That is internally
inconsistent, and `queue.go` does not say which behaviour is right.

A networked backend almost certainly errors on all of them — there is no
connection — so the first backend to be asked will diverge from the twin, and
neither will be wrong by the contract as written.

## One consequence was a live bug, now fixed

`PauseTopic` on a stopped queue took a hold, and `Start` did not clear it:

```
Stop → PauseTopic → Start → Subscribe → Publish   ⇒ nothing delivered,
                                                     holds=[sandbox]
```

A restarted queue reporting itself running, silently deaf on that seat — the
exact incident `Stop`'s doc cites ("a hold that outlived a stop left a reused
queue silently deaf"), reached through the window between `Stop` and `Start`
rather than across the `Stop` itself. Reachable by a sandbox gate or a
config-divergence shed racing a drain.

Fixed in the twin by clearing the process-local gates in `Start` as well as in
`Stop`: the invariant belongs to the START of a life, not the end of one. Case
added (`a_hold_taken_while_stopped_does_not_survive_a_restart`, gated on
`Restartable`), mutation-checked to fail without the fix and only there.

## The question this leaves

The bug is closed either way, but the contract gap is not:

1. **Every verb refuses on a stopped queue**, consistently with `Publish` and
   `Subscribe`. Cleanest to reason about; makes the window above unreachable
   rather than merely harmless. Costs `DeleteSubscription` the ability to
   decommission from a stopped client — though that verb's requirement is "must
   not require a local *consumer*", which is not the same as "must work with no
   *connection*".
2. **Broker-side admin verbs stay available**, and the contract says so, on the
   grounds that `EnsureSubscription` / `DeleteSubscription` are administrative
   rather than data-path.
3. **Left unspecified**, and the suite must never send those verbs at that
   point — which is what it was already doing by accident, and is how this
   stayed invisible.

I would take (1). I have not changed the contract: the suite's new case skips
when a backend refuses `PauseTopic` while stopped, so both readings pass today.
