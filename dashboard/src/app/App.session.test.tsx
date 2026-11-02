/**
 * A browser the engine does not accept is sent to sign in — from wherever it
 * learns that, back to wherever it was.
 *
 * This replaced a token dialog raised by the socket's refusal. What it has to
 * get right is invisible in any one screen: a `401` from ANY transport moves
 * the reader, the sign-in screens themselves are never moved (they answer
 * their own refusals), and the move REPLACES the entry, so Back after signing
 * in does not land on the screen that could not be drawn.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, test, vi } from "vitest";
import { CHUNKS, loadChunk } from "./lazyScreen.ts";
import { App } from "./App.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import {
  currentSessionNeed,
  LiveSocket,
  Store,
  needSession,
  sessionRestored,
} from "~/protocol/index.ts";
import { settled } from "~/test/settle.ts";

// EVERY SCREEN'S CODE IS IN BEFORE A CASE STARTS — the sign-in screens' chunk
// and every workspace's (`app/lazyScreen.ts`) — because this suite asserts
// where a reader is sent and what is drawn there, not how long a cold
// `import()` takes under a test transformer.
beforeAll(async () => {
  await Promise.all([...CHUNKS, "signin" as const].map((chunk) => loadChunk(chunk)));
});

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/**
 * The application at `hash`, RENDERED UNDER AN AWAITED ACT: a screen reads
 * its chunk through `use` (`app/lazyScreen.ts`), and a render under a
 * synchronous act that leaves React work still queued draws its warning that
 * a component "suspended inside an `act` scope" — a line the harness causes,
 * not the page, which the console gate (`~/test/console.ts`) fails the case
 * for.
 */
async function mount(hash: string) {
  location.hash = hash;
  const store = new Store();
  await act(async () => {
    render(
      <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
  });
}

const loginFor = (from: string) => `#/login?next=${encodeURIComponent(from)}`;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify({ error: "invalid_token" }), { status: 401 })),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionRestored();
  location.hash = "#/";
});

