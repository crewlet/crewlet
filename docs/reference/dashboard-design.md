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

Everything else — a seat, a unit, an event category, an integration's state, a
node, a tool origin — is **neutral**, and its identity is carried by its name,
its icon and its position. Those are stable, legible, and do not run out at
eight.

The one exception is a **third-party app's own mark** on the Integrations screen
(`ui/VendorMark.tsx`): Slack's four colours, Atlassian's blue, GitLab's orange,
Datadog's violet, drawn as the third-party app draws them. A mark is identity by
definition, and a recoloured Slack mark is not Slack's. The exception is held
to exactly that: a mark is drawn only beside the third-party app's name,
nothing reads state from it, none of its hues is reused as a token, and the
integration's STATE beside it is carried by the status tone like everything
else. A tool the company has not set up keeps its mark, dimmed.

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

The sidebar's inset is the one place that scale is split in two, because a rail
row has two edges that want different things. `--nav-gutter` insets the rail —
it is where a row's own background, hover and active tint begin, so it decides
how much of the rail's width the click target covers. `--nav-row-pad` insets
the content inside that row. Every glyph in the rail therefore lands on the sum
of the two, and anything with no row of its own — the brand lockup, the group
labels — adds them rather than carrying a literal. That is what lets the rows
be widened without moving one glyph: shrink the gutter, grow the pad by the
same step, and the vertical line the mark, the group labels and the item text
share does not move.

---

## Information architecture

**Two levels, because this product has six unrelated trees.** A **workspace**
is a noun with a tree of its own — Work has projects, Company has units,
Knowledge has containers, Activity has kinds of run, Cost has scopes, Admin has
estates. The 80 px **rail** shows the workspaces and nothing else; the 236 px
**workspace sidebar** shows one workspace's tree and nothing else.

A single sidebar works when there is one tree. With six it either hides every
tree behind disclosure — three clicks to a project — or grows to sixty rows and
stops being scannable. The tracker had already grown a second rail inside its
own screen, which is the same conclusion reached one screen at a time.

### The grammar, stated once and asserted

- A **rail row** is a workspace.
- A **sidebar row** is a destination with its **own path**.
- A **tab** is a `tab=` or `view=` query on the path you are already on.

Nothing is two of those. A tab never appears as a sidebar row, a sidebar row
never appears as a tab, and a filter — a reason, a scope, a status — lives in
the page's own filter bar rather than in the sidebar. This is the boundary
every tool of this shape loses first, and it is lost one pull request at a time:
"just one more row under a project" is what turns two levels into three.

`router.test.ts` holds it against the definitions rather than against this
paragraph: every destination has a path of its own, no two share one, each
resolves to the workspace it declares, no two workspaces own a first segment,
and no reserved segment has the shape of a key the engine mints.

**Reserved segments cannot collide with keys.** Project and container keys are
uppercase (`ENG`), item keys are `KEY-n`, everything else the engine mints is a
uuid — and every reserved segment (`views`, `sprints`, `people`, `units`,
`turns`, `runs`, `schedules`, `a2a`, `events`, `servers`, `revisions`, `fleet`,
`config`, `credentials`, `me`) is lowercase. That is what lets `#/work/views`
resolve before any answer arrives.

### The rail

The order is the product's story: you, the work, the people, what they know,
what they did, what it cost, the machine.

| Row | Route prefix | Badge |
|---|---|---|
| **Inbox** | `#/inbox` | unread notices on the first page, `caution` hue — the only badge in the chrome allowed a status colour |
| **My work** | `#/me` | — |
| **Work** | `#/work`, `#/goals` | — |
| **Company** | `#/company` | — |
| **Knowledge** | `#/knowledge` | — |
| **Activity** | `#/activity` | seats working now, neutral |
| **Cost** | `#/cost` | — |
| **Admin** | `#/admin` | a lock when no operator credential is presented |

**Admin's row is never hidden.** A section that vanishes without a credential is
indistinguishable from one that does not exist, so an operator on a fresh
browser would conclude the product has no configuration screen.

`g` then a letter jumps to a workspace (`g i`, `g m`, `g w`, `g c`, `g k`,
`g a`, `g o`, `g d`); `[` collapses the rail. A chord rather than a modifier,
because every single-modifier combination worth having is already the browser's.

### The routes

