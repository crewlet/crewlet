/**
 * A person the socket refuses is told who they are, and can still reach their
 * Account and end their own session.
 *
 * The engine refuses a person whose seat was taken out of the chart `403
 * seat_unavailable` everywhere but `/auth/`, and a person without `state:read`
 * the socket — and the socket then stops dialling, so the `viewer` question,
 * which is asked over it, is never answered. The identity menu (now the
 * sidebar's user block) drew nothing until it was and the refusal's banner
 * offered only a retry, so the people the engine had shut out were exactly the
 * people left with no way to sign out: on a shared machine their cookie stayed
 * live until its own deadline. And every screen mounted anyway — Home said it
 * was "not connected" and showing "the last state it sent" when nothing was
 * ever sent, beside figures that never resolved, while the sidebar's foot said
 * "Checking who you are" for good. So every screen but the Account is replaced
 * by one panel that says who they are, what they hold and why.
 *
 * A SESSION WITHOUT `state:read` IS NO ACCESS YET, read off the session the
 * frame asks for first: nothing is dialled for it — a dial was a refused
 * handshake, a refused probe and a refused snapshot, twice, on landing — the
 * panel, the sidebar's foot and its rows say so without alarm, and the
 * session is asked again, so a grant an administrator makes opens the
 * dashboard without a reload. Nothing else could tell the tab: the identity
 * move naming the person is pushed over the socket it does not have.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { App } from "./App.tsx";
import { loadChunk } from "./lazyScreen.ts";
import { Router } from "./router.tsx";
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
  await loadChunk("work");
  await loadChunk("account");
  await loadChunk("settings");
});

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  location.hash = "#/work";
  sent = [];
  grants = ["state:read"];
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
 * makes for a session holding `state:read`, landing once it has made it.
 */
async function mount(
  refused: string | null = "your seat is no longer in the chart",
  answer: (what: string) => Promise<unknown> = () => new Promise(() => {}),
) {
  const store = new Store();
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

/** The panel drawn in place of the screen, once it names the reader. */
async function panelTitled(title: string): Promise<HTMLElement> {
  const panel = (await screen.findByText(title)).closest(".crewlet-empty-state") as HTMLElement;
  await waitFor(() => expect(within(panel).getByText("jane.doe")).toBeDefined());
  return panel;
}

test("every screen is one panel naming who you are, and it signs out", async () => {
  await mount();
  const panel = await panelTitled("The engine will not serve this screen to you");
  expect(within(panel).getByText(/you hold state:read/)).toBeDefined();
  expect(within(panel).getByText(/your seat is no longer in the chart/)).toBeDefined();
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
  fireEvent.click(await screen.findByRole("button", { name: "Account" }));
  await waitFor(() => expect(location.hash).toBe("#/account"));
  await waitFor(() =>
    expect(screen.queryByText("The engine will not serve this screen to you")).toBeNull(),
  );
});

test("the user block offers both sign-outs though the viewer never answers", async () => {
  grants = [];
  await mount(null);
  fireEvent.click(await screen.findByRole("button", { name: "Account and preferences" }));
  const popover = await screen.findByRole("dialog", { name: "Account and preferences" });
  // UNDER THE SESSION'S OWN LOGIN, the viewer that would name a seat never
  // answering — with the way to their Account, which reads no socket.
  await waitFor(() => expect(within(popover).getByText("jane.doe")).toBeDefined());
  expect(within(popover).getByRole("link", { name: "Account" })).toBeDefined();
  expect(within(popover).getByRole("button", { name: "Sign out everywhere" })).toBeDefined();
  expect(within(popover).getByRole("button", { name: "Sign out" })).toBeDefined();
});

// NO ACCESS YET IS NOT A REFUSAL: a person invited with no grants landed on a
// red "Access refused" after two rounds of refused dials, every row offered and
// unmarked. The CONTROL is the seat-gone refusal above, which is drawn in the
// engine's words and dialled for. Mutation: dial whatever the session holds,
// or draw this state as the refusal, and a line here goes red.
test("a session without state:read dials nothing and is told what would open the dashboard", async () => {
  grants = [];
  const { dial } = await mount(null);
  const panel = await panelTitled("You have no access to the company yet");
  expect(within(panel).getByText(/you hold no grants yet/)).toBeDefined();
  expect(within(panel).getByRole("button", { name: "Check again" })).toBeDefined();
  expect(screen.queryByText("The engine will not serve this screen to you")).toBeNull();
  // THE FOOT SAYS IT WITHOUT ALARM, and nothing says it is reconnecting.
  expect(screen.getByText("No access yet")).toBeDefined();
  expect(screen.queryByText(/Access refused|Reconnecting|Not connected/)).toBeNull();
  // EVERY ROW IS LOCKED, NEVER HIDDEN, naming what would open it.
  const inbox = screen.getByRole("link", { name: /^Inbox/ });
  expect(inbox.querySelector("[title='Needs state:read']")).not.toBeNull();
  expect(dial).not.toHaveBeenCalled();
  expect(sent.some((s) => s.path === "/stream/snapshot" || s.path === "/ws/stream")).toBe(false);
  // ASKED ONCE: the frame records its own no-access from the read, and read it
  // again straight after for having recorded it. Mutation: re-read on the
  // frame's own refusal and the session is asked twice on landing.
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 50));
  });
  expect(sent.filter((s) => s.path === "/auth/session")).toHaveLength(1);
});

