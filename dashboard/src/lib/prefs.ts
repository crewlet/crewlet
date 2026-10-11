/**
 * What this reader has said about how the product should look (mode, theme
 * and density) persisted per browser and shared by every reader of them, and
 * the zone every timestamp is drawn in.
 *
 * THE ZONE AND THE DATE SHAPE ARE THE BROWSER'S, and not preferences. A
 * timestamp is drawn in the zone the reader's machine is in and a date in its
 * locale's own spelling, which is right for almost everybody and asks nothing
 * of them; a per-browser override of either was a setting few readers needed
 * and every screenshot had to be read against.
 *
 * MODE AND THEME ARE TWO CHOICES. The mode is light, dark or the system's,
 * kept as `ThemeChoice` and written as `data-theme`, the name the design
 * system's contract gives it; the theme is a named palette from
 * `@crewlethq/tokens`, written as `data-palette`. A theme is a pair of
 * palettes and the mode chooses the half, so neither choice ever changes the
 * other.
 *
 * Three mode states, not two: `system` follows `prefers-color-scheme` and is
 * the DEFAULT. The dashboard this replaces hard-coded `data-theme="dark"` on
 * the html element and had no `prefers-color-scheme` rule anywhere, so a
 * reader whose machine is set to light got a dark page on first visit and no
 * indication that a choice existed.
 *
 * # One store, not one per caller
 *
 * These were `useState` inside the hooks, so every component calling one held
 * its OWN copy: the DOM attribute stayed right — whichever effect ran last
 * wrote it — and the controls disagreed about what was selected. With one
 * caller each that is invisible, which is exactly why it survived; the second
 * caller is what makes a footer toggle read "dark" beside a palette command
 * offering to switch to dark.
 *
 * A module-level value plus `useSyncExternalStore` is the same shape
 * `lib/recents.ts` uses and for the same reason: the preference is one fact
 * about one browser, so it has one home.
 */

import { useCallback, useSyncExternalStore } from "react";
import { DEFAULT_THEME_ID, isThemeId, type ThemeId } from "@crewlethq/tokens";
import { STORAGE_KEYS } from "~/lib/storage.ts";

export type ThemeChoice = "system" | "light" | "dark";
export type Density = "compact" | "normal" | "comfortable";

/** The closed sets, in the order a control offers them. */
export const THEMES: ThemeChoice[] = ["system", "light", "dark"];
export const DENSITIES: Density[] = ["compact", "normal", "comfortable"];

/**
 * The zone the browser is in: a timestamp is rendered where the reader is.
 *
 * WRAPPED, because `resolvedOptions()` is allowed to throw and has been known
 * to answer an empty string in a locked-down environment — and a dashboard
 * that failed to render a date because it could not name a zone would be
 * trading a working screen for a label.
 */
export function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

const THEME_KEY = STORAGE_KEYS.theme;
const DENSITY_KEY = STORAGE_KEYS.density;
const PALETTE_KEY = STORAGE_KEYS.palette;

/**
 * The theme a machine that asks for more contrast gets while its reader has
 * chosen none. A choice the reader makes, the default included, wins over it.
 */
export const CONTRAST_THEME_ID: ThemeId = "high-contrast";
const MORE_CONTRAST = "(prefers-contrast: more)";

function read(key: string, fallback: string, allowed: readonly string[]): string {
  try {
    const stored = localStorage.getItem(key);
    // VALIDATED AGAINST THE SET, not trusted. The value survives upgrades and
    // is editable by hand, and a theme of "purple" would put an attribute on
    // the html element that no stylesheet answers — a page with no colours at
    // all, and nothing to say why.
    return stored && allowed.includes(stored) ? stored : fallback;
  } catch {
    // A privacy mode, a sandboxed iframe, a blocked third-party context. A
    // preference that cannot be read is not a reason to fail to render.
    return fallback;
  }
}

function write(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* the choice applies for this session and is simply not remembered */
  }
}