// A PAGE THAT NEEDS NO SESSION ASKS NOTHING THAT DOES. The socket was dialled
// at boot, so the sign-in, an invitation's and a reset link's page each sent a
// refused handshake, a refused probe of it and a refused degraded-mode
// snapshot — console errors on a page that needs nobody. The frame dials it
// now, and only once `GET /auth/session` has said somebody is signed in: a
// browser that opened `/dashboard` signed out still sent all three on its way
// to the sign-in. WHATEVER THE SESSION HOLDS IT IS DIALLED — the socket serves
// every principal the overview — so a person holding no app dials as one
// holding Admin does. Mutation: dial at boot again, from the frame before the
// session has answered, or only for a session holding some access, and one
// side goes red.
describe("the socket", () => {
  test.each([
    ["the sign-in", "#/login", [], 0],
    ["an invitation's page", "#/invite/abc.def", [], 0],
    ["a reset link's page", "#/reset/abc.def", [], 0],
    ["a screen in the frame, nobody signed in", "#/inbox", null, 0],
    ["a screen in the frame, signed in holding no app", "#/inbox", [], 1],
    ["a screen in the frame, signed in holding one app", "#/inbox", ["doclet"], 1],
    ["a screen in the frame, signed in holding Admin", "#/inbox", ["admin"], 1],
  ])("%s, at %s, holding %j, dials %d times", async (_, hash, grants, dials) => {
    const dialled: string[] = [];
    Object.defineProperty(globalThis, "WebSocket", {
      writable: true,
      value: class extends InertWebSocket {
        constructor(url: string) {
          super();
          dialled.push(url);
        }
      },
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const path = new URL(String(input), "http://engine.test").pathname;
        if (grants !== null && path === "/auth/session") {
          return new Response(
            JSON.stringify({
              person: "p-1",
              login: "jane.doe",
              kind: "person",
              grants,
              status: "signed_in",
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          );
        }
        return new Response(JSON.stringify({ error: "invalid_token" }), { status: 401 });
      }),
    );
    await mount(hash);
    // EVERY REQUEST THE FRAME ASKED HAS LANDED, and what each caused: a dial
    // follows the session's answer, so a frame that dialled for nobody does
    // it then, and a count taken before could not see it.
    await settled(() => vi.mocked(fetch).mock.calls.length);
    expect(dialled).toHaveLength(dials);
    const asked = vi.mocked(fetch).mock.calls.map(([url]) => String(url));
    expect(asked.filter((url) => /\/(ws|stream)\//.test(url))).toHaveLength(0);
  });

  // ONE REFUSED REQUEST ON THE WAY TO THE SIGN-IN, AND NOTHING OF OURS IN THE
  // CONSOLE. A signed-out load of `/dashboard` asked `GET /auth/session` three
  // times: the frame before dialling and the sidebar's user block at once,
  // then the sign-in it was routed to — three 401s in the console. The frame
  // asks once for all of it, and a sign-in the app routed here on a `401`
  // already knows the answer. That one `401` STAYS (dashboard-design.md,
  // "Signing in is a screen outside the frame"): the cookie is `HttpOnly`, so
  // only the engine can say whether this browser is signed in, and its honest
  // answer for nobody is the `401` every transport routes on. A browser logs
  // every 4xx a page loads and no page can stop it — so what is ours is that
  // it is the ONLY refused request (`/health` is unguarded and answers), and
  // that the dashboard adds no console line of its own. The CONTROL is
  // `#/login` opened directly, which asks once itself. Mutation: let the user
  // block, the Account or the sign-in ask again and the count is two or
  // three; log the expected refusal and the console is not empty.
  test.each([
    ["the frame, routed on to the sign-in", "#/"],
    ["the reader's Account, routed on to the sign-in", "#/account"],
    ["the sign-in, opened directly (the control)", "#/login"],
  ])("a signed-out load of %s is refused once, and logs nothing", async (_, hash) => {
    const answered: { path: string; status: number }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const path = new URL(String(input), "http://engine.test").pathname;
        const status = path === "/health" ? 200 : 401;
        answered.push({ path, status });
        const body =
          status === 200 ? { status: "ok", identity: "ready" } : { error: "invalid_token" };
        return new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        });
      }),
    );
    const consoleLines = [vi.spyOn(console, "error"), vi.spyOn(console, "warn")];
    try {
      await mount(hash);
      await waitFor(() => expect(location.hash.startsWith("#/login")).toBe(true));
      await screen.findByRole("heading", { name: "Sign in to Crewlet" });
      // Every request asked has landed, so a second ask would be counted.
      await settled(() => answered.length);
      expect(answered.filter((a) => a.status !== 200).map((a) => a.path)).toEqual([
        "/auth/session",
      ]);
      for (const line of consoleLines) expect(line).not.toHaveBeenCalled();
    } finally {
      for (const line of consoleLines) line.mockRestore();
    }
  });
});

