/**
 * A page's thread, and a person's remark on it (`comment_on_page`, as the
 * signed-in person).
 *
 * A COMMENT DOES NOT SUBSCRIBE ITS WRITER here — the knowledge base's own rule,
 * the opposite of the tracker's, so a page a hundred people remarked on does
 * not wake a hundred seats when somebody fixes a heading. The composer says
 * who is told instead: the page's watchers, and anybody @-mentioned.
 */

import { useId, useState } from "react";
import { Button, Textarea } from "@crewlethq/ui";
import { MessageSquareGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import { SeatChip } from "~/components/common.tsx";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import type { PageComment } from "~/protocol/index.ts";

type Who = (handle: string) => { name: string; kind?: "agent" | "human" };

export function PageComments({
  pageID,
  title,
  comments,
  who,
  now,
}: {
  pageID: string;
  title: string;
  comments: PageComment[];
  who: Who;
  now: number;
}) {
  const write = useAct("comment_on_page");
  const [draft, setDraft] = useState("");
  const [replyTo, setReplyTo] = useState<PageComment | null>(null);
  const boxID = useId();
  const byID = new Map(comments.map((c) => [c.id, c]));
  const body = draft.trim();

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
    <section className="kpage-section" aria-labelledby={`${pageID}-comments`}>
      <h2 className="kpage-section-title" id={`${pageID}-comments`}>
        <MessageSquareGlyph size="sm" />
        Comments
        <span className="kpage-count">{comments.length}</span>
      </h2>
      {comments.length === 0 ? (
        <p className="muted">Nobody has commented.</p>
      ) : (
        <ol className="kpage-comments">
          {comments.map((c) => {
            const parent = c.reply_to ? byID.get(c.reply_to) : undefined;
            return (
              <li key={c.id} className="kpage-comment">
                <div className="row gap-2 wrap">
                  <SeatChip handle={c.author} {...who(c.author)} />
                  <time
                    className="muted t-caption"
                    dateTime={c.created_at}
                    title={fmtDateTime(c.created_at)}
                  >
                    {relTime(c.created_at, now)}
                  </time>
                  {c.updated_at && c.updated_at !== c.created_at && (
                    <span className="muted t-caption" title={fmtDateTime(c.updated_at)}>
                      (edited)
                    </span>
                  )}
                  {parent && (
                    <span className="muted t-caption">in reply to {who(parent.author).name}</span>
                  )}
                  <span className="spacer" />
                  {write.access.can && (
                    <Button size="small" variant="ghost" onClick={() => setReplyTo(c)}>
                      Reply
                    </Button>
                  )}
                </div>
                <div className="prose md">{renderMarkdown(c.body)}</div>
              </li>
            );
          })}
        </ol>
      )}

      <div className="kpage-compose">
        {replyTo && (
          <span className="kpage-replying">
            Replying to {who(replyTo.author).name}
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
          {replyTo ? `Reply to ${who(replyTo.author).name}` : `Comment on “${title}”`}
        </label>
        <Textarea
          id={boxID}
          value={draft}
          rows={3}
          placeholder="Ask about something it says, or flag that it has gone stale. Markdown; @handle reaches a seat."
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
              event.preventDefault();
              if (pressable(write, body ? undefined : "empty")) void send();
            }
          }}
        />
        <div className="row gap-2 wrap">
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
          <span className="t-caption">
            The page&rsquo;s watchers are told, and anyone you @-mention.
          </span>
        </div>
        <RefusalNote write={write} />
      </div>
    </section>
  );
}
