/**
 * Whether an element is narrower than a width, from its OWN width: a page is
 * a container whose width the sidebar and the peek decide, so the window's
 * says nothing. A board's frame takes its `columns`, so a narrow board passes
 * every frame the whole grid rather than overriding the span it set.
 *
 * `ready` re-measures once the element exists: a screen that draws a skeleton
 * first mounts the measured element later than the hook.
 */

import { useLayoutEffect, useState, type RefObject } from "react";

export function useNarrowerThan(
  node: RefObject<HTMLElement | null>,
  below: number,
  ready = true,
): boolean {
  const [narrow, setNarrow] = useState(false);
  useLayoutEffect(() => {
    const el = node.current;
    if (!el || !ready) return;
    const measure = () => setNarrow(el.clientWidth > 0 && el.clientWidth < below);
    measure();
    const watch = new ResizeObserver(measure);
    watch.observe(el);
    return () => watch.disconnect();
  }, [node, below, ready]);
  return narrow;
}
