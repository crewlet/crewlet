/**
 * A platform notice a reader has accepted for now: put off on this browser for
 * a stated time, and back the moment its condition worsens.
 *
 * # Why the browser, and why it says so
 *
 * A platform notice is read off its condition and never written (ADR-0116):
 * the engine keeps no mark over it, so it is open for exactly as long as the
 * condition holds, for every administrator. A person who has read "the objects
 * bucket is near its ceiling" and decided to live with it for a week still
 * needs their Inbox and badge to stop counting it, or the count stops meaning
 * anything. So the mark is theirs, kept per reader on this browser
 * (`lib/readerStore.ts`), the way their Home's board is, and every control that
 * sets it says "on this browser": it hides nothing from anybody else, and
 * nothing on another desk.
 *
 * # Never for good
 *
 * A mark lasts at most [PUT_OFF_CHOICES]' longest choice, a week, and it ends
 * early, by itself, when the condition WORSENS: when it cleared and came back
 * (a new `since`, which is a new condition), or when it stands on more nodes
 * than when it was put off. A put-off notice stays listed in the Inbox, under
 * "Put off", with the way to bring it back now.
 */

import { useCallback } from "react";
import { readerStore } from "./readerStore.ts";
import { STORAGE_KEYS } from "./storage.ts";

/** One notice put off: until when, and how it stood when it was. */
export interface PutOffMark {
  until: string;
  /** When the condition began, as the notice said; "" where it said nothing. */
  since: string;
  /** How many nodes it stood on. */
  spread: number;
}

/** What a notice says of itself, for a mark to be held against. */
export interface PutOffable {
  id: string;
  /** When the condition began. */
  at?: string;
  /** How many nodes it stands on: present exactly on a platform notice. */
  spread?: number;
}

/** How long a notice may be put off, each as its control says it. */
export const PUT_OFF_CHOICES: readonly { label: string; hours: number }[] = [
  { label: "For a day", hours: 24 },
  { label: "For 3 days", hours: 72 },
  { label: "For a week", hours: 168 },
];

const LONGEST_MS = Math.max(...PUT_OFF_CHOICES.map((c) => c.hours)) * 3_600_000;

/** Storage is untrusted: keep a mark only where every field is what it says. */
function valid(raw: unknown): Record<string, PutOffMark> {
  const out: Record<string, PutOffMark> = {};
  if (typeof raw !== "object" || raw === null) return out;
  for (const [id, m] of Object.entries(raw as Record<string, unknown>)) {
    const mark = m as Partial<PutOffMark> | null;
    if (
      mark &&
      typeof mark.until === "string" &&
      Number.isFinite(Date.parse(mark.until)) &&
      typeof mark.since === "string" &&
      typeof mark.spread === "number"
    ) {
      out[id] = { until: mark.until, since: mark.since, spread: mark.spread };
    }
  }
  return out;
}

const store = readerStore<Record<string, PutOffMark>>(STORAGE_KEYS.putOff, {}, valid);

/**
 * Whether mark still holds for item at now: before its time, past no longer
 * than the longest choice from now (a value written by hand is held to it),
 * the same condition (the same `since`) and on no more nodes than it was.
 */
export function putOffHolds(mark: PutOffMark | undefined, item: PutOffable, now: number): boolean {
  if (!mark || item.spread === undefined) return false;
  const until = Date.parse(mark.until);
  return (
    until > now &&
    until - now <= LONGEST_MS &&
    mark.since === (item.at ?? "") &&
    item.spread <= mark.spread
  );
}

/** The queue split into what is shown and what this reader put off, held at now. */
export function splitPutOff<T extends PutOffable>(
  queue: readonly T[],
  marks: Readonly<Record<string, PutOffMark>>,
  now: number,
): { shown: T[]; putOff: T[] } {
  const shown: T[] = [];
  const putOff: T[] = [];
  for (const item of queue) (putOffHolds(marks[item.id], item, now) ? putOff : shown).push(item);
  return { shown, putOff };
}

/** This reader's marks, and the two gestures that change them. */
export function usePutOff(): {
  marks: Readonly<Record<string, PutOffMark>>;
  putOff: (item: PutOffable, hours: number, now: number) => void;
  bringBack: (id: string) => void;
} {
  const marks = store.use();
  const putOff = useCallback((item: PutOffable, hours: number, now: number) => {
    const next: Record<string, PutOffMark> = { ...store.read() };
    next[item.id] = {
      until: new Date(now + Math.min(hours * 3_600_000, LONGEST_MS)).toISOString(),
      since: item.at ?? "",
      spread: item.spread ?? 0,
    };
    // A MARK THAT NO LONGER HOLDS IS DROPPED as another is written, so the
    // list does not grow with every notice ever put off.
    for (const [id, m] of Object.entries(next)) if (Date.parse(m.until) <= now) delete next[id];
    store.write(next);
  }, []);
  const bringBack = useCallback((id: string) => {
    store.update((current) => {
      const next = { ...current };
      delete next[id];
      return next;
    });
  }, []);
  return { marks, putOff, bringBack };
}
