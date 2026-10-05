/**
 * A phase prompt's outline: which sections it has, where each begins and ends
 * in the source, and how much of the prompt each one is.
 *
 * WHY A MAP, AND WHY ONE FROM THE BUILDER. The outline used to be derived by
 * splitting the prompt on its `##` lines, and a prompt carries other people's
 * markdown verbatim: a chat trigger's "## Triage" and "## Thread context", a
 * pull request's "# Title", a model's own "## Summary" inside the review's
 * evidence or a ledger's reply. Every one of those either escaped the section
 * it was quoted in or swallowed the rest of the prompt under a title the
 * engine never wrote. Only the BUILDER knows where its parts begin and end, so
 * the engine now sends that as a section map (`system_sections`,
 * `user_sections`, `prompt_messages[].sections`) and the top level of the
 * outline IS the map. A heading inside a span still structures the span — it
 * nests under it — but it can no longer leave it.
 *
 * AND WHY THE HEADINGS STILL. A record an older engine wrote carries no map,
 * and neither does a resumed phase; and a map that does not tile its prompt is
 * not one this reader may trust, because slicing by it would cut a section in
 * the wrong place or through a character. Both fall back to the outline the
 * prompt's own headings give, which is right for every prompt whose quoted
 * content holds none.
 *
 * SIZES ARE UTF-8 BYTES, INCLUDING EACH SECTION'S HEADING LINE, so the
 * sections of a half add up to the half exactly and agree with the Turn
 * screen's Context tab, which counts the same bytes. They were UTF-16 "chars"
 * over the bodies only, so they never added up and disagreed with every other
 * size on the turn.
 *
 * PURE FUNCTIONS OVER VALUES, so every rule here — the tiling, the validation,
 * the walk and the find — is exercised without rendering anything.
 */

import type { PromptSection } from "~/protocol/index.ts";
import { headingLines, nestSections, splitSections, type SectionNode } from "./markdown.ts";

/** Which message of the request a section is part of. */
export type Half = "system" | "user";

/** One top-level section of one half, ready to read. */
export interface OutlineSection {
  /** Unique across the whole prompt — `system:<key>` — and stable across renders. */
  id: string;
  half: Half;
  /** The builder's key, or `h<n>` for a section the headings gave. */
  key: string;
  /**
   * The title, as INLINE MARKDOWN (`the \`run_sandbox\` tool`): the map's
   * title, or the heading's own text. "" only for a lead run the headings
   * gave, which has no heading and no builder to name it.
   */
  title: string;
  /** The exact source this section spans: its heading line, its body, the separator after it. */
  source: string;
  /** `source`'s length in UTF-8 bytes. A half's sections sum to the half. */
  bytes: number;
  /** The text under the title and before the first heading inside the span, blank lines trimmed. */
  body: string;
  /** The headings inside the span, nested on their levels — never outside it. */
  children: SectionNode[];
}

/** One half of the request and its outline. */
export interface OutlineHalf {
  half: Half;
  text: string;
  /** The half's length in UTF-8 bytes. */
  bytes: number;
  sections: OutlineSection[];
  /** Where the top level came from: the builder's map, or the prompt's own headings. */
  from: "map" | "headings";
}

const encoder = new TextEncoder();
// FATAL, so a slice that is not whole UTF-8 throws rather than decoding into
// replacement characters a reader would take for the prompt's own.
const decoder = new TextDecoder("utf-8", { fatal: true });

/**
 * A section map off the wire, or null when there is none to read.
 *
 * ALL OR NOTHING: an entry without a string key and title and an integer byte
 * count makes the whole map unreadable, since one dropped entry would shift
 * every boundary after it.
 */
export function decodeSections(raw: unknown): PromptSection[] | null {
  if (!Array.isArray(raw) || raw.length === 0) return null;
  const out: PromptSection[] = [];
  for (const entry of raw as unknown[]) {
    if (entry === null || typeof entry !== "object") return null;
    const { key, title, bytes } = entry as Record<string, unknown>;
    if (typeof key !== "string" || typeof title !== "string") return null;
    if (typeof bytes !== "number" || !Number.isInteger(bytes)) return null;
    out.push({ key, title, bytes });
  }
  return out;
}

/** One span of a half: what the builder or the headings say it is, and its source. */
interface Span {
  key: string;
  title: string;
  source: string;
  bytes: number;
}

/**
 * The spans a section map cuts a half into, or null when the map does not
 * tile it.
 *
 * EVERY INVARIANT THE CONTRACT STATES, checked rather than assumed, because a
 * map that breaks one is not a slightly wrong outline but a mis-sliced one: the
 * byte counts add up to the half; every span has at least one byte; every key
 * is unique and non-empty; and every boundary falls between two characters —
 * the decoder is fatal, so a span that begins or ends inside a multi-byte
 * character refuses the map rather than rendering a replacement glyph.
 */
export function tileByMap(text: string, map: readonly PromptSection[]): Span[] | null {
  const bytes = encoder.encode(text);
  const total = map.reduce((sum, s) => sum + s.bytes, 0);
  if (total !== bytes.length) return null;
  const keys = new Set<string>();
  const out: Span[] = [];
  let at = 0;
  for (const s of map) {
    if (s.bytes <= 0 || s.key === "" || keys.has(s.key)) return null;
    keys.add(s.key);
    let source: string;
    try {
      source = decoder.decode(bytes.subarray(at, at + s.bytes));
    } catch {
      return null;
    }
    out.push({ key: s.key, title: s.title || s.key, source, bytes: s.bytes });
    at += s.bytes;
  }
  return out;
}

