/**
 * Doclet Home: the landing, the ranked answer, and a space's peek.
 *
 * What these protect, each one a fix or a rule of the approved design:
 *
 *  - A SNIPPET IS PROSE, drawn as what its markdown renders to and never with
 *    its `#` and `**` in it, and the words a search matched are marked in it.
 *  - THE MODE ON THE WIRE IS THE ENGINE'S WORD: "Meaning" is read, `semantic`
 *    is sent.
 *  - A PARTIAL SEARCH SAYS SO, naming the node that did not answer.
 *  - THE COPY IS TRUE OF THE BACKEND: it never claims a Doclet search has no
 *    copy of its own to read.
 *  - AN ADDRESS NAMING NO MODE RUNS IN ONE THE ENGINE SERVES, waiting for the
 *    probe rather than running Hybrid and reporting its degradation.
 *  - A PHRASE PAST THE SEARCH BOUND IS NOT ASKED, and the screen says why.
 *  - THE RESULTS ARE GROUPED BY SPACE IN THE ENGINE'S ORDER, and j and k move
 *    the choice the preview reads.
 *  - THE LANDING DRAWS WHOEVER THE RECORD NAMES: a seat by the chart's name
 *    for it, a credential bound to no seat by its login, and the engine's own
 *    change as the engine. Its days are the company's, cut at the company's
 *    midnight.
 *  - WATCHING IS ASKED BY THE SEAT THE READER ACTS AS, so a credential bound
 *    to no seat, which a watch never subscribes, asks for nothing.
 *  - A SPACE'S PEEK COUNTS OFF THE TOTAL, never off the rows one read
 *    returned, and WHO FILES THERE HAS FOUR STATES: "needs Admin" is
 *    said only to a reader who does not hold it, never to one whose read of
 *    the company document is merely in flight.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Doclet } from "./Doclet.tsx";
import { ContainerPeek } from "./peeks.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings, Shell } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { recentsKey, resetForTest as resetRecents } from "~/lib/recents.ts";
import { resetForTest as resetStars, starsKey } from "~/lib/starred.ts";
import { noteReader } from "~/lib/reader.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { SEARCH_QUERY_MAX } from "~/contract/wire.ts";
import { withDerived } from "~/test/org.ts";

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
  location.hash = "#/doclet?q=runbook";
});

afterEach(() => {
  cleanup();
  try {
    localStorage.clear();
    sessionStorage.clear();
  } catch {
    // no storage in this environment is fine
  }
  resetStars();
  resetRecents();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const ORG = {
  name: "Acme",
  timezone: "Asia/Tokyo",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Crewlet-SWE", handle: "swe", kind: "agent" },
  ],
  units: [],
};

/** The person signed in on this browser, who writes as their seat. */
const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["save_page", "comment_on_page", "write_page"],
};

/** What the frame itself reads, answered for every screen. */
const FRAME: Record<string, unknown> = {
  viewer: JANE,
  inbox: { handle: "jane", notices: [], primary_reasons: [], unread: 0, primary: 0 },
  decisions: { handle: "jane", items: [], total: 0, capped: false },
};

const hit = (id: string, title: string, container: string, snippet = "") => ({
  id,
  title,
  url: "",
  container,
  snippet,
  updated_at: "",
});

const ran = {
  backend: "native",
  query: "runbook",
  mode: "hybrid",
  served_mode: "hybrid",
  modes: ["hybrid", "keyword", "semantic"],
  degraded: "",
  coverage: {
    nodes: [{ id: "node-1", answered: true, error: "" }],
    complete: true,
    buckets_missing: 0,
  },
  available: true,
  reason: "",
  note: "",
  hits: [
    hit(
      "p-1",
      "Provisioner runbook",
      "ENG",
      "## Recovery **Drain** the node, then `crewlet run` again",
    ),
  ],
};

type Answer = unknown | ((params: Record<string, unknown>) => unknown);

/** Render a screen against a socket answering each question from `answers`. */
function mount(
  answer: Record<string, unknown>,
  containers: unknown[] = [],
  answers: Record<string, Answer> = {},
  view = <Doclet />,
) {
  const store = new Store();
  store.applyOrg(withDerived(ORG) as never);
  const socket = new LiveSocket(store);
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params: Record<string, unknown>) => {
    asked.push({ what, params });
    const all: Record<string, Answer> = { ...FRAME, ...answers };
    if (what in all) {
      const a = all[what];
      return Promise.resolve(typeof a === "function" ? (a as (p: unknown) => unknown)(params) : a);
    }
    if (what === "knowledge") return Promise.resolve(answer);
    if (what === "containers") return Promise.resolve({ containers });
    return Promise.resolve({});
  }) as typeof socket.query;
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>{view}</Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return asked;
}

// ---------------------------------------------------------------------------
// The ranked answer
// ---------------------------------------------------------------------------

test("a hit's snippet is drawn as prose, and the words searched for are marked", async () => {
  mount(ran);
  await waitFor(() => expect(document.querySelector(".knowledge-hit-snippet")).not.toBeNull());
  const snippet = document.querySelector(".knowledge-hit-snippet");
  expect(snippet?.textContent).toBe("Recovery Drain the node, then crewlet run again");
  // THE TITLE CARRIES THE MARK: "runbook" is what was searched for.
  const marked = [...document.querySelectorAll(".knowledge-hit-title mark")].map(
    (m) => m.textContent,
  );
  expect(marked).toEqual(["runbook"]);
});

test("Meaning is sent as semantic, and the results say what ranked them", async () => {
  location.hash = "#/doclet?q=runbook&mode=semantic";
  const asked = mount({ ...ran, mode: "semantic", served_mode: "semantic" });
  await waitFor(() => expect(screen.getByText(/ranked by Meaning/)).toBeTruthy());
  const search = asked.find((a) => a.what === "knowledge" && a.params.q === "runbook");
  expect(search?.params.mode).toBe("semantic");
});

