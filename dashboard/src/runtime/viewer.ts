/**
 * The page's file viewer: a link to one of the artifact's own files that the
 * frame cannot hold opens here, over the page, and the frame stays where it is.
 *
 * WHY THE RUNTIME TAKES THE CLICK. A frame can only hold a document that says
 * hello, which is a page served with the runtime. Navigated to a picture, a
 * PDF, a video or an asset, the frame gets a file no frame may show
 * (`X-Frame-Options: DENY`) or a document with no runtime in it, loads with no
 * hello, and the dashboard closes it as a page that left its frame. That rule
 * is right; what a page lacked was a way to show its own file that is not a
 * frame navigation.
 *
 * WHY IT IS DRAWN HERE rather than by the host: the file loads exactly as the
 * page itself may load it (its policy admits images and media from its own
 * origin), so no raw file is ever framed and nothing new crosses the bridge;
 * every host that embeds a page gets it, the render browser included; and only
 * the page knows the link, its picture's words and the files linked beside it.
 *
 * WHY NOT THE DESIGN SYSTEM'S COMPONENTS: the runtime is one classic script
 * placed first in arbitrary author pages, so it cannot bring React or the
 * dashboard's stylesheet. The viewer is plain DOM in a shadow root, where the
 * page's styles cannot reach, painted with the kit's own token values and
 * glyphs. The root is open, as a page's own components' are, so the tools
 * that read a page's accessibility (and its suites) read the viewer too.
 */

import closeGlyph from "@crewlethq/icons/glyphs/x.svg?raw";
import previousGlyph from "@crewlethq/icons/glyphs/chevron-left.svg?raw";
import nextGlyph from "@crewlethq/icons/glyphs/chevron-right.svg?raw";
// RELATIVE, as in the bridge: the runtime is built alone, where the `~` alias does not exist.
import { ARTIFACT_PAGE_EXTENSIONS } from "../contract/artifactsworkspace.ts";

const PAGES: ReadonlySet<string> = new Set(ARTIFACT_PAGE_EXTENSIONS);
const XLINK = "http://www.w3.org/1999/xlink";
const ASSETS = "_assets/";

/** One file the viewer can show: where it is, its path under the ticket, and what the page says it is. */
export interface ViewerItem {
  /** Its address, as the page linked it, without a fragment. */
  url: string;
  /** Its path under the ticket, decoded: `img/before.png`, or `_assets/<id>` for an asset. */
  file: string;
  /** What the page says it is: its picture's alternative text, or the link's own words. */
  label: string;
}

/** How the artifact origin answered for a file: the type it serves, or the status it refused with. */
export type Probe = { ok: true; type: string } | { ok: false; status: number };

/** A path's extension, lower case and without its dot; empty where its last segment has none. */
function extensionOf(path: string): string {
  const name = path.slice(path.lastIndexOf("/") + 1);
  const dot = name.lastIndexOf(".");
  return dot > 0 ? name.slice(dot + 1).toLowerCase() : "";
}

/**
 * The file of this artifact `url` names, as its path under the ticket, when
 * the frame cannot hold it: null for a page (the frame follows the link), for
 * the ticket's own document, for another site, and for anything not served
 * beside this page. An asset is never a page: no runtime is placed in one.
 */
export function viewerFile(url: URL, origin: string, ticketBase: string): string | null {
  if (url.origin !== origin) return null;
  const prefix = `${ticketBase}/`;
  if (!url.pathname.startsWith(prefix)) return null;
  let file: string;
  try {
    file = decodeURIComponent(url.pathname.slice(prefix.length));
  } catch {
    return null;
  }
  if (file === "" || PAGES.has(extensionOf(file))) return null;
  return file;
}

/** An `a` or `area` element's address as written, an SVG link's included; null for none. */
function hrefOf(el: Element): string | null {
  return el.getAttribute("href") ?? el.getAttributeNS(XLINK, "href");
}

/** Whether something on an event's path is a link with an address. Read by shape, so a frame's elements count. */
function isLink(node: EventTarget): node is Element {
  const el = node as Element;
  return (
    el.nodeType === 1 && (el.localName === "a" || el.localName === "area") && hrefOf(el) !== null
  );
}

/** Words as a label shows them: whitespace collapsed, empty for none. */
function words(s: string | null | undefined): string {
  return (s ?? "").replace(/\s+/g, " ").trim();
}

