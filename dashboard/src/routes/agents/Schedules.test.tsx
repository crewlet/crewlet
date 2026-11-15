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
  CronReading,
  NextFires,
  OutcomeTag,
  ScheduleDefinition,
  Schedules,
  ScopeCell,
  Wakes,
  scheduleFacts,
} from "./Schedules.tsx";
import { PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { Router } from "~/app/router.tsx";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import type { ScheduleRow } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { afterMicrotasks } from "~/test/settle.ts";

beforeEach(() => {
  // The READER's zone, which is what each instant is rendered in and is a
  // different question from the one under test. Pinned so the assertions are
  // about the schedule's zone alone.
  pinZoneForTest("UTC");
});

afterEach(() => {
  cleanup();
  pinZoneForTest("");
});

const NOW = Date.parse("2026-06-15T00:30:00Z");

function row(over: Partial<ScheduleRow> = {}): ScheduleRow {
  return {
    scope_type: "role",
    // The scope's IDENTITY is the seat's agent id, which nothing shows; a
    // person reads its handle.
    scope_id: "0b6f6c1e-5d1a-5c8e-9a51-2f0c3e7a9b10",
    scope_name: "ceo",
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
  expect(screen.getAllByText(/09:00$/).length).toBe(3);
  expect(screen.getByText("next 3 runs, on Asia/Tokyo time")).toBeTruthy();
});

// ONE CLOCK, NAMED, AND THE READER'S ONLY BESIDE IT. The heading said "in
// Europe/Berlin" over times drawn on the reader's clock, and a footnote said
// they were the reader's: a reader in Los Angeles read 00:30 for a 09:30
// standup. Each fire is on the schedule's clock now, and the reader's own time
// is a second, labelled line where their clock reads it differently, and
// absent where it does not.
//
// Mutation: draw the rows with fmtDateTime again, and the Berlin 09:30 reads
// 00:30 under a heading naming Berlin.
test("each fire is on the schedule's clock, with the reader's time beside it only where it differs", () => {
  pinZoneForTest("America/Los_Angeles");
  const berlin = row({ cron: "30 9 * * *", timezone: "Europe/Berlin" });
  render(<NextFires row={berlin} now={NOW} count={3} />);
  expect(screen.getByText("next 3 runs, on Europe/Berlin time")).toBeTruthy();
  expect(screen.getAllByText(/09:30$/).length).toBe(3);
  expect(screen.getAllByText(/00:30 your time$/).length).toBe(3);
  expect(document.body.textContent).not.toContain("Shown in your time zone");
  cleanup();
  pinZoneForTest("Europe/Berlin");
  render(<NextFires row={berlin} now={NOW} count={3} />);
  expect(screen.getAllByText(/09:30$/).length).toBe(3);
  expect(screen.queryByText(/your time/)).toBeNull();
});

// A PAUSED SCHEDULE LISTS NO RUNS AHEAD. The card went on listing five times
// under a header whose Next said paused, as if the switch changed nothing.
test("a paused schedule's next-runs card says it is paused instead of listing runs", () => {
  render(<NextFires row={row({ enabled: false, next_run: "" })} now={NOW} count={3} />);
  expect(screen.queryAllByText(/09:00$/)).toEqual([]);
  expect(screen.getByText(/Nothing while it is paused/)).toBeTruthy();
});

// AND A SCHEDULE THAT NAMES NO ZONE ARRIVES ON THE COMPANY'S CLOCK (ADR-0018):
// the engine resolves it, so the row names the company's zone and the fires are
// worked out there. The screen defaults nothing — a zone-less row read as UTC
// was nine hours out for a company in Tokyo — so a row that somehow named no
// zone draws no fires rather than a list on a clock the engine does not fire
// on.
test("a row is worked out in the zone the engine names, and never in a default", () => {
  render(<NextFires row={row({ timezone: "Asia/Tokyo" })} now={NOW} count={3} />);
  expect(screen.getAllByText(/09:00$/).length).toBe(3);
  cleanup();
  const { container } = render(<NextFires row={row({ timezone: "" })} now={NOW} count={3} />);
  expect(container.textContent).toBe("");
});

// A FIRE'S OUTCOME IS A WORD, AND NO SKIP ASKS FOR ANYBODY. The raw enum sat
// in an amber pill on every row of a company whose node had been down
// overnight: `skipped_catchup` is the catchup cap doing its job, and amber is
// the one state that asks a person for something.
test("an outcome is named in words, and a skip is never the needs-you amber", () => {
  const { container } = render(
    <>
      <OutcomeTag outcome="fired" />
      <OutcomeTag outcome="fired_manually" />
      <OutcomeTag outcome="skipped_catchup" />
      <OutcomeTag outcome="skipped_paused" />
      <OutcomeTag outcome="skipped_superseded" />
    </>,
  );
  expect(container.textContent).not.toContain("_");
  expect(
    screen.getByText("skipped · missed").closest(".crewlet-tag")?.getAttribute("title"),
  ).toMatch(/catchup window/);
  // A RUN A PERSON ASKED FOR READS AS ONE, and as a run.
  const manual = screen.getByText("fired · run now").closest(".crewlet-tag");
  expect(manual?.getAttribute("title")).toMatch(/ran it now/);
  expect(manual?.className).toContain("crewlet-tag--success");
  expect(container.querySelector(".crewlet-tag--warning")).toBeNull();
});

// A FIRE A LATER ONE REPLACED SAYS SO, AND WHY. Fires that waited for a seat
// that could not work run once, as the latest, and the earlier ones are
// skipped: drawn as the engine's word they read as a column name, and drawn
// as `fired` they told a reader the duty ran once per waiting fire.
test("a fire a later fire ran in place of reads as skipped, with the reason", () => {
  const { container } = render(<OutcomeTag outcome="skipped_superseded" />);
  const tag = screen.getByText("skipped · replaced").closest(".crewlet-tag");
  expect(tag?.getAttribute("title")).toBe("A later fire of this schedule ran in its place");
  expect(container.querySelector(".crewlet-tag--success")).toBeNull();
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
    "#/company/org/seats/swe",
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
async function mountScreen(
  scope?: string[],
  over: {
    schedules?: unknown[];
    grants?: string[];
    /** The fires both histories answer with, in place of the one fire. */
    runs?: Record<string, unknown>[];
    /** Who reads the screen, in place of a person the engine serves nothing. */
    viewer?: Record<string, unknown>;
  } = {},
) {
  Object.defineProperty(globalThis, "WebSocket", {
    writable: true,
    value: class {
      readyState = 0;
      send(): void {}
      close(): void {}
    },
  });
  const store = new Store();
  store.applyHealth(healthFrame());
  store.applyOrg(CHART_ORG);
  const socket = new LiveSocket(store);
  const fire = {
    scope_type: "role",
    scope_id: "pm",
    scope_name: "pm",
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
        ? {
            schedules: over.schedules ?? [
              row({ scope_id: "pm", scope_name: "pm", runners: ["pm"] }),
            ],
            recent_runs: over.runs ?? (over.schedules ? [] : [fire]),
          }
        : what === "schedule_runs"
          ? { runs: over.runs ?? [fire], truncated: false }
          : what === "viewer"
            ? (over.viewer ?? { login: "ana", owner: "ana", acts: [], grants: over.grants ?? [] })
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
  await afterMicrotasks();
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
    (a) => a.getAttribute("href") === "#/company/org/seats/pm",
  );
  expect(seat).toHaveLength(4);
});

// AND THE SCHEDULE'S OWN HEADER NAMES ITS SCOPE. Its eyebrow drew the address,
// `role:pm`, in the mono face — a line above a definition that named the same
// seat — on the one header this change had already taught to name the seats
// it wakes.
test("the schedule's eyebrow names its scope, never its address", async () => {
  const { container } = await mountScreen(["role", "pm", "standup"]);
  const eyebrow = container.querySelector(".crewlet-object-header__eyebrow")?.textContent ?? "";
  expect(eyebrow).toContain("Seat · PM");
  expect(eyebrow).not.toContain("role:");
  expect(
    container.querySelector(".crewlet-object-header__eyebrow .crewlet-object-header__id--mono"),
  ).toBeNull();
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
      return this.classList.contains("crewlet-data-grid") ? box : 0;
    });
    // Every head cell 120px wide, laid end to end from the wrap's left edge.
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      const at = this.classList.contains("crewlet-data-grid__th")
        ? [...this.parentElement!.children].indexOf(this)
        : -1;
      const right =
        at >= 0 ? (at + 1) * HEAD : this.classList.contains("crewlet-data-grid") ? box : 0;
      return { left: 0, right, top: 0, bottom: 0, width: right, height: 0 } as DOMRect;
    });
  });
  afterEach(() => vi.restoreAllMocks());

  /** The runs grid: the one whose head names the tick. */
  const runsGrid = (container: HTMLElement) =>
    [...container.querySelectorAll<HTMLElement>(".crewlet-data-grid")].find((g) =>
      g.textContent?.includes("For the tick"),
    )!;
  const heads = (grid: HTMLElement) =>
    [...grid.querySelectorAll(".crewlet-data-grid__th")].map((h) => h.textContent);

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
  const fires = [...container.querySelectorAll<HTMLElement>(".crewlet-data-grid")].find((g) =>
    g.textContent?.includes("Woke"),
  );
  expect(fires).toBeDefined();
  // Fired, then Woke — the one flexible track — then the two content-sized.
  expect(fires!.style.gridTemplateColumns).toMatch(/^\S+ minmax\(12rem, 1fr\) /);
});

