/**
 * The layer host a scrolling or panning surface has to tell when it moves.
 *
 * A surface anchored to something inside a pannable canvas or a scrolling grid
 * follows it by listening for `LAYER_REPOSITION_EVENT` on the layer it was
 * rendered into, so the surface that MOVES has to dispatch it there. A
 * `LayerHost` provides that element through context to everything inside it
 * and exposes no ref, which is exactly right for the overlays that consume it
 * and leaves the container with no way to name its own host. This reports it
 * back up.
 *
 * @crewlethq/ui has the same bridge for its own Canvas and does not export it;
 * this file goes when the engine's canvas and outline become uilet's.
 */

import { useLayoutEffect, useRef } from "react";
import { useLayerContainer } from "@crewlethq/ui";

export function LayerNode({ onNode }: { onNode: (el: HTMLElement | null) => void }) {
  const container = useLayerContainer();
  const report = useRef(onNode);
  report.current = onNode;
  useLayoutEffect(() => {
    // The document body is the fallback when there is no host, and a surface
    // portalled into the body is not inside this container at all.
    report.current(container && container !== document.body ? container : null);
  }, [container]);
  return null;
}
