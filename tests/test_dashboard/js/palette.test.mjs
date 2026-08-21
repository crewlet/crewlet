// The palette's promises, measured against the stylesheet that ships.
//
// styles/tokens.css claims separations and contrasts in a doc comment. They
// were measured by hand once, and nothing kept them true: by the time this
// suite was written the dark theme's `--text-dim` had drifted to 2.67:1 —
// under the 3:1 floor it exists to clear — while body text sat at 19.25:1,
// high enough to halate. Both are invisible to review and neither shows up
// as a broken layout.
//
// Every number in tokens.css's header comment is computed here.

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { test, run } from "./harness.mjs";
import {
  parseTokens,
  themeTokens,
  parseColor,
  flatten,
  contrast,
  deltaE,
  chroma,
  simulate,
} from "./color.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const TOKENS_CSS = join(
  HERE,
  "../../../src/crewlet/static/dashboard/styles/tokens.css",
);
const STATE_JS = join(
  HERE,
  "../../../src/crewlet/static/dashboard/js/state.js",
);

const blocks = parseTokens(readFileSync(TOKENS_CSS, "utf8"));
const THEMES = {
  dark: [":root", ':root[data-theme="dark"]'],
  light: [":root", ':root[data-theme="light"]'],
};

function theme(name) {
  const tokens = themeTokens(blocks, THEMES[name]);
  const col = (token) => {
    const c = parseColor(tokens.get(token), tokens);
    if (!c) throw new Error(`${name}: ${token} is missing or not a colour`);
    return c;
  };
  const bg = col("--bg");
  // What the eye actually receives: the panel fill is an alpha in the dark
  // theme, so measuring a label against the token's nominal value would
  // describe a colour that is never painted.
  const card = flatten([bg, col("--bg-card")]);
  return { tokens, col, bg, card };
}

const HUES = [
  "green",
  "blue",
  "amber",
  "purple",
  "cyan",
  "orange",
  "pink",
  "brown",
];

// ---------------------------------------------------------------------
// The two orders that render edge to edge
// ---------------------------------------------------------------------
// Read from state.js rather than restated here: a hue reassignment that
// forgot this suite would otherwise validate the old order forever.

function hueOrders() {
  const src = readFileSync(STATE_JS, "utf8");

  const phaseBlock = /const PHASE_HUE = \{([\s\S]*?)\}/.exec(src);
  if (!phaseBlock) throw new Error("PHASE_HUE not found in state.js");
  const phaseHue = new Map();
  for (const m of phaseBlock[1].matchAll(/(\w+):\s*"(\w+)"/g)) {
    phaseHue.set(m[1], m[2]);
  }

  const orderBlock = /export const PHASE_ORDER = \{([\s\S]*?)\}/.exec(src);
  if (!orderBlock) throw new Error("PHASE_ORDER not found in state.js");
  const ranked = [];
  for (const m of orderBlock[1].matchAll(/(\w+):\s*(\d+)/g)) {
    ranked.push([m[1], Number(m[2])]);
  }
  ranked.sort((a, b) => a[1] - b[1]);
  const phase = ranked
    .map(([name]) => phaseHue.get(name))
    .filter(Boolean);

  const catBlock = /export const EVENT_CATEGORIES = \[([\s\S]*?)\];/.exec(src);
  if (!catBlock) throw new Error("EVENT_CATEGORIES not found in state.js");
  const catKeys = [...catBlock[1].matchAll(/key:\s*"([\w-]+)"/g)].map(
    (m) => m[1],
  );
  const catHueBlock = /const CATEGORY_HUE = \{([\s\S]*?)\}/.exec(src);
  if (!catHueBlock) throw new Error("CATEGORY_HUE not found in state.js");
  const catHue = new Map();
  for (const m of catHueBlock[1].matchAll(/(\w+):\s*"([\w-]+)"/g)) {
    catHue.set(m[1], m[2]);
  }
  const category = catKeys.map((k) => catHue.get(k)).filter(Boolean);

  return { phase, category };
}

const ORDERS = hueOrders();

function adjacentPairs(order) {
  const out = [];
  for (let i = 0; i + 1 < order.length; i++) {
    // A family repeated back to back is a deliberate sharing (auxiliary and
    // subagent are one hue), not a pair that has to hold apart.
    if (order[i] !== order[i + 1]) out.push([order[i], order[i + 1]]);
  }
  return out;
}

// ---------------------------------------------------------------------
// Gates
// ---------------------------------------------------------------------

