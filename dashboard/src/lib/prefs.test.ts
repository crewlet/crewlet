import { beforeEach, describe, expect, it } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { reloadForTest, setDensity, setTheme, useDensity, useTheme } from "./prefs.ts";

beforeEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-density");
  reloadForTest();
});

describe("the preference is one fact with one home", () => {
  it("shows every reader the same value", () => {
    // These were `useState` inside the hooks, so every component calling one
    // held its OWN copy: the DOM attribute stayed right — whichever effect
    // ran last wrote it — and the controls disagreed about what was selected.
    // With one caller each that is invisible, which is why it survived.
    const a = renderHook(() => useTheme());
    const b = renderHook(() => useTheme());
    act(() => a.result.current[1]("dark"));
    expect(a.result.current[0]).toBe("dark");
    expect(b.result.current[0]).toBe("dark");
  });

  it("does the same for density", () => {
    const a = renderHook(() => useDensity());
    const b = renderHook(() => useDensity());
    act(() => b.result.current[1]("compact"));
    expect(a.result.current[0]).toBe("compact");
    expect(b.result.current[0]).toBe("compact");
  });
});

describe("what reaches the document", () => {
  it("names the choice as an attribute, and removes it for system", () => {
    setTheme("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    // `system` removes the attribute rather than resolving it here: the
    // stylesheet's media query is what should decide, and it keeps deciding
    // if the reader changes their OS setting while the tab is open.
    setTheme("system");
    expect(document.documentElement.getAttribute("data-theme")).toBeNull();
  });

  it("removes the density attribute for the default", () => {
    setDensity("comfortable");
    expect(document.documentElement.getAttribute("data-density")).toBe("comfortable");
    setDensity("normal");
    expect(document.documentElement.getAttribute("data-density")).toBeNull();
  });
});

describe("a stored value this build cannot use", () => {
  it("falls back to the default rather than putting it on the element", () => {
    // The value is editable by hand and survives upgrades. A theme of
    // "purple" would put an attribute on the html element that no stylesheet
    // answers — a page with no colours at all, and nothing to say why.
    localStorage.setItem("crewlet_theme", "purple");
    localStorage.setItem("crewlet_density", "enormous");
    reloadForTest();
    const { result } = renderHook(() => useTheme());
    expect(result.current[0]).toBe("system");
    expect(renderHook(() => useDensity()).result.current[0]).toBe("normal");
  });

  it("refuses to set one either", () => {
    setTheme("light");
    setTheme("purple" as never);
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });
});
