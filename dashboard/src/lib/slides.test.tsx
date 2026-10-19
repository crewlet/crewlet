/**
 * A slide drawn from the engine's canonical tree, and written back.
 *
 * The deck these draw is the engine's own answer for its twelve-slide test
 * deck (`internal/artifacts/slideformat/testdata/deck.golden.json`), which a
 * Go test holds equal to what the parser sends, so the renderer is never
 * tested on a tree the engine would not produce.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { render, within } from "@testing-library/react";
import { describe, expect, test, vi } from "vitest";
import type { ArtifactDeck, DeckSlide, SlideNode } from "~/protocol/index.ts";
import { nodeAt, renderSlide, slideSource, slideStyle, titleOf, withText } from "./slides.tsx";

const golden = JSON.parse(
  readFileSync(
    join(process.cwd(), "..", "internal/artifacts/slideformat/testdata/deck.golden.json"),
    "utf8",
  ),
) as ArtifactDeck;
const slides = golden.slides ?? [];
const slide = (id: string) => slides.find((s) => s.id === id)!;
const ctx = { prefix: "t-", image: (path: string) => `blob:test/${path}` };

describe("drawing a slide", () => {
  test("every slide of the deck draws at the stage's size", () => {
    expect(slides).toHaveLength(12);
    for (const s of slides) {
      const { container, unmount } = render(<>{renderSlide(s, ctx)}</>);
      const canvas = container.firstElementChild as HTMLElement;
      expect(canvas.className).toBe("slide-canvas");
      expect(canvas.style.width).toBe("1920px");
      expect(canvas.style.height).toBe("1080px");
      expect(canvas.textContent).toContain(s.title);
      unmount();
    }
  });

  test("the notes are never on the slide", () => {
    const { container } = render(<>{renderSlide(slide("cover"), ctx)}</>);
    expect(container.textContent).not.toContain("Welcome everyone");
  });

  test("an image is the object URL of its version file, with its alt", () => {
    const { container } = render(<>{renderSlide(slide("team"), ctx)}</>);
    const img = container.querySelector("img")!;
    expect(img.getAttribute("src")).toBe("blob:test/images/team.png");
    expect(img.getAttribute("alt")).toBe("The support team");
    const pending = render(<>{renderSlide(slide("team"), { ...ctx, image: () => null })}</>);
    expect(pending.container.querySelector("img")).toBeNull();
    expect(within(pending.container).getByRole("img", { name: "The support team" })).toBeTruthy();
  });

  test("a drawing's ids and references move into the instance's own space", () => {
    const { container } = render(<>{renderSlide(slide("chart"), { ...ctx, prefix: "one-" })}</>);
    expect(container.querySelector("linearGradient")?.getAttribute("id")).toBe("one-bar");
    expect(container.querySelector("rect")?.getAttribute("fill")).toBe("url(#one-bar)");
    expect(container.querySelector("svg")?.getAttribute("viewBox")).toBe("0 0 800 400");
  });

  test("a link opens the https address in a new tab and gives it no opener", () => {
    const { container } = render(<>{renderSlide(slide("ask"), ctx)}</>);
    const a = container.querySelector("a")!;
    expect(a.getAttribute("href")).toBe("https://example.com/plan");
    expect(a.getAttribute("rel")).toBe("noopener noreferrer");
  });

  test("a tree the engine would not send is drawn as its text, and nothing else", () => {
    const hostile: DeckSlide = {
      id: "x",
      path: "slides/x.html",
      title: "x",
      root: {
        tag: "section",
        children: [
          { tag: "script", children: [{ text: "alert(1)" }] },
          {
            tag: "p",
            attrs: { onclick: "x()" },
            style: [
              { p: "background", v: "url(https://x.test/a)" },
              { p: "z-index", v: "9" },
              { p: "color", v: "#123456" },
            ],
            children: [{ text: "Hi" }],
          },
          { tag: "a", attrs: { href: "javascript:alert(1)" }, children: [{ text: "link" }] },
          {
            tag: "svg",
            children: [
              { tag: "foreignObject", children: [{ text: "no" }] },
              { tag: "use", attrs: { href: "https://x.test/s.svg#a" } },
            ],
          },
        ],
      },
    };
    const { container } = render(<>{renderSlide(hostile, ctx)}</>);
    expect(container.querySelector("script, foreignObject, a")).toBeNull();
    expect(container.querySelector("use")?.getAttribute("href")).toBeNull();
    const p = container.querySelector("p")!;
    expect(p.getAttribute("onclick")).toBeNull();
    expect(p.style.background).toBe("");
    expect(p.style.zIndex).toBe("");
    expect(p.style.color).toBe("rgb(18, 52, 86)");
    expect(container.textContent).toContain("alert(1)");
  });

  test("a style is held to the subset again, a vendor property camelCased", () => {
    expect(
      slideStyle([
        { p: "-webkit-text-stroke", v: "2px #000" },
        { p: "font-size", v: "48px" },
        { p: "behavior", v: "x" },
        { p: "color", v: "var(--x)" },
      ]),
    ).toEqual({ WebkitTextStroke: "2px #000", fontSize: "48px" });
  });
});

describe("editing in place", () => {
  test("every run of text is editable, and a finished edit reports its path", () => {
    const onText = vi.fn();
    const { container } = render(<>{renderSlide(slide("plan"), { ...ctx, edit: { onText } })}</>);
    const run = [...container.querySelectorAll<HTMLElement>("[data-run]")].find((r) =>
      r.textContent?.includes("plcae"),
    )!;
    expect(run.getAttribute("contenteditable")).toBe("plaintext-only");
    run.textContent = "Every request lands in one place.";
    run.dispatchEvent(new FocusEvent("blur", { bubbles: false }));
    run.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
    expect(onText).toHaveBeenCalledWith(run.dataset.run, "Every request lands in one place.");
    const edited = withText(
      slide("plan").root,
      run.dataset.run!,
      "Every request lands in one place.",
    );
    expect(nodeAt(edited, run.dataset.run!)?.text).toBe("Every request lands in one place.");
    expect(nodeAt(slide("plan").root, run.dataset.run!)?.text).toContain("plcae");
  });

  test("a slide's title is its first heading, as the engine reads it", () => {
    for (const s of slides) expect(titleOf(s.root)).toBe(s.title);
  });
});

describe("writing a slide back", () => {
  test("a slide is written as one section with its notes last", () => {
    const src = slideSource({ ...slide("plan") });
    expect(
      src.startsWith('<section id="plan" data-transition="push" style="background:#fbfbf8;'),
    ).toBe(true);
    expect(src).toContain('<p style="font-size:28px">Every request lands in one plcae.</p>');
    expect(
      src
        .trimEnd()
        .endsWith(
          "<aside>Recieve the questions first, then show the two cards.</aside>\n</section>",
        ),
    ).toBe(true);
  });

  test("text and attributes are escaped, and a drawing's empty element closes itself", () => {
    const root: SlideNode = {
      tag: "section",
      style: [{ p: "background", v: "#fff" }],
      children: [
        { tag: "p", children: [{ text: 'A < B & "C"' }] },
        {
          tag: "svg",
          attrs: { "aria-label": 'x"y' },
          children: [{ tag: "rect", attrs: { width: "1" } }],
        },
      ],
    };
    const src = slideSource({ id: "a", root, notes: "1 < 2" });
    expect(src).toContain('<p>A &lt; B &amp; "C"</p>');
    expect(src).toContain('<svg aria-label="x&quot;y"><rect width="1"/></svg>');
    expect(src).toContain("<aside>1 &lt; 2</aside>");
    expect(src).not.toContain(" hidden");
  });

  test("writing is stable: a slide written twice is written the same", () => {
    for (const s of slides)
      expect(slideSource(s)).toBe(slideSource(JSON.parse(JSON.stringify(s)) as DeckSlide));
  });
});
