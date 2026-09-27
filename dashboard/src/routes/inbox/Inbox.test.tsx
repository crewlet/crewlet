/**
 * The Inbox: where a person acts on what reached them.
 *
 * It was the landing screen, opened by a strip of company figures; those are Home's
 * now (`routes/home/Home.test.tsx` holds it), and what is left here is the
 * place a person acts. Two claims below were real defects:
 *
 *  1. Both bands are drawn at once. They were a segmented toggle, so the
 *     engine's own conditions sat behind a control the founder had to press to
 *     discover they existed.
 *  2. The controls do not move under the pointer. The reason chips were
 *     ordered by count — so a poll reordered them — and picking one narrowed
 *     the answer the chips were derived FROM, which unmounted the whole rail:
 *     the chip a reader had just pressed vanished and there was no way back to
 *     the others without hunting for a clear button somewhere else.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { EMPTY_VALUE, LayerHost, ToastProvider } from "@crewlethq/ui";

import { Inbox } from "./Inbox.tsx";
import { SUBJECTS } from "~/lib/attention.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { FrameReadings } from "~/app/Shell.tsx";

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

/** Every question the screen put to the socket, in order, with its params. */
let asked: { what: string; params?: Record<string, unknown> }[] = [];

afterEach(() => {
  asked = [];
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
  ).query = (what: string, params?: Record<string, unknown>) => {
    asked.push({ what, params });
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
    if (what === "decisions")
      return Promise.resolve({ handle: "ada", items: [], total: 0, capped: false });
    if (what === "work_projects")
      return Promise.resolve({ projects: [], total: 0, complete: true });
    if (what === "work_workload") return Promise.resolve({ rows: [] });
    return Promise.resolve({});
  };
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>
              <Inbox />
            </Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
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

// A QUIET INBOX SAYS WHAT ITS BANDS ARE, rather than disappearing with their
// rows — an empty box is what a broken dashboard looks like too.
test("an inbox with nothing waiting still names every band", async () => {
  mount();
  await settle();
  expect(screen.getByText("Waiting on your decision")).toBeTruthy();
  expect(screen.getByText("Nothing waits on your decision")).toBeTruthy();
  expect(screen.getByText("Needs a decision")).toBeTruthy();
  expect(screen.getByText("Notices")).toBeTruthy();
  expect(screen.getByText("Nothing needs a decision")).toBeTruthy();
  // The strip is Home's; it is not drawn twice.
  expect(screen.queryByRole("group", { name: "The company right now" })).toBeNull();
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
// answered — the exact claim Home's own figures are forbidden from
// making — and the empty state asserted a read history over an
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

// THE SNOOZED TAB ASKS FOR ONLY WHAT WAS PUT OFF.
//
// It asked `include_snoozed: true`, which the engine answered with EVERY notice
// — the snoozed ones among them — so the tab listed the whole inbox. The scope
// is three-valued now and the tab asks the one question it names; the default
// asks for the inbox with the snoozes hidden.
test("the Snoozed tab asks for only snoozed notices, and the default hides them", async () => {
  location.hash = "#/inbox?state=snoozed";
  mount();
  await settle();
  // THE LIST'S OWN READ, told from the sidebar badge's count by the scope it
  // names — the badge asks the engine's default, which already hides a snooze.
  const listed = () =>
    asked.filter((q) => q.what === "work_inbox" && "snoozed" in (q.params ?? {}));
  const snoozed = listed().at(-1);
  expect(snoozed?.params).toMatchObject({ snoozed: "only", unread: false });
  expect(snoozed?.params).not.toHaveProperty("include_snoozed");

  cleanup();
  asked = [];
  location.hash = "#/inbox";
  mount();
  await settle();
  const unread = listed().at(-1);
  expect(unread?.params).toMatchObject({ snoozed: "exclude", unread: true });
});

// AN OPERATOR'S CHANGE IS DRAWN AS THE PERSON, NOT THE TOKEN.
//
// A founder's own writes are authored by their credential — `actor` is the
// token's id, which is the audit trail — and the pane printed that id where
// the person belonged. `actor_seat` is who the token is bound to.
test("the detail pane names the person behind an operator's token", async () => {
  location.hash = "#/inbox?row=r-assignee-1";
  mount({
    work_inbox: {
      handle: "ada",
      notices: [
        {
          ...notice("assignee", 1),
          actor: "founder-token",
          actor_kind: "operator",
          actor_seat: "jane-founder",
        },
      ],
      primary_reasons: ["assignee"],
      unread: 1,
      primary: 1,
    },
  });
  await settle();
  expect(screen.getByText(/jane-founder ·/)).toBeTruthy();
  expect(screen.queryByText(/founder-token ·/)).toBeNull();
});

// A PAGE THAT FILLED IS A FLOOR. The band head read "Notices 50" while the
// sidebar badge beside it said "50+": the head counted the rows it drew, and a
// page with more behind it is not all of them.
test("a notices page with more behind it counts itself as a floor", async () => {
  mount({
    work_inbox: {
      handle: "ada",
      notices: [notice("assignee", 1), notice("watcher", 2)],
      primary_reasons: ["assignee"],
      unread: 2,
      primary: 1,
      next_cursor: "c-2",
    },
  });
  await settle();
  const head = screen.getByText("Notices").closest("header");
  expect(head?.querySelector(".inbox-band-count")?.textContent).toBe("2+");
});

// THE DETAIL PANE IS ONE SLOT THE SELECTION MOVES THROUGH, and a write drawn in
// it belongs to the notice it was drawn for: a refusal for one notice was
// still drawn under the next one's Mark read — and its Try again would have
// marked the notice now on screen rather than the one refused.
test("a refusal for one notice is not carried to the next", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            error: "invalid",
            tool: "mark_inbox",
            detail: "read: r-x is no record",
          }),
          { status: 422, headers: { "Content-Type": "application/json" } },
        ),
    ),
  );
  location.hash = "#/inbox?row=r-assignee-1";
  mount({
    viewer: {
      operator_id: "U0FOUNDER",
      operator: true,
      handle: "ada",
      name: "Ada",
      kind: "human",
      acts: ["mark_inbox"],
    },
    work_inbox: {
      handle: "ada",
      notices: [notice("assignee", 1), notice("assignee", 2)],
      primary_reasons: ["assignee"],
      unread: 2,
      primary: 2,
    },
  });
  await settle();
  fireEvent.click(await screen.findByRole("button", { name: "Mark read" }));
  // The refusal drawn under the button — the toast host is an alert region
  // too, now that the rows the decisions band draws need one.
  await waitFor(() =>
    expect(
      screen.getAllByRole("alert").some((a) => a.textContent?.includes("read: r-x is no record")),
    ).toBe(true),
  );
  // Notice 2's excerpt is in the list only, until the pane moves to it.
  const second = () => screen.queryAllByText(/something happened, assignee 2/).length;
  const listed = second();

  await act(async () => {
    location.hash = "#/inbox?row=r-assignee-2";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await settle();
  expect(second()).toBeGreaterThan(listed);
  expect(screen.getByRole("button", { name: "Mark read" })).toBeTruthy();
  expect(document.querySelector(".write-refusal")).toBeNull();
});

