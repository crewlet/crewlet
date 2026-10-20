/**
 * Home's figures, all five or the few a board's Figure widget names.
 *
 * The row's columns are its figures' (`data-count`), so a figure drawn on its
 * own fills its cell rather than a fifth of it, and a lone figure's row is
 * named by that figure rather than as the whole company at a glance.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { KPI_IDS, KPI_LABELS, Kpis, type KpiId } from "./Kpis.tsx";
import { crewOf, rangeOf, DEFAULT_RANGE, type Waiting } from "./model.ts";
import { PARTIAL_FIGURE } from "./board/kit.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import { LiveSocket, Store, type WorkFlowAnswer } from "~/protocol/index.ts";

afterEach(cleanup);

/** A question that has not answered yet: each tile draws its label and waits. */
function pending<T>(): QueryResult<T> {
  return { data: null, loading: true, error: null, refusal: null, refetch: () => {} };
}

/** An admin bound to a seat, with the whole state a screen reads. */
const VIEWER: ViewerState = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet", "doclet"],
  admin: true,
  worklet: true,
  doclet: true,
  chatlet: true,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  acts: [],
  kind: "human",
  project: "",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
  configWriter: true,
  configManagedBy: [],
  assistant: "disabled",
  assistantModel: null,
  chatBackend: "",
  chatVendors: [],
};

function draw(
  figures?: readonly KpiId[],
  over: { waiting?: Waiting | null; flow?: QueryResult<WorkFlowAnswer> } = {},
) {
  const store = new Store();
  const socket = new LiveSocket(store);
  render(
    <Router>
      <ClientContext.Provider value={{ store, socket }}>
        <Kpis
          {...(figures ? { figures } : {})}
          range={rangeOf(DEFAULT_RANGE)}
          crew={crewOf([])}
          viewer={VIEWER}
          waiting={over.waiting ?? null}
          waitingError={null}
          flow={over.flow ?? pending()}
          spend={pending()}
          now={Date.parse("2026-09-22T09:00:00Z")}
        />
      </ClientContext.Provider>
    </Router>,
  );
}

/** Each tile's label, in the order the row draws them. */
const labels = () =>
  [...document.querySelectorAll(".home-kpis > .crewlet-statcard")].map(
    (tile) => tile.textContent ?? "",
  );

test("all five figures by default, in the row's order, named as the company at a glance", () => {
  draw();
  const row = screen.getByRole("region", { name: "The company at a glance" });
  expect(row.getAttribute("data-count")).toBe("5");
  const drawn = labels();
  expect(drawn).toHaveLength(KPI_IDS.length);
  KPI_IDS.forEach((id, i) => expect(drawn[i]).toContain(KPI_LABELS[id]));
});

test("only the figures named, in the order named", () => {
  draw(["tokens", "agents"]);
  const row = screen.getByRole("region", { name: "The company at a glance" });
  expect(row.getAttribute("data-count")).toBe("2");
  const drawn = labels();
  expect(drawn).toHaveLength(2);
  expect(drawn[0]).toContain(KPI_LABELS.tokens);
  expect(drawn[1]).toContain(KPI_LABELS.agents);
});

test("a figure on its own is named by that figure and fills its row", () => {
  draw(["waiting"]);
  const row = screen.getByRole("region", { name: KPI_LABELS.waiting });
  expect(row.getAttribute("data-count")).toBe("1");
  expect(labels()).toHaveLength(1);
});

/** A flow answer over a quiet fortnight, saying whether it accounted for everything. */
function flowAnswer(complete: boolean): QueryResult<WorkFlowAnswer> {
  const day = (i: number) => new Date(Date.parse("2026-09-09T00:00:00Z") + i * 86_400_000);
  const points = Array.from({ length: 14 }, (_, i) => ({
    window: day(i).toISOString().slice(0, 10),
    start: day(i).toISOString(),
    end: day(i + 1).toISOString(),
    not_started: 0,
    active: 0,
    done: 0,
    closed: 0,
    completed: 0,
  }));
  return {
    data: {
      bucket: "day",
      points,
      now: { not_started: 0, active: 0, blocked: 0, overdue: 0 },
      blocked_history: false,
      complete,
    },
    loading: false,
    error: null,
    refusal: null,
    refetch: () => {},
  };
}

// A FIGURE OVER AN ANSWER THAT COULD NOT ACCOUNT FOR EVERYTHING says so on
// its second line.
test.each([
  ["waiting", { waiting: { count: 0, floor: false, oldestAt: null, partial: true } }],
  ["progress", { flow: flowAnswer(false) }],
  ["completed", { flow: flowAnswer(false) }],
] as const)("the %s figure says it may be incomplete", (figure, over) => {
  draw([figure], over);
  const tile = document.querySelector(".home-kpis > .crewlet-statcard")!;
  expect(tile.textContent).toContain(PARTIAL_FIGURE);
});

test("a figure over a whole answer makes its claim", () => {
  draw(["waiting"], { waiting: { count: 0, floor: false, oldestAt: null, partial: false } });
  const tile = document.querySelector(".home-kpis > .crewlet-statcard")!;
  expect(tile.querySelector(".crewlet-statcard__number")?.textContent).toBe("0");
  expect(tile.textContent).not.toContain(PARTIAL_FIGURE);
  cleanup();
  draw(["progress"], { flow: flowAnswer(true) });
  expect(document.body.textContent).not.toContain(PARTIAL_FIGURE);
});
