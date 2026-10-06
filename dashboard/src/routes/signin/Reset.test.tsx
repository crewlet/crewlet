/**
 * `#/reset/<id>.<secret>`: the link `POST /iam/people/{id}/password-reset` and
 * `crewlet iam reset-password` print lands on a screen that sets a password
 * once, keeps the secret out of every URL, and signs nobody in.
 *
 * Before this screen existed the link landed on Not Found, so a reset link
 * could not be spent from a browser at all.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, test, vi } from "vitest";
import { CHUNKS, loadChunk } from "~/app/lazyScreen.ts";
import { App } from "~/app/App.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, needSession, Store, sessionRestored } from "~/protocol/index.ts";

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

function engine(routes: Record<string, Answer>): Sent[] {
  const sent: Sent[] = [];
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
      const answer = routes[`${method} ${url.pathname}`];
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
const SECRET = "58e715a2e34b2898e76ec9b37a5b1b9a66449119434420927595e02c51c705fa";
const PATH = `/auth/reset/${ID}`;

const VIEW: Answer = {
  status: 200,
  body: { login: "jane.doe", expires_at: "2026-10-06T09:00:00Z", min_password_length: 12 },
};

const SPENT: Answer = {
  status: 410,
  body: {
    error: "reset_spent",
    message: "This password reset link is no longer valid. Ask an administrator for a new one.",
  },
};

function mount(link = `${ID}.${SECRET}`) {
  location.hash = `#/reset/${link}`;
  const store = new Store();
  const socket = new LiveSocket(store);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
}

function type(label: RegExp | string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

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

describe("opening the link", () => {
  test("says whose password it sets, asking with the secret beside the id", async () => {
    const sent = engine({ [`GET ${PATH}`]: VIEW });
    mount();
    await screen.findByLabelText("New password");
    expect(screen.getByText("jane.doe")).toBeDefined();
    const view = sent.find((s) => s.url.includes("/auth/reset/"));
    expect(view?.method).toBe("GET");
    expect(view?.headers["X-Crewlet-Reset-Secret"]).toBe(SECRET);
    for (const request of sent) expect(request.url).not.toContain(SECRET);
  });

  // A BROWSER HOLDING NO SESSION is the ordinary one here, and the socket's
  // refusal probe raises the sign-in need on every load: the screen stays.
  test("stays put when the transports say nobody is signed in", async () => {
    engine({ [`GET ${PATH}`]: VIEW });
    mount();
    await screen.findByLabelText("New password");
    needSession("sign_in");
    await screen.findByLabelText("New password");
    expect(location.hash).toBe(`#/reset/${ID}.${SECRET}`);
  });

  test("a link cut short says so, and asks the engine nothing", () => {
    const sent = engine({});
    mount(ID);
    expect(screen.getByText("Part of the link is missing.")).toBeDefined();
    expect(sent.filter((s) => s.url.includes("/auth/reset/"))).toEqual([]);
  });

  test("a link that no longer works is the engine's one answer, with no form", async () => {
    engine({ [`GET ${PATH}`]: SPENT });
    mount();
    expect(
      await screen.findByText(
        "This password reset link is no longer valid. Ask an administrator for a new one.",
      ),
    ).toBeDefined();
    expect(screen.queryByLabelText("New password")).toBeNull();
  });
});

describe("spending it", () => {
  test("a password under the floor is refused here, and nothing is posted", async () => {
    const sent = engine({ [`GET ${PATH}`]: VIEW });
    mount();
    await screen.findByLabelText("New password");
    type("New password", "too-short");
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    expect(await screen.findByText(/is 9 characters, and the minimum is 12/)).toBeDefined();
    expect(sent.filter((s) => s.method === "POST")).toEqual([]);
  });

  // TYPED TWICE, for the invitation's reason. The CONTROL is the next case,
  // which types it the same way twice and is posted.
  test("a password typed differently the second time is refused here, and nothing is posted", async () => {
    const sent = engine({ [`GET ${PATH}`]: VIEW });
    mount();
    await screen.findByLabelText("New password");
    type("New password", "correct horse battery staple");
    type("Confirm new password", "correct horse battery stable");
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    expect(await screen.findByText("The two passwords are not the same.")).toBeDefined();
    expect(sent.filter((s) => s.method === "POST")).toEqual([]);
  });

  // THE CONTROL for the sign-in it does not do: the answer carries no session,
  // so the screen sends the person to the sign-in form rather than in.
  test("it posts the secret in the body and sends the person to sign in", async () => {
    const sent = engine({
      [`GET ${PATH}`]: VIEW,
      [`POST ${PATH}`]: { status: 200, body: { status: "password_set", login: "jane.doe" } },
    });
    mount();
    await screen.findByLabelText("New password");
    type("New password", "correct horse battery staple");
    type("Confirm new password", "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));

    expect(await screen.findByText("Your password is set")).toBeDefined();
    const spend = sent.find((s) => s.method === "POST");
    expect(spend?.body).toEqual({ secret: SECRET, password: "correct horse battery staple" });
    expect(spend?.url).not.toContain(SECRET);
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(location.hash).toBe("#/login"));
  });

  test("a link spent between the view and the post becomes the one answer", async () => {
    engine({ [`GET ${PATH}`]: VIEW, [`POST ${PATH}`]: SPENT });
    mount();
    await screen.findByLabelText("New password");
    type("New password", "correct horse battery staple");
    type("Confirm new password", "correct horse battery staple");
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    expect(
      await screen.findByText(
        "This password reset link is no longer valid. Ask an administrator for a new one.",
      ),
    ).toBeDefined();
  });
});
