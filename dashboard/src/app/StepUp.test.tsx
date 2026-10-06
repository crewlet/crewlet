/**
 * The one step-up ceremony, as a person meets it: a save refused for a
 * fresher proof asks for the password, and then the save goes through — the
 * same request, with nothing retyped.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { StepUpHost } from "./StepUp.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import {
  LiveSocket,
  RestError,
  Store,
  currentSessionNeed,
  rest,
  sessionRestored,
} from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

type Answer = { status: number; body: unknown };

interface Sent {
  path: string;
  body: unknown;
}

function engine(routes: Record<string, Answer[]>): Sent[] {
  const sent: Sent[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      sent.push({
        path: url.pathname,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      });
      const queue = routes[url.pathname] ?? [];
      const answer = queue.length > 1 ? queue.shift()! : queue[0];
      return new Response(JSON.stringify(answer?.body ?? { error: "no_route" }), {
        status: answer?.status ?? 404,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

const refusedFor = (window: string): Answer => ({
  status: 403,
  body: {
    error: "step_up_required",
    message: "This action needs you to have confirmed who you are recently.",
    reason: "step_up",
    window,
    grants: [],
  },
});

const STEPPED_UP: Answer = {
  status: 200,
  body: {
    person: "p-1",
    login: "jane.doe",
    expires_at: "2026-10-05T00:00:00Z",
    position: "1:30",
    status: "signed_in",
  },
};

/** What the save the test presses came to: its answer, or its refusal. */
let outcome: unknown;

function Saver() {
  return (
    <button
      type="button"
      onClick={() => {
        rest.put("/secrets/GITHUB_TOKEN", { value: "typed into a form" }).then(
          (answer) => (outcome = answer),
          (err: unknown) => (outcome = err),
        );
      }}
    >
      Save
    </button>
  );
}

function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  const reconnect = vi.spyOn(socket, "reconnect");
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <LayerHost>
          <StepUpHost />
          <Saver />
        </LayerHost>
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

async function refuseAndConfirm(password: string) {
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  const dialog = await screen.findByRole("dialog", { name: "Confirm it is you" });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: password } });
  fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
  return dialog;
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  outcome = undefined;
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionRestored();
});

