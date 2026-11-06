/**
 * A seat's profile beside the conversation: who a name in a room is.
 *
 * THE INVARIANT: a profile is drawn for a seat the chart holds, and only for
 * one. A handle nobody holds (a stale mention, a removed seat, an edited
 * address) is said to be nobody's, with nothing to message, rather than drawn
 * as the agent-shaped stranger a message row falls back to.
 */

import { act, cleanup, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { actEngine } from "~/test/chat.tsx";
import { CHAT_ORG, SERVED } from "~/test/chatAccess.ts";

import { mountInFrame } from "./frameKit.tsx";
import { ProfilePane } from "./Profile.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** One of the reader's rooms, as `chat_channels` answers it. */
function room(id: string, name: string) {
  return {
    channel: { v: 1, id, kind: "public", name, created_at: "2026-01-01T00:00:00Z" },
    revision: 1,
    unread: 0,
    mentions: 0,
    member: true,
  };
}

/**
 * The profile of `handle`, with the chart arrived unless `chart` is false,
 * and `common` the rooms the engine says the reader shares with it.
 */
function mount(
  handle: string,
  { chart = true, common = [] }: { chart?: boolean; common?: unknown[] } = {},
) {
  actEngine();
  const mounted = mountInFrame(<ProfilePane handle={handle} onClose={() => undefined} />, {
    hash: "#/chatlet",
    answer: async (what, params) =>
      what === "chat_channels"
        ? { ...SERVED, read_state: true, channels: params?.with ? common : [] }
        : {},
  });
  if (chart) act(() => mounted.store.applyOrg(CHAT_ORG as never));
  return mounted;
}

// Mutation: draw the stranger as a profile again and the heading is gone and
// Message is offered.
test("a handle no seat holds is said to be nobody's, with nothing to message", async () => {
  const { socket } = mount("nobody-here");
  expect(await screen.findByRole("heading", { name: "No one holds @nobody-here" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Message/ })).toBeNull();
  expect(screen.queryByText(/Agent seat/)).toBeNull();
  // NOR ARE THE ROOMS OF NOBODY ASKED FOR.
  const asked = socket.query.mock.calls.filter(
    ([what, params]) => what === "chat_channels" && (params as { with?: string })?.with,
  );
  expect(asked).toHaveLength(0);
});

test("a seat the chart holds is drawn as its profile, with Message", async () => {
  mount("bo");
  expect(await screen.findByRole("heading", { name: "Bo Chen" })).toBeTruthy();
  expect(screen.getByRole("button", { name: /^Message/ })).toBeTruthy();
  expect(screen.queryByText(/No one holds/)).toBeNull();
});

// BEFORE THE CHART HAS COME, every handle is unknown: that is a pane still
// loading, not a seat nobody holds.
test("a handle is not called nobody's before the chart has arrived", async () => {
  mount("bo", { chart: false });
  await waitFor(() => expect(screen.getByRole("heading", { name: "Profile" })).toBeTruthy());
  expect(screen.queryByText(/No one holds/)).toBeNull();
  expect(screen.queryByRole("button", { name: /^Message/ })).toBeNull();
});

// ROOMS IN COMMON ARE A SECTION OF THEIR OWN, one room a row whose whole name
// is its title: as a value beside its term in a narrow pane, a long room name
// broke at every hyphen. Mutation: put the rooms back in the facts' list and
// the first assertion goes red.
test("rooms in common are a list of their own under the facts, one room a row", async () => {
  const long = "release-coordination-autumn-study-streaks-web-ios-android";
  mount("bo", { common: [room("r-1", long), room("r-2", "general")] });
  const list = await screen.findByRole("list");
  expect(list.closest("dl")).toBeNull();
  expect(screen.getByRole("heading", { name: "Rooms in common" })).toBeTruthy();
  const rows = within(list).getAllByRole("listitem");
  expect(rows).toHaveLength(2);
  const link = within(rows[0]!).getByRole("link", { name: long });
  expect(link.getAttribute("title")).toBe(long);
  expect(link.querySelector(".chat-profile__room-name")?.textContent).toBe(long);
});
