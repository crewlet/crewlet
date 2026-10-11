/**
 * A page's body as a reader reads it.
 */

/**
 * The body without a first heading that only repeats the page's title.
 *
 * A seat writing a page often opens it with `# {title}`, and drawn under the
 * page's own title that is the same words twice, the second smaller. Only an
 * EXACT repeat on the first line goes; any other opening heading is content.
 */
export function withoutRepeatedTitle(body: string, title: string): string {
  const match = /^\s*#{1,6}\s+(.+?)\s*#*\s*(?:\n|$)/.exec(body);
  if (!match || match[1]!.trim().toLowerCase() !== title.trim().toLowerCase()) return body;
  return body.slice(match[0].length).replace(/^\s*\n/, "");
}
