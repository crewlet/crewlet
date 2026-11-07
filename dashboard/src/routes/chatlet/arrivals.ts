/**
 * A message that arrives in the open room or thread, said once in the
 * application's polite region.
 *
 * The transcript is a log with its live region off (the kit's `ChatThread`),
 * because a log that read itself would read every row its window draws as the
 * reader scrolls. So arrivals are said here, from the data rather than the
 * DOM: only what is newer than the newest message already read, never a page
 * of history, never the reader's own words.
 */

import { useEffect, useRef } from "react";
import { announce } from "@crewlethq/ui";

import { plainText } from "~/lib/markdownText.ts";
import { andList } from "~/lib/words.ts";
import type { ChatMessageView } from "~/protocol/index.ts";

/** How much of one message is read out before it is cut. */
export const SAID_CHARS = 140;

/** The words for what arrived, oldest first: one message read out, several counted. */
export function arrivalWords(
  arrived: readonly ChatMessageView[],
  nameOf: (handle: string) => string,
): string {
  if (arrived.length === 0) return "";
  if (arrived.length === 1) {
    const { author, body } = arrived[0]!.message;
    const text = plainText(body ?? "");
    const said = text.length > SAID_CHARS ? `${text.slice(0, SAID_CHARS - 1).trimEnd()}…` : text;
    return said ? `${nameOf(author)}: ${said}` : `New message from ${nameOf(author)}`;
  }
  const from = [...new Set(arrived.map((view) => view.message.author))].map(nameOf);
  return `${arrived.length} new messages from ${andList(from)}`;
}

/** Whether a row is somebody else's words, rather than the reader's, an event or a deletion. */
function said(view: ChatMessageView, viewer: string): boolean {
  const { author, deleted_at, event } = view.message;
  return author !== viewer && !deleted_at && !event;
}

/**
 * Says what arrived in `views` since the last time it was read. `list` names
 * the room or thread; a change of list starts again without saying anything,
 * as does the first answer, which is history rather than an arrival.
 */
export function useArrivals(
  list: string,
  views: readonly ChatMessageView[],
  ready: boolean,
  viewer: string,
  nameOf: (handle: string) => string,
): void {
  const seen = useRef<{ list: string; seq: number } | null>(null);
  const newest = views.reduce((most, view) => Math.max(most, view.channel_seq), 0);
  // The latest of both, read inside the effect without re-running it on
  // every render that hands over a new array or a new name function.
  const latest = useRef({ views, viewer, nameOf });
  latest.current = { views, viewer, nameOf };
  useEffect(() => {
    if (!ready) return;
    const was = seen.current;
    const same = was?.list === list;
    seen.current = { list, seq: Math.max(newest, same ? was.seq : 0) };
    if (!same) return;
    const now = latest.current;
    const arrived = now.views
      .filter((view) => view.channel_seq > was.seq && said(view, now.viewer))
      .sort((a, b) => a.channel_seq - b.channel_seq);
    const words = arrivalWords(arrived, now.nameOf);
    if (words) announce(words);
  }, [list, newest, ready]);
}
