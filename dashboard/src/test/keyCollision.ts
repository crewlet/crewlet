/**
 * Two tasks that hold ONE key, in the wire's own shapes. Imported by tests only.
 *
 * A key counter restored beside work minted after it hands out numbers those
 * tasks already hold, so two tasks answer to `ENG-7`. The engine opens the key
 * on the one that claimed it first and flags the other `key_collision`, which
 * is then reached by its id alone. Every surface that opens an item from a row
 * has to open the two as two different tasks — and the cases that hold each
 * surface to it share these, so they agree about which task is which.
 */

import type { WorkRanked, WorkSummary } from "~/protocol/index.ts";

/** The key both tasks carry. */
export const SHARED_KEY = "ENG-7";

/** The task that claimed the key first, which the key opens. */
export const CLAIMANT = "0198f0a0-0000-7000-8000-0000000000c1";

/** The task that holds the key beside it, flagged and reached by its id. */
export const DUPLICATE = "0198f0a0-0000-7000-8000-0000000000d2";

/** What a reader sees each one as — two different titles under one key. */
export const CLAIMANT_TITLE = "The claimant";
export const DUPLICATE_TITLE = "The duplicate";

/** The route each one's page is at: the claimant by its key, the duplicate by its id. */
export const CLAIMANT_HREF = `#/work/${SHARED_KEY}`;
export const DUPLICATE_HREF = `#/work/${DUPLICATE}`;

/** The two as board rows, the claimant first. */
export function collidingRows(over: Partial<WorkSummary> = {}): [WorkSummary, WorkSummary] {
  const base = {
    key: SHARED_KEY,
    project: "ENG",
    type: "task",
    status: "todo",
    status_group: "not_started",
    updated: "2031-04-16T00:00:00Z",
    version: 1,
    ...over,
  };
  return [
    { ...base, id: CLAIMANT, title: CLAIMANT_TITLE },
    { ...base, id: DUPLICATE, title: DUPLICATE_TITLE, key_collision: true },
  ];
}

/** The two as ranked search hits, the claimant first. */
export function collidingHits(): [WorkRanked, WorkRanked] {
  const base = {
    key: SHARED_KEY,
    project: "ENG",
    type: "task",
    status: "todo",
    priority: "normal",
  };
  return [
    { ...base, id: CLAIMANT, title: CLAIMANT_TITLE, rank: 1 },
    { ...base, id: DUPLICATE, title: DUPLICATE_TITLE, rank: 2, key_collision: true },
  ];
}

/** The `peek=` the address bar holds now, or null. */
export function peekNow(): string | null {
  const query = location.hash.split("?")[1] ?? "";
  return new URLSearchParams(query).get("peek");
}

/** The href of the link a title sits in. */
export function linkOf(title: string, root: ParentNode = document): string | null {
  const text = [...root.querySelectorAll("*")].find(
    (el) => el.children.length === 0 && el.textContent === title,
  );
  return text?.closest("a")?.getAttribute("href") ?? null;
}
