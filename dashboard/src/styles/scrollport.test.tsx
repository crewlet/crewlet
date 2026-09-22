import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Router } from "~/app/router.tsx";
import { WorkGrid } from "~/routes/work/shapes/Grid.tsx";
import type { WorkGroup, WorkSummary } from "~/protocol/index.ts";

/**
 * WHICH SCROLLPORT HOLDS A STICKY BAND — the half `sticky.test.ts` cannot see.
 *
 * That file asks what each band's `top` is, and every band answers
 * `var(--sticky-top)`: the height the toolbar publishes, so a band stops under
 * it rather than beneath it. The offset is only meaningful if the toolbar's own
 * scroller is the band's scroller too. A sticky box is confined to its NEAREST
 * scroll container, so any box between the band and `.screen` that makes one
 * takes the band over — and then `top` is measured from THAT box instead.
 *
 * The failure is not a band that ignores its offset. It is a band that obeys it
 * against the wrong ruler. `.work-list` carried `overflow: hidden` for the
 * rounding, which makes a scroll container; the list has no height constraint,
 * so it grows to its rows and its scroll offset is 0 for ever; and every group
 * heading was therefore held 48px DOWN from the top of the list. Each band left
 * its own slot blank and painted over the first row of its own group — a row in
 * the DOM, laid out, drawn, and covered by an opaque box at `--z-sticky`. On a
 * nine-item list that is one task per group a reader cannot see and cannot
 * click. `overflow: clip` rounds the corners exactly as `hidden` did and makes
 * no scroll container, which is the whole of the fix.
 *
 * # Why it is asserted over a rendered tree rather than over the sheets
 *
 * Neither half is a fact about one rule. The band's `top` is in one sheet, the
 * `overflow` that captured it was eleven lines away in the same file, and what
 * binds them is that one element is inside the other — which is in a component.
 * A case asserting `overflow: clip` on the panel would restate the fix rather
 * than the rule it follows from, and would say nothing at all the day a new
 * panel is wrapped around the rows. So the two sets come from the sheets, the
 * ANCESTRY comes from the component, and what is checked is that no element
 * carrying one set's class sits under an element carrying the other's.
 *
 * jsdom computes no layout, so this can never be a test of what covers what.
 * It does not need to be: containment is structural, and it is the structure
 * that went wrong.
 *
 * # What it walks, and what it does not
 *
 * The work screen's own grid is the one surface walked here, because it is the
 * one whose bands render from plain props. That is now a `DataGrid` — the list
 * and the table collapsed into one renderer with two column sets — so the walk
 * covers `.grid-head` and `.grid-band-head`, the two rules this file used to
 * name as uncovered. Three others take `--sticky-top` and are not walked:
 * `.work-band` on My work's day, `.work-item-side` on an item page,
 * `.list-day` on Activity and `.inbox-detail` on Inbox each draw inside a
 * screen that needs the store, so standing them up would buy less than it
 * would cost to keep honest. The roster is derived from every sheet
 * regardless, so a band added to THIS tree is covered the moment it appears,
 * and the day one of those is rendered here by something else it is covered
 * with no change to this file.
 */

/**
 * FROM THE PROJECT ROOT, not from `import.meta.url`, which the sheet-reading
 * suites beside this one use. Each of those opts into the node environment
 * with a pragma on its first line; this one renders, so it stays on the suite's
 * jsdom default, where `import.meta.url` is served as an `http:` URL and
 * `fileURLToPath` throws before a case starts. Vitest resolves its config from
 * this directory's package root, so `cwd` is `dashboard/`.
 *
 * The pragma is deliberately not quoted anywhere in this file, for a reason
 * worth knowing: vitest scans the whole source for it rather than only the
 * leading block, so a comment MENTIONING it sets it. Naming node here dropped
 * `document` out from under every case in this file.
 */
const STYLES = join(process.cwd(), "src", "styles");

