/**
 * What a bridged run's tool log claims.
 *
 * This is the one tool log with nowhere else to live — a bridged run's calls
 * are made by a process outside the engine, minutes apart and possibly across
 * a restart — so what the screen says about it is the whole record a reviewer
 * gets. Two claims have to be exactly right: that a run made no calls, and
 * that the log in front of them is complete.
 */

import {
  act,
  cleanup,
  fireEvent,
  render as rtlRender,
  screen,
  within,
} from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { ReactElement } from "react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { overflowing } from "~/testing.tsx";
import { BridgeLog, RunScreen } from "./Runs.tsx";
import type { SandboxRun } from "~/protocol/index.ts";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  location.hash = "";
});

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

// ---------------------------------------------------------------------------
// A run's own page
// ---------------------------------------------------------------------------

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const JANE = {
  login: "U0FOUNDER",
  grants: [
    "config:read",
    "config:write",
    "secrets:write",
    "fleet:operate",
    "people:manage",
    "audit:read",
    "state:read",
    "work:write",
    "knowledge:write",
  ],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["answer_run"],
};

const ORG = {
  name: "Nimbus",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Ada Engineer", handle: "ada", kind: "agent", role: "SWE" },
  ],
  units: [],
};

/**
 * Mount `#/live/runs/turn-1` over stubbed answers, recording every question
 * and every write; `outcome` is what the engine answers a write with.
 */
