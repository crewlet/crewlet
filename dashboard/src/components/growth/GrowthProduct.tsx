/**
 * A growth product's own part of its integration page: the company
 * connections and the accounts each reaches, every role the vendor has with
 * what it allows and the ones Crewlet holds, and, for a spending product, the
 * caps per account. Read from the product's details in the company answer
 * (`connections`, `roles`), which the engine fills from its last pass.
 */

import { Text } from "@crewlethq/ui";
import type {
  GrowthAccount,
  GrowthAccountState,
  GrowthClass,
  GrowthConnection,
  GrowthLevel,
  GrowthRoleOffer,
} from "~/contract/growth.ts";
import type { IntegrationRow } from "~/contract/integrations.ts";
import { SPEND_PRODUCTS } from "./words.ts";
import { GrowthConnections } from "./GrowthConnections.tsx";
import { AnalyticsAttribution } from "./AnalyticsAttribution.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { GrowthRoles } from "./GrowthRoles.tsx";
import { RecentChanges } from "./RecentChanges.tsx";
import { SpendCapsSection } from "./SpendCaps.tsx";
import type { CapAccount } from "./caps.ts";
import "~/styles/growth.css";

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);
const strings = (v: unknown): string[] =>
  Array.isArray(v) ? v.filter((s): s is string => typeof s === "string") : [];
const text = (v: unknown): string => (typeof v === "string" ? v : "");

/**
 * The connections and roles a product's row carries, read defensively: a row
 * from an engine that sends neither, or a member this build cannot read, is
 * left out rather than drawn half.
 */
export function growthDetailsOf(row: IntegrationRow | undefined): {
  connections: GrowthConnection[];
  roles: GrowthRoleOffer[];
} {
  const connections = (Array.isArray(row?.connections) ? row.connections : [])
    .filter(isRecord)
    .map((c): GrowthConnection => ({
      id: text(c.id),
      name: text(c.name),
      identity: text(c.identity),
      state: text(c.state),
      roles_held: strings(c.roles_held),
      accounts: (Array.isArray(c.accounts) ? c.accounts : [])
        .filter(isRecord)
        .map((a): GrowthAccount => ({
          product: text(a.product),
          account: text(a.account),
          account_name: text(a.account_name),
          ...(typeof a.connection === "string" ? { connection: a.connection } : {}),
          ...(typeof a.currency === "string" ? { currency: a.currency } : {}),
          ...(typeof a.time_zone === "string" ? { time_zone: a.time_zone } : {}),
          state: (text(a.state) || "unknown") as GrowthAccountState,
          roles: strings(a.roles),
        })),
    }));
  const roles = (Array.isArray(row?.roles) ? row.roles : [])
    .filter(isRecord)
    .map((r): GrowthRoleOffer => ({
      id: text(r.id),
      name: text(r.name),
      note: text(r.note),
      classes: strings(r.classes) as GrowthClass[],
      levels: strings(r.levels) as GrowthLevel[],
    }));
  return {
    connections,
    roles: roles.filter((r) => r.id !== ""),
  };
}

/** Every account a product's connections reach, once, with the currency the vendor reports. */
export function capAccountsOf(connections: readonly GrowthConnection[]): CapAccount[] {
  const seen = new Map<string, CapAccount>();
  for (const connection of connections) {
    for (const account of connection.accounts) {
      if (!account.account || seen.has(account.account)) continue;
      seen.set(account.account, {
        id: account.account,
        name: account.account_name,
        currency: account.currency ?? "",
      });
    }
  }
  return [...seen.values()];
}

export function GrowthProductSection({
  kind,
  product,
  row,
}: {
  kind: string;
  product: string;
  row: IntegrationRow | undefined;
}) {
  const { connections, roles } = growthDetailsOf(row);
  const viewer = useViewer();
  const held = [...new Set(connections.flatMap((c) => c.roles_held))];
  return (
    <div className="growth-product">
      <section aria-labelledby={`${kind}-connections`}>
        <Text as="h3" variant="label" id={`${kind}-connections`}>
          Connections
        </Text>
        <GrowthConnections product={product} connections={connections} roles={roles} />
        {/* THE GOOGLE ANALYTICS LOGO BESIDE ITS DATA, as Google's policy asks, once there is some. */}
        {kind === "google_analytics" && connections.length > 0 ? <AnalyticsAttribution /> : null}
      </section>
      {roles.length === 0 ? null : (
        <section aria-labelledby={`${kind}-roles`}>
          <Text as="h3" variant="label" id={`${kind}-roles`}>
            {product} roles
          </Text>
          <Text as="p" tone="secondary">
            Give Crewlet the least role that does the job: each role below says what it allows at{" "}
            {product} and what Crewlet offers a seat through it.
          </Text>
          <GrowthRoles product={product} roles={roles} held={held} />
        </section>
      )}
      {SPEND_PRODUCTS.has(kind) ? (
        <SpendCapsSection kind={kind} accounts={capAccountsOf(connections)} />
      ) : null}
      {/* THE ACCESS LOG IS AN ADMINISTRATOR'S, so only one is shown what agents did here. */}
      {viewer.admin ? (
        <section aria-labelledby={`${kind}-changes`}>
          <Text as="h3" variant="label" id={`${kind}-changes`}>
            Recent agent changes
          </Text>
          <RecentChanges
            product={kind}
            accounts={
              new Map(capAccountsOf(connections).map((account) => [account.id, account.currency]))
            }
          />
        </section>
      ) : null}
    </div>
  );
}
