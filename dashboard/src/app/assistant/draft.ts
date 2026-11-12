/**
 * What a screen with a draft tells the assistant: which draft is open, where
 * the person is in it, its values, and how a change to them is applied.
 *
 * # One contract for "what is open"
 *
 * An unsaved draft exists nowhere a tool could read it, so a screen that
 * edits one declares it with [useAssistantDraft], and the page context
 * carries it (`draft`, `contract/assistant.ts`). Before this, a screen nobody
 * wired by hand was invisible: the assistant searched the company for a draft
 * the person had open in front of it. A gate (`draftGate.test.ts`) fails for
 * an editing screen that declares nothing.
 *
 * # Redacted and bounded on the way out
 *
 * A field the screen marks `secret`, or whose path names a credential
 * (`ASSISTANT_PAGE_DRAFT_SECRET_PATH`, the engine's own rule), is sent with
 * no value and is never set. Every value is cut at the engine's bound and
 * marked `truncated`, and fields past the count are counted in `more`
 * (`internal/assistant/wire.PageDraft`), so a draft never makes a question
 * the engine refuses.
 *
 * # Changed through the screen, approved, then saved by the person
 *
 * The one page action into a draft is `draft.set`
 * (`{"values": {"<path>": "<value>"}}`). The screen's own `write` turns each
 * value into its draft, or refuses it in a sentence, exactly as its fields
 * would; the card shows each field's value now beside the proposed one; the
 * screen's setter applies it once the person approves; and saving stays the
 * person's own gesture. A draft the person changed since is a stale proposal
 * (its snapshot is the registration's data version).
 *
 * # Held in branded state
 *
 * A screen holds what it declares in branded state: [useDraftState] or
 * [useDraftReducer], whose setter and dispatch carry a brand nothing else
 * mints, or a store that extends [DraftStore]. A spec takes only a write made
 * from that state ([writeTo], [writeParts], [writeVia], [writeWith],
 * [writeEach], [writeThrough], [storeWrite], [latestWrite]) or, for a draft
 * it reads and never sets, the proof that it is held there ([readOnlyFrom];
 * a saved draft the page holds in no state of its own says
 * [readOnlySaved]). A draft held in plain React state is therefore a type
 * error, never a declaration.
 *
 * The brand is what the draft gate (`draftGate.test.ts`) reads through the
 * TypeScript checker. A module listed as a modal passes while it holds only
 * its own transient state, and is refused once it holds any of a page's
 * draft (a branded value it takes from context or a prop) or a page hands it
 * one, or a closure that reaches one: a modal whose change outlives it is
 * part of its page.
 */

import {
  useCallback,
  useLayoutEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
  type Dispatch,
  type SetStateAction,
} from "react";
import {
  ASSISTANT_ACTION_ARGS_DESCRIPTION_MAX_RUNES,
  ASSISTANT_ACTION_ARGS_MAX_BYTES,
  ASSISTANT_ACTION_REPORT_LINE_MAX_BYTES,
  ASSISTANT_PAGE_DRAFT_FILE_NAME_MAX_BYTES,
  ASSISTANT_PAGE_DRAFT_HELP_MAX_RUNES,
  ASSISTANT_PAGE_DRAFT_HINT_MAX_RUNES,
  ASSISTANT_PAGE_DRAFT_LABEL_MAX_RUNES,
  ASSISTANT_PAGE_DRAFT_MAX_FIELDS,
  ASSISTANT_PAGE_DRAFT_SECRET_PATH,
  ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES,
  ASSISTANT_PAGE_ID_MAX_BYTES,
  ASSISTANT_PAGE_KEY_PATTERN,
  type AssistantActionArgs,
  type AssistantDraftField,
  type AssistantDraftFieldKind,
  type AssistantDraftFileRef,
  type AssistantDraftKind,
  type AssistantPageDraft,
} from "~/contract/assistant.ts";
import { bytesOf, clipUtf8 } from "~/lib/clip.ts";
import { wireBytes } from "~/protocol/assistantWire.ts";
import {
  hashOf,
  type PageActionParse,
  type PageActionRow,
  type PageActionSpec,
} from "./context.ts";
import { useAssistantContext } from "./useAssistantContext.ts";

/** The ground a proposed file is previewed on, and its colour where the draft names one. */
export interface FileGround {
  tone: "light" | "dark";
  color?: string;
}

/** A field's value: text, or a list of items. A file field reads as its file's name. */
export type DraftValue = string | readonly string[];

/** Why a field will not take a proposed value, in a sentence the card shows. */
export class DraftRefusal {
  constructor(readonly reason: string) {}
}

