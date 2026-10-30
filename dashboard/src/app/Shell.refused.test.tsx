/**
 * A person the socket refuses is told who they are, and can still reach their
 * Account and end their own session — and a person with no access to an app
 * is never refused for it.
 *
 * The engine refuses a person whose seat was taken out of the chart `403
 * seat_unavailable` everywhere but `/auth/` — the socket included — and the
 * socket then stops dialling, so the `viewer` question, which is asked over
 * it, is never answered. The identity menu (now the sidebar's user block)
 * drew nothing until it was and the refusal's banner offered only a retry, so
 * the people the engine had shut out were exactly the people left with no way
 * to sign out: on a shared machine their cookie stayed live until its own
 * deadline. And every screen mounted anyway — Home said it was "not
 * connected" and showing "the last state it sent" when nothing was ever sent,
 * beside figures that never resolved. So every screen but the Account is
 * replaced by one panel that says who they are, what they hold and why.
 *
 * ACCESS IS NEVER A REFUSAL. The socket serves every principal the engine
 * resolves the overview and decides each push by what they hold, so a session
 * holding no app is dialled like any other: its sidebar locks the apps it
 * lacks, naming each, and a section it may not open is the one sentence that
 * says which access would open it.
 */

import { withDerived } from "~/test/org.ts";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { App } from "./App.tsx";
import { loadChunk } from "./lazyScreen.ts";
import { Router } from "./router.tsx";
import { workspaceRow } from "./nav.ts";
import { page } from "~/lib/session.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, sessionRestored } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

interface Sent {
  method: string;
  path: string;
}

let sent: Sent[];
let reloads: ReturnType<typeof vi.spyOn>;
/** What `GET /auth/session` says the session holds. */
let grants: string[];

// THE TWO SCREENS' CHUNKS ARE THE SUITE'S TO LOAD, once: imported cold inside
// the first case they took two of its five seconds alone, and past five under
// the full suite's load, so that case timed out on whichever run was busiest.
beforeAll(async () => {
  await loadChunk("worklet");
  await loadChunk("account");
  await loadChunk("settings");
});

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  location.hash = "#/worklet";
  sent = [];
  grants = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      const method = (init?.method ?? "GET").toUpperCase();
      sent.push({ method, path: url.pathname });
      const json = (body: unknown, status = 200) =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        });
      // `/auth/` IS STILL SERVED to this session, which is the whole point.
      if (method === "GET" && url.pathname === "/auth/session") {
        return json({
          person: "p-1",
          login: "jane.doe",
          kind: "person",
          grants,
          expires_at: "2026-10-05T00:00:00Z",
          status: "signed_in",
        });
      }
      if (method === "POST" && url.pathname === "/auth/logout") return json({});
      // Everything else is refused, as the guard refuses it.
      return json(
        { error: "seat_unavailable", detail: "your seat is no longer in the chart" },
        403,
      );
    }),
  );
  reloads = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  sessionRestored();
  sessionStorage.clear();
  location.hash = "#/";
});

/**
 * The shell, whose socket's questions never answer. The Work screen's chunk
 * was fetched before the suite, so the screen draws rather than suspending
 * inside the render. `refused` is the engine's refusal of the dial the frame
 * makes for every session, landing once it has made it.
 */
async function mount(
  refused: string | null = "your seat is no longer in the chart",
  answer: (what: string) => Promise<unknown> = () => new Promise(() => {}),
) {
  const store = new Store();
  // A SERVED SESSION'S FIRST FRAME CARRIES THE ORG; a refused one's never
  // comes, so nothing about what the company runs is known to it.
  if (refused === null) store.applyOrg(withDerived({ name: "Acme", roles: [] }));
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = answer;
  const dial = vi.spyOn(socket, "start");
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
  if (refused !== null) {
    await waitFor(() => expect(dial).toHaveBeenCalled());
    act(() => store.setAccessRefused(refused));
  }
  return { store, dial };
}

/** The panel drawn in place of the screen. */
async function panelTitled(title: string): Promise<HTMLElement> {
  return (await screen.findByText(title)).closest(".crewlet-empty-state") as HTMLElement;
}

test("every screen is one panel giving the engine's reason, and it signs out", async () => {
  grants = ["worklet"];
  await mount();
  const panel = await panelTitled("You can’t use Crewlet right now");
  expect(
    within(panel).getByText("Your seat is no longer in the chart. Your Account still works."),
  ).toBeDefined();
  // NOTHING SAYS IT IS RECONNECTING, and nothing the screen would have asked
  // is drawn: the socket was refused and stopped.
  expect(screen.queryByText(/Reconnecting|Not connected/)).toBeNull();
  // AND THE FOOT NAMES THEM, from the session, though the viewer never answers.
  expect(screen.queryByText("Checking who you are")).toBeNull();

  fireEvent.click(within(panel).getByRole("button", { name: "Sign out" }));
  await waitFor(() => expect(reloads).toHaveBeenCalledWith("#/login"));
  expect(sent.some((s) => s.method === "POST" && s.path === "/auth/logout")).toBe(true);
});

