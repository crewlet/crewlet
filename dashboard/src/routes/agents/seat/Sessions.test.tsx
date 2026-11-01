/**
 * A seat's executor sessions, drawn as units.
 *
 * Mounted as UNITS rather than through the seat screen, for the reason
 * `Memory.test.tsx` gives: the screen needs an org, a roster, a projection and
 * its queries before the section draws at all, and a rule exercised only
 * through the whole machine is a rule nobody re-runs. The shapes below are the
 * ones the engine sends — `contract/sessions.ts`, which
 * `internal/api/queries`' `TestTheSessionsScreenReadsWhatTheAnswersSend` holds
 * to both answers in both directions.
 */

import { Callout } from "@crewlethq/ui";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import {
  cutWords,
  KIND_LABELS,
  SessionAccount,
  SessionSummary,
  STATE_TITLES,
  TranscriptEntryView,
} from "./Sessions.tsx";
import { Router } from "~/app/router.tsx";
import type { ExecutorSessionRow, TranscriptEntry } from "~/contract/sessions.ts";
import { drawnClasses, isDrawnAs } from "~/testing.tsx";

afterEach(cleanup);

/** The reader's clock: two hours after the turn below began. */
const NOW = Date.parse("2026-10-30T14:00:00Z");

const TURN = "0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c11";

/** A completed turn whose session is kept, with `over` laid on top. */
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
    continuable: true,
    ...over,
  };
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

// WHETHER A LATER LAUNCH COULD TAKE THE SESSION UP is the engine's to say, and
// where it could not, the engine's sentence says why: a reader deciding
// whether the next turn remembers this one needs the reason, not a flag.
test("a session no later launch continues says why", () => {
  const why = "the seat's session budget released this session's object to make room";
  render(
    <Router>
      <SessionAccount row={session({ kept: false, continuable: false, not_continuable: why })} />
    </Router>,
  );
  expect(screen.getByText(`No later launch continues it: ${why}`)).toBeTruthy();
  expect(screen.queryByText(/kept/)).toBeNull();
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
// such and with the bound, where a page's own bound is only where the next
// one starts.
test("a page cut at the read's own bound says the rest cannot be shown", () => {
  expect(cutWords("read", 256 * 1024 * 1024)).toMatch(/cannot be shown.*256 MiB/);
  for (const cut of ["entries", "bytes"] as const) {
    expect(cutWords(cut, 256 * 1024 * 1024)).not.toMatch(/cannot/);
  }
});
