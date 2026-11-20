/**
 * The map's lanes arranged for its canvas (rows, columns, card width), so the fitted map fills it.
 * Each is scored by the fit the kit's sizes give it (docs/reference/dashboard-design.md).
 */

import { spacing } from "@crewlethq/tokens";
import {
  CANVAS_FIT_MAX_ZOOM,
  CANVAS_FIT_PADDING,
  GRAPH_CANVAS_FIT_FLOOR,
  GRAPH_CANVAS_NARROW_WIDTH,
  graphDetail,
} from "@crewlethq/ui";

/** One lane as the arrangement weighs it, in reading order. */
export interface LaneLoad {
  id: string;
  /** Its row of `MAP_ROWS`: a row's lanes stay together, in order. */
  row: number;
  /** Characters each card says: an entry's name, or a closed group's label and count. */
  cards: readonly number[];
  /** Where columns mean something (a call's depth): how many cards name each column. */
  named?: readonly number[] | undefined;
  /** Characters an empty lane's message says; 0 for a lane that holds cards. */
  empty: number;
  /** Characters of its title that must show: its name's first letters, count and tally. */
  title: number;
}

/** The canvas a map is drawn on, and what of it the kit's chrome takes, in CSS px. */
export interface MapRoom {
  width: number;
  height: number;
  /** The legend's band along a wide canvas's foot, which the fit leaves clear. */
  legend: number;
  /** The zoom bar in the top right corner, with its inset. */
  bar: { width: number; height: number };
}

/** The kit's sizes at the reader's root font size and density, in CSS px. */
export interface MapMetrics {
  card: number;
  cardHeight: number;
  cell: number;
  gutter: number;
  rowGap: number;
  laneHead: number;
  bandGap: number;
  lanePad: number;
  margin: number;
  /** The room beside a card's name, and beside a tile's (held on screen). */
  pad: number;
  tilePad: number;
  /** The type of a card's name, a tile's (the kit's least), a lane's title, an empty lane's words. */
  type: { name: number; tile: number; title: number; words: number };
}

export interface MapPlan {
  rows: { lanes: { id: string; columns: number }[] }[];
  /** A card's width, in CSS px. */
  card: number;
  /** The zoom the map is planned to fit at. */
  zoom: number;
  /** Changes whenever the arrangement does. */
  key: string;
}

/** The kit's card (GraphCanvas.css), in rem: its width, its height and its glyph's cell. */
const KIT_CARD = { width: 11, height: 2.375, cell: 2.125 };
/** The type the kit sets them in, in rem: compact, badge, 2xs and xs. */
const KIT_TYPE = { name: 0.8125, tile: 0.625, title: 0.6875, words: 0.75 };

/** The kit's chrome in rem: the zoom bar's corner, a wide canvas's legend, a narrow one's foot. */
const KIT_CHROME = {
  bar: { width: 11.625, height: 3 },
  legend: 3,
  foot: { line: 2.125, more: 1.6, words: 43.4 },
};

/** The widest card is twice the kit's: past that it holds a name's tail, and the zoom pays. */
const WIDEST = 2;
const WIDTHS = 12;

/** A character's average advance in the map's type, in em, the ellipsis allowed for. */
const CHAR_EM = 0.62;
/** The same for a lane's title: mono capitals spaced wide, with its padding. */
const TITLE_EM = 0.72;

/** A card's name within half a pixel of a tile's reads as well as one. */
const LEGIBLE = 0.95;

/** How far a fit may land from its plan (its chrome estimated): no plan sits on a level's edge. */
const MARGIN = 0.03;

/** A fit within a tenth of the largest is as large; the card saying the most names whole wins. */
const NEAR_ZOOM = 0.9;

/** Choices this close (or one name apart) are one; the reading nearer `MAP_ROWS` wins. */
const NEAR = 0.97;

/** The most columns a map is planned at. */
const MAX_COLUMNS = 40;

const px = (value: string) => Number.parseFloat(value);

