/**
 * A seat's profile beside the conversation: who a name in a room is.
 *
 * THE INVARIANT: a profile is drawn for a seat the chart holds, and only for
 * one. A handle nobody holds (a stale mention, a removed seat, an edited
 * address) is said to be nobody's, with nothing to message, rather than drawn
 * as the agent-shaped stranger a message row falls back to.
 */

import { act, cleanup, screen, waitFor } from "@testing-library/react";
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

/** The profile of `handle`, with the chart arrived unless `chart` is false. */
function mount(handle: string, { chart = true }: { chart?: boolean } = {}) {
  actEngine();
  const mounted = mountInFrame(<ProfilePane handle={handle} onClose={() => undefined} />, {
    hash: "#/chatlet",
    answers: { chat_channels: { ...SERVED, read_state: true, channels: [] } },
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
