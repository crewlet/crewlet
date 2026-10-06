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
import { afterEach, beforeEach, expect, test, vi } from "vitest";
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
 * is fetched first, so the screen draws rather than suspending inside the
 * render. `refused` is the engine's refusal of the dial the frame makes for a
 * session holding `state:read`, landing once it has made it.
 */
async function mount(refused: string | null = "your seat is no longer in the chart") {
  await loadChunk("work");
  await loadChunk("account");
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => new Promise(() => {});
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

// THE `+` IS HELD IN A FRAME THE ENGINE SERVES NO COMPANY, saying why. It was
// pressable for a person invited with no grants and opened the whole New task
// sheet, its project stuck on "Reading the projects…" over a footer saying
// "Offline — reconnect to make changes" to somebody online and refused. The
// CONTROL is the same frame for a session holding state:read, whose `+` is
// offered. Mutation: drop the hold and the sheet opens.
test("a session without state:read is offered no New task, and is told why", async () => {
  grants = [];
  const { store } = await mount(null);
  await panelTitled("You have no access to the company yet");
  const plus = sidebarPlus();
  expect(plus.getAttribute("aria-disabled")).toBe("true");
  expect(plus.getAttribute("title")).toMatch(/not serving you the company/);
  fireEvent.click(plus);
  await act(async () => {});
  expect(screen.queryByRole("dialog", { name: /New task/ })).toBeNull();
  // AND THE REFUSAL IS THE ONE EVERY SURFACE READS, never "offline".
  expect(store.state.accessRefused).not.toBeNull();
  expect(screen.queryByText(/Offline — reconnect/)).toBeNull();
});

test("a session holding state:read is offered New task", async () => {
  await mount(null);
  await waitFor(() => expect(sidebarPlus()).toBeDefined());
  expect(sidebarPlus().getAttribute("aria-disabled")).toBeNull();
});

/** The sidebar head's own `+`, beside the company's name. */
function sidebarPlus(): HTMLElement {
  const head = document.querySelector(".side-head") as HTMLElement;
  return within(head).getByRole("button", { name: "New task" });
}

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
