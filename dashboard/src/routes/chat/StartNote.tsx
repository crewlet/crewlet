/**
 * What a start gesture answered, on screen.
 *
 * ONE RENDERING FOR FOUR DIALOGS — creating a room, opening a direct
 * conversation, joining, leaving — because the four outcomes are the same four
 * wherever the gesture was made, and the one that must never be drawn as
 * success is the same one every time. Four copies would be four chances for
 * the pending case to become a tick.
 */

import { Callout } from "@crewlethq/ui";

import { calloutFor, type StartReport } from "./start.ts";

export function StartNote({ report }: { report: StartReport | null }) {
  if (!report || report.note === "") return null;
  return (
    // ANNOUNCED POLITELY, never assertively: this appears IN RESPONSE to a
    // button the reader just pressed and is already looking at, so it is news
    // — but it is not an interruption.
    <Callout variant={calloutFor(report.tone)} live="polite">
      {report.note}
    </Callout>
  );
}
