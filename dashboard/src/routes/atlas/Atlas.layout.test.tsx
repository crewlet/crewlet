/**
 * THE ATLAS PAGE, LAID OUT, in a real browser: every lane drawn with what
 * would fill an empty one, the selection docked on the map and marked in the
 * catalogue under it, a large company opening with its groups closed, two
 * thousand entries drawn without mounting each, and no screen that scrolls
 * sideways, on a wide screen and on a phone.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
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

async function mount(width: number, fixture: AtlasFixture, hash = "#/company/atlas") {
  location.hash = hash;
  await page.viewport(width, 900);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px; height: 900px; display: flex; flex-direction: column;`;
  document.body.append(host);
  root = createRoot(host);
  const ask = (what: string) =>
    what === "atlas_map"
      ? mapAnswer(fixture)
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
    // AN EMPTY LANE SAYS WHAT IN THIS BUILD WOULD FILL IT, and a filled one draws its cards.
    const empty = texts(".crewlet-graph-canvas__empty").join(" ");
    expect(empty).toContain("Services appear when a person adds one, by asking Ask Crewlet.");
    expect(empty).toContain("Servers appear from the SSH servers the SSH integration declares.");
    expect(empty).not.toContain("Repositories appear");
    expect(texts(".crewlet-graph-canvas__name")).toEqual(
      expect.arrayContaining(["acme/web", "acme/api", "acme/infra"]),
    );
    // THE MAP TAKES ITS SHARE OF THE WINDOW, the catalogue follows under it,
    // and nothing scrolls sideways.
    const picture = map()!.getBoundingClientRect();
    expect(picture.height).toBeGreaterThan(300);
    const catalogue = document.querySelector(".atlas-catalogue")!.getBoundingClientRect();
    expect(catalogue.top).toBeGreaterThanOrEqual(picture.bottom);
    expect(sidewaysScrollers(host!)).toEqual([]);
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
