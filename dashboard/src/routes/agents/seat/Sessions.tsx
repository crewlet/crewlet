/**
 * A seat's executor sessions, under its memory on the Memory tab.
 *
 * Every turn whose executor captured its harness's session has a row, and so
 * does every turn that read its conversation, from its first launch on: it
 * names the session the harness kept when its run ended — the whole record of
 * what the run read, called and wrote — and how the turn ended. The list is every
 * turn's: how it ended, what it was asked and the harness that wrote it.
 * Choosing one opens its transcript, as the harness recorded it: the brief,
 * the notes a person sent the run, the model's words, every tool call and what
 * answered it, the compaction summaries, and each subagent's own transcript
 * after the run's.
 *
 * # The seat's trail, read through any node
 *
 * Both answers are the seat's TRAIL (`audit.seat.read`, decided as the run's
 * live output is), which is why this section lives in the Memory tab, which
 * asks nothing of a reader the trail is not open to. The rows are the
 * replicated estate's and the transcript is read from the fleet's object
 * store, so any node answers both; nothing here is "held by" a node.
 *
 * # A page at a time, both of them, and a transcript of one capture
 *
 * The list pages by the engine's cursor (`next_cursor`), and so does a
 * transcript, whose cursor names the capture it was read from: a live turn's
 * session moves to a newer archive with every capture, and a page continued in
 * another archive would skip or repeat entries. So A TRANSCRIPT IS OF ONE
 * CAPTURE for as long as it is on screen: when the list shows a newer one, or
 * its first page is answered again for one (a write from this tab asks every
 * question again), or the engine refuses a page because its pages no longer
 * follow on, the reader is told — in its runs' words while its turn has not
 * ended, that they have kept its session again — and offered the transcript
 * again from its first page, never handed the newer capture under the pages
 * they are reading. Each page states why it ends where it does — the page's
 * own bound, with more to read, the most one read takes, with nothing more to
 * show, or that the rest could not be read, and why — and an entry cut to its
 * bound says how large the whole was.
 *
 * The account of how the session's turn ended is the LIST'S ROW, which the
 * list's poll keeps current: a turn's end is recorded with no new capture, so
 * the transcript's own first page, read once, would go on saying it had not
 * ended beside a list saying how it did.
 *
 * # Choosing a session shows it — that session, and never the last one
 *
 * The chosen session is a filter on the address (`session=`), the list is
 * bounded beside the transcript and keeps the chosen row in its own view — the
 * moment the rows are drawn, for a session the address named before the list
 * was read — and on a phone, where the transcript is below the whole list,
 * choosing one scrolls to it and focuses its heading: the row's highlight is
 * otherwise the only feedback, a screen above what it opened. Opening one
 * replaces the history entry, so Back leaves the seat rather than walking
 * every session the reader glanced at. The transcript is drawn fresh for every
 * choice and every re-read, from its loading state: a read of an archive takes
 * a moment, and the previous session's account and entries drawn under the new
 * one's heading meanwhile would read as the new one's. A re-read moves focus to
 * the transcript's heading, the button pressed for it being gone with the pages
 * it stood under. And the transcript is NOT INSIDE THE LIST'S ANSWER: a poll of
 * the list that failed draws the failure in the list's own card, and leaves
 * the transcript, and the pages a reader loaded, where they are.
 */

import { Button, Callout, Card, CodeBlock, EmptyState, Skeleton, Tag } from "@crewlethq/ui";
import { CircleAlertGlyph, FileTextGlyph, SquareTerminalGlyph } from "@crewlethq/icons/glyphs";
import { useLayoutEffect, useRef, useState } from "react";
import { href, useParam, useRoute } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { conversationLabel } from "~/lib/conversationKey.ts";
import { fmtBytes, fmtDateTime, relTime } from "~/lib/format.ts";
import { plainText } from "~/lib/markdownText.ts";
import { reveal } from "~/lib/scroller.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor, type QueryResult } from "~/lib/useQuery.ts";
import type { Seat } from "~/lib/seats.ts";
import { keepInView } from "./keepInView.ts";
import type {
  ExecutorSessionAnswer,
  ExecutorSessionRow,
  ExecutorSessionsAnswer,
  SessionContinuedBy,
  SessionContinuesIn,
  SessionOutcome,
  TranscriptEntry,
  TranscriptEntryKind,
  TranscriptState,
} from "~/contract/sessions.ts";