| Route | Page | Tabs / views |
|---|---|---|
| `#/` → `#/inbox` | **Inbox** — the landing screen | `band=decisions\|notices` · `state=unread\|all\|snoozed` · `reason=` |
| `#/me` | **My work** — the seven claims, plus what reached you | `handle=` (an operator reading somebody else's day) |
| `#/work` | **All work** | `view=list\|board\|calendar\|timeline` + the filter grammar |
| `#/work/search` | **Search** — the company's work ranked against a phrase | `q=` |
| `#/work/views` · `#/work/views/{id}` | **Saved views** — the inventory, and one view run | |
| `#/work/{KEY}` | **Project** | the same view strip, scoped to the project |
| `#/work/{KEY}/sprints` · `#/work/{KEY}/sprints/{n}` | **Sprints** — figures, velocity, burndown | |
| `#/work/{KEY}-{n}` · `#/work/{id}` | **Item** — description, thread, history, links, properties | `thread=comments\|history\|woke` · `record=` (which change's routing) |
| `#/goals` · `#/goals/{id}` | **Goals** | |
| `#/company` | **Company** — the charter and the chart | `lens=chart\|charter` |
| `#/company/people` | **People** — the one directory, and who is carrying how much | `view=seats\|workload` · `group=state\|unit\|flat` · `q=` |
| `#/company/people/{handle}` | **Seat** — agent or human | agent: overview · model · conversations · memory · cost · access; human: overview · access. `conversation=` opens one thread |
| `#/company/units/{id}` | **Unit** — lead, purpose, goals, seats, sub-units | |
| `#/knowledge` | **Knowledge** — live search over the backend | `q=` |
| `#/knowledge/{CONTAINER}` | **Container** — browse the tree | `kind=prose\|skills\|all` |
| `#/knowledge/{CONTAINER}/{Title}` | **Page** | |
| `#/activity` | **Live now** — what the company is doing at this moment | `window=15m\|1h\|6h` |
| `#/activity/turns` · `#/activity/turns/{id}` | **Turns** — every phase, round by round | |
| `#/activity/runs` · `#/activity/runs/{turn_id}` | **Coding runs** — live and durable | |
| `#/activity/schedules` | **Schedules** | |
| `#/activity/a2a` | **Agent-to-agent** | |
| `#/activity/events` · `#/activity/events/{id}` | **Event log** — the time axis, then the rows | `window=1h\|6h\|1d\|7d\|30d\|<from>/<to>` · `category=` · `actor=` · `q=` · `failed=` |
| `#/cost` | **Spend** — over time, then by phase, model, seat and turn | `window=1d\|7d\|30d\|90d\|<from>/<to>` · `group=phase\|model\|seat\|unit\|worker\|turn` · `compare=previous` |
| `#/cost/budgets` | **Budgets** — caps, the durable counter, what is refused | |
| `#/admin/fleet` · `#/admin/fleet/{node}` | **Infrastructure** — nodes, leases, duties, replication *(operator)* | |
| `#/admin/integrations` · `#/admin/integrations/{kind}` | **Integrations** — the catalogue, and one tool with what has actually been arriving on each of its surfaces *(operator)* | |
| `#/admin/tools` | **Tools** *(operator)* | `q=` · `origin=` |
| `#/admin/config` · `#/admin/config/revisions/{id}` | **Configuration** *(operator)* | `lens=active\|history\|diff` |
| `#/admin/credentials` | **Credentials** — names and provenance, never values *(operator)* | |

**There is no redirect table.** There was one, and it was always a liability: a
redirect whose old path is now a live route sends every reader of that route
somewhere else, permanently, with the address bar agreeing with them — which is
strictly worse than the dead link it exists to avoid, because a dead link is
visible. It happened once, when `#/work` still meant a coding run. No `v*` tag
has ever shipped a route from this tree, so there is nobody holding an old link;
`NotFound` names the screen and offers the palette.

**Three of those surfaces are the engine answering a question it has always
been able to answer.** Search is the ranking a seat gets from `search_work` —
BM25 over the engine's own index — which the operator reading the same company
had no access to at all; the board's `q=` is an escaped substring over an
excerpt and answers something else. The item's **Woke** tab is who one change
actually reached and under which of twenty reasons, which is the fact no
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
set — `15m` `1h` `6h` `1d` `7d` `30d` `90d` — or an explicit interval written as
ISO 8601's own `<from>/<to>`, so a custom window is still one value a reader can
copy out of the address bar.

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
windowed question takes. A chart's edges are rounded **up to the bucket it
draws**, so the hour in progress is on the chart while it is still being spent
and the query changes once per column rather than once per second; a list's are
not. An interval never moves at all — it is the one window that is stable to
link to.

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

**`FacetRail`** is the other half — one dimension of the list, as chips that
narrow it. Two screens had two of these and they disagreed about the one thing
that matters: whether a count is over what is **loaded** or over what **exists**.
That is a required prop now rather than a caption somebody remembers, and it is
said beside the chips. A number next to a chip is read as "how many there are",
and when it is really "how many of the four hundred rows this tab happens to be
holding" the reader is being told something false about their own company.

A facet count is computed with its **own** filter lifted and every other one
applied, because that is the only meaning it can have: a chip says how many rows
choosing it would show. Counted through its own filter, every chip but the
selected one reads zero and the rail is a dead end.

### The four frame-level keys

| Key | Kind | Meaning |
|---|---|---|
| `peek={kind}:{id}` | section | the detail rail is open on that object |
| `tab=` | section | the object page's tab |
| `view=` | section | a list container's view |
| `sort=` `cols=` | filter | the grid's order and its visible columns |

**`peek` is one key, one component, one rule.** A plain click peeks; ⌘-click,
middle-click and the rail's `Open ↗` go to the page. Inside a peek `[` and `]`
step through the list it was opened from and `esc` closes it. Opening the rail
**pushes** — Back closes it, which is what a reader means by Back with a panel
open — and moving it **replaces**, because four objects walked through one open
rail are one place the reader has been, exactly as four ticked chips are one
screen.

The id is split on its FIRST colon only, so `sprint:ENG/3` and
`page:ENG/Deploy runbook` survive being carried in a query value.

### The frame

`dashboard/src/app/frame/` owns everything a screen wears, and a screen renders
none of it:

| Piece | What it is |
|---|---|
| `AppRail` | the workspaces, the badges, the engine pill, theme and density |
| `WorkspaceSidebar` | one workspace's tree, built from LIVE answers rather than a table — a hand-kept copy would be wrong the first time somebody adds a project |
| `PageBar` + `Breadcrumb` | where you are, derived from the route by one function; the last segment is the object and is not a link |
| `StateBar` | the answer's own honesty in one place: degradation, `read_level`, `complete: false`, how far this node has applied |
| `ObjectHeader` + `TabStrip` | an object's eyebrow, title, status and up to six facts, in the same order on the page and in the peek; the strip takes the tabs the object HAS |
| `DetailRail` | the peek, resizable, a drawer under 1180 px |
| `DataGrid` + `cells` | sorting in the URL, bands from a grouped answer, typed cells |
| `PageActions` + `PageNote` | a screen's own controls, portalled into the bar; its one sentence of explanation |

**A screen publishes what the chrome needs and renders none of it.** The labels
the route cannot supply (`usePageLabels`), the coverage of the answer it drew
from (`usePageCoverage`), and its own controls. Twenty screens each drawing
their own header is how five of them came to drop the coverage badge.

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
the moment the reader touches the page.

Both of these shipped wrong once, and neither is visible in a URL.

### The Inbox is the landing screen

A dashboard's home used to be a summary of the company. What a person opening
this actually wants to know is whether anything is waiting on them.

**Two bands, and the split is the engine's.** `work_inbox` reports the
`primary_reasons` that were APPLIED — defaulted from the person's own record —
so a company that has re-decided what counts as primary gets its own split
without the client knowing anything about it. **Decisions** is what is waiting
on somebody; **Notices** is what merely reached them.

**The wake reason opens every row.** The applier records, per change and per
recipient, the ONE reason of twenty under which that person heard about it.
Nothing drew it before, and it is the fact no commercial tracker keeps: Linear,
Jira and ClickUp can all tell you that you were notified, and none can tell you
why. The client carries no copy of the split — that is a property of the person,
and the answer states which one it applied.

**Three viewer states, three sentences.** A reader with no credential, a reader
whose credential no seat claims, and a bound reader. Only the first is anybody's
fault; an unbound token is an ordinary state whose remedy is a line of company
configuration, so the screen names the id to bind rather than reporting a fault.

### The attention queue

`dashboard/src/lib/attention.ts` is one list because it is one question, and it
is the question an operator opens the page with. It renders in the Inbox, above
the notices. Every one of these conditions was already known to the dashboard
and each lived in a different screen:

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

A page's history shows **what a save changed**, not only what one version
said. `lib/diff.ts` is a line diff over the two revisions — Myers, by line,
with no word-level refinement inside a changed line, because these documents
are rewritten in paragraphs and a word diff makes a small edit prettier and a
large one unreadable. Unchanged runs collapse to a row saying how many lines
they stand for: a gap silently closed makes a document edited at both ends
look like one rewritten in the middle. "This save did not change the body" is
its own answer, because a title, a label and a move are saved beside a body
and a pane of unmarked lines reads as one that failed to load.

## The palette has scopes, and remembers

A launcher with one index answers "where do I go", and three questions do not
fit that shape. Each gets a **sigil**, typed in the same box in the same
keystroke:

| Typed | Searches |
|---|---|
| *(nothing)* | screens, seats, units, tools, and any event / trace / turn id pasted out of a log |
| `#` | the company's **work**, ranked — a server query, which is why it cannot be folded into the index above |
| `@` | **people** — seats and units only, so a colleague is not buried under four tools whose names happen to match |
| `>` | **commands** — theme, density, copy link, set token — which have no name to search for at all |

The scopes are named in the palette's footer with the one in use marked: a
sigil nobody is told about is a feature that does not exist.

An empty palette offers **recents** — the last few objects this reader opened,
newest first, per browser. Objects only: anything the rail or a sidebar
already lists is left out, because a recents list repeating the navigation
beside it costs a reader a scan and tells them nothing. The label stored is
the one the **screen** resolved, which lands a render after the route.

A `>` command that would change nothing is not offered: the list omits the
theme and the density already in use, because a control that says "switch to
dark" while the page is already dark does not know what it is looking at.

---

## A cron expression is read, not printed

The schedules screen printed `0 9 * * 1-5` and nothing else. A reader who
knows cron reads it; a founder reads five numbers and a dash on the one screen
that says when the company wakes itself up.

`lib/cron.ts` reads the five fields twice over: a **sentence** beside each
expression in the list ("at 09:00 on weekdays"), and the **next five instants**
on the schedule a reader has opened. The instants matter because the engine
sends one next fire and "every 4 hours" and "at 4am" have the same next fire
for most of the day — it is the fires after the next that say whether an
expression means what its author thought.

Two rules keep it honest:

- **The engine is the authority.** `next_run` on a row is the engine's own
  computation and is what the screen shows as *Next*; this is a reading aid
  beside it, never a second source for the same fact. A schedule that names a
  timezone says so under the list, because the engine evaluates it in that
  zone and this reads it in UTC.
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

## Controls that mean what they look like

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

Four more controls that looked like something they were not:

- **A copy button says whether it copied.** The clipboard is invisible, so a
  control that writes to it and reports nothing is indistinguishable from a
  dead one — and this one *was* sometimes dead: the Clipboard API is gated on
  a secure context, so `navigator.clipboard` is simply undefined at the
  `http://<node-ip>:8000` anyone reads the dashboard of a machine that is not
  their laptop at. `navigator.clipboard?.writeText(x)` swallowed that. The
  `CopyButton` primitive falls back to the deprecated `execCommand` path,
  which is the only one that works there, and then says `Copied` or
  `Copy failed` — announced as well as drawn.
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
  hovering. `.t-link` is the caption register that keeps `--accent-ink`,
  which the palette suite already measures.
- **Select-all is a local verb on a record.** ⌘A / Ctrl+A is a *document*
  gesture, so on a screen whose point is one JSON record — a turn's record,
  the active configuration — it took the nav, the stat row and every phase
  card along with it. A `Code selectable` block is focusable and owns the
  chord while it holds focus; everywhere else the browser keeps it. The
  focus ring is not decoration: a keyboard verb that changes meaning on
  click is a secret without one.

Three more that announced as something they were not. Each of these is a
control a sighted reader could use and a keyboard or screen-reader one could
not, which is the class of defect that never shows up in a screenshot:

- **A segmented control is a radio group, not a tab list.** `Segmented`
  declared `role="tablist"` with `role="tab"` children, and not one of its
  eight call sites is a tab widget: they pick a theme, a density, a grouping,
  a scope, a window and a lens. A tab controls a `tabpanel` it is adjacent to
  and labels; these narrow or regroup what is already on screen, and several
  sit in a screen head with the content they affect hundreds of pixels below.
  It is `role="radiogroup"` with `aria-checked` options now. `Tabs` keeps the
  tab role — it is the one control here whose options sit directly above the
  panel each of them shows.
- **A group of choices is ONE tab stop, with arrow keys inside it.** Both
  controls rendered plain buttons under the old role, so the ARIA promised
  one stop and arrow-key movement while the DOM delivered N stops and no
  arrow keys — neither behaviour, rather than one or the other. The shell's
  theme and density controls alone put six stops in front of the page on
  every screen. One shared roving-focus hook gives both of them the contract
  their role implies: arrows (both axes) and Home/End move within the group,
  `tabIndex` 0 travels with them, and every other key — Tab above all — is
  left to the browser.
- **A group always keeps one tab stop, whatever the URL says.** `value` comes
  off the query string at seven of the nine call sites, so a link from an
  older build, a typo or a renamed option reaches the control as a value no
  option carries. The roving stop fell back only when the *held* option left
  the set, not when `value` was outside it — so `?lens=bogus` gave every
  option `tabIndex="-1"` and the whole control dropped out of the page's tab
  order, unreachable by keyboard and strictly worse than the plain buttons it
  replaced. It falls back to the first option now; nothing is checked, so
  nothing else has a claim to the stop.
- **The arrows move focus; Enter and Space choose.** That is manual
  activation, and it is the default on both controls because of what they are
  wired to: seven of the nine groups on the dashboard drive a `useParam`,
  five of those push a history entry, and every one of them re-runs its
  screen's query — which the socket mints fresh, with no cache, no dedupe and
  no coalescing behind it. Under selection-follows-focus, one reader arrowing
  across the five options of a group to hear what is there is four queries
  nobody asked for and four history entries they then have to press Back
  through, and the reader most likely to arrow across every option is the one
  using a screen reader. That is the trade the pattern names by its own
  terms: selection follows focus only while the result is displayed without
  noticeable latency and is not costly to undo, and a query behind a pushed
  history entry satisfies neither clause. Two groups do satisfy both — the
  shell's theme and density, which write `localStorage` and a `data-`
  attribute — and those pass `activate="automatic"`, where selection
  following focus is the better control and there is nothing to undo.
  Nothing in the hook handles Enter or Space: every option is a real
  `<button>`, so the browser's own activation fires the click the group
  already listens for, and a second handler would commit one keypress twice.
- **The tab stop is under the reader's feet, not on the selection.** The two
  stop being the same question is what ends when arrows stop selecting: a
  reader stands on an option they have not chosen for as long as they are
  still deciding, and a stop left behind on the checked option means tabbing
  out and back drops them somewhere they did not leave. A change from
  outside the group — a click elsewhere, the browser's Back button, a pasted
  URL — retires whatever the arrows were pointing at and takes the stop back,
  and so does an option disappearing from `options`, which would otherwise
  leave the group carrying no tab stop at all and reachable by no key.
- **A tab list without a panel is a row of buttons wearing the role.** `Tabs`
  declared `role="tablist"` and `role="tab"` and stopped there: no rendered
  element carried `role="tabpanel"`, nothing was referenced by
  `aria-controls`, and the switched content was an ordinary run of siblings
  after the strip. A screen reader could find the tabs and then had no way to
  reach what the selected one controlled — pressing Tab from a freshly chosen
  tab left the widget and landed on whatever came next in the DOM, so choosing
  a tab moved the reader *further* from the content they had just chosen. The
  panel is part of the component now: pass the content as `children` and both
  ids, the `aria-controls` and the `aria-labelledby` back-reference are minted
  with `useId` here. A documented id convention would have been a convention
  each caller could follow halfway, and a half-wired widget looks exactly like
  a whole one. `aria-controls` sits on the *selected* tab only, because only
  its panel is rendered — an id that resolves to nothing offers a reader a
  jump that goes nowhere. The panel takes a tab stop for reach rather than for
  interaction, so its focus ring is suppressed while it stays reachable, and
  `.tabpanel` carries its own flex column and gap: the sections it now wraps
  used to take their spacing from the screen's own column, and that is the one
  part of this change no rendered test can see.
- **A manual group owes the reader a sentence.** A radio group's learned
  contract is that the arrows choose — native radios do, and the authoring
  practices describe no manual variant of the pattern. The deviation here is
  deliberate and every announcement along the way is honest: a reader arrowing
  onto an option hears it is not checked, which is true. What they were never
  told is *which key would check it*, so a reader who pressed Right, heard
  "not checked" and moved on took the silence for a control that ignored them.
  The group carries an `aria-describedby` note saying the arrows move and
  Enter or Space chooses, announced on entry, and only where the arrows do not
  already choose. A description rather than a different role: the alternatives
  that would make the arrows conform — a toolbar of `aria-pressed` buttons, a
  plain group with `aria-current` — each drop either the mutual exclusivity
  that says these are one choice or the "2 of 3" that says how many there are.
  Losing a true semantic to gain a convention is the wrong trade when a
  sentence closes the gap.
- **A meter needs a name, and a value inside its own range.** `role="meter"`
  with no accessible name announces as a bare number on screens that render
  several, and the visible legend is not the name: two call sites pass none
  at all, and the ones that do pass a reading ("94% of the meter used")
  rather than a noun. `ariaLabel` is a required prop for that reason, as it
  already is on `Segmented`, `Tabs` and `Select`. The bar's fill was clamped
  and `aria-valuenow` was not, so a budget *lowered* under a counter that has
  already spent past it — the exact state an operator opens the screen in —
  published a value above `aria-valuemax`; it is clamped now, with the true
  figures in `aria-valuetext` so the overage is reported rather than hidden.
  A meter with no ceiling drops the role entirely instead of announcing "0 of
  100", which is a confident claim that nothing has been spent where the
  truth is that nobody has said what the limit is.

And one that was only reachable by mouse. `.code` is `overflow: auto` under a
460px cap, so **any** block taller than that is a scroll container — and in
Chrome and Safari a scroll container is reachable by keyboard only if
something makes it focusable. The `selectable` blocks were, because ⌘A needed
it; the rest were not, and they are the tall ones: a phase card's verbatim
system prompt runs to tens of kilobytes and could not be scrolled from the
keyboard at all. `Code` measures its own box — both axes, since `plain` sets
`white-space: pre` and scrolls sideways — and the blocks that actually
overflow become named `region`s with a tab stop. A stop on every block would
put one in front of each of a round's tool arguments, so it is measured rather
than assumed, and `label` is required on every block because taking focus
without a name is the other half of the same trade.

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
`agents` is pushed twice per round.

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

2. **Weight is meaning.** `reflection_completed` is a sentinel whose own
   payload doc says it deliberately carries no outcome; a guard breach is a
   turn the engine stopped. As two identical feed rows an operator scanning
   for the second reads past it. So the rows are grouped by the question they
   answer — **What went wrong** (above the phases, absent on a healthy turn),
   **What the turn was given**, **What else it did**, **What it left behind**
   — and anything this build has no opinion about falls through to a residual
   list rather than being dropped. The event registry is additive-only; a type
   a newer node publishes has to still render.

   A band is not a rendering, though, and *What the turn was given* was
   rendering half of its own. `prompt.size` — six integers per phase, which
   exist so prompt-slimming progress is measurable rather than argued about —
   was banded here and then read by nobody: the panel took `prefetch_summary`
   out of the band and dropped the rest, so the only route to a phase's prompt
   size was the raw payload of a row in the residual list. The panel carries
   both halves now, which is the pair that says whether a heavy prompt is
   heavy *because* of what was prefetched or in spite of it. Per phase and per
   round, never summed: a prompt is re-sent on every round of the tool loop,
   so a total would be neither the turn's input bill — the tiles above already
   report that — nor any single thing that was ever sent.

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

**A qualifier follows what it qualifies.** Moving that chip onto the trigger
line put it in FRONT of the text, which is its own mistake: the eye landed on
a label before the sentence it labels, and the one thing worth reading on the
row — what actually woke the turn — was pushed to second place. The subject
comes first and the source follows it, with the flex sizing deciding who gives
way on a narrow card: the message truncates, the source stays whole.

### The document does not scroll

`.screen` scrolls, and it is the only thing that may. The sidebar is a fixed
rail beside a scrolling pane, so a page that can *also* scroll as a whole
carries that rail off the top of the window and leaves the reader looking at
background below the app — with two scrollbars, neither obviously the one they
want.

`body { min-height: 100dvh }` only asked the body to be at least a viewport
tall. It still permitted it to grow, so the invariant held because nothing
happened to exceed it rather than because anything enforced it. `height:
100dvh` with `overflow: hidden` makes it unreachable instead of merely unused,
and costs nothing: `.app` is already exactly that height. Verified by driving
the built bundle in a browser — with 6000px of injected content and an
explicit `window.scrollTo(0, 5000)`, `window.scrollY` stays 0 and the rail
stays at the top, at every viewport from 600×900 to 1854×890.

### A panel has one left edge

`.panel-body.tight` reduced the horizontal padding as well as the vertical
one, so a tight panel's content sat on a different vertical line from its own
heading — visibly closer to the border than the title above it — and a code
block inside one was pushed hard against the panel's right edge with nowhere
for its scrollbar. What `tight` is for is a panel whose rows carry their own
vertical rhythm (a stack of cards, a footer strip), and that is a claim about
height. It is vertical-only now, on the `--space-4` inset the head sets, and
all five tight panels in the product align with their own titles.

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

### A bar has to be told which way full means

`Meter` derived its tone from the fill alone — 75% caution, 100% critical —
which is exactly right for a budget and exactly backwards for progress. A bar
at 100% is two opposite pieces of news: a budget at 100% is refused charges, a
goal at 100% is the goal reached. So a goal three-quarters of the way there
rendered as a **warning**, and one fully achieved would have rendered as a
**crisis**, on the one screen a founder reads to see how a quarter is going.

`fullMeans` is therefore **required**, not defaulted. A default is the wrong
answer half the time, silently — and the one call site that had noticed was
passing `tone="accent"` to opt out of the rule rather than fixing it, which is
the shape a wrong default always leaves behind.

- `spent` — a budget, a capacity, a quota. Full is bad and the bar warns
  before it gets there.
- `achieved` — progress towards something wanted. Full is GOOD and says so;
  nothing below it is a fault the bar can diagnose.

An explicit `tone` still wins, for what a caller knows and a ratio does not: a
budget already refusing charges is critical at any fill.

---

### `KeyValue` is a metadata list, not a panel layout

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

`KeyValue` keeps the one thing it is for on this screen: the conversation key,
which really is a label and a value.

And **a chip must not repeat the sentence beside it.** The trigger's summary
is built by the vendor's own summariser and already opens with who wrote it
("Message from founder: …"), so a `founder` chip next to that line was the
same fact twice — and two chips plus a link after one line of prose is a
hedge, not a header. Only the integration stays: it is the one thing the
sentence does not reliably carry.

### A row is not an inline link

`a:hover` underlines, which is right for a link inside a sentence and wrong
for a whole ROW that happens to be an anchor: hovering one struck a line under
its timestamp, its summary and its type at once — three unrelated fragments,
none of them a link in the sense the underline means. A row-shaped link
already says it is hoverable with its ground, so the decoration is suppressed
on every one of them (`.feed-row`, `.turn-row`, `.seat-card`,
`.attention-row`, `.brand`, `.hit-title`). `.nav-item` had always done this;
the rest had not, and nothing connected them.

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

- **The project is the rail, not a dropdown.** A company's projects are the
  first division of its work, so they are always on screen with their open
  counts beside them, and choosing one is a SECTION change rather than a
  filter. `All work` is a real destination above them, because "what is the
  whole company doing" is a question somebody asks.
- **Six views over ONE answer.** List, Board, Calendar, Timeline, Sprint and
  Backlog differ in how rows are SHAPED and never in what was asked for — the
  board's columns are the server's own grouping, the calendar buckets rows the
  server already returned, and the timeline lays out the `start`, `due` and
  `waiting_on` those same rows carry. A view that fetched differently would be
  a second idea of what the filters mean. The timeline's own window is derived
  from the rows on screen, which is why it asks for a big unpaged page: a
  second page would redraw the first one's axis.
- **A view SETS the scope segment.** Open / Closed / All is the single
  authority on the status group a read asks for: the screen spreads a view's
  saved parameters and then writes that one key from the segment. So a
  segment defaulting to a constant made a view saved over closed work
  unrunnable — it opened on `Open`, overwrote the view's own group, and named
  a scope its rows did not match. The segment therefore takes its DEFAULT
  from the chosen view, which is what makes the control and the query agree;
  a scope somebody picks is in the URL and outlives a view switch, like every
  other filter here. A saved group the three segments cannot name — `active`
  alone — reads as `All`, so the segment and the read agree on the wider set
  rather than the control claiming a narrowing that is not applied.
- **A row is a table, and its columns belong to the LIST.** Every row was its
  own grid container once, so `auto` tracks sized against that row's content
  alone and a status badge landed at a different x on every line. The tracks
  are declared on the list and each row takes them with `subgrid`, so a
  column is as wide as the widest value in it. The row also keeps a CELL for
  a value it does not have: an undated, unassigned, unestimated task lines
  its status up with the task above it rather than pulling every later
  column one place left.
- **Nothing is drawn for the default.** `normal` priority is what a task gets
  when nobody said, so it is most of a board — a mark on every card is a mark
  that says nothing, and it buries the four that ARE urgent. A due date drops
  the year it shares with the reader for the same reason: one year repeated
  down forty rows is how the one row due next year goes unnoticed.
- **Reading an item does not lose the board.** A plain click opens a PEEK
  beside the rows; ⌘-click and middle-click follow the anchor to the item's
  own page, because a card that cannot be opened in a tab is not a link.
  Below about 1500px it becomes a DRAWER over the board rather than a third
  column, and that threshold is measured rather than guessed: the sidebar,
  the rail and the peek are three panes and the board is what is left, so at
  1280 — an ordinary laptop — one board column was left beside a detail
  panel, which is not a board.
- **The charts answer the questions the numbers cannot.** A census bar says
  the shape of a project where three counts say only their sizes; a velocity
  bar list is read in SPRINT order, never sorted by size, because the
  question is a trend; a burndown draws remaining against an ideal, where
  remaining is an OPEN STATUS GROUP rather than "not delivered" — counting
  cancelled work as remaining makes a descoped sprint run flat. All of them
  wear STATUS tones rather than the categorical hues, so one fact is never
  two colours on one screen.

---

## Honest empty states

A screen that renders a blank where data would go is a screen that cannot be
trusted when it IS blank. Three distinctions the product makes everywhere:

- **Nothing happened** vs **nothing could be read.** "No events" on a fresh
  company and "no events" on a node with no event log are the same empty list
  and completely different problems. `QueryState` renders the engine's own code
  — `no_event_store`, `unauthorized`, `unknown_query`, `bad_params`,
  `timeout` — as a sentence saying which. `bad_params` is the one that names
  the SCREEN as the fault: the engine understood the question and refused it,
  so retrying sends the same bad request again.
- **Zero** vs **unknown.** The integrations answer's counts are three-valued,
  and a node not serving ingress reports `unknown`, not `0`. The budgets answer
  says `durable: false` when the counter could not be READ.
- **Not configured** vs **empty.** A knowledge search with no backend says so;
  a company with no seats says roles come from the configuration.

Every empty state names what would fill it.

---

## Changing what you are looking at

**The dashboard only reads.** Every change in a Crewlet company is attributed
to whoever made it, and a browser form posting as "the dashboard" would be the
one actor an audit trail cannot name — so there is no edit button anywhere in
it, and that is a decision rather than a gap.

What an object page offers instead is a closed disclosure, **Change this with
your assistant**, holding the operator MCP calls that would make the change,
with this object's ids already in them:

```
update_work_item {"item":"ENG-8","status":"in_progress"}
comment_on_work_item {"item":"ENG-8","body":"…"}
remove_work_item {"item":"ENG-8"}
```

Copy one, edit it, and send it to whatever assistant you have connected to
`/operator/mcp`. Anything irreversible is last and says so. One line under the
block names who would be attributed — your token, as an operator, never a seat.

The calls are checked against the real operator catalogue by a test
(`TestEveryToolCallTheScreenOffersIsOneAnOperatorHas`): every tool named
exists, every argument filled is one that tool takes, and none of them is a
read. A block of tool names written by hand is a documentation surface that
starts lying on the first rename, and it lies in the worst way — a copied call
the engine refuses, on the one screen whose whole promise is that this is what
to send.

## How it is built

```
dashboard/                  the source — React 19 + TypeScript, built by Vite
  src/protocol/             the wire, typed. NO React, NO DOM at module scope
  src/app/                  shell, hash router, IA, command palette
  src/lib/                  store bindings, one clock, formatting, derivations
  src/ui/                   the component library and the chart kit
  src/routes/<workspace>/   one directory per workspace — inbox, me, work,
                            company, knowledge, activity, cost, admin — and
                            one file per screen inside it. The exception is
                            NotFound, which belongs to no workspace: it is
                            what the dispatch falls through to
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

---

## Rules a change has to keep

1. **Colour is state, never identity.** No hash-to-hue, no per-agent tint, no
   per-category chip colour. If you need to tell two things apart, use their
   names. The third-party app marks in `ui/VendorMark.tsx` are the one, bounded
   exception (see "The one rule" above); nothing else is.
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
7. **A write says what happened.** Every write goes through
   `protocol/rest.ts` and reports its outcome: a toast on success, and the
   engine's own refusal beside the field it names. A button whose result is
   invisible is a button an operator presses twice.
8. **No screen renders a credential.** The setup dialog shows the `${VAR}` a
   field points at, or — for a hand-written literal — an empty box whose
   PLACEHOLDER is dots saying one is held. Never a value, because no route
   returns one, and never dots as the value: a placeholder cannot be
   submitted, and a sentinel that has to be recognised on the way out is one
   an edit can defeat.
9. **A card is one object in two states.** The Integrations screen draws one
   bordered card per tool rather than rows in a shared panel, so a connected
   one can grow a body and still read as the thing it already was. A row that
   expands inside a list of rows pushes its neighbours around and reads as the
   list breaking. The disclosure is the identity block, never the whole
   header, so the card's own buttons are not nested inside a button.
10. **Every screen, section and filter is in the URL**, and obeys the
    push/replace table above.
11. **A screen subscribes to the slices it reads and no others.**
12. **Numbers are tabular**, and an absent number is an em dash rather than a
    zero — zero is a measurement.
13. **A control reports its own outcome**, especially an invisible one. A copy,
    a download, a write, a revoke — if the reader cannot see the result, the
    control says it, in text a screen reader reaches as well as an icon. A
    refusal is reported too, and it OUTLASTS the confirmation: a reader who
    looked away for three seconds must still be able to learn it did not
    land, so a failed state holds until the next click while a successful one
    settles back.
14. **Per-subject state is keyed on its subject.** A hash change re-renders
    the route switch rather than remounting it, so `#/turns/A` → `#/turns/B`
    reconciles and anything the screen remembers about A — a held refusal, an
    open disclosure, a selected tab — describes B until something clears it.
    Every id-bearing route carries `key={id}`.
15. **A head's controls wrap.** `.row` does not on its own and every `.btn` is
    `white-space: nowrap`, so a head with several controls overflows a phone's
    line instead of taking a second one.
16. **A link reads as a link at every size.** A text-register class on an `<a>`
    that overrides its colour makes a navigation into decoration; `.t-link` is
    the caption-sized register that keeps the accent.
17. **Run `make dashboard` and commit `static/dashboard` with the change.** CI
    diffs it; a bundle that has drifted from its source is a red build.
