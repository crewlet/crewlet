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
 */

import { useCallback, useLayoutEffect, useMemo, useRef } from "react";
import {
  ASSISTANT_PAGE_DRAFT_HELP_MAX_RUNES,
  ASSISTANT_PAGE_DRAFT_MAX_FIELDS,
  ASSISTANT_PAGE_DRAFT_SECRET_PATH,
  ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES,
  type AssistantDraftField,
  type AssistantDraftKind,
  type AssistantPageDraft,
} from "~/contract/assistant.ts";
import { hashOf, type PageActionParse, type PageActionSpec } from "./context.ts";
import { useAssistantContext } from "./useAssistantContext.ts";

/** A field's value: text, or a list of items. */
export type DraftValue = string | readonly string[];

/** Why a field will not take a proposed value, in a sentence the card shows. */
export class DraftRefusal {
  constructor(readonly reason: string) {}
}

/** A field's `write` refusing a value: the sentence is the person's to read. */
export function refuse(reason: string): DraftRefusal {
  return new DraftRefusal(reason);
}

/** One field of an open draft, as the screen declares it. */
export interface DraftFieldSpec<D> {
  /** The field's path in the draft (`colors.primary`): lowercase, dotted. */
  path: string;
  /** The field as the person reads it ("Primary colour"). */
  label: string;
  /** Its value now, read from the draft. A list is a list. */
  read(draft: D): DraftValue;
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
  /** Set the screen's draft, through its own setter. */
  apply(next: D): void;
  /** Why the draft may not be changed now (a save in flight), or null. */
  blocked?(): string | null;
}

/** What `draft.set` parses to: the draft it makes, and a row per field it sets. */
export interface DraftChange<D> {
  readonly next: D;
  readonly rows: readonly { field: string; next: string; current: string | null }[];
}

const KEY = /^[a-z][a-z0-9_.]{0,63}$/;
const SECRET_PATH = new RegExp(ASSISTANT_PAGE_DRAFT_SECRET_PATH);
const LABEL_MAX_RUNES = 120;
const HINT_MAX_RUNES = 200;
const ID_MAX_BYTES = 200;
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

/** An id or a revision the engine takes: plain, bounded; undefined otherwise. */
function plainId(value: string | number | null | undefined): string | undefined {
  if (value === null || value === undefined) return undefined;
  const text = String(value);
  return text !== "" &&
    encoder.encode(text).length <= ID_MAX_BYTES &&
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
      label: line(f.label, LABEL_MAX_RUNES),
      value: "",
    };
    if (f.hint) field.hint = line(f.hint, HINT_MAX_RUNES);
    if (typeof value !== "string") field.list = true;
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
      if (f.write) field.editable = true;
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

/** A proposed value off the wire, held to the field's shape: text, or a list for a list. */
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

/**
 * Parse a `draft.set` payload against the draft as it is now: every path a
 * field the screen lets the assistant set, every value in its field's shape
 * and taken by its `write`, or the whole change refused in a sentence.
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
  let next = spec.value;
  const rows: { field: string; next: string; current: string | null }[] = [];
  for (const [path, value] of entries) {
    const field = spec.fields.find((f) => f.path === path);
    if (!field) return refused(`The draft has no field “${path}”.`);
    if (!field.write || field.secret || isSecretDraftPath(field.path)) {
      return refused(`${field.label} is not something the assistant can set here.`);
    }
    const list = typeof field.read(spec.value) !== "string";
    const typed = proposed(value, list);
    if (typed === null) {
      return refused(
        list ? `${field.label} is a list of short lines.` : `${field.label} is a line of text.`,
      );
    }
    const written = field.write(next, typed);
    if (written instanceof DraftRefusal) return refused(written.reason);
    next = written;
    const show = field.summary ?? shown;
    rows.push({
      field: field.label,
      next: show(field.read(next)),
      current: show(field.read(spec.value)),
    });
  }
  return { ok: true, value: { next, rows } };
}

/** What the draft's fields read now, as one comparable value. */
function fingerprint<D>(spec: AssistantDraftSpec<D>, draft: D): string {
  return JSON.stringify(spec.fields.map((f) => f.read(draft)));
}

/** What a change to a draft that has closed is told. */
const CLOSED = "The draft is no longer open on this page.";

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
    parse: (raw) => {
      const spec = latest();
      return spec ? parseDraftChange(spec, raw) : { ok: false, reason: CLOSED };
    },
    preview: (change) => change.rows,
    blocked: () => {
      const spec = latest();
      return spec ? (spec.blocked?.() ?? null) : CLOSED;
    },
    apply: (change) => latest()?.apply(change.next),
    undo: (change) => {
      const before = latest()?.value;
      return () => {
        const spec = latest();
        if (!spec || before === undefined) return false;
        if (fingerprint(spec, spec.value) !== fingerprint(spec, change.next)) return false;
        spec.apply(before);
        return true;
      };
    },
  };
}

/**
 * Declare the draft a screen has open, while it is mounted: its snapshot
 * reaches every question the person asks here, and `draft.set` changes it
 * through `apply` once they approve. Null where the screen has nothing open
 * now (a card not in edit), which declares no draft and no action. See the
 * module doc.
 */
export function useAssistantDraft<D>(spec: AssistantDraftSpec<D> | null): void {
  const latest = useRef(spec);
  useLayoutEffect(() => {
    latest.current = spec;
  });
  const draft = spec ? draftSnapshot(spec) : null;
  const read = useCallback(() => latest.current, []);
  const action = useMemo(() => draftSetAction(read), [read]);
  useAssistantContext(
    draft
      ? {
          draft,
          // WHAT THE DRAFT SAYS IS THE PAGE'S DATA: a change prepared
          // against an older draft is stale once the person typed over it.
          // The registration is keyed on this content, so a render that
          // changed nothing registers nothing.
          dataVersion: `draft:${hashOf(JSON.stringify(draft))}`,
          actions: { "draft.set": action as PageActionSpec<unknown> },
        }
      : {},
  );
}
