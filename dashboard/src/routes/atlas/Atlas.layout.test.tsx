/**
 * THE ATLAS PAGE, LAID OUT, in a real browser: every lane drawn with what
 * would fill an empty one, the map opening with all of it in view (or on the
 * entry a link names), the selection docked on the map and marked in the
 * catalogue under it, a large company opening with its groups closed, two
 * thousand entries drawn without mounting each, and no screen that scrolls
 * sideways, on a wide screen and on a phone.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { graphItemKey } from "@crewlethq/ui";
import { Atlas } from "./Atlas.tsx";
import { InertWebSocket, LayoutClient, sidewaysScrollers } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import {
  activityAnswer,
  entriesAnswer,
  facetsAnswer,
  generatedCompany,
  GROWN_LANES,
  grownCompany,
  mapAnswer,
  shapedCompany,
  sparseCompany,
  summary,
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
  gutter = 0,
  height = 900,
) {
  location.hash = hash;
  await page.viewport(width, height);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px; height: ${height}px; display: flex; flex-direction: column; padding-inline: ${gutter}px; box-sizing: border-box;`;
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

/** The fitted map's zoom, from the world's transform. */
function zoom(): number {
  const world = document.querySelector<HTMLElement>(".crewlet-canvas__world")!;
  return Number(/scale\(([0-9.e-]+)\)/.exec(world.style.transform)?.[1]);
}

/** Whether two boxes share any area, past half a pixel. */
const overlap = (a: DOMRect, b: DOMRect) =>
  a.left < b.right - 0.5 &&
  b.left < a.right - 0.5 &&
  a.top < b.bottom - 0.5 &&
  b.top < a.bottom - 0.5;

// THE FITTED MAP FILLS ITS CANVAS, on a wide screen, a laptop, a tablet and a
// phone. One shape on every canvas fitted a grown company's eighty-one entries
// at 39% on a wide screen, a narrow block in the middle of it with nearly every
// name cut, and at 20% on a phone, marks with no name. Nothing stands outside
// the canvas, under the zoom bar or over another frame. Mutation: draw
// `MAP_ROWS` at fixed columns again, and each fit falls under its floor.
const FITS = [
  { width: 1920, height: 860, floor: 0.46, share: 0.44 },
  { width: 1440, height: 900, floor: 0.46, share: 0.55 },
  { width: 1024, height: 768, floor: 0.44, share: 0.45 },
  { width: 390, height: 844, floor: 0.25, share: 0.4 },
];
test.each(FITS)(
  "a grown company's map fills its canvas at $width by $height",
  async ({ width, height, floor, share }) => {
    await mount(width, shapedCompany(GROWN_LANES), "#/company/atlas", undefined, 0, height);
    const canvas = await shownMap();
    const lanes = drawn(".crewlet-graph-canvas__lane").map((el) => el.getBoundingClientRect());
    expect(lanes).toHaveLength(LANES.length);
    for (const lane of lanes) expect(inside(lane, canvas), "a frame is in view").toBe(true);
    lanes.forEach((a, i) =>
      lanes.slice(i + 1).forEach((b) => expect(overlap(a, b), "two frames overlap").toBe(false)),
    );
    const bar = document
      .querySelector("[aria-label='Zoom in']")!
      .closest(".crewlet-canvas__bar")!
      .getBoundingClientRect();
    for (const el of [
      ...drawn(".crewlet-graph-canvas__items .crewlet-graph-canvas__card"),
      ...drawn(".crewlet-graph-canvas__lane-title"),
    ]) {
      expect(overlap(el.getBoundingClientRect(), bar), "under the zoom bar").toBe(false);
    }
    const covered =
      lanes.reduce((sum, lane) => sum + lane.width * lane.height, 0) /
      (canvas.width * canvas.height);
    expect(zoom()).toBeGreaterThanOrEqual(floor);
    expect(covered).toBeGreaterThanOrEqual(share);
    expect(sidewaysScrollers(host!)).toEqual([]);
  },
);

// THE SAME COMPANY ON THE SAME CANVAS IS DRAWN ALIKE: the arrangement reads
// the canvas and the entries, and nothing else. Mutation: let it read the time.
test("the same company on the same canvas draws its lanes in the same places", async () => {
  const company = shapedCompany(GROWN_LANES);
  const places = () =>
    drawn(".crewlet-graph-canvas__lane").map((el) => {
      const box = el.getBoundingClientRect();
      return [box.left, box.top, box.width, box.height].map(Math.round);
    });
  await mount(1440, company);
  await shownMap();
  const first = places();
  flushSync(() => root?.unmount());
  host?.remove();
  await mount(1440, company);
  await shownMap();
  expect(places()).toEqual(first);
});

