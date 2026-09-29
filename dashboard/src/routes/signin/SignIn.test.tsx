/**
 * `#/login`: a person signs in as themselves, and the page keeps nothing.
 *
 * The invariants worth breaking a build over, in the order they cost when
 * they go: a sign-in lands where the reader was, REPLACING the sign-in's own
 * history entry, on a socket re-dialled with the new session; a refusal is
 * the engine's own sentence and never a branch of this screen's; the second
 * factor is asked for only when the engine asks; and a token handed to the
 * exchange is sent once, in the header, and kept nowhere.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { App } from "~/app/App.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { currentSessionNeed, LiveSocket, Store, sessionRestored } from "~/protocol/index.ts";

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
  headers: Record<string, string>;
  body: unknown;
}

type Answer = { status: number; body: unknown; headers?: Record<string, string> };

/** A fetch that answers each `METHOD /path` from `routes`, recording what was sent. */
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
        path: url.pathname + url.search,
        headers: (init?.headers ?? {}) as Record<string, string>,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      });
      const queue = queues.get(`${method} ${url.pathname}`);
      const answer = queue && (queue.length > 1 ? queue.shift()! : queue[0]!);
      if (!answer) return new Response(JSON.stringify({ error: "no_route" }), { status: 404 });
      return new Response(JSON.stringify(answer.body), {
        status: answer.status,
        headers: { "Content-Type": "application/json", ...answer.headers },
      });
    }),
  );
  return sent;
}

const NOBODY: Answer = { status: 401, body: { error: "invalid_token" } };
const LOCAL: Answer = { status: 200, body: { backend: "local" } };
const SIGNED_IN: Answer = {
  status: 200,
  body: {
    person: "p-1",
    login: "jane.doe",
    expires_at: "2026-10-05T00:00:00Z",
    position: "1:9",
    status: "signed_in",
  },
};

function mount() {
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

function type(label: RegExp | string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

/**
 * What the alerts on the page say. The toaster's two live regions are alerts
 * as well and stay empty, so only an alert with words in it is a refusal.
 */
function alerts(): string[] {
  return screen
    .queryAllByRole("alert")
    .map((el) => el.textContent ?? "")
    .filter((text) => text !== "");
}

/** The one refusal the form is showing, once it shows one. */
async function refusal(): Promise<string> {
  await waitFor(() => expect(alerts()).toHaveLength(1));
  return alerts()[0]!;
}

const NEXT = "#/work/ENG-42";

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  location.hash = `#/login?next=${encodeURIComponent(NEXT)}`;
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionRestored();
  localStorage.clear();
  location.hash = "#/";
});

