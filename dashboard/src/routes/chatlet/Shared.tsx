/**
 * What a room shared: every task, page, artifact and pull request posted in
 * it, newest first, under the room's details.
 *
 * THE ENGINE'S INDEX, THE ROOM'S UNFURL. The list is `chat_links`, each link of
 * each message that carried one, exactly as it was sent; each is drawn by the
 * one rendering a message draws its links with (`MessageLinks.tsx`), so a
 * link reads the same here as under the message it came in, by the same link
 * rules: an address the company's hosts do not name is drawn inert, never
 * followed.
 *
 * Each row says who shared it and when, and opens the message it came in
 * (its thread, for one said in a thread).
 */

import { Button } from "@crewlethq/ui";

import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import type { ChatLinksAnswer, ChatSharedLink } from "~/protocol/index.ts";

import { useChatActivity, useCoalesced } from "~/lib/chatLive.ts";
import { MessageUnfurls } from "./MessageLinks.tsx";
import { useChatNav } from "./nav.tsx";
import { useChatPeople } from "./people.ts";

/** How many links a page holds: one screen of the pane, a few times over. */
const LINKS_PAGE = 30;

const linksOn = (answer: ChatLinksAnswer) => answer.links;
const linkKey = (link: ChatSharedLink) => `${link.message_id}:${link.url}`;
const nextOf = (answer: ChatLinksAnswer) => answer.next_cursor ?? "";

export function SharedList({ channelID }: { channelID: string }) {
  const nav = useChatNav();
  const now = useNow();
  const people = useChatPeople();
  const { socket } = useClient();
  const asked = { channel_id: channelID, limit: LINKS_PAGE };
  const first = useQuery("chat_links", asked);
  const links = usePaged(
    first,
    asked,
    (cursor: string) =>
      socket.query(
        "chat_links",
        withFloor("chat_links", JSON.stringify({ ...asked, cursor })),
      ) as Promise<ChatLinksAnswer>,
    linksOn,
    linkKey,
    nextOf,
  );
  // A MESSAGE POSTED ANYWHERE may carry a link here, so the first page is
  // asked again on what the socket has seen, as the rail is.
  useCoalesced(useChatActivity(), first.refetch);

  return (
    <div className="chat-pane-section">
      <QueryState
        error={links.error}
        refusal={links.refusal}
        detail={first.detail}
        loading={links.loading}
        empty={
          links.items.length === 0
            ? {
                title: "Nothing shared yet",
                hint: "Tasks, pages, artifacts and pull requests posted here are listed here.",
              }
            : undefined
        }
      >
        <ul className="chat-shared">
          {links.items.map((link) => (
            <li key={linkKey(link)} className="chat-shared__row">
              <MessageUnfurls links={[link.url]} />
              <a
                className="chat-shared__by"
                href={
                  link.thread_root
                    ? nav.hrefOf(nav.place, {
                        kind: "thread",
                        room: channelID,
                        root: link.thread_root,
                      })
                    : nav.hrefOf({ kind: "room", id: channelID, message: link.message_id })
                }
              >
                Shared by {people.person(link.author, link.author_kind).name} ·{" "}
                {relTime(link.at, now)}
              </a>
            </li>
          ))}
        </ul>
        {links.pageError && <QueryState error={links.pageError} refusal={null} loading={false} />}
        {links.more && (
          <Button variant="ghost" size="small" onClick={links.next} loading={links.paging}>
            More shared links
          </Button>
        )}
      </QueryState>
    </div>
  );
}
