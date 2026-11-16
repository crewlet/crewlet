/**
 * The Doclet landing's arithmetic: how the company's changes fold into
 * rows, which one the landing opens on, which revisions its diff is read
 * between, and the day each row is drawn under.
 *
 * PURE, beside the screen that draws it, for the reason `lib/` gives: a rule
 * that can only be exercised by rendering a screen is a rule nobody re-reads.
 */

import { fmtDateCompact, tsKey } from "~/lib/format.ts";
import { companyMidnight } from "~/lib/range.ts";
import { dayLabelIn } from "~/lib/companyDay.ts";
import type { PageChange, PageRevision } from "~/protocol/index.ts";
import { changeWords, changedBy } from "./parts.tsx";
import { assisted } from "~/lib/attribution.ts";

/**
 * One row of the landing: one change, or BACK-TO-BACK SAVES of one page by one
 * writer on one company day, newest first. A seat saves the page it is working
 * on several times in a row, and a feed listing each save gave one page a
 * morning's rows and everything else written that morning none.
 */
export type Run = readonly [PageChange, ...PageChange[]];

/** The feed, newest first, folded into rows. */
export function foldRuns(changes: readonly PageChange[], zone: string | undefined): Run[] {
  const out: [PageChange, ...PageChange[]][] = [];
  for (const change of changes) {
    const run = out[out.length - 1];
    if (run && continues(run[0], change, zone)) run.push(change);
    else out.push([change]);
  }
  return out;
}

function continues(head: PageChange, next: PageChange, zone: string | undefined): boolean {
  return (
    head.kind === "saved" &&
    next.kind === "saved" &&
    head.page_id === next.page_id &&
    changedBy(head) === changedBy(next) &&
    // A save through the assistant and the person's own save are two lines:
    // folded together, one would be named for the other.
    assisted(head.operator_id) === assisted(next.operator_id) &&
    dayLabelIn(tsKey(head.at), zone) === dayLabelIn(tsKey(next.at), zone)
  );
}

/** What a row's writer did, where the page goes without saying: "saved it twice". */
export function runPhrase(run: Run): string {
  if (run.length === 1) return changeWords(run[0].kind).phrase;
  return `saved it ${run.length === 2 ? "twice" : `${run.length} times`}`;
}

/**
 * The row the landing opens on: the newest whose page is still there. A
 * purged page's changes keep no title and lead nowhere, so they stay in the
 * feed as the record that the page existed and never take the big tile. A
 * page in the trash keeps its title, so its status says it is not there.
 * -1 when no row has a page to open.
 */
export function leadOf(runs: readonly Run[]): number {
  return runs.findIndex((run) => Boolean(run[0].title) && run[0].status !== "trashed");
}

/**
 * Recent activity's rows: ONE PER PAGE, the newest row of each, and never the
 * page the big tile leads with (`lead`, an index into `runs`, or -1), at most
 * `max` of them, newest first.
 *
 * BY PAGE, NOT BY CHANGE. Three people answering one comment were three rows
 * of the same page ("Meeting notes 8" three times running), and every comment
 * on the page the big tile already opens was listed again beside it, so a
 * morning's discussion of one page pushed every other page written that
 * morning off the column. A page's newer changes say what happened to it
 * lately; the rest are its History and the audit log's.
 *
 * A PAGE THAT IS GONE keeps its id on every change, so its changes are one row
 * as well: the record that it existed, said once.
 */
export function pageRows(runs: readonly Run[], lead: number, max: number): Run[] {
  const seen = new Set<string>();
  const leadPage = lead >= 0 ? runs[lead]?.[0].page_id : undefined;
  if (leadPage !== undefined) seen.add(leadPage);
  const out: Run[] = [];
  for (const run of runs) {
    if (out.length >= max) break;
    const page = run[0].page_id;
    if (seen.has(page)) continue;
    seen.add(page);
    out.push(run);
  }
  return out;
}

/**
 * The revisions a save's line diff is read between: the one the save made and
 * the one before it, and whether the first is the page's current body.
 *
 * MATCHED BY INSTANT, because the history row a save wrote and the revision it
 * wrote carry the same one (the record's), and the activity row carries no
 * version. Null for anything but a save, and where this node no longer holds
 * either revision: a page keeps a bounded number, so that is an ordinary
 * absence rather than a failure.
 */
export function saveRevisions(
  change: PageChange,
  history: readonly PageRevision[],
): { made: PageRevision; before: PageRevision; current: boolean } | null {
  if (change.kind !== "saved") return null;
  const at = tsKey(change.at);
  const i = history.findIndex((rev) => tsKey(rev.created_at) === at);
  const made = history[i];
  const before = history[i + 1];
  if (i < 0 || !made || !before) return null;
  return { made, before, current: i === 0 };
}

/**
 * The COMPANY's day a change fell on, as a heading over its rows: the day the
 * engine counts a day in, so a change at 23:00 company time is under the same
 * day on this screen as on every other.
 */
export function dayOf(at: string, now: number, zone: string | undefined): string {
  const when = tsKey(at);
  const today = companyMidnight(now, zone);
  if (when >= today) return "Today";
  if (when >= companyMidnight(today - 1, zone)) return "Yesterday";
  // NOON OF THE COMPANY'S DAY, drawn as a date: the day's own midnight can
  // read as the day before in the reader's zone.
  const noon = companyMidnight(when, zone) + 12 * 3_600_000;
  return fmtDateCompact(new Date(noon).toISOString(), now);
}

/** Rows grouped under the day each one's newest change fell on, in order. */
export function byDay(
  runs: readonly Run[],
  now: number,
  zone: string | undefined,
): { day: string; runs: Run[] }[] {
  const out: { day: string; runs: Run[] }[] = [];
  for (const run of runs) {
    const day = dayOf(run[0].at, now, zone);
    const last = out[out.length - 1];
    if (last && last.day === day) last.runs.push(run);
    else out.push({ day, runs: [run] });
  }
  return out;
}

/**
 * How much a save changed, in words a reader weighs at a glance: "3 words
 * added, 1 removed, in 2 places". Counted in words, not markdown lines.
 */
export function changeSize(added: number, removed: number, places: number): string {
  const words = (n: number) => `${n.toLocaleString()} ${n === 1 ? "word" : "words"}`;
  const size =
    added && removed
      ? `${words(added)} added, ${removed.toLocaleString()} removed`
      : added
        ? `${words(added)} added`
        : removed
          ? `${words(removed)} removed`
          : "No words changed";
  return places > 1 ? `${size}, in ${places} places` : size;
}
