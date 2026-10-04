/**
 * Which zone the fires after the next one are worked out in.
 *
 * The engine resolves a schedule's own timezone and evaluates the expression
 * there (`internal/schedule/entries.go`, `Expr.FireTimes`), so a reading aid
 * that worked the same expression out in UTC did not merely drift across a
 * daylight-saving change — it was wrong by the zone's STANDING offset on every
 * row of every company that does not run in UTC. This panel is the only place
 * in the dashboard that predicts a fire rather than reporting the one the
 * engine already computed, so it is the only place that can be wrong this way.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import {
  NextFires,
  OutcomeTag,
  ScheduleDefinition,
  Schedules,
  ScopeCell,
  Wakes,
  scheduleFacts,
} from "./Schedules.tsx";
import { act } from "@testing-library/react";
import { PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { Router } from "~/app/router.tsx";
import { setZone } from "~/lib/prefs.ts";
import type { ScheduleRow } from "~/protocol/index.ts";

beforeEach(() => {
  // The READER's zone, which is what each instant is rendered in and is a
  // different question from the one under test. Pinned so the assertions are
  // about the schedule's zone alone.
  setZone("UTC");
});

afterEach(() => {
  cleanup();
  setZone("");
});

const NOW = Date.parse("2026-06-15T00:30:00Z");

function row(over: Partial<ScheduleRow> = {}): ScheduleRow {
  return {
    scope_type: "role",
    scope_id: "ceo",
    name: "standup",
    cron: "0 9 * * *",
    timezone: "Asia/Tokyo",
    task: "run the standup",
    target: "",
    enabled: true,
    timeout_seconds: 600,
    catchup: false,
    runners: ["ceo"],
    next_run: "2026-06-16T00:00:00Z",
    ...over,
  };
}

// 09:00 IN TOKYO IS 00:00Z, and the nine-hour gap is the whole finding: the
// list used to be worked out in UTC and put every fire nine hours late, for
// ever, on a screen whose header carries the engine's own answer right above
// it.
test("the fires are worked out in the schedule's zone, not in UTC", () => {
  render(<NextFires row={row()} now={NOW} count={3} />);
  expect(screen.getAllByText(/00:00:00/).length).toBe(3);
  expect(screen.queryByText(/09:00:00/)).toBeNull();
  expect(screen.getByText(/evaluated in Asia\/Tokyo/)).toBeTruthy();
});

// AND A SCHEDULE THAT NAMES NO ZONE ARRIVES ON THE COMPANY'S CLOCK (ADR-0018):
// the engine resolves it, so the row names the company's zone and the fires are
// worked out there. The screen defaults nothing — a zone-less row read as UTC
// was nine hours out for a company in Tokyo — so a row that somehow named no
// zone draws no fires rather than a list on a clock the engine does not fire
// on.
test("a row is worked out in the zone the engine names, and never in a default", () => {
  render(<NextFires row={row({ timezone: "Asia/Tokyo" })} now={NOW} count={3} />);
  expect(screen.getAllByText(/00:00:00/).length).toBe(3);
  cleanup();
  const { container } = render(<NextFires row={row({ timezone: "" })} now={NOW} count={3} />);
  expect(container.textContent).toBe("");
});

// A LEDGER OUTCOME IS A WORD, AND NEITHER SKIP ASKS FOR ANYBODY. The raw enum
// sat in an amber pill on every row of a company whose node had been down
// overnight: `skipped_catchup` is the catchup cap doing its job, and amber is
// the one state that asks a person for something.
test("an outcome is named in words, and a skip is never the needs-you amber", () => {
  const { container } = render(
    <>
      <OutcomeTag outcome="fired" />
      <OutcomeTag outcome="skipped_catchup" />
      <OutcomeTag outcome="skipped_paused" />
    </>,
  );
  expect(container.textContent).not.toContain("_");
  expect(
    screen.getByText("skipped · missed").closest(".crewlet-tag")?.getAttribute("title"),
  ).toMatch(/catchup window/);
  expect(container.querySelector(".crewlet-tag--warning")).toBeNull();
});

// THE FACES ARE THE CHART'S BADGES. Handed the whole name the kit made "AS",
// "AI" and "AA" of three seats the chart draws as SW, FS and AS.
test("a stack of the seats a schedule wakes draws each seat's own badge", () => {
  const names: Record<string, string> = {
    swe: "Agent SWE",
    fs: "Agent Frontend SWE",
    as: "Agent AI Systems Engineer",
  };
  const who = (h: string) => ({ name: names[h]!, kind: "agent" as const });
  const { container } = render(
    <Router>
      <Wakes runners={["swe", "fs", "as"]} who={who} />
    </Router>,
  );
  const faces = [...container.querySelectorAll(".crewlet-avatar-stack__member")].map((el) =>
    el.textContent?.trim(),
  );
  expect(faces).toEqual(["SW", "FS", "AS"]);
});

// SEVERAL SEATS ARE A COUNT AND FACES. A chip per seat in a column that never
// wraps cut three names to "A" and "Age…"; the names are still said.
test("a schedule waking several seats says how many, and names them for a screen reader", () => {
  const who = (h: string) => ({ name: `Agent ${h.toUpperCase()}`, kind: "agent" as const });
  const { container } = render(
    <Router>
      <Wakes runners={["swe", "fs", "as"]} who={who} />
    </Router>,
  );
  expect(container.textContent).toContain("3 seats");
  expect(container.querySelector(".sr-only")?.textContent).toContain(
    "Agent SWE, Agent FS, Agent AS",
  );
  cleanup();
  render(
    <Router>
      <Wakes runners={["swe"]} who={who} />
    </Router>,
  );
  expect(screen.getByRole("link", { name: /Agent SWE/ }).getAttribute("href")).toBe(
    "#/agents/seats/swe",
  );
});

/*
 * A HANDLE IS AN ADDRESS, NOT A LABEL — on every surface of a schedule, not
 * only the grid that learnt it first. The schedule's own header joined the
 * raw handles it wakes ("agent-swe, agent-frontend-swe, agent…"), the company's
 * fires drew a role's scope as `agent-pm` in a pill one card below the grid
 * that drew it as "Agent PM", and the definition's Scope read `role ·
 * agent-pm`.
 */
