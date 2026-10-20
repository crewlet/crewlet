/**
 * A design before it is drawn: the boards a person lays out on the New design
 * page, their sizes and what each is for, and the notes beside them. Created,
 * it is the engine's design layout starter (`canvasformat.Layout`): one
 * placeholder page per board, which an agent then fills.
 *
 * THE BOUNDS ARE THE CANVAS FORMAT'S, said here before the press rather than
 * refused after it: at most 64 boards, sides of 40 to 8000 pixels, a name of
 * at most 120 bytes and a brief of at most 2000. The engine holds the same
 * bounds and names the board it refuses.
 */

import { ARTIFACT_TITLE_MAX_BYTES } from "~/contract/artifacts.ts";
import { textBudget, utf8Bytes } from "~/lib/format.ts";
import {
  CANVAS_MAX_SIDE,
  CANVAS_MIN_SIDE,
  CANVAS_VERSION,
  addNote,
  changeNote,
  removeNote,
  type CanvasNote,
  type DesignCanvas,
} from "./canvas.ts";
import type { CanvasBoardNote } from "@crewlethq/ui";

/** The most boards one design holds (`canvasformat.MaxBoards`). */
export const DESIGN_MAX_BOARDS = 64;

/** The most a board's brief holds, in bytes (`canvasformat.MaxBriefBytes`). */
export const BOARD_BRIEF_MAX_BYTES = 2000;

/** The room left between two boards laid side by side, in canvas pixels. */
export const BOARD_GAP = 120;

/** A frame size a board can take, as a design tool's frame presets offer them. */
export interface FramePreset {
  id: "desktop" | "laptop" | "tablet" | "phone" | "presentation";
  label: string;
  w: number;
  h: number;
}

/**
 * The frame presets, largest screen first. Desktop is the common design
 * canvas, Laptop a 13 inch screen, Tablet an 11 inch tablet held upright,
 * Phone a current phone, and Presentation a 16 by 9 slide.
 */
export const FRAME_PRESETS: readonly FramePreset[] = [
  { id: "desktop", label: "Desktop", w: 1440, h: 1024 },
  { id: "laptop", label: "Laptop", w: 1280, h: 832 },
  { id: "tablet", label: "Tablet", w: 834, h: 1194 },
  { id: "phone", label: "Phone", w: 390, h: 844 },
  { id: "presentation", label: "Presentation", w: 1920, h: 1080 },
];

/** The preset a size is, or undefined for a custom size. */
export function presetOf(w: number, h: number): FramePreset | undefined {
  return FRAME_PRESETS.find((p) => p.w === w && p.h === h);
}

const preset = (id: FramePreset["id"]): FramePreset => FRAME_PRESETS.find((p) => p.id === id)!;

/** One board of a draft: its frame on the canvas, its name and what it is for. */
export interface DraftBoard {
  /** The board's id on this page only; the engine files each board by its name. */
  id: string;
  title: string;
  x: number;
  y: number;
  w: number;
  h: number;
  brief: string;
}

/** A design before it is drawn: its boards in order (the first the cover) and its notes. */
export interface DesignDraft {
  boards: DraftBoard[];
  notes: CanvasNote[];
}

/** A starting layout: a few boards laid out for a common kind of design. */
export interface StarterLayout {
  id: string;
  label: string;
  /** What it starts, under its name. */
  detail: string;
  boards: { title: string; preset: FramePreset["id"] }[];
}

/** The starting layouts the page offers, in the order it offers them. */
export const STARTER_LAYOUTS: readonly StarterLayout[] = [
  {
    id: "landing",
    label: "Landing page",
    detail: "Desktop and phone",
    boards: [
      { title: "Home", preset: "desktop" },
      { title: "Pricing", preset: "desktop" },
      { title: "Home on phone", preset: "phone" },
    ],
  },
  {
    id: "app",
    label: "App screens",
    detail: "Four phone screens",
    boards: [
      { title: "Sign in", preset: "phone" },
      { title: "Home", preset: "phone" },
      { title: "Detail", preset: "phone" },
      { title: "Settings", preset: "phone" },
    ],
  },
  {
    id: "responsive",
    label: "Responsive screen",
    detail: "Desktop, tablet and phone",
    boards: [
      { title: "Desktop", preset: "desktop" },
      { title: "Tablet", preset: "tablet" },
      { title: "Phone", preset: "phone" },
    ],
  },
  {
    id: "presentation",
    label: "Presentation",
    detail: "Four 16 by 9 slides",
    boards: [
      { title: "Cover", preset: "presentation" },
      { title: "The problem", preset: "presentation" },
      { title: "The solution", preset: "presentation" },
      { title: "Next steps", preset: "presentation" },
    ],
  },
  { id: "blank", label: "Blank canvas", detail: "Place boards yourself", boards: [] },
];

