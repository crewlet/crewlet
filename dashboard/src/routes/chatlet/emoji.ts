/**
 * A reaction's one spelling, and how it is drawn.
 *
 * THE ENGINE STORES A REACTION AS A SHORTCODE (`:eyes:`), and agents react in
 * shortcodes, so a reaction made here is sent as one too: sent as the glyph,
 * the same emoji counted twice, once per spelling, and every agent's reaction
 * drew as its raw text. A reaction is DRAWN as its glyph wherever it is shown.
 *
 * The table covers every emoji the kit's picker offers (`EMOJI_GROUPS`, held
 * to it by `emoji.test.ts`), under the standard shortcode names.
 */

import { emojiName } from "@crewlethq/ui";

const SHORTCODES: readonly (readonly [glyph: string, shortcode: string])[] = [
  ["👍", ":+1:"],
  ["✅", ":white_check_mark:"],
  ["👀", ":eyes:"],
  ["🙌", ":raised_hands:"],
  ["🚀", ":rocket:"],
  ["🎉", ":tada:"],
  ["🙏", ":pray:"],
  ["❤️", ":heart:"],
  ["😀", ":grinning:"],
  ["😄", ":smile:"],
  ["😂", ":joy:"],
  ["🙂", ":slightly_smiling_face:"],
  ["😍", ":heart_eyes:"],
  ["🤔", ":thinking:"],
  ["😅", ":sweat_smile:"],
  ["😮", ":open_mouth:"],
  ["😬", ":grimacing:"],
  ["😴", ":sleeping:"],
  ["🤯", ":exploding_head:"],
  ["🥳", ":partying_face:"],
  ["😎", ":sunglasses:"],
  ["🫡", ":saluting_face:"],
  ["🤝", ":handshake:"],
  ["👏", ":clap:"],
  ["🔥", ":fire:"],
  ["💯", ":100:"],
  ["⚠️", ":warning:"],
  ["❌", ":x:"],
  ["⏳", ":hourglass_flowing_sand:"],
  ["📌", ":pushpin:"],
  ["📎", ":paperclip:"],
  ["🧪", ":test_tube:"],
  ["🐛", ":bug:"],
  ["🛠️", ":hammer_and_wrench:"],
  ["📈", ":chart_with_upwards_trend:"],
  ["📉", ":chart_with_downwards_trend:"],
  ["🔒", ":lock:"],
  ["🔑", ":key:"],
  ["💡", ":bulb:"],
  ["📝", ":memo:"],
];

const CODE_OF = new Map(SHORTCODES);
const GLYPH_OF = new Map(SHORTCODES.map(([glyph, code]) => [code, glyph]));

/** The glyphs the table spells, for the suite that holds it to the kit. */
export const SPELLED_GLYPHS: readonly string[] = SHORTCODES.map(([glyph]) => glyph);

/** Whether a stored reaction is written as a shortcode. */
function isShortcode(stored: string): boolean {
  return /^:[^:\s]+:$/.test(stored);
}

/** What a reaction picked here is sent as: its shortcode. */
export function shortcodeOf(glyph: string): string {
  return CODE_OF.get(glyph) ?? glyph;
}

/**
 * The glyph a stored reaction is drawn as. A shortcode the table does not
 * know has none: null, so the caller draws its name rather than its colons.
 */
export function glyphOf(stored: string): string | null {
  if (!isShortcode(stored)) return stored;
  return GLYPH_OF.get(stored) ?? null;
}

/** The words a stored reaction is read by: "eyes", "sparkles". */
export function reactionName(stored: string): string {
  const glyph = glyphOf(stored);
  if (glyph !== null) return emojiName(glyph);
  return stored.slice(1, -1).replace(/[_-]+/g, " ");
}

/** What a reaction shows in a pill or a line: its glyph, or its name. */
export function reactionShown(stored: string): string {
  return glyphOf(stored) ?? reactionName(stored);
}

/** A shortcode written in a message's words: `:tada:`. */
const WRITTEN_SHORTCODE = /:[a-z0-9_+-]+:/gi;

/**
 * Words with every shortcode the table knows drawn as its glyph ("Shipped
 * :tada:" reads "Shipped 🎉"). One it does not know stays as written, colons
 * and all, since it may be words rather than an emoji.
 */
export function emojiInWords(text: string): string {
  if (!text.includes(":")) return text;
  return text.replace(WRITTEN_SHORTCODE, (code) => GLYPH_OF.get(code.toLowerCase()) ?? code);
}
