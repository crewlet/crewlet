/**
 * A change's diff: the files a revision changes, one file's patch drawn by the
 * dashboard's one hueless `DiffView`, and every finding and thread on the line
 * it is about.
 *
 * # Read by object id, placed by the engine
 *
 * A revision's files, a file's patch and where each finding sits in the
 * revision asked about are the engine's reads of the repository's content
 * (`code_change` parts `files`, `diff` and `findings`): the diff from the base
 * the revision's facts record — or, with `since=`, what changed since an
 * earlier revision — and each finding placed by the engine's one rule
 * (`code.PlaceFindings`), which a seat's own `code_get_change` reads by too.
 * So a seat and a screen are shown one diff and one finding on one line; the
 * page compares and moves nothing itself.
 *
 * # Outdated is said, never hidden
 *
 * A finding whose own lines changed since it was raised is OUTDATED, and one
 * whose file the revision no longer holds is GONE; a thread on a line of
 * another revision has no line here. Each is drawn above the file's diff
 * under a Disclosure that counts them, never dropped: a reviewer who cannot
 * find what they raised re-raises it.
 *
 * # A finding is drafted on its lines, and sent with a review
 *
 * On the head revision, pressing a line's number picks it (Shift extends the
 * pick), and the composer drawn under the pick drafts a finding — its weight
 * and subject in the engine's words (`FINDING_SEVERITIES`,
 * `FINDING_CATEGORIES`), a title, a body and, on the newer side, the lines it
 * suggests in their place. A draft is the tab's (`lib/reviewDrafts.ts`) until
 * a review sends every draft of the revision in ONE `code_review_change`.
 */

import { useMemo, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  Disclosure,
  Input,
  Select,
  Skeleton,
  Stack,
  Tag,
  Textarea,
} from "@crewlethq/ui";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useParam } from "~/app/router.tsx";
import { DiffView, type DiffLine } from "~/components/DiffView.tsx";
import { QueryState } from "~/components/common.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import {
  CATEGORY_CHOICES,
  SEVERITY_CHOICES,
  categoryWord,
  changeLive,
  findingStateWord,
  severityWord,
} from "~/lib/code.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { lineSide, parsePatch } from "~/lib/patch.ts";
import { useReviewDrafts, type DraftFinding, type ReviewDrafts } from "~/lib/reviewDrafts.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useAct } from "~/lib/useAct.ts";
import type {
  CodeChangeDetail,
  CodeCommentRow,
  CodeDiffFile,
  CodeFindingPlace,
  CodeFindingRow,
} from "~/protocol/index.ts";
import { ReplyComposer, Thread } from "./Conversation.tsx";
import { useChangePart } from "./reads.ts";

/** A whole number from 1 off the address, or null. */
function wholeParam(value: string): number | null {
  if (!/^[1-9][0-9]{0,15}$/.test(value)) return null;
  const n = Number(value);
  return Number.isSafeInteger(n) ? n : null;
}

/** The lines a reviewer has picked: one side's run, from its first line to its last. */
interface Pick {
  side: "old" | "new";
  start: number;
  end: number;
}

/** A line's number on one side of the diff: its newer one, or its older on a line the newer lacks. */
function numberOn(line: DiffLine, side: "old" | "new"): number | null {
  if (side === "new") return line.after;
  return line.kind === "add" ? null : line.before;
}

/** One finding as its revision places it: raised here, moved, outdated or gone. */
interface Placed {
  finding: CodeFindingRow;
  place: CodeFindingPlace | undefined;
}