/** A field's `write` refusing a value: the sentence is the person's to read. */
export function refuse(reason: string): DraftRefusal {
  return new DraftRefusal(reason);
}

/** A list a refusal names ([said]): as many of its names as fit, then how many more. */
export interface Listing {
  readonly names: readonly string[];
  /** All of the names, said where they fit: "a, b, c", or "a, b or c". */
  readonly whole: (names: readonly string[]) => string;
}

/** Names joined by commas, as a list in a sentence reads where it ends in nothing. */
function commas(names: readonly string[]): string {
  return names.join(", ");
}

/** `names` as a refusal lists them ([said]), `whole` saying all of them where they fit. */
export function listing(
  names: readonly string[],
  whole: (names: readonly string[]) => string = commas,
): Listing {
  return { names, whole };
}

/**
 * `names` within `room`, as `measure` counts it: all of them, said by
 * `whole`, where they fit, else as many as fit and how many more, and how many
 * there are where not even the first does.
 */
export function listedWithin(
  names: readonly string[],
  room: number,
  measure: (text: string) => number,
  whole: (names: readonly string[]) => string = commas,
): string {
  const all = whole(names);
  if (measure(all) <= room) return all;
  const kept: string[] = [];
  for (const name of names) {
    const more = ` and ${names.length - kept.length - 1} more`;
    if (measure([...kept, name].join(", ")) + measure(more) > room) break;
    kept.push(name);
  }
  return kept.length === 0
    ? `${names.length} of them`
    : `${kept.join(", ")} and ${names.length - kept.length} more`;
}

/**
 * `room` shared out among values of `sizes`: each takes its own size where the
 * room holds it, and THE LONGEST GIVE WAY FIRST, each in turn to an even share
 * of what the shorter ones left.
 */
function shares(room: number, sizes: readonly number[]): number[] {
  const caps = sizes.map(() => 0);
  const order = sizes.map((_, i) => i).sort((a, b) => sizes[a]! - sizes[b]!);
  let left = Math.max(room, 0);
  order.forEach((i, at) => {
    const cap = Math.min(sizes[i]!, Math.floor(left / (order.length - at)));
    caps[i] = cap;
    left -= cap;
  });
  return caps;
}

/**
 * A REFUSAL IN ONE SHORT LINE, written as a tagged template, as in
 * said`${field.label} is a line of text.`. A refusal is the reason a page
 * action's report carries back to the model (`chunk/pageActions.ts`), which
 * the engine takes as one line of [ASSISTANT_ACTION_REPORT_LINE_MAX_BYTES]
 * bytes, and the report cuts a longer one at its end: where a refusal says
 * what its field DOES take. So the template's own words stay whole and the
 * values placed in it give way to them, the longest first ([shares]): a text
 * is cut at a whole character and marked ([clipUtf8]), and a [Listing] says as
 * many of its names as fit and how many more. A name or a value somebody typed
 * can run to any length; the sentence around it cannot.
 */
export function said(
  words: TemplateStringsArray,
  ...values: readonly (string | Listing)[]
): string {
  const whole = values.map((v) => (typeof v === "string" ? v : v.whole(v.names)));
  const fixed = words.reduce((n, w) => n + bytesOf(w), 0);
  const caps = shares(ASSISTANT_ACTION_REPORT_LINE_MAX_BYTES - fixed, whole.map(bytesOf));
  let out = words[0] ?? "";
  values.forEach((v, i) => {
    const cap = caps[i]!;
    out +=
      typeof v === "string"
        ? clipUtf8(v, cap)
        : clipUtf8(listedWithin(v.names, cap, bytesOf, v.whole), cap);
    out += words[i + 1] ?? "";
  });
  return out;
}

/** One field of an open draft, as the screen declares it. */
export interface DraftFieldSpec<D> {
  /** The field's path in the draft (`colors.primary`): lowercase, dotted. */
  path: string;
  /** The field as the person reads it ("Primary colour"). */
  label: string;
  /** Its value now, read from the draft. A list is a list; a file is its name. */
  read(draft: D): DraftValue;
  /**
   * A file field (a logo): what it accepts, by media type. Its value now is
   * the file's name, and a change to it is a reference to a file of the
   * person's own conversation, which the screen fetches and keeps through its
   * own save, never markup.
   */
  file?: {
    accepts: readonly string[];
    /** The draft with this file proposed for the field, or [refuse] with why not. */
    write(draft: D, ref: AssistantDraftFileRef): D | DraftRefusal;
    /**
     * The ground the field's file is shown on, read from the draft it lands
     * in: a logo for dark grounds is previewed on the brand's dark ground, so
     * a light wordmark reads on the card as it will in use. Absent: light.
     */
    ground?(draft: D): FileGround;
  };
  /** What a value must be, in a few words ("a #RRGGBB colour"). */
  hint?: string;
  /** A credential: its value never leaves the page and the assistant never sets it. */
  secret?: boolean;
  /**
   * The draft with this field set, or [refuse] with a sentence saying why it
   * may not be; absent for a field the assistant reads and does not set.
   */
  write?(draft: D, value: DraftValue): D | DraftRefusal;
  /**
   * The value as one line of the approval card, where the value itself is
   * too long to read there ("412 words"); the value itself otherwise.
   */
  summary?(value: DraftValue): string;
}

