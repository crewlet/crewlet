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
import { EMPTY_VALUE } from "@crewlethq/ui";

import { Inbox } from "./Inbox.tsx";
import { SUBJECTS } from "~/lib/attention.ts";
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
    if (what in answers) {
      const answer = answers[what];
      // A THUNK, so a case can answer with silence or a rejection. Building the
      // promise in the fixture object would create one nothing has adopted yet,
      // and an unhandled rejection fails the run somewhere else entirely.
      return typeof answer === "function"
        ? (answer as () => Promise<unknown>)()
        : Promise.resolve(answer);
    }
    if (what === "viewer") {
      return Promise.resolve({
        login: "U0FOUNDER",
        grants: ["state:read", "work:write", "knowledge:write", "people:manage"],
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
      return Promise.resolve({
        login: "U0FOUNDER",
        grants: ["state:read", "work:write", "knowledge:write", "people:manage"],
        handle: "",
        name: "",
      });
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
  expect(openFact?.textContent).toContain(EMPTY_VALUE);
  expect(openFact?.textContent).not.toContain("0");
});

// AN INBOX NOBODY HAS EVER WRITTEN CLAIMS NO READ HISTORY.
//
// Zero rows is six facts, and the band branched on the FACET: a person the
// applier has never written a `tracker_notifications` row for was told
// "Everything the company told you about has been marked read. Switch to All to
// read back through it." — a history that does not exist, and a pointer at a
// facet that is just as empty. `seen_through` is the evidence that separates
// the two and nothing in this bundle read it.
test("an inbox nobody has ever written claims no read history", async () => {
  mount();
  await settle();

  expect(screen.getByText("Nothing has reached you yet")).toBeTruthy();
  const notices = screen.getByText("Notices").closest("section");
  expect(notices?.textContent).not.toContain(
    "Everything the company told you about has been marked read",
  );
  expect(notices?.textContent).not.toContain("Switch to All");
});

// AND THE OTHER SIDE OF THE ONE BIT, so a fix cannot collapse both branches
// into the never-reached sentence: a person who HAS marked a page read is
// pointed back through it.
test("a person who has marked a page read is pointed back through it", async () => {
  mount({
    work_inbox: {
      handle: "ada",
      notices: [],
      primary_reasons: [],
      unread: 0,
      primary: 0,
      seen_through: { stream: "CREWLET_WORK_LOG", generation: 1, seq: 42 },
    },
  });
  await settle();

  expect(screen.getByText("You are caught up")).toBeTruthy();
  expect(screen.getByText("Notices").closest("section")?.textContent).toContain("Switch to All");
});

// A REASON THAT MATCHES NOTHING KEEPS THE CHIP THAT WOULD LIFT IT.
//
// `reasons` deliberately keeps a sticky zero-count entry for the selected value
// — "a filter you cannot see is a filter you cannot lift" — and the band hid
// the whole rail exactly when that entry was the only way back, because the
// rail was a CHILD and children are not drawn on the empty path.
test("a reason that matches nothing keeps the chip that would lift it", async () => {
  location.hash = "#/inbox?reason=mention";
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

  // The chip's own text carries its count beside the label, so this matches the
  // label rather than the whole node.
  const rail = screen.getByRole("group", { name: "Reason" });
  expect(rail.textContent).toContain("mentioned you");
  expect(screen.getByText("Nothing on this page carries that reason")).toBeTruthy();
  expect(screen.getByText("Notices").closest("section")?.textContent).not.toContain(
    "has been marked read",
  );
});

// A BAND WHOSE READ HAS NOT ANSWERED COUNTS NOTHING AND CLAIMS NOTHING.
//
// `count={notices.length}` showed a literal `0` in the head before anything had
// answered — the exact claim the pulse strip's own figures are forbidden from
// making one component up — and the empty state asserted a read history over an
// answer nobody had.
test("a band whose read has not answered counts nothing and claims nothing", async () => {
  mount({ work_inbox: () => new Promise(() => {}) });
  await settle();

  // Scoped to the quiet block rather than to the section: the band's own NOTE
  // legitimately says "what reached you", and it draws whatever the read does.
  const notices = screen.getByText("Notices").closest("section");
  expect(notices?.querySelector(".inbox-band-count")?.textContent).toBe(
    `${EMPTY_VALUE}Not counted: this read did not answer`,
  );
  expect(notices?.querySelector(".inbox-quiet")).toBeNull();
});

// A REFUSED INBOX READ IS NOT A CAUGHT-UP INBOX.
//
// `QueryState` was a CHILD of the band, and children are not rendered on the
// empty path — so an `unauthorized` on `work_inbox` drew "everything has been
// marked read" and never drew the refusal.
test("a refused inbox read is not a caught-up inbox", async () => {
  mount({ work_inbox: () => Promise.reject(new Error("unauthorized")) });
  await settle();

  const notices = screen.getByText("Notices").closest("section");
  expect(notices?.textContent).toContain("auth-gated");
  expect(notices?.textContent).not.toContain("marked read");
  expect(notices?.textContent).not.toContain("Nothing has reached you");
});

// THE NOTICES SCOPE IS A SCOPE, NOT A FACET RAIL.
//
// "All" and "Snoozed" name rows the loaded page does not hold — they are
// `work_inbox` parameters — so no count over the loaded rows could ever
// describe them, and the rail still printed the caption that qualifies counts.
// Its all-chip, the chip that means "no filter", had to be labelled `Unread`,
// the NARROWEST of the three.
test("the notices scope offers three states with one chosen, and claims no counts", async () => {
  mount({
    work_inbox: {
      handle: "ada",
      // ONE reason deliberately: the Reason rail's own `length > 1` guard keeps
      // the one rail that legitimately counts off screen, so the caption has no
      // other source.
      notices: [notice("assignee", 1), notice("assignee", 2)],
      primary_reasons: ["assignee"],
      unread: 2,
      primary: 1,
    },
  });
  await settle();

  const group = screen.getByRole("radiogroup", { name: "Which notices" });
  const options = [...group.querySelectorAll('[role="radio"]')];
  expect(options.map((o) => (o.textContent ?? "").trim())).toEqual(["Unread", "All", "Snoozed"]);
  const checked = options.filter((o) => o.getAttribute("aria-checked") === "true");
  expect(checked).toHaveLength(1);
  expect((checked[0]?.textContent ?? "").trim()).toBe("Unread");
  expect(screen.queryByText("counts over the rows loaded")).toBeNull();
});

// AND CHOOSING ONE IS ONE PRESS. A facet rail's all-chip clears on a SECOND
// press, which here silently returned the reader to Unread — a gesture with no
// affordance and a destination nobody named.
test("choosing All is one press, and pressing it again does not go back", async () => {
  mount();
  await settle();

  const all = () =>
    [
      ...screen
        .getByRole("radiogroup", { name: "Which notices" })
        .querySelectorAll('[role="radio"]'),
    ].find((o) => (o.textContent ?? "").trim() === "All")!;
  fireEvent.click(all());
  await settle();
  const param = () => new URLSearchParams(location.hash.split("?")[1] ?? "").get("state");
  expect(param()).toBe("all");
  expect(all().getAttribute("aria-checked")).toBe("true");

  fireEvent.click(all());
  await settle();
  expect(param()).toBe("all");
  expect(all().getAttribute("aria-checked")).toBe("true");
});

// THE QUIET BAND NAMES WHAT WAS CHECKED, AND `lib/attention.ts` OWNS THE LIST.
//
// It read "No seat is stopped, no run is parked on a question, and no budget is
// refusing" — three of the twelve conditions that queue raises, written as a
// closed sentence on the one screen an operator opens to find out whether
// anything is wrong. An engine with no active configuration, a node shedding
// its seats, a draining node, a refused token and a round stalled for eleven
// minutes were all inside the silence that sentence claimed to have measured.
test("the quiet band names every subject the engine's queue watches", async () => {
  mount();
  await settle();

  const quiet = screen.getByText("Nothing needs a decision").closest(".inbox-quiet");
  expect(quiet).toBeTruthy();
  for (const phrase of Object.values(SUBJECTS)) {
    expect(quiet?.textContent, phrase).toContain(phrase);
  }
});
