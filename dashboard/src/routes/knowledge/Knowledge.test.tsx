/**
 * Knowledge Home: the landing, the ranked answer, and a space's peek.
 *
 * What these protect, each one a fix or a rule of the approved design:
 *
 *  - A SNIPPET IS PROSE, drawn as what its markdown renders to and never with
 *    its `#` and `**` in it, and the words a search matched are marked in it.
 *  - THE MODE ON THE WIRE IS THE ENGINE'S WORD: "Meaning" is read, `semantic`
 *    is sent.
 *  - A PARTIAL SEARCH SAYS SO, naming the node that did not answer.
 *  - THE COPY IS TRUE OF THE BACKEND: a native search reads this node's own
 *    copy of the pages, and says so.
 *  - AN ADDRESS NAMING NO MODE RUNS IN ONE THE ENGINE SERVES, waiting for the
 *    probe rather than running Hybrid and reporting its degradation.
 *  - A PHRASE PAST THE SEARCH BOUND IS NOT ASKED, and the screen says why.
 *  - THE RESULTS ARE GROUPED BY SPACE IN THE ENGINE'S ORDER, and j and k move
 *    the choice the preview reads.
 *  - THE LANDING DRAWS THE PERSON BEHIND A TOKEN, and its days are the
 *    company's, cut at the company's midnight.
 *  - A SPACE'S PEEK COUNTS OFF THE TOTAL, never off the rows one read
 *    returned, and WHO FILES THERE HAS FOUR STATES.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Knowledge } from "./Knowledge.tsx";
import { ContainerPeek } from "./peeks.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { resetForTest as resetStars } from "~/lib/starred.ts";
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
  location.hash = "#/knowledge?q=runbook";
});

afterEach(() => {
  cleanup();
  try {
    localStorage.clear();
  } catch {
    // no storage in this environment is fine
  }
  resetStars();
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

/** The person this browser's token is bound to. */
const JANE = {
  operator_id: "U0FOUNDER",
  operator: true,
  handle: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["save_page", "comment_on_page", "write_page"],
};

