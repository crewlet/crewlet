# 506 — The observability pipeline, and the gate that found it missing

Status: decided, implemented
Relates to: [502 — the dashboard wire protocol is frozen](502-dashboard-wire-protocol.md), [504 — the dashboard gate](504-the-dashboard-gate.md)

## What was actually there

Every piece of the dashboard existed and worked.

`events/types/agent.go` defined `AgentPhaseStarted`, `AgentTurnProgress`,
`AgentPhaseCompleted`, `AgentTurnCompleted` and `TurnCompleted`, each with its
summary and its actor. `api/livestate` keyed on all five: it moved a seat into
`working`, hung an in-flight call off it, folded the spend, and cleared the row
at turn end. `api/stream` fanned the result out. The dashboard rendered it.
`store.EventLog` persisted rows and `queries` answered pages of them. Every one
of those had tests, and every one of those tests passed.

Nothing published a single one of those events.

```
$ grep -r 'AgentPhaseStarted\|AgentTurnProgress\|AgentPhaseCompleted' \
       --include=*.go . | grep -v '_test\|events/types'
$
```

And nothing subscribed either. The only `Ingest` call in the tree was the
webhook receiver's; the only `store.EventRecord` writer was the same. So a
Crewlet node ran turns, spent money, made deliveries — and served a dashboard
that showed an idle company with an empty feed, for ever, with no error
anywhere.

Each component was correct. The pipeline between them did not exist.

## Why nothing caught it

Every test in the tree stopped at a seam. The runner's tests scripted a model
and asserted a decision. The projection's tests fed it envelopes and asserted a
row moved. The socket's tests fed it a projection and asserted a frame. The
dashboard's own suites fed it frames and asserted a render.

Every one of those substituted the thing on the other side of the seam. So the
question "does anything actually connect these" was the one question no test
asked, and it is the question a gate has to ask, because it is the only one
that cannot be answered from inside a component.

## The pipeline

**Phases publish their own telemetry** (`agent/runner/telemetry.go`). The emit
point is `runPhase`, which alone knows the phase, the model that answered, the
tokens, the messages and the tool executions — `toolloop.Result` carries all of
it and `phaseResult` was discarding everything but the text. Three events:

- `agent_phase_started`, before the first provider call, plus the opening
  progress round (`round_num: -1`) carrying the prompt — so a seat that is
  thinking says which phase it is thinking in, and the live view can show what
  it was asked while it is still answering.
- `agent_turn_progress`, twice per round: once the model has spoken, before its
  tools run, and again once they return. Live-only.
- `agent_phase_completed`, the durable record — prompts verbatim, response,
  tools, tokens, decision.

The failure path publishes too. `toolloop.Progress` exists precisely for it: a
phase that dies returns no Result, and without the snapshot the only trace is
the started event — a dashboard showing an in-flight call with no response and
no reason.

**The engine closes the turn** (`engine/telemetry.go`), publishing
`agent_turn_completed` for the dashboard and `turn_completed` for the learning
subsystem. Two events for one turn because they have different readers, and one
event serving both would be the union of two schemas with every reader having
to know which half applied to it. Published on the error path as well: an error
means a phase broke, which is precisely when a dashboard most needs the turn
closed — the phase events already put the seat into `working`.

The token totals come from a per-turn tally on the Runner, so the turn event
reports the SAME numbers the phase events did. The engine summing what it can
see would be a second derivation, and the two drift the moment a rescue fires
or an extension runs the loop twice.

**The store is written by a publish listener** (`observe.Writer`), registered
at engine construction. It fires inline on the node that published — the node
that certainly has the event — so there is no round trip, no consumer group,
and therefore no way for two nodes of a fleet to write the same row and no way
for a rebalance to lose one. Every event is published exactly once by exactly
one node, which makes "the publisher writes it" the whole exactly-once rule.

Registered at construction rather than beside the dashboard for two reasons: a
worker-only node with no API still keeps a record of its turns, and a listener
attached later races the first turn a restarting node picks up off its durable
inbox.

**The projection is fed by a broadcast subscription** (`observe.Projector`),
over `crewlet.events.>`. It has to be: a dashboard served by node B must show
turns that ran on node A. A competing-consumer group would hand each event to
exactly one node's projection, so which turns a browser saw would depend on
which node answered its socket — and a refresh would tell a different story
than the page it replaced. A publish listener has the same defect from the
other side: it only ever sees what its own node published.

**The category map is the admission list.** A type with no category is written
nowhere and reaches no feed, silently, because the projection keys "is this
persisted" on the category being non-empty. So `TestEveryRegisteredTypeIsPlaced`
holds the map against the event registry: a new type must be given a category
or named in `excluded` **with the reason it must stay out**. The reason is the
point — an exclusion and an oversight are indistinguishable from outside, since
both are a type that is published and then vanishes.

Three types are excluded, and the third was found by that test on its first
run: `raw_webhook` is the wake a delivery publishes onto a seat's inbox, and
the delivery is already a row written by the receiver under its own id with the
raw provider bytes. Categorising it would have written a second row per
delivery saying the same thing.

## The wire-protocol bug the gate found next

With events flowing, `internal/e2e` ran a real company end to end — real
engine, real broker, real API, real turn, scripted vendor endpoint only — and
read the socket the way the dashboard dials it. Every frame arrived. Every
field was right. And the seat rendered idle from the first phase to the last.

The `agents` push was an object keyed by role:

```json
{"kind":"agents","data":{"CEO":{"state":"working","live_call":{...}}}}
```

`store.js` reads it as a list of rows, each carrying its own role:

```js
applyAgents(rows) {
  if (!Array.isArray(rows) || !rows.length) return;
  const byRole = new Map(rows.map((r) => [r.role, r]));
```

So every push failed the guard and was dropped. Four per phase, twelve per
turn, all discarded in silence. The server's own test asserted the map shape
and passed for it; the dashboard's suites passed; the socket delivered
everything.

The client is the compatibility reference and wins (502), so the server was
wrong and the server changed. The test that had certified the map is now
written the way the client reads the frame, and says why.

## The gate that closes it

Asserting "the frames say the right things" is not the same question as "the
client can read them", and only the second one is the product. So the gate has
two halves:

- `TestAGoldenCompanyRunsATurnOntoTheDashboard` — a real turn, and what reached
  the socket AND the store, which are fed by deliberately different halves of
  the pipeline so one working says nothing about the other.
- `TestTheDashboardClientCanReadWhatThisServerSends` — those exact bytes,
  unre-encoded, driven through the dashboard's own `socket.js` and `store.js`
  by `tests/test_dashboard/js/replay.mjs`, asserting the client ends up showing
  a seat working with an in-flight call through all three phases.

Verified by reintroducing the map shape: the replay fails with
"no seats: every `agents` push was dropped … no live call named the plan
phase". That is the bug, named, from the client's own reading of the wire.
