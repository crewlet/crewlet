/**
 * A design's canvas (`canvas.json`, `internal/artifacts/canvasformat`): read,
 * changed by a person, saved as a version against its base, and merged onto a
 * newer version when somebody published first (D69).
 *
 * WHAT A PERSON CHANGES is the layout and the notes: a board's frame, a note's
 * frame, text and colour, and which notes there are. A board's page is the
 * agent's (or a file upload's), so a save changes `canvas.json` alone and every
 * artboard is carried by the object it already was: no page's bytes move.
 *
 * THE SAVE IS ONE EDIT whose old text is the whole canvas the draft started
 * from, so the engine applies it only to exactly that file; a save whose base
 * moved is refused before anything is written (`412 version_moved`).
 *
 * THE MERGE IS BY ITEM. A board or a note only one side changed takes that
 * side's value; one both changed takes the person's, since the person is the
 * one looking at it and saving it, and is told which items those were. A note
 * the other side deleted stays deleted unless the person changed it.
 *
 * PURE: no fetch and no React.
 */

import type { CanvasBoardItem, CanvasBoardNote, CanvasNoteColor } from "@crewlethq/ui";

/** Where a design keeps its canvas (`canvasformat.Path`). */
export const CANVAS_PATH = "canvas.json";

/** The canvas format this build reads and writes (`canvasformat.Version`). */
export const CANVAS_VERSION = 1;

/** The bounds the engine holds a frame to (`canvasformat.MinSide`, `MaxSide`, `MaxNotes`). */
export const CANVAS_MIN_SIDE = 40;
export const CANVAS_MAX_SIDE = 8000;
export const CANVAS_MAX_NOTES = 200;

/** A board of the canvas, keyed by its page's path. */
export interface CanvasBoardFrame {
  x: number;
  y: number;
  w: number;
  h: number;
  title?: string;
}

/** A note of the canvas. */
export interface CanvasNote {
  id: string;
  kind: "title" | "sticky";
  x: number;
  y: number;
  w: number;
  h: number;
  text: string;
  color?: CanvasNoteColor;
}

/** A design's canvas, as `canvas.json` holds it. */
export interface DesignCanvas {
  v: number;
  title?: string;
  boards: Record<string, CanvasBoardFrame>;
  order: string[];
  notes?: CanvasNote[];
  cover?: string;
}

/** A canvas file that does not read as a canvas: the engine held it at publish, so this is a fault. */
export class CanvasUnreadable extends Error {}

/** Reads a version's canvas text. */
export function parseCanvas(text: string): DesignCanvas {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    throw new CanvasUnreadable("The canvas is not JSON.");
  }
  const c = raw as Partial<DesignCanvas> | null;
  if (
    !c ||
    typeof c !== "object" ||
    c.v !== CANVAS_VERSION ||
    !c.boards ||
    typeof c.boards !== "object" ||
    !Array.isArray(c.order)
  ) {
    throw new CanvasUnreadable(`The canvas is not version ${CANVAS_VERSION} of the format.`);
  }
  return c as DesignCanvas;
}

/** A canvas as the file a save writes: the field order the format documents, two-space indented. */
export function canvasText(c: DesignCanvas): string {
  const boards: Record<string, CanvasBoardFrame> = {};
  for (const path of c.order) {
    const b = c.boards[path];
    if (b)
      boards[path] = { x: b.x, y: b.y, w: b.w, h: b.h, ...(b.title ? { title: b.title } : {}) };
  }
  const out: DesignCanvas = {
    v: c.v,
    ...(c.title ? { title: c.title } : {}),
    boards,
    order: c.order,
    ...(c.notes && c.notes.length > 0
      ? {
          notes: c.notes.map((n) => ({
            id: n.id,
            kind: n.kind,
            x: n.x,
            y: n.y,
            w: n.w,
            h: n.h,
            text: n.text,
            ...(n.kind === "sticky" && n.color ? { color: n.color } : {}),
          })),
        }
      : {}),
    ...(c.cover ? { cover: c.cover } : {}),
  };
  return `${JSON.stringify(out, null, 2)}\n`;
}

/** A board's label: its title, else its page's file name. */
export function boardTitle(path: string, b: CanvasBoardFrame): string {
  return b.title || path.slice(path.lastIndexOf("/") + 1);
}

/** The canvas's boards as the design system's board draws them, in the canvas's order. */
export function boardItems(c: DesignCanvas): CanvasBoardItem[] {
  return c.order.flatMap((path) => {
    const b = c.boards[path];
    return b ? [{ id: path, x: b.x, y: b.y, w: b.w, h: b.h, title: boardTitle(path, b) }] : [];
  });
}

/** The canvas's notes as the design system's board draws them. */
export function noteItems(c: DesignCanvas): CanvasBoardNote[] {
  return (c.notes ?? []).map((n) => ({ ...n }));
}

/** The canvas with one board's frame changed. */
export function moveBoard(
  c: DesignCanvas,
  path: string,
  rect: { x: number; y: number; w: number; h: number },
): DesignCanvas {
  const b = c.boards[path];
  if (!b) return c;
  return { ...c, boards: { ...c.boards, [path]: { ...b, ...rect } } };
}

