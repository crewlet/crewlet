/**
 * The Inbox as values: which rows it holds and in what order, what a row and
 * the pane say about each, where the selection goes once the open row is
 * settled, and when a snooze can end.
 *
 * PURE, so every rule here is exercised without a socket: the order is what
 * j and k step through and what the selection moves along after an answer,
 * and a rule that could only be tested by rendering the screen is a rule
 * nobody re-reads.
 *
 * # Two groups, and two links under them
 *
 * NEEDS YOU is what waits on this person: an ask put to them, a coding run
 * parked on a question to them, a seat the engine stopped on a spent token
 * budget, and the engine's own conditions a person decides
 * (`conditionsToDecide`). UPDATES is what reached them and is not read yet.
 * What they snoozed and what they already read are not filters over the list
 * but two quiet links at its end, Snoozed and Earlier, each read when it is
 * opened: a filter bar is read every time, and these are opened rarely.
 *
 * EVERY NATIVE DOMAIN'S NOTICES, ONE LIST. The engine composes the inbox
 * (`inbox`): the tracker's notices and the code host's, interleaved by when
 * each happened, each source read under its own permission. A tracker notice
 * is a `notice` row as it always was, its own shape in the composed notice's
 * `extra`; a code notice is a `change` row. Each domain keeps the marks over
 * its own notices on its own log, so a row is marked through its own source's
 * tool, and "Mark all read" reads each source through its own newest notice.
 *
 * AN ASK IS ONE ROW, NOT TWO. The notice that told a person they were asked
 * and the decision the ask IS are the same question, so while the ask is in
 * Needs you its notices ride on it rather than being listed again under
 * Updates, and answering it marks them read, so an answered question does not
 * come back as an update.
 */

import {
  firstLine,
  fmtDateCompact,
  fmtDuration,
  plural,
  toWall,
  zoneOffset,
} from "~/lib/format.ts";
import { dateTimeFormat, type DateShape } from "~/lib/formatters.ts";
import { zone } from "~/lib/prefs.ts";
import { companyZone } from "~/lib/range.ts";
import { plainText } from "~/lib/markdown.ts";
import { reasonPhrase, reasonWhy } from "~/lib/reasons.ts";
import { codeReasonPhrase, parseChangeRef, severityWord } from "~/lib/code.ts";
import { FINDING_SEVERITIES } from "~/contract/code.ts";
import { changeDiffHref } from "~/routes/code/paths.ts";
import {
  ENGINE_SENTENCES,
  authorOf,
  nameAuthor,
  noticeItem,
  turnItem,
  type ItemRef,
} from "~/lib/work.ts";
import { PERIOD_ADJECTIVE } from "~/lib/budget.ts";
import { conditionKey, type Attention } from "~/lib/attention.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";
import type { OrgIndex } from "~/lib/seats.ts";
import { subjectKey } from "~/components/DecisionRow.tsx";
import type { DecisionSubject } from "~/lib/decisions.ts";
import {
  checkStateWord,
  classCount,
  gatedByChecks,
  humanPathsAsking,
  measuredChecks,
  type ClassCount,
  type Measured,
} from "~/lib/checks.ts";
import type {
  CodeChangeAnswer,
  CodeCheckRow,
  CodeInboxNotice,
  InboxAnswer,
  InboxNotice,
  WorkAskRow,
  WorkInboxNotice,
} from "~/protocol/index.ts";

export type InboxRow =
  | {
      kind: "decision";
      key: string;
      at?: string;
      subject: DecisionSubject;
      /** The notices this person holds about it: what answering it marks read. */
      notices: WorkInboxNotice[];
    }
  | { kind: "condition"; key: string; at?: string; item: Attention }
  | { kind: "notice"; key: string; at: string; notice: WorkInboxNotice }
  | { kind: "change"; key: string; at: string; notice: CodeInboxNotice };

/** A row a reader marks: an update from either source. */
export type NoticeRow = Extract<InboxRow, { kind: "notice" | "change" }>;

