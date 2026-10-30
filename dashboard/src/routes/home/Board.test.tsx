/**
 * Home as the reader's own board: what it draws before anybody arranges it,
 * and how Edit Home arranges it.
 *
 * Its failure modes are quiet ones: a widget added from the tray and then
 * lost, a Cancel that kept what it promised to throw away, a sheet with no
 * way back, an Undo key that also undid the text being typed, a board saved
 * under nobody's name, and a tray offering a widget the reader's access
 * would only refuse.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useEffect, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Home } from "./Home.tsx";
import { Router, useNavigator, type Navigator } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { noteReader } from "~/lib/reader.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { FrameReadings } from "~/app/Shell.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { healthFrame } from "~/test/health.ts";
import { pick } from "~/testing.tsx";

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
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["comment_on_work_item", "answer_run", "update_work_item", "create_work_item"],
};

/** A reader who holds the tracker and the knowledge base but not Admin. */
const LEE = { ...JANE, login: "U0LEE", grants: ["worklet", "doclet"], handle: "", owner: "lee" };

const ORG = {
  name: "Nimbus",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "CTO", handle: "cto", kind: "agent" },
  ],
  units: [],
  derived: { seats: [], units: [] },
};

const QUIET = {
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  work_flow: {
    bucket: "day",
    points: [],
    now: { not_started: 0, active: 0, blocked: 0, overdue: 0 },
    blocked_history: false,
    complete: true,
  },
  token_series: {
    group: "unit",
    bucket: "day",
    series: [],
    by_group: [],
    totals: { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 },
    grouped: { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 },
  },
  company_feed: { rows: [], complete: true },
  work_projects: { projects: [], total: 0, census: {}, complete: true },
  sandbox_runs: { runs: [] },
  work_inbox: { handle: "jane", notices: [], primary_reasons: [] },
};

/** Each case reads as a reader of its own, so no case sees another's saved board. */
let readers = 0;
let reader = "";

let nav: Navigator | null = null;
function NavProbe() {
  const navigator = useNavigator();
  useEffect(() => {
    nav = navigator;
  }, [navigator]);
  return null;
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/home";
  readers += 1;
  reader = `board-reader-${readers}`;
  noteReader(reader);
  // THE FRAME LEARNS WHO READS THE TAB from `GET /auth/session`, so the
  // session answers as this case's reader and the board is kept under them.
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      String(url).includes("/auth/session")
        ? new Response(JSON.stringify({ person: reader, name: "Jane Founder" }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          })
        : new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }),
    ),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  reloadForTest();
  location.hash = "";
  nav = null;
});

function mount({
  viewer = JANE,
  services,
}: { viewer?: Record<string, unknown>; services?: Record<string, string> } = {}) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg((services ? { ...ORG, services } : ORG) as never);
  store.applySeats([] as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string) => {
    const all: Record<string, unknown> = { ...QUIET, viewer };
    return Promise.resolve(what in all ? all[what] : {});
  }) as typeof socket.query;
  const page: ReactNode = (
    <>
      <Home />
      <NavProbe />
    </>
  );
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <SessionReading>
            <FrameReadings>
              <Router>{page}</Router>
            </FrameReadings>
          </SessionReading>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
}

/** The blocks on the page, outside any group. */
const blocks = () => [...document.querySelectorAll(".crewlet-arrange-grid > [data-widget]")];
const notes = () => document.querySelectorAll(".crewlet-note").length;
const editBar = () => screen.getByRole("group", { name: "Editing Home" });
const tray = () => screen.getByRole("dialog", { name: "Add to Home" });
const queryTray = () => screen.queryByRole("dialog", { name: "Add to Home" });

async function editHome() {
  fireEvent.click(screen.getByRole("button", { name: "Edit Home" }));
  await settle();
}

async function openTray() {
  fireEvent.click(within(editBar()).getByRole("button", { name: /Add widgets/ }));
  await settle();
}

