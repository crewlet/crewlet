/**
 * A page action over the address: the one shape almost every screen's
 * assistant action has, written once.
 *
 * A screen's view state lives on the URL (`app/router.tsx`): a chip, a sort,
 * a window, a tab. So the change the assistant may make to a view is a PATCH
 * of address keys, and [urlAction] builds the whole `PageActionSpec` for one
 * from the keys the screen names:
 *
 *  - PARSE accepts an object of those keys and nothing else, each value plain
 *    bounded text or null (clear it), held to the key's own `check` where the
 *    screen has one (the window its offer allows, a grouping it draws). A key
 *    the screen did not name is refused by name, so the assistant can never
 *    write a key the screen does not read, and nothing is partly applied.
 *  - PREVIEW is one row per key: the key's label, the new value and the one
 *    on the address now, each in the screen's own words.
 *  - APPLY is ONE navigation through the router, so a patch is one history
 *    entry: replaced where every key is a filter (four chips are one screen),
 *    pushed where any key is a section (a window, a tab: a place the reader
 *    went), exactly as the screen's own controls write those keys.
 *  - UNDO writes back what the address held, and only while every key still
 *    holds what the action wrote, so it never reverts a change made since.
 *
 * The screen's own gate (a host's lock that is what the page IS) comes in as
 * `blocked`; the screen decides it, never the model.
 */

import type { AssistantActionArgs } from "~/contract/assistant.ts";
import { parseHash, type Navigator } from "../router.tsx";
import { parseWindow, TODAY, windowLabel, windowParam, type Offer } from "~/lib/range.ts";
import type { PageActionId, PageActionParse, PageActionSpec } from "./context.ts";

/** How a screen writes a key: a filter replaces the history entry, a section pushes one. */
export type UrlKeyHistory = "filter" | "section";

/** One address key a page action may write, and how a person reads it. */
export interface UrlActionKey {
  /** The field, as the screen's own control names it: "Status", "Window". */
  label: string;
  /** How the screen's own control writes it. */
  history: UrlKeyHistory;
  /** Refuse a value the screen would not take, in a sentence; null where it takes it. */
  check?: (value: string) => string | null;
  /**
   * Every value the key takes, where the screen can name them all (a
   * choice, a tab, a window of a fixed offer): what the assistant is told it
   * may write, beside `check`, which stays the rule.
   */
  values?: readonly string[];
  /** How a value is written, said to the assistant where `values` cannot list them. */
  hint?: string;
  /** The value, in the screen's own words. Unset: the value as written. */
  words?: (value: string) => string;
  /**
   * The value the screen reads when the key is absent, as `useParam`'s
   * fallback: written as a clear, so the address keeps no key nobody chose,
   * and shown as the current value where the address holds none.
   */
  fallback?: string;
}

/** A change to the address: a key to its new value, or null to clear it. */
export type UrlPatch = Readonly<Record<string, string | null>>;

/** What a URL action is built from. */
export interface UrlActionOptions {
  /** What it touches, for the card's scope line: "Worklet list filters (not saved)". */
  scope: string;
  /**
   * The keys it may write, by their address spelling. A key ending in a dot
   * (`f.`) stands for every key beneath it, as `URL_HOMES` spells a family.
   */
  keys: Readonly<Record<string, UrlActionKey>>;
  /** The router's writers. */
  nav: Pick<Navigator, "filter" | "to">;
  /** The screen's own gate beyond the keys: a sentence, or null where it may. */
  blocked?: (patch: UrlPatch) => string | null;
  /**
   * For an action that takes NO arguments (Clear): the patch the screen
   * writes, computed from the address at the moment it is asked. Only the
   * keys the address holds are written, so the card lists what goes.
   */
  preset?: () => UrlPatch;
}

/** The most keys one patch writes; a list has fewer controls than this. */
const MAX_KEYS = 16;
/** The longest value a key takes, in UTF-16 units; a filter value is a few words. */
const MAX_VALUE = 200;
/** A family member's own part: a slug, as `f.<slug>` and `cols.<shape>` spell one. */
const MEMBER_SOURCE = "[a-z0-9][a-z0-9_-]{0,63}";
const MEMBER = new RegExp(`^${MEMBER_SOURCE}$`);

/** The key a patch names, or null where the screen offers none by that name. */
function keyOf(keys: Readonly<Record<string, UrlActionKey>>, name: string): UrlActionKey | null {
  if (Object.hasOwn(keys, name) && !name.endsWith(".")) return keys[name]!;
  const dot = name.indexOf(".");
  if (dot < 0) return null;
  const family = name.slice(0, dot + 1);
  return Object.hasOwn(keys, family) && MEMBER.test(name.slice(dot + 1)) ? keys[family]! : null;
}

