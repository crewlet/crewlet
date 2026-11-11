/**
 * A text held to a cap the engine counts in UTF-8 BYTES, cut at a whole
 * character and marked where it is longer.
 *
 * A cut never splits a character, or an emoji's two halves, so what is left is
 * still text the engine reads as UTF-8; and the mark is counted inside the
 * cap, so a reader sees that more was written and the engine still takes it.
 *
 * ITS OWN MODULE, not `lib/format.ts`'s: only the assistant's chunk and a
 * screen's draft cut a sentence to an engine's cap, so this rides their chunks
 * rather than every reader's first paint, which `format.ts` is part of.
 */

const encoder = new TextEncoder();

/** What marks a text cut short. */
export const CUT_MARK = "…";

/** How many UTF-8 bytes a text takes: the unit each cap here is counted in. */
export function bytesOf(text: string): number {
  return encoder.encode(text).length;
}

/** `text` within `max` bytes: whole where it fits, else cut at a whole character and marked. */
export function clipUtf8(text: string, max: number): string {
  if (bytesOf(text) <= max) return text;
  const room = max - bytesOf(CUT_MARK);
  if (room < 0) return "";
  let kept = "";
  let used = 0;
  for (const ch of text) {
    const n = bytesOf(ch);
    if (used + n > room) break;
    kept += ch;
    used += n;
  }
  return `${kept}${CUT_MARK}`;
}