/** A board id no board of the draft holds. */
function freshBoardID(boards: readonly DraftBoard[]): string {
  const taken = new Set(boards.map((b) => b.id));
  let n = boards.length + 1;
  while (taken.has(`b${n}`)) n++;
  return `b${n}`;
}

/** A board name no board of the draft holds: "Screen 4". */
export function freshBoardTitle(boards: readonly DraftBoard[]): string {
  const taken = new Set(boards.map((b) => b.title.trim().toLowerCase()));
  let n = boards.length + 1;
  while (taken.has(`screen ${n}`)) n++;
  return `Screen ${n}`;
}

/** The boards laid side by side in their order, tops aligned, an even gap apart. */
export function tidy(boards: readonly DraftBoard[]): DraftBoard[] {
  let x = 0;
  return boards.map((b) => {
    const placed = { ...b, x, y: 0 };
    x += b.w + BOARD_GAP;
    return placed;
  });
}

/** A draft from a starting layout, laid out in order. */
export function draftFrom(layout: StarterLayout): DesignDraft {
  const boards: DraftBoard[] = [];
  for (const b of layout.boards) {
    const p = preset(b.preset);
    boards.push({
      id: freshBoardID(boards),
      title: b.title,
      x: 0,
      y: 0,
      w: p.w,
      h: p.h,
      brief: "",
    });
  }
  return { boards: tidy(boards), notes: [] };
}

/**
 * The draft with a new board of a preset's size, and the board's id: at a
 * canvas point where one was placed (the Frame tool), else after the last
 * board, tops aligned. Unchanged, with no id, at the format's board bound.
 */
export function addBoard(
  d: DesignDraft,
  size: Pick<FramePreset, "w" | "h">,
  at?: { x: number; y: number },
): { draft: DesignDraft; id: string | undefined } {
  if (d.boards.length >= DESIGN_MAX_BOARDS) return { draft: d, id: undefined };
  const right = d.boards.reduce((edge, b) => Math.max(edge, b.x + b.w + BOARD_GAP), 0);
  const top = d.boards.length > 0 ? Math.min(...d.boards.map((b) => b.y)) : 0;
  const board: DraftBoard = {
    id: freshBoardID(d.boards),
    title: freshBoardTitle(d.boards),
    x: Math.round(at?.x ?? right),
    y: Math.round(at?.y ?? top),
    w: size.w,
    h: size.h,
    brief: "",
  };
  return { draft: { ...d, boards: [...d.boards, board] }, id: board.id };
}

/** The draft with one board's fields changed. */
export function changeBoard(
  d: DesignDraft,
  id: string,
  fields: Partial<Omit<DraftBoard, "id">>,
): DesignDraft {
  return { ...d, boards: d.boards.map((b) => (b.id === id ? { ...b, ...fields } : b)) };
}

/** The draft without one board. */
export function removeBoard(d: DesignDraft, id: string): DesignDraft {
  return { ...d, boards: d.boards.filter((b) => b.id !== id) };
}

/** The draft with one board moved by `by` places in the order, held to its ends. */
export function reorderBoard(d: DesignDraft, id: string, by: -1 | 1): DesignDraft {
  const from = d.boards.findIndex((b) => b.id === id);
  const to = from + by;
  if (from < 0 || to < 0 || to >= d.boards.length) return d;
  const boards = [...d.boards];
  const [moved] = boards.splice(from, 1);
  boards.splice(to, 0, moved!);
  return { ...d, boards };
}

/*
 * THE NOTES ARE A CANVAS'S NOTES, placed, changed and removed by the same
 * rules the design's own page uses (canvas.ts), through a canvas that holds
 * nothing else.
 */
const notesOf = (notes: CanvasNote[], change: (c: DesignCanvas) => DesignCanvas): CanvasNote[] =>
  change({ v: CANVAS_VERSION, boards: {}, order: [], notes }).notes ?? [];

/** The draft with a note placed at a point, and the note's id. */
export function placeNote(
  d: DesignDraft,
  kind: CanvasNote["kind"],
  at: { x: number; y: number },
): { draft: DesignDraft; id: string | undefined } {
  const notes = notesOf(d.notes, (c) => addNote(c, kind, at, kind === "title" ? "Title" : "Note"));
  return notes.length === d.notes.length
    ? { draft: d, id: undefined }
    : { draft: { ...d, notes }, id: notes[notes.length - 1]!.id };
}