test("a search served in another mode says what it served and why", async () => {
  mount({ ...ran, served_mode: "keyword", modes: ["keyword"], degraded: "no_embeddings" });
  await waitFor(() => expect(screen.getByText(/Asked for Hybrid, served Keyword/)).toBeTruthy());
  expect(screen.getByText(/no embeddings provider/)).toBeTruthy();
});

test("a partial fan-out is a callout naming the node that did not answer", async () => {
  mount({
    ...ran,
    coverage: {
      nodes: [
        { id: "node-1", answered: true, error: "" },
        { id: "node-2", answered: false, error: "no answer inside the budget" },
      ],
      complete: false,
      buckets_missing: 21,
    },
  });
  await waitFor(() => expect(screen.getByText(/This search is partial/)).toBeTruthy());
  expect(screen.getByText(/21 of the corpus's buckets went unsearched/)).toBeTruthy();
  expect(screen.getByText("node-2")).toBeTruthy();
});

// NOT SENT, because the engine refuses it rather than cutting it, and a
// refusal drawn as a failed search reads as a company with nothing written.
test("a phrase past the search bound is not asked, and the screen says why", async () => {
  location.hash = `#/doclet?q=${"p".repeat(SEARCH_QUERY_MAX + 1)}&mode=keyword`;
  const asked = mount(ran);
  expect(
    await screen.findByText(
      `This is ${SEARCH_QUERY_MAX + 1} bytes, and a search takes at most ${SEARCH_QUERY_MAX}. Search on a few keywords or a phrase, not a pasted passage.`,
    ),
  ).toBeTruthy();
  expect(asked.filter((a) => a.what === "knowledge" && a.params.q !== "")).toEqual([]);
  expect(document.querySelector(".knowledge-hit")).toBeNull();
});

test("a complete search draws no partial callout", async () => {
  mount(ran);
  await waitFor(() => expect(document.querySelector(".knowledge-hit")).not.toBeNull());
  expect(screen.queryByText(/This search is partial/)).toBeNull();
});

test.each([
  [
    "no_scope",
    "the confluence backend is configured but knowledge.scope lists no container",
    "Search can’t run: the confluence backend is configured but knowledge.scope lists no container. Add spaces to knowledge.scope.",
  ],
  [
    "no_backend",
    "this company has no knowledge backend",
    "Search can’t run: this company has no knowledge backend. Set knowledge.backend to doclet or confluence.",
  ],
  // NOT A MISCONFIGURATION, so no remedy and no note: the node is indexing.
  ["building", "this node is still indexing", "Still indexing. Try again shortly."],
])(
  "a search that did not run (%s) says why, with the remedy its reason names",
  async (reason, note, said) => {
    mount({ ...ran, hits: [], served_mode: "", available: false, reason, note });
    const callout = await waitFor(() => {
      const found = [...document.querySelectorAll("span")].find((el) =>
        el.textContent?.startsWith(said.slice(0, 16)),
      );
      expect(found).toBeTruthy();
      return found!;
    });
    expect(callout.textContent).toBe(said);
  },
);

test("the results end with how to move through them, and never claim there is no copy", async () => {
  mount(ran, [{ key: "ENG", name: "Engineering", pages: 3 }]);
  await waitFor(() => expect(document.querySelector(".knowledge-hit")).not.toBeNull());
  expect(document.body.textContent).not.toMatch(/no local copy|no staleness window/i);
  expect(document.querySelector(".knowledge-results-foot")?.textContent).toBe(
    "j k move, Enter opens.",
  );
});

// THE ENGINE'S RANK, KEPT INSIDE EACH SPACE: grouping orders the spaces by
// their best hit and never reorders a space's own hits.
test("the results are grouped by space in the engine's order, and Ranked is the one list", async () => {
  mount(
    {
      ...ran,
      hits: [
        hit("p-1", "Provisioner runbook", "ENG"),
        hit("p-2", "Billing runbook", "OPS"),
        hit("p-3", "Deploy runbook", "ENG"),
      ],
    },
    [
      { key: "ENG", name: "Engineering", pages: 3 },
      { key: "OPS", name: "Operations", pages: 1 },
    ],
  );
  const list = await screen.findByRole("region", { name: "Results" });
  await waitFor(() => expect(within(list).getAllByRole("heading", { level: 3 }).length).toBe(2));
  expect(
    within(list)
      .getAllByRole("heading", { level: 3 })
      .map((h) => h.textContent),
  ).toEqual(["EngineeringENG2 results", "OperationsOPS1 result"]);
  const ranks = () =>
    [...list.querySelectorAll(".knowledge-hit-rank")].map((r) => r.textContent ?? "");
  expect(ranks()).toEqual(["1", "3", "2"]);

  fireEvent.click(within(list).getByRole("radio", { name: "Ranked" }));
  await waitFor(() => expect(ranks()).toEqual(["1", "2", "3"]));
});

test("j and k move the chosen result, and the preview reads the chosen page", async () => {
  const asked = mount(
    {
      ...ran,
      hits: [hit("p-1", "Provisioner runbook", "ENG"), hit("p-2", "Deploy runbook", "ENG")],
    },
    [{ key: "ENG", name: "Engineering", pages: 2 }],
    {
      page: (params: Record<string, unknown>) => ({
        page: {
          id: params.id,
          container: "ENG",
          title: params.id === "p-1" ? "Provisioner runbook" : "Deploy runbook",
          status: "published",
          version: 3,
          author: "jane",
          updated_at: "2026-09-24T22:00:00Z",
          revision: 9,
          body: "The runbook body.",
        },
        revision: 9,
        skill: false,
        onboarding: false,
        history: [
          { version: 3, author: "swe", created_at: "2026-09-24T22:00:00Z", message: "Fix step 2" },
        ],
      }),
    },
  );
  const preview = await screen.findByRole("region", { name: "Preview" });
  await waitFor(() =>
    expect(within(preview).getByRole("heading", { name: "Provisioner runbook" })).toBeTruthy(),
  );
  // THE SEAT THE AUTHOR WROTE AS, by the chart's name for it.
  expect(within(preview).getByText("Jane Founder")).toBeTruthy();
  fireEvent.keyDown(screen.getByRole("region", { name: "Results" }), { key: "j" });
  await waitFor(() =>
    expect(within(preview).getByRole("heading", { name: "Deploy runbook" })).toBeTruthy(),
  );
  expect(asked.filter((a) => a.what === "page").map((a) => a.params.id)).toContain("p-2");
});

test("an address naming no mode runs in a mode the engine serves, never a degraded default", async () => {
  location.hash = "#/doclet?q=runbook";
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: Record<string, unknown>[] = [];
  // EVERY PROBE IS HELD: the page bar's search asks one too.
  const probes: ((v: unknown) => void)[] = [];
  socket.query = ((what: string, params: Record<string, unknown>) => {
    if (what in FRAME) return Promise.resolve(FRAME[what]);
    if (what !== "knowledge") return Promise.resolve({ containers: [] });
    asked.push(params);
    if (params.q === "") return new Promise<unknown>((resolve) => probes.push(resolve));
    return Promise.resolve({ ...ran, mode: "keyword", served_mode: "keyword", modes: ["keyword"] });
  }) as typeof socket.query;
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>
              <Doclet />
            </Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  // NOTHING RUNS BEFORE THE PROBE SAYS WHICH MODES EXIST.
  await waitFor(() => expect(asked.some((p) => p.q === "")).toBe(true));
  expect(asked.some((p) => p.q === "runbook")).toBe(false);
  for (const answer of probes) {
    answer({
      ...ran,
      hits: [],
      query: "",
      served_mode: "",
      modes: ["keyword"],
      degraded: "no_embeddings",
    });
  }
  await waitFor(() => expect(document.querySelector(".knowledge-hit")).not.toBeNull());
  const search = asked.filter((p) => p.q === "runbook");
  expect(search.map((p) => p.mode)).toEqual(["keyword"]);
  // AND NOTHING SAYS IT WAS DEGRADED: nobody asked for Hybrid.
  expect(screen.queryByText(/Asked for Hybrid/)).toBeNull();
});

test("a mode the reader named is kept even where the engine would degrade it", async () => {
  location.hash = "#/doclet?q=runbook&mode=hybrid";
  const asked = mount({
    ...ran,
    modes: ["keyword"],
    served_mode: "keyword",
    degraded: "no_embeddings",
  });
  await waitFor(() => expect(screen.getByText(/Asked for Hybrid, served Keyword/)).toBeTruthy());
  expect(asked.find((a) => a.what === "knowledge" && a.params.q === "runbook")?.params.mode).toBe(
    "hybrid",
  );
});

// ---------------------------------------------------------------------------
// The landing
// ---------------------------------------------------------------------------

// A PAGE IS STARRED FROM ITS OWN MORE MENU. The frame's star in the landing's
// bar offered the workspace the sidebar already lists, beside a design that
// keeps no star of its own.
test("the landing's page bar draws no star", async () => {
  location.hash = "#/doclet";
  const store = new Store();
  store.applyOrg(withDerived(ORG) as never);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  socket.query = (() => new Promise(() => {})) as typeof socket.query;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>
          <Doclet />
        </Shell>
      </Router>
    </ClientContext.Provider>,
  );
  // THE BAR IS DRAWN, with the screen's own controls in it.
  expect(await screen.findByRole("search", { name: "Search the knowledge base" })).toBeTruthy();
  expect(screen.getByRole("button", { name: /New page/ })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Starred/ })).toBeNull();
});