/**
 * What each kind of entry is called on screen. Keyed by the engine's own set,
 * so a kind the engine adds is a typecheck failure here before it is a raw
 * word on a reader's screen.
 */
export const KIND_LABELS: Record<TranscriptEntryKind, string> = {
  brief: "Brief",
  note: "Note",
  model: "Model",
  tool_call: "Tool call",
  tool_result: "Result",
  summary: "Summary",
  unread: "Not read",
};

/**
 * How a turn's outcome is drawn: in the reviewer's own tone, as a turn's
 * outcome is everywhere else — a failure red, a stop amber, a loss neither.
 */
const OUTCOMES: Record<SessionOutcome, "success" | "danger" | "warning" | "neutral"> = {
  completed: "success",
  failed: "danger",
  stopped: "warning",
  lost: "neutral",
};

/**
 * The heading of a session whose transcript is not shown, by why. The
 * engine's own sentence (`why`) follows it, so each says what happened and the
 * answer says the rest.
 */
export const STATE_TITLES: Record<Exclude<TranscriptState, "read">, string> = {
  none: "No session was kept",
  missing: "The session is gone",
  unreadable: "The session does not open",
  malformed: "The session is not a readable archive",
  unsupported: "This build does not read this session",
};

/**
 * A transcript entry with its place among all of the transcript's, in the
 * capture it was read from.
 *
 * AN ENTRY IS ITS PLACE IN ONE CAPTURE: a live turn's session moves to a newer
 * archive with every capture, and the hundredth entry of one is not the
 * hundredth of the next — so a page read from a later capture never meets the
 * pages held from an earlier one, and the transcript is read again from its
 * start rather than spliced from two archives.
 */
type PlacedEntry = TranscriptEntry & { ordinal: number; capture: string };

const pickSessions = (a: ExecutorSessionsAnswer) => a.sessions;
const sessionId = (row: ExecutorSessionRow) => row.turn_id;
const nextOf = (a: { next_cursor?: string }) => a.next_cursor ?? "";
const pickEntries = (a: ExecutorSessionAnswer): PlacedEntry[] =>
  a.entries.map((entry, i) => ({
    ...entry,
    ordinal: a.from + i,
    capture: a.session.captured_at ?? "",
  }));
const entryId = (entry: PlacedEntry) => `${entry.capture}#${entry.ordinal}`;

/** The open session's heading, which a press [reveal]s and focuses. */
const SESSION_DETAIL_ID = "prof-session-transcript";

/**
 * How often the list is asked again while it is on screen.
 *
 * A MINUTE: a row appears at a turn's first launch where the turn read its
 * conversation, and at its first capture otherwise, and changes with each
 * capture and when the turn ends — minutes apart on a busy seat — and nothing
 * pushes any of it. The transcript is not polled at all, nor asked again on a
 * reconnect: a session's archive changes only with a new capture, which the
 * list shows and the reader is offered, and how its turn ended is the list's
 * row's.
 */
const SESSIONS_POLL_MS = 60_000;

