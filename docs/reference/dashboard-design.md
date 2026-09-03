# Dashboard Design System

The dashboard is a React + TypeScript application, built by Vite from
`dashboard/` into `static/dashboard`, which the engine binary embeds. It talks
to the engine over ONE WebSocket: state arrives as pushes, and anything it needs
on demand is a query on the same socket (see
[API Endpoints § the live stream](api-endpoints.md#live-stream)).

This page documents the **visual system** and the **rules a change has to keep
holding**. What each screen answers is [Information architecture](#information-architecture);
how it is built and shipped is [How it is built](#how-it-is-built).

---

## The one rule

**Colour carries STATE, never IDENTITY.**

Everything else here follows from it. A hash of a name carries no information —
rename the seat and its colour changes, which is the proof that it never meant
anything — and an eight-family palette shared between "which agent", "which
phase" and "which event category" makes one amber mean three things at once on
one screen.

So colour is spent in exactly four places:

| | What it means | How many |
|---|---|---|
| **Status** | positive · caution · critical · info | 4, fixed |
| **Phase** | onboarding · execute · review | 3, fixed |
| **Accent** | *where the reader is* — the active nav row, the primary button, the focus ring, the on filter | 1 |
| **Data** | a chart series, inside a chart that carries a legend | 5 + a neutral residual |

Everything else — a seat, a unit, an event category, an integration, a node, a
tool origin — is **neutral**, and its identity is carried by its name, its icon
and its position. Those are stable, legible, and do not run out at eight.

A seat's chrome takes one of four **tones**, from what it is DOING:

| Tone | When | Drawn as |
|---|---|---|
| `working` | mid-turn, or waiting on a detached coding run | an info-blue rail |
| `needs` | parked on a question only a person can answer | a caution-amber rail |
| `broken` | the engine stopped it, or it reported an error | a critical-red rail |
| `quiet` | idle, offline, or a human seat | **nothing** |

`quiet` is deliberately untinted. An idle seat used to draw a tinted, glowing
tile that read as activity, and the fix for that is not a duller hue — it is
none. `needs` and `broken` are separate because a seat parked on a question and
a seat that fell over have both stopped, and only one of them is a failure.

---

## The palette is measured, not asserted

`dashboard/src/styles/tokens.css` is the one source of colour, type, space and
motion. Every claim it makes is recomputed from the shipped file by
`dashboard/src/styles/palette.test.ts`, in **both themes**, over **every
composited surface a token can land on** — including a hovered row inside a
nested panel, which is where a ramp anchored to the panel fill quietly falls
under its floor.

What is measured, and the floor each clears:

| Claim | Floor |
|---|---|
| `--text`, `--heading` on every surface | 7:1 |
| `--text-secondary`, `--text-muted` on every surface | 4.5:1 |
| `--text-faint` on a panel | **between 2.8 and 4.5** — it is decoration, and a step that crept up to 4.5 would invite itself into a table cell |
| every `-ink` step as TEXT on every surface | 4.5:1 |
| every fill step as a MARK on the page surfaces it sits on | 3:1 |
| `--text-on-fill` on `--accent` | 4.5:1 |
| the three phase hues, pairwise, under normal / protan / deutan vision | ΔE 10 |
| adjacent data hues, in series order, under all three | ΔE 9 |
| every data hue against the reserved `--critical` | ΔE 14 |
| the neutral ramp's chroma | ≤ 2.2 |
| the accent's chroma against every other hue | the highest |

**The fill/ink split is enforced by that measurement, not by convention.** An
`-ink` step is text; a fill step is a mark or a background. Mixing them is how
the screen this replaces shipped role badges at 1.63:1 — an inline
`background:` with a hue token in it and the default text colour left on top.
Only `--accent` is ever a fill behind text. A status is a soft tint carrying its
own ink step, never a solid block with a label on it, which is also what lets
the status fills be light enough to read as marks on a dark ground.

### The ground

Light is the base definition; dark is a token override, declared twice — once
under `prefers-color-scheme` and once under `[data-theme="dark"]` — so the OS
setting works AND an explicit choice wins in both directions. A test asserts
the two blocks agree, and that every colour has a value on bare `:root`: a
colour whose only definition is inside a media query is a colour that
disappears for somebody.

The dark ground is deliberately **not** `#000`. An operator reads this page for
hours, and pure black behind near-white text is the specific combination that
halates. It is also barely blue: the system this replaces tinted every neutral
with `rgba(176, 152, 255, …)`, so the whole product read violet and the accent
had nothing to separate itself from. A near-neutral ground with one saturated
accent is what makes the accent mean "here".

### Type

Two self-hosted variable faces — **Inter** and **JetBrains Mono**, the `latin`
and `latin-ext` subsets, 176 KB, embedded like every other asset. They replace
three families fetched from a CDN, which was the tree's ONLY external runtime
reference: on an air-gapped engine — a supported deployment — every face fell
back to a system font the design was never measured against.

Nine sizes, `--fs-3xs` … `--fs-3xl`, and **they are the only sizes in the
product**. The system this replaces had 194 `font-size` declarations across 14
literal pixel values, eight of them off any ramp, so its scale was fiction —
and so was its density control, which resized three of those steps and left the
rest.

There is ONE micro-label register (`.t-label`), declared once and never
overridden. Numbers are tabular everywhere: a live token count that changes
width as it counts makes the column beside it jitter.

### Space, radius, elevation

A 4px base scale, with `--density` multiplying the tokens that set row height
and padding — so compact mode is a real change to every surface. Four radii and
a pill (the previous system had ten literal radii across 44 declarations).
Three elevation steps: on the dark theme a panel is lifted by the light along
its top edge, because a shadow is invisible against near-black; on light the
shadow does the work. One recipe, two grounds, no second component.

---

## Information architecture

The sidebar is grouped by **what the reader is looking at**, in the order the
product's own story runs: the company, the work it is doing, the thinking behind
that work, what it costs, and the machine underneath. A founder opening this
meets their company first and the engine last.

| Group | Screen | Route | Answers, from |
|---|---|---|---|
| — | **Overview** | `#/` | what needs a person · what the company is doing · what it has cost. The snapshot's `agents` / `events` / `sandboxes` / `org` / `tokens` / `budget`, plus the `stream` query |
| **Company** | People | `#/people?group=&q=` | every seat and what it is doing — grouped by state, by unit, or flat |
| | Org chart | `#/org?lens=chart\|directory\|charter` | the hierarchy, the directory, and the company's own mission, vision and policies |
| | *a seat* | `#/seats/{handle}?tab=` | overview · model activity · memory · cost · access |
| **Work** | Coding runs | `#/runs?run=` | the live `sandboxes` plus the durable `sandbox_runs` — including runs whose box has been reclaimed |
| | Agent-to-agent | `#/conversations` | `a2a_channels` — who asked whom, how many messages, and when |
| | Schedules | `#/schedules` | `schedules` — what fires, when it next fires, how it last went |
| **Intelligence** | Model activity | `#/model?role=&phase=&failed=` | `phases` — one row per phase across the fleet; the transcript is on the seat |
| | Event log | `#/activity?category=&actor=&q=&failed=` | the live feed, then `events` for older pages |
| | Knowledge | `#/knowledge?q=` | `knowledge` — the company's own live search |
| **Cost** | Spend & budgets | `#/spend?window=` | the pushed spend rollup, the `tokens` query for other windows, and `budgets` |
| **Operations** | Fleet | `#/fleet` | `fleet` — the lease table |
| | Integrations | `#/integrations` | `integrations` |
| | Tools | `#/tools?q=&origin=` | the pushed tool catalogue |
| | Configuration | `#/config?lens=&revision=` | `config` / `config_audit` / `config_diff` *(operator-gated)* |
| | Secrets | `#/secrets` | the names and provenance the fleet holds — **never a value** *(operator-gated)* |
| — | Trace | `#/traces/{id}` | `trace` — reached from a row or from search |
| — | Turn | `#/turns/{id}` | `turn` — everything one unit of work published |
| — | Event | `#/events/{id}` | `event` |
| — | Engine | the pill in the sidebar footer | the `health` push plus the `stream` query |

**Old routes redirect, with their query strings intact.** `#/agents`,
`#/agents/{id}`, `#/work`, `#/tokens`, `#/events`, `#/company`, `#/audit` and
`#/org?lens=seats` all resolve to their new homes. Those links are in bookmarks
and in chat threads; a redirect costs one navigation, a dead link costs the
reader the thing they were looking for.

### Moving, and going back

Every screen, section and filter is in the URL, so a view can be refreshed,
bookmarked and handed to somebody else as a link. What takes deciding is the
**session stack** — which navigations leave an entry behind — because that, not
the URL, is what the Back button reads.

| Move | Stack | Why |
|---|---|---|
| a **moved** path | replaces | the entry names a route that no longer exists; leaving it means Back lands on it, it redirects forward, and you arrive where you started |
| a **section** — a lens, a tab | pushes | the reader called these screens; Back after three of them should walk out through them |
| a **filter** — chips, sort, a search box | replaces | four ticked chips are ONE screen; Back means "off this list", not "untick one" |

The line between the last two is whether the reader would call it a different
screen, and it is the only judgement call in the router.

**Scroll is a property of a history entry, not of a URL.** The same screen
reached twice is two places the reader has been, and keying a position by URL
collapses them onto one. Each navigation stamps a key into `history.state` and
files the outgoing position under it; an entry with NO key is exactly the test
for "somewhere new", which is the only case that starts at the top. A restored
position is re-applied for a short window while the rows arrive — a scroll is
clamped to the height that exists, so one attempt lands short — and abandoned
the moment the reader touches the page.

All three of these shipped wrong once, and none of them is visible in a URL.

### The attention queue

`dashboard/src/lib/attention.ts` is one list because it is one question, and it
is the question an operator opens the page with. Every one of these conditions
was already known to the dashboard and each lived in a different screen:

| Condition | Where it used to live |
|---|---|
| a coding run parked on a question | a badge — and a run whose box had been reclaimed appeared **nowhere at all** |
| a seat the engine stopped | a card among the healthy ones |
| a budget refusing charges | a bar on one seat's page |
| an engine with **no active configuration**, dropping every inbound webhook | a line in a popover |
| a live round that has not moved in minutes | nowhere — the row animated either way |

Ordered by what it costs to ignore, then newest first inside a severity. Every
row says what happened AND what it costs to leave it, and carries a link to
where the answer is.

---

## The transcript is stable, and reads in order

The sharpest complaint about the screen this replaces was that the LLM calls
jumped around, were hard to follow, and did not say much worth reading. Ten
rules fix it, and each one names a specific mechanism:

1. **One identity.** A phase is keyed `turn_id|phase|iteration`, live and
   finished alike. They used to differ — the live row was keyed
   `live|turn|phase|iteration` and the stored one carried a timestamp — so the
   instant a phase completed its row was REMOVED and a different one inserted:
   the entrance animation replayed, the row relocated from the end of the list
   into its chronological slot, and its expanded state was lost with the key it
   was filed under. Now a live phase *becomes* a finished phase in place.

   **A phase has to HAVE a finished half for that to mean anything**, and for a
   while it did not. A screen reads two sources — the seat overlay's `live_call`
   and a query answered ONCE, at mount — and the projection clears `live_call`
   the instant a phase completes. Nothing delivered the durable record to a tab
   already open, so a turn watched to its end did not become finished: it
   *disappeared*, most completely on a seat's first turn, where the mount-time
   history is empty and the page was left saying the seat had never run at all.
   The record was on the wire the whole time — the `event` push carries the
   whole `agent_phase_completed` envelope, payload included, and is sent BEFORE
   the overlay that clears the call — so the store keeps the recent ones
   (`MAX_PHASES`) and every screen merges them over its own query answer through
   the same `fromPhaseEvent` the stored half uses. Same function, same key, so
   the streamed record and the one the query would return next time are the same
   row.

   That buffer is bounded on the retained PAYLOADS, not on a row count: a phase
   carries its verbatim system prompt, its response and every tool result, which
   is why the server itself caps one page of these at 60. It is company-wide and
   drop-oldest, so it is a supplement rather than a guarantee — a fleet busy
   enough to evict a record a tab still wants renders that turn with a phase
   missing, and the reload that supersedes it is authoritative.
2. **One block per round: thought, speech, then calls.** A round groups
   `round_narration[]` and `tool_executions[]` on the `round` they share,
   and rounds only ever append — so nothing above an insertion point can
   move. The previous surface distributed badges across inter-paragraph
   slots with `floor(j × slots / tools.length)`; both the divisor and the
   slot count grow every round, so every earlier badge was re-placed each
   time a new tool ran. `round` was on the wire the whole time and never
   read.

   The model's *words* had the same problem one level up. `response` is the
   JOIN of every round's turn, and a join cannot be undone — its parts are
   separated by a blank line and prose contains blank lines — so splitting
   it on the leading `<think>` tag showed round 1's thinking as "the
   reasoning" and every later round's thinking as "the model output", tags
   and all. The engine now sends the split it already knows, at the point
   the round's assistant message is appended. A phase recorded before that
   has only the joined string and is shown whole rather than guessed apart.
3. **The model's words are prose; JSON is monospace.** Reasoning and speech
   get a proportional face, real leading and a bounded measure. Monospace
   stays where it carries meaning — tool arguments and tool results.
4. **Grouping is structural; colour is semantic.** Rounds are separated by a
   numbered rail and a two-step alternating tint, not by a hue apiece: a
   colour per round would read as meaning something and mean nothing, which
   is the same objection as a colour per agent and worse at nine rounds. A
   round's node takes colour for exactly three states — normal, contains a
   failed call, in flight — because "which round went wrong" and "where is
   it now" are the two questions a reader brings to a running turn.
5. **A running phase tails; a finished one flows.** While live the ledger is
   bounded and follows the newest round, but only while the reader is
   already at the bottom — following regardless yanks them off whatever
   they stopped to read.

   The engine STREAMS: a round's text arrives while the model is writing
   it, coalesced to five frames a second, and the round in flight rides
   `partial_round` on the live-only progress event. It is never merged
   into `round_narration` — arriving text and committed text are different
   facts — and the moment the round commits, its narration replaces the
   fragment. An endpoint that cannot stream is negotiated down to the
   unary call once per process, so nothing regresses; the text then
   appears a round at a time, as it used to.

   A provider that dies mid-answer keeps what it wrote, dimmed and
   labelled, with the retry below it. Erasing text somebody has already
   read looks like a glitch, and "this model wrote four hundred characters
   and then died" is the useful fact about a flaky provider.
6. **A round's number is the PHASE's, not one loop invocation's.** The tool
   loop counts from 1 each time it is entered, and an extended phase enters
   it again — so an unshifted second invocation made the phase read as
   running backwards. The live projection drops a round numbered below the
   one it holds, so the whole extension vanished from the screen; the ledger
   merged extension round 1 into original round 1; and the completed record,
   which assigned the last invocation wholesale, lost every tool call and
   every round of narration from before the extension — on exactly the long,
   hard phases that get extended.
7. **A discarded round changes nothing.** The seat's state used to be set
   before the two guards that decide whether to keep the round, so a
   straggler arriving after its own phase completed flipped the seat to
   "working" and was then thrown away — with nothing pushed to correct it,
   the seat sat rendering as busy with no call to show. A round about to be
   discarded must not move the seat either.
8. **A phase start does not blank a call its own first round already
   seeded.** `agent_phase_started` and `agent_turn_progress` travel on
   different subjects, so the opening round can land first; the seed was
   unconditional and replaced a call that already had a model, a response
   and tool calls with an empty placeholder.
9. **Open/closed is latched.** Once a reader opens a phase or a turn it stays
   open. The previous surface derived it — open while live, closed once
   finished — so a transcript vanished at exactly the moment it became
   complete, and a new failure elsewhere silently re-opened a different card
   and shoved everything below it down the page.
10. **Time is compared as an instant.** Go's `RFC3339Nano` trims trailing
   zeros, so `…:07Z` sorts *after* `…:07.42Z` on a raw string compare — it
   compares `'Z'` (0x5A) against `'.'` (0x2E). Every list sorts through
   `tsKey`, and every comparator is three-way: one returning −1 for equal
   operands makes equal rows trade places on each render.

## A monitor is not a reader

`#/model` showed every running phase as an expanded transcript. With one agent
that was pleasant. With seven it was a race: seven seats each republishing
streamed prose five times a second, in cards that grew as they wrote, so
nothing held still long enough to read. Adding streaming made a bad shape
worse rather than causing it.

**Reading what a model said is a one-agent activity.** It needs one turn in
focus, and it belongs on that agent's own page. A fleet screen has the
opposite job — *which seats are working, which are stuck, what is this
costing* — and that job is rows.

So the screen is a table. One fixed-height row per phase, the same shape
running or finished, sorted on the seat handle, which does not move. A live
row updates its cells — rounds, tokens, elapsed — and **nothing reflows**,
because a number changing inside a row of settled height cannot move the
layout around it. That single property is why the table holds at fifty seats
where a list of cards did not hold at seven. The transcript is one click away,
on the seat.

The rules below still apply to the seat screen, where the transcript now
lives.

## A live screen that stays readable

The first read of "crowded" was density; the second was MOTION. Rows arrived
while somebody was reading one, and splicing a phase in at the top pushes
everything below it down by a card — mid-sentence, every few seconds on a busy
company.

Three rules, the first two of which are the same rule the round ledger already
follows — **the page moves only when the reader is not reading** — and the third
of which is what makes them worth having at all:

- **Running and settled are different lists.** A live phase changes every
  couple of hundred milliseconds; a finished one never changes again.
  Rendering them as one list let the churn of the first reflow the second.
  The live region is bounded and visibly bordered, which is the promise that
  motion stops at that edge.
- **The settled list does not splice rows in under a reader.** At the top of
  the scroller new rows merge straight in — that is somebody watching the
  feed, and holding rows back from them would look broken. Scrolled down,
  they are counted and offered: *"3 new turns finished while you were
  reading — show"*. Keyed on identity, never position, so a row that UPDATES
  in place — a round landing, a phase completing — is never held back.

  **And identity reaches across the two lists.** A running turn lives in the
  live region; when it finishes it leaves that region and arrives in the
  settled list with a key that list has never admitted — so the row a reader
  had been watching for four minutes was replaced by *"1 new turn finished
  while you were reading"*. It was on their screen a moment earlier: it is not
  new to them, whatever list it was in. Each screen passes the keys it is
  rendering live, and they are admitted without the scroll check.

- **The live half of a screen does not wait on the stored half.** The seat's
  Model activity tab wrapped its turns in the query-state component, which
  renders nothing while a query is in flight and a banner *instead of* its
  children when one fails. So a turn happening right now was invisible until
  the event store answered — and invisible for good on a node that keeps no
  event log, where the answer is a permanent `no_event_store`. The query's
  state renders beside the turns now, never in place of them.

The seat screen makes the same split, where it answers a second question:
which of these turns is happening right now, readable at a glance from the
accent ring rather than only by finding a badge.

## One filter bar, and a control that means what it looks like

The Model screen collected eighteen controls in one sticky row — a segmented
control, a free-text box, a chip per seat, a chip per phase and a failures
chip — fifteen of them near-identical pills in two different active idioms.
Above them sat four `--fs-2xl` numerals, a step LARGER than the screen title,
so the loudest thing on a transcript page was a token count. Below the list
sat a copy of Spend's own panel, which every "load older" click pushed
another sixty cards further down a single scroller.

What replaced it:

- **The counts moved into the header badges**, and the failure count became
  the control that filters to failures. It used to be an inert tile reading
  "4 failed" beside an unrelated chip that did the filtering, so a reader who
  saw the number had to go find the pill that acted on it. `Badge` renders as
  a real `<button>` with `aria-pressed` when given an action — a `<span>`
  with a click handler is neither focusable nor announced, and looks
  identical to the inert badges next to it.
- **The seat filter became a picker.** It was a text box, but the match is
  exact on both sides of the wire — the server query and the in-memory
  filter both compare for equality — so typing a prefix returned nothing
  while looking exactly like a search that found no matches. `Select` is a
  native `<select>`: keyboard navigation, type-ahead and the platform's own
  overlay come free, and a hand-built listbox would have to earn all three
  back. It also scales past the ten seats at which the chip row silently
  disappeared.
- **The spend panel is a link.** It was Spend's panel on Spend's data at
  Spend's window; the screens are split by question, and duplicating one
  screen's answer at the bottom of another is how the two come to disagree.


The header carries the same facts in the same order whether a phase is live or
finished — phase, decision, model, rounds, tokens, age — so the row does not
change shape when it completes. `decision`, `exhausted_rounds`, `rescue_fired`,
`notes`, `tools_available` and `conversation_key` are all rendered; every one of
them was on the wire and shown nowhere.

**Nothing animates on a data push.** A list that re-flows every time a
tool-loop round lands is a list nobody can read while it is running, and
`agents` is pushed twice per round.

---

## Honest empty states

A screen that renders a blank where data would go is a screen that cannot be
trusted when it IS blank. Three distinctions the product makes everywhere:

- **Nothing happened** vs **nothing could be read.** "No events" on a fresh
  company and "no events" on a node with no event log are the same empty list
  and completely different problems. `QueryState` renders the engine's own code
  — `no_event_store`, `unauthorized`, `unknown_query`, `timeout` — as a
  sentence saying which.
- **Zero** vs **unknown.** The integrations answer's counts are three-valued,
  and a node not serving ingress reports `unknown`, not `0`. The budgets answer
  says `durable: false` when the counter could not be READ.
- **Not configured** vs **empty.** A knowledge search with no backend says so;
  a company with no seats says roles come from the configuration.

Every empty state names what would fill it.

---

## How it is built

```
dashboard/                  the source — React 19 + TypeScript, built by Vite
  src/protocol/             the wire, typed. NO React, NO DOM at module scope
  src/app/                  shell, hash router, IA, command palette
  src/lib/                  store bindings, one clock, formatting, derivations
  src/ui/                   the component library and the chart kit
  src/routes/               one file per screen
  src/styles/               tokens, base, components, shell, screens
static/dashboard/           THE BUILD OUTPUT — committed, and what the binary embeds
```

**The build output is committed**, and that is deliberate: `go build ./...` and
`go install …@latest` must work on a clean checkout with no Node on the machine,
and an embed directive cannot run a bundler. A stale bundle would compile,
embed, serve and pass every Go test while running code nobody wrote — so CI
rebuilds it and diffs the tree (`make dashboard-check`), the same idiom as
`go mod tidy -diff` and the generated `schema/`.

| To | Run |
|---|---|
| change the dashboard | `make dashboard` — then commit `static/dashboard` with your source change |
| develop against a running engine | `make dashboard-dev` (proxies to `localhost:8000`) |
| run its suites | `make dashboard-test` |
| check the committed bundle is current | `make dashboard-check` |

`static/dashboard/protocol.js` is a **second** build target: the protocol layer
alone, unminified, importable by plain `node`. `internal/e2e/golden_test.go`
runs a real company, captures every frame its socket pushed, and replays those
bytes through it — so the gate asks "does the client understand what the server
sent", not "did the server send something". That is the gate that caught a full
turn's worth of `agents` pushes being sent as an object keyed by role while the
client guarded on `Array.isArray`: both sides' own suites passed and the seat
rendered idle from the first phase to the last.

### What the client half guarantees

- **The store derives nothing.** The server computes the projection once and
  pushes the result. This layer once held a second implementation of the
  engine's state machine — an event→state map, sandbox lifecycle tracking and
  an 85-line reimplementation of the token aggregation — and three copies of
  that logic meant a refresh routinely disagreed with what had been on screen a
  moment earlier.
- **Subscriptions are per-slice.** `agents` is pushed twice per tool-loop
  round; a store that woke every listener on every envelope would re-render the
  application several times a second for the length of a turn.
- **A query WAITS for the socket rather than failing.** Screens issue their
  first query as the page boots, so rejecting when not-yet-connected made every
  deep link render "could not load" and stay there. Queries are pure reads, so
  one in flight when the socket drops is re-sent on reconnect.
- **A refused credential is diagnosed over HTTP.** A handshake the engine
  answers 401 never reaches the page as `close(1008)` — a connection that never
  opened has no frames, so the browser reports 1006, the same code it gives for
  an engine that is simply down. A plain `GET /ws/stream` runs the same guard
  and stops one line short of the upgrade: 401 is a refused credential, 426
  means it was accepted.
- **One clock.** Every relative time on screen advances together and none of
  them is baked at render.

---

## Rules a change has to keep

1. **Colour is state, never identity.** No hash-to-hue, no per-agent tint, no
   per-category chip colour. If you need to tell two things apart, use their
   names.
2. **No new colour, size, radius or spacing literal.** If a component needs
   one, the TOKEN is what gets added.
3. **A fill step is never text and an `-ink` step is never a background.** The
   palette suite measures both; an inline `background:` carrying a hue token is
   the specific mistake it exists to catch.
4. **Nothing animates on a data push.** Entrances and control states only.
5. **Every list sorts through `tsKey` with a three-way comparator**, and every
   keyed row uses an identity that survives the row's own lifecycle.
6. **Every empty state says why it is empty** and what would fill it, and
   distinguishes "nothing happened" from "nothing could be read".
7. **Every screen, section and filter is in the URL**, and obeys the
   push/replace table above.
8. **A screen subscribes to the slices it reads and no others.**
9. **Numbers are tabular**, and an absent number is an em dash rather than a
   zero — zero is a measurement.
10. **Run `make dashboard` and commit `static/dashboard` with the change.** CI
    diffs it; a bundle that has drifted from its source is a red build.