test("the spaces row links every space, with its key, its count and its purpose", async () => {
  location.hash = "#/doclet";
  mount({ ...ran, hits: [], served_mode: "" }, [
    { key: "ENG", name: "Engineering", pages: 412, purpose: "How the platform runs" },
    { key: "PROD", name: "Product", pages: 0 },
  ]);
  const row = await screen.findByRole("navigation", { name: "Spaces" });
  const eng = within(row).getByRole("link", { name: /Engineering/ });
  expect(eng.getAttribute("href")).toBe("#/doclet/ENG");
  expect(eng.getAttribute("title")).toBe("Engineering: 412 pages. How the platform runs");
  expect(
    within(row)
      .getByRole("link", { name: /Product/ })
      .getAttribute("title"),
  ).toBe("Product: 0 pages");
});

/** The landing at 09:00 on the 25th in Tokyo, answering `answers`. */
function landing(answers: Record<string, Answer>) {
  location.hash = "#/doclet";
  vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-09-25T00:00:00Z"));
  return mount(
    { ...ran, hits: [], served_mode: "" },
    [{ key: "ENG", name: "Engineering", pages: 3 }],
    answers,
  );
}

/** One change to page `page_id`, a save by Crewlet-SWE unless `over` says otherwise. */
function written(id: string, at: string, over: Record<string, unknown> = {}) {
  return {
    id,
    page_id: "p-1",
    title: "Settlement algorithm",
    container: "ENG",
    kind: "saved",
    actor: "swe",
    at,
    log_seq: 0,
    ...over,
  };
}