/** Whether a row is an update a reader marks, from either source. */
export function isNoticeRow(row: InboxRow): row is NoticeRow {
  return row.kind === "notice" || row.kind === "change";
}

/**
 * A code notice's row key: the record id behind a prefix of its own, so a
 * code record and a tracker record can never name one row in the URL.
 */
export function changeRowKey(recordId: string): string {
  return `change:${recordId}`;
}

/** The two groups the list always draws. */
export interface InboxRows {
  needs: InboxRow[];
  updates: InboxRow[];
}

/**
 * Needs you and Updates.
 *
 * NEEDS YOU IS NEWEST FIRST with the conditions after the decisions, the order
 * `decisionSubjects` hands the decisions in: a condition is the engine's own
 * reading of the company and stays until it clears, while a decision is
 * somebody waiting on an answer.
 */
export function inboxRows(input: {
  subjects: readonly DecisionSubject[];
  conditions: readonly Attention[];
  /** The unread notices that are not snoozed, every source's: what Updates lists. */
  notices: readonly InboxNotice[];
}): InboxRows {
  const { subjects, conditions, notices } = input;
  const byAsk = new Map<string, WorkInboxNotice[]>();
  for (const n of notices) {
    if (n.source !== "tracker") continue;
    const ask = n.extra.ask?.comment;
    if (!ask) continue;
    byAsk.set(ask, [...(byAsk.get(ask) ?? []), n.extra]);
  }
  const attached = new Set<string>();
  const needs: InboxRow[] = subjects.map((subject) => {
    const held = subject.kind === "ask" ? (byAsk.get(subject.ask.comment) ?? []) : [];
    for (const n of held) attached.add(n.record_id);
    return { kind: "decision", key: subjectKey(subject), at: subject.at, subject, notices: held };
  });
  for (const item of conditions) {
    needs.push({ kind: "condition", key: conditionKey(item.id), at: item.at, item });
  }
  return {
    needs,
    updates: noticeRows(notices.filter((n) => !attached.has(n.record_id))),
  };
}

/**
 * Notices as rows, for Updates and for the Snoozed and Earlier links, in the
 * order the engine composed them: a tracker notice as a `notice` row, a code
 * notice as a `change` row.
 */
export function noticeRows(notices: readonly InboxNotice[]): InboxRow[] {
  return notices.map((n): InboxRow =>
    n.source === "code"
      ? { kind: "change", key: changeRowKey(n.record_id), at: n.at, notice: n.extra }
      : { kind: "notice", key: n.record_id, at: n.at, notice: n.extra },
  );
}

/**
 * The notices a page loaded that Earlier lists: the read ones.
 *
 * THE ENGINE HAS NO "READ ONLY" QUESTION. `unread: false` answers every notice
 * that is not snoozed, read or not, so Earlier is that page with the unread
 * ones taken out (they are in Updates already).
 */
export function earlierOf(notices: readonly InboxNotice[]): InboxNotice[] {
  return notices.filter((n) => n.read);
}

/**
 * Where the selection goes once the open row is settled: the first row after
 * it, in the order the list had when it was settled, that is still listed and
 * not settled itself; failing that, the first unsettled row anywhere.
 *
 * THE ORDER IT HAD, because a settled row may already have left the list (a
 * read notice leaves Updates when the read lands), and "the row after it" is a
 * question about where it WAS. Null where nothing is left to open.
 */
export function nextAfter(
  key: string,
  before: readonly string[],
  now: readonly string[],
  settled: ReadonlySet<string>,
): string | null {
  const listed = new Set(now);
  const open = (k: string) => k !== key && listed.has(k) && !settled.has(k);
  const at = before.indexOf(key);
  const following = at < 0 ? undefined : before.slice(at + 1).find(open);
  return following ?? now.find(open) ?? null;
}

/** Where "Mark all read" reads each source through, where it has anything unread. */
export interface ReadThrough {
  tracker: string | null;
  code: string | null;
}

