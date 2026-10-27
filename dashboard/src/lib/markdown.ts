/**
 * A CommonMark subset, rendered to React nodes.
 *
 * Pages and work-item descriptions are MARKDOWN BY CONTRACT — the tools that
 * write them say so, the agents write it, and the engine stores it verbatim —
 * and the dashboard rendered every one of them as `white-space: pre-wrap`. A
 * reader saw `## Decision` and `- [ ] ship it` as literal characters, and a
 * link as its own URL in angle brackets.
 *
 * THE GRAMMAR IS THE DESIGN SYSTEM'S (`parseMarkdown` and `markdownInline`
 * from @crewlethq/ui), and this file is the dashboard's RENDERER of it. The
 * rich editor a writer edits a page in reads the body by that same grammar,
 * so what an editor shows and what a reader sees cannot disagree about where
 * a list ends or what a backslash escapes. What the grammar reads and what it
 * deliberately does not (raw HTML is text, never parsed) is written there.
 *
 * NO MARKUP IS EVER PARSED OR EMITTED. The grammar answers plain objects and
 * this file builds React elements from them, so there is nothing to sanitise
 * and no `dangerouslySetInnerHTML` anywhere on this path, which is what lets
 * the dashboard enforce Trusted Types with no policy at all.
 *
 * A LINK'S HREF IS RESTRICTED by `safeHref` to `http:`, `https:`, `mailto:`
 * and the app's own `#/` routes. Anything else renders as TEXT rather than as
 * a link: a page is written by an agent acting on content it read somewhere
 * else, so a href in one is untrusted input.
 *
 * IT ALSO SPLITS A DOCUMENT INTO ITS SECTIONS ([splitSections],
 * [headingLines]) without rendering anything, for a surface that needs the
 * document's OUTLINE and its source both — a phase prompt, tens of kilobytes,
 * which its screen outlines (by its builder's section map where it has one,
 * see `promptmap.ts`) and then renders one section at a time. A walk that
 * rendered as it split could hand back neither. Both share this file's
 * the grammar's heading and fence readers through one walk rather than restating them; see its
 * own note.
 *
 * AND IT READS TWO KINDS OF AUTHOR ([RenderOptions]): a document's — soft line
 * wraps, headings that are sections — and a model's, whose every newline is a
 * break and whose headings are lines of one transcript item.
 */

import { createElement, Fragment, type ReactNode } from "react";
import {
  Highlight,
  InlineCode,
  MARKDOWN_CALLOUT_NAMES,
  markdownClosesFence,
  markdownFenceOf,
  markdownHeadingOf,
  markdownInline,
  parseMarkdown,
  safeHref,
  type MarkdownBlock,
  type MarkdownFence,
} from "@crewlethq/ui";

// --- sectioning ------------------------------------------------------------

/** One ATX heading and the source under it. */
export interface Section {
  /** 1–6 for a heading; 0 for the run of text before the first one. */
  level: number;
  /** The heading's own text, inline markup and all. "" at level 0. */
  title: string;
  /**
   * The source under the heading, VERBATIM and with its own line endings —
   * the heading line itself removed, and the blank lines that bound the run
   * with it. It is a SLICE of the document, so it is always found in it.
   */
  body: string;
}