/** A page's detail as the `page` question answers it. */
function detailOf(id: string, title: string, body: string, history: unknown[] = []) {
  return {
    page: {
      id,
      container: "ENG",
      title,
      status: "published",
      version: history.length,
      author: "swe",
      updated_at: "2026-09-24T23:00:00Z",
      revision: 9,
      body,
    },
    revision: 9,
    skill: false,
    onboarding: false,
    history,
  };
}

// THE NEWEST CHANGE LEADS, named by the seat its writer wrote as; the feed
// beside it holds the rest under the COMPANY's days.
test("the newest change leads in its own tile, and the feed holds the rest", async () => {
  landing({
    page_activity: {
      changes: [
        written("c-1", "2026-09-24T23:00:00Z", {
          title: "Provisioner runbook",
          actor: "jane",
          actor_kind: "human",
        }),
        written("c-2", "2026-09-24T13:00:00Z", {
          page_id: "p-2",
          title: "Deploy notes",
          kind: "comment",
        }),
      ],
    },
  });
  const latest = await screen.findByRole("article", { name: "Newest change" });
  await waitFor(() => expect(within(latest).getByText("Jane Founder")).toBeTruthy());
  expect(within(latest).getByRole("heading", { level: 2 }).textContent).toBe("Provisioner runbook");
  expect(within(latest).queryByText("jane")).toBeNull();
  // 08:00 on the 25th in Tokyo: today, though it is the 24th in UTC.
  expect(latest.querySelector(".knowledge-latest-who time")?.textContent).toBe("Today 08:00");
  expect(
    within(latest)
      .getByRole("link", { name: /Open page/ })
      .getAttribute("href"),
  ).toBe("#/doclet/pages/p-1");

  const feed = screen.getByRole("region", { name: /Recently written/ });
  expect(
    within(feed)
      .getAllByRole("heading", { level: 3 })
      .map((h) => h.textContent),
  ).toEqual(["Yesterday"]);
  const rows = within(feed).getAllByRole("listitem");
  expect(rows).toHaveLength(1);
  expect(rows[0]?.textContent).toMatch(/Deploy notes.*Crewlet-SWE commented/);
  // THE LEAD IS NEVER LISTED AGAIN beside itself.
  expect(feed.textContent).not.toContain("Provisioner runbook");
  // EVERY CHANGE is the audit log, narrowed to Doclet's history.
  expect(within(feed).getByRole("link", { name: "Every change" }).getAttribute("href")).toBe(
    "#/settings/audit?kind=doclet",
  );
});

// OPEN TO READ: what each save said, the lines the newest save changed as the
// page's History draws them, and the page's opening, its title said once.
test("the newest saves are open to read: each note, the lines changed, and the opening", async () => {
  const asked = landing({
    page_activity: {
      changes: [
        written("c-2", "2026-09-24T23:00:00Z", { excerpt: "Breaks ties by join order" }),
        written("c-1", "2026-09-24T22:10:00Z", { excerpt: "Adds a worked example" }),
        written("c-0", "2026-09-24T13:00:00Z", {
          page_id: "p-2",
          title: "Split modes",
          kind: "created",
          excerpt: "Split modes",
        }),
      ],
    },
    page: (params: Record<string, unknown>) =>
      params.id === "p-1"
        ? detailOf(
            "p-1",
            "Settlement algorithm",
            "# Settlement algorithm\n\nNets every balance first.\n\n## Ties\n\nEqual balances are ordered by join order.\n",
            [
              { version: 3, author: "swe", created_at: "2026-09-24T23:00:00Z" },
              { version: 2, author: "swe", created_at: "2026-09-24T22:10:00Z" },
              { version: 1, author: "swe", created_at: "2026-09-20T09:00:00Z" },
            ],
          )
        : {},
    page_revision: (params: Record<string, unknown>) => ({
      page_id: "p-1",
      version: params.version,
      title: "Settlement algorithm",
      body: "# Settlement algorithm\n\nNets every balance first.\n\n## Ties\n\nEqual balances are paid in any order.\n",
      created_at: "2026-09-24T22:10:00Z",
    }),
  });
  const latest = await screen.findByRole("article", { name: "Newest change" });
  await waitFor(() =>
    expect(within(latest).queryByRole("group", { name: "Latest change" })).not.toBeNull(),
  );
  expect(latest.textContent).toContain("Crewlet-SWE saved it twice");
  const notes = [...latest.querySelectorAll(".knowledge-latest-note")].map((n) => n.textContent);
  expect(notes).toEqual(["08:00Breaks ties by join order", "07:10Adds a worked example"]);
  // BY THE WORD READ IN PLACE OF THE MARK, which is what a screen reader hears.
  const lines = (word: "Added" | "Removed") =>
    [...latest.querySelectorAll<HTMLElement>(".diff-view__line")]
      .filter((row) => within(row).queryByText(word))
      .map((row) => row.querySelector(".diff-view__text")?.textContent);
  expect(lines("Removed")).toEqual(["Equal balances are paid in any order."]);
  expect(lines("Added")).toEqual(["Equal balances are ordered by join order."]);
  // THE PAGE'S OWN BODY IS THE SAVE'S, so only the revision before it is read.
  expect(asked.filter((a) => a.what === "page_revision").map((a) => a.params.version)).toEqual([2]);
  const opening = latest.querySelector(".knowledge-latest-opening");
  expect(opening?.textContent).toContain("Nets every balance first.");
  expect(latest.textContent?.match(/Settlement algorithm/g)).toHaveLength(1);
  // THE TWO SAVES ARE ONE ROW, and it leads: the feed holds only the new page.
  const feed = screen.getByRole("region", { name: /Recently written/ });
  expect(within(feed).getAllByRole("listitem")).toHaveLength(1);
  expect(feed.textContent).not.toContain("Settlement algorithm");
});

