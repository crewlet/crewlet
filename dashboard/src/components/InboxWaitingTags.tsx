/**
 * What waits in a seat's inbox, as the kit's smallest tag: the one rendering
 * every surface that shows the count shares (a running turn's row on Live, a
 * seat's card on the Agents page), so the words and the tones cannot drift
 * apart between them. The words are `waitingWords`'s.
 *
 * A person waiting is the `info` tone and said first; the rest of the seat's
 * work is `neutral`. Nothing is drawn for an empty or unreported inbox.
 */

import { Tag } from "@crewlethq/ui";
import { waitingWords } from "~/lib/inboxWaiting.ts";
import type { InboxWaiting } from "~/protocol/index.ts";

export function InboxWaitingTags({ waiting }: { waiting: InboxWaiting | null | undefined }) {
  return (
    <>
      {waitingWords(waiting).map((word) => (
        <Tag key={word.text} size="xs" variant={word.person ? "info" : "neutral"}>
          {word.text}
        </Tag>
      ))}
    </>
  );
}
