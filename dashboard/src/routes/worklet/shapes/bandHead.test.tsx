/**
 * A GROUP'S STUCK HEAD PAINTS OVER ITS ROWS, the editable cells included.
 *
 * The kit's band head sticks under the column heads and the rows scroll under
 * it. A writer's cell lifts its picker over the row's link (`.cell-edit`), and
 * a z-index on it once put the status pill, the priority bars and the avatar
 * of a group's first row over the group's own name. jsdom lays nothing out,
 * so `elementFromPoint` cannot answer here; what decides it is the paint
 * order, worked out from the cascade the kit's grid sheet and this
 * dashboard's rule give each element (CSS 2.1 Appendix E, for positioned
 * boxes and stacking contexts).
 */

import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { DataGrid } from "@crewlethq/ui";
import { sheetRules } from "~/test/sheets.ts";

afterEach(() => {
  cleanup();
  document.head.querySelectorAll("style[data-band-head]").forEach((s) => s.remove());
});

const resolve = createRequire(join(process.cwd(), "package.json")).resolve;
const KIT = dirname(resolve("@crewlethq/ui/styles.css"));
const TOKENS = new Map(
  [
    ...readFileSync(resolve("@crewlethq/tokens/css"), "utf8").matchAll(/(--[\w-]+):\s*([^;]+);/g),
  ].map(([, name, value]) => [name!, value!.trim()]),
);

/** The kit's grid sheet and this dashboard's `.cell-edit`, one selector to a rule (jsdom scores a list by its widest member). */
function installSheets() {
  const kit = readFileSync(
    join(
      KIT,
      readFileSync(join(KIT, "styles.css"), "utf8").match(/DataGrid-[\w]+\.css/)?.[0] ?? "",
    ),
    "utf8",
  );
  const own = sheetRules()
    .filter((r) => r.file === "screens.css" && r.selector === ".cell-edit")
    .map((r) => `${r.selector} {${r.body}}`)
    .join("\n");
  const split = (css: string) =>
    css
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .replace(/([^{}@]+)\{([^{}]*)\}/g, (_, selectors: string, body: string) =>
        selectors
          .split(/,(?![^(]*\))/)
          .map((one) => `${one.trim()} {${body}}`)
          .join("\n"),
      );
  const style = document.createElement("style");
  style.dataset.bandHead = "";
  style.textContent = split(kit) + "\n" + own;
  document.head.append(style);
}

/** A z-index as the cascade leaves it, its var() and calc() resolved. */
function zOf(element: Element): number | "auto" {
  let value = getComputedStyle(element).zIndex.trim();
  if (value === "" || value === "auto") return "auto";
  for (let pass = 0; /var\(/.test(value); pass++) {
    if (pass > 10) throw new Error(`unresolved z-index: ${value}`);
    value = value.replace(
      /var\((--[\w-]+)(?:,\s*([^()]*))?\)/g,
      (_, name: string, fallback?: string) => {
        for (let at: Element | null = element; at; at = at.parentElement) {
          const own = getComputedStyle(at).getPropertyValue(name).trim();
          if (own) return own;
        }
        const resolved = TOKENS.get(name) ?? fallback;
        if (resolved === undefined) throw new Error(`${name} has no value`);
        return resolved;
      },
    );
  }
  const arithmetic = value.replace(/calc\(/g, "(");
  if (!/^[\d\s+\-*/().]+$/.test(arithmetic)) throw new Error(`not a number: ${value}`);
  return Function(`return (${arithmetic});`)() as number;
}

const formsContext = (element: Element): boolean => {
  const style = getComputedStyle(element);
  return (
    style.position === "sticky" ||
    style.position === "fixed" ||
    style.isolation === "isolate" ||
    (style.position !== "static" && zOf(element) !== "auto")
  );
};

/** Whether `top` paints over `under` where the two overlap. */
function paintsOver(top: Element, under: Element): boolean {
  // The nearest stacking context that holds both.
  let context: Element = document.documentElement;
  for (let at = top.parentElement; at; at = at.parentElement) {
    if (at.contains(under) && formsContext(at)) {
      context = at;
      break;
    }
  }
  // Each one's box in that context: its outermost ancestor below the context
  // that forms a context of its own, else itself.
  const place = (element: Element): { box: Element; level: number } => {
    let box = element;
    for (let at: Element | null = element; at && at !== context; at = at.parentElement) {
      if (formsContext(at)) box = at;
    }
    const style = getComputedStyle(box);
    const z = zOf(box);
    // A box in flow paints under every positioned one at level 0.
    return {
      box,
      level: style.position === "static" && !formsContext(box) ? -0.5 : z === "auto" ? 0 : z,
    };
  };
  const a = place(top);
  const b = place(under);
  if (a.level !== b.level) return a.level > b.level;
  // The same level: the later in the tree paints over the earlier.
  return Boolean(b.box.compareDocumentPosition(a.box) & Node.DOCUMENT_POSITION_FOLLOWING);
}

interface Row {
  id: string;
  title: string;
}

test("a group's stuck head paints over its rows' editable cells, and the column heads over both", () => {
  installSheets();
  const { container } = render(
    <DataGrid<Row>
      aria-label="Tasks"
      rowKey={(r) => r.id}
      rowHref={(r) => `#/worklet/${r.id}`}
      columns={[
        {
          key: "status",
          header: "Status",
          shrink: true,
          cell: () => (
            <span className="cell-edit">
              <button type="button">To do</button>
            </span>
          ),
        },
        { key: "title", header: "Title", cell: (r) => r.title },
      ]}
      bands={[
        {
          key: "none",
          label: "No due date",
          rows: [],
          bands: [
            { key: "todo", label: "To do", rows: [{ id: "ENG-1", title: "Write the runbook" }] },
          ],
        },
      ]}
    />,
  );
  const head = container.querySelector(".crewlet-data-grid__head")!;
  const [band, sub] = [...container.querySelectorAll(".crewlet-data-grid__band-head")];
  const pill = container.querySelector(".crewlet-data-grid__row .cell-edit")!;
  const button = pill.querySelector("button")!;
  for (const stuck of [band!, sub!]) {
    expect(paintsOver(stuck, pill), "a band head over the editable cell").toBe(true);
    expect(paintsOver(stuck, button), "a band head over the cell's control").toBe(true);
    expect(paintsOver(head, stuck), "the column heads over a band head").toBe(true);
  }
  // A SUB-BAND'S HEAD SLIDES UNDER ITS PARENT'S as it leaves, never over it.
  expect(paintsOver(band!, sub!)).toBe(true);
});
