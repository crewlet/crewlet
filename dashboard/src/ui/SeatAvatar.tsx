/**
 * A seat's badge: the kit's `Avatar`, handed the words that tell seats apart.
 *
 * # The shape already says "agent", so the initials do not have to
 *
 * The kit draws an agent as a squircle and a person as a circle, and makes the
 * initials from the first two words of whatever name it is given. A company
 * that names its agent seats for what they are — "Agent CEO", "Agent CTO",
 * "Agent PM" — therefore got `AC`, `AC` and `AP`: the two seats a chart draws
 * most were the same badge, side by side, and again in every "Unit lead" and
 * "Reports to" row. The one word they share is the word the OUTLINE already
 * carries, so [badgeName] drops it for an agent's badge and the initials come
 * from the words that differ: `CE`, `CT`, `PM`.
 *
 * ONLY FOR AN AGENT, and only a LEADING "agent" word, because that is the only
 * case where the word restates the shape: a person called "Agent Smith" is a
 * circle, and the word is their name. A name that is nothing but the word
 * keeps it, since an empty name draws the kit's anonymous badge instead.
 *
 * # What a reader hears is still the whole name
 *
 * The kit derives a badge's accessible name from the same `name` its initials
 * come from, so handing it the shortened words would announce "CEO avatar"
 * for a seat called "Agent CEO". The badge is therefore always drawn
 * decorative, and where it is NOT decorative — nothing beside it prints the
 * name — this wrapper is the image and carries the full name itself.
 */

import type { ComponentProps } from "react";
import { Avatar } from "@crewlethq/ui";

/** The words an agent's name leads with that its squircle already says. */
const KIND_WORD = /^agent(?:[\s._-]+)(?=\S)/i;

/**
 * The name a badge's initials are made from.
 *
 * An agent's leading "agent" word is dropped (`Agent CEO` → `CEO`,
 * `agent-cto` → `cto`); every other name is returned as it is.
 */
export function badgeName(name: string, kind: "human" | "agent"): string {
  if (kind !== "agent") return name;
  const rest = name.replace(KIND_WORD, "");
  return rest.trim() ? rest : name;
}

type AvatarProps = ComponentProps<typeof Avatar>;

/** A seat's badge. Takes the kit Avatar's props; `name` and `kind` are required. */
export function SeatAvatar({
  name,
  kind,
  decorative,
  ...rest
}: Omit<AvatarProps, "name" | "kind"> & { name: string; kind: "human" | "agent" }) {
  const badge = <Avatar {...rest} name={badgeName(name, kind)} kind={kind} decorative />;
  if (decorative) return badge;
  return (
    <span className="seat-avatar" role="img" aria-label={`${name} avatar`}>
      {badge}
    </span>
  );
}

/**
 * The badge a kit component draws for a seat itself — `OrgLabel`'s `avatar` —
 * from the seat's name and the kind the chart holds. A kind the chart does not
 * hold takes the agent's squircle, the kit's own default: the engine runs
 * agents, and a person is always declared.
 */
export function seatBadge(
  name: string,
  kind: string | undefined,
): { name: string; kind: "human" | "agent" } {
  const k = kind === "human" ? "human" : "agent";
  return { name: badgeName(name, k), kind: k };
}
