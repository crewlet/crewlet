/**
 * A new HTML page: its own create page, in the shared frame (`CreateShell`).
 *
 * ONE PAGE, TWO WAYS IN. The page is the source: written, pasted, dropped or
 * picked, checked as it is written and previewed as a private draft. Asking
 * an agent to build it from a description is the hand-off panel beside it,
 * open as the page arrives because most people describe a page rather than
 * write one (`html/SourceBody.tsx`).
 */

import { SourceBody } from "./html/SourceBody.tsx";

export function HtmlCreate() {
  return <SourceBody />;
}
