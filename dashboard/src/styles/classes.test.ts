// @vitest-environment node
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * Every class the dashboard names must exist in the stylesheets.
 *
 * # Why this is a test and not a convention
 *
 * The stylesheet is one global namespace and `className` is a bare string, so
 * nothing connects the two: a class nobody declared is not an error, not a
 * warning, and not a visual difference a reviewer would notice in a diff. It
 * is silent, and it stays silent for as long as nobody opens that screen.
 *
 * Twelve of them had accumulated when this test was written. `dim` (23 sites)
 * was the muted-text class on two whole screens and had never once set a
 * colour — `.muted` is its real name. `gap-2` (21 sites) asked for the one rung
 * a scale of 1/3/4/6 was missing. `comment` gave a thread its blocks and was
 * never declared, so a work item's comments ran together as one paragraph.
 * `toast-close` left the dismiss X as the browser's grey chrome box. And
 * `stack`, the worst of them, DID exist — as a six-pixel segmented chart bar
 * with `overflow: hidden` — so the work item screen rendered its entire body,
 * every panel, inside a six-pixel strip.
 *
 * That last one is why the check is "declared", not "spelled like a class":
 * the failure was a name that resolved to the wrong component, and the only
 * thing that catches it is reading both sides.
 *
 * # What it cannot see
 *
 * A class assembled at runtime — `` `banner ${tone}` `` — contributes only its
 * literal half. The interpolation is left alone rather than guessed at, since
 * a guess here would either miss variants or invent them. Anything a screen
 * hard-codes, which is nearly all of it, is covered.
 */

const SRC = fileURLToPath(new URL("..", import.meta.url));

/** Every stylesheet in the tree, concatenated. */
function stylesheets(): string {
  const dir = join(SRC, "styles");
  return readdirSync(dir)
    .filter((f) => f.endsWith(".css"))
    .map((f) => readFileSync(join(dir, f), "utf8"))
    .join("\n");
}

/** Class names any stylesheet declares, from every selector in the tree. */
function declared(): Set<string> {
  const dir = join(SRC, "styles");
  const css = readdirSync(dir)
    .filter((f) => f.endsWith(".css"))
    .map((f) => readFileSync(join(dir, f), "utf8"))
    .join("\n");
  const names = new Set<string>();
  for (const [, name] of css.matchAll(/\.(-?[_a-zA-Z][-\w]*)/g)) {
    if (name) names.add(name);
  }
  return names;
}

interface Use {
  name: string;
  where: string;
}

/**
 * Class names a source file hard-codes.
 *
 * Inside `cx(...)` only two positions are a class: a bare string argument, and
 * the right-hand side of a `&&` guard. A string that is being COMPARED is not
 * one — `cx("avatar", size !== "md" && size)` names `avatar`, while `md` is the
 * default it is testing for and `size` is the variant it yields. Reading every
 * quoted string in the call would report `md` as undeclared for ever, and a
 * check that cries wolf is one somebody switches off.
 */
function uses(file: string, text: string): Use[] {
  const found: Use[] = [];
  const add = (raw: string, line: number) => {
    for (const name of raw.split(/\s+/).filter(Boolean)) {
      found.push({ name, where: `${file}:${line}` });
    }
  };
  text.split("\n").forEach((ln, i) => {
    for (const m of ln.matchAll(/className=(?:"([^"]*)"|\{`([^`]*)`\}|\{cx\(([^)]*)\))/g)) {
      if (m[1] !== undefined) add(m[1], i + 1);
      // A template literal contributes its literal half only.
      if (m[2] !== undefined) add(m[2].replace(/\$\{[^}]*\}/g, " "), i + 1);
      if (m[3] !== undefined) {
        for (const arg of m[3].split(",")) {
          const bare = arg.match(/^\s*"([^"]*)"\s*$/);
          if (bare?.[1] !== undefined) {
            add(bare[1], i + 1);
            continue;
          }
          const guarded = arg.match(/&&\s*"([^"]*)"\s*$/);
          if (guarded?.[1] !== undefined) add(guarded[1], i + 1);
        }
      }
    }
  });
  return found;
}

