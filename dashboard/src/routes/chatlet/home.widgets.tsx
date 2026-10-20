/**
 * Chatlet's widgets for Home: the rooms with something the reader has not read,
 * and the newest messages that named them.
 *
 * THE RAIL'S QUESTION AND THE FEED'S. Each widget asks what its page asks —
 * the rooms `chat_channels` lists in the rail, the newest page of
 * `chat_mentions` — and re-reads on the same live signals, so a count on Home
 * and the badge in the rail are one reading. Nothing here marks a room read:
 * a cursor moves only where the reader reads (`cursor.ts`).
 *
 * READ AS A SEAT, OR NOT ASKED. Every chat question is answered for the seat
 * the session holds and refused to a credential acting as none, so such a
 * reader is told so in one line and nothing is asked; and a company whose chat
 * is a vendor's or nobody's serves no chat question, so the widget says where
 * its chat is instead. Who may add either at all is the navigation's: the chat
 * row's app.
 */

import { useEffect, useMemo } from "react";
import { Count, List, ListItem } from "@crewlethq/ui";
import { BellGlyph, MessageSquareGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime } from "~/lib/format.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useClient, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { WidgetCard, WidgetNote, defineWidgets } from "~/routes/home/board/kit.tsx";
import { useChatActivity, useChatFrames, useChatRead, useCoalesced } from "./live.ts";
import { badgesAreMeasured, isDirect, roomMark, roomTitle, unreadLabel } from "./rooms.ts";
import { vendorChatName } from "./vendors.ts";

const ROWS = 5;

/**
 * Whether this reader's chat widgets ask anything, and the line each draws in
 * place of its rooms where they do not — the chat screen's own two answers,
 * said in a line: a company whose chat is not the engine's own, and a
 * credential acting as no seat. The first viewer read still out asks nothing
 * and says nothing, as the screen waits it out.
 */
function useChatHome(): { asks: boolean; note: string; viewer: string } {
  const viewer = useViewer();
  const backend = viewer.chatBackend;
  if (backend === "none") return { asks: false, note: "This company runs no chat.", viewer: "" };
  if (backend === "vendor")
    return {
      asks: false,
      note: `This company's chat is on ${vendorChatName(viewer.chatVendors)}.`,
      viewer: "",
    };
  if (viewer.unbound)
    return {
      asks: false,
      note: "Chatlet is read as a seat, and this sign-in holds none.",
      viewer: "",
    };
  return { asks: !viewer.asking, note: "", viewer: viewer.handle };
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
 * told only its most recently active are counted. WHAT IS LEFT OUT IS SAID IN
 * PLACE OF "CAUGHT UP" TOO: with nothing else to list, the empty state is
 * where those lines go, because "you are caught up" over a muted room with
 * messages waiting, or over rooms past the bound, is the claim they correct.
 */
function UnreadRooms() {
  const home = useChatHome();
  const rail = useQuery("chat_channels", {}, { enabled: home.asks });

  // THE RAIL'S OWN LIVE HALF: the answer seeds this tab's read state through
  // the store's ordering, any chat frame moves the list, and so does the
  // person's own read state once its revision passes what was drawn.
  const { store } = useClient();
  useEffect(() => store.applyChatRead(rail.data?.read), [store, rail.data]);
  useCoalesced(useChatActivity(), rail.refetch);
  const readRevision = useChatRead()?.revision ?? 0;
  const drawnRevision = rail.data?.read?.revision ?? 0;
  useCoalesced(readRevision, () => {
    if (readRevision > drawnRevision) rail.refetch();
  });

  const nameOf = useNameOf();
  const now = useNow();
  const measured = badgesAreMeasured(rail.data);
  const unread = (rail.data?.channels ?? []).filter((room) => room.unread > 0);
  const rooms = unread.filter((room) => !room.muted);
  const muted = unread.length - rooms.length;
  const leftOut = [
    muted > 0 ? `${plural(muted, "muted room")} with unread messages left out.` : "",
    rail.data?.truncated
      ? `You are in more rooms than Chatlet lists, so only your ${rail.data.channels.length} most recently active are counted here.`
      : "",
  ].filter(Boolean);
  return (
    <WidgetCard
      title={measured && rooms.length > 0 ? `Unread rooms · ${rooms.length}` : "Unread rooms"}
      from="chatlet"
    >
      {home.note ? (
        <WidgetNote>{home.note}</WidgetNote>
      ) : rail.data && !measured ? (
        <WidgetNote>
          Unread counts couldn&rsquo;t be read, so this can&rsquo;t tell which rooms have something
          new.
        </WidgetNote>
      ) : (
        <QueryState
          error={rail.error}
          refusal={rail.refusal}
          detail={rail.detail}
          loading={rail.loading && !rail.data}
          empty={
            rail.data && rooms.length === 0
              ? leftOut.length > 0
                ? { title: "Nothing unread to list", hint: leftOut.join(" ") }
                : {
                    title: "You are caught up",
                    hint: "A room with something you have not read lands here.",
                  }
              : undefined
          }
        >
          <List className="home-tasks">
            {rooms.slice(0, ROWS).map((room) => {
              const { channel } = room;
              const title = roomTitle(channel, {
                participants: room.participants,
                viewer: home.viewer,
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
                  href={href(["chatlet", channel.id])}
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
            <a className="home-card-more t-link" href={href(["chatlet"])}>
              {`All ${plural(rooms.length, "unread room")}`}
            </a>
          ) : null}
          {leftOut.map((line) => (
            <p key={line} className="home-card-note">
              {line}
            </p>
          ))}
        </QueryState>
      )}
    </WidgetCard>
  );
}

/** The newest messages that named the reader, as Mentions lists them. */
function NewestMentions() {
  const home = useChatHome();
  const first = useQuery("chat_mentions", { limit: ROWS }, { enabled: home.asks });
  // A MENTION ANYWHERE IS A FRAME SOMEWHERE, so the newest page re-reads on
  // every chat frame, coalesced, exactly as the Mentions screen's does.
  useChatFrames(null, first.refetch);
  const nameOf = useNameOf();
  const now = useNow();
  const mentions = first.data?.mentions ?? [];
  return (
    <WidgetCard title="Mentions" from="chatlet-mentions">
      {home.note ? (
        <WidgetNote>{home.note}</WidgetNote>
      ) : (
        <QueryState
          error={first.error}
          refusal={first.refusal}
          detail={first.detail}
          loading={first.loading && !first.data}
          empty={
            first.data && mentions.length === 0
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
                key={mention.message_id}
                // A MENTION IN A THREAD OPENS THE THREAD, as the feed's row does.
                href={href(
                  ["chatlet", mention.channel_id],
                  mention.thread_root ? { thread: mention.thread_root } : undefined,
                )}
                leading={<NamedGlyph name={roomMark(mention.channel_kind)} size="sm" />}
                trailing={<span className="home-tasks-when">{relTime(mention.at, now)}</span>}
              >
                <span className="home-tasks-stack">
                  <span className="home-tasks-title">
                    {mention.channel_name ? `#${mention.channel_name}` : "A direct conversation"}
                    {mention.thread_root ? " · in a thread" : ""}
                  </span>
                  <span className="home-tasks-meta">
                    {mention.author ? `${nameOf(mention.author)}: ` : ""}
                    {mention.excerpt || "…"}
                  </span>
                </span>
              </ListItem>
            ))}
          </List>
        </QueryState>
      )}
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
    from: "chatlet-mentions",
    label: "Mentions",
    description: "The newest messages that named you",
    icon: BellGlyph,
    hue: "purple",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: NewestMentions,
  },
]);
