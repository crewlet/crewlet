/**
 * THE ATLAS PAGE, LAID OUT, in a real browser: every lane drawn with what
 * would fill an empty one, the map opening with all of it in view (or on the
 * entry a link names), the selection docked on the map and marked in the
 * catalogue under it, a large company opening with its groups closed, two
 * thousand entries drawn without mounting each, and no screen that scrolls
 * sideways, on a wide screen and on a phone.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { graphItemKey } from "@crewlethq/ui";
import { Atlas } from "./Atlas.tsx";
import { InertWebSocket, LayoutClient, sidewaysScrollers } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import {
  activityAnswer,
  entriesAnswer,
  facetsAnswer,
  generatedCompany,
  grownCompany,
  mapAnswer,
  sparseCompany,
  type AtlasFixture,
} from "~/test/atlas.ts";

const VIEWER = {
  login: "U0FOUNDER",
  grants: ["admin"],
  admin: true,
  configWriter: true,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
};

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }],
  units: [],
};

const FRAME: Record<string, unknown> = {
  viewer: VIEWER,
  inbox: { handle: "jane", notices: [], sources: [], unread: 0, primary: 0, complete: true },
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  answered_decisions: { handle: "jane", items: [] },
};

// THE ROWS OF THE MAP, read in order: what a request travels, what that
// stands on, and what stands around it.
const LANES = [
  "Entry",
  "Services",
  "Data",
  "Runtime",
  "Platforms",
  "Code and delivery",
  "Observability",
  "Tools and models",
];

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.unstubAllGlobals();
  location.hash = "";
});

async function mount(
  width: number,
  fixture: AtlasFixture,
  hash = "#/company/atlas",
  narrow: (params: Record<string, unknown>) => AtlasFixture = () => fixture,
) {
  location.hash = hash;
  await page.viewport(width, 900);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px; height: 900px; display: flex; flex-direction: column;`;
  document.body.append(host);
  root = createRoot(host);
  const ask = (what: string, params: Record<string, unknown>) =>
    what === "atlas_map"
      ? mapAnswer(narrow(params))
      : what === "atlas_facets"
        ? facetsAnswer(fixture)
        : what === "atlas_entries"
          ? entriesAnswer(fixture, 50, 0, "health")
          : what === "atlas_activity"
            ? activityAnswer(fixture)
            : undefined;
  flushSync(() =>
    root!.render(
      <LayoutClient answers={FRAME} ask={ask} org={ORG}>
        <SessionReading>
          <Atlas />
        </SessionReading>
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
}

const map = () => document.querySelector<HTMLElement>(".crewlet-graph-canvas");
/** What the map draws for a reader: never the probes it measures its parts with. */
const drawn = (selector: string) =>
  [...document.querySelectorAll<HTMLElement>(selector)].filter(
    (el) => !el.closest(".crewlet-graph-canvas__probe, .crewlet-graph-canvas__empty-probe"),
  );
const texts = (selector: string) => drawn(selector).map((el) => el.textContent?.trim() ?? "");

test.each([1440, 768, 390])(
  "a new company with only GitHub draws every lane at %ipx",
  async (width) => {
    await mount(width, sparseCompany());
    await eventually(() => expect(texts(".crewlet-graph-canvas__lane-label")).toEqual(LANES));
    await shownMap();
    // AN EMPTY LANE SAYS WHAT IN THIS BUILD WOULD FILL IT (drawn from card size
    // up, an outline under it), and a filled one draws its entries at whatever
    // level of detail the fitted zoom has: names on cards and tiles, marks past them.
    const empty = texts(".crewlet-graph-canvas__empty").join(" ");
    expect(empty).toContain(
      "Services appear when a person adds one, with Add entry, and containers from the servers the SSH integration reads for Atlas.",
    );
    expect(empty).toContain("Servers appear from the SSH servers the SSH integration declares.");
    expect(empty).not.toContain("Repositories appear");
    if (
      document.querySelector<HTMLElement>(".crewlet-graph-canvas__world")!.dataset.detail ===
      "marks"
    ) {
      const marks = [...document.querySelectorAll(".crewlet-graph-canvas__shapes path")].map(
        (path) => path.getAttribute("d")?.match(/M/g)?.length ?? 0,
      );
      expect(marks.reduce((sum, n) => sum + n, 0)).toBeGreaterThanOrEqual(3);
    } else {
      expect(texts(".crewlet-graph-canvas__name")).toEqual(
        expect.arrayContaining(["acme/web", "acme/api", "acme/infra"]),
      );
    }
    // THE MAP TAKES ITS SHARE OF THE WINDOW, the catalogue follows under it,
    // and nothing scrolls sideways.
    const picture = map()!.getBoundingClientRect();
    expect(picture.height).toBeGreaterThan(300);
    const catalogue = document.querySelector(".atlas-catalogue")!.getBoundingClientRect();
    expect(catalogue.top).toBeGreaterThanOrEqual(picture.bottom);
    expect(sidewaysScrollers(host!)).toEqual([]);
  },
);

