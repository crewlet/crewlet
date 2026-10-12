/**
 * A page's thread, and a person's remark on it (`comment_on_page`, as the
 * signed-in person).
 *
 * A COMMENT DOES NOT SUBSCRIBE ITS WRITER here, which is the knowledge base's
 * own rule and the opposite of the tracker's: a page a hundred people remarked
 * on does not wake a hundred seats when somebody fixes a heading. The composer
 * says who is told instead: the page's watchers, and anybody mentioned.
 *
 * A comment a person wrote through their own token is RECORDED under the
 * token and DRAWN as the person (`comment_seats`), as every other row of a
 * page is ([drawnBy]).
 */

import { useId, useState } from "react";
import { Button, Document, Textarea } from "@crewlethq/ui";
import { MessageSquareGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import { SeatChip } from "~/components/common.tsx";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import type { PageComment } from "~/protocol/index.ts";
import { drawnBy, type Who } from "../parts.tsx";

export function PageComments({
  pageID,
  title,
  comments,
  seats,
  who,
  now,
}: {
  pageID: string;
  title: string;
  comments: PageComment[];
  /** The person behind each comment an operator's token wrote, by comment id. */
  seats?: Record<string, string> | undefined;
  who: Who;
  now: number;
}) {
  const write = useAct("comment_on_page");
  const [draft, setDraft] = useState("");
  const [replyTo, setReplyTo] = useState<PageComment | null>(null);
  const boxID = useId();
  const headingID = useId();
  const byID = new Map(comments.map((c) => [c.id, c]));
  const body = draft.trim();
  const person = (c: PageComment) => drawnBy(c.author, seats?.[c.id]);

  const send = async () => {
    const result = await write.run(
      { page: pageID, body, ...(replyTo ? { reply_to: replyTo.id } : {}) },
      { done: `Commented on “${title}”` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      setDraft("");
      setReplyTo(null);
    }
  };

  return (
    <section className="knowledge-comments" aria-labelledby={headingID}>
      <h2 className="knowledge-section-title" id={headingID}>
        <MessageSquareGlyph size="sm" />
        Comments
        <span className="knowledge-count">{comments.length}</span>
      </h2>
      {comments.length === 0 ? (
        <p className="knowledge-note">Nobody has commented.</p>
      ) : (
        <ol className="knowledge-comment-list">
          {comments.map((c) => {
            const parent = c.reply_to ? byID.get(c.reply_to) : undefined;
            const by = person(c);
            return (
              <li key={c.id} className="knowledge-comment">
                <div className="knowledge-comment-head">
                  {by ? <SeatChip handle={by} {...who(by)} /> : <b>The engine</b>}
                  <time
                    className="knowledge-note"
                    dateTime={c.created_at}
                    title={fmtDateTime(c.created_at)}
                  >
                    {relTime(c.created_at, now)}
                  </time>
                  {c.updated_at && c.updated_at !== c.created_at && (
                    <span className="knowledge-note" title={fmtDateTime(c.updated_at)}>
                      (edited)
                    </span>
                  )}
                  {parent && (
                    <span className="knowledge-note">in reply to {who(person(parent)).name}</span>
                  )}
                  <span className="spacer" />
                  {write.access.can && (
                    <Button size="small" variant="ghost" onClick={() => setReplyTo(c)}>
                      Reply
                    </Button>
                  )}
                </div>
                <Document size="sm" measure="none">
                  {renderMarkdown(c.body)}
                </Document>
              </li>
            );
          })}
        </ol>
      )}

      <div className="knowledge-compose">
        {replyTo && (
          <span className="knowledge-replying">
            Replying to {who(person(replyTo)).name}
            <Button
              size="small"
              variant="ghost"
              leadingIcon={<XGlyph size="sm" />}
              aria-label="Stop replying"
              onClick={() => setReplyTo(null)}
            />
          </span>
        )}
        <label className="sr-only" htmlFor={boxID}>
          {replyTo ? `Reply to ${who(person(replyTo)).name}` : `Comment on “${title}”`}
        </label>
        <Textarea
          id={boxID}
          value={draft}
          rows={3}
          placeholder="Comment, or @mention a seat"
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
              event.preventDefault();
              if (pressable(write, body ? undefined : "empty")) void send();
            }
          }}
        />
        <div className="knowledge-compose-foot">
          <WriteButton
            write={write}
            variant="secondary"
            size="small"
            showRefusal={false}
            blocked={body ? undefined : "Write the comment first."}
            onPress={() => void send()}
          >
            {replyTo ? "Reply" : "Comment"}
          </WriteButton>
          <span className="knowledge-note">
            The page&rsquo;s watchers are told, and anyone you mention.
          </span>
        </div>
        <RefusalNote write={write} />
      </div>
    </section>
  );
}
