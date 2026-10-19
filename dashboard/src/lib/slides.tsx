/**
 * A slide drawn from its canonical tree, and written back as the slide format.
 *
 * NOTHING HERE PARSES MARKUP. The engine reads every slide through the parser
 * its publish was held to (`internal/artifacts/slideformat`) and answers the
 * tree as JSON (`GET …/versions/{n}/deck`); this module turns that tree into
 * React elements, one `createElement` per node, the same discipline as
 * `lib/markdown.ts`. No author string reaches `innerHTML`, a style sheet or a
 * URL the dashboard loads: text is a text node, a style value is a CSSOM
 * property React sets, an image is an object URL of a version file the
 * dashboard fetched itself, and a link is `https:` only.
 *
 * THE SAME ALLOWLISTS AGAIN. The tree is the engine's, but a renderer that
 * trusted its input would draw whatever a future bug put there. Every element,
 * style property, drawing element and drawing attribute is checked against the
 * subset (`contract/artifacts.ts`, gated equal to the engine's), and anything
 * else is drawn as nothing (an attribute, a declaration) or as its text alone
 * (an element). A style value that names a URL, a variable or an expression is
 * dropped whatever its property.
 *
 * ONE DRAWING, ONE ID SPACE. An inline SVG's ids are global in a document, and
 * one slide is drawn several times at once (the stage, its thumbnail, the
 * presenter's next slide), so every drawing's ids and references are prefixed
 * with the instance's own prefix.
 *
 * WRITTEN BACK CANONICALLY. A person's edit changes the tree; `slideSource`
 * writes it as slide HTML the engine's parser reads back to the same tree,
 * every value in the canonical form the engine wrote, so a slide saved by the
 * editor is normalized the way Claude's editor normalizes one.
 */

import { createElement, Fragment, type CSSProperties, type ReactNode } from "react";
import {
  ActivityGlyph,
  BookOpenGlyph,
  ChartColumnGlyph,
  CheckGlyph,
  CircleCheckGlyph,
  ClockGlyph,
  CodeGlyph,
  DatabaseGlyph,
  GlobeGlyph,
  HouseGlyph,
  KeyGlyph,
  LinkGlyph,
  LockGlyph,
  MessageSquareGlyph,
  SearchGlyph,
  SendGlyph,
  SettingsGlyph,
  ShieldGlyph,
  StarGlyph,
  TriangleAlertGlyph,
  UsersGlyph,
  WrenchGlyph,
  ZapGlyph,
} from "@crewlethq/icons/glyphs";
import {
  SLIDE_ELEMENTS,
  SLIDE_ICONS,
  SLIDE_SHAPES,
  SLIDE_STAGE_HEIGHT,
  SLIDE_STAGE_WIDTH,
  SLIDE_STYLE_PROPERTIES,
  SLIDE_SVG_ATTRIBUTES,
  SLIDE_SVG_ELEMENTS,
} from "~/contract/artifacts.ts";
import type { DeckSlide, SlideDecl, SlideNode } from "~/protocol/index.ts";

type SlideIcon = (typeof SLIDE_ICONS)[number];

/** Each icon name the format takes, drawn with Crewlet's own glyph for it. */
const ICONS: Readonly<Record<SlideIcon, typeof CheckGlyph>> = {
  Activity: ActivityGlyph,
  Book: BookOpenGlyph,
  Chart: ChartColumnGlyph,
  Chat: MessageSquareGlyph,
  Check: CheckGlyph,
  CheckCircle: CircleCheckGlyph,
  Clock: ClockGlyph,
  Code: CodeGlyph,
  Database: DatabaseGlyph,
  Globe: GlobeGlyph,
  Home: HouseGlyph,
  Key: KeyGlyph,
  Lightning: ZapGlyph,
  Link: LinkGlyph,
  Lock: LockGlyph,
  PaperPlane: SendGlyph,
  Search: SearchGlyph,
  Settings: SettingsGlyph,
  Star: StarGlyph,
  Tool: WrenchGlyph,
  Trust: ShieldGlyph,
  Users: UsersGlyph,
  Warning: TriangleAlertGlyph,
  Wrench: WrenchGlyph,
};

const ELEMENTS: ReadonlySet<string> = new Set(SLIDE_ELEMENTS);
const PROPERTIES: ReadonlySet<string> = new Set(SLIDE_STYLE_PROPERTIES);
const SVG_ELEMENTS: ReadonlySet<string> = new Set(SLIDE_SVG_ELEMENTS);
const SVG_ATTRIBUTES: ReadonlySet<string> = new Set(SLIDE_SVG_ATTRIBUTES);
const SHAPES: ReadonlySet<string> = new Set(SLIDE_SHAPES);

