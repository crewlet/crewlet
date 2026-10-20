/**
 * An element's own width, kept current as it changes: a page is a container
 * whose width the sidebar and the peek decide, so the window's says nothing.
 * Zero until the element is laid out.
 *
 * `ready` re-measures once the element exists: a screen that draws a skeleton
 * first mounts the measured element later than the hook.
 */

import { useLayoutEffect, useState, type RefObject } from "react";

export function useWidth(node: RefObject<HTMLElement | null>, ready = true): number {
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = node.current;
    if (!el || !ready) return;
    const measure = () => setWidth(el.clientWidth);
    measure();
    const watch = new ResizeObserver(measure);
    watch.observe(el);
    return () => watch.disconnect();
  }, [node, ready]);
  return width;
}
