/**
 * Formatting, and the ordering rules that go with it.
 *
 * The ordering half is load-bearing. Timestamps arrive in two encodings —
 * aware (`…+00:00`) and naive — often for the same instant, and Go's
 * `RFC3339Nano` trims trailing zeros, so a raw string compare puts `…:07Z`
 * before `…:07.42Z` by comparing `'Z'` (0x5A) against `'.'` (0x2E) and orders
 * the LATER instant first. Store rows are additionally microsecond-truncated
 * while live rows keep nanoseconds. Every list in this product therefore sorts
 * through `tsKey`, never through `<` on the string.
 */

/** Parse an ISO timestamp, tolerating a naive one (the engine emits both). */
export function parseUTC(ts: string | null | undefined): Date | null {
  if (!ts) return null;
  let s = String(ts);
  if (!/[zZ]|[+-]\d\d:?\d\d$/.test(s)) s += "Z";
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? null : d;
}

/** The ONE ordering key for a timestamp: epoch ms, or 0 when unparseable. */
export function tsKey(ts: string | null | undefined): number {
  const at = parseUTC(ts);
  return at ? at.getTime() : 0;
}

/**
 * Descending `(instant, id)` comparator — feed order.
 *
 * The id tiebreak is not optional: burst writes share a timestamp at
 * microsecond resolution, and a merge keyed on a non-unique value drops or
 * duplicates whatever collided with it. Returns 0 for genuinely equal rows, so
 * the sort stays transitive and a stable sort can do its job.
 */
export function newestFirst(
  a: { timestamp?: string; id?: string },
  b: { timestamp?: string; id?: string },
): number {
  const at = tsKey(a?.timestamp);
  const bt = tsKey(b?.timestamp);
  if (at !== bt) return bt - at;
  const ai = String(a?.id ?? "");
  const bi = String(b?.id ?? "");
  return ai < bi ? 1 : ai > bi ? -1 : 0;
}

/** Ascending `(instant, id)` comparator — trace and transcript order. */
export function oldestFirst(
  a: { timestamp?: string; id?: string },
  b: { timestamp?: string; id?: string },
): number {
  return -newestFirst(a, b);
}

// ---------------------------------------------------------------------------
// Time
// ---------------------------------------------------------------------------

export function fmtTime(ts: string | null | undefined): string {
  const d = parseUTC(ts);
  if (!d) return "";
  return d.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

export function fmtDateTime(ts: string | null | undefined): string {
  const d = parseUTC(ts);
  if (!d) return "—";
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

export function fmtDate(ts: string | null | undefined): string {
  const d = parseUTC(ts);
  if (!d) return "—";
  return d.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "2-digit" });
}

/** A duration in ms as the shortest honest string. */
export function fmtDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s < 10 ? s.toFixed(1) : Math.round(s)} s`;
  const m = Math.floor(s / 60);
  const rem = Math.round(s % 60);
  if (m < 60) return `${m}m ${rem}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

/** Elapsed between two ISO stamps, or null when either is missing. */
export function elapsedMs(from?: string | null, to?: string | null): number | null {
  const a = tsKey(from);
  const b = tsKey(to);
  if (!a || !b) return null;
  return b - a;
}

// ---------------------------------------------------------------------------
// Numbers
// ---------------------------------------------------------------------------

/**
 * A count, abbreviated once it stops being readable in full.
 *
 * The threshold is 10,000 rather than 1,000: a four-digit token count is
 * something an operator reads exactly, and rounding it to "1.2k" throws away
 * the digit they were looking at.
 */
