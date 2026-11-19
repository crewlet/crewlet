/**
 * Message a seat, and its second way in: a direct conversation in Chatlet
 * where the company runs it and the reader holds it, with the tracker's ask
 * as More's "Ask as a task"; the ask itself where no conversation can be.
 *
 * Mutation: make Message the ask whatever chat the company runs, as it was,
 * and the first case files a task instead of opening the conversation; draw
 * "Ask as a task" where Message is already the ask and the vendor case finds
 * two ways to file the same thing.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { NewTaskOpener } from "~/app/newTask.ts";
import { MessageSeatButton } from "~/components/writes.tsx";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { OWN_SEAT_DIRECT, WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import type { OrgServices } from "~/contract/services.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import { Menu } from "@crewlethq/ui";
import { useAskTaskEntry } from "./AskTask.tsx";

/** A seat's "More" holding the ask, or nothing where there is none. */
function More({ handle }: { handle: string }) {
  const entries = useAskTaskEntry(handle);
  return entries.length > 0 ? <Menu label="More" items={entries} /> : null;
}

vi.mock("~/lib/store-hooks.ts", () => ({ useConnection: vi.fn(), useOrg: vi.fn() }));
vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

/** A person holding Chatlet, bound to `ada`, on a company running its own chat. */
const ADA: ViewerState = {
  login: "ada.lovelace",
  grants: ["chatlet", "worklet"],
  admin: false,
  worklet: true,
  doclet: false,
  chatlet: true,
  gitlet: false,
  handle: "ada",
  owner: "ada",
  name: "Ada",
  acts: ["open_dm", "create_work_item"],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
  failed: null,
  failedRefusal: null,
  configWriter: false,
  configManagedBy: [],
  chatVendors: [],
  assistant: "disabled",
  assistantModel: null,
  assistantReadsImages: false,
};

/** What the engine says the company runs: Crewlet's own, all four. */
const CHATLET: OrgServices = {
  tracker: "worklet",
  knowledge: "doclet",
  chat: "chatlet",
  code: "gitlet",
};

beforeEach(() => {
  location.hash = "#/agents/swe";
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

/** What the New task sheet was opened with, by Message or by Ask as a task. */
let filed: unknown[] = [];

function mount(viewer: ViewerState, handle = "swe", services: OrgServices = CHATLET) {
  filed = [];
  vi.mocked(useViewer).mockReturnValue(viewer);
  vi.mocked(useOrg).mockReturnValue({ name: "Acme", services } as OrgProjection);
  render(
    <Router>
      <NewTaskOpener.Provider value={(preset) => filed.push(preset)}>
        <MessageSeatButton handle={handle} />
        <More handle={handle} />
      </NewTaskOpener.Provider>
    </Router>,
  );
}

const message = () => screen.queryByRole("button", { name: "Message" });

/** The More menu's Ask as a task, opened, or null where More is not drawn. */
function askEntry() {
  const more = screen.queryByRole("button", { name: "More" });
  if (!more) return null;
  fireEvent.click(more);
  return screen.getByRole("menuitem", { name: /ask as a task/i });
}

test("where the company runs Chatlet, Message opens a direct conversation with the seat", async () => {
  mount(ADA);
  expect(message()?.getAttribute("aria-disabled")).not.toBe("true");
  fireEvent.click(message()!);
  await waitFor(() => expect(location.hash).toBe("#/chatlet?dm=swe"));
  expect(filed).toEqual([]);
});

test("the ask is still offered there, as More's Ask as a task", () => {
  mount(ADA);
  const ask = askEntry();
  expect(ask?.getAttribute("aria-disabled")).not.toBe("true");
  fireEvent.click(ask!);
  expect(filed).toEqual([{ assignee: "swe", ask: "swe" }]);
});

test.each([
  ["a company on a vendor's chat", ADA, { ...CHATLET, chat: "vendor" as const }],
  ["a company with no chat", ADA, { ...CHATLET, chat: "none" as const }],
  ["a reader without Chatlet", { ...ADA, chatlet: false }, CHATLET],
])("for %s Message is the ask, and More offers no second one", (_, viewer, services) => {
  mount(viewer, "swe", services);
  fireEvent.click(message()!);
  expect(filed).toEqual([{ assignee: "swe", ask: "swe" }]);
  expect(location.hash).not.toContain("chatlet");
  expect(askEntry()).toBeNull();
});

test("a company running Chatlet and no Worklet still offers Message, and no ask", () => {
  mount(ADA, "swe", { ...CHATLET, tracker: "jira" });
  expect(message()).not.toBeNull();
  expect(askEntry()).toBeNull();
});

test.each([
  ["on the reader's own seat", ADA, "ada", OWN_SEAT_DIRECT],
  [
    "where the engine does not open one for them",
    { ...ADA, acts: ["create_work_item"] },
    "swe",
    WRITE_REASONS.not_served,
  ],
])("Message is held, with why, %s", (_, viewer, handle, reason) => {
  mount(viewer, handle);
  expect(message()?.getAttribute("aria-disabled")).toBe("true");
  expect(message()?.getAttribute("title")).toBe(reason);
});
