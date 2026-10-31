/**
 * The Inbox's rules as values: the rows, where the selection goes once an
 * item is settled, what a notice is called, the read position "Mark all
 * read" sends, the snooze presets and the quiet sentences.
 */

import { describe, expect, test } from "vitest";

import {
  askContext,
  askQuestion,
  changeCard,
  earlierOf,
  findingsLine,
  firstSentence,
  inboxQuiet,
  inboxRows,
  nextAfter,
  noticeNote,
  noticeRows,
  noticeTitle,
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
  CodeInboxNotice,
  InboxAnswer,
  InboxNotice,
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

/** A tracker notice as the composed inbox carries it. */
function composed(n: WorkInboxNotice): InboxNotice {
  return {
    source: "tracker",
    record_id: n.record_id,
    position: { stream: n.log_stream, generation: n.log_generation, seq: n.log_seq },
    at: n.at,
    reason: n.reason,
    primary: n.primary,
    addressed: n.addressed,
    kind: n.kind,
    subject: { id: n.subject_id, key: n.subject_key },
    read: n.read,
    extra: n,
  };
}

/** A code notice as the composed inbox carries it. */
function codeNotice(id: string, extra: Partial<CodeInboxNotice> = {}): InboxNotice {
  const n: CodeInboxNotice = {
    record_id: id,
    log_seq: 7,
    log_stream: "CREWLET_CODE_LOG",
    log_generation: 1,
    at: "2026-09-27T11:00:00Z",
    reason: "review_requested",
    primary: true,
    addressed: true,
    repo: "web",
    change_id: "change-1",
    change_ref: "web!42",
    change: { title: "Retry the upload once", state: "open" },
    read: false,
    ...extra,
  };
  return {
    source: "code",
    record_id: n.record_id,
    position: { stream: n.log_stream, generation: n.log_generation, seq: n.log_seq },
    at: n.at,
    reason: n.reason,
    primary: n.primary,
    addressed: n.addressed,
    kind: "change",
    subject: { id: n.change_id, key: n.change_ref, title: n.change?.title },
    read: n.read,
    extra: n,
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
        composed(notice("r-1", { ask: { comment: "c-12", asked_of: "jane", open: true } })),
        composed(notice("r-2")),
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
      earlierOf(
        [notice("a"), notice("b", { read: true }), notice("c", { read: true })].map(composed),
      ).map((n) => n.record_id),
    ).toEqual(["b", "c"]);
  });

  test("a code notice is a change row, keyed apart from every tracker record", () => {
    const rows = noticeRows([composed(notice("r-1")), codeNotice("r-1")]);
    expect(rows.map((r) => [r.kind, r.key])).toEqual([
      ["notice", "r-1"],
      ["change", "change:r-1"],
    ]);
    expect(rowName(rows[1]!, null)).toBe("web!42");
  });
});

/** A code notice whole, as its card reads it from the composed notice's `extra`. */
function codeExtra(extra: Partial<CodeInboxNotice> = {}): CodeInboxNotice {
  const n = codeNotice("r-1", extra);
  if (n.source !== "code") throw new Error("codeNotice composes a code notice");
  return n.extra;
}

