/**
 * Links pressed while Home is being arranged open in a new tab, so following
 * one never leaves an unsaved board.
 *
 * THE LINK ITSELF OPENS THE TAB: the press is aimed at a new tab for that one
 * press and the link is put back after it, so the same link behaves as written
 * once editing ends. A press the reader already aimed is left alone.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { cleanup, fireEvent, render } from "@testing-library/react";
import { createElement } from "react";
import { openLinksInNewTab } from "./newTab.ts";

/** What the link said while the press reached it, read in the press's own bubble. */
function pressed(
  link: HTMLAnchorElement,
  init: MouseEventInit = {},
  on: HTMLElement = link,
): { target: string | null; rel: string | null } {
  const seen = { target: null as string | null, rel: null as string | null };
  const read = (e: Event) => {
    seen.target = link.getAttribute("target");
    seen.rel = link.getAttribute("rel");
    // jsdom would follow the link; the press is what is under test.
    e.preventDefault();
  };
  link.addEventListener("click", read);
  fireEvent.click(on, init);
  link.removeEventListener("click", read);
  return seen;
}

function board(link: Record<string, string> = {}) {
  const { getByText } = render(
    createElement(
      "div",
      { onClickCapture: openLinksInNewTab },
      createElement(
        "a",
        { href: "#/settings/backups", ...link },
        createElement("span", null, "Backups"),
      ),
    ),
  );
  return getByText("Backups").closest("a")!;
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
  cleanup();
});

describe("a plain press on a link", () => {
  test("opens a new tab, from the link's own words as much as from the link", () => {
    const link = board();
    expect(pressed(link, {}, link.querySelector("span")!)).toEqual({
      target: "_blank",
      rel: "noopener noreferrer",
    });
  });

  test("leaves the link as it was written once the press is over", () => {
    const link = board();
    pressed(link);
    vi.runAllTimers();
    expect(link.hasAttribute("target")).toBe(false);
    expect(link.hasAttribute("rel")).toBe(false);
  });

  test("puts back a target and rel the link already had", () => {
    const link = board({ target: "_self", rel: "help" });
    expect(pressed(link).target).toBe("_blank");
    vi.runAllTimers();
    expect(link.getAttribute("target")).toBe("_self");
    expect(link.getAttribute("rel")).toBe("help");
  });
});

describe("a press the reader already aimed", () => {
  test.each([
    ["the command key", { metaKey: true }],
    ["Control", { ctrlKey: true }],
    ["Shift", { shiftKey: true }],
    ["Alt", { altKey: true }],
    ["a button other than the first", { button: 1 }],
  ])("with %s is left alone", (_, init) => {
    const link = board();
    expect(pressed(link, init).target).toBeNull();
  });

  test("on a link that opens a tab of its own is left alone", () => {
    const link = board({ target: "_blank", rel: "noreferrer" });
    expect(pressed(link)).toEqual({ target: "_blank", rel: "noreferrer" });
  });

  test("on a download link is left alone", () => {
    const link = board({ download: "report.csv" });
    expect(pressed(link).target).toBeNull();
  });

  test("that something before it already handled is left alone", () => {
    const link = board();
    const stop = (e: Event) => e.preventDefault();
    window.addEventListener("click", stop, { capture: true });
    try {
      expect(pressed(link).target).toBeNull();
    } finally {
      window.removeEventListener("click", stop, { capture: true });
    }
  });

  test("on anything but a link is nothing to aim", () => {
    const { getByText } = render(
      createElement(
        "div",
        { onClickCapture: openLinksInNewTab },
        createElement("button", null, "Edit"),
      ),
    );
    expect(() => fireEvent.click(getByText("Edit"))).not.toThrow();
  });
});
