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

The one exception is a **third-party app's own mark** on the Integrations
screen (`VendorMark`, from `@crewlethq/icons`): Slack's four colours,
Atlassian's blue, GitLab's orange,
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

`@crewlethq/tokens` is the one source of colour, type, space and motion. The
dashboard declares none of its own: it imports the palette, the themes, the
density scale, the faces and the document baseline, in that order, above every
other import in `dashboard/src/main.tsx`. Above, because an import is
evaluated in source order and the components carry their own stylesheets, and
the baseline has to be the thing a component rule outranks rather than the
other way round.

The rule table and the colour maths are the package's own,
`@crewlethq/tokens/test/palette`, so the design system and this application
cannot come to disagree about what a floor is.
`dashboard/src/styles/palette.test.ts` runs them over the INSTALLED
stylesheets in that same import order, in **every theme state** (light, dark
by media query, dark by attribute) and over **every composited surface a token
can land on**, including a hovered row inside a nested panel, which is where a
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
| the three phase hues, pairwise, under normal / protan / deutan vision | ΔE 10 |
| the four status hues, pairwise, under all three | ΔE 10 |
| adjacent data hues, in series order, under all three | ΔE 9 |
| every data hue against the reserved danger hue | ΔE 14 |
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

Nine sizes, `--font-size-2xs` … `--font-size-3xl`, and **they are the only
sizes in the product**. The system this replaces had 194 `font-size` declarations across 14
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
row has two edges that want different things. `--nav-gutter` insets the
rail, and it is where a row's own background, hover and active tint begin, so
it decides how much of the rail's width the click target covers.
`--nav-row-pad` insets
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
- A **tab** is a `tab=`, `view=` or `lens=` query on the path you are
  already on — three spellings of one thing, each named for what it
  switches: an object's tabs, a list's views, a screen's whole lens.

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
uuid — and every reserved segment is lowercase. That is what lets `#/work/views`
resolve before any answer arrives.

The set itself is `RESERVED_SEGMENTS` in `app/nav.ts` and is deliberately NOT
copied out here: a prose list of twenty-one strings is a list that goes stale,
and this one had drifted to fifteen while two of the entries it did name were
reserving route space nothing routed. `router.test.ts` holds the routes named
in the table above against that list and against `RAIL`, so the table and the
code cannot disagree about which addresses exist.

### The rail

The order is the product's story: you, the work, the people, what they know,
what they did, what it cost, the machine.

| Row | Route prefix | Badge |
|---|---|---|
| **Inbox** | `#/inbox` | unread notices on the first page under a reason the person's record counts as PRIMARY, `caution` hue — the only badge in the chrome allowed a status colour. Not every unread notice: most of a busy company's are things it merely told you (a task you watch moved, a sprint you are in started), nobody answers those, and a count that never reaches zero however diligent the reader is reads as a broken counter. The primary half is small by construction and goes down by answering |
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

**A number beside a workspace row says what it counts.** `SidebarRow.count` is
one field holding a value AND the sentence naming its question, because the two
used to be a number and an optional sibling and the optional half is the one
that went missing. A unit carried three unqualified figures across the product:
its whole subtree in this rail, its own members on the org chart's block, its
on-screen rows in the roster's group head. All three now come off
`lib/seats.ts`'s `unitTally`, which answers both questions at once —
`unitSeatsLabel` says the subtree first and names the direct count only where
the two differ, and `unitDirectLabel` is what a roster group says, because a
seat sits in exactly one group and nothing under a unit is in it. A filter
outranks both: with one on, every count on the screen is over what matched.

`g` then a letter jumps to a workspace (`g i`, `g m`, `g w`, `g c`, `g k`,
`g a`, `g o`, `g d`); `[` collapses the rail. A chord rather than a modifier,
because every single-modifier combination worth having is already the browser's.

### The routes

