/**
 * A message body, drawn.
 *
 * MARKDOWN, READ BY THE DASHBOARD'S ONE RENDERER (`lib/markdown.ts`), over the
 * design system's grammar: bold, italic, strikethrough, code, lists, quotes
 * and links read the same in a room as in a page or an answer, and what the
 * composer's formatting row writes is what this reads. Nothing is stored
 * differently; this is the reading of the string the engine keeps. A second
 * reader of the marks for chat alone would be a second grammar, and the two
 * would come to disagree about where a list ends.
 *
 * AS A MODEL'S WORDS ARE READ, because a message is typed line by line: every
 * newline is the break its writer meant, and a heading is emphasis inside one
 * message rather than a section of the page.
 *
 * A LINK IS LIVE ONLY WHERE THE DASHBOARD BUILT IT (`CHAT_WORDS`, `links:
 * "own"`), and a picture is its alt text, never fetched: a message is words
 * somebody else wrote, an agent among them that may have read anything, so an
 * address in one opens only as it is rebuilt on a host the company configured
 * or as a page of the dashboard's own rebuilt from its path, and any other is
 * drawn inert with the host it names.
 *
 * WHO A MESSAGE NAMED IS THE ENGINE'S READING OF IT. The message carries the
 * handles it resolved (`mentions`) and whether it named the whole room
 * (`collective`), and only those are drawn as the kit's `MentionChip`: the
 * reader's own name and the whole room in the warning tone, anybody else in
 * the info tone. A
 * handle the engine did not resolve stays the inline token every other
 * surface draws, so an `@word` that named nobody never looks like a mention.
 */

import type { ReactNode } from "react";
import { MentionChip, Prose } from "@crewlethq/ui";

import { CHAT_WORDS, renderMarkdown } from "~/lib/markdown.ts";
import { inlineTokens } from "~/lib/inlineTokens.ts";
import { handleLabel } from "~/lib/seats.ts";
import type { ChatMessage } from "~/protocol/index.ts";

/** The handle the engine reads as the whole room. */
export const ROOM_HANDLE = "channel";

export interface MessageBodyProps {
  message: Pick<ChatMessage, "body" | "mentions" | "collective">;
  /** The seat reading, whose own name is drawn as theirs. */
  viewer: string;
  nameOf: (handle: string) => string;
  /** A named person or agent, pressed: their profile. */
  onPerson?: (handle: string) => void;
  /** Drawn at the end of the last line: "(edited)". */
  after?: ReactNode;
}

export function MessageBody({ message, viewer, nameOf, onPerson, after }: MessageBodyProps) {
  const named = new Set(message.mentions ?? []);
  const collective = !!message.collective;
  const prose = (text: string, key: string): ReactNode => {
    const out: ReactNode[] = [];
    let at = 0;
    for (const token of inlineTokens(text)) {
      if (token.kind !== "mention") continue;
      const everyone = token.handle === ROOM_HANDLE && collective;
      if (!everyone && !named.has(token.handle)) continue;
      if (token.at > at) out.push(text.slice(at, token.at));
      out.push(
        <Mention
          key={`${key}-m${token.at}`}
          handle={token.handle}
          everyone={everyone}
          mine={token.handle === viewer}
          nameOf={nameOf}
          onPerson={onPerson}
        />,
      );
      at = token.end;
    }
    if (at === 0) return text;
    if (at < text.length) out.push(text.slice(at));
    return out;
  };
  return (
    <Prose as="div" measure="none" className="prose md chat-body">
      {renderMarkdown(message.body ?? "", { ...CHAT_WORDS, prose })}
      {after ? <span className="chat-body-after">{after}</span> : null}
    </Prose>
  );
}

/** One mention the engine resolved, as the kit's chip draws it. */
function Mention({
  handle,
  everyone,
  mine,
  nameOf,
  onPerson,
}: {
  handle: string;
  everyone: boolean;
  mine: boolean;
  nameOf: (handle: string) => string;
  onPerson: ((handle: string) => void) | undefined;
}) {
  // THE WHOLE ROOM IS NOBODY TO OPEN, and reached the reader as surely as
  // their own name, so it takes the same tone.
  if (everyone) return <MentionChip tone="everyone">{handleLabel(ROOM_HANDLE)}</MentionChip>;
  return (
    <MentionChip
      tone={mine ? "self" : "other"}
      {...(onPerson ? { onPress: () => onPerson(handle) } : {})}
    >
      {handleLabel(nameOf(handle))}
    </MentionChip>
  );
}

/** A body quoted on one line, its marks read away: an excerpt, a delete's preview. */
export function bodyExcerpt(body: string | undefined): string {
  return (body ?? "").replace(/\s+/g, " ").trim();
}