// NO FIGURES OVER NOTHING: three tiles reading 0 above an empty state that
// says no schedule is defined said it four times. And the empty state's one
// way on, the document a schedule is written in, is offered only to a reader
// who may open it.
test("no schedule draws no figures, and offers the configuration to a reader who may read it", async () => {
  await mountScreen(undefined, { schedules: [], grants: ["admin"] });
  expect(screen.getByText("No recurring work is defined")).toBeTruthy();
  expect(document.querySelector(".crewlet-stat-group")).toBeNull();
  expect(screen.getByRole("link", { name: "Open the configuration" }).getAttribute("href")).toBe(
    "#/settings/config",
  );
  cleanup();
  await mountScreen(undefined, { schedules: [], grants: ["worklet"] });
  expect(screen.queryByRole("link", { name: "Open the configuration" })).toBeNull();
});

/** A fire of `standup` on the seat `pm`, as both histories send one. */
function fire(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    scope_type: "role",
    scope_id: "pm",
    scope_name: "pm",
    schedule_name: "standup",
    fire_label: "20260615T0000",
    target_handle: "pm",
    scheduled_at: "2026-06-15T00:00:00Z",
    fired_at: "2026-06-15T00:00:01Z",
    outcome: "fired",
    trace_id: "",
    ...over,
  };
}

