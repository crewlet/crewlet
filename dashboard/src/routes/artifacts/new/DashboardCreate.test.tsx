/**
 * New dashboard over a fake engine that answers the catalogue, each source's
 * preview as the preview route would, and the create's upload and write.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import type { DashboardEngineSource, DashboardSpec } from "~/protocol/index.ts";
import catalogue from "./dashboard/testdata/catalogue.json";
import { DashboardCreate } from "./DashboardCreate.tsx";

const MADE = "0192f2c4-3333-7000-8000-000000000003";

/** What the fake engine was sent, in order. */
let sent: { method: string; path: string; body: unknown }[] = [];
/** What the create's write answers: the artifact, or a refusal. */
let createAnswer: { status: number; body: unknown } = { status: 201, body: { artifact: MADE } };

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
  sent = [];
  createAnswer = { status: 201, body: { artifact: MADE } };
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
      if (url.pathname === "/artifacts/sources") return json(200, { sources: catalogue });
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
      return json(404, { error: "not_found" });
    }),
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const mount = () =>
  render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <DashboardCreate />
        </ToastProvider>
      </LayerHost>
    </Router>,
  );

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