describe("a gesture that needs a fresher proof", () => {
  test("asks for the password, and then the same request goes through", async () => {
    const sent = engine({
      "/secrets/GITHUB_TOKEN": [refusedFor("step_up"), { status: 200, body: { stored: true } }],
      "/auth/step-up": [STEPPED_UP],
    });
    const { reconnect } = mount();
    await refuseAndConfirm("correct horse battery staple");

    await waitFor(() => expect(outcome).toEqual({ stored: true }));
    expect(screen.queryByRole("dialog")).toBeNull();
    // AND THE DIALOG'S OWN READ of whether a code is held, beside them.
    const asked = sent.filter((s) => s.path !== "/iam/credentials");
    expect(asked.map((s) => s.path)).toEqual([
      "/secrets/GITHUB_TOKEN",
      "/auth/step-up",
      "/secrets/GITHUB_TOKEN",
    ]);
    expect(asked[1]?.body).toEqual({ password: "correct horse battery staple" });
    // THE REPLAY IS THE REQUEST THAT WAS REFUSED, which is what keeps a form.
    expect(asked[2]?.body).toEqual(asked[0]?.body);
    // THE SESSION WAS REPLACED, and NOTHING HERE RE-DIALS the socket opened on
    // the old one: the engine closes it `4401`, and the socket dials again
    // itself once this tab's requests have settled, with the new cookie.
    expect(reconnect).not.toHaveBeenCalled();
  });

  // A PERSON WHO HOLDS AN AUTHENTICATOR IS ASKED FOR ITS CODE, not offered it:
  // the field read "Code (optional)" and a password-only Confirm was refused
  // for the code. Confirm waits for one. The CONTROL is a person who holds
  // none, for whom it stays optional. Mutation: leave the field optional
  // whatever the person holds.
  test.each([
    ["holds an authenticator", [{ method: "totp", revoked: false }], true],
    ["holds none (the control)", [{ method: "password", revoked: false }], false],
  ])("a person who %s is asked for the code accordingly", async (_, credentials, required) => {
    engine({
      "/secrets/GITHUB_TOKEN": [refusedFor("step_up")],
      "/iam/credentials": [{ status: 200, body: { credentials } }],
    });
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByRole("dialog", { name: "Confirm it is you" });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "a password" } });
    const confirm = screen.getByRole("button", { name: "Confirm" });
    await waitFor(() => expect(confirm).toHaveProperty("disabled", required));
    expect(screen.queryByText(/optional/i) === null).toBe(required);
  });

  // ONE WINDOW, so the dialog names none: every sensitive gesture is held to
  // the same `step_up`, and a second sentence for a second window would be a
  // claim about a window the engine no longer has.
  test("names the one confirmation every sensitive change asks for", async () => {
    engine({ "/secrets/GITHUB_TOKEN": [refusedFor("step_up")] });
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByRole("dialog", { name: "Confirm it is you" });
    expect(screen.getByText(/confirmed who you are recently/)).toBeDefined();
  });

  // A WRONG PASSWORD OR CODE IS A CONFIRMATION REFUSED, not a sign-in: the
  // engine's one refusal reads "Those sign-in details were not accepted", which
  // the dialog showed verbatim, and its text asked for the password alone of
  // somebody it also asked a code of. The CONTROL is a person holding no
  // authenticator, asked for the password and told the password was wrong.
  // Mutation: show the engine's sentence, or ask for the password alone.
  test.each([
    [
      "holds an authenticator",
      [{ method: "totp", revoked: false }],
      "000000",
      /password and a code from your authenticator app/,
      "That password or code was not accepted. Check both and confirm again.",
    ],
    [
      "holds none (the control)",
      [{ method: "password", revoked: false }],
      "",
      /Enter your password to carry on/,
      "That password was not accepted. Check it and confirm again.",
    ],
  ])(
    "a person who %s is asked for, and refused, in a confirmation's words",
    async (_, credentials, code, asks, refused) => {
      engine({
        "/secrets/GITHUB_TOKEN": [refusedFor("step_up")],
        "/iam/credentials": [{ status: 200, body: { credentials } }],
        "/auth/step-up": [
          {
            status: 401,
            body: {
              error: "sign_in_refused",
              message: "Those sign-in details were not accepted. Check them and try again.",
            },
          },
        ],
      });
      mount();
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      await screen.findByRole("dialog", { name: "Confirm it is you" });
      await waitFor(() => expect(screen.getByText(asks)).toBeDefined());
      fireEvent.change(screen.getByLabelText("Password"), { target: { value: "a password" } });
      if (code) fireEvent.change(screen.getByLabelText(/^Code/), { target: { value: code } });
      fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
      await waitFor(() => expect(alerts()).toContain(refused));
      expect(alerts().join(" ")).not.toMatch(/sign-in details/);
    },
  );

  test("a wrong password stays for another try, and sends nothing again", async () => {
    const sent = engine({
      "/secrets/GITHUB_TOKEN": [refusedFor("step_up")],
      "/auth/step-up": [
        {
          status: 401,
          body: {
            error: "sign_in_refused",
            message: "Those sign-in details were not accepted. Check them and try again.",
          },
        },
      ],
    });
    mount();
    await refuseAndConfirm("wrong horse battery staple");

    await waitFor(() =>
      expect(alerts()).toContain("That password was not accepted. Check it and confirm again."),
    );
    expect(screen.getByRole("dialog", { name: "Confirm it is you" })).toBeDefined();
    // NOTHING WAS SENT AGAIN, and the need to sign in was not raised: a
    // mistyped password is not a lost session.
    expect(sent.filter((s) => s.path === "/secrets/GITHUB_TOKEN")).toHaveLength(1);
    expect(currentSessionNeed()).toBeNull();

    // DECLINING IS THE REFUSAL IT WAS, handed back to the screen that asked.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(outcome).toBeInstanceOf(RestError));
    expect((outcome as RestError).code).toBe("step_up_required");
  });

  test("a session the engine no longer accepts closes the dialog and goes to sign in", async () => {
    engine({
      "/secrets/GITHUB_TOKEN": [refusedFor("step_up")],
      "/auth/step-up": [{ status: 401, body: { error: "invalid_token" } }],
    });
    mount();
    await refuseAndConfirm("correct horse battery staple");

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(currentSessionNeed()).toBe("sign_in");
    await waitFor(() => expect((outcome as RestError).code).toBe("step_up_required"));
  });

  test("the host going away answers the request rather than stranding it", async () => {
    engine({ "/secrets/GITHUB_TOKEN": [refusedFor("step_up")] });
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByRole("dialog", { name: "Confirm it is you" });
    act(() => cleanup());
    await waitFor(() => expect((outcome as RestError).code).toBe("step_up_required"));
  });
});
