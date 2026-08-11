# Dashboard Design System

The dashboard is a zero-build, modular ES-module app (see
[API Endpoints § the dashboard](api-endpoints.md#live-stream)). This page
documents its **visual system** — the tokens every component reads, the panel
recipe, and the rules a change has to keep holding.

The system is the one the Crewlet marketing site ships. A panel here is the
same object as a panel there: a fill just off the page tone, a hairline border
doing the separation work, a 1px settle shadow, and a 2px lift on hover.

---

## Screens

The sidebar is a flat list with `Company` as the one collapsible group. Every
entry resolves to a view backed by a real endpoint — nothing is rendered as a
placeholder or a coming-soon stub.

| Nav | Route | Reads |
|---|---|---|
| Dashboard | `#/dashboard` | `/stream/snapshot` + `/tokens/breakdown` |
| Company → Overview | `#/company` | `/org` |
| Company → People Directory | `#/people` | `/org` + live agent state |
| Company → Org Chart | `#/org` | `/org` + live agent state |
| Company → Audit log | `#/audit` | `/config/audit` *(auth-gated)* |
| Agents | `#/agents` | `/org` + live agent state |
| Activity | `#/events` | `/events`, live `/ws/stream` |
| Tokens | `#/tokens` | `/tokens/breakdown` |
| Tools | `#/tools` | `/tools` |
| Schedules | `#/schedules` | `/schedules` |
| Configuration | `#/config` | `/config` *(auth-gated, secrets redacted server-side)* |

`js/org.js` is where the `/org` tree is flattened into **seats** — every role
with its unit chain, effective unit lead, and the MCP surfaces it inherits.
Views consume seats, never the raw payload, so lead inheritance and `mcp_env`
inheritance are resolved once.

### The seat card

`js/cards.js` renders one card per seat, used by both the overview's seat row
and the Agents index. Human seats (`kind: human`) appear alongside agents —
they hold seats in the same org chart — and are marked, never given a fake
runtime. Everything on a card is real:

| Element | Source |
|---|---|
| Identity hue on the name | `roleColor` / `roleInk`, hashed from the seat name |
| State dot | the live projection (`effectiveAgentState`), or `human` |
| Integration chips | the seat's own + inherited `mcp_env` server keys; for a human seat, the `contact` identities it is reachable at |
| Status line | `statusLine` in `state.js` — derived from live state only |

`statusLine` never invents activity. An agent with nothing in flight says so;
an AFK agent shows its engine-detected cause; a seat running a detached
sandbox says it is writing code, and says when it is blocked on an answer.

---

Everything lives in four stylesheets, loaded in this order:

| File | Owns |
|---|---|
| `styles/tokens.css` | Every colour, radius, easing, and type-stack value. Nothing else defines a colour. |
| `styles/base.css` | Reset, app shell, sidebar, frosted topbar, motion, icon primitive |
| `styles/components.css` | The panel recipe, badges, rows, tables, buttons, trace tree, skeletons |
| `styles/views.css` | Per-view internals only — panels get their surface from the recipe |

---

## Tokens

Themes are `[data-theme="dark"]` (the default) and `[data-theme="light"]` on
`<html>`. A theme is *only* a different set of values in `tokens.css`; no
component branches on the theme.

### Surfaces, borders, text

| Token | Job |
|---|---|
| `--bg` / `--bg-sidebar` | Page tone; the nav rail one step off it |
| `--bg-card` | Panel fill — deliberately close to `--bg` |
| `--bg-card-2` | Raised inner surface (table headers, chips) |
| `--bg-hover` / `--bg-active` | Row and control states |
| `--bg-inset` | Recessed surface (code blocks, expanded bodies) |
| `--border-subtle` / `--border` / `--border-strong` | Hairline ramp |
| `--text` / `--text-secondary` / `--text-muted` / `--text-dim` | Ink ramp |
| `--heading` | Headings, row titles, headline numbers |

### Accent and glass

`--accent` is a **pale fill**, not a text colour — its readable partner is
`--accent-ink`, and `--accent-soft` is the wash used behind counts and chips.
`--glass` / `--glass-hover` back the topbar, icon buttons, and segmented
controls, always with `backdrop-filter: blur(…)`.

### Elevation and motion

`--panel-shadow` (the 1px settle), `--card-shadow` (floating chrome: toasts,
the mobile drawer), `--lift-shadow` (hover). Easings are `--ease` and
`--ease-snappy`; durations `--dur` / `--dur-slow`.

### Categorical hues

Eight hue families, each shipping two steps:

- `--<hue>` — the **mark** step: bars, dots, stack segments, heat fills
- `--<hue>-ink` — the **text** step: badge labels, links, tinted type

Soft fills are derived at the use site with
`color-mix(in srgb, var(--<hue>) 10%, transparent)`.

`--red` / `--red-ink` is a **reserved status hue** (failed, error,
destructive) and is never used as a categorical slot.

**The rule for text.** Any label that carries a hue uses the `-ink` step. The
mark step is tuned for a mark on a surface (≥ 3:1), not for 11px type. Setting
a mark step as `color` on text is the one mistake this split exists to prevent.

---

## Where the hues are assigned

Two orders render hues side by side, so those are the orders that have to hold
apart:

| Order | Source | Assignment |
|---|---|---|
| Phases | `PHASE_ORDER` in `js/state.js`, used by the stacked phase bar and its legend | onboarding → green, plan → blue, execute → amber, review → purple, auxiliary/subagent → orange, judge → cyan, turn → neutral |
| Event categories | `EVENT_CATEGORIES` in `js/state.js`, used by the activity filter row and category tags | lifecycle → blue, task → green, communication → purple, decision → amber, knowledge → pink, learning → purple, a2a → orange, notification → cyan, webhook → brown, system → neutral |

`PHASE_HUE` in `state.js` is the single source of truth for phase colour;
`phaseColor()` returns the mark step and `phaseInk()` the text step. There is
no second hardcoded copy — the agent × stage heatmap mixes its fill from
`phaseColor()` so it re-steps with the theme instead of being frozen at one
theme's values.

The assignments are not free choices. They are the ones under which every
adjacent pair in **both** orders clears, in **both** themes:

| Check | Gate | Light | Dark |
|---|---|---|---|
| Normal-vision separation (worst adjacent pair, OKLab ΔE ×100) | ≥ 15 | 17.9 | 17.8 |
| Protan / deutan separation | ≥ 8 | 15.8 | 15.6 |
| Mark contrast vs the surface | ≥ 3:1 | 3.29 | 3.70 |

The hue *families* (angle and chroma) come from the marketing site's brand
tokens; only lightness is re-stepped per theme, because the site's own steps
are tuned for occasional accent use on a marketing page rather than a dense
operational surface where six of them stack edge to edge.

**If you change a hue, re-check both orders in both themes.** Changing one
value can break a pair three slots away.

Colour is never the only carrier of identity: every pill, legend entry, and
badge renders its name, and every heat cell prints its value.

---

## The panel

One recipe, shared by `.panel`, `.card`, `.list`, `.widget`, `.stat`,
`.tool-card`, `.turn`, and `.mem-card`:

```css
background: color-mix(in srgb, var(--bg-card) 94%, transparent);
border: 1px solid var(--border);
border-radius: var(--radius);          /* 13px */
box-shadow: var(--panel-shadow);       /* 0 1px 0 */
```

An actionable panel adds `.clickable`, which swaps in `--lift-shadow` and
`translateY(-2px)` on hover. `.dot-texture` adds the site's decorative accent
dots — used on the overview's lead panel only.

Views must not re-declare a panel's fill, border, radius, or elevation. If a
view needs a new surface, it gets the recipe, not a copy of it.

## Type

- **Body** — Instrument Sans, `letter-spacing: -0.011em`. The negative
  tracking is what makes the face read as the brand rather than as a default
  UI font.
- **Headings** — the same face at `-0.025em` to `-0.035em`, in `--heading`.
- **Micro-labels** — JetBrains Mono, uppercase, `letter-spacing: 0.1em`–`0.16em`,
  in `--text-muted`. This is the site's eyebrow, and it is what section
  headers (`.sec-title`), table headers (`.tbl th`), block labels
  (`.block-label`), stat labels, and status badges all wear. The shared
  `.eyebrow` class in `base.css` is the standalone form.
- **Numbers** — `font-variant-numeric: tabular-nums` everywhere they can
  change, so a live value does not jitter its column.

Status badges are mono/uppercase because they are codes. A badge carrying
prose (a task title) adds `.text`, which returns it to the sans face and
sentence case.

## Motion

The view container carries `.cascade-enter`, so a route change rises its
children in with a short stagger (`fade-up`, 0.45s, ≤ 0.18s delay). Controls
settle with `scale(0.97)` on `:active`. Everything is disabled under
`prefers-reduced-motion: reduce`.

---

## Changing the dashboard

1. **New colour? Add a token.** Component stylesheets and view modules never
   contain a hex value.
2. **Colour on text is `-ink`, colour on a mark is the base step.**
3. **New surface? Use the panel recipe** by adding the selector to the shared
   rule in `components.css`.
4. **New hue or reassignment?** Re-verify both adjacency orders in both
   themes against the gates above before shipping.
5. **New micro-label?** Mono, uppercase, wide tracking — not bold sans.
6. **New nav entry?** It ships only once an endpoint backs it. A nav item
   that leads to an empty screen is worse than no nav item.
7. **Need the org tree?** Go through `flattenSeats` / `flattenUnits` in
   `js/org.js`, so lead and `mcp_env` inheritance stay resolved in one place.
