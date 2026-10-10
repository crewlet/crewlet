/**
 * "Tokens by team" split by seat: each team's bar divided by the seats that
 * spent in it and keyed under the bar by the seat's name, a big team's tail
 * folded into one part in the residual hue, and the residual row left whole.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { DATA_COLOR_OTHER, DATA_COLORS } from "@crewlethq/ui";
import { ClientContext } from "~/lib/store-hooks.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import { LiveSocket, Store, type SeriesBand, type TokenSeries } from "~/protocol/index.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { HOME_RANGES } from "./model.ts";
import { seatParts, TokensByTeam } from "./TokensByTeam.tsx";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(cleanup);

/** A bucket of `total` tokens, as the engine's rows carry one. */
function tokens(total: number) {
  return { input_tokens: total, output_tokens: 0, total_tokens: total, calls: 1 };
}

function part(handle: string, total: number) {
  return { group: handle, handle, ...tokens(total) };
}

function band(group: string, total: number, rest: Partial<SeriesBand> = {}): SeriesBand {
  return { group, other: false, folded: 0, ...tokens(total), ...rest };
}

function mount(byGroup: SeriesBand[]) {
  const store = new Store();
  store.applyOrg(CHART_ORG);
  const socket = new LiveSocket(store);
  const total = byGroup.reduce((sum, b) => sum + b.total_tokens, 0);
  const spend = {
    data: {
      group: "unit",
      bucket: "day",
      series: [],
      by_group: byGroup,
      totals: tokens(total),
      grouped: tokens(total),
    } as unknown as TokenSeries,
    loading: false,
    error: null,
  } as QueryResult<TokenSeries>;
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <TokensByTeam spend={spend} range={HOME_RANGES[1]} />
    </ClientContext.Provider>,
  );
}

// A TEAM'S BAR IS ITS SEATS: the bar keeps the team's length, and is divided
// by the seats that spent in it, each keyed under it by the name the chart
// gives the seat today.
test("each team's bar is split by its seats and keyed by their names", () => {
  const { container } = mount([
    band("Core", 900, { seats: 2, parts: [part("swe", 600), part("fe", 300)] }),
    band("Management", 300, { seats: 1, parts: [part("pm", 300)] }),
  ]);
  const bars = [...container.querySelectorAll<HTMLElement>(".crewlet-bar-list__bar")];
  expect(bars.map((bar) => bar.style.width)).toEqual(["100%", `${(300 / 900) * 100}%`]);
  const core = [...bars[0]!.querySelectorAll<HTMLElement>(".crewlet-bar-list__part")];
  expect(core.map((p) => p.style.flexGrow)).toEqual(["600", "300"]);
  const keys = [...container.querySelectorAll(".crewlet-bar-list__key")].map((key) =>
    [...key.querySelectorAll("li")].map((item) => item.textContent),
  );
  expect(keys).toEqual([["SWE600", "FE300"], ["PM300"]]);
});

// THE RESIDUAL ROW IS SEVERAL TEAMS, so it is one bar in the residual hue
// rather than a split of seats from teams the card does not name; a team the
// engine answered without its seats is one bar in the first hue, as before.
test("the residual row and a team answered without its seats are drawn whole", () => {
  const { container } = mount([
    band("Core", 900, { seats: 2 }),
    band("", 100, { other: true, folded: 3 }),
  ]);
  expect(container.querySelectorAll(".crewlet-bar-list__part")).toHaveLength(0);
  expect(container.querySelectorAll(".crewlet-bar-list__key")).toHaveLength(0);
  const bars = [...container.querySelectorAll<HTMLElement>(".crewlet-bar-list__bar")];
  expect(bars.map((bar) => bar.style.getPropertyValue("--crewlet-bar-list-bar-color"))).toEqual([
    DATA_COLORS[0],
    DATA_COLOR_OTHER,
  ]);
  expect(container.textContent).toContain("Everyone else");
});

// FOUR HUES ARE HELD APART, so a team of five or more is its three biggest
// seats and the rest as one part in the residual hue; four or fewer are all
// named. A seat the chart no longer has is named by the key it spent under.
test("a team of more seats than hues names its three biggest and folds the rest", () => {
  const nameOf = (key: string) => key.toUpperCase();
  const five = [part("a", 50), part("b", 40), part("c", 30), part("d", 20), part("e", 10)];
  expect(seatParts(five, nameOf).map((p) => [p.label, p.value, p.color])).toEqual([
    ["A", 50, undefined],
    ["B", 40, undefined],
    ["C", 30, undefined],
    ["2 more agents", 30, DATA_COLOR_OTHER],
  ]);
  expect(seatParts(five.slice(0, 4), nameOf).map((p) => p.label)).toEqual(["A", "B", "C", "D"]);
  expect(seatParts([{ group: "Gone", ...tokens(5) }], nameOf)[0]!.label).toBe("GONE");
});
