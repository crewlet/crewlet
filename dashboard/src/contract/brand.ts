/**
 * The brand kit's closed sets and bounds (`internal/artifacts/brandformat`):
 * the text a brand's name, tagline and voice may run to, the faces a brand may
 * name with no file and the family lists they resolve to, the colours the
 * artifact kit paints where a brand sets none, the contrast pairs an adoption
 * measures, and what a logo or an uploaded face may be.
 *
 * EXACTLY THE ENGINE'S, each held by the gate its row in
 * `internal/clientsource/contract.go` names, as every contract module is.
 *
 * ITS OWN MODULE, apart from `artifacts.ts`, because only the brand's screens
 * read it (Company › Brand, a brand kit's view and the brand's touch points,
 * every one a lazy chunk), while `artifacts.ts` is read by the frame itself:
 * a row here would otherwise ride the first page every reader loads.
 */

/** The longest brand name, in UTF-8 bytes (`brandformat.MaxNameBytes`). */
export const BRAND_NAME_MAX_BYTES = 80;

/** The longest brand tagline, in UTF-8 bytes (`brandformat.MaxTaglineBytes`). */
export const BRAND_TAGLINE_MAX_BYTES = 140;

/** The longest voice summary, in UTF-8 bytes (`brandformat.MaxVoiceSummaryBytes`). */
export const BRAND_VOICE_SUMMARY_MAX_BYTES = 300;

/** The most lines in "Write like this" and in "Avoid" (`brandformat.MaxVoiceExamples`). */
export const BRAND_VOICE_MAX_EXAMPLES = 5;

/** The longest voice line, in UTF-8 bytes (`brandformat.MaxVoiceExampleBytes`). */
export const BRAND_VOICE_EXAMPLE_MAX_BYTES = 160;

/** The faces the artifact kit ships, which a brand may name with no file (`brandformat.KitFaces`). */
export const BRAND_KIT_FACES = ["geist", "geist-mono"] as const;

/** The curated system font stacks a brand may name (`brandformat.SystemStacks`). */
export const BRAND_SYSTEM_STACKS = ["system-sans", "system-serif", "system-mono"] as const;

/**
 * The font-family list each kit face and system stack resolves to
 * (`brandformat.FaceFamilies`), exactly as the generated brand stylesheet writes
 * it. An uploaded family is its name, quoted, before the `system-sans` list.
 */
export const BRAND_FACE_FAMILIES = {
  geist:
    "Geist, -apple-system, BlinkMacSystemFont, Segoe UI, Noto Sans, Helvetica, Arial, sans-serif, Apple Color Emoji, Segoe UI Emoji",
  "geist-mono":
    "Geist Mono, ui-monospace, SFMono-Regular, SF Mono, Menlo, Consolas, Liberation Mono, monospace",
  "system-sans":
    "system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, Roboto, Noto Sans, Helvetica Neue, Arial, sans-serif",
  "system-serif":
    "ui-serif, Iowan Old Style, Palatino Linotype, Palatino, Georgia, Cambria, Times New Roman, serif",
  "system-mono":
    "ui-monospace, SFMono-Regular, SF Mono, Menlo, Consolas, Liberation Mono, monospace",
} as const;

/**
 * The colours the artifact kit paints where a brand sets no input
 * (`brandformat.Fallbacks`): the Crewlet theme's light text and the two modes'
 * page grounds, and the dark mode's text, which a brand never restyles. A brand
 * with no `primary_on_dark` takes its primary. Contrast is measured on these
 * effective values, exactly as the adoption measures it.
 */
export const BRAND_COLOR_FALLBACKS = {
  text: "#121625",
  light_ground: "#f6f7fb",
  dark_ground: "#0c0e16",
  dark_text: "#e7eaf3",
} as const;

/**
 * The colour pairs an adoption measures (`brandformat.ContrastPairs`), in the
 * order the adopt dialog lists them. Only `text` on `light_ground` refuses an
 * adoption; the rest warn. `on_primary` is whichever of white and black
 * contrasts more with the primary; `dark_text` is `BRAND_COLOR_FALLBACKS.dark_text`.
 * Compare the unrounded WCAG 2 ratio (`contrast()` of `@crewlethq/tokens`)
 * against `min`.
 */
export const BRAND_CONTRAST_PAIRS = [
  { foreground: "text", ground: "light_ground", min: 4.5, refuses: true },
  { foreground: "on_primary", ground: "primary", min: 4.5, refuses: false },
  { foreground: "dark_text", ground: "dark_ground", min: 4.5, refuses: false },
  { foreground: "primary", ground: "light_ground", min: 3, refuses: false },
  { foreground: "primary_on_dark", ground: "dark_ground", min: 3, refuses: false },
] as const;

/**
 * An uploaded family's name (`brandformat.FamilyPattern`): an ASCII letter, then
 * letters, digits, single spaces and hyphens. Its length is held apart, by
 * `BRAND_FAMILY_NAME_MAX_BYTES`.
 */
export const BRAND_FAMILY_NAME_PATTERN = "^[A-Za-z](?:[A-Za-z0-9-]| [A-Za-z0-9-])*$";

/** The longest uploaded family name, in bytes (`brandformat.MaxFamilyNameBytes`), the slide format's family bound. */
export const BRAND_FAMILY_NAME_MAX_BYTES = 40;

/** The most WOFF2 files one uploaded family holds (`brandformat.MaxFontFilesPerFamily`). */
export const BRAND_FONT_FILES_MAX = 4;

/** The largest WOFF2 file, in bytes (`brandformat.MaxFontFileBytes`, 2 MiB). */
export const BRAND_FONT_FILE_MAX_BYTES = 2097152;

/** The largest SVG logo, in bytes (`brandformat.MaxSVGBytes`, 512 KiB). */
export const BRAND_SVG_MAX_BYTES = 524288;

/** The largest PNG or WebP logo, in bytes (`brandformat.MaxImageBytes`, 2 MiB). */
export const BRAND_IMAGE_MAX_BYTES = 2097152;

/** The longest side of a PNG or WebP logo, in pixels (`brandformat.MaxImageSide`). */
export const BRAND_IMAGE_MAX_SIDE = 4096;

/** The weights a face's file may draw: the whole hundreds from the least to the most (`brandformat.MinFontWeight`, `MaxFontWeight`, `FontWeightStep`). */
export const BRAND_FONT_WEIGHT_MIN = 100;
export const BRAND_FONT_WEIGHT_MAX = 900;
export const BRAND_FONT_WEIGHT_STEP = 100;

/** The styles a face's file may draw (`brandformat.FontStyles`). */
export const BRAND_FONT_STYLES = ["normal", "italic"] as const;
