/**
 * The Inbox: what a person decides and what reached them, read one item at a
 * time beside the list, and the gestures they make on it as themselves.
 *
 * Its failure modes are quiet ones. A mark that sent the whole inbox back
 * erased a snooze made in another tab; a row drawn as anybody but whoever the
 * record names put a stranger where a person belongs; a link on a key another
 * task claimed first opened that task; a Snoozed list that listed everything;
 * an option that sent something other than the choice, or answered beside the
 * one write every surface answers through; an answered question that came
 * back as an update; and "posted to #leadership" promised on an ask that
 * promised nothing.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { ADVANCE_MS, Inbox } from "./Inbox.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { FrameReadings } from "~/app/Shell.tsx";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { settled } from "~/test/settle.ts";

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
  "code_mark_inbox",
  "write_page",
];

const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "tracker", "knowledge"],
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
    kind: "comment",
    subject_id: "t-91",
    subject_key: "PROD-91",
    excerpt: `something about ${id}`,
    actor: "cto",
    actor_kind: "agent",
    read: false,
    ...extra,
  };
}

/**
 * A notice as the composed inbox carries it: a code notice (`codeNotice`) as
 * it is, and any other as the tracker's, its own shape whole in `extra`.
 */
function composedOf(n: Record<string, unknown>): Record<string, unknown> {
  if (n.source === "code") return n;
  return {
    source: "tracker",
    record_id: n.record_id,
    position: { stream: n.log_stream, generation: n.log_generation, seq: n.log_seq },
    at: n.at,
    reason: n.reason,
    primary: n.primary,
    addressed: n.addressed,
    kind: n.kind,
    subject: { id: n.subject_id, key: n.subject_key },
    excerpt: n.excerpt,
    actor: n.actor,
    actor_kind: n.actor_kind,
    read: n.read,
    snoozed: n.snoozed,
    snoozed_until: n.snoozed_until,
    extra: n,
  };
}

/** A notice the code host routed, as the composed inbox carries it. */
function codeNotice(id: string, extra: Record<string, unknown> = {}) {
  const n = {
    record_id: id,
    log_seq: Number(id.replace(/\D/g, "")) || 1,
    log_stream: "CREWLET_CODE_LOG",
    log_generation: 1,
    at: new Date(Date.now() - 30_000).toISOString(),
    reason: "review_requested",
    primary: true,
    addressed: true,
    repo: "web",
    change_id: "change-42",
    change_ref: "web!42",
    excerpt: "Retry the upload once, not twice.",
    change: { title: "Retry the upload once", state: "open" },
    read: false,
    ...extra,
  };
  return {
    source: "code",
    record_id: id,
    position: { stream: n.log_stream, generation: n.log_generation, seq: n.log_seq },
    at: n.at,
    reason: n.reason,
    primary: n.primary,
    addressed: n.addressed,
    kind: "change",
    subject: { id: n.change_id, key: n.change_ref, title: n.change.title },
    excerpt: n.excerpt,
    read: n.read,
    snoozed: (n as { snoozed?: boolean }).snoozed,
    extra: n,
  };
}

/** A composed `inbox` page of notices, every source read. */
function inboxOf(notices: unknown[], extra: Record<string, unknown> = {}) {
  return {
    handle: "jane",
    notices: (notices as Record<string, unknown>[]).map(composedOf),
    sources: [
      { name: "tracker", primary_reasons: [] },
      { name: "code", primary_reasons: [] },
    ],
    unread: 0,
    primary: 0,
    ...extra,
  };
}

type Answer = unknown | ((params: Record<string, unknown>) => Promise<unknown>);

const QUIET: Record<string, Answer> = {
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  inbox: inboxOf([]),
  work_person: {
    handle: "jane",
    version: 1,
    held: true,
    complete: true,
    max_snooze_ahead: 30 * 86_400,
  },
  work_comments: { item: "t", key: "LEAD-12", title: "Q4 release plan", comments: [] },
  work_items: { items: [], total_hint: 0 },
  sandbox_runs: { runs: [] },
};

let asked: { kind: string; params: Record<string, unknown> }[];
let posted: { tool: string; args: Record<string, unknown> }[];

