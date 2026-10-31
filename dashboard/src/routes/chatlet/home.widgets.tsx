/**
 * Chatlet's widgets for Home: the rooms with something the reader has not read,
 * and the newest messages that named them.
 *
 * THE RAIL'S READING AND THE ACTIVITY FEED'S. The rooms are the frame's one
 * reading of the reader's rooms (`lib/chatRooms.ts`), the same answer the
 * Chatlet screen's list and the sidebar's badge draw, so a count on Home and
 * the badge in the rail are one reading by construction. The mentions are the
 * newest page of the reader's Activity narrowed to mentions (`chat_activity`),
 * re-read on every chat frame as Activity is. Nothing here marks a room read:
 * a cursor moves only where the reader reads (`cursor.ts`).
 *
 * READ AS A SEAT, OR NOT ASKED. Every chat question is answered for the seat
 * the session holds and refused to a credential acting as none, so such a
 * reader is told so in one line and nothing is asked. A company whose chat is
 * a vendor's or nobody's is offered neither widget, since Home's board hides
 * what reads a service the company does not run (`kit.tsx`'s `widgetShown`),
 * and nothing is asked before the engine has said it runs Chatlet
 * (`lib/services.ts`). Who may add either at all is the navigation's: the
 * chat row's app.
 */

import { useMemo, type ReactNode } from "react";
import { Count, List, ListItem } from "@crewlethq/ui";
import { BellGlyph, MessageSquareGlyph } from "@crewlethq/icons/glyphs";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime } from "~/lib/format.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useServicesAsk } from "~/lib/services.ts";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import {
  WidgetCard,
  WidgetNote,
  WidgetQuery,
  defineWidgets,
  gapsOf,
} from "~/routes/home/board/kit.tsx";
import { useChatActivity, useCoalesced } from "~/lib/chatLive.ts";
import { badgesAreMeasured, isDirect, useChatRooms } from "~/lib/chatRooms.ts";
import { bodyExcerpt } from "./Body.tsx";
import { placeHref } from "./nav.tsx";
import { roomMark, roomTitle, unreadLabel } from "./rooms.ts";

const ROWS = 5;

/**
 * Whether this reader's chat widgets ask anything, and the line each draws in
 * place of its rooms where they do not: the chat screen's own answer to a
 * credential acting as no seat, said in a line. The first viewer read still
 * out asks nothing and says nothing, as the screen waits it out, and so does
 * a company the engine has not yet said runs Chatlet.
 */
function useChatHome(): { asks: boolean; note: string; viewer: string } {
  const viewer = useViewer();
  const runs = useServicesAsk().chat;
  if (viewer.unbound)
    return {
      asks: false,
      note: "Chatlet is read as a seat, and this sign-in holds none.",
      viewer: "",
    };
  return { asks: runs && !viewer.asking, note: "", viewer: viewer.handle };
}

/** A handle to a display name, off the chart. */
function useNameOf(): (handle: string) => string {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  return useMemo(() => (handle: string) => index.byHandle.get(handle)?.name ?? handle, [index]);
}

/**
 * The reader's rooms with something unread, newest activity first.
 *
 * THE COUNT IS THE ENGINE'S, against the seat's own read cursor, as the rail
 * draws it; where the cursors could not be read the engine zeroes every count
 * and says so, and this says it cannot tell rather than "caught up". A MUTED
 * room is left out — the reader asked it not to call for them — and counted in
 * a line under the list, so a muted room is never silently missing; and a
 * reader in more rooms than the rail lists (`truncated`, the engine's bound) is
 * told only its most recently active are counted. A room this build cannot
 * present (`unreadable`) may hold unread messages too, and a read that met a
 * record this build cannot decode (`complete: false`) may be missing rooms, so
 * both are said beside the list as well. WHAT IS LEFT OUT IS SAID IN PLACE OF
 * "CAUGHT UP" TOO: those lines are the widget's gaps (the kit's
 * `WidgetQuery`), because "you're caught up" over a muted room with messages
 * waiting, or over rooms past the bound, is the claim they correct.
 */
function UnreadRooms() {
  const home = useChatHome();
  // THE READING IS CLAIMED ONLY WHERE THE WIDGET ASKS: mounting it claims
  // the frame's reading, so a reader chat answers nothing for, or a company
  // the engine has not said runs Chatlet, is asked nothing at all.
  if (!home.asks) {
    return (
      <WidgetCard title="Unread rooms" from="chatlet">
        {home.note ? <WidgetNote>{home.note}</WidgetNote> : null}
      </WidgetCard>
    );
  }
  return <UnreadRoomsRead viewer={home.viewer} />;
}

