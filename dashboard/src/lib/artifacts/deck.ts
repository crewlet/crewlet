/**
 * A deck being edited by a person: the draft, what it changed against the
 * version it started from, the publish that saves it, and the merge a refused
 * save is moved onto the newer version by (D69).
 *
 * A SAVE IS A VERSION, based on the version the draft started from: a patch
 * whose edits replace each changed slide's file (and the index, where the
 * order, the title or the cover changed) and whose removes drop a deleted
 * slide's file. The engine refuses a save whose base moved (`412
 * version_moved`) before a byte is written, and the draft is then merged onto
 * the newer version slide by slide: a slide only one side changed takes that
 * side's version; a slide both changed the same way is one change; a slide
 * both changed differently is the person's to resolve, never picked for them.
 * The order merges the same way, as one value.
 *
 * PURE: no fetch and no React. The screen hands it decks and source texts.
 */

import type { ArtifactDeck, DeckSlide, SlideNode } from "~/protocol/index.ts";
import { slideSource, titleOf } from "../slides.tsx";
import { mergeOrder, placeAfter } from "./orderMerge.ts";

/** The index's path, and where slide id's file is (`slideformat.IndexPath`, `slideformat.SlidePath`). */
export const DECK_INDEX = "deck.json";
export const slidePath = (id: string): string => `slides/${id}.html`;

/** One slide of a draft: what a person may change on it. */
export interface DraftSlide {
  id: string;
  root: SlideNode;
  notes: string;
  hidden: boolean;
  transition: string;
}

/** A deck as a person is editing it, against the version it started from. */
export interface DeckDraft {
  /** The version the draft started from: what its save is based on. */
  base: number;
  title: string;
  order: string[];
  slides: Readonly<Record<string, DraftSlide>>;
}

function draftSlide(s: DeckSlide): DraftSlide {
  return {
    id: s.id,
    root: s.root,
    notes: s.notes ?? "",
    hidden: !!s.hidden,
    transition: s.transition ?? "",
  };
}

/** A fresh draft of a version's deck. */
export function draftOf(deck: ArtifactDeck, version: number): DeckDraft {
  const slides: Record<string, DraftSlide> = {};
  for (const s of deck.slides ?? []) slides[s.id] = draftSlide(s);
  return { base: version, title: deck.title, order: (deck.slides ?? []).map((s) => s.id), slides };
}

/** A draft slide as the deck's slide shape, for drawing. */
export function deckSlide(s: DraftSlide): DeckSlide {
  return {
    id: s.id,
    path: slidePath(s.id),
    title: titleOf(s.root),
    hidden: s.hidden,
    transition: s.transition,
    notes: s.notes,
    root: s.root,
  };
}

/** Whether two slides are the same slide, compared as the engine would write them. */
export function sameSlide(a: DraftSlide | undefined, b: DraftSlide | undefined): boolean {
  if (!a || !b) return a === b;
  return slideSource(a) === slideSource(b);
}

/** What a draft changed against its base. */
export interface DraftChanges {
  /** Slides whose file is rewritten, in the draft's order. */
  slides: string[];
  /** Slides deleted. */
  removed: string[];
  /** Whether the index changes: the order, or the title. */
  index: boolean;
}

/** What draft changed against base, the deck it started from. */
export function changesOf(base: DeckDraft, draft: DeckDraft): DraftChanges {
  const slides = draft.order.filter((id) => !sameSlide(base.slides[id], draft.slides[id]));
  const removed = base.order.filter((id) => !draft.order.includes(id));
  const index = draft.title !== base.title || draft.order.join("\n") !== base.order.join("\n");
  return { slides, removed, index };
}

/** Whether a draft changed anything. */
export function changed(c: DraftChanges): boolean {
  return c.slides.length > 0 || c.removed.length > 0 || c.index;
}

/**
 * The index with the draft's title and order, every other key kept as it was
 * written: Claude's own keys, the faces, the sections. A cover or a section
 * that named a deleted slide moves to the slide after it, as Claude's format
 * says.
 */
export function indexSource(raw: string, base: DeckDraft, draft: DeckDraft): string {
  const index = JSON.parse(raw) as Record<string, unknown>;
  const order = draft.order;
  const moved = (id: unknown): unknown => {
    if (typeof id !== "string" || order.includes(id)) return id;
    const after = base.order.slice(base.order.indexOf(id) + 1).find((n) => order.includes(n));
    return after ?? order[order.length - 1];
  };
  index.title = draft.title;
  index.order = order;
  if (index.cover !== undefined) index.cover = moved(index.cover);
  if (index.sections && typeof index.sections === "object") {
    for (const s of Object.values(index.sections as Record<string, { start?: unknown }>)) {
      if (s && typeof s === "object") s.start = moved(s.start);
    }
  }
  return JSON.stringify(index, null, 2) + "\n";
}

/** The body of the publish that saves a draft (`POST /artifacts/{id}/versions`). */
export interface DeckPublish {
  mode: "patch";
  edits: { path: string; old_str: string; new_str: string }[];
  files: { path: string; remove: true }[];
  label?: string;
}

