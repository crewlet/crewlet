/**
 * What every place holding a conversation with Ask Crewlet reads of the
 * frame: why the reader may not ask, what is known to be wrong with its
 * model, whether a question carries images, and who is asking. One reading,
 * so the drawer and Ask Crewlet's page cannot come to say different things.
 */

import { useMemo } from "react";
import { useFrameSession } from "~/lib/frameSession.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { ownName, useViewer } from "~/lib/viewer.ts";
import {
  assistantModelFor,
  assistantUnavailableFor,
  attachClosedFor,
  type AssistantModelProblem,
  type AssistantUnavailable,
  type AttachClosed,
} from "./availability.ts";

export interface AskReading {
  /** Why the reader may not ask, or null where they may. */
  unavailable: AssistantUnavailable | null;
  /** Why the assistant's model cannot answer now, or null when nothing is known to be wrong. */
  model: AssistantModelProblem | null;
  /** Why a question cannot carry images, or null where it can. */
  attach: AttachClosed | null;
  /** The reader holds admin. */
  admin: boolean;
  /** The reader's own name, for the home's greeting. */
  name: string;
}

export function useAskReading(): AskReading {
  const viewer = useViewer();
  const org = useOrg();
  const session = useFrameSession();
  // ONE VALUE WHILE IT SAYS THE SAME THING: the frame's control carries the
  // reason to every message row in a room, and a fresh one on each render of
  // the frame (a health tick every five seconds) redrew them all.
  const companyName = org?.name ?? "";
  const unavailable = useMemo(
    () => assistantUnavailableFor(viewer, companyName),
    [viewer, companyName],
  );
  const readsImages = viewer.assistantReadsImages;
  const attach = useMemo(
    () => attachClosedFor({ assistantReadsImages: readsImages }),
    [readsImages],
  );
  return {
    unavailable,
    model: assistantModelFor(viewer),
    attach,
    admin: viewer.admin,
    name: ownName(session.answer),
  };
}
