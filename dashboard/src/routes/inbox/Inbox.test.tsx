/**
 * The Inbox: what a person decides and what reached them, and the gestures
 * they make on it as themselves.
 *
 * Its failure modes are quiet ones. A Done that sent the whole inbox back
 * erased a snooze made in another tab; a row drawn as somebody other than
 * whoever the record names; a Snoozed tab that listed everything; an option
 * button that sent something other than the choice; a snooze preset the
 * engine refuses every time it is pressed; "posts it to #leadership" promised
 * on an ask that promised nothing; a notice about a duplicate that opened its
 * claimant; an unbound reader shown no inbox at all; and a screen drawn whole
 * once a second.
 */

import { Profiler } from "react";
import { act, cleanup, fireEvent, render, screen, within } from "~/test/inCase.ts";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Inbox } from "./Inbox.tsx";
import { PAGE_LOCAL_CHIPS } from "./NoticeList.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { INBOX_SETTLE_MS } from "~/lib/useQuery.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { FrameReadings } from "~/app/Shell.tsx";
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

const EVERY_TOOL = [
  "comment_on_work_item",
  "answer_run",
  "update_work_item",
  "mark_inbox",
  "write_page",
];

const JANE = {
  login: "jane.founder",
  grants: ["state:read", "work:write", "knowledge:write"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: EVERY_TOOL,
};

const ORG = {
  name: "Nimbus",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Maya Ops", handle: "maya", kind: "human" },
    { name: "CTO", handle: "cto", kind: "agent" },
  ],
  units: [],
};

const DECISION = {
  question: "Hold the 2.4 release until the scheduler migration lands?",
  options: [
    { id: "hold", label: "Hold release" },
    { id: "ship", label: "Ship anyway", detail: "Migration ships as a manual step" },
  ],
  recommended: "hold",
  rationale: "The only cost is the webinar slot.",
  role: "approver" as const,
};

function askItem(decision: Record<string, unknown> = DECISION) {
  return {
    kind: "ask",
    at: new Date(Date.now() - 12 * 60_000).toISOString(),
    ask: {
      id: "t-12",
      key: "LEAD-12",
      project: "LEAD",
      title: "Q4 release plan",
      type: "task",
      status: "todo",
      comment: "c-12",
      asked_by: "cto",
      asked_at: new Date(Date.now() - 12 * 60_000).toISOString(),
      body: "2.4 is scheduled for Thursday.",
      decision,
      answer_with: "",
    },
  };
}

function notice(id: string, extra: Record<string, unknown> = {}) {
  return {
    record_id: id,
    log_seq: Number(id.replace(/\D/g, "")) || 1,
    log_stream: "CREWLET_TRACKER_LOG",
    log_generation: 1,
    at: new Date(Date.now() - 60_000).toISOString(),
    reason: "watcher",
    primary: false,
    addressed: false,
    kind: "comment_added",
    subject_id: "t-91",
    subject_key: "PROD-91",
    // A TASK COMMIT'S NOTICE NAMES ITS OWN SUBJECT as its task, as the
    // engine sends it.
    task: "t-91",
    excerpt: `something about ${id}`,
    actor: "cto",
    actor_kind: "agent",
    read: false,
    ...extra,
  };
}

function inboxOf(notices: unknown[], extra: Record<string, unknown> = {}) {
  return { handle: "jane", notices, primary_reasons: [], unread: 0, primary: 0, ...extra };
}

type Answer = unknown | ((params: Record<string, unknown>) => Promise<unknown>);

const QUIET: Record<string, Answer> = {
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  work_inbox: inboxOf([]),
  work_person: {
    handle: "jane",
    version: 1,
    held: true,
    complete: true,
    max_snooze_ahead: 30 * 86_400,
  },
  work_comments: { item: "t", key: "LEAD-12", title: "Q4 release plan", comments: [] },
  sandbox_runs: { runs: [] },
};

let asked: { kind: string; params: Record<string, unknown> }[];
let posted: { tool: string; args: Record<string, unknown> }[];