test("the panel's Account is the one screen it does not replace", async () => {
  await mount();
  fireEvent.click(await screen.findByRole("button", { name: "Open Account" }));
  await waitFor(() => expect(location.hash).toBe("#/account"));
  await waitFor(() => expect(screen.queryByText("You can’t use Crewlet right now")).toBeNull());
});

test("the user block offers both sign-outs though the viewer never answers", async () => {
  await mount(null);
  // THE ROW IS NAMED BY THE SESSION'S OWN LOGIN, the viewer that would name a
  // seat never answering, and its menu holds the way to their Account, which
  // reads no socket.
  fireEvent.click(await screen.findByRole("button", { name: /jane\.doe/ }));
  const menu = await screen.findByRole("menu", { name: "Your account" });
  expect(within(menu).getByRole("menuitem", { name: /^Account/ })).toBeDefined();
  expect(within(menu).getByRole("menuitem", { name: /^Sign out everywhere/ })).toBeDefined();
  expect(within(menu).getByRole("menuitem", { name: "Sign out" })).toBeDefined();
});

// A SESSION HOLDING NO APP IS DIALLED, as every session is: the socket serves
// it the overview. Its sidebar draws every row, locking — never hiding — the
// apps it lacks, each naming the access that opens it, while the overview's
// rows stay open. Mutation: hold the dial back for a session holding nothing,
// or lock the overview, and a line here goes red.
test("a session holding no app is dialled, and its sidebar locks the apps by name", async () => {
  const { dial } = await mount(null);
  await waitFor(() => expect(dial).toHaveBeenCalled());
  const rows = screen.getByRole("navigation", { name: "Navigation" });
  const row = (name: RegExp) => within(rows).getByRole("link", { name });
  await waitFor(() =>
    expect(row(/^Inbox/).querySelector("[title='Needs Worklet access']")).not.toBeNull(),
  );
  expect(row(/^Worklet/).querySelector("[title='Needs Worklet access']")).not.toBeNull();
  expect(row(/^Doclet/).querySelector("[title='Needs Doclet access']")).not.toBeNull();
  expect(row(/^Chatlet/).querySelector("[title='Needs Chatlet access']")).not.toBeNull();
  expect(row(/^Home/).querySelector(".crewlet-locked-label")).toBeNull();
  expect(row(/^Principles/).querySelector(".crewlet-locked-label")).toBeNull();
  expect(row(/^Org chart/).querySelector(".crewlet-locked-label")).toBeNull();
  // NOTHING SAYS IT WAS REFUSED: there is no refusal to say.
  expect(screen.queryByText("You can’t use Crewlet right now")).toBeNull();
  expect(screen.queryByText(/Blocked|no access/i)).toBeNull();
});

// A LOCK GOES WHERE THE CLOSED THING IS. Spend's readings are every reader's,
// so the Settings row is never locked though most of its sections are, and
// Spend's row in the column is open. Mutation: lock a row for any closed
// section and the Settings row is locked.
test("a session holding no app finds Settings unlocked, and Spend open in it", async () => {
  location.hash = "#/settings/spend";
  await mount(null);
  // THE FOOT'S SETTINGS, its glyph beside the reader's own row, names no lock,
  // and says the reader is in Settings.
  const settings = await waitFor(() =>
    within(document.querySelector<HTMLElement>(".crewlet-app-shell__rail-foot")!).getByRole(
      "link",
      {
        name: "Settings",
      },
    ),
  );
  expect(settings.getAttribute("title")).toBe("Settings");
  expect(settings.getAttribute("aria-current")).toBe("page");
  // AND IT OPENS ON ITS LANDING, Models, which every member reads: each
  // model's condition is what tells a member why a seat waits.
  await waitFor(() => expect(settings.getAttribute("href")).toBe("#/settings/models"));
  const column = await screen.findByRole("navigation", { name: "Settings sections" });
  const spend = within(column)
    .getAllByRole("link")
    .find((a) => a.getAttribute("href") === "#/settings/spend");
  expect(spend?.textContent).toBe("Token spend");
});

// ONE READING OF WHAT THE READER HOLDS: Settings' column draws its locks off
// the same reading the sidebar does (the session's, while the viewer has not
// answered), so every section a grant opens is locked naming that grant, and
// Spend is not. Mutation: hand the column the viewer's grants and no row says
// it is locked.
test("Settings' column is locked where a grant is, and nowhere else", async () => {
  location.hash = "#/settings/models";
  await mount(null);
  const column = await screen.findByRole("navigation", { name: "Settings sections" });
  const sections = new Map(
    (workspaceRow("settings")?.sections ?? []).map((s) => [`#/${s.path.join("/")}`, s.grants]),
  );
  const rows = within(column).getAllByRole("link");
  const locked = rows.filter((a) => (sections.get(a.getAttribute("href") ?? "") ?? []).length > 0);
  expect(locked.length).toBeGreaterThan(5);
  // THE LOCKS ARE DRAWN ONCE THE SESSION HAS ANSWERED, a commit after the
  // column itself, so they are waited for rather than read at once.
  await waitFor(() => {
    for (const a of rows) {
      const at = a.getAttribute("href") ?? "";
      if (locked.includes(a)) {
        expect(a.textContent, at).toMatch(/, Needs (Admin|Worklet) access/);
      } else {
        expect(a.textContent, at).not.toMatch(/needs/);
      }
    }
  });
});