/** What no style value may carry, whatever its property: the engine's own list. */
const FORBIDDEN = [
  "url(",
  "image(",
  "image-set(",
  "var(",
  "env(",
  "attr(",
  "expression",
  "@",
  "\\",
  "/*",
  "*/",
  "!important",
  "<",
  ">",
  "{",
  "}",
];

/** Where a slide is drawn: its drawings' id prefix and its images' URLs. */
export interface SlideRenderContext {
  /** A prefix unique to this drawn instance, for its drawings' ids. */
  prefix: string;
  /** The object URL a version file is shown through, or null until it is. */
  image: (path: string) => string | null;
  /**
   * Editing in place: every run of text becomes editable, and a finished edit
   * reports the run's path and its new text. Absent, the slide is read only.
   */
  edit?: { onText: (path: string, text: string) => void } | undefined;
}

/** A node's address in its slide: child indexes from the section, "0/2/1" (D65's element path). */
export type NodePath = string;

/** The camelCase React property a declaration is set through. */
function reactProperty(p: string): string {
  const camel = p.replace(/-([a-z])/g, (_, c: string) => c.toUpperCase());
  return p.startsWith("-webkit-") ? "Webkit" + camel.slice("Webkit".length) : camel;
}

/** A tree's declarations as a style object, each held to the subset again. */
export function slideStyle(decls: readonly SlideDecl[] | undefined): CSSProperties {
  const out: Record<string, string> = {};
  for (const d of decls ?? []) {
    if (!PROPERTIES.has(d.p)) continue;
    const low = d.v.toLowerCase();
    if (FORBIDDEN.some((f) => low.includes(f))) continue;
    out[reactProperty(d.p)] = d.v;
  }
  return out as CSSProperties;
}

/** The clip a shape draws its box as. */
const SHAPE_CLIPS: Readonly<Record<string, string | undefined>> = {
  diamond: "polygon(50% 0, 100% 50%, 50% 100%, 0 50%)",
  "arrow-right": "polygon(0 30%, 60% 30%, 60% 0, 100% 50%, 60% 100%, 60% 70%, 0 70%)",
  "arrow-left": "polygon(100% 30%, 40% 30%, 40% 0, 0 50%, 40% 100%, 40% 70%, 100% 70%)",
  "arrow-down": "polygon(30% 0, 70% 0, 70% 60%, 100% 60%, 50% 100%, 0 60%, 30% 60%)",
  "arrow-up": "polygon(30% 100%, 70% 100%, 70% 40%, 100% 40%, 50% 0, 0 40%, 30% 40%)",
};

/** The drawing attribute's React property: SVG's camelCase names stay, a hyphenated one is camelCased, aria- stays. */
function svgProperty(name: string): string {
  if (name.startsWith("aria-")) return name;
  return name.replace(/-([a-z])/g, (_, c: string) => c.toUpperCase());
}

