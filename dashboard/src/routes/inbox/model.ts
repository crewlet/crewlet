/**
 * The Inbox as values: which rows it holds, how the chips count and narrow
 * them, which day group a notice falls in, and when a snooze can end.
 *
 * PURE, so every rule here is exercised without a socket: the chips and the
 * groups are the two things a reader sees move when a poll lands, and a rule
 * that could only be tested by rendering the screen is a rule nobody re-reads.
 *
 * # One list, three kinds of row
 *
 * A DECISION is what the engine says waits on this person (`decisions`): an ask
 * put to them, or a coding run parked on a question to them — plus the seats
 * the engine stopped on a spent token budget, which a person unblocks. A
 * CONDITION is one of the engine's own conditions a person decides
 * (`conditionsToDecide`). A NOTICE is what reached them, under the one reason
 * of eighteen the applier recorded.
 *
 * AN ASK IS ONE ROW, NOT TWO. The notice that told a person they were asked
 * and the decision the ask IS are the same question, so while the ask is in
 * the "Needs a decision" group its notices are attached to it rather than
 * listed again under Today — and Done or Snooze on the decision marks those
 * notices, which is the only thing about an ask a person's inbox can mark.
 */

import { reasonPhrase } from "~/lib/reasons.ts";
import { companyMidnight } from "~/lib/range.ts";
import { dateFormatter, zoneOffset } from "~/lib/format.ts";
import { conditionKey, type Attention } from "~/lib/attention.ts";
import { DECISIONS_VIEW, subjectKey, type DecisionSubject } from "~/components/DecisionRow.tsx";
import type { WorkInboxAnswer, WorkInboxNotice } from "~/protocol/index.ts";

/** The three scopes, each a different question to the engine. */
export const SCOPES = ["unread", "all", "snoozed"] as const;
export type Scope = (typeof SCOPES)[number];

/** A string off the URL, resolved against the three that exist. */
export function scopeOf(raw: string): Scope {
  return (SCOPES as readonly string[]).includes(raw) ? (raw as Scope) : "unread";
}

/**
 * The chips over the list, as `reason=` names them.
 *
 * NOT THE EIGHTEEN REASONS. Every notice still says its own reason on its row
 * (`contract/reasons.ts`), and the chips are the four questions a person asks
 * of the list: what needs my decision, what needs my sign-off, who mentioned
 * me, what was handed to me. `decisions` is Home's `DECISIONS_VIEW`, so its
 * "Review" lands on this chip.
 */
export const CHIPS = [
  { value: "", label: "Everything" },
  { value: DECISIONS_VIEW, label: "Decisions" },
  { value: "reviews", label: "Reviews" },
  { value: "mentions", label: "Mentions" },
  { value: "assigned", label: "Assigned" },
] as const;
export type Chip = (typeof CHIPS)[number]["value"];

export function chipOf(raw: string): Chip {
  return CHIPS.some((c) => c.value === raw) ? (raw as Chip) : "";
}

/** The notice reasons the two notice chips are, in the engine's own words. */
const CHIP_REASON: Partial<Record<Chip, string>> = {
  mentions: "mention",
  assigned: "assignee",
};

/**
 * One row of the list. `id` is the ROW's identity — what `row=` names in the
 * address and what the pane is remounted on: a decision's subject key, a
 * condition's key or a notice's record id. Never a work item's key, which is a
 * different thing a row may be ABOUT (`NoticeList.rowItem`).
 */
export type InboxRow =
  | {
      kind: "decision";
      id: string;
      at?: string;
      subject: DecisionSubject;
      /** The notices this person holds about it — what Done and Snooze mark. */
      notices: WorkInboxNotice[];
    }
  | { kind: "condition"; id: string; at?: string; item: Attention }
  | { kind: "notice"; id: string; at: string; notice: WorkInboxNotice };

/** The rows, split into the decisions group and the notices it leaves. */
export interface InboxRows {
  decisions: InboxRow[];
  notices: InboxRow[];
}

/**
 * Every row the Inbox holds for one scope.
 *
 * UNDER `snoozed` THERE ARE NO DECISIONS: that scope is what the person put
 * off, and a decision is not a notice anybody can put off — it leaves when it
 * is answered.
 */