/**
 * Split a document into its ATX-heading sections.
 *
 * WHY IT IS NOT [parseMarkdown]. That walk answers "what does this document
 * RENDER to", and a caller that wants sections wants something it cannot get
 * from the answer: the source back, unchanged, grouped under the headings
 * somebody wrote. A phase prompt is the case — the headings are the only
 * structure a ~30 kB document has, and its screen needs each block's SOURCE so
 * it can render that block on its own (or hand the whole record back byte for
 * byte). Rendering first and sectioning after would have to reassemble
 * markdown out of React nodes to do either.
 *
 * WHY IT IS HERE AND NOT BESIDE THAT CALLER: what a heading IS and where a
 * fenced block suspends the grammar are decisions [parseMarkdown] has already
 * made, and the grammar's heading and fence readers are the two constants this shares with it.
 * Written again next to the screen that needed sections, the two would drift
 * exactly as `textcut`'s four copies and `whsec`'s two did — and the drift
 * that costs here is the fence, which nobody remembers until a `# ` inside a
 * code sample splits a document somewhere its author never put a heading.
 *
 * THE LEAD RUN IS A SECTION, at level 0 with no title: a document whose first
 * line is not a heading has content there, and dropping it would make this a
 * lossy view of a record. It is omitted only when it is blank.
 *
 * WHAT IS LOST, deliberately and only this: the blank lines bounding each run.
 * They are the separators between sections rather than content of one, and
 * keeping them would open every section on an empty line. Every other line of
 * the source appears in exactly one section, in order, byte for byte — LINE
 * ENDINGS INCLUDED, which is why this reads lines through [readLines] rather
 * than normalising them the way [parseMarkdown] does.
 */
export function splitSections(source: string): Section[] {
  const out: Section[] = [];
  let level = 0;
  let title = "";
  let body: Line[] = [];

  const close = () => {
    const text = joinLines(trimBlankLines(body));
    body = [];
    // A heading with nothing under it is still a section — that is a fact
    // about the document. An EMPTY lead run is not: nothing was written
    // there, so there is nothing to name.
    if (level === 0 && text === "") return;
    out.push({ level, title, body: text });
  };

  for (const { line, heading } of walkLines(source ?? "")) {
    if (!heading) {
      body.push(line);
      continue;
    }
    close();
    level = heading.level;
    title = heading.title;
  }
  close();
  return out;
}

/** One ATX heading line, and where its line starts in the document. */
export interface HeadingLine {
  /** The index of the line's first character in the source (UTF-16). */
  at: number;
  /** 1–6. */
  level: number;
  /** The heading's own text, inline markup and all. */
  title: string;
}

/**
 * Every heading line of a document, in order, with the offset its line starts
 * at — what [splitSections] splits on, for a caller that has to cut the SOURCE
 * at those lines rather than receive it regrouped. A phase prompt's reader is
 * the one: its outline sizes every section in bytes that must add up to the
 * document, so a section is a slice running from its heading line to the next,
 * blank lines and all — which [splitSections], trimming them, cannot hand back.
 *
 * THE SAME WALK as [splitSections] ([walkLines]), so the two can never
 * disagree about which lines are headings — the fence above all.
 */
export function headingLines(source: string): HeadingLine[] {
  const out: HeadingLine[] = [];
  for (const { at, heading } of walkLines(source ?? "")) {
    if (heading) out.push({ at, ...heading });
  }
  return out;
}

/**
 * A document's lines, each with its offset and — outside a fenced block — the
 * heading it is, if it is one.
 *
 * A FENCE SUSPENDS THE GRAMMAR, which is the whole reason this is not a
 * `split(/^#/m)`: `# install deps` inside a shell sample is a comment, and a
 * tool catalogue is full of them. ONE walk for both readers of a document's
 * headings, so the fence rule is written once.
 */
function* walkLines(
  source: string,
): Generator<{ line: Line; at: number; heading: { level: number; title: string } | null }> {
  // The fence marker currently open, or null outside one.
  let fence: MarkdownFence | null = null;
  let at = 0;
  for (const line of readLines(source)) {
    const start = at;
    at += line.text.length + line.eol.length;
    if (fence !== null) {
      if (markdownClosesFence(line.text, fence)) fence = null;
      yield { line, at: start, heading: null };
      continue;
    }
    const opened = markdownFenceOf(line.text);
    if (opened) {
      fence = opened.marker;
      yield { line, at: start, heading: null };
      continue;
    }
    // Closing hashes are decoration, not content: the same reader
    // [parseMarkdown] applies to the same line.
    yield { line, at: start, heading: markdownHeadingOf(line.text) };
  }
}

/** One section, and the sections nested under it. */
export interface SectionNode extends Section {
  children: SectionNode[];
}