/** The grid whose heads name `head`, among a screen's grids. */
function gridWith(container: HTMLElement, head: string): HTMLElement {
  const grid = [...container.querySelectorAll<HTMLElement>(".crewlet-data-grid")].find((g) =>
    [...g.querySelectorAll('[role="columnheader"]')].some((h) => h.textContent?.includes(head)),
  );
  if (!grid) throw new Error(`no grid with a ${head} head`);
  return grid;
}

/** A grid's head cell named `head`. */
function headOf(grid: HTMLElement, head: string): HTMLElement {
  return [...grid.querySelectorAll<HTMLElement>('[role="columnheader"]')].find((h) =>
    h.textContent?.includes(head),
  )!;
}

// ONE SET OF FIRE COLUMNS FOR BOTH GRIDS. The schedule's own fires redeclared
// the company's by hand and drifted: the tick, a time, stood at the left edge
// there and the right edge here, and neither grid sorted by who a fire woke.
// Mutation: give the Fires grid its own tick column without `align`.
test("a schedule's fires draw the tick and who it woke as the company's runs do", async () => {
  const { container } = await mountScreen(["role", "pm", "standup"]);
  const fires = gridWith(container, "Woke");
  expect(headOf(fires, "For the tick").className).toContain("crewlet-data-grid__th--right");
  expect(headOf(fires, "Woke").querySelector(".crewlet-data-grid__sort")).not.toBeNull();
  cleanup();
  const list = await mountScreen();
  const runs = gridWith(list.container, "For the tick");
  expect(headOf(runs, "For the tick").className).toContain("crewlet-data-grid__th--right");
  expect(headOf(runs, "Woke").querySelector(".crewlet-data-grid__sort")).not.toBeNull();
});

