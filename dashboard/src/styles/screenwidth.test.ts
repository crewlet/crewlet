// @vitest-environment node
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";
import { density, spacing } from "@crewlethq/tokens";

import {
  CHAT_RAIL_FOLD,
  CHAT_RAIL_MAX,
  CHAT_RAIL_MIN,
  CHAT_THREAD_FOLD,
  CHAT_THREAD_WIDTH,
  TRANSCRIPT_FLOOR,
  densityScale,
} from "~/app/layout.ts";

/**
 * A SCREEN'S SHAPE FOLLOWS THE SCREEN'S WIDTH, NEVER THE WINDOW'S.
 *
 * The assistant is a column of the frame the reader opens and drags wider, and
 * it takes width the window never reports: at a 1375px window with a 640px
 * assistant the screen is 443px wide while a media query still answers
 * "desktop". Every screen that changes shape for a narrow width (one pane
 * instead of two, a control moved into a popover) therefore asks the screen:
 * a stylesheet through `@container page`, JavaScript through the kit's
 * `useScreenBelow`. jsdom lays nothing out, so these rules are read.
 */

const SRC = fileURLToPath(new URL("..", import.meta.url));
const STYLES = fileURLToPath(new URL(".", import.meta.url));
const strip = (css: string) => css.replace(/\/\*[\s\S]*?\*\//g, "");
const screens = strip(readFileSync(join(STYLES, "screens.css"), "utf8"));
/** Company › Integrations' own sheet, which its screen loads with it. */
const integrations = strip(readFileSync(join(STYLES, "integrations.css"), "utf8"));

/** Every top-level block opened by `at` (an at-rule prelude), with its body. */
function blocks(css: string, at: RegExp): string[] {
  const out: string[] = [];
  for (const m of css.matchAll(at)) {
    let depth = 0;
    let i = css.indexOf("{", m.index);
    const start = i;
    for (; i < css.length; i++) {
      if (css[i] === "{") depth++;
      else if (css[i] === "}" && --depth === 0) break;
    }
    out.push(css.slice(start + 1, i));
  }
  return out;
}

/** A selector's weight as (ids, classes + attributes + pseudo-classes, types). */
function specificity(selector: string): [number, number, number] {
  const s = selector.replace(/:where\([^)]*\)/g, "").replace(/::?(before|after)\b/g, "");
  const ids = (s.match(/#[\w-]+/g) ?? []).length;
  const classes = (s.match(/\.[\w-]+|\[[^\]]+\]|:(?!:)[\w-]+(\([^)]*\))?/g) ?? []).length;
  const types = (s.replace(/\[[^\]]+\]|\([^)]*\)/g, "").match(/(^|[\s>+~])[a-z][\w-]*/g) ?? [])
    .length;
  return [ids, classes, types];
}

const weighs = (a: [number, number, number], b: [number, number, number]) =>
  a[0] !== b[0] ? a[0] - b[0] : a[1] !== b[1] ? a[1] - b[1] : a[2] - b[2];

/** The body of the first rule whose selector is exactly `selector`. */
function rule(css: string, selector: string): string {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const m = new RegExp(`(?:^|\\})\\s*${escaped}\\s*\\{([^}]*)\\}`).exec(css);
  expect(m, `${selector} is not declared at all`).not.toBeNull();
  return m![1]!;
}

/** The selectors of every rule in `body` that sets `property`. */
function settersOf(body: string, property: string): string[] {
  const out: string[] = [];
  for (const m of body.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    if (new RegExp(`(^|;|\\s)${property}\\s*:`).test(m[2]!)) {
      out.push(...m[1]!.split(",").map((s) => s.trim()));
    }
  }
  return out;
}

test("Home's figures undo every wider span under the phone's width", () => {
  const phone = blocks(screens, /@container page \(width < 640px\)/g).join("\n");
  const wider = blocks(screens, /@container page \(width < (?!640px)\d+px\)/g).join("\n");
  const spans = settersOf(wider, "grid-column").filter((s) => s.startsWith(".home-kpis"));
  const resets = settersOf(phone, "grid-column").filter((s) => s.startsWith(".home-kpis"));
  expect(spans.length).toBeGreaterThan(0);
  expect(resets.length).toBeGreaterThan(0);
  // The phone's reset comes later in the sheet, so equal weight wins; less
  // weight loses, and the span it failed to undo grows implicit tracks.
  const reset = resets.map(specificity).sort(weighs).at(-1)!;
  for (const span of spans) expect(weighs(reset, specificity(span))).toBeGreaterThanOrEqual(0);
});

test("the split screens choose their panes by the screen's width", () => {
  const sheets = `${screens}\n${integrations}`;
  const phone = blocks(sheets, /@media \(width < 640px\)/g).join("\n");
  // The window's own rules may let a page scroll as a whole; they never pick
  // a pane, which is the screen's width's to decide.
  for (const selector of [".inbox-panes", ".int-back"]) {
    expect(phone.includes(selector), `${selector} under a window media query`).toBe(false);
  }
  const page = blocks(sheets, /@container page \(width < 640px\)/g).join("\n");
  expect(page).toMatch(/\.inbox-panes\s*\{[^}]*grid-template-columns:\s*minmax\(0, 1fr\)/);
  expect(page).toMatch(/\.int-back\s*\{[^}]*display:\s*inline-flex/);
});

