import { useEffect, useId } from "react";

/**
 * The props of an element that takes the focus when the control that held it
 * goes away: its id, `tabIndex: -1` so a script can focus it and a Tab press
 * cannot stop on it, and a name read off its own words.
 */
export interface FocusLanding {
  id: string;
  tabIndex: -1;
  "aria-labelledby": string;
}

/**
 * Where the focus lands once `active` turns true.
 *
 * A PRESS THAT REPLACES ITS OWN BUTTON (an option sending an answer, whose
 * outcome line takes the options' place) drops the focus to the document's
 * body: a keyboard reader's next Tab starts again from the top of the page,
 * and a screen reader is left nowhere near what the press did. The element
 * spread with these props takes it instead, and ONLY WHILE NOTHING ELSE
 * HOLDS IT: a reader who moved on while the write was out keeps their place.
 */
export function useFocusLanding(active: boolean): FocusLanding {
  const id = useId();
  useEffect(() => {
    if (!active) return;
    const held = document.activeElement;
    if (held && held !== document.body) return;
    document.getElementById(id)?.focus();
  }, [active, id]);
  return { id, tabIndex: -1, "aria-labelledby": id };
}
