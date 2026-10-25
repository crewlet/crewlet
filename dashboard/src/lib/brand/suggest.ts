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

/** Why a low pair matters, in one short line: what would be hard to read or see, and where. */
export function pairReason(pair: BrandContrast): string {
  switch (`${pair.foreground}/${pair.ground}`) {
    case "text/light_ground":
      return "Hard to read as body text on light pages.";
    case "on_primary/primary":
      return "Hard to read as labels on buttons and badges.";
    case "dark_text/dark_ground":
      return "Hard to read as text on dark pages.";
    case "primary/light_ground":
      return "Hard to see as links and buttons on light pages.";
    case "primary_on_dark/dark_ground":
      return "Hard to see as links and buttons on dark pages.";
    default:
      return `Below ${pair.min}:1.`;
  }
}

/** What a low pair needs, beside its badge's measure: "needs 3:1". */
export function pairNeeds(pair: BrandContrast): string {
  return `needs ${pair.min}:1`;
}

/**
 * One change a pair's panel offers, as an answer option says it: the colour
 * (`label`, "#00a0a0") or the move ("Use on dark grounds"), a quiet line on
 * what it gives (`detail`, "3:1, meets the minimum"), the chip's colour, and
 * the roles it sets.
 */
export interface ContrastSuggestion {
  key: string;
  label: string;
  detail: string;
  /** The colour the offer applies, for its chip; none for a move. */
  color?: string | undefined;
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

/** The minimum body text needs: the shade a link reads at when it is text. */
const TEXT_MIN = 4.5;

/** A shade's offer: "#00a0a0", with the ratio it reaches and what that is good for. */
function shadeOffer(
  key: string,
  role: BrandColorRole,
  shade: string,
  against: string,
  goodFor: string,
): ContrastSuggestion {
  const ratio = contrastRatio(shade, against);
  const measured = ratio === null ? "" : `${formatContrastRatio(ratio)}:1, `;
  return {
    key,
    label: shade,
    detail: `${measured}${goodFor}`,
    color: shade,
    set: { [role]: shade },
  };
}

/**
 * What a pair that falls short offers: the nearest shade of the colour it
 * changes that meets the minimum ("#00a0a0", "3:1, meets the minimum"); for a
 * primary too light for a light page, also the shade that reads as text links
 * ("4.5:1, best for text links") and, where it reads on the dark ground, using
 * it there instead (it moves to Primary on dark grounds and the primary takes
 * the nearest shade). Nothing for a pair that passes or cannot be measured.
 */
export function contrastSuggestions(
  colors: BrandDocument["colors"],
  pair: BrandContrast,
): ContrastSuggestion[] {
  if (pair.ratio === null || pair.passes) return [];
  const fix = fixOf(pair);
  if (!fix || !isBrandColor(fix.from)) return [];
  const out: ContrastSuggestion[] = [];
  const id = `${pair.foreground}/${pair.ground}`;
  const shade = nearestContrastShade(fix.from, fix.against, pair.min);
  if (shade) out.push(shadeOffer(`${id}:shade`, fix.role, shade, fix.against, "meets the minimum"));
  const primaryOnLight = pair.foreground === "primary" && pair.ground === "light_ground";
  if (primaryOnLight && pair.min < TEXT_MIN) {
    const text = nearestContrastShade(fix.from, fix.against, TEXT_MIN);
    if (text && text !== shade) {
      out.push(shadeOffer(`${id}:text`, fix.role, text, fix.against, "best for text links"));
    }
  }
  if (primaryOnLight && !colors.primary_on_dark) {
    const effective = effectiveColors(colors);
    const onDark = contrastRatio(fix.from, effective.dark_ground ?? "");
    if (onDark !== null && onDark >= pair.min) {
      out.push({
        key: `${id}:move`,
        label: "Use on dark grounds",
        detail: `Keep ${fix.from}, use it on dark grounds`,
        set: { primary_on_dark: fix.from, ...(shade ? { primary: shade } : {}) },
      });
    }
  }
  return out;
}