describe("signing in with a password", () => {
  test("it lands where the reader was, replacing the sign-in, on a re-dialled socket", async () => {
    const sent = engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/login": SIGNED_IN,
    });
    const { reconnect } = mount();
    const depth = history.length;

    type(/login or email/i, "jane.doe");
    type(/^password$/i, "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    await waitFor(() => expect(location.hash).toBe(NEXT));
    expect(reconnect).toHaveBeenCalled();
    // A REPLACE, NOT A PUSH: Back from the page the reader asked for must
    // not land them on a form for a session they already hold.
    expect(history.length).toBe(depth);
    const login = sent.find((s) => s.method === "POST" && s.path === "/auth/login");
    expect(login?.body).toEqual({ login: "jane.doe", password: "correct horse battery staple" });
  });

  test("a refusal is the engine's own sentence, and the form is kept", async () => {
    engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/login": {
        status: 401,
        body: {
          error: "sign_in_refused",
          message: "Those sign-in details were not accepted. Check them and try again.",
        },
      },
    });
    mount();
    type(/login or email/i, "jane.doe");
    type(/^password$/i, "wrong horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    const alert = await refusal();
    expect(alert).toBe("Those sign-in details were not accepted. Check them and try again.");
    // STILL HERE: a mistyped password is not a lost session.
    expect(location.hash).toBe(`#/login?next=${encodeURIComponent(NEXT)}`);
    expect((screen.getByLabelText(/login or email/i) as HTMLInputElement).value).toBe("jane.doe");
  });

  test("the code is asked for only when the engine asks, and sent with the resubmission", async () => {
    const sent = engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/login": [{ status: 401, body: { error: "second_factor_required" } }, SIGNED_IN],
    });
    mount();
    // THE CONTROL: nothing asks for a code before the password has proved
    // itself, because only the engine knows whether this person holds one.
    expect(screen.queryByLabelText(/^code$/i)).toBeNull();

    type(/login or email/i, "jane.doe");
    type(/^password$/i, "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    await screen.findByLabelText(/^code$/i);
    expect(alerts()).toEqual([]);
    type(/^code$/i, "123456");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    await waitFor(() => expect(location.hash).toBe(NEXT));
    const posts = sent.filter((s) => s.method === "POST" && s.path === "/auth/login");
    expect(posts[1]?.body).toEqual({
      login: "jane.doe",
      password: "correct horse battery staple",
      code: "123456",
    });
  });

  test("a throttled attempt says how long, and the button waits it out", async () => {
    engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/login": {
        status: 429,
        body: { error: "throttled", message: "There have been too many failed attempts." },
        headers: { "Retry-After": "16" },
      },
    });
    mount();
    type(/login or email/i, "jane.doe");
    type(/^password$/i, "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    const alert = await refusal();
    expect(alert).toContain("Try again in 16 seconds.");
    expect((screen.getByRole("button", { name: "Sign in" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
  });

  test("a session that may only enrol goes on to the enrolment, carrying next", async () => {
    engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/login": {
        status: 200,
        body: { ...(SIGNED_IN.body as object), status: "second_factor_enrolment_required" },
      },
      "POST /auth/totp": { status: 200, body: { secret: "JBSWY3DPEHPK3PXP", uri: "otpauth://x" } },
    });
    const { reconnect } = mount();
    // FROM THE REAL BROWSER'S STATE: the screen's own session read answered
    // 401, which records that nobody is signed in. Without this the case
    // passed while the enrolment was routed straight back to this form — its
    // `waitFor` caught the enrolment's hash on the way through.
    await waitFor(() => expect(currentSessionNeed()).toBe("sign_in"));
    type(/login or email/i, "jane.doe");
    type(/^password$/i, "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    const enrol = `#/enrol?next=${encodeURIComponent(NEXT)}`;
    await waitFor(() => expect(location.hash).toBe(enrol));
    // AND IT STAYS THERE once the frame has followed the need it recorded.
    await screen.findByText("Set up two-step verification");
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(location.hash).toBe(enrol);
    expect(currentSessionNeed()).toBe("second_factor");
    // THE SOCKET WAITS: a session that may only enrol is refused it.
    expect(reconnect).not.toHaveBeenCalled();
  });
});

describe("signing in with the deployment's token", () => {
  test("the token is sent once, in the header, and kept nowhere", async () => {
    const sent = engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/token": {
        ...SIGNED_IN,
        body: { ...(SIGNED_IN.body as object), login: "token:ops" },
      },
    });
    const { reconnect } = mount();
    fireEvent.click(screen.getByRole("button", { name: /use an api token instead/i }));
    type("API token", "the-break-glass-token-value-long-enough");
    fireEvent.click(screen.getByRole("button", { name: "Sign in with the token" }));

    await waitFor(() => expect(location.hash).toBe(NEXT));
    expect(reconnect).toHaveBeenCalled();
    const exchange = sent.find((s) => s.path === "/auth/token");
    expect(exchange?.headers.Authorization).toBe("Bearer the-break-glass-token-value-long-enough");
    // NOWHERE: not in either storage area — the frame keeps its recents
    // there, so what is asserted is that no value holds the token — and not
    // in any URL this page asked for.
    for (const area of [localStorage, sessionStorage]) {
      for (let i = 0; i < area.length; i++) {
        expect(area.getItem(area.key(i) ?? "") ?? "").not.toContain("the-break-glass");
      }
    }
    expect(sent.some((s) => s.path.includes("the-break-glass"))).toBe(false);
  });

  test("a deployment with no people offers the token and no password form", async () => {
    engine({
      "GET /auth/config": { status: 200, body: { backend: "none" } },
      "GET /auth/session": NOBODY,
    });
    mount();
    await screen.findByText(/signs nobody in with a password/i);
    expect(screen.queryByLabelText(/^password$/i)).toBeNull();
    expect(screen.getByLabelText("API token")).toBeDefined();
  });
});

describe("a browser that is already signed in", () => {
  test("is told whose session it holds, and may carry on as them", async () => {
    engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": {
        status: 200,
        body: { person: "p-1", login: "jane.doe", kind: "person", status: "signed_in" },
      },
    });
    const { reconnect } = mount();
    const carry = await screen.findByRole("button", { name: "Continue as jane.doe" });
    // NOT SENT ON UNASKED: "sign in as somebody else" is a thing people mean.
    expect(location.hash).toBe(`#/login?next=${encodeURIComponent(NEXT)}`);
    act(() => carry.click());
    await waitFor(() => expect(location.hash).toBe(NEXT));
    expect(reconnect).toHaveBeenCalled();
  });
});
