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
 *     digit, underscore or hyphen on either side, so GOP-11 is never found in
 *     GOP-110 or GOP-11-b. A record carries a key where it has one (a task's,
 *     a project's: `AssistantEntityCard.key`).
 *  2. A NAME is matched only for the kinds whose title is a name people
 *     write (an agent, a team, a project, a page, a space, an artifact, a
 *     schedule), exactly, as a whole phrase at the same boundaries, and only
 *     two characters or longer. Never a task's title, which is a sentence
 *     (a task is matched by its key), and never a table's or a record of no
 *     kind: those are listed under the answer.
 *  3. In each run of text, the earliest mention wins; where two start at one
 *     place, the longer. Only a record's FIRST mention, in reading order, is
 *     its chip; the rest stay text.
 *  4. Only the prose is read: the Markdown renderer hands over each run of
 *     the words a reader reads (`RenderOptions.prose`), never a code span,
 *     a code block or a link's label, so a key in `code` is the model
 *     quoting it, not naming it.
 */

import type { ReactNode } from "react";
import type { AssistantCardEvent } from "~/contract/assistant.ts";
import { narrowCard } from "~/protocol/assistantWire.ts";

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
      label: t.title,
      title: t.title,
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

/** A word character on either side ends a match: a letter, a digit, an underscore or a hyphen. */
const BEFORE = "(?<![\\p{L}\\p{N}_-])";
const AFTER = "(?![\\p{L}\\p{N}_-])";

interface Pattern {
  read: Read;
  by: "key" | "name";
  re: RegExp;
}

function patternsOf(reads: readonly Read[]): Pattern[] {
  return reads.flatMap((read) => [
    ...(read.key
      ? [{ read, by: "key" as const, re: new RegExp(`${BEFORE}${escape(read.key)}${AFTER}`, "u") }]
      : []),
    ...(read.name
      ? [
          {
            read,
            by: "name" as const,
            re: new RegExp(`${BEFORE}${escape(read.name)}${AFTER}`, "u"),
          },
        ]
      : []),
  ]);
}

/** The first mention in `text` of a read still waiting for one (rule 3), or null. */
function firstIn(
  text: string,
  patterns: readonly Pattern[],
  named: ReadonlySet<string>,
): { at: number; mention: Mention } | null {
  let best: { at: number; mention: Mention } | null = null;
  for (const p of patterns) {
    if (named.has(p.read.id)) continue;
    const m = p.re.exec(text);
    if (!m) continue;
    const better =
      best === null ||
      m.index < best.at ||
      (m.index === best.at && m[0].length > best.mention.text.length);
    if (better) best = { at: m.index, mention: { read: p.read, text: m[0], by: p.by } };
  }
  return best;
}

/**
 * The answer's prose runs, each with the first mention of each read drawn by
 * `chip`, for `RenderOptions.prose`; and which reads were named, filled in as
 * the renderer hands the runs over in reading order. One per render: it
 * remembers what it has named.
 */
export function mentionsOf(
  reads: readonly Read[],
  chip: (mention: Mention, key: string) => ReactNode,
): { prose: (text: string, key: string) => ReactNode; named: ReadonlySet<string> } {
  const named = new Set<string>();
  const patterns = patternsOf(reads);
  const prose = (text: string, key: string): ReactNode => {
    const found = patterns.length > 0 ? firstIn(text, patterns, named) : null;
    if (found === null) return text;
    named.add(found.mention.read.id);
    const end = found.at + found.mention.text.length;
    return [
      text.slice(0, found.at),
      chip(found.mention, `${key}:${found.mention.read.id}`),
      prose(text.slice(end), `${key}+`),
    ];
  };
  return { prose, named };
}
