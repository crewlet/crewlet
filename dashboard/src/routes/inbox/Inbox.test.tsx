/**
 * What the landing screen says on the day nothing is wrong.
 *
 * This is the first screen anybody sees, and its failure mode is not a crash:
 * it is a queue-shaped home rendering a healthy company as a blank page, which
 * a reader cannot tell from a dashboard that is broken. Three claims below are
 * about exactly that, and each one was a real defect:
 *
 *  1. The first fold is the COMPANY. A quiet queue leaves the pulse strip and
 *     two named bands, not an empty box.
 *  2. Both bands are drawn at once. They were a segmented toggle, so the
 *     engine's own conditions sat behind a control the founder had to press to
 *     discover they existed.
 *  3. The controls do not move under the pointer. The reason chips were
 *     ordered by count — so a poll reordered them — and picking one narrowed
 *     the answer the chips were derived FROM, which unmounted the whole rail:
 *     the chip a reader had just pressed vanished and there was no way back to
 *     the others without hunting for a clear button somewhere else.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Inbox } from "./Inbox.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

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
  location.hash = "#/inbox";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

function notice(reason: string, n: number) {
  return {
    record_id: `r-${reason}-${n}`,
    log_seq: n,
    log_stream: "CREWLET_WORK_LOG",
    log_generation: 1,
    at: new Date(Date.now() - n * 60_000).toISOString(),
    reason,
    primary: reason === "assignee",
    addressed: false,
    kind: "task_updated",
    subject_id: `s-${n}`,
    subject_key: `ENG-${n}`,
    excerpt: `something happened, ${reason} ${n}`,
    read: false,
  };
}

/**
 * Mount the Inbox over a socket answering a BOUND viewer and a quiet company.
 *
 * `answers` overrides one question; everything else comes back as the smallest
 * well-formed answer, because a screen that has to be fed eleven fixtures to
 * render at all is a screen no case will keep up to date.
 */
function mount(answers: Record<string, unknown> = {}) {
  const store = new Store();
  // CONNECTED, because an inert socket is a condition in its own right: the
  // attention queue raises "the dashboard is not connected" and band 1 is then
  // legitimately non-empty. These cases are about a company that is FINE.
  store.applyHealth({ status: "healthy" });
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what: string) => {
    if (what in answers) return Promise.resolve(answers[what]);
    if (what === "viewer") {
      return Promise.resolve({
        operator_id: "U0FOUNDER",
        operator: true,
        handle: "ada",
        name: "Ada",
        kind: "human",
      });
    }
    if (what === "work_inbox") {
      return Promise.resolve({
        handle: "ada",
        notices: [],
        primary_reasons: [],
        unread: 0,
        primary: 0,
      });
    }
    if (what === "sandbox_runs") return Promise.resolve({ runs: [] });
    if (what === "work_projects")
      return Promise.resolve({ projects: [], total: 0, complete: true });
    if (what === "work_workload") return Promise.resolve({ rows: [] });
    return Promise.resolve({});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Inbox />
      </Router>
    </ClientContext.Provider>,
  );
  return { store, socket };
}

async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

// A HEALTHY COMPANY IS NOT A BLANK PAGE.
//
// Nothing is waiting, nothing is alarming, and the screen still has to say what
// the company IS — otherwise the reader's first contact with the product is an
// empty box, and an empty box is what a broken dashboard looks like too.
test("a company with nothing waiting still renders its own state", async () => {
  mount();
  await settle();

  // The pulse strip, with its figures — the fold that is true whatever the
  // queue holds.
  expect(screen.getByRole("group", { name: "The company right now" })).toBeTruthy();
  expect(screen.getByText("open")).toBeTruthy();
  expect(screen.getByText("tokens")).toBeTruthy();
  expect(screen.getByText("alarms")).toBeTruthy();

  // And both bands say what they are rather than disappearing with their rows.
  expect(screen.getByText("Needs a decision")).toBeTruthy();
  expect(screen.getByText("Notices")).toBeTruthy();
  expect(screen.getByText("Nothing needs a decision")).toBeTruthy();
});

