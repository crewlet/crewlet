/**
 * Switching Work's scope: the panel's every state as data (`panelOf`), the
 * figures All projects' lenses carry, and the panel in the frame, opened from
 * the trail and by G then P.
 */

import type { OrgServices } from "~/contract/services.ts";
import { withDerived } from "~/test/org.ts";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { Shell } from "../Shell.tsx";
import { Router } from "../router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { WorkProjectRow, WorkProjectsAnswer } from "~/protocol/index.ts";
import type { Recent } from "~/lib/recents.ts";
import type { ScopeAt } from "../scope.ts";
import { figuresOf, panelOf, type PanelActions } from "./ProjectSwitcher.tsx";
import { CONFIG_WRITE_REASONS, type ConfigWriteAccess } from "~/lib/useWriteAccess.ts";

// THE FRAME DIALS ITS SOCKET as soon as the viewer answers (`Shell.tsx`), and
// jsdom's own WebSocket really dials: refused, it schedules a reconnect that
// nothing here stops, which then fires after the environment is torn down and
// throws on a `location` that is gone. A socket that never opens and never
// closes is what every other suite mounting the frame stands in for it.
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
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

const project = (key: string, name: string, todo = 1, active = 0): WorkProjectRow => ({
  key,
  name,
  unit: { resolved: true },
  lead: {},
  task_counts: { todo, active, done: 0, closed: 0 },
  version: 1,
});

const listing = (
  projects: WorkProjectRow[],
  over: Partial<WorkProjectsAnswer> = {},
): WorkProjectsAnswer => ({
  projects,
  total: projects.length,
  census: { active: projects.length, archived: 0 },
  complete: true,
  ...over,
});

const FAIRSPLIT = [
  project("APP", "Mobile app", 3),
  project("FAIR", "Product and Engineering", 2),
  project("RCPT", "Receipt scanning", 1),
  project("SETL", "Settle up", 0),
  project("SPLIT", "Split engine", 4),
];

function actions(): PanelActions & { went: string[] } {
  const went: string[] = [];
  return {
    went,
    go: (to) => went.push(to.kind === "all" ? "all" : to.key),
    hrefOf: (to) => (to.kind === "all" ? "#/worklet" : `#/worklet/${to.key}`),
    browse: vi.fn(),
    searchTasks: vi.fn(),
    newProject: vi.fn(),
    retry: vi.fn(),
  };
}

function panel(
  over: {
    data?: WorkProjectsAnswer | null;
    error?: string | null;
    searched?: WorkProjectsAnswer | null;
    query?: string;
    at?: ScopeAt | null;
    recents?: Recent[];
    newProject?: ConfigWriteAccess;
  } = {},
  acts = actions(),
) {
  return panelOf(
    {
      base: {
        data: over.data === undefined ? listing(FAIRSPLIT) : over.data,
        error: over.error ?? null,
      },
      searched: over.searched ?? null,
      query: over.query ?? "",
      at:
        over.at === undefined
          ? { scope: { kind: "project", key: "FAIR" }, lens: "tasks" }
          : over.at,
      recents: over.recents ?? [],
      newProject: over.newProject ?? { can: true },
    },
    acts,
  );
}