// A PREVIEW SAYS IT IS ONE: a save that changed more than the tile shows
// counts the rest and links the whole save in the page's History.
test("a long save's lines are a preview that counts the rest and opens the save", async () => {
  const before = Array.from({ length: 12 }, (_, i) => `line ${i}`).join("\n");
  const after = before.replace(/line (\d+)/g, "LINE $1");
  landing({
    page_activity: { changes: [written("c-1", "2026-09-24T23:00:00Z")] },
    page: detailOf("p-1", "Settlement algorithm", after, [
      { version: 5, author: "swe", created_at: "2026-09-24T23:00:00Z" },
      { version: 4, author: "swe", created_at: "2026-09-20T09:00:00Z" },
    ]),
    page_revision: { page_id: "p-1", version: 4, title: "Settlement algorithm", body: before },
  });
  const latest = await screen.findByRole("article", { name: "Newest change" });
  await waitFor(() =>
    expect(within(latest).queryByRole("group", { name: "Latest change" })).not.toBeNull(),
  );
  expect(latest.querySelectorAll(".diff-view__line")).toHaveLength(6);
  // TWENTY-FOUR CHANGED LINES, twelve out and twelve in, and six drawn.
  expect(latest.textContent).toContain("18 more changed lines in this save");
  expect(within(latest).getByRole("link", { name: "this save" }).getAttribute("href")).toBe(
    "#/doclet/pages/p-1?version=5",
  );
});

// A LONG PAGE IS COMPARED, NEVER REPLACED WHOLE: past 2,000 lines the design
// system's line diff answers every line removed and re-added, and the tile
// said a typo fixed on line 1,800 removed the page's first six lines.
test("a save to a long page previews the line it changed, never the page's first lines", async () => {
  const before = Array.from({ length: 2_500 }, (_, i) => `- step ${i}`);
  const after = before.map((line, i) => (i === 1_799 ? "- step 1799, fixed" : line));
  landing({
    page_activity: { changes: [written("c-1", "2026-09-24T23:00:00Z")] },
    page: detailOf("p-1", "Runbook", after.join("\n"), [
      { version: 5, author: "swe", created_at: "2026-09-24T23:00:00Z" },
      { version: 4, author: "swe", created_at: "2026-09-20T09:00:00Z" },
    ]),
    page_revision: { page_id: "p-1", version: 4, title: "Runbook", body: before.join("\n") },
  });
  const latest = await screen.findByRole("article", { name: "Newest change" });
  const diff = await within(latest).findByRole("group", { name: "Latest change" });
  expect(
    [...diff.querySelectorAll<HTMLElement>(".diff-view__line")].map((row) => [
      row.dataset.kind,
      row.querySelector(".diff-view__text")?.textContent,
    ]),
  ).toEqual([
    ["remove", "- step 1799"],
    ["add", "- step 1799, fixed"],
  ]);
  expect(latest.textContent).not.toContain("more changed line");
});

// A COMPARISON NOT MADE IS SAID: a preview of it would list unchanged lines
// as removed, so the tile says the save was not compared and links it.
test("a save the line diff did not compare says so and opens the save", async () => {
  const before = Array.from({ length: 2_001 }, (_, i) => `old ${i}`).join("\n");
  const after = Array.from({ length: 2_001 }, (_, i) => `new ${i}`).join("\n");
  landing({
    page_activity: { changes: [written("c-1", "2026-09-24T23:00:00Z")] },
    page: detailOf("p-1", "Runbook", after, [
      { version: 5, author: "swe", created_at: "2026-09-24T23:00:00Z" },
      { version: 4, author: "swe", created_at: "2026-09-20T09:00:00Z" },
    ]),
    page_revision: { page_id: "p-1", version: 4, title: "Runbook", body: before },
  });
  const latest = await screen.findByRole("article", { name: "Newest change" });
  await waitFor(() =>
    expect(latest.textContent).toContain(
      "The save was not compared line by line: more than 4,000 lines were removed or added. Open this save to read it.",
    ),
  );
  expect(within(latest).queryByRole("group", { name: "Latest change" })).toBeNull();
  expect(latest.querySelector(".diff-view__line")).toBeNull();
  expect(within(latest).getByRole("link", { name: "this save" }).getAttribute("href")).toBe(
    "#/doclet/pages/p-1?version=5",
  );
});

// A DIFF IS OF ONE PAIR OF VERSIONS: a read keeps its last answer while the
// next one is asked, so an answer for another version is never diffed.
test("a revision answered for another version is not diffed against the save", async () => {
  landing({
    page_activity: { changes: [written("c-1", "2026-09-24T23:00:00Z")] },
    page: detailOf("p-1", "Settlement algorithm", "Equal balances are ordered by join order.", [
      { version: 5, author: "swe", created_at: "2026-09-24T23:00:00Z" },
      { version: 4, author: "swe", created_at: "2026-09-20T09:00:00Z" },
    ]),
    page_revision: { page_id: "p-1", version: 3, title: "Settlement algorithm", body: "Older." },
  });
  const latest = await screen.findByRole("article", { name: "Newest change" });
  await waitFor(() => expect(latest.querySelector(".knowledge-latest-opening")).not.toBeNull());
  expect(within(latest).queryByRole("group", { name: "Latest change" })).toBeNull();
});