// CHATLET'S COLUMNS GIVE WAY BY THE SCREEN'S WIDTH TOO, at the widths
// `app/layout.ts` derives from the transcript's floor. They folded at the
// phone's WINDOW width, so beside the assistant's column on a 1280 window the
// rail, the transcript and an open thread stood side by side in a 556px
// screen, and the transcript beside the thread was 0px wide.
test("Chatlet's columns fold by the screen's width, where the transcript keeps its floor", () => {
  // NO WINDOW WIDTH LAYS THEM OUT, at any width: a media query cannot see the
  // sidebar, a peek or the assistant's column.
  const windowRules = blocks(screens, /@media[^{]*width/g).join("\n");
  expect(windowRules, "Chatlet's columns under a window media query").not.toMatch(
    /\.chat-(?:frame|room|rail|thread)(?![\w-])/,
  );

  // THE TRACKS THE ARITHMETIC COUNTS, as the sheet draws them: the rail's
  // range, the room's track starting at the floor (so the rail gives way
  // first), the thread's width, and the gap between each.
  const frame = rule(screens, ".chat-frame");
  expect(frame).toMatch(
    new RegExp(
      `grid-template-columns:\\s*minmax\\(${CHAT_RAIL_MIN}px, ${CHAT_RAIL_MAX}px\\) ` +
        `minmax\\(${TRANSCRIPT_FLOOR}px, 1fr\\);`,
    ),
  );
  expect(frame).toMatch(/gap:\s*var\(--spacing-4\);/);
  expect(rule(screens, ".chat-room")).toMatch(/gap:\s*var\(--spacing-4\);/);
  expect(rule(screens, ".chat-room.with-thread")).toMatch(
    new RegExp(
      `grid-template-columns:\\s*minmax\\(0, 1fr\\) minmax\\(0, ${CHAT_THREAD_WIDTH}px\\);`,
    ),
  );

  // THE FOLDS, at the module's widths: the thread under the room, then the
  // rail above it.
  const below = (width: number) =>
    blocks(screens, new RegExp(`@container page \\(width < ${width}px\\)`, "g")).join("\n");
  expect(below(CHAT_THREAD_FOLD)).toMatch(
    /\.chat-room\.with-thread\s*\{\s*grid-template-columns:\s*minmax\(0, 1fr\);/,
  );
  expect(below(CHAT_RAIL_FOLD)).toMatch(
    /\.chat-frame\s*\{\s*grid-template-columns:\s*minmax\(0, 1fr\);/,
  );

  // THE NUMBERS. At every density the transcript each fold leaves keeps its
  // floor, walked term by term from the tokens so that a term the module
  // dropped is a transcript under the floor here; and at the widest density,
  // one pixel narrower it would not.
  expect([CHAT_RAIL_FOLD, CHAT_THREAD_FOLD]).toEqual([539, 978]);
  const scales = (Object.keys(density) as (keyof typeof density)[]).map(densityScale);
  for (const scale of scales) {
    const gap = Number.parseFloat(spacing["4"]) * scale;
    const besideRail = (screen: number) => screen - CHAT_RAIL_MIN - gap;
    const besideThread = (screen: number) => screen - CHAT_RAIL_MAX - gap - gap - CHAT_THREAD_WIDTH;
    expect(besideRail(CHAT_RAIL_FOLD), `density ${scale}`).toBeGreaterThanOrEqual(TRANSCRIPT_FLOOR);
    expect(besideThread(CHAT_THREAD_FOLD), `density ${scale}`).toBeGreaterThanOrEqual(
      TRANSCRIPT_FLOOR,
    );
    if (scale !== Math.max(...scales)) continue;
    expect(besideRail(CHAT_RAIL_FOLD - 1)).toBeLessThan(TRANSCRIPT_FLOOR);
    expect(besideThread(CHAT_THREAD_FOLD - 1)).toBeLessThan(TRANSCRIPT_FLOOR);
  }
});

/**
 * The screens that still ask the WINDOW at the phone's width, each for a
 * reason that is the window's: whether the whole page scrolls (a phone) or a
 * canvas is legible. A new screen that changes shape for a narrow width asks
 * `useScreenBelow` instead, or adds itself here with its reason.
 */
const ASKS_THE_WINDOW: Record<string, string> = {
  "routes/inbox/Inbox.tsx": "whether the page fills the window or scrolls as a whole",
  "routes/worklet/WorkItem.tsx": "whether the page fills the window or scrolls as a whole",
  "routes/settings/integrations/Directory.tsx":
    "whether the page fills the window or scrolls as a whole",
  "routes/agents/OrgChart.tsx":
    "the canvas is legible at any screen width above a phone (LEGIBLE_ZOOM)",
  "routes/artifacts/new/SlidesCreate.tsx":
    "a phone's touch arrangement; it measures its own width otherwise",
};

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return sources(path);
    return /\.tsx?$/.test(name) && !/\.test\.tsx?$/.test(name) ? [path] : [];
  });
}

test("no screen asks the window at the phone's width without a reason", () => {
  const asking = [...sources(join(SRC, "routes")), ...sources(join(SRC, "components"))]
    .filter((path) =>
      /useMediaMatch\(\s*`\(width < \$\{PHONE_BREAKPOINT\}px\)`/.test(readFileSync(path, "utf8")),
    )
    .map((path) => relative(SRC, path))
    .sort();
  expect(asking).toEqual(Object.keys(ASKS_THE_WINDOW).sort());
});
