# Authoring Artifacts

How to write an [artifact](../concepts/artifacts.md) that publishes cleanly and
works for everybody it is shared with, whether a person, the CLI or an agent
publishes it. These are the rules the engine enforces at publish; the page's
runtime API is the [artifact runtime reference](../reference/artifact-runtime.md).

---

## Choosing a kind

| You want | Kind |
|---|---|
| A table people fill in together: a review, a sign-up, a tracker of decisions | `sheet`. Declare the schema; the dashboard draws it with presence, per-cell history and accessibility, and no page code |
| A document: a report, a plan, release notes, a handbook in tabs | `doc`, Markdown in one file or in tabs; nothing runs. See [Writing a doc](#writing-a-doc) |
| A presentation: a pitch, a review, a talk | `slides`, a deck in Claude's slide format. The dashboard draws, presents and prints it, people fix text and order in place, nothing runs. See [Writing a deck](#writing-a-deck) |
| Several screens reviewed side by side: a landing page's sections, an onboarding flow, three takes on one hero | `design`, a canvas of artboards, each a plain HTML page, rearranged and annotated in place. See [Writing a design](#writing-a-design) |
| A dashboard: figures over the company's work, spend and agents, or over data an agent collects | `dashboard`, a closed spec of datasets and widgets, every figure answered as its viewer and citing its source. See [Writing a dashboard](#writing-a-dashboard) |
| A calculator, a custom view over shared data, anything the formats above cannot draw | `html`, with the capabilities it needs |

Start from a `sheet` whenever the content is rows and columns; an `html` page
can still read and write the same store for a custom view.

---

## The page contract

An `html` version is a set of files with one **entry** (usually `index.html`).
The engine refuses a publish that breaks a rule below, naming the file and the
rule, and warns on success about what is legal but likely wrong.

**Files and paths**

- Paths are relative: no leading slash, no `.` or `..` segment, no backslash,
  no control character, at most 1024 bytes. Reference your own files the same
  way (`app.js`, `img/logo.svg`); every version is served under a path the page
  never sees.
- A file's served type comes from its extension, via the engine's one media
  table (`internal/artifacts/mediatype`); a client never chooses it. Use web
  formats: HTML, CSS, JavaScript, JSON, SVG, the common image, font, audio and
  video formats, plain text, CSV, PDF and WebAssembly. An unknown extension is
  refused.
- `preflight.js` at the root is reserved (at most 8 KiB).

**Limits**

| Bound | Value |
|---|---|
| The entry | 16 MiB |
| Any other text file | 16 MiB |
| Any binary file | 15 MiB |
| New files in one publish | 255 (carried files do not count) |
| New bytes in one publish | 64 MiB |
| Files in one version | 511 |
| Bytes in one version | 256 MiB |
| Title, description, icon, version label | 120, 1000, 40 and 60 bytes |
| Edits in one publish | 64 |

**Scripts and styles**

- Inline `<script>` blocks and your own script files run as written: the
  engine marks every script element and gives each served document a fresh
  nonce. Do not write a nonce.
- Inline event handlers (`onclick="..."`) work: their digests are recorded at
  publish. Prefer `addEventListener`; a handler added to the DOM after load is
  not admitted.
- Scripts loaded by your own scripts run too (the policy is `'strict-dynamic'`),
  so a bundler's chunk loader works.
- A `<script src>` or stylesheet `<link>` on another host must be on the
  publish linter's CDN allowlist, at an **exact** version (`react@18.3.1`,
  never `react` or `react@18`). The engine copies the file into your version,
  checking its integrity, and rewrites the reference; viewers never reach the
  CDN. Any other remote reference is refused: upload the file.
- An ES module imports another origin only through an **import map**
  (`<script type="importmap">`), never a literal URL in an `import`. Each entry
  naming another origin follows the rule above and is rewritten to the copy.
  The copied module must import nothing itself, so map a self-contained build;
  an entry mapping a whole folder (`"lodash/": "https://..."`) is refused.
- `eval`, `new Function` and WebAssembly compilation are admitted only for a
  page the linter finds needs them (an in-browser transform, a `.wasm`
  module).

**What a page cannot do**

- Reach the network beyond its own files: `fetch`, images and media load only
  from the artifact's own origin. Live data comes through the runtime.