/**
 * Where "Mark all read" reads through: for EACH SOURCE, the newest position
 * among its notices LOADED, as `<stream>@<generation>:<sequence>`.
 *
 * ONE POSITION PER SOURCE, because each domain counts its own log: a
 * position on the tracker's log says nothing about the code log's, so the
 * gesture is two writes, each through its own source's newest notice.
 *
 * THE NEWEST LOADED, never "everything": a notice that arrived after this
 * page was drawn is one the person has not seen, and reading through it on
 * their behalf would mark it without them. Null for a source with nothing
 * loaded unread, since its gesture would change nothing.
 */
export function readThrough(notices: readonly InboxNotice[]): ReadThrough {
  const newest: Record<"tracker" | "code", InboxNotice | null> = { tracker: null, code: null };
  for (const n of notices) {
    if (n.read) continue;
    const held = newest[n.source];
    const at = n.position;
    if (
      !held ||
      (at.generation ?? 0) > (held.position.generation ?? 0) ||
      ((at.generation ?? 0) === (held.position.generation ?? 0) && at.seq > held.position.seq)
    ) {
      newest[n.source] = n;
    }
  }
  const spelled = (n: InboxNotice | null) =>
    n ? `${n.position.stream}@${n.position.generation ?? 0}:${n.position.seq}` : null;
  return { tracker: spelled(newest.tracker), code: spelled(newest.code) };
}

// ---------------------------------------------------------------------------
// Who and what
// ---------------------------------------------------------------------------

/** Who a row is about, as its badge and its first words draw it. */
export interface Who {
  name: string;
  kind: "agent" | "human";
  /** How an agent the chart holds is drawn; null for a person or a seat the chart has lost. */
  avatar: AgentAvatar | null;
  /** Null where the row is about no seat at all (the company's budget). */
  handle: string | null;
}

/**
 * The seat behind a handle, named as the chart names it, or, for a name no
 * seat holds, the name itself: the LOGIN of somebody the identity directory
 * binds to no seat, which is the name their own changes are recorded under
 * (`iam.ActorFor`). `kind` is what the record says the author is, for a name
 * the chart cannot answer for: a login is a person's, never an agent's.
 */
export function whoOf(
  index: OrgIndex,
  handle: string | null | undefined,
  kind?: string,
): Who | null {
  const h = (handle ?? "").trim();
  if (!h) return null;
  const seat = index.byHandle.get(h);
  return {
    name: seat?.name ?? h,
    kind: seat?.kind ?? (kind === undefined || kind === "agent" ? "agent" : "human"),
    avatar: seat?.avatar ?? null,
    handle: h,
  };
}

/**
 * Who a row is about.
 *
 * A NOTICE'S AUTHOR IS WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person the
 * identity directory binds to a seat writes AS that seat, so their change is
 * the seat's and draws as them; anybody bound to none writes under their own
 * login, which is a name rather than a secret and is drawn as one. A change
 * the engine made itself (`system`) names nobody.
 */
export function rowWho(row: InboxRow, index: OrgIndex): Who | null {
  switch (row.kind) {
    case "decision":
      switch (row.subject.kind) {
        case "ask":
          return whoOf(index, row.subject.ask.asked_by);
        case "run":
          return whoOf(index, row.subject.run.agent_handle);
        case "seat":
          return whoOf(index, row.subject.seat.row.handle ?? row.subject.seat.row.role);
      }
      break;
    case "condition":
      return whoOf(index, row.item.who);
    case "notice": {
      const n = row.notice;
      if (n.actor_kind === "system") return null;
      return whoOf(index, n.actor, n.actor_kind);
    }
    case "change":
      // A CODE NOTICE NAMES NOBODY: the code host keeps who it reached and
      // why, and the change's own page says who said what.
      return null;
  }
  return null;
}

/**
 * The name a row leads with: its seat's, or what stands in for nobody. A row
 * about no seat is the company's own condition, or a change the engine made
 * itself.
 */
export function rowName(row: InboxRow, who: Who | null): string {
  if (who) return who.name;
  if (row.kind === "change") return row.notice.change_ref;
  return row.kind === "condition" ? "The company" : "The engine";
}