export function Sessions({ seat, now }: { seat: Seat; now: number }) {
  const [turn, setTurn] = useParam("session", "", "filter");
  // SET BY A PRESS, read once the chosen session has rendered: a session named
  // in the address the page was opened on is not a reader's gesture.
  const chose = useRef(false);
  const list = useRef<HTMLDivElement>(null);
  // HOW MANY TIMES THE READER ASKED for the open transcript again from its
  // start: for a newer capture, or after the engine refused a page of it.
  const [rereads, setRereads] = useState(0);
  // SET BY A RE-READ, read once the transcript it drew afresh has rendered.
  const reread = useRef(false);
  const { socket } = useClient();
  const handle = seat.handle;
  const asked = { handle };
  const sessions = usePaged(
    useQuery("executor_sessions", asked, { enabled: handle !== "", pollMs: SESSIONS_POLL_MS }),
    asked,
    (cursor) =>
      socket.query(
        "executor_sessions",
        withFloor("executor_sessions", JSON.stringify({ ...asked, cursor })),
      ),
    pickSessions,
    sessionId,
    nextOf,
  );
  const rows = sessions.items;
  const first = sessions.answers[0];
  const chosen = turn ? rows.find((r) => r.turn_id === turn) : undefined;
  // THE CHOSEN ROW STAYS IN ITS LIST'S VIEW, scrolled within the list alone,
  // ONCE IT IS DRAWN: a session the address named on opening has no row until
  // the list is answered, which is after the first render — so this waits for
  // the row rather than for the choice, and keeps each choice in view once.
  const keptFor = useRef("");
  const drawn = chosen !== undefined;
  useLayoutEffect(() => {
    if (!turn) {
      keptFor.current = "";
      return;
    }
    if (!drawn || keptFor.current === turn) return;
    keptFor.current = turn;
    const box = list.current;
    const row = box?.querySelector<HTMLElement>('[aria-pressed="true"]');
    if (box && row) keepInView(box, row);
  }, [turn, drawn]);
  useLayoutEffect(() => {
    if (!turn || !chose.current) return;
    chose.current = false;
    reveal(document.getElementById(SESSION_DETAIL_ID), { focus: true });
  }, [turn]);
  // A RE-READ PUTS FOCUS ON THE TRANSCRIPT'S HEADING, in view or not: the
  // button it was pressed on is gone with the transcript it stood in, and
  // focus left on nothing is a keyboard's place lost.
  useLayoutEffect(() => {
    if (!reread.current) return;
    reread.current = false;
    const heading = document.getElementById(SESSION_DETAIL_ID);
    reveal(heading);
    heading?.focus({ preventScroll: true });
  }, [rereads]);
  return (
    <section className="col gap-2" aria-labelledby="prof-sessions">
      <h3 id="prof-sessions" className="prof-section-title">
        Sessions
      </h3>
      <div className="split thread-split">
        <Card padding="none">
          <Card.Header
            icon={<SquareTerminalGlyph size="sm" />}
            subtitle={
              first && !first.complete
                ? "The node that answered may be missing sessions it could not read."
                : "What its harness wrote on each turn"
            }
          >
            <Card.Title as="h4">Turns</Card.Title>
          </Card.Header>
          {sessions.loading && !first && (
            <Skeleton variant="text" rows={3} label="Loading this seat's sessions" />
          )}
          {/* THE LIST'S OWN ANSWER, SAID IN ITS OWN CARD: a poll that failed
              replaces the rows with why, and never the transcript beside
              them, whose pages a reader may have spent minutes loading. */}
          <QueryState
            error={sessions.error}
            refusal={sessions.refusal}
            loading={sessions.loading && !first}
          >
            {rows.length ? (
              <div className="list thread-list" ref={list}>
                {rows.map((row) => (
                  <button
                    key={row.turn_id}
                    type="button"
                    className={`thread-entry as-row${row.turn_id === turn ? " selected" : ""}`}
                    aria-pressed={row.turn_id === turn}
                    onClick={() => {
                      chose.current = row.turn_id !== turn;
                      setTurn(row.turn_id === turn ? "" : row.turn_id);
                    }}
                  >
                    <SessionSummary row={row} now={now} />
                  </button>
                ))}
              </div>
            ) : (
              <EmptyState
                size="compact"
                icon={<SquareTerminalGlyph size={32} />}
                title="No sessions recorded"
                description="A turn appears here from its first launch where it read its conversation, and otherwise once its session is first kept or it ends."
              />
            )}
            {(sessions.more || sessions.paging || sessions.pageError) && (
              <footer className="panel-foot">
                {sessions.pageError ? (
                  <QueryState error={sessions.pageError} refusal={null} loading={false} />
                ) : (
                  <Button
                    size="small"
                    variant="secondary"
                    onClick={sessions.next}
                    disabled={sessions.paging}
                  >
                    {sessions.paging ? "Loading…" : "Load older sessions"}
                  </Button>
                )}
              </footer>
            )}
          </QueryState>
        </Card>

        <Card padding="none" className="thread-detail-card">
          <Card.Header
            icon={<FileTextGlyph size="sm" />}
            // WHICH SESSION, named where its transcript is: on a phone the
            // list that says it is a screen above.
            subtitle={turn ? sessionSubtitle(chosen) : undefined}
          >
            <Card.Title as="h4" id={SESSION_DETAIL_ID} tabIndex={-1}>
              Transcript
            </Card.Title>
          </Card.Header>
          {turn ? (
            <Transcript
              // ONE TRANSCRIPT PER CHOICE AND PER RE-READ: keyed so each
              // starts from its loading state with no page held — never the
              // last session's answer under this one's heading, and never a
              // newer capture's pages continued from an older one's.
              key={`${turn}#${rereads}`}
              handle={handle}
              turn={turn}
              row={chosen}
              onReread={() => {
                reread.current = true;
                setRereads((n) => n + 1);
              }}
            />
          ) : (
            <EmptyState
              size="compact"
              icon={<FileTextGlyph size={32} />}
              title="Nothing selected"
              description="Choose a session."
            />
          )}
        </Card>
      </div>
    </section>
  );
}

