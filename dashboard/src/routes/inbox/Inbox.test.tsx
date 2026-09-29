/**
 * The Inbox: what a person decides and what reached them, and the gestures
 * they make on it as themselves.
 *
 * Its failure modes are quiet ones. A Done that sent the whole inbox back
 * erased a snooze made in another tab; a row drawn with the token's id put a
 * credential where a person belongs; a Snoozed tab that listed everything;
 * an option button that sent something other than the choice; a snooze preset
 * the engine refuses every time it is pressed; and "posts it to #leadership"
 * promised on an ask that promised nothing.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Inbox } from "./Inbox.tsx";
import { PAGE_LOCAL_CHIPS } from "./NoticeList.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { reloadForTest } from "~/lib/prefs.ts";
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

const EVERY_TOOL = [
  "comment_on_work_item",
  "answer_run",
  "update_work_item",
  "mark_inbox",
  "write_page",
];

const JANE = {
  operator_id: "U0FOUNDER",
  operator: true,
  handle: "jane",
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
}: {
  answers?: Record<string, Answer>;
  viewer?: Record<string, unknown>;
  agents?: Record<string, unknown>[];
} = {}) {
  const store = new Store();
  store.applyHealth({ status: "healthy", nodes: 1 } as never);
  store.applyOrg(ORG as never);
  store.applyAgents(agents as never);
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

  // THE PERSON, NEVER THE CREDENTIAL.
  test("a row is drawn as the seat behind a token, never as the token", async () => {
    mount({
      answers: {
        work_inbox: inboxOf([
          notice("r-1", { actor: "U0MAYA", actor_kind: "operator", actor_seat: "maya" }),
          notice("r-2", { actor: "U0ANON", actor_kind: "operator" }),
        ]),
      },
    });
    await settle();
    expect(screen.getAllByText("Maya Ops").length).toBeGreaterThan(0);
    expect(screen.getByText("An operator")).toBeTruthy();
    expect(screen.queryByText("U0MAYA")).toBeNull();
    expect(screen.queryByText("U0ANON")).toBeNull();
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
            actor: "U0MAYA",
            actor_kind: "operator",
            actor_seat: "maya",
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

  test("the thread is read a page at a time and names the person behind a token", async () => {
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
              author: "U0MAYA",
              author_kind: "operator",
              body: "I can move the webinar.",
              reply_to: "c-12",
              created_at: new Date().toISOString(),
            },
          ],
          comment_seats: { "c-20": "maya" },
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
    expect(thread.queryByText("U0MAYA")).toBeNull();
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
  const NOBODY = { operator_id: "", operator: false, handle: "", name: "", kind: "", acts: [] };
  const UNBOUND = { operator_id: "ci", operator: true, handle: "", name: "", kind: "", acts: [] };

  test.each([
    [
      "an anonymous reader",
      { ...NOBODY, anonymous: true },
      "No API token is presented",
      WRITE_REASONS.anonymous,
    ],
    ["an unbound token", UNBOUND, "no seat claims it", WRITE_REASONS.unbound],
  ])("%s is told what would make this their inbox", async (_who, viewer, sentence, reason) => {
    mount({ viewer, answers: { viewer } });
    await settle();
    expect(screen.getByText(new RegExp(sentence))).toBeTruthy();
    // NO PERSON'S READS: nothing to ask for on nobody's behalf.
    expect(asked.some((a) => a.kind === "work_inbox" || a.kind === "decisions")).toBe(false);
    const markAll = screen.getByRole("button", { name: "Mark all read" });
    expect(markAll.getAttribute("aria-disabled")).toBe("true");
    expect(reasonOf(markAll)).toContain(reason);
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