/**
 * The work item a row is on, or null, to ADDRESS it ([itemAddress]): a key
 * another task claimed first opens that task, so a row about the task that did
 * not claim it links, threads and replies by its id. The engine says so on
 * every row that carries a key it may not own (`key_collision`,
 * `subject_key_collision`); a turn's reference carries no flag, so a native
 * task a turn was on is addressed by its id ([turnItem]).
 */
export function rowItem(row: InboxRow): ItemRef | null {
  switch (row.kind) {
    case "decision":
      switch (row.subject.kind) {
        case "ask":
          return row.subject.ask;
        case "run": {
          const ref = row.subject.run.work_item;
          return ref ? turnItem(ref) : null;
        }
        case "seat": {
          const r = row.subject.seat.row;
          const ref = r.turn?.work_item ?? r.live_call?.work_item;
          return ref ? turnItem(ref) : null;
        }
      }
      break;
    case "condition":
      return null;
    case "notice": {
      const item = noticeItem(row.notice);
      return item.id || item.key ? item : null;
    }
    case "change":
      // THE WORK ITEM THE CHANGE IMPLEMENTS is the row's task, as a turn's is
      // a run's: the intent the change serves, one click from the card.
      return changeItem(row.notice);
  }
  return null;
}

/** The work item a code notice's change implements, to address it, or null. */
function changeItem(notice: CodeInboxNotice): ItemRef | null {
  const work = notice.change?.work_item;
  return work ? turnItem({ ...work, project: work.project ?? "" }) : null;
}

/** The work item a row is on, by the key a reader reads, or "". */
export function rowKey(row: InboxRow): string {
  switch (row.kind) {
    case "decision":
      switch (row.subject.kind) {
        case "ask":
          return row.subject.ask.key;
        case "run":
          return row.subject.run.work_item?.key ?? "";
        case "seat": {
          const r = row.subject.seat.row;
          return r.turn?.work_item?.key ?? r.live_call?.work_item?.key ?? "";
        }
      }
      break;
    case "condition":
      return "";
    case "notice":
      return row.notice.subject_key ?? "";
    case "change":
      return row.notice.change?.work_item?.key ?? "";
  }
  return "";
}

/**
 * What a code notice says: the change's title as its row's line, and what the
 * record said — a comment, a review's summary, the description it opened with
 * — as the quieter words after it.
 */
export function changeWords(notice: CodeInboxNotice): { text: string; excerpt?: string } {
  const text = notice.change?.title || notice.change_ref;
  const said = plainText(notice.excerpt ?? "");
  return said ? { text, excerpt: said } : { text };
}

/** How many open agent findings stand on a change at one weight. */
export interface FindingCount {
  severity: string;
  count: number;
}

/**
 * A code notice's card, INTENT FIRST: what the change is for before what it
 * did — the work item it implements and the description its author wrote —
 * then what stands in its way: the findings seats raised that still stand
 * open, the heaviest first, and the one sentence of where its merge stands.
 * Its diff is one click away, on the change's own page.
 *
 * DRAWN FROM THE NOTICE ALONE, with no question per card: the engine reads
 * all of it with the inbox page, in the page's one snapshot
 * (`code.NoticeChange`), so a page of fifty cards is one read and a card
 * never says something the row beside it contradicts.
 */
export interface ChangeCard {
  /** The change's repository and number, from its reference (`web!42`); null
   *  for a reference this build cannot read, which then links nowhere. */
  named: { repo: string; number: number } | null;
  /** Why it reached the reader, in their own voice ("asked for your review"). */
  why: string;
  /** The work item it implements, to open, and the key it is named by. */
  item: ItemRef | null;
  itemKey: string;
  /** Its description's opening, ending in "…" where the engine cut it; "" for none. */
  intent: string;
  /** Its open agent findings by weight, the heaviest first; none at zero. */
  findings: FindingCount[];
  /** Where a live change's merge stands, the engine's sentence; null for a change
   *  no longer live, or one whose change no row holds any more. */
  readiness: string | null;
  /** The change's page on its diff; null where the reference names no change. */
  diff: string | null;
}