/** A tile in the tray, by the name it is offered under. */
function tile(name: string): HTMLElement {
  const found = within(tray())
    .getAllByRole("button")
    .find((b) => b.querySelector(".home-tray-tile-name")?.textContent === name);
  if (!found) throw new Error(`no tile named ${name}`);
  return found;
}

async function closeTray() {
  fireEvent.click(within(tray()).getByRole("button", { name: "Done" }));
  await settle();
}

async function addNote() {
  await openTray();
  fireEvent.click(tile("Note"));
  await settle();
  await closeTray();
}

describe("at rest", () => {
  test("Home draws the board it shipped with, with Edit Home and New task", async () => {
    mount();
    await settle();
    expect(blocks()).toHaveLength(7);
    expect(screen.getByRole("region", { name: "The company at a glance" })).toBeTruthy();
    for (const title of [
      /^Needs your decision/,
      /^Live now/,
      "Tasks completed",
      "Tokens by team",
      "Projects",
      "Recent activity",
    ]) {
      expect(screen.getByRole("heading", { name: title })).toBeTruthy();
    }
    expect(screen.getByRole("button", { name: "Edit Home" })).toBeTruthy();
    expect(screen.getByRole("button", { name: /New task/ })).toBeTruthy();
  });
});

// A COMPANY RUNNING JIRA HIDES THE TRACKER'S WIDGETS, and each row closes up
// over them: Tokens by team stood alone in a third of its row with two thirds
// of the board empty beside it. Every row the board draws is as wide as the
// row it was saved as. While Home is edited the hidden widget keeps its cell
// and says why, so it can be removed. Mutation: draw the saved widths and a
// row comes up short.
describe("where the company runs no Worklet", () => {
  const span = (el: Element) =>
    Number(/span (\d+)/.exec((el as HTMLElement).style.gridColumn)?.[1] ?? "0");
  test("every row of the board is drawn full, the tracker's widgets left out", async () => {
    mount({ services: { tracker: "jira", knowledge: "doclet", chat: "chatlet" } });
    await settle();
    const drawn = blocks();
    const ids = drawn.map((b) => b.getAttribute("data-widget"));
    expect(ids.some((id) => id?.includes("completed"))).toBe(false);
    let row = 0;
    const rows: number[] = [];
    for (const b of drawn) {
      if (row + span(b) > 12) {
        rows.push(row);
        row = 0;
      }
      row += span(b);
    }
    rows.push(row);
    expect(
      rows.every((r) => r === 12),
      `rows ${rows.join(", ")}`,
    ).toBe(true);
  });

  // THE TRAY DESCRIBES WHAT THE READER WILL GET: no task figures, and no
  // finished work or hand-offs, where Worklet is not run.
  test("the tray's lines say what each widget shows here", async () => {
    mount({ services: { tracker: "jira", knowledge: "doclet", chat: "chatlet" } });
    await settle();
    await editHome();
    await openTray();
    expect(within(tray()).getByText("Agents, decisions and tokens at a glance")).toBeTruthy();
    expect(within(tray()).getByText("Schedules run and pages published")).toBeTruthy();
    expect(within(tray()).queryByText(/tasks and tokens/)).toBeNull();
  });

  test("in Edit Home a hidden widget keeps its cell and says why", async () => {
    mount({ services: { tracker: "jira", knowledge: "doclet", chat: "chatlet" } });
    await settle();
    await editHome();
    expect(
      screen.getAllByText(/^Hidden: the company does not run Worklet\./).length,
    ).toBeGreaterThan(0);
  });
});

