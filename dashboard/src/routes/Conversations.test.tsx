/**
 * The agent-to-agent record says how many channels it holds, in its head.
 *
 * The line that used to close the list carried that number, and it counted the
 * page rather than the record. This screen is the one of the seven that had no
 * other place saying it: its three stat tiles count open channels, messages
 * and pairs, and none of them counts the record itself.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Conversations } from "./Conversations.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

const CHANNELS = [
  {
    id: "c1",
    requester: "planner",
    target: "reviewer",
    subject: "the release note",
    messages: 2,
    opened_at: "2026-09-14T10:00:00Z",
    last_at: "2026-09-14T10:05:00Z",
    closed_at: "2026-09-14T10:06:00Z",
  },
  {
    id: "c2",
    requester: "reviewer",
    target: "planner",
    subject: "the second pass",
    messages: 1,
    opened_at: "2026-09-14T11:00:00Z",
    last_at: "2026-09-14T11:01:00Z",
    closed_at: "",
  },
];

function mount() {
  location.hash = "#/conversations";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "a2a_channels" ? { channels: CHANNELS, available: true } : null);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Conversations />
      </Router>
    </ClientContext.Provider>,
  );
}

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

test("how many channels there are is in the head, not under the rows", async () => {
  mount();
  const header = screen.getByRole("banner");
  expect(await within(header).findByText("2 channels in the record")).toBeDefined();
  // What the removed line said, in the words it said it in.
  expect(screen.queryByText(/match/)).toBeNull();
  expect(screen.queryByText(/^Showing/)).toBeNull();
});
