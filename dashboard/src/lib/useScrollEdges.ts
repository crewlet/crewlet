/**
 * Which ends of a sideways scroller have more past them, as the two data
 * attributes the sheet fades an edge on (`data-more-start`, `data-more-end`).
 *
 * A ROW THAT SCROLLS WITH NO SCROLLBAR HAS TO SAY SO. On a phone the page's
 * controls, a list's shapes and its filter bar each take one line and scroll
 * rather than stack, with the bar hidden because it would sit on the text —
 * and a control cut at the edge ("All wo", "Ta", "No g") read as a broken label
 * rather than as more to swipe to. A fade on the edge that has more is the
 * hint; measured again on every scroll and resize, and absent entirely on a row
 * that fits. (The section tabs do not scroll: what does not fit there folds
 * into "More" — see `SectionTabs` in `app/header/PageHeader.tsx`.)
 *
 * ONE HOOK FOR EVERY SCROLLER, so every one fades by the same rule: it lived
 * inside the page header, and the list's two scrollers — written after it —
 * never faded at all.
 */

import { useLayoutEffect, useState, type RefObject } from "react";

export function useScrollEdges(el: RefObject<HTMLElement | null>): {
  "data-more-start"?: "";
  "data-more-end"?: "";
} {
  const [edges, setEdges] = useState({ start: false, end: false });
  useLayoutEffect(() => {
    const node = el.current;
    if (!node) return;
    const measure = () => {
      const start = node.scrollLeft > 1;
      const end = node.scrollLeft + node.clientWidth < node.scrollWidth - 1;
      setEdges((was) => (was.start === start && was.end === end ? was : { start, end }));
    };
    measure();
    const watch = new ResizeObserver(measure);
    watch.observe(node);
    node.addEventListener("scroll", measure, { passive: true });
    return () => {
      watch.disconnect();
      node.removeEventListener("scroll", measure);
    };
  });
  return {
    ...(edges.start ? { "data-more-start": "" as const } : {}),
    ...(edges.end ? { "data-more-end": "" as const } : {}),
  };
}
