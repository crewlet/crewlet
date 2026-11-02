/**
 * The top of a room, and the quiet lines that say what happened to it.
 *
 * THE INTRO stands where the transcript reaches the room's very beginning:
 * the room's name in large type, who made it and when, its description with
 * Edit beside it, and while the room is new (nothing has been said in it, as
 * opposed to people joining it) a row of first steps: add people and agents,
 * add a description, start from a template. A direct conversation names
 * everybody in it instead, since it is the people in it.
 *
 * EACH FIRST STEP IS OFFERED ONLY WHERE IT CAN BE TAKEN: a card is an
 * invitation, and one whose change the room or the browser would refuse is
 * not drawn at all (the details pane draws the same change held, with its
 * sentence). Which change that is, and why it is refused, is the room scope's
 * one reading of the room's rules ([RoomScope.rules]).
 *
 * A ROOM EVENT is a typed row the engine writes on the room's own log (a
 * join, people added, a description or topic set, a template): not
 * conversation, so it is one quiet line with an icon in the face's column,
 * never a message with a face, and it breaks a run of speech.
 */

import { Fragment, type ReactNode } from "react";

import { ActionCard, ActionCardRow, ArtifactKindIllustration, Button } from "@crewlethq/ui";
import {
  LayoutGridGlyph,
  PencilGlyph,
  PlusGlyph,
  UserGlyph,
  LogOutGlyph,
  UserPlusGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, readerDay } from "~/lib/format.ts";
import { dateTimeFormat, type DateShape } from "~/lib/formatters.ts";
import { zone } from "~/lib/prefs.ts";
import type { ChatChannel, ChatMessageView } from "~/protocol/index.ts";

import { dayOf, daysBetween, timeOf } from "./days.ts";
import { ChatFace } from "./Face.tsx";
import { useChatNav, useOpenRoom } from "./nav.tsx";
import { useChatPeople, type ChatPerson } from "./people.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { roomMark, roomWhere } from "./rooms.ts";
import { useRoomScope } from "./scope.tsx";

/** Faces a group conversation's intro shows; the title and the words name the rest. */
const GROUP_FACES = 4;

/** Names an event line spells out before it counts the rest. */
const EVENT_NAMES = 2;

const MONTH_DAY: DateShape = Object.freeze({ month: "long", day: "numeric" });

/** "today", "yesterday", or "on October 27", in the reader's zone. */
function madeWhen(createdAt: string, today: string): string {
  const days = daysBetween(dayOf(createdAt), today);
  if (days === 0) return "today";
  if (days === 1) return "yesterday";
  const at = Date.parse(createdAt);
  return Number.isFinite(at)
    ? `on ${dateTimeFormat(undefined, zone(), MONTH_DAY).format(new Date(at))}`
    : "";
}

/**
 * Who did something to a room, as its intro and its event lines say it:
 * "You", a seat's name, or the org chart, which is nobody's name (a team's
 * room is made and filled by the chart, and its actor's handle is nothing a
 * reader knows).
 */
function actorName(
  handle: string,
  kind: string | undefined,
  viewer: string,
  nameOf: (handle: string) => string,
): string {
  if (kind === "system") return "The org chart";
  return handle === viewer ? "You" : nameOf(handle);
}

/** Whether anything has been SAID in a room: events are not conversation. */
export function isNewRoom(messages: readonly ChatMessageView[]): boolean {
  return !messages.some((view) => !view.message.event);
}

