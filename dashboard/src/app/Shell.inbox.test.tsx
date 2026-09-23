/**
 * The shell is what makes the inbox a push rather than a poll.
 *
 * It is mounted once for the life of the tab, so it is the one place a watch
 * outlives the screen a reader is on: it watches the viewer's own seat, and an
 * `inbox_changed` frame for that seat asks the rail's inbox again well inside
 * the sixty-second poll that used to be the only way the badge moved.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Shell } from "./Shell.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { INBOX_SETTLE_MS } from "~/lib/useQuery.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

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

test("the shell watches the viewer's seat and asks for their inbox again when it moves", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  const watched: string[] = [];
  socket.watch = (seat: string) => void watched.push(seat);
  let inboxAsks = 0;
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    switch (what) {
      case "viewer":
        return Promise.resolve({
          login: "ana.lee",
          grants: ["state:read"],
          handle: "ana",
          name: "Ana Lee",
          kind: "human",
        });
      case "work_inbox":
        inboxAsks++;
        return Promise.resolve({
          handle: "ana",
          notices: [],
          primary_reasons: [],
          unread: 0,
          primary: 0,
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
  await act(async () => {});
  expect(watched).toEqual(["ana"]);
  const before = inboxAsks;
  expect(before).toBeGreaterThan(0);

  act(() => {
    store.applyInboxChanged({ handle: "ana", unread_delta: 1, subject: "t-1", reason: "assignee" });
  });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(INBOX_SETTLE_MS);
  });
  // WELL INSIDE THE POLL, which is the point: the poll alone would not have
  // asked again for another minute.
  expect(inboxAsks).toBe(before + 1);

  // AND THE WATCH ENDS WITH THE FRAME, so a tab that unmounts it leaves the
  // engine routing nothing to a socket nobody reads.
  view.unmount();
  expect(watched).toEqual(["ana", ""]);
});
