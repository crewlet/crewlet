/**
 * What the page under the assistant tells it about itself, and the actions
 * the page will take from it.
 *
 * # Identifiers, never content, but the open draft
 *
 * A page context names the screen, the route's own parameters and query, the
 * records in view by kind and id, the one record the person has open (its
 * selection, with what the screen calls it), and a revision. It never
 * carries a row's contents: what a record says is read by the assistant
 * through the person's own access, one request at a time, so nothing a screen
 * happened to hold reaches a model by a side door. The engine reads the
 * context as a hint about what the person is looking at, never as permission.
 *
 * The one exception is the draft the person has open (`draft.ts`): what they
 * typed and have not saved exists nowhere a tool could read it, so a screen
 * with a draft sends its values, redacted and bounded, and the assistant reads
 * what is open before it searches for it.
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
  ASSISTANT_PAGE_ACTIONS,
  ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS,
  ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES,
  type AssistantActionArgs,
  type AssistantDraftFileRef,
  type AssistantPageContext,
  type AssistantPageDraft,
  type AssistantRecordNoun,
  type AssistantResourceRef,
} from "~/contract/assistant.ts";
import { pageContextFits } from "~/protocol/assistantWire.ts";
import { resolve } from "../routes.ts";
import type { Crumb } from "../crumbs.ts";
import { LENS_LABELS, scopeAt } from "../scope.ts";
import { parseRef, type ObjectKind, type ObjectRef } from "../frame/objects.ts";
import { emit, on, type AssistantBusEvents, type AssistantInvalidateReason } from "./bus.ts";

/** A resource or a selection, as the wire names it. */
type ResourceKind = AssistantPageContext["resources"][number]["kind"];

/**
 * One thing in view, by kind and id, with the version the screen drew: the
 * contract's own shape (`contract/assistant.ts`), never a second copy of it.
 */
export type PageResource = AssistantPageContext["resources"][number];

/**
 * A frame object's kind as the wire names it, or null for a kind the wire
 * has no name for (an event, a node, a credential): a screen's object is
 * named once, here, rather than each screen spelling the wire's word.
 */
const WIRE_KIND: Record<ObjectKind, ResourceKind | null> = {
  item: "task",
  project: "project",
  seat: "seat",
  unit: "unit",
  page: "page",
  container: "container",
  turn: "turn",
  run: "run",
  schedule: "schedule",
  event: null,
  node: null,
  channel: null,
  tool: null,
  integration: null,
  credential: null,
  revision: null,
  person: null,
  notice: null,
};

/** A frame object as a page resource, or null where the wire has no name for its kind. */
export function resourceOf(ref: ObjectRef | null | undefined): PageResource | null {
  const kind = ref ? WIRE_KIND[ref.kind] : null;
  return ref && kind ? { kind, id: ref.id } : null;
}

/**
 * What a person calls a record of each kind, where its kind alone says it: a
 * seat is an agent. A screen that knows better says so itself (an ask in the
 * Inbox is a decision); a kind with no record noun on the wire (a team, a
 * space) is named by the drawer from its kind.
 */
const NOUN_OF_KIND: Partial<Record<ResourceKind, AssistantRecordNoun>> = {
  task: "task",
  project: "project",
  seat: "agent",
  page: "page",
  artifact: "artifact",
  turn: "turn",
  run: "run",
  room: "room",
};

/** A selection with its noun: the screen's own, or its kind's. */
function withNoun(ref: {
  kind: ResourceKind;
  id: string;
  noun?: AssistantRecordNoun;
}): AssistantResourceRef {
  const noun = ref.noun ?? NOUN_OF_KIND[ref.kind];
  return noun === undefined ? { kind: ref.kind, id: ref.id } : { kind: ref.kind, id: ref.id, noun };
}

/**
 * The record a record's own screen is of, by the route parameter that names
 * it: a task's page is that task, a seat's page that agent, a Chatlet room's
 * page that room, which a brief then cites. A list screen is of no one record
 * and answers null.
 */
const SUBJECT_OF_SCREEN: Readonly<Record<string, { kind: ResourceKind; param: string }>> = {
  item: { kind: "task", param: "id" },
  project: { kind: "project", param: "key" },
  seat: { kind: "seat", param: "handle" },
  page: { kind: "page", param: "id" },
  artifact: { kind: "artifact", param: "id" },
  turn: { kind: "turn", param: "id" },
  executors: { kind: "run", param: "id" },
  chatlet: { kind: "room", param: "id" },
};

