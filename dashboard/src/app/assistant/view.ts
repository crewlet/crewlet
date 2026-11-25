/**
 * An outline of what the screen shows, for the page context: built once and
 * generically from the rendered accessibility tree (each element's role and
 * accessible name), so every screen has one with no code of its own.
 *
 * It lists headings and named sections; fields with their label, kind,
 * whether they are required, filled, disabled, and the error they show;
 * buttons and links; tabs and steps with the current one; and notices. An
 * open dialog comes first. Hidden elements are skipped.
 *
 * NEVER A VALUE: a field says only filled or empty, the text inside a field is
 * never read, and a subtree marked `data-assistant-private` is left out
 * whole. The outline goes to the model provider with the rest of the context.
 */

import {
  ASSISTANT_PAGE_DRAFT_SECRET_PATH,
  ASSISTANT_PAGE_VIEW_ERROR_MAX_RUNES,
  ASSISTANT_PAGE_VIEW_MAX_BYTES,
  ASSISTANT_PAGE_VIEW_MAX_NODES,
  ASSISTANT_PAGE_VIEW_NAME_MAX_RUNES,
  type AssistantPageContext,
  type AssistantPageView,
  type AssistantViewFieldKind,
  type AssistantViewNode,
} from "~/contract/assistant.ts";
import { pageContextFits, wireBytes } from "~/protocol/assistantWire.ts";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";

/** The attribute that keeps a subtree out of the outline. */
export const PRIVATE_ATTRIBUTE = "data-assistant-private";

/** Nesting past this is a layout, not an outline (`wire.pageViewMaxDepth`). */
const MAX_DEPTH = 8;

const SECRET = new RegExp(ASSISTANT_PAGE_DRAFT_SECRET_PATH);
const SECRET_AUTOCOMPLETE = new Set(["current-password", "new-password", "one-time-code"]);
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/g;

/** Text as one line, its runs of space folded. */
function fold(text: string): string {
  return text.replace(CONTROL, " ").replace(/\s+/g, " ").trim();
}

/** Text cut to `max` characters at a whole character, its runs of space folded. */
function clip(text: string, max: number): string {
  const runes = [...fold(text)];
  return runes.length <= max ? runes.join("") : `${runes.slice(0, max - 1).join("")}…`;
}

/** Whether an element is drawn: not hidden, inert, `display: none` or invisible. */
function shown(el: Element): boolean {
  if (el.hasAttribute("hidden") || el.getAttribute("aria-hidden") === "true") return false;
  if (el.hasAttribute("inert") || el.tagName === "TEMPLATE") return false;
  const check = (el as HTMLElement).checkVisibility;
  if (typeof check === "function") return check.call(el, { visibilityProperty: true });
  const style = el.ownerDocument.defaultView?.getComputedStyle(el);
  return !style || (style.display !== "none" && style.visibility !== "hidden");
}

function isPrivate(el: Element): boolean {
  return el.hasAttribute(PRIVATE_ATTRIBUTE);
}

/** A form control, whose text is a value and never read. */
function isControl(el: Element): boolean {
  return (
    el.tagName === "INPUT" ||
    el.tagName === "TEXTAREA" ||
    el.tagName === "SELECT" ||
    (el as HTMLElement).isContentEditable === true ||
    el.getAttribute("contenteditable") === "true"
  );
}

/** The text a person reads in `el`: its drawn text, never a control's value or a private subtree. */
function textOf(el: Element): string {
  let out = "";
  for (const node of Array.from(el.childNodes)) {
    if (node.nodeType === 3) out += node.textContent ?? "";
    else if (node.nodeType === 1) {
      const child = node as Element;
      if (isControl(child) || isPrivate(child) || !shown(child)) continue;
      if (child.tagName === "SVG" || child.tagName === "svg") {
        out += ` ${child.getAttribute("aria-label") ?? ""} `;
        continue;
      }
      const label = child.getAttribute("aria-label");
      out += ` ${label ?? textOf(child)} `;
    }
  }
  return out;
}

