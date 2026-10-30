/**
 * What the chat screen's own suites mount that is not a screen. Imported by
 * tests only.
 *
 * HERE RATHER THAN IN `test/chat.tsx`, which holds what every chat suite
 * needs of the FRAME (the act route, the socket, the viewer): a kit outside
 * `routes/` that reached into a screen's module would pull the screen into
 * the entry chunk the moment anything else imported it, which is what
 * `app/lazy.test.tsx` refuses.
 */

import { useOutbox } from "./outbox.ts";
import { Room, type RoomProps } from "./Room.tsx";

/**
 * A room mounted on its own, with the outbox the chat screen holds above every
 * room (`Chat.tsx`) — what a suite about one room needs, and no second idea
 * of what that outbox is.
 */
export function LoneRoom(props: Omit<RoomProps, "outbox">) {
  return <Room {...props} outbox={useOutbox()} />;
}
