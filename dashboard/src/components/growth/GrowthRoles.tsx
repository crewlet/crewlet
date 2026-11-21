/**
 * Every role a vendor has, with what it allows there, which of Crewlet's seat
 * levels it serves, and the one Crewlet actually holds: so a person setting
 * up a product picks the least role that does the job, and sees at once when
 * the role held is above or below what the seats were given.
 */

import { Table, Tag, Text } from "@crewlethq/ui";
import type { GrowthLevel, GrowthRoleOffer } from "~/contract/growth.ts";
import "~/styles/growth.css";

const LEVEL_WORDS: Readonly<Record<GrowthLevel, string>> = {
  read: "Read",
  operate: "Operate",
  administer: "Administer, each act a decision",
};

/** What Crewlet offers a seat through a role, in a few words. */
export function levelsWords(levels: readonly GrowthLevel[]): string {
  if (levels.length === 0) return "Not used by Crewlet";
  return levels.map((level) => LEVEL_WORDS[level]).join(", ");
}

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
  return (
    <Table
      caption={`${product} roles`}
      captionHidden
      headers={["Role", "What it allows", "Crewlet offers"]}
      data={roles.map((role) => [
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
}