/**
 * The brand of a draft's own state: a type-only key, so nothing outside this
 * module mints a value that carries it but by a cast, which the draft gate
 * refuses, and the gate tells a draft's setter from every other setter by
 * its type alone.
 */
declare const HELD: unique symbol;

/**
 * What every branded value of a draft's state carries: [DraftSetter],
 * [DraftDispatch], [DraftWrite], [DraftReadOnly] and a [DraftStore]. The
 * draft gate reads a value as the draft's own when its type is assignable to
 * this.
 */
export interface DraftHeld {
  readonly [HELD]: "setter" | "dispatch" | "write" | "read" | "store";
}

/** The setter of a draft held in [useDraftState]: React's own, branded. */
export type DraftSetter<D> = Dispatch<SetStateAction<D>> & { readonly [HELD]: "setter" };

/** The dispatch of a draft held in [useDraftReducer]: React's own, branded. */
export type DraftDispatch<A> = Dispatch<A> & { readonly [HELD]: "dispatch" };

/**
 * How an approved change reaches a screen's draft: a write made only from
 * the branded state the draft is held in ([writeTo], [writeParts],
 * [writeVia], [writeWith], [writeEach], [writeThrough], [storeWrite],
 * [latestWrite]), so a spec cannot be built from any other.
 */
export type DraftWrite<D> = ((next: D) => void) & { readonly [HELD]: "write" };

/**
 * A draft the assistant reads and never sets, held in branded state all the
 * same ([readOnlyFrom]): changed through its own controls or a page action,
 * so a modal handed it is still part of its page.
 */
export interface DraftReadOnly {
  readonly [HELD]: "read";
}

/**
 * A store that keeps a page's drafts outside React state (the chat's unsent
 * messages, kept across its rooms), branded by extending this as React state
 * is by [useDraftState]: a write into it ([storeWrite]) is a draft's own, and
 * the draft gate reads a module that holds the store as holding the draft.
 * Nothing is added at run time.
 */
export abstract class DraftStore {
  declare readonly [HELD]: "store";
}

/** A screen's draft, or one part of it, held in React state whose setter is branded. */
export function useDraftState<D>(initial: D | (() => D)): [D, DraftSetter<D>] {
  const [draft, set] = useState(initial);
  return [draft, set as DraftSetter<D>];
}

/** A screen's draft held in a reducer whose dispatch is branded. */
export function useDraftReducer<S, A>(
  reducer: (state: S, action: A) => S,
  initial: S,
): [S, DraftDispatch<A>] {
  const [state, dispatch] = useReducer(reducer, initial);
  return [state, dispatch as DraftDispatch<A>];
}

/**
 * A branded dispatch that runs `around` first, which may hold an action back
 * (an edit refused while the page is read-only) or pass it on: still the
 * draft's own dispatch for whoever is handed it.
 */
export function guardedDispatch<A>(
  dispatch: DraftDispatch<A>,
  around: (action: A, pass: (action: A) => void) => void,
): DraftDispatch<A> {
  return ((action: A) => around(action, dispatch)) as DraftDispatch<A>;
}

/** The one place a [DraftWrite] is minted, from a write the functions below build. */
function minted<D>(write: (next: D) => void): DraftWrite<D> {
  return write as DraftWrite<D>;
}

/**
 * What a screen does beside writing its draft when a change reaches it from
 * outside (a refusal cleared, an editor told its body was replaced): never a
 * write of a draft, which goes through a write of its own ([writeEach]). It
 * is handed the change and nothing that writes.
 */
export type DraftAlso<D> = (next: D) => void;

/**
 * A draft held whole in one branded state: a change is set as it is, or as a
 * pure `transform` makes it of the change and the draft now. `also` is what
 * the screen's own control does beside setting the draft (a refusal cleared,
 * an editor told its body was replaced), and never a write of the draft.
 */
