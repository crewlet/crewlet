/**
 * What the builder's table actually DRAWS, read through the cascade rather
 * than from a stylesheet's text.
 *
 * WHY IT EXISTS. The only guard over this screen's drawing used to read
 * `org.css` as a string and match regular expressions against it. A guard like
 * that passes while the defect ships: it cannot see a rule that reaches
 * nothing the screen renders, a later rule that wins on source order, or a
 * declaration restated on an element that already carried it from the design
 * system. Three defects reached an owner in one week behind exactly that.
 *
 * HOW. The design system's SHIPPED stylesheet (`@crewlethq/ui/dist/styles.css`,
 * which is what a browser loads) and this screen's own go into one document,
 * the real `TableView` is rendered into it, and every answer below is what
 * `getComputedStyle` says about an element this screen drew. jsdom resolves no
 * custom property and lays nothing out, so what is read here is the keyword
 * and the flex answer; the package's own suite measures the lengths those
 * properties resolve to against the token file.
 *
 * WHAT IT PROTECTS.
 * - The table's name cell IS the design system's name group, so the two-line
 *   drawing, the truncation and the trailing slot are that component's.
 * - Nothing on this screen takes that drawing over. Four declarations were
 *   written twice on the same element, byte for byte, and a fifth was inert;
 *   that they are GONE is asserted in `builderStyles.test.ts`, because a
 *   restatement that agrees computes to the same answer and an absence is the
 *   one thing a rendered document cannot show.
 * - The slots this screen does own (a live state, a problem count, a wiring
 *   mark) keep their size, so a push and a check change a word and never a
 *   row's height.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { fixtureCompany } from "./model/testkit.ts";
import { checkedEdit } from "./testState.ts";
import { TableView } from "./TableView.tsx";
import { builderSpies, BuilderHarness, harnessProbe } from "./viewTestkit.tsx";
import { insidePart, orgTableParts } from "~/testing.tsx";

/*
 * Read from the working directory rather than from `import.meta.url`: in a
 * jsdom run the module's own URL is the dev server's, which is not a file.
 */
const read = (path: string) => readFileSync(join(process.cwd(), path), "utf8");

/**
 * This screen's stylesheet. `screens.css` in this tree: the builder is a lens
 * of the company screen, and every screen's recipes live in one sheet here —
 * see the note in `builderStyles.test.ts`. The cascade below is read over the
 * whole sheet, which is what the browser does anyway.
 */
const SCREEN = read("src/styles/screens.css");

/**
 * The design system's, as the dashboard ships it. From the INSTALLED package's
 * `dist`, because what reaches a browser is what the build wrote, not what a
 * source tree says.
 */
const PACKAGE = read("node_modules/@crewlethq/ui/dist/styles.css");

/*
 * What the design system calls the parts of a row, asked of the design system:
 * a suite that spells one of those names is a test of the package.
 */
const PART = orgTableParts();

let sheets: HTMLStyleElement[] = [];

/** Puts stylesheets into the document, in the order a page loads them. */
function apply(...css: string[]) {
  for (const text of css) {
    const style = document.createElement("style");
    style.textContent = text;
    document.head.append(style);
    sheets.push(style);
  }
}

afterEach(() => {
  cleanup();
  for (const style of sheets) style.remove();
  sheets = [];
});

function mount() {
  return render(
    <BuilderHarness
      initial={checkedEdit(fixtureCompany())}
      spies={builderSpies()}
      probe={harnessProbe()}
    >
      <TableView />
    </BuilderHarness>,
  );
}

/** The row whose NAME cell says `name`. */
function row(name: string): HTMLElement {
  const found = screen
    .getAllByRole("row")
    .find((candidate) =>
      [...candidate.querySelectorAll<HTMLElement>(".btable-name")].some(
        (cell) => within(cell).queryAllByText(name, { exact: true }).length > 0,
      ),
    );
  if (!found) throw new Error(`no row named ${name}`);
  return found;
}

/** The box answers of an element, which is what one stylesheet can take from another. */
const box = (element: Element) => {
  const seen = getComputedStyle(element);
  return {
    display: seen.display,
    alignItems: seen.alignItems,
    gap: seen.gap,
    minWidth: seen.minWidth,
    flexGrow: seen.flexGrow,
    flexShrink: seen.flexShrink,
    flexBasis: seen.flexBasis,
  };
};

/*
 * THE NAME CELL IS THE DESIGN SYSTEM'S NAME GROUP. `btable-name` is a HANDLE
 * for this suite, and the class that draws arrives beside it: if it stopped,
 * every rule below would be this screen drawing a name group of its own.
 */
test("the name cell is the package's own name group, and the screen only names it", () => {
  apply(PACKAGE, SCREEN);
  mount();
  const cell = row("Dev").querySelector(".btable-name")!;
  expect(cell.classList.contains(PART.node)).toBe(true);
  expect(box(cell).display).toBe("flex");
  // It FILLS the cell, which is what makes the name truncate and the trailing
  // slot sit at the column's end on every row rather than after the name.
  expect(box(cell).flexGrow).toBe("1");
});

/*
 * AND NOTHING HERE TAKES THAT DRAWING OVER. `.btable-name` declared display,
 * align-items, gap and min-width, and the design system set the same four on
 * the same element, byte for byte, so the screen owned four answers it never
 * meant to own and would have kept them through any change to the package.
 *
 * Read by drawing the table TWICE, once with this screen's stylesheet and once
 * without: what the package decides is what is drawn either way. That a
 * restatement is GONE rather than merely agreeing is `builderStyles.test.ts`,
 * which is where absence can be asserted.
 */
test("this screen never overrides what the package draws in that cell", () => {
  apply(PACKAGE);
  mount();
  const alone = box(row("Dev").querySelector(".btable-name")!);
  const label = getComputedStyle(row("Dev").querySelector(".btable-label")!).minWidth;
  cleanup();

  apply(SCREEN);
  mount();
  const together = box(row("Dev").querySelector(".btable-name")!);
  expect(together).toEqual(alone);
  expect(getComputedStyle(row("Dev").querySelector(".btable-label")!).minWidth).toBe(label);
});

/*
 * A PUSH CHANGES A WORD AND NEVER A ROW. The live state of a saved agent seat
 * arrives twice a tool-loop round; its slot does not shrink, does not grow and
 * does not wrap, so the row it sits in is the same height before and after.
 */
test("the live state slot keeps its room on a drawn row", () => {
  apply(PACKAGE, SCREEN);
  mount();
  const slot = row("Dev").querySelector(".bnode-state")!;
  const seen = getComputedStyle(slot);
  expect(seen.flexGrow).toBe("0");
  expect(seen.flexShrink).toBe("0");
  expect(seen.whiteSpace).toBe("nowrap");
});

/*
 * AND SO DOES A WIRING MARK. It rides the caption, which is the row's second
 * line: as a tag it was the row's own height and a seat's height depended on
 * its wiring. Its size comes from this screen and its place in the line from
 * the package, so both halves are read here.
 */
test("a wiring mark on a row neither grows nor shrinks the line it rides", () => {
  apply(PACKAGE, SCREEN);
  mount();
  const mark = row("Designer").querySelector(".bnode-mark")!;
  expect(insidePart(mark, PART.caption)).toBe(true);
  const seen = getComputedStyle(mark);
  expect(seen.flexGrow).toBe("0");
  expect(seen.flexShrink).toBe("0");
  expect(seen.display).toBe("inline-flex");
});