/**
 * Midday on the company's clock (the fixture org keeps UTC).
 *
 * THE DAY GROUPS CUT AT THE COMPANY'S MIDNIGHT, and every fixture notice is
 * stamped a minute (an ask twelve) before `Date.now()`: on the real clock a run
 * that reached the "Today" case in the first minute after midnight filed its
 * notice under Yesterday and failed. Only `Date` is faked — the suite's own
 * waits keep real timers.
 */
const PINNED_NOW = "2026-09-30T12:00:00Z";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(PINNED_NOW));
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/inbox";
  asked = [];
  posted = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(
        JSON.stringify({
          tool,
          outcome: "applied",
          position: "CREWLET_TRACKER_LOG@1:99",
          receipt: {},
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  localStorage.clear();
  reloadForTest();
  location.hash = "";
});

function mount({
  answers = {},
  viewer = JANE,
  agents = [],
  onCommit = () => {},
}: {
  answers?: Record<string, Answer>;
  viewer?: Record<string, unknown>;
  agents?: Record<string, unknown>[];
  onCommit?: () => void;
} = {}) {
  const store = new Store();
  store.applyHealth({ status: "healthy", nodes: 1 } as never);
  store.applyOrg(ORG as never);
  store.applySeats(agents as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ kind: what, params: params ?? {} });
    const all: Record<string, Answer> = { ...QUIET, viewer, ...answers };
    if (what in all) {
      const answer = all[what];
      return typeof answer === "function"
        ? (answer as (p: Record<string, unknown>) => Promise<unknown>)(params ?? {})
        : Promise.resolve(answer);
    }
    return Promise.resolve({});
  }) as typeof socket.query;
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>
              <Profiler id="inbox" onRender={onCommit}>
                <Inbox />
              </Profiler>
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
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

