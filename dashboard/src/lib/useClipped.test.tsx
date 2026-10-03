/**
 * Whether a clamp cut anything is MEASURED, never guessed from a length: the
 * same goal is three lines in a 340px side and one on a wide phone.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { useRef } from "react";
import { afterEach, expect, test } from "vitest";

import { isClipped, useClipped } from "./useClipped.ts";

afterEach(cleanup);

/** A box of the given drawn and laid-out heights, as jsdom lays out none. */
function box(scrollHeight: number, clientHeight: number): HTMLElement {
  const el = document.createElement("span");
  Object.defineProperty(el, "scrollHeight", { value: scrollHeight });
  Object.defineProperty(el, "clientHeight", { value: clientHeight });
  return el;
}

test("a box taller inside than it is drawn is clipped; a rounding pixel is not", () => {
  expect(isClipped(box(66, 44))).toBe(true);
  expect(isClipped(box(45, 44))).toBe(false);
  expect(isClipped(box(44, 44))).toBe(false);
});

function Probe({ inner, drawn }: { inner: number; drawn: number }) {
  const root = useRef<HTMLDivElement>(null);
  const clipped = useClipped(root, `${inner}/${drawn}`);
  return (
    <div ref={root}>
      <span
        className="clamp"
        ref={(el) => {
          if (!el) return;
          Object.defineProperty(el, "scrollHeight", { value: inner, configurable: true });
          Object.defineProperty(el, "clientHeight", { value: drawn, configurable: true });
        }}
      >
        words
      </span>
      <output>{clipped ? "cut" : "whole"}</output>
    </div>
  );
}

test("the hook reads every clamp under the element, after layout", () => {
  render(<Probe inner={90} drawn={66} />);
  expect(screen.getByRole("status").textContent).toBe("cut");
  cleanup();
  render(<Probe inner={40} drawn={40} />);
  expect(screen.getByRole("status").textContent).toBe("whole");
});