describe("Edit Home", () => {
  test("puts the editor's controls in the bar and New task out of it", async () => {
    mount();
    await settle();
    await editHome();
    const bar = editBar();
    expect(within(bar).getByText("Editing layout")).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Undo" }).getAttribute("aria-disabled")).toBe(
      "true",
    );
    expect(within(bar).getByRole("button", { name: "Redo" }).getAttribute("aria-disabled")).toBe(
      "true",
    );
    expect(within(bar).getByRole("button", { name: /Add widgets/ })).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Done" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit Home" })).toBeNull();
    expect(screen.queryByRole("button", { name: /New task/ })).toBeNull();
  });

  test("a note added from the tray lands on the board, and Undo and Redo take it back and forth", async () => {
    mount();
    await settle();
    await editHome();
    await addNote();
    expect(notes()).toBe(1);
    expect(within(editBar()).getByText("1 unsaved change")).toBeTruthy();

    fireEvent.click(within(editBar()).getByRole("button", { name: "Undo" }));
    await settle();
    expect(notes()).toBe(0);
    fireEvent.click(within(editBar()).getByRole("button", { name: "Redo" }));
    await settle();
    expect(notes()).toBe(1);
  });

  test("the platform's keys undo and redo from the page, but not from a text field", async () => {
    mount();
    await settle();
    await editHome();
    await addNote();
    expect(notes()).toBe(1);

    fireEvent.keyDown(document.body, { key: "z", ctrlKey: true });
    await settle();
    expect(notes()).toBe(0);
    fireEvent.keyDown(document.body, { key: "z", ctrlKey: true, shiftKey: true });
    await settle();
    expect(notes()).toBe(1);

    // A group's name is a field on the board: the keys belong to its text.
    await openTray();
    fireEvent.click(tile("Group"));
    await settle();
    await closeTray();
    const field = screen.getByRole("textbox", { name: "Group name" });
    const before = blocks().length;
    fireEvent.keyDown(field, { key: "z", ctrlKey: true });
    await settle();
    expect(blocks()).toHaveLength(before);
  });

  test("Done keeps the board under the reader's own name, and Home draws it again", async () => {
    mount();
    await settle();
    await editHome();
    await addNote();
    fireEvent.click(within(editBar()).getByRole("button", { name: "Done" }));
    await settle();
    const saved = localStorage.getItem(`${STORAGE_KEYS.homeBoard}${reader}`);
    expect(saved).toContain('"kind":"note"');
    expect(screen.getByRole("button", { name: "Edit Home" })).toBeTruthy();

    cleanup();
    mount();
    await settle();
    expect(notes()).toBe(1);
  });

  test("Cancel with changes asks, and discarding puts the saved board back", async () => {
    mount();
    await settle();
    await editHome();
    await addNote();
    fireEvent.click(within(editBar()).getByRole("button", { name: "Cancel" }));
    await settle();
    const ask = screen.getByRole("alertdialog", { name: "Discard your changes?" });
    fireEvent.click(within(ask).getByRole("button", { name: "Discard changes" }));
    await settle();
    expect(notes()).toBe(0);
    expect(blocks()).toHaveLength(7);
    expect(screen.getByRole("button", { name: "Edit Home" })).toBeTruthy();
  });

  test("leaving with unsaved changes asks first", async () => {
    mount();
    await settle();
    await editHome();
    await addNote();
    act(() => nav!.to(["inbox"]));
    await settle();
    expect(screen.getByRole("alertdialog", { name: "Leave without saving?" })).toBeTruthy();
    expect(location.hash).toBe("#/home");
  });
});

