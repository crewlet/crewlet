/**
 * Markdown as the TEXT it renders to: one run of inline markdown, a block, or
 * a whole document flattened to one line ([plainText]).
 *
 * WHY IT IS ITS OWN FILE. These are pure string to string functions over the
 * design system's grammar, and the cells that draw an excerpt are on the
 * frame's first load (`lib/work.ts` reads [plainText] for every activity
 * line). The renderer beside them (`lib/markdown.ts`) draws every inline
 * token with its mark and its seat's face, which no first load needs: kept
 * in one file, the first load carried the whole renderer for a string.
 *
 * ONE WALK FOR BOTH. The renderer reads [inlineText] from here for its
 * outline and its anchors, so what a heading's text is and what a cell's
 * excerpt says cannot disagree.
 */

import { markdownInline, parseMarkdown, type MarkdownBlock } from "@crewlethq/ui";

/**
 * One run of inline markdown as the text it renders to.
 *
 * THE SAME WALK AS [renderInline] ([markdownInline]), deliberately. What a code
 * span is, where a destination ends and which emphasis run closes which are
 * decisions this file has already made once; a second set of them for the text
 * case is the drift `textcut` and `whsec` each record in their own package docs.
 * What differs between the two walks is only what a match BECOMES: a node
 * there, a string here.
 */
export function inlineText(text: string): string {
  const out: string[] = [];
  let at = 0;

  for (const { index, end, groups: g } of markdownInline(text)) {
    if (index > at) out.push(text.slice(at, index));
    at = end;

    if (g.esc !== undefined) {
      out.push(g.esc);
    } else if (g.tick !== undefined) {
      // A code span's own content, minus the one padding space CommonMark lets
      // a span carry so it can start with a backtick.
      out.push((g.code ?? "").replace(/^ | $/g, ""));
    } else if (g.src !== undefined) {
      // AN IMAGE IS ITS ALT TEXT, which is what alt text is for. One with no alt
      // contributes nothing rather than a filename nobody wrote.
      out.push(g.alt ?? "");
    } else if (g.href !== undefined) {
      // A LINK IS ITS LABEL, never its destination: the label is the sentence
      // somebody wrote, and a URL in a one-line cell spends the whole line.
      out.push(inlineText(g.label ?? ""));
    } else if (g.auto !== undefined) {
      // An autolink has no label, so the URL IS the text it renders to.
      out.push(g.auto);
    } else {
      out.push(inlineText(g.strong ?? g.strongUnder ?? g.em ?? g.emUnder ?? g.del ?? ""));
    }
  }
  if (at < text.length) out.push(text.slice(at));
  return out.join("");
}

/** One block as text. Every case is the prose the block renders to. */
function blockText(block: MarkdownBlock): string {
  switch (block.kind) {
    case "heading":
    case "paragraph":
      return inlineText(block.text);
    case "code":
      // THE CODE, WITHOUT ITS FENCE. What was inside a fence is still what the
      // comment said; the backticks are the only part that was never content.
      return block.text;
    case "quote":
    case "callout":
      return block.blocks.map(blockText).join(" ");
    case "list":
      return block.items
        .map((item) => {
          const said = item.blocks.map(blockText).join(" ");
          // A TASK ITEM KEEPS ITS BOX. `[ ]` and `[x]` are markdown syntax, and
          // they are the one piece of it whose meaning survives being flattened:
          // dropping them turns "not done yet" into a sentence that reads as a
          // statement of fact.
          if (item.checked === null) return said;
          return (item.checked ? "[x] " : "[ ] ") + said;
        })
        .join(" ");
    case "table":
      return [block.head, ...block.rows].map((row) => row.map(inlineText).join(" ")).join(" ");
    case "rule":
      // A rule is a MARK rather than content: its whole meaning is visual, so it
      // renders to nothing rather than to three dashes in a sentence.
      return "";
  }
}

/**
 * A markdown document as ONE LINE of the prose it renders to.
 *
 * WHY IT EXISTS: a comment body and a task description are markdown by contract
 * (every tool that writes one says so in its own schema), and the engine's
 * `excerpt` is a cut of one of them. Every surface that draws an excerpt in a
 * single cell was therefore drawing `## Understanding the work` with the hashes
 * in it: the same `pre-wrap` bug this file was written to end, surviving in the
 * places too narrow to render blocks into.
 *
 * WHY NOT [renderMarkdown] THERE: those cells are one line inside a grid track,
 * and `.truncate` is `white-space: nowrap`: it cannot clip a block box at all,
 * so an `<h2>` or a `<table>` in one breaks the row rather than being shortened.
 * Several of them are a `<p>`, where a block child is invalid DOM. And an
 * excerpt is a CUT fragment rather than a document: `textcut.Within` ends it
 * mid-construct, which is exactly the case `firstLines` avoids on the one
 * surface that can render blocks.
 *
 * WHITESPACE COLLAPSES TO A SINGLE SPACE and blocks join with one, which is the
 * engine's own rule for the same job (`internal/search`'s `excerptOf` is
 * `strings.Join(strings.Fields(body), " ")`). No separator is invented between
 * blocks: a reader cannot tell a "·" this function added from one somebody
 * typed, and the row already links to the full text.
 */
export function plainText(source: string): string {
  return parseMarkdown(source ?? "")
    .map(blockText)
    .join(" ")
    .replace(/\s+/g, " ")
    .trim();
}
