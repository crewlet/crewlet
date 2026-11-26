/** The Comments panel (`panel=comments`): how to start, the lists and counts, a card per thread. */

import { useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  CommentPinGlyph,
  CommentThread,
  Composer,
  EmptyState,
  SegmentedControl,
  Skeleton,
  type CommentThreadComment,
} from "@crewlethq/ui";
import { CheckGlyph, RotateCcwGlyph } from "@crewlethq/icons/glyphs";
import { buildHash, useRoute } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { renderMarkdown } from "~/lib/markdown.ts";
import { ownsRemark, useViewer } from "~/lib/viewer.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import {
  editRemark,
  openComment,
  removeRemark,
  reopenComment,
  replyToComment,
  resolveComment,
} from "~/lib/artifacts/api.ts";
import {
  filterOf,
  listed,
  phrase,
  placeOf,
  stateWords,
  type CommentFilter,
} from "~/lib/artifacts/comments.ts";
import type { ArtifactCommentThread } from "~/protocol/index.ts";
import { DraftComposer } from "./DraftComposer.tsx";
import { useComments, useCommentWrite } from "./state.tsx";
import { useDictation } from "~/lib/dictation/source.ts";

/** What the list says when a filter holds nothing. */
const NONE_UNDER: Readonly<Record<CommentFilter, string>> = {
  open: "Nothing is open on this version.",
  resolved: "Nothing is resolved yet.",
  outdated: "Every thread still has its place on this version.",
};

export function CommentsPanel({
  now,
  onPlace,
}: {
  /** The clock the remarks' times are read against; the kit's shared one where left out. */
  now?: number | undefined;
  /** Comment mode, asked for from the panel. */
  onPlace: () => void;
}) {
  const c = useComments();
  if (!c) return null;
  const counts = c.answer?.counts ?? { open: 0, resolved: 0, outdated: 0 };
  const shown = listed(c.threads, c.filter);
  const none = c.answer !== null && c.threads.length === 0;
  return (
    <div className="artifact-comments">
      {c.draft && (c.narrow || c.draft.docked) ? (
        <DraftComposer />
      ) : c.can ? (
        <Start onPlace={onPlace} />
      ) : (
        <Callout variant="info" layout="card" title="You can read the comments">
          {c.reason}
        </Callout>
      )}
      {c.failed ? (
        <QueryState error={c.failed.error} refusal={c.failed.refusal} loading={false} />
      ) : c.answer === null ? (
        <Skeleton variant="text" rows={4} label="Loading the comments" />
      ) : none ? (
        <EmptyState
          size="compact"
          title="No comments yet"
          description={
            c.can
              ? c.placeable
                ? "Press Comment on a part, then pick a part of the artifact, or comment on the version as a whole. Its owner is told, and answers in a new version."
                : "Comment on the version as a whole. Its owner is told, and answers in a new version."
              : "When somebody comments on this artifact, the thread shows here."
          }
        />
      ) : (
        <>
          <SegmentedControl
            label="Show threads"
            semantics="radio"
            size="sm"
            fill
            value={c.filter}
            onValueChange={(v: CommentFilter) => c.setFilter(v)}
            options={[
              { value: "open", label: "Open", count: counts.open },
              { value: "resolved", label: "Resolved", count: counts.resolved },
              { value: "outdated", label: "Outdated", count: counts.outdated },
            ]}
          />
          {shown.length === 0 ? (
            <p className="artifact-comment-note">{NONE_UNDER[c.filter]}</p>
          ) : (
            <ol className="artifact-comment-threads">
              {shown.map((t) => (
                <ThreadCard key={t.id} t={t} now={now} />
              ))}
            </ol>
          )}
        </>
      )}
    </div>
  );
}