describe("a widget with a choice", () => {
  test("is chosen in a sheet of its own, with a way back that adds nothing", async () => {
    mount();
    await settle();
    await editHome();
    await openTray();
    const before = blocks().length;
    fireEvent.click(tile("Page shortcut"));
    await settle();
    const sheet = screen.getByRole("dialog", { name: "Add Page shortcut" });
    expect(queryTray()).toBeNull();
    expect(blocks()).toHaveLength(before);
    expect(
      within(sheet).getByRole("button", { name: "Add to Home" }).getAttribute("aria-disabled"),
    ).toBe("true");

    fireEvent.click(within(sheet).getByRole("button", { name: "Back to widgets" }));
    await settle();
    expect(screen.queryByRole("dialog", { name: "Add Page shortcut" })).toBeNull();
    expect(tray()).toBeTruthy();
    expect(blocks()).toHaveLength(before);
  });

  test("is placed once its choice is made", async () => {
    mount();
    await settle();
    await editHome();
    await openTray();
    const before = blocks().length;
    fireEvent.click(tile("Page shortcut"));
    await settle();
    pick(screen.getByLabelText("Page"), /Backups/);
    await settle();
    const sheet = screen.getByRole("dialog", { name: /^Add Shortcut to Backups/ });
    const add = within(sheet).getByRole("button", { name: "Add to Home" });
    expect(add.getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.click(add);
    await settle();
    expect(blocks()).toHaveLength(before + 1);
    expect(document.querySelector(".home-shortcut")?.textContent).toContain("Backups");
    expect(tray()).toBeTruthy();
  });

  test("a placed one edited and then cancelled is put back as it was", async () => {
    mount();
    await settle();
    await editHome();
    await openTray();
    fireEvent.click(tile("Figure"));
    await settle();
    pick(screen.getByLabelText("Figure"), "Tokens");
    await settle();
    fireEvent.click(
      within(screen.getByRole("dialog", { name: "Add Tokens" })).getByRole("button", {
        name: "Add to Home",
      }),
    );
    await settle();
    await closeTray();
    expect(screen.getByRole("region", { name: "Tokens" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Edit Tokens" }));
    await settle();
    pick(screen.getByLabelText("Figure"), "Completed");
    await settle();
    expect(screen.getByRole("region", { name: "Completed" })).toBeTruthy();
    fireEvent.click(
      within(screen.getByRole("dialog", { name: "Edit Completed" })).getByRole("button", {
        name: "Cancel",
      }),
    );
    await settle();
    expect(screen.getByRole("region", { name: "Tokens" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Completed" })).toBeNull();
  });
});

describe("the tray", () => {
  test("a card already on the board is offered as on your Home, and is not added twice", async () => {
    mount();
    await settle();
    await editHome();
    await openTray();
    const placed = tile("Needs your decision");
    expect(placed.textContent).toContain("On your Home");
    expect(placed.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(placed);
    await settle();
    expect(screen.getAllByRole("heading", { name: /^Needs your decision/ })).toHaveLength(1);
  });

  test("search narrows the widgets by their words, and says when nothing matches", async () => {
    mount();
    await settle();
    await editHome();
    await openTray();
    const search = within(tray()).getByRole("searchbox", { name: "Search widgets" });
    fireEvent.change(search, { target: { value: "backup" } });
    await settle();
    const names = [...tray().querySelectorAll(".home-tray-tile-name")].map((n) => n.textContent);
    expect(names).toContain("Backups");
    expect(names).not.toContain("Note");

    fireEvent.change(search, { target: { value: "zzzz" } });
    await settle();
    expect(within(tray()).getByText("No widgets match “zzzz”")).toBeTruthy();
  });

  // SPEND'S READINGS AND THE MODELS ARE EVERY READER'S, and the rest of
  // Settings is Admin's: a reader without Admin is offered those widgets and
  // none of the rest.
  test("a reader without Admin is offered only Settings' spend and models widgets, and is told why", async () => {
    mount({ viewer: LEE });
    await settle();
    await editHome();
    await openTray();
    const sections = [...tray().querySelectorAll(".home-tray-title")].map((h) => h.textContent);
    expect(sections).toContain("Settings");
    expect(sections).toContain("Worklet");
    const names = [...tray().querySelectorAll(".home-tray-tile-name")].map((n) => n.textContent);
    expect(names).toContain("Token spend");
    expect(names).toContain("Models");
    expect(names).not.toContain("Backups");
    expect(within(tray()).getByText(/need access you don’t have/)).toBeTruthy();
  });
});
