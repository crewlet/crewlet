/**
 * The copy a comment's picture is drawn from. What crosses the picture's rows
 * is copied whole, computed styles inline; every other block stays as an empty
 * box of its own size and margins, so the copy lays out where the page does.
 */

import type { CropRegion, Point } from "./crop.ts";

const XHTML = "http://www.w3.org/1999/xhtml";
const SVG = "http://www.w3.org/2000/svg";

/** How far past the picture's rows a box still counts as in it: shadows and outlines paint outside their box. */
export const BAND_SLACK = 48;

/** Box properties a copy always states: resolved pixels, or a UA default given in em. */
const STATED = new Set([
  "display",
  "position",
  "width",
  "height",
  "font-size",
  "margin-top",
  "margin-right",
  "margin-bottom",
  "margin-left",
  "padding-top",
  "padding-right",
  "padding-bottom",
  "padding-left",
]);

/** What a still picture never shows: motion would restart in the copy, and a pointer has nowhere to be. */
const MOVES = /^(--|animation|transition|cursor|pointer-events|will-change|caret|user-select)/;

/** What an empty stand-in for a box keeps: only what places it and its neighbours. */
const SHELL = [
  "display",
  "position",
  "top",
  "right",
  "bottom",
  "left",
  "float",
  "clear",
  "box-sizing",
  "width",
  "height",
  "min-width",
  "min-height",
  "max-width",
  "max-height",
  "margin-left",
  "margin-right",
  "padding-top",
  "padding-right",
  "padding-bottom",
  "padding-left",
  "border-top-width",
  "border-right-width",
  "border-bottom-width",
  "border-left-width",
  "border-top-style",
  "border-right-style",
  "border-bottom-style",
  "border-left-style",
  "overflow-x",
  "overflow-y",
  "contain",
  "zoom",
  "order",
  "align-self",
  "justify-self",
  "grid-row-start",
  "grid-row-end",
  "grid-column-start",
  "grid-column-end",
  "column-span",
  "writing-mode",
  "direction",
  "anchor-name",
] as const;

/** Elements that run, load or describe the page rather than draw it. */
const SKIP = new Set([
  "script",
  "style",
  "link",
  "meta",
  "title",
  "base",
  "noscript",
  "template",
  "iframe",
  "object",
  "embed",
  "source",
  "track",
]);

/** Controls that scroll their own text, which no wrapper can hold. */
const UNWRAPPED = new Set(["textarea", "select", "input"]);

/** A scrolled element's properties that lay out its children, which the wrapper in its copy takes over. */
const LAYS_OUT = [
  "flex-direction",
  "flex-wrap",
  "justify-content",
  "justify-items",
  "align-items",
  "align-content",
  "row-gap",
  "column-gap",
  "grid-template-columns",
  "grid-template-rows",
  "grid-template-areas",
  "grid-auto-columns",
  "grid-auto-rows",
  "grid-auto-flow",
  "column-count",
  "column-width",
  "column-fill",
  "column-rule-width",
  "column-rule-style",
  "column-rule-color",
  "-webkit-box-orient",
  "-webkit-line-clamp",
  "padding-top",
  "padding-right",
  "padding-bottom",
  "padding-left",
] as const;

/** How a scrolled element's copy is displayed: a box that clips, around a wrapper laying out its children. */
export function scrollerDisplays(display: string): { outer: string; inner: string } {
  const inner = display.endsWith("flex")
    ? "flex"
    : display.endsWith("grid")
      ? "grid"
      : display.endsWith("box")
        ? "-webkit-box"
        : "flow-root";
  if (inner === "flow-root") return { outer: display, inner };
  const inline = display.startsWith("inline-") || display === "-webkit-inline-box";
  return { outer: inline ? "inline-block" : "block", inner };
}

/** Where a box fixed at `at` in the viewport goes in a copy whose box holding it has its corner at `holder`. */
export function fixedAt(at: Point, holder: Point, scale = 1): Point {
  return { x: (at.x - holder.x) / scale, y: (at.y - holder.y) / scale };
}

/** How far a sticky box is shifted from where it flows, less what a sticky box around it shifts it. */
export function stuckBy(shown: Point, flows: Point, around: Point = { x: 0, y: 0 }): Point {
  return { x: shown.x - flows.x - around.x, y: shown.y - flows.y - around.y };
}

