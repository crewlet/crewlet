/**
 * A seat's face in chat: the kit's `Avatar`, through `ui/SeatAvatar`, handed
 * what [useChatPeople] read about the seat.
 *
 * THE DRAWING IS THE KIT'S, all of it (the shape, the character on its hue,
 * the status badge on the corner); what is chat's is only which seat and
 * whether the badge is drawn. A row that is about something other than the
 * seat's state (a chip, a deleted message's preview, an activity row whose
 * corner carries the activity's own mark) draws the face without its badge.
 */

import type { ReactNode } from "react";

import type { AvatarSize, AvatarStackMember } from "@crewlethq/ui";
import { SeatAvatar, seatBadge } from "~/ui/SeatAvatar.tsx";

import type { ChatPerson } from "./people.ts";

export function ChatFace({
  person,
  size = "md",
  status = true,
  decorative = true,
  mark,
  markLabel,
}: {
  person: ChatPerson;
  size?: AvatarSize;
  /** Draw the seat's state on the face's corner. */
  status?: boolean;
  /** The face beside the name says nothing the name does not; a face alone names its seat. */
  decorative?: boolean;
  /**
   * A mark on the corner in place of the state: what a row is about where
   * the seat's state is not (an activity's kind). Wins over `status`.
   */
  mark?: ReactNode;
  markLabel?: string;
}) {
  return (
    <SeatAvatar
      name={person.name}
      kind={person.kind}
      avatar={person.avatar}
      size={size}
      decorative={decorative}
      {...(mark !== undefined
        ? { mark, markLabel }
        : status && person.status
          ? { status: person.status }
          : {})}
    />
  );
}

/** A seat as a kit stack (`AvatarStack`) draws it, keyed on its handle. */
export function stackMember(person: ChatPerson): AvatarStackMember {
  return { id: person.handle, ...seatBadge(person.name, person.kind, person.avatar) };
}
