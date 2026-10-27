/**
 * Which records an answer names, and where: the drawer draws no card for a
 * record the turn read. The first time the answer's words name one, that
 * mention is the record's chip; what the turn read and never named is listed
 * on one line under the answer.
 *
 * THE RECORDS ARE THE TURN'S OWN READS, never the model's words. A read is a
 * table or a record card the engine built from a tool's answer; the answer's
 * prose only says where a read is mentioned. A key the prose spells that the
 * turn never read stays text, so nothing the model writes becomes a link.
 *
 * ONE RECORD, HOWEVER OFTEN IT WAS READ. Reads are merged by what they are (a
 * record's kind and id, a table's title and window), in the order the turn
 * first read each, and the LAST card of each stands for it: it is the
 * freshest.
 *
 * THE MATCHING RULE, decided once, here:
 *
 *  1. A KEY is matched exactly, case and all, at token boundaries: no letter,
 *     digit, underscore or hyphen on either side, so GOP-11 is never found
 *     in GOP-110 or GOP-11-b. A slash delimits tokens, so both keys of
 *     "GOP-10/GOP-11" are found. A record carries a key where it has one (a
 *     task's, a project's: `AssistantEntityCard.key`).
 *  2. A NAME is matched only for the kinds whose title is a name people
 *     write (an agent, a team, a project, a page, a space, an artifact, a
 *     schedule), exactly, as a whole phrase at the same boundaries, and only
 *     two characters or longer. Never a task's title, which is a sentence
 *     (a task is matched by its key), and never a table's, a record of no
 *     kind or a read too large to show: those are listed under the answer,
 *     a list as one chip named by what it read and how many rows.
 *     - A ONE-WORD NAME IN SENTENCE CASE IS NEVER A SENTENCE'S FIRST WORD:
 *       "Design is late" and "Notes from the call" start a sentence with a
 *       capital the way every sentence does, and are not the team or the
 *       page. A name no ordinary word is spelled like (all capitals, a
 *       capital inside it, a digit: "QA", "GoPhrase", "iOS"), a name of two
 *       words or more, and a key are matched there.
 *     - A NAME TWO READS SHARE NAMES NEITHER: two seats both called "QA"
 *       cannot be told apart by the word, so neither is a chip and both are
 *       listed under the answer. A record with a key is still matched by it.
 *  3. NOTHING IN AN ADDRESS OR A PATH: a word (a run of text between spaces,
 *     its surrounding brackets, quotes and stops aside) that is a bare
 *     address (a scheme's `://`, a `www.` host) or a path (one that starts
 *     with `/`, `./` or `../`, or has a slash after a part that is no key) is
 *     something the model wrote out, and a key in it (https://x.test/GOP-11,
 *     /worklet/GOP-11, worklet/GOP-11) is part of it, never a mention.
 *  4. In each run of text, the earliest mention wins; where two start at one
 *     place, the longer. Only a record's FIRST mention, in reading order, is
 *     its chip; the rest stay text.
 *  5. Only the prose is read: the Markdown renderer hands over each run of
 *     the words a reader reads (`RenderOptions.prose`), never a code span,
 *     a code block or a link's label, so a key in `code` is the model
 *     quoting it, not naming it. It says which run opens a block, so a
 *     sentence's first word is known across a paragraph, a heading, a list
 *     item and a table cell.
 */

import type { ReactNode } from "react";
import type { AssistantCardEvent } from "~/contract/assistant.ts";
import { narrowCard } from "~/protocol/assistantWire.ts";
import { fmtExact } from "~/lib/format.ts";

type EntityKind = NonNullable<NonNullable<AssistantCardEvent["card"]["entity"]>["kind"]>;

/** The kinds whose title is a name an answer writes, and so is matched (rule 2). */
const NAMED_KINDS: ReadonlySet<EntityKind> = new Set<EntityKind>([
  "seat",
  "unit",
  "project",
  "page",
  "container",
  "artifact",
  "schedule",
]);

/** The shortest name matched in prose: a one-letter name would match a word. */
export const MIN_NAME_LENGTH = 2;

/** One record, or one table, the turn read. */
export interface Read {
  /** What makes two reads one: a record's kind and id, a table's title and window. */
  id: string;
  /** The turn's last card of it, the freshest. */
  event: AssistantCardEvent;
  /** The key prose writes it by, matched by rule 1. */
  key: string | undefined;
  /** The name prose writes it by, matched by rule 2. */
  name: string | undefined;
  /** What its chip says where the answer did not name it: its key, else its title. */
  label: string;
  /** Its full name. */
  title: string;
}

