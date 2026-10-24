/**
 * What the two link screens outside the frame share: an invitation's and a
 * password reset's. The engine mints both as `#/<screen>/<id>.<secret>`, and
 * both forms hold a password to the floor the engine counts in runes.
 */

/**
 * The two halves of a link's last segment, or null for one that is not a
 * whole link. The id is a uuid and the secret has no dot in it, so neither
 * half can hold the dot that joins them.
 */
export function parseLink(segment: string): { id: string; secret: string } | null {
  const at = segment.indexOf(".");
  if (at <= 0 || at === segment.length - 1) return null;
  return { id: segment.slice(0, at), secret: segment.slice(at + 1) };
}

/** How many characters a password is, as the engine counts them: runes. */
export function characters(text: string): number {
  return [...text].length;
}
