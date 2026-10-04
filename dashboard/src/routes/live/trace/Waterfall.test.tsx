/**
 * The Timeline tab as a screen: what it asks the engine, and what it says.
 *
 * The placement rules are `lib/waterfall.test.ts`'s. What these hold is the
 * part a pure model cannot: that a running coding run's live output is asked
 * for ONLY while a reader has its span open (and stops the moment they close
 * it), that a silent owner is named rather than drawn as an empty tail, and
 * that a person's note is drawn as what became of it.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { buildWaterfall } from "~/lib/waterfall.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { EventRecord, SandboxTailAnswer } from "~/protocol/index.ts";
import { SANDBOX_TAIL_POLL_MS, Waterfall, liveState, steerMarks } from "./Waterfall.tsx";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/live/turns/turn-1";
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const T0 = Date.parse("2026-09-28T10:00:00Z");
const iso = (ms: number) => new Date(T0 + ms).toISOString();

function event(type: string, ms: number, payload: Record<string, unknown>): EventRecord {
  return {
    id: `${type}-${ms}`,
    type,
    timestamp: iso(ms),
    source: "SWE",
    actor: "SWE",
    summary: "",
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    payload: { turn_id: "turn-1", ...payload },
  };
}

/** A turn parked on one coding run that is still out. */
const PARKED = [
  event("agent_turn_started", 0, { started_at: iso(0) }),
  event("sandbox_run_started", 1_000, {
    launch_id: "L1",
    started_at: iso(1_000),
    coding_agent: "claude-code",
  }),
];

/** Mount the Timeline over a stubbed tail answer, counting the asks. */
function mount(
  events: EventRecord[],
  tail: SandboxTailAnswer,
  {
    selected: initial = "",
    now = T0 + 60_000,
    org,
  }: { selected?: string; now?: number; org?: Record<string, unknown> } = {},
) {
  const store = new Store();
  if (org) store.applyOrg(org as never);
  const socket = new LiveSocket(store);
  const asked: Record<string, unknown>[] = [];
  (socket as unknown as { query: (what: string, p: unknown) => Promise<unknown> }).query = (
    what: string,
    params: unknown,
  ) => {
    if (what === "sandbox_tail") {
      asked.push(params as Record<string, unknown>);
      return Promise.resolve(tail);
    }
    return Promise.resolve({});
  };
  const model = buildWaterfall({ events, phases: [], now, running: true, parked: true });
  let selected = initial;
  const draw = () => (
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <Waterfall
            model={model}
            phases={[]}
            marks={steerMarks(events, "turn-1")}
            agent="SWE"
            selected={selected}
            onSelect={(id) => {
              selected = id;
              view.rerender(draw());
            }}
            now={now}
            turnId="turn-1"
          />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>
  );
  const view = render(draw());
  return { asked, selected: () => selected };
}

async function flush() {
  await act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
}

const RUNNING: SandboxTailAnswer = {
  outcome: "tail",
  turn_id: "turn-1",
  launch_id: "L1",
  node: "node-b",
  output: {
    text: "running go test ./...",
    source: "transcript",
    cut: false,
    as_of: iso(59_000),
    finished: false,
  },
};

describe("a running coding run's live output", () => {
  test("is asked for only while its span is open, and no longer once it is closed", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: false, toFake: ["setTimeout", "clearTimeout"] });
    const { asked } = mount(PARKED, RUNNING);
    await flush();
    // NOBODY IS LOOKING: the run is drawn, and nothing is asked.
    expect(screen.getByRole("button", { name: /Coding run/ })).toBeTruthy();
    expect(asked).toHaveLength(0);

    fireEvent.click(screen.getByRole("button", { name: /Coding run/ }));
    await flush();
    expect(asked).toEqual([{ turn_id: "turn-1", launch_id: "L1" }]);
    expect(screen.getByText("running go test ./...")).toBeTruthy();

    // IT POLLS while open…
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SANDBOX_TAIL_POLL_MS);
    });
    expect(asked).toHaveLength(2);

    // …AND STOPS WHEN CLOSED.
    fireEvent.click(screen.getByRole("button", { name: "Close the span" }));
    await flush();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SANDBOX_TAIL_POLL_MS * 3);
    });
    expect(asked).toHaveLength(2);
  });

  test("a silent owner is named, never drawn as an empty tail", async () => {
    mount(
      PARKED,
      { outcome: "owner_silent", turn_id: "turn-1", launch_id: "L1", node: "node-b" },
      { selected: "run:L1" },
    );
    expect(
      await screen.findByText(/node-b, the node that owns this run, did not answer/),
    ).toBeTruthy();
    expect(screen.queryByText(/has not written anything/)).toBeNull();
  });

  test("a run that has written nothing says so, which is not a silent owner", async () => {
    mount(
      PARKED,
      { ...RUNNING, output: { ...RUNNING.output!, text: "", source: "none" } },
      { selected: "run:L1" },
    );
    expect(await screen.findByText(/has not written anything it can show yet/)).toBeTruthy();
  });

  test("a run that stopped running stops the poll", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: false, toFake: ["setTimeout", "clearTimeout"] });
    const { asked } = mount(
      PARKED,
      {
        outcome: "not_running",
        turn_id: "turn-1",
        launch_id: "L1",
        status: "awaiting_clarification",
      },
      { selected: "run:L1" },
    );
    await flush();
    expect(screen.getByText(/stopped to ask a person something/)).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SANDBOX_TAIL_POLL_MS * 3);
    });
    expect(asked).toHaveLength(1);
  });
});