const NAMES: Record<string, string> = { ceo: "Chief Executive", swe: "Agent SWE" };
const who = (h: string) => ({ name: NAMES[h] ?? h, kind: "agent" as const });

test("the header's Wakes names the seats, never their handles", () => {
  const wakes = scheduleFacts(row({ runners: ["ceo"] }), NOW, who).find(
    (fact) => fact.label === "Wakes",
  )!;
  const { container } = render(<Router>{wakes.value}</Router>);
  expect(container.textContent).toContain("Chief Executive");
  expect(container.textContent).not.toMatch(/\bceo\b/);
});

test("a role's scope is its seat by name, in every grid and in the definition", () => {
  const { container } = render(
    <Router>
      <ScopeCell scopeType="role" name="swe" who={who} />
      <ScopeCell scopeType="unit" name="Core" who={who} />
    </Router>,
  );
  expect(container.textContent).toContain("Agent SWE");
  expect(container.textContent).toContain("Core");
  expect(container.textContent).not.toMatch(/\bswe\b/);
  cleanup();
  render(
    <Router>
      <ScheduleDefinition row={row()} who={who} />
    </Router>,
  );
  expect(screen.getByText("Seat · Chief Executive")).toBeTruthy();
  expect(screen.queryByText(/role · /)).toBeNull();
});