function mountPage(answers: Record<string, unknown>, outcome = "pending") {
  location.hash = "#/live/runs/turn-1";
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  const posted: { tool: string; args: Record<string, unknown> }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(JSON.stringify({ tool, outcome, position: "", receipt: {} }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  const store = new Store();
  store.applyHealth({ status: "healthy" } as never);
  store.applyOrg(ORG as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ what, params: params ?? {} });
    const all: Record<string, unknown> = {
      viewer: JANE,
      work_inbox: { handle: "jane", notices: [], primary_reasons: [] },
      ...answers,
    };
    const answer = all[what];
    // AN `Error` STANDS FOR A READ THAT FAILED: the socket rejects with the
    // refusal's code, which is what `useQuery` surfaces as `error`.
    if (answer instanceof Error) return Promise.reject(answer);
    return Promise.resolve(answer ?? {});
  }) as typeof socket.query;
  rtlRender(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>
              <RunScreen turnId="turn-1" />
            </Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { asked, posted };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

// A PARKED RUN IS ANSWERED ON ITS PAGE, BY ITS TURN.
//
// The banner said how a run could be answered and offered nothing to press —
// and a run a schedule or an assignment launched stored no conversation, so
// there was no reply path at all. `answer_run` names the run by its turn, and
// a `pending` outcome (the answer is on the seat's inbox, the owning node
// resumes the run) closes the dialog like an applied one.
//
// Mutation: drop the Answer button from the banner, and there is nothing to
// press; send `run_id` for `turn_id`, and the write names no run.
test("a parked run's page answers it by its turn", async () => {
  const { posted } = mountPage({
    sandbox_runs: {
      runs: [
        run({
          status: "awaiting_clarification",
          question: "Which retry ceiling?",
          audience: "requester",
          audience_handles: ["jane"],
        }),
      ],
    },
    turn: { turn_id: "turn-1", events: [] },
  });
  await settle();
  const banner = screen.getByText("Which retry ceiling?").closest(".run-awaiting") as HTMLElement;
  expect(within(banner).getByText(/Put to Jane Founder\./)).toBeTruthy();
  fireEvent.click(within(banner).getByRole("button", { name: "Answer" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("Your answer"), {
    target: { value: "Thirty seconds." },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Send answer" }));
  await settle();
  expect(posted).toEqual([
    { tool: "answer_run", args: { turn_id: "turn-1", answer: "Thirty seconds." } },
  ]);
  expect(screen.queryByRole("dialog")).toBeNull();
});

// A RUNNING JOB'S OUTPUT IS ASKED OF ITS OWNER, by the job the row holds.
//
// Mutation: ask without `launch_id`, and the engine refuses every read.
test("a running run's page polls the live output of the job it holds", async () => {
  const { asked } = mountPage({
    sandbox_runs: { runs: [run({ status: "running", launch_id: "job-2" })] },
    turn: { turn_id: "turn-1", events: [] },
    sandbox_tail: {
      outcome: "tail",
      turn_id: "turn-1",
      launch_id: "job-2",
      node: "node-a",
      output: {
        text: "running the tests",
        source: "transcript",
        cut: false,
        as_of: new Date().toISOString(),
        finished: false,
      },
    },
  });
  await settle();
  expect(asked.find((a) => a.what === "sandbox_tail")?.params).toEqual({
    turn_id: "turn-1",
    launch_id: "job-2",
  });
  expect(await screen.findByText("running the tests")).toBeTruthy();
});

// A SETTLED RUN STILL HAS A PAGE: its record is gone from the board, and what
// it did is the `sandbox` phase its turn published, with the transcript on it.
// It used to answer "no coding run" to a link to a run that had finished.
//
// Mutation: drop the collected records, and the page claims there is no run.
test("a collected run's page shows what its job did, off its turn", async () => {
  mountPage({
    sandbox_runs: { runs: [] },
    turn: {
      turn_id: "turn-1",
      events: [
        {
          id: "s-1",
          type: "sandbox_run_started",
          timestamp: "2026-06-15T12:00:00Z",
          source: "SWE",
          actor: "SWE",
          summary: "",
          category: "system",
          trace_id: "tr-1",
          span_id: "",
          parent_span_id: "",
          topic: "",
          payload: { turn_id: "turn-1", task: "Add retry to the webhook client" },
        },
        {
          id: "p-1",
          type: "agent_phase_completed",
          timestamp: "2026-06-15T12:10:00Z",
          source: "SWE",
          actor: "SWE",
          summary: "",
          category: "llm",
          trace_id: "tr-1",
          span_id: "",
          parent_span_id: "",
          topic: "",
          payload: {
            turn_id: "turn-1",
            phase: "sandbox",
            iteration: 1,
            role: "SWE",
            coding_agent: "claude",
            launch_id: "job-1",
            activity_transcript: "edited retry.go; tests pass",
            delivered_refs: ["crewlet/ada/retry"],
          },
        },
      ],
    },
  });
  await settle();
  expect(screen.getByText("edited retry.go; tests pass")).toBeTruthy();
  // NAMED BY ITS TASK, off the launch the turn holds — the row that carried
  // it is gone once the run is collected.
  expect(screen.getByRole("heading", { name: /Add retry to the webhook client/ })).toBeTruthy();
  expect(screen.getByText("collected")).toBeTruthy();
  expect(screen.getByText("crewlet/ada/retry")).toBeTruthy();
  expect(screen.queryByText("No coding run for this turn")).toBeNull();
});

// "COULD NOT READ" IS NOT "NONE". The page is drawn from two reads, and a run
// found in neither is absent only when both answered. It said "Neither the
// run record nor the turn holds a coding run" whenever ONE read failed — a
// claim about a record it never read — and headed the page "No task was
// recorded" as though a run existed.
//
// Mutation: gate the empty state on `!run && collected.length === 0` alone
// (as it was), and the failed board reads as an absent run.
test("a failed run record is named rather than read as no run", async () => {
  mountPage({
    sandbox_runs: new Error("unavailable"),
    turn: { turn_id: "turn-1", events: [] },
  });
  await settle();
  expect(screen.queryByText(/Neither the run record nor the turn holds/)).toBeNull();
  expect(screen.getByText(/The run record could not be read/)).toBeTruthy();
  expect(screen.queryByRole("heading", { name: "No coding run" })).toBeNull();
});

test("a failed turn is named rather than read as no run", async () => {
  mountPage({
    sandbox_runs: { runs: [] },
    turn: new Error("timeout"),
  });
  await settle();
  expect(screen.queryByText(/Neither the run record nor the turn holds/)).toBeNull();
  expect(screen.getByText(/The turn could not be read/)).toBeTruthy();
});

// A TURN WITH NO RUN IS HEADED AS HAVING NONE. "No task was recorded" is a
// fact about a run whose launch carried no task, and over a turn with no run
// at all it asserted a run that does not exist.
//
// Mutation: fall back to "No task was recorded" for every run-less page.
test("a turn with no coding run is headed as having none", async () => {
  mountPage({ sandbox_runs: { runs: [] }, turn: { turn_id: "turn-1", events: [] } });
  await settle();
  expect(screen.getByRole("heading", { name: "No coding run" })).toBeTruthy();
  expect(screen.queryByText("No task was recorded")).toBeNull();
  expect(screen.getByText("No coding run for this turn")).toBeTruthy();
});
