/**
 * Settings › Diagnostics' usage insights (ADR-0119). The invariants: it is off
 * until somebody chooses to share, and says what a record holds and never holds
 * before they do; choosing to share accepts this build's catalogue; a record
 * waiting for review is sent or declined by its class and week, and always
 * sending a class adds it to the list and sends the record; every record opens
 * its exact bytes.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import type { InsightsAnswer } from "~/protocol/insights.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { settled } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Insights } from "./Insights.tsx";

const save = vi.hoisted(() => vi.fn());
const read = vi.hoisted(() => vi.fn());
vi.mock("~/protocol/configWrite.ts", async (original) => ({
  ...(await original<typeof import("~/protocol/configWrite.ts")>()),
  getConfig: read,
  savePatch: save,
}));

const post = vi.hoisted(() => vi.fn());
vi.mock("~/protocol/rest.ts", async (original) => {
  const real = await original<typeof import("~/protocol/rest.ts")>();
  return { ...real, rest: { ...real.rest, post } };
});

const access = vi.hoisted(() => vi.fn());
vi.mock("~/lib/useWriteAccess.ts", async (original) => ({
  ...(await original<typeof import("~/lib/useWriteAccess.ts")>()),
  useConfigWriteAccess: access,
}));

const NOOP = {
  class: "wake.noop_share",
  title: "Agents were woken for turns that changed nothing",
  summary: "The share of turns that changed nothing.",
  facts: [
    { key: "seats", values: ["1", "2-3", "4+"] },
    { key: "share", values: ["25-34", "35-49", "50+"] },
  ],
};

const RECORD = {
  schema: 1,
  class: "wake.noop_share",
  week: "2030-W02",
  version: "v1.4.0",
  facts: { seats: "1", share: "35-49" },
};

function reading(over: Partial<InsightsAnswer> = {}): InsightsAnswer {
  return {
    share: "off",
    accepted: 0,
    schema: 1,
    always: [],
    relay: "https://reports.crewlet.ai/v1/insights",
    catalogue: { schema: 1, classes: [NOOP] },
    records: [],
    ...over,
  };
}

async function mount(answer: InsightsAnswer) {
  location.hash = "#/settings/diagnostics";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "viewer" ? ADMIN_VIEWER : answer);
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <Insights />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  await settled();
  return view;
}

beforeEach(() => {
  vi.clearAllMocks();
  access.mockReturnValue({ can: true });
  read.mockResolvedValue({ kind: "document", document: {}, etag: '"rev"' });
  save.mockResolvedValue({ kind: "saved", revisionId: "r", epoch: 2, warnings: [], derived: null });
  post.mockResolvedValue({});
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

test("off by default, it says what a record holds and never holds before anybody shares", async () => {
  await mount(reading());
  expect((screen.getByRole("radio", { name: /Don't share/ }) as HTMLInputElement).checked).toBe(
    true,
  );
  expect(screen.getByText("A record never holds")).toBeTruthy();
  expect(screen.getByText(/Any role, agent, person, team, server or host/)).toBeTruthy();
  expect(screen.queryByText("Waiting for your review")).toBeNull();
});

test("choosing to review accepts this build's catalogue, on the revision just read", async () => {
  await mount(reading());
  await act(async () => {
    fireEvent.click(screen.getByRole("radio", { name: /Review each record first/ }));
  });
  await settled();
  expect(save).toHaveBeenCalledOnce();
  const [patch, etag] = save.mock.calls[0] as unknown[];
  expect(patch).toEqual({ diagnostics: { insights: { share: "review", accepted: 1 } } });
  expect(etag).toBe('"rev"');
});

test("a sample record is the exact shape a class would send", async () => {
  await mount(reading());
  const sample = screen.getByLabelText(/Sample record: Agents were woken/);
  expect(JSON.parse(sample.textContent ?? "")).toEqual({
    schema: 1,
    class: "wake.noop_share",
    week: "2030-W02",
    version: "v1.4.0",
    facts: { seats: "1", share: "25-34" },
  });
});

test("a newer catalogue than the one accepted shares nothing until it is accepted", async () => {
  await mount(reading({ share: "auto", accepted: 1, schema: 2 }));
  expect(screen.getByText("Nothing is shared until you accept the new list")).toBeTruthy();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Accept the new list" }));
  });
  await settled();
  expect(save.mock.calls[0]?.[0]).toEqual({ diagnostics: { insights: { accepted: 2 } } });
});

test("a record waiting for review is sent or declined by its class and week", async () => {
  await mount(
    reading({
      share: "review",
      accepted: 1,
      records: [{ class: "wake.noop_share", week: "2030-W02", status: "queued", record: RECORD }],
    }),
  );
  const queue = screen.getByRole("table", { name: /waiting for your review/i });
  await act(async () => {
    fireEvent.click(within(queue).getByRole("button", { name: "Send" }));
  });
  await settled();
  expect(post).toHaveBeenCalledWith("/diagnostics/insights/decisions", {
    class: "wake.noop_share",
    week: "2030-W02",
    choice: "send",
  });
  await act(async () => {
    fireEvent.click(within(queue).getByRole("button", { name: /Don.t send/ }));
  });
  await settled();
  expect(post).toHaveBeenLastCalledWith("/diagnostics/insights/decisions", {
    class: "wake.noop_share",
    week: "2030-W02",
    choice: "decline",
  });
});

test("always sending a pattern adds it to the list and sends the record", async () => {
  await mount(
    reading({
      share: "review",
      accepted: 1,
      always: ["review.idle"],
      records: [{ class: "wake.noop_share", week: "2030-W02", status: "queued", record: RECORD }],
    }),
  );
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Always send this pattern" }));
  });
  await settled();
  expect(save.mock.calls[0]?.[0]).toEqual({
    diagnostics: { insights: { always: ["review.idle", "wake.noop_share"] } },
  });
  expect(post).toHaveBeenCalledWith("/diagnostics/insights/decisions", {
    class: "wake.noop_share",
    week: "2030-W02",
    choice: "send",
  });
});

test("every record kept opens its exact bytes", async () => {
  await mount(
    reading({
      share: "auto",
      accepted: 1,
      records: [
        { class: "wake.noop_share", week: "2030-W02", status: "sent", answer: 202, record: RECORD },
      ],
    }),
  );
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /See the record for Agents were woken/ }));
  });
  await settled();
  const code = screen.getByLabelText(/exactly as it is sent/);
  expect(JSON.parse(code.textContent ?? "")).toEqual(RECORD);
});

test("a reader who may not write cannot change the consent or answer a record", async () => {
  access.mockReturnValue({ can: false, reason: "Only an admin can change this." });
  await mount(
    reading({
      share: "review",
      accepted: 1,
      records: [{ class: "wake.noop_share", week: "2030-W02", status: "queued", record: RECORD }],
    }),
  );
  expect(screen.getByText("Only an admin can change this.")).toBeTruthy();
  expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
});
