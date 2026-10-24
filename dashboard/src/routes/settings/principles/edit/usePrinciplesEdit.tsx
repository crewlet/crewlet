/**
 * How Company › Principles writes the charter: in place, each part in its own
 * card (`useCharterEditing.tsx` says why per part rather than a page mode),
 * for an administrator on a document this deployment lets them change
 * (`access.ts`). The page calls this once and places what it returns
 * (`../PrincipleCard.tsx` declares the shape).
 *
 * A READER WHO MAY NOT WRITE IT is handed no control at all, rather than a
 * disabled one beside every part. The one reader told why is an ADMINISTRATOR
 * of a document another system manages (ADR-0030), who would otherwise look
 * for an Edit that is not there: the notice says where the charter is
 * changed instead.
 */

import { Callout } from "@crewlethq/ui";
import { managedBlock } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import type { PrinciplesCardsEdit } from "../PrincipleCard.tsx";
import { partEdit } from "./partEdit.tsx";
import { useCharterEditing } from "./useCharterEditing.tsx";

export function usePrinciplesEdit(org: OrgProjection): PrinciplesCardsEdit {
  const { editing, dialog } = useCharterEditing(org);
  const viewer = useViewer();
  const managed = !viewer.loading && viewer.admin ? managedBlock(viewer) : null;
  return {
    mission: partEdit(editing, "mission"),
    vision: partEdit(editing, "vision"),
    policies: partEdit(editing, "policies"),
    notice: (
      <>
        {managed && <Callout variant="info">{managed}</Callout>}
        {dialog}
      </>
    ),
  };
}
