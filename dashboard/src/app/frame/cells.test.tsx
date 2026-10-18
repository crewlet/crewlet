/**
 * The grid pieces the engine keeps beside uilet's table cells, and the rules
 * every grid in the tree is held to.
 *
 * # A figure is spelled by lib/format.ts
 *
 * `lib/format.ts` owns how a count and a duration are SPELLED. The cells once
 * wrote their own ("500ms" where `fmtDuration` writes "500 ms", "5.0k" where
 * `fmtCount` writes "5,000"), so adopting one silently changed every figure in
 * its column. uilet's `TableNumberCell` takes the host's formatter and
 * defaults to plain locale grouping, so the same trap is one missing prop
 * away; the gate below holds every call site to a formatter of ours.
 *
 * # Absent is not zero
 *
 * The other half of why a cell exists. "Nothing is estimated" and "everything
 * is estimated at nothing" are different facts, and a grid rendering both as
 * `0` makes the first invisible, while `value || "-"` makes the SECOND
 * invisible instead.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { EMPTY_VALUE, TableEntityCell, TableNumberCell } from "@crewlethq/ui";

import { linkedSeat, plainSeat } from "./cells.tsx";
import { fmtCount, fmtDuration } from "~/lib/format.ts";
import { SRC, modules } from "~/test/source.ts";

afterEach(cleanup);

/**
 * EVERY FIGURE CELL NAMES ITS FORMATTER.
 *
 * Scanned as text over the product: each `<TableNumberCell` opening tag must
 * carry a `format=` prop. Without it the figure is spelled by uilet's default
 * grouping, which writes "12,345" in a column whose neighbours write "12.3k".
 */
