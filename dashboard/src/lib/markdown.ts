/**
 * A CommonMark subset, rendered to React nodes.
 *
 * Pages and work-item descriptions are MARKDOWN BY CONTRACT — the tools that
 * write them say so, the agents write it, and the engine stores it verbatim —
 * and the dashboard rendered every one of them as `white-space: pre-wrap`. A
 * reader saw `## Decision` and `- [ ] ship it` as literal characters, and a
 * link as its own URL in angle brackets.
 *
 * HAND-ROLLED, and the two reasons are the ones the rest of this engine gives
 * for its own small implementations. A markdown library's real surface is its
 * HTML passthrough and the sanitiser that has to follow it, and this renderer
 * needs NEITHER: it never emits raw HTML, at all, under any input, so there is
 * nothing to sanitise and no `dangerouslySetInnerHTML` anywhere on this path.
 * And the subset a company's own pages actually use is small enough to read in
 * one sitting, which a dependency's is not.
 *
 * WHAT IT RENDERS: ATX headings, paragraphs, emphasis and strong, inline code,
 * fenced code, links, images, bullet and ordered lists (nested), task list
 * items, block quotes, tables, thematic breaks, hard line breaks.
 *
 * WHAT IT DELIBERATELY DOES NOT: raw HTML (rendered as its own text, never
 * parsed), reference links, footnotes, bare autolink literals, setext
 * headings. Each is a shape these documents do not use, and every one of them
 * is a place a renderer grows a parser nobody exercises.
 *
 * A LINK'S HREF IS RESTRICTED to `http:`, `https:`, `mailto:` and the app's
 * own `#/` routes. Anything else renders as TEXT rather than as a link: a page
 * is written by an agent acting on content it read somewhere else, so a href
 * in one is untrusted input, and an allowlist is the only form of this check
 * that is safe by construction rather than by exhaustive denial.
 */

import { createElement, type ReactNode } from "react";

/** One parsed block. The renderer walks these; nothing else does. */
type Block =
  | { kind: "heading"; level: number; text: string }
  | { kind: "paragraph"; text: string }
  | { kind: "code"; lang: string; text: string }
  | { kind: "quote"; blocks: Block[] }
  | { kind: "list"; ordered: boolean; start: number; items: ListItem[] }
  | { kind: "table"; head: string[]; align: Align[]; rows: string[][] }
  | { kind: "rule" };

type Align = "left" | "center" | "right";

interface ListItem {
  /** null when the item is not a task item — which is not "unticked". */
  checked: boolean | null;
  blocks: Block[];
}

/**
 * The schemes a link may carry.
 *
 * AN ALLOWLIST, never a denylist. `javascript:` is the one everybody
 * remembers; `data:`, `vbscript:`, a whitespace character inside the scheme
 * and a protocol-relative `//host` are the ones they do not, and a renderer
 * that enumerated the bad ones would be wrong the moment a browser grew one
 * more.
 */
const SAFE_SCHEME = /^(https?:|mailto:)/i;

/**
 * Every character a browser drops before it resolves a URL's scheme.
 *
 * They are removed so that the string CHECKED is the string emitted — see
 * [safeHref], where returning the normalised value rather than the raw one is
 * the whole of the guarantee. Stripping them is not itself the defence: an
 * allowlist cannot be slipped past by ADDING noise, only by a denylist.
 */
const URL_NOISE = /[\u0000-\u0020]/g;

/**
 * Whether a href may be rendered as a link at all, and in what form.
 *
 * In-app routes (`#/work/ENG-1`) are allowed because they are how a page
 * points at this company's own objects and they cannot leave the app.
 * Everything else must carry an allowed scheme — including, deliberately, a
 * bare `/path` or `guide.html`, which would resolve against whatever origin
 * serves the dashboard and mean nothing to a reader.
 */
export function safeHref(raw: string): string | null {
  const href = raw.trim();
  if (href === "") return null;
  // NORMALISE, THEN CHECK, THEN EMIT THE NORMALISED ONE. A browser drops the
  // characters in URL_NOISE before resolving a scheme, so a href and its
  // stripped form can reach different destinations — and a function that
  // checked one and returned the other would be checking a string nobody
  // navigates to. The safety comes from the allowlist below; this is what
  // makes the allowlist's subject and the rendered href the same value.
  const clean = href.replace(URL_NOISE, "");
  if (clean.startsWith("#/")) return clean;
  if (SAFE_SCHEME.test(clean)) return clean;
  return null;
}

// --- block parsing ---------------------------------------------------------

