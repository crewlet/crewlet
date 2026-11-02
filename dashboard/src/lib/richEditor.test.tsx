/**
 * The rich editor's round trip, held to the bodies this engine actually has.
 *
 * A Doclet page and a doc's tab are stored as Markdown, and the bytes are
 * the record: a page's save is merged line by line, a doc's is a patch of the
 * lines that moved. So opening a body in the editor and saving it with no edit
 * must write the body back BYTE FOR BYTE, and a body the editor rewrites must
 * still read, to the dashboard's own renderer, as what the writer saw.
 *
 * THE CORPUS IS EVERY MARKDOWN FILE THE REPOSITORY HOLDS (the product docs,
 * the ADRs, the examples and the bundled skills, which are the pages agents
 * write and read), the renderer's own fixtures, a body holding every block the
 * editor writes, the composer's templates, and the doc fixtures, so a
 * construct the editor cannot carry is found here rather than in somebody's
 * diff.
 */

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { render } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { canonicalMarkdown, roundTripMarkdown } from "@crewlethq/ui/RichEditorMarkdown";
import { headingLines, renderMarkdown } from "./markdown.ts";
import { TEMPLATES } from "~/routes/doclet/Composer.tsx";
import { DOC_TEMPLATES } from "~/lib/artifacts/docTemplates.ts";
import { repoRoot } from "~/test/engineFiles.ts";

const SKIP = new Set(["node_modules", ".git", "static", "dashboard"]);

function markdownUnder(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    if (SKIP.has(name)) continue;
    const path = join(dir, name);
    if (statSync(path).isDirectory()) out.push(...markdownUnder(path));
    else if (name.endsWith(".md")) out.push(path);
  }
  return out;
}

/**
 * The most of a document one rewrite case reads, unless one section is longer.
 *
 * A REWRITE CASE RENDERS ITS TEXT TWICE through React into jsdom, which is CPU
 * work in the text's size: `docs/reference/api-endpoints.md` (about 760 KB)
 * costs about six seconds of it on an idle machine and took 8 to 37 over nine
 * runs beside `go build` and `go vet`, one case outrunning the bound every
 * other case is held to and growing with the document. So a document longer
 * than this is read in parts, cut at its heading lines (`headingLines`, the
 * fence-aware walk the page outline cuts on), each part a case of its own: a
 * heading ends every block before it, so a part renders as that stretch of the
 * whole does, and the same bytes are read. A part of 50 000 characters costs
 * well under a second idle, and a few dozen of the corpus's documents are cut.
 */
const PART_CHARS = 50_000;

/** One stretch of a document a rewrite case reads: where it starts, and its text. */
interface Part {
  /** The heading the part opens with, or "" for a document's lead. */
  from: string;
  text: string;
}

/**
 * `text` cut at its heading lines into parts of at most [PART_CHARS]: whole
 * sections run together while they fit, and a section longer than that is a
 * part on its own. The parts, joined, are the document byte for byte.
 */
function parts(text: string): Part[] {
  const headings = headingLines(text).filter((h) => h.at > 0);
  const starts = [0, ...headings.map((h) => h.at)];
  const titles = new Map(headingLines(text).map((h) => [h.at, h.title]));
  const out: Part[] = [];
  let open: { at: number; end: number } | null = null;
  for (let i = 0; i < starts.length; i++) {
    const at = starts[i]!;
    const end = starts[i + 1] ?? text.length;
    if (open !== null && end - open.at > PART_CHARS) {
      out.push({ from: titles.get(open.at) ?? "", text: text.slice(open.at, open.end) });
      open = null;
    }
    open = open === null ? { at, end } : { at: open.at, end };
  }
  if (open !== null)
    out.push({ from: titles.get(open.at) ?? "", text: text.slice(open.at, open.end) });
  return out;
}

const root = repoRoot();
const fixtures = join(root, "dashboard", "src", "test", "markdown");
const corpus: { name: string; text: string }[] = [
  ...["docs", "adr", "examples", "skills"]
    .flatMap((dir) => markdownUnder(join(root, dir)))
    .map((path) => ({ name: relative(root, path), text: readFileSync(path, "utf8") })),
  ...readdirSync(fixtures).map((name) => ({
    name: `fixture ${name}`,
    text: readFileSync(join(fixtures, name), "utf8"),
  })),
  ...TEMPLATES.map((t) => ({ name: `template ${t.value}`, text: t.body })),
  ...DOC_TEMPLATES.map((t) => ({ name: `doc template ${t.key}`, text: t.body })),
];

