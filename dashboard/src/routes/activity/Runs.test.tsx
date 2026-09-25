/**
 * What a bridged run's tool log claims.
 *
 * This is the one tool log with nowhere else to live — a bridged run's calls
 * are made by a process outside the engine, minutes apart and possibly across
 * a restart — so what the screen says about it is the whole record a reviewer
 * gets. Two claims have to be exactly right: that a run made no calls, and
 * that the log in front of them is complete.
 */

import { cleanup, fireEvent, render as rtlRender, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { ReactElement } from "react";

import { Router } from "~/app/router.tsx";
import { overflowing } from "~/testing.tsx";
import { BridgeLog } from "./Runs.tsx";
import type { SandboxRun } from "~/protocol/index.ts";

afterEach(cleanup);

function render(ui: ReactElement) {
  return rtlRender(<Router>{ui}</Router>);
}

function run(extra: Partial<SandboxRun> = {}): SandboxRun {
  return {
    turn_id: "turn-1",
    agent_handle: "ada",
    role: "SWE",
    status: "running",
    coding_agent: "claude",
    placement: "direct",
    task_description: "Ship it",
    question: "",
    audience: "",
    audience_handles: [],
    audience_fallback: false,
    branch: "",
    trace_id: "",
    owner: "node-0",
    box_exists: true,
    paused_at: "",
    pause_ttl_seconds: 0,
    started_at: "2026-06-15T12:00:00Z",
    updated_at: "2026-06-15T12:05:00Z",
    answerable_in_chat: false,
    ...extra,
  };
}

test("an ordinary run has no tool-call panel at all", () => {
  // ABSENT IS NOT EMPTY. A coding run that is not bridged made no calls
  // THROUGH A BRIDGE, which is not the same as a bridged one that made none —
  // and "it called nothing" is exactly the shape the delivery check reads as
  // a turn that did not act.
  render(<BridgeLog run={run()} />);
  expect(screen.queryByText("Tool calls")).toBeNull();
});

test("a bridged run's calls are listed, and a failure is marked", () => {
  render(
    <BridgeLog
      run={run({
        bridge_calls: [
          {
            name: "read_file",
            args: '{"path":"a.go"}',
            output: "package a",
            at: "2026-06-15T12:01:00Z",
          },
          {
            name: "write_file",
            args: '{"path":"b.go"}',
            output: "denied",
            failed: true,
            at: "2026-06-15T12:02:00Z",
          },
        ],
      })}
    />,
  );
  expect(screen.getByText("Tool calls")).toBeTruthy();
  expect(screen.getByText("read_file")).toBeTruthy();
  expect(screen.getByText("write_file")).toBeTruthy();
  expect(screen.getByText("failed")).toBeTruthy();
  expect(screen.getByText("1 failure")).toBeTruthy();
});

test("an elided middle is said out loud, never left to be inferred", () => {
  // The engine bounds the log at 200 and drops the MIDDLE rather than the
  // start — how a run began and how it ended are what explain it. A log that
  // silently skips is a log that lies about what the run did.
  render(
    <BridgeLog
      run={run({
        bridge_calls: [{ name: "read_file", at: "2026-06-15T12:01:00Z" }],
        bridge_calls_elided: 47,
      })}
    />,
  );
  expect(screen.getByText(/47 calls from the middle of this log were dropped/)).toBeTruthy();
});

test("a bridged run that genuinely made no calls still shows nothing", () => {
  // The panel is about what the run DID. A bridged run with an empty log and
  // nothing elided has the same thing to say as an unbridged one, and saying
  // it twice in two different ways is how a reader learns to distrust both.
  render(<BridgeLog run={run({ bridge_calls: [], bridge_calls_elided: 0 })} />);
  expect(screen.queryByText("Tool calls")).toBeNull();
});

// THE ROW MOUNTS ITS BLOCKS ON OPENING, so a case about what a block does has
// to open it — and it has to open it while `overflowing`'s measurement fake is
// still installed, or the block measures a box jsdom reports as zero and the
// branch under test never runs. That is what the callback is for.
function openFirstCall(container: HTMLElement): void {
  fireEvent.click(container.querySelector("button")!);
}

function longLog() {
  return run({
    bridge_calls: [
      {
        name: "read_file",
        args: '{"path":"a.go"}',
        output: "package a\n".repeat(400),
        at: "2026-06-15T12:01:00Z",
      },
    ],
  });
}

// A BLOCK THAT SCROLLS IS REACHABLE. Under a ceiling the block is
// `overflow: auto`, and in Chrome and Safari a scroll container with no
// tabindex cannot be scrolled by keyboard at all — so a bridged run's output,
// which is the longest machine text this dashboard shows, was readable by
// pointer and by nothing else.
test("a tool block long enough to scroll is a named, focusable region", () => {
  const container = overflowing(
    <Router>
      <BridgeLog run={longLog()} />
    </Router>,
    "height",
    openFirstCall,
  );
  const region = container.querySelector('[role="region"][tabindex="0"]');
  expect(region).not.toBeNull();
  expect(region?.getAttribute("aria-label")).toContain("read_file");
});

// …AND IT STILL DOES NOT SWALLOW ⌘A. `focusWhenScrollable` and `selectable`
// are two different doors onto the same tab stop, and this screen goes
// through the first: a bridged tool's arguments are not the record the screen
// is about, so select-all keeps meaning what it means everywhere else.
// Switching this call to `selectable` would keep every assertion above green
// and silently change what the chord does on a screen with two hundred blocks
// on it, which is why the refusal is asserted rather than assumed.
test("a scrolling tool block leaves select-all to the browser", () => {
  const container = overflowing(
    <Router>
      <BridgeLog run={longLog()} />
    </Router>,
    "height",
    openFirstCall,
  );
  const region = container.querySelector('[role="region"][tabindex="0"]')!;
  const event = new KeyboardEvent("keydown", {
    code: "KeyA",
    key: "a",
    ctrlKey: true,
    bubbles: true,
    cancelable: true,
  });
  region.dispatchEvent(event);
  expect(event.defaultPrevented).toBe(false);
});

test("a bridged call's arguments are shown as the JSON document they are", () => {
  // The engine writes them with a plain `json.Marshal`, so what arrives is the
  // whole call on one line. Indented rather than re-encoded — lib/jsontext.ts
  // has the reason — so an id too wide for a double survives the trip.
  const container = overflowing(
    <Router>
      <BridgeLog
        run={run({
          bridge_calls: [
            {
              name: "read_file",
              args: '{"path":"a.go","id":9007199254740993}',
              output: "package a",
              at: "2026-06-15T12:01:00Z",
            },
          ],
        })}
      />
    </Router>,
    "height",
    openFirstCall,
  );
  const block = container.querySelector('[aria-label="read_file arguments"]');
  expect(block?.textContent).toContain('{\n  "path": "a.go",\n  "id": 9007199254740993\n}');
});
