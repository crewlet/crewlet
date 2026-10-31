/**
 * The Inbox's list: Needs you, then Updates, then the two quiet links to what
 * was snoozed and what was read already.
 *
 * AN ANSWER IS SAID, NEVER STRUCK. A row answered here leaves Needs you, its
 * height closing so the rows under it slide up (the kit's group draws it on
 * its way out), and lands at the head of the closed, counted Answered group,
 * whose heading takes a brief wash as it arrives. There it is marked Answered
 * in its head, with what was chosen or written under the question, every word
 * legible. Needs you's count is what is still open, and the group goes with
 * its last row once that row has closed, so the empty state shows in its place
 * rather than a heading over nothing. Answers from before this visit (the
 * last week's, from `answered_decisions`) wait in the same Answered group,
 * each saying who answered and how.
 *
 * EVERY ROW SAYS THE SAME THREE THINGS, whatever it is: WHO (whoever the
 * record names, `iam.ActorFor`: the seat a person or a bound credential writes
 * as, or the login of a credential bound to none), WHAT, in up to three lines
 * and never cut at the edge, and the TASK it is about, as a quiet link of its
 * own under the row that opens the task beside the inbox. A row that drew its
 * kind in a different shape would make a reader learn four lists to read one.
 */

import { memo, useCallback, useState, type ReactNode } from "react";
import {
  Button,
  EmptyState,
  Tag,
  InboxGroup,
  InboxList,
  InboxMore,
  InboxRow as Row,
  InboxToggle,
  Skeleton,
  Text,
} from "@crewlethq/ui";
import { CheckGlyph, MessageSquareGlyph } from "@crewlethq/icons/glyphs";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { AskAnswer } from "~/components/writes.tsx";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { firstLine } from "~/lib/format.ts";
import { TokenScope } from "~/components/InlineToken.tsx";
import { renderTokens } from "~/lib/markdown.ts";
import type { OrgIndex } from "~/lib/seats.ts";
import type { ItemRef } from "~/lib/work.ts";
import {
  answerWords,
  answeredQuestion,
  answeredTexts,
  askQuestion,
  askTexts,
  budgetLine,
  moment,
  noticeText,
  noticeTitle,
  rowItem,
  rowKey,
  rowName,
  rowWho,
  shortWhen,
  type AnswerWords,
  type InboxRow,
  type Who,
} from "./model.ts";
import { TaskLink } from "./TaskLink.tsx";
import { QUESTION_WITHHELD, questionWithheld } from "~/lib/access.ts";
import { runAskTexts } from "~/lib/runs.ts";

/** A row dealt with here: how ("Answered"), and the answer that was given. */
export interface Settled {
  label: string;
  answer?: AskAnswer;
}

