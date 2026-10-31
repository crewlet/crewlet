/**
 * New dashboard over a fake engine that answers the catalogue, each source's
 * preview as the preview route would, and the create's upload and write.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import {
  LiveSocket,
  Store,
  type DashboardEngineSource,
  type DashboardSpec,
  type WorkProjectRow,
} from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import catalogue from "./dashboard/testdata/catalogue.json";
import { DashboardCreate } from "./DashboardCreate.tsx";
import { ROWS_UNREAD } from "~/lib/artifacts/dashboard.ts";

const MADE = "0192f2c4-3333-7000-8000-000000000003";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "tracker", "artifacts"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["create_work_item"],
  project: "OPS",
};

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Agent Analyst", handle: "analyst", kind: "agent", goal: "Metrics and dashboards" },
    { name: "Agent Designer", handle: "designer", kind: "agent", goal: "Brand and UX design" },
  ],
  units: [],
};

const project = (key: string): WorkProjectRow =>
  ({ key, name: key, unit: { resolved: true }, lead: { handle: "jane" } }) as WorkProjectRow;

/** What the fake engine was sent, in order. */
let sent: { method: string; path: string; body: unknown }[] = [];
/** What the create's write answers: the artifact, or a refusal. */
let createAnswer: { status: number; body: unknown } = { status: 201, body: { artifact: MADE } };
/** Whether the catalogue read fails, as an engine that cannot answer it does. */
let catalogueFails = false;

function rows(name: string): Record<string, unknown>[] {
  switch (name) {
    case "tracker.items":
      return [
        {
          key: "ENG-1",
          title: "Ship it",
          status: "in_progress",
          status_group: "active",
          assignee: "ana",
        },
        {
          key: "ENG-2",
          title: "Plan it",
          status: "todo",
          status_group: "not_started",
          assignee: "bo",
        },
      ];
    case "tracker.workload":
      return [{ handle: "ana", open: 4, points: 9, blocked: 1, overdue: 2, unscheduled: 0 }];
    default:
      return [];
  }
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  sent = [];
  createAnswer = { status: 201, body: { artifact: MADE } };
  catalogueFails = false;
  location.hash = "#/artifacts/new/dashboard";
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      const method = init?.method ?? "GET";
      const raw = typeof init?.body === "string" ? init.body : "";
      let body: unknown = raw;
      try {
        body = raw ? JSON.parse(raw) : undefined;
      } catch {
        // A transfer's body is the file's own text.
      }
      sent.push({ method, path: url.pathname, body });
      if (url.pathname === "/artifacts/sources")
        return catalogueFails
          ? json(500, { error: "internal", message: "The catalogue could not be read." })
          : json(200, { sources: catalogue });
      const preview = /^\/artifacts\/sources\/preview\/(.+)$/.exec(url.pathname);
      if (preview) {
        const name = decodeURIComponent(preview[1]!);
        if (name === "spend.seats") {
          return json(200, {
            source: name,
            status: "refused",
            refusal: { dataset: name, reason: "no_grant" },
            detail: "You cannot read spend: everybody the company admits.",
          });
        }
        const src = (catalogue as DashboardEngineSource[]).find((s) => s.name === name)!;
        return json(200, {
          source: name,
          status: "ready",
          as_of: new Date().toISOString(),
          table: { columns: src.columns, key: src.key, rows: rows(name) },
        });
      }
      if (url.pathname === "/artifacts/uploads" && method === "POST") {
        return json(201, {
          upload: "up-1",
          transfers: [{ path: "dashboard.json", method: "PUT", url: "/ignored", direct: false }],
          op_id: "op-1",
        });
      }
      if (url.pathname.startsWith("/artifacts/uploads/up-1/files/"))
        return json(200, { stored: true });
      if (url.pathname === "/artifacts" && method === "POST") {
        return json(createAnswer.status, createAnswer.body);
      }
      if (url.pathname === "/operator/act/create_work_item") {
        return json(200, {
          tool: "create_work_item",
          outcome: "applied",
          position: "CREWLET_TRACKER_LOG@1:9",
          receipt: { id: "0f0f", item: "OPS-4", key: "OPS-4" },
        });
      }
      if (url.pathname === `/artifacts/${MADE}/work-items`) return json(200, { linked: true });
      return json(404, { error: "not_found" });
    }),
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  localStorage.clear();
  reloadForTest();
});