/** The record `page` is the screen of, or null. */
function subjectOf(page: FramePage): { kind: ResourceKind; id: string } | null {
  const subject = SUBJECT_OF_SCREEN[page.screen];
  const id = subject ? page.params[subject.param] : undefined;
  return subject && id ? { kind: subject.kind, id } : null;
}

/**
 * One row of a page action's card: the field, its proposed value and its
 * value now, and for a file the reference the card draws a thumbnail of.
 */
export interface PageActionRow {
  field: string;
  next: string;
  current: string | null;
  /** `next` as its items, where the field is a list: the card draws a list as a list. */
  items?: readonly string[];
  /** `current` as its items, likewise. */
  currentItems?: readonly string[];
  file?: AssistantDraftFileRef;
  /** The ground `file` is previewed on, as the screen declares it; light where absent. */
  fileGround?: { tone: "light" | "dark"; color?: string };
}

/** What a page action's parser answers: a typed value, or why it is refused. */
export type PageActionParse<P> = { ok: true; value: P } | { ok: false; reason: string };

/**
 * One action a screen takes from the assistant. Every half is the screen's:
 * see the file's doc.
 *
 * DECLARED AS METHODS, so a screen's typed action (`PageActionSpec<UrlPatch>`)
 * registers beside every other as `PageActionSpec<unknown>`: the registry
 * only ever hands a value back to the spec whose own parser produced it.
 */
export interface PageActionSpec<P> {
  /** What the action touches, as the person reads it: "Worklet list filters (not saved)". */
  scope: string;
  /**
   * What the action takes, as the screen takes it NOW: built from the same
   * declaration `parse` reads (the keys a URL action writes, the seats a
   * screen draws), so the two cannot disagree. Sent in the page context's
   * `action_args`; the model reads it, and the engine holds every proposal to
   * it before a card is drawn, so a call the screen could only refuse never
   * reaches the person.
   */
  args: AssistantActionArgs;
  /**
   * What a change by this action depends on, beyond the screen and its path:
   * the address's query, the selection, the screen's data. All three where
   * unset. A change goes stale only when one of these moves, so an action
   * that re-checks what it edits when it applies (a ceiling for its editor)
   * tracks less, and is not refused because the person opened a peek beside it.
   */
  tracks?: readonly PageRevisionPart[];
  /** Turn what the model proposed into a typed value, or refuse it. */
  parse(raw: unknown): PageActionParse<P>;
  /** Each field the action changes: its name, the new value and the current one. */
  preview(value: P): readonly PageActionRow[];
  /** The screen's own gate, as its control applies it: a sentence, or null where it may. */
  blocked(value: P): string | null;
  /** Apply it with the setters the screen already has. */
  apply(value: P): void;
  /**
   * What puts the page back, taken BEFORE it applies: a function that undoes
   * this one change and says whether it did. It answers false, and changes
   * nothing, once the page no longer shows what the change made of it, so an
   * undo never reverts something the person changed since. A pure view change
   * offers it, so its receipt can say Undo (D11).
   */
  undo?(value: P): () => boolean;
}

/** A part of the page a change's revision may track (see [PageActionSpec.tracks]). */
export type PageRevisionPart = "query" | "selection" | "data";

/** Every part, which an action that names none tracks. */
const ALL_PARTS: readonly PageRevisionPart[] = ["query", "selection", "data"];

/**
 * A page action's id: one of the contract's (`ASSISTANT_PAGE_ACTIONS`), which
 * also says whether it asks the person's approval first, so no screen states
 * that a second time.
 */
export type PageActionId = keyof typeof ASSISTANT_PAGE_ACTIONS;

/** Whether an id off the wire, or off a registration, is one of the contract's page actions. */
export function isPageActionId(id: string): id is PageActionId {
  return Object.hasOwn(ASSISTANT_PAGE_ACTIONS, id);
}

/** What a screen registers beyond what the frame knows. */
export interface AssistantPageRegistration {
  /** The records in view, by kind and id. */
  resources?: readonly PageResource[];
  /**
   * The one record the person has open, with what the screen calls it where
   * its kind does not say ("decision" for an ask). Unset, it is the frame's
   * peek, or on a record's own screen that record, so a screen that registers
   * nothing still says what is open; null says nothing is.
   */
  selection?: { kind: ResourceKind; id: string; noun?: AssistantRecordNoun } | null;
  /** The version of the data the screen drew, so a change prepared against it can be judged stale. */
  dataVersion?: string;
  /** The actions this screen takes, by their contract id (`work.filter.apply`). */
  actions?: Readonly<Partial<Record<PageActionId, PageActionSpec<unknown>>>>;
  /** The draft the screen has open, already redacted and bounded (`useAssistantDraft`). */
  draft?: AssistantPageDraft;
}

