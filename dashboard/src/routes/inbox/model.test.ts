/**
 * The Inbox's rules as values: the rows, where the selection goes once an
 * item is settled, what a notice is called, the read position "Mark all
 * read" sends, the snooze presets and the quiet sentences.
 */

import { describe, expect, test } from "vitest";

import {
  answerWords,
  answeredQuestion,
  answeredRows,
  askContext,
  askQuestion,
  earlierOf,
  followThrough,
  inboxQuiet,
  inboxResources,
  inboxSelection,
  inboxRows,
  nextAfter,
  noticeNote,
  noticeTitle,
  pairAnswered,
  readThrough,
  rowItem,
  rowKey,
  rowName,
  rowWho,
  snoozePresets,
  type InboxRow,
} from "./model.ts";
import type { DecisionSubject } from "~/lib/decisions.ts";
import type {
  AgentRow,
  AnsweredRun,
  AnsweredWorkAsk,
  SandboxRun,
  WorkAskRow,
  WorkInboxNotice,
} from "~/protocol/index.ts";
import { itemAddress } from "~/lib/work.ts";
import { indexOrg } from "~/lib/seats.ts";
import { withDerived } from "~/test/org.ts";

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
    kind: "comment",
    subject_id: "t",
    read: false,
    ...extra,
  };
}

const ASK_ROW = {
  id: "t",
  key: "LEAD-12",
  project: "LEAD",
  title: "Q4 release plan",
  type: "task",
  status: "todo",
  comment: "c-12",
  asked_by: "cto",
  asked_at: "2026-09-27T09:00:00Z",
  body: "2.4 is scheduled for Thursday.",
  answer_with: "",
  decision: {
    question: "Hold the release?",
    options: [
      { id: "a", label: "A" },
      { id: "b", label: "B" },
    ],
    role: "approver",
  },
} as unknown as WorkAskRow;

const ASK: DecisionSubject = { kind: "ask", at: "2026-09-27T09:00:00Z", ask: ASK_ROW };

describe("the rows", () => {
  test("an ask's notices ride on its row in Needs you and leave Updates", () => {
    const rows = inboxRows({
      subjects: [ASK],
      conditions: [],
      notices: [
        notice("r-1", { ask: { comment: "c-12", asked_of: "jane", open: true } }),
        notice("r-2"),
      ],
    });
    expect(rows.needs).toHaveLength(1);
    expect(
      rows.needs[0]!.kind === "decision" && rows.needs[0]!.notices.map((n) => n.record_id),
    ).toEqual(["r-1"]);
    expect(rows.updates.map((r) => r.key)).toEqual(["r-2"]);
  });

  test("Earlier lists only what is read, since the engine answers read and unread alike", () => {
    expect(
      earlierOf([notice("a"), notice("b", { read: true }), notice("c", { read: true })]).map(
        (n) => n.record_id,
      ),
    ).toEqual(["b", "c"]);
  });
});

// THE NEXT ITEM OPENS once the open one is settled, and it is the one AFTER it
// in the list as it stood, not the first of the list as it now is.
describe("where the selection goes next", () => {
  test("to the row after it, in the order it had", () => {
    expect(nextAfter("b", ["a", "b", "c", "d"], ["a", "b", "c", "d"], new Set(["b"]))).toBe("c");
  });

  test("past a row that has left the list since", () => {
    // `b` was read and left Updates; `c` was read elsewhere meanwhile.
    expect(nextAfter("b", ["a", "b", "c", "d"], ["a", "d"], new Set())).toBe("d");
  });

  test("past a row already settled here", () => {
    expect(nextAfter("a", ["a", "b", "c"], ["a", "b", "c"], new Set(["a", "b"]))).toBe("c");
  });

  test("back to the first unsettled row when nothing follows it", () => {
    expect(nextAfter("c", ["a", "b", "c"], ["a", "b", "c"], new Set(["a", "c"]))).toBe("b");
  });

  test("nowhere when nothing is left to open", () => {
    expect(nextAfter("a", ["a"], [], new Set(["a"]))).toBeNull();
    expect(nextAfter("a", ["a", "b"], ["a", "b"], new Set(["a", "b"]))).toBeNull();
  });
});