/** A file's own name: the last segment of its path, or the label for an asset, whose path is an id. */
export function nameOf(item: ViewerItem): string {
  if (item.file.startsWith(ASSETS)) return item.label;
  return item.file.slice(item.file.lastIndexOf("/") + 1);
}

/**
 * What a link says about the file it opens, as a reader of the page hears it:
 * its picture's alternative text first (a picture link's subject is the
 * picture), then the link's own label, an area's alternative text, its title,
 * its words, and the file's name where it says nothing.
 */
export function labelOf(link: Element, file: string): string {
  const picture = link.querySelector("img[alt]");
  for (const said of [
    picture?.getAttribute("alt"),
    link.getAttribute("aria-label"),
    link.localName === "area" ? link.getAttribute("alt") : null,
    link.getAttribute("title"),
    link.textContent,
  ]) {
    const label = words(said);
    if (label) return label;
  }
  return file.slice(file.lastIndexOf("/") + 1);
}

/** The viewer's item for one link, or null where the frame can follow it. */
function itemOf(link: Element, origin: string, ticketBase: string): ViewerItem | null {
  const href = hrefOf(link);
  if (href === null) return null;
  let url: URL;
  try {
    url = new URL(href, link.ownerDocument.baseURI);
  } catch {
    return null;
  }
  const file = viewerFile(url, origin, ticketBase);
  if (file === null) return null;
  url.hash = "";
  return { url: url.href, file, label: labelOf(link, file) };
}

/** Every file the document links that the frame cannot hold, in document order, each once. */
export function linkedFiles(doc: Document, origin: string, ticketBase: string): ViewerItem[] {
  const out: ViewerItem[] = [];
  const seen = new Set<string>();
  for (const link of doc.querySelectorAll("a, area")) {
    const item = itemOf(link, origin, ticketBase);
    if (!item || seen.has(item.file)) continue;
    seen.add(item.file);
    out.push(item);
  }
  return out;
}

/** What the page's file links are watched over. */
export interface FileLinksOptions {
  window: Window;
  /** The ticket path the page was served under (`/t/<ticket>`), empty outside one. */
  ticketBase: string;
  /** Shows `items[at]`, and gives focus back to `opener` when it closes. */
  open: (items: ViewerItem[], at: number, opener: Element) => void;
}

/**
 * Opens a link to one of the artifact's own files in the viewer instead of
 * navigating the frame to it. Returns the call that stops.
 *
 * HEARD LAST, on the window as the click bubbles, and only where nothing
 * prevented it: a page that handles its own links (a gallery of its own, a
 * client-side router) keeps them. A click with a modifier is the browser's,
 * as everywhere: it asks for a new window or a download, which the frame's
 * sandbox refuses without moving the frame. A key that activates a link is a
 * click too, so the keyboard reaches the viewer the same way.
 */
