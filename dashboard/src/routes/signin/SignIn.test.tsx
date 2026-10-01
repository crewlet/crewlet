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

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { App } from "~/app/App.tsx";
import { Router } from "~/app/router.tsx";
import { currentReader, noteReader } from "~/lib/reader.ts";
import { recentsKey } from "~/lib/recents.ts";
import { page } from "~/lib/session.ts";
import { starsKey } from "~/lib/starred.ts";
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

/**
 * A fetch that answers each `METHOD /path` from `routes`, recording what was
 * sent.
 *
 * AND THE COOKIE A SIGN-IN SETS. Once a sign-in route answers with a session,
 * `GET /auth/session` answers that session, as the engine does for the cookie
 * the browser now holds. Answering "nobody" for ever, as the routes alone did,
 * sent every page the sign-in landed on straight back to the form — and the
 * cases still passed, because a poll caught the address on its way through.
 */
function engine(routes: Record<string, Answer | Answer[]>): Sent[] {
  const sent: Sent[] = [];
  const queues = new Map(
    Object.entries(routes).map(([k, v]) => [k, Array.isArray(v) ? [...v] : [v]] as const),
  );
  let cookie: { person?: string; login?: string; status?: string; expires_at?: string } | null =
    null;
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
      if (method === "GET" && url.pathname === "/auth/session" && cookie) {
        const login = cookie.login ?? "";
        return new Response(
          JSON.stringify({
            person: cookie.person ?? "",
            login,
            kind: login.startsWith("token:") ? "machine" : "person",
            status: cookie.status,
            expires_at: cookie.expires_at,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }
      const queue = queues.get(`${method} ${url.pathname}`);
      const answer = queue && (queue.length > 1 ? queue.shift()! : queue[0]!);
      if (!answer) return new Response(JSON.stringify({ error: "no_route" }), { status: 404 });
      const body = answer.body as { status?: unknown } | null;
      const signsIn =
        method === "POST" &&
        url.pathname.startsWith("/auth/") &&
        answer.status >= 200 &&
        answer.status < 300 &&
        typeof body?.status === "string";
      if (signsIn) cookie = body as typeof cookie;
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

/**
 * Lets the stubbed engine's answers land, and renders the page they leave.
 *
 * NOT A POLL, and the settled page rather than the first one that matched.
 * A `waitFor` gave every step a second of real time, re-reading the page at
 * every change, which a loaded machine could not always give the first
 * sign-in of a file; and it could pass on a hash the page only went THROUGH
 * — the enrolment case below was once green on exactly that. The engine's
 * answers are promises, so `act` runs them and the renders they cause to the
 * end, and what is read is where the page came to rest.
 */
async function answered(): Promise<void> {
  await act(async () => {});
}

/** The one refusal the form is showing, once it shows one. */
async function refusal(): Promise<string> {
  await answered();
  expect(alerts()).toHaveLength(1);
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
  sessionStorage.clear();
  vi.restoreAllMocks();
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

    await answered();

    expect(location.hash).toBe(NEXT);
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

    await answered();

    screen.getByLabelText(/^code$/i);
    expect(alerts()).toEqual([]);
    type(/^code$/i, "123456");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    await answered();

    expect(location.hash).toBe(NEXT);
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
    // passed while the enrolment was routed straight back to this form — a
    // `waitFor` caught the enrolment's hash on the way through.
    await answered();
    expect(currentSessionNeed()).toBe("sign_in");
    type(/login or email/i, "jane.doe");
    type(/^password$/i, "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    const enrol = `#/enrol?next=${encodeURIComponent(NEXT)}`;
    await answered();
    expect(location.hash).toBe(enrol);
    // AND IT STAYS THERE once the frame has followed the need it recorded.
    await answered();
    screen.getByText("Set up two-step verification");
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

    await answered();

    expect(location.hash).toBe(NEXT);
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
    await answered();
    screen.getByText(/signs nobody in with a password/i);
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
    await answered();
    const carry = screen.getByRole("button", { name: "Continue as jane.doe" });
    // NOT SENT ON UNASKED: "sign in as somebody else" is a thing people mean.
    expect(location.hash).toBe(`#/login?next=${encodeURIComponent(NEXT)}`);
    act(() => carry.click());
    await answered();
    expect(location.hash).toBe(NEXT);
    expect(reconnect).toHaveBeenCalled();
  });
});

// A SESSION ALSO ENDS WITH NOBODY SIGNING OUT — an idle deadline, a
// revocation, a sign-out in another tab — and the tab is then ROUTED here with
// everything its last reader saw still in it: the store's company state, and
// the builder's kept draft. Only a sign-out reloaded it, so whoever signed in
// next was served the last person's company, and their unsaved draft as
// their own.
describe("a sign-in in a tab somebody else was reading", () => {
  let reloads: ReturnType<typeof vi.spyOn>;
  beforeEach(() => {
    reloads = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
  });

  test("hands the tab over: its storage emptied, the new reader recorded, reloaded where it was going", async () => {
    engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/login": SIGNED_IN,
    });
    noteReader("p-9");
    sessionStorage.setItem("crewlet_org_draft", "{}");
    const { reconnect } = mount();
    type(/login or email/i, "jane.doe");
    type(/^password$/i, "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    await answered();

    expect(reloads).toHaveBeenCalledWith(NEXT);
    expect(sessionStorage.getItem("crewlet_org_draft")).toBeNull();
    expect(currentReader()).toBe("p-1");
    // THE RELOAD DIALS: a socket re-dialled in a page about to be dropped is
    // a connection opened for nothing.
    expect(reconnect).not.toHaveBeenCalled();
  });

  test("a session that may only enrol is handed over into the enrolment", async () => {
    engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/login": {
        status: 200,
        body: { ...(SIGNED_IN.body as object), status: "second_factor_enrolment_required" },
      },
    });
    noteReader("p-9");
    mount();
    type(/login or email/i, "jane.doe");
    type(/^password$/i, "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    await answered();
    expect(reloads).toHaveBeenCalledWith(`#/enrol?next=${encodeURIComponent(NEXT)}`);
  });

  // THE CONTROL: the same person back after their session lapsed carries on
  // where they were — the builder keeps their draft for exactly this.
  test("the same person signing in again keeps the tab and what it holds", async () => {
    engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/login": SIGNED_IN,
    });
    noteReader("p-1");
    sessionStorage.setItem("crewlet_org_draft", "{}");
    const { reconnect } = mount();
    type(/login or email/i, "jane.doe");
    type(/^password$/i, "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    await answered();

    expect(location.hash).toBe(NEXT);
    expect(reloads).not.toHaveBeenCalled();
    expect(reconnect).toHaveBeenCalled();
    expect(sessionStorage.getItem("crewlet_org_draft")).toBe("{}");
  });

  // THE BROWSER IS THE SIGNING-IN PERSON'S, and a list kept per reader in its
  // localStorage outlives every tab: a key of one's own kept the last person's
  // titles out of this person's rail and not out of their browser. So every
  // other reader's recents and stars go, and this person's own stay.
  test("every other reader's recents and stars leave the browser, and the person's own stay", async () => {
    engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/login": SIGNED_IN,
    });
    noteReader("p-9");
    for (const key of [recentsKey("p-9"), starsKey("p-9"), recentsKey("p-1"), starsKey("p-1")]) {
      localStorage.setItem(key, "[]");
    }
    localStorage.setItem("crewlet_rail_collapsed", "1");
    mount();
    type(/login or email/i, "jane.doe");
    type(/^password$/i, "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    await answered();

    expect(reloads).toHaveBeenCalledWith(NEXT);
    expect(localStorage.getItem(recentsKey("p-9"))).toBeNull();
    expect(localStorage.getItem(starsKey("p-9"))).toBeNull();
    expect(localStorage.getItem(recentsKey("p-1"))).toBe("[]");
    expect(localStorage.getItem(starsKey("p-1"))).toBe("[]");
    // A preference of the browser's own is nobody's list.
    expect(localStorage.getItem("crewlet_rail_collapsed")).toBe("1");
  });

  // A TAB NOBODY WAS READING has nothing of anybody's in it to hand over.
  test("a first sign-in in a fresh tab records its reader and carries on", async () => {
    engine({
      "GET /auth/config": LOCAL,
      "GET /auth/session": NOBODY,
      "POST /auth/login": SIGNED_IN,
    });
    // ANOTHER TAB'S READER, whose session ended with no sign-out: the tab
    // that knew them is gone, and their lists are still in the browser.
    localStorage.setItem(recentsKey("p-9"), "[]");
    mount();
    type(/login or email/i, "jane.doe");
    type(/^password$/i, "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    await answered();

    expect(location.hash).toBe(NEXT);
    expect(reloads).not.toHaveBeenCalled();
    expect(currentReader()).toBe("p-1");
    expect(localStorage.getItem(recentsKey("p-9"))).toBeNull();
  });
});
