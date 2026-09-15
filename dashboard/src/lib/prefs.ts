/**
 * What this reader has said about how the product should look and read —
 * theme, density, timezone and date format — persisted per browser and shared
 * by every reader of them.
 *
 * NAMED FOR WHAT IT HOLDS. It was `theme.ts` and already held density; adding
 * a timezone to a file called "theme" is how a name starts lying, and the next
 * person looking for the zone looks in `format.ts` and finds the browser's.
 *
 * Three theme states, not two: `system` follows `prefers-color-scheme` and is
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

export type ThemeChoice = "system" | "light" | "dark";
export type Density = "compact" | "normal" | "comfortable";

/**
 * How a date is written out.
 *
 * `auto` follows the browser's own locale, which is right for almost
 * everybody and is why it is the default. The other two exist because a date
 * of `03/04/2026` means two different days on two sides of one video call, and
 * an operator reading a colleague's screenshot has no way to tell which — so
 * `iso` is offered as the spelling with no ambiguity at all, and `long` as the
 * one with none either, at the cost of width.
 */
export type DateFormat = "auto" | "iso" | "long";

/** The closed sets, in the order a control offers them. */
export const THEMES: ThemeChoice[] = ["system", "light", "dark"];
export const DENSITIES: Density[] = ["compact", "normal", "comfortable"];
export const DATE_FORMATS: DateFormat[] = ["auto", "iso", "long"];

/**
 * The zone the browser is in, which is the default and the honest one: a
 * timestamp is rendered where the reader is unless they say otherwise.
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

/**
 * Whether a string is a zone this browser can actually format in.
 *
 * ASKED OF `Intl`, never matched against a list: the IANA database ships with
 * the runtime and changes with it, so any list here would be a second, stale
 * opinion about which zones exist. A zone it refuses throws, which is exactly
 * the question being asked.
 */
export function zoneExists(zone: string): boolean {
  if (!zone) return false;
  try {
    new Intl.DateTimeFormat(undefined, { timeZone: zone }).format(0);
    return true;
  } catch {
    return false;
  }
}

const THEME_KEY = "crewlet_theme";
const DENSITY_KEY = "crewlet_density";
const ZONE_KEY = "crewlet_timezone";
const DATE_KEY = "crewlet_date_format";

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

let theme = read(THEME_KEY, "system", THEMES) as ThemeChoice;
let density = read(DENSITY_KEY, "normal", DENSITIES) as Density;
let dateFormat = read(DATE_KEY, "auto", DATE_FORMATS) as DateFormat;
// THE ZONE IS NOT A CLOSED SET, so it is validated by asking `Intl` rather
// than by membership — see [zoneExists]. An empty stored value means "the
// browser's", which is a real choice rather than an absent one: a reader who
// travels wants the zone to follow them.
let timezone = readZone();
const listeners = new Set<() => void>();

function readZone(): string {
  let stored = "";
  try {
    stored = localStorage.getItem(ZONE_KEY) ?? "";
  } catch {
    stored = "";
  }
  return stored && zoneExists(stored) ? stored : "";
}

/** The zone every timestamp is rendered in: the reader's choice, or theirs. */
export function zone(): string {
  return timezone || browserZone();
}

/** Whether the zone is the reader's own choice rather than the browser's. */
export function zoneIsChosen(): boolean {
  return timezone !== "";
}

/** How dates are written: the reader's choice. */
export function dates(): DateFormat {
  return dateFormat;
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

export function useDensity(): [Density, (next: Density) => void] {
  const value = useSyncExternalStore(
    subscribe,
    () => density,
    () => "normal" as Density,
  );
  return [value, useCallback(setDensity, [])];
}

/** Set the zone every timestamp renders in. Empty means the browser's. */
export function setZone(next: string): void {
  // A ZONE THIS RUNTIME CANNOT FORMAT IN IS REFUSED rather than stored: it
  // would make every `toLocaleString` on every screen throw, which is a blank
  // product rather than a wrong time.
  if (next !== "" && !zoneExists(next)) return;
  timezone = next;
  write(ZONE_KEY, next);
  announce();
}

/** Set how dates are written. */
export function setDateFormat(next: DateFormat): void {
  if (!DATE_FORMATS.includes(next)) return;
  dateFormat = next;
  write(DATE_KEY, next);
  announce();
}

/** Every preference and its setter, for a surface that offers all of them. */
export interface ViewerPrefs {
  theme: ThemeChoice;
  density: Density;
  /** The zone in effect — the reader's choice, or the browser's. */
  timezone: string;
  /** Whether that zone was chosen rather than inherited. */
  timezoneChosen: boolean;
  dateFormat: DateFormat;
  setTheme: (next: ThemeChoice) => void;
  setDensity: (next: Density) => void;
  setTimezone: (next: string) => void;
  setDateFormat: (next: DateFormat) => void;
}

export function useViewerPrefs(): ViewerPrefs {
  const [themeValue, applyThemeChoice] = useTheme();
  const [densityValue, applyDensityChoice] = useDensity();
  const zoneValue = useSyncExternalStore(subscribe, zone, () => "UTC");
  const chosen = useSyncExternalStore(subscribe, zoneIsChosen, () => false);
  const dateValue = useSyncExternalStore(subscribe, dates, () => "auto" as DateFormat);
  return {
    theme: themeValue,
    density: densityValue,
    timezone: zoneValue,
    timezoneChosen: chosen,
    dateFormat: dateValue,
    setTheme: applyThemeChoice,
    setDensity: applyDensityChoice,
    setTimezone: useCallback(setZone, []),
    setDateFormat: useCallback(setDateFormat, []),
  };
}

/** Apply the stored preferences before React mounts, so there is no flash. */
export function bootTheme(): void {
  applyTheme(theme);
  applyDensity(density);
}

/** Test seam: re-read storage after a test has written to it. */
export function reloadForTest(): void {
  theme = read(THEME_KEY, "system", THEMES) as ThemeChoice;
  density = read(DENSITY_KEY, "normal", DENSITIES) as Density;
  dateFormat = read(DATE_KEY, "auto", DATE_FORMATS) as DateFormat;
  timezone = readZone();
  announce();
}