/** The text of the elements an id list names (`aria-labelledby`). */
function byIds(el: Element, ids: string | null): string {
  if (!ids) return "";
  const doc = el.ownerDocument;
  return ids
    .split(/\s+/)
    .map((id) => doc.getElementById(id))
    .filter((e): e is HTMLElement => e !== null && !isPrivate(e))
    .map((e) => e.getAttribute("aria-label") ?? textOf(e))
    .join(" ");
}

/** A field's label: its own naming, else the label that names it, else its placeholder or title. */
function labelOf(el: Element): string {
  const own = byIds(el, el.getAttribute("aria-labelledby")) || el.getAttribute("aria-label");
  if (own) return own;
  const labels = (el as HTMLInputElement).labels;
  if (labels && labels.length > 0) return Array.from(labels).map(textOf).join(" ");
  return el.getAttribute("placeholder") ?? el.getAttribute("title") ?? "";
}

/** An element's accessible name, from what names it or the text it draws. */
function nameOf(el: Element): string {
  return (
    byIds(el, el.getAttribute("aria-labelledby")) ||
    el.getAttribute("aria-label") ||
    textOf(el) ||
    el.getAttribute("title") ||
    ""
  );
}

/** A field's kind, and whether it is a field at all. */
function fieldKind(el: Element): AssistantViewFieldKind | null {
  const role = el.getAttribute("role");
  if (role === "combobox" || role === "listbox") return "choice";
  if (role === "switch" || role === "checkbox") return "check";
  if (el.tagName === "INPUT") {
    const input = el as HTMLInputElement;
    switch (input.type) {
      case "hidden":
      case "submit":
      case "button":
      case "reset":
      case "image":
        return null;
      case "password":
        return "secret";
      case "checkbox":
      case "radio":
        return "check";
      case "file":
        return "file";
    }
    const named = `${input.name} ${input.id}`.toLowerCase().split(/\s+/);
    if (SECRET_AUTOCOMPLETE.has(input.autocomplete) || named.some((n) => n && SECRET.test(n))) {
      return "secret";
    }
    return "text";
  }
  if (el.tagName === "TEXTAREA") return "text";
  if (el.tagName === "SELECT") return "choice";
  switch (role) {
    case "textbox":
    case "searchbox":
    case "spinbutton":
      return "text";
    case "combobox":
    case "listbox":
    case "radiogroup":
    case "slider":
      return "choice";
    case "checkbox":
    case "switch":
      return "check";
  }
  return null;
}

/** Whether a field holds a value, read without ever keeping the value. */
function filledOf(el: Element, kind: AssistantViewFieldKind): boolean {
  if (el.tagName === "INPUT") {
    const input = el as HTMLInputElement;
    if (kind === "check") return input.checked;
    if (kind === "file") return (input.files?.length ?? 0) > 0;
    return input.value !== "";
  }
  if (el.tagName === "TEXTAREA") return (el as HTMLTextAreaElement).value !== "";
  if (el.tagName === "SELECT") return (el as HTMLSelectElement).value !== "";
  const checked = el.getAttribute("aria-checked");
  if (checked !== null) return checked === "true";
  if (el.getAttribute("role") === "radiogroup") {
    return el.querySelector('[aria-checked="true"], input:checked') !== null;
  }
  const value = el.getAttribute("aria-valuetext") ?? el.getAttribute("aria-valuenow");
  if (value !== null) return value !== "";
  return (el.textContent ?? "").trim() !== "";
}

function disabledOf(el: Element): boolean {
  return (
    (el as HTMLButtonElement).disabled === true ||
    el.getAttribute("aria-disabled") === "true" ||
    el.closest("fieldset:disabled") !== null
  );
}

/** The error a field shows: what it names as its error, else its description while invalid. */
function errorOf(el: Element): string {
  if (el.getAttribute("aria-invalid") !== "true") return "";
  return (
    byIds(el, el.getAttribute("aria-errormessage")) ||
    byIds(el, el.getAttribute("aria-describedby")) ||
    "Invalid"
  );
}

