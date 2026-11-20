/**
 * A task's three histories as one list, cut where it is complete.
 *
 * The failure this guards is silent: three histories paged on their own and
 * merged naively draw a stretch of time holding only the one that read
 * furthest back, and a fortnight of changes with no turns in it reads as a
 * fortnight in which no agent touched the task.
 */

import { expect, test } from "vitest";
import type {
  WorkActivityRecord,
  WorkComment,
  WorkFlowEntry,
  WorkItemTurn,
} from "~/protocol/index.ts";
import { timeline } from "./timeline.ts";

const change = (id: string, at: string, over: Partial<WorkActivityRecord> = {}) =>
  ({
    id,
    at,
    effective_at: at,
    kind: "status",
    log_seq: 1,
    log_stream: "s",
    log_generation: 1,
    subject_kind: "task",
    subject_id: "t-1",
    notified: false,
    ...over,
  }) as WorkActivityRecord;
const comment = (id: string, at: string) =>
  ({ id, task: "t-1", author: "ada", body: id, created_at: at }) as WorkComment;
const turn = (id: string, at: string) =>
  ({
    turn_id: id,
    ordinal: 1,
    seat: "swe",
    segments: 1,
    tokens: 1,
    cache_read: 0,
    rounds: 1,
    wall_ms: 1,
    outcome: "done",
    phases: [],
    at,
  }) as WorkItemTurn;

// THE MERGE IS CUT AT THE NEWEST POINT ANY HISTORY WITH MORE PAGES STOPPED AT.
// The turns stopped at the 10th with more behind them, so the change of the
// 3rd is held back until the turns before the 10th are read — and it is the
// turns, not the changes, whose next page the reader is offered.
test("the merge is cut where every history it shows is complete", () => {
  const got = timeline("all", {
    changes: {
      items: [change("c-12", "2031-04-12T00:00:00Z"), change("c-3", "2031-04-03T00:00:00Z")],
      more: false,
    },
    comments: { items: [comment("m-11", "2031-04-11T00:00:00Z")], more: false },
    turns: { items: [turn("r-10", "2031-04-10T00:00:00Z")], more: true },
  });
  expect(got.entries.map((e) => e.id)).toEqual(["turn:r-10", "comment:m-11", "change:c-12"]);
  expect(got.earlier).toEqual(["turns"]);
});

// ONE HISTORY'S TAB IS NOT CUT BY ANOTHER'S: the Changes tab shows every
// change it holds, whatever the turns have read.
test("a tab of one history is cut only by that history", () => {
  const got = timeline("changes", {
    changes: { items: [change("c-3", "2031-04-03T00:00:00Z")], more: false },
    comments: { items: [], more: true },
    turns: { items: [turn("r-10", "2031-04-10T00:00:00Z")], more: true },
  });
  expect(got.entries.map((e) => e.id)).toEqual(["change:c-3"]);
  expect(got.earlier).toEqual([]);
});

// A COMMENT IS DRAWN ONCE: the change record that announced it is dropped,
// and the comment itself is what is listed.
test("a comment's own change record is not listed beside it", () => {
  const got = timeline("all", {
    changes: {
      items: [change("c-1", "2031-04-03T00:00:00Z", { kind: "comment", comment_id: "m-1" })],
      more: false,
    },
    comments: { items: [comment("m-1", "2031-04-03T00:00:00Z")], more: false },
    turns: { items: [], more: false },
  });
  expect(got.entries.map((e) => e.id)).toEqual(["comment:m-1"]);
});

// A HISTORY THAT HAS MORE AND HAS LOADED NOTHING YET HOLDS EVERYTHING BACK
// rather than letting the others draw a stretch it may belong in.
test("an unread history with more holds the list back", () => {
  const got = timeline("all", {
    changes: { items: [change("c-3", "2031-04-03T00:00:00Z")], more: false },
    comments: { items: [], more: true },
    turns: { items: [], more: false },
  });
  expect(got.entries).toEqual([]);
  expect(got.earlier).toEqual(["comments"]);
});

const flowRecord = (id: string, at: string, log_seq: number, backfilled = false) =>
  ({
    id,
    task: "t-1",
    state: "stuck",
    cause: "",
    since: null,
    until: null,
    seat: "swe",
    notice: "",
    told: "",
    backfilled,
    at,
    log_seq,
  }) as WorkFlowEntry;

// THE RECORDED FLOW IS DRAWN WITH THE CHANGES (ADR-0162), by its instant and
// then its log position: a backfilled record lands in the log after the past
// it records, so its position alone would draw it last.
test("the recorded flow is drawn with the changes, by its instant", () => {
  const loaded = {
    changes: { items: [change("c-5", "2031-04-05T00:00:00Z")], more: false },
    comments: { items: [comment("m-6", "2031-04-06T00:00:00Z")], more: false },
    turns: { items: [], more: false },
    flow: [
      flowRecord("f-new", "2031-04-07T00:00:00Z", 90),
      flowRecord("f-old", "2031-04-04T00:00:00Z", 95, true),
      flowRecord("f-tie", "2031-04-07T00:00:00Z", 12),
    ],
  };
  const ids = (tab: "all" | "changes" | "comments" | "turns") =>
    timeline(tab, loaded).entries.map((e) => e.id.replace(/^flow:\d+:/, "flow:"));
  expect(ids("all")).toEqual([
    "flow:f-old",
    "change:c-5",
    "comment:m-6",
    "flow:f-tie",
    "flow:f-new",
  ]);
  expect(ids("changes")).toEqual(["flow:f-old", "change:c-5", "flow:f-tie", "flow:f-new"]);
  expect(ids("comments")).toEqual(["comment:m-6"]);
  expect(ids("turns")).toEqual([]);
});