export function Files({ change }: { change: CodeChangeDetail }) {
  const [revParam, setRev] = useParam("rev", "", "filter");
  const [sinceParam, setSince] = useParam("since", "", "filter");
  const [fileParam, setFile] = useParam("file", "", "filter");
  const [layout, setLayout] = useParam("layout", "unified", "filter");
  const [thread] = useParam("thread", "", "filter");
  const comments = useChangePart(change, "comments");
  const threadRoot = (comments.data?.comments ?? []).find((c) => c.id === thread && c.anchor);
  // A THREAD NAMED BY THE ADDRESS opens the file and the revision it is on,
  // where the address names neither.
  const rev = Math.min(
    wholeParam(revParam) ?? threadRoot?.anchor?.revision ?? change.revision,
    change.revision,
  );
  const sinceAsked = wholeParam(sinceParam);
  const since = sinceAsked !== null && sinceAsked < rev ? sinceAsked : null;
  const file = fileParam || threadRoot?.anchor?.path || "";
  const files = useChangePart(change, "files", {
    revision: rev,
    ...(since !== null ? { since_revision: since } : {}),
  });
  const findings = useChangePart(change, "findings", { revision: rev });
  // A FINDING IS DRAFTED ON THE HEAD, read whole: the lines a review of the
  // head judges, never an interdiff's or an earlier revision's.
  const drafting = rev === change.revision && since === null && changeLive(change.state);
  const drafts = useReviewDrafts(change.id, rev);
  const listed = files.data?.diff?.files ?? [];
  const revisions = Array.from({ length: change.revision }, (_, i) => change.revision - i);
  return (
    <Stack gap={3}>
      <div className="toolbar">
        <Select
          width="auto"
          value={String(rev)}
          onChange={(value) =>
            setRev(String(value) === String(change.revision) ? "" : String(value))
          }
          options={revisions.map((n) => ({
            value: String(n),
            label: n === change.revision ? `Revision ${n} (head)` : `Revision ${n}`,
          }))}
          ariaLabel="Revision"
        />
        <Select
          width="auto"
          value={since === null ? "" : String(since)}
          onChange={(value) => setSince(String(value))}
          options={[
            { value: "", label: "Since its base" },
            ...revisions
              .filter((n) => n < rev)
              .map((n) => ({ value: String(n), label: `Since revision ${n}` })),
          ]}
          ariaLabel="Compared with"
          active={since !== null}
        />
        <Select
          width="auto"
          value={layout}
          onChange={(value) => setLayout(String(value))}
          options={[
            { value: "unified", label: "Unified" },
            { value: "split", label: "Side by side" },
          ]}
          ariaLabel="Layout"
        />
        {drafts.drafts.length > 0 && (
          <Tag appearance="outline">
            {drafts.drafts.length} drafted finding{drafts.drafts.length === 1 ? "" : "s"}
          </Tag>
        )}
      </div>
      {files.loading && !files.data && (
        <Skeleton variant="text" rows={4} label="Loading the files" />
      )}
      <QueryState
        error={files.error}
        refusal={files.refusal}
        detail={files.detail}
        loading={files.loading && !files.data}
        empty={
          files.data && listed.length === 0
            ? {
                title: "No file changed",
                hint: "This revision changes nothing against what it is compared with.",
              }
            : undefined
        }
      >
        <div className="code-files">
          <ul className="code-files__list" aria-label="Files changed">
            {listed.map((f) => (
              <li key={f.path}>
                <button
                  type="button"
                  className="code-files__file"
                  aria-current={f.path === file || undefined}
                  onClick={() => setFile(f.path)}
                >
                  <span className="mono">{f.path}</span>{" "}
                  <span className="muted">
                    {f.status} +{f.added} −{f.deleted}
                  </span>
                </button>
              </li>
            ))}
          </ul>
          {files.data?.next && <p className="muted">More files changed than one page lists.</p>}
          {file ? (
            <FileDiff
              change={change}
              rev={rev}
              since={since}
              path={file}
              entry={listed.find((f) => f.path === file)}
              layout={layout === "split" ? "split" : "unified"}
              findings={findings.data?.findings ?? []}
              placed={findings.data?.placed ?? {}}
              comments={comments.data?.comments ?? []}
              drafts={drafting ? drafts : null}
              thread={thread}
            />
          ) : (
            <p className="muted">Choose a file to read its changes.</p>
          )}
        </div>
      </QueryState>
    </Stack>
  );
}

