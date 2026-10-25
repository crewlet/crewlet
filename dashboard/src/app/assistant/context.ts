/**
 * What the page under the assistant tells it about itself, and the actions
 * the page will take from it.
 *
 * # Identifiers, never content
 *
 * A page context names the screen, the route's own parameters and query, the
 * records in view by kind and id, what is selected, and a revision. It never
 * carries a row's contents: what a record says is read by the assistant
 * through the person's own access, one request at a time, so nothing a screen
 * happened to hold reaches a model by a side door. The engine reads the
 * context as a hint about what the person is looking at, never as permission.
 *
 * # Two layers
 *
 * The FRAME publishes what every page has ([setFramePage]): which screen, the
 * crumbs it is called by, and the address. A SCREEN that knows more registers
 * it ([registerPage], through `useAssistantContext`): the records in view, the
 * selection, the version of the data it drew, and the page actions it will
 * take. A screen that registers nothing still has a context, and an action it
 * never declared is answered `no_handler`.
 *
 * # Bounded on the way out
 *
 * [pageContextNow] holds what it builds to the engine's own rules
 * (`internal/assistant/wire.PageContext.Validate`): a key that is not a name,
 * a value that is not plain text, a title over its length or anything past a
 * count is left out rather than sent to be refused after the person pressed
 * Send, and a context still over its byte budget sheds its query and then its
 * resources, which the engine's `page_context` frame reports as `trimmed`.
 *
 * # Page actions
 *
 * An action is the SCREEN'S: its own parser turns what the model proposed
 * into a typed value or refuses it, its own gate applies the rule its control
 * applies (a locked filter is never widened), and its own setters apply it.
 * The bus carries the request and the result (`bus.ts`), and [applyPageAction]
 * is the registry's half: the current registration, a fresh revision, and an
 * outcome for every way it can go.
 */

import { useSyncExternalStore } from "react";
import {
  ASSISTANT_ACTION_CONTEXT_INVALIDATE,
  ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS,
  ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES,
  type AssistantPageContext,
} from "~/contract/assistant.ts";
import { pageContextFits } from "~/protocol/assistantWire.ts";
import { resolve } from "../routes.ts";
import type { Crumb } from "../crumbs.ts";
import { emit, type AssistantBusEvents, type AssistantInvalidateReason } from "./bus.ts";

/** A resource or a selection, as the wire names it. */
type ResourceKind = AssistantPageContext["resources"][number]["kind"];

/**
 * One thing in view, by kind and id, with the version the screen drew: the
 * contract's own shape (`contract/assistant.ts`), never a second copy of it.
 */
export type PageResource = AssistantPageContext["resources"][number];

/** What a page action's parser answers: a typed value, or why it is refused. */
export type PageActionParse<P> = { ok: true; value: P } | { ok: false; reason: string };

/**
 * One action a screen takes from the assistant. Every half is the screen's:
 * see the file's doc.
 */
export interface PageActionSpec<P> {
  /** What the action touches, as the person reads it: "Work list filters (not saved)". */
  scope: string;
  /** Turn what the model proposed into a typed value, or refuse it. */
  parse: (raw: unknown) => PageActionParse<P>;
  /** Each field the action changes: its name, the new value and the current one. */
  preview: (value: P) => readonly { field: string; next: string; current: string | null }[];
  /** The screen's own gate, as its control applies it: a sentence, or null where it may. */
  blocked: (value: P) => string | null;
  /** Apply it with the setters the screen already has. */
  apply: (value: P) => void;
  /** Whether the person approves it first (D11): a pure view change may say `none`. */
  confirm?: "approve" | "none";
}

/** What a screen registers beyond what the frame knows. */
export interface AssistantPageRegistration {
  /** The records in view, by kind and id. */
  resources?: readonly PageResource[];
  /** What is selected, if one thing is. */
  selection?: { kind: ResourceKind; id: string } | null;
  /** The version of the data the screen drew, so a change prepared against it can be judged stale. */
  dataVersion?: string;
  /** The actions this screen takes, by id (`work.filters.apply`). */
  actions?: Readonly<Record<string, PageActionSpec<never>>>;
}

/** What the frame publishes about every page. */
export interface FramePage {
  /** The route's screen name (`work-tasks`). */
  screen: string;
  /** The page as its attachment chip names it: the last two crumbs, "Work › Tasks". */
  title: string;
  /** The page's address, which identifies it for the chip's "another page" rule. */
  path: readonly string[];
  /** The route's own parameters (a project's key, a seat's handle). */
  params: Readonly<Record<string, string>>;
  /** The address's query, the URL state the screen reads. */
  query: Readonly<Record<string, string>>;
}

