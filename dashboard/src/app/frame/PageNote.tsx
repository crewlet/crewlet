/**
 * One sentence saying what a screen answers.
 *
 * NOT A HEADER. The page bar's breadcrumb is the title now, so what is left of
 * the old screen header is the sentence that explained the screen — and that
 * sentence is worth keeping: this product's screens answer questions whose
 * shape is not obvious from a noun ("seat ownership is a lease with a fencing
 * epoch"), and a reader who does not know that reads the table wrongly.
 *
 * It is deliberately a muted line rather than a panel: a reader who has read it
 * once should be able to scan past it for ever after.
 */

import type { ReactNode } from "react";

export function PageNote({ children }: { children: ReactNode }) {
  return <p className="page-note">{children}</p>;
}