/** The address as the browser holds it now, never as a render saw it. */
function liveQuery(): { path: string[]; query: URLSearchParams } {
  const route = parseHash(location.hash);
  return { path: route.path, query: route.query };
}

/** A patch's value as the address spells it: null for an absent key. */
function held(query: URLSearchParams, key: string): string | null {
  const value = query.get(key);
  return value === null || value === "" ? null : value;
}

/** Write `patch` as ONE navigation, pushed where any key is a section. */
function write(
  patch: UrlPatch,
  keys: Readonly<Record<string, UrlActionKey>>,
  nav: UrlActionOptions["nav"],
): void {
  const pushes = Object.keys(patch).some((k) => keyOf(keys, k)?.history === "section");
  if (!pushes) {
    nav.filter({ ...patch });
    return;
  }
  const { path, query } = liveQuery();
  for (const [k, v] of Object.entries(patch)) {
    if (v === null || v === "") query.delete(k);
    else query.set(k, v);
  }
  nav.to(path, query);
}

/** Parse what the model proposed into a patch of the screen's own keys, or refuse it whole. */
export function parseUrlPatch(
  raw: unknown,
  keys: Readonly<Record<string, UrlActionKey>>,
): PageActionParse<UrlPatch> {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ok: false, reason: "That is not a change to this page's view." };
  }
  const entries = Object.entries(raw as Record<string, unknown>);
  if (entries.length === 0) return { ok: false, reason: "That change changes nothing." };
  if (entries.length > MAX_KEYS) return { ok: false, reason: "That change is too large." };
  const out: Record<string, string | null> = {};
  for (const [name, value] of entries) {
    const key = keyOf(keys, name);
    if (!key) return { ok: false, reason: `This page has no "${name}" setting.` };
    if (value === null) {
      out[name] = null;
      continue;
    }
    if (
      typeof value !== "string" ||
      value.length > MAX_VALUE ||
      /[\u0000-\u001f\u007f-\u009f]/.test(value)
    ) {
      return { ok: false, reason: `That is not a value ${key.label} takes.` };
    }
    const refused = key.check?.(value) ?? null;
    if (refused !== null) return { ok: false, reason: refused };
    out[name] = value === "" || value === key.fallback ? null : value;
  }
  return { ok: true, value: out };
}

/** An action that takes no arguments: nothing, or an empty object, and the screen's own patch. */
function parsePreset(raw: unknown, preset: () => UrlPatch): PageActionParse<UrlPatch> {
  const empty =
    raw === null ||
    raw === undefined ||
    (typeof raw === "object" && !Array.isArray(raw) && Object.keys(raw).length === 0);
  if (!empty) return { ok: false, reason: "That change takes no settings." };
  const { query } = liveQuery();
  const patch: Record<string, string | null> = {};
  for (const [name, value] of Object.entries(preset())) {
    if (held(query, name) !== (value === "" ? null : value)) patch[name] = value;
  }
  if (Object.keys(patch).length === 0) return { ok: false, reason: "There is nothing to clear." };
  return { ok: true, value: patch };
}

/** A family's member pattern as the engine reads it: `f.` then a member slug. */
function familyPattern(family: string): string {
  return `^${family.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}${MEMBER_SOURCE}$`;
}

/** What one key takes, as the action's arguments state it. */
function keyArgs(key: UrlActionKey): AssistantActionArgs {
  const words = [key.label];
  if (key.hint) words.push(key.hint);
  words.push("null clears it");
  return {
    type: ["string", "null"],
    description: words.join(". ") + ".",
    maxLength: MAX_VALUE,
    ...(key.values && key.values.length > 0 ? { enum: [...key.values] } : {}),
  };
}

/** An example patch: the first key with a value it takes, and a cleared one beside it. */
function urlExamples(keys: Readonly<Record<string, UrlActionKey>>): unknown[] {
  const names = Object.keys(keys).filter((n) => !n.endsWith("."));
  const valued = names.find((n) => (keys[n]!.values?.length ?? 0) > 0);
  if (valued !== undefined) return [{ [valued]: keys[valued]!.values![0]! }];
  return names.length > 0 ? [{ [names[0]!]: null }] : [];
}

/**
 * The arguments a URL action takes, from the very keys its parser reads: an
 * object of those keys and nothing else, each text or null (clear it), with
 * the values a key takes where it can list them; a family (`f.`) by the
 * pattern its members are named by. An action with a preset takes nothing.
 */