export function inboxRows(input: {
  scope: Scope;
  subjects: readonly DecisionSubject[];
  conditions: readonly Attention[];
  notices: readonly WorkInboxNotice[];
}): InboxRows {
  const { scope, subjects, conditions, notices } = input;
  if (scope === "snoozed") {
    return { decisions: [], notices: notices.map(noticeRow) };
  }
  const byAsk = new Map<string, WorkInboxNotice[]>();
  for (const n of notices) {
    const ask = n.ask?.comment;
    if (!ask) continue;
    byAsk.set(ask, [...(byAsk.get(ask) ?? []), n]);
  }
  const attached = new Set<string>();
  const decisions: InboxRow[] = subjects.map((subject) => {
    const held = subject.kind === "ask" ? (byAsk.get(subject.ask.comment) ?? []) : [];
    for (const n of held) attached.add(n.record_id);
    return { kind: "decision", id: subjectKey(subject), at: subject.at, subject, notices: held };
  });
  for (const item of conditions) {
    decisions.push({ kind: "condition", id: conditionKey(item.id), at: item.at, item });
  }
  return {
    decisions,
    notices: notices.filter((n) => !attached.has(n.record_id)).map(noticeRow),
  };
}

function noticeRow(notice: WorkInboxNotice): InboxRow {
  return { kind: "notice", id: notice.record_id, at: notice.at, notice };
}

/** Whether a row answers a chip. */
export function rowMatches(row: InboxRow, chip: Chip): boolean {
  switch (chip) {
    case "":
      return true;
    case DECISIONS_VIEW:
      return row.kind !== "notice";
    case "reviews":
      return (
        row.kind === "decision" &&
        row.subject.kind === "ask" &&
        row.subject.ask.decision?.role === "approver"
      );
    default:
      return row.kind === "notice" && row.notice.reason === CHIP_REASON[chip];
  }
}

/**
 * How many rows each chip holds, OVER THE ROWS LOADED — which each chip says
 * of itself wherever the page is not every row, because a page of fifty
 * notices is a page, not a total.
 */
export function chipCounts(rows: InboxRows): Record<Chip, number> {
  const all = [...rows.decisions, ...rows.notices];
  const out = {} as Record<Chip, number>;
  for (const chip of CHIPS) out[chip.value] = all.filter((r) => rowMatches(r, chip.value)).length;
  return out;
}

/** A day group's heading and its notices. */
export interface DayGroup {
  label: "Today" | "Yesterday" | "Earlier";
  rows: InboxRow[];
}

/**
 * The notices in Today, Yesterday and Earlier, cut at the COMPANY's midnight.
 *
 * THE COMPANY'S CLOCK, not the reader's: "today" on this screen is the day the
 * engine counts a day in (its budgets, its day charts), and a founder in
 * Lisbon reading a company run on New York time would otherwise see a notice
 * filed at 23:00 company time under "Today" on one screen and "Yesterday" on
 * the next. A group with nothing in it is not drawn.
 */
export function dayGroups(
  rows: readonly InboxRow[],
  now: number,
  zone: string | undefined,
): DayGroup[] {
  const today = companyMidnight(now, zone);
  const yesterday = companyMidnight(today - 1, zone);
  const groups: DayGroup[] = [
    { label: "Today", rows: [] },
    { label: "Yesterday", rows: [] },
    { label: "Earlier", rows: [] },
  ];
  for (const row of rows) {
    const at = Date.parse(row.at ?? "");
    const group = !Number.isFinite(at) || at < yesterday ? 2 : at < today ? 1 : 0;
    groups[group]!.rows.push(row);
  }
  return groups.filter((g) => g.rows.length > 0);
}

/**
 * Where "Mark all read" reads through: the newest position among the notices
 * LOADED, as `<stream>@<generation>:<sequence>`.
 *
 * THE NEWEST LOADED, never "everything": a notice that arrived after this
 * page was drawn is one the person has not seen, and reading through it on
 * their behalf would mark it without them. Null when nothing loaded is
 * unread, since the gesture would change nothing.
 */
export function readThrough(notices: readonly WorkInboxNotice[]): string | null {
  let newest: WorkInboxNotice | null = null;
  for (const n of notices) {
    if (n.read) continue;
    if (
      !newest ||
      n.log_generation > newest.log_generation ||
      (n.log_generation === newest.log_generation && n.log_seq > newest.log_seq)
    ) {
      newest = n;
    }
  }
  return newest ? `${newest.log_stream}@${newest.log_generation}:${newest.log_seq}` : null;
}

