/**
 * What a dialog whose write is out says it is waiting for.
 *
 * NOT ALWAYS THE ENGINE. A write the engine refuses `403 step_up_required` is
 * held in `protocol/rest.ts` while "Confirm it is you" asks the person for
 * their password, and replayed once they have confirmed — so for as long as
 * that dialog is open, the one behind it is waiting for the PERSON. It read
 * "Waiting for the engine to answer." and pressed "Minting" while nothing was
 * waiting on the engine at all.
 */

import { useSyncExternalStore } from "react";
import { onStepUpAsked, stepUpAsked } from "~/protocol/index.ts";

export interface Waiting {
  /** A step-up is being asked of the person right now. */
  asked: boolean;
  /** Why a dialog cannot be closed while its write is out. */
  reason: string;
}

export function useWaiting(): Waiting {
  const asked = useSyncExternalStore(onStepUpAsked, stepUpAsked, () => false);
  return {
    asked,
    reason: asked ? "Waiting for you to confirm who you are." : "Waiting for the engine to answer.",
  };
}
