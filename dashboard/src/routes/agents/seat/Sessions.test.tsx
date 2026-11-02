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
  KIND_LABELS,
  readCutWords,
  SessionAccount,
  SessionSummary,
  STATE_TITLES,
  TranscriptEntryView,
} from "./Sessions.tsx";
import { Router } from "~/app/router.tsx";
import type { ExecutorSessionRow, TranscriptEntry } from "~/contract/sessions.ts";
import { drawnClasses, isDrawnAs, overflowing } from "~/testing.tsx";

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
      <SessionAccount row={session({ kept: false, object_bytes: 0 })} />
    </Router>,
  );
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
