/**
 * `#/invite/<id>.<secret>`: the link the engine mints lands on a screen that
 * joins the person, and the secret never reaches a URL on the way.
 *
 * Every link `crewlet iam invite` and `POST /iam/invitations` print points
 * here; before this screen existed it landed on Not Found, and nobody could
 * join from a browser at all.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, test, vi } from "vitest";
import { CHUNKS, loadChunk } from "~/app/lazyScreen.ts";
import { App } from "~/app/App.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, needSession, Store, sessionRestored } from "~/protocol/index.ts";
import { parseLink } from "./link.ts";

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
  url: string;
  headers: Record<string, string>;
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
        url: String(input),
        headers: (init?.headers ?? {}) as Record<string, string>,
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

const ID = "0192e7a0-1c2b-7d3e-8f40-5a6b7c8d9e0f";
const SECRET = "c2VjcmV0LXRoYXQtaXMtdGhlLWxpbmtz";
const PATH = `/auth/invite/${ID}`;

const VIEW: Answer = {
  status: 200,
  body: {
    email: "jane.doe@example.com",
    invited_by: "ana.founder",
    login: "jane.doe",
    min_password_length: 12,
    seat: { handle: "jane", name: "Jane Doe" },
  },
};

const SPENT: Answer = {
  status: 410,
  body: {
    error: "invite_spent",
    message: "This invitation is no longer valid. Ask whoever sent it for a new one.",
  },
};

function mount(link = `${ID}.${SECRET}`) {
  location.hash = `#/invite/${link}`;
  const store = new Store();
  const socket = new LiveSocket(store);
  // THE FRAME'S DIAL (`start`), for the session it reads once the redemption
  // lands; `reconnect` is nothing a redemption may call.
  const dial = vi.spyOn(socket, "start");
  const reconnect = vi.spyOn(socket, "reconnect");
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
  return { dial, reconnect };
}

function type(label: RegExp | string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

function alerts(): string[] {
  return screen
    .queryAllByRole("alert")
    .map((el) => el.textContent ?? "")
    .filter((text) => text !== "");
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

describe("reading the link", () => {
  test("the two halves are split at the dot that joins them", () => {
    expect(parseLink(`${ID}.${SECRET}`)).toEqual({ id: ID, secret: SECRET });
    // THE CONTROL: a link a mail client cut short is not a link.
    expect(parseLink(ID)).toBeNull();
    expect(parseLink(`${ID}.`)).toBeNull();
    expect(parseLink(`.${SECRET}`)).toBeNull();
  });

  test("the view is asked with the secret beside the id, and never in a URL", async () => {
    const sent = engine({ [`GET ${PATH}`]: VIEW });
    mount();
    await screen.findByLabelText("Login");

    const view = sent.find((s) => s.url.includes("/auth/invite/"));
    expect(view?.method).toBe("GET");
    expect(view?.headers["X-Crewlet-Invite-Secret"]).toBe(SECRET);
    for (const request of sent) expect(request.url).not.toContain(SECRET);
  });

  test("a link cut short says so, and asks the engine nothing", () => {
    const sent = engine({});
    mount(ID);
    expect(screen.getByText("Part of the link is missing.")).toBeDefined();
    expect(sent.filter((s) => s.url.includes("/auth/invite/"))).toEqual([]);
  });
});

describe("what the screen shows before anything is spent", () => {
  test("who it is for, who sent it, the seat it binds, and the login it proposes", async () => {
    engine({ [`GET ${PATH}`]: VIEW });
    mount();
    expect((await screen.findByLabelText("Login")) as HTMLInputElement).toHaveProperty(
      "value",
      "jane.doe",
    );
    expect(screen.getByText("jane.doe@example.com")).toBeDefined();
    expect(screen.getByText("ana.founder")).toBeDefined();
    expect(screen.getByText("Jane Doe")).toBeDefined();
    expect(screen.getByText(/At least 12 characters/)).toBeDefined();
  });

  // REDEEMING ENDS THE SESSION THIS BROWSER HOLDS, so the form says whose it
  // is before Join is pressed. The CONTROL is the view above, which names no
  // session and draws no notice.
  test("a browser already signed in as somebody else is told joining signs them out", async () => {
    engine({
      [`GET ${PATH}`]: {
        status: 200,
        body: { ...(VIEW.body as object), signed_in_as: "dave.lee" },
      },
    });
    mount();
    await screen.findByLabelText("Login");
    expect(screen.getByText("dave.lee")).toBeDefined();
    expect(screen.getByText(/Joining signs that session out/)).toBeDefined();
  });

  // EVERY INVITATION NAMES A SEAT, and a redemption is refused when the chart
  // no longer holds it as a human seat — which the view says with a seat that
  // has no handle. Said before the form: a warning after a password is typed
  // is a wasted form. A view with no seat at all is not one the engine sends
  // (a link naming none is the dead link's 410), and it is warned the same
  // rather than offered as a seat. The CONTROL is the view above, which names
  // a seat and draws no warning. Mutation: draw the warning only for a seat
  // with no handle, and a view carrying none promises nothing and warns
  // nothing.
  test.each([
    ["names a seat the chart no longer holds", { seat: {} }],
    ["names no seat at all", { seat: undefined }],
  ])("a link that %s warns before any password that redeeming is refused", async (_, seat) => {
    engine({ [`GET ${PATH}`]: { status: 200, body: { ...(VIEW.body as object), ...seat } } });
    mount();
    const login = await screen.findByLabelText("Login");
    const warning = screen.getByText(/so redeeming it will be refused/);
    expect(warning.compareDocumentPosition(login) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.queryByText(/You will hold the seat/)).toBeNull();
  });

  test("a link naming a seat says the person will hold it, and warns nothing", async () => {
    engine({ [`GET ${PATH}`]: VIEW });
    mount();
    await screen.findByLabelText("Login");
    expect(screen.getByText(/You will hold the seat/)).toBeDefined();
    expect(screen.queryByText(/so redeeming it will be refused/)).toBeNull();
  });

  test("a browser signed in as nobody is told nothing about a session", async () => {
    engine({ [`GET ${PATH}`]: VIEW });
    mount();
    await screen.findByLabelText("Login");
    expect(screen.queryByText(/Joining signs that session out/)).toBeNull();
  });

  // ONE ANSWER for redeemed, withdrawn, expired and a wrong secret — in the
  // engine's own words, and with no form to fill in for nothing.
  test("a link that no longer works is one answer, with nothing to fill in", async () => {
    engine({ [`GET ${PATH}`]: SPENT });
    mount();
    expect(
      await screen.findByText(
        "This invitation is no longer valid. Ask whoever sent it for a new one.",
      ),
    ).toBeDefined();
    expect(screen.queryByLabelText("Password")).toBeNull();
  });

  // NOTHING WAS DECIDED about a link the engine could not read, so the screen
  // must not tell somebody to go and ask for a new one.
  test("an engine that could not read it says so, and offers to ask again", async () => {
    engine({
      [`GET ${PATH}`]: [
        {
          status: 503,
          body: { error: "identity_unavailable", message: "This node cannot tell who you are." },
        },
        VIEW,
      ],
    });
    mount();
    const again = await screen.findByRole("button", { name: "Try again" });
    expect(alerts().join(" ")).toContain("it has not been used");
    fireEvent.click(again);
    expect(await screen.findByLabelText("Login")).toBeDefined();
  });
});

describe("redeeming it", () => {
  test("a password under the floor is refused here, and nothing is posted", async () => {
    const sent = engine({ [`GET ${PATH}`]: VIEW });
    mount();
    await screen.findByLabelText("Login");
    type("Password", "too-short");
    fireEvent.click(screen.getByRole("button", { name: "Join" }));

    expect(await screen.findByText(/is 9 characters, and the minimum is 12/)).toBeDefined();
    expect(sent.filter((s) => s.method === "POST")).toEqual([]);
  });

  // THE ENGINE'S GRAMMAR, said under the field before anything is posted:
  // somebody who types their own name used to get the domain's refusal at the
  // foot of the form. The CONTROL is the redemption below, whose proposed
  // login fits and is posted.
  test("a login outside the grammar is refused under the field, and nothing is posted", async () => {
    const sent = engine({ [`GET ${PATH}`]: VIEW });
    mount();
    await screen.findByLabelText("Login");
    type("Login", "Frank");
    type("Password", "correct horse battery staple");
    type("Confirm password", "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Join" }));

    expect(await screen.findByText(/Use lowercase words joined by dots/)).toBeDefined();
    expect(sent.filter((s) => s.method === "POST")).toEqual([]);
  });

  // TYPED TWICE, because a slip in the only copy is an account its owner
  // cannot sign in to. The CONTROL is the next case, the same password typed
  // the same way twice, which is posted.
  test("a password typed differently the second time is refused here, and nothing is posted", async () => {
    const sent = engine({ [`GET ${PATH}`]: VIEW });
    mount();
    await screen.findByLabelText("Login");
    type("Password", "correct horse battery staple");
    type("Confirm password", "correct horse battery stable");
    fireEvent.click(screen.getByRole("button", { name: "Join" }));

    expect(await screen.findByText("The two passwords are not the same.")).toBeDefined();
    expect(sent.filter((s) => s.method === "POST")).toEqual([]);
  });

  test("it posts the secret in the body and lands on the Inbox, signed in", async () => {
    const sent = engine({
      [`GET ${PATH}`]: VIEW,
      [`POST ${PATH}`]: {
        status: 200,
        body: {
          person: "p-1",
          login: "jane.doe",
          seat: "jane",
          expires_at: "2026-10-05T00:00:00Z",
          position: "1:9",
          status: "signed_in",
        },
      },
      "GET /auth/session": {
        status: 200,
        body: {
          person: "p-1",
          login: "jane.doe",
          kind: "person",
          grants: ["state:read"],
          status: "signed_in",
        },
      },
    });
    const { dial, reconnect } = mount();
    await screen.findByLabelText("Login");
    type(/your name/i, "Jane Doe");
    type("Password", "correct horse battery staple");
    type("Confirm password", "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Join" }));

    await waitFor(() => expect(location.hash).toBe("#/"));
    // THE FRAME DIALS, for the session it read; the redemption dials nothing.
    await waitFor(() => expect(dial).toHaveBeenCalled());
    expect(reconnect).not.toHaveBeenCalled();
    const redeem = sent.find((s) => s.method === "POST");
    expect(redeem?.body).toEqual({
      secret: SECRET,
      login: "jane.doe",
      name: "Jane Doe",
      password: "correct horse battery staple",
    });
    expect(redeem?.url).not.toContain(SECRET);
  });

  test("a login somebody holds is said in the engine's words, and the form is kept", async () => {
    engine({
      [`GET ${PATH}`]: VIEW,
      [`POST ${PATH}`]: {
        status: 409,
        body: { error: "bad_params", detail: "that login is already taken — choose another" },
      },
    });
    mount();
    await screen.findByLabelText("Login");
    type("Password", "correct horse battery staple");
    type("Confirm password", "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Join" }));

    await waitFor(() =>
      expect(alerts()).toContain("That login is already taken — choose another."),
    );
    expect((screen.getByLabelText("Password") as HTMLInputElement).value).toBe(
      "correct horse battery staple",
    );
  });

  test("a link spent between the view and the post becomes the one answer", async () => {
    engine({ [`GET ${PATH}`]: VIEW, [`POST ${PATH}`]: SPENT });
    mount();
    await screen.findByLabelText("Login");
    type("Password", "correct horse battery staple");
    type("Confirm password", "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Join" }));
    expect(
      await screen.findByText(
        "This invitation is no longer valid. Ask whoever sent it for a new one.",
      ),
    ).toBeDefined();
  });

  test("a deployment that requires a second factor goes on to enrol it", async () => {
    engine({
      [`GET ${PATH}`]: VIEW,
      [`POST ${PATH}`]: {
        status: 200,
        body: {
          person: "p-1",
          login: "jane.doe",
          expires_at: "2026-10-05T00:00:00Z",
          position: "1:9",
          status: "second_factor_enrolment_required",
        },
      },
      "POST /auth/totp": { status: 200, body: { secret: "JBSWY3DPEHPK3PXP", uri: "otpauth://x" } },
    });
    const { dial, reconnect } = mount();
    await screen.findByLabelText("Login");
    // THE REAL BROWSER'S STATE: the socket's refusal probe records that
    // nobody is signed in on every load of this screen. This suite's socket
    // never dials, so the need is set as the probe would set it — and with it
    // in place the enrolment was sent straight back to the sign-in form.
    needSession("sign_in");
    type("Password", "correct horse battery staple");
    type("Confirm password", "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Join" }));

    const enrol = `#/enrol?next=${encodeURIComponent("#/")}`;
    await waitFor(() => expect(location.hash).toBe(enrol));
    // AND IT STAYS THERE once the frame has followed the need.
    await screen.findByText("Set up two-step verification");
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(location.hash).toBe(enrol);
    expect(dial).not.toHaveBeenCalled();
    expect(reconnect).not.toHaveBeenCalled();
  });
});