describe("a person's note", () => {
  test("is drawn at the round that read it, or as expired", async () => {
    const events = [
      ...PARKED,
      event("agent_turn_steered", 2_000, {
        note_id: "n1",
        outcome: "delivered",
        phase: "execute",
        iteration: 1,
        round: 3,
        note: "use staging",
        steered_by: "jane",
        steered_by_kind: "human",
      }),
      event("agent_turn_steered", 3_000, { note_id: "n2", outcome: "expired", note: "too late" }),
    ];
    const marks = steerMarks(events, "turn-1");
    expect(marks.map((m) => [m.outcome, m.spanId])).toEqual([
      ["delivered", "turn-1|execute|1.r3"],
      ["expired", ""],
    ]);
    // THE SENDER BY NAME, as every other attribution is drawn: the record
    // carries the handle, and "from jane" beside "Jane Founder" everywhere
    // else read as two people.
    mount(events, RUNNING, {
      org: { name: "Acme", roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }] },
    });
    await flush();
    expect(screen.getByText(/read at round 3 of Execute · from Jane Founder/)).toBeTruthy();
    // NEUTRAL, because the reader is not always the sender.
    expect(screen.getByText("the turn finished before it read the note")).toBeTruthy();
    expect(screen.getByText("delivered")).toBeTruthy();
    expect(screen.getByText("expired")).toBeTruthy();
  });

  // NAMED BY ITS HEADING, which a screen reader announces: an `aria-label` on
  // a plain `div` is ignored, so the list had no name and no landmark.
  test("the notes are a region a screen reader can find by name", async () => {
    mount(
      [...PARKED, event("agent_turn_steered", 3_000, { note_id: "n2", outcome: "expired" })],
      RUNNING,
    );
    await flush();
    const region = screen.getByRole("region", { name: "Notes sent to this turn" });
    expect(within(region).getByText("expired")).toBeTruthy();
  });
});

/** A settled turn with three spans on its clock: the turn, a context, a phase. */
const SETTLED = [
  event("agent_turn_started", 0, { started_at: iso(0) }),
  event("prefetch_summary", 500, { started_at: iso(0), duration_ms: 500 }),
  event("sandbox_run_started", 1_000, {
    launch_id: "L1",
    started_at: iso(1_000),
    coding_agent: "claude-code",
  }),
];

const rows = () => screen.getAllByRole("button", { pressed: false }).filter((b) => b.dataset.span);

describe("the waterfall from a keyboard", () => {
  // ONE TAB STOP, and the arrows inside it: a turn of sixteen spans was
  // sixteen presses to walk past on the way to the tabs below.
  test("the rows are one tab stop, and the arrows move between them", async () => {
    mount(SETTLED, RUNNING);
    await flush();
    const all = rows();
    expect(all.length).toBeGreaterThan(2);
    expect(all.filter((b) => b.tabIndex === 0)).toHaveLength(1);
    all[0]!.focus();
    fireEvent.keyDown(all[0]!, { key: "ArrowDown" });
    expect(document.activeElement).toBe(all[1]);
    expect(all[1]!.tabIndex).toBe(0);
    expect(all[0]!.tabIndex).toBe(-1);
    fireEvent.keyDown(all[1]!, { key: "End" });
    expect(document.activeElement).toBe(all[all.length - 1]);
    fireEvent.keyDown(document.activeElement!, { key: "Home" });
    expect(document.activeElement).toBe(all[0]);
  });

  // ESCAPE CLOSES, AND FOCUS GOES HOME: the close button unmounts under the
  // reader's focus, which a browser then drops on the page's top.
  test("Escape closes the open span and puts focus back on its row", async () => {
    const view = mount(SETTLED, RUNNING);
    await flush();
    const run = screen.getByRole("button", { name: /Coding run/ });
    fireEvent.click(run);
    await flush();
    expect(view.selected()).toBe("run:L1");
    fireEvent.keyDown(screen.getByRole("button", { name: "Close the span" }), { key: "Escape" });
    await act(async () => {
      await new Promise((r) => requestAnimationFrame(() => r(null)));
    });
    expect(view.selected()).toBe("");
    expect(document.activeElement?.getAttribute("data-span")).toBe("run:L1");
  });
});