/**
 * The spans a half's own headings cut it into: one per heading the outline
 * puts at its top level, plus the lead run before the first.
 *
 * THE TOP LEVEL IS [nestSections]'s, so this outline is the one the headings
 * always gave: a heading is a root when no heading of a LOWER level is open
 * above it, and everything until the next root is its span. The cut is at the
 * start of each root's heading LINE, so the spans tile the source byte for
 * byte, separators included — a blank line between two sections belongs to
 * the one before it, which is where the map puts it too.
 *
 * A BLANK LEAD IS NOT A SECTION. Whitespace before the first heading is folded
 * into the first section's span rather than drawn as a row nobody could name.
 */
export function tileByHeadings(text: string): Span[] {
  const cuts: { at: number; title: string }[] = [];
  const open: number[] = [];
  for (const h of headingLines(text)) {
    while (open.length > 0 && (open[open.length - 1] ?? 0) >= h.level) open.pop();
    if (open.length === 0) cuts.push({ at: h.at, title: h.title });
    open.push(h.level);
  }
  const out: Span[] = [];
  const first = cuts[0]?.at ?? text.length;
  const lead = text.slice(0, first);
  const blankLead = lead.trim() === "" && cuts.length > 0;
  if (lead !== "" && !blankLead) out.push(span("h0", "", lead));
  cuts.forEach((cut, i) => {
    const from = i === 0 && blankLead ? 0 : cut.at;
    const to = cuts[i + 1]?.at ?? text.length;
    out.push(span(`h${i + 1}`, cut.title, text.slice(from, to)));
  });
  return out;
}

function span(key: string, title: string, source: string): Span {
  return { key, title, source, bytes: encoder.encode(source).length };
}

/**
 * One span as a section to read: its own heading taken off (the reader draws
 * the title itself), the text before the first heading inside it, and every
 * heading inside it nested on its level.
 */
function section(half: Half, s: Span): OutlineSection {
  // The span's FIRST section is its own: the heading that opens a headed
  // span (the reader draws its title itself, so only its text is the body),
  // or the run a headless one opens with — a lead run, a builder's untitled
  // part. Every section after it is a heading inside the span.
  const inner = splitSections(s.source);
  const own = inner[0];
  return {
    id: `${half}:${s.key}`,
    half,
    key: s.key,
    title: s.title,
    source: s.source,
    bytes: s.bytes,
    body: own?.body ?? "",
    children: nestSections(inner.slice(1)),
  };
}

/**
 * One half of the request, outlined by its builder's map when it carries a map
 * that tiles it, and by its own headings otherwise.
 */
export function outlineHalf(
  half: Half,
  text: string,
  map: readonly PromptSection[] | null | undefined,
): OutlineHalf {
  const mapped = map && map.length > 0 ? tileByMap(text, map) : null;
  const spans = mapped ?? tileByHeadings(text);
  return {
    half,
    text,
    bytes: encoder.encode(text).length,
    sections: spans.map((s) => section(half, s)),
    from: mapped ? "map" : "headings",
  };
}

/** Every section of the request, the system prompt's first, in reading order. */
export function sectionsOf(halves: readonly OutlineHalf[]): OutlineSection[] {
  return halves.flatMap((h) => h.sections);
}

/** The sections either side of `id` in reading order, across the two halves. */
export function neighbours(
  all: readonly OutlineSection[],
  id: string,
): { prev: OutlineSection | null; next: OutlineSection | null } {
  const i = all.findIndex((s) => s.id === id);
  if (i < 0) return { prev: null, next: null };
  return { prev: all[i - 1] ?? null, next: all[i + 1] ?? null };
}

/**
 * The pattern a find query matches: the query's own characters, every one
 * literal, case-insensitively. `null` for a query of nothing but whitespace,
 * which is no query.
 *
 * ONE PATTERN for counting and for highlighting, so the count a row shows and
 * the marks in the reader are one reading of one query. `u` because the
 * prompt is Unicode and case folding outside ASCII is part of "ignore case".
 */
export function findPattern(query: string): RegExp | null {
  if (query.trim() === "") return null;
  return new RegExp(query.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "giu");
}

/**
 * How many times the query occurs in each section's SOURCE — the prompt the
 * model received, markup and all, rather than the rendering. A count on the
 * rendering would miss what rendering consumes (`**`, a heading's hashes, a
 * fence's info string) and the question a find answers is what the model was
 * told.
 */
export function findCounts(all: readonly OutlineSection[], query: string): Map<string, number> {
  const pattern = findPattern(query);
  const out = new Map<string, number>();
  if (!pattern) return out;
  for (const s of all) {
    const n = s.source.match(pattern)?.length ?? 0;
    if (n > 0) out.set(s.id, n);
  }
  return out;
}

/**
 * The next section after `id` (or before it, `by: -1`) that has a match,
 * wrapping at the ends; null when nothing matches. The current section counts
 * only once the walk has come all the way round to it, so Enter on the only
 * match stays put rather than reporting nothing.
 */
export function stepToMatch(
  all: readonly OutlineSection[],
  counts: ReadonlyMap<string, number>,
  id: string,
  by: 1 | -1,
): OutlineSection | null {
  if (all.length === 0 || counts.size === 0) return null;
  const n = all.length;
  const from = Math.max(
    0,
    all.findIndex((s) => s.id === id),
  );
  for (let step = 1; step <= n; step++) {
    const s = all[(((from + by * step) % n) + n) % n];
    if (s && (counts.get(s.id) ?? 0) > 0) return s;
  }
  return null;
}

/** "23%" of a whole, or "under 1%" for a share that rounds to nothing. */
export function share(part: number, whole: number): string {
  if (whole <= 0) return "0%";
  const pct = (part / whole) * 100;
  if (pct > 0 && pct < 1) return "under 1%";
  return `${Math.round(pct)}%`;
}
