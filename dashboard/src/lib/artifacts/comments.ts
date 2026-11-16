/**
 * An artifact's threads as the dashboard draws the engine's answer: the list
 * each is under, a draft in the engine's own words (`commentanchor.Label`),
 * and where a pin stands. The engine places every thread; nothing here does.
 */

import type {
  ArtifactCommentAnchor,
  ArtifactCommentThread,
  ArtifactKind,
  SlideNode,
} from "~/protocol/index.ts";
import type { CommentAnchorBox, CommentLayout } from "~/runtime/bridge.ts";

/** The three lists a thread is filed under. */
export type CommentFilter = "open" | "resolved" | "outdated";

export const COMMENT_FILTERS: readonly CommentFilter[] = ["open", "resolved", "outdated"];

/** The kinds whose parts a reader can place a comment on; every kind takes a comment on its whole version. */
export const PLACEABLE_KINDS: ReadonlySet<ArtifactKind> = new Set(["html", "slides", "design"]);

/** The filter an address names (`filter=`), Open where it names none. */
export function filterParam(raw: string): CommentFilter {
  return raw === "resolved" || raw === "outdated" ? raw : "open";
}

/** Which list a thread is under on the version on show: resolved, open and here, or open and gone. */
export function filterOf(t: ArtifactCommentThread): CommentFilter {
  if (t.status === "resolved") return "resolved";
  return t.placement === "outdated" ? "outdated" : "open";
}

/** The threads a filter lists: a thread made on a later version is not about this one. */
export function listed(
  threads: readonly ArtifactCommentThread[],
  filter: CommentFilter,
): ArtifactCommentThread[] {
  return threads.filter((t) => t.placement !== "later" && filterOf(t) === filter);
}

/** Whether a thread has a place on the version on show to pin, and which. */
export function placeOf(t: ArtifactCommentThread): ArtifactCommentAnchor | null {
  if (t.placement !== "here" && t.placement !== "moved") return null;
  const at = t.place ?? null;
  return at && at.kind !== "version" ? at : null;
}

/**
 * Whether a thread's pin is drawn: an open thread placed here, a resolved
 * one while Resolved is listed, and the one selected whatever it is.
 */
export function pinned(
  t: ArtifactCommentThread,
  filter: CommentFilter,
  selected: string | null,
): boolean {
  if (!placeOf(t)) return false;
  return t.status === "open" || filter === "resolved" || selected === t.id;
}

// ---- words ----------------------------------------------------------------

/** A name as words: `-`, `_` and `.` as spaces, its first letter a capital (`commentanchor.phrase`). */
export function phrase(name: string): string {
  const out = name
    .replace(/[-_.]+/g, " ")
    .trim()
    .replace(/\s+/g, " ");
  return out.charAt(0).toUpperCase() + out.slice(1);
}

/** How much of an element's text a label quotes (`commentanchor.MaxLabelQuoteRunes`). */
const LABEL_QUOTE = 60;

/** Where an anchor points, in the engine's own words: `slides` numbers a slide by the order on show. */
export function anchorLabel(a: ArtifactCommentAnchor, slides: readonly string[] = []): string {
  switch (a.kind) {
    case "html":
      return a.name ? `Section: ${phrase(a.name)}` : "A point on the page";
    case "slides": {
      const at = slides.indexOf(a.slide ?? "");
      const slide = at >= 0 ? `Slide ${at + 1}` : "A removed slide";
      if (!a.text) return slide;
      const runes = [...a.text];
      const quote =
        runes.length <= LABEL_QUOTE ? a.text : `${runes.slice(0, LABEL_QUOTE).join("").trim()}…`;
      return `${slide}: “${quote}”`;
    }
    case "design": {
      const base = (a.board ?? "").split("/").pop() ?? "";
      const name = base.replace(/\.html?$/i, "") || base;
      return `${phrase(name)} board, at ${a.x ?? 0}, ${a.y ?? 0}`;
    }
    default:
      return "The whole version";
  }
}

