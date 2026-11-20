import { GRAPH_CANVAS_DETAIL_ZOOM, graphDetail } from "@crewlethq/ui";
import { describe, expect, test } from "vitest";
import type { AtlasLane } from "~/contract/atlas.ts";
import { GROWN_LANES } from "~/test/atlas.ts";
import { arrangeMap, mapMetrics, mapRoom, readings, type LaneLoad } from "./arrange.ts";
import { LANES, MAP_ROWS } from "./words.ts";

const METRICS = mapMetrics(16, 1);

/** The lanes of a map holding `counts`, every name `length` characters long. */
function loads(counts: Readonly<Partial<Record<AtlasLane, number>>>, length = 24): LaneLoad[] {
  return MAP_ROWS.flatMap((row, r) =>
    row.lanes.map((id) => {
      const n = counts[id] ?? 0;
      return {
        id,
        row: r,
        cards: Array.from({ length: n }, (_, i) => length - (i % 7)),
        empty: n === 0 ? LANES[id].empty.length : 0,
        title: LANES[id].label.length + 4,
      };
    }),
  );
}

const SMALL = {
  entry: 2,
  services: 3,
  data: 1,
  runtime: 1,
  platforms: 1,
  code: 1,
  observability: 1,
};
const GROUPS = {
  entry: 2,
  services: 1,
  data: 3,
  runtime: 3,
  platforms: 5,
  code: 2,
  observability: 1,
  tools: 3,
};

describe("arranging the map for its canvas", () => {
  // A ROW OF THE MAP'S READING STAYS ONE: it is split into consecutive rows or
  // stood whole beside its neighbours, never mixed with part of another.
  // Mutation: let a reading join any consecutive lanes and a row holds Data
  // and Runtime with Services on the row above.
  test("every reading keeps the lanes in order and each row of the map whole or split, never mixed", () => {
    const lanes = loads(GROWN_LANES);
    const order = lanes.map((lane) => lane.id);
    const all = readings(lanes);
    expect(all.length).toBeGreaterThan(MAP_ROWS.length);
    for (const reading of all) {
      expect(reading.flat().map((lane) => lane.id)).toEqual(order);
      for (const band of reading) {
        const rows = new Set(band.map((lane) => lane.row));
        if (rows.size === 1) continue;
        // A band of several rows holds each of them whole.
        for (const row of rows) {
          expect(band.filter((lane) => lane.row === row).length).toBe(MAP_ROWS[row]!.lanes.length);
        }
      }
    }
  });

  test("the same lanes on the same canvas are arranged alike", () => {
    const room = mapRoom(1148, 544, 16);
    expect(arrangeMap(loads(GROWN_LANES), room, METRICS)).toEqual(
      arrangeMap(loads(GROWN_LANES), room, METRICS),
    );
  });

  // A WIDE CANVAS STANDS MORE SIDE BY SIDE AND A TALL ONE STACKS: one shape on
  // every canvas packed a wide one into a block in its middle and a phone's
  // into a strip of marks. Mutation: plan every canvas as the widest.
  test("a wide canvas takes more columns, and a phone's stacks the lanes in more rows", () => {
    const lanes = loads(GROWN_LANES);
    const wide = arrangeMap(lanes, mapRoom(1628, 544, 16), METRICS);
    const phone = arrangeMap(lanes, mapRoom(352, 544, 16), METRICS);
    const columns = (plan: typeof wide) =>
      Math.max(...plan.rows.map((row) => row.lanes.reduce((sum, lane) => sum + lane.columns, 0)));
    expect(wide.rows.length).toBeLessThanOrEqual(MAP_ROWS.length);
    expect(columns(wide)).toBeGreaterThan(7);
    expect(phone.rows.length).toBeGreaterThan(MAP_ROWS.length);
    expect(phone.zoom).toBeGreaterThan(wide.zoom / 3);
  });

  // NO NAME SMALLER THAN A TILE'S: from half size a card draws its name at the
  // card's own scale, 6.5px at half size against a tile's 10px. Mutation: drop
  // the rule and a grown company on a laptop fits at 57%, every name 7px.
  test("a fit never draws a card's name smaller than a tile's", () => {
    const { tile, name: card } = METRICS.type;
    for (const counts of [SMALL, GROWN_LANES, GROUPS]) {
      for (let width = 360; width <= 2560; width += 220) {
        for (const height of [352, 544, 900]) {
          const plan = arrangeMap(loads(counts), mapRoom(width, height, 16), METRICS);
          if (graphDetail(plan.zoom) === "cards") {
            expect(card * plan.zoom, `${width}x${height}`).toBeGreaterThanOrEqual(tile * 0.95);
          }
          expect(plan.zoom).toBeLessThanOrEqual(1);
        }
      }
    }
    expect(GRAPH_CANVAS_DETAIL_ZOOM).toBeLessThan(tile / card);
  });

  test("a card is as wide as the kit's at the least and twice that at the most", () => {
    for (const [width, height] of [
      [352, 544],
      [1148, 544],
      [1628, 544],
      [2560, 1300],
    ] as const) {
      const plan = arrangeMap(loads(GROWN_LANES, 34), mapRoom(width, height, 16), METRICS);
      expect(plan.card).toBeGreaterThanOrEqual(Math.round(METRICS.card));
      expect(plan.card).toBeLessThanOrEqual(Math.round(2 * METRICS.card));
    }
  });

  // THE SPARE WIDTH GOES TO THE CARDS, and more of it where the names are
  // long. Mutation: plan every card at the kit's width.
  test("long names widen the cards on a wide canvas, at least as far as short ones", () => {
    const room = mapRoom(1628, 544, 16);
    const long = arrangeMap(loads(GROWN_LANES, 30), room, METRICS);
    const short = arrangeMap(loads(GROWN_LANES, 8), room, METRICS);
    expect(long.card).toBeGreaterThan(Math.round(METRICS.card));
    expect(long.card).toBeGreaterThanOrEqual(short.card);
  });

  test("a narrow canvas stands its legend in a foot under it, a wide one in a band over its foot", () => {
    const narrow = mapRoom(352, 544, 16);
    const wide = mapRoom(1148, 544, 16);
    expect(narrow.legend).toBe(0);
    expect(narrow.height).toBeLessThan(544 - 2);
    expect(wide.legend).toBeGreaterThan(0);
    expect(wide.height).toBe(544 - 2);
  });
});