/**
 * Nest a flat section list on its heading levels.
 *
 * Each heading goes under the nearest preceding heading of a LOWER level, so
 * an outline is the document's own shape rather than a list of every heading
 * in it. That distinction is the whole reason this exists: a conversation
 * ledger's `### 2026-08-20` entries belong inside `## Earlier in this
 * conversation`, and flattened they read as blocks of the message in their own
 * right, sitting between it and the ask.
 *
 * WHICH IS ONLY CORRECT BECAUSE THE LEVELS ARE. Nesting says a section is PART
 * of the one above it, so a document with a stray `#` over a run of `##` nests
 * everything inside it — see `internal/agent/prompts`, whose own suite now
 * holds every builder to one level for exactly this reason.
 *
 * THE LEAD RUN IS NEVER A PARENT. It has no heading, so it contains nothing;
 * at level 0 it would otherwise swallow the document that follows it.
 */
export function nestSections(sections: Section[]): SectionNode[] {
  const roots: SectionNode[] = [];
  const open: SectionNode[] = [];
  for (const section of sections) {
    const node: SectionNode = { ...section, children: [] };
    if (node.level === 0) {
      roots.push(node);
      continue;
    }
    while (open.length > 0 && (open[open.length - 1]?.level ?? 0) >= node.level) open.pop();
    const parent = open[open.length - 1];
    if (parent) parent.children.push(node);
    else roots.push(node);
    open.push(node);
  }
  return roots;
}

/** One source line and the break that ended it — "" on the last. */
interface Line {
  text: string;
  eol: string;
}

/**
 * Split a source into lines, KEEPING each one's own line break.
 *
 * [parseMarkdown] normalises `\r\n` away, which is right for a renderer: a break
 * becomes a React node and the bytes behind it are gone either way. A SECTION
 * IS A SLICE OF A RECORD, though, and the same normalisation handed back a
 * body that could not be found in the document it came out of — a prompt
 * carries a webhook's task text and a page's body verbatim, and a GitHub issue
 * body is CRLF, so the whole-document view showed the record and every fold
 * showed an edited copy of it.
 */
function readLines(source: string): Line[] {
  const out: Line[] = [];
  const breaks = /\r\n|\r|\n/g;
  let start = 0;
  for (let m = breaks.exec(source); m !== null; m = breaks.exec(source)) {
    out.push({ text: source.slice(start, m.index), eol: m[0] });
    start = breaks.lastIndex;
  }
  out.push({ text: source.slice(start), eol: "" });
  return out;
}

/**
 * A run of lines as the source it was read from.
 *
 * The LAST line's break is dropped: it separates this run from whatever
 * follows rather than ending the run's own content, which is the same reason
 * a closed fence never carries its closing line.
 */
function joinLines(lines: Line[]): string {
  return lines
    .map((line, i) => (i === lines.length - 1 ? line.text : line.text + line.eol))
    .join("");
}

/** A run of lines with its leading and trailing blank lines dropped. */
function trimBlankLines(lines: Line[]): Line[] {
  let lo = 0;
  let hi = lines.length;
  while (lo < hi && (lines[lo]?.text ?? "").trim() === "") lo++;
  while (hi > lo && (lines[hi - 1]?.text ?? "").trim() === "") hi--;
  return lines.slice(lo, hi);
}

// --- inline parsing --------------------------------------------------------

/**
 * The longest code span kept on one line. Thirty-two monospace characters at
 * the body's size are about 250px — inside the narrowest column a document is
 * drawn in (a 390px phone less its gutters), so a whole span never overflows.
 */
export const UnbrokenCode = 32;

/**
 * How a renderer reads what a document's author did not mark up.
 *
 * TWO READERS WRITE MARKDOWN WITH DIFFERENT HABITS, and the defaults are the
 * document's. A page, a description or a comment is a DOCUMENT: a newline
 * inside a paragraph is a soft wrap somebody typed to keep a source line
 * short, and a heading opens a section of it. A MODEL'S WORDS — a round's
 * speech, a coding run's report, a prompt the engine built for a
 * model — are neither. A model writes one fact per line and means each line,
 * and a "## Summary" in the middle of a transcript is emphasis inside one item
 * of the page, not a section of it.
 */