const HEADING = /^H([1-6])$/;
/** What a person acts on inside a notice. */
const INTERACTIVE =
  'button, a[href], input, select, textarea, [role="button"], [role="link"], [role="checkbox"], [role="switch"], [role="combobox"]';
const NOTICE_ROLES = new Set(["status", "note", "log"]);

/** The outline as it is built: its entries and whether any were left out. */
interface Builder {
  nodes: AssistantViewNode[];
  trimmed: boolean;
  /** Elements already outlined (a dialog), skipped where the walk meets them again. */
  done: Set<Element>;
}

function push(b: Builder, node: AssistantViewNode): void {
  if (b.nodes.length >= ASSISTANT_PAGE_VIEW_MAX_NODES) {
    b.trimmed = true;
    return;
  }
  const name = clip(node.name, ASSISTANT_PAGE_VIEW_NAME_MAX_RUNES);
  if (name === "" && node.role !== "field") return;
  const out: AssistantViewNode = { ...node, name };
  if (out.error !== undefined) out.error = clip(out.error, ASSISTANT_PAGE_VIEW_ERROR_MAX_RUNES);
  b.nodes.push(out);
}

/** The entries for a list whose items are steps (one marked `aria-current="step"`). */
function steps(b: Builder, list: Element, depth: number): void {
  for (const item of Array.from(list.children)) {
    if (!shown(item) || isPrivate(item)) continue;
    const current =
      item.getAttribute("aria-current") === "step" ||
      item.querySelector('[aria-current="step"]') !== null;
    push(b, {
      role: "step",
      name: textOf(item),
      ...(depth ? { depth } : {}),
      ...(current ? { current } : {}),
    });
  }
}

/** Outline `el` and what it holds, at `depth` named sections deep. */
function walk(b: Builder, el: Element, depth: number): void {
  if (b.done.has(el) || !shown(el) || isPrivate(el) || el.tagName === "IFRAME") return;
  const role = el.getAttribute("role");
  const at = depth > 0 ? { depth: Math.min(depth, MAX_DEPTH) } : {};
  const heading = HEADING.exec(el.tagName);
  if (heading || role === "heading") {
    const level = Number(el.getAttribute("aria-level") ?? heading?.[1] ?? 2);
    push(b, { role: "heading", name: nameOf(el), level: Math.min(Math.max(level, 1), 6), ...at });
    return;
  }
  const kind = fieldKind(el);
  if (kind !== null) {
    const required =
      (el as HTMLInputElement).required || el.getAttribute("aria-required") === "true";
    const disabled = disabledOf(el);
    const error = errorOf(el);
    push(b, {
      role: "field",
      name: labelOf(el),
      field: kind,
      filled: filledOf(el, kind),
      ...(required ? { required } : {}),
      ...(disabled ? { disabled } : {}),
      ...(error ? { error } : {}),
      ...at,
    });
    return;
  }
  const button =
    el.tagName === "BUTTON" ||
    el.tagName === "SUMMARY" ||
    role === "button" ||
    (el.tagName === "INPUT" &&
      ["submit", "button", "reset"].includes((el as HTMLInputElement).type));
  if (role === "tab") {
    const current = el.getAttribute("aria-selected") === "true";
    push(b, { role: "tab", name: nameOf(el), ...(current ? { current } : {}), ...at });
    return;
  }
  if (button || role === "menuitem") {
    const name =
      el.tagName === "INPUT" ? ((el as HTMLInputElement).value ?? "") || nameOf(el) : nameOf(el);
    const disabled = disabledOf(el);
    push(b, { role: "button", name, ...(disabled ? { disabled } : {}), ...at });
    return;
  }
  if ((el.tagName === "A" && el.hasAttribute("href")) || role === "link") {
    const disabled = el.getAttribute("aria-disabled") === "true";
    push(b, { role: "link", name: nameOf(el), ...(disabled ? { disabled } : {}), ...at });
    return;
  }
  if (role === "alert" || role === "alertdialog") {
    push(b, { role: "alert", name: textOf(el), ...at });
    for (const control of Array.from(el.querySelectorAll(INTERACTIVE))) walk(b, control, depth);
    return;
  }
  // A NOTICE is a short status line or a callout; a long live region is a
  // region that updates, outlined like any other.
  const live = el.getAttribute("aria-live");
  const text = textOf(el);
  if (
    el.classList.contains("crewlet-callout") ||
    (((role !== null && NOTICE_ROLES.has(role)) || (live !== null && live !== "off")) &&
      [...fold(text)].length <= ASSISTANT_PAGE_VIEW_NAME_MAX_RUNES)
  ) {
    push(b, { role: "notice", name: text, ...at });
    for (const control of Array.from(el.querySelectorAll(INTERACTIVE))) walk(b, control, depth);
    return;
  }
  if (
    (el.tagName === "OL" || el.tagName === "UL" || role === "list") &&
    el.querySelector('[aria-current="step"]') !== null
  ) {
    steps(b, el, depth);
    return;
  }
  // A NAMED REGION is a section of the outline, and what it holds sits one deeper.
  const legend = el.tagName === "FIELDSET" ? el.querySelector(":scope > legend") : null;
  const sectionName =
    el.tagName === "SECTION" ||
    el.tagName === "FIELDSET" ||
    el.tagName === "FORM" ||
    role === "region" ||
    role === "group" ||
    role === "form" ||
    role === "tabpanel"
      ? byIds(el, el.getAttribute("aria-labelledby")) ||
        el.getAttribute("aria-label") ||
        (legend ? textOf(legend) : "")
      : "";
  let inner = depth;
  if (sectionName.trim() !== "") {
    push(b, { role: "section", name: sectionName, ...at });
    inner = depth + 1;
  }
  for (const child of Array.from(el.children)) {
    if (child === legend) continue;
    walk(b, child, inner);
  }
}