const HEADING = /^(#{1,6})\s+(.*)$/;
const FENCE = /^(```|~~~)\s*([A-Za-z0-9_+-]*)\s*$/;
const RULE = /^(?:\s*(?:-{3,}|\*{3,}|_{3,})\s*)$/;
const BULLET = /^(\s*)([-*+])\s+(.*)$/;
const ORDERED = /^(\s*)(\d{1,9})[.)]\s+(.*)$/;
const QUOTE = /^\s{0,3}>\s?(.*)$/;
const TABLE_RULE = /^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)*\|?\s*$/;

/** Split a document into blocks. Pure, and the only thing that reads lines. */
export function parseBlocks(source: string): Block[] {
  const lines = (source ?? "").replace(/\r\n?/g, "\n").split("\n");
  const out: Block[] = [];
  let i = 0;

  const paragraph: string[] = [];
  const flush = () => {
    if (paragraph.length) {
      out.push({ kind: "paragraph", text: paragraph.join("\n") });
      paragraph.length = 0;
    }
  };

  while (i < lines.length) {
    const line = lines[i] ?? "";

    if (line.trim() === "") {
      flush();
      i++;
      continue;
    }

    const fence = FENCE.exec(line);
    if (fence) {
      flush();
      const marker = fence[1] ?? "```";
      const body: string[] = [];
      i++;
      while (i < lines.length && !(lines[i] ?? "").startsWith(marker)) {
        body.push(lines[i] ?? "");
        i++;
      }
      // A fence that is never closed still renders what it opened: the
      // alternative is dropping the rest of a document over a missing line.
      // Its last line is the document's own terminating newline rather than
      // a blank line somebody typed, so it is not content — a closed fence
      // never carries its closing line either.
      if (i >= lines.length && body.length > 0 && body[body.length - 1] === "") {
        body.pop();
      }
      i++;
      out.push({ kind: "code", lang: fence[2] ?? "", text: body.join("\n") });
      continue;
    }

    if (RULE.test(line)) {
      flush();
      out.push({ kind: "rule" });
      i++;
      continue;
    }

    const heading = HEADING.exec(line);
    if (heading) {
      flush();
      out.push({
        kind: "heading",
        level: (heading[1] ?? "#").length,
        // Closing hashes are decoration, not content.
        text: (heading[2] ?? "").replace(/\s+#+\s*$/, "").trim(),
      });
      i++;
      continue;
    }

    if (QUOTE.test(line)) {
      flush();
      const inner: string[] = [];
      while (i < lines.length && QUOTE.test(lines[i] ?? "")) {
        inner.push(QUOTE.exec(lines[i] ?? "")?.[1] ?? "");
        i++;
      }
      out.push({ kind: "quote", blocks: parseBlocks(inner.join("\n")) });
      continue;
    }

    // A TABLE IS A HEADER PLUS ITS RULE, checked together: a line of pipes
    // with no delimiter under it is a paragraph that happens to contain
    // pipes, which is what a shell command or a file path looks like.
    if (line.includes("|") && TABLE_RULE.test(lines[i + 1] ?? "")) {
      flush();
      const head = splitRow(line);
      const align = splitRow(lines[i + 1] ?? "").map(alignOf);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && (lines[i] ?? "").includes("|") && (lines[i] ?? "").trim() !== "") {
        rows.push(splitRow(lines[i] ?? ""));
        i++;
      }
      out.push({ kind: "table", head, align, rows });
      continue;
    }

    if (BULLET.test(line) || ORDERED.test(line)) {
      flush();
      const [list, next] = parseList(lines, i);
      out.push(list);
      i = next;
      continue;
    }

    paragraph.push(line);
    i++;
  }
  flush();
  return out;
}

function alignOf(cell: string): Align {
  const t = cell.trim();
  if (t.startsWith(":") && t.endsWith(":")) return "center";
  if (t.endsWith(":")) return "right";
  return "left";
}

/** Split one table row, dropping the optional leading and trailing pipes. */
function splitRow(line: string): string[] {
  let t = line.trim();
  if (t.startsWith("|")) t = t.slice(1);
  if (t.endsWith("|")) t = t.slice(0, -1);
  return t.split("|").map((c) => c.trim());
}

const TASK = /^\[([ xX])\]\s+(.*)$/;

/**
 * Parse one list and everything nested under it.
 *
 * NESTING IS BY INDENT, measured against the FIRST item's marker rather than
 * against a fixed two spaces — which is what makes a four-space-indented
 * document render flat.
 */
