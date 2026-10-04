/**
 * The knowledge search: its ranked hits and every sentence about them.
 *
 * Four invariants, each one a fix:
 *
 *  - A SNIPPET IS PROSE. It is a cut of a markdown page, drawn as what that
 *    page renders to, never with its `#` and `**` in it.
 *  - THE MODE ON THE WIRE IS THE ENGINE'S WORD. "Meaning" is what a reader
 *    reads; `semantic` is what is sent, and the engine refuses the other.
 *  - A PARTIAL SEARCH SAYS SO, naming the node that did not answer — a short
 *    list is otherwise indistinguishable from a small corpus.
 *  - THE COPY IS TRUE OF THE BACKEND. The screen said "no local copy … no
 *    staleness window" of a native search that reads exactly that: this
 *    node's own copy of the pages.
 *  - A CONTAINER'S RAIL COUNTS OFF THE TOTAL, never off the rows one read
 *    returned: "42 more pages" under a container of four hundred was a window
 *    of fifty subtracted from itself.
 *  - AN ADDRESS NAMING NO MODE RUNS IN ONE THE ENGINE SERVES. With no
 *    embeddings provider that is Keyword, and the search waits for the probe
 *    to say so rather than running Hybrid and reporting its degradation.
 *  - WHO FILES HERE HAS FOUR STATES, and "needs config:read" is said
 *    only to a reader who does not hold it — never to one whose read of
 *    the company document is merely in flight.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ContainerPeek, Knowledge } from "./Knowledge.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
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
  location.hash = "#/knowledge?q=runbook";
});

afterEach(() => {
  cleanup();
  try {
    localStorage.clear();
  } catch {
    // no storage in this environment is fine
  }
  vi.unstubAllGlobals();
  location.hash = "#/";
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
    {
      id: "p-1",
      title: "Provisioner runbook",
      url: "",
      container: "ENG",
      snippet: "## Recovery **Drain** the node, then `crewlet run` again",
      updated_at: "",
    },
  ],
};

/** Render the screen against a socket answering `knowledge` with `answer`. */
function mount(answer: Record<string, unknown>, containers: unknown[] = []) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params: Record<string, unknown>) => {
    asked.push({ what, params });
    if (what === "knowledge") return Promise.resolve(answer);
    if (what === "containers") return Promise.resolve({ containers });
    return Promise.resolve({});
  }) as typeof socket.query;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Knowledge />
      </Router>
    </ClientContext.Provider>,
  );
  return asked;
}

test("a page's snippet is drawn as prose, without its markdown marks", async () => {
  mount(ran);
  await waitFor(() => expect(screen.getByText("Provisioner runbook")).toBeTruthy());
  const snippet = document.querySelector(".hit-snippet");
  expect(snippet?.textContent).toBe("Recovery Drain the node, then crewlet run again");
});

