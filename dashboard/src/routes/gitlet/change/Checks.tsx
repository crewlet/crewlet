/**
 * A change's Checks tab: the attempts that judge it — its head's, checked for
 * its target, whichever change's dispatch queued them (`code_checks` with
 * `change=`) — each opening in the pane every checks listing shares.
 */

import { useMemo } from "react";
import { refName, shortOID } from "~/lib/code.ts";
import type { CodeChangeSummary } from "~/protocol/index.ts";
import { CheckListing } from "../checks/CheckList.tsx";

export function Checks({ change }: { change: CodeChangeSummary }) {
  const params = useMemo(
    () => ({ repo: change.repo, change: change.number }),
    [change.repo, change.number],
  );
  return (
    <CheckListing
      params={params}
      label={`Checks of ${shortOID(change.head)}`}
      showRepo={false}
      empty={{
        title: "No check has run on this change's head",
        hint: `A head's checks are the ones .crewlet/checks.yaml on ${refName(change.target_ref).name} defines, queued once its revision is recorded.`,
      }}
    />
  );
}
