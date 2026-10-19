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
import { crewOf, rangeOf, DEFAULT_RANGE } from "./model.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

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
};

function draw(figures?: readonly KpiId[]) {
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
          waiting={null}
          waitingError={null}
          flow={pending()}
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