test("Meaning is sent as semantic, and the results say what ranked them", async () => {
  location.hash = "#/knowledge?q=runbook&mode=semantic";
  const asked = mount({ ...ran, mode: "semantic", served_mode: "semantic" });
  await waitFor(() => expect(screen.getByText("Provisioner runbook")).toBeTruthy());
  const search = asked.find((a) => a.what === "knowledge" && a.params.q === "runbook");
  expect(search?.params.mode).toBe("semantic");
  expect(screen.getByText("ranked by Meaning")).toBeTruthy();
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

test("a complete search draws no partial callout", async () => {
  mount(ran);
  await waitFor(() => expect(screen.getByText("Provisioner runbook")).toBeTruthy());
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

test("the copy never claims there is no local copy", async () => {
  mount(ran, [{ key: "ENG", name: "Engineering", pages: 3 }]);
  await waitFor(() => expect(screen.getByText("Provisioner runbook")).toBeTruthy());
  expect(document.body.textContent).not.toMatch(/no local copy|no staleness window/i);
  expect(screen.getByText(/this node’s own copy of the pages/)).toBeTruthy();

  cleanup();
  location.hash = "#/knowledge";
  mount({ ...ran, hits: [], served_mode: "" }, [{ key: "ENG", name: "Engineering", pages: 3 }]);
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(document.body.textContent).not.toMatch(/no local copy|no staleness window/i);
});

test("the home lists every space with its page count", async () => {
  location.hash = "#/knowledge";
  mount({ ...ran, hits: [], served_mode: "" }, [
    { key: "ENG", name: "Engineering", pages: 412, purpose: "How the platform runs" },
    { key: "PROD", name: "Product", pages: 0 },
  ]);
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(screen.getByText("412 pages")).toBeTruthy();
  expect(screen.getByText("0 pages")).toBeTruthy();
  expect(screen.getByText("How the platform runs")).toBeTruthy();
});

// ---------------------------------------------------------------------------
// The container rail
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

/** A signed-in reader holding `config:read`, which who files here is read under. */
const READER = { login: "ana", owner: "ana", grants: ["state:read", "config:read"] };

/** Render the container rail; `config` undefined leaves that read in flight. */
function mountPeek(options: {
  rows: number;
  total: number;
  after?: string;
  config?: unknown;
  viewer?: unknown;
}) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params: Record<string, unknown>) => {
    asked.push({ what, params });
    if (what === "containers")
      return Promise.resolve({
        containers: [{ key: "ENG", name: "Engineering", pages: options.total }],
      });
    if (what === "pages")
      return Promise.resolve({
        pages: Array.from({ length: options.rows }, (_, i) => pageRow(i + 1)),
        limit: 500,
        total: options.total,
        after: options.after,
      });
    if (what === "config")
      return options.config === undefined ? new Promise(() => {}) : Promise.resolve(options.config);
    if (what === "viewer") return Promise.resolve(options.viewer ?? READER);
    return Promise.resolve({});
  }) as typeof socket.query;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <ContainerPeek id="ENG" />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  return asked;
}

test("a container's rail counts its pages off the total, never off the rows read", async () => {
  const asked = mountPeek({ rows: 50, total: 400, after: "cursor-1" });
  await waitFor(() => expect(screen.getByText(/more pages in this container/)).toBeTruthy());
  // THE REST IS THE TOTAL LESS WHAT IS DRAWN — not the window less what is
  // drawn, which said 42.
  expect(screen.getByText("392 more pages in this container →")).toBeTruthy();
  // ONE FULL WINDOW, not the engine's default fifty.
  expect(asked.find((a) => a.what === "pages")?.params).toMatchObject({
    container: "ENG",
    limit: 500,
  });
  // A WINDOW SAYS IT IS ONE, on both panels derived from it.
  expect(screen.getByText("newest of the first 50 pages by title")).toBeTruthy();
  expect(screen.getByText("creators of the first 50 by title")).toBeTruthy();
});

test("a container read whole says nothing about a window", async () => {
  mountPeek({ rows: 12, total: 12 });
  await waitFor(() => expect(screen.getByText("4 more pages in this container →")).toBeTruthy());
  expect(screen.queryByText(/by title/)).toBeNull();
  expect(screen.getByText("creators, not editors")).toBeTruthy();
});

test("who files here: a reader without the grant is told so, and one whose read is in flight is not", async () => {
  mountPeek({ rows: 1, total: 1, viewer: { ...READER, grants: ["state:read"] } });
  await waitFor(() =>
    expect(screen.getByText("Who files here needs config:read to read")).toBeTruthy(),
  );

  cleanup();
  mountPeek({ rows: 1, total: 1 });
  await waitFor(() => expect(screen.getByText("Who files here is still being read")).toBeTruthy());
  expect(screen.queryByText(/needs config:read/)).toBeNull();

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

test("an address naming no mode runs in a mode the engine serves, never a degraded default", async () => {
  location.hash = "#/knowledge?q=runbook";
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: Record<string, unknown>[] = [];
  let answerProbe: (v: unknown) => void = () => {};
  socket.query = ((what: string, params: Record<string, unknown>) => {
    if (what !== "knowledge") return Promise.resolve({ containers: [] });
    asked.push(params);
    if (params.q === "") return new Promise<unknown>((resolve) => (answerProbe = resolve));
    return Promise.resolve({ ...ran, mode: "keyword", served_mode: "keyword", modes: ["keyword"] });
  }) as typeof socket.query;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Knowledge />
      </Router>
    </ClientContext.Provider>,
  );
  // NOTHING RUNS BEFORE THE PROBE SAYS WHICH MODES EXIST.
  await waitFor(() => expect(asked.some((p) => p.q === "")).toBe(true));
  expect(asked.some((p) => p.q === "runbook")).toBe(false);
  answerProbe({
    ...ran,
    hits: [],
    query: "",
    served_mode: "",
    modes: ["keyword"],
    degraded: "no_embeddings",
  });
  await waitFor(() => expect(screen.getByText("Provisioner runbook")).toBeTruthy());
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
