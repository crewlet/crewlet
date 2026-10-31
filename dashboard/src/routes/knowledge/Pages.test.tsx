/**
 * A space: its head, and its pages as an outline read one level at a time,
 * with Create at the end of every open level.
 *
 * What these protect:
 *  - THE OUTLINE ASKS FOR ONE LEVEL, and a folder's pages only once its row
 *    opens: a space of four hundred pages costs one window until somebody
 *    opens a folder.
 *  - A ROW SAYS WHO LAST SAVED WHAT THE PAGE SAYS, drawn as whoever the record
 *    names: the seat a person writes as.
 *  - CREATE FILES WHERE IT IS: a Document goes to the composer filed there, and
 *    a Folder named in place is a page with an empty body, written at once.
 *  - CREATE IS DRAWN FOR EVERY READER, and held with the reason for one who
 *    cannot write.
 *  - "LAST WRITTEN" IS THE SPACE'S OWN CHANGE LOG, never the newest row of a
 *    listing ordered by title.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Pages } from "./Pages.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { afterMicrotasks } from "~/test/settle.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const NOW = Date.parse("2026-09-25T00:00:00Z");
const FOLDER = "11111111-1111-4111-8111-111111111111";
const RUNBOOK = "22222222-2222-4222-8222-222222222222";
const CHILD = "33333333-3333-4333-8333-333333333333";

const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "tracker", "knowledge"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["save_page", "comment_on_page", "write_page"],
};

const ORG = {
  name: "Acme",
  timezone: "Asia/Tokyo",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Crewlet-SWE", handle: "swe", kind: "agent" },
  ],
  units: [],
};

const summary = (over: Record<string, unknown>) => ({
  container: "ENG",
  status: "published",
  version: 1,
  revision: 1,
  updated_at: "2026-09-24T22:00:00Z",
  ...over,
});

const TOP = [
  summary({ id: FOLDER, title: "Designs", folder: true, children: 1 }),
  summary({
    id: RUNBOOK,
    title: "Runbook",
    status: "draft",
    saved_by: "jane",
    saved_at: "2026-09-24T22:00:00Z",
  }),
];

let posted: { tool: string; args: Record<string, unknown> }[];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.spyOn(Date, "now").mockReturnValue(NOW);
  location.hash = "#/knowledge/ENG";
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
          position: "CREWLET_PAGES_LOG@1:99",
          receipt: { id: "44444444-4444-4444-8444-444444444444" },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  reloadForTest();
  location.hash = "#/";
});

function mount(viewer: unknown = JANE) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(withDerived(ORG) as never);
  const socket = new LiveSocket(store);
  const asked: { kind: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    const p = params ?? {};
    asked.push({ kind: what, params: p });
    switch (what) {
      case "viewer":
        return Promise.resolve(viewer);
      case "inbox":
        return Promise.resolve({
          handle: "jane",
          notices: [],
          primary_reasons: [],
          unread: 0,
          primary: 0,
        });
      case "decisions":
        return Promise.resolve({ handle: "jane", items: [], total: 0, capped: false });
      case "containers":
        return Promise.resolve({
          containers: [
            {
              key: "ENG",
              name: "Engineering",
              pages: 3,
              purpose: "How the platform runs",
              created_at: "2026-09-01T00:00:00Z",
            },
          ],
        });
      case "page_activity":
        return Promise.resolve({
          changes: [{ id: "c-1", page_id: RUNBOOK, kind: "saved", at: "2026-09-24T22:00:00Z" }],
        });
      case "pages":
        if (p.parent === FOLDER) {
          return Promise.resolve({
            pages: [summary({ id: CHILD, title: "FAIR-2 Money", parent_id: FOLDER })],
            limit: 500,
            total: 1,
          });
        }
        return Promise.resolve({ pages: TOP, limit: 500, total: 2 });
      default:
        return Promise.resolve({});
    }
  }) as typeof socket.query;
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>
              <Pages container="ENG" />
            </Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { asked };
}

async function settle() {
  await afterMicrotasks();
}

const outline = () => screen.getByRole("treegrid", { name: "Pages in Engineering" });
/** The Create control of the level an add row closes, found by the row's own name. */
const createIn = (where: string) =>
  within(within(outline()).getByRole("row", { name: `Create in ${where}` })).getByRole("button", {
    name: `Create in ${where}`,
  });
const rowNamed = (name: string) =>
  within(outline())
    .getAllByRole("row")
    .find((row) => row.textContent?.includes(name));

test("the head names the space, what it is for, and when somebody last wrote in it", async () => {
  const { asked } = mount();
  await settle();
  expect(screen.getByRole("heading", { name: "Engineering" })).toBeTruthy();
  expect(screen.getByText("How the platform runs")).toBeTruthy();
  const facts = screen.getByRole("list", { name: "About Engineering" });
  expect(facts.textContent).toContain("3 pages");
  expect(facts.textContent).toContain("Last written 2h ago");
  // FROM THE SPACE'S OWN CHANGE LOG, asked for the newest change to what a
  // page says.
  expect(asked.find((a) => a.kind === "page_activity")?.params).toEqual({
    container: "ENG",
    kinds: "created,saved,renamed",
    limit: 1,
  });
});

