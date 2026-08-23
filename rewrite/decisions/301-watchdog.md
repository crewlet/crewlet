# d-301 — The watchdog watches the DUTY, not the loop

Status: **decided** · Phase: 3 · Spec: `src/crewlet/seat/watchdog.py`,
`docs/concepts/seat-ownership.md` § The wedged node, and why it leaves ·
Implementation: `go/internal/seat/watchdog.go`

## The thing Python was watching does not exist here

`EventLoopWatchdog` beats a coroutine on the asyncio loop and watches that beat
from a daemon thread. It works because **one loop ran everything**: a stalled
loop stopped the seat heartbeat, the queue consumers, the turn engine and every
hook at the same instant, so "is the loop turning?" was a complete health
question. And it was the *only* question a thread could ask, since a stalled
loop does not run `call_soon_threadsafe` either — nothing the thread schedules
can execute, so ending the process is the only remedy available to it.

Go has no such chokepoint. The seat heartbeat is a goroutine; so is each queue
consumer, each turn, each sandbox poll. **A stalled duty does not stall the
runtime**, and the runtime turning proves nothing about the duty. Transliterating
the Python design — a goroutine that stamps a timestamp on a ticker, watched by
another goroutine — would produce a watchdog that is green during the exact
failure it exists to catch: the heartbeat pass deadlocked on a mutex, blocked in
a hook that never returns, or wedged in a store call with no deadline, while the
beat goroutine ticks along beside it perfectly happily.

## The decision

**The watchdog watches named duties, and a duty proves itself by completing
work — not by existing.**

```go
type Pulse interface {
    Beat() (last time.Time, live bool)
}

w := seat.NewWatchdog()
w.Watch("seat-heartbeat", host)
w.Start(ctx)
```

`SeatHost` implements `Pulse`. Its heartbeat goroutine ticks far faster than it
renews — `beatInterval` = min(1 s, ttl/5, heartbeat interval) — stamping on every
tick and again when a pass returns, and running a renewal pass every *N* ticks.
So the stamp advances only while that goroutine is genuinely turning, and a pass
that hangs anywhere inside it freezes the stamp immediately, because the pass
runs inline in the goroutine whose ticks are the proof.

The seat heartbeat is the duty that matters, and it is worth saying why it is
the *only* one registered today: it is the single piece of work whose stall
silently converts "this node owns these seats" into "a peer owns these seats and
this node has not noticed". A stalled sweep costs a rebalance; a stalled
scheduler costs a fire; a stalled heartbeat costs ownership.

### Why one signal and not two

An earlier draft also ran an internal "the Go scheduler still runs Go code"
pulse, as the direct analogue of the Python beat. It was dropped as strictly
redundant: any stall broad enough to starve a plain ticker also freezes the
duty's stamp, so the duty check already fires — while a duty stall that leaves
the runtime healthy is invisible to the scheduler pulse. One signal, and the one
with the shorter path to the consequence.

There is a real limit here and it is worth stating rather than hiding: the
watcher is a goroutine, subject to the same scheduler as everything else, so it
cannot survive a *total* runtime freeze. That is acceptable, and for a reason
that is load-bearing rather than convenient — if the runtime is frozen that
completely, the queue client's own goroutines are frozen too, the server stops
seeing its heartbeats, and the session drops on its own. The state the watchdog
exists for is precisely the one where some goroutines still run, which is
exactly the state where the watcher still runs. (Go's `sysmon` runs on its own
thread and preempts, so a timer fires unless every P is stuck in
non-preemptible code.)

## Carried over unchanged

- **The threshold is the seat lease TTL and is not a config knob.** Past it the
  node is provably not the owner, and letting the two drift is how a process
  gets to be simultaneously "not the owner" and "still holding the mail". The
  Go form is stronger than the Python one: `NewWatchdog()` takes no threshold at
  all. The tight-threshold and no-exit seams are unexported, so only this
  package's own tests can reach them.
