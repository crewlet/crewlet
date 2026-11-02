# Slide Format

A `slides` artifact is a deck: an index and one file per slide, in **Claude's
slide format** without its `project/` prefix, so a deck moves between the two
and an agent that writes one writes the other. The engine holds every version
of a deck to this format when it is published (`internal/artifacts/slideformat`)
and refuses one outside it, naming the file, the line and the rule broken; the
dashboard draws, presents and prints the deck from the canonical tree the same
parser produces, and never parses a slide's markup itself.

Everything below is a closed set. What is not listed is refused, never
silently dropped: a deck that publishes is a deck that draws.

- [The files](#the-files)
- [The index](#the-index)
- [A slide](#a-slide)
- [Elements](#elements)
- [Styles](#styles)
- [Inline drawings](#inline-drawings)
- [Images and typefaces](#images-and-typefaces)
- [Bounds](#bounds)
- [What is refused, and what to write instead](#what-is-refused-and-what-to-write-instead)
- [Themes and layouts](#themes-and-layouts)
- [How a deck is drawn](#how-a-deck-is-drawn)

## The files

| Path | What it is |
|---|---|
| `deck.json` | The index, and the version's entry. A deck's kind is told by this path: `crewlet artifacts publish` infers `slides` from a directory holding it |
| `slides/<id>.html` | One slide. The id is 1 to 64 letters, digits, `_` and `-`, and is the slide's name in the index, the file and its section |
| Any other path | Images and font files the slides and the index name, by their path from the deck's root (`images/team.png`, `fonts/inter.woff2`) |

A deck holds HTML only as its slides: every `slides/<id>.html` is in the
index's `order`, every id in `order` has its file, and no other `.html` file
is accepted.

## The index

```json
{
  "title": "Quarterly plan",
  "order": ["cover", "plan", "numbers"],
  "cover": "cover",
  "sections": {
    "s1": { "description": "Where we are", "start": "cover" },
    "s2": { "description": "What we will do", "start": "plan" }
  },
  "faces": {
    "source-serif-4": { "family": "Source Serif 4", "src": "fonts/source-serif-4.woff2" }
  }
}
```

| Key | Rule |
|---|---|
| `title` | Required, at most 120 bytes |
| `order` | Required: the slide ids in deck order, each once, 1 to 500 |
| `cover` | Optional: the slide the gallery draws as the card's cover, else the first |
| `sections` | Optional: the outline, any key to a run's one sentence (at most 300 bytes) and the slide it starts at. No two start at one slide |
| `theme` | Optional: the look the deck began in, one of `nightshade`, `ink`, `paper`, `signal`, `studio`, `grove`, or `brand` (the company's adopted brand). Crewlet's own key, written by the starter; the slides hold their own styles, so a deck naming none draws the same. The deck editor adds a slide from that theme's layouts, and duplicates a slide in a deck naming none. See [Themes and layouts](#themes-and-layouts) |
| `faces` | Optional, at most 4: a typeface keyed by its family lowercased with spaces as `-`, its `family` and the font file of the version it loads from (`src`). See [Images and typefaces](#images-and-typefaces) |
| `v`, `createdOnFiles`, `lists` | Claude's own bookkeeping: accepted and kept as written, never read. `lists` may only be `css` |
| `designSystems` | Must be empty or absent: a Crewlet deck installs no design system |

Any other key is refused, so a misspelt key is an error rather than a setting
that does nothing.

## A slide

A slide file is exactly one `<section>`, with nothing before or after it but
whitespace and comments: no doctype, `<html>`, `<head>` or `<body>`. Every
element is closed explicitly (`<br>`, `<img>` and `<hr>` take no closing tag,
and inside a drawing an empty element may close itself, `<rect/>`).

```html
<section id="plan" data-transition="fade" style="background:#fbfbf8;color:#1a1a1a;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;gap:48px">
<h2 style="font-size:72px;font-weight:400;line-height:1.1">The plan</h2>
<div style="display:flex;gap:32px">
<p style="flex:1;font-size:32px">Every request lands in one place.</p>
<img src="images/team.png" alt="The support team" style="width:480px;height:360px;object-fit:cover;border-radius:16px">
</div>
<p style="position:absolute;left:128px;bottom:64px;width:800px;font-size:24px;color:#6a7179">Source: support desk</p>
<aside>Take the questions first.</aside>
</section>
```

- **The stage is 1920 by 1080 CSS pixels**, 16:9, laid out at that size and
  scaled as a whole wherever it is drawn, so a thumbnail, the editor and a
  projector show the same line breaks.
- **The section** carries `id` (the slide's id, its file's name), `style`
  (which must set a `background`, so the slide reads the same in every theme),
  and optionally `hidden` (Present skips it, Print leaves it out) and
  `data-transition` (`fade` or `push`: how the slide leaves the stage in
  Present).
- **Layout.** A section or `<div>` that names no `display` lays its children
  out as a column; `display:flex` is a row, `display:grid` a grid. Children
  flow; `position:absolute` with `left`, `top`, `right`, `bottom`, `width`
  and `height` pins one to the slide (or to a `position:relative` `<div>`).
  Later children paint over earlier ones: there is no `z-index`.
- **Defaults.** `h1`, `h2`, `h3` are 96, 64 and 44px at weight 600; text
  elsewhere inherits from its container, 32px by default. Set sizes yourself:
  nothing under 24px reads from the back of a room.
- **Speaker notes** are plain text in one `<aside>`, the section's last child,
  at most 4,000 characters. Everyone who can open the deck can read them.

## Elements

| Element | Holds | Attributes |
|---|---|---|
| `section` | blocks, then one `aside` | `id`, `style`, `hidden`, `data-transition` |
| `div` | blocks (at most 15 nested) | `style`, `data-text` (a text box; its value is a list indent in px, 0 to 200) |
| `h1` `h2` `h3` `p` | text and inline marks | `style` |
| `ul` `ol` | `li` only (one level) | `style` |
| `li` | text and inline marks | `style` |
| `b` `i` `u` `span` | text and inline marks | `span`: `style` |
| `a` | text and `b`, `i`, `u`, `span`, `br` | `href`, an `https://` address |
| `br` | nothing | none |
| `img` | nothing | `src` and `alt` (both required; `alt=""` for decoration), `style` |
| `table` | `tr` only | `style` |
| `tr` | `th` and `td` | `style` (`background` only) |
| `th` `td` | text and inline marks | `style` |
| `hr` | nothing | `style` |
| `svg` | a drawing | `aria-label` (required: what it shows), drawing attributes, `style` |
| `x-shape` | nothing | `kind`: `rect`, `rounded`, `ellipse`, `diamond`, `arrow-right`, `arrow-left`, `arrow-up`, `arrow-down`, `line`; `style` |
| `x-icon` | nothing | `name`, one of `Activity`, `Book`, `Chart`, `Chat`, `Check`, `CheckCircle`, `Clock`, `Code`, `Database`, `Globe`, `Home`, `Key`, `Lightning`, `Link`, `Lock`, `PaperPlane`, `Search`, `Settings`, `Star`, `Tool`, `Trust`, `Users`, `Warning`, `Wrench`; `style` |
| `aside` | plain text | none |

A block is `h1` to `h3`, `p`, `ul`, `ol`, `div`, `img`, `table`, `hr`, `svg`,
`x-shape` and `x-icon`. Text sits in a text element, never directly in a
`section` or `div`.

An `x-icon` draws its glyph at half its box, centred, as Claude's format
does; the rest of the box is padding. A `width:64px; height:64px` icon draws
a 32px glyph, so size the box at twice the glyph you want: at least 32px, and
48px beside 24px body text.

## Styles

Every style is inline, in `style`, from the table below, and only on the
elements its row names. A value is read by its property's grammar and stored
in one canonical form: a bare number is pixels (`width:480` is `480px`), a
point size becomes pixels (`36pt` is `48px`), keywords are lowercased and a
family name is quoted.

| Grammar | Means |
|---|---|
| LEN | `Npx` or a bare number of pixels (`pt` converts); never `em`, `rem`, `vw`, `vh` or `%` |
| PCT | `N%` |
| COLOR | `#rgb`, `#rgba`, `#rrggbb`, `#rrggbbaa`, `rgb()`, `rgba()`, `hsl()`, `hsla()`, a CSS named colour, or `transparent`; never `currentcolor` or `var()` |
| ANGLE | `Ndeg`, `Nturn` or `Nrad` |
| FAMILY | up to eight family names, quoted or not, each a letter then letters, digits, spaces, `_` and `-`, at most 40; or `serif`, `sans-serif`, `monospace`, `cursive`, `system-ui` |

| Property | Value | On |
|---|---|---|
| `position` | `absolute`, `relative` | placed elements |
| `left`, `top` | LEN, PCT, `auto`, `calc(PCT ± LEN)` | pinned elements |
| `right`, `bottom` | LEN, PCT, `auto` | pinned elements |
| `width` | LEN, PCT, `auto` | placed elements, `th`, `td` |
| `height` | LEN, PCT, `auto` | placed elements |
| `min-width` | LEN, `min-content`, `max-content` | placed elements |
| `min-height`, `max-width`, `max-height` | LEN | placed elements |
| `display` | `flex`, `grid`, `none` | `section`, `div` |
| `flex-direction` | `row`, `column`, `row-reverse`, `column-reverse` | `section`, `div` |
| `flex-wrap` | `wrap`, `nowrap` | `div` |
| `gap` | LEN, 0 to 512 | `section`, `div` |
| `align-items` | `start`, `center`, `end`, `stretch`, `baseline` and the `flex-`/`self-` forms | `section`, `div` |
| `justify-content` | `start`, `center`, `end`, `flex-start`, `flex-end`, `space-between`, `space-around`, `space-evenly` | `section`, `div` |
| `justify-items` | `start`, `center`, `end`, `stretch` | `section`, `div` |
| `align-self` | as `align-items`, or `auto` | placed elements |
| `justify-self` | `auto`, `start`, `center`, `end`, `stretch` | placed elements |
| `flex` | `none`, `auto`, or grow [shrink] [basis] | placed elements |
| `flex-grow`, `flex-shrink` | N | placed elements |
| `flex-basis` | LEN, `auto` | placed elements |
| `grid-template-columns`, `grid-template-rows` | tracks of LEN, `Nfr`, `auto` and `repeat(N, …)`, at most 24 | `section`, `div` |
| `grid-column`, `grid-row` | `span N`, 1 to 24 | placed elements |
| `aspect-ratio` | N or N / N | placed elements |
| `padding` | one to four LEN, 0 to 256 | `section`, `div`, text elements, `table`, `th`, `td` |
| `overflow` | `hidden`, `visible` | `div` |
| `font-family` | FAMILY | `section`, `div`, text elements, `table`, `th`, `td` |
| `font-size` | LEN, 8 to 400 | as `font-family` |
| `font-weight` | `normal`, `bold`, 100 to 900 in hundreds | as `font-family`, and `span` |
| `font-style` | `normal`, `italic` | as `font-family`, and `span` |
| `font` | `[italic] [weight] size[/line-height] family` | as `font-family` |
| `line-height` | a multiplier 0.5 to 4, PCT, or LEN | as `font-family` |
| `letter-spacing` | `normal`, LEN −24 to 32, or `Nem` | as `font-family` |
| `text-align` | `left`, `center`, `right`, `justify` | as `font-family` |
| `text-transform` | `none`, `uppercase`, `lowercase`, `capitalize` | as `font-family` |
| `white-space` | `normal`, `nowrap` | as `font-family` |
| `text-decoration` | `none`, `underline` or `line-through`, an optional style and COLOR (`underline` only on a `span`) | as `font-family`, and `span` |
| `font-variant-numeric` | `normal`, `tabular-nums` | as `font-family` |
| `-webkit-text-stroke` | LEN 0 to 8 and COLOR | as `font-family` |
| `color` | COLOR | as `font-family`, and `span`, `x-icon`, `hr` |
| `background` | COLOR, or one `linear-gradient()`, `repeating-linear-gradient()` or `radial-gradient()` of 2 to 8 stops with an optional COLOR under it (a `tr`, `x-shape` or `hr` takes a COLOR) | painted elements |
| `background-color` | COLOR | painted elements |
| `background-clip`, `-webkit-background-clip` | `text` | text elements |
| `-webkit-text-fill-color` | `transparent`, or `currentcolor` on a `span` | text elements, `span` |
| `border`, `border-top`, `border-right`, `border-bottom`, `border-left` | `[LEN 0 to 16] solid\|dashed\|dotted\|double\|none [COLOR]` (the style word is required), or `none` | painted elements |
| `border-radius` | one to four LEN or PCT | `div`, `img`, `table`, `x-shape`, text elements |
| `box-shadow` | up to eight `[inset] x y [blur [spread]] COLOR` | `div`, `img`, `table`, `x-icon`, `x-shape`, text elements |
| `text-shadow` | up to eight `x y [blur] COLOR` | text elements, `table` |
| `opacity` | 0 to 1 | placed elements, `section` |
| `transform` | `translate()`, `translateX()`, `translateY()`, `rotate()`, `scale()` (0.5 to 2), `skew()`, `skewX()`, `skewY()` (±60deg), each once, in that order (`rotate()` alone on a shape) | placed elements |
| `filter`, `backdrop-filter` | `blur()` (to 20px), `brightness()`, `contrast()`, `saturate()`, `grayscale()`, `sepia()`, `invert()`, `hue-rotate()` | `div`, `img`, `x-shape`, text elements |
| `mix-blend-mode` | `normal`, `multiply`, `screen`, `overlay`, `darken`, `lighten` | `div`, `img`, `x-shape`, text elements |
| `object-fit` | `cover`, `contain` | `img` |
| `margin`, `box-sizing`, `grid-template` | `0`, `border-box`, `none` only, accepted and left out | |

A placed element is any block a container holds; a text element is `h1` to
`h3`, `p`, `ul`, `ol` and `li`; a painted element is a `div`, `img`, `table`,
`x-icon`, `x-shape`, `hr`, a text element or a cell.

No value may carry `url(`, `image-set(`, `var(`, `env(`, `attr(`,
`expression`, `@`, a backslash, a comment, `!important`, `<`, `>`, `{` or `}`,
whatever its property.

## Inline drawings

A chart or a diagram is one `<svg>` with an `aria-label`, at most 52 KB of
source: `g`, `defs`, `path`, `rect`, `circle`, `ellipse`, `line`,
`polyline`, `polygon`, `text`, `tspan`, `title`, `desc`, `use`, `symbol`,
`marker`, `pattern`, `clipPath`, `mask`, `linearGradient`, `radialGradient`,
`stop`, `filter` and the `feGaussianBlur`, `feOffset`, `feBlend`,
`feColorMatrix`, `feFlood`, `feComposite`, `feMerge`, `feMergeNode` and
`feDropShadow` primitives, with their geometry and presentation attributes
(`fill`, `stroke`, `stroke-width`, `opacity`, `transform`, `viewBox` and the
rest the engine's table lists).

A drawing reaches nothing outside itself: no `<script>`, no event attribute,
no `<foreignObject>`, no `<style>` element or `style` attribute (use
presentation attributes), and every reference (`href`, `url(#…)`) is to an id
the same drawing declares. Ids are a letter then letters, digits, `_`, `.`
and `-`. A presentation attribute is read as CSS, where an escape or a
function could reach outside the drawing, so an attribute value carries no
backslash (an `aria-label` excepted), a parenthesis only in `url(#id)`, a
transform (`matrix`, `translate`, `scale`, `rotate`, `skewX`, `skewY`) or a
colour (`rgb()`, `rgba()`, `hsl()`, `hsla()`), and is otherwise numbers,
lengths, percentages, hex colours and keywords; a `font-family` list may
quote its names. A filter, clip or mask is the drawing's own element, named
by `url(#id)`, never a CSS function such as `blur()` or `inset()`. A drawing
draws at most 50,000 elements once every `<use>`, marker (at each vertex of a
path naming it as `marker-mid`) and paint server is expanded, and no reference
reaches back into what refers to it. The
dashboard gives each drawn copy of a drawing its own id prefix,
so a slide drawn twice (its thumbnail and the stage) never resolves the other
copy's gradients.

## Images and typefaces

An image is an `<img>` whose `src` is a PNG, JPEG, GIF, WebP or SVG file of
the same version, by its path from the deck's root. Never a URL, a `data:`
address, a path starting with `/` or `./`, or one holding `..`. The dashboard
reads the file with the viewer's own sign-in and shows it through an object
URL of its own, so a deck makes no request a slide names.

A typeface is a `faces` entry whose `src` is a woff2, woff, ttf or otf file of
the version. The dashboard loads it from the file's bytes; a slide names it in
`font-family` like any family, and a family that is not declared falls back to
the next one the slide names, as in any stylesheet. A Google Fonts `href`,
which Claude's format accepts, is refused here with how to fix it: download
the font file and include it in the version, so the deck draws the same
wherever it is opened.

## Bounds

| Bound | Value |
|---|---|
| Slides in a deck | 500 |
| Elements on a slide (a drawing counted once) | 200 |
| Nested `<div>`s | 15 |
| Pinned children of one `<div>` | 24 |
| Characters of text on a slide, notes aside | 20,000 |
| Inline marks (`b`, `i`, `u`, `a`, `span`) on a slide | 100 |
| Rows of a table, cells of a row, cells on a slide | 100, 24, 4,000 |
| Characters of speaker notes | 4,000 |
| One inline drawing's source | 52 KB |
| Typefaces | 4 |

A version's own bounds apply too: 511 files, and each file's size by its type.

## What is refused, and what to write instead

| Refused | Write instead |
|---|---|
| `class`, `<style>`, `<link>`, `margin`, `z-index`, `em`, `var()`, `url()` | Inline styles from the table; paint order is source order |
| `<script>`, an `on…` attribute, `<iframe>`, `<object>`, `<canvas>`, form controls | Nothing runs on a slide; draw in an inline `<svg>` |
| `x-embed` (a live page on a slide) | A picture of it as an `<img>` |
| `x-connector` | A line in an inline `<svg>` |
| `data-build-in`, `data-build-out` (builds) | Show the element from the start, or split the slide |
| `data-transition="magic"`, an `id` on any element but the section | `fade` or `push` |
| `data-section` on a slide | `sections` in `deck.json` |
| `data-crop` | Crop the image file itself; `object-fit` covers or contains it in its box |
| `<thead>`, `<tbody>`, `<h4>`, `<em>`, `<strong>`, `<pre>`, `<code>`, `<blockquote>` | `<tr>` directly, `<h3>`, `<i>`, `<b>`, a styled `<p>` |
| A Google Fonts `href` in `faces`, a design system | A font file of the version in `src`; the company's look comes from its own decks |

A refusal lists every finding at once (up to 50), each as
`file:line: rule: message`, under the code `slide_format`; an agent's tool
answer shows the first 20 and counts the rest. A structural fault in
`deck.json` (an unknown key, a value of the wrong type) is named with the keys
or the shape the index reads there, every one in the same refusal, and
`get_artifact_format` with `kind: "slides"` lists every key of `deck.json`.

## Themes and layouts

A deck begun in the dashboard is composed by the engine from a **plan**: the
theme it is drawn in and the slides it opens with, each a layout and its
title. The engine writes ordinary slides, held to this format like any
version, and names the theme in the index so a slide added later is drawn
from the same theme's layouts. `GET /artifacts/starters/slides` answers every
theme and layout (see [API Endpoints](api-endpoints.md)).

| Theme | Look |
|---|---|
| `nightshade` | Periwinkle on blue-black, set in Inter |
| `ink` | Navy and paper, set in Georgia (the default) |
| `paper` | White, quiet and exact, set in Helvetica Neue |
| `signal` | Yellow on black, for a big room |
| `studio` | Warm cream and terracotta, Palatino over Avenir Next |
| `grove` | Deep green and mint, set in the system face |
| `brand` | The company's adopted brand: its colours, its typefaces and its logo on the cover. Offered first whenever a brand is adopted, and refused when none is |

A deck begun in `brand` is drawn from the brand adopted at that moment, and
keeps that look: its slides hold their styles inline like any deck's, the
brand's uploaded typefaces are copied in as font files of the version and
declared in `faces`, and the cover shows the logo meant for its ground (the
logo for dark grounds when the cover's ground is dark), copied in as an image
of the version under `brand/`. A later adoption changes no deck already
begun; start a new one from the new brand to take it.

| Layout | Holds |
|---|---|
| `cover` | The deck's title and who it is for; its title is the deck's |
| `agenda` | A numbered list of the parts to come |
| `section` | Opens a part of the deck; it starts a section of the outline under its title |
| `points` | A title and a short list |
| `columns` | Two cards side by side |
| `number` | One figure, large, and what it measures |
| `quote` | A quote, large, and who said it |
| `closing` | The one decision or ask to end on |

A slide's id is its layout's, numbered from two when a layout is used again
(`points`, `points-2`).

### Changes to the format

- **`theme` in the index.** Added as an optional key, so every deck written
  before it is still in the format. A deck that names a theme outside the
  list above is refused, naming the themes there are.
- **`brand` as a theme.** Added beside the engine's six. A deck naming it is
  in the format whether or not a brand is adopted now, since the index only
  records the look the deck began in.

## How a deck is drawn

- **Read.** `GET /artifacts/{id}/versions/{n}/deck` answers the canonical tree
  (see [API Endpoints](api-endpoints.md#viewing-a-version)), and
  `GET …/outline` a deck's titles and cover slide for the gallery. An agent
  reads the outline with `get_artifact` and `view: "outline"`, then only the
  slide files it changes.
- **Edit.** A person with `editor` edits text in place, speaker notes, the
  order and the deck's title, and deletes slides; Save publishes a version
  against the one they opened. The editor writes each changed slide back in
  this format's canonical form, every value as the engine stores it, so a
  slide a person saved reads back to the same tree. A save refused because
  somebody published since is merged onto their version slide by slide, and
  what both changed differently is the person's to resolve.
- **Present.** Present draws the slide as large as fits, full screen on the
  reader's press, with the keys a clicker sends (Right, Down, Page Down,
  Space and Enter forward; Left, Up, Page Up and Backspace back; Home and End;
  Escape out), each slide announced by its number and title. Full screen is
  a projector's: the slide on black, its controls floating over its foot and
  put away with the cursor while the pointer rests (a clicker's keys do not
  bring them back). The Presenter
  window, the dashboard's own, shows the slide, the next one, the notes and a
  timer, and moves the room's screen. Anybody in the room can **Follow** the
  presenter, a phone included, and is moved to the slide they are on.
- **Print.** Print draws every shown slide on its own 16:9 page, so the
  browser's Save as PDF is the deck as a PDF.
- **Compare.** History compares two versions By slide: each slide's two
  versions side by side under their versions' names, paired by id, with changed, added, removed and moved
  slides said in words, and unchanged ones folded away.
- **Start.** New › Deck, or `POST /artifacts` with `"kind": "slides"` and
  `"starter": true`, begins a deck from the engine's starter slides.
