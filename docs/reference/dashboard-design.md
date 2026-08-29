# Dashboard Design System

The dashboard is a zero-build, modular ES-module app (see
[API Endpoints § the dashboard](api-endpoints.md#live-stream)). This page
documents its **visual system** — the tokens every component reads, the panel
recipe, and the rules a change has to keep holding.

The ground is a cool blue-black and every division on it — panel fill,
hairline, inset, and the type itself — is a different alpha or lightness of
the *same* cool blue-white. That single material is what makes a dense
operational surface read as one object rather than as a stack of grey boxes,
and it is the rule to keep: a new surface is another step of the ramp, never a
new colour.

**The temperature is the identity.** A cool ground puts the warm half of the
categorical set — amber, orange, brown, and the reserved red — in opposition
to the surface rather than in sympathy with it, so a status mark separates
from its own background before hue is even considered. The brand violet is the
accent because it *is* the mark's colour (`#7c56ff`), which is what keeps the
logo belonging to the page rather than sitting on it.

The ground is deliberately **not** `#000`. An operator reads this page for
hours, and pure black behind near-white text is the specific combination that
halates — the smear around small bright glyphs that anyone with astigmatism
sees, and the pupil oscillation that makes a long session tiring for everyone
else. Lifting it to `#090d1c` costs nothing visually (it still reads as
black), keeps the drop-shadow-is-invisible property the elevation model
depends on, and lets the text ramp sit in a comfort band instead of at the top
of the contrast range.

The chroma is load-bearing, not decoration. A first pass at this ground used
`#0a0c12`, which measures as blue and renders as neutral black: at 5% alpha
over a near-black ground the panel tint had a chroma of 3.3 and the page read
as grey. Panels carry the identity because they are both the largest area and
the lightest, and chroma is far more visible at higher lightness — so the
lever that actually works is the panel fill, not the ground.

---

## Rooms

The sidebar is grouped by the **question a room answers**, not by the kind of
data it holds. That is the reorganisation: nine top-level nouns — Dashboard,
Agents, Activity, Tokens, Tools, Schedules, Fleet, Configuration — meant that
the questions an operator actually arrives with (*is anything waiting on me?
did anything break? what is this costing?*) each needed three or four screens
and a mental join, and the most actionable facts in the system had no
aggregation point at all.

Every entry resolves to a view backed by a real endpoint — nothing is
rendered as a placeholder or a coming-soon stub.

| Zone | Nav | Route | Reads |
|---|---|---|---|
| **Now** — *what is happening, and what needs me?* | Mission Control | `#/` | the snapshot's `agents` / `events` / `sandboxes` / `org` / `tokens` / `budget` / `health` |
| | Agents | `#/agents?group=` | `/org` + live `agents` / `sandboxes` — who is working, who is not, and why |
| | Work | `#/work` | the pushed `sandboxes` for live runs + the `sandbox_runs` query for the durable ones |
| | Activity | `#/activity` | the snapshot's `events`, then live `event` pushes, then the `events` query for stored history |
| **Company** — *who is this company?* | Org | `#/org?lens=` | `/org` + live agent state; lenses `chart` / `directory` / `charter` |
| | Schedules | `#/schedules` | `/schedules` |
| | *a seat* | `#/seats/{handle}?tab=` | the `agent` query, plus `agent_memory` and `tokens` per tab |
| **Operations** — *is the machine healthy?* | Spend & Budgets | `#/spend` | the pushed spend rollup + the `budgets` query |
| | Integrations | `#/integrations` | the `integrations` query |
| | Fleet | `#/fleet` | the `fleet` query (the lease table) — the one polled view, and the one that must say when its poll failed: it is read when nodes are dying, which is when the API answering it is least reliable |
| | Configuration | `#/config?lens=` | `config` / `config_audit` / `config_diff` / `config_entities` *(auth-gated, secrets redacted server-side)* |
| | Tools | `#/tools` | `/tools` |
| — | Engine health | the dot in the brand | the pushed `health` envelope + the `stream` query — a popover, not a screen (see [Health](#health)) |
| — | Trace | `#/traces/{trace_id}` | the `trace` query — reached from a row, never from the nav |
| — | Event | `#/events/{id}` | the `event` query — reached from a row |

**Old routes redirect, with their query strings intact.** `#/events`,
`#/tokens`, `#/agents/{id}`, `#/people`, `#/company` and `#/audit` all resolve
to their new homes: those links are in bookmarks, in chat threads, and in the
seat page's own "Events" button. A redirect costs one `hashchange`; a dead
link costs the reader the thing they were looking for.

**A lens can move too, and it does not look like a moved route.** When the
seat list left Org for the Agents room, `#/org?lens=seats` kept resolving —
the path is still live, so the room simply fell back to its default lens and
put the reader on the org chart. `MOVED_LENSES` in `router.js` keys those on
`route?lens` and redirects them like any other move.

`js/org.js` is where the `/org` tree is flattened into **seats** — every role
with its unit chain, effective unit lead, its configured `token_budget`, and
the MCP surfaces it inherits. Views consume seats, never the raw payload, so
lead inheritance and `mcp_env` inheritance are resolved once.

### Moving, and going back

Every room, section and filter is in the URL, so a screen can be refreshed,
bookmarked, and handed to somebody else as a link. That much is easy. What
takes deciding is the **session stack** — which navigations leave an entry
behind — because that, not the URL, is what the Back button reads.

Three rules, one per kind of move:

| Move | Stack | Why |
|---|---|---|
| A **moved** path (`#/events`, `#/tokens`, `#/agents/:id`, `#/people`, `#/company`, `#/audit`) | replaces | The entry names a route that no longer exists. Leaving it behind means Back lands on it, it redirects forward, and you arrive where you started. |
| A **section** — a lens, a tab | pushes | The reader calls these screens. Back after three of them should walk out through them. |
| A **filter** — pills, sort, a search box | replaces | Four ticked pills are one screen; Back means "take me off this list", not "untick one". |

The line between the last two is whether the reader would call it a
different screen, and it is the only judgement call in the router.

All three shipped wrong, and none of them is visible in a URL. Measured in a
browser: from a redirected route Back could not escape *at all*; Back from a
room's fourth lens left the room entirely; and Back to a list the reader had
scrolled halfway down landed at the top, because the shell reset the scroll
on every mount.

**Scroll is a property of a history entry, not of a URL.** The same room
reached twice is two places the reader has been, and keying a position by URL
collapses them onto one. `takeRoute()` stamps a key into `history.state` and
files the outgoing position under it; an entry with *no* key is exactly the
test for "somewhere new", which is the only case that starts at the top. A
restored position is re-applied for a short window while the view's rows
arrive — a scroll is clamped to the height that exists, so one attempt lands
short — and abandoned the moment the reader touches the page.

**A section change does not remount the view.** The shell compares the
route's *path* parameters (`sameScreen`) and hands a match to
`view.setParams()` instead of tearing the view down. Identity is the path and
never the query: a different tab of one seat is the same screen and keeps its
loaded LLM history, a different seat is not. A view that cannot absorb the
change simply does not implement `setParams` and gets the full mount.

### The attention queue

`js/attention.js` is the one genuinely new idea, and it exists because the
dashboard already knew every one of these conditions and kept each in a
different room:

| Condition | Where it used to live |
|---|---|
| A sandbox run parked on a question | a badge on the overview — and a `reseed` run, box reclaimed, appeared **nowhere at all** |
| A seat the engine stopped | a card among the healthy ones |
| A budget refusing charges | a bar on one seat's page |
| An engine with no active configuration | a line in the health popover |

`buildAttention(state)` derives one list from the slices the server already
pushes, and each item names the room it belongs to. It has three outlets: the
lead panel of Mission Control, a count on each nav zone, and the tab title
(`(3) Crewlet`), so a backgrounded dashboard is a pager.

Two rules make it trustworthy rather than merely useful:

- **It answers `{items, stale}`.** Losing the socket freezes every slice the
  items are derived from, so an empty list on a disconnected page means
  "cannot see", never "nothing to do". `attentionCounts` returns `null`
  rather than a confident zero, and the zone badges are simply not drawn.
- **Severity follows the same rule as everything else.** Red for a seat that
  stopped; amber for an obligation where nothing has broken. A budget doing
  exactly what it was configured to do is not a failure, however much it is
  blocking.

Ordering is severity first, then **oldest obligation first** — the opposite of
a feed, and the right way round for a list of debts: a question that has been
waiting four days outranks one asked a minute ago.

Fleet-sourced conditions (a role no node runs, a seat whose teardown could not
be proven) are absent rather than guessed at: they cannot be reached from the
pushed slices. Moving the derivation into the server projection is what would
let them join, and would follow the rule that the client never derives what
the server can project.

### Search

One palette (`⌘K`, or `/`), over rooms, seats, any event or trace id pasted
from a log, and **commands**. There was no search of any kind before: a
thirty-seat org was navigated by scrolling, and an id copied out of a log could
only be opened by hand-editing the URL. A search box per view would have meant
one ranking rule per view and four places to keep them agreeing.

`buildResults` is pure, so the ranking is testable without a DOM: a prefix
beats a word boundary beats a substring. The current `{theme, density}` is
passed *in* rather than read off `document`, so a command whose label depends
on the current setting is not the thing that drags a DOM into the ranking.

**The API token is state the reader holds, and the page must say so.** It
lives in `localStorage`, rides every WebSocket handshake and every query frame,
and so a browser given one once is authenticated on every later visit and is
never prompted again. That is correct, and it was invisible: nothing on any
screen said a credential was being held, and nothing could drop it — "why does
it never ask me for a token?" could only be answered from devtools. The health
popover now reports `API token: held / not set`, and both the popover and the
palette offer the matching action. Clearing it reaches three places — storage,
the socket's own copy, and the live connection — because the handshake carries
the credential, so an already-open socket stays authenticated until it re-dials.

**And the page has to notice it was refused, which it cannot learn from the
socket.** A rejected handshake reaches a browser with no status and no close
code — a connection that never opened sends no close frame — so it is
reported as 1006, exactly like a stopped engine. This client was written
believing the engine could answer `close(1008)`, and it cannot: every affordance
above hung off a code that never arrived, so a stale token produced a page that
said "retrying" for ever and never mentioned the one thing that was wrong. On a
close that never opened, the client now re-asks over plain HTTP — `GET
/ws/stream`, which answers `401` refused and `426` accepted — and only then
raises the banner. A throw is the network and raises nothing, or a genuinely
disconnected page would beg for a token on every backoff.

**A chrome preference is a command, not a topbar button.** The topbar is the
most valuable strip on every screen and a preference is set once and then
never again. Density spent a permanent, icon-only slot there next to the theme
toggle and read as a mystery control — two horizontal-rule glyphs that say
nothing about spacing, reported as unreadable twice. A command can afford the
words, so it says what it will *do* ("Switch to compact spacing"), the same
rule an icon button's tooltip follows. Theme keeps its button: it is flipped by
the light in the room, which changes through the day.

Commands rank below rooms and seats — somebody typing into the palette is
navigating — and are hidden entirely on an empty query, since the palette opens
on navigation and a preference at the top of a blank palette is one offered
every single time it is opened.

`commandPalette.test.mjs` reads the sidebar's own list out of `app.js` and
checks every room in it is reachable. That is not hypothetical: the Agents room
shipped in the sidebar and was the one place `⌘K` could not take you.

### Mission Control

**A room only earns a band for a question no other room owns.** Mission
Control's job is triage, and the way it fails is by re-rendering the rest of
the product: a per-seat grid the Agents room answers better, an in-flight
board with a better-sourced twin in Work, a phase bar that belongs on Spend
where the window is selectable, and a truncated Activity with none of
Activity's machinery. Half of that was also untrustworthy — `store.setConnected`
clears only the `health` slice, so `agents`, `events`, `tokens`, `budget` and
`sandboxes` stay frozen, and a page that prints them at full confidence on a
dead socket is worse than a page that prints nothing.

Five bands, top to bottom, in order of urgency:

| Band | Answers | Owned here because |
|---|---|---|
| **Needs you** | Is anything waiting on me, and how long has the oldest waited? — the [attention queue](#the-attention-queue) | No other room aggregates obligations across sandboxes, seats, budgets and config |
| **Engine** | What is the *engine* doing — turns in flight, posture, event store, draining | Most of it reached no pixel outside a popover you had to know to click |
| **Stuck** | Which turns stopped producing rounds, and which seats hold a lease whose teardown was never proven | `internal/api/runtime.go` calls the second "the one to alert on" and it reached no screen at all |
| **Recent record** | Did anything break, and how far back can this page even see? | One company-wide track, not one row per seat — the per-seat view is the Agents room's |
| **Cost** | What is this costing, in the two spans that are honest | The 24h rollup and the org meter-against-its-own-cap; per-seat detail is Spend's |

Two rules hold across all five:

- **A number whose precondition is absent renders as an em dash, never as a
  zero.** "This page cannot see the engine" and "the engine is doing nothing"
  are opposite facts, and a `0` merges them.
- **A frozen projection is not a quiet company.** Each band says, on its own,
  which part of itself it can no longer vouch for once the socket drops.

The attention queue is drawn even when it is empty — an answer that disappears
when it is "nothing" teaches the reader to check whether the panel is there
rather than to read it — and it keeps its `<section class="panel">`, because
the rows carry separators and no surface of their own.

### Agents

*Who is working, who is not, and why.* This was the product's most-asked
question and its worst-buried answer: it lived as one lens of the Org room,
which files it under how the company is **arranged** rather than what it is
**doing**, and the LLM calls behind it were another two clicks down in a tab of
a seat page you could only reach from that lens. Meanwhile the most prominent
list of agents in the product — the pulse strip on Mission Control — rendered
every row with a pointer cursor and did nothing when clicked.

So it is a room in **Now**, and the only place the seats list lives. Org keeps
the structure (the chart, the directory, the charter); this owns the live
state. Two screens answering "who is working" would eventually disagree.

**Grouped by what a seat asks of the reader, not by the state enum.** A seat
parked on a question and a seat that fell over are both "not working", and only
one of them is broken — the enum cannot express that, so the grouping does:

| Group | Holds | Tone |
|---|---|---|
| **Needs a person** | A sandbox parked on a question, a seat with `last_error`, a seat gone `afk` | amber, or red for the two that are failures |
| **Working** | Mid-turn, including `awaiting_sandbox` — a detached run is work | the working hue |
| **Idle** | Running, empty inbox | uncoloured — quiet is not a status |
| **Not running** | No process is serving the seat; on a fleet, no node has claimed it | uncoloured |

`classify(seat, {agent, sandbox, sandboxes})` decides the group and, separately,
whether the row is `broken` — separately on purpose, because "needs a person"
holds both a question (amber) and a failure (red), and red is reserved for
failure. Inside a group, oldest first: a question asked on Friday outranks one
asked a minute ago.

**The whole row opens the seat.** It was a plain div with the name as its
only target — a row you could point anywhere in and click nothing, on the room
whose entire job is getting you to a seat. That also retires the "Open seat"
button that sat at the end of it: a second control for what the row already
does is one affordance too many, and it made the row's own click look like it
must do something else. A chevron says the row goes somewhere; **LLM calls**
stays a button because it is the one destination that *isn't* the seat's front
page, and it lands on `#/seats/{handle}?tab=now`.

A nested control inside a `role="link"` row works because
`closest("[data-action]")` resolves innermost-first — no `stopPropagation`
anywhere. The keyboard needed one fix for it: a `<button>` already fires its
own click on Enter, so the shell's row handler walking up to the enclosing row
and clicking that too ran both actions, and the row's won because it was
second. It now leaves native controls to the browser.

The group chips are a **filter**, so they replace rather than push (see
[Moving, and going back](#moving-and-going-back)), and their counts render as
`–` rather than a confident `0` when the socket is down.

### The pulse

`js/pulse.js` — one cell per minute of the last hour, lit by what the company
actually did. A cell is lit because work happened in that minute, its
brightness is that minute's event count against the busiest cell on the track,
and a red cell is a real failure.

| Element | Source |
|---|---|
| Cell brightness | Feed events in that minute, over the track's busiest cell |
| Red cell | The feed row's `failed` flag (see [Failure](#failure)) |
| Pale cell | Past the retention edge — *unknown*, not quiet |
| The sentence beneath | `n events in the last m minutes · k failed`, from one bucketing pass |

`buildPulse` is a pure function over data the page already holds, so the band
costs no request and no server work. It buckets **once** and answers both the
company-wide `cells` track and the per-seat `rows` — the count is incremented
before the roster check, so engine-authored events with no actor are in the
total; summing the seat rows instead would undercount every one of them.

`failures / total` is the one ratio legal on Mission Control: one bucketing
pass, one window. It must never be divided into the 24-hour token rollup.

**The band claims only what the feed can speak for.** The projection retains
a bounded number of events (`MAX_EVENTS` in `js/store.js`, matching the
server's `EVENT_FEED_LIMIT`), and a busy org fills that in minutes — so on
such an org the older part of the hour has *no record*, which is not the same
as no activity. Cells past the retention edge render pale and say so, and the
sentence counts the minutes actually covered. Drawing the gap as idle would
make a company that had been flat out all hour look like one that woke up five
minutes ago.

### Reading history

The live feed is a bounded ring (`MAX_EVENTS`), which a busy org fills in
minutes. Activity pages beneath it into the event store with the `events`
query, and three rules keep the merged list honest:

- **One ordering key**, `(instant, id)` descending — `newestFirst` in
  `format.js`, mirroring the server's own ordering. Raw ISO strings are not
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

### Event detail

`#/events/{id}` lays a single event out field by field, and it is the one
screen where text an outsider chose — a Slack message, a Jira summary, a
merge-request title — is rendered as markup. Two rules hold it together:

- **Every value is escaped**; a caller opts out by wrapping it in the
  module-level `raw()` marker. The default used to be the other way
  round, with each caller expected to remember `esc`, and one that forgot
  was a stored-XSS hole.
- **A URL out of a payload is checked before it becomes an `href`.** Only
  `http(s)` renders as a link — anything else, `javascript:` above all,
  renders as escaped text.

An inbound webhook gets a per-source layout (Jira, Confluence, Slack,
Mattermost, GitHub, GitLab, Plane), and the raw payload block stays
beneath every one of them: a layout can only surface the fields it knows
about, and the field it does not know is the one an operator came for.

Each layout reads the same fields the engine's own router reads, so the
screen answers *why did this wake anyone*, not just *what arrived*:

| Source | What the layout names |
| --- | --- |
| GitLab | `object_kind`.`action`; the actor (`user.username`, or the flattened `user_username` a push hook sends instead of a user object); `project.path_with_namespace`; the MR, issue, pipeline or branch the event hangs off — a sibling key on a `note` or `pipeline` hook, `object_attributes` on the others; `state` and the MR's source → target branches; the `changes.{assignees,reviewers}` `previous → current` diff the parser routes on; `object_attributes.url` |
| Plane | `event`.`action`; `activity.actor` (a bare UUID or an expanded user); `workspace_slug`; the project identifier; the work item as `{identifier}-{sequence_id}`, or the page; `activity.field` with `old_value → new_value`; `data.assignees`; the `<mention-component>` ids a comment carries |

Two of those are load-bearing for an operator reading a failure.
`pipeline.failed` is the one GitLab event routed back to the *actor*
rather than to the thread — the agent whose push broke the build owns the
fix — so its status renders as a failure badge and the failing jobs are
named rather than left in the payload. And on a Plane work item,
`data.id` is the work item on an `issue` event but the comment or intake
row on the others, where the work item is `data.issue`; the screen draws
that distinction exactly where the transport draws it, because the other
id produces a pointer that 404s.

The matching question for a *notification* is answered by
`metadata.routed_via` (`assignee`, `assignee_added`, `mention`,
`subscriber`, `project_lead_fallback`, `intake_triage`, …). It is
promoted into a row of its own instead of appearing as one chip among a
dozen coordinates, where it read as another id.

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

### Colouring a seat

**A seat's colour says what it is doing. Nothing on the page colours by
identity.**

The dashboard used to hash the seat name into one of the eight categorical
hues (`roleColor` / `roleInk`) and tint the avatar, the name and a leading
hairline with it. Two things were wrong with that, and they compounded:

- **Identity and status shared one palette.** The same eight families carry
  event category, phase, and integration brand. A seat whose name happened to
  hash to amber looked like a seat that needed attention, and an amber-hued
  seat that genuinely needed attention looked like itself.
- **The tint was unconditional**, so an idle seat carried a lit, saturated
  blob that read as work in progress — on the one screen whose whole job is
  telling working from not.

`seatTone(agent, sandboxes)` in `state.js` is the single derivation, and every
surface that draws a seat calls it:

| Tone | Means | Used for |
|---|---|---|
| `needs` | A sandbox is parked on a question | amber — attention, not failure |
| `broken` | `last_error`, or the seat has gone `afk` | red — reserved for failure |
| `working` | Mid-turn, including `awaiting_sandbox` | the working hue |
| `quiet` | Idle, not running, or a human seat | **untinted** — quiet is not a status |

`avatarFor(role, tone)` defaults to `quiet`, so a caller with no state to hand
renders an untinted tile rather than inventing one. That is the honest answer
when the page does not know, and it is also why a seat page's header avatar is
plain: it is identifying a seat, not reporting on one.

`statusLine` never invents activity either. An agent with nothing in flight
says so; an AFK agent shows its engine-detected cause; a seat running a
detached sandbox says it is writing code, and says when it is blocked on an
answer.

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
| `--bg-card` | Panel fill — a cool blue-white **alpha** over the ground in dark, so nested panels accumulate depth without another token |
| `--bg-card-2` | Raised inner surface (table headers, chips) |
| `--bg-hover` / `--bg-active` | Row and control states |
| `--bg-inset` | Recessed surface (code blocks, expanded bodies) |
| `--border-subtle` / `--border` / `--border-strong` | Hairline ramp |
| `--text` / `--text-secondary` / `--text-muted` / `--text-dim` | Ink ramp — the cream hue at a small chroma, so the whole surface reads as one family rather than as tinted panels holding neutral text |
| `--heading` | Headings, row titles, headline numbers |
| `--focus` | The keyboard focus ring. Its own token because it has to clear the surface *and* every hue it might land on, so it cannot be the accent |

**Contrast is a band, not a floor.** Guidance sets minimums, and a palette
tuned only against minimums drifts to the top of the range where it is legible
and unpleasant. The palette this replaced did exactly that in both directions
at once: body text at 19.25:1 (halation) while `--text-dim` sat at 2.67:1,
under the 3:1 floor it existed to clear, and the light theme's `--text-muted`
at 3.55:1, under the text floor.

**And every step is measured against the worst surface it can land on**, which
is the part that is easy to get wrong. Because the dark panel fill is an alpha,
panels nest; a row inside one can be hovered or selected, and a code block cuts
an inset into it. The **chrome** composites too, and off the sidebar rather
than off the ground — which in the light theme makes the selected nav item the
darkest surface in the whole design, darker than anything in the content area,
and it is exactly where the nav's alert badge sits. That is **ten** distinct
composites carrying the same tokens, spanning about a point and a half of
contrast, so a ramp anchored to the panel is anchored to neither end. A browser
audit across ten rooms found sixteen elements rendering under 4.5:1 while every
token in the file "passed" its panel-anchored check — and twice since, an
*incomplete* surface set has been the bug rather than the value measured
against it (a tint measuring 4.87 rendered 4.29 on the nav surface nobody had
listed).

The ranges below are what each step covers across all ten surfaces in both
themes, so the low figure is a guarantee rather than an average, and the high
one is a ceiling rather than a score:

| Step | Range |
|---|---|
| body text | 10.1 – 14.7 — comfortable for sustained reading |
| headings | 11.2 – 16.2 — larger glyphs tolerate, and want, more weight |
| secondary | 7.3 – 10.6 |
| muted | 5.8 – 8.4 |
| dim | 4.7 – 6.8 — de-emphasised meta, still a **text** step |
| hue ink | 5.9 – 11.0 |
| hue mark | 3.3 – 12.2 (the 3:1 non-text floor) |

An ink is held to two things, not one: the flat floor above, and a clear step
above **its own mark**. A badge is that hue softened into a fill with that
hue's ink printed on it, so the ink's real ground is the mark composited into
the panel — and when the two steps converge, as `--purple` and `--purple-ink`
did, nothing is left to clear. How strongly a hue is softened for that job is
one token, `--tint`, because those sites are one object; they had drifted to
six different percentages with nothing choosing between them, and the 15% one
rendered its ink at 3.8:1. The token carries its own `%`, so a consumer writes
`var(--tint)` and never `var(--tint)%` — CSS drops an invalid declaration
without falling back a cascade level, so a doubled unit silently deletes the
fill rather than reverting it (a gate in `palette.test.mjs` fails the build on
one). The direction flips per theme — on dark a tint
lifts the ground toward the ink, on light it pulls the ground down toward it —
so a tint judged by eye on dark has been judged for the wrong theme.

`--text-dim` takes the text floor rather than the 3:1 non-text one because in
this design there is no non-text use of it: the same audit found it carrying
10px labels in ten places — a clock, "last 24h", "no live meter" — and marking
nothing anywhere. A step specced as a mark and used as type everywhere is a
text step measured against the wrong floor.

### Spacing, type, and density

Font sizes and paddings were literals scattered across ~3,000 lines of
component CSS, at half-pixel precision and in eleven distinct values, which
made "make this readable" a sweep rather than an edit. Both are ramps now:
`--text-2xs … --text-3xl` and `--space-1 … --space-8`.

The spacing ramp is multiplied by one scalar, `--density`, so an operator who
wants more rows on screen gets them from a token swap rather than a second
stylesheet: `data-density="compact"` on `<html>`.

### Accent and glass

`--accent` is a **pale fill**, not a text colour — its readable partner is
`--accent-ink`, and `--accent-soft` is the wash used behind counts and chips.
`--glass` / `--glass-hover` back the topbar, icon buttons, and segmented
controls, always with `backdrop-filter: blur(…)`.

### Brand

`--brand-gradient` is the full seven-stop mark; `--brand-ramp` is its
three-stop cousin for anything only a few pixels tall. Both are used as
**light, never as a fill** — a hairline along the topbar's bottom edge, and
the packet travelling the turn rail. There is no third place for them.

The topbar rather than a panel, because a panel comes and goes with the room:
the gradient used to light the overview's hero, and when that panel was cut in
the Mission Control redesign the mark left the product entirely except for the
rail packet. The one edge that parts chrome from content is drawn by every
room, so it cannot lose the mark that way. It sits *on* the border rather than
beside it — the chrome is still parted by a single hairline; the gradient only
says which hairline it is.

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
`color-mix(in srgb, var(--<hue>) var(--tint), transparent)`. The strength is
**one token**, because a badge, a chip and a severity wash are one object; the
six percentages they had drifted to were chosen by nobody, and the 15% one
rendered its ink at 3.8:1. `--tint` carries its own `%`, so a consumer writes
`var(--tint)` and never `var(--tint)%`.

**A hue name is never assembled at the use site.** `var(--${tone}-ink)` looks
like it types itself and does not: a caller naming the *meaning* it wants
("bad", "warn") builds `--bad-ink`, which is not a token, so the declaration is
invalid and the browser drops it — without falling back a cascade level, so the
figure that most needed colour renders plain. Renderers map a semantic word to a
token through a closed map (`STRIP_TONES` in `ui.js`, `PHASE_HUE` and
`EVENT_CATEGORIES` in `state.js`), and `palette.test.mjs` runs each of them over
its whole input domain and checks every `var(--…)` they actually emit.

Chroma is capped at 13.5 (OKLab ×100), because highly saturated
light-on-dark type is what "vibrates". Only lightness is re-stepped per theme.
Two families sit above that cap, and only because a measurement forced it:
`--red` (below) and `--pink` at 14.5, which is the least rotation and chroma
that lets the dimmest mark clear the non-text floor on a selected row while
still holding ΔE 15 against purple.

`--red` / `--red-ink` is a **reserved status hue** (failed, error,
destructive) and is never used as a categorical slot. It is allowed more
chroma than the cap and sits slightly off the orange family: it has to stay
legible as "this broke" among the categorical marks, and it is used sparingly
— rails, badges, dots — never as body copy, which is where a saturated colour
would tire the eye.

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
| Event categories | `CATEGORY_HUE` in `js/state.js`, rendered in `EVENT_CATEGORIES` order by the activity filter row and category tags | lifecycle → blue, task → green, communication → purple, decision → amber, knowledge → pink, learning → purple, a2a → orange, notification → cyan, webhook → brown, system → neutral |

`CATEGORY_HUE` lives in `state.js` beside `PHASE_HUE` rather than only in the
`.cat-*` rules, so both orders can be read — and measured — from one place. A
test holds the CSS to it.

`PHASE_HUE` in `state.js` is the single source of truth for phase colour;
`phaseColor()` returns the mark step and `phaseInk()` the text step. There is
no second hardcoded copy — the agent × stage heatmap mixes its fill from
`phaseColor()` so it re-steps with the theme instead of being frozen at one
theme's values.

The assignments are not free choices. They are the ones under which every
adjacent pair in **both** orders clears, in **both** themes:

| Check | Gate | Light | Dark |
|---|---|---|---|
| Normal-vision separation (worst adjacent pair, OKLab ΔE ×100) | ≥ 15 | 18.6 | 17.8 |
| Protan / deutan separation | ≥ 8 | 11.5 | 15.9 |
| Mark contrast vs the surface | ≥ 3:1 | 3.3 | 3.3 |

**The mark steps do not all sit at one contrast, and that is load-bearing.**
Capping chroma for comfort costs separation, and placing every mark at one
comfortable contrast then collapses pink against purple to ΔE 12.5 (gate 15)
and 8.0 for protanopia (gate 8) — at equal lightness they are only a hue
rotation apart. Three lightness tiers, assigned so that adjacent families
differ in lightness as well as hue, is what clears every gate in both themes
with ≥18% margin while keeping the set inside the comfort band.

Red against green under protanopia is left unsolved deliberately: those are
the two hues that cone cannot separate, and pushing red until it cleared green
for them would leave a colour nobody else reads as red. The rule that colour
never carries identity alone is what closes that gap.

**These numbers are computed, not remembered.**
`tests/test_dashboard/js/palette.test.mjs` reads `tokens.css` and `state.js`
on every run and measures all of it — contrast bands, ink and mark floors,
chroma ceilings, and OKLab separation for normal, protan and deutan vision
across both orders. They were measured by hand once before that, and by the
time the suite was written the dark theme's `--text-dim` had drifted under its
own floor. The suite caught a red/orange collapse on its first run.

**If you change a hue, run the suite.** Changing one value can break a pair
three slots away.

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
4. **New hue or reassignment?** Run
   `node tests/test_dashboard/js/palette.test.mjs`. It measures both
   adjacency orders in both themes against every gate — and each colour
   against the worst of the ten surfaces the design composites, not
   against the panel. Do not eyeball it: the panel-anchored version of this
   suite passed a `--pink` that renders at 2.58:1 on a selected row. A new
   *phase* or *category* needs a hue the set may not have spare — the eight
   families are already doubled up in one order.
5. **New micro-label?** Mono, uppercase, wide tracking — not bold sans.
6. **New nav entry?** It ships only once an endpoint backs it. A nav item
   that leads to an empty screen is worse than no nav item.
7. **Need the org tree?** Go through `flattenSeats` / `flattenUnits` in
   `js/org.js`, so lead and `mcp_env` inheritance stay resolved in one place.
8. **New data?** It arrives over the websocket — a snapshot section and a
   push, or a query. A view that reaches for `fetch` to READ is
   reintroducing the split-transport bug, and that is not hypothetical: the
   Fleet view took its HTTP client from a context field the shell never
   populated and held a loading skeleton in every shipped build until it was
   moved onto the `fleet` query. Writes stay REST, for the auth
   middleware's attribution. `wiring.test.mjs` enforces both halves.
9. **New view module? It has to be reachable from `app.js`.** ES modules
   fail as a graph: one missing import and *nothing* renders — no view, no
   nav, no error on the page. The same suite checks that every import
   exists and that nothing is orphaned.
10. **New room? Put its CSS in `styles/rooms/<room>.css`.** One stylesheet
    per room, so a change to one room's layout cannot reach another's.
11. **New obligation the operator has to act on?** Add it to
    `buildAttention` rather than to a panel of its own. A condition that
    needs a person and lives only on the screen that owns it is a condition
    nobody finds — that is what the queue exists to end. Decide its
    severity by the rule: red only if something broke.
12. **New repeated row? Give it a `data-k`.** And never derive state the
    server already projects; mirror what it pushes.
13. **New engine fact? Decide what it renders as when it is unknown.** The
    health slice is cleared on disconnect, so every field it carries arrives
    `undefined` at exactly the moment the reader most needs the truth. A
    two-way read of a three-way fact renders the unknown case as the healthy
    one.
14. **New figure? Say what window it covers, and do not divide two windows
    into each other.** Two numbers on one screen that disagree is worse than
    one number with a caveat. Budgets are where this bites: a cap, the
    engine-run meter and the durable counter are three numbers over three
    spans, and only the meter shares a span with the cap. Exhaustion is read
    from `refused_at`, never from `used >= max` — the engine refuses a
    charge that would exceed the cap and increments nothing, so a blocked
    seat never reaches its own maximum.
15. **New spacing or font size? Use the ramp.** `--space-*` and `--text-*`
    exist so density is a token swap; a literal px value re-opens the sweep
    they replaced.
16. **Changed a colour or a surface? Measure the rendered page, not the
    tokens.** The suite in step 4 checks what the stylesheet defines; it
    cannot see which token a component actually applies, or at what size.
    Both defects the last colour pass shipped were of that kind — a text
    ramp anchored to the wrong surface, and a step specced as a mark and
    used as 10px type — and both were found by walking every rendered
    element in a browser, compositing its colour against the stack behind
    it, and sorting by ratio. Nothing under 4.5:1, nothing over 16.5:1.
17. **New navigation? Decide whether it pushes or replaces.** A section
    (`pushParams`) or a filter (`replaceParams`), by the rule in
    [Moving, and going back](#moving-and-going-back) — and a redirect always
    replaces. Nothing about a wrong choice shows up in the URL, in a
    screenshot, or in a render test; it shows up the first time somebody
    presses Back, which is why `routing.test.mjs` asserts stacks and scroll
    positions rather than URLs.
18. **Colouring a seat? Call `seatTone`.** Nothing colours by identity, and
    `quiet` is untinted — see [Colouring a seat](#colouring-a-seat).
19. **A whole row that navigates? Make the ROW the target.** `role="link"`,
    `tabindex="0"`, `data-action` on the row itself — not a link buried in
    one cell with the rest of the row inert. A nested control for a
    *different* destination is fine (innermost wins); a second control for
    the row's own destination is not.
20. **A new chrome preference is a palette command, not a topbar button.**
    See [Search](#search).
21. **Deleted a view? Delete its stylesheet rules in the same change.** Dead
    CSS breaks nothing, so it accumulates until a later rule collides with
    it; two rooms' worth survived the redesign that cut them.
    `wiring.test.mjs` fails the build on a rule with no markup behind it,
    and understands a class that is *built* (`cat-${category}`,
    `tone-${tone}`, `"is-" + stale`) rather than written out.