describe("the panel, as data", () => {
  test("All projects leads, with the open work across every active project", () => {
    const p = panel();
    expect(p.pinned?.label).toBe("All projects");
    expect(p.pinned?.meta).toBe(10);
    expect(p.pinned?.accessibleLabel).toBe("All projects, 10 open");
    expect(p.pinned?.current).toBe(false);
    expect(panel({ at: { scope: { kind: "all" }, lens: "projects" } }).pinned?.current).toBe(true);
  });

  test("every active project is a row: its key, its name, its open count, and the current one", () => {
    const p = panel();
    const rows = p.groups?.find((g) => g.id === "projects");
    expect(rows?.label).toBe("Projects");
    expect(rows?.count).toBe(5);
    const fair = rows?.options.find((o) => o.id === "FAIR");
    expect(fair).toMatchObject({
      label: "Product and Engineering",
      keyChip: "FAIR",
      meta: 2,
      accessibleLabel: "Product and Engineering, FAIR, 2 open",
      href: "#/worklet/FAIR",
      current: true,
    });
    // ZERO IS A FIGURE, drawn as 0.
    expect(rows?.options.find((o) => o.id === "SETL")?.meta).toBe(0);
  });

  test("taking a row switches to its scope", () => {
    const a = actions();
    const p = panel({}, a);
    p.groups?.[0]?.options.find((o) => o.id === "SPLIT")?.onSelect?.();
    p.pinned?.onSelect?.();
    expect(a.went).toEqual(["SPLIT", "all"]);
  });

  // NEW PROJECT IS HELD BY THE ORG CHART'S GATE, in its own words: the key
  // mark only where a grant is what is missing, since offline is not cleared
  // by asking an administrator.
  test("the footer browses the projects and makes a new one, held with the gate's reason", () => {
    expect(panel().footer.map((o) => o.label)).toEqual(["Browse all projects", "New project…"]);
    const held = panel({
      newProject: { can: false, block: "no_grant", reason: CONFIG_WRITE_REASONS.no_grant },
    }).footer.find((o) => o.id === "new");
    expect(held?.held).toBe(CONFIG_WRITE_REASONS.no_grant);
    expect(held?.onSelect).toBeUndefined();
    expect(held?.meta).toBeDefined();
    const offline = panel({
      newProject: { can: false, block: "offline", reason: CONFIG_WRITE_REASONS.offline },
    }).footer.find((o) => o.id === "new");
    expect(offline?.held).toBe(CONFIG_WRITE_REASONS.offline);
    expect(offline?.meta).toBeUndefined();
  });

  test("while loading it draws placeholder rows and offers only New project", () => {
    const p = panel({ data: null });
    expect(p.loading).toBe(true);
    expect(p.pinned).toBeUndefined();
    expect(p.footer.map((o) => o.id)).toEqual(["new"]);
  });

  test("a listing that did not load says so, offers Try again, and keeps the panel open for it", () => {
    const a = actions();
    const p = panel({ data: null, error: "unavailable" }, a);
    expect(p.message?.title).toBe("The project list did not load");
    expect(p.message?.body).toBe("The page behind it is unchanged.");
    const retry = p.message?.actions?.[0];
    expect(retry?.label).toBe("Try again");
    expect(retry?.keepsOpen).toBe(true);
    retry?.onSelect?.();
    expect(a.retry).toHaveBeenCalled();
  });

  test("a company with no projects is told how one appears", () => {
    const p = panel({ data: listing([], { census: { active: 0, archived: 0 } }) });
    expect(p.message).toEqual({
      title: "No projects yet",
      body: "Create one with New project.",
    });
    expect(p.footer.map((o) => o.id)).toEqual(["new"]);
  });

  test("a company that archived every project is told where they are", () => {
    const p = panel({ data: listing([], { census: { active: 0, archived: 3 } }) });
    expect(p.message?.title).toBe("No active projects");
    expect(p.message?.body).toBe("Archived projects are under Projects.");
    expect(p.footer.map((o) => o.id)).toEqual(["browse", "new"]);
  });

  test("a listing cut at the cap says so, and its open figure is a floor", () => {
    const p = panel({
      data: listing(FAIRSPLIT, { truncated: true, census: { active: 340, archived: 0 } }),
    });
    expect(p.note).toBe("5 of 340. Type to search.");
    expect(p.pinned?.meta).toBe("10+");
    expect(figuresOf(listing(FAIRSPLIT, { truncated: true })).open).toBe("10+");
  });

  test("past eight projects the ones opened last lead, the current one aside", () => {
    const many = [...FAIRSPLIT, ...["P1", "P2", "P3", "P4"].map((k) => project(k, `Project ${k}`))];
    const recents: Recent[] = [
      { path: ["worklet", "FAIR"], label: "Product and Engineering", workspace: "worklet" },
      { path: ["worklet", "FAIR-3"], label: "A task", workspace: "worklet" },
      { path: ["worklet", "SPLIT"], label: "Split engine", workspace: "worklet" },
      { path: ["worklet", "OLD"], label: "Archived", workspace: "worklet" },
      { path: ["worklet", "APP"], label: "Mobile app", workspace: "worklet" },
      { path: ["worklet", "P2"], label: "Project P2", workspace: "worklet" },
      { path: ["worklet", "P3"], label: "Project P3", workspace: "worklet" },
    ];
    const p = panel({ data: listing(many), recents });
    const recent = p.groups?.find((g) => g.id === "recent");
    expect(recent?.options.map((o) => o.id)).toEqual(["SPLIT", "APP", "P2"]);
    // NOT WITH A QUERY, and not for a company of eight or fewer.
    expect(panel({ data: listing(many), recents, query: "s" }).groups?.map((g) => g.id)).toEqual([
      "projects",
    ]);
    expect(panel({ recents }).groups?.map((g) => g.id)).toEqual(["projects"]);
  });

  test("a search ranks keys first, says how many, and keeps All projects only where it matches", () => {
    const p = panel({ query: "s" });
    expect(p.groups?.[0]?.options.map((o) => o.id)).toEqual(["SETL", "SPLIT", "RCPT"]);
    expect(p.announcement).toBe("3 projects");
    expect(p.pinned).toBeUndefined();
    expect(panel({ query: "proj" }).pinned?.label).toBe("All projects");
    expect(panel({ query: "fair" }).announcement).toBe("1 project");
  });

  test("a search that matches nothing says so, points at Projects, and offers a task search", () => {
    const a = actions();
    const p = panel({ query: "invoice" }, a);
    expect(p.groups).toEqual([]);
    expect(p.message?.title).toBe("No project matches “invoice”");
    expect(p.message?.body).toBe("Archived projects are listed under Projects.");
    const search = p.message?.actions?.[0];
    expect(search?.label).toBe("Search tasks for “invoice”");
    expect(search?.href).toBe("#/worklet/search?q=invoice");
    search?.onSelect?.();
    expect(a.searchTasks).toHaveBeenCalledWith("invoice");
    expect(p.announcement).toBe("No project matches");
  });

  test("past the cap, the engine's answer is the list once it lands, and nothing is said before", () => {
    const cut = listing(FAIRSPLIT, { truncated: true, census: { active: 340, archived: 0 } });
    const pending = panel({ data: cut, query: "billing" });
    expect(pending.loading).toBe(true);
    expect(pending.message).toBeUndefined();
    expect(pending.announcement).toBeUndefined();
    const answered = panel({
      data: cut,
      query: "billing",
      searched: listing([project("BILL", "Billing"), project("OPS", "Operations")]),
    });
    // THE ENGINE MATCHED OPS ON ITS PURPOSE: kept, after the name match.
    expect(answered.groups?.[0]?.options.map((o) => o.id)).toEqual(["BILL", "OPS"]);
    expect(answered.announcement).toBe("2 projects");
  });

  test("All projects' figures are the active census and the open work", () => {
    expect(figuresOf(listing(FAIRSPLIT))).toEqual({ projects: 5, open: 10 });
    expect(figuresOf(null)).toEqual({});
    // AN ANSWER WITH NO CENSUS IN IT IS NOT A CENSUS OF NOTHING.
    const bare = { ...listing(FAIRSPLIT) } as Partial<WorkProjectsAnswer>;
    delete bare.census;
    expect(figuresOf(bare as WorkProjectsAnswer)).toEqual({ open: 10 });
  });
});