export function writeTo<D>(
  set: DraftSetter<D>,
  how: { transform?: (next: D, current: D) => D; also?: DraftAlso<D> } = {},
): DraftWrite<D> {
  const { transform, also } = how;
  return minted((next: D) => {
    if (transform) set((current) => transform(next, current));
    else set(next);
    also?.(next);
  });
}

/**
 * A draft whose value is several branded states, one per part, each written
 * where the change moved it, through its own setter or a write made from it:
 * a create page holds its title, its brief and its picker each in its own
 * state, and a page's boxes each hold their own text so a keystroke renders
 * the box alone (`boxes.ts`). A part is data, never a function, so a setter
 * is handed its value as it is.
 */
export function writeParts<D extends object>(
  current: D,
  parts: { readonly [K in keyof D]-?: DraftSetter<D[K]> | DraftWrite<D[K]> },
  also?: DraftAlso<D>,
): DraftWrite<D> {
  return minted((next: D) => {
    for (const key of Object.keys(parts) as (keyof D)[]) {
      if (Object.is(next[key], current[key])) continue;
      (parts[key] as (value: D[typeof key]) => void)(next[key]);
    }
    also?.(next);
  });
}

/**
 * A draft, or a part of one, held in another shape than it is declared in:
 * written through its branded setter as the pure `to` makes it of the change
 * and the state now (the template a declared key names; one editor's text in
 * the map of every editor open on a page; a draft held as null while shut).
 */
export function writeVia<V, S>(
  set: DraftSetter<S>,
  to: (next: V, current: S) => S,
  also?: DraftAlso<V>,
): DraftWrite<V> {
  return minted((next: V) => {
    set((current) => to(next, current));
    also?.(next);
  });
}

/**
 * A change written through several branded writes in turn: a draft held in
 * several states that one change moves together (a sheet's way in, and the
 * grid its template makes the first time it is chosen).
 */
export function writeEach<D>(...writes: readonly DraftWrite<D>[]): DraftWrite<D> {
  return minted((next: D) => {
    for (const write of writes) write(next);
  });
}

/**
 * A draft written in steps through a branded write: `write` is handed `put`
 * and writes the draft through it alone, now and once what the change names
 * has arrived (a logo's bytes, fetched after the change is approved).
 */
export function writeWith<D, P>(
  put: DraftWrite<P>,
  write: (next: D, put: DraftWrite<P>) => void,
): DraftWrite<D> {
  return minted((next: D) => write(next, put));
}

/**
 * A write that runs the latest render's: a part a page gathers from its
 * children (a box, a tab) is written through the child as it is now, never
 * as it was when the page last declared, so the child's own setter and what
 * it does beside it see the render the change lands in.
 */
export function latestWrite<D>(
  latest: () => DraftSetter<D> | DraftWrite<D> | undefined,
): DraftWrite<D> {
  return minted((next: D) => latest()?.(next));
}

/**
 * A draft held in a branded reducer: a change is dispatched as the actions
 * it maps to, in order (one per part it moved, as each part's own control
 * dispatches it).
 */
export function writeThrough<D, A>(
  dispatch: DraftDispatch<A>,
  actions: (next: D) => readonly A[],
): DraftWrite<D> {
  return minted((next: D) => {
    for (const action of actions(next)) dispatch(action);
  });
}

/**
 * A draft held in a branded store: a change is written there as `write`
 * writes it (under the box's room and thread). The store is what places the
 * draft where the gate can follow it, so it is asked for though only `write`
 * runs.
 */
export function storeWrite<D>(store: DraftStore, write: (next: D) => void): DraftWrite<D> {
  return minted(write);
}

/**
 * The proof that a draft the assistant reads and never sets is held in
 * branded state: `held` is never written here, and is asked for so that a
 * read-only spec cannot be built from plain React state either.
 */
export function readOnlyFrom<S>(
  held: DraftSetter<S> | DraftDispatch<S> | DraftStore,
): DraftReadOnly {
  return {} as DraftReadOnly;
}

/**
 * A saved draft (`kind: "server"`) the page reads and holds in no state of
 * its own, changed on the screen that edits it, which its help names. The
 * draft gate holds every spec that says this to `kind: "server"`, so a draft
 * the page holds is never declared as one it does not.
 */
export function readOnlySaved(): DraftReadOnly {
  return {} as DraftReadOnly;
}

