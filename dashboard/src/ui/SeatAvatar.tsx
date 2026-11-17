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
 * ONE RULE FOR EVERY SURFACE: an agent the chart holds is its character
 * wherever it is drawn, alone or beside its name; a person, and a key the chart
 * does not hold, are initials. `avatar` is a required input so no caller can
 * leave an agent's character behind by omission.
 *
 * `status` is the engine's own word (`lib/seats.ts`' `statusOf`), drawn as the
 * kit's corner badge, never a ring round the plate.
 */

import type { ComponentProps } from "react";
import { Avatar, type AvatarStackMember } from "@crewlethq/ui";
import { UserGlyph } from "@crewlethq/icons/glyphs";
import { faceOf, type AgentAvatar } from "~/lib/avatar.ts";
import type { SeatBadge } from "~/lib/seats.ts";

type AvatarProps = ComponentProps<typeof Avatar>;

/**
 * A seat's badge. Takes the kit Avatar's props; `name`, `kind` and `avatar`
 * are required, `avatar` being the chart's answer (null for a person,
 * undefined for a key the chart does not hold).
 */
export function SeatAvatar({
  name,
  kind,
  avatar,
  ...rest
}: Omit<AvatarProps, "name" | "kind" | "character" | "hue"> & {
  name: string;
  kind: "human" | "agent";
  avatar: AgentAvatar | null | undefined;
}) {
  return <Avatar {...rest} {...faceOf(kind, avatar)} name={name} kind={kind} />;
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
  avatar: AgentAvatar | null | undefined,
): Pick<AvatarStackMember, "name" | "kind" | "character" | "hue"> & { kind: "human" | "agent" } {
  const k = kind === "human" ? "human" : "agent";
  return { name, kind: k, ...faceOf(k, avatar) };
}

/**
 * A seat's face beside its name in a picker (a Select option's `leading`, a
 * Menu item's `icon`), off the chart's [SeatBadge]; `null` is the Nobody choice.
 * `text` sizes it to a line of words, as in a filter pill.
 */
export function ChoiceFace({
  seat,
  size = "xs",
}: {
  seat: Pick<SeatBadge, "name" | "kind" | "avatar"> | null;
  size?: "xs" | "text";
}) {
  if (!seat) return <NobodyFace />;
  return (
    <SeatAvatar
      name={seat.name}
      kind={seat.kind === "human" ? "human" : "agent"}
      avatar={seat.avatar}
      size={size}
      decorative
    />
  );
}

/**
 * Nobody, as a face: the dashed empty square an unassigned task and a picker's
 * "Nobody" choice draw, the size of the smallest badge. `label` names it where
 * no word is printed beside it.
 */
export function NobodyFace({ label }: { label?: string | undefined }) {
  return (
    <span className="seat-nobody" title={label} aria-hidden={label ? undefined : true}>
      <UserGlyph size="xs" />
      {label && <span className="sr-only">{label}</span>}
    </span>
  );
}
