/**
 * The Inbox's rules as values: the rows, the chips, the day groups, the read
 * position "Mark all read" sends, the snooze presets and the quiet sentences.
 */

import { describe, expect, test } from "vitest";

import {
  chipCounts,
  dayGroups,
  inboxQuiet,
  inboxRows,
  readThrough,
  rowMatches,
  snoozePresets,
  type InboxRow,
} from "./model.ts";
import type { DecisionSubject } from "~/components/DecisionRow.tsx";
import type { WorkInboxNotice } from "~/protocol/index.ts";

function notice(id: string, extra: Partial<WorkInboxNotice> = {}): WorkInboxNotice {
  return {
    record_id: id,
    log_seq: 1,
    log_stream: "CREWLET_TRACKER_LOG",
    log_generation: 1,
    at: "2026-09-27T10:00:00Z",
    reason: "watcher",
    primary: false,
    addressed: false,
    kind: "comment_added",
    subject_id: "t",
    read: false,
    ...extra,
  };
}

const ASK: DecisionSubject = {
  kind: "ask",
  at: "2026-09-27T09:00:00Z",
  ask: {
    id: "t",
    key: "LEAD-12",
    project: "LEAD",
    title: "t",
    type: "task",
    status: "todo",
    comment: "c-12",
    asked_by: "cto",
    asked_at: "2026-09-27T09:00:00Z",
    body: "?",
    answer_with: "",
    decision: {
      question: "?",
      options: [
        { id: "a", label: "A" },
        { id: "b", label: "B" },
      ],
      role: "approver",
    },
  } as never,
};

describe("the rows", () => {
  test("an ask's notices ride on its decision and leave the notice rows", () => {
    const rows = inboxRows({
      scope: "unread",
      subjects: [ASK],
      conditions: [],
      notices: [
        notice("r-1", { ask: { comment: "c-12", asked_of: "jane", open: true } }),
        notice("r-2"),
      ],
    });
    expect(rows.decisions).toHaveLength(1);
    expect(
      rows.decisions[0]!.kind === "decision" && rows.decisions[0]!.notices.map((n) => n.record_id),
    ).toEqual(["r-1"]);
    expect(rows.notices.map((r) => r.key)).toEqual(["r-2"]);
  });

  test("the Snoozed scope holds no decisions", () => {
    const rows = inboxRows({
      scope: "snoozed",
      subjects: [ASK],
      conditions: [],
      notices: [notice("r-1")],
    });
    expect(rows.decisions).toEqual([]);
    expect(rows.notices).toHaveLength(1);
  });

  test("Reviews are the asks put to the reader as approver, and Decisions every non-notice", () => {
    const rows = inboxRows({
      scope: "all",
      subjects: [
        ASK,
        {
          kind: "ask",
          at: ASK.at!,
          ask: {
            ...(ASK.kind === "ask" ? ASK.ask : ({} as never)),
            comment: "c-2",
            decision: { question: "?", options: [], role: "contributor" },
          },
        },
      ],
      conditions: [],
      notices: [notice("r-1", { reason: "mention" }), notice("r-2", { reason: "assignee" })],
    });
    const counts = chipCounts(rows);
    expect(counts).toEqual({ "": 4, decisions: 2, reviews: 1, mentions: 1, assigned: 1 });
    const all: InboxRow[] = [...rows.decisions, ...rows.notices];
    expect(all.filter((r) => rowMatches(r, "assigned")).map((r) => r.key)).toEqual(["r-2"]);
  });
});

