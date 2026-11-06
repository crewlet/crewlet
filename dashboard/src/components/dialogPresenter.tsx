/**
 * Where a control opens its dialog: in place, or through the screen.
 *
 * A KIT MODAL OPENS INSIDE THE NEAREST LAYER HOST, and a TreeGrid draws one
 * around its rows so its own surfaces work in its fullscreen mode. A dialog a
 * row's control opened (Answer, Raise budget) was then drawn inside the
 * table's box rather than over the window, and on a scrolled phone its title
 * and close button sat under the page's sticky bar. A screen that draws
 * controls inside such a host provides a presenter from outside it, and the
 * controls hand it their dialog to draw there; anywhere else a control draws
 * its dialog in place, as before.
 *
 * A presented dialog is an element made when it opened, so it reads what it
 * shows itself (its own write, its own read of the run) rather than through
 * props a later render would have changed.
 */

import { createContext, useCallback, useContext, useState, type ReactNode } from "react";

type Present = (dialog: ReactNode | null) => void;

const Presenter = createContext<Present | null>(null);

/** Hands the controls under it a presenter, drawn where the screen renders what it presents. */
export const DialogPresenter = Presenter.Provider;

/**
 * A control's dialog: what to draw in place (nothing where a screen presents
 * it), and the way to show it or, with null, close it.
 */
export function usePresentedDialog(): [inPlace: ReactNode, show: Present] {
  const present = useContext(Presenter);
  const [local, setLocal] = useState<ReactNode | null>(null);
  const show = useCallback<Present>(
    (dialog) => (present ? present(dialog) : setLocal(dialog)),
    [present],
  );
  return [present ? null : local, show];
}