// A ROUTE'S OWN LOCK READS THE SESSION TOO: a session whose socket never
// answers its viewer was held on "Checking your access to Nodes" for good.
// Mutation: decide the section on the viewer alone and the skeleton never
// goes. AND IT NAMES THE READER FROM THAT SAME READING: named off the viewer,
// it told a person signed in that nobody was.
test("a section the session's access does not open is locked though the viewer never answers", async () => {
  location.hash = "#/settings/nodes";
  await mount(null);
  expect(await screen.findByText("You need Admin access")).toBeDefined();
  expect(screen.queryByText("Checking your access to Nodes")).toBeNull();
  const main = screen.getByRole("main");
  expect(main.textContent).toMatch(/Signed in as jane\.doe/);
  expect(main.textContent).toMatch(/Ask an admin for access/);
  expect(main.textContent).not.toMatch(/You’re not signed in/);
  expect(within(main).getByRole("button", { name: "Switch account" })).toBeDefined();
});

// A REFUSAL ASKS WHO THIS IS AGAIN: the session the frame read before the
// refusal may hold what it no longer does. Mutation: drop the re-read and the
// count stays where it was.
test("a refusal asks who this is again", async () => {
  grants = ["worklet", "doclet"];
  const { store, dial } = await mount(null);
  await waitFor(() => expect(dial).toHaveBeenCalled());
  const before = sent.filter((s) => s.path === "/auth/session").length;
  grants = ["doclet"];
  act(() => store.setAccessRefused("your seat is no longer in the chart"));
  await panelTitled("You can’t use Crewlet right now");
  await waitFor(() =>
    expect(sent.filter((s) => s.path === "/auth/session").length).toBeGreaterThan(before),
  );
});

// NO NEW TASK IS OFFERED IN A FRAME THE ENGINE REFUSED, nor where Work is
// closed: every door is on a screen the panel or the lock replaces. The
// CONTROL is the same frame for a session holding Worklet, whose Tasks lens
// offers New task in its page bar.
test("a refused session is offered no New task, and is told why", async () => {
  location.hash = "#/worklet/tasks";
  const { store } = await mount();
  await panelTitled("You can’t use Crewlet right now");
  expect(screen.queryAllByRole("button", { name: /^New task/ })).toEqual([]);
  // AND THE REFUSAL IS THE ONE EVERY SURFACE READS, never "offline".
  expect(store.state.accessRefused).not.toBeNull();
  expect(screen.queryByText(/Offline\. Reconnect/)).toBeNull();
});

test("a session holding no Worklet finds Worklet locked, and no New task in it", async () => {
  location.hash = "#/worklet/tasks";
  await mount(null);
  expect(await screen.findByText("You need Worklet access")).toBeDefined();
  expect(screen.queryAllByRole("button", { name: /^New task$/ })).toEqual([]);
});

test("a session holding Worklet is offered New task", async () => {
  location.hash = "#/worklet/tasks";
  grants = ["worklet"];
  await mount(null);
  const press = await screen.findByRole("button", { name: /^New task$/ });
  expect(press.getAttribute("aria-disabled")).toBeNull();
});

// A REFUSAL DROPS WHAT THE SOCKET SENT: the tab went on naming the company and
// listing its agents from the snapshot it held. Mutation: keep the snapshot on
// a refusal and "Nimbus" stays.
test("a refusal keeps nothing the socket sent", async () => {
  const { store, dial } = await mount(null);
  await waitFor(() => expect(dial).toHaveBeenCalled());
  act(() => store.applyOrg({ name: "Nimbus" } as never));
  expect(await screen.findByText("Nimbus")).toBeDefined();
  act(() => store.setAccessRefused("your seat is no longer in the chart"));
  await panelTitled("You can’t use Crewlet right now");
  expect(screen.queryByText("Nimbus")).toBeNull();
  expect(store.state.org).toBeNull();
});

// A REFUSAL WITH NO REASON SAYS NOTHING EMPTY: it read "The engine says: ."
// Mutation: draw the reason sentence whatever it holds, and the panel carries
// a stray full stop.
test("a refusal the engine gave no reason for leaves the reason out", async () => {
  const { store, dial } = await mount(null);
  await waitFor(() => expect(dial).toHaveBeenCalled());
  act(() => store.setAccessRefused(""));
  const panel = await panelTitled("You can’t use Crewlet right now");
  expect(within(panel).getByText("Your Account still works.")).toBeDefined();
  expect(panel.textContent).not.toMatch(/: \.|^\./);
});