| Route | Page | Tabs / views |
|---|---|---|
| `#/` → `#/inbox` | **Inbox** — the landing screen | `state=unread\|all\|snoozed` · `reason=` · `row=` (which row the detail pane is on) |
| `#/me` | **My work** — the seven claims, plus what reached you | `handle=` (an operator reading somebody else's day) |
| `#/work` | **All work** | `view=list\|board\|calendar\|timeline\|table\|trash` + the filter grammar |
| `#/work/search` | **Search** — the company's work ranked against a phrase | `q=` |
| `#/work/views` · `#/work/views/{id}` | **Saved views** — the inventory, and one view run | |
| `#/work/{KEY}` | **Project** | the same view strip, scoped to the project |
| `#/work/{KEY}/sprints` · `#/work/{KEY}/sprints/{n}` | **Sprints** — figures, velocity, burndown | |
| `#/work/{KEY}-{n}` · `#/work/{id}` | **Item** — description, thread, history, links, properties | `thread=comments\|history\|woke` · `record=` (which change's routing) |
| `#/goals` · `#/goals/{id}` | **Goals** | |
| `#/company` | **Company** — the charter, the chart, and editing them | `lens=chart\|charter\|builder` (builder is *(operator)*) · `unit=` · `seat=` |
| `#/company/people` | **People** — the one directory, and who is carrying how much | `view=seats\|workload` · `group=state\|unit\|flat` · `q=` |
| `#/company/people/{handle}` | **Seat** — agent or human | agent: overview · work · turns · conversations · memory · cost · access · schedules; human: overview · work · access. `conversation=` opens one thread |
| `#/company/units/{id}` | **Unit** — lead, purpose, goals, seats, sub-units | |
| `#/knowledge` | **Knowledge** — live search over the backend | `q=` |
| `#/knowledge/{CONTAINER}` | **Container** — browse the tree | `kind=prose\|skills\|all` |
| `#/knowledge/{CONTAINER}/{Title}` | **Page** | |
| `#/activity` | **Live now** — what the company is doing at this moment | `window=15m\|1h\|6h` |
| `#/activity/turns` · `#/activity/turns/{id}` | **Turns** — every phase, round by round | |
| `#/activity/runs` · `#/activity/runs/{turn_id}` | **Coding runs** — live and durable | |
| `#/activity/schedules` | **Schedules** | |
| `#/activity/a2a` | **Agent-to-agent** | |
| `#/activity/traces/{id}` | **Trace** — one distributed trace, every span of it. NO LIST: nothing enumerates traces, so a bare `#/activity/traces` is the turns list, which is the nearest thing to "the traces" this product has | |
| `#/activity/events` · `#/activity/events/{id}` | **Event log** — the time axis, then the rows | `window=1h\|6h\|1d\|7d\|30d\|<from>/<to>` · `category=` · `actor=` · `q=` · `failed=` |
| `#/cost` | **Spend** — over time, then by phase, model, seat and turn | `window=1d\|7d\|30d\|90d\|<from>/<to>` · `group=phase\|model\|seat\|unit\|worker\|turn` · `compare=previous` |
| `#/cost/budgets` | **Budgets** — caps, the durable counter, what is refused | |
| `#/admin/fleet` · `#/admin/fleet/{node}` · `#/admin/fleet/domains/{domain}` | **Infrastructure** — nodes, leases, duties, replication, and one state-log domain with every node's position in it *(operator)*. ONE tail segment is a node and two are a domain, discriminated on the tail's LENGTH rather than on the word, because a node id is operator-chosen and `domains` is a legal one | |
| `#/admin/integrations` · `#/admin/integrations/{kind}` | **Integrations** — the catalogue, and one tool with what has actually been arriving on each of its surfaces *(operator)* | |
| `#/admin/tools` · `#/admin/tools/{tool}` · `#/admin/tools/servers/{name}` | **Tools** *(operator)* | `q=` · `origin=` |
| `#/admin/config` · `#/admin/config/revisions/{id}` | **Configuration** *(operator)* | `lens=active\|entities\|audit\|diff` |
| `#/admin/credentials` | **Credentials** — names and provenance, never values *(operator)* | |
| `#/admin/audit` | **Audit** — every write a person or a token made, across all four subsystems that record one: the tracker, the knowledge base, the configuration history and the credential store *(operator)*. NO DETAIL ROUTE — every row already has a page of its own somewhere else | `window=1d\|7d\|30d\|90d\|<from>/<to>` · `actor=` · `kind=work\|knowledge\|config\|credentials` |

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

### Every list is one grid

`DataGrid` draws all of them. There is no `<table>` left in the product except
the retention screen's per-domain terms — a block that already titled itself,
with no header row and nothing to sort, which is a table in the sense the
element means rather than a list of objects.

**The sort is in the URL**, which is the rule the component exists for: a
sorted ops table that cannot be sent to anybody, does not survive a reload and
comes back unsorted from Back is sorted for one person for one minute.

**A screen's primary grid takes `sort=` and every other one takes
`sort.<name>=`.** Several screens carry two or three — the spend by seat and
the recent turns, a node's leases and its duties — and unnamed they would all
read the one key: sorting the lower one would silently re-sort the upper, and a
link to a sorted screen would mean something different depending on which table
the reader had touched. A name rather than an index, because an index is a fact
about the source order and inserting a grid above would move every link's
meaning by one.

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

So the head goes and each cell draws its own name beside its value. Three
things follow from that, and each is a rule a new column has to keep:

- **A column with no word in its head declares one.** `header` is what the head
  row draws, so a twenty-pixel column carries a mark or nothing — a work item's
  type, a row's actions, a pair of state tags. A card has no head to explain
  it, so the column supplies the word separately in `label`, drawn only here.
  Without it the card gets a bare mark on a line of its own between two
  labelled ones, which reads as a rendering fault rather than as a value.
  `app/source.test.ts` is what stops one reaching a screen.
- **A value wraps rather than being cut.** `.truncate` is right for a cell that
  IS one line of a fixed column and wrong once the column is gone: at 390px it
  cut the one field the reader opened the list for and left the rest of the
  card empty underneath it. What bounds the value instead is the engine — a
  title is at most `tracker.MaxTitle`.
- **The sort control goes with the head.** That is the real cost of the shape
  and it is the right trade: a column head a reader cannot see is not an
  affordance, and `sort=` is in the URL — so a sorted list still arrives sorted
  from a link, or from the wider layout that set it.

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

**An absent value is a decision.** Three different facts share one empty cell
and they are not interchangeable:

- the row is **dropped** — this object has no such property at all (a task with
  no collaborators has no collaborators row);
- the row is a **dash** — the property exists and holds nothing (a task with no
  due date has a due date, unset);
- the row **says something** — the empty state means something a reader should
  know ("none — reflection uses the default").

The caller says which, and there is no path by which forgetting produces a
plausible-looking wrong one. `0` and `false` are values a property can hold and
are rendered, never folded into "nothing set".

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
| `lens=` | section | which whole reading of a screen is drawn — the Company screen's chart, charter and builder, the Configuration screen's active, history and diff |
| `sort=` `cols=` | filter | the grid's order and its visible columns |
| `sort.<name>=` `cols.<name>=` | filter | the same, for a second grid on the page |

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
screen ever did: nineteen kinds were addressable and only a work item could
appear in the rail. A kind with no entry in that table is not an error — a
notice is read in its own inbox and a model has no page at all — and the rail
closes rather than opening empty.

**Each peek asks its own question.** It takes an id and fetches, rather than
being handed the row that opened it: the row carries what its list needed, a
peek answers "what is this thing", and the two differ on every kind. It is
also what lets a pasted `peek=` open on arrival, where no row exists.

**`[` and `]` walk the list, so the list publishes its order** with
`usePeekNeighbours`. Only the list knows what the reader is looking at —
sorted, filtered and paged as they left it — and a rail that stepped through
anything else would be walking a different set from the one on screen. A
screen that publishes nothing gets a rail with no stepper, which is honest.

The id is split on its FIRST colon only, so `sprint:ENG/3` and
`page:ENG/Deploy runbook` survive being carried in a query value.

### The frame

`dashboard/src/app/frame/` owns everything a screen wears, and a screen renders
none of it:

| Piece | What it is |
|---|---|
| `AppRail` | the workspaces, the badges, the engine pill, theme and density. 80px with labels, 48px of icons under 960, and a fixed BOTTOM BAR under 860 — an eighth of a phone's window spent permanently on a side column is the one column a phone cannot spare, and the side edge is where a thumb reaches worst. It stays the grid's first child in the markup either way: reordering it would put the navigation after the page for Tab and for a screen reader, which is the opposite of what a bottom bar is for |
| `WorkspaceSidebar` | one workspace's tree, built from LIVE answers rather than a table — a hand-kept copy would be wrong the first time somebody adds a project |
| `PageBar` + `Breadcrumb` | where you are, derived from the route by one function; the last segment is the object and is not a link |
| `StateBar` | the answer's own honesty in one place: degradation, `read_level`, `complete: false`, how far this node has applied |
| `ObjectHeader` | an object's eyebrow, title, status and up to six facts, in the same order on the page and in the peek. A fact may carry a `note` saying where its value came from — whether a duration was measured by the engine or derived from the events a page holds, what a token figure covers — for the facts a reader can reasonably doubt, and only those |
| `useTab` | which tab is real. `tab=` is a string off a URL and the tab set belongs to the object — a human seat has three and an agent seat has eight — so the hook resolves the parameter against the tabs this object HAS and the caller renders what it returns. It binds `1`–`9` for a `section`, which is where the tabs of an object live; the strip itself is `@crewlethq/ui`'s `Tabs`, the one tab widget, which mints the `aria-controls` pair so it controls a panel rather than claiming to |
| `DetailRail` | the peek's chrome — resizable, a drawer under 1180 px |
| `PeekHost` + `peeks.tsx` | the one peek in the product, mounted by the shell: the body belongs to the KIND, so a list opens a peek by naming what it points at. `usePeekNeighbours` is how a list publishes the order `[` and `]` walk |
| `DataGrid` + `cells` | sorting in the URL, bands from a grouped answer, typed cells; a row becomes a labelled card under 860 |
| `PropertiesRail` | an object's own facts, in sections, with who set each |
| `Histogram` + `FacetRail` | a log's time axis, and one dimension of it as chips |
| `TimeRangePicker` | the one control for `window=` |
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
this actually wants to know is whether anything is waiting on them — but a home
that is ONLY that queue has a failure mode that arrives on the first day: a
company where nothing is wrong renders as a blank page, and a reader cannot
tell that from a dashboard that is broken.

**So the first fold is the company, and the queue is under it.** The pulse
strip is eight facts on one line — seats working, runs parked, open, overdue,
blocked, the active sprint closing soonest and its days left, tokens, alarms —
each a link into the workspace that owns it, each carrying the scope of its own
claim in its title. Every one of them is true whatever the queue holds, so an
empty band below then MEANS something: nothing is waiting on you, on a company
that is visibly running. A figure whose query has not answered draws an em
dash, never a `0`: "your company has no open work" is a claim, and it is a
false one for as long as the read is in flight. `tokens` is deliberately not
labelled "today" — the window is the engine's and the screen was not given one,
so the strip names the figure and puts the window it covers on hover.

**Two bands, stacked on one screen, never two tabs.** **Needs a decision** is
what the engine derived, over the four subjects `lib/attention.ts` declares: the
engine and this node's link to it, the token budgets, the coding runs waiting on
an answer, and the seats themselves. **Notices** is the person's own inbox:
what reached them, and why. They are never interleaved, because one fused list
ordered by time would eventually rank a backup-age alarm above the CEO seat
asking whether to hold a release. And they are never a toggle: a tab hides the
engine's state behind a control the reader has to press, which is exactly what
the landing screen cannot afford — the founder's first glance is the whole of
what this screen is for. A band with nothing in it still draws its own heading
and says why it is quiet, because a band that disappears takes its name with it
and a reader cannot then tell "nothing is waiting on you" from "this product
does not have that".

The sentence it draws is DERIVED FROM THE ANSWER, never from the row count. Zero
rows is six different facts — nothing has answered yet, the answer was a
refusal, a reason chip took them all, the page holds none and there are more
pages, the reader is caught up, and nothing has ever reached them — and the band
branched on the facet alone, so a person who had never received a single notice
was told that everything had been marked read and sent to a facet that was just
as empty. `work_inbox` carries the evidence that separates the last two:
`seen_through` is the person's own read watermark, written only by `mark_inbox`,
and absent until they have marked something, so the copy claims the watermark
and not more than it. A band also draws its own filters whether or not anything
survives them — a reason chip matching nothing is exactly when it is the only
way back — and its count is an em dash before the first answer, for the same
reason the pulse strip's figures are.

**Unread, all and snoozed is a scope, not a facet.** Each is a different
question put to `work_inbox` — `unread` and `include_snoozed` are its parameters
— so the other two are pages this one does not hold and no count over the loaded
rows could describe them. It is a three-option `Segmented` with exactly one
always chosen, and an unknown `state=` resolves to `unread` rather than leaving
the control blank and the query wide.

**Two panes, and the right one is there before it is needed.** Reading one row
IS the activity here, so the detail sits beside the list rather than behind a
navigation, and the column is present from the first paint: a pane that appears
on the first click reflows the list under the pointer, so the row a reader
clicked is no longer the row they are looking at. Under 860px it is one pane,
list then detail — a human teammate answering an ask from their phone is this
product's second named reader, and this is the one screen drawn for them.

**The reason facets narrow client-side, which is what makes their counts
honest.** Sent to the engine, the filter narrowed the ANSWER — so the page the
counts were derived from became the page one facet had selected, every other
count went to nothing, and the whole rail unmounted under the pointer. Over the
loaded page the chips and the rows are the same set by construction, which is
what `over="loaded"` on the rail already promised. They are ordered by NAME and
never by count, because a count-ordered row reorders itself on a poll and the
chip a reader is reaching for moves between the decision to press it and the
press.

`work_inbox` still reports the `primary_reasons` that were APPLIED — defaulted
from the person's own record — and the rail badge counts those, so a company
that has re-decided what counts as primary gets its own badge without the
client knowing anything about it.

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

**A thing worth linking to gets an address, not a scroll position.** The
previous dashboard revealed a unit by scrolling the org screen to it
(`#/org?unit=Backend`, with a router-level reveal hook); this tree gives a unit
its own page instead — `#/company/units/{id}` — so it can be linked to, opened
in a peek, and carry its own state. There is no reveal-on-arrival hook here.
Where a selection genuinely belongs in the URL rather than in the path it stays
a filter: the Builder lens reads `unit=` and `seat=` to name the selected node
on its canvas (see the builder's toolbar below), and a `DataGrid` scrolls its
selected row into view. The ring is static: nothing on a live screen animates
for data.

**Work that exists nowhere else is not left behind unasked.** A surface
holding it registers a leave guard (`useLeaveGuard`, the org builder's node
editor while it has typed changes), and every move to another entry is put to
that guard first: a push from code, a link, and Back or Forward. A guard is
handed the route the move goes to and holds only a move that would lose its
work: the org builder keeps its draft and an open editor through a move within
the lens (a view or a chart), so its guards let that go
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

**What the band says when it is EMPTY is derived, not written.** Every condition
names one of four subjects, `SUBJECTS` maps each to the phrase a reader sees,
and the quiet band draws all four. That sentence used to be prose on the screen
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

**And it closes when the route changes**, the way the workspace drawer does.
Picking a row closes it on the way out, so what this covers is every other way
the route can move while it is open — Back, Forward, a phone's back gesture, a
restored history entry. A palette that survives one of those is left ranking
the objects of the screen the reader just left, over a screen it knows nothing
about. The token dialog is deliberately not in that rule: a credential prompt
is about the reader's access rather than about where they are.

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
  Model activity tab wrapped its turns in the query-state component, which
  renders nothing while a query is in flight and a banner *instead of* its
  children when one fails. So a turn happening right now was invisible until
  the event store answered, and invisible for good on a node that keeps no
  event log, where the engine does not serve the question at all and the
  answer is a permanent `unknown_query`. The query's
  state renders beside the turns now, never in place of them.

The seat screen makes the same split, where it answers a second question:
which of these turns is happening right now, readable at a glance from the
accent ring rather than only by finding a badge.

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
  hovering. `.t-link` is the caption register that keeps `--accent-ink`,
  which the palette suite already measures.
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
  arguments, most of them three lines. `label` is what such a block takes
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
`agents` is pushed twice per round.

**A control state is the other half of that rule, and it DOES ease.** The
distinction is what the pointer did: a fill that changes because somebody moved
onto a row is a response and should look like one, and a fill that changes
because the engine said something must land on the frame it is told. Almost
every interactive surface in the frame — the rail's rows, the sidebar's links,
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

`#screen-scroll`, the shell's own main region, scrolls, and it is the only
thing that may. The rail is fixed beside a scrolling pane, so a page that can
*also* scroll as a whole carries that rail off the top of the window and leaves
the reader looking at background below the application, with two scrollbars and
neither obviously the one they want. The shell is the design system's, and it
is what holds that: the document is not allowed to grow, so the invariant is
unreachable rather than merely unused.

It carries an id so the things that need it can find it without depending on a
class that styling owns. The router holds the only accessor (`scrollTarget`,
private to `app/router.tsx`) and uses it to restore a position per history
entry. The settled list reaches the same element to ask whether the reader is
at the top before it splices rows in — today by its `.screen` class rather than
by the id, which works only because one element carries both.

Everything here scrolls a chosen element directly rather than calling
`scrollIntoView`, which scrolls every scrollable ancestor it can find.

### A card has one left edge

A tight body used to reduce the horizontal padding as well as the vertical
one, so its content sat on a different vertical line from its own heading,
visibly closer to the border than the title above it, and a code block inside
one was pushed hard against the card's right edge with nowhere for its
scrollbar. What `tight` is for is a card whose rows carry their own vertical
rhythm (a stack of cards, a footer strip), and that is a claim about height.
It is vertical-only in `Card.Body`, on the inset the header sets, and all four
tight bodies in the product align with their own titles.

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

`PropertiesRail` keeps the one thing this shape is for on this screen: the
conversation key, which really is a term and its detail — labelled, with the
caption that says which external thread it names.

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
- **Five SHAPES over one answer, and eight tabs.** List, Board, Calendar,
  Timeline and Table differ in how rows are DRAWN and never in what was asked
  for — the board's columns are the server's own grouping, the calendar buckets
  rows the server already returned, the timeline lays out the `start`, `due`
  and `waiting_on` those same rows carry, and the table puts one field per
  column so a column can be compared down and sorted at its head. A shape that
  fetched differently would be a second idea of what the filters mean. The
  timeline's own window is derived from the rows on screen, which is why it
  asks for a big unpaged page: a second page would redraw the first one's axis.
  Sprint, Backlog and **Trash** are the other three tabs, and each is a saved
  QUERY rather than a shape — which is the distinction the strip is built on. A
  tab whose meaning is a parameter is a view; a tab whose meaning is a drawing
  is a type. The trash is a Table carrying `removed=true`, so every view saved
  with that parameter is read as one: the removal's actor and instant come from
  `work_activity` rather than from the row, and PURGED work has no row at all —
  its history entry is the only evidence it existed, so it gets a band of its
  own saying it cannot be restored.
- **The table sorts at the ENGINE.** A header writes the query's own `sort=`
  key, so the whole set is ordered rather than the hundred rows that happen to
  be loaded — and a column the grammar has no key for is therefore NOT
  sortable, because `ParseQuery` refuses an unknown key rather than ignoring
  it: one wrong header would take the board down with a refusal rather than
  mis-ordering a column. `status` is the one a reader expects and cannot have;
  `group_by=status` is what that question actually wants.
- **A view SETS the scope segment.** Open / Closed / All is the single
  authority on the status group a read asks for: the screen spreads a view's
  saved parameters and then writes that one key from the segment. So a
  segment defaulting to a constant made a view saved over closed work
  unrunnable — it opened on `Open`, overwrote the view's own group, and named
  a scope its rows did not match. The segment therefore takes its DEFAULT
  from the chosen view, which is what makes the control and the query agree;
  a scope somebody picks is in the URL and outlives a view switch, like every
  other filter here. A saved group the three segments cannot name — `active`
  alone — reads as `Open`, the wider set nearest what its author asked for, so
  the segment and the read agree rather than the control claiming a narrowing
  that is not applied. And a view that WIDENED — `show_closed` with no group at
  all — opens on `All`, because seeding `Open` there writes the narrow group
  back over exactly the half the view asked for: "and whatever finished this
  week" answered as open work alone, and the Trash tab answered as the removed
  tasks that were still open, hiding every removal of anything already done.
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
  (`unknown_query` for a question this node does not serve, such as the event
  log on a node with none, `unauthorized`, `not_found`, `unavailable`,
  `bad_params`, `query_failed`) and the client's own `timeout` as a sentence
  saying which. `bad_params` is the one that names the SCREEN as the fault: the
  engine understood the question and refused it, so retrying sends the same bad
  request again.
- **Zero** vs **unknown.** The integrations answer's counts are three-valued,
  and a node not serving ingress reports `unknown`, not `0`. The budgets answer
  says `durable: false` when the counter could not be READ.
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
  src/components/           the pieces more than one screen draws, composed
                            out of the design system
  src/routes/<workspace>/   one directory per workspace — inbox, me, work,
                            company, knowledge, activity, cost, admin — and
                            one file per screen inside it. Two things sit
                            outside that shape: NotFound, which belongs to no
                            workspace and is what the dispatch falls through
                            to, and routes/org/builder/, the organization
                            builder — one screen big enough to be a directory
                            of its own (see below)
  src/styles/               tokens, base, components, shell, frame, screens —
                            what the design system does not draw
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
- **A screen that throws takes only itself down.** `app/App.tsx` wraps the
  routed screen in an error boundary, so a malformed field renders "This
  screen could not be drawn" with the error's message and a Try again button,
  inside a shell whose navigation still works. Without it React unmounts the
  whole application on a render error, which is what a seat whose `llm` was a
  per-phase mapping once did. The boundary resets when the reader navigates.
- **One REST transport, one REST loader.** `protocol/rest.ts` is the only
  path to a REST route: `rest.request(method, path, options)` answers the
  status and the `ETag` beside the body, takes a caller's `AbortSignal`, never
  lets the browser cache a guarded answer, and resolves a 304 rather than
  throwing it; the body-only wrappers sit on top of it. The deadline and the
  caller's abort cover reading the body as well as waiting for the headers,
  and a body that breaks part way through is status 0 (an answer never fully
  heard, so a write's outcome is unknown) rather than an empty success. A
  screen that reads a REST answer uses `lib/useRest.ts`, which aborts a
  superseded read, re-reads when the operator token changes and, where asked,
  when the tab comes back.
  A refusal replaces what is on screen; a request that never reached the
  engine keeps the last answer with the error beside it; and an answer belongs
  to its path, so a read whose path changed reports nothing until the new path
  answers.
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
- **The shell is this application's own**, drawn with the package's primitives
  rather than taken whole from it. Two levels of navigation, a page bar, a
  state bar and a peek rail are what this product's six trees need, and they
  are not a shape a design system shared with a console and a documentation
  site can supply — [The frame](#the-frame) is the whole of it. The narrow
  layout opens the WORKSPACE SIDEBAR as a drawer, never the rail, because the
  rail is eight rows that already fit. A screen's own half is its labels, its
  coverage and its controls, portalled into the bar; every list it draws is
  [one grid](#every-list-is-one-grid).
- **One stack decides which surface a key belongs to.** Dialogs, sheets,
  menus and listbox popups register on the design system's layer stack
  (`useModalLayer`, `usePopupLayer`), in the order they opened, and so do the
  shell's own token dialog, its command palette and the narrow layout's
  sidebar drawer: no modal hand-rolls its veil or listens for Escape beside
  the stack. That drawer is the workspace sidebar itself, a dialog only while
  it is open; closed, the stylesheet hides it rather than only sliding it
  away, so its links leave the tab order, and it closes on a route change and
  when a resize takes the layout past the breakpoint. Only the topmost
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
  The page's own shortcuts (Ctrl or Command with K, and a bare `/`, for
  search) wait for the page: while a modal is open (`isModalLayerOpen`) they do
  nothing, because the page behind `aria-modal` is inert and search opened
  over a dialog could navigate away from under it, unmounting an unsaved
  editor or a write whose outcome the operator has not seen. Search closes on
  its own chord only from inside it.
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
  and after that moves only when the operator moves it. The canvas is a
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

### The org builder lens

`routes/org/builder/Builder.tsx` is the one component with a lifetime in the
builder: the reducer over the pure model in `routes/org/builder/model/`, the
dry-run check, the live region, the shortcuts, the selection and the dialog
that is open. Its views and dialogs are handed in (`BuilderSurfaces`, bound
in `routes/org/builder/surfaces.ts`) and reach all of it through
`BuilderContext`, so no view or dialog starts a request or touches storage.
Every surface is required: a Builder suite stands a view in with a fake,
while the screen binds the real canvas, outline, editor and dialogs, and
`surfaces.test.tsx` mounts the lens with exactly those.

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
- **The canvas view fills the screen.** With `view=canvas` the lens asks the
  shell for the window's height (`useFillScreen`), and it and the tab panel it
  sits in become flex columns of definite height, so the canvas takes what is
  left under the toolbar and the shell's scroller has nothing to scroll. The
  outline view withdraws the request, and so does the posture screen the lens
  draws before the engine has answered.
- **Fullscreen takes the builder container**, never the canvas: the toolbar,
  the view, the dialog host, a toast outlet of its own and the live region all
  render inside it, because a fullscreen element renders only its subtree.
  The control is not drawn where the Fullscreen API is missing. The shell's
  token dialog is outside it, so asking for a token leaves fullscreen first.
- **The selection is in the URL, and the toolbar mirrors it.** `unit=` and
  `seat=` are filters that name the selected node; a link naming one selects
  it, a rename rewrites it, and a removed node clears it. The toolbar carries
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
  storage cannot keep at all asks before the lens is left, since that loses it
  too; a move within the lens keeps the Builder and asks nothing.
- **A save is the checked write, signed, and never believed blindly.**
  `useSave.ts` sends the model's save request with the audit summary signed by
  a write id minted when the review opens and kept for as long as the draft
  does not change, so a second press after a lost answer carries the same id.
  A lost answer is settled from the revision history before anything else
  happens; while it is unknown the lens records nothing. A write in flight is
  never aborted, and a save that lands after the lens was left still records
  the revision and clears the kept draft. The kept log is marked with the
  write id before the save is sent and unmarked once the save is known not to
  have landed, so an answer lost after the lens was left, or across a reload,
  is settled on the next visit (`useSave.resume`) before the kept log is
  offered: replayed onto its own revision it would apply every change twice.
  A save this page still has out when the lens opens again is waited for
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
  (`savedRevision.ts`), watches the `stream` query's `applied_epoch` and the
  `fleet` query on `recheck.ts`'s cadence, and resolves to Applied, Applied on
  N of M nodes, or the node that refused it with a link to the Fleet screen.
  Its View changes opens the saved revision against its parent
  (`against=`), since against the active revision a save that is active now
  differs from nothing; the conflict banner's Show what changed is the newer
  revision against the draft's base for the same reason. Until this node
  applies it, the Company screen's read lenses carry a note that they draw the
  previous revision.
- **The status is the last answer about the current draft**, whoever asked:
  a save's refusal is placed on the nodes like a check's, and the check
  machine decides the status only while a check is out or before any answer.
- **A read-only lens records nothing.** The guarded and read-only postures, a
  conflict and a base the engine has not keyed yet all refuse operations at
  the one door every view goes through, and say why in the live region. The
  actions themselves are DISABLED rather than hidden, so an operator still
  reads what the builder does; Edit and Open seat change no draft and stay
  available.

### The org builder draws the engine's organization

The Builder lens of the Company screen shows the draft two ways, and both
read one module (`routes/org/builder/chartModel.ts`), so they can never
disagree about where a seat is drawn, who leads a unit or who a seat reports
to.

- **The document gives the shape, the engine gives the meaning.** Units,
  their seats and their children are drawn as the draft holds them, because
  that is what an operation edits. Every derived fact comes from the last
  check's `derived` block, read through the document that check was sent: a
  root seat the engine placed in a unit by its `unit:` reference is drawn in
  that unit and marked "Placed by unit reference", a reference that names no
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
- **The canvas is a tree of cards.** The structure chart has the company card
  at the root, root seats as cards and units as cards with their seats
  stacked inside as rows. The reporting chart is the engine's forest: seats
  with no manager at the top, marked "No manager", and seats that manage each
  other in a loop under one "Reporting cycle" group, each loop drawn from its
  first seat in the engine's order. The reporting chart is read-only, because
  a reporting line is not written anywhere as such; "Edit reports" (and Enter
  on a reporting card) opens the seat's editor at its Manages field, where
  `manages` is. The Builder's `openEditor` names the part of the form to start
  on (`EditorSectionName`), so an action about one field lands on it: a lead
  chip's "Choose another seat" opens the unit's editor at its lead.
- **A card holds no control a keyboard has to reach.** Each card header and
  each seat row is a `treeitem` with its level, position and expansion, and
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
- **Colour stays state.** A card is neutral whatever it holds. A human seat
  has the dashed edge every human seat on the dashboard has, a problem count
  takes the critical tone, a reference that names nothing takes the caution
  tone, and the Datadog fallback seat carries a neutral badge while Datadog is
  enabled, the only time the engine routes an alert to it.
- **A live push never moves a card, and neither does a check.** A saved agent
  seat shows the same `StateBadge` as every other screen, in a slot that
  neither shrinks nor wraps while the seat's name truncates beside it, and
  live state is no input to the layout, so a push changes a word and never a
  measured height. The problem count, which is the current draft's and so is
  absent while the check of every edit is out, has a slot of its own on the
  same first line, which is as tall with it as without it. A mark the engine
  gave (a lead or a unit reference that names nothing) is held by the chart
  like a placement, while the node still writes what the check warned about,
  rather than leaving with each check and coming back with its answer.
- **The outline is a treegrid of rows.** The same structure as rows with
  navigable cells: Name, Kind or type, Handle, Lead or reports to, Problems
  and the row's actions. A row's keys are a card's keys; Right opens a row or
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
  being cut off under the last rows. An inline add row closes each unit's rows
  and the company's while the draft can change. Alt+Up and Alt+Down move a row
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
   **A seat's identity badge is `Avatar`, everywhere**, drawn from its name or
   handle so the initials are what tell one seat from another, with
   `variant="dashed"` for a human seat — the engine does not run it, which is a
   structural fact and therefore an edge rather than a hue. A hand-rolled mark
   holding a robot glyph drew the KIND, which the roster gives at a glance, in
   the slot that should have been saying WHO: the same engineer was "FE" on the
   board and an identical generic robot on Search and on a goal's Owners panel.
   `tone="brand"` is for the one badge that IS the reader, in the rail's own
   account row, and for nothing else.
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
    the route switch rather than remounting it, so `#/activity/turns/A` →
    `#/activity/turns/B`
    reconciles and anything the screen remembers about A — a held refusal, an
    open disclosure, a selected tab — describes B until something clears it.
    Every id-bearing route carries `key={id}`.
15. **A screen head's controls wrap.** A button does not break its own label,
    so a head carrying several of them overflows a phone's line instead of
    taking a second one. `PageBar` and `ObjectHeader` take their badges and
    their actions as slots that wrap, and any row built beside one wraps too.
16. **A link reads as a link at every size.** A text-register class on an `<a>`
    that overrides its colour makes a navigation into decoration; `.t-link` is
    the caption-sized register that keeps the accent.
16b. **Text takes the ink, never the fill.** Every status family is three
    rungs: the base is what a thing is painted *with* — a primary button, a
    status dot, a meter bar — the `-soft` is the ground it tints, and the
    `-ink` is the one of the three that is a text colour. The design system
    measures the pairs it publishes; nothing but `styles/rungs.test.ts`
    measures which rung this application spends where, and both mistakes are
    silent. Spending `--accent` as text gives 3.09:1 on its own soft ground and
    3.71 on the page, against the 4.5 small text needs; the ink gives 5.96 and
    7.15. Putting an `-ink` **on** its family's solid fill is the one pairing
    of the three that was never measured — the rail's attention badge did it,
    at 1.72:1 on a 9px digit, so the unread count rendered as a dot.
16c. **The faint rung is decoration, and that is a contrast rule.**
    `--text-faint` is about 3:1 against the ground in *both* themes, which is
    the floor a non-text mark takes and not the 4.5 a word needs. Spent on a
    word it is a word nobody can read — it had decayed onto twenty-six rules,
    every grid column head and sidebar section name among them. A word takes
    `--text-muted` (6.6–7.0:1). What is left on the faint rung is ten marks —
    two tree characters, a breadcrumb slash, four icons and three icon-only
    controls — each named in `styles/rungs.test.ts` with the reason it is one.
17. **Run `make dashboard` and commit `static/dashboard` with the change.** CI
    diffs it; a bundle that has drifted from its source is a red build.
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
