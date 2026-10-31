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

There are six exceptions, and each is held to exactly what makes it one.

The first is a **third-party app's own mark** on Company › Integrations
(`VendorMark`, from `@crewlethq/icons`): Slack's four colours,
Atlassian's blue, GitLab's orange,
Datadog's violet, drawn as the third-party app draws them. A mark is identity by
definition, and a recoloured Slack mark is not Slack's. The exception is held
to exactly that: a mark is drawn only beside the third-party app's name, or
beside a link or a reference INTO that app in operator-facing words, where it
says whose the pull request or the channel is ([What prose names is drawn as
what it is](#what-prose-names-is-drawn-as-what-it-is)); nothing reads state
from it, none of its hues is reused as a token, and the
integration's STATE beside it is carried by the status tone like everything
else. A tool the company has not set up keeps its mark at full strength:
whether it is connected is the state beside it, and a drained Datadog read as
a placeholder image rather than as Datadog. Crewlet's own services carry their
**product marks** (`ProductMark`) on the same terms: drawn only beside the
service's name, the one place the brand's violet is identity rather than a
token.

The second exception is the organization's two charts, **Company › Org chart**
and **Org chart › Edit org**, which draw it the way the console's org chart does:
every agent seat is its own Crewlet character on a node toned in its own
colour, with the branch arriving at it (and, in Edit org's table, its row) in
the same hue. It is held to that: the hue is the one the seat's profile icon
names (the third exception, below), never a hash of a name, a person's node
and a unit's stay neutral, and the tone is drawn only on those two charts.
Nothing reads state from it: on the live chart what a seat is doing is the
state's dot and word under its name, as it is everywhere else.

The third exception is **an agent's profile icon**: the Crewlet character and
the colour its author chose for it, written in the seat's own `avatar` block
(see the [Role Fields Summary](../getting-started/configuration.md#role-fields-summary)).
A hash of a name is identity by accident; this is identity by CHOICE, so
renaming the seat keeps it and it says exactly what somebody meant. It is held
to exactly that. Its six colours are the design system's node hues
(`--color-node-*`), a family no state, accent or data series draws from. A
seat that chose nothing is the original Crewlet in purple, never a look the
dashboard picked for it. Only an agent wears one: a person is drawn as
themself and a unit is neutral. It is drawn as the agent's own badge
(`SeatAvatar`) and its node on the two charts, and nothing reads state from
it, so the status glyph on its corner and the word beside it still say what
the seat is doing. The
org builder's Add dialog suggests a character no agent in the draft wears yet
and the next colour round the six, so a company growing one agent at a time is
told apart before anybody has chosen; the suggestion is written into the new
seat, where its author can change it.

The fourth is a **QR code** (`@crewlethq/ui`'s `QrCode`), which is not colour
at all but a symbol a camera reads: black modules on its own white plate, the
quiet zone included, in the dark theme as in the light one and under forced
colours too. A code is specified dark on light, and reading one inverted is
something some scanners do and others do not, so a code inverted to suit the
dark theme is one some authenticator apps will not read. Its two inks are the
`color.qr` tokens, which are the same in every palette; nothing reads state
from them and nothing else is painted with them.

The fifth is a **collaborator's presence colour** in an artifact (D22): the
hue a person or an agent present in it wears on the marks they make on its
content, so a reader matches an outlined cell to a face in the presence stack
at a glance. It is held to exactly that. It is drawn only on another
collaborator's cell outline, name flag and the ring round their badge in the
stack while they are present (`PresenceRing`, `CursorFlag`, `PresenceStack`
from `@crewlethq/ui`), always with their name beside it, never on chrome and
never read as state. An agent wears the hue its profile icon names (the third
exception); a person is given one of the six node hues for the session, in the
order they joined, skipping hues already worn in the room, never a hash of a
name and never stored on them. Past six people hues repeat and the flag tells
them apart, and the same fact is in words in every cell's description and the
presence list, so nothing relies on colour alone. The reader's own place is
the focus ring, as everywhere.

The sixth is a **colour value's swatch** (`ColorChip` at the text's size):
where words write a colour (`#ffe100`), a small square of it is drawn before
the value. It is not identity and not state but the value itself, shown. It is
held to exactly that: drawn only beside the value it shows, which stays
written out (the swatch is decoration, so nothing is read from colour alone);
filled only from a value that passed the design system's `#rrggbb` gate, never
from raw CSS; and drawn with an edge of its own, so a colour that matches the
ground is still a swatch.

A seat's badge takes one of three **status glyphs**, or none, from what it is DOING,
and what it is doing is the ENGINE'S word, never the dashboard's. Every seat row carries
`activity` (`working`, `needs`, `stopped` or `idle`) and, when stopped, a
`stopped_reason` (`paused`, `unplaced`, `budget` or `provider`), computed once
by the live projection from the seat's turn, its coding runs' durable record,
its pause, its placement across the fleet and its budget windows (see
[Agent States](../concepts/agent-runtime.md#agent-states)). `lib/seats.ts`
maps the word to a status, a pill's tone, a label and a state line and folds nothing in: the
dashboard used to derive a seat's state three ways on three screens, and a run
parked on a question past the old twelve-hour age-out dropped out of every one
of them. A source gate in `seats.test.ts` refuses any other module turning a
live call's or a run's fields into one of the state words. The two words are
the contract's `SeatActivity` and `StoppedReason`, held to the engine's by a
gate.

**The status is a glyph on the badge's corner and a word in the row, never a
line round the badge.** The kit's `status` (`lib/seats.ts`' `statusOf`, which
passes the engine's own word through and drops idle) draws a small round
badge on the lower-right corner: the state's fill carrying a play for
working, a question mark for needs you and a square for stopped, so the
shape says it to a reader who cannot tell the fills apart. A ring in the
state's hue round a plate in the agent's own hue put two colours on one
small mark and framed what the agent was doing louder than who it is, so it
is gone, and so is the coloured edge a roster card drew down its side. Where
the row has room it says the state in words as well: a decision row opens
its second line with "Needs you" or "Stopped" in the state's ink, an Inbox
row carries its pill, a roster card its state pill, a profile its state in
the engine's word. A badge drawn alone (a sidebar row, a work card's holder,
a spend row) carries the glyph and, where it names itself, the state in its
accessible name. A stack of badges carries none: each overlaps the one
before it, and the sentence beside it says what they are doing.

| Status (pill tone) | `activity` | Label |
|---|---|---|
| `working` (`info`) | `working`: mid-turn, or a coding run it launched is running | "Working"; the state line names the phase and the item ("Executing ENG-412"), or a parked run ("Coding run on ENG-9") — which is what it says for as long as the run works, workers it delegated to included: a seat's workers run under its executor's run, whose calls the engine does not stream |
| `needs` (`warning`) | `needs`: a coding run it launched waits on a question only a person can answer | "Needs you", "Needs you · run parked" |
| `stopped` (`danger`) | `stopped`: paused, placed on no node, its budget window refusing, or its provider unreachable | "Paused by Jane Founder · 12m" (the pauser by the name the chart gives their handle; a token nobody bound, by its own name), "Not placed on any node", "Stopped · budget", "Stopped · provider" |
| **none** — a neutral pill | `idle`, no row from the engine yet, or a human seat | "Idle · last turn 24m ago", "No state from the engine yet" |

A handle is written `@pm`, and a seat with none — a builder seat the engine
has not derived one for yet — gets nothing rather than a bare `@`, which is
why no markup prints `@{handle}` itself: every handle goes through
`handleLabel` in `lib/seats.ts`, and `app/source.test.ts` refuses JSX text
ending in `@` before an expression and a template inside markup that does the
same. A live round that has not moved in two minutes
is called stale, in ten stalled — except while the turn is **parked** on a
detached coding run, which is silent by design for as long as the run takes:
the executor's phase card then reads "parked on its coding run", because the
stage travels with the live call off the seat's own turn.

A failed last turn does not colour a seat. The seat takes its next wake like
any other; its `last_error` is shown where the seat is read about, not as a
stop.

**Where a seat runs is which node holds it** — the peek's **Running on** fact
(**Runs on** on the profile's Setup card), never the human seat's **Held by**:
"this node · node-a", "another node", or "no node", read off this
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
`components.css`, `frame.css`, `screens.css`) come last. One more is a
screen's own: `create.css`, the create pages' rules, which
`routes/artifacts/new/CreateShell.tsx` imports so that it arrives with their
chunks rather than in the first paint. It lands after every other sheet, so it
selects only the create pages' own classes (`styles/classes.test.ts`), and
never a kit class (`TestNoLazyStylesheetCarriesTheDesignSystem`).

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
stylesheets in that same import order, for **every theme the registry lists**
(the default, and each named theme over it under the profile its entry
declares), in **every theme state** (dark on the bare root, light by media
query, light by attribute) and over **every composited surface a token can
land on**, including a hovered row inside a nested panel, which is where a
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
| `--color-text-on-accent` on the accent, `--color-text-on-danger` on the danger fill, `--color-text-on-scrim` on the scrim | 4.5:1 |
| the focus ring against every surface | 3:1 |
| the four state hues, pairwise, under normal / protan / deutan vision | ΔE 10 |
| adjacent data hues, in series order, under all three | ΔE 9, and ΔE 15 under normal vision |
| every data hue against the reserved danger hue | ΔE 14 normal, 8 dichromat |
| every other hue against the accent, under all three | ΔE 10 normal, 8 dichromat |
| the neutral ramp's chroma | ≤ 2.2, or inside the tint a theme declares, with the accent carrying twice the chroma of its most tinted rung |
| text, inks, the muted step and the card's hairline in the high-contrast theme | 7:1, 7:1, 4.5:1 and 3:1 |

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
label, `--color-text-on-danger`, which the package measures to 4.5:1; a theme
whose accent is light labels it dark, so the danger fill and the scrim have
labels of their own). A state is a soft tint
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

### Mode and theme

**Two choices, never one.** The **mode** is light, dark or the machine's, and
is the `data-theme` attribute the design system's contract names. The
**theme** is one of the named palettes `@crewlethq/tokens` ships (Crewlet,
the default, and nine more in five groups), each a full token set with a dark
and a light half, and is the root's `data-palette` attribute: the mode chooses
the half, so a theme never changes the mode and the mode never changes the
theme. `main.tsx` imports `css/themes/all` after `css/themes`, so every theme
is in the first sheet and switching needs no request and nothing new for the
content security policy. The default paints the bare root, so a reader who
has chosen nothing is drawn exactly as before.

**One id is stored, nothing else.** `lib/prefs.ts` keeps the choice under
`crewlet_palette` (`lib/storage.ts`), validated against the registry the
package exports, so an id no build ships (a theme since removed, a hand edit)
reads as no choice rather than a page no stylesheet answers. A reader who has
chosen no theme gets the default, or High contrast while their machine asks
for more contrast (`prefers-contrast: more`); any choice, the default included,
wins over the machine. `bootTheme()` paints the stored mode, density and theme
before React mounts, so nobody sees the default flash on the way to their own,
and a storage event from another tab of the browser applies what that tab
chose, so two tabs never disagree.

**A list previews; choosing applies.** The preferences popover's theme list
and the palette's Change theme step repaint the whole frame in the theme their
highlight comes to rest on (at once for a key, once the pointer has rested on
a row for 120 ms), through `previewPalette`, which writes nothing; the list
closing, emptying, its step being left or the pointer leaving it puts back the
theme in effect, so a preview never outlives the list that started it.
There is no Appearance page: the popover is where a reader sets how this
browser looks. Every pick, from either of the two, applies through one helper
(`app/appearance.tsx`), which says "Theme set to Abyss" in a toast with an
**Undo** that puts back the reader's previous choice, including no choice at
all.

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
reader can go (fifteen **workspaces**, in three runs and a foot) and the
few objects they keep coming back to: the views and artifacts they pinned and
the pages they starred. A workspace's own **sections** are paths drawn as tabs
in its page header (Settings draws its fifteen as a column of four groups, and
lists nothing that lives elsewhere; Worklet draws its as the lenses of the scope
its trail names),
and its objects — a unit, a
container, a node — are rows on the section that lists them.

It replaced two columns of navigation: an 80 px rail of workspaces, and beside
it a 236 px sidebar holding the open workspace's own tree. Every workspace had a
different second column, the reader's place moved between two columns on every
navigation, and the rail spent 97 px of every screen on eight icons. The trees
were never the sidebar's to hold: a unit list is a section of the Org chart, a node
list a section of Settings.

### The grammar, stated once and asserted

- A **sidebar row** is a workspace, or a live shortcut to a pinned view, a
  starred page or a running turn. A project is not one: it is a **scope** of
  Worklet, named and switched in Worklet's trail
  ([Worklet's scope](#worklets-scope-is-named-and-switched-in-the-trail)).
- A **section** is a **path segment** inside a workspace, drawn by one
  renderer: `tabs` in the page header, `lenses` beside Worklet's scope in the
  trail, `column` for Settings, or `panel` for Chatlet, whose screen draws them
  at the head of its Rooms panel. Chatlet draws no tabs because the header
  draws them on a section's own page and never on an object's, and a room is
  the object page a chat reader spends their time on, so Activity, Threads,
  Drafts and sent and Saved head the panel, drawn from the same table, and the
  frame draws nothing for them.
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
  on screen: the five shapes are a segmented row of glyphs ahead of the
  saved views (the kit's `SegmentedControl`, the same switch the Artifacts
  gallery draws its List and Cards with), and **Group by** and **Sort** sit at the far end of the chip bar,
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
  columns half is one component, the kit's `ColumnChooser`, drawn by
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
their reason at the entry. `routes/worklet/toolbar/grammar.test.tsx` holds it against
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
so it is an option in the strip. The five shapes are not options beside it: a shape
is a way of drawing any query, and mixed into one strip the two read as the
same kind of thing. That is also why `view=` and `shape=` are two keys:
switching a saved board to a list must not throw the saved filters away.

**And a narrowing the SCREEN is is not a filter either — it is LOCKED.** One
list serves three screens, and two of them are that list narrowed to something
the reader did not choose: `#/worklet/{KEY}` is the work in one project, `#/me`'s
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
  shape glyphs ahead of the saved views, and the
  landing shape is never conditional on how much work exists, because a screen
  that redraws itself as a company fills up is a screen nobody can learn.
- **A closed-set axis draws every declared value; an open one draws what
  exists.** The ENGINE decides which lanes there are — `work_items` carries
  every column the query's own predicate admits, so a one-item company gets the
  denominator a board's comparison needs and Open draws no dead Done lane. What
  is the screen's is the DRAWING: an empty lane says "Nothing here" rather than
  standing as a heading over nothing. The rule and the axes it covers are in
  [the Worklet guide](../guides/worklet.md#views); a list, a table
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
uuid, and every reserved segment is lowercase. That is what lets `#/worklet/views`
resolve before any answer arrives. The resolver recognises a key by that shape
rather than by elimination: the engine uppercases every container key it
keeps, so a segment under Doclet holding a lowercase letter is a section or
Not Found, never a container: `#/doclet/skills` does not open a container
called `skills` before the screen that section names exists.

The set itself is `RESERVED_SEGMENTS` in `app/nav.ts` and is deliberately NOT
copied out here: a prose list of twenty-one strings is a list that goes stale,
and this one had drifted to fifteen while two of the entries it did name were
reserving route space nothing routed. `router.test.ts` holds the routes named
in the table above against that list and against `WORKSPACES`, so the table
and the code cannot disagree about which addresses exist.

### The sidebar

The order is the product's story, in four runs. You first, under no label:
what needs you, what reached you, what is yours. Then **Company**, what the
company decides: who it is, the policies its agents read, its org chart, its
schedules, the services and apps it runs, and, last, the ceilings it sets on
what it spends. Then **Workspace**, what the company does with all of that:
its work, what its people say to each other, what it knows, what it made and
what is running. The engine stands beside the company's name in the head, and
the foot is the reader, with Settings and Preferences as glyphs beside them.

| Row | Route | Figure |
|---|---|---|
| **Home** | `#/home` | — |
| **Inbox** | `#/inbox` | the accent **badge**: unread notices under a reason the person's record counts as PRIMARY — the only filled figure in the chrome. Not every unread notice: most of a busy company's are things it merely told you, nobody answers those, and a count that never reaches zero reads as a broken counter. Asked of the engine as `unread` + `primary_only` over one page, so a page that fills is drawn as a floor ("50+"). It moves the moment the person's inbox does: the frame watches their own record and asks again within half a second of an `inbox_changed` frame, and the minute-long poll behind it is only the fallback |
| **My work** | `#/me` | the open work assigned to you — the SAME figure the Queue tab carries on your own day, read once by the frame for both, so the row and the tab can never name two numbers. The engine's own total, counted in full rather than the length of a page and written as a floor ("200+") where the count stopped at its ceiling; nothing on you draws no figure. The questions put to you are counted on their own tab (Asked of me) and each reached you as an Inbox notice |
| *Company* | | |
| **Principles** | `#/company/principles` (and `#/company`) | none: the company's mission, vision and policies are every reader's |
| **Brand** | `#/company/brand` | none: the published brand is every reader's, and publishing one is an Admin's on the page itself |
| **Org chart** | `#/company/org` | none |
| **Schedules** | `#/company/schedules` | none |
| **Integrations** | `#/company/integrations` | none; every reader opens it, and an app's state and connecting are an Admin's on the page itself |
| **Budgets** | `#/company/budgets` | none (tokens only; nothing in this product renders money) |
| *Workspace* | | |
| **Worklet** | `#/worklet` | none |
| **Chatlet** | `#/chatlet` | the accent **badge**: what waits on you in Chatlet, the unread messages that name you (or the whole room) in a named room and every unread message in a direct conversation, in the rooms you have not muted; a floor ("100+") where a room counted to the engine's bound or the list was cut short, and none where the counts were not measured. Counted from the frame's ONE reading of your rooms (`lib/chatRooms.ts`), which the Chatlet screen draws too, so the badge and the list beside it cannot name two numbers; re-read on every live chat frame, at the room's rate while a surface draws the rooms and every two seconds at most while only the badge reads it. Locked, like Worklet, for a reader without Chatlet access; a reader who holds it but whose credential acts as no seat is answered by the screen itself, since every chat page is read as a seat |
| **Doclet** | `#/doclet` | none |
| **Artifacts** | `#/artifacts` | none, and never a lock: a share alone admits a reader, and starting a sheet is the one thing in it the Artifacts access gates |
| **Live** | `#/live` | the agents working now, beside the working mark, the chrome's one pulsing dot. On Live because a count says how many of something the DESTINATION holds, and Live › Now running holds the running turns, one row each; on the org chart's row it sent a reader looking for a running turn to the chart |
| *Foot* | | |
| **Settings** | `#/settings/models` | none, and never a lock: Spend's readings are every reader's, and a section closed to the reader is locked in Settings' own column |

Under the workspaces:
**Pinned** (the views this reader pinned, asked for WITH the viewer — a pin is a
person's, and the shared strip has none — and across EVERY container
(`work_saved_views`), so a view pinned from a project board is here with the
project's key as its lead; each with the total it selects, counted in the
view's own container; a
pinned row RUNS the view, `#/worklet/tasks?view=<key>` or its project's list for a view
saved on a project, rather than opening the inventory page about it — and the
total beside it is that list's own: the engine counts a pinned view with every
task filtered on its own, as every surface that runs a view asks for it, never
in the grammar's tree mode, where an open epic's finished subtasks rode along;
then the ARTIFACTS this reader pinned, each a row to its page with its kind's
glyph, listed for every reader the engine serves because a share alone admits
a reader to Artifacts, and read from the one listing All and an artifact's
Pin item read (`lib/artifacts/pins.ts`), so a pin made anywhere moves the
sidebar at once),
**Starred** (drawn only when there is one; **one row is current**: a pin, a
star or a running turn that opens exactly the page on screen is the current
row, and the workspace row containing it gives the mark up), and **Running**, one row per seat
the engine says is working, drawn while one is: the seat's badge with its
status glyph, its name, the key of the item its turn is charged to as the lead,
and a link that WATCHES the turn (its Transcript, the phase it is on open, or
a parked turn's coding run live above its phases; see
[the turn trace](#the-turn-trace-four-tabs-one-clock)). It is the way to a
running turn from every screen, where it was the Agents row, the chart, a
card, the profile and a tab. It is the same set as the figure on Live and in
the order every list of running turns draws — the turn that has run longest
first, and a seat with no readable start last, since the record the engine
creates for it when its parked turn resumes carries the newest start there
is — so a turn that starts is APPENDED and the rows a reader is reaching for
never reorder, and it is LAST so nothing above it moves when a turn starts or
ends. Four rows with a turn id, then one row to Live › Now running for the
rest — **N more running**, or **N running** when no row stands above it. A
working seat whose turn has published no id yet is counted there rather than
linked to nothing, and takes none of the four places. Home's Live now draws
the SAME four rows in the same order, because both are one selection
(`runningShortList` in `lib/turns.ts`) rather than two lists under one cap,
which had named different seats side by side. A seat that needs a person is not listed: it is running
nothing, and what it waits on is the Inbox's and Now running's *Waiting on a
person*. Nothing in it ticks — no elapsed clock and no pulse per row — because
the chrome is on every screen; the Live row's one dot says something runs, and
these rows say which. The foot is the **user block** alone, with Settings and
Preferences as icon buttons at its end: Settings a link to where its row
opened (the kit's `IconButtonLink`, current on a Settings page), Preferences
its popover. The head carries the company's lockup and, at the end of its
line, the **engine panel**'s compact status.

**The sidebar folds to a compact rail.** Above the shell breakpoint the kit's
fold control, or `Mod+\`, folds the sidebar to the kit's 76 px rail and back,
and the choice is the reader's, kept per reader in this browser
(`app/sidebar/railShape.ts`), as their Home board is. Folded, it is the kit's
`CompactNav`: **Search** (the palette the full sidebar's field opens), then
Home, Inbox and My work, each its glyph over its name, then ONE tile for
Company and one for Workspace, each opening a flyout of its places beside the
rail on a point, a press or the arrow keys; the tile carries its places'
badges and is marked current when the page on screen is one of them. Under
the foot's rule stand the engine's status (its dot and alarms), **Settings**
as a tile, and the reader's face alone, which opens the same menu the user
block's row does, with **Preferences** in it, opening as a dialog of the same
three controls. Both shapes are drawn from one list of places, so they cannot
list a place with a different link, lock or figure. The live shortcuts
(Pinned, Starred, Running) are the full sidebar's, and a shortcut is never
current in the folded rail, so the place holding the page keeps the mark. The
company's mark and the fold control stand in the same place in both shapes,
the mark in the head's row and the control in the row under it, so neither
moves when the sidebar folds. Below the breakpoint the sidebar is the drawer,
which is always the full list, and the stored choice comes back when the
window widens.

**Every "New task" is one sheet.** A work screen's
page-bar New task (Home, the Tasks lens, every lens of a project) and a board lane's `+` all open the
frame's single New task sheet (`dashboard/src/routes/worklet/NewTask.tsx`, mounted
by the shell out of the Worklet chunk), each door telling it only what it knows.
A board
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
the field says which before the press. Where the company does not run
Worklet, none of those doors is drawn (see the rule on services above). An applied create that came
back with a caveat (the act answer's `caveats`, in a person's words) says it in
its toast rather than a plain "Filed". The sheet files project, title,
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
refusal stays in the sheet in the words the engine wrote for a person (a
title over its cap is "The title is too long"), or, for a refusal nobody has
worded for one yet, in its kind's own sentence: never the tool's sentence to a
model, which names arguments (ADR-0060). What the engine would
refuse is said on the field BEFORE the press: the title and the description are
counted in the engine's unit (UTF-8 bytes, against `worklet.MaxTitle`, 256, and
`MaxBody`, 32 KiB, held by `contract/work.ts`), counting down in the last fifth
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

**Settings is never hidden, and a lock goes where the closed thing is.** A
section that vanishes without access is indistinguishable from one that
does not exist, so an operator on a fresh browser would conclude the product
has nowhere to configure anything. ONE RULE decides every lock, `sectionOpen`
in `nav.ts`, over three kinds of section (ADR-0033). The reader's own, their
Account, needs nothing. The OVERVIEW needs nothing beyond being signed in:
Home, Company › Principles and Brand, the Org chart (the chart, the roster,
Teams and every seat page's status), Schedules, Budgets, Live › Now running and
Coding runs, and Settings › Token spend: what every seat is doing, what it has spent,
what it may spend and who answers to whom, which everybody who works here
reads. An APP's sections need that app: Inbox, My work, Worklet and Settings ›
Expensive tasks need Worklet, and Doclet needs Doclet, and every
section of Chatlet is chat's. And the
company-wide TRANSCRIPTS (Live › Turns, Agent-to-agent and the Event log,
Doclet › Agent diaries, and a seat's Turns and Memory tabs to anybody but its
lead) and every other Settings page are admin's; Company › Integrations is
every reader's, with an app's state and connecting kept for Admins on the page. Every comparison goes through
`lib/access.ts`'s `holdsAny`, which counts every app an admin carries, so an
administrator is never drawn a lock on an app. A sidebar row is
locked only when NOTHING in its workspace is open, its title naming the access
that would open it — what a reader would ask an admin for; a workspace only
part of which is closed draws its locks where the closed part is, which for
Settings is its column. So the foot's Settings is never locked: it carried a key
whenever any section was closed, and a person holding nothing but the overview
read Settings as closed to them while its Spend readings were theirs to
read. A lock is drawn only once somebody has said what the reader holds,
the viewer, or the session's own answer while the viewer has not — since a
lock while the first read is out claims a refusal nobody has made. That is ONE
READING, `useHeldReading` in `lib/viewer.ts` — who the reader is and what they
hold, from one source — which the sidebar's rows, Settings' column and the
frame's refusal of a section all take, so the column and the sidebar can never
lock one reader differently. The command palette says the same of a guarded
destination in the same words — "needs Admin access" beside its hint — read
off its address by the rule the frame's refusal reads (`nav.ts`'s `closedBy`),
the workspace's access first: every section of Worklet, My work or Chatlet is
offered to a reader without that app saying what opens the workspace, where
a lock read off the section alone offered them unlocked. There is
NO "no access yet" state: a person somebody gave no app still reads the
overview, so the socket is dialled for every signed-in principal and the
frame never replaces the company with a panel saying they hold nothing — they
hold the overview, and a lock beside each app says what would open it. Only a
socket the engine REFUSED — a person whose seat was taken out of the chart —
offers nothing of the company: the sidebar's Pinned section is not drawn, and
the palette asks none of its questions and lists no seat or tool. A refusal
DROPS what an earlier socket sent (`Store.setAccessRefused`), so a tab whose
seat was withdrawn shows exactly what a session that never held one does,
where it went on naming the company and listing its agents from the snapshot
it held — an outage keeps the last state, and a refusal is not an
outage.

**A service the company does not run is hidden, never locked.** A company
runs Worklet, or Jira in its place, or no tracker; Doclet, or Confluence in
its place, or no knowledge base; and Crewlet's own chat (Chatlet), or the
Mattermost or Slack it declares, or none. The engine decides which from the
company document and publishes it to every signed-in reader as `services` on
the org projection, which the frame already reads (`lib/services.ts`). Where
the company does not run one, the places that read
it are HIDDEN rather than locked, because there is nothing to ask an admin
for and a lock would say there was (`serviceShown` in `nav.ts`): without
Worklet, its workspace and My work leave the sidebar, the palette and the `g`
chords, and so does every way to file or move a task (New task, the palette's
Create task, Ask an agent and Assign, a seat's Message and Assign, every
create page's Ask an agent, and New design, whose one way to finish files a
task); without Doclet, its workspace does; without Chatlet, Chatlet
does, with a seat's Direct message and Chatlet's Home widgets. The Inbox stays, because
the seat and budget conditions waiting on a person arrive under every
backend: it draws those, and one line in place of its updates. Home leaves
out the blocks and figures that cannot arrive, and each row of the board
closes up over them: the columns a hidden block held go to the blocks left in
its row, so no row is drawn short (the saved widths are kept, and Edit Home
shows the hidden block's cell saying why). The tray's lines say what each
widget shows where it is placed. An address under a hidden
place draws a notice in place (`app/ServiceNotice.tsx`): what the company runs
instead, in the engine's word, and why the page asked for is not here (a New
design address says a design is made by an agent working a task). It is
readable by every signed-in reader, and offers the service's own page under
Company › Integrations (`#/company/integrations/worklet` or `/doclet`) to a
reader who may open it, which is every reader. It
is drawn only once the engine has answered, so a deep link opened on a cold
tab is never refused on a guess. Until the org projection arrives every
place is DRAWN as if its service ran, so nothing is hidden on a guess, but no
read is ASKED of a service (`useServicesAsk`), as an app the reader holds
counts as not held, and a service's page waits for the projection before it
mounts: nothing is asked of a service before somebody has said the company
runs it.

**A guarded section says so and nothing else.** Opened by a reader the engine
has SAID holds none of the access it needs, a guarded section is replaced by
one refusal (`app/frame/GrantRequired.tsx`) — "Nodes needs Admin access",
who the reader is signed in as and that an admin can give it to them, and
**Switch account** — drawn by the frame, before the
screen mounts. The access is said once, in the title: the button repeated it
("Sign in as somebody holding …"), and a button
does not wrap, so on a phone it ran past both edges of the screen. The reader
is named from the reading the refusal was decided on (`useHeldReading`), never
from the viewer beside it, which has not answered in the moment between the
session's answer and the socket's — named off it, the refusal told a person
signed in that nobody was. It names ACCESS by the word People gives
it (Admin, Worklet, Doclet, Chatlet) rather than a credential,
because a signed-in person lacks access, and "needs an operator credential"
sent them looking for a credential they have no use for. A screen that mounted anyway was
refused and drew the refusal as data: "0 nodes" in its header, four tiles of 0
and "No nodes are reporting", under a banner calling the refusal the last
reading that succeeded, while the Settings column beside it counted one node.
The section waits for the frame's FIRST answer about the reader — one round
trip, behind a placeholder — so it asks nothing it may be refused; a cold load
of Secrets used to send its guarded reads, be refused, and draw the refusal a
frame before the frame knew to. That answer is the frame's one reading
(`useHeldReading`) decided by the one rule (`sectionOpen`), so the session's
answer decides where the viewer has not: decided on the viewer alone, a
section was held at "Checking your access to Nodes" until the socket's viewer
answered, while the session it had already read said the reader does not hold
admin. One address an app's lock does not close is a WORK ITEM SOMEBODY WAS
ASKED ABOUT (`App.tsx`): the engine answers the person an open ask names
without Worklet access, so the item's own read decides it. A read that FAILED is no answer and does not hold the
section — treating it as "holds nothing" would lock a reader out until the
next poll — so the screen mounts, and the guarded screens draw no figure from
a read that never succeeded. Edit org is
the one guarded section that answers its own refusal, because its draft is
kept in the tab's storage: replacing the builder the moment the reader's
access was withdrawn would take the draft off screen while the person who wrote it is
still there to copy it out, and the builder says for itself which of its
saves the reader's access reaches.

**Chatlet is guarded whole, and keeps one refusal of its own.** Every chat page
is read AS A SEAT — the engine answers for the seat the session's principal
holds, with no handle parameter anywhere in the chat family and no arm, an
administrator's included, that reads somebody else's rooms — so the
workspace's row and all three of its sections name the Chatlet app, and a reader
without it meets the frame's refusal on every chat page, a room included. What
the frame cannot say is chat's own. A principal that holds Chatlet and acts as NO
seat — a deployment's own token, a service account — reads nothing there, not
even a public room, so the screen says that ("Chatlet is read as the seat you
hold"), naming the binding that would give the credential one (`crewlet iam
bind`). A company whose chat is not Chatlet (`chat.backend: vendor` or
`none`) is no refusal at all: Chatlet is a service like Worklet and Doclet, so
its row is hidden there and an address under it says where the
company talks instead, naming the vendor to a reader holding Chatlet, rather than
showing rooms it cannot have or offering a room it cannot hold.

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

**The engine panel** opens from a compact status at the end of the rail
head's line, beside the company's name (the kit's `AppShell.RailStatus` in the
head's `status` slot): a dot and, while alarms stand, how many, beside a
warning triangle. Its words are its name and its title rather than a line of
the rail ("Engine serving, 5 alarms"), and it reserves its width, so the
company's name truncates at one place whatever the engine's state, and the
panel opens below it. It says the one state that decides what a reader should do, in precedence
order. Two are about THIS BROWSER rather than the engine: nobody is signed
in ("Not signed in": the status is then the button that goes to the sign-in, and
opens nothing; see [Signing in](#signing-in-is-a-screen-outside-the-frame)),
and the engine knows who this is and will not serve them ("Access refused", a
seat taken out of the chart). Then the engine is unreachable, the tab is still
connecting (a neutral dot: nothing has reported yet, which is neither healthy
nor a fault), draining, falling behind (amber, while the push carries
`stall_lag_seconds`, which the engine sends only once the node's watched duty
has missed its beat; the panel says how far behind, "31s behind", a figure
that climbs towards the lease TTL at which the node restarts itself), starting
(a neutral dot, while the node applies its first configuration and its posture
says `wait`), no configuration, a posture
that diverged from the fleet (`shed`, `stuck`, `isolated`), and serving. The
dot is the POSTURE and nothing else: a serving node has a success dot whatever
its alarms say. "Engine healthy" is said only when the alarm table was
evaluated and nothing in it fires, and "Engine serving" otherwise, because
"healthy" beside a count of alarms is a claim the count refutes.

**A refusal is a statement**, repaired by an administrator
rather than by a press, so its row wears nothing a control wears: it is the
kit's static rail row, with no trailing mark, no pointer, no hover and no
place in the tab order, where the sign-in row (which carries a key mark) and
every other state's row (which opens the panel) are buttons. A statement
wearing a control's marks reads as a control that does nothing. Every screen but the reader's own (their Account) is then replaced by one panel that names who they are from
`GET /auth/session`, what they hold and the engine's reason, and offers the
Account, a retry, and the sign-out. A person somebody gave no app is NOT in
this state: they read the overview, and the frame draws it.

Pressing the row in any other state opens the panel above it (the kit's
`Popover`, named "Engine"): the state again with this node's name, then the
live node count ("2 live", or "Count unavailable" where the presence read
failed, never a zero), the configuration epoch this node applied ("Applying"
while it starts), how long it has been up, and the alarms, then **Open node**
(`#/settings/nodes/{node}`) and, while alarms stand, **Open alarms**
(`#/settings/backups?at=alarms`, which brings the alarm list into view without
moving it). While the engine is unreachable the panel says how long ago it
last answered and offers **Reconnect now**, which dials at once rather than at
the end of the backoff. The alarms are listed by name, the longest-standing
first. Their COUNT is the health push's, the evaluation the engine's gauge and
log lines come from; their NAMES are the operator's retention report, the
document Backups & retention lists them from, asked for when the panel opens
and again when the count moves, because the push is public and names only the
one that has stood longest. The report is asked only of a reader holding the
access that section opens on (Admin); anybody else, or a report not
answered yet, is told the longest-standing alarm and how many more stand. A
name is the alarm's kind read as words, its initialisms kept whole ("Backup
age", "Trim blocked"), and names one alarm: the engine raises each kind once,
with the logs it stands on in it, and which logs those are is Backups &
retention's to list (the push names a kind and no log).
`lib/alarms.ts` holds the rule.
A table the engine has not evaluated is "Not evaluated", never "None". The
push counts the nodes holding a presence lease and does not know how many a
fleet should have, so a node that is down is a smaller count and nothing
more. On a phone the row and its panel are in the drawer, and nowhere else.

**The user block** is who this browser is — a principal the identity
directory binds to a seat, which every person is (bound: their name, their
login and their seat; the seat's kind is never captioned, since every
binding is to a human seat and the guard refuses any other), somebody resolved with no
seat (unbound: the login, and *No seat* — an API token's session or
a service account acting as itself, which is ordinary; a person always holds a
seat), or nobody (a **Sign in** button). **The reader is called by their own
name, never their seat's** (`readerName` in `lib/viewer.ts`): their directory
row's name, as `GET /auth/session` answers it, else their login — and the seat
is said beside it as the seat, its name off the chart or `@handle` where the
chart is unread (before the socket's first snapshot, or on a socket the
engine refused, it draws the seat the session names). The seat's name is the chart's name for a
place and often a role: a person with no name of their own was "Founder" in
the block and greeted "Good afternoon, Founder" on Home, while their Account
said they had no name. Home's greeting takes their own name alone, and a person
with none is greeted without one. **A credential that is not a person but acts
as a seat is called by the seat**: a Tier A token bound to Miles's human seat
is "Miles", with "via API token" under it ("via service account" for a
service account), because it has no name of its own and the seat is who it
acts as. The row says only the kind of credential; which one it is ("via API
token review", "via service account ci:deploy") is the row's title, which a
screen reader hears as its description, and a line under the access in the
reader's menu. It read "token:review" over "Miles", the credential's
spelling as the name and the place it acts for as the aside. Its badge is the
seat's own initials wearing a key on its corner (the kit's `Avatar` `mark`), so
the face says who and the corner says how; a credential bound to no seat keeps
the key as its whole face. The badge is the medium step, which the kit's
account row sizes its badge column around in the sidebar's foot. Which
credential it is comes from the login's
grammar (`lib/login.ts`), never the session's `kind`, which the engine answers
`person` for a token acting as a seat. **Pressing the name opens the reader's own menu.** For anybody resolved, the
row is a menu button (the kit's `AccountRow` `menu`), named by the reader and
", your menu", with a title saying what the menu holds. In the sidebar's foot
the kit draws no trailing chevron: its width goes to the name beside the
larger badge, and the row's hover fill says it can be pressed. **The
menu says only what the row does not**: its header is the reader's access, the
one line People uses for it ("Admin", "Worklet · Doclet",
"Overview only"), and under it come **Account** (the one workspace with no
sidebar row of its own, `place: "menu"` in `app/nav.ts`, where the password,
the second factor and the sessions are), then **Sign out** and **Sign out
everywhere**, the latter in the danger ink and saying what it ends. The name
used to link straight to the Account while the session's gestures sat in a
second popover beside it, mixed with the preferences and opening on "Signed in
as" over the very login the row had just said. The sign-outs stay in this
menu because it is reached even by a person the socket refuses; see
[Signing in](#signing-in-is-a-screen-outside-the-frame). It linked to the seat
once, which is the company's page about a place in the chart and has none of
what a person looks for: a person clicking their own name to find their
account met a profile with no security on it, and concluded the dashboard had
no two-step verification. **And it is the Account's row**: the Account has no
sidebar row, so on `#/account` the block is marked current exactly as a row is
on its own page, raised, with the hairline, no hue. **A session read that
failed is not nobody**: where `GET /auth/session` failed (a 503, the network)
rather than being refused, the browser may hold a session, so the block says
it could not tell who this is and its menu still offers the Account (whose
page says what its own read finds) and the sign-out of this browser; nobody is
offered **Sign in**. Beside the row sits one control, **Preferences**, drawn
with a palette and holding nothing else: the mode (light, match the system,
dark), the density and the theme. The two switchers draw one box of three
equal cells each, so a row of glyphs and a row of letters read as the same
control; the theme is a select whose options carry each theme's two halves as
a chip, its name and its tag, and whose list previews (see
[Mode and theme](#mode-and-theme)). The preferences are
per browser and none is the company's: every key the
dashboard keeps in browser storage is declared in one table
(`lib/storage.ts`), and a key no build reads any more is listed there as
retired and removed at boot, so a stale value does not sit in a reader's
profile for ever. The session cookie is the only credential the dashboard
has, and nothing of it is in browser storage. There is no zone or date
preference: every timestamp is drawn in the browser's own zone and its
locale's own date spelling, and that zone is also the one a day is filed in.
A calendar cell and a timeline column are that zone's date, so
a task stamped "Sep 23" is never in the cell for the 22nd — while a day's
arithmetic (the grid, the span between two dates) is civil and moves with no
zone. There is no company switcher: one engine runs one company.

**The lockup is the company**, beside the product's mark: "Nimbus", on one
line, and the way home. The product's name stands in only while no company has
been sent — which includes a socket the engine refused, since a refusal
drops what an earlier socket sent. The kit's own default puts the product first and the company under
it; on this screen the company is the subject, and the product is in the mark
and the tab.

`g` then a letter jumps to a workspace: `g h`, `g i` and `g m` for the first
run; `g c` for Company's Principles, `g e` for Brand, `g a` for the Org chart,
`g d` for Schedules, `g n` for Integrations and `g b` for Budgets; `g w`,
`g t` for Chatlet, `g k`, `g f` for Artifacts and `g l` for Live; `g s` for
Settings, and `g u` for your Account. A chord to a hidden place is not
answered, as its row is not drawn. A chord rather than a
modifier, because every single-modifier combination worth having is already
the browser's. `g r` is the one `g` sequence that is not a workspace: it
watches the running turn when exactly one seat is working, and opens Live ›
Now running — every running turn — when several are, or none. Every key the
dashboard answers is in [one table](#the-keys), and `?` shows it.

### Worklet's scope is named and switched in the trail

Worklet's pages end their trail in the scope they show: **All projects**, or one
project, written as its key chip and its name (`Worklet › FAIR Product and
Engineering`). That last crumb is the page's `h1` and the kit's `Switcher`
button at once (`app/header/ProjectSwitcher.tsx`): pressing it, or `g p` from
anywhere, opens one panel that changes the scope. Beside it sit the scope's
**lenses**, drawn as the kit's chip `Tabs`: All projects' are Worklet's own
sections (Projects, Tasks and History, each a path), and a project's are keys
on its address (Tasks, About, History and Files). Both draw alike, the lens
that is on outlined by its own accent hairline, so the bar reads one way in
either scope, and Worklet keeps no row of section tabs. Worklet is plain words on its
landing, where a link would lead to the page it is on, and the frame's star is
not drawn on these pages. On a phone the lenses take a row of their own under
the bar's line, and Worklet shrinks to its glyph.

**The panel.** A search band, a list that scrolls and a footer that does not.
**All projects** comes first, with the open work across every active project;
then a note when the listing was cut at 200 ("200 of 340. Type to search.");
**Recent**, the last three projects opened with the
current one aside, once a company has more than eight; **Projects**, the
active ones with a 52 px key column, the name and the open count, the current
scope outlined; and the footer's **Browse all projects** and **New project…**,
which is held with its reason for a reader who cannot set a project on the org
chart: a project is a key another system finds a unit or seat by, so setting
one takes Admin, a unit's lead included.
Archived projects are never offered: they are listed under Projects, and the
panel says so where a reader looks. It opens under its button on Worklet's scope
pages, centred over the page for `g p` anywhere else (nothing behind it moves
until a row is chosen), and as a sheet from the bottom edge under 640 px.

**Searching.** Keys and names, ignoring case, with the matched letters bold.
The order is an exact key, then keys that start with the query, then names
with a word that starts with it, then keys and names that contain it, ties by
key (`lib/projectRank.ts`, the order ⌘K uses too). Past the listing's cap the
engine is asked as well, through the listing's own `q`, once typing rests,
which also matches a project's purpose. The count is announced politely ("3
projects", "No project matches"), and a search that matches nothing offers
**Search tasks for “…”**.

**Keys.** `Enter` opens a row in place and `Mod+Enter` opens it in a new tab; a
middle or `Mod` click does the same, because every row is a link. The arrows
wrap and pass over a held row. `Esc` closes the panel and returns focus to the
button, `Tab` closes it and moves on to the lenses, and after a switch focus
stays on the button of the page it lands on. `Home` and `End` move the caret in
the search field, as in every editable combobox the kit draws.

**What a switch keeps** (`app/scope.ts`). Tasks and History carry over, About
and Files carry between two projects, and every other lens opens the next
scope's first lens. Only the list's shape travels, and only with Tasks: a saved
view and the filters belong to the scope they were set in, because a project's
statuses, labels and fields are its own. The browser tab reads the lens, then
the scope: "Tasks · All projects", "About · Product and Engineering".

**New project** (`routes/worklet/NewProject.tsx`) asks for an owner (a team, or an
agent seat above every team, that owns no project yet, each offered by its key
or handle; never a person's seat, which the engine refuses a project) and a key (`KEY_SHAPE`, and
no project or node already has it), then continues in Edit org with that
node's editor open on its **Work** tab at the tracker project field, holding
the key, not yet applied. The org is what makes a project: the engine creates
it when the change is saved. Setting a node's project is an admin's
write even inside a unit somebody leads, because a project is a key another
system finds the node by, so the button is drawn for every reader and held
with that reason for one without Admin.

### The routes

Only a route a screen draws is written here, and `router.test.ts` holds this
table against `routes.ts`'s resolver both ways: every address below resolves to
a screen, and every workspace and section the code declares is below.

| Route | Page | Params |
|---|---|---|
| `#/` · `#/home` | **Home** — the landing screen: the company's day, the engine's one sentence, the pulse figures and what needs a decision | |
| `#/inbox` | **Inbox**: what waits on your decision, and what reached you *(needs Worklet access)* | `row=` (which item the pane is on) |
| `#/me` | **My work › Queue**: what one person holds, by due date or in the order somebody put it, reordered by dragging a row *(the whole of My work needs Worklet access)* | `order=due\|priorities` · `handle=` (whoever leads somebody, or an admin, reading their day, kept across the sections) · on the queue, `shape=`, `cols.list=` / `cols.table=` and the filter grammar with the assignee LOCKED |
| `#/me/asked-of-me` · `#/me/unblocked` · `#/me/collaborating` · `#/me/watching` · `#/me/checklist` | **My work** — Asked of me (answered in place) · Unblocked · Collaborating · Watching · Checklist, each with the engine's own total on its tab | `handle=` |
| `#/me/asked-by-me` | **My work › Asked by me** — the work this person asked a question on that is still waiting for its answer: the work list, held to the asker | `handle=` · `shape=` and the filter grammar, with the asker LOCKED |
| `#/worklet` | **Worklet › Projects**: Worklet's landing, All projects' first lens: every project, its lead, its target date and its four counts *(the whole of Worklet needs Worklet access, but for a task somebody was asked about, which the person asked opens without it)*. The segment and the sort are the ENGINE's question (`shown=` becomes `archived=`, `sort=` travels as written), because the answer stops at the engine's own 200. **Unit** is the one optional column, reached through the **Columns** menu | `shown=active\|archived\|all` · `sort=key\|name\|unit\|todo\|active\|done\|closed\|last_change\|target`, `-` for descending · `cols=` |
| `#/worklet/tasks` | **Worklet › Tasks**: All projects' Tasks lens: every task in the company, in one list | `view=` (a saved view) · `shape=list\|board\|calendar\|timeline\|table` · `cols.list=` / `cols.table=` + the filter grammar |
| `#/worklet/views` · `#/worklet/views/{id}` | **Saved views**: the inventory, and one view run | |
| `#/worklet/history` | **History**: All projects' History lens: Worklet's own log, on the log frame. One project's changes are that project's History lens | `window=1d\|7d\|30d\|90d\|<from>/<to>` · `kind=` · `actor=` |
| `#/worklet/search` | **Search**: the company's work ranked against a phrase | `q=` `mode=hybrid\|keyword\|semantic` |
| `#/worklet/{KEY}` | **Project**: one project as Worklet's scope: its Tasks lens, About, History and Files | `lens=about\|history\|files` (Tasks, the first, takes no key) · the same view strip and filter grammar, scoped to the project |
| `#/worklet/{KEY}-{n}` · `#/worklet/{id}` | **Task**: description, checklists, sub-tasks, activity (changes, comments, agent turns), properties and cost | `activity=all\|comments\|turns\|changes` · `list=` |
| `#/company` · `#/company/principles` | **Company › Principles** (the sidebar's **Principles** row, and the Company landing): the mission and the vision as two equal cards side by side, then the standing policies every agent reads on every turn, numbered in one card across the page, each card under its own glyph (a target, a telescope, a scroll), then Settings › Limits as a related row to a reader who may open it. An unwritten mission, vision or policy list each says so inside its card. Each card carries its own Edit, and each unwritten part its Add, for a reader who may write the charter, and nothing for anybody else; either opens the part for writing in place in its card (`?edit=mission`, `vision` or `policies`, a comma list for several). Read from the org projection by every reader | |
| `#/company/brand` | **Brand**: how the company looks and sounds in every doc, deck and page its agents make: the published (adopted) version of the company's brand kit, as four cards (Logos, Colours, Typography, Voice and tone) beside a sticky preview of a sample doc or slide in light or dark. Every reader reads it, downloads a logo and copies a colour *(changing a card, publishing a newer version of the kit and stopping the brand are an Admin's, a person's, with a recent sign-in)* | |
| `#/company/brand/setup` | **Brand › Set up**: the company's brand set up in six steps (Identity, Logos, Colours, Typography, Voice, Review) on a page of its own, never a dialog: a vertical step rail, the step's fields in one column under a lead sentence saying why the part matters, and the Brand page's preview beside them labelled "Preview of your draft" (under the content at 1024; a Preview disclosure and a horizontal step row on a phone). Every Continue saves the step into a draft version of the company's kit (a keyed publish, never an adoption; the first makes the kit, shared with the company to view), and opening the page again resumes at the first step not done. Colours carries the contrast panel: the pair that refuses an adoption (text on the light ground) in danger tone, the advisory pairs in warning tone with their reason, and one-click shades that keep the hue. Review lists a summary card per step with Edit, the full preview and an acknowledgement for each contrast warning; an Admin, a person, finishes with **Publish as the company brand**, and a person the kit is shared with to edit with **Save for review**. Entered from the Brand page's empty state, the Make row chip an Admin sees with no brand, the notice over an unpublished draft (**Finish draft**, or **Edit draft** once it is ready) and the Brand page's **Run setup again** | |
| `#/company/org` | **Org chart**, the hierarchy every seat works inside; every human seat with its kind; what holds each one for an admin and for the holder themselves; and, for an admin, where a person is invited or created onto the vacant human seat they will hold | `unit=` · `seat=` |
| `#/company/org/roster` | **Org chart › Roster**, every seat, and who is carrying how much | `view=seats\|workload` · `group=state\|unit\|flat` · `q=` |
| `#/company/org/teams` · `#/company/org/teams/{unit}` | **Org chart › Teams**, every unit with what it is for and its goals; one unit's page | |
| `#/company/org/edit` | **Org chart › Edit org**: the builder, opened from the chart's button, where the units and seats and the company's name are written; the company's node carries no Edit, and its principles are the toolbar's Edit principles, which opens Company › Principles, the one place its mission, vision and standing policies are written *(saves as an admin, or as a unit's lead inside the units they lead)* | `view=visualization\|table` · `chart=structure\|reporting` · `unit=` · `seat=` (the selection; arriving with one opens its editor) · `add=unit\|agent\|human` (opens the Add once, then leaves the address) · `project=` (with `unit=` or `seat=`: Worklet's New project, which opens that node's editor on its Project field holding the key, then leaves the address) |
| `#/company/org/seats/{handle}` | **Seat**, an agent's or a person's profile; a person's says what holds the seat, its holder, the open invitation that names it (with **Cancel invitation**), or nobody, with **Invite** and **Add person**, the gestures drawn for an admin. Handles live only under `seats/` | agent: `tab=overview\|work\|turns\|memory\|schedules\|settings` · `conversation=` (Memory); human: overview · work · settings |
| `#/company/schedules` · `#/company/schedules/{scope_type}/{scope_id}/{name}` | **Schedules**, recurring work, its figures drawn once there is a schedule to count, and with none an empty state offering the configuration a schedule is written in to a reader who may open it; one schedule | |
| `#/company/integrations` · `#/company/integrations/{kind}` | **Integrations**: a directory of every integration by category (Chat, Work tracking, Docs and knowledge, Code, Observability), Crewlet's own service first in each, Atlassian listed as Jira and Confluence; beside it the page of the line chosen (an app's Overview, Agents, Activity and Settings, its settings form opening in place; a service's counts, agents, activity and settings, or what does its work instead), and with nothing chosen what is in use, as compact cards, with what needs a look, the recent activity and the address apps deliver to. `{kind}` is an app's key, a surface (`atlassian` and `forge` select Jira) or a service (`worklet`, `doclet`, `chat`). Every reader's; an app's state and connecting are an Admin's | |
| `#/company/budgets` | **Budgets**, the company's and every agent seat's day, week and month: spent, the ceiling (raised in place by an admin whose credential the document admits), and what is refusing. Last in Company: what the company decides, while what it spent is Settings › Token spend | `window=` is not read here |
| `#/chatlet` | **Chatlet**: the company's own rooms and conversations, under a top bar (Menu, the Rooms and Messages switchers, and the open room's actions), with the Rooms panel slid in from Menu or pinned beside the conversation *(the whole of Chatlet needs Chatlet access)*. Every chat page is read as the seat your session holds: a credential acting as no seat (a deployment's own token, or a service account bound to nobody) reads nothing here, not even a public room | `dm=` (a seat's handle, from its page or peek: replaced at once by `#/chatlet/new?to={handle}`) · `pane=` (on every Chatlet address: see the room) |
| `#/chatlet/activity` | **Chatlet › Activity**: every message that named you, every reply in a thread you follow and every reaction to what you said, across every room. The whole away story: the engine sends no email, no push and no SMS | |
| `#/chatlet/threads` | **Chatlet › Threads**: every thread you follow, newest reply first | |
| `#/chatlet/drafts` | **Chatlet › Drafts and sent**: what you have not sent yet, in every room and thread, and what you sent | |
| `#/chatlet/saved` | **Chatlet › Saved**: the messages you saved for later | |
| `#/chatlet/search` | **Chatlet › Search chat**: ranked over every room you may READ, which is wider than your own rooms | `q=` · `in=` (narrowed to one room) |
| `#/chatlet/browse` | **Chatlet › Browse rooms**: every room you can read, to join or open | |
| `#/chatlet/new` | **Chatlet › New message**: a conversation with the people or agents you name, the one you already share with them if there is one | `to=` (handles, comma-separated) |
| `#/chatlet/{id}` | **Room**: the transcript, who is here, and the composer | `message=` (the message a link landed on, flashed, then left) · `pane=thread:{room}:{root}` · `pane=details:{room}:about\|members\|shared\|notifications` · `pane=profile:{handle}` · `pane=catchup:{room}` · `pane=summarize:{room}` (ONE pane at a time, beside the conversation or over it; any place may carry one) |
| `#/live` | **Live › Now running** — the running turns, the coding runs waiting on a person and the rest in a box, the activity strip and the recent phases | `window=15m\|1h\|6h` (the activity strip) · `seat=` (a handle) · `phase=` · `failed=true` (the same spelling Turns uses) |
| `#/live/turns` · `#/live/turns/{id}` | **Turns** — the turns that ended over the window, counted by the engine, then every turn one row each; one turn *(needs Admin access)* | `window=1h\|6h\|1d\|7d\|30d\|<from>/<to>` · `seat=` (a handle) · `failed=true\|false` · `sort=-started\|-tokens` (the engine's order) |
| `#/live/runs` · `#/live/runs/{turn_id}` | **Coding runs** — live and durable; a run's own page draws the run alone, answered in place, and a collected run's page reads its turn | |
| `#/live/a2a` · `#/live/a2a/{id}` | **Agent-to-agent** — a channel's own page draws the channel and what crossed it *(needs Admin access)* | |
| `#/live/traces/{id}` | **Trace** — one distributed trace. NO LIST: nothing enumerates traces, so a bare traces address is Not Found, saying a trace is opened from a turn, a run or an event | |
| `#/live/events` · `#/live/events/{id}` | **Event log** — the time axis, then the rows *(needs Admin access)* | `window=1h\|6h\|1d\|7d\|30d\|<from>/<to>` · `category=` · `actor=` · `seat=` · `trace=` · `channel=` · `q=` · `failed=true` |
| `#/doclet` | **Doclet**: with no phrase, the landing (the newest change open to read, Recently written, Continue reading, Pinned, Watching, For the agents); with one, the ranked answer grouped by space beside a preview. The spaces row and the search sit on every Doclet screen *(the whole of Doclet needs Doclet access)* | `q=` · `mode=hybrid\|keyword\|semantic` (Hybrid, Keyword, Meaning) · `by=space\|rank` · `space=` (narrows what is shown of the answer) |
| `#/doclet/{CONTAINER}` | **Space** (a knowledge container, by its key): its head, and its pages as an outline read one level at a time, with Create (a document or a folder) at the end of every open level | |
| `#/doclet/new` | **New page**: the composer, filed where it was opened from | `space=` · `parent=` (a page id) |
| `#/doclet/pages/{id}` | **Page**: addressed by its id, which a rename does not change. The document with its outline, its properties, its comments and what it is connected to; edited as you | `edit=1` · `version=` (History: one revision in the document's place) · `lens=diff\|full` |
| `#/doclet/skills` | **Agent skills**: the tool skills the engine offers a phase, who loaded each; and what one agent learned | `kind=pages\|learned` · `sort=title\|-updated` (tool skills: the engine's order) · `seat=` (learned) |
| `#/doclet/diaries` · `#/doclet/diaries/{handle}` | **Agent diaries**: every agent's diary at a glance, each counted by the node holding the agent; one agent's diary and episodes *(needs Admin access)*. Handles live here as they do under `seats/` | |
| `#/artifacts` · `#/artifacts/yours` · `#/artifacts/shared` | **Artifacts**: All (everything the reader can open, the latest activity first, their pins above as a Pinned group), Yours (what they own, with their trash behind the Trash toggle) and Shared with you, each the engine's `artifacts` listing under one scope, every row decided by `get_artifact`. An earlier gallery's `#/artifacts/mine` is rewritten in place to Yours, and `#/artifacts/company` and `#/artifacts/agents` to All. All and Yours open on **Start a new artifact**, a quiet tray of the kinds (`ArtifactMakeRow`): each kind's glyph on its own tint, its name and what it is for, on the dotted field the org chart's canvas uses, one line on a wide page (the words under each name dropped on a narrower one) and three across in two rows on a phone, each opening New on that kind; it is drawn for a reader who may start one, and steps aside for a search, a type and the trash. The artifacts are a **grid of cards** by default: each card's preview is a snapshot of the artifact's own content, drawn small by the renderer its page uses (a doc's opening, a deck's cover slide, a sheet's first rows, a board's widgets from its top with their figures, filling the card within six dataset reads, a design's cover board filling the card edge to edge (a phone's board, which that would cut to its top, shown whole) and a page, each framed, a page that reads its datastore answered its reads by a read-only preview host that refuses every write, names no viewer and joins no room, and a page that keeps each viewer's own state handed a newcomer's, empty and kept by that host alone, so it draws as a first visit does), rendered once when the card comes within a screen of the scroller's view, no more than six rendering at once, and then kept as a still copy with nothing live behind it, so scrolling past a card again reads and draws nothing; a card mounted again on the same visit (a filter, the shape, a page of results) shows its copy at once, a new version is drawn afresh, and a page loaded afresh takes new snapshots; a page or a design's board keeps its frame instead, since a frame cannot be copied, at most twelve kept, and its datastore reads are each answered once; the kind's drawing stands in before a card has drawn, and every read is a preview that records no open; under it the title, who can open it (a lock, people or the company) and when it was edited, and a More menu (pin, copy link, rename, Move to… a folder, move to the trash; restore in the trash). The list is the second shape, and which one a browser shows is remembered there: each row the kind's glyph on its tint beside the title, with the kind's word and the description under it (`ArtifactNameCell`, so there is no Type column), who can open it as a glyph and a word (`ArtifactAccessLabel`: Private, Shared or Company; not in the trash), the owner's badge and name, the folder it is kept in as the reader sees it (its path, or Top level; not in the trash), when it was edited and the reader's role, a row's ground moving under the pointer and its mark's hairline taking the kind's hue. The page bar holds the search and New; the toolbar holds the type, the order, the trash (Yours), **Select** and the shape. Select (or `x`) puts a box on every card and row, a card's body then picks rather than opens, and a `SelectionBar` under the toolbar says how many are picked with **Move to…** (`m`), held with its reason only for a reader without the Artifacts access and otherwise sending every pick for the engine to decide one by one (a folder's manager may take what they can only read to the top level), the move's toast naming each that did not move and why, and Clear; the picks belong to one question and clear when it changes, and the trash offers none. The reader's own artifacts are owned by "You", and Yours draws no owner or role column; the trash's rows say when each was trashed and when it is deleted for good. No permission opens the workspace: a share alone admits a reader (D01), and the engine decides every artifact. | `kind=html\|doc\|slides\|design\|dashboard\|sheet` · `q=` · `sort=updated\|created\|title\|opened` (all three kept across the sections) · `status=trashed` (Yours: the reader's own trash) |
| `#/artifacts/folders` · `#/artifacts/folders/{id}` | **Folders**, a fourth tab beside the three flat questions: where things are kept rather than who made them. A folder is a place and never a permission (it grants nothing on what it holds, and moving an artifact changes nobody's access), so every reader browses the folders they can see. The section is the top level: its folders as tiles (`FolderTile`, the folder glyph on a neutral tint, its name, who sees it on a top-level tile, Everyone or Only you, and what directly inside it this reader can open, "12 artifacts · 2 folders" or "Nothing you can open yet", never "Empty"; four across, three on a narrower page, a full-width 44px row on a phone), then **Not in a folder**, everything the reader can open filed in no folder they can see. A folder's page, addressed by the id the engine minted so a rename or a move breaks no link, carries the trail Artifacts › Folders › every folder above it, a header (the open folder's glyph, its name, who sees it, and a More menu: Copy link for anyone, and Rename, Move to… and Remove for its owner, the owner's lead or an administrator), its subfolders A to Z, then its artifacts in the chosen order, each the engine's `artifacts` listing for the folder (`folder=<id>&delimiter=/`, S3's one level); a phrase searches the place's whole subtree instead, offering the folders beneath it whose names match and naming each artifact's folder. The toolbar adds whose they are (Anyone's, Yours, Shared with you) to the gallery's; New in a folder offers **Folder** above the kinds, and a kind's page opens filed there. **New folder** (`n`) asks for a name and, at the top level only (a person's), who can see it: Everyone at the company (who may file into it) or Only you; a subfolder is seen by whoever sees its top-level folder. **Move to…** is one dialog for an artifact, a selection and a folder: every place the reader may file into as its path in words, Top level first, the current one marked, in a list always on screen under its field at one height (so nothing moves under the pointer when the field loses focus), found by typing any part of the path, ending in Create “name” in the folder being browsed (or the one a typed path names; at the top level it first asks who sees it), and on a phone takes the whole window as a full-height sheet at every step; it says "Moving never changes who can open it", each artifact of a selection is decided alone, and a toast names what did not move and why and offers Undo. Moving a folder where more people see it asks first. **Remove** moves everything in the folder up to the folder above, deletes nothing and is said so. An empty folder says so with New and New folder to a reader who can file into it, and "Nothing here is shared with you" to one who cannot; a folder the reader cannot see is "Folder not found", as one that does not exist. `u` goes up a folder | `kind=` · `q=` · `sort=` (kept from the gallery) · `scope=mine\|shared` (whose, on these pages only) |
| `#/artifacts/new/doc` · `#/artifacts/new/slides` · `#/artifacts/new/sheet` · `#/artifacts/new/design` · `#/artifacts/new/dashboard` · `#/artifacts/new/html` | **New doc**, **New slide deck**, **New sheet**, **New design**, **New dashboard**, **New HTML page**: each kind's own create page, in one shared frame (the artifact page's slim bar, then a full-bleed body the kind draws), each a chunk of its own. A doc or a deck starts from the engine's starter and a sheet blank or from the Review sheet template, owned by the reader *(needs Artifacts access)*. A design starts as the canvas a person lays out: a starting layout (landing page, app screens, one screen at three sizes, a presentation or a blank canvas), then boards shaped on the canvas (Frame `F` places one; Text and Sticky place notes) with a panel that follows the selection (a board's name, frame preset or size, order and brief) and the hand-off panel beside it (the design's name, brief and references and who builds it); Create and ask publishes the boards as placeholder pages, shares the design with the chosen agent as an editor, files the agent a task carrying the brief and every board, links the design to it, and opens the design, whose boards fill in as the agent publishes. Every page offers **Ask an agent** in its bar, which opens the hand-off panel beside the page rather than replacing it ([Asking an agent from a create page is a panel, not a mode](#asking-an-agent-from-a-create-page-is-a-panel-not-a-mode)); a design and an HTML page arrive with it open where it fits beside the page. The gallery's Make row and its New menu open them. The bar's first control is **Save in**, the folder the new artifact is filed in (the folder page New was pressed on), which opens Move to… to choose another and writes nothing until the artifact is made; the way back leads to that folder, and a task asking an agent names it by its id alone (a task is read by people a private folder is not shown to, so it never carries the path) | `item=` (the task it is made for, which lists it on that task's Artifacts card) · `folder=` (the folder it is filed in, by id) · `panel=ask\|closed` (the hand-off panel; `closed` on a design or an HTML page, which open with it) |
| `#/artifacts/new` | Not a page: an earlier link, rewritten in place to its kind's page when it carries `kind=`, and to the gallery when it does not | `kind=` (read once, then dropped) |
| `#/artifacts/{id}` | **Artifact**: addressed by its id, drawn under ONE slim bar in the frame's place ([An open artifact is the screen](#an-open-artifact-is-the-screen)). A page runs in a sandboxed frame on the artifact origin, edge to edge, a doc is rendered here tab by tab by the dashboard's own Markdown renderer with its outline beside it, Edit (the text in place, in the rich editor, and the tabs: add, rename, reorder, remove; each save a version, a raced save merged onto the newer one) and Print, a deck is drawn from its canonical tree with Edit, Grid and Present in the bar, a sheet is drawn from its schema. The bar holds the way back (to the folder it is kept in, named, where the reader can see it), the title (renamed in place by an editor), the version chip, the kind's views, who is here, and the controls that open a panel at the side: Details (what the page header and its fact line used to say, and the Folder it is kept in, a link where the reader can see it, with Move…), History (versions, Compare, Restore, labels and the change record), Data where it keeps any (rows, the schema editor and every write, with Undo), People (who is here, with Follow) and, from More, Settings (details, what it can do, its owner, the trash); Share and More close the bar. A reader with no relation is told it is not there or not theirs, in one card | `panel=details\|history\|data\|people\|settings` (pushes; the old `tab=` and `rail=people` are read as the panel they became and rewritten) · `expand=1` (the panel takes the workspace) · `version=` · `against=` · `lens=` · `sheet=` (which collection a sheet draws) · `doctab=` (which tab of a doc is on show) · `data=rows\|schema\|history` · `mode=grid\|present\|presenter` · `slide=` · `follow=` |
| `#/settings/models` · `#/settings/models/{id}` | **Settings › Models**, the column's landing and the Settings row's address, open to every member: every model with its condition, how it signs in and what it is used for (`llm_entries`), the model alarms and, as the *By job* lens (`?view=jobs`), every seat's model for each category of work; one model's page with its sign-in, sit-out, usage and uses. Every change is an admin's: the wizard (`?flow=add`, or `?flow=edit&step=…` on a model; `&add=` lists the uses it opens with added, each `category:handle` or `assistant`), and the test, retry, device sign-in and removal flows (`?flow=`). `{id}` is the model's key, the name a seat's `llm:` writes | |
| `#/settings/mcp-tools` · `#/settings/mcp-tools/{tool}` · `#/settings/mcp-tools/servers/{name}` | **MCP Tools**, the engine's runtime beside the models, every MCP server with what each node did with it, and every tool a seat can call, by origin *(needs Admin access, like every company setting: it is where MCP servers are added and their templates read; which tools a seat has is on that seat's page, from the overview's `tools` push)*. ONE tail segment is a tool and two are an origin filter, discriminated on LENGTH, because a tool name is a third party's string | `q=` · `origin=` · `add=server` (the add form) |
| `#/settings/secrets` · `#/settings/secrets/{name}` | **Secrets** — names and provenance, never values *(needs Admin access)* | |
| `#/settings/limits` | **Limits**, the caps the engine holds every turn to, as it enforces them: the turn's guardrails (review rounds, tool rounds on a first turn, delegation depth), delegation, notifications and a scheduled turn's timeout, one read-only settings table a group, each cap with its key in the company document, its value and whether it is the engine's default or changed from it *(needs Admin access)*. Read-only | |
| `#/settings/spend` | **Settings › Token spend**: the window's tokens against the one before, the monthly budget, the prompt-cache share and the median task; daily tokens by phase (or model, provider, seat, unit, worker); then by agent beside by provider and by team, and the three most expensive tasks beside the auxiliary model by purpose (tokens only) | `window=7d\|30d\|90d\|<from>/<to>` (whole company days, kept across Settings' sections) · `group=phase\|model\|provider\|seat\|unit\|worker` |
| `#/settings/spend/tasks` | **Settings › Token spend › Expensive tasks**, the tasks last changed inside the window, most tokens first, each with what drove it: turns, workers, reopens and send-backs *(needs Worklet access; not offered where the company does not run Worklet)* | `window=7d\|30d\|90d\|<from>/<to>` (kept across the sections) |
| `#/settings/people` | **People**, the identity directory but its service accounts: every person with their stage, login, access and the seat they are bound to, their credentials and sessions, the invitations nobody has redeemed (`/iam/invitations`), the directory's report on them (`/iam/check`), and every human seat with whatever holds it, a person, a service account or an open invitation (`/iam/seats`) *(needs Admin access)*. An admin changes, suspends, resets, signs out and removes people, and cancels invitations. It invites and creates nobody: a person comes from the vacant human seat they will hold, on the org chart. An invitation's link and a reset link are shown once and never read back. No tail: a row opens in the peek (`peek=person:{id}`, by the directory's id), which is the only page a directory row has | `peek=person:{id}` (the opened person, closed when a removal made from it lands; an id the directory does not hold is said in the peek) |
| `#/settings/service-accounts` | **Service accounts**, the machine principals: each with its stage, login, access and the seat it acts as, and the directory's report on them; an admin creates one (on a human seat or none) and mints its token, shown once *(needs Admin access)*. A row opens in the peek, as on People | `peek=person:{id}` |
| `#/settings/tokens` | **API tokens**, this node's Tier A token labels, each joined to the directory row its login names and the seat that row binds (`/iam/node-tokens`), and, where the company document is managed, the tokens that alone may write it *(needs Admin access)*. Never a value. A person's own tokens are on their Account | |
| `#/settings/sign-in` | **Sign-in policy**, how people sign in, as the engine serves it: the shortest password, whether two-step verification is required, how long a session lives (from sign-in and unused) and how long a step-up lasts, read-only and naming the node that answered *(needs Admin access)* | |
| `#/settings/audit` | **Audit log**: every write a person or a token made, every call they made at runtime, the identity estate's own trail, every artifact's history (an administrator's read of somebody's private artifact said in words), and every configuration revision labelled with the kind of writer it recorded (`agent`, `human`, `operator` or `system`) *(needs Admin access)*. No detail route | `window=` · `actor=` · `kind=worklet\|doclet\|config\|credentials\|runtime\|identity\|artifacts` |
| `#/settings/nodes` · `#/settings/nodes/{node}` | **Nodes**, leases (each seat's holder and **since** when), duties, config rollout, and the fleet broker's members as the nodes advertise them and as its metadata group counts them *(needs Admin access)* | |
| `#/settings/config` · `#/settings/config/revisions` · `#/settings/config/revisions/{id}` | **Configuration** *(needs Admin access)* — `revisions` lands on the History lens; one revision's page draws no lenses | `lens=active\|entities\|audit\|diff` |
| `#/settings/backups` · `#/settings/backups/{domain}` | **Backups & retention** — take a backup, what the fleet has backed up and the backup history, each state-log domain and what holds its trim; one domain *(needs Admin access)*. Domains live only under `backups/` | `at=alarms` (brings the alarm list into view) |
| `#/settings/storage` | **File storage**, where the company's files are kept (the object store's backend, as a settings table row with its key), its collector's last passes from the fleet's own record, and the company's storage against the plan's quotas (`artifacts.limits`: artifact files, artifacts and datastores, each a figure over its ceiling with a bar, counted over every artifact, private ones and the trash included) *(needs Admin access)* | |
| `#/account` | **Account**, the signed-in reader's own page, opened from the reader's own name in the user block at the sidebar's foot, from its menu (it has no sidebar row) or `g u`: the profile the directory holds and the reader's access, "Your access" (an administrator changes both), the password (changed with the current one, which ends every other session and personal token and keeps this browser), the second factor and recovery codes, where they are signed in (this browser marked, any other signed out by name, or everywhere), and their personal access tokens (minted, the value shown once, and revoked). A credential that is not a person's, a Tier A token's session whether or not it is bound to a seat, is told what it is and offered none of it, and asks the directory nothing; the engine answers a seat-bound token's session with a person's `kind`, so the page reads the login's grammar (`ownsAccount` in `lib/login.ts`) | |
| `#/login` | **Sign in** *(outside the frame)* — a login or address and a password, the second factor when the engine asks for it, or an API token exchanged for a one-hour session | `next=` — where to go once signed in: a hash route of this dashboard, and anything else lands on Home; `login=` fills the login, and `after=reset` says the password was just set — what a reset link's own **Sign in** sends. Neither is a credential |
| `#/invite/{id}.{secret}` | **Invitation** *(outside the frame)* — who it is for, who sent it (a person, never a machine: an invitation a Tier A token or a service account issued names no sender) and the seat it binds — or, where the chart no longer holds that seat, that redeeming it will be refused — then a login, a name and a password, typed twice. The link the engine mints lands here | |
| `#/reset/{id}.{secret}` | **Password link** *(outside the frame)* — whose password the link sets, then the password, typed twice, once: a RESET, which ends every session the person held, or the FIRST password of a person an administrator created on a seat, which ends nothing. Neither signs anybody in, so it ends on the sign-in form. The reset link an administrator issues, and the first password link a person's create hands back, land here | |
| `#/enrol` | **Second factor** *(outside the frame)* — the authenticator a deployment that requires one asks for before anything else opens, then the first recovery codes | `next=`, as the sign-in's |

**There is no redirect table.** There was one, and it was always a liability: a
redirect whose old path is now a live route sends every reader of that route
somewhere else, permanently, with the address bar agreeing with them — which is
strictly worse than the dead link it exists to avoid, because a dead link is
visible. It happened once, to an address that had meant a coding run before a
workspace took it over. No `v*` tag
has ever shipped a route from this tree, so there is nobody holding an old link:
an address from before the one-sidebar rebuild (one under the retired
`activity`, `cost` or `admin` heads), or from before Company and Settings were
split (one under the retired `agents` and `spend` heads, or a Settings page
that moved to Company or was renamed), is Not Found, which names what was asked for, says
where the product knows why there is nothing there, and offers the way home.

**An object's segment is decided by its shape, never by a lookup**, so every
address resolves before any answer arrives. A project or container key is
uppercase, a task is `KEY-n` or its uuid, and a page is its uuid, so a word in
one of those positions (`people` after `worklet`, `runbook` after `doclet/pages`)
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

### An item's address

**An item is opened by its KEY, unless another task claimed that key first.**
Two tasks can hold one key — a key counter restored beside work minted after it
hands out numbers that work already holds — and the engine opens the key on the
task that claimed it, on every node, so every link and chat message written
against that task keeps reaching it. The other one is flagged `key_collision`,
and its key opens its neighbour: it is reached by its **id**, which the item
route takes as readily as a key. So every row the engine lists a task in says
which it is, beside the key it qualifies — `key_collision` on a board row, a
search hit, an item and its links, and a page's backlinks;
`subject_key_collision` on a feed record and an inbox notice;
`task_key_collision` on a checklist item. A notice is addressed
by its `task` rather than its subject, because a `prioritised` notice's subject
is the person whose list a lead reordered.

`itemAddress` in `lib/work.ts` is the one place the rule is written — the key,
unless flagged or empty, and then the id — and every link, peek, `[`/`]` step,
"this is the open row" check and copied tool call goes through it, with
`itemPath` for the route and an adapter per row shape (`subjectItem`,
`noticeItem`, `checklistTask`, `linkedItem`, `detailItem`) so no screen pairs
an id with the wrong flag. A project's route is `projectPath` for the same reason: no route
into Worklet is spelled at the call site. `routes/worklet/collision.test.tsx`
clicks each of a pair in every shape the work list draws and reads where it
went, because every shape built its link, its peek, its stepper and its "this
is the open one" check out of the key, and each opened the claimant from both
of a pair. The key is still what a row SHOWS — the address decides
where it goes, never what it is called.

**A write names the task by its address too**, because the engine resolves a
key for a change exactly as it does for a read: a duplicate's page that sent
its key changed the claimant — its ticks, its watch, its comments and asks,
its hand-off — and refused a field edit against the claimant's version. So the
task page and its peek hold the address once (`ItemEditsProvider`'s `item`,
the composer's, the hand-off's) and pass the key beside it only as the word a
sentence uses; its turns, like its history and its comments, are asked for by
the id. The grid's inline edits and the note naming who won a lost race, a
link chosen in the composer (by the hit's id, linked by its address) and the
trash's **Restore** follow the same rule. `routes/worklet/WorkItem.test.tsx`
opens a duplicate's page and reads what every write named, and
`collision.test.tsx` restores each of a pair from the trash.

**A turn's task is asked for by its id.** A turn's work item
(`work_item{backend, id, key, project}`) is what the turn was charged to, not a
tracker row, so it carries no `key_collision` — and a turn on the task that did
NOT claim its key, asked for by the key, was titled, numbered and opened as its
neighbour. So a Worklet task is asked for and opened by its id (`turnItem` in
`lib/work.ts`), an answer about it is matched to the question by either name
(`answersAddress`), and once an answer has carried the flag the task is opened
through `itemAddress`; a vendor's item, whose id is the vendor's, keeps its
key. The seat profile's **Current turn** card, the seat peek's "Turn N", the
turn page's ordinal and its **Open task**, and an Inbox decision about a
parked run or a stopped seat all follow it.

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

`DataGrid` draws all of them: `@crewlethq/ui`'s `DataGrid`, which the
dashboard binds to the address (the sort and the column set), the peek (the
row it is open on) and the keymap (which grid `j` and `k` drive). There is no
`<table>` left in the product except
a project's Files listing: a block inside a card that already titles it, with
nothing to sort, which is a table in the sense the element means rather than a
list of objects. (The retention screen's per-domain terms were the other one;
they are a description list now.)

**Including Worklet's own list and table, which are one grid with two
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
then whom it woke. People is a column of the sheet beside a person's
peek, and every flexible column it keeps has a floor (Seat and Access `8rem`):
at zero the seat drew its avatar alone and the access stacked a row tall
while Kind and Login stayed, so it gives up Access, then Kind, then Login,
and keeps the name, the stage and the seat. Access in ANY cell (the People
list, the invitations, a person's or the Account's tokens) is the one line the
dashboard says it in (`AccessTags` over `accessLine`, `lib/access.ts`): one
**Admin** tag for anybody holding it, never Admin beside the apps it includes;
otherwise a tag per app, or **Overview only** — at most two tags and any raw
name this build does not know, so it no longer needs a "+n" tag: eleven grants
drawn whole once stood one row of the People list about 290px tall at 1280. The peek's own grids are a 385px
column: a credential's name and a session's state keep `8rem` floors, its
state pill and a session's start take their own width (`max-content`, short
by construction, where a fifth of the column cut "In use" to "In u…" and the
"Session" header to "SESS…"), and a credential gives up Expires, then Carries,
then Created. None of it happens on a
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

**Below 640px (`breakpoint.phone`) a row is a card, because 390px has no columns.** The audit's six
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
  dashed placeholder the board card already stopped drawing. The kit's
  `.crewlet-data-grid__cell:empty`
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
  card empty underneath it. What bounds the value instead is the engine: a
  title is at most `worklet.MaxTitle`.
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
above it is a word. Measured on a Doclet page's header: `v1` under a 146px
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

**A trail of one segment is not a trail.** The Doclet page draws its
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
  is carried by the name, the mark and the position. **An agent the chart holds
  is drawn as itself instead**: its own Crewlet character on a plate of its own
  colour, the look its author chose (the one rule's third exception), where its
  initials would be. A person, and a handle the chart does not hold, keep the
  initials. Beyond that its only variant is
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
  agent. That is what a credential bound to no seat needs — an API token's
  session, a service account acting as itself: its write is authored under
  its login, which no chart lists, and drawn from the chart alone the founder
  who filed a task through the deployment's token was a squircle. A handle with neither
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
Worklet's compact `Assignee`, the Projects directory's Lead and Last change,
the trash's `removed_by`, the log's actor, a schedule's scope and every fire it
woke, a saved view's owner, a page's watchers and commenters and a delivery's
recipient — and the rule is written here rather than at any one of them because
the whole point is that the same person looks the same on every screen they
appear on. `components/work.test.tsx` and `routes/worklet/shapes/Grid.test.tsx`
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
plan, its record. It draws with `@crewlethq/ui`'s `PropertyList` (each section
a description list on one subgrid, so every label shares one term column, read
from the description list's own measure), and translates the engine's routes,
turns by id and change log instants into it; the rules below are the kit's and
its suite holds them. Two components did this before it: a flat `KeyValue` with no sections
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
clock in the browser's zone, because five repetitions of today's date under
a chart of minutes is the noise the compact form exists to remove. They are
**ink**, tabular and at the caption size: the chart's one accent is spent on the
bars.

**What the bars are a count OF is a required prop, not a caption.** `over` is
`window` where the engine counted them through the listing's own predicate and
`loaded` where the browser bucketed the pages it is holding, and the noun is the
caller's too — so the one sentence a screen reader hears says "9,412 events in
this window" on the event log and "41 changes on the pages loaded" on
Worklet's. Optional, it was wrong on the second caller the day that caller
arrived: a Worklet chart announced a page count as a window count, in a noun
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
(a scope control, whose other scopes are rows the loaded page does not hold)
and qualified figures the reader could not see. A
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
middle-click and the rail's `Open ↗` go to the page. A kind with NO page of
its own, a person, whose directory row is read beside People and
nowhere else (`KindSpec.peekIsPage` in `app/frame/objects.ts`) — has that list
WITH ITS PEEK OPEN as its address, so a new tab opens on who it was about
rather than on the bare list, and the rail draws no `Open` over that list,
which could only close it. Inside a peek `[` and `]`
step through the list it was opened from and `esc` closes it. Every list a
plain click peeks from draws the row the rail holds as the open one — the work
list in every shape, the search, and an item's own subtasks — matched on the
same reference the peek was opened with, so the mark and the rail cannot name
two different rows. Opening the rail
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
also what lets a pasted `peek=` open on arrival, where no row exists. For a
work item the id is its ADDRESS — see [An item's address](#an-items-address) —
so the rail, the row it was opened from and the row drawn as open all name
the same task when two of them share a key.

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
`Ctrl+\` elsewhere), the chord that above the breakpoint, where the sidebar is
always on screen, folds it to its compact rail and back
([The sidebar](#the-sidebar)); the skip link precedes everything and lands on
the scroller. None of that is written in this repository, because a second
copy of it is how the two drift. What `dashboard/src/app/` owns:

| Piece | What it is |
|---|---|
| `sidebar/` | the one sidebar ([The sidebar](#the-sidebar)), with the engine panel and the user block |
| `sidebar/UserBlock` | who is signed in, at the sidebar's foot, called by their OWN name and never their seat's (`readerName`: their directory row's name as `GET /auth/session` answers it, else their login): bound (that name, the login where it is not the name, and the seat — its name off the chart, or `@handle` where the chart is unread — the seat's kind never captioned, since every binding is to a human seat), a credential acting as a seat (a Tier A token or a service account bound to a human seat: the seat's name, "via API token" under it with the token's name in the row's title and under the access in the menu, and the seat's initials with a key on the corner), unbound (the same name and login, and *No seat* — a Tier A token's session or a service account acting as itself, which is ordinary; never a person, who holds a human seat for as long as they are here ([ADR-0032](../../adr/0032-every-person-holds-a-human-seat.md))) or nobody (a **Sign in** button back to the screen they are on); a session read that FAILED says it could not tell who this is and still offers the Account and the sign-out. Pressing the name of anybody resolved opens the reader's own menu (the kit's `AccountRow` `menu`, named ", your menu", marked current on `#/account`, where it stands in for the row the Account does not have): their access as its header, then the reader's own [Account](#account-the-readers-own-page) (`place: "menu"` in `app/nav.ts`; never the seat, which holds none of what a person looks for there), where the password, the second factor and recovery codes, the sessions and the personal access tokens are, and both sign-outs, which need only `GET /auth/session` to answer — the frame's one read of it (`lib/frameSession.ts`), never one of the block's own: while the socket's `viewer` question has not (it never does for a person the socket refuses, or never dials for), a session is offered the two sign-outs under its own login. See [Signing in](#signing-in-is-a-screen-outside-the-frame) |
| `header/PageHeader` + `Breadcrumb` | the page header: the kit's top bar with the trail (whose last crumb is the page's `h1`), who is working now (Home's bar only, `useWorkingNow`), the star, and last the screen's own controls (portalled in by `PageActions`); then the workspace's SECTION TABS, as links in a labelled `nav` with `aria-current` on the section the reader is on, drawn only on a section's own page; then the `StateBar`. Settings draws its sections as a grouped COLUMN beside the screen instead (`SectionColumn`, the kit's `SidebarNav` inside the kit's `SectionFrame`, which lays the column and the screen side by side and folds the column into a section picker below 1212px), with a lock naming what a section closed to the reader lacks (`sectionOpen`; drawn only once somebody has said what the reader holds), each row's hint as its title, and nothing that leads out of Settings. A screen whose design keeps the star in a menu rather than in the bar gets no star from the frame (`useOwnStar`): a Doclet page, whose More carries it, and Doclet's landing, whose pages are each starred from their own More |
| `routes.ts` + `crumbs.ts` | the route table as a pure resolver, and the trail derived from it |
| `layout.ts` | the frame's breakpoints, READ from the kit's tokens (`breakpoint.shell`, `breakpoint.phone`), and the widths the dashboard derives itself: where the peek becomes a column, where Ask Crewlet does, where Settings' column folds, and where Chat's columns fold |
| `StateBar` | the answer's own honesty in one place: degradation, `read_level`, `complete: false`, how far this node has applied. The two positions that last one compares (`applied_through` against `log_seq`) are PACKED on the wire — (generation × 2^40) + sequence, so they compare with a plain `<` across a re-anchor — and are printed unpacked (`positionWords` in `components/work.tsx`): the bare sequence in a log's first generation and `generation:sequence` past it. The stride is the engine's `statelog.GenerationStride`, and a gate in internal/api holds the dashboard's copy (`GENERATION_STRIDE`) to it |
| `ObjectHeader` | an object's eyebrow, title (a level-two heading in every frame: on a page the last crumb is the `h1` and already names the object, and a peek sits inside a page that has one), state marks and up to six facts, in the same order wherever the object appears — though a peek whose body is a properties rail passes none, because a header and a rail stacked in one column are [one reading](#an-objects-own-facts). A fact may carry a `note` saying where its value came from, for the facts a reader can reasonably doubt, and only those. A fact that is one token (an id, a build) or a set of chips (a node's roles) takes two tracks, so it is never broken mid-token or stacked chip over chip. STATE lives here, never in the page bar: see [What a mark MEANS, and where a control belongs](#what-a-mark-means-and-where-a-control-belongs) |
| `OwnPageHeader` | a screen that draws its own head in the frame's place: the frame draws the slot it is portalled into, with the state bar under it, instead of the page bar. ONE screen does, an open artifact, for the reason [An open artifact is the screen](#an-open-artifact-is-the-screen) gives; `FrameNavToggle` is the frame's navigation toggle for that head to place first |
| `useTab` | which OBJECT tab is real. `tab=` is a string off a URL and the tab set belongs to the object — a human seat has three and an agent seat has six — so the hook resolves the parameter against the tabs this object HAS. It binds `1`–`9`; the strip itself is `@crewlethq/ui`'s `Tabs` |
| `peekAddress` | the peek's address: `peek=` in the URL, opened by a push and moved by a replace, and the page a row links to (`peekHref`); which clicks peek is the kit's (`peekClickHandler`, `peekRowActivation`) |
| `PeekHost` + `peeks.tsx` | the one peek in the product, mounted by the shell in the kit's `peek` slot and drawn by the kit's `PeekRail` (resizable from the keyboard as well as the pointer, a column of the sheet where the frame leaves the list beside it its floor, 1160 px on most screens and 1396 beside Settings' column, and a drawer under that): the body belongs to the KIND, so a list opens a peek by naming what it points at. `usePeekNeighbours` is how a list publishes the order `[` and `]` walk |
| `DataGrid` + `cells` | the kit's `DataGrid`, bound to the URL (its sort and columns), the peek and the keymap; bands from a grouped answer, typed cells; a row becomes a labelled card under 640 px, or two unlabelled lines on a `compact` grid |
| `PropertiesRail` | an object's own facts, in sections, with who set each: the kit's `PropertyList`, given routes and turns |
| `Histogram` + `FacetRail` | a log's time axis (the kit's `Histogram`, given the engine's buckets and the reader's zone), and one dimension of it as chips |
| `lib/rangePicker` | the one control for `window=`: a screen's range mapped onto the kit's `TimeRangePicker` (Today as the company's day, a custom window on the reader's zone or as whole company days) |
| `PageActions` + `PageNote` | a screen's own controls, portalled into the bar; its one sentence of explanation (the kit's `PageNote`), wrapped so its last line never holds a word alone |

**Two widths are the kit's, and the rest are arithmetic.** Below 1024 px (`breakpoint.shell`) the
sidebar is the drawer. Below 640 px (`breakpoint.phone`) a layout is single
pane: a `DataGrid`'s rows become labelled cards — or, on a grid that asks for
`phoneRows="compact"`, two unlabelled lines: what the row IS (its
`phoneLead` cells), then its marks. The work list takes the compact row,
because it is scanned by the dozen and its labelled card stood eight lines
and 230px per task; the table keeps the labels, since its question is a field
at a time. A column whose head is a glyph or nothing names itself on the card
with `label`, except a column of row actions, which says `action` instead:
its button is its own name, so the card draws none beside it — a `label`
there read `SIGN OUT  Sign out` on every session of the Account page. The
same `label` names such a column's head at every width, said to a screen
reader and shown on hover, so no column of marks is an unnamed column. The Settings column stacks above the screen beside it, and the
page bar's controls take a line of their own and scroll. Both are the kit's numbers, written as media-query literals that
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
| Settings' section column, on a screen that draws one | 236 |
| the list | 444 |

That is **1160 px** on a screen and **1396 px** beside Settings' column at the
normal density (1152 and 1388 compact, 1167 and 1403 comfortable). Below that the peek
is a drawer over the screen. The first cut of this counted only the sidebar,
the inset, the hairlines, the peek and the list, and put the threshold at
1112: the Worklet list beside a peek there was 396 px, and at 1280 a peek on
Settings › Nodes left the nodes grid 328 px with its columns cut off.

It is **the shell, not a media query**, that applies it, because two of those
terms are not the window's: the section column belongs to the screen, and the
inset and padding scale with the reader's density — a media query can see
neither. `app/layout.ts` (`peekColumnMin`) does the arithmetic, the shell asks
`matchMedia` for the one width right for the frame on screen, and it hands the
kit's `AppShell` one `peekShape`, which the kit publishes as `data-peek="column"`
or `"drawer"`, so there is no width with both or neither. And the column is the
reader's dragged width CAPPED at what the sheet can give with the list at its
floor (the rail's `reserve`, from `listReserve`), because a width dragged to 640
on a wide window outlives it.

**Beside a canvas the peek rests at 330.** A list beside a peek keeps its floor
and reflows; a chart is SHRUNK into whatever width the peek leaves it, so every
pixel the rail takes is one the whole company is drawn smaller in. The org
chart asks for the approved artboard's 330 (`usePeekWidth`, the way a screen
asks for the window's height with `useFillScreen`), and the rail rests there
until the reader drags it: a width they dragged is their preference on every
screen, anywhere from 330 to 640. The threshold above is computed at the
resting 420 for every screen.

**Chatlet's columns give way by the screen's width.** Chatlet draws its rail, a
room's transcript and an open thread side by side. They fold by the width of
the SCREEN (`@container page`), never the window's, because the sidebar, a
peek and Ask Crewlet's column each take a share of the window that a media
query cannot see. The transcript keeps a 320 px floor (`TRANSCRIPT_FLOOR` in
`app/layout.ts`, the width WCAG's Reflow criterion asks every page to read
at). So the rail narrows from 260 px towards 200 px first, and below a 539 px
screen it stacks above the room. A thread stands beside the transcript from a
978 px screen (the rail at 260, the floor, the thread's 360 and two gaps) and
under it below that, never in its place. The gaps are counted at the widest
density, so the floor holds at every density.

Beside Ask Crewlet's column at its resting width, a 1280 px window leaves
the screen 556 px at the normal density: the rail narrows to 220 px and an
open thread goes under the room. At 1168 px, where the column first fits, the
screen is 444 px and the rail stacks too. A column dragged wider leaves the
screen narrower still, and the columns fold sooner, because they ask the
screen and not the window.

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
`#/worklet/{KEY}?lens=history` the project's read and the log's read both wrote it,
whichever polled last won, and switching lenses unmounted the log and cleared the
slot to nothing over a page still showing the project it had read. A body states
its own rows' coverage INLINE instead, beside the rows, which is what a peek rail
and the Tasks lens already do. The two halves never both apply: a screen
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
| Anywhere | `Mod+K` search (the same chord closes it), `Mod+.` Ask Crewlet (pressed inside it, it closes it), `/` search this screen, `?` the legend, `g` then a letter a workspace, `g r` the running turn (Now running when several run), `g p` switch project (under Worklet's trail on its scope pages, centred everywhere else), `Mod+\` folds the sidebar to its compact rail and back, and opens the navigation drawer on a narrow window |
| In a list | `j` and `k` walk the rows — on a task opened from a list, they open the next and the previous task — `Enter` opens one |
| With a peek open | `[` and `]` step through the list it was opened from, `Esc` closes it |
| On a page with tabs | `1`–`9` go to that tab |
| On an open artifact | `i` its details, `h` its versions and history, `d` its data (where it keeps any), `p` who is here, each pressed again to close what it opened; `s` Share. `Esc` from inside a panel closes it (bound by the design system's `ArtifactPanel`, and listed with the layer's Escape) |
| In the artifacts gallery and its folders | `x` starts picking artifacts (and, pressed again, stops and clears the picks), `m` moves what is picked to a folder, `n` makes a new folder where the reader is, `u` goes up to the folder above. Bare letters, since a window chord compares no Shift and no Alt |
| On a chart | `+`, `-` zoom and `0` fits, with focus in the chart (bound by the design system's canvas) |
| In the org builder | `Mod+Z` undo, `Mod+Shift+Z` redo |
| Writing a reply or a message | `Mod+Enter` sends it: a reply in the Inbox or a comment on a task. `Enter` alone is a new line, since such a letter is often several lines |
| Writing a chat message | `Enter` sends it and `Shift+Enter` adds a line (bound by the design system's `Composer`), `Up` in an empty box edits your last message, `Mod+B` and `Mod+I` bold and italicise the selection |
| On a focused chat message | `E` edits your message, `T` opens its thread, `S` saves it for later, `Delete` deletes it, `Up` and `Down` move to the message above and below |
| Writing the company's principles | `Mod+Enter` saves the part being written and `Esc` stops writing it, asking first when it has changes (bound by the design system's `InlineEditor`); in the standing policies `Enter` adds the policy typed, and `Alt+Up` and `Alt+Down` move the one you are in |
| In search | `Tab` and `Shift+Tab` walk the scopes (bound by the design system's palette), `Mod+Enter` asks an agent, `Alt+A` assigns the task at hand, `Alt+C` creates a task, `Backspace` in an empty picker goes back |
| In Ask Crewlet | `Enter` sends the question and `Shift+Enter` adds a line (bound by the design system's `Composer`); `Esc` stops a running answer first, then closes Ask Crewlet and never a peek beside it; `Up` in an empty box brings back the last question |
| In a dialog, a menu, the drawer or Ask Crewlet | `Esc` closes whatever is on top (bound by the design system's layer stack) |

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
`matchesRow`. And a row nobody binds fails too: the frame's rows are bound by
the frame alone, so the suite mounts it and refuses an "Anywhere" row it did
not bind — a key the legend offered and nothing answered.

**`/` searches what you are looking at.** A screen with a search box of its
own (the seat filter, the event log, the tools catalogue, the audit log's
actor, work search and a list's key-or-title box, Doclet and its
page finder, Search chat) registers it with `useSearchTarget`, and `/` focuses it with its
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
artboard draws it — the sidebar's Live row carries the same count on every
other page, and a chip in every bar sat between a task's trail and its actions
and pushed a profile's controls past a phone's edge), the star, and LAST
the screen's own controls (portalled in by `PageActions`), so a screen's
primary action ("New task") ends the row where the approved designs put it.
A SCOPE's lenses (a project's Tasks, About, History and Files, portalled in by
its page, or All projects' Projects, Tasks and History, drawn by the header)
sit in a second slot straight after the trail (`PageLenses`): that slot is no
flex item at all while it is empty, and when it holds lenses it is the lens
group that takes the line's slack — the trail sizes to its content, capped at
62% of the bar so a long name ellipsises beside its lenses rather than pushing
them onto a line of their own.
On a phone the bar keeps ONE action in view — the screen's primary one, which
leads the line — and folds the rest into **More** (⋯) at the line's end: the
frame's star, and whatever a screen marks as secondary and
publishes to the frame with `usePageMenu` (a project's **Edit project**, which
opens the same dialog its inline button does). The menu is drawn only at that
width, so no action is ever in two places a reader can see at once, and what a
press did (the star cap, a refusal) is said in a toast, because the
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
hundred pixels, and "Org chart › Engineering · Co" was cut mid-letter under
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
  factor above zero pushes the star onto a second row by a fraction of a
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
  never changes with the width. A tab the reader cannot open draws the same
  lock the sidebar and the Settings column draw (the kit's `LockedLabel`), naming
  the access it needs. **A group the reader holds none of draws its key once.**
  A Worklet-only reader met a key on 13 of the sidebar's 15 rows, which read as
  noise rather than as a reason. Where every row of a named group shares one
  reason (`groupLock`), the key and the reason sit on the group's label and its
  rows go quiet, each still naming the access as its title and to a screen
  reader; a group with an open row keeps the key on each locked row. Rows are
  never hidden and stay focusable, and the sidebar and the Settings column
  follow the same rule. The widths are measured, and measured again
  whenever the strip or a tab changes size (a figure arriving, the web font
  loading, the density preference). **An object's own tabs fold the same
  way** (a seat's Overview … Settings, a turn's sections): a scrolling row
  hides its scrollbar, and on a phone a seat's Settings began five pixels past
  the strip's end with nothing to say it was there. **Both are the kit's
  underline row with `overflow="menu"`**, so the fold, its arithmetic, "More"
  and the reading note are the kit's: the section strip is `Tabs` in router
  mode (a `nav` of links, the current one `aria-current="page"`, a folded link
  taking the kit's `foldProps`), and an object's tabs are its tab list, "More"
  beside it. The tab, its figure, the accent bar under the current tab and the
  focus ring are one control across a workspace and an object.
- **The star is on every page that is one.** A workspace's own page is already
  a sidebar row, so the star there cannot keep it, but it is drawn,
  unavailable and saying "Already in the sidebar", rather than left out: left
  out, it appeared on every section of a workspace but the first, and moving
  between two tabs moved the header's controls. Whether a page IS a row is
  `nav.ts`'s `isSidebarRow`, asked by the inline star and by the phone's More
  alike, and never the path's length: the reader's Account is one segment long
  and has no row, so it is kept like any other page. Doclet's landing and
  its pages are the exception: a page is starred from its own More menu, so
  neither draws the frame's star (`useOwnStar`).

### An open artifact is the screen

Every object page in the frame is read the same way: the page bar with its
trail, then the object's header (its eyebrow, title, facts and actions), then
its tabs. An open artifact is the one deliberate exception, and it draws ONE
slim bar instead, the height of a large row (`size.row.lg`, 48 px), with the
artifact at full size under it.

**Why.** An artifact is not read the way a task or a seat is. A task's page is
its facts; an artifact's page is the artifact, a deck, a page, a sheet, a
canvas, and every pixel of chrome above it is a pixel it is drawn smaller in.
The frame's bar, an object header with a fact line and a tab strip stacked
over it cost about 280 px at 1440 (a deck's slide was drawn a third smaller
for it) and most of a phone's first screen, to say things a reader asks after
once: who owns it, when it changed, their role. And the tabs split one object
into four places the reader had to leave the artifact to visit. A document
application draws one bar and opens the rest on demand, and so does this.

**The bar** is the kit's `ArtifactBar`, drawn in the frame's place
(`OwnPageHeader`, so the frame draws no page bar and no trail under it):

| Where | What |
|---|---|
| Start | the frame's navigation toggle on a narrow window, the way back to the gallery, the kind's glyph, the title as the page's `h1` (an editor renames it in place: Enter takes it, Escape puts it back, and a refusal is said at the field), and the version chip (`v3`, or `v2 of 3` in the accent while an older version is on show) whose menu picks a version |
| Middle | the kind's own views, as one segmented control: a deck's Edit, Grid and Present. A view PUBLISHES them (`lib/artifacts/kindControls.ts`), with its own tools (a doc's Edit) and its rows in More (Print, the presenter's window), so no kind draws a second row of chrome over itself. While a doc is edited it publishes its editing state in their place ("Editing", and "Unsaved changes" once anything differs), with Cancel and Save, so the writer finishes where they pressed Edit |
| End | who is here (the presence stack), Details, History, Data where the artifact keeps any, People, then Share and More (Rename, Change icon, Settings, the kind's rows, Open in a new tab, Copy link, Pin, Move to the trash) |

Every control is an icon with its name and its key in a tooltip, and its
name is the button's accessible name; a panel's control is pressed while its
panel is open.

**It folds by its own width**, a container query, since the sidebar or a
panel beside it decides its room: under 860 px the presence stack folds (the
People control counts who is here instead), under 720 px Share's word goes
and it stays as its glyph, and under 640 px the views and every unlabelled
control fold into More as rows with their keys, and the kind's glyph gives
the title its room. Both arrangements are drawn and the stylesheet shows one,
as the page bar's phone More does, so no control is in two places a reader
can see at once.

**The panels** are the kit's `ArtifactPanel` in its `ArtifactWorkspace`: one
at a time at the artifact's side, named by its heading, `panel=` in the
address (opening one pushes, so Back closes it). Beside the artifact where
the workspace holds two useful columns (960 px), over its end edge where it
does not, and the whole screen on a phone. Data and History can take the
whole workspace (`expand=1`, and a comparison always does), with the
artifact kept mounted behind them. Opening a panel moves focus to its
heading; closing it from inside hands focus back to the control that opened
it; a panel a link opened takes no focus. The old addresses still open:
`tab=data`, `tab=history`, `tab=settings` and `rail=people` are read as the
panel each became and rewritten in place.

**What did not move**: the notices (an administrator's opening, a room that
could not be joined, offline, a newer version published) are one-line
banners across the top of the workspace, and following somebody is the
`FollowBanner` there. The page still asks the frame for the window's height
(`useFillScreen`), so the body scrolls inside itself and a panel beside it
stays in view however far the artifact is read; a kind's view measures the
body (`--artifact-view`) rather than reserving room for a header that is not
there.

### Asking an agent from a create page is a panel, not a mode

Every kind's create page is the artifact being made: a doc's page, a deck's
stage and filmstrip, a sheet's grid, a board, a design's canvas, an HTML
page's source. Handing the work to an agent does not swap that page for a
form. The bar carries **Ask an agent**, drawn with the suggested agent's
Crewlet character, and it opens the hand-off panel at the page's side: the
kit's `AgentHandoffPanel`, an `ArtifactPanel` like an open artifact's Details
or History, so it is a column where there is room, a drawer over the page's
end where there is not, and the screen on a phone. The page stays live
beside it.

**Why.** Each page used to switch between "make it yourself" and "ask an
agent" with a segmented pill ("Write it | Ask an agent", "Who writes it: You,
in the editor | An agent", "Build it now | Ask an agent", "Describe it | Bring
the HTML"), which replaced half the page with a form and told the reader the
two ways were rivals. They are not: a person lays out a few slides, then asks
an agent to write them; drafts a page, then asks for the rest. A side panel
is how Figma's and Notion's assistants and Linear's agent assignment sit
beside the work, and it keeps the direct way the page's own.

**The panel, top to bottom**: a line saying what the agent does with the
brief, the brief (a composer, Command+Enter to send) with briefs to start
from while it is empty, the kind's own fields (a design's name and references), who
builds it and in which project (`WhoBuildsIt`), **Goes with it** (what the
reader already made: the doc's draft or template, the slides and theme, the
board's figures, the sheet's columns and rows, the design's boards, the HTML
page's source and what it may use, and "Your brief alone" while there is
nothing) with the task exactly as it is filed behind a disclosure, then
**What happens next**. The send is the panel's foot, "Ask SRE Lead" beside
that agent's character, pressed by Command+Enter anywhere in the panel; held,
it says why in words beside the button, and the engine's refusal is said
above it. The bar's primary stays the direct create.

**Two kinds arrive with it open.** A design is drawn only by an agent, so its
page opens with the panel beside the canvas and its bar's Ask an agent is the
primary while the panel is closed (Command+Enter outside the panel opens it).
An HTML page is most often described rather than written, so its page is the
source with the panel open beside it; closing it leaves the source to write
in. Each arrives with it open only where the panel stands beside the page
as a column (a laptop's width and up); narrower, it would arrive as a drawer
over the page's first step, so there it waits for the press. The panel's
state is the address's (`panel=ask`, and `panel=closed` on a kind that opens
with it), as an open artifact's panels are.

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

### Signing in is a screen outside the frame

Four routes draw no sidebar and no page header — `#/login`,
`#/invite/{id}.{secret}`, `#/reset/{id}.{secret}` and `#/enrol` (`FRAMELESS` in
`app/nav.ts`): a frame whose every row is locked is a frame showing a person
what they cannot open. **And they dial nothing that needs a session**: the
socket is made at boot and dialled by the frame alone (`app/Shell.tsx`) — a
sign-in lands in the frame and dials nothing itself — so a browser that opens
one of these four, or opens `/dashboard` signed out, sends no handshake, no
refusal probe and no degraded-mode snapshot on its way to the sign-in.

**The frame asks who this is once, before anything else** — `GET
/auth/session` (`lib/frameSession.ts`), which the sidebar's user block, the
refusal panel, the Account, Home's greeting and People read too,
where each used to ask for itself — and **dials for
every session it reads**. The socket admits every principal the engine
resolves whose stage may act, and what a reader may SEE on it is decided per
push by their access (the overview for everybody, a seat's transcript for its
lead and an admin, Worklet's pushes for whoever holds Worklet; ADR-0033),
so a person somebody gave no app still reads the org chart, the agents'
status and their own account, and nothing is held back for them. A `401` is
nobody, and the sign-in follows with nothing dialled; a read that failed on
anything else says nothing about the session, and the handshake decides.
Access given or taken later reaches an open tab by the engine closing its
socket to reconnect (1013), after which the viewer is asked again — the frame
does not poll for it. There was a **no access yet** state while the overview
needed a grant of its own: nothing was dialled for a session without it, and
the session was asked again every thirty seconds in case an administrator had
given it; it went with that grant. A socket the engine REFUSES outright — a
person whose seat was taken out of the chart — is the handshake's refusal,
drawn by `app/frame/AccessRefused.tsx`. The sign-in asks
`GET /auth/session` once, to say whose session the browser already holds —
and not at all when a `401` routed the reader there, which has already said
it: a signed-out load of `/dashboard` asks once on its way to the sign-in, as
`#/login` opened directly does. **A
sign-out holds the socket** until it is answered (`LiveSocket.hold`): the
engine closes the session's socket as it applies the sign-out, and that close
re-dialled before the reload, one more refused handshake on the way out.

**The session cookie is the browser's only credential.** The dashboard keeps
no token anywhere — not in storage, not in a URL — and every REST call and the
socket's handshake carry the cookie the browser attaches on its own, which no
script on the page can read. An API token typed on the sign-in screen (for a
deployment whose only credential so far is a Tier A token, or somebody who
signs in as a machine) is sent once, in the `Authorization` header of
`POST /auth/token`, exchanged for a one-hour session, and held nowhere but the
form while it is typed.

**A refused session sends the reader to sign in, and back.** A `401` from any
REST call, or from the socket's plain-HTTP re-ask, raises a session need
(`protocol/session.ts`) that the app follows by replacing the current entry
with `#/login?next=<where they were>`. The need is STATE rather than an event,
because the socket can learn it before React has mounted, and a sign-in clears
it. A `403 second_factor_enrolment_required` does the same toward `#/enrol`. A
`4403`, a `seat_unavailable` and a refusal that names an access do NOT: the
person is signed in, and signing in again reaches the same person with the
same access. A tab that was read by somebody (`lib/reader.ts`) and lands here
holding no session lost it without a sign-out in this tab — one made here
empties the tab's storage — so the sign-in says **You were signed out** and
what ends a session elsewhere: a sign-out from another browser or tab, a
password change, an administrator, its own deadline. Only a `401` from
`/auth/session` says so: a node behind on the identity log answers `503`
precisely so that no browser is told it was signed out when it may not have
been, and a throttle or a dropped connection says nothing either way.

**`next` is an address on this page, or nothing.** It is the one parameter of
a sign-in an outsider can choose for somebody else — a `#/login?next=…` link in
a message — so `lib/session.ts` accepts only a hash route of this dashboard: no
`//`, no backslash, no control character, never one of the four screens above.
Anything it refuses lands on Home.

**A sign-in replaces the history entry it was made from** and goes to `next`,
where the frame dials the socket for the session it reads — through
`#/enrol` first when the engine opened the session only for enrolling a second
factor. The screen prints the engine's own sentence for a refusal and nothing it
composed itself: a failed sign-in is one refusal on purpose, and a
distinguishing message would be a roster. `second_factor_required` asks for the
six-digit code or a recovery code and resubmits the same details with it; a
`429` says how many seconds its `Retry-After` names. The password form is
always there — password sign-in is always served — and the deployment's API
token waits behind **Use an API token instead**. Where `/health` says
`identity: unclaimed`, nobody has joined yet — an open invitation does not
change that, since redeeming it is what creates its person — and the token is
the only way in, so its form is open from the start and the page says how to
begin: sign in with the token, then open **Company › Org chart**, choose your
human seat and invite or create yourself on it — every person holds a human
seat, so a seat is where a person comes from — or open a link already issued.
A node that runs no company yet has no seat to put anybody on, so there the
company comes first, created from **Org chart › Edit org** with a seat for
yourself (see [How the first person
exists](../concepts/identity-and-access.md#how-the-first-person-exists));
`unknown` keeps the ordinary page, because a node that cannot read its identity
estate is not one with nobody in it. Somebody already signed in is told who, and
offered to continue as them.

**A signed-out load costs one refused request, a two-step sign-in one more,
and the dashboard logs neither.** The session cookie is `HttpOnly`, so whether
this browser is signed in is a question only the engine can answer, and its
answer for nobody is the `401` the frame, the socket's refusal probe and every
REST call route on — a `200` meaning "nobody" would be one more exemption from
the request guard and a second shape of "who am I". So a cold load, of
`#/login` or of any screen opened signed out on its way there, makes exactly
one refused request: `GET /auth/session`. A person who holds a second factor
adds exactly one more, at the password step: `POST /auth/login` answers
`401 second_factor_required`, typed so that nothing reads it as signed out,
and a `401` rather than a `200` because a `200` from `/auth/login` always
opened a session — the same details resubmitted with the code are the sign-in.
A browser writes every 4xx a page loads to its console ("Failed to load
resource … 401") and no page can stop it, so those one or two lines are
expected on a sign-in that works. Nothing of the dashboard's is beside them —
it calls the console nowhere — and a refused request beyond these, or a
console line of ours, is a regression: `app/App.session.test.tsx` holds the
cold loads of `#/`, `#/account` and `#/login` to the one, and
`routes/signin/SignIn.test.tsx` holds the two-step sign-in to its one more and
every sign-in path to a quiet console.

**An invitation is read, never spent, by opening it.** The link carries the
whole credential in the fragment, which no browser sends to a server, and the
screen sends the secret BESIDE the id — the `X-Crewlet-Invite-Secret` header on
the view, the body on the redemption — never in a request URL. Both link
headers are declared in `contract/identity.ts` (`INVITE_SECRET_HEADER`,
`RESET_SECRET_HEADER`) and held to the views' own spellings by an engine test,
because a header renamed on one side alone presents no secret and every link
opens as the `410` an id nobody issued answers. The view names
who it is for, who sent it — when a person did: one a Tier A token or a service
account issued, the company's first person's above all, names no sender, since
`token:founder` is nothing its reader would recognise — and the seat it binds.
Every person holds a human seat, so every invitation names one; where the
chart no longer holds that seat, the screen says before the form that
redeeming it will be refused and to ask whoever sent it for a new one, since a
warning after a password is typed is a wasted form. The form proposes
the login
the engine suggested — held to the login grammar, and said under the field,
before anything is posted — says which session it signs out when the browser
is already signed in as somebody, states the password length `/auth/config`
asks for and takes the password twice — a slip in the only copy is an account nobody can
sign in to, and the way back is an administrator's reset link. A
spent, withdrawn or mistyped link is one screen (`410`), and a login or address
somebody else holds is the engine's own sentence (`409`) over the form as
typed.

**A password link is the invitation's screen again, and signs nobody in.**
Two links land on `#/reset`: the **reset link** an administrator issues
somebody who has a password, and the **first password link** a person's
create hands back — the way a person an administrator created on a seat sets
the password they have never had. Each carries its credential in the fragment
the same way, and the screen sends the secret beside the id — the
`X-Crewlet-Reset-Secret` header on the view, the body on the spend. The view
names whose password it sets and says which link it is (`first`): a reset is
**Choose a new password**, and says setting it ends every session the person
holds; a first password is **Choose your password**, and ends nothing, since
there was nothing to end — told that it would, a person just created read
their first sign-in as a lock-out. Either takes the password twice and states
the length `/auth/config` asks for. A spent, revoked, expired or mistyped link
is one screen (`410`), **This password link can no longer be used**, which
names no lifetime — a reset lasts a day and a first password link a week, and
a link that no longer opens does not say which it was. Setting the password
opens no session, so the screen ends on a **Sign in** button rather than in
the product: a second factor the person holds is asked for there, which a
session handed out by the link would skip.

**A required second factor is enrolled before anything else opens.** `#/enrol`
asks `POST /auth/totp` for a seed — drawn as a **QR code** of the seed's
`otpauth://` URI, exactly as the engine answered it, with the key, grouped in
fours for typing, and a link that opens the same URI in an app on this device
beside it for whoever cannot scan — confirms a code from the app, and shows
the first recovery codes once. The code comes first because an authenticator
app usually lives on a phone, and a phone scans: shown only the key, a person
types thirty-two characters of base32 into it by hand, and one slip is a first
code that does not match and the whole key typed again. It is drawn as inline
SVG from the encoder's module matrix (`uqr`, MIT, with no dependencies of its
own) — no image URL, no markup string, nothing fetched, so the page's
Content-Security-Policy is unchanged and the URI, which carries the seed, goes
nowhere but the drawing and the link: no log, no URL, no storage. It is shown
while the seed is, and gone with it. The
[Account](#account-the-readers-own-page) page offers the same two steps, and a
new set of recovery codes, to a person who wants to add or replace one later.

**A step-up is one modal, and the refused request is replayed.** A
`403 step_up_required` is not a screen's to handle: `protocol/rest.ts` asks the
one confirmer the app installed (`app/StepUp.tsx`), which opens a single
"Confirm it’s you" dialog however many requests were refused at once, posts
the password — and a code where the person holds a second factor, which the
dialog reads from their own credentials (`GET /iam/credentials`) so the code is
asked for rather than offered as optional, and its text asks for both — to
`POST /auth/step-up`, and on success replays each refused request exactly once.
A wrong password or code is the engine's one sign-in refusal, said as a
confirmation refused ("That password or code was not accepted") rather than
as the sign-in form's "sign-in details", and the dialog stays for another try. While it asks, a dialog
whose write it holds says it is waiting for the person (`lib/waiting.ts`) —
its button "Waiting for you", its close "Waiting for you to confirm who you
are." — rather than for the engine, which has nothing in hand. Cancelled, each request fails with the refusal it had. A refusal of
`/auth/step-up` itself is never confirmed, so a confirmation cannot ask for
another.

**A sign-out reloads.** `POST /auth/logout` — or `POST /auth/logout/all` for
every device — and, only once the engine has answered, a reload into
`#/login`: a route change would leave the store, every cached answer and the
socket's last snapshot in memory for whoever sits down next. The tab's
`sessionStorage`, which holds the org builder's kept draft, is emptied first,
because a reload keeps it. A sign-out nothing answered, or a revocation the
engine could not confirm (`503` with an `op_id`), is said, and the page stays.
Both sign-outs are drawn from `GET /auth/session` rather than from the socket's
`viewer` question, so a person the socket refuses — a seat taken out of the
chart — still has them: the user block offers
the two under the session's own login, and the refusal's strip carries a
**Sign out** of its own.

**A sign-in by somebody else hands the tab over the same way.** A session also
ends with nobody signing out — an idle deadline, a revocation, a sign-out in
another tab — and the tab is then routed to the sign-in with what its last
reader saw still in memory. So the tab records WHO it is read by
(`lib/reader.ts`: the principal id every sign-in answers, kept in the tab's
`sessionStorage`, and adopted from `GET /auth/session` by a tab opened with a
session already in the browser), and a sign-in naming anybody else empties the
tab's storage, records the new reader and reloads into where the sign-in was
going — the enrolment first, for a session that may only enrol. The same person
signing in again after their session lapsed keeps the tab, and with it the
builder's kept draft. That draft is stamped with the reader it was kept for, and
one kept for anybody else is discarded unoffered.

**And so does a session somebody else left in the browser.** Signing in as
somebody else in one tab moves the cookie every other tab of the browser
shares, and tries to end the session those tabs were opened with — best effort,
since a sign-in is never refused over the close of somebody's other session.
Where the close lands, each socket closes `4401`, and the socket's
`GET /auth/session` before it dials again answers for the new person; the
frame's own read answers for them too when the tab next becomes visible. Where
it does not, nothing on the socket says so, and a window left open beside the
one that signed in would go on drawing the last person's screen with every
request it sent going out as the new one. So every request a tab makes for its
reader names them (`X-Crewlet-Reader`, `protocol/signin.ts`): a REST read or
change, an act and its Retry, a download, the degraded-mode snapshot, the
socket's refusal probe, and the session keepalive (`protocol/keepalive.ts`),
which reads the session every five minutes while the socket is open unless the
engine answered something else in the last five. The socket's handshake is the
one request a browser cannot put a header on, so it names the reader in its URL
instead (`/ws/stream?reader=`, `READER_PARAM`), which the engine reads on an
upgrade alone. The engine refuses one whose cookie is anybody else's `409
reader_changed` before it runs, so a press in that window makes no change as
the new person, a Retry of the last person's change sends their operation as
nobody's, and a socket that dropped for another reason and dialled again under
the new cookie draws nothing of the new person's company: its refused handshake
reaches the page as a bare 1006, and the refusal probe reads the `409`. The two
requests that name nobody are the session read itself, which is how a tab
learns who to be handed to, and the token exchange, which signs somebody in. On
any of these signals a tab read by anybody else is handed over exactly as a
sign-in by them would hand it — emptied, its new reader recorded, reloaded
where it was — and the socket dials nothing for them first (`lib/session.ts`'s
`takeSession`, which `main.tsx` hands the socket, and `sessionMoved`, which it
installs for the refusal). The reader's id is no secret, but it is in the
socket's URL, so a proxy's access log of `/ws/stream` records it. A window
whose socket stays open and that sends nothing of its own is handed over by
the keepalive in under ten minutes, two keepalive intervals rather than one: a
tick passes over a tab the engine answered less than five minutes before, and
that answer may have come just before the sign-in.

**A tab learns who it is read by before it is served anything.** The check
above covers what a tab asks once it has recorded its reader, and a tab opened
with a session already in the browser records nobody until a read of the
session answers. The frame's own `GET /auth/session` records it before the
socket is dialled, and the socket holds every dial it makes for a tab that has
still recorded nobody — the frame's when that read FAILED, a reconnect, a
signed-out tab coming back into view — behind a session read of its own
(`protocol/socket.ts`'s `learnReader`): it records whoever answers and dials
naming them, a `401` is the sign-in, and any other failure dials nothing and
asks again on the backoff, since a dial naming nobody is served as whoever
holds the cookie. A dial made blind was the tab's first sight of the company,
and a sign-in in another tab before its next session read was then ADOPTED as
its reader by a tab drawing what it had been served for the last one. What
stays outside the check is exact. A browser that refuses the tab's storage
records nobody however often it reads the session (`lib/reader.ts` keeps the
reader nowhere else), so its requests name nobody and a cookie moved under it
is served rather than refused — the setting that blocks a site's storage
usually blocks its cookie as well, and with it the session. And a screen's own
REST read sent before any session read has answered names nobody; none is sent
today, because every screen's REST read waits for the frame's session answer or
for the viewer, which the socket answers only after a session read has.

**A thing worth linking to gets an address, not a scroll position.** The
previous dashboard revealed a unit by scrolling the org screen to it
(an `org` address carrying `?unit=Backend`, with a router-level reveal hook); this tree gives a unit
its own page instead, `#/company/org/teams/{unit}`, so it can be linked to, opened
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
company did.** That is Home until its reader arranges it (see
[Home is the reader's board](#home-is-the-readers-board)). Top to bottom:

- **The greeting.** The reader's morning, by the first word of their OWN
  name as `GET /auth/session` answers it from their directory row (`ownName`)
  — never their seat's, which is the chart's name for a place and often a
  role, so a person with no name was greeted "Good afternoon, Founder"; a
  reader whose row holds no name is greeted without one — and one sentence
  of the engine's: "Nimbus is running on 3 nodes. **3 decisions** are waiting on
  you, 1 condition needs a look, and 4 agents are working right now." The
  decisions figure is weighted; an engine condition — a refused session, a lost
  connection, no configuration, a node that shed its seats — takes the sentence
  over rather than sitting beside it. **Prose never prints a zero as a digit or
  a wrong plural**: "nothing needs your decision", "no agents are working right
  now", "1 condition needs", "2 conditions need" — and a fleet the presence
  read counted as none is "Nimbus is running.", never "on 0 nodes". "N
  conditions need a look" is a link to where they are listed, the Inbox's
  "Needs you" group (opened on the one when there is one). A reader
  nobody bound is told nothing about decisions at all. The **Today · 7 days · 30 days** control
  (`range=`) sets the window the figures below read over; Today is one company
  day, cut at the company's midnight by the engine.
- **Five figures** (kit `StatCard`s): *Agents working now* (`working / total`
  from the engine's `activity`, with the state bar of working, waiting,
  stopped and idle, and a sub-line naming only the states somebody is in);
  *Waiting on your decision* (the engine's `decisions` count plus the seats
  stopped on their budget that THIS reader can act on — raise the ceiling, a
  `/config` write an admin may make and so may the lead of the
  seat's unit — on a [managed](../concepts/configuration.md#managed-configuration)
  document, only where the reader's credential is one of its writers
  (`mayChangeConfig`) — or hand the item on with
  `update_work_item` — the oldest wait in the warning ink, and **Review**
  into the Inbox's decisions view; a stopped seat the reader can do neither
  about is one of the conditions that "need a look" instead — an em dash and "Not bound to a person" for a reader who is
  nobody); *Tasks in progress* (`work_flow.now.active`, its change over the
  week and what is blocked or overdue, with the fortnight's sparkline);
  *Completed* over the window, against the window before it; and *Tokens* over
  the window, with the **week's** budget meter captioned as the week's ("63% of
  this week's budget · resets Mon") in the engine's own state — or "No weekly
  budget", which links to Company › Budgets for a reader who may change the
  configuration (`mayChangeConfig`: Admin, on a document
  that admits the reader's credential) — who can set one there — and is plain
  text for anybody else. The Tokens tile draws no delta:
  its second line is the budget's, and a bare change in front of it read as a
  claim about the budget. A second line is whole FACTS, so a narrow tile breaks
  between them ("+17 vs last week" / "· 2 blocked"), never inside one. Each
  figure waits for its own answer and says so; one whose answer has not
  arrived is never a `0`.
- **Needs your decision**, the first three, newest first, each answerable IN
  PLACE (`DecisionList`). Asks are GROUPED BY WHO IS ASKING: one head names the
  asker (the seat they asked as, never a credential) with their badge, and
  what every one of their asks shares (the item, the role the reader is asked
  in, how long ago); what differs between two asks is said on the ask, and so
  is the channel an ask promised to post its answer to ("answer posted to
  #releases"). Under each question one line reads "Recommended: Hold release"
  with **Accept**, which sends that option, and **Change**, which opens every
  option as a choice with its detail line and the recommended one tagged
  (the kit's `ChoiceList`), plus **Something else**, whose field (the kit's
  `Composer`, up to 2,000 characters, Command+Enter to send) sends a reply
  instead of a choice; **Submit** sends what is chosen. While the options are
  open the recommendation line is hidden, and only one ask's options are open
  at a time. An ask with no recommendation offers **Answer** in place of
  Accept and Change. A choice is `comment_on_work_item{answers, choice}` and a
  reply `{answers, body}`; the engine wakes the asker with either. An answered
  ask stays as one line, "Answered Hold release (Recommended)", "Answered Ship
  anyway" or "Answered “the reply”", until the card is drawn afresh; there is
  no undo, because the asker is woken the moment the answer lands. A coding run parked on a question is answered
  by its turn (`answer_run`), with how long its box is still held: its row shows
  the question's one sentence, never the context under it, and its **Answer**
  dialog draws the run's ask as the Inbox does, its options as buttons. A seat the
  engine stopped on a spent budget offers **Raise budget** — a dialog of that
  scope's three ceilings, opened on the seat's own when its own window is the
  one refusing and on the company's when the company's is, with the refusing
  window focused (see [Company › Budgets](#company--budgets-raised-in-place)),
  and **Reassign** of the item it was on. An ask with no options is
  answered in words in place: **Reply** opens the field under the question
  and sends `comment_on_work_item{answers, body}`. Every
  control is drawn for every reader and disabled with its reason where this
  browser cannot act. **Open inbox** and **Review** both land on
  `#/inbox`, whose list opens with every one of them under Needs you.
- **Live now**: how many seats the engine says are working, and for the
  longest-running four whose turn has an id — the very rows the sidebar's
  Running group names, in its order — what each is on (the item the turn is
  charged to, never a work key), how long, where it is in context → execute →
  review ("Execute · coding run"), and the last call it made, each a way
  into its turn. **Open live view** for the rest, including a working seat
  whose turn has no id yet: it is counted here and listed there.
- **Tasks completed** per company day over the last fourteen (today in the
  accent), **Tokens by team** over the window (tokens, never money), each
  team's bar split by the seats that spent in it and keyed under the bar (the
  engine's `parts`; a team of more than four seats names its three biggest and
  draws the rest as one part in the residual hue), and
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

**Home leaves out what cannot arrive.** Where the company does not run
Worklet (see [the sidebar](#the-sidebar)), the *Tasks in progress* and
*Completed* figures, **Tasks completed** and **Projects** are not drawn, nor
offered in the tray, and New task leaves the page bar: a tile reading zero
would claim a tracker that answered nothing. The decisions, the conditions,
Live now and the tokens arrive under every backend and stay.

Five tiles sit in a row while the page column holds them — five tiles at their
176px floor and four of the board's own 16px gaps, 944px, so the figures'
gutters stand on the gutters of the blocks under them — three over two under
that, one on a phone; the board's blocks stack below the same width. Every one of those is measured from
the SCREEN's column (`@container page`), which the shell declares on its
content as well as on its header, because the sidebar and an open peek take
their share of the window first.

#### Home is the reader's board

**Edit Home**, in the page bar, turns the page into a board on a fine hatch:
the reader drags a widget to move it, drags its corner (or, for a group, its
edge) to resize it on twelve columns and 16px rows, groups widgets under a
coloured heading that folds away, and adds widgets from a tray. While Home is
arranged, the page bar holds the editor alone: how many changes are unsaved,
Undo and Redo (Mod+Z and Mod+Shift+Z, the key table's `draft.*` rows), Add
widgets, Cancel and Done. New task steps aside until Done.

- **A draft, kept on Done.** Nothing is stored until Done. Cancel with changes
  asks before discarding them, and so does leaving the page. Folding a group
  outside editing is kept at once, because it is a choice about the page rather
  than a layout change to confirm.
- **The tray** offers Home's own cards (one each: two "Needs your decision"
  cards would be one decision drawn twice), a single figure, a shortcut to any
  page, the reader's own notes, headings, dividers and links, and the widgets
  every section of the dashboard offers, under the section's sidebar name and
  in the sidebar's order, with a search. A widget with a choice to make (which
  saved view, which project, which agent) opens a sheet of its own first, with
  a step back to the tray beside its title and **Add to Home** at its end,
  disabled until the choice is made; nothing is added until then.
- **A section offers its own widgets.** Each lives in a `home.widgets.tsx`
  beside the section's screens (`dashboard/src/routes/<section>/`), exporting
  `widgets = defineWidgets([...])` from `routes/home/board/kit.tsx`; the
  registry finds every such file at build time, so a section added later
  offers widgets by adding that one file. A widget names the page it
  summarises by its `DESTINATIONS` key, and takes from there the tray section
  it is listed under, the access that opens it and where its card's **Open**
  goes: a reader whose access does not open the page is never offered the
  widget (the tray says how many were held back), and one who loses that access
  later sees a sentence in its place, never the data. A widget asks the same
  questions its page asks and only reads; every write stays on the page. A
  contribution that names no such page is left out and reported, and a suite
  (`board/registry.test.ts`) fails the build on it.
- **A widget never says "nothing" over an answer that could not account for
  everything.** An answer says when it is short: a record this build cannot
  read met the read (`complete: false`), or a node did not answer a fleet read
  (`coverage`). `WidgetQuery` in `routes/home/board/kit.tsx` is the one place
  that is decided, over the gaps `gapsOf` reads from every answer a widget
  draws and the widget's own lines (a muted room, a list cut at its bound): a
  widget's claim ("You're caught up", "No turn failed in the last 24 hours")
  is drawn only over an answer with no gap; with a gap and no rows the card
  says there is nothing to list and why it may be wrong, and with rows it says
  the gap under them. A figure says "May be incomplete" on its second line, and
  a count that stopped at its ceiling is written as the floor it is ("50+").
  A count another surface already says is read from that surface's reading,
  never from the widget's own page: the Inbox card's is the sidebar badge's
  (`useInboxCounts`), so the two never name different numbers. And a widget
  that follows one thing reads it by its key, as its own page does, never by
  finding it in a page of a listing: the Project widget and a pinned project
  ask `work_project`, so a project past the listing's first page is still the
  one the reader chose, and a picker whose list the engine cut says so.
  Every widget module, and every card Home's own widgets draw, goes through
  `WidgetQuery` rather than `QueryState` or `EmptyState`, which
  `board/kit.test.tsx` holds, and `board/widgets.render.test.tsx` answers every
  widget's questions short and holds each to saying so.
- **Per reader, per browser**, as stars are (`STORAGE_KEYS.homeBoard`), and
  read back as untrusted input: every field is checked, every string capped,
  and every link held to a `#/` address or an `https://` one. A widget whose
  section no longer offers it keeps its place and says so.
- **Links open in a new tab while Home is arranged**, so following one never
  leaves an unsaved board. At rest they behave as everywhere else.
- **Below the stacking width** (944px of the page's own column) every block
  spans the page and cannot be resized, as the paired cards always stacked.
- **No star in Home's bar.** Home is the sidebar's first row and has no
  sections whose bar a disabled star would hold steady (`WorkspaceRow.starless`),
  so the bar carries Edit Home instead.

### The Inbox is where you act

The Inbox is built around what waits on the person (the questions put to
them, the runs parked on them, the seats they can unblock) and then what
reached them. It is two columns, each its own scroller: the list, and the open
item read in full beside it with the answer to it. Every visual part of both
is the kit's (`InboxList` and `ReadingPane` in `@crewlethq/ui`); the screen
only places them.

**One list, in a fixed order, with no filter bar.** **Needs you** leads: the
asks put to this person and the coding runs parked on a question to them
(`decisions`, the read Home's figure counts), the seats the engine stopped on
a spent token budget, and the conditions a person decides (`lib/attention.ts`,
the subjects whose home is `seat`). **Updates** follows: what reached them and
is not read yet (`work_inbox{unread, snoozed: exclude}`). Decisions are never
interleaved with updates by time, which would eventually rank "a task you
watch moved" above the CEO asking whether to hold a release. An ask is ONE
row: the notice that told the person they were asked rides on the decision it
is about rather than being listed again under Updates, and answering the ask
marks it read, so an answered question never comes back as an update. What
was snoozed and what was read already are two quiet links at the end of the
list, **N snoozed** and **Earlier updates**, rather than a scope or a chip at
its top: a filter bar is read every time, and these are opened rarely. The
snoozed count is read whether or not its list is open (`snoozed: only`);
Earlier is read only once it is opened (`unread: false`, of which the read
ones are listed), and both page forward fifty at a time with **Show more**.
Home's **Review** and **Open inbox** land on `#/inbox`, whose list opens with
every decision they counted. A change the person made themselves is never
one of the updates: they write as the seat the inbox is kept under.

**Every row says the same three things.** Who: the seat behind it, with its
avatar; a person's change is drawn as the seat the engine recorded it under,
one by a credential bound to no seat (author kind `operator`: a Tier A token's
session or a service account acting as itself, never a person, who holds a
human seat for as long as they are here) under its own login, the name the record carries
(`iam.ActorFor`) and a name rather than a secret, and one the engine made
itself (`system`) names nobody and reads "The engine", with the engine's mark;
never as the credential a change came through (`operator_id`). What, in up to
three lines, never cut at the edge: the question, the stop ("The daily token budget
is spent", with **stopped** after the name in the danger ink), a run's
question (with **coding run** after the name), or what an update did ("Mentioned
you", "Changed its status") with its excerpt after it in the quiet ink. And
the task it is about, as a quiet link of its own under the row: its key and
title (one `work_items{key}` read resolves the titles of every row that names
its task by key alone), which opens the task in the frame's side panel on a
plain click and its own page on a modified one. An excerpt the engine
COMPOSED names its author by name as the row's head does: the engine writes
"maya-ops put LEAD-3 at position 1 of your priorities" with a handle, for the
seat it wakes, and the row reads "Maya Ops put LEAD-3 …"; a comment's excerpt
is somebody's own words and is left as they typed it. Unread is heavier;
the open row takes the neutral pressed fill (never the accent, which made a
column of rows read as a column of alerts) and is the one row marked current.

**The pane reads one item top to bottom, with nothing to open.** One sentence
says who is asking, what is asked of the reader and on which task ("CTO asks
for your approval on LEAD-12 Q4 release plan"); the time is on the row, so it
is not repeated. The question follows in large type, then its context, then
the asker's reasoning on an inset ground ("CTO recommends **Hold release**")
with the evidence it cites under it: a task, a page, a turn trace, a coding
run or a link. An update's pane names what it did as its heading, quotes a
comment whole under it, and says why it reached the reader where the heading
does not ("· you follow this task", from the reason of an update that only
informs, or "· snoozed until …").

**An ask is answered in one press, through the one write every surface uses.**
The pane draws `AnswerAsk` in its `buttons` layout: each option is a button
that SENDS that choice as the answer (`comment_on_work_item{item, answers,
choice}`), the recommended one first and filled, all through ONE write so a
second choice cannot be sent while the first is in flight. **Something else**
opens the field in their place and sends the answer in words
(`{item, answers, body}`); an ask with no options is answered in that field
from the start. Where the ask promised a channel (`decision.inform`) the line
under the answer says "Your answer is also posted to #channel": the engine
holds the asker to it (`ExternalNotification.owes`), and an ask that promised
nothing draws no such line. A parked coding run's pane is drawn as a
question is (ADR-0068): its one-sentence question heads it with its inline
references placed, its context follows as Markdown, its recommendation sits with
the reasoning and the evidence it cites, and its options are buttons, the
recommended one first and filled, with "Something else" to answer in words. An
open question is answered in its field. It says how long the box is still held
and answers by its turn (`answer_run`). A question from an older build, one
free-text block, draws its first sentence as the heading and the rest as
Markdown; nothing is ever quoted verbatim. A stopped
seat's names the window that is spent and offers the two ways out: raise the
ceiling, or hand the item on. A condition's says what it costs to leave and
where the answer is.

**What was done is shown, then the next item opens.** An answered question
or run stays in its place in Needs you until the reader leaves the screen,
marked **Answered** in its head (a soft success tag where the time was) with
the answer under the question ("You chose **Hold release**", or what was
written, with a speech mark), in full ink and never struck through: a strike
reads as "cancelled" and cuts through the words a reader comes back to check.
The group's count is what is still open. Its pane shows the answer where the
controls were; a read or snoozed update shows its new state. 700 ms later
(`ADVANCE_MS`) the next item after it in the list opens, unless the reader has
moved on first.

**What was answered before waits in a closed group.** The asks put to the
reader and closed in the last week, and the coding runs' questions the reader
answered themselves (`answered_decisions`), are an **Answered**
group after Updates, closed and counted ("Answered 4"): its label is a
disclosure button inside the heading, so a reader moving by heading lands on
it and Enter opens it. Each row names who asked, the question, and who
answered and how ("Maya Ops chose **Ship anyway**"), since a colleague may
answer a question put to you. A coding run's question the reader answered is
a row of the same group, drawn the same way: its pane reads the run's ask in
full (the question, its context, its options with the chosen one marked) and
links to the run, with no conversation, since a run's question has no thread.
A row answered on this visit is in Needs you
instead, never in both. Opened, its rows step with `j` and `k`; a link to one
opens the group. An answered ask's pane reads like the one it was answered
on, with the answer in place of the buttons, a line saying who answered for
you where it was not you, **What happened next** as four steps (asked,
answered, picked up, acted on: the last two from the asker's seat as it is now
and its first record on the task since, with a paused or stopped seat marked
for attention), and the conversation with a reply box.

**The conversation is the last part of the reading.** It is labelled by what
it holds: "Replies to this question" under an ask, "Replies to this comment"
under an update about a comment (whose comment is quoted above, so it is not
listed again unless the update carries only a preview of it), and
"Conversation on this task" under any other update. It is read a page at a
time from `work_comments`, the newest page polled, and **Earlier comments**
reads the page before. A reply thread is narrowed by the engine (`thread`), so
the button appears only when that conversation has earlier comments, and an
empty one reads as its heading, "No replies yet." and the reply box, one
spacing step apart. A comment's author is the seat a person wrote it as
(the engine records it so), one by a credential bound to no seat is drawn
under its own login, and a comment with no author reads "The engine"; never
the credential it came through. The reply box at its end writes as you:
`comment_on_work_item{item, body, reply_to}`, which answers nothing. `@` opens
a picker over the org that writes `@handle` into the text (the engine resolves
mentions from the body, so the picker never sends a list that could disagree
with it); `#` links another task (`update_work_item{linked}`); the paperclip
attaches a page, an existing one or what was written saved as a new page
(`write_page`) and then attached (`linked_pages`). `Mod+Enter` sends. A
question answered in words has no second box: its own field above is the
answer.

**Every mark is a gesture on exactly what it names, and only an update has
them.** **Mark as read** marks the open update read and turns into **Mark as
unread**; **Snooze** puts it off to an hour from now, tomorrow at 09:00 or next
Monday at 09:00 on the company's clock (each moment offered once, so a Sunday
has no second Monday), or a moment the reader picks, and ONLY the presets
inside the engine's `max_snooze_ahead` (from `work_person`) are offered, since
the write refuses the rest. A question, a run, a seat and a condition carry
none of them: each leaves the inbox when it is answered or cleared, which is
the engine's rule for decisions. **Mark all read** is `read_through` at the
newest update LOADED: an update that arrived after the page was drawn is one
the person has not seen. None of them sends the inbox back whole, which is
what the write they replaced did: a Done in one tab erased a snooze made in
another.

**What the list says when it is empty is derived from the answer**, never
from the row count: nothing answered yet and a refusal draw no sentence, and
"you are caught up" (the person's own `seen_through` watermark exists) is told
from "nothing has reached you yet". With nothing left to open in a list that
holds rows, the pane says **All clear**; beside a list that is empty, whose own
empty state already says so, the pane holds one quiet line ("New updates open
here.") rather than a second empty state.

**Two panes on a wide screen, one on a narrow one.** Both columns are present
from the first paint and the first item not yet dealt with is open until the
reader picks one, so the pane is never an empty box beside a list with
something in it; stepping down the list, or `j` and `k`, replaces history, so
four rows read through one pane are one place the reader has been. Under 640px
of the SCREEN's width the list is the screen and an item opens in its place,
with Back at the start of its byline: on a phone, and beside Ask Crewlet,
whose column can leave a desktop window's screen a phone's width. Whether the
page then scrolls as a whole is the window's: on a phone it does, and beside
Ask Crewlet the screen still fills the window and the one pane scrolls
itself.

**Three viewer states, three sentences.** A reader with no credential, a
reader whose credential no seat claims, and a bound reader. The unbound reader
is a token's session or a service account — never a person, who always holds a
seat — and is told what binding its row to a human seat nothing else holds
would add (`crewlet iam bind`, or a row created with `-seat`). The conditions a person decides are listed for all three; a person's
updates and decisions need a credential bound to their seat, and every write
control is drawn for all three, disabled with the reason.

**Without Worklet the Inbox stays, and says where the rest went.** The
asks and the updates are Worklet's notices, but the seats stopped on a
spent budget and the conditions a person decides arrive under every backend,
so the Inbox is never hidden with Worklet (see [the sidebar](#the-sidebar)).
It draws those, and one line in place of its updates naming where asks and
mentions go instead: "Jira handles work items, so asks and mentions arrive
there.", or that the company runs no work tracker.

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
explains it, and each empty section says in its own words what would fill it. Every
section is a question about work items, so where the company does not run
Worklet, My work is hidden, never locked (see [the sidebar](#the-sidebar)).

**Every figure for one person's day counts one named thing.** The Queue's is
the open work ASSIGNED to the person, and on your own day it is the same
reading the sidebar's My work figure draws, so the two cannot disagree. The
questions put to you have their own tab. And the priorities reading, below,
says above its first row what IT counts, because it is a different list from
the one the tab counts.

**Two sections are the work list, held to the person.** The Queue is
`ItemsView` with the ASSIGNEE locked — its shapes, Filter, Display, scope,
grouping and count line, opening banded by the engine's `due:bucket` — and
Asked by me is the same list with the ASKER locked instead (`asked_by=`
alone: a question is recorded under its asker's own record name, the seat for
a person — every person holds one — and the login for a credential bound to
none, so one
name finds every question they asked and no question accepts a `viewer=`),
opening on every status. A lock is not a chip and not a
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

**Asked of me is the decision list Home draws** (`DecisionList`): asks
grouped under who asked, each with its recommendation, Accept and Change. It
is shared rather than restated, so how an ask is answered is one rule.

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
reorder (anybody above them in the chart, as Worklet defines a lead), and
the page says before the press that the person is told when the reader
reorders.

### A task's page is one reading

`#/worklet/{KEY}` is the approved Issue artboard: a reading column and a rail
beside it, each its own scroller and flush with the sheet (the page asks the
shell for the window's height, as the Inbox does, and gives it back below a
phone's width, where the rail follows the column in one scroller). There is no
header card: the page bar's trail names the project by its name with its key as
a chip ("Worklet › ENG Core platform › ENG-412"), and the bar carries ↑ ↓ "3 of 18"
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
history that stopped there. Each history stays whole while its newest page
moves under a reader who paged back (`lib/usePaged.ts`, which a seat's Turns
tab pages through too): an arrival at the top pushes the newest page's oldest
entry off its foot, so before the list moves the range between the newest page
and the pages held is read and spliced in, and until it has been the list stays
as it was drawn. A gap longer than everything the reader paged back through,
or one a read failed on, drops the older pages instead, so the reader pages
back again from a list with no hole in it.
All | Comments | Agent turns | Changes narrows it.
A change is a sentence about the task ("Jane Founder filed this in ENG",
"CTO assigned it to SWE: “owns the provisioner”", "Jane Founder ticked 1 on
Acceptance — 1 of 4 done"), with a quiet **· Who this reached ›** at the end of
its line opening its routing: the reason each person was woken, whether it asked
anything, and the three different empty answers in three sentences. A comment
is a card with its ask, answer or choice and a **Reply**. A turn is the
artboard's card: "SWE ran turn 1", its phases as checked steps — the one a failed
turn broke in wears a cross in the danger tone, and its pill says "failed in
review" — a pill for a send-back ("sent back for another pass") or a park, what
it did in the agent's own words (a turn that failed or parked before it wrote an
account of itself says "No summary. See the trace." in one quiet line), the reviewer's request on the raised rung with the
caution ink on its mark, the tools it called as chips ("comment_on_work_item ×4"), and under them, on
a line of its own read from the left, its wall time and tokens and **Trace**. The number is the TASK's count of turns,
so the newest card and the cost panel agree. While a seat's turn is running on
this task — joined on the item the engine charges it to, and only while the
seat is `working` — the foot of the timeline is the live row in the working
state's own ground ("SWE is on turn 3 · coding run · for 2m",
with a pulse that is static under reduced motion) linking to the live trace.
The words after the turn are the task card's strip's own, from one derivation
— the round counted from one, as the stepper counts it, and the phase named
("reading context", "reviewing", "coding run") — so the
card, the row and the seat's profile never name different rounds of one turn.
The composer is last: a comment, a reply, or **Ask…** — a question put to one
seat, optionally with two to six options, the one recommended and whether the
answer is the decision or an input to it.

**The rail is the task's fields, each the control that changes it.** Status,
priority, assignee (with **Reassign**, whose dialog asks why), reporter,
project, type, labels (the project's declared set), due and start, and the
estimate, then any custom fields and the record (created, updated, in status
since, filed into). A row a change touched after the task was filed says who
last set it, named as the person the activity names — a person's change is
drawn as the seat the engine recorded it under, and so is the reporter; the
create itself draws no line,
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

`#/company/org` draws the company in the two arrangements Edit org draws a draft
in, and with the same drawing: the design system's tree canvas in its `node`
appearance, elbow connectors on a dotted field, and the switch between the two
under the zoom bar (`chart=`, the builder's own section param, so a link names
the same chart on either screen). What a node LOOKS like is shared
(`ui/orgNodes.tsx`); what it is drawn FROM is not. The **Structure** is the
default: the company at the root, its root seats and top units off it, and
each unit's seats and child units off that unit, seats first. A unit's node
says its type and the key of the tracker project its work is filed under
("Team · ENG"), and its lead stands in a pill along its bottom edge ("Lead:
CTO", with "(inherited)" where it is). The **Reporting** chart is every seat
under its primary manager, the engine's derived `manager`, the line escalation
goes up. Both are built by `lib/orgchart.ts` from the projection this node has
APPLIED, never the builder's draft (a test holds that it imports none of the
builder's model), so between a save and this node applying it the chart has
not moved, and the "Still applying …" note says so. Every reporting
line and every unit's seats are the engine's derived ones; the client draws no
hierarchy of its own. **The field runs to the sheet's edges**, dotted, under
the tabs, as the artboard draws it: the screen's padding and the canvas's own
frame are taken back for this one screen, because every pixel of them was a
pixel of every node. **The chart opens fitted and centred**, and so does Fit to
view: all of it, in the middle of the canvas. On the Reporting chart a person
who reports to nobody and leads nobody is a root with no tree, and the canvas
packs such a node beside the root it follows on the top row; placed after a
founder whose tree already leans right, it pushed the top of the chart further
right, so the chart read as sitting right of centre even with its box
centred. Such roots go to whichever side of the tree centres the top row over
the drawing (`balanceRoots`), a tie keeping the document's order.

**A node says who and what, and its title says the rest.** A seat leads with
the console chart's mark (an agent's own character on a node in its own colour, a
person's glyph in a dashed ring) and its NAME. Under the name an agent's node
says what it is doing in the engine's word, behind the state's dot
("Working", "Needs you", "Stopped", "Idle"), and a person's says "Human seat".
The word is one of a few short ones on purpose: a node is as wide as its name,
and a caption that grew with the engine's state line would relay the whole
chart twice a tool-loop round. That line ("Executing ENG-412", "3 workers on
ENG-405", "Needs you · run parked", "Stopped · budget", "Idle · last turn 24m
ago") is the node's title, and it is read after the node's name. **A seat's
dot shows what it is doing**: the dot and the word are the only state hue on a
node, and an agent's colour is its own look (see "The one rule"). The legend at
the chart's foot counts Working, Needs you, Stopped and Idle from the engine's
`activity`: a seat with no row yet is counted nowhere, and a person never.

**A human seat's card says what holds it**, for every signed-in reader:
`GET /iam/seats` is everybody's (`iam.seats.read`), because who holds each
seat is part of the overview, and the engine masks the listing per reader — a
holder's name and kind to everybody, their person id, login and stage to an
admin and to the holder themselves, and an open invitation to an admin alone
(ADR-0033). Every person holds exactly one human seat for as long as they
exist ([ADR-0032](../../adr/0032-every-person-holds-a-human-seat.md)), and the
company document cannot say who that is, so a human card's third line is
**Held by Jane Doe** (the login where no name is sent), **Invited ·
sam@example.com** (or *Invited*, where this node's keyring cannot open the
address — and to anybody but an admin an invited seat reads **Open seat**, since
the invitation is not theirs to see) or **Open seat** —
in the card's NEUTRAL ink, beside a neutral dot, because who holds a seat is
not something the seat is doing and the card's one hue is state. The whole
chart is ONE read of the seat listing, asked once the viewer has said who this
is, kept current on the directory's own terms (every minute and when the tab
comes back) and read again the moment a directory write anywhere on the page
lands, so a seat invited from the peek is never "Invited" beside a card still
saying "Vacant". On a phone the rows carry the same line. A read that FAILED
is said above the canvas or the rows — **The identity
directory did not answer**, that who holds each human seat (and, for an
admin, Invite and Create on a vacant one) waits until it does, and
the refusal's own words beneath — wherever the chart holds a human seat,
because drawn as nothing, an administrator saw a chart whose human cards read
"Human · availability" and carried no menu, with no word why.

**A vacant human seat is where a person joins.** A person is never invited or
created anywhere else: Settings › People lists, changes and removes
people but adds none, because a person comes from the seat they will hold. So
for an admin a human card carries a menu — the tree's
own pattern, as the builder's node actions are drawn: a strip of icon buttons
for a pointer ("Invite a person to Jane Doe"), which the canvas draws over the
end of the card once it is hovered, focused or selected, out of the tab order
and hidden from assistive technology — and the same items as the card's menu,
which the ContextMenu key and Shift+F10 open on the card itself, since a
tree's items hold nothing focusable. What it offers is decided by what holds
the seat:

| The seat | The menu |
|---|---|
| Vacant | **Invite someone…** and **Add a person…** |
| Invited — an open invitation names it | **Cancel invitation** |
| Held by a person or a service account | nothing: a holder is moved to another seat or removed on their row in People (Service accounts, for a service account) |
| Not answered for — the listing failed, has not arrived, or does not hold the seat yet | nothing, because a listing that did not answer says nothing about whether the seat is free |

On a phone, whose chart is rows with no card menu, the gestures are the
peek's. The seat's peek and its page offer the same three, and the dialogs are
one set (`components/people.tsx`), each about the seat as it was when the
gesture started rather than the live row — the listing is read again the
moment the write lands, and a cancellation reading the live row lost the
invitation it was about, and its dialog with it, before a `202` could say it
was recorded:

- **Invite** — "Invite a person to Jane Doe", the seat stated as a fact rather
  than offered as a choice: an **Email address** and the **access picker**
 , see [People](#settings-the-frame-people-secrets-nodes-and-configuration), opened with every app
  ticked and Admin not, and the note that warns what an unticked app costs
  the person.
  It sends `POST /iam/invitations` with the seat, and the **Invitation link**
  comes back **once**, with its expiry and the sentence that it is the
  credential: it works once, this engine sends no mail, and opening it is
  where the person chooses a login and a password — redeeming it binds them
  to the seat, which the open invitation holds until then.
- **Add person** — "Add someone as Jane Doe": an **Email address**, a
  **Name** and a **Login**, both optional — left empty, the engine proposes
  the login from the address, the same proposal an invitation's screen makes,
  and says which it took; one typed is held to the person grammar under the
  field before anything is posted — and the access picker, on the invitation's
  terms. It sends `POST /iam/people` (`kind: person`, the seat) under a fresh
  operation key, and the person is created ACTIVE on the seat at once. The
  answer names the login the engine took and shows the **Password link**
  **once**: it sets their first password, once, within a week, signs nobody
  in, and until it is used nobody can sign in as them. The key is what makes
  the link survive a dropped answer: the engine derives the person and their
  link from the operation, so **Try again** after an unknown answer hands back
  the same link rather than a second person, and a retry that finds the link
  already spent, revoked or lapsed shows *No link to show* with the engine's
  own sentence naming the way to a new one — a password reset link.
- **Cancel invitation** — confirmed, with **Keep it** as the way out (a
  "Cancel" beside "Cancel invitation" kept the invitation for somebody who
  pressed it meaning to cancel it): the link stops working at once, as one
  nobody issued, and the address and the seat are free again.

Each is one `/iam` write through `lib/iamWrite.ts`, on the terms [People &
access](#settings-the-frame-people-secrets-nodes-and-configuration) states for
every directory write — a step-up asked and the request replayed, a `202`
leaving the dialog only **Done**, a refusal in the engine's words with the
access that would admit.

**A node opens beside the chart**: a seat its seat (`peek=seat:{handle}`) and
a unit its unit (`peek=unit:{key}`). Enter does the same, the arrows walk the
tree, `[` and `]` step the seats in the order the tree reads (pressed on a node
as well as in the peek, since a node's own keys would otherwise read them as
type-ahead), and while the peek is open it follows the node that has focus. A step replaces the address rather than adding to the
history, so Back closes the peek however many seats it walked through. **A new
width is fitted again**: the canvas fits the chart once, and the peek then
takes a share of it — so whenever the canvas's width changes (the peek opening
or closing, the window resized) a chart still at the view it was last fitted to
is fitted again, all of it and centred, as the approved chart draws it beside a
peek. **But never shrunk past reading**: a view the chart chooses on its own —
its first fit and every refit — is drawn at exactly 85% where the fit would
fall below it, centred across on the node the peek is on, or on the root the company hangs
from where nobody is selected, and down the canvas where the kit's own fit
would put the chart, pulled just far enough to keep that seat on it. Only a Fit
the reader presses draws a chart smaller than that. A view the reader moved
(zoomed in to read the nodes, say) stays theirs. Either way the node the peek
is about is revealed (the least pan that
shows it), and a reader stepping in the peek keeps their focus. **On a phone the chart is rows**: a chart of
nodes has no size a phone can read it at, so below the phone breakpoint the
reporting tree is the design system's tree grid, every seat a row indented
under its manager, with the badge and its status glyph, name, kind, place and state line a card
carries — headed by the legend; a row's name or Enter opens the seat. The page
bar carries **Find a seat** (name, handle or unit; the found seat is focused on
the canvas and opened — its list grows past the field so a seat's name is never
squeezed out by its handle and unit), **Edit org**, and **Add seat**, which goes
to the builder for an admin and is held with its reason for
everybody else.
On a phone Edit org folds into the bar's More menu, so Add seat stays in view.
It is the same bar on every section of the Org chart. **Edit org is one
control** (`EditOrgButton`): the chart's bar draws it as a secondary button,
asked the builder's own gate, so a reader who may not write the chart sees it
held with the reason rather than a link to a refusal. Company › Principles
draws none of it: each of its cards carries its own Edit, for a reader who
may write the charter. Company › Schedules is its own row and its bar is its own:
it carries none of the chart's controls.
The section tabs carry the Roster and Teams figures on every section of the
Org chart, as the kit's count beside each name (the figure an object's own
tabs draw). Those sections, and Schedules, open on their work, with no
introduction above it.

**The seat peek answers "who is this, and what is it doing".** A state card
tinted by the state — the line, how long the turn has run, "Turn 2 · coding
run in progress", where the turn number is the task's own count
(`work_item_turns`, asked by the task's id — see [An item's
address](#an-items-address)) — then Reports to, Model (the phase chain from
the org projection, which carries it only to an admin —
every other reader is told Admin access shows it, never that the seat has no
provider — and the model serving the call in flight), one row per capped
budget window labelled by its own period ("Budget today", "Budget this week")
with a meter in the engine's state, or "No seat budget; the company's … applies"
("No seat budget; the company's 60M/day applies") where only the company's
ceilings bind it ("No budget" where nothing does),
Running on (the node that holds the lease and since when for an admin;
every other reader reads what the health push says — this node by name,
or "another node" — which is what the profile's Setup card says too, and no
guarded read is sent), Open work (`work_workload`), where its tools come from
(the projection's `tool_sources`, on the same terms as the chain),
and its goal, drawn as the inline markdown it is written in. A human seat's
peek adds **Held by** — the holder's name for everybody, and for an admin and
the holder themselves their login with, short of active, their stage ("jane.doe (suspended)"; "The service
account `ci:release`" for a machine), "An open invitation ·
sam@example.com", or "Nobody — the seat is vacant", never the card's line,
which repeats the term — or says the directory did not answer, does not list
the seat yet, or is still being read, rather than leaving the fact out. It asks at most three questions,
and for a human seat one REST read of the directory (`GET /iam/seats`), which
every signed-in reader is answered (masked per reader) and which is never
asked for an agent's seat, whose holder is the engine. The open work and the
turn number are Worklet's questions, asked only of a reader holding
Worklet. While the
seat is working the state card ends with **Watch live**, the turn's watch link,
so the chart's node, then that link, is the way from the company running to
a turn running: two presses, with no link nested inside the chart's tree item,
which a keyboard and a screen reader could not reach as itself. **Open profile** and **Message** are the
rail's foot, pinned to its bottom edge — beside them, for an admin, the
seat's gestures as buttons (**Invite** and **Add person** on a
vacant seat, **Cancel invitation** on an invited one), which on a phone are
the only place they are; Message opens the New task sheet with
the seat as assignee and `ask` set — on the project filed under the seat's own
unit, or the nearest unit above it that has one — so the question is filed as
work the seat owes an answer on and the answer lands in the Inbox. Where the
company runs the engine's own chat and you hold Chatlet, **Direct message** sits
beside it and opens a conversation with the seat instead
(`#/chatlet?dm={handle}`), a remark that obliges nothing; it never replaces
Message. Message files a task, so where the company does not run
Worklet it is not drawn, nor is a seat's Assign (see
[the sidebar](#the-sidebar)). **The peek of your own seat is your own seat's page in
small**: Message and Direct message are held, saying it is your own seat — held by the button
itself (`useMessageSeatAccess`), never by the screen drawing it, because the
page held it while the peek drew it bare and offered a person who found
themselves on the chart an enabled Message to themselves — and under the state
card is the profile's **Your account** card, linking to
[Account](#account-the-readers-own-page). No other seat's peek draws either.

**Roster** (`#/company/org/roster`) is every seat as a card, grouped by state, by
unit or flat, and always in NAME order —
never by a field a push moves. A seat the engine still reports and the chart no
longer holds is its own group, "Removed from the company". **Workload** is who
is carrying how much (`work_workload`). On the Roster the bar's **Find a seat**
is the list's filter (`q=`: name, handle, goal or unit, and on Workload name,
handle or unit), one field, where every section of the Org chart has it, rather than a
second box in the roster's own toolbar. **Teams** (`#/company/org/teams`) is the
units nested as the document nests them — the seats above every unit first —
each with its project key, kind, effective lead ("(inherited)" where it is),
headcount, purpose, goals and seats.
A unit's own page is `#/company/org/teams/{unit}`.

### A seat's profile

`#/company/org/seats/{handle}` is the approved Agent artboard: a head flush with the
sheet — the seat's badge (an agent's own character on its squircle, a person's initials in a circle) with
its status glyph, the name, an **Agent** or **Person** pill and, for an agent, the
state in the engine's word ("Working", "Needs you", "Paused", "Stopped ·
budget", "Not placed", "Idle"); under it the line that places the seat
(handle · unit path · reports to its manager) — and the tab strip on the
head's own rule. The trail reads **Org chart › Engineering · Core › SWE**: the
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

**A person's seat says what holds it.** The company document cannot: the
identity directory binds a person to a human seat. So above the tabs a
person's profile says it whole, through the same holding the chart's card and
the peek draw (`SeatHolding` in `components/people.tsx`): **Held by**
the holder's name to everybody — and for an admin and the holder themselves
their login, `jane.doe`, "Held by you, as `jane.doe`" on your own seat, "the
service account `ci:release`" where a machine holds it, with the stage short of
active; or, for an admin, the open invitation that names it — its address, until when, and
that until it is redeemed, cancelled or lapses nobody else can be invited or
bound to the seat — with **Cancel invitation**; or **This seat is open**,
with **Invite** and **Add person**, the two ways a person comes to exist,
opening [the chart's dialogs](#the-org-chart-is-the-company-running). A seat
nobody holds says it as its Overview's empty state, in the column a person's
day and work would fill (its other tabs keep the line above them), rather than
as a notice above an empty column. The gestures are drawn for an admin. The answer is
`GET /iam/seats`, which every signed-in reader is answered, masked per reader
— to anybody but an admin a seat an invitation holds reads as nobody's. A read
that failed is SAID, with the engine's refusal, rather than drawn as nothing —
the gestures live here, and a page without them would say nothing about why —
and a seat the chart draws but the listing does not hold yet is one this node
has not applied the revision for, which the page says. A dialog opened here
OUTLIVES the listing it was opened from: every write reads the listing again,
and a re-read refused then — a node behind its identity log, or the refetch as
the tab comes back from the email the link was pasted into — would otherwise
have taken the open dialog, and the link it shows only once, with it.

**While the seat is working, the page bar leads with Watch live** — the
running turn's watch link (its Transcript, the phase it is on open, or the
coding run a parked turn waits on drawn live above its phases) — and the
**Turns** tab carries the kit's pulsing info dot, the steady-state mark of a
state still happening, with its words read after the tab's name ("Turns,
running now"), because a dot is never the only carrier; folded into the
strip's More, the words are the item's second line. Neither is drawn while
the seat is idle, needs a person or is stopped, and the profile's Overview
card keeps its own **Watch live**.

**Three actions, in the page bar, each a write control held with its reason**
for a reader who cannot make it:

- **Message** opens the New task sheet with the seat as assignee and `ask` set:
  the question is filed as work the seat owes an answer on, and the answer
  lands in the asker's Inbox. Where the company runs the engine's own chat and
  the reader holds Chatlet, **Direct message** beside it opens chat's New direct
  message with the seat chosen — a conversation rather than an ask, so neither
  stands in for the other; it folds into "More" on a phone.
- **Assign task** finds an existing task by search and hands it over against
  the version a read of it returned (`update_work_item{assignee, if_match}`),
  so a task somebody moved in between is refused rather than taken from
  whoever holds it now — or files a new one for the seat. The matches list the
  tasks that CAN move first; the seat's own come last, drawn but disabled and
  marked "already theirs", so a term most of whose matches the seat already
  holds still offers eight it could be handed.
- **Pause** (agents only) asks why, and whether to **also stop the current
  turn**: without it the turn the seat is on finishes first, and only a ticked
  box sends `stop_running`, which also ends a coding run the turn has out (see [pausing a
  seat](../concepts/agent-runtime.md#pausing-a-seat)). A paused seat's profile
  says who paused it, when and why, and offers **Resume**.

"Events" (the [event log](#the-routes) narrowed to this seat) and "Edit in
org" (the builder opened on the seat) are the bar's More menu, on either kind's
profile, and the KIND decides the narrowing. An agent's is `seat=<handle>`,
which the engine resolves to the id every node derives for the seat — never
the name, which a namesake shares. A person publishes nothing under an agent
id, since the engine never runs a human seat, but what the log records about
them names them as its ACTOR, and the name `iam.ActorFor` records for a person
bound to a seat is the seat's handle — so theirs is `actor=<handle>`, an
equality the display name never matched. On a phone the bar keeps
**one** action in view — Message, or Resume on a paused seat — and folds the
rest into the same More menu (Watch live first, while there is a turn to
watch), each disabled there with the sentence its inline control is held with. A notice under the tab strip, above whichever tab is
open, carries what stops the seat or waits on somebody, in the state's own
tone: its last error, a pause, another stop, or a coding run parked on a
question — answerable from the notice.

**The Overview is the seat's week and its charter.** Four figures, all ONE
answer (`seat_activity` with `previous`, every data node's usage rows summed):
turns over seven company days with the change against the week before, the
engine's first-pass rate over the turns it REVIEWED (a week nobody reviewed has
no rate, never 0%) with how many reviews sent work back, the median turn with
its p90, and tokens — the seat's capped window nearest its ceiling, over that
window's ceiling ("of 3M budget"), or the seven days' tokens where the seat
has no ceiling of its own — "company cap 60M/day +1" (and "no seat cap" on hover) where the
company's own budget caps a window, because that ceiling binds every seat (one
line in the tile, the whole sentence on hover). A shortened token count carries
no zero fraction: `60M`, never `60.0M`. The **current turn** card shows where the turn is — Context, Execute
("coding run" once the turn parks on its run), Review; there is no
delivery step, because whether a turn reached anybody is decided as it
closes — the turn's number on its task and the task's title (read by the
turn's own task, by its id — see [An item's address](#an-items-address) —
never looked up in the card beside it), its
elapsed time and, for a reader the seat's trail is open to — an admin, or
whoever leads the seat (`audit.seat.read`) — tokens so far, and the phase's calls, each with the engine's own start time and
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
(the model chain and tool grants from the org projection, which carries both
only to an admin, because both are derived from the
seat's configuration; anybody else is told Admin access shows them rather
than that the seat has none; the sandbox, placement and workers from the
company document, read by an admin — or, for the lead of the seat's
unit, from the seat's own entity and its home unit's
(`GET /config/roles/{handle}`, `GET /config/units/{key}`), which the engine
admits a lead to — and said as unread with the access that reads them for
anybody else, who is never asked, or as absent when the active revision names
no seat by this handle; the node
holding the seat now — the fleet's lease, asked only of an admin, and for
anybody else what the health push says, this node by name or
"another node", which the seat's peek says too; and Edit), **Memory** (the
holder's counted totals and its newest reflection, asked only of a reader
the seat's trail is open to, and for anybody else a sentence naming Admin
access and that whoever leads the seat reads it too, rather than a refusal
drawn in the side column) and **Schedules**. A person's
Overview is **Their day** — or
**Your day**, with the way to your Inbox and My work, on your own — with the
unread notices waiting on them (the inbox's own count, the sidebar badge's
reading on your own day, "50+" past a page), their priorities — the engine's
count of what on the list is still open, the figure My work and their Work tab
read, never the stored list's length, which still names a task they finished
until their next reorder — and who set them, and pinned views, for the person
and whoever leads them, holding Worklet, and an admin, and withheld with that
sentence for anybody else; their open
work, for a reader holding Worklet; and About. On your own seat the whole page speaks to you, not only its
day — the holder line says "Held by you, as `jane.doe`", the work card "Nothing
open is assigned to you", the Work tab "What you mean to do first" and "Waiting
on your answer" — and **Message** is held, saying it is your own seat, because
a message files an ask of the seat that only you could answer. The hold is the
BUTTON'S rule, so the bar's Message, its phone "More" entry and the seat's
peek are held in one sentence. **Assign task**
stays: taking a task yourself is a hand-off like any other. And the side column
leads with **Your account** — the seat is not the account, and a person who
opened their own page to find their password or second factor found neither —
linking to [Account](#account-the-readers-own-page); the card decides for
itself whose seat it is on, so the seat's peek carries it too, and a
colleague's seat draws no such card.

**Turns** is the seat's own turns (`turns{seat}`, the seat's TRAIL, which the
engine answers to an admin and to whoever leads the seat, `audit.seat.read` —
for anybody else the tab is drawn and locked, "This seat's turns needs Admin
access", with the sentence that whoever leads the seat reads it too, and
nothing is asked — answered from every node, over the thirty
days the event store keeps, a page of fifty at a time with "Load older turns"
following the engine's cursor, and a count of what is loaded — "50+" while an
older page exists), with **Running now** leading the tab so a turn in flight is
on screen as it opens, and the settled transcripts after the list under their
own heading, **Transcripts · newest first**. A turn under Running now opens
the phase it is on, as the trace's Transcript does, and STAYS under Running now
for as long as the seat's own turn record says it is on it — between phases
and while parked too, the rule the turn's page reads — because decided on "has
a phase in flight" the card left for the settled list in the gap between one
phase's record and the next phase's start, came back remounted, and shut the
transcript the reader was following, at every boundary. Its card then moves
once, as the turn ends, and opens with the phases the reader watched live
still open. A running card's header gives ONE duration, how long the turn has
been running; the length across its phases is a settled card's, where it sat
beside the running clock as a second unlabelled figure. A phase that finishes while the
tab is open reaches its transcripts off the push, matched on the seat's own id
as Live's recent phases are — never on its role name, which two unit seats
stamped from one template share. A row for a turn still running says what the seat is doing
and what it is on, from the push ("running — reviewing · round 2 of 4",
ENG-32), and draws its iterations and tokens as not settled rather than as
zeros — `#/live/turns` does the same. On a phone a row's summary takes two
lines with its item under it, and a settled turn has no State line at all. And
**Where its tokens go**, its week by phase and by model — the buckets that spent
some, since calls that failed before a model answered recorded none and were a
bar of nothing named "unknown" — with the way to all of its activity on Live. **Memory** is the seat's TRAIL too, which the engine answers to an admin and
to whoever leads the seat (`audit.seat.read`, ADR-0033) — so the tab asks
nothing of anybody else and says, as Turns does, what would open it, rather
than sending two questions to be refused and drawing the refusals as a seat's
memory. It is read from the node HOLDING the
seat, whose copy is the one kept current, and says which node answered: the diary, the
episodes (each outcome in the reviewer's own tone — `failed` is red here as on
a turn; each turn's **Woken by** — the waking event's label, with what it was
asked under it — apart from **What it did**, its account, each an opening
marked with `…` and a **Read all** that opens the turn whole where the text is
longer than the list carries, because the label
under "What it did" said every chat turn had done "Message from Ana"; and a
compacted row as how many turns it stands for, their pattern and what varied,
and how many of them ended done), the skills it taught itself and who it has worked with (when last,
relative, as every list on the profile says it), each header
the holder's total ("Latest 50 of 142" where the list is a page of it), and the
**Conversations** it holds a ledger in (`conversation=` opens one) — each
thread's KEY on a line of its own, since it is the thread's only identity, with
its turns and when it last moved under it, and a Worklet task's uuid cut to its
head (`work:task:4d631f6d`) with the whole key on the row's title. Choosing a
thread shows it: beside the list (a wide screen column) the list scrolls
inside a box bounded by the scroller's view, handing the wheel back to the page
at either end, and the thread's turns stick under the page's top; stacked
on a phone, the turns are scrolled to and their heading takes focus. Each
turn's trigger reads as plain words, its markdown stripped. A seat no
node holds says why it shows nothing. **Settings** is the seat's half of the
company document, read by its handle — the seat and its home unit, whose
`mcp_env` a seat inherits per variable — through the same one read the Setup
card makes (`useSeatSetup`): the company document for an admin, the seat's
and its unit's own entities for the lead of its unit, and nothing asked for
anybody else, whose tab names the access in the frame's words ("Reading this
seat's settings needs Admin access", and, for a seat in a unit, that whoever
leads it or a unit above it reads them too) —
never "No company configuration is active", which is said only of a read that
came back with no document, and which a reader nobody asked for was told
while a company ran; every other outcome — still out (waiting on who the
reader is included), refused naming the access, absent — has its own
sentence rather than an empty value. What it draws is
identity and contacts, the model and budget with each capped window's live
meter, and a seat's tool credentials by server and **variable name only**: no
value the engine sent for a credential reaches the page, not the reference,
not the mask. A person's has no model, budget or credential card, because the
engine refuses all three on a human seat. Every value there is changed in the
org editor ("Edit in org").

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
| `seat`: the Inbox's "Needs you", and Home's "N conditions need a look" | the seats' own state, the token budgets | a person decides these: raise a ceiling, resume a seat, look at one that failed |
| `engine`: the sidebar's engine panel and Home's status sentence | the engine and this node's link to it | a fact about the product the reader is looking at, on every screen, not an item in anybody's queue |
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

`lib/markdown.ts` renders a CommonMark subset to React nodes. The grammar it
reads is the design system's (`parseMarkdown` and `markdownInline` from
`@crewlethq/ui`), the same grammar the rich editor reads a body by, so what a
writer edits and what a reader sees cannot disagree about where a list ends or
what a backslash escapes. It is written in-tree rather than taken from a
markdown library for the reason the rest of this engine writes its own small
pieces: a library's real surface is its HTML passthrough and the sanitiser
that has to follow it, and this renderer needs **neither**. It emits no raw
HTML under any input, so there is no `dangerouslySetInnerHTML` on the path and
nothing to sanitise.

Beyond the usual blocks it reads three things a page or a doc uses: a
**backslash escape** (`\*` is an asterisk), an **escaped pipe** in a table
cell (`a \| b`, a pipe of that cell, inside a code span too, as GitHub reads
it), and a **callout** in GitHub's alert syntax, a quote whose first line is
`[!NOTE]`, `[!TIP]`, `[!IMPORTANT]`, `[!WARNING]` or `[!CAUTION]`, drawn as a
tinted note with its tone named on a line of its own.

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
- **A picture is shown from this origin only.** The dashboard's policy loads
  images from its own origin (and `data:` and `blob:`), so an image whose
  allowed address is on another host renders as a link to it, labelled by its
  alt text, rather than as a picture the browser refuses; loading it would also
  tell that host who reads the page and when. The rich editor shows the same
  picture as its alt text (`shownImage` in `lib/markdown.ts`). A doc's pictures
  are files of its version, fetched with the reader's credential, and are
  unaffected. A link inside a link's label is that label's text.

An outbound link opens in a new tab with `rel="noopener noreferrer"`, and a
screen reader is told it does; an in-app `#/` link does neither, because it
*is* this application. A task
list's checkboxes are disabled — this surface performs no writes, and a box a
reader could tick would report a change that never reached the page.

**A model's words are markdown too**, and this page used to say they were
not. A round's speech and a coding run's report were a `pre-wrap`
block on the rule that a model's line breaks are load-bearing and it does not
write markdown — and a round that wrote a fenced JSON block printed three
backticks, a language tag and a wall of braces. The line breaks *are*
load-bearing, which is why a model's words are rendered with two options a
document does not take (`MODEL_WORDS` in `lib/markdown.ts`):

- **Every newline is a line break** (`breaks: "hard"`). A document's single
  newline is a soft wrap somebody typed to keep a source line short, and
  CommonMark reads it as a space; a model writes one fact per line and means
  each one, so read softly its list of facts became one run-on line.
- **A heading is a styled line, not a heading** (`headings: "text"`, the
  `.md-heading` block). A transcript item is not a section of the page — the
  reason a tool row's disclosure renders no heading either — and a model's
  `## Summary` put an `h2` into the turn page's outline between the page's own
  sections. It keeps a heading's weight and air at the body's size, since a
  transcript item has no outline of its own to rank.

Everything else is the document renderer's, unchanged: raw HTML is text, the
link allowlist holds, and a fence is a block holding the exact bytes — a round
still streaming can end inside one, and what has arrived is shown as code. An
attempt a provider abandoned mid-answer stays plain dimmed text: it is evidence
of a failure, cut wherever the provider died, and never becomes the round's
answer, whereas streaming text is about to become it and is rendered so the
block does not reshape at the moment it commits.

**Emphasis follows CommonMark's delimiter-run rules**, and for a long time it
did not: a `*` or `_` opened wherever it stood and closed at the next one. A
document rarely noticed; a model's words and a prompt do, because they are
full of snake_case names nobody put in a code span — "call submit_work with
the result_summary" rendered as "call submit*work with the result*summary",
both underscores eaten, and a tool catalogue's `- list_mcp_server_tools:` line
named a tool that does not exist in the one view whose job is showing what a
model was told. A run now opens only when it is left-flanking and closes only
when it is right-flanking, so `*foo *` is not emphasis; an underscore inside a
word is a letter of it (`snake_case_name` stays itself, `_word_` and
`__init__` are still emphasis); and an asterisk inside a word still opens and
closes, as the spec says (`foo*bar*`). One walk serves the renderer and the
plain-text flattening alike, over the whole text, so a run's neighbour is the
character really beside it.

`.prose` on its own stays a `pre-wrap` block, for text whose every space and
break is the record — a coding run's activity log. Rendered markdown carries
`.prose.md`.

**A fenced block wraps rather than scrolling**, and it took a wrong class name
to notice it did neither. The renderer emitted `class="code plain"` on every
fence, two generic names (a table header once spent `plain`) and neither of
them a recipe for a `pre`. So a fenced
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
its heading levels describe, and `headingLines` for where each heading's line
starts in the source — for the one surface that needs a document's outline and
its source both: a phase prompt, which the transcript outlines, renders one
section at a time, and hands back byte for byte on the other view (see rule 11
below). A walk that rendered as it split could give neither back. All three
read the document through ONE fence-aware walk, beside the renderer, because
what a heading is and where a fenced block suspends the grammar are decisions
the renderer has already made — written again next to its caller, the copies
would drift exactly as `textcut`'s four copies and `whsec`'s two did, and the
clause that would go first is the fence.

A page's history shows **what a save changed**, not only what one version
said. `lib/diff.ts` is a line diff over the two revisions — Myers, by line,
with no word-level refinement inside a changed line, because these documents
are rewritten in paragraphs and a word diff makes a small edit prettier and a
large one unreadable. Unchanged runs collapse to a row saying how many lines
they stand for: a gap silently closed makes a document edited at both ends
look like one rewritten in the middle. "This save did not change the body" is
its own answer, because a title, a label and a move are saved beside a body
and a pane of unmarked lines reads as one that failed to load.

### What prose names is drawn as what it is

Operator-facing words are full of things a reader has to stop and decode:
`gophrase/backend#14` is a pull request on GitHub, `#ffe100` is a yellow,
`~design-review` is a Mattermost channel, `#launch` is a chat room, `GP-66` is
a task. Every surface `lib/markdown.ts` renders (a page, a description, a
comment, the Inbox, an Ask Crewlet answer, a transcript) draws each with the
mark that says what it is, a little below the text's size and just before its
words (`InlineRef` from `@crewlethq/ui`), and a seat's handle by its words
alone:

| Written | Drawn |
|---|---|
| `#ffe100` in words, or a code span that is one colour (`` `#fff` ``, `` `rgb(255, 225, 0)` ``) | A swatch before the value |
| A web address | The mark of the app it belongs to (or a globe), and the reference it is (`gophrase/backend#14`, `Message in gophrase`, `~design-review`, `MOB-12`, `DES page 123`) in place of the whole address, which the link's title keeps |
| `[label](address)` | Its label as written, after the mark of the app its address belongs to |
| `owner/repo#14`, `group/project!7`, `owner/repo@b5362d2` | The code host's mark, linked to the company's code host |
| `owner/repo`, under an owner the company works in | The code host's mark, linked to the repository |
| `#11`, where its message places it | The code host's mark, linked to the issue or pull request |
| A SHA after a reference ("at b5362d2") or after the word *commit* | The same, linked to the commit |
| `GP-66` | The task glyph, linked to the work item |
| `#launch`, a chat room the reader may read | The room's mark, linked to the room; `#e2e` and `#2026` too where the company has rooms of those names |
| `@miles` | Its words, linked to the seat |

`lib/inlineTokens.ts` holds the recognition rules and the reasons for each.
Each rule is a guess that can go wrong in two directions, and both are
refused:

- **Too eager.** A colour in words needs a digit and a letter, so `#123` (an
  issue) and `#facade` (a hashtag) are words; a colour inside a longer code
  span (`color: #fff;`) is code; a SHA needs a reference or the word *commit*
  before it, so `deadbeef1` is a word; a key needs its project to be one of
  the company's, so `ISO-8601` and `UTF-8` are words; a handle needs a seat,
  so `@nobody` is a word and an email address is never a mention; a bare
  `owner/repo` needs its owner to be one the company's code hosts name, so
  `and/or`, `src/app` and `2024/10` are words; a `#name` is a room only
  where the reader may read a room of that name, and a room's name wins the
  colour or the issue number spelled the same, so `#e2e` and `#2026` are rooms
  where the company has them, and colours and numbers where it does not. A
  room the reader cannot see (a private room they are outside, or a company
  whose chat is a vendor's) reads by those rules exactly as a vendor's channel
  does, and a code span holding just a room's `#name` is code; and an address
  swallows every key and colour spelled inside it.
- **Too trusting.** A mark is chosen from the address a link GOES TO, parsed,
  and never from its words: a link labelled "github.com/…" that goes
  elsewhere carries no GitHub mark, and a look-alike host
  (`github.com.login.example.net`, `github.com@login.example.net`) is
  nobody's. An app is recognised at its public host exactly (github.com,
  gitlab.com), under its own domain on a dot (`*.slack.com`,
  `*.atlassian.net`), or at a host the company configured for it. A link's
  label is read as its words and never as a token, so
  `[https://github.com/a/b/pull/1](https://login.example.net)` draws no
  GitHub mark. A label built from an address names what anybody could have
  chosen only with its site: an Atlassian Cloud tenant the company did not
  configure is said after its key or page ("DES page 123
  (evil.atlassian.net)"), a page is named by its space and id and never by
  the title its address carries, a Mattermost channel only by a name
  Mattermost allows, and a path segment that decodes to a slash
  (`a%2Fevil`) or that no code host allows labels nothing, so the address
  shows as its host and path.
- **Too slow.** Every pattern runs in time linear in its run, since a
  comment an agent wrote is rendered by every reader, and a run is read once
  rather than on every render of the surface around it.

**The company's hosts reach every reader, and nothing else of its integrations
does.** A self-hosted GitLab, an Enterprise Server, a Mattermost or a Jira
site lives at an address no rule can guess, so the frame reads `link_hosts`
once ([API Endpoints](api-endpoints.md)): a kind and a base address per
configured app, which every reader may ask for, where `integrations` is an
administrator's. A code host's row also names its owners (the organizations or
top-level groups the company works in), which is what makes a bare
`owner/repo` a repository, and it carries no repository, project, team,
credential or health. A link to a code host is built only to a repository
under an owner its row names: `attacker/exfil#1` keeps GitHub's mark and links
nowhere, since an owner and a name are the writer's words, and a code host
whose row names no owners (a company that configured no organization, group or
repository) links no reference at all. With both GitHub and GitLab configured,
a reference goes to the host whose owners hold it, and one neither or both
hold is left as words. On GitHub a repository is exactly `owner/repo`, so a
longer path is words. With no code host configured, `owner/repo#14` keeps
GitHub's mark and links nowhere. The project keys a key needs are the reader's
own `work_projects`, read once by the frame where Worklet is served to them, and
the rooms a `#name` may be are the reader's own `chat_room_names`, every room
they may read, joined or not, read once by the frame where Chatlet is served
to them as a seat, with a room this tab has seen a live frame for since.
No mark is ever fetched from anywhere: every mark is the design system's.

**A bare `#11` is placed by its message.** `#11` alone is a list's number, a
rank or an issue in some repository, so it is a reference only where the
message it is in says which: the same sentence binds it (`#7 in
gophrase/frontend`, `gophrase/frontend PR #7`, read as one reference); the
message names that number in full (`gophrase/backend#11`); or the message
names exactly one repository. Anything else leaves it words. The trade-off
is chosen: in a message that names one repository, a rank or a heading's
number ("our #1 priority", a heading "#2 Rollout") is read as an issue in
it too, as GitHub's own autolinking reads one in that repository's
comments, rather than guess at the words around a number. A message is
one comment, one page or one answer, and an ask's question, context,
reasoning and options together (`TokenScope`), so the Inbox's "the #11 PR
body" is placed by the `gophrase/backend` its context names. A render is part
of a scope only when it says so (`scoped`): a reply drawn under an ask is its
own message, and its numbers are placed by its own words. A number with a
leading zero, inside a word, a path or an entity, or in code is never one.

**A token is a link only where a link may be.** Inside a control or a link (an
Inbox row, an answer's option, a link's own label) a link would be invalid or
steal the press, so those draw marks and swatches alone (`renderTokens`).
Where the words were written by an agent or a model that may have read
anything (Ask Crewlet's answers, the Inbox and a coding run's question,
`links: "checked"`), a link
is live only where the dashboard built it, from parts it checked, to a place
that is the company's: a key's, a room's and a handle's routes, a reference
on the configured code host, and an address on a configured host rebuilt from
what it names, with no query or fragment. Any other address there is drawn
inert, as written and with the host it names, readable and copyable and never
a control, because `https://evil.example/?q=<what it read>` is how an
injected instruction would carry words away. An address an ask cites as
evidence is the same: a button only when it is rebuilt on the company's host,
labelled by the dashboard with the reference it is and its app's mark, and
otherwise the address as written, never the asker's label ([ADR-0061](../../adr/0061-a-read-is-named-in-the-answer-never-drawn-as-a-card.md)).
A chat message is read the same way, whoever wrote it, with one addition
(`links: "own"`, `CHAT_WORDS`): a page of the dashboard's own (a `#/` route,
or the whole address a Copy link writes on its origin) opens in place, rebuilt
from its path alone (`lib/appRoute.ts`), so the task or page a message is about
is one click away and no query a writer chose rides along. Ask Crewlet's
answers never take it: their one way to move the page is an Open in app card
the engine built. A message's links (`routes/chatlet/MessageLinks.tsx`): a task, a page, an artifact or a pull
request the reader may open unfurls into a preview the engine resolved as the
reader (`chat_unfurls`, with an address the engine built), an address of an app
the dashboard knows unfurls into its mark, name and reference, every other is
the address token, and one whose scheme the allowlist refuses is its words.
A coding run's question is read so wherever it is answered: its Answer dialog
(`components/writes.tsx`), opened from Live › Runs, a seat's page and every
decision row, reads the run's context and reasoning `checked`.
An outbound link a token makes opens a tab with `rel="noopener noreferrer"`
and says so to a screen reader, as every outbound link in rendered Markdown
does.

**The caller reads first.** Ask Crewlet's record chips
(`RenderOptions.prose`) are drawn before any token is read, and tokens are
read only in the words the chips left, so a record the turn read stays its
chip; a search's highlight marks the words left after that.

## ⌘K: search, answer, act

The command palette is one box for the whole product: where to go, what the
company knows, and the three changes a person most often makes from a search.
It is the design system's `CommandPalette` — the combobox, the scope tabs, the
answer's live region and the key legend are the kit's — and `app/palette/` is
what it searches and what a row does.

### Six scopes, three sigils

A launcher with one index answers "where do I go", and five questions do not
fit that shape, so each is a tab under the field:

| Scope | Searches | Sigil |
|---|---|---|
| **All** | screens and **Switch project…**, a pasted event / trace / turn id, three projects ranked as the switcher ranks them, three tasks and three pages (two of each under an answer), agents, people and teams, the tools the company connected (never the engine's own agent tools, which Settings › MCP Tools lists), **Find “…” in artifacts**, which carries the term into Artifacts, the three actions, the answer's remaining sources, and **Search all work for “…”**, the full ranked search; when nothing is typed, recents, then **Running now**: a row per seat working as the palette opened, oldest first, its badge, its name and what it is doing ("Executing ENG-412"), each going to the turn's watch link | |
| **Tasks** | `work_search`, hybrid, eight hits, and a row to the full search screen | `#` |
| **Pages** | `knowledge`, the company's one knowledge backend | |
| **Artifacts** | `artifacts` with the term over everything the reader can open, eight hits, each row its kind | |
| **Agents** | the org chart — the engine's own name tiers first (`colleague`), each seat's status glyph from the agents push, then people and teams | `@` |
| **Actions** | the per-browser commands (Change theme…, a step listing the themes in their groups, which a query narrows by name, tag or group, whose highlight previews behind the palette, whose Enter applies and closes with an Undo toast, and from which Escape or an empty Backspace goes back to Actions; the mode; density; copy link, the one place a page's address is copied from, a refusal said in a toast; sign in as somebody else; clear recents; the key legend; Switch project…) and the three changes; the legend names who they are made as | `>` |

`Tab` and `Shift+Tab` walk the tabs from the field, and a sigil typed at the
start selects its tab and leaves the box, so the tab row — not a character the
reader has to know is special — says where they are.

**A screen is found by its name, then by what it holds.** A reader names a
place by what is on it as often as by what it is called — "password", "2fa",
"two-step", "sessions", "my account" — and only the word "account" found the
reader's own Account, while the hint under its row named all of them. So a
workspace or a section declares, once in `nav.ts`, the words a reader looks
for it by (`keywords`), and `DESTINATIONS` carries them. Every LABEL match
ranks above every keyword match, however good, so a screen called what was
typed is never pushed under one that merely holds it; a query of several
words also matches when each word starts a word of the field ("two step"
finds "two-step verification"). The hint's prose is never searched: it would
offer People for "token", which manages somebody else's.

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
palette's (`work_search`, `knowledge`, `colleague`; a picker holds two;
Artifacts asks `artifacts` alone), so the screen under it keeps one. That is
why All offers a row into Artifacts rather than asking the listing itself. The company's projects (`work_projects`) are read
once as the palette opens, before a term can be typed, so they never contend
with the three. A term longer than the engine resolves to a colleague (200
bytes — a pasted log line) is not sent to `colleague`, and the chart's own
matching answers it; the cap is `COLLEAGUE_QUERY_MAX` in the contract, held to
the engine's by a Go gate. A term past the engine's search bound (400 bytes —
a pasted passage) asks neither search nor the answer, and the lead says so,
naming its size and the limit, where the answer would be: sent, all three
would be refused, and a list that lost its tasks and pages with nothing saying
why reads as nothing matching. Worklet's search and Doclet's do the
same with a phrase in their address, and so do the two pickers that find a
task — handing one over and linking one — in place of their list, and the
chat screen's search and its room finder, whose `chat_search` takes the same
bound. Every `work_search`, `knowledge` and `chat_search` read is asked
through one hook,
`useSearchQuery` (`lib/useSearchQuery.ts`), which applies the bound, says why
and holds no answer from a shorter phrase under one it never sent; a source
test fails on a ranked search asked any other way, so a new picker cannot skip
it. The bound is `SEARCH_QUERY_MAX` in the contract, held to the engine's by a
Go gate, and no search box carries a `maxLength` — that is the same cut, made
by the browser on a paste.

### The answer, and when it spends tokens

A term that reads as a question — **three words and twelve characters** —
gets an answer from the company's own pages and tasks above the rows, written
by `answer_knowledge` as the person asking. Every answer is a model call
charged to the company's budget windows, so it is asked only when:

- the scope is All or Pages;
- the question is within the search bound (400 bytes), since it is the search
  text and one past it is refused;
- typing has paused for **800 ms** (a typist's gap is 150–250 ms, so this is a
  question finished rather than one being typed);
- this browser may ask: somebody signed in and bound to a seat, on an engine
  that answers;
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
the highlight. The answer ends with a footnote in **tokens** ("1,440 tokens,
charged to the company", or "Cached answer, no tokens spent"), never money. An
anonymous reader gets one line instead (*Sign in to ask.*), and a reader bound
to no seat (an API token's session, or a service account) is told *Asking needs
a seat. Ask whoever manages people here to bind your login to one.*, because
the answer is written by the asker's seat's own model and the engine refuses it
to anybody without a seat whose model it can run on. A person signed in as
themselves always gets the answer: every person holds a human seat.

### Three changes, made as you

Each is a `useAct` write, made as the principal the session resolves to
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

An empty palette lists every place **under the sidebar's own headings** (Go to,
Company, Workspace, Settings and Account), so the tree the sidebar draws is the
palette's too, and a workspace's entry opens where its row does: Settings on
the first section the reader may open (`landingPath`), never on a lock.

An empty palette offers **recents** — the last few objects this reader opened,
most recently visited first, per browser AND PER READER: recents and stars are
kept under the principal the tab is read by (`lib/reader.ts`), because one
browser is often several people and a single list drew the last person's
recent and starred titles in the next person's palette and sidebar. A tab that has
not learned who reads it draws and keeps neither. And a key of one's own keeps a
list out of the next person's sidebar, not out of their browser, where every title
in it is one read of `localStorage` away — so a **sign-out deletes every
reader's recents and stars**, and a sign-in deletes every reader's but the
signing-in person's own: the session is the browser's, one person at a time, so
a list whose reader is not the one signed in belongs to somebody who has left.
The same person signing in again after their session lapsed keeps theirs.
Objects only: anything the sidebar
already lists is left out, because a recents list repeating the
navigation beside it costs a reader a scan and tells them nothing. The label
stored is the one the **screen** resolved, which lands a render after the route
— so a recents row says what a turn did, what a trace began at, what a coding
run was asked for, and falls back to the id only where nothing has named the
object yet. See [A screen publishes what the chrome needs](#the-frame).

**One reader, so one order: most recently visited first.** The palette is
opened fresh, ranks what it offers and closes, so it has a re-sort boundary and
"where I just was" is what an empty query should put first; the list is stored
that way, and at its cap of eight the bottom row — the place nobody has opened
in longest — leaves. The sidebar keeps no recents: a place a reader keeps on
purpose is a star. A route no workspace owns is not remembered, and a stored
row whose route this build no longer has is dropped when the list is read.

**Running now sits under the recents, and holds still while the palette is
open.** Under them because "⌘K, Enter" is a reader's way back to where they
just were, and a group of running turns above Recent would have turned that
habit into "open whichever turn has run longest" whenever the company is
working. Still because the kit holds the highlighted option by its INDEX, so a
group that gained or lost a row on an agents push — twice a tool-loop round —
moved every row beneath it, and Enter opened a row nobody had highlighted. The
set of turns and their order are read once, as the palette opens (the same
re-sort boundary the recents keep), and later pushes reach only a row's words:
a turn that ends while the palette is open keeps its row, still opens that
turn, and says **Finished**; a turn that starts is offered by the next ⌘K.

**And a name is never replaced by an identifier.** Every screen publishes its
object's name a render after the route, so the first write of every navigation
carries the raw segment — a uuid for a turn — and the second carries the name.
`remember` takes whether a screen supplied the label, so a revisit never
downgrades a named row to its id. A place nothing has EVER named still stores
its id, because an object with no name has that and nothing else.

A command that would change nothing is not offered: the list omits the mode
and the density already in use, because a control that says
"switch to dark" while the page is already dark does not know what it is
looking at.

**It closes when the route changes**, the way the workspace drawer does.
Picking a row closes it on the way out, so what this covers is every other way
the route can move while it is open — Back, Forward, a phone's back gesture, a
restored history entry. `Mod+K` closes it from its own field. The step-up
confirmation is deliberately not in that rule: it is holding a refused request
open until the reader answers it, and a navigation answers nothing.

## Ask Crewlet: a panel of the frame

Ask Crewlet answers questions about the company and prepares
changes for the person to approve, acting strictly as the signed-in person:
it reads only what they can open, and nothing changes until they approve it.
The engine's half is its own section of the docs; what the dashboard owns is
where the panel stands, what it is told about the page, and how a turn is
drawn.

**It is a panel of the frame, never of a screen.** The **Ask** button ends
the page bar (above the panel it opens), carries Ask Crewlet's spark mark
and the `Mod+.` hint, and stays inline at a phone's width as an icon button.
An open artifact, which draws its own head, places the same button at the end
of its bar.
It is never hidden: where the reader may not ask it is drawn disabled with the
engine's reason (`viewer.assistant`: turned off, no model, a session that is
not a person's, or access that does not include it). While the panel is
closed, a dot on the button says an answer is on its way or a change waits on
the reader.

**The frame decides its shape by arithmetic, as it decides the peek's**
(`app/layout.ts`): a third track of the frame, mirroring the rail, where the
window holds the page's sheet beside it with the list at its floor (1168 px
on a screen and 1404 beside Settings' column, at the normal density); over
the page's end edge below that, leaving the page beside it live, with no veil
and no Tab trap; and the whole window on a phone, as a modal. Its
track is one more term of every other width, so a peek beside it becomes a
drawer first (both are columns only from 1588 px) and Settings' column folds
sooner. It rests at 420 px and is dragged between 360 and 640, from the
pointer or the keyboard, capped where the page keeps its floor, and kept per
browser (`crewlet.assistant.width`). It stays open across a navigation, and a
reload brings back what the tab had (`crewlet_assistant_tab`, in the tab's
own storage, which the hand-over to another person empties).

**What a page tells it is identifiers, never a record's content**
(`app/assistant/context.ts`): the screen, its parameters and query, the
records in view by kind and id, what is selected, a revision, and the draft
the person has open with its values (below), which exists nowhere a tool
could read it. A screen
that knows more registers it with `useAssistantContext`, along with the page
actions it takes from Ask Crewlet (each parsed, gated and applied by the
screen's own code). The page goes with a question as a chip in the composer,
named by the last two crumbs; removing it sends the question with no page
until the reader adds it back or opens another page. The drawer and a screen
talk over a typed module-scoped bus (`app/assistant/bus.ts`), never
`postMessage` or a DOM event, which an artifact's frame or any script could
reach.

**A page action is carried out exactly once** (`app/assistant/chunk/pageActions.ts`).
Every tab open on the conversation hears it, so the tab it was meant for (the
one that approved it or asked the question, or else a tab showing the very
page it was prepared against) claims it first (`PUT /assistant/actions/{id}`,
`applying`), and every later claim is answered `409 action_already_claimed`.
A claim nobody answered applies nothing. The screen then applies it through
its own parser, gate and setters with a fresh look at the page's revision,
and the outcome goes back on the same claim, so the turn hears it as the
action's result. An approved change shows its outcome on its card; a pure
view change the screen declared needs no approval (D11) shows a receipt in
the thread, with Undo while the page still shows what the change made of it.

**The page behind the drawer reads every change Ask Crewlet applied.** Each
server write an approval lands is followed on the turn's stream by a
`context.invalidate` action naming the domain and the position its record
landed at, where the route answered one, and arriving all the same where it
did not (a brand adopted, a seat changed through the configuration). The tab
raises its read floor for that domain, exactly as a press of its own would,
and asks every live question again, quietly: each `useQuery` and each
`useRest` read (`protocol/session.ts`'s `EVERY_QUESTION`). No table maps
routes to the screens they move, so none can drift.

**What each screen offers** is one id per change, the same list the engine's
catalogue holds, each parsed, gated and applied by the screen's own code over
the vocabulary its own controls offer (`app/assistant/urlAction.ts` builds the
ones that are address keys). **Each declares what it takes** (`args`, sent as
the page context's `action_args`), built from the declaration its parser
reads: `urlArgs` from the same keys, the Budgets ceiling from the seats and
windows it draws, the org builder's operations from the vocabulary its parser
reads, and `draft.set` from the open draft's editable fields. The engine holds
every proposal to it before a card is drawn, and
`app/assistant/actionArgs.test.ts` feeds every declaration's examples and
listed values through the real parser. **Each also states what it depends
on** (`tracks`: the address, the selection, the data), and its own revision
(`action_revisions`) is what a proposal is bound to and a claim reports: a
view change tracks the address without the peek, `draft.set` the draft, and
the Budgets ceiling only the screen, since its parser checks the seat and the
window again when it applies. Pure view changes apply at once: Home's window
and feed (`home.range.set`, `home.feed.set`), the work list's filters, Clear
and arrangement (`work.filter.apply`, `work.filters.clear`, `work.arrange.set`,
never widening the person `#/me` is about), a project's lens
(`project.lens.set`), a task's activity tab (`item.activity.set`), Spend's
window and split (`spend.range.set`, `spend.group.set`), Live's and the Turns
list's filters (`live.filter.apply`, `turns.filter.apply`) and Doclet's
search (`knowledge.search.set`). Changes to a draft ask for approval and write
into the draft only, saving staying the person's: the open draft's fields on
any screen that declares one (`draft.set`, below), the org builder's draft
(`org.draft.apply_ops`, recorded through the builder's own door so undo, the
dry run and the review work unchanged) and a ceiling filled in for its editor
(`budgets.ceiling.prefill`). A page of several components offers every one's
actions together.

**Every screen with a draft says what is open** (`app/assistant/draft.ts`).
One hook, `useAssistantDraft`, takes the draft, its identity (`client` while
the values live in the page, `server` where they are saved at an id and a
revision), the step or section, the screen's help (what Ask Crewlet can and
cannot do there, said by the screen rather than the prompt), and its fields,
each a `read` from the draft and, where Ask Crewlet may set it, a `write`
that refuses a value in the sentence the field's own control would. The page
context carries the snapshot redacted (a field marked `secret`, or whose path
names a credential by the engine's rule, is sent empty) and bounded (forty
fields, a thousand characters a value, the rest counted in `more`, and fields
shed from the end before the turn is ever refused). The same hook declares
`draft.set`, whose card shows each field's value now beside the proposed one,
which applies through the screen's setter, and which offers Undo while the
draft still says what the change made of it. A gate
(`app/assistant/draftGate.test.ts`) fails for a module with draft or form state
(a leave guard, an editing status, a setup wizard, a rich, inline or list
editor, a form field) that neither declares a draft nor is listed as part of a
screen that does, or as a modal or signed-out form with its reason. A page
with several boxes open at once declares ONE draft whose fields are every box
open on it (`app/assistant/boxes.ts`), each read from its box and set through
the box's own setter, and the draft's step names the box written in last: the
page context carries one draft, and a box that declared its own hid every
other box from Ask Crewlet at its next keystroke. A chat room is such a page
(its message box, the reply box of a thread open beside it and each of the
person's messages open for a rewrite), and so is a task's, with its title and
its description open together. The brand
setup and the Brand page share one field list (`lib/brand/assistantDraft.ts`):
name, tagline, colours, typefaces and voice are settable, and a logo is a
`file` field, set only by a reference to one of the person's files in the
conversation (the engine checks it is theirs before the card is drawn). The
card draws a proposed image through the owner's route in `LogoSlot`'s
thumbnail, an `<img>`, never inlined (`app/assistant/chunk/DraftFile.tsx`);
once approved, the screen fetches the bytes and keeps them as a picked file
for its own save. The help tells Ask Crewlet it makes no images there and
offers an SVG starting point only when asked. The palette offers
"Ask Crewlet" for a typed question, which the panel asks as its own box would.

**A turn is the engine's frames, reduced** (`app/assistant/chunk/turn.ts`):
one pure reducer over every gesture and every frame, a frame at or below the
sequence a turn holds dropped, so a resumed stream never draws a delta twice.
The stream is server-sent events over `fetch` (`protocol/assistant.ts`):
`Last-Event-ID` on every resume, a stall after two missed keepalives dialled
again, a refusal or another protocol version stopping it for good.

**What it draws is the design system's** (`app/assistant/chunk/blocks.tsx`
maps every wire state to exactly one component state): the question in a
`MessageBubble` with the page it carried, a `RunChip` that opens on the
`StepList` of the ordinary requests it made, the answer as prose with no
container, and the engine's cards (`ResultTableCard` with its CSV and XLSX
downloads from the stored snapshot, `EntityList`, `OpenInAppCard`,
`RefusalCard`, `ProposalCard` with the before and after of every change, and,
where the result's visibility changes, who will see it). A refusal of access
is a `RefusalCard` (a value not accepted or a record not found is a warning
`Callout`), and it names a missing permission, with a request to copy for an
admin and the line that Ask Crewlet acts with your access only, only where
access is the way past it, which the refusal's `reason` decides
(`lib/authorityRemedy.ts`, the table a screen's refusal asks too); any other
says the engine's own sentence, with nothing to copy. A conversation an
administrator read for compliance says so at its top, with each reader, when
and the reason they gave.

**Deep work is one card, redrawn in place.** When Ask Crewlet hands a large
task to a deep-work run, its step carries the run's progress and the engine
sends it again under the same step id as the run moves. The drawer draws it as
a `DeepWorkCard`: the goal, its state, its time against its deadline, its
Crewlet calls against their cap and the share of its budget used (each a
meter), and its latest activity lines in mono. The steps the run made (`parent`
is its index) are nested under the card, folded behind their count when there
are more than three, rather than listed in the `RunChip`, which says "Deep
work" while the run works. The turn's Stop ends it; a stopped or failed run
says why, and a waiting run says it waits on the change card below it.

**Files are bytes read as the person, previewed only as pictures.** A file
card lists what a run delivered in a `FileList`, each with its name, type,
size and a `DownloadButton` that fetches its `download` route under the
session. An image (an SVG included) is previewed through an object URL in an
`<img>`, typed from the reference rather than from the response, so nothing
a file carries reaches the page as markup; any other file shows no preview.

**A question can carry images.** The person attaches up to four PNG, JPEG,
WebP or GIF images of at most 5 MiB each by Attach image, by dropping them on
the composer, or by pasting (a screenshot). Each uploads at once
(`POST /assistant/conversations/{id}/files`, under its own operation key and
through the same request path as every other Ask Crewlet write), stands in the
composer as an `AttachmentChip` with its thumbnail, and its file id travels
in the question's `attachments`. One the engine would refuse is refused above
the composer in a sentence that says what to do, and Send waits while an
image is still uploading. A sent question shows its images as chips under its
bubble.
**A model's words are drawn inert**: a link is its label and the host it
names, never an anchor; a picture is its alt text, never fetched
(`ASSISTANT_WORDS` in `lib/markdown.ts`). Ask Crewlet moves the page only
through an engine-built Open in app card, whose address the drawer checks is
one of this dashboard's own framed routes before it follows it, or through a
page action the screen itself declared. There is no
answer rating, and the one toast is "Conversation cleared". The toast stack
keeps clear of the panel (`app/toastInset.tsx`): beside it while it is a
column or over the page, above its composer while it is the whole window on a
phone. On a screen with a bar of its own, Chatlet's, it stands under that bar
at the top right instead (see
[Chatlet](#chatlet-a-top-bar-a-rooms-panel-a-room-and-where-you-have-read-to)).

**The panel is a chunk of its own**: the first paint carries the Ask button,
the page context registry, the bus and the one-word status the button reads,
and the drawer, its store, its reducer, its cards and its stream client load
the first time it opens, second in the idle prefetch after the palette.

---

## Acting, as yourself

**The dashboard acts as you.** Every change in a Crewlet company is attributed
to whoever made it, and the one actor an audit trail cannot name is "the
dashboard" — so there is no such actor. A write from a browser is made by the
**principal your session resolves to**
([ADR-0024](../../adr/0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md)),
through the same operator tools your own assistant calls, and it is recorded
exactly as that assistant's would be: a person — every person holds a human
seat — or a token the identity directory binds to one writes as that seat,
author kind `human`, with the session's lineage as the credential
(`operator_id`); a credential bound to no seat — an API token's session, a
service account acting as itself — writes under its own login, kind
`operator`. Being bound is not a condition of writing — it decides only whose
name the change carries.

**The audit log draws a person as a person.** A write of kind `human` reads as
the seat it was made as — the person's circle and the seat's name, linking to
the seat — with the credential in the tooltip and in the export. A write of
kind `operator` is a person too, but it has no page: its row is the circle and
the login as plain text, never a link to a seat of that name. A seat's own
write takes the chart's kind, or, for a seat the chart no longer holds, the
kind the write was recorded under.

The engine's half is [`POST /operator/act/{tool}`](api-endpoints.md#operatoract--the-dashboards-write-surface):
one catalogue tool per request, an operation key the screen mints per gesture
(`newActOpID()` in `protocol/act.ts`) and sends as the `Idempotency-Key`
header — again, unchanged, on a retry — and an answer carrying the write's
`outcome` and `position`, the floor the screen's next read waits for. The live
socket stays read-only; writes and credentials travel over REST. `viewer.acts`
is the engine's own list of what it would serve you — the catalogue's writes
the authority table can admit you to before any object is named — which is
what a screen reads to disable a control and say why, rather than offer a
press the engine refuses.

### A write control is never hidden

Every control that changes the company is drawn for every reader. Where your
browser cannot make the change it is **disabled, and says why** — the kit
button's `disabledReason`, which keeps it focusable and reads the sentence to
a screen reader as the button's description. The kit draws that sentence for
nobody else, so the same words are the button's `title` while it is held — a
pointer resting on it sees why — and a screen reader, which reads a described
button's description rather than its title, hears it once. A form whose
primary action is held also writes the reason on the page beside it (the New
task sheet's foot), because neither reaches a touch screen. The seven reasons,
in the order you clear them (`lib/useWriteAccess.ts`) — and being bound to no
seat is not one of them, since the engine makes a credential's change all the
same, under its own login:

| You are | The control says |
|---|---|
| Refused the company — a refusal the socket met (your seat was taken out of the chart) | Can’t make changes right now. |
| Offline | Offline. Reconnect to make changes. |
| Not yet known | Checking who you are before anything can be changed. |
| Anonymous | Sign in to make changes. |
| Without any app the change needs: Worklet for a change to work, Doclet for one to a page, Chatlet for one in a room, either for a question answered from both (the act's own `apps`, which the engine holds to its authority table) | This change needs Worklet access. An admin can give it to you. (Doclet, for a page; Chatlet, in a room; Worklet or Doclet, for a question.) |
| Somebody the engine does not make this change for | This can’t be changed from here. |
| Looking at somebody else's record | The screen's own sentence, naming whose record it is — My work read on a report's day holds every control on it (`HoldWrites`), and releases exactly the one change it offers there, a lead's reorder of that queue. |

**The one set of writes drawn for admin only is the identity
directory's**, People, Service accounts and API tokens, and a human seat's
**Invite**, **Create person** and **Cancel invitation** on the org chart's
card, the seat's peek and its page. Settings' Access group is an admin's to
open at all, so its controls
are never drawn held. A seat's gestures are the same writes about the same
directory, and held on every human card of the chart, on every peek and on
every person's page they would be noise on the company's busiest screens,
read by everybody — never news, since the one access that would enable them
is Admin. So they are drawn for an admin and left out for everybody else, and
the engine refuses whatever reaches it all the same.

**The charter's writes are the second set.** Company › Principles is every
reader's page and one admin's text: an **Edit** on each of its three cards and
an **Add** in each unwritten one, held for everybody who reads the company's
charter and never writes it, would be three or six disabled controls on a
page whose whole content is reading. So a reader without the grant to write
the charter is offered none, while an admin whose write is held (offline,
not yet known) is shown the control disabled with its reason, as everywhere
else. A document another system manages (ADR-0030) is the one hold that never
clears from this page, so there an admin is offered no control either, and a
line above the cards says where the charter is changed instead.

**One press at a time, by every way in.** A write control refuses a press
while its last one is still out, and so does every other way into the same
press — a dialog is a form Enter submits without touching the button, a reply
field sends on ⌘Enter, a one-line form (a sub-task, a title) files on Enter —
because each reaches the same gate (`pressable` in `components/WriteButton.tsx`).
They used to go straight to the write, and a second Enter before the first
answer came back filed a second sub-task or posted the comment twice, under a
new operation key the engine rightly took for a second change. On a task page a
change conditional on the version you were looking at is not sent at all while
another is out: it carries the version the first is about to move, so the
engine could only refuse it, and the page would then have reported your own
first change as somebody else's. A checklist tick or a follow, which carry no
version, still go.

A change is **never queued**: a write sent when the connection came back would
be one you walked away from believing it had happened, onto a company that may
have moved since. `viewer.acts` is the engine's own list of what it makes for
you, and the last row is read from it, never guessed from a tool's name — a
company whose tracker is Jira has no Worklet to write to, and its task page shows
Assign disabled rather than a button the engine refuses.

### Writes are confirmed, not optimistic

Nothing on screen moves until the engine has answered. Each answer is one of
four, and each tells you something different:

| Outcome | What you see |
|---|---|
| `applied` | A toast naming what changed; the screen is re-read and redrawn with it. When the engine answered **caveats** beside the change, the toast carries them in a person's words: it stays until you dismiss it when one of them needs somebody to act (a project's default assignee who has left the chart), and goes by itself when they are only something to know (a time stored as its date). |
| `pending` | "Sent. This node has not applied it yet." The re-read waits until this node has. |
| `unknown` | "Could not confirm. It may have landed", which **stays** until you dismiss it, with Retry. |
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
Retry it sends the **same operation key** it sent the first time, from which the
engine derives the same operations; a new press is a new key. Each Retry is
bound to the press it reports — press twice and the first toast's Retry still
sends the first press, never the second — and to the person it was pressed
for: it names the tab's reader as every request does, so a Retry from a tab
whose browser somebody else has signed in to since is refused
`reader_changed` unrun, and the tab handed to them, rather than made as their
write. A refusal a busy, draining or starting node caused offers "Try again"
on the same terms; one the request caused does not,
because the same request would be refused again. A control reused across
objects — the Inbox's Mark read as you move between notices — belongs to the
object it is drawn for, so one notice's refusal never appears under another's.

**The refusal is in the dashboard's words**, except where the engine's own
sentence names the argument that was wrong (`invalid`) or the rule that
forbade it (`forbidden`): every other tool sentence is written for a model
reading a tool result. Those two carry the writer's sentence alone:
Worklet's refusals hold their words apart from the `worklet:` a Go error opens
with (`worklet.Sentence`), so a person reads "create_work_item refused that: a
decision has 0 option(s) …" rather than the package's name in front of it, and
a forbidden one no longer ends by repeating the sentinel's own "not this
actor's to write". `contract/errors.ts` `ACT_ERRORS` holds one sentence per
code and is held against the engine's codes both ways
(`TestTheDashboardKnowsExactlyTheActRefusals`), but for a refusal on authority:
`unauthorized` is every rule's, so it is the engine's sentence for the rule
that refused, which follows its `reason`. A refused change names the access to
ask an admin for ("You need Worklet access for this. Ask an admin.") only where
that access is the way past the rule (`lib/authorityRemedy.ts`): a proof gone
stale names the access that admitted you, and an artifact's role is cleared by
its owner's share. A conditional edit that lost
the race reads "Changed by somebody else since you opened it." — somebody
else, not a name, because the refusal carries the version it lost to and not
its author. That refusal, and every other that says the screen is out of date
(`conflict`, `not_found`, `exists`, `already_answered`), asks the write's
questions again without raising your read floor, so the page redraws what is
there now and your next press is made against it.

**One exception: a cell of a sheet (D23).** A native sheet, and a page's
sheet through the bridge, draw the reader's value at once, MARKED AS PENDING (a
dashed underline and "Saving" in the cell's description and the sheet's sync
strip), and make it solid when the engine confirms it; a refusal reverts the
cell and says why on it, and a write nobody could confirm is kept for Retry
under its own key. What is drawn solid is what the engine holds. A sheet that
waited for the engine on every checkbox would be unusable by thirty people at
once, and the pending mark is what keeps it honest. Everything else an
artifact's screens write (sharing, a schema change, a version label, the trash)
is confirmed like every other write.

### What a screen can change today

`contract/actions.ts` `ACTIONS` is the dashboard's whole write vocabulary —
the tool, the arguments a screen may send, the log it lands in and the
questions it moves — and `TestEveryActionTheDashboardTakesIsOneTheActTransportServes`
holds it against the real operator catalogue: every tool is served by the act
route and is not a read, every argument is one the tool takes, every required
one can be sent. It carries a row for each tool a control presses and no
other — `app/source.test.ts` holds it to the `useAct("…")` calls both ways — so
a tool arrives in it with the control that sends it. The controls
(`components/writes.tsx`, and chat's in its own screen, `routes/chatlet`):

| Control | Where | Tool |
|---|---|---|
| Assign / Reassign (with a reason) | A task's page | `update_work_item` (`if_match` on the version you are looking at) |
| Status · Priority · Labels · Due · Start · Estimate · a custom field · the title · the description | A task's page, from the value itself | `update_work_item` (`if_match` on the version you are looking at) |
| A checklist box · Watch / Unwatch | A task's page | `update_work_item{checklist}` / `{watch}` — gestures applied as they land, with no `if_match` |
| + (a sub-task) | A task's sub-tasks | `create_work_item{title, project, parent}` |
| Create task (the New task sheet) | A work screen's New task, a board lane's `+` | `create_work_item{title, project, body?, type?, status?, assignee?, priority?, due?, labels?}`, only what was set |
| Message (the same sheet, titled "Ask {seat}") | A seat's peek | `create_work_item{title, project, assignee, ask, …}` — the answer lands in your Inbox |
| Direct message (chat's New direct message, the seat chosen) | A seat's page and peek, on the engine's own chat | `open_dm{to}` — opening one that exists is that conversation |
| Edit project (the target date) | A project's page bar | `write_project{project, target_date}` — `null` clears it; a company write, whose authority the engine decides |
| Comment · Reply · Ask… | A task's composer | `comment_on_work_item{item, body}`, `{item, body, reply_to}`, `{item, body, ask, decision?}` |
| Restore (named "Restore ENG-42" to a screen reader, so a grid of them can be told apart) | A task in the trash, on its page and in the trash grid | `restore_work_item` |
| Pin / Unpin | A saved view's page | `set_pins` |
| Mark as read · Mark as unread · Snooze · Unsnooze | An open update in the Inbox | `mark_inbox` (exactly that one notice; every other mark and your read position stay) |
| Mark all read | The Inbox's page bar | `mark_inbox{read_through}` at the newest notice LOADED, never past it |
| An option · Something else and Send | A question in the Inbox's pane | `comment_on_work_item{item, answers, choice}` / `{item, answers, body}`, then `mark_inbox{read}` for the notices that asked it |
| Send | The reply box under the Inbox's conversation | `comment_on_work_item{item, body, reply_to}` |
| Link a task · Attach a page | The Inbox's reply box | `update_work_item{item, linked}` / `{item, linked_pages}`; "Save as a page" first writes it with `write_page` |
| An option · Something else and Send answer | A parked coding run in the Inbox's pane, and its Answer dialog | `answer_run{turn_id, launch_id, choice}` / `answer_run{turn_id, launch_id, answer}` |
| Accept · Change and Submit · Something else · Reply | A question in My work's Asked of me, and Home's "Needs your decision" | `comment_on_work_item{item, answers, choice}` / `{item, answers, body}` |
| Drag a row · `Alt` + ↑/↓ | My work's Priorities reading (your own queue, or one in your line) | `set_priorities{handle, items, if_match}` — the whole stored list with the one row moved, conditional on the record's version |
| Publish · Save as draft | The composer (`#/doclet/new`) | `write_page{title, body, container, parent?, labels?, status}`: a draft and a published page are one write with the status set |
| Create › Folder | A space's outline, at the end of an open level | `write_page{title, body: "", container, parent?}`: a page with an empty body is a folder |
| Save (with "What changed") | A page's editor | `save_page{page, base_version, body, message?}`, against the revision the edit started from |
| Watch · Stop watching (the eye) | A page's bar | `save_page{page, base_version, watch}`: the person's own subscription, nothing else |
| Comment · Reply | A page's comments | `comment_on_page{page, body}` / `{page, body, reply_to}` |
| Send (`Enter`) | A chat room's composer | `post_message{channel, body}`: under the operation key the outbox keeps (`Idempotency-Key`), so a retry after a lost answer is the same message rather than a second one |
| Reply | A chat thread's composer | `reply_in_thread{channel, reply_to, body}` |
| Edit message, then Save | A chat message of your own (its More menu) | `edit_message{channel, message_id, body}`: the author's alone, never an admin's on somebody else's words, and taken in an archived room. Save is held over a rewrite that is the message as it stands or is empty; the editor closes on an answer that took it and keeps your words on any other |
| Delete, then Delete in its dialog | A chat message of your own (its More menu), and anybody's to an admin | `delete_message{channel, message_id}`: the author's, or an admin's moderation, and the tombstone records who. The dialog shows the message as the room draws it and says that everybody in the room sees who deleted it. Reaches past membership and past an archive; held on a room this build cannot classify |
| A reaction | A chat message's reaction chips | `react_to_message{channel, message_id, emoji, remove}` — quiet, no toast; a refusal is said above the transcript. Held outside a private room you are not in, as a post is; an archived room takes it |
| Join · Leave | Join on a room you are not in and in **Browse rooms**; Leave in the room's **More** | `join_channel{channel}` / `leave_channel{channel}`, refused on a unit's room and a direct conversation, so neither offers it |
| Mute · Unmute | A chat room's header | `mark_chat_read{mute}` / `{unmute}` — this room alone, every other room's state kept |
| New room | The Rooms switcher's and the Rooms panel's foot, and Browse rooms | `create_channel{channel, kind, topic?, members?}`: the name is claimed, never renamed, and a name somebody holds goes back to its field; the visibility opens on the company's own default |
| A new message | **New message** (`#/chatlet/new`) | `open_dm{to}`, then `post_message` to the conversation it answers: only once there is something to say; the people chosen may already share a conversation, which the view shows and sending adds to |
| Add people | A room's details (Members), its first-step card and More | `add_channel_members{channel, members}`: never on a unit's room (the org chart's) or a direct conversation (its people are its identity); the answer names who it actually put in. Leaving a new room's dialog with nobody chosen asks once |
| When new agents and people join, add them | Add people, to an admin, on a public room | `update_channel{channel, auto_join}`: an admin's alone, on a public room alone; it adds the seats that join the company from then on and nobody already in it |
| Topic · Description · A template | A room's details (About), its first-step cards and More | `update_channel{channel, topic?, purpose?, template?}`: only the field changed; each held to 250 characters, counted as the engine counts them (code points, so an emoji is one); a template sets both in one change that names it, one line in the transcript. Archiving and the room's horizon are not offered anywhere in the dashboard: an archive closes the room for everybody in it, and the horizon decides what survives on every data node and is an administrator's step-up gesture (`PUT /chatlet/channels/{channel_id}/retention`) |
| Every message · Mentions · Muted | The top bar's notification choice, and a room's details (Notifications) | `mark_chat_read{notify}`: the reader's own mark, waking nobody; Muted is the mute |
| Follow · Unfollow | A thread's pane, and Threads | `mark_chat_read{follow}` / `{unfollow}`: the reader's own mark; a thread they started, replied in or were named in is followed without one, and replying in one they unfollowed follows it again |
| Save for later · Remove from Saved | A chat message's tray, and Saved | `mark_chat_read{save}` / `{unsave}`: the reader's own; past `MAX_READER_MARKS` the press says Saved is full rather than sending, and a saved message that is gone stays listed until it is removed |
| Mark unread | A chat message's More menu | `mark_chat_read{rewind}`: the room's place set to just before the message, the one way it moves back |
| Join room | The bar in place of a room's composer, for a room you may read but are not in | `join_channel{channel}` |
| — (the read cursor, no control) | Written as you read a chat room | `mark_chat_read{cursors}` — every `READ_FLUSH_MS` (15 s) and at once on leaving the room, hiding the tab or closing it; quiet, and on no log (`domain: null`). Its answer is your seat's read state as it left it, and the tab applies it as it would the push — so the rail asks again once, when the revision moves |

A change to the **company's configuration** is not an act. The company
document — its ceiling, an MCP server, a model — is a new configuration
revision, and every screen that makes one goes through
`protocol/configWrite.ts` (read the document and its entity tag, or one entity
and the same tag; dry-run a merge patch or an entity replacement; save either
with its audit summary), always conditional on the revision it edited. A seat,
a unit and a seat's own ceiling are part of that document too, and the org
builder writes them through it (`lib/useCeilingWrite.ts` writes a ceiling
raised in place as the seat's own entity). They are decided by
ADMIN rather than by the act catalogue or a seat binding — or,
for a seat or a unit inside a unit the reader leads, by that LEAD, whom the
engine admits to `/config/roles/{handle}` and `/config/units/{key}`
(`lib/leadScope.ts`) — and, where another system [manages the
document](../concepts/configuration.md#managed-configuration), only a
credential `api.auth.company_writers` names may change it, a lead's edit of
their own team included. So their controls ask `useConfigWriteAccess()`, or
`useOrgWriteAccess(target)` for one part of the org chart, which read
`viewer.grants` and the reader's scope and then the managed block
(`viewer.config_writer` and `viewer.config_managed_by`), and are disabled for
anybody else with a sentence naming the access and who can give it, the units
the reader leads, or who manages the document (`managedSentence`) — the managed
block checked after the authority and before a screen's hold, since no screen
can release it. A screen that decides whether a change is the reader's to make
before anything is pressed — Home counting a budget-stopped seat as their
decision, "No weekly budget" or "Set one" offered as a link, the org chart's
**Add seat** — asks `mayChangeConfig(viewer)`, Admin and the managed block
without this moment's connection or hold. A write the engine answers `403
step_up_required` is not refused on screen: the transport asks the person to
confirm who they are and replays it (see [Signing
in](#signing-in-is-a-screen-outside-the-frame)).
`protocol/configAnswer.ts` is the one reading of a `/config` refusal — a
conflict to re-read, `403 config_managed` (the `managed` kind, with the
writers the engine named), the drain gate's certain `503`, a request that may
have landed, or the problems the document has — which the org builder's model
classifies through too.

## Honest empty states

A screen that renders a blank where data would go is a screen that cannot be
trusted when it IS blank. Four distinctions the product makes everywhere:

- **Nothing happened** vs **nothing could be read.** "No events" on a fresh
  company and "no events" from a query the engine refused are the same empty
  list and completely different problems. `QueryState` renders the engine's own
  code (`unknown_query`, `unauthorized`, `not_found`, `unavailable`,
  `bad_params`, `query_failed`) and the client's own — the socket's `timeout`
  and `closed` — as a sentence saying which.
  `bad_params` is the one that names the SCREEN as the fault: the engine
  understood the question and refused it, so retrying sends the same bad
  request again. `unauthorized` says the rule that refused — the access the
  refusal names where access is a way past it, the rule's own sentence
  (`contract/authority.ts`, keyed by the engine's reasons) where it is not —
  and offers the ONE control that clears that rule (`lib/authorityRemedy.ts`):
  **Sign in** for a reader short of access or an agent's seat on a verb only a
  person makes, **Confirm it’s you** for a proof gone stale (the step-up
  ceremony, after which the screen asks again on the socket's reconnect),
  **Sign in as yourself** for a credential acting as no seat or a token on a
  gesture that needs the person, **Try again** once an admin has activated an
  account that is not active or an artifact's owner has shared it at the role
  the verb needs, **Reload** for a request this engine's build has no rule
  for, and nothing where no reader can clear it from here (a verb only an
  agent's seat makes, a request that named no record, an agent acting for
  somebody who could not take the verb, a request Ask Crewlet made
  for a gesture it never takes or a change its person did not approve, which
  only its own panel meets). A stale proof's `grants` are what
  admitted the reader, and an artifact refusal's `admin` is the override beside
  the share its owner makes, so neither is drawn as access to ask for. `unavailable` is the opposite: the node will answer in a
  moment, so `useQuery` asks again on its own rather than leaving a person to
  reload — WHEN the engine's frame says (`retry_after`, read through
  `protocol/retry.ts`), never after a constant of the client's. A hint is
  bounded at thirty seconds (`RETRY_AFTER_MAX_MS` in `contract/retry.ts`),
  which cuts only the one hint the engine DERIVES — a backlog over a drain
  rate, which runs to minutes on a node that has just joined a busy log — and
  a `retry_after` of `0` means waiting will not change the answer (a state-log
  refusal such as a full log or a record the node cannot decode): then nothing
  re-asks on a timer, and a reconnect, a write the screen makes or a reload
  asks again, because re-asking a read an operator has to unblock is a loop,
  not a retry. Every REST read goes through one hook, `lib/useRest.ts`, which
  reads a `503`'s `Retry-After` the same way and reads again when the socket
  comes back. The table is keyed on the contract's `QueryErrorCode` union, so a
  code added to the union without a sentence here is a compile error, and a Go
  test in `internal/api/stream` pins that union to the codes the engine sends.
- **Zero** vs **unknown.** The integrations answer's `skipped` and `coalesced`
  are three-valued, and a count this node could not read comes back `null`,
  never `0`; `inbound` is a plain count whose unknown-ness rides on the
  answer's own `traffic_known`. The budgets answer says `durable: false` when
  the counter could not be READ.
- **Not configured** vs **empty.** A knowledge search with no backend says so;
  a company with no seats says roles come from the configuration; and an
  address under a service the company does not run says what runs instead
  ("This company talks on Mattermost" under Chatlet) rather than asking a
  question the engine does not serve there and drawing the `unknown_query` it
  would get as a node fault.
- **Everything in the window** vs **everything that arrived.** Where a screen
  narrows client-side it says so, naming the SOURCE rather than hedging the
  whole answer. **Audit** is the case that made this a rule: it composes four
  subsystems and only one of them, Worklet's feed, takes a wall-clock
  window, so the other three are asked for their newest page and narrowed on
  the client. A page that fills up before it reaches the start of the window is
  older rows the screen never saw, and a caption reading "some of this may be
  missing" is one nobody can act on where "Doclet shows only its newest
  rows" says where to look.
- **The engine** vs **a stated writer.** An empty actor on a tracker or wiki
  commit is the engine's own write, and the Audit screen draws it as "the
  engine". A configuration revision is different: the revision states WHAT
  wrote it (`created_by_kind` — `agent`, `human`, `operator` or `system`) and
  the row shows that word rather than assuming one — it used to label every
  revision `operator`, so a node's boot seed and the reconcile loop's reloads
  read as a person's writes. Every revision carries its kind, so neither the
  Audit screen nor Configuration's history ever guesses one, and a kind this
  build does not know is shown as the word it is.
- **An empty CONTAINER** vs **a query that matched nothing.** A container says
  its own emptiness, from what it already knows about itself, before the list
  it holds has answered anything — and that state REPLACES the list rather than
  sitting under it. A project page handed its whole body to the work list, so a
  project nobody has ever filed anything in said "Nothing matches — no item
  matches these filters. Widen them", with no filter set: a claim about a
  narrowing that did not exist, on the day-one state of every project. It is
  drawn from the project's own maintained counts now, names the project, and
  offers New task to file the first one. "Nothing matches" is reserved for a
  query that genuinely narrowed. The same rule sorts the two empty pages apart:
  the work list's empty state is about ITEMS and the projects directory's is
  about PROJECTS, so a company with no projects is pointed at New project,
  which starts one.
- **An empty SEGMENT is not an empty company — so the answer carries the
  census.** The projects directory's three segments each ask the engine for
  their own set, which is what makes the listing honest and is also what makes
  an empty answer ambiguous: an empty **Active** answer is either a company
  with no projects or one that has archived every one of them, and a reader
  acts on those oppositely. The screen cannot derive the difference, because on
  the Active segment the archived projects have no row on screen to be derived
  from. So `work_projects` answers with a `census` of BOTH sets under the same
  narrowing, and the page never guesses: `active + archived === 0` is the
  company having nothing and draws "No projects yet" on
  **whichever segment the reader is on** — which matters, because they land on
  Active; an empty Active answer with archived projects behind it links to
  them; and Archived says nothing is archived. The counts sit
  on the segment control itself, so the switch says what is behind each option
  before it is pressed. A screen that hedges — one sentence naming both ways a
  state happens — is a screen missing a number, and the fix is to send the
  number rather than to word around it.

  **And the directory is not the only reader of it.** Every surface that draws
  a conclusion from an empty project listing reads the same census, because
  every one of them asks the ACTIVE set: the project switcher, and the
  Tasks lens (`#/worklet/tasks`), which replaces the whole list with "No work has been
  filed yet". Read from the ROWS, each concluded the first state — so a
  company that had archived all four of its projects was told by the sidebar that
  no project had been created and by the landing that nothing could be filed
  until a unit declared a `project` key, both beside a directory saying all
  four had been archived, and both false about a company holding every item it
  ever filed. `active + archived === 0` is the only thing that draws the
  first-run panel now; `active === 0 && archived > 0` keeps the ordinary list
  and gives it an empty state with the way to
  `#/worklet?shown=archived`, the same sentence the directory's own
  Active state writes, because it is the same fact. An answer carrying no
  census concludes neither, on the rule its rows already followed.

Every empty state names what would fill it.

**And a restart is not an empty company.** The pushed surfaces come from the
engine's live projection, which is seeded from the fleet's event stores when
the process starts — every live data node's, through the scatter the history
queries use: the newest events for the activity feed, and each seat's newest
turns for the last one it ended. Until that read existed, every one of these
screens started blank after a restart, a deploy or a node joining a fleet,
which is the one empty state a reader has no way to question; a seed that
missed a node is named on `/health` (`seeded_from`) rather than drawn as a
quieter company, and every screen drawing the seeded feed names it too: Live ›
Events and Live now's Activity card add it to the coverage note above their
rows, and Home's Latest events widget says it as its gap. Spend is not among them: the Overview and Spend screens read
named windows — whole company days from the replicated `usage` domain, the
same on every node — so a restart never empties them, and the 24-hour spend
window the seed also fills is the `tokens` push's, which no screen draws.

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
   server fills from the promoted column, rather than out of the payload. A
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
   separated by a blank line and prose contains blank lines — so a card
   that read only the string could not say which round said what, and put
   the model's words in a block detached from the calls they were about. So
   the engine sends the split it already knows, at the point
   the round's assistant message is appended, and the card reads the rounds
   from that alone — the joined string is rendered only as a coding run's
   report, whose rounds happened in its box.

   **A round that answered in prose where a call was owed says what became of
   it.** A phase of the engine's tool loop that finishes only by a tool call —
   the reviewer's `submit_review`, a worker's `submit_result`, the onboarding
   pass's `mark_onboarded` — can still get words and no call, and the commonest form is the model
   printing its own submission as a fenced JSON block. Without a word about it
   the reader saw that block, then "incomplete, no report" and "rescued" on
   the header, and nothing connecting the two. The engine flags such a round
   (`round_narration[].declined`), and the round says beneath its words what
   happened next, which the ledger already knows: a later round means the
   engine asked again (said neutrally — that is the loop working); the last
   round of a settled phase means the bound was spent and the phase ended
   without its submission (said in the caution tone, with its glyph); and the
   newest round of a running phase claims neither. The header counts them as
   "N answered in prose", beside its sibling "N empty" — both are rounds that
   ended without the call the phase needed, one with words and one without.

   **"Rescued" means the phase ENDED without its submission succeeding**, so
   the engine wrote the decision in its place — `incomplete` for an executor,
   `self_iterate` for a reviewer. Its title used to say the phase "was
   re-asked", which nothing does to a phase as a whole. And a rescued review's
   decision reads "undecided, sent back", because `self_iterate` is also the
   word a reviewer chooses on purpose, and "sent the turn back" claimed a
   judgement nobody made.

   **A round needs no sentence about how it ended.** Each round's
   `rounds[].stop_reason` is `end` or `tool_use` — an answer, or calls to run
   — which its words and its calls already show, so the card draws nothing
   from it. There is no reason for a round cut short: a CLI whose model hit
   its ceiling or declined says so in its own words, which the round shows as
   it shows any words, or fails, and a failed phase carries its `error_kind`
   on the header in the danger tone, and its error text in a danger callout in
   the body.
3. **The model's words are prose; JSON is monospace, and INDENTED.**
   Speech gets a proportional face, real leading and a bounded
   measure, and are rendered as the markdown they are, read by a model's
   habits — every newline a break, every heading a styled line rather than a
   heading of the page (see
   [Markdown is rendered, not printed](#markdown-is-rendered-not-printed)).
   Monospace stays where it carries meaning — tool arguments and tool
   results, and a fence the model wrote in its own words.

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
   ten thousand levels. And a LIVE result over four thousand bytes is
   sent as its last four thousand, with a leading ellipsis, which is not a JSON document — so a long
   result renders flat while the phase runs and indented once it completes.
   The text already changes at that moment; the shape changing with it is the
   same fact, not a second one.

   The engine does not write the other half of the unreadability.
   `json.Marshal` escapes `<`, `>` and `&` for embedding in HTML, which would
   make every URL argument read `?a=1\u0026b=2` and every markdown body
   `\u003ch1\u003e` — on a screen that renders into a `pre` and needs none of
   it. `tools.RecordArgs` is the one encoder both the phase event and the
   bridged-run row use, and it writes the characters the model actually
   sent.
4. **Grouping is structural; colour is semantic.** A round is BRACKETED by a
   rail running from its numbered node down its own content, and adjacent
   rounds are told apart by a two-step alternating tint — not by a hue
   apiece: a colour per round would read as meaning something and mean
   nothing, which is the same objection as a colour per agent and worse at
   nine rounds. A round's node takes colour for exactly four states —
   normal, contains a failed call or did not finish (danger), answered in
   prose where the phase needed a call (caution), in flight (the accent) — because "which round went
   wrong" and "where is it now" are the two questions a reader brings to a
   running turn, and a round that printed its submission instead of calling
   it is the first question's other answer. That round's note says what
   became of it under its words (rule 2), and the note alone, at caption
   size, was easy to read past: the node looked like every other round's, so
   nothing on the rail a reader scans caught the eye. Caution and never
   danger, because the engine asks again or rescues the phase and the turn
   goes on either way.

   Bounding a round and separating it from the next one are two jobs, and
   only the second was being done. The rail was drawn strictly *between*
   rounds and the tint only on even ones, so a phase with a single round —
   the common case — got neither, and rendered as a bare numeral in the
   gutter beside a flat stack of look-alike rows: the words and the call
   they asked for read as siblings. The round's content also sat
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

   **The bound is the scroller's view, and at either end the wheel is the
   page's.** It was `620px` with `overscroll-behavior: contain`, and the pair
   was a trap a reader described exactly: scrolling the turn, reaching its
   end, and getting half the content until they moved the pointer out of the
   box. 620px is unrelated to the view — the page bar wraps, and the object
   header, the tabs and two card heads sit above the box — so on most windows
   the box's own bottom was below the fold; following keeps a live box pinned
   at its end, and containment swallowed every wheel-down there. The bound is
   now derived from one requirement — *once scrolled to, the whole box fits in
   the view* — as `100cqb` (the shell's scroller, a size container on every
   screen) less the sticky band and the card's inset below it, with a floor of
   four rows for a window too short for that; and nothing contains the wheel,
   so at the box's top or bottom the page carries on. The box ALONE: its
   caption is an uppercase label in a column the sidebar and a peek narrow,
   and it wraps, so a bound that subtracted one line of it promised a fit that
   held only while the caption did.

   The same requirement applies one level down. A tool's record is a scroller
   of its own, and one taller than the box that scrolls it is the same trap:
   inside a running ledger a record is at most the box less its one-word label,
   and anywhere else at most the view less its label — the record ceiling every
   block of machine text shares, declared once (see
   [The document does not scroll](#the-document-does-not-scroll)) — so on any
   window with room for 460px of record it keeps the ceiling it always had.
   And the tail
   re-sticks when the BOX changes height (a resized window) as well as when
   its content grows, and opening a running phase always starts at its tail:
   the "still following?" flag used to outlive the box, so a reader who
   scrolled up, closed the card and opened it again got a box that never
   followed — and a phase opened before its first round came back never
   attached at all.

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
11. **A prompt is a document, not a wall of text — and it is read through a
   map, an outline and one section.** Both halves of a phase's prompt were one
   code block each, and a seat's system prompt ran to tens of kilobytes:
   identity, mission, policies, the roster, the turn contract, whatever the
   turn prefetched, the workers, the skills and a tool catalogue. An
   operator asking the question this screen exists for — *what was the
   reviewer actually told about self-iterating?* — scrolled 30 kB looking for
   a heading. The first answer was a stack of closed folds, one per `##`, and
   a reader said that was hard to follow too: a column of look-alike rows,
   titled with raw inline markdown, sized in a unit that never added up, whose
   bodies collapsed one-fact-per-line text into run-on paragraphs. The
   *Rendered* view is now a designed reader (`components/PromptDoc.tsx`):

   - **The map** — one slim bar for the whole request, the system prompt's
     sections, a gap, then the user message's, each as wide as its share of
     the bytes, so where a prompt's weight went is a glance. Neutral fills in
     two alternating steps that are two different greys — they were two tokens
     holding the same colour, so neighbours never alternated and the dark
     palette's bar sat barely off the card — and the accent only on the
     section being read. A half is never narrower than its segments, so a
     small user message beside a large system prompt does not spill past the
     bar's end on a phone. Pointing at a segment says "Your turn · 2.8 KB ·
     23% of the system prompt", and pressing it reads that section. It is a
     picture of the outline, so it is `aria-hidden` and takes no focus; the
     outline is the keyboard's way to the same sections.
   - **The outline** — both halves under their names and sizes ("System prompt
     · 11 KB", "User message · 1.2 KB"), one row per top-level section with
     its title rendered as the inline markdown it is, its size and a weight
     bar. The bars are neutral, the selected row's too: its ground already
     says which row it is, and an accent bar a hundredth of a row wide was a
     stray dot. A title's link is drawn as its words in a row and in the
     picker — an option's children are presentational, and a click on a link
     there navigates instead of choosing — and as a link only in the reader.
     A listbox with one tab stop: the arrows, Home and End move through
     both halves and the reader follows, with focus moving to the row it
     selects. Beside the reader when the prompt view itself is at least 720px
     wide — a container query on the view, never the window, which the sidebar
     and a peek narrow without changing — and a picker above the reader below
     that, with the map kept.
   - **The reader** — only the selected section is mounted, so an unselected
     section's text is not in the card. Its title is a styled line, never an
     `h` element (a quoted prompt's headings are not this page's — a turn with
     six phases would put eighty of them into one screen's outline), and the
     region takes its name from it; then its size and share ("2.8 KB · 23% of
     the system prompt"); then Previous and Next, each naming the section it
     goes to — the user message's first section from the system prompt's
     last, which the label says — and staying put at the ends with the reason
     they cannot move, because a control that vanishes under the press takes
     the reader's focus with it. Each owns half the row, Previous at its start
     and Next at its end, and a long destination wraps inside its own half:
     as a wrapping row, two long names on a phone put Next on a line of its
     own under Previous. Then the body, with the headings inside the
     section drawn in place beneath it under styled sub-titles, nested on
     their levels.
   - **Find** — a box that annotates rather than filters: every row says how
     many matches it holds, a polite status says "12 matches in 3 sections",
     and Escape clears. A row that matches lifts — its title to the primary
     text step, its count on the ground the matches are marked with — and a
     row with none recedes to the tertiary step with its weight bar faded. It
     was one step of grey, and in the dark palette the secondary and tertiary
     steps are the same grey to the eye; dimming further would take a row's
     words under the contrast they owe, and a row with no match is still one
     somebody may choose. **The selection follows the find until the reader
     takes it**: typing a query the section on screen does not contain moves
     the reader to the next section that does (the first, from the top), so
     the first thing a find shows is a match rather than a count — but once
     the reader chooses a section themselves during that query, the next
     keystroke leaves them there, because they are reading it on purpose.
     Enter and Shift+Enter jump to the next and previous section that
     matches and bring its first match into view, at the nearest edge of
     whatever scrolls it; typing never scrolls, which would move the page
     under the box being typed in. Matches are counted on the SOURCE,
     case-insensitively — what the model was told, markup included — and
     marked in the reader with the CSS Custom Highlight API where the browser
     has it, which marks text without adding an element around it; where it
     does not, the counts still say where the matches are. **What is marked
     is what was counted**: the title is marked only when it is the section's
     own heading line, because a builder's "Task" or the untitled lead's
     words are not in the source, and the reader's own words — the size, the
     share, where Previous and Next go — never are.

   **The outline is the builder's, and the headings only where there is no
   builder's.** It was derived by cutting the prompt at every `##`, and a
   prompt carries other people's markdown verbatim: a chat trigger's "##
   Triage" and "## Thread context", a pull request's "# Title", a model's own
   "## Summary" inside review evidence or a ledger reply. Each of those either
   escaped the section it was quoted in or swallowed the rest of the prompt
   under a title nobody wrote. Only the builder knows where its parts begin and
   end, so the engine sends a section map with each prompt (`system_sections`
   and `user_sections` on the settled record, `prompt_messages[].sections`
   live — a key, a title, a byte count and whether the part opens with its own
   heading, per part), and the top level of the
   outline IS that map, sliced by UTF-8 byte offsets; a heading inside a span
   still structures it, nested under it, but cannot leave it. That includes a
   heading a HEADLESS part opens with: a worker's persona, a task the
   executor wrote or a trigger's text is the builder's part with no heading
   of its own (`headed` false), and when the markdown it quotes begins with
   "## Goal", that heading nests under the builder's "Task" like any other
   quoted one. It was taken for the span's own heading, so its title gave way
   to the builder's and "Goal" was drawn nowhere in the reading view while
   the Source view still had it. A part the map does not call headed is
   headless, whatever its first line: the engine writes `headed` on every part
   that opens with its own heading and leaves it off the rest. Spans are
   decoded without stripping a leading byte order
   mark, so a part that opens with a pasted document's U+FEFF is still a
   slice of the prompt. A record without
   a map — a resumed phase's — is outlined by its headings
   as before, and so is one whose map does not tile its prompt (the bytes do
   not add up, a boundary falls inside a character, a span is empty, a key
   repeats): a map that would mis-slice is not trusted at all
   (`lib/promptmap.ts`). Either way the outline is DERIVED — from the document
   or from its builder — never a list this app keeps, so a prompt that grows a
   section grows a row and no rename leaves a stale name on screen. It is
   derived once per prompt and map VALUE: every push rebuilds a phase's record
   from the wire — a streaming phase's five frames a second among them — so
   the map arrives as a new array each time while saying the same thing, and
   an open Prompt fold keyed on the array re-sliced both halves and re-counted
   its find on every frame.

   **Sizes are UTF-8 bytes, heading lines included.** They were UTF-16
   characters of each body with its heading line left out, so a half's
   sections never added up and disagreed with the Turn screen's Context tab,
   which counts bytes. Now each section is a slice of the source from its
   heading line to the next, separators included, so the sections of a half
   tile it exactly; the Prompt fold's own count is the two halves' sizes
   ("11 KB system · 1.2 KB user") rather than the phase's name, which the
   card's tag already says.

   **The prompt's line breaks are its own.** A builder writes a prompt line by
   line for a model, and an identity block of one fact per line, read as
   markdown's soft wraps, became one run-on sentence. Bodies render with every
   newline a break — the same reading a model's own words get (see
   [Markdown is rendered, not printed](#markdown-is-rendered-not-printed)).

   Three properties travel with it. **Each view does its whole job.**
   *Rendered* is for reading; *Source* is the record — each half whole, one
   block, byte for byte, one selection, which is what an operator reproduces a
   turn from and what they diff when a model starts behaving differently. What
   is genuinely record-sensitive survives rendering anyway: a fence comes out
   as its own block holding the exact text, so a tool schema, a JSON example
   and a contract template are byte-identical either way, and what the reading
   view spends is emphasis markers and list bullets. **The switch is offered on
   every document**, headings or none, since one decodes the markdown and one
   is the bytes; a prompt with no headings is one section, named for the
   whole half it is — "The whole user message" — since "Before the first
   heading", the lead run's name in a headed half, would claim a heading the
   prompt does not have. And **the outline is
   the document's own shape**, which mostly means flat and sometimes does not:
   an executor's sections are peers, but a ledger writes one `###` per prior
   turn *inside* its block, and hoisting those would put a turn of somebody's
   conversation between "Earlier in this conversation" and the ask — so a
   heading nests under the nearest heading above it of a lower level, and the
   lead run before the first heading is a section of its own, the first one
   read, never hidden.

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

   **Nothing in it scrolls or sticks.** The card hides its overflow and a box
   that scrolls inside the page is a second thing under the wheel (see
   [The document does not scroll](#the-document-does-not-scroll)); showing one
   section at a time is what keeps the page short instead.

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
   prompt is what was sent, and the tool surface is the tools that went *with*
   it — served to the CLI as its MCP server's for the length of each run — one
   call described in two folds, so they belong beside each other rather than
   either side of the transcript.

   **What it did is grouped by CLI run.** Most phases are one run of their
   CLI's loop; one that was steered, delegated or corrected is several, each a
   process handed the conversation so far. So the rounds sit under a head per
   run (`RunHead`): *CLI run N*, the entry that served it, how many rounds it
   made, how long it lived, what it spent and how it ended — or, for the run
   still in flight, that it is running. A run whose CLI's record showed no
   requests is one round, and its head says so, since that round's timing is
   the run's.

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
  they are counted and offered: *"3 new turns. Show"*. Keyed on identity,
  never position, so a row that UPDATES
  in place — a round landing, a phase completing — is never held back.

  **And identity reaches across the two lists.** A running turn lives in the
  live region; when it finishes it leaves that region and arrives in the
  settled list with a key that list has never admitted — so the row a reader
  had been watching for four minutes was replaced by *"1 new turn. Show"*. It
  was on their screen a moment earlier: it is not
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
which of these turns is happening right now, readable at a glance from its
rail rather than only by finding a badge. The rail is the WORKING hue, info,
the fill a working seat's status glyph stands on, because running is a state; it was
the accent once, which says what to do, and a second rule drawing it silently
replaced the info rail the card was meant to carry.

## Live › Now running and the turn list

**What a seat is doing is everybody's; what it SAID is its trail.** Now
running, Coding runs, the org chart's state lines and a seat's page are the
overview, open to every signed-in reader, and the engine splits what it sends
them into a status half and a body half ([the API's
rule](api-endpoints.md#a-status-reader-never-receives-a-transcript)): a call's
prompt, its answer, a tool's arguments and a coding run's task, question and
reply go to an admin, to whoever leads the seat, and — for a run's parked
question — to the person it is put to. Anybody else is sent the status with
`body_withheld: true`, and the dashboard draws each withheld value as **Not
shown to you** (`WITHHELD_WORDS`, `lib/access.ts`) — a run's question as *Its
question is not shown to you* — with the one sentence why: what a seat says is
its trail, which its lead and an admin read. Never as an empty value, because
"the prompt was empty" and "you may not read the prompt" are opposite facts. A
work item a call names is Worklet's, and is left off the row for a reader
without Worklet.

**Now running** (`#/live`) is five cards in fixed places, each saying so when
it is empty, because a screen whose sections come and go with the data cannot
be read at a glance:

- **Running turns** — one row per seat the engine says is working, the row
  Home's Live now draws (`LiveTurnRow`): what it is doing and on what — the
  item's key, or on a turn charged to no item the trigger's own summary
  ("Executing · Drafting the 2.4 launch brief") rather than a bare verb —
  where the turn is (Context → Execute "coding run" → Review, and "round 1
  of 1" from the executor's opening frame, while its run is being launched;
  on a phone the round leads the call line instead, so Review keeps its place
  on the stepper's one line),
  how long it has run from the turn's own start —
  so a turn still gathering context is already timed — the call it is making
  or last made, and the whole row a link to its trace — the turn's record, on
  its Timeline, as every row of a list of turns is; the links that say
  **Watch** are the ones that open its Transcript on the running phase (see
  [the turn trace](#the-turn-trace-four-tabs-one-clock)). A round with no update
  for two minutes is marked **no update**, for ten **stalled**, in the
  caution and failure tones — and never while the turn is parked, because a
  detached coding run is silent on purpose.
- **Waiting on a person** — every coding run parked on a question: whose, the
  question, who it asks (the chart's names for the seats the engine resolved),
  how long it has waited and how long its box is still held, and **Answer**,
  which sends `answer_run{turn_id, launch_id, choice}` for an option or
  `answer_run{turn_id, launch_id, answer}` for words, as the person reading,
  naming the question it drew.
- **In a box** — the rest of the detached runs in flight, eight before it
  says how many more. Empty, it says *Finished runs are on their turn’s
  trace*: a settled run's row is deleted, so the sentence it replaced,
  promising every finished run under Runs, was false.
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
the sidebar's Live row says how many seats are working on every screen (and
Home's page bar does, on Home), and a second figure here said the same number
twice and pushed the bar's controls off a phone's page bar. It is also where
the sidebar's **N more running** row lands. A seat is
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
  starts at that offset. Worklet's filter bar restated every one of
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
- **A setting is a radio group.** The mode and density controls are
  `SegmentedControl` with `semantics="radio"`, and so is the theme gallery: announced as a choice rather than as
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
  comes off the query string at most of these call sites, so a typo or a
  hand-edited link arrives as a value no option matches, and a
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
  (`download started: turn-….json`) because a reader who cannot see the
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
  is what `.t-link` on every "…No Mattermost bot for this seat. **Open
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
a grid row, a crumb, the user block, a work row, a turn row, a feed row — used
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
   measured*, never *took no time*: an executor's rounds ran inside its
   coding CLI's own loop in another process, and that run's wall clock is on
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
   **Tools**, **Approx. tokens** — so the total can be checked against the row
   rather than taken on trust. *Tools* is the tool-definition array as compact
   JSON, with the definition count on its hover, and it is usually the
   largest: while the engine did not measure it, a turn reported ~6,900 tokens
   on a prompt the provider billed 205,000 input tokens for. For the executor,
   *System* and *User* are the system prompt and the brief its run was
   launched with. Four fixed columns beside the phase overflow a phone, which
   is what the sideways scroll on `.num-block` is for.

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

   Nor does a parked turn put two there: the executor publishes its prompt
   once, when it launches its run, and the collection that re-enters the
   *same* phase at the *same* iteration sends no prompt of its own — the
   run's was the one it was launched with — while a send-back's relaunch is
   the next iteration, a row of its own. So a row is one prompt this turn
   sent, and the panel draws every measurement it is given, in publish order,
   rather than folding any under a phase key: a fold reads two different
   prompts as a range of one quantity, which is the one thing this panel must
   never say.

   The figures are **bytes** — `len()` of a Go string, and the wire keys say
   so (`system_bytes`, `user_bytes`, `tool_bytes`). They only
   agree with a character count on ASCII, so a roster of non-Latin names or a
   chat thread with emoji in it would make "the count" a different quantity
   per company under a character label. See `PromptSize`.

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

`#screen-scroll`, the shell's own main region, is what scrolls a page, and the
document as a whole never does. The sidebar is a fixed column beside a
scrolling sheet, so a page that can *also* scroll as a whole carries the
sidebar off the top of the window and leaves the reader looking at background
below the application, with two scrollbars and neither obviously the one they
want. The frame is the design system's `AppShell`, which is exactly one
viewport tall, and `base.css` gives `body` a fixed `100dvh` with its overflow
hidden: the document is not allowed to grow, so the invariant is unreachable
rather than merely unused.

Two other kinds of scroller sit inside it, and both are covered below. A
**full-height screen** (`useFillScreen`, whose doc in `app/fill.tsx` is the one
list of the screens that ask and of when each withdraws) holds `#screen-scroll`
still. A canvas fills it and pans rather than scrolls; a screen of two columns
lets each column scroll on its own, as a task does with its body and its rail
(`.task-main`, `.task-rail`) and the Inbox with its list and its pane
(`.inbox-list`, `.inbox-pane`). Each column is exactly the main region's
height, so `100cqb` is its view too, and on that screen those columns are the
page's scrollers: a box inside one hands the wheel to the column, not to
`#screen-scroll`. Chatlet asks for the height FLUSH (`useFillScreen(true, {
flush: true })`, the kit's `fill="flush"`): its conversation runs edge to edge
under the page bar, so the content column drops its inset, its measure and
its gap. And a **box inside the page** may scroll its own content, on
the two conditions below.

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

**A box inside the page may scroll its own content, on two conditions.** A
running phase's transcript, a seat's thread list beside its open thread, the
Settings column, a board lane's body and a long record each scroll inside
themselves, and each is a second thing under the reader's wheel — so each has
to keep the page within reach:

- **It is bounded by the scroller's view, never by a literal.** The shell's
  `main` is a size container on every screen (`container: scroller / size`), so
  a box asks for `100cqb` — the window less the page bar, which wraps, the
  sheet's inset and any banner, none of which a descendant can count — and
  subtracts what covers or must accompany it (`--sticky-top`, a board lane's
  head), with a floor of four rows for a window too short for that. The
  requirement is that the BOX fits once scrolled to, not its caption: the
  transcript's bound used to subtract one line of its caption, which wraps in
  a narrowed column, so the fit held only while the caption did. A `px`
  height or a `100dvh` sum is a guess at that number: too tall on a short
  window, where the box's own bottom then sits below the fold, and too short
  on a tall one for nothing — the board's lanes were `min(70dvh, 760px)`
  until the gate below learned to see every scroller rather than the two it
  named. Whether such a box sits beside what it serves is a container query on
  the screen's width (`@container page`), never a window media query, because
  the sidebar and a peek take width the window does not report.
- **At either end the wheel is the page's.** No in-page scroller sets
  `overscroll-behavior` to `contain` or `none` on the block axis: a box whose
  end is reached while the page has more to show must let the page move, and
  containment there is the wheel doing nothing. The one exception is a box
  that is always wholly in view and so cannot hide anything — the sticky
  Settings column, exactly the view's height, where chaining would move the
  section beside it instead.

**A long record is bounded once, in the cascade.** Every block of machine
text the kit's `CodeBlock` draws — a tool's arguments and result, a phase's
verbatim prompt, an event's payload, a configuration, a run's transcript —
reads its ceiling from `--crewlet-codeblock-max-height`, and the shell declares
it once on the two scrollers a record can sit in, the main region and the
peek's body (itself a size container for this): `--record-ceiling`, the view
less the sticky band and a label's line, floored at four rows and capped at
`--record-max` (460px, the height these records have always had). A running
ledger narrows it to its own box less the record's label. It used to be a
number each call passed, and nineteen of twenty-one blocks passed the bare
460 — so on a window whose view was shorter, a record's bottom sat below the
fold while the wheel was inside it, and a sheet gate could not see a ceiling
that arrived as an inline style. A block states its own only where it
genuinely differs: the YAML dialog, which is portaled outside both scrollers
and takes the root's cap, and the trace's paired Input and Output, half the
ceiling each, derived from it.

`styles/scrollers.test.ts` holds all of it. It scans EVERY rule in the sheets
that scrolls its block axis with a declared bound, and passes one with a
literal bound only from a roster that says why it cannot hide anything — an
overlay or a dialog the page's fold does not reach (the secret completion
list, the filter menu's list, the composer's and the setup dialog's
scrollers), or a box shorter than any view (a finding's 9rem of chips); a
roster entry that stopped being true fails too. Its containment half keeps
the Settings column as its one entry and its reason. It reads the record
ceiling on both scrollers and in the running ledger, and the source for any
`CodeBlock` handed a `maxHeight` outside its two named exceptions, or a number
inside them. A sideways strip is not its subject, since it never holds the
page's own wheel.

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
and full at once, depending on which screen it was drawn on. `refusing` is the
gate's own word — no room left for a single token — and it is the condition a
seat is parked on, so a parked seat's bar can never read as merely near. The
refusal stamp says when the gate last turned a call away — a refused charge, or
work turned away unsent because the window was already full (a turn's next
call, a parked delivery, a person's question, a reflection pass) — and only
beside a refusing window: under a ceiling raised since, the stamp is history
until the next admitted charge clears it, and the window is drawn by its spend.
A window that refused a round reads past its ceiling, because the refused round
is counted like any other (the vendor billed it), and its figures say so —
102 120 of 100 000, the spend the company was billed for, rather than the
ceiling it crossed.

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
the section tabs and `.crumb-link` already answer a pointer by moving to
`--color-text-primary`, and a link does the same: in dark the accent ink (#a9b0ff) brightens
toward near-white, in light the violet (#4f51bd) darkens toward near-black, so
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
`total_tokens` / `subagent_tokens` split means — and, the same way, the
**auxiliary** spend inside the turn (`+N auxiliary`: its turn-start context,
its ledgers' rewrites, its card), read off the turn's `auxiliary_spend`
records of the `turn` stage. The figure and its notes add up to the turn
list's tokens for the same turn. The reflection after the turn is never in
it: that is the seat's learning rather than the work's cost, and it is drawn
in the Timeline's Reflection lane instead.

---

### Worklet is a workspace, not a table

Everything above about rows is why Worklet's own screen is not one. It
began as a flat filter row over a plain table beside a "board" that was three
`<div>`s, and it read as a dump of a database rather than as the place a
company does its work. What replaced it is a WORKSPACE, and every part of it
is one of the rules on this page applied to a tracker.

- **A project is a SCOPE, not a shape of it.** Worklet's trail names the scope,
  All projects or one project, and switches it
  ([Worklet's scope](#worklets-scope-is-named-and-switched-in-the-trail)), each
  project with its key and its maintained open count. Its list, its board and
  its calendar are ways of drawing the scope you are already on, so a lens for
  each would make "Engineering ▸ Board" a destination competing with
  Engineering. The counts are the engine's maintained columns rather than an
  aggregate per poll, which is what lets the switcher and the lenses carry a
  number at all.
- **A SHAPE is a drawing, a VIEW is a query, and only the query was saved by
  anybody.** [A filter is a chip](#a-filter-is-a-chip-an-arrangement-says-what-it-is-set-to)
  states the general rule; what is Worklet's own is where the line falls.
  The engine ships SIX builtin views — one per shape, plus the trash — and
  neither the strip nor the sidebar draws them, for two different reasons worth
  keeping apart. The STRIP drops them because a way of drawing is not something
  somebody saved: mixed in beside an arranged query, the two read as the same
  kind of thing, which is why `view=` and `shape=` are two keys. The SIDEBAR
  drops them because a builtin carries no id, so there is no row to address.
  The five shapes are the first row's own segmented switch, and the strip is a
  second one in its own well. Both are radio groups with manual activation,
  because each choice is a URL change that re-runs the list: the arrows move
  focus and Enter or Space chooses, so walking a row leaves no history entry
  per keypress. A strip with more views than the row holds wraps inside its
  well, as every kit segmented row does, and a long name is cut at 200px with
  its title carrying the rest. The strip is the container's own list, the views THIS reader pinned (★) plus whichever one
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
  row is the sticky band, so it stays ONE line that scrolls sideways: wrapped, it stood three rows tall and covered the
  board's lane heads as the page scrolled under it. It fades the edge with
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
  that are rows in columns. What is Worklet's own is the KEYING: `cols.list=`
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
  person with the seat's STATE as the glyph on its corner; the dashed "nobody" square
  sits there when nobody holds the task, so an unclaimed card says so on every
  card rather than floating alone in a foot. Then the title, clamped to two
  lines. Then the facts: priority, blocked, the labels, the due date, "blocks N"
  (the live tasks waiting on this one — the reason to pick it first), the open
  questions and, at the end, what the task has cost in TOKENS. A card on a task
  nobody has touched has no fact row at all rather than an empty band, and the
  predicate that decides restates each mark's own emptiness rule — the price of
  asking BEFORE rendering — so a mark that gains a field adds it there. The
  last row is a STATE and the only part of a card with a hue: blue while a
  seat's turn is on the task ("SWE · coding run · 6m"), amber
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
  `place_work_item` as the principal the session resolves to, naming the card it went
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
  list of finished work (`lib/work.ts` holds `TYPE_ICON` and `STATUS_SHAPE`
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
  should overlay below about 1500. That measured a peek Worklet's own
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
  is not sortable: Lead is resolved against the org chart at read time and
  Worklet holds no chart, so there is no column behind it.
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
  same about its pickers' counts (once, rather than under each of three
  facet rails, which the one compact bar Worklet narrows from replaced), and the histogram takes that scope as a REQUIRED
  prop, so the sentence a screen reader hears cannot drift from the one on the
  card. A client-side count dressed as the engine's is the one thing this
  product never does.
- **ONLY A SCREEN PUBLISHES TO THE FRAME.** The log is a screen at
  `#/worklet/history` and a lens inside a project, and the frame holds one
  coverage slot with one setter — so a body publishing from inside another
  screen fought that screen's own answer: on a project's history lens both
  wrote it, whichever polled last won, and switching back to Tasks cleared the
  slot to nothing over a page still showing a project it had read. The lens
  publishes nothing and states its own rows' coverage inline, which is what the
  Tasks lens beside it already does. It is a COMPONENT rather than a bare call
  because the rule is conditional and a hook may not be: called with nothing,
  the publish writes null over whatever the enclosing screen published, which
  is the same clobber under a quieter name.
- **A project opens on its work.** The Tasks lens is the list at the top of
  the page, as the approved Board draws a project: no header about the
  container above the work, and no row of lenses either — the lenses sit on the
  page bar beside the name they are lenses on (`Worklet › ENG Core · Tasks | About
  | History | Files`, `PageLenses`, the chips All projects' lenses wear too),
  controlling the region below by id. A row of their
  own cost the first screenful a line the Board does not spend. What the container IS — who leads it, its unit,
  its target, its census, its vocabulary and its latest changes — is the
  **About** lens; a finding on the project's own record (a unit the chart lost)
  is drawn over every lens, because it is a fact about the work below. The
  page bar ends with **Edit project** — the lead's target date, a company write
  (`write_project{target_date}`) whose authority the engine decides, titled
  with the project's NAME and confirmed with the day as the Target fact writes
  it ("Set Core's target to 30 Oct 2026", never the wire's ISO form) — and
  **New task**, filed into this project. A project that declares no purpose
  says so in a sentence for a person ("Add a purpose in Edit org."), not a
  configuration key in backticks.
- **A lens keeps its filters.** The lenses over a project (Tasks, About,
  History and Files) are readings of ONE container, not separate screens,
  so switching keeps the narrowing AND the arrangement: `lens=` is written
  through the frame's section move, which copies the whole current query and
  sets one key. A lens that built a fresh query would make History a one-way
  trip, with the way back being every chip re-added by hand — and it would fail
  silently, because each lens renders perfectly on its own. Only Tasks carries
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
  `dashboard/src/routes/worklet/census.tsx`, in the About lens's header, the peek
  and the directory's Progress column (beside the lead's Target date), so they
  cannot disagree about one row.

---

## The turn trace: four tabs, one clock

`#/live/turns/{id}` is where every deep link to a turn lands, and it is read
in two ways: *what did this turn do, in order and how long*, and *what is it
doing now*. So its header says where the turn is and what a reader can do
about it, and its body is four tabs (`tab=timeline|transcript|context|tools`).

**The two readings are two kinds of link.** A link into a turn's RECORD — a
row of a list, a settled turn's *Trace*, a monitor's row on Home's Live now and
Live › Now running — opens the page on its Timeline, the default tab. A link
whose job is to WATCH a running turn — every **Watch live** (a seat's current
turn card, its profile's page bar and its peek, a task's live row and its page
bar), the sidebar's **Running** group,
`g r` and ⌘K's **Running now** — opens it on the
**Transcript** (`?tab=transcript`, `lib/turns.ts`' `watchLink`), where the
phase the turn is on is drawn OPEN with its rounds arriving. Landing on the
Timeline, a reader who pressed Watch saw one drifting bar and had to find the
tab, then open the phase, to read a word of it.

**A parked turn is watched on the same tab.** Its executor launched a detached
coding run and suspended, so no phase is in flight while the engine still
calls the seat working — for most of the turn, and for hours on a long
one. So while the turn is parked the Transcript
leads with a **Coding run** card: the run's live output (the same
`sandbox_tail` poll the Timeline's run span opens, read off the waterfall's
own open run span), the coding agent, and **Open the run** to the run's page
(`#/live/runs/{turn_id}`); the executor's card it parked in is drawn open
beneath it. A turn that parks while a reader watches therefore keeps
something live on the page, which sending a parked turn's watch link
elsewhere could not have done.

**The page moves once, on arrival, and only for a reader who has not moved
it.** The live phase's ledger tails its newest round at the bottom of a box as
tall as the view, and under the header and the cards above it that bottom
landed below the fold — the one round a reader came to watch was the one
they could not see. When the page is reached on the Transcript and the turn's
answer has landed (the stored phases it carries are drawn above the live one,
so deciding earlier would be deciding about a box that is about to move), the
shell's scroller (`lib/scroller.ts`) is moved until that box's bottom is in
view and never past its top — and only if it is still at the top. Never on a
later push, and never by `scrollIntoView`, which would scroll every ancestor
that can.

- **The trail names the turn the way its task does.** On a work item it reads
  *Live › {agent} › Turn n · KEY* — the ordinal off the task's own turn list
  (`work_item_turns`, asked by the task's id and walked a page at a time until
  the turn is found; a turn still running is the next one after the newest,
  and before any of that is known it is *Turn · KEY*, never a guessed number —
  see [An item's address](#an-items-address)). Off a task it is the lead
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
  next step (`steer_turn`) — offered on every turn still going, a turn parked
  on its coding run included, whose note waits on the run's record, and held,
  with the reason, only on a turn that has ended. The toast says when the turn
  reads it, from the answer's `reads`: *at its next step*, or, for a run waiting
  on a question, *once that is answered* (`lib/steer.ts`). What became of a note
  is the turn's own `agent_turn_steered`, drawn under the waterfall and where it
  was read — at the round (*read at round 2 of Review*), or for a read inside a
  coding run on the run's bar, in the words of its step, owned by the phase
  whose run it was: *read by the coding run of Execute in its brief*, *after a
  tool batch*, *as it was finishing*, *at its next step*; a step a newer build
  names and this one does not is *read by the turn in Execute*, on the run's
  bar where its record names the run and never drawn or said as a round — or
  *expired — the turn finished before it read the note*. *Pause* is the seat's
  (with *Also stop the current turn*), *Open task* its item — by the task's
  id, since a turn's item carries no word on whether its key is one another
  task claimed first — and one menu
  (*More on this turn*) holds the other attempts, the traces, and the turn as
  JSON — *Copy turn as JSON* and *Download turn as JSON*, each saying what it
  did in a toast. On a phone Steer stays in view and everything
  else folds into the frame's *More*.
- **Timeline is a waterfall on the engine's own clock** (`lib/waterfall.ts`).
  Every instant is one the engine measured — each round's model call, each
  tool call, a phase, the prefetch, a coding run — and the model only arranges
  them: a phase's CLI runs under it, each a **CLI run N** span from its launch
  to its exit (`invocations[]`) and its rounds under it — the entry that served
  it, the rounds it made, whether its CLI's record showed them, how it ended and
  what it spent in its detail, and the run still going drawn open from its
  launch (`run_started_at`), saying what it made lands when its CLI exits —
  a round's tool calls after its model call (a
  call the engine timed but did not stamp is run serially from the model's
  answer and says it was *placed*), a delegate's workers and the round-cap
  judge under the round that spawned them (`host_round`), each coding run as
  its own span keyed by its job's `launch_id`, the reflection pass after the
  last phase — its workers drawn from what each SPENT, one span per
  `auxiliary_spend` record of the `reflection` stage from its first call to
  its last, marked where a call failed — and *review pending* while an executor has finished and no
  reviewer has started. The ROUND IN FLIGHT is drawn only from a start the
  engine announced for it: a request is in flight from the frame published as
  its run opens or as the last call of the response before it is answered, and
  `round_started_at` stays the previous round's while that round's tools run —
  so a start no later than the last recorded round's is that round's tools,
  never "the next round" drawn minutes early. Several calls of one response
  running side by side are each drawn as running (`running_calls`). The Turn row
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
  the trace asks the node that can read the run's box for its live output
  every three seconds (`sandbox_tail`, see [Watching a run
  live](../concepts/code-sandbox.md#watching-a-run-live)), and only then:
  closing the span or the job stopping ends the poll — and only
  `not_running` is stopping: a job still being set up (`launching`), a box
  paused before its record moves on (`box_paused`), a box gone before its
  record moves on, the job recorded lost and its round carried on in a
  fresh one (`box_gone`), a silent node and a failed read all keep asking,
  and keep what is on screen. The one failed read that
  ends the poll is a refusal the engine says no wait clears (`unavailable`
  whose `retry_after` is zero: a sandbox setup somebody has to fix, a refused
  key), since every ask would be answered the same and logged on the node that
  reads the box; a reconnect, or opening the view again, asks again. A failed
  read is told in the engine's own sentence for it (`QueryState`, as every
  screen tells one), never by its code, over what the view already shows.
  - **The view holds what it was sent.** It asks by CURSOR — the reading it
    holds, how far and the owner's digest there — and APPENDS what the owner
    says it lacks, or replaces what it holds on a reset. `lib/useLiveTail.ts`
    is that hook, and not `useQuery`, because the cursor moves with every
    answer and a cursor in a query's parameters re-runs its effect the moment
    an answer lands. It holds at most what the run's record will
    (`LIVE_OUTPUT_MAX_BYTES` in `contract/sandbox.ts`, the engine's
    `sandbox.MaxRunTextBytes`, held to it by a gate), dropping its FRONT on a
    line past that, and the caption says how much earlier output it no longer
    holds — every figure read from the answer or the view's own count, never
    spelled on the screen. A delta that does not follow what the view holds is
    never spliced in: the view keeps its text and asks for a reset.
  - **It follows the end unless the reader scrolled up.** At the bottom (within
    about a line, `FOLLOW_SLACK_PX`) the block scrolls to each new line;
    anywhere else it stays where they put it.
  - Bytes the owner holds back until their redaction is settled — a line not
    finished, a private key whose end has not been written — are counted
    under the block rather than shown.
  - *No output yet* and *node-b didn’t answer* are different sentences, and
    the second names the node. A screen reader
    is told the tail's STATE — one sentence in a status region, which changes
    only when the state does — never the output or the "read 3s ago" clock,
    which change every poll.
- **Notes sent to a turn** name their sender as a seat's name, the way every
  other attribution does, and say what became of each — read at a round, or
  expired — without assuming the reader sent it.
- **A turn with no closing record** is *running* while a seat is on it and
  *not settled* when none is — the same reading on the trace, the Live turns
  list and a seat's Turns tab — and neither while the live view is
  disconnected, when there are no seats to ask.
- **Transcript** is the phase cards and the story bands above — the first
  phase open and, while the turn runs, the phase it is on: a card opens as it
  MOUNTS when its phase is live, so a phase that starts while the page is open
  arrives open, and one that completes keeps its card and its open state
  (phase cards latch; the reader's own open and close is never overridden); **Context** is
  what woke the turn and what its prompt was assembled from; **Tools** is every
  execution with where it ran (built in, or the MCP server), each row opening
  its span on the Timeline.
- **The answer is the fleet's.** A node that did not answer the turn read is
  named above the tabs; the answer is asked again whenever one of the turn's
  phases lands on the stream, its seat's stage changes, or its reflection
  pass's `reflection_completed` lands, because what the phases do not carry —
  a run's announcement, a note's outcome, what the auxiliary model spent, the
  reflection — arrives only in the answer. The engine publishes a turn's
  in-turn `auxiliary_spend` before its end and a pass's before its sentinel,
  so the asks at those two moments read them.


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
    held, and carries **Answer**: `answer_run{turn_id, launch_id, choice}` for
    an option, or `{turn_id, launch_id, answer}` for words, which reaches
    a run whatever launched it. `pending` is the ordinary outcome: the answer
    is on the seat's inbox and the node holding the seat resumes the run.
  - A running run's page polls its live output from the node that owns it
    (`sandbox_tail`, by the `launch_id` the row names, every 3 s while the page
    is open — the same `LiveOutput` the turn trace draws). The view is keyed
    on the turn AND the launch, so a later job starts a view of its own rather
    than appending its output to the last job's; an answer naming the job
    `replaced` reads the board again at once, so the page follows the job the
    row holds now rather than waiting out `RUNS_POLL_MS`.
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

### A refusal is read, never parsed

Every refused HTTP request answers the same three-part
[envelope](api-endpoints.md#every-refusal-is-one-envelope), and the client
treats each part as exactly one kind of thing:

- **`message` is rendered VERBATIM.** It is product copy the engine holds one
  sentence of per code, so the client shows it as sent — never reworded, never
  truncated into a toast title, never prefixed with the code. Nothing branches
  on it, and nothing matches its text: a copy improvement in the engine must
  not be a screen that stops working.
- **The detail is what the client KEYS ON.** Everything beside `error` and
  `message` is machine-readable and typed, so a screen reads the field it needs
  and acts: `fields` puts a marker beside each input an integration is still
  missing, `problems` puts a located failure beside the line of the document it
  is about, `current_revision_id` tells a lost update from a stale form, a
  `503`'s `refusal` names the state log's reason. Parsing a
  sentence to find any of those is what this envelope exists to end.
- **`error` decides WHICH screen state it is**, because it is the only closed
  set of the three. A code the client does not know is rendered with the
  engine's `message` and no behaviour attached, which is the honest fallback: a
  refusal nobody anticipated still reads as a sentence rather than as a blank.

A refusal with no `message` — a route whose code has not joined the shared
vocabulary yet — falls back to the client's own line for that code, the same
table the socket's query errors use.

---

## Chatlet: a top bar, a Rooms panel, a room, and where you have read to

The company's own chat (`chat.backend: chatlet`), read AS A SEAT: the engine
answers every chat question for the seat the session's principal holds, so the
workspace is guarded whole and a principal that acts as no seat is told so and
how to give it one (see [The sidebar](#the-sidebar)). On a company whose chat
is a vendor's or none there is no Chatlet: its row is hidden and its addresses
say where the company talks, as every service the company does not run is
(see [The sidebar](#the-sidebar)).

Chatlet runs edge to edge under the page bar (the shell's flush fill), and
every place in it is an address: a room by its id, or one of the views beside
the rooms, with at most ONE pane named by the `pane=` parameter (see the
address table above), so Back closes a pane, a reload keeps it and a thread is
a link from any place (`routes/chatlet/nav.tsx`). While it is open, a toast
stands under its top bar at the top right (`app/toastInset.tsx`'s
`useToastTop`), held off the window's top by where the bar ends: at the
window's foot it would lie over the room's composer and a thread's, which grow
as they are typed in.

- **The top bar** holds, on one line and at one height: **Menu**, which slides
  the Rooms panel in from the start edge of the chat area (or hides it while it
  is pinned), its badge the sidebar's own figure of what waits on you; the
  **Rooms** switcher, named after the open room, listing your rooms (Starred,
  Rooms, Team rooms) with Browse rooms and New room at its foot; the
  **Messages** switcher, named after the open conversation or Direct messages,
  listing your conversations and then the people and agents to start one with,
  with New message at its foot; and the open room's actions: its members (three
  faces and the count, which open the details' Members), Search this room, the
  notification choice (Every message, Mentions, the default, or Muted), and
  **More**, which leads with **Catch up with Ask Crewlet** (or **Summarize**,
  with nothing unread) and then the details, what was shared, Add people, the
  description and topic, the star, Mark as read, Copy link and Leave. Each is
  offered only where the engine would take it, by the room's one reading of its
  rules. Ask Crewlet itself is the page bar's button, so the bar has none.
  While the panel is pinned it lists what the switchers do, so they are not
  drawn; a narrow bar drops Menu's word, and under 480px the search and the
  notification choice move into More.
- **The Rooms panel is the row's renderer** (`panel`, `app/nav.ts`): a find row
  (the filter, a menu for unread rooms only and the order, and the pin), the
  row's views (Activity, with the accent badge of what waits there; Threads;
  Drafts and sent; Saved), then the rooms in four collapsible sections
  (Starred, Channels, Team rooms, Direct messages) in dense rows: a room with
  unread messages in the primary ink and the medium weight, a muted one dimmed
  with a bell, one holding a draft with a pencil, and the accent badge of what
  waits on you in it (in a conversation, every unread message; elsewhere, the
  unread messages that name you or the whole room). A collapsed section still
  shows the open room and the unread ones. The counts are the ENGINE'S, and
  when the cursors could not be read the badges are absent and the panel says
  so, because a zero would claim the reader is caught up; a list past its bound
  says so too. Browse rooms and New message stand at its foot. Pinned (a
  per-browser choice), it is a column beside the conversation; on a chat area
  under 760px it is never pinned.
- **Panes** stand beside the conversation, and lie over it on a screen under
  980px (a whole narrow chat area under 760px); the widths are
  `app/layout.ts`'s, which keep the transcript to its floor at every step.
- **Keys** (the legend's "In Chatlet"): `Alt+Up` and `Alt+Down` move between
  rooms, with `Shift` between rooms with unread messages; `Mod+F` searches the
  open room; `Escape` marks the open room read when nothing else is listening
  for it (a pane, the slid-in panel, a peek, a menu); `Mod+/` lists Chatlet's
  keys.
- **A room** (`#/chatlet/{id}`) is the transcript, paged back on request, who is
  looking at it, typing in it or working on a reply in it, and the composer
  (`Mod+Enter` sends; `Enter` is a new line). A message's links are drawn under
  it, in the room and in a thread alike, as rendered prose draws an address in
  an agent's words (`links: "checked"`,
  [above](#what-prose-names-is-drawn-as-what-it-is)), whoever wrote the
  message: an address on a host the company configured opens as the dashboard
  rebuilds it, in a new tab, a GitHub or GitLab address only under an owner
  that host's row names; any other, a link to the company's own dashboard
  among them, is drawn whole and inert with the host it names; and one whose
  scheme the allowlist refuses is its words. Ask Crewlet reads
  every box open on the room as one draft, the message, the reply in a thread
  open beside it and each message open for a rewrite, and proposes text into
  any of them; Send, Reply and Save stay the person's. A room that takes no messages from
  you — archived, a private room you are outside, a kind this build cannot
  classify — closes the box and says why in it; a browser that cannot send
  (offline, a reader the engine posts nothing for) leaves the box open and holds
  Send, with the sentence written beside it, as every composer does. Its
  membership, notification and settings gestures are the top bar's (above).
  Each of a message's changes (Edit, Delete, a reaction) is held by its own
  tool, because the engine serves each to a person on its own — and by that
  tool's own rule in the room rather than the composer's: an archive closes a
  room to new messages and to nothing else, a delete reaches past membership,
  and every one of them is drawn and held with its sentence where the engine
  would refuse it, never hidden. A room whose own read failed says so on every
  control rather than reading for ever. A public room or a unit's room is
  readable by every seat, joined or not, so arriving from a search outside a
  room whose whole transcript is on screen is ordinary. A public room's header
  then offers to join it; a unit's room offers neither Join nor Leave, because
  its membership is the org chart's — a seat is in it by being in the unit — and
  says so instead. A message is not drawn until the engine has it: what you sent
  waits beneath the transcript until a read carries it — with **Send again**
  where nobody can say whether it landed (asked under the same operation key,
  because the engine derives the message's id from it and a fresh key would
  say it twice), **Try again** where the node refused it, and **Take it off
  the screen**, which unsays nothing. A message the node accepted without having
  applied it yet (`pending`) is not yet decided — the room may have been
  archived, or its author taken out of it, just ahead of it — so the screen asks
  again under the same key, first after the two seconds the node's applier is
  given to reach it (`contract/chat.ts`'s `PENDING_REASK_MS`, the engine's
  `chatlet.DecisionWait`) and then at doubling intervals up to thirty seconds,
  until the answer is a decision: it landed, and the transcript brings it, or it
  was refused, and the row says why. It waits there while you look at another
  room, and is there when you come back.
- **The panes** draw in the kit's side panel, so opening one moves focus to
  its heading, Escape inside closes it, and closing gives focus back to what
  opened it (to a menu's trigger, when a menu item opened it and is gone).
  **A thread** (`pane=thread:{room}:{root}`), opened beside the conversation
  or over it, is its root, the reply count, the
  replies as the room draws them, an agent writing a reply, the reply box
  where the room takes one, who a reply reaches, and Follow. It is one level
  deep, so a reply offers no thread of its own, and it pages forwards from its
  root. Its read place moves to the newest reply on screen. **Details**
  (`pane=details:{room}:{tab}`) groups the room's facts in cards (its name,
  which never changes; its topic and description, each with Edit where the
  engine would take the change; its team, who made it and when, how long it
  keeps what is said, and Leave where a person may leave), its Members (find,
  Add people where the engine adds anybody, people before agents, each opening
  their profile), what it **Shared** (every link said in it, newest first, as
  the room unfurls it, each opening the message it came in) and its
  **Notifications**. **A profile** (`pane=profile:{handle}`) is the seat's
  face, team and what it is doing now, Message, its page in the org chart, and
  the rooms you share with it (`chat_channels{with}`). **Catch up**
  (`pane=catchup:{room}`, or `summarize` with nothing unread) is Ask Crewlet's
  brief of the room: the gist, what waits on you, and what was said, each
  point opening the message it came from; it reads only what you can read and
  posts nothing, and goes on in Ask Crewlet.
- **Activity** (`#/chatlet/activity`) is every message that named your seat, in
  every room, every reply in a thread you follow and every reaction to what you
  said (`chat_activity`): the whole away story, because the engine sends no
  email, push or SMS. It filters to mentions, replies or reactions, and to the
  unread alone; the row chosen is read beside the list as the room draws it
  (`chat_message`: the message, and for a reply its root and the replies just
  before it) with a reply box under it, and choosing an unread row marks it
  read through its room's cursor. A narrow column draws no reading pane, so a
  row opens where it was said instead. Each row is the moment it happened and
  is dated by it.
- **Threads** (`#/chatlet/threads`) lists the threads you follow, newest reply
  first (`chat_threads`), each its root and newest replies with Unfollow and
  Reply; **Drafts and sent** (`#/chatlet/drafts`) lists what you have not sent
  (a message the engine did not confirm, and the words left in a box, kept in
  the tab and never in the address or the browser's storage) and what you sent
  (`chat_sent`); **Saved** (`#/chatlet/saved`) lists what you saved, the newest
  save first (`chat_saved`). A card there quotes its message read-only: its
  actions are a press away in the thread or the room.
- **New message** (`#/chatlet/new`, `to=`) takes people and agents as chips in
  its To field (`#` there finds one of your rooms instead), shows the
  conversation you already share with them, and opens one only when you send.
- **Browse rooms** (`#/chatlet/browse`) lists every room you can read, joined or
  not, archived ones under their own filter (`chat_rooms`), with each room's
  topic, size and last message, sorted by activity, name or size; Join where
  the engine would take one, Open everywhere else.
- **Search chat** (`#/chatlet/search`, `q=` and `in=`) ranks what was said in every
  room your seat may READ (wider than your own rooms) by keyword alone: chat is not
  in the vector corpus
  ([ADR-0071](../../adr/0071-chat-is-a-keyword-corpus-and-takes-no-vectors.md)).
  It searches as you type, once the words rest (`lib/useQuiet.ts`), and keeps
  the settled words in the address by replacement. In and From narrow it at
  the engine (`channel_id`, `author`); Who (people or agents) narrows the page
  the engine ranked, and says so. A phrase past the engine's bound is not sent,
  and the screen says why. It asks for the engine's own page (forty hits,
  `contract/chat.ts`'s `CHAT_SEARCH_PAGE`, the engine's
  `search.MaxChatSearchLimit`), and the ranking has no next page, so a full
  page says there may be more matches and how to narrow the search, rather
  than reading as everything that matched. Its People tab matches the org
  chart's seats and its Rooms tab every room you can read, on the words each
  row draws.
- **On Home**, Chatlet offers two widgets for the reader's board
  (`routes/chatlet/home.widgets.tsx`; see
  [Home is the reader's board](#home-is-the-readers-board)), each asking what
  its page asks and re-reading on the frames its page re-reads on: **Unread
  rooms**, the rail's rooms with something unread and the engine's count on
  each (a muted room left out and counted, a rail cut at the engine's bound
  said to count only the reader's most recently active rooms, a room a newer
  engine made counted as left out, a rail that met a record this build cannot
  read said to be incomplete, and where the cursors could not be read a
  sentence saying so, none of them ever drawn as "caught up"), and **Mentions**,
  Activity's newest mentions, a mention in a thread opening the thread. The
  rooms are the frame's one reading of the reader's rooms, the one the
  sidebar's badge and the Rooms panel draw. Neither marks anything read. Like every chat page
  they are the Chatlet app's: a credential acting as no seat is told so in a line
  and asked nothing, and a company whose chat is a vendor's or none is offered
  neither, as Home offers no widget of a service it does not run.
- **Live, by evidence.** A chat frame carries no body: it says a room moved, and
  the screen re-reads, a burst coalesced into one read. What a reader has paged
  to is read again only where a frame names something on it: a room's newest
  page on every frame in it, and then — once that page has caught up with the
  frame on the log — the page that ends at an edited, deleted or reacted-to
  message it does not reach, or every page held below it for an erase or a
  prune; a thread's first page for its root, the page holding a changed reply,
  and its last page for a new reply; Mentions' newest page on every frame, and
  a page held below it for a change to a mention on it, an erase or prune in a
  room it holds a mention from, or a membership change. A page read again is
  put back only once it still meets the page after it: a room joined makes the
  mentions said in it visible, so a page can come back holding the same number
  over a shorter range, and the range it no longer reaches is read in before
  anything moves rather than drawn as a hole. A tab that let a frame go unseen —
  a bound, or a frame naming no room, which is the engine saying it dropped
  some — reads everything it holds again, because what it missed is unknown.
  Presence is pushed whole for the rooms the tab says it has open. Your seat's
  read state reaches every tab of yours as `chat_cursor_moved` — the one that
  wrote it included, which also has the write's own answer — and the rail asks
  again once when its revision moves, whichever of the two lands first.
- **Where you have read to** is written as you read — every `READ_FLUSH_MS`
  (15 s), at once on leaving a room, hiding the tab or closing it — through
  `mark_chat_read`, the same audited act every other change is. A badge
  therefore clears when a flush lands, not on the scroll. A message counts as
  read only once it has been on screen while the room is in view: the tab in
  front, and nothing over the page. Ask Crewlet as the whole window or over
  the page's end edge covers the room, and so does a dialog or the palette;
  Ask Crewlet's column only narrows the page. What arrived while the room
  was covered counts once it is in view again.
- **A seat that moves is a new screen.** A write goes out as the seat the
  engine resolves when it arrives, so the whole screen is mounted under the
  seat the viewer names and is drawn again when the directory binds the person
  elsewhere: the last seat's unsent messages with their Retry, a draft, an open
  thread and a half-filled dialog go with it, and what it had read and not yet
  written is not written as the next seat — a cursor only moves forward, so it
  would mark rooms read for good. A different person signing in hands the
  whole tab over by a reload with its storage emptied, which is the same thing
  for every screen.
- **Offline**, chat is every other socket screen: it keeps what it last read,
  the frame's banner says the live channel is down, and it reads again on
  reconnect. Nothing is said while the socket is down; a message is never
  queued for later.
- **On a narrow screen**, a phone's or one Ask Crewlet's column has
  narrowed, the rail stacks above the room and each takes its share. The
  screen still takes the window's height, because a transcript inside a page
  that also scrolls is two scrollers under one wheel. The widths are in
  [The frame](#the-frame).

## Doclet: the spaces, a landing, and search with real modes

Doclet is the approved **Home** layout. No column of navigation stands
beside the screen (the workspace's section renderer is `none`, `app/nav.ts`):
the spaces are ONE ROW at the top of every Doclet screen, the search sits in
the page bar of every Doclet screen, and a page is read at the full width
of the sheet. The landing, a space and the search's answer take the sheet's
whole width as every screen's column does, with no cap of their own: capped at
the kit's content maximum, they sat in a 1600px column against the left edge
of a 2560px screen (`styles/frame.test.ts` refuses a screen that caps itself
that way). Agent skills and Agent diaries are reached from the landing, the
palette and the trail.

- **The spaces row** (`SpacesRow`) is a navigation landmark named Spaces: a
  link per space, each with the folder every space wears and its key
  (`KeyChip`), drawn with the design system's chip tabs. The space being read
  is `aria-current="page"`, and a space's page count and purpose are its title.
- **The search** is a field and, BESIDE it, the ranking mode in a menu:
  Hybrid, Keyword or Meaning, sent as `mode=hybrid|keyword|semantic` (the word
  for `semantic` is the reader's and is never sent). The menu sits next to the
  field rather than inside it, because a control inside a text box is one a
  pointer reaches by missing the box. Enter goes to the results with the
  phrase and the mode in the address, so a search is a place a reader can
  return to. The menu learns what the backend serves from the `knowledge`
  PROBE, the question asked with no phrase, which runs nothing and answers
  `modes` and, asked for `semantic`, the configuration reason the others would
  degrade (`no_embeddings`, `unsupported`). A mode the answer does not list is
  **disabled with that reason as the item's second line**, and before the
  probe answers nothing is disabled, because "not known yet" is never a reason
  to take a mode away. **The default is a mode the engine serves**: Hybrid
  where it is served as asked, else the first mode the probe lists (Keyword on
  a company with no embeddings provider). An address that names no `mode=`
  runs in that same default: the results wait for the probe rather than
  running Hybrid first and reporting "asked for Hybrid, served Keyword" about
  a choice nobody made. A mode the reader named in the address is theirs and
  is kept, and the answer says what served it.
- **The landing** (no phrase) is TILES SIZED BY WHAT EACH IS WORTH, on one
  screen (the approved Bento layout). The newest save, comment, rename or new
  page across the company takes the big tile, OPEN TO READ: the page's space
  and the way into it, its title, who changed it and when, what each save said
  (back-to-back saves of one page by one writer on one company day are one
  row, "saved it twice"), the lines the newest save changed as the page's
  History draws them (six at most, with the count of the rest and a link to
  that save), and the page's opening, never ending on a heading with nothing
  under it. That costs ONE `page` read, of the featured page alone, and the
  `page_revision` before the save; a change to a page since purged never
  leads. **Recently written** runs down the right with the rest of the same
  `page_activity` read, never the featured change again, under the COMPANY's
  days, Today and Yesterday cut at the company's midnight. Each row says its
  time of day on that same clock (its age is the time's title, since an age
  under a date heading contradicted it) and its note: a save's own, a
  comment's words or a new page's first line, never the title again. **Every
  change** opens the audit log narrowed to Doclet's changes. With nothing
written beside the featured change the column is not drawn, since the big
tile is then the whole of it. The tiles share one inset and three equal
columns. Four small
  tiles list three pages each and say one line when empty: **Continue
  reading**, the pages this browser opened last, and **Pinned**, the pages
  this reader starred from a page's More menu, each page read for its title
  now and who last saved it; **Watching**, the pages this person follows
  (`pages{watcher}`, asked by the seat they act as, and not asked at all for a
  credential bound to no seat, which follows no page and is told so); and
  **For the agents**, Agent skills with the engine's
  count of published tool skills and Agent diaries with how many agents keep
  one and when the newest reflection was written. The grid places each tile by
  a named area, so the markup keeps the reading order at every width: below
  928px the feed moves under the newest change and the small tiles pair up,
  and below 640px everything is one column. On a company whose knowledge lives
  in a vendor's wiki there is nothing on the engine to browse, and the landing
  says so rather than drawing an error.
- **The results** are the ranked answer GROUPED BY SPACE beside a PREVIEW of
  the chosen result. Grouping never reorders the engine's rank: a group is one
  space's hits in the engine's order, the groups follow their best hit, and
  every hit keeps its rank number; **Ranked** draws the one list as it came. A
  space filter narrows what is shown of the ranked answer, because the
  engine's search takes no space. `j` and `k` move the choice, and the preview
  reads the chosen page: its space, Author, Last saved, Read by agents, Linked
  from, and its opening with the searched words MARKED (`Highlight`, never
  inside a word).
- **Results say what ranked them**: "ranked by Keyword" in the results' head,
  a sentence above the hits when the engine served something other than what
  was asked (`served_mode`, `degraded`), and a warning when the fan-out was
  PARTIAL, naming the buckets nobody scanned and the node that did not answer
  (`coverage`), because a short list from two thirds of the corpus is
  otherwise indistinguishable from a small corpus. On Doclet the
  search reads this node's own copy of the pages. The results end with how to
  move through them (`j` and `k` move, Enter opens).
- **A search that did not run** says which of `no_company`, `no_backend`,
  `no_scope` or `building` it was, with the remedy that reason names: a
  backend to set, spaces to add to `knowledge.scope`, or, while a node that
  joined recently is still indexing, to try again shortly. The fifth reason,
  `no_access`, is the READER'S rather than the company's (a caller holding
  Worklet and not Doclet, whose question searched nothing of the knowledge
  base) and is said in the words every lock uses ("Search needs Doclet
  access. An admin can give it to you."), with nothing to configure. The
  dashboard asks the question only of a Doclet holder
  (`lib/useSearchQuery.ts`), so it is drawn only for an answer that raced an
  access change.
- **A space's peek** (opened from a list elsewhere) reads one window of 500
  and COUNTS off the listing's `total`: Recent pages and "n more pages in this
  space" are the space's, never the window's. When the space outgrows the
  window, Recent pages and Who writes here each say they cover the first pages
  by title, since the listing is ordered by title rather than by time.

Who files into a space and the tracker project it works in are named for a
reader who may read the company document — an admin — in four states: read
(which may be "no unit names it"), not held (the reader does not hold Admin),
read in flight, refused. "Needs Admin access" is said only to a reader without
it. A
project the space's key does not already name is drawn beside it as a dashed
key, not only in a tooltip.

### A space: its head and its outline

`#/doclet/{CONTAINER}` is the space's head over its pages as an OUTLINE. The
head names the space and its key, what it is for, how many pages it holds,
when somebody last wrote in it (the newest change to what a page says, from
the space's own change log, `page_activity{container, kinds}`, never the
newest row of a listing ordered by title), who files into it, the tracker
project its units work in where the key does not already name it, and when it
was made.

- **The outline** is the design system's tree grid (`TreeGrid`, `headless`):
  each row's title and its Draft mark, who last saved what the page says (the
  engine's `saved_by`, drawn as an author is below) and when
  (`saved_at`, never `updated_at`, which a label or a watch moves). Enter on a
  row opens the page.
- **One level at a time.** The space's top level (`pages{container, roots}`)
  and a page's level (`pages{parent}`) are each read the moment the row opens
  and never before (`defaultExpanded="none"`), in windows of 500 with **Load
  more** and "n of total". A row opens where the engine counted pages under it
  the same listing would show, and on a FOLDER (a page with nothing written in
  it) even when it holds none, so its Create row can be reached.
- **Create** ends every open level. **Document** goes to the composer filed
  where the row is; **Folder** turns the row into a name field, and Enter
  writes the folder at once as a page with an EMPTY BODY (`write_page`), which
  is what a folder is: the engine derives it on every read and stores no kind
  for it. Escape lets the name go. A name already taken in the space is the
  engine's refusal, drawn under the field. Create is drawn for every reader and
  disabled with the reason for one who cannot write.

### A page: the document, its outline, and what it is connected to

`#/doclet/pages/{id}` is the document in one column with its outline to
the left, following the scroll (`Outline`, `useActiveHeading`), so where the
reader is in a long page is always in view. Below the phone breakpoint the
page is one column and the outline is not drawn.

- **The page bar**: the eye, **History**, **Edit** and **More**. The eye
  watches or stops watching as the person (a `save_page` carrying nothing but
  `watch`), and the count beside it opens who is told: the watchers less the
  ones who muted the page, since an unwatch mutes rather than deletes. A watch
  is kept under the seat the person acts as, because a wake reaches a seat and
  nothing else, so for a credential the directory binds to no seat the eye is
  drawn disabled with that reason (the engine subscribes it to nothing). More
  holds **Star this page**, **New sub-page**, **History** and **Copy the
  page's address**. The star lives in More beside the page's other actions,
  and the page tells the frame to draw none of its own (`useOwnStar`), so the
  action is never in two places at once.
- **The properties**, minimal by rule: Space (with the pages above this one),
  Author (who started it), Last saved ("2h ago by SWE", with the revision and
  the note kept with that save on the line itself, read on hover and by a
  screen reader), and Kind where the page is not plain prose: Draft,
  Onboarding, and a tool skill with who it reached as a skill over thirty
  days ("loaded as a skill by SWE", "offered" when no seat asked for the body,
  "not loaded in 30 days" when neither happened). A kind is words and never a
  state's colour. Every name here (the author, the last saver, a commenter,
  a revision's author, a change's) is drawn as a task's reporter is: a handle
  the roster holds is that seat, which is how a person appears, since they
  write as the seat they hold, and anything else is the login of a
  credential bound to no seat.
- **The body** is the design system's `Document` over the dashboard's
  renderer, with anchors on its headings, and a first heading that repeats the
  title is not drawn twice. A FOLDER says it groups pages and lists what is
  filed under it.
- **Comments**, then **Connections**: three cards, *Linked from* (tasks by key
  with their status, one whose key another task claimed first linking by its
  id, then pages by title), *Read by agents* (over the last 30
  days: each seat with its read count, then how it reached the page and when,
  linking the turn it happened in, the way-read cut with an ellipsis so the
  age never wraps onto a third line) and *Under this page*, three rows each and
  "Show n more". A signal this node cannot answer is not drawn rather than
  drawn empty: no readers on a node without the usage domain, no *Linked from*
  on one without an index. A node that holds an index and cannot answer says
  why instead of "No page or task links here" (`linked_from_status`
  `building` or `unavailable`), and the capped-reads note is worded
  company-wide ("Some reads may be missing."), because `elided` counts what
  the cap dropped on every page rather than this one.
- **History** swaps the outline for the revisions and the comments for the
  page's changes, each drawn as the person, the change and the turn that made
  it. It reads one revision in the page's place, as what it changed against
  the one saved before it (by line) or as its full text.
- **Edit writes as you, in place.** The page itself becomes the editor (the
  design system's `RichEditor`, [ADR-0044](../../adr/0044-a-page-is-edited-in-place-and-saved-as-the-markdown-it-was.md)): it looks the same while it is
  edited as when it is read, blocks come from a slash menu or from Markdown
  typed as Markdown, a selection gets a formatting toolbar, and the page bar's
  Edit becomes the editing state, Cancel and Save. The body stays Markdown,
  and a save with no edit sends the bytes that were opened, so only the lines
  a writer changed ever move. It saves through `save_page` on
  [`/operator/act`](api-endpoints.md#operatoract--the-dashboards-write-surface)
  with the revision it was opened on as `base_version`. The page keeps being
  read while it is open (every 20 s), so a save by somebody else is SEEN
  before the person presses Save: the editor says "Somebody saved revision n
  while you edited", offers their change as a diff, and "Keep editing on top of
  it" MERGES their change into the draft by line before the base moves
  (`lib/merge.ts`, a three-way merge over the revision the edit started from).
  A change only one side made is taken as it is; where both changed the same
  lines the base stays behind and each clash is shown, their lines beside the
  draft's, for the person to keep theirs, yours or both, with nothing
  pre-chosen and the draft held until they apply. Moving the base without the
  merge is precisely what the engine's refusal of a stale base exists to
  prevent: the next save would be accepted against the new revision and
  delete what they wrote. Save and Command or Control with Enter read one
  answer for whether a save can be sent. A page link is inserted from the
  slash menu's "Link to a page", by picking a page, and is written in the
  address the backlinks read (`PAGE_ADDRESS_PREFIX`, held against
  `doclet.AddressPrefix` by a test). The note kept with the revision is a line
  under the page, beside its word count.

### The composer: a new page

`#/doclet/new` writes a new page, with where it is filed in the address
(`?space=…&parent=…`): **New page** in the bar, a space's Create row and a
page's **New sub-page** each open it filed where they were pressed. The page is
written at the measure it is read at (`--size-measure`, the page's own reading
column), with Similar pages and Labels beside it, so a draft's lines are the
lines its readers will see.

- **File it in** names the space and the page it goes under (or the space's
  top), and the writer can change either. **Start from** offers a blank page
  or a template (a design for a work item, a runbook, a decision record); a
  template takes only an empty page, or one still holding the template it
  started from, so it never replaces writing.
- **The title is the page's address in its space**: a title already taken
  there is said under the field as it settles, beside a way to open the page
  that has it, and holds both presses. Its size, the body's (512 KB) and the
  labels' (32, each 64 bytes) are held at the engine's own caps
  (`PAGE_TITLE_MAX_BYTES`, `PAGE_BODY_MAX_BYTES`, `PAGE_LABELS_MAX`,
  `PAGE_LABEL_MAX_BYTES`, held against `internal/doclet` by a test) and said,
  never cut.
- **Similar pages** is the knowledge search on the title, asked once the title
  rests, so the page that already answers this is found before a second one is
  written. **Labels** are typed in place.
- **Save as draft** and **Publish** are one `write_page` with `status`: a
  draft is listed in its space and marked Draft, and found by no search until
  it is published. The writer lands on the new page by the id the engine
  answered with.
- **Discard** asks first when anything is written, and so does any move away
  (`useLeaveGuard`), so nothing typed is lost to a stray press.

A reader without a write credential sees every button with the reason it is
unavailable, never a button that fails.

### Agent skills

`#/doclet/skills` has two kinds behind one address. **Tool skills**
(`kind=pages`, the default) lists every published tool-skill page with who it
reached over thirty company days — the `pages` listing's `skill_loaded_by`,
read with the listing rather than per row — and the total the listing counts,
with Load more past a window. Their order is the ENGINE's, chosen in the
toolbar's **Order** and kept in `sort=`: **Name** (`sort=title`, the
knowledge base's own order) or **Newest** (`sort=-updated`, the listing's
`order: "newest"`, by when each skill last changed, read from the skill rows'
own index). The column heads are not sortable: a head would order the rows
one window held, so past a window "Updated" put the newest of the
alphabetically first skills at the top. Home's **Agent skills** widget asks
for the newest the same way, a widget's window of the engine's order.
**Learned** (`kind=learned&seat=`) is one agent
seat's synthesized skills from `agent_memory`, answered by the node holding
the seat, with "n of total" when the page is cut. Their order is the HOLDER's
for the same reason, chosen in the same toolbar **Order** and kept in `sort=`:
**Most used** (`sort=-uses`, the default) or **Recently changed**
(`sort=-updated`), asked as the question's `skills_order` (`uses`, `updated`)
and applied to the seat's whole set before the page is cut. The heads are not
sortable: sorted here, "most used" was the most used of the newest fifty.

### Agent diaries

`#/doclet/diaries` lists EVERY agent seat in the chart (the landing it
replaced stopped at a dozen), from `memory_overview`: each agent's newest note,
when it was written, and its entry, episode and skill TOTALS, counted by the
node holding the agent, which the row names. An agent no node holds says so
and shows no counts, since no copy of its memory is current; an agent whose
holder did not answer says that instead of drawing zeros, and the partial
callout every fleet answer draws names the node and why. The landing's **Agent
diaries** row counts the agents listed. `#/doclet/diaries/{handle}` is one
agent's diary and episodes — the same two cards its profile's Memory tab
draws, from the same holder-answered `agent_memory` — with the node that
answered, or the reason nothing is shown, and the way to its whole memory.

## Spend: tokens by phase, agent and model

Spend is Settings' **Spend** group: what the engine's runs spent, read by
every reader, while the ceilings the company sets on it are
[Company › Budgets](#company--budgets-raised-in-place). Settings lists the readings and
never the ceilings, and Budgets is not a second row here.

`#/settings/spend` is one window of **whole company days**, 7, 30 or 90 (it opens on
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

**Three rows, each one height.** The hero beside the daily chart; **By agent**
beside **By model** and **By team**, stacked; then the **most expensive tasks**
beside the **auxiliary model**. The two rows under the hero share one pair of
tracks (1.7 : 1), so the gutter runs down the page as one line: the wide track
holds what is read along a row, the seats' table and the tasks' titles, and the
narrow one the ranked lists, which a wide track only stretches. The three
ranked lists give their words one column, half the card, so their bars start at
one x down the page and a team's name is not cut at the kit's 128px. Every card
in a row stretches to the tallest, so the row's edges line up and nothing
leaves a hole under it. A stretched card keeps its content at the top and its
foot on the floor, the stacked lists' last card takes what the stack leaves,
and a card whose partner is absent (no task spend to rank on a tracker that is
not Worklet) takes the whole row. In the top row the hero's figure stays
at its top while the budget and the tiles stand on its floor, level with the
chart's split control; a tile with nothing to say is not drawn, and the other
takes the tiles' whole row. Where the hero is the taller card, the chart's plot
takes the spare height (the kit's `height="fill"`, never under 12rem), so the
split control stands right under the dates rather than below a band of nothing.
Below a page column of 900px every row stacks, the table first; the width is
the page column's, not the window's.

- **The hero** is the window's tokens, the change against the window before it
  (the engine's `previous=true`, cut on the company's calendar, and "nothing
  was spent" rather than a percentage when the window before spent nothing),
  and where the history's floor is (`horizon.floor`).
- **This month** is the budget gate's monthly window from the `budget` push —
  "used of limit · resets Oct 1" on a kit `Meter` handed the engine's `state`,
  never a fraction judged here, with the reset on the company's calendar. With
  no monthly ceiling it says so, and offers **Set one** (to `#/company/budgets`)
  only to an admin — and only once a report has SAID so: before the engine's
  first report the push is `null` and the tile waits, because "nobody has read
  the counter" is not "there is no ceiling" (Home's weekly line reads the same
  slice the same way).
- **The prompt cache's share** is `cache_read_tokens / input_tokens` (the input
  already includes the cached prefix). A window in which nothing reported a
  cache has **no cache tile** — not 0%, which would be a claim about caching
  nobody made.
- **The median task** is Worklet's own `totals=spend_tokens:median` over
  the tasks FINISHED in the window that spent anything, with how many. On a
  company whose tracker is not Worklet, `work_items` is not served and the
  tile is not drawn.
- **A reader without Worklet is asked nothing about tasks.** The overview is
  everybody's and a task is Worklet's, so the median tile and the most
  expensive tasks card each say *Needs Worklet access* (the card with no
  **All** link, since the list is Worklet's too), and no `work_items` is
  sent: asked, the engine's refusal drew a callout offering to sign in to
  somebody already signed in.
- **Daily tokens by phase** is the kit's `StackedColumns` over the engine's four
  bands, each in its `BANDS` hue, and the legend is always those four — the
  engine answers a band nothing spent in at zero; the split control offers the
  contract's six `GROUPS`, and any split but phase draws the engine's top four
  and a residual. A seat band is labelled with the seat's name, as the table
  under it names it, and a team band with the unit's. The comparison is the
  hero's; the chart draws no ghost of the window before. The legend stands at
  the end of the title's row, and on a card too narrow for both (a phone) it
  takes a row of its own under the title, still at the end.
- **By agent** — turns, tokens, share, **budget today** (the seat's daily window
  from its overlay, `exhausted` where the engine says `refusing`) and tokens per
  ended turn. A PERSON the auxiliary model spent for (`person: true` — their
  questions answered with `answer_knowledge`, and the background passes of a
  unit they lead) is a row of their own, keyed by their handle, whose turns and
  per-turn cells say it is auxiliary model spend for that person. A row opens the seat
  in the peek; on a phone a seat is one compact row of two lines, at every
  density: name, tokens and share, then turns · today · per turn, each with its
  unit, and today as its percentage alone (the day's bar is for scanning the
  column, which a compact row has none of). **Recent turns by tokens** goes to `#/live/turns?sort=-tokens`
  over the same window where the turn list has it (a quarter goes to its thirty
  days, and a caption beside the link says so): a company day holds no turn, so
  per-turn spend is the turn list's.
- **By model** is the engine's `by_provider`: each configured provider entry,
  the models it answered with, who used it (the top three and how many more)
  and its tokens. **By team** is `token_series{group: unit}` asked for every
  team; **Auxiliary model** is `by_worker` — what the seats' cheap model spent,
  by purpose (`memory filter`, `condense thread`, `persist decider`, …) and its
  provider calls. A breakdown rather than an addition: every one of those calls
  is already in the row of the seat or person it was made for, under the
  Auxiliary band. It used to be headed "Background workers" and to claim the
  embedding pass, which is metered nowhere.
- **An empty card says so one way**, the kit's compact empty state, whether it
  holds a ranked list, a table or the chart, so no card answers "nothing in
  this window" as a bare sentence beside another's empty state.
- **Export** is a client CSV of the rollup and the by-agent table, every row
  labelled with its window and section, tokens only, and every text cell
  guarded so a spreadsheet opens a name as text rather than a formula.

`#/settings/spend/tasks`, **Expensive tasks** (the Spend group's second row,
not offered where the company does not run Worklet), is the tasks last changed inside the
window, bounded at both edges (`closed_since` plus `updated=range:<since>..<until>`
from the spend answer's own instants, so a window that ended last month lists
nothing that moved only this week), most tokens first
(`sort=-spend_tokens`, the engine's order), each with what drove it from
`fields=spend`: its turns, and its reopens, workers and send-backs where they
are not zero, and a bar beside the figure scaled to the top task (not a
percentage: a lifetime's tokens are a share of no window's total). A task's
tokens are its whole life's (Worklet charges a task, not a day), and the
list says so. The overview shows the top three. A
window the engine refused has no list: the page says the refusal and nothing
under it waits for an answer that will never be asked. On a phone each task is
the overview card's compact row — key and title with what drove it under, the
tokens at the end — rather than a labelled card per task.

## Company › Budgets, raised in place

`#/company/budgets`, the last row under Company (what the company decides,
where what it spent is Settings › Token spend), is every scope's three calendar windows on the company clock
— the day, the ISO week and the month — from the `budgets` answer: what each
has spent, its ceiling or **No ceiling**, when it resets and, while the gate is
refusing it, since when. **The company** comes first, as one row, because
everything a seat spends is the company's spend too and its ceiling binds every
seat at once; **Agent seats** follow as one table with a column per window, so
the one seat near its ceiling is found down a column. The company's row stands
on the table's own columns (one track list handed to both), so each window
sits over its column, and its first track says once what the windows are:
every seat together, on the company clock, and when each turns over.
**Ask Crewlet, per person** follows the company, on the same tracks: the ceiling every
person's Ask Crewlet questions are held to (`assistant.token_budget`),
each person on their own. Its figures are the reader's own spend against it,
drawn only where the engine counts one (`counted`: a person, under a company
that caps a person's spend); elsewhere the row draws the ceilings and no figure,
since a zero nobody measured is not "0 tokens". A window's bar is a kit
`Meter` handed the engine's `state`, and the page divides nothing itself; a
window with no ceiling draws no bar, because a bar needs something to be a
fraction of. A
counter nobody could read (`durable: false`) says so and draws no figures.

**Every ceiling is editable in place by an admin.** The pencil beside it
turns the figure into a field that takes the spelling the screen draws — `40M`,
`2.5M`, `750k`, or the digits — and **empty for no ceiling**; a 0 is refused
before anything is sent, in the engine's terms (empty is the only "none"). A
save is **checked first**, against the revision it was read from, and only the
warnings the change *introduces* are shown — a seat ceiling at or above the
company's, which can never refuse a turn, is the one this screen exists to
catch — with **Save anyway** to store it regardless. The company's ceilings are
written as a merge patch of the windows that changed (`null` removes one), and
each person's Ask Crewlet ceiling as the same patch one level down
(`{assistant: {token_budget: {...}}}`), which leaves the rest of the
`assistant:` block as it was; a
seat's by replacing that seat (`PUT /config/roles/{handle}`, its check the same
request with `dry_run=true`), because a patch cannot address one seat in a
roster. The revision's summary says whose ceiling, which window, and from what
to what. After a save the figure shows the new ceiling marked **applying…**
until this node has applied the saved revision, and then re-reads — judged by
where this node stands on that revision (`lib/nodeStanding.ts`), never by its
applied epoch alone, which a refusal leaves where it was. A node that refused
it puts the figure back to the ceiling the gate still enforces, marked
**refused by this node**, until the pencil is pressed again or a later
revision it applies settles it; a save a later activation passed before this
node applied it settles on the re-read like an apply. The mark follows this
node's LAST health push (`lib/useLastHealth.ts`): a dropped socket clears the
store's health slice, and is no news of the save, so a refusal stays said
through it rather than turning back into the saved figure, applying. A conflict saves
nothing and offers **Reload**; so does a request that may have landed. The
pencils are drawn for every reader and disabled, with the reason said once
above the page in the standard notice, for a reader without Admin — except a seat's, for
the lead of the unit it sits in, whom the engine admits to that seat's write
(`useOrgWriteAccess`, `lib/leadScope.ts`).

**There is no reset.** A window's `used` is what it spent; the room comes back
when it turns over, or now by raising its ceiling. The Inbox's and a stopped
seat's **Raise budget** open the same write as a dialog of one scope's three
ceilings (`RaiseBudgetDialog`), each field captioned with what that window has
spent and when it resets. A reader who may not raise that scope's ceiling is
not handed the button: the row says who can instead ("An admin can raise this
ceiling.", the units a lead leads, or who manages the document).

**Each ceiling is a section a refusal opens.** A refusal or a notice a token
budget caused names the setting behind it (ADR-0060's remedy), and
`lib/remedy.ts` is the one table from a setting to this screen and its
section: `section=company`, `section=assistant`, or `section=seat:<handle>`
for a seat's row. The section is brought into view, focused, and marked as the
one the reader came for (the card's `selected`, the seat's row as current); a
press on the address already open arrives again; and a section the screen does
not draw for the reader (a seat it no longer lists) reveals nothing. The press
is offered by the gate the editor there is drawn by (`useOrgWriteAccess`), so
the link and the pencils it opens cannot disagree: a reader the gate refuses
for who they are is told who can raise it, and never handed a link to a screen
whose controls are all locked for them. It is drawn on Ask Crewlet's refusal
of a question and on a turn a budget stopped, on a refused act (the palette's
knowledge answer among them), on the Inbox's company budget condition, and on
Home's spent week.

## Account: the reader's own page

`#/account` is the signed-in reader's own page, opened from **the menu their
own name opens** in the user block at the sidebar's foot, from the **Your account** card on their own seat's
page and on its peek, from the palette or with `g u`. It has **no sidebar row**: the user
block already is the reader, and a row beside it would be the same person
twice. **Every way a person looks for their own security leads here**, because
the page was reached only through a text link inside the preferences popover:
the name opened the seat, the seat said nothing of a password, and Settings
was all locks to a reader holding nothing but their own account — so a company's people
reported that the dashboard had no two-step verification at all. **Every read and write on it names the caller and
nobody else** — `GET /auth/session` for who this browser is, the self arm of
`/iam/people/{id}` and its sessions, `GET /iam/credentials` and a token's mint
and revocation with no `?person=` — so the page holds no id it could be talked
into swapping for somebody else's.

- **Profile** — login, name, address, seat and **Access** (the
  People list's own words: Admin, the apps they hold, or Overview only),
  read-only, at the card's own inset, with the line "Only an admin can change
  these." (an admin is linked to their own row in People instead): a
  person changing their own would be the escalation the directory exists to
  close. A sealed value reads *Locked*, never blank. The seat is written as
  every seat is — its badge and name, then its `@handle` — and as the handle
  alone where the chart cannot be read (it read *cara cara* once); it links
  to the seat's page for every reader but one the engine refused the company
  (a seat taken out of the chart), for whom that page is the panel refusing
  them.
- **Security** — a line each, with its gesture beside it, in the order a
  person comes to check them: **Two-step verification** first, then — beside
  an app — **Recovery codes**, then **Password**, whose **Change password…**
  opens the form in a dialog, as the authenticator's setup does. The form
  drawn in place stood four fields tall above the second factor and pushed
  it below the fold at 1280×720 and on a phone. **Change password** takes
  the current one, a **Code** — for
  somebody holding a second factor, an authenticator app or recovery codes:
  the app's current code or a recovery code, as at the sign-in — and the new
  one typed twice against the floor `/auth/config` names. The password and
  the code are the proof, a step-up's taken in the one request, so no step-up
  is asked. The Code field is drawn when the credential list says a factor is
  held, and also once the engine answers `second_factor_required` for a list
  that was behind; somebody holding none is never shown it. The change ends
  every other session and every personal access token the person holds and
  keeps this browser signed in, which the page says; a `202` (this node has
  not applied it yet) ended this browser's session too and cleared its
  cookie, so the tab goes straight to the sign-in with a toast saying the
  password changed and to sign in with the new one — the page reads nothing
  more, since every read would be refused. A wrong current password or code
  is the sign-in surface's one refusal, about what was typed, and never sends
  the person to sign in: the engine names a wrong current password and words
  it itself, and a refusal naming no field after a code was sent reads as the
  step-up dialog reads it ("That password or code was not accepted"), the
  code cleared. **Two-step verification** sets up or
  replaces the authenticator app — the seed drawn as `#/enrol` draws it, a QR
  code with the key and the link beside it — a first one issuing a first set of recovery
  codes, shown once, as `#/enrol` does, and a replacement keeping the set held
  — and **Create new codes** issues a set —
  offered only beside an app, because recovery codes held alone are a second
  factor of their own, which the engine refuses to issue.
- **Where you are signed in** — the live sessions, with **This browser**
  marked by the lineage `/auth/session` names; any other is signed out by name
  (`POST /auth/logout/{lineage}`), and **Sign out everywhere** asks first —
  here and in the user menu, which closes to ask — saying it ends every
  personal access token too, which it does. It stands in the card's body
  beside the sentence saying so, not in its header, where on a phone it cut
  the title to *Where you are sign…*. Every gesture on the page
  reads `/auth/session` again — the frame's read, through its reload — before
  the lists, because one the engine asked a
  step-up for replaced this browser's session under a new lineage, and a mark
  read before it would offer this browser a named sign-out.
- **Personal access tokens** — the tokens that act as the reader, each with
  what it carries and when it expires, **Revoke** on a live one, and **New
  token**: a label, a lifetime (empty takes the engine's default) and grants
  out of the reader's own, the value shown once with the sentence that it acts
  as them. It is the same dialog Settings › Service accounts mints a service
  account's token with (`components/people.tsx`), minting with no `?person=`.

A session exchanged from a Tier A token is the deployment's credential, not a
person: the page names it (*signed in with this deployment's API token
`ops`*) with its grants, and draws none of the sections above, each of which
the engine would refuse. The page's note is chosen by what the session is:
the person's promises a password, a second factor, sessions and tokens, and
a token's says it has none of them and that its session ends within the
hour — the person's, drawn for every session, stood directly above the card
saying the opposite.

## Settings: the frame, people, secrets, nodes and configuration

Settings is what the ENGINE runs, and nothing the company decides. It is the
one workspace that draws its sections as a **column** beside the screen rather
than as tabs in the page bar, in four groups: **Runtime** (Models, MCP
Tools, Secrets and Limits: what a turn runs on and the caps it is held to),
**Spend** (Token spend and Expensive tasks: the readings, never the
ceilings; the row is not called Spend, since without Worklet it is the
group's only row and read "Spend › Spend"),
**Access** (People, Service accounts, API tokens, Sign-in policy and Audit
log) and **Engine** (Nodes, Configuration, Backups & retention and File
storage). **It lists only what lives in it.** A place that moved to Company
(the charter, the policies, Integrations and Budgets) is found there, by the
sidebar and the palette, and never again as a second row here leading out:
Budgets lives once, under Company, and Settings draws no row for it. The
reader's Account is the user block's, and how this browser looks is the
preferences popover's, so the column has no group of the reader's own and no
row that leaves Settings. Every row's title is its hint, the words the palette
shows under it. **The column stands beside the section only where the window
holds both** (`SECTION_COLUMN_MIN`, 1212px: the sidebar, the sheet, the column
and a 720px section floor, in `app/layout.ts`); below that it folds to the
one-row picker the phone draws, naming the section the reader is on, and the
section takes the screen's width. At 1024 the column beside it left a 489px
section, whose tables drew their columns 46px wide. A section closed to the reader draws a lock naming what it
lacks and **is never hidden**: a section that vanished for a reader without
its access is one they cannot know exists. Closed is one rule, `sectionOpen`:
Token spend's readings and Models are open to everybody (each model's
condition is what tells a member why a seat waits; every action on it stays
an admin's), Expensive tasks is Worklet's, and every other Settings section is
admin's. It is the same rule
the sidebar locks a row by (a row only when nothing in its workspace is open,
so Settings never, since Spend always is) and the frame draws a section's
refusal by. A section that reads a service the company does not run is not
drawn at all, by the sidebar's rule: Expensive tasks, where the company does
not run Worklet. The landing page, Models (`#/settings/models`, where
the sidebar's Settings row goes), is one of the column's sections, so its
trail reads **Settings › Models** rather than "Settings" alone. **On a
phone the column folds to one row naming the section** the reader is on,
"Section · Models", the word said and not only spoken, since a
section's name alone read as the page's title rather than as a control
holding the others. It opens the list and folds again once a section is
picked, the one the reader is already on included: stacked whole above the
section it took about 520px, and a section's own content began below the
fold. So a LINK to a section arrives folded, the section it named starting
right under the picker. But the Settings row's own address names no section
anybody chose, so **arriving there opens the list**, every group in it:
folded, a phone reader saw one row naming the first section and never learned
what else Settings holds. A pick that lands on that address (Models)
folds it like any other. **Beside a
section the column stays put**: it is the scroller's height rather than the
section's, so it sticks while the section scrolls, its hairline runs the height
of the window, and a list longer than a short window scrolls inside itself. On a
phone the folded picker is its own height and the section takes the rest, so a
short section starts right under it. An address that names no screen is in no
section, so a mistyped one is never drawn beside a column claiming a section.

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
| Nodes | the nodes holding a presence lease ("1 node live"); a warning dot and "*n* behind on config" in the row's name when a node does not run the revision the fleet activated — an older epoch, a refusal or a partial apply of the activated one, another revision, or no apply reported (the Nodes screen's own "Behind on config" count, by one rule: `lib/configStanding.ts`) | the health push, and `fleet` |
| Configuration | "epoch *n*" — the epoch this node applied, in words rather than a code | the health push |
| Backups & retention | how many alarms stand, the count the engine panel shows, drawn only when one does | `retention` |

`fleet` and `retention` are admin's answers with no push behind them, so those
figures are a poll, and **they are asked only while the Settings column is on
screen, and only of a reader the section opens for** (`useSettingsSidebar`,
at 30 s and 60 s), by the rule the column locks it by (`sectionOpen`),
so no figure is asked for beside a lock. The frame outlives every screen, so a figure hook
that asked unconditionally would put two operator questions on a timer
behind every screen of every tab, and two refusals behind every reader who
is not an admin. On Nodes the column asks no `fleet` of its own: the screen
hands the frame the reading it already polls every 15 s (`usePublishFleet`),
so "*n* behind on config" and the screen's "Behind on config" tile are one
reading rather than two polls on two clocks that disagreed for up to 30 s.

**The charter is Company's, not Settings'.** **Company › Principles**
(`#/company/principles`, the sidebar's **Principles** row and the Company
landing at `#/company`, once Settings › General, then Company › Overview and
Company › Policies) is what every agent is told about the company, in the
order an agent reads it. It is read from the org projection, so every
signed-in reader can open it: it is the overview's, like the org chart. Under
its heading (the kit's `ObjectHeader`: *Who the company is and the rules every
agent works by.*) it is three kit `Card`s, each a region named by its title
and headed by its own glyph:

- **Mission** (a target) and **Vision** (a telescope) are two equal cards
  side by side, since neither outranks the other, each holding its statement
  as the kit's `passage` text. The mission is one line by the engine's rule
  and wraps as prose; the vision may hold paragraphs, and keeps them as
  written. The
  pair shares its rows on a subgrid, so the two heads are one height whatever
  controls each carries and the two bodies one height however long either
  runs. Below 720px of the page's own width they stack.
- **Standing policies** (a scroll of text) is one card across the page under
  them, its count in the head once there is one and its rules numbered (a
  reader cites "policy 2") in the kit's compact divided rows, each rule one
  line by the engine's rule, wrapping as prose at the kit's wide reading
  measure while the rows and their rules span the card.
- **An unwritten part says so** inside its card, as the kit's inline empty
  state saying what the part is for, with its own way to write it (**Add a
  mission**, **Add a vision**, **Add a policy**) for a reader who may write the
  charter. A written part carries its own **Edit** in its card's head.
- **A reader who may not write the charter** reads the cards with no control
  on the page at all: no Edit, no Add, and no disabled copy of either (see
  [A write control is never hidden](#a-write-control-is-never-hidden)).
- **Engine limits** follows the cards as a related row, for a reader who may
  open Settings › Limits.

**Writing the charter happens on the page, one part at a time.** Edit (or an
unwritten part's Add) opens that part in place, in its own card, as the kit's
`InlineEditor`: the field, then a bar with the editing status, Cancel and
Save. The other cards stay as they read, so the person writes against them,
and several parts may be open at once. Each save is one configuration
revision whose summary names the part ("Changed the mission", "Standing
policies: added 1, reordered"), so a page mode that saved three unrelated
edits as one revision was declined. The source is
`dashboard/src/routes/settings/principles/edit/`.

- **Who may write it.** Admin, on a company document this deployment lets the
  reader's credential change (the viewer's `config_writer`, ADR-0030):
  `useMayWriteCharter`, which the org builder's company node asks too. Anybody
  else is offered no control at all. An admin on a document another system
  manages is told so above the cards instead of finding no Edit. Offline or
  while the browser is still being identified, the controls stay and are
  held with the reason.
- **The fields.** The mission is one line, and so is each policy, because each
  is inlined in every seat's prompt (a policy as one item of a list); one line
  still wraps, so a long one is read whole while it is written (the kit's
  `singleLine` textarea, which takes no line break and folds a pasted one into
  a space). The vision is paragraphs. Each statement and each policy shows its count as it
  nears its bound, counted in characters as the engine counts them (1024 for
  the mission and for a policy, 2048 for the vision, 25 policies, from
  `contract/config.ts`). The policies are the kit's `ListInput`: edited where
  they stand, added with Enter, moved by a grip, by `Alt` with an arrow or by
  their buttons, and removed with an Undo where the policy was. A policy typed
  in the new box and not yet added counts as a change and is saved with the
  rest; a blank row is dropped. A repeated policy is said under its later
  copy, and Save waits while any problem stands. Only what the person changed
  is held to the rules, as the engine holds a write: a policy or a statement
  stored before the rules and left as it was is said as a warning above the
  bar (*Saved before the current rules*) and never holds Save.
- **Saving.** The editor starts from the engine's own charter
  (`GET /config/charter`), not the page's copy, which can lag it. Save reads
  the charter again and writes it with only this part replaced, conditional
  on its tag (`PUT /config/charter`, `If-Match`), so a colleague's change to
  another part is kept. The card then shows the saved words until this
  browser's chart carries them, with *Saved. This page shows it as soon as
  this node applies the new revision.* under them, and a toast says it saved.
  The save's answer names the revision it stored beside its epoch, and the
  card follows this node's standing on that revision (`lib/nodeStanding.ts`):
  once this node refused it, the card draws the part as this node still runs
  it, with *Saved, but this node refused the new revision and goes on running
  the one before. Settings › Nodes says why.*, until a later revision settles
  it; once it applied the revision, or passed it for a later one, the card
  draws the chart. It reads this node's last push (`lib/useLastHealth.ts`), so
  a dropped socket leaves a refusal said rather than drawing the saved words
  again. A refusal keeps the draft and says
  why once, beside the Save that caused it (the editor's own alert, so no toast
  reads it a second time); a save that got no answer is settled by reading the
  charter back rather than sent twice.
- **A colleague's change to the same part** is never written over unseen. The
  card says the part changed while the person was editing, shows what changed
  line by line (the kit's `ConflictNote` and `DiffView`), keeps the draft, and
  offers **Keep my draft** (save it over theirs knowingly) or **Use theirs**.
- **Unsaved writing is never lost to a stray key.** Cancel or `Esc` on a part
  with changes asks first (*Discard your changes?*), and so does a move to
  another page (*Leave without saving?*) and a reload or a closed tab. Focus
  goes into the field as a part opens, and back to its Edit (or Add) when it
  closes.

The company's name and clock are not principles and are not on the page: the
name is the shell's, and the clock is the company document's.
The caps the engine holds a turn to are not policies the company writes for
its agents to read: they are **Settings › Limits** (`#/settings/limits`), in
the Runtime group, the engine's own answer for each (the turn's guardrails,
delegation, notifications and a scheduled turn's timeout). Each group is a
card holding the kit's `SettingsTable`, the shape every read-only settings
page takes (Settings › Sign-in policy too): the cap's name with its key in
the company document under it, the value in tabular figures, and its source,
*Default* as a quiet word where the value is the engine's and a marked
*Changed from* the default otherwise, *Off* rather than 0 where a value
switches its limit off (the answer's `off` and `default_off`). Limits is
read-only and admin's, and Principles offers it as a related row to a reader
who may open it.

**The brand is Company's too, and published.** **Company › Brand**
(`#/company/brand`) is how the company looks and sounds in everything its
agents make. A brand KIT is an artifact of kind `brand` (its `brand.json` and
the logo and font files it names), versioned like any artifact, which agents
and editors may draft; the company's brand is the one version an
administrator PUBLISHED (`brand{}`; an adoption in the engine's word, which
`POST /brand/adoptions` and `artifacts.brand.adopt` keep, while every label a
person reads says publish), which the engine snapshots and serves by its
bytes. The page draws that adoption as four cards in the Principles card
grammar, Logos (`image`: the name and tagline over three `LogoSlot`s, primary
on a light ground, one for dark grounds and the square mark, each with a
download), Colours (`blend`: each set role as a `ColorSwatch` with its value
to copy, and a `ContrastBadge` per pair the adoption measures), Typography
(`type`: the heading and body faces as `TypeSpecimen`s, drawn in themselves)
and Voice and tone (`text-quote`), beside a sticky preview (`BrandSample`, a
doc or a slide, light or dark) in an `AsideLayout` that stacks it under the
cards on a narrow page. An administrator, a person, edits one card at a time
in place, the preview drawing every change; Save opens one dialog listing
what changes (each colour with both values, each logo by its bytes), the
contrast pairs that fall short, and what publishing makes public (**Save
and publish**), then publishes a version of the company's kit and adopts it,
the engine asking for a recent sign-in when the proof is stale. Text on the
light ground below 4.5:1 holds Save with the engine's own reason; every other
pair warns. With no brand published, an administrator is offered **Set up
your brand**, which opens the setup on a page of its own
(`#/company/brand/setup`, six steps, each Continue saving a draft), and
everyone else reads "No brand yet". A draft nobody published yet (one a setup
left, or a version an agent or an editor published on the published kit) is
offered under the header in a notice that says it is not live ("This draft
of the Acme brand is not published yet, so artifacts still use Crewlet's
look.", or "Version 9 of the Acme brand is a draft. Artifacts keep using the
published version 7 until this one is published."), who last changed it and
when (its version's author and day), and ONE primary action chosen by its
readiness: **Publish brand** for an administrator once it is ready (the
review of that version, then "Publish version 9 as the Acme brand?"), with
**Edit draft** beside it; **Finish draft** while it lacks a primary logo or
colour, beside the sentence saying so ("Not ready to publish: add a primary
logo and colour"), never offered for publishing; an editor who may not
publish reads that an administrator can review and publish it. With none
published, the draft itself is drawn under the notice in the four cards,
read-only, its preview tagged **Draft**, in place of "No brand yet". Its
owner or an administrator, a person, may **Discard draft**, which confirms
and moves the kit to the trash (`DELETE /artifacts/{id}`, `artifacts.trash`),
restorable by its owner until it is purged; a newer version of the published
kit is a version, not a kit, and is never offered for discarding. The engine
publishes a draft that lacks a primary logo or colour as a version and
refuses only to adopt it. **Run setup again** in the bar walks every part
again, prefilled from the kit where it stands. The bar holds
the kit's **History** and **Stop using this brand**. The brand meets people
where they make things: the Make row's `BrandChip` (or **Set up your brand**
for an administrator), the company's theme first and chosen in a new deck's
picker, a note under Ask an agent, and a **Brand** fact in an artifact's
Details (current, an older brand that changes at the next publish, or
Crewlet's look; a deck, which copies the brand when it is made and never
follows it, says "Made with the Acme brand of" the day it was made). A kit's
own page draws the same cards and preview for the
version on show, says whether it is the published one, and offers an
administrator **Publish this version**.

**Access** is the identity directory, over the routes `/iam` serves to an
admin, split over three pages so that no row is drawn twice: People, Service
accounts and API tokens. **People** (`#/settings/people`)
is every principal in the directory but its service accounts (`/iam/people`), their login, stage,
**Access** column and the seat the directory binds them to — the Access column
saying the set in the one line the dashboard says it everywhere (`accessLine`
in `lib/access.ts`): **Admin** for anybody holding it, since every app is
included and naming them beside it would read as separate; otherwise the apps
they hold, **Worklet · Doclet**; **Overview only** for none; and a name this
build does not know appended raw, so it is never silently dropped — and a row opens that
person in the frame's **peek** (`peek=person:{id}`), which reads their row
(`/iam/people/{id}`), their credentials (`/iam/credentials`) and their
sessions for itself: in view beside the list on a wide window and a drawer
on a phone, stepped with `[` and `]`, closed by Back. It was a card drawn
under the stats, the report and the whole grid, so pressing a row opened
something below the fold and the page did not move. **The reader's own row
leads with their Account** — "This is you" and a link to it, where their
password, two-step verification, sessions and personal access tokens are
theirs to manage — and the administrator's gestures follow, each saying what
it would do to them: before, a person who opened their own row was offered a
reset link and "Sign out everywhere" and nothing that pointed at their own
security. A sealed value this node's keyring cannot open reads *Locked*,
never blank. A service account bound
to none reads *No seat*, which is ordinary; a person always holds a
human seat. A stage that is the resting one (*Active*) is quiet words in its
column, and a stage that differs (*Invited*, *Suspended*) is a tag, so the
row that needs a look is the one that stands out. **Service accounts**
(`#/settings/service-accounts`) is the machine principals, drawn as People
draws a person and opened in the same peek, with how many act as a seat in
the card's head. **Human seats**, on People, is every
human seat of the running company with whatever holds it (`/iam/seats`) — a
person or a service account by login and stage, an open invitation as an
**Invited** tag with its address, or *Open seat* — and where agents
reach it: its contact identities, read from the company document, which is
admin's like this screen. The tile above it counts **Open seats**
as the seats held by NOTHING — an open invitation holds its seat as surely as
a person does, and counted as vacant it read as a seat an administrator could
still invite somebody onto — with "*n* invited" beside the total. **API
tokens** (`#/settings/tokens`) is this node's Tier A labels joined to the directory rows holding
their logins (`/iam/node-tokens`): a token whose login nobody holds acts as
itself, unbound, and the row says so in quiet words (*Not bound*, the Seat
column drawn only where a token is bound) and names the one command that binds it
— `crewlet iam create -kind machine -login token:<id> -seat <seat> -grants none`, a row
created on a human seat nothing else holds — since a label mistyped on either
side otherwise looks bound until the token presses something. When the company
document is [managed](../concepts/configuration.md#managed-configuration), the
card names its writers (each row says whether that token is one,
`company_writer`) and that only those tokens may change the company;
nothing is said when the document is not managed. The directory's
own report (`/iam/check`), the **Needs attention** card (*All good.* when
empty), is drawn on the page of the principal each finding is about, and names a company no active person can administer
(`no_admin_holder`), somebody active with no way in, a binding whose seat is
gone and a row holding admin that this node refuses (`admin_clamped_by_node`,
on a node whose `api.auth.admin` is `refuse`), in the engine's order, each kind in
words keyed on the engine's own list (`FINDING_KINDS` in
`contract/identity.ts`, held against `iamapi.FindingKinds`), so a kind the
engine grows is a compile error rather than a raw code on the screen. Each
finding is written here as a sentence about the person it names ("Deploy
pipeline has no token, so it can’t sign in.") rather than the engine's detail,
which names an API path; a principal with no way in gets its fix beside it,
**Create token** for a service account and **Get reset link** for a person.
**Invitations**, on People, is every invitation nobody has redeemed and that is still good
(`/iam/invitations`) — address, seat, access, who sent it, when it expires —
and **Show used and expired** asks for every one the estate still holds.
**Sign-in policy** (`#/settings/sign-in`) is how a person signs in, as the
node that answered enforces it (`sign_in_policy`, an admin's): whether a
second factor is required, the shortest password, how long a session lives
from sign-in and unused, and how long a step-up authorises a sensitive
gesture, each with its default applied where the node's file writes none. It
is Tier A, written in each node's own `crewlet.yaml` (`api.auth`), so the page
names the node rather than presenting the reading as the company's, and it is
read-only: it changes with an edit to that file and a restart.

**An admin writes the directory from here** (see [A write control is never hidden](#a-write-control-is-never-hidden)
for why the directory's writes are drawn for an admin alone). A person holds
exactly one human seat from the moment they exist, so a person still comes
from the vacant seat they will hold: **Invite** and **Add person** are on
that seat's card in the org chart, its peek and its page ([The org chart is
the company running](#the-org-chart-is-the-company-running)). People's
primary action, **Invite person**, picks an open seat (one nothing holds,
`?unheld=true`) and then opens that seat's own invitation, and **Identity
trail** and **Edit org** sit in its bar's overflow menu. **New service
account** is the primary action of Service accounts. What they write:

- **New service account** — a machine: a login in the colon grammar
  (`ci:release`, checked under the field before anything is posted, as an
  edit's login is against its holder's grammar), a name, its access — the
  picker opened with nothing ticked, since a service account is a credential
  somebody scripts and what it needs is the script's business; Admin may be
  ticked, a service account being the one non-person that may hold it — and an
  optional **Seat** — a human seat nothing holds, for an account that should
  act as that seat (a deploy pipeline acting as its owner's seat, say), sent
  in the same record as the account, because created bare and bound after, a
  pipeline meant to act as a seat acted as itself until a second write landed;
  with **No seat** it acts as itself, under its own login. Then **Mint its
  token**: a label, a lifetime and access out of the account's own, opened
  with all of it ticked that the reader holds too — the account exists to act
  with it — Admin included, since a token may carry admin and what keeps it off
  the gestures that need a person present are the authority table's two locks
  rather than the picker; one the reader does not hold is left unticked and
  disabled, since the engine refuses a token carrying what its minter does not
  hold. The value is shown once.
- **Cancel** on an invitation nobody redeemed: its link stops working at once
  and the address and the seat it names are free again — the same dialog the
  seat's own **Cancel invitation** opens.
- In an **opened person's peek** — **Edit** (one `PATCH` carrying
  only what changed since the dialog opened — measured against that row and
  not the directory's minute-by-minute re-read, so a login or a seat left alone
  never puts back what another administrator changed meanwhile — and the
  access as what was ticked and unticked, `add_grants` and `remove_grants`,
  which the engine applies to what the person holds when it decides, so a
  permission left alone stays as another administrator left it whichever
  others are ticked — the picker the invitation's, Admin one switch that ticks
  and locks the apps, and the note under it saying what an unticked app costs; the seat offered among the vacant ones and their own — with
  **No seat** for a service account only: a person's seat is MOVED, never
  cleared, since the engine refuses a person's edit that clears it
  (`seat_required`), and removing them is what frees it; a person recorded
  with no seat starts on **Choose a seat** with a warning saying so, and the
  edit sends a seat only once one is chosen, so an unrelated change is never
  refused for it; a
  permission the person already holds may be unticked whoever edits, since the
  engine checks only what an edit adds, while one neither holds stays
  disabled — and a name this build does not know, kept on the row, is offered
  as a ticked box of its own, which unticking removes and nothing can add),
  **Suspend** / **Reactivate** (which of the two is fixed when the dialog
  opens, so its **Try again** after an unknown answer sends the same stage
  even once the re-read shows it landed; a suspension says the person's seat,
  by name, is withheld and stays theirs, so nobody else is put on it
  meanwhile). Those two stay in view; the rest sit under **More**: for a
  person **Get reset link** (shown once; the outstanding link is listed among
  their credentials, tagged *Not used yet* until it is opened),
  **Reset two-step verification** (when they hold one) and **Sign out everywhere** or,
  for a machine, **Create token**, and last and set apart **Remove**, confirmed by typing their
  login, since it cannot be undone, which closes the peek once it lands and
  says who was removed. A removal says it frees the seat, by name, which then
  stands vacant in the org chart, and that a person comes back by being
  invited onto a seat again — a service account by being created again, since
  nobody redeems an invitation for one. Each credential row has **Revoke**.
  **Suspend**, **Reset two-step verification** and **Sign out everywhere** each say that
  an outstanding password reset link ends with the sessions, **Reactivate**
  that it stays ended, and **Get reset link** that it is issued
  after those — a link sent first is dead by the time it is opened. On the
  reader's OWN row — the person `GET /auth/session` names, by id — every
  gesture that ends their sessions says first that it is them and signs this
  browser out at once, and speaks to them throughout, its title and body
  alike ("You may not act while suspended"), and **Suspend** and **Remove** add, where `GET
  /iam/check` counts nobody else active holding admin (`people_with_admin`),
  that afterwards only this node's API token could administer people. **Edit**
  speaks to them too, and an edit unticking their own Admin says it
  is them, that they stop administering the company once it saves, and the
  same sentence where nobody else would be left — a WARNING and never a block:
  the engine refuses no one taking away a permission, the last administrator's
  own included, and Tier A's token is the way back in.

Every one of these — and a seat's **Invite**, **Add person** and **Cancel
invitation** — is one `/iam` write (`lib/iamWrite.ts`): a `403
step_up_required` opens the step-up dialog and the same request is replayed; a
create — a service account's or a person's — carries a fresh uuid7
`Idempotency-Key`, kept for the retry an unknown answer asks for — **Try
again** sends the key the engine handed back, so it lands once, and a
person's create hands back the same first password link — and a request
changed before it is pressed is a new one under a new key, since a create's
key sent with another body is refused as reused — while a token's mint and a
reset link read no key and a retry issues another; a refusal is the engine's
sentence with the access that would admit; a `202` says the change is
recorded and this node is catching up, and — like a stale refusal — leaves
the dialog only **Done**, because the gesture is over and pressing it again
would send it again; and after every answer — a refusal part way and an
unknown one included, since either may have changed something — the lists the
write touches are read again, every seat listing on the page among them. Where
`/health` says `identity: unclaimed`, a callout says nobody has joined yet and
what comes next — drawn once the invitations and the human seats have both
answered or been refused, so it never names one step and then another: with
an invitation open, that its link is where its person chooses a login and
password — naming the address where there is one invitation — and that a
lost link is cancelled here or on its seat and issued again from the seat; on
a node running no company (the seat listing's `409 no_active_revision`), that
there is no seat yet and the company comes first, with **Create the company**
(`#/company/org/edit`) and `crewlet config import` beside it, while the Human seats
card says the same in a sentence rather than as a failed read; in a company
with no human seat at all, that nobody can join until one is added, with
**Add a human seat** (`#/company/org/edit?add=human`); otherwise **Open the org
chart**, where you choose your human seat and invite or create yourself on it
— and where the listing was refused for any other reason, the callout points
at the chart without claiming anything about its seats. A token is still
declared in Tier A (`api.auth.tokens`) and changes at a restart. **It never
reads a value back** — not a token's, not a password's, not a second factor's.

**Company › Integrations** (`#/company/integrations`) is **every reader's**, and
lives under Company because which services the company runs and which apps it
connects are the company's decisions, not the engine's. It is a **directory
beside a detail** (the kit's `DirectoryList` in a `ListDetail`): the list holds
its place while the detail scrolls beside it (above a phone's width the screen
asks the frame for the window's height, as the Inbox does), and on a phone the
two are one column, the list until something is chosen, then the detail with a
way back.

The directory is **every integration by category**, each of Crewlet's own
services first in its category, so the built-in answer sits beside the apps
that answer the same need:

| Category | Lines |
|---|---|
| Chat | Chatlet, Slack, Mattermost |
| Work tracking | Worklet, Jira |
| Docs and knowledge | Doclet, Confluence |
| Code | GitHub, GitLab |
| Observability | Datadog |

**Atlassian is listed by product**: it is one connection that serves two needs,
so Jira sits under work tracking and Confluence under docs, each with its own
product's state, and both open the one Atlassian page, since it is one key and
one account per agent. A line stands at the rail's row step (30px) and carries
its mark, its name (a product's vendor inline after it, in the quiet ink:
Atlassian after Jira and Confluence, Crewlet after Worklet and Doclet) and its
state only where it carries one: **In use** or **Not in use** for a
service, which is Crewlet's own and so never *connected* (none on a node
running no company, which is "not known yet", never Not in use), and
for an app the engine's bucket (*Connected*, *Needs attention*, *Setting up*),
never *Not connected* repeated down the list.

**The home** (the bare address, nothing chosen) is **what the company has in
use**, under one fixed description of what the page is for (*The tools your
agents work with: Crewlet's own services and the apps your company connects
for them.*), the same for every reader and never a count. Then what needs a
look (each issue under its app's name, linking to the app's page with the form
its fix needs already open), or one line saying all is well. Then, under **In
use** with a quiet count of every card beneath it (`home.ts`, one function and
one table test), the connected apps (Jira and Confluence as their own
products) and the Crewlet services in use, each a **compact card** (the kit's
`CatalogTile` at `density` `compact`: one header line with the mark, the name,
the state and a figure, the one action at its end, and one line of what it
gives), two to a row. The action is named for its card to a screen reader
(`actionName` in `parts.tsx`: "Open Worklet", "Replace key GitLab", the
spelling the app's own page uses), because a column of cards each saying Open
is a list of links nobody can tell apart. And, for an Admin while only Crewlet's services are in
use, one line saying apps connected from the list show there too. Then the
recent activity (the latest deliveries and each app's last check), and the
**address apps deliver to**, said once at the foot, cut to fit with the whole
of it in its title and copied with a press. Services not in use and apps nobody
connected are the list's alone, and a home with nothing in use says so and
points at the list. A connected app's figure is its agents (*x of y agents
ready*) where it gives each agent an account, and otherwise its newest
delivery. Sections stand one small step apart (the kit's `Section` at
`spacing` `tight`).

**The address is the selection**: `#/company/integrations/{kind}` selects an
app's key, a surface (`jira`, `confluence`, and the `forge` relay and
`atlassian` select Jira's line) or a service (`worklet`, `doclet`, `chat`), so
an address naming any surface of a tool lands on that tool. An arrival's
`open=` names the form to open (`connect`, `continue`, `rotate`, `settings`,
`agents`), taken out of the address once read and held for the selection it
arrived with: a fix followed from the home opens its form on a page that is
already mounted, and leaving for another selection drops it, so coming back
through the list opens none.

**An app's page** is its header (the kit's `ObjectHeader` carrying the vendor's
own mark, the engine's word for its state, its one-line description, and the
facts: agents ready and the deliveries this node counted, only when
`traffic_known`) and four tabs:

- **Overview**: the issues, each its own row with what is wrong, what it is
  about (its subjects as chips, a long list folded to a count that expands in
  place), what to do, **who owes the next move** (*Waiting on you*, *Crewlet is
  on it*, *The app is on it*) and the one control that answers it; the reconcile
  loop's findings in the engine's words, and the facts a surface's own row
  states (a secret that did not resolve, an address that moved, deliveries that
  reach no agent, a pass that failed with nothing to show for it), each said
  once. A working tool says so, and an advisory (an access wider than asked) is
  a note, never an issue.
- **Agents**: each agent with its standing, its tier where the app grades one
  (the engine's own words, beside the name, weighted by trust only on a scale
  the engine names), what it reaches, and its own step at the app (*Create app
  on GitHub*, *Install on GitHub*); and **Check now**, which asks the reconcile
  loop to look at the tool at once (`POST /setup/integrations/{kind}/check`).
- **Activity**: the provisioning passes and each surface's deliveries.
- **Settings**: what the tool is set to, a credential named and never shown.

The header's one action is exactly one of four, decided by `actionFor` from
the same inputs as the state:

| Action | When | What it does |
|---|---|---|
| **Connect** | nothing configured | opens the tool's settings form in the pane |
| **Finish setup** | a requirement unanswered, or a surface of a partly-connected tool not yet configured | opens the same form |
| **Replace key** | a `credential_expiring` or `credential_rejected` finding on a configured surface | opens the same form, headed *Replace the {tool} key*, with the engine's reason first |
| *(none)* | anything else, including a tool this build answers no form for | the page is the tool's management, or links its docs |

beside **Settings** and **Disconnect** on a connected tool. **The settings form
opens in the pane** in place of the tabs, with the directory still beside it:
connecting, finishing setup and replacing a key never leave the page. A tool
not yet connected shows, in place of the tabs, what connecting gives the
agents, how each gets in, and what to have ready. A credential the engine can
check carries **Test key** under its field: one read-only call to the app
(`POST /setup/integrations/{kind}/verify`), whose verdict (*works*, *refused*,
*unreachable*, or not checked, with the app's reason) shows under the field
and goes when the value is typed over; a credential the app proves only by
using it says how, in its help. Credentials the app checks together
(`check_with`, Datadog's API key and application key) share one **Test keys**
control after the last of them: it is disabled, with an accessible reason
naming the empty fields, until all of them hold a value, shows one verdict
that names the key that failed, and that verdict goes when any of them is
typed over. A check the engine turned away (a `${NAME}`
naming no secret) says why and claims no verdict.

**Crewlet's own services** have pages laid out as an app's and filled only
with what the engine answers. Worklet counts projects and their open
tasks, lists the projects, every agent with its open, blocked and overdue
work, the newest changes, and the `tracker` block's settings; Doclet
counts spaces and pages, lists the spaces, every agent's own space
and onboarding pages, the newest edits, and the `knowledge` block's settings.
An unset key reads *The engine's default* rather than a copy of the engine's
constant. A service the company runs elsewhere says what does its work instead
(*Jira does this for your company*, with the way to Jira's page), what running
it here would give, and that the company document decides it: turning a
service on or off is not offered, since a switch here that did nothing would
be a lie. Chatlet counts nothing, since the rooms it answers for are the
reader's own seat's: in use, its page says what it gives and, to a reader
holding Chatlet, opens it (a reader without Chatlet is told it needs Chatlet access); on a
vendor's chat it says which (*Slack does this for your company*, named from the
`viewer` answer, which tells it to a holder of Chatlet), with the way to that
app's line.

**A reader without Admin** sees the whole directory: the services with their
states and their pages in full (each count read only where the reader holds
that service's app), and the apps with no state, each page saying an admin
connects and manages apps (and offering the sign-in to a reader who is
nobody). Nothing on it asks a question the engine keeps for Admins: the
`integrations` question, `/setup` and the `config` answer are read only for an
Admin, and the address at the foot is the setup listing's, so it is not shown.

On a [managed document](../concepts/configuration.md#managed-configuration),
for a credential that is not one of its writers, the page leads with a callout
carrying the managed sentence, and **Connect**, **Finish setup**,
**Disconnect** and an agent's **Create app on GitHub** are disabled with it
(they write the document), while **Replace key** and the settings form stay
open: a rotation seals a value the document already names and changes nothing
in it, which is the break-glass a managed deployment keeps for a person. The
directory ends at the tools this build serves: every other tool an agent can
reach is an MCP server, added on Settings › MCP Tools. See [integration
reconcile](../concepts/integration-reconcile.md#what-the-dashboard-shows).

**MCP Tools** (`#/settings/mcp-tools`, once Tools & MCP) is admin's, like every engine setting: it is where MCP
servers are added and their launches read, and which tools a seat has is
already on that seat's page, from the overview's `tools` push. It is the
servers first, then the catalogue. **MCP servers** draws
the engine's `mcp_servers_status`: each server's launch (the command and its
arguments, or the address — never an `env` value or a header), who it
**reaches** (every agent seat, *n seats* or *No seat*, from each agent seat's
`tool_sources` on the pushed org — the engine's own grant), the engine's
**state** (*Running*, *Partly failing*, *Failing*, *Waiting for credentials*,
*Not started*, *Not reported*) and **one chip per live node** with what that
node started, and under THAT chip the first failure it reported, in secondary
ink with the seat it was launched for — the chip carries the danger tone and
names the node, so the reason does not repeat either. A seat's instance held
back for a credential an integration is still minting is named on the chip
(*n waiting*) and never toned, since it is no fault, and what it waits for sits
under the chip as a failure's reason does; the server's page lists it whole
under *What it waits for*, apart from *Why it failed*. A server with
`tool_annotations` keys no started instance lists says
*n tool_annotations keys match no tool* under its
name, and its page names them whole. The engine puts `tool_sources` on every
agent seat it pushes, so a shared server reaches every agent seat and a
per-seat one the seats granted it. The launch
is one line cut at its end, never broken inside a token. A node whose last
heartbeat carried no status is named once above the grid and its chips read
*not reported*, never *none*. The **From MCP servers** tile counts *n of m servers
running* off the same answer — never the registry's origins, which read *0*
over a table of failing servers. The status names nodes, commands and
failures, which is one more reason the section is admin's. A row opens the
**server's page** (`servers/{name}`): its state, transport, instances, reach
and tool count, the launch whole, the annotation overrides that match no tool,
and every node's reason as the heartbeat carried it, unclamped — the one place a reason the grid clamps can be read
without a pointer — above the
catalogue narrowed to its origin, whose chip is drawn and selected even when
the server registered nothing. A reader refused the status (or one whose read
failed) is told the page could not read it — never that no configuration
carries the server, which only an answer can say. An origin with no tools says so — *docs has
registered no tools*, with the reason from the engine's state and **Show all
tools** — rather than *No tool matches “”*, which is kept for a search that
found nothing. The catalogue's rows are a tool's name, its description and
what it can do; its origin is a column of words (*Built in*, or the MCP
server's name) and the filter's chips name it the same way, both drawn only
where the catalogue holds more than one origin, and the sentence of what its
hints say, with what nobody advertised, is on the tool's own page.
**Add an MCP server** is drawn for every reader and
disabled with the reason for one who cannot change the configuration: a
name (its example is never a name the company already has; a name the
configuration carries is refused, and one only a node still runs from an
earlier revision is free and said beside the field, since the engine's
create judges the configuration alone), how it runs
(*Launch a command* or *Connect to an address*), whether
there is one for the company or one per seat granted it, the command and its
arguments one per line (or the address), and the environment or headers as
`NAME: value` rows whose value is a secret field (`$` offers the sealed
entries), each with its own **may be unset** box: ticked, the row's key is
sent in the server's `optional_keys`, so a value whose secret has no value is
left out rather than keeping the server from starting — said once above the
rows, and refused before the round trip for a row whose value names no secret,
since nothing in a literal can be unset. Two header rows spelling one header
apart (`Authorization`, `authorization`) are refused the same way, since HTTP
and the engine read them as one. Add checks the whole company with the
server in it (the create-only
`PUT /config/mcp-servers/{name}` with `If-None-Match: *` and `dry_run`), says
any warning the add introduces — not one the company already carried — and
places a refusal beside its field; then it stores the revision. A tool's own
panel names the seats **granted** its server from the same pushed
`tool_sources`, so opening a tool, or stepping through them with `[`/`]`,
asks the engine nothing. That list is derived from each seat's configuration
(the credentials it declares under `mcp_env`), so the engine pushes it only to
an admin; an absent list on this admin's screen means a node older than the
field, and the panel says so.

**Models** is every model the company runs and every task an operator does on
one, read from the engine's own reading, `llm_entries`, which every member may
ask: a member's names no credential, no account and no vendor sentence, and
every action on the page is an admin's. It is kept current on a fifteen-second
poll while the page is open, when the fleet applies a new configuration and
when a seat's provider limit moves on the live state. Two lenses sit in the
page bar. **Models** has stat tiles (the models, how many are ready, how many
sit out or cannot sign in, and the week's tokens), then the model alarms
the engine raised (`llm_credential_expiring`, `llm_credential_expires_soon`,
`llm_sign_in_refused`, `llm_limit`, `llm_limit_waiting`), each naming its cause
and what it stops, grouping the models one credential or one plan holds, its
vendor's sentence behind **Details**, and offering the one gesture that fixes
it. A limit that holds seats also offers **Use a different model for these
seats**: a menu of the models that answer now (never one on the limited plan,
and for coding only a CLI that runs it), or a line saying none can, and **Add a
model**, each opening the wizard on *Use it for* with every seat's category on
the limited models added (`?add=`), for Review to show before anything is
written. The address only preselects: a seat the company no longer has, or
coding on a CLI that runs none, is dropped. Then a row per model: its CLI's mark, key and model; its status (one word
from the engine's reading, and when it changes); how it signs in and the secret
it reads; what it is used for, by category; when it last answered; and its
menu. **By job** is every agent seat as a row with the model it runs for each
category of work (*Coding*, *Reviewing and delegations*, *Routines and
evaluations*), Ask Crewlet's model above the grid, a cell's model's condition
in the cell, a split category said job by job, and a quiet note where a model
under a fixed deadline is not a fast one; a pick opens that model's wizard on
*Use it for* with the seat's category added.

A model's page (`models/{id}`) has its sign-in (the route, the secret, when and
by whom it was stored, its expiry, and on a CLI login each node's state with the
command that makes it there), its sit-out with **Retry now** and **Clear
sit-out** for every model on its plan, its last answer and failure, its usage
over seven days, its limits and every place that runs on it, grouped by
category with backups apart and Ask Crewlet named. **Test** runs the CLI's
own checks on the answering node and says each in words. A Codex model signs
in with a device code: the page shows the address and the code and follows the
session until it ends. **Remove** names every place that runs on the model in
words, asks where each moves, checks each choice with a dry run and removes the
model and every repoint as one revision.

**Adding and editing a model is one wizard**, a page of its own in place of the
list: the name and the coding CLI (fixed once the model exists), how it signs
in, the model (the CLI's aliases and the public catalogue's models, or any name
as text), where its coding runs (for a CLI with a runner), what it is used for,
its limits, a test call and a review of exactly what changes. A pasted
credential is tested before anything is stored and sealed before the document
that names it is written; the document is checked with a dry run against the
revision it edits and stored with `If-Match`, so a colleague's save in between
is a conflict to re-read. *Use it for* writes each category's fields as one
model each, as
[Managing models from the dashboard](../concepts/llm-backends.md#managing-models-from-the-dashboard)
lists, and the review names every fallback a write drops.

**Secrets** lists the names the fleet holds, their key ids and provenance, and
can store, rotate and remove one over the same guarded routes the CLI uses.
Every row is sealed in the fleet's store; **Source is provenance** — which path
last wrote the row, in that path's own word and never one a request chose
(`api` for this page, `PUT /secrets` and the CLI against a running node;
`setup` for an integration's setup; `provision` for what a provisioning pass
minted, the engine's own or a provisioner's `-secret-store` run made with its
node's engine stopped; `cli` for `crewlet secrets set` and `llm-login` for a
coding CLI's login stored by `crewlet llm`, each written to the fleet's store
directly with its node's engine stopped), drawn in the column as API,
Integration setup, Provisioning pass, CLI and CLI login — and the credential's
peek says what its word means rather than leaving it to a pointer. Those five
are all the listing can carry, so a word outside them is said as one this
build does not know rather than guessed at.
Three tiles count the credentials, the names **read by nothing** (no field in
the active configuration points at them: a forgotten `${VAR}`, or a credential
nothing needs any more — not known, and never 0, when the reference check did
not answer) and the distinct key ids. The peek says each property once: its
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
warning since it is the one reading that is a guess. The **Posture** column,
like the Posture fact on a node's page and in its peek, is the node's own word,
`serve` as a success tag and any other as a warning. Its dash says which of two
absences it is, in its title and to a screen reader alike: a node that could
not read the control plane on its last heartbeat publishes the rest of its
status without a posture, rather than the `serve` its readiness probe falls
back to, and one whose last heartbeat carried no status has no in-flight count
either. Then
**Seat placement** — each seat, the node holding it, its lease and **Since**:
the tenure's start (`acquired_at`), stamped when the epoch is minted and
carried through every renewal, so "since 2h ago" means the seat has not moved
node in two hours. A node's own page draws the same column for
the seats it holds. The seat (and, beside it, the duty) is each lease table's
one flexible column and **Held by**, **Since** and **Lease** size to their
content, so the name the row exists to show is the value that keeps its width;
Seat placement and Company-wide duties sit side by side only where each panel
is at least 480px wide and stack below that (about a 1520px window). **Held by**
is the same link to the node's page in both tables, at the body's weight, so
the seat or the duty stays the strongest text in its row. A node's build
version is one token in its header, on one line with the whole string in its
title. Neither screen repeats a count in its page bar that a tile below
already shows. **Broker members** is
the fleet broker's membership twice, as the nodes advertise it and as the
metadata group counts it, with every disagreement named and the removal of a
dead member behind a typed confirmation; its count is the rows it lists. Under
the last card, **Backups & retention** is a related row (`RelatedRow`, the
quiet linked row Company › Principles points at Settings › Limits with, drawn
only for a reader who may open the page it names) rather than a footnote,
since every log's position is where "which node is behind" goes next. See [seat
ownership](../concepts/seat-ownership.md) and [the fleet guide](../guides/fleet.md).

**File storage** (`#/settings/storage`) is a page of its own in the Engine
group, where it was a card under Nodes' lease tables and nobody looking for
the company's files would think to scroll. It is the object store: which store the
company's files are in — the fleet's own NATS bucket, or the S3 bucket it names
— as a settings table row with its key, `store.objects.backend`, then the
collector's passes under a head naming the node that ran the last of them, one
row for each (the hourly
collection and the daily audit) with when it ran and what it found: the
objects a collection deleted because no file names them and the unfinished
uploads it abandoned; the files an
audit asked about and how many were missing or damaged. It reads
the fleet's record rather than this node, so it says the same thing whichever
node served the screen. **Files that are missing or damaged** are the one
reading an operator opens it for: when the last audit to run to its end found any missing
or damaged, a danger callout above the passes names the first of them — each
file, whether it is missing or damaged, and its object — to restore from a
backup or upload again; an audit that failed after it is said in its row and
never clears the callout. A collection that stopped judging keeps what it
deleted first in its row, and a sweep of unfinished uploads the store refused
is a warning naming the permissions an S3 identity needs. A record the coordination store would not give up is said as that, not
as a fleet whose collector has not run. There is nothing to press: the store
keeps its own copies, so there is no gesture on it. Under the passes,
**Storage quotas** is what the company holds against the plan's quotas in each
node's file (`artifacts.limits`): the artifacts' files, their count and the
datastores' footprint, each a figure over its ceiling with a bar and its key,
counted over every artifact, private ones and the trash included. It was the
foot of Artifacts › All, the one block on that gallery about the engine's
storage, and the gallery keeps no pointer here: the quotas are an
administrator's reading about the engine, found in Settings with the rest of
it. See [the object store](../concepts/object-store.md).

**Backups & retention** says each problem once, and says it first. It reads one
retention report for the whole page and hands it to every panel, so the alarms
and the panels under them describe the same report. **Alarms** lead: each kind
once, named in words, with the engine's measurement as a sentence, the logs it
stands on (logs one term holds are one line, "Trim held by the backup on
tracker, vectors, pages and usage, for 46h", with each log's own measurement
under it, since what a log keeps past its replay window is why it is listed and
no shared date says it; each log a link to its page; a single measurement that
is the alarm's is said once with the logs it was taken on beside it, so a fleet
with no backup yet reads "tracker, vectors, pages, iam and usage: No verified
backup of this log has been recorded…" once — the engine carries a sentence
measured alike on several logs once, naming them, and the page draws what it
is given), and the engine's remedy, each a row
of the one Alarms card with its warning glyph rather than a callout framed
inside it. The backup they ask for is the page bar's **Take a backup**, the
only one on the screen. Then the record of
backups, then **Replication**, then **Log retention**: a row per log with its
facts and what holds its trim in words ("held by the backup"), said once in
the card's head where most logs share it ("Held by the backup on every log"),
so only a log that differs carries the tag, and its six
checks behind a disclosure, each in words beside the term's own name, with the
remedy printed only for the check that decides the trim. **Snapshots and
joining** ends the page; a single node, which takes no snapshots by design,
reads as one sentence rather than a table of `sole_node`. The page said a
missing backup about twenty times before this: three empty panels, a banner and
an alarm per log, and the same remedy under every log.

The record reads `backups`. **Take a
backup** asks the node serving the page for a copy (`POST /backup`) in a
directory on **that node's host** — the dialog names the host first, since
nothing is downloaded — and offers a fresh directory beside that node's last
copy when it has one (stamped to the second, never one a backup already went
to, and nothing when that copy sat directly under `/`); a relative path is
refused before the round trip, and the engine's own refusal of a directory —
occupied, or one its host cannot create or write — is said on the field. The request
waits up to 30 minutes, the CLI's own wait, and closing the dialog does not
stop the copy: the outcome arrives as a notice either way. Its figures are the
oldest backup any log's trim reads (each log reads the newest counted backup
that covers it, so under `operator` there is one per log acknowledged, and the
stalest is the one the backup-age alarm leads with), the `backup_floor` policy,
how many were requested and failed (over the span the event log reads, named
from the `event_history_seconds` the engine reports rather than written into
the screen, or "in the newest N" when the history filled its page), and the
backup owner (`retention.backup_owner`, "Not set" where nobody is named). With
nothing backed up and every node's history read, the figure says "none yet"
and the two lists are not drawn. Otherwise come **Newest per node and log**,
each node's newest copy and the operator's newest acknowledgement of each log,
one row per log, with its directory, size (none for an acknowledgement, never
0), reach, and whether the trim counts it: a row reads **counted · newest**
where some log's trim reads it, the engine's choice rather than the latest
date, so a newer copy nobody verified reads **not verified**. Then **Backup
history**: every backup a person asked any node for over the event log's 30
days, with the node that holds it, the person, the directory and whether its
manifest was written. A failure is drawn as one. Neither table is derived from
the other: the register keeps each node's newest point and each log's newest
acknowledgement only, which is why a failed night shows in the history alone.

**Audit log** reads five sources over one window: Worklet's feed, Doclet's
changes, the configuration history, the credential rows,
and **Runtime**, the runtime audit (`events{source: "operator"}`), which is
every call a person made through a running node whatever became of it: each
operator tool call that is not a proven read — through the act route, an
assistant over `/operator/mcp` or the human write surface, whose chat erase,
prune and horizon are rows named by their verbs (`chat.erase`, `chat.prune`,
`chat.channel.retention`) — and every backup. **Chatlet is not a source of its
own**: the other four are change records and a transcript is content — a
private room's is not an administrator's to read — while what a person *did*
in chat is already here, every chat write being a call this source records.
A runtime row names the tool (or "backup" and its directory), who made the
call as the engine recorded it (`actor` and `actor_kind`: the seat a bound
person acted as, or a credential's own login) with the credential it came
through (`operator_id`) beside it, and in its detail's title the node that
served it; its arguments are
never recorded and the cell says so. A refused or failed call is marked as a
failure. Worklet's feed and the runtime audit are windowed by the engine,
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
revision** — its header, where it came from and which nodes run it (a node
whose last attempt of it failed is listed as refusing it, naming what it
runs, even when that is this revision: the refused re-activation a credential
rotation makes, and not counted among those that run it) — with
no lens bar and no running document under it, and **See its changes** opens
the Diff lens against the revision's *parent*, which is what that save
changed (against the active revision, the active revision's own changes were
empty). **It is read-only for the document**: a revision is written by
`PUT /config`, `crewlet config import`, or a screen's own scoped write (a
budget ceiling, the org builder), each of which validates against the schema
before anything is stored, and the screen says so first, as its page note on
every lens.

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
  src/routes/<dir>/         one directory per group of framed screens, each
                            its own lazy chunk, as ScreenDir in
                            app/lazyScreen.ts declares them (home, inbox, me,
                            worklet, chatlet, agents, live, doclet, artifacts,
                            brand, spend, settings, account), with one file
                            per screen inside it and an index.ts naming what
                            its chunk exports. Four things sit outside that
                            shape: NotFound, which belongs to no directory and
                            is what the dispatch falls through to; routes/org/,
                            the organization builder, one screen big enough to
                            be a directory, and a chunk, of its own;
                            routes/signin/, the screens drawn outside the
                            frame, a chunk the idle prefetch leaves alone; and
                            routes/artifacts/new/, where each kind's create
                            page is a chunk of its own, never prefetched either
                            (see below)
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
reasons, Worklet's sort keys, change kinds and grouping axes, the
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
model call, so the gate refuses it for real — a round's charge, or the call the
seat's meter holds once its own context assembly has filled the day, which the
meter records as the gate's refusal — and its meter arrives with
`refused_at`, `resets_at` and `state: refusing`; and the health push has to
count its live `nodes` and name the worst of its standing `alarms` (the company
has never taken a backup, so one stands). The replay and the Go half each hold
all of it, so a red run says which side stopped.

The captured tab is a **person's**: a browser holding the cookie a password
sign-in minted, whom the identity directory binds to the founder's seat, holding
`admin` — every permission there is, so the one tab is sent every kind of frame
a tab can be. It is the only kind of socket the company's own chat is pushed to,
since every chat frame is decided per recipient seat; one tab is one person, as
on the dashboard. So the capture also carries the founder using **Chatlet** the way
its screen does: a room made from the tab with a seat in it, looked at (the
chat room's `presence` frame), a message said and reacted to, and the seat asked in that
message's thread. The seat's turn claims to have answered, is refused for
having said nothing, replies in the thread — held on that round until the
room's presence has shown it at work, which is true only while its turn runs —
and submits citing the reply: three rounds, the captured company's cap, so the
turn ends on its own `delivered` rather than on the engine's rescue, which the
Go half reads off the turn's phase record. Then the first message is edited and
deleted, the room read through the seat's answer with `mark_chat_read`, left,
and pruned through the human write surface's
`POST /chatlet/channels/{channel_id}/prune` — an administrator's gesture, with the
step-up the sign-in has just proved. Every other change is a press of the act
transport. Every chat push kind is then on the capture, held against the
engine's own list (`stream.ChatPushKinds`), so a kind the engine adds is a
capture that stops passing until it is produced. The replay is handed an
account of the room written beside the frames, never read from them — its id,
the person and the seat, the highest `channel_seq` the engine minted, every
record it took (its op, the message it names, its author and their kind, and a
post's thread), where its last record sits and the cursor the founder flushed.
What the engine minted is read from its answers: the room's id, each message's
id from its post's receipt (the seat's from the room's transcript), the
numbering, the prune's position. Each record is otherwise the gesture the
capture made, written down as it made it — the founder as the human seat a
person writes as, through the act transport and the prune route alike, and the
seat as an agent in the thread it was asked in — so an author or a thread the
write path recorded wrongly is a failure rather than part of the expectation.
The replay holds the store to that account: the room filed with that
high-water mark, a frame filed for every record naming exactly what the capture
did and none naming anything else (the Go half holds the frames to the same
account first, so a red run says which side stopped), its newest moment the
prune's position, presence that at some frame showed the founder looking and
the seat working, and the founder's read state with that cursor and a
revision. Each record is held on its own because the room's messages taken
together are named by its posts alone: an edit, a delete or a reaction filed
under the wrong message is a screen re-reading the wrong range of a room, and
only a comparison per frame sees it.

The capture also carries two **writes** whose floors the replay holds, each
made AS THE PERSON THE SESSION RESOLVES TO under its own operation key in
`Idempotency-Key`, exactly as the dashboard's `act` sends it. The founder pins
a project through `POST /operator/act/set_pins` after the frames are taken, and
the founder's first message in the room is `POST /operator/act/post_message`.
The route is the node's own operator surface, the one `crewlet run` builds
inside `api.NewHumanSurfaces`, so the harness wires no second catalogue, and
the viewer names each write among its `acts` before it is pressed. The Go half
holds each answer to the read it promises: its `position` is in the grammar a
read accepts back, and `GET /worklet/people/{handle}` (for the pin) or
`GET /chatlet/channels/{channel_id}/messages` (for the message) at
`read_level=session&min_position=<position>` is served at `session` and already
holds the write. The replay hands the same answers, byte for byte, to the
client's own `act` and one tab's `SessionFloors`: the `tracker` floor must be
the pin's position and the `chat` floor the message's, and a read of
`work_person` or `work_items` — or of `chat_messages` or `chat_channels` — must
name its log's as its `min_position`. A floor the client could not parse raises
nothing, and the screen that pressed the button would redraw from before the
press with nothing to say so.

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

**A chunk per screen directory, and the next one fetched while nobody waits.**
Every screen is loaded by `app/lazyScreen.ts`, one chunk for each directory the
tree above lists (`routes/<dir>/index.ts`, held to `ScreenDir` by
`app/lazy.test.tsx` in both directions), plus one for the org builder, which is a
section of the Org chart and larger than any whole workspace, one for the
sign-in screens drawn outside the frame, and one for each kind's create page
(`routes/artifacts/new/`). The entry the shell loads holds
the frame (the sidebar, the page header, the socket client) and nothing a
screen draws, so a reader who came to read the Inbox parses the Inbox and not
the org builder. The command palette and Ask Crewlet's panel are layers
mounted only while open, each a large piece of the frame, so each is a chunk of
its own, and they are the first two the idle prefetch fetches;
`app/lazy.test.tsx` refuses a static path to either from `main.tsx`. A
screen's peek lives in its directory's chunk beside it. Two things keep the
second click fast: hovering or focusing a sidebar row starts fetching the chunk
it leads to, and once the first screen is up every other chunk is fetched in
turn, one at a time, each in an idle moment the browser reports (and not at all
for a reader whose browser asks to save data), except the sign-in screens and
the create pages, which a reader who is signed in, or who opened Artifacts to
look, never needs. `app/lazy.test.tsx` refuses a static import of a screen's
module from outside `routes/`, because the bundler follows one wherever it is
and it would pull that directory back into the entry with nothing failing.

**A chunk that does not arrive says so, and can be retried.** The usual reason
is an upgrade: the page names its chunks by content hash, and an engine
upgraded while the tab was open serves different ones. The screen then names
what is missing ("The Spend screens could not be loaded"), says "The engine was
updated. Reload to continue.", and offers Reload, which is what fixes it, beside
Try again for the other reason, a request the network dropped. Try again really asks
again: a failed load is forgotten rather than cached for the life of the page
the way `React.lazy` caches one — once its boundary has drawn it, so React's
replay of the render that waited on it reads the same rejection rather than
starting a request nobody reads. A load only a hover or the idle prefetch asked
for has no render to deliver it to, and is forgotten as it fails.

**The bundle has a budget, measured as the engine sends it.** The split above is
undone by one static import of a screen's module from the frame — the bundler
follows it and the workspace is back in the entry with every other test green —
so `internal/api`'s `TestTheDashboardFitsItsBudget` measures the crawl described above
and fails a build over any of four numbers:

| What | Budget | Measured as | Why this number |
|---|---|---|---|
| The initial load: the entry, its static imports and the linked stylesheet | 358 KiB | gzip at the best level, which is what the engine serves | Under two seconds on a 1.5 Mbit/s link. It was 300 KiB until the rich editor: the first paint had reached 297 KiB, and the shell sends every design system stylesheet in one sheet by rule, so the editor's 1.8 KiB of rules ride it although its code is fetched only to edit, and the Markdown grammar the renderer and the editor share became a 2 KiB chunk of its own. The create pages took it to 314.5 KiB: their rules (3.1 KiB) moved into `create.css`, which arrives with them, and the command palette (about 10 KiB) became a chunk of its own, which left it at 300.2 KiB. The rail head's engine status (uilet 0.41) took it to 312.1 KiB, over the 312 the settings tables had asked for. The editing pieces of uilet 0.46 and 0.47 (their rules in the one sheet, and the character count FormField's limit brings into the entry) took it to 315.1 KiB. Artifact folders and the company brand (uilet 0.49 to 0.51) took it to 322.6 KiB: the brand scope's and the folder screens' rules ride the one sheet, and the folder and brand routes are the frame's. Ask Crewlet took it to 336.4 KiB: the kit's Ask Crewlet pieces ride the one sheet, and its button, page context and page actions are the frame's while its drawer is a chunk of its own; uilet 0.57.1 and 0.63.0 and its setup form took it to 340.3 KiB. One vocabulary for seat states, uilet 0.66.0 and choosing screen shapes by the screen's width took it to 345.6 KiB, under 346. uilet 0.67.0's product marks and 0.69.0's BriefCard, a read named in the answer rather than drawn as a card (ADR-0061) and a model's usage limit as one condition per entry (ADR-0064) took it to 351.0 KiB, under 352, and drawing what prose names with its mark, with the flattening moved out of the renderer, brought it back to 348.9 KiB. Steering a turn whose coding run is out measured 350.9 KiB, under 351, before it met that flattening and 348.8 KiB after, and a coding run's question asked as a decision took it to 349.0 KiB. Chatlet adds 6.0 KiB to it: its pushes and rooms in the socket client, routes and rail are the frame's, since a mention is told on whatever screen the reader is on. With it the load measured 355.0 KiB, 29 bytes past 355 KiB, so it moved to 356 KiB. Merged beside Settings › Models, whose frame measured 351.0 KiB, it measured 357.0 KiB, 28 bytes past 357 KiB, so it moved to 358 KiB: 1.96 seconds on that link |
| Each chunk fetched later: a workspace, the org builder, a lazy stylesheet | 150 KiB | the same | Half the initial load when it was 300 KiB, for the first click into a screen the idle prefetch has not reached |
| Every font face the build embeds | 90 KB | as served — woff2 is never recompressed | The four Geist faces (83,736 bytes) and room to re-cut them, never a fifth |
| The whole of `static/dashboard`, which every binary and image carries | 4913 KiB | raw | Every binary carries it, opened or not. 3 MiB, then 3.5 MiB for the artifacts workspace, then 4 MiB for the rich editor, whose chunk is about 276 KB raw (ProseMirror 228 KB of it) plus 15 KB of its packages' notices: it measured 3.54 MiB after it. Then 4.25 MiB: artifact folders and the company brand took it from about 3.99 MiB to about 4.14 MiB. Ask Crewlet (its panel, page actions, deep work and files, and the kit's components and rules) took it to 4504.1 KiB. Telling Ask Crewlet what each page action takes and what the Inbox holds, its cards drawn in a person's terms, one vocabulary for seat states, uilet 0.66.0, a brand draft's own cards and which revision each node runs took it to 4553.1 KiB, under 4554 KiB. uilet 0.67.0 to 0.74.0, the answered inbox, the brief, a write's caveats in a person's words, a read named in the answer (ADR-0061), a limit's remedy and a model's usage limit (ADR-0064) took it to 4610.7 KiB, under 4611 KiB. Drawing what prose names with its mark (uilet 0.75.0 and 0.75.1's InlineRef) and linking only what the dashboard built took it to 4629.3 KiB, under 4630 KiB. A person's stop ending a seat's coding run and steering a turn whose coding run is out took it to 4631.8 KiB, the hardening of inline links and uilet 0.75.2's unclipped marks to 4632.7 KiB, and a coding run's question asked as a decision to 4637.3 KiB, under 4638 KiB. Settings › Models (the models API's pages and wizard, with uilet 0.76 and 0.77) took it to 4773.6 KiB. Chatlet took it to 4912.3 KiB, so it moved to 4913 KiB, past a doubled baseline: the 2.3 MiB measured when the budget was first set, doubled, is about 4710 KiB |

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
  key in `manages` expands to, automatic management by a lead and which of
  several managers is primary are all rules of the engine, and the `org`
  projection carries their result in its `derived` block. `lib/seats.ts`
  indexes that block over the authored tree and implements none of the rules:
  its earlier TypeScript copy derived a different handle than Go for a name
  like "İlker Demir", and a handle keys a seat's memory. The engine derives
  both halves from one document in one call, so they always pair, and the
  block is laid over the tree as the authority rather than checked against
  it — there is no "hierarchy not reported" state for a screen to draw. Units
  pair by key and seats by handle, both minted into every document a write
  stores, so a namesake never claims another's entry. `{}`, a node running no
  company, is an empty company.
- **A seat is never paired by its name.** A seat's name is prose and two
  seats may share one. The roster row, every `agents` overlay, a sandbox run,
  a phase record, a turn row and a spend row all carry the seat's `agent_id`
  — derived from its handle, which no namesake shares — and the store merges an overlay onto its row by that id
  alone (`protocol/store.ts`), dropping one for a seat the roster does not
  carry. A seat of the org index pairs with its roster row by HANDLE
  (`lib/seats.ts`' `liveRowFor`), since the two are cut from one chart view
  and a handle is unique; a URL or a filter names a seat by handle, and so
  does the question it asks the engine. Every one
  of these used to match on the name, so the second "Engineer" in a company
  wore the first one's state, live call, sandbox, turns and spend.
- **A link kept before a rename still opens what it named.** A seat is
  addressed by its handle and a unit by its key (`lib/seats.ts`' `seatPath`
  and `unitByKey`), and a rename moves neither: both are the object's
  identity, which the engine never changes, so there is no alias to resolve
  and no route to rewrite. A name is never an address, except for a seat the
  engine gave no handle, which `seatPath` links to by name.
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
  each wrong about a different case. The one reader outside it is the
  org builder's session over the `/config` document, which is not a screen's
  read but half of a conditional write: it holds the `ETag` a save sends back
  as `If-Match`. It aborts a
  superseded read and an unmounted screen's, re-reads when the socket comes
  back after a drop and, where asked, when the tab comes back or on a poll —
  those last quietly, keeping what is on screen. A sign-in needs no re-read of
  its own: it is a screen outside the frame, so the screen it returns to mounts
  again and reads as the new reader. A refusal replaces what is on screen; a request that never reached
  the engine keeps the last answer with the error beside it; and an answer
  belongs to its key, so a read whose key changed reports nothing until the
  new key answers.
- **A retry is the engine's to schedule.** A socket refusal is asked again
  after its `retry_after` and a REST refusal after its `Retry-After`, quietly,
  bounded at thirty seconds (`RETRY_AFTER_MAX_MS`), and nothing else is
  retried on its own: a `retry_after` of `0`, and a `503` the engine wrote with
  no hint, is a node no wait repairs (a full log, no keyring, no surface), and
  re-asking it only repeats it — see
  [Honest empty states](#honest-empty-states). There is no retry constant of
  the client's own beyond the engine's health tick, because a flat one is
  wrong both ways on one fleet — too early for a node grinding through a long
  backlog, too late for one that caught up in milliseconds.
- **A newer peer's frame is ignored and counted.** A push kind this build does
  not dispatch is dropped rather than thrown on or applied by a guess, and the
  store counts it (`unknownPushes`), because the same fall-through is what a
  kind this build's engine sends and its client forgot looks like; the e2e
  replay fails on any. A field a later build adds is declared optional in that
  build's `protocol/types.ts`, so an answer from a node still on the build
  before it, without the field, takes the screen's "unknown" branch rather
  than reading `undefined` as a value.
- **Nothing outside `src/protocol/` reaches the network.** A screen that called
  `fetch` itself would work on the happy path and be the one request in the
  product with no session, no deadline and no refusal it could branch
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
  with the same cookie and stops one line short of the upgrade: 401 is nobody
  signed in, sends the reader to the sign-in and STOPS the dialling — every
  dial until somebody signs in is the same 401, two console errors each on a
  sign-in page left open — until a sign-in in this tab re-dials, or the tab
  coming back dials once, which is what notices a sign-in made in another tab
  (it gives this one the cookie too); 426 means the session was accepted; a
  `403 second_factor_enrolment_required` stops the dialling and sends them to
  the enrolment. The loop otherwise keeps dialling on its backoff — except that
  a `503` the engine wrote (a node that cannot read its identity
  estate yet) times the next dial by its `Retry-After`, sooner or later than
  the backoff would have, bounded like every hint. A `503` with no
  `Retry-After`, and one something in front of the engine wrote, keep the
  backoff rather than stopping the loop: a dial may reach another node, which
  is what a zero says to do, and the loop is the tab's only way back.
- **A frame arrives already encoded, and two tabs in the same posture get the
  byte-identical one.** The engine marshals a push once per POSTURE present on
  the node, not once per connection: what a client receives is decided by the
  posture its socket is being served in (`live` or `degraded`) and by the kind's
  own route (every client, one seat's watchers, or the one client that asked).
  A tab therefore cannot see a frame differ from another tab's in the same
  posture — which is the property the transcript, the activity feed and the
  agent overlays are all compared against.
- **A degraded node's socket stays open, and says so.** When this node's own
  posture is `shed` or `stuck` its copy of the company is wrong rather than
  behind, so pushes stop, queries come back `unavailable`, and the `ping`/`pong`
  keepalive and the 5-second `health` frame carry on. The page must render that
  as a stated condition rather than as a stall: a socket that closed instead
  would reconnect on a backoff, learn nothing from any attempt, and show
  "retrying" for as long as the node stayed degraded. `unavailable` is never
  flattened into an empty result — "this company has no work" is something a
  reader acts on.
- **Three close codes, and what each one asks of the reader.** A refused
  handshake cannot carry one (see the HTTP diagnosis above); these are for a
  socket that is already open, which the engine decides again — against the
  credential it was opened with — whenever an identity record names that
  credential, a company is published, or the credential reaches its own end.
  `4401` means that credential no longer
  resolves to anybody — the session ended or was revoked — so once this tab's
  own requests have settled, ask `GET /auth/session` once: a `401` sends the
  reader to sign in with nothing dialled — dialled blind, a session ended
  elsewhere was a refused handshake, its probe and a refused snapshot, three
  `401`s where one says it — and any other answer re-dials with the cookie the
  browser holds now. `4403` means the person resolves and may not have this surface —
  their seat is gone: the socket STOPS (no
  reconnect, no REST fallback, both of which the same decision would refuse)
  and every screen but the Account, which reads no socket, is replaced by one
  panel saying who they are (`GET /auth/session`, asked again on the refusal),
  what they hold and why, with the *Account*, a *Try again* for after an
  administrator has acted, and a
  *Sign out* — the person refused is the person who needs to leave, and the
  engine keeps `/auth/` open to their session for exactly that. Drawn as
  they were, the screens said the engine was "not connected" and showed
  figures that never resolved, and the sidebar's foot, which names the reader
  from the same session while the viewer question goes unanswered, said
  "Checking who you are" for good. A handshake refused `403` is the same state, read through the
  plain-HTTP re-ask. `1013` — the standard's *try again later* — means the
  engine will not vouch for the credential on this socket now (it could not
  read what decides it, an identity move named nobody, or the person's seat
  answers to another handle): reconnect on a backoff, and let the handshake
  decide and say why (`503 identity_unavailable` while the node cannot read its
  identity estate). Nothing else closes this socket for a fault. And the
  socket never moves the session's cookie — only a REST response can — so a tab
  that would otherwise sit on the socket alone makes an ordinary REST read now
  and then, or its session idles out under it however busy the screen is.
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
- **What a control did is said, in one region the application mounts.** The
  package's list, tag and notice controls announce every add, remove and move
  through a module-level `announce()`, which says nothing unless an
  `<Announcer>` is mounted — and this application mounted none, so the org
  builder's editor said nothing to a screen reader and warned in the console
  instead. `app/announcer.tsx` mounts exactly one, because the package keeps
  one sentence for the page and a second region would repeat it; beside the
  frame rather than in the shell, so a sign-in does not tear it down; named
  Announcements rather than the package's default, which is also what the
  toast outlet calls its polite region; and portalled into the fullscreen
  element while there is one. The shell once mounted a second, under the
  default name, and every add, move and restore was said twice; a case in
  `app/announcer.test.tsx` now counts the regions that hold a sentence, by
  what they hold rather than by name.
- **One stack decides which surface a key belongs to.** Dialogs, sheets,
  menus and listbox popups register on the design system's layer stack
  (`useModalLayer`, `usePopupLayer`), in the order they opened, and so do the
  app's step-up confirmation, the shell's command palette and the narrow
  layout's sidebar drawer: no modal hand-rolls its veil or listens for Escape
  beside the stack. That drawer is the sidebar itself below the kit's shell
  breakpoint (1024px) — the kit's `AppShell` owns it — a dialog only while it
  is open; closed, it is out of the tab order rather than only slid away, and
  it closes on a route change (the shell hands the route to the kit as its
  `navigationKey`). Only the topmost
  surface handles Escape or a press outside, so a prompt over the node editor
  closes on its own Escape and leaves the editor open, a step-up confirmation
  raised by a refused request over that editor does the same, and an open menu
  closes before the dialog it sits in. A modal traps Tab, moves focus in when
  it opens (honouring a field's `autoFocus`) and returns it to whatever opened
  it. When that control has gone with a modal that closed as this one opened
  (a menu entry handing over to the dialog it opens), focus goes back
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
  The same stack decides what PAINTS above what, and it can only do that
  inside one stacking context: a surface's z-index is its depth on the stack,
  and a z-index orders nothing outside the context it is set in. So every
  overlay is portalled to the page's body, and the only `LayerHost` is a
  FULLSCREEN container's (the org builder's), never one around the frame. The
  drawer is why: it is the sidebar itself, painted in place one step above its
  own depth, and an application-wide host is a stacking context at the bottom
  of the band — every popover, dialog and palette inside it was capped beneath
  the open drawer whatever its depth, so on a phone the Account popover opened
  from the drawer, and the palette opened from its search, drew under it and
  could not be tapped. With one context, the palette opened from the drawer is
  above it and owns the keys; taking one of its rows navigates, and the drawer
  closes on the route change; Escape closes the palette alone and returns focus
  to the drawer's search.
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
  sign". The command palette's own hints use it too. **A keycap is a hint for a
  keyboard, so the frame draws none where the reader's primary input is
  touch** (`TOUCH_MEDIA` in `app/layout.ts`, no hover and a coarse pointer,
  read through `lib/media.ts`'s `useKeyHints`): the drawer's search drew
  "Ctrl + K" on a phone, a key nobody there has, and the keycap cut the
  trigger's own label to "Ask or jump t…"; the palette's key legend and its
  rows' chords go the same way. It asks "is this touch?" rather than "is there
  a keyboard?", because no media feature can see a keyboard, and a browser
  that answers neither keeps its hints. The keys still work for a reader who
  attaches one.
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
check, the save, the live region, the shortcuts, the selection and the dialog
that is open. Its views and dialogs are handed in (`BuilderSurfaces`, bound
in `routes/org/builder/surfaces.ts`) and reach all of it through
`BuilderContext`, so no view or dialog starts a request or touches storage.
Every surface is required: a Builder suite stands a view in with a fake,
while the screen binds the real chart, table, editor and dialogs, and
`surfaces.test.tsx` mounts the builder with exactly those.

- **The posture is what the engine answers.** `GET /config` is read on
  mount and whenever the reader changes, and its answer decides edit mode,
  create mode, a node that has not caught up, a request to sign in, a process
  that does not serve the configuration, or an unreachable engine
  ([the guide](../guides/org-builder.md#opening-the-builder) has the table).
  Being signed in is never the test: it says nothing about whether this
  person's grants reach the company, and the only thing that can answer is
  the engine. The reader changes when the viewer's login does — another tab
  signing in as somebody else, because the cookie is the browser's — and a
  change mid-edit keeps the draft on screen, forgets the kept copy and checks
  it again.
- **A managed document opens read-only.** When the viewer says another
  system [manages the
  document](../concepts/configuration.md#managed-configuration) and the
  reader's credential is not one of its writers (`useManagedConfig()`), the
  builder draws the organization with every editing control disabled — a
  lead's own team included — and an info callout carrying the managed
  sentence, and collects no draft for a save the engine would refuse. If a
  check is answered `403 config_managed` before the viewer says so, the
  toolbar's check status is **Managed by another system** and the check's own
  `managed_by` decides the same way. A writer's token edits as it would on a
  document nobody manages.
- **Every draft is a dry run of the write a save would send.** The same
  `PATCH` with `If-Match` (or `PUT` with `If-None-Match: *` in create mode),
  plus `dry_run=true` and without the audit summary — a draft that changes
  nothing included, a lead's unit too, since the engine refuses a lead only
  the WRITE of an unchanged unit. A draft that changes moves its generation,
  and an answer for an older generation is dropped.
- **A lead edits one unit as a document of its own.** A reader without
  Admin who leads a unit (`lib/leadScope.ts`) is opened on it
  rather than refused: `model/scope.ts` reads `GET /config/units/{key}` as
  `{name, units: [that unit]}`, and the request is that unit's —
  `PUT /config/units/{key}` with `If-Match`, the unit whole — so nothing else
  in the model knows it is a lead's. The engine answers about the WHOLE
  company, so its paths are moved into the draft's coordinates by where its
  derivation places the unit, and a problem outside the unit is said about
  the draft rather than put on whichever node sits at that path. What reaches
  outside the unit (the company's name, the top level, the unit's own lead and
  place, and on every node the keys another system finds it by: a project, a
  space, a channel, an email, a contact identity) is refused at the
  reducer's recording door and drawn disabled with the reason, and a refusal the engine names part by part (`403` with
  `refused`) is a problem on each part, not a refusal of the person. Until
  the viewer has answered, and what a reader who might lead leads is known,
  nothing is read, so a lead is never shown a refusal of the company and
  their kept draft is never forgotten over one. A lead of several units edits
  one per draft, chosen in the toolbar, and a kept draft is offered only for
  the unit it was made of. Their lost save is settled by reading the unit
  back, and their update stands on the unit the node serves: the revision
  history is admin's.
- **The canvas is handed the chart it draws.** `chart=structure|reporting` is
  the Builder's own section param, chosen in its toolbar, so the canvas is
  given the answer as a prop rather than reading the URL a second time.
- **The visualization fills the screen.** With `view=visualization` the
  builder asks the shell for the window's height (`useFillScreen`), and it and
  the tab panel it sits in become flex columns of definite height, so the
  canvas takes what is left under the toolbar and the shell's scroller has
  nothing to scroll. The table view withdraws the request, and so does the
  posture screen the builder draws before the engine has answered.
- **One drawing with the live chart.** The nodes, their marks, the agent hue
  and the branches are the live org chart's (`ui/orgNodes.tsx`), so the chart
  a reader edits and the one they watch are one drawing. The branches are the
  design system's elbows, drawn at the weight the design system gives a chart
  of NODES, twice a card chart's, because a node is half a card's height and
  the branch keeps its ratio to what it joins; the builder sets no stroke of
  its own.
- **Fullscreen takes the builder container**, never the canvas: the toolbar,
  the view, the dialog host, a toast outlet of its own and the live region all
  render inside it, because a fullscreen element renders only its subtree.
  The application's announcer, which the editor's lists and pickers speak
  into, is portalled in beside them for as long as the container is
  fullscreen.
  The control is not drawn where the Fullscreen API is missing. The sign-in
  is a screen outside the frame, so asking to sign in leaves fullscreen first.
- **The selection is in the URL, by address, and the toolbar mirrors it.**
  `unit=` names the selected unit by its key and `seat=` the selected seat by
  its handle — never a name, which two units may share; a link naming one
  selects it, a new address rewrites it, and a removed node clears it. **The address the
  builder was MOUNTED on is also a request**: `seat=` or `unit=` opens that
  node's editor and `add=unit|agent|human` opens the Add (under `unit=`, or at
  the top level), once, as soon as the builder can edit — loaded, in edit
  mode and not paused — and `add=` then leaves the
  address. Only the arrival does it, because the builder writes `seat=` and
  `unit=` on every selection and a Back restores an older one; a reader who
  selects another node first has spent the link (`editWiring.test.tsx`). The
  toolbar carries the selected node's own actions, because a canvas tree item
  may contain no tab stops of its own, and they are the card's and the row's
  own list (`nodeActions.nodeMenu`) rather than a copy: the same entries,
  order, names and icons, Edit reports included. Open seat is offered only for
  a seat the saved company has. **The company's node has no Edit**
  (`nodeActions.editsInPlace`): no pencil on its card or row, and Enter and a
  double-click on it do nothing, because its mission, vision and standing
  policies are written on Company › Principles alone and a pencil on the root
  of the chart that left for another page read as editing the node. Once the
  company exists, the toolbar's menu for the selected company offers that page
  (`BuilderApi.principles`, `openPrinciples`), in edit (`?edit=mission`) for a
  reader the page's own gate admits (`useMayWriteCharter`) and named **Edit principles**, or
  plainly and named **Open principles** for one it does not; the card's and the
  row's menus leave it out. The name, the one company field the model writes
  (`model/document.ts`'s `COMPANY_FIELDS`), is **Rename company** on every
  surface's menu, and a company the draft is still creating is named the same
  way. The model refuses a change to
  the principles at its recording door, a kept log holding one is unreadable,
  `buildPatch` never names them, and an engine problem about one is listed at
  document level with **Open Principles**. A node's key is its IDENTITY (a seat's
  handle, a unit's key, both carried by every node the engine stores), so the
  base is keyed as it loads and a rename never moves a key; only a node the
  draft created moves, from the key the draft minted to the handle or key it
  was given, once a save has created it. The reducer lists what moved
  (`state.rekeyed`), and the
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
  configuration again: a plain read replaces the draft whole, so an open
  editor lost its node, typed form and all, and the selection was cleared.
  The update is read only from a node serving the conflict's revision or a
  descendant of it, with the engine's description of it — a lead's from the
  unit the node serves, since a lead reads no history — and
  `UpdateDraftDialog.tsx` shows what still applies, what is dropped and every
  conflict with its three values; confirming waits for a choice on each. A
  value is shown as a person reads it: the model tags a conflict over its own
  structures with their shape, so a node key reads as the node's name, a
  removed node as the fields that changed, and a credential's mask as "A literal value is set (hidden)",
  never as the marker or as JSON.
- **Only the operation log is kept, and nothing is written before it is
  decided.** `useDraftKeeping.ts` reads the kept draft once the base is loaded,
  offers Keep or Discard for the same revision (and, for a lead, the same
  unit; read-only until answered), restores through the update flow for
  another, and discards one kept for the other mode or another unit. A lead's
  builder opens the unit a kept draft was made of while they still lead it
  (`persistence.keptScope`, read before the unit is chosen and deciding
  nothing), so another unit means one they no longer lead. It
  writes nothing before that decision, because the plan for an empty log is to
  clear. A change of reader or a refused credential clears it and withdraws
  an offer — but not a step-up the reader declined, which is the same person
  choosing not to confirm yet — and a sign-out empties the tab's storage
  before it reloads. A draft with changes asks the browser's prompt before
  the tab goes (session storage does not outlive the tab), and one that
  storage cannot keep at all asks before the builder is left, since that loses
  it too; a move within the builder keeps it and asks nothing.
- **A save is the checked write, signed, and never believed blindly.**
  `useSave.ts` sends the model's save request with the audit summary signed by
  a write id minted when the review opens and kept for as long as the draft
  does not change, so a second press after a lost answer carries the same id.
  A lost answer is settled from the revision history before anything else
  happens — a lead's by reading the unit back, since the history is
  admin's — and while it is unknown the builder records nothing. A
  write in flight is never aborted, and a save that lands after the builder
  was left still records the revision and clears the kept draft. The kept log
  is marked with the write id before the save is sent and unmarked once the
  save is known not to have landed, so an answer lost after the builder was
  left, or across a reload, is settled on the next visit (`useSave.resume`)
  before the kept log is offered: replayed onto its own revision it would
  apply every change twice. A save this page still has out when the builder
  opens again is waited for first, because the engine may not have stored it
  yet and settling it then would read as not landed. Such a save is not the
  draft on screen, so the revision it stored is read like any newer revision
  rather than made the base. A save of the draft on screen makes that draft
  the base, keyed by the save's answer, and the stored document is then read
  back, never over an edit made while that read is out: such an edit already
  stands on the saved revision.
  `ReviewSaveDialog.tsx` states every change and consequence and gates the
  irreversible ones on an acknowledgement, whose sentences (`dialogParts`'s
  `ACKNOWLEDGEMENT_TEXT`) the editor's company rename shares.
- **Create mode is a form, one template operation, and a create-only write.**
  `CreateCompany.tsx` collects the company's name, the starting shape and a seat for
  the operator — **Add a seat for yourself**, ticked from the start and named
  "Founder", because every person holds a human seat and is invited or
  created onto one from the org chart, so a company started without one is a
  company nobody, the operator included, can join until somebody adds one;
  unticking it is a deliberate answer (an agents-only company run through API
  tokens) — and records the model's `applyTemplate` (refused
  outside create mode), so the start is a single undo and is checked like any
  other draft. The save is `PUT` with `If-None-Match: *`; a company that
  appeared meanwhile is offered instead, and the draft is discarded rather
  than replayed onto it.
- **A save is not an apply, and the screen says so.** `AfterSaveStrip.tsx`
  keeps `{revision_id, epoch}` in a tab-lived store outside any screen
  (`savedRevision.ts`), reads this node's `applied_epoch` and
  `config_attempt` off the health push (which carries both every five
  seconds, so there is nothing to poll) and the
  `fleet` query on `recheck.ts`'s cadence, and resolves to Applied, Applied on
  N of M nodes, the node that refused it with a link to Settings › Nodes, or
  "Revision … has been activated since". Each node's fleet row is judged by
  the rule a lead's strip judges this node's push by
  (`lib/nodeStanding.ts`, `fleetNodeStanding` beside `nodeStanding`, one
  predicate over the facts both wires carry): the row's
  `config_applied_epoch` is the push's `applied_epoch`, and its
  `config_epoch`, `config_revision_id` and `config_status` are the push's
  `config_attempt`. So a node counts as having applied the save only on
  evidence — its attempt at that epoch on that revision went ok, or it has
  applied that epoch or a later one (a node that applied 5 and refused 6 has
  applied a save at 4), or it runs that revision
  (`config_serving_revision_id`) — and one that attempted a later epoch
  without applying the save's has PASSED it (two activations inside one
  reconcile tick are never attempted one by one) and is neither applied nor
  waited on: once every node has applied or passed it and one passed it, the
  strip settles on "activated since", rather than calling Applied a save no
  node may run. An Admin and a lead are told the same of the same node, and
  the row's one fact more is the revision a node runs: one running the saved
  revision has applied it. A
  report at the same epoch on another revision — one number names two once a
  rebuilt coordination store restarts the count — is that revision's, neither
  this save applied nor refused. A save whose
  answer was lost carries no epoch: it takes the fleet activation's only
  while that names the saved revision, never `target_epoch`, which is the
  newest activation's whatever it names (and 0 for none). An activation
  naming another revision settles the strip on "Revision … has been
  activated since" — said as since, not as a replacement, because the
  history walk that found the save finds it as readily beneath a revision
  built on it — and no activation keeps it applying and asking. A reader
  without Admin, who reads no fleet, follows a save by this node's push
  alone (`lib/nodeStanding.ts`, the rule a ceiling's mark and a charter
  card follow their own writes by too): the last attempt first — applied when
  it names that revision at that epoch and went ok, refused when it failed
  there, since the applied epoch cannot say it (a failed attempt leaves it
  where it was) and, from a pointer before a rebuilt coordination store, can
  stand above the save's — then applied once the applied epoch reaches the
  save's, or passed once it attempted a later epoch first; and a save whose
  answer was lost takes its epoch from that
  attempt only while it names the saved revision, and is otherwise told it
  cannot be followed from here rather than that it is applying, which nothing
  would ever correct. An epoch an answer supplied for such a save is pinned
  to it in the store (`pinSavedEpoch`), so the next activation, which moves
  this node's attempt and the fleet's activation on, does not turn an applied
  or refused save into one that cannot be followed. What neither wire can tell
  is a node holding an applied epoch from the previous pointer above the
  save's that has not attempted the save yet: it reads as applied, on the
  push and the row alike, since an epoch orders activations within one
  pointer only, and telling the two apart takes the applied activation whole,
  which [#322](https://github.com/crewlet/crewlet/issues/322) has the engine
  keep. Nor does either wire keep the applied epoch across a RESTART: the
  push's `applied_epoch` and the row's `config_applied_epoch` start again at
  0 with the process, and move only on the node's next apply that goes ok,
  while a node with the `data` role boots on the revision it applied before
  (one without it boots on nothing, runs no company, and has truly passed the
  save until it applies one). Until it applies
  again, a restarted node that refused a later activation reads as having
  passed the save — on its push alone when it booted on the saved revision,
  which its row says it runs, and on both when it booted on a revision
  activated after the save — so a verdict said before the restart can turn:
  the strip's "Applied." into "activated since" (a lead's on the next push,
  an Admin's on the fleet read a reconnect makes), and the org chart's note,
  gone once the node applied the save, back as "did not apply". Neither wire
  carries the epoch of the activation a restarted node booted on, so the
  dashboard cannot tell this node from one that never ran the save; carrying
  that epoch across a restart belongs with #322 too, an activation's identity
  across pointer lifetimes. Every follower reads this node's LAST push
  (`lib/useLastHealth.ts`), never the store's live health slice: a dropped
  socket clears that, and is no news of the save — read as a standing, it
  turned the strip's verdict and the chart's note back into applying for as
  long as the socket was down. The TAB keeps that push
  (`StoreState.lastHealth`, written with every frame and left by a drop),
  not each follower: the strip mounts afresh on every return to the Builder
  and the note on the chart, Teams and a unit's page, and a follower that
  kept its own had nothing to read when it mounted during a drop, and said
  the save was applying. The org chart's `PreviousRevisionNote` reads the same
  standing, and learns a lost save's epoch from this node's attempt itself,
  so a node that refused the save says so rather than "still
  applying" for good. The Builder's toast says the same of the same save: one settled
  beneath a later revision (`Landed.activeRevisionId`) names that revision as
  the one active now, rather than saying the save is being applied.
  Its View changes opens the saved revision against its parent
  (`against=`), since against the active revision a save that is active now
  differs from nothing; the conflict banner's Show what changed is the newer
  revision against the draft's base for the same reason. Both, and Copy as
  YAML, read the company document, so none is offered to a reader without
  Admin. Until this node applies it, the org chart and Teams carry a
  note that they draw the previous revision.
- **The status is the last answer about the current draft**, whoever asked:
  a save's refusal is placed on the nodes like a check's, and the check
  machine decides the status only while a check is out or before any answer.
- **A read-only builder records nothing.** The guarded posture, a conflict,
  a kept draft waiting for Keep or Discard, and a save whose outcome is
  unknown or that this page still has out all refuse operations at the one
  door every view goes through, and say why in the live region; so does a
  lead's draft, for each operation that reaches outside its unit. The actions
  themselves are DISABLED rather than hidden, so a reader without
  Admin still reads what the builder does; Edit and Open seat change
  no draft and stay available. A refused check or save is worded by the
  refusal's code, in the sentence every writer of the company document uses
  (`configGuardedReason`): only access the reader lacks says "Needs Admin
  access", and a step-up they declined — a configuration write asks
  for one, so it is the refusal an hour-old session meets — says so and
  offers Check again, which sends the check again and so asks them to confirm
  who they are again.

### The org builder draws the engine's organization

Org chart › Edit org shows the draft two ways, and both
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
  value the engine has not given yet reads "Lead after the check" or "Manager
  after the check", never that a check is running: whether one is on its way,
  or the engine cannot be reached at all, is the toolbar's check status to
  say, and a card cannot know. A seat's handle and a unit's key are written in
  the document — every stored seat and unit carries the ones the engine
  minted, and a node this draft adds carries the one typed into its form — so
  nothing waits on a check to name one, and the editor, the dialogs and
  `chartModel.ts` read the same address. The reporting chart, drawn from the
  last check while the draft has moved past it, says so in a note over the
  canvas.
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
- **The node editor is a sheet of tabs, and the head names the node only.**
  A seat's form ran to a dozen sections in one column, every group under a
  rule and every field with a help line and an info icon. It is now the
  kit's 560px sheet (`Modal variant="sheet" flush`) with a 72px head (the
  node's mark, its live name, then the handle and kind or the unit's type,
  never where it reports: that is the **Work** tab's), a sticky band of
  underline `Tabs`, and one panel at a time, laid out to fixed sizes (a 20px
  inset with 32px below, 24px between groups, 16px between fields, 30px
  controls, 32px fact rows). The panel is the active tab's `tabpanel`; the
  form's state is the editor's, so a tab that unmounts loses nothing. Each
  tab carries one `TabsMarker`, the most urgent of a problem count, a warning
  and a change, mapped from a problem's path or an edit part's path by the
  longest tab prefix, and a field that blocks Apply counts as a problem on
  its tab. Help lines show on focus (`helperOnFocus`), and what the builder
  shows and does not edit is a `FactList` row with a lock whose reason is a
  hover tooltip and is read inline by a screen reader, with no info icons.
  A connected tool is a card with its `VendorMark` and the `integrations`
  roll-up's own label and tone (`lib/toolState.ts`, shared with the
  Integrations screen), so a tool has one status word everywhere; a seat's
  own app state is a row in the card. The tools nobody connected are one
  line. A seat's kind is a locked fact: there is no kind-change operation,
  because the kinds share almost no fields, so the editor ends in **Delete**.
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
- **An agent seat is toned, and nothing else is.** This is the console org
  chart's drawing and the colour rule's second, bounded exception (see "The one
  rule"), and the live org chart's: every agent seat takes the colour its
  profile icon names (purple until its author chooses), on its card, the
  branch into it and its table row, and every other node is neutral. No hue is
  made from a seat (one hashed from its key, stated in its editor as a
  "Colour" fact, was removed: a legend nobody could decode); the editor offers
  an agent's character and colour in one `AvatarPicker` instead, in its head
  and under **Profile icon**, and a person's and a unit's editor offers
  neither. A seat leads with
  the console chart's mark rather than a badge: the agent's own character for
  an agent, a person's glyph in a dashed ring for a person (drawn alone in a
  table row). A
  dashed edge on a node means only "a place nothing fills yet": the ghost of a
  node being added, and a unit's lead slot while no lead is set. A reference
  that names nothing takes the caution tone, and the Datadog fallback seat
  carries a neutral mark while Datadog is enabled, the only time the engine
  routes an alert to it.
- **One line round every box.** A card is framed once: the selected node's
  card takes the accent on its own boundary rather than a ring inside it, its
  focus ring is drawn over that boundary, a toned card has no halo, and a set
  lead is a raised pill with no frame of its own. The chart stands on the
  same dotted field as the live org chart.
- **A click on nothing puts the selection down**, on the chart's field, round
  the table or on the page under either (`ui/clickOnNothing.ts`). A click on a
  node, a row or a control is that thing's own, and a pan keeps the selection.
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
  navigable cells: Name (the kind or type under it), Handle or key, Lead or
  reports to, Problems and the row's actions. A row's keys are a card's keys; Right opens a row or
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

1. **Colour is state, never a derived identity.** No hash-to-hue, no tint the
   dashboard picks for an agent, no per-category chip colour. If you need to
   tell two things apart, use their names. The third-party app marks in
   `@crewlethq/icons`, the org charts' toned agent nodes (Company › Org chart
   and Edit org), an agent's own profile icon, a Crewlet character in the
   colour its author chose, and a QR code's black on white (`@crewlethq/ui`'s
   `QrCode`, whose two inks are the fixed `color.qr` tokens) are
   the four bounded exceptions (see "The one rule" above); nothing else is. The character and colour lists are the
   contract's (`contract/avatar.ts`), held to the engine's and to the design
   system's, and one reading of them (`lib/avatar.ts`'s `avatarOf`) draws a
   part this build does not know as the default part rather than as nothing.
   **A seat's identity badge is `Avatar`, everywhere but the org charts,
   through `SeatAvatar`** (both charts and Edit org's table draw the console
   chart's marks instead, `ui/orgNodes.tsx`'s `seatMark`: see "An agent seat
   is toned, and nothing else is") (`ui/SeatAvatar.tsx`; `seatBadge` for
   a kit component that draws the badge itself), handed the seat's `avatar`
   wherever the chart holds the seat, so an agent is drawn as its own character
   and a person, or a seat the chart does not hold, by initials made from its
   name or handle. For an AGENT a leading
   word "agent" is dropped before the initials are made, because the squircle
   already says it: "Agent CEO" and "Agent CTO" are `CE` and `CT`, where the
   kit alone drew both as `AC`. What a screen reader hears is still the whole
   name. Its `kind`
   is its OUTLINE — a circle for a person, a squircle for an agent — which is
   the one cue telling them apart: who holds a seat is a structural fact and
   therefore a shape rather than a hue. A kind the chart does not hold is
   read from the writer's recorded kind where the screen has one (a write of
   kind `operator` is a person no chart lists), and otherwise takes the kit's default,
   the agent's squircle. A hand-rolled mark holding a robot glyph drew the KIND, which the
   roster gives at a glance, in the slot that should have been saying WHO: the
   same engineer was "FE" on the board and an identical generic robot on
   Search and on a project's Lead panel. `ring="brand"` — the accent drawn as a
   ring round the neutral badge, meaning *selected* — is for the one badge that
   IS the reader, in the sidebar's own account row (unless it wears a `mark`,
   whose notch is cut from the outline the ring would follow), and for nothing else in
   the dashboard (the kit's `CharacterPicker` wears it on the chosen
   character, which is the same word: selected). What a seat is DOING is its
   `status` (`statusOf`), the kit's corner glyph, never a ring and never a
   coloured edge on the card it stands in; a row that draws it says the state
   in words too where it has room.
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
7. **A write says what happened, and is made as you** — the principal your
   session resolves to, through `POST /operator/act`, never as "the
   dashboard". A change to the company's work goes through `act`
   (`protocol/act.ts`) from `useAct` (`lib/useAct.ts`) and nowhere else —
   `app/writeGate.test.tsx` holds that, and renders every write control for an
   anonymous, an unbound and a bound reader — and a change to the company's
   configuration through `protocol/configWrite.ts`. Nothing outside `src/protocol/` reaches the network
   (`protocol/transport.test.ts`). Every write reports its outcome: a toast
   when it landed, a persistent notice with Retry when nobody can say, and the
   engine's refusal beside the control that caused it. A write control is
   never hidden and never optimistic (see "Acting, as yourself"). A button
   whose result is invisible is a button an operator presses twice.
8. **No screen renders a credential it read back.** The setup dialog shows
   the `${VAR}` a field points at, or — for a hand-written literal — an empty
   box whose PLACEHOLDER is dots saying one is held. Never a value, because no
   route returns one, and never dots as the value: a placeholder cannot be
   submitted, and a sentinel that has to be recognised on the way out is one
   an edit can defeat. The one exception is a secret the engine has just
   MINTED for the person looking at it, which exists to be written down: a
   second factor's key at enrolment (with Copy, and drawn as a QR code of its
   `otpauth://` URI beside the link that opens it) and a set of recovery codes
   (with Copy and Download), each shown once, never stored by the page and
   never read back — no route returns either again, and the screen says so. A
   token the reader types on the sign-in screen is the other direction: it is
   sent once and kept nowhere.
9. **A directory line carries no control, and a card one.** Company ›
   Integrations lists every tool and service as one link with its mark, its
   name and its state, because a list is read in one glance; a card on its
   home carries exactly one action (the one fix it owes, or Open), and
   everything else a tool offers is on its page, where the reason for each
   sits next to it.
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
    so Worklet's trash screen drew a 12px em dash in the table's DUE column
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
    ([API endpoints](api-endpoints.md#token-spend-breakdown)). One of the two
    places it can be read on a screen is a record shown **verbatim**, as the engine
    stored it — an event's own payload — because a verbatim view that dropped
    a field would misreport the record, and nothing there is drawn as a
    figure. The other is an artifact sheet's own cells: a number column whose
    schema declares a currency is the author's data, drawn by the design
    system's sheet model in the format the author chose, and never a figure the
    engine recorded, so the sheet draws it as declared. The bundle gate admits
    exactly that construct and the currency signs the sheet strips from typed
    input before parsing a number, each written as the whole construct, so a
    currency format or a sign anywhere else is still refused. Three gates hold
    the rule, each over a different artefact: the
    SOURCE, where `src/money.test.tsx` parses every shipped module — comments
    excluded, so a note explaining the rule can name what it forbids; the
    SCREENS, where the same suite renders the landing screen, Spend, a task, a
    seat's Overview and Turns tabs and a turn over wire fixtures that carry a price everywhere
    the engine puts one, and reads text and attributes back for any trace of
    it; and the BUNDLE, where `TestTheDashboardRendersNoPrice` scans every
    module the engine serves, lazy chunks and `protocol.js` included, because
    the committed bundle is what a browser runs.