export function urlArgs(
  keys: Readonly<Record<string, UrlActionKey>>,
  preset = false,
): AssistantActionArgs {
  if (preset) {
    return {
      type: ["object", "null"],
      description: "Takes no settings: send {} and the page clears what it holds.",
      additionalProperties: false,
      properties: {},
      examples: [{}],
    };
  }
  const properties: Record<string, AssistantActionArgs> = {};
  const patternProperties: Record<string, AssistantActionArgs> = {};
  for (const [name, key] of Object.entries(keys)) {
    if (name.endsWith(".")) patternProperties[familyPattern(name)] = keyArgs(key);
    else properties[name] = keyArgs(key);
  }
  const examples = urlExamples(keys);
  return {
    type: "object",
    description: "The page's own settings to change, by name; leave out what stays.",
    additionalProperties: false,
    properties,
    ...(Object.keys(patternProperties).length > 0 ? { patternProperties } : {}),
    ...(examples.length > 0 ? { examples } : {}),
  };
}

/**
 * A page action that writes address keys, by `id`, ready to spread into a
 * screen's `useAssistantContext` actions.
 */
export function urlAction(
  id: PageActionId,
  options: UrlActionOptions,
): Partial<Record<PageActionId, PageActionSpec<UrlPatch>>> {
  const { keys, nav } = options;
  const words = (name: string, value: string | null) => {
    const key = keyOf(keys, name);
    const read = value ?? key?.fallback ?? null;
    if (read === null) return null;
    return key?.words?.(read) ?? read;
  };
  const spec: PageActionSpec<UrlPatch> = {
    scope: options.scope,
    args: urlArgs(keys, options.preset !== undefined),
    // A VIEW CHANGE IS A CHANGE TO THE ADDRESS, and it tracks the address:
    // a filter the person changed since is theirs. Neither the peek beside
    // it nor a draft open on the page is what it edits.
    tracks: ["query"],
    parse: (raw) => (options.preset ? parsePreset(raw, options.preset) : parseUrlPatch(raw, keys)),
    preview: (patch) => {
      const { query } = liveQuery();
      return Object.entries(patch).map(([name, value]) => ({
        field: keyOf(keys, name)?.label ?? name,
        next: words(name, value) ?? "Cleared",
        current: words(name, held(query, name)),
      }));
    },
    blocked: (patch) => options.blocked?.(patch) ?? null,
    apply: (patch) => write(patch, keys, nav),
    undo: (patch) => {
      const { query } = liveQuery();
      const before: Record<string, string | null> = {};
      for (const name of Object.keys(patch)) before[name] = held(query, name);
      return () => {
        // ONLY WHILE THE ADDRESS STILL HOLDS WHAT THE ACTION WROTE: a key the
        // person changed since is theirs now, and putting the old value back
        // would undo their change rather than the assistant's.
        const now = liveQuery().query;
        const intact = Object.entries(patch).every(
          ([name, value]) => held(now, name) === (value === "" ? null : value),
        );
        if (!intact) return false;
        write(before, keys, nav);
        return true;
      };
    },
  };
  return { [id]: spec };
}

/**
 * The `window=` key a screen's time range writes, held to the screen's own
 * offer: a window the screen would silently replace with its fallback is
 * refused, since the card would otherwise promise a window the page will not
 * show. A section, as `useTimeRange` writes it.
 */
export function windowKey(offer: Offer, now: () => number): UrlActionKey {
  const named: string[] = [...offer.ranges, ...(offer.today ? [TODAY] : [])];
  return {
    label: "Window",
    history: "section",
    fallback: offer.fallback,
    // A FIXED OFFER IS LISTED WHOLE; one that also takes a custom window
    // says how one is written, and `check` stays the rule either way.
    ...(offer.custom
      ? {
          hint: `One of ${named.join(", ")}, or a custom window written <from>/<to> in ISO 8601 UTC`,
        }
      : { values: named }),
    check: (value) =>
      windowParam(parseWindow(value, offer, now())) === value
        ? null
        : "This screen does not offer that window.",
    words: (value) => windowLabel(parseWindow(value, offer, now())),
  };
}

/**
 * A key that takes one of a closed set of values, worded as the control that
 * offers them words each: a chart's grouping, a screen's tab.
 */
export function choiceKey(
  label: string,
  options: readonly { value: string; label: string }[],
  history: UrlKeyHistory,
  fallback?: string,
): UrlActionKey {
  return {
    label,
    history,
    values: options.map((o) => o.value),
    ...(fallback === undefined ? {} : { fallback }),
    check: (value) =>
      options.some((o) => o.value === value) ? null : `${label} has no "${value}" option.`,
    words: (value) => options.find((o) => o.value === value)?.label ?? value,
  };
}

/**
 * The key a `useTab` strip writes: the object's own tabs and nothing else,
 * worded as the strip words them, with its landing tab as the fallback, so a
 * patch to the landing tab clears the key as the strip itself does.
 */
export function tabKey<T extends string>(
  label: string,
  tabs: readonly T[],
  words: Readonly<Record<T, string>>,
  history: UrlKeyHistory = "section",
  landing: T = tabs[0] as T,
): UrlActionKey {
  return choiceKey(
    label,
    tabs.map((value) => ({ value, label: words[value] })),
    history,
    landing,
  );
}