/** The reason a write control is disabled with, off the kit's described-by. */
function reasonOf(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

describe("the list", () => {
  test("a decision leads the list, and its notice is not listed a second time", async () => {
    mount({
      answers: {
        decisions: { handle: "jane", items: [askItem()], total: 1, capped: false },
        work_inbox: inboxOf([
          notice("r-1", {
            reason: "asked",
            addressed: true,
            ask: { comment: "c-12", asked_of: "jane", open: true },
          }),
          notice("r-2"),
        ]),
      },
    });
    await settle();
    const groups = screen.getAllByRole("region").map((r) => r.getAttribute("aria-label"));
    expect(groups.slice(0, 2)).toEqual(["Needs a decision", "Today"]);
    const today = within(screen.getByRole("region", { name: "Today" }));
    // THE ASK'S NOTICE rides on the decision row; only the other notice is a notice row.
    expect(today.getAllByRole("button")).toHaveLength(1);
    expect(today.getByText("something about r-2")).toBeTruthy();
  });

  // WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person the directory binds to
  // a seat writes AS the seat, so the row is that seat by its name; anybody
  // bound to none writes under their login, a name drawn as one; and what the
  // engine did itself names nobody.
  test("a row is drawn as whoever the record names", async () => {
    mount({
      answers: {
        work_inbox: inboxOf([
          notice("r-1", { actor: "maya", actor_kind: "human" }),
          notice("r-2", { actor: "ci.release", actor_kind: "operator" }),
          notice("r-3", { actor: "", actor_kind: "system" }),
        ]),
      },
    });
    await settle();
    const who = [...document.querySelectorAll(".inbox-row-who")].map((n) => n.textContent);
    expect(who).toEqual(["Maya Ops", "ci.release", "The engine"]);
  });

  // AND THE SENTENCE NAMES THEM AS THE ROW DOES. The engine writes a lead's
  // reorder with the person's HANDLE, for the seat it wakes; the row's head said
  // "Maya Ops" over "maya put PROD-91 at position 1 of your priorities".
  test("a sentence the engine wrote names its author as the row does", async () => {
    mount({
      answers: {
        work_inbox: inboxOf([
          notice("r-1", {
            kind: "prioritised",
            reason: "prioritised",
            subject_kind: "person",
            subject_id: "jane",
            actor: "maya",
            actor_kind: "human",
            excerpt: "maya put PROD-91 at position 1 of your priorities",
          }),
          // A COMMENT IS SOMEBODY'S OWN WORDS, and a handle in it stays.
          notice("r-2", { excerpt: "maya will pick this up" }),
        ]),
      },
    });
    await settle();
    // THE ROW AND THE PANE IT OPENS, both.
    const said = "Maya Ops put PROD-91 at position 1 of your priorities";
    expect(document.querySelector(".inbox-row-line")?.textContent).toBe(said);
    expect(screen.getAllByText(said).length).toBeGreaterThan(0);
    expect(screen.queryByText(/^maya put/)).toBeNull();
    expect(screen.getAllByText("maya will pick this up").length).toBeGreaterThan(0);
  });

  test("the Snoozed scope asks for only what was put off, and lists no decisions", async () => {
    mount({
      answers: {
        decisions: { handle: "jane", items: [askItem()], total: 1, capped: false },
        work_inbox: (p: Record<string, unknown>) =>
          Promise.resolve(
            inboxOf(
              p.snoozed === "only"
                ? [notice("r-9", { snoozed: true, snoozed_until: "2030-01-01T09:00:00Z" })]
                : [notice("r-1")],
            ),
          ),
      },
    });
    await settle();
    // THE SCREEN'S OWN READ, not the frame's badge count beside it.
    const own = () => asked.filter((a) => a.kind === "work_inbox" && !a.params.primary_only);
    const first = own().at(-1)!;
    expect(first.params.snoozed).toBe("exclude");
    fireEvent.click(screen.getByRole("radio", { name: /Snoozed/ }));
    await settle();
    const last = own().at(-1)!;
    expect(last.params.snoozed).toBe("only");
    expect(last.params.unread).toBe(false);
    expect(screen.queryByRole("region", { name: "Needs a decision" })).toBeNull();
    expect(screen.getByText(/Snoozed until/)).toBeTruthy();
    expect(screen.queryByText("something about r-1")).toBeNull();
  });

  test("the chips narrow the rows loaded, and a whole page draws no page note", async () => {
    mount({
      answers: {
        decisions: {
          handle: "jane",
          items: [
            askItem(),
            {
              ...askItem(),
              ask: {
                ...askItem().ask,
                comment: "c-13",
                decision: { ...DECISION, role: "contributor" },
              },
            },
          ],
          total: 2,
          capped: false,
        },
        work_inbox: inboxOf([
          notice("r-1", { reason: "mention" }),
          notice("r-2", { reason: "assignee" }),
        ]),
      },
    });
    await settle();
    const chip = (name: RegExp) => screen.getByRole("radio", { name });
    // THE PAGE IS THE WHOLE (no cursor behind it), so the counts are totals
    // and nothing says otherwise — neither the stray muted line the row once
    // carried under it, nor a description on any chip.
    expect(screen.queryByText(/on this page/)).toBeNull();
    expect(chip(/Mentions/).getAttribute("aria-describedby")).toBeNull();
    expect(chip(/Mentions/).getAttribute("title")).toBeNull();
    expect(chip(/Decisions/).textContent).toContain("2");
    expect(chip(/Reviews/).textContent).toContain("1");
    expect(chip(/Mentions/).textContent).toContain("1");
    fireEvent.click(chip(/Mentions/));
    fireEvent.keyDown(chip(/Mentions/), { key: "Enter" });
    await settle();
    expect(location.hash).toContain("reason=mentions");
    expect(screen.queryByRole("region", { name: "Needs a decision" })).toBeNull();
    const today = within(screen.getByRole("region", { name: "Today" }));
    expect(today.getByText("something about r-1")).toBeTruthy();
    expect(screen.queryByText("something about r-2")).toBeNull();
  });
});

// WHERE THE PAGE STOPPED WITH MORE BEHIND IT the chip counts are the page's,
// and each chip SAYS so — in its accessible description, which is read with it,
// and in its title — rather than in a line of muted text under the row, which
// read like debug output and was read by nobody with the chip.
test("a page with more behind it says so on each chip, not under the row", async () => {
  mount({
    answers: {
      work_inbox: inboxOf([notice("r-1", { reason: "mention" })], { next_cursor: "c-50" }),
    },
  });
  await settle();
  const mentions = screen.getByRole("radio", { name: /Mentions/ });
  expect(mentions.getAttribute("title")).toBe(PAGE_LOCAL_CHIPS);
  const described = mentions.getAttribute("aria-describedby");
  expect(described).toBeTruthy();
  expect(document.getElementById(described!)?.textContent).toBe(PAGE_LOCAL_CHIPS);
  // NOT ONE LINE BESIDE THE ROW: the sentence is only the chips' own.
  expect(screen.queryByText(PAGE_LOCAL_CHIPS, { selector: ":not(.sr-only)" })).toBeNull();
});

// AND A DECISIONS PAGE THAT IS NOT THE WHOLE is the other read the chips count
// over: its Decisions and Reviews counts are the page's too.
test("a decisions page short of its total says so on the chips", async () => {
  mount({
    answers: { decisions: { handle: "jane", items: [askItem()], total: 30, capped: false } },
  });
  await settle();
  const decisions = screen.getByRole("radio", { name: /Decisions/ });
  expect(decisions.getAttribute("title")).toBe(PAGE_LOCAL_CHIPS);
});

// THE UNREAD COUNT IS INSIDE THE UNREAD OPTION, as the approved Inbox draws it
// ("Unread 5") — beside the group it read as a fact about all three — and where
// the page stopped with more behind it the option draws a floor, never an
// exact figure.
describe("the unread count", () => {
  test("is drawn inside the Unread option", async () => {
    mount({ answers: { work_inbox: inboxOf([notice("r-1"), notice("r-2"), notice("r-3")]) } });
    await settle();
    const unread = screen.getByRole("radio", { name: /Unread/ });
    expect(unread.querySelector(".count-chip")?.textContent).toBe("3");
    expect(screen.queryByText(/unread$/)).toBeNull();
  });

  test("is a floor inside the option where the page has more behind it", async () => {
    mount({
      answers: { work_inbox: inboxOf([notice("r-1"), notice("r-2")], { next_cursor: "c-50" }) },
    });
    await settle();
    const unread = screen.getByRole("radio", { name: /Unread/ });
    expect(unread.querySelector(".count-chip")?.textContent).toBe("2+");
    expect(unread.getAttribute("title")).toContain("more lie past this page");
    // AND NOTHING BESIDE THE GROUP: the old "2+ unread" caption is gone.
    expect(screen.queryByText("2+ unread")).toBeNull();
  });
});

describe("the pane", () => {
  test("an option answers the ask's own comment with that choice, and nothing else", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: { decisions: { handle: "jane", items: [askItem()], total: 1, capped: false } },
    });
    await settle();
    expect(screen.getByRole("heading", { name: DECISION.question })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Ship anyway/ }));
    await settle();
    expect(posted).toEqual([
      { tool: "comment_on_work_item", args: { item: "LEAD-12", answers: "c-12", choice: "ship" } },
    ]);
  });

  // THE E-W9 LINE: only an ask that promised a channel says it posts to one.
  test("the posts-to line is drawn only for an ask that promised a channel", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: { decisions: { handle: "jane", items: [askItem()], total: 1, capped: false } },
    });
    await settle();
    expect(screen.queryByText(/posts it to #/)).toBeNull();
    cleanup();
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: {
          handle: "jane",
          items: [askItem({ ...DECISION, inform: { surface: "slack", channel: "leadership" } })],
          total: 1,
          capped: false,
        },
      },
    });
    await settle();
    expect(
      screen.getByText("CTO is woken with your answer and posts it to #leadership."),
    ).toBeTruthy();
    // AND THE COMPOSER SAYS IT TOO, once it is the answer.
    fireEvent.click(screen.getByRole("button", { name: /Reply with instructions/ }));
    await settle();
    expect(screen.getByText("Also posts to Slack #leadership")).toBeTruthy();
  });

  test("Done marks the one notice read and never sends the whole inbox", async () => {
    location.hash = "#/inbox?row=r-2";
    mount({ answers: { work_inbox: inboxOf([notice("r-1"), notice("r-2")]) } });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    await settle();
    expect(posted).toEqual([{ tool: "mark_inbox", args: { read: ["r-2"] } }]);
  });

  test("Mark all read reads through the newest notice loaded, not past it", async () => {
    mount({
      answers: {
        work_inbox: inboxOf([
          notice("r-7", { log_seq: 7 }),
          notice("r-12", { log_seq: 12 }),
          notice("r-3", { log_seq: 3, read: true }),
        ]),
      },
    });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Mark all read" }));
    await settle();
    expect(posted).toEqual([
      { tool: "mark_inbox", args: { read_through: "CREWLET_TRACKER_LOG@1:12" } },
    ]);
  });

  test("a decision that is not a notice cannot be snoozed, and says why", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: { decisions: { handle: "jane", items: [askItem()], total: 1, capped: false } },
    });
    await settle();
    const snooze = screen.getByRole("button", { name: "Snooze" });
    expect(snooze.getAttribute("aria-disabled")).toBe("true");
    expect(reasonOf(snooze)).toContain("This is not a notice");
  });

  test("the thread is read a page at a time and names whoever wrote each reply", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: { handle: "jane", items: [askItem()], total: 1, capped: false },
        work_comments: {
          item: "t-12",
          key: "LEAD-12",
          title: "Q4 release plan",
          comments: [
            {
              id: "c-20",
              task: "t-12",
              author: "maya",
              author_kind: "human",
              body: "I can move the webinar.",
              reply_to: "c-12",
              created_at: new Date().toISOString(),
            },
          ],
          next_cursor: "1:c-19",
        },
      },
    });
    await settle();
    const read = asked.find((a) => a.kind === "work_comments")!;
    expect(read.params).toMatchObject({ item: "LEAD-12", limit: 50 });
    expect(read.params.cursor).toBeUndefined();
    const thread = within(screen.getByRole("region", { name: "Thread" }));
    expect(thread.getByText("Maya Ops")).toBeTruthy();
    expect(thread.queryByText("maya")).toBeNull();
    expect(thread.getByText("Thread · 1 reply loaded")).toBeTruthy();
    fireEvent.click(thread.getByRole("button", { name: "Earlier comments" }));
    await settle();
    expect(asked.some((a) => a.kind === "work_comments" && a.params.cursor === "1:c-19")).toBe(
      true,
    );
  });
});

