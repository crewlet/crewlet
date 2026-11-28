/**
 * Settings › Diagnostics. The invariants worth breaking a build over: it is off
 * until somebody turns it on, and says what is sent and what never is before they
 * do; the write is the one setting, checked by the engine first and on the
 * revision just read, through the one setting write; a build that sends
 * nothing says so once it is on; and a reader who may not write cannot move it.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { Router } from "~/app/router.tsx";
import type { IssueReportInert, IssueReportsAnswer } from "~/protocol/issueReports.ts";
import type { InsightsAnswer } from "~/protocol/insights.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { settled } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { toastText } from "~/testing.tsx";
import { APPLYING_SENTENCE } from "~/lib/savedToast.ts";
import { Diagnostics } from "./Diagnostics.tsx";

const save = vi.hoisted(() => vi.fn());
const read = vi.hoisted(() => vi.fn());
const check = vi.hoisted(() => vi.fn());
vi.mock("~/protocol/configWrite.ts", async (original) => ({
  ...(await original<typeof import("~/protocol/configWrite.ts")>()),
  getConfig: read,
  savePatch: save,
  dryRunPatch: check,
}));

const access = vi.hoisted(() => vi.fn());
vi.mock("~/lib/useWriteAccess.ts", async (original) => ({
  ...(await original<typeof import("~/lib/useWriteAccess.ts")>()),
  useConfigWriteAccess: access,
}));

const FILED = {
  class: "engine.panicked",
  title: "The engine panicked",
  fingerprint: "ab".repeat(32),
  first_sent: "2026-11-06T07:00:00Z",
  last_sent: "2026-11-06T07:00:00Z",
  status: "filed",
  issue: "https://github.com/crewlet/crewlet/issues/42",
  payload: { schema: 1, class: "engine.panicked" },
} as const;

function reading(over: Partial<IssueReportsAnswer> = {}): IssueReportsAnswer {
  return {
    enabled: false,
    inert: "",
    relay: "https://reports.crewlet.ai/v1/reports",
    reports: [],
    previews: [
      {
        class: "engine.panicked",
        title: "The engine panicked",
        summary: "A panic was recovered.",
        payload: { schema: 1, site: "internal/agent/turn.Recovered" },
      },
    ],
    ...over,
  };
}

/** Usage insights, off and empty: this file is about bug reports. */
const NO_INSIGHTS: InsightsAnswer = {
  share: "off",
  accepted: 0,
  schema: 1,
  always: [],
  relay: "https://reports.crewlet.ai/v1/insights",
  catalogue: { schema: 1, classes: [] },
  records: [],
};

/** The screen, over a socket answering `answer`. */
async function mount(answer: IssueReportsAnswer) {
  location.hash = "#/settings/diagnostics";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "viewer" ? ADMIN_VIEWER : what === "diagnostics_insights" ? NO_INSIGHTS : answer,
    );
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <ToastProvider>
            <Router>
              <Diagnostics />
            </Router>
          </ToastProvider>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  await settled();
  return view;
}

const theSwitch = () =>
  screen.getByRole("switch", { name: /send anonymous bug reports/i }) as HTMLInputElement;

