/**
 * A seat's executor sessions, drawn as units.
 *
 * Mounted as UNITS rather than through the seat screen, for the reason
 * `Memory.test.tsx` gives: the screen needs an org, a roster, a projection and
 * its queries before the section draws at all, and a rule exercised only
 * through the whole machine is a rule nobody re-runs. What the section does
 * with its questions — the list, a choice, its paging, a session that cannot be
 * read — is mounted on the seat's page, in `routes/agents/Seat.test.tsx`. The
 * shapes below are the ones the engine sends — `contract/sessions.ts`, which
 * `internal/api/queries`' `TestTheSessionsScreenReadsWhatTheAnswersSend` holds
 * to both answers in both directions.
 */

import { Callout } from "@crewlethq/ui";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import {
  blockLabel,
  continuableWords,
  KIND_LABELS,
  readCutWords,
  recapturedWords,
  SessionAccount,
  SessionSummary,
  STATE_TITLES,
  TranscriptEntryView,
  unfollowedWords,
} from "./Sessions.tsx";
import { Router } from "~/app/router.tsx";
import type { ExecutorSessionRow, TranscriptEntry } from "~/contract/sessions.ts";
import { drawnClasses, isDrawnAs, overflowing } from "~/testing.tsx";

afterEach(cleanup);

/** The reader's clock: two hours after the turn below began. */
const NOW = Date.parse("2026-10-30T14:00:00Z");

const TURN = "0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c11";

/** The turn whose session the turn below was handed, a run id as the engine spells one. */
const BASE = "0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c10";

/** The newest turn in the line of the conversation the turn below served: its head. */
const NEWEST = "0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c19";

/** Why an older turn's session is not the one its conversation's next turn continues. */
const OLDER_THAN_HEAD_WHY =
  `the conversation's next turn is handed turn ${NEWEST}'s session, the newest in the ` +
  "conversation's line, and is told how this one ended where its brief has room";

/**
 * A completed turn whose session is kept, its conversation's head — which
 * the conversation's next turn would continue — with `over` laid on top.
 */
function session(over: Partial<ExecutorSessionRow> = {}): ExecutorSessionRow {
  return {
    turn_id: TURN,
    conversation_key: "chatlet:room:eng",
    work_key: "",
    harness: "claude-code",
    harness_version: "2.1.310",
    captured_at: "2026-10-30T12:04:00Z",
    ended_at: "2026-10-30T12:05:00Z",
    ending: { outcome: "completed", decision: "done", delivered: true, delivered_to: "chatlet" },
    ask_excerpt: "## Task\n\nFix the **flaky** cache test.",
    kept: true,
    object_bytes: 41234,
    in_progress: false,
    continued_by: "next_turn",
    continuable: true,
    continues_in: "any_box",
    ...over,
  };
}

/** What the engine tells every reader of a live turn's kept session, as it
 *  sends it (`in_progress_note`, `executordomain.Session.InProgressNote`). */
const IN_PROGRESS_NOTE =
  "This turn has not ended: this is the newest session its runs have kept, captured at " +
  "2026-10-30T12:04:00Z; its run may have gone on past it, and its last call may have no result yet.";

/** What it tells them of a session its turn went on past, beside a newer capture not
 *  kept, as it sends it (`kept_note`, `executordomain.Session.WentOnNote`): a clause of
 *  the engine's, the unkept capture's reason closing it with a full stop of its own. */
const WENT_ON_NOTE =
  "kept up to a session taken while the turn still worked; the turn went on past it, and what " +
  "it did after is not in it; its newer capture (launch 2, capture 1) was not kept: refused: past " +
  "the seat's budget.";

/** A live turn's session, which a relaunch of it decides and which is in progress, with
 *  `over` laid on top. */
function live(over: Partial<ExecutorSessionRow> = {}): ExecutorSessionRow {
  return session({
    ending: null,
    ended_at: null,
    in_progress: true,
    in_progress_note: IN_PROGRESS_NOTE,
    continued_by: "relaunch",
    ...over,
  });
}

/** An entry of `kind`, with `over` laid on top. */
function entry(over: Partial<TranscriptEntry>): TranscriptEntry {
  return { kind: "model", text: "Running the tests first.", bytes: 24, ...over };
}