test("the outline reads the top level, and a folder's pages only once it opens", async () => {
  const { asked } = mount();
  await settle();
  const levels = () => asked.filter((a) => a.kind === "pages").map((a) => a.params);
  expect(levels()).toEqual([
    { status: "published,draft", skills: false, container: "ENG", roots: true, limit: 500 },
  ]);
  expect(rowNamed("Designs")?.getAttribute("aria-expanded")).toBe("false");
  expect(rowNamed("FAIR-2 Money")).toBeUndefined();

  fireEvent.click(screen.getByRole("button", { name: "Expand Designs", hidden: true }));
  await settle();
  expect(levels()).toContainEqual({
    status: "published,draft",
    skills: false,
    parent: FOLDER,
    limit: 500,
  });
  expect(rowNamed("FAIR-2 Money")).toBeDefined();
});

test("a row links its page, marks a draft, and says who last saved what it says", async () => {
  mount();
  await settle();
  const row = rowNamed("Runbook")!;
  expect(
    within(row)
      .getByRole("link", { name: /Runbook/ })
      .getAttribute("href"),
  ).toBe(`#/knowledge/pages/${RUNBOOK}`);
  expect(within(row).getByText("Draft")).toBeTruthy();
  // THE SEAT A PERSON WRITES AS, drawn by the chart's name for it.
  expect(within(row).getByText("Last saved by Jane Founder")).toBeTruthy();
});

test("Create's Document goes to the composer filed where the row is", async () => {
  mount();
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Expand Designs", hidden: true }));
  await settle();
  fireEvent.click(createIn("Designs"));
  fireEvent.click(await screen.findByRole("menuitem", { name: /Document/ }));
  await settle();
  expect(location.hash).toBe(`#/knowledge/new?space=ENG&parent=${FOLDER}`);
});

test("a folder named in place is a page with an empty body, written at once", async () => {
  mount();
  await settle();
  fireEvent.click(createIn("Engineering"));
  fireEvent.click(await screen.findByRole("menuitem", { name: /Folder/ }));
  await settle();
  const field = screen.getByRole("textbox", { name: "New folder in Engineering" });
  expect(document.activeElement).toBe(field);
  // AN EMPTY NAME IS HELD, and said, rather than sent.
  fireEvent.keyDown(field, { key: "Enter" });
  await settle();
  expect(posted).toEqual([]);
  fireEvent.change(field, { target: { value: "Postmortems" } });
  fireEvent.keyDown(field, { key: "Enter" });
  await settle();
  expect(posted).toEqual([
    { tool: "write_page", args: { title: "Postmortems", body: "", container: "ENG" } },
  ]);
});

test("Escape lets a folder's name go, and nothing is written", async () => {
  mount();
  await settle();
  fireEvent.click(createIn("Engineering"));
  fireEvent.click(await screen.findByRole("menuitem", { name: /Folder/ }));
  await settle();
  const field = screen.getByRole("textbox", { name: "New folder in Engineering" });
  fireEvent.change(field, { target: { value: "Postmortems" } });
  fireEvent.keyDown(field, { key: "Escape" });
  await settle();
  expect(screen.queryByRole("textbox", { name: "New folder in Engineering" })).toBeNull();
  expect(posted).toEqual([]);
});

test.each([
  [
    "an anonymous reader",
    { login: "", grants: [], handle: "", owner: "", name: "", acts: [], anonymous: true },
    WRITE_REASONS.anonymous,
  ],
  ["a person the engine does not serve", { ...JANE, acts: [] }, WRITE_REASONS.not_served],
])("Create is drawn for %s, held with the reason", async (_who, viewer, reason) => {
  mount(viewer);
  await settle();
  const create = within(outline()).getByRole("button", { name: "Create" });
  expect(create.getAttribute("aria-disabled")).toBe("true");
  expect(create.getAttribute("title")).toBe(reason);
  expect(posted).toEqual([]);
});

// AN UNBOUND PRINCIPAL IS NOT A BLOCK (ADR-0024): a credential the directory
// binds to no seat writes under its own login, so Create is held only where the
// engine says it would not make the change.
test("Create is offered to a credential bound to no seat that the engine serves", async () => {
  mount({
    login: "svc:docs",
    grants: ["knowledge"],
    handle: "",
    owner: "svc:docs",
    name: "",
    kind: "",
    acts: ["write_page"],
  });
  await settle();
  const create = within(outline()).getByRole("button", { name: /^Create in / });
  expect(create.getAttribute("aria-disabled")).not.toBe("true");
});