- Open a window or popup. Use `crewlet.openLink(url)`: the dashboard shows the
  viewer where the link goes, then opens it.
- Show a non-page file in its frame. A link to one of its own pictures, videos,
  PDFs or assets opens in the runtime's file viewer over the page, so link a
  thumbnail to its full-size picture and give the thumbnail an `alt`, which
  names the picture there too
  ([Artifact runtime](../reference/artifact-runtime.md#links-to-the-pages-own-files)).
- Submit a form anywhere. The `submit` event fires; handle it in script.
- Redirect off its origin with `<meta http-equiv="refresh">` (refused at
  publish), embed another page in a frame, or use plugins.
- Rely on browser storage. On a deployment serving artifacts from one shared
  host, `localStorage`, `sessionStorage` and IndexedDB throw. Wrap every access
  in `try`/`catch` and render correctly without it, or declare `state` and keep
  per-viewer state through the runtime (it follows the viewer across devices).

**What the linter warns about**

Browser storage used for state other viewers should see (use the datastore),
no `<meta name="viewport">`, and a version that ignores its reader's
appearance setting. A version follows it when its pages link both of
[the kit's](#capabilities-and-the-runtime) theming files, `kit.css` and
`theme.js`, at a release the engine serves, or when a page or stylesheet of its
own carries a `prefers-color-scheme` rule. Also warned:

- `kit.css` alone (it follows the device, not the reader's chosen mode), or
  `theme.js` alone (it applies a mode nothing styles).
- A link to a kit release the engine does not serve (a 404, so no kit loads);
  the warning names the newest release.
- A link to a file the named release does not ship, such as `content.css` at an
  older release (a 404); the warning names the same file at the newest release
  when that ships it.

---

## Capabilities and the runtime

A version declares what its page may ask the host for, and a viewer's ticket
grants it:

| Capability | Lets the page |
|---|---|
| `db` | Read and write the artifact's shared datastore, under the collection rules |
| `state` | Keep per-viewer state (a remembered filter, a collapsed panel) |
| `identity` | Learn who is viewing: their seat, kind and role on this artifact. Never a token or an address |
| `presence` | Say where the viewer is, and show who is in a part of the page; the dashboard's bar already shows who has it open |
| `assets` | Reference files from the artifact's asset store |

A publish declaring an unknown capability is refused. An undeclared namespace
still exists on `window.crewlet`, and every call on it rejects with
`capability_not_declared`, so `crewlet.capabilities.has("presence")` detects
one.

**The runtime contract.** A version pins the runtime it was written against:
pass `contract: "latest"` for the newest this engine serves (stored as the
exact version), an exact version to pin one, or nothing to keep the current
pin. Never include the runtime yourself; the engine serves it from
`/_crewlet/runtime/<contract>/` on the artifact's origin and it installs
`window.crewlet`. See the [runtime reference](../reference/artifact-runtime.md).

**The kit.** The artifact kit is the company's design system for pages:
`kit.css` (tokens and a bare base that follow the reader's theme),
`content.css` (the design system's look for the plain HTML a page writes
itself), `theme.js` (a few hundred bytes that apply the reader's light or dark
mode before first paint) and `kit.js` (custom elements for a sheet bound to the
datastore and presence, used without a framework). It is served from
`/_crewlet/kit/<version>/` on the artifact origin:

```html
<link rel="stylesheet" href="/_crewlet/kit/<version>/kit.css">
<link rel="stylesheet" href="/_crewlet/kit/<version>/content.css">
<script src="/_crewlet/kit/<version>/theme.js"></script>
<script type="module" src="/_crewlet/kit/<version>/kit.js"></script>
```

Link the newest version this engine serves, the one that reads the company's
[brand](#your-companys-brand). It moves with the engine, so find it in a seat's
artifacts skill (`${crewlet_kit_version}`), in `get_company_brand` (as
`kit_version`) or in the publish warning on an unserved release. The version in
the path is a pin: the engine keeps serving every kit version it has shipped,
so a published page keeps its files. To take a newer kit, publish a new version
that links it.

Take colours, spacing and type from the kit's tokens, never literal values, so
a page matches the company in both themes.

**Plain HTML.** Link `content.css` right after `kit.css` whenever the page
writes its own headings, paragraphs, lists, quotes, code, tables, rules or form
controls. Without it they render on the kit's bare base (one heading size, no
space between blocks, buttons as plain text); with it they take the dashboard's
document and form look in both themes:

- It styles elements only, each rule at its element's weight, so a class of
  your own overrides it. Link it before your own `<style>`.
- It never touches what the kit draws (a `<crewlet-sheet>` and its menus). A
  region you design entirely yourself opts out with
  `data-crewlet-content="off"`.
- Wrap a table in a `<div>` or `<figure>`: a table wider than a phone scrolls
  inside it. Mark a column of figures `data-align="right"` (or `"center"`),
  which also keeps each figure on one line.
- A `<button type="submit">` is the primary action; every other button is
  secondary.

It is opt in: a page that does not link it paints as before.

### Your company's brand

When an administrator has adopted a brand (Company, Brand), the artifact origin
links its generated stylesheet beside the kit on every page and design, pinned
to the brand current at publish. It sets only the kit's brand inputs, so a page
using the kit's tokens (`var(--color-brand-accent)`,
`var(--font-family-display)`) takes the brand's colours and typefaces in both
themes; a page with colour literals, or linked to a kit older than the one that
reads the brand, does not. Docs and dashboards follow the adopted brand as the
dashboard draws them.

- **A logo** is the kit's markup, never a URL:
  `<span class="crewlet-logo" role="img" aria-label="Acme"></span>`, with
  `data-variant="on-dark"` for a dark band or `"mark"` for a square space. It
  draws the brand's logo and takes no room where there is none.
- **A file of the brand in a version** (a deck's logo or typeface, a doc's
  image) is copied from the adoption, never redrawn: an agent names it with
  `{from_brand: "logo.primary"}` in `files` (`logo.on_dark`, `logo.mark`,
  `font.heading`, `font.body`); `get_company_brand` lists each with the path to
  copy it to.
- **A deck** begun in New, Slides or with an agent's `starter` in the theme
  `brand` is drawn in the brand once: colours inline, logo and typefaces as
  files of the version; a later adoption changes nothing in it.
- **A page that must not take the brand**, such as a mock of somebody else's
  screen, sets `<html data-crewlet-brand="off">`.

---

## Shared data

Declare `db`, then describe the data as **collections** with a schema, so the
engine validates every write and the dashboard shows the data in its Data tab
even when your page is broken.

```json
{
  "name": "review",
  "title": "Review",
  "preset": "contribute",
  "columns": [
    { "key": "item",     "type": "text",     "label": "Item", "primary": true, "required": true },
    { "key": "reviewer", "type": "person",   "label": "Reviewer" },
    { "key": "status",   "type": "status",   "label": "Status",
      "options": [
        { "id": "todo", "label": "To review", "category": "todo" },
        { "id": "ok",   "label": "Approved",  "category": "done" }
      ] },
    { "key": "signoff",  "type": "checkbox", "label": "Lead sign-off", "write": "editor", "humans_only": true },
    { "key": "notes",    "type": "longtext", "label": "Notes" }
  ]
}
```

- **Pick a preset by intent**: `shared` (participants read, editors write),
  `contribute` (participants add rows and edit their own), `author` (only the
  owner and the authoring agent write) or `private` (each viewer's own rows,
  invisible to others). Refine single columns with a least role (`write`), a
  condition (`when: own_row` or `when: assigned_to_viewer` with
  `assigned_field`), `mode: append`, `immutable` or `humans_only`.
- **`humans_only` is for approvals.** An agent can never write that column,
  whatever its role; an agent may add the flag and never remove it.
- **Write small, write blind.** A checkbox click is a `merge` of one field with
  no version guard; concurrent clicks on different cells never conflict. Guard
  with the document's version only where overwriting somebody would be wrong (a
  long text field), and handle a `conflict` answer by showing both values.
- **Batch what belongs together.** One write carries up to 100 ops, all applied
  or none.
- **Expect `pending`.** The dashboard's sheet and a page using the runtime show
  an edit at once and make it solid when the engine's verdict arrives.
- **Mind the bounds**: 64 collections per store, 128 columns per collection,
  50 000 documents per store, 64 KiB per document, and the store's footprint
  under the company's `data_store_bytes`.

Names are restricted: a collection matches `^[a-z][a-z0-9_]{0,47}$`, a document
id `^[A-Za-z0-9_-]{1,128}$` and a field `^[A-Za-z][A-Za-z0-9_]{0,63}$`. A
leading `_` is reserved for the engine, and so are the collection names
`schema`, `history`, `state`, `writes`, `undo` and `export` and the document id
`query` (the data routes' own path segments).

---

## Publishing

**From the dashboard**, create an artifact under Artifacts and upload files or
write Markdown; every save is a version.

**From the CLI**, with a machine token in `CREWLET_API_TOKEN`:

```sh
crewlet artifacts publish ./dist
```

`crewlet artifacts` also lists, reads, updates, versions, restores, labels,
shares, unshares, trashes, untrashes and purges artifacts, manages assets and
reports usage. It always asks for an upload intent first and sends only files
the artifact does not already hold. A directory's dotted names are never
published; leave anything else out with `-exclude` (`-exclude '*.map'
-exclude drafts`). See [CLI](../reference/cli.md#crewlet-artifacts).

**From an agent**, through `create_artifact` and `publish_artifact`. An agent
publishes what it built as inline `content` and `files` (at most 1 MiB per
call) or copies a project's file in with `from_project_file`. A seat's executor
run publishes a larger build from its own box, with `from_sandbox` for a file
and `source: {sandbox}` for a directory, while it works; the engine reads a box
only for the run working in it. It refuses the sources `not_running` to a call
from another seat's turn, or from a run parked on a question, collected or torn
down; `forbidden` to a caller that is not an agent seat; and `unavailable` on a
node that runs no sandbox
([Code Sandbox](../concepts/code-sandbox.md#how-a-coding-task-runs)).
A project's file is copied in as
[`read_project_file`](worklet.md#a-projects-files) reads it, and refused in
that tool's words: `forbidden` to a caller who may not read the project's
files; `invalid` for a project or file that is not there or a path the tracker
refuses (the argument to change); `unavailable` for content the object store
cannot hand back, or `internal_error` where what it holds is not the file (the
copy checks the file's SHA-256 before a byte moves).
Small changes go as `edits` (`old_str` and `new_str` pairs against the base
version), which saves most of a whole-file republish's output. Every publish
names `base_version`; when somebody else published since, the tool answers with
the live version, who published it, when, the changed paths and the start of
each changed file, so the agent merges and publishes again rather than
discarding a colleague's work.

---

## Writing a doc

A doc of one page is one Markdown file, `index.md` by default. A doc of several
tabs is a directory: `doc.json`, the index, naming the Markdown file of each tab
in order, with the tabs and any images beside it.

```json
{ "v": 1, "tabs": [ { "path": "tabs/plan.md", "title": "Plan" },
                    { "path": "tabs/risks.md", "title": "Risks" } ] }
```

- **The index is checked at publish.** `v` is `1`; there are 1 to 32 tabs;
  every tab names a Markdown file the version holds, once, under a title of at
  most 120 bytes; the tabs total at most 16 MiB. Otherwise the publish is
  refused under the code `doc_format`, each finding naming the rule it broke
  (`index`, `reference`, `bound` or `encoding`). A Markdown file the index does
  not name is a file of the version, which a tab may link to, not a tab.
- **Nothing in it runs.** The dashboard draws every tab with its own Markdown
  renderer, so a doc carries no script, no styles and no HTML that becomes
  markup. Images are files of the version, named by path.
- **Publish it** with `crewlet artifacts publish <dir>` (a `doc.json` or a lone
  `.md` file makes it a `doc`); from the dashboard with New › Doc (one tab,
  written on the create page or left to fill later, then edited in place:
  adding, renaming, reordering and removing tabs, each save a version); or from
  an agent with `create_artifact`, `kind: "doc"` and either `content` (one tab)
  or `files` with `entry: "doc.json"`. Revise a tab with `edits` on its file
  against your base version; `get_artifact` with `view: "outline"` lists the
  tabs with file and size, so you read only the tab you change. A patch that
  removes a tab's file must drop the tab from `doc.json` in the same version.

---

## Writing a deck

A deck is a directory: `deck.json`, the index, and `slides/<id>.html` per
slide, with any images and font files beside them. The
[Slide Format](../reference/slide-format.md) reference lists every element,
style and bound; the short version:

```json
{ "title": "Q3 plan", "order": ["cover", "plan"], "cover": "cover",
  "sections": { "s1": { "description": "Where we are", "start": "cover" } } }
```

```html
<section id="cover" style="background:#13233a;color:#f4f1ea;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;justify-content:center">
<h1 style="font-size:120px">Q3 plan</h1>
<aside>Welcome everyone.</aside>
</section>
```

- **One idea per slide** on the 1920 by 1080 stage, with a `background` on
  every section and nothing under 24px.
- **Styles are inline** and from the subset; images and typefaces are files of
  the version named by path (`images/team.png`), never URLs.
- **Publish it** with `crewlet artifacts publish <dir>` (a `deck.json` makes it
  `slides`), or from an agent with `create_artifact`, `kind: "slides"` and
  `files`. In the dashboard, New › Slides starts one from a theme and the
  slides you choose to open with (see
  [Themes and layouts](../reference/slide-format.md#themes-and-layouts)), which
  you edit in place; an agent does the same with `create_artifact`'s `starter`
  (`theme: "brand"` for the company's brand). An agent's deck may name one of
  those themes as `theme` in `deck.json`, so a person adds slides from that
  theme's layouts. Revise one slide at a time with `edits` on its file against
  your base version; `get_artifact` with `view: "outline"` lists the slides
  first.

## Writing a dashboard

A dashboard is `dashboard.json`, with any CSV or JSON data files beside it. The
[Dashboard Spec](../reference/dashboard-spec.md) reference lists every source,
step, widget and bound; the short version:

```json
{ "v": 1, "title": "Delivery",
  "datasets": [
    { "id": "work", "title": "My open work", "source": "engine",
      "engine": { "name": "tracker.items", "params": { "scope": "mine" } } } ],
  "widgets": [
    { "id": "open", "type": "stat", "title": "Open items", "dataset": "work", "agg": "count", "w": 3 },
    { "id": "list", "type": "table", "title": "My work", "dataset": "work", "columns": ["key", "title", "due"], "w": 9 } ] }
```

- **Name the data, never the numbers.** A dataset says where rows come from
  (the artifact's own store, a named engine read, a file of the version, or a
  calculation over another dataset) and the board computes every figure.
- **Every viewer sees their own.** An engine dataset is answered as whoever
  opens the board, under their own rules: `scope: "mine"` is each person's own
  work, and somebody without the tracker sees that the figure is closed to
  them. Publish one board for everybody.
- **Bring outside data through the store.** An agent reads GitHub, Jira or
  Slack with its own tools and writes the rows to a `store` collection; every
  open board redraws as they land.
- **Publish it** with `crewlet artifacts publish <dir>` (a `dashboard.json`
  makes it `dashboard`), or from an agent with `create_artifact`,
  `kind: "dashboard"` and `content`. `crewlet artifacts sources` and
  `list_dashboard_sources` list the engine reads and their parameters.

---

## Writing a design

A design is a directory: `canvas.json`, the entry, and one `.html` file per
artboard, with shared stylesheets, scripts and images beside them. Every
artboard is a whole page held to [the page contract](#the-page-contract) and
linted as an `html` page is: give each its own head, kit links and body
background.

```json
{
  "v": 1,
  "title": "Launch landing page",
  "boards": {
    "hero.html": { "x": 0, "y": 0, "w": 1440, "h": 900, "title": "Hero" },
    "mobile.html": { "x": 1540, "y": 0, "w": 390, "h": 844, "title": "Mobile" }
  },
  "order": ["hero.html", "mobile.html"],
  "notes": [
    { "id": "title-1", "kind": "title", "x": 0, "y": -160, "w": 1200, "h": 100, "text": "Launch landing page" },
    { "id": "note-1", "kind": "sticky", "x": 2040, "y": 0, "w": 320, "h": 240, "text": "Copy by Friday", "color": "pink" }
  ],
  "cover": "hero.html"
}
```

| Field | What it holds |
|---|---|
| `v` | The format version, `1` |
| `title` | The canvas's heading, optional |
| `boards` | One entry per artboard, keyed by its page's path: `x` and `y` (position), `w` and `h` (size, also the width its page lays out for), and an optional `title` (the file name otherwise) |
| `order` | Every board's path once, back to front: the order boards are drawn, listed and printed in |
| `notes` | Titles over groups of boards (`kind: "title"`) and stickies beside them (`kind: "sticky"`), each with an `id`, a frame and its `text`. A sticky may take a `color`: `yellow` (the default), `pink`, `blue`, `green` or `purple`; a title takes none |
| `cover` | The board the gallery's cover titles, optional: the first in `order` otherwise |

- **Every HTML page is a board, and every board an HTML page.** A board naming
  a path the version does not hold as a page, a page no board frames, or a
  field the format does not have is refused at publish, every problem named at
  its place in `canvas.json`.
- **Bounds.** A side is 40 to 8000 pixels, a position at most 1,000,000 pixels
  from the origin, a canvas at most 64 boards and 200 notes, a note's text at
  most 2000 bytes, and a note id 1 to 32 lower-case letters, digits and
  hyphens.
- **Boards never link to each other.** Each board's frame opens that board
  only: a link or fetch from one board to another is answered `404`. Put shared
  behaviour in a shared script.
- **Print is the reader's.** A board that offers a Print button calls
  `crewlet.print()`; the dashboard prints that board alone on a page of its own
  size, on the reader's own click
  ([Artifact Runtime](../reference/artifact-runtime.md)).
- **People rearrange it.** An editor moves and resizes boards and places, edits
  and deletes notes in the dashboard; each save is a version that changes
  `canvas.json` alone. Read the head before you publish, and merge onto a
  layout a person changed rather than restoring your own.
- **Building a design a person laid out.** A design begun on the dashboard's
  New design page already holds the person's boards, each a placeholder page
  under `boards/` stating the board's size and purpose. Read the outline, keep
  each board's path, place and size, and publish each board's real page over
  its placeholder (an `edits` or a `files` publish on that path).
- **Publish it** with `crewlet artifacts publish <dir>` (a `canvas.json` makes
  it a `design`), or from an agent with `create_artifact`, `kind: "design"` and
  `files`. The entry is `canvas.json` whether or not you name it. Revise one
  board at a time with `edits` on its file; `get_artifact` with
  `view: "outline"` lists every board's file, title and frame first.

---

## Render it before review

An agent cannot open its own page: its box holds no sign-in, and a frame opens
only inside the dashboard. So `render_artifact` opens a version of an `html`
page, or one artboard of a `design`, in a headless Chromium the engine drives,
in the very frame a viewer gets: the same ticket rules, runtime, kit, script
policy and brand, the page's data read as the agent, at the widths, themes and
scroll positions asked (by default 1440 and 390 pixels wide, light and dark,
from the top). It can first click, hover or press keys on named elements, to
open a menu or a tab. It answers:

| Finding | What it means |
|---|---|
| Unresolved properties | A `var(--name)` your own CSS uses that no loaded stylesheet defines, with the rule, file and line, how many elements it styles and the nearest defined tokens. Without a fallback every declaration using it is dropped: `background: var(--color-surface-base)` paints nothing, since the kit names that surface `--color-surface-background` |
| See-through bars | A sticky or fixed element at least half the frame wide with a transparent background, and the unresolved property its background asked for, if any |
| Hidden under bars | A heading, link or control a sticky or fixed element covers at the top of the page, or where an in-page link lands |
| Console | Errors, warnings, uncaught exceptions and the script policy's refusals, by file and line |
| Failed requests | A file the version does not hold, a refusal, or a request to another site, which a render never makes |
| Links | An in-page link to no id, a link to a page the version lacks, an external link the frame cannot open (use `crewlet.openLink`), a `javascript:` link |
| Writes | Each write the page tried: a render writes nothing, so they are refused and listed |

The screenshots are PNG files in the agent's own box, under
`~/.crewlet/renders/<artifact>/`, replacing that artifact's earlier ones, where
its harness opens them like any image. A render writes nothing to the artifact:
no open is recorded, nobody sees the agent in the room, the page's data is only
read.

A render is bounded: at most twelve shots, 320 to 1920 pixels wide, ten
actions, ninety seconds and eight renders a turn. It needs a browser the
deployment runs ([Serving artifacts](artifacts-serving.md#the-render-browser)).

---

## Before you share it

- Render it (above), or open it in both light and dark mode, and at phone
  width.
- Try it with storage unavailable, if your deployment serves artifacts from one
  shared host.
- Check the Data tab: every collection should have a schema, and approval
  columns `humans_only`.
- Share with the narrowest audience that needs it. A share can be widened
  later; an agent can never widen one to the whole company.