/** The chosen session's subtitle: its conversation, or that its turn was on none. */
function sessionSubtitle(row: ExecutorSessionRow | undefined): string | undefined {
  if (!row) return undefined;
  return row.conversation_key ? conversationLabel(row.conversation_key) : "No conversation";
}

/**
 * One session as its row in the list draws it: what the turn was asked — its
 * words, the markdown stripped, two lines and the whole on hover — and under
 * it how it ended, when, and the harness that wrote it.
 *
 * A TURN WITH NO ENDING IS STILL RUNNING, or its end was lost and the engine
 * has yet to record that — either way it has not ended, and is not drawn as
 * having an outcome.
 */
export function SessionSummary({ row, now }: { row: ExecutorSessionRow; now: number }) {
  const asked = row.ask_excerpt ? plainText(row.ask_excerpt) : "";
  const at = row.ended_at ?? row.captured_at;
  return (
    <>
      <span className="t-body clamp" title={asked || undefined}>
        {asked || "Nothing it was asked was recorded."}
      </span>
      <span className="row gap-1 wrap t-caption">
        {row.ending ? (
          <Tag size="xs" variant={OUTCOMES[row.ending.outcome]}>
            {row.ending.outcome}
          </Tag>
        ) : (
          <Tag size="xs" appearance="outline">
            not ended
          </Tag>
        )}
        <span>
          {row.harness}
          {row.harness_version ? ` ${row.harness_version}` : ""}
        </span>
        {at && <span title={fmtDateTime(at)}>{relTime(at, now)}</span>}
      </span>
    </>
  );
}

/**
 * A session's transcript, a page at a time, under the account of how its turn
 * ended — one session's and one capture's, for as long as it is mounted.
 *
 * A NEWER CAPTURE IS OFFERED, NEVER SWAPPED IN: when the list, asked again
 * each minute, shows the session captured since the transcript on screen was
 * read — or the first page, asked again because this tab wrote somewhere, is
 * answered for a newer capture — the reader is told and offered it from its
 * start, rather than having the pages they are reading replaced under them,
 * or bridged page by page to a capture they can never meet.
 *
 * HOW THE TURN ENDED IS THE LIST'S ROW where the list holds it: a turn's end
 * is recorded with no new capture, so it reaches the list's poll and never
 * this transcript's first page, read once.
 */