test("every TableNumberCell is handed one of our formatters", () => {
  const files = modules();
  expect(files.length).toBeGreaterThan(50);
  let seen = 0;
  const offenders: string[] = [];
  for (const { path, text } of files) {
    if (path.endsWith(".test.tsx")) continue;
    for (const match of text.matchAll(/<TableNumberCell\b[\s\S]*?\/>/g)) {
      seen += 1;
      if (!/\bformat=\{/.test(match[0]))
        offenders.push(`${path}: ${match[0].replace(/\s+/g, " ")}`);
    }
  }
  // A scan that matched nothing would pass silently.
  expect(seen).toBeGreaterThan(10);
  expect(
    offenders,
    "a figure is spelled by lib/format.ts: pass format={fmtCount} or format={fmtDuration}",
  ).toEqual([]);
});

test("a count and a duration read as their formatters spell them", () => {
  for (const n of [0, 7, 999, 5_000, 12_345, 1_250_000]) {
    cleanup();
    render(<TableNumberCell value={n} format={fmtCount} />);
    expect(screen.getByText(fmtCount(n)), String(n)).toBeTruthy();
  }
  for (const ms of [0, 500, 5_000, 95_000, 7_200_000]) {
    cleanup();
    render(<TableNumberCell value={ms} format={fmtDuration} />);
    expect(screen.getByText(fmtDuration(ms)), String(ms)).toBeTruthy();
  }
});

test("a zero renders as a zero, and an absence says which absence it is", () => {
  const { container } = render(
    <>
      <TableNumberCell value={0} format={fmtCount} />
      <TableNumberCell value={null} format={fmtCount} />
      <TableNumberCell value={undefined} format={fmtDuration} emptyLabel="Not measured" />
    </>,
  );
  expect(screen.getAllByText("0").length).toBe(1);
  const dashes = container.querySelectorAll(".crewlet-empty-value");
  expect([...dashes].map((d) => d.textContent)).toEqual([
    `${EMPTY_VALUE}Nothing recorded`,
    `${EMPTY_VALUE}Not measured`,
  ]);
});

// ---------------------------------------------------------------------------
// One mark, tree-wide
// ---------------------------------------------------------------------------

/**
 * NOTHING IN THIS TREE SPELLS AN ABSENCE ITSELF.
 *
 * The cases above hold these cells to `EmptyValue`; this holds the rest of the
 * product to it, because the defect was never one component. `@crewlethq/ui`
 * shipped [EmptyValue] and half the screens adopted it while the other half
 * kept a local `Dash` — an em dash the design system's own doc says it "does
 * not use anywhere" — so the work trash screen drew a 12px em dash in the
 * table's DUE column beside a 6px en dash in the activity feed's object
 * column, one fact and two glyphs in one viewport. A per-component test could
 * not see that, and neither could a reviewer: each half looked right alone.
 *
 * SCANNED AS TEXT, and narrowly. An em dash is this codebase's house
 * punctuation — it is in nearly every comment, and `lib/range.ts` spells a
 * window's two edges with one — so what is refused is the single shape that is
 * always a MARK rather than punctuation: an em dash that is the WHOLE of a
 * string literal or the whole of a JSX text node. That is what `return "—"`,
 * `value || "—"` and `<span>—</span>` all are, and it is the exact shape that
 * came back.
 *
 * OVER THE PRODUCT, NOT THE SUITE. A test's own `toContain("—")` is an
 * assertion about a separator or about the absence of the old mark, and both
 * are legitimate; what this gate is about is what a reader sees.
 *
 * NOT A LINT RULE, because there is no rule to write: `no-irregular-whitespace`
 * and friends have nothing to say about a glyph, and a custom ESLint plugin for
 * one string is a plugin nobody maintains.
 */
test("no module spells an absent value with a dash of its own", () => {
  const offenders: string[] = [];
  const files = modules();
  // A scan that walked nothing would pass silently, which is the one failure a
  // gate of this shape has.
  expect(files.length).toBeGreaterThan(50);
  for (const { path, text } of files) {
    const src = text
      // Comments first, and BLANKED RATHER THAN DELETED so the line numbers a
      // failure prints are the file's own. The house style is full of em
      // dashes and every one of them is prose about the code.
      .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
      .replace(/(^|[^:])\/\/[^\n]*/g, (m, lead) => lead + " ".repeat(m.length - lead.length))
      // A REACT KEY IS NOT A MARK. `key={group.key || "—"}` names a list entry
      // whose own key is empty; nothing renders it, and demanding EmptyValue
      // there would put a React element where a string belongs.
      .replace(/\bkey[={:]\s*\{?[^\n}]*\}?/g, "");
    src.split("\n").forEach((line, i) => {
      if (/(["'`])—\1/.test(line) || />\s*—\s*</.test(line)) {
        offenders.push(`${path}:${i + 1}: ${line.trim()}`);
      }
    });
  }
  expect(
    offenders,
    "an absent value is drawn by @crewlethq/ui's EmptyValue (or, in a string, " +
      "its EMPTY_VALUE) — never by a dash spelled at the call site, which is " +
      "how this product came to have two marks for one fact",
  ).toEqual([]);
});

// ---------------------------------------------------------------------------
// One identity badge
// ---------------------------------------------------------------------------

/**
 * A SEAT LOOKS LIKE ITSELF WHEREVER IT APPEARS.
 *
 * The seat cell once drew its own 22px `.seat-mark` circle holding a robot glyph while
 * the board, the list, the roster and every seat chip drew `Avatar` — so the
 * same engineer was "FE" on the board and an identical generic robot on
 * Search and on a project's Lead panel. Two badges for one seat, and the robot
 * half was the one saying nothing: it drew the KIND, which the roster gives at
 * a glance, in the slot that should have been saying WHO.
 */
test("a seat's badge is its initials, the same ones the board draws", () => {
  const { container } = render(
    <TableEntityCell {...linkedSeat({ handle: "frontend-engineer", name: "Frontend Engineer" })} />,
  );
  // FE, from the name — and `getInitials` would give FE from the handle too,
  // since it splits on hyphens. Either way it is a mark that identifies.
  expect(container.querySelector(".crewlet-avatar")?.textContent).toBe("FE");
});

// AND A HUMAN SEAT IS STILL DRAWN AS A PERSON. The badge's outline is the
// design system's own word for it — a circle for a person, a squircle for an
// agent — and losing it would make a person indistinguishable from an agent on
// every grid in the product.
test("a human seat is drawn as a person and an agent as an agent", () => {
  const { container } = render(
    <TableEntityCell {...linkedSeat({ handle: "ada", name: "Ada Lovelace", kind: "human" })} />,
  );
  const badge = container.querySelector(".crewlet-avatar")!;
  expect(badge.classList.contains("crewlet-avatar--human")).toBe(true);
  expect(badge.classList.contains("crewlet-avatar--agent")).toBe(false);
  cleanup();
  const agent = render(<TableEntityCell {...plainSeat({ name: "Agent CTO", kind: "agent" })} />);
  const mark = agent.container.querySelector(".crewlet-avatar")!;
  expect(mark.classList.contains("crewlet-avatar--agent")).toBe(true);
  expect(mark.classList.contains("crewlet-avatar--human")).toBe(false);
});

/**
 * AND NOTHING DRAWS A SECOND ONE.
 *
 * The per-component case above cannot see the defect, because the defect was
 * two components each correct on its own. `.seat-mark` is the class the local
 * badge was drawn with; a tree that still declares it, or still uses it, has a
 * second identity mark again — which is how the product came to have one.
 */
test("no module draws an identity badge of its own", async () => {
  const { readFileSync, readdirSync } = await import("node:fs");
  const { join, relative } = await import("node:path");

  // The modules are the tree's one walk; a stylesheet is not a module, and
  // the class is DECLARED in one, so the sheets are listed here beside them.
  const sheets = (dir: string): string[] =>
    readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
      const at = join(dir, e.name);
      return e.isDirectory() ? sheets(at) : e.name.endsWith(".css") ? [at] : [];
    });
  const css = sheets(SRC).map((at) => ({
    path: relative(SRC, at).split("\\").join("/"),
    text: readFileSync(at, "utf8"),
  }));
  const code = modules();
  // Either half reading nothing would pass silently.
  expect(code.length).toBeGreaterThan(50);
  expect(css.length).toBeGreaterThan(0);
  // COMMENTS BLANKED FIRST, or this gate catches the paragraphs above it — and
  // a gate that forbids its own explanation is a gate somebody deletes.
  const bare = (src: string) =>
    src.replace(/\/\*[\s\S]*?\*\//g, " ").replace(/(^|[^:])\/\/[^\n]*/g, "$1");
  const offenders = [...code, ...css].filter(({ text }) => /\bseat-mark\b/.test(bare(text)));
  expect(
    offenders.map(({ path }) => path),
    "a seat's identity badge is @crewlethq/ui's Avatar — its initials are what " +
      "make one seat tellable from another, and its `kind` outline is what " +
      "makes a human seat tellable from an agent one",
  ).toEqual([]);
});

// A SEAT LINKS TO ITS PAGE, and a seat with no handle is nobody: the link is
// the seat's, so a name alone draws the absence rather than a dead link.
test("a linked seat goes to its page, and no handle is the Nobody absence", () => {
  render(<TableEntityCell {...linkedSeat({ handle: "cto", name: "Agent CTO" })} />);
  expect(screen.getByRole("link", { name: "Agent CTO" }).getAttribute("href")).toBe(
    "#/agents/seats/cto",
  );
  cleanup();
  const { container } = render(
    <TableEntityCell {...linkedSeat({ handle: null, name: "Ghost" })} />,
  );
  expect(container.textContent).toBe(`${EMPTY_VALUE}Nobody`);
});