/** One file's patch, with what is anchored to its lines drawn under them. */
function FileDiff({
  change,
  rev,
  since,
  path,
  entry,
  layout,
  findings,
  placed,
  comments,
  drafts,
  thread,
}: {
  change: CodeChangeDetail;
  rev: number;
  since: number | null;
  path: string;
  entry: CodeDiffFile | undefined;
  layout: "unified" | "split";
  findings: readonly CodeFindingRow[];
  placed: Readonly<Record<string, CodeFindingPlace>>;
  comments: readonly CodeCommentRow[];
  drafts: ReviewDrafts | null;
  thread: string;
}) {
  const patch = useChangePart(
    change,
    "diff",
    { revision: rev, file: path, ...(since !== null ? { since_revision: since } : {}) },
    !entry?.binary,
  );
  const text = patch.data?.diff?.patch ?? "";
  const hunks = useMemo(() => parsePatch(text), [text]);
  const [pick, setPick] = useState<Pick | null>(null);
  const ofFile: Placed[] = findings
    .filter((f) => f.path === path)
    .map((f) => ({ finding: f, place: placed[f.id] }));
  const placedHere = ofFile.filter((p) => p.place && !p.place.outdated && !p.place.gone);
  const outdated = ofFile.filter((p) => !p.place || p.place.outdated || p.place.gone);
  const threads = threadsOf(comments).filter((t) => t.root.anchor?.path === path);
  const threadsHere = threads.filter((t) => t.root.anchor?.revision === rev);
  const threadsElsewhere = threads.filter((t) => t.root.anchor?.revision !== rev);
  const repliesTo = (finding: string) => comments.filter((c) => c.finding_id === finding);

  const notes = (line: DiffLine) => {
    const drawn: ReactNode[] = [];
    for (const side of ["new", "old"] as const) {
      const n = numberOn(line, side);
      if (n === null) continue;
      for (const p of placedHere) {
        if (p.finding.side === side && (p.place?.end ?? p.place?.start) === n) {
          drawn.push(
            <FindingNote
              key={p.finding.id}
              change={change}
              finding={p.finding}
              place={p.place}
              replies={repliesTo(p.finding.id)}
              applicable={rev === change.revision && since === null}
            />,
          );
        }
      }
      for (const t of threadsHere) {
        if (t.root.anchor?.side === side && t.root.anchor.line === n) {
          drawn.push(
            <div key={t.root.id} data-current={t.root.id === thread || undefined}>
              <Thread root={t.root} replies={t.replies} change={change} />
            </div>,
          );
        }
      }
      for (const d of drafts?.drafts ?? []) {
        if (d.path === path && d.side === side && d.line_end === n) {
          drawn.push(<DraftNote key={d.id} draft={d} remove={() => drafts!.remove(d.id)} />);
        }
      }
      if (drafts && pick && pick.side === side && pick.end === n) {
        drawn.push(
          <FindingComposer
            key="composer"
            pick={pick}
            onAdd={(draft) => {
              drafts.add({
                ...draft,
                path,
                side: pick.side,
                line_start: pick.start,
                line_end: pick.end,
              });
              setPick(null);
            }}
            onCancel={() => setPick(null)}
          />,
        );
      }
    }
    return drawn.length > 0 ? <>{drawn}</> : null;
  };

  return (
    <section aria-label={`Changes to ${path}`} className="code-file">
      {(outdated.length > 0 || threadsElsewhere.length > 0) && (
        <Disclosure title={`${outdated.length + threadsElsewhere.length} outdated on this file`}>
          <Stack gap={2}>
            {outdated.map((p) => (
              <FindingNote
                key={p.finding.id}
                change={change}
                finding={p.finding}
                place={p.place}
                replies={repliesTo(p.finding.id)}
                applicable={false}
              />
            ))}
            {threadsElsewhere.map((t) => (
              <Thread key={t.root.id} root={t.root} replies={t.replies} change={change} />
            ))}
          </Stack>
        </Disclosure>
      )}
      {entry?.binary ? (
        <p className="muted">A binary file: git draws no lines of it.</p>
      ) : patch.loading && !patch.data ? (
        <Skeleton variant="text" rows={8} label="Loading the patch" />
      ) : patch.error ? (
        <QueryState
          error={patch.error}
          refusal={patch.refusal}
          detail={patch.detail}
          loading={false}
        >
          {null}
        </QueryState>
      ) : (
        <>
          <DiffView
            hunks={hunks}
            layout={layout}
            aria-label={`Changes to ${path}`}
            notes={notes}
            onPick={
              drafts
                ? (line, extend) => {
                    const at = lineSide(line);
                    if (!at) return;
                    setPick((was) =>
                      extend && was && was.side === at.side
                        ? {
                            side: at.side,
                            start: Math.min(was.start, at.n),
                            end: Math.max(was.end, at.n),
                          }
                        : { side: at.side, start: at.n, end: at.n },
                    );
                  }
                : undefined
            }
            picked={(line) => {
              if (!pick) return false;
              const n = numberOn(line, pick.side);
              return n !== null && n >= pick.start && n <= pick.end;
            }}
          />
          {patch.data?.next && (
            <Callout variant="neutral">
              This file's patch runs past one page: its end is not drawn.
            </Callout>
          )}
        </>
      )}
    </section>
  );
}