// FULLSCREEN HOLDS THE SCREEN, ARRANGED FOR IT: the map's own control takes
// its canvas fullscreen, the map is arranged and fitted for the screen, and
// leaving gives the box its own arrangement back. Drawing a new arrangement on
// a new canvas removed the element holding the screen, which dropped it at
// once. Mutation: key the canvas on its arrangement again.
test("the Fullscreen control holds the screen with the map arranged for it, and gives the box back", async () => {
  await mount(1440, shapedCompany(GROWN_LANES), "#/company/atlas", undefined, 0, 900);
  await shownMap();
  const box = document.querySelector<HTMLElement>(".atlas-map")!;
  const card = () => getComputedStyle(box).getPropertyValue("--crewlet-graph-canvas-card-width");
  const boxed = card();
  const canvas = document.querySelector<HTMLElement>(".atlas-map .crewlet-graph-canvas")!;
  /** The map's world inside its canvas, filling it along one side. */
  const fitted = () => {
    const room = canvas.querySelector(".crewlet-canvas__viewport")!.getBoundingClientRect();
    const world = canvas.querySelector(".crewlet-graph-canvas__world")!.getBoundingClientRect();
    expect(inside(world, room), "the map is in view").toBe(true);
    expect(Math.max(world.width / room.width, world.height / room.height)).toBeGreaterThan(0.75);
  };

  await userEvent.click(page.getByRole("button", { name: "Fullscreen", exact: true }));
  await eventually(() => expect(card()).not.toBe(boxed));
  await eventually(() => {
    expect(document.fullscreenElement).toBe(canvas);
    expect(canvas.isConnected).toBe(true);
    const held = canvas.getBoundingClientRect();
    expect([held.width, held.height]).toEqual([innerWidth, innerHeight]);
    fitted();
  });

  await userEvent.click(page.getByRole("button", { name: "Leave fullscreen" }));
  await eventually(() => {
    expect(document.fullscreenElement).toBeNull();
    expect(card()).toBe(boxed);
    expect(document.querySelector(".atlas-map .crewlet-graph-canvas")).toBe(canvas);
    fitted();
  });
});

// A PHONE'S LANES KEEP THEIR TITLES: a grown company on a phone fits as marks,
// so each lane's title and tally are what reads, and a lane one column wide
// cut its title to a letter. Mutation: drop the titles from the arrangement.
test("on a phone, every lane's title says at least the first letters of its name", async () => {
  await mount(390, shapedCompany(GROWN_LANES), "#/company/atlas", undefined, 0, 844);
  await shownMap();
  const labels = drawn(".crewlet-graph-canvas__lane-label");
  expect(labels).toHaveLength(LANES.length);
  for (const label of labels) {
    const text = label.textContent ?? "";
    const shown =
      label.scrollWidth <= label.clientWidth + 0.5
        ? text.length
        : Math.floor(label.clientWidth / (label.scrollWidth / text.length)) - 1;
    expect(shown, text).toBeGreaterThanOrEqual(Math.min(4, text.length));
  }
});