/** One way to put a notice off. */
export interface SnoozePreset {
  key: string;
  label: string;
  /** When it comes back, as RFC3339. */
  until: string;
  /** The day a company-clock preset lands on, in that clock ("Mon, Sep 28"),
   *  so "09:00" is never read against the reader's own zone. */
  day?: string;
}

/**
 * The snooze presets this person may set now: an hour, tomorrow at nine and
 * next Monday at nine, both on the COMPANY's clock.
 *
 * ONLY THE ONES THE ENGINE WILL TAKE. The write refuses a snooze further ahead
 * than `max_snooze_ahead` (the person answer serves it), so a preset past it
 * is a button that is refused every time it is pressed — it is not offered.
 * An answer that does not carry the bound (an older node) offers them all and
 * the engine remains the judge.
 */
export function snoozePresets(
  now: number,
  zone: string | undefined,
  maxAheadSeconds: number | undefined,
): SnoozePreset[] {
  const tz = zone || "UTC";
  const civil = civilDate(now, tz);
  const tomorrow = addDays(civil, 1);
  // Monday is 1; a reader on a Monday means the one after.
  const weekday = new Date(Date.UTC(civil.y, civil.m - 1, civil.d)).getUTCDay();
  const monday = addDays(civil, (8 - weekday) % 7 || 7);
  const presets: SnoozePreset[] = [
    { key: "hour", label: "In an hour", until: new Date(now + 3_600_000).toISOString() },
    {
      key: "tomorrow",
      label: "Tomorrow, 09:00",
      until: atNine(tomorrow, tz),
      day: dayIn(tomorrow),
    },
    { key: "monday", label: "Next Monday, 09:00", until: atNine(monday, tz), day: dayIn(monday) },
  ];
  if (maxAheadSeconds === undefined || !(maxAheadSeconds > 0)) return presets;
  const limit = now + maxAheadSeconds * 1_000;
  return presets.filter((p) => Date.parse(p.until) <= limit);
}

interface Civil {
  y: number;
  m: number;
  d: number;
}

function civilDate(at: number, tz: string): Civil {
  // THROUGH THE KEPT FORMATTERS (`lib/format.ts`), one per zone: a zone
  // `Intl` does not know throws at construction and is never kept, which is
  // what sends a mistyped company zone to UTC here.
  let parts: Intl.DateTimeFormatPart[];
  try {
    parts = dateFormatter("en-CA", {
      timeZone: tz,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    }).formatToParts(at);
  } catch {
    parts = dateFormatter("en-CA", {
      timeZone: "UTC",
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    }).formatToParts(at);
  }
  const get = (type: string) => Number(parts.find((p) => p.type === type)?.value);
  return { y: get("year"), m: get("month"), d: get("day") };
}

/** A civil date as a reader names it: "Mon, Sep 28". */
function dayIn(c: Civil): string {
  return dateFormatter("en-US", {
    weekday: "short",
    month: "short",
    day: "numeric",
    timeZone: "UTC",
  }).format(Date.UTC(c.y, c.m - 1, c.d));
}

function addDays(c: Civil, days: number): Civil {
  const t = new Date(Date.UTC(c.y, c.m - 1, c.d + days));
  return { y: t.getUTCFullYear(), m: t.getUTCMonth() + 1, d: t.getUTCDate() };
}

/**
 * 09:00 on a civil date in a zone, as an instant. The zone's offset is read at
 * the guess and again at the answer, because the two differ exactly on the
 * day a clock moves.
 */
function atNine(c: Civil, tz: string): string {
  const wall = Date.UTC(c.y, c.m - 1, c.d, 9, 0, 0);
  let offset: number;
  try {
    offset = zoneOffset(wall, tz);
    offset = zoneOffset(wall - offset, tz);
  } catch {
    offset = 0;
  }
  return new Date(wall - offset).toISOString();
}

/**
 * The words for a notice's pill, and whether it asks something of the reader.
 *
 * `asked` DRAWS AS A NEED — it is the one reason whose notice waits on an
 * answer — and every other reason is a quiet fact. The reason's words are
 * `contract/reasons.ts`'s; the pills for the rows that are NOT notices are
 * [CONDITION_PILLS], kept out of that table because they are not reasons.
 */
export function noticePill(notice: WorkInboxNotice): { label: string; need: boolean } {
  return {
    label: reasonPhrase(notice.reason),
    need: notice.addressed || notice.reason === "asked",
  };
}