// THE EXPRESSION'S CHIP HUGS ITS TEXT. In the column's flex stack it was
// stretched to the sentence under it, a box round blank space.
test("the cron cell does not stretch the expression to the sentence under it", () => {
  const read = (path: string) => readFileSync(join(process.cwd(), path), "utf8");
  const css = read("src/styles/screens.css").replace(/\/\*[\s\S]*?\*\//g, "");
  const rule = /\.cron-cell\s*\{([^}]*)\}/.exec(css)?.[1] ?? "";
  expect(rule).toMatch(/align-items:\s*flex-start/);
  const source = read("src/routes/agents/Schedules.tsx");
  expect(source).toContain('className="col cron-cell"');
});

/** The screen itself, over the chart's org and one schedule with one fire. */
async function mountScreen(scope?: string[]) {
  Object.defineProperty(globalThis, "WebSocket", {
    writable: true,
    value: class {
      readyState = 0;
      send(): void {}
      close(): void {}
    },
  });
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.applyOrg(CHART_ORG);
  const socket = new LiveSocket(store);
  const fire = {
    scope_type: "role",
    scope_id: "pm",
    schedule_name: "standup",
    fire_label: "2026-06-15T00:00:00Z",
    target_handle: "pm",
    scheduled_at: "2026-06-15T00:00:00Z",
    fired_at: "2026-06-15T00:00:01Z",
    outcome: "fired",
    trace_id: "",
  };
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "schedules"
        ? { schedules: [row({ scope_id: "pm", runners: ["pm"] })], recent_runs: [fire] }
        : what === "schedule_runs"
          ? { runs: [fire], truncated: false }
          : what === "viewer"
            ? { login: "", owner: "", acts: [] }
            : {},
    );
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <PeekNeighbours>
            <Schedules scope={scope} />
          </PeekNeighbours>
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
  return view;
}

// AND THE SCREEN USES IT IN BOTH GRIDS: the fires below the definitions drew
// a role's scope as its bare handle in a pill.
test("both grids name a role's scope by its seat", async () => {
  const { container } = await mountScreen();
  expect(container.textContent).toContain("Recent runs");
  const pills = [...container.querySelectorAll(".crewlet-tag")].map((el) => el.textContent);
  expect(pills).not.toContain("pm");
  // The seat, linked, four times: the definition's scope and whom it wakes,
  // and the fire's scope and whom it woke.
  const seat = [...container.querySelectorAll("a")].filter(
    (a) => a.getAttribute("href") === "#/agents/seats/pm",
  );
  expect(seat).toHaveLength(4);
});

// AND THE SCHEDULE'S OWN HEADER NAMES ITS SCOPE. Its eyebrow drew the address,
// `role:pm`, in the mono face — a line above a definition that named the same
// seat — on the one header this change had already taught to name the seats
// it wakes.
test("the schedule's eyebrow names its scope, never its address", async () => {
  const { container } = await mountScreen(["role", "pm", "standup"]);
  const eyebrow = container.querySelector(".object-eyebrow")?.textContent ?? "";
  expect(eyebrow).toContain("Seat · PM");
  expect(eyebrow).not.toContain("role:");
  expect(container.querySelector(".object-eyebrow .mono")).toBeNull();
});

// THE HEADER'S CRON IS THE GRID'S DRAWING, WHOLE. Inline after the chip and
// clamped to two lines, "every 20 minutes every day" was cut after its third
// word, while the grid one card below stacked the same value.
test("the header's cron fact stacks the expression over its sentence, unclamped", () => {
  const cron = scheduleFacts(row({ cron: "*/20 * * * *" }), NOW, who).find(
    (fact) => fact.label === "Cron",
  )!;
  expect(cron.whole).toBe(true);
  const { container } = render(<Router>{cron.value}</Router>);
  const stack = container.querySelector(".cron-cell");
  expect(stack?.firstElementChild?.textContent).toBe("*/20 * * * *");
  expect(stack?.lastElementChild?.textContent).toMatch(/^every 20 minutes/);
});