/**
 * The page the frame is on, as it publishes it: the route's screen and its
 * own parameters, the address's query, and the last two crumbs as the chip's
 * title ("Work › Tasks", "Tasks › ENG-156"), the way an Open in app card names
 * a place. Null for an address the product has no framed page for.
 */
export function framePageOf(
  path: readonly string[],
  query: URLSearchParams,
  crumbs: readonly Crumb[],
): FramePage | null {
  const where = resolve([...path]);
  if (!where.resolved || where.workspace === "") return null;
  const params: Record<string, string> = {};
  for (const [key, value] of Object.entries(where)) {
    if (key === "resolved" || key === "workspace" || key === "screen") continue;
    if (typeof value === "string") params[key] = value;
    else if (Array.isArray(value)) params[key] = value.join("/");
  }
  return {
    screen: where.screen,
    title: crumbs
      .slice(-2)
      .map((c) => c.label)
      .join(" › "),
    path,
    params,
    query: Object.fromEntries(query),
  };
}

/** The context as the drawer reads it: the chip, and the wire value it stands for. */
export interface PageSnapshot {
  /** The page's identity for the chip: a new one is "another page". */
  key: string;
  /** What the chip says. */
  title: string;
  /** What a turn sends, already held to the engine's bounds. */
  context: AssistantPageContext;
}

const SCREEN = /^[a-z][a-z0-9-]{0,63}$/;
const KEY = /^[a-z][a-z0-9_.]{0,63}$/;
const ACTION = /^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$/;
const TITLE_MAX_RUNES = 120;
const VALUE_MAX_BYTES = 512;
const ID_MAX_BYTES = 200;
const MAP_MAX_KEYS = 32;
const ACTION_MAX_BYTES = 64;

let frame: FramePage | null = null;
let registration: AssistantPageRegistration | null = null;
let snapshot: PageSnapshot | null = null;
const listeners = new Set<() => void>();

const encoder = new TextEncoder();

function bytes(text: string): number {
  return encoder.encode(text).length;
}

/** Whether a value is text the engine takes as an id or a value: bounded, no control character. */
function plain(text: string, max: number): boolean {
  return bytes(text) <= max && !/[\u0000-\u001f\u007f-\u009f]/.test(text);
}

/** A map of names to plain values, held to the engine's key rule and counts. */
function plainMap(map: Readonly<Record<string, string>>): Record<string, string> {
  const out: Record<string, string> = {};
  let n = 0;
  for (const [key, value] of Object.entries(map).sort(([a], [b]) => a.localeCompare(b))) {
    if (n >= MAP_MAX_KEYS) break;
    if (!KEY.test(key) || !plain(value, VALUE_MAX_BYTES)) continue;
    out[key] = value;
    n += 1;
  }
  return out;
}

/** The title, cut to the engine's bound at a whole character. */
function title(text: string): string {
  const runes = [...text.replace(/[\u0000-\u001f\u007f-\u009f]/g, " ")];
  return runes.length <= TITLE_MAX_RUNES
    ? runes.join("")
    : runes.slice(0, TITLE_MAX_RUNES).join("");
}

/**
 * A stable, bounded name for what the page shows now: the same page with the
 * same data is the same revision, and any change to its address, its
 * selection or its data is another one. FNV-1a, because a revision is
 * compared and never read.
 */
function revisionOf(page: FramePage, reg: AssistantPageRegistration | null): string {
  const parts = [
    page.screen,
    page.path.join("/"),
    Object.entries(page.query)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([k, v]) => `${k}=${v}`)
      .join("&"),
    reg?.selection ? `${reg.selection.kind}:${reg.selection.id}` : "",
    reg?.dataVersion ?? "",
  ].join("\u0000");
  let hash = 0x811c9dc5;
  for (let i = 0; i < parts.length; i++) {
    hash ^= parts.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return `${page.screen}@${hash.toString(16).padStart(8, "0")}`;
}

