/**
 * Settings › Models & keys, in words and in values: what each state the engine
 * sends means, which seats run on a model, and the edit form's rules.
 *
 * NOTHING HERE DECIDES A STATE. Whether a key is cooling and whether a model can
 * still answer are the engine's (`credential_pool`), read off its pools and the
 * fleet's cooldown ledger; which seats run on a model is the engine's too — the
 * org projection carries each seat's resolved chain per phase. This file names
 * them and builds the one write the screen makes.
 */

import type {
  CredentialKeyRow,
  CredentialKeySource,
  CredentialKeyState,
  CredentialPoolRow,
  CredentialPoolState,
} from "~/contract/credentials.ts";
import type { Seat } from "~/lib/seats.ts";
import { REDACTED } from "~/lib/format.ts";
import type { ConfigProblem } from "~/protocol/types.ts";

type Tone = "success" | "neutral" | "warning" | "danger";

/**
 * A model's condition, in words, with what to do about it.
 *
 * EXHAUSTIVE OVER THE CONTRACT'S UNION, so a state the engine adds fails the
 * typecheck here rather than drawing as nothing.
 */
export const POOL_STATE_WORDS: Record<
  CredentialPoolState,
  { label: string; tone: Tone; hint: string }
> = {
  ready: {
    label: "Ready",
    tone: "success",
    hint: "Every key resolves and none is cooling.",
  },
  degraded: {
    label: "Fewer keys",
    tone: "warning",
    hint: "Calls still land, on fewer keys than configured: some are cooling or resolve to nothing.",
  },
  exhausted: {
    label: "All keys cooling",
    tone: "danger",
    hint: "Every key is benched, so each call falls through to the seat's next model until one lifts.",
  },
  no_key: {
    label: "No key",
    tone: "danger",
    hint: "No key resolves on this node, so every call is refused as unauthorised. Store the secret its key names, or point it at one that exists.",
  },
  login: {
    label: "CLI login",
    tone: "neutral",
    hint: "A coding CLI's own login, held on disk: there is no key bag to rotate and nothing cools.",
  },
};

/** One key's condition, in words. Exhaustive for the reason above. */
export const KEY_STATE_WORDS: Record<CredentialKeyState, { label: string; tone: Tone }> = {
  ready: { label: "Ready", tone: "success" },
  cooling: { label: "Cooling", tone: "warning" },
  unresolved: { label: "Not set", tone: "danger" },
  duplicate: { label: "Duplicate", tone: "neutral" },
};

/** Whether a model needs somebody: every state that is not one to leave alone. */
export function needsAttention(row: CredentialPoolRow): boolean {
  return row.state === "degraded" || row.state === "exhausted" || row.state === "no_key";
}

/**
 * Where a key's value comes from, in words, for its title. Exhaustive over the
 * contract's union for the reason above.
 */
export const KEY_SOURCE_WORDS: Record<CredentialKeySource, string> = {
  reference: "a secret or environment variable this model names",
  default: "the vendor's own variable, read because this model names no keys",
  inline: "a value written into the configuration itself, never sent here",
};

/**
 * What a key is called on screen: its variable, the conventional variable it
 * defaults to, or its position when it is written into the document and has
 * no name. Never a value — there is none in the answer to show.
 */
export function keyName(key: CredentialKeyRow, position: number): string {
  if (key.source === "inline") return `Key ${position} (written inline)`;
  return key.ref;
}

/**
 * What a key's MARK prints on the models list: its variable, or — for a key
 * written into the document, which has no name — its place and where it
 * lives. The bare "#1" this replaced read as a cipher beside the named marks,
 * while the model's own page called the same key "Key 1 (written inline)".
 */
export function keyMark(key: CredentialKeyRow, position: number): string {
  return key.source === "inline" ? `key ${position} · inline` : key.ref;
}

/**
 * What a key's "Back" cell says when it carries no deadline: a ready key is in
 * the pool now, a duplicate is in it ONCE — as the key it repeats — and only an
 * unresolved key is not in the pool at all.
 */
export function keyBackWords(key: CredentialKeyRow): string {
  switch (key.state) {
    case "ready":
      return "Now";
    case "duplicate":
      return `As key ${key.same_as}`;
    case "cooling":
    case "unresolved":
      return "Not in the pool";
  }
}

/** Why a key is in the state it is, in one sentence for its row. */
export function keyWhy(key: CredentialKeyRow): string {
  switch (key.state) {
    case "ready":
      return key.source === "default"
        ? "Read from the vendor's own variable, because this model names no keys."
        : "In the pool.";
    case "cooling":
      return "Benched after the vendor refused it — a rate limit or an auth failure — by this node or a peer.";
    case "unresolved":
      return key.source === "inline"
        ? "Written empty."
        : `Nothing sets ${key.ref} on this node — no stored secret and no environment variable.`;
    case "duplicate":
      return `The same key as key ${key.same_as}: the pool holds it once.`;
  }
}