/** The canvas with one note replaced. */
export function changeNote(c: DesignCanvas, note: CanvasBoardNote): DesignCanvas {
  return {
    ...c,
    notes: (c.notes ?? []).map((n) =>
      n.id === note.id
        ? {
            id: n.id,
            kind: n.kind,
            x: note.x,
            y: note.y,
            w: note.w,
            h: note.h,
            text: note.text,
            ...(n.kind === "sticky" && (note.color ?? n.color)
              ? { color: note.color ?? n.color }
              : {}),
          }
        : n,
    ),
  };
}

/** The canvas with one note removed. */
export function removeNote(c: DesignCanvas, id: string): DesignCanvas {
  return { ...c, notes: (c.notes ?? []).filter((n) => n.id !== id) };
}

/** A note id the canvas does not use yet: short, lower case, readable in a diff. */
export function freshNoteID(c: DesignCanvas, kind: CanvasNote["kind"]): string {
  const taken = new Set((c.notes ?? []).map((n) => n.id));
  const stem = kind === "title" ? "title" : "note";
  for (let i = 1; ; i++) {
    if (!taken.has(`${stem}-${i}`)) return `${stem}-${i}`;
  }
}

/** The canvas with a new note placed at a point, or unchanged at the format's note bound. */
export function addNote(
  c: DesignCanvas,
  kind: CanvasNote["kind"],
  at: { x: number; y: number },
  text: string,
): DesignCanvas {
  const notes = c.notes ?? [];
  if (notes.length >= CANVAS_MAX_NOTES) return c;
  const size = kind === "title" ? { w: 720, h: 80 } : { w: 280, h: 200 };
  const note: CanvasNote = {
    id: freshNoteID(c, kind),
    kind,
    x: Math.round(at.x),
    y: Math.round(at.y),
    ...size,
    text,
    ...(kind === "sticky" ? { color: "yellow" as const } : {}),
  };
  return { ...c, notes: [...notes, note] };
}

/** Whether two canvases write the same file. */
export function sameCanvas(a: DesignCanvas, b: DesignCanvas): boolean {
  return canvasText(a) === canvasText(b);
}

/** A person's save: the canvas they started from, as text, replaced by the draft. */
export function publishOf(
  baseText: string,
  draft: DesignCanvas,
  label: string,
): { mode: "patch"; edits: unknown[]; files: unknown[]; label?: string } {
  return {
    mode: "patch",
    edits: [{ path: CANVAS_PATH, old_str: baseText, new_str: canvasText(draft) }],
    files: [],
    ...(label ? { label } : {}),
  };
}

/** A draft merged onto a newer version: the canvas to keep editing, and what both sides changed. */
export interface CanvasMerge {
  draft: DesignCanvas;
  /** The boards and notes both sides changed, by label; the person's change was kept. */
  overlaps: string[];
  /** How many boards and notes only the other side changed, which the draft now carries. */
  theirs: number;
}

const json = (v: unknown) => JSON.stringify(v ?? null);

/**
 * Moves a draft started from `base` onto `live`, item by item. The boards are
 * live's (a board is a page, which only a publish of files adds or removes);
 * each frame is the draft's where the draft changed it, else live's. Notes the
 * draft added are added; notes it removed are removed; a note both changed
 * keeps the draft's.
 */
export function mergeCanvas(
  base: DesignCanvas,
  mine: DesignCanvas,
  live: DesignCanvas,
): CanvasMerge {
  const overlaps: string[] = [];
  let theirs = 0;
  const boards: Record<string, CanvasBoardFrame> = {};
  for (const path of live.order) {
    const l = live.boards[path];
    if (!l) continue;
    const b = base.boards[path];
    const m = mine.boards[path];
    const mineChanged = !!b && !!m && json(m) !== json(b);
    const theirsChanged = !b || json(l) !== json(b);
    if (mineChanged && theirsChanged && json(m) !== json(l)) overlaps.push(boardTitle(path, l));
    else if (theirsChanged && !mineChanged) theirs++;
    boards[path] = mineChanged ? { ...m!, ...(l.title ? { title: l.title } : {}) } : l;
  }

  const byID = (c: DesignCanvas) => new Map((c.notes ?? []).map((n) => [n.id, n]));
  const baseNotes = byID(base);
  const mineNotes = byID(mine);
  const liveNotes = byID(live);
  const notes: CanvasNote[] = [];
  for (const l of live.notes ?? []) {
    const b = baseNotes.get(l.id);
    const m = mineNotes.get(l.id);
    if (b && !m) {
      // The person deleted it; a change of theirs to it is said, and lost.
      if (json(l) !== json(b)) overlaps.push(l.text);
      continue;
    }
    const mineChanged = !!b && !!m && json(m) !== json(b);
    const theirsChanged = !b || json(l) !== json(b);
    if (mineChanged && theirsChanged && json(m) !== json(l)) overlaps.push(l.text);
    else if (theirsChanged && !mineChanged) theirs++;
    notes.push(mineChanged ? m! : l);
  }
  for (const m of mine.notes ?? []) {
    if (baseNotes.has(m.id)) {
      // Theirs deleted a note the person kept: kept only if the person changed it.
      if (!liveNotes.has(m.id) && json(m) !== json(baseNotes.get(m.id))) notes.push(m);
      continue;
    }
    if (liveNotes.has(m.id)) {
      // Both sides added a note under one id: the person's takes a fresh one.
      notes.push({ ...m, id: freshNoteID({ ...live, notes }, m.kind) });
    } else {
      notes.push(m);
    }
  }
  return { draft: { ...live, boards, notes }, overlaps, theirs };
}
