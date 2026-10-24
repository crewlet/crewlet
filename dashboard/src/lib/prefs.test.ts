import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { renderHook, act } from "@testing-library/react";
import {
  CONTRAST_THEME_ID,
  bootTheme,
  chosenTheme,
  previewPalette,
  reloadForTest,
  setDensity,
  setPalette,
  setTheme,
  useDensity,
  usePalette,
  useTheme,
} from "./prefs.ts";
import { STORAGE_KEYS } from "./storage.ts";

beforeEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-density");
  document.documentElement.removeAttribute("data-palette");
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

describe("the theme", () => {
  const root = () => document.documentElement;
  const original = window.matchMedia;

  /** A machine that does, or does not, ask for more contrast. */
  function contrast(more: boolean) {
    window.matchMedia = ((query: string) => ({
      ...original(query),
      matches: more && query === "(prefers-contrast: more)",
    })) as typeof window.matchMedia;
  }

  afterEach(() => {
    window.matchMedia = original;
  });

  it("is the default and no attribute while nothing is chosen", () => {
    const { result } = renderHook(() => usePalette());
    expect(result.current[0]).toBe("crewlet");
    expect(chosenTheme()).toBeNull();
    bootTheme();
    expect(root().hasAttribute("data-palette")).toBe(false);
  });

  it("is written as the root's data-palette and kept, and the default is the bare root", () => {
    setPalette("abyss");
    expect(root().getAttribute("data-palette")).toBe("abyss");
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBe("abyss");
    setPalette("crewlet");
    expect(root().hasAttribute("data-palette")).toBe(false);
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBe("crewlet");
    // Forgetting the choice is not choosing the default: the key goes.
    setPalette(null);
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBeNull();
    expect(chosenTheme()).toBeNull();
  });

  it("never changes the mode, and the mode never changes it", () => {
    setPalette("iris");
    setTheme("light");
    expect(root().getAttribute("data-palette")).toBe("iris");
    expect(root().getAttribute("data-theme")).toBe("light");
    setPalette("dusk");
    expect(root().getAttribute("data-theme")).toBe("light");
  });

  it("reads an id no build ships as no choice, and refuses to set one", () => {
    localStorage.setItem(STORAGE_KEYS.palette, "amethyst");
    reloadForTest();
    expect(chosenTheme()).toBeNull();
    expect(renderHook(() => usePalette()).result.current[0]).toBe("crewlet");
    setPalette("ledger");
    setPalette("amethyst" as never);
    expect(root().getAttribute("data-palette")).toBe("ledger");
  });

  it("is high contrast where the machine asks for more and nothing is chosen, and a choice wins", () => {
    contrast(true);
    reloadForTest();
    bootTheme();
    expect(renderHook(() => usePalette()).result.current[0]).toBe(CONTRAST_THEME_ID);
    expect(root().getAttribute("data-palette")).toBe(CONTRAST_THEME_ID);
    // Choosing the default is a choice, and the machine's asking does not undo it.
    setPalette("crewlet");
    reloadForTest();
    expect(renderHook(() => usePalette()).result.current[0]).toBe("crewlet");
  });

  it("previews without writing, and the preview ends on null or on a choice", () => {
    setPalette("violet");
    previewPalette("nocturne");
    expect(root().getAttribute("data-palette")).toBe("nocturne");
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBe("violet");
    expect(renderHook(() => usePalette()).result.current[0]).toBe("violet");
    previewPalette(null);
    expect(root().getAttribute("data-palette")).toBe("violet");
    previewPalette("origin");
    setPalette("ledger");
    expect(root().getAttribute("data-palette")).toBe("ledger");
    previewPalette(null);
    expect(root().getAttribute("data-palette")).toBe("ledger");
  });

  it("shows every reader one value", () => {
    const a = renderHook(() => usePalette());
    const b = renderHook(() => usePalette());
    act(() => a.result.current[1]("nightshade"));
    expect(b.result.current[0]).toBe("nightshade");
  });

  it("follows a choice another tab of this browser made", () => {
    bootTheme();
    const reader = renderHook(() => usePalette());
    localStorage.setItem(STORAGE_KEYS.palette, "abyss");
    localStorage.setItem(STORAGE_KEYS.theme, "dark");
    act(() => {
      window.dispatchEvent(new StorageEvent("storage", { key: STORAGE_KEYS.palette }));
    });
    expect(reader.result.current[0]).toBe("abyss");
    expect(root().getAttribute("data-palette")).toBe("abyss");
    expect(root().getAttribute("data-theme")).toBe("dark");
    // A key that is not a preference changes nothing here.
    localStorage.setItem(STORAGE_KEYS.palette, "dusk");
    act(() => {
      window.dispatchEvent(new StorageEvent("storage", { key: STORAGE_KEYS.recents }));
    });
    expect(reader.result.current[0]).toBe("abyss");
  });
});
