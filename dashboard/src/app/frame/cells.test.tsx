/**
 * The typed cells, and the one thing they must never do.
 *
 * # They may not restate a format
 *
 * `lib/format.ts` owns how a number, a count and a duration are SPELLED.
 * These cells wrote their own: `DurationCell` rendered "500ms" where
 * `fmtDuration` writes "500 ms", and `TokenCell` abbreviated five thousand as
 * "5.0k" where `fmtCount` writes "5,000". That is worse than a drift — it is
 * a trap, because adopting a cell would silently change every figure in the
 * column, so the module built to make the product consistent could not be
 * adopted without making it inconsistent.
 *
 * These cases hold each cell against the formatter it composes, which is the
 * only way that stays true: a second spelling is a test failure rather than a
 * number somebody notices on a screen six months later.
 *
 * # Absent is not zero
 *
 * The other half of why a cell exists. "Nothing is estimated" and "everything
 * is estimated at nothing" are different facts, and a grid rendering both as
 * `0` makes the first invisible — while `value || "—"`, which is what three
 * columns in this product did, makes the SECOND invisible instead.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";

import { DurationCell, NumberCell, SeatCell, TokenCell } from "./cells.tsx";
import { fmtCount, fmtDuration } from "~/lib/format.ts";
import { SRC, modules } from "~/test/source.ts";

afterEach(cleanup);

test("a count is spelled the way fmtCount spells it", () => {
  for (const n of [0, 7, 999, 5_000, 12_345, 1_250_000]) {
    cleanup();
    render(<NumberCell value={n} />);
    expect(screen.getByText(fmtCount(n)), String(n)).toBeTruthy();
  }
});

test("tokens are spelled the way every other count is", () => {
  for (const n of [0, 900, 5_000, 250_000, 3_400_000]) {
    cleanup();
    render(<TokenCell value={n} />);
    expect(screen.getByText(fmtCount(n)), String(n)).toBeTruthy();
  }
});

test("a duration is spelled the way fmtDuration spells it", () => {
  for (const ms of [0, 500, 5_000, 95_000, 7_200_000]) {
    cleanup();
    render(<DurationCell ms={ms} />);
    expect(screen.getByText(fmtDuration(ms)), String(ms)).toBeTruthy();
  }
});

test("a zero renders as a zero, in every one of them", () => {
  // NOT a dash. `t.rounds || "—"` said "not measured" about a turn that
  // measurably ran no rounds.
  render(
    <>
      <NumberCell value={0} />
      <TokenCell value={0} />
    </>,
  );
  expect(screen.getAllByText("0").length).toBe(2);
  expect(screen.queryByText(EMPTY_VALUE)).toBeNull();
});

test("an absent value renders a dash that says which absence it is", () => {
  const { container } = render(
    <>
      <NumberCell value={null} />
      <TokenCell value={undefined} />
      <DurationCell ms={null} />
    </>,
  );
  // ONE MARK, THE DESIGN SYSTEM'S. These cells drew a local `Dash` — an em
  // dash in a `.cell-dash` span with a `title` — while newer screens drew
  // `EmptyValue`'s en dash, so the work trash screen showed both at once. And
  // the absence is READ now rather than hovered: a `title` on a span is a
  // tooltip no keyboard reaches, which is a poor home for the one fact that
  // distinguishes "nothing recorded" from "zero".
  const dashes = container.querySelectorAll(".crewlet-empty-value");
  expect(dashes.length).toBe(3);
  expect([...dashes].map((d) => d.textContent)).toEqual([
    `${EMPTY_VALUE}Nothing recorded`,
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
 * `SeatCell` drew its own 22px `.seat-mark` circle holding a robot glyph while
 * the board, the list, the roster and every seat chip drew `Avatar` — so the
 * same engineer was "FE" on the board and an identical generic robot on
 * Search and on a project's Lead panel. Two badges for one seat, and the robot
 * half was the one saying nothing: it drew the KIND, which the roster gives at
 * a glance, in the slot that should have been saying WHO.
 */
test("a seat's badge is its initials, the same ones the board draws", () => {
  const { container } = render(<SeatCell handle="frontend-engineer" name="Frontend Engineer" />);
  // FE, from the name — and `getInitials` would give FE from the handle too,
  // since it splits on hyphens. Either way it is a mark that identifies.
  expect(container.querySelector(".crewlet-avatar")?.textContent).toBe("FE");
});

// AND A HUMAN SEAT IS STILL DRAWN, NOT RUN. The dashed edge is the design
// system's own word for it; the local mark carried the same fact as a dashed
// ring, and losing it would have made a person indistinguishable from an agent
// on every grid in the product.
test("a human seat keeps the drawn edge an agent does not have", () => {
  const { container } = render(<SeatCell handle="ada" name="Ada Lovelace" kind="human" />);
  const badge = container.querySelector(".crewlet-avatar")!;
  expect(badge.className).toContain("dashed");
  cleanup();
  const agent = render(<SeatCell handle="cto" name="Agent CTO" kind="agent" />);
  expect(agent.container.querySelector(".crewlet-avatar")!.className).not.toContain("dashed");
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
      'make one seat tellable from another, and `variant="dashed"` is what ' +
      "makes a human seat tellable from an agent one",
  ).toEqual([]);
});