export function mapMetrics(rem: number, density: number): MapMetrics {
  const space = (step: keyof typeof spacing) => px(spacing[step]) * density;
  return {
    card: KIT_CARD.width * rem,
    cardHeight: KIT_CARD.height * rem,
    cell: KIT_CARD.cell * rem,
    gutter: space("7"),
    rowGap: space("3"),
    laneHead: space("5"),
    bandGap: space("7"),
    lanePad: space("3"),
    margin: space("7"),
    pad: space("2"),
    tilePad: space("1"),
    type: {
      name: KIT_TYPE.name * rem,
      tile: KIT_TYPE.tile * rem,
      title: KIT_TYPE.title * rem,
      words: KIT_TYPE.words * rem,
    },
  };
}

/** The canvas the kit draws in a box of `width` by `height`, its one pixel frame inside. */
export function mapRoom(width: number, height: number, rem: number): MapRoom {
  const narrow = width < GRAPH_CANVAS_NARROW_WIDTH;
  const { line, more, words } = KIT_CHROME.foot;
  const lines = Math.max(1, Math.ceil((words * rem) / Math.max(rem, width - rem)));
  const foot = narrow ? (line + (lines - 1) * more) * rem : 0;
  return {
    width: width - 2,
    height: height - 2 - foot,
    legend: narrow ? 0 : KIT_CHROME.legend * rem,
    bar: { width: KIT_CHROME.bar.width * rem, height: KIT_CHROME.bar.height * rem },
  };
}

/** The rows a lane takes at `span` columns. */
function laneRows(lane: LaneLoad, span: number, metrics: MapMetrics): number {
  if (lane.cards.length === 0) {
    // An empty lane is as tall as its words, which the kit measures and this estimates.
    const inner = span * metrics.card + (span - 1) * metrics.gutter - 2 * metrics.lanePad;
    const type = metrics.type.words;
    const lines = Math.ceil((lane.empty * CHAR_EM * type) / Math.max(inner, type)) + 1;
    const tall = lines * type * 1.5 + 2 * metrics.lanePad;
    return Math.max(1, Math.ceil((tall + metrics.rowGap) / (metrics.cardHeight + metrics.rowGap)));
  }
  if (span > 1 && lane.named && lane.named.length > 0) {
    // Named columns, stood as the kit stands them: a deeper one folds into the last.
    const stacks = Array.from({ length: span }, () => 0);
    let loose = lane.cards.length;
    lane.named.forEach((count, column) => {
      stacks[Math.min(span - 1, column)]! += count;
      loose -= count;
    });
    for (; loose > 0; loose--) {
      let at = 0;
      for (let c = 1; c < span; c++) if (stacks[c]! < stacks[at]!) at = c;
      stacks[at]! += 1;
    }
    return Math.max(...stacks);
  }
  return Math.ceil(lane.cards.length / span);
}

/**
 * The ways the lanes may be read as rows: a row of `MAP_ROWS` split into consecutive rows, or
 * whole rows side by side, never part of one with another, so each still reads as one.
 */
export function readings(lanes: readonly LaneLoad[]): LaneLoad[][][] {
  const rows: LaneLoad[][] = [];
  for (const lane of lanes) {
    const last = rows[rows.length - 1];
    if (last && last[0]!.row === lane.row) last.push(lane);
    else rows.push([lane]);
  }
  if (rows.length === 0) return [];
  const splits = (row: LaneLoad[]): LaneLoad[][][] =>
    Array.from({ length: 1 << (row.length - 1) }, (_, mask) => {
      const parts: LaneLoad[][] = [[row[0]!]];
      for (let i = 1; i < row.length; i++) {
        if (mask & (1 << (i - 1))) parts.push([row[i]!]);
        else parts[parts.length - 1]!.push(row[i]!);
      }
      return parts;
    });
  const out: LaneLoad[][][] = [];
  for (let mask = 0; mask < 1 << (rows.length - 1); mask++) {
    let ways: LaneLoad[][][] = [[]];
    let block: LaneLoad[][] = [rows[0]!];
    const close = () => {
      const options = block.length === 1 ? splits(block[0]!) : [[block.flat()]];
      ways = ways.flatMap((way) => options.map((option) => [...way, ...option]));
    };
    for (let i = 1; i < rows.length; i++) {
      if (mask & (1 << (i - 1))) block.push(rows[i]!);
      else {
        close();
        block = [rows[i]!];
      }
    }
    close();
    out.push(...ways);
  }
  return out;
}

