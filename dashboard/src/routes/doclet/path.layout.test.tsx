/**
 * A PATH'S SEPARATOR NEVER ENDS A LINE. A page's place ("Core / ADR 1: …")
 * and the composer's File it in ("[space] / [page]") are written with
 * `PathSteps`, which binds each "/" to the segment it introduces: a path too
 * long for its line breaks before a "/", and a long segment wraps its words
 * beside it. Drawn as loose items of one wrapping row, the "/" stood alone on
 * a line under the space's name at every width, and dangled at the end of a
 * phone's line with the select it introduces on the next.
 *
 * A measurement, so it runs in a headless browser (`~/test/layout.tsx`).
 */
import "~/test/layout.tsx";
import { afterEach, expect, test } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import type { ReactNode } from "react";
import { DescriptionList, LayerHost, Select } from "@crewlethq/ui";

import { PathSteps } from "./parts.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

function mount(node: ReactNode, width: number): HTMLElement {
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() => root!.render(<LayerHost>{node}</LayerHost>));
  return host;
}

/** The top of the first line `el` draws on. */
const firstLine = (el: Element) => Math.round(el.getClientRects()[0]!.top);
const middle = (el: Element) => {
  const r = el.getClientRects()[0]!;
  return r.top + r.height / 2;
};

/** Every "/" of the path, with what it introduces. */
function separators(box: HTMLElement): { slash: Element; segment: Element }[] {
  return [...box.querySelectorAll(".knowledge-step")].map((step) => ({
    slash: step.firstElementChild!,
    segment: step.lastElementChild!,
  }));
}

test("a page's place breaks before a '/', never after it, at a phone's and a desktop's measure", () => {
  for (const width of [240, 420]) {
    const box = mount(
      <DescriptionList dense className="knowledge-props">
        <DescriptionList.Item term="Space">
          <PathSteps>
            <a className="knowledge-props-link" href="#/doclet/ENG">
              Core
            </a>
            <a className="knowledge-props-link" href="#/doclet/pages/p-1">
              ADR 1: Add GPU-aware scheduling for MIG partitions on bare metal nodes
            </a>
          </PathSteps>
        </DescriptionList.Item>
      </DescriptionList>,
      width,
    );
    const [step] = separators(box);
    expect(step, `width ${width}`).toBeTruthy();
    // THE "/" IS ON ITS SEGMENT'S FIRST LINE, never alone above it.
    expect(firstLine(step!.slash), `width ${width}`).toBe(firstLine(step!.segment));
    // AND THE SEGMENT WRAPS BESIDE IT rather than running past the box.
    expect(box.scrollWidth, `width ${width}`).toBeLessThanOrEqual(width);
    flushSync(() => root?.unmount());
    host?.remove();
  }
});

test("the composer's '/' goes to the next line with the select it introduces", () => {
  const options = [{ value: "", label: "Top of the space" }];
  const box = mount(
    <div className="knowledge-composer-row" style={{ width: 240 }}>
      <span className="knowledge-composer-label">File it in</span>
      <PathSteps>
        <Select
          size="sm"
          width="auto"
          ariaLabel="The space"
          value={undefined}
          placeholder="Choose a space"
          options={[{ value: "ENG", label: "Core" }]}
        />
        <Select
          size="sm"
          width="auto"
          ariaLabel="The page it goes under"
          value=""
          options={options}
        />
      </PathSteps>
    </div>,
    240,
  );
  const [step] = separators(box);
  const under = box.querySelector('[aria-label="The page it goes under"]')!;
  const space = box.querySelector('[aria-label="The space"]')!;
  // THE ROW HAD TO BREAK: the label and two selects do not fit one 240px line.
  expect(firstLine(under)).toBeGreaterThan(firstLine(space));
  // AND THE "/" BROKE WITH THE SELECT, centred on its line.
  expect(Math.abs(middle(step!.slash) - middle(under))).toBeLessThan(4);
});
