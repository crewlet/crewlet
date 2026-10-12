import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import {
  MODEL_WORDS,
  type RenderOptions,
  nestSections,
  outline,
  parseBlocks,
  plainText,
  renderInline,
  renderMarkdown,
  safeHref,
  splitSections,
  UnbrokenCode,
} from "./markdown.ts";
// THE FIXTURES ARE REAL FILES, imported verbatim rather than inlined as
// template literals: a document written in a `.md` file is a document
// somebody can read and edit as markdown, and a fixture that only exists
// inside a test string is one nobody checks against a real renderer.
import pageFixture from "../test/markdown/page.md?raw";
import hostileFixture from "../test/markdown/hostile.md?raw";

function draw(source: string): HTMLElement {
  const { container } = render(<div className="prose md">{renderMarkdown(source)}</div>);
  return container.firstElementChild as HTMLElement;
}

describe("a href", () => {
  it("allows the schemes a page legitimately links with", () => {
    expect(safeHref("https://docs.crewlet.ai/x")).toBe("https://docs.crewlet.ai/x");
    expect(safeHref("http://example.com")).toBe("http://example.com");
    expect(safeHref("mailto:ops@example.com")).toBe("mailto:ops@example.com");
    expect(safeHref("#/work/ENG-1")).toBe("#/work/ENG-1");
  });

  it("refuses everything else, by allowlist rather than by enumeration", () => {
    // A page is written by an agent acting on content it read somewhere
    // else, so a href in one is untrusted input.
    for (const bad of [
      "javascript:alert(1)",
      "JaVaScRiPt:alert(1)",
      "data:text/html;base64,PHNjcmlwdD4=",
      "vbscript:msgbox(1)",
      // A protocol-relative URL inherits the page's scheme and goes to
      // somebody else's host.
      "//evil.example.com/steal",
      // A bare path resolves against whatever origin serves the dashboard
      // and means nothing to a reader.
      "/admin/credentials",
      "guide.html",
      "",
      "   ",
    ]) {
      expect(safeHref(bad), bad).toBeNull();
    }
  });

  it("refuses a disallowed scheme however it is spelled", () => {
    // A browser drops whitespace and control characters before resolving a
    // scheme, so these reach `javascript:` — and the allowlist refuses them
    // whether or not it normalises first, because noise can only ever make a
    // string fail an allowlist.
    expect(safeHref("java\u0009script:alert(1)")).toBeNull();
    expect(safeHref("\n  javascript:alert(1)")).toBeNull();
    expect(safeHref("java\u0000script:alert(1)")).toBeNull();
  });

  it("returns the value it checked, never the raw one", () => {
    // THE LOAD-BEARING HALF of the normalisation. A href and its stripped
    // form can reach different destinations, so a function that checked one
    // and handed back the other would be vouching for a string nobody
    // navigates to.
    const noisy = "https://docs.crewlet.ai/\u0009guide";
    const got = safeHref(noisy);
    expect(got).not.toBe(noisy);
    expect(got).toBe("https://docs.crewlet.ai/guide");
    // And whatever comes back passes the check a second time, unchanged.
    expect(safeHref(got ?? "")).toBe(got);
  });
});

