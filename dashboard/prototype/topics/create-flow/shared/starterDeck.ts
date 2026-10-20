/**
 * The engine's starter deck, drawn before it exists: the four slides a deck
 * is created from (`internal/artifacts/slideformat/starter.go`: a cover, an
 * agenda, two columns and a closing slide), read into the tree the shipped
 * renderer draws, so a new deck opens on its cover with every line editable
 * before anything is made. What the person changes is kept as edits by slide
 * and node path, and applied to the real starter once it is created.
 */

import type { DeckSlide, SlideNode } from "~/protocol/index.ts";

/** One text change: which slide (by its place in the deck), which node, and the new text. */
export interface SlideTextEdit {
  slide: number;
  path: string;
  text: string;
}

/** The starter's slides, as `starter.go` writes them, `{{title}}` where the title goes. */
const STARTER: readonly { id: string; markup: string }[] = [
  {
    id: "cover",
    markup: `<section id="cover" style="background:#1d2433;color:#f5f3ee;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;justify-content:center;gap:32px">
<h1 style="font-size:112px;font-weight:600;line-height:1.05;letter-spacing:-2px">{{title}}</h1>
<p style="font-size:40px;color:#c7cdd8">A subtitle: who it is for, and when</p>
<aside>The cover. Say what the deck is for in one sentence before the first slide.</aside>
</section>`,
  },
  {
    id: "agenda",
    markup: `<section id="agenda" style="background:#f7f6f2;color:#1d2433;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;gap:48px">
<h2 style="font-size:72px;font-weight:400;line-height:1.1">Agenda</h2>
<ol style="font-size:40px;line-height:1.5">
<li>Where we are</li>
<li>What we will do</li>
<li>What we need</li>
</ol>
<aside>Three parts at most. Each one becomes a section of the outline.</aside>
</section>`,
  },
  {
    id: "points",
    markup: `<section id="points" style="background:#f7f6f2;color:#1d2433;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;gap:48px">
<h2 style="font-size:72px;font-weight:400;line-height:1.1">One idea per slide</h2>
<div style="display:flex;gap:32px">
<div style="flex:1;display:flex;flex-direction:column;gap:12px;background:#ffffff;padding:40px;border:1px solid #e2e4e8;border-radius:16px">
<h3 style="font-size:36px;font-weight:600">The point</h3>
<p style="font-size:28px">Say it in one sentence a person remembers.</p>
</div>
<div style="flex:1;display:flex;flex-direction:column;gap:12px;background:#ffffff;padding:40px;border:1px solid #e2e4e8;border-radius:16px">
<h3 style="font-size:36px;font-weight:600">The evidence</h3>
<p style="font-size:28px">A number, a quote or a picture that backs it.</p>
</div>
</div>
<aside>Two cards side by side. Click any text to change it.</aside>
</section>`,
  },
  {
    id: "close",
    markup: `<section id="close" style="background:#1d2433;color:#f5f3ee;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;justify-content:center;gap:32px">
<h2 style="font-size:88px;font-weight:600;line-height:1.1">What we need</h2>
<p style="font-size:40px;color:#c7cdd8">The one decision or the one ask, said plainly</p>
<aside>End on what the room should do next.</aside>
</section>`,
  },
];

function declsOf(style: string): { p: string; v: string }[] {
  return style
    .split(";")
    .map((part) => part.trim())
    .filter(Boolean)
    .map((part) => {
      const at = part.indexOf(":");
      return { p: part.slice(0, at).trim().toLowerCase(), v: part.slice(at + 1).trim() };
    });
}

/** A DOM element as the slide tree holds it, whitespace between blocks dropped as the parser drops it. */
function nodeOf(el: Element): SlideNode {
  const children: SlideNode[] = [];
  for (const child of Array.from(el.childNodes)) {
    if (child.nodeType === Node.TEXT_NODE) {
      const text = child.textContent ?? "";
      if (text.trim() !== "") children.push({ text });
    } else if (child instanceof Element && child.tagName.toLowerCase() !== "aside") {
      children.push(nodeOf(child));
    }
  }
  const style = el.getAttribute("style");
  return {
    tag: el.tagName.toLowerCase(),
    ...(style ? { style: declsOf(style) } : {}),
    ...(children.length > 0 ? { children } : {}),
  };
}

/** The starter's slides for a deck called `title`. */
export function starterSlides(title: string): DeckSlide[] {
  const parser = new DOMParser();
  return STARTER.map(({ id, markup }) => {
    const doc = parser.parseFromString(
      markup.replace("{{title}}", title.replace(/[<&>]/g, " ")),
      "text/html",
    );
    const section = doc.querySelector("section")!;
    const notes = section.querySelector("aside")?.textContent?.trim() ?? "";
    const root = nodeOf(section);
    const heading = section.querySelector("h1, h2")?.textContent?.trim() ?? id;
    return { id, path: `slides/${id}.html`, title: heading, notes, root };
  });
}

/** The cover title's text: the cover's first child, its one run of text. */
export const COVER_TITLE_PATH = "0/0";