/** A thread's state beside its version: resolved in a version, gone from this one, or moved. */
export function stateWords(t: ArtifactCommentThread, showing: number): string | null {
  if (t.status === "resolved") {
    return t.resolved_version ? `Resolved in v${t.resolved_version}` : "Resolved";
  }
  if (t.placement === "outdated") {
    const a = t.anchor;
    switch (a.kind) {
      case "html":
        return a.name
          ? `Outdated: v${showing} has no ${phrase(a.name).toLowerCase()} section`
          : `Outdated: v${showing} has no page ${a.page ?? ""}`.trim();
      case "slides":
        // THE ENGINE'S LABEL SAYS WHICH: a slide it cannot number is gone.
        return t.label.startsWith("A removed slide")
          ? `Outdated: v${showing} has no such slide`
          : `Outdated: the slide in v${showing} no longer has this element`;
      case "design":
        return `Outdated: this point is on no board of v${showing}`;
      default:
        return `Outdated: not in v${showing}`;
    }
  }
  if (t.placement === "moved") return "Moved with its content";
  return null;
}

// ---- a slide's element ------------------------------------------------------

/**
 * An element's text as an anchor keeps it (`commentanchor.ElementText`):
 * every run beneath it joined, a line break as a space, whitespace collapsed,
 * at most `max` characters.
 */
export function elementText(n: SlideNode | null | undefined, max = 200): string {
  if (!n) return "";
  const parts: string[] = [];
  const gather = (node: SlideNode) => {
    if (node.tag === undefined) parts.push(node.text ?? "");
    else if (node.tag === "br") parts.push(" ");
    else for (const c of node.children ?? []) gather(c);
  };
  gather(n);
  const text = parts.join("").split(/\s+/).filter(Boolean).join(" ");
  return [...text].slice(0, max).join("");
}

// ---- where a pin stands ------------------------------------------------------

/** A box in some drawing's pixels. */
export interface Box {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** How far in from a part's top end corner its pin stands, so it sits on the part, clear of its edge. */
export const PIN_INSET = 32;

/** A part's pin: at its top end corner, inside it however small it is. */
export function partPin(box: Box): { x: number; y: number } {
  return {
    x: box.x + Math.max(box.w / 2, box.w - PIN_INSET),
    y: box.y + Math.min(PIN_INSET, box.h),
  };
}

/** A declared anchor's box in the frame's viewport, from the page's last layout. */
export function anchorBox(layout: CommentLayout, name: string): Box | null {
  const a: CommentAnchorBox | undefined = layout.anchors.find((b) => b.name === name);
  return a ? { x: a.x - layout.scroll.x, y: a.y - layout.scroll.y, w: a.w, h: a.h } : null;
}

/**
 * Where a page anchor's pin stands in the frame's viewport: on its section,
 * or at its point on the document. Null where the page does not draw it.
 */
export function pagePin(
  layout: CommentLayout,
  a: ArtifactCommentAnchor,
): { x: number; y: number } | null {
  if (a.kind !== "html") return null;
  if (a.name) {
    const box = anchorBox(layout, a.name);
    return box ? partPin(box) : null;
  }
  if (!a.point) return null;
  return {
    x: a.point.x * layout.doc.w - layout.scroll.x,
    y: a.point.y * layout.doc.h - layout.scroll.y,
  };
}

/** An artboard on the canvas, in canvas pixels; its own pixels are the canvas's from its corner. */
export interface BoardFrame {
  id: string;
  x: number;
  y: number;
  w: number;
  h: number;
}

/** A design anchor's point on the canvas, or null where its board is not drawn. */
export function boardWorld(
  boards: readonly BoardFrame[],
  a: ArtifactCommentAnchor,
): { x: number; y: number } | null {
  if (a.kind !== "design") return null;
  const b = boards.find((x) => x.id === a.board);
  return b ? { x: b.x + (a.x ?? 0), y: b.y + (a.y ?? 0) } : null;
}

/** The design anchor at a point on the canvas: the topmost board under it, in the board's own pixels. */
export function boardAnchorAt(
  boards: readonly BoardFrame[],
  world: { x: number; y: number },
): ArtifactCommentAnchor | null {
  for (let i = boards.length - 1; i >= 0; i--) {
    const b = boards[i]!;
    const x = Math.floor(world.x - b.x);
    const y = Math.floor(world.y - b.y);
    if (x >= 0 && y >= 0 && x < b.w && y < b.h) return { kind: "design", board: b.id, x, y };
  }
  return null;
}
