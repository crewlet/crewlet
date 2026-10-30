/**
 * A page with several boxes open at once, declared as ONE draft.
 *
 * # Why one draft
 *
 * The page context carries one draft, the newest any component declared
 * (`context.ts`), and a declaration is made again whenever what it says
 * moves. Two boxes that each declared their own therefore took turns being
 * the draft: every keystroke made the box typed in the only one the
 * assistant could read or fill, and a change it prepared for the other
 * landed nowhere. A chat room has its message box, the reply box of a thread
 * open beside it and each of the person's own messages open for a rewrite; a
 * task's page has its title and its description. So such a page declares one
 * draft whose fields are every box open on it, the pattern of every page with
 * several editors (the charter's parts,
 * `routes/settings/principles/edit/assistantAction.ts`).
 *
 * # Each box keeps its own text
 *
 * A box holds its text in its own state, so a keystroke renders the box
 * alone: lifted into the page, every box's text would render the page, a
 * transcript or a task's history included, on every keystroke in any of
 * them. Instead each box puts itself into the page's [DraftBoxes] as it
 * renders ([useDraftBox]), with its text, its own setter and the words it
 * refuses a value in, and a small child of the page reads the boxes
 * ([useOpenBoxes]) and declares the draft ([boxesDraft]).
 *
 * # A field is its box's
 *
 * A field reads its box's text and is set through the box's own setter once
 * the person approves, and a value the box would not take now is refused in
 * the box's own words. The draft's step names the box written in last, opened,
 * typed in or set, so a request about "this" reads the box the person was in.
 */

import { createContext, useContext, useLayoutEffect, useSyncExternalStore } from "react";
import { refuse, type AssistantDraftSpec } from "./draft.ts";

/** One box open on a page. */
export interface DraftBox {
  /** The field's path, lowercase and dotted: `message`, `rewrite.12`. */
  path: string;
  /** The box as the person reads it: "Message to #general". */
  label: string;
  /** What a value must be, in a few words: "Plain text". */
  hint: string;
  /** Where the field stands among the page's, lowest first. */
  rank: number;
  /** What the box holds now. */
  value: string;
  /** The box's own setter. */
  set: (next: string) => void;
  /** Why the box would not take `next` now, in the box's own words, or null. */
  refuses: (next: string) => string | null;
}

/** The boxes open on a page, in field order, and the path written in last. */
export interface OpenBoxes {
  readonly boxes: readonly DraftBox[];
  readonly last: string | null;
}

const NONE: OpenBoxes = { boxes: [], last: null };

/** The boxes open on one page: each box puts itself here, and the page declares them. */
export class DraftBoxes {
  private readonly open = new Map<string, DraftBox>();
  /** The paths in the order they were last written in, oldest first. */
  private written: string[] = [];
  private readonly listeners = new Set<() => void>();
  private current: OpenBoxes = NONE;

  /**
   * A box as it renders. Opened, or holding text it did not hold before, it
   * was written in. A render that moved neither its text nor its words on the
   * card publishes nothing: its setter and its refusals are read through the
   * latest render's box ([live]).
   */
  put(box: DraftBox): void {
    const before = this.open.get(box.path);
    this.open.set(box.path, box);
    if (
      before &&
      before.value === box.value &&
      before.label === box.label &&
      before.hint === box.hint &&
      before.rank === box.rank
    ) {
      return;
    }
    if (before?.value !== box.value) {
      this.written = [...this.written.filter((path) => path !== box.path), box.path];
    }
    this.publish();
  }

  /** A box that closed. */
  drop(path: string): void {
    if (!this.open.delete(path)) return;
    this.written = this.written.filter((written) => written !== path);
    this.publish();
  }

  /** What is open now: the same object until a box opens, closes or moves. */
  readonly read = (): OpenBoxes => this.current;

  readonly subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  private publish(): void {
    const boxes = [...this.open.values()]
      .sort((a, b) => a.rank - b.rank)
      .map((box) => this.live(box));
    this.current = boxes.length === 0 ? NONE : { boxes, last: this.written.at(-1) ?? null };
    for (const listener of [...this.listeners]) listener();
  }

  /** A box as the page reads it: its text as published, its setter and its words the latest render's. */
  private live(box: DraftBox): DraftBox {
    const { path } = box;
    return {
      ...box,
      set: (next) => this.open.get(path)?.set(next),
      refuses: (next) => this.open.get(path)?.refuses(next) ?? null,
    };
  }
}

/** The page's boxes, which every box in it puts itself into. */
export const DraftBoxesContext = createContext<DraftBoxes | null>(null);

/**
 * Put a box into the page's draft while it is open; null where it is not open
 * now (a room that takes no messages), which takes it out. Outside a page that
 * gathers its boxes it does nothing.
 */
export function useDraftBox(box: DraftBox | null): void {
  const boxes = useContext(DraftBoxesContext);
  const path = box?.path ?? null;
  // EVERY RENDER, so the page reads the box's text, setter and words as they
  // are now; the page declares again only when what its draft says moved.
  useLayoutEffect(() => {
    if (boxes && box) boxes.put(box);
  });
  useLayoutEffect(() => {
    if (!boxes || path === null) return;
    return () => boxes.drop(path);
  }, [boxes, path]);
}

/** The boxes open on a page now, read again whenever one opens, closes or moves. */
export function useOpenBoxes(boxes: DraftBoxes): OpenBoxes {
  return useSyncExternalStore(boxes.subscribe, boxes.read, boxes.read);
}

/**
 * The page's one draft over the boxes open on it, or null where none is.
 * The page says what the draft is of (`id`, `revision`), its scope line, its
 * help and, where something holds every box at once, why; each box is a text
 * field read from the box and set through its own setter, and the step is the
 * box written in last.
 */
export function boxesDraft(
  open: OpenBoxes,
  page: {
    id?: string;
    revision?: string | number;
    scope: string;
    help: string;
    blocked?: () => string | null;
  },
): AssistantDraftSpec<Readonly<Record<string, string>>> | null {
  if (open.boxes.length === 0) return null;
  return {
    kind: "client",
    id: page.id ?? null,
    revision: page.revision ?? null,
    step: open.last,
    scope: page.scope,
    help: page.help,
    value: Object.fromEntries(open.boxes.map((box) => [box.path, box.value])),
    fields: open.boxes.map((box) => ({
      path: box.path,
      label: box.label,
      hint: box.hint,
      read: (draft) => draft[box.path] ?? "",
      write: (draft, next) => {
        if (typeof next !== "string") return refuse(`${box.label} takes text.`);
        const why = box.refuses(next);
        return why === null ? { ...draft, [box.path]: next } : refuse(why);
      },
    })),
    apply: (next) => {
      for (const box of open.boxes) {
        const text = next[box.path];
        if (text !== undefined && text !== box.value) box.set(text);
      }
    },
    ...(page.blocked ? { blocked: page.blocked } : {}),
  };
}
