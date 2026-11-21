/**
 * A product's company connections, several where the company has several
 * (App Store Connect teams, ad accounts' owners, Google identities): who
 * Crewlet is at the vendor through each, the roles it holds there, and every
 * account it reaches with the currency and zone its figures are in.
 *
 * NAMES WRAP AND IDS ARE WHOLE. An account's name is somebody's words and
 * may run long; its ID is how two accounts are told apart, so it is never cut.
 */

import { useId, type ReactNode } from "react";
import { Tag, Text } from "@crewlethq/ui";
import { AccountList } from "./AccountList.tsx";
import type { GrowthAccountState, GrowthConnection, GrowthRoleOffer } from "~/contract/growth.ts";
import "~/styles/growth.css";

const STATE_WORDS: Readonly<Record<GrowthAccountState, string>> = {
  serving: "Serving",
  paused: "Paused",
  suspended: "Suspended by the vendor",
  closed: "Closed",
  unapproved: "Not approved by the vendor yet",
  unknown: "State not read yet",
};

/** An account's state in words, or the engine's own word for a state this build does not know. */
export function accountStateWords(state: string): string {
  return STATE_WORDS[state as GrowthAccountState] ?? state;
}

/**
 * An identifier (an email, a key's name) with line-break opportunities at its
 * own boundaries, after `@`, `.`, `-` and `_`: one too long for a phone wraps
 * between its parts rather than inside one, and copies as it was.
 */
export function breakable(identifier: string): ReactNode[] {
  return identifier
    .split(/(?<=[@._-])/)
    .flatMap((part, index) => (index === 0 ? [part] : [<wbr key={index} />, part]));
}

export function GrowthConnections({
  product,
  connections,
  roles,
}: {
  product: string;
  connections: readonly GrowthConnection[];
  /** The product's roles, to name the ones each connection holds. */
  roles: readonly GrowthRoleOffer[];
}) {
  const base = useId();
  if (connections.length === 0) {
    return <Text as="p">{product} has no connection yet.</Text>;
  }
  const roleName = (id: string) => roles.find((role) => role.id === id)?.name ?? id;
  // A CONNECTION'S ID IS THE DOCUMENT'S, any text: the page's own ids are made here.
  const named = (index: number) => `${base}-${index}`;
  const nameOf = (connection: GrowthConnection) =>
    connection.name || connection.id || `${product} connection`;
  return (
    <div className="growth-connections">
      {connections.map((connection, index) => (
        <section
          key={`${index}-${connection.id}`}
          className="growth-connection"
          aria-labelledby={named(index)}
        >
          <div className="growth-connection__head">
            <Text as="h4" variant="label" id={named(index)} className="growth-connection__name">
              {nameOf(connection)}
            </Text>
            <span className="growth-connection__identity">{breakable(connection.identity)}</span>
            {/* Where it stands, in the engine's words: a team the engine cannot reach says so. */}
            {connection.state ? (
              <span className="growth-connection__state">{connection.state}</span>
            ) : null}
          </div>
          <div className="growth-connection__roles">
            {connection.roles_held.length === 0 ? (
              <Text as="span" tone="secondary">
                Holds no role Crewlet can read.
              </Text>
            ) : (
              connection.roles_held.map((id) => (
                <Tag key={id} size="sm">
                  {roleName(id)}
                </Tag>
              ))
            )}
          </div>
          {connection.accounts.length === 0 ? (
            <Text as="p" tone="secondary">
              Reaches no account yet.
            </Text>
          ) : (
            <AccountList
              label={`Accounts reached through ${nameOf(connection)}`}
              rows={connection.accounts.map((account) => ({
                key: account.account,
                name: account.account_name,
                id: account.account,
                facts: [
                  account.currency,
                  account.time_zone,
                  accountStateWords(account.state),
                ].filter((fact): fact is string => Boolean(fact)),
              }))}
            />
          )}
        </section>
      ))}
    </div>
  );
}
