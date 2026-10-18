import type { CoverageFacts } from "~/components/work.tsx";
import type { ChatServed } from "~/protocol/index.ts";
import { packPosition } from "./cursor.ts";

/**
 * How much of a room one read asks for.
 *
 * FIFTY — the page the engine reads when a question names none
 * (`chatlet.DefaultLimit`), asked for by name so the screen's page is the
 * screen's decision. It is a screen and a bit at this density, so opening a
 * room is one round trip rather than two, and paging back is a deliberate
 * gesture rather than something that happens while somebody scrolls. The
 * engine's ceiling (`chatlet.MaxLimit`, five hundred, what it clamps a larger
 * ask to) is for an export; asking for it here would make every room open by
 * reading ten screens nobody will look at.
 *
 * ONE CONSTANT for the transcript, the thread and the mention feed, because
 * they are the same question — how much conversation is a page — and three
 * numbers would be three answers that drift.
 */
export const PAGE = 50;

/**
 * What a chat answer says about its own coverage, as the frame's state bar
 * draws it (`usePageCoverage`): how fresh it was served, whether a deferred
 * record met it, and the point on the chat log it was derived through.
 *
 * EVERY VIEW PUBLISHES ITS OWN ANSWER'S, never the rail's: the rule is "the
 * answer this screen was drawn from", and an incomplete transcript or mention
 * feed under a state bar describing the rail is a warning nobody is shown.
 */
export function coverageOf(answer: ChatServed | null | undefined): CoverageFacts | null {
  if (!answer) return null;
  return {
    read_level: answer.read_level,
    complete: answer.complete,
    // PACKED, as every coverage fact's position is (`positionWords`): a bare
    // sequence reads as a point on the log's FIRST generation, which after a
    // re-anchor is another point than the one the answer was derived
    // through. A position this browser cannot hold exactly is no figure at
    // all rather than a rounded one ([packPosition]).
    applied_through: packPosition(answer.position) ?? undefined,
  };
}