/** An open draft, as the screen declares it. */
export interface AssistantDraftSpec<D> {
  /**
   * `client` while the values live in the page until the person saves;
   * `server` where every value shown is saved, at `id` and `revision`.
   */
  kind: AssistantDraftKind;
  /** The saved record the draft is of (a kit, a page), or null for one nothing is saved of yet. */
  id?: string | null;
  /** The saved record's version the draft was opened on. */
  revision?: string | number | null;
  /** The step or section the person is on (`colours`). */
  step?: string | null;
  /**
   * The screen's word to the assistant: what it can and cannot do with this
   * draft. Said here, by the screen, never as a special case in the prompt.
   */
  help?: string;
  /** The approval card's scope line: "The brand draft you are setting up (not saved)". */
  scope: string;
  /** The draft now. */
  value: D;
  fields: readonly DraftFieldSpec<D>[];
  /**
   * How a change reaches the screen's draft, made from the branded state it
   * is held in ([DraftWrite]); [DraftReadOnly] for a draft the screen shows
   * and does not edit (a saved draft whose editor is another screen), which
   * declares no `draft.set` and so is read and never changed here, its help
   * saying where it is changed.
   */
  write: DraftWrite<D> | DraftReadOnly;
  /** Why the draft may not be changed now (a save in flight), or null. */
  blocked?(): string | null;
}

/** What `draft.set` parses to: the draft it makes, and a row per field it sets. */
export interface DraftChange<D> {
  readonly next: D;
  readonly rows: readonly PageActionRow[];
}

const KEY = new RegExp(ASSISTANT_PAGE_KEY_PATTERN);
const SECRET_PATH = new RegExp(ASSISTANT_PAGE_DRAFT_SECRET_PATH);
/** The longest text one proposed value may be: well past any field a draft screen draws. */
const PROPOSED_MAX_RUNES = 16 * 1024;
/** Every control character but a line break and a tab. */
const CONTROL = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/;
const CONTROL_ALL = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/g;

const encoder = new TextEncoder();

/** Whether a draft field's path names a credential, by the engine's rule. */
export function isSecretDraftPath(path: string): boolean {
  return SECRET_PATH.test(path);
}

/** Text cut at `max` characters, whole characters only, and whether it was cut. */
function cut(text: string, max: number): { text: string; cut: boolean } {
  const runes = [...text];
  return runes.length <= max
    ? { text, cut: false }
    : { text: runes.slice(0, max).join(""), cut: true };
}

/** One line of text, no control character, at most `max` characters. */
function line(text: string, max: number): string {
  return cut(text.replace(/[\u0000-\u001f\u007f-\u009f]/g, " "), max).text;
}

/** A value as the draft's text: a list one item per line. */
function asText(value: DraftValue): string {
  return typeof value === "string" ? value : value.join("\n");
}

/** A value as one line of an approval card. */
function shown(value: DraftValue): string {
  const text = typeof value === "string" ? value : value.join("; ");
  return text.trim() === "" ? "(empty)" : text;
}

/** Whether the screen lets the assistant set a field. */
function settable<D>(field: DraftFieldSpec<D>): boolean {
  return (
    (field.write !== undefined || field.file !== undefined) &&
    !field.secret &&
    !isSecretDraftPath(field.path)
  );
}

/** A field's kind: a file where the screen declares one, else by what it reads. */
function kindOf<D>(field: DraftFieldSpec<D>, value: DraftValue): AssistantDraftFieldKind {
  if (field.file) return "file";
  return typeof value === "string" ? "text" : "list";
}

/** An id or a revision the engine takes: plain, bounded; undefined otherwise. */
function plainId(value: string | number | null | undefined): string | undefined {
  if (value === null || value === undefined) return undefined;
  const text = String(value);
  return text !== "" &&
    encoder.encode(text).length <= ASSISTANT_PAGE_ID_MAX_BYTES &&
    !/[\u0000-\u001f\u007f-\u009f]/.test(text)
    ? text
    : undefined;
}

/**
 * The draft as the page context carries it: redacted, each string held to
 * the engine's bound, fields past the count counted in `more`. A field whose
 * path is outside the grammar, or that repeats one, is left out and counted.
 */