/** The dialogs open now, outside `skip` (the assistant's own panel). */
function openDialogs(doc: Document, skip: Element | null): Element[] {
  return Array.from(
    doc.querySelectorAll('dialog[open], [role="dialog"], [role="alertdialog"]'),
  ).filter((d) => shown(d) && !isPrivate(d) && !(skip && skip.contains(d)));
}

/**
 * The outline of `main`, the screen's own pane, with any dialog open over it
 * first; `skip` is the assistant's own panel, never outlined. Held to the
 * outline's bounds, its entries past them left out and `trimmed` said.
 */
export function buildPageView(
  main: Element | null,
  skip: Element | null = null,
): AssistantPageView {
  const b: Builder = { nodes: [], trimmed: false, done: new Set() };
  const doc = main?.ownerDocument ?? (typeof document === "undefined" ? null : document);
  if (doc) {
    for (const dialog of openDialogs(doc, skip)) {
      push(b, { role: "dialog", name: nameOf(dialog) || "Dialog" });
      b.done.add(dialog);
      for (const child of Array.from(dialog.children)) walk(b, child, 1);
    }
  }
  if (main && !(skip && skip.contains(main))) {
    for (const child of Array.from(main.children)) {
      if (skip && child.contains(skip)) continue;
      walk(b, child, 0);
    }
  }
  const view: AssistantPageView = { nodes: b.nodes };
  if (b.trimmed) view.trimmed = true;
  while (view.nodes.length > 0 && wireBytes(view) > ASSISTANT_PAGE_VIEW_MAX_BYTES) {
    view.nodes.pop();
    view.trimmed = true;
  }
  return view;
}

/**
 * `context` with an outline of the screen as it is drawn now ([buildPageView]),
 * read when a question or a report is sent rather than on every publish,
 * since the drawn page moves without the registry hearing. The outline is
 * shed from its end, and marked `trimmed`, where the whole would not fit.
 */
export function withView(context: AssistantPageContext): AssistantPageContext {
  if (typeof document === "undefined") return context;
  const view = buildPageView(
    document.getElementById(SCREEN_SCROLL_ID),
    document.querySelector(".crewlet-app-shell__aside"),
  );
  const out: AssistantPageContext = { ...context, view };
  while (!pageContextFits(out) && view.nodes.length > 0) {
    view.nodes.pop();
    view.trimmed = true;
  }
  return pageContextFits(out) ? out : context;
}