function parseList(lines: string[], start: number): [Block, number] {
  const head = lines[start] ?? "";
  const first = BULLET.exec(head) ?? ORDERED.exec(head);
  const baseIndent = (first?.[1] ?? "").length;
  const ordered = ORDERED.test(head) && !BULLET.test(head);
  const startNumber = ordered ? Number(ORDERED.exec(head)?.[2] ?? 1) : 1;

  const items: ListItem[] = [];
  let i = start;
  let own: string[] = [];
  let checked: boolean | null = null;
  let open = false;

  const close = () => {
    if (!open) return;
    items.push({ checked, blocks: parseBlocks(own.join("\n")) });
    own = [];
    checked = null;
    open = false;
  };

  while (i < lines.length) {
    const line = lines[i] ?? "";
    const bullet = BULLET.exec(line);
    const numbered = ORDERED.exec(line);
    const marker = bullet ?? numbered;
    const indent = (marker?.[1] ?? "").length;

    if (marker && indent === baseIndent) {
      // A marker of the OTHER kind at this level ends the list: a bulleted
      // list and a numbered one are two lists, not one with mixed markers.
      if (ordered !== (numbered !== null && bullet === null)) break;
      close();
      open = true;
      const body = (bullet ? bullet[3] : numbered?.[3]) ?? "";
      const task = TASK.exec(body);
      if (task) {
        checked = (task[1] ?? " ").toLowerCase() === "x";
        own.push(task[2] ?? "");
      } else {
        own.push(body);
      }
      i++;
      continue;
    }
    if (marker && indent > baseIndent) {
      // Nested: the whole indented run goes to this item's own parse, with
      // the base indent removed so the inner list measures from zero.
      own.push(line.slice(baseIndent));
      i++;
      continue;
    }
    if (line.trim() === "") {
      // A blank line inside a list ends it only if nothing continues it.
      const following = lines[i + 1] ?? "";
      const continues =
        BULLET.test(following) || ORDERED.test(following) || /^\s{2,}\S/.test(following);
      if (!continues) break;
      own.push("");
      i++;
      continue;
    }
    if (marker === null && /^\s{2,}\S/.test(line) && open) {
      own.push(line.slice(baseIndent));
      i++;
      continue;
    }
    break;
  }
  close();
  return [{ kind: "list", ordered, start: startNumber, items }, i];
}

// --- inline parsing --------------------------------------------------------

/**
 * The inline grammar, as one alternation.
 *
 * ORDER MATTERS AND CODE COMES FIRST: a backtick span is opaque, so
 * `**not bold**` inside one stays literal. Images precede links because their
 * syntax is a link with a `!` in front of it.
 *
 * A DESTINATION MAY CARRY ONE LEVEL OF BALANCED PARENTHESES, which is what a
 * Wikipedia URL needs — and also what stopped a refused
 * `[label](javascript:alert(1))` from rendering its closing bracket as a stray
 * character beside the label.
 */