export function draftSnapshot<D>(spec: AssistantDraftSpec<D>): AssistantPageDraft {
  const fields: AssistantDraftField[] = [];
  const seen = new Set<string>();
  let more = 0;
  for (const f of spec.fields) {
    if (!KEY.test(f.path) || seen.has(f.path) || fields.length >= ASSISTANT_PAGE_DRAFT_MAX_FIELDS) {
      more += 1;
      continue;
    }
    seen.add(f.path);
    const value = f.read(spec.value);
    const field: AssistantDraftField = {
      path: f.path,
      label: line(f.label, ASSISTANT_PAGE_DRAFT_LABEL_MAX_RUNES),
      kind: kindOf(f, value),
      value: "",
    };
    const hint = f.file ? (f.hint ?? `A file: ${f.file.accepts.join(", ")}`) : f.hint;
    if (hint) field.hint = line(hint, ASSISTANT_PAGE_DRAFT_HINT_MAX_RUNES);
    if (f.secret || isSecretDraftPath(f.path)) {
      // A CREDENTIAL NEVER LEAVES THE PAGE, and is never the assistant's to set.
      field.redacted = true;
    } else {
      const text = cut(
        asText(value).replace(CONTROL_ALL, " "),
        ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES,
      );
      field.value = text.text;
      if (text.cut) field.truncated = true;
      if (settable(f)) field.editable = true;
    }
    fields.push(field);
  }
  const id = plainId(spec.id);
  const revision = plainId(spec.revision);
  const step = spec.step && KEY.test(spec.step) ? spec.step : undefined;
  const help = spec.help
    ? cut(spec.help.replace(CONTROL_ALL, " "), ASSISTANT_PAGE_DRAFT_HELP_MAX_RUNES).text
    : "";
  // A SAVED DRAFT NAMES ITS RECORD; one that cannot is the page's own.
  const kind: AssistantDraftKind =
    spec.kind === "server" && id === undefined ? "client" : spec.kind;
  return {
    kind,
    ...(id === undefined ? {} : { id }),
    ...(revision === undefined ? {} : { revision }),
    ...(step === undefined ? {} : { step }),
    ...(help === "" ? {} : { help }),
    fields,
    more,
  };
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * A proposed file reference held to the wire's shape and to what the field
 * accepts: ids, a plain file name (never a path) and an accepted media type.
 * The engine checked before the card was drawn that the file is the person's,
 * in this conversation; the payload is bound to that by its digest.
 */
function fileRef(raw: unknown, accepts: readonly string[]): AssistantDraftFileRef | null {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
  const { kind, file_id, conversation_id, name, media_type, ...rest } = raw as Record<
    string,
    unknown
  >;
  if (Object.keys(rest).length > 0 || kind !== "file") return null;
  if (typeof file_id !== "string" || !UUID.test(file_id)) return null;
  if (typeof conversation_id !== "string" || !UUID.test(conversation_id)) return null;
  if (typeof name !== "string" || name === "" || /[/\\\u0000-\u001f\u007f-\u009f]/.test(name)) {
    return null;
  }
  if (encoder.encode(name).length > ASSISTANT_PAGE_DRAFT_FILE_NAME_MAX_BYTES) return null;
  if (typeof media_type !== "string" || !accepts.includes(media_type)) return null;
  return { kind: "file", file_id, conversation_id, name, media_type };
}

/** A proposed text or list off the wire, held to the field's shape. */
function proposed(raw: unknown, list: boolean): DraftValue | null {
  if (typeof raw === "string") {
    if (CONTROL.test(raw) || [...raw].length > PROPOSED_MAX_RUNES) return null;
    // A LIST MAY ARRIVE AS ITS TEXT, one item per line, as the snapshot shows it.
    return list
      ? raw
          .split("\n")
          .map((item) => item.trim())
          .filter((item) => item !== "")
      : raw;
  }
  if (!list || !Array.isArray(raw)) return null;
  if (!raw.every((item): item is string => typeof item === "string" && !CONTROL.test(item)))
    return null;
  if (raw.join("").length > PROPOSED_MAX_RUNES) return null;
  return raw.map((item) => item.trim()).filter((item) => item !== "");
}

/** One field's proposed value written into the draft, and its card row; or why not. */
function written<D>(
  field: DraftFieldSpec<D>,
  draft: D,
  now: D,
  value: unknown,
): { next: D; row: PageActionRow } | string {
  const show = field.summary ?? shown;
  const current = show(field.read(now));
  if (field.file) {
    const ref = fileRef(value, field.file.accepts);
    if (ref === null) {
      return said`${field.label} takes one of your files: ${listing(field.file.accepts)}.`;
    }
    const next = field.file.write(draft, ref);
    if (next instanceof DraftRefusal) return next.reason;
    // A FILE IS SHOWN BY ITS NAME AND TYPE, and drawn by the card from the
    // owner's route as an image, never inlined.
    const row: PageActionRow = {
      field: field.label,
      next: `${ref.name} (${ref.media_type})`,
      current,
      file: ref,
    };
    if (field.file.ground) row.fileGround = field.file.ground(next);
    return { next, row };
  }
  const list = typeof field.read(now) !== "string";
  const typed = proposed(value, list);
  if (typed === null) {
    return list
      ? said`${field.label} is a list of short lines.`
      : said`${field.label} is a line of text.`;
  }
  const next = field.write!(draft, typed);
  if (next instanceof DraftRefusal) return next.reason;
  const row: PageActionRow = { field: field.label, next: show(field.read(next)), current };
  // A LIST IS DRAWN AS ITS ITEMS, unless the screen summarises it ("412 words").
  if (list && !field.summary) {
    const was = field.read(now);
    const is = field.read(next);
    if (typeof was !== "string") row.currentItems = was;
    if (typeof is !== "string") row.items = is;
  }
  return { next, row };
}

/**
 * Parse a `draft.set` payload against the draft as it is now: every path a
 * field the screen lets the assistant set (an unknown path is refused, never
 * ignored), every value in its field's shape and taken by its `write`, or the
 * whole change refused in a sentence.
 *
 * WRITTEN IN THE SCREEN'S ORDER, never the payload's. A change is a set of
 * values, and the order a proposal names them in is an accident of how it
 * was encoded (the engine's encoder sorts them by path). A screen declares its
 * fields in the order its controls are filled in, so a field whose control
 * waits on another (an agent's access, chosen once the agent is on) is
 * written after that one, and its card's rows read in the screen's order.
 */
export function parseDraftChange<D>(
  spec: AssistantDraftSpec<D>,
  raw: unknown,
): PageActionParse<DraftChange<D>> {
  const refused = (reason: string): PageActionParse<DraftChange<D>> => ({ ok: false, reason });
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return refused("That is not a change to the draft.");
  }
  const { values, ...rest } = raw as Record<string, unknown>;
  if (
    Object.keys(rest).length > 0 ||
    values === null ||
    typeof values !== "object" ||
    Array.isArray(values)
  ) {
    return refused("A change to the draft is its values, by field.");
  }
  const entries = Object.entries(values as Record<string, unknown>);
  if (entries.length === 0) return refused("That changes nothing in the draft.");
  if (entries.length > ASSISTANT_PAGE_DRAFT_MAX_FIELDS)
    return refused("That changes more fields than the draft has.");
  const given = new Map<string, unknown>();
  for (const [path, value] of entries) {
    const field = spec.fields.find((f) => f.path === path);
    if (!field) return refused(said`The draft has no field “${path}”.`);
    if (!settable(field))
      return refused(said`${field.label} is not something Ask Crewlet can set here.`);
    given.set(path, value);
  }
  let next = spec.value;
  const rows: PageActionRow[] = [];
  for (const field of spec.fields) {
    // A PATH IS ITS FIRST FIELD'S, as the snapshot lists it and the lookup
    // above finds it; a repeat is never written twice.
    if (!given.has(field.path)) continue;
    const value = given.get(field.path);
    given.delete(field.path);
    const out = written(field, next, spec.value, value);
    if (typeof out === "string") return refused(out);
    next = out.next;
    rows.push(out.row);
  }
  return { ok: true, value: { next, rows } };
}

