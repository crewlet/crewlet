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
import { AppShell, LayerHost, ToastProvider } from "@crewlethq/ui";

import { ADVANCE_MS, Inbox } from "./Inbox.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { FrameReadings } from "~/app/Shell.tsx";
import { healthFrame } from "~/test/health.ts";
import { installScreen } from "~/testing.tsx";
import { withDerived } from "~/test/org.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";

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
  login: "U0FOUNDER",
  grants: ["admin", "worklet", "doclet"],
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

function inboxOf(notices: unknown[], extra: Record<string, unknown> = {}) {
  return { handle: "jane", notices, primary_reasons: [], unread: 0, primary: 0, ...extra };
}

type Answer = unknown | ((params: Record<string, unknown>) => Promise<unknown>);

const QUIET: Record<string, Answer> = {
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  answered_decisions: { handle: "jane", items: [] },
  work_inbox: inboxOf([]),
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
/** What the engine answers a press of each tool with, where not `applied`. */
let actAnswers: Record<
  string,
  | { outcome: string; receipt: Record<string, unknown> }
  | { refused: number; body: Record<string, unknown> }
>;

/**
 * Midday on the company's clock (the fixture org keeps UTC).
 *
 * Every fixture notice is stamped a minute (an ask twelve) before
 * `Date.now()`, so the time a row draws and the day a snooze preset lands on
 * are the same on every run. Only `Date` is faked: the suite's own waits, and
 * the moment before the next item opens, keep real timers.
 */
const PINNED_NOW = "2026-09-30T12:00:00Z";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(PINNED_NOW));
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/inbox";
  asked = [];
  posted = [];
  actAnswers = {};
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      const answer = actAnswers[tool] ?? { outcome: "applied", receipt: {} };
      if ("refused" in answer) {
        return new Response(JSON.stringify(answer.body), {
          status: answer.refused,
          headers: { "Content-Type": "application/json" },
        });
      }
      return new Response(
        JSON.stringify({
          tool,
          outcome: answer.outcome,
          position: answer.outcome === "applied" ? "CREWLET_TRACKER_LOG@1:99" : null,
          receipt: answer.receipt,
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
  shell = false,
}: {
  answers?: Record<string, Answer>;
  viewer?: Record<string, unknown>;
  agents?: Record<string, unknown>[];
  services?: Record<string, string>;
  /** Inside the kit's shell, whose content column is the screen's width. */
  shell?: boolean;
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
            <Router>{shell ? <AppShell>{<Inbox />}</AppShell> : <Inbox />}</Router>
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

/** A `work_inbox` answer per question: Updates, Snoozed and Earlier ask apart. */
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

async function wait(ms: number) {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, ms));
  });
}

