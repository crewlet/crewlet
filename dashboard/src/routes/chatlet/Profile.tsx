/**
 * A seat's profile, beside the conversation: who a name in a room is.
 *
 * THE CHART'S SEAT AND THE ENGINE'S WORD FOR IT, read through [useChatPeople]
 * as every other chat surface reads them: the face, the team, and for an agent
 * what it is doing now. Its page in the org chart is one press away, because
 * this pane is a glance and the seat's page is the record.
 *
 * ROOMS IN COMMON ARE THE ENGINE'S ANSWER. Which of the reader's rooms the seat
 * is in is a membership question, and membership is the server's (a private
 * room's member list is not the reader's to enumerate), so the rooms come from
 * `chat_channels` narrowed to the rooms the seat is in, never from a member list
 * this screen happens to hold for one room.
 */

import { Button, ButtonLink, DescriptionList, Link, Skeleton, StatusDot, Tag } from "@crewlethq/ui";
import { MessageSquareGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";

import { href } from "~/app/router.tsx";
import { handleLabel, seatPath, stateWord, toneOf, useOrgIndex } from "~/lib/seats.ts";
import { useQuery } from "~/lib/useQuery.ts";

import { ChatFace } from "./Face.tsx";
import { useChatFrame, useChatNav } from "./nav.tsx";
import { ChatSidePanel } from "./PanePanel.tsx";
import { useChatPeople } from "./people.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { roomMark, roomTitle } from "./rooms.ts";

export function ProfilePane({ handle, onClose }: { handle: string; onClose: () => void }) {
  const nav = useChatNav();
  const frame = useChatFrame();
  const people = useChatPeople();
  const index = useOrgIndex();
  const person = people.person(handle);
  const me = handle === frame.viewer;
  const seat = index.byHandle.get(handle);
  const common = useQuery("chat_channels", { with: handle }, { enabled: !me });
  // THE READER'S OWN ROOMS ARE THEIR MEMBERSHIPS: the rail also lists rooms
  // they starred and are not in, which are not rooms they are in.
  const rooms = (
    me ? frame.channels.filter((summary) => summary.member) : (common.data?.channels ?? [])
  )
    .map((summary) => summary.channel)
    .filter((channel) => !isDirect(channel.kind));

  return (
    <ChatSidePanel title="Profile" onClose={onClose}>
      <div className="chat-profile">
        <ChatFace person={person} size={72} />
        <h3 className="chat-profile__name">{person.name}</h3>
        <p className="chat-profile__title">
          {person.team || (person.kind === "agent" ? "Agent seat" : "Person")}
          {person.kind === "agent" && <Tag size="xs">Agent</Tag>}
        </p>
        {person.kind === "agent" && person.state !== "offline" && (
          <StatusDot tone={toneOf(person.state)} pulse={person.state === "working"}>
            {stateWord(person.state)}
            {person.doing ? `: ${person.doing}` : ""}
          </StatusDot>
        )}
        <div className="chat-profile__actions">
          {!me && (
            <Button
              variant="primary"
              size="small"
              leadingIcon={<MessageSquareGlyph size="sm" />}
              onClick={() => nav.go({ kind: "new", to: [handle] })}
            >
              Message
            </Button>
          )}
          {/* THE SEAT'S PAGE, for a seat the chart holds: a handle it no
              longer holds has no page to open. */}
          {seat && (
            <ButtonLink variant="secondary" size="small" href={href(seatPath(seat))}>
              {person.kind === "agent" ? "Open agent" : "View in org chart"}
            </ButtonLink>
          )}
        </div>
      </div>
      <div className="chat-pane-section">
        <DescriptionList
          dense
          items={[
            ["Handle", handleLabel(person.handle)],
            ["Kind", person.kind === "agent" ? "Agent seat" : "Person"],
            [
              me ? "Your rooms" : "Rooms in common",
              !me && common.loading ? (
                <Skeleton variant="text" rows={1} label="Reading the rooms" />
              ) : !me && common.error ? (
                // COULD NOT SAY IS NOT "NONE".
                "Couldn’t be read just now"
              ) : rooms.length > 0 ? (
                <span className="chat-room-links">
                  {rooms.map((channel) => (
                    <Link
                      key={channel.id}
                      variant="default"
                      size="inherit"
                      className="chat-inline-mark"
                      href={nav.hrefOf({ kind: "room", id: channel.id })}
                    >
                      <NamedGlyph name={roomMark(channel.kind)} size="xs" />
                      {roomTitle(channel)}
                    </Link>
                  ))}
                </span>
              ) : (
                "None"
              ),
            ],
          ]}
        />
      </div>
    </ChatSidePanel>
  );
}