/** Elements whose box starts a new line of text. */
const BLOCK = new Set([
  "P",
  "H1",
  "H2",
  "H3",
  "H4",
  "H5",
  "H6",
  "LI",
  "UL",
  "OL",
  "PRE",
  "BLOCKQUOTE",
  "TABLE",
  "TR",
  "TH",
  "TD",
  "DIV",
  "HR",
  "BR",
]);

/**
 * What a reader sees of a body: its text as a browser sets it (a run of
 * whitespace is one space wherever it falls, and none at a line's ends), and
 * the elements that carry meaning, with what each points at. A span's
 * boundaries, two equal marks' nesting order and a code span's wrapping hint
 * are not seen, so they are not compared.
 */
function seen(markdown: string): { text: string; elements: string[] } {
  const { container, unmount } = render(<div>{renderMarkdown(markdown)}</div>);
  let text = "";
  const elements: string[] = [];
  const walk = (node: Node) => {
    if (node.nodeType === Node.TEXT_NODE) {
      text += node.textContent ?? "";
      return;
    }
    if (!(node instanceof HTMLElement)) return;
    const tag = node.tagName;
    if (BLOCK.has(tag)) text += "\n";
    if (tag !== "SPAN" && tag !== "DIV") {
      const at =
        node.getAttribute("href") ?? node.getAttribute("src") ?? node.getAttribute("start");
      elements.push(
        `${tag}${at ? `(${at})` : ""}${node.style.textAlign ? `[${node.style.textAlign}]` : ""}`,
      );
    }
    if (tag === "INPUT") elements.push(`checked=${(node as HTMLInputElement).checked}`);
    if (node.dataset.callout) elements.push(`callout=${node.dataset.callout}`);
    node.childNodes.forEach(walk);
    if (BLOCK.has(tag)) text += "\n";
  };
  walk(container);
  unmount();
  return {
    text: text
      .replace(/[ \t\n]*\n[ \t\n]*/g, "\n")
      .replace(/[ \t]+/g, " ")
      .trim(),
    // Inline formatting is compared by what is there, not by how it nests.
    elements: [...elements].sort(),
  };
}

describe("the rich editor's round trip", () => {
  test("reads a corpus large enough to mean something", () => {
    expect(corpus.length).toBeGreaterThan(100);
    expect(corpus.some((c) => c.name === "fixture every-block.md")).toBe(true);
  });

  test.each(corpus)("a load and a save with no edit change nothing in $name", ({ text }) => {
    expect(roundTripMarkdown(text)).toBe(text);
  });

  // A DOCUMENT IS READ IN PARTS once it is longer than [PART_CHARS] (which
  // says why), so no case's cost grows with the longest document the
  // repository holds.
  const rewritten = (text: string) => {
    const before = seen(text);
    const after = seen(canonicalMarkdown(text));
    expect(after.elements).toEqual(before.elements);
    // Compared where they first part, so a failure names the passage.
    let at = 0;
    while (at < before.text.length && before.text[at] === after.text[at]) at++;
    expect(after.text.slice(Math.max(0, at - 80), at + 80)).toBe(
      before.text.slice(Math.max(0, at - 80), at + 80),
    );
  };
  const whole = corpus.filter((c) => c.text.length <= PART_CHARS);
  const cut = corpus
    .filter((c) => c.text.length > PART_CHARS)
    .flatMap((c) => parts(c.text).map((p) => ({ name: c.name, ...p })));
  test.each(whole)("$name rewritten whole reads as it did", ({ text }) => rewritten(text));
  test.each(cut)("$name rewritten from “$from” reads as it did", ({ text }) => rewritten(text));

  // THE PARTS ARE THE DOCUMENT, every byte once and in order, each no longer
  // than a part may be unless it is one section — so reading them is reading
  // the document, and a cut that dropped or doubled a stretch fails here.
  test("a long document's parts are the document, cut at its headings", () => {
    const long = corpus.filter((c) => c.text.length > PART_CHARS);
    expect(long.map((c) => c.name)).toContain("docs/reference/api-endpoints.md");
    for (const { name, text } of long) {
      const cutUp = parts(text);
      expect(cutUp.map((p) => p.text).join(""), name).toBe(text);
      expect(cutUp.length, name).toBeGreaterThan(1);
      const starts = new Set(headingLines(text).map((h) => h.at));
      let at = 0;
      for (const p of cutUp) {
        if (at > 0) expect(starts.has(at), `${name} cut at ${at}`).toBe(true);
        if (p.text.length > PART_CHARS) {
          // One section on its own: no heading inside it to cut at.
          const inside = [...starts].filter((s) => s > at && s < at + p.text.length);
          expect(inside, `${name} part at ${at}`).toEqual([]);
        }
        at += p.text.length;
      }
    }
  });
});
