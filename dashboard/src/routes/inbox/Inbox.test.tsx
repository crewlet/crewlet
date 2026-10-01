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

import { Profiler } from "react";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";

import { Inbox } from "./Inbox.tsx";
import { SUBJECTS } from "~/lib/attention.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import {
  CLAIMANT,
  CLAIMANT_HREF,
  DUPLICATE,
  DUPLICATE_HREF,
  SHARED_KEY,
} from "~/test/keyCollision.ts";

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
  vi.useRealTimers();
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
function mount(answers: Record<string, unknown> = {}, onCommit: () => void = () => {}) {
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
        owner: "ada",
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
        <Profiler id="inbox" onRender={onCommit}>
          <Inbox />
        </Profiler>
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
        owner: "U0FOUNDER",
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
  expect(notices?.textContent).toContain("does not carry the grant");
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

/** A seat on one round, last heard from `updated`. */
function onOneRound(updated: string) {
  return {
    id: "a",
    agent_id: "id-a",
    role: "Dev A",
    handle: "dev-a",
    kind: "agent",
    live_call: {
      turn_id: "t1",
      phase: "execute",
      iteration: 1,
      model: "",
      trigger: null,
      prompt: "",
      prompt_messages: null,
      response: "",
      input_tokens: 0,
      output_tokens: 0,
      total_tokens: 0,
      tool_executions: null,
      round_num: 3,
      rounds: 3,
      in_progress: true,
      updated_at: updated,
    },
  };
}

/** Seconds of the shared clock, one tick at a time, as the browser runs it. */
function tick(times: number) {
  for (let i = 0; i < times; i++) {
    act(() => {
      vi.advanceTimersByTime(1_000);
    });
  }
}

// THE LANDING SCREEN IS NOT DRAWN ONCE A SECOND.
//
// It held the one-second clock for the attention queue, and handed it to every
// notice row for its "5m ago": a tick drew the pulse strip, both bands and
// every row again, on the screen every person opens first and leaves open.
// The queue is read as a value now and each row reads its own words, so ten
// seconds in which nothing crosses a threshold commit nothing at all.
test("a tick of the clock draws nothing on the inbox", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  const now = Date.parse("2031-04-16T12:00:00Z");
  vi.setSystemTime(now);
  let commits = 0;
  const { store } = mount(
    {
      work_inbox: {
        handle: "ada",
        // FIVE MINUTES AGO AND MORE, so ten seconds cannot change a row's
        // words: a row whose "4m ago" turned over would commit honestly, and
        // this case is about the ones that did not.
        notices: [notice("assignee", 5), notice("watcher", 6)],
        primary_reasons: ["assignee"],
        unread: 2,
        primary: 1,
      },
    },
    () => {
      commits += 1;
    },
  );
  // A LIVE ROUND HEARD FROM JUST NOW, so the queue has a condition that reads
  // the clock on every tick and finds nothing to raise.
  act(() => {
    store.applySeats([onOneRound(new Date(now - 1_000).toISOString())]);
  });
  await settle();
  expect(screen.getByText("something happened, assignee 5")).toBeTruthy();
  expect(screen.getByText("5m ago")).toBeTruthy();

  const settled = commits;
  tick(10);
  expect(commits).toBe(settled);
});

// AND THE QUEUE STILL MOVES WITH THE CLOCK. A round goes stale at two minutes
// whether or not anything else on the screen changed, so the tick that crosses
// the threshold is the one that raises the row — not the next poll, and not
// never, which is what a queue computed once from its inputs would do.
test("a round that stops moving is raised on the tick that crosses two minutes", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  const now = Date.parse("2031-04-16T12:00:00Z");
  vi.setSystemTime(now);
  const { store } = mount();
  act(() => {
    store.applySeats([onOneRound(new Date(now - 115_000).toISOString())]);
  });
  await settle();
  expect(screen.queryByText(/has been on one round/)).toBeNull();

  tick(4);
  expect(screen.queryByText(/has been on one round/)).toBeNull();

  tick(1);
  expect(screen.getByText("Dev A has been on one round for over 2 minutes")).toBeTruthy();
});

// A NOTICE LEADS TO THE TASK IT IS ABOUT, even under a key another task holds.
//
// A notice keeps the key its task held when it was written, and a key two
// tasks hold opens the one that claimed it first — so a notice about the
// duplicate sent its reader to the claimant. The engine says beside the stored
// key when it opens another task, and that notice's link goes by the task's id.
test("two notices under one key lead to their own two tasks", async () => {
  mount({
    work_inbox: {
      handle: "ada",
      notices: [
        {
          ...notice("assignee", 1),
          subject_id: DUPLICATE,
          subject_key: SHARED_KEY,
          subject_key_collision: true,
          excerpt: "about the duplicate",
        },
        {
          ...notice("assignee", 2),
          subject_id: CLAIMANT,
          subject_key: SHARED_KEY,
          excerpt: "about the claimant",
        },
      ],
      primary_reasons: ["assignee"],
      unread: 2,
      primary: 2,
    },
  });
  await settle();
  const subjectLink = () =>
    [...document.querySelectorAll<HTMLAnchorElement>("a.t-link.mono")].find((a) =>
      a.textContent?.startsWith(SHARED_KEY),
    );

  fireEvent.click(screen.getByText("about the duplicate").closest("button")!);
  await settle();
  expect(subjectLink()?.getAttribute("href")).toBe(DUPLICATE_HREF);

  fireEvent.click(screen.getByText("about the claimant").closest("button")!);
  await settle();
  expect(subjectLink()?.getAttribute("href")).toBe(CLAIMANT_HREF);
});