/** A code notice as its card draws it — [ChangeCard]. */
export function changeCard(notice: CodeInboxNotice): ChangeCard {
  const named = parseChangeRef(notice.change_ref);
  const change = notice.change;
  const body = change?.body ?? "";
  return {
    named,
    why: codeReasonPhrase(notice.reason, "short"),
    item: changeItem(notice),
    itemKey: change?.work_item?.key ?? "",
    intent: body && change?.body_cut ? `${body}…` : body,
    findings: findingCounts(change?.agent_findings),
    readiness: change?.readiness?.first || null,
    diff: named ? changeDiffHref(named.repo, named.number) : null,
  };
}

/**
 * What a change's merge gate says on its card (M5), read for the ONE card
 * the reader has open — never a question per listed row, which keeps a page
 * of fifty cards one read ([ChangeCard]):
 *
 * - `measured`: what the head revision's REQUIRED checks measured, each
 *   one's state and how long it ran — durations, never tokens: a check runs
 *   no model. Null where the target holds the change to no check (its
 *   readiness judges no `check` precondition), so a change into an ungated
 *   branch says nothing of checks rather than "none measured".
 * - `classes`: the head revision's change classes in a line — how many of
 *   its touched paths change only whitespace or only comments — or "" where
 *   no lexer classed anything.
 * - `humanPaths`: the engine's sentence of the human paths that asked for
 *   THIS reader — the gate's human_approval precondition, unmet and waiting
 *   on them — or "" where none does.
 *
 * Every field is read from the change's own answers (`code_change`'s
 * readiness and revisions, `code_checks` of the change), so the card needs
 * nothing of the engine the change's page does not already read.
 */
export interface ChangeGate {
  measured: Measured[] | null;
  classes: string;
  humanPaths: string;
}

/** A change's gate as its open card draws it — [ChangeGate]. */
export function changeGate(
  answer: CodeChangeAnswer | undefined,
  checks: readonly CodeCheckRow[],
  handle: string,
): ChangeGate {
  const change = answer?.change;
  const readiness = change?.readiness;
  const head = (answer?.revisions ?? []).find((r) => r.n === change?.revision);
  return {
    measured: change && gatedByChecks(readiness) ? measuredChecks(checks, change.head) : null,
    classes: classesLine(classCount(head?.facts)),
    humanPaths: humanPathsAsking(readiness, handle) ?? "",
  };
}

/**
 * A revision's change classes in a line: "Of the 5 paths it touches, 2
 * change only whitespace, 1 only comments; the rest change code" — the rest
 * said only where every path was classified, since a path no lexer reached
 * is in neither list without being code. "" where nothing was classed.
 */
export function classesLine(count: ClassCount | null): string {
  if (!count) return "";
  const parts: string[] = [];
  if (count.whitespaceOnly > 0) parts.push(`${count.whitespaceOnly} change only whitespace`);
  if (count.commentOnly > 0) parts.push(`${count.commentOnly} only comments`);
  if (parts.length === 0) return count.complete ? "Every path it touches changes code" : "";
  const lead =
    count.touched === null
      ? "Of the paths it touches"
      : `Of the ${plural(count.touched, "path")} it touches`;
  return `${lead}, ${parts.join(", ")}${count.complete ? "; the rest change code" : ""}`;
}

/** One line of what a check measured: "unit passed in 1m 4s", "e2e running". */
export function measuredLine(m: Measured): string {
  const word = checkStateWord(m.state);
  return m.durationMs === null
    ? `${m.name} ${word}`
    : `${m.name} ${word} in ${fmtDuration(m.durationMs)}`;
}

/**
 * A card's open agent findings as one line, the heaviest first: "Blocker 1 ·
 * Nit 3". Weight then count, never "3 nit": a weight is an adjective ("Major"),
 * and no plural reads right after a number for all four. "" for none.
 */
export function findingsLine(findings: readonly FindingCount[]): string {
  return findings.map((f) => `${severityWord(f.severity)} ${f.count}`).join(" · ");
}

