/**
 * Settings › Appearance: how the console looks in this browser.
 *
 * # The reader's own, and nobody else's
 *
 * Mode, density and theme are kept per browser (`lib/prefs.ts`) and change
 * nothing of the company's, so this section is in the You group at the head
 * of the column, beside the reader's Account, takes no grant, and asks the
 * engine for nothing. The
 * preferences popover beside the reader's name offers the same three in less
 * room; both read and write one store, so the two never disagree.
 *
 * # Pointing previews in the pane, choosing applies everywhere
 *
 * The gallery's cards are a radio group: a click, or the arrows, choose, and
 * the whole console takes the theme at once. Pointing at a card only shows it
 * in the pane beside the gallery, once the pointer rests, so crossing the grid
 * does not repaint the window at every card it passes. The pane draws the
 * theme in the mode the console is in.
 */

import { useState, type ReactNode } from "react";
import type { ThemeId } from "@crewlethq/tokens";
import {
  Section,
  SegmentedControl,
  ThemeGallery,
  ThemeSwatch,
  resolveTheme,
  useSystemTheme,
} from "@crewlethq/ui";
import { MonitorGlyph, MoonGlyph, SunGlyph } from "@crewlethq/icons/glyphs";
import { themeEntry, useApplyTheme } from "~/app/appearance.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { useViewerPrefs, type Density, type ThemeChoice } from "~/lib/prefs.ts";

const MODES: { value: ThemeChoice; label: string; icon: ReactNode }[] = [
  { value: "system", label: "System", icon: <MonitorGlyph size="sm" /> },
  { value: "light", label: "Light", icon: <SunGlyph size="sm" /> },
  { value: "dark", label: "Dark", icon: <MoonGlyph size="sm" /> },
];

const DENSITY_WORDS: { value: Density; label: string }[] = [
  { value: "compact", label: "Compact" },
  { value: "normal", label: "Normal" },
  { value: "comfortable", label: "Comfortable" },
];

export function Appearance() {
  const prefs = useViewerPrefs();
  const applyTheme = useApplyTheme();
  const system = useSystemTheme();
  // The card the pointer rests on, for the pane alone; null is the theme in effect.
  const [pointed, setPointed] = useState<ThemeId | null>(null);
  const shown = themeEntry(pointed ?? prefs.palette);
  const mode = resolveTheme(prefs.theme, system);

  return (
    <>
      <PageNote>
        How the console looks in this browser. Nobody else sees this choice, and it never changes
        the company.
      </PageNote>
      <div className="appearance">
        <div className="appearance-fields">
          <Section title="Mode" description="Follow the machine, or pin light or dark.">
            <div className="appearance-choice">
              <SegmentedControl
                label="Mode"
                semantics="radio"
                size="sm"
                fill
                options={MODES}
                value={prefs.theme}
                onValueChange={prefs.setTheme}
              />
            </div>
          </Section>
          <Section title="Density" description="How much room every list gives itself.">
            <div className="appearance-choice">
              <SegmentedControl
                label="Density"
                semantics="radio"
                size="sm"
                fill
                options={DENSITY_WORDS}
                value={prefs.density}
                onValueChange={prefs.setDensity}
              />
            </div>
          </Section>
          <Section title="Theme" description="Each theme has a light and a dark variant.">
            <ThemeGallery value={prefs.palette} onValueChange={applyTheme} onPreview={setPointed} />
          </Section>
        </div>
        {/* THE PANE IS A PICTURE of the theme named over it, so it says
            nothing a reader needs beyond that name. */}
        <aside className="appearance-preview" aria-label="Theme preview">
          <span className="t-caption">
            Preview · <b>{shown.name}</b>
          </span>
          <ThemeSwatch swatch={shown.swatch} size="pane" mode={mode} />
        </aside>
      </div>
    </>
  );
}
