/**
 * The seat's Direct message door: drawn only where a room can be, held where
 * the press it opens onto would be, and a way into chat's New direct message
 * with the seat already chosen.
 *
 * Mutation: draw it whatever the company runs and the vendor case goes red;
 * send it anywhere but `#/chatlet?dm=` and the press case does.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { OWN_SEAT_DIRECT, WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import type { OrgServices } from "~/contract/services.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import { DirectMessageSeatButton } from "./DirectMessage.tsx";

vi.mock("~/lib/store-hooks.ts", () => ({ useConnection: vi.fn(), useOrg: vi.fn() }));
vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

/** A person holding Chatlet, bound to `ada`, on a company running its own chat. */
const ADA: ViewerState = {
  login: "ada.lovelace",
  grants: ["chatlet"],
  admin: false,
  worklet: false,
  doclet: false,
  chatlet: true,
  handle: "ada",
  owner: "ada",
  name: "Ada",
  acts: ["open_dm"],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
  configWriter: false,
  configManagedBy: [],
  chatVendors: [],
  assistant: "disabled",
  assistantModel: null,
};

/** What the engine says the company runs: Crewlet's own, all three. */
const CHATLET: OrgServices = { tracker: "worklet", knowledge: "doclet", chat: "chatlet" };

beforeEach(() => {
  location.hash = "#/agents/swe";
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

function mount(viewer: ViewerState, handle = "swe", services: OrgServices = CHATLET) {
  vi.mocked(useViewer).mockReturnValue(viewer);
  vi.mocked(useOrg).mockReturnValue({ name: "Acme", services } as OrgProjection);
  render(
    <Router>
      <DirectMessageSeatButton handle={handle} />
    </Router>,
  );
  return screen.queryByRole("button", { name: /direct message/i });
}

test("opens chat's New direct message with the seat chosen", async () => {
  const door = mount(ADA);
  expect(door?.getAttribute("aria-disabled")).not.toBe("true");
  fireEvent.click(door!);
  await waitFor(() => expect(location.hash).toBe("#/chatlet?dm=swe"));
});

test.each([
  ["a company on a vendor's chat", ADA, { ...CHATLET, chat: "vendor" as const }],
  ["a company with no chat", ADA, { ...CHATLET, chat: "none" as const }],
  ["a reader without Chatlet", { ...ADA, chatlet: false }, CHATLET],
])("is not drawn for %s", (_, viewer, services) => {
  expect(mount(viewer, "swe", services)).toBeNull();
});

test.each([
  ["on the reader's own seat", ADA, "ada", OWN_SEAT_DIRECT],
  [
    "where the engine does not open one for them",
    { ...ADA, acts: [] },
    "swe",
    WRITE_REASONS.not_served,
  ],
])("is held, with why, %s", (_, viewer, handle, reason) => {
  const door = mount(viewer, handle);
  expect(door?.getAttribute("aria-disabled")).toBe("true");
  expect(door?.getAttribute("title")).toBe(reason);
});