/** One mention: the read it names, the words it names it with, and how. */
export interface Mention {
  read: Read;
  text: string;
  by: "key" | "name";
}

/**
 * How much a list read holds, as its chip says it: "8", or "first 50 of 210"
 * where the card holds only the first rows, or "first 20" where the answer
 * says more lie past its page.
 */
function tableCount(t: {
  rows: unknown[];
  row_count: number;
  truncated: boolean;
  more?: boolean;
}): string {
  if (t.truncated) return `first ${fmtExact(t.rows.length)} of ${fmtExact(t.row_count)}`;
  if (t.more) return `first ${fmtExact(t.row_count)}`;
  return fmtExact(t.row_count);
}

function readOf(event: AssistantCardEvent): Read | null {
  const card = narrowCard(event.card);
  if (card === null) return null;
  if (card.type === "table") {
    const t = card.table;
    return {
      id: `table:${t.title}\u0000${t.window ?? ""}`,
      event,
      key: undefined,
      name: undefined,
      // ONE CHIP FOR THE LIST, named by what it read and how much: "Matching
      // tasks (10)", never a chip per row.
      label: `${t.title} (${tableCount(t)})`,
      title: t.title,
    };
  }
  if (card.type === "oversize") {
    const o = card.oversize;
    // A READ PAST WHAT ONE STEP READS IS STILL A READ: named for what it
    // read, it is listed with the others, and its peek says where all of it is.
    return {
      id: `oversize:${o.title}`,
      event,
      key: undefined,
      name: undefined,
      label: `${o.title} (too large to show here)`,
      title: o.title,
    };
  }
  if (card.type === "entity") {
    const e = card.entity;
    const key = e.key?.trim() || undefined;
    const name =
      e.kind && NAMED_KINDS.has(e.kind) && e.title.trim().length >= MIN_NAME_LENGTH
        ? e.title.trim()
        : undefined;
    return {
      id: `${e.kind ?? "record"}:${e.id}`,
      event,
      key,
      name,
      label: key ?? e.title,
      title: e.title,
    };
  }
  return null;
}

/**
 * An answer's cards as the records it read, one each in the order first
 * read, and every other card (a proposal, a refusal, a notice, a file, a
 * brief, an open in app) as it came.
 */
export function readsOf(cards: readonly AssistantCardEvent[]): {
  reads: Read[];
  others: AssistantCardEvent[];
} {
  const reads = new Map<string, Read>();
  const others: AssistantCardEvent[] = [];
  for (const event of cards) {
    const read = readOf(event);
    if (read === null) {
      others.push(event);
      continue;
    }
    // A MAP KEEPS THE FIRST INSERTION'S PLACE: setting it again keeps the
    // order the turn first read it in and takes the fresher card.
    reads.set(read.id, read);
  }
  return { reads: [...reads.values()], others };
}