/** Adjoining margins as one (CSS 2.1 8.3.1): the largest positive plus the most negative. */
export function collapsed(...margins: number[]): number {
  return Math.max(0, ...margins) + Math.min(0, ...margins);
}

/** Whether a box spanning `top`..`bottom` reaches into the rows `band` covers. */
export function inBand(
  top: number,
  bottom: number,
  band: { top: number; bottom: number },
): boolean {
  return bottom >= band.top && top <= band.bottom;
}

/**
 * The properties a copy states for one element: those differing from what it
 * would inherit or from its tag's default, since either difference shows.
 */
export function stated(
  names: readonly string[],
  own: readonly string[],
  parent: readonly string[] | null,
  fallback: readonly string[],
  important = false,
): string {
  const end = important ? " !important;" : ";";
  let text = "";
  for (let i = 0; i < names.length; i++) {
    const name = names[i]!;
    const value = own[i]!;
    if (value === "" || MOVES.test(name)) continue;
    if (!STATED.has(name) && parent?.[i] === value && fallback[i] === value) continue;
    text += `${name}:${value}${end}`;
  }
  return text;
}

/** A resource the copy needs drawn in once it is built. */
export interface PixelJob {
  /** The live element whose pixels are drawn: an image, a canvas, a video or an image button. */
  live: HTMLImageElement | HTMLCanvasElement | HTMLVideoElement | HTMLInputElement;
  /** The copy's image that shows them. */
  out: HTMLImageElement;
  /** The content box, in the drawing's pixels. */
  content: CropRegion;
  /** The content box in the element's own pixels, which its copy is laid out in. */
  layout: { w: number; h: number };
  /** Its border box's own size, and its padding, top right bottom left. */
  border: { w: number; h: number };
  padding: [number, number, number, number];
  fit: string;
  position: string;
  /** Rotated or skewed on the page, so only its whole content box can be drawn. */
  turned: boolean;
}

/** An SVG image or use naming a file. */
export interface HrefJob {
  out: SVGElement;
  url: string;
  kind: "image" | "use";
  /** Its size in the drawing's pixels, which a picture need not exceed. */
  size: { w: number; h: number };
}

/** The copy, and what still has to be drawn into it. */
export interface Copied {
  copy: Element;
  pixels: PixelJob[];
  hrefs: HrefJob[];
  /** Elements whose styles name a URL, and pseudo-element rules that do, as written. */
  urls: ({ out: Element } | { selector: string; text: string })[];
  /** Pseudo-element rules, keyed by the attribute the copy gives their element. */
  rules: string[];
  /** Every font family the copied text asks for. */
  families: Set<string>;
  /** How many live elements the copy read in full, and how many it kept as empty boxes. */
  counts: { full: number; shells: number };
}

/** The attribute that ties a copied element to its pseudo-element rules. */
export const PSEUDO_ATTRIBUTE = "data-crewlet-crop";

/** Each document's tag defaults, kept for its next picture. */
const known = new WeakMap<Document, Map<string, string[]>>();

/** Each tag's computed defaults, read once in a shadow root no page rule reaches. */
class Defaults {
  private readonly cache: Map<string, string[]>;
  private probe: { host: HTMLElement; root: ShadowRoot; wrap: Element } | null = null;

  constructor(
    private readonly doc: Document,
    private readonly names: readonly string[],
  ) {
    let cache = known.get(doc);
    if (!cache) {
      cache = new Map();
      known.set(doc, cache);
    }
    this.cache = cache;
  }

  of(el: Element, pseudo?: string): readonly string[] {
    const key = `${el.namespaceURI}|${el.localName}|${pseudo ?? ""}|${this.names.length}`;
    let values = this.cache.get(key);
    if (!values) {
      values = this.read(el, pseudo);
      this.cache.set(key, values);
    }
    return values;
  }