export function RoomIntro({
  channel,
  participants,
  title,
  fresh,
}: {
  channel: ChatChannel;
  /** Who is in a direct conversation (the rail's `participants`). */
  participants: readonly string[];
  /** The room's name, or who a conversation is with. */
  title: string;
  /** Nothing has been said in the room yet. */
  fresh: boolean;
}) {
  const scope = useRoomScope();
  const nav = useChatNav();
  const people = useChatPeople();
  const today = readerDay(useNow());

  if (isDirect(channel.kind)) {
    const others = participants
      .filter((handle) => handle !== scope.viewer)
      .map((handle) => people.person(handle));
    return <DirectIntro others={others} title={title} />;
  }

  const creator = channel.created_by
    ? actorName(channel.created_by, channel.created_by_kind, scope.viewer, people.nameOf)
    : "";
  const when = madeWhen(channel.created_at, today);
  const open = !channel.archived_at && scope.member;
  const canAdd = open && scope.rules.members === "";
  const canDescribe = open && scope.rules.settings === "";
  const dialog = (kind: "people" | "description" | "template") =>
    nav.openDialog({ kind, room: channel.id });

  const cards: ReactNode[] = [];
  if (fresh && canAdd) {
    cards.push(
      <ActionCard
        key="people"
        tone="purple"
        title="Add people and agents"
        art={<PeopleArt />}
        onSelect={() => dialog("people")}
      />,
    );
  }
  if (fresh && canDescribe && !channel.purpose) {
    cards.push(
      <ActionCard
        key="description"
        tone="blue"
        title="Add a description"
        art={<ArtifactKindIllustration kind="doc" />}
        onSelect={() => dialog("description")}
      />,
    );
  }
  if (fresh && canDescribe && !channel.topic && !channel.purpose) {
    cards.push(
      <ActionCard
        key="template"
        tone="amber"
        title="Start from a template"
        art={<ArtifactKindIllustration kind="slides" />}
        onSelect={() => dialog("template")}
      />,
    );
  }
  const glyph = roomMark(channel.kind);
  const what = channel.kind === "unit" && channel.unit ? `team room for ${channel.unit}` : "room";

  return (
    <header className={`chat-intro${fresh ? " is-new" : ""}`}>
      <h2 className="chat-intro-title">
        <span className="chat-intro-mark" aria-hidden="true">
          <NamedGlyph name={glyph} size="xl" />
        </span>
        {title}
      </h2>
      <p className="chat-intro-text">
        {creator
          ? `${creator} created this ${what}${when ? ` ${when}` : ""}. `
          : `This ${what} was created${when ? ` ${when}` : ""}. `}
        This is the very beginning of the{" "}
        <strong className="chat-intro-room">
          <span className="chat-intro-mark" aria-hidden="true">
            <NamedGlyph name={glyph} size="sm" />
          </span>
          {title}
        </strong>{" "}
        room.
      </p>
      {channel.purpose && (
        <p className="chat-intro-description">
          {channel.purpose}
          {canDescribe && (
            <button type="button" className="chat-intro-edit" onClick={() => dialog("description")}>
              Edit
            </button>
          )}
        </p>
      )}
      {cards.length > 0 ? (
        <ActionCardRow label="First steps for this room" className="chat-intro-cards">
          {cards}
        </ActionCardRow>
      ) : (
        (canAdd || (canDescribe && !channel.purpose)) && (
          <div className="chat-intro-actions">
            {canAdd && (
              <Button
                size="small"
                variant="secondary"
                leadingIcon={<UserPlusGlyph size="sm" />}
                onClick={() => dialog("people")}
              >
                Add people
              </Button>
            )}
            {canDescribe && !channel.purpose && (
              <Button
                size="small"
                variant="ghost"
                leadingIcon={<PencilGlyph size="sm" />}
                onClick={() => dialog("description")}
              >
                Add a description
              </Button>
            )}
          </div>
        )
      )}
    </header>
  );
}

/**
 * A direct conversation's intro: everybody in it, by face and by name. A
 * group showed only its first member's face once, and read as a conversation
 * with one seat.
 */
function DirectIntro({ others, title }: { others: readonly ChatPerson[]; title: string }) {
  const nav = useChatNav();
  const scope = useRoomScope();
  const group = others.length > 1;
  const agents = others.filter((p) => p.kind === "agent");
  const solo = others[0];
  return (
    <header className="chat-intro is-direct">
      {others.length > 0 && (
        <div className="chat-intro-faces">
          {others.slice(0, GROUP_FACES).map((p) => (
            <ChatFace key={p.handle} person={p} size={72} />
          ))}
        </div>
      )}
      <h2 className="chat-intro-title">{title}</h2>
      {others.length === 0 ? (
        // A CONVERSATION WITH NOBODY ELSE IN IT, which the engine allows and
        // the title calls "Just you": "between you and" named nobody.
        <p className="chat-intro-text">
          Only you are in this conversation. Here you can send yourself messages and keep links.
        </p>
      ) : (
        <p className="chat-intro-text">
          This conversation is just between you{group ? ", " : " and "}
          {others.map((p, i) => (
            <Fragment key={p.handle}>
              {i === 0 ? "" : i === others.length - 1 ? " and " : ", "}
              <strong>{p.name}</strong>
            </Fragment>
          ))}
          .
          {agents.length === 1
            ? ` ${agents[0]!.name} answers here, and its work shows up as it happens.`
            : agents.length > 1
              ? " The agents in it answer here, and their work shows up as it happens."
              : " Here you can send messages and share links."}
        </p>
      )}
      {solo && (
        <div className="chat-intro-actions">
          {group ? (
            <Button
              size="small"
              variant="secondary"
              leadingIcon={<UsersGlyph size="sm" />}
              onClick={() =>
                nav.openPane({ kind: "details", room: scope.channelID, tab: "members" })
              }
            >
              View members
            </Button>
          ) : (
            <Button
              size="small"
              variant="secondary"
              leadingIcon={<UserGlyph size="sm" />}
              onClick={() => nav.openPane({ kind: "profile", handle: solo.handle })}
            >
              View profile
            </Button>
          )}
        </div>
      )}
    </header>
  );
}