/** The draft with one note moved, resized or retyped. */
export function retypeNote(d: DesignDraft, note: CanvasBoardNote): DesignDraft {
  return { ...d, notes: notesOf(d.notes, (c) => changeNote(c, note)) };
}

/** The draft without one note. */
export function dropNote(d: DesignDraft, id: string): DesignDraft {
  return { ...d, notes: notesOf(d.notes, (c) => removeNote(c, id)) };
}

/** What is wrong with one board, field by field, in the sentence each field says. */
export interface BoardProblems {
  title?: string | undefined;
  w?: string | undefined;
  h?: string | undefined;
  brief?: string | undefined;
}

const sideProblem = (v: number): string | undefined =>
  !Number.isInteger(v) || v < CANVAS_MIN_SIDE || v > CANVAS_MAX_SIDE
    ? `A side is a whole number from ${CANVAS_MIN_SIDE} to ${CANVAS_MAX_SIDE} pixels.`
    : undefined;

/** What the engine would refuse in one board, before the press. */
export function boardProblems(b: DraftBoard): BoardProblems {
  const name = b.title.trim();
  const brief = textBudget(b.brief, BOARD_BRIEF_MAX_BYTES, "a board's brief");
  return {
    title:
      name === ""
        ? "Give the board a name."
        : utf8Bytes(name) > ARTIFACT_TITLE_MAX_BYTES
          ? `The name is ${utf8Bytes(name)} bytes and the most is ${ARTIFACT_TITLE_MAX_BYTES}.`
          : undefined,
    w: sideProblem(b.w),
    h: sideProblem(b.h),
    brief: brief.over ? brief.line : undefined,
  };
}

/** Why the draft cannot be created yet, or undefined when it can. */
export function draftHold(d: DesignDraft): string | undefined {
  if (d.boards.length === 0) return "Add a board.";
  if (d.boards.length > DESIGN_MAX_BOARDS)
    return `A design holds at most ${DESIGN_MAX_BOARDS} boards.`;
  const wrong = d.boards.find((b) => Object.values(boardProblems(b)).some(Boolean));
  return wrong ? `Fix the board “${wrong.title.trim() || "without a name"}”.` : undefined;
}

/** The engine's design layout starter (`canvasformat.Layout`), as a create sends it. */
export interface DesignLayout {
  boards: { title: string; x: number; y: number; w: number; h: number; brief?: string }[];
  notes?: CanvasNote[];
}

/** The design layout starter for a draft. */
export function layoutOf(d: DesignDraft): DesignLayout {
  return {
    boards: d.boards.map((b) => ({
      title: b.title.trim(),
      x: Math.round(b.x),
      y: Math.round(b.y),
      w: b.w,
      h: b.h,
      ...(b.brief.trim() ? { brief: b.brief.trim() } : {}),
    })),
    ...(d.notes.length > 0 ? { notes: d.notes } : {}),
  };
}

/** A board's size in words, with its preset's name where it is one: "390 by 844, Phone". */
export function sizeWords(b: Pick<DraftBoard, "w" | "h">): string {
  const p = presetOf(b.w, b.h);
  return p ? `${b.w} by ${b.h}, ${p.label}` : `${b.w} by ${b.h}`;
}

/**
 * The brief an agent is handed with the task, shaped as a goal, the boards
 * with their sizes and briefs, the references, and when it is done.
 */
export function taskBrief(fields: {
  brief: string;
  draft: DesignDraft;
  links: readonly string[];
}): string {
  const parts: string[] = [];
  if (fields.brief.trim()) parts.push(fields.brief.trim());
  parts.push(
    [
      "Boards, in order:",
      ...fields.draft.boards.map((b, i) => {
        const line = `${i + 1}. ${b.title.trim()} (${sizeWords(b)})`;
        return b.brief.trim() ? `${line}: ${b.brief.trim()}` : line;
      }),
    ].join("\n"),
  );
  if (fields.links.length > 0) {
    parts.push(["References:", ...fields.links.map((l) => `- ${l}`)].join("\n"));
  }
  parts.push(
    "Each board is a placeholder page in the design, at its size and place on the canvas. " +
      "Publish each board's real page over its placeholder, keeping its name, size and place. " +
      "It is done when every board is designed.",
  );
  return parts.join("\n\n");
}

/** A reference link the page takes: an absolute http or https address. */
export function linkProblem(value: string): string | undefined {
  try {
    const url = new URL(value.trim());
    return url.protocol === "https:" || url.protocol === "http:"
      ? undefined
      : "A reference is a web address starting with https://.";
  } catch {
    return "A reference is a web address starting with https://.";
  }
}