function sources(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) sources(p, out);
    else if (/\.tsx?$/.test(entry) && !/\.test\.tsx?$/.test(entry)) out.push(p);
  }
  return out;
}

describe("every class the dashboard names", () => {
  const css = declared();
  const all = sources(SRC).flatMap((p) => uses(p.slice(SRC.length), readFileSync(p, "utf8")));

  test("is declared by a stylesheet", () => {
    const missing = all.filter((u) => !css.has(u.name));
    expect(missing.map((u) => `${u.name} — ${u.where}`)).toEqual([]);
  });

  test("is found at all, so the scan cannot pass by reading nothing", () => {
    // The check above is vacuous if `uses` returns an empty list, which is
    // exactly what a changed `className=` spelling or a moved source root
    // would do to it. These two are the shapes the tree actually writes.
    expect(all.length).toBeGreaterThan(500);
    expect(all.some((u) => u.name === "panel-head")).toBe(true);
    expect(all.some((u) => u.name === "muted")).toBe(true);
  });

  test("does not include a layout container built from a chart primitive", () => {
    // `.stackbar` is a 6px flex ROW that clips. It belongs to StackedBar and
    // to nothing else: read as a vertical stack it swallows a whole screen.
    const strays = all.filter((u) => u.name === "stackbar" && !u.where.includes("charts.tsx"));
    expect(strays.map((u) => u.where)).toEqual([]);
  });

  // THE TAB PANEL CARRIES A COLUMN, and nothing else in the suite can see it.
  //
  // A screen's switched sections used to be direct children of `.screen-inner`
  // — a flex column with a gap — and took their vertical rhythm from it.
  // Giving the tab widget a real `role="tabpanel"` put one plain element
  // between the column and them, so without these three declarations every
  // panel on the Seat screen renders with its cards butted together.
  //
  // It fails in the one way nothing catches: the markup stays correct, the
  // roles stay correct, every case in groups.test.tsx stays green, and jsdom
  // computes no layout to assert against. A reader would find it by opening
  // the screen. This is a properties assertion rather than a rendered one for
  // exactly that reason — it is the only place the rule can be checked at all.
  test("the tab panel keeps the column its children lost", () => {
    const block = /\.tabpanel\s*\{([^}]*)\}/.exec(stylesheets());
    expect(block, ".tabpanel is not declared at all").not.toBeNull();
    const body = block![1]!;
    expect(body).toMatch(/display:\s*flex/);
    expect(body).toMatch(/flex-direction:\s*column/);
    expect(body).toMatch(/gap:\s*var\(--space-\d+\)/);
  });
});

// AND EVERY STYLESHEET REACHES THE BROWSER.
//
// The gate above proves a class is DECLARED somewhere under `styles/`. It
// cannot prove the declaration is loaded — and a stylesheet nothing imports is
// invisible to the bundler, so the rules in it never ship. That is exactly how
// the frame's own stylesheet came to pass every check while the application
// rendered with none of it: `frame.css` existed, declared every class the rail,
// the sidebar and the page bar use, and `main.tsx` did not import it, so the
// dashboard loaded with a 1440px rail and a stacked page bar.
//
// A missing import has no other symptom: nothing errors, nothing warns, and
// the page renders — just wrongly, and only where somebody looks.
test("every stylesheet is imported by the entry point", () => {
  const entry = readFileSync(join(SRC, "main.tsx"), "utf8");
  const sheets = readdirSync(join(SRC, "styles"))
    .filter((f) => f.endsWith(".css"))
    .sort();
  expect(sheets.length).toBeGreaterThan(0);
  const missing = sheets.filter((f) => !entry.includes(`./styles/${f}`));
  expect(
    missing,
    "these stylesheets are in the tree and nothing imports them, so none of " +
      "their rules reach the browser",
  ).toEqual([]);
});