// A LOCK GOES WHERE THE CLOSED THING IS. Settings' first group, You — the
// reader's Account and Appearance — reads nothing of the company, so the
// Settings row is never locked: it carried "Needs state:read" here while the
// one thing this person came to Settings for was open to them. Every row whose
// whole workspace reads the company keeps its lock, and the reader's own
// Appearance is drawn, as their Account is, rather than the panel. Mutation:
// lock a row for any closed section, or refuse every screen but the Account,
// and a line here goes red.
test("a session without state:read finds Settings unlocked, and its own Appearance drawn", async () => {
  grants = [];
  location.hash = "#/settings/appearance";
  await mount(null);
  await screen.findByText("No access yet");
  const rows = screen.getByRole("navigation", { name: "Navigation" });
  const row = (name: RegExp) => within(rows).getByRole("link", { name });
  await waitFor(() =>
    expect(row(/^Home/).querySelector("[title='Needs state:read']")).not.toBeNull(),
  );
  expect(row(/^Work/).querySelector("[title='Needs state:read']")).not.toBeNull();
  const foot = screen.getByRole("navigation", { name: "Settings" });
  const settings = within(foot).getByRole("link", { name: /^Settings/ });
  expect(settings.querySelector(".side-locked")).toBeNull();
  expect(settings.textContent).toBe("Settings");
  expect(await screen.findByRole("radiogroup", { name: "Mode" })).toBeDefined();
  expect(screen.queryByText("You have no access to the company yet")).toBeNull();
});

// ONE READING OF WHAT THE READER HOLDS. Settings' column drew its locks off the
// viewer alone, which a session without `state:read` never gets an answer
// from — so for that person every company section from General to Audit log
// was drawn OPEN, while the sidebar beside it, reading the session, locked
// the rows. Every section the company's grant opens is locked naming it; the
// reader's own two are not; and the route that reads the company is the panel.
// Mutation: hand the column the viewer's grants and no row says it is locked.
test("a session without state:read finds Settings' column locked where the company is", async () => {
  grants = [];
  location.hash = "#/settings";
  await mount(null);
  await panelTitled("You have no access to the company yet");
  const column = await screen.findByRole("navigation", { name: "Settings sections" });
  const own = new Set(["#/account", "#/settings/appearance"]);
  const rows = within(column).getAllByRole("link");
  expect(rows.filter((a) => own.has(a.getAttribute("href") ?? ""))).toHaveLength(own.size);
  for (const a of rows) {
    const at = a.getAttribute("href") ?? "";
    if (own.has(at)) {
      expect(a.textContent, at).not.toMatch(/needs/);
      expect(a.querySelector(".sr-only")?.textContent ?? "", at).not.toMatch(/needs/);
    } else {
      expect(a.textContent, at).toMatch(/, needs state:read/);
    }
  }
});

// AND A GRANT WITHDRAWN IS READ OFF THE SESSION, though the viewer the socket
// answered before still lists it: the refusal that closed the socket is that
// very grant, so the column left every company section open beside the panel
// saying there was no access. Mutation: prefer an answered viewer whatever the
// session says, and General is drawn open.
test("a grant withdrawn under an answered viewer locks the column again", async () => {
  location.hash = "#/settings";
  const { store, dial } = await mount(null, (what) =>
    what === "viewer"
      ? Promise.resolve({ login: "jane.doe", owner: "jane.doe", grants: ["state:read"] })
      : new Promise(() => {}),
  );
  await waitFor(() => expect(dial).toHaveBeenCalled());
  const column = await screen.findByRole("navigation", { name: "Settings sections" });
  const general = () =>
    within(column)
      .getAllByRole("link")
      .find((a) => a.getAttribute("href") === "#/settings");
  await waitFor(() => expect(general()?.textContent).toBe("General"));
  grants = [];
  act(() => store.setAccessRefused("the live socket needs state:read"));
  await panelTitled("You have no access to the company yet");
  await waitFor(() => expect(general()?.textContent).toMatch(/, needs state:read/));
});

