/**
 * Every role a vendor has, with what it allows there, which of Crewlet's seat
 * levels it serves, and the one Crewlet actually holds: so a person setting
 * up a product picks the least role that does the job, and sees at once when
 * the role held is above or below what the seats were given.
 */

import { Disclosure, Table, Tag, Text } from "@crewlethq/ui";
import type { GrowthLevel, GrowthRoleOffer } from "~/contract/growth.ts";
import "~/styles/growth.css";

/** The growth levels as a person reads them, by what they let an agent do (`growth.Level.Label`). */
const LEVEL_WORDS: Readonly<Record<GrowthLevel, string>> = {
  read: "View",
  operate: "Manage",
  administer: "Administer, each act a decision",
};

/** What Crewlet offers a seat through a role, in a few words. */
export function levelsWords(levels: readonly GrowthLevel[]): string {
  if (levels.length === 0) return "Not used by Crewlet";
  // A LEVEL THIS BUILD DOES NOT KNOW is said as sent, never left blank.
  return levels.map((level) => LEVEL_WORDS[level] ?? level).join(", ");
}

/**
 * COMPACT: the roles that back a Crewlet level first, one line each (the role,
 * what it allows, the level it backs, the one held marked); the roles Crewlet
 * does not use fold away under one line, so the table is the few roles that
 * matter.
 */
export function GrowthRoles({
  product,
  roles,
  held,
}: {
  /** The product's name, for the table's caption. */
  product: string;
  roles: readonly GrowthRoleOffer[];
  /** The ids of the roles Crewlet holds, read from the vendor. */
  held: readonly string[];
}) {
  const used = roles.filter((r) => r.levels.length > 0 || held.includes(r.id));
  const unused = roles.filter((r) => !used.includes(r));
  const table = (rows: readonly GrowthRoleOffer[], caption: string) => (
    <Table
      caption={caption}
      captionHidden
      headers={["Role", "What it allows", "Backs"]}
      data={rows.map((role) => [
        <span key="role" className="growth-role">
          <span className="growth-role__name">{role.name}</span>
          {held.includes(role.id) ? (
            <Tag size="sm" variant="info">
              Held
            </Tag>
          ) : null}
        </span>,
        <Text key="note" as="span" className="growth-role__note">
          {role.note}
        </Text>,
        levelsWords(role.levels),
      ])}
    />
  );
  return (
    <div className="growth-roles">
      {used.length > 0 ? table(used, `${product} roles Crewlet uses`) : null}
      {unused.length > 0 ? (
        <Disclosure title={`Roles Crewlet does not use (${unused.length})`}>
          {table(unused, `${product} roles Crewlet does not use`)}
        </Disclosure>
      ) : null}
    </div>
  );
}
