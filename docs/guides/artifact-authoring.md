# Authoring Artifacts

How to write an [artifact](../concepts/artifacts.md) that publishes cleanly,
opens fast and works for everybody it is shared with, whether a person writes
it, the CLI publishes it or an agent publishes it with its tools. The rules
here are the ones the engine enforces at publish; the page's runtime API is the
[artifact runtime reference](../reference/artifact-runtime.md).

---

## Choosing a kind

| You want | Kind |
|---|---|
| A table people fill in together: a review, a sign-up, a tracker of decisions | `sheet`. Declare the schema and the dashboard draws it, with presence, per-cell history and accessibility, and no page code at all |
| A document: a report, a plan, release notes, a handbook in tabs | `doc`, Markdown in one file or in tabs. The dashboard renders it; there is nothing to run. See [Writing a doc](#writing-a-doc) |
| A presentation: a pitch, a review, a talk | `slides`, a deck in Claude's slide format. The dashboard draws, presents and prints it, people fix text and order it in place, and nothing runs. See [Writing a deck](#writing-a-deck) |
| Several screens reviewed side by side: a landing page's sections, an onboarding flow, three takes on one hero | `design`, a canvas of artboards, each a plain HTML page. People rearrange the boards and annotate them in place. See [Writing a design](#writing-a-design) |
| A dashboard: figures over the company's work, spend and agents, or over data an agent collects | `dashboard`, a closed spec of datasets and widgets the dashboard draws, every figure answered as its viewer and citing its source. See [Writing a dashboard](#writing-a-dashboard) |
| A calculator, a custom view over shared data, anything the formats above cannot draw | `html`, with the capabilities it needs |

Start from a `sheet` whenever the content is rows and columns. A native sheet
leaves nothing for a model to get wrong, and an `html` page can still read and
write the same kind of store when it needs a custom view.

---

## The page contract

An `html` version is a set of files with one **entry** (usually `index.html`).
The engine refuses a publish that breaks a rule below, naming the file and the
rule, and warns on success about what is legal but likely wrong.

**Files and paths**

- Paths are relative, with no leading slash, no `.` or `..` segment, no
  backslash and no control character, at most 1024 bytes. Reference your own
  files the same way (`app.js`, `img/logo.svg`), never with a leading slash:
  every version is served under a path the page never sees.
- A file's served type comes from its extension, from the engine's one media
  table (`internal/artifacts/mediatype`). Use web formats: HTML, CSS,
  JavaScript, JSON, SVG, the common image, font, audio and video formats,
  plain text, CSV, PDF and WebAssembly. An extension the table does not know is
  refused, and a client can never choose the served type.
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
  engine marks every script element at publish and gives each served document
  a fresh nonce. Do not write a nonce yourself.
- Inline event handlers (`onclick="..."`) work: their digests are recorded at
  publish and admitted by hash. Prefer `addEventListener`; a handler added to
  the DOM after load is not one the policy knows.
- Scripts a page loads from its own scripts run too (the policy is
  `'strict-dynamic'`), so a bundler's chunk loader works.
- A `<script src>` or stylesheet `<link>` on another host must be on the CDN
  allowlist the publish linter holds, at an **exact** version (`react@18.3.1`,
  never `react` or `react@18`). The engine copies each such file into your
  version at publish, checking its integrity, and rewrites the reference to
  the copy; viewers never reach the CDN. Any other remote reference is
  refused: upload the file instead.
- An ES module imports another origin only through an **import map**
  (`<script type="importmap">`), never by a literal URL in an `import`. Each
  map entry naming another origin is held to the rule above and rewritten to
  the version's copy. The copied module must import nothing itself, so map a
  self-contained build, and an entry mapping a whole folder (`"lodash/":
  "https://..."`) is refused, since a folder cannot be copied in.
- `eval`, `new Function` and WebAssembly compilation are admitted only for a
  page the linter finds needs them (an in-browser transform, a `.wasm`
  module); every other page runs without them.

**What a page cannot do**

- Reach the network beyond its own files: `fetch`, images and media load only
  from the artifact's own origin. Live data comes through the runtime.
- Open a window or a popup. Use `crewlet.openLink(url)`: the dashboard shows
  the viewer where the link goes, then opens it.
- Submit a form anywhere. A form's `submit` event fires; handle it in script.
- Redirect off its origin with `<meta http-equiv="refresh">` (refused at
  publish), embed another page in a frame, or use plugins.
- Rely on browser storage. On a deployment that serves artifacts from one
  shared host, `localStorage`, `sessionStorage` and IndexedDB throw. Wrap every
  access in `try`/`catch` and render correctly without it, or declare `state`
  and keep per-viewer state through the runtime (it follows the viewer across
  devices, which browser storage never did).

**What the linter warns about**

Browser storage used for state other viewers should see (use the datastore),
no `<meta name="viewport">`, and a version that ignores its reader's
appearance (artifacts follow each reader's own appearance setting). A version
follows it when its pages link both of [the kit's](#capabilities-and-the-runtime)
theming files, `kit.css` and `theme.js`, at a release the engine serves, or
when a page or stylesheet of its own carries a `prefers-color-scheme` rule.
`kit.css` alone is warned (it follows the device, not the reader's chosen
mode), as is `theme.js` alone (it applies a mode nothing styles). A link to a
kit release the engine does not serve is warned too, naming the newest one it
does: the origin answers it with a 404, so the page loads no kit at all. So is
a link to a file the named release does not ship, such as `content.css` at a
release older than it: that one link is a 404, and the warning names the
same file at the newest release when that release ships it.

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

A publish declaring a capability this build does not know is refused. A
namespace the version did not declare still exists on `window.crewlet`, and
every call on it rejects with `capability_not_declared`, so
`crewlet.capabilities.has("presence")` is how a page detects one.

**The runtime contract.** A version pins the runtime it was written against:
pass `contract: "latest"` to take the newest this engine serves (stored as the
exact version), an exact version to pin one, or nothing to keep the artifact's
current pin. You never include the runtime yourself; the engine serves it from
`/_crewlet/runtime/<contract>/` on the artifact's own origin, and it installs
`window.crewlet`. See the [runtime reference](../reference/artifact-runtime.md).

**The kit.** The artifact kit is the company's design system for pages:
`kit.css` (tokens and a bare base that follow the reader's theme),
`content.css` (the design system's look for the plain HTML a page writes
itself), `theme.js` (a few hundred bytes that apply the reader's light or
dark mode before first paint) and `kit.js` (custom elements for a sheet bound
to the datastore and presence, used without a framework). It is served from
`/_crewlet/kit/<version>/` on the artifact origin:

```html
<link rel="stylesheet" href="/_crewlet/kit/<version>/kit.css">
<link rel="stylesheet" href="/_crewlet/kit/<version>/content.css">
<script src="/_crewlet/kit/<version>/theme.js"></script>
<script type="module" src="/_crewlet/kit/<version>/kit.js"></script>
```

Link the newest version this engine serves, the one that reads the company's
[brand](#your-companys-brand). Nothing here names it, because it moves with the
engine: a seat's artifacts skill carries it as `${crewlet_kit_version}`, which
the engine fills in from the kit it embeds, `get_company_brand` answers it as
`kit_version`, and the publish warning on a release the engine does not serve
names it. The version in the path is a pin: the engine keeps serving every kit
version it has shipped, so a page published against an older kit keeps the
exact files it was written against when the engine moves to a newer one. To
take a newer kit, publish a new version of the page that links it.

Colours, spacing and type come from the kit's tokens, never literal values,
so a page looks like the rest of the company in both themes.

**Plain HTML.** Link `content.css` right after `kit.css` whenever the page
writes its own headings, paragraphs, lists, quotes, code, tables, rules or
form controls. Without it they render on the kit's bare base: headings at one
size, no space between blocks, and buttons as plain text. With it they take
the dashboard's document and form look in both themes:

- It styles elements only, each rule at the weight of its element, so a class
  of your own overrides any of it. Link it before your own `<style>`.
- It never touches what the kit draws (a `<crewlet-sheet>` and its menus).
  A region you design entirely yourself opts out with
  `data-crewlet-content="off"`.
- Wrap a table in a `<div>` or `<figure>`: a table wider than a phone scrolls
  inside it rather than moving the page. Mark a column of figures
  `data-align="right"` (or `"center"`), which also keeps each figure on one
  line.
- A `<button type="submit">` is the primary action; every other button is
  secondary.

It is opt in: a page that does not link it, every page published before it
existed included, paints exactly as before.

### Your company's brand

When an administrator has adopted a brand (Company, Brand), the artifact
origin links its generated stylesheet beside the kit on every page and design,
pinned to the brand current when the version was published. The stylesheet
sets only the kit's brand inputs, so a page written with the kit's tokens
(`var(--color-brand-accent)`, `var(--font-family-display)`) takes the brand's
colours and typefaces in both themes without a line of its own; a page with
colour literals, or linked to a kit older than the one that reads the brand,
does not. Docs and dashboards follow the adopted brand as the dashboard draws
them.

- **A logo** is the kit's markup, never a URL:
  `<span class="crewlet-logo" role="img" aria-label="Acme"></span>`, with
  `data-variant="on-dark"` for a dark band or `"mark"` for a square space. It
  draws the brand's own logo and takes no room where the brand has none.
- **A file of the brand in a version** (a deck's logo or typeface, a doc's
  image) is copied from the adoption, never redrawn: an agent names it with
  `{from_brand: "logo.primary"}` in `files` (`logo.on_dark`, `logo.mark`,
  `font.heading`, `font.body`), and `get_company_brand` lists each with the
  path to copy it to.
- **A deck** begun in New, Slides or with an agent's `starter` in the theme
  `brand` is drawn in the brand once: its colours are inline, its logo and
  typefaces are files of the version, and a later adoption changes nothing in
  it.
- **A page that must not take the brand**, such as a mock of somebody else's
  screen, sets `<html data-crewlet-brand="off">`.

---

## Shared data

Declare `db`, then describe the data as **collections** with a schema, so the
engine validates every write and the dashboard can show the data in its Data
tab even when your page is broken.

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
  invisible to everybody else). Refine single columns with a least role
  (`write`), a condition (`when: own_row` or `when: assigned_to_viewer` with
  `assigned_field`), `mode: append`, `immutable` or `humans_only`.
- **`humans_only` is for approvals.** An agent can never write that column,
  whatever its role; an agent may add the flag and never remove it.
- **Write small, write blind.** A click on a checkbox is a `merge` of one field
  with no version guard, and that is right: concurrent clicks on different
  cells never conflict. Guard with the document's version only where
  overwriting somebody would be wrong (a long text field), and handle a
  `conflict` answer by showing both values.
- **Batch what belongs together.** One write carries up to 100 ops, all
  applied or none.
- **Expect `pending`.** The dashboard's sheet, and a page using the runtime,
  show an edit at once and make it solid when the engine's verdict arrives.
- **Mind the bounds**: 64 collections per store, 128 columns per collection,
  50 000 documents per store, 64 KiB per document, and the store's footprint
  under the company's `data_store_bytes`.

The store's names are restricted: a collection matches `^[a-z][a-z0-9_]{0,47}$`,
a document id `^[A-Za-z0-9_-]{1,128}$` and a field
`^[A-Za-z][A-Za-z0-9_]{0,63}$`. A leading `_` is reserved for the engine, and
so are the collection names `schema`, `history`, `state`, `writes`, `undo` and
`export` and the document id `query`, which the data routes' own path segments
take.

---

## Publishing

**From the dashboard**, create an artifact under Artifacts and upload files or
write Markdown; every save is a version.

**From the CLI**, with a machine token in `CREWLET_API_TOKEN`:

```sh
crewlet artifacts publish ./dist
```

`crewlet artifacts` also lists, reads, updates, versions, restores, labels, shares,
unshares, trashes, untrashes and purges artifacts, manages assets and reports
usage. It always asks for an upload intent first and sends only files the
artifact does not already hold. A directory's dotted names are never
published; leave anything else out with `-exclude` (`-exclude '*.map'
-exclude drafts`). See [CLI](../reference/cli.md#crewlet-artifacts).

**From an agent**, through `create_artifact` and `publish_artifact`. An agent
publishes what it built as inline `content` and `files` (at most 1 MiB per
call) or copies a project's file in with `from_project_file`; the box sources,
`from_sandbox` and `source: {sandbox}`, are read only for a turn whose run's
completion the engine has claimed for its resume, so a seat's executor run
calling them while it works is refused
([Code Sandbox](../concepts/code-sandbox.md#durability-completion-and-observability)).
Small changes go as `edits` (`old_str` and `new_str` pairs against the base
version), which saves most of the output a whole-file republish would cost. Every publish names `base_version`; when somebody else
published since, the tool answers with the live version, who published it,
when, the changed paths and the start of each changed file, so the agent
merges and publishes again rather than discarding a colleague's work.

---

## Writing a doc

A doc of one page is one Markdown file, `index.md` by default. A doc of
several tabs is a directory: `doc.json`, the index, naming the Markdown file
of each tab in order, with the tabs and any images beside it.

```json
{ "v": 1, "tabs": [ { "path": "tabs/plan.md", "title": "Plan" },
                    { "path": "tabs/risks.md", "title": "Risks" } ] }
```

- **The index is checked at publish.** `v` is `1`; there are 1 to 32 tabs;
  every tab names a Markdown file the version holds, once, under a title of
  at most 120 bytes; and the tabs total at most 16 MiB. A publish outside
  that is refused under the code `doc_format`, each finding naming the rule
  it broke (`index`, `reference`, `bound` or `encoding`). A Markdown file the
  index does not name is a file of the version, which a tab may link to, and
  not a tab.
- **Nothing in it runs.** The dashboard draws every tab with its own Markdown
  renderer, so a doc carries no script, no styles and no HTML that becomes
  markup. Images are files of the version, named by their path.
- **Publish it** with `crewlet artifacts publish <dir>` (a `doc.json` or a
  lone `.md` file makes it a `doc`), from the dashboard with New › Doc (one tab,
  written on the create page or left to fill later, that you then edit in
  place, adding, renaming, reordering and removing tabs, each save a version), or from an agent with
  `create_artifact`,
  `kind: "doc"` and either `content` (one tab) or `files` with
  `entry: "doc.json"`. Revise a tab with `edits` on its file against your base
  version; `get_artifact` with `view: "outline"` lists the tabs, each with its
  file and size, so you read only the tab you change. A patch that removes a
  tab's file must drop the tab from `doc.json` in the same version.

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
- **Styles are inline** and from the subset; images and typefaces are files
  of the version named by their path (`images/team.png`), never URLs.
- **Publish it** with `crewlet artifacts publish <dir>` (a `deck.json` makes it
  `slides`), or from an agent with `create_artifact`, `kind: "slides"` and
  `files`. In the dashboard, New › Slides starts one from a theme and the
  slides you choose to open with (see
  [Themes and layouts](../reference/slide-format.md#themes-and-layouts)), which
  you edit in place; an agent starts one the same way with `create_artifact`'s
  `starter` (`theme: "brand"` for the company's brand). An agent's deck may name
  one of those themes as `theme` in `deck.json`, so a person adds slides to it
  from that theme's layouts. Revise one slide at a time with `edits` on its file against your
  base version; `get_artifact` with `view: "outline"` lists the slides first.

## Writing a dashboard

A dashboard is `dashboard.json`, with any CSV or JSON data files beside it.
The [Dashboard Spec](../reference/dashboard-spec.md) reference lists every
source, step, widget and bound; the short version:

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
  opens the board, under the rules they already have: `scope: "mine"` is each
  person's own work, and somebody without the tracker sees that the figure is
  closed to them. Publish one board for everybody.
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
linted exactly as an `html` page is: give each its own head, kit links and
body background.

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
| `boards` | One entry per artboard, keyed by its page's path: `x` and `y` (where it sits), `w` and `h` (its size, which is also the width its page lays itself out for), and an optional `title` (the page's file name otherwise) |
| `order` | Every board's path once, back to front: the order boards are drawn, listed and printed in |
| `notes` | Titles laid over groups of boards (`kind: "title"`) and stickies beside them (`kind: "sticky"`), each with an `id`, a frame and its `text`. A sticky may take a `color`: `yellow` (the default), `pink`, `blue`, `green` or `purple`; a title takes none |
| `cover` | The board the gallery's cover titles, optional: the first in `order` otherwise |

- **Every HTML page is a board, and every board an HTML page.** A board naming
  a path the version does not hold as a page, or a page no board frames, is
  refused at publish, with every problem named at its place in `canvas.json`;
  so is a field the format does not have.
- **Bounds.** A side is 40 to 8000 pixels, a position at most 1,000,000 pixels
  from the origin, a canvas at most 64 boards and 200 notes, a note's text at
  most 2000 bytes, and a note id 1 to 32 lower-case letters, digits and
  hyphens.
- **Boards never link to each other.** Each board's frame opens that board and
  no other page of the version: a link or a fetch from one board to another is
  answered `404`. Put shared behaviour in a shared script.
- **Print is the reader's.** A board that offers a Print button calls
  `crewlet.print()`; the dashboard prints that board alone on a page of its
  own size, on the reader's own click
  ([Artifact Runtime](../reference/artifact-runtime.md)).
- **People rearrange it.** An editor moves and resizes boards and places,
  edits and deletes notes in the dashboard, and each save is a version that changes `canvas.json`
  alone. Read the head before you publish, and merge onto a layout a person
  changed rather than restoring your own.
- **Building a design a person laid out.** A design begun on the dashboard's
  New design page already holds the person's boards, each a placeholder page
  under `boards/` that says the board's size and what it is for. Read the
  outline, keep each board's path, place and size, and publish each board's
  real page over its placeholder (an `edits` or a `files` publish on that
  path), so the canvas the person drew is the one that fills in.
- **Publish it** with `crewlet artifacts publish <dir>` (a `canvas.json` makes
  it a `design`), or from an agent with `create_artifact`, `kind: "design"`
  and `files`. The entry is `canvas.json` whether or not you name it. Revise
  one board at a time with `edits` on its file; `get_artifact` with
  `view: "outline"` lists every board's file, title and frame first.

---

## Before you share it

- Open it in both light and dark mode, and at phone width.
- Try it with storage unavailable, if your deployment serves artifacts from
  one shared host.
- Check the Data tab: every collection should have a schema, and approval
  columns `humans_only`.
- Share with the narrowest audience that needs it. A share can be widened
  later; an agent can never widen one to the whole company.