function Transcript({
  handle,
  turn,
  row,
  onReread,
}: {
  handle: string;
  turn: string;
  /** The session's row as the list last read it, undefined where the list
   *  does not hold it (yet). */
  row: ExecutorSessionRow | undefined;
  /** Read the transcript again from its first page. */
  onReread: () => void;
}) {
  const { socket } = useClient();
  const asked = { handle, turn_id: turn };
  const capture = useOneCapture(
    useQuery("executor_session", asked, {
      enabled: handle !== "" && turn !== "",
      refetchOnReconnect: false,
    }),
  );
  const pages = usePaged(
    capture.first,
    asked,
    (cursor) =>
      socket.query(
        "executor_session",
        withFloor("executor_session", JSON.stringify({ ...asked, cursor })),
      ),
    pickEntries,
    entryId,
    nextOf,
  );
  const first = pages.answers[0];
  const last = pages.answers[pages.answers.length - 1];
  const read = first?.session.captured_at ?? null;
  const newer =
    first !== undefined && (capturedLater(row?.captured_at ?? null, read) || capture.newer);
  // THE ROW AS LAST READ: the list's, which its poll keeps current, or the
  // first page's where the list does not hold it — what says whether the turn
  // has not ended, and so in which words a capture since is told.
  const latest = row ?? first?.session;
  return (
    <div className="col gap-2">
      {pages.loading && !first && (
        <Skeleton variant="text" rows={6} label="Loading this session's transcript" />
      )}
      <QueryState error={pages.error} refusal={pages.refusal} loading={pages.loading && !first}>
        {first && latest && (
          <>
            <SessionAccount row={latest} />
            {newer && (
              <div className="row gap-2 wrap">
                <span className="t-caption">{recapturedWords(latest)}</span>
                <Button size="small" variant="secondary" onClick={onReread}>
                  Read the newer capture
                </Button>
              </div>
            )}
            {first.state !== "read" ? (
              <Callout variant="warning" icon={<CircleAlertGlyph size="md" />}>
                <strong>{STATE_TITLES[first.state]}.</strong> {first.why}
              </Callout>
            ) : pages.items.length ? (
              <div className="list">
                {pages.items.map((entry) => (
                  <TranscriptEntryView key={entryId(entry)} entry={entry} />
                ))}
              </div>
            ) : last?.cut === "read" ? null : (
              // NO ENTRIES ONLY WHERE THE READ WENT TO THE END: a read the
              // budget stopped before its first entry says so below, and
              // "nothing recorded" beside it would be false.
              <EmptyState
                size="compact"
                icon={<FileTextGlyph size={32} />}
                title="No entries"
                description="The harness recorded nothing a transcript shows."
              />
            )}
            {last && last !== first && last.state !== "read" && (
              // A LATER PAGE THAT COULD NOT BE READ — the archive damaged past
              // the entries above, its object gone or released since — ends
              // the transcript here and says why, rather than as if the run
              // had ended where the entries do.
              <Callout variant="warning" icon={<CircleAlertGlyph size="md" />}>
                <strong>The rest of this transcript could not be read.</strong>{" "}
                {STATE_TITLES[last.state]}: {last.why}
              </Callout>
            )}
            {last?.cut === "read" && (
              <p className="t-caption">
                {readCutWords(last.bounds.read_bytes, pages.items.length)}
              </p>
            )}
            {(pages.more || pages.paging || pages.pageError) && (
              <footer className="panel-foot">
                {pages.pageError === "bad_params" ? (
                  // THE ENGINE REFUSES A PAGE THAT NO LONGER FOLLOWS ON — the
                  // session captured again since its first page was read, or
                  // a node counting entries otherwise answered — and the only
                  // way on is the transcript from its start, which the reader
                  // is offered rather than a refusal with no way past it.
                  <>
                    <span>{unfollowedWords(latest)}</span>
                    <Button size="small" variant="secondary" onClick={onReread}>
                      Read it again from the start
                    </Button>
                  </>
                ) : pages.pageError ? (
                  <QueryState error={pages.pageError} refusal={null} loading={false} />
                ) : (
                  <Button
                    size="small"
                    variant="secondary"
                    onClick={pages.next}
                    disabled={pages.paging}
                  >
                    {pages.paging ? "Loading…" : "Load more of the transcript"}
                  </Button>
                )}
              </footer>
            )}
          </>
        )}
      </QueryState>
    </div>
  );
}

/**
 * The first page of ONE capture of a session: the first answer's, and any
 * later answer of the same capture — the same archive read again, its row
 * perhaps moved on — but never one of another, which is said instead
 * (`newer`).
 *
 * The first page is asked again on a write from this tab, which asks every
 * question again, and a later answer drawn as the first page would hand the
 * reader another capture's entries with no page held, or set the pages held
 * bridging to a capture whose cursors and entries they can never meet.
 */
function useOneCapture(asked: QueryResult<ExecutorSessionAnswer>): {
  first: QueryResult<ExecutorSessionAnswer>;
  newer: boolean;
} {
  const [held, setHeld] = useState<ExecutorSessionAnswer | null>(null);
  const data = asked.data;
  const same =
    data !== null && (held === null || data.session.captured_at === held.session.captured_at);
  if (same && data !== held) setHeld(data);
  const shown = same ? data : held;
  return {
    first: shown === data ? asked : { ...asked, data: shown },
    newer:
      data !== null &&
      held !== null &&
      capturedLater(data.session.captured_at, held.session.captured_at),
  };
}

/** Whether a capture at `at` came after one at `than` — any capture after none. */
function capturedLater(at: string | null, than: string | null): boolean {
  if (at === null) return false;
  return than === null || Date.parse(at) > Date.parse(than);
}

