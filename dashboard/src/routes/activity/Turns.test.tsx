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
  location.hash = "#/activity/turns";
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