/** A drawing attribute with its references moved into the instance's id space. */
function svgValue(name: string, value: string, prefix: string): string | null {
  if (/javascript:|data:|<|expression/i.test(value)) return null;
  if (name === "id") return prefix + value;
  if (name === "href") return value.startsWith("#") ? "#" + prefix + value.slice(1) : null;
  if (/url\(/i.test(value)) {
    const m = /^url\(\s*#([A-Za-z][A-Za-z0-9_.-]{0,63})\s*\)$/.exec(value.trim());
    return m ? `url(#${prefix}${m[1]})` : null;
  }
  return value;
}

function renderSVG(node: SlideNode, ctx: SlideRenderContext, key: string): ReactNode {
  if (node.tag === undefined) return node.text ?? "";
  if (!SVG_ELEMENTS.has(node.tag)) return null;
  const props: Record<string, string> = { key };
  for (const [name, value] of Object.entries(node.attrs ?? {})) {
    if (!SVG_ATTRIBUTES.has(name)) continue;
    const v = svgValue(name, value, ctx.prefix);
    if (v !== null) props[svgProperty(name)] = v;
  }
  return createElement(
    node.tag,
    props,
    ...(node.children ?? []).map((c, i) => renderSVG(c, ctx, `${key}.${i}`)),
  );
}

function renderNode(node: SlideNode, ctx: SlideRenderContext, path: NodePath): ReactNode {
  if (node.tag === undefined) {
    const text = node.text ?? "";
    if (!ctx.edit) return text;
    const onText = ctx.edit.onText;
    // A RUN OF TEXT EDITED IN PLACE: plain text only, so no element is ever
    // typed into the tree, and Enter finishes the edit rather than breaking
    // the line, which is a <br> the format writes on purpose.
    return (
      <span
        key={path}
        className="slide-run"
        contentEditable="plaintext-only"
        suppressContentEditableWarning
        spellCheck
        data-run={path}
        onKeyDown={(event) => {
          if (event.key === "Enter") {
            event.preventDefault();
            event.currentTarget.blur();
          }
          event.stopPropagation();
        }}
        onBlur={(event) => {
          const next = event.currentTarget.textContent ?? "";
          if (next !== text) onText(path, next);
        }}
      >
        {text}
      </span>
    );
  }
  const tag = node.tag;
  const children = (node.children ?? []).map((c, i) => renderNode(c, ctx, `${path}/${i}`));
  if (!ELEMENTS.has(tag)) return <Fragment key={path}>{children}</Fragment>;
  const style = slideStyle(node.style);
  const attrs = node.attrs ?? {};
  switch (tag) {
    case "svg": {
      const props: Record<string, string> = {};
      for (const [name, value] of Object.entries(attrs)) {
        if (!SVG_ATTRIBUTES.has(name)) continue;
        const v = svgValue(name, value, ctx.prefix);
        if (v !== null) props[svgProperty(name)] = v;
      }
      return createElement(
        "svg",
        { key: path, ...props, role: "img", style, className: "slide-svg" },
        ...(node.children ?? []).map((c, i) => renderSVG(c, ctx, `${path}/${i}`)),
      );
    }
    case "img": {
      const src = attrs.src ? ctx.image(attrs.src) : null;
      return src ? (
        <img key={path} className="slide-img" src={src} alt={attrs.alt ?? ""} style={style} />
      ) : (
        <span
          key={path}
          className="slide-img slide-img-pending"
          role="img"
          aria-label={attrs.alt ?? ""}
          style={style}
        />
      );
    }
    case "a": {
      const href = attrs.href ?? "";
      return /^https:\/\/[^\s<>"'`]+$/.test(href) ? (
        <a key={path} className="slide-a" href={href} target="_blank" rel="noopener noreferrer">
          {children}
        </a>
      ) : (
        <Fragment key={path}>{children}</Fragment>
      );
    }
    case "x-shape": {
      const kind = SHAPES.has(attrs.kind ?? "") ? attrs.kind! : "rect";
      const clip = SHAPE_CLIPS[kind];
      return (
        <span
          key={path}
          className={`slide-shape slide-shape-${kind}`}
          aria-hidden="true"
          style={clip ? { ...style, clipPath: clip } : style}
        />
      );
    }
    case "x-icon": {
      const Glyph = ICONS[attrs.name as SlideIcon];
      return (
        <span key={path} className="slide-icon" aria-hidden="true" style={style}>
          {Glyph ? <Glyph size="50%" /> : null}
        </span>
      );
    }
    case "table":
      return (
        <table key={path} className="slide-table" style={style}>
          <tbody>{children}</tbody>
        </table>
      );
    case "div": {
      const indent = attrs["data-text"];
      const box = indent !== undefined && /^\d{1,3}$/.test(indent);
      return (
        <div
          key={path}
          className={box ? "slide-div slide-text-box" : "slide-div"}
          data-display={declared(node.style, "display") ? "set" : undefined}
          style={
            box
              ? {
                  ...style,
                  paddingInlineStart: undefined,
                  ["--slide-indent" as string]: `${indent}px`,
                }
              : style
          }
        >
          {children}
        </div>
      );
    }
    case "br":
      return <br key={path} />;
    case "hr":
      return <hr key={path} className="slide-hr" style={style} />;
    default:
      return createElement(tag, { key: path, className: `slide-${tag}`, style }, ...children);
  }
}

function declared(decls: readonly SlideDecl[] | undefined, p: string): boolean {
  return (decls ?? []).some((d) => d.p === p);
}

/**
 * A slide drawn at its own size, 1920 by 1080: put it in a `SlideStage`,
 * which scales it to its box.
 */
export function renderSlide(slide: DeckSlide, ctx: SlideRenderContext): ReactNode {
  const root = slide.root;
  return (
    <div
      className="slide-canvas"
      data-display={declared(root.style, "display") ? "set" : undefined}
      style={{ ...slideStyle(root.style), width: SLIDE_STAGE_WIDTH, height: SLIDE_STAGE_HEIGHT }}
    >
      {(root.children ?? []).map((c, i) => renderNode(c, ctx, String(i)))}
    </div>
  );
}

// ---- the tree, edited ----------------------------------------------------- //

/** The node at path in a slide's root, or null. */
export function nodeAt(root: SlideNode, path: NodePath): SlideNode | null {
  let at: SlideNode | undefined = root;
  for (const step of path.split("/")) {
    at = at?.children?.[Number(step)];
    if (!at) return null;
  }
  return at;
}

/** root with the run of text at path replaced by text: a copy, the original untouched. */
export function withText(root: SlideNode, path: NodePath, text: string): SlideNode {
  const steps = path.split("/").map(Number);
  const walk = (node: SlideNode, depth: number): SlideNode => {
    if (depth === steps.length) return node.tag === undefined ? { text } : node;
    const i = steps[depth]!;
    const children = (node.children ?? []).map((c, j) => (j === i ? walk(c, depth + 1) : c));
    return { ...node, children };
  };
  return walk(root, 0);
}

/** A slide's title from its tree: its first heading's text, else its first paragraph's, as the engine reads it. */
export function titleOf(root: SlideNode): string {
  const text = (n: SlideNode): string =>
    n.tag === undefined
      ? (n.text ?? "")
      : n.tag === "br"
        ? " "
        : (n.children ?? []).map(text).join("");
  const first = (n: SlideNode, tags: readonly string[]): string => {
    if (n.tag !== undefined && tags.includes(n.tag))
      return text(n).split(/\s+/).filter(Boolean).join(" ");
    for (const c of n.children ?? []) {
      const t = first(c, tags);
      if (t) return t;
    }
    return "";
  };
  return first(root, ["h1", "h2", "h3"]) || first(root, ["p"]);
}

// ---- writing a slide back ------------------------------------------------- //

const VOID = new Set(["br", "img", "hr"]);

function escapeText(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function escapeAttr(s: string): string {
  return escapeText(s).replace(/"/g, "&quot;");
}

function attrsSource(
  attrs: Record<string, string> | undefined,
  style: readonly SlideDecl[] | undefined,
): string {
  let out = "";
  for (const name of Object.keys(attrs ?? {}).sort()) {
    const v = attrs![name]!;
    out += v === "" && name === "data-text" ? ` ${name}` : ` ${name}="${escapeAttr(v)}"`;
  }
  if (style && style.length > 0) {
    out += ` style="${escapeAttr(style.map((d) => `${d.p}:${d.v}`).join(";"))}"`;
  }
  return out;
}

function nodeSource(node: SlideNode, inSVG: boolean): string {
  if (node.tag === undefined) return escapeText(node.text ?? "");
  const svg = inSVG || node.tag === "svg";
  const open = `<${node.tag}${attrsSource(node.attrs, node.style)}`;
  if (VOID.has(node.tag) && !svg) return open + ">";
  const block = !svg && ["div", "ul", "ol", "table", "tr"].includes(node.tag);
  // A BLOCK'S CHILDREN EACH ON A LINE: whitespace between blocks draws
  // nothing and the engine drops it, so the file reads as its author wrote it.
  const kids = (node.children ?? []).map((c) => nodeSource(c, svg)).join(block ? "\n" : "");
  if (svg && node.tag !== "svg" && kids === "") return open + "/>";
  const nl = block && kids ? "\n" : "";
  return `${open}>${nl}${kids}${nl}</${node.tag}>`;
}

/**
 * A slide as its file: one `<section>` and its notes, in the slide format,
 * which the engine reads back to the same tree.
 */
export function slideSource(
  slide: Pick<DeckSlide, "id" | "hidden" | "transition" | "notes" | "root">,
): string {
  let head = `<section id="${escapeAttr(slide.id)}"`;
  if (slide.transition) head += ` data-transition="${escapeAttr(slide.transition)}"`;
  if (slide.hidden) head += " hidden";
  const style = slide.root.style ?? [];
  if (style.length > 0)
    head += ` style="${escapeAttr(style.map((d) => `${d.p}:${d.v}`).join(";"))}"`;
  const body = (slide.root.children ?? []).map((c) => nodeSource(c, false)).join("\n");
  const notes = slide.notes ? `\n<aside>${escapeText(slide.notes)}</aside>` : "";
  return `${head}>\n${body}${notes}\n</section>\n`;
}