// A NEW PAGE leads with what it says: no note to repeat its title or its
// first line, and its opening under it.
test("a new page leads with its opening, and no note repeats its title", async () => {
  landing({
    page_activity: {
      changes: [
        written("c-1", "2026-09-24T23:00:00Z", {
          title: "Split modes",
          kind: "created",
          excerpt: "Split modes",
        }),
      ],
    },
    page: detailOf(
      "p-1",
      "Split modes",
      "# Split modes\n\nEqual, exact amounts and percentages.\n",
      [{ version: 1, author: "swe", created_at: "2026-09-24T23:00:00Z" }],
    ),
  });
  const latest = await screen.findByRole("article", { name: "Newest change" });
  await waitFor(() =>
    expect(latest.querySelector(".knowledge-latest-opening")?.textContent).toContain(
      "Equal, exact amounts and percentages.",
    ),
  );
  expect(latest.textContent).toContain("Crewlet-SWE wrote it");
  expect(latest.querySelector(".knowledge-latest-notes")).toBeNull();
  expect(within(latest).queryByRole("group", { name: "Latest change" })).toBeNull();
  expect(latest.textContent?.match(/Split modes/g)).toHaveLength(1);
  // NOTHING ELSE was written: the big tile is the whole story, and no
  // three-row Recently written tile beside it says "nothing else".
  expect(screen.queryByRole("region", { name: /Recently written/ })).toBeNull();
  expect(document.querySelector(".knowledge-tiles")?.getAttribute("data-written")).toBe("none");
});

// A PURGED PAGE keeps its changes and no title: the record that it existed,
// never the lead, and never a link to nowhere.
test("a change to a page that is gone never leads and links nowhere", async () => {
  landing({
    page_activity: {
      changes: [
        written("c-2", "2026-09-24T23:30:00Z", { page_id: "gone", title: "", container: "" }),
        written("c-1", "2026-09-24T23:00:00Z", { title: "Provisioner runbook" }),
      ],
    },
  });
  const latest = await screen.findByRole("article", { name: "Newest change" });
  await waitFor(() =>
    expect(within(latest).getByRole("heading", { level: 2 }).textContent).toBe(
      "Provisioner runbook",
    ),
  );
  const feed = screen.getByRole("region", { name: /Recently written/ });
  const gone = within(feed).getByRole("listitem");
  expect(gone.textContent).toContain("A page that is gone");
  expect(within(gone).queryByRole("link")).toBeNull();
});

test("an empty landing says so once, and each small tile in one line", async () => {
  landing({ page_activity: { changes: [] } });
  const latest = await screen.findByRole("article", { name: "Newest change" });
  await waitFor(() =>
    expect(within(latest).getByText("Nothing has been written yet")).toBeTruthy(),
  );
  const tile = (name: RegExp) => screen.getByRole("region", { name });
  // ONCE: the newest change's tile says nothing has been written, and no
  // three-row Recently written tile beside it says the same (`data-written`).
  expect(screen.queryByRole("region", { name: /Recently written/ })).toBeNull();
  expect(document.querySelector(".knowledge-tiles")?.getAttribute("data-written")).toBe("none");
  expect(
    within(tile(/Continue reading/)).getByText("The pages you open are listed here."),
  ).toBeTruthy();
  expect(within(tile(/Pinned/)).getByText("Star a page to pin it here.")).toBeTruthy();
  await waitFor(() =>
    expect(within(tile(/Watching/)).getByText("Watch a page to follow it.")).toBeTruthy(),
  );
  expect(within(tile(/For the agents/)).getByRole("link", { name: /Agent skills/ })).toBeTruthy();
});

// A KEPT PAGE IS READ FOR WHAT IT IS NOW: a recent carries the label the page
// had when it was opened, and a rename since is drawn by the new title.
test("continue reading draws each page by its title now, and who last saved it", async () => {
  // RECENTS ARE THE READER'S OWN (`lib/recents.ts`): kept under a key naming them.
  noteReader("p-1");
  localStorage.setItem(
    recentsKey("p-1"),
    JSON.stringify([
      {
        // A page's address is its id, and a recent whose path resolves to no
        // route is dropped on read.
        path: ["doclet", "pages", "0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a"],
        label: "Old title",
        workspace: "doclet",
      },
      { path: ["worklet", "ENG-4"], label: "ENG-4", workspace: "worklet" },
    ]),
  );
  resetRecents();
  landing({
    page_activity: { changes: [] },
    page: detailOf("p-1", "Settlement algorithm", "Nets every balance.", [
      { version: 2, author: "jane", created_at: "2026-09-24T22:00:00Z" },
    ]),
  });
  const reading = await screen.findByRole("region", { name: /Continue reading/ });
  await waitFor(() => expect(within(reading).getByText("Settlement algorithm")).toBeTruthy());
  expect(within(reading).queryByText("Old title")).toBeNull();
  expect(within(reading).queryByText("ENG-4")).toBeNull();
  expect(within(reading).getByText("Last saved 2h ago by Jane Founder")).toBeTruthy();
});

test("watching draws who last saved each page, and nothing where no save is named", async () => {
  landing({
    page_activity: { changes: [] },
    pages: {
      pages: [
        {
          id: "p-1",
          title: "Settlement algorithm",
          container: "ENG",
          status: "published",
          version: 3,
          updated_at: "2026-09-24T23:00:00Z",
          revision: 3,
          saved_by: "swe",
          saved_at: "2026-09-24T23:00:00Z",
        },
        {
          id: "p-2",
          title: "Split modes",
          container: "ENG",
          status: "published",
          version: 1,
          updated_at: "2026-09-24T23:00:00Z",
          revision: 1,
          saved_at: "0001-01-01T00:00:00Z",
        },
      ],
      limit: 3,
      total: 2,
    },
  });
  const watching = await screen.findByRole("region", { name: /Watching/ });
  await waitFor(() => expect(within(watching).getByText("Settlement algorithm")).toBeTruthy());
  expect(within(watching).getByText("Last saved 1h ago by Crewlet-SWE")).toBeTruthy();
  const split = within(watching)
    .getAllByRole("listitem")
    .find((row) => row.textContent?.includes("Split modes"));
  expect(split?.textContent).not.toMatch(/Last saved/);
});