// BOTH BANDS AT ONCE, WITHOUT PRESSING ANYTHING.
//
// They were a segmented toggle, which put the engine's own conditions behind a
// control: a founder glancing at the landing screen saw one band and had no
// reason to think the other existed.
test("the engine's conditions and the person's notices are both on screen at once", async () => {
  mount({
    work_inbox: {
      handle: "ada",
      notices: [notice("assignee", 1), notice("watcher", 2)],
      primary_reasons: ["assignee"],
      unread: 2,
      primary: 1,
    },
  });
  await settle();

  expect(screen.getByText("Needs a decision")).toBeTruthy();
  expect(screen.getByText("something happened, assignee 1")).toBeTruthy();
  // The non-primary one is in the SAME list, not behind a second tab.
  expect(screen.getByText("something happened, watcher 2")).toBeTruthy();
});

// THE CHIPS DO NOT MOVE, AND THEY DO NOT VANISH.
//
// Two defects in one control. Ordered by count, a poll that changed one
// reordered the row, so the chip a reader was reaching for moved between the
// decision to press and the press. And the filter was sent to the ENGINE, so
// the answer the chips were derived from became the one reason that had been
// picked: every other count went to zero, the rail's `length > 1` guard failed,
// and the whole row unmounted under the pointer.
test("picking a reason keeps every other reason on screen, in the same order", async () => {
  mount({
    work_inbox: {
      handle: "ada",
      // Deliberately lopsided: `watcher` has three and would sort FIRST by
      // count while sorting LAST by name, so the two orderings disagree here.
      notices: [
        notice("assignee", 1),
        notice("watcher", 2),
        notice("watcher", 3),
        notice("watcher", 4),
      ],
      primary_reasons: ["assignee"],
      unread: 4,
      primary: 1,
    },
  });
  await settle();

  const chips = () =>
    [...screen.getByRole("group", { name: "Reason" }).querySelectorAll("button")].map((b) =>
      (b.textContent ?? "").trim(),
    );
  const before = chips();
  // Stable by NAME: "assigned to you" before "watching it", whatever the counts.
  expect(before.length).toBeGreaterThan(2);
  const named = before.filter((c) => c !== "All");
  expect([...named].sort((a, b) => a.localeCompare(b))).toEqual(named);

  const watcher = screen.getAllByRole("button").find((b) => /watch/i.test(b.textContent ?? ""));
  expect(watcher).toBeTruthy();
  fireEvent.click(watcher!);
  await settle();

  // Every chip is still there, in the same order — only the rows narrowed.
  expect(chips()).toEqual(before);
  expect(screen.queryByText("something happened, assignee 1")).toBeNull();
  expect(screen.getByText("something happened, watcher 2")).toBeTruthy();
});

// THE DETAIL PANE IS PRESENT BEFORE ANYTHING IS SELECTED.
//
// A pane that appears on the first click reflows the list under the pointer:
// the row a reader clicked is no longer the row they are looking at, which is
// the same defect as a chip that moves, one component up.
test("the detail pane holds its column before a row is picked", async () => {
  mount();
  await settle();

  const pane = screen.getByRole("complementary", { name: "The selected row" });
  expect(pane).toBeTruthy();
  expect(pane.textContent).toContain("Pick a row");
});

// A FIGURE NOBODY HAS ANSWERED IS NOT A ZERO.
//
// The strip renders before its two slow polls land. A `0` there tells a founder
// their company has no open work, which is a false statement that corrects
// itself a second later — and on a slow link, not for several.
test("a figure whose query has not answered draws a dash, never a zero", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what: string) => {
    if (what === "viewer") {
      return Promise.resolve({ operator_id: "U0FOUNDER", operator: true, handle: "", name: "" });
    }
    // The two the strip reads never answer.
    if (what === "work_projects" || what === "work_workload") return new Promise(() => {});
    if (what === "sandbox_runs") return Promise.resolve({ runs: [] });
    return Promise.resolve({});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Inbox />
      </Router>
    </ClientContext.Provider>,
  );
  await settle();

  const strip = screen.getByRole("group", { name: "The company right now" });
  const openFact = [...strip.querySelectorAll("a")].find((a) => a.textContent?.includes("open"));
  expect(openFact?.textContent).toContain("—");
  expect(openFact?.textContent).not.toContain("0");
});