/** The wire context for a page and its registration, held to the engine's bounds. */
export function buildPageContext(
  page: FramePage,
  reg: AssistantPageRegistration | null,
): AssistantPageContext {
  const resources = (reg?.resources ?? [])
    .filter((r) => r.id !== "" && plain(r.id, ID_MAX_BYTES))
    .filter((r) => r.version === undefined || plain(r.version, ID_MAX_BYTES))
    .slice(0, ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES)
    .map((r) => (r.version === undefined ? { kind: r.kind, id: r.id } : { ...r }));
  const selection =
    reg?.selection && reg.selection.id !== "" && plain(reg.selection.id, ID_MAX_BYTES)
      ? { kind: reg.selection.kind, id: reg.selection.id }
      : null;
  // AN ACTION ID IS DOTTED LOWERCASE SEGMENTS, and never the one the engine
  // reserves for itself (`context.invalidate`), as `wire.ValidActionID` holds.
  const accepts = Object.keys(reg?.actions ?? {})
    .filter(
      (id) =>
        bytes(id) <= ACTION_MAX_BYTES &&
        ACTION.test(id) &&
        id !== ASSISTANT_ACTION_CONTEXT_INVALIDATE,
    )
    .slice(0, ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS);
  const context: AssistantPageContext = {
    screen: SCREEN.test(page.screen) ? page.screen : "unknown",
    title: title(page.title),
    params: plainMap(page.params),
    query: plainMap(page.query),
    resources,
    selection,
    source_revision: revisionOf(page, reg),
    accepts,
  };
  // OVER ITS BYTE BUDGET, it sheds what matters least first: the query (the
  // screen's resources still say what is in view), then the resources from
  // the end. The engine's `page_context` frame says `trimmed`.
  if (!pageContextFits(context)) context.query = {};
  while (!pageContextFits(context) && context.resources.length > 0) context.resources.pop();
  return context;
}

/** Rebuild the snapshot and tell every listener, and the bus when it moved. */
function publish(reason: AssistantInvalidateReason | null): void {
  const before = snapshot?.context.source_revision ?? null;
  snapshot = frame
    ? {
        key: `${frame.screen}:${frame.path.join("/")}`,
        title: frame.title,
        context: buildPageContext(frame, registration),
      }
    : null;
  for (const listener of listeners) listener();
  const after = snapshot?.context.source_revision ?? null;
  if (reason !== null && after !== null && after !== before) {
    emit("assistant.context.invalidate", { reason, revision: after });
  }
}

/** Which kind of change took the page from `a` to `b`. */
function whatMoved(a: FramePage | null, b: FramePage): AssistantInvalidateReason {
  if (a === null || a.screen !== b.screen || a.path.join("/") !== b.path.join("/")) {
    return "route";
  }
  return "filters";
}

/** The frame publishes the page on screen; null where there is none (signed out). */
export function setFramePage(page: FramePage | null): void {
  const reason = page === null ? null : whatMoved(frame, page);
  frame = page;
  publish(reason);
}

/**
 * A screen registers what it knows; the function it is handed back withdraws
 * it. The screen registered last wins, so a page's own registration replaces
 * the one the screen it replaced made.
 */
export function registerPage(next: AssistantPageRegistration): () => void {
  const before = registration;
  registration = next;
  const reason: AssistantInvalidateReason =
    before?.dataVersion !== next.dataVersion ? "data" : "selection";
  publish(reason);
  return () => {
    if (registration !== next) return;
    registration = null;
    publish(null);
  };
}

/** The page as the drawer reads it now, or null where the frame has published none. */
export function pageSnapshot(): PageSnapshot | null {
  return snapshot;
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** The page as a value a component re-renders on. */
export function usePageSnapshot(): PageSnapshot | null {
  return useSyncExternalStore(subscribe, pageSnapshot, () => null);
}

/**
 * Apply a page action the drawer claimed, through the screen's own handler,
 * and say what became of it: `no_handler` where the screen declares no such
 * action, `stale` where the page moved since the change was prepared,
 * `invalid` where the screen's parser refuses it, `blocked` where its gate
 * does, and `applied` once its setters ran.
 */
export function applyPageAction(
  request: AssistantBusEvents["assistant.action.apply"],
): AssistantBusEvents["assistant.action.result"] {
  const now = snapshot?.context.source_revision ?? "";
  const result = (
    outcome: AssistantBusEvents["assistant.action.result"]["outcome"],
    reason?: string,
  ): AssistantBusEvents["assistant.action.result"] => ({
    correlation_id: request.correlation_id,
    outcome,
    revision: now,
    ...(reason === undefined ? {} : { reason }),
  });
  const spec = registration?.actions?.[request.action] as PageActionSpec<unknown> | undefined;
  if (!spec) return result("no_handler");
  if (request.source_revision !== now) return result("stale");
  const parsed = spec.parse(request.payload);
  if (!parsed.ok) return result("invalid", parsed.reason);
  const blocked = spec.blocked(parsed.value);
  if (blocked !== null) return result("blocked", blocked);
  spec.apply(parsed.value);
  return result("applied");
}

/** Test seam: the registry as a fresh page has it. */
export function resetPageContextForTest(): void {
  frame = null;
  registration = null;
  snapshot = null;
}