// ONE NAME PER WRITER, the one the record carries: a person writes as their
// seat and is drawn by the chart's name for it, a credential bound to no seat
// writes under its login and is drawn as that, and the engine's own change is
// the engine rather than the name it writes under.
test("the landing draws a credential by its login, and the engine's change as the engine", async () => {
  landing({
    page_activity: {
      changes: [
        written("c-1", "2026-09-24T23:00:00Z", {
          title: "Provisioner runbook",
          actor: "jane",
          actor_kind: "human",
        }),
        written("c-2", "2026-09-24T13:00:00Z", {
          page_id: "p-2",
          title: "Release checklist",
          kind: "created",
          actor: "ci.docs",
          actor_kind: "operator",
        }),
        written("c-3", "2026-09-24T12:00:00Z", {
          page_id: "p-3",
          title: "Tool catalogue",
          actor: "crewlet",
          actor_kind: "system",
        }),
      ],
    },
  });
  const feed = await screen.findByRole("region", { name: /Recently written/ });
  await waitFor(() => expect(within(feed).getAllByRole("listitem")).toHaveLength(2));
  const rows = within(feed).getAllByRole("listitem");
  expect(rows[0]?.textContent).toMatch(/Release checklist.*ci\.docs wrote it/);
  expect(rows[1]?.textContent).toMatch(/Tool catalogue.*The engine saved it/);
  expect(feed.textContent).not.toContain("crewlet");
});

// A WATCH IS FILED UNDER A SEAT: a bound reader asks by the seat they act as,
// and a credential bound to none subscribes nobody, so it asks nothing.
test("watching asks by the reader's seat, and asks nothing for an unbound credential", async () => {
  const asked = landing({ page_activity: { changes: [] } });
  await screen.findByRole("region", { name: /Watching/ });
  await waitFor(() =>
    expect(asked.find((a) => a.what === "pages" && "watcher" in a.params)?.params.watcher).toBe(
      "jane",
    ),
  );
  cleanup();

  const unbound = {
    login: "svc:docs",
    grants: ["doclet"],
    handle: "",
    owner: "svc:docs",
    name: "",
    kind: "",
    acts: ["save_page", "write_page"],
  };
  const none = landing({ page_activity: { changes: [] }, viewer: unbound });
  const region = await screen.findByRole("region", { name: /Watching/ });
  await waitFor(() =>
    expect(within(region).getByText(/has none, so it follows no page/)).toBeTruthy(),
  );
  expect(none.some((a) => a.what === "pages" && "watcher" in a.params)).toBe(false);
});

// AN AGE IS NOT A DAY: a row under "Oct 09" said "1d ago", the same as the
// rows under Yesterday. A row says its time on the clock its day is cut on.
test("a row under a company day says its time of day on the company's clock", async () => {
  location.hash = "#/doclet";
  // 09:00 on the 25th in Tokyo.
  vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-09-25T00:00:00Z"));
  mount({ ...ran, hits: [], served_mode: "" }, [{ key: "ENG", name: "Engineering", pages: 3 }], {
    page_activity: {
      changes: [
        {
          id: "c-1",
          page_id: "p-1",
          title: "Provisioner runbook",
          container: "ENG",
          kind: "saved",
          actor: "swe",
          // 08:00 on the 25th in Tokyo: today.
          at: "2026-09-24T23:00:00Z",
        },
        {
          id: "c-2",
          page_id: "p-2",
          title: "Deploy notes",
          container: "ENG",
          kind: "comment",
          actor: "swe",
          // 22:00 on the 24th in Tokyo: yesterday.
          at: "2026-09-24T13:00:00Z",
        },
        {
          id: "c-3",
          page_id: "p-3",
          title: "Onboarding",
          container: "ENG",
          kind: "created",
          actor: "swe",
          // 10:00 on the 23rd in Tokyo, 47 hours ago: "1d ago", as Yesterday's.
          at: "2026-09-23T01:00:00Z",
        },
      ],
    },
  });
  const written = await screen.findByRole("region", { name: /Recently written/ });
  await waitFor(() => expect(within(written).getByText(/Onboarding/)).toBeTruthy());
  const days = within(written)
    .getAllByRole("heading", { level: 3 })
    .map((h) => h.textContent);
  expect(days).toContain("Sep 23");
  const timeOf = (title: string) =>
    within(written)
      .getAllByRole("listitem")
      .find((row) => row.textContent?.includes(title))
      ?.querySelector("time");
  expect(timeOf("Deploy notes")?.textContent).toBe("22:00");
  expect(timeOf("Onboarding")?.textContent).toBe("10:00");
  // THE AGE IS STILL THERE, in the title beside the exact instant.
  expect(timeOf("Onboarding")?.getAttribute("title")).toMatch(/^1d ago, /);
});

// A NEW PAGE'S EXCERPT IS ITS FIRST LINE, which is usually `# {title}`: the
// row named the page and then named it again as its note.
test("a new page's row never repeats its title as its note", async () => {
  location.hash = "#/doclet";
  vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-09-25T00:00:00Z"));
  mount({ ...ran, hits: [], served_mode: "" }, [{ key: "ENG", name: "Engineering", pages: 3 }], {
    page_activity: {
      changes: [
        {
          id: "c-1",
          page_id: "p-1",
          title: "Provisioner runbook",
          container: "ENG",
          kind: "saved",
          actor: "swe",
          excerpt: "Adds the drain step",
          at: "2026-09-24T23:30:00Z",
        },
        {
          id: "c-2",
          page_id: "p-2",
          title: "Deploy notes",
          container: "ENG",
          kind: "created",
          actor: "swe",
          excerpt: "Deploy notes",
          at: "2026-09-24T23:00:00Z",
        },
        {
          id: "c-3",
          page_id: "p-3",
          title: "Onboarding",
          container: "ENG",
          kind: "created",
          actor: "swe",
          excerpt: "Welcome to the team.",
          at: "2026-09-24T22:00:00Z",
        },
      ],
    },
  });
  const written = await screen.findByRole("region", { name: /Recently written/ });
  // A FIRST LINE THAT IS NOT THE TITLE IS THE NOTE, as it always was.
  await waitFor(() => expect(within(written).getByText("Welcome to the team.")).toBeTruthy());
  const deploy = within(written)
    .getAllByRole("listitem")
    .find((row) => row.textContent?.includes("Deploy notes"));
  expect(deploy?.textContent?.match(/Deploy notes/g)).toHaveLength(1);
});