export function fmtCount(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "—";
  const abs = Math.abs(n);
  if (abs < 10_000) return n.toLocaleString();
  if (abs < 1_000_000) return `${(n / 1000).toFixed(abs < 100_000 ? 1 : 0)}k`;
  if (abs < 1_000_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  return `${(n / 1_000_000_000).toFixed(2)}B`;
}

/** Always the exact figure, grouped. For a cell a reader is comparing. */
export function fmtExact(n: number | null | undefined): string {
  return n == null || !Number.isFinite(n) ? "—" : n.toLocaleString();
}

export function fmtPct(part: number, whole: number, digits = 0): string {
  if (!whole) return "—";
  return `${((part / whole) * 100).toFixed(digits)}%`;
}

export function fmtBytes(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "—";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${v < 10 && u > 0 ? v.toFixed(1) : Math.round(v)} ${units[u]}`;
}

// ---------------------------------------------------------------------------
// Text
// ---------------------------------------------------------------------------

/** A human label from a snake_case or dotted engine identifier. */
export function humanize(key: string | null | undefined): string {
  if (!key) return "";
  return String(key)
    .replace(/[._]/g, " ")
    .replace(/\s+/g, " ")
    .trim()
    .replace(/^\w/, (c) => c.toUpperCase());
}

/**
 * A conversation key's two halves.
 *
 * The grammar is `{source}:{local}` — `jira:POC-7`, `slack:C9:1718.001`,
 * `github:acme/api#42` — and the local half may itself contain colons, so this
 * splits ONCE.
 */
export function splitConversationKey(key: string): { source: string; local: string } {
  const idx = (key ?? "").indexOf(":");
  if (idx < 0) return { source: "", local: key ?? "" };
  return { source: key.slice(0, idx), local: key.slice(idx + 1) };
}

/**
 * "1 seat", "2 seats" — a count and its noun, agreeing.
 *
 * A helper rather than a ternary at every call site: the previous product
 * printed "1 humans", "1 seats" and "1 phases loaded" on three different
 * screens, which is the kind of thing nobody fixes one at a time.
 */
export function plural(n: number, one: string, many?: string): string {
  return `${n.toLocaleString()} ${n === 1 ? one : (many ?? `${one}s`)}`;
}

// ---------------------------------------------------------------------------
// Configuration values
// ---------------------------------------------------------------------------

/**
 * The order the engine declares a seat's per-phase chains in
 * (`config.PhaseLLM`), so a mapping renders in the order its reader wrote it
 * against rather than in whatever order a JSON object happened to arrive.
 */
const PHASE_ORDER = ["default", "review", "subagent", "auxiliary", "judge", "sandbox"];

/** One row of a seat's model setting: which phase, and the chain it runs on. */
export interface PhaseChain {
  /** The mapping key, or "" when one chain covers every phase. */
  phase: string;
  /** The provider keys, first choice first, joined for reading. */
  chain: string;
}

/**
 * A seat's `llm:` field as rows a person reads.
 *
 * THREE SHAPES, ONE READING. A key and a chain are one row covering every
 * phase; a per-phase mapping is one row per phase it names. The seat screen
 * used to render the field as a React child, which drew a chain as its keys
 * glued together and threw on the mapping, taking the whole page with it.
 *
 * A fallback chain reads as "first, then second": the order is the whole
 * meaning of a chain, and a bare comma list reads as a set.
 *
 * `unknown` in, because this is the one reader standing between a field a
 * newer engine may shape differently and a render that must not throw.
 */
export function formatPhaseLLM(llm: unknown): PhaseChain[] {
  const chain = (keys: unknown): string => {
    if (typeof keys === "string") return keys.trim();
    if (!Array.isArray(keys)) return "";
    return keys
      .filter((k): k is string => typeof k === "string" && k.trim() !== "")
      .map((k) => k.trim())
      .join(", then ");
  };
  if (typeof llm === "string" || Array.isArray(llm)) {
    const only = chain(llm);
    return only ? [{ phase: "", chain: only }] : [];
  }
  if (!llm || typeof llm !== "object") return [];
  const rank = (phase: string) => {
    const at = PHASE_ORDER.indexOf(phase);
    return at < 0 ? PHASE_ORDER.length : at;
  };
  return Object.entries(llm as Record<string, unknown>)
    .map(([phase, keys], i) => ({ phase, chain: chain(keys), i }))
    .filter((row) => row.chain !== "")
    .sort((a, b) => rank(a.phase) - rank(b.phase) || a.i - b.i)
    .map(({ phase, chain: joined }) => ({ phase, chain: joined }));
}

/**
 * The mask the engine writes in place of a credential it will not send.
 *
 * `config.Redacted` in Go. A redacted document carries this in every
 * credential field that holds a literal; a field holding one whole `${VAR}`
 * reference carries the reference, which names a secret rather than being one.
 */
export const REDACTED = "__redacted__";

/** How a value read from the redacted document may be shown. */
export type ConfigValueKind = "empty" | "hidden" | "reference" | "literal";

/**
 * What a value read from the redacted document is, for rendering.
 *
 * `hidden` is the mask: a literal is set and its value is not on the wire. A
 * `reference` is ONE whole `${NAME}`, the only form the engine sends unmasked
 * in a credential field. Anything else is a `literal`.
 *
 * `secret` says the value comes from a CREDENTIAL field, and there a literal
 * is `hidden` as well. The engine is meant never to send one, but its
 * redaction once treated any value containing `${` as a reference, so
 * `Bearer sk-live-${SUFFIX}` reached the wire with its literal half intact.
 * What this page prints must not depend on every engine it talks to having
 * got that right: a credential field shows a whole reference or nothing.
 */
export function configValueKind(
  value: string | null | undefined,
  { secret = false }: { secret?: boolean } = {},
): ConfigValueKind {
  if (value == null || value === "") return "empty";
  if (value === REDACTED) return "hidden";
  // The grammar and the trim are `envref.Whole`'s, so what this calls a
  // reference is exactly what the engine resolves as one.
  if (/^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/.test(value.trim())) return "reference";
  return secret ? "hidden" : "literal";
}