describe("a span a reader opens", () => {
  const had = {
    scroll: Element.prototype.scrollIntoView,
    rect: HTMLElement.prototype.getBoundingClientRect,
  };
  afterEach(() => {
    Element.prototype.scrollIntoView = had.scroll;
    HTMLElement.prototype.getBoundingClientRect = had.rect;
  });

  /** Lay the detail out UNDER the rows (a phone, a laptop with the peek) or beside them. */
  function lay(stacked: boolean) {
    HTMLElement.prototype.getBoundingClientRect = function (this: HTMLElement) {
      const detail = this.classList.contains("span-detail");
      const rail = this.classList.contains("waterfall");
      const top = detail ? (stacked ? 900 : 100) : 100;
      const bottom = rail ? 880 : detail ? top + 300 : 0;
      return {
        top,
        bottom,
        left: 0,
        right: 0,
        width: 0,
        height: bottom - top,
        x: 0,
        y: top,
      } as DOMRect;
    };
    const scrolled: Element[] = [];
    Element.prototype.scrollIntoView = function (this: Element) {
      scrolled.push(this);
    };
    return scrolled;
  }

  // UNDER SIXTEEN ROWS IS BELOW THE FOLD, so a click that opened it seemed to
  // do nothing: the detail is brought into view and focused where it stacks.
  test("is brought into view and focused when it stacks under the rows", async () => {
    const scrolled = lay(true);
    mount(SETTLED, RUNNING);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /Coding run/ }));
    await flush();
    const detail = screen.getByRole("region", { name: /Coding run/ });
    expect(scrolled).toEqual([detail]);
    expect(document.activeElement).toBe(detail);
  });

  test("stays where it is, and focus on its row, when it sits beside them", async () => {
    const scrolled = lay(false);
    mount(SETTLED, RUNNING);
    await flush();
    const run = screen.getByRole("button", { name: /Coding run/ });
    run.focus();
    fireEvent.click(run);
    await flush();
    expect(scrolled).toEqual([]);
    expect(document.activeElement).toBe(run);
  });

  // A LINK THAT ARRIVED WITH A SPAN is where the reader was sent: the page
  // does not scroll away from its own header on load.
  test("that a link arrived with is not scrolled to", async () => {
    const scrolled = lay(true);
    mount(SETTLED, RUNNING, { selected: "run:L1" });
    await flush();
    expect(screen.getByRole("region", { name: /Coding run/ })).toBeTruthy();
    expect(scrolled).toEqual([]);
  });

  // THE EYEBROW ONLY WHERE IT ADDS SOMETHING: "Context" over "Context" read
  // as a stutter.
  test("is not headed with its kind twice", async () => {
    mount(SETTLED, RUNNING, { selected: "" });
    await flush();
    const context = rows().find((b) => b.dataset.kind === "context");
    expect(context, "the fixture draws a context span").toBeTruthy();
    fireEvent.click(context!);
    await flush();
    const region = screen.getByRole("region", {
      name: context!.querySelector(".wf-name")!.textContent!,
    });
    const heading = within(region).getAllByText(context!.querySelector(".wf-name")!.textContent!);
    expect(heading).toHaveLength(1);
  });
});

describe("a live tail's announcements", () => {
  // THE STATE IS ANNOUNCED, NEVER THE OUTPUT. The whole block was a polite
  // live region, so every poll read the clock and the new output aloud.
  test("only the state is in a live region, and it holds still across polls", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: false, toFake: ["setTimeout", "clearTimeout"] });
    mount(PARKED, RUNNING, { selected: "run:L1" });
    await flush();
    const live = document.querySelectorAll("[aria-live], [role='status']");
    expect(live).toHaveLength(1);
    expect(live[0]!.textContent).toBe("Showing the run's live output.");
    expect(live[0]!.contains(screen.getByText("running go test ./..."))).toBe(false);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SANDBOX_TAIL_POLL_MS);
    });
    expect(document.querySelector("[role='status']")!.textContent).toBe(
      "Showing the run's live output.",
    );
  });

  test("names each state in one sentence", () => {
    expect(liveState(undefined, false)).toBe("Asking the node that runs it.");
    expect(liveState(undefined, true)).toBe("The run's output could not be read.");
    expect(
      liveState({ outcome: "owner_silent", turn_id: "t", launch_id: "L", node: "n" }, false),
    ).toBe("The node that owns this run did not answer.");
    expect(
      liveState(
        { outcome: "not_running", turn_id: "t", launch_id: "L", status: "replaced" },
        false,
      ),
    ).toBe("The run is no longer running.");
  });
});