/**
 * Midday on the company's clock (the fixture org keeps UTC).
 *
 * Every fixture notice is stamped a minute (an ask twelve) before
 * `Date.now()`, so the time a row draws and the day a snooze preset lands on
 * are the same on every run. Only `Date` is faked here; the one case about
 * the moment before the next item opens fakes its timer as well.
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
          // EACH TOOL ANSWERS ON ITS OWN DOMAIN'S LOG, as the engine does.
          position:
            tool === "code_mark_inbox" ? "CREWLET_CODE_LOG@1:99" : "CREWLET_TRACKER_LOG@1:99",
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
  services,
}: {
  answers?: Record<string, Answer>;
  viewer?: Record<string, unknown>;
  agents?: Record<string, unknown>[];
  services?: Record<string, string>;
} = {}) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(withDerived(services ? { ...ORG, services } : ORG) as never);
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
              <Inbox />
            </Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { store, socket };
}

/**
 * Lets everything the screen asked land: every read the socket was asked and
 * every write the act route took, and what their answers drew, including the
 * reads those renders asked in turn (see `settled`).
 */
async function settle() {
  await settled(
    () => asked.length,
    () => posted.length,
  );
}

/** The reason a write control is disabled with, off the kit's described-by. */
function reasonOf(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

/** An `inbox` answer per question: Updates, Snoozed and Earlier ask apart. */
function inboxes({
  unread = [],
  snoozed = [],
  all,
}: {
  unread?: unknown[];
  snoozed?: unknown[];
  all?: unknown[];
}): Answer {
  return (p: Record<string, unknown>) =>
    Promise.resolve(
      inboxOf(p.snoozed === "only" ? snoozed : p.unread === false ? (all ?? unread) : unread),
    );
}

const ASKED = {
  reason: "asked",
  addressed: true,
  primary: true,
  ask: { comment: "c-12", asked_of: "jane", open: true },
};

function decisionsOf(...items: unknown[]) {
  return { handle: "jane", items, total: items.length, capped: false };
}

/** The open item's one-sentence byline, as it reads. */
function byline(): string {
  return document.querySelector(".crewlet-reading-pane__byline")?.textContent ?? "";
}

describe("the list", () => {
  test("a decision leads the list under Needs you, and its notice is not listed again", async () => {
    mount({
      answers: {
        decisions: decisionsOf(askItem()),
        inbox: inboxes({ unread: [notice("r-1", ASKED), notice("r-2")] }),
      },
    });
    await settle();
    const list = within(screen.getByRole("navigation", { name: "Inbox" }));
    const needs = within(list.getByRole("region", { name: "Needs you" }));
    const updates = within(list.getByRole("region", { name: "Updates" }));
    expect(needs.getAllByRole("listitem")).toHaveLength(1);
    // THE ASK'S NOTICE rides on its row; only the other notice is an update.
    expect(updates.getAllByRole("listitem")).toHaveLength(1);
    expect(updates.getByText("something about r-2")).toBeTruthy();
  });

  // NO FILTER BAR: what was snoozed and what was read are two words at the end.
  test("Snoozed and Earlier are two links at the end, each read when it is opened", async () => {
    mount({
      answers: {
        inbox: inboxes({
          unread: [notice("r-1")],
          snoozed: [notice("r-9", { snoozed: true, snoozed_until: "2030-01-01T09:00:00Z" })],
          all: [notice("r-1"), notice("r-5", { read: true, excerpt: "an old one" })],
        }),
      },
    });
    await settle();
    expect(screen.queryByRole("radio")).toBeNull();
    const reads = () => asked.filter((a) => a.kind === "inbox").map((a) => a.params);
    expect(reads().some((p) => p.unread === true && p.snoozed === "exclude")).toBe(true);
    expect(reads().some((p) => p.snoozed === "only")).toBe(true);
    // EARLIER IS NOT READ until somebody opens it.
    expect(reads().some((p) => p.unread === false)).toBe(false);

    const snoozed = screen.getByRole("button", { name: "1 snoozed" });
    expect(snoozed.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText("something about r-9")).toBeNull();
    fireEvent.click(snoozed);
    await settle();
    expect(snoozed.getAttribute("aria-expanded")).toBe("true");
    expect(
      within(screen.getByRole("region", { name: "Snoozed" })).getByText("something about r-9"),
    ).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Earlier updates" }));
    await settle();
    expect(reads().some((p) => p.unread === false && p.snoozed === "exclude")).toBe(true);
    const earlier = within(screen.getByRole("region", { name: "Earlier updates" }));
    expect(earlier.getByText("an old one")).toBeTruthy();
    // THE UNREAD ONE IS AN UPDATE ALREADY, and is not listed twice.
    expect(earlier.queryByText("something about r-1")).toBeNull();
  });

  // FROM "SHOW MORE" THE FIRST PAGE IS HELD: the next page starts after the
  // last notice the first was answered with, and the first read again under a
  // newer notice pushes that one out of both — so neither its poll, an inbox
  // push nor a reconnect asks it again. Mutation: ask it without the pager's
  // hold, and the tick asks it again.
  test("after Show more, a poll, a push and a reconnect ask nothing of Earlier's first page", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    try {
      const { store } = mount({
        answers: {
          inbox: (p: Record<string, unknown>) =>
            Promise.resolve(
              p.unread !== false
                ? inboxOf([])
                : p.cursor === "e-2"
                  ? inboxOf([notice("r-1", { read: true, excerpt: "the oldest one" })])
                  : inboxOf(
                      [
                        notice("r-3", { read: true, excerpt: "a newer one" }),
                        notice("r-2", { read: true, excerpt: "an older one" }),
                      ],
                      { next_cursor: "e-2" },
                    ),
            ),
        },
      });
      await settle();
      fireEvent.click(screen.getByRole("button", { name: "Earlier updates" }));
      await settle();
      fireEvent.click(screen.getByRole("button", { name: "Show more" }));
      await settle();
      const earlier = within(screen.getByRole("region", { name: "Earlier updates" }));
      expect(earlier.getByText("the oldest one")).toBeTruthy();
      const firsts = () =>
        asked.filter((a) => a.kind === "inbox" && a.params.unread === false && !a.params.cursor)
          .length;
      const before = firsts();
      await act(async () => void vi.advanceTimersByTime(5 * 30_000));
      act(() => store.applyInboxChanged({ handle: "jane" }));
      act(() => store.setConnected(false));
      act(() => store.setConnected(true));
      await settle();
      expect(firsts()).toBe(before);
      expect(earlier.getByText("a newer one")).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });

  // WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person the directory binds
  // to a seat writes AS the seat, so their row draws as the seat, never as the
  // credential they wrote through; a principal bound to none writes under its
  // own login, which is a name and is drawn as one; and only the engine's own
  // change names nobody.
  test("a row is drawn as the seat a person writes as, and an unbound principal as its login", async () => {
    mount({
      answers: {
        inbox: inboxes({
          unread: [
            notice("r-1", { actor: "maya", actor_kind: "human", operator_id: "pat:t-1" }),
            notice("r-2", { actor: "ci.bot", actor_kind: "operator" }),
            notice("r-3", { actor: "crewlet", actor_kind: "system" }),
          ],
        }),
      },
    });
    await settle();
    expect(screen.getAllByText("Maya Ops").length).toBeGreaterThan(0);
    expect(screen.getAllByText("ci.bot").length).toBeGreaterThan(0);
    expect(screen.getAllByText("The engine").length).toBeGreaterThan(0);
    expect(screen.queryByText("pat:t-1")).toBeNull();
    expect(screen.queryByText("An operator")).toBeNull();
  });

  // AND THE SENTENCE NAMES THEM AS THE ROW DOES. The engine writes a lead's
  // reorder with the person's HANDLE, for the seat it wakes.
  test("a sentence the engine wrote names its author as the row does", async () => {
    mount({
      answers: {
        inbox: inboxes({
          unread: [
            notice("r-1", {
              kind: "prioritised",
              reason: "prioritised",
              primary: true,
              subject_kind: "person",
              subject_id: "jane",
              actor: "maya",
              actor_kind: "human",
              excerpt: "maya put PROD-91 at position 1 of your priorities",
            }),
            // A COMMENT IS SOMEBODY'S OWN WORDS, and a handle in it stays.
            notice("r-2", { excerpt: "maya will pick this up" }),
          ],
        }),
      },
    });
    await settle();
    const said = "Maya Ops put PROD-91 at position 1 of your priorities";
    expect(document.querySelector(".crewlet-inbox-row__excerpt")?.textContent).toBe(said);
    expect(screen.queryByText(/^maya put/)).toBeNull();
    expect(screen.getAllByText("maya will pick this up").length).toBeGreaterThan(0);
  });

  test("each row names its task, as a link that opens it beside the inbox", async () => {
    mount({
      answers: {
        inbox: inboxes({ unread: [notice("r-1")] }),
        work_items: {
          items: [{ id: "t-91", key: "PROD-91", title: "Ledger export" }],
          total_hint: 1,
        },
      },
    });
    await settle();
    // ONE READ for the titles of every row that names its task by key alone.
    const read = asked.filter((a) => a.kind === "work_items").at(-1)!;
    expect(read.params).toMatchObject({ key: "PROD-91", show_closed: true });
    const list = within(screen.getByRole("navigation", { name: "Inbox" }));
    const task = list.getByRole("link", { name: "PROD-91 Ledger export" });
    // A REAL LINK, so a modified click opens the task's own page in a tab.
    expect(task.getAttribute("href")).toBe("#/work/PROD-91");
    // AND A QUIET ONE, in the kit's own ink for a reference among other
    // words: a column of full accent links read as a column of links.
    expect(task.className).toContain("crewlet-link--quiet");
    // AND NOT INSIDE THE ROW'S OWN BUTTON, which would make it unreachable.
    expect(task.closest("button")).toBeNull();
    fireEvent.click(task);
    await settle();
    expect(decodeURIComponent(location.hash)).toContain("peek=item:PROD-91");
  });

  // A KEY TWO TASKS HOLD OPENS THE ONE THAT CLAIMED IT, so a notice about the
  // other is reached by its task's id, and titled by it: the title read is by
  // key, and answers both.
  test("a row on a key another task claimed first opens and names its own task", async () => {
    mount({
      answers: {
        inbox: inboxes({
          unread: [notice("r-1", { task: "t-dup", subject_key_collision: true })],
        }),
        work_items: {
          // THE CLAIMANT LAST, so a title filed by key alone would be its.
          items: [
            { id: "t-dup", key: "PROD-91", key_collision: true, title: "The duplicate" },
            { id: "t-91", key: "PROD-91", title: "The claimant" },
          ],
          total_hint: 2,
        },
      },
    });
    await settle();
    const list = within(screen.getByRole("navigation", { name: "Inbox" }));
    const task = list.getByRole("link", { name: "PROD-91 The duplicate" });
    expect(task.getAttribute("href")).toBe("#/work/t-dup");
    expect(list.queryByText(/The claimant/)).toBeNull();
  });

  // THE OPEN ROW WAS DRAWN TWICE: a tint on the row and a rail beside it.
  test("the open row is the one row marked current, with one highlight", async () => {
    location.hash = "#/inbox?row=r-2";
    mount({ answers: { inbox: inboxes({ unread: [notice("r-1"), notice("r-2")] }) } });
    await settle();
    const list = screen.getByRole("navigation", { name: "Inbox" });
    const current = list.querySelectorAll("[aria-current]");
    expect(current).toHaveLength(1);
    expect(current[0]!.textContent).toContain("something about r-2");
    expect(list.querySelectorAll(".crewlet-inbox-row--selected")).toHaveLength(1);
  });

  test("j and k step through the rows, opening each in the pane", async () => {
    location.hash = "#/inbox?row=r-1";
    mount({
      answers: {
        inbox: inboxes({ unread: [notice("r-1"), notice("r-2"), notice("r-3")] }),
      },
    });
    await settle();
    fireEvent.keyDown(window, { key: "j" });
    await settle();
    expect(location.hash).toContain("row=r-2");
    fireEvent.keyDown(window, { key: "j" });
    fireEvent.keyDown(window, { key: "k" });
    await settle();
    expect(location.hash).toContain("row=r-2");
  });
});

describe("the pane", () => {
  test("the byline is one sentence: who, what is asked of you, and the task", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({ answers: { decisions: decisionsOf(askItem()) } });
    await settle();
    expect(byline()).toBe("CTO asks for your approval on LEAD-12 Q4 release plan");
    // THE ROLE LINE IT REPLACED, with its doubled info glyph, is gone.
    expect(screen.queryByText(/You are the/)).toBeNull();
    expect(screen.getByRole("heading", { name: DECISION.question })).toBeTruthy();
    // AND THE REASONING SITS WITH WHAT IT RECOMMENDS.
    expect(screen.getByText("The only cost is the webinar slot.")).toBeTruthy();
  });

  test("an option answers with one press, through the one write, and marks its notice read", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: decisionsOf(askItem()),
        inbox: inboxes({ unread: [notice("r-1", ASKED)] }),
      },
    });
    await settle();
    // ONE SET OF ANSWERS, AnswerAsk's: the pane draws no options of its own.
    expect(screen.getAllByRole("group", { name: "Answers" })).toHaveLength(1);
    const options = within(screen.getByRole("group", { name: "Answers" })).getAllByRole("button");
    // THE RECOMMENDED ONE FIRST, and marked.
    expect(options[0]!.textContent).toBe("Hold release Recommended");
    fireEvent.click(screen.getByRole("button", { name: /Ship anyway/ }));
    await settle();
    expect(posted).toEqual([
      { tool: "comment_on_work_item", args: { item: "LEAD-12", answers: "c-12", choice: "ship" } },
      // AN ANSWERED QUESTION DOES NOT COME BACK AS AN UPDATE.
      { tool: "mark_inbox", args: { read: ["r-1"] } },
    ]);
    expect(document.querySelector(".crewlet-reading-pane__outcome")?.textContent).toBe(
      "Answered Ship anyway",
    );
  });

  test("Something else answers in words, in the options' place", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({ answers: { decisions: decisionsOf(askItem()) } });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Something else" }));
    await settle();
    expect(screen.queryByRole("group", { name: "Answers" })).toBeNull();
    fireEvent.change(screen.getByRole("textbox", { name: "Reply to CTO" }), {
      target: { value: "Hold it a week." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await settle();
    expect(posted).toEqual([
      {
        tool: "comment_on_work_item",
        args: { item: "LEAD-12", answers: "c-12", body: "Hold it a week." },
      },
    ]);
  });

  test("an answered item stays in its place as done, and the next one opens a moment later", async () => {
    const second = {
      ...askItem(),
      at: new Date(Date.now() - 60 * 60_000).toISOString(),
      ask: { ...askItem().ask, comment: "c-13", question: undefined },
    };
    // The moment before the next item opens runs on the case's clock, so
    // "a moment later" is held exactly: not before ADVANCE_MS, and then.
    vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"], now: new Date(PINNED_NOW) });
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({ answers: { decisions: decisionsOf(askItem(), second) } });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: /Hold release/ }));
    await settle();
    // WHAT HAPPENED, FIRST: the open item shows its answer, and its row is
    // struck through and read as answered.
    expect(location.hash).toContain("row=ask%3Ac-12");
    const needs = within(screen.getByRole("region", { name: "Needs you" }));
    expect(needs.getAllByRole("button", { name: /Answered$/ })).toHaveLength(1);
    act(() => {
      vi.advanceTimersByTime(ADVANCE_MS - 1);
    });
    expect(location.hash).toContain("row=ask%3Ac-12");
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(location.hash).toContain("row=ask%3Ac-13");
  });

  // THE E-W9 LINE: only an ask that promised a channel says it posts to one.
  test("the posts-to line is drawn only for an ask that promised a channel", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({ answers: { decisions: decisionsOf(askItem()) } });
    await settle();
    expect(screen.queryByText(/also posted to #/)).toBeNull();
    cleanup();
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: decisionsOf(
          askItem({ ...DECISION, inform: { surface: "slack", channel: "leadership" } }),
        ),
      },
    });
    await settle();
    expect(screen.getByText("Your answer is also posted to #leadership.")).toBeTruthy();
  });

  test("Mark as read marks the one update, and turns into its undo", async () => {
    location.hash = "#/inbox?row=r-2";
    mount({ answers: { inbox: inboxes({ unread: [notice("r-1"), notice("r-2")] }) } });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Mark as read" }));
    await settle();
    expect(posted).toEqual([{ tool: "mark_inbox", args: { read: ["r-2"] } }]);
    expect(screen.getByRole("button", { name: "Mark as unread" })).toBeTruthy();
  });

  // DONE AND SNOOZE ARE AN UPDATE'S: a question leaves when it is answered.
  test("a question carries no Mark as read and no Snooze", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({ answers: { decisions: decisionsOf(askItem()) } });
    await settle();
    expect(screen.queryByRole("button", { name: "Mark as read" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Snooze/ })).toBeNull();
  });

  // A CODE NOTICE IS MARKED THROUGH THE CODE HOST'S OWN TOOL: each domain
  // keeps the marks over its own notices on its own log. Mutation: mark every
  // update with mark_inbox — the code notice's read lands on the tracker's
  // record, red.
  test("a code notice is listed beside the tracker's and marked through its own tool", async () => {
    location.hash = "#/inbox?row=change%3Ac-5";
    mount({
      services: { tracker: "native", knowledge: "native", code: "native" },
      answers: { inbox: inboxes({ unread: [codeNotice("c-5"), notice("r-1")] }) },
    });
    await settle();
    const updates = within(screen.getByRole("region", { name: "Updates" }));
    expect(updates.getAllByRole("listitem")).toHaveLength(2);
    expect(updates.getByText("Retry the upload once")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Mark as read" }));
    await settle();
    expect(posted).toEqual([{ tool: "code_mark_inbox", args: { read: ["c-5"] } }]);
  });

  // MARK ALL READ SENDS BOTH: one read-through a source, each at its own
  // newest notice. Mutation: read every source through the newest notice of
  // any — the code host is sent a tracker position, red.
  test("Mark all read reads each source through its own newest update", async () => {
    mount({
      services: { tracker: "native", knowledge: "native", code: "native" },
      answers: {
        inbox: inboxes({
          unread: [codeNotice("c-5"), notice("r-12", { log_seq: 12 }), codeNotice("c-2")],
        }),
      },
    });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Mark all read" }));
    await settle();
    expect(posted).toEqual([
      { tool: "mark_inbox", args: { read_through: "CREWLET_TRACKER_LOG@1:12" } },
      { tool: "code_mark_inbox", args: { read_through: "CREWLET_CODE_LOG@1:5" } },
    ]);
  });

  test("Mark all read reads through the newest update loaded, not past it", async () => {
    mount({
      answers: {
        inbox: inboxes({
          unread: [
            notice("r-7", { log_seq: 7 }),
            notice("r-12", { log_seq: 12 }),
            notice("r-3", { log_seq: 3, read: true }),
          ],
        }),
      },
    });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Mark all read" }));
    await settle();
    expect(posted).toEqual([
      { tool: "mark_inbox", args: { read_through: "CREWLET_TRACKER_LOG@1:12" } },
    ]);
  });

  // THE HEADING WAS THE WHOLE COMMENT, cut at its first line.
  test("an update's heading says what it did, and a comment is quoted under it", async () => {
    const long =
      "Can you check the rounding on the last line? It drops a cent when three people split ten dollars, and the fix touches the ledger.";
    location.hash = "#/inbox?row=r-1";
    mount({
      answers: {
        inbox: inboxes({
          unread: [
            notice("r-1", { reason: "mention", primary: true, comment_id: "c-40", excerpt: long }),
          ],
        }),
      },
    });
    await settle();
    expect(screen.getByRole("heading", { name: "Mentioned you" })).toBeTruthy();
    expect(document.querySelector("blockquote")?.textContent).toBe(long);
    expect(byline()).toBe("CTO on PROD-91");
    expect(screen.getByRole("region", { name: "Replies to this comment" })).toBeTruthy();
  });

  test("an update that only informs says why it reached you", async () => {
    location.hash = "#/inbox?row=r-1";
    mount({
      answers: {
        inbox: inboxes({ unread: [notice("r-1", { kind: "status", excerpt: "" })] }),
      },
    });
    await settle();
    expect(screen.getByRole("heading", { name: "Changed its status" })).toBeTruthy();
    expect(byline()).toBe("CTO on PROD-91 · you follow this task");
    expect(screen.getByRole("region", { name: "Conversation on this task" })).toBeTruthy();
  });

  test("the conversation is read a page at a time and names each comment's author", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: decisionsOf(askItem()),
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
    const thread = within(screen.getByRole("region", { name: "Replies to this question" }));
    expect(thread.getByText("Maya Ops")).toBeTruthy();
    expect(thread.queryByText("maya")).toBeNull();
    // THE REPLY BOX, at the end, for words that answer nothing.
    expect(
      thread.getByPlaceholderText("Reply without answering. @mention to bring someone in."),
    ).toBeTruthy();
    fireEvent.click(thread.getByRole("button", { name: "Earlier comments" }));
    await settle();
    expect(asked.some((a) => a.kind === "work_comments" && a.params.cursor === "1:c-19")).toBe(
      true,
    );
  });

  // A QUESTION THE READER CAN'T SEE IS NOT ASKED OF THEM: the pane asked for
  // an answer to a question it never showed. The CONTROL is the case below,
  // whose question is sent and answered. Mutation: draw the form whatever
  // was withheld.
  test("a parked run whose question is hidden offers no answer form", async () => {
    const run = {
      kind: "run",
      at: new Date(Date.now() - 5 * 60_000).toISOString(),
      run: {
        turn_id: "turn-8",
        agent_handle: "cto",
        status: "paused",
        body_withheld: true,
        paused_at: new Date(Date.now() - 5 * 60_000).toISOString(),
        updated_at: new Date(Date.now() - 5 * 60_000).toISOString(),
        pause_ttl_seconds: 3_600,
      },
    };
    location.hash = "#/inbox?row=run%3Aturn-8";
    mount({ answers: { decisions: decisionsOf(run) } });
    await settle();
    expect(
      screen.getByRole("heading", { name: "This run is waiting on a question you can’t see." }),
    ).toBeTruthy();
    expect(screen.getByText("Its lead or an admin can answer it.")).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: "Your answer to the run" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Send answer" })).toBeNull();
  });

  test("a parked run's question heads the pane, quoted whole, and is answered by its turn", async () => {
    const run = {
      kind: "run",
      at: new Date(Date.now() - 5 * 60_000).toISOString(),
      run: {
        turn_id: "turn-7",
        agent_handle: "cto",
        status: "paused",
        question: "May I delete the fixtures directory? It is regenerated by make fixtures.",
        paused_at: new Date(Date.now() - 5 * 60_000).toISOString(),
        updated_at: new Date(Date.now() - 5 * 60_000).toISOString(),
        pause_ttl_seconds: 3_600,
        work_item: { backend: "native", id: "t-12", key: "LEAD-12", project: "LEAD" },
      },
    };
    location.hash = "#/inbox?row=run%3Aturn-7";
    mount({ answers: { decisions: decisionsOf(run) } });
    await settle();
    expect(
      screen.getByRole("heading", { name: "May I delete the fixtures directory?" }),
    ).toBeTruthy();
    expect(document.querySelector("blockquote")?.textContent).toBe(run.run.question);
    expect(byline()).toBe("CTO’s coding run on LEAD-12");
    // A TURN'S TASK IS OPENED BY ITS ID: the turn's reference carries no
    // collision flag, and a key another task claimed first opens that task.
    const pane = document.querySelector(".crewlet-reading-pane__byline") as HTMLElement;
    expect(within(pane).getByRole("link").getAttribute("href")).toBe("#/work/t-12");
    fireEvent.change(screen.getByRole("textbox", { name: "Your answer to the run" }), {
      target: { value: "Yes, delete it." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
    await settle();
    expect(posted).toEqual([
      { tool: "answer_run", args: { turn_id: "turn-7", answer: "Yes, delete it." } },
    ]);
  });

  // NO EM DASH SHIPS: the copy the inbox draws is written with commas,
  // parentheses and full stops.
  test("nothing the inbox draws is written with an em dash", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: decisionsOf(
          askItem({ ...DECISION, inform: { surface: "slack", channel: "leadership" } }),
        ),
        inbox: inboxes({
          unread: [notice("r-1"), notice("r-2", { kind: "status", excerpt: "" })],
          snoozed: [notice("r-9", { snoozed: true, snoozed_until: "2030-01-01T09:00:00Z" })],
        }),
      },
    });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "1 snoozed" }));
    await settle();
    expect(document.body.textContent).not.toContain("\u2014");
  });
});