// WHAT A TURN WAS ASKED IS WHAT TELLS ITS ROW FROM THE NEXT, so it reads as
// words — the brief is a markdown document, and a row that drew its syntax
// raw spent its two lines on `##` and `**` — beside how the turn ended and
// the harness that wrote the session.
test("a session's row reads what it was asked as words, how it ended and its harness", () => {
  const { container } = render(
    <Router>
      <SessionSummary row={session()} now={NOW} />
    </Router>,
  );
  expect(screen.getByText(/Fix the flaky cache test\./)).toBeTruthy();
  expect(container.textContent).not.toMatch(/##|\*\*/);
  expect(screen.getByText("completed")).toBeTruthy();
  expect(screen.getByText("claude-code 2.1.310")).toBeTruthy();
  const when = screen.getByText("1h ago");
  expect(when.getAttribute("title")).toMatch(/2026/);
});

// A TURN WITH NO ENDING HAS NOT ENDED — it runs, or its end was lost and the
// engine has yet to say so — and an outcome drawn for it would be one nobody
// recorded.
test("a turn with no ending reads as not ended, never with an outcome", () => {
  render(
    <Router>
      <SessionSummary row={session({ ending: null, ended_at: null })} now={NOW} />
    </Router>,
  );
  expect(screen.getByText("not ended")).toBeTruthy();
  expect(screen.queryByText("completed")).toBeNull();
});

// A LIVE TURN'S KEPT SESSION READS AS IN PROGRESS, beside "not ended" and in the
// engine's own sentence (`in_progress_note`) — the newest session its runs have
// kept, which its run may have gone on past — so it never reads as an ended
// turn's session; and an ended turn's, or a live turn's that has kept no
// session yet, says nothing of progress beside its tag.
//
// Mutation: drop the in-progress line, and the sentence is not drawn; draw it
// under the line saying when the session was captured, and it is no longer
// beside "not ended"; draw the went-on note (`kept_note`) in its place, and the
// ended turn's note is drawn beside its tag.
test("a live turn's kept session reads as in progress", () => {
  const { unmount } = render(
    <Router>
      <SessionAccount row={live()} />
    </Router>,
  );
  const said = screen.getByText(IN_PROGRESS_NOTE);
  // BESIDE "NOT ENDED": the line under the one its tag is on.
  expect(said.previousElementSibling?.textContent).toMatch(/^not ended/);
  unmount();

  for (const row of [
    session({ kept_note: WENT_ON_NOTE }),
    live({ kept: false, object_bytes: 0, in_progress: false, in_progress_note: undefined }),
  ]) {
    const { container, unmount: done } = render(
      <Router>
        <SessionAccount row={row} />
      </Router>,
    );
    expect(container.textContent).not.toMatch(/has not ended/);
    // NOTHING BESIDE ITS TAG: the line under the one its tag is on is the
    // harness's, and when it was captured.
    const tagged = container.querySelector(".thread-entry > .row");
    expect(tagged?.nextElementSibling?.textContent).toMatch(/^claude-code 2\.1\.310/);
    done();
  }
});

// HOW A TURN ENDED IS TOLD ONLY WHERE THE ENDING CARRIES IT: a reply that went
// out says where, a stop says by whom and why, a failure the engine's own
// account — and a turn nobody stopped has no "Stopped by" line naming nobody.
test("how a turn ended is told only where its ending carries it", () => {
  const { unmount } = render(
    <Router>
      <SessionAccount row={session()} />
    </Router>,
  );
  expect(screen.getByText("Its reply was delivered on chatlet.")).toBeTruthy();
  expect(screen.queryByText(/Stopped by/)).toBeNull();
  unmount();

  render(
    <Router>
      <SessionAccount
        row={session({
          ending: {
            outcome: "stopped",
            stopped_by: "ana.diaz",
            stop_reason: "wrong branch",
            failure: "the run was stopped while it ran",
          },
        })}
      />
    </Router>,
  );
  expect(screen.getByText("stopped")).toBeTruthy();
  expect(screen.getByText("Stopped by ana.diaz: wrong branch")).toBeTruthy();
  expect(screen.getByText("the run was stopped while it ran")).toBeTruthy();
  expect(screen.queryByText(/reply was delivered/)).toBeNull();
});

// A SESSION THE SEAT NO LONGER KEEPS STATES NO SIZE: the budget released its
// object, and a size beside it would read as bytes still held. Why it is gone
// is the transcript's to say, in the engine's own sentence.
test("a session no longer kept states no size kept", () => {
  const { unmount } = render(
    <Router>
      <SessionAccount row={session()} />
    </Router>,
  );
  expect(screen.getByText(/40 KiB kept/)).toBeTruthy();
  unmount();

  render(
    <Router>
      <SessionAccount
        row={session({
          kept: false,
          object_bytes: 0,
          continuable: false,
          continues_in: undefined,
          not_continuable:
            "the seat's session budget released this session's object to make room for newer " +
            "ones; how the turn ended is kept",
        })}
      />
    </Router>,
  );
  // A SIZE KEPT, never the word: the sentence saying whether a launch
  // continues the session may say the session is not kept.
  expect(screen.queryByText(/B kept/)).toBeNull();
});

/** Why a parked turn's kept capture is continued in no other box. */
const OLDER_WHY = "the session was written by Claude Code 2.1.265, older than 2.1.300";

// WHETHER A RELAUNCH CONTINUES A LIVE TURN'S SESSION IS SAID UNDER ITS ACCOUNT,
// as the engine decided it and no more: that one would, in whichever box it
// gets, where that box and the seat's budget can take it — which only the
// launch finds out — or only in the box its run parked in, and why no other;
// and where one would not, the engine's own reason. And AN UNKNOWN IS NEVER
// DRAWN AS A NO: a session the engine could not decide (its turn's run record
// could not be read) may well be continued, so it says that this cannot be said
// now, and why.
test("a session's account says whether a relaunch would continue it", () => {
  const { unmount } = render(
    <Router>
      <SessionAccount row={live()} />
    </Router>,
  );
  const anyBox =
    "A relaunch of this turn would continue this session in whichever box it gets, " +
    "where that box can take it up and the seat's budget can carry it.";
  expect(screen.getByText(anyBox)).toBeTruthy();
  unmount();

  const parked = render(
    <Router>
      <SessionAccount row={live({ continues_in: "parked_box", not_continuable: OLDER_WHY })} />
    </Router>,
  );
  const onlyThere =
    "A relaunch of this turn would continue this session only in the box its run parked " +
    "in, while that box is held and where the seat's budget can carry it; no other box " +
    `would: ${OLDER_WHY}.`;
  expect(screen.getByText(onlyThere)).toBeTruthy();
  expect(screen.queryByText(/would not continue/)).toBeNull();
  parked.unmount();

  const refusing = render(
    <Router>
      <SessionAccount
        row={live({ continuable: false, continues_in: undefined, not_continuable: OLDER_WHY })}
      />
    </Router>,
  );
  expect(
    screen.getByText(`A relaunch would not continue this session: ${OLDER_WHY}.`),
  ).toBeTruthy();
  refusing.unmount();

  render(
    <Router>
      <SessionAccount
        row={live({
          continuable: null,
          continues_in: undefined,
          not_continuable: "the turn's run record could not be read; ask again shortly",
        })}
      />
    </Router>,
  );
  const unknown =
    "Whether a relaunch continues this session cannot be said now: the turn's run record " +
    "could not be read; ask again shortly.";
  expect(screen.getByText(unknown)).toBeTruthy();
  expect(screen.queryByText(/would not continue/)).toBeNull();
  expect(screen.queryByText(/would continue this session/)).toBeNull();
});

// WHETHER AN ENDED TURN'S SESSION IS CONTINUED IS ITS CONVERSATION'S NEXT
// TURN'S: the session of the conversation's head would be continued by that
// turn in whichever box it gets; an older one is not, the engine saying which
// session that turn is handed instead; one whose launch cannot be said is never
// drawn as a no; and a turn that served no conversation names no launch at all.
test("an ended turn's account says whether its conversation's next turn would continue it", () => {
  expect(continuableWords(session())).toBe(
    "The conversation's next turn would continue this session in whichever box it gets, " +
      "where that box can take it up and the seat's budget can carry it.",
  );
  expect(
    continuableWords(
      session({
        continuable: false,
        continues_in: undefined,
        not_continuable: OLDER_THAN_HEAD_WHY,
      }),
    ),
  ).toBe(`The conversation's next turn would not continue this session: ${OLDER_THAN_HEAD_WHY}.`);
  expect(
    continuableWords(
      session({
        continuable: null,
        continues_in: undefined,
        not_continuable: "every member of the seat's executor chain is sitting out its plan",
      }),
    ),
  ).toBe(
    "Whether the conversation's next turn continues this session cannot be said now: every " +
      "member of the seat's executor chain is sitting out its plan.",
  );
  expect(
    continuableWords(
      session({
        conversation_key: "",
        continued_by: undefined,
        continuable: false,
        continues_in: undefined,
        not_continuable: "the turn served no conversation, so no later turn continues its session",
      }),
    ),
  ).toBe(
    "This session is not continued: the turn served no conversation, so no later turn " +
      "continues its session.",
  );
});

// A SCHEDULE'S SESSION IS ITS NEXT FIRE'S TO CONTINUE: a schedule is a
// conversation of each seat that runs it, every fire a turn of it, so an ended
// fire's account names the schedule's next fire wherever another
// conversation's names its next turn — continued, not continued with the
// engine's reason, and not to be said yet.
//
// Mutation: drop CONTINUED_BY's next_fire entry, so a fire's account falls back
// to NO_LAUNCH's words, or give it next_turn's words, and a line reads
// otherwise.
test("an ended fire's account says whether the schedule's next fire would continue it", () => {
  const fire: Partial<ExecutorSessionRow> = {
    conversation_key: `schedule/role/${BASE}/health-check`,
    continued_by: "next_fire",
  };
  const olderFireWhy =
    `the schedule's next fire is handed fire ${NEWEST}'s session, the newest in the ` +
    "schedule's line, and is told how this one ended where its brief has room";
  expect(continuableWords(session(fire))).toBe(
    "The schedule's next fire would continue this session in whichever box it gets, " +
      "where that box can take it up and the seat's budget can carry it.",
  );
  expect(
    continuableWords(
      session({
        ...fire,
        continuable: false,
        continues_in: undefined,
        not_continuable: olderFireWhy,
      }),
    ),
  ).toBe(`The schedule's next fire would not continue this session: ${olderFireWhy}.`);
  expect(
    continuableWords(
      session({
        ...fire,
        continuable: null,
        continues_in: undefined,
        not_continuable: "every member of the seat's executor chain is sitting out its plan",
      }),
    ),
  ).toBe(
    "Whether the schedule's next fire continues this session cannot be said now: every " +
      "member of the seat's executor chain is sitting out its plan.",
  );
});

// THE ENGINE'S REASON IS ONE SENTENCE OF THE SCREEN'S, closed once: a reason
// that brings its own full stop is not closed again, and a row the engine sent
// no reason for still reads as a sentence.
test("a reason is closed once, and a row with none still reads as a sentence", () => {
  const refused = { continuable: false, continues_in: undefined };
  expect(continuableWords(live({ ...refused, not_continuable: "the turn is ending." }))).toBe(
    "A relaunch would not continue this session: the turn is ending.",
  );
  expect(continuableWords(live({ ...refused, not_continuable: undefined }))).toBe(
    "A relaunch would not continue this session.",
  );
  expect(
    continuableWords(
      live({ continuable: null, continues_in: undefined, not_continuable: undefined }),
    ),
  ).toBe("Whether a relaunch continues this session cannot be said now.");
});

// THE SESSION A TURN'S FIRST LAUNCH WAS HANDED IS A LINK THAT OPENS IT HERE: the
// chain a conversation's turns make, followed a turn at a time, the seat's own
// address kept and only the session it opens changed. A turn handed none says
// nothing of one.
test("the session a turn's first launch was handed opens from its account", () => {
  const { unmount } = render(
    <Router>
      <SessionAccount row={live({ base_turn: BASE })} />
    </Router>,
  );
  const link = screen.getByText(BASE);
  expect(link.getAttribute("href")).toContain(`session=${BASE}`);
  expect(link.closest("p")?.textContent).toBe(
    `Its first launch was handed turn ${BASE}'s session to continue.`,
  );
  unmount();

  render(
    <Router>
      <SessionAccount row={live()} />
    </Router>,
  );
  expect(screen.queryByText(/was handed turn/)).toBeNull();
});

// A REPLY THAT DID NOT REACH THE PARTY WAITING IS SAID, naming the surface it was
// owed on — and one the engine cannot vouch for either way is said as not
// known, never as not made, which would send a reader to say it again.
test("a reply that did not reach the party waiting is said, and an unknown one as unknown", () => {
  const { unmount } = render(
    <Router>
      <SessionAccount row={session({ ending: { outcome: "failed", delivered_to: "chatlet" } })} />
    </Router>,
  );
  expect(screen.getByText("Its reply did not reach the party waiting on chatlet.")).toBeTruthy();
  expect(screen.queryByText(/reached anybody is not known/)).toBeNull();
  unmount();

  render(
    <Router>
      <SessionAccount
        row={session({
          ending: { outcome: "completed", delivered_to: "chatlet", delivery_unknown: true },
        })}
      />
    </Router>,
  );
  expect(screen.getByText("Whether its reply reached anybody is not known.")).toBeTruthy();
  expect(screen.queryByText(/did not reach/)).toBeNull();
  expect(screen.queryByText(/reply was delivered/)).toBeNull();
});

// A NEWEST CAPTURE THAT WAS NOT KEPT IS SAID WITH ITS REASON, closed once: the
// row then names none of the turn's sessions, and the reader is told why.
test("a turn whose newest session was not kept says why", () => {
  render(
    <Router>
      <SessionAccount
        row={session({
          kept: false,
          object_bytes: 0,
          continuable: false,
          continues_in: undefined,
          not_continuable:
            "the turn's newest capture of its session (launch 2, capture 1) was not kept: " +
            "refused: past the seat's budget; how the turn ended is kept",
          ending: {
            outcome: "failed",
            unkept: { launch_seq: 2, capture_seq: 1, reason: "refused: past the seat's budget." },
          },
        })}
      />
    </Router>,
  );
  expect(
    screen.getByText("Its newest session was not kept: refused: past the seat's budget."),
  ).toBeTruthy();
});

// A SESSION ITS TURN WENT ON PAST SAYS SO BESIDE WHEN IT WAS CAPTURED, in the
// engine's own words made a sentence of the screen's — its first letter
// capitalised and closed once, the reason bringing its own full stop — so the
// capture is never read as all the turn did; it is never drawn beside its
// outcome as a session in progress is; and the reason its newer capture was
// not kept, which the note names, is said once — never again by the line a row
// that keeps no session says it in.
//
// Mutation: drop the note's line, and nothing says the turn went on; draw it
// beside the outcome, and it is no longer under the line saying when it was
// captured; draw the note as sent, and it reads uncapitalised; close it again,
// and it ends in two full stops; draw the unkept line whatever the note says,
// and the reason is said twice.
test("a session its turn went on past says so beside captured", () => {
  const { container } = render(
    <Router>
      <SessionAccount
        row={session({
          ending: {
            outcome: "stopped",
            went_on: true,
            unkept: { launch_seq: 2, capture_seq: 1, reason: "refused: past the seat's budget." },
          },
          kept_note: WENT_ON_NOTE,
        })}
      />
    </Router>,
  );
  const said = screen.getByText(
    "Kept up to a session taken while the turn still worked; the turn went on past it, and " +
      "what it did after is not in it; its newer capture (launch 2, capture 1) was not kept: " +
      "refused: past the seat's budget.",
  );
  // BESIDE "CAPTURED": the line under the one saying when it was captured.
  expect(said.previousElementSibling?.textContent).toMatch(/ · captured /);
  expect(container.querySelector(".thread-entry > .row")?.nextElementSibling).not.toBe(said);
  // THE REASON ONCE: the note names the capture that was not kept, and why.
  expect(screen.queryByText(/Its newest session was not kept/)).toBeNull();
  expect(container.textContent?.split("past the seat's budget")).toHaveLength(2);
});

// A LIVE SESSION KEPT AGAIN READS IN THE RUN'S WORDS: a capture newer than the
// transcript on screen, and a page the engine refused because it no longer
// follows on, are said of a turn that has not ended as its runs having kept its
// session again — true whichever capture moved it, and never that a run saved
// it, which nothing the row holds says — and of an ended turn as its session
// captured again, as the engine's own refusal says each, in its own spelling
// (`LIVE_KEPT`, held to `executorread.LiveKept` by the engine's contract test).
// And "again" only of a transcript that read a capture: one opened before
// anything of its turn was captured read none, and its first capture since is
// not kept, nor captured, again.
//
// Mutation: word the live banners as an ended row's, or key them on the ending
// rather than on `in_progress`, or say a run saved the session, or say "again"
// of a transcript that read nothing, and a line reads otherwise.
test("a live session kept again reads in the run's words", () => {
  expect(recapturedWords(live(), true)).toBe(
    "This turn has not ended, and its runs have kept its session again since this transcript " +
      "was read.",
  );
  expect(unfollowedWords(live())).toBe(
    "Its later pages no longer follow on from those read: the turn has not ended, and its runs " +
      "have kept its session again since, or a node running another release answered.",
  );
  expect(recapturedWords(session(), true)).toBe(
    "This session was captured again since this transcript was read.",
  );
  expect(unfollowedWords(session())).toBe(
    "Its later pages no longer follow on from those read: it was captured again since, or a " +
      "node running another release answered.",
  );
  // A TRANSCRIPT THAT READ NO CAPTURE is told of its turn's first one since.
  expect(recapturedWords(live(), false)).toBe(
    "This turn has not ended, and its runs have kept its session since this transcript was read.",
  );
  expect(recapturedWords(session(), false)).toBe(
    "This session was captured since this transcript was read.",
  );
  // A LIVE TURN'S ROW THAT HAS KEPT NOTHING IS NOT IN PROGRESS, so a capture
  // after it is any capture.
  expect(
    recapturedWords(
      live({ kept: false, object_bytes: 0, in_progress: false, in_progress_note: undefined }),
      true,
    ),
  ).toBe("This session was captured again since this transcript was read.");
  for (const words of [
    recapturedWords(live(), true),
    recapturedWords(live(), false),
    unfollowedWords(live()),
  ]) {
    expect(words).not.toMatch(/sav(e|ed|ing)/i);
  }
});

// THE ACCOUNT LEADS TO THE TURN, whose phases, review and cost are its own
// page: a session is what the harness wrote, never the whole of the turn.
test("a session's account links the turn it is of", () => {
  render(
    <Router>
      <SessionAccount row={session()} />
    </Router>,
  );
  expect(screen.getByText("turn").getAttribute("href")).toBe(`#/live/turns/${TURN}`);
});

// AN ENTRY SAYS WHO IT IS FROM, and a tool's result names its tool and is
// marked when the harness recorded it as an error — the one entry a reader
// scanning for what went wrong is looking for.
test("an entry is labelled by who it is from, a failed result marked with its tool", () => {
  render(
    <Router>
      <TranscriptEntryView
        entry={entry({
          kind: "tool_result",
          tool: "Bash",
          failed: true,
          text: "FAIL cache_test.go:40",
        })}
      />
    </Router>,
  );
  expect(screen.getByText(KIND_LABELS.tool_result)).toBeTruthy();
  expect(screen.getByText("Bash")).toBeTruthy();
  expect(screen.getByText("failed")).toBeTruthy();
  expect(screen.getByText("FAIL cache_test.go:40")).toBeTruthy();
});

// A SUBAGENT'S ENTRIES FOLLOW THE RUN'S OWN, and each names the subagent whose
// transcript it is — without it, a subagent's brief reads as the engine's.
test("a subagent's entry names the subagent", () => {
  render(
    <Router>
      <TranscriptEntryView
        entry={entry({ kind: "brief", thread: "agent-a1", text: "Look for the race." })}
      />
    </Router>,
  );
  expect(screen.getByText("agent-a1")).toBeTruthy();
  expect(screen.getByText(KIND_LABELS.brief)).toBeTruthy();
});

// AN ENTRY CUT TO ITS BOUND IS AN OPENING, and says how large the whole was,
// so a file read or a build log is never taken for all it said.
test("an entry cut to its bound says it is the opening of a larger whole", () => {
  render(
    <Router>
      <TranscriptEntryView entry={entry({ kind: "tool_result", cut: true, bytes: 2_100_000 })} />
    </Router>,
  );
  expect(screen.getByText("The opening alone: the whole entry is 2.0 MiB.")).toBeTruthy();
});

// A PART OF THE RECORD NOT READ IS A WARNING IN ITS PLACE: drawn as an ordinary
// entry, or left out, the gap would read as continuity.
test("a part of the record not read is drawn as a warning where it was", () => {
  const text = "a line of 40.0 MiB of the harness's record was not read: it is past the 32.0 MiB";
  render(
    <Router>
      <TranscriptEntryView entry={entry({ kind: "unread", text, bytes: text.length })} />
    </Router>,
  );
  // IN THE CAUTION TONE, asked of the design system rather than named by its
  // class, and with no label of a kind beside it — a label would make the gap
  // read as one more step of the run.
  const said = screen.getByText(text);
  const callout = said.closest(`.${drawnClasses(Callout, { children: "" })[0]}`);
  expect(callout).not.toBeNull();
  expect(isDrawnAs(callout!, Callout, { variant: "warning", children: "" }, { children: "" })).toBe(
    true,
  );
  expect(screen.queryByText(KIND_LABELS.unread)).toBeNull();
});

// EVERY KIND AND EVERY STATE HAS ITS OWN WORDS. The tables are keyed by the
// engine's sets, so a kind it adds fails the typecheck; this holds what the
// type cannot — that no two read alike and none is blank.
test("every kind of entry and every unread state has words of its own", () => {
  for (const table of [KIND_LABELS, STATE_TITLES]) {
    const words = Object.values(table);
    expect(words.every((w) => w.trim() !== "")).toBe(true);
    expect(new Set(words).size).toBe(words.length);
  }
});

// A PAGE CUT AT THE READ'S OWN BOUND IS THE END OF WHAT CAN BE SHOWN, said as
// such and with the bound — the one cut that has words, since a page cut at
// its own bound has its Load more instead — and of the whole session where
// the read reached none of it.
test("a page cut at the read's own bound says the rest cannot be shown", () => {
  expect(readCutWords(256 * 1024 * 1024, 3)).toMatch(
    /^The rest of this session cannot be shown.*256 MiB/,
  );
  expect(readCutWords(256 * 1024 * 1024, 0)).toMatch(/^None of this session can be shown.*256 MiB/);
});

// A NOTE A PERSON SENT THE RUN IS LABELLED A NOTE, never a brief: the model
// changing course after it reads as following the note, not as a new task.
test("a note a person sent the run reads as a note", () => {
  render(
    <Router>
      <TranscriptEntryView
        entry={entry({
          kind: "note",
          text: "## A note from ada, sent while this turn was running\n\nTarget main.",
        })}
      />
    </Router>,
  );
  expect(screen.getByText(KIND_LABELS.note)).toBeTruthy();
  expect(screen.queryByText(KIND_LABELS.brief)).toBeNull();
});

// A TOOL'S BLOCK THAT SCROLLS IS A NAMED REGION, AND ONLY THAT. Long enough to
// scroll under its ceiling it takes a tab stop — a scroll container no keyboard
// can reach is content no keyboard can read — named by its tool and which half
// of the call it is; and it leaves select-all to the browser, as the bridge's
// tool log does: a page holds two hundred such blocks, none of them the record
// the screen is about. A short one takes no stop at all.
test("a tool's block that scrolls is a named region and leaves select-all to the browser", () => {
  const long = entry({ kind: "tool_result", tool: "Bash", text: "ok\n".repeat(400) });
  const container = overflowing(
    <Router>
      <TranscriptEntryView entry={long} />
    </Router>,
  );
  const region = container.querySelector('[role="region"][tabindex="0"]');
  expect(region).not.toBeNull();
  expect(region!.getAttribute("aria-label")).toBe("Bash output");
  const event = new KeyboardEvent("keydown", {
    code: "KeyA",
    key: "a",
    ctrlKey: true,
    bubbles: true,
    cancelable: true,
  });
  region!.dispatchEvent(event);
  expect(event.defaultPrevented).toBe(false);
  cleanup();

  const { container: short } = render(
    <Router>
      <TranscriptEntryView entry={entry({ kind: "tool_call", tool: "Bash", text: "{}" })} />
    </Router>,
  );
  expect(short.querySelector('[role="region"]')).toBeNull();
});

// A BLOCK IS NAMED BY ITS TOOL AND ITS HALF OF THE CALL, a failure as one.
test("a tool's block is named by its tool and which half of the call it is", () => {
  expect(blockLabel(entry({ kind: "tool_call", tool: "Read" }))).toBe("Read input");
  expect(blockLabel(entry({ kind: "tool_result", tool: "Read" }))).toBe("Read output");
  expect(blockLabel(entry({ kind: "tool_result", tool: "Read", failed: true }))).toBe("Read error");
  expect(blockLabel(entry({ kind: "tool_result" }))).toBe("Tool output");
});