beforeEach(() => {
  vi.clearAllMocks();
  access.mockReturnValue({ can: true });
  read.mockResolvedValue({ kind: "document", document: {}, etag: '"rev"' });
  check.mockResolvedValue({ kind: "valid", warnings: [], derived: null });
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
  expect(screen.getByText("What’s sent")).toBeTruthy();
  expect(screen.getByText("What’s never sent")).toBeTruthy();
  expect(screen.getByText(/Error messages, logs, paths on your servers/)).toBeTruthy();
  expect(screen.getByText(/up to five lines of Crewlet's code that led there/)).toBeTruthy();
});

// THE GUIDE'S LINK SITS INSIDE THE NOTE'S SENTENCE, so it carries the
// underline register: the kit's link marks itself by ink alone until hovered,
// 1.18:1 against the words around it in the dark theme. Mutation: drop
// `.prose-link` from it.
test("the link inside the note's sentence is underlined as one", async () => {
  await mount(reading());
  const link = screen.getByRole("link", { name: /How it works/ });
  expect(link.classList.contains("prose-link")).toBe(true);
});

test("turning it on writes the one setting, on the revision just read, with its summary", async () => {
  await mount(reading());
  await act(async () => {
    fireEvent.click(theSwitch());
    await settled();
  });
  // CHECKED FIRST, as every setting is: the change and the company as it
  // stands, both on the revision just read.
  expect(check.mock.calls.map((call) => call.slice(0, 2))).toEqual([
    [{}, '"rev"'],
    [{ diagnostics: { issue_reports: { enabled: true } } }, '"rev"'],
  ]);
  expect(save).toHaveBeenCalledOnce();
  const [patch, etag, summary] = save.mock.calls[0] ?? [];
  expect(patch).toEqual({ diagnostics: { issue_reports: { enabled: true } } });
  expect(etag).toBe('"rev"');
  expect(summary).toBe("Turned on bug reports to Crewlet");
  expect(theSwitch().checked).toBe(true);
  // CONFIRMED BY ITS TOAST, watched until this node applies it; no line on the card.
  expect(toastText()).toContain("Turned on bug reports to Crewlet");
  expect(toastText()).toContain(APPLYING_SENTENCE);
  expect(screen.queryByText(/Saved, applying/)).toBeNull();
});

// A WARNING THE CHANGE INTRODUCES STOPS THE SAVE, as it does for every other
// setting: the switch wrote straight through and never asked.
test("a warning the change introduces stops the save until Save anyway", async () => {
  const warning = { path: "diagnostics.issue_reports", message: "reports go to a public tracker" };
  check.mockImplementation(async (patch: Record<string, unknown>) => ({
    kind: "valid",
    warnings: Object.keys(patch).length === 0 ? [] : [warning],
    derived: null,
  }));
  await mount(reading());
  await act(async () => {
    fireEvent.click(theSwitch());
    await settled();
  });
  expect(save).not.toHaveBeenCalled();
  expect(screen.getByText("This change brings 1 warning")).toBeTruthy();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save anyway" }));
    await settled();
  });
  expect(save).toHaveBeenCalledOnce();
});

test("the document already saying so saves nothing", async () => {
  read.mockResolvedValue({
    kind: "document",
    document: { diagnostics: { issue_reports: { enabled: true } } },
    etag: '"rev"',
  });
  await mount(reading());
  await act(async () => {
    fireEvent.click(theSwitch());
    await settled();
  });
  expect(save).not.toHaveBeenCalled();
  expect(check).not.toHaveBeenCalled();
});

// A DISABLED INPUT DROPS FOCUS, so disabling the switch while it saves left a
// keyboard reader's next Space going nowhere. Mutation: set `disabled` while saving.
test("the switch keeps focus through a save, and a press during one is ignored", async () => {
  let finish: (value: unknown) => void = () => {};
  save.mockReturnValue(new Promise((resolve) => (finish = resolve)));
  await mount(reading());
  const toggle = theSwitch();
  toggle.focus();
  await act(async () => {
    fireEvent.click(toggle);
    await settled();
  });
  expect(document.activeElement).toBe(toggle);
  expect(toggle.disabled).toBe(false);
  await act(async () => {
    fireEvent.click(toggle);
    await settled();
  });
  expect(save).toHaveBeenCalledOnce();
  await act(async () => {
    finish({ kind: "saved", revisionId: "r", epoch: 2, warnings: [], derived: null });
    await settled();
  });
  expect(document.activeElement).toBe(toggle);
  expect(toggle.checked).toBe(true);
});

