/**
 * An event's page bar holds short controls: the seat it was published by is a
 * "Seat" button, with the seat's whole name in its tooltip and its name.
 */

import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { EventScreen } from "./Event.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import {
  drawnRef,
  expectNamedLinks,
  namedAnswer,
  NAMED_LINE,
  NAMED_REFS,
} from "~/test/namedRefs.tsx";

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

/* AN EVENT'S OWN PAGE IS HEADED BY ITS LINE, and what the line names is a
   marked link to its page the reader may open; what is gone is its words. */
test("an event's page links what its line names", async () => {
  location.hash = "#/live/events/ev-1";
  const store = new Store();
  const socket = new LiveSocket(store);
  (
    socket as unknown as { query: (what: string, p: Record<string, unknown>) => Promise<unknown> }
  ).query = (what, p) =>
    Promise.resolve(
      what === "event"
        ? { ...EVENT, summary: `${NAME} was ${NAMED_LINE}`, refs: NAMED_REFS }
        : what === "references"
          ? namedAnswer(p)
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
  await waitFor(() => expect(drawnRef(document.body, "ENG-287").tagName).toBe("A"));
  expectNamedLinks(document.body);
});

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
  expect(screen.getByRole("tooltip", { hidden: true }).textContent).toBe(NAME);
});

// THE PAYLOAD WRAPS. Drawn unwrapped, a long value ran past the card's edge
// and was cut there: at a phone's width the ids and times of every line, and
// at any width a long summary, with a sideways scroll nobody saw.
//
// Mutation: draw the payload with `wrap={false}` again.
test("the payload's long lines wrap rather than run past the card", async () => {
  location.hash = "#/live/events/ev-1";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "event"
        ? { ...EVENT, payload: { conversation_key: "work:task:" + "2eec2dd5".repeat(12) } }
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
  const block = (await screen.findByLabelText("The event payload, as JSON")).closest(
    ".crewlet-codeblock",
  );
  expect(block).not.toBeNull();
  expect(block!.classList.contains("crewlet-codeblock--nowrap")).toBe(false);
});