/** How a commenter starts: on a part of the artifact, or on the version as a whole. */
function Start({ onPlace }: { onPlace: () => void }) {
  const c = useComments()!;
  const write = useCommentWrite();
  const [whole, setWhole] = useState(false);
  const [text, setText] = useState("");
  const dictation = useDictation();
  const post = async () => {
    if (!text.trim() || write.busy) return;
    const ok = await write.run(
      openComment(c.detail.id, { body: text, version: c.version, anchor: { kind: "version" } }),
      `Commented on version ${c.version}`,
    );
    if (ok) {
      setText("");
      setWhole(false);
    }
  };
  return (
    <div className="artifact-comment-start">
      {whole ? (
        <Composer
          label={`Comment on version ${c.version}`}
          value={text}
          onChange={setText}
          placeholder={`Comment on version ${c.version} as a whole`}
          onSubmit={() => void post()}
          dictation={dictation}
          actions={
            <>
              <Button size="small" variant="ghost" onClick={() => setWhole(false)}>
                Cancel
              </Button>
              <Button
                size="small"
                disabled={!text.trim()}
                loading={write.busy}
                onClick={() => void post()}
              >
                Comment
              </Button>
            </>
          }
        />
      ) : (
        <div className="artifact-comment-actions">
          {c.placeable && (
            <Button
              size="small"
              variant={c.placing ? "primary" : "outline"}
              leadingIcon={<CommentPinGlyph size="sm" />}
              aria-pressed={c.placing}
              onClick={onPlace}
            >
              {c.placing ? "Placing a comment" : "Comment on a part"}
            </Button>
          )}
          <Button size="small" variant="ghost" onClick={() => setWhole(true)}>
            Comment on v{c.version}
          </Button>
        </div>
      )}
      <p className="artifact-comment-note">
        {c.ownerName} owns this artifact and is told of every comment.
      </p>
    </div>
  );
}

/** The same address with another version on show: the latest is the address with none. */
function useVersionHref(): (n: number) => string {
  const route = useRoute();
  const c = useComments()!;
  return (n: number) => {
    const q = new URLSearchParams(route.query);
    if (n === c.detail.head_number) q.delete("version");
    else q.set("version", String(n));
    return buildHash(route.path, q);
  };
}