describe("what a notice is called", () => {
  test("a notice the reader is on the hook for is named by its reason", () => {
    expect(noticeTitle(notice("a", { reason: "mention", primary: true }))).toBe("Mentioned you");
    expect(noticeTitle(notice("a", { reason: "assignee", primary: true, kind: "assignee" }))).toBe(
      "Assigned to you",
    );
  });

  test("any other notice is named by what the change did", () => {
    expect(noticeTitle(notice("a", { kind: "comment" }))).toBe("Commented");
    expect(noticeTitle(notice("a", { kind: "status" }))).toBe("Changed its status");
    // A KIND THIS BUILD HAS NO WORDS FOR is still said, never dropped.
    expect(noticeTitle(notice("a", { kind: "brand_new_kind" }))).toBe("Brand new kind");
  });

  test("says why it reached the reader only where its title does not", () => {
    const until = (at: string) => `at ${at}`;
    expect(noticeNote(notice("a", { reason: "watcher" }), until)).toBe("you follow this task");
    expect(noticeNote(notice("a", { reason: "mention", primary: true }), until)).toBe("");
    expect(noticeNote(notice("a", { primary: true, fallback: true }), until)).toBe(
      "nobody better was found for it",
    );
    expect(
      noticeNote(notice("a", { snoozed: true, snoozed_until: "2030-01-01T09:00:00Z" }), until),
    ).toBe("snoozed until at 2030-01-01T09:00:00Z");
  });
});

