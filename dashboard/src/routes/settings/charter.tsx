/**
 * The frame both of the company's own pages draw in: Company › Overview (who
 * the company is) and Company › Policies (what every agent is told).
 *
 * # Read from the org projection, by every reader
 *
 * Both read the org PROJECTION rather than the guarded configuration, so a
 * reader without Admin reads them: the charter is not a secret, and it is the
 * company's own voice. Both are written in the org builder, so both offer it
 * in the bar and say when the chart on screen is still the previous revision.
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
import { href } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { ORG_EDIT_PATH } from "~/app/routes.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import { PreviousRevisionNote } from "~/routes/org/builder/AfterSaveStrip.tsx";

export function CharterPage({
  loading,
  children,
}: {
  /** What the skeleton says it is waiting for: "Loading the company's charter". */
  loading: string;
  /** The page's body, drawn only from a chart the engine has sent. */
  children: (org: OrgProjection) => ReactNode;
}) {
  const org = useOrg();
  return (
    <>
      <PageActions>
        <a className="t-link" href={href(ORG_EDIT_PATH)}>
          Edit in org →
        </a>
      </PageActions>
      <PreviousRevisionNote />
      {org ? (
        children(org)
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
