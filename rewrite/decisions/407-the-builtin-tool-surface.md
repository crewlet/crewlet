# 407 — The builtin tool surface, and how a tool learns who called it

Status: decided, implemented
Relates to: [401 — context threading](401-context-threading.md), [404 — hot reload as immutable epochs](404-hot-reload-epochs.md), [601 — MCP annotations](601-mcp-annotations-and-child-supervision.md)

## What was missing

The Go registry held three tools: `activate_tool`, `list_mcp_server_tools` and
`spawn_subagent` — and the first two are meta-tools the runner builds per
surface, while the third was a type nothing ever registered.

Python's set also has `lookup_colleague`, `use_skill`, `load_tool_skill`,
`a2a_ask`, `query_episodes`, `reflect_and_persist`, `refine_skill`,
`refresh_memory` and `mark_onboarded`. The subsystems behind all of them were
already built here — `internal/learning`, `internal/a2a`, `internal/org`. What
was missing was the surface that lets an agent reach them.

Worse, `NewCompany` built every epoch with `tools.NewRegistry()` and nothing
ever put anything in it. A Crewlet node ran turns with an **empty tool
catalogue**: every planner was shown nothing, every Execute phase could call
nothing, and the delivery gate correctly judged every turn as having delivered
nothing — which reads as a model that stopped trying.

## The problem the design has to solve

Every one of these tools speaks FOR a seat. `a2a_ask` asks as somebody.
`reflect_and_persist` writes into somebody's diary. `use_skill` loads
somebody's distilled experience. `mark_onboarded` records that somebody has
oriented themselves.

So each is an authorization decision, and each would be forgeable if the seat
arrived in the model's arguments — a model that wrote `"requester":
"agent-cto"` could ask a question as its colleague.

But the registry is **per epoch, not per seat**, and has to stay that way: the
catalogue a planner is shown comes from the registry, so a per-seat
registration would list every builtin once per colleague. Meanwhile 401 forbids
a third `context.Context` value (only the work key and log fields qualify — the
consumers of both are leaves in code with no turn concept), and widening
`mcp.Callable` would put a turn-shaped parameter on every bridged MCP tool to
be ignored.

## Decision

**An optional interface, resolved at the one frame that holds both halves.**

```go
// tools.SeatCallable
type SeatCallable interface {
    Callable
    CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (Result, error)
}
```

`Surface.invoke` type-asserts. A plain `Callable` — every MCP tool, every
discovery meta-tool — is unaffected and knows nothing about turns. A
`SeatCallable` is handed the turn the **surface** was built for, by the runner,
which is the frame that knows.

The seat therefore cannot come from the arguments, structurally rather than by
a check somebody has to remember to write.

### `turnctx.Turn` is 401's TurnContext, arriving

It carries what a turn IS: the work key, the acting seat, the org it is pinned
to, and the delegation depth and chain. Immutable; `ForSubagent` derives rather
than mutates, keeps the org (a sub-agent must see the same company its parent
does), gives the child its OWN seat (a sub-agent acting as its parent makes the
delegation cap unenforceable, because nothing downstream can tell them apart),
and copies the chain rather than appending in place — `append` can share a
backing array, and two sub-agents derived from one parent would write over each
other's.

The org comes off the **pinned** epoch, so a colleague lookup mid-turn resolves
against the roster the turn started under (d-404).

### Node deps at registration, seat at call

`builtin.Deps` holds the node-level things — the A2A service, the learning
stores. Those are the same for every seat, which is exactly what makes one
registration per epoch correct: the thing that varies per call is the CALLER.

Every field is optional, and a tool whose dependency is absent is **omitted,
never registered-and-broken**. A model shown a tool that always fails learns to
distrust the whole catalogue and burns a round finding out each time. A node
with no store gets `lookup_colleague` and nothing else, which is exactly what it
can serve.

### Epochs are equipped, and equipped every time

`NewCompany` still builds an epoch without tools, because building one must be
something `crewlet validate` can do on a laptop with no database. `Engine.equip`
fills the registry — at boot **before the epoch is published**, and on every
apply for the same reason. An epoch is published rather than mutated, so each
one gets a new registry; a node that equipped only its first would serve a
company whose agents lost every builtin at the first config change, with nothing
failing.

A failure to equip fails the **apply**. Serving the epoch anyway would publish a
company whose agents cannot look up a colleague or recall their own work.

## `lookup_colleague` never guesses

An agent that silently addressed the wrong colleague is worse than one that
asked which: the wrong person is pulled into work that is not theirs, and the
right one never hears about it. So an ambiguous query returns the candidate
list and says it is ambiguous.

Four tiers, earlier ones short-circuiting later ones (`internal/agent/colleague`):
exact identifiers → case/separator-folded equality → partial names in both
directions → fuzzy. The short-circuit is what stops a query that IS somebody's
handle from coming back as a list of everyone it resembles.

Two details are ported rather than approximated:

- **`Normalize`** does NFKD + casefold + combining-mark stripping, then maps
  Turkish dotless `ı` explicitly — it is not decomposed by NFKD and does not
  casefold to ASCII `i`, so without it an ASCII query `yazilim` cannot reach a
  role named `Yazılım`, which is exactly the kind of seat a model types from
  memory. Verified against Python on 38 samples including `İK`→`ik`, `ß`→`ss`,
  `ﬁle`→`file`, `Ⅻ`→`xii`.
- **`ratio`** is `difflib.SequenceMatcher.ratio`, ported. Not substituted:
  `FuzzyCutoff = 0.6` is a number tuned against real role names on THIS
  measure, and swapping in Levenshtein or Jaro-Winkler would keep the constant
  while silently changing what it means. Differential-tested against Python on
  1,800 pairs — 600 ASCII, 1,200 with Unicode and lengths to 30 — bit-identical
  after one fix the test found: two empty strings are 1.0 in difflib, and the
  first Go version returned 0. Unreachable from `Resolve` (tier 4 needs four
  runes), but a ported primitive that disagrees with its original anywhere is a
  disagreement waiting for a caller.

## Annotations are not optional here

The delivery gate reads them, and **unannotated counts as not a known read** —
the safe default for an MCP server nobody has classified. A read-only builtin
left unannotated would make every recall look like a delivery, and every turn
that only remembered something would pass the gate. So each builtin states
whether it is a read, whether it is idempotent, and whether it leaves the
process; `a2a_ask` is the only `OpenWorld` one, and deliberately not idempotent
— asking twice wakes a colleague twice and spends two of their turns.

None writes to a *shared* surface: a seat's own diary, skills and onboarding
marker are private state, and an ask is a message to one colleague.

## Not built here, and why

`load_tool_skill` is absent. It loads a knowledge-base-sourced prompt fragment
out of the tool-skills subsystem — `agent/skills/` plus its Confluence and Plane
sync workers — and none of that exists in the Go tree yet. Registering the tool
against a store that is not there would mean either a tool that always fails
(the thing this design explicitly refuses) or a stub pretending to be a feature.
It ships with the subsystem, in the phase that builds it.

`run_sandbox` is Phase 6 and belongs with the sandbox.