// AN UNSET TICK IS NO TIME, NOT THE EPOCH. A fire recorded before the history
// kept its tick arrives at 1970-01-01, which read "Dec 31, 1969" in a zone west
// of UTC beside a Fired cell saying "10d ago". Mutation: test the tick for a
// non-empty string rather than parsing it.
test("a fire with no recorded tick draws the empty value, never the epoch", async () => {
  const { container } = await mountScreen(undefined, {
    runs: [fire({ scheduled_at: "1970-01-01T00:00:00Z", outcome: "skipped_catchup" })],
  });
  const runs = gridWith(container, "For the tick");
  expect(runs.textContent).not.toMatch(/1969|1970/);
  expect(runs.textContent).toContain("No time recorded");
});

// A SEAT'S CUT NAME IS READ WHOLE ON HOVER, with the handle beside it: the
// title was the handle alone, so a scope cut to "Community Moderator and L…"
// could only be read by opening it. Mutation: title linkedSeat by the handle.
test("a seat cell's hover gives its whole name and its handle", () => {
  render(
    <Router>
      <ScopeCell scopeType="role" name="swe" who={who} />
      <Wakes runners={["ceo"]} who={who} />
    </Router>,
  );
  expect(screen.getByRole("link", { name: /Agent SWE/ }).getAttribute("title")).toBe(
    "Agent SWE · @swe",
  );
  expect(screen.getByRole("link", { name: /Chief Executive/ }).getAttribute("title")).toBe(
    "Chief Executive · @ceo",
  );
});

// THE TASK IS SAID ONCE, in the definition: as the page's note as well it was
// the same paragraph twice in a row on a phone. And who it wakes takes two
// tracks of the fact line, never clamped, rather than one cut at 168px.
test("a schedule's page says its task once, and gives who it wakes room", async () => {
  const { container } = await mountScreen(["role", "pm", "standup"]);
  const said = container.textContent?.split("run the standup").length ?? 0;
  expect(said - 1).toBe(1);
  const wakes = scheduleFacts(row(), NOW, who).find((fact) => fact.label === "Wakes")!;
  expect(wakes.set).toBe(true);
});

// AN ADDRESS THAT NEVER NAMED A SCHEDULE IS NOT FOUND. With no definition and
// no fire there, the page claimed "This schedule was removed" under a "no
// longer declared" badge for a mistyped link. Removed is said only where a
// fire proves the schedule existed. Mutation: drop the runs check in OneSchedule.
test("an unknown schedule is Not Found, and only one with fires was removed", async () => {
  const { container } = await mountScreen(["role", "nope", "nothing"], { runs: [] });
  expect(container.textContent).not.toContain("no longer declared");
  expect(container.textContent).not.toContain("was removed");
  expect(container.textContent).toContain("there is no such screen");
  expect(screen.getByRole("link", { name: "All schedules" }).getAttribute("href")).toBe(
    "#/company/schedules",
  );
  cleanup();
  const removed = await mountScreen(["role", "pm", "retired"], {
    runs: [fire({ schedule_name: "retired" })],
  });
  expect(removed.container.textContent).toContain("no longer declared");
  expect(removed.container.textContent).toContain("This schedule was removed");
});

