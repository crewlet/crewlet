/** Settings › Advisor's analyst: off says how to turn it on; a run that did not end clean never
 *  reads as all clear; Run now and each answer use the engine's routes; "Nothing found" is said
 *  only for a fresh, whole, clean run. */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import type { AdvisorAnalysisAnswer, AnalystFinding } from "~/protocol/advisor.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { settled } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { AdvisorAnalyst } from "./AdvisorAnalyst.tsx";

const post = vi.hoisted(() => vi.fn());
vi.mock("~/protocol/rest.ts", async (original) => {
  const actual = await original<typeof import("~/protocol/rest.ts")>();
  return { ...actual, rest: { ...actual.rest, post } };
});

afterEach(() => {
  cleanup();
  post.mockReset();
});

const FINDING: AnalystFinding = {
  class: "analyst.finding",
  severity: "costs",
  actor: "owner",
  source: "analyst",
  area: "work",
  subject: { kind: "seat", seat: "qa", name: "QA" },
  title: "One run can spend most of QA's daily budget.",
  why: "One more run would stop it.",
  item: "900K of 1M tokens",
  path: "",
  evidence: [{ key: "tokens", value: 900000 }],
  records: [{ kind: "turn", id: "turn-9" }],
  fix: { kind: "open_page", page: "seat", target: "qa", sentence: "Open the agent.", options: [] },
  fingerprint: "f1",
};

function answer(over: Partial<AdvisorAnalysisAnswer>): AdvisorAnalysisAnswer {
  return {
    on: true,
    model: "assistant",
    cadence: "daily",
    hour: 6,
    ceiling: 200000,
    stale: false,
    findings: [],
    set_aside: 0,
    last: {
      id: "r1",
      status: "done",
      started: new Date().toISOString(),
      trigger: "schedule",
      model: "assistant",
      ceiling: 200000,
      input_tokens: 30000,
      output_tokens: 2000,
      dropped: {},
      unread: [],
    },
    ...over,
  };
}

async function mount(a: AdvisorAnalysisAnswer) {
  location.hash = "#/settings/advisor";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "viewer" ? ADMIN_VIEWER : a);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <AdvisorAnalyst />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  await settled();
}

test("an analyst that is off says how to turn it on, suggesting Ask Crewlet's entry", async () => {
  await mount({ ...answer({}), on: false, last: undefined, suggest: "assistant" });
  expect(screen.getByText("The analyst is off")).toBeTruthy();
  expect(screen.getByText("advisor.analyst.llm")).toBeTruthy();
  expect(screen.getByText("assistant")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Run now" })).toBeNull();
});

test("run now and an answer are sent as the engine's routes", async () => {
  post.mockResolvedValue({});
  await mount(answer({ findings: [FINDING] }));
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Run now" }));
    await settled();
  });
  expect(post).toHaveBeenCalledWith("/advisor/analysis", {});
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Details/ }));
    await settled();
  });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Wrong" }));
    await settled();
  });
  expect(post).toHaveBeenCalledWith("/advisor/analysis/answers", {
    fingerprint: "f1",
    verdict: "wrong",
  });
});

test("a run that stopped at its ceiling says so, and never says nothing was found", async () => {
  const base = answer({});
  await mount({
    ...base,
    last: { ...base.last!, status: "ceiling", reason: "The run stopped at its ceiling." },
  });
  expect(screen.getByText("The last run stopped at its token ceiling")).toBeTruthy();
  expect(screen.queryByText("Nothing found")).toBeNull();
});

test("nothing found is said only for a fresh, whole, clean run", async () => {
  await mount(answer({}));
  expect(screen.getByText("Nothing found")).toBeTruthy();
  cleanup();
  await mount(answer({ stale: true }));
  expect(screen.queryByText("Nothing found")).toBeNull();
  expect(screen.getByText("The last run is out of date")).toBeTruthy();
});
