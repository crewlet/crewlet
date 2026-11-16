/**
 * The page's half of the comment layer (`crewlet.comments`): the declared
 * anchors' layout while the host asks, a click in Comment mode, and a picture
 * of a part. The page only reports what it draws; the host writes the comment.
 */

import {
  COMMENT_LAYOUT_MAX_ANCHORS,
  COMMENT_QUOTE_MAX_BYTES,
  isAnchorName,
  type CommentAnchorBox,
  type CommentLayout,
  type CommentMode,
  type CommentPoint,
  type CommentRequest,
  type CommentTarget,
} from "./bridge.ts";
import { drawRegion, padRegion, pointRegion, type CropRegion } from "./crop.ts";

/** The attribute a part is declared by. */
export const ANCHOR_ATTRIBUTE = "data-crewlet-anchor";

/** The calls this half makes, each answered by the host. */
export type CommentsMethod =
  "comments.layout" | "comments.pick" | "comments.crop" | "comments.escape";

/** What the page's comment layer is built over. */
export interface PageCommentsOptions {
  /** One call to the host; its answer carries nothing the page waits for. */
  call: (method: CommentsMethod, params: Record<string, unknown>) => Promise<unknown>;
  /** The refusal a page branches on (`CrewletError`), with its code. */
  refuse: (code: string, message: string) => Error;
  /** The page's window; the suite's own outside a frame. */
  window?: Window;
}

/** A string cut to at most `max` UTF-8 bytes, never inside a character. */
export function clipBytes(s: string, max: number): string {
  const encoder = new TextEncoder();
  if (encoder.encode(s).length <= max) return s;
  let out = "";
  let used = 0;
  for (const ch of s) {
    const n = encoder.encode(ch).length;
    if (used + n > max) break;
    out += ch;
    used += n;
  }
  return out;
}

/** An element's words as a quote keeps them: whitespace collapsed, at most 1 KiB. */
export function quoteOf(el: Element): string {
  const text = (el as HTMLElement).innerText ?? el.textContent ?? "";
  return clipBytes(text.replace(/\s+/g, " ").trim(), COMMENT_QUOTE_MAX_BYTES);
}

/** The innermost declared anchor at or around `target`, skipping a name the grammar refuses. */
export function anchorAround(target: EventTarget | null): Element | null {
  let at = target instanceof Element ? target : null;
  while (at) {
    const found: Element | null = at.closest(`[${ANCHOR_ATTRIBUTE}]`);
    if (!found) return null;
    if (isAnchorName(found.getAttribute(ANCHOR_ATTRIBUTE))) return found;
    at = found.parentElement;
  }
  return null;
}

/** A point on the document as fractions of its size, held to 0..1. */
export function fractionOf(
  page: { x: number; y: number },
  doc: { w: number; h: number },
): CommentPoint {
  const clamp = (v: number) => Math.min(1, Math.max(0, v));
  return { x: clamp(page.x / doc.w), y: clamp(page.y / doc.h) };
}

export class PageComments {
  private mode: CommentMode = "off";
  private hover: string | null = null;
  private queued = false;
  private cursor: string | null = null;
  private stops: (() => void)[] = [];
  private readonly win: Window;

  constructor(private readonly options: PageCommentsOptions) {
    this.win = options.window ?? window;
  }

  private get doc(): Document {
    return this.win.document;
  }

  /** `crewlet.comments.anchor`: declares `element` as the part named `name`, until the returned call. */
  anchor(name: string, element: Element): () => void {
    if (!isAnchorName(name)) {
      throw this.options.refuse(
        "invalid",
        "comments.anchor takes a name of 1 to 64 lowercase letters, digits, `.`, `_` and `-`, starting with a letter or a digit",
      );
    }
    const Own = (this.win as Window & { Element?: typeof Element }).Element ?? Element;
    if (!(element instanceof Own)) {
      throw this.options.refuse("invalid", "comments.anchor takes the element the name declares");
    }
    element.setAttribute(ANCHOR_ATTRIBUTE, name);
    this.schedule();
    return () => {
      if (element.getAttribute(ANCHOR_ATTRIBUTE) === name)
        element.removeAttribute(ANCHOR_ATTRIBUTE);
      this.schedule();
    };
  }

  /** What the host asked on the `comments` topic. */
  receive(request: CommentRequest): void {
    if ("mode" in request) this.setMode(request.mode);
    else if ("reveal" in request) this.reveal(request.reveal);
    else void this.capture(request.capture.id, request.capture);
  }

  /** A key the page heard: Escape while placing tells the host, which stops. */
  onKey(event: KeyboardEvent): void {
    if (this.mode !== "place" || event.key !== "Escape") return;
    if (event.metaKey || event.ctrlKey || event.defaultPrevented) return;
    event.preventDefault();
    this.send("comments.escape", {});
  }

  /** The declared anchors, the document's size and how much of it is in view. */
  layout(): CommentLayout {
    const { w, h } = this.size();
    const anchors: CommentAnchorBox[] = [];
    for (const el of this.doc.querySelectorAll(`[${ANCHOR_ATTRIBUTE}]`)) {
      const name = el.getAttribute(ANCHOR_ATTRIBUTE);
      if (!isAnchorName(name)) continue;
      const r = el.getBoundingClientRect();
      anchors.push({
        name,
        x: r.left + this.win.scrollX,
        y: r.top + this.win.scrollY,
        w: r.width,
        h: r.height,
      });
      if (anchors.length === COMMENT_LAYOUT_MAX_ANCHORS) break;
    }
    return {
      anchors,
      doc: { w, h },
      scroll: { x: this.win.scrollX, y: this.win.scrollY },
      viewport: { w: this.win.innerWidth, h: this.win.innerHeight },
      ...(this.mode === "place" && this.hover ? { hover: this.hover } : {}),
    };
  }