- **Beat and poll are CEILINGS scaled to threshold/5.** A beat that is slow
  relative to the threshold makes a perfectly healthy process report itself a
  whole window behind and shoot itself — invisible at the shipped values (45 s
  vs 1 s) and lethal to anyone who lowers the TTL. Both the watcher's poll and
  the host's beat derive from the threshold rather than from a constant, and
  both are pinned by table tests.
- **A GONE duty is not a WEDGED one.** From the watcher the two are
  indistinguishable — the beat simply stops refreshing — and they are opposite
  situations. `Beat` therefore answers *when* and *whether it is still supposed
  to be beating* in ONE call, because reading them separately races, and a
  stopped host read as a wedged one is the suicide timer this must never arm.
  In the Python engine that bug killed this repo's own test suite at 63 %, exit
  code 75, with zero test failures. A watchdog whose duties have all stopped
  stands down and stops watching; one with no duties registered never fires at
  all.
- **Disarmed for the whole of a graceful shutdown.** Teardown is the one part of
  the process that legitimately blocks for a long time — reaping MCP
  subprocesses, joining goroutines, tearing sandboxes down — so `Watchdog.Stop`
  is called *first* in the engine's shutdown, before `SeatHost.Stop`. Exiting
  through the middle of teardown abandons the seat release that makes a drain
  graceful: a shutdown that hangs is a `SIGKILL` away, a shutdown that exits
  without releasing costs every peer a full TTL of dark seats. The `live` flag
  is the second line of defence for a shutdown that forgets to disarm.
- **`os.Exit(75)`, and nothing else.** Go's `os.Exit` runs no deferred function,
  which is exactly the crudeness this wants — a process that cannot complete its
  own renewal pass cannot be trusted to run a graceful shutdown, and trying is
  how a watchdog ends up wedged too. 75 is distinct from any ordinary failure so
  an orchestrator's restart log says what happened. The stall line goes straight
  to stderr rather than through the logger, because a configured handler may
  batch or ship lines somewhere and that is more machinery than a wedged process
  has earned.

## What the exit is worth, honestly, on JetStream

The Python justification is a measured Pulsar number: a wedged-but-alive node
keeps its broker session open (the client answers keepalives from its own IO
threads), so the broker holds its **prefetch** of seats a peer now owns for the
full unacked-message timeout — roughly 30 minutes — and exiting collapses that
to 9 ms. On JetStream that specific hostage is smaller: pull consumers fetch
rather than receive, so a wedged node holds only what it had already fetched,
bounded by `AckWait` rather than by a 1000-message push queue (d-102).

The Go justification is different and, if anything, sharper. Because a stalled
duty does not stall the process, a Go node in this state is **still doing
things**: its consumers still fetch, its in-flight turn still runs, its MCP
children still act, its sandbox runs still complete — all on seats whose leases
have lapsed and which a peer has taken over. The freshness gate bounds part of
that (`MayStart` refuses within one heartbeat interval of the frozen stamp, and
epoch fencing bounces the writes), but it does not stop the turn already
running, and it cannot un-fetch the messages already reserved.

And nothing else can end it. Go has no way to kill a goroutine from outside;
`context` cancellation only reaches code that is watching a context, which a
deadlocked mutex, a blocking syscall or a cgo call is not. So the conclusion is
the Python one for a partly different reason: **ending the process is the only
remedy available**, and it is the one that lets a supervisor restart into a
healthy node.

Single node or fleet, it is armed the same way. With one node nothing is waiting
on the seats, but a node whose renewals have stopped is a dead node either way,
and leaving is what lets a supervisor notice.

## What it still cannot do

- It cannot stop the work — only the process. That is the same limitation the
  Python design records, and it is why correctness against zombies comes from
  epoch fencing rather than from the watchdog.
- It cannot tell a duty that is slow from one that is stopped, and does not try:
  the threshold is the point at which the distinction stops mattering, because
  the lease has lapsed either way.
- It cannot survive a total runtime freeze — see above for why that case does
  not need it.