describe("the day groups", () => {
  // THE COMPANY'S MIDNIGHT: at 03:00 UTC on the 27th it is still 23:00 on the
  // 26th in New York, so a notice from 05:00 UTC on the 26th — 01:00 there —
  // is yesterday for a company on UTC and today for one on New York time.
  test("are cut at the company's midnight, not the reader's", () => {
    const now = Date.parse("2026-09-27T03:00:00Z");
    const rows: InboxRow[] = [
      { kind: "notice", key: "a", at: "2026-09-27T02:00:00Z", notice: notice("a") },
      { kind: "notice", key: "b", at: "2026-09-26T05:00:00Z", notice: notice("b") },
      { kind: "notice", key: "c", at: "2026-09-24T12:00:00Z", notice: notice("c") },
    ];
    const utc = dayGroups(rows, now, "UTC").map((g) => [g.label, g.rows.map((r) => r.key)]);
    expect(utc).toEqual([
      ["Today", ["a"]],
      ["Yesterday", ["b"]],
      ["Earlier", ["c"]],
    ]);
    const ny = dayGroups(rows, now, "America/New_York").map((g) => [
      g.label,
      g.rows.map((r) => r.key),
    ]);
    expect(ny).toEqual([
      ["Today", ["a", "b"]],
      ["Earlier", ["c"]],
    ]);
  });
});

describe("mark all read", () => {
  test("reads through the newest unread notice loaded, by generation then sequence", () => {
    expect(
      readThrough([
        notice("a", { log_generation: 1, log_seq: 90 }),
        notice("b", { log_generation: 2, log_seq: 3 }),
        notice("c", { log_generation: 2, log_seq: 40, read: true }),
      ]),
    ).toBe("CREWLET_TRACKER_LOG@2:3");
    expect(readThrough([notice("a", { read: true })])).toBeNull();
  });
});

describe("the snooze presets", () => {
  const now = Date.parse("2026-09-23T15:00:00Z"); // a Wednesday

  test("are an hour, tomorrow at nine and next Monday at nine on the company's clock", () => {
    const presets = snoozePresets(now, "Europe/Lisbon", undefined);
    expect(presets.map((p) => [p.key, p.until])).toEqual([
      ["hour", "2026-09-23T16:00:00.000Z"],
      // 09:00 in Lisbon is 08:00 UTC in September (WEST).
      ["tomorrow", "2026-09-24T08:00:00.000Z"],
      ["monday", "2026-09-28T08:00:00.000Z"],
    ]);
  });

  test("say the day a company-clock preset lands on", () => {
    const presets = snoozePresets(now, "Europe/Lisbon", undefined);
    expect(presets.map((p) => p.day)).toEqual([undefined, "Thu, Sep 24", "Mon, Sep 28"]);
  });

  test("on a Monday, next Monday is a week away", () => {
    const monday = Date.parse("2026-09-28T07:00:00Z");
    expect(snoozePresets(monday, "UTC", undefined).find((p) => p.key === "monday")?.until).toBe(
      "2026-10-05T09:00:00.000Z",
    );
  });

  // THE ENGINE'S BOUND: a preset it refuses is not offered.
  test("offers nothing past the engine's max_snooze_ahead", () => {
    const keys = (ahead: number) => snoozePresets(now, "UTC", ahead).map((p) => p.key);
    expect(keys(2 * 86_400)).toEqual(["hour", "tomorrow"]);
    expect(keys(3_600)).toEqual(["hour"]);
    expect(keys(30 * 86_400)).toEqual(["hour", "tomorrow", "monday"]);
  });
});

describe("the quiet sentences", () => {
  const base = { bound: true, error: false, shown: 0, watched: "every seat's own state" };
  const answer = { handle: "jane", notices: [], primary_reasons: [], unread: 0, primary: 0 };

  test("say nothing before an answer or beside a refusal", () => {
    expect(inboxQuiet({ ...base, answer: null, scope: "unread", chip: "" })).toBeNull();
    expect(inboxQuiet({ ...base, answer, error: true, scope: "unread", chip: "" })).toBeNull();
  });

  test("claim no read history for a person who never marked one", () => {
    expect(inboxQuiet({ ...base, answer, scope: "unread", chip: "" })?.title).toBe(
      "Nothing has reached you yet",
    );
    expect(
      inboxQuiet({
        ...base,
        answer: { ...answer, seen_through: { seq: 4 } },
        scope: "unread",
        chip: "",
      })?.title,
    ).toBe("You are caught up");
  });

  test("name what the conditions were measured over for a reader who is nobody", () => {
    expect(
      inboxQuiet({ ...base, bound: false, answer: null, scope: "unread", chip: "" })?.hint,
    ).toContain("every seat's own state");
  });
});
