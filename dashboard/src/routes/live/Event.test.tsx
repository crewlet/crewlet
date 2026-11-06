/**
 * An event's page bar holds short controls: the seat it was published by is a
 * "Seat" button, with the seat's whole name in its tooltip and its name.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { EventScreen } from "./Event.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { afterMicrotasks } from "~/test/settle.ts";

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
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const NAME = "Principal Product Manager for Learner Journeys, Spaced Repetition and Discovery";

const EVENT = {
  id: "ev-1",
  type: "task_assigned",
  category: "task",
  source: "engine",
  actor: NAME,
  summary: "assigned a task",
  timestamp: "2026-09-13T10:00:00Z",
  trace_id: "",
  span_id: "",
  parent_span_id: "",
  topic: "",
  failed: false,
  tags: { agent_id: "a-pm" },
  payload: {},
};

/* A seat's whole name in the bar cut the breadcrumb. Mutation: label it {data.actor} again. */
test("the actor's button is a short noun, with the seat's name on hover and in its name", async () => {
  location.hash = "#/live/events/ev-1";
  const store = new Store();
  store.applySeats([{ role: NAME, handle: "pm", agent_id: "a-pm" }]);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "event"
        ? EVENT
        : what === "viewer"
          ? { login: "founder", owner: "jane", grants: ["admin"] }
          : {},
    );
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <EventScreen eventId="ev-1" />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  await afterMicrotasks();
  const seat = await screen.findByRole("button", { name: `Seat, ${NAME}` });
  expect(seat.textContent).toBe("Seat");
  act(() => seat.focus());
  expect(screen.getByRole("tooltip").textContent).toBe(NAME);
});
