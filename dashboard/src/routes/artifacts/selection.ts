/**
 * What a gallery's reader has picked for a bulk action, and whether they are
 * picking: the Select toggle in the toolbar, the boxes on the cards and the
 * rows, and the `SelectionBar` that acts on them.
 *
 * # Picking is a mode, never a hover
 *
 * A card's box shows while the reader is choosing (the toggle is on, or
 * anything is picked), and the card's body then picks rather than opens, so a
 * reader is never sent away mid-selection (`ArtifactCard selecting`). With
 * nothing picked and the toggle off the gallery is a gallery again.
 *
 * # Picks belong to one question
 *
 * The picks are cleared when the question the listing asks changes (another
 * type, order, phrase, scope or folder): a bulk action acting on rows the
 * reader can no longer see would act on something they did not look at.
 *
 * # A range is the drawn order
 *
 * Shift with a card's box picks every card between it and the box pressed
 * before it, in the order the gallery draws them (`order`), set to the state
 * of the box pressed, never toggled card by card. The list's own range is the
 * grid's (`DataGrid selection`), which hands back the whole set.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

export interface GallerySelection {
  /** The reader is choosing: the toggle is on, or something is picked. */
  selecting: boolean;
  /** The toolbar's Select toggle. */
  toggled: boolean;
  setToggled: (on: boolean) => void;
  /** The picked artifacts, by id. */
  selected: ReadonlySet<string>;
  /** Pick or clear one, or with `range` every one drawn between it and the last pressed. */
  pick: (id: string, on: boolean, range: boolean) => void;
  /** Replace every pick: what the list's head box and its ranges hand back. */
  replace: (next: ReadonlySet<string>) => void;
  /** Clear every pick and stop choosing. */
  clear: () => void;
}

/**
 * The selection over a gallery drawing `order` (every id on screen, top to
 * bottom), kept for as long as the listing asks `question`.
 */
export function useGallerySelection(order: readonly string[], question: string): GallerySelection {
  const [toggled, setToggled] = useState(false);
  const [selected, setSelected] = useState<ReadonlySet<string>>(() => new Set());
  const anchor = useRef<string | null>(null);
  const latestOrder = useRef(order);
  latestOrder.current = order;

  // A NEW QUESTION CLEARS THE PICKS, and leaves the toggle as the reader set
  // it: they are still choosing, from what the new question shows.
  useEffect(() => {
    setSelected(new Set());
    anchor.current = null;
  }, [question]);

  const pick = useCallback((id: string, on: boolean, range: boolean) => {
    // THE SPAN IS READ NOW, from the anchor as it stands before this press:
    // an updater runs later, after the anchor has moved to this card.
    const drawn = latestOrder.current;
    const to = drawn.indexOf(id);
    const from = range && anchor.current !== null ? drawn.indexOf(anchor.current) : -1;
    const span =
      from >= 0 && to >= 0 ? drawn.slice(Math.min(from, to), Math.max(from, to) + 1) : [id];
    anchor.current = id;
    setSelected((was) => {
      const next = new Set(was);
      for (const one of span) {
        if (on) next.add(one);
        else next.delete(one);
      }
      return next;
    });
  }, []);

  const replace = useCallback((next: ReadonlySet<string>) => setSelected(new Set(next)), []);

  const clear = useCallback(() => {
    setSelected(new Set());
    setToggled(false);
    anchor.current = null;
  }, []);

  return useMemo(
    () => ({
      selecting: toggled || selected.size > 0,
      toggled,
      setToggled: (on: boolean) => (on ? setToggled(true) : clear()),
      selected,
      pick,
      replace,
      clear,
    }),
    [toggled, selected, pick, replace, clear],
  );
}
