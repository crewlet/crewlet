import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { parseBlocks, plainText, renderMarkdown, safeHref } from "./markdown.ts";
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