const SEPARATION_GATE = 15; // normal vision, OKLab ΔE ×100
const CVD_GATE = 8; // protanopia / deuteranopia
const MARK_FLOOR = 3; // non-text contrast
const INK_FLOOR = 4.5; // text contrast
const CHROMA_CEILING = 15; // saturated light-on-dark type vibrates
// The reserved status hue is allowed more: it is used sparingly (rails,
// badges, dots) rather than as body copy, and a danger colour that reads as
// muted is its own kind of failure.
const RED_CHROMA_CEILING = 16;
// Red's distance from the nearest categorical mark, for normal vision. The
// palette this replaced managed 7.8 (red against orange), so this is a
// floor the dashboard has already cleared in practice — set just above it
// so the status hue cannot quietly regress toward the category set.
const RED_SEPARATION_GATE = 8;

// Body text is held inside a BAND. The floor is legibility; the ceiling is
// the point where small bright glyphs start to halate against the ground.
const BODY_BAND = [11, 15];
const HEADING_CEILING = 16.5; // larger glyphs tolerate more

for (const name of Object.keys(THEMES)) {
  test(`${name}: body text sits inside the comfort band`, () => {
    const { col, bg } = theme(name);
    const ratio = contrast(col("--text"), bg);
    if (ratio < BODY_BAND[0] || ratio > BODY_BAND[1]) {
      throw new Error(
        `--text is ${ratio.toFixed(2)}:1 against --bg, outside ${BODY_BAND[0]}..${BODY_BAND[1]}`,
      );
    }
  });

  test(`${name}: headings stay under the halation ceiling`, () => {
    const { col, bg } = theme(name);
    const ratio = contrast(col("--heading"), bg);
    if (ratio > HEADING_CEILING) {
      throw new Error(
        `--heading is ${ratio.toFixed(2)}:1, over ${HEADING_CEILING}`,
      );
    }
  });

  test(`${name}: the whole text ramp clears its floor and descends`, () => {
    const { col, card } = theme(name);
    const steps = [
      ["--text", 11],
      ["--text-secondary", 6],
      ["--text-muted", 4.5],
      // Dim is de-emphasised meta, not body copy, so it is held to the
      // non-text floor rather than the text one — but it IS held to it.
      ["--text-dim", 3],
    ];
    let previous = Infinity;
    for (const [token, floor] of steps) {
      const ratio = contrast(flatten([card, col(token)]), card);
      if (ratio < floor) {
        throw new Error(
          `${token} is ${ratio.toFixed(2)}:1 on the panel, under ${floor}`,
        );
      }
      if (ratio > previous) {
        throw new Error(
          `${token} (${ratio.toFixed(2)}) is stronger than the step above it`,
        );
      }
      previous = ratio;
    }
  });

  test(`${name}: every hue ink clears text contrast`, () => {
    const { col, card } = theme(name);
    for (const hue of [...HUES, "red", "accent"]) {
      const ratio = contrast(flatten([card, col(`--${hue}-ink`)]), card);
      if (ratio < INK_FLOOR) {
        throw new Error(
          `--${hue}-ink is ${ratio.toFixed(2)}:1 on the panel, under ${INK_FLOOR}`,
        );
      }
    }
  });

  test(`${name}: every hue mark clears non-text contrast`, () => {
    const { col, card } = theme(name);
    for (const hue of [...HUES, "red", "accent"]) {
      const ratio = contrast(flatten([card, col(`--${hue}`)]), card);
      if (ratio < MARK_FLOOR) {
        throw new Error(
          `--${hue} is ${ratio.toFixed(2)}:1 on the panel, under ${MARK_FLOOR}`,
        );
      }
    }
  });

  test(`${name}: no hue is saturated enough to vibrate`, () => {
    const { col } = theme(name);
    for (const hue of [...HUES, "red", "accent"]) {
      const ceiling = hue === "red" ? RED_CHROMA_CEILING : CHROMA_CEILING;
      for (const suffix of ["", "-ink"]) {
        const c = chroma(col(`--${hue}${suffix}`));
        if (c > ceiling) {
          throw new Error(
            `--${hue}${suffix} has chroma ${c.toFixed(1)}, over ${ceiling}`,
          );
        }
      }
    }
  });

  for (const [label, order] of Object.entries(ORDERS)) {
    test(`${name}: ${label} order stays separable, including for CVD`, () => {
      const { col, card } = theme(name);
      const pairs = adjacentPairs(order);
      if (!pairs.length) throw new Error(`${label} order produced no pairs`);
      for (const [x, y] of pairs) {
        const a = flatten([card, col(`--${x}`)]);
        const b = flatten([card, col(`--${y}`)]);
        const checks = [
          ["normal", deltaE(a, b), SEPARATION_GATE],
          [
            "protan",
            deltaE(simulate(a, "protan"), simulate(b, "protan")),
            CVD_GATE,
          ],
          [
            "deutan",
            deltaE(simulate(a, "deutan"), simulate(b, "deutan")),
            CVD_GATE,
          ],
        ];
        for (const [vision, value, gate] of checks) {
          if (value < gate) {
            throw new Error(
              `${x}|${y} separates by only ${value.toFixed(1)} for ${vision} vision (gate ${gate})`,
            );
          }
        }
      }
    });
  }

  test(`${name}: the reserved status hue reads apart from the categoricals`, () => {
    // Red has to keep meaning "this broke" while sitting in a list beside
    // category marks. It is in neither adjacency order, so it would
    // otherwise go unchecked — and its nearest neighbour is orange, which
    // is where a "failed" rail and an a2a chip can genuinely meet.
    //
    // NORMAL VISION ONLY, deliberately. Red against green under protanopia
    // is the one separation no palette can win: those are the two hues a
    // protanope's missing cone cannot tell apart, and pushing red until it
    // clears green for them would leave a colour nobody else reads as red.
    // That gap is closed by the rule that colour never carries identity
    // alone — every failed row also says "failed" — not by the hue.
    const { col, card } = theme(name);
    const red = flatten([card, col("--red")]);
    for (const hue of HUES) {
      const other = flatten([card, col(`--${hue}`)]);
      const value = deltaE(red, other);
      if (value < RED_SEPARATION_GATE) {
        throw new Error(
          `--red is only ΔE ${value.toFixed(1)} from --${hue}`,
        );
      }
    }
  });

  test(`${name}: the focus ring clears every surface it lands on`, () => {
    const { col, bg, card } = theme(name);
    const focus = col("--focus");
    for (const [label, surface] of [
      ["--bg", bg],
      ["--bg-card", card],
    ]) {
      const ratio = contrast(focus, surface);
      if (ratio < MARK_FLOOR) {
        throw new Error(
          `--focus is ${ratio.toFixed(2)}:1 on ${label}, under ${MARK_FLOOR}`,
        );
      }
    }
  });
}