/** A change's threads on lines, each by its first comment with its replies. */
function threadsOf(comments: readonly CodeCommentRow[]) {
  const threads = new Map<string, { root: CodeCommentRow; replies: CodeCommentRow[] }>();
  for (const c of comments) {
    if (c.finding_id) continue;
    if (!c.parent_id) threads.set(c.id, { root: c, replies: [] });
    else threads.get(c.parent_id)?.replies.push(c);
  }
  return [...threads.values()].filter((t) => t.root.anchor);
}

/**
 * One finding, where its line is: its weight, its subject and its state, what
 * it says, the lines it suggests and — on the head, while it is open — the
 * press that applies them, and its replies with a reply of the reader's own.
 */
function FindingNote({
  change,
  finding,
  place,
  replies,
  applicable,
}: {
  change: CodeChangeDetail;
  finding: CodeFindingRow;
  place: CodeFindingPlace | undefined;
  replies: readonly CodeCommentRow[];
  applicable: boolean;
}) {
  const seatBadge = useSeatBadgeOf();
  const apply = useAct("code_apply_suggestions");
  const where = place?.gone
    ? "its file is gone from this revision"
    : place?.outdated
      ? "its lines changed since it was raised"
      : !place
        ? "not placed in this revision"
        : null;
  return (
    <article className="code-finding" aria-label={`Finding: ${finding.title}`}>
      <p className="row gap-1 wrap">
        {/* COLOUR IS STATE: a blocker holds the change back, which is the one
            weight that is a state of the change rather than a word about it. */}
        <Tag size="sm" variant={finding.severity === "blocker" ? "danger" : "neutral"}>
          {severityWord(finding.severity)}
        </Tag>
        <Tag size="sm" appearance="outline">
          {categoryWord(finding.category)}
        </Tag>
        <strong>{finding.title}</strong>
        <span className="muted">
          {seatBadge(finding.raiser).name}, revision {finding.revision},{" "}
          {findingStateWord(finding.state)}
        </span>
      </p>
      {where && (
        <p className="muted mono">
          {finding.path}:{finding.line_start}
          {finding.line_end !== finding.line_start ? `–${finding.line_end}` : ""} — {where}
        </p>
      )}
      {finding.body && <div className="prose md">{renderMarkdown(finding.body)}</div>}
      {finding.suggestion !== undefined && (
        <>
          <pre className="code-suggestion mono">{finding.suggestion}</pre>
          {applicable && finding.side === "new" && finding.state === "open" && (
            <WriteButton
              write={apply}
              size="small"
              variant="secondary"
              onPress={() =>
                void apply.run(
                  { change: change.ref, findings: [finding.id], expected_head: change.head },
                  { done: `Applied the suggestion of “${finding.title}”` },
                )
              }
            >
              Apply suggestion
            </WriteButton>
          )}
        </>
      )}
      {replies.map((c) => (
        <div key={c.id}>
          <p>
            <strong>{seatBadge(c.author).name}</strong>
          </p>
          <div className="prose md">{renderMarkdown(c.body)}</div>
        </div>
      ))}
      <ReplyComposer change={change} target={{ finding: finding.id }} />
    </article>
  );
}