export interface RenderOptions {
  /**
   * `soft` (the default): a newline inside a paragraph is a space, and only
   * two trailing spaces or a backslash break the line — CommonMark's rule.
   * `hard`: every newline is a line break, which is what a model's single
   * newlines are; read as soft, a list of facts collapsed into a run-on line.
   */
  breaks?: "soft" | "hard";
  /**
   * `sections` (the default): a heading line is an `h2`–`h6`. `text`: a
   * heading line is a styled block that is NOT a heading element
   * (`.md-heading`), for content that is a transcript item rather than a
   * section of the page — the same reason a tool row's disclosure takes
   * `headingLevel="none"`. A model's "## Summary" put an `h2` into the turn
   * page's outline, between the page's own sections.
   */
  headings?: "sections" | "text";
  /**
   * Give each top-level heading the id [outline] points at, for a page with
   * an "On this page" list. Nothing to address under `headings: "text"`.
   */
  anchors?: boolean;
  /**
   * The terms a search matched, marked where they start a word of the TEXT a
   * reader reads (`Highlight`). Never inside a code span, a link's address or
   * an image's source: those are not prose, and a mark there points at a
   * match the search never made.
   */
  highlight?: readonly string[];
  /**
   * What each run of the PROSE a reader reads becomes, in reading order: the
   * words between the code spans, the links (their labels included) and the
   * images, never inside them, and never a code block's. Its `key` is unique
   * within the render. The Dashboard Assistant makes the first mention of each
   * record a turn read its chip here (`app/assistant/chunk/mentions.ts`), so a
   * key quoted in code or written into a link's label stays as written. A
   * caller gives this or `highlight`; given both, this is the one applied.
   */
  prose?: (text: string, key: string) => ReactNode;
  /**
   * Where an image's source is shown from, in place of the source itself: a
   * doc's pictures are files of its version, fetched with the reader's own
   * credential and shown through an object URL, never loaded from an address
   * the text names. It answers null for a source it does not show (not a file
   * of the version, or not loaded yet), which renders as the alt text. Without
   * it, a source is shown only when it is a safe link ([safeHref]).
   */
  image?: (src: string) => string | null;
  /**
   * `anchor` (the default): a link is a link. `text`: a link is its label and
   * an autolink its address, as plain text: for a run drawn INSIDE a control
   * or a link, a listbox option, a picker's entry or a gallery card's cover,
   * whose children are presentational to assistive tech, so a link there is
   * unreachable as itself, and whose click a link would steal to navigate
   * instead of choosing the row. Inside a link it is also invalid HTML: an
   * `<a>` may not hold another.
   *
   * `inert`: a MODEL'S link is never live. The Dashboard Assistant's prose is
   * written by a model that may have read anything, so a link in it is drawn
   * as its label followed by the host it names (`docs.example.com/pricing`),
   * in the quiet link ink, not underlined and not focusable; an autolink is
   * its host and path; and an image is its alt text, never fetched and never
   * a link. Navigation from the assistant is only ever an engine-built Open in
   * app card, which the drawer checks itself.
   */
  links?: "anchor" | "text" | "inert";
}

/** What the inline walk needs of [RenderOptions]. */
export type InlineOptions = Pick<
  RenderOptions,
  "breaks" | "highlight" | "image" | "links" | "prose"
>;

