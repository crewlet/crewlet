/**
 * Catch up and Summarize, where the reader may not ask Ask Crewlet.
 *
 * THE INVARIANT: nothing is asked of the assistant that it would refuse. Where
 * the reader may not ask (an admin turned it off, no model, not a person),
 * the room menu holds Catch up with the reason the Ask button gives, and a
 * pane reached anyway (an address naming it) says why in the drawer's words,
 * with no Ask again and Continue held, rather than drawing the engine's bare
 * refusal as a failure a retry could clear.
 */

import { fireEvent, cleanup, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { ReactNode } from "react";

import { assistantUnavailableFor } from "~/app/assistant/availability.ts";
import type { AssistantControl } from "~/app/assistant/control.ts";
import { actEngine } from "~/test/chat.tsx";
import { CHAT_ORG, SERVED, holdOf } from "~/test/chatAccess.ts";

import { CatchUpPane } from "./CatchUp.tsx";
import { mountInFrame } from "./frameKit.tsx";
import { RoomScopeProvider } from "./scope.tsx";
import { ChatTopBar } from "./TopBar.tsx";

const ROOM = "0b0e9a8e-2f4c-4f7e-9d8a-5d1f0c6b7a10";

const channel = {
  v: 1,
  id: ROOM,
  kind: "public",
  name: "general",
  created_at: "2026-01-01T00:00:00Z",
};

const answers = {
  chat_channels: {
    ...SERVED,
    read_state: true,
    read: { handle: "ada", revision: 1, cursors: {} },
    channels: [{ channel, revision: 1, unread: 3, mentions: 0 }],
  },
  chat_channel: {
    ...SERVED,
    channel,
    revision: 1,
    message_seq: 1,
    member: true,
    members: [{ handle: "ada", joined_at: "2026-01-01T00:00:00Z" }],
  },
};

/** The company left Ask Crewlet off, as the frame tells every control. */
const OFF = assistantUnavailableFor(
  { anonymous: false, asking: false, assistant: "disabled" },
  "Acme",
)!;

function control(unavailable: AssistantControl["unavailable"]): AssistantControl {
  return { open: false, unavailable, toggle: () => undefined, ask: () => undefined };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** `children` in the chat frame over the room, with the assistant as `assistant` says. */
function mount(children: ReactNode, assistant: AssistantControl) {
  actEngine();
  const mounted = mountInFrame(children, { hash: `#/chatlet/${ROOM}`, answers, assistant });
  mounted.store.applyOrg(CHAT_ORG as never);
  return mounted;
}

/** Every request the screen sent to the assistant. */
function assistantAsks(): string[] {
  return vi
    .mocked(fetch)
    .mock.calls.map(([url]) => String(url))
    .filter((url) => url.includes("/assistant/"));
}

// Mutation: ask the brief whatever the assistant's availability, and the
// engine's refusal is drawn with Ask again, and a request is sent.
test("a catch up the reader may not ask says why, with no retry, and asks nothing", async () => {
  mount(<CatchUpPane room={ROOM} mode="catchup" onClose={() => undefined} />, control(OFF));
  expect(await screen.findByRole("heading", { name: OFF.title })).toBeTruthy();
  expect(screen.getByText(OFF.body)).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Ask again/ })).toBeNull();
  const go = screen.getByRole("button", { name: /Continue in Ask Crewlet/ });
  expect(holdOf(go)).toBe(OFF.short);
  await new Promise((resolve) => setTimeout(resolve, 20));
  expect(assistantAsks()).toHaveLength(0);
});

// Mutation: drop the hold on the menu entry and it is pressable with nothing
// said, opening a pane that can only refuse.
test("the room menu holds Catch up with why where the reader may not ask", async () => {
  mount(
    <RoomScopeProvider channelID={ROOM}>
      <ChatTopBar />
    </RoomScopeProvider>,
    control(OFF),
  );
  fireEvent.click(await screen.findByRole("button", { name: "More" }));
  const entry = await screen.findByRole("menuitem", {
    name: /(Catch up|Summarize) with Ask Crewlet/,
  });
  await waitFor(() => expect(entry.getAttribute("aria-disabled")).toBe("true"));
  expect(within(entry).getByText(OFF.short)).toBeTruthy();
});

test("the room menu offers Catch up where the reader may ask", async () => {
  mount(
    <RoomScopeProvider channelID={ROOM}>
      <ChatTopBar />
    </RoomScopeProvider>,
    control(null),
  );
  fireEvent.click(await screen.findByRole("button", { name: "More" }));
  const entry = await screen.findByRole("menuitem", {
    name: /(Catch up|Summarize) with Ask Crewlet/,
  });
  expect(entry.getAttribute("aria-disabled")).not.toBe("true");
});