/** How far a reading strays from `MAP_ROWS`: each place it breaks or joins them differently. */
function strays(reading: readonly LaneLoad[][]): number {
  let count = 0;
  reading.forEach((band, i) => {
    for (let j = 1; j < band.length; j++) if (band[j]!.row !== band[j - 1]!.row) count += 1;
    const next = reading[i + 1]?.[0];
    if (next && next.row === band[band.length - 1]!.row) count += 1;
  });
  return count;
}

interface Candidate {
  reading: LaneLoad[][];
  spans: number[][];
  columns: number;
  card: number;
  zoom: number;
  readable: number;
  titles: number;
  legible: boolean;
  clear: boolean;
  fill: number;
  strays: number;
}

/** The arrangement the map is drawn in on `room`. */
export function arrangeMap(
  lanes: readonly LaneLoad[],
  room: MapRoom,
  metrics: MapMetrics,
): MapPlan {
  const all = readings(lanes);
  if (all.length === 0) return { rows: [], card: Math.round(metrics.card), zoom: 1, key: "" };
  const names = lanes.flatMap((lane) => lane.cards).sort((a, b) => a - b);
  const total = lanes.reduce((sum, lane) => sum + Math.max(1, lane.cards.length), 0);
  const limit = Math.max(lanes.length, Math.min(total, MAX_COLUMNS));
  // Each lane's rows at every span, and its fewest columns within any number of rows.
  const rowsAt = new Map(
    lanes.map((lane) => [
      lane,
      Array.from({ length: limit + 1 }, (_, span) =>
        span === 0 ? Number.POSITIVE_INFINITY : laneRows(lane, span, metrics),
      ),
    ]),
  );
  const spanFor = new Map(
    lanes.map((lane) => {
      const table = rowsAt.get(lane)!;
      const fewest = Array.from({ length: table[1]! + 1 }, () => Number.POSITIVE_INFINITY);
      for (let span = limit; span >= 1; span--) {
        for (let rows = table[span]!; rows < fewest.length; rows++) fewest[rows] = span;
      }
      return [lane, fewest];
    }),
  );
  const pitch = metrics.cardHeight + metrics.rowGap;
  const tileType = metrics.type.tile;
  const cardType = metrics.type.name;
  /** The size a name is drawn at: a tile's, a card's at its scale, or none on a mark. */
  const typeAt = (zoom: number) => {
    const detail = graphDetail(zoom);
    return detail === "tiles" ? tileType : detail === "cards" ? cardType * zoom : 0;
  };
  const pad = CANVAS_FIT_PADDING;
  const roomWidth = room.width - 2 * pad;
  const roomHeight = room.height - room.legend - 2 * pad;
  // A lane's title holds its size on screen and stands half its height above the outline.
  const titleType = metrics.type.title;
  const overhang = titleType * 0.7;

  /** A band at `columns`: as few rows as fit, each spare column to its most crowded lane. */
  const placeBand = (band: readonly LaneLoad[], columns: number) => {
    const tallest = Math.max(...band.map((lane) => rowsAt.get(lane)![1]!));
    for (let rows = 1; rows <= tallest; rows++) {
      const spans = band.map((lane) => spanFor.get(lane)![Math.min(rows, rowsAt.get(lane)![1]!)]!);
      let used = spans.reduce((sum, span) => sum + span, 0);
      if (used > columns) continue;
      for (; used < columns; used++) {
        let pick = 0;
        band.forEach((lane, i) => {
          if (lane.cards.length / spans[i]! > band[pick]!.cards.length / spans[pick]!) pick = i;
        });
        spans[pick]! += 1;
      }
      return { spans, rows: Math.max(...band.map((lane, i) => rowsAt.get(lane)![spans[i]!]!)) };
    }
    return null;
  };

  const candidates: Candidate[] = [];
  for (const reading of all) {
    const away = strays(reading);
    for (
      let columns = Math.max(...reading.map((band) => band.length));
      columns <= limit;
      columns++
    ) {
      const placed = reading.map((band) => placeBand(band, columns));
      if (placed.some((band) => band === null)) continue;
      const bands = placed as { spans: number[]; rows: number }[];
      const height =
        2 * metrics.margin +
        bands.reduce(
          (sum, band) =>
            sum + metrics.laneHead + band.rows * pitch - metrics.rowGap + metrics.lanePad,
          0,
        ) +
        (bands.length - 1) * metrics.bandGap;
      for (let step = 0; step < WIDTHS; step++) {
        const card = metrics.card * (1 + ((WIDEST - 1) * step) / (WIDTHS - 1));
        const width = 2 * metrics.margin + columns * (card + metrics.gutter);
        const zoom = Math.max(
          GRAPH_CANVAS_FIT_FLOOR,
          Math.min(CANVAS_FIT_MAX_ZOOM, roomWidth / width, roomHeight / height),
        );
        // Where the fit puts it (centred over the legend), and whether the zoom bar covers it.
        const left = (room.width - width * zoom) / 2;
        const top = Math.max(pad, (room.height - room.legend - height * zoom) / 2);
        const clear =
          top + metrics.margin * zoom - overhang >= room.bar.height + metrics.tilePad ||
          left + (width - metrics.margin) * zoom <= room.width - room.bar.width - metrics.tilePad;
        const detail = graphDetail(zoom);
        const type = typeAt(zoom);
        const space =
          detail === "tiles"
            ? zoom * (card - metrics.cell) - 2 * metrics.tilePad
            : zoom * (card - metrics.cell - 2 * metrics.pad);
        // A title reads where its lane is as wide on screen as the words it must show.
        let titles = 0;
        reading.forEach((band, b) =>
          band.forEach((lane, i) => {
            const span = bands[b]!.spans[i]!;
            const head = (span * (card + metrics.gutter) - metrics.gutter) * zoom;
            if (head >= (lane.title + 2) * TITLE_EM * titleType) titles += 1;
          }),
        );
        candidates.push({
          reading,
          spans: bands.map((band) => band.spans),
          columns,
          card,
          zoom,
          readable: type > 0 ? upTo(names, Math.floor(space / (CHAR_EM * type))) : 0,
          titles,
          legible: [1 - MARGIN, 1 + MARGIN].every(
            (off) => typeAt(Math.min(CANVAS_FIT_MAX_ZOOM, zoom * off)) >= tileType * LEGIBLE,
          ),
          clear,
          fill: (width * zoom * height * zoom) / (roomWidth * roomHeight),
          strays: away,
        });
      }
    }
  }

  // Nothing under the zoom bar, and no name smaller than a tile's: from half size a card draws
  // its name at its own scale, under a tile's until about three quarters.
  let pool = candidates.filter((one) => one.clear);
  if (pool.length === 0) pool = candidates;
  const legible = pool.filter((one) => one.legible);
  const byZoom = () => {
    const largest = pool.reduce((top, one) => Math.max(top, one.zoom), 0);
    pool = pool.filter((one) => one.zoom >= largest * NEAR_ZOOM);
  };
  const byTitles = () => {
    const titles = pool.reduce((top, one) => Math.max(top, one.titles), 0);
    pool = pool.filter((one) => one.titles === titles);
  };
  // Where no name can read (every fit marks), the lanes' titles are what reads, before the zoom.
  if (legible.length > 0) {
    pool = legible;
    byZoom();
    byTitles();
  } else {
    byTitles();
    byZoom();
  }
  const readable = pool.reduce((top, one) => Math.max(top, one.readable), 0);
  pool = pool.filter((one) => one.readable >= Math.min(readable * NEAR, readable - 1));
  pool.sort(
    (a, b) =>
      a.strays - b.strays ||
      Math.round(b.zoom * 100) - Math.round(a.zoom * 100) ||
      a.columns - b.columns ||
      b.fill - a.fill ||
      a.card - b.card,
  );
  const best = pool[0]!;
  const rows = best.reading.map((band, b) => ({
    lanes: band.map((lane, i) => ({ id: lane.id, columns: best.spans[b]![i]! })),
  }));
  const card = Math.round(best.card);
  const shape = rows.map((row) => row.lanes.map((lane) => `${lane.id}:${lane.columns}`).join(","));
  return { rows, card, zoom: best.zoom, key: `${card}|${shape.join("/")}` };
}

/** How many of the sorted `values` are at most `limit`. */
function upTo(values: readonly number[], limit: number): number {
  let low = 0;
  let high = values.length;
  while (low < high) {
    const mid = (low + high) >> 1;
    if (values[mid]! <= limit) low = mid + 1;
    else high = mid;
  }
  return low;
}
