/**
 * The heavy-field versions of a live call no frame has moved yet — what a
 * fixture's `live_call` carries, since the engine names every version on every
 * surface that carries a call (`LiveCall.versions`).
 */

import type { CallVersions } from "~/protocol/types.ts";

export const ZERO_VERSIONS: CallVersions = {
  prompt: 0,
  response: 0,
  narration: 0,
  executions: 0,
  rounds: 0,
};