// THE INTENT-FIRST CARD: what the change is for — the work item it
// implements, its description — then what stands in its way, the diff one
// press away, every word of it the notice's own, with no question per card.
describe("a code notice's card", () => {
  const change = {
    title: "Retry the upload once",
    state: "open",
    work_item: { backend: "native", id: "item-7", key: "LEAD-7", project: "LEAD" },
    body: "Uploads retried twice doubled the bill",
    body_cut: true,
    agent_findings: { nit: 3, brand_new: 1, blocker: 1, major: 0 },
    readiness: { met: 3, total: 5, first: "web!42 waits on an approval from a reviewer" },
  };

  test("leads with the work item it implements, as the row's task", () => {
    const n = codeExtra({ change });
    const card = changeCard(n);
    expect(card.item).toEqual({ id: "item-7" });
    expect(card.itemKey).toBe("LEAD-7");
    const row: InboxRow = { kind: "change", key: "change:r-1", at: n.at, notice: n };
    expect(rowItem(row)).toEqual({ id: "item-7" });
    expect(rowKey(row)).toBe("LEAD-7");
    // A VENDOR'S ITEM is addressed by its key too, as a turn's is.
    const jira = { ...change, work_item: { backend: "jira", id: "10042", key: "ENG-4" } };
    expect(changeCard(codeExtra({ change: jira })).item).toEqual({ id: "10042", key: "ENG-4" });
    expect(changeCard(codeExtra()).item).toBeNull();
  });

  test("carries the description, marked where the engine cut it", () => {
    expect(changeCard(codeExtra({ change })).intent).toBe(
      "Uploads retried twice doubled the bill…",
    );
    expect(changeCard(codeExtra({ change: { ...change, body_cut: false } })).intent).toBe(
      "Uploads retried twice doubled the bill",
    );
    expect(changeCard(codeExtra()).intent).toBe("");
  });

  test("counts the open agent findings heaviest first, a weight it does not know last", () => {
    const card = changeCard(codeExtra({ change }));
    expect(card.findings).toEqual([
      { severity: "blocker", count: 1 },
      { severity: "nit", count: 3 },
      { severity: "brand_new", count: 1 },
    ]);
    expect(findingsLine(card.findings)).toBe("Blocker 1 · Nit 3 · brand_new 1");
    expect(findingsLine(changeCard(codeExtra()).findings)).toBe("");
  });

  test("says where a live change's merge stands, and nothing for one that merges nothing", () => {
    expect(changeCard(codeExtra({ change })).readiness).toBe(
      "web!42 waits on an approval from a reviewer",
    );
    const merged = { title: change.title, state: "merged", agent_findings: change.agent_findings };
    expect(changeCard(codeExtra({ change: merged })).readiness).toBeNull();
  });

  test("opens the diff on the change's own page, in one press", () => {
    expect(changeCard(codeExtra()).diff).toBe("#/code/repos/web/changes/42?tab=files");
    expect(changeCard(codeExtra({ change_ref: "not a change" })).diff).toBeNull();
  });

  test("says why it reached the reader in the code host's own words", () => {
    expect(changeCard(codeExtra({ reason: "review_requested" })).why).toBe("asked for your review");
    expect(changeCard(codeExtra({ reason: "re_review" })).why).toBe("asked to look again");
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

  test("a heading is the first sentence of a longer text", () => {
    expect(firstSentence("May I delete it? It holds the fixtures.")).toBe("May I delete it?");
    expect(firstSentence("v1.2 ships today")).toBe("v1.2 ships today");
  });
});

describe("mark all read", () => {
  test("reads through the newest unread notice loaded, by generation then sequence", () => {
    expect(
      readThrough(
        [
          notice("a", { log_generation: 1, log_seq: 90 }),
          notice("b", { log_generation: 2, log_seq: 3 }),
          notice("c", { log_generation: 2, log_seq: 40, read: true }),
        ].map(composed),
      ),
    ).toEqual({ tracker: "CREWLET_TRACKER_LOG@2:3", code: null });
    expect(readThrough([composed(notice("a", { read: true }))])).toEqual({
      tracker: null,
      code: null,
    });
  });

  test("reads each source through its own newest notice, never one through the other's", () => {
    expect(
      readThrough([
        composed(notice("a", { log_generation: 1, log_seq: 90 })),
        codeNotice("x", { log_generation: 1, log_seq: 4 }),
        codeNotice("y", { log_generation: 1, log_seq: 2 }),
      ]),
    ).toEqual({ tracker: "CREWLET_TRACKER_LOG@1:90", code: "CREWLET_CODE_LOG@1:4" });
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

  // A COMPANY ZONE THIS BROWSER CANNOT READ is read as UTC, the day a preset
  // lands on and the instant it names alike, as every company-clock reading
  // is (`range.ts`'s companyZone): never a menu that fails to draw.
  test("read a company zone this browser cannot read as UTC", () => {
    expect(snoozePresets(now, "Not/AZone", undefined)).toEqual(
      snoozePresets(now, "UTC", undefined),
    );
    expect(snoozePresets(now, "Not/AZone", undefined)[1]!.until).toBe("2026-09-24T09:00:00.000Z");
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
  const answer: InboxAnswer = {
    handle: "jane",
    notices: [],
    sources: [
      { name: "tracker", primary_reasons: [] },
      { name: "code", primary_reasons: [] },
    ],
    unread: 0,
    primary: 0,
    complete: true,
  };

  test("say nothing before an answer, beside a refusal or over rows", () => {
    expect(inboxQuiet({ ...base, answer: null })).toBeNull();
    expect(inboxQuiet({ ...base, answer, error: true })).toBeNull();
    expect(inboxQuiet({ ...base, answer, shown: 1 })).toBeNull();
  });

  test("claim no read history for a person who never marked one", () => {
    expect(inboxQuiet({ ...base, answer })?.title).toBe("Nothing has reached you yet");
    // EITHER SOURCE'S READ POSITION is evidence of having read something.
    const read = (name: "tracker" | "code"): InboxAnswer => ({
      ...answer,
      sources: answer.sources.map((s) =>
        s.name === name ? { ...s, seen_through: { stream: "S", seq: 4 } } : s,
      ),
    });
    expect(inboxQuiet({ ...base, answer: read("tracker") })?.title).toBe("You are caught up");
    expect(inboxQuiet({ ...base, answer: read("code") })?.title).toBe("You are caught up");
  });

  test("name what the conditions were measured over for a reader who is nobody", () => {
    expect(inboxQuiet({ ...base, bound: false, answer: null })?.hint).toContain(
      "every seat's own state",
    );
  });
});

// A PARKED RUN'S TASK is a turn's: its work item carries no collision flag, so
// the pane opens, threads and replies by the native task's id, since a key another
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