/** What the frame publishes about every page. */
export interface FramePage {
  /** The route's screen name (`work-tasks`). */
  screen: string;
  /** The page as its attachment chip names it: the last two crumbs, "Worklet › Tasks". */
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
 * title ("Worklet › Tasks", "Tasks › ENG-156"), the way an Open in app card names
 * a place. On Worklet's scope pages the trail ends in the scope and the lens is a
 * tab, so the lens takes the scope's place: "Worklet › Tasks" across all projects,
 * "Core platform › Board" in one, and the chip on the Tasks tab never names
 * only the scope. Null for an address the product has no framed page for.
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
  const at = crumbs.at(-1)?.switch ? scopeAt([...path], query) : null;
  const trail = crumbs.map((c) => c.label);
  if (at) {
    // ACROSS ALL PROJECTS the scope adds nothing the workspace does not say;
    // in one project, the project is what the chip names the lens of.
    const lead = at.scope.kind === "all" ? trail[0] : trail.at(-1);
    trail.splice(0, trail.length, lead ?? "", LENS_LABELS[at.lens]);
  }
  return {
    screen: where.screen,
    title: trail.slice(-2).join(" › "),
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
const TITLE_MAX_RUNES = 120;
const VALUE_MAX_BYTES = 512;
const ID_MAX_BYTES = 200;
const MAP_MAX_KEYS = 32;

let frame: FramePage | null = null;
/** Every mounted component's registration, oldest first. */
let registrations: AssistantPageRegistration[] = [];
/** What they say together, rebuilt whenever one comes or goes ([merged]). */
let registration: AssistantPageRegistration | null = null;
let snapshot: PageSnapshot | null = null;
const listeners = new Set<() => void>();
/**
 * The undo each applied action left, by its correlation id. Held while the
 * page stays on the same screen: an address that moves to another screen
 * takes its undos with it, since the change they put back was that screen's.
 */
const undos = new Map<string, () => boolean>();

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

/** A short, stable name for a text: FNV-1a, because it is compared and never read. */
export function hashOf(text: string): string {
  let hash = 0x811c9dc5;
  for (let i = 0; i < text.length; i++) {
    hash ^= text.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash.toString(16).padStart(8, "0");
}

/**
 * A stable, bounded name for what the page shows now: the same page with the
 * same data is the same revision, and any change to the parts it tracks is
 * another one. The page's own revision tracks every part; an action's tracks
 * what it depends on ([PageActionSpec.tracks]).
 */
function revisionOf(
  page: FramePage,
  reg: AssistantPageRegistration | null,
  tracks: readonly PageRevisionPart[] = ALL_PARTS,
): string {
  const parts = [
    page.screen,
    page.path.join("/"),
    tracks.includes("query")
      ? Object.entries(page.query)
          // THE PEEK IS THE SELECTION'S, not the view's: an action that
          // tracks the address and not the selection is not moved by the
          // person opening a row beside it.
          .filter(([k]) => k !== "peek" || tracks.includes("selection"))
          .sort(([a], [b]) => a.localeCompare(b))
          .map(([k, v]) => `${k}=${v}`)
          .join("&")
      : "",
    tracks.includes("selection") && reg?.selection
      ? `${reg.selection.kind}:${reg.selection.id}`
      : "",
    tracks.includes("data") ? (reg?.dataVersion ?? "") : "",
  ].join("\u0000");
  return `${page.screen}@${hashOf(parts)}`;
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
  // WHAT IS OPEN, in the order a person sees it: what the screen says it has
  // open, else the row peeked over it, else the record the screen is of.
  const chosen =
    reg?.selection !== undefined
      ? reg.selection
      : (resourceOf(parseRef(page.query.peek)) ?? subjectOf(page));
  const selection =
    chosen && chosen.id !== "" && plain(chosen.id, ID_MAX_BYTES) ? withNoun(chosen) : null;
  // ONLY THE CONTRACT'S IDS, which the engine's catalogue holds too (its
  // client gate keeps the two equal): an id the engine does not know is one
  // every proposal for would be refused, so it is never offered.
  const accepts = Object.keys(reg?.actions ?? {})
    .filter(isPageActionId)
    .slice(0, ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS);
  // WHAT EACH TAKES AND WHAT EACH DEPENDS ON, by its id: the engine holds a
  // proposal to the first and binds it to the second.
  const actionArgs: Record<string, AssistantActionArgs> = {};
  const actionRevisions: Record<string, string> = {};
  for (const id of accepts) {
    const spec = reg?.actions?.[id];
    if (!spec) continue;
    actionArgs[id] = spec.args;
    actionRevisions[id] = revisionOf(page, reg, spec.tracks ?? ALL_PARTS);
  }
  const context: AssistantPageContext = {
    screen: SCREEN.test(page.screen) ? page.screen : "unknown",
    title: title(page.title),
    params: plainMap(page.params),
    query: plainMap(page.query),
    resources,
    selection,
    source_revision: revisionOf(page, reg),
    accepts,
    ...(accepts.length === 0 ? {} : { action_args: actionArgs, action_revisions: actionRevisions }),
  };
  if (reg?.draft) context.draft = { ...reg.draft, fields: [...reg.draft.fields] };
  // OVER ITS BYTE BUDGET, it sheds what matters least first: the query (the
  // screen's resources still say what is in view), then the resources from
  // the end, then the open draft's fields from its end, each counted in its
  // `more`, and last its help. The engine's `page_context` frame says
  // `trimmed`.
  if (!pageContextFits(context)) context.query = {};
  while (!pageContextFits(context) && context.resources.length > 0) context.resources.pop();
  const draft = context.draft;
  while (draft && !pageContextFits(context) && draft.fields.length > 0) {
    draft.fields.pop();
    draft.more += 1;
  }
  if (draft && !pageContextFits(context)) delete draft.help;
  return context;
}

/** Rebuild the snapshot and tell every listener, and the bus when it moved. */
function publish(reason: AssistantInvalidateReason | null): void {
  const before = snapshot?.context.source_revision ?? null;
  const beforeActions = JSON.stringify(snapshot?.context.action_revisions ?? {});
  snapshot = frame
    ? {
        key: `${frame.screen}:${frame.path.join("/")}`,
        title: frame.title,
        context: buildPageContext(frame, registration),
      }
    : null;
  for (const listener of listeners) listener();
  const after = snapshot?.context.source_revision ?? null;
  const revisions = snapshot?.context.action_revisions ?? {};
  if (
    reason !== null &&
    after !== null &&
    (after !== before || JSON.stringify(revisions) !== beforeActions)
  ) {
    emit("assistant.context.invalidate", { reason, revision: after, revisions: { ...revisions } });
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
  if (reason !== "filters") undos.clear();
  frame = page;
  publish(reason);
}

/**
 * What every mounted registration says together.
 *
 * A PAGE IS SEVERAL COMPONENTS: a project's screen offers its lenses and the
 * task list inside it offers the list's filters, each registering what it
 * knows. So the registry merges them rather than keeping the last: the
 * resources of all of them (each record once), the actions of all of them
 * (the newest wins where two declare one id), the newest selection and the
 * newest draft anyone declared, and every data version, so a move in any of them is a move of
 * the page.
 */
function merged(all: readonly AssistantPageRegistration[]): AssistantPageRegistration | null {
  if (all.length === 0) return null;
  const resources: PageResource[] = [];
  const seen = new Set<string>();
  const actions: Record<string, PageActionSpec<unknown>> = {};
  let selection: AssistantPageRegistration["selection"];
  let draft: AssistantPageDraft | undefined;
  const versions: string[] = [];
  for (const reg of all) {
    for (const r of reg.resources ?? []) {
      const id = `${r.kind}:${r.id}`;
      if (seen.has(id)) continue;
      seen.add(id);
      resources.push(r);
    }
    Object.assign(actions, reg.actions ?? {});
    if (reg.selection !== undefined) selection = reg.selection;
    if (reg.draft) draft = reg.draft;
    if (reg.dataVersion) versions.push(reg.dataVersion);
  }
  return {
    resources,
    actions,
    ...(selection === undefined ? {} : { selection }),
    ...(draft === undefined ? {} : { draft }),
    ...(versions.length === 0 ? {} : { dataVersion: versions.join("|") }),
  };
}

/**
 * A component registers what it knows; the function it is handed back
 * withdraws it. Every mounted registration counts ([merged]), so the screen
 * and the components inside it each say their own part.
 */
export function registerPage(next: AssistantPageRegistration): () => void {
  const before = registration;
  registrations = [...registrations, next];
  registration = merged(registrations);
  const reason: AssistantInvalidateReason =
    before?.dataVersion !== registration?.dataVersion ? "data" : "selection";
  publish(reason);
  return () => {
    if (!registrations.includes(next)) return;
    registrations = registrations.filter((r) => r !== next);
    registration = merged(registrations);
    publish(null);
  };
}

/**
 * The revision a change by `action` is judged against now: the action's own,
 * or the page's where the page does not take it. What a claim reports and
 * what the screen compares before applying.
 */
export function actionRevision(action: string): string {
  const context = snapshot?.context;
  return context?.action_revisions?.[action] ?? context?.source_revision ?? "";
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

/** How long the page must stay unchanged before a changed screen counts as settled. */
const SETTLE_QUIET_MS = 50;
/** The longest a settled screen is waited for, however often the page republishes. */
const SETTLE_MAX_MS = 400;

/**
 * The page's context once the screen settles after a change it just applied:
 * a screen's setters re-render it and its draft republishes after the
 * render, so the context read at once would describe the draft before the
 * change. Resolves once the page has been quiet for a moment, and never later
 * than a bound, with the context then (null where the frame published none).
 */
export function settledPageContext(): Promise<AssistantPageContext | null> {
  return new Promise((resolve) => {
    let quiet: ReturnType<typeof setTimeout>;
    const done = (): void => {
      clearTimeout(quiet);
      clearTimeout(cap);
      off();
      resolve(snapshot ? snapshot.context : null);
    };
    const cap = setTimeout(done, SETTLE_MAX_MS);
    const rearm = (): void => {
      clearTimeout(quiet);
      quiet = setTimeout(done, SETTLE_QUIET_MS);
    };
    const off = subscribe(rearm);
    rearm();
  });
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
  const result = (
    outcome: AssistantBusEvents["assistant.action.result"]["outcome"],
    extra: { reason?: string; receipt?: string; undoable?: boolean } = {},
  ): AssistantBusEvents["assistant.action.result"] => ({
    correlation_id: request.correlation_id,
    outcome,
    revision: actionRevision(request.action),
    ...extra,
  });
  const spec = isPageActionId(request.action) ? registration?.actions?.[request.action] : undefined;
  if (!spec) return result("no_handler", { reason: "This page does not take that change." });
  // A FRESH REVISION, read at the moment of applying: what the page shows
  // now, never what it showed when the change was prepared or approved, and
  // only of what this action depends on.
  if (request.source_revision !== actionRevision(request.action)) {
    return result("stale", { reason: "The page changed since this was prepared." });
  }
  const parsed = spec.parse(request.payload);
  if (!parsed.ok) return result("invalid", { reason: parsed.reason });
  const blocked = spec.blocked(parsed.value);
  if (blocked !== null) return result("blocked", { reason: blocked });
  const title = snapshot?.title ?? "";
  const undo = spec.undo?.(parsed.value);
  spec.apply(parsed.value);
  if (undo) undos.set(request.correlation_id, undo);
  return result("applied", {
    receipt: title === "" ? "Applied to this page." : `Applied to ${title}.`,
    undoable: undo !== undefined,
  });
}

/** What a screen says about a page action it was proposed, for the card the person approves. */
export interface PageActionPreview {
  /** Each field it changes, in the screen's own words. */
  readonly rows: readonly PageActionRow[];
  /** Why the screen's own gate refuses it, or null where it may. */
  readonly blocked: string | null;
}

/**
 * What the screen under the drawer says about a proposed page action: its
 * rows and its gate, from the screen's OWN PARSER RUN FIRST over the payload,
 * which the model wrote and nothing here trusts. Null where no mounted screen
 * declares the action or its parser refuses the payload: the card then shows
 * the engine's rows, and the action, if approved, is refused by the screen.
 */
export function pageActionPreview(name: string, payload: unknown): PageActionPreview | null {
  const spec = isPageActionId(name) ? registration?.actions?.[name] : undefined;
  if (!spec) return null;
  const parsed = spec.parse(payload);
  if (!parsed.ok) return null;
  return { rows: spec.preview(parsed.value), blocked: spec.blocked(parsed.value) };
}

/**
 * Undo an applied action: `undone` where the screen put the page back,
 * `moved` where the page has changed since (nothing is changed, and the undo
 * is dropped), and `gone` where there is nothing to undo, because the screen
 * that applied it has gone.
 */
export function undoPageAction(correlationId: string): "undone" | "moved" | "gone" {
  const undo = undos.get(correlationId);
  if (!undo) return "gone";
  undos.delete(correlationId);
  return undo() ? "undone" : "moved";
}

// THE REGISTRY ANSWERS THE BUS: the drawer (a chunk) asks for an action by
// name and hears its result, and never holds a screen's handler itself.
on("assistant.action.apply", (request) => {
  emit("assistant.action.result", applyPageAction(request));
});

/** Test seam: the registry as a fresh page has it. */
export function resetPageContextForTest(): void {
  undos.clear();
  frame = null;
  registrations = [];
  registration = null;
  snapshot = null;
}