describe("a hostile document", () => {
  const source = hostileFixture;

  it("renders no element the renderer did not construct", () => {
    const el = draw(source);
    // THE WHOLE POINT. There is no dangerouslySetInnerHTML on this path, so
    // raw HTML in a document is text — and a document that could introduce
    // a script tag or an onerror handler would make every page in the
    // company a place to put one.
    expect(el.querySelector("script")).toBeNull();
    expect(el.querySelector("img[onerror]")).toBeNull();
    expect(el.querySelector("b")).toBeNull();
    // Not one element in the tree carries an event handler or a style,
    // whatever the document said: every element here was constructed by
    // this renderer with the props it chose.
    for (const node of el.querySelectorAll("*")) {
      for (const attr of node.attributes) {
        expect(attr.name.startsWith("on"), `${node.tagName}[${attr.name}]`).toBe(false);
      }
    }
    // The tags are still READABLE, as their own text: a page that wrote
    // about HTML should show what it wrote.
    expect(el.textContent).toContain("<script>alert(1)</script>");
    expect(el.textContent).toContain("<b>tags</b>");
  });

  it("renders a refused link as its own label, never as an anchor", () => {
    const el = draw(source);
    for (const a of el.querySelectorAll("a")) {
      expect(safeHref(a.getAttribute("href") ?? ""), a.outerHTML).not.toBeNull();
    }
    // The label survives so the reader can still see what the page said.
    expect(el.textContent).toContain("click me");
    expect(el.textContent).toContain("protocol relative");
    // AND NOTHING ELSE. A destination carrying balanced parentheses —
    // `javascript:alert(1)` is one — used to end the match early and leave
    // its closing bracket beside the label as a stray character.
    expect(el.textContent).not.toContain("click me)");
  });

  it("renders a refused image as its alt text", () => {
    const el = draw(source);
    expect(el.querySelector("img")).toBeNull();
    expect(el.textContent).toContain("bad image");
  });
});

describe("a company page", () => {
  const source = pageFixture;

  it("renders headings under the screen's own h1", () => {
    const el = draw(source);
    // A body emitting its own h1 makes two documents claim the same level
    // to a screen reader: the page's title is the h1 around this.
    expect(el.querySelector("h1")).toBeNull();
    expect(el.querySelector("h2")?.textContent).toBe("Migration off Pulsar");
    expect(el.querySelector("h3")?.textContent).toBe("What changed");
  });

  it("renders emphasis, code and both kinds of list", () => {
    const el = draw(source);
    expect(el.querySelector("strong")?.textContent).toBe("one NATS cluster");
    expect(el.querySelector("em")?.textContent).toBe("second");
    expect(el.querySelector("ol")?.querySelectorAll("li")).toHaveLength(3);
    expect(el.querySelector("code.inline")?.textContent).toBe("stream.type: pulsar");
    expect(el.querySelector("pre code")?.textContent).toContain("topics.NotificationsInbound");
    expect(el.querySelector("blockquote")?.textContent).toContain("compare-and-set");
    expect(el.querySelector("hr")).not.toBeNull();
  });

  it("names a fenced block for a recipe that exists", () => {
    // WHAT THE NAME COSTS WHEN IT IS WRONG. This block was `class="code
    // plain"`, and neither name is a recipe for a `pre` — `.grid-th.plain` is
    // a table header and nothing declares `.code` at all. So a fenced sample
    // in a page body, a work item's description or a phase prompt rendered
    // with no surface and, worse, with a `pre`'s own `white-space: pre` and
    // `overflow: visible`: one long line pushed the whole PAGE sideways. The
    // class is what ties the block to its recipe, so it is what is asserted —
    // and `md-code` is the family the other three block classes are already
    // in, which is the reason a missing one is now conspicuous.
    const el = draw(source);
    expect(el.querySelector("pre")?.className).toBe("md-code");
  });

  it("renders a task list as ticked and unticked boxes nobody can click", () => {
    const el = draw(source);
    const boxes = [...el.querySelectorAll<HTMLInputElement>("input[type=checkbox]")];
    expect(boxes.map((b) => b.checked)).toEqual([true, true, false]);
    // This surface performs no writes at all, so a box a reader could tick
    // would report a change that never reached the page.
    for (const box of boxes) expect(box.disabled).toBe(true);
  });

  it("nests a sublist under the item it belongs to", () => {
    const el = draw(source);
    const nested = el.querySelectorAll("ul ul li");
    expect([...nested].map((li) => li.textContent)).toEqual([
      "it still holds a volume",
      "and the volume holds a topic nobody reads",
    ]);
    // AND UNDER IT, NOT BESIDE IT. A task item's box and its content are a
    // flex row, so the content is one column: without the wrapper every
    // block of the item became a cell of that row, and the sublist rendered
    // to the RIGHT of the line it hangs off.
    const owner = el.querySelector("ul.md-tasks > li:last-child");
    const column = owner?.querySelector(":scope > .md-task-body");
    expect(column, "a task item's content is not wrapped").not.toBeNull();
    expect(column?.querySelector("ul")).not.toBeNull();
    expect(owner?.querySelector(":scope > ul")).toBeNull();
  });

  it("renders a table with its own alignment", () => {
    const el = draw(source);
    const headers = [...el.querySelectorAll("th")];
    expect(headers.map((h) => h.textContent)).toEqual(["Backend", "CAS", "Ran a second estate"]);
    expect(headers.map((h) => h.style.textAlign)).toEqual(["left", "center", "right"]);
    expect(el.querySelectorAll("tbody tr")).toHaveLength(2);
  });

  it("links out in a new tab and stays in this one for an app route", () => {
    // Scoped to THIS render rather than to the document: every draw in this
    // file stays mounted, so a document-wide query finds the same link once
    // per test that has run.
    const el = draw(source);
    const out = el.querySelector<HTMLAnchorElement>('a[href^="https://docs.crewlet.ai"]');
    expect(out?.textContent).toBe("the deployment guide");
    // `noopener` because a page's links are written by an agent from content
    // it read elsewhere; `noreferrer` because this URL names the company.
    expect(out?.getAttribute("target")).toBe("_blank");
    expect(out?.getAttribute("rel")).toBe("noopener noreferrer");

    const inApp = el.querySelector<HTMLAnchorElement>('a[href^="#/"]');
    expect(inApp?.getAttribute("href")).toBe("#/work/ENG-214");
    // An in-app link IS this application: opening it in a tab would break
    // every internal reference a page makes.
    expect(inApp?.getAttribute("target")).toBeNull();
    expect(inApp?.getAttribute("rel")).toBeNull();

    const mail = el.querySelector<HTMLAnchorElement>('a[href^="mailto:"]');
    expect(mail?.getAttribute("href")).toBe("mailto:ops@example.com");
  });

  it("wraps a soft line break and honours a hard one", () => {
    const el = draw(source);
    // A hand-wrapped paragraph reads as a paragraph, not as a column of
    // short lines — which is what a pre-wrap render made of every one.
    const first = el.querySelector("p");
    expect(first?.textContent).toContain("is one NATS cluster now, and the coordination");
    expect(first?.querySelector("br")).toBeNull();
    expect(el.querySelectorAll("br").length).toBe(1);
  });
});