function mount() {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(withDerived(ORG) as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string) => {
    const answers: Record<string, unknown> = {
      viewer: JANE,
      work_projects: { projects: [project("DES"), project("OPS")] },
      inbox: { handle: "jane", notices: [], unread: 0, primary: 0 },
    };
    return Promise.resolve(answers[what] ?? {});
  }) as typeof socket.query;
  return render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>
              <DashboardCreate />
            </Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
}

/** The task the page filed, as the act route was sent it. */
const filed = () =>
  sent.find((r) => r.path === "/operator/act/create_work_item")?.body as
    { args: { title: string; assignee: string; body: string; project: string } } | undefined;

const data = () => screen.getByRole("complementary", { name: "Data" });

test("the Data panel lists the engine's sources with the rows each reader may read", async () => {
  mount();
  const panel = data();
  await waitFor(() => expect(within(panel).getByText("Work items")).toBeTruthy());
  await waitFor(() => expect(within(panel).getAllByText(/2 rows/).length).toBeGreaterThan(0));
  // EVERY PREVIEW ASKS THE PREVIEW ROUTE, never a board's dataset route.
  expect(sent.some((r) => r.path === "/artifacts/sources/preview/tracker.items")).toBe(true);
  expect(sent.some((r) => r.path.includes("/versions/"))).toBe(false);
});

// GROUPED BY AREA, one list per area named by it: a tag on every row naming
// its area was a column a reader learned to skip.
test("the Data panel groups its sources by area, with no tag per row", async () => {
  mount();
  const panel = data();
  await waitFor(() => expect(within(panel).getByText("Work items")).toBeTruthy());
  const lists = within(panel).getAllByRole("list");
  expect(lists.length).toBeGreaterThan(0);
  for (const list of lists) {
    const name = list.getAttribute("aria-label") ?? "";
    expect(within(panel).getByRole("heading", { level: 3, name })).toBeTruthy();
  }
  expect(panel.querySelector(".crewlet-dataset-source__kind")).toBeNull();
});

test("a source the reader may not read says why and offers no figure", async () => {
  mount();
  const panel = data();
  const entry = await within(panel).findByRole("button", { name: /Spend by seat/ });
  fireEvent.click(entry);
  await waitFor(() =>
    expect(
      within(panel).getByText("You cannot read spend: everybody the company admits."),
    ).toBeTruthy(),
  );
  expect(within(panel).queryByRole("button", { name: /to the board/ })).toBeNull();
});

test("an open source's figure is added to the board, live", async () => {
  mount();
  const panel = data();
  fireEvent.click(await within(panel).findByRole("button", { name: /Workload/ }));
  const add = await within(panel).findByRole("button", { name: "Add “Open work” to the board" });
  fireEvent.click(add);
  const board = screen.getByRole("region", { name: "Board" });
  await waitFor(() => expect(within(board).getByText("Open work")).toBeTruthy());
  expect(within(panel).getByRole("button", { name: "Add “Open work” again" })).toBeTruthy();
});

test("Create is held for a title and a figure, then publishes the composed board and opens it", async () => {
  mount();
  const create = () => screen.getByRole("button", { name: /Create dashboard/ });
  await waitFor(() => expect(within(data()).getByText("Work items")).toBeTruthy());
  expect(create().getAttribute("aria-disabled") ?? create().hasAttribute("disabled")).toBeTruthy();

  fireEvent.click(screen.getByRole("button", { name: /Work overview/ }));
  fireEvent.change(screen.getByRole("textbox", { name: "Title" }), {
    target: { value: "Delivery pulse" },
  });
  fireEvent.click(create());

  await waitFor(() => expect(location.hash).toBe(`#/artifacts/${MADE}`));
  const put = sent.find((r) => r.method === "PUT")!;
  const spec = (typeof put.body === "string" ? JSON.parse(put.body) : put.body) as DashboardSpec;
  expect(spec.title).toBe("Delivery pulse");
  expect(spec.datasets.map((d) => d.engine?.name)).toEqual([
    "tracker.items",
    "tracker.items",
    "tracker.items",
  ]);
  const made = sent.find((r) => r.method === "POST" && r.path === "/artifacts")!;
  expect(made.body).toEqual({
    kind: "dashboard",
    title: "Delivery pulse",
    upload: "up-1",
    files: [{ path: "dashboard.json", upload: true }],
    entry: "dashboard.json",
  });
});

