/**
 * Publishes a sticky toolbar's drawn height to its scroller as
 * `--toolbar-band`, which `--sticky-top` reads (styles/frame.css), so what
 * sticks under the bar starts flush below it even when the bar wraps.
 * Measured when the bar resizes, never on scroll.
 */

import { useCallback, type RefCallback } from "react";

export function useStickyBand(): RefCallback<HTMLElement> {
  return useCallback((bar: HTMLElement | null) => {
    const scroller = bar?.closest<HTMLElement>(".crewlet-app-shell__main");
    if (!bar || !scroller) return undefined;
    const publish = () =>
      scroller.style.setProperty("--toolbar-band", `${bar.getBoundingClientRect().height}px`);
    publish();
    const watch = new ResizeObserver(publish);
    watch.observe(bar);
    return () => {
      watch.disconnect();
      scroller.style.removeProperty("--toolbar-band");
    };
  }, []);
}
