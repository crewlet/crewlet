# Authoring Artifacts

How to write an [artifact](../concepts/artifacts.md) that publishes cleanly,
opens fast and works for everybody it is shared with, whether a person writes
it, the CLI publishes it or an agent builds it in a sandbox. The rules here are
the ones the engine enforces at publish; the page's runtime API is the
[artifact runtime reference](../reference/artifact-runtime.md).

---

## Choosing a kind

| You want | Kind |
|---|---|
| A table people fill in together: a review, a sign-up, a tracker of decisions | `sheet`. Declare the schema and the dashboard draws it, with presence, per-cell history and accessibility, and no page code at all |
| A document: a report, a plan, release notes | `markdown`. The dashboard renders it; there is nothing to run |
| A dashboard, a calculator, a custom view over shared data | `html`, with the capabilities it needs |

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
does: the origin answers it with a 404, so the page loads no kit at all.

---

## Capabilities and the runtime

A version declares what its page may ask the host for, and a viewer's ticket
grants it:

| Capability | Lets the page |
|---|---|
| `db` | Read and write the artifact's shared datastore, under the collection rules |
| `state` | Keep per-viewer state (a remembered filter, a collapsed panel) |
| `identity` | Learn who is viewing: their seat, kind and role on this artifact. Never a token or an address |
| `presence` | See who else is here and say where the viewer is |
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
`kit.css` (tokens and base styles that follow the reader's theme),
`theme.js` (a few hundred bytes that apply the reader's light or dark mode
before first paint) and `kit.js` (custom elements for a sheet bound to the
datastore and presence, used without a framework). It is served from
`/_crewlet/kit/<version>/` on the artifact origin:

```html
<link rel="stylesheet" href="/_crewlet/kit/0.20.0/kit.css">
<script src="/_crewlet/kit/0.20.0/theme.js"></script>
<script type="module" src="/_crewlet/kit/0.20.0/kit.js"></script>
```

Link the version shown here, which is the newest this engine serves. The
version in the path is a pin: the engine keeps serving every kit version it
has shipped, so a page published against an older kit keeps the exact files
it was written against when the engine moves to a newer one. To take a newer
kit, publish a new version of the page that links it.

Colours, spacing and type come from the kit's tokens, never literal values,
so a page looks like the rest of the company in both themes.

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
building in a sandbox passes `from_sandbox` and the engine reads the files out
of the box itself; small changes go as `edits` (`old_str` and `new_str` pairs
against the base version), which saves most of the output a whole-file
republish would cost. Every publish names `base_version`; when somebody else
published since, the tool answers with the live version, who published it,
when, the changed paths and the start of each changed file, so the agent
merges and publishes again rather than discarding a colleague's work.

---

## Before you share it

- Open it in both light and dark mode, and at phone width.
- Try it with storage unavailable, if your deployment serves artifacts from
  one shared host.
- Check the Data tab: every collection should have a schema, and approval
  columns `humans_only`.
- Share with the narrowest audience that needs it. A share can be widened
  later; an agent can never widen one to the whole company.