export function interceptFileLinks({
  window: win,
  ticketBase,
  open,
}: FileLinksOptions): () => void {
  const click = (event: MouseEvent) => {
    if (event.defaultPrevented || event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const link = event.composedPath().find(isLink);
    if (!link) return;
    const origin = win.location.origin;
    const item = itemOf(link, origin, ticketBase);
    if (!item) return;
    event.preventDefault();
    const items = linkedFiles(win.document, origin, ticketBase);
    const at = items.findIndex((i) => i.file === item.file);
    // A LINK INSIDE A SHADOW ROOT is one the document's own list cannot see.
    if (at < 0) open([item], 0, link);
    else open(items, at, link);
  };
  win.addEventListener("click", click);
  return () => win.removeEventListener("click", click);
}

/** What a served type is drawn as. */
export function drawnAs(type: string): "image" | "video" | "audio" | "file" {
  const essence = words(type.split(";")[0]).toLowerCase();
  if (essence.startsWith("image/")) return "image";
  if (essence.startsWith("video/")) return "video";
  if (essence.startsWith("audio/")) return "audio";
  return "file";
}

/** The kind a file the viewer cannot draw is named by, from its served type. */
export function kindOf(type: string): string {
  const essence = words(type.split(";")[0]).toLowerCase();
  if (essence === "application/pdf") return "PDF";
  if (essence === "text/html") return "Web page";
  if (essence.startsWith("font/")) return "Font";
  if (essence.startsWith("model/")) return "3D model";
  if (essence === "application/wasm") return "WebAssembly";
  if (essence.startsWith("text/") || essence.endsWith("json") || essence.endsWith("xml")) {
    return "Text";
  }
  return "File";
}

/**
 * The design kit's values the viewer is drawn with (`@crewlethq/tokens`), the
 * dark ones: a picture reads best on a dark surface, so the viewer is one in
 * either theme. COPIED, NOT IMPORTED: the token module is one object per
 * group, and importing it would put every colour the dashboard has into every
 * page's runtime. `viewer.test.ts` holds each value equal to the kit's.
 */
export const KIT = {
  surfaceGlass: "rgba(28, 30, 36, 0.90)",
  surfaceElevated: "rgba(255, 255, 255, 0.12)",
  textPrimary: "#fafafa",
  textSecondary: "#c9ced8",
  textTertiary: "#95a2b6",
  borderDefault: "rgba(255, 255, 255, 0.08)",
  borderStrongHover: "#5a5e65",
  focus: "#7e5bff",
  fontSans:
    "'Geist', -apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans', Helvetica, Arial, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji'",
  size2xs: "11px",
  sizeXs: "12px",
  sizeSm: "14px",
  sizeMd: "16px",
  weightSemibold: "600",
  radiusLg: "12px",
  radiusCircle: "50%",
  controlLg: "44px",
  spacing2: "8px",
  spacing3: "12px",
  spacing6: "24px",
} as const;

const STYLE = `
:host { all: initial; }
dialog {
  box-sizing: border-box; position: fixed; inset: 0; width: 100%; height: 100%;
  max-width: none; max-height: none; margin: 0; padding: ${KIT.spacing3}; border: 0;
  background: transparent; color: ${KIT.textPrimary}; overscroll-behavior: contain;
  font-family: ${KIT.fontSans}; font-size: ${KIT.sizeSm}; line-height: 1.5;
}
dialog[open] { display: flex; flex-direction: column; gap: ${KIT.spacing2}; }
dialog::backdrop { background: ${KIT.surfaceGlass}; }
.bar { display: flex; align-items: center; justify-content: space-between; gap: ${KIT.spacing3}; }
.count { padding-inline: ${KIT.spacing2}; color: ${KIT.textSecondary}; font-size: ${KIT.sizeXs}; font-variant-numeric: tabular-nums; }
.stage { flex: 1 1 auto; min-height: 0; display: flex; align-items: center; gap: ${KIT.spacing3}; }
figure {
  flex: 1 1 auto; align-self: stretch; min-width: 0; min-height: 0; margin: 0;
  display: flex; flex-direction: column; align-items: center; justify-content: center; gap: ${KIT.spacing2};
}
.media { display: block; flex: 0 1 auto; min-height: 0; max-width: 100%; max-height: 100%; object-fit: contain; }
audio.media { width: min(100%, 480px); }
figcaption { flex: none; max-width: 72ch; text-align: center; color: ${KIT.textSecondary}; overflow-wrap: anywhere; }
.card {
  box-sizing: border-box; max-width: min(100%, 400px); padding: ${KIT.spacing6};
  display: flex; flex-direction: column; gap: ${KIT.spacing2}; text-align: center;
  background: ${KIT.surfaceElevated}; border: 1px solid ${KIT.borderDefault}; border-radius: ${KIT.radiusLg};
}
.kind { color: ${KIT.textTertiary}; font-size: ${KIT.size2xs}; font-weight: ${KIT.weightSemibold}; letter-spacing: 0.06em; text-transform: uppercase; }
.name { color: ${KIT.textPrimary}; font-size: ${KIT.sizeMd}; font-weight: ${KIT.weightSemibold}; overflow-wrap: anywhere; }
.card p, .loading { margin: 0; }
.note, .loading { color: ${KIT.textSecondary}; }
button {
  all: unset; box-sizing: border-box; flex: none; display: inline-grid; place-items: center;
  width: ${KIT.controlLg}; height: ${KIT.controlLg}; border-radius: ${KIT.radiusCircle};
  color: ${KIT.textPrimary}; background: ${KIT.surfaceElevated};
  border: 1px solid ${KIT.borderDefault}; cursor: pointer;
}
button:hover { border-color: ${KIT.borderStrongHover}; }
button:focus-visible { outline: 2px solid ${KIT.focus}; outline-offset: 2px; }
button[hidden] { display: none; }
button svg { width: 20px; height: 20px; pointer-events: none; }
.said { position: absolute; width: 1px; height: 1px; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
`;

/** What the viewer is drawn over. */
export interface FileViewerOptions {
  /** The page's window; the suite's own, or a frame's, outside a page. */
  window: Window;
  /** How the origin answers for a file: a HEAD request outside a suite. */
  probe?: (url: string) => Promise<Probe>;
}

/** The parts of the viewer, made at its first open. */
interface Parts {
  dialog: HTMLDialogElement;
  bar: HTMLElement;
  count: HTMLElement;
  close: HTMLButtonElement;
  stage: HTMLElement;
  previous: HTMLButtonElement;
  next: HTMLButtonElement;
  figure: HTMLElement;
  said: HTMLElement;
}

/**
 * A modal viewer over the page: one file at a time, at the largest size the
 * frame fits, with the page's other linked files a step either side.
 *
 * MODAL, so the page behind is inert, Tab stays inside, and Escape closes it,
 * as a `<dialog>` does natively; a click on the dark around the file closes it
 * too, and focus goes back to the link that opened it. Its clicks and keys stay
 * inside, so the page's own shortcuts do not act behind it.
 */
export class FileViewer {
  private readonly win: Window;
  private readonly probe: (url: string) => Promise<Probe>;
  private parts: Parts | null = null;
  private items: ViewerItem[] = [];
  private at = 0;
  private opener: Element | null = null;
  // Each file shown is a generation, so a late answer for one already left is dropped.
  private generation = 0;

  constructor(options: FileViewerOptions) {
    this.win = options.window;
    // THE PAGE'S FETCH AS IT WAS when the runtime ran, before the page's own scripts could replace it.
    const fetchFile = this.win.fetch.bind(this.win);
    this.probe =
      options.probe ??
      (async (url) => {
        const res = await fetchFile(url, { method: "HEAD" });
        return res.ok
          ? { ok: true, type: res.headers.get("Content-Type") ?? "" }
          : { ok: false, status: res.status };
      });
  }

  /** Whether a file is showing. */
  get isOpen(): boolean {
    return this.parts?.dialog.open ?? false;
  }

  /** Shows `items[at]`, and gives focus back to `opener` when the viewer closes. */
  open(items: ViewerItem[], at: number, opener: Element | null): void {
    if (items.length === 0) return;
    const parts = this.build();
    this.items = items;
    this.opener = opener;
    parts.previous.hidden = parts.next.hidden = items.length < 2;
    this.show(at);
    if (!parts.dialog.open) parts.dialog.showModal();
  }

  close(): void {
    if (this.parts?.dialog.open) this.parts.dialog.close();
  }

  /** Moves `by` files along the page's, past either end to the other. */
  step(by: number): void {
    if (this.isOpen && this.items.length > 1) this.show(this.at + by);
  }

  private show(index: number): void {
    const parts = this.parts;
    if (!parts) return;
    const n = this.items.length;
    this.at = ((index % n) + n) % n;
    const item = this.items[this.at];
    if (!item) return;
    const generation = ++this.generation;
    const doc = this.win.document;
    const place = n > 1 ? `${this.at + 1} of ${n}` : "";
    parts.dialog.setAttribute("aria-label", item.label);
    parts.count.textContent = place;
    parts.said.textContent = place ? `${item.label}, ${place}` : item.label;
    const loading = doc.createElement("p");
    loading.className = "loading";
    loading.textContent = `Loading ${nameOf(item)}`;
    parts.figure.replaceChildren(loading);
    const draw = (answer: Probe) => {
      if (generation === this.generation) parts.figure.replaceChildren(...this.draw(item, answer));
    };
    this.probe(item.url).then(draw, () => draw({ ok: false, status: 0 }));
  }

  /** The figure's contents for one file, as the origin answered for it. */
  private draw(item: ViewerItem, answer: Probe): Node[] {
    const doc = this.win.document;
    if (!answer.ok) {
      return answer.status === 404
        ? [this.card("Not found", nameOf(item), "This artifact has no file at this address.")]
        : [this.unavailable(item)];
    }
    const as = drawnAs(answer.type);
    if (as === "file") {
      return [
        this.card(kindOf(answer.type), nameOf(item), "This file can't be shown inside the page."),
      ];
    }
    let media: HTMLImageElement | HTMLMediaElement;
    if (as === "image") {
      const picture = doc.createElement("img");
      picture.alt = item.label;
      picture.decoding = "async";
      media = picture;
    } else {
      const player = doc.createElement(as);
      player.controls = true;
      player.preload = "metadata";
      player.setAttribute("aria-label", item.label);
      media = player;
    }
    media.className = "media";
    media.addEventListener("error", () => media.replaceWith(this.unavailable(item)), {
      once: true,
    });
    media.setAttribute("src", item.url);
    // THE CAPTION IS FOR THE EYE: the file's own name for it (alt or label) is what is heard.
    const caption = doc.createElement("figcaption");
    caption.setAttribute("aria-hidden", "true");
    caption.textContent = item.label;
    return [media, caption];
  }

  private unavailable(item: ViewerItem): HTMLElement {
    return this.card(
      "Unavailable",
      nameOf(item),
      "This file could not be loaded. Reload the page to try again.",
    );
  }

  private card(kind: string, name: string, note: string): HTMLElement {
    const doc = this.win.document;
    const card = doc.createElement("div");
    card.className = "card";
    for (const [cls, text] of [
      ["kind", kind],
      ["name", name],
      ["note", note],
    ] as const) {
      const line = doc.createElement("p");
      line.className = cls;
      line.textContent = text;
      card.append(line);
    }
    return card;
  }

  /** The viewer's elements, made once, in a shadow root on the page's root element. */
  private build(): Parts {
    if (this.parts) return this.parts;
    const doc = this.win.document;
    const host = doc.createElement("crewlet-file-viewer");
    const root = host.attachShadow({ mode: "open" });
    const style = doc.createElement("style");
    style.textContent = STYLE;
    const el = <K extends keyof HTMLElementTagNameMap>(tag: K, cls: string) => {
      const made = doc.createElement(tag);
      if (cls) made.className = cls;
      return made;
    };
    const button = (label: string, glyph: string) => {
      const made = el("button", "");
      made.type = "button";
      made.setAttribute("aria-label", label);
      made.title = label;
      made.append(this.glyph(glyph));
      return made;
    };
    const dialog = el("dialog", "");
    const bar = el("div", "bar");
    const count = el("span", "count");
    const close = button("Close", closeGlyph);
    close.autofocus = true;
    const stage = el("div", "stage");
    const previous = button("Previous file", previousGlyph);
    const next = button("Next file", nextGlyph);
    const figure = el("figure", "");
    const said = el("p", "said");
    said.setAttribute("role", "status");
    bar.append(count, close);
    stage.append(previous, figure, next);
    dialog.append(bar, stage, said);
    root.append(style, dialog);
    doc.documentElement.append(host);

    close.addEventListener("click", () => this.close());
    previous.addEventListener("click", () => this.step(-1));
    next.addEventListener("click", () => this.step(1));
    dialog.addEventListener("click", (event) => {
      event.stopPropagation();
      const on = event.target;
      if (on === dialog || on === bar || on === stage || on === figure) this.close();
    });
    dialog.addEventListener("keydown", (event) => {
      event.stopPropagation();
      if (event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey) return;
      // A PLAYER'S OWN ARROWS seek; the viewer's step only from anywhere else.
      if ((event.target as Element).localName === "video") return;
      if ((event.target as Element).localName === "audio") return;
      const by = event.key === "ArrowLeft" ? -1 : event.key === "ArrowRight" ? 1 : 0;
      if (by === 0) return;
      event.preventDefault();
      this.step(by);
    });
    dialog.addEventListener("close", () => this.closed());
    this.parts = { dialog, bar, count, close, stage, previous, next, figure, said };
    return this.parts;
  }

  /**
   * Puts the viewer away: the file goes (a playing one stops), and focus
   * returns to the link. Nothing where it opened again before the close was
   * told, as a page's script may close and open it within one task.
   */
  private closed(): void {
    if (this.parts?.dialog.open) return;
    this.generation++;
    this.parts?.figure.replaceChildren();
    const opener = this.opener;
    this.opener = null;
    if (opener?.isConnected) (opener as HTMLElement).focus?.({ preventScroll: true });
  }

  /** One of the kit's glyphs, read from its own drawing. */
  private glyph(source: string): Element {
    const drawn = new DOMParser().parseFromString(source, "image/svg+xml").documentElement;
    const svg = this.win.document.importNode(drawn, true);
    svg.removeAttribute("class");
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("focusable", "false");
    return svg;
  }
}