/**
 * A field's description, `what` it is and then `how` its kind is written,
 * within the engine's bound for one description: `what` (a label and a hint
 * the screen wrote, each long within its own bound) gives way, marked, so the
 * kind's own words always arrive.
 */
function described(what: string, how: string): string {
  const tail = how === "" ? "" : `. ${how}`;
  const room = ASSISTANT_ACTION_ARGS_DESCRIPTION_MAX_RUNES - [...tail].length;
  const fitted = cut(what, room);
  return fitted.cut ? `${cut(what, room - 1).text}…${tail}` : `${what}${tail}`;
}

/** What one editable field takes, by its kind: text, a list, or a file of this conversation. */
function fieldArgs(field: AssistantPageDraft["fields"][number]): AssistantActionArgs {
  const what = field.hint ? `${field.label}. ${field.hint}` : field.label;
  switch (field.kind) {
    case "list":
      return {
        type: ["array", "string"],
        description: described(what, "A list of items, or one item per line."),
        items: { type: "string" },
      };
    case "file":
      return {
        type: ["string", "object"],
        description: described(
          what,
          "The id of a file delivered or attached in this conversation.",
        ),
      };
    default:
      return { type: "string", description: described(what, "") };
  }
}

/**
 * What `draft.set` takes on this draft: `values`, each an editable field by
 * its path, in its kind's shape. Built from the snapshot the page sends, so
 * the model reads the same fields the screen's parser sets.
 *
 * A MEMBER PER EDITABLE FIELD grows with the draft past what one schema may
 * declare (its members, its bytes), and the frame then sends `draft.set`
 * without its arguments (`buildPageContext`), which the engine takes for it
 * alone, reading what it takes from the draft's editable fields
 * (`catalog.PageActionFor`). What is better cut than lost with the rest is cut
 * here: each description to its bound ([described]), and the example offered
 * only where it still fits.
 */