/**
 * Counts by weight, in the contract's order, the heaviest first — a blocker
 * before a nit — and a weight a newer engine names after every one this build
 * knows, by name, rather than dropped.
 */
function findingCounts(counts: Readonly<Record<string, number>> | undefined): FindingCount[] {
  const known: readonly string[] = FINDING_SEVERITIES;
  const rank = (severity: string) => {
    const at = known.indexOf(severity);
    return at < 0 ? known.length : at;
  };
  return Object.entries(counts ?? {})
    .filter(([, count]) => count > 0)
    .map(([severity, count]) => ({ severity, count }))
    .sort((a, b) => rank(a.severity) - rank(b.severity) || a.severity.localeCompare(b.severity));
}

/**
 * What a notice SAYS, in this screen's words.
 *
 * The excerpt, flattened, and, where the engine composed it (a reorder of
 * the reader's priorities), with its author named as the row above it names
 * them: the engine writes a HANDLE, because a seat reads the same sentence,
 * and the row read "Maya Ops" over "maya-ops put LEAD-3 at position 1 of your
 * priorities". The Inbox is the reader's own, so "your" is theirs.
 */
export function noticeText(notice: WorkInboxNotice, index: OrgIndex): string {
  const said = plainText(notice.excerpt ?? "");
  if (!ENGINE_SENTENCES.has(notice.kind)) return said;
  return nameAuthor(said, authorOf(notice), (h) => index.byHandle.get(h)?.name ?? h);
}

/** A sentence's first letter in capitals: a reason's words as a heading. */
function capital(s: string): string {
  return s ? s[0]!.toUpperCase() + s.slice(1) : s;
}

/**
 * The first sentence of a text, or its first line where no sentence ends in
 * it: what fits a heading of what a longer text says.
 */
export function firstSentence(text: string): string {
  const line = firstLine(text);
  const end = /^(.+?[.?!])(?:\s|$)/.exec(line);
  return end ? end[1]! : line;
}

/** What a change of each kind did, for a notice whose reason does not say it. */
const CHANGE_DONE: Readonly<Record<string, string>> = {
  created: "Created the task",
  fields: "Changed its fields",
  status: "Changed its status",
  assignee: "Changed its assignee",
  collaborators: "Changed its collaborators",
  watchers: "Changed its watchers",
  tags: "Changed its tags",
  relations: "Changed its links",
  routed: "Routed it",
  moved: "Moved it",
  reparented: "Moved it under another task",
  checklist: "Changed its checklist",
  archived: "Archived it",
  comment: "Commented",
  comment_edited: "Edited a comment",
  comment_resolved: "Resolved a comment",
  comment_removed: "Removed a comment",
  removed: "Removed it",
  restored: "Restored it",
  purged: "Purged it",
  prioritised: "Prioritised it",
  file_written: "Wrote a file",
  file_removed: "Removed a file",
};

/**
 * What a notice did, without who or the task, which the row and the byline
 * already say: "Mentioned you", "Changed its status".
 *
 * A PRIMARY NOTICE IS NAMED BY ITS REASON, because its reason is the news (a
 * person was mentioned, asked, handed a task), and the engine states which
 * notices are primary for this person. Any other notice is named by what the
 * change did, and the byline says why it reached them.
 */
export function noticeTitle(notice: WorkInboxNotice): string {
  if (notice.primary && notice.reason) return capital(reasonPhrase(notice.reason));
  return CHANGE_DONE[notice.kind] ?? capital(notice.kind.replaceAll("_", " "));
}

/**
 * Why a notice reached the reader, where its title does not already say it:
 * the snooze that put it off, the fallback that brought it, or the reason of
 * a notice that only informs ("you follow this task").
 */
export function noticeNote(notice: WorkInboxNotice, until: (at: string) => string): string {
  if (notice.snoozed && notice.snoozed_until) return `snoozed until ${until(notice.snoozed_until)}`;
  if (notice.fallback) return "nobody better was found for it";
  if (notice.primary) return "";
  return notice.reason ? reasonWhy(notice.reason) : "";
}

