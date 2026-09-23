/**
 * What a container's "Filed by" fact says when the company document did not
 * answer.
 *
 * WHO FILES INTO A CONTAINER is read from the company document — a unit's
 * `space:` is guarded — and the fact printed "Needs an operator token to read"
 * for every way that read could fail to answer: to a person signed in without
 * `config:read`, who lacks a grant rather than a token; to a node still
 * catching up after a restart; and to a read that simply had not come back.
 * Three facts, and only the first is about the reader.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { ContainerPeek } from "./Knowledge.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryRefusedError, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

const container = {
  key: "ENG",
  name: "Engineering",
  pages: 0,
  created_at: "2026-09-01T00:00:00Z",
};

/** The peek, over a socket answering the container and its pages, and `config` as told. */
function mount(config: () => Promise<unknown>) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = async (what) => {
    if (what === "containers") return { containers: [container] };
    if (what === "pages") return { pages: [], limit: 50 };
    if (what === "config") return config();
    return {};
  };
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ContainerPeek id="ENG" />
      </Router>
    </ClientContext.Provider>,
  );
}

test("a refused document names the grant the refusal named", async () => {
  mount(() =>
    Promise.reject(
      new QueryRefusedError("unauthorized", { reason: "no_grant", grants: ["config:read"] }),
    ),
  );
  expect(
    await screen.findByText(
      "Reading who files here needs config:read, which the credential you presented does not carry.",
    ),
  ).toBeDefined();
  expect(screen.queryByText(/operator token/)).toBeNull();
});

test("a node that could not answer is not a refusal of the reader", async () => {
  mount(() => Promise.reject(new Error("unavailable")));
  expect(await screen.findByText("The company document could not be read just now")).toBeDefined();
  expect(screen.queryByText(/needs/)).toBeNull();
});

test("a read still in flight says so", async () => {
  mount(() => new Promise(() => {}));
  await waitFor(() => expect(screen.getByText("Engineering")).toBeDefined());
  expect(screen.getByText("The company document has not answered yet")).toBeDefined();
});
