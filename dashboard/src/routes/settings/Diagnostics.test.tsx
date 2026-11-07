/**
 * Settings › Diagnostics. The invariants worth breaking a build over: it is off
 * until somebody turns it on, and says what is sent and what never is before they
 * do; the write is the one setting, on the revision just read; a build that sends
 * nothing says so once it is on; and a reader who may not write cannot move it.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import type { IssueReportInert, IssueReportsAnswer } from "~/protocol/issueReports.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { settled } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Diagnostics } from "./Diagnostics.tsx";

const save = vi.hoisted(() => vi.fn());
const read = vi.hoisted(() => vi.fn());
vi.mock("~/protocol/configWrite.ts", async (original) => ({
  ...(await original<typeof import("~/protocol/configWrite.ts")>()),
  getConfig: read,
  savePatch: save,
}));

const access = vi.hoisted(() => vi.fn());
vi.mock("~/lib/useWriteAccess.ts", async (original) => ({
  ...(await original<typeof import("~/lib/useWriteAccess.ts")>()),
  useConfigWriteAccess: access,
}));

const FILED = {
  class: "turn.panicked",
  title: "A turn panicked",
  fingerprint: "ab".repeat(32),
  first_sent: "2026-11-06T07:00:00Z",
  last_sent: "2026-11-06T07:00:00Z",
  status: "filed",
  issue: "https://github.com/crewlet/crewlet/issues/42",
  payload: { schema: 1, class: "turn.panicked" },
} as const;

function reading(over: Partial<IssueReportsAnswer> = {}): IssueReportsAnswer {
  return {
    enabled: false,
    inert: "",
    relay: "https://reports.crewlet.ai/v1/reports",
    reports: [],
    previews: [
      {
        class: "turn.panicked",
        title: "A turn panicked",
        summary: "A panic was recovered.",
        payload: { schema: 1, site: "internal/agent/turn.Recovered" },
      },
    ],
    ...over,
  };
}

/** The screen, over a socket answering `answer`. */
async function mount(answer: IssueReportsAnswer) {
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
            <Diagnostics />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  await settled();
  return view;
}

const theSwitch = () =>
  screen.getByRole("switch", { name: /report engine defects/i }) as HTMLInputElement;

beforeEach(() => {
  vi.clearAllMocks();
  access.mockReturnValue({ can: true });
  read.mockResolvedValue({ kind: "document", document: {}, etag: '"rev"' });
  save.mockResolvedValue({ kind: "saved", revisionId: "r", epoch: 2, warnings: [], derived: null });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

test("off by default, it says what is sent and what never is before anybody opts in", async () => {
  await mount(reading());
  expect(theSwitch().checked).toBe(false);
  expect(screen.getByText("What is sent")).toBeTruthy();
  expect(screen.getByText("What never is")).toBeTruthy();
  expect(screen.getByText(/Error messages, stacks, paths/)).toBeTruthy();
});

test("turning it on writes the one setting, on the revision just read, with its summary", async () => {
  await mount(reading());
  await act(async () => {
    fireEvent.click(theSwitch());
    await settled();
  });
  expect(save).toHaveBeenCalledOnce();
  const [patch, etag, summary] = save.mock.calls[0] ?? [];
  expect(patch).toEqual({ diagnostics: { issue_reports: { enabled: true } } });
  expect(etag).toBe('"rev"');
  expect(summary).toBe("Turned engine issue reports on");
  expect(theSwitch().checked).toBe(true);
});

test("a refused write says so, and the switch stays where the engine has it", async () => {
  save.mockResolvedValue({ kind: "draining", detail: "" });
  await mount(reading());
  await act(async () => {
    fireEvent.click(theSwitch());
    await settled();
  });
  expect(screen.getByText("The change was not saved")).toBeTruthy();
  expect(theSwitch().checked).toBe(false);
});

test("a reader who may not write cannot move it, and is told why", async () => {
  access.mockReturnValue({ can: false, block: "no_grant", reason: "Changing this takes Admin." });
  await mount(reading());
  await act(async () => {
    fireEvent.click(theSwitch());
    await settled();
  });
  expect(save).not.toHaveBeenCalled();
  expect(theSwitch().checked).toBe(false);
  expect(screen.getAllByText("Changing this takes Admin.").length).toBeGreaterThan(0);
});

test.each<[IssueReportInert, RegExp]>([
  ["development", /development build/],
  ["modified", /uncommitted changes/],
  ["no_commit", /recorded no commit/],
])("a %s build says plainly that it sends nothing, once the switch is on", async (inert, words) => {
  const on = await mount(reading({ enabled: true, inert }));
  expect(screen.getByText("This build sends no reports")).toBeTruthy();
  expect(screen.getByText(words)).toBeTruthy();
  on.unmount();

  await mount(reading({ enabled: false, inert }));
  expect(screen.queryByText("This build sends no reports")).toBeNull();
});

test("a build that reports says nothing of the kind", async () => {
  await mount(reading({ enabled: true, inert: "" }));
  expect(screen.queryByText("This build sends no reports")).toBeNull();
});

test("the list names each report's status and issue, and opens the bytes it sent", async () => {
  await mount(reading({ enabled: true, reports: [FILED] }));
  const grid = screen.getByRole("table", { name: /reports this company has sent/i });
  expect(within(grid).getByText("Filed")).toBeTruthy();
  expect(within(grid).getByRole("link", { name: "#42" }).getAttribute("href")).toBe(FILED.issue);
  await act(async () => {
    fireEvent.click(within(grid).getByRole("button", { name: /what was sent for/i }));
    await settled();
  });
  expect(within(screen.getByRole("dialog")).getByText(/turn\.panicked/)).toBeTruthy();
});

test("a report the relay named no issue for draws no link", async () => {
  await mount(reading({ reports: [{ ...FILED, status: "failed", issue: "" }] }));
  const grid = screen.getByRole("table", { name: /reports this company has sent/i });
  expect(within(grid).getByText("Failed")).toBeTruthy();
  expect(within(grid).queryByRole("link")).toBeNull();
});

test("a preview shows the exact JSON a report of the class would send", async () => {
  await mount(reading());
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /a turn panicked/i }));
    await settled();
  });
  expect(screen.getByText(/internal\/agent\/turn\.Recovered/)).toBeTruthy();
});
