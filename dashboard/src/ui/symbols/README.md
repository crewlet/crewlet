# Material Symbols the design system has not vendored yet

`@crewlethq/icons` vendors 101 Material Symbols and draws all of them. Three
marks this dashboard needs are not among them, and each names something the
product has and the design system's own set has no word for:

| File              | What draws it here                                                    | Why no vendored glyph fits                                                                                                                                  |
| ----------------- | --------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `star.svg`        | Starred: the page bar's keep toggle, and the sidebar section it fills | Nothing in the vendored set means "kept". `flag` is the nearest and means something else — a thing marked for attention, not a thing you chose to keep      |
| `bug_report.svg`  | The `bug` work type, in `lib/work.ts`'s `TYPE_ICON`                   | A defect is not a live failure, so `error` and `warning` both say the wrong thing about a filed item                                                        |
| `view_column.svg` | A board, and the saved views that open one                            | `dashboard` is a pane grid meaning a landing screen; borrowing it would give one drawing two meanings, which is the mistake a per-type mark exists to avoid |

## This directory should not exist

It is a gap in `@crewlethq/icons`, not a decision. All three are ordinary
Material Symbols at the commit that package already pins, and its own README
documents the fix as one command in that repository:

```
node scripts/vendor-symbols.mjs star bug_report view_column
```

Once they ship there, `src/ui/glyph.tsx`'s `LOCAL` table empties, `MarkName`
collapses into `GlyphName`, and this directory is deleted. The file names here
are upstream's own so that change is a deletion rather than a rename.

## Provenance

Copied byte for byte from the `master` branch of `google/material-design-icons`
at commit
[`40a7a29`](https://github.com/google/material-design-icons/commit/40a7a292a79d9394157e1ea24f83d52d5e17c556)
(11 September 2026) — the same commit `@crewlethq/icons` pins, so these drawings
and the vendored ones are from one revision and cannot drift apart. The upstream
path of each file follows from its name: `24/star.svg` is
`symbols/web/star/materialsymbolsoutlined/star_24px.svg`.

Every file is `<svg xmlns height viewBox width><path d="..."/></svg>` on the
`0 -960 960 960` viewBox, which is the shape `@crewlethq/icons`' own build
refuses anything else in.

`SHA256SUMS` holds the checksum of every file, and `shasum -a 256 -c SHA256SUMS`
verifies them from this directory exactly as it does upstream.

`../symbols.test.ts` runs in `make dashboard-test`, and it checks the half a
checksum cannot: `glyph.tsx` carries this path data INLINE, so a drawing can be
changed without touching a file here at all. It asserts that every vendored file
is drawn, that nothing is drawn that is not vendored, and that each inline path
is byte-identical to the file it came from — plus the checksums, the `0 -960 960
960` shape, and that the glob reached anything at all.

## License

The drawings are under the Apache License, Version 2.0. `LICENSE` holds the
license text, which section 4(a) requires to travel with every copy, and
`NOTICE` the attribution. The rest of the dashboard stays MIT.
