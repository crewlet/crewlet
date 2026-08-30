# adr-402 — Suspending a turn is a return value, and the state is a wire format

Status: **Accepted**
Related: `401` (the TurnContext a resume rebuilds), `404` (which epoch a resumed
turn runs under), `docs/concepts/code-sandbox.md`

## The shape

An Execute phase that launches a detached sandbox run must stop and be resumed
later — possibly on another node, certainly after a restart. Two ways to do
that, and only one survives the requirement:

- **A parked goroutine.** Cheap, obvious, and wrong: the run outlives the
  process. A node restart or a seat handoff loses every parked turn, and the
  sandbox completion arrives to nobody.
- **Serialize the state and re-enter.** The suspended Execute conversation is
  written to the run record's `execute_state`, and a completion re-enters the
  loop with it. Survives restarts and handoffs because the state is a record,
  not a stack — and because that record is the FLEET's rather than the node's,
  which is what makes "possibly on another node" above true rather than
  aspirational (see adr-601 and migration 0013).

**Serialize and re-enter. Never a parked goroutine.** This is not a preference;
a parked goroutine cannot satisfy "the run must survive the node that started
it", which is the entire point of the sandbox being detached.

## Suspension is a value, not a control-flow exception

The tempting shape is a control-flow exception thrown out of the loop. This
returns instead:

```go
// Outcome is what a phase run produced. Exactly one field is set.
type Outcome struct {
    Done      *PhaseResult
    Suspended *ExecuteState   // the loop stopped and can be re-entered
    Err       error
}
```

Same reasoning as `queue.Result` replacing `DeferDelivery` (adr-101): a suspension
is an ordinary, expected outcome of a phase, and expected outcomes are values.
An exception-shaped suspension is invisible in the signature and inevitably
caught by a `recover` somewhere that meant to catch something else.

## ExecuteState is a WIRE FORMAT, versioned from day one

It crosses two boundaries that make it a contract rather than an
implementation detail:

- **Between subsystems** — the agent layer writes it, the sandbox coordinator
  reads it back.
- **Between BUILDS** — a rolling upgrade means the node that resumes a run is
  routinely not the build that suspended it.

So it carries an explicit `Version int`, and decoding an unknown version is a
loud refusal that leaves the row untouched, never a best-effort read. A resumed
turn acting on a half-understood conversation is worse than a run that waits for
a node that understands it. Additive-only evolution, same rule as the event
envelope: new fields get defaults, nothing is removed or repurposed.

## The invariants, preserved verbatim

Each of these was learned at a cost and none is open for re-derivation. Each is checked at BOTH serialize and resume, because a state
that violates one is corrupt whichever side produced it:

1. **Exactly one dangling `tool_use`.** The suspended conversation ends with the
   `run_sandbox` call and no result. Zero means nothing to resume into; two
   means the model will answer one and strand the other.
2. **A repeat call of the pending tool is refused BEFORE execution.** Not after
   — launching a second sandbox and then rejecting it leaves a box running that
   nothing will ever collect.
3. **Resume is the SAME turn.** Same `turn_id`, Plan skipped, tool activations
   and skill-guard state replayed. A resumed turn that re-planned would re-derive
   a plan for work already half-done.
4. **Post-resume phase events show only the post-resume slice.** The pre-suspend
   rounds are already recorded; re-emitting them double-counts every token and
   redraws a turn the dashboard already has.
5. **The agent flips WORKING → AWAITING_SANDBOX in the suspending turn's
   deferred cleanup, never through IDLE.** A seat that passes through IDLE
   advertises itself as free, and something will give it work.

## Where the resumed turn's TurnContext comes from

Rebuilt, not restored. The state carries the turn's IDENTITY (turn id, trigger
refs, delegation chain, the conversation) — never its live handles. The budget
meter, phase recorder, LLM scope and config pin are constructed fresh at resume
from the node that is resuming.

The config pin is the interesting one: a resumed turn pins to the epoch **live
at resume**, not the one it suspended under. A sandbox run can be parked for
days waiting on a human answer, and re-entering under a config revision that has
since been deleted would resume a turn into a company that no longer exists. The
cost — a turn observing a config change across its own suspension — is real and
accepted, and it is why suspension is a phase boundary rather than a mid-round
pause.