  private read(el: Element, pseudo?: string): string[] {
    const win = this.doc.defaultView!;
    try {
      const probe = this.open();
      const made = this.doc.createElementNS(el.namespaceURI ?? XHTML, el.localName);
      const outer = el.namespaceURI === SVG ? this.doc.createElementNS(SVG, "svg") : made;
      if (outer !== made) outer.append(made);
      if (pseudo) made.setAttribute("class", "p");
      probe.wrap.append(outer);
      const style = win.getComputedStyle(made, pseudo);
      // LEFT IN until the copy is done, so each read restyles only the probe it adds.
      return this.names.map((n) => style.getPropertyValue(n));
    } catch {
      return this.names.map(() => "");
    }
  }

  private open() {
    if (this.probe) return this.probe;
    const host = this.doc.createElement("crewlet-crop-probe");
    host.style.cssText =
      "position:fixed;left:0;top:0;width:0;height:0;overflow:hidden;visibility:hidden";
    const root = host.attachShadow({ mode: "closed" });
    const sheet = this.doc.createElement("style");
    sheet.textContent = ".p::before,.p::after{content:''}";
    const wrap = this.doc.createElement("div");
    wrap.style.cssText = "all:initial";
    root.append(sheet, wrap);
    (this.doc.body ?? this.doc.documentElement).append(host);
    this.probe = { host, root, wrap };
    return this.probe;
  }

  close(): void {
    this.probe?.host.remove();
    this.probe = null;
  }
}

/** The child nodes an element draws: its shadow tree's where it has one, a slot's assigned nodes. */
function drawn(el: Element): ArrayLike<Node> {
  if (el.shadowRoot) return el.shadowRoot.childNodes;
  if (el.localName === "slot" && "assignedNodes" in el) {
    const assigned = (el as HTMLSlotElement).assignedNodes();
    if (assigned.length > 0) return assigned;
  }
  return el.childNodes;
}

interface Live {
  el: Element;
  style: CSSStyleDeclaration;
  values: string[];
}

/**
 * Copies `root` for a picture of `region`, in its untransformed pixels: every
 * element reaching into the region's rows in full, every other block as an
 * empty box that places its neighbours as it did.
 */
export function copyRegion(root: Element, region: CropRegion): Copied {
  const doc = root.ownerDocument;
  const win = (doc.defaultView ?? window) as Window & typeof globalThis;
  const first = win.getComputedStyle(root);
  const names: string[] = [];
  for (let i = 0; i < first.length; i++) names.push(first[i]!);
  const defaults = new Defaults(doc, names);
  try {
    return new Copier(root, region, win, names, defaults).run();
  } finally {
    defaults.close();
  }
}

class Copier {
  private readonly doc: Document;
  /** Where the copy is built: a document with no window, so nothing in it loads, runs or upgrades. */
  private readonly inert: Document;
  private readonly band: { top: number; bottom: number };
  private readonly scale: number;
  private readonly corner: Point;
  private readonly reaches = new Map<Element, boolean>();
  private readonly twin = new Map<Element, Element>();
  private readonly live = new Map<Element, Live>();
  private readonly scrolled: Live[] = [];
  private readonly placed: { el: HTMLElement; viewport: Point | null }[] = [];
  private readonly sticky: HTMLElement[] = [];
  private readonly out: Copied;
  private pseudoCount = 0;

  constructor(
    private readonly root: Element,
    region: CropRegion,
    private readonly win: Window & typeof globalThis,
    private readonly names: readonly string[],
    private readonly defaults: Defaults,
  ) {
    this.doc = root.ownerDocument;
    this.inert = this.doc.implementation.createHTMLDocument("");
    const bounds = root.getBoundingClientRect();
    const isDoc = root === this.doc.documentElement;
    this.scale =
      isDoc || !(root instanceof win.HTMLElement) || !root.offsetWidth
        ? 1
        : bounds.width / root.offsetWidth;
    this.corner = isDoc ? { x: -win.scrollX, y: -win.scrollY } : { x: bounds.left, y: bounds.top };
    this.band = { top: region.y - BAND_SLACK, bottom: region.y + region.h + BAND_SLACK };
    this.out = {
      copy: root,
      pixels: [],
      hrefs: [],
      urls: [],
      rules: [],
      families: new Set(),
      counts: { full: 0, shells: 0 },
    };
  }

  /** A viewport rectangle in the drawing's own pixels. */
  private local(r: DOMRect): CropRegion {
    return {
      x: (r.left - this.corner.x) / this.scale,
      y: (r.top - this.corner.y) / this.scale,
      w: r.width / this.scale,
      h: r.height / this.scale,
    };
  }