// AND A ROUTE'S OWN REFUSAL READS IT TOO: a session whose socket never answers
// its viewer was held on "Checking your access to Nodes" for good, while the
// session it had already read said it holds no fleet:operate. Mutation: decide
// the section on the viewer alone and the skeleton never goes. AND IT NAMES
// THE READER FROM THAT SAME READING: named off the viewer, which has not
// answered, it told a person signed in that nobody was — the moment between
// the session's answer and the socket's on every cold load. Mutation: hand
// the refusal the viewer's login and it says "Nobody is signed in".
test("a section the session's grants do not open is refused though the viewer never answers", async () => {
  location.hash = "#/settings/nodes";
  await mount(null);
  expect(await screen.findByText("Nodes needs fleet:operate")).toBeDefined();
  expect(screen.queryByText("Checking your access to Nodes")).toBeNull();
  const main = screen.getByRole("main");
  expect(main.textContent).toMatch(/You are signed in as jane\.doe/);
  expect(main.textContent).not.toMatch(/Nobody is signed in/);
  expect(within(main).getByRole("button", { name: "Sign in as somebody else" })).toBeDefined();
});

// AND THE GRANT IS NOTICED: the tab kept refusing after an administrator gave
// it, contradicting itself ("you hold state:read. The engine says: the live
// socket needs state:read") until a reload. The session is asked again — here
// as the tab comes back — and the answer holding the grant dials. Mutation:
// stop asking again, or dial only once per frame, and the dial never comes.
test("a grant made meanwhile dials the socket and the panel goes", async () => {
  grants = [];
  const { dial } = await mount(null);
  await panelTitled("You have no access to the company yet");
  grants = ["state:read"];
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await waitFor(() => expect(dial).toHaveBeenCalled());
  await waitFor(() =>
    expect(screen.queryByText("You have no access to the company yet")).toBeNull(),
  );
});

// A GRANT WITHDRAWN UNDER AN OPEN SOCKET closes it, and the session the frame
// read before still held the grant: the panel said "you hold state:read"
// beside the engine's "the live socket needs state:read". The refusal asks the
// session again. Mutation: drop that and the panel draws the stale grants.
test("a refusal for want of state:read asks who this is again, and says no access", async () => {
  const { store, dial } = await mount(null);
  await waitFor(() => expect(dial).toHaveBeenCalled());
  grants = [];
  act(() => store.setAccessRefused("the live socket needs state:read"));
  const panel = await panelTitled("You have no access to the company yet");
  expect(within(panel).getByText(/you hold no grants yet/)).toBeDefined();
  expect(within(panel).queryByText(/you hold state:read/)).toBeNull();
});

// NO NEW TASK IS OFFERED IN A FRAME THE ENGINE SERVES NO COMPANY. The
// sidebar's head `+` was pressable for a person invited with no grants and
// opened the whole New task sheet, its project stuck on "Reading the
// projects…" over a footer saying "Offline. Reconnect to make changes" to
// somebody online and refused. The head carries no `+` now, and every other
// door is on a screen the panel replaces. The CONTROL is the same frame for a
// session holding state:read, whose Tasks lens offers New task in its page
// bar. Mutation: draw the screen behind the panel and its New task is found.
test("a session without state:read is offered no New task, and is told why", async () => {
  location.hash = "#/work/tasks";
  grants = [];
  const { store } = await mount(null);
  await panelTitled("You have no access to the company yet");
  expect(screen.queryAllByRole("button", { name: /^New task/ })).toEqual([]);
  // AND THE REFUSAL IS THE ONE EVERY SURFACE READS, never "offline".
  expect(store.state.accessRefused).not.toBeNull();
  expect(screen.queryByText(/Offline\. Reconnect/)).toBeNull();
});

test("a session holding state:read is offered New task", async () => {
  location.hash = "#/work/tasks";
  await mount(null);
  const press = await screen.findByRole("button", { name: /^New task$/ });
  expect(press.getAttribute("aria-disabled")).toBeNull();
});

// A GRANT WITHDRAWN DROPS WHAT THE SOCKET SENT: the tab went on naming the
// company and listing its agents from the snapshot it held, while a session
// that never held the grant named none of it — two answers to one state.
// Mutation: keep the snapshot on a refusal and "Nimbus" stays.
test("a withdrawn grant keeps nothing the socket sent, as a session that never held it", async () => {
  const { store, dial } = await mount(null);
  await waitFor(() => expect(dial).toHaveBeenCalled());
  act(() => store.applyOrg({ name: "Nimbus" } as never));
  expect(await screen.findByText("Nimbus")).toBeDefined();
  grants = [];
  act(() => store.setAccessRefused("grant withdrawn: state:read"));
  await panelTitled("You have no access to the company yet");
  expect(screen.queryByText("Nimbus")).toBeNull();
  expect(store.state.org).toBeNull();
});
