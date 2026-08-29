# d-601 — A coding run outlives its turn, and every rule follows from that

Status: decided
Related: `402` (the suspended conversation as a wire format), `401` (the
TurnContext a resume rebuilds), `101` (a queue result is a value),
`docs/concepts/code-sandbox.md`

## The one fact

A coding agent runs for minutes to hours. A broker ack window is measured in
minutes at best, a node restarts on any deploy, and a person answering a
question can take days. So a coding run cannot be a function call, a parked
goroutine, or a held connection: it is **work that outlives the process that
started it**, and every design choice below is downstream of that.

Three consequences, and they are not independent:

1. **The run's state is a record, not a stack.** The run record is what
   survives; the box, the conversation, and the seat's busy flag are all
   recovered from it. It lives in the FLEET's coordination store rather than
   the node's own database, and that is not an implementation detail: "the run
   outlives the process that started it" is only half the requirement, because
   the seat moves too. It was a `pending_sandbox_run` row in the node's file,
   so when the seat handed over — a lease lapse, a drain, a rolling upgrade —
   the successor's recovery pass listed nothing, the suspended conversation was
   unreachable, and a billed box ran to its own TTL with nobody to collect it.
   See migration 0013.
2. **The turn suspends rather than blocks.** The Execute loop stops with the
   `run_sandbox` call unanswered, its conversation is serialized, and a
   completion re-enters it. See d-402.
3. **Completion is detected by polling, not by a callback.** Only a poll can
   see a job that died before reaching its last step.

## Suspending is an optional interface, not a field on the tool contract

`tools.Result` is also the MCP tool contract. An MCP server has no notion of a
turn to suspend, so a `Suspend` field there would put a turn-engine concept on
every bridged tool to be ignored. The one tool whose work outlives its turn
implements `tools.Detached` and asks for the ability; the registry is
otherwise untouched. Same shape, same reason, as `SeatCallable`.

Its non-detached `Call` **refuses**. A surface that invoked it without the seam
would run the job and answer normally — the turn would end believing the work
was done, with nothing ever collecting the result.

## Three completion signals, and no timer

The poll asks three questions, because a finished, hung or dead job must not be
able to wedge a run, and there is no run-time TTL to fall back on:

- **the done marker** — the wrapper returned and wrote its exit code;
- **a terminal event in the streamed output** — for an agent that finishes its
  work and never exits (an open watcher or MCP child keeps its loop alive), so
  the shell never reaches the marker write;
- **process liveness** — the wrapper's pid is gone with no marker, meaning the
  group died before the tail `echo` ran.

The marker **carries the exit code** rather than being a zero-byte touch: a
sandbox read answers the same for a missing and an empty file, so a touched
marker would read as "not yet" forever.

A live wrapper always reads as NOT DONE, whether it is working or hung. A
hung-but-alive process is indistinguishable from a working one without a timer,
and imposing one is exactly what this refuses: the keepalive means a box is
bounded only by how long the engine can go *without* heart-beating, never by a
fixed deadline.

## The findings file is the result carrier of record

Not the streamed final message, and not the parsed envelope. Both can be
absent — a run that finishes without exiting loses its last message, and a
tool-only run parses to no text at all — while a file the brief asked for
survives both. Its presence is the success signal unless the process itself
crashed.

## The busy gate is a predicate, not a paused topic

The Python held a seat's inbox topic paused under a named reason while a run
was in flight, and carries three paragraphs about the hazard: a hold that is
never released leaves the seat *owned, attached and deaf* until the process
restarts, and nothing else in the engine can release it.

Here the inbox screening already parks (requeues) a partition when a seat is
awaiting a sandbox, so the gate is a question this node answers from memory:
the seat's owner is the only node that runs its turns, so its own memory is
authoritative for it, and taking the seat re-seeds the answer from the store.
The whole class of "one subsystem released another's hold" disappears.

It is **counted**, not boolean: a resumed Execute can launch a second run
before the first is settled, and the seat is free only when the last of them
is.

## Claim before you destroy, free at the last moment

Two orderings carry the flow, and both were arrived at from failures:

- **The pause reaper claims first and destroys second.** It decides from a
  snapshot seconds old, and the answer that un-parks the run may have arrived
  since — with an Execute loop already reconnecting to that very box. The
  compare-and-set is the authority for the whole reap, not just for the status
  field, which is why it is its own store verb (`ExpirePause`) rather than a
  plain status write.
- **The seat is freed only immediately before the resume dispatch.** Freeing it
  earlier lets a queued event take the slot, the resume fail, the completion
  NAK, and the redelivery find the claim already flipped — the suspended
  conversation lost for good. A resume that does fail un-claims back to the
  exact status the claim snapshotted.

## Recovery is per-seat, inside the acquire hook

Never a boot-wide scan. A fleet-wide scan is wrong twice: every node would reap
runs belonging to seats its peers own, and a node that takes a seat over
*later* would never recover it at all. Taking the seat's lease is also what
proves no live process holds the row, which is what makes reaping an abandoned
mid-resume tail safe there and only there.

## The local provider is the flagship

`type: local` is not the fallback; it is the backend that needs no account, no
API key and no token minting, running the coding CLI the operator already
logged in to. Its guarantees are POSIX primitives rather than a vendor SDK —
process groups for reaching an agent's whole tree, SIGSTOP/SIGCONT for the
clarification pause, `/proc` start times for the pid-reuse guard — which is why
the backend refuses to construct at all on a platform without them rather than
offering the same interface with none of the guarantees.

Go adds one requirement POSIX does not: a child is reaped only when something
calls `Wait`, and an unreaped process is a zombie that `kill -0` reports as
ALIVE — which is the completion probe. So the detached job gets a `Wait`
goroutine. Container mode gets the same property from `--init`.

**`Kill` sends SIGKILL with no SIGCONT first**, unlike the Python it replaces.
SIGKILL cannot be caught or blocked, so it reaches a stopped process directly;
waking the group first lets a reaped clarification pause run again on its way
out, which is the one thing `Kill` exists to avoid (`Connect` is the path that
resumes). `Close` keeps its SIGCONT — a stopped process really never runs to
handle SIGTERM.

## E2B is post-v1

The `Provider` seam ships; the backend lands when a deployment needs it. It is
**refused explicitly** rather than silently downgraded to local, which would
run an operator's code on the engine host without saying so.