describe("the list", () => {
  test("a decision leads the list under Needs you, and its notice is not listed again", async () => {
    mount({
      answers: {
        decisions: decisionsOf(askItem()),
        work_inbox: inboxes({ unread: [notice("r-1", ASKED), notice("r-2")] }),
      },
    });
    await settle();
    const list = within(screen.getByRole("navigation", { name: "Inbox" }));
    const needs = within(list.getByRole("region", { name: /^Needs you/ }));
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
        work_inbox: inboxes({
          unread: [notice("r-1")],
          snoozed: [notice("r-9", { snoozed: true, snoozed_until: "2030-01-01T09:00:00Z" })],
          all: [notice("r-1"), notice("r-5", { read: true, excerpt: "an old one" })],
        }),
      },
    });
    await settle();
    expect(screen.queryByRole("radio")).toBeNull();
    const reads = () => asked.filter((a) => a.kind === "work_inbox").map((a) => a.params);
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

  // WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person the directory binds
  // to a seat writes AS the seat, so their row draws as the seat, never as the
  // credential they wrote through; a principal bound to none writes under its
  // own login, which is a name and is drawn as one; and only the engine's own
  // change names nobody.
  test("a row is drawn as the seat a person writes as, and an unbound principal as its login", async () => {
    mount({
      answers: {
        work_inbox: inboxes({
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
        work_inbox: inboxes({
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
        work_inbox: inboxes({ unread: [notice("r-1")] }),
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
    expect(task.getAttribute("href")).toBe("#/worklet/PROD-91");
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
        work_inbox: inboxes({
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
    expect(task.getAttribute("href")).toBe("#/worklet/t-dup");
    expect(list.queryByText(/The claimant/)).toBeNull();
  });

  // THE OPEN ROW WAS DRAWN TWICE: a tint on the row and a rail beside it.
  test("the open row is the one row marked current, with one highlight", async () => {
    location.hash = "#/inbox?row=r-2";
    mount({ answers: { work_inbox: inboxes({ unread: [notice("r-1"), notice("r-2")] }) } });
    await settle();
    const list = screen.getByRole("navigation", { name: "Inbox" });
    const current = list.querySelectorAll("[aria-current]");
    expect(current).toHaveLength(1);
    expect(current[0]!.textContent).toContain("something about r-2");
    expect(list.querySelectorAll(".crewlet-inbox-row--selected")).toHaveLength(1);
  });

  test("beside an assistant that leaves the screen a phone's width, the inbox is one pane with Back", async () => {
    // A wide window (the setup stub answers no width query) and a 443px
    // screen: what a 1375px window leaves beside a 640px assistant.
    const narrow = installScreen(443);
    try {
      location.hash = "#/inbox";
      mount({
        shell: true,
        answers: { work_inbox: inboxes({ unread: [notice("r-1"), notice("r-2")] }) },
      });
      await settle();
      // The list alone: no row is opened beside it, so no pane is drawn.
      expect(screen.getByRole("navigation", { name: "Inbox" })).toBeTruthy();
      expect(document.querySelector(".crewlet-reading-pane")).toBeNull();
      location.hash = "#/inbox?row=r-2";
      await settle();
      // The open row in the list's place, with its way back.
      expect(screen.queryByRole("navigation", { name: "Inbox" })).toBeNull();
      expect(screen.getByRole("button", { name: "Back to the inbox" })).toBeTruthy();
    } finally {
      narrow.restore();
    }
  });

  test("j and k step through the rows, opening each in the pane", async () => {
    location.hash = "#/inbox?row=r-1";
    mount({
      answers: {
        work_inbox: inboxes({ unread: [notice("r-1"), notice("r-2"), notice("r-3")] }),
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

  // AN ASK IS ONE MESSAGE: its question's bare `#11` is placed by the
  // repository its context names; an address its asker wrote opens only as
  // the dashboard rebuilt it on the company's own host; and a cited task's
  // button names its key once, whatever an older label repeats.
  test("an ask's words link only what the dashboard built, and name a cited key once", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    const ask = askItem({
      ...DECISION,
      question: "How should the guard patch from the #11 PR body be applied?",
      evidence: [{ kind: "task", ref: "t-20", label: "GP-20 sitemap fix, blocked on this access" }],
    });
    ask.ask.body =
      "Assigned to you in gophrase/backend. See https://evil.example.net/c?q=secret and " +
      "https://github.com/gophrase/backend/pull/11?x=1.";
    mount({
      answers: {
        decisions: decisionsOf(ask),
        link_hosts: {
          hosts: [{ kind: "github", url: "https://github.com", owners: ["gophrase"] }],
        },
        work_item: {
          task: {
            id: "t-20",
            key: "GP-20",
            project: "GP",
            title: "Sitemap fix",
            status: "todo",
            version: 1,
          },
        },
      },
    });
    await settle();
    const pane = document.querySelector(".crewlet-reading-pane")!;
    const title = pane.querySelector(".crewlet-reading-pane__title")!;
    expect(title.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("github");
    const hrefs = [...pane.querySelectorAll("a")].map((a) => a.getAttribute("href"));
    expect(hrefs).toContain("https://github.com/gophrase/backend");
    expect(hrefs).toContain("https://github.com/gophrase/backend/pull/11");
    expect(hrefs.some((h) => h?.includes("evil.example.net"))).toBe(false);
    expect(pane.textContent).toContain("https://evil.example.net/c?q=secret");
    expect(
      screen.getByRole("link", { name: /GP-20 sitemap fix, blocked on this access/ }),
    ).toBeTruthy();
    expect(pane.textContent).not.toContain("GP-20 GP-20");
  });

  // A CITED ADDRESS IS A BUTTON ONLY WHERE THE DASHBOARD BUILDS IT, labelled by
  // the dashboard: an asker's label and its address as written are its words.
  //
  // Mutation: draw a url evidence as the live external button it was and the
  // evil address is an anchor labelled "Sign in to GitHub".
  test("a cited address opens only as the dashboard rebuilt it, never by the asker's label", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: decisionsOf(
          askItem({
            ...DECISION,
            evidence: [
              {
                kind: "url",
                ref: "https://evil.example.net/c?q=secret",
                label: "Sign in to GitHub",
              },
              {
                kind: "url",
                ref: "https://github.com/gophrase/backend/pull/14?ref=x",
                label: "Approve here",
              },
            ],
          }),
        ),
        link_hosts: {
          hosts: [{ kind: "github", url: "https://github.com", owners: ["gophrase"] }],
        },
      },
    });
    await settle();
    const pane = document.querySelector(".crewlet-reading-pane")!;
    const anchors = [...pane.querySelectorAll("a")].filter((a) =>
      a.getAttribute("href")?.startsWith("https://"),
    );
    expect(anchors.map((a) => a.getAttribute("href"))).toEqual([
      "https://github.com/gophrase/backend/pull/14",
    ]);
    // THE DASHBOARD'S LABEL AND THE APP'S MARK, never the asker's words.
    expect(anchors[0]!.textContent).toContain("gophrase/backend#14");
    expect(anchors[0]!.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("github");
    expect(pane.textContent).not.toContain("Approve here");
    expect(pane.textContent).not.toContain("Sign in to GitHub");
    // THE OTHER IS ITS ADDRESS AS WRITTEN, to read and copy.
    expect(pane.textContent).toContain("https://evil.example.net/c?q=secret");
  });

  // A CITED ADDRESS READS AS THE SAME ADDRESS DOES IN A SENTENCE: what its
  // mark means is said before its words, a reference is set in mono, and one
  // that opens nothing still says whose it is (one helper draws both).
  //
  // Mutation: draw the evidence's mark and words by hand, without `kind` and
  // `code`, and a screen reader hears no "GitHub" and the reference is prose.
  test("a cited address says what its mark means, live or inert", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: decisionsOf(
          askItem({
            ...DECISION,
            evidence: [
              { kind: "url", ref: "https://github.com/gophrase/backend/pull/14" },
              { kind: "url", ref: "https://github.com/attacker/x" },
            ],
          }),
        ),
        link_hosts: {
          hosts: [{ kind: "github", url: "https://github.com", owners: ["gophrase"] }],
        },
      },
    });
    await settle();
    const evidence = document.querySelector(".inbox-evidence")!;
    const live = evidence.querySelector("a")!;
    expect(live.textContent).toContain("GitHub: gophrase/backend#14");
    expect(live.querySelector(".crewlet-inline-ref--code")).not.toBeNull();
    const inert = [...evidence.querySelectorAll(".crewlet-inline-ref")].find((r) =>
      r.textContent?.includes("attacker"),
    )!;
    expect(inert.closest("a")).toBeNull();
    expect(inert.textContent).toBe("GitHub: https://github.com/attacker/x");
  });

  // A REBUILT ADDRESS IS LABELLED FROM WHAT IT WAS REBUILT FROM: a Confluence
  // page's title is the asker's words, which the rebuilt address drops.
  test("a cited Confluence page's button names its space and id, never its title", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: decisionsOf(
          askItem({
            ...DECISION,
            evidence: [
              {
                kind: "url",
                ref: "https://acme.atlassian.net/wiki/spaces/DES/pages/123/Sign+in+to+GitHub",
              },
            ],
          }),
        ),
        link_hosts: { hosts: [{ kind: "confluence", url: "https://acme.atlassian.net/wiki" }] },
      },
    });
    await settle();
    const pane = document.querySelector(".crewlet-reading-pane")!;
    const button = [...pane.querySelectorAll("a")].find((a) =>
      a.getAttribute("href")?.startsWith("https://acme.atlassian.net"),
    )!;
    expect(button.getAttribute("href")).toBe(
      "https://acme.atlassian.net/wiki/spaces/DES/pages/123",
    );
    expect(button.textContent).toContain("DES page 123");
    expect(pane.textContent).not.toContain("Sign in to GitHub");
  });

  // EVERY PART OF AN ASK IS ONE MESSAGE, each by its own opt-in (`scoped`):
  // the question, the context, the reasoning, the recommendation, each option
  // and its detail, in the pane, the Inbox row and the answered pane. Each
  // part below holds a bare number and another part holds the repository, so
  // a part that stops opting in reads its own words alone and loses its mark.
  //
  // Mutation: drop `scoped` at any one of those sites and its case fails.
  describe("an ask's parts read the ask's whole message", () => {
    const HOSTS = { hosts: [{ kind: "github", url: "https://github.com", owners: ["gophrase"] }] };
    const marked = (el: Element | null | undefined) =>
      el?.querySelectorAll("[data-vendor]").length ?? 0;

    test("the context, the reasoning, the recommendation and the options", async () => {
      location.hash = "#/inbox?row=ask%3Ac-12";
      const ask = askItem({
        ...DECISION,
        question: "Ship gophrase/backend?",
        options: [
          { id: "hold", label: "Take #14", detail: "after #15" },
          { id: "ship", label: "Ship anyway" },
        ],
        recommended: "hold",
        rationale: "#13 is safer.",
      });
      ask.ask.body = "After #12.";
      mount({ answers: { decisions: decisionsOf(ask), link_hosts: HOSTS } });
      await settle();
      const pane = document.querySelector(".crewlet-reading-pane")!;
      const hrefs = [...pane.querySelectorAll("a")].map((a) => a.getAttribute("href"));
      // THE CONTEXT and THE REASONING, whose links are checked and built.
      expect(hrefs).toContain("https://github.com/gophrase/backend/issues/12");
      expect(hrefs).toContain("https://github.com/gophrase/backend/issues/13");
      // THE RECOMMENDATION, named in the reasoning's title.
      expect(marked(pane.querySelector(".crewlet-reading-pane__reasoning strong"))).toBe(1);
      // THE OPTION AND ITS DETAIL, each its own render.
      const take = screen.getByRole("button", { name: /^Take/ });
      expect(marked(take)).toBe(2);
    });

    test("the question, in the pane, in its Inbox row and once answered", async () => {
      location.hash = "#/inbox?row=ask%3Ac-12";
      const ask = askItem({ ...DECISION, question: "Apply #11?" });
      ask.ask.body = "Assigned in gophrase/backend.";
      mount({ answers: { decisions: decisionsOf(ask), link_hosts: HOSTS } });
      await settle();
      expect(marked(document.querySelector(".crewlet-reading-pane__title"))).toBe(1);
      const list = screen.getByRole("navigation", { name: "Inbox" });
      expect(marked(list)).toBeGreaterThanOrEqual(1);
    });

    test("the question of an answered ask", async () => {
      const answered = answeredItem();
      answered.ask = {
        ...answered.ask,
        decision: { ...DECISION, question: "Apply #11?" } as never,
      };
      answered.ask.body = "Assigned in gophrase/backend.";
      location.hash = "#/inbox?row=ask%3Ac-20";
      mount({
        answers: {
          decisions: decisionsOf(),
          answered_decisions: { handle: "jane", items: [answered] },
          link_hosts: HOSTS,
        },
      });
      await settle();
      expect(marked(document.querySelector(".crewlet-reading-pane__title"))).toBe(1);
    });
  });

  // A REPLY IS ITS OWN MESSAGE: the ask's repository places the ask's numbers,
  // never a reply's.
  test("a reply's bare number is placed by the reply's own words", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    const ask = askItem({ ...DECISION, question: "Apply the #11 PR body?" });
    ask.ask.body = "Assigned to you in gophrase/backend.";
    const reply = (id: string, body: string) => ({
      id,
      task: "t-12",
      author: "maya",
      author_kind: "human",
      body,
      reply_to: "c-12",
      created_at: new Date().toISOString(),
    });
    mount({
      answers: {
        decisions: decisionsOf(ask),
        link_hosts: {
          hosts: [{ kind: "github", url: "https://github.com", owners: ["gophrase"] }],
        },
        work_comments: {
          item: "t-12",
          key: "LEAD-12",
          title: "Q4 release plan",
          comments: [
            reply("c-20", "Done with #11."),
            reply("c-21", "Merged in gophrase/web, see #5."),
          ],
        },
      },
    });
    await settle();
    const thread = screen.getByRole("region", { name: "Replies to this question" });
    const hrefs = [...thread.querySelectorAll("a")]
      .map((a) => a.getAttribute("href"))
      .filter((h) => h?.startsWith("https://"));
    expect(hrefs).toEqual([
      "https://github.com/gophrase/web",
      "https://github.com/gophrase/web/issues/5",
    ]);
    // THE ASK'S OWN TITLE IS STILL PLACED BY ITS CONTEXT.
    const title = document.querySelector(".crewlet-reading-pane__title")!;
    expect(title.querySelector("[data-vendor]")?.getAttribute("data-vendor")).toBe("github");
  });

  test("an option answers with one press, through the one write, and marks its notice read", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: decisionsOf(askItem()),
        work_inbox: inboxes({ unread: [notice("r-1", ASKED)] }),
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

  // THE FOCUS FOLLOWS THE ANSWER: the option pressed is replaced by the
  // outcome line, which takes the focus rather than leaving it on the body.
  //
  // Mutation: drop the outcome's focus, and the body holds it.
  test("an option's answer moves the focus to its outcome", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({ answers: { decisions: decisionsOf(askItem()) } });
    await settle();
    const option = screen.getByRole("button", { name: /Ship anyway/ });
    option.focus();
    fireEvent.click(option);
    await settle();
    const outcome = document.querySelector(".crewlet-reading-pane__outcome") as HTMLElement;
    expect(document.activeElement).toBe(outcome);
    expect(screen.getByRole("status", { name: "Answered Ship anyway" })).toBe(outcome);
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
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({ answers: { decisions: decisionsOf(askItem(), second) } });
    await settle();
    fireEvent.click(screen.getByRole("button", { name: /Hold release/ }));
    await settle();
    // WHAT HAPPENED, FIRST: the open item shows its answer, and its row stays
    // where it stood, marked Answered with the choice under the question,
    // and read as answered. Never struck: every word stays legible.
    expect(location.hash).toContain("row=ask%3Ac-12");
    const region = screen.getByRole("region", { name: /^Needs you/ });
    const needs = within(region);
    const done = needs.getAllByRole("button", { name: /Answered$/ });
    expect(done).toHaveLength(1);
    expect(done[0]!.textContent).toContain("You chose Hold release");
    expect(done[0]!.querySelector(".crewlet-inbox-row__status")?.textContent).toBe("Answered");
    expect(done[0]!.querySelector(".crewlet-inbox-row__meta")).toBeNull();
    // THE COUNT IS WHAT IS STILL OPEN.
    expect(region.getAttribute("aria-labelledby")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Needs you, 1" })).toBe(region);
    await wait(ADVANCE_MS + 50);
    expect(location.hash).toContain("row=ask%3Ac-13");
  });

  test("a run answered in words stays in place with what was written under it", async () => {
    location.hash = "#/inbox?row=run%3Aturn-7";
    const run = {
      turn_id: "turn-7",
      launch_id: "q1",
      agent_handle: "cto",
      status: "awaiting_clarification",
      question: "May I delete the fixtures directory?",
      question_context: "",
      audience_handles: ["jane"],
      started_at: new Date(Date.now() - 20 * 60_000).toISOString(),
      updated_at: new Date(Date.now() - 5 * 60_000).toISOString(),
    };
    // THE ENGINE'S ANSWER AND ITS RECORD: pending, then recorded on the run.
    actAnswers = {
      answer_run: { outcome: "pending", receipt: { launch_id: "q1", answer_id: "a1" } },
    };
    mount({
      answers: {
        decisions: decisionsOf({
          kind: "run",
          at: new Date(Date.now() - 5 * 60_000).toISOString(),
          run,
        }),
        sandbox_runs: () =>
          Promise.resolve({
            runs: [
              posted.some((p) => p.tool === "answer_run")
                ? { ...run, status: "answered", answer_ids: ["a1"] }
                : run,
            ],
          }),
      },
    });
    await settle();
    fireEvent.change(screen.getByRole("textbox", { name: "Your answer to the run" }), {
      target: { value: "Yes, but keep the golden files." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
    await settle();
    await settle();
    const row = within(screen.getByRole("region", { name: /^Needs you/ })).getByRole("button", {
      name: /Answered$/,
    });
    expect(row.textContent).toContain("You replied “Yes, but keep the golden files.”");
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
    mount({ answers: { work_inbox: inboxes({ unread: [notice("r-1"), notice("r-2")] }) } });
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

  test("Mark all read reads through the newest update loaded, not past it", async () => {
    mount({
      answers: {
        work_inbox: inboxes({
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
        work_inbox: inboxes({
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
    // QUOTED WHOLE ABOVE, so its thread is the replies alone.
    const read = asked.find((a) => a.kind === "work_comments")!;
    expect(read.params).toMatchObject({ item: "PROD-91", thread: "c-40" });
    expect(read.params.with_root).toBeUndefined();
  });

  test("a comment quoted only in preview is read whole in its thread", async () => {
    location.hash = "#/inbox?row=r-1";
    mount({
      answers: {
        work_inbox: inboxes({
          unread: [
            notice("r-1", { reason: "mention", comment_id: "c-40", excerpt: "Can you check…" }),
          ],
        }),
      },
    });
    await settle();
    const read = asked.find((a) => a.kind === "work_comments")!;
    expect(read.params).toMatchObject({ thread: "c-40", with_root: true });
  });

  test("an update that only informs says why it reached you", async () => {
    location.hash = "#/inbox?row=r-1";
    mount({
      answers: {
        work_inbox: inboxes({ unread: [notice("r-1", { kind: "status", excerpt: "" })] }),
      },
    });
    await settle();
    expect(screen.getByRole("heading", { name: "Changed its status" })).toBeTruthy();
    expect(byline()).toBe("CTO on PROD-91 · you follow this task");
    expect(screen.getByRole("region", { name: "Conversation on this task" })).toBeTruthy();
    // NOT ABOUT A COMMENT, so the conversation is the task's whole thread.
    const read = asked.find((a) => a.kind === "work_comments")!;
    expect(read.params.thread).toBeUndefined();
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
    expect(read.params).toMatchObject({ item: "LEAD-12", thread: "c-12", limit: 50 });
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

  // "EARLIER COMMENTS" OVER NO REPLIES: the pane paged the task's whole
  // thread and kept the question's replies, so the TASK's cursor offered
  // earlier replies to a question nobody had replied to. The engine here
  // answers as it does: the question's own thread is empty, the task is not.
  // Mutation: read `work_comments` without `thread`.
  test("a question nobody replied to offers no earlier comments", async () => {
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: decisionsOf(askItem()),
        work_comments: async (params: Record<string, unknown>) => ({
          item: "t-12",
          key: "LEAD-12",
          title: "Q4 release plan",
          ...(params.thread === "c-12"
            ? { comments: [] }
            : {
                comments: [
                  {
                    id: "c-30",
                    task: "t-12",
                    author: "maya",
                    author_kind: "human",
                    body: "Unrelated to the question.",
                    created_at: new Date().toISOString(),
                  },
                ],
                next_cursor: "1:c-29",
              }),
        }),
      },
    });
    await settle();
    const region = screen.getByRole("region", { name: "Replies to this question" });
    const thread = within(region);
    expect(thread.queryByRole("button", { name: "Earlier comments" })).toBeNull();
    expect(thread.getByText("No replies yet.")).toBeTruthy();
    // NO EMPTY LIST between the heading and the line: an empty <ol> is a flex
    // item, and the thread's gap around it doubled the space above the line.
    expect(region.querySelector(".inbox-thread-list")).toBeNull();
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
        launch_id: "q8",
        agent_handle: "cto",
        status: "paused",
        question_context: "",
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

  // A PARKED RUN IS READ AS AN ASK IS. The pane drew the run's whole question
  // twice: its first sentence as the title, then all of it again as a
  // verbatim, monospace quote, so a run that asked to choose read as a wall
  // of machine text with no option to press. A run asks through a structured
  // tool now: a one-sentence question, its context as Markdown and, where it
  // asks to choose, a decision of an ask's own shape.
  //
  // Mutation: quote the question verbatim again, render the context as plain
  // text, or send the option as `answer`, and a case below fails.
  describe("a parked run's question", () => {
    const parkedRun = (extra: Record<string, unknown>) => ({
      kind: "run",
      at: new Date(Date.now() - 5 * 60_000).toISOString(),
      run: {
        turn_id: "turn-7",
        launch_id: "q1",
        agent_handle: "cto",
        status: "awaiting_clarification",
        question_context: "",
        paused_at: new Date(Date.now() - 5 * 60_000).toISOString(),
        updated_at: new Date(Date.now() - 5 * 60_000).toISOString(),
        pause_ttl_seconds: 3_600,
        work_item: { backend: "native", id: "t-12", key: "LEAD-12", project: "LEAD" },
        ...extra,
      },
    });
    const pane = () => document.querySelector(".crewlet-reading-pane") as HTMLElement;
    const lead = () => document.querySelector(".crewlet-reading-pane__lead");

    test("that asks to choose reads as an ask and is answered by an option's press", async () => {
      const run = parkedRun({
        question: "May I delete the fixtures directory?",
        question_context: "It is regenerated by **make fixtures**.\n\n- golden files\n- snapshots",
        decision: {
          question: "May I delete the fixtures directory?",
          options: [
            { id: "keep", label: "Keep it", detail: "The run works around it" },
            { id: "delete", label: "Delete it" },
          ],
          recommended: "delete",
          rationale: "Nothing else reads it.",
          role: "approver",
        },
      });
      actAnswers = {
        answer_run: { outcome: "pending", receipt: { launch_id: "q1", answer_id: "a1" } },
      };
      location.hash = "#/inbox?row=run%3Aturn-7";
      mount({
        answers: {
          decisions: decisionsOf(run),
          // THE RUN'S RECORD once the answer was sent: resumed with it.
          sandbox_runs: () =>
            Promise.resolve({
              runs: [
                posted.some((p) => p.tool === "answer_run")
                  ? { ...run.run, status: "resumed", answer_ids: ["a1"] }
                  : run.run,
              ],
            }),
        },
      });
      await settle();
      expect(
        screen.getByRole("heading", { name: "May I delete the fixtures directory?" }),
      ).toBeTruthy();
      expect(byline()).toBe("CTO’s coding run asks for your approval on LEAD-12");
      // THE CONTEXT IS MARKDOWN, never a verbatim quote of the question.
      expect(lead()?.querySelector("strong")?.textContent).toBe("make fixtures");
      expect(lead()?.querySelectorAll("li")).toHaveLength(2);
      expect(document.querySelector("blockquote")).toBeNull();
      expect(document.querySelector(".crewlet-reading-pane__quote")).toBeNull();
      expect(pane().textContent?.split("May I delete the fixtures directory?")).toHaveLength(2);
      // THE RECOMMENDATION, with its reasoning.
      const reasoning = document.querySelector(".crewlet-reading-pane__reasoning");
      expect(reasoning?.textContent).toContain("CTO recommends Delete it");
      expect(reasoning?.textContent).toContain("Nothing else reads it.");
      // THE OPTIONS, the recommended one first.
      const options = within(screen.getByRole("group", { name: "Answers" })).getAllByRole("button");
      expect(options[0]!.textContent).toBe("Delete it Recommended");
      expect(screen.queryByRole("textbox", { name: "Your answer to the run" })).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: /Delete it/ }));
      await settle();
      await settle();
      expect(posted).toEqual([
        { tool: "answer_run", args: { turn_id: "turn-7", launch_id: "q1", choice: "delete" } },
      ]);
      expect(document.querySelector(".crewlet-reading-pane__outcome")?.textContent).toBe(
        "Answered Delete it (Recommended)",
      );
    });

    test("that asks to choose takes words instead through Something else", async () => {
      const run = parkedRun({
        question: "May I delete the fixtures directory?",
        decision: {
          question: "May I delete the fixtures directory?",
          options: [
            { id: "keep", label: "Keep it" },
            { id: "delete", label: "Delete it" },
          ],
          role: "contributor",
        },
      });
      location.hash = "#/inbox?row=run%3Aturn-7";
      mount({ answers: { decisions: decisionsOf(run) } });
      await settle();
      fireEvent.click(screen.getByRole("button", { name: "Something else" }));
      await settle();
      expect(screen.queryByRole("group", { name: "Answers" })).toBeNull();
      fireEvent.change(screen.getByRole("textbox", { name: "Your answer to the run" }), {
        target: { value: "Move it under testdata instead." },
      });
      fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
      await settle();
      expect(posted).toEqual([
        {
          tool: "answer_run",
          args: { turn_id: "turn-7", launch_id: "q1", answer: "Move it under testdata instead." },
        },
      ]);
    });

    test("that is open reads its context as Markdown and is answered in words by its turn", async () => {
      const run = parkedRun({
        question: "Which retry ceiling should the client use?",
        question_context: "The API allows **five** retries.",
      });
      location.hash = "#/inbox?row=run%3Aturn-7";
      mount({ answers: { decisions: decisionsOf(run) } });
      await settle();
      expect(
        screen.getByRole("heading", { name: "Which retry ceiling should the client use?" }),
      ).toBeTruthy();
      expect(byline()).toBe("CTO’s coding run on LEAD-12");
      // A TURN'S TASK IS OPENED BY ITS ID: the turn's reference carries no
      // collision flag, and a key another task claimed first opens that task.
      const by = document.querySelector(".crewlet-reading-pane__byline") as HTMLElement;
      expect(within(by).getByRole("link").getAttribute("href")).toBe("#/worklet/t-12");
      expect(lead()?.querySelector("strong")?.textContent).toBe("five");
      expect(document.querySelector("blockquote")).toBeNull();
      expect(screen.queryByRole("group", { name: "Answers" })).toBeNull();
      fireEvent.change(screen.getByRole("textbox", { name: "Your answer to the run" }), {
        target: { value: "Three." },
      });
      fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
      await settle();
      expect(posted).toEqual([
        { tool: "answer_run", args: { turn_id: "turn-7", launch_id: "q1", answer: "Three." } },
      ]);
    });

    // A RUN'S QUESTION IS ITS TITLE, WHOLE. A row with no context was split
    // at its first sentence, and a question with a full stop of its own was
    // cut there: "Should I drop the legacy flag (e.g." headed the pane and the
    // rest of the question was read as its context.
    //
    // Mutation: split the question at its first sentence again, and the
    // heading is half of it.
    test("is its title whole, never split at a full stop inside it", async () => {
      const question = "Should I drop the legacy flag (e.g. --old) from the client?";
      const run = parkedRun({ question });
      location.hash = "#/inbox?row=run%3Aturn-7";
      mount({ answers: { decisions: decisionsOf(run) } });
      await settle();
      expect(screen.getByRole("heading", { name: question })).toBeTruthy();
      expect(lead()).toBeNull();
      expect(screen.queryByRole("button", { name: "Show all" })).toBeNull();
    });

    // AN OLDER BUILD'S FREE-TEXT QUESTION runs past the title's cap. It is
    // drawn whole and clamped, with every word a press away, rather than cut
    // into a title and a context it never had.
    //
    // Mutation: split it on its sentences, or draw it unclamped with no
    // control, and this fails.
    test("past the title's cap is drawn whole, clamped, with Show all", async () => {
      const question =
        "May I delete the fixtures directory? It is regenerated by make fixtures. " +
        "The golden files are kept beside it, and the snapshots are rebuilt on the next run. ".repeat(
          4,
        );
      const run = parkedRun({ question });
      location.hash = "#/inbox?row=run%3Aturn-7";
      mount({ answers: { decisions: decisionsOf(run) } });
      await settle();
      const title = document.querySelector(".crewlet-reading-pane__title") as HTMLElement;
      expect(title.textContent).toBe(question.trim());
      expect(lead()).toBeNull();
      const clamp = () => title.querySelector(".crewlet-text--clamp-3");
      expect(clamp()).not.toBeNull();
      const more = screen.getByRole("button", { name: "Show all" });
      expect(more.getAttribute("aria-expanded")).toBe("false");
      expect(more.getAttribute("aria-controls")).toBe(title.firstElementChild?.id);
      fireEvent.click(more);
      expect(clamp()).toBeNull();
      const less = screen.getByRole("button", { name: "Show less" });
      expect(less.getAttribute("aria-expanded")).toBe("true");
    });

    // A RUN'S CONTEXT AND THE ADDRESSES IT CITES are an agent's words, read
    // by the Inbox's `checked` rule as an ask's are: a link only where the
    // dashboard built it, on a host the company configured, and any other
    // address drawn inert, as written.
    //
    // Mutation: render the context with live links, or draw a url evidence
    // as the external button it was, and the evil address is an anchor.
    test("links only what the dashboard built, in its context and its evidence", async () => {
      const run = parkedRun({
        question: "May I apply the guard patch?",
        question_context:
          "Assigned in gophrase/backend. See https://evil.example.net/c?q=secret and " +
          "https://github.com/gophrase/backend/pull/11?x=1.",
        decision: {
          question: "May I apply the guard patch?",
          options: [
            { id: "apply", label: "Apply it" },
            { id: "hold", label: "Hold it" },
          ],
          recommended: "apply",
          rationale: "It is the reviewed fix.",
          evidence: [
            { kind: "url", ref: "https://evil.example.net/p", label: "Sign in to GitHub" },
            { kind: "url", ref: "https://github.com/gophrase/backend/pull/14", label: "Here" },
          ],
          role: "approver",
        },
      });
      location.hash = "#/inbox?row=run%3Aturn-7";
      mount({
        answers: {
          decisions: decisionsOf(run),
          link_hosts: {
            hosts: [{ kind: "github", url: "https://github.com", owners: ["gophrase"] }],
          },
        },
      });
      await settle();
      const hrefs = [...pane().querySelectorAll("a")].map((a) => a.getAttribute("href"));
      expect(hrefs).toContain("https://github.com/gophrase/backend/pull/11");
      expect(hrefs).toContain("https://github.com/gophrase/backend/pull/14");
      expect(hrefs.some((h) => h?.includes("evil.example.net"))).toBe(false);
      expect(lead()?.textContent).toContain("https://evil.example.net/c?q=secret");
      const evidence = document.querySelector(".inbox-evidence") as HTMLElement;
      expect(evidence.textContent).toContain("https://evil.example.net/p");
      expect(evidence.textContent).not.toContain("Sign in to GitHub");
      expect(evidence.textContent).not.toContain("Here");
    });

    // THE FOCUS FOLLOWS THE ANSWER. The option pressed is replaced by the
    // outcome line, and the focus fell to the page's body with it, so a
    // keyboard reader's next Tab started again from the top.
    //
    // Mutation: drop the outcome's focus, and the body holds it.
    test("moves the focus to the outcome once an option sent the answer", async () => {
      const run = parkedRun({
        question: "May I delete the fixtures directory?",
        decision: {
          question: "May I delete the fixtures directory?",
          options: [
            { id: "keep", label: "Keep it" },
            { id: "delete", label: "Delete it" },
          ],
          role: "approver",
        },
      });
      actAnswers = {
        answer_run: { outcome: "pending", receipt: { launch_id: "q1", answer_id: "a1" } },
      };
      location.hash = "#/inbox?row=run%3Aturn-7";
      mount({ answers: { decisions: decisionsOf(run) } });
      await settle();
      const option = screen.getByRole("button", { name: /Delete it/ });
      option.focus();
      fireEvent.click(option);
      await settle();
      const outcome = document.querySelector(".crewlet-reading-pane__outcome") as HTMLElement;
      expect(outcome).not.toBeNull();
      expect(document.activeElement).toBe(outcome);
      expect(outcome.getAttribute("tabindex")).toBe("-1");
      expect(screen.getByRole("status", { name: /Answer sent/ })).toBe(outcome);
    });

    // AN ANSWER NAMES THE QUESTION IT ANSWERS. A run that moved on to another
    // question since the pane was drawn refuses an answer naming the old one
    // `stale_version`, and the pane says it changed and draws the question the
    // run asks now, asked again by the refusal itself.
    //
    // Mutation: send no `launch_id`, or keep drawing the old question, and
    // this fails.
    test("refused because the question changed, says so and draws the one it asks now", async () => {
      const first = parkedRun({ question: "May I delete the fixtures directory?" });
      const now = parkedRun({
        launch_id: "q2",
        question: "May I move the fixtures under testdata?",
        question_context: "The **golden files** move with them.",
      });
      actAnswers = {
        answer_run: { refused: 409, body: { error: "stale_version", tool: "answer_run" } },
      };
      const sent = () => posted.some((p) => p.tool === "answer_run");
      location.hash = "#/inbox?row=run%3Aturn-7";
      mount({
        answers: {
          decisions: () => Promise.resolve(decisionsOf(sent() ? now : first)),
          sandbox_runs: () => Promise.resolve({ runs: [sent() ? now.run : first.run] }),
        },
      });
      await settle();
      fireEvent.change(screen.getByRole("textbox", { name: "Your answer to the run" }), {
        target: { value: "Yes." },
      });
      fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
      await settle();
      await settle();
      expect(posted).toEqual([
        { tool: "answer_run", args: { turn_id: "turn-7", launch_id: "q1", answer: "Yes." } },
      ]);
      const strip = document.querySelector(".crewlet-refusal-strip")?.firstChild?.textContent ?? "";
      expect(strip).toBe(ACT_ERRORS.stale_version);
      expect(
        screen.getByRole("heading", { name: "May I move the fixtures under testdata?" }),
      ).toBeTruthy();
      expect(lead()?.querySelector("strong")?.textContent).toBe("golden files");
    });
  });

  // AN ANSWER TO A PARKED RUN IS SENT, NOT ANSWERED, UNTIL THE RUN SAYS SO.
  //
  // `answer_run` answers `pending`: the answer is on its way to the node that
  // holds the run's seat. The pane drew that as "Answered" and dropped the
  // question, and a person whose answer never reached the run was told it had
  // while the question kept coming back. The pane now says what the run's own
  // record says: sent while it still waits on the question, answered once it
  // is recorded there, and asked again, with the reason, when the question
  // let the answer go.
  //
  // Mutation: settle on `pending` as before, and the first case reads
  // "Answered"; ignore `declined_answers`, and the last never asks again.
  describe("an answer by turn", () => {
    const parked = {
      turn_id: "turn-7",
      launch_id: "q1",
      agent_handle: "cto",
      status: "awaiting_clarification",
      question: "May I delete the fixtures directory?",
      question_context: "",
      paused_at: new Date(Date.now() - 5 * 60_000).toISOString(),
      updated_at: new Date(Date.now() - 5 * 60_000).toISOString(),
      pause_ttl_seconds: 3_600,
      declined_answers: [] as string[],
    };
    // How often the run record was asked for before the press, for a case
    // about what the press makes the pane ask again.
    let runsAskedAtPress = 0;
    const answerRun = async (
      after: Record<string, unknown>,
      others: object[] = [],
      press: (typeof actAnswers)[string] = {
        outcome: "pending",
        receipt: { launch_id: "q1", answer_id: "a1" },
      },
    ) => {
      actAnswers = { answer_run: press };
      location.hash = "#/inbox?row=run%3Aturn-7";
      mount({
        answers: {
          decisions: decisionsOf({ kind: "run", at: parked.paused_at, run: parked }),
          // THE RUN'S RECORD, as the engine holds it once the answer was sent.
          sandbox_runs: () =>
            Promise.resolve({
              runs: [
                ...(!posted.some((p) => p.tool === "answer_run")
                  ? [parked]
                  : after.gone
                    ? []
                    : [{ ...parked, ...after }]),
                ...others,
              ],
            }),
        },
      });
      await settle();
      fireEvent.change(screen.getByRole("textbox", { name: "Your answer to the run" }), {
        target: { value: "Yes, delete it." },
      });
      runsAskedAtPress = asked.filter((a) => a.kind === "sandbox_runs").length;
      fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
      await settle();
      await settle();
    };
    const outcome = () => document.querySelector(".crewlet-reading-pane__outcome")?.textContent;

    test("is shown as sent while the run still waits on its question", async () => {
      await answerRun({});
      expect(
        document.querySelector(".crewlet-reading-pane__outcome")?.getAttribute("data-tone"),
      ).toBe("pending");
      expect(outcome()).toBe("Answer sent. Waiting for CTO’s run to take it.");
      expect(screen.queryByRole("button", { name: "Send answer" })).toBeNull();
      expect(screen.queryByRole("button", { name: /Answered$/ })).toBeNull();
    });

    test("says the run ended, not that it was answered, when its record goes away", async () => {
      await answerRun({ gone: true });
      expect(
        document.querySelector(".crewlet-reading-pane__outcome")?.getAttribute("data-tone"),
      ).toBe("pending");
      expect(outcome()).toBe("Answer sent. CTO’s run has ended.");
      expect(screen.queryByRole("button", { name: /Answered$/ })).toBeNull();
    });

    test("is answered once the run was resumed with it", async () => {
      await answerRun({ status: "resumed", answer_ids: ["a1"] });
      expect(
        document.querySelector(".crewlet-reading-pane__outcome")?.getAttribute("data-tone"),
      ).toBe("done");
      expect(outcome()).toBe("Answered “Yes, delete it.”");
    });

    test("says what the seat is doing while the recorded answer waits for it", async () => {
      const busy = {
        ...parked,
        turn_id: "turn-9",
        launch_id: "q9",
        status: "running",
        role: "Senior Backend Engineer",
        work_item: { backend: "native", id: "t-4", key: "GP-4", project: "GP" },
      };
      await answerRun({ status: "answered", answer_ids: ["a1"] }, [busy]);
      expect(
        document.querySelector(".crewlet-reading-pane__outcome")?.getAttribute("data-tone"),
      ).toBe("pending");
      expect(outcome()).toBe(
        "Answer recorded. The Senior Backend Engineer is busy on GP-4’s run and picks this up next.",
      );
    });

    test("refused as answered already, says whose and when on the reader's clock", async () => {
      await answerRun(
        {
          status: "answered",
          answer_ids: ["a0"],
          answered_by: "miles",
          answered_at: "2026-09-30T11:35:00Z",
        },
        [],
        {
          refused: 409,
          body: {
            error: "already_answered",
            tool: "answer_run",
            detail: "This question was already answered by @miles, so your answer was not used.",
          },
        },
      );
      await settle();
      // THE REFUSAL'S ONE SENTENCE, drawn as every refusal of its code is,
      // and only the time beside it, on the reader's clock.
      const strip = document.querySelector(".crewlet-refusal-strip")?.firstChild?.textContent ?? "";
      expect(strip).toBe(ACT_ERRORS.already_answered);
      const when = screen.getByText(/^Answered by @miles at /).textContent ?? "";
      expect(when).toMatch(/^Answered by @miles at \S+\.$/);
      expect(when).not.toContain("UTC");
      // ASKED AGAIN FOR THE RECORD, by the refusal's own look-again.
      expect(asked.filter((a) => a.kind === "sandbox_runs").length).toBeGreaterThan(
        runsAskedAtPress,
      );
    });

    test("is refused, naming whose answer was used, when another was recorded first", async () => {
      await answerRun({
        status: "answered",
        answer_ids: ["a0"],
        answered_by: "jane",
        answered_at: "2026-09-30T11:35:00Z",
      });
      expect(outcome()).toBeUndefined();
      expect(
        screen.getByText("Your answer was not used: another answer was recorded first."),
      ).toBeTruthy();
      expect(screen.getByText(/^Answered by @jane at \S+\.$/)).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Send answer" })).toBeNull();
    });

    test("is asked again, with the reason, when the question let it go", async () => {
      await answerRun({ declined_answers: ["a1"] });
      expect(outcome()).toBeUndefined();
      expect(
        screen.getByText(
          "CTO’s run could not use your answer and is waiting on its question again. Answer it again.",
        ),
      ).toBeTruthy();
      expect(screen.getByRole("button", { name: "Send answer" })).toBeTruthy();
    });
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
        work_inbox: inboxes({
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
    grants: ["worklet"],
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
    expect(asked.some((a) => a.kind === "work_inbox" || a.kind === "decisions")).toBe(false);
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
      answers: { viewer: UNBOUND, work_inbox: inboxes({ unread: [notice("r-1")] }) },
    });
    await settle();
    expect(screen.getByText(/not bound to a seat/)).toBeTruthy();
    // WHAT BINDING WOULD ADD, and the command that does it.
    expect(screen.getByText(/bind this account to an open seat/)).toBeTruthy();
    expect(screen.getByText(/crewlet iam bind ID SEAT/)).toBeTruthy();
    const reads = asked.filter((a) => a.kind === "work_inbox");
    expect(reads.length).toBeGreaterThan(0);
    expect(reads.every((a) => a.params.handle === "svc:ops")).toBe(true);
    const markAll = screen.getByRole("button", { name: "Mark all read" });
    expect(markAll.getAttribute("aria-disabled")).not.toBe("true");
  });

  // PUSHED AS WELL AS POLLED: a commit that moves the reader's own record asks
  // Updates and the Snoozed count again at once, rather than a poll later.
  test("a push on the reader's own record asks their updates and snoozed again", async () => {
    const { store } = mount({ answers: { work_inbox: inboxes({ unread: [notice("r-1")] }) } });
    await settle();
    // THE SCREEN'S OWN READS, not the frame's badge count (`primary_only`).
    const count = (p: (params: Record<string, unknown>) => boolean) =>
      asked.filter((a) => a.kind === "work_inbox" && p(a.params)).length;
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
        work_inbox: inboxes({ unread: [notice("r-1")] }),
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
      answers: { viewer: unserved, work_inbox: inboxes({ unread: [notice("r-1")] }) },
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
  mount({ services: { tracker: "jira", knowledge: "doclet" } });
  await settle();
  expect(
    screen.getByText("Jira handles work items, so asks and mentions arrive there."),
  ).toBeTruthy();
  expect(screen.getByText("Asks and mentions arrive in Jira")).toBeTruthy();
  expect(screen.queryByText("New updates open here.")).toBeNull();
  // AND THE TRACKER'S NOTICES ARE NEVER ASKED FOR.
  expect(asked.some((a) => a.kind === "work_inbox")).toBe(false);
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

/** An ask put to Jane and closed, as `answered_decisions` lists it. */
function answeredItem(extra: Record<string, unknown> = {}) {
  const ask = { ...askItem().ask, comment: "c-20", key: "LEAD-20", id: "t-20" };
  return {
    at: new Date(Date.now() - 60 * 60_000).toISOString(),
    ask: { ...ask, open: false, answer_with: undefined },
    answer: {
      by: "maya",
      by_kind: "human",
      at: new Date(Date.now() - 60 * 60_000).toISOString(),
      comment: "c-21",
      choice: "ship",
      body: "Ship anyway",
    },
    ...extra,
  };
}

describe("what was answered", () => {
  test("answers from before this visit wait in a closed, counted group after Updates", async () => {
    mount({
      answers: {
        decisions: decisionsOf(askItem()),
        answered_decisions: { handle: "jane", items: [answeredItem()] },
        work_inbox: inboxes({ unread: [notice("r-2")] }),
      },
    });
    await settle();
    const list = within(screen.getByRole("navigation", { name: "Inbox" }));
    const toggle = list.getByRole("button", { name: "Answered, 1" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    // CLOSED: its rows are neither drawn nor stepped through.
    expect(screen.queryByRole("button", { name: /Maya Ops chose/ })).toBeNull();
    const groups = list.getAllByRole("region").map((g) => g.getAttribute("aria-labelledby"));
    expect(groups).toHaveLength(3);
    fireEvent.click(toggle);
    await settle();
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const row = within(list.getByRole("region", { name: "Answered, 1" })).getByRole("button", {
      name: /Answered$/,
    });
    // WHO ANSWERED IS SAID: a colleague may answer what was put to you.
    expect(row.textContent).toContain("Maya Ops chose Ship anyway");
    // AND THE GROUP COMES AFTER UPDATES.
    const order = list.getAllByRole("region").map((g) => g.textContent ?? "");
    expect(order[1]).toMatch(/^Updates/);
    expect(order[2]).toMatch(/^Answered/);
  });

  test("a link to an answered ask opens its group and its pane, with what happened next", async () => {
    location.hash = "#/inbox?row=ask%3Ac-20";
    mount({
      agents: [{ id: "cto", agent_id: "cto", handle: "cto", role: "CTO", activity: "idle" }],
      answers: {
        answered_decisions: {
          handle: "jane",
          items: [
            answeredItem({
              followed_up: {
                at: new Date(Date.now() - 30 * 60_000).toISOString(),
                kind: "status",
                fields: { status: { from: "todo", to: "in_progress" } },
              },
            }),
          ],
        },
      },
    });
    await settle();
    await settle();
    expect(screen.getByRole("button", { name: "Answered, 1" }).getAttribute("aria-expanded")).toBe(
      "true",
    );
    expect(byline()).toBe("CTO asked for your approval on LEAD-20 Q4 release plan");
    expect(document.querySelector(".crewlet-reading-pane__outcome")?.textContent).toBe(
      "Maya Ops chose Ship anyway",
    );
    expect(screen.getByText(/Maya Ops answered for you at/)).toBeTruthy();
    const steps = within(screen.getByRole("list", { name: "What CTO did with the answer" }));
    expect(steps.getByText("CTO acted on it")).toBeTruthy();
    expect(steps.getByText(/In progress/)).toBeTruthy();
    // NOTHING TO ANSWER AGAIN: the pane draws no options.
    expect(screen.queryByRole("group", { name: "Answers" })).toBeNull();
  });

  test("a row answered here is in Needs you, never in the Answered group as well", async () => {
    const mine = answeredItem({
      ask: { ...askItem().ask, open: false },
      answer: { by: "jane", at: new Date().toISOString(), choice: "hold", body: "Hold release" },
    });
    let answeredYet = false;
    location.hash = "#/inbox?row=ask%3Ac-12";
    mount({
      answers: {
        decisions: decisionsOf(askItem()),
        answered_decisions: () =>
          Promise.resolve({ handle: "jane", items: answeredYet ? [mine] : [] }),
      },
    });
    await settle();
    answeredYet = true;
    fireEvent.click(screen.getByRole("button", { name: /Hold release/ }));
    await settle();
    await settle();
    expect(screen.queryByRole("button", { name: /^Answered, / })).toBeNull();
    expect(
      within(screen.getByRole("region", { name: /^Needs you/ })).getAllByRole("button", {
        name: /Answered$/,
      }),
    ).toHaveLength(1);
  });

  // A CODING RUN'S QUESTION, ONCE ANSWERED, IS READ BACK AS AN ASK IS. The
  // engine lists it in `answered_decisions` beside the work items' asks, and
  // the Inbox drew none of it: the row read the item's `ask`, which a run's
  // carries none of. It is the same row and the same pane, with the run's
  // options in place of a conversation it never had.
  //
  // Mutation: key a run's answered row by its turn alone, drop its options
  // or their mark, or draw the reply box, and a case below fails.
  describe("a coding run's question", () => {
    function answeredRun(answer: Record<string, unknown> = {}) {
      return {
        at: new Date(Date.now() - 60 * 60_000).toISOString(),
        run: {
          turn_id: "turn-7",
          launch_id: "q1",
          agent_handle: "cto",
          question: "Which branch should the fix target?",
          question_context: "The release branch is **frozen** until Friday.",
          decision: {
            question: "Which branch should the fix target?",
            options: [
              { id: "release", label: "Target the release branch" },
              { id: "main", label: "Target main", detail: "Backported after the freeze" },
            ],
            recommended: "main",
            role: "approver",
          },
          asked_at: new Date(Date.now() - 70 * 60_000).toISOString(),
          work_item: { backend: "native", id: "t-12", key: "LEAD-12", project: "LEAD" },
        },
        answer: {
          by: "jane",
          by_kind: "human",
          at: new Date(Date.now() - 60 * 60_000).toISOString(),
          choice: "main",
          body: "Target main",
          ...answer,
        },
      };
    }

    /** The Answered group, opened. */
    async function openAnswered() {
      fireEvent.click(screen.getByRole("button", { name: "Answered, 1" }));
      await settle();
      return within(screen.getByRole("region", { name: "Answered, 1" }));
    }

    test("answered with an option is listed and read in full as an answered ask", async () => {
      mount({ answers: { answered_decisions: { handle: "jane", items: [answeredRun()] } } });
      await settle();
      const row = (await openAnswered()).getByRole("button", { name: /Answered$/ });
      expect(row.textContent).toContain("CTO");
      expect(row.textContent).toContain("Which branch should the fix target?");
      expect(row.textContent).toContain("You chose Target main");
      // AND WHEN, as an answered ask's row keeps the time it was given.
      expect(row.textContent).toContain("1h");
      fireEvent.click(row);
      await settle();
      expect(location.hash).toBe("#/inbox?row=run%3Aturn-7%3Aq1");
      expect(byline()).toBe("CTO’s coding run asked for your approval on LEAD-12");
      expect(
        screen.getByRole("heading", { name: "Which branch should the fix target?" }),
      ).toBeTruthy();
      // THE CONTEXT IS MARKDOWN.
      const lead = document.querySelector(".crewlet-reading-pane__lead");
      expect(lead?.querySelector("strong")?.textContent).toBe("frozen");
      // THE OPTIONS AS THEY WERE OFFERED, the chosen one marked, none sendable,
      // and the recommended one still saying so: chosen, it keeps its mark.
      const answers = within(screen.getByRole("group", { name: "Answers" }));
      const chosen = answers.getByRole("button", { pressed: true });
      expect(chosen.textContent).toContain("Target main");
      expect(chosen.textContent).toContain("Recommended");
      expect(answers.getByRole("button", { pressed: false }).textContent).toContain(
        "Target the release branch",
      );
      for (const option of answers.getAllByRole("button")) {
        expect(option.getAttribute("aria-disabled")).toBe("true");
      }
      fireEvent.click(chosen);
      await settle();
      expect(posted).toEqual([]);
      expect(document.querySelector(".crewlet-reading-pane__outcome")?.textContent).toBe(
        "You chose Target main (Recommended)",
      );
      expect(screen.getByRole("link", { name: "Open the run" }).getAttribute("href")).toBe(
        "#/live/runs/turn-7",
      );
      // NO CONVERSATION AND NO REPLY BOX: a run's question has no thread.
      expect(screen.queryByText("Replies to this question")).toBeNull();
      expect(screen.queryByRole("textbox")).toBeNull();
      expect(screen.queryByText("What happened next")).toBeNull();
      expect(asked.some((a) => a.kind === "work_comments")).toBe(false);
    });

    test("answered in words says what was written, and marks no option", async () => {
      mount({
        answers: {
          answered_decisions: {
            handle: "jane",
            items: [answeredRun({ choice: undefined, body: "Neither, hold it until Friday." })],
          },
        },
      });
      await settle();
      const row = (await openAnswered()).getByRole("button", { name: /Answered$/ });
      expect(row.textContent).toContain("You replied “Neither, hold it until Friday.”");
      fireEvent.click(row);
      await settle();
      expect(document.querySelector(".crewlet-reading-pane__outcome")?.textContent).toBe(
        "You replied “Neither, hold it until Friday.”",
      );
      const answers = within(screen.getByRole("group", { name: "Answers" }));
      expect(answers.queryByRole("button", { pressed: true })).toBeNull();
      // WHAT WAS RECOMMENDED STILL SHOWS, though the person chose none of it.
      expect(answers.getAllByRole("button")[0]!.textContent).toMatch(/^Target main .*Recommended$/);
    });

    // A CHOICE WITH WORDS OF ITS OWN is read back with both: the record holds
    // the words where the person wrote any beside the option, and the row and
    // the pane said only the option.
    //
    // Mutation: drop the words beside a choice, and neither says them.
    test("answered with an option and words says both, in the row and the pane", async () => {
      mount({
        answers: {
          answered_decisions: {
            handle: "jane",
            items: [answeredRun({ body: "Backport it after the freeze." })],
          },
        },
      });
      await settle();
      const row = (await openAnswered()).getByRole("button", { name: /Answered$/ });
      expect(row.textContent).toContain(
        "You chose Target main and wrote “Backport it after the freeze.”",
      );
      fireEvent.click(row);
      await settle();
      expect(document.querySelector(".crewlet-reading-pane__outcome")?.textContent).toBe(
        "You chose Target main (Recommended) and wrote “Backport it after the freeze.”",
      );
    });

    // AN ANSWERED RUN'S CONTEXT IS STILL AN AGENT'S WORDS, read by the
    // `checked` rule: a link only where the dashboard built it.
    //
    // Mutation: render the context with live links, and the evil address is
    // an anchor.
    test("reads its context's links by the checked rule", async () => {
      const item = answeredRun();
      item.run.question_context =
        "See https://evil.example.net/c?q=secret and https://github.com/gophrase/backend/pull/11.";
      mount({
        answers: {
          answered_decisions: { handle: "jane", items: [item] },
          link_hosts: {
            hosts: [{ kind: "github", url: "https://github.com", owners: ["gophrase"] }],
          },
        },
      });
      await settle();
      fireEvent.click((await openAnswered()).getByRole("button", { name: /Answered$/ }));
      await settle();
      const lead = document.querySelector(".crewlet-reading-pane__lead") as HTMLElement;
      const hrefs = [...lead.querySelectorAll("a")].map((a) => a.getAttribute("href"));
      expect(hrefs).toEqual(["https://github.com/gophrase/backend/pull/11"]);
      expect(lead.textContent).toContain("https://evil.example.net/c?q=secret");
    });

    // THE SAME QUESTION IS NEVER LISTED TWICE. The two reads are asked apart,
    // and `decisions` can still list a run parked on a question the person
    // answered a moment ago (in another tab, say) once `answered_decisions`
    // lists the answer. Its open row is keyed by its turn alone and its
    // answered one by the launch as well, so the two never met by key.
    test("an open run row and the answer to its question are not both listed", async () => {
      const parked = {
        kind: "run",
        at: new Date(Date.now() - 70 * 60_000).toISOString(),
        run: {
          turn_id: "turn-7",
          launch_id: "q1",
          agent_handle: "cto",
          status: "awaiting_clarification",
          question: "Which branch should the fix target?",
          question_context: "",
          paused_at: new Date(Date.now() - 70 * 60_000).toISOString(),
          updated_at: new Date(Date.now() - 70 * 60_000).toISOString(),
          pause_ttl_seconds: 3_600,
        },
      };
      mount({
        answers: {
          decisions: decisionsOf(parked),
          answered_decisions: { handle: "jane", items: [answeredRun()] },
        },
      });
      await settle();
      expect(screen.queryByRole("region", { name: /^Needs you/ })).toBeNull();
      expect(screen.getByRole("button", { name: "Answered, 1" })).toBeTruthy();
      // A NEW QUESTION ON THE SAME TURN still waits.
      cleanup();
      mount({
        answers: {
          decisions: decisionsOf({ ...parked, run: { ...parked.run, launch_id: "q2" } }),
          answered_decisions: { handle: "jane", items: [answeredRun()] },
        },
      });
      await settle();
      expect(
        within(screen.getByRole("region", { name: /^Needs you/ })).getAllByRole("listitem"),
      ).toHaveLength(1);
    });

    test("answered here stays in Needs you with its option marked, never under Answered as well", async () => {
      const parked = {
        kind: "run",
        at: new Date(Date.now() - 5 * 60_000).toISOString(),
        run: {
          ...answeredRun().run,
          status: "awaiting_clarification",
          paused_at: new Date(Date.now() - 5 * 60_000).toISOString(),
          updated_at: new Date(Date.now() - 5 * 60_000).toISOString(),
          pause_ttl_seconds: 3_600,
        },
      };
      actAnswers = {
        answer_run: { outcome: "pending", receipt: { launch_id: "q1", answer_id: "a1" } },
      };
      const sent = () => posted.some((p) => p.tool === "answer_run");
      location.hash = "#/inbox?row=run%3Aturn-7";
      mount({
        answers: {
          decisions: () => Promise.resolve(sent() ? decisionsOf() : decisionsOf(parked)),
          answered_decisions: () =>
            Promise.resolve({ handle: "jane", items: sent() ? [answeredRun()] : [] }),
          sandbox_runs: () =>
            Promise.resolve({
              runs: [
                sent() ? { ...parked.run, status: "resumed", answer_ids: ["a1"] } : parked.run,
              ],
            }),
        },
      });
      await settle();
      fireEvent.click(screen.getByRole("button", { name: /Target main/ }));
      await settle();
      await settle();
      // THE OPTION PRESSED IS MARKED, as the answered pane marks it.
      const answers = within(screen.getByRole("group", { name: "Answers" }));
      expect(answers.getByRole("button", { pressed: true }).textContent).toContain("Target main");
      expect(screen.queryByRole("button", { name: "Something else" })).toBeNull();
      expect(screen.queryByRole("button", { name: /^Answered, / })).toBeNull();
      expect(
        within(screen.getByRole("region", { name: /^Needs you/ })).getAllByRole("listitem"),
      ).toHaveLength(1);
    });

    // ASKED ON EVERY COMPANY: a coding run's question is answered here
    // whatever runs the tracker, and so is read back here.
    test("is listed where the company runs another tracker", async () => {
      mount({
        services: { tracker: "jira", knowledge: "doclet" },
        answers: { answered_decisions: { handle: "jane", items: [answeredRun()] } },
      });
      await settle();
      const row = (await openAnswered()).getByRole("button", { name: /Answered$/ });
      expect(row.textContent).toContain("You chose Target main");
    });
  });
});

// THE INBOX DOES NOT FLICKER. While a spent subscription was retried once a
// second, the engine pushed every seat's state about thirteen times a second,
// each seat flipping between working, stopped and a failed turn, and every
// flip took one row out of Needs you and put another in (two ids for one seat),
// moving every row below it. A burst of pushes that changes nothing a person
// sees must leave every row where it was, the same element, and a seat moving
// between stopped and failed is one row whose words change.
describe("a burst of live pushes", () => {
  const limit = {
    parked: true,
    resets_at: "2026-10-27T23:30:00Z",
    since: "2026-10-27T23:16:00Z",
    cli: "claude-code",
    subscription: true,
  };
  function failed(n: number) {
    return {
      kind: "rate_limit",
      detail: "llm chain exhausted after 1 providers (default)",
      phase: "execute",
      turn_id: `t-${n}`,
      at: new Date(Date.now() - 60_000 + n).toISOString(),
      event_id: `e-${n}`,
    };
  }
  const CTO = {
    id: "cto",
    agent_id: "cto",
    handle: "cto",
    role: "CTO",
    activity: "stopped",
    stopped_reason: "provider",
  };
  const PM = {
    id: "pm",
    agent_id: "pm",
    handle: "pm",
    role: "PM",
    activity: "stopped",
    stopped_reason: "unplaced",
  };

  /** Every control in Needs you, in order: a row remounted or moved is a
   *  different element at its place. */
  function rows(): HTMLElement[] {
    return within(screen.getByRole("region", { name: /^Needs you/ })).getAllByRole("button");
  }

  test("moves no row, remounts none, and says the limit once", async () => {
    const { store } = mount({
      agents: [{ ...CTO, provider_limit: limit, last_error: failed(0) }, PM],
      answers: { decisions: decisionsOf(askItem()) },
    });
    await settle();
    const before = rows();
    const names = before.map((r) => r.textContent);
    expect(names.some((n) => n?.includes("Claude Code subscription limit reached"))).toBe(true);
    // NOT ONCE PER SEAT: the parked seat draws no stop or failed turn of its own.
    expect(names.filter((n) => /CTO is stopped|CTO's last turn failed/.test(n ?? ""))).toHaveLength(
      0,
    );

    for (let i = 1; i <= 25; i++) {
      await act(async () => {
        store.applyAgents([
          // The parked seat's probe: its live state moves, its condition
          // does not.
          {
            agent_id: "cto",
            activity: "stopped",
            stopped_reason: "provider",
            provider_limit: limit,
            last_error: failed(i),
            live_call_seq: i,
          },
          // A seat re-sent exactly as it was.
          { agent_id: "pm", activity: "stopped", stopped_reason: "unplaced" },
        ]);
      });
    }
    const after = rows();
    expect(after).toHaveLength(before.length);
    after.forEach((row, i) => expect(row, `row ${i} was remounted or moved`).toBe(before[i]));
  });

  test("a seat moving between stopped and a failed turn is one row in one place", async () => {
    const { store } = mount({
      agents: [PM],
      answers: { decisions: decisionsOf(askItem()) },
    });
    await settle();
    const before = rows();
    for (let i = 1; i <= 6; i++) {
      await act(async () => {
        store.applyAgents([
          i % 2
            ? { agent_id: "pm", activity: "idle", stopped_reason: null, last_error: failed(i) }
            : { agent_id: "pm", activity: "stopped", stopped_reason: "unplaced", last_error: null },
        ]);
      });
      const now = rows();
      expect(now).toHaveLength(before.length);
      now.forEach((row, n) =>
        expect(row, `flip ${i}: row ${n} was remounted or moved`).toBe(before[n]),
      );
    }
  });
});