// A FIRE'S TICK IS ON ITS SCHEDULE'S CLOCK, WITH THE ZONE NAMED. The cell drew
// the reader's clock with nothing said, under a title holding the tick's label
// on the schedule's: a 09:00 standup in Tokyo read 00:00 beside a label of
// 0900. And a run a person asked for stood for no tick, and says so.
// Mutation: draw the tick with fmtDateTime again.
test("a fire's tick is drawn on its schedule's clock, with the zone named", async () => {
  const { container } = await mountScreen(undefined, {
    runs: [
      fire(),
      fire({
        outcome: "fired_manually",
        fire_label: "manual-0f3a",
        fired_at: "2026-06-15T03:00:00Z",
        scheduled_at: "2026-06-15T03:00:00Z",
      }),
    ],
  });
  const runs = gridWith(container, "For the tick");
  expect(runs.textContent).toMatch(/09:00 GMT\+9/);
  expect(runs.querySelector('[title*="Asia/Tokyo time"]')).not.toBeNull();
  expect(runs.textContent).toContain("by hand");
  expect(runs.textContent).toContain("fired · run now");
});

// THE SCHEDULE'S CONTROLS ARE DRAWN FOR EVERYBODY, each held with its reason
// where it cannot act: New schedule on the list, and Run now, Edit, Pause and
// Delete on a schedule's own page.
test("the list offers a new schedule and a schedule's page its own controls", async () => {
  await mountScreen();
  expect(screen.getByRole("button", { name: /New schedule/ })).toBeTruthy();
  cleanup();
  await mountScreen(["role", "pm", "standup"]);
  for (const name of [/Run now/, /Edit/, /Pause/, /Delete/]) {
    expect(screen.getByRole("button", { name })).toBeTruthy();
  }
});

// RUN NOW IS DRAWN FOR A READER WHO CANNOT RUN IT, disabled with the sentence
// that says what would change that: it is the one control here that makes a
// change through the act gate (`run_schedule`), so the write gate holds this
// suite to drawing it for each reader the gate refuses.
test.each([
  ["an anonymous reader", { login: "", acts: [], grants: [] }, WRITE_REASONS.anonymous],
  [
    "a person the engine does not serve",
    { login: "ana", owner: "ana", acts: [], grants: [] },
    WRITE_REASONS.not_served,
  ],
])("Run now is drawn for %s, disabled with the reason", async (_who, viewer, reason) => {
  await mountScreen(["role", "pm", "standup"], { viewer });
  const button = screen.getByRole("button", { name: /Run now/ });
  expect(button.getAttribute("aria-disabled")).toBe("true");
  const said = (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
  expect(said).toContain(reason);
});

// THE CRON'S SENTENCE IS THE SCHEDULE'S CLOCK, said where the reader's reads
// differently: "at 09:30 on weekdays" under a reader in Los Angeles read as
// their morning. Mutation: drop the zone from CronReading's sentence.
test("a cron's sentence names the schedule's clock where the reader's differs", () => {
  pinZoneForTest("America/Los_Angeles");
  const { container } = render(
    <CronReading cron="30 9 * * 1-5" timezone="Europe/Berlin" now={NOW} />,
  );
  expect(container.textContent).toContain("Berlin time");
  cleanup();
  pinZoneForTest("Europe/Berlin");
  const same = render(<CronReading cron="30 9 * * 1-5" timezone="Europe/Berlin" now={NOW} />);
  expect(same.container.textContent).not.toContain("time");
});