/** What the frame itself reads, answered for every screen. */
const FRAME: Record<string, unknown> = {
  viewer: JANE,
  work_inbox: { handle: "jane", notices: [], primary_reasons: [], unread: 0, primary: 0 },
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
  view = <Knowledge />,
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
  location.hash = "#/knowledge?q=runbook&mode=semantic";
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
  location.hash = `#/knowledge?q=${"p".repeat(SEARCH_QUERY_MAX + 1)}&mode=keyword`;
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

test("a search that did not run says why, with the remedy its reason names", async () => {
  mount({
    ...ran,
    hits: [],
    served_mode: "",
    available: false,
    reason: "no_scope",
    note: "the confluence backend is configured but knowledge.scope lists no container",
  });
  await waitFor(() => expect(screen.getByText(/Search cannot run here/)).toBeTruthy());
  expect(screen.getByText(/The backend itself is fine/)).toBeTruthy();
});

test("the copy says a native search reads this node's own copy, and never that there is none", async () => {
  mount(ran, [{ key: "ENG", name: "Engineering", pages: 3 }]);
  await waitFor(() => expect(document.querySelector(".knowledge-hit")).not.toBeNull());
  expect(document.body.textContent).not.toMatch(/no local copy|no staleness window/i);
  expect(screen.getByText(/this node's own copy of the pages/)).toBeTruthy();
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
          author: "founder",
          updated_at: "2026-09-24T22:00:00Z",
          revision: 9,
          body: "The runbook body.",
        },
        revision: 9,
        skill: false,
        onboarding: false,
        author_seat: "jane",
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
  // THE PERSON BEHIND THE TOKEN, never the token: the author is Jane.
  expect(within(preview).getByText("Jane Founder")).toBeTruthy();
  fireEvent.keyDown(screen.getByRole("region", { name: "Results" }), { key: "j" });
  await waitFor(() =>
    expect(within(preview).getByRole("heading", { name: "Deploy runbook" })).toBeTruthy(),
  );
  expect(asked.filter((a) => a.what === "page").map((a) => a.params.id)).toContain("p-2");
});

test("an address naming no mode runs in a mode the engine serves, never a degraded default", async () => {
  location.hash = "#/knowledge?q=runbook";
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
              <Knowledge />
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
  location.hash = "#/knowledge?q=runbook&mode=hybrid";
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

test("the spaces row links every space, with its key, its count and its purpose", async () => {
  location.hash = "#/knowledge";
  mount({ ...ran, hits: [], served_mode: "" }, [
    { key: "ENG", name: "Engineering", pages: 412, purpose: "How the platform runs" },
    { key: "PROD", name: "Product", pages: 0 },
  ]);
  const row = await screen.findByRole("navigation", { name: "Spaces" });
  const eng = within(row).getByRole("link", { name: /Engineering/ });
  expect(eng.getAttribute("href")).toBe("#/knowledge/ENG");
  expect(eng.getAttribute("title")).toBe("Engineering: 412 pages. How the platform runs");
  expect(
    within(row)
      .getByRole("link", { name: /Product/ })
      .getAttribute("title"),
  ).toBe("Product: 0 pages");
});

test("recently written draws the person behind a token, grouped by the company's day", async () => {
  location.hash = "#/knowledge";
  // 09:00 on the 25th in Tokyo, which is still the 24th in UTC.
  vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-09-25T00:00:00Z"));
  mount({ ...ran, hits: [], served_mode: "" }, [{ key: "ENG", name: "Engineering", pages: 2 }], {
    page_activity: {
      changes: [
        {
          id: "c-1",
          page_id: "p-1",
          title: "Provisioner runbook",
          container: "ENG",
          kind: "saved",
          actor: "founder",
          actor_seat: "jane",
          // 08:00 on the 25th in Tokyo: today, though it is the 24th in UTC.
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
      ],
    },
  });
  const written = await screen.findByRole("region", { name: /Recently written/ });
  await waitFor(() => expect(within(written).getByText("Jane Founder")).toBeTruthy());
  expect(within(written).queryByText("founder")).toBeNull();
  const days = within(written)
    .getAllByRole("heading", { level: 3 })
    .map((h) => h.textContent);
  expect(days).toEqual(["Today", "Yesterday"]);
  const rows = within(written).getAllByRole("listitem");
  expect(rows[0]?.textContent).toMatch(/Jane Founder saved Provisioner runbook/);
  expect(rows[1]?.textContent).toMatch(/Crewlet-SWE commented on Deploy notes/);
});

// A NEW PAGE'S EXCERPT IS ITS FIRST LINE, which is usually `# {title}`: the
// row named the page and then named it again as its note.
test("a new page's row never repeats its title as its note", async () => {
  location.hash = "#/knowledge";
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
  location.hash = "#/knowledge";
  localStorage.setItem(
    "crewlet_starred",
    JSON.stringify([
      {
        path: ["knowledge", "pages", "0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a"],
        label: "Provisioner runbook",
        workspace: "knowledge",
        at: 1,
      },
      { path: ["work", "ENG-4"], label: "ENG-4", workspace: "work", at: 2 },
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

/** Render the space's peek; `config` undefined leaves that read in flight. */
function mountPeek(options: { rows: number; total: number; after?: string; config?: unknown }) {
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
      config: () => (options.config === undefined ? new Promise(() => {}) : options.config),
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

test("who files here: no token is told so, and a token whose read is in flight is not", async () => {
  mountPeek({ rows: 1, total: 1 });
  await waitFor(() =>
    expect(screen.getByText("Who files here needs an operator token to read")).toBeTruthy(),
  );

  cleanup();
  localStorage.setItem("crewlet_api_token", "tok");
  mountPeek({ rows: 1, total: 1 });
  await waitFor(() => expect(screen.getByText("Who files here is still being read")).toBeTruthy());
  expect(screen.queryByText(/needs an operator token/)).toBeNull();

  cleanup();
  mountPeek({
    rows: 1,
    total: 1,
    config: { units: [{ name: "Engineering", space: "eng", project: "ENG" }] },
  });
  // THE TITLE AND THE FACT: the unit that files here, named.
  await waitFor(() => expect(screen.getAllByText("Engineering").length).toBe(2));
  expect(screen.queryByText(/Who files here/)).toBeNull();
});