/** Render one run of inline markdown to React nodes. */
export function renderInline(
  text: string,
  keyPrefix = "i",
  options: InlineOptions = {},
): ReactNode[] {
  const out: ReactNode[] = [];
  let at = 0;
  let n = 0;
  const hard = options.breaks === "hard";

  for (const { index, end, groups: g } of markdownInline(text)) {
    if (index > at) {
      out.push(breakLines(text.slice(at, index), `${keyPrefix}-t${n++}`, hard, options));
    }
    const key = `${keyPrefix}-${n++}`;
    at = end;

    if (g.esc !== undefined) {
      // AN ESCAPED CHARACTER IS ITSELF: `\*` is an asterisk somebody meant.
      out.push(createElement("span", { key }, g.esc));
    } else if (g.tick !== undefined) {
      // A code span loses ONE leading and trailing space, which is how
      // CommonMark lets a span start with a backtick of its own.
      const code = (g.code ?? "").replace(/^ | $/g, "");
      // A SHORT SPAN IS ONE TOKEN to a reader: `nimbus jobs wait --node <n>`
      // split at a space reads as two commands, and the break always lands on
      // the flag. Up to [UnbrokenCode] characters it is kept on one line; a
      // longer one (a URL, a whole config line) still wraps, since holding it
      // whole would overflow a phone's column.
      out.push(createElement(InlineCode, { key, whole: code.length <= UnbrokenCode }, code));
    } else if (g.src !== undefined && options.links === "inert") {
      // A MODEL'S PICTURE IS ITS ALT TEXT: nothing it names is fetched.
      out.push(createElement("span", { key }, g.alt ?? ""));
    } else if (g.src !== undefined) {
      const alt = g.alt ?? "";
      const src = options.image ? options.image(g.src) : shownImage(g.src);
      // A PICTURE FROM ANOTHER ORIGIN IS A LINK TO IT, labelled by its alt
      // text: the dashboard's policy (`img-src 'self' data: blob:`) refuses to
      // load it, so an `img` there was a broken picture, and loading it would
      // tell a third party who reads the page and when. The reader can still
      // follow it on purpose. Inside a link's own label it stays its alt text,
      // since a link inside a link is no link at all.
      const elsewhere = !src && !options.image && options.links !== "text" ? safeHref(g.src) : null;
      // AN IMAGE WITH A REFUSED SOURCE RENDERS AS ITS ALT TEXT, which is
      // what alt text is for — not as a broken image and not as nothing.
      out.push(
        src
          ? createElement("img", { key, src, alt, loading: "lazy" })
          : elsewhere
            ? anchor(key, elsewhere, [alt || elsewhere])
            : createElement("span", { key }, alt),
      );
    } else if (g.href !== undefined && options.links === "inert") {
      out.push(inertLink(key, renderInline(g.label ?? "", key, unprosed(options)), g.href));
    } else if (g.auto !== undefined && options.links === "inert") {
      out.push(inertLink(key, [], g.auto));
    } else if (g.href !== undefined) {
      const href = options.links === "text" ? null : safeHref(g.href);
      const label = g.label ?? "";
      // A LINK'S LABEL IS TEXT TO ANY LINK INSIDE IT: an anchor inside an
      // anchor is invalid, and a browser splits the outer one around it.
      const inner = href ? { ...unprosed(options), links: "text" as const } : unprosed(options);
      out.push(
        href
          ? anchor(key, href, renderInline(label, key, inner))
          : createElement("span", { key }, renderInline(label, key, inner)),
      );
    } else if (g.auto !== undefined) {
      const href = options.links === "text" ? null : safeHref(g.auto);
      out.push(href ? anchor(key, href, [g.auto]) : createElement("span", { key }, g.auto));
    } else if (g.strong !== undefined || g.strongUnder !== undefined) {
      const inner = g.strong ?? g.strongUnder ?? "";
      out.push(createElement("strong", { key }, renderInline(inner, key, options)));
    } else if (g.em !== undefined || g.emUnder !== undefined) {
      const inner = g.em ?? g.emUnder ?? "";
      out.push(createElement("em", { key }, renderInline(inner, key, options)));
    } else if (g.del !== undefined) {
      out.push(createElement("del", { key }, renderInline(g.del, key, options)));
    }
  }
  if (at < text.length) {
    out.push(breakLines(text.slice(at), `${keyPrefix}-t${n}`, hard, options));
  }
  return out;
}

/**
 * A picture source this origin shows by itself: a safe address on the
 * dashboard's own origin, which is all its policy (`img-src 'self' data:
 * blob:`) loads. Null for anything else, which the renderer draws as a link
 * and the rich editor as the picture's alt text, rather than as a picture the
 * browser refuses. An app route (`#/...`) is an address, never a picture.
 */
