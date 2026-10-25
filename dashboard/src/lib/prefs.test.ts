import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, act } from "@testing-library/react";
import {
  CONTRAST_THEME_ID,
  bootTheme,
  browserZone,
  chosenTheme,
  pinZoneForTest,
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
import { dateTimeFormat, keptFormattersForTest } from "./formatters.ts";
import { fmtDate } from "./format.ts";
import { targetLabel } from "./work.ts";
import { shortWhen } from "~/routes/inbox/model.ts";

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

// THE BROWSER'S ZONE IS READ ONCE AND KEPT, because reading it builds a
// formatter and every timestamp on a screen asks for it. It can move under an
// open tab — a laptop opened again across a border, a person changing it in
// another window — so it is read again when the runtime's offsets say it moved,
// when the page comes back into view and when it takes the focus.
describe("the browser's zone", () => {
  /** Answer `zone` from `Intl` from now on, as a system that moved would. */
  function systemZone(zone: string): () => void {
    const real = Intl.DateTimeFormat.prototype.resolvedOptions;
    const moved = vi
      .spyOn(Intl.DateTimeFormat.prototype, "resolvedOptions")
      .mockImplementation(function (this: Intl.DateTimeFormat) {
        return { ...real.call(this), timeZone: zone };
      });
    return () => moved.mockRestore();
  }

  it("is read once, and again when the page comes back into view", () => {
    const own = browserZone();
    const put = systemZone("Asia/Tokyo");
    try {
      // KEPT: the answer moved, and nothing has looked since.
      expect(browserZone()).toBe(own);
      act(() => {
        document.dispatchEvent(new Event("visibilitychange"));
      });
      expect(browserZone()).toBe("Asia/Tokyo");
    } finally {
      put();
      document.dispatchEvent(new Event("visibilitychange"));
    }
    expect(browserZone()).toBe(own);
  });

  // A PERSON WHO CHANGED IT BY HAND comes back to the tab from the window they
  // changed it in, and the tab takes the focus: read again there too, which
  // covers the one move the offsets cannot see.
  it("is read again when the page takes the focus", () => {
    const own = browserZone();
    const put = systemZone("Asia/Tokyo");
    try {
      expect(browserZone()).toBe(own);
      window.dispatchEvent(new Event("focus"));
      expect(browserZone()).toBe("Asia/Tokyo");
    } finally {
      put();
      window.dispatchEvent(new Event("focus"));
    }
    expect(browserZone()).toBe(own);
  });

  // THE ZONE ALSO MOVES WITH THE TAB IN VIEW, and no event says so. Every date
  // written after the move is written in the new zone, whichever formatter
  // writes it and whenever that formatter was built. Moved for real, through
  // the runtime's own zone: a stubbed answer could not reach a formatter built
  // before the stub and kept.
  it("is followed when it moves with the page in view, by every date written after", () => {
    // Berlin's afternoon of the 10th is Auckland's small hours of the 11th.
    const at = "2026-03-10T12:00:00Z";
    const weekLater = Date.parse(at) + 8 * 86_400_000;
    const before = process.env.TZ;
    expect(browserZone()).toBe("Europe/Berlin");
    expect(fmtDate(at)).toBe("Mar 10, 2026");
    expect(shortWhen(at, weekLater)).toBe("Mar 10");
    expect(targetLabel("2026-03-10")).toBe("Mar 10, 2026");
    try {
      process.env.TZ = "Pacific/Auckland";
      expect(fmtDate(at)).toBe("Mar 11, 2026");
      expect(shortWhen(at, weekLater)).toBe("Mar 11");
      // A DAY IS NO INSTANT: a target is the 10th wherever the reader is.
      expect(targetLabel("2026-03-10")).toBe("Mar 10, 2026");
    } finally {
      process.env.TZ = before;
    }
    expect(browserZone()).toBe("Europe/Berlin");
    expect(fmtDate(at)).toBe("Mar 10, 2026");
  });

  // A FORMATTER FOR A ZONE NOBODY ASKS FOR is not kept: a pinned zone and a
  // zone that moved drop them all, and the next ask builds for the zone in use.
  it("drops every kept formatter when a zone is pinned or moves", () => {
    dateTimeFormat(undefined, browserZone(), { weekday: "short" });
    expect(keptFormattersForTest()).toBeGreaterThan(0);
    pinZoneForTest("UTC");
    expect(keptFormattersForTest()).toBe(0);
    pinZoneForTest("");

    dateTimeFormat(undefined, browserZone(), { weekday: "short" });
    const put = systemZone("Asia/Tokyo");
    try {
      window.dispatchEvent(new Event("focus"));
      expect(keptFormattersForTest()).toBe(0);
    } finally {
      put();
      window.dispatchEvent(new Event("focus"));
    }
  });
});
