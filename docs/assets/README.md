# Brand assets

Illustrations and icons used by the README and the documentation.

| File | Used for |
|---|---|
| `crewlet-icon.svg` | The Crewlet mark — README header, docs header |
| `crewlet-mascot.svg` | The Crewlet character, on its own dark canvas |
| `hierarchy.svg` | Feature icon — the org chart as the execution graph |
| `company-as-code.svg` | Feature icon — the company as versioned config |
| `turn-engine.svg` | Feature icon — the Plan → Execute → Review loop |
| `code-sandbox.svg` | Feature icon — sandboxed code authoring |
| `knowledge.svg` | Feature icon — knowledge and agent learning |
| `human-in-loop.svg` | Feature icon — human seats in the org chart |

These come from the Crewlet design system; the mark, the character, and the
first three feature icons originate there, and the remaining feature icons were
drawn to the same brief.

## Conventions

Feature icons are **80×80**, drawn on a `#111` disc so they read on both light and
dark backgrounds, with a diagonal brand gradient (`#6a5cff` → `#a055ff` → `#e24a90`)
and opacity tiers for depth.

Gradients must use `gradientUnits="userSpaceOnUse"` spanning the 80×80 canvas.
With the default `objectBoundingBox` units, any horizontal or vertical stroke has a
zero-area bounding box, which makes the gradient degenerate — the shape then does
not paint at all.

The character artwork carries near-white details, so it only reads on a dark
surface; `crewlet-mascot.svg` therefore embeds its own dark rounded canvas rather
than relying on the page background.

## Optimizing the traced artwork

`crewlet-mascot.svg` and `crewlet-icon.svg` are traced from raster art, so they carry
far more path data than they need. The mascot is minified (`svgo`, `floatPrecision: 0`)
— verified against the original with a difference blend: the only deltas are
sub-pixel anti-aliasing along edges.

The mark is **not** minified. At the same aggressive settings `svgo` drops geometry
from it (the lower half of the figure disappears), and at 4.6 KB there is nothing
worth reclaiming. If you re-optimize either file, diff the render against the
original before committing — a size win that silently deletes a limb is easy to miss
in a side-by-side.
