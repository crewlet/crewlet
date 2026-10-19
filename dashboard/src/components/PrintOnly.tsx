/**
 * What the browser prints while it is mounted, and nothing else of the
 * dashboard: drawn always, shown only in print, so Print is the browser's own
 * dialog with nothing to prepare (a deck's slides one to a page, a doc's tabs
 * in order). It sits directly in the body, where the print stylesheet hides
 * every sibling.
 */

import type { ReactNode } from "react";
import { createPortal } from "react-dom";

export function PrintOnly({ className, children }: { className?: string; children: ReactNode }) {
  return createPortal(
    <div className={className ? `print-only ${className}` : "print-only"} aria-hidden="true">
      {children}
    </div>,
    document.body,
  );
}
