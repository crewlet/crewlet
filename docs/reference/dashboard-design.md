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

So colour is spent in exactly three places:

| | What it means | How many |
|---|---|---|
| **State** | *what a piece of work is doing* — info is working, warning needs a person, danger is stopped, success is done | 4, fixed |
| **Accent** | *what to do* — the primary action, the focus ring, the Inbox's count of what is waiting, the on filter | 1 |
| **Data** | a chart series, inside a figure that names it (a legend, or the label of a one-series figure such as a meter) | 4 + a neutral residual |

Everything else — a seat, a unit, a **phase**, an event category, an
integration's state, a node, a tool origin — is **neutral**, and its identity
is carried by its name, its icon and its position. Those are stable, legible,
and do not run out at eight.

A phase is a category, so it is its **word** on the neutral pill. It used to be
a fourth use of colour with three hues of its own, and the design system
removed that family in `@crewlethq/tokens` 0.5.0: beside a state badge the
state has to be the one coloured thing on the row. Inside a figure a phase is a
series like any other, drawn in the hue of the **band** the engine folds it
into (`tokens.PhaseBand`: execute and a coding run are Execute, review is
Review, delegated workers are Workers, and the rest are Auxiliary), so one
phase is one colour on every chart. `contract/spend.ts`' `PHASE_BANDS` is that
fold written out by phase, and `internal/api/queries` holds it against the
engine's in both directions.

The one exception is a **third-party app's own mark** on the Integrations
screen (`VendorMark`, from `@crewlethq/icons`): Slack's four colours,
Atlassian's blue, GitLab's orange,
Datadog's violet, drawn as the third-party app draws them. A mark is identity by
definition, and a recoloured Slack mark is not Slack's. The exception is held
to exactly that: a mark is drawn only beside the third-party app's name,
nothing reads state from it, none of its hues is reused as a token, and the
integration's STATE beside it is carried by the status tone like everything
else. A tool the company has not set up keeps its mark, drained of its
colour (the kit's `muted` mark) — the mark rather than the tile, because the
mark is decoration beside the name and the tile's text keeps its contrast.

A seat's chrome takes one of three **rings**, or none, from what it is DOING —
and what it is doing is the ENGINE'S word, never the dashboard's. Every seat row carries
`activity` (`working`, `needs`, `stopped` or `idle`) and, when stopped, a
`stopped_reason` (`paused`, `unplaced`, `budget` or `provider`), computed once
by the live projection from the seat's turn, its coding runs' durable record,
its pause, its placement across the fleet and its budget windows (see
[Agent States](../concepts/agent-runtime.md#agent-states)). `lib/seats.ts`
maps the word to a ring, a label and a state line and folds nothing in: the
dashboard used to derive a seat's state three ways on three screens, and a run
parked on a question past the old twelve-hour age-out dropped out of every one
of them. A source gate in `seats.test.ts` refuses any other module turning a
live call's or a run's fields into one of the state words. The two words are
the contract's `SeatActivity` and `StoppedReason`, held to the engine's by a
gate.

| Ring (kit tone) | `activity` | Label |
|---|---|---|
| `info` | `working`: mid-turn, or a coding run it launched is running | "Working"; the state line names the phase and the item ("Executing ENG-412"), the workers ("3 workers on ENG-405" — the tasks of the `delegate` call the seat is waiting on, read off its call in flight, the one record that exists while they run), or a parked run ("Coding run on ENG-9") |
| `warning` | `needs`: a coding run it launched waits on a question only a person can answer | "Needs you", "Needs you · run parked" |
| `danger` | `stopped`: paused, placed on no node, its budget window refusing, or its provider unreachable | "Paused by Jane Founder · 12m" (the pauser by the name the chart gives their handle; a token nobody bound, by its own name), "Not placed on any node", "Stopped · budget", "Stopped · provider" |
| **none** — a neutral pill | `idle`, no row from the engine yet, or a human seat | "Idle · last turn 24m ago", "No state from the engine yet" |

A handle is written `@pm`, and a seat the engine reported no handle for gets
nothing rather than a bare `@` — which is why no markup prints `@{handle}`
itself: every handle goes through `handleLabel` in `lib/seats.ts`, and
`app/source.test.ts` refuses JSX text ending in `@` before an expression and a
template inside markup that does the same. A live round that has not moved in two minutes
is called stale, in ten stalled — except while the turn is **parked** on a
detached coding run, which is silent by design for as long as the run takes:
the executor's phase card then reads "parked on its coding run", because the
stage travels with the live call off the seat's own turn.

A failed last turn does not colour a seat. The seat takes its next wake like
any other; its `last_error` is shown where the seat is read about, not as a
stop.

**Where a seat runs is which node holds it** — the seat's "Held by" fact:
"this node · node-a", "another node", or "no node — not placed", read off this
node's own health push (the seats it holds) and the engine's `unplaced`. It was
the agent instance id, which exists only mid-turn, so an idle seat this very
node held read "not running on this node". Which peer holds one held elsewhere
is Settings › Nodes' to say: asking the fleet to label one seat would make
opening a peek a company-wide read.

An idle seat is deliberately untinted. It used to draw a tinted, glowing tile
that read as activity, and the fix for that is not a duller hue — it is none.
`warning` and `danger` are separate because a seat parked on a question and a
seat that fell over have both stopped, and only one of them is a failure.

---

## The palette is measured, not asserted

`@crewlethq/tokens` is the one source of colour, type, space and motion. The
dashboard declares none of its own: it imports the palette, the themes, the
density scale, the faces and the document baseline, in that order, above every
other import in `dashboard/src/main.tsx`. Above, because an import is
evaluated in source order and the components carry their own stylesheets, and
the baseline has to be the thing a component rule outranks rather than the
other way round. The dashboard's own sheets (`tokens.css`, `base.css`,
`components.css`, `frame.css`, `screens.css`) come last.

**There is no alias layer.** Every stylesheet in `dashboard/src` reads the
package's tokens under their own names — `--color-text-tertiary`,
`--spacing-4`, `--size-control-md`, `--shadow-focus` — and `tokens.css` keeps
only three values that are this application's own (a row list's inset, the
toolbar's band and the sticky offset under it). A short name for a token is a
second name for one value: it hides a token the package renames behind a name
that still parses, and it is where the two come to disagree about which value
a word means. `styles/tokens.test.ts` refuses a stylesheet of ours that
DECLARES any `--color-*` or `--shadow-*`, and reads every `var()` against the
installed package.

The rule table and the colour maths are the package's own,
`@crewlethq/tokens/test/palette`, so the design system and this application
cannot come to disagree about what a floor is.
`dashboard/src/styles/palette.test.ts` runs them over the INSTALLED
stylesheets in that same import order, in **every theme state** (dark on the
bare root, light by media query, light by attribute) and over **every
composited surface a token can land on**, including a hovered row inside a nested panel, which is where a
ramp anchored to the panel fill quietly falls under its floor. The engine
measures as well as the package because a tokens release that lowered a ratio
would otherwise arrive here through an auto-merged bump with nothing in `make
check` measuring one.

What is measured, and the floor each clears:

| Claim | Floor |
|---|---|
| `--color-text-primary` on every surface | 7:1 |
| `--color-text-secondary`, `--color-text-tertiary` on every surface | 4.5:1 |
| `--color-text-muted` on a panel | **between 2.8 and 4.5**, because it is decoration, and a step that crept up to 4.5 would invite itself into a table cell |
| every `-ink` step as TEXT on every surface, and on its own `-soft` tint | 4.5:1 |
| every fill step as a MARK on the page surfaces it sits on | 3:1 |
| `--color-text-on-accent` on the accent, and white on danger | 4.5:1 |
| the focus ring against every surface | 3:1 |
| the four state hues, pairwise, under normal / protan / deutan vision | ΔE 10 |
| adjacent data hues, in series order, under all three | ΔE 9, and ΔE 15 under normal vision |
| every data hue against the reserved danger hue | ΔE 14 normal, 8 dichromat |
| every other hue against the accent, under all three | ΔE 10 normal, 8 dichromat |
| the neutral ramp's chroma | ≤ 2.2 |

**A measured token is only as good as where it is spent.**
`--color-text-muted` clears its floor as decoration, and the dashboard spent it
on words a reader has to read: "(optional)", "not set", "none", "not reported
by this engine", a seat's goal, an event's category, the placeholder in every
text box. Those take `--color-text-tertiary` now. The floors themselves are measured by
`@crewlethq/tokens`' own `runPalette` over all four states of the cascade,
driven from `styles/palette.test.ts` — a second implementation beside it would
be two ideas of what "4.5:1 on every surface it can land on" means. WHERE a
step is spent is the rule this page states and a reviewer keeps; what is
mechanical is that it exists at all: `styles/tokens.test.ts` fails on a `var()`
naming a token nothing declares, because an undeclared custom property does not
fall back to what it used to be — it takes its whole declaration with it.

**The fill/ink split is enforced by that measurement, not by convention.** An
`-ink` step is text; a fill step is a mark or a background. Mixing them is how
the screen this replaces shipped role badges at 1.63:1 — an inline
`background:` with a hue token in it and the default text colour left on top.
Only `--color-brand-accent` is ever a fill behind a word, under
`--color-text-on-accent` (and the danger fill behind a destructive action's
white label, which the package measures to 4.5:1). A state is a soft tint
carrying its own ink step, never a solid block with a label on it, which is
also what lets the state fills be light enough to read as marks on a dark
ground.

### The ground

**Dark first.** The design system paints dark on the bare root and light under
`@media (prefers-color-scheme: light) :root:not([data-theme="dark"])` and
`:root[data-theme="light"]`, so a browser that reports no preference gets the
palette the product is drawn in, the OS setting works, and an explicit choice
wins in both directions. The package's own suite compares the two light blocks
key for key. The dashboard adds no theme block of its own — `tokens.css`
declares no colour at all — so there is nothing here for the two to disagree
about.

The dark ground is deliberately **not** `#000`. An operator reads this page for
hours, and pure black behind near-white text is the specific combination that
halates. It is also barely blue: the system this replaces tinted every neutral
with `rgba(176, 152, 255, …)`, so the whole product read violet and the accent
had nothing to separate itself from. A near-neutral ground with one saturated
accent is what makes the accent mean "here".

**Four opaque rungs, and each is painted by what it names.** `--color-surface-
frame` is the window and its navigation — the sidebar stands on it; `--color-surface-background` is the SHEET, the page column
everything is read on, and a bar inside the page (the page bar) stands on it
too; `--color-surface-subtle` is a card, which is what the kit's `Card` paints;
and `--color-surface-elevated` is the one step a card can show — a block inside
a card, a grid's head, a chip, a well. Below them `--color-surface-inset` is a
translucent recess that reads as a well on whatever it lands on. A card is
found by its HAIRLINE as much as its rung — the sheet-to-card step is nearly
flat on purpose, and a page of cards each lifted by a large step is a relief
map.

The ramp was once four numbered names over three values, and the collision
was at the bottom, where the chrome lives: the sidebar, every card and every
grid painted the page's own colour and were told apart from it by a
1px border — twenty-two declarations, none of which drew anything — and at the
top, the calendar's out-of-month cells painted the card's own colour. A
numbered ramp cannot state which rung a thing stands on, so nothing about
either rule looked wrong in the file. `styles/surfaces.test.ts` measures the
rungs against the installed palette — all four a different colour from every
other in every state, the frame under the one sidebar, the sheet under the
page, the card rung under the kit's `Card` — and refuses an interaction
state (`:hover`, `:focus`, `:active`) that paints a structural rung rather than
an overlay.

### Type

Two self-hosted variable faces — **Geist** and **Geist Mono**, the `latin`
and `latin-ext` subsets, 83,736 bytes for the four files, embedded like every
other asset and served from `/static/dashboard/fonts/` beside their OFL text.
They arrive from `@crewlethq/tokens`, whose 0.5.0 replaced Inter and JetBrains
Mono with them. Self-hosted because the faces this tree once had were fetched
from a CDN, which was its ONLY external runtime reference: on an air-gapped
engine — a supported deployment — every face fell back to a system font the
design was never measured against. `TestTheBuiltDashboardIsWhole` requires
exactly the four Geist faces in the built stylesheet, each under `fonts/`.

The document's line is the kit's: 13px on 1.45.

Nine sizes, `--font-size-2xs` … `--font-size-3xl`, and **they are the only
sizes in the product**. The system this replaces had 194 `font-size` declarations across 14
literal pixel values, eight of them off any ramp, so its scale was fiction —
and so was its density control, which resized three of those steps and left the
rest.

There is ONE micro-label register (`.t-label`), declared once and never
overridden. Numbers are tabular everywhere: a live token count that changes
width as it counts makes the column beside it jitter.

### Space, radius, elevation

A 4px base scale, with the package's `--density` multiplying every spacing and
size token — rows, controls, gaps — so compact mode is a real change to every
surface. A row is `--size-row-md` (36px), a control `--size-control-md` (30px),
and the sidebar's rows `--size-nav-row` (30px), all the kit's own steps rather
than literals written to line up with them. Four radii and a pill (the previous
system had ten literal radii across 44 declarations). **The card is flat:** it
is found by its rung and its hairline, and a shadow is spent only on what
stands OVER the page — a popover, a dialog, the command palette. The focus
ring is the package's `--shadow-focus`.

The sidebar's inset is the one place that scale is split in two, because a
sidebar row has two edges that want different things. `--size-nav-gutter`
insets the sidebar, and it is where a row's own background, hover and active
tint begin, so it decides how much of the sidebar's width the click target
covers.
`--size-nav-row-pad` insets
the content inside that row. Every glyph in the sidebar therefore lands on the sum
of the two, and anything with no row of its own — the brand lockup, the group
labels — adds them rather than carrying a literal. That is what lets the rows
be widened without moving one glyph: shrink the gutter, grow the pad by the
same step, and the vertical line the mark, the group labels and the item text
share does not move.

---

## Information architecture

**One sidebar, and three nouns that keep it one.** The sidebar lists where a
reader can go — nine **workspaces** — and the few objects they keep coming back
to: the company's projects, the views they pinned, the pages they starred. A
workspace's own **sections** are paths drawn as tabs in its page header
(Settings draws its eight, and a cross-link to Budgets, as a grouped column),
and its objects — a unit, a
container, a node — are rows on the section that lists them.

It replaced two columns of navigation: an 80 px rail of workspaces, and beside
it a 236 px sidebar holding the open workspace's own tree. Every workspace had a
different second column, the reader's place moved between two columns on every
navigation, and the rail spent 97 px of every screen on eight icons. The trees
were never the sidebar's to hold: a unit list is a section of Agents, a node
list a section of Settings.

### The grammar, stated once and asserted

- A **sidebar row** is a workspace, or a live shortcut to a project, a pinned
  view or a starred page.
- A **section** is a **path segment** inside a workspace, drawn by one
  renderer — `tabs` in the page header, or `column` for Settings.
- An **object tab** is a `tab=` query on an object's own page (a seat's
  Overview, Work, Turns…), bound to 1–9.

Nothing is two of those. A section is never a sidebar row and never a query; an
object tab is never a path. A filter — a reason, a scope, a status — is none of
the three and lives in the page's own filter bar. `view=` and `lens=` remain
where a screen switches how ONE object is read (a list's saved view, the
configuration's active / history / diff), which is an object tab by another
name.

`app/nav.ts` holds the one table, `WORKSPACES`, with each workspace's sections;
`DESTINATIONS` (the palette's "Go to") is derived from it. `app/routes.ts`'s
`resolve(path)` is the one route table: the screen dispatch, the breadcrumb,
the stars and recents a reader keeps and the tests all ask it, and a stored
path that stops resolving is dropped when it is read. `router.test.ts` holds
the grammar against the definitions — every destination resolves to the
workspace it declares, chords are unique, no reserved segment has the shape of
a key the engine mints — and holds the table below against the resolver in
both directions.

### A filter is a chip; an arrangement says what it is set to

The grammar above says where a control LIVES. This says what a control is, and
it is the boundary the work screens lost first: a bar of eleven controls, five
of which were pickers drawn whether or not they were set, so the two that were
narrowing looked exactly like the nine that were not — and which of them
appeared depended on the shape, so one switch sat at three different places on
three tabs.

Two kinds, and nothing is both:

- A **filter** narrows the answer. It is added from one **+ Filter** menu at
  the end of the chip row, it is drawn as a removable **chip** in that row, and
  there is no chip for a filter that is off — so an unfiltered list's row is
  "+ Filter" alone. Taking a chip off clears the one URL key it names.
- An **arrangement** decides how the same answer is DRAWN — the shape, the
  grouping, the order, the grid's columns, the board lanes put away — and it
  is drawn as a control that SAYS WHAT IT IS SET TO, so the arrangement is
  readable without opening anything. The three a reader changes all day are
  on screen: the five shapes are a row of pressed buttons ahead of the saved
  views, and **Group by** and **Sort** sit at the far end of the chip bar,
  each showing its value — the default order named for what it is ("Manual",
  "Recently updated", "Start date") rather than "Default". What a reader sets
  once and forgets — the second axis, the grid's columns, what a board's
  cards show, the lanes a board has put away — is the **Display** menu. They
  were all in that one menu for a while, and a menu button cannot say what
  four things are set to: the board, the shape a team lives in, was two
  presses away. Its way back to "this view's own shape" is offered only while
  a SAVED view is running in another shape: over a plain board it pointed at
  a view nobody had open.

  **A board's arrangement is what its cards show** (`card_hide=`, the
  facts put away): size, priority, labels, due date and tokens, each a box
  in the Display menu, ticked while it is drawn. Only the DESCRIPTIVE facts
  are offered. The key, the holder, the title and the STATES — blocked,
  "blocks N", open asks, the live band — are what a board is triaged by, and
  a board that could hide them would report a quiet project that is not one.
  The facts are one declaration (`CARD_FACTS` in `components/work.tsx`) that
  the menu and the card both read.

  **A screen whose grid has an optional column has a menu to reach it.** The
  columns half is one component, `ColumnChooser` in `DataGrid.tsx`, drawn by
  the work list's Display menu and by the projects directory's own **Columns**
  button beside its segment — because `optional` with no chooser is a column
  that can be turned off in the source and never on by anybody, which is what
  the directory's Unit column shipped as for one commit, taking `sort=unit`
  with it. It is one implementation rather than two checkbox lists for the two
  rules that are easy to get wrong separately: an empty `cols=` is the DEFAULT
  set rather than an empty grid, and the value written back is in the columns'
  DECLARATION order rather than the reader's click order, because `cols=`
  carries an order as well as a selection. What each screen keeps is which
  columns exist — per shape on the work list, per grid everywhere else — and
  the choices are DERIVED from the columns the grid draws, so a column added
  appears in its menu with no second edit.

`URL_HOMES` in `lib/work.ts` is where that is declared per key rather than per
paragraph: every URL key the work list carries names the control that owns it —
chip, arrangement, or one of the three that are deliberately neither and carry
their reason at the entry. `routes/work/toolbar/grammar.test.tsx` holds it against
the SCREEN in both directions, reading the key set out of `ItemsView.tsx`
rather than listing it: every key the list puts on the address has a home, and
every home is a key the list actually writes. Both halves fail silently
otherwise — a key with no home is a narrowing reachable only by editing the
address, which `unit=` was for its chip's whole life while the Clear control
walked past it, and a key with two is two controls for one fact that disagree
the first time either writes.

The one control outside both is the **scope** picker (**Show**: Open / Recent /
Closed / All, at the bar's far end beside Group by and Sort), which is always
set to something: as a chip it would either be
permanently present, which is not a chip, or absent on its default, which hides
the one control that decides whether finished work is on screen at all. Its
last value has a NAME (`all`) rather than the empty string, because a key set
to `""` is a key the router deletes — so the scope snapped back to its
fallback on the next render, and the one choice whose whole job is to show
finished work could not be selected. **Recent** is the one scope that is not
a status group: it is open work plus whatever finished since the start of the
week, sent as the engine's own `closed_since=sow` so the week is the COMPANY's
rather than the browser's. The engine refuses it beside `show_closed`, so the
scope the reader chose replaces whichever of the two a saved view carried
rather than adding a second answer to one question. A **board opens on it**,
where every other shape opens on Open: a board's last lane is Done, and under
Open that lane is a column nothing can ever fall into — the workflow drawn with
its end cut off. A view that said anything about finished work keeps what it
said.

**A key that is there and empty is not a key that is missing.** The scope's
third value has a NAME for one half of this; the column narrowing has the other
half, and it cannot borrow the trick. Narrowing a board to one of its columns
writes `group=<value>`, and the column holding the rows with no value on the
axis — Unassigned, Untagged, No parent — has the empty string as its key, on
every axis, which is what the engine reads with `Params.Has`. So no word could
be spelled that some axis will not one day hold as a value, and the honest
spelling is PRESENCE: the address carries `group=` with nothing after it,
`URLSearchParams` round-trips it, and every reader asks whether the key is there
rather than what it says. Written as a plain string it was unreachable end to
end — the query builder dropped an empty value, `useParam` could not tell it
from an absent key, and the address writer deleted it — so the Unassigned
column's own "N more →" loaded the whole board.

A **saved view** is neither: it is a query somebody arranged and put somewhere,
so it is a tab in the strip. The five shapes are not tabs beside it — a shape
is a way of drawing any query, and mixed into one strip the two read as the
same kind of thing. That is also why `view=` and `shape=` are two keys:
switching a saved board to a list must not throw the saved filters away.

**And a narrowing the SCREEN is is not a filter either — it is LOCKED.** One
list serves three screens, and two of them are that list narrowed to something
the reader did not choose: `#/work/{KEY}` is the work in one project, `#/me`'s
Queue is the work one person holds and its Asked by me the work one person is
waiting on an answer about. A locked narrowing is held to four rules, and each
one is a way the same defect appears if it is broken:

- It is **applied under everything else**, after every key the reader set and
  every default a saved view supplied, so nothing can widen the list past the
  thing the screen is about.
- It is **not offered** in the Filter menu. A row there is a second answer to a
  question the screen has already answered, and choosing it writes a key the
  lock overwrites — a control that does nothing.
- It draws **no chip**, because a chip is removable and this is not. A chip
  that will not come off is a control reporting a state the reader cannot
  reach.
- It is **not in the URL**. The screen's own key is what addresses it —
  `#/me?handle=cto`, a project's path segment — so a second key naming the same
  fact is a second place for the answer to be written, and the two can differ.

It follows that a locked narrowing does not make a list "filtered": the empty
state that blames a narrowing and offers to clear it is for a filter that is
actually on, and over a locked one it would name a control the reader has no
way to press. What a host screen says instead is its own sentence, about its
own subject.

### The sparse state

Every screen above is drawn, argued about and reviewed on a company with work
in it. A company's first week is the state every company passes through, and it
is the one where each of these bands is conditional on data that does not exist
yet — so the page a new operator meets is the one nobody designed. Five rules,
each the answer to one thing that went missing at one item:

- **The landing shape is the list.** A board's information is the comparison
  ACROSS its lanes, which makes it the worst shape at low N and the best at
  high N: four lanes holding one card between them say nothing one lane could
  not, and that card is a 292 px object in a 1500 px field. A list degrades to
  one full-width row and is still a list. So a container with no default view
  opens as a list and the board is one press away, the second of the five
  shape buttons ahead of the saved views — and the
  landing shape is never conditional on how much work exists, because a screen
  that redraws itself as a company fills up is a screen nobody can learn.
- **A closed-set axis draws every declared value; an open one draws what
  exists.** The ENGINE decides which lanes there are — `work_items` carries
  every column the query's own predicate admits, so a one-item company gets the
  denominator a board's comparison needs and Open draws no dead Done lane. What
  is the screen's is the DRAWING: an empty lane says "Nothing here" rather than
  standing as a heading over nothing. The rule and the axes it covers are in
  [the work tracker guide](../guides/work-tracker.md#views); a list, a table
  and a timeline draw only the bands that hold rows, which is the bullet under
  "A board draws every lane the scope admits" below.
- **The view strip is always drawn.** Its first tab is the container's own
  list — a real destination, and the only thing that names the page inside its
  own content column — so gating the strip on somebody having saved a query
  left the toolbar as the top edge of the screen. It ends in a link to the
  inventory, because saved views are undiscoverable until somebody has used
  one. The container tab carries no count: the engine's total is over the
  FILTER and not over the container, and a number an answer does not give is
  not drawn.
- **An empty list is a function of what was ASKED.** One sentence answered
  three questions that send a reader to three different places, and it blamed
  filters on a screen with no chip on it at all. So: a narrowing is on and
  nothing matched → "Nothing matches", with the control that clears it rather
  than a description of one; nothing is narrowing and this container has
  nothing in this SCOPE → "Nothing open here", naming the switch, with a number
  only where the container's own counts give one; nothing at all → how work
  arrives. The last one is drawn by whoever holds the wider fact — the page for
  a company with no projects, the project screen for an empty project — and the
  list stays quiet rather than stacking a second panel under it.
- **The end of a complete list is said.** A list that reached its end and one
  that was cut off both ended in rows and then page ground: the count in the
  bar is silent once everything matching is on screen, and a cursor is
  invisible. A complete answer with rows on it closes with "That is all of it ·
  N items". It never says how many a filter hides — `total_hint` counts what
  MATCHED, over the same predicate as the rows, so the unfiltered total is not
  in the answer at all and synthesising it would take a second query at a
  second instant.

**Reserved segments cannot collide with keys.** Project and container keys are
uppercase (`ENG`), item keys are `KEY-n`, everything else the engine mints is a
uuid — and every reserved segment is lowercase. That is what lets `#/work/views`
resolve before any answer arrives. The resolver recognises a key by that shape
rather than by elimination: the engine uppercases every container key it
keeps, so a segment under Knowledge holding a lowercase letter is a section or
Not Found, never a container — `#/knowledge/skills` does not open a container
called `skills` before the screen that section names exists.

The set itself is `RESERVED_SEGMENTS` in `app/nav.ts` and is deliberately NOT
copied out here: a prose list of twenty-one strings is a list that goes stale,
and this one had drifted to fifteen while two of the entries it did name were
reserving route space nothing routed. `router.test.ts` holds the routes named
in the table above against that list and against `WORKSPACES`, so the table
and the code cannot disagree about which addresses exist.

### The sidebar

The order is the product's story: you first — what needs you, what reached
you, what is yours — then the company's work, its agents, what is running,
what it knows and what it spent, and the machine last.

| Row | Route | Figure |
|---|---|---|
| **Home** | `#/home` | — |
| **Inbox** | `#/inbox` | the accent **badge**: unread notices under a reason the person's record counts as PRIMARY — the only filled figure in the chrome. Not every unread notice: most of a busy company's are things it merely told you, nobody answers those, and a count that never reaches zero reads as a broken counter. Asked of the engine as `unread` + `primary_only` over one page, so a page that fills is drawn as a floor ("50+") |
| **My work** | `#/me` | the open work assigned to you — the SAME figure the Queue tab carries on your own day, read once by the frame for both, so the row and the tab can never name two numbers. The engine's own total, counted in full rather than the length of a page and written as a floor ("200+") where the count stopped at its ceiling; nothing on you draws no figure. The questions put to you are counted on their own tab (Asked of me) and each reached you as an Inbox notice |
| **Work** | `#/work` | — |
| **Agents** | `#/agents` | agents working now, beside the working mark |
| **Live** | `#/live` | — |
| **Knowledge** | `#/knowledge` | — |
| **Spend** | `#/spend` | — (tokens only; nothing in this product renders money) |
| **Settings** | `#/settings` | a key mark when no operator credential is presented |

Under the workspaces: **Projects** (every project with its key chip and its
open work — the engine's maintained `task_counts`, waiting plus started — and a
`+` that files a new task in the project the reader is in),
**Pinned** (the views this reader pinned, asked for WITH the viewer — a pin is a
person's, and the shared strip has none — and across EVERY container
(`work_saved_views`), so a view pinned from a project board is here with the
project's key as its lead; each with the total it selects, counted in the
view's own container; a
pinned row RUNS the view, `#/work?view=<key>` or its project's list for a view
saved on a project, rather than opening the inventory page about it — and the
total beside it is that list's own: the engine counts a pinned view with every
task filtered on its own, as every surface that runs a view asks for it, never
in the grammar's tree mode, where an open epic's finished subtasks rode along), and
**Starred** (drawn only when there is one). At the foot, Settings, the
**health card** and the **user block**. The head carries the company's lockup
and, at the end of the same line, the chrome's one create: `+` New task.

**Every "New task" is one sheet.** The sidebar's head `+`, the Projects group's
`+`, a work screen's page-bar New task (Home, Work, a project) and a board
lane's `+` all open the frame's single New task sheet
(`dashboard/src/routes/work/NewTask.tsx`, mounted by the shell out of the Work
chunk), each door telling it only what it knows. The two sidebar doors file into
the project the route is in — its page, or the project a task key names —
asked of the route's own resolver, so `#/work/views` and a task opened by its
uuid are in no project and their `+` reads "New task in a project". A board
lane's `+` files INTO its lane (`app/newTask.ts`'s `presetForLane`): it
presets exactly the value that puts a task there — the lane's status, type,
priority, project, holder or label, and a status-group lane the group's first
status — and is drawn only where the sheet can hold that value as one. A due
band other than "No due date" is a span of days and a unit is the project's own
team, so those lanes take no `+`; nor do the lanes of work that ended
undelivered — Cancelled, Closed and the closed status group — because the
sheet could hold that status and nobody files new work as already abandoned
(Done keeps its `+`: work finished before anybody tracked it is recorded as
done). The Unassigned lane's `+` chooses nobody,
which the project routes as its own unassigned work — to its default assignee
where it names one and the chart still holds that seat, else triage — and
the field says which before the press. An applied create that came
back with a caveat — the engine's `warnings` — says it in its toast rather
than a plain "Filed". The sheet files project, title,
description, type, status, assignee, priority, due and labels as ONE
`create_work_item`, sending only the fields the person set (an unset one is the
engine's default, never the sheet's guess of it); the type, status words and
labels are the chosen project's own, and the assignee completes against the
chart with the engine's `colleague` match offered first — as the **best match**,
its row's hint as short as every other's (`@handle · best match`), because the
kit's hint does not shrink and its label does: the tier written into the hint
drew the seat's name as "Age…", or not at all. Why it matched is in the row's
accessible name and, once that seat is taken, on the field's help line ("The
engine's match for “front”: part of the name matches"). The field's clear
control is the kit's own, inside the field as a search box draws it, so the
completion list — anchored to the field's box — spans everything it hangs
from; beside the field it was a column the list did not cover, and the help
line's last words showed next to the open list. Words typed in the
Assignee field are not a seat until one is taken from the list: while they are
unchosen the field says so, Create is held with "Choose who “…” is from the
Assignee list, or clear it", and Enter opens the list on them — a name left
typed is never silently dropped from the create, nor sent as a handle. Left
empty, the field says where the task lands — the project's default assignee
(which the engine applies inside the create), else triage for its lead. Every
field's help line is its control's description, so a screen reader hears it
with the field. `applied` opens the new task by the
key the engine minted; `pending` closes with the not-yet-applied toast; a
refusal stays in the sheet in the engine's own sentence, which names the
argument it refused (drawn as a value, not in backticks). What the engine would
refuse is said on the field BEFORE the press: the title and the description are
counted in the engine's unit — UTF-8 bytes, against `tracker.MaxTitle` (256) and
`MaxBody` (32 KiB), held by `contract/work.ts` — counting down in the last fifth
and marked invalid past the cap, saying by how much. Create is then held, and
the reason is written in the sheet's foot beside it, because the kit reads a
held button's reason to a screen reader only; Enter while held takes focus to
the field that is holding it. Enter in any single-line field is the press, and
the sheet says so itself: the kit's sheet is a form, and a browser submits a
form of several fields on Enter only by pressing its submit button, which a
write control is not — so Enter did nothing there, held or not. A description
takes its newline, and Enter that a completion list took chooses from it. The
same place says, while nothing holds the
press, who the task is filed as ("Filed as Jane Founder") — and the head is the
title alone, as the kit's sheet head (the page bar's height) is sized for.

**Settings is never hidden.** A section that vanishes without a credential is
indistinguishable from one that does not exist, so an operator on a fresh
browser would conclude the product has nowhere to configure anything. General — the
charter — and Tools & MCP — the registry every reader is pushed — are readable
by anybody; the key mark says the rest needs an operator. It is a KEY rather than
a padlock because the kit's glyph set carries no padlock, and a key names what
is missing: a credential.

**A guarded section says so and nothing else.** Opened by a reader the engine
has SAID holds no operator credential, a guarded section is replaced by one
refusal — "Nodes needs an operator credential", and a button to set a token —
drawn by the frame, before the screen mounts. A screen that mounted anyway was
refused and drew the refusal as data: "0 nodes" in its header, four tiles of 0
and "No nodes are reporting", under a banner calling the refusal the last
reading that succeeded, while the Settings column beside it counted one node.
The section waits for the frame's FIRST answer about the reader — one round
trip, behind a placeholder — so it asks nothing it may be refused; a cold load
of Secrets used to send its guarded reads, be refused, and draw the refusal a
frame before the frame knew to. A viewer read that FAILED is no answer and
does not hold the section — treating it as "no credential" would lock an
operator out until the next poll — so the screen mounts, and the guarded
screens draw no figure from a read that never succeeded. Edit org is
the one guarded section that answers its own refusal, because its draft is
forgotten on a token change only a mounted builder hears.

**A figure beside a row says what it counts.** The kit's `NavItem` takes a
figure only with the words that name it, and two kinds of figure are two
props: a **badge** is how many things are waiting on the reader, and a
**count** is how many of something the destination holds. A figure the engine
did not answer is absent, never a zero.

**Who the reader is, and how much is waiting on them, are read ONCE.** The
frame asks both — the `viewer` and the Inbox count — and the sidebar, Home's
status line and the Inbox's own band all draw that one answer. Each of them
asking for itself was a standing query per surface: several of the socket's
four query slots held before a screen's first read could run, and the badge
and the sentence beside it polled on separate minutes and could name two
numbers. A surface mounted outside the frame is refused rather than handed a
read of its own, because that fallback would bring the per-surface reads back
without a sound.

**The health card** says the one state that decides what a reader should do,
in precedence order: the token was refused (the card is then the button that
asks for another), reconnecting, draining, no configuration, a posture that
diverged from the fleet (`shed`, `stuck`, `isolated`), no health push yet
("Waiting for the engine" — nothing reported is not healthy), and serving —
"3 nodes · config epoch 42", or "node count unavailable" where the presence
read failed. The dot is the POSTURE and nothing else: a serving node has a
success dot whatever its alarms say — and the dot sits on the TITLE's line,
sharing its grid row, rather than centred on a card whose height the alarm line
grows. The title is "Engine healthy" only when the alarm table was evaluated
and nothing in it fires, and "Engine serving" otherwise, because "healthy"
directly above "5 alarms" is a claim the line under it refutes.
The alarm line beneath is a second fact with its own tone and glyph — the
health push's `alarms`, a count and the alarm that has stood longest ("5
alarms · oldest: backup age") in warning ink beside a warning triangle, never
the rows, because the push is public; a push with no alarm table says "Alarms
not evaluated yet" in plain text, because an unevaluated table is neither an
empty one nor a fault. Recolouring the dot for an alarm drew a green title
beside an amber dot, and a title such as "serving, with alarms" only repeats
the alarm line. Everything comes off the one health push; nothing polls.

**The user block** is who this browser is — a person (bound: their name and
seat, linking to it), a token no seat claims (unbound: the token's id, and what
would bind it), or nobody (anonymous) — beside the theme flip and the
**preferences**: theme (light, dark, match the system), density, the zone
timestamps are drawn in (`Intl.supportedValuesOf` plus UTC, which the runtime's
canonical list omits, or the browser's own), how a date is written, and the
token. All of them are per browser and none is the company's: every key the
dashboard keeps in browser storage is declared in one table
(`lib/storage.ts`), and a key no build reads any more is listed there as
retired and removed at boot, so a stale value does not sit in a reader's
profile for ever. The zone is the one every timestamp is drawn in AND the one a day is
filed in — a calendar cell and a timeline column are the chosen zone's date, so
a task stamped "Sep 23" is never in the cell for the 22nd — while a day's
arithmetic (the grid, the span between two dates) is civil and moves with no
zone. There is no company switcher: one engine runs one company.

**The lockup is the company**, beside the product's mark: "Nimbus", on one
line, and the way home. The product's name stands in only while no company has
been sent. The kit's own default puts the product first and the company under
it; on this screen the company is the subject, and the product is in the mark
and the tab.

`g` then a letter jumps to a workspace (`g h`, `g i`, `g m`, `g w`, `g a`,
`g l`, `g k`, `g t`, `g s`). A chord rather than a modifier, because every
single-modifier combination worth having is already the browser's. Every key
the dashboard answers is in [one table](#the-keys), and `?` shows it.

### The routes

Only a route a screen draws is written here, and `router.test.ts` holds this
table against `routes.ts`'s resolver both ways: every address below resolves to
a screen, and every workspace and section the code declares is below.

| Route | Page | Params |
|---|---|---|
| `#/` · `#/home` | **Home** — the landing screen: the company's day, the engine's one sentence, the pulse figures and what needs a decision | |
| `#/inbox` | **Inbox** — what waits on your decision, and what reached you and why | `scope=unread\|all\|snoozed` · `reason=decisions\|reviews\|mentions\|assigned` (the chip) · `row=` (which row the pane is on) |
| `#/me` | **My work › Queue** — what one person holds, by due date or in the order somebody put it, reordered by dragging a row | `order=due\|priorities` · `handle=` (an operator reading somebody else's day, kept across the sections) · on the queue, `shape=`, `cols.list=` / `cols.table=` and the filter grammar with the assignee LOCKED |
| `#/me/asked-of-me` · `#/me/unblocked` · `#/me/collaborating` · `#/me/watching` · `#/me/checklist` | **My work** — Asked of me (answered in place) · Unblocked · Collaborating · Watching · Checklist, each with the engine's own total on its tab | `handle=` |
| `#/me/asked-by-me` | **My work › Asked by me** — the work this person asked a question on that is still waiting for its answer: the work list, held to the asker | `handle=` · `shape=` and the filter grammar, with the asker LOCKED |
| `#/work` | **Work › Tasks** — every task, in one list | `view=` (a saved view) · `shape=list\|board\|calendar\|timeline\|table` · `cols.list=` / `cols.table=` + the filter grammar |
| `#/work/projects` | **Projects** — the directory: every project, its lead, its target date and its four counts. The segment and the sort are the ENGINE's question (`shown=` becomes `archived=`, `sort=` travels as written), because the answer stops at the engine's own 200. **Unit** is the one optional column, reached through the **Columns** menu | `shown=active\|archived\|all` · `sort=key\|name\|unit\|todo\|active\|done\|closed\|last_change\|target`, `-` for descending · `cols=` |
| `#/work/views` · `#/work/views/{id}` | **Saved views** — the inventory, and one view run | |
| `#/work/history` | **Every change** — the tracker's own log, on the log frame | `window=1d\|7d\|30d\|90d\|<from>/<to>` · `kind=` · `actor=` · `project=` |
| `#/work/search` | **Search** — the company's work ranked against a phrase | `q=` `mode=hybrid\|keyword\|semantic` |
| `#/work/{KEY}` | **Project** | `lens=items\|about\|history` · the same view strip and filter grammar, scoped to the project |
| `#/work/{KEY}-{n}` · `#/work/{id}` | **Task** — description, checklists, sub-tasks, activity (changes, comments, agent turns), properties and cost | `activity=all\|comments\|turns\|changes` · `list=` |
| `#/agents` | **Agents › Org chart** — the hierarchy every seat works inside | `unit=` · `seat=` |
| `#/agents/roster` | **Roster** — every seat, and who is carrying how much | `view=seats\|workload` · `group=state\|unit\|flat` · `q=` |
| `#/agents/teams` · `#/agents/teams/{unit}` | **Teams** — every unit with what it is for and its goals; one unit's page | |
| `#/agents/schedules` · `#/agents/schedules/{scope_type}/{scope_id}/{name}` | **Schedules** — recurring work; one schedule | |
| `#/agents/edit` | **Edit org** — the builder, opened from the chart's button *(operator)* | `view=visualization\|table` · `chart=structure\|reporting` · `unit=` · `seat=` (the selection; arriving with one opens its editor) · `add=unit\|agent\|human` (opens the Add once, then leaves the address) |
| `#/agents/seats/{handle}` | **Seat** — an agent's or a person's profile. Handles live only under `seats/` | agent: `tab=overview\|work\|turns\|memory\|schedules\|settings` · `conversation=` (Memory); human: overview · work · settings |
| `#/live` | **Live › Now running** — the running turns, the coding runs waiting on a person and the rest in a box, the activity strip and the recent phases | `window=15m\|1h\|6h` (the activity strip) · `seat=` (a handle) · `phase=` · `failed=true` (the same spelling Turns uses) |
| `#/live/turns` · `#/live/turns/{id}` | **Turns** — the turns that ended over the window, counted by the engine, then every turn one row each; one turn | `window=1h\|6h\|1d\|7d\|30d\|<from>/<to>` · `seat=` (a handle) · `failed=true\|false` · `sort=-started\|-tokens` (the engine's order) |
| `#/live/runs` · `#/live/runs/{turn_id}` | **Coding runs** — live and durable; a run's own page draws the run alone, answered in place, and a collected run's page reads its turn | |
| `#/live/a2a` · `#/live/a2a/{id}` | **Agent-to-agent** — a channel's own page draws the channel and what crossed it | |
| `#/live/traces/{id}` | **Trace** — one distributed trace. NO LIST: nothing enumerates traces, so a bare traces address is Not Found, saying a trace is opened from a turn, a run or an event | |
| `#/live/events` · `#/live/events/{id}` | **Event log** — the time axis, then the rows | `window=1h\|6h\|1d\|7d\|30d\|<from>/<to>` · `category=` · `actor=` · `seat=` · `trace=` · `channel=` · `q=` · `failed=true` |
| `#/knowledge` | **Knowledge** — the ranked search, or with no phrase the spaces at a glance; beside every Knowledge screen, the tree (search, mode, spaces) | `q=` · `mode=hybrid\|keyword\|semantic` (Hybrid, Keyword, Meaning) |
| `#/knowledge/{CONTAINER}` | **Container** — browse the tree | `kind=prose\|skills\|all` |
| `#/knowledge/pages/{id}` | **Page** — addressed by its id, which a rename does not change: the document, who read it and how, what links to it, its revisions and thread; edited as you | `edit=1` · `version=` (one revision in the document's place) · `lens=diff\|full` |
| `#/knowledge/skills` | **Agent skills** — the tool skills the engine offers a phase, who loaded each; and what one agent learned | `kind=pages\|learned` · `seat=` (learned) |
| `#/knowledge/diaries` · `#/knowledge/diaries/{handle}` | **Agent diaries** — every agent's diary at a glance, each counted by the node holding the agent; one agent's diary and episodes. Handles live here as they do under `seats/` | |
| `#/spend` | **Spend › Overview** — the window's tokens against the one before, the monthly budget, the prompt-cache share and the median task; daily tokens by phase (or model, provider, seat, unit, worker); then by agent, by provider, by team, background workers and the three most expensive tasks (tokens only) | `window=7d\|30d\|90d\|<from>/<to>` (whole company days, kept across the sections) · `group=phase\|model\|provider\|seat\|unit\|worker` |
| `#/spend/tasks` | **Expensive tasks** — the tasks last changed inside the window, most tokens first, each with what drove it: turns, workers, reopens and send-backs | `window=7d\|30d\|90d\|<from>/<to>` (kept across the sections) |
| `#/spend/budgets` | **Budgets** — the company's and every agent seat's day, week and month: spent, the ceiling (raised in place by an operator), and what is refusing. ONE address: Settings lists it as a cross-link | `window=` is not read here, and is carried through to the other sections |
| `#/settings` | **Settings › General** — the charter: mission, vision, policies | |
| `#/settings/people` | **People & access** — every human seat, where agents reach them and whether they act here as themselves; every API token by label, the person it acts as and its scope. Never a value *(operator)*. No tail: a person's page is their seat | |
| `#/settings/integrations` · `#/settings/integrations/{kind}` | **Integrations** *(operator)* | |
| `#/settings/tools` · `#/settings/tools/{tool}` · `#/settings/tools/servers/{name}` | **Tools & MCP** — every MCP server with what each node did with it, and every tool a seat can call, by origin. Not guarded: the registry and who holds each tool are pushes every reader gets; only the servers' status (`mcp_servers_status`) is the operator's, and its section says so in its own place. ONE tail segment is a tool and two are an origin filter, discriminated on LENGTH, because a tool name is a third party's string | `q=` · `origin=` · `add=server` (the add form) |
| `#/settings/models` · `#/settings/models/{id}` | **Models & keys** — every `providers.llm` entry, the keys it rotates through by variable name, which a vendor is refusing and when each comes back (`credential_pool`), and the seats whose chain names it; one model's keys whole and its seats, and its **Edit** *(operator)*. `{id}` is the entry's config key, the name a seat's `llm:` writes | |
| `#/settings/secrets` · `#/settings/secrets/{name}` | **Secrets** — names and provenance, never values *(operator)* | |
| `#/settings/nodes` · `#/settings/nodes/{node}` | **Nodes** — leases (each seat's holder and **since** when), duties, config rollout, the fleet broker's members as the nodes advertise them and as its metadata group counts them, and where the company's files are kept (the object store's backend and its collector's last passes) *(operator)* | |
| `#/settings/config` · `#/settings/config/revisions` · `#/settings/config/revisions/{id}` | **Configuration** *(operator)* — `revisions` lands on the History lens; one revision's page draws no lenses | `lens=active\|entities\|audit\|diff` |
| `#/settings/backups` · `#/settings/backups/{domain}` | **Backups & retention** — take a backup, what the fleet has backed up and the backup history, each state-log domain and what holds its trim; one domain *(operator)*. Domains live only under `backups/` | |
| `#/settings/audit` | **Audit log** — every write a person or a token made, every call they made at runtime, and every configuration revision labelled with the kind of writer it recorded (`operator` or `node`) *(operator)*. No detail route | `window=` · `actor=` · `kind=work\|knowledge\|config\|credentials\|runtime` |

**There is no redirect table.** There was one, and it was always a liability: a
redirect whose old path is now a live route sends every reader of that route
somewhere else, permanently, with the address bar agreeing with them — which is
strictly worse than the dead link it exists to avoid, because a dead link is
visible. It happened once, when `#/work` still meant a coding run. No `v*` tag
has ever shipped a route from this tree, so there is nobody holding an old link:
an address from before the one-sidebar rebuild (one under the retired `company`,
`activity`, `cost` or `admin` heads) is Not Found, which names what was asked for, says
where the product knows why there is nothing there, and offers the way home.

**An object's segment is decided by its shape, never by a lookup**, so every
address resolves before any answer arrives. A project or container key is
uppercase, a task is `KEY-n` or its uuid, and a page is its uuid — so a word in
one of those positions (`tasks` after `work`, `runbook` after `knowledge/pages`)
is Not Found rather than an object the engine never minted, which would draw its
refusal under a trail naming a page that does not exist.

**Three of those surfaces are the engine answering a question it has always
been able to answer.** Search is the ranking a seat gets from `search_work_items` —
by the words (Keyword, BM25 over the engine's own index), by what they mean
(Meaning, the wire's `semantic`), or both fused (Hybrid, the default) — which
the operator reading the same company had no access to at all; the board's `q=`
is an escaped substring over an excerpt and answers something else. The mode is
`mode=` in the address, and an answer served in a different mode than asked
(`served_mode`, from the engine's own `degraded` value — a company with no
embeddings provider asking Hybrid is served Keyword) says so above the hits.
The phrase field never gives way to the modes beside it: the form wraps by what
it holds, so on a phone the field takes a line of its own and the modes and
Search the next. A hit's snippet is a cut of a markdown body and is drawn as
the prose it renders to, without its `**` and `#` (the knowledge search's
snippets too). The item's **Woke** tab is who one change
actually reached and under which of eighteen reasons, which is the fact no
commercial tracker records: all of them can say you were notified and none can
say why. And a seat's **Conversations** tab is its own thread ledger — the only
account of what a seat said on a surface this engine does not own. Every one of
those readers was written, tested and swept on a retention horizon before any
of them reached a screen.

### `window=` — one vocabulary for every time range

Every screen with a range had its own. Spend's picker counted whole days, the
live strip was a `STRIP_MINUTES` constant nobody could change, and the event log
showed "whatever happens to be loaded". Three screens showing the same hour
disagreed about how long an hour was, and a link from one to another carried no
range at all.

There is now **one key and one control**. The value is a duration from a closed
set — `15m` `1h` `6h` `1d` `7d` `30d` `90d` — or `today`, or an explicit
interval written as ISO 8601's own `<from>/<to>`, so a custom window is still
one value a reader can copy out of the address bar. The custom window's Apply
is its form's submit button, so Enter in either box applies it — as a plain
button it left Enter doing nothing, for the reason the New task sheet gives.

**A screen of whole company days** (`Offer.customDays` — Spend) puts every edge
on a company midnight: `30d` is the thirty company days ending today, exactly as
the engine answers `days=30`, running to the first instant after today — never
a span aligned to UTC midnights, which in Berlin ended at 02:00 tomorrow for
twenty-two hours of every day. The custom dialog is prefilled from those edges,
so opening it on `30d` shows the thirty dates the chart is drawn over and
Apply without an edit asks for the same window. The window before is the same
number of days, counted in days rather than hours across a clock change.

**`today` is the company's day so far**, cut at the first instant of the date on
the company's clock (`org.timezone`, the calendar `internal/period` keeps) —
not the browser's midnight and not "the last 24 hours". The first instant, not
00:00: in a zone that moves its clock across midnight the day starts at 01:00
(Santiago) or at the first of two midnights (Amman), and a "Today" that
disagreed with the engine's day by an hour would count a turn the engine
charged to yesterday. It moves with the clock like a duration, is charted in
hours from its first, and its "previous" window is the same span before
midnight. A screen offers it like any other window; the control draws it
first.

**The offered set is the screen's, not the vocabulary's.** A spend chart has
nothing useful to say about fifteen minutes; a strip folded from the events the
browser is holding has nothing to say about ninety days, and no honest answer at
all for an interval that ended last Tuesday. Each screen declares which windows
it has and whether an interval is one of them, and a URL naming anything else
gets that screen's own default — because a value the control cannot show would
put a heading nobody chose over the rows, and a reader who then touched the
control could never get back to it. The control renders the offered set in the
vocabulary's order either way, so `1d` reads the same on every screen.

`window=` is a **section**, not a filter: a reader who widened the range and
pressed Back means the narrower one.

The two edges reach the engine as the half-open `since`/`until` pair every
windowed question takes — except on Spend, whose named windows are whole
company days the engine cuts on the company's clock, so the screen sends
`days` and offers no interval of two instants. A chart's edges are rounded **up to the bucket it
draws**, so the hour in progress is on the chart while it is still being spent
and the query changes once per column rather than once per second; a list's are
not. An interval never moves at all — it is the one window that is stable to
link to.

### Every list is one grid

`DataGrid` draws all of them. There is no `<table>` left in the product except
the retention screen's per-domain terms — a block that already titled itself,
with no header row and nothing to sort, which is a table in the sense the
element means rather than a list of objects.

**Including the tracker's own list and table, which are one grid with two
column sets.** They were two components over one answer, and the cost was
exactly what two renderers of one row always costs: the list had none of the
rules this section states — one track list for the whole grid, the cap on a
shrink column, the card a row becomes below 640px, the cursor `j` and `k`
walk — and the table had none of the list's, so it drew an EMPTY band per
group the moment a second axis was asked for, because sub-groups replace a
group's rows and it read only `rows`. The shape now picks a **column set** and
nothing else: same fetch, same bands, same head, same phone layout. A set is a
default (which columns are on with nothing chosen), an order, and how a value
is drawn — the list's priority is the bare mark that opens its row and the
table's is the word in a column you can sort.

**`cols=` therefore selects WITHIN the active set, and is keyed per shape** —
`cols.list=` and `cols.table=`. Not because a name might be one the other set
lacks; a grid already drops a name its columns do not hold, and falls back to
its default when nothing survives. It is because `cols=` carries an **order**
as well as a selection, and the order is the set's own: one key read against
both sets draws the list's columns in the table's arrangement, which is a row
nobody asked for and which no validation catches, since every name in it is
legal in both. Per shape, each arrangement survives the other, and an address
says which set its columns belong to. The **sort** is deliberately not keyed
that way: it is a fact about the QUESTION — the engine orders the whole set
the same way whatever draws it — so both shapes share one `sort=`.

**The sort is in the URL**, which is the rule the component exists for: a
sorted ops table that cannot be sent to anybody, does not survive a reload and
comes back unsorted from Back is sorted for one person for one minute.

**A grid too narrow for its columns gives some up, and says which.** The wrap
clips (it is what keeps the sticky head working), so columns that overrun it
were cut off at the edge with nothing to scroll — or, with every flexible
track floored at zero, the one column that is the point of the list was
squeezed to nothing: the work list beside a peek at 1280 drew its titles 70px
wide. So a column may declare a `floor` (the work title's is `16rem`), and the
columns that can go declare the order they go in (`drop`, lowest first): when
the tracks overrun the box — or a content-sized column is squeezed below its
fifth of the grid and cuts a value short ("T…" for a status), the quieter
failure — the next goes, until they fit or nothing droppable is left; when the
box grows, every column gets another chance. The foot says
"Hidden to fit: Updated and Due", because a column that vanished without a
word is a value the reader cannot tell was ever there. The work sets give up
what a task's own page answers one click away — when it last moved, then the
dates and sizes a reader switched on, then the due date, then the marks — and
never the key, the status, the title or who holds it — and the status word is
drawn whole rather than cut to a column's share. The schedules' grids keep a
schedule's name whole (`12rem`, a slug like `morning-tracker-review`) in both:
beside a peek at 1280 the recent runs drew it 35px wide ("b.") as the one
flexible column among five sized to their content, so a run gives up its Scope
first (a role schedule's scope is the seat it woke), then the tick it was for,
then whom it woke. None of it happens on a
phone, where a row is a card that shows every value.

**The row a peek is open on is marked, in every grid.** The grid compares
ADDRESSES: a row's link is its object's page, and the open peek names an
object whose page is the same `peekHref` every such link is built with — so
the tint lands on exactly the row the rail describes and cannot name a
different one, and the row's link carries `aria-current` so the mark is said
as well as painted. Four screens once spelled the mark for themselves and the
rest never did, so beside a peek on Nodes, Secrets, a seat's turns or a spend
table nothing said which row the rail was about. A mark that is not the peek —
the credential a path addresses, the revision the Diff lens reads — is the
screen's own (`isSelected`).

**A band may hold bands.** A second grouping is a heading under a heading,
which is what a second axis means where every row is a line, and a band
carries rows OR sub-bands rather than both — sub-groups replace a group's rows
exactly as groups replace the ungrouped ones. A band counts what is under it,
its sub-bands included, and a band whose page of rows is empty still draws its
heading: "Ada Okonkwo · 0 of 3" is an answer, where the empty state drawn over
it is a second and false one.

**A screen's primary grid takes `sort=` and every other one takes
`sort.<name>=`.** Several screens carry two or three — the spend by seat and
the recent turns, a node's leases and its duties — and unnamed they would all
read the one key: sorting the lower one would silently re-sort the upper, and a
link to a sorted screen would mean something different depending on which table
the reader had touched. A name rather than an index, because an index is a fact
about the source order and inserting a grid above would move every link's
meaning by one.

**A `shrink` column is capped, because `max-content` is not "shrink to
content" — it is *grow* to content, with no ceiling.** Grid resolves an
intrinsic track before it gives anything to a flexible one, so a single long
value takes whatever it likes and every flexible column collapses to zero. The
schedules grid drew its Wakes column at 502px of an 822px grid with **Name and
Task at 0px** — at every width, not just a narrow one — and still ran 317px
past a box that clips; the work table at 1000px drew Assignee at 220px with the
**title** at zero. A shrink track is `fit-content` of a fraction of the grid
now, so a column under the cap is untouched and only one that would take more
than its share gives way. One fraction for every grid rather than a pixel floor
per column, which is a number that would have to be invented nineteen times and
re-invented at every width.

**Below 860px a row is a card, because 390px has no columns.** The audit's six
want 808px between them and the work trash's twelve want more, and the grid's
wrap is `overflow: clip` — the same decision that keeps the head sticky — so
everything past the viewport was cut off with nothing to scroll to: WHAT, TO
and DETAIL simply did not exist on a phone. Scrolling sideways is not the way
out either, which is worth saying because it is the obvious fix: the flexible
tracks are `minmax(0, 1fr)`, so the moment the wrap becomes a scroller its
content box is the viewport, the content-sized columns alone exceed it, and
every flexible track resolves to zero — the title column vanishing to make room
for a due date.

So the head goes and each cell draws its own name beside its value. Four
things follow from that, and each is a rule a new column has to keep:

- **A column with no word in its head declares one.** `header` is what the head
  row draws, so a twenty-pixel column carries a mark or nothing — a work item's
  type, a row's actions, a pair of state tags. A card has no head to explain
  it, so the column supplies the word separately in `label`, drawn only here.
  Without it the card gets a bare mark on a line of its own between two
  labelled ones, which reads as a rendering fault rather than as a value.
  `app/source.test.ts` is what stops one reaching a screen.
- **A cell with nothing in it is not a line.** A column draws no value on a row
  that has none — `PriorityMark` renders null for `none`, which a task nobody
  prioritised carries, and every mark whose rule is "nothing is drawn for no
  value" does the same. In the table that is an empty track under a head, which is correct
  and is what keeps the row's columns lined up. Here the head is gone and the
  label is the CELL's own, so the card opened with `PRIORITY` alone on a line
  with nothing beside it, on every ordinary row — the labelled form of the
  dashed placeholder the board card already stopped drawing. `.grid-cell:empty`
  is what drops it, and it is `:empty` rather than a column predicate because
  the constraint here is the opposite of the board card's: a container cannot
  ask a child that drew nothing whether it did, and the element has to stay in
  the DOM regardless, since the wide layout's tracks are positional and
  dropping it would move every later value one column left. The browser has
  already rendered it, so `:empty` is it answering — and generated content is
  not a child node, so the label's own `::before` does not defeat it. A cell
  drawing a marked absence has content and keeps its line, which is the
  distinction `EmptyValue` exists to make.
- **A value wraps rather than being cut.** `.truncate` is right for a cell that
  IS one line of a fixed column and wrong once the column is gone: at 390px it
  cut the one field the reader opened the list for and left the rest of the
  card empty underneath it. What bounds the value instead is the engine — a
  title is at most `tracker.MaxTitle`.
- **A head longer than the label column wraps inside it.** The label is a
  fixed 7rem — 112px — so every value in a card starts at the same place with
  no track to line them up with, and 7rem is what the heads measure: the widest
  that fits on one line is CONVERSATION at 93.09px. A basis on a line that
  cannot break is a request rather than a width, though: `min-width` on a flex
  item is `auto`, which floors it at its own content, so the one head over the
  basis grew its label box to 128.03px and started only ITS value 16.03px right
  of every other value in the card — measured at 390px on the provisioning
  passes, five values 136px from the card's edge and WHAT IT CONCLUDED's at
  152.03px. The label wraps instead, and breaks mid-word where it has nowhere
  else to break, which is the trade `.num-col` takes one layout over: a heading
  longer than its column wraps, visibly, and the column holds.
- **The sort control goes with the head.** That is the real cost of the shape
  and it is the right trade: a column head a reader cannot see is not an
  affordance, and `sort=` is in the URL — so a sorted list still arrives sorted
  from a link, or from the wider layout that set it.

**A footnote may not set its column's width.** A `.fact` is a column flex box,
so it is as wide as its widest child — and a note is prose where the value
above it is a word. Measured on a knowledge page's header: `v1` under a 146px
"set by Agent CEO · 2m ago" and `2m ago` under a 153px "any change, not only a
save", beside three note-less facts at 66 to 75px, so the gaps between the five
labels ran 85, 162, 90, 169 and the row read as five columns placed at random.
A note is held to sixteen characters — about two short words a line at
`--font-size-2xs`, roughly the width of the label above it — which brought those gaps
to 85, 144, 90, 144. `text-wrap: balance` is what makes the wrap look
deliberate: at a hard 18ch the same two notes left "2m ago" and "a save" alone
on a second line, and an orphan reads as a fault. This is deliberately not
`--size-measure`: that is a reading measure for prose somebody sits down with, and a
caption under a fact is read in one glance beside the thing it qualifies.

**A trail of one segment is not a trail.** The knowledge page draws its
ancestor chain above the header, outermost first, because a title alone says
nothing about which team's tree a page is in. On a page filed directly in its
container — which is most of them — that chain is the container and nothing
else: a lone accent word in an otherwise empty band, reading as a stray button,
saying exactly what the `Container` fact three lines below it already says as a
link to the same place. The trail is drawn only once it has what the fact
cannot carry, which is the path THROUGH the tree.

And the guard wraps the NOTE, not its contents. `PageNote` renders its
paragraph whatever it is handed, and that paragraph carries a margin of its
own — so a guard on the breadcrumb inside it swaps a stray link for an empty
band, which is the same gap with nothing in it. A test that counts the links
cannot see that, so the element is asserted separately.

**A person in a grid cell is a RESOLVED NAME.** A handle is the database's word
for somebody — it is what a filter takes, what a payload carries and what a
column stores — and it is not what anybody is called. Every cell that draws a
person therefore resolves it through the org chart and prints the NAME, with
four clauses that travel together because each one was got wrong separately:

- **The handle is the fallback, never a blank.** A seat the chart no longer has
  still names somebody, so an unresolved handle is drawn as the handle. A blank
  there would read as "nobody", which the same column already has its own mark
  for — and those are different facts: nobody holds this, versus somebody the
  chart has lost.
- **The hover title carries whichever one the cell does not print.** A cell
  that prints the name titles the HANDLE, because that is what an operator
  types into a filter and resolving the name must not destroy the identifier it
  was resolved from; a cell that prints no word — the compact row's badge —
  titles the NAME, because there the title is the only place the answer is.
  Either way the cell says more than it draws, and neither spends the title on
  what is already on screen.
- **Where an avatar is drawn it is initials on a NEUTRAL badge**, built from
  the resolved name rather than from the handle — initials off a handle make
  every seat whose handle begins with the same letter the same mark. It takes
  no colour, by [the one rule](#the-one-rule): a seat is identity, and identity
  is carried by the name, the mark and the position. The only variant it has is
  STRUCTURAL — its outline, a circle for a person and a squircle for an agent —
  and **every** cell that draws a person resolves the kind and draws it. That is the same resolution as the name and it arrives with it:
  `seatLookup` answers both, the row chrome carries both, and the two cells a
  screen hands them to (`SeatCell`, `SeatChip`) take the same `kind` prop, so a
  caller cannot thread the name and drop the kind. Half-threaded is what this
  was: `SeatCell` took the kind and the compact `Assignee` had no way to be
  handed one, so one human seat was a person in the table column set and an
  agent in the list — on the same grid, over the same row — while eighteen of
  the nineteen seat chips passed nothing at all. A handle the chart does not
  hold is resolved from the RECORD next: every change row and comment carries
  its writer's `actor_kind`/`author_kind`, and `kindWithAuthors` layers those
  under the chart's answer — `human` and `operator` draw a person, `agent` an
  agent. That is what an operator needs: a write through an operator token is
  authored by the TOKEN's name, which no chart lists, and drawn from the chart
  alone the founder who filed a task was a squircle. A handle with neither
  answer takes the kit's default, the agent's squircle — the kit has no third
  outline — and is never drawn as a person, since a circle there would claim an
  answer nothing gave. Inside a row that is itself a link, `SeatLabel` draws
  the same badge and name with no anchor of its own, so a seat column is never
  a processor glyph where every other surface draws the seat's badge. The two places that pass a literal `agent` are the
  ones where the chart cannot be wrong — a turn's seat and a coding run's seat,
  which the engine RUNS, and it never runs a human seat — and they say so at
  the call rather than leaving the default to stand for a kind nobody threaded. A
  badge drawn BESIDE the printed name is decorative, or the row reads "Ada
  Lovelace avatar, Ada Lovelace"; a badge drawn ALONE keeps its accessible
  name, or the columns that draw no word announce the assignee as nothing at
  all.
- **The KIND is marked only where it is not the ordinary one.** Who wrote
  something is a person and a kind — `agent`, `human`, `operator`, `system` —
  and `agent` is what nearly every write in an agent company is. A mark on
  every row separates nothing: the tag exists so that the writes that were NOT
  an agent's stand out. The word checked against is the engine's own, and each cell spelling it
  separately is how one of them came to check for a kind the engine does not
  mint and mark every row in its column.

The cells this governs are the frame's `SeatCell`, the shared `SeatChip`, the
tracker's compact `Assignee`, the Projects directory's Lead and Last change,
the trash's `removed_by`, the log's actor, a schedule's scope and every fire it
woke, a saved view's owner, a page's watchers and commenters and a delivery's
recipient — and the rule is written here rather than at any one of them because
the whole point is that the same person looks the same on every screen they
appear on. `components/work.test.tsx` and `routes/work/shapes/Grid.test.tsx`
hold the outline over the pieces and over the grid's two column sets.

### A row of numbers shares its lines

A stat row's tiles are one grid's columns, and **every figure in a row stands
at one height whatever its label does**. Each tile is a subgrid over the row's
three lines — label, number, sub — so a line is as tall as the tallest of it
across the row: a label that wraps makes the label line two lines tall for
every tile, and each other label sits at its foot, one gap above its own
number. Stacked per tile, a wrapped label pushed its own number down and no
other — beside a node's peek at 1440, "Behind on config" broke over two lines
and its figure sat 18px below the other three. Where the kit folds a row to
two columns (below 1024px) an odd last tile takes the whole row, and a sub
wraps rather than ending in an ellipsis. `styles/stats.test.tsx` holds the
span to the lines the kit's tile renders.

### A dropdown's list sizes to its options

The product has one dropdown, `@crewlethq/ui`'s `Select` in its listbox mode,
and for a while this tree carried three rules compensating for it. All three
are gone: they were fixed in the package, and `0.4.4` was the first release
that carried them.

What they were is worth keeping, because the shapes recur. A row in the panel
shrank below its own content — an explicit `min-height` on a flex item replaces
the automatic content minimum, so a list taller than its panel squeezed every
row before it scrolled, and the overflow drew over the next row. An option's
label declared `overflow: hidden` and `text-overflow: ellipsis` and no
`white-space`, and neither of the first two does anything to text that is
allowed to wrap, so the ellipsis was inert. And the panel was placed with the
TRIGGER's width while its height was measured off the panel, in the same object
literal — which pinned a toolbar picker's list to whatever its current answer
happened to be, made `align="right"` compute the same number as `align="left"`,
and clamped a panel that had grown as though it had not, so it hung over the
viewport's edge.

**A rule compensating for a package is a rule with an expiry date, and it is
the consumer's job to notice.** These were written with the diagnosis attached
precisely so they could be deleted rather than inherited — the last of them
said outright that the fix belonged upstream, in the `placePopup` call that
already measured the panel's own height. It does now. Nothing in this tree
styles `.crewlet-select__menu`, `.crewlet-listbox__option` or
`.crewlet-select__option-label` any more, and `styles/classes.test.ts` is what
keeps that true: a rule on a package class has to earn an `ALLOWED` entry with
a reason, so the next one cannot arrive quietly or outlive its cause.

### An object's own facts

`PropertiesRail` is the rail beside an object — its state, its people, its
plan, its record. Two components did this: a flat `KeyValue` with no sections
used by the event, the run, the turn and the seat, and the work item's own
sectioned rail used by nothing else. They had different label widths, different
gaps and different rules about an absent value, so the same kind of panel read
differently depending on which screen it was on — and neither could say **who**
set a fact, which on a product whose objects are mostly written by agents is
the thing a reader asks about most. "In progress" is a different fact from
"moved to in progress by ada, eleven minutes ago, in turn ↗".

**An absent value is a decision.** Four different facts share one empty cell
and they are not interchangeable:

- the row is **dropped** — this object has no such property at all (a task with
  no collaborators has no collaborators row);
- the row is a **dash** — the property exists and holds nothing (a task with no
  due date has a due date, unset);
- the row **says something** — the empty state means something a reader should
  know ("none — reflection uses the default");
- the whole **group says something**, in one line, because every property in it
  is absent — `whenAllAbsent`. A section of nothing is not the same claim as a
  row of nothing: a fresh task spent a heading, a hairline and four dashes
  telling the reader that nothing about it is scheduled, which in a 420px peek
  is most of the space between the header and the description. "Nothing
  scheduled: no dates, no estimate, no size." states it once, the heading stays
  — the group exists and is empty, which is not the claim the *dropped* case
  makes — and the rows come back the moment any one of them is filled.

The caller says which, on all four, and there is no path by which forgetting
produces a plausible-looking wrong one. The fourth is deliberately an opt-in
rather than something the rail works out for itself: a rail of annotation rows
would fold into one sentence the day somebody cleared them, with nobody having
decided that. `0` and `false` are values a property can hold and are rendered,
never folded into "nothing set".

**One attribution per change, not per row.** A change can set several fields at
once, so consecutive rows carried identical copies of "agent-ceo · 21h ago ·
turn ↗" — one sentence written out six times, which turned three rows of a
peek into six lines. A **run** of consecutive properties sharing one (actor,
instant, turn) draws ONE line, naming what it covers, under the last of them:
"Status, Priority and Type · set by agent-ceo · 21h ago · turn ↗". It is the
rule [A row is not a row](#a-row-is-not-a-row) already states for a feed —
rows under one heading do not each repeat the actor — and a row that draws no
line ends the run rather than being reached over, because a line naming
"Status and Type" with a Priority between them is a claim the reader has to
check.

**And never an attribution under an absence.** Provenance under a dash claims
the engine recorded somebody setting nothing. The one exception is a change
that EMPTIED the field, which the log did witness, and it reads "cleared by"
rather than "set by" — they are different events about the same blank cell.
The wording is one component shared with the header's fact line, because
written twice it had already drifted: the line said "set by ada" and the rail
said a bare "ada", which under a value reads as who wrote the row.

**A header and a rail stacked in one column are one reading.** On an object's
page the rail sits BESIDE the header, in a sticky side column, so the header's
fact line and the rail's rows are read across a gap — the line to scan in a
second, the rows to study. In a **peek** both are in one 420px column with the
rail a hundred pixels under the header, and there the same facts in both is not
a second reading: it is the same answer given twice before the description has
been reached. So a peek whose body is a properties rail passes NO facts and the
rail states every property once; a peek with no rail under it — a unit, a
revision — keeps its fact line, because there it is the only place the object's
own values are stated. The caller decides, since the rule is about the frame's
BODY rather than its header.

### A log is an axis, then its rows

Two screens are log-shaped — the event log and the Inbox — and both had the
same hole: a page of rows answers *what happened* and has no dimension at all
for *when the company was busy*. A burst at four in the morning and a steady
trickle across a week are the same hundred rows in the same column.

**`Histogram`** is that dimension, and its bars are the **engine's**. The
browser holds at most a page and the store's window it never holds, so an axis
folded client-side would be right for one window and absent for every other —
the same rule the spend series follows. Both halves compile their filters
through one predicate in the store, so a bar can never claim rows the listing
beneath it would not show. Every bucket is drawn, empty ones included, because
a quiet hour is a fact about the company rather than a gap in the chart; the
height is the true proportion with a floor in **pixels**, not percent, so a
bucket of 4 beside a bucket of 400 stays visibly shorter instead of both being
rounded up to the same visible sliver.

**A bar is a control.** Clicking one narrows `window=` to the bucket it covers,
which is the gesture every log tool has and the reason the axis is worth having:
"what happened in that spike" is the question the spike creates.

**And the axis is labelled, or it is a box of bars.** A seven-day window holding
one busy day is a single column hard against the right edge of an empty plot,
which reads as a chart that failed to load rather than as a company that was
quiet until yesterday. So the first bucket, the last bucket and an even spread
between them carry their own dates — five labels at most, which is what the
narrowest layout holds at the axis's size — positioned over the bucket each one
names rather than laid out in flow, since a label is wider than the bar it points
at. A day bucket is a date with the year dropped in the reader's own year, the
form every column of dates in this product takes; anything shorter is a wall
clock in the reader's chosen zone, because five repetitions of today's date under
a chart of minutes is the noise the compact form exists to remove. They are
**ink**, tabular and at the caption size: the chart's one accent is spent on the
bars.

**What the bars are a count OF is a required prop, not a caption.** `over` is
`window` where the engine counted them through the listing's own predicate and
`loaded` where the browser bucketed the pages it is holding, and the noun is the
caller's too — so the one sentence a screen reader hears says "9,412 events in
this window" on the event log and "41 changes on the pages loaded" on the
tracker's. Optional, it was wrong on the second caller the day that caller
arrived: a tracker chart announced a page count as a window count, in a noun
nothing on that screen is.

**A log reaches its older rows by fetching them.** A page boundary is not a
window: telling a reader to narrow the window to see what came before this page
sends them toward fewer and newer rows. So a log that fills its page offers
**Load older**, holds the pages it has, and discards all of them together when
the window or a server-side facet changes — pages fetched under the previous
question are rows the engine would never have answered for this one, and the
cursor walks that question's history. While a reader is paging back the first
page is held still: the cursor was minted from its last row, so re-reading it
under a poll pushes that row out and the changes between two pages belong to
neither, which is a silent hole in the middle of a log.

**A date is a band, not a wider cell.** A row's time column is sized for a wall
clock, which is right for every row in a log but the first of each day — and
that one used to render the full instant in the same 62 px track, so it wrapped
to three lines and the feed read as a rendering fault rather than as a date
marker. A date is a property of the rows *under* it rather than of the first of
them, so it is a heading between days, at the list's own inset, parked at
`--sticky-top` like every other band so the day a reader is inside stays named
while they scroll it.

**And a repeat is drawn once, counted.** Where a log is about one object — a
turn's bands — a consecutive run of rows a reader cannot tell apart collapses
into one carrying the count and the span of its first and last instants. It is
counted rather than dropped: a chain that fell through eight times is a
different fact from one that fell through once, and the heading above still
counts what went wrong rather than what the list draws. Consecutive only,
because the axis is time and a merge across an intervening row would either lie
about when or reorder the band to make the lie true.

**Every row in the product takes one inset**, `--row-inline`, and it is the
step `Card.Header` pads by — because a row list usually sits in a card, and
seven row classes each writing their own literal is how four panels on the turn
page came to draw their content four pixels inside their own titles while the
panels between them did not.

**`FacetRail`** is the other half — one dimension of the list, as chips that
narrow it. Two screens had two of these and they disagreed about the one thing
that matters: whether a count is over what is **loaded** or over what **exists**.
That is a required prop now rather than a caption somebody remembers, and it is
said beside the chips. A number next to a chip is read as "how many there are",
and when it is really "how many of the four hundred rows this tab happens to be
holding" the reader is being told something false about their own company.

**The note is a caption on the numbers, so it is drawn only where a number is.**
Printed unconditionally it appeared beside a rail that could never carry a count
— the Inbox's unread/all/snoozed control, whose two other scopes are rows the
loaded page does not hold — and qualified figures the reader could not see. A
dimension that cannot be counted at all is not a facet rail: it is one of N
scopes, which is a `Segmented`, the same control the work toolbar's
open/closed/everything uses.

A facet count is computed with its **own** filter lifted and every other one
applied, because that is the only meaning it can have: a chip says how many rows
choosing it would show. Counted through its own filter, every chip but the
selected one reads zero and the rail is a dead end.

### The frame-level keys

| Key | Kind | Meaning |
|---|---|---|
| `peek={kind}:{id}` | section | the detail rail is open on that object |
| `tab=` | section | the object page's tab |
| `view=` | section | a list container's view |
| `lens=` | section | which whole reading of an object is drawn — a project's items, about and history, Settings › Configuration's active, history and diff |
| `sort=` `cols=` | filter | the grid's order and its visible columns |
| `sort.<name>=` `cols.<name>=` | filter | the same, for a second grid on the page — and `cols.<shape>=` alone on the work screen, whose one grid has a column set per shape while both share its `sort=` |

**`peek` is one key, one component, one rule.** A plain click peeks; ⌘-click,
middle-click and the rail's `Open ↗` go to the page. Inside a peek `[` and `]`
step through the list it was opened from and `esc` closes it. Opening the rail
**pushes** — Back closes it, which is what a reader means by Back with a panel
open — and moving it **replaces**, because four objects walked through one open
rail are one place the reader has been, exactly as four ticked chips are one
screen.

**The shell mounts the rail; no screen renders one.** A peek's BODY belongs to
the kind, in `app/frame/peeks.tsx`, and the shell dispatches on the `peek=`
token — so a list opens a peek by naming what it points at and needs to know
nothing about it. The rail took its body as `children` once, which meant the
screen that opened a peek had to know how to render one, and so exactly one
screen ever did: every addressable kind — seventeen of them — could be named
and only a work item could appear in the rail. A kind with no entry in that table is not an error — a
notice is read in its own inbox and a model has no page at all — and the rail
closes rather than opening empty.

**Each peek asks its own question.** It takes an id and fetches, rather than
being handed the row that opened it: the row carries what its list needed, a
peek answers "what is this thing", and the two differ on every kind. It is
also what lets a pasted `peek=` open on arrival, where no row exists.

**A peek is one column, so it says everything once.** Its header carries the
object's identity and its state marks; where a properties rail follows below,
that rail states every property and the header carries no fact line — see
[An object's own facts](#an-objects-own-facts). A list inside a peek measures
ITS OWN box too: `.work-rows` drops to four columns on a container query
rather than a viewport one, or a 420px panel on a wide screen gets the full
eight-track row and the title is starved to nothing.

**`[` and `]` walk the list, so the list publishes its order** with
`usePeekNeighbours`. Only the list knows what the reader is looking at —
sorted, filtered and paged as they left it — and a rail that stepped through
anything else would be walking a different set from the one on screen. A
screen that publishes nothing gets a rail with no stepper, which is honest.

The id is split on its FIRST colon only, so an id that carries a colon of its
own survives being carried in a query value.

### The frame

The frame is the kit's `AppShell`: the sidebar stands on the frame rung and
the screen floats beside it on a sheet, inset by `--size-shell-inset` and
rounded, with ONE scroller inside it. Below the kit's `breakpoint.shell`
(1024 px) the sheet is the whole window and the sidebar is a modal drawer that
takes Escape, traps Tab, hands focus back to its toggle and closes on a
navigation — opened from the bar's toggle or with `Mod+\` (`⌘\` on a Mac,
`Ctrl+\` elsewhere), a chord that does nothing above the breakpoint, where the
sidebar is always on screen; the skip link precedes everything and lands on
the scroller. None of that is written in this repository, because a second
copy of it is how the two drift. What `dashboard/src/app/` owns:

| Piece | What it is |
|---|---|
| `sidebar/` | the one sidebar — [The sidebar](#the-sidebar) — with the health card and the user block |
| `header/PageHeader` + `Breadcrumb` | the page header: the kit's top bar with the trail (whose last crumb is the page's `h1`), who is working now (Home's bar only, `useWorkingNow`), the star and Copy link, and last the screen's own controls (portalled in by `PageActions`); then the workspace's SECTION TABS, as links in a labelled `nav` with `aria-current` on the section the reader is on, drawn only on a section's own page; then the `StateBar`. Settings draws its sections as a grouped COLUMN beside the screen instead (`SectionColumn`, the kit's `SidebarNav`), with a key mark on a guarded section and an arrow on a cross-link, and Knowledge draws a TREE there — its search, the search's mode and every space's pages (`KnowledgeTree`, out of the Knowledge chunk) |
| `routes.ts` + `crumbs.ts` | the route table as a pure resolver, and the trail derived from it |
| `layout.ts` | the frame's breakpoints, READ from the kit's tokens (`breakpoint.shell`, `breakpoint.phone`), and the one width the dashboard derives itself — where the peek becomes a column |
| `StateBar` | the answer's own honesty in one place: degradation, `read_level`, `complete: false`, how far this node has applied |
| `ObjectHeader` | an object's eyebrow, title (a level-two heading in every frame: on a page the last crumb is the `h1` and already names the object, and a peek sits inside a page that has one), state marks and up to six facts, in the same order wherever the object appears — though a peek whose body is a properties rail passes none, because a header and a rail stacked in one column are [one reading](#an-objects-own-facts). A fact may carry a `note` saying where its value came from, for the facts a reader can reasonably doubt, and only those. A fact that is one token (an id, a build) or a set of chips (a node's roles) takes two tracks, so it is never broken mid-token or stacked chip over chip. STATE lives here, never in the page bar: see [What a mark MEANS, and where a control belongs](#what-a-mark-means-and-where-a-control-belongs) |
| `useTab` | which OBJECT tab is real. `tab=` is a string off a URL and the tab set belongs to the object — a human seat has three and an agent seat has six — so the hook resolves the parameter against the tabs this object HAS. It binds `1`–`9`; the strip itself is `@crewlethq/ui`'s `Tabs` |
| `ObjectTabs` + `tabFit` | an object's own tabs as the kit's underline row, drawing the tabs that FIT and folding the rest into a "More" menu at its end — the tab the reader is on always drawn — by the same arithmetic (`foldTabs`) the section strip folds with; the widths are read off a hidden, inert twin of the row |
| `DetailRail` | the peek's chrome — resizable, a column of the sheet where the frame leaves the list beside it its floor (1160 px on most screens, 1396 beside Settings' column, 1416 beside Knowledge's tree), a drawer under that |
| `PeekHost` + `peeks.tsx` | the one peek in the product, mounted by the shell in the kit's footer slot: the body belongs to the KIND, so a list opens a peek by naming what it points at. `usePeekNeighbours` is how a list publishes the order `[` and `]` walk |
| `DataGrid` + `cells` | sorting in the URL, bands from a grouped answer, typed cells; a row becomes a labelled card under 640 px, or two unlabelled lines on a `compact` grid |
| `PropertiesRail` | an object's own facts, in sections, with who set each |
| `Histogram` + `FacetRail` | a log's time axis, and one dimension of it as chips |
| `TimeRangePicker` | the one control for `window=` |
| `PageActions` + `PageNote` | a screen's own controls, portalled into the bar; its one sentence of explanation, wrapped so its last line never holds a word alone |

**Three widths, and only one is ours.** Below 1024 px (`breakpoint.shell`) the
sidebar is the drawer. Below 640 px (`breakpoint.phone`) a layout is single
pane: a `DataGrid`'s rows become labelled cards — or, on a grid that asks for
`phoneRows="compact"`, two unlabelled lines: what the row IS (its
`phoneLead` cells), then its marks. The work list takes the compact row,
because it is scanned by the dozen and its labelled card stood eight lines
and 230px per task; the table keeps the labels, since its question is a field
at a time. The Settings column and the Knowledge tree stack
above the screen beside them — the tree with its spaces folded behind one
disclosure, since its length is the company's page count — and the page bar's controls take a line of their own and
scroll. Both are the kit's numbers, written as media-query literals that
`frame.test.ts` holds against `app/layout.ts`, which reads them from the kit.

And the peek is a column of the sheet wherever the list beside it keeps its
444 px floor, which is arithmetic over EVERY width in front of the list:

| Term | Width |
|---|---|
| the sidebar (`size.shell.rail`) | 236 |
| the sheet's inset (`size.shell.inset`) | 8 × density |
| the sheet's two hairlines | 2 |
| the peek, at rest | 420 |
| the scroller's reserved gutter (`scrollbar-gutter: stable`, drawn 10 px wide) | 10 |
| the screen's padding (`spacing.5`, each side) | 40 × density |
| Settings' section column, on a screen that draws one — or Knowledge's tree (`TREE_COLUMN`) | 236, or 256 |
| the list | 444 |

— **1160 px** on a screen, **1396 px** beside Settings' column and **1416 px**
beside Knowledge's tree at the normal density (1152 and 1388 compact, 1167 and 1403 comfortable). Below that the peek
is a drawer over the screen. The first cut of this counted only the sidebar,
the inset, the hairlines, the peek and the list, and put the threshold at
1112: the Work list beside a peek there was 396 px, and at 1280 a peek on
Settings › Nodes left the nodes grid 328 px with its columns cut off.

It is **the shell, not a media query**, that applies it, because two of those
terms are not the window's: the section column belongs to the screen, and the
inset and padding scale with the reader's density — a media query can see
neither. `app/layout.ts` (`peekColumnMin`) does the arithmetic, the shell asks
`matchMedia` for the one width right for the frame on screen, and it writes one
attribute, `data-peek="column"` or `"drawer"`, so there is no width with both
or neither. And the column's track is the reader's dragged width CAPPED at what
the sheet can give with the list at its floor (`--peek-reserve`, from
`listReserve`), because a width dragged to 640 on a wide window outlives it.

**Beside a canvas the peek rests at 330.** A list beside a peek keeps its floor
and reflows; a chart is SHRUNK into whatever width the peek leaves it, so every
pixel the rail takes is one the whole company is drawn smaller in. The org
chart asks for the approved artboard's 330 (`usePeekWidth`, the way a screen
asks for the window's height with `useFillScreen`), and the rail rests there
until the reader drags it: a width they dragged is their preference on every
screen, anywhere from 330 to 640. The threshold above is computed at the
resting 420 for every screen.

**A screen publishes what the chrome needs and renders none of it.** The labels
the route cannot supply (`usePageLabels`), the coverage of the answer it drew
from (`usePageCoverage`), a section's figure for its tab (`useSectionCounts` —
a string the screen already wrote, "12" or "20+", because only the screen knows
whether a number is a total or a floor), and its own controls. Twenty screens
each drawing their own header is how five of them came to drop the coverage
badge.

**And only a SCREEN publishes — a body rendered inside one never does.** The
frame holds one coverage slot with one setter, so a reusable body that published
from inside another screen fought that screen's own answer: on
`#/work/{KEY}?lens=history` the project's read and the log's read both wrote it,
whichever polled last won, and switching lenses unmounted the log and cleared the
slot to nothing over a page still showing the project it had read. A body states
its own rows' coverage INLINE instead, beside the rows, which is what a peek rail
and the Items lens already do. The two halves never both apply: a screen
publishes and draws none, a body draws and publishes none, and the same component
serving as both takes that as a prop rather than guessing.

**Every object screen publishes the name it draws, and it is the same string.**
A route addresses an object by its identifier — a turn or a trace or an event
by its id, a seat by its handle, a revision by its ULID, a page by its id — and
the frame has nowhere else to learn what that object is called. So the one name
a screen resolves for its `ObjectHeader` is the one it publishes, and it
reaches three places at once: the last crumb, the browser tab (`Shell` titles
it from the trail) and the palette's recents. Written a second time for any of
them, the three drift, and a reader is offered a string the page never showed.

**A screen with no name publishes none, and the id stands.** Every object here
has an unnamed case — a turn still running, a trace whose spans fell out of the
window, a revision nobody summarised — and each header fills the slot with a
placeholder, because a header needs a word. A placeholder must not reach the
label: "Turn" names no particular turn, and two of them in the recents are two
identical rows pointing at different places, which is strictly worse than the
two ids they replaced. The crumb draws an unlabelled segment in the mono face
for the same reason it draws a handle in it — an identifier has to look like
one.

### The keys

Every key the dashboard answers is a row of ONE table, `app/keymap.ts`, and
`?` anywhere shows it — the legend renders the table and nothing else, so a
key added there is a key the legend shows, with no second list to remember.
The command palette has a row for the legend too, because the reader looking
for the keys is the reader who does not know `?` yet.

| Where it is live | Keys |
|---|---|
| Anywhere | `Mod+K` search (the same chord closes it), `/` search this screen, `?` the legend, `g` then a letter a workspace, `Mod+\` the navigation drawer on a narrow window |
| In a list | `j` and `k` walk the rows — on a task opened from a list, they open the next and the previous task — `Enter` opens one |
| With a peek open | `[` and `]` step through the list it was opened from, `Esc` closes it |
| On a page with tabs | `1`–`9` go to that tab |
| On a chart | `+`, `-` zoom and `0` fits, with focus in the chart (bound by the design system's canvas) |
| In the org builder | `Mod+Z` undo, `Mod+Shift+Z` redo |
| Writing a reply | `Mod+Enter` sends it (the Inbox's composer; `Enter` alone is a new line) |
| In search | `Tab` and `Shift+Tab` walk the scopes (bound by the design system's palette), `Mod+Enter` asks an agent, `Alt+A` assigns the task at hand, `Alt+C` creates a task, `Backspace` in an empty picker goes back |
| In a dialog, a menu or the drawer | `Esc` closes whatever is on top (bound by the design system's layer stack) |

`Mod` is Command on a Mac and Control elsewhere, and every hint is drawn in
the platform's own notation with the kit's `Kbd`.

**A bare `Enter` or `Space` on a focused button or link is that control's.**
The list's `Enter` opens the cursor row only when focus is not on a control
the platform activates itself: a button inside a row — Restore in the trash, a
ceiling's pencil on Budgets — is pressed, never skipped for the row behind it.

**A binding names a row, never a key.** A component asks `useKeymap` for the
rows it answers — `{"peek.close": close}` — and the table says which key that
is, so no two places can spell one key two ways. `keymap.test.tsx` refuses two
rows answering one press where both can be live: the page's scopes are all
live together (a list, with a peek open, on an object with tabs, beside a
chart), so a press is unique across them, and the layer's rows — search's
among them, since the palette is a layer — are their own namespace because
every page key stands aside while a layer is up. Search's accelerators take
`Alt` rather than a bare letter because they are pressed in its field, where
a bare letter is a letter of the query; they match the physical key, since
Option+A on a Mac types `å`. Rows the
design system binds itself — the canvas's zoom, the layer's Escape — are in
the table so the legend is the whole answer and the collision check sees
them, and `useKeymap` refuses to bind one a second time. Nothing else binds a
key: the suite fails a module that calls the mechanism under `useKeymap`, or
listens for `keydown` itself, except the org builder's undo, which is scoped to
presses inside the builder and reads its keys from the table with
`matchesRow`.

**`/` searches what you are looking at.** A screen with a search box of its
own — the seat filter, the event log, the tools catalogue, the audit log's
actor, work search and a list's key-or-title box, the knowledge base and its
page finder — registers it with `useSearchTarget`, and `/` focuses it with its
text selected to type over; on a screen with none, `/` opens the palette. It
opened the palette everywhere, so on those screens the key a reader pressed to
search the screen took them away from it.

**A key waits for the reader.** A bare key does nothing while they are typing
in a field, a select or anything a screen made focusable and handles keys for;
only a row that says so (`Mod+K`, `Esc` out of a peek's field, `Mod+\`) fires
from one. Every page key waits while a dialog, a menu or the drawer is open,
because the page behind `aria-modal` is inert. And a press somebody already
answered — the chart claiming `+`, a menu its arrows — or one an input method
is still composing is not a key at all: Enter that picks a Japanese candidate
must not also open the grid row behind the field.

### The page bar wraps by what it holds

The bar holds the trail, who is working now (on Home's bar only, as the Main
artboard draws it — the sidebar's Agents badge carries the same count on every
other page, and a chip in every bar sat between a task's trail and its actions
and pushed a profile's controls past a phone's edge), the star and Copy link — which is what the
approved artboards label **Share**: sharing a page here IS its address, since
anyone the link reaches reads it under their own token — and LAST
the screen's own controls (portalled in by `PageActions`), so a screen's
primary action ("New task") ends the row where the approved designs put it.
An OBJECT page's lenses (a project's Items, About and History) are portalled
into a second slot straight after the trail (`PageLenses`): that slot is no
flex item at all while it is empty, and when it holds lenses it is the lens
group that takes the line's slack — the trail sizes to its content, capped at
60% of the bar so a long name ellipsises beside its lenses rather than pushing
them onto a line of their own.
On a phone the bar keeps ONE action in view — the screen's primary one, which
leads the line — and folds the rest into **More** (⋯) at the line's end: the
frame's star and Copy link, and whatever a screen marks as secondary and
publishes to the frame with `usePageMenu` (a project's **Edit project**, which
opens the same dialog its inline button does). The menu is drawn only at that
width, so no action is ever in two places a reader can see at once, and what a
press did (copied, refused, at the star cap) is said in a toast, because the
menu closes on it. A task's own "More for ENG-42" folds into it too, rather
than standing as a second ellipsis beside the frame's. What is left of the
controls shares a line with the trail or the lenses when it fits: laid out
inline, a project's bar stood three rows tall on a 390 px screen — the trail,
the lenses, then four controls — about 105 px of chrome over the work.

**A wrapping flex container assigns lines BEFORE it shrinks**, so a trail at its
content width broke the line as soon as a long title and the controls exceeded
the bar, with free space left on the first line. The trail's flex BASIS is
therefore 0, clamped by its FLOOR: a line holds the floor plus the controls,
the break comes only when even the floor cannot sit beside them, and the grow
factor hands the trail everything the line has left. A threshold measured on
the busiest bar would push the controls onto their own line on every screen
under it, a sparse one included.

**The floor is the way back out, measured.** It is where the last crumb starts
— every ancestor at its own width — plus the few characters the last crumb
keeps as a stub, never under 20ch and never past the room the bar's first line
has beside the drawer toggle (below the shell breakpoint), or the trail would
drop under the toggle and leave it a line of its own. A fixed 20ch
alone let a seat's trail share a phone's line with its one button at two
hundred pixels, and "Agents › Engineering · Co" was cut mid-letter under
Message with the seat's own crumb out of sight; measured, a trail that cannot
show its ancestry beside the controls takes a line of its own.

- **The trail is what gives way**, and past its floor it SCROLLS rather than
  clipping as a box: the ancestors do not shrink, so once they alone are wider
  than the whole bar there is nothing left to ellipsise. The scrollbar is not drawn — on a
  52 px bar it would sit on the baseline the trail is drawn on — and while it
  overflows the trail is a tab stop, which is how a keyboard reaches what is
  past its edge. Only then: a trail that fits has nowhere for the arrow keys
  to go, and a stop on every page that did nothing was a wasted Tab press.
- **Each crumb's words are their own box**, and that box is what ellipsises.
  A crumb is a flex row (a glyph and a label), and text straight inside a flex
  container is an anonymous item no `text-overflow` reaches — so the page's
  `h1` was cut mid-word at the bar's edge rather than ending in "…".
- **The control group may not shrink at all.** It wraps, so a shrink does not
  shave a label off a button, it drops the last control onto a row of its own —
  and a flex container distributes shrinkage proportionally in one pass, so any
  factor above zero pushes "Copy link" onto a second row by a fraction of a
  pixel. A `max-width` is the valve that keeps a group wider than the whole bar
  wrapping rather than running past the edge.
- **On a phone's width the controls are one line that scrolls** — sharing the
  trail's or the lenses' line when they fit, by the same rule the wide bar
  keeps, and a line of their own when they do not — a
  container query on the header, because what overflows is the header and a
  viewport query cannot see a sidebar or a peek. Rows of controls stacked under
  the trail would push the screen a third of the way down a phone. The edge
  with more past it FADES: with the scrollbar hidden, a control cut at the edge
  read as a broken label rather than as more to swipe to. A row that fits
  carries no fade.
- **The section tabs never scroll: what does not fit folds into "More".** A
  fade was too quiet for a strip of places — at 1280 My work cut "Checklist 0"
  at the edge and nothing said a section was past it — so the strip draws the
  tabs that fit, in the workspace's order, and a "More" menu at its end holds
  the rest, each with its figure. The tab you are on is always drawn, taking
  the last place that fits; a tab only ever leaves from the end, so the order
  never changes with the width. The widths are measured, and measured again
  whenever the strip or a tab changes size (a figure arriving, the web font
  loading, the density preference). **An object's own tabs fold the same
  way** (`ObjectTabs`, a seat's Overview … Settings): the kit's underline row
  scrolls with its scrollbar hidden, and on a phone a seat's Settings began
  five pixels past the strip's end with nothing to say it was there. The
  object strip measures a twin of the kit's row — hidden, inert, clipped —
  since the kit draws a row whole, and hands the widths to the same fold.
- **The star is on every page that is one.** A workspace's own page is already
  a sidebar row, so the star there cannot keep it — but it is drawn,
  unavailable and saying "Already in the sidebar", rather than left out: left
  out, it appeared on every section of a workspace but the first, and moving
  between two tabs moved the header's controls.

### Moving, and going back

Every screen, section and filter is in the URL, so a view can be refreshed,
bookmarked and handed to somebody else as a link. What takes deciding is the
**session stack** — which navigations leave an entry behind — because that, not
the URL, is what the Back button reads.

| Move | Stack | Why |
|---|---|---|
| a **section** — a tab, a view, opening a peek | pushes | the reader called these screens; Back after three of them should walk out through them |
| a **filter** — chips, sort, a search box, moving a peek | replaces | four ticked chips are ONE screen; Back means "off this list", not "untick one" |

The line between them is whether the reader would call it a different screen,
and it is the only judgement call in the router.

**Scroll is a property of a history entry, not of a URL.** The same screen
reached twice is two places the reader has been, and keying a position by URL
collapses them onto one. Each navigation stamps a key into `history.state` and
files the outgoing position under it; an entry with NO key is exactly the test
for "somewhere new", which is the only case that starts at the top. A restored
position is re-applied for a short window while the rows arrive — a scroll is
clamped to the height that exists, so one attempt lands short — and abandoned
the moment the reader touches the page. A REPLACE restores nothing: it stays on
the entry the reader is looking at, so they are already where they are, and a
restore there undid whatever the screen had scrolled into view in answer to its
own filter.

Both of these shipped wrong once, and neither is visible in a URL.

**A thing worth linking to gets an address, not a scroll position.** The
previous dashboard revealed a unit by scrolling the org screen to it
(an `org` address carrying `?unit=Backend`, with a router-level reveal hook); this tree gives a unit
its own page instead — `#/agents/teams/{unit}` — so it can be linked to, opened
in a peek, and carry its own state. There is no reveal-on-arrival hook here.
Where a selection genuinely belongs in the URL rather than in the path it stays
a filter: Edit org reads `unit=` and `seat=` to name the selected node on its
chart (see the builder's toolbar below) — and an address it is OPENED on names
the node whose editor it opens, once — and a `DataGrid` scrolls its selected
row into view. The ring is static: nothing on a live screen animates
for data.

**Work that exists nowhere else is not left behind unasked.** A surface
holding it registers a leave guard (`useLeaveGuard`, the org builder's node
editor while it has typed changes), and every move to another entry is put to
that guard first: a push from code, a link, and Back or Forward. A guard is
handed the route the move goes to and holds only a move that would lose its
work: the org builder keeps its draft and an open editor through a move within
the builder (a view or a chart), so its guards let that go
(`BuilderContext.keepsTheLens`). A replace is never held, because by the table
above it stays on the entry. Back has already happened by the time a page
hears of it, so a held one is undone at once and made again only when the
reader agrees to lose the work; every entry carries its place in the session
(`crewletIndex`) beside its scroll key, which is how the router knows which
way to undo it. The guard that began to hold last is asked first, and agreeing
to it asks the next one before the move is made. A reload or a closed tab gets
the browser's own prompt while any guard holds, and `useUnloadGuard` asks for
that prompt alone, for work a move within the page keeps but a closed tab does
not (the builder's draft, kept in session storage). The page listens for a
reload only while something holds, because some browsers keep a page with a
`beforeunload` listener out of their back-forward cache.

### Home is the landing screen

`#/` and `#/home` are Home. What a person opening this wants to know first is
whether anything needs them — but a home that is ONLY that queue has a failure
mode that arrives on the first day: a company where nothing is wrong renders as
a blank page, and a reader cannot tell that from a dashboard that is broken.

**So the first fold is the company, then what needs the reader, then what the
company did.** Top to bottom:

- **The greeting.** The company's day (on `org.timezone`, the one clock every
  due date and budget window is cut on), the reader's morning, and one sentence
  of the engine's: "Nimbus is running on 3 nodes. **3 decisions** are waiting on
  you, 1 condition needs a look, and 4 agents are working right now." The
  decisions figure is weighted; an engine condition — a refused token, a lost
  connection, no configuration, a node that shed its seats — takes the sentence
  over rather than sitting beside it. **Prose never prints a zero as a digit or
  a wrong plural**: "nothing needs your decision", "no agents are working right
  now", "1 condition needs", "2 conditions need" — and a fleet the presence
  read counted as none is "Nimbus is running.", never "on 0 nodes". "N
  conditions need a look" is a link to where they are listed, the Inbox's
  "Needs a decision" band (opened on the one when there is one). A reader
  nobody bound is told nothing about decisions at all. The **Today · 7 days · 30 days** control
  (`range=`) sets the window the figures below read over; Today is one company
  day, cut at the company's midnight by the engine.
- **Five figures** (kit `StatCard`s): *Agents working now* (`working / total`
  from the engine's `activity`, with the state bar of working, waiting,
  stopped and idle, and a sub-line naming only the states somebody is in);
  *Waiting on your decision* (the engine's `decisions` count plus the seats
  stopped on their budget that THIS reader can act on — raise the ceiling, a
  `/config` write any presented token may make, or hand the item on with
  `update_work_item` — the oldest wait in the warning ink, and **Review**
  into the Inbox's decisions view; a stopped seat the reader can do neither
  about is one of the conditions that "need a look" instead — an em dash and "Not bound to a person" for a reader who is
  nobody); *Tasks in progress* (`work_flow.now.active`, its change over the
  week and what is blocked or overdue, with the fortnight's sparkline);
  *Completed* over the window, against the window before it; and *Tokens* over
  the window, with the **week's** budget meter captioned as the week's ("63% of
  this week's budget · resets Mon") in the engine's own state — or "No weekly
  budget", which links to Spend › Budgets for an operator and is plain text for
  anybody the screen behind it would refuse. The Tokens tile draws no delta:
  its second line is the budget's, and a bare change in front of it read as a
  claim about the budget. A second line is whole FACTS, so a narrow tile breaks
  between them ("+17 vs last week" / "· 2 blocked"), never inside one. Each
  figure waits for its own answer and says so; one whose answer has not
  arrived is never a `0`.
- **Needs your decision**, the first three, newest first, each answerable IN
  PLACE. A structured ask draws each option as a button that sends the choice
  as the answer (`comment_on_work_item{answers, choice}`), the recommended one
  primary, with who asked (the person behind a token, never the token), the
  role the reader is asked in, "reports to you" where the chart says so, and
  the line saying what the answer sets off — "CTO is woken with your answer and
  posts it to #releases" when the ask promised a channel, "CTO continues from
  your answer" when it did not. A coding run parked on a question is answered
  by its turn (`answer_run`), with how long its box is still held. A seat the
  engine stopped on a spent budget offers **Raise budget** — a dialog of that
  scope's three ceilings, opened on the seat's own when its own window is the
  one refusing and on the company's when the company's is, with the refusing
  window focused (see [Spend › Budgets](#spend--budgets-raised-in-place)) —
  and **Reassign** of the item it was on. An ask with no options is
  answered in words on its row: **Reply** opens the same dialog a parked run's
  **Answer** does and sends `comment_on_work_item{answers, body}`. Every
  control is drawn for every reader and disabled with its reason where this
  browser cannot act. **Open inbox** and **Review** both land on
  `#/inbox?reason=decisions`, which draws every one of them.
- **Live now**: every seat the engine says is working — what it is on (the
  item the turn is charged to, never a work key), how long, where it is in
  context → execute → review ("Execute · round 7 of 25"), and the last call it
  made — each a way into its turn.
- **Tasks completed** per company day over the last fourteen (today in the
  accent), **Tokens by team** over the window (tokens, never money), and
  **Projects**: each project's done, active and to-do work as one bar, its
  lead and its target date.
- **Recent activity** (`company_feed`): work delivered ("approved on first
  review", "1 turn · 38.2k tokens"), filed ("from Slack", off the create's own
  origin), handed on ("hand-off 1 of 8"), pages published and schedules run —
  Everything, Completed, Hand-offs or Schedules (`feed=`) — with Load older,
  and the full event log a link away. A schedule's rows are its RUNS, and the
  engine folds a schedule's consecutive runs into one ("Schedule backlog-sweep
  ran 12 times for PM", "since 02:40"): a tick it skipped is never a row that
  says "ran", and a frequent schedule never buries the work under a page of
  itself. Each row's sentence comes from ONE table (`FEED_PHRASES` in
  `routes/home/model.ts`) keyed on the feed's own kinds. On a phone a row is a
  three-column grid — time, mark, sentence — whose sentence wraps inside its
  own column with the aside under it, so a row that wraps and one that does not
  keep the same alignment.

Five tiles sit in a row while the page column holds them — five tiles at their
176px floor and four gaps, 928px — three a row under that, one on a phone; the
paired cards stack below the same width. Every one of those is measured from
the SCREEN's column (`@container page`), which the shell declares on its
content as well as on its header, because the sidebar and an open peek take
their share of the window first.

### The Inbox is where you act

The Inbox is built around what waits on the person — the questions put to
them, the runs parked on them, the seats they can unblock — and then what
reached them and why. It is two columns, each its own scroller: the list, and
the open row in full with the answer to it.

**One list, in a fixed order.** "Needs a decision" leads: the asks put to this
person and the coding runs parked on a question to them (`decisions`, the read
Home's figure counts), the seats the engine stopped on a spent token budget,
and the conditions a person decides (`lib/attention.ts`, the subjects whose
home is `seat`). Then the notices, grouped **Today**, **Yesterday** and
**Earlier** at the COMPANY's midnight — the day the engine counts its budgets
and day charts in, so a notice filed at 23:00 company time is not "Today" on
one screen and "Yesterday" on the next. Decisions are never interleaved with
notices by time, which would eventually rank "a task you watch moved" above
the CEO asking whether to hold a release. An ask is ONE row: the notice that
told the person they were asked rides on the decision it is about rather than
being listed again under Today, and Done and Snooze on the decision mark it.

**Every row has the same three lines.** Who — the seat behind it, drawn with
its avatar and state ring; a token's change is drawn as the person
(`actor_seat`), never as the token id, and one bound to nobody as "An
operator" — what (the question, the stop, the excerpt), and why (a pill). An
excerpt the engine COMPOSED names its author by name as the row's head does:
the engine writes "maya-ops put LEAD-3 at position 1 of your priorities" with a
handle, for the seat it wakes, and the row reads "Maya Ops put LEAD-3 …" — a
comment's excerpt is somebody's own words and is left as they typed it. A
notice's pill is its wake reason in the engine's eighteen words
(`contract/reasons.ts`); the rows that are not notices take theirs from
`CONDITION_PILLS` beside the Inbox, kept out of that table because they are
not reasons. Unread is heavier and carries the accent dot; read is quieter,
never hidden.

**Unread, All and Snoozed is a scope, not a facet.** Each is a different
question put to `work_inbox` (`unread`, and `snoozed=exclude|only`), so the
engine narrows the scan rather than the page and an empty page is an empty
scope. Snoozed lists only what was put off, each row saying "Snoozed until …",
and no decisions: a decision leaves when it is answered, not when it is put
off. An unknown `scope=` resolves to `unread`. The unread count is drawn
INSIDE the Unread option ("Unread 5"), as the approved design draws it — beside
the group it read as a fact about all three — and it is the page's, so where
the page stopped with more behind it the option says `50+` and its title says
the engine answers fifty at a time.

**The chips are four questions of the list** — Decisions (every row that is
not a notice), Reviews (the asks put to the person as `approver`), Mentions
and Assigned — and they narrow the rows LOADED. Where that is every row their
counts are totals and nothing more is said; where the page stopped with more
behind it, each chip says its count is the page's in its own description and
title — read with the chip, rather than as a muted line under the row that read
like debugging output. `reason=decisions` is where Home's **Review** and
**Open inbox** land. Every notice still names its own reason on its row. A
change the person made themselves — under either of their names — is never one
of the notices.

**The pane is the decision, answered in place.** The question is the heading;
under it who asked, when, and the role the person is asked in ("You are the
approver on LEAD-12"), with "reports to you" derived from the chart. The card
holds the context, what the asker recommends and why, and the evidence it
cites — a task with its status as the board draws it, a page, a turn trace, a
coding run or a link. Under "Your decision" each option is a card that SENDS
that choice as the answer (`comment_on_work_item{item, answers, choice}`), the
recommended one filled, all through ONE write so a second choice cannot be
sent while the first is in flight. **Reply with instructions** turns the
composer into the answer in words. Where the ask promised a channel
(`decision.inform`) the line under the options says "<asker> is woken with
your answer and posts it to #channel", and the composer says "Also posts to
Slack #channel" beside Send — the engine holds the asker to it
(`ExternalNotification.owes`); an ask that promised nothing draws neither.

A parked coding run's pane quotes its question, says how long its box is still
held, and answers by its turn (`answer_run`). A stopped seat's names the
window that is spent and offers the two ways out — raise the ceiling, or hand
the item on. A condition's says what it costs to leave and where the answer
is. A notice's says who, when and why ("You see this because you follow this
task").

**The thread is read, not replayed.** Under an ask it is the replies in the
ask's own thread; under a notice about a comment, that comment and its
replies; under any other notice, the task's latest conversation. It is read a
page at a time from `work_comments`, the newest page polled, and "Earlier
comments" reads the page before — the detail's twenty are no longer where a
long conversation ends. A token's comment is drawn as the person it was bound
to (`comment_seats`).

**The composer writes as you.** A reply is `comment_on_work_item{item, body,
reply_to}` and answers nothing; an answer closes the ask. `@` opens a picker
over the org that writes `@handle` into the text — the engine resolves
mentions from the body, so the picker never sends a list that could disagree
with it. `#` links another task (`update_work_item{linked}`); the paperclip
attaches a page — an existing one, or what was written saved as a new page
(`write_page`) and then attached (`linked_pages`). `Mod+Enter` sends.

**Every mark is a gesture on exactly what it names.** Done marks the open
row's notices read; Snooze puts them off to one of three presets — in an hour,
tomorrow at 09:00 and next Monday at 09:00 on the company's clock — or a moment
the reader picks, and ONLY the presets inside the engine's
`max_snooze_ahead` (from `work_person`) are offered, since the write refuses
the rest. **Mark all read** is `read_through` at the newest notice LOADED: a
notice that arrived after the page was drawn is one the person has not seen.
None of them sends the inbox back whole, which is what the write they replaced
did — a Done in one tab erased a snooze made in another. A row that is not a
notice draws Snooze and Done disabled with that sentence rather than hiding
them.

**What the list says when it is empty is derived from the answer**, never from
the row count: nothing answered yet and a refusal draw no sentence; a chip
that took every row says so; Snoozed says what a snooze is; and Unread tells
"you are caught up" (the person's own `seen_through` watermark exists) from
"nothing has reached you yet".

**Two panes above a phone, one below it.** Both columns are present from the
first paint and the first row is open until the reader picks one, so the pane
is never an empty box beside a list with something in it; stepping down the
list — or `j` and `k` — replaces history, so four rows read through one pane
are one place the reader has been. Under 640px the list is the screen and a
row opens in its place, with Back.

**Three viewer states, three sentences.** A reader with no credential, a
reader whose credential no seat claims, and a bound reader. The conditions a
person decides are listed for all three; a person's notices and decisions need
a credential bound to their seat, and every write control is drawn for all
three, disabled with the reason.

### My work is one person's day

`#/me` is everything one person is expected to look at, as the SECTIONS of
the page header: the **Queue**, **Asked of me**, **Asked by me**,
**Unblocked**, **Collaborating**, **Watching** and **Checklist**. Every tab
carries the engine's own count of its claim — `work_my_work`'s `totals`, or
`total_hint` for the two that are the work list — written as a floor (`20+`)
only where the engine stopped counting, absent while its read is in flight, and
drawn at zero rather than hidden, because a tab that vanished when it was
empty is what the page was rebuilt to stop. A section holds at most twenty
rows of its claim, and one holding fewer than its count says which ones. The
page opens straight on its sections, as the working screens do: no paragraph
explains it, and each empty section says in its own words what would fill it.

**Every figure for one person's day counts one named thing.** The Queue's is
the open work ASSIGNED to the person, and on your own day it is the same
reading the sidebar's My work figure draws, so the two cannot disagree. The
questions put to you have their own tab. And the priorities reading, below,
says above its first row what IT counts, because it is a different list from
the one the tab counts.

**Two sections are the work list, held to the person.** The Queue is
`ItemsView` with the ASSIGNEE locked — its shapes, Filter, Display, scope,
grouping and count line, opening banded by the engine's `due:bucket` — and
Asked by me is the same list with the ASKER locked instead (`asked_by=`, and
`viewer=` naming the same person, so the questions put through their own
token are theirs too), opening on every status. A lock is not a chip and not a
key on the address: it is what the section IS.

**The Queue's other reading is the order somebody put it in**
(`order=priorities`): the person's stored list, numbered, never re-sorted —
and reordered by dragging a row, or with `Alt` and an arrow on a focused row.
It is a list somebody WROTE, and it can name a task a colleague holds as
readily as one of the person's own, so it is not the set the Queue's tab
counts: its first line says how many open tasks it holds and how many of them
are assigned to the person ("5 open tasks on your list, in the order to work
them — 2 of them assigned to you. The Queue counts only the work assigned to
you.").
The write is `set_priorities` as the reader, carrying the WHOLE stored list
with the moved row at its new place (entries the page does not draw keep
theirs) and `if_match` the record's version; the row sits at its new place
with a pending mark until the engine answers, and a refusal puts it back in
the engine's words. Nothing is optimistic — and nothing else moves while the
write is out: the next move is held, but every row keeps its grip, so the
list does not step sideways and back. A person's seat page counts the same
list as **Priorities** in its "Their day" card, never as their Queue: the
Queue is the open work assigned to them, everywhere it is named.

**Asked of me is the decision row Home draws** (`DecisionRow`): who asked,
the role, what they recommend, and the options as buttons that send the
choice. It is shared rather than restated, so the rule for which option is
primary is one rule.

The whose-day picker in the page bar lists yours, then your line, then
anybody, each person on two lines (the name, then the handle and how much is
open on that desk), and its panel is tall enough for six of them before it
scrolls. Every row of a list is the same height whether or not somebody holds
its task — the holder's cell keeps a badge's height with nobody in it.

**Whose day it is decides the pronoun and the controls.** The band above the
sections names whose day is on screen and carries the stamp when somebody else
put the queue in order — by the person's name. On somebody ELSE's day every
row speaks about them ("Rui is the approver") and every change on the page is
HELD (`HoldWrites`): drawn, disabled, and explained once in words above the
rows, because the questions put to them are theirs to answer and their work is
changed from the work screens. The one change released there is a LEAD's
reorder — anybody above them in the chart, as the tracker defines a lead — and
the page says before the press that the reorder is stamped with the reader's
name and tells them what is now first. Where the engine sent no hierarchy,
whether the reader leads them is unknown and the sentence says so.

### A task's page is one reading

`#/work/{KEY}` is the approved Issue artboard: a reading column and a rail
beside it, each its own scroller and flush with the sheet (the page asks the
shell for the window's height, as the Inbox does, and gives it back below a
phone's width, where the rail follows the column in one scroller). There is no
header card: the page bar's trail names the project by its name with its key as
a chip ("Work › ENG Core platform › ENG-412"), and the bar carries ↑ ↓ "3 of 18"
when the task was opened from a list — by its row's link or through the peek's
**Open**, which carries the list the peek was opened from — with `k` and `j`
stepping to the task before and after it (the list's own two keys, standing
aside while a grid on the page holds them), **Watch live** while a turn is running on it, Restore for a task
in the trash, and a menu with **Open on the board**.

**The column is the task in the order a person reads one.** The exceptional
flags only (in the trash, blocked, archived), the title (24px, edited in place),
the description as prose (edited as its markdown), each named checklist ("Done
when") as boxes a writer ticks, and the sub-tasks as a card whose head is "1 /
3" and a meter, each row the child's status mark, key, title and holder. A
status mark is drawn in the artboards' own shapes, one per status — an empty
ring for to do, a ring filled a half for in progress and three quarters for in
review, a filled check for done, a struck ring for cancelled and a quiet disc for
closed — so the state reads by shape as well as by hue, everywhere a task is
drawn — with
a **+** that files a new one under this task. A failed sub-tree read is drawn
where the rows would have been, because a refusal that drew nothing would say
the task has no sub-tasks.

**Activity is one timeline read down**, over three histories each read a page
at a time — `work_activity` for what changed, `work_comments` for what was
said, `work_item_turns` for the agent turns charged to the task — and merged
only down to the newest point any of them with more pages stopped at, so a
stretch it draws is complete in all three; **Earlier activity** reads the
history that stopped there. All | Comments | Agent turns | Changes narrows it.
A change is a sentence about the task ("Jane Founder filed this in ENG",
"CTO assigned it to SWE — “owns the provisioner”", "Jane Founder ticked 1 on
Acceptance — 1 of 4 done"), with a quiet **· Who this reached ›** at the end of
its line opening its routing: the reason each person was woken, whether it asked
anything, and the three different empty answers in three sentences. A comment
is a card with its ask, answer or choice and a **Reply**. A turn is the
artboard's card: "SWE ran turn 1", its phases as checked steps — the one a failed
turn broke in wears a cross in the danger tone, and its pill says "failed in
review" — a pill for a send-back ("sent back for another pass") or a park, what
it did in the agent's own words (a turn an older build recorded says "No summary
recorded — see the trace" in one quiet line), the reviewer's request on the raised rung with the
caution ink on its mark, the tools it called as chips ("run_sandbox ×4"), and under them, on
a line of its own read from the left, its wall time and tokens and **Trace**. The number is the TASK's count of turns,
so the newest card and the cost panel agree. While a seat's turn is running on
this task — joined on the item the engine charges it to, and only while the
seat is `working` — the foot of the timeline is the live row in the working
state's own ground ("SWE is on turn 3 · executing · round 7 of 25 · for 2m",
with a pulse that is static under reduced motion) linking to the live trace.
The words after the turn are the task card's strip's own, from one derivation
— the round counted from one, as the stepper counts it, and the phase named
("reading context", "reviewing", "3 workers running", "coding run") — so the
card, the row and the seat's profile never name different rounds of one turn.
The composer is last: a comment, a reply, or **Ask…** — a question put to one
seat, optionally with two to six options, the one recommended and whether the
answer is the decision or an input to it.

**The rail is the task's fields, each the control that changes it.** Status,
priority, assignee (with **Reassign**, whose dialog asks why), reporter,
project, type, labels (the project's declared set), due and start, and the
estimate, then any custom fields and the record (created, updated, in status
since, filed into). A row a change touched after the task was filed says who
last set it, named as the person the activity names — a bound token's change is
drawn as that token's seat, and the reporter a token filed as is the seat the
engine names beside it (`reporter_seat`); the create itself draws no line,
because the reporter row and the record already say who filed it and when. Then the relations — "Part
of LEAD-12 2.4 release" first, then each link named from this end — the pages
it cites by their titles, **What this task has cost** (turns, tokens and agent
time from the task's own counters — tokens only, never money) with the
hand-offs against the engine's budget, and who is watching with **Watch** /
**Unwatch**.

**Every change is made as you, and never hidden.** The fields share one
`update_work_item` and one refusal line, conditional on the version the page
was drawn from, so a race lost to somebody else names who won it; a checklist
tick and following are gestures and carry no version. A reader who cannot act
sees every value as a value, every button disabled with its reason, and the
reason said once above the title.

The peek (`peek=item:{KEY}`) is the same page in one 420px column: the task's
identity as its head, the rail above the body, and no cost or record.

### The org chart is the company running

`#/agents` is the approved Org artboard: one card per seat, each under its
primary manager — the engine's derived `manager`, the line escalation goes up —
drawn on the design system's tree canvas with elbow connectors. The seats of a
unit under the lead they report to sit in one dashed box labelled with the unit
("Engineering · Core", or "Developer Relations" where the lead's own card
already says "Product") and the key of the tracker project its work is filed
under. A lead's own unit is not boxed round the seats beside the lead: the CEO
and the CTO who share Executives are peers on a line, not a team. The chart is
built by `lib/orgchart.ts` from the projection this node has APPLIED, never the
builder's draft — a test holds that it imports none of the builder's model —
so between a save and this node applying it the chart has not moved, and the
"still applying revision …" note says so. An engine that reports no derived
hierarchy leaves every seat a root and says why, rather than drawing a
hierarchy the client made up. **The field runs to the sheet's edges**, dotted,
under the tabs, as the artboard draws it — the screen's padding and the
canvas's own frame are taken back for this one screen, because every pixel of
them was a pixel of every card. **The chart opens fitted and centred**, and so
does Fit to view: all of it, in the middle of the canvas. A person who reports
to nobody and leads nobody is a root with no tree, and the canvas packs such a
card beside the root it follows on the top row; placed after a founder whose
tree already leans right, it pushed the top of the chart further right, so the
chart read as sitting right of centre even with its box centred. Such roots go
to whichever side of the tree centres the top row over the drawing
(`balanceRoots`), a tie keeping the document's order.

**A card says who, where and what, in that order**, in the artboard's 184 px:
the badge (a circle for a person, a squircle for an agent) with the state ring
and the NAME, which has the first line to itself; then the Agent/Human pill
leading the unit path, where the unit is what gives way (beside the pill the
name had some sixty pixels, and every "Agent …" seat read "Agent S…"); then
the engine's state line ("Executing
ENG-412", "3 workers on ENG-405", "Needs you · run parked", "Stopped · budget",
"Idle · last turn 24m ago") after a dot in the state's hue. **Colour shows what
a seat is doing — never who it is**, which the section tabs say at the end of
their row on every Agents section, wherever the row has room for it beside
every tab (it gives way to the tabs, so a phone does without it): the ring, the dot and the line are the only
hue on a card, and no seat has a colour of its own. The legend at the chart's
foot counts Working, Needs you, Stopped and Idle from the engine's `activity` —
a seat with no row yet is counted nowhere, and a person never.

**A card opens the seat beside the chart** (`peek=seat:{handle}`); Enter does
the same, the arrows walk the tree, `[` and `]` step the seats in the order the
tree reads — pressed on a card as well as in the peek, since a card's own keys
would otherwise read them as type-ahead — and while the peek is open it follows
the card that has focus. A step replaces the address rather than adding to the
history, so Back closes the peek however many seats it walked through. **A new
width is fitted again**: the canvas fits the chart once, and the peek then
takes a share of it — so whenever the canvas's width changes (the peek opening
or closing, the window resized) a chart still at the view it was last fitted to
is fitted again, all of it and centred, as the approved chart draws it beside a
peek. **But never shrunk past reading**: a view the chart chooses on its own —
its first fit and every refit — is drawn at exactly 85% where the fit would
fall below it (the card's 12 px place and state lines then draw at 10 px),
centred across on the seat the peek is on, or on the root the company hangs
from where nobody is selected, and down the canvas where the kit's own fit
would put the chart, pulled just far enough to keep that seat on it. Beside a peek
the Nimbus chart is whole at 94% at 1440 and drawn at 85% at 1280, where a fit
was 54%; only a Fit the reader presses draws it smaller. A view the reader moved — zoomed in to read the cards, say — stays
theirs. Either way the card the peek is about is revealed (the least pan that
shows it), and a reader stepping in the peek keeps their focus. **On a phone the chart is rows**: a chart of
cards has no size a phone can read it at, so below the phone breakpoint the
same tree is the design system's tree grid — every seat a row indented under
its manager, with the badge, ring, name, kind, place and state line a card
carries — headed by the legend; a row's name or Enter opens the seat. The page
bar carries **Find a seat** (name, handle or unit; the found seat is focused on
the canvas and opened — its list grows past the field so a seat's name is never
squeezed out by its handle and unit), **Edit org**, and **Add seat**, which goes
to the builder for an operator and is held with its reason for everybody else.
On a phone Edit org folds into the bar's More menu, so Add seat stays in view.
It is the same bar on every Agents section, Schedules included, where a found
seat opens beside the list as it does on Teams.
The section tabs carry the Roster, Teams and Schedules figures on every Agents
section, as plain numbers in the tab's quiet ink — which is why Schedules no
longer restates its count in the bar. The Agents sections open on their work,
with no introduction above it.

**The seat peek answers "who is this, and what is it doing".** A state card
tinted by the state — the line, how long the turn has run, "Turn 2 · round 7 of
25 · coding run in progress", where the turn number is the task's own count
(`work_item_turns`) — then Reports to, Model (the phase chain from the public
projection and the model serving the call in flight), one row per capped
budget window labelled by its own period ("Budget today", "Budget this week")
with a meter in the engine's state, or "No seat budget — the company's applies"
where only the company's ceilings bind it ("No budget" where nothing does),
Running on (the node that holds the lease and since when for an operator;
every other reader reads what the public health push says — this node by name,
or "another node" — which is what the profile's Setup card says too, and no
guarded read is sent), Open work (`work_workload`), where its tools come from,
and its goal, drawn as the inline markdown it is written in. It asks at most three questions. **Open profile** and **Message** are the
rail's foot, pinned to its bottom edge; Message opens the New task sheet with
the seat as assignee and `ask` set — on the project filed under the seat's own
unit, or the nearest unit above it that has one — so the question is filed as
work the seat owes an answer on and the answer lands in the Inbox — there is no
person-to-seat chat.

**Roster** (`#/agents/roster`) is every seat as a card, grouped by state, by
unit or flat, and always in NAME order —
never by a field a push moves. A seat the engine still reports and the chart no
longer holds is its own group, "Removed from the company". **Workload** is who
is carrying how much (`work_workload`). On the Roster the bar's **Find a seat**
is the list's filter (`q=`: name, handle, goal or unit, and on Workload name,
handle or unit) — one field, where every Agents section has it, rather than a
second box in the roster's own toolbar. **Teams** (`#/agents/teams`) is the
units nested as the document nests them — the seats above every unit first —
each with its project key, kind, effective lead ("(inherited)" where it is),
headcount, purpose, goals and seats; without the engine's derived block a unit
declaring no lead reads "Lead not reported by this engine" rather than a blank.
A unit's own page is `#/agents/teams/{unit}`.

### A seat's profile

`#/agents/seats/{handle}` is the approved Agent artboard: a head flush with the
sheet — the seat's badge (a squircle for an agent, a circle for a person) with
its state ring, the name, an **Agent** or **Person** pill and, for an agent, the
state in the engine's word ("Working", "Needs you", "Paused", "Stopped ·
budget", "Not placed", "Idle"); under it the line that places the seat
(handle · unit path · reports to its manager) — and the tab strip on the
head's own rule. The trail reads **Agents › Engineering · Core › SWE**: the
unit is a way to its team, and the seat's crumb wears its badge.

**The kind decides the tabs.** An agent has Overview, Work, Turns, Memory,
Schedules and Settings; a person has Overview, Work and Settings. Turns, Memory
and Schedules are properties of a RUNTIME, and the engine never spawns a
person — so a person's profile asks nothing a runtime answers: no activity, no
memory, no turn list, no phase history. A `tab=` naming a tab the seat has not
got — a bookmark, or a link made before the tabs were renamed (`model`,
`cost`, `access`, `threads`) — lands on Overview, and `app/source.test.ts`
holds every link to a seat to a tab its profile has. The Work tab counts the
seat's open work — the engine's `total_hint`, the same answer the Overview's
card and the Work tab read.

**Three actions, in the page bar, each a write control held with its reason**
for a reader who cannot make it:

- **Message** opens the New task sheet with the seat as assignee and `ask` set:
  the question is filed as work the seat owes an answer on, and the answer
  lands in the asker's Inbox. There is no person-to-seat chat.
- **Assign task** finds an existing task by search and hands it over against
  the version a read of it returned (`update_work_item{assignee, if_match}`),
  so a task somebody moved in between is refused rather than taken from
  whoever holds it now — or files a new one for the seat. The matches list the
  tasks that CAN move first; the seat's own come last, drawn but disabled and
  marked "already theirs", so a term most of whose matches the seat already
  holds still offers eight it could be handed.
- **Pause** (agents only) asks why, and whether to **also stop the current
  turn**: without it the turn the seat is on finishes first, and only a ticked
  box sends `stop_running` (see [pausing a
  seat](../concepts/agent-runtime.md#pausing-a-seat)). A paused seat's profile
  says who paused it, when and why, and offers **Resume**.

"Events" (the [event log](#the-routes) narrowed to this seat, `seat=`) and
"Edit in org" (the builder opened on the seat) are the bar's More menu. Events
is offered on an **agent's** profile only: `seat=` narrows the log to the
events a seat published under the id every node derives for it, and a person
publishes none — the engine never runs a human seat. On a phone the bar keeps
**one** action in view — Message, or Resume on a paused seat — and folds the
rest into the same More menu, each disabled there with the sentence its inline
control is held with. A notice under the tab strip, above whichever tab is
open, carries what stops the seat or waits on somebody, in the state's own
tone: its last error, a pause, another stop, or a coding run parked on a
question — answerable from the notice.

**The Overview is the seat's week and its charter.** Four figures, all ONE
answer (`seat_activity` with `previous`, every node's usage rows summed): turns
over seven company days with the change against the week before, the
engine's first-pass rate over the turns it REVIEWED (a week nobody reviewed has
no rate, never 0%) with how many reviews sent work back, the median turn with
its p90, and tokens — the seat's capped window nearest its ceiling, over that
window's ceiling ("of 3M budget"), or the seven days' tokens where the seat
has no ceiling of its own — "company cap 60M/day +1" (and "no seat cap" on hover) where the
company's own budget caps a window, because that ceiling binds every seat (one
line in the tile, the whole sentence on hover). A shortened token count carries
no zero fraction: `60M`, never `60.0M`. The **current turn** card shows where the turn is — Context, Execute
("round 7 of 25", against the cap the phase was granted), Review; there is no
delivery step, because whether a turn reached anybody is decided as it
closes — the turn's number on its task and the task's title (read by the
turn's own key, never looked up in the card beside it), its elapsed time and
tokens so far, and the phase's calls, each with the engine's own start time and
duration, the running one last. A call reads as words, never JSON: the tool's
name whole in a column as wide as the longest name the phase called, then the
argument that says what it is about bare (`ENG-38`, the query, the command)
and every other argument by its name (`assignee me · open`). Between turns it is the seat's state in one line and its
last turn. Beside it are the seat's open work (the five most urgent, and "All
n") and its turns per company day over the fortnight the delta spans, today in
the accent. The side column is **About** (the goal to three lines and the first
three responsibilities to two each, as inline markdown, and "Show all" where
that cut anything — a charter is the seat's prompt text and is never
summarised), **Setup**
(the model chain and tool grants from the public chart; the sandbox,
placement and workers from the company document, said as unread rather than
empty for a reader without an operator token; the node holding the seat now —
the fleet's lease for an operator, and for anybody else what the public health
push says, this node by name or "another node", which the seat's peek says
too; and Edit), **Memory** (the holder's counted totals and its newest
reflection) and **Schedules**. A person's Overview is **Their day** — or
**Your day**, with the way to your Inbox and My work, on your own — with the
unread notices waiting on them (the inbox's own count, the sidebar badge's
reading on your own day, "50+" past a page), their priorities — the engine's
count of what on the list is still open, the figure My work and their Work tab
read, never the stored list's length, which still names a task they finished
until their next reorder — and who set them, and pinned views, for the person
and an operator, and withheld with that sentence for anybody else; their open
work; and About.

**Turns** is the seat's own turns (`turns{seat}`, every node, over the thirty
days the event store keeps, a page of fifty at a time with "Load older turns"
following the engine's cursor, and a count of what is loaded — "50+" while an
older page exists), with **Running now** leading the tab so a turn in flight is
on screen as it opens, and the settled transcripts after the list under their
own heading, **Transcripts · newest first**. A row for a turn still running says what the seat is doing
and what it is on, from the push ("running — executing · round 2 of 24",
ENG-32), and draws its iterations and tokens as not settled rather than as
zeros — `#/live/turns` does the same. On a phone a row's summary takes two
lines with its item under it, and a settled turn has no State line at all. And
**Where its tokens go**, its week by phase and by model — the buckets that spent
some, since calls that failed before a model answered recorded none and were a
bar of nothing named "unknown" — with the way to all of its activity on Live. **Memory** is read from the node HOLDING the seat, whose
copy is the one kept current, and says which node answered: the diary, the
episodes (each outcome in the reviewer's own tone — `failed` is red here as on
a turn), the skills it taught itself and who it has worked with (when last,
relative, as every list on the profile says it), each header
the holder's total ("Latest 50 of 142" where the list is a page of it), and the
**Conversations** it holds a ledger in (`conversation=` opens one) — each
thread's KEY on a line of its own, since it is the thread's only identity, with
its turns and when it last moved under it, and a native task's uuid cut to its
head (`work:task:4d631f6d`) with the whole key on the row's title. Choosing a
thread shows it: beside the list (a wide column) the list scrolls inside a
viewport-high box and the thread's turns stick under the page's top; stacked
on a phone, the turns are scrolled to and their heading takes focus. Each
turn's trigger reads as plain words, its markdown stripped. A seat no
node holds says why it shows nothing. **Settings** is the company document's
half — identity and contacts, the model and budget with each capped window's
live meter, and a seat's tool credentials by server and **variable name only**:
no value the engine sent for a credential reaches the page, not the reference,
not the mask. Every value there is changed in the org editor ("Edit in org").

### The attention queue

`dashboard/src/lib/attention.ts` is one derivation because it is one question,
and every one of these conditions was already known to the dashboard and each
lived in a different screen:

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

**Each condition has ONE home**, by its subject (`WHERE_OF`), so none is drawn
twice and none nowhere:

| Home | Subjects | Why there |
|---|---|---|
| `seat` — the Inbox's "Needs a decision", and Home's "N conditions need a look" | the seats' own state, the token budgets | a person decides these: raise a ceiling, resume a seat, look at one that failed |
| `engine` — the sidebar's health card and Home's status sentence | the engine and this node's link to it | a fact about the product the reader is looking at, on every screen, not an item in anybody's queue |
| `live` — Live › Now running, beside what each is about | a round that has stopped moving (a caution or failure mark on its own running-turn row, never while the turn is parked), every coding run parked on a question (a row of **Waiting on a person**, answered in place) | watched rather than decided; a run waiting on a PERSON already reaches them through their own decisions, and a list of alarms restating the rows around it was a second copy of each |

On Home the queue is COUNTED, not drawn: the `seat` conditions that are not
the reader's own decisions — a seat stopped for a reason other than its
budget, a company budget near or at its ceiling — are the status sentence's
"N conditions need a look", linked to the Inbox, and the engine's own
conditions take the sentence over. What the reader can settle is the
decisions card beside it.

**What a quiet list says is derived, not written.** Every condition names one
of five subjects, `SUBJECTS` maps each to the phrase a reader sees, and a place
that lists none draws the phrases of exactly the subjects whose home it is
(`watchedIn`). That sentence used to be prose on the screen
— "No seat is stopped, no run is parked on a question, and no budget is
refusing" — three of the twelve conditions, read as the whole list, so an
operator whose engine had no active configuration or whose node was shedding its
seats was told in a closed sentence that those had been checked and were clean.
A thirteenth condition cannot be pushed without naming a subject, and a new
subject fails the build until it has its own phrase.

---

## Markdown is rendered, not printed

Page bodies, work-item descriptions and both kinds of comment are **markdown
by contract** — every one of the tools that writes them says so in its own
schema — and the dashboard printed them as literal characters: a reader saw
`## Decision`, `- [ ] ship it` and `[the guide](https://…)` as the text an
agent typed rather than as the document it wrote.

`lib/markdown.ts` renders a CommonMark subset to React nodes. It is in-tree
for the same reason the rest of this engine writes its own small pieces: a
markdown library's real surface is its HTML passthrough and the sanitiser that
has to follow it, and this renderer needs **neither** — it emits no raw HTML
under any input, so there is no `dangerouslySetInnerHTML` on the path and
nothing to sanitise.

Two rules are load-bearing, because a page is written by an agent acting on
content it read somewhere else:

- **Raw HTML is text.** A `<script>` in a body renders as the characters
  `<script>`, and the reader still sees what the page said.
- **A link's scheme is an allowlist** — `http:`, `https:`, `mailto:` and the
  app's own `#/` routes. Anything else (`javascript:`, `data:`, a
  protocol-relative `//host`, a bare `/path`) renders as its own **label**
  rather than as a link, and an image with a refused source renders as its alt
  text. An allowlist rather than a denylist: enumerating the bad schemes is
  wrong the moment a browser grows one more.

An outbound link opens in a new tab with `rel="noopener noreferrer"`; an
in-app `#/` link does neither, because it *is* this application. A task
list's checkboxes are disabled — this surface performs no writes, and a box a
reader could tick would report a change that never reached the page.

`.prose` on its own stays a `pre-wrap` block, which is right for a model's own
speech: its line breaks are load-bearing there and it is not markdown. A
document carries `.prose.md` beside it.

**A fenced block wraps rather than scrolling**, and it took a wrong class name
to notice it did neither. The renderer emitted `class="code plain"` on every
fence — two names this stylesheet spends on something else (`.grid-th.plain`
is a table header) and neither of them a recipe for a `pre`. So a fenced
sample had no surface, no padding, and a `pre`'s own `white-space: pre` with
`overflow: visible`: one long line of JSON in a page body, a work item's
description or a phase prompt pushed the whole page sideways — 1378px of
scroll width in a 700px viewport, measured. The class is `md-code` now, in the
same family as `md-table`, `md-tasks` and `md-task-body`, and its recipe
wraps: that is the design system's own default for a block a reader *reads*,
and unlike a scroll container it owes no tab stop. The forward half of the
class gate could not see any of it, because it read a `className` **attribute**
and the renderer writes a `className:` **property** — so that half reads both
now, and both directions of the gate fail on the old name.

The same file also **splits a document into its sections without rendering
anything** — `splitSections` for the flat run, `nestSections` for the outline
its heading levels describe — for the one surface that needs a document's
outline and its source both: a phase prompt, which the transcript folds on its
own headings, renders one section at a time, and hands back byte for byte on
the other view (see rule 11 below). A walk that rendered as it split could give
neither back. It lives beside the renderer because what a heading is and where
a fenced block suspends the grammar are decisions the renderer has already
made — written again next to its caller, the two would drift exactly as
`textcut`'s four copies and `whsec`'s two did, and the clause that would go
first is the fence.

A page's history shows **what a save changed**, not only what one version
said. `lib/diff.ts` is a line diff over the two revisions — Myers, by line,
with no word-level refinement inside a changed line, because these documents
are rewritten in paragraphs and a word diff makes a small edit prettier and a
large one unreadable. Unchanged runs collapse to a row saying how many lines
they stand for: a gap silently closed makes a document edited at both ends
look like one rewritten in the middle. "This save did not change the body" is
its own answer, because a title, a label and a move are saved beside a body
and a pane of unmarked lines reads as one that failed to load.

## ⌘K: search, answer, act

The command palette is one box for the whole product: where to go, what the
company knows, and the three changes a person most often makes from a search.
It is the design system's `CommandPalette` — the combobox, the scope tabs, the
answer's live region and the key legend are the kit's — and `app/palette/` is
what it searches and what a row does.

### Five scopes, three sigils

A launcher with one index answers "where do I go", and four questions do not
fit that shape, so each is a tab under the field:

| Scope | Searches | Sigil |
|---|---|---|
| **All** | screens, a pasted event / trace / turn id, three tasks and three pages (two of each under an answer), agents, people and teams, tools, the three actions, and the answer's remaining sources; recents when nothing is typed | |
| **Tasks** | `work_search`, hybrid, eight hits, and a row to the full search screen | `#` |
| **Pages** | `knowledge`, the company's one knowledge backend | |
| **Agents** | the org chart — the engine's own name tiers first (`colleague`), each seat's ring from the agents push, then people and teams | `@` |
| **Actions** | the per-browser commands — theme, density, dates, time zone, copy link, set token, clear recents, the key legend — and the three changes; the legend names who they are made as | `>` |

`Tab` and `Shift+Tab` walk the tabs from the field, and a sigil typed at the
start selects its tab and leaves the box, so the tab row — not a character the
reader has to know is special — says where they are.

**A server search has four answers, and each says which.** Too short to run
(under two characters), searching, refused — the refusal in the one table of
what each code means, since "this node does not serve it" and "the socket went
away" both read as "nothing matched" otherwise — and nothing matched. A
building index is its own sentence, because the work exists and is not
findable from this node yet. A term's hits are never shown under the next
term: the answer on hand is remembered against the term it answers.

The pill at the end of the scope row is the mode the search was SERVED in —
"Hybrid search", or "Keyword search" on a company with no embeddings — never
the mode asked. At most three of the socket's four query slots are the
palette's (`work_search`, `knowledge`, `colleague`; a picker holds two), so the
screen under it keeps one. The company's projects (`work_projects`) are read
once as the palette opens, before a term can be typed, so they never contend
with the three. A term longer than the engine resolves to a colleague (200
bytes — a pasted log line) is not sent to `colleague`, and the chart's own
matching answers it; the cap is `COLLEAGUE_QUERY_MAX` in the contract, held to
the engine's by a Go gate.

### The answer, and when it spends tokens

A term that reads as a question — **three words and twelve characters** —
gets an answer from the company's own pages and tasks above the rows, written
by `answer_knowledge` as the person asking. Every answer is a model call
charged to the company's budget windows, so it is asked only when:

- the scope is All or Pages;
- typing has paused for **800 ms** (a typist's gap is 150–250 ms, so this is a
  question finished rather than one being typed);
- this browser may ask: a token bound to a person, on an engine that answers;
- the question has not been answered already this session — answers are kept
  per normalised question for as long as the page is open.

At most one is in flight: a new term abandons the question before it, and an
answer is only ever drawn under the term it answers. It shows its sources as
chips numbered the way the answer cites them (`[1]`) — one line of them, the
first four and "+N" — and every source the hits did not already list is a row
under **Sources of the answer**, below the actions, so the keyboard reaches
all of them and the three changes stay in the first screen. A term that reads
as a question also trims All to two task and two page hits, from the moment it
is typed rather than when the answer lands, so the list does not shrink under
the highlight. The answer ends with a footnote in **tokens** — "1,440 tokens, charged to
the company", or "Answered before — no tokens spent" — never money. An
anonymous or unbound reader gets one line instead: *Set a token bound to your
seat to get an answer from your company's knowledge.*

### Three changes, made as you

Each is a `useAct` write, attributed to the person the token is bound to
(ADR-0024), and each row is drawn for every reader — where this browser cannot
act, its hint is the reason, and a press on it says so again rather than doing
anything.

| Row | Key | What it does |
|---|---|---|
| Assign {KEY} to an agent… | `Alt+A` | Opens a picker over the agents — the one holding the task marked "holds it" — and assigns the task at hand (the one on screen, else the best task hit) on a pick — `update_work_item`, conditioned on the version the picker read, so a task somebody moved meanwhile is refused rather than overwritten |
| Ask {agent} about "…" | `Mod+Enter` | Files the term as an ask on a new task (`create_work_item{title, assignee, ask}`), whose answer lands in the asker's Inbox; with an answer on screen, the task's body carries it |
| Create task "…" | `Alt+C` | Files the term as a task and opens it |

**Nothing is picked for the person.** The agent a row suggests is the one the
term NAMES — the engine's `colleague` `match`, set only when exactly one seat
matches through the same tiers an agent's `lookup_colleague` uses. Several is a
list and no name is the whole roster; either way the person presses the row.
The same holds for where a create lands: the project on screen, else the one
the person's own create defaults to (`viewer.project`, the engine's own
derivation — the one the operator surface's `create_work_item` applies), and
the row names it ("in ENG · Core platform", why that one on hover). Where
neither says, the press opens a **project step** listing the company's
projects, the one the top task hit is filed in first as a suggestion — so a
founder on Home can file work without first navigating to a project. The only
reason a create is refused outright is a company with no project at all.

**A row that cannot act is drawn, not disabled.** The kit's palette rows take
no disabled state, so a write this browser may not make (F-7) stays a row
whose hint is the reason; a press on it — or its key — adds that reason as a
line UNDER the answer, never in its place, and sends nothing. A screen reader
hears the reason as part of the row.

A write row keeps the palette open until the engine answers: a refusal is drawn
in it, in the engine's words, because a refusal on a palette that already closed
is one nobody reads. An applied or pending change closes it with a toast; an
unknown one closes it with the toast that stays, carrying Retry.

### It remembers, and it closes

An empty palette offers **recents** — the last few objects this reader opened,
most recently visited first, per browser. Objects only: anything the sidebar
already lists is left out, because a recents list repeating the navigation
beside it costs a reader a scan and tells them nothing. The label stored is
the one the **screen** resolved, which lands a render after the route — so a
recents row says what a turn did, what a trace began at, what a coding run was
asked for, and falls back to the id only where nothing has named the object
yet. See [A screen publishes what the chrome needs](#the-frame).

**One reader, so one order: most recently visited first.** The palette is
opened fresh, ranks what it offers and closes, so it has a re-sort boundary and
"where I just was" is what an empty query should put first; the list is stored
that way, and at its cap of eight the bottom row — the place nobody has opened
in longest — leaves. The sidebar keeps no recents: a place a reader keeps on
purpose is a star. A route no workspace owns is not remembered, and a stored
row whose route this build no longer has is dropped when the list is read.

**And a name is never replaced by an identifier.** Every screen publishes its
object's name a render after the route, so the first write of every navigation
carries the raw segment — a uuid for a turn — and the second carries the name.
`remember` takes whether a screen supplied the label, so a revisit never
downgrades a named row to its id. A place nothing has EVER named still stores
its id, because an object with no name has that and nothing else.

A command that would change nothing is not offered: the list omits the theme,
the density and the date format already in use, because a control that says
"switch to dark" while the page is already dark does not know what it is
looking at.

**It closes when the route changes**, the way the workspace drawer does.
Picking a row closes it on the way out, so what this covers is every other way
the route can move while it is open — Back, Forward, a phone's back gesture, a
restored history entry. `Mod+K` closes it from its own field. The token dialog
is deliberately not in that rule: a credential prompt is about the reader's
access rather than about where they are.

---

## Acting, as yourself

**The dashboard acts as you.** Every change in a Crewlet company is attributed
to whoever made it, and the one actor an audit trail cannot name is "the
dashboard" — so there is no such actor. A write from a browser is made by the
**person your token is bound to** (`contact.crewlet_operator_id` on your seat),
through the same operator tools your own assistant calls, and it is recorded
exactly as that assistant's would be: your token as the author, author kind
`operator`, your seat as the person.

**The audit log draws an operator as a person.** A write your token made
reads as you — the person's circle and your seat's name, linking to your seat,
with the token in the tooltip and in the export's `who` column (`who_seat`
beside it names the seat). A token no seat binds is a person too, but it has
no page: its row is the circle and the token's name as plain text, never a
link to a seat of that name. A seat's own write takes the chart's kind, or,
for a seat the chart no longer holds, the kind the write was recorded under.

The engine's half is [`POST /operator/act/{tool}`](api-endpoints.md#operatoract--the-dashboards-write-surface):
one catalogue tool per request, a `request_id` the screen mints per gesture
and repeats on a retry, and an answer carrying the write's `outcome` and
`position` — the floor the screen's next read waits for. The live socket stays
read-only; writes and credentials travel over REST. A token no seat binds, and
every caller while the guard is disabled, is refused `unbound`, and
`viewer.acts` is empty for exactly those callers — which is what a screen
reads to disable a control and say why, rather than offer a press the engine
refuses.

### A write control is never hidden

Every control that changes the company is drawn for every reader. Where your
browser cannot make the change it is **disabled, and says why** — the kit
button's `disabledReason`, which keeps it focusable and reads the sentence to
a screen reader as the button's description. The kit draws that sentence for
nobody else, so the same words are the button's `title` while it is held — a
pointer resting on it sees why — and a screen reader, which reads a described
button's description rather than its title, hears it once. A form whose
primary action is held also writes the reason on the page beside it (the New
task sheet's foot), because neither reaches a touch screen. The five reasons,
in the order you clear them:

| You are | The control says |
|---|---|
| Offline | Offline — reconnect to make changes. Nothing is queued while you are away. |
| Not yet known | Checking who you are before anything can be changed. |
| Anonymous | Set an API token to act — every change is recorded under your name. |
| An unbound token | This token is not bound to a person — set contact.crewlet_operator_id on your seat to act as yourself. |
| A person the engine does not make this change for | This engine does not make this change for you. |

**One press at a time, by every way in.** A write control refuses a press
while its last one is still out, and so does every other way into the same
press — a dialog is a form Enter submits without touching the button, a reply
field sends on ⌘Enter, a one-line form (a sub-task, a title) files on Enter —
because each reaches the same gate (`pressable` in `components/WriteButton.tsx`).
They used to go straight to the write, and a second Enter before the first
answer came back filed a second sub-task or posted the comment twice, under a
new request id the engine rightly took for a second change. On a task page a
change conditional on the version you were looking at is not sent at all while
another is out: it carries the version the first is about to move, so the
engine could only refuse it, and the page would then have reported your own
first change as somebody else's. A checklist tick or a follow, which carry no
version, still go.

A change is **never queued**: a write sent when the connection came back would
be one you walked away from believing it had happened, onto a company that may
have moved since. `viewer.acts` is the engine's own list of what it makes for
you, and the last row is read from it, never guessed from a tool's name — a
company whose tracker is Jira has no native writer, and its task page shows
Assign disabled rather than a button the engine refuses.

### Writes are confirmed, not optimistic

Nothing on screen moves until the engine has answered. Each answer is one of
four, and each tells you something different:

| Outcome | What you see |
|---|---|
| `applied` | A toast naming what changed; the screen is re-read and redrawn with it. |
| `pending` | "Sent — this node has not applied it yet." The re-read waits until this node has. |
| `unknown` | "Could not confirm — it may have landed", which **stays** until you dismiss it, with Retry. |
| refused | The engine's reason, beside the control that caused it, until your next press. |

**The re-read waits for your write.** An `applied` or `pending` answer carries
the position its record landed at in its domain's log, and the tab keeps it as
a **read floor** for that domain (`protocol/session.ts`) — a per-tab value that
only rises. From then on every question that reads that log
(`contract/domains.ts`, held against the engine by
`TestEverySessionQueryTakesAFreshnessFloor`) asks with
`read_level=session&min_position=<floor>`, which the engine answers only once
the serving node holds that position. So the list you just changed never comes
back from a node that has not applied the change, whether the next read is the
refetch your press fires or a poll a second later.

**A retry is the same write.** `unknown` means nobody can vouch either way —
the connection dropped after the request left, a gateway gave up, or the
engine's call was interrupted (the act route says `outcome: unknown` for that
one, beside its class). The dashboard never retries on its own. When you press
Retry it sends the **same `request_id`** it sent the first time, from which the
engine derives the same operations; a new press is a new id. Each Retry is
bound to the press it reports — press twice and the first toast's Retry still
sends the first press, never the second. A refusal a busy or draining node
caused offers "Try again" on the same terms; one the request caused does not,
because the same request would be refused again. A control reused across
objects — the Inbox's Mark read as you move between notices — belongs to the
object it is drawn for, so one notice's refusal never appears under another's.

**The refusal is in the dashboard's words**, except where the engine's own
sentence names the argument that was wrong (`invalid`) or the rule that
forbade it (`forbidden`): every other tool sentence is written for a model
reading a tool result. Those two carry the writer's sentence alone — the
tracker's refusals hold their words apart from the `tracker:` a Go error opens
with (`tracker.Sentence`), so a person reads "create_work_item refused that: a
decision has 0 option(s) …" rather than the package's name in front of it, and
a forbidden one no longer ends by repeating the sentinel's own "not this
actor's to write". `contract/errors.ts` `ACT_ERRORS` holds one sentence per
code and is held against the engine's codes both ways
(`TestTheDashboardKnowsExactlyTheActRefusals`). A conditional edit that lost
the race reads "Changed by somebody else since you opened it." — somebody
else, not a name, because the refusal carries the version it lost to and not
its author. That refusal, and every other that says the screen is out of date
(`conflict`, `not_found`, `exists`, `already_answered`), asks the write's
questions again without raising your read floor, so the page redraws what is
there now and your next press is made against it.

### What a screen can change today

`contract/actions.ts` `ACTIONS` is the dashboard's whole write vocabulary —
the tool, the arguments a screen may send, the log it lands in and the
questions it moves — and `TestEveryActionTheDashboardTakesIsOneTheActTransportServes`
holds it against the real operator catalogue: every tool is served by the act
route and is not a read, every argument is one the tool takes, every required
one can be sent. It carries a row for each tool a control presses and no
other — `app/source.test.ts` holds it to the `useAct("…")` calls both ways — so
a tool arrives in it with the control that sends it. The controls
(`components/writes.tsx`):

| Control | Where | Tool |
|---|---|---|
| Assign / Reassign (with a reason) | A task's page | `update_work_item` (`if_match` on the version you are looking at) |
| Status · Priority · Labels · Due · Start · Estimate · a custom field · the title · the description | A task's page, from the value itself | `update_work_item` (`if_match` on the version you are looking at) |
| A checklist box · Watch / Unwatch | A task's page | `update_work_item{checklist}` / `{watch}` — gestures applied as they land, with no `if_match` |
| + (a sub-task) | A task's sub-tasks | `create_work_item{title, project, parent}` |
| Create task (the New task sheet) | The sidebar's head `+`, the Projects group's `+`, a work screen's New task, a board lane's `+` | `create_work_item{title, project, body?, type?, status?, assignee?, priority?, due?, labels?}` — only what was set |
| Message (the same sheet, titled "Ask {seat}") | A seat's peek | `create_work_item{title, project, assignee, ask, …}` — the answer lands in your Inbox |
| Edit project (the target date) | A project's page bar | `write_project{project, target_date}` — `null` clears it; a company write, whose authority the engine decides |
| Comment · Reply · Ask… | A task's composer | `comment_on_work_item{item, body}`, `{item, body, reply_to}`, `{item, body, ask, decision?}` |
| Restore (named "Restore ENG-42" to a screen reader, so a grid of them can be told apart) | A task in the trash, on its page and in the trash grid | `restore_work_item` |
| Pin / Unpin | A saved view's page | `set_pins` |
| Done · Snooze · Mark unread | The open row in the Inbox | `mark_inbox` (exactly the notices that row holds; every other mark and your read position stay) |
| Mark all read | The Inbox's page bar | `mark_inbox{read_through}` at the newest notice LOADED — never past it |
| An option card | A decision in the Inbox's pane | `comment_on_work_item{item, answers, choice}` |
| Send / Send answer | The Inbox's composer | `comment_on_work_item{item, body, reply_to}` for a reply, `{item, body, answers}` for an answer |
| Link a task · Attach a page | The Inbox's composer | `update_work_item{item, linked}` / `{item, linked_pages}`; "Save as a page" first writes it with `write_page` |
| Answer | A parked coding run in the Inbox's pane | `answer_run{turn_id, answer}` |
| An option · Reply | A question in My work's Asked of me, and Home's "Needs your decision" | `comment_on_work_item{item, answers, choice}` / `{item, answers, body}` |
| Drag a row · `Alt` + ↑/↓ | My work's Priorities reading (your own queue, or one in your line) | `set_priorities{handle, items, if_match}` — the whole stored list with the one row moved, conditional on the record's version |

A change to the **company document** — a seat, a budget ceiling, an MCP
server, a model — is not an act: it is a new configuration revision, and every
screen that makes one goes through `protocol/configWrite.ts` (read the document
and its entity tag, or one entity and the same tag; dry-run a merge patch or an
entity replacement; save either with its audit summary), always conditional on
the revision it edited. Its gate is the operator credential rather than a seat
binding — `/config` is guarded by the API token and nothing narrower — so its
controls ask `useConfigWriteAccess()`, which is `viewer.operator`, and are
disabled with that sentence for anybody else.
`protocol/configAnswer.ts` is the one reading of a `/config` refusal — a
conflict to re-read, the drain gate's certain `503`, a request that may have
landed, or the problems the document has — which the org builder's model
classifies through too.

## Honest empty states

A screen that renders a blank where data would go is a screen that cannot be
trusted when it IS blank. Four distinctions the product makes everywhere:

- **Nothing happened** vs **nothing could be read.** "No events" on a fresh
  company and "no events" from a query the engine refused are the same empty
  list and completely different problems. `QueryState` renders the engine's own
  code (`unknown_query`, `unauthorized`, `not_found`, `unavailable`,
  `bad_params`, `query_failed`) and the client's own `timeout` as a sentence
  saying which. `bad_params` is the one that names the SCREEN as the fault: the
  engine understood the question and refused it, so retrying sends the same bad
  request again. `unavailable` is the opposite: the node will answer in a
  moment, so `useQuery` asks again on its own rather than leaving a person to
  reload — after the wait the engine named in the refusal's
  `retry_after_seconds`, never after a constant of the client's. The table is keyed on the contract's `QueryErrorCode` union, so a
  code added to the union without a sentence here is a compile error, and a Go
  test in `internal/api/stream` pins that union to the codes the engine sends.
- **Zero** vs **unknown.** The integrations answer's `skipped` and `coalesced`
  are three-valued, and a count this node could not read comes back `null`,
  never `0`; `inbound` is a plain count whose unknown-ness rides on the
  answer's own `traffic_known`. The budgets answer says `durable: false` when
  the counter could not be READ.
- **Not configured** vs **empty.** A knowledge search with no backend says so;
  a company with no seats says roles come from the configuration.
- **Everything in the window** vs **everything that arrived.** Where a screen
  narrows client-side it says so, naming the SOURCE rather than hedging the
  whole answer. **Audit** is the case that made this a rule: it composes four
  subsystems and only one of them — the tracker's feed — takes a wall-clock
  window, so the other three are asked for their newest page and narrowed on
  the client. A page that fills up before it reaches the start of the window is
  older rows the screen never saw, and a caption reading "some of this may be
  missing" is one nobody can act on where "Knowledge answered one page" says
  where to look.
- **Not recorded** vs **the engine.** An empty actor on a tracker or wiki
  commit is the engine's own write, and the Audit screen draws it as "the
  engine". A configuration revision is different: the revision states WHAT
  wrote it (`created_by_kind`, `operator` or `node`) and the row shows that
  word rather than assuming one — it used to label every revision `operator`,
  so a node's boot seed and the reconcile loop's reloads read as a person's
  writes. A revision whose kind is EMPTY was adopted from an older engine's
  pointer that named nobody, and it reads "Not recorded" on both the Audit
  screen and Configuration's history, never "the engine" and never a guess.
- **An empty CONTAINER** vs **a query that matched nothing.** A container says
  its own emptiness, from what it already knows about itself, before the list
  it holds has answered anything — and that state REPLACES the list rather than
  sitting under it. A project page handed its whole body to the work list, so a
  project nobody has ever filed anything in said "Nothing matches — no item
  matches these filters. Widen them", with no filter set: a claim about a
  narrowing that did not exist, on the day-one state of every project. It is
  drawn from the project's own maintained counts now, names the project, and
  says how work gets filed (a seat's `create_work_item`, an inbound webhook, a
  schedule, or your own assistant at `/operator/mcp`). "Nothing matches" is
  reserved for a query that genuinely narrowed. The same rule sorts the two
  empty pages apart: the work list's empty state is about ITEMS and the
  projects directory's is about PROJECTS, so a company with no projects is told
  what a project is and that a unit's `project` key in the company
  configuration is what mints one.
- **An empty SEGMENT is not an empty company — so the answer carries the
  census.** The projects directory's three segments each ask the engine for
  their own set, which is what makes the listing honest and is also what makes
  an empty answer ambiguous: an empty **Active** answer is either a company
  with no projects or one that has archived every one of them, and a reader
  acts on those oppositely. The screen cannot derive the difference, because on
  the Active segment the archived projects have no row on screen to be derived
  from. So `work_projects` answers with a `census` of BOTH sets under the same
  narrowing, and the page never guesses: `active + archived === 0` is the
  company having nothing and draws "No project has been created yet" on
  **whichever segment the reader is on** — which matters, because they land on
  Active; an empty Active answer with archived projects behind it says how many
  and links to them; and Archived says nothing is archived. The counts also sit
  on the segment control itself, so the switch says what is behind each option
  before it is pressed. A screen that hedges — one sentence naming both ways a
  state happens — is a screen missing a number, and the fix is to send the
  number rather than to word around it.

  **And the directory is not the only reader of it.** Every surface that draws
  a conclusion from an empty project listing reads the same census, because
  every one of them asks the ACTIVE set: the sidebar's Projects group, and
  the `#/work` landing, which replaces the whole list with "No work has been
  filed yet". Read from the ROWS, each concluded the first state — so a
  company that had archived all four of its projects was told by the sidebar that
  no project had been created and by the landing that nothing could be filed
  until a unit declared a `project` key, both beside a directory saying all
  four had been archived, and both false about a company holding every item it
  ever filed. `active + archived === 0` is the only thing that draws the
  first-run panel now; `active === 0 && archived > 0` keeps the ordinary list
  and gives it an empty state naming the count with the way to
  `#/work/projects?shown=archived` — the same sentence the directory's own
  Active state writes, because it is the same fact. An answer carrying no
  census concludes neither, on the rule its rows already followed.

Every empty state names what would fill it.

**And a restart is not an empty company.** The pushed surfaces come from the
engine's live projection, which is seeded from the node's own event store when
the process starts: the newest events for the activity feed, and the 24-hour
spend window the Overview and Spend screens are folded from. Until that read
existed, every one of these screens started blank after a restart, a deploy or
a node joining a fleet, which is the one empty state a reader has no way to
question. What the seed cannot cover is a fleet peer's history, because the
event store is per node; that is what the `events` query's fleet scatter and
the `tokens` query's replicated company days are for, and what the window badge
on Spend names.

---

## A cron expression is read, not printed

The schedules screen printed `0 9 * * 1-5` and nothing else. A reader who
knows cron reads it; a founder reads five numbers and a dash on the one screen
that says when the company wakes itself up.

`lib/cron.ts` reads the five fields twice over: a **sentence** under each
expression ("at 09:00 on weekdays"), and the **next five instants** on the
schedule a reader has opened. The instants matter because the engine sends one
next fire and "every 4 hours" and "at 4am" have the same next fire for most of
the day — it is the fires after the next that say whether an expression means
what its author thought.

The expression and its sentence are ONE drawing wherever a schedule is shown
(`CronReading`): the chip over the sentence, in the grid, on a phone's card and
as the Cron fact of the schedule's own header, where the fact is drawn whole
rather than clamped (`Fact.whole`) — run on after the chip and cut at two
lines, "every 20 minutes every day" read "every 20 minutes…". The sentence is
said once per page: the definition under the header carries no "Means" row.
The instants carry how far away each one is to the MINUTE ("in 1h 20m",
`inTimeExact`), because the rows of a series are read against each other and a
one-unit reading put "in 1h" beside "in 1h" for two fires twenty minutes apart.
A schedule's eyebrow names its scope in words ("Seat · Agent PM", "Unit ·
Core"), never its address (`role:agent-pm`).

Two rules keep it honest:

- **The engine is the authority.** `next_run` on a row is the engine's own
  computation and is what the screen shows as *Next*; this is a reading aid
  beside it, never a second source for the same fact. The instants are worked
  out in the zone the engine names on the row — the schedule's own, or the
  [company's clock](../getting-started/configuration.md#the-companys-clock)
  where it names none — and never in a default of the screen's: a row read as
  UTC was wrong by the zone's standing offset. A row whose zone is not UTC
  says so under the list, because each instant is then shown in the reader's
  own zone.
- **An unreadable expression says so.** The engine runs the schedule; a
  reading aid that invented a sentence would put words on screen the engine
  does not act on. The expression renders as itself with nothing claimed.

Cron ORs the day-of-month and day-of-week fields when **both** are restricted
— `0 0 1 * 1` fires on the first of the month *and* on every Monday — and both
the sentence and the instants say so, because that is the rule readers get
wrong.

---

## The transcript is stable, and reads in order

The sharpest complaint about the screen this replaces was that the LLM calls
jumped around, were hard to follow, and did not say much worth reading. Twelve
rules fix it, and each one names a specific mechanism:

1. **One identity.** A phase is keyed `turn_id|phase|iteration`, live and
   finished alike. They used to differ — the live row was keyed
   `live|turn|phase|iteration` and the stored one carried a timestamp — so the
   instant a phase completed its row was REMOVED and a different one inserted:
   the entrance animation replayed, the row relocated from the end of the list
   into its chronological slot, and its expanded state was lost with the key it
   was filed under. Now a live phase *becomes* a finished phase in place.

   That key has to be unique per EXECUTION, which is why `turn_id` names one
   run rather than the unit of work behind it (see
   [a turn's two identities](../concepts/turn-engine.md#a-turns-two-identities)).
   It named the unit of work once, and a redelivered trigger's retry then
   published the identity the failed attempt already held: the merge kept the
   older row, so the screen showed a dead call for as long as the real one
   ran. A turn's `work_key` is what relates the attempts, and a card carrying
   one says `attempt 2/2` — read off the event row's own `work_key`, which the
   server fills from the promoted column, rather than out of the payload. The
   `0029` backfill reaches the column and deliberately not the stored payloads,
   which record what a build that had no such field actually published, so a
   payload read reports no unit of work for every turn older than the split. A
   live frame is the one case with no column yet, and the payload is what it
   falls back to.

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
3. **The model's words are prose; JSON is monospace, and INDENTED.**
   Reasoning and speech get a proportional face, real leading and a bounded
   measure. Monospace stays where it carries meaning — tool arguments and tool
   results.

   Those arrive as the engine encoded them, which is the whole call on ONE
   line however many arguments it had, with the one a reader came for — the
   channel, the page id, the item key — somewhere in the middle of it. Every
   other JSON block in this product is written at two spaces, and these were
   the exception rather than a decision: the screen indents them now, and the
   same rule covers a tool's result, which is a JSON document as often as the
   call was. What that buys is a key per line, so a call is read down rather
   than scanned across.

   **It does not shorten a long VALUE, and must not.** A `create_page` body is
   four thousand characters whether or not the document around it is indented,
   because `\n` inside a JSON string is two characters. Resolving those would
   produce text that is no longer JSON, no longer idempotent and no longer
   paste-able into `jq`. What handles a long value is the block WRAPPING —
   which is why both blocks stopped turning wrapping off at the same time as
   they started indenting, and why the pair is one change rather than two.

   **Indented by walking the text, never by decoding it.** The obvious
   `JSON.stringify(JSON.parse(text), null, 2)` is the move both of these
   screens had refused in a comment, and the reason is a display bug rather
   than a purity argument. Two of the things that round trip loses are live on
   this path: an id wider than 2^53 comes back a *different* id, and a
   number's spelling (`1.0`, `1e3`) stops being what the model sent. Both are
   there to lose only because the engine went to the trouble — every decoder
   between a model and the record reads through Go's `json.Number` so a
   19-digit Jira id or Slack timestamp stays exact — and a screen whose job is
   to say which object a tool was called on must not show one that is off by
   one. (It also keeps a duplicate key, an unresolved escape and the wire's
   key order, none of which the engine's own encoder can produce: those are
   what it costs nothing to be right about.) So `lib/jsontext.ts` scans the
   text: structural characters get the newlines and the indent, and every
   literal is copied across byte for byte. `JSON.parse` is still called, for
   its verdict and nothing else — text that is not a JSON document at all
   comes back exactly as it arrived, because a provider that answered with
   something else is a reading the transcript has to be able to show.

   With the structure now carried by newlines, nothing is aligned in columns
   any more and both blocks wrap: the only thing left that can overrun one is
   a single long string value, which is the case wrapping exists for.

   Two bounds travel with it, and both are visible rather than magic. The
   indent STOPS GROWING at 32 levels — 64 leading spaces is most of the width
   one of these blocks has in a 420px detail rail, so past it a deeper
   document keeps its newlines and gains no margin. That reading is also what keeps the
   output linear: indentation is quadratic in nesting depth, and the depth
   here belongs to whichever MCP server answered, with Go's decoder accepting
   ten thousand levels. And a LIVE result over four thousand characters is
   sent with a leading ellipsis, which is not a JSON document — so a long
   result renders flat while the phase runs and indented once it completes.
   The text already changes at that moment; the shape changing with it is the
   same fact, not a second one.

   The engine stopped writing the other half of the unreadability at the same
   time. `json.Marshal` escapes `<`, `>` and `&` for embedding in HTML, so
   every URL argument recorded before that read `?a=1\u0026b=2` and every
   markdown body `\u003ch1\u003e` — on a screen that renders into a `pre` and
   needs none of it. `tools.RecordArgs` is the one encoder both the phase
   event and the bridged-run row use now, and it writes the characters the
   model actually sent. Records taken by an older build still carry the
   escapes, and are shown as what that build published.
4. **Grouping is structural; colour is semantic.** A round is BRACKETED by a
   rail running from its numbered node down its own content, and adjacent
   rounds are told apart by a two-step alternating tint — not by a hue
   apiece: a colour per round would read as meaning something and mean
   nothing, which is the same objection as a colour per agent and worse at
   nine rounds. A round's node takes colour for exactly three states —
   normal, contains a failed call, in flight — because "which round went
   wrong" and "where is it now" are the two questions a reader brings to a
   running turn.

   Bounding a round and separating it from the next one are two jobs, and
   only the second was being done. The rail was drawn strictly *between*
   rounds and the tint only on even ones, so a phase with a single round —
   the common case — got neither, and rendered as a bare numeral in the
   gutter beside a flat stack of look-alike disclosure rows: the thinking
   and the call it asked for read as siblings. The round's content also sat
   on three different left edges, because a disclosure head carries its own
   inset and the model's speech carried none. One bracket per round, one
   left edge, and 24px between rounds against 8px inside one.
5. **A running phase tails; a finished one flows.** While live the ledger is
   bounded and follows the newest round, but only while the reader is
   already at the bottom — following regardless yanks them off whatever
   they stopped to read. What does NOT change is the ledger's frame: it is
   the same border and padding either way, and only the height bound and the
   scroll come and go. The frame used to be part of the tailing rule, so the
   transcript grew a box the moment a phase went live and lost it again the
   moment it completed — rule 9's defect, one level down.

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
11. **A prompt is a document, not a wall of text.** Both halves of a phase's
   prompt were one code block each, and a seat's system prompt runs to tens
   of kilobytes: identity, mission, policies, the roster, the turn contract,
   whatever the turn prefetched, the workers, the skills and the tool
   catalogue. An operator asking the question this screen exists for — *what
   was the reviewer actually told about self-iterating?* — scrolled 30 kB
   looking for a heading. Every prompt the engine builds is markdown, so the
   fold now shows one section per `##` the builders wrote, sized and closed:
   the outline is DERIVED from the document, never a list this app keeps, so
   a prompt that grows a section grows a fold and no rename can leave a stale
   name on screen.

   Three properties travel with it. **Each view does its whole job.**
   *Rendered* is for reading — the outline, with every section set as the
   markdown it is; *Source* is the record — the whole document, one block,
   byte for byte, one selection, which is what an operator reproduces a turn
   from and what they diff when a model starts behaving differently. The
   bodies used to be source slices in code blocks on the rule that a prompt is
   a record, but that view was never the record: building the outline consumes
   the heading lines, so a fold showed the bytes with their structure taken
   out, and the one thing the screen had no other route to was the reading. A
   seat's identity arrived as `You are **Engineer** at **Acme**` and every
   `` `submit_work` `` kept its backticks — a reader decoding markdown the
   model was handed already decoded. What is genuinely record-sensitive
   survives rendering anyway: a fence comes out as its own block holding the
   exact text, so a tool schema, a JSON example and a contract template are
   byte-identical either way, and what the reading view spends is emphasis
   markers and list bullets.

   **The switch is offered on every document**, headings or none. It used to be
   gated on having an outline, because without one the two views were the same
   picture under two names; they are not any more, since one decodes the
   markdown and one is the bytes. And **the outline is the document's own
   shape**, which mostly means flat and sometimes does not: an executor's
   thirteen sections are thirteen peers, but a ledger writes one `###` per
   prior turn *inside* its block, and hoisting those would put a turn of
   somebody's conversation between "Earlier in this conversation" and the ask.

   Nesting is only correct because **the levels are**, and two of them were
   not. `internal/agent/prompts` carried a single `#` over a run of `##`,
   which claimed the rest of the executor's prompt was part of the agent's
   identity — nested, that would have put the whole document inside it. And
   the executor's user message carried a bare `Task:` label between two headed
   ledgers, which filed the ask, the newest thing anybody said, inside the
   conversation history above it. Both are structural facts about the
   document rather than about this screen, so both are asserted there: every
   heading a prompt builder emits is a sibling, and every block of the user
   message is a peer.

   The splitter is `lib/markdown.ts`'s, beside the renderer's own heading and
   fence constants rather than next to the screen that wanted sections: what a
   heading is and where a fenced block suspends the grammar are decisions that
   file has already made once, and a `# install deps` inside a catalogue's
   shell sample is exactly the drift a second copy starts with.
12. **A phase card reads in the order the phase happened**: what it was given
   — the Prompt, then the Tool surface — then what it did, then what it
   delegated. The transcript came first and both of its inputs sat underneath
   it, so the question every round raises (*what was this told? what was it
   allowed to call?*) was answered past the end of the answer, and a phase
   with forty rounds put a whole scroll between the two. Both inputs are
   closed folds, so what the order costs a reader who only wants the
   transcript is two header rows; what it buys is a card that can be read top
   to bottom as the story of one phase. Their own order is the request's: the
   prompt is what was sent, and the tool surface is the schema array that went
   *with* it — one payload described in two folds, so they belong beside each
   other rather than either side of the transcript.

   The error callout is the one thing that does not wait its turn. It is a
   banner rather than a section — the reason the reader opened the card at all
   — so it stays above the inputs.

## A monitor is not a reader

A fleet-wide phase monitor showed every running phase as an expanded
transcript. With one agent
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
  Turns tab (then called Model activity) wrapped its turns in the query-state component, which
  renders nothing while a query is in flight and a banner *instead of* its
  children when one fails. So a turn happening right now was invisible until
  the event store answered, and invisible for good whenever that query
  failed. The query's state renders beside the turns now, never in place of
  them.

The seat screen makes the same split, where it answers a second question:
which of these turns is happening right now, readable at a glance from the
accent ring rather than only by finding a badge.

## Live › Now running and the turn list

**Now running** (`#/live`) is five cards in fixed places, each saying so when
it is empty, because a screen whose sections come and go with the data cannot
be read at a glance:

- **Running turns** — one row per seat the engine says is working, the row
  Home's Live now draws (`LiveTurnRow`): what it is doing and on what — the
  item's key, or on a turn charged to no item the trigger's own summary
  ("Executing · Drafting the 2.4 launch brief") rather than a bare verb —
  where the turn is (Context → Execute "round 7 of 25" → Review, and "round 1
  of 25" from the phase's opening frame, before the model has answered once;
  on a phone the round leads the call line instead, so Review keeps its place
  on the stepper's one line),
  how long it has run from the turn's own start —
  so a turn still gathering context is already timed — the call it is making
  or last made, and the whole row a link to its trace. A round with no update
  for two minutes is marked **no update**, for ten **stalled**, in the
  caution and failure tones — and never while the turn is parked, because a
  detached coding run is silent on purpose.
- **Waiting on a person** — every coding run parked on a question: whose, the
  question, who it asks (the chart's names for the seats the engine resolved),
  how long it has waited and how long its box is still held, and **Answer**,
  which sends `answer_run{turn_id, answer}` as the person reading.
- **In a box** — the rest of the detached runs in flight, eight before it
  says how many more. Empty, it says *No coding run is in flight. A finished
  run's record is its turn's trace* — a settled run's row is deleted, so the
  sentence it replaced, promising every finished run under Runs, was false.
- **Activity** — the engine's own count of events over the window
  (`event_series`, minute bars summed exactly into the strip's cells) and the
  latest seven events, each the time and ONE line: the actor, then what
  happened. The source and category are the log's columns, a link away; at
  half a page they were what the sentence gave its room up to, and the same
  node printed as actor and as source is now said once everywhere.
- **Recent phases** — the settled model calls, the only reader of `phases`,
  paged with the engine's cursor (**Load 60 older**). There is no second
  "running now" table: the running half of every phase is the turn rows above.

`seat=` (a handle), `phase=` and `failed=true` narrow every card that can
honour them, and the window (`15m`, `1h`, `6h`) is the activity strip's.
`failed=true` is the spelling Turns uses for its own failure filter, so an
address carried from one Live screen to the other means the same thing on
both. The seat menu offers every agent the chart holds, in every unit, and
shows ten before it scrolls. The page carries no working count of its own:
the shell's header chip says how many seats are working on every screen, and
a second one beside it said the same number twice and pushed the chip off a
phone's page bar. A seat is
asked for by its handle and the phases finishing on the push are matched on
the seat's own id, never on a role name two unit seats can share. The
spend panels and the onboarding cards are gone: the spend is Spend's, and
the cards pointed at screens the sidebar already names.

**Which finished phases are let in at once.** The settled list follows the
rule above — nothing spliced in under a reader — and it states what counts as
already seen: every row of a page the engine answered (the first, which can
land after a restored scroll position, and each older page somebody pressed
for) and the phases the running rows were drawing. Only a phase another turn
finished while the reader was down the list waits behind the button.

**Turns** (`#/live/turns`) opens on the axis, and the axis is the ENGINE's:
`event_series` over `agent_turn_completed` with `suspended=false` — the turns
that ENDED, one completion each, since a turn that parks on a coding run
writes a completion for the segment that parked it — over the whole window,
on every node, with each bar's failed share drawn at its foot. It was folded
from the page of rows the screen held, which drew a week from the newest two
hundred turns. The list under it is paged (a hundred a page, **Load older**),
in the engine's order: `sort=-started` or `sort=-tokens`, the order Spend's
costliest-turns drill-down lands on. Columns: Started, Seat, What it did,
State (re-run, parked, running, failure — a headed column, sized to its
badges), Iterations, Tokens, Took. The chart's key sits under its title, the
count and the failed share first, so on a phone the explanatory tail is what
gives way. Any node
that did not answer either read is named above the list (`CoverageNote`).

**The list is asked for the window itself** — `since` and `until`, the
window's two instants on the turn's START — and the engine's answer is the
list, with no second filter here. It was asked for "the last N days", N being
the window's LENGTH, and filtered in the browser: a one-hour bar picked three
days ago was asked as the last day, every row that came back was newer than
the bar, and the list said "No turns in this window" under an axis counting a
dozen. **Load older** is offered whenever the engine's cursor says there is
more, on a page with no row as well, since the fleet's cursor can stop above
every turn a node held.

**The seat and the failure narrowing are controls, not chips.** The plan drew
`seat=` and `failed=` as chips; they are a `Select` (every agent seat the chart
holds, read from the same org index every screen walks — the top-level roles
alone are the founders, so a menu built from them offered no seat at all) and a three-way `Segmented` (All · Carried a failure · Clean). A chip
per seat is the eighteen-pill row the next section retired, and a chip is a
two-state toggle where `failed=` has THREE values — every turn, only the ones
that carried a failure, only the clean ones — and "absent" is the default an
operator opens the screen for. **The axis is not narrowed by failure**, and
its subtitle says so while `failed=` is set: "carried a failure" is a fact
about a whole turn (any of its records failed), while the axis counts
completion records, each with only its own flag — narrowed that way it would
count a different set of turns from the list under the same heading, so it
stays every turn with the failed share at each bar's foot.

## Controls that mean what they look like

The Model screen collected eighteen controls in one sticky row — a segmented
control, a free-text box, a chip per seat, a chip per phase and a failures
chip — fifteen of them near-identical pills in two different active idioms.
Above them sat four `--font-size-2xl` numerals, a step LARGER than the screen title,
so the loudest thing on a transcript page was a token count. Below the list
sat a copy of Spend's own panel, which every "load older" click pushed
another sixty cards further down a single scroller.

What replaced it:

- **The row of controls is the list screen's own toolbar**, and every axis on
  it is one the screen declares: a search box that is always there, a Filter
  menu offering the rest, a chip per axis a reader added, and a Sort menu over
  every column. A screen that hid its search behind "add a filter" is a screen
  where nobody finds the search.
- **The counts moved into the header badges**, and the failure count became
  the control that filters to failures. It used to be an inert tile reading
  "4 failed" beside an unrelated chip that did the filtering, so a reader who
  saw the number had to go find the pill that acted on it. `Tag` renders as
  a real `<button>` with `aria-pressed` when given an action: a `<span>`
  with a click handler is neither focusable nor announced, and looks
  identical to the inert tags next to it. A tag that is ON keeps the ground
  its label was measured on, because the press is carried by its boundary;
  the badge this replaces repainted the ground with its own ink, which is a
  contrast ratio of 1:1 on the one control whose state has to be readable.
- **The seat filter became a picker.** It was a text box, but the match is
  exact on both sides of the wire — the server query and the in-memory
  filter both compare for equality — so typing a prefix returned nothing
  while looking exactly like a search that found no matches. `Select` is the
  design system's listbox, and it searches once the roster passes eight,
  which no native dropdown can do at all. It also scales past the ten seats
  at which the chip row silently disappeared.

  **EVERY choice on this dashboard is that listbox.** No dropdown here is the
  platform's own, on any screen or in any dialog: the list takes the theme, the
  density, the tokens and the layer stack, rather than the operating system's
  palette in the middle of a dark dialog. What a reader used to get free from
  the platform it earns back for itself, and those rules are shared with every
  other list in the package: type-ahead, Home and End, disabled answers stepped
  over, the highlight announced through `aria-activedescendant`, Tab closing
  the list and carrying on to the next control, Escape stopping at the list,
  and a stored value the options no longer offer kept rather than swapped.
- **The spend panel is a link.** It was Spend's panel on Spend's data at
  Spend's window; the screens are split by question, and duplicating one
  screen's answer at the bottom of another is how the two come to disagree.
- **A toolbar carries the `toolbar` class, and its controls travel in
  groups.** The class is not decoration: `.screen:has(.toolbar)` publishes
  `--sticky-top`, and every other band that sticks to the same scroller — a
  grid's column heads, a grouped list's group heads, an item's side rail —
  starts at that offset. The tracker's filter bar restated every one of
  `.toolbar`'s declarations under a name of its own, so the property stayed at
  its `0px` default and the table's own header parked underneath an opaque
  band. Inside the bar, what narrows the rows is one group and what switches
  between answers is another, because the bar draws different controls on
  different tabs: as one flat wrapping row, the scope control moved from x≈345
  on List to x≈1338 on Board and x≈1155 on Calendar, and the Overdue chip
  wrapped to a line of its own ~1,200px from the count. A wrap breaks between
  groups.
- **A panel that rounds a run of rows clips; it does not hide.** The offset
  above only means anything while the screen is the band's own scroller, and a
  sticky box is held by the nearest one: a panel wrapped around the rows with
  `overflow: hidden` becomes that scroller, and because such a panel grows to
  its rows it can never scroll, so its own offset is permanently zero and the
  band is pushed `--sticky-top` DOWN from the top of the panel instead. The
  grouped work list did exactly that — every band left its slot blank and
  painted over the first row of its own group, one task per group in the DOM,
  drawn and covered. `overflow: clip` rounds the corners identically and makes
  no scroller, which is the same decision the data grid's wrap records.

Three rows of buttons, and a table, that behaved differently from a keyboard
than they looked:

- **A section row activates manually.** `Tabs`, and a `SegmentedControl`
  with `semantics="tabs"` (a lens, a window, a grouping), are one tab stop: the
  arrow keys and Home and End move focus along the row, and Enter or Space
  selects. A section pushes a history entry, and a row that selected as focus
  moved left one entry per keypress for Back to walk through. Each tab names
  the `TabPanel` it controls.
- **A setting is a radio group.** The theme and density controls are
  `SegmentedControl` with `semantics="radio"`: announced as a choice rather than as
  tabs with no panel, and the arrows select as they move, because changing a
  setting costs nothing on every keypress. The same applies to a filter that
  replaces the history entry.
- **The group keeps one tab stop, wherever the reader is standing.** The stop
  travels with the arrows rather than sitting on the selection, because on a
  section row the two stop being the same question: a reader stands on an
  option they have not chosen for as long as they are still deciding, and a
  stop left behind on the selected one drops them somewhere they did not leave
  when they tab out and back. A change from outside the group (a click
  elsewhere, the browser's Back button, a pasted URL) retires whatever the
  arrows were pointing at and takes the stop back, and so does an option
  leaving `options`. It survives a value the options do not carry, too: `value`
  comes off the query string at most of these call sites, so an older build's
  link, a typo or a renamed option arrives as a value no option matches, and a
  stop that fell back only when the *held* option left the set gave every
  option `tabIndex="-1"` on `?lens=bogus` and dropped the whole control out of
  the page's tab order, unreachable by keyboard and strictly worse than the
  plain buttons it replaced. It falls back to the first option; nothing is
  selected, so nothing else has a claim to the stop.
- **A sortable column is a button in its header.** The click used to be on the
  `th` itself, which a keyboard cannot reach and a screen reader does not
  announce as a control. `aria-sort` stays on the header cell, and each column
  says which way its FIRST press sorts: a number and a timestamp read from the
  big end and the newest row, everything else from A, where one blanket
  direction for a whole table ordered every name from Z. An absent value sorts
  last in BOTH directions, because a seat with no meter has not spent nothing,
  it has not been measured.

Four more controls that looked like something they were not:

- **A copy button says whether it copied.** The clipboard is invisible, so a
  control that writes to it and reports nothing is indistinguishable from a
  dead one — and this one *was* sometimes dead: the Clipboard API is gated on
  a secure context, so `navigator.clipboard` is simply undefined at the
  `http://<node-ip>:8000` anyone reads the dashboard of a machine that is not
  their laptop at. `navigator.clipboard?.writeText(x)` swallowed that. The
  design system's `writeClipboard` falls back to the deprecated `execCommand`
  path, which is the only one that works there, and then says `Copied` or
  `Copy failed` — announced as well as drawn. The WRITE is the package's and
  the answer is this dashboard's, which is the split on purpose: the package's
  `useClipboard` settles a refusal back to offering its action after a couple
  of seconds, and a control that has quietly gone back to offering is
  indistinguishable from one nobody ever pressed. A refusal here holds until
  the next press.
- **A download button says whether it downloaded, and refuses rather than
  navigating.** The same invisible outcome with a worse failure available to
  it: an `<a>` whose `download` attribute the browser ignores does not save
  the JSON, it OPENS it, and a tab full of text looks enough like something
  happening that nobody checks. `DownloadButton` tests for both halves —
  `URL.createObjectURL` and `download` — before it builds anything, says
  `Download failed` when either is missing. What it says on success is
  `Downloading`, not `Saved`: there is no completion event on an
  `<a download>`, so the only thing observed is the hand-off, and a browser
  can still stop it afterwards. It names the file
  (`download started — turn-….json`) because a reader who cannot see the
  download shelf has nothing else to tell them what to go and open. It shares
  the confirmation machinery with `CopyButton` rather than reimplementing it:
  the two sit side by side in the Turn header, so a difference in how long
  either holds its answer is a visible inconsistency rather than a private
  detail. A `data:` URL is deliberately NOT the fallback — several engines cap
  one around two megabytes, so it would work on the small turns nobody needs
  it for and fail silently on the large ones.
- **A caption-sized link is still a link.** `.t-caption` on an `<a>` sets the
  muted colour, which wins over the anchor rule, so four real navigations —
  a phase's own event, a turn card's id, a seat's last error, the spend
  screen — rendered as dim static micro-text a reader could only find by
  hovering. `.t-link` is the caption register that keeps `--color-brand-accent-ink`,
  which the palette suite already measures.
- **A link inside a sentence says so, because nothing else can tell it apart.**
  The anchor reset is right for chrome — a breadcrumb, a row that happens to be
  an anchor, a caption-sized navigation — because each of those is a *thing* on
  the page rather than a word in a line. It is wrong the moment an anchor is a
  word in a line: the sentence around it is `--color-text-primary` and the link is
  `--color-brand-accent-ink`, and colour alone is what WCAG 1.4.1 refuses. `.prose-link`
  puts the underline back and keeps it, at rest rather than on hover.
  `.prose.md a` (rendered Markdown) and `.int-form-note a` (the sentence under
  a setup form) are the two containers that get it without asking. The rule was
  first written as prose here and in `base.css` — "the only two registers in
  this tree that are genuinely a phrase" — and was already false: seven anchors
  sat in running sentences in five other files with no cue but colour. No scan
  can decide whether an anchor is inside a sentence, so `proselinks.test.ts`
  refuses the shape where nobody asked, an `<a>` carrying no class at all.

  **And then fifteen more, classified WRONG rather than not at all.** A chrome
  class on an anchor that is a word in a line passes that scan silently, which
  is what `.t-link` on every "…so it has no channel to set. **Open
  Integrations**" was. The baseline's hover underline had been standing in for
  the mark — a cue no keyboard or touch reader ever saw, so it never satisfied
  1.4.1 — and resetting it is what made the gap visible rather than what opened
  it. Eleven of them go through one component, so that is where the fix lives:
  `ScreenLink` carries `.prose-link` BY DEFAULT and takes `standalone` for the
  three call sites that really are a call to action on a line of their own. A
  default that is the common case is what makes the next call site right
  without anybody deciding.

  The scan gained a second question for the rest, and it is decidable rather
  than a guess: JSX collapses whitespace around a newline, so an author who
  wants a space between running text and the tag after it has to write `{" "}`.
  Text, then `{" "}`, then an anchor, means the anchor continues a line of
  prose — sixteen hits over this tree, no false positives. It does NOT catch an
  anchor separated by a literal space on the same line, where the space is
  ordinary text and the shape cannot be told from a caption followed by a chip;
  two of the fifteen were that, and were found by reading. A gate exactly right
  about a subset beats one that guesses about everything. The scan skips a bare
  `<ScreenLink>` on the strength of that default, which is a premise about a
  component and not something a source scan can hold — deleting the default left
  the scan green and all eleven links unmarked — so `dialogParts.test.tsx`
  renders it and checks what comes out.
- **Select-all is a local verb on a record.** ⌘A / Ctrl+A is a *document*
  gesture, so on a screen whose point is one JSON record — a turn's record,
  the active configuration — it took the nav, the stat row and every phase
  card along with it. A `CodeBlock selectable` block is focusable and owns the
  chord while it holds focus; everywhere else the browser keeps it. The
  focus ring is not decoration: a keyboard verb that changes meaning on
  click is a secret without one.

Two more a sighted reader could use and a keyboard or screen-reader one could
not, which is the class of defect that never shows up in a screenshot:

- **A meter needs a name, and a value inside its own range.** `role="meter"`
  with no accessible name announces as a bare number on a screen that renders
  several, and the visible legend is not the name: a reading ("94% of the
  meter used") is not a noun. `Meter` takes its `label` for that reason, as
  every other named control in the package does. The bar's fill is clamped and
  so is `aria-valuenow`, because a budget *lowered* under a counter that has
  already spent past it (the exact state an operator opens the screen in)
  would otherwise publish a value above `aria-valuemax`; `valueText` carries
  the true figures, so the overage is reported rather than hidden. A meter
  with no ceiling is not drawn as one at all, because "0 of 100" is a
  confident claim that nothing has been spent where the truth is that nobody
  has said what the limit is.
- **A tall record block is reachable from a keyboard.** A `CodeBlock` scrolls
  under its height cap, and in Chrome and Safari a scroll container is
  reachable by keyboard only if something makes it focusable. A `selectable`
  block is, because select-all needs it; the rest were not, and they are the
  tall ones: a phase card's verbatim system prompt runs to tens of kilobytes
  and could not be scrolled from the keyboard at all. `focusWhenScrollable` is
  the second door onto the same tab stop, and the two are separate because
  they answer different questions: `selectable` is an INTENT only the screen
  has (this block is the record the page is about), while the stop a scrollbar
  owes is an OBLIGATION the block discharges for itself. A bridged run's tool
  log and a seat's thread take the second — they scroll, so they are named
  regions with a tab stop, and ⌘A goes on meaning what it means everywhere
  else. Measured rather than assumed, and on both axes since `wrap={false}`
  scrolls a block sideways in a box that is nowhere near tall enough to scroll
  down: a stop on every block would put one in front of each of a round's tool
  arguments, most of them a handful of lines. `label` is what such a block takes
  focus under, from either door — taking focus without a name is the other
  half of the same trade.

The header carries the same facts in the same order whether a phase is live or
finished — phase, decision, model, rounds, tokens, age — so the row does not
change shape when it completes. `decision`, `exhausted_rounds`,
`empty_answer_rounds`, `rescue_fired`, `notes`, `tools_available` and
`conversation_key` are all rendered; every one of them was on the wire and shown
nowhere. `empty_answer_rounds` is the newest and the one with the least warning
attached elsewhere: a model that answers with nothing used to fail its provider
call, which walked the fallback chain and could end the turn as a red
`llm_unavailable`; it is corrected in the tool loop now, so this badge is what
keeps a seat whose model never speaks from reading as a seat that merely gets
rescued a lot.

**Nothing animates on a data push.** A list that re-flows every time a
tool-loop round lands is a list nobody can read while it is running, and
`agents` is pushed twice per round and once more before every tool call. Two
kinds of motion are allowed, and `styles/motion.test.ts` holds every animation
in the stylesheets to being declared as one of them, with its reason:

- **an entrance** of a surface the reader opened — the palette rising, its
  veil fading in — which plays once because somebody pressed something;
- **a steady-state pulse** on something live right now — the round in flight,
  a phase waiting on its first answer, the caret on text being written. It runs
  for as long as the state holds rather than playing once when the state
  arrives, so a push that starts or ends one changes WHETHER it runs and never
  triggers it.

An animation on a class a push adds — a row that is new, a value that changed —
is neither, and the suite refuses one by its selector. **Under
`prefers-reduced-motion: reduce` both stop.** `base.css` ends every entrance
and transition on its first frame, document-wide; a pulse is not left to that
rule, which would play one iteration instantly and leave the element wherever
its keyframes rest, so each pulse carries its own `animation: none` and is
static — drawn in its resting look, still saying "live", never moving. No
component animates from a `style` attribute, where no sheet could stop it.

**A control state is the other half of that rule, and it DOES ease.** The
distinction is what the pointer did: a fill that changes because somebody moved
onto a row is a response and should look like one, and a fill that changes
because the engine said something must land on the frame it is told. Almost
every interactive surface in the frame — the sidebar's rows and links,
a grid row, a crumb, the viewer chip, a work row, a turn row, a feed row — used
to snap, and a whole product of instant fills reads as a thing that jerks
rather than a thing that responds. It is ONE declaration in `base.css` naming
those surfaces rather than a `transition:` on each of them: written per rule it
was 24 places to forget, and the five that had one had already drifted to three
different durations. `prefers-reduced-motion` zeroes every duration in the
document, so there is nothing to opt out of.

**A control must not move under the pointer either**, which is the same rule
one level up. A figure that resizes its own cell when it goes from 9 to 10
shifts everything beside it, so every live number sits in a tabular cell with a
floor; a count that lives inside a heading's text resizes the heading, so it is
its own element; a mark drawn only in one state (an unread dot) is a fixed cell
that is filled or not; and a filter chip ordered by a live count reorders itself
on a poll, so chip rows are ordered by name. Each of those was a real defect on
the Inbox before it was the landing screen's own suite.

---

## A turn is a story, not a log of itself

The Turn screen answers "what happened in this turn". It used to answer it by
saying the same things up to five times, and by putting the things nobody else
said into a flat list called *Everything else this turn published*. On a turn
that self-iterated three times, six of that list's twelve rows read
`started execute (iter 2)` — one per phase card 200px above, which already
says EXECUTE, iter 2, its model, its rounds, its tokens and what it decided. A
seventh was the turn's own completion event, which the same screen also
rendered as the stat strip and as a raw JSON dump.

Four rules replace it, and each one names what it fixes.

1. **A duplicate is a row whose fact belongs somewhere else.**
   `agent_phase_started` and `agent_phase_completed` are the same phase — same
   `turn_id|phase|iteration`, which *is* the phase key — and the start says
   nothing the finished card does not. It used to say one thing: paired with
   the completion instant it gave the phase a duration, which no completed
   record carried. That pairing is gone, because it needed **both** events in
   one reader's hands and the reader who needs the number most never has them:
   a turn deep-linked *while it runs* asked its query before the phase started,
   and the only envelopes it buffers afterwards are completed ones. So the
   engine measures the phase where the clock is and publishes `duration_ms` on
   `agent_phase_completed` itself. Every phase reports how long it took —
   which is what answers "why did this turn cost 290k tokens" on exactly the
   self-iterating turns where the question gets asked — and so does every
   **nested** call, a delegate's worker and the round-cap judge included, which
   publish no start and so could never have been given one. Zero means *not
   measured*, never *took no time*: an agent-mode executor's rounds ran inside
   a coding CLI's own loop in another process, and that run's wall clock is on
   `sandbox_run_started` / `sandbox_run_completed` instead.

   **And the page says where each one went.** *Relocated* is only better than
   *dropped* if the page says so: a row that reaches the browser and is drawn
   by no panel is indistinguishable from a row the store never returned, and
   the reader most likely to meet that is the one checking a **cut** turn's
   stated event total against the panels below it. So every absorbed type
   names its destination — "the phase card it opens", "the turn's header and
   record" — and the page prints those under the bands as a folded **Already
   on this page** note: how many of the turn's rows are not listed, what they
   were, and where each kind is drawn instead. Absent when nothing was
   absorbed, like every other section here. Until that note existed the count
   was assembled on every frame and read by no screen, and the destinations
   were prose written for a reader who was never shown them.

2. **Weight is meaning.** `reflection_completed` is a sentinel whose own
   payload doc says it deliberately carries no outcome; a guard breach is a
   turn the engine stopped. As two identical feed rows an operator scanning
   for the second reads past it. So the rows are grouped by the question they
   answer — **What went wrong** (above the phases, absent on a healthy turn),
   **What the turn was given**, **What else it did**, **What it left behind**
   — and anything this build has no opinion about falls through to a residual
   list rather than being dropped. The event registry is additive-only; a type
   a newer node publishes has to still render.

   **A band entry is a query predicate, not a wish.** Every row these panels
   sort came from one read — "this turn's events", which is `WHERE turn_id = ?`
   — and that column is filled from the event's own `turn_id` *field*. So an
   event type whose payload does not carry one can never be in the answer, and
   naming it in a band is a promise the wire cannot keep. It fails **empty**,
   which is the one way a panel cannot say it is broken: a turn that asked
   three colleagues renders exactly like a turn that spoke to nobody. Eight
   types were banded that way, and they split two ways once each was read
   against its publisher.

   **Four were the engine's omission, and were fixed there.** The loudest were
   the three A2A audit records — *What else it did* advertised "colleagues" on
   their behalf, and an ask had never once been drawn under that heading —
   published from inside the asking turn's own tool loop by code already
   holding its id. The fourth was a reflection worker stamping the turn id
   onto every sibling record but not that one. All four carry `turn_id` and
   `work_key` now and are back in their bands, so an ask finally appears on
   the turn that made it.

   **Four are facts about the event and stay out.** The scheduler's cron fire
   is the *trigger* of a turn rather than work a turn did — it precedes every
   turn id there could be, and the screen already renders it as the brief. Two
   more are the records that say **no** turn ran at all. The last is the
   curator promoting a skill off a cluster of many seats' turns, which names
   none because there is no single right one to name.

   The engine holds the line either way:
   `internal/events/types/turnbands_client_test.go` reads the dashboard's own
   declarations and fails the build both ways — on a band entry that can never
   fill, and on a kept-out type that has since *gained* a `turn_id` and is
   therefore ready to come back, which is how the four repairs above announced
   themselves. It covers the **absorbed** map too, on the same terms and for
   the same reason: those rows are drawn, in the inventory below, so a type
   there that the query cannot return is a line that never appears. The one
   exception is declared rather than assumed — the live progress event is
   stream-only and in no table, which is a different fact from a repair
   somebody owes, and is kept on a roster of its own that says so.

   A band is not a rendering, though, and *What the turn was given* was
   rendering half of its own. `prompt.size` — a row of integers per phase,
   which exist so prompt-slimming progress is measurable rather than argued
   about — was banded here and then read by nobody: the panel took
   `prefetch_summary` out of the band and dropped the rest, so the only route
   to a phase's prompt size was the raw payload of a row in the residual list.
   The panel carries both halves now, which is the pair that says whether a
   heavy prompt is heavy *because* of what was prefetched or in spite of it.
   Per phase and per round, never summed: a prompt is re-sent on every round
   of the tool loop, so a total would be neither the turn's input bill — the
   tiles above already report that — nor any single thing that was ever sent.

   Every term of the approximation gets a column — **System**, **User**,
   **Messages**, **Tools**, **Approx. tokens** — so the total can be checked
   against the row rather than taken on trust. *Tools* is the
   tool-definition array as compact JSON, with the definition count on its
   hover, and it is usually the largest: while the engine did not measure it, a
   turn reported ~6,900 tokens on a prompt the provider billed 205,000 input
   tokens for. *Messages* is what a **resumed** phase sends in place of a
   system and user pair — a detached coding run re-enters its saved
   conversation, counted with the reasoning each parked round carries and the
   arguments of its tool calls — and reads 0 on every phase that opens one of
   its own. Five fixed columns overflow a phone, which is what the sideways
   scroll on `.num-block` is for.

   **One row per measurement**, and the phase key is deliberately not the
   row's identity. This half used to collapse a repeated
   `turn_id|phase|iteration` into one row with an `×N` chip, on the reading
   that the turn id *was* the work key so a repeat meant the dispatch had been
   re-delivered and that phase had run again. That has not been true since
   [ADR-0017](https://github.com/crewlet/crewlet/blob/main/adr/0017-a-turn-id-names-one-run.md):
   a run id is minted per dispatch, a redelivered trigger therefore runs under
   a *different* turn id, and the turn query is `WHERE turn_id = ?` — so two
   attempts land on two Turn screens, which is what the attempt banner at the
   top of the page is for. A re-run cannot put two rows in this table at all.

   What can is a **suspend**, which that ADR excludes by name: a detached
   coding run re-enters the run that parked it. The executor publishes its
   opening frame, parks mid-loop, and the resume re-enters the *same* phase at
   the *same* iteration and publishes the parked conversation it sends
   instead. Measured on a real suspend and resume, that pair is
   `system=3,166 user=31 message=0 ~1,753 tokens` and
   `system=0 user=0 message=3,329 ~1,814 tokens`. Collapsed it drew one row
   reading `EXECUTE ×2`, System 0 B, User 0 B, over a tooltip saying the phase
   "ran 2 times" and "ranged ~1,753–1,814 tokens" — and every clause of that
   is false. The phase ran once; its opening carried 3,166 bytes of system
   prompt, not zero; and the two figures are not a range of one quantity but
   two different prompts, both sent and both billed, which is the whole of
   what this panel is for. So both are drawn, in publish order, and the second
   is marked **resumed** — which is also what makes its empty System and User
   columns read as a fact about a re-entered phase rather than as a failed
   render.

   The figures are **bytes**, and they said characters while carrying `len()`
   of a Go string. Nothing rounded it back — a byte formatter printed `24 KB`
   under a tooltip asserting characters — and the two only agree on ASCII, so
   a roster of non-Latin names or a chat thread with emoji in it silently made
   "the count" a different quantity per company. The **labels** were what
   lied, so the labels were fixed; the wire keys stay `system_chars` /
   `user_chars`, frozen by ADR-0006, because a key is an identifier rather
   than an assertion and renaming one reads back as `0 B` on every row already
   in the store. The reader therefore takes one key each and no
   both-spellings chain. See `PromptSize`.

3. **A healthy turn must be able to say so — and only when it can.** A set
   defined by subtraction (`type !== …`) has no meaningful empty state, so
   "nothing went wrong here" was not a state this screen could reach — and a
   section that is always full is a section nobody reads. It is a badge in the
   header now, beside the problem count it replaces. It is withheld on a
   **cut** view for the same reason it is withheld on a running turn: the
   `turn` answer carries a `truncated` flag (the `trace` answer always did;
   this one did not), so a guard breach among the rows the read could not
   reach is one this page cannot see.

   Which rows those are changed too, because the original answer was the worse
   half. A turn is read oldest first, so a head-only read dropped the
   **ending** — including the two records the header reads its outcome, its
   duration and its plan summary off — and the page said so out loud, printing
   "no turn record" directly above the rows it did get and captioning a
   partial event span as the turn's own measurement. Naming that would have
   been honest and still useless: the outcome is the headline of this screen.
   So the answer recovers the turn's last rows beside its first, and what the
   flag names is a gap in the **middle**. The badge reads "middle not shown".

4. **The feed's row is not this screen's row.** `EventRow` has four columns —
   time, actor, summary, source and category. On a page about ONE turn the
   actor is the same seat on every row (it was rendered twelve times) and the
   category is an internal taxonomy. Half the row was noise, so these sections
   use a narrower row that spends the space on the event's own type instead.

### And the header has to read the record that has the field

`duration_ms` and `review_outcome` were read off `agent_turn_completed`, which
has **neither**. They are on `turn_completed` — the learning subsystem's
record of the same turn, published in the same breath, sitting in the same
query answer. So *Took* silently fell back to the span between the turn's
first and last event on every turn that ever ran, and *Review outcome* showed
an em dash under a caption asserting the value came from the turn's own
record. Two events describe one turn for two consumers; the screen reads both
and names each for what it is.

Three more header rules follow from the same audit:

- **A heading is a name, and a turn has none.** The title is the LEAD SENTENCE
  of `plan_summary`, not the whole of it. That field is the reviewer's own
  account of the turn, written by a model against the engine's call ledger, and
  a model asked to account for four tool calls writes four sentences: one real
  turn headed itself with 280 characters at `--font-size-xl` semibold, three lines
  deep, pushing the fact line under it off a laptop's first screen — and the
  turns list, the peek rail and the feed card all head with the same string. A
  boundary is a stop FOLLOWED BY A SPACE, which keeps `1m 52s`, `v1.2` and
  `mattermost_post_message` whole where a bare `.` split every one. The rest is
  not dropped: `TurnBrief` prints the whole summary under "It set out to"
  whenever the lead took less than all of it, and stays away when it did not —
  which is the same no-sentence-twice rule that moved it out of the panel in the
  first place. Prose with no boundary at all is left alone, because a cut
  mid-clause reads as a broken string; `.object-title` wears `.clamp` so that
  case is bounded at two lines rather than by a rule nobody can see. The
  TRIGGER follows the same rule, and it is worth saying because the comparison
  is exact: the panel drops its "Woken by" prose only where the title says ALL
  of it, which now means a one-sentence trigger. A longer one prints whole,
  with the header's own sentence at the front of it — dropping it would lose
  everything the lead did not take, and on a turn with no plan summary that
  prose is the only account of itself the screen has.
- **A turn's outcome is a state, so it takes a tone.** `Stat` grows a `tone`
  for the case where the value IS an outcome — `done` positive, `self_iterate`
  caution, a guard breach critical. Deliberately not on the other tiles: a
  token count and an elapsed time are not in a state, and tinting every tile
  would spend the four status hues on decoration. The tile also names the
  **executor's** own last word (`delivered` / `no_action` / `blocked` /
  `incomplete`) beside the **reviewer's** decision, because a turn that
  delivered nothing and a turn that delivered read identically when only the
  reviewer's word is shown.
- **The failure badge is derived from what failed, not from the phase
  records.** `phases.some(p => p.failed)` misses every turn the engine killed
  *between* phases — a refused charge, an exhausted chain, a guard that fired
  — which are precisely the turns with no failed phase record to find.
- **An unlabelled identifier is not information.** The conversation key sat
  under the seat's name as a raw truncated string
  (`mattermost:9zd7xj4…:cnjza…`) with nothing saying what it was, and it is a
  property of the *turn* rather than of the seat. It is labelled and explained
  beside the turn's record now. In its place the header gained the two facts
  that were missing entirely: **what woke this turn** (the trigger rides on
  every phase event and links to its own event) and **what it set out to do**
  (`plan_summary` — the agent's own account, which nothing read).

### What a mark MEANS, and where a control belongs

Five corrections came out of reading the rebuilt screen, and each is a rule
rather than a tweak:

- **✓/✗ is a pass/fail vocabulary, so it may not describe an absence.** The
  prefetch panel first drew a check or a cross per context block: four crosses
  down the left of a turn where nothing had gone wrong. A seat with no prior
  episodes on this topic, no synthesized skills yet and a turn that is not its
  first has four empty blocks and a perfectly healthy prompt. It leads with
  what the prompt actually GOT, sized, and names the rest in one quiet line.
  The one genuinely diagnostic state — a block that was never *searched*,
  because the trigger was a bare pointer — stays called out on its own.
- **A section's name must not be readable as a failure.** "What it left behind"
  read as work abandoned. It is the reflection pass, and everything in it is
  something the seat now knows: **What the seat learned**.
- **"Nothing went wrong" is a badge, not a banner.** It is a property of the
  turn, so it belongs beside the phase count where the reader already looks
  for the turn's state, and in the slot the problem badge would occupy. A
  full-width banner said the same thing at ten times the weight, after
  everything else, reading as an announcement about nothing. It is claimed
  only on a *finished* turn with a record to claim it from: "nothing went
  wrong" and "nothing was read" must not render alike.
- **A link to the page you are on is a lie.** A phase card carries `event →`
  to its own event — a way out on the turn and on the seat, and on that
  event's own page a loop. `useIsCurrent` answers it generally, so no
  component has to know where it is rendered. Outside a Router it answers
  "no" rather than throwing: a phase card renders fine on its own, and a
  router should not be the price of drawing one.
- **The way into a turn is a control, not a caption.** The turn card's
  `turn 28e93bc3 →` was a mono link in a footer corner with the sentence
  explaining it pushed to the opposite end of the row — the most useful action
  on the card, styled like debug output. It is a button now, with its promise
  beside it.

And **a chip's shape is a claim about what kind of thing it is.** The turn
card put the trigger's integration in the same row, size and shape as the
phase tags, so `EXECUTE  REVIEW  mattermost` read as three phases, one of them
a chat product. The trigger and its source are one fact and belong on one
line; the phases are a different one.

**A page bar holds what you can DO; an object header holds what the object
IS.** The turn page had it the other way round for five of its marks — the
seat, the phase count, the attempt, the problem count and "nothing went wrong"
were all portalled into `PageActions`, beside Copy turn and Download turn. It
cost three things at once. The bar reached ten items and broke onto a second
line on a laptop (see [The page bar wraps by what it
holds](#the-page-bar-wraps-by-what-it-holds)). The reader's eye
had to travel to the far right corner and back for a fact about the object
named forty pixels below. And two of the five were the fact line repeated:
`Agent CEO` and `7 phases` sat directly above `SEAT Agent CEO` and `PHASES 7`
— a chip that repeats the fact under it is not a second reading of the turn,
it is the same reading twice, so those two went rather than moved.

`ObjectHeader` has carried a `status` slot for exactly this all along, and one
of the five was already in it. **State goes there, and the marks are derived
where BOTH frames can reach them** — on the turn's view rather than on the
page, so the peek gets them too. A rail opened over a failed turn used to show
no problem badge at all, which is the one mark a reader opening that rail is
looking for.

**A qualifier follows what it qualifies.** Moving that chip onto the trigger
line put it in FRONT of the text, which is its own mistake: the eye landed on
a label before the sentence it labels, and the one thing worth reading on the
row — what actually woke the turn — was pushed to second place. The subject
comes first and the source follows it, with the flex sizing deciding who gives
way on a narrow card: the message truncates, the source stays whole.

### The document does not scroll

`#screen-scroll`, the shell's own main region, scrolls, and it is the only
thing that may. The sidebar is a fixed column beside a scrolling sheet, so a
page that can *also* scroll as a whole carries the sidebar off the top of the
window and leaves the reader looking at background below the application, with
two scrollbars and neither obviously the one they want. The frame is the design
system's `AppShell`, which is exactly one viewport tall, and `base.css` gives
`body` a fixed `100dvh` with its overflow hidden: the document is not allowed to
grow, so the invariant is unreachable rather than merely unused.

The shell hands the kit that id as its `mainId`, and everything that needs the
scroller finds it through ONE accessor, `screenScroller()` in `lib/scroller.ts`:
the router restoring a position per history entry, the settled list asking
whether the reader is at the top before it splices rows in, and a seat's Memory
tab checking whether a detail it opened is in view. It used to have two
spellings — the router's `getElementById` and the settled list's `.screen`
class, the same node only by coincidence of markup — and a class is a styling
hook, so a layout change could have split them silently. The id identifies the
element; the class only paints it.

The router and the settled list scroll that element directly rather than
calling `scrollIntoView`, which scrolls every scrollable ancestor it can find.
The one `scrollIntoView` (the Memory tab bringing a stacked detail's heading
into view) runs only when the heading is outside the scroller's box, and with a
document that cannot scroll there is no other ancestor for it to move.

### A card has one left edge

A tight body used to reduce the horizontal padding as well as the vertical
one, so its content sat on a different vertical line from its own heading,
visibly closer to the border than the title above it, and a code block inside
one was pushed hard against the card's right edge with nowhere for its
scrollbar. What `tight` is for is a card whose rows carry their own vertical
rhythm (a stack of cards, a footer strip), and that is a claim about height.
It is vertical-only in `Card.Body`, on the inset the header sets, and all four
tight bodies in the product align with their own titles.

### A card's head keeps its title whole

A head is one line: a title, a subtitle beside it and the card's actions at
its end. Beside actions the kit shrinks the title and the subtitle together, so
on a phone a plain title lost a fraction of a pixel and broke over two lines —
*Active* over *revision* — while its subtitle was cut beside the Copy button.
So **on a narrow screen a head holding both a subtitle and actions puts the
subtitle on a line of its own**, under the title and the actions, where it has
the head's whole width. It is one rule for every head of that shape, not a
screen's own.

### A fact that moves is a fact nobody can scan

The turn card's source chip took four positions before landing. Beside the
phase tags it read as a third phase; in front of the trigger text the eye hit
a label before the sentence it labels; after that text it sat wherever the
sentence happened to end, which is a different spot on every card. It belongs
in the header's metadata cluster with the turn's other attributes — how much,
how long, when — because the rule this header already keeps is that the same
facts are in the same places always. That is what lets a reader scan a list
down a column instead of hunting each row, and a source is exactly the kind of
thing somebody scans.

### A budget bar is coloured by the engine, never by its fill

A budget window arrives with the engine's own `state` — `ok`, `near` or
`refusing` — computed once beside the shared counter, and every surface draws
that and nothing else: the attention queue, the live meters on Now, Spend and a
seat's page, the Budgets table and `crewlet budgets show`. There is **one
threshold**, the engine's (`near` at nine tenths of a ceiling, served as the
budgets answer's `near_fraction`), and no screen divides a counter by a ceiling
to decide a colour.

There were three before. The Budgets table restated the 75% the old `Meter`
primitive derived from the fill, the attention queue warned at 90%, and the
kit's `Meter` ramp had its own — so one window read as healthy, nearly spent
and full at once, depending on which screen it was drawn on. And a ratio is the
wrong question at the one moment that matters: a refused charge increments
nothing, so a scope the gate is turning away sits just below its ceiling and a
fill-derived colour draws it as the calmest bar on the screen. `refusing` is the
gate's own word — a refusal stamped in the window, or no room left for a single
token — and it is the condition a seat is parked on, so a parked seat's bar can
never read as merely near.

Each capped window is its own bar: a scope capped by the day and by the month
has two ceilings, and one bar can only be drawn against one of them. `ok` is
drawn in the first data hue — the kit meter's `quantity` tone — rather than in
the accent, because the accent means where the reader is and a window with
room is a number, not a state: a meter is a figure of ONE quantity its label
names, which is all a data hue asks for.

---

### A description list is a metadata list, not a panel layout

Its grid is `minmax(120px, max-content) 1fr`, sized for compact pairs — an id,
a timestamp, a key. Used for a panel's actual content it puts everything in a
narrow left band with the panel empty beside it: one line of prose sat in a
column sized to the longest label, and a byte count sat stranded mid-panel
with the whole right half unused.

Two shapes replace it, both already in the product. **Prose takes a
micro-label above it and the full width below** (the seat's Profile panel has
always done this) — these are sentences, not fields. **A figure goes at the
far end of a full-width row**, behind a `.spacer`, with the heading naming the
unit once rather than every row repeating it — a bare "134 B" beside a label
says nothing about what was measured.

**And the far end is the ledger's, not the card's.** A `.spacer` puts a figure
at the end of whatever box it is in, which is right in a rail and wrong in a
full-bleed panel: the turn's prompt ledger drew a phase tag at x=37 and its
token count at x=645 on a 1570px screen, most of a panel of nothing between
them, and a reader tracking a row across that gap arrives at the wrong one. So
`.num-block` is `width: fit-content` — sized to its own widest row, which
leaves the spacer nothing to push with and puts the figures where the labels
end. Not a chosen cap, which is what this was first written as: a cap is a
number invented to be wider than the content, so it still leaves a gap, and it
has to be re-invented the day a column is added. `max-width` stays as a
*ceiling* only — 100% so a phone scrolls the block rather than the page, and
38rem so labels that grow prose-long cannot quietly restore the gap.

Two things follow, and both were bugs before they were rules:

- **The rows are sized as one box, not one at a time.** A row's own
  `max-content` is its own tag and its own chips, so once the block is clamped
  — a phone — every row falls back to a *different* width and the columns
  splay: 12 KB, 23 KB and 5.3 KB at three x positions under one heading. The
  width belongs to `.num-rows`, the single box around all of them, and the
  rows stretch to it. jsdom computes no layout, so what is asserted is the
  ancestry: every `.num-col` sits inside a `.num-rows` inside a `.num-block`.
- **A fixed column is sized by its heading, not its figure.** `.num-col` is
  `7rem` because the widest label these tables carry — APPROX. TOKENS —
  measures 103px at `--font-size-2xs` with `--font-letter-spacing-wide`, and at the 5.5rem this
  started on it wrapped across two lines above values that each sat on one. A
  column cannot grow for its own heading, so the heading is what sets the
  width.

`PropertiesRail` keeps the one thing this shape is for on this screen: the
conversation key, which really is a term and its detail — labelled, with the
caption that says which external thread it names.

And **a chip must not repeat the sentence beside it.** The trigger's summary
is built by the vendor's own summariser and already opens with who wrote it
("Message from founder: …"), so a `founder` chip next to that line was the
same fact twice — and two chips plus a link after one line of prose is a
hedge, not a header. Only the integration stays: it is the one thing the
sentence does not reliably carry.

### A link is a colour, and its hover is a step within the text rungs

Nothing in this product underlines on hover. `@crewlethq/tokens`' baseline does
(`a:hover { text-decoration: underline }`) and `base.css` used to repeat it,
and both were wrong here rather than wrong in general: almost nothing in this
tree is a link inside a *sentence*. A breadcrumb, a seat in a grid cell, a
caption-sized `turn 28e93bc3 →`, a whole row that happens to be an anchor —
every one is a piece of chrome, and a rule struck under a timestamp, a summary
and a type at once decorates three fragments that are not a phrase. The
suppression had already spread to fifteen rules across four stylesheets, each
undoing the same inherited declaration one class at a time, which is what a
wrong default looks like from the inside: the exceptions outnumber it.

**A reset has to reach the declaration it undoes, and for a while this one did
not.** The paragraph above was true as a decision and false as a description:
dropping the repeat left `a { text-decoration: none }` against the baseline's
`a:hover { text-decoration: underline }` — (0,0,1) against (0,1,1) — so the
package won the only state a reader can see it in, and every link in the
dashboard underlined under the pointer, the sidebar's own rows included.
Nothing failed: an outranked declaration is not an error, not a warning and not
a build failure, and an unhovered screenshot looks exactly right. So the reset
is written at `a:hover` as well, and `styles/baseline.test.ts` holds it against
the *installed* package — every entry point `main.tsx` loads, not just the one
called `base`.

That gate then got the same treatment from the other side. Its first version
held the reset SELECTOR for selector and never looked at the value, so writing
`a:hover { text-decoration: underline }` in `base.css` — the reported bug,
verbatim — left every one of its tests green. A gate that cannot fail for the
regression it is named after is worth less than none, because it also stops
anybody writing the real one. It now holds four things, each of which was a
live hole: that the selector is answered, that what answers it *is* a reset,
that no sheet of ours re-decorates a bare anchor from the other side
(`screens.css` is imported later, so it would win on order with `base.css`
untouched), and that `main.tsx` still imports the baseline *before* our reset —
equal specificity means the later sheet wins, which is the one premise the
rules themselves cannot carry. It is two-sided as well: a reset for a
declaration the package has dropped is a rule with nothing on the other side,
indistinguishable to the next reader from one whose declaration they failed to
find.

So the frame's own hover vocabulary carries it. The sidebar's rows,
`.section-tab` and `.crumb-link` already answer a pointer by moving to
`--color-text-primary`, and a link does the same — in dark the accent ink (#b3a1ff) brightens
toward near-white, in light the violet (#5a33de) darkens toward near-black, so
in BOTH themes hovering makes a link MORE prominent rather than merely
different. Both ends are measured text rungs, which is what rules out the
obvious alternative: the accent family publishes exactly one text step, and
`--color-brand-accent`, `--color-brand-accent-hover` and `--color-brand-accent-active` are fills that
`styles/rungs.test.ts` refuses as a colour.

**And where the colour step cannot be seen, the underline is the hover.** Under
`forced-colors: active` the UA forces `color` to `LinkText` in *both* states, so
the rung step this whole section trades the underline for is unobservable, and
it composites the 4–5% row wash onto its own `Canvas` as nothing. Measured in
Chromium with the mode on: after the reset, `.crumb-link`, `.t-link`,
`.cell-seat`, `.work-col-foot a` and every prose register render pixel-identical
hovered and unhovered, and only a row that paints a wash on hover still
answers at all, because its wash happens to survive. `text-decoration` is the one property the mode leaves to the author — which
is exactly why the baseline's default is right *there* and wrong everywhere
else — so `base.css` puts it back inside an `@media (forced-colors: active)`
block, and the gate requires that block to exist for as long as the reset does.

**Where an underline is the only honest mark it is permanent, not a hover.** A
link inside running prose is distinguished from the text around it by colour
alone otherwise, which is WCAG 1.4.1. `.prose.md a` (rendered Markdown) and
`.int-form-note a` (the sentence under a setup form) carry it at rest, and
`.prose-link` is the third register: an anchor anywhere else that *is* a word
in a line, which is the correction *A link inside a sentence says so* above
records — "the only two registers in this tree" was written here as well, and
seven anchors in five other files were already outside it.

All three state their hover, because the reset now reaches one. `.prose-link`
is (0,1,0) against `a:hover`'s (0,1,1), so a class alone would put the mark
back everywhere except under a pointer — the one place a permanent mark must
not go missing, since it is where a reader is deciding whether the word is a
link. `.prose.md a` outranks the reset on arithmetic and `.int-form-note a`
ties with it and wins on import order; both say it anyway, so neither depends
on a fact stated somewhere else, and `styles/baseline.test.ts` refuses a
register that draws the underline without also drawing it at `:hover` —
deleting one of those selectors reads exactly like removing a duplicate.

Stating the underline is not the same as answering the pointer, and
`.prose.md a` was the register that proved it: pinning its colour at (0,2,1)
kept the `a:hover` step from ever reaching it, so it was the one anchor in the
product where hovering changed nothing whatsoever. It takes the step now, like
everything else — see rule 16a.

### A row is not a row

`EventRow` is the activity feed's row, on a four-track grid: time, actor,
summary, tail. The turn screen borrowed the class and passed three children,
so the summary landed in the 132px *actor* column and truncated at about
twenty characters while the raw event type had the whole tail to itself, and a
full date wrapped to three lines inside the 62px time column. Every row was
three lines tall to show half a sentence.

A page about ONE turn needs a different row, and the differences are all the
same point — the columns that carry information in a feed carry none here. The
actor is the same seat on every row; the category is an internal taxonomy; the
date is a turn the header already dates. The actor has to come off the
**summary** too, not just out of a column: the engine builds these lines as
`lead(actor, …)`, so each one opens with the seat's name, and four rows under
that seat's own heading do not each need to repeat it.

Finally, **a nested call hangs off the phase that made it.** `host_phase` and
`host_iteration` have always been on the wire, `groupTurns` has always done
the split and `PhaseCard` has always had the prop — this screen used none of
it, so a delegate fan-out of eight rendered as eight siblings of the turn's
own two phases, and both the "N phases" badge and the token total disagreed
with the feed's card for the same turn. The token tile now counts the turn's
own phases and reports worker spend beside it, which is what the engine's own
`total_tokens` / `subagent_tokens` split means.

---

### The tracker is a workspace, not a table

Everything above about rows is why the tracker's own screen is not one. It
began as a flat filter row over a plain table beside a "board" that was three
`<div>`s, and it read as a dump of a database rather than as the place a
company does its work. What replaced it is a WORKSPACE, and every part of it
is one of the rules on this page applied to a tracker.

- **A project is a LEAF, not a shape of it.** The sidebar's Projects are the
  company's own projects, each with its key and its maintained open count, and
  a project is where that list stops. Its list, its board and its calendar are
  ways of drawing the project you are already on, so a sidebar row for each
  would make "Engineering ▸ Board" a destination competing with Engineering.
  The counts are the engine's maintained columns rather than an aggregate per
  poll, which is what lets the sidebar carry a number at all: see
  [the sidebar](#the-sidebar).
- **A SHAPE is a drawing, a VIEW is a query, and only the query was saved by
  anybody.** [A filter is a chip](#a-filter-is-a-chip-an-arrangement-says-what-it-is-set-to)
  states the general rule; what is the tracker's own is where the line falls.
  The engine ships SIX builtin views — one per shape, plus the trash — and
  neither the strip nor the sidebar draws them, for two different reasons worth
  keeping apart. The STRIP drops them because a way of drawing is not something
  somebody saved: mixed in beside an arranged query, the two read as the same
  kind of thing, which is why `view=` and `shape=` are two keys. The SIDEBAR
  drops them because a builtin carries no id, so there is no row to address.
  The five shapes are the first row's own buttons; after a divider the strip is
  the container's own tab, the views THIS reader pinned (★) plus whichever one
  is running, **+ View** — the query on screen saved under a name, shared or
  kept to the person who saved it, with the shape as its type and without its
  paging or a calendar's month — and the inventory, as one glyph named **All
  saved views** (a real link) rather than the words, because written out it was
  the item that pushed the search box and the menus onto a second line at
  1280px. That the strip is drawn even when nothing was saved is [the sparse
  state](#the-sparse-state)'s rule, not this one.
- **The second row is how the answer is cut, and it is the last row above the
  work.** The chips open it, ending in **+ Filter** — what narrows the answer
  is what a reader reads first, and the control that adds a narrowing sits
  after the ones it adds to, as the approved Board draws it. At the far end,
  one cluster: **Show** (the scope), Group by, Sort, a board's lanes out of
  view and the count, so a row made narrow by an open peek or a 1280 window
  moves them down together rather than leaving the count alone on a line. The
  scope was four segments opening the row; it is a picker because it is the
  same kind of choice as Group by and Sort, and as segments it took the start
  of the row from the chips. NOTHING ELSE sits between this bar and the work:
  the Tasks screen's introduction and the board's "cards move in the manual
  order" line each cost a row on every visit to say something needed once, so
  the first lives in these docs and the empty state and the second is said
  when somebody tries to drag outside the manual order (and in the Sort
  control's title on a board). On a phone the
  row is the sticky band, so it stays ONE line that scrolls sideways, like the
  shape tabs above it: wrapped, it stood three rows tall and covered the
  board's lane heads as the page scrolled under it. Both fade the edge with
  more past it, as the page bar's controls do — cut at the edge with nothing
  said, "Ta" and "No g" read as broken labels — and what scrolls in the bar is
  the run of controls inside it, never the opaque band, which a fade would let
  the rows beneath show through.
- **The landing shape is the CLIENT's fallback, not a builtin the engine marks
  `default`.** Why it is the list is under [the sparse state](#the-sparse-state);
  why it lives here is that exactly one view row may carry `default` and the
  applier settles that in the same transaction as the write. A builtin claiming
  it would collide with whatever a company saved, so `LANDING_SHAPE` is what
  holds when nothing claims it.
- **The trash is `removed=true`, which is what the engine says it is.** Its own
  `ViewKeyTrash` puts it plainly: a view carrying that parameter *is* a trash
  listing and a client may treat it as one. So it is a filter in the Filter
  menu rather than a tab, and the grid takes the removals rather than a flag
  naming a tab — which is the property a magic view key would not have: a trash
  of one project's bugs is expressible, and every view saved with the parameter
  is read the same way. `show_closed` travels with it, because a removed task
  is very often a finished one and the status predicate is ANDed otherwise: the
  one listing whose whole job is "what did my assistant delete" would hide
  every deletion of anything already done. On the list that is the Show
  segment's DEFAULT under `removed=true` (All rather than Open), so the trash
  reads the same whether it was reached from the Filter menu or from its own
  address, and a Show picked on the bar still overrides it. Its three extra
  columns APPEND to
  whichever set is active, so the trash is readable as a list too, and the
  removal's actor and instant come from the activity feed rather than from the
  row — the row carries no tombstone, and a row the loaded page of the feed
  does not reach says which fact is missing rather than drawing a blank.
  **Purged work has no row at all**, so its history entry is the only evidence
  it existed and it gets a band of its own saying it cannot be restored.
- **Where a WRITE is attributed, the kind of writer travels with the name.**
  The trash's `removed_by`, the log's actor and the directory's Last change all
  answer "who did this", and a person removing one task, an assistant removing
  a subtree and the engine repairing one look identical without it. The name is
  the grid's own rule — see **A person in a grid cell is a resolved name**
  under [Every list is one grid](#every-list-is-one-grid) — and the kind is the
  clause those three cells share, `agent` being the ordinary case and the one
  left unmarked. Spelled per cell, one of them came to check against a word the
  engine does not mint, so its tag was drawn on every row of the trash and
  separated nothing.
- **The list and the table are ONE grid with two column sets**, which is
  [Every list is one grid](#every-list-is-one-grid) applied to the two shapes
  that are rows in columns. What is the tracker's own is the KEYING: `cols.list=`
  and `cols.table=`, because a column set carries an order as well as a
  selection and one key read against both draws the list's columns in the
  table's arrangement — a row nobody asked for that no validation catches,
  since every name in it is legal in both. The `sort=` is deliberately shared,
  because it is a fact about the QUESTION rather than about the drawing.
- **The table sorts at the ENGINE.** A header writes the query's own `sort=`
  key, so the whole set is ordered rather than the hundred rows that happen to
  be loaded — and a column the grammar has no key for is therefore NOT
  sortable, because the query grammar refuses an unknown key rather than
  ignoring it: one wrong header would take the board down with a refusal rather
  than mis-ordering a column. `status` is the one a reader expects and cannot
  have; grouping by status is what that question actually wants.
- **The four shapes ask four different questions of ONE grammar, and each
  difference is its own axis.** A board is `group_by` and no cursor, because
  across a set of columns there is no single order to be after. A list and a
  table are one page with a sort — the same rows, the same grouping, the same
  hundred, and the same cursor followed on Load more — so a second arm for the
  table would be a second copy of one paging rule. A calendar is a DATE RANGE and no grouping, because its axis IS the
  grammar's one `due` key: the month on screen spends it, the Overdue chip is
  not offered there, and the count says what it counted. A timeline is a
  page of five hundred ordered by start, because its window is derived from the
  rows present — a later page may widen it, which is the honest drawing of
  more rows and the only way to the five-hundred-and-first bar. A bar too
  narrow for its title carries it beside it, on a plate of the strip's own
  ground so a week rule stops at its edge rather than striking through a word,
  on the side with room — before the bar near the strip's end — and bounded by
  that room, so it ellipsises rather than running off the strip. What no shape
  does is narrow differently — the FILTERS mean one thing on all four, and a
  shape that reinterpreted them would be a second idea of what the reader asked.
- **A view SETS the scope (the Show picker), and the scope is read once
  rather than per reader.** Open / Recent / Closed / All is the single
  authority on which finished work a read asks for — the status group, or
  Recent's `closed_since`: the screen spreads a view's saved parameters and
  then writes that key from the scope. So a scope defaulting to a constant made a
  view saved over closed work unrunnable — it opened on `Open`, overwrote the
  view's own group, and named a scope its rows did not match. The scope
  therefore takes its DEFAULT from the chosen view — Recent where the view
  carries `closed_since`, and on a board that says nothing about finished work.
  Two absences land on `Open` for different reasons: a view naming no group at all is not a view asking for
  everything, and a view naming a group the scopes cannot express
  (`active` alone) has no reading that answers it as saved — only a choice of
  which wider set to show, and `Open` is the one nearer what its author asked
  for. The exception is a view that WIDENED — `show_closed` with no group —
  which opens on `All`, because seeding `Open` there writes the narrow group
  back over exactly the half the view asked for. And a `scope=` off the address
  that names none of the four reads as `All` in ONE place rather than per
  reader: spelled separately, a hand-edited `?scope=opne` drew an unset switch
  over a query showing every closed task, which is the one combination the
  scope exists to make impossible.
- **A row is a table, and its columns belong to the LIST.** This is the
  EMBEDDED list — a seat's queue, a person's day, an item's subtasks, which
  draw the same compact row inside somebody else's panel; the work screen's
  own list is the grid above, which states the identical rule on its own
  tracks. Every row was its own grid container once, so `auto` tracks sized
  against that row's content alone and a status badge landed at a different x
  on every line. The tracks are declared on the list and each row takes them
  with `subgrid`, so a column is as wide as the widest value in it. The row
  also keeps a CELL for
  a value it does not have: an undated, unassigned, unestimated task lines
  its status up with the task above it rather than pulling every later
  column one place left. **And how wide the list is has never been a fact
  about the window** — a subtask list is 420px in a peek on a 2000px screen —
  so the narrow track set is a container query over the list's own box rather
  than a viewport one, and the task page's rail-beside-column threshold is one
  over the page column, which is the window less the sidebar, less an open
  peek. The narrow set is declared ONCE beside the wide one: a
  track list is positional, so a second copy that missed a reordering hands
  every cell the wrong column and says nothing about it.
- **And a row's own inset comes out of the tracks at its ends.** A subgrid
  item's padding is SUBTRACTED from the first and last track it spans, and the
  row pads itself so a hover tints it edge to edge rather than as a box
  floating inside the panel. A browser hands that back where the track is
  sized from content and cannot where it is fixed — so a fixed opening track
  the same width as the inset resolves to nothing at all. Both of the list's
  track sets had one: the priority mark overflowed a zero-wide cell and came
  to rest on the key, so `↑LEAD-3` read as one identifier with a stray
  character on the front. An end track states the inset as well as the
  column — in both sets, which is a second reason the narrow one is declared
  once rather than twice.
- **A card is four fixed rows, and only the last one comes and goes.** The
  identity row is the type — drawn only where it is not the default `task`, so
  a board of tasks carries no mark and a bug or a milestone is what the eye
  finds across a lane — the key, the size and — at its end, as the approved
  board draws it — the holder, a squircle for an agent and a circle for a
  person with the seat's STATE as the ring round it; the dashed "nobody" square
  sits there when nobody holds the task, so an unclaimed card says so on every
  card rather than floating alone in a foot. Then the title, clamped to two
  lines. Then the facts: priority, blocked, the labels, the due date, "blocks N"
  (the live tasks waiting on this one — the reason to pick it first), the open
  questions and, at the end, what the task has cost in TOKENS. A card on a task
  nobody has touched has no fact row at all rather than an empty band, and the
  predicate that decides restates each mark's own emptiness rule — the price of
  asking BEFORE rendering — so a mark that gains a field adds it there. The
  last row is a STATE and the only part of a card with a hue: blue while a
  seat's turn is on the task ("SWE · executing · round 7 of 20 · 6m"), amber
  while a coding run on it is parked waiting for the READER. The turn is joined
  on the item the engine CHARGES it to (`live_call.work_item`), never on the
  trigger's `work_key`, which names whatever a webhook mentioned; and a run
  waiting on somebody else is a fact on the task page, not a call to this
  person. Labels, "blocks N", the open questions and the tokens are opt-in row
  facts (`fields=`) the board asks for and no agent's listing carries.
- **A status is coloured by its GROUP.** Somebody is on it — `in_progress` and
  `in_review` alike — is blue, finished is green, nothing happening is no hue.
  `in_review` was amber once, the hue reserved across the product for NEEDS
  YOU, so every task in review read as a task waiting on the reader.
- **A drag is a write, made as the reader.** Dropping a card calls
  `place_work_item` as the person the token is bound to, naming the card it went
  above — or the last card of the lane it went to the bottom of — and, across
  lanes, the lane's status, conditional on the card's version. Never an index:
  the engine mints the place between the neighbours as the board stands when
  the move lands, and on the company's board a card of another project is no
  neighbour, since a rank is an order within one project. It is CONFIRMED, NOT
  OPTIMISTIC: the card is drawn where it was dropped with a pending mark — the
  two lanes' counts moving by one with it, since a heading still reading the
  old tally over the card drawn beneath it is two claims that disagree — a
  refusal puts card and counts back and says why in the engine's words, and an
  applied move is re-read. A lane change whose place was refused after the status landed
  (`placed: false`) stays in its new lane — snapping it back would draw a
  status that is no longer true — and the sentence says the place was not
  taken. A drag moves cards only in the MANUAL order (a project's default, or
  Sort → Manual), because in any other a dropped card jumps back to where its
  date puts it, which reads as a refusal that was not one; `Alt` with an arrow
  is the same gesture for a keyboard. A drag tried in any other order is
  caught — a card is a link, and an uncaught drag is the browser dragging its
  address — and answered with the order that moves cards, which the Sort
  control's title on a board also says. A reader the engine will not move
  cards for is told why once, above the lanes, in the sentence every write
  control uses.
- **A card's mark row is one line.** Priority, blocked, due, "blocks N" and
  open asks stay whole and the token count stays at the row's right end; the
  LABELS give way first, to a "+N" that names the rest in its title and to a
  screen reader. The row wrapped once, and the count stood on a line of its
  own under a full row of chips.
- **A lane is headed by its status's own mark** — the ring while the work is
  open, the check once it was delivered, in the status's tone — the mark the
  palette and a decision's task already draw, and the one a list band on the
  status axis carries too. A task TYPE is never drawn with either: `task` wore
  the delivered check once, and a list of work nobody had started read as a
  list of finished work (`lib/work.ts` holds `TYPE_ICON` and `STATUS_MARK`
  apart, and a test says so).
- **A board shows whole lanes and names the rest.** A lane is sized from the
  board's own width: as many whole lanes as fit at a 260px floor, sharing the
  width exactly with no gap after the last, so the scroller's edge falls
  between two lanes and never through a card, and a scroll snaps to a lane's
  start. A fixed 276px lane fitted the approved four at exactly one window
  width — at 1440 the fourth ended 8px past the scroller and every Done card
  lost its edge, and at 1280 Done was cut mid-title. The lanes past the edge
  are COUNTED at the end of the bar in one phrasing at every width ("2 more
  lanes ›", "‹ 1 earlier lane" — the names are the button's accessible name
  and tooltip, because a bare "Cancelled, Closed" read as two status words
  rather than as a control), a button that pages to them, with a shadow on that side of the board: a Recent
  board has six lanes, and its scrollbar sits under its tallest lane, usually
  below the fold, so the last two were on the page with nothing on screen to
  say so. On a phone a lane is the screen less a gutter, and the next lane's
  edge is the cue.
- **A lane has a count, a ⋯ and a way to the rest.** The count is over the
  whole lane; ⋯ opens it as a list or puts it away (`hide=`, which the Display
  menu names and brings back, because a hidden lane on an address somebody else
  sent is a column of work nobody can tell is missing); and past its fifty
  cards the lane ends in "N more", which is the list narrowed to that lane —
  "N more this week" on a finished lane under Recent, since that is all the lane
  holds there. A turn ending on a card the board draws is a card that just
  changed, so the board asks again then rather than a poll later.
- **A row is edited in place, as the reader.** A writer's list and table rows
  draw the status, the priority and the holder as the same badge, mark and face
  a reader sees, each the trigger of a menu of its values — never a select box
  in every row. Each change is `update_work_item` conditional on the version
  the row was drawn at (`if_match`), so a change somebody made a second ago is
  refused rather than overwritten, and the refusal NAMES who made it ("changed
  by Maya since you opened it"), read from the task's newest history entry. A
  reader who cannot change tasks sees the values and one sentence above the
  grid saying why, rather than a hundred pickers that each refuse.
- **Every row is reachable.** A list and a table are a page of a hundred, a
  calendar and a timeline a page of five hundred, and the rest follow the
  answer's cursor on **Load more** beside "N of M loaded"; a busy calendar
  day's "+N more" is the list narrowed to that day, bounded by the READER's own
  midnights because the cell is the reader's day. `[` and `]` walk the rows in
  the order they are DRAWN — lanes left to right, bands top to bottom — and a
  task opened from a list carries that list's question (`list=`), so its own
  page asks the engine where it sits in the WHOLE answer (`around=`) and says
  "3 of 18" with the tasks either side, rather than counting the page that
  happened to be loaded.
- **A filter's operator is the client's, and the wire gets a list.** Priority
  is / is not / ≥ is expanded on the way out — `≥ normal` is
  `priority=normal,high,urgent` — and read back off the list's shape, so a
  saved view stays a plain list any reader can run and a pasted link draws the
  chip that wrote it. Labels take the grammar's own three modes: any of them
  (bare, the engine's default), all of them (`all:`), or none of them
  (`none:`).
- **Every row is filtered on its own** (`subtasks=separate`), on every shape
  and over any saved view. The grammar's default filters ROOTS and lets their
  subtrees ride along unfiltered, which suits a surface that folds a tree under
  its root — and no shape here draws one. Under that default the Open list
  listed the finished subtasks of every open parent, and a person's own day
  drew subtasks somebody else held, with nothing on a flat row to say which
  rows matched and which rode along. A task's own subtasks are its page's
  list.
- **A board draws every lane the scope admits, and a list draws only the
  bands that hold something.** The engine mints the lanes: on a closed axis
  — status, status group, priority and the due bands — `work_items` carries
  every column the query's own predicate could have put a row in, empty ones
  at `count: 0`, so a young company's open work is three lanes with one card
  rather than one lane in an empty page, and a Done lane appears only when
  finished work was asked for. That is the histogram's rule (every bucket is drawn)
  applied to a board, and the two shapes then differ only in the DRAWING,
  as shapes must: a lane with room in it is what a board is, where a band
  over nothing is a rule separating nothing from nothing. The admission is the
  predicate's own, and the fourth axis is the one that is not a stored value at
  all: the due bands are WHEN work is due, cut on the COMPANY's midnight rather
  than the reader's, so the bands, the row's own overdue flag and every `due=`
  filter cannot disagree about a task. An empty scope says which it is — nothing
  open yet, nothing finished yet, nothing filed — and "Nothing matches" is
  reserved for a filter that is actually on; the three sentences and which
  screen owns each are in [the sparse state](#the-sparse-state).
- **A board lane is a range, not a ceiling.** A lane stretches to whatever
  height the tallest lane on the board sets, which answers a short column
  beside a long one and nothing else: a board whose lanes are ALL short — one
  status, one card — has no tall lane to take a height from, and the lane
  closed directly under its single card at 138px, reading as a stray panel
  somebody left on the screen rather than as a place work belongs. So the lane
  carries a floor of two cards' worth of room as well as its scroll ceiling,
  and the floor wins under a viewport short enough for the two to cross — a
  lane that holds one card and scrolls beats a lane too short to hold one.
- **Priority is signal bars, counted.** Three rising bars filled to the step —
  one for `low`, two for `normal`, three for `high` — and `urgent` as the alert
  mark rather than a fourth bar, because urgent is not "more high". The level
  is read by counting, so it takes no hue; only `urgent`'s mark is drawn in the
  critical ink. A chevron pair drew it once, and a column of chevrons in a list
  reads as rows that fold. Where the word is printed beside the mark — the
  task's header and its properties rail — it is set at the size and ink of the
  values around it, because priority is a setting there, not a warning.
- **Every step of the scale is drawn.** `normal` is two bars of three on every
  card and row, as the approved Board draws it; left blank, a list's priority
  column was mostly empty, the two-bar state never appeared, and a blank could
  not be told from a value that never arrived. The urgent rows still stand out,
  as the only alert mark in a column of bars. `none` — the engine's "nobody
  said" — is not a step and draws nothing. A due date drops the year it shares
  with the reader, because one year repeated down forty rows is how the one row
  due next year goes unnoticed.
- **Reading an item does not lose the board.** A plain click opens a PEEK
  beside the rows; ⌘-click and middle-click follow the anchor to the item's
  own page, because a card that cannot be opened in a tab is not a link. The
  peek is the FRAME's — mounted by the shell, addressed by `peek=`, and the
  list only publishes the order `[` and `]` walk, because a list that published
  nothing would otherwise get a rail stepping through somebody else's rows.
  Below the frame's threshold — 1160 px on a screen like this one at the
  normal density, with its arithmetic at `peekColumnMin` in `app/layout.ts` —
  it becomes a DRAWER over the content rather than a column beside it. That
  threshold is the FRAME's and governs every screen's peek, so the quantity it
  turns on is the one common to all of them: a LIST too narrow to read, whose
  floor is 444px, plus everything in front of it at the reader's density. The
  board once had a number of its own — three panes are
  the rail, the sidebar and the peek, a lane is 292px, and at 1280 exactly one
  lane was left beside a detail panel, "which is not a board", so the peek
  should overlay below about 1500. That measured a peek the tracker's own
  screen owned as a column of its own body grid; the frame took the peek over,
  the rules enforcing 1500 matched an attribute nothing ever set, and they are
  gone. A board's answer to a narrow body is fewer whole lanes and a pager to
  the rest — see **A board shows whole lanes** above — which is why the list,
  not the board, is what sets the number.
- **The item's peek is the page's header with NO facts under it.** A header and
  a properties rail stacked in one 420px column are one reading: the header's
  line said Status, Type and Assignee and the rail said all three again a
  hundred pixels below, so a third of the panel above the fold was the same
  answer twice and the description started under it. The page keeps its fact
  line because there the rail is in a column BESIDE the header rather than
  under it. And the peek resolves its own people: the frame mounts it from a
  `peek=` token knowing nothing about an org chart, so a peek taking the list's
  chrome was handed `{}` and drew every person as a raw handle while the page
  for the same task named them.
- **A group of absences is ONE sentence, and the caller decides that.** A
  heading, a hairline and four dashes cost a reader four lines to learn that
  nothing about this task is scheduled, so the properties rail takes a
  `whenAllAbsent` line per group and draws it in place of them. It is
  deliberately not a rule the rail applies by itself: a rail of annotation rows
  would collapse to one sentence the day somebody cleared them, with nobody
  having decided that. The other three absences stay distinct — a row that is
  DROPPED, a row that is a DASH, and a row that says something — which is
  [Honest empty states](#honest-empty-states) inside one panel.
- **Every project the company has is a page, and its segment is the ENGINE's
  question.** `shown=active|archived|all` is a SCREEN segment and `archived=`
  is the engine's three-mode enum; mapping between them is one object, where
  collapsing them would either put `archived=only` in a route people share or
  invent a second name for a mode the engine already has. The segment SELECTS a
  set rather than narrowing a wider one — asked for both and filtered here, the
  page past the engine's own 200 held no archived row at all, so Archived said
  "No project is archived" about a company that had retired dozens. The sort
  travels for the same reason: applied after a capped answer it orders the rows
  that survived the key order, which reads exactly like the answer to the
  question it is not. The corollary is that a column the engine cannot order by
  is not sortable — Lead is resolved against the org chart at read time and the
  tracker holds no chart, so there is no column behind it.
- **A census on the segments is what stops the directory guessing.** Selecting
  one set is what makes the listing honest and it is also what makes an empty
  answer ambiguous — no projects, or every project archived — so the engine
  sends BOTH counts under the same narrowing the rows were read under. Nothing
  derives them from the rows: on Active the archived count has no row on screen
  to be derived from, which is the whole point. With them, the empty state says
  how many are archived and links to them, where it used to hedge by naming
  both ways it happens and sending the reader to look; and a company with
  nothing filed is sayable from whichever segment they landed on, which is
  Active.
- **The log pages BACKWARD, and says when it is holding still.** The history
  screen is the event log's own frame — a window, an axis, then the rows, with
  its three dimensions as facet rails: the KIND of change, who made it, and
  which PROJECT it was in. That third one is the narrowing a project's own
  History lens already is, reachable from the company-wide log rather than only
  by starting from a project. Paging was the one part of the frame this screen
  did not borrow: it asked for one page, printed that older changes existed,
  and told the reader to narrow the window — which moves the window's NEWEST
  edge and reaches fewer, newer rows, the opposite of what they were reaching
  for. **Load older changes** fetches instead, and once a head is held the foot
  says the pages are held still, because until then it is not true: the first
  page is re-asked on every poll.
- **The bars are ours and the rows are the engine's, and the screen says
  which.** The event log asks the engine for its histogram; this question has
  no such read, so the axis is bucketed from the pages the screen is holding —
  and the caption says the bars cover the changes LOADED, the bar says the
  same about its pickers' counts — once, rather than under each of three
  facet rails, which the one compact bar Work narrows from replaced — and the histogram takes that scope as a REQUIRED
  prop, so the sentence a screen reader hears cannot drift from the one on the
  card. A client-side count dressed as the engine's is the one thing this
  product never does.
- **ONLY A SCREEN PUBLISHES TO THE FRAME.** The log is a screen at
  `#/work/history` and a lens inside a project, and the frame holds one
  coverage slot with one setter — so a body publishing from inside another
  screen fought that screen's own answer: on a project's history lens both
  wrote it, whichever polled last won, and switching back to Items cleared the
  slot to nothing over a page still showing a project it had read. The lens
  publishes nothing and states its own rows' coverage inline, which is what the
  Items lens beside it already does. It is a COMPONENT rather than a bare call
  because the rule is conditional and a hook may not be: called with nothing,
  the publish writes null over whatever the enclosing screen published, which
  is the same clobber under a quieter name.
- **A project opens on its work.** The Items lens is the list at the top of
  the page, as the approved Board draws a project: no header about the
  container above the work, and no row of lenses either — the lenses sit on the
  page bar beside the name they are lenses on (`Work › ENG Core · Items | About
  | History`, `PageLenses`), controlling the region below by id. A row of their
  own cost the first screenful a line the Board does not spend. What the container IS — who leads it, its unit,
  its target, its census, its vocabulary and its latest changes — is the
  **About** lens; a finding on the project's own record (a unit the chart lost)
  is drawn over every lens, because it is a fact about the work below. The
  page bar ends with **Edit project** — the lead's target date, a company write
  (`write_project{target_date}`) whose authority the engine decides, titled
  with the project's NAME and confirmed with the day as the Target fact writes
  it ("Set Core's target to 30 Oct 2026", never the wire's ISO form) — and
  **New task**, filed into this project. A project that declares no purpose
  says so in a sentence for a person ("No purpose is set for it — add one to
  Core in the company configuration…"), not a configuration key in backticks.
- **A lens keeps its filters.** The three lenses over a project — Items,
  About, History — are three readings of ONE container, not three screens,
  so switching keeps the narrowing AND the arrangement: `lens=` is written
  through the frame's section move, which copies the whole current query and
  sets one key. A lens that built a fresh query would make History a one-way
  trip, with the way back being every chip re-added by hand — and it would fail
  silently, because each lens renders perfectly on its own. Only Items carries
  a count, from the project's own maintained census: About is a description
  rather than a collection, and History is PAGED, so a count of the page it
  loaded would read as a count of the lens.
- **The charts answer the questions the numbers cannot.** A census bar says
  how far along a project is where four counts say only their sizes; a load bar
  is drawn against the HEAVIEST QUEUE on screen rather than an absolute
  ceiling, because a queue of thirty is heavy in one company and a quiet week
  in another. All of them wear STATUS tones rather than the categorical hues,
  so one fact is never two colours on one screen.
- **A progress meter is the SPLIT of the work, and its whole is stated.** The
  project census is the kit's `SegmentedMeter` over the maintained
  `task_counts`: done (the success tone) and active (the info tone — `in_review`
  is working) as parts, and what is still to do as the quiet remainder, against
  the whole `todo + active + done` — so the parts ARE the census and the bar's
  accessible name reads them ("3 done, 2 active, 6 to do of 11"). Closed work is
  not in the whole: it left the question rather than answering it, and has its
  own column. It was an AMOUNT of done over everything filed before that, which
  drew waiting and started work as one blank — and a SHARE before that, which
  drew a project holding one open item as finished. The legend names the three
  parts in the meter's own tokens. The one meter is drawn by
  `dashboard/src/routes/work/census.tsx`, in the About lens's header, the peek
  and the directory's Progress column (beside the lead's Target date), so they
  cannot disagree about one row.

---

## The turn trace: four tabs, one clock

`#/live/turns/{id}` is where every deep link to a turn lands, and it is read
in two ways: *what did this turn do, in order and how long*, and *what is it
doing now*. So its header says where the turn is and what a reader can do
about it, and its body is four tabs (`tab=timeline|transcript|context|tools`).

- **The trail names the turn the way its task does.** On a work item it reads
  *Live › {agent} › Turn n · KEY* — the ordinal off the task's own turn list
  (`work_item_turns`, walked a page at a time until the turn is found; a turn
  still running is the next one after the newest, and before any of that is
  known it is *Turn · KEY*, never a guessed number). Off a task it is the lead
  of what the turn did. The agent crumb leads to *Live* narrowed to that seat.
- **The status is state, beside the title.** *Running · 6m 12s* off the turn's
  own start, *Parked on a coding run* while a detached run holds it, how it
  ended in the reviewer's or the executor's word, or *not settled* for a turn
  no seat is running that published no closing record (its node stopped
  before it ended — the turns list's *Not settled*) — plus the attempt, the
  problem count and *middle not shown* for a capped read. The facts under it
  are the trigger, where the turn is (*Execute* with *round 3 of 25* as its
  note while it runs — the round its engine last ANNOUNCED, see the Timeline
  below; *Execute → Review* once it is over, with its iterations as the note,
  so the value holds one line in its track), its tokens in
  and out with the workers outside them, the share of its input the prompt
  cache served (**absent, not 0%, when no phase reported one** — a provider
  that reports none and a cache that served nothing are one value on the
  wire), its tool calls, its workers, the node that ran it (the turn answer's
  `nodes`) and its wall clock — the engine's own measurement, or, on a turn
  with no closing record, the waterfall's window, so the header and the Turn
  row under it never print two lengths for one turn. A fact's note sits under
  its own value (`.fact-body`), never on a band a neighbour's two-line value
  can push away from it.
- **The actions are the turn's.** *Steer* sends a note the turn reads at its
  next round (`steer_turn`); it is held, with the reason, on a turn that has
  ended and on one parked inside a coding agent's own loop, which reads no
  notes. What became of a note is the turn's own `agent_turn_steered`, drawn
  under the waterfall and at the round that read it — *delivered*, or
  *expired — the turn finished before it read your note*. *Pause* is the seat's
  (with *Also stop the current turn*), *Open task* its item, and one menu
  (*More on this turn*) holds the other attempts, the traces, and the turn as
  JSON — *Copy turn as JSON* and *Download turn as JSON*, each saying what it
  did in a toast. A bare *Copy* beside the frame's *Copy link* was a second
  copy meaning something else. On a phone Steer stays in view and everything
  else folds into the frame's *More*.
- **Timeline is a waterfall on the engine's own clock** (`lib/waterfall.ts`).
  Every instant is one the engine measured — each round's model call, each
  tool call, a phase, the prefetch, a coding run — and the model only arranges
  them: rounds under their phase, a round's tool calls after its model call (a
  call the engine timed but did not stamp is run serially from the model's
  answer and says it was *placed*), a delegate's workers and the round-cap
  judge under the round that spawned them (`host_round`), each coding run as
  its own span keyed by its job's `launch_id`, the reflection pass after the
  last phase, and *review pending* while an executor has finished and no
  reviewer has started. The ROUND IN FLIGHT is drawn only from a start the
  engine announced for it: every round publishes a frame as its provider call
  is made, and `round_started_at` stays the previous round's while that
  round's tools run — so a start no later than the last recorded round's is
  that round's tools, never "the next round" drawn minutes early. The Turn row
  ends at the turn's own measurement; the reflection pass, which runs after
  it, trails the bar. A turn that parked is one turn: its executor's record
  carries every round from both sides of the park, so the phase spans the run
  it waited on. A record that cannot be placed is listed as **not placed on
  this clock** rather than drawn at an invented place, and whether a call was
  measured at all is one answer the Tools tab shares — a submission stamped
  and done inside a millisecond is `0ms` on both, never "not timed" on one. The bars carry no hue of
  their own — a model call solid, a tool call lighter, a container a thin rule
  — and colour only says state: running (a drifting stripe, still under
  reduced motion), failed, selected. The ruler counts offsets from the turn's
  start on a 1-2-5 step, as many marks as its own measured width has room for
  (64px a label), so a phone's track reads `0 · 50s` rather than six labels
  run together, every label in the duration column's one format (`500ms`,
  `1.5s`, `10s`, `1m 40s`) rather than `10 s` beside `1m 40s`; on a phone the indent halves and the model name beside a round
  steps aside so the span's own name stays readable. `SpanBar`, `TimeAxis` and
  `NowLine` (`components/time/`) are this product's compositions on the kit's
  tokens.
- **A span opens beside the waterfall** (`span=`) from a laptop's page width
  (a 960px page container; the laptop's is about 1024), and under it on a
  phone or with the peek open — where a span a reader opens is scrolled into
  view and focused, since under a long waterfall it lands below the fold. The
  rows are one tab stop: Up, Down, Home and End move between them, Enter
  opens one, and Escape closes the open span and puts focus back on its row.
  A span's kind is its eyebrow only where the title does not already say it.
  It gives the span's offsets on the turn's clock and what only that kind has:
  a model call's words and tokens; a tool call's input and output, with **Why
  {agent} called it** — the narration of the ROUND that asked for it, labelled
  *from round N*, and *this round asked for K calls* where it asked for more
  than one, because a sentence quoted under one of four calls would otherwise
  read as that call's alone; a coding run's box facts, and its transcript once
  collected.
- **A running coding run is watched, not replayed.** While its span is open
  the trace asks the node that owns the run for its live output every three
  seconds (`sandbox_tail`, see [Watching a run
  live](../concepts/code-sandbox.md#watching-a-run-live)), and only then:
  closing the span or the run stopping ends the poll. *Nothing to show yet*
  and *the node that owns this run did not answer* are different sentences,
  and the second names the node. A screen reader is told the tail's STATE —
  one sentence in a status region, which changes only when the state does —
  never the output or the "read 3s ago" clock, which change every poll.
- **Notes sent to a turn** name their sender as a seat's name, the way every
  other attribution does, and say what became of each — read at a round, or
  expired — without assuming the reader sent it.
- **A turn with no closing record** is *running* while a seat is on it and
  *not settled* when none is — the same reading on the trace, the Live turns
  list and a seat's Turns tab — and neither while the live view is
  disconnected, when there are no seats to ask.
- **Transcript** is the phase cards and the story bands above; **Context** is
  what woke the turn and what its prompt was assembled from; **Tools** is every
  execution with where it ran (built in, or the MCP server), each row opening
  its span on the Timeline.
- **The answer is the fleet's.** A node that did not answer the turn read is
  named above the tabs; the answer is asked again whenever one of the turn's
  phases lands on the stream or its seat's stage changes, because what the
  phases do not carry — a run's announcement, a note's outcome, the
  reflection — arrives only in the answer.


## Coding runs, agent-to-agent, traces and the event log

The four Live screens beside Now running and the turns are each a list and a
page, and each page draws ITS OBJECT ALONE: `#/live/runs/{turn_id}` and
`#/live/a2a/{id}` used to draw the whole list with the object tacked on under
it, so a link to one run landed on a board with the run below the fold.

- **Coding runs** reads the durable run record (`sandbox_runs`) merged with
  the live projection, on ONE poll, `RUNS_POLL_MS` (20 s) in `lib/runs.ts` —
  the board, a run's page and rail, and the attention queue Home and the Inbox
  draw all name it, so the board and the Inbox cannot disagree about whether
  anybody is being waited on (a source test holds every reader to it).
  - A parked run's banner says what it asked, **who it is put to** (the
    audience the engine resolved when the run parked, by name, and the lead
    chain when the coding agent's label named nobody), how long its box is
    held, and carries **Answer** — `answer_run{turn_id, answer}`, which reaches
    a run whatever launched it. `pending` is the ordinary outcome: the answer
    is on the seat's inbox and the node holding the seat resumes the run.
  - A running run's page polls its live output from the node that owns it
    (`sandbox_tail`, by the `launch_id` the row names, every 3 s while the page
    is open — the same `LiveOutput` the turn trace draws).
  - A run is collected when its record is deleted, and its page still
    answers: it reads the turn and draws every `sandbox` phase the turn
    published — what the job reported, what it delivered, its tokens and its
    transcript — named by the task its `sandbox_run_started` carried.
  - Beside a peek the board keeps the seat and the task and drops the coding
    agent, the placement and the box, in that order, saying so in its footer.
  - The page reads two sources, and "no run" needs BOTH: only when the run
    record and the turn both answered and neither holds a run does it say
    there is none, headed **No coding run**. A source that could not be read is
    named in a warning over whatever the other one holds, never folded into
    "none"; "No task was recorded" heads only a real run whose launch carried
    no task.
- **Agent-to-agent**: a channel is an authorization record — the pair, the
  count and the window — so its page reads the words from the log: the
  channel's own events (`events{channel_id}`), oldest first, a conversation
  read top down. "Read this channel's events" and "Open in the event log" are
  the log narrowed to the channel, `#/live/events?channel={id}`. An id the
  record does not hold is ONE not-found state carrying that link, not a
  second empty events card under it; the events card still appears when the
  log holds what crossed a channel whose record is gone.
- **A trace** has no list. Its page states each number once in its header —
  events, spans, elapsed, when it began, failures — and draws ONE ROW PER
  EVENT, nested by the span that carried it. A span is not an event: every
  event of one phase carries that phase's span id, so a tree keyed on the span
  id drew one phase's last event over and over and lost the rest (a 25-event
  trace drew 344 rows and neither of its two failures). A span holds its
  events, appears once under its parent, and its events and child spans are
  interleaved by time. A failed event is marked as the event log marks it —
  the danger edge and the warning glyph. Each row places its event on a
  **lane**, a rail spanning the trace's first event to its last with a mark at
  the event's instant, on a fixed-width track so every row's lane is the same
  scale; a trace whose events share one instant draws no lane. On a phone the
  sentence wraps. It names a node that did not answer. "In the log" is the log
  narrowed to the trace, `#/live/events?trace={id}`.
- **The event log** asks the ENGINE for every filter but the text search:
  category, actor, seat, one trace, one channel and "Failures only"
  (`failed=true`) narrow the axis and every page alike, and the live rows are
  narrowed by the same values each row carries (a live row carries its seat and
  its channel exactly as the store's columns hold them). "Failures only" used
  to mark the rows a tab held, so the axis counted the whole window while the
  list held the failures among the newest hundred. The search box is the one
  filter over what is loaded, and an empty search says older pages may still
  match. The end of the pages is the end of what was ASKED — "No older event
  in this window matches these filters" under a filter, "That is the oldest
  event in this window" without — never the store's retention, which a
  24-hour window over a thirty-day store does not reach. A node that did not
  answer is named above the list.
- **An identifier in a header** — an event's wire type, its trace id — is a
  TOKEN fact (`Fact.token`): it takes two of the fact line's tracks on one line
  and is cut with an ellipsis, its whole value in the title, only where two
  are still too narrow. The clamp every other fact takes split
  `turn.guard_breach` as "turn.guard_breac" over "h".

## Knowledge: a tree, and search with real modes

Every Knowledge screen stands beside ONE COLUMN, the approved artboard's tree:
the search box, the search's mode, and every space's pages. It is the
workspace's `tree` renderer (`app/nav.ts`) — drawn by the shell and mounted
once for the workspace, 256 px wide (`TREE_COLUMN`, counted by the peek
arithmetic above) — so a folder a reader opened stays open while they read the
pages in it.

- **The mode is Hybrid, Keyword or Meaning**, and the wire is
  `mode=hybrid|keyword|semantic` — the word for `semantic` is the reader's and
  is never sent. The column learns what the backend serves from the
  `knowledge` PROBE: the question asked with no phrase, which runs nothing and
  answers `modes` and, asked for `semantic`, the configuration reason the
  others would degrade (`no_embeddings`, `unsupported`). A mode the answer does
  not list is **disabled with that reason written under the control** —
  `aria-disabled`, so it stays in the arrows' path and a keyboard reader
  lands on it to hear why — and before the probe answers nothing is disabled,
  because "not known yet" is never a reason to take a mode away. **The default
  is a mode the engine serves**: Hybrid where it is served as asked, else the
  first mode the probe lists — Keyword on a company with no embeddings
  provider — so the checked segment is never one the control also draws as
  unavailable, and an address that names no `mode=` runs in that same default
  (the search screen waits for the probe rather than running Hybrid first and
  reporting "asked for Hybrid, served Keyword" about a choice nobody made). A
  mode the reader named in the address is theirs and is kept, and the answer
  says what served it. On the search screen a mode re-runs the search;
  anywhere else it is the mode the next search runs.
- **Results say what ranked them**: "ranked by Keyword" on every result card,
  a sentence above the hits when the engine served something other than what
  was asked (`served_mode`, `degraded`), and a warning when the fan-out was
  PARTIAL — the buckets nobody scanned and the node that did not answer, from
  the answer's `coverage` — because a short list from two thirds of the corpus
  is otherwise indistinguishable from a small corpus. The search reads this
  node's own copy of the pages on the native backend, and the screen says so;
  it no longer claims "no local copy", which was true only of Confluence.
- **A search that did not run** says which of `no_company`, `no_backend`,
  `no_scope` or `building` it was, with the remedy that reason names.
- **The spaces** are listed with each space's key; the unit that files into
  one and the tracker project it works in are named for a reader who may read
  the company document, in four states — read (which may be "no unit names
  it"), no token held, read in flight, refused — and "needs an operator token"
  is said only to a browser that holds none. A project the space's key does not
  already name is drawn beside it as a dashed key, not only in a tooltip.
  A space opens onto its top level (`pages{container, roots}`) and a page that
  holds pages onto its children (`pages{parent}`), each read in windows of 500
  with **Load more** from the answer's `after` and "n of total" beside it. A
  row opens only where the engine counted children the same listing would show.
  The page being read is opened to — its space and every ancestor.
- **On a phone the spaces fold.** Below 640 px the frame is one pane and the
  column stacks above the screen, so a tree as long as the company's pages
  would stand between a reader and every page they open — opened to a page,
  a space's top level alone is up to 500 rows. The search and its mode stay
  drawn; the spaces are one disclosure (**Spaces**, with how many), closed on
  every arrival, and nothing under it is read until it is opened.
- **The home** (no phrase) is the spaces as cards: name, key, purpose and page
  count. On a company whose knowledge lives in a vendor's wiki there is nothing
  on the engine to browse, and both the tree and the home say so rather than
  drawing an error.
- **A space's own page** reads every page, not the first fifty, and says
  "n of total pages loaded" with Load more when a space outgrows a window.
- **A container's peek** reads one window of 500 and COUNTS off the listing's
  `total`: the Recent pages count and "n more pages in this container" are the
  container's, never the window's. When the container outgrows the window,
  Recent pages and Who writes here each say they cover the first pages by
  title, since the listing is ordered by title rather than by time.

Below the spaces, past a hairline, the column draws the workspace's own
SECTIONS: **Agent skills**, with the count of published tool-skill pages
(`pages{skills: true, status: published, limit: 1}`'s `total`, so the number
is the engine's rather than a window's), and **Agent diaries**, with how many
agents keep one (`memory_overview`'s seats). A section the engine cannot answer
is not drawn — on a company whose knowledge lives in a vendor's wiki the
skills listing is `unknown_query` and the row would open onto an error; a
diary is the engine's own whatever the wiki.

### A page: the document, who read it, and who links to it

`#/knowledge/pages/{id}` is the approved Knowledge artboard: the document in
the middle and a rail beside it.

- **The head** is the space and ancestors as crumbs, the title, and one meta
  row — who wrote it, when it was last saved and by whom, its revision count
  and, on a tool skill, a pill saying who it reached: "Loaded as a skill by
  SWE, CTO +2" from the `page` answer's `skill_loaded_by`, "offered to" when
  no seat asked for the body but phases offered it, and "not loaded in 30
  days" when neither happened. The pill is neutral: the accent means where
  the reader is.
- **The body is rendered with anchors on its headings**, and a first heading
  that repeats the title is not drawn twice.
- **The rail**, top to bottom: *On this page* (the headings, the current one
  marked as the document scrolls, a press moving focus to the heading so a
  keyboard continues from it), *Read by* (`page_reads`: each seat and how it
  reached the page — "turn 2 on ENG-412", `search: “DHCP lease”` — linking
  the turn it happened in), *Read today* (the faces, and the engine's
  `distinct_seats_today` rather than the length of the faces drawn),
  *Linked from* (`linked_from`: tasks by key with their status, then pages by
  title), *Revisions*, *Children* and *Watchers*. Readers, backlinks and
  revisions each draw five rows and "Show n more". A signal this node cannot answer is not drawn rather than
  drawn empty: no `page_reads` on a node without the usage domain, no
  *Linked from* on one without an index. A node that holds an index and
  cannot answer says why instead of "No page or task links here" —
  `linked_from_status` `building` ("still building its search index … look
  again in a few minutes") or `unavailable` — and the capped-reads note is
  worded company-wide ("this list may be missing some"), because `elided`
  counts what the cap dropped on every page rather than this one. The
  readers section names its window ("Read by agents · last 30 days") beside
  the page bar's company-day count, and each row is two lines as the artboard
  draws it: the seat with its read count, then how it read the page and when —
  the way-read cut with an ellipsis (whole on hover) so the age is never pushed
  onto a third line.
- **Comments and the change log sit under the document**, not in the rail —
  a reply is prose and a rail is 280 px wide.
- **Edit writes as you.** The editor saves through `save_page` on
  [`/operator/act`](api-endpoints.md#operatoract--the-dashboards-write-surface)
  with the revision it was opened on as `base_version`. The page keeps being
  read while it is open (every 20 s), so a save by somebody else is SEEN
  before the person presses Save: the editor says "Somebody saved revision n
  while you edited", offers their change as a diff, and "Keep editing on top
  of it" MERGES their change into the draft by line before the base moves
  (`lib/merge.ts`, a three-way merge over the revision the edit started
  from). A change only one side made is taken as it is; where both changed
  the same lines the base stays behind and each clash is shown — their lines
  beside the draft's — for the person to keep theirs, yours or both, with
  nothing pre-chosen and the draft read-only until they apply. Moving the
  base without the merge is precisely what the engine's refusal of a stale
  base exists to prevent: the next save would be accepted against the new
  revision and delete what they wrote. Save and ⌘/Ctrl-Enter read one
  answer for whether a save can be sent. Write and Preview are two tabs; a page link
  is inserted by picking a page, and is written in the address the backlinks
  read (`PAGE_ADDRESS_PREFIX`, held against `pages.AddressPrefix` by a test).
- **New page** on a space files the page at its top, and **New sub-page** in
  a page's menu files it under that page (`write_page` with `parent`); the
  reader lands on the page by the id the engine answered with.
- **History** reads any revision back and shows what a save changed against
  the one before it, by line.

A reader without a write credential sees every button with the reason it is
unavailable, never a button that fails.

### Agent skills

`#/knowledge/skills` has two kinds behind one address. **Tool skills**
(`kind=pages`, the default) lists every published tool-skill page with who it
reached over thirty company days — the `pages` listing's `skill_loaded_by`,
read with the listing rather than per row — and the total the listing counts,
with Load more past a window. **Learned** (`kind=learned&seat=`) is one agent
seat's synthesized skills from `agent_memory`, answered by the node holding
the seat, with "n of total" when the page is cut.

### Agent diaries

`#/knowledge/diaries` lists EVERY agent seat in the chart — the old Knowledge
home stopped at a dozen — from `memory_overview`: each agent's newest note,
when it was written, and its entry, episode and skill TOTALS, counted by the
node holding the agent, which the row names. An agent no node holds says so
and shows no counts, since no copy of its memory is current; an agent whose
holder did not answer says that instead of drawing zeros, and the partial
callout every fleet answer draws names the node and why. The tree's **Agent
diaries** row counts the agents listed. `#/knowledge/diaries/{handle}` is one
agent's diary and episodes — the same two cards its profile's Memory tab
draws, from the same holder-answered `agent_memory` — with the node that
answered, or the reason nothing is shown, and the way to its whole memory.

## Spend: tokens by phase, agent and model

`#/spend` is one window of **whole company days** — 7, 30 or 90 (it opens on
30, a daily chart's shortest useful run and the monthly budget's scale), or two
dates a reader names (the custom picker takes two dates on the company's clock, both
counted, because the answer is company days and a minute picker would offer a
precision it cannot have). Everything on the screen is that window, read from
the replicated usage domain, and every card says which window in words built
from the ANSWER (`last 30 days`, or the dates) rather than from the control.
There is no live rolling window here any more: a "24 hours" beside a chart of
company days put two windows that cannot be compared on one screen.

**A window the engine refuses draws no figures.** More than 90 days, or a start
before the history's floor, is refused in the engine's own sentence — which
says which of the two it is and what to change — shown once at the top of the
page and nothing under it: the previous window's answer is not kept on screen
beneath a control that now names a different window, and Export has nothing to
export.

- **The hero** is the window's tokens, the change against the window before it
  (the engine's `previous=true`, cut on the company's calendar, and "nothing
  was spent" rather than a percentage when the window before spent nothing),
  and where the history's floor is (`horizon.floor`).
- **This month** is the budget gate's monthly window from the `budget` push —
  "used of limit · resets Oct 1" on a kit `Meter` handed the engine's `state`,
  never a fraction judged here, with the reset on the company's calendar. With
  no monthly ceiling it says so, and offers **Set one** (to `#/spend/budgets`)
  only to an operator — and only once a report has SAID so: before the engine's
  first report the push is `null` and the tile waits, because "nobody has read
  the counter" is not "there is no ceiling" (Home's weekly line reads the same
  slice the same way).
- **The prompt cache's share** is `cache_read_tokens / input_tokens` (the input
  already includes the cached prefix). A window in which nothing reported a
  cache has **no cache tile** — not 0%, which would be a claim about caching
  nobody made.
- **The median task** is the tracker's own `totals=spend_tokens:median` over
  the tasks FINISHED in the window that spent anything, with how many. On a
  company whose tracker is not the engine's, `work_items` is not served and the
  tile is not drawn.
- **Daily tokens by phase** is the kit's `StackedColumns` over the engine's four
  bands, each in its `BANDS` hue, and the legend is always those four — the
  engine answers a band nothing spent in at zero; the split control offers the
  contract's six `GROUPS`, and any split but phase draws the engine's top four
  and a residual. A seat band is labelled with the seat's name, as the table
  under it names it, and a team band with the unit's. The comparison is the
  hero's; the chart draws no ghost of the window before.
- **By agent** — turns, tokens, share, **budget today** (the seat's daily window
  from its overlay, `exhausted` where the engine says `refusing`) and tokens per
  ended turn. A row opens the seat in the peek; on a phone a seat is one
  compact row — name, tokens and share, then turns · today · per turn, each
  with its unit. **Recent turns by tokens** goes to `#/live/turns?sort=-tokens`
  over the same window where the turn list has it (a quarter goes to its thirty
  days, and a caption beside the link says so): a company day holds no turn, so
  per-turn spend is the turn list's.
- **By model** is the engine's `by_provider`: each configured provider entry,
  the models it answered with, who used it (the top three and how many more)
  and its tokens. **By team** is `token_series{group: unit}` asked for every
  team; **Background workers** is `by_worker`.
- **Export** is a client CSV of the rollup and the by-agent table, every row
  labelled with its window and section, tokens only, and every text cell
  guarded so a spreadsheet opens a name as text rather than a formula.

`#/spend/tasks` — **Expensive tasks** — is the tasks last changed inside the
window, bounded at both edges (`closed_since` plus `updated=range:<since>..<until>`
from the spend answer's own instants, so a window that ended last month lists
nothing that moved only this week), most tokens first
(`sort=-spend_tokens`, the engine's order), each with what drove it from
`fields=spend`: its turns, and its reopens, workers and send-backs where they
are not zero, and a bar beside the figure scaled to the top task (not a
percentage: a lifetime's tokens are a share of no window's total). A task's
tokens are its whole life's — the tracker charges a task, not a day — and the
list says so. The overview shows the top three. A
window the engine refused has no list: the page says the refusal and nothing
under it waits for an answer that will never be asked. On a phone each task is
the overview card's compact row — key and title with what drove it under, the
tokens at the end — rather than a labelled card per task.

## Spend › Budgets, raised in place

`#/spend/budgets` is every scope's three calendar windows on the company clock
— the day, the ISO week and the month — from the `budgets` answer: what each
has spent, its ceiling or **No ceiling**, when it resets and, while the gate is
refusing it, since when. **The company** comes first, as three tiles, because
everything a seat spends is the company's spend too and its ceiling binds every
seat at once; **Agent seats** follow as one table with a column per window, so
the one seat near its ceiling is found down a column. A window's bar is a kit
`Meter` handed the engine's `state` — the page states the engine's
`near_fraction` in its lede and divides nothing itself — and a window with no
ceiling draws no bar, because a bar needs something to be a fraction of. A
counter nobody could read (`durable: false`) says so and draws no figures.

**Every ceiling is editable in place by an operator.** The pencil beside it
turns the figure into a field that takes the spelling the screen draws — `40M`,
`2.5M`, `750k`, or the digits — and **empty for no ceiling**; a 0 is refused
before anything is sent, in the engine's terms (empty is the only "none"). A
save is **checked first**, against the revision it was read from, and only the
warnings the change *introduces* are shown — a seat ceiling at or above the
company's, which can never refuse a turn, is the one this screen exists to
catch — with **Save anyway** to store it regardless. The company's ceilings are
written as a merge patch of the windows that changed (`null` removes one); a
seat's by replacing that seat (`PUT /config/roles/{handle}`, its check the same
request with `dry_run=true`), because a patch cannot address one seat in a
roster. The revision's summary says whose ceiling, which window, and from what
to what. After a save the figure shows the new ceiling marked **applying…**
until this node reports the saved epoch, and then re-reads. A conflict saves
nothing and offers **Reload**; so does a request that may have landed. The
pencils are drawn for every reader and disabled, with the reason said once
above the page, for a reader without an operator credential.

**There is no reset.** A window's `used` is what it spent; the room comes back
when it turns over, or now by raising its ceiling. Home's and the Inbox's
**Raise budget** open the same write as a dialog of one scope's three ceilings
(`RaiseBudgetDialog`), each field captioned with what that window has spent and
when it resets.

## Settings: the frame, people, secrets, nodes and configuration

Settings is the one workspace that draws its sections as a **column** beside
the screen rather than as tabs in the page bar, in three groups: **Company**
(General, People & access, and Budgets as a cross-link — it lives once, under Spend, and its
arrow says pressing it leaves Settings), **Connect** (Integrations, Tools &
MCP, Models & keys, Secrets) and **Engine** (Nodes, Configuration, Backups &
retention, Audit log). A guarded section draws a key and **is never hidden**: a section that
vanished for a reader without an operator credential is one they cannot know
exists. The landing page is one of the column's sections, so its trail reads
**Settings › General** rather than "Settings" alone. **On a phone the column
folds to one row naming the section** the reader is on, which opens the list
and closes again once a section is picked: stacked whole above the section it
took about 520px, and a section's own content began below the fold. **Beside a
section the column stays put**: it is the scroller's height rather than the
section's, so it sticks while the section scrolls, its hairline runs the height
of the window, and a list longer than a short window scrolls inside itself. On a
phone the folded picker is its own height and the section takes the rest, so a
short section starts right under it. An address that names no screen is in no
section, so a mistyped one is never drawn beside a column claiming General.

**A section's name is its trail, not a heading over the section — the one
place the approved Settings artboard is deliberately not followed.** The
artboard sets the section's name again as a 24px heading above its lede, 36px
in from the column. On every screen of this product the page bar's last crumb
IS the page's `h1` (see `header/PageHeader` under [The frame](#the-frame)),
so a second heading a line under it would give the page two titles — two
level-one headings to a screen reader, one of them saying the other again —
and a screen's lede (`PageNote`) opens its body at the content padding every
screen shares rather than at an inset of its own.

**The figures beside a section are the engine's, and absent is never zero.**

| Row | Figure | From |
|---|---|---|
| Integrations | a **pill in the warning tone** — the tools the engine's roll-up says need a person (`tools[].state = attention`), the one figure here that is waiting on the reader. It is a *state*, so it wears the warning pair the artboard draws it in, never the accent the inbox's unread badge fills with | `integrations` |
| Nodes | the nodes holding a presence lease ("1 node live"); a warning dot and "*n* behind on config" in the row's name when a node has applied an older epoch than the fleet activated (the Nodes screen's own "Behind on config" count) | the health push, and `fleet` |
| Configuration | "epoch *n*" — the epoch this node applied, in words rather than a code | the health push |
| Backups & retention | how many state-log domains have a trim something is holding, drawn only when one is | `retention` |

`fleet`, `retention` and `integrations` are operator answers with no push
behind them, so those figures are a poll — and **they are asked only while the
Settings column is on screen, and only for an operator** (`useSettingsSidebar`,
at 30 s, 60 s and 120 s). The frame outlives every screen, so a figure hook
that asked unconditionally would put three operator questions on a timer
behind every screen of every tab, and three refusals behind a reader who holds
no credential. On Nodes the column asks no `fleet` of its own: the screen
hands the frame the reading it already polls every 15 s (`usePublishFleet`),
so "*n* behind on config" and the screen's "Behind on config" tile are one
reading rather than two polls on two clocks that disagreed for up to 30 s.

**General** is the company's charter — mission, vision and the standing
policies every executor is given verbatim — read from the org projection, so a
reader without a credential can open it, as they can Tools & MCP. It is
edited in the org builder (**Edit in org**).

**People & access** draws the engine's `access` answer: one join, walked from
both ends. **People** is every human seat — the person, their team, where
agents reach them (one chip per contact field, as written: a `${VAR}` is its
name, and one this engine's environment does not set is a warning chip saying
so) and whether they **act here** as themselves: *Acts as themself*, *Not
bound*, *Variable unset* or *No such token*, each with the binding as written
beside it, because each failure has its own remedy. **API tokens** is every
label the guard accepts, the person it acts as (or *Nobody — writes under its
own label*), its scope (*Person* or *Operator*) and **yours** on the one this
browser presents. A tile counts the **broken bindings** — a seat naming a token
that binds nobody, which otherwise looks bound until that person presses a
button and is refused. Tokens are declared in Tier A (`api.auth.tokens`) and
change at a restart, so the screen edits nothing; **Edit people in org** leaves
for the builder, where a contact and its binding are written. **It never holds
a value** — not an answer's, and not the token this browser presents. A
disabled guard is a red banner: every caller is `anonymous` and nobody acts as
a person.

**Integrations** is a grid of **tiles, one per tool** — Slack, Mattermost,
Atlassian, GitHub, GitLab, Datadog — each with the vendor's mark, the
engine's word for its state (`tools[].label` from `integration.Rollup`, the
same word the Settings column counts), one sentence and **one action**. The
sentence is the roll-up's `reason` while something is owed, and what the tool
does for the company otherwise. The foot counts what the engine sent: the
agents on its roster, the credentials it holds a finding about, and the
deliveries this node counted (only when `traffic_known`). **All · Connected ·
Available** narrows the grid, with the rows' own counts. The action is exactly
one of five, decided by `actionFor` from the same inputs as the tag:

| Action | When | What it does |
|---|---|---|
| **Connect** | nothing configured | opens the tool's setup form |
| **Continue** | a requirement unanswered, or a surface of a partly-connected tool not yet configured | opens the same form |
| **Rotate token** | a `credential_expiring` or `credential_rejected` finding on a configured surface | opens the same form, titled *Rotate the {tool} token*, with the engine's reason first — type the new token over the held one and save; the engine seals it and re-activates the configuration |
| **Manage** | anything else configured, including a tool the engine is mid-flight on | goes to `#/settings/integrations/{tool}` |
| **Learn more** | nothing configured and this build answers no form for it | opens the tool's page on docs.crewlet.ai |

The three that open the form are disabled — never hidden — with *Setting an
integration up needs an operator token* when `/setup` refused the read. A
tile carries no Disconnect: **the tool's own page is where Manage goes**, and
it holds everything else — the facts, the agents and any faulted surface
(open on arrival, behind a disclosure that can fold it away), each agent's own
step at the vendor (*Create app on GitHub*, *Install on GitHub*), the settings
square, **Disconnect**, the provisioning passes and each surface's deliveries.
A dashed tile at the end of the grid, **Add an MCP server**, opens the add form
on Tools & MCP (`?add=server`) for every other tool an agent can reach. See [integration reconcile](../concepts/integration-reconcile.md#what-the-dashboard-shows).

**Tools & MCP** is the servers first, then the catalogue. **MCP servers** draws
the engine's `mcp_servers_status`: each server's launch (the command and its
arguments, or the address — never an `env` value or a header), who it
**reaches** (every agent seat, *n seats* or *No seat*, from each agent seat's
`tool_sources` on the pushed org — the engine's own grant), the engine's
**state** (*Running*, *Partly failing*, *Failing*, *Not started*, *Not
reported*) and **one chip per live node** with what that node started, and
under THAT chip the first failure it reported, in secondary ink with the seat
it was launched for — the chip carries the danger tone and names the node, so
the reason does not repeat either. A per-seat server on a roster that carries
no `tool_sources` reaches *Unknown*, never *No seat*; a shared one reaches
every agent seat by the engine's own rule whatever the roster says. The launch
is one line cut at its end, never broken inside a token. A node whose build
does not report is named once above the grid and its chips read *not
reported*, never *none*. The **From MCP servers** tile counts *n of m servers
running* off the same answer — never the registry's origins, which read *0*
over a table of failing servers. The section is the operator's — it names
nodes, commands and failures — so a reader without the credential sees the
refusal in its place and the catalogue around it unchanged. A row opens the
**server's page** (`servers/{name}`): its state, transport, instances, reach
and tool count, the launch whole, and every node's reason as the heartbeat
carried it, unclamped — the one place a reason the grid clamps can be read
without a pointer — above the
catalogue narrowed to its origin, whose chip is drawn and selected even when
the server registered nothing. A reader refused the status (or one whose read
failed) is told the page could not read it — never that no configuration
carries the server, which only an answer can say. An origin with no tools says so — *docs has
registered no tools*, with the reason from the engine's state and **Show all
tools** — rather than *No tool matches “”*, which is kept for a search that
found nothing. **Add an MCP server** is drawn for every reader and
disabled with the reason for one who cannot change the configuration: a
name (its example is never a name the company already has; a name the
configuration carries is refused, and one only a node still runs from an
earlier revision is free and said beside the field, since the engine's
create judges the configuration alone), how it runs
(*Launch a command* or *Connect to an address*), whether
there is one for the company or one per seat granted it, the command and its
arguments one per line (or the address), and the environment or headers as
`NAME: value` rows whose value is a secret field (`$` offers the sealed
entries). Add checks the whole company with the server in it (the create-only
`PUT /config/mcp-servers/{name}` with `If-None-Match: *` and `dry_run`), says
any warning the add introduces — not one the company already carried — and
places a refusal beside its field; then it stores the revision. A tool's own
panel names the seats **granted** its server from the same pushed
`tool_sources`, so opening a tool, or stepping through them with `[`/`]`,
asks the engine nothing.

**Models & keys** draws the engine's `credential_pool`: every model the
company configures in config order (the order a seat that names no model falls
back through), its **state** as the engine judged it (*Ready*, *Fewer keys*,
*All keys cooling*, *No key*, *CLI login*) and **one mark per key** in
declaration order — the variable it names (`ANTHROPIC_KEY_A`), the vendor's
own variable marked *default* when the model names none, or *key n · inline*
for a key written into the configuration itself — in its state's tone, a
cooling key carrying how long it has left; several marks wrap two to a line
where the column allows. The bench times sit one cause per line (*rate limit
1h* over *auth 5m*), so neither is ever clipped to a different value, and a
model no seat names reads *–* on its row and on its page alike. A key is **never a value**: the answer has no
member one could travel in, and a key the document holds inline is drawn by its
position. The deadline is the later of the answering node's own bench and the
**fleet's cooldown ledger**, so a key a peer benched a second ago reads
*Cooling* here before this node's refresher has pulled it; a node that could
not read the ledger says its cooldowns are its own and why. Three tiles count
the models (and how many need attention), the keys ready now, and when the next
cooling key comes back. **Runs** is the seats whose resolved chain (the org
projection's `llm`, the engine's own resolution) starts with the model, and
how many fall back to it. A row opens the **model's page** (`models/{id}`):
its facts, the state's sentence when it is not ready, every key with its
state, when it comes back, why (*Nothing sets ACME_KEY on this node*, *The
same key as key 1*, and its Back cell says *As key 1*, since the pool
holds it once), this node's leases of it and the non-reversible **hint**
the engine's `credential_cooled` log lines carry, then **Seats on it** — each
seat and which phases run on it or fall back to it. A seat's model chain on
its Settings tab links here. **Edit** (the page's action, and a row's pencil)
is drawn for every reader and disabled with the reason for one who cannot
change the configuration: it reads the entry (`GET
/config/llm-providers/{id}`), edits its model id, its keys (secret fields —
`$` offers the sealed entries; a key written inline is a locked row that can
be kept, removed or replaced in its place, never shown — and while one is in
the list the list neither grows nor shrinks and no removal moves it, because
the engine puts an inline value back **by its position** and a moved mask
would come back as its neighbour's key), its endpoint and its two bench times
(named for what benches a key — *rate limit 1h · auth 5m* — rather than by
status code), checks
the whole company with the change in it and says only a warning the edit
introduces, then stores it with `PUT /config/llm-providers/{id}` and
`If-Match`, sending back every field it did not show exactly as read. The
answer is polled every 15 s while the screen is open — the node's own
cooldown refresher's cadence.

**Secrets** lists the names the fleet holds, their key ids and provenance, and
can store, rotate and remove one over the same guarded routes the CLI uses.
Every row is sealed in the fleet's store; **Source is provenance** — which path
last wrote the row (`api` for this page or `PUT /secrets`, `cli`, `setup`,
`provision`, `rekey`, `migrated`) — and the credential's peek says what its
word means rather than leaving it to a pointer. Three tiles count the
credentials, the names **read by nothing** (no field in the active
configuration points at them: a forgotten `${VAR}`, or a credential nothing
needs any more — not known, and never 0, when the reference check did not
answer) and the distinct key ids. The peek says each property once: its
header carries the name, and **Where it came from** carries Source, Key id,
Set by and Updated.
**It never asks for a value**: there is no reveal, and a rotation asks for the
new credential rather than showing the old one. **The name is the table's one
flexible column and is never cut** — it is what a reader matches against the
`${VAR}` in their configuration — and every fact beside it is its content's
width. Where the row cannot hold them all, whole columns give way in this
order: **Key id** (one id covers every row until a rekey), **Set by**,
**Source**, then **Updated** — each a fact the credential's peek carries —
and **Read by**, what would break if the name went, never does. Beside a peek
at 1440 the name, Read by and Updated stay. See
[the secret store](../concepts/secret-store.md).

A node, a secret, an MCP server, a model and a backup domain are each **titled
by their own name**, so the trail draws that word as a name too, in the face
the page's title uses — never the mono face kept for a key still waiting for
the name a screen publishes. A tool's trail follows its title, as its header
does.

**Nodes** is every live node's lease, broker kind, posture, config epoch and uptime. **The
node is the table's one flexible column and every fact beside it is its content's
width** — the role chips on one line, a count as wide as its head — so no column
is a share of the row: three shared columns once put a single digit in 150px of
air while the chips wrapped onto three lines, and under a peek fell to a letter
("R.", "S.", "I.") with the chips as empty pills. Where the row cannot hold every
fact, whole columns give way, in this order: **Lease** (every listed node's is
unexpired by definition), **Broker** (a kind that changes with a restart at
most, named again by the Broker members panel), **Up since**, **Roles**, then
**In flight** — each one a fact the node's peek carries, **Roles** and
**Broker** among its header facts — and the grid names what it hid. At 1280
Lease and Broker give way; beside a peek at 1440, Node, Seats, In flight,
Posture and Config stay. The **Broker** column is the kind the node's presence
advertises — `member`, `leaf` or `client` as a neutral tag, `unknown` as a
warning since it is the one reading that is a guess, and nothing at all from a
build older than the field. Then
**Seat placement** — each seat, the node holding it, its lease and **Since**:
the tenure's start (`acquired_at`), stamped when the epoch is minted and
carried through every renewal, so "since 2h ago" means the seat has not moved
node in two hours. A lease an older build wrote carries no stamp and reads
**Not recorded**, never "Never". A node's own page draws the same column for
the seats it holds. The seat (and, beside it, the duty) is each lease table's
one flexible column and **Held by**, **Since** and **Lease** size to their
content, so the name the row exists to show is the value that keeps its width;
Seat placement and Company-wide duties sit side by side only where each panel
is at least 480px wide and stack below that (about a 1520px window). **Held by**
is the same link to the node's page in both tables, at the body's weight, so
the seat or the duty stays the strongest text in its row. A node's build
version is one token in its header, on one line with the whole string in its
title. Neither screen repeats a count in its page bar that a tile below
already shows. See [seat ownership](../concepts/seat-ownership.md).

Below the lease tables, **File storage** is the object store: which store the
company's files are in — the fleet's own NATS bucket, or the S3 bucket it names
— in the header, then one row for each of the collector's passes (the hourly
collection and the daily audit) with when it ran and what it found. It reads
the fleet's record rather than this node, so it says the same thing whichever
node served the screen. **Missing chunks** are the one reading an operator
opens it for: when the last audit found any, a danger callout above the passes
names the first of them — parts of files nobody can download, to restore from a
backup. A record the coordination store would not give up is said as that, not
as a fleet whose collector has not run. There is nothing to press: the store
keeps its own copies, so there is no gesture on it. **Broker members** is
the fleet broker's membership twice — as the nodes advertise it and as the
metadata group counts it — with every disagreement named and the removal of a
dead member behind a typed confirmation. See [the object
store](../concepts/object-store.md) and [the fleet guide](../guides/fleet.md).

**Backups & retention** reads `backups` beside the retention panels. **Take a
backup** asks the node serving the page for a copy (`POST /backup`) in a
directory on **that node's host** — the dialog names the host first, since
nothing is downloaded — and offers a fresh directory beside that node's last
copy when it has one (stamped to the second, never one a backup already went
to, and nothing when that copy sat directly under `/`); a relative path is
refused before the round trip, and the engine's own refusal of a directory —
occupied, or one its host cannot create or write — is said on the field. The request
waits up to 30 minutes, the CLI's own wait, and closing the dialog does not
stop the copy: the outcome arrives as a notice either way. Three tiles (the
newest backup the trim counts, the `backup_floor` policy, and how many were
requested and failed — over the span the event log reads, named from the
`event_history_seconds` the engine reports rather than written into the
screen, or "in the newest N" when the history filled its page), then **Newest per owner** — each node's newest copy and
the operator's acknowledgement, with its directory, size (none for an
acknowledgement, never 0), reach, and whether the trim counts it: exactly one
row reads **counted · newest**, the engine's choice rather than the latest
date, so a newer copy nobody verified reads **not verified** — and **Backup
history**, every backup a person asked any node for over the event log's 30
days, with the node that holds it, the person, the directory and whether its
manifest was written. A failure is drawn as one. Neither table is derived from
the other: the register keeps each owner's newest point only, which is why a
failed night shows in the history alone.

**Audit log** reads five sources over one window: the tracker's feed, the
knowledge base's changes, the configuration history, the credential rows —
and **Runtime**, the runtime audit (`events{source: "operator"}`), which is
every call a person made through a running node whatever became of it: each
act tool call that is not a proven read, and every backup. A runtime row names
the tool (or "backup" and its directory), the person the credential is bound
to, and in its detail's title the node that served it; its arguments are
never recorded and the cell says so. A refused or failed call is marked as a
failure. The tracker's feed and the runtime audit are windowed by the engine,
the other three are narrowed here, and a page that filled before it reached
the window's start is named in a notice above the card — windowed or not. A
long value in the To column is cut with an ellipsis and carries the whole of
itself on its title (on a phone card it wraps), and an empty one says why in
the row's own terms: a tool call's arguments are "Not recorded", a backup row
with no directory says so. The
export carries the node and the failure beside the eight columns on screen.

**Configuration** reads the company document through four lenses — the
active revision, its entities (the kinds declared once, `ENTITY_KINDS`, and
held against the engine's by `entities_client_test.go`, drawn as a list of
handles read down its left edge beside the one it opened), the revision history
and a diff. **The Diff lens draws the changes first** and the revisions they
are picked from under them — beneath fifteen rows of history the changes began
off the screen, so **See its changes** landed on a list — and a revision
pressed further down brings the changes back on screen (only when they are off
it; focus stays in the list). **A change is drawn whole**: its mark (`+`, `−`,
`~`, said as a word to a screen reader), its path wrapping rather than cut, and
its value as JSON — an object or a list in the document's own indented shape —
in tracks the whole list shares, so the values start at one edge; on a phone
the value takes the line under its path. A revision's peek and its page say
each property once: the header carries its id, summary and state, and **Where
it came from** its full id and parent (both in the mono face), Source, who
created it, when, and when it was activated. In the history (and the Diff
lens's list of revisions) **Summary is the one flexible column**: the revision id and its *active* pill are exactly
their own width, and When and By size to theirs, so the id is never cut and the
summary takes what is left — never less than 12rem: **beside a peek, When
gives way** (the revision's peek draws Created) rather than the summary. **By puts the name before its kind**: where the
column is narrow — beside a peek — the kind's chip leaves the line before the
name gives up a character, and the name's title keeps both. A picked handle is a **list selection**, drawn as the column's
current row is (the raised neutral surface and its hairline), never as the
primary button: the accent belongs to the one primary action, and a violet
fill under a handle in its own grey ink was 1.5:1. `#/settings/config/revisions`
is the history, so it lands on the History lens. **A revision's own page is the
revision** — its header, where it came from and which nodes report it — with
no lens bar and no running document under it, and **See its changes** opens
the Diff lens against the revision's *parent*, which is what that save
changed (against the active revision, the active revision's own changes were
empty). **It is read-only for the document**: a revision is written by
`PUT /config`, `crewlet config import`, or a screen's own scoped write (a
budget ceiling, the org builder), each of which validates against the schema
before anything is stored.

## How it is built

```
dashboard/                  the source — React 19 + TypeScript, built by Vite
  src/contract/             every declaration an engine test holds against the
                            engine — data and shapes, importing nothing else
  src/protocol/             the wire, typed. NO React, NO DOM at module scope
  src/app/                  shell, hash router, IA, command palette
  src/lib/                  store bindings, one clock, formatting, derivations
  src/ui/                   the component library and the chart kit
  src/components/           the pieces more than one screen draws, composed
                            out of the design system
  src/routes/<workspace>/   one directory per workspace — home, inbox, me,
                            work, agents, live, knowledge, spend, settings — and
                            one file per screen inside it, with an index.ts
                            naming what its lazy chunk exports. Two things sit
                            outside that shape: NotFound, which belongs to no
                            workspace and is what the dispatch falls through
                            to, and routes/org/, the organization builder — one
                            screen big enough to be a directory, and a chunk,
                            of its own (see below)
  src/styles/               tokens, base, components, shell, frame, screens —
                            what the design system does not draw
static/dashboard/           THE BUILD OUTPUT — committed, and what the binary embeds
```

**The build output is committed**, and that is deliberate: `go build ./...` and
`go install …@latest` must work on a clean checkout with no Node on the machine,
and an embed directive cannot run a bundler. A stale bundle would compile,
embed, serve and pass every Go test while running code nobody wrote — so CI
rebuilds it and fails on any difference from the commit (`make dashboard-check`),
the same idiom as `go mod tidy -diff` and the generated `schema/`. A difference
includes a file the rebuild wrote that the commit does not carry at all: every
emitted name is content-hashed, so a changed chunk is a new path, which
`git diff` cannot see and `git commit -a` never stages.

| To | Run |
|---|---|
| change the dashboard | `make dashboard && git add -A -- static/dashboard` — then commit it with your source change |
| develop against a running engine | `make dashboard-dev` (proxies to `localhost:8000`) |
| run its suites | `make dashboard-test` |
| check the committed bundle is current | `make dashboard-check` |

**The dev server forwards exactly what the source reaches.** `make
dashboard-dev` proxies to an engine on `localhost:8000`, and
`protocol/proxy.test.ts` reads every URL the tree hands `rest`, `fetch`, a
socket or a beacon, and every path a JSX `src`, `href` or `poster` names, the
shell's `index.html` included — parsed, up to its first substitution — against
the proxy table in `vite.config.ts`. The markup is read because the browser
fetches it with no call to read: the tab icon and the sidebar's brand mark both
answered 404 in the dev loop while every call was forwarded. It fails five
ways: a path no entry forwards, which Vite would answer with a 404 of its own
that a screen cannot tell from the engine's refusal; a socket whose entry does
not upgrade; an entry nothing reaches, which reads as a dependency somebody
relies on and outlives the screen that once did; an entry whose prefix covers
the dev server's own `base`, which would hand the dashboard's modules to the
engine; and a call whose path it cannot read, unless it is the REST transport
forwarding its caller's path, which is named with its reason.

**What the engine owns is declared once, in `src/contract/`.** Several lists
exist on both sides by necessity — the dashboard is a separate build in a
separate language and cannot import a Go identifier — so where a screen must
know a set the engine owns (the event categories, the turn bands, the wake
reasons, the work tracker's sort keys, change kinds and grouping axes, the
config kinds, the query error codes, the push kinds, the feed length, the
write vocabulary and its refusal codes, the questions a write is read back
through) or read
an answer an engine test holds it to (the integrations row, a seat's memory,
the health envelope), the declaration lives in one module per concern there,
and nowhere else. Each is held against the engine by ONE Go test, through
`internal/clientsource`, which finds a declaration by its name and reads it by
its syntax, and whose `Contract()` table names every one with its reader and
its owning gate. The table is held both ways: every row is declared in
`src/contract/`, and every name that directory exports is a row. A second copy
declared in a screen is two declarations, which the reader refuses.

A contract module is PURE, and `contract/contract.test.ts` is what says so: it
imports nothing but its siblings — not React, not the design system, not a
screen's helper — declares no behaviour, loads under plain `node` exporting
only plain data, and every name it exports is imported by something outside
the directory. That is what lets `protocol/` compose the contract's shapes
into `protocol.js`, and it imports them by a RELATIVE path, because that second
build has no `~` alias and an import through one is left unresolved there
rather than failing it.

**The dashboard used to be held to reading, and is now held to writing as
you.** Three gates carry that, each owning one side of it:
`TestEveryActionTheDashboardTakesIsOneTheActTransportServes` (`ACTIONS` against
the real operator catalogue, with every seam wired so the catalogue is the
largest any company serves), `TestTheDashboardKnowsExactlyTheActRefusals`
(`ACT_ERRORS` against the transport's codes and the tool refusal classes, both
ways) and `TestEverySessionQueryTakesAFreshnessFloor` (`SESSION_QUERIES` against
the questions the engine serves at a floor). On the dashboard side
`app/source.test.ts` holds every `act(` and `useAct(` to a literal tool, and
`app/writeGate.test.tsx` holds the one write to one hook and renders every
write control for every kind of reader. The gate these replaced checked a
block of copyable tool calls; the block is gone, because the button is now
the call.

`static/dashboard/protocol.js` is a **second** build target: the protocol layer
alone, unminified, importable by plain `node`. `internal/e2e/golden_test.go`
runs a real company, captures every frame its socket pushed, and replays those
bytes through it — so the gate asks "does the client understand what the server
sent", not "did the server send something". That is the gate that caught a full
turn's worth of `agents` pushes being sent as an object keyed by role while the
client guarded on `Array.isArray`: both sides' own suites passed and the seat
rendered idle from the first phase to the last.

The captured company is shaped so that every field a live screen keys on has
a real value to be read, rather than a null the client would read as easily
as the value it replaced: its turn is woken by a TASK assigned to the seat, so
a live call names its `work_item` whole and states its `max_rounds`; a seat's
`turn.stage` is seen in `phase`; a second seat caps its own day below one
model call, so the gate refuses it for real and its meter arrives with
`refused_at`, `resets_at` and `state: refusing`; and the health push has to
count its live `nodes` and name the worst of its standing `alarms` (the company
has never taken a backup, so one stands). The replay and the Go half each hold
all of it, so a red run says which side stopped.

The capture ends with a **write**. The founder's seat binds a bearer token, and
the test pins a project through `POST /operator/act/set_pins` — served by the
node's own operator surface, the one `crewlet run` builds
(`api.NewEngineOperator`), so the harness wires no second catalogue. The Go
half holds the answer to the read it promises: its `position` is in the grammar
a read accepts back, and `GET /work/people/{handle}` at
`read_level=session&min_position=<position>` is served at `session` and already
holds the pin. The replay hands the same answer, byte for byte, to the client's
own `act` and `SessionFloors`: the tab's `tracker` floor must be that position,
and a read of `work_person` or `work_items` must name it as its `min_position`.
A floor the client could not parse raises nothing, and the screen that pressed
the button would redraw from before the press with nothing to say so.

The suites themselves run in **Europe/Berlin** under jsdom (`vitest.config.ts`), and
`src/test/environment.test.tsx` says so by what the zone produces — both
offsets and the 23- and 25-hour days — rather than by reading the config: under
UTC, the zone of every runner, a whole family of date bugs cannot be
reproduced at all. The same suite renders one design-system component, because
every kit component imports its own stylesheet and a suite that cannot load a
`.css` file fails to LOAD, reporting no tests rather than a failure.

**The engine's own suite loads the shell the way a browser does.**
`internal/api` fetches `/dashboard` from the server, never from disk, and
follows every file a browser is sent to: the shell's script, preload and
stylesheet links, every static `import`, every lazy `import()`, the preload list
Vite writes beside one (`__vite__mapDeps`, the only place a lazy chunk's own
stylesheet is named, its entries relative to `base`), each stylesheet's `url()`s
and any `/static/` path a module holds. A file that does not answer 200 with the
type a browser requires fails the build, and the failure names the file still
asking for it, because that is where the fix is. The converse fails too: a file
under `assets/` that nothing reached. The build writes nothing there that the
page does not load, so an unreached file is either dead weight in every binary
or a reference the crawl cannot read — and a crawl that silently skipped a lazy
chunk would certify a screen nobody can open. Every stylesheet the crawl
reaches, a lazy one included, is also held to the Content-Security-Policy.

**A chunk per workspace, and the next one fetched while nobody waits.** Every
screen is loaded by `app/lazyScreen.ts`, one chunk per workspace's
`routes/<workspace>/index.ts` plus one for the org builder, which is a section
of Agents and larger than any whole workspace. The entry the shell loads holds
the frame — the sidebar, the page header, the palette, the socket client — and
nothing a screen draws, so a reader who came to read the Inbox parses the
Inbox and not the org builder. A screen's peek lives in its workspace's chunk
beside it. Two things keep the second click fast: hovering or focusing a
sidebar row starts fetching the chunk it leads to, and once the first screen is
up every other chunk is fetched in turn, one at a time, each in an idle moment
the browser reports (and not at all for a reader whose browser asks to save
data). `app/lazy.test.tsx` refuses a static import of a screen's module from
outside `routes/`, because the bundler follows one wherever it is and it would
pull that workspace back into the entry with nothing failing.

**A chunk that does not arrive says so, and can be retried.** The usual reason
is an upgrade: the page names its chunks by content hash, and an engine
upgraded while the tab was open serves different ones. The screen then names
what is missing ("The Spend screens could not be loaded"), says the page is
probably from another engine version, and offers Reload — which is what fixes it — beside Try again,
for the other reason, a request the network dropped. Try again really asks
again: a failed load is forgotten rather than cached for the life of the page
the way `React.lazy` caches one.

**The bundle has a budget, measured as the engine sends it.** The split above is
undone by one static import of a screen's module from the frame — the bundler
follows it and the workspace is back in the entry with every other test green —
so `internal/api`'s `TestTheDashboardFitsItsBudget` measures the crawl described above
and fails a build over any of four numbers:

| What | Budget | Measured as |
|---|---|---|
| The initial load: the entry, its static imports and the linked stylesheet | 300 KiB | gzip at the best level, which is what the engine serves |
| Each chunk fetched later: a workspace, the org builder, a lazy stylesheet | 150 KiB | the same |
| Every font face the build embeds | 90 KB | as served — woff2 is never recompressed |
| The whole of `static/dashboard`, which every binary and image carries | 3 MiB | raw |

The initial load is the shell's scripts and stylesheet and everything they reach
through a static `import`, derived from the modules themselves rather than from
the order the crawl met them in, so a chunk the entry asks for lazily and a
vendor chunk imports statically counts as the initial load it is. A failure
lists the files largest first. The values and their reasons live beside the
test's constants; `vite.config.ts` sets `chunkSizeWarningLimit` to the raw
equivalent of the chunk budget (439 kB, at the most compressible ratio the build
writes), so the bundler warns at or before the point the test fails.

**The design system's stylesheet is one sheet, above ours.** A uilet component
imports its own stylesheet as a side effect of its module, which in a
code-split build puts a component reached only from one workspace into that
workspace's chunk — and a browser appends a lazy chunk's sheet after the
dashboard's own, so every one-class tie the dashboard's rules win would flip
the first time a reader opened that workspace. So `main.tsx` takes every
component sheet the kit and its icons ship as one stylesheet between the token
sheets and ours (`vite.config.ts`'s `designSystemSheet`, over the kit's own
single-sheet build), and every per-component import is answered empty. The
engine's suite holds the result from the built artifact:
`TestNoLazyStylesheetCarriesTheDesignSystem` fails any stylesheet a lazy chunk
brings that carries a design-system rule.

### What the client half guarantees

- **The store derives nothing.** The server computes the projection once and
  pushes the result. This layer once held a second implementation of the
  engine's state machine — an event→state map, sandbox lifecycle tracking and
  an 85-line reimplementation of the token aggregation — and three copies of
  that logic meant a refresh routinely disagreed with what had been on screen a
  moment earlier.
- **The hierarchy is the engine's, too.** A seat's handle, the unit a root
  seat's `unit:` reference moved it into, a unit's inherited lead, what a unit
  name in `manages` expands to, automatic management by a lead and which of
  several managers is primary are all rules of the engine, and the `org`
  projection carries their result in its `derived` block. `lib/seats.ts`
  indexes that block over the authored tree and implements none of the rules:
  its earlier TypeScript copy derived a different handle than Go for a name
  like "İlker Demir", and a handle keys a seat's memory. A projection with no
  `derived` block (an older engine) is indexed as the document wrote it, and
  every screen reports reporting lines and inherited leads as unknown rather
  than computing them.
- **What a redacted document holds is shown as what it is.** A credential
  field arrives as one whole `${VAR}` reference, shown as the name it is, or
  as the engine's mask, shown as "A literal value is set (hidden)". The mask is
  never printed as if it were a value, and a credential field holding anything
  else (a partial reference such as `Bearer sk-${SUFFIX}`) is hidden the same
  way whatever the engine sent. The guarded half of a screen is drawn only
  while the guarded read succeeds: a refused re-read takes it off the page
  rather than leaving the last answer beside the banner.
- **A region that throws takes only itself down.** Without a boundary React
  unmounts the whole application on a render error, which is what a seat whose
  `llm` was a per-phase mapping once did: a blank page and no way out. So
  `app/boundaries.tsx` puts one around every region that draws what the engine
  sent, and never around the frame, which is the reader's way somewhere else:
  - the ROUTED SCREEN, which renders "This screen could not be drawn" with the
    error's message and Try again, inside a frame whose sidebar and ⌘K still
    work. It resets when the resolved path changes; a query string alone (a
    filter, an open peek) is the same screen and keeps its failure on view;
  - the PEEK's body, inside the rail, so the rail's Close and the screen
    behind it stand. It resets when the peek moves to another object;
  - the COMMAND PALETTE, whose failure is a dialog of its own ("Search could
    not be drawn") that closes the way the palette does — the next ⌘K is a
    fresh palette;
  - each LIVE SIDEBAR SECTION — Projects, Pinned, Starred, the engine card and
    the account block — which keeps its heading and says "Could not be drawn"
    with Try again, so a malformed project row costs the Projects list and
    never the navigation above it.
- **One REST transport, one REST loader.** `protocol/rest.ts` is the only
  path to a REST route: `rest.request(method, path, options)` answers the
  status and the `ETag` beside the body, takes a caller's `AbortSignal`, never
  lets the browser cache a guarded answer, and resolves a 304 rather than
  throwing it; the body-only wrappers sit on top of it. The deadline and the
  caller's abort cover reading the body as well as waiting for the headers,
  and a body that breaks part way through is status 0 (an answer never fully
  heard, so a write's outcome is unknown) rather than an empty success. A
  screen that reads a REST answer uses `lib/useRest.ts` — the Secrets,
  Integrations, Setup and Audit screens each carried a loader of their own,
  one of which never re-read on a new token. The one reader outside it is the
  org builder's session over the `/config` document, which is not a screen's
  read but half of a conditional write: it holds the `ETag` a save sends back
  as `If-Match`. It aborts a
  superseded read and an unmounted screen's, re-reads when the operator token
  changes, when the socket comes back after a drop and, where asked, when the
  tab comes back or on a poll — those last quietly, keeping what is on
  screen. A refusal replaces what is on screen; a request that never reached
  the engine keeps the last answer with the error beside it; and an answer
  belongs to its key, so a read whose key changed reports nothing until the
  new key answers.
- **A retry is the engine's to schedule.** A socket refusal is asked again
  after its `retry_after_seconds` and a REST refusal after its `Retry-After`,
  quietly, and nothing else is retried on its own: a 503 with no hint is a node
  no wait repairs (no keyring, no surface), and re-asking it only repeats it.
  There is no retry constant anywhere in the client, because a flat one is
  wrong both ways on one fleet — too early for a node grinding through a bulk
  apply, too late for one that caught up in milliseconds.
- **A newer peer's frame is ignored and counted.** A push kind this build does
  not dispatch is dropped rather than thrown on or applied by a guess, and the
  store counts it (`unknownPushes`), because the same fall-through is what a
  kind this build's engine sends and its client forgot looks like; the e2e
  replay fails on any. Every field the engine gained after a screen was built
  against it is optional in `protocol/types.ts`, so an older node's answer
  without it takes the screen's "unknown" branch rather than reading
  `undefined` as a value.
- **Nothing outside `src/protocol/` reaches the network.** A screen that called
  `fetch` itself would work on the happy path and be the one request in the
  product with no operator token, no deadline and no refusal it could branch
  on. `protocol/transport.test.ts` parses every module and refuses, outside
  that directory, any READ of `fetch`, `XMLHttpRequest`, `WebSocket`,
  `EventSource` or `WebTransport` — bare, or off `window`, `globalThis` or
  `self` — of `navigator.sendBeacon`, and a form that posts to a URL of its own.
  A read rather than a call, because `const send = fetch` is how a call escapes
  a scan of calls; `refetch()`, a type, a member that shares the name and a
  test kit assigning a stub are not reads, and the suite certifies both
  halves.
- **A read names its question.** The first argument of `useQuery(` and
  `query(` — bare or as a method, like the socket's `.query(` — is a string
  literal: never a variable, an expression or a template with something
  substituted in. The engine's registry gates
  (`TestEveryQueryARoomMakesIsAnswered` and
  `TestEveryQueryThisServerAnswersHasAReader` in `internal/api/queries`) read
  the kinds this tree asks at those calls, by the name alone, and can read only
  a constant, so a kind passed through a variable is a question neither can
  hold the registry to. `app/source.test.ts` reads the same calls the same
  way — both names by spelling, whatever they are bound to — and refuses a
  non-literal kind, a method named by a computed key (`socket["query"](`),
  which the Go reader cannot see, and an aliased import. The two calls that
  forward a kind — the hook's own body and the builder test kit's stub socket
  — are named there with their reasons.
- **Subscriptions are per-slice.** `agents` is pushed twice per tool-loop
  round and once before each of its calls; a store that woke every listener on every envelope would re-render the
  application several times a second for the length of a turn.
- **A query WAITS for the socket rather than failing.** Screens issue their
  first query as the page boots, so rejecting when not-yet-connected made every
  deep link render "could not load" and stay there. Queries are pure reads, so
  one in flight when the socket drops is re-sent on reconnect.
- **A question whose parameter is not chosen yet is SKIPPED, not sent.** Absent
  params are an empty params object on the wire, not a skipped question, so
  `useQuery("work_project", project ? { key: project } : undefined)` reads like
  a guard and is not one: the engine refuses a question missing a parameter it
  has no default for, and the screen shows an error for something nobody asked.
  The engine classifies it as `bad_params` and logs it as `stream_query_refused`
  at DEBUG — the caller's fault, not the node's, so it is not a warning and not
  in an operator's log at all unless they went looking. The guard that works is
  `enabled`, and a test scans for the shape that admits an empty parameter
  without one.
- **A refused credential is diagnosed over HTTP.** A handshake the engine
  answers 401 never reaches the page as `close(1008)` — a connection that never
  opened has no frames, so the browser reports 1006, the same code it gives for
  an engine that is simply down. A plain `GET /ws/stream` runs the same guard
  and stops one line short of the upgrade: 401 is a refused credential, 426
  means it was accepted.
- **One clock.** Every relative time on screen advances together and none of
  them is baked at render.

### The components come from the design system

- **`@crewlethq/ui` draws it, and the engine composes it.** The palette, the
  glyphs, the overlays, the layer stack, the primitives, most of the chart
  kit, the canvas and the tree model are the design system's, shared with the
  console and the documentation site, so a control looks and behaves the same
  wherever somebody meets it. Everything with a peer in the package is the
  package's: the badge, the panel, the empty state, the stat row, the
  skeleton, the select, the tab strip, the avatar, the banner, the disclosure,
  the search input, the button and the chip — and the ranked list, the legend,
  the stacked bar, the activity strip and the sparkline with them.
- **What is left of `src/ui/` is of three kinds, and none of them is a second
  recipe for something the package already draws.** A VOCABULARY the package
  does not have and should not — `Tone`, the six words the engine's own
  payloads are spelled in, and the single translation of them into the
  package's spelling. A TABLE rather than a recipe — `PhaseTag` draws the
  package's `Tag` and adds the one thing the package cannot know, which of the
  six phase strings the engine emits map onto the three hues it publishes. And
  the few components this product needs that no shared design system has,
  because each is about a company's own data: the histogram, the facet rail,
  the `window=` control, the meter, and the three charts the package has no
  shape for — a time series whose series may be a REFERENCE rather than a
  measurement, its stacked form, and the phase ramp. `src/components/` above
  it is COMPOSITION, each piece built out of the package and each one about
  this engine's own domain — a seat, a phase, a turn, a configuration field.
- **The shell is the package's `AppShell`, and what sits in it is ours.** The
  window, the one sidebar, the floating sheet, the 52px bar and the drawer
  below the shell breakpoint are the kit's geometry, taken whole; the page
  header's two rows, the state bar, the peek as a column of the sheet and the
  Settings column are this product's — [The frame](#the-frame) is the whole of
  it. A screen's own half is its labels, its coverage and its controls,
  portalled into the bar; every list it draws is
  [one grid](#every-list-is-one-grid).
- **One stack decides which surface a key belongs to.** Dialogs, sheets,
  menus and listbox popups register on the design system's layer stack
  (`useModalLayer`, `usePopupLayer`), in the order they opened, and so do the
  shell's own token dialog, its command palette and the narrow layout's
  sidebar drawer: no modal hand-rolls its veil or listens for Escape beside
  the stack. That drawer is the sidebar itself below the kit's shell
  breakpoint (1024px) — the kit's `AppShell` owns it — a dialog only while it
  is open; closed, it is out of the tab order rather than only slid away, and
  it closes on a route change (the shell hands the route to the kit as its
  `navigationKey`). Only the topmost
  surface handles Escape or a press outside, so a prompt over the node editor
  closes on its own Escape and leaves the editor open, a token dialog raised
  by a refused request over that editor does the same, and an open menu
  closes before the dialog it sits in. A modal traps Tab, moves focus in when
  it opens (honouring a field's `autoFocus`) and returns it to whatever opened
  it. When that control has gone with a modal that closed as this one opened
  (a panel's "Set token" handing over to the token dialog), focus goes back
  where that modal would have sent it; when it has gone from a modal that is
  still open, or is still there but can no longer take focus (disabled by the
  action the modal confirmed), to that modal rather than behind its veil. A
  modal closed
  beneath a surface still open above it (by a route change or a data push)
  leaves focus where it is. A control that consumes Escape itself, such as a
  completion list, keeps the key. A veil press closes its modal on the
  press's click rather than on its first contact, so the tap that dismisses a
  dialog never also lands on the control the veil was covering.
  The page's own shortcuts ([every one of them](#the-keys)) wait for the page: while a modal is open (`isModalLayerOpen`) they do
  nothing, because the page behind `aria-modal` is inert and search opened
  over a dialog could navigate away from under it, unmounting an unsaved
  editor or a write whose outcome the operator has not seen. Search closes on
  its own chord from inside it: the palette reads `Mod+K` off its own
  surface's keys, because the page's binding is standing aside for it — and
  a key pressed in a dialog raised over it never reaches it.
  What happens inside an open menu stays there: its keys, presses and clicks
  do not reach the card or row it was opened from, so Enter on "Delete" is
  never also the card's Enter.
- **Anything drawn as a button is `Button` or `ButtonLink`, never a
  hand-written `.btn` class list.** `Button` passes a ref, aria and data
  attributes, `id`, `tabIndex` and `onKeyDown` through, so a menu trigger or
  a list's Move control is built on it rather than beside it; it refuses
  `className` and `style`, so the variants stay the only way to style it.
  `ButtonLink` is the same recipe on an anchor, for an action that goes
  somewhere, and its `external` form opens a new tab without the referrer or
  the opener. A control drawn as a glyph ALONE is `IconButton`, whose name is
  required: an icon-only button with none is announced as "button".
  `styles/classes.test.ts` is what holds that, and it fails in BOTH
  directions: a `.btn` list spelled beside the package's own button is a class
  nothing declares, and a recipe left behind in the stylesheets is one nothing
  draws.
- **A shortcut hint is `Kbd`, never a hand-written `<kbd>`.** The
  command key is Command on Apple platforms and Control everywhere else, so a
  literal "⌘K" tells most readers to press a key they do not have. The glyphs
  are hidden from assistive technology, which reads the key names instead
  ("Control plus K"), because a screen reader reads "⌘" as "place of interest
  sign". The command palette's own hints use it too.
- **A key an input method is composing with belongs to the input method.**
  Somebody typing Japanese, Chinese or Korean walks candidates with the
  arrows, accepts a word with Enter and abandons it with Escape, and every one
  of those presses still reaches the page. `isComposing` decides once whether a
  press is part of a composition, reading both the `isComposing` flag and the
  key code Safari leaves on the Enter that ends one, and the layer stack, the
  listbox keys, the multi-select and the list field all leave such a press
  alone: search does not close or navigate in the middle of a word, and a list
  field does not add half of one.
- **A canvas never takes the page's scroll.** The design system's `Canvas`
  fills the box its screen gives it and clips, and the shell's one scroller
  stops scrolling while it is on screen: the screen ASKS for the window's
  height (`useFillScreen`) and the shell answers, rather than a rule in the
  screen's own stylesheet reaching up at a wrapper the shell draws. A plain
  wheel pans the canvas only while focus is inside it, and Ctrl or Command
  with the wheel zooms toward the cursor. On touch, one finger scrolls the page
  until the canvas is tapped, after which it pans and a visible Done control
  gives the finger back; two fingers pinch at any time. `+`, `-` and `0` zoom
  and fit only while the viewport element itself holds focus, so an item's
  own keys are never taken. The view fits once, on the first measured layout,
  and after that moves only when the operator moves it. The two charts of the
  organization — the live org chart and Edit org's — hold that first fit at the
  LEGIBILITY FLOOR (`ui/canvasView.ts`, 85%): a company too wide to read
  whole is drawn at exactly the floor, in one ctrl-and-wheel gesture solved
  for the point that centres the node the reader is on (or the root) across
  the canvas and puts the chart down it where the kit's own fit would, pulled
  just far enough to keep that node on screen; only the reader's own Fit goes
  below it. The canvas is a
  `LayerHost`, so a menu or picker opened from an item renders in an
  untransformed layer over it rather than inside the transform, and the zoom
  neither scales nor clips them; the canvas tells that layer whenever the
  content beneath it moves, so an open surface follows what it is anchored to. Fullscreen belongs to the screen,
  which must take its dialogs and toasts into the fullscreen element with it.
- **Every dropdown is the design system's own, and no screen draws a native
  one.** A platform dropdown is painted by the operating system: it takes none
  of the theme, none of the density and none of the tokens, so a dark dialog
  opened a light grey menu in the middle of itself, and the product read as
  two products in one surface. What a reader used to get free from the
  platform, `Select` earns back and shares with every other list in the
  package: type-ahead, Home and End, disabled rows stepped over, the highlight
  announced through `aria-activedescendant`, Tab closing the list and carrying
  on, and a stored value the options no longer offer kept rather than swapped.
  It is also the only control that can do what several of these surfaces need
  at all: a search over dozens, a group heading, and a second line under an
  option, which is where a choice's hint belongs. Choosing MANY is `TagsInput`
  over the same keys, because a multiple select cannot be searched and loses
  its selection to a stray click, and completing a `${NAME}` inside a longer
  value is `Combobox`, which filters on the name under the caret rather than
  on the whole field. Search's results take those keys too: its input is a
  combobox naming the highlighted result, and the list is the modal's whole
  body rather than a popup over it (`popup: false`), so Escape and the veil
  stay the modal's. The one choice that is not a `Select` is a value picked IN
  PLACE from a menu an item already opens, such as a unit's lead chosen from
  its chart card: a list there would be a second control inside the item and a
  second click after the first. Its answers are `menuitemradio` entries (a
  `checked` item of the design system's `Menu`) that say which one is current,
  nested in one `group` apart from the menu's actions so a screen reader
  counts them among themselves, and the form that edits the same value still
  uses the list.
- **The layout is measured, never assumed.** A chart card's width is the
  `--crewlet-tree-canvas-card-width` knob, declared on the builder's own root;
  its height is measured in the browser and fed to the design system's own pure
  tidy tree layout, because density and the reader's font size change every
  card's height. Nothing is shown before the first measurement, and a relayout
  keeps the node the operator acted on where it was on screen.

### The org builder

`routes/org/builder/Builder.tsx` is the one component with a lifetime in the
builder: the reducer over the pure model in `routes/org/builder/model/`, the
dry-run check, the live region, the shortcuts, the selection and the dialog
that is open. Its views and dialogs are handed in (`BuilderSurfaces`, bound
in `routes/org/builder/surfaces.ts`) and reach all of it through
`BuilderContext`, so no view or dialog starts a request or touches storage.
Every surface is required: a Builder suite stands a view in with a fake,
while the screen binds the real chart, table, editor and dialogs, and
`surfaces.test.tsx` mounts the builder with exactly those.

- **The posture is what the engine answers.** `GET /config` is read on mount
  and on every token change, and its answer decides edit mode, create mode,
  a node that has not caught up, a request for a token, a process that does
  not serve the configuration, or an unreachable engine
  ([the guide](../guides/org-builder.md#opening-the-builder) has the table).
  A stored token is never the test: an engine with auth disabled needs none.
  A token change mid-edit keeps the draft and checks it again.
- **Every draft is a dry run of the write a save would send.** The same
  `PATCH` with `If-Match` (or `PUT` with `If-None-Match: *` in create mode),
  plus `dry_run=true` and without the audit summary. A draft that changes
  moves its generation, and an answer for an older generation is dropped.
- **The canvas is handed the chart it draws.** `chart=structure|reporting` is
  the Builder's own section param, chosen in its toolbar, so the canvas is
  given the answer as a prop rather than reading the URL a second time.
- **The visualization fills the screen.** With `view=visualization` the
  builder asks the shell for the window's height (`useFillScreen`), and it and
  the tab panel it sits in become flex columns of definite height, so the
  canvas takes what is left under the toolbar and the shell's scroller has
  nothing to scroll. The table view withdraws the request, and so does the
  posture screen the builder draws before the engine has answered.
- **Every node is neutral.** Colour is state, and a draft is doing nothing:
  the chart asks the design system for no `cardTone`, so no seat carries a hue
  of its own, a person's seat is told from an agent's by its badge's outline,
  and the one colour a node takes is the selection's accent ring. Its branches
  are the design system's elbows, the shape the live org chart draws, so the
  chart a reader edits and the one they watch are one drawing
  (`CanvasView.test.tsx`, "colour is not identity"). They are drawn at the
  weight the design system gives a chart of NODES, twice a card chart's,
  because a node is half a card's height and the branch keeps its ratio to
  what it joins; the builder sets no stroke of its own.
- **Fullscreen takes the builder container**, never the canvas: the toolbar,
  the view, the dialog host, a toast outlet of its own and the live region all
  render inside it, because a fullscreen element renders only its subtree.
  The control is not drawn where the Fullscreen API is missing. The shell's
  token dialog is outside it, so asking for a token leaves fullscreen first.
- **The selection is in the URL, and the toolbar mirrors it.** `unit=` and
  `seat=` are filters that name the selected node; a link naming one selects
  it, a rename rewrites it, and a removed node clears it. **The address the
  builder was MOUNTED on is also a request**: `seat=` or `unit=` opens that
  node's editor and `add=unit|agent|human` opens the Add (under `unit=`, or at
  the top level), once, as soon as the builder can edit — loaded, keyed by the
  engine's handles, in edit mode and not paused — and `add=` then leaves the
  address. Only the arrival does it, because the builder writes `seat=` and
  `unit=` on every selection and a Back restores an older one; a reader who
  selects another node first has spent the link (`editWiring.test.tsx`). The toolbar carries
  the selected node's own actions, because a canvas tree item may contain no
  tab stops of its own, and they are the card's and the row's own list
  (`nodeActions.nodeMenu`) rather than a copy: the same entries, order, names
  and icons, Edit reports included. Open seat is offered only for a seat the
  saved company has. A node's key can move under whatever holds it: the first
  check keys a loaded base by the engine's handles, and a save keys the nodes
  it created. The reducer lists what moved (`state.rekeyed`), and the
  selection and an open dialog read their node through that list in the very
  render the keys change. Each dialog is mounted once per opening, never keyed
  by its node, so a key that moves under an open editor keeps its drawer, its
  form and its focus.
- **One polite live region** says what each operation, undo and redo did (an
  undo or a redo names itself, since the operation's own sentence is in the
  past tense and would announce the change as just made), and focus moves to
  the node it touched through the mounted view's registered handle
  (`useBuilderView`).
- **Undo and redo are Ctrl or Command with Z**, and Shift with it, anywhere in
  the builder except a text field, and never under a modal.
- **A newer revision is an update, never an overwrite.** A check answering
  `409` (or a dry run validated against another base) halts checking and
  offers Update my draft. A draft holding no work is updated at once instead,
  an update of nothing confirmed as it lands, and never by reading the
  configuration again: a plain read keys the seats that declare no handle by
  their paths until the next check, so an open editor lost its node, typed
  form and all, and the selection was cleared. The update is read only from a
  node serving the conflict's revision or a descendant of it, with the
  engine's description of it, and `UpdateDraftDialog.tsx` shows what still
  applies, what is dropped and every conflict with its three values;
  confirming waits for a choice on each. A value is shown as a person reads
  it: the model tags a conflict over its own structures with their shape, so
  a node key reads as the node's name, a removed node as the fields that
  changed, a kind change's stripped fields by name only, and a credential's
  mask as "A literal value is set (hidden)", never as the marker or as JSON.
- **Only the operation log is kept, and nothing is written before it is
  decided.** `useDraftKeeping.ts` reads the kept draft once the base is keyed,
  offers Keep or Discard for the same revision (read-only until answered),
  restores through the update flow for another, and discards one kept for the
  other mode. It writes nothing before that decision, because the plan for an
  empty log is to clear. A token change or a refused token clears it and
  withdraws an offer. A draft with changes asks the browser's prompt before
  the tab goes (session storage does not outlive the tab), and one that
  storage cannot keep at all asks before the builder is left, since that loses
  it too; a move within the builder keeps it and asks nothing.
- **A save is the checked write, signed, and never believed blindly.**
  `useSave.ts` sends the model's save request with the audit summary signed by
  a write id minted when the review opens and kept for as long as the draft
  does not change, so a second press after a lost answer carries the same id.
  A lost answer is settled from the revision history before anything else
  happens; while it is unknown the builder records nothing. A write in flight is
  never aborted, and a save that lands after the builder was left still records
  the revision and clears the kept draft. The kept log is marked with the
  write id before the save is sent and unmarked once the save is known not to
  have landed, so an answer lost after the builder was left, or across a reload,
  is settled on the next visit (`useSave.resume`) before the kept log is
  offered: replayed onto its own revision it would apply every change twice.
  A save this page still has out when the builder opens again is waited for
  first, because the engine may not have stored it yet and settling it then
  would read as not landed. Such a save is not the draft on screen, so the revision it stored is read
  like any newer revision rather than made the base. A save of the draft on
  screen makes that draft the base, keyed by the save's answer, and the
  stored document is then read back, never over an edit made while that read
  is out: such an edit already stands on the saved revision.
  `ReviewSaveDialog.tsx` states every change and consequence and gates the
  irreversible ones on an acknowledgement, whose sentences (`dialogParts`'s
  `ACKNOWLEDGEMENT_TEXT`) the editor's company rename shares.
- **Create mode is a form, one template operation, and a create-only write.**
  `CreateCompany.tsx` collects the charter, the starting shape and an optional
  seat for the operator, and records the model's `applyTemplate` (refused
  outside create mode), so the start is a single undo and is checked like any
  other draft. The save is `PUT` with `If-None-Match: *`; a company that
  appeared meanwhile is offered instead, and the draft is discarded rather
  than replayed onto it.
- **A save is not an apply, and the screen says so.** `AfterSaveStrip.tsx`
  keeps `{revision_id, epoch}` in a tab-lived store outside any screen
  (`savedRevision.ts`), reads this node's `applied_epoch` off the health push
  (which carries it every five seconds, so there is nothing to poll) and the
  `fleet` query on `recheck.ts`'s cadence, and resolves to Applied, Applied on
  N of M nodes, or the node that refused it with a link to Settings › Nodes.
  Its View changes opens the saved revision against its parent
  (`against=`), since against the active revision a save that is active now
  differs from nothing; the conflict banner's Show what changed is the newer
  revision against the draft's base for the same reason. Until this node
  applies it, the org chart and Teams carry a note that they draw the
  previous revision.
- **The status is the last answer about the current draft**, whoever asked:
  a save's refusal is placed on the nodes like a check's, and the check
  machine decides the status only while a check is out or before any answer.
- **A read-only builder records nothing.** The guarded and read-only postures, a
  conflict and a base the engine has not keyed yet all refuse operations at
  the one door every view goes through, and say why in the live region. The
  actions themselves are DISABLED rather than hidden, so an operator still
  reads what the builder does; Edit and Open seat change no draft and stay
  available.

### The org builder draws the engine's organization

Agents › Edit org shows the draft two ways, and both
read one module (`routes/org/builder/chartModel.ts`), so they can never
disagree about where a seat is drawn, who leads a unit or who a seat reports
to.

- **The document gives the shape, the engine gives the meaning.** Units,
  their seats and their children are drawn as the draft holds them, because
  that is what an operation edits. Every derived fact comes from the last
  check's `derived` block, read through the document that check was sent: a
  root seat the engine placed in a unit by its `unit:` reference is drawn in
  that unit and marked "Declared at the root with a unit reference", a reference that names no
  unit stays at the root marked with the engine's warning, a unit with no lead
  of its own shows the lead it inherits, and a seat shows its primary manager.
  A derived fact is shown only while the draft still holds the values the
  check saw (for an inherited lead, that includes where each unit above sits);
  after an edit the chart says it is waiting for the check rather than showing
  a placement or a lead the engine has not confirmed. A seat's primary
  manager follows from the whole organization, so no single field says it
  still holds: it is shown only from a check of the draft as it stands. A
  value the engine has not given yet reads "Lead after the check", "Manager
  after the check" or "Handle after the check", never that a check is
  running: whether one is on its way, or the engine cannot be reached at all,
  is the toolbar's check status to say, and a card cannot know. A seat's
  handle is one rule for every surface and for the reducer that records by it
  (`model/document.knownHandles`): declared, else the one its key carries,
  else the one a check derived while the seat keeps the name that check saw,
  since the engine derives an undeclared handle from the name alone. The
  editor and the dialogs read it, the seat a reported handle names and the
  Datadog fallback from `chartModel.ts` too, so a card and the dialog it opens
  never name a seat two ways. The reporting
  chart, drawn from the last check while the draft has moved past it, says so
  in a note over the canvas.
- **The canvas is a tree of nodes.** The structure chart has the company at
  the root, and the root seats and the units hang off it; every seat is a node
  of its own, hanging off its unit like the unit's child units, joined by the
  design system's elbow branches. The reporting chart is the engine's forest: seats
  with no manager at the top, marked "No manager", and seats that manage each
  other in a loop under one "Reporting cycle" group, each loop drawn from its
  first seat in the engine's order. The reporting chart is read-only, because
  a reporting line is not written anywhere as such; "Edit reports" (and Enter
  on a reporting card) opens the seat's editor at its Manages field, where
  `manages` is. The Builder's `openEditor` names the part of the form to start
  on (`EditorSectionName`), so an action about one field lands on it: a lead
  chip's "Choose another seat" opens the unit's editor at its lead.
- **A node holds no control a keyboard has to reach.** Each node is a
  `treeitem` with its level, position and expansion, and
  one roving tab stop moves among them: the arrows, Home, End and type-ahead
  walk the tree, Enter edits, Delete or Backspace deletes, and the ContextMenu
  key or Shift+F10 opens the node's menu. The buttons a pointer uses (expand,
  Add, More and the lead chip) sit beside the treeitem, hidden from assistive
  technology and out of the tab order, and open the same menus. **Nothing in a
  hidden strip ever holds focus**: a press on one of those buttons focuses the
  node instead, so a screen reader always has something to announce and a menu
  closing hands focus back to a card rather than to a button nobody can reach.
  Focus moves with `preventScroll` and the canvas then pans to reveal the node,
  opening a collapsed unit on the way. The Builder decides which node is
  focused after an add, a delete, a move, an undo or a redo; the mounted view
  performs it.
- **Colour stays state.** A node is neutral whatever it holds: no seat has a
  hue of its own (one hashed from its key, stated in its editor as a
  "Colour" fact, was removed — a legend for a decoration the live chart never
  drew). A seat leads with its badge, a person's circle or an agent's
  squircle, on the same solid node — a dashed edge means only "a place nothing
  fills yet" — a reference that names nothing takes the caution tone, and the
  Datadog fallback seat carries a neutral mark while Datadog is enabled, the
  only time the engine routes an alert to it.
- **A chart node says what it is, not how it is doing.** It carries neither
  the seat's live state nor a problem count: drawn on every node they were a
  column of "idle" dots and an empty box beside every node with nothing wrong.
  The table writes the live state beside a seat's name, as the same
  `StateBadge` every other screen draws, in a slot that neither shrinks nor
  wraps while the name truncates, so a push changes a word and never a row's
  height; the table's Problems column and the toolbar's status count the
  problems. A mark the engine gave (a lead or a unit reference that names
  nothing) is held by the chart like a placement, while the node still writes
  what the check warned about, rather than leaving with each check and coming
  back with its answer.
- **The table is a treegrid of rows.** The same structure as rows with
  navigable cells: Name (the kind or type under it), Handle, Lead or reports
  to, Problems and the row's actions. A row's keys are a card's keys; Right opens a row or
  steps into its cells, Left steps back out, Up and Down keep the column, and
  a cell holding a control (the lead choice, the actions menu, an add button)
  focuses the control, which becomes the grid's one tab stop. A press that
  opens such a control moves that tab stop too, because the control keeps the
  focus and hands it back when its menu closes; the row's chevron, which is
  hidden from assistive technology, takes no focus at all and hands the press
  to the row. Navigation keys are the grid's before they are the control's, so
  ArrowDown on a menu button moves to the next row rather than opening the
  menu. The grid scrolls sideways in its own box at narrow widths, and that
  box is the containing block of everything in it: a scroller clips only the
  descendants placed inside it, and a screen-reader label placed against the
  frame instead gave the page a sideways overflow as wide as the grid. A box
  that scrolls on one axis clips on both, so a row's menus open in a
  `.popup-layer` over the grid, placed from their trigger, and follow a
  sideways scroll (or close once their trigger has left the frame) rather than
  being cut off under the last rows. The company's row and each unit's carry
  the Add that splits into the three kinds. Alt+Up and Alt+Down move a row
  among its siblings of the same kind, past the row drawn beside it: a root
  seat the engine placed in a unit by its reference is drawn in that unit, so
  the root seats around it step over it rather than make a move nobody can
  see. Because the engine's primary manager is the first seat that lists a
  seat, the reporting lines the next check reports are compared with the ones
  before, and a changed primary manager is announced.

---

## Rules a change has to keep

1. **Colour is state, never identity.** No hash-to-hue, no per-agent tint, no
   per-category chip colour. If you need to tell two things apart, use their
   names. The third-party app marks in `@crewlethq/icons` are the one, bounded
   exception (see "The one rule" above); nothing else is.
   **A seat's identity badge is `Avatar`, everywhere, through `SeatAvatar`**
   (`ui/SeatAvatar.tsx`; `seatBadge` for a kit component that draws the badge
   itself, such as the chart's `OrgLabel`), drawn from its name or handle so
   the initials are what tell one seat from another. For an AGENT a leading
   word "agent" is dropped before the initials are made, because the squircle
   already says it: "Agent CEO" and "Agent CTO" are `CE` and `CT`, where the
   kit alone drew both as `AC`. What a screen reader hears is still the whole
   name. Its `kind`
   is its OUTLINE — a circle for a person, a squircle for an agent — which is
   the one cue telling them apart: who holds a seat is a structural fact and
   therefore a shape rather than a hue. A kind the chart does not hold is
   read from the writer's recorded kind where the screen has one (an operator
   token is a person no chart lists), and otherwise takes the kit's default,
   the agent's squircle. A hand-rolled mark holding a robot glyph drew the KIND, which the
   roster gives at a glance, in the slot that should have been saying WHO: the
   same engineer was "FE" on the board and an identical generic robot on
   Search and on a project's Lead panel. `ring="brand"` — the accent drawn as a
   ring round the neutral badge, meaning *selected* — is for the one badge that
   IS the reader, in the sidebar's own account row, and for nothing else.
2. **No new colour, size, radius or spacing literal.** If a component needs
   one, the TOKEN is what gets added.
3. **A fill step is never text and an `-ink` step is never a background.** The
   palette suite measures both; an inline `background:` carrying a hue token is
   the specific mistake it exists to catch.
4. **Nothing animates on a data push.** Entrances, control states and the
   steady-state pulse on something live now — and under reduced motion every
   pulse is static and every entrance ends on its first frame
   (`styles/motion.test.ts`).
5. **Every list sorts through `tsKey` with a three-way comparator**, and every
   keyed row uses an identity that survives the row's own lifecycle.
6. **Every empty state says why it is empty** and what would fill it, and
   distinguishes "nothing happened" from "nothing could be read".
7. **A write says what happened, and is made as you.** A change to the
   company's work goes through `act` (`protocol/act.ts`) from `useAct`
   (`lib/useAct.ts`) and nowhere else — `app/writeGate.test.tsx` holds that,
   and renders every write control for an anonymous, an unbound and a bound
   reader — and a change to the company document through
   `protocol/configWrite.ts`. Nothing outside `src/protocol/` reaches the
   network (`protocol/transport.test.ts`). Every write reports its outcome: a
   toast when it landed, a persistent notice with Retry when nobody can say,
   and the engine's refusal beside the control that caused it. A write control
   is never hidden and never optimistic (see "Acting, as yourself"). A button
   whose result is invisible is a button an operator presses twice.
8. **No screen renders a credential.** The setup dialog shows the `${VAR}` a
   field points at, or — for a hand-written literal — an empty box whose
   PLACEHOLDER is dots saying one is held. Never a value, because no route
   returns one, and never dots as the value: a placeholder cannot be
   submitted, and a sentinel that has to be recognised on the way out is one
   an edit can defeat.
9. **A catalogue tile carries one action.** Settings › Integrations draws
   one tile per tool with exactly one control — Connect, Continue, Rotate
   token, Manage or Learn more — because a grid is read in one glance, and
   the card it replaced carried a tag, a Connect, a Disconnect, a settings
   square and a chevron to say that nothing was owed. Everything else a tool
   offers is on its own page, where the card that discloses its agents keeps
   its disclosure on the identity block, never the whole header, so the
   card's own buttons are not nested inside a button.
10. **Every screen, section and filter is in the URL**, and obeys the
    push/replace table above.
11. **A screen subscribes to the slices it reads and no others.**
12. **Numbers are tabular**, and an absent number is a MARKED absence rather
    than a zero: zero is a measurement. `EmptyValue` draws the mark and says
    what the absence means ("Not reported", "Not measured"), because a bare
    dash is read aloud as "dash" or skipped, and a value still arriving is a
    different fact again, which a tile reports by being busy.
    **There is exactly ONE absent mark, and it is `EmptyValue`'s** — its en
    dash in JSX, its `EMPTY_VALUE` constant in a string a module returns. No
    module spells one itself. Half the screens adopted it while the rest kept a
    local `Dash`, an em dash the design system says it "does not use anywhere",
    so the tracker's trash screen drew a 12px em dash in the table's DUE column
    beside a 6px en dash in the activity feed's object column — one fact, two
    glyphs, one viewport. `cells.test.tsx` scans the tree for a string literal
    or a JSX text node that is nothing but a dash; an em dash inside prose is
    house punctuation and is not what it is looking for.
13. **A control reports its own outcome**, especially an invisible one. A copy,
    a download, a write, a revoke — if the reader cannot see the result, the
    control says it, in text a screen reader reaches as well as an icon. A
    refusal is reported too, and it OUTLASTS the confirmation: a reader who
    looked away for three seconds must still be able to learn it did not
    land, so a failed state holds until the next click while a successful one
    settles back.
14. **Per-subject state is keyed on its subject.** A hash change re-renders
    the route switch rather than remounting it, so `#/live/turns/A` →
    `#/live/turns/B`
    reconciles and anything the screen remembers about A — a held refusal, an
    open disclosure, a selected tab — describes B until something clears it.
    Every id-bearing route carries `key={id}`.
15. **A screen head's controls wrap.** A button does not break its own label,
    so a head carrying several of them overflows a phone's line instead of
    taking a second one. The page header and `ObjectHeader` take their badges
    and their actions as slots that wrap, and any row built beside one wraps
    too.
16. **A link reads as a link at every size, and a rule under the words is not
    how.** A text-register class on an `<a>` that overrides its colour makes a
    navigation into decoration; `.t-link` is the caption-sized register that
    keeps the accent. The hover is a step within the text rungs, not a rule
    struck under the words. The exception is the one WCAG 1.4.1 forces and it
    is PERMANENT rather than a hover: an anchor that is a word in a running
    sentence takes `.prose-link`, and `.prose.md a` and `.int-form-note a` get
    it without asking — colour alone is what that clause refuses, and a
    hover-only mark was never an answer to it, since it is invisible to a
    keyboard, a touch screen and anybody who has not already guessed. The
    second exception is `forced-colors: active`, where the rung step cannot be
    seen at all. See *A link is a colour* above for all three.
16a. **And every anchor register answers the pointer.** A register that pins its
    own colour silently takes the `a:hover` step away — `.work-col-foot a` and
    `.int-form-note a` are (0,1,1) exactly as the step is, and the later sheet
    wins — and one that is already `--color-text-primary` at rest, like `.cell-seat` and
    `.wl-who`, is handed a step to `--color-text-primary` that moves nothing. Three of those
    shipped, each invisible for as long as the baseline's hover underline was
    quietly doing the work: with it reset, hovering them changed nothing at
    all. A register whose rest colour is the accent ink steps to `--color-text-primary`; one
    that is `--color-text-primary` at rest steps to the accent ink, which is what says *this
    name* is the link rather than the row around it. Nothing in
    `styles/` can check this today — it needs a real cascade, not a parse.
16b. **Text takes the ink, never the fill.** Every status family is three
    rungs: the base is what a thing is painted *with* — a primary button, a
    status dot, a meter bar — the `-soft` is the ground it tints, and the
    `-ink` is the one of the three that is a text colour. The design system
    measures the pairs it publishes; nothing but `styles/rungs.test.ts`
    measures which rung this application spends where, and both mistakes are
    silent. Spending `--color-brand-accent` as text gives 3.09:1 on its own soft ground and
    3.71 on the page, against the 4.5 small text needs; the ink gives 5.96 and
    7.15. Putting an `-ink` **on** its family's solid fill is the one pairing
    of the three that was never measured — the old workspace rail's attention badge did it,
    at 1.72:1 on a 9px digit, so the unread count rendered as a dot. That
    badge is the ACCENT's ink on the accent's soft ground now: a count of
    things waiting for the reader is "act here", never a caution.
16c. **The faint rung is decoration, and that is a contrast rule.**
    `--color-text-muted` is about 3:1 against the ground in *both* themes, which is
    the floor a non-text mark takes and not the 4.5 a word needs. Spent on a
    word it is a word nobody can read — it had decayed onto twenty-six rules,
    every grid column head and sidebar section name among them. A word takes
    `--color-text-tertiary` (6.1–6.6:1). What is left on the faint rung is ten marks —
    two tree characters, a breadcrumb slash, four icons and three icon-only
    controls — each named in `styles/rungs.test.ts` with the reason it is one.
17. **Run `make dashboard` and commit `static/dashboard` with the change —
    `git add -A -- static/dashboard`, not `git add -u`.** CI rebuilds and
    fails on a diff AND on an untracked file there (`make dashboard-check`),
    because a lazy chunk is a NEW hashed file every time its source changes:
    `git add -u` stages the entry that imports it and leaves the chunk behind,
    and a binary built from that commit serves a screen whose code is not in
    it. A bundle that has drifted from its source is a red build.
18. **Everything the page runs or loads is its own bundle.** The engine serves
    the shell under a Content-Security-Policy that allows scripts, styles,
    fonts, images and connections from this origin only (images also as
    `data:`), and no inline script or `<style>` block
    ([API endpoints](api-endpoints.md#security-headers-on-every-response)).
    A `style` prop is fine, because React applies it through the CSSOM; a
    `style` attribute in markup set as a string, an injected `<style>`, an
    `eval`, or a font, image or request from another host is refused by the
    browser with nothing on screen but a console violation. A form may post
    to this origin or to an `https:` host, which is what the GitHub App
    manifest flow needs.
    **And everything under `assets/` is content-hashed, because the engine
    caches it for good.** `/static/dashboard/assets/*` is served
    `public, max-age=31536000, immutable`, so a browser holding a file there
    never asks for it again; that is correct only because the build names
    each one `<name>-<hash>.<ext>`, so different bytes are a different URL.
    Emit a file with a fixed name anywhere but `assets/` — the fonts,
    `protocol.js`, the notices and the brand marks do — and it is served
    `no-cache` and revalidated on every load, like the shell that names the
    hashed files. `TestEveryFileUnderAssetsIsContentHashed` fails on an
    unhashed name under `assets/`. Text is gzipped by the engine for a client
    that asks, once per file at the best level, so nothing here is
    pre-compressed and no proxy is needed for it
    ([API endpoints](api-endpoints.md#wiring)).
19. **Tokens, never money.** Every spend figure the dashboard draws is a token
    count — on Spend, on a seat's profile, on a task's cost, on a turn's
    phases and in the landing screen's pulse. The engine records a price where
    one is reported, and only a subscription coding CLI reports one, so a
    currency figure covers the minority of calls that quote it while the token
    count beside it covers all of them: on a screen it reads as the company's
    spend and is a fraction of it. So no screen draws a price, a currency sign
    in front of a figure, a currency code or an `Intl` currency format — and
    the client declares no price field at all, because a field
    `protocol/types.ts` declares is one a component is a single line away from
    drawing. The engine's price stays on its API for anyone who asks for it
    ([API endpoints](api-endpoints.md#token-spend-breakdown)). The one place it
    can be read on a screen is a record shown **verbatim**, as the engine
    stored it — an event's own payload — because a verbatim view that dropped
    a field would misreport the record, and nothing there is drawn as a
    figure. Three gates hold the rule, each over a different artefact: the
    SOURCE, where `src/money.test.tsx` parses every shipped module — comments
    excluded, so a note explaining the rule can name what it forbids; the
    SCREENS, where the same suite renders the landing screen, Spend, a task, a
    seat's Overview and Turns tabs and a turn over wire fixtures that carry a price everywhere
    the engine puts one, and reads text and attributes back for any trace of
    it; and the BUNDLE, where `TestTheDashboardRendersNoPrice` scans every
    module the engine serves, lazy chunks and `protocol.js` included, because
    the committed bundle is what a browser runs.
