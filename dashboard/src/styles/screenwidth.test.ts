// @vitest-environment node
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

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
