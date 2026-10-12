/**
 * A tool's company-wide state, as the engine rolls it up, drawn the one way
 * every screen draws it: the Integrations screen's cards and the org builder's
 * integration cards say the same word in the same tone.
 */

import type { Tone } from "@crewlethq/ui";
import type { IntegrationTool, IntegrationToolState } from "~/contract/integrations.ts";

/**
 * The tone each state is drawn in.
 *
 * CONNECTED IS ONLY EVER GREEN, and nothing else is: the engine never sends
 * `connected` over a surface nothing can reach. `not_connected` is AMBER for the
 * reason every in-progress phase is: an integration that does not work yet,
 * which is not the neutral kept for a state nobody needs to come back to.
 * `not_in_use` is that neutral.
 */
export const TOOL_TONE: Record<IntegrationToolState, Tone> = {
  attention: "warning",
  not_connected: "warning",
  connected: "success",
  not_in_use: "neutral",
};

/** A tool's roll-up out of an `integrations` answer, or undefined where it carries none. */
export function toolRollup(
  tools: readonly IntegrationTool[] | undefined,
  key: string,
): IntegrationTool | undefined {
  return tools?.find((tool) => tool.key === key);
}