/** The map's canvas, once it has been fitted and shown. */
async function shownMap(): Promise<DOMRect> {
  await eventually(() =>
    expect(document.querySelector(".crewlet-canvas")?.getAttribute("data-ready")).toBe("true"),
  );
  await eventually(() =>
    expect(
      document.querySelector<HTMLElement>(".crewlet-graph-canvas__world")?.dataset.detail,
    ).toBeDefined(),
  );
  return document.querySelector(".crewlet-canvas__viewport")!.getBoundingClientRect();
}

/** Whether `inner` is drawn inside `outer`, to half a pixel. */
const inside = (inner: DOMRect, outer: DOMRect) =>
  inner.left >= outer.left - 0.5 &&
  inner.top >= outer.top - 0.5 &&
  inner.right <= outer.right + 0.5 &&
  inner.bottom <= outer.bottom + 0.5;

/** How much of the canvas the whole map takes along the side it fills. */
function filling(canvas: DOMRect): number {
  const world = document.querySelector(".crewlet-graph-canvas__world")!.getBoundingClientRect();
  return Math.max(world.width / canvas.width, world.height / canvas.height);
}

// THE MAP OPENS FITTED: a company too large to read whole at half size has
// every cluster in view the moment the map is shown, centred, drawn at the
// level of detail its zoom has, never as blank boxes. Mutation: open such a
// map on its start at three quarters, as the kit did, and the clusters on the
// right and below run off the canvas.
test.each([1440, 390])(
  "a company of a hundred and twenty opens with all of its map in view at %ipx",
  async (width) => {
    await mount(width, generatedCompany(120));
    await eventually(() => expect(texts(".crewlet-graph-canvas__lane-label")).toEqual(LANES));
    const canvas = await shownMap();
    const lanes = drawn(".crewlet-graph-canvas__lane");
    expect(lanes).toHaveLength(LANES.length);
    for (const lane of lanes)
      expect(inside(lane.getBoundingClientRect(), canvas), "a cluster is in view").toBe(true);
    const world = document.querySelector(".crewlet-graph-canvas__world")!;
    const box = world.getBoundingClientRect();
    expect(inside(box, canvas), "the whole map is in view").toBe(true);
    expect(Math.abs(box.left - canvas.left - (canvas.right - box.right))).toBeLessThan(1);
    const detail = (world as HTMLElement).dataset.detail;
    expect(["tiles", "marks"]).toContain(detail);
    if (detail === "tiles") {
      for (const name of drawn(".crewlet-graph-canvas__items .crewlet-graph-canvas__name")) {
        expect(name.textContent).not.toBe("");
      }
    } else {
      expect(
        document.querySelector(".crewlet-graph-canvas__shapes [data-shape='card']"),
      ).not.toBeNull();
    }
    expect(sidewaysScrollers(host!)).toEqual([]);
  },
);

// A FILTER FITS THE MAP IT ANSWERS: the last answer stays on screen while the
// new one is read, so a fit keyed on the question landed on the old map and
// left the new one small in a corner of it. Mutation: key the fit on the
// question again and the narrowed map fills a fraction of the canvas.
test("a filter fits the map to the answer it narrows to, not the one it replaces", async () => {
  const company = generatedCompany(120);
  const alerting = {
    ...company,
    nodes: company.nodes.filter((node) => node.health === "alerting"),
  };
  await mount(1440, company, "#/company/atlas", (params) =>
    params.health === "alerting" ? alerting : company,
  );
  const canvas = await shownMap();
  const whole = document
    .querySelector(".crewlet-graph-canvas__world")!
    .getBoundingClientRect().width;
  location.hash = "#/company/atlas?health=alerting";
  await eventually(() =>
    expect(
      drawn(".crewlet-graph-canvas__items .crewlet-graph-canvas__card").length,
    ).toBeLessThanOrEqual(alerting.nodes.length),
  );
  await eventually(() => expect(filling(canvas)).toBeGreaterThan(0.9));
  expect(
    document.querySelector(".crewlet-graph-canvas__world")!.getBoundingClientRect().width,
  ).not.toBe(whole);
  for (const lane of drawn(".crewlet-graph-canvas__lane")) {
    expect(inside(lane.getBoundingClientRect(), canvas)).toBe(true);
  }
});

