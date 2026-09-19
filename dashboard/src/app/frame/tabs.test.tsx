/**
 * A `tab=` off a URL is a string, and the tab set belongs to the object.
 *
 * What this protects is the blank page: every screen with tabs used to cast
 * the parameter to its own union and render `{tab === "overview" && …}` down
 * the page, so a value naming no tab selected nothing and matched no branch.
 * The header and the strip rendered; below them was nothing at all.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { Router } from "../router.tsx";
import { useTab } from "./tabs.ts";

const AGENT = ["overview", "model", "memory"] as const;
const HUMAN = ["overview", "access"] as const;

function at(hash: string, tabs: readonly string[], kind?: "section" | "filter") {
  location.hash = hash;
  return renderHook(() => useTab("tab", tabs, kind), { wrapper: Router });
}

beforeEach(() => {
  location.hash = "#/";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

describe("which tab is real", () => {
  test("a tab the object has is the tab that renders", () => {
    const { result } = at("#/company/people/ada?tab=memory", AGENT);
    expect(result.current[0]).toBe("memory");
  });

  test("a tab the object does not have resolves to the first one", () => {
    // THE REPORTED SHAPE: a bookmark, a hand-typed URL, or a link made before
    // the seat's kind changed. It must land on a tab, never between them.
    const { result } = at("#/company/people/ada?tab=zzz", AGENT);
    expect(result.current[0]).toBe("overview");
  });

  test("the other kind's tab resolves rather than rendering nothing", () => {
    // A human seat has no Memory tab, and `#/company/people/ada?tab=memory`
    // is exactly what a link between two seats produces.
    const { result } = at("#/company/people/ada?tab=memory", HUMAN);
    expect(result.current[0]).toBe("overview");
  });

  test("an empty tab list resolves to nothing rather than throwing", () => {
    const { result } = at("#/company/people/ada?tab=memory", []);
    expect(result.current[0]).toBe(undefined);
  });
});

describe("the URL it writes", () => {
  test("the first tab is the fallback, so landing writes no parameter", () => {
    const { result } = at("#/company/people/ada?tab=memory", AGENT);
    act(() => result.current[1]("overview"));
    expect(location.hash).not.toContain("tab=");
  });

  test("moving to another tab names it", () => {
    const { result } = at("#/company/people/ada", AGENT);
    act(() => result.current[1]("model"));
    expect(location.hash).toContain("tab=model");
  });
});

describe("the digits", () => {
  function press(key: string) {
    act(() => {
      window.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true }));
    });
  }

  test("a digit selects the tab at that position", () => {
    const { result } = at("#/company/people/ada", AGENT);
    press("2");
    expect(result.current[0]).toBe("model");
  });

  test("a digit past the end of this object's tabs is left alone", () => {
    // NOT SWALLOWED: the binding list is the shortcut table, so a strip that
    // could not have used the key never claims it.
    const { result } = at("#/company/people/ada", HUMAN);
    press("3");
    expect(result.current[0]).toBe("overview");
    expect(location.hash).not.toContain("tab=");
  });

  test("a filter leaves the digits for whatever else wants them", () => {
    // A section is the page you are on; a filter narrows what is on it. Only
    // the first takes the keyboard, so a lens inside a peek does not fight an
    // object's own tabs for `1`.
    const { result } = at("#/work/ENG-42", AGENT, "filter");
    press("2");
    expect(result.current[0]).toBe("overview");
  });
});