/** The closed group of what was answered, here and before this visit. */
export interface AnsweredGroup {
  rows: readonly InboxRow[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /**
   * The key of the row that has just arrived here, which washes the group's
   * heading (and the row, when the group is open). Absent on a first draw.
   */
  arrived?: string | undefined;
}

/** What the list holds beyond its two groups, read only when opened. */
export interface Later {
  /** The rows, or null until the read answers. */
  rows: InboxRow[] | null;
  /** The read stopped at its page with more behind it. */
  more: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function InboxColumn({
  needs,
  updates,
  snoozed,
  snoozedCount,
  earlier,
  onEarlierMore,
  settled,
  answered,
  reader,
  selected,
  onOpen,
  titles,
  index,
  now,
  loading,
  quiet,
  more,
  bound,
  refusal,
  elsewhere = null,
}: {
  needs: readonly InboxRow[];
  updates: readonly InboxRow[];
  snoozed: Later;
  /** How many notices are snoozed, as a count and whether it is a floor; null until read. */
  snoozedCount: { count: number; floor: boolean } | null;
  earlier: Later;
  /** Read the page of earlier updates after the ones shown. */
  onEarlierMore: () => void;
  /**
   * The rows dealt with here: an answer held where it stands until the engine
   * settles it, and a settled one, now under Answered.
   */
  settled: ReadonlyMap<string, Settled>;
  /** What was answered before this visit, in its closed group. */
  answered: AnsweredGroup;
  /** The reader's own record name, so their answers read "You". */
  reader: string;
  selected: string;
  onOpen: (key: string) => void;
  /** Task titles by id and by unclaimed key ([titleOf]), for a row that carries none. */
  titles: ReadonlyMap<string, string>;
  index: OrgIndex;
  now: number;
  /** Nothing has answered yet. */
  loading: boolean;
  /** What to say instead of rows, or null where rows (or a refusal) are drawn. */
  quiet: { title: string; hint: string } | null;
  /** More unread updates lie past the page. */
  more: boolean;
  /** The reader has a record of their own (`ViewerState.owner`), so there are notices. */
  bound: boolean;
  /** A read's refusal, drawn where its rows would be. */
  refusal?: ReactNode;
  /**
   * Where the company runs no Worklet: the line saying where asks and
   * mentions go instead, drawn in place of every tracker pane. Null where it
   * runs it.
   */
  elsewhere?: string | null;
}) {
  const [closing, closed] = useClosing(needs);
  const item = (
    row: InboxRow,
    how: { arrived?: boolean; onClosed?: (key: string) => void } = {},
  ) => (
    <InboxItem
      key={row.key}
      row={row}
      index={index}
      now={now}
      title={titleOf(titles, rowItem(row))}
      settled={settled.get(row.key)}
      reader={reader}
      selected={row.key === selected}
      onOpen={onOpen}
      {...how}
    />
  );
  const snoozedLabel = snoozedCount
    ? `${snoozedCount.count}${snoozedCount.floor ? "+" : ""} snoozed`
    : "";
  return (
    <InboxList label="Inbox" className="inbox-list">
      {refusal}
      {(needs.length > 0 || closing.size > 0) && (
        // THE COUNT IS WHAT IS STILL OPEN: an answer held where it stands
        // until the engine settles it is not waiting on the reader. THE GROUP
        // OUTLIVES ITS LAST ROW only while that row closes, then goes.
        <InboxGroup label="Needs you" count={needs.filter((r) => !settled.has(r.key)).length}>
          {needs.map((row) => item(row, { onClosed: closed }))}
        </InboxGroup>
      )}
      {elsewhere && (
        <Text as="p" variant="caption" className="inbox-note">
          {elsewhere}
        </Text>
      )}
      {updates.length > 0 && (
        <InboxGroup label="Updates">{updates.map((row) => item(row))}</InboxGroup>
      )}
      {answered.rows.length > 0 && (
        // CLOSED UNTIL OPENED, and counted: what a reader decided is there to
        // look back at, never in the way of what still waits on them.
        <InboxGroup
          label="Answered"
          count={answered.rows.length}
          collapsible
          open={answered.open}
          onOpenChange={answered.onOpenChange}
          arrived={answered.arrived}
        >
          {answered.rows.map((row) => item(row, { arrived: row.key === answered.arrived }))}
        </InboxGroup>
      )}
      {loading && <Skeleton variant="text" rows={5} label="Loading the inbox" />}
      {quiet && closing.size === 0 && (
        // AFTER THE LAST ROW HAS CLOSED, never under it as it closes.
        <EmptyState size="compact" title={quiet.title} description={quiet.hint} />
      )}
      {more && (
        <Text as="p" variant="caption" className="inbox-note">
          More unread below. Read these to see them.
        </Text>
      )}
      {bound && (
        // SNOOZED AND EARLIER ARE TWO WORDS AT THE END, never a filter at the
        // top: a filter bar is read every time, and these are opened rarely.
        <InboxMore>
          {snoozedLabel && snoozedCount && snoozedCount.count > 0 && (
            <InboxToggle
              expanded={snoozed.open}
              onClick={() => snoozed.onOpenChange(!snoozed.open)}
            >
              {snoozedLabel}
            </InboxToggle>
          )}
          <InboxToggle expanded={earlier.open} onClick={() => earlier.onOpenChange(!earlier.open)}>
            Earlier updates
          </InboxToggle>
        </InboxMore>
      )}
      {snoozed.open && (
        <LaterRows label="Snoozed" later={snoozed} empty="Nothing is snoozed." item={item} />
      )}
      {earlier.open && (
        <LaterRows
          label="Earlier updates"
          later={earlier}
          empty="Nothing here is read yet."
          item={item}
          onMore={onEarlierMore}
        />
      )}
    </InboxList>
  );
}

/**
 * The keys of rows that have left a group and are still closing in it, and
 * what to call as each one closes. The kit's group keeps drawing a row on its
 * way out until its height has closed, then says so through the row's
 * `onLeft`. Held beside the group rather than with the answer that moved the
 * row, so a row taken away by any means is waited out the same way, and
 * nothing outlives the list if it is put away mid close.
 */
function useClosing(rows: readonly InboxRow[]): [ReadonlySet<string>, (key: string) => void] {
  const [held, setHeld] = useState<{ rows: readonly InboxRow[]; keys: ReadonlySet<string> }>(
    () => ({ rows, keys: new Set() }),
  );
  let now = held;
  if (held.rows !== rows) {
    const present = new Set(rows.map((row) => row.key));
    const keys = new Set(
      [...held.keys, ...held.rows.map((row) => row.key)].filter((key) => !present.has(key)),
    );
    now = { rows, keys };
    setHeld(now);
  }
  const closed = useCallback(
    (key: string) =>
      setHeld((was) => {
        if (!was.keys.has(key)) return was;
        const keys = new Set(was.keys);
        keys.delete(key);
        return { ...was, keys };
      }),
    [],
  );
  return [now.keys, closed];
}

/** The rows a quiet link opened, named for a screen reader by the link's own words. */
function LaterRows({
  label,
  later,
  empty,
  item,
  onMore,
}: {
  label: string;
  later: Later;
  empty: string;
  item: (row: InboxRow) => ReactNode;
  onMore?: () => void;
}) {
  if (later.rows === null) return <Skeleton variant="text" rows={2} label={`Loading ${label}`} />;
  return (
    <>
      {later.rows.length > 0 ? (
        <InboxGroup label={label} hideLabel>
          {later.rows.map((row) => item(row))}
        </InboxGroup>
      ) : (
        !later.more && (
          <Text as="p" variant="caption" className="inbox-note">
            {empty}
          </Text>
        )
      )}
      {later.more && onMore && (
        <InboxMore>
          {/* A READ OF THE NEXT PAGE, not a disclosure: it opens nothing that
              closes again, so it is a plain button rather than a toggle. */}
          <Button size="small" variant="ghost" onClick={onMore}>
            Show more
          </Button>
        </InboxMore>
      )}
    </>
  );
}

/**
 * A row as a sentence says it, from the words it is drawn with: who, then
 * what ("Rui Santos: Ship the release on Friday?"), for an announcement that
 * names the row opening.
 */
export function rowSpoken(row: InboxRow, index: OrgIndex): string {
  return `${rowName(row, rowWho(row, index))}: ${rowWords(row, index).text}`;
}

/** What a row says under the name, and the quieter words after it. */
function rowWords(row: InboxRow, index: OrgIndex): { text: string; excerpt?: string } {
  switch (row.kind) {
    case "decision":
      switch (row.subject.kind) {
        case "ask":
          return { text: askQuestion(row.subject.ask) };
        case "run":
          return {
            // THE QUESTION WHOLE: the row's own clamp cuts a long one.
            text:
              (row.subject.run.question ?? "").trim() ||
              (questionWithheld(row.subject.run) ? QUESTION_WITHHELD : "Parked on a question"),
          };
        case "seat":
          return { text: budgetLine(row.subject.seat.window?.period) };
      }
      break;
    case "condition":
      return { text: row.item.title };
    case "answered":
      return { text: answeredQuestion(row.answered) };
    case "notice": {
      const said = noticeText(row.notice, index);
      return said
        ? { text: noticeTitle(row.notice), excerpt: said }
        : { text: noticeTitle(row.notice) };
    }
  }
  return { text: "" };
}

/** The word after the name, where the row is not a plain question or update. */
function qualifierOf(row: InboxRow): { word: string; danger: boolean } | null {
  if (row.kind !== "decision") return null;
  if (row.subject.kind === "run") return { word: "coding run", danger: false };
  if (row.subject.kind === "seat") return { word: "stopped", danger: true };
  return null;
}

/** When, as the row's end says it: back at a snooze's end, since a condition began. */
function whenOf(row: InboxRow, now: number): string {
  if (row.kind === "notice" && row.notice.snoozed && row.notice.snoozed_until) {
    return `back ${moment(row.notice.snoozed_until, now)}`;
  }
  if (row.kind === "condition") return row.at ? `since ${moment(row.at, now)}` : "";
  return shortWhen(row.at, now);
}

/**
 * One row, drawn again only when what it shows can have changed: its row, its
 * selection, its settlement, the clock its age is read on. MEMOISED because the
 * list around it is redrawn for every live push the Inbox reads, and a row
 * whose props are the ones it was drawn with has nothing new to say
 * ([useStableList] keeps them so).
 */
const InboxItem = memo(function InboxItem({
  row,
  index,
  now,
  title,
  settled,
  reader,
  selected,
  onOpen,
  arrived = false,
  leaving,
  onLeft,
  onClosed,
}: {
  row: InboxRow;
  index: OrgIndex;
  now: number;
  title: string | undefined;
  /** How the row was settled here, and with what, or undefined. */
  settled: Settled | undefined;
  /** The reader's own record name, so their answers read "You". */
  reader: string;
  selected: boolean;
  /** Opens a row by its key: one function for every row, so a row's props
   *  are the same from one draw to the next. */
  onOpen: (key: string) => void;
  /** Just arrived in its group: the kit's brief wash. */
  arrived?: boolean;
  /**
   * On its way out of its group, and what to run once it has closed: the
   * kit's group sets both on the row it keeps drawing, and they reach the
   * row through this one.
   */
  leaving?: boolean | undefined;
  onLeft?: (() => void) | undefined;
  /**
   * Told the row's key once it has closed on its way out: one function for
   * every row, like `onOpen`, so a row's props hold from one draw to the next.
   */
  onClosed?: ((key: string) => void) | undefined;
}) {
  const who = rowWho(row, index);
  const item = rowItem(row);
  const words = rowWords(row, index);
  const answered = row.kind === "answered" ? answerWords(row.answered, reader, index) : null;
  const done = settled !== undefined || answered !== null;
  const qualifier = done ? null : qualifierOf(row);
  const unread = row.kind === "notice" && !row.notice.read && !row.notice.snoozed;
  // WHAT WAS SAID: the answer this visit gave, or the one the engine recorded.
  const said = answered ?? (settled?.answer ? givenWords(settled.answer) : null);
  const texts = rowTexts(row);
  const drawn = (
    <Row
      title={rowName(row, who)}
      qualifier={qualifier?.word}
      qualifierTone={qualifier?.danger ? "danger" : "default"}
      // ANSWERED HERE, the head says so in place of the time; an answer
      // from before this visit keeps the time it was given.
      meta={settled ? undefined : whenOf(row, now) || undefined}
      status={
        settled ? (
          <Tag size="xs" variant="success" appearance="soft" leadingIcon={<CheckGlyph size="sm" />}>
            {settled.label}
          </Tag>
        ) : undefined
      }
      leading={<Badge row={row} who={who} />}
      // A ROW IS A LINK ALREADY, so what it says is drawn with its marks and
      // swatches and never a link of its own (`renderTokens`).
      excerpt={words.excerpt === undefined ? undefined : renderTokens(words.excerpt, "excerpt")}
      outcome={said ? <AnswerLine words={said} /> : undefined}
      outcomeIcon={said ? <AnswerMark words={said} /> : undefined}
      outcomeTone="success"
      unread={unread}
      actionable={row.kind !== "notice" && !done}
      done={done}
      doneLabel={settled?.label ?? (answered?.kind === "resolved" ? "Resolved" : "Answered")}
      selected={selected}
      arrived={arrived}
      leaving={leaving}
      onLeft={() => {
        onLeft?.();
        onClosed?.(row.key);
      }}
      onClick={() => onOpen(row.key)}
      aside={
        item ? (
          <TaskLink item={item} label={rowKey(row)} title={title ?? taskTitleOf(row)} glyph />
        ) : undefined
      }
    >
      {renderTokens(words.text, "words", "text", true)}
    </Row>
  );
  // A ROW'S WORDS ARE ITS QUESTION'S: a bare `#11` in it is placed by what
  // the question's context names, as in the reading pane.
  return texts ? <TokenScope texts={texts}>{drawn}</TokenScope> : drawn;
});

/**
 * Every text of the question a row is about, open or answered, an ask's or a
 * coding run's, or undefined for a row that is not a question.
 */
function rowTexts(row: InboxRow): string[] | undefined {
  if (row.kind === "answered") return answeredTexts(row.answered);
  if (row.kind !== "decision") return undefined;
  if (row.subject.kind === "ask") return askTexts(row.subject.ask);
  return row.subject.kind === "run" ? runAskTexts(row.subject.run) : undefined;
}

/** The answer this visit gave, in the words an answered row says it with. */
function givenWords(answer: AskAnswer): AnswerWords {
  return answer.kind === "choice"
    ? {
        who: "You",
        verb: "chose",
        said: answer.label,
        note: "",
        kind: "choice",
        recommended: answer.recommended,
      }
    : {
        who: "You",
        verb: "replied",
        said: answer.text,
        note: "",
        kind: "reply",
        recommended: false,
      };
}

/**
 * The most of an answer in words a row carries: its first line, cut at a
 * word near ninety characters. The pane and the thread carry it whole.
 */
const SAID_MAX = 90;

function shortSaid(text: string): string {
  const line = firstLine(text);
  if (line.length <= SAID_MAX) return line;
  const cut = line.slice(0, SAID_MAX);
  return `${cut.slice(0, Math.max(cut.lastIndexOf(" "), SAID_MAX / 2)).trimEnd()}…`;
}

/**
 * "You chose **Hold release**", "Maya Ops replied **“Hold it a week.”**",
 * "You chose **Hold release** and wrote **“Tell support first.”**".
 */
function AnswerLine({ words }: { words: AnswerWords }) {
  if (words.kind === "resolved") return <>{`${words.who} ${words.verb}`}</>;
  return (
    <>
      {`${words.who} ${words.verb} `}
      <strong>{words.kind === "reply" ? `“${shortSaid(words.said)}”` : words.said}</strong>
      {words.note && (
        <>
          {" and wrote "}
          <strong>{`“${shortSaid(words.note)}”`}</strong>
        </>
      )}
    </>
  );
}

/** The mark before an answer: a check for a choice or a resolution, a speech mark for words. */
function AnswerMark({ words }: { words: AnswerWords }) {
  return words.kind === "reply" ? <MessageSquareGlyph size="sm" /> : <CheckGlyph size="sm" />;
}

/**
 * A task's title from the one `work_items` read the screen makes for every
 * row's task, BY ID FIRST: that read is by key, and a key two tasks hold
 * answers both, so only the id says which title is this row's. The key is the
 * fallback for a vendor item, which a turn names by key alone, and is listed
 * only for the task that claimed it.
 */
export function titleOf(
  titles: ReadonlyMap<string, string>,
  item: ItemRef | null,
): string | undefined {
  if (!item) return undefined;
  return (
    (item.id ? titles.get(item.id) : undefined) ?? (item.key ? titles.get(item.key) : undefined)
  );
}

/** The title a row carries for its own task, where it carries one. */
export function taskTitleOf(row: InboxRow): string | undefined {
  // A RUN'S QUESTION CARRIES NO TITLE for its task, which the screen reads.
  if (row.kind === "answered") return "run" in row.answered ? undefined : row.answered.ask.title;
  return row.kind === "decision" && row.subject.kind === "ask" ? row.subject.ask.title : undefined;
}

/** The badge a row leads with: its seat, the engine's mark for a condition, or nobody. */
export function Badge({ row, who, size = 24 }: { row: InboxRow; who: Who | null; size?: number }) {
  if (who) {
    return (
      <SeatAvatar name={who.name} kind={who.kind} avatar={who.avatar} size={size} decorative />
    );
  }
  if (row.kind === "condition") {
    return (
      <span className="attention-icon inbox-badge" data-severity={row.item.severity}>
        <NamedGlyph name={row.item.icon} size="sm" />
      </span>
    );
  }
  // A CHANGE THE ENGINE MADE ITSELF names nobody: every person and every
  // credential bound to no seat is named by the record ([rowWho]), so what is
  // left is the engine's own mark.
  return (
    <span className="attention-icon inbox-badge" aria-hidden="true">
      <NamedGlyph name="cpu" size="sm" />
    </span>
  );
}