// THREE VIEWER STATES, three sentences, and every control drawn for each,
// enabled only where the engine makes the change for this person.
describe("who is looking", () => {
  const NOBODY = { login: "", grants: [], handle: "", owner: "", name: "", kind: "", acts: [] };
  const UNBOUND = {
    login: "svc:ops",
    grants: ["tracker"],
    handle: "",
    owner: "svc:ops",
    name: "",
    kind: "",
    acts: ["mark_inbox"],
  };

  test("an anonymous reader is told what would make this their inbox", async () => {
    const viewer = { ...NOBODY, anonymous: true };
    mount({ viewer, answers: { viewer } });
    await settle();
    expect(screen.getByText(/Nobody is signed in/)).toBeTruthy();
    // NO PERSON'S READS: nothing to ask for on nobody's behalf.
    expect(asked.some((a) => a.kind === "inbox" || a.kind === "decisions")).toBe(false);
    const markAll = screen.getByRole("button", { name: "Mark all read" });
    expect(markAll.getAttribute("aria-disabled")).toBe("true");
    expect(reasonOf(markAll)).toContain(WRITE_REASONS.anonymous);
  });

  // AN UNBOUND READER IS SOMEBODY: its record is its login, and what it
  // follows and what names it reaches it. Asked by `handle`, it had no inbox
  // here at all while its assistant wrote notices to its login. Only a
  // credential is ever unbound (a service account here), since every person
  // holds a human seat, and the remedy names a HUMAN seat.
  test("a service account bound to no seat reads the inbox kept under its login", async () => {
    mount({
      viewer: UNBOUND,
      answers: { viewer: UNBOUND, inbox: inboxes({ unread: [notice("r-1")] }) },
    });
    await settle();
    expect(screen.getByText(/not bound to a seat/)).toBeTruthy();
    // WHAT BINDING WOULD ADD, and the command that does it.
    expect(screen.getByText(/bind this account to an open seat/)).toBeTruthy();
    expect(screen.getByText(/crewlet iam bind ID SEAT/)).toBeTruthy();
    const reads = asked.filter((a) => a.kind === "inbox");
    expect(reads.length).toBeGreaterThan(0);
    expect(reads.every((a) => a.params.handle === "svc:ops")).toBe(true);
    const markAll = screen.getByRole("button", { name: "Mark all read" });
    expect(markAll.getAttribute("aria-disabled")).not.toBe("true");
  });

  // PUSHED AS WELL AS POLLED: a commit that moves the reader's own record asks
  // Updates and the Snoozed count again at once, rather than a poll later.
  test("a push on the reader's own record asks their updates and snoozed again", async () => {
    const { store } = mount({ answers: { inbox: inboxes({ unread: [notice("r-1")] }) } });
    await settle();
    // THE SCREEN'S OWN READS, not the frame's badge count (`primary_only`).
    const count = (p: (params: Record<string, unknown>) => boolean) =>
      asked.filter((a) => a.kind === "inbox" && p(a.params)).length;
    const isUpdates = (p: Record<string, unknown>) => p.unread === true && p.snoozed === "exclude";
    const isSnoozed = (p: Record<string, unknown>) => p.snoozed === "only";
    const updates = count(isUpdates);
    const snoozed = count(isSnoozed);
    // ANOTHER RECORD'S PUSH moves nothing here.
    await act(async () => store.applyInboxChanged({ handle: "maya" }));
    await settle();
    expect(count(isUpdates)).toBe(updates);
    await act(async () => store.applyInboxChanged({ handle: "jane" }));
    await settle();
    expect(count(isUpdates)).toBe(updates + 1);
    expect(count(isSnoozed)).toBe(snoozed + 1);
  });

  test("a bound person the engine does not serve sees every control, disabled with the reason", async () => {
    const unserved = { ...JANE, acts: [] };
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      viewer: unserved,
      answers: {
        viewer: unserved,
        decisions: decisionsOf(askItem()),
        inbox: inboxes({ unread: [notice("r-1")] }),
      },
    });
    await settle();
    for (const name of ["Mark all read", /Hold release/]) {
      const button = screen.getByRole("button", { name });
      expect(button.getAttribute("aria-disabled")).toBe("true");
      expect(reasonOf(button)).toContain(WRITE_REASONS.not_served);
    }
    cleanup();
    location.hash = "#/inbox?row=r-1";
    mount({
      viewer: unserved,
      answers: { viewer: unserved, inbox: inboxes({ unread: [notice("r-1")] }) },
    });
    await settle();
    const read = screen.getByRole("button", { name: "Mark as read" });
    expect(read.getAttribute("aria-disabled")).toBe("true");
    expect(reasonOf(read)).toContain(WRITE_REASONS.not_served);
  });
});