describe("a session the engine does not accept", () => {
  test("sends the reader to sign in, carrying where they were", async () => {
    await mount("#/worklet?view=board");
    const depth = history.length;
    act(() => needSession("sign_in"));
    await waitFor(() => expect(location.hash).toBe(loginFor("#/worklet?view=board")));
    expect(await screen.findByRole("heading", { name: "Sign in to Crewlet" })).toBeDefined();
    // A REPLACE: the screen the reader was on cannot be drawn for them.
    expect(history.length).toBe(depth);
  });

  // THE CONTROL: a sign-in form routed to itself would lose what was typed,
  // and an invitation routed to sign-in would lose the link.
  test("leaves the sign-in screens where they are", async () => {
    const at = loginFor("#/settings/spend");
    await mount(at);
    act(() => needSession("sign_in"));
    await screen.findByRole("heading", { name: "Sign in to Crewlet" });
    expect(location.hash).toBe(at);

    cleanup();
    await mount("#/invite/abc");
    act(() => needSession("sign_in"));
    expect(location.hash).toBe("#/invite/abc");
  });

  test("a need recorded before anything mounted is followed when it does", async () => {
    // A REFUSAL CAN ANSWER BEFORE THE ROUTE'S FOLLOWER HAS MOUNTED.
    needSession("sign_in");
    await mount("#/inbox");
    await waitFor(() => expect(location.hash).toBe(loginFor("#/inbox")));
  });

  test("a session that may only enrol goes to the enrolment instead", async () => {
    // AS THE ENGINE ANSWERS SUCH A SESSION: `403
    // second_factor_enrolment_required` to every request but the enrolment's,
    // not the `401` of a browser signed in as nobody, which answering the
    // screen's own request under the awaited mount would send it to sign in
    // before the case says anything.
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: "second_factor_enrolment_required" }), {
            status: 403,
          }),
      ),
    );
    await mount("#/settings/nodes");
    act(() => needSession("second_factor"));
    await waitFor(() =>
      expect(location.hash).toBe(`#/enrol?next=${encodeURIComponent("#/settings/nodes")}`),
    );
  });

  test("an enrolment that loses its session signs in toward where it was going", async () => {
    await mount(`#/enrol?next=${encodeURIComponent("#/settings/spend")}`);
    act(() => needSession("sign_in"));
    await waitFor(() => expect(location.hash).toBe(loginFor("#/settings/spend")));
  });

  // END TO END THROUGH A SCREEN'S OWN REQUEST: no screen recognises a lost
  // session itself, so the one path that matters is a REST 401 reaching the
  // router without the screen saying a word.
  test("a REST 401 on any screen is enough", async () => {
    await mount("#/settings/secrets");
    await waitFor(() => expect(location.hash).toBe(loginFor("#/settings/secrets")));
  });
});

// A SIGN-IN'S OWN ANSWER IS THE NEWEST FACT ABOUT THE SESSION. The browser
// signing in holds nothing, so a transport has already recorded `sign_in` —
// and a sign-in answering with a session that may only enrol went to the
// enrolment and was routed straight back to this form by that stale need,
// with no word said, on every first sign-in of a deployment requiring a
// second factor.
describe("a sign-in answered with a session that may only enrol", () => {
  test("lands on the enrolment and stays there", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input), "http://engine.test");
        const method = (init?.method ?? "GET").toUpperCase();
        const json = (body: unknown, status = 200) =>
          new Response(JSON.stringify(body), {
            status,
            headers: { "Content-Type": "application/json" },
          });
        if (method === "GET" && url.pathname === "/health") return json({ identity: "ready" });
        if (method === "POST" && url.pathname === "/auth/login") {
          return json({
            person: "p-1",
            login: "jane.doe",
            expires_at: "2026-10-05T00:00:00Z",
            position: "1:9",
            status: "second_factor_enrolment_required",
          });
        }
        if (method === "POST" && url.pathname === "/auth/totp") {
          return json({ secret: "JBSWY3DPEHPK3PXP", uri: "otpauth://x" });
        }
        return json({ error: "invalid_token" }, 401);
      }),
    );
    needSession("sign_in");
    await mount(loginFor("#/worklet/ENG-42"));
    fireEvent.change(await screen.findByLabelText(/login or email/i), {
      target: { value: "jane.doe" },
    });
    fireEvent.change(screen.getByLabelText(/^password$/i), {
      target: { value: "correct horse battery staple" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    const enrol = `#/enrol?next=${encodeURIComponent("#/worklet/ENG-42")}`;
    await screen.findByText("Set up two-step verification");
    // EVERY REQUEST THE ENROLMENT ASKED HAS LANDED: a refusal recording
    // `sign_in` is what routed it back to the form.
    await settled(() => vi.mocked(fetch).mock.calls.length);
    expect(location.hash).toBe(enrol);
    expect(currentSessionNeed()).toBe("second_factor");
  });
});
