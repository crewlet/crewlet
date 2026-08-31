/**
 * Theme and density, persisted per browser.
 *
 * Three theme states, not two: `system` follows `prefers-color-scheme` and is
 * the DEFAULT. The dashboard this replaces hard-coded `data-theme="dark"` on
 * the html element and had no `prefers-color-scheme` rule anywhere, so a
 * reader whose machine is set to light got a dark page on first visit and no
 * indication that a choice existed.
 */

import { useCallback, useEffect, useState } from "react";

export type ThemeChoice = "system" | "light" | "dark";
export type Density = "compact" | "normal" | "comfortable";

const THEME_KEY = "crewlet_theme";
const DENSITY_KEY = "crewlet_density";

function read(key: string, fallback: string): string {
  try {
    return localStorage.getItem(key) ?? fallback;
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

export function useTheme(): [ThemeChoice, (next: ThemeChoice) => void] {
  const [choice, setChoice] = useState<ThemeChoice>(() => read(THEME_KEY, "system") as ThemeChoice);
  useEffect(() => applyTheme(choice), [choice]);
  const set = useCallback((next: ThemeChoice) => {
    setChoice(next);
    write(THEME_KEY, next);
  }, []);
  return [choice, set];
}

export function useDensity(): [Density, (next: Density) => void] {
  const [density, setDensity] = useState<Density>(() => read(DENSITY_KEY, "normal") as Density);
  useEffect(() => applyDensity(density), [density]);
  const set = useCallback((next: Density) => {
    setDensity(next);
    write(DENSITY_KEY, next);
  }, []);
  return [density, set];
}

/** Apply the stored preferences before React mounts, so there is no flash. */
export function bootTheme(): void {
  applyTheme(read(THEME_KEY, "system") as ThemeChoice);
  applyDensity(read(DENSITY_KEY, "normal") as Density);
}