  private size(): { w: number; h: number } {
    const de = this.doc.documentElement;
    return { w: Math.max(de.scrollWidth, 1), h: Math.max(de.scrollHeight, 1) };
  }

  private send(method: CommentsMethod, params: Record<string, unknown>): void {
    this.options.call(method, params).then(
      () => undefined,
      () => undefined,
    );
  }

  /** One layout on the next frame, however many changes asked for it. */
  private schedule(): void {
    if (this.mode === "off" || this.queued) return;
    this.queued = true;
    this.win.requestAnimationFrame(() => {
      this.queued = false;
      if (this.mode !== "off") this.send("comments.layout", { ...this.layout() });
    });
  }

  private setMode(mode: CommentMode): void {
    if (mode === this.mode) return;
    const was = this.mode;
    this.mode = mode;
    this.hover = null;
    if (was === "off") this.listen();
    if (mode === "off") this.unlisten();
    const root = this.doc.documentElement;
    if (mode === "place") {
      this.cursor = root.style.cursor;
      root.style.cursor = "crosshair";
    } else if (was === "place") {
      root.style.cursor = this.cursor ?? "";
      this.cursor = null;
    }
    if (mode !== "off") this.send("comments.layout", { ...this.layout() });
  }

  /** What moves an anchor: a scroll, a resize, a reflow, an anchor declared or let go. */
  private listen(): void {
    const win = this.win;
    const schedule = () => this.schedule();
    win.addEventListener("scroll", schedule, { passive: true });
    win.addEventListener("resize", schedule);
    this.stops.push(() => win.removeEventListener("scroll", schedule));
    this.stops.push(() => win.removeEventListener("resize", schedule));
    const Resize = (win as Window & { ResizeObserver?: typeof ResizeObserver }).ResizeObserver;
    if (typeof Resize === "function") {
      const resize = new Resize(schedule);
      resize.observe(this.doc.documentElement);
      this.stops.push(() => resize.disconnect());
    }
    const Mutation = (win as Window & { MutationObserver?: typeof MutationObserver })
      .MutationObserver;
    if (typeof Mutation === "function") {
      const mutation = new Mutation(schedule);
      mutation.observe(this.doc.documentElement, {
        subtree: true,
        childList: true,
        attributes: true,
        attributeFilter: [ANCHOR_ATTRIBUTE],
      });
      this.stops.push(() => mutation.disconnect());
    }
    // A CLICK WHILE PLACING IS THE READER'S PICK, heard before the page's own
    // handlers and kept from them, so placing a comment on a button or a link
    // never presses it.
    const click = (event: MouseEvent) => {
      if (this.mode !== "place") return;
      event.preventDefault();
      event.stopPropagation();
      this.pick(event);
    };
    const move = (event: PointerEvent) => {
      if (this.mode !== "place") return;
      const name = anchorAround(event.target)?.getAttribute(ANCHOR_ATTRIBUTE) ?? null;
      if (name === this.hover) return;
      this.hover = name;
      this.schedule();
    };
    win.addEventListener("click", click, true);
    win.addEventListener("pointermove", move, { passive: true });
    this.stops.push(() => win.removeEventListener("click", click, true));
    this.stops.push(() => win.removeEventListener("pointermove", move));
  }

  private unlisten(): void {
    for (const stop of this.stops.splice(0)) stop();
    this.queued = false;
  }

  private pick(event: MouseEvent): void {
    const el = anchorAround(event.target);
    this.send("comments.pick", {
      name: el?.getAttribute(ANCHOR_ATTRIBUTE) ?? null,
      quote: el ? quoteOf(el) : "",
      point: fractionOf({ x: event.pageX, y: event.pageY }, this.size()),
    });
  }

  private element(name: string): Element | null {
    return this.doc.querySelector(`[${ANCHOR_ATTRIBUTE}="${name}"]`);
  }

  private reveal(target: CommentTarget): void {
    const smooth = !this.win.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    const behavior: ScrollBehavior = smooth ? "smooth" : "auto";
    if ("name" in target) {
      this.element(target.name)?.scrollIntoView({ block: "center", behavior });
      return;
    }
    const { w, h } = this.size();
    this.win.scrollTo({
      left: target.point.x * w - this.win.innerWidth / 2,
      top: target.point.y * h - this.win.innerHeight / 2,
      behavior,
    });
  }

  /** The region a request names, in the document's own pixels. */
  regionOf(target: CommentTarget): CropRegion {
    const size = this.size();
    if ("name" in target) {
      const el = this.element(target.name);
      if (el) {
        const r = el.getBoundingClientRect();
        return padRegion(
          { x: r.left + this.win.scrollX, y: r.top + this.win.scrollY, w: r.width, h: r.height },
          size,
        );
      }
    }
    return pointRegion("point" in target ? target.point : { x: 0.5, y: 0.5 }, size);
  }

  private async capture(id: number, target: CommentTarget): Promise<void> {
    const body = this.doc.body;
    const ground = body ? this.win.getComputedStyle(body).backgroundColor : "";
    const image = await drawRegion(
      this.doc.documentElement,
      this.size(),
      this.regionOf(target),
      ground && ground !== "rgba(0, 0, 0, 0)" && ground !== "transparent" ? ground : "#ffffff",
    );
    this.send("comments.crop", { id, image });
  }
}