// A LINK TO ONE ENTRY (its sheet, or "Show it on the map") opens the map on
// it, in the middle, as a card that reads, where the map otherwise opens on
// the whole company. Mutation: drop `initialNode` and it opens fitted, the
// entry a tile among a hundred and twenty.
test.each([1440, 390])(
  "a link to one entry opens the map on it, in the middle, at %ipx",
  async (width) => {
    const company = generatedCompany(120);
    const target = company.nodes.filter((node) => node.lane === "data")[3]!;
    await mount(width, company, `#/company/atlas?sel=${target.id}`);
    const canvas = await shownMap();
    expect(
      document.querySelector<HTMLElement>(".crewlet-graph-canvas__world")!.dataset.detail,
    ).toBe("cards");
    const card = document
      .querySelector(
        `.crewlet-graph-canvas__items [data-graph-key="${graphItemKey("node", target.id)}"]`,
      )!
      .getBoundingClientRect();
    expect(Math.abs(card.left + card.width / 2 - (canvas.left + canvas.width / 2))).toBeLessThan(1);
    expect(card.top).toBeGreaterThan(canvas.top + canvas.height / 4);
    expect(card.bottom).toBeLessThan(canvas.bottom - canvas.height / 4);
    expect(texts(".atlas-dock").join(" ")).toContain(target.name);
  },
);

// ONE SELECTION, TWO SURFACES: the entry the address selects is lit on the
// map with its dock and marked in the catalogue, and Details opens its sheet.
// Mutation: hold the selection in the map alone and the row is never marked.
test("a selection is docked on the map, marked in the catalogue, and opens its sheet", async () => {
  const company = grownCompany();
  await mount(1440, company, `#/company/atlas?sel=${company.ordersDb.id}`);
  await eventually(() =>
    expect(
      texts(".crewlet-graph-canvas__card[data-state='lit'] .crewlet-graph-canvas__name"),
    ).toContain("orders-db"),
  );
  // WHAT IS NOT ON ITS PATH RECEDES, and the dock says what is selected.
  expect(drawn(".crewlet-graph-canvas__card[data-state='dim']").length).toBeGreaterThan(0);
  expect(texts(".atlas-dock").join(" ")).toContain("orders-db");
  await eventually(() =>
    expect(texts(".crewlet-data-grid__row--selected").join(" ")).toContain("orders-db"),
  );
  const details = [...document.querySelectorAll<HTMLButtonElement>(".atlas-dock button")].find(
    (button) => button.textContent?.trim() === "Details",
  )!;
  flushSync(() => details.click());
  expect(decodeURIComponent(location.hash)).toContain(`peek=atlas:${company.ordersDb.id}`);
  const clear = document.querySelector<HTMLButtonElement>(
    ".atlas-dock button[aria-label='Clear the selection']",
  )!;
  flushSync(() => clear.click());
  expect(location.hash).not.toContain("sel=");
  expect(location.hash).not.toContain("peek=");
  expect(sidewaysScrollers(host!)).toEqual([]);
});

test.each([768, 390])("a grown company's page does not scroll sideways at %ipx", async (width) => {
  await mount(width, grownCompany());
  await eventually(() =>
    expect(
      document.querySelectorAll(".crewlet-data-grid__row, [class*='data-grid'] li").length,
    ).toBeGreaterThan(0),
  );
  expect(sidewaysScrollers(host!)).toEqual([]);
});

test("three hundred entries open with their groups closed, each one card saying what it holds", async () => {
  await mount(1440, generatedCompany(300));
  await eventually(() =>
    expect(drawn(".crewlet-graph-canvas__card--group").length).toBeGreaterThan(0),
  );
  // EVERY ENTRY STANDS IN A GROUP, so a closed map draws no entry's own card.
  // Mutation: raise ATLAS_OPEN_MAX past 300 and the groups open.
  expect(drawn(".crewlet-graph-canvas__card:not(.crewlet-graph-canvas__card--group)")).toEqual([]);
  expect(texts(".crewlet-graph-canvas__card--group").join(" ")).toMatch(/\d+ entries/);
  expect(sidewaysScrollers(host!)).toEqual([]);
});

test("two thousand entries draw in a moment, mounting only what is near the view", async () => {
  const started = performance.now();
  await mount(1440, generatedCompany(2000));
  await eventually(() => expect(texts(".crewlet-graph-canvas__lane-label")).toEqual(LANES));
  // A JUDGEMENT, not a benchmark: the map's own layout is about 25 ms at this size.
  expect(performance.now() - started).toBeLessThan(5000);
  expect(drawn(".crewlet-graph-canvas__card").length).toBeLessThan(2000);
  expect(sidewaysScrollers(host!)).toEqual([]);
});
