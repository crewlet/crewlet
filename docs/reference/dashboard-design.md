# Dashboard Design System

The dashboard is a zero-build, modular ES-module app (see
[API Endpoints § the dashboard](api-endpoints.md#live-stream)). This page
documents its **visual system** — the tokens every component reads, the panel
recipe, and the rules a change has to keep holding.

The system is the one the Crewlet marketing site ships. The ground is pure
black and every division on it — panel fill, hairline, inset — is a different
alpha of the *same* warm cream. That single material is what makes a dense
operational surface read as one object rather than as a stack of grey boxes,
and it is the rule to keep: a new surface is another step of the ramp, never a
new colour.

---

## Screens

The sidebar is a flat list with `Company` as the one collapsible group. Every
entry resolves to a view backed by a real endpoint — nothing is rendered as a
placeholder or a coming-soon stub.

| Nav | Route | Reads |
|---|---|---|
| Dashboard | `#/dashboard` | the snapshot's `agents` / `events` / `sandboxes` / `org` / `tools` / `tokens` |
| Company → Overview | `#/company` | `/org` |
| Company → People Directory | `#/people` | `/org` + live agent state |
| Company → Org Chart | `#/org` | `/org` + live agent state |
| Company → Audit log | `#/audit` | `/config/audit` *(auth-gated)* |
| Agents | `#/agents` | `/org` + live agent state |
| Activity | `#/events` | the snapshot's `events`, then live `event` pushes, then the `events` query for stored history |
| Engine health | the dot in the brand | the pushed `health` envelope + the `stream` query — a popover, not a screen (see [Health](#health)) |
| Trace | `#/traces/{trace_id}` | the `trace` query — reached from a row, never from the nav |
| Tokens | `#/tokens` | the pushed spend rollup; a `tokens` query for any other window |
| Tools | `#/tools` | `/tools` |
| Schedules | `#/schedules` | `/schedules` |
| Fleet | `#/fleet` | `/fleet` (the lease table) |
| Configuration | `#/config` | `/config` *(auth-gated, secrets redacted server-side)* |

`js/org.js` is where the `/org` tree is flattened into **seats** — every role
with its unit chain, effective unit lead, its configured `token_budget`, and
the MCP surfaces it inherits. Views consume seats, never the raw payload, so
lead inheritance and `mcp_env` inheritance are resolved once.

### The overview

The Dashboard reads, top to bottom, in order of urgency:

| Band | Answers |
|---|---|
| **Company pulse** (the lead panel) | Is anything happening, and did anything break? |
| **In flight** | What is running right now, how far through its turn, and how long since it last moved |
| **The team** | Who is on the roster and what each seat is doing |
| **Running sandboxes** | Which detached coding jobs are open, and which are blocked on an answer |
| **Engine activity** | What just happened |

### The pulse

`js/pulse.js` — one row per agent seat, one cell per minute of the last hour,
lit by what that seat actually did. It is the site's masked dot field applied
literally rather than decoratively: a cell is lit because a seat was working
in that minute, its brightness is that minute's event count against the
busiest cell on the grid, and a red cell is a real failure.

| Element | Source |
|---|---|
| Rows | `flattenSeats(org)`, agents only — a seat that has done nothing still gets a row, which is itself a finding |
| Cell brightness | Feed events for that actor in that minute, over the grid's busiest cell |
| Red cell | The feed row's `failed` flag (see [Failure](#failure)) |
| Breathing cell | The current minute of a seat that is working *now* — the only cell on the grid that moves |
| Right-hand figure | The seat's spend from the pushed rollup, or its failure count |

`buildPulse` is a pure function over data the page already holds, so the panel
costs no request and no server work. One bucketing pass per render is threaded
through to the hero grid *and* to every seat card's strip, so a card and the
hero can never tell different stories about the same seat.

**The panel claims only what the feed can speak for.** The projection retains
a bounded number of events (`MAX_EVENTS` in `js/store.js`, matching the
server's `EVENT_FEED_LIMIT`), and a busy org fills that in minutes — so on
such an org the older part of the hour has *no record*, which is not the same
as no activity. Cells past the retention edge render as a hairline marked "no
record", the axis says so, and the headline counts the minutes actually
covered. Drawing the gap as idle would make a company that had been flat out
all hour look like one that woke up five minutes ago.

The strip on a seat card is deliberately the **same device** at a smaller
size, not a second chart type.

### Reading history

The live feed is a bounded ring (`MAX_EVENTS`), which a busy org fills in
minutes. Activity pages beneath it into the event store with the `events`
query, and three rules keep the merged list honest:

- **One ordering key**, `(instant, id)` descending — `newestFirst` in
  `format.js`, mirroring `timescaledb/_time.py`. Raw ISO strings are not
  safely comparable (the API emits naive and aware forms for the same
  instant) and a shared timestamp needs the id to break it. A row's
  position depends only on its own key, so nothing jumps when a page lands.
- **Dedupe by id, live wins.** A page can arrive twice (queries are
  re-sent across a reconnect) and legitimately overlaps the live window.
  The live copy carries `payload` and `topic`, which store rows do not.
- **The cursor is recomputed from the merged set**, never kept as a
  running variable. The live ring evicts as it grows, so a row can leave
  the live window while still held in the fetched half — deriving the
  cursor from one half opens a hole that nothing reports.

The pager's two jobs must not look alike: revealing rows already in
memory is instant, reading the store is a network trip that can fail,
run out, or find no store at all. Each says which it is.

### Traces

`#/traces/{trace_id}` is a turn's whole story — the notification that
woke the agent, each phase, the tools, the completion — oldest first,
indented by span. It is reached from the event-detail Trace row, from
Activity's trace chip, and it grows live while its turn is still running.

It has **no nav entry**, deliberately: a trace view with no trace to show
is exactly the placeholder screen rule 6 forbids.

`traceNodes.js` owns the node markup, the span arrangement, and the
"is this worth opening" test, because three surfaces render the same
thing and had begun to render it three ways.

### Health

The whole health surface used to be one 7px dot with three colours. Everything
behind it had been on the wire the entire time and reached no pixel: whether a
company configuration is even active, whether the event store is durable or
will evaporate on the next restart, whether the activity feed was seeded from
history, how many envelopes this tab has silently dropped.

`js/health.js` renders it in a **popover on the dot** (`#health-pop`, a child
of `<body>` — the sidebar is `position: sticky`, which always creates a
stacking context, so a popover inside it paints under `.main` whatever
z-index it carries). A health screen would be backed by real data, so it would
not literally break the no-placeholder-screens rule — but it would break its
purpose. Health has to be seen when you were *not* looking for it, and a
screen you navigate to is one you open after you already suspect a problem.

Three conditions escalate out of the popover into always-on chrome, because
they must never wait for a click:

| Condition | Chrome |
|---|---|
| The socket is down | Red dot, red banner naming how old the state on screen is |
| No company configuration is active | Amber dot, amber banner — the engine is running and **discarding every inbound webhook** |
| The engine refused this browser's API token | Amber banner **plus a button**, the only affordance the banner carries |

The last one is checked before "the socket is down", and has to be: a refused
handshake leaves the socket down, so the outage wording is literally true and
completely misleading — "retrying" describes a wait that ends by itself, and
this one ends only when somebody supplies a token. It is the only degraded
state that resolves for nobody, which is why it is the only one with a button:
the prompt fires once per rejection on purpose (reconnect backs off to 30 s, and
a modal that reopens on every attempt is worse than the problem), so dismissing
it would otherwise leave a permanent notice with no way to act on it. Reads are
open by default, so most deployments never see any of this; it is what
`api.auth.allow_anonymous_read: false` looks like from the browser.

The middle one is what this surface exists for. An unconfigured engine used to
render identically to a correctly-configured idle one: green dot, `status: ok`,
and every list empty. It now says so in the banner, in the popover, in the
overview's headline, and in every empty list — `emptyOrPending` in `js/ui.js`
is where those three cases (socket down / nothing configured / this list is
genuinely empty) are told apart once, instead of each view collapsing them
into two.

Nothing in the popover asserts a fact it cannot currently see. Losing the
socket **clears** the health slice — a five-second-old tick is not evidence
about now — so every field arrives `undefined`, and a plain
`=== false ? bad : good` would render "Configuration: active" on a page that
cannot reach the engine at all. Booleans from the engine are read three-valued,
and the dot's status→class mapping is a lookup table rather than a ternary
chain for the same reason: the chain's final `else` sent every unrecognised
status to green, so a state added on the server would have *arrived* on screen
as healthy.

Per-socket facts (dropped envelopes, queue depth, tabs connected) come from
the `stream` query, not the shared tick — the tick encodes one JSON string for
every client by design, so a per-client field would force one encode per client
per tick. A tab that has dropped envelopes is offered a reconnect, because a
dropped envelope is gone: the server discards the *oldest* queued frame and
never re-sends it, so a fresh handshake snapshot is the only repair.

### The turn rail

`turnRail()` in `js/ui.js` draws an in-flight turn as an object: the canonical
phases in order (plan → execute → review), the ones already spent filled in
their own hue, the current one lit and breathing, the rest hollow, with a
packet travelling the segment feeding the live phase. A phase running off that
path — onboarding, a judge, a sub-agent — renders as its own single-node rail
rather than being placed on a path it is not on.

### The seat card

`js/cards.js` renders one card per seat, used by both the overview's seat row
and the Agents index. Human seats (`kind: human`) appear alongside agents —
they hold seats in the same org chart — and are marked, never given a fake
runtime. Everything on a card is real:

| Element | Source |
|---|---|
| Identity hue on the name and the leading hairline | `roleColor` / `roleInk`, hashed from the seat name |
| State dot | the live projection (`effectiveAgentState`), or `human` |
| Marker | the live phase when the seat is working, otherwise its state badge; a human seat gets its unit |
| Status line | `statusLine` in `state.js` — derived from live state only |
| Activity strip | this seat's row from `buildPulse` — the same hour the pulse grid shows |
| Budget bar | the engine's live token meter against the cap that meter enforces |
| Integration chips | the seat's own + inherited `mcp_env` server keys; for a human seat, the `contact` identities it is reachable at |
| Cost line | 24-hour spend, and when the seat was last active |

`statusLine` never invents activity. An agent with nothing in flight says so;
an AFK agent shows its engine-detected cause; a seat running a detached
sandbox says it is writing code, and says when it is blocked on an answer.

### The budget bar

The bar is the **only** place the dashboard divides one token figure by
another, and it is honest for exactly one reason: both figures cover the same
span. A seat card carries three token numbers with three different spans — a
24-hour spend rollup, a 7-day per-agent total, and the engine's
process-lifetime meter — and only the meter shares a span with the cap that
constrains it. Dividing either of the others produces a percentage wrong by
however long the engine has been up, and it looks entirely plausible.

Three states, none of which invents a number:

| State | Rendered as |
|---|---|
| Metered — a live meter and a cap | a bar, `used / max`, labelled `this run` |
| Capped but unmetered — no engine reporting, or none yet | the cap, stated, marked `no live meter` |
| No cap | nothing at all |

**Exhaustion is a state, not a ratio.** `TokenBudget.consume` refuses a charge
that would exceed the cap and increments nothing, so a seat charged in
3k-token rounds against a 100k cap stalls at ~99k and can never reach its own
maximum. A bar keyed on `used >= max` shows a permanently-blocked seat at 97%
and calls it healthy. The engine records *when the cap turned a charge away*,
and that is what the bar reads.

---

Everything lives in four stylesheets, loaded in this order:

| File | Owns |
|---|---|
| `styles/tokens.css` | Every colour, radius, easing, and type-stack value. Nothing else defines a colour. |
| `styles/base.css` | Reset, app shell, sidebar, frosted topbar, motion, icon primitive |
| `styles/components.css` | The panel recipe, badges, rows, tables, buttons, trace tree, skeletons |
| `styles/views.css` | Per-view internals only — panels get their surface from the recipe |

---

## Tokens

Themes are `[data-theme="dark"]` (the default) and `[data-theme="light"]` on
`<html>`. A theme is *only* a different set of values in `tokens.css`; no
component branches on the theme.

### Surfaces, borders, text

| Token | Job |
|---|---|
| `--bg` / `--bg-sidebar` | Page tone; the nav rail one step off it |
| `--bg-card` | Panel fill — a warm cream **alpha** over the ground in dark, so nested panels accumulate depth without another token |
| `--bg-card-2` | Raised inner surface (table headers, chips) |
| `--bg-hover` / `--bg-active` | Row and control states |
| `--bg-inset` | Recessed surface (code blocks, expanded bodies) |
| `--border-subtle` / `--border` / `--border-strong` | Hairline ramp |
| `--text` / `--text-secondary` / `--text-muted` / `--text-dim` | Ink ramp |
| `--heading` | Headings, row titles, headline numbers |

### Accent and glass

`--accent` is a **pale fill**, not a text colour — its readable partner is
`--accent-ink`, and `--accent-soft` is the wash used behind counts and chips.
`--glass` / `--glass-hover` back the topbar, icon buttons, and segmented
controls, always with `backdrop-filter: blur(…)`.

### Brand

`--brand-gradient` is the full seven-stop mark; `--brand-ramp` is its
three-stop cousin for anything only a few pixels tall. Both are used as
**light, never as a fill** — a hairline along the overview panel's top edge,
the packet travelling the turn rail. There is no third place for them.

### Elevation and motion

`--panel-shadow` (the settle), `--card-shadow` (floating chrome: toasts,
the mobile drawer), `--lift-shadow` (hover). A drop shadow is invisible on
pure black, so in the dark theme a panel is lifted by *light* instead: a
one-pixel warm highlight along its top edge, the way a physical panel catches
the light above it. Easings are `--ease` and `--ease-snappy`; durations
`--dur` / `--dur-slow`.

### Categorical hues

Eight hue families, each shipping two steps:

- `--<hue>` — the **mark** step: bars, dots, stack segments, heat fills
- `--<hue>-ink` — the **text** step: badge labels, links, tinted type

Soft fills are derived at the use site with
`color-mix(in srgb, var(--<hue>) 10%, transparent)`.

`--red` / `--red-ink` is a **reserved status hue** (failed, error,
destructive) and is never used as a categorical slot.

**The rule for text.** Any label that carries a hue uses the `-ink` step. The
mark step is tuned for a mark on a surface (≥ 3:1), not for 11px type. Setting
a mark step as `color` on text is the one mistake this split exists to prevent.

---

## Where the hues are assigned

Two orders render hues side by side, so those are the orders that have to hold
apart:

| Order | Source | Assignment |
|---|---|---|
| Phases | `PHASE_ORDER` in `js/state.js`, used by the stacked phase bar and its legend | onboarding → green, plan → blue, execute → amber, review → purple, auxiliary/subagent → orange, judge → cyan, turn → neutral |
| Event categories | `EVENT_CATEGORIES` in `js/state.js`, used by the activity filter row and category tags | lifecycle → blue, task → green, communication → purple, decision → amber, knowledge → pink, learning → purple, a2a → orange, notification → cyan, webhook → brown, system → neutral |

`PHASE_HUE` in `state.js` is the single source of truth for phase colour;
`phaseColor()` returns the mark step and `phaseInk()` the text step. There is
no second hardcoded copy — the agent × stage heatmap mixes its fill from
`phaseColor()` so it re-steps with the theme instead of being frozen at one
theme's values.

The assignments are not free choices. They are the ones under which every
adjacent pair in **both** orders clears, in **both** themes:

| Check | Gate | Light | Dark |
|---|---|---|---|
| Normal-vision separation (worst adjacent pair, OKLab ΔE ×100) | ≥ 15 | 17.9 | 17.8 |
| Protan / deutan separation | ≥ 8 | 15.8 | 15.6 |
| Mark contrast vs the surface | ≥ 3:1 | 3.29 | 3.70 |

The hue *families* (angle and chroma) come from the marketing site's brand
tokens; only lightness is re-stepped per theme, because the site's own steps
are tuned for occasional accent use on a marketing page rather than a dense
operational surface where six of them stack edge to edge.

**If you change a hue, re-check both orders in both themes.** Changing one
value can break a pair three slots away.

Colour is never the only carrier of identity: every pill, legend entry, and
badge renders its name, and every heat cell prints its value.

---

## The panel

One recipe, shared by `.panel`, `.card`, `.list`, `.stat`, `.tool-card`,
`.turn`, and `.mem-card`:

```css
background: var(--bg-card);
border: 1px solid var(--border);
border-radius: var(--radius);          /* 14px */
box-shadow: var(--panel-shadow);
```

`--bg-card` is used neat: it is *itself* an alpha over the ground in the dark
theme, and diluting it a second time leaves a panel under the threshold where
its edge reads at all.

An actionable panel adds `.clickable`, which swaps in `--lift-shadow` and
`translateY(-2px)` on hover. `.dot-texture` adds the site's decorative accent
dots — used on the overview's lead panel only.

Views must not re-declare a panel's fill, border, radius, or elevation. If a
view needs a new surface, it gets the recipe, not a copy of it.

## Type

Three faces and three tracking values, all tokens — so the brand's voice
survives even where none of the faces resolve.

- **Body** — Inter (`--font-sans`) at `--track-body` (`-0.011em`). The
  negative tracking is what makes the text read as the brand rather than as a
  default UI font.
- **Display** — Inter Tight (`--font-display`) at `--track-display`
  (`-0.024em`), in `--heading`. Anything that reads as a *title* wears it:
  headings, the overview's headline figure, panel figures, the `.display`
  class. Where neither face is installed both fall back to the same system
  face and the tracking alone still separates them, which is why tracking is a
  token rather than baked into the font choice.
- **Micro-labels** — JetBrains Mono, uppercase, `--track-label` (`0.12em`),
  in `--text-muted`. This is the site's eyebrow, and it is what section
  headers (`.sec-title`), table headers (`.tbl th`), block labels
  (`.block-label`), stat and strip labels, and status badges all wear. The
  shared `.eyebrow` class in `base.css` is the standalone form.

`index.html` requests all three from the font CDN with `display=swap`, and
every token names a full system fallback: an engine on a closed network must
render immediately in the fallback rather than block on a request that will
never answer.
- **Numbers** — `font-variant-numeric: tabular-nums` everywhere they can
  change, so a live value does not jitter its column.

Status badges are mono/uppercase because they are codes. A badge carrying
prose (a task title) adds `.text`, which returns it to the sans face and
sentence case.

## Rendering

Views are pure: `render(state)` returns markup and touches no DOM. The
shell patches that markup into `#view` with a keyed patcher
(`js/patch.js`) on the next animation frame, so a re-render updates only
what changed. A view also declares the store slices it reads, so a health
tick wakes the header alone and a streaming round wakes only the views
showing agents.

This is load-bearing, not an optimisation. Rendering with
`innerHTML = markup` on every websocket envelope — which is what the
dashboard used to do, several times a second while a turn ran — replaced
every node on screen: it restarted the container's entrance animation
(so the page strobed rather than updated), reset scroll inside every
panel, dropped text selection mid-read, and re-collapsed anything
expanded.

Two rules keep it working:

- **Every row in a repeated list carries `data-k`.** The patcher matches
  on that key, so a new event arriving at the top of a feed inserts one
  node instead of rewriting the list beneath it. Never key by array
  index — that is the same as not keying at all.
- **Toggle state lives in the view, not the DOM.** An `onAction` handler
  updates its own set and calls `refresh()`; it never flips a class
  directly, because the next patch renders from state and would revert
  it.
- **A streaming record's toggle keys must not move.** Toggle state is
  keyed per record, and a stored record's key includes its timestamp —
  that is what separates a resumed Execute phase's two records under
  the same turn / phase / iteration. A *live* record must not use it:
  `updated_at` advances on every streamed round, so a timestamped key
  hands each round a fresh identity and silently re-opens whatever the
  reader had just collapsed. `recordFromLiveCall` stamps `_key` for
  this, and views read it through `keyFor(record)`.

## One list per thing

A seat's page answers "what is this agent doing and what did it cost".
Its raw event list is a different question — "what happened, across the
company" — and Activity already answers that one, with category, failure
and actor filters the agent page never had. Rendering a second copy of it
below the turns pushed the page's own content off the fold, so the agent
page links across instead (`#/events?actor=<role>`, which seeds Activity's
actor filter and shows it as a pill even before a matching event loads).

The rule generalises: when a screen wants a list another screen already
owns, link to it filtered. A second implementation is a second set of
filters to maintain, a second paging path, and two answers to one
question.

## What a live row shows

A live row and the finished turn you expand afterwards run through the
same renderer (`llm.js` `responseBody`) over the same text — the engine
builds the live `AgentTurnProgress.response` and the durable
`AgentPhaseCompleted.response` with one function, so the row settles into
the record rather than being replaced by a different rendering of it (see
[Turn Engine § What streams during a
turn](../concepts/turn-engine.md#what-streams-during-a-turn)).

That text carries the model's reasoning wrapped in `<think>...</think>`,
which `responseBody` turns into a collapsible **Reasoning** block placed
inline with the numbered tool badges, in the order things happened.
`llm.js` owns that grammar: anything needing the plain text — the
collapsed row preview, the overview's live card — calls `stripThink`
rather than open-coding the regex, which is how the agent view's live
preview ended up as the one branch that never stripped at all.

## Motion

Entrance motion belongs to the thing that arrived. A view's children
rise once on mount (`fade-up`, 0.32s); a row the patcher genuinely
inserts is marked `.is-entering` and arrives on its own (`row-in`,
0.26s). Nothing re-animates because something else updated.

Motion otherwise marks **live state only**, so movement on screen always
means something is running:

- `live-halo` — a 1.6s pulse in `currentColor`, so one keyframe serves
  every hue. On live badges and working dots.
- `breathe` — a 2.6s pulse on the idle indicator. Waiting is drawn as
  waiting; a busy row has its own motion already.
- `pip-lit` — the round-budget pips on a live phase row, lit in
  sequence with a per-pip delay, so a phase held for a second still
  reads as progressing.
- `rail-packet` — the brand ramp running the turn-rail segment that feeds
  the live phase.
- `cell-breathe` — the current minute of a working seat on the pulse grid.

**Motion stops when work stops.** A live row whose call has not moved for
`STALE_MS` (`js/state.js`, 2 minutes) drops its pip animation and shows its
age in amber; past `STALLED_MS` (10 minutes) it says so in red and takes the
red rail. Pips that keep pulsing on a hung turn actively claim progress that
is not happening, which is worse than showing nothing — this is the rule the
thresholds exist to enforce, not a decoration.

Controls settle with `scale(0.97)` on `:active`. Everything is disabled
under `prefers-reduced-motion: reduce`.

## Failure

`--red` / `--red-ink` is the reserved status hue and never a categorical
slot, because an operations dashboard needs red to keep meaning "this
broke". A failed phase takes it everywhere it appears: the phase rail,
the row badge, the `.failure` block that leads a failed response, and
the `.failure-card` on the agent header that names *why* a seat stopped.

The rule for a failed call is that it stays on screen. The projection
freezes it rather than clearing it, and the response block renders the
error above whatever partial work the phase managed — the prompt it died
on, the tools that had already run. "No response text yet" is a
statement about a call still in flight and is never shown for one that
ended.

Amber is the other half of that rule. `--amber` / `--amber-ink` (`.caution-ink`)
means *needs attention, nothing has broken* — an engine with no event store, a
provider chain that fell through and recovered, a company configuration that is
not active. An engine with no event store has not failed, and painting it the
same colour as a crashed turn trains the reader to ignore the colour that
matters.

**Every feed row carries a `failed` boolean**, decided once on the server
(`live_state._light_event`): the event's own `failed` field, or a type that
*is* a failure (`task_failed`, `llm_unavailable`, `budget_exhausted`,
`turn.guard_breach`). It survives a restart because the writer stamps it as a
`failed` tag — `list_events` never selects the payload column, so without the
tag every historical failure would read back as a success. The client never
re-derives failure from a type list of its own.

That one boolean drives: the red cells on the pulse grid, the red rail and
`--red-ink` summary on feed rows and trace nodes, the **Failures** filter on
Activity, and the red count in the sidebar next to Activity — a grey count of
everything never told you whether to click.

A seat that stopped is as visible as one that is working. The overview's
in-flight board lists a seat with a `last_error` *or* simply known to be AFK:
`last_error` is the full record and does not survive an API restart, and
keying the board on it alone hid every broken seat at exactly the moment an
operator went looking for them.

---

## Changing the dashboard

1. **New colour? Add a token.** Component stylesheets and view modules never
   contain a hex value.
2. **Colour on text is `-ink`, colour on a mark is the base step.**
3. **New surface? Use the panel recipe** by adding the selector to the shared
   rule in `components.css`.
4. **New hue or reassignment?** Re-verify both adjacency orders in both
   themes against the gates above before shipping.
5. **New micro-label?** Mono, uppercase, wide tracking — not bold sans.
6. **New nav entry?** It ships only once an endpoint backs it. A nav item
   that leads to an empty screen is worse than no nav item.
7. **Need the org tree?** Go through `flattenSeats` / `flattenUnits` in
   `js/org.js`, so lead and `mcp_env` inheritance stay resolved in one place.
8. **New data?** It arrives over the websocket — a snapshot section and a
   push, or a query. A view that reaches for `fetch` is reintroducing the
   split-transport bug the refactor removed.
9. **New repeated row? Give it a `data-k`.** And never derive state the
   server already projects; mirror what it pushes.
10. **New engine fact? Decide what it renders as when it is unknown.** The
    health slice is cleared on disconnect, so every field it carries arrives
    `undefined` at exactly the moment the reader most needs the truth. A
    two-way read of a three-way fact renders the unknown case as the healthy
    one.
11. **New figure? Say what window it covers, and do not divide two windows
    into each other.** Two numbers on one screen that disagree is worse than
    one number with a caveat. The budget bar is the one ratio on the page,
    and only because its numerator and denominator are the same engine run.