describe("the block parser", () => {
  it("does not read a line of pipes as a table without its delimiter", () => {
    // A shell pipeline in a paragraph is not a one-column table.
    const blocks = parseBlocks("run `cat x | grep y` and see");
    expect(blocks).toHaveLength(1);
    expect(blocks[0]?.kind).toBe("paragraph");
  });

  it("keeps markdown inside a code span literal", () => {
    const el = draw("use `**not bold**` here");
    expect(el.querySelector("strong")).toBeNull();
    expect(el.querySelector("code")?.textContent).toBe("**not bold**");
  });

  it("does not close a fence on a line that merely starts with its marker", () => {
    // A closing fence carries no info string, so a ```ts line inside a ```sh
    // sample is CONTENT. Read as a close it ended the block on its first line
    // and the whole sample was thrown away — an empty code block where a page
    // was teaching somebody how to write one.
    const blocks = parseBlocks("```sh\n```ts\n# install deps\n```");
    expect(blocks).toEqual([{ kind: "code", lang: "sh", text: "```ts\n# install deps" }]);
  });

  it("renders an unterminated fence rather than dropping the rest", () => {
    const el = draw("before\n\n```\nnever closed\n");
    expect(el.querySelector("pre code")?.textContent).toBe("never closed");
    expect(el.textContent).toContain("before");
  });

  it("keeps an ordered list's own start", () => {
    const el = draw("3. three\n4. four");
    expect(el.querySelector("ol")?.getAttribute("start")).toBe("3");
  });

  it("treats a bulleted run after a numbered one as its own list", () => {
    const blocks = parseBlocks("1. one\n2. two\n- a\n- b");
    expect(blocks.map((b) => b.kind)).toEqual(["list", "list"]);
  });

  it("answers an empty document with nothing rather than throwing", () => {
    expect(parseBlocks("")).toEqual([]);
    expect(renderMarkdown("")).toEqual([]);
  });
});