export function shownImage(src: string): string | null {
  const href = safeHref(src);
  if (!href || href.startsWith("#")) return null;
  try {
    return new URL(href, window.location.href).origin === window.location.origin ? href : null;
  } catch {
    return null;
  }
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
 * Where a link points, as a reader checks it: the host and the path of a web
 * address with no scheme (`docs.example.com/pricing`), the address of a
 * `mailto:`, and nothing for anything else (an app route, a scheme this
 * product does not follow), which is drawn as its label alone.
 */
export function linkTarget(raw: string): string | null {
  const href = safeHref(raw);
  if (!href || href.startsWith("#")) return null;
  if (href.startsWith("mailto:")) return href.slice("mailto:".length);
  try {
    const url = new URL(href);
    const path = url.pathname === "/" ? "" : url.pathname;
    return `${url.host}${path}`;
  } catch {
    return null;
  }
}

/** A model's link, drawn inert: its label, then the place it names. See [RenderOptions.links]. */
function inertLink(key: string, label: ReactNode[], raw: string): ReactNode {
  const target = linkTarget(raw);
  const named = label.length > 0;
  return createElement(
    "span",
    { key },
    ...(named ? label : []),
    target === null
      ? named
        ? null
        : raw
      : createElement(
          "span",
          { key: `${key}-host`, className: "md-link-inert-host" },
          named ? ` (${target})` : target,
        ),
  );
}

/**
 * Two trailing spaces, or a backslash, is a hard break.
 *
 * Every other newline inside a paragraph is a SOFT break and renders as a
 * space, which is the one rule that makes a hand-wrapped paragraph read as a
 * paragraph rather than as a column of short lines — unless the reader asked
 * for `breaks: "hard"`, where every newline is a break and the two markers
 * that ask for one are consumed with it rather than left as stray characters.
 */
/** A link's label is not prose: `prose` never runs inside one. */
function unprosed(options: InlineOptions): InlineOptions {
  if (options.prose === undefined) return options;
  const rest = { ...options };
  delete rest.prose;
  return rest;
}

function breakLines(text: string, key: string, hard: boolean, options: InlineOptions): ReactNode {
  const { highlight, prose } = options;
  // A RUN OF PROSE, as the caller makes it, or marked where a search matched
  // it when the caller asked.
  const run = (part: string, at: number): ReactNode =>
    prose
      ? createElement(Fragment, { key: `${key}-p${at}` }, prose(soften(part), `${key}-p${at}`))
      : highlight && highlight.length > 0
        ? createElement(Highlight, { key: `${key}-h${at}`, text: soften(part), terms: highlight })
        : soften(part);
  const parts = text.split(hard ? /(?:  +|\\)?\n/ : /(?:  +|\\)\n/);
  if (parts.length === 1) return createElement("span", { key }, run(text, 0));
  const nodes: ReactNode[] = [];
  parts.forEach((part, i) => {
    if (i > 0) nodes.push(createElement("br", { key: `${key}-br${i}` }));
    nodes.push(run(part, i));
  });
  return createElement("span", { key }, ...nodes);
}

function soften(text: string): string {
  return text.replace(/\n/g, " ");
}

// --- block rendering -------------------------------------------------------

function renderBlock(
  block: MarkdownBlock,
  key: string,
  options: RenderOptions,
  anchor?: string,
): ReactNode {
  switch (block.kind) {
    case "heading":
      // NOT A SECTION when the reader said so: a transcript item's heading is
      // emphasis inside the item, and an `h2` there joins the PAGE's outline.
      // A `p` rather than a `div`, because it is a line of text and a reader
      // that lists paragraphs should find it.
      if (options.headings === "text") {
        return createElement(
          "p",
          { key, className: "md-heading" },
          renderInline(block.text, key, options),
        );
      }
      // CLAMPED TO h2–h6. A page's own title is the h1 on the screen around
      // it, and a body emitting a second h1 makes two documents claim the
      // same level to a screen reader.
      return createElement(
        `h${Math.min(6, block.level + 1)}`,
        anchor ? { key, id: anchor } : { key },
        renderInline(block.text, key, options),
      );
    case "paragraph":
      return createElement("p", { key }, renderInline(block.text, key, options));
    case "code":
      // `md-code`, in the same family as `md-table`, `md-tasks` and
      // `md-task-body`. It was `code plain`, two generic names (a table
      // header once spent `plain`), and NEITHER of them was ever a recipe for this block. So a
      // fenced sample in a page body, a work item's description, a comment or
      // a phase prompt had no surface, no padding and, because a `pre`
      // defaults to `white-space: pre` with `overflow: visible`, no way to
      // contain a long line: one line of JSON pushed the whole page 678px
      // wider than its own viewport (measured at 700px). The forward half of
      // the class gate could not see it either, because it read a `className`
      // ATTRIBUTE and this is a property in a props object — which is the
      // other half of why it stayed silent, and is fixed in that gate.
      return createElement(
        "pre",
        { key, className: "md-code", "data-lang": block.lang || undefined },
        createElement("code", null, block.text),
      );
    case "quote":
      return createElement(
        "blockquote",
        { key },
        block.blocks.map((b, i) => renderBlock(b, `${key}-${i}`, options)),
      );
    case "callout":
      // A CALLOUT IS A NOTE with its tone named on a line of its own, in the
      // shape `Document` sets (`data-callout`, `data-callout-label`,
      // `data-callout-body`). The tone's name is the grammar's, the word the
      // editor draws too, so a writer and a reader see the same one.
      return createElement(
        "div",
        { key, role: "note", "data-callout": block.tone },
        createElement("p", { "data-callout-label": "" }, MARKDOWN_CALLOUT_NAMES[block.tone]),
        createElement(
          "div",
          { "data-callout-body": "" },
          block.blocks.map((b, i) => renderBlock(b, `${key}-${i}`, options)),
        ),
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
              ? item.blocks.map((b, j) => renderBlock(b, `${key}-${i}-${j}`, options))
              : createElement(
                  "div",
                  { key: `${key}-${i}-body`, className: "md-task-body" },
                  item.blocks.map((b, j) => renderBlock(b, `${key}-${i}-${j}`, options)),
                ),
          ),
        ),
      );
    case "table":
      // THE WRAPPER SCROLLS a table wider than its column, so it is a tab stop
      // with a name: a scroller no keyboard can reach is a table no keyboard
      // can read to its end, and "region" alone says nothing about which.
      return createElement(
        "div",
        {
          key,
          className: "md-table",
          tabIndex: 0,
          role: "region",
          "aria-label": `Table: ${block.head.map((cell) => plainText(cell)).join(", ")}`,
        },
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
                  renderInline(cell, `${key}-h${i}`, options),
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
                    renderInline(cell, `${key}-${r}-${c}`, options),
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
 * element this file did not construct — whichever [RenderOptions] are set,
 * since they choose between elements this file builds and nothing else.
 */
export function renderMarkdown(source: string, options: RenderOptions = {}): ReactNode[] {
  const blocks = parseMarkdown(source ?? "");
  if (!options.anchors || options.headings === "text") {
    return blocks.map((block, i) => renderBlock(block, `b${i}`, options));
  }
  // THE SAME WALK [outline] TAKES, so a heading's id and the entry pointing at
  // it are one computation and cannot disagree.
  const ids = anchorIds(blocks);
  return blocks.map((block, i) => renderBlock(block, `b${i}`, options, ids.get(i)));
}

/**
 * How a MODEL'S WORDS are rendered: a round's speech, a coding
 * run's report. Every newline is the line break the model meant, and a heading
 * is emphasis inside one transcript item rather than a section of the page —
 * see [RenderOptions]. One constant, so the transcript's every block of a
 * model's prose reads by one rule.
 */
export const MODEL_WORDS: RenderOptions = { breaks: "hard", headings: "text" };

/**
 * How the Dashboard Assistant's prose is read: a model's words, whose every
 * link is inert and whose pictures are their alt text (`links: "inert"`).
 */
export const ASSISTANT_WORDS: RenderOptions = { ...MODEL_WORDS, links: "inert" };

/** One heading of a document, as an "On this page" entry points at it. */
export interface Heading {
  /** The markdown level, 1 for `#`. */
  level: number;
  /** The heading as the prose it renders to. */
  text: string;
  /** The id [renderMarkdown] gives it with `anchors`. */
  id: string;
}

/**
 * The document's TOP-LEVEL headings, in order — the "On this page" outline.
 *
 * TOP-LEVEL ONLY: a heading inside a quote or a list item is part of that
 * block rather than a section of the document, and it gets no anchor.
 */
export function outline(source: string): Heading[] {
  const blocks = parseMarkdown(source ?? "");
  const ids = anchorIds(blocks);
  const out: Heading[] = [];
  blocks.forEach((block, i) => {
    const id = ids.get(i);
    if (block.kind === "heading" && id) {
      out.push({ level: block.level, text: inlineText(block.text).trim(), id });
    }
  });
  return out;
}

/**
 * An id per top-level heading: `h-` and the words of its text, a repeat
 * suffixed `-2`, `-3` in order — so two "Escalation" sections are two
 * addresses, and an anchor never collides with an id the frame itself uses.
 */
function anchorIds(blocks: MarkdownBlock[]): Map<number, string> {
  const ids = new Map<number, string>();
  const seen = new Map<string, number>();
  blocks.forEach((block, i) => {
    if (block.kind !== "heading") return;
    const words = inlineText(block.text)
      .toLowerCase()
      .normalize("NFKD")
      .replace(/[^\p{L}\p{N}]+/gu, "-")
      .replace(/^-+|-+$/g, "");
    const base = `h-${words || "section"}`;
    const n = (seen.get(base) ?? 0) + 1;
    seen.set(base, n);
    ids.set(i, n === 1 ? base : `${base}-${n}`);
  });
  return ids;
}

// --- flattening ------------------------------------------------------------

/**
 * One run of inline markdown as the text it renders to.
 *
 * THE SAME WALK AS [renderInline] ([markdownInline]), deliberately. What a code
 * span is, where a destination ends and which emphasis run closes which are
 * decisions this file has already made once; a second set of them for the text
 * case is the drift `textcut` and `whsec` each record in their own package docs.
 * What differs between the two walks is only what a match BECOMES — a node
 * there, a string here.
 */
function inlineText(text: string): string {
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
 * Every image source the document names, once each, in order: what a host
 * resolves before [renderMarkdown] draws it with an `image` option.
 *
 * THE SAME WALK the renderer takes ([parseMarkdown], then [markdownInline] into
 * every run a block renders), so a source listed here is one the renderer
 * will ask for, and one inside a code span, which renders as text, is not.
 */
export function imageSources(source: string): string[] {
  const out = new Set<string>();
  const inline = (text: string) => {
    for (const { groups: g } of markdownInline(text)) {
      if (g.esc !== undefined || g.tick !== undefined || g.auto !== undefined) continue;
      if (g.src !== undefined) out.add(g.src);
      else inline(g.label ?? g.strong ?? g.strongUnder ?? g.em ?? g.emUnder ?? g.del ?? "");
    }
  };
  const block = (b: MarkdownBlock) => {
    switch (b.kind) {
      case "heading":
      case "paragraph":
        inline(b.text);
        return;
      case "quote":
      case "callout":
        b.blocks.forEach(block);
        return;
      case "list":
        for (const item of b.items) item.blocks.forEach(block);
        return;
      case "table":
        for (const row of [b.head, ...b.rows]) row.forEach(inline);
        return;
      case "code":
      case "rule":
        return;
    }
  };
  parseMarkdown(source ?? "").forEach(block);
  return [...out];
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
  return parseMarkdown(source ?? "")
    .map(blockText)
    .join(" ")
    .replace(/\s+/g, " ")
    .trim();
}