/** Every rule in every sheet, as `[selector, declarations]`. */
function rules(): [string, string][] {
  const out: [string, string][] = [];
  for (const file of readdirSync(STYLES).filter((f) => f.endsWith(".css"))) {
    const bare = readFileSync(join(STYLES, file), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
    // SPLIT ON THE CLOSING BRACE, for the reason `sticky.test.ts` gives at the
    // same idiom: a regex that matches whole rules has to consume the `}` that
    // precedes one to anchor on it, which leaves the next rule without one and
    // silently reports every other rule.
    for (const chunk of bare.split("}")) {
      const at = chunk.lastIndexOf("{");
      if (at < 0) continue;
      // AND FROM THE BRACE BEFORE IT, which is the other half of the same
      // hazard: a rule inside an at-rule shares its chunk with that at-rule's
      // prelude, so everything-before-the-brace reads as
      // `@media (max-width: 860px) { .work-rows` and is then discarded as an
      // at-rule. Nothing in these sheets declares an `overflow` or a sticky
      // band inside a media query today, so the hole is invisible — which is
      // exactly the shape of failure this file exists to catch, and the day
      // one is added the gate would have gone quiet rather than red.
      const selector = chunk
        .slice(chunk.lastIndexOf("{", at - 1) + 1, at)
        .trim()
        .replace(/\s+/g, " ");
      if (!selector || selector.startsWith("@")) continue;
      out.push([selector, chunk.slice(at + 1)]);
    }
  }
  return out;
}

/**
 * The classes a selector can put a rule on an element THROUGH — every class in
 * its rightmost compound, per comma-separated part.
 *
 * Deliberately an over-approximation: `.a .b { overflow: auto }` is recorded
 * against `.b` although it only applies under `.a`. A gate over containment
 * that guessed the other way would answer "nothing to see" for exactly the
 * rules it exists to find, and the classes here are ones this product owns —
 * where an over-approximation fires, the honest answer is to look at the rule.
 */
function subjects(selector: string): string[] {
  const out: string[] = [];
  for (const part of selector.split(",")) {
    const last = part
      .trim()
      .split(/\s+|>|\+|~/)
      .filter(Boolean)
      .pop();
    if (!last) continue;
    for (const m of last.matchAll(/\.([A-Za-z0-9_-]+)/g)) out.push(m[1]!);
  }
  return out;
}

/** Classes whose rule makes a scroll container. `clip` and `visible` do not. */
function scrollportClasses(): Set<string> {
  const out = new Set<string>();
  for (const [selector, body] of rules()) {
    if (!/(?:^|;)\s*overflow(?:-x|-y)?\s*:\s*(auto|scroll|hidden|overlay)\b/.test(body)) continue;
    for (const c of subjects(selector)) out.add(c);
  }
  return out;
}

/** Classes that stick against the offset the toolbar publishes. */
function stickyClasses(): Set<string> {
  const out = new Set<string>();
  for (const [selector, body] of rules()) {
    if (!/position:\s*sticky/.test(body)) continue;
    if (!/(?:^|;)\s*top:\s*[^;]*var\(--sticky-top\)/.test(body)) continue;
    for (const c of subjects(selector)) out.add(c);
  }
  return out;
}

/**
 * Every `sticky element -> the scrollport-making ancestor that captured it`
 * inside `root`, which is what must come back empty.
 */
function captured(root: Element): string[] {
  const sticky = stickyClasses();
  const ports = scrollportClasses();
  const found: string[] = [];
  for (const el of root.querySelectorAll<HTMLElement>("*")) {
    const own = [...el.classList].filter((c) => sticky.has(c));
    if (own.length === 0) continue;
    for (let up = el.parentElement; up && up !== root.parentElement; up = up.parentElement) {
      const bad = [...up.classList].filter((c) => ports.has(c));
      if (bad.length > 0) found.push(`.${own.join(".")} inside .${bad.join(".")}`);
    }
  }
  return found;
}

const row = (id: string, over: Partial<WorkSummary> = {}): WorkSummary => ({
  id,
  key: "ENG-" + id,
  project: "ENG",
  title: "a task called " + id,
  type: "task",
  status: "todo",
  updated: "2031-04-16T00:00:00Z",
  version: 1,
  ...over,
});

const group = (key: string, ids: string[]): WorkGroup => ({
  key,
  count: ids.length,
  rows: ids.map((id) => row(id, { status: key })),
});

/**
 * The grouped list as the work screen draws it: bands, and rows under them.
 *
 * THE ROUTER IS REAL, because the grid underneath reads `sort=` and
 * `cols.<shape>=` off the URL and a grid without one throws before it renders
 * a row.
 */
function grouped() {
  return render(
    <Router>
      <WorkGrid
        shape="list"
        rows={[]}
        groups={[group("todo", ["1", "2"]), group("in_progress", ["3"])]}
        axis="status"
        chrome={{}}
        now={Date.parse("2031-04-16T00:00:00Z")}
        workspace
        hrefOf={(r) => `#/work/${r.key}`}
        onOpen={() => {}}
        onOverflow={() => {}}
        overflowHref={() => "#/work"}
      />
    </Router>,
  );
}

afterEach(cleanup);

test("the two rosters are read from the sheets at all", () => {
  // A GATE OVER AN EMPTY SET CERTIFIES NOTHING, and both sets come from a scan
  // that a change of idiom in the sheets could silently empty.
  expect(stickyClasses().has("work-band")).toBe(true);
  expect(stickyClasses().has("grid-head")).toBe(true);
  expect(stickyClasses().has("grid-band-head")).toBe(true);
  expect(scrollportClasses().has("screen")).toBe(true);
  expect(scrollportClasses().has("work-col-body")).toBe(true);
  // And the two values that are NOT scroll containers are not counted as one,
  // or the fix below would read as the bug.
  expect(scrollportClasses().has("work-list")).toBe(false);
  expect(scrollportClasses().has("grid-wrap")).toBe(false);
});

test("a grouped list's bands are held by the screen, not by the list", () => {
  const { container } = grouped();
  const bands = container.querySelectorAll(".grid-band-head");
  // The rendering has to carry the thing being asserted about.
  expect(bands.length).toBe(2);
  expect(container.querySelectorAll(".grid-head").length).toBe(1);
  expect(captured(container)).toEqual([]);
});

test("and the walk can tell — a scrolling ancestor is reported", () => {
  // THE MUTATION, run rather than described: the panel is given the class of a
  // box that really does scroll, and every band under it comes back named.
  // Without this the two cases above would pass just as happily over a walk
  // that never looked at an ancestor.
  const { container } = grouped();
  container.querySelector(".grid-wrap")!.classList.add("work-col-body");
  expect(captured(container)).toEqual([
    ".grid-head inside .work-col-body",
    ".grid-band-head inside .work-col-body",
    ".grid-band-head inside .work-col-body",
  ]);
});