// THREE VIEWER STATES, three sentences, and every control drawn for each —
// enabled only where the engine makes the change for this person.
describe("who is looking", () => {
  const NOBODY = {
    login: "",
    grants: [],
    handle: "",
    owner: "",
    name: "",
    kind: "",
    acts: [],
  };
  const UNBOUND = {
    login: "ci.release",
    grants: ["state:read", "work:write"],
    handle: "",
    owner: "ci.release",
    name: "",
    kind: "",
    acts: ["mark_inbox"],
  };

  test("an anonymous reader is told to sign in, and nothing is asked on nobody's behalf", async () => {
    const viewer = { ...NOBODY, anonymous: true };
    mount({ viewer, answers: { viewer } });
    await settle();
    expect(screen.getByText(/Nobody is signed in/)).toBeTruthy();
    // NO PERSON'S READS: nothing to ask for on nobody's behalf.
    expect(asked.some((a) => a.kind === "work_inbox" || a.kind === "decisions")).toBe(false);
    const markAll = screen.getByRole("button", { name: "Mark all read" });
    expect(markAll.getAttribute("aria-disabled")).toBe("true");
    expect(reasonOf(markAll)).toContain(WRITE_REASONS.anonymous);
  });

  // AN UNBOUND READER HAS AN INBOX: the record kept under their login, which
  // is what their own assistant's notices and the work that names them reach.
  // Asked by the seat they do not hold, the screen was empty for them.
  test("an unbound reader reads the inbox kept under their login", async () => {
    mount({
      viewer: UNBOUND,
      answers: {
        viewer: UNBOUND,
        work_inbox: inboxOf([notice("r-1", { excerpt: "the release moved" })], {
          handle: "ci.release",
        }),
      },
    });
    await settle();
    expect(screen.getByText(/binds you to no seat/)).toBeTruthy();
    const own = asked.filter((a) => a.kind === "work_inbox" && !a.params.primary_only);
    expect(own.length).toBeGreaterThan(0);
    expect(own.every((a) => a.params.handle === "ci.release")).toBe(true);
    expect(asked.some((a) => a.kind === "decisions")).toBe(true);
    // NO QUESTION NAMES WHOSE RECORD IT IS by a `viewer=`.
    expect(asked.some((a) => "viewer" in a.params)).toBe(false);
    expect(screen.getAllByText("the release moved").length).toBeGreaterThan(0);
    // AND THEY MARK IT AS THEMSELVES.
    const markAll = screen.getByRole("button", { name: "Mark all read" });
    expect(markAll.getAttribute("aria-disabled")).toBeNull();
  });

  test("a bound person the engine does not serve sees every control, disabled with the reason", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      viewer: { ...JANE, acts: [] },
      answers: {
        viewer: { ...JANE, acts: [] },
        decisions: { handle: "jane", items: [askItem()], total: 1, capped: false },
        work_inbox: inboxOf([notice("r-1")]),
      },
    });
    await settle();
    for (const name of ["Mark all read", /Hold release/, "Done"]) {
      const button = screen.getByRole("button", { name });
      expect(button.getAttribute("aria-disabled")).toBe("true");
      expect(reasonOf(button)).toContain(WRITE_REASONS.not_served);
    }
  });
});

