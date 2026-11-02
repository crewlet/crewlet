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
 * The head of a body, cut on a LINE boundary so every markdown block it keeps
 * is whole; a fence the cut leaves open is rendered as what it opened. What a
 * page's peek and the Doclet landing show of a page before it is opened.
 */
export function firstLines(body: string, max: number): { head: string; more: number } {
  const lines = body.split("\n");
  if (lines.length <= max) return { head: body, more: 0 };
  return { head: lines.slice(0, max).join("\n"), more: lines.length - max };
}

/**
 * A page's opening: its first `max` lines, without a first heading that only
 * repeats the title, and WITHOUT A HEADING LEFT AT THE END with nothing under
 * it, which a cut on a line makes whenever a section starts at the line
 * budget. What the Doclet landing shows of the page it leads with.
 */
export function openingOf(body: string, title: string, max: number): string {
  const lines = firstLines(withoutRepeatedTitle(body, title), max).head.split("\n");
  while (lines.length > 0 && /^\s*(#{1,6}\s.*)?$/.test(lines[lines.length - 1]!)) lines.pop();
  return lines.join("\n");
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