  run(): Copied {
    this.reach(this.root);
    const copy = this.full(this.root, null)!;
    this.out.copy = copy;
    this.restick();
    this.replace(copy);
    for (const s of this.scrolled)
      wrapScroll(s.el as HTMLElement, s.style, this.twin.get(s.el) as HTMLElement);
    return this.out;
  }

  /** Marks every element whose box, or a box inside it, reaches the band; returns its rows. */
  private reach(el: Element): { top: number; bottom: number } | null {
    let rows: { top: number; bottom: number } | null = null;
    const r = el.getBoundingClientRect();
    if (r.width > 0 || r.height > 0) {
      const box = this.local(r);
      rows = { top: box.y, bottom: box.y + box.h };
    }
    if (el.namespaceURI !== SVG) {
      const nodes = drawn(el);
      for (let i = 0; i < nodes.length; i++) {
        const child = nodes[i]!;
        if (child.nodeType !== 1) continue;
        const inner = this.reach(child as Element);
        if (!inner) continue;
        rows = rows
          ? { top: Math.min(rows.top, inner.top), bottom: Math.max(rows.bottom, inner.bottom) }
          : inner;
      }
    }
    this.reaches.set(el, rows !== null && inBand(rows.top, rows.bottom, this.band));
    return rows;
  }

  private values(style: CSSStyleDeclaration): string[] {
    return this.names.map((n) => style.getPropertyValue(n));
  }

  /** `el` copied with its styles and, as the band asks, its children. */
  private full(el: Element, parent: Live | null): Element | null {
    if (SKIP.has(el.localName)) return null;
    const style = this.win.getComputedStyle(el);
    const me: Live = { el, style, values: this.values(style) };
    this.live.set(el, me);
    this.out.counts.full++;
    if (el.namespaceURI === SVG) return this.svg(el, me, parent);
    const out = this.made(el);
    if (style.display === "none") {
      out.setAttribute("style", "display:none");
      return out;
    }
    let text = stated(this.names, me.values, parent?.values ?? null, this.defaults.of(el));
    if (el instanceof this.win.HTMLElement && el !== this.root) {
      const below = this.bottomMargin(el, style);
      if (below !== null) text += `margin-bottom:${below}px;`;
    }
    this.styled(out, text);
    this.family(style);
    this.pseudos(el, me, out);
    this.track(el, me);
    if (out.localName === "img" && out !== el) this.pixelJob(el, me, out as HTMLImageElement);
    const nodes = drawn(el);
    for (let i = 0; i < nodes.length; i++) {
      const node = nodes[i]!;
      if (node.nodeType === 3) {
        out.append(this.inert.importNode(node, false));
        continue;
      }
      if (node.nodeType !== 1) continue;
      const child = node as Element;
      if (child.localName === "source" || SKIP.has(child.localName)) continue;
      const kept =
        this.reaches.get(child) || !this.shellable(child, style)
          ? this.full(child, me)
          : this.shell(child as HTMLElement, me);
      if (kept) out.append(kept);
    }
    return out;
  }

  /** The element's own copy: an image for anything whose pixels are drawn in, else a shallow clone. */
  private made(el: Element): Element {
    const tag = el.localName;
    const drawsPixels =
      tag === "img" ||
      tag === "canvas" ||
      tag === "video" ||
      (tag === "input" && (el as HTMLInputElement).type === "image");
    if (!drawsPixels) {
      const out = this.inert.importNode(el, false);
      tidy(out);
      formState(el, out);
      this.twin.set(el, out);
      return out;
    }
    const img = this.inert.createElementNS(XHTML, "img") as HTMLImageElement;
    for (const a of ["id", "class"]) {
      const v = el.getAttribute(a);
      if (v !== null) img.setAttribute(a, v);
    }
    this.twin.set(el, img);
    return img;
  }

  private styled(out: Element, text: string): void {
    out.setAttribute("style", text);
    if (text.includes("url(")) this.out.urls.push({ out });
  }