/**
 * SECTIONING IS THE OPPOSITE WALK: the source back, grouped, unchanged.
 *
 * Its one caller renders a phase prompt, which is a RECORD — an operator
 * reproduces a turn from it and diffs it when a model starts behaving
 * differently — so the property that matters is not what a section LOOKS like
 * but that the split loses nothing and invents nothing.
 */
describe("sectioning", () => {
  it("splits on ATX headings and keeps the run before the first one", () => {
    const sections = splitSections("lead line\n\n# One\nunder one\n\n## Two\nunder two");
    expect(sections).toEqual([
      { level: 0, title: "", body: "lead line" },
      { level: 1, title: "One", body: "under one" },
      { level: 2, title: "Two", body: "under two" },
    ]);
  });

  it("does not read a heading inside a fenced block", () => {
    // The drift this walk exists to avoid. A tool catalogue and a skill body
    // both carry shell samples, and `# install deps` is a comment — a split
    // that took it for a heading would cut a document where nobody wrote one.
    const sections = splitSections("## Setup\n```sh\n# install deps\nnpm ci\n```\ndone");
    expect(sections).toHaveLength(1);
    expect(sections[0]?.title).toBe("Setup");
    expect(sections[0]?.body).toBe("```sh\n# install deps\nnpm ci\n```\ndone");
  });

  it("loses no line of the document and reorders none", () => {
    // THE CLAIM THE SCREEN RESTS ON, over a real document rather than a case
    // somebody thought of: every non-blank line that is not a heading appears
    // in exactly one section, verbatim, in source order.
    const sections = splitSections(pageFixture);
    const kept = sections.flatMap((s) => s.body.split("\n"));
    const expected = pageFixture
      .split("\n")
      .filter((line) => line.trim() !== "" && !/^#{1,6}\s/.test(line));
    // Blank lines INSIDE a section are kept; only the ones bounding a run go.
    expect(kept.filter((line) => line.trim() !== "")).toEqual(expected);
    // And the headings are the sections, in order.
    expect(sections.filter((s) => s.level > 0).map((s) => s.title)).toEqual(
      pageFixture
        .split("\n")
        .filter((line) => /^#{1,6}\s/.test(line))
        .map((line) => line.replace(/^#{1,6}\s+/, "").trim()),
    );
  });

  it("does not end a fence on a line that merely starts with its marker", () => {
    // The same defect one level up, and worse: the block ends early and every
    // `# ` line left in the sample becomes a section of the DOCUMENT.
    const sections = splitSections("## Sample\n```sh\n```ts\n# install deps\n```\ntail");
    expect(sections.map((s) => s.title)).toEqual(["Sample"]);
    expect(sections[0]?.body).toBe("```sh\n```ts\n# install deps\n```\ntail");
  });

  it("hands a body back with the line endings the document had", () => {
    // A prompt carries a webhook's task text and a page's body verbatim, and a
    // GitHub issue body is CRLF. Normalised, a fold showed an edited copy of a
    // record the whole-document view showed unchanged.
    const source = "lead\r\n\r\n## One\r\na\r\nb\r\n\r\n## Two\r\nc\r\n";
    expect(splitSections(source).map((s) => s.body)).toEqual(["lead", "a\r\nb", "c"]);
  });

  it("makes every body a slice of the source, whatever its line endings", () => {
    // THE CONTRACT THE SCREEN RESTS ON, stated as the thing a caller can check:
    // a section's body is found in the document, so the folds and the whole
    // view can never disagree about what was sent.
    for (const source of [
      "lead\n\n## One\na\nb\n\n## Two\nc\n",
      "lead\r\n\r\n## One\r\na\r\nb\r\n\r\n## Two\r\nc\r\n",
      "## Mixed\r\na\nb\r\nc",
    ]) {
      for (const section of splitSections(source)) {
        expect(source).toContain(section.body);
      }
    }
  });

  it("answers a document with no headings as one untitled run", () => {
    // Which is how the caller knows there is no outline to draw — not by
    // counting sections, which would read a one-heading document as none.
    expect(splitSections("just prose\nover two lines")).toEqual([
      { level: 0, title: "", body: "just prose\nover two lines" },
    ]);
  });

  it("keeps a heading that has nothing under it", () => {
    // A fact about the document, so it is reported rather than swallowed.
    expect(splitSections("## Empty\n\n## Next\nbody")).toEqual([
      { level: 2, title: "Empty", body: "" },
      { level: 2, title: "Next", body: "body" },
    ]);
  });

  it("drops a heading's decoration but never its words", () => {
    expect(splitSections("## Closing hashes ##\nbody")[0]?.title).toBe("Closing hashes");
    // Inline markup is the author's, and a title is shown as written.
    expect(splitSections("## **Bold** heading\nbody")[0]?.title).toBe("**Bold** heading");
  });

  it("answers an empty document with nothing rather than throwing", () => {
    expect(splitSections("")).toEqual([]);
    expect(splitSections("   \n\n  ")).toEqual([]);
  });
});

/**
 * AN OUTLINE IS THE DOCUMENT'S SHAPE, NOT A LIST OF ITS HEADINGS.
 *
 * The executor's user message is the case: its conversation ledger writes one
 * `###` per prior turn INSIDE `## Earlier in this conversation`, and flattened
 * those read as blocks of the message in their own right, sitting between it
 * and the ask.
 */
describe("nesting", () => {
  /** An outline as `title(child, child)`, which is the shape under test. */
  function shape(source: string): string {
    const render = (nodes: ReturnType<typeof nestSections>): string =>
      nodes
        .map((n) => {
          const name = n.level === 0 ? "(lead)" : n.title;
          return n.children.length ? `${name}(${render(n.children)})` : name;
        })
        .join(", ");
    return render(nestSections(splitSections(source)));
  }

  it("puts a heading under the nearest shallower one", () => {
    expect(
      shape("## Ledger\nrules\n\n### Turn one\na\n\n### Turn two\nb\n\n## Task\nthe ask"),
    ).toBe("Ledger(Turn one, Turn two), Task");
  });

  it("keeps a run of peers flat", () => {
    // Which is every prompt this engine builds: the company's policies are not
    // part of the turn contract, they are the next thing the model reads.
    expect(shape("## One\na\n\n## Two\nb\n\n## Three\nc")).toBe("One, Two, Three");
  });

  it("never nests a document inside its opening lines", () => {
    // The lead run has no heading, so it contains nothing. At level 0 it would
    // otherwise be shallower than everything and swallow the whole document.
    expect(shape("an identity line\n\n## One\na\n\n## Two\nb")).toBe("(lead), One, Two");
  });

  it("carries a deeper heading with no parent as a section in its own right", () => {
    // A malformed document is still a document: the alternative is dropping it.
    expect(shape("### Deep\na\n\n## Shallow\nb")).toBe("Deep, Shallow");
  });

  it("closes a level when a shallower heading arrives", () => {
    expect(shape("## A\n\n### A1\n\n#### A1a\n\n### A2\n\n## B")).toBe("A(A1(A1a), A2), B");
  });

  it("answers an empty list with an empty outline", () => {
    expect(nestSections([])).toEqual([]);
  });
});

/**
 * A BODY IS RENDERED; AN EXCERPT IS FLATTENED.
 *
 * The engine's `excerpt` is a CUT of a markdown body, and the surfaces that draw
 * one are a single line inside a grid track. `renderMarkdown` is wrong there
 * three times over: `.truncate` is `white-space: nowrap`, which cannot clip a
 * block box at all; several of those cells are a `<p>`, where a block child is
 * invalid; and a cut fragment stops mid-construct. So the tracker's feed printed
 * "## Understanding the work This task is to interview three energy trading
 * desks…" with the hashes in it.
 */
describe("plain text", () => {
  it("renders every construct to the prose it draws", () => {
    expect(plainText("# Title\n\nbody")).toBe("Title body");
    expect(plainText("- one\n- two")).toBe("one two");
    // A TASK ITEM KEEPS ITS BOX: `[ ]` is the one piece of syntax whose meaning
    // survives being flattened, and dropping it turns "not done yet" into a
    // statement of fact.
    expect(plainText("- [ ] ship it\n- [x] merged")).toBe("[ ] ship it [x] merged");
    expect(plainText("use `**not bold**` here")).toBe("use **not bold** here");
    expect(plainText("![a diagram](x.png)")).toBe("a diagram");
    expect(plainText("read [the guide](https://docs.crewlet.ai/x)")).toBe("read the guide");
    expect(plainText("<https://docs.crewlet.ai/x>")).toBe("https://docs.crewlet.ai/x");
    expect(plainText("a\n\n---\n\nb")).toBe("a b");
    expect(plainText("| h |\n| --- |\n| c |")).toBe("h c");
  });

  it("emits no markdown syntax over a whole real document", () => {
    // THE PAGE FIXTURE, because a case list only covers what somebody thought
    // of. Every construct the renderer knows is in that file.
    const flat = plainText(pageFixture);
    expect(flat).not.toMatch(/^#|\s#{1,6}\s|\*\*|```|\| ---/);
    expect(flat).not.toContain("\n");
  });

  it("answers an empty document with an empty string rather than throwing", () => {
    expect(plainText("")).toBe("");
    expect(plainText("   \n\n  ")).toBe("");
  });
});

// AN OUTLINE AND ITS ANCHORS ARE ONE WALK: every entry "On this page" draws
// points at an id the rendered body actually carries, a repeated heading gets
// an address of its own, and a heading inside a quote is part of the quote.
describe("the outline", () => {
  const doc = "# Top\n\n## Steps\n\nx\n\n## Steps\n\n> ## quoted\n\n## `code` *and* [link](#/x)";

  it("names each top-level heading, a repeat suffixed", () => {
    expect(outline(doc)).toEqual([
      { level: 1, text: "Top", id: "h-top" },
      { level: 2, text: "Steps", id: "h-steps" },
      { level: 2, text: "Steps", id: "h-steps-2" },
      { level: 2, text: "code and link", id: "h-code-and-link" },
    ]);
  });

  it("the rendered body carries every id the outline points at", () => {
    const { container } = render(<div>{renderMarkdown(doc, { anchors: true })}</div>);
    for (const h of outline(doc)) expect(container.querySelector(`#${h.id}`)).toBeTruthy();
    // And nothing inside the quote is an anchor.
    expect(container.querySelectorAll("[id]")).toHaveLength(outline(doc).length);
  });

  it("a body rendered without anchors carries no ids", () => {
    const { container } = render(<div>{renderMarkdown(doc)}</div>);
    expect(container.querySelectorAll("[id]")).toHaveLength(0);
  });
});

describe("inline code", () => {
  // A SHORT COMMAND IS ONE TOKEN: split at a space it reads as two commands
  // (`nimbus jobs wait --` / `node <n>` at 1280). A long span may still wrap,
  // or it would overflow a phone's column.
  it("keeps a short span whole and lets a long one wrap", () => {
    const el = draw("Wait (`nimbus jobs wait --node <n>`) or read `" + "x".repeat(40) + "`.");
    const [short, long] = Array.from(el.querySelectorAll("code.inline"));
    expect(short?.classList.contains("is-whole")).toBe(true);
    expect(long?.classList.contains("is-whole")).toBe(false);
    expect("nimbus jobs wait --node <n>".length).toBeLessThanOrEqual(UnbrokenCode);
  });
});

/**
 * A MODEL'S WORDS ARE MARKDOWN, READ BY A MODEL'S HABITS.
 *
 * A round's speech showed a fenced ```json block as literal backticks — a model
 * writes markdown as readily as a person does. But it does not hand-wrap
 * paragraphs and it does not section a transcript: its single newlines are the
 * lines it meant, and its "## Summary" is emphasis inside one item of a page
 * whose outline belongs to the page.
 */
describe("a model's words", () => {
  function words(source: string, options: RenderOptions = MODEL_WORDS): HTMLElement {
    const { container } = render(<div className="prose md">{renderMarkdown(source, options)}</div>);
    return container.firstElementChild as HTMLElement;
  }

  it("keep every newline as the line break the model meant", () => {
    const el = words("Name: Engineer\nTeam: Platform\nReports to: Lead");
    expect(el.querySelectorAll("p")).toHaveLength(1);
    expect(el.querySelectorAll("br")).toHaveLength(2);
    // THE CONTROL: a document reads the same newlines as soft wraps.
    const doc = draw("Name: Engineer\nTeam: Platform\nReports to: Lead");
    expect(doc.querySelectorAll("br")).toHaveLength(0);
  });

  it("consume a break marker with its newline rather than printing it", () => {
    const el = words("one\\\ntwo  \nthree");
    expect(el.textContent).toBe("onetwothree");
    expect(el.querySelectorAll("br")).toHaveLength(2);
  });

  it("break lines inside a list item and a quote too", () => {
    const el = words("- first\n  second\n\n> said\n> twice");
    expect(el.querySelector("li")?.querySelectorAll("br")).toHaveLength(1);
    expect(el.querySelector("blockquote")?.querySelectorAll("br")).toHaveLength(1);
  });

  it("draw a heading line as a styled line of text, never a heading of the page", () => {
    const el = words("## Summary\nPosted it.\n\n> ### Inside a quote");
    expect(el.querySelectorAll("h1, h2, h3, h4, h5, h6")).toHaveLength(0);
    const heads = [...el.querySelectorAll(".md-heading")].map((h) => h.textContent);
    expect(heads).toEqual(["Summary", "Inside a quote"]);
    // Its inline markup still renders.
    expect(words("## The `submit_work` call").querySelector(".md-heading code")).not.toBeNull();
    // THE CONTROL: a document's heading is still a heading.
    expect(draw("## Summary").querySelector("h3")).not.toBeNull();
  });

  it("keep a fence byte for byte, closed or still streaming", () => {
    const closed = words('```json\n{"outcome": "delivered"}\n```');
    expect(closed.querySelector("pre.md-code")?.textContent).toBe('{"outcome": "delivered"}');
    // A round still being written can end inside a fence; what has arrived is
    // shown as code rather than as the rest of the document.
    const open = words('Here:\n```json\n{"outcome": "deli');
    expect(open.querySelector("pre.md-code")?.textContent).toBe('{"outcome": "deli');
  });

  it("render raw HTML as its text and refuse a link outside the allowlist", () => {
    const el = words('<img src=x onerror="alert(1)">\n[go](javascript:alert(1))');
    expect(el.querySelector("img")).toBeNull();
    expect(el.querySelector("a")).toBeNull();
    expect(el.textContent).toContain("<img src=x");
  });

  it("give a heading no anchor when there is no heading to anchor", () => {
    const el = words("## Summary", { ...MODEL_WORDS, anchors: true });
    expect(el.querySelector("[id]")).toBeNull();
  });
});

/**
 * Emphasis by CommonMark's delimiter-run rules — the reason being every
 * snake_case name a model or a prompt mentions without a code span. Each case
 * runs in BOTH readings, a document's and a model's words, because the
 * inline grammar is one walk under both and the transcript and the prompt
 * reader are where the names are.
 */
describe("emphasis", () => {
  const modes: [string, RenderOptions | undefined][] = [
    ["a document", undefined],
    ["a model's words", MODEL_WORDS],
  ];
  function inline(source: string, options?: RenderOptions): HTMLElement {
    const { container } = render(<div className="prose md">{renderMarkdown(source, options)}</div>);
    return container.firstElementChild as HTMLElement;
  }
  const marked = (el: HTMLElement, tag: "em" | "strong") =>
    [...el.querySelectorAll(tag)].map((e) => e.textContent);

  for (const [mode, options] of modes) {
    describe(`in ${mode}`, () => {
      it("keeps an underscore inside a word as a letter of it", () => {
        for (const said of [
          "I will call submit_work with the result_summary field now.",
          "fields: task_id, result_summary, next_step",
          "Use list_mcp_server_tools to discover, then call snake_case_name.",
          // One guard each: a run that could only OPEN inside a word, and one
          // that could only CLOSE inside one.
          "the key is foo_bar_",
          "_private_name is internal",
        ]) {
          const el = inline(said, options);
          expect(el.querySelector("em, strong"), said).toBeNull();
          expect(el.textContent, said).toBe(said);
        }
      });

      it("keeps a tool catalogue's names whole", () => {
        // The executor's "## Available tools" lines, which name every tool
        // bare — the prompt reader's default view of what the model was told.
        const el = inline(
          "- list_mcp_server_tools: list what a server offers\n- post_message: post to a chat_thread",
          options,
        );
        expect([...el.querySelectorAll("li")].map((li) => li.textContent)).toEqual([
          "list_mcp_server_tools: list what a server offers",
          "post_message: post to a chat_thread",
        ]);
        expect(el.querySelector("em")).toBeNull();
      });

      it("still emphasises what was marked, underscores included", () => {
        // THE CONTROLS: each of these is emphasis to CommonMark, so a guard
        // that stopped them would be a guard against emphasis, not intraword.
        expect(marked(inline("*emph* and _emph_", options), "em")).toEqual(["emph", "emph"]);
        expect(marked(inline("**strong** and __strong__", options), "strong")).toEqual([
          "strong",
          "strong",
        ]);
        // An underscore inside the run is a letter; the run is the emphasis.
        expect(marked(inline("_a_b_ c", options), "em")).toEqual(["a_b"]);
        expect(marked(inline("__init__ method", options), "strong")).toEqual(["init"]);
        // An asterisk inside a word still opens and closes, as the spec says.
        expect(marked(inline("foo*bar*", options), "em")).toEqual(["bar"]);
        expect(marked(inline("multiply 2*3 and 4*5", options), "em")).toEqual(["3 and 4"]);
        // Inside parentheses, beside punctuation, is still a word boundary.
        expect(marked(inline("(see _this_).", options), "em")).toEqual(["this"]);
        expect(marked(inline("***both***", options), "strong")).toEqual(["both"]);
        expect(marked(inline("***both***", options), "em")).toEqual(["both"]);
      });

      it("never opens on whitespace or closes after it", () => {
        expect(inline("*foo *", options).querySelector("em")).toBeNull();
        expect(inline("a * b * c", options).querySelector("em")).toBeNull();
        expect(inline("_ foo_", options).querySelector("em")).toBeNull();
        expect(inline("** not strong**", options).querySelector("strong")).toBeNull();
      });

      it("judges a run beside another construct by the same rules", () => {
        // A word that runs on from a code span is still a word.
        const el = inline("`a`b_c and d_e", options);
        expect(el.querySelector("em")).toBeNull();
        expect(el.textContent).toBe("ab_c and d_e");
        // A construct's closing mark is punctuation, so a run against it opens.
        expect(marked(inline("`a`_b_ and [x](https://example.com)*y*", options), "em")).toEqual([
          "b",
          "y",
        ]);
      });
    });
  }

  it("draws a link as its words where the run sits inside a control", () => {
    // `links: "text"` is for an outline row or a picker's option: a link
    // inside one is unreachable as itself and steals the row's click.
    const { container } = render(
      <p>
        {renderInline("see [**the** guide](https://example.com) or <https://example.com/x>", "k", {
          links: "text",
        })}
      </p>,
    );
    expect(container.querySelector("a")).toBeNull();
    expect(container.textContent).toBe("see the guide or https://example.com/x");
    // The label is still markdown.
    expect(container.querySelector("strong")?.textContent).toBe("the");
    // THE CONTROL: the default is a link.
    const linked = render(<p>{renderInline("see [the guide](https://example.com)")}</p>);
    expect(linked.container.querySelector("a")?.textContent).toBe("the guide");
  });

  it("flattens to plain text by the same rules", () => {
    expect(plainText("call submit_work with result_summary")).toBe(
      "call submit_work with result_summary",
    );
    expect(plainText("_a_b_ and **c**")).toBe("a_b and c");
  });
});
