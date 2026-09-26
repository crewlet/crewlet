/**
 * What the turns list says a turn IS.
 *
 * A turn that launched a detached coding run PARKS: it completes a segment and
 * will complete again when the run is collected. The engine used to list it as
 * finished the moment it parked, and before that distinction existed this
 * screen had only two words for a row — finished, or `running`. A parked turn
 * is neither, and drawing it as either sends a reader to the wrong place: a
 * "running" turn with no live phase reads as a wedged loop, and a finished one
 * hides that its real work is still out in a box.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { TurnRow } from "~/protocol/index.ts";
import { Turns } from "./Turns.tsx";

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
  location.hash = "";
});

function row(id: string, summary: string, extra: Partial<TurnRow>): TurnRow {
  const at = new Date(Date.now() - 60_000).toISOString();
  return {
    turn_id: id,
    role: "CEO",
    started_at: at,
    ended_at: at,
    duration_ms: 0,
    complete: false,
    parked: false,
    phases: 1,
    iterations: 1,
    failed: false,
    input_tokens: 10,
    output_tokens: 2,
    total_tokens: 12,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
    summary,
    ...extra,
  };
}

function mount(turns: TurnRow[]) {
  location.hash = "#/live/turns";
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.applyOrg({ roles: [{ name: "CEO", handle: "ceo" }] });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    Promise.resolve(what === "turns" ? { turns, next: null } : {});
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Turns />
      </Router>
    </ClientContext.Provider>,
  );
}

const SUMMARIES = ["the finished one", "the parked one", "the running one"];

/** The widest element around one summary that holds no other row's — its row,
 *  whatever markup the table draws a row with. */
function rowOf(summary: string): HTMLElement {
  let at: HTMLElement = screen.getByText(summary);
  const others = SUMMARIES.filter((s) => s !== summary);
  while (at.parentElement && !others.some((s) => at.parentElement!.textContent?.includes(s))) {
    at = at.parentElement;
  }
  return at;
}

test("a parked turn is marked parked, never running or finished", async () => {
  mount([
    row("t-done", "the finished one", { complete: true, duration_ms: 4_000 }),
    row("t-parked", "the parked one", { parked: true, duration_ms: 3_000 }),
    row("t-live", "the running one", {}),
  ]);
  await screen.findByText("the parked one");

  await waitFor(() => expect(rowOf("the parked one").textContent).toContain("parked"));
  expect(rowOf("the parked one").textContent).not.toContain("running");
  expect(rowOf("the running one").textContent).toContain("running");
  expect(rowOf("the running one").textContent).not.toContain("parked");
  expect(rowOf("the finished one").textContent).not.toMatch(/parked|running/);
});

/**
 * ONE SEAT'S TURNS ARE ASKED FOR BY ITS HANDLE.
 *
 * The sidebar's seat rows link here with `seat=<handle>`, and the list used to
 * read a `role` parameter instead — so every one of those rows landed on the
 * whole company's turns. The engine resolves the handle to the seat's own id,
 * which a role name shared by two unit seats cannot name.
 */
test("the seat in the address is sent to the engine as its handle", async () => {
  location.hash = "#/live/turns?seat=ceo";
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.applyOrg({ roles: [{ name: "CEO", handle: "ceo" }] });
  const socket = new LiveSocket(store);
  const asked: Record<string, unknown>[] = [];
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params) => {
    if (what === "turns") asked.push(params ?? {});
    return Promise.resolve(what === "turns" ? { turns: [], next: null } : {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Turns />
      </Router>
    </ClientContext.Provider>,
  );
  await waitFor(() => expect(asked.length).toBeGreaterThan(0));
  expect(asked[0]).toMatchObject({ seat: "ceo" });
  expect(asked[0]).not.toHaveProperty("role");
});

// A ROW NAMES ITS SEAT BY THE SEAT'S BADGE, AND ITS ITEM BY ONE UNBROKEN KEY.
//
// The seat cell drew a processor glyph where every other surface identifies a
// seat by its badge; the key after the summary broke at its hyphen into
// `ENG-` over `22`, making that row taller than its neighbours. `.item-key`
// is what holds the key on one line (base.css), and the summary truncates
// instead.
test("a row draws the seat's badge and its item key as one token", async () => {
  mount([
    row("t-1", "reviewed the work", {
      complete: true,
      work_item: { backend: "native", id: "x", key: "ENG-22", project: "ENG" },
    }),
  ]);
  await screen.findByText("reviewed the work");
  const r = rowOf("reviewed the work");
  expect(r.querySelector(".cell-seat .crewlet-avatar")).not.toBeNull();
  const key = [...r.querySelectorAll("span")].find((s) => s.textContent === "ENG-22");
  expect(key?.classList.contains("item-key")).toBe(true);
});

// THE CHART'S HINT GIVES WAY BEFORE ITS TITLE. It sat in the header's
// actions, which never shrink, so on a phone the title read "W" beside a
// whole sentence; the subtitle is the part the kit lets give way first.
test("the chart's hint is in the header's subtitle, not its actions", async () => {
  const { container } = mount([row("t-1", "one turn", { complete: true })]);
  await screen.findByText("one turn");
  const subtitle = container.querySelector(".crewlet-card__subtitle");
  expect(subtitle?.textContent).toMatch(/click one to narrow the window/);
  expect(container.querySelector(".crewlet-card__header-actions")?.textContent ?? "").not.toMatch(
    /narrow the window/,
  );
});

// A FAILED TURN WEARS THE DANGER TONE. It wore amber — the one state that asks
// a person for a decision — which a failed turn does not.
test("a turn that carried a failure is marked in the danger tone", async () => {
  mount([row("t-bad", "the failed one", { complete: true, failed: true })]);
  const tag = (await screen.findByText("failure")).closest(".crewlet-tag");
  expect(tag?.classList.contains("crewlet-tag--danger")).toBe(true);
});
