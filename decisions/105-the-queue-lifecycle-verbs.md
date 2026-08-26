# d-105 — what the eleven verbs answer when a queue is not live

Status: decided
Supersedes: nothing
Related: `decisions/101-queue-contract.md` (the contract these verbs belong
to)
Applies to: `internal/queue/queue.go`, `internal/queue/queuetest/core.go`,
and every backend

## The gap

`EventQueue` has thirteen error-returning methods. Until this decision, the
contract said what exactly two of them do outside a queue's living state:
`Start` connects, `Stop` closes. For the other eleven — `Publish`,
`Subscribe`, `SubscribeBatch`, `Quiesce`, `Unquiesce`, `Detach`,
`EnsureSubscription`, `DeleteSubscription`, `SubscribeStream`, `PauseTopic`,
`ResumeTopic` — it said nothing, and the suite had never sent nine of them at
either point in a queue's life.

Both backends were internally inconsistent there, in opposite directions, and
neither was wrong by the contract as written.

**The in-memory twin**, measured on a stopped queue:

```
Publish             -> ErrNotStarted        <- refuses
Subscribe           -> ErrNotStarted        <- refuses
EnsureSubscription  -> true,  nil           <- CREATED broker state
DeleteSubscription  -> true,  nil           <- DESTROYED broker state
PauseTopic          -> nil                  <- TOOK A HOLD
SubscribeStream     -> nil                  <- registered a subscriber
Quiesce/Unquiesce/Detach/ResumeTopic -> false, nil
```

The `PauseTopic` line was a live bug. A hold is process-local state about an
attachment, so it survived into the queue's next life: `Stop`, `PauseTopic`,
`Start`, `Subscribe`, `Publish` delivered nothing, on a queue reporting itself
running, with `holds=[sandbox]`. Reachable by a sandbox gate or a config shed
racing a drain.

**JetStream**, measured on a stopped queue, is inconsistent the other way
round: everything that touches the broker fails with `nats: connection
closed`, while `Quiesce`, `Unquiesce`, `Detach`, `PauseTopic` and
`ResumeTopic` all return **success** against a connection that is gone. No
next life to leak into — `Start` cannot revive it — but a shutdown path could
believe it had gated a subscription that no longer existed.

**Pulsar**, measured on a stopped queue after the rule above was already
written, failed it in *both* ways at once — and is the reason this section now
lists three backends rather than two:

```
Publish/Subscribe/SubscribeBatch/SubscribeStream
                    -> pulsar: queue closed   <- refused, but NOT ErrNotLive
Quiesce/Unquiesce/Detach/PauseTopic/ResumeTopic
                    -> false, nil             <- succeeded
EnsureSubscription  -> true,  nil             <- CREATED broker state
DeleteSubscription  -> true,  nil             <- DESTROYED broker state
```

The two admin verbs are the sharp edge and are particular to this backend.
Subscription lifecycle here runs over the **REST admin endpoint**, which is a
separate client that `Stop` does not close — so unlike JetStream, where a
closed connection refuses on its own, a stopped Pulsar queue could still
provision a durable subscription nothing in the process would ever attach to,
or delete one along with the mail it retained. Nothing about the client being
gone stops it; only the flag does, so the guard is the whole mechanism rather
than a second line of defence.

`DeleteSubscription` gets no flag check of its own: it detaches locally first,
and that detach is the gate, so the admin call is unreachable on a stopped
queue. A second check there would be a guard no test could tell apart from the
first — and this repo does not keep guards nothing can falsify.

Pulsar reached this state honestly: it was written before the rule and
certified by a suite that only a CI job with a real broker runs, so the gap
was invisible to every local `go test ./...`. That is why the eleven verbs are
now also asserted against a *stopped offline queue* in the backend's own
package, where the answers are decided before anything reaches the wire.

## The rule, and why it is two rules

The two points a queue is not live are not the same point, and one rule
covering both is what left this ambiguous in the first place.

**Before `Start`: one answer for all eleven, and the backend picks it.**
Whether `Start` is the thing that makes a client live is a genuine property of
the backend — JetStream's `Open` establishes the connection and the streams,
so there is nothing before `Start` at all, while on the twin `Start` is
exactly what opens the client. `Capabilities.RequiresStart` says which, and
`an_unstarted_queue_answers_the_same_way_for_every_verb` sends all eleven and
refuses a backend that answers some one way and some the other.

**After `Stop`: every one of the eleven refuses. Not a capability.** "Not
connected yet" is a state a backend may legitimately not have; "closed" is a
state every backend genuinely reaches, and a verb that mutates through a
closed client is wrong on all of them. It is wrong in two different ways and
both were measured above: state that outlives the client (the twin), and
success reported for work that did not happen (JetStream).

The **drain protocol is exempt** along with `Start` and `Stop`.
`PauseDelivery`, `WaitForHandlers` and `InFlightCount` exist to run *around* a
stop rather than in spite of one — a drain that could not report its own
in-flight count once the queue was down would have no way to say whether it
finished. `Backend` and `AddPublishListener` return no error and so have
nothing to refuse with.

## What this rejects

**"Refusing costs nothing, so let the local verbs succeed."** It costs the
caller the ability to tell a hold that gates delivery from one that gates
nothing. Both of the measured failures are that confusion, reached from
opposite ends.

**"Make it one flag."** A single capability forces the pre-`Start` and
post-`Stop` answers to agree, which is precisely what JetStream cannot do:
before `Start` everything works, after `Stop` the connection is closed. The
first attempt at this rule did force them to agree, and the suite caught it —
JetStream failed six of eleven immediately.

**Clearing the gates in `Start` stays**, whichever answer a backend gives. It
is defence at the START of a life — a started queue serves, and is never
silently gated by state from a previous one — and it is a different guarantee
from this one.

## One refusal, one sentinel

A backend refusing is only half of it. `Node.OnRelease` detaches a seat's
mailbox and the seat host **keeps the lease** when that detach cannot be
proven — a seat this process may still be consuming must not go to a peer. A
queue that is not live consumes nothing, so a refusal there is *proof* of
teardown, and reading it as a failure would strand the seat for a full TTL on
the one path where the node is trying to hand it back.

Telling those apart cannot mean asking which backend is running; nothing above
`internal/queue` may. So the contract owns one sentinel, `queue.ErrNotLive`,
and each backend's own error wraps it — `jetstream.ErrClosed`,
`memory.ErrNotStarted`, `pulsar.ErrClosed`. `a_stopped_queue_refuses_every_verb` checks for it, so
a backend cannot refuse in a way its callers cannot read.

JetStream needed a second fix to satisfy that: its broker verbs surfaced
`nats: connection closed` from wherever the transport gave up. The check now
sits in `streamFor`, the one door they all pass through — and **before** the
stream cache, because a verb whose stream was already provisioned never
reached the transport at all. That made the answer a function of what some
other caller had ensured earlier in the process: correct most of the time, and
intermittent in exactly the way that gets called a flake.

## The generalisation

`Capabilities.RejectsPublishBeforeStart` was this rule with a sample size of
one verb, and the narrowness was the bug: it certified that `Publish` agreed
with the flag and said nothing about the ten methods beside it. When a
capability describes a backend's answer to a *class* of question, the suite
has to send the whole class — one member of it certifies that member.