function escape(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * A word character on either side ends a match: a letter, a digit, an
 * underscore or a hyphen (rule 1). A slash delimits.
 */
const BEFORE = "(?<![\\p{L}\\p{N}_-])";
const AFTER = "(?![\\p{L}\\p{N}_-])";

/** A word of the run: what lies between spaces. */
const WORD = /\S+/gu;
/** What surrounds a word without being part of it: brackets, quotes, stops. */
const AROUND = /^["'\u201c\u2018([{<]*(.*?)[.,;:!?"'\u201d\u2019)\]}>]*$/su;
/** A bare address: a scheme's `://` or a `www.` host. */
const ADDRESS = /^(?:[a-z][a-z0-9+.-]*:\/\/|www\.)/iu;
/** A part of a word a key could be: a capital, then capitals and digits, then a number. */
const KEY_SHAPED = /^\p{Lu}[\p{Lu}\p{N}]*(?:-\p{N}+)?$/u;

/** Whether a word is an address or a path, which nothing in is a mention (rule 3). */
function writtenOut(word: string): boolean {
  if (ADDRESS.test(word) || /^\.{0,2}\//u.test(word)) return true;
  // A SLASH AFTER A PART THAT IS NO KEY: "worklet/GOP-11" is a path, and
  // "GOP-10/GOP-11" two keys.
  const parts = word.split("/");
  return parts.slice(0, -1).some((part) => !KEY_SHAPED.test(part));
}

/** Where each address or path in `text` lies, as [start, end) (rule 3). */
function addresses(text: string): Array<readonly [number, number]> {
  const out: Array<readonly [number, number]> = [];
  for (const m of text.matchAll(WORD)) {
    const word = AROUND.exec(m[0])?.[1] ?? m[0];
    if (word !== "" && writtenOut(word)) out.push([m.index, m.index + m[0].length]);
  }
  return out;
}

/** Text that ends a sentence: its stop, and any closing quote or bracket after it. */
const ENDS_SENTENCE = /[.!?]["'\u201d\u2019)\]]*\s*$/u;
/** What may open a sentence before its first word: a quote, a bracket, space. */
const OPENING = /[\s"'\u201c\u2018([]+$/u;

/** One word as a sentence starts an ordinary one: a capital, then small letters. */
const SENTENCE_CASE = /^\p{Lu}\p{Ll}+$/u;

interface Pattern {
  read: Read;
  by: "key" | "name";
  /**
   * A name of one word spelled as an ordinary word is at a sentence's start
   * ("Design"), which is never matched as a sentence's first word (rule 2).
   */
  ordinary: boolean;
  re: RegExp;
}

function patternsOf(reads: readonly Read[]): Pattern[] {
  // A NAME TWO READS SHARE NAMES NEITHER (rule 2).
  const sharing = new Map<string, number>();
  for (const read of reads) {
    if (read.name) sharing.set(read.name, (sharing.get(read.name) ?? 0) + 1);
  }
  const re = (text: string) => new RegExp(`${BEFORE}${escape(text)}${AFTER}`, "gu");
  return reads.flatMap((read) => [
    ...(read.key ? [{ read, by: "key" as const, ordinary: false, re: re(read.key) }] : []),
    ...(read.name && sharing.get(read.name) === 1
      ? [{ read, by: "name" as const, ordinary: SENTENCE_CASE.test(read.name), re: re(read.name) }]
      : []),
  ]);
}

/** Where a run sits: whether it opens its block, and the run before it. */
interface RunContext {
  opens: boolean;
  previous: string;
  addresses: ReadonlyArray<readonly [number, number]>;
}

/** Whether the word at `at` in `text` is its sentence's first (rule 2). */
function startsSentence(text: string, at: number, run: RunContext): boolean {
  const before = text.slice(0, at).replace(OPENING, "");
  if (before !== "") return ENDS_SENTENCE.test(before);
  return run.opens || run.previous.trim() === "" || ENDS_SENTENCE.test(run.previous);
}

/** Whether a match of `p` at `at` is a mention by rules 2 and 3. */
function mentions(p: Pattern, text: string, at: number, end: number, run: RunContext): boolean {
  if (run.addresses.some(([from, to]) => at < to && end > from)) return false;
  return !(p.ordinary && startsSentence(text, at, run));
}

/** The first mention in `text` from `from` of a read still waiting for one (rule 4), or null. */
function firstIn(
  text: string,
  from: number,
  patterns: readonly Pattern[],
  named: ReadonlySet<string>,
  run: RunContext,
): { at: number; mention: Mention } | null {
  let best: { at: number; mention: Mention } | null = null;
  for (const p of patterns) {
    if (named.has(p.read.id)) continue;
    p.re.lastIndex = from;
    for (let m = p.re.exec(text); m !== null; m = p.re.exec(text)) {
      if (best !== null && m.index > best.at) break;
      if (!mentions(p, text, m.index, m.index + m[0].length, run)) continue;
      const better =
        best === null ||
        m.index < best.at ||
        (m.index === best.at && m[0].length > best.mention.text.length);
      if (better) best = { at: m.index, mention: { read: p.read, text: m[0], by: p.by } };
      break;
    }
  }
  return best;
}

/**
 * The answer's prose runs, each with the first mention of each read drawn by
 * `chip`, for `RenderOptions.prose`; and which reads were named, filled in as
 * the renderer hands the runs over in reading order. One per render: it
 * remembers what it has named and the run before.
 */
export function mentionsOf(
  reads: readonly Read[],
  chip: (mention: Mention, key: string) => ReactNode,
): {
  prose: (text: string, key: string, opens?: boolean) => ReactNode;
  named: ReadonlySet<string>;
} {
  const named = new Set<string>();
  const patterns = patternsOf(reads);
  let previous = "";
  const prose = (text: string, key: string, opens = false): ReactNode => {
    const run: RunContext = { opens, previous, addresses: addresses(text) };
    previous = text;
    if (patterns.length === 0) return text;
    const out: ReactNode[] = [];
    let from = 0;
    for (let found = firstIn(text, from, patterns, named, run); found !== null;) {
      named.add(found.mention.read.id);
      out.push(text.slice(from, found.at), chip(found.mention, `${key}:${found.mention.read.id}`));
      from = found.at + found.mention.text.length;
      found = firstIn(text, from, patterns, named, run);
    }
    if (out.length === 0) return text;
    out.push(text.slice(from));
    return out;
  };
  return { prose, named };
}