test("a board the engine refuses is said where the reader is looking, and the draft stays", async () => {
  createAnswer = {
    status: 422,
    body: { error: "invalid", detail: "dashboard.json: widgets[0].w: is 0" },
  };
  mount();
  await waitFor(() => expect(within(data()).getByText("Work items")).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name: /Team workload/ }));
  fireEvent.change(screen.getByRole("textbox", { name: "Title" }), { target: { value: "Load" } });
  fireEvent.click(screen.getByRole("button", { name: /Create dashboard/ }));
  expect(await screen.findByText("The dashboard was not created")).toBeTruthy();
  expect(location.hash).toBe("#/artifacts/new/dashboard");
  expect(within(screen.getByRole("region", { name: "Board" })).getByText("Open work")).toBeTruthy();
});

test("an agent asked with nothing on the board gets a task with the brief, and the reader lands on it", async () => {
  mount();
  await waitFor(() => expect(within(data()).getByText("Work items")).toBeTruthy());
  fireEvent.click(screen.getAllByRole("button", { name: "Ask an agent" })[0]!);
  fireEvent.change(screen.getByRole("textbox", { name: "Title" }), {
    target: { value: "Team load" },
  });
  fireEvent.change(screen.getByRole("textbox", { name: "What should the board answer?" }), {
    target: { value: "Who is carrying the most work." },
  });
  // THE AGENT WHOSE WORK IS DASHBOARDS is suggested first, and with nothing
  // on the board the brief goes alone.
  const panel = screen.getByRole("complementary", { name: "Ask an agent" });
  expect(within(panel).queryByText(/^The board, with/)).toBeNull();
  const ask = await within(panel).findByRole("button", { name: "Ask Agent Analyst" });
  fireEvent.click(ask);
  await waitFor(() => expect(location.hash).toBe("#/work/OPS-4"));
  expect(filed()?.args).toMatchObject({ title: "Team load", assignee: "analyst" });
  expect(filed()?.args.body).toContain("Who is carrying the most work.");
  expect(sent.some((r) => r.method === "POST" && r.path === "/artifacts")).toBe(false);
});

test("an agent asked with figures on the board refines it: the board is made, then linked to its task", async () => {
  mount();
  await waitFor(() => expect(within(data()).getByText("Work items")).toBeTruthy());
  // A BOARD MADE BEFORE ASKING goes with the brief: the page is the same page.
  fireEvent.click(screen.getByRole("button", { name: /Team workload/ }));
  fireEvent.click(screen.getAllByRole("button", { name: "Ask an agent" })[0]!);
  fireEvent.change(screen.getByRole("textbox", { name: "Title" }), {
    target: { value: "Team load" },
  });
  fireEvent.change(screen.getByRole("textbox", { name: "What should the board answer?" }), {
    target: { value: "Add a trend per team." },
  });
  const panel = screen.getByRole("complementary", { name: "Ask an agent" });
  expect(within(panel).getByText(/^The board, with \d+ figures?$/)).toBeTruthy();
  fireEvent.click(
    await within(panel).findByRole("button", { name: "Create and ask Agent Analyst" }),
  );
  await waitFor(() => expect(location.hash).toBe(`#/artifacts/${MADE}`));
  const order = sent.map((r) => `${r.method} ${r.path}`);
  const made = order.indexOf("POST /artifacts");
  const task = order.indexOf("POST /operator/act/create_work_item");
  const link = order.indexOf(`POST /artifacts/${MADE}/work-items`);
  expect(made).toBeGreaterThan(-1);
  expect(task).toBeGreaterThan(made);
  expect(link).toBeGreaterThan(task);
  expect(filed()?.args.body).toContain(`artifact ${MADE}`);
});

test("a preview that cannot be read says so in a reader's words, never the route's", async () => {
  const real = globalThis.fetch;
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL, init?: RequestInit) =>
      String(input).includes("/sources/preview/")
        ? json(404, { error: "not_found", detail: "this node serves nothing at GET /artifacts/x" })
        : real(input, init),
    ),
  });
  mount();
  const panel = data();
  await waitFor(() => expect(within(panel).getAllByText(ROWS_UNREAD).length).toBeGreaterThan(0));
  expect(within(panel).queryByText(/serves nothing/)).toBeNull();
});

// A CATALOGUE THAT CANNOT BE READ leaves no blank panel: the board says why,
// and the Data panel says where the sources will be.
test("a catalogue that cannot be read is said on the board, and the Data panel is not left blank", async () => {
  catalogueFails = true;
  mount();
  const panel = await screen.findByRole("complementary", { name: "Data" });
  await within(panel).findByText("The sources appear here once they can be read.");
  expect(within(panel).queryByRole("list", { name: "Data you can use" })).toBeNull();
});