// A FILTER FITS THE MAP IT ANSWERS: the last answer stays on screen while the
// new one is read, so a fit keyed on the question landed on the old map and
// left the new one small in a corner of it. Mutation: key the fit on the
// question again and the narrowed map fills a fraction of the canvas.
test("a filter fits the map to the answer it narrows to, not the one it replaces", async () => {
  const company = generatedCompany(120);
  const alerting = {
    ...company,
    // What a filter leaves: a few alerting entries, in two of the eight lanes.
    nodes: company.nodes.filter(
      (node) => node.health === "alerting" && (node.lane === "services" || node.lane === "data"),
    ),
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
  // Fitted to the new answer: it fills the canvas along one side, or a map
  // that small is drawn at actual size, the most a fit takes.
  await eventually(() =>
    expect(
      filling(canvas) > 0.9 ||
        document.querySelector("[aria-label^='Zoom is 100 percent']") !== null,
    ).toBe(true),
  );
  expect(
    document.querySelector(".crewlet-graph-canvas__world")!.getBoundingClientRect().width,
  ).not.toBe(whole);
  for (const lane of drawn(".crewlet-graph-canvas__lane")) {
    expect(inside(lane.getBoundingClientRect(), canvas)).toBe(true);
  }
});

// A FILTER THAT RE-ARRANGES THE MAP AT ANOTHER CARD WIDTH IS FITTED AT IT: the
// fit asked for with the filter lands before the kit measures the new card,
// so a map narrowed to two lanes stood at two thirds of its size in a corner
// of the canvas. Mutation: run on a kit that keeps an untouched fit through
// new card sizes (no `refitKey`).
test("a filter that changes the card width fits the map at the new width", async () => {
  const company = shapedCompany(GROWN_LANES);
  const two = {
    ...company,
    nodes: company.nodes.filter((node) => node.lane === "entry" || node.lane === "code"),
  };
  await mount(1440, company, "#/company/atlas", (params) => (params.lane ? two : company));
  const canvas = await shownMap();
  const box = document.querySelector<HTMLElement>(".atlas-map")!;
  const card = () => getComputedStyle(box).getPropertyValue("--crewlet-graph-canvas-card-width");
  const whole = card();
  location.hash = "#/company/atlas?lane=entry&lane=code";
  await eventually(() => expect(drawn(".crewlet-graph-canvas__lane")).toHaveLength(2));
  expect(card()).not.toBe(whole);
  await eventually(() => expect(filling(canvas)).toBeGreaterThan(0.9));
  for (const lane of drawn(".crewlet-graph-canvas__lane")) {
    expect(inside(lane.getBoundingClientRect(), canvas)).toBe(true);
  }
});

// SERVICES ON NO CALL CHAIN FLOW ACROSS THEIR LANE: seventy-eight services
// read from a cluster before any call is known stood in one column, so the
// lane alone set the fit and no name read. Mutation: name every service's
// depth again and the lane is one column of 78, the map marks.
test("seventy-eight services nothing calls open across their lane, at a size their cards draw", async () => {
  const services = Array.from({ length: 78 }, (_, i) =>
    summary("service", `svc-${String(i + 1).padStart(2, "0")}`),
  );
  const company = { nodes: services, edges: [] };
  await mount(1440, company, "#/company/atlas?lane=services");
  await shownMap();
  const detail = document.querySelector<HTMLElement>(".crewlet-graph-canvas__world")!.dataset
    .detail;
  expect(detail).not.toBe("marks");
  const lefts = new Set(
    drawn(".crewlet-graph-canvas__items .crewlet-graph-canvas__card").map((card) =>
      Math.round(card.getBoundingClientRect().left),
    ),
  );
  expect(lefts.size).toBeGreaterThan(1);
});

// A CARD IN THE BAND SHOWS ITS ENTRY ON THE MAP, and on a phone the map is
// below the fold: the press selected the entry where nobody could see it.
// Mutation: select without bringing the map into view.
test("a Needs attention card brings the map, with its selection, into view on a phone", async () => {
  const company = grownCompany();
  await mount(390, company);
  // THE SHELL'S SCROLLER, which the page scrolls in: a bare screen has none.
  host!.id = SCREEN_SCROLL_ID;
  host!.style.overflowY = "auto";
  await shownMap();
  const card = await eventually(() => {
    const found = [...document.querySelectorAll<HTMLElement>(".atlas-band__entry")].find((one) =>
      one.textContent?.startsWith("orders-db"),
    );
    expect(found).toBeDefined();
    return found!;
  });
  // Below the fold: most of the map, and its dock at the foot, out of view.
  const view = host!.getBoundingClientRect();
  const before = document.querySelector(".atlas-map")!.getBoundingClientRect();
  expect(before.bottom).toBeGreaterThan(view.bottom + before.height / 2);
  card.click();
  await eventually(() => {
    const box = document.querySelector(".atlas-map")!.getBoundingClientRect();
    expect(box.top).toBeGreaterThanOrEqual(view.top - 1);
    expect(box.bottom).toBeLessThanOrEqual(view.bottom + 1);
  });
  expect(location.hash).toContain(`sel=${company.ordersDb.id}`);
});

// A NARROWED MAP DRAWS WHAT MATCHED: all eight lanes at their full width set
// the fit, so on a phone the alerting entries the band's link leads to were
// marks with no name. Mutation: draw every lane on a narrowed map again, or
// give a lane of two matches its full width of columns.
test("on a phone, a filter's few matches open in their own lanes at a size their names read", async () => {
  const company = generatedCompany(120);
  const alerting = {
    ...company,
    // What a filter leaves: a few alerting entries, in two of the eight lanes.
    nodes: company.nodes.filter(
      (node) => node.health === "alerting" && (node.lane === "services" || node.lane === "data"),
    ),
  };
  // Inside the phone's 16px gutter, as the app draws the page, not edge to edge.
  await mount(
    390,
    company,
    "#/company/atlas?health=alerting",
    (params) => (params.health === "alerting" ? alerting : company),
    16,
  );
  await shownMap();
  const lanes = new Set(alerting.nodes.map((node) => node.lane));
  await eventually(() => expect(drawn(".crewlet-graph-canvas__lane")).toHaveLength(lanes.size));
  expect(lanes.size).toBeLessThan(LANES.length);
  expect(
    document.querySelector<HTMLElement>(".crewlet-graph-canvas__world")!.dataset.detail,
  ).not.toBe("marks");
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
  // NEVER A BLANK CELL: a group whose vendor has no mark (or no vendor at all)
  // draws the card's own glyph there. Mutation: pass a lead that draws nothing.
  const cells = drawn(".crewlet-graph-canvas__card--group .crewlet-graph-canvas__cell");
  expect(cells.length).toBeGreaterThan(0);
  for (const cell of cells)
    expect(cell.querySelector("svg"), cell.textContent ?? "").not.toBeNull();
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