const INLINE =
  /(`+)([\s\S]*?)\1|!\[([^\]]*)\]\(((?:[^()\s]|\([^()\s]*\))*)(?:\s+"[^"]*")?\)|\[([^\]]*)\]\(((?:[^()\s]|\([^()\s]*\))*)(?:\s+"[^"]*")?\)|<((?:https?:|mailto:)[^>\s]+)>|(\*\*|__)([\s\S]+?)\8|(\*|_)([^\s][\s\S]*?)\10|(~~)([\s\S]+?)\12/;

/** Render one run of inline markdown to React nodes. */
export function renderInline(text: string, keyPrefix = "i"): ReactNode[] {
  const out: ReactNode[] = [];
  let rest = text;
  let n = 0;

  while (rest.length > 0) {
    const m = INLINE.exec(rest);
    if (!m || m.index === undefined) break;
    if (m.index > 0) out.push(hardBreaks(rest.slice(0, m.index), `${keyPrefix}-t${n++}`));
    const key = `${keyPrefix}-${n++}`;

    if (m[1] !== undefined) {
      // A code span loses ONE leading and trailing space, which is how
      // CommonMark lets a span start with a backtick of its own.
      out.push(
        createElement("code", { key, className: "inline" }, (m[2] ?? "").replace(/^ | $/g, "")),
      );
    } else if (m[3] !== undefined || m[4] !== undefined) {
      const src = safeHref(m[4] ?? "");
      // AN IMAGE WITH A REFUSED SOURCE RENDERS AS ITS ALT TEXT, which is
      // what alt text is for — not as a broken image and not as nothing.
      out.push(
        src
          ? createElement("img", { key, src, alt: m[3] ?? "", loading: "lazy" })
          : createElement("span", { key }, m[3] ?? ""),
      );
    } else if (m[5] !== undefined || m[6] !== undefined) {
      const href = safeHref(m[6] ?? "");
      const label = m[5] ?? "";
      out.push(
        href ? anchor(key, href, renderInline(label, key)) : createElement("span", { key }, label),
      );
    } else if (m[7] !== undefined) {
      const href = safeHref(m[7]);
      out.push(href ? anchor(key, href, [m[7]]) : createElement("span", { key }, m[7]));
    } else if (m[9] !== undefined) {
      out.push(createElement("strong", { key }, renderInline(m[9], key)));
    } else if (m[11] !== undefined) {
      out.push(createElement("em", { key }, renderInline(m[11], key)));
    } else if (m[13] !== undefined) {
      out.push(createElement("del", { key }, renderInline(m[13], key)));
    }
    rest = rest.slice(m.index + m[0].length);
  }
  if (rest.length > 0) out.push(hardBreaks(rest, `${keyPrefix}-t${n}`));
  return out;
}

/**
 * An outbound link opens in a new tab and carries `noopener noreferrer`.
 *
 * `noopener` because a page's links are written by an agent from content it
 * read somewhere else, so the target must not get a handle on this window;
 * `noreferrer` because the dashboard's own URL names the company. An in-app
 * `#/` link is neither — it IS this application, and opening it in a tab
 * would break every internal reference a page makes.
 */
function anchor(key: string, href: string, children: ReactNode[]): ReactNode {
  const internal = href.startsWith("#/");
  return createElement(
    "a",
    internal ? { key, href } : { key, href, target: "_blank", rel: "noopener noreferrer" },
    children,
  );
}

/**
 * Two trailing spaces, or a backslash, is a hard break.
 *
 * Every other newline inside a paragraph is a SOFT break and renders as a
 * space, which is the one rule that makes a hand-wrapped paragraph read as a
 * paragraph rather than as a column of short lines.
 */
function hardBreaks(text: string, key: string): ReactNode {
  const parts = text.split(/(?:  +|\\)\n/);
  if (parts.length === 1) return createElement("span", { key }, soften(text));
  const nodes: ReactNode[] = [];
  parts.forEach((part, i) => {
    if (i > 0) nodes.push(createElement("br", { key: `${key}-br${i}` }));
    nodes.push(soften(part));
  });
  return createElement("span", { key }, ...nodes);
}

function soften(text: string): string {
  return text.replace(/\n/g, " ");
}

// --- block rendering -------------------------------------------------------

function renderBlock(block: Block, key: string): ReactNode {
  switch (block.kind) {
    case "heading":
      // CLAMPED TO h2–h6. A page's own title is the h1 on the screen around
      // it, and a body emitting a second h1 makes two documents claim the
      // same level to a screen reader.
      return createElement(
        `h${Math.min(6, block.level + 1)}`,
        { key },
        renderInline(block.text, key),
      );
    case "paragraph":
      return createElement("p", { key }, renderInline(block.text, key));
    case "code":
      return createElement(
        "pre",
        { key, className: "code plain", "data-lang": block.lang || undefined },
        createElement("code", null, block.text),
      );
    case "quote":
      return createElement(
        "blockquote",
        { key },
        block.blocks.map((b, i) => renderBlock(b, `${key}-${i}`)),
      );
    case "rule":
      return createElement("hr", { key });
    case "list":
      return createElement(
        block.ordered ? "ol" : "ul",
        {
          key,
          // The list's own start, so a document numbering from 3 renders
          // from 3 rather than being silently renumbered.
          ...(block.ordered && block.start !== 1 ? { start: block.start } : {}),
          ...(block.items.some((it) => it.checked !== null) ? { className: "md-tasks" } : {}),
        },
        block.items.map((item, i) =>
          createElement(
            "li",
            { key: `${key}-${i}` },
            item.checked === null
              ? null
              : createElement("input", {
                  key: `${key}-${i}-box`,
                  type: "checkbox",
                  checked: item.checked,
                  // READ-ONLY, ALWAYS. The box renders what the document
                  // says, and this surface performs no writes at all — a box
                  // a reader could tick would report a change that never
                  // reached the page.
                  readOnly: true,
                  disabled: true,
                }),
            // A TASK ITEM'S CONTENT IS ONE COLUMN beside its box. The box
            // and the content are a flex row, and without this wrapper every
            // block of the item became a cell of that row — so a sublist
            // rendered to the RIGHT of the line it belongs under rather than
            // beneath it.
            item.checked === null
              ? item.blocks.map((b, j) => renderBlock(b, `${key}-${i}-${j}`))
              : createElement(
                  "div",
                  { key: `${key}-${i}-body`, className: "md-task-body" },
                  item.blocks.map((b, j) => renderBlock(b, `${key}-${i}-${j}`)),
                ),
          ),
        ),
      );
    case "table":
      return createElement(
        "div",
        { key, className: "md-table" },
        createElement(
          "table",
          null,
          createElement(
            "thead",
            null,
            createElement(
              "tr",
              null,
              block.head.map((cell, i) =>
                createElement(
                  "th",
                  { key: i, style: { textAlign: block.align[i] ?? "left" } },
                  renderInline(cell, `${key}-h${i}`),
                ),
              ),
            ),
          ),
          createElement(
            "tbody",
            null,
            block.rows.map((row, r) =>
              createElement(
                "tr",
                { key: r },
                row.map((cell, c) =>
                  createElement(
                    "td",
                    { key: c, style: { textAlign: block.align[c] ?? "left" } },
                    renderInline(cell, `${key}-${r}-${c}`),
                  ),
                ),
              ),
            ),
          ),
        ),
      );
  }
}

/**
 * Render a markdown document.
 *
 * Returns React nodes, never a string and never HTML: there is no
 * `dangerouslySetInnerHTML` on this path, so no input can introduce an
 * element this file did not construct.
 */
export function renderMarkdown(source: string): ReactNode[] {
  return parseBlocks(source ?? "").map((block, i) => renderBlock(block, `b${i}`));
}

// --- flattening ------------------------------------------------------------

/**
 * One run of inline markdown as the text it renders to.
 *
 * THE SAME [INLINE] ALTERNATION AS [renderInline], deliberately. What a code
 * span is, where a destination ends and which emphasis run closes which are
 * decisions this file has already made once; a second set of them for the text
 * case is the drift `textcut` and `whsec` each record in their own package docs.
 * What differs between the two walks is only what a match BECOMES — a node
 * there, a string here.
 */
function inlineText(text: string): string {
  const out: string[] = [];
  let rest = text;

  while (rest.length > 0) {
    const m = INLINE.exec(rest);
    if (!m || m.index === undefined) break;
    if (m.index > 0) out.push(rest.slice(0, m.index));

    if (m[1] !== undefined) {
      // A code span's own content, minus the one padding space CommonMark lets
      // a span carry so it can start with a backtick.
      out.push((m[2] ?? "").replace(/^ | $/g, ""));
    } else if (m[3] !== undefined || m[4] !== undefined) {
      // AN IMAGE IS ITS ALT TEXT, which is what alt text is for. One with no alt
      // contributes nothing rather than a filename nobody wrote.
      out.push(m[3] ?? "");
    } else if (m[5] !== undefined || m[6] !== undefined) {
      // A LINK IS ITS LABEL, never its destination: the label is the sentence
      // somebody wrote, and a URL in a one-line cell spends the whole line.
      out.push(inlineText(m[5] ?? ""));
    } else if (m[7] !== undefined) {
      // An autolink has no label, so the URL IS the text it renders to.
      out.push(m[7]);
    } else if (m[9] !== undefined) {
      out.push(inlineText(m[9]));
    } else if (m[11] !== undefined) {
      out.push(inlineText(m[11]));
    } else if (m[13] !== undefined) {
      out.push(inlineText(m[13]));
    }
    rest = rest.slice(m.index + m[0].length);
  }
  if (rest.length > 0) out.push(rest);
  return out.join("");
}

/** One block as text. Every case is the prose the block renders to. */
function blockText(block: Block): string {
  switch (block.kind) {
    case "heading":
    case "paragraph":
      return inlineText(block.text);
    case "code":
      // THE CODE, WITHOUT ITS FENCE. What was inside a fence is still what the
      // comment said; the backticks are the only part that was never content.
      return block.text;
    case "quote":
      return block.blocks.map(blockText).join(" ");
    case "list":
      return block.items
        .map((item) => {
          const said = item.blocks.map(blockText).join(" ");
          // A TASK ITEM KEEPS ITS BOX. `[ ]` and `[x]` are markdown syntax, and
          // they are the one piece of it whose meaning survives being flattened
          // — dropping them turns "not done yet" into a sentence that reads as a
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
 * — every tool that writes one says so in its own schema — and the engine's
 * `excerpt` is a cut of one of them. Every surface that draws an excerpt in a
 * single cell was therefore drawing `## Understanding the work` with the hashes
 * in it: the same `pre-wrap` bug this file was written to end, surviving in the
 * places too narrow to render blocks into.
 *
 * WHY NOT [renderMarkdown] THERE: those cells are one line inside a grid track,
 * and `.truncate` is `white-space: nowrap` — it cannot clip a block box at all,
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
  return parseBlocks(source ?? "")
    .map(blockText)
    .join(" ")
    .replace(/\s+/g, " ")
    .trim();
}
