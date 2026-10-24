/**
 * A seat's badge: the kit's `Avatar`, handed what the org chart says about the
 * seat.
 *
 * THE DRAWING IS THE KIT'S, all of it: the squircle for an agent and the
 * circle for a person, an agent's initials taken past its leading "agent"
 * word (the shape already says it, so "Agent CEO" and "Agent CTO" are CE and
 * CT rather than AC twice), the corner glyph for what a seat is doing, and an
 * accessible name that keeps the whole name. What stays here is the one thing
 * the kit cannot know: how the engine's org draws an agent ([AgentAvatar],
 * its character and colour), which is drawn in place of the initials where the
 * chart holds one. A person is always drawn as themself.
 *
 * `status` is the engine's own word (`lib/seats.ts`' `statusOf`), drawn as the
 * kit's corner badge, never a ring round the plate.
 */

import type { ComponentProps } from "react";
import { Avatar, type AvatarStackMember } from "@crewlethq/ui";
import type { AgentAvatar } from "~/lib/avatar.ts";

type AvatarProps = ComponentProps<typeof Avatar>;

/**
 * A seat's badge. Takes the kit Avatar's props; `name` and `kind` are
 * required, and `avatar` is how an agent is drawn where the chart holds one.
 */
export function SeatAvatar({
  name,
  kind,
  avatar,
  ...rest
}: Omit<AvatarProps, "name" | "kind" | "character" | "hue"> & {
  name: string;
  kind: "human" | "agent";
  avatar?: AgentAvatar | null | undefined;
}) {
  return <Avatar {...rest} {...faceOf(kind, avatar)} name={name} kind={kind} />;
}

/** The kit's character and hue for an agent drawn as itself; nothing for anybody else. */
function faceOf(
  kind: "human" | "agent",
  avatar: AgentAvatar | null | undefined,
): Pick<AvatarProps, "character" | "hue"> {
  return kind === "agent" && avatar ? { character: avatar.character, hue: avatar.color } : {};
}

/**
 * The badge a kit component draws for a seat itself (an `AvatarStack`'s
 * member) from the seat's name, the kind the chart holds and how an agent is
 * drawn. A kind the chart does not hold takes the agent's squircle, the kit's
 * own default: the engine runs agents, and a person is always declared.
 */
export function seatBadge(
  name: string,
  kind: string | undefined,
  avatar?: AgentAvatar | null,
): Pick<AvatarStackMember, "name" | "kind" | "character" | "hue"> & { kind: "human" | "agent" } {
  const k = kind === "human" ? "human" : "agent";
  return { name, kind: k, ...faceOf(k, avatar) };
}