describe("what an ask says", () => {
  test("a structured ask states its question apart from its body", () => {
    expect(askQuestion(ASK_ROW)).toBe("Hold the release?");
    expect(askContext(ASK_ROW)).toBe("2.4 is scheduled for Thursday.");
  });

  test("a free-text ask's question is its first line, and the rest its context", () => {
    const words = {
      ...ASK_ROW,
      decision: undefined,
      body: "\nShould we ship Friday?\nThe webinar moved.\n",
    } as unknown as WorkAskRow;
    expect(askQuestion(words)).toBe("Should we ship Friday?");
    expect(askContext(words)).toBe("The webinar moved.");
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

  // ON A SUNDAY NEXT MONDAY IS TOMORROW: the menu offered one moment twice.
  test("offer each moment once, so a Sunday has no second Monday", () => {
    const sunday = Date.parse("2026-10-11T17:40:00Z");
    const presets = snoozePresets(sunday, "America/Vancouver", undefined);
    expect(presets.map((p) => p.key)).toEqual(["hour", "tomorrow"]);
    expect(presets[1]!.day).toBe("Mon, Oct 12");
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

  test("say nothing before an answer, beside a refusal or over rows", () => {
    expect(inboxQuiet({ ...base, answer: null })).toBeNull();
    expect(inboxQuiet({ ...base, answer, error: true })).toBeNull();
    expect(inboxQuiet({ ...base, answer, shown: 1 })).toBeNull();
  });

  test("claim no read history for a person who never marked one", () => {
    expect(inboxQuiet({ ...base, answer })?.title).toBe("Nothing has reached you yet");
    expect(inboxQuiet({ ...base, answer: { ...answer, seen_through: { seq: 4 } } })?.title).toBe(
      "You are caught up",
    );
  });

  test("name what the conditions were measured over for a reader who is nobody", () => {
    expect(inboxQuiet({ ...base, bound: false, answer: null })?.hint).toContain(
      "every seat's own state",
    );
  });
});

// A PARKED RUN'S TASK is a turn's: its work item carries no collision flag, so
// the pane opens, threads and replies by the Worklet task's id, since a key another
// task claimed first would read and write the claimant's conversation.
test("a parked run's task is addressed by its id", () => {
  const run = {
    work_item: { backend: "native", id: "t-dup", key: "ENG-7", project: "ENG" },
  } as unknown as SandboxRun;
  const row: InboxRow = {
    kind: "decision",
    key: "run:1",
    subject: { kind: "run", at: "2026-09-27T10:00:00Z", run },
    notices: [],
  };
  const item = rowItem(row);
  expect(item && itemAddress(item)).toBe("t-dup");
});

// WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person writes AS their seat, so
// the row is the seat and draws its name; a credential bound to no seat writes
// under its own login, which is a name rather than a secret; and only a change
// the engine made itself names nobody.
describe("who a notice is from", () => {
  const index = indexOrg(
    withDerived({
      name: "Acme",
      roles: [{ handle: "maya", name: "Maya Ops", kind: "human" }],
    }) as never,
  );
  const row = (extra: Partial<WorkInboxNotice>): InboxRow => ({
    kind: "notice",
    key: "r-1",
    at: "2026-09-27T10:00:00Z",
    notice: notice("r-1", extra),
  });

  test("a person is drawn as the seat they write as", () => {
    const who = rowWho(row({ actor: "maya", actor_kind: "human" }), index);
    expect(who?.name).toBe("Maya Ops");
    expect(who?.kind).toBe("human");
  });

  test("a credential bound to no seat is drawn as its login, as a person", () => {
    const who = rowWho(row({ actor: "ci.bot", actor_kind: "operator" }), index);
    expect(who?.name).toBe("ci.bot");
    expect(who?.kind).toBe("human");
  });

  test("the engine's own change names nobody, and the row says so", () => {
    const r = row({ actor: "crewlet", actor_kind: "system" });
    expect(rowWho(r, index)).toBeNull();
    expect(rowName(r, null)).toBe("The engine");
  });
});

// A NOTICE ON A DUPLICATE'S KEY is reached by its task's id: the key opens the
// task that claimed it first.
test("a notice whose key another task claimed is addressed by its task's id", () => {
  const r: InboxRow = {
    kind: "notice",
    key: "r-1",
    at: "2026-09-27T10:00:00Z",
    notice: notice("r-1", { task: "t-dup", subject_key: "ENG-7", subject_key_collision: true }),
  };
  const item = rowItem(r);
  expect(item && itemAddress(item)).toBe("t-dup");
});

describe("what Needs you names for the assistant", () => {
  test("each question's task, each waiting run and each stopped seat, once each", () => {
    const ask = { kind: "ask", at: "2026-10-26T08:00:00Z", ask: { id: "t1", key: "FAIR-10" } };
    const collided = {
      kind: "ask",
      at: "2026-10-26T08:00:00Z",
      ask: { id: "t9", key: "FAIR-1", key_collision: true },
    };
    const run = { kind: "run", at: "2026-10-26T08:00:00Z", run: { turn_id: "turn-1" } };
    const budget = { kind: "seat", seat: { row: { handle: "pm" }, window: undefined } };
    const needs = [
      { kind: "decision", key: "a", subject: ask, notices: [] },
      { kind: "decision", key: "b", subject: collided, notices: [] },
      { kind: "decision", key: "c", subject: run, notices: [] },
      { kind: "decision", key: "d", subject: budget, notices: [] },
      { kind: "condition", key: "e", item: { id: "error-x", subject: "seat", who: "qa" } },
      { kind: "condition", key: "f", item: { id: "stopped-y", subject: "seat", who: "qa" } },
      { kind: "condition", key: "g", item: { id: "sandbox-turn-2", subject: "run" } },
      { kind: "condition", key: "h", item: { id: "posture-draining", subject: "engine" } },
    ] as unknown as InboxRow[];
    expect(inboxResources(needs)).toEqual([
      { kind: "task", id: "FAIR-10" },
      { kind: "task", id: "t9" },
      { kind: "run", id: "turn-1" },
      { kind: "seat", id: "pm" },
      { kind: "seat", id: "qa" },
      { kind: "run", id: "turn-2" },
    ]);
  });
});

describe("the row the reader opened, for the assistant", () => {
  test("an ask or a parked run is a decision, an update its task, a stopped seat an agent", () => {
    const ask = { kind: "ask", ask: { id: "t1", key: "FAIR-10" } };
    const collided = { kind: "ask", ask: { id: "t9", key: "FAIR-1", key_collision: true } };
    const run = { kind: "run", run: { turn_id: "turn-1" } };
    const budget = { kind: "seat", seat: { row: { handle: "pm" } } };
    const rows = [
      { kind: "decision", key: "a", subject: ask, notices: [] },
      { kind: "decision", key: "b", subject: collided, notices: [] },
      { kind: "decision", key: "c", subject: run, notices: [] },
      { kind: "decision", key: "d", subject: budget, notices: [] },
      { kind: "condition", key: "e", item: { id: "stopped-y", subject: "seat", who: "qa" } },
      { kind: "condition", key: "f", item: { id: "posture-draining", subject: "engine" } },
      {
        kind: "notice",
        key: "g",
        at: "2026-10-26T08:00:00Z",
        notice: notice("g", { task: "t-3", subject_key: "FAIR-3" }),
      },
      {
        kind: "answered",
        key: "h",
        at: "2026-10-26T08:00:00Z",
        answered: { ask: { id: "t-4", key: "FAIR-4" } },
      },
    ] as unknown as InboxRow[];
    expect(rows.map(inboxSelection)).toEqual([
      { kind: "task", id: "FAIR-10", noun: "decision" },
      { kind: "task", id: "t9", noun: "decision" },
      { kind: "run", id: "turn-1", noun: "decision" },
      { kind: "seat", id: "pm", noun: "agent" },
      { kind: "seat", id: "qa", noun: "agent" },
      null,
      { kind: "task", id: "FAIR-3", noun: "update" },
      { kind: "task", id: "FAIR-4", noun: "decision" },
    ]);
  });

  test("no row opened is nothing selected: the Inbox is what a brief is of", () => {
    expect(inboxSelection(null)).toBeNull();
  });
});

describe("what was answered", () => {
  const ask = {
    id: "t-20",
    key: "LEAD-20",
    project: "LEAD",
    title: "Q4 release plan",
    type: "task",
    status: "todo",
    comment: "c-20",
    asked_by: "cto",
    asked_at: "2026-09-30T10:00:00Z",
    body: "",
    decision: {
      question: "Hold?",
      options: [
        { id: "hold", label: "Hold release" },
        { id: "ship", label: "Ship anyway" },
      ],
      recommended: "hold",
      role: "approver" as const,
    },
    open: false,
  };
  const answered = (extra: Partial<AnsweredWorkAsk> = {}): AnsweredWorkAsk => ({
    at: "2026-09-30T11:00:00Z",
    ask: ask as AnsweredWorkAsk["ask"],
    answer: { by: "jane", at: "2026-09-30T11:00:00Z", choice: "hold", body: "Hold release" },
    ...extra,
  });
  const index = indexOrg(
    withDerived({
      name: "Nimbus",
      roles: [
        { name: "Jane Founder", handle: "jane", kind: "human" },
        { name: "Maya Ops", handle: "maya", kind: "human" },
        { name: "CTO", handle: "cto", kind: "agent" },
      ],
      units: [],
    }) as never,
  );

  test("an answer says who gave it: you, or the colleague who answered for you", () => {
    expect(answerWords(answered(), "jane", index)).toEqual({
      who: "You",
      verb: "chose",
      said: "Hold release",
      note: "",
      kind: "choice",
      recommended: true,
    });
    const maya = answered({
      answer: { by: "maya", by_kind: "human", at: "2026-09-30T11:00:00Z", body: "Not yet." },
    });
    expect(answerWords(maya, "jane", index)).toMatchObject({
      who: "Maya Ops",
      verb: "replied",
      said: "Not yet.",
      kind: "reply",
    });
    const resolved = answered({
      answer: { by: "cto", at: "2026-09-30T11:00:00Z", body: "", resolved: true },
    });
    expect(answerWords(resolved, "jane", index).kind).toBe("resolved");
  });

  // A CHOICE WITH WORDS OF ITS OWN KEEPS THEM: the record's body is the
  // person's words where they wrote any beside the option, and the option's
  // label only where they did not. Read as the label alone, the words were
  // dropped from the row and the pane.
  //
  // Mutation: drop `note`, or fill it with the label, and this fails.
  test("a choice keeps the words written beside it", () => {
    const withWords = answered({
      answer: {
        by: "jane",
        at: "2026-09-30T11:00:00Z",
        choice: "hold",
        body: "Tell support before Thursday.",
      },
    });
    expect(answerWords(withWords, "jane", index)).toMatchObject({
      verb: "chose",
      said: "Hold release",
      note: "Tell support before Thursday.",
      kind: "choice",
    });
  });

  test("how far the asker has got: acted from the tracker, the rest from its seat now", () => {
    const seat = (extra: Record<string, unknown>) =>
      ({ handle: "cto", activity: "idle", ...extra }) as unknown as AgentRow;
    expect(
      followThrough(
        answered({ followed_up: { at: "2026-09-30T11:30:00Z", kind: "status" } }),
        seat({ paused: { at: "x" } }),
      ),
    ).toEqual({ state: "acted", at: "2026-09-30T11:30:00Z" });
    expect(
      followThrough(
        answered(),
        seat({
          activity: "working",
          turn: { work_item: { id: "t-20", key: "LEAD-20" }, started_at: "2026-09-30T11:05:00Z" },
        }),
      ),
    ).toEqual({ state: "acting", at: "2026-09-30T11:05:00Z" });
    // ON ANOTHER TASK IS NOT ON THIS ONE.
    expect(
      followThrough(
        answered(),
        seat({ activity: "working", turn: { work_item: { id: "t-9", key: "LEAD-9" } } }),
      ),
    ).toEqual({ state: "waiting" });
    expect(followThrough(answered(), seat({ paused: { at: "x" } }))).toEqual({
      state: "held",
      reason: "paused",
    });
    expect(followThrough(answered(), seat({ activity: "stopped" }))).toEqual({
      state: "held",
      reason: "stopped",
    });
    // A PERSON WHO ASKED has no seat state to read.
    expect(followThrough(answered(), undefined)).toBeNull();
  });

  // A CODING RUN'S QUESTION, ANSWERED, IS ONE OF THE SAME ROWS: who asked is
  // the run's seat, its task the one the run was on, and the answer reads as
  // an ask's does. Mutation: read `ask` off a run's item, and every case
  // here throws.
  const ranAsk = (extra: Partial<AnsweredRun["run"]> = {}): AnsweredRun => ({
    at: "2026-09-30T11:00:00Z",
    run: {
      turn_id: "turn-7",
      launch_id: "q1",
      agent_handle: "cto",
      question: "Which branch should the fix target?",
      question_context: "The release branch is frozen.",
      decision: {
        question: "Which branch should the fix target?",
        options: [
          { id: "main", label: "Target main" },
          { id: "release", label: "Target the release branch" },
        ],
        recommended: "main",
        role: "approver",
      },
      asked_at: "2026-09-30T10:55:00Z",
      work_item: { backend: "native", id: "t-12", key: "LEAD-12", project: "LEAD" },
      ...extra,
    },
    answer: { by: "jane", at: "2026-09-30T11:00:00Z", choice: "main", body: "Target main" },
  });

  test("a coding run's answered question is a row of its own, as an ask's is", () => {
    const [row] = answeredRows({ handle: "jane", items: [ranAsk()] });
    // KEYED BY ITS TURN AND THE LAUNCH THAT ASKED: a turn's run asks again.
    expect(row!.key).toBe("run:turn-7:q1");
    expect(rowWho(row!, index)?.name).toBe("CTO");
    expect(rowKey(row!)).toBe("LEAD-12");
    // A TURN'S TASK IS ADDRESSED BY ITS ID.
    expect(itemAddress(rowItem(row!)!)).toBe("t-12");
    expect(inboxSelection(row!)).toEqual({ kind: "run", id: "turn-7", noun: "decision" });
    expect(answeredQuestion(ranAsk())).toBe("Which branch should the fix target?");
    expect(answerWords(ranAsk(), "jane", index)).toEqual({
      who: "You",
      verb: "chose",
      said: "Target main",
      note: "",
      kind: "choice",
      recommended: true,
    });
    const inWords = { ...ranAsk(), answer: { by: "jane", at: "x", body: "Neither, hold it." } };
    expect(answerWords(inWords, "jane", index)).toMatchObject({
      verb: "replied",
      said: "Neither, hold it.",
      kind: "reply",
    });
  });

  // ONE QUESTION, ONE ROW. A run's open row is keyed by its turn alone and its
  // answered row by the launch as well, so the two are paired by the question
  // they share, never by key. Mutation: pair by key, and the run is listed in
  // both groups.
  test("a question is in Needs you or under Answered, never both", () => {
    const parked = (launch: string): InboxRow => ({
      kind: "decision",
      key: "run:turn-7",
      at: "2026-09-30T10:55:00Z",
      subject: {
        kind: "run",
        at: "2026-09-30T10:55:00Z",
        run: { turn_id: "turn-7", launch_id: launch } as unknown as SandboxRun,
      },
      notices: [],
    });
    const answeredRow = answeredRows({ handle: "jane", items: [ranAsk()] });
    // ANSWERED ELSEWHERE: the open row the slower read still lists leaves.
    expect(pairAnswered([parked("q1")], answeredRow, new Set())).toEqual({
      needs: [],
      answered: answeredRow,
    });
    // ANSWERED HERE: the row stays in Needs you, and Answered does not list it.
    expect(pairAnswered([parked("q1")], answeredRow, new Set(["run:turn-7"]))).toEqual({
      needs: [parked("q1")],
      answered: [],
    });
    // THE RUN ASKED AGAIN: its new question still waits.
    expect(pairAnswered([parked("q2")], answeredRow, new Set())).toEqual({
      needs: [parked("q2")],
      answered: answeredRow,
    });
  });
});