function forget(key: string): void {
  try {
    localStorage.removeItem(key);
  } catch {
    /* nothing was remembered, or nothing can be */
  }
}

/**
 * The theme this reader chose, or null for none. VALIDATED AGAINST THE
 * REGISTRY the design system exports: an id no build ships (a theme since
 * removed, a hand edit) is no choice, and paints the default rather than a
 * page no stylesheet answers.
 */
function readPalette(): ThemeId | null {
  try {
    const stored = localStorage.getItem(PALETTE_KEY);
    return isThemeId(stored) ? stored : null;
  } catch {
    return null;
  }
}

/** The theme for a reader who chose none: high contrast where the machine asks for it. */
function systemPalette(): ThemeId {
  try {
    return window.matchMedia(MORE_CONTRAST).matches ? CONTRAST_THEME_ID : DEFAULT_THEME_ID;
  } catch {
    return DEFAULT_THEME_ID;
  }
}

function applyTheme(choice: ThemeChoice): void {
  const root = document.documentElement;
  // `system` removes the attribute entirely rather than resolving it here: the
  // stylesheet's media query is what should decide, and it keeps deciding if
  // the reader changes their OS setting while the tab is open.
  if (choice === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", choice);
}

function applyDensity(density: Density): void {
  const root = document.documentElement;
  if (density === "normal") root.removeAttribute("data-density");
  else root.setAttribute("data-density", density);
}

/** The default theme paints the bare root, so it is the absence of the attribute. */
function applyPalette(id: ThemeId): void {
  const root = document.documentElement;
  if (id === DEFAULT_THEME_ID) root.removeAttribute("data-palette");
  else root.setAttribute("data-palette", id);
}

let theme = read(THEME_KEY, "system", THEMES) as ThemeChoice;
let density = read(DENSITY_KEY, "normal", DENSITIES) as Density;
/** What the reader chose; null is no choice. */
let chosenPalette = readPalette();
/** The theme in effect: the choice, or the system's while there is none. */
let palette: ThemeId = chosenPalette ?? systemPalette();
/**
 * A theme a list is showing while the reader decides, painted over the one in
 * effect and never written. NOT ANNOUNCED: nothing renders from it but the
 * root, and every control goes on reading the theme in effect.
 */
let previewing: ThemeId | null = null;
const listeners = new Set<() => void>();

/** Paint what the root should show: the preview while there is one, else the theme in effect. */
function paintPalette(): void {
  applyPalette(previewing ?? palette);
}

/**
 * A zone a suite pins the formatters to, or empty for the browser's. A TEST
 * SEAM ONLY: a date-line edge (Kiritimati, a midnight that moves) cannot be
 * reached from whatever zone the machine running the suite is in.
 */
let pinnedZone = "";

/** The zone every timestamp is rendered in: the browser's. */
export function zone(): string {
  return pinnedZone || browserZone();
}

/** Test seam: draw every timestamp in `next`, or in the browser's zone again for "". */
export function pinZoneForTest(next: string): void {
  pinnedZone = next;
  announce();
}

function announce(): void {
  for (const fn of listeners) fn();
}

function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** Set the theme for this browser, now and on the next visit. */
export function setTheme(next: ThemeChoice): void {
  if (!THEMES.includes(next)) return;
  theme = next;
  applyTheme(next);
  write(THEME_KEY, next);
  announce();
}

/**
 * Set the theme for this browser, now and on the next visit; null forgets the
 * choice, and the theme follows the system again. Applying ends any preview.
 */
export function setPalette(next: ThemeId | null): void {
  if (next !== null && !isThemeId(next)) return;
  chosenPalette = next;
  palette = next ?? systemPalette();
  previewing = null;
  if (next === null) forget(PALETTE_KEY);
  else write(PALETTE_KEY, next);
  paintPalette();
  announce();
}

/**
 * Paint a theme the reader is looking at without choosing it, or end the
 * preview with null. Nothing is written: a preview lives as long as the list
 * that started it, and the list ending it (closing, emptying, the pointer
 * leaving) is what puts back the theme in effect.
 */
export function previewPalette(next: ThemeId | null): void {
  if (next !== null && !isThemeId(next)) return;
  if (previewing === next) return;
  previewing = next;
  paintPalette();
}

/** What the reader chose, or null for none: what an Undo goes back to. */
export function chosenTheme(): ThemeId | null {
  return chosenPalette;
}

/** Set the density for this browser, now and on the next visit. */
export function setDensity(next: Density): void {
  if (!DENSITIES.includes(next)) return;
  density = next;
  applyDensity(next);
  write(DENSITY_KEY, next);
  announce();
}

export function useTheme(): [ThemeChoice, (next: ThemeChoice) => void] {
  const value = useSyncExternalStore(
    subscribe,
    () => theme,
    // The server snapshot: this never renders outside a browser, but a hook
    // that cannot answer one throws in a test environment that asks.
    () => "system" as ThemeChoice,
  );
  return [value, useCallback(setTheme, [])];
}

export function usePalette(): [ThemeId, (next: ThemeId | null) => void] {
  const value = useSyncExternalStore(
    subscribe,
    () => palette,
    () => DEFAULT_THEME_ID,
  );
  return [value, useCallback(setPalette, [])];
}

export function useDensity(): [Density, (next: Density) => void] {
  const value = useSyncExternalStore(
    subscribe,
    () => density,
    () => "normal" as Density,
  );
  return [value, useCallback(setDensity, [])];
}

/** Every preference and its setter, for a surface that offers all of them. */
export interface ViewerPrefs {
  /** The mode. Named `theme` for the attribute it writes, `data-theme`. */
  theme: ThemeChoice;
  density: Density;
  /** The theme in effect. */
  palette: ThemeId;
  setTheme: (next: ThemeChoice) => void;
  setDensity: (next: Density) => void;
  setPalette: (next: ThemeId | null) => void;
}

export function useViewerPrefs(): ViewerPrefs {
  const [themeValue, applyThemeChoice] = useTheme();
  const [densityValue, applyDensityChoice] = useDensity();
  const [paletteValue, applyPaletteChoice] = usePalette();
  return {
    theme: themeValue,
    density: densityValue,
    palette: paletteValue,
    setTheme: applyThemeChoice,
    setDensity: applyDensityChoice,
    setPalette: applyPaletteChoice,
  };
}

/** Read every stored preference again: another tab wrote, or a test did. */
function reread(): void {
  theme = read(THEME_KEY, "system", THEMES) as ThemeChoice;
  density = read(DENSITY_KEY, "normal", DENSITIES) as Density;
  chosenPalette = readPalette();
  palette = chosenPalette ?? systemPalette();
}

let watching = false;

/**
 * Follow what changes outside this tab: another tab of this browser writing a
 * preference, so two tabs never disagree about the theme, and the machine's
 * contrast setting while the reader has chosen no theme. Once per page.
 */
function watch(): void {
  if (watching) return;
  watching = true;
  window.addEventListener("storage", (event) => {
    // A null key is a storage cleared whole, which took every choice with it.
    if (
      event.key !== null &&
      event.key !== THEME_KEY &&
      event.key !== DENSITY_KEY &&
      event.key !== PALETTE_KEY
    ) {
      return;
    }
    reread();
    applyTheme(theme);
    applyDensity(density);
    paintPalette();
    announce();
  });
  try {
    window.matchMedia(MORE_CONTRAST).addEventListener("change", () => {
      if (chosenPalette !== null) return;
      palette = systemPalette();
      paintPalette();
      announce();
    });
  } catch {
    /* a platform with no media queries has no contrast setting to follow */
  }
}

/** Apply the stored preferences before React mounts, so there is no flash. */
export function bootTheme(): void {
  applyTheme(theme);
  applyDensity(density);
  paintPalette();
  watch();
}

/** Test seam: re-read storage after a test has written to it. */
export function reloadForTest(): void {
  reread();
  previewing = null;
  announce();
}