// A SERIES IS READ ROW AGAINST ROW. Rounded to one unit, the fires after the
// hour of a schedule every twenty minutes all read "in 1h".
test("the fires after the next one never read alike", () => {
  render(<NextFires row={row({ cron: "*/20 * * * *", timezone: "UTC" })} now={NOW} count={5} />);
  const labels = [...document.querySelectorAll(".fires li")].map(
    (li) => li.lastElementChild?.textContent,
  );
  expect(labels).toHaveLength(5);
  expect(new Set(labels).size).toBe(5);
});

/*
 * THE RUNS GRID FITS BESIDE A PEEK. Its schedule name was the one flexible
 * column among five sized to their content, so at 1280 with a schedule open
 * beside it the name was drawn 35px wide ("b.") while Scope repeated the seat
 * Woke named. The name keeps a floor now, and the facts give way around it:
 * Scope first, then the tick, then who it woke.
 */
describe("the recent runs grid beside a peek", () => {
  const HEAD = 120;
  let box = 0;
  beforeEach(() => {
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (
      this: HTMLElement,
    ) {
      return this.classList.contains("grid-wrap") ? box : 0;
    });
    // Every head cell 120px wide, laid end to end from the wrap's left edge.
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      const at = this.classList.contains("grid-th")
        ? [...this.parentElement!.children].indexOf(this)
        : -1;
      const right = at >= 0 ? (at + 1) * HEAD : this.classList.contains("grid-wrap") ? box : 0;
      return { left: 0, right, top: 0, bottom: 0, width: right, height: 0 } as DOMRect;
    });
  });
  afterEach(() => vi.restoreAllMocks());

  /** The runs grid: the one whose head names the tick. */
  const runsGrid = (container: HTMLElement) =>
    [...container.querySelectorAll<HTMLElement>(".grid-wrap")].find((g) =>
      g.textContent?.includes("For the tick"),
    )!;
  const heads = (grid: HTMLElement) =>
    [...grid.querySelectorAll(".grid-th")].map((h) => h.textContent);

  test("the schedule's name keeps its floor and the facts give way in order", async () => {
    box = 600; // six 120px heads: one has to go
    const { container } = await mountScreen();
    const grid = runsGrid(container);
    expect(grid.style.gridTemplateColumns).toMatch(/^\S+ minmax\(12rem, 1fr\)/);
    // And the outcome is drawn whole: a fifth of the grid cut both skips to
    // "skipped · …", the one word a skip row says.
    // One track per column: a `minmax(…, …)` holds a space of its own.
    const tracks = grid.style.gridTemplateColumns.match(/[\w-]+\([^)]*\)|\S+/g) ?? [];
    const outcome = heads(grid).indexOf("Outcome");
    expect(tracks[outcome]).toBe("max-content");
    expect(heads(grid)).not.toContain("Scope");
    expect(heads(grid)).toContain("For the tick");
    expect(heads(grid)).toContain("Woke");
  });

  test("narrower, the tick goes next and who it woke last", async () => {
    box = 360; // room for three: Fired, Schedule and Outcome, which never go
    const { container } = await mountScreen();
    const grid = runsGrid(container);
    expect(heads(grid).map((h) => h?.replace(/[^A-Za-z ]/g, "").trim())).toEqual([
      "Fired",
      "Schedule",
      "Outcome",
    ]);
    expect(grid.parentElement?.textContent).toContain(
      "Hidden to fit: Scope, For the tick and Woke",
    );
  });
});

// AND ONE COLUMN OF A SCHEDULE'S OWN FIRES FILLS ITS CARD. Every column was
// sized to its content, so the grid ended halfway across the card and its head
// band stopped over nothing.
test("the one schedule's fires fill their card on who each fire woke", async () => {
  const { container } = await mountScreen(["role", "pm", "standup"]);
  const fires = [...container.querySelectorAll<HTMLElement>(".grid-wrap")].find((g) =>
    g.textContent?.includes("Woke"),
  );
  expect(fires).toBeDefined();
  // Fired, then Woke — the one flexible track — then the two content-sized.
  expect(fires!.style.gridTemplateColumns).toMatch(/^\S+ minmax\(12rem, 1fr\) /);
});
