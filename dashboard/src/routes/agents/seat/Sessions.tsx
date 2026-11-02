/**
 * A seat's executor sessions, beside its Conversations under Memory.
 *
 * Every turn whose executor captured its harness's session has a row: it names
 * the session the harness kept when its run ended — the whole record of what
 * the run read, called and wrote — and how the turn ended. The list is every
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
 * # A page at a time, both of them
 *
 * The list pages by the engine's cursor (`next_cursor`), and so does a
 * transcript, whose cursor names the capture it was read from: a live turn's
 * session moves to a newer archive with every capture, and a page continued in
 * another archive would skip or repeat entries. So when the list shows a newer
 * capture than the transcript on screen, or the engine refuses a page because
 * the session moved since, the reader is told and offered the transcript again
 * from its first page. Each page states why it ends where it does — the page's
 * own bound, with more to read, or the most one read takes, with nothing more
 * to show — and an entry cut to its bound says how large the whole was.
 *
 * # Choosing a session shows it — that session, and never the last one
 *
 * As Conversations does: the chosen session is a filter on the address
 * (`session=`), the list is bounded beside the transcript and keeps the chosen
 * row in its own view, and on a phone, where the transcript is below the
 * whole list, choosing one scrolls to it and focuses its heading. The
 * transcript is drawn fresh for every choice, from its loading state: a read
 * of an archive takes a moment, and the previous session's account and entries
 * drawn under the new one's heading meanwhile would read as the new one's.
 */

import { Button, Callout, Card, CodeBlock, EmptyState, Skeleton, Tag } from "@crewlethq/ui";
import { CircleAlertGlyph, FileTextGlyph, SquareTerminalGlyph } from "@crewlethq/icons/glyphs";
import { useLayoutEffect, useRef, useState } from "react";
import { href, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { conversationLabel, fmtBytes, fmtDateTime, relTime } from "~/lib/format.ts";
import { plainText } from "~/lib/markdownText.ts";
import { keepInView, reveal } from "~/lib/scroller.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import type { Seat } from "~/lib/seats.ts";
import type {
  ExecutorSessionAnswer,
  ExecutorSessionRow,
  ExecutorSessionsAnswer,
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
 * A MINUTE: a row appears when a turn's run is collected and changes when the
 * turn ends — minutes apart on a busy seat — and nothing pushes either. The
 * transcript is not polled at all: a session's archive changes only with a new
 * capture, which the list shows, and a newer capture reads the transcript
 * again from its start.
 */
const SESSIONS_POLL_MS = 60_000;

export function Sessions({ seat, now }: { seat: Seat; now: number }) {
  const [turn, setTurn] = useParam("session", "", "filter");
  // SET BY A PRESS, read once the chosen session has rendered: a session named
  // in the address the page was opened on is not a reader's gesture.
  const chose = useRef(false);
  const list = useRef<HTMLDivElement>(null);
  // HOW MANY TIMES THE READER ASKED for the open transcript again from its
  // start: for a newer capture the list shows, or after the engine refused a
  // page of it as one of an older capture.
  const [rereads, setRereads] = useState(0);
  useLayoutEffect(() => {
    if (!turn) return;
    const box = list.current;
    const row = box?.querySelector<HTMLElement>('[aria-pressed="true"]');
    if (box && row) keepInView(box, row);
    if (!chose.current) return;
    chose.current = false;
    reveal(document.getElementById(SESSION_DETAIL_ID), { focus: true });
  }, [turn]);
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
  return (
    <section className="col gap-2" aria-labelledby="prof-sessions">
      <h3 id="prof-sessions" className="prof-section-title">
        Sessions
      </h3>
      <QueryState
        error={sessions.error}
        refusal={sessions.refusal}
        loading={sessions.loading && !first}
      >
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
                description="A turn's session appears here once its run is collected."
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
                listed={chosen?.captured_at ?? null}
                onReread={() => setRereads((n) => n + 1)}
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
      </QueryState>
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
 * read, the reader is told and offered it from its start — a page continued
 * from the older capture would be refused — rather than having the pages they
 * are reading replaced under them.
 */
function Transcript({
  handle,
  turn,
  listed,
  onReread,
}: {
  handle: string;
  turn: string;
  /** When the list says the session was last captured, null where it has not
   *  listed the session (yet). */
  listed: string | null;
  /** Read the transcript again from its first page. */
  onReread: () => void;
}) {
  const { socket } = useClient();
  const asked = { handle, turn_id: turn };
  const pages = usePaged(
    useQuery("executor_session", asked, { enabled: handle !== "" && turn !== "" }),
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
  const read = first?.session.captured_at;
  const newer = Boolean(listed && read && Date.parse(listed) > Date.parse(read));
  return (
    <div className="col gap-2">
      {pages.loading && !first && (
        <Skeleton variant="text" rows={6} label="Loading this session's transcript" />
      )}
      <QueryState error={pages.error} refusal={pages.refusal} loading={pages.loading && !first}>
        {first && (
          <>
            <SessionAccount row={first.session} />
            {newer && (
              <div className="row gap-2 wrap">
                <span className="t-caption">
                  This session was captured again since this transcript was read.
                </span>
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
            ) : (
              <EmptyState
                size="compact"
                icon={<FileTextGlyph size={32} />}
                title="No entries"
                description="The harness recorded nothing a transcript shows."
              />
            )}
            {last?.cut === "read" && (
              <p className="t-caption">{readCutWords(last.bounds.read_bytes)}</p>
            )}
            {(pages.more || pages.paging || pages.pageError) && (
              <footer className="panel-foot">
                {pages.pageError === "bad_params" ? (
                  // THE ENGINE REFUSES A PAGE OF A CAPTURE IT NO LONGER HOLDS
                  // — the session was captured again since its first page was
                  // read — and the only way on is the transcript from its
                  // start, which the reader is offered rather than a refusal
                  // with no way past it.
                  <>
                    <span>
                      This session was captured again since its first page was read, so its later
                      pages no longer follow on.
                    </span>
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
 * Why a page of a transcript ends where no later page can go further, in a
 * reader's words: the archive is past what one read takes (`read_bytes` of it),
 * or a part of it too large to read left the rest unreadable. A page that ends
 * at its own bound says nothing of the kind — its Load more is the way on.
 */
export function readCutWords(readBytes: number): string {
  return (
    `The rest of this session cannot be shown: a read takes at most ` +
    `${fmtBytes(readBytes)} of its archive, or a part of it was too large to read.`
  );
}

/**
 * How the session's turn ended, as the engine recorded it — what its
 * harness's own transcript cannot know — beside what is kept of it.
 *
 * EVERY FACT IS DRAWN ONLY WHERE THE ENDING CARRIES IT: an ending says what
 * the way its turn ended established, and a blank "Stopped by" on a turn
 * nobody stopped would read as a stop by nobody.
 */
export function SessionAccount({ row }: { row: ExecutorSessionRow }) {
  const ending = row.ending;
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
      <p className="t-caption">
        {row.harness}
        {row.harness_version ? ` ${row.harness_version}` : ""}
        {row.captured_at ? ` · captured ${fmtDateTime(row.captured_at)}` : ""}
        {row.ended_at ? ` · ended ${fmtDateTime(row.ended_at)}` : ""}
        {row.kept ? ` · ${fmtBytes(row.object_bytes)} kept` : ""}
      </p>
      {ending?.delivered && (
        <p className="t-caption">
          Its reply was delivered{ending.delivered_to ? ` on ${ending.delivered_to}` : ""}.
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
        <p className="t-body session-prose">{entry.text}</p>
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
