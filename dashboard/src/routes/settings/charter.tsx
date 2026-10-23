/**
 * The frame both of the company's own pages draw in: Company › Overview (who
 * the company is) and Company › Policies (what every agent is told).
 *
 * # Read from the org projection, by every reader
 *
 * Both read the org PROJECTION rather than the guarded configuration, so a
 * reader without Admin reads them: the charter is not a secret, and it is the
 * company's own voice. Both are written in the org builder, so both offer it
 * in the bar, named for what the page edits and opening the company's editor
 * at it, and say when the chart on screen is still the previous revision.
 *
 * # Nothing is claimed before the chart arrives
 *
 * "No mission is set" and a policy count of 0 are statements about the
 * company, and before the org push lands this browser knows nothing about it
 * at all: a figure the engine did not answer is absent, never a zero. So the
 * page draws a skeleton until the projection is in, and only then its body.
 */

import type { ReactNode } from "react";
import { Card, Skeleton } from "@crewlethq/ui";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { EditCharterButton, type CharterPart } from "~/routes/agents/header.tsx";
import { useOrg } from "~/lib/store-hooks.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import { PreviousRevisionNote } from "~/routes/org/builder/AfterSaveStrip.tsx";

export function CharterPage({
  loading,
  edits,
  children,
}: {
  /** What the skeleton says it is waiting for: "Loading the company's charter". */
  loading: string;
  /** What the page's edit control opens: the whole charter, or its policies. */
  edits: CharterPart;
  /** The page's body, drawn only from a chart the engine has sent. */
  children: (org: OrgProjection) => ReactNode;
}) {
  const org = useOrg();
  return (
    <>
      {/* THE ORG CHART'S OWN CONTROL, the same button, named for what this
          page edits: "Edit org" over a page of policies opened the whole
          chart and left the reader to find the company card. */}
      <PageActions>
        <EditCharterButton part={edits} />
      </PageActions>
      <PreviousRevisionNote />
      {org ? (
        // ONE READABLE COLUMN for the company's own words: across the whole
        // canvas a policy ran to a line no reader follows, and a lone card
        // sat in an empty field. See `.charter-page`.
        <div className="col gap-4 charter-page">{children(org)}</div>
      ) : (
        <div aria-busy="true">
          <Card>
            <Skeleton variant="text" rows={4} label={loading} />
          </Card>
        </div>
      )}
    </>
  );
}
