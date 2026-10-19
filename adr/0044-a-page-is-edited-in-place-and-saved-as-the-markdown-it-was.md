# ADR-0044: A page is edited in place, and saved as the Markdown it was

- **Status:** accepted
- **Authority:** `docs/reference/dashboard-design.md`
- **Enforced-by:** `internal/api.TestTheDashboardFitsItsBudget` (the editor is a chunk of its own, under the lazy budget), and the dashboard's `src/lib/richEditor.test.tsx` (every Markdown body in this repository, loaded and saved with no edit, byte for byte, and rewritten whole, read the same) and `src/app/trustedTypes.test.ts`, with the design system's own suites over the editor
- **Measured:** each candidate as the lazy chunk an edit would fetch, built with every package it needs (minified, then gzip at level 9): ProseMirror's packages 228 KB and 71 KB; TipTap with its starter kit, Markdown and tables 530 KB and 167 KB; Lexical with rich text, lists, tables, code, links, history and Markdown 440 KB and 142 KB; Milkdown with CommonMark, GFM, history and its clipboard 463 KB and 142 KB. The editor as built is 270 KB and 85 KB against a 150 KiB budget per chunk, and the 138 Markdown bodies of this repository round-trip unchanged
- **Cost-when-tried:** the Markdown source editor these surfaces had (a textarea, a formatting bar and a Preview tab) made every page look like its source while it was edited, and the people writing pages found it old-fashioned. Every rich editor considered reads pasted HTML by assigning it to `innerHTML` behind a pass-through Trusted Types policy of its own (ProseMirror's `ProseMirrorClipboard`, Lexical's and TipTap's `DOMParser` paths); under the dashboard's `trusted-types 'none'` that throws on the first paste, and a policy that allowed it would be the hole the header exists to close
- **Tag-status:** unreleased

## The decision

**A Knowledge page and a doc's tab are edited where they are read**, in the
design system's `RichEditor`, which is ProseMirror drawn as `Document`: the
page looks the same while it is edited as when it is read, with a slash menu,
a selection toolbar, Markdown typed as Markdown, and the bar's Edit becoming
the editing state, Cancel and Save. There is no preview pane, because there is
nothing for one to show.

**The record stays Markdown, and a save with no edit changes no byte.** Every
top-level block keeps the lines it was read from and is written back as them
until it is edited; a changed block is written in the spelling it was read
with and checked by reading it back. A page's three-way merge and a doc's
version patch therefore see only the lines a writer changed. The grammar is
one, `@crewlethq/ui`'s, read by the dashboard's renderer and by the editor
alike, so the two cannot disagree about a body.

**No markup is parsed through a sink.** The editor takes paste and drop
itself, reads HTML with a tokenizer that builds an inert tree element by
element, and never reaches ProseMirror's own HTML path; the dashboard keeps
enforcing Trusted Types with no policy.

**It is fetched only when somebody edits.** `RichEditor` is a shell that
loads the editor with `import()`, so no reader's first paint pays for it. The
editor raised the dashboard's tree budget to 4 MiB; ProseMirror was chosen
over TipTap, Lexical and Milkdown because it is the smallest of the four by
half, every one of them carries the same `innerHTML` paste path to take over,
and the two that write Markdown themselves (Lexical's transformers, Milkdown's
remark) normalise a body's spelling on every save, which the merge cannot
afford. All four are MIT.
