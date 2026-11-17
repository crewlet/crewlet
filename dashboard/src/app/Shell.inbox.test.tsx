/**
 * The shell is what makes the inbox a push rather than a poll.
 *
 * It is mounted once for the life of the tab, so it is the one place a watch
 * outlives the screen a reader is on: it watches the viewer's own seat, and an
 * `inbox_changed` frame for that seat asks the rail's inbox again well inside
 * the sixty-second poll that used to be the only way the badge moved.
 */

import { withDerived } from "~/test/org.ts";
import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Shell } from "./Shell.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
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
  Element.prototype.scrollIntoView = () => {};
  location.hash = "#/";
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "#/";
});

/**
 * The shell over a socket whose viewer holds `grants`, recording every seat
 * it watches and every question it asks of what waits on them.
 */
function shellAs(grants: string[], services?: Record<string, string>) {
  const store = new Store();
  // THE SOCKET'S FIRST FRAME CARRIES THE ORG, as a served frame's always
  // does: what the company runs is known before a read is asked.
  store.applyOrg(
    withDerived({ name: "Acme", roles: [], ...(services ? { services } : {}) }) as never,
  );
  const socket = new LiveSocket(store);
  const watched: string[] = [];
  socket.watch = (seat: string) => void watched.push(seat);
  const asks = { decisions: 0 };
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    switch (what) {
      case "viewer":
        return Promise.resolve({
          login: "ana.lee",
          grants,
          handle: "ana",
          owner: "ana",
          name: "Ana Lee",
          kind: "human",
        });
      case "decisions":
        // WHAT WAITS ON THE READER, which the frame's count reads.
        asks.decisions++;
        return Promise.resolve({
          handle: "ana",
          items: [],
          total: 0,
          answerable: 0,
          more: false,
          capped: false,
        });
      default:
        // Every other question the frame asks is beside the point here.
        return new Promise(() => {});
    }
  };
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>
          <div />
        </Shell>
      </Router>
    </ClientContext.Provider>,
  );
  return { store, view, watched, asks };
}

test("the shell watches the viewer's seat and asks what waits on them again when it moves", async () => {
  const { store, view, watched, asks } = shellAs(["worklet"]);
  await afterMicrotasks();
  expect(watched).toEqual(["ana"]);
  const before = asks.decisions;
  expect(before).toBeGreaterThan(0);

  act(() => {
    store.applyInboxChanged({ handle: "ana", unread_delta: 1, subject: "t-1", reason: "assignee" });
  });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  // WELL INSIDE THE POLL, which is the point: the poll alone would not have
  // asked again for another minute.
  expect(asks.decisions).toBe(before + 1);

  // AND THE WATCH ENDS WITH THE FRAME, so a tab that unmounts it leaves the
  // engine routing nothing to a socket nobody reads.
  view.unmount();
  expect(watched).toEqual(["ana", ""]);
});

// THE INBOX IS EVERY SIGNED-IN PERSON'S: it is composed from every native
// domain's notices, each read under its own permission on the engine's side,
// so a reader without Worklet is watched and asked their inbox all the same.
// Mutation: gate the watch on Worklet again and the seat is not watched.
test("a reader without Worklet is watched and asked what waits on them", async () => {
  const { watched, asks } = shellAs(["doclet"]);
  await afterMicrotasks();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(watched.filter(Boolean)).toEqual(["ana"]);
  expect(asks.decisions).toBeGreaterThan(0);
});

// EVERY COMPANY KEEPS ARTIFACTS, whose comments reach a person's inbox, so the
// watch is taken whatever the company runs. Mutation: take it only where
// Worklet or Gitlet runs, and this reader's seat is never watched.
test("a reader on a company running neither Worklet nor Gitlet is watched all the same", async () => {
  const { watched } = shellAs(["doclet"], {
    tracker: "jira",
    knowledge: "doclet",
    chat: "chatlet",
    code: "none",
  });
  await afterMicrotasks();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(watched.filter(Boolean)).toEqual(["ana"]);
});