function ThreadCard({ t, now }: { t: ArtifactCommentThread; now: number | undefined }) {
  const c = useComments()!;
  const viewer = useViewer();
  const people = useArtifactPeople();
  const write = useCommentWrite();
  const versionHref = useVersionHref();
  const dictation = useDictation();
  const card = useRef<HTMLLIElement>(null);
  const selected = c.selected === t.id;
  useEffect(() => {
    if (selected) card.current?.scrollIntoView?.({ block: "nearest" });
  }, [selected]);
  const where = t.label;
  const state = stateWords(t, c.version);
  const placed = placeOf(t) !== null;
  const rows = useMemo<CommentThreadComment[]>(
    () =>
      t.remarks.map((r) => {
        // THE READER'S OWN REMARK is theirs to rewrite and take down; an
        // administrator may take down anybody's, as the engine allows.
        const own = c.can && !r.removed && ownsRemark(r, viewer);
        return {
          id: r.id,
          author: people.seatPrincipal(r.author),
          body: r.removed ? "" : renderMarkdown(r.body),
          createdAt: r.created_at,
          ...(r.edited_at ? { editedAt: r.edited_at } : {}),
          ...(r.reply_to ? { replyTo: r.reply_to } : {}),
          source: r.body,
          editable: own,
          removable: !r.removed && (own || (c.can && viewer.admin)),
          removed: r.removed === true,
        };
      }),
    [t.remarks, c.can, viewer, people],
  );
  const resolvedBy = t.resolved_by ? people.seatName(t.resolved_by) : "";
  return (
    <li ref={card} className="artifact-comment-thread" data-selected={selected || undefined}>
      <div className="artifact-comment-head">
        <button
          type="button"
          className="artifact-comment-anchor"
          aria-expanded={selected}
          onClick={() => {
            c.select(selected ? null : t.id);
            if (!selected && placed) c.reveal(t);
          }}
        >
          <span className="artifact-comment-number" data-state={filterOf(t)} aria-hidden="true">
            {t.status === "resolved" ? <CheckGlyph size="xs" /> : c.numberOf(t.id)}
          </span>
          <span className="artifact-comment-label">{where}</span>
        </button>
        <span className="artifact-comment-version" title={`Made on version ${t.version}`}>
          v{t.version}
        </span>
      </div>
      {state && (
        <p className="artifact-comment-state" data-state={filterOf(t)}>
          {state}
          {t.status === "resolved" && resolvedBy ? ` by ${resolvedBy}` : ""}
        </p>
      )}
      {t.quote && (
        <blockquote className="artifact-comment-quote clamp" title="What the artifact said there">
          {t.quote}
        </blockquote>
      )}
      {t.crop && (
        <img
          className="artifact-comment-crop"
          src={t.crop}
          alt={`What was commented on: ${where}`}
        />
      )}
      <CommentThread
        heading={false}
        comments={rows}
        subject={where}
        dictation={dictation}
        {...(now === undefined ? {} : { now })}
        {...(c.can && selected
          ? {
              onSubmit: (body: string, replyTo: CommentThreadComment | null) =>
                write.run(
                  replyToComment(c.detail.id, t.id, body, replyTo?.id),
                  `Replied on ${where}`,
                ),
            }
          : {})}
        {...(c.can
          ? {
              onEdit: (r: CommentThreadComment, body: string) =>
                write.run(editRemark(c.detail.id, t.id, r.id, body), "Edited your comment"),
              onRemove: (r: CommentThreadComment) =>
                write.run(removeRemark(c.detail.id, t.id, r.id), "Took the comment down"),
            }
          : {})}
        labels={{
          placeholder: "Reply",
          submit: "Reply",
          field: (_subject, to) => (to ? `Reply to ${to}` : `Reply on ${where}`),
        }}
      />
      <div className="artifact-comment-foot">
        {t.status === "resolved" && t.resolved_version && t.resolved_version !== c.version ? (
          <ButtonLink size="small" variant="ghost" href={versionHref(t.resolved_version)}>
            Show v{t.resolved_version}
          </ButtonLink>
        ) : t.placement === "outdated" && t.version !== c.version ? (
          <ButtonLink size="small" variant="ghost" href={versionHref(t.version)}>
            Show v{t.version}, where it was made
          </ButtonLink>
        ) : null}
        <span className="artifact-comment-foot-end">
          {placed && (
            <Button
              size="small"
              variant="ghost"
              onClick={() => {
                c.select(t.id);
                c.stepAside();
                c.reveal(t);
              }}
            >
              Jump to it
            </Button>
          )}
          {c.can && !selected && (
            <Button size="small" variant="ghost" onClick={() => c.select(t.id)}>
              Reply
            </Button>
          )}
          {c.can &&
            (t.status === "open" ? (
              <Button
                size="small"
                variant="outline"
                leadingIcon={<CheckGlyph size="sm" />}
                loading={write.busy}
                onClick={() =>
                  void write.run(
                    resolveComment(c.detail.id, t.id, c.version),
                    `Resolved in v${c.version}`,
                  )
                }
              >
                Resolve
              </Button>
            ) : (
              <Button
                size="small"
                variant="ghost"
                leadingIcon={<RotateCcwGlyph size="sm" />}
                loading={write.busy}
                onClick={() => void write.run(reopenComment(c.detail.id, t.id), "Reopened")}
              >
                Reopen
              </Button>
            ))}
        </span>
      </div>
      {t.anchor.kind === "html" && t.anchor.name && t.placement === "outdated" && (
        <p className="artifact-comment-note">
          The {phrase(t.anchor.name).toLowerCase()} section is not on this version. The thread stays
          until somebody resolves it.
        </p>
      )}
    </li>
  );
}
