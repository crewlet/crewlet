/**
 * Settings › Backups & retention: the state log's domains, what is holding
 * each one's trim, and every node's place in it.
 *
 * `#/settings/backups` is the panels, and `#/settings/backups/{domain}` one
 * domain's page. Domains live ONLY under this segment, one level down, which
 * is what lets a domain be called anything: the fleet page used to hold nodes
 * and domains under one segment and tell them apart by the tail's length.
 */

import { QueryState } from "~/components/common.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { useEngineHealth } from "~/lib/store-hooks.ts";
import { apiToken } from "~/protocol/index.ts";
import { RetentionPanels } from "./Retention.tsx";
import { DomainScreen } from "./Domain.tsx";

export function Backups({ domain }: { domain?: string }) {
  const health = useEngineHealth();
  if (domain !== undefined) return <DomainScreen key={domain} name={domain} />;
  // THE SECTION IS OPERATOR-SCOPED AND SAYS SO. `RetentionPanels` draws
  // nothing for a reader with no token — it would otherwise refuse on every
  // poll — which on a page of its own is a blank screen under a heading that
  // promises backups. The refusal every other guarded answer draws is what a
  // reader here is owed, with its way to set a token.
  const operator = apiToken() !== "";
  return (
    <>
      <PageNote>
        Each state-log domain is an ordered log every node applies; its trim waits for the slowest
        node and for the newest backup that covers it.
      </PageNote>
      <QueryState error={operator ? null : "unauthorized"} loading={false}>
        <RetentionPanels thisNode={health?.node} />
      </QueryState>
    </>
  );
}