/**
 * The Add people card's drawing: a team of four seats, three of them the
 * company's agents and the fourth open with a plus, which says "add someone"
 * without a badge floating over the faces.
 */
function PeopleArt() {
  const people = useChatPeople();
  const members = new Set((useOpenRoom()?.detail.data?.members ?? []).map((m) => m.handle));
  const faces = people.everyone
    .filter((p) => p.kind === "agent" && !members.has(p.handle))
    .slice(0, 3);
  return (
    <span className="chat-people-art" aria-hidden="true">
      {faces.map((p) => (
        <span key={p.handle} className="chat-people-art-seat">
          <ChatFace person={p} size={38} status={false} />
        </span>
      ))}
      <span className="chat-people-art-seat is-open">
        <PlusGlyph size="md" />
      </span>
    </span>
  );
}

/** Up to two names as profile links, then how many more. */
function Names({ handles }: { handles: readonly string[] }) {
  const nav = useChatNav();
  const scope = useRoomScope();
  const people = useChatPeople();
  const shown = handles.slice(0, EVENT_NAMES);
  const rest = handles.length - shown.length;
  return (
    <>
      {shown.map((handle, i) => (
        <Fragment key={handle}>
          {i > 0 && (rest > 0 ? ", " : " and ")}
          <button
            type="button"
            className="chat-event-name"
            onClick={() => nav.openPane({ kind: "profile", handle })}
          >
            {handle === scope.viewer ? "You" : people.nameOf(handle)}
          </button>
        </Fragment>
      ))}
      {rest > 0 && ` and ${rest} ${rest === 1 ? "other" : "others"}`}
    </>
  );
}

/** A room event, as one quiet line. */
export function EventRow({ view }: { view: ChatMessageView }) {
  const scope = useRoomScope();
  const nav = useChatNav();
  const people = useChatPeople();
  const { message } = view;
  const event = message.event!;
  const who = actorName(message.author, message.author_kind, scope.viewer, people.nameOf);
  const where = scope.channel?.name
    ? roomWhere(scope.channel.kind, scope.channel.name)
    : "this room";
  const handles = event.handles ?? [];
  let icon: ReactNode = <UserPlusGlyph size="sm" />;
  let words: ReactNode;
  switch (event.kind) {
    case "joined":
      words = (
        <>
          <Names handles={handles} /> joined {where}.
        </>
      );
      break;
    case "added":
      words = (
        <>
          <span className="chat-event-who">{who}</span> added <Names handles={handles} /> to {where}
          .
        </>
      );
      break;
    case "left":
      icon = <LogOutGlyph size="sm" />;
      words = (
        <>
          <Names handles={handles} /> left {where}.
        </>
      );
      break;
    case "removed":
      icon = <LogOutGlyph size="sm" />;
      words = (
        <>
          <span className="chat-event-who">{who}</span> removed <Names handles={handles} /> from{" "}
          {where}.
        </>
      );
      break;
    case "description":
    case "topic": {
      icon = <PencilGlyph size="sm" />;
      const what = event.kind === "description" ? "description" : "topic";
      words = event.text ? (
        <>
          <span className="chat-event-who">{who}</span> set the room {what}:{" "}
          <span className="chat-event-quote">{event.text}</span>
        </>
      ) : (
        <>
          <span className="chat-event-who">{who}</span> cleared the room {what}.
        </>
      );
      break;
    }
    case "template":
      icon = <LayoutGridGlyph size="sm" />;
      words = (
        <>
          <span className="chat-event-who">{who}</span> started this room from the{" "}
          <span className="chat-event-quote">{event.text}</span> template.
          <button
            type="button"
            className="chat-event-link"
            onClick={() => nav.openPane({ kind: "details", room: scope.channelID, tab: "about" })}
          >
            See the details
          </button>
        </>
      );
      break;
    default:
      // A KIND THIS BUILD DOES NOT KNOW is said as a change to the room, by
      // whom, rather than dropped: the row is on the log either way.
      icon = <PencilGlyph size="sm" />;
      words = (
        <>
          <span className="chat-event-who">{who}</span> changed {where}.
        </>
      );
  }
  return (
    <div className="chat-event" data-message-id={message.id}>
      <span className="chat-event-icon" aria-hidden="true">
        {icon}
      </span>
      <p className="chat-event-text">
        {words}{" "}
        <time
          className="chat-event-time"
          dateTime={message.created_at}
          title={fmtDateTime(message.created_at)}
        >
          {timeOf(message.created_at)}
        </time>
      </p>
    </div>
  );
}
