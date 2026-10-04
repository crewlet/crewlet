import { afterEach, describe, expect, test } from "vitest";
import { cleanup, render } from "@testing-library/react";

import { REDUCED_MOTION, SpanBar } from "./SpanBar.tsx";

const original = globalThis.matchMedia;

function reduceMotion(on: boolean) {
  Object.defineProperty(globalThis, "matchMedia", {
    configurable: true,
    writable: true,
    value: (query: string) => ({
      matches: on && query === REDUCED_MOTION,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }),
  });
}

afterEach(() => {
  cleanup();
  Object.defineProperty(globalThis, "matchMedia", {
    configurable: true,
    writable: true,
    value: original,
  });
});

function bar(container: HTMLElement) {
  return container.querySelector(".span-bar") as HTMLElement;
}

describe("SpanBar", () => {
  test("a running span drifts, and holds still for a reader who asked for less motion", () => {
    reduceMotion(false);
    const moving = bar(render(<SpanBar kind="tool" left={0.1} width={0.2} open />).container);
    expect(moving.classList.contains("drifting")).toBe(true);
    expect(moving.dataset.motion).toBe("drift");
    cleanup();

    reduceMotion(true);
    const still = bar(render(<SpanBar kind="tool" left={0.1} width={0.2} open />).container);
    // STILL RUNNING, still striped — only the movement is gone.
    expect(still.classList.contains("open")).toBe(true);
    expect(still.classList.contains("drifting")).toBe(false);
    expect(still.dataset.motion).toBe("static");
  });

  test("a finished span does not move at all", () => {
    reduceMotion(false);
    const done = bar(render(<SpanBar kind="model" left={0} width={0.5} />).container);
    expect(done.classList.contains("drifting")).toBe(false);
    expect(done.dataset.motion).toBeUndefined();
  });

  test("it is placed by its fractions and never narrower than a mark", () => {
    reduceMotion(false);
    const b = bar(render(<SpanBar kind="model" left={0.25} width={0} failed selected />).container);
    expect(b.style.insetInlineStart).toBe("25%");
    expect(b.style.inlineSize).toContain("max(2px");
    expect(b.classList.contains("failed")).toBe(true);
    expect(b.classList.contains("selected")).toBe(true);
    expect(b.getAttribute("aria-hidden")).toBe("true");
  });
});
