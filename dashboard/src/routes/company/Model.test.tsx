/**
 * How the phase monitor reads past its first page.
 *
 * The first page is the `phases` question, payloads included; the older pages
 * were an `events` listing followed by one `event` read per row, and they
 * survived a change of seat, so the list filled with another seat's phases
 * under this one's name. It is the same question now, resumed from the cursor
 * its own answer carries, and a new seat starts a new walk.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ModelActivity } from "./Model.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { EventRecord } from "~/protocol/index.ts";

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
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function phase(id: string, role: string, minutesAgo: number): EventRecord {
  return {
    id,
    type: "agent_phase_completed",
    source: "engine",
    actor: role,
    summary: "",
    category: "llm",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    timestamp: new Date(Date.now() - minutesAgo * 60_000).toISOString(),
    payload: {
      turn_id: `turn-${id}`,
      phase: "execute",
      iteration: 1,
      role,
      model: `model-${id}`,
      total_tokens: 10,
    },
  } as EventRecord;
}

/**
 * The monitor over a socket that answers the first page at once and each
 * older page when `older` settles — so a test can hold one in flight.
 */
function mount(older: () => Promise<unknown> = () => Promise.resolve(OLDEST)) {
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params = {}) => {
    asked.push({ what, params });
    if (what !== "phases") return Promise.resolve({});
    if (params.before_id) return older();
    const role = typeof params.role === "string" ? params.role : "CEO";
    return Promise.resolve({
      phases: [phase(`new-${role}`, role, 1)],
      next: { before_time: "2026-09-13T10:00:00Z", before_id: `new-${role}` },
      exhausted: false,
    });
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ModelActivity />
      </Router>
    </ClientContext.Provider>,
  );
  return asked;
}

/** The last page of the CEO's record: one phase, and the end. */
const OLDEST = { phases: [phase("old", "CEO", 90)], next: {}, exhausted: true };

function pickSeat(role: string) {
  location.hash = `#/?role=${role}`;
  window.dispatchEvent(new HashChangeEvent("hashchange"));
}

test("an older page is the same question, resumed from its own cursor", async () => {
  const asked = mount();
  expect(await screen.findByText("model-new-CEO")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /Load 60 older phases/ }));
  await waitFor(() => expect(screen.getByText("model-old")).toBeTruthy());
  const older = asked.filter((a) => a.what === "phases" && a.params.before_id);
  expect(older).toHaveLength(1);
  expect(older[0]?.params).toMatchObject({
    limit: 60,
    before_time: "2026-09-13T10:00:00Z",
    before_id: "new-CEO",
  });
  // ONE READ, payloads included: no per-row `event` fetch behind it.
  expect(asked.some((a) => a.what === "event" || a.what === "events")).toBe(false);
  // THE LAST PAGE SAID IT WAS THE LAST.
  expect(screen.getByText("That is the beginning of the retained record.")).toBeTruthy();
});

test("a change of seat drops the pages read for the one before", async () => {
  mount();
  expect(await screen.findByText("model-new-CEO")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /Load 60 older phases/ }));
  await waitFor(() => expect(screen.getByText("model-old")).toBeTruthy());
  await act(async () => pickSeat("CFO"));
  await waitFor(() => expect(screen.queryByText("model-old")).toBeNull());
});

// A PAGE IN FLIGHT WHEN THE SEAT CHANGES lands nowhere: the list is not
// filtered by seat on this side, so it would draw the CEO's phase under the
// CFO's name and hand the CEO's cursor to the CFO's walk.
test("an older page that answers after a change of seat is dropped", async () => {
  let answer!: (page: unknown) => void;
  const asked = mount(() => new Promise((resolve) => (answer = resolve)));
  expect(await screen.findByText("model-new-CEO")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /Load 60 older phases/ }));
  await act(async () => pickSeat("CFO"));
  expect(await screen.findByText("model-new-CFO")).toBeTruthy();
  await act(async () => answer(OLDEST));
  expect(screen.queryByText("model-old")).toBeNull();
  expect(screen.queryByRole("button", { name: /Back to the newest/ })).toBeNull();
  // THE CFO'S OWN CURSOR, not the one the CEO's answer carried.
  fireEvent.click(screen.getByRole("button", { name: /Load 60 older phases/ }));
  const last = asked.filter((a) => a.what === "phases" && a.params.before_id).at(-1);
  expect(last?.params).toMatchObject({ role: "CFO", before_id: "new-CFO" });
});

test("while older pages are held the screen says so and offers the newest", async () => {
  const asked = mount();
  expect(await screen.findByText("model-new-CEO")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /Load 60 older phases/ }));
  await waitFor(() => expect(screen.getByText("model-old")).toBeTruthy());
  const firstReads = asked.filter((a) => a.what === "phases" && !a.params.before_id).length;
  fireEvent.click(screen.getByRole("button", { name: /Back to the newest/ }));
  await waitFor(() => expect(screen.queryByText("model-old")).toBeNull());
  // THE FIRST PAGE IS ASKED AGAIN, rather than the held one shown as the newest.
  expect(asked.filter((a) => a.what === "phases" && !a.params.before_id).length).toBe(
    firstReads + 1,
  );
});
