/**
 * Whether an element is narrower than a width, from its OWN width (see
 * [useWidth]). A board's frame takes its `columns`, so a narrow board passes
 * every frame the whole grid rather than overriding the span it set.
 */

import type { RefObject } from "react";
import { useWidth } from "./useWidth.ts";

export function useNarrowerThan(
  node: RefObject<HTMLElement | null>,
  below: number,
  ready = true,
): boolean {
  const width = useWidth(node, ready);
  return width > 0 && width < below;
}