/**
 * That the session on screen has a newer capture than the transcript read, in
 * the words its row says it in — the engine's, as its refusal of a page that no
 * longer follows on says them (`unfollowedWords`).
 *
 * IN ITS RUNS' WORDS WHILE ITS TURN HAS NOT ENDED (`in_progress`): its runs have
 * kept its session again, which is true whichever capture moved it, a later
 * launch's or one taken as its run works — and never that a run saved it,
 * which nothing the row holds says. Of an ended turn, that it was captured
 * again, its last capture landing before its end.
 */
export function recapturedWords(row: ExecutorSessionRow): string {
  return row.in_progress
    ? "This turn has not ended, and its runs have kept its session again since this transcript was read."
    : "This session was captured again since this transcript was read.";
}

/**
 * Why the engine refused a later page of the transcript on screen: its pages
 * no longer follow on from those read, because the session was kept again since
 * — in its runs' words while its turn has not ended, as `recapturedWords` says
 * it — or a node running another release answered, which the refusal does not
 * tell apart from it.
 */
export function unfollowedWords(row: ExecutorSessionRow): string {
  const since = row.in_progress
    ? "the turn has not ended, and its runs have kept its session again since"
    : "it was captured again since";
  return `Its later pages no longer follow on from those read: ${since}, or a node running another release answered.`;
}

/**
 * The engine's note on the session a row keeps (`kept_note`) as one of the
 * screen's sentences: its first letter capitalised and closed once. The note is
 * a clause of the engine's, which it closes or not, and its reason quotes
 * whoever refused a capture, which may bring a full stop of its own.
 */
function noteSentence(note: string): string {
  const body = note.trim().replace(/\.\s*$/, "");
  return body ? `${body.charAt(0).toUpperCase()}${body.slice(1)}.` : "";
}

/**
 * Why a page of a transcript ends where no later page can go further, in a
 * reader's words: the archive is past what one read takes (`read_bytes` of it),
 * or a part of it too large to read left the rest unreadable — said of the
 * whole session where nothing of it was read. A page that ends at its own
 * bound says nothing of the kind — its Load more is the way on.
 */
export function readCutWords(readBytes: number, shown: number): string {
  const why =
    `a read takes at most ${fmtBytes(readBytes)} of its archive, ` +
    `or a part of it was too large to read.`;
  return shown === 0
    ? `None of this session can be shown: ${why}`
    : `The rest of this session cannot be shown: ${why}`;
}

/** The three leads of a sentence saying whether a launch continues a session. */
interface ContinuedWords {
  /** It would, before where. */
  would: string;
  /** It would not. */
  not: string;
  /** That cannot be said now. */
  unknown: string;
}

/**
 * Which launch would continue a session, as the sentence that says whether it
 * would leads. Keyed by the engine's own set, so a launch the engine adds is a
 * typecheck failure here before it is a raw word on a reader's screen.
 */
const CONTINUED_BY: Record<SessionContinuedBy, ContinuedWords> = {
  relaunch: {
    would: "A relaunch of this turn would continue this session",
    not: "A relaunch would not continue this session",
    unknown: "Whether a relaunch continues this session cannot be said now",
  },
  next_turn: {
    would: "The conversation's next turn would continue this session",
    not: "The conversation's next turn would not continue this session",
    unknown: "Whether the conversation's next turn continues this session cannot be said now",
  },
  next_fire: {
    would: "The schedule's next fire would continue this session",
    not: "The schedule's next fire would not continue this session",
    unknown: "Whether the schedule's next fire continues this session cannot be said now",
  },
};

/**
 * The leads where no launch is named: a turn that served no conversation,
 * whose session no launch continues once its run is over — or a launch a newer
 * engine names, said without a guess at who it is.
 */
const NO_LAUNCH: ContinuedWords = {
  would: "This session would be continued",
  not: "This session is not continued",
  unknown: "Whether this session is continued cannot be said now",
};

/**
 * Where a launch would continue a session, after the lead that says which.
 * Keyed by the engine's own set, so a place the engine adds is a typecheck
 * failure here before it is a raw word on a reader's screen.
 */
const CONTINUES_IN: Record<SessionContinuesIn, string> = {
  any_box:
    "in whichever box it gets, where that box can take it up and the seat's budget can carry it",
  parked_box:
    "only in the box its run parked in, while that box is held and where the seat's budget can " +
    "carry it",
};