/**
 * The publish that saves draft against base, given the base version's source
 * of every file it rewrites (`sources`, by path). Each changed file is one
 * edit replacing its whole text, so the engine applies it to exactly the bytes
 * the draft was read from and refuses it if they moved.
 */
export function publishOf(
  base: DeckDraft,
  draft: DeckDraft,
  sources: Readonly<Record<string, string>>,
  label = "",
): DeckPublish {
  const c = changesOf(base, draft);
  const edits: DeckPublish["edits"] = [];
  for (const id of c.slides) {
    const path = slidePath(id);
    const next = slideSource(draft.slides[id]!);
    const old = sources[path];
    if (old === undefined) {
      throw new Error(`the source of ${path} was not read, so its edit has nothing to replace`);
    }
    if (old !== next) edits.push({ path, old_str: old, new_str: next });
  }
  if (c.index) {
    const old = sources[DECK_INDEX];
    if (old === undefined) throw new Error("the deck's index was not read");
    edits.push({ path: DECK_INDEX, old_str: old, new_str: indexSource(old, base, draft) });
  }
  return {
    mode: "patch",
    edits,
    files: c.removed.map((id) => ({ path: slidePath(id), remove: true })),
    ...(label ? { label } : {}),
  };
}

/** The paths whose base source a save of draft must read first. */
export function sourcesNeeded(base: DeckDraft, draft: DeckDraft): string[] {
  const c = changesOf(base, draft);
  return [...c.slides.map(slidePath), ...(c.index ? [DECK_INDEX] : [])];
}

// ---- the merge ------------------------------------------------------------ //

/** One thing both sides changed differently, for the person to resolve. */
export type DeckConflict =
  { kind: "slide"; id: string; title: string } | { kind: "order" } | { kind: "title" };

/** A draft moved onto a newer version. */
export interface DeckMerge {
  /** The merged draft, based on the newer version; a conflict holds the person's own side until resolved. */
  draft: DeckDraft;
  /** The newer version, as its own draft: the base the merged draft's save is against. */
  live: DeckDraft;
  /** The slides the newer version changed, added or removed, which the person is told about. */
  theirs: string[];
  conflicts: DeckConflict[];
}

/** How a person resolved one conflict. */
export type DeckResolution = "mine" | "theirs";

/**
 * Move mine (started from base) onto live, the newer version: a three-way
 * merge by slide, the order and the title each as one value.
 */
export function mergeDeck(base: DeckDraft, mine: DeckDraft, live: DeckDraft): DeckMerge {
  const conflicts: DeckConflict[] = [];
  const slides: Record<string, DraftSlide> = { ...live.slides };
  const theirs = [
    ...live.order.filter((id) => !sameSlide(base.slides[id], live.slides[id])),
    ...base.order.filter((id) => !live.order.includes(id)),
  ];
  const deleted = new Set<string>();
  for (const id of new Set([...base.order, ...mine.order])) {
    const b = base.slides[id];
    const m = mine.order.includes(id) ? mine.slides[id] : undefined;
    const l = live.order.includes(id) ? live.slides[id] : undefined;
    const mineChanged = !sameSlide(b, m);
    const liveChanged = !sameSlide(b, l);
    if (!mineChanged) continue;
    if (!liveChanged || sameSlide(m, l)) {
      if (m) slides[id] = m;
      else deleted.add(id);
      continue;
    }
    conflicts.push({ kind: "slide", id, title: titleOf((m ?? l ?? b)!.root) || id });
    if (m) slides[id] = m;
  }
  const merged = mergeOrder(base.order, mine.order, live.order, deleted);
  if (merged.conflict) conflicts.push({ kind: "order" });
  const order = merged.order;
  let title = live.title;
  if (mine.title !== base.title) {
    if (live.title !== base.title && live.title !== mine.title) conflicts.push({ kind: "title" });
    title = mine.title;
  }
  for (const id of order) if (!slides[id] && mine.slides[id]) slides[id] = mine.slides[id];
  return { draft: { base: live.base, title, order, slides }, live, theirs, conflicts };
}

/** A merge with one conflict resolved: the person's side kept, or the newer version's taken. */
export function resolve(
  merge: DeckMerge,
  conflict: DeckConflict,
  choice: DeckResolution,
): DeckMerge {
  const rest = merge.conflicts.filter((c) => c !== conflict);
  if (choice === "mine") return { ...merge, conflicts: rest };
  const d = merge.draft;
  const live = merge.live;
  switch (conflict.kind) {
    case "slide": {
      const slides = { ...d.slides };
      const theirs = live.slides[conflict.id];
      let order = d.order;
      if (theirs) slides[conflict.id] = theirs;
      else order = order.filter((id) => id !== conflict.id);
      if (theirs && !order.includes(conflict.id)) {
        order = [...order];
        order.splice(
          placeAfter(order, live.order.slice(0, live.order.indexOf(conflict.id))),
          0,
          conflict.id,
        );
      }
      return { ...merge, draft: { ...d, slides, order }, conflicts: rest };
    }
    case "order":
      return {
        ...merge,
        draft: { ...d, order: live.order.filter((id) => d.slides[id]) },
        conflicts: rest,
      };
    case "title":
      return { ...merge, draft: { ...d, title: live.title }, conflicts: rest };
  }
}
