/**
 * The theme, as every picker in the frame offers it: the preferences popover's
 * Theme row, the Change theme step in the command palette, and Settings ›
 * Appearance.
 *
 * ONE ANSWER TO "WHAT DOES CHOOSING A THEME DO". Each surface draws its own
 * list from the design system's parts, and all three apply through
 * [useApplyTheme], so a theme chosen anywhere is written once, says so once,
 * and can be taken back the same way. The colours are never here: a theme is
 * an id, and everything it paints is `@crewlethq/tokens`'.
 */

import { useCallback } from "react";
import { themeGroups, themeRegistry, type ThemeEntry, type ThemeId } from "@crewlethq/tokens";
import { ThemeSwatch, useToast, type SelectOption } from "@crewlethq/ui";
import { chosenTheme, setPalette } from "~/lib/prefs.ts";

/** The registry entry for a theme id. */
export function themeEntry(id: ThemeId): ThemeEntry {
  return themeRegistry.find((theme) => theme.id === id) ?? themeRegistry[0];
}

/** The name of the group a theme is listed under. */
export function themeGroupName(theme: ThemeEntry): string {
  return themeGroups.find((group) => group.id === theme.group)?.name ?? "";
}

/**
 * Every theme as an option of the kit's `Select`, under its group: its two
 * palettes as the leading chip, its name, and its tag at the line's end.
 */
export const THEME_OPTIONS: SelectOption[] = themeRegistry.map((theme) => ({
  value: theme.id,
  label: theme.name,
  hint: theme.tag,
  group: themeGroupName(theme),
  leading: <ThemeSwatch swatch={theme.swatch} size="chip" />,
}));

/**
 * Apply a theme the reader chose, and say so with a way back.
 *
 * THE UNDO GOES BACK TO THE CHOICE, not to the colours: a reader who had chosen
 * nothing (the default, or high contrast because their machine asks for it)
 * is put back to having chosen nothing, so the theme follows the machine
 * again. One toast for every pick, replaced rather than stacked, because the
 * arrows on the Appearance page choose as they move.
 */
export function useApplyTheme(): (id: ThemeId) => void {
  const toast = useToast();
  return useCallback(
    (id: ThemeId) => {
      const before = chosenTheme();
      if (before === id) return;
      setPalette(id);
      toast.ok(`Theme set to ${themeEntry(id).name}`, {
        id: "theme",
        action: { label: "Undo", onClick: () => setPalette(before) },
      });
    },
    [toast],
  );
}