/**
 * Whether a launch would continue the session, as a sentence: which launch
 * (`continued_by` — a relaunch of its turn, its conversation's next turn, or
 * its schedule's next fire), the engine's decision (`continuable`) and where
 * (`continues_in`), with its own reason (`not_continuable`) where it would not,
 * where a relaunch would only in the box the turn's run parked in, and where it
 * cannot be said now.
 *
 * NO MORE THAN THE ENGINE DECIDED: a launch WOULD continue the session where
 * the box it gets can take it up and the seat's budget can carry it, which only
 * the launch finds out — so the sentence says so rather than that it does.
 *
 * AN UNKNOWN IS NEVER DRAWN AS A NO: a session the engine could not decide may
 * well be continued, and "would not" would send a reader after a fresh start
 * that need not happen.
 */
export function continuableWords(row: ExecutorSessionRow): string {
  const why = (row.not_continuable ?? "").replace(/\.\s*$/, "");
  const who = (row.continued_by && CONTINUED_BY[row.continued_by]) ?? NO_LAUNCH;
  if (row.continuable) {
    const where = CONTINUES_IN[row.continues_in ?? "any_box"] ?? CONTINUES_IN.any_box;
    const lead = `${who.would} ${where}`;
    if (row.continues_in === "parked_box") {
      return why ? `${lead}; no other box would: ${why}.` : `${lead}.`;
    }
    return `${lead}.`;
  }
  const lead = row.continuable === null ? who.unknown : who.not;
  return why ? `${lead}: ${why}.` : `${lead}.`;
}

/**
 * How the session's turn ended, as the engine recorded it — what its
 * harness's own transcript cannot know — beside what is kept of it, the
 * session its first launch was handed, and whether a launch would continue it
 * (`continuableWords`).
 *
 * EVERY FACT IS DRAWN ONLY WHERE THE ENDING CARRIES IT: an ending says what
 * the way its turn ended established, and a blank "Stopped by" on a turn
 * nobody stopped would read as a stop by nobody. A reply that did not reach
 * the party waiting is said only where the ending names whom it was owed to
 * and knows it did not go: a delivery the engine cannot vouch for either way
 * is said as not known, never as not made.
 *
 * WHAT THE ENGINE TELLS EVERY READER OF THE SESSION IT KEEPS is its own
 * sentence, never one of the screen's, each where it reads: a session in
 * progress (`in_progress_note`) beside "not ended", so a live turn's kept
 * session never reads as an ended turn's; and an ended turn's kept up to a
 * session it went on past (`kept_note`) under when it was captured, so the
 * capture is never read as all the turn did. That note names the newer capture
 * that was not kept and why, so the line saying the newest session was not
 * kept is drawn only where no note says it: the reason once.
 */
export function SessionAccount({ row }: { row: ExecutorSessionRow }) {
  const ending = row.ending;
  const unkept = ending?.unkept ? ending.unkept.reason.replace(/\.\s*$/, "") : "";
  const note = row.kept_note ? noteSentence(row.kept_note) : "";
  return (
    <div className="thread-entry">
      <div className="row gap-1 wrap">
        {ending ? (
          <Tag variant={OUTCOMES[ending.outcome]}>{ending.outcome}</Tag>
        ) : (
          <Tag appearance="outline">not ended</Tag>
        )}
        {ending?.decision && <Tag appearance="outline">{ending.decision}</Tag>}
        <span className="spacer" />
        <a className="t-link mono t-caption" href={href(["live", "turns", row.turn_id])}>
          turn
        </a>
      </div>
      {row.in_progress_note && <p className="t-caption">{row.in_progress_note}</p>}
      <p className="t-caption">
        {row.harness}
        {row.harness_version ? ` ${row.harness_version}` : ""}
        {row.captured_at ? ` · captured ${fmtDateTime(row.captured_at)}` : ""}
        {row.ended_at ? ` · ended ${fmtDateTime(row.ended_at)}` : ""}
        {row.kept ? ` · ${fmtBytes(row.object_bytes)} kept` : ""}
      </p>
      {note && <p className="t-caption">{note}</p>}
      {row.base_turn && <HandedSession turn={row.base_turn} />}
      <p className="t-caption">{continuableWords(row)}</p>
      {ending?.delivered && (
        <p className="t-caption">
          Its reply was delivered{ending.delivered_to ? ` on ${ending.delivered_to}` : ""}.
        </p>
      )}
      {ending && !ending.delivered && ending.delivery_unknown && (
        <p className="t-caption">Whether its reply reached anybody is not known.</p>
      )}
      {ending && !ending.delivered && !ending.delivery_unknown && ending.delivered_to && (
        <p className="t-caption">
          Its reply did not reach the party waiting on {ending.delivered_to}.
        </p>
      )}
      {ending?.unkept && !note && (
        <p className="t-caption">
          {unkept
            ? `Its newest session was not kept: ${unkept}.`
            : "Its newest session was not kept."}
        </p>
      )}
      {ending?.answer && <p className="t-caption">It answered a colleague: {ending.answer}.</p>}
      {ending?.completed_work && <p className="t-body">{ending.completed_work}</p>}
      {ending?.blocked_on && <p className="t-caption">Blocked on: {ending.blocked_on}</p>}
      {ending?.stopped_by && (
        <p className="t-caption">
          Stopped by {ending.stopped_by}
          {ending.stop_reason ? `: ${ending.stop_reason}` : "."}
        </p>
      )}
      {ending?.failure && <p className="t-caption">{ending.failure}</p>}
    </div>
  );
}