// A NOTICE IS ASKED AGAIN THE MOMENT THE READER'S RECORD MOVES. The frame
// watches it and the engine sends `inbox_changed`; a list that waited out its
// poll left a new notice unseen for up to half a minute.
test("a move of the reader's own record asks the list again", async () => {
  const { store } = mount();
  await settle();
  const own = () =>
    asked.filter((a) => a.kind === "work_inbox" && !a.params.primary_only && a.params.handle);
  const before = own().length;
  act(() => {
    store.applyInboxChanged({ handle: "jane" });
  });
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, INBOX_SETTLE_MS + 100));
  });
  await settle();
  expect(own().length).toBeGreaterThan(before);
  expect(own().at(-1)?.params.handle).toBe("jane");
});

/** Seconds of the shared clock, one tick at a time, as the browser runs it. */
function tick(times: number) {
  for (let i = 0; i < times; i++) {
    act(() => {
      vi.advanceTimersByTime(1_000);
    });
  }
}

/** A seat on one round, last heard from `updated`. */
function onOneRound(updated: string) {
  return {
    id: "a",
    agent_id: "id-a",
    role: "Dev A",
    handle: "dev-a",
    kind: "agent",
    activity: "working",
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

// THE SCREEN IS NOT DRAWN ONCE A SECOND.
//
// It held the one-second clock for the attention queue and handed it to every
// row for its "12m": a tick drew both groups, every row and the pane again, on
// a screen a person opens and leaves open. The queue is read as a value now,
// the day groups read only the company's midnight, and each cell reads its own
// words — so ten seconds in which nothing crosses a threshold commit nothing.
test("a tick of the clock draws nothing on the inbox", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  const now = Date.parse("2031-04-16T12:00:00Z");
  vi.setSystemTime(now);
  let commits = 0;
  const { store } = mount({
    answers: {
      // FIVE MINUTES AGO AND MORE, so ten seconds cannot change a row's words:
      // a row whose "4m" turned over would commit honestly, and this case is
      // about the ones that did not.
      work_inbox: inboxOf([
        notice("r-5", { at: new Date(now - 5 * 60_000).toISOString() }),
        notice("r-6", { at: new Date(now - 6 * 60_000).toISOString() }),
      ]),
    },
    onCommit: () => {
      commits += 1;
    },
  });
  // A LIVE ROUND HEARD FROM JUST NOW, so the queue has a condition that reads
  // the clock on every tick and finds nothing to raise.
  act(() => {
    store.applySeats([onOneRound(new Date(now - 1_000).toISOString())] as never);
  });
  await settle();
  expect(screen.getAllByText("something about r-5").length).toBeGreaterThan(0);
  expect(screen.getByText("5m")).toBeTruthy();

  const settled = commits;
  tick(10);
  expect(commits).toBe(settled);
});

// A NOTICE LEADS TO THE TASK IT IS ABOUT, even under a key another task holds.
//
// A notice keeps the key its task held when it was written, and a key two
// tasks hold opens the one that claimed it first — so a notice about the
// duplicate sent its reader to the claimant. The engine says beside the stored
// key when it opens another task, and that notice's link, its thread and its
// reply go by the task's id.
describe("a notice under a key two tasks hold", () => {
  const paneLink = () => document.querySelector<HTMLAnchorElement>("a.inbox-pane-key");

  test("each of two notices under one key leads to its own task", async () => {
    mount({
      answers: {
        work_inbox: inboxOf([
          notice("r-1", {
            subject_id: DUPLICATE,
            subject_key: SHARED_KEY,
            task: DUPLICATE,
            subject_key_collision: true,
            excerpt: "about the duplicate",
          }),
          notice("r-2", {
            subject_id: CLAIMANT,
            subject_key: SHARED_KEY,
            task: CLAIMANT,
            excerpt: "about the claimant",
          }),
        ]),
      },
    });
    await settle();
    fireEvent.click(screen.getAllByText("about the duplicate")[0]!.closest("button")!);
    await settle();
    expect(paneLink()?.textContent).toBe(SHARED_KEY);
    expect(paneLink()?.getAttribute("href")).toBe(DUPLICATE_HREF);
    // ITS THREAD IS THE DUPLICATE'S, read by the address and never the key.
    expect(asked.filter((a) => a.kind === "work_comments").at(-1)?.params.item).toBe(DUPLICATE);

    fireEvent.click(screen.getAllByText("about the claimant")[0]!.closest("button")!);
    await settle();
    expect(paneLink()?.getAttribute("href")).toBe(CLAIMANT_HREF);
    expect(asked.filter((a) => a.kind === "work_comments").at(-1)?.params.item).toBe(SHARED_KEY);
  });

  // A PRIORITISED NOTICE LEADS TO THE TASK, NOT TO THE PERSON WHOSE LIST IT IS:
  // its subject is that PERSON and the key beside it is the task's, so read off
  // the subject a duplicate at the top of somebody's list sent its reader to
  // `#/work/<their handle>`.
  test("a prioritised notice leads to the task it put first", async () => {
    const prioritised = (id: string, task: string, excerpt: string, collision?: boolean) =>
      notice(id, {
        kind: "prioritised",
        reason: "prioritised",
        primary: true,
        subject_id: "jane",
        subject_key: SHARED_KEY,
        task,
        subject_key_collision: collision,
        excerpt,
      });
    mount({
      answers: {
        work_inbox: inboxOf([
          prioritised("r-1", DUPLICATE, "the duplicate is first", true),
          prioritised("r-2", CLAIMANT, "the claimant is first"),
        ]),
      },
    });
    await settle();
    fireEvent.click(screen.getAllByText("the duplicate is first")[0]!.closest("button")!);
    await settle();
    expect(paneLink()?.getAttribute("href")).toBe(DUPLICATE_HREF);
    fireEvent.click(screen.getAllByText("the claimant is first")[0]!.closest("button")!);
    await settle();
    expect(paneLink()?.getAttribute("href")).toBe(CLAIMANT_HREF);
  });

  // AN ANSWER NAMES THE ASK'S TASK BY ITS ADDRESS: by the key, a choice made on
  // a duplicate's ask was a comment on the claimant.
  test("an option on a duplicate's ask answers on the duplicate", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    const ask = askItem();
    mount({
      answers: {
        decisions: {
          handle: "jane",
          items: [
            {
              ...ask,
              ask: { ...ask.ask, id: DUPLICATE, key: SHARED_KEY, key_collision: true },
            },
          ],
          total: 1,
          capped: false,
        },
      },
    });
    await settle();
    expect(paneLink()?.getAttribute("href")).toBe(DUPLICATE_HREF);
    fireEvent.click(screen.getByRole("button", { name: /Ship anyway/ }));
    await settle();
    expect(posted).toEqual([
      { tool: "comment_on_work_item", args: { item: DUPLICATE, answers: "c-12", choice: "ship" } },
    ]);
  });
});
