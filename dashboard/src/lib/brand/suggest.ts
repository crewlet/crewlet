/**
 * What to do about a contrast pair that falls short: the reason it matters,
 * said for the pair, and one-click changes that keep the colour's hue.
 *
 * THE SHADE IS THE TOKENS PACKAGE'S. `nearestContrastShade` keeps a colour's
 * hue and chroma and moves its lightness to the nearest value that meets the
 * pair's minimum, the one implementation every surface asks; this module only
 * decides WHICH role a pair's fix changes (always one the brand sets, never
 * the theme's fixed dark text or the white or black label the kit picks) and
 * words the offer.
 */

import { contrastRatio, formatContrastRatio } from "@crewlethq/ui";
import { nearestContrastShade } from "@crewlethq/tokens";
import type { BrandDocument } from "~/protocol/index.ts";
import { effectiveColors, isBrandColor, type BrandColorRole, type BrandContrast } from "./brand.ts";

/** Why a pair matters, said with the colour that falls short. */
export function pairReason(pair: BrandContrast): string {
  const fg = pair.foregroundColor;
  const ground = pair.groundColor;
  switch (`${pair.foreground}/${pair.ground}`) {
    case "text/light_ground":
      return `Body text in ${fg} will be hard to read on ${ground}, in every branded doc.`;
    case "on_primary/primary":
      return `Labels on buttons and badges in ${ground} will be hard to read.`;
    case "dark_text/dark_ground":
      return `Text will be hard to read on the dark ground ${ground}.`;
    case "primary/light_ground":
      return `Links and buttons in ${fg} will be hard to see on a light page.`;
    case "primary_on_dark/dark_ground":
      return `Links and buttons in ${fg} will be hard to see on a dark page.`;
    default:
      return `${pair.label} measures below ${pair.min}:1.`;
  }
}

/** One change a pair's panel offers: which role becomes which colour, and the button's words. */
export interface ContrastSuggestion {
  key: string;
  label: string;
  /** The roles it sets, and to what. */
  set: Partial<Record<BrandColorRole, string>>;
}

/**
 * The role a pair's fix changes, and the colour it is measured against: the
 * foreground where the brand sets it, else the ground (the label on the
 * primary is white or black, so the primary moves; the dark mode's text is
 * the theme's, so the dark ground moves).
 */
function fixOf(
  pair: BrandContrast,
): { role: BrandColorRole; from: string; against: string } | null {
  switch (`${pair.foreground}/${pair.ground}`) {
    case "text/light_ground":
      return { role: "text", from: pair.foregroundColor, against: pair.groundColor };
    case "on_primary/primary":
      return { role: "primary", from: pair.groundColor, against: pair.foregroundColor };
    case "dark_text/dark_ground":
      return { role: "dark_ground", from: pair.groundColor, against: pair.foregroundColor };
    case "primary/light_ground":
      return { role: "primary", from: pair.foregroundColor, against: pair.groundColor };
    case "primary_on_dark/dark_ground":
      return { role: "primary_on_dark", from: pair.foregroundColor, against: pair.groundColor };
    default:
      return null;
  }
}

/** A colour and the ratio it reaches, as a button says it: "#007f80 (4.51:1)". */
function shadeWords(shade: string, against: string): string {
  const ratio = contrastRatio(shade, against);
  return ratio === null ? shade : `${shade} (${formatContrastRatio(ratio)}:1)`;
}

/**
 * What a pair that falls short offers: the nearest shade of the colour it
 * changes that meets the minimum ("Use #007f80 (4.51:1)"), and, for a primary
 * too light for a light page that reads on the dark ground, moving it to
 * Primary on dark grounds while the primary takes that shade. Nothing for a
 * pair that passes or cannot be measured yet.
 */
export function contrastSuggestions(
  colors: BrandDocument["colors"],
  pair: BrandContrast,
): ContrastSuggestion[] {
  if (pair.ratio === null || pair.passes) return [];
  const fix = fixOf(pair);
  if (!fix || !isBrandColor(fix.from)) return [];
  const out: ContrastSuggestion[] = [];
  const shade = nearestContrastShade(fix.from, fix.against, pair.min);
  if (shade) {
    out.push({
      key: `${pair.foreground}/${pair.ground}:shade`,
      label: `Use ${shadeWords(shade, fix.against)}`,
      set: { [fix.role]: shade },
    });
  }
  if (pair.foreground === "primary" && pair.ground === "light_ground" && !colors.primary_on_dark) {
    const effective = effectiveColors(colors);
    const onDark = contrastRatio(fix.from, effective.dark_ground ?? "");
    if (onDark !== null && onDark >= pair.min) {
      out.push({
        key: `${pair.foreground}/${pair.ground}:move`,
        label: `Move ${fix.from} to Primary on dark grounds`,
        set: { primary_on_dark: fix.from, ...(shade ? { primary: shade } : {}) },
      });
    }
  }
  return out;
}
