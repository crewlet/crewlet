/**
 * A screen's toolbar: the sticky band of controls over the work it narrows.
 *
 * Its rhythm is `.toolbar`'s (styles/frame.css), and its controls stand at the
 * small step. It publishes its drawn height to the scroller (lib/useStickyBand.ts),
 * so a grid's column heads stick flush under it on every screen, wrapped or not.
 */

import type { HTMLAttributes } from "react";
import { useStickyBand } from "~/lib/useStickyBand.ts";

export function PageToolbar({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  const band = useStickyBand();
  return <div {...rest} ref={band} className={className ? `toolbar ${className}` : "toolbar"} />;
}