test("a refused write says so, and the switch stays where the engine has it", async () => {
  save.mockResolvedValue({ kind: "draining", detail: "" });
  await mount(reading());
  await act(async () => {
    fireEvent.click(theSwitch());
    await settled();
  });
  expect(screen.getByText("Nothing was saved")).toBeTruthy();
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
  ["modified", /local code changes/],
  ["no_commit", /which version of the code it came from/],
])("a %s build says plainly that it sends nothing, once the switch is on", async (inert, words) => {
  const on = await mount(reading({ enabled: true, inert }));
  expect(screen.getByText("This build won't send reports")).toBeTruthy();
  expect(screen.getByText(words)).toBeTruthy();
  on.unmount();

  await mount(reading({ enabled: false, inert }));
  expect(screen.queryByText("This build won't send reports")).toBeNull();
});

test("a build that reports says nothing of the kind", async () => {
  await mount(reading({ enabled: true, inert: "" }));
  expect(screen.queryByText("This build won't send reports")).toBeNull();
});

test("the list names each report's status and issue, and opens the bytes it sent", async () => {
  await mount(reading({ enabled: true, reports: [FILED] }));
  const grid = screen.getByRole("table", { name: /bug reports you've sent/i });
  expect(within(grid).getByText("Reported")).toBeTruthy();
  expect(within(grid).getByRole("link", { name: /#42/ }).getAttribute("href")).toBe(FILED.issue);
  await act(async () => {
    fireEvent.click(within(grid).getByRole("button", { name: /see what was sent for/i }));
    await settled();
  });
  expect(within(screen.getByRole("dialog")).getByText(/engine\.panicked/)).toBeTruthy();
});

test("a report the relay named no issue for draws no link", async () => {
  await mount(reading({ reports: [{ ...FILED, status: "failed", issue: "" }] }));
  const grid = screen.getByRole("table", { name: /bug reports you've sent/i });
  expect(within(grid).getByText("Didn't send")).toBeTruthy();
  expect(within(grid).queryByRole("link")).toBeNull();
});

test("a crash kept private says why, and offers the private report rather than a public one", async () => {
  const held = {
    ...FILED,
    class: "process.crashed",
    title: "The engine crashed",
    status: "held",
    issue: "",
    payload: { schema: 2, class: "process.crashed" },
  } as const;
  await mount(reading({ enabled: true, reports: [held] }));
  const grid = screen.getByRole("table", { name: /bug reports you've sent/i });
  expect(within(grid).getByText("Kept private")).toBeTruthy();
  await act(async () => {
    fireEvent.click(within(grid).getByRole("button", { name: /see what was kept for/i }));
    await settled();
  });
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("This crash wasn't sent")).toBeTruthy();
  expect(within(dialog).getByRole("button", { name: /copy the report/i })).toBeTruthy();
  expect(
    within(dialog)
      .getByRole("link", { name: /report privately on github/i })
      .getAttribute("href"),
  ).toBe("https://github.com/crewlet/crewlet/security/advisories/new");
  expect(within(dialog).getByText(/process\.crashed/)).toBeTruthy();
});

test("a preview shows the exact JSON a report of the class would send", async () => {
  await mount(reading());
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /the engine panicked/i }));
    await settled();
  });
  expect(screen.getByText(/internal\/agent\/turn\.Recovered/)).toBeTruthy();
});

// A CLASS'S SENTENCE NAMES A FACT OF THE REPORT IN BACKTICKS, as the
// catalogue writes it ("The `fatal_kind` says what it was"): code, beside
// the JSON that carries it, never the punctuation it travelled in.
test("a preview's sentence draws a fact it names as code", async () => {
  await mount(
    reading({
      previews: [
        {
          class: "process.crashed",
          title: "The engine crashed",
          summary: "The `fatal_kind` says what it was.",
          payload: { schema: 2, facts: { fatal_kind: "concurrent_map" } },
        },
      ],
    }),
  );
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /the engine crashed/i }));
    await settled();
  });
  expect(screen.getByText("fatal_kind", { selector: "code" })).toBeTruthy();
  expect(document.body.textContent).not.toContain("`fatal_kind`");
});