function waitingAsk() {
  return {
    kind: "ask",
    at: new Date(Date.now() - 5 * 60_000).toISOString(),
    ask: {
      id: "t-12",
      key: "LEAD-12",
      project: "LEAD",
      title: "2.4 release",
      type: "task",
      status: "todo",
      comment: "c-12",
      asked_by: "cto",
      asked_at: new Date(Date.now() - 5 * 60_000).toISOString(),
      body: "Hold the release?",
      decision: {
        question: "Hold the 2.4 release?",
        options: [
          { id: "hold", label: "Hold release" },
          { id: "ship", label: "Ship anyway" },
        ],
        recommended: "hold",
        role: "approver",
      },
      answer_with: "",
    },
  };
}

// WHAT WAITS ON THIS PERSON IS ON THEIR INBOX, answerable on its row — the
// same read Home's "Waiting on your decision" counts, so the figure that sent
// a reader here and the band they land on cannot disagree.
test("the asks put to the reader are listed and answered on their rows", async () => {
  mount({ decisions: { handle: "ada", items: [waitingAsk()], total: 1, capped: false } });
  await settle();
  expect(screen.getByText(/asks: Hold the 2\.4 release\?/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Hold release" })).toBeTruthy();
  // The notices are still here: this is the whole inbox, not the view.
  expect(screen.getByText("Notices")).toBeTruthy();
});

// `?reason=decisions` IS A VIEW, NOT A NOTICE REASON. As a reason it filtered
// the notice page on a value no notice carries and drew "decisions 0" with
// "Nothing on this page carries that reason" — under a Home figure that had
// just said a decision was waiting.
test("the decisions view sets the notices aside and never filters them on it", async () => {
  location.hash = "#/inbox?reason=decisions";
  mount({
    decisions: { handle: "ada", items: [waitingAsk()], total: 1, capped: false },
    work_inbox: {
      handle: "ada",
      notices: [notice("assignee", 1)],
      primary_reasons: ["assignee"],
      unread: 1,
      primary: 1,
    },
  });
  await settle();
  expect(screen.getByText(/asks: Hold the 2\.4 release\?/)).toBeTruthy();
  expect(screen.queryByText("Nothing on this page carries that reason")).toBeNull();
  expect(screen.queryByText("Notices")).toBeNull();
  expect(screen.queryByText("something happened, assignee 1")).toBeNull();
  expect(screen.getByRole("link", { name: "Show notices" })).toBeTruthy();
});

// A PAGE THAT FILLED IS A FLOOR, and the band says how many lie past it.
test("more decisions than the page holds are counted as a floor and named", async () => {
  mount({ decisions: { handle: "ada", items: [waitingAsk()], total: 4, capped: false } });
  await settle();
  expect(screen.getByText("3 more beyond the 1 newest shown here.")).toBeTruthy();
});