/** The earliest instant any key in these models lifts, or null. */
export function nextLift(rows: readonly CredentialPoolRow[]): string | null {
  let best: string | null = null;
  let bestAt = Infinity;
  for (const row of rows) {
    for (const key of row.keys) {
      if (key.state !== "cooling" || !key.cooling_until) continue;
      const at = Date.parse(key.cooling_until);
      if (at < bestAt) {
        bestAt = at;
        best = key.cooling_until;
      }
    }
  }
  return best;
}

/**
 * A bench time as the dashboard writes every span: unit letters, no space —
 * "1h", "5m", "1h 30m", "90s" — the convention `fmtDuration` and a cooling
 * key's "41m" already keep, so the bench, the chips and the tile beside them
 * read as one clock rather than three.
 */
export function benchWords(seconds: number): string {
  if (!(seconds > 0)) return "0s";
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  const parts: string[] = [];
  if (h) parts.push(`${h}h`);
  if (m) parts.push(`${m}m`);
  if (s) parts.push(`${s}s`);
  return parts.join(" ");
}

/**
 * A model's two bench times, NAMED FOR WHAT BENCHES A KEY rather than by the
 * status code: "429" and "401" are what a reader has to already know. One
 * list for every place the bench is drawn — the list's column, the model's
 * header — so it is written one way; the codes ride in `title`.
 */
export function benchParts(row: {
  rate_limit_seconds: number;
  auth_seconds: number;
}): { key: string; words: string; title: string }[] {
  return [
    {
      key: "rate",
      words: `rate limit ${benchWords(row.rate_limit_seconds)}`,
      title: `A key the vendor answers 429 (rate limited) is left out for ${benchWords(row.rate_limit_seconds)}, or as long as its Retry-After says`,
    },
    {
      key: "auth",
      words: `auth ${benchWords(row.auth_seconds)}`,
      title: `A key the vendor answers 401 or 403 (refused) is left out for ${benchWords(row.auth_seconds)}, doubling on each repeat`,
    },
  ];
}

/**
 * The line under a model's key: its type, and the vendor's model id when it
 * names one. A cli-agent entry names none, and "cli-agent ·" was a separator
 * with nothing after it.
 */
export function modelLine(row: { type: string; model: string }): string {
  return row.model.trim() ? `${row.type} · ${row.model}` : row.type;
}

/**
 * The phases a seat's chain names, in the engine's own order (the executor
 * first, because it is what the seat IS running on), as words. A phase this
 * build has no word for is drawn as its wire name, after the known ones.
 */
const PHASE_WORDS: ReadonlyArray<readonly [string, string]> = [
  ["execute", "executor"],
  ["review", "reviewer"],
  ["subagent", "workers"],
  ["auxiliary", "auxiliary"],
  ["judge", "judge"],
  ["sandbox", "coding runs"],
  ["onboarding", "onboarding"],
];

/** One seat's use of one model. */
export interface ModelUse {
  seat: Seat;
  /** Phases whose chain STARTS with this model — what the seat runs them on. */
  runs: string[];
  /** Phases that fall back to it after another model. */
  fallback: string[];
  /** Every phase the seat has runs on this model first: said as one fact, not a list of seven. */
  every: boolean;
}

/**
 * Which seats run on a model, and for which phases — read off the chain the
 * engine resolved for each seat (`llm` on the org projection), never worked out
 * again here. A seat whose projection carries no chain (a human, or a company
 * with no providers) uses nothing.
 */