  private family(style: CSSStyleDeclaration): void {
    for (const f of style.fontFamily.split(",")) {
      const name = f.trim().replace(/^["']|["']$/g, "");
      if (name) this.out.families.add(name.toLowerCase());
    }
  }

  /** `::before` and `::after` as rules on the copy, since no page rule is copied. */
  private pseudos(el: Element, me: Live, out: Element): void {
    for (const which of ["::before", "::after"] as const) {
      const ps = this.win.getComputedStyle(el, which);
      const content = ps.content;
      if (!content || content === "none" || content === "normal") continue;
      let id = out.getAttribute(PSEUDO_ATTRIBUTE);
      if (!id) {
        id = String(++this.pseudoCount);
        out.setAttribute(PSEUDO_ATTRIBUTE, id);
      }
      const text = stated(
        this.names,
        this.values(ps),
        me.values,
        this.defaults.of(el, which),
        true,
      );
      const selector = `[${PSEUDO_ATTRIBUTE}="${id}"]${which}`;
      if (text.includes("url(")) this.out.urls.push({ selector, text });
      else this.out.rules.push(`${selector}{${text}}`);
    }
  }

  /** Remembers what a clone loses: the scroll, where a fixed box is, how far a sticky box is stuck. */
  private track(el: Element, me: Live): void {
    if (el === this.root || !(el instanceof this.win.HTMLElement)) return;
    const { style } = me;
    if (style.position === "sticky") this.sticky.push(el);
    else if (style.position === "fixed" || style.position === "absolute") {
      // FIXED TO THE VIEWPORT when no ancestor holds it, which offsetParent says.
      const viewport = style.position === "fixed" && el.offsetParent === null;
      this.placed.push({
        el,
        viewport: viewport ? { x: parseFloat(style.left), y: parseFloat(style.top) } : null,
      });
    }
    if (
      (el.scrollTop !== 0 || el.scrollLeft !== 0) &&
      el !== this.doc.scrollingElement &&
      !UNWRAPPED.has(el.localName)
    ) {
      this.scrolled.push(me);
    }
  }

  /** An inline SVG copied whole, each part's styles inline. */
  private svg(el: Element, me: Live, parent: Live | null): Element {
    const out = this.inert.importNode(el, true);
    this.twin.set(el, out);
    const from = [el, ...el.querySelectorAll("*")];
    const to = [out, ...out.querySelectorAll("*")];
    for (const part of to) tidy(part);
    const values = new Map<Element, string[]>([[el, me.values]]);
    from.forEach((part, i) => {
      const copy = to[i]!;
      if (part.localName === "style" || part.localName === "script") {
        copy.remove();
        return;
      }
      const own = part === el ? me.values : this.values(this.win.getComputedStyle(part));
      values.set(part, own);
      const above =
        part === el ? (parent?.values ?? null) : (values.get(part.parentElement!) ?? null);
      this.styled(copy, stated(this.names, own, above, this.defaults.of(part)));
      const href =
        part.getAttribute("href") ?? part.getAttributeNS("http://www.w3.org/1999/xlink", "href");
      if (
        (part.localName === "image" || part.localName === "use") &&
        href &&
        !href.startsWith("#")
      ) {
        const box = this.local(part.getBoundingClientRect());
        this.out.hrefs.push({
          out: copy as SVGElement,
          url: new URL(href, part.baseURI).href,
          kind: part.localName,
          size: { w: box.w, h: box.h },
        });
      }
    });
    return out;
  }

  /** Whether an element out of the band can stand as an empty box without moving anything that is in it. */
  private shellable(el: Element, parentStyle: CSSStyleDeclaration): boolean {
    if (!(el instanceof this.win.HTMLElement)) {
      // AN SVG HOLDING ANYTHING NAMED may hold what an SVG in the band refers to.
      return (
        el.localName === "svg" && !el.querySelector("[id]") && blockLevel(el, this.win, parentStyle)
      );
    }
    return blockLevel(el, this.win, parentStyle);
  }

  /** An empty box standing for `el`: its size, its place and the margins it and its children had. */
  private shell(el: HTMLElement, parent: Live): Element {
    const style = this.win.getComputedStyle(el);
    this.out.counts.shells++;
    const out = this.inert.createElementNS(XHTML, el.localName === "svg" ? "div" : el.localName);
    let text = "";
    for (const name of SHELL) {
      const v = style.getPropertyValue(name);
      if (v) text += `${name}:${v};`;
    }
    text += `margin-top:${topMargin(el, style, this.win)}px;`;
    text += `margin-bottom:${this.bottomMargin(el, style) ?? parseFloat(style.marginBottom)}px;`;
    if (parent.style.display.endsWith("flex")) text += "flex:none;";
    // NOTHING OF IT PAINTS, so one placed where its page put it cannot draw into the picture.
    out.setAttribute("style", `${text}visibility:hidden;`);
    return out;
  }

  /** A block's bottom margin with its last child's collapsed into it, which an explicit height stops in the copy. */
  private bottomMargin(el: HTMLElement, style: CSSStyleDeclaration): number | null {
    const own = parseFloat(style.marginBottom) || 0;
    if (!lets(style, "bottom") || !heightAuto(el)) return null;
    const last = edgeChild(el, this.win, "last");
    if (!last) return null;
    if (Math.abs(last.el.getBoundingClientRect().bottom - el.getBoundingClientRect().bottom) > 0.5)
      return null;
    const inner =
      this.bottomMargin(last.el, last.style) ?? (parseFloat(last.style.marginBottom) || 0);
    return collapsed(own, inner);
  }

  /** Sticky boxes put where they are stuck, measured with every stick let go for a moment. */
  private restick(): void {
    for (const [el, shift] of stickShifts(this.sticky, this.win)) {
      const out = this.twin.get(el) as HTMLElement;
      out.style.setProperty("position", "relative");
      out.style.setProperty("top", `${shift.y / this.scale}px`);
      out.style.setProperty("left", `${shift.x / this.scale}px`);
      out.style.setProperty("bottom", "auto");
      out.style.setProperty("right", "auto");
    }
  }

  /**
   * A POSITIONED BOX THAT ITS SCROLLER DOES NOT CARRY leaves the wrapper that
   * stands for the scroll, for the box that holds it on the page; one leaving
   * its parent states every style, since it no longer inherits them.
   */
  private replace(copy: Element): void {
    const wrapped = new Set<Element>(this.scrolled.map((s) => s.el));
    const scrollerAround = (el: Element): Element | null => {
      for (let at = el.parentElement; at && at !== this.root; at = at.parentElement) {
        if (wrapped.has(at)) return at;
      }
      return null;
    };
    const restate = (el: Element, out: HTMLElement) => {
      const me = this.live.get(el)!;
      const keep = out.style.cssText;
      out.setAttribute("style", stated(this.names, me.values, null, this.defaults.of(el)) + keep);
    };
    for (const { el, viewport } of this.placed) {
      const out = this.twin.get(el) as HTMLElement;
      const around = scrollerAround(el);
      if (viewport) {
        const at = fixedAt(viewport, this.corner, this.scale);
        out.style.setProperty("top", `${at.y}px`);
        out.style.setProperty("left", `${at.x}px`);
        out.style.setProperty("bottom", "auto");
        out.style.setProperty("right", "auto");
        if (around) {
          restate(el, out);
          copy.append(out);
        }
        continue;
      }
      const holder = el.offsetParent;
      if (around && (!holder || !around.contains(holder))) {
        restate(el, out);
        ((holder ? this.twin.get(holder) : null) ?? copy).append(out);
      }
    }
  }

  /** An image, canvas, video or image button whose visible part is drawn in once the copy is built. */
  private pixelJob(el: Element, me: Live, out: HTMLImageElement): void {
    const s = me.style;
    const px = (n: string) => parseFloat(s.getPropertyValue(n)) || 0;
    const padding: [number, number, number, number] = [
      px("padding-top"),
      px("padding-right"),
      px("padding-bottom"),
      px("padding-left"),
    ];
    const borders = [
      px("border-top-width"),
      px("border-right-width"),
      px("border-bottom-width"),
      px("border-left-width"),
    ];
    const across = padding[1] + padding[3] + borders[1]! + borders[3]!;
    const down = padding[0] + padding[2] + borders[0]! + borders[2]!;
    const sized = s.boxSizing === "border-box";
    const layout = {
      w: Math.max(0, px("width") - (sized ? across : 0)),
      h: Math.max(0, px("height") - (sized ? down : 0)),
    };
    const border = { w: layout.w + across, h: layout.h + down };
    const box = this.local(el.getBoundingClientRect());
    const kx = border.w ? box.w / border.w : 1;
    const ky = border.h ? box.h / border.h : 1;
    const turned = border.w > 0 && border.h > 0 && Math.abs(kx - ky) > 0.01 * Math.max(kx, ky);
    this.out.pixels.push({
      live: el as PixelJob["live"],
      out,
      content: {
        x: box.x + (borders[3]! + padding[3]) * kx,
        y: box.y + (borders[0]! + padding[0]) * ky,
        w: layout.w * kx,
        h: layout.h * ky,
      },
      layout,
      border,
      padding,
      fit: el.localName === "video" ? s.objectFit || "contain" : s.objectFit || "fill",
      position: s.objectPosition || "50% 50%",
      turned,
    });
  }
}

/** Whether `el` is laid out as a block by its parent, so an empty box of its size stands for it. */
function blockLevel(
  el: Element,
  win: Window & typeof globalThis,
  parentStyle: CSSStyleDeclaration,
): boolean {
  const style = win.getComputedStyle(el);
  const display = style.display;
  if (display === "none") return true;
  if (display === "contents" || display.startsWith("table-") || display === "table-caption")
    return false;
  const parent = parentStyle.display;
  // BASELINES: an empty box has none of its own, so a parent aligning on them would move it.
  if (/flex|grid/.test(parent)) {
    return !/baseline/.test(style.alignSelf) && !/baseline/.test(parentStyle.alignItems);
  }
  if (style.position === "absolute" || style.position === "fixed" || style.float !== "none")
    return true;
  if (parent === "contents" || parent.startsWith("inline") || parent === "ruby") return false;
  return !display.startsWith("inline") && display !== "ruby" && display !== "run-in";
}

/** Whether a block lets a child's margin collapse through its `side`: no border, padding or own formatting context there. */
function lets(style: CSSStyleDeclaration, side: "top" | "bottom"): boolean {
  if (style.display !== "block" && style.display !== "list-item") return false;
  if (style.float !== "none" || style.position === "absolute" || style.position === "fixed")
    return false;
  if (style.overflowX !== "visible" && style.overflowX !== "clip") return false;
  if (style.overflowY !== "visible" && style.overflowY !== "clip") return false;
  if (/layout|paint|strict|content/.test(style.contain)) return false;
  if (style.columnCount !== "auto" || style.columnWidth !== "auto") return false;
  return (
    (parseFloat(style.getPropertyValue(`border-${side}-width`)) || 0) === 0 &&
    (parseFloat(style.getPropertyValue(`padding-${side}`)) || 0) === 0
  );
}

/** Whether a block's height came from its content, which is when its last child's margin may pass through it. */
function heightAuto(el: Element): boolean {
  const map = (el as Element & { computedStyleMap?: () => StylePropertyMapReadOnly })
    .computedStyleMap;
  if (typeof map !== "function") return true;
  try {
    const h = map.call(el).get("height");
    return !h || h.toString() === "auto";
  } catch {
    return true;
  }
}

/** A block's first or last in-flow child, where it is a block whose margin can adjoin the parent's. */
function edgeChild(
  el: Element,
  win: Window & typeof globalThis,
  which: "first" | "last",
): { el: HTMLElement; style: CSSStyleDeclaration } | null {
  const nodes = Array.from(drawn(el));
  if (which === "last") nodes.reverse();
  for (const node of nodes) {
    if (node.nodeType === 3) {
      if (/\S/.test(node.textContent ?? "")) return null;
      continue;
    }
    if (node.nodeType !== 1) continue;
    const child = node as Element;
    if (!(child instanceof win.HTMLElement)) return null;
    const style = win.getComputedStyle(child);
    if (style.display === "none" || style.position === "absolute" || style.position === "fixed")
      continue;
    if (style.float !== "none") continue;
    if (!["block", "list-item", "flow-root", "flex", "grid", "table"].includes(style.display))
      return null;
    return { el: child, style };
  }
  return null;
}

/** A block's top margin with its first child's collapsed into it, which an empty box no longer has. */
function topMargin(
  el: HTMLElement,
  style: CSSStyleDeclaration,
  win: Window & typeof globalThis,
): number {
  const own = parseFloat(style.marginTop) || 0;
  if (!lets(style, "top")) return own;
  const first = edgeChild(el, win, "first");
  if (!first) return own;
  if (Math.abs(first.el.getBoundingClientRect().top - el.getBoundingClientRect().top) > 0.5)
    return own;
  return collapsed(own, topMargin(first.el, first.style, win));
}

/** Drops what a copy must not carry: handlers, and names XML cannot write. */
function tidy(out: Element): void {
  for (const a of [...out.attributes]) {
    if (!/^[A-Za-z_][\w.:-]*$/.test(a.name) || /^on/i.test(a.name)) out.removeAttribute(a.name);
  }
}

/** A control's current value, which a clone's attributes do not carry. */
function formState(el: Element, out: Element): void {
  if (el.localName === "input") {
    const input = el as HTMLInputElement;
    if (input.type === "checkbox" || input.type === "radio") {
      if (input.checked) out.setAttribute("checked", "");
      else out.removeAttribute("checked");
    } else if (input.type !== "file" && input.type !== "password") {
      out.setAttribute("value", input.value);
    }
  } else if (el.localName === "textarea") {
    out.textContent = (el as HTMLTextAreaElement).value;
  } else if (el.localName === "option") {
    if ((el as HTMLOptionElement).selected) out.setAttribute("selected", "");
    else out.removeAttribute("selected");
  }
}

/** Each sticky box's shift from where it flows, measured with every stick let go for a moment. */
function stickShifts(
  sticky: HTMLElement[],
  win: Window & typeof globalThis,
): Map<HTMLElement, Point> {
  const shifts = new Map<HTMLElement, Point>();
  if (sticky.length === 0) return shifts;
  const shown = sticky.map((el) => el.getBoundingClientRect());
  const kept = sticky.map((el) => el.getAttribute("style"));
  // NO FRAME IS DRAWN between letting go and putting back: the reader never sees it.
  for (const el of sticky) {
    el.style.setProperty("position", "relative", "important");
    el.style.setProperty("inset", "auto", "important");
  }
  const flows = sticky.map((el) => el.getBoundingClientRect());
  sticky.forEach((el, i) => {
    const style = kept[i];
    if (style === null || style === undefined) el.removeAttribute("style");
    else el.setAttribute("style", style);
  });
  const total = new Map<HTMLElement, Point>();
  sticky.forEach((el, i) => {
    total.set(
      el,
      stuckBy({ x: shown[i]!.left, y: shown[i]!.top }, { x: flows[i]!.left, y: flows[i]!.top }),
    );
  });
  for (const el of sticky) {
    let around: Point | undefined;
    for (let at = el.parentElement; at && !around; at = at.parentElement) {
      around = at instanceof win.HTMLElement ? total.get(at) : undefined;
    }
    const own = total.get(el)!;
    shifts.set(el, around ? stuckBy(own, { x: 0, y: 0 }, around) : own);
  }
  return shifts;
}

/** A scrolled element's copy: its children in a wrapper that lays them out as it did, moved by its scroll. */
function wrapScroll(el: HTMLElement, style: CSSStyleDeclaration, out: HTMLElement): void {
  const { outer, inner } = scrollerDisplays(style.display);
  const wrap = out.ownerDocument.createElementNS(XHTML, "div") as HTMLElement;
  for (const p of LAYS_OUT) wrap.style.setProperty(p, style.getPropertyValue(p));
  // OVER THE PADDING BOX: its padding is the element's, so its children sit where they did.
  for (const side of ["top", "right", "bottom", "left"]) {
    wrap.style.setProperty(`margin-${side}`, `-${style.getPropertyValue(`padding-${side}`)}`);
  }
  wrap.style.setProperty("display", inner);
  wrap.style.setProperty("box-sizing", "border-box");
  wrap.style.setProperty("width", `${el.clientWidth}px`);
  wrap.style.setProperty("height", `${el.clientHeight}px`);
  wrap.style.setProperty("transform", `translate(${-el.scrollLeft}px, ${-el.scrollTop}px)`);
  wrap.append(...out.childNodes);
  out.append(wrap);
  out.style.setProperty("display", outer);
  out.style.setProperty("overflow", "hidden");
  out.style.setProperty("column-count", "auto");
  out.style.setProperty("column-width", "auto");
  out.style.setProperty("align-content", "normal");
}