/** A free-text ask's question is its first line, and the rest its context. */
export function askQuestion(ask: WorkAskRow): string {
  return ask.decision?.question || firstLine(ask.body) || ask.title;
}

/** What an ask says beside its question: the body, or what follows its first line. */
export function askContext(ask: WorkAskRow): string {
  if (ask.decision) return ask.body.trim();
  const lines = ask.body.split("\n");
  const first = lines.findIndex((l) => l.trim());
  return first < 0
    ? ""
    : lines
        .slice(first + 1)
        .join("\n")
        .trim();
}

/** A spent budget, as the row and the pane both say it. */
export function budgetLine(period: keyof typeof PERIOD_ADJECTIVE | undefined): string {
  return period
    ? `The ${PERIOD_ADJECTIVE[period]} token budget is spent`
    : "The token budget is spent";
}

/**
 * WHEN, as few characters as a row can spare: "now", "12m", "3h", then the
 * weekday inside a week and the date past it.
 *
 * A MOMENT, NOT AN AGE, which is why it is not `lib/seats.ts`' `shortAge`
 * ("2d"): a list of what reached somebody is read as a timeline, where "Tue"
 * places a row and "4d" makes the reader count back.
 */
export function shortWhen(at: string | undefined, now: number): string {
  const t = Date.parse(at ?? "");
  if (!Number.isFinite(t)) return "";
  const minutes = Math.max(0, Math.floor((now - t) / 60_000));
  if (minutes < 1) return "now";
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  // IN THE READER'S ZONE, as every other time on the screen: a formatter with
  // no zone writes in the runtime's, which a suite pinning one does not move.
  if (hours < 24 * 7) return dateTimeFormat(undefined, zone(), WEEKDAY).format(t);
  return dateTimeFormat(undefined, zone(), MONTH_DAY).format(t);
}

const WEEKDAY: DateShape = Object.freeze({ weekday: "short" });
const MONTH_DAY: DateShape = Object.freeze({ month: "short", day: "numeric" });

/**
 * A moment on the READER's own clock, to the minute: the time alone today and
 * the date with it before, as a thread entry or a snooze's return is read.
 */
export function moment(at: string, now: number): string {
  const t = Date.parse(at);
  if (!Number.isFinite(t)) return "";
  const wall = toWall(t);
  return wall.slice(0, 10) === toWall(now).slice(0, 10)
    ? wall.slice(11)
    : `${fmtDateCompact(at, now)} ${wall.slice(11)}`;
}

// ---------------------------------------------------------------------------
// When the list is empty
// ---------------------------------------------------------------------------

/**
 * What the list says when it draws no rows, and null where it must say
 * nothing (nothing has answered, or the answer was a refusal the list draws).
 *
 * DERIVED FROM THE ANSWER, NEVER FROM THE ROW COUNT ALONE. A count of zero is
 * shared by a read nobody answered yet, a refusal, a caught-up reader and a
 * person nothing has ever reached, and a list that inferred one sentence from
 * the count told a person the applier had never written a row for that
 * "everything has been marked read".
 *
 * `seen_through` IS THE EVIDENCE of having read anything: the person's own
 * watermark, written by `mark_inbox` alone and omitted by the engine while it
 * is zero.
 */
export function inboxQuiet(input: {
  /** The Updates answer, or null where there is none (unanswered, or nobody signed in). */
  answer: InboxAnswer | null;
  /** The notices are part of this view: a reader with a record of their own. */
  bound: boolean;
  error: boolean;
  /** Rows drawn in Needs you and Updates. */
  shown: number;
  /** The sentence naming what the conditions are measured over. */
  watched: string;
}): { title: string; hint: string } | null {
  const { answer, bound, error, shown, watched } = input;
  if (error || shown > 0) return null;
  if (!bound) return { title: "Nothing needs a decision", hint: `Checked and clear: ${watched}.` };
  if (!answer) return null;
  return answer.sources.some((s) => (s.seen_through?.seq ?? 0) > 0)
    ? {
        title: "You are caught up",
        hint: "Read updates are under Earlier updates.",
      }
    : {
        title: "Nothing has reached you yet",
        hint: "Mentions, questions and assigned work appear here.",
      };
}

