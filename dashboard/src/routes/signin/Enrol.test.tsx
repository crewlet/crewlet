/**
 * `#/enrol`: the second factor a deployment requires before anything else,
 * enrolled in the engine's two legs, with the first recovery codes shown the
 * once the engine answers them.
 *
 * Without this screen every password or invitation session on such a
 * deployment is refused every route but three, and the only way on was a
 * terminal.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "~/test/inCase.ts";
import { afterEach, beforeAll, beforeEach, describe, expect, test, vi } from "vitest";
import { CHUNKS, loadChunk } from "~/app/lazyScreen.ts";
import { App } from "~/app/App.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import {
  LiveSocket,
  Store,
  currentSessionNeed,
  needSession,
  sessionRestored,
} from "~/protocol/index.ts";
import { groupedKey } from "./SecondFactor.tsx";

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
  body: unknown;
}

type Answer = { status: number; body: unknown };

function engine(routes: Record<string, Answer | Answer[]>): Sent[] {
  const sent: Sent[] = [];
  const queues = new Map(
    Object.entries(routes).map(([k, v]) => [k, Array.isArray(v) ? [...v] : [v]] as const),
  );
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      const method = (init?.method ?? "GET").toUpperCase();
      sent.push({
        method,
        path: url.pathname,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      });
      const queue = queues.get(`${method} ${url.pathname}`);
      const answer = queue && (queue.length > 1 ? queue.shift()! : queue[0]!);
      if (!answer) return new Response(JSON.stringify({ error: "no_route" }), { status: 404 });
      return new Response(JSON.stringify(answer.body), {
        status: answer.status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

const SEED: Answer = {
  status: 200,
  body: {
    secret: "JBSWY3DPEHPK3PXPJBSWY3DP",
    uri: "otpauth://totp/crewlet.example.com:jane.doe?secret=JBSWY3DPEHPK3PXPJBSWY3DP",
  },
};
const ENROLLED: Answer = {
  status: 200,
  body: {
    status: "enrolled",
    session: {
      person: "p-1",
      login: "jane.doe",
      expires_at: "2026-10-05T00:00:00Z",
      position: "1:12",
      status: "signed_in",
    },
  },
};
const CODES = ["a1b2-c3d4", "e5f6-g7h8", "i9j0-k1l2"];
const NEXT = "#/admin/config";

function mount() {
  location.hash = `#/enrol?next=${encodeURIComponent(NEXT)}`;
  const store = new Store();
  const socket = new LiveSocket(store);
  const reconnect = vi.spyOn(socket, "reconnect");
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
  return { reconnect };
}

function alerts(): string[] {
  return screen
    .queryAllByRole("alert")
    .map((el) => el.textContent ?? "")
    .filter((text) => text !== "");
}

async function confirmCode(code: string) {
  fireEvent.change(await screen.findByLabelText(/six-digit code/i), { target: { value: code } });
  fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
}

// THE SIGN-IN SCREENS ARE A CHUNK OF THEIR OWN (`app/lazyScreen.ts`), and so is
// every workspace a finished sign-in lands on. This suite asserts what is drawn
// and where a reader is sent, not how long a cold `import()` takes under a
// test transformer — so every chunk is in before a case starts.
beforeAll(async () => {
  await Promise.all([...CHUNKS, "signin" as const].map((chunk) => loadChunk(chunk)));
});

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionRestored();
  location.hash = "#/";
});

describe("enrolling", () => {
  test("the key is shown to type and the link to open, and the code goes back with it", async () => {
    const sent = engine({
      "POST /auth/totp": [SEED, ENROLLED],
      "POST /auth/totp/recovery": {
        status: 200,
        body: { codes: CODES },
      },
    });
    mount();

    expect(await screen.findByText(groupedKey("JBSWY3DPEHPK3PXPJBSWY3DP"))).toBeDefined();
    expect(screen.getByRole("link", { name: "open it in the app" }).getAttribute("href")).toBe(
      (SEED.body as { uri: string }).uri,
    );
    // THE FIRST LEG STORES NOTHING and sends nothing but the ask.
    expect(sent[0]).toEqual({ method: "POST", path: "/auth/totp", body: {} });

    await confirmCode("123456");
    await screen.findByText(CODES[0]!);
    expect(sent[1]).toEqual({
      method: "POST",
      path: "/auth/totp",
      body: { secret: "JBSWY3DPEHPK3PXPJBSWY3DP", code: "123456" },
    });
  });

  test("the first recovery codes are issued unasked, shown once, and then the reader goes on", async () => {
    const sent = engine({
      "POST /auth/totp": [SEED, ENROLLED],
      "POST /auth/totp/recovery": { status: 200, body: { codes: CODES } },
    });
    const { reconnect } = mount();
    needSession("second_factor");
    await confirmCode("123456");

    for (const code of CODES) expect(await screen.findByText(code)).toBeDefined();
    expect(sent.filter((s) => s.path === "/auth/totp/recovery")).toHaveLength(1);

    fireEvent.click(screen.getByRole("button", { name: /I have saved them/ }));
    await waitFor(() => expect(location.hash).toBe(NEXT));
    // THE SOCKET WAS REFUSED under the restricted session and re-dials under
    // the whole one; and the need that sent the reader here is answered.
    expect(reconnect).toHaveBeenCalled();
    expect(currentSessionNeed()).toBeNull();
  });

  test("a code that does not match says what to check, and asks for another", async () => {
    engine({
      "POST /auth/totp": [
        SEED,
        {
          status: 400,
          body: {
            error: "invalid_body",
            detail: "that code does not match the secret",
            hint: "check the authenticator app has the right account, and that this device's clock is correct",
          },
        },
      ],
    });
    mount();
    await confirmCode("000000");

    await waitFor(() =>
      expect(alerts()).toContain(
        "That code does not match the secret. Check the authenticator app has the right account, and that this device's clock is correct.",
      ),
    );
    expect((screen.getByLabelText(/six-digit code/i) as HTMLInputElement).value).toBe("");
  });

  // A PASSWORD NEVER REPLACES A FACTOR: a session that could only enrol,
  // whose person has come to hold one since, is sent to sign in with it.
  test("a factor held since the session opened sends the reader to sign in with it", async () => {
    engine({
      "POST /auth/totp": [
        SEED,
        {
          status: 403,
          body: {
            error: "second_factor_required",
            detail: "this account holds a second factor now",
          },
        },
      ],
    });
    mount();
    await confirmCode("123456");
    fireEvent.click(await screen.findByRole("button", { name: "Sign in again" }));
    await waitFor(() => expect(location.hash).toBe(`#/login?next=${encodeURIComponent(NEXT)}`));
  });

  test("codes that cannot be issued are said, and the reader may go on without them", async () => {
    engine({
      "POST /auth/totp": [SEED, ENROLLED],
      "POST /auth/totp/recovery": {
        status: 503,
        body: { error: "unavailable", message: "This node cannot answer that right now." },
      },
    });
    mount();
    await confirmCode("123456");
    fireEvent.click(await screen.findByRole("button", { name: "Continue without recovery codes" }));
    await waitFor(() => expect(location.hash).toBe(NEXT));
  });
});
