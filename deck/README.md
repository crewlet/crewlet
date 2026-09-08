# Deck

`index.html` is the fundraising deck — a single self-contained HTML file, 22
slides, no build step and no dependencies beyond two webfont requests.

Open it in a browser. Arrow keys, space, `Home` and `End` move between slides;
`P` opens the print dialog, which lays the deck out one slide per landscape page
so it exports to PDF for anyone who wants a file rather than a link.

## What it is built from

Everything on it is sourced. Mechanism claims carry the file in this repository
that implements them, in the monospace line under the slide; market claims carry
the publication and its date. That is the deck's one structural device — a claim
we cannot point at is a claim the deck does not make.

The palette, the type pairing and the "colour carries state, never identity"
discipline come from the dashboard's own design system
(`dashboard/src/styles/tokens.css`), so the deck and the product read as one
thing. The deck commits to a single dark theme rather than following the
viewer's: a presenter should not have their slides change with an OS setting.

## Keeping it honest

Slide 21 is the two-column ledger of what is built and what is not, at equal
visual weight. It is load-bearing. Anything that ships moves from the right
column to the left; anything the deck starts claiming that is not yet true has
to appear on the right. Figures on slides 4, 5, 14, 15 and 20 are dated —
re-check them before a meeting, and re-check the competitor row, which was
verified 2026-09-08.