/**
 * The session a turn's first launch was handed to continue — its
 * conversation's head as the turn's dispatch read it — as a link that opens that
 * session here, where this seat still keeps it: the chain a conversation's
 * turns make, followed a turn at a time. The engine says whether the session is
 * still kept once it is opened; an absorbed one is gone with the turn that was
 * handed it.
 */
function HandedSession({ turn }: { turn: string }) {
  const route = useRoute();
  const opens = href(route.path, { ...Object.fromEntries(route.query), session: turn });
  return (
    <p className="t-caption">
      Its first launch was handed turn{" "}
      <a className="t-link mono prose-link" href={opens}>
        {turn}
      </a>
      's session to continue.
    </p>
  );
}

/**
 * One entry of a transcript: who it is from, the subagent whose transcript
 * holds it, the tool, a failure marked, when — and its text, an opening marked
 * as one where the entry was cut to its bound.
 *
 * A CALL'S INPUT AND A TOOL'S OUTPUT ARE BLOCKS, scrolling within the ceiling
 * every block in the shell's scroller takes, because their whitespace is the
 * content; a brief, a note, the model's words and a summary are prose. A block
 * takes a tab stop only when it scrolls (`focusWhenScrollable`), named by its
 * tool — never `selectable`, which would put a stop before each of two hundred
 * short blocks a page holds and take select-all from the page while one has
 * focus, for a block that is not the record the screen is about. A PART OF THE
 * RECORD NOT READ is drawn as the warning it is, in its place, so a gap never
 * reads as continuity.
 */
export function TranscriptEntryView({ entry }: { entry: TranscriptEntry }) {
  if (entry.kind === "unread") {
    return (
      <div className="thread-entry">
        <Callout variant="warning" icon={<CircleAlertGlyph size="md" />}>
          {entry.text}
        </Callout>
      </div>
    );
  }
  const block = entry.kind === "tool_call" || entry.kind === "tool_result";
  return (
    <div className="thread-entry">
      <div className="row gap-1 wrap">
        <Tag size="xs" appearance="outline">
          {KIND_LABELS[entry.kind]}
        </Tag>
        {entry.thread && (
          <Tag size="xs" appearance="soft" title="A subagent's own transcript">
            {entry.thread}
          </Tag>
        )}
        {entry.tool && <span className="mono t-caption">{entry.tool}</span>}
        {entry.failed && (
          <Tag size="xs" variant="danger">
            failed
          </Tag>
        )}
        <span className="spacer" />
        {entry.at && <span className="t-caption">{fmtDateTime(entry.at)}</span>}
      </div>
      {block ? (
        <CodeBlock plain focusWhenScrollable label={blockLabel(entry)} code={entry.text} />
      ) : (
        <p className="prose">{entry.text}</p>
      )}
      {entry.cut && (
        <p className="t-caption">The opening alone: the whole entry is {fmtBytes(entry.bytes)}.</p>
      )}
    </div>
  );
}

/**
 * What a tool's block is called where it takes focus: the tool, and which
 * half of its call — a region named only "Input" or "Output", one of two
 * hundred, tells a reader moving by landmark nothing.
 */
export function blockLabel(entry: TranscriptEntry): string {
  const tool = entry.tool || "Tool";
  if (entry.kind === "tool_call") return `${tool} input`;
  return `${tool} ${entry.failed ? "error" : "output"}`;
}
