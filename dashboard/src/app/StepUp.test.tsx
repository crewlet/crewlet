/**
 * The one step-up ceremony, as a person meets it: a save refused for a
 * fresher proof asks for the password, and then the save goes through — the
 * same request, with nothing retyped.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "~/test/inCase.ts";
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
    expect(sent.map((s) => s.path)).toEqual([
      "/secrets/GITHUB_TOKEN",
      "/auth/step-up",
      "/secrets/GITHUB_TOKEN",
    ]);
    expect(sent[1]?.body).toEqual({ password: "correct horse battery staple" });
    // THE REPLAY IS THE REQUEST THAT WAS REFUSED, which is what keeps a form.
    expect(sent[2]?.body).toEqual(sent[0]?.body);
    // THE SESSION WAS REPLACED, so the socket opened on the old one re-dials.
    expect(reconnect).toHaveBeenCalled();
  });

  test("says which window, when it is the sensitive one", async () => {
    engine({ "/secrets/GITHUB_TOKEN": [refusedFor("step_up_sensitive")] });
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByRole("dialog", { name: "Confirm it is you" });
    expect(screen.getByText(/very recent confirmation/)).toBeDefined();
  });

  test("a wrong password is the engine's own sentence, and the dialog stays for another try", async () => {
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
      expect(alerts()).toContain(
        "Those sign-in details were not accepted. Check them and try again.",
      ),
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
