/**
 * Whether this reader may write the company's charter: the ONE gate every
 * surface that offers to write it asks (Company › Principles' Edit and Add,
 * the org builder's company node), so the page and the builder can never
 * disagree about who is offered the editor.
 *
 * THE STANDING ANSWER, not this moment's: Admin, on a document this
 * deployment lets the reader's credential change (ADR-0030, the viewer's
 * `config_writer`), which is `mayChangeConfig`. Offline or still connecting
 * is "may write, not right now", which the controls say by being disabled
 * with the reason (`useConfigWriteAccess`), never by disappearing. While the
 * viewer is still being read the answer is no, so a control never appears
 * and then vanishes for a reader who turns out not to hold it.
 */

import { mayChangeConfig } from "~/lib/useWriteAccess.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";

/** [useMayWriteCharter] over values. */
export function mayWriteCharter(viewer: ViewerState): boolean {
  return !viewer.loading && mayChangeConfig(viewer);
}

/** Whether this reader may write the charter at all. */
export function useMayWriteCharter(): boolean {
  return mayWriteCharter(useViewer());
}