/** A drafted finding, marked as one, with the press that drops it. */
function DraftNote({ draft, remove }: { draft: DraftFinding; remove: () => void }) {
  return (
    <article className="code-finding" aria-label={`Drafted finding: ${draft.title}`}>
      <p className="row gap-1 wrap">
        <Tag size="sm" appearance="outline">
          Draft
        </Tag>
        <Tag size="sm" variant={draft.severity === "blocker" ? "danger" : "neutral"}>
          {severityWord(draft.severity)}
        </Tag>
        <strong>{draft.title}</strong>
        <Button size="small" variant="ghost" onClick={remove}>
          Drop
        </Button>
      </p>
      {draft.body && <div className="prose md">{renderMarkdown(draft.body)}</div>}
    </article>
  );
}

/**
 * The composer under a pick: a finding drafted on the picked lines. It writes
 * nothing to the engine — a draft is the tab's until a review sends it.
 */
function FindingComposer({
  pick,
  onAdd,
  onCancel,
}: {
  pick: Pick;
  onAdd: (draft: Omit<DraftFinding, "id" | "path" | "side" | "line_start" | "line_end">) => void;
  onCancel: () => void;
}) {
  const [severity, setSeverity] = useState("major");
  const [category, setCategory] = useState("correctness");
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [suggesting, setSuggesting] = useState(false);
  const [suggestion, setSuggestion] = useState("");
  const lines = pick.start === pick.end ? `line ${pick.start}` : `lines ${pick.start}–${pick.end}`;
  // THE FINDING'S BOXES AS FIELDS of the change page's one draft
  // (`ChangePage.tsx`), so the person's assistant reads and can fill what is
  // being written on these lines; adding it to the review stays the person's.
  useDraftBox({
    path: "finding.title",
    label: `Finding on ${lines}: title`,
    hint: "One line",
    rank: 10,
    value: title,
    set: setTitle,
    refuses: (next) => (next.includes("\n") ? "A finding's title is one line." : null),
  });
  useDraftBox({
    path: "finding.body",
    label: `Finding on ${lines}: why, and what to do`,
    hint: "Plain text",
    rank: 11,
    value: body,
    set: setBody,
    refuses: () => null,
  });
  useDraftBox(
    suggesting
      ? {
          path: "finding.suggestion",
          label: `Finding on ${lines}: the lines to put in their place`,
          hint: "The replacement lines, exactly",
          rank: 12,
          value: suggestion,
          set: setSuggestion,
          refuses: () => null,
        }
      : null,
  );
  return (
    <form
      className="code-finding"
      aria-label={`A finding on ${lines}`}
      onSubmit={(event) => {
        event.preventDefault();
        if (!title.trim()) return;
        onAdd({ severity, category, title, body, ...(suggesting ? { suggestion } : {}) });
      }}
    >
      <p className="muted">A finding on {lines}, sent with your review.</p>
      <div className="row gap-2 wrap">
        <Select
          width="auto"
          value={severity}
          onChange={(value) => setSeverity(String(value))}
          options={SEVERITY_CHOICES.map((c) => ({ value: c.value, label: c.label }))}
          ariaLabel="Weight"
        />
        <Select
          width="auto"
          value={category}
          onChange={(value) => setCategory(String(value))}
          options={CATEGORY_CHOICES.map((c) => ({ value: c.value, label: c.label }))}
          ariaLabel="About"
        />
      </div>
      <Input
        aria-label="Title"
        placeholder="What is wrong, in a line"
        value={title}
        onChange={(event) => setTitle(event.target.value)}
      />
      <Textarea
        aria-label="Body"
        placeholder="Why, and what to do (optional)"
        value={body}
        rows={3}
        onChange={(event) => setBody(event.target.value)}
      />
      {pick.side === "new" && (
        <label className="row gap-1">
          <input
            type="checkbox"
            checked={suggesting}
            onChange={(event) => setSuggesting(event.target.checked)}
          />
          Suggest the lines to put in their place
        </label>
      )}
      {suggesting && (
        <Textarea
          aria-label="Suggestion"
          className="mono"
          value={suggestion}
          rows={4}
          onChange={(event) => setSuggestion(event.target.value)}
        />
      )}
      <div className="row gap-2">
        <Button type="submit" size="small" variant="primary" disabled={!title.trim()}>
          Add to review
        </Button>
        <Button size="small" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