test("pinned lists the pages starred from a page's More menu, and only pages", async () => {
  location.hash = "#/doclet";
  // STARS ARE THE READER'S OWN (`lib/starred.ts`): kept under a key naming them.
  noteReader("p-1");
  localStorage.setItem(
    starsKey("p-1"),
    JSON.stringify([
      {
        path: ["doclet", "pages", "0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a"],
        label: "Provisioner runbook",
        workspace: "doclet",
        at: 1,
      },
      { path: ["worklet", "ENG-4"], label: "ENG-4", workspace: "worklet", at: 2 },
    ]),
  );
  resetStars();
  mount({ ...ran, hits: [], served_mode: "" }, [{ key: "ENG", name: "Engineering", pages: 2 }]);
  const pinned = await screen.findByRole("region", { name: /Pinned/ });
  await waitFor(() => expect(within(pinned).getByText("Provisioner runbook")).toBeTruthy());
  expect(within(pinned).queryByText("ENG-4")).toBeNull();
});

// ---------------------------------------------------------------------------
// A space's peek
// ---------------------------------------------------------------------------

function pageRow(n: number) {
  return {
    id: `p-${n}`,
    title: `Page ${String(n).padStart(3, "0")}`,
    container: "ENG",
    status: "published",
    version: 1,
    author: n % 2 ? "swe" : "cto",
    updated_at: new Date(Date.UTC(2026, 8, 1, 0, n)).toISOString(),
    revision: 1,
  };
}

/** A signed-in reader holding Admin, which who files here is read under. */
const READER = { ...JANE, grants: ["admin"] };

/**
 * Render the space's peek; `config` undefined leaves that read in flight,
 * `refused` refuses it, and `viewer` is [READER] unless told.
 */
function mountPeek(options: {
  rows: number;
  total: number;
  after?: string;
  config?: unknown;
  /** The company document's read is refused. */
  refused?: boolean;
  viewer?: unknown;
}) {
  return mount(
    ran,
    [{ key: "ENG", name: "Engineering", pages: options.total }],
    {
      pages: {
        pages: Array.from({ length: options.rows }, (_, i) => pageRow(i + 1)),
        limit: 500,
        total: options.total,
        after: options.after,
      },
      config: () =>
        options.refused
          ? Promise.reject(new Error("unauthorized"))
          : options.config === undefined
            ? new Promise(() => {})
            : options.config,
      viewer: options.viewer ?? READER,
    },
    <ContainerPeek id="ENG" />,
  );
}

test("a space's peek counts its pages off the total, never off the rows read", async () => {
  const asked = mountPeek({ rows: 50, total: 400, after: "cursor-1" });
  await waitFor(() => expect(screen.getByText(/more pages in this space/)).toBeTruthy());
  // THE REST IS THE TOTAL LESS WHAT IS DRAWN, not the window less what is drawn.
  expect(screen.getByText("392 more pages in this space")).toBeTruthy();
  expect(asked.find((a) => a.what === "pages")?.params).toMatchObject({
    container: "ENG",
    limit: 500,
  });
  // A WINDOW SAYS IT IS ONE, on both panels derived from it.
  expect(screen.getByText("newest of the first 50 pages by title")).toBeTruthy();
  expect(screen.getByText("authors of the first 50 by title")).toBeTruthy();
});

test("a space read whole says nothing about a window", async () => {
  mountPeek({ rows: 12, total: 12 });
  await waitFor(() => expect(screen.getByText("4 more pages in this space")).toBeTruthy());
  expect(screen.queryByText(/by title/)).toBeNull();
  expect(screen.getByText("authors, not editors")).toBeTruthy();
});

test("who files here: a reader without the grant is told so, and one whose read is in flight is not", async () => {
  const asked = mountPeek({ rows: 1, total: 1, viewer: { ...READER, grants: [] } });
  await waitFor(() =>
    expect(screen.getByText("Who files here needs Admin access to read")).toBeTruthy(),
  );
  // NOTHING IS ASKED that the engine has already said this reader may not read.
  expect(asked.some((a) => a.what === "config")).toBe(false);

  cleanup();
  mountPeek({ rows: 1, total: 1 });
  await waitFor(() => expect(screen.getByText("Who files here is still being read")).toBeTruthy());
  expect(screen.queryByText(/needs Admin/)).toBeNull();

  // REFUSED: the read answered with an error, which is neither of the above.
  cleanup();
  mountPeek({ rows: 1, total: 1, refused: true });
  await waitFor(() => expect(screen.getByText("Owner unknown")).toBeTruthy());

  cleanup();
  mountPeek({
    rows: 1,
    total: 1,
    config: { units: [{ name: "Engineering", space: "eng", project: "ENG" }] },
  });
  // THE TITLE AND THE FACT: the unit that files here, named.
  await waitFor(() => expect(screen.getAllByText("Engineering").length).toBe(2));
  expect(screen.queryByText(/Who files here/)).toBeNull();
  expect(screen.queryByText("Owner unknown")).toBeNull();
});