/**
 * THE PILLS FOR THE ROWS THAT ARE NOT NOTICES — a parked run, a stopped seat,
 * a condition. An ask's pill is the `asked` reason's own words, since an ask
 * IS that reason; these are not reasons, so they live here rather than in
 * `contract/reasons.ts`, whose table is exactly the engine's eighteen.
 */
export const CONDITION_PILLS = {
  run: "run waiting on you",
  seat: "seat stopped",
  failed: "seat failed",
  refusing: "budget refusing",
  near: "budget nearly spent",
} as const;

export type PillTone = "warning" | "danger" | "neutral";

/** A decision or condition row's pill: its words, and whose move it is. */
export function rowPill(row: InboxRow): { label: string; tone: PillTone } {
  switch (row.kind) {
    case "notice": {
      const { label, need } = noticePill(row.notice);
      return { label, tone: need ? "warning" : "neutral" };
    }
    case "decision":
      switch (row.subject.kind) {
        case "ask":
          return { label: reasonPhrase("asked"), tone: "warning" };
        case "run":
          return { label: CONDITION_PILLS.run, tone: "warning" };
        case "seat":
          return { label: CONDITION_PILLS.seat, tone: "danger" };
      }
      break;
    case "condition": {
      const item = row.item;
      const tone: PillTone = item.severity === "critical" ? "danger" : "warning";
      if (item.subject === "budget") {
        return {
          label: item.id.endsWith("-near") ? CONDITION_PILLS.near : CONDITION_PILLS.refusing,
          tone,
        };
      }
      return {
        label: item.id.startsWith("error-") ? CONDITION_PILLS.failed : CONDITION_PILLS.seat,
        tone,
      };
    }
  }
  return { label: "", tone: "neutral" };
}

/**
 * What the list says when it draws no rows — and null where it must say
 * nothing (nothing has answered, or the answer was a refusal the list draws).
 *
 * DERIVED FROM THE ANSWER, NEVER FROM THE ROW COUNT ALONE. Six facts share a
 * count of zero — nothing answered yet, a refusal, a chip that took every row,
 * a caught-up reader, an empty scope and a person nothing has ever reached —
 * and a list that inferred one sentence from the count told a person the
 * applier had never written a row for that "everything has been marked read".
 *
 * `seen_through` IS THE EVIDENCE of having read anything: the person's own
 * watermark, written by `mark_inbox` alone and omitted by the engine while it
 * is zero.
 */
export function inboxQuiet(input: {
  /** The notices answer, or null where there is none (unanswered, or nobody bound). */
  answer: WorkInboxAnswer | null;
  /** The notices are part of this view: a bound reader. */
  bound: boolean;
  error: boolean;
  scope: Scope;
  chip: Chip;
  /** Rows drawn after the chip. */
  shown: number;
  /** The sentence naming what the conditions are measured over. */
  watched: string;
}): { title: string; hint: string } | null {
  const { answer, bound, error, scope, chip, shown, watched } = input;
  if (error || shown > 0) return null;
  if (!bound) {
    return {
      title: "Nothing needs a decision",
      hint: `Checked and clear: ${watched}.`,
    };
  }
  if (!answer) return null;
  if (chip === DECISIONS_VIEW) {
    return {
      title: "Nothing waits on your decision",
      hint: `No question is put to you, no coding run is waiting on your answer, and nothing in ${watched} needs a person.`,
    };
  }
  if (chip === "reviews") {
    return {
      title: "Nothing waits on your sign-off",
      hint: "A review is a decision you were asked to make as its approver.",
    };
  }
  if (chip !== "" && answer.notices.length > 0) {
    const held = answer.notices.length;
    return {
      title: "Nothing on this page matches",
      hint: `The page holds ${held} notice${held === 1 ? "" : "s"} under other reasons. Everything shows them all.`,
    };
  }
  const arrives =
    "A notice arrives when something you are on the hook for moves — a mention, a question, work assigned to you, a task you watch.";
  if (scope === "snoozed") {
    return {
      title: "Nothing is snoozed",
      hint: "A snooze means not now — the notice leaves your list until the time set on it, and comes back when it arrives.",
    };
  }
  if (scope === "all") return { title: "Nothing has reached you", hint: arrives };
  return (answer.seen_through?.seq ?? 0) > 0
    ? {
        title: "You are caught up",
        hint: "Everything that reached you has been marked read. All reads back through it.",
      }
    : {
        title: "Nothing has reached you yet",
        hint: `${arrives} Nothing has been marked read either, so All holds this same empty list.`,
      };
}