function UnreadRoomsRead({ viewer }: { viewer: string }) {
  // THE FRAME'S ONE READING of the reader's rooms, which re-reads on every
  // chat frame and on the reader's own read state while it is drawn.
  const rail = useChatRooms();
  const nameOf = useNameOf();
  const now = useNow();
  const measured = badgesAreMeasured(rail.data);
  const unread = (rail.data?.channels ?? []).filter((room) => room.unread > 0);
  const rooms = unread.filter((room) => !room.muted);
  const muted = unread.length - rooms.length;
  const unreadable = rail.data?.unreadable ?? 0;
  const leftOut = [
    ...gapsOf([rail.data]),
    muted > 0 ? `${plural(muted, "muted room")} with unread messages left out.` : "",
    rail.data?.truncated
      ? `You’re in more rooms than Chatlet lists, so only your ${rail.data.channels.length} most recently active are counted here.`
      : "",
    unreadable > 0
      ? `${plural(unreadable, "room")} came from a newer version of the engine and ${unreadable === 1 ? "isn’t" : "aren’t"} counted.`
      : "",
  ].filter(Boolean);
  return (
    <WidgetCard
      title={measured && rooms.length > 0 ? `Unread rooms · ${rooms.length}` : "Unread rooms"}
      from="chatlet"
    >
      {rail.data && !measured ? (
        <WidgetNote>
          Unread counts couldn&rsquo;t be read, so this can&rsquo;t tell which rooms have something
          new.
        </WidgetNote>
      ) : (
        <WidgetQuery
          error={rail.error}
          refusal={rail.refusal}
          detail={rail.detail}
          loading={rail.loading && !rail.data}
          gaps={leftOut}
          empty={
            rail.data && rooms.length === 0
              ? {
                  title: "You’re caught up",
                  hint: "A room with something you haven’t read lands here.",
                }
              : undefined
          }
        >
          <List className="home-tasks">
            {rooms.slice(0, ROWS).map((room) => {
              const { channel } = room;
              const title = roomTitle(channel, {
                participants: room.participants,
                viewer,
                nameOf,
              });
              const said = room.last
                ? `${room.last.author ? `${nameOf(room.last.author)}: ` : ""}${
                    room.last.deleted ? "(deleted)" : room.last.excerpt || "…"
                  }`
                : "";
              return (
                <ListItem
                  key={channel.id}
                  href={placeHref({ kind: "room", id: channel.id })}
                  leading={<NamedGlyph name={roomMark(channel.kind)} size="sm" />}
                  trailing={
                    <Count
                      value={unreadLabel(room.unread, room.unread_capped)}
                      label={`unread in ${title}`}
                    />
                  }
                >
                  <span className="home-tasks-stack">
                    <span className="home-tasks-title">
                      {isDirect(channel.kind) ? title : `#${title}`}
                    </span>
                    <span className="home-tasks-meta">
                      {[said, room.last ? relTime(room.last.at, now) : ""]
                        .filter(Boolean)
                        .join(" · ")}
                    </span>
                  </span>
                </ListItem>
              );
            })}
          </List>
          {rooms.length > ROWS ? (
            <a className="home-card-more t-link" href={placeHref({ kind: "activity" })}>
              {`${plural(rooms.length, "unread room")} in Chatlet`}
            </a>
          ) : null}
        </WidgetQuery>
      )}
    </WidgetCard>
  );
}

/** The newest messages that named the reader, as Activity's Mentions lists them. */
function NewestMentions() {
  const home = useChatHome();
  const feed = useQuery("chat_activity", { kind: "mention", limit: ROWS }, { enabled: home.asks });
  // A MENTION ANYWHERE IS A FRAME SOMEWHERE, so the newest page re-reads on
  // every chat frame, coalesced, exactly as Activity does.
  useCoalesced(useChatActivity(), feed.refetch);
  const nameOf = useNameOf();
  const now = useNow();
  const mentions = feed.data?.items ?? [];
  let body: ReactNode = null;
  if (home.note) body = <WidgetNote>{home.note}</WidgetNote>;
  else if (home.asks) {
    body = (
      <WidgetQuery
        error={feed.error}
        refusal={feed.refusal}
        detail={feed.detail}
        loading={feed.loading && !feed.data}
        gaps={gapsOf([feed.data])}
        empty={
          feed.data && mentions.length === 0
            ? {
                title: "Nobody has named you",
                hint: "A message that names your handle lands here.",
              }
            : undefined
        }
      >
        <List className="home-tasks">
          {mentions.map((mention) => (
            <ListItem
              key={mention.key}
              // A MENTION IN A THREAD OPENS THE THREAD, as Activity's row does.
              href={
                mention.thread_root
                  ? placeHref(
                      { kind: "room", id: mention.channel_id },
                      { kind: "thread", room: mention.channel_id, root: mention.thread_root },
                    )
                  : placeHref({ kind: "room", id: mention.channel_id, message: mention.message_id })
              }
              leading={<NamedGlyph name={roomMark(mention.channel_kind)} size="sm" />}
              trailing={<span className="home-tasks-when">{relTime(mention.at, now)}</span>}
            >
              <span className="home-tasks-stack">
                <span className="home-tasks-title">
                  {mention.channel_name ? `#${mention.channel_name}` : "A direct conversation"}
                  {mention.thread_root ? " · in a thread" : ""}
                </span>
                <span className="home-tasks-meta">
                  {`${nameOf(mention.actor)}: `}
                  {bodyExcerpt(mention.excerpt) || "…"}
                </span>
              </span>
            </ListItem>
          ))}
        </List>
      </WidgetQuery>
    );
  }
  return (
    <WidgetCard title="Mentions" from="chatlet">
      {body}
    </WidgetCard>
  );
}

export const widgets = defineWidgets([
  {
    key: "chatlet.unread",
    from: "chatlet",
    label: "Unread rooms",
    description: "Your rooms with messages you have not read",
    icon: MessageSquareGlyph,
    hue: "purple",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: UnreadRooms,
  },
  {
    key: "chatlet.mentions",
    from: "chatlet",
    label: "Mentions",
    description: "The newest messages that named you",
    icon: BellGlyph,
    hue: "purple",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: NewestMentions,
  },
]);