export function draftSetArgs(draft: AssistantPageDraft): AssistantActionArgs {
  const properties: Record<string, AssistantActionArgs> = {};
  for (const field of draft.fields) {
    if (field.editable === true) properties[field.path] = fieldArgs(field);
  }
  const args: AssistantActionArgs = {
    type: "object",
    description: "Values for the open draft's editable fields, by path.",
    additionalProperties: false,
    required: ["values"],
    properties: {
      values: { type: "object", additionalProperties: false, properties },
    },
  };
  // THE FIELD'S OWN VALUE is the one example every field takes as it stands,
  // offered where it still fits: a value runs to a thousand characters.
  const first = draft.fields.find((f) => f.editable === true && f.kind === "text" && f.value);
  if (!first) return args;
  const shown = { ...args, examples: [{ values: { [first.path]: first.value } }] };
  return wireBytes(shown) <= ASSISTANT_ACTION_ARGS_MAX_BYTES ? shown : args;
}

/** What the draft's fields read now, as one comparable value. */
function fingerprint<D>(spec: AssistantDraftSpec<D>, draft: D): string {
  return JSON.stringify(spec.fields.map((f) => f.read(draft)));
}

/** What a change to a draft that has closed is told. */
const CLOSED = "The draft is no longer open on this page.";

/** What a closed draft takes: nothing, since every half refuses it. */
const CLOSED_ARGS: AssistantActionArgs = {
  type: "object",
  description: CLOSED,
  additionalProperties: false,
  properties: {},
};

/**
 * The `draft.set` page action over a draft spec: the screen's own parser,
 * rows, gate and setter, and an undo that puts the draft back while it still
 * says what the change made of it.
 *
 * `latest` reads the spec of the latest render, so a value the person typed
 * since is the one a change is parsed against and applied over; null once
 * the screen closed its draft, which every half then refuses.
 */
export function draftSetAction<D>(
  latest: () => AssistantDraftSpec<D> | null,
): PageActionSpec<DraftChange<D>> {
  return {
    get scope() {
      return latest()?.scope ?? "";
    },
    get args() {
      const spec = latest();
      return spec ? draftSetArgs(draftSnapshot(spec)) : CLOSED_ARGS;
    },
    parse: (raw) => {
      const spec = latest();
      return spec ? parseDraftChange(spec, raw) : { ok: false, reason: CLOSED };
    },
    preview: (change) => change.rows,
    blocked: () => {
      const spec = latest();
      return spec ? (spec.blocked?.() ?? null) : CLOSED;
    },
    apply: (change) => {
      const write = latest()?.write;
      if (typeof write === "function") write(change.next);
    },
    undo: (change) => {
      const before = latest()?.value;
      return () => {
        const spec = latest();
        if (!spec || before === undefined) return false;
        if (fingerprint(spec, spec.value) !== fingerprint(spec, change.next)) return false;
        if (typeof spec.write !== "function") return false;
        spec.write(before);
        return true;
      };
    },
  };
}

/**
 * Declare the draft a screen has open, while it is mounted: its snapshot
 * reaches every question the person asks here, and `draft.set` changes it
 * through its `write` once they approve. Null where the screen has nothing
 * open now (a card not in edit), which declares no draft and no action. A
 * draft that is read only, or with no field the assistant may set, is
 * declared without
 * `draft.set`: the page does not accept a change it could only refuse. See
 * the module doc.
 */
export function useAssistantDraft<D>(spec: AssistantDraftSpec<D> | null): void {
  const latest = useRef(spec);
  useLayoutEffect(() => {
    latest.current = spec;
  });
  const draft = spec ? draftSnapshot(spec) : null;
  const read = useCallback(() => latest.current, []);
  const action = useMemo(() => draftSetAction(read), [read]);
  const changeable =
    typeof spec?.write === "function" &&
    draft !== null &&
    draft.fields.some((f) => f.editable === true);
  useAssistantContext(
    draft
      ? {
          draft,
          // WHAT THE DRAFT SAYS IS THE PAGE'S DATA: a change prepared
          // against an older draft is stale once the person typed over it.
          // The registration is keyed on this content, so a render that
          // changed nothing registers nothing.
          dataVersion: `draft:${hashOf(JSON.stringify(draft))}`,
          actions: changeable
            ? {
                "draft.set": {
                  ...(action as PageActionSpec<unknown>),
                  args: draftSetArgs(draft),
                  // THE DRAFT IS WHAT IT EDITS: a change prepared against an
                  // older draft is stale once the person typed over it, and
                  // a filter or a peek beside the editor is not.
                  tracks: ["data"],
                },
              }
            : {},
        }
      : {},
  );
}