test("the dark ground is lifted off pure black", () => {
  // Not a style preference: #000 behind near-white text is the combination
  // that halates, and the elevation model reads a drop shadow as invisible
  // either way. A future edit that "restores" pure black should fail here
  // and read this comment.
  const { col } = theme("dark");
  const bg = col("--bg");
  if (bg.r === 0 && bg.g === 0 && bg.b === 0) {
    throw new Error("--bg is pure black; lift it so body text can sit lower");
  }
});

test("light panels are not pure white", () => {
  const { col } = theme("light");
  const card = col("--bg-card");
  if (card.r === 255 && card.g === 255 && card.b === 255) {
    throw new Error("--bg-card is pure white; the light theme's glare source");
  }
});

test("the category hues in CSS match the map in state.js", () => {
  // Two copies of one assignment: `.cat-*` paints it, CATEGORY_HUE declares
  // it, and the gates above are measured against the declaration. If they
  // drift, the suite validates an order the page does not render.
  const css = readFileSync(
    join(HERE, "../../../src/crewlet/static/dashboard/styles/components.css"),
    "utf8",
  );
  const src = readFileSync(STATE_JS, "utf8");
  const mapBlock = /const CATEGORY_HUE = \{([\s\S]*?)\}/.exec(src);
  const declared = new Map();
  for (const m of mapBlock[1].matchAll(/(\w+):\s*"([\w-]+)"/g)) {
    declared.set(m[1], m[2]);
  }
  for (const [key, hue] of declared) {
    const rule = new RegExp(
      `\\.cat-${key}\\s*\\{\\s*color:\\s*var\\(--([\\w-]+)\\)`,
    ).exec(css);
    if (!rule) throw new Error(`components.css has no .cat-${key} rule`);
    if (rule[1] !== `${hue}-ink`) {
      throw new Error(
        `.cat-${key} paints --${rule[1]} but CATEGORY_HUE declares ${hue}`,
      );
    }
  }
});

test("both themes define the same token names", () => {
  // A token defined in only one theme renders one theme's colour on the
  // other theme's ground — the classic unreadable-artifact bug.
  const dark = themeTokens(blocks, THEMES.dark);
  const light = themeTokens(blocks, THEMES.light);
  const missing = [...dark.keys()].filter((k) => !light.has(k));
  if (missing.length) {
    throw new Error(`light theme is missing: ${missing.join(", ")}`);
  }
});

await run();
