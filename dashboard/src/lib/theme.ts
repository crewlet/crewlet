/**
 * Theme and density, persisted per browser and shared by every reader of them.
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

/** The closed sets, in the order a control offers them. */
export const THEMES: ThemeChoice[] = ["system", "light", "dark"];
export const DENSITIES: Density[] = ["compact", "normal", "comfortable"];

const THEME_KEY = "crewlet_theme";
const DENSITY_KEY = "crewlet_density";

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
const listeners = new Set<() => void>();

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

/** Both preferences and their setters, for a surface that offers all of them. */
export interface Appearance {
  theme: ThemeChoice;
  density: Density;
  setTheme: (next: ThemeChoice) => void;
  setDensity: (next: Density) => void;
}

export function useAppearance(): Appearance {
  const [themeValue, applyThemeChoice] = useTheme();
  const [densityValue, applyDensityChoice] = useDensity();
  return {
    theme: themeValue,
    density: densityValue,
    setTheme: applyThemeChoice,
    setDensity: applyDensityChoice,
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
  announce();
}