// ---------------------------------------------------------------------------
// Snoozing
// ---------------------------------------------------------------------------

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
 * EACH MOMENT ONCE. On a Sunday the next Monday at nine IS tomorrow at nine,
 * and the menu offered the one moment twice under two names; the later name
 * is dropped, so the reader picks between moments rather than spellings.
 *
 * ONLY THE ONES THE ENGINE WILL TAKE. The write refuses a snooze further ahead
 * than `max_snooze_ahead` (the person answer serves it), so a preset past it
 * is a button that is refused every time it is pressed, and it is not offered.
 * Until the person answer has been read there is no bound, so every preset is
 * offered and the engine remains the judge.
 */
export function snoozePresets(
  now: number,
  zone: string | undefined,
  maxAheadSeconds: number | undefined,
): SnoozePreset[] {
  // ONE CLOCK FOR THE WHOLE MENU, the company's as this runtime reads it, so
  // the day a preset lands on and the instant it names are read on one.
  const tz = companyZone(zone);
  const civil = civilDate(now, tz);
  const tomorrow = addDays(civil, 1);
  // Monday is 1; a reader on a Monday means the one after.
  const weekday = new Date(Date.UTC(civil.y, civil.m - 1, civil.d)).getUTCDay();
  const monday = addDays(civil, (8 - weekday) % 7 || 7);
  const offered: SnoozePreset[] = [
    { key: "hour", label: "In an hour", until: new Date(now + 3_600_000).toISOString() },
    {
      key: "tomorrow",
      label: "Tomorrow, 09:00",
      until: atNine(tomorrow, tz),
      day: dayIn(tomorrow),
    },
    { key: "monday", label: "Next Monday, 09:00", until: atNine(monday, tz), day: dayIn(monday) },
  ];
  const presets = offered.filter((p, i) => offered.findIndex((q) => q.until === p.until) === i);
  if (maxAheadSeconds === undefined || !(maxAheadSeconds > 0)) return presets;
  const limit = now + maxAheadSeconds * 1_000;
  return presets.filter((p) => Date.parse(p.until) <= limit);
}

interface Civil {
  y: number;
  m: number;
  d: number;
}

const CIVIL: DateShape = Object.freeze({ year: "numeric", month: "2-digit", day: "2-digit" });

/** The civil date an instant falls on in `tz`, a zone this runtime reads ([companyZone]). */
function civilDate(at: number, tz: string): Civil {
  const parts = dateTimeFormat("en-CA", tz, CIVIL).formatToParts(at);
  const get = (type: string) => Number(parts.find((p) => p.type === type)?.value);
  return { y: get("year"), m: get("month"), d: get("day") };
}

/** A civil date as a reader names it: "Mon, Sep 28". */
function dayIn(c: Civil): string {
  return dateTimeFormat("en-US", "UTC", DAY).format(Date.UTC(c.y, c.m - 1, c.d));
}

const DAY: DateShape = Object.freeze({ weekday: "short", month: "short", day: "numeric" });

function addDays(c: Civil, days: number): Civil {
  const t = new Date(Date.UTC(c.y, c.m - 1, c.d + days));
  return { y: t.getUTCFullYear(), m: t.getUTCMonth() + 1, d: t.getUTCDate() };
}

/**
 * 09:00 on a civil date in `tz`, a zone this runtime reads ([companyZone]),
 * as an instant. The zone's offset is read at the guess and again at the
 * answer, because the two differ exactly on the day a clock moves.
 */
function atNine(c: Civil, tz: string): string {
  const wall = Date.UTC(c.y, c.m - 1, c.d, 9, 0, 0);
  const guess = zoneOffset(wall, tz);
  return new Date(wall - zoneOffset(wall - guess, tz)).toISOString();
}
