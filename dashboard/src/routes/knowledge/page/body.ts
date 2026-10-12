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
  if (!match || !repeatsTitle(match[1]!, title)) return body;
  return body.slice(match[0].length).replace(/^\s*\n/, "");
}

/**
 * Whether a line says nothing but the page's title: the same words, in any
 * case and spacing, which is how the engine compares titles too (a title is an
 * address, lowercased with its runs of whitespace collapsed).
 *
 * A heading's closing hashes are not words, so `Runbook #` repeats `Runbook`.
 */
export function repeatsTitle(line: string, title: string): boolean {
  const said = words(line);
  return said !== "" && said === words(title);
}

function words(text: string): string {
  return text
    .replace(/\s+#+\s*$/, "")
    .replace(/\s+/g, " ")
    .trim()
    .toLowerCase();
}
