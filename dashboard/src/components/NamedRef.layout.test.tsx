/**
 * A REFERENCE AT THE END OF A LINE CUT TO ONE ROW KEEPS ITS WORDS IN THE LINE.
 * A feed row is one line with an ellipsis (`.truncate`), and the kit draws a
 * reference as one atomic box: crossing the row's edge, the box was clipped
 * whole, so the row read "…work item ▢" with the key it names gone. A line
 * drawn as cut (`EngineLine`'s `cut`) has its references flow (the kit's
 * `InlineRef` `flow`), so the row's ellipsis cuts their words after the mark.
 * Laid out in a real browser, which is the only place a cut is a measurement.
 */
import "~/test/layout.tsx";
import { afterEach, expect, test } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { EngineLine } from "./InlineToken.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  root?.unmount();
  host?.remove();
  root = null;
  host = null;
});

const LINE = "Site Reliability Engineer was handed its next ready work item ENG-287";
const REFS = [{ kind: "work_item", id: "0198-task", text: "ENG-287" }];

/** Where the reference's words start. */
function wordsStart(at: Element): number {
  const range = document.createRange();
  range.selectNodeContents(at.querySelector(".crewlet-inline-ref__text")!);
  return range.getClientRects()[0]!.left;
}

for (const cut of ["truncate", "live-doing"]) {
  test(`a reference crossing a .${cut} row's edge is cut inside its words, not clipped whole`, () => {
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
    flushSync(() =>
      root!.render(
        <span className={cut} style={{ display: "block", width: 600 }}>
          <EngineLine text={LINE} refs={REFS} cut />
        </span>,
      ),
    );
    const row = host.querySelector<HTMLElement>(`.${cut}`)!;
    // THE ROW'S EDGE THROUGH THE KEY, whatever the face measures.
    row.style.width = `${wordsStart(row) - row.getBoundingClientRect().left + 12}px`;
    const ref = row.querySelector<HTMLElement>(".crewlet-inline-ref")!;
    expect(wordsStart(row)).toBeLessThan(row.getBoundingClientRect().right);
    // NO ATOMIC BOX: the row's own ellipsis cuts the words.
    expect(getComputedStyle(ref).display).toBe("inline");
  });
}

test("a line that is not cut keeps the kit's box, which moves to the next line whole", () => {
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <p style={{ width: 200 }}>
        <EngineLine text={LINE} refs={REFS} />
      </p>,
    ),
  );
  expect(getComputedStyle(host.querySelector(".crewlet-inline-ref")!).display).toBe("inline-block");
});