export function usesOf(seats: readonly Seat[], key: string): ModelUse[] {
  const out: ModelUse[] = [];
  const known = new Set(PHASE_WORDS.map(([phase]) => phase));
  for (const seat of seats) {
    const chains = seat.raw.llm;
    if (!chains) continue;
    const phases = [
      ...PHASE_WORDS.filter(([phase]) => phase in chains),
      ...Object.keys(chains)
        .filter((phase) => !known.has(phase))
        .sort()
        .map((phase) => [phase, phase] as const),
    ];
    const runs: string[] = [];
    const fallback: string[] = [];
    for (const [phase, word] of phases) {
      const at = (chains[phase] ?? []).indexOf(key);
      if (at < 0) continue;
      (at === 0 ? runs : fallback).push(word);
    }
    if (runs.length || fallback.length) {
      out.push({ seat, runs, fallback, every: runs.length === phases.length });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// The edit form
// ---------------------------------------------------------------------------

/** One key row of the form, with a client id so a removal keeps its neighbours' state. */
export interface KeyField {
  id: string;
  /** A `${NAME}`, or [REDACTED] for a value written into the document. */
  value: string;
}

let nextKeyId = 0;

/** A fresh key row. */
export function newKeyField(value = ""): KeyField {
  nextKeyId += 1;
  return { id: `key-${nextKeyId}`, value };
}

/** What the form edits: the parts of an entry a person changes by hand. */
export interface ModelForm {
  model: string;
  keys: KeyField[];
  baseUrl: string;
  /** Seconds, as typed; empty is the default. */
  rateLimit: string;
  auth: string;
}

export type ModelField = "model" | "keys" | "baseUrl" | "rateLimit" | "auth";

/** The form over an entity as the engine served it (redacted). */
export function formOf(entity: Record<string, unknown>): ModelForm {
  const cooldowns = isRecord(entity.cooldowns) ? entity.cooldowns : {};
  const keys = Array.isArray(entity.api_keys) ? entity.api_keys : [];
  return {
    model: typeof entity.model === "string" ? entity.model : "",
    keys: keys.filter((k): k is string => typeof k === "string").map((k) => newKeyField(k)),
    baseUrl: typeof entity.base_url === "string" ? entity.base_url : "",
    rateLimit: secondsText(cooldowns.rate_limit_seconds),
    auth: secondsText(cooldowns.auth_seconds),
  };
}

/** The bounds the engine holds a bench time to (`internal/config`'s). */
export const MIN_BENCH_SECONDS = 60;
export const MAX_BENCH_SECONDS = 86_400;

/**
 * Whether a key list still says, BY POSITION, which value each inline key
 * hid.
 *
 * A key written into the document reaches this page as the mask, and the
 * engine puts its value back by its PLACE in the list (`api_keys` is a list
 * of anonymous values, so position is the only correspondence it has —
 * `config.Company.RestoreRedacted`). So while any mask is in the list the
 * list keeps the length it was read at and every mask stays where it was
 * read. The engine refuses a list that changed length; one that kept its
 * length with a mask moved is WORSE — the mask is restored from whatever
 * value used to stand at its new place, and a key is silently swapped for
 * another with nothing refused (`[${A}, <inline>]`, remove key 1, add
 * `${C}`: the inline key becomes `${A}`).
 */
export function keysKeepInlinePlaces(
  read: readonly KeyField[],
  keys: readonly KeyField[],
): boolean {
  if (!keys.some((k) => k.value === REDACTED)) return true;
  if (keys.length !== read.length) return false;
  return keys.every((k, i) => k.value !== REDACTED || read[i]?.value === REDACTED);
}

/** The first inline key's number (1-based) in a list, or 0 for none. */
export function firstInlineKey(keys: readonly KeyField[]): number {
  return keys.findIndex((k) => k.value === REDACTED) + 1;
}

/** Why a key list cannot grow or shrink while it holds an inline key. */
export function inlinePlaceWords(keys: readonly KeyField[]): string {
  return `Key ${firstInlineKey(keys)} is written inline and is kept by its place in the list. Replace it with a \${NAME} from Secrets before adding or removing a key.`;
}

/**
 * What stops a save before the engine is asked. The engine validates the
 * whole company again; this is only what a person can see is wrong while
 * typing — and the one thing the engine cannot see at all, an inline key
 * moved to another key's place (see [keysKeepInlinePlaces]).
 */
export function formErrors(form: ModelForm, read: ModelForm): Partial<Record<ModelField, string>> {
  const out: Partial<Record<ModelField, string>> = {};
  if (!form.model.trim()) out.model = "Name the model this entry serves.";
  if (form.keys.some((k) => !k.value.trim())) {
    out.keys = "A key row is empty. Fill it in or remove it.";
  } else if (!keysKeepInlinePlaces(read.keys, form.keys)) {
    out.keys = inlinePlaceWords(form.keys);
  }
  for (const field of ["rateLimit", "auth"] as const) {
    const text = form[field].trim();
    if (!text) continue;
    const n = Number(text);
    if (!Number.isInteger(n) || n < MIN_BENCH_SECONDS || n > MAX_BENCH_SECONDS) {
      out[field] =
        `A whole number of seconds from ${MIN_BENCH_SECONDS} to ${MAX_BENCH_SECONDS}, or empty for the default.`;
    }
  }
  return out;
}

/**
 * The entity to send: the one the engine served, with only the edited fields
 * replaced. Everything the form does not show — the type, reasoning, the
 * timeout, a cli-agent's block — goes back exactly as it came, and a key the
 * document holds inline goes back as the mask, which the engine restores from
 * the revision this was read from.
 */
export function modelEntity(
  entity: Record<string, unknown>,
  form: ModelForm,
): Record<string, unknown> {
  const out: Record<string, unknown> = { ...entity, model: form.model.trim() };
  const keys = form.keys.map((k) => k.value.trim()).filter(Boolean);
  if (keys.length) out.api_keys = keys;
  else delete out.api_keys;
  if (form.baseUrl.trim()) out.base_url = form.baseUrl.trim();
  else delete out.base_url;
  const cooldowns: Record<string, number> = {};
  if (form.rateLimit.trim()) cooldowns.rate_limit_seconds = Number(form.rateLimit.trim());
  if (form.auth.trim()) cooldowns.auth_seconds = Number(form.auth.trim());
  if (Object.keys(cooldowns).length) out.cooldowns = cooldowns;
  else delete out.cooldowns;
  return out;
}

/**
 * The revision's audit summary: which model, and what moved. Key VALUES are
 * never in it — a count and the variable names that came and went.
 */
export function editSummary(key: string, before: ModelForm, after: ModelForm): string {
  const changes: string[] = [];
  if (before.model.trim() !== after.model.trim()) {
    changes.push(`model ${before.model.trim() || "unset"} → ${after.model.trim()}`);
  }
  const was = before.keys.map((k) => k.value.trim()).filter(Boolean);
  const now = after.keys.map((k) => k.value.trim()).filter(Boolean);
  const added = now.filter((k) => !was.includes(k) && k !== REDACTED);
  const removed = was.filter((k) => !now.includes(k) && k !== REDACTED);
  if (added.length) changes.push(`keys added: ${added.join(", ")}`);
  if (removed.length) changes.push(`keys removed: ${removed.join(", ")}`);
  if (!added.length && !removed.length && was.join("\n") !== now.join("\n")) {
    changes.push(
      now.length === was.length ? "keys reordered" : `keys ${was.length} → ${now.length}`,
    );
  }
  if (before.baseUrl.trim() !== after.baseUrl.trim()) {
    changes.push(after.baseUrl.trim() ? `endpoint ${after.baseUrl.trim()}` : "endpoint cleared");
  }
  for (const [field, word] of [
    ["rateLimit", "rate-limit bench"],
    ["auth", "auth bench"],
  ] as const) {
    if (before[field].trim() !== after[field].trim()) {
      changes.push(`${word} ${benchOrDefault(before[field])} → ${benchOrDefault(after[field])}`);
    }
  }
  return changes.length ? `Edit the model ${key}: ${changes.join("; ")}` : `Edit the model ${key}`;
}

/** Whether the form differs from what was read. */
export function formChanged(before: ModelForm, after: ModelForm): boolean {
  const flat = (f: ModelForm) =>
    JSON.stringify([
      f.model.trim(),
      f.keys.map((k) => k.value.trim()),
      f.baseUrl.trim(),
      f.rateLimit.trim(),
      f.auth.trim(),
    ]);
  return flat(before) !== flat(after);
}

/**
 * The engine's problems placed beside the field each is about, by the
 * problem's own segments — `["providers", "llm", "smart", "api_keys", 1]` is
 * the keys — never its message. The rest is said above the form.
 *
 * A problem on a key this page SENT AS THE MASK is said in this screen's own
 * words: the engine could not put an inline key's value back, and its line
 * for that is written for the whole document (seats, units, MCP servers),
 * which means nothing beside a model's keys. Recognised by what was sent at
 * that place, never by the message.
 */
export function problemsByField(
  problems: readonly ConfigProblem[],
  key: string,
  sent: Record<string, unknown>,
): { fields: Partial<Record<ModelField, string>>; rest: string[] } {
  const fields: Partial<Record<ModelField, string>> = {};
  const rest: string[] = [];
  const keys = Array.isArray(sent.api_keys) ? sent.api_keys : [];
  for (const p of problems) {
    const seg = p.segments ?? [];
    const field =
      seg[0] === "providers" && seg[1] === "llm" && seg[2] === key && typeof seg[3] === "string"
        ? fieldOf(seg[3], seg[4])
        : null;
    const masked = field === "keys" && typeof seg[4] === "number" && keys[seg[4]] === REDACTED;
    const message = masked
      ? `Key ${Number(seg[4]) + 1} is written inline and its value could not be put back, because the list no longer says by its place which key it is. Put the keys back as they were, or replace key ${Number(seg[4]) + 1} with a \${NAME} from Secrets.`
      : p.message;
    if (field && !fields[field]) fields[field] = message;
    else rest.push(message);
  }
  return { fields, rest };
}

function fieldOf(segment: string, next: string | number | undefined): ModelField | null {
  switch (segment) {
    case "model":
      return "model";
    case "api_keys":
      return "keys";
    case "base_url":
      return "baseUrl";
    case "cooldowns":
      return next === "auth_seconds" ? "auth" : next === "rate_limit_seconds" ? "rateLimit" : null;
    default:
      return null;
  }
}

function benchOrDefault(text: string): string {
  return text.trim() ? `${text.trim()} s` : "default";
}

function secondsText(value: unknown): string {
  return typeof value === "number" && value > 0 ? String(value) : "";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