// WHERE THE COMPANY RUNS JIRA THE INBOX SAYS WHERE ASKS GO, in the list and in
// the reading pane: the pane said "All clear. New updates open here.",
// promising updates that cannot arrive. Mutation: keep the pane's default
// copy and the Jira title is gone.
test("on a Jira company the list and the empty pane say asks and mentions arrive in Jira", async () => {
  mount({ services: { tracker: "jira", knowledge: "native" } });
  await settle();
  expect(
    screen.getByText("Jira handles work items, so asks and mentions arrive there."),
  ).toBeTruthy();
  expect(screen.getByText("Asks and mentions arrive in Jira")).toBeTruthy();
  expect(screen.queryByText("New updates open here.")).toBeNull();
  // AND THE TRACKER'S NOTICES ARE NEVER ASKED FOR.
  expect(asked.some((a) => a.kind === "inbox")).toBe(false);
});

// ONE EMPTY STATE FOR AN EMPTY INBOX: the list says it, and the reading pane
// beside it holds one quiet line rather than a second "All clear". Mutation:
// drop `listEmpty` and the pane's All clear is back.
test("an empty inbox says so once, in the list, and the pane holds a quiet line", async () => {
  mount();
  await settle();
  expect(document.querySelectorAll(".crewlet-empty-state")).toHaveLength(1);
  expect(screen.queryByText("All clear")).toBeNull();
  expect(document.querySelector(".inbox-pane")?.textContent).toBe("New updates open here.");
});