// ---------------------------------------------------------------------------
// In the frame
// ---------------------------------------------------------------------------

function mount(answers: Record<string, unknown>, services?: OrgServices, asked: string[] = []) {
  const store = new Store();
  // THE SOCKET'S FIRST FRAME CARRIES THE ORG, as a served frame's always
  // does: what the company runs is known before a read is asked.
  store.applyOrg(withDerived({ name: "Acme", roles: [], ...(services ? { services } : {}) }));
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    if (what === "viewer") {
      return Promise.resolve({
        login: "ada",
        grants: ["worklet", "admin"],
        handle: "ada",
        owner: "ada",
        name: "Ada",
        kind: "human",
        acts: [],
        config_writer: true,
        config_managed_by: [],
      });
    }
    return what in answers ? Promise.resolve(answers[what]) : new Promise(() => {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>
          <div />
        </Shell>
      </Router>
    </ClientContext.Provider>,
  );
}

async function settle(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
}

function key(k: string): void {
  act(() => {
    window.dispatchEvent(new KeyboardEvent("keydown", { key: k, bubbles: true, cancelable: true }));
  });
}

describe("in the frame", () => {
  test("G then P away from Work opens the panel centred, and a row moves the page", async () => {
    location.hash = "#/home";
    mount({ work_projects: listing(FAIRSPLIT) });
    await settle();
    key("g");
    key("p");
    const dialog = await screen.findByRole("dialog", { name: "Switch project" });
    expect(dialog.classList.contains("crewlet-switcher--centered")).toBe(true);
    // THE LISTING IS WAITED FOR, NOT ASSUMED. Away from Work nothing asks
    // for it until G then P opens the panel, so the panel and the answer it
    // asked for are drawn on two schedules, and settle()'s fixed count of
    // microtasks waits on neither: under load the panel was found first and
    // the click met a list still busy — as it does every time the listing
    // is answered 300 ms late.
    fireEvent.click(
      await within(dialog).findByRole("option", { name: "Split engine, SPLIT, 4 open" }),
    );
    await waitFor(() => expect(location.hash).toBe("#/worklet/SPLIT"));
    expect(screen.queryByRole("dialog", { name: "Switch project" })).toBeNull();
  });

  test("on Work, the trail's heading is the button, and G then P opens the panel under it", async () => {
    location.hash = "#/worklet/tasks";
    mount({ work_projects: listing(FAIRSPLIT) });
    await settle();
    const button = within(screen.getByRole("heading", { level: 1 })).getByRole("button", {
      name: "All projects",
    });
    expect(button.getAttribute("aria-expanded")).toBe("false");
    key("g");
    key("p");
    const dialog = await screen.findByRole("dialog", { name: "Switch project" });
    expect(dialog.classList.contains("crewlet-switcher--anchored")).toBe(true);
    expect(button.getAttribute("aria-expanded")).toBe("true");
    // ALL PROJECTS IS THE SCOPE THE PAGE IS ON.
    await settle();
    expect(
      within(dialog)
        .getByRole("option", { name: "All projects, 10 open" })
        .getAttribute("aria-current"),
    ).toBe("page");
    // TASKS CARRIES OVER, and so does the list's shape.
    fireEvent.click(within(dialog).getByRole("option", { name: "Settle up, SETL, 0 open" }));
    await waitFor(() => expect(location.hash).toBe("#/worklet/SETL"));
  });

  test("the lenses beside All projects carry the listing's figures", async () => {
    location.hash = "#/worklet";
    mount({ work_projects: listing(FAIRSPLIT) });
    await settle();
    const lenses = screen.getByRole("navigation", { name: "Lens" });
    expect(within(lenses).getByRole("link", { name: /^Projects/ }).textContent).toContain("5");
    expect(within(lenses).getByRole("link", { name: /^Tasks/ }).textContent).toContain("10");
    expect(document.title).toBe("Projects · All projects · Crewlet");
  });
});

// A HIDDEN WORK ADDRESS STILL NAMES A SCOPE, and the switcher asked the
// company's projects on it where the company runs Jira, and was refused.
// Mutation: gate the read on the reader's grant alone and it is asked.
test("on a company running Jira the switcher asks no projects, even on a Work address", async () => {
  location.hash = "#/worklet/tasks";
  const asked: string[] = [];
  mount({}, { tracker: "jira", knowledge: "doclet", chat: "chatlet" }, asked);
  await settle();
  expect(asked.filter((w) => w === "work_projects")).toEqual([]);
  location.hash = "#/";
});
