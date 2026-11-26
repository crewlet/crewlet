/** What a change draws on its stack's lines: findings, threads, the reader's drafts and the
 *  composer under a pick, handed to the shared stack as its `lines` (`../FileStack.tsx`). */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  Button,
  Composer,
  Disclosure,
  SegmentedControl,
  Select,
  Stack,
  Tag,
  Textarea,
} from "@crewlethq/ui";
import { PencilGlyph } from "@crewlethq/icons/glyphs";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useDraftState } from "~/app/assistant/draft.ts";
import type { DiffLine } from "~/components/DiffView.tsx";
import { CATEGORY_CHOICES, SEVERITY_CHOICES, categoryWord, severityWord } from "~/lib/code.ts";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useDictationBox } from "~/lib/dictation/source.ts";
import { useViewer } from "~/lib/viewer.ts";
import { plural } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { lineSide } from "~/lib/patch.ts";
import type { DraftFinding, ReviewDrafts } from "~/lib/reviewDrafts.ts";
import type {
  CodeChangeDetail,
  CodeCommentRow,
  CodeFindingPlace,
  CodeFindingRow,
} from "~/protocol/index.ts";
import type { StackLinesProps } from "../FileStack.tsx";
import { Face } from "../ui.tsx";
import { Thread } from "./Conversation.tsx";
import { CATEGORY_GLYPHS, FindingCard, Suggestion, ownLine } from "./Finding.tsx";
import "~/styles/gitlet.css";

/** A line's number on one side of the diff: its newer one, or its older on a line the newer lacks. */
function numberOn(line: DiffLine, side: "old" | "new"): number | null {
  if (side === "new") return line.after;
  return line.kind === "add" ? null : line.before;
}

/** The lines a reviewer has picked: one side's run, from its first line to its last. */
interface LinePick {
  side: "old" | "new";
  start: number;
  end: number;
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

/** What lies on a stack's lines: its findings, threads and drafts, shared by every section. */
export interface StackNotes {
  findings: readonly CodeFindingRow[];
  placed: Readonly<Record<string, CodeFindingPlace>>;
  comments: readonly CodeCommentRow[];
  /** The reader's drafts on the head, or null where none is drafted. */
  drafts: ReviewDrafts | null;
  thread: string;
}

/** The change and revision a Files changed tab draws, with what lies on its lines. */
export interface StackNotesOf {
  change: CodeChangeDetail;
  rev: number;
  since: number | null;
  notes: StackNotes;
}

export const StackNotesContext = createContext<StackNotesOf | null>(null);

/** A change's findings, threads and drafts on one section's lines, from the tab's context. */
export function ChangeLines({ file, hunks, children }: StackLinesProps): ReactNode {
  const of = useContext(StackNotesContext);
  if (!of) throw new Error("ChangeLines is drawn inside a StackNotesContext");
  const { change, rev, since, notes } = of;
  const path = file.path;
  const [pick, setPick] = useState<LinePick | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const { findings, placed, comments, drafts, thread } = notes;
  const { placedHere, outdated } = useMemo(() => {
    const ofFile = findings
      .filter((f) => f.path === path)
      .map((f) => ({ finding: f, place: placed[f.id] }));
    return {
      placedHere: ofFile.filter((p) => p.place && !p.place.outdated && !p.place.gone),
      outdated: ofFile.filter((p) => !p.place || p.place.outdated || p.place.gone),
    };
  }, [findings, placed, path]);
  const { threadsHere, threadsElsewhere } = useMemo(() => {
    const threads = threadsOf(comments).filter((t) => t.root.anchor?.path === path);
    return {
      threadsHere: threads.filter((t) => t.root.anchor?.revision === rev),
      threadsElsewhere: threads.filter((t) => t.root.anchor?.revision !== rev),
    };
  }, [comments, path, rev]);
  const repliesTo = useCallback(
    (finding: string) => comments.filter((c) => c.finding_id === finding),
    [comments],
  );
  // A FINDING'S OWN LINES AS THE PATCH ON SCREEN DRAWS THEM, all of them or none.
  const ownOf = useCallback(
    (finding: Pick<CodeFindingRow, "side" | "line_start" | "line_end">): DiffLine[] => {
      const own = hunks.flatMap((h) => h.lines.filter((l) => ownLine(finding, l)));
      return own.length === finding.line_end - finding.line_start + 1 ? own : [];
    },
    [hunks],
  );
  const threadRoot = threadsHere.find((t) => t.root.id === thread)?.root;
  const revealLine = useCallback(
    (line: DiffLine) => {
      const anchor = threadRoot?.anchor;
      return anchor ? numberOn(line, anchor.side === "old" ? "old" : "new") === anchor.line : false;
    },
    [threadRoot],
  );
  // THE DRAFT LAST EDITED gets the focus back on its Edit once its form closes.
  const [backTo, setBackTo] = useState<string | null>(null);
  const draftCard = useCallback(
    (d: DraftFinding) =>
      editing === d.id ? (
        <FindingComposer
          key={d.id}
          pick={{ side: d.side, start: d.line_start, end: d.line_end }}
          initial={d}
          onAdd={(next) => {
            drafts!.update(d.id, {
              path: d.path,
              side: d.side,
              line_start: d.line_start,
              line_end: d.line_end,
              ...next,
            });
            setEditing(null);
            setBackTo(d.id);
          }}
          onCancel={() => {
            setEditing(null);
            setBackTo(d.id);
          }}
        />
      ) : (
        <DraftNote
          key={d.id}
          draft={d}
          own={ownOf({ ...d, side: d.side })}
          onEdit={() => setEditing(d.id)}
          remove={() => drafts!.remove(d.id)}
          focusEdit={backTo === d.id}
          onFocused={() => setBackTo(null)}
        />
      ),
    [editing, drafts, ownOf, backTo],
  );
  // A DRAFT ON A LINE THE PATCH DOES NOT DRAW (a gap, or lines expanded on an
  // earlier visit) is drawn above the diff, never lost.
  const unplaced = useMemo(() => {
    const drawn = { old: new Set<number>(), new: new Set<number>() };
    for (const h of hunks) {
      for (const l of h.lines) {
        for (const side of ["old", "new"] as const) {
          const n = numberOn(l, side);
          if (n !== null) drawn[side].add(n);
        }
      }
    }
    return (drafts?.drafts ?? []).filter((d) => d.path === path && !drawn[d.side].has(d.line_end));
  }, [drafts, hunks, path]);

  const lineNotes = useCallback(
    (line: DiffLine) => {
      const out: ReactNode[] = [];
      for (const side of ["new", "old"] as const) {
        const n = numberOn(line, side);
        if (n === null) continue;
        for (const p of placedHere) {
          if (p.finding.side === side && (p.place?.end ?? p.place?.start) === n) {
            out.push(
              <FindingCard
                key={p.finding.id}
                change={change}
                finding={p.finding}
                place={p.place}
                replies={repliesTo(p.finding.id)}
                applicable={rev === change.revision && since === null}
                own={ownOf(p.finding)}
                where="line"
              />,
            );
          }
        }
        for (const t of threadsHere) {
          if (t.root.anchor?.side === side && t.root.anchor.line === n) {
            out.push(
              <div key={t.root.id} data-current={t.root.id === thread || undefined}>
                <Thread root={t.root} replies={t.replies} change={change} />
              </div>,
            );
          }
        }
        for (const d of drafts?.drafts ?? []) {
          if (d.path !== path || d.side !== side || d.line_end !== n) continue;
          out.push(draftCard(d));
        }
        if (drafts && pick && pick.side === side && pick.end === n) {
          out.push(
            <LineComposer
              key="composer"
              change={change}
              path={path}
              pick={pick}
              reviewing={drafts.drafts.length > 0}
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
      return out.length > 0 ? <>{out}</> : null;
    },
    [
      placedHere,
      threadsHere,
      drafts,
      pick,
      path,
      change,
      rev,
      since,
      thread,
      repliesTo,
      ownOf,
      draftCard,
    ],
  );
  const picked = useCallback(
    (line: DiffLine) => {
      if (pick) {
        const n = numberOn(line, pick.side);
        if (n !== null && n >= pick.start && n <= pick.end) return true;
      }
      // A FINDING HANGS FROM ITS LAST LINE, which wears the accent bar.
      return placedHere.some((p) => {
        const n = numberOn(line, p.finding.side === "old" ? "old" : "new");
        return n !== null && n === (p.place?.end ?? p.place?.start);
      });
    },
    [pick, placedHere],
  );
  const onPick = useMemo(
    () =>
      drafts
        ? (line: DiffLine, extend: boolean) => {
            const where = lineSide(line);
            if (!where) return;
            setPick((was) =>
              extend && was && was.side === where.side
                ? {
                    side: where.side,
                    start: Math.min(was.start, where.n),
                    end: Math.max(was.end, where.n),
                  }
                : { side: where.side, start: where.n, end: where.n },
            );
          }
        : undefined,
    [drafts],
  );
  const above = (outdated.length > 0 || threadsElsewhere.length > 0) && (
    <div className="code-file__outdated">
      <Disclosure title={`${outdated.length + threadsElsewhere.length} outdated on this file`}>
        <Stack gap={2}>
          {outdated.map((p) => (
            <FindingCard
              key={p.finding.id}
              change={change}
              finding={p.finding}
              place={p.place}
              replies={repliesTo(p.finding.id)}
              applicable={false}
              where="line"
            />
          ))}
          {threadsElsewhere.map((t) => (
            <Thread key={t.root.id} root={t.root} replies={t.replies} change={change} />
          ))}
        </Stack>
      </Disclosure>
    </div>
  );
  const aboveDiff = unplaced.length > 0 && (
    <div className="code-file__unplaced">
      <p className="code-file__note">{plural(unplaced.length, "draft")} on lines not shown</p>
      {unplaced.map(draftCard)}
    </div>
  );
  return children({
    notes: lineNotes,
    picked,
    onPick,
    reveal: threadRoot ? revealLine : undefined,
    above,
    aboveDiff,
  });
}

/** A drafted finding: a dashed card of the reader's own, edited in place or dropped. */
function DraftNote({
  draft,
  own,
  onEdit,
  remove,
  focusEdit,
  onFocused,
}: {
  draft: DraftFinding;
  own: readonly DiffLine[];
  onEdit: () => void;
  remove: () => void;
  /** Its edit form just closed: Edit takes the focus back. */
  focusEdit: boolean;
  onFocused: () => void;
}) {
  const viewer = useViewer();
  const edit = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!focusEdit) return;
    edit.current?.focus();
    onFocused();
  }, [focusEdit, onFocused]);
  const Category = CATEGORY_GLYPHS[draft.category];
  return (
    <article className="code-finding" data-draft="" aria-label={`Drafted finding: ${draft.title}`}>
      <div className="code-finding__head">
        {viewer.owner && <Face who={viewer.owner} size="xs" />}
        <strong>You</strong>
        <Tag size="xs" appearance="outline" leadingIcon={<PencilGlyph size="xs" />}>
          Draft
        </Tag>
        <Tag size="xs" variant={draft.severity === "blocker" ? "danger" : "neutral"}>
          {severityWord(draft.severity)}
        </Tag>
        <Tag
          size="xs"
          appearance="outline"
          leadingIcon={Category ? <Category size="xs" /> : undefined}
        >
          {categoryWord(draft.category)}
        </Tag>
        <span className="code-finding__aside">sent with your review</span>
        <span className="code-finding__end">
          <Button ref={edit} size="small" variant="ghost" onClick={onEdit}>
            Edit
          </Button>
          <Button size="small" variant="ghost" onClick={remove}>
            Drop
          </Button>
        </span>
      </div>
      <h4 className="code-finding__title">{draft.title}</h4>
      {draft.body && <div className="prose md">{renderMarkdown(draft.body)}</div>}
      {draft.suggestion !== undefined && draft.side === "new" && (
        <Suggestion finding={draft} own={own} />
      )}
    </article>
  );
}

/** What a pick leaves on its lines: a comment, posted at once, or a finding, drafted for the review. */
type LineNote = "comment" | "finding";

/** The composer under a pick: a comment or a finding, a finding first while a review is drafted. */
function LineComposer({
  change,
  path,
  pick,
  reviewing,
  onAdd,
  onCancel,
}: {
  change: CodeChangeDetail;
  path: string;
  pick: LinePick;
  /** The reader is drafting a review: its findings are what they leave next. */
  reviewing: boolean;
  onAdd: (draft: Omit<DraftFinding, "id" | "path" | "side" | "line_start" | "line_end">) => void;
  onCancel: () => void;
}) {
  const [note, setNote] = useState<LineNote>(reviewing ? "finding" : "comment");
  const lines = linesOf(pick);
  return (
    <div className="code-line-compose">
      <SegmentedControl
        label={`What to leave on ${lines}`}
        semantics="radio"
        size="sm"
        value={note}
        options={[
          { value: "comment", label: "Comment" },
          { value: "finding", label: "Finding for the review" },
        ]}
        onValueChange={(next) => setNote(next === "finding" ? "finding" : "comment")}
      />
      {note === "comment" ? (
        <LineComment change={change} path={path} pick={pick} onDone={onCancel} />
      ) : (
        <FindingComposer pick={pick} onAdd={onAdd} onCancel={onCancel} />
      )}
    </div>
  );
}

/** "line 6" or "lines 4–6". */
function linesOf(pick: LinePick): string {
  return pick.start === pick.end ? `line ${pick.start}` : `lines ${pick.start}–${pick.end}`;
}

/** A comment on the picked lines, posted at once as a thread of its own anchored at the last of them. */
function LineComment({
  change,
  path,
  pick,
  onDone,
}: {
  change: CodeChangeDetail;
  path: string;
  pick: LinePick;
  onDone: () => void;
}) {
  const write = useAct("code_comment_on_change");
  const [body, setBody] = useDraftState("");
  const { dictation, refusal: dictating } = useDictationBox();
  const lines = linesOf(pick);
  useDraftBox({
    path: "line_comment",
    label: `Comment on ${path}, ${lines}`,
    hint: "Markdown",
    rank: 10,
    value: body,
    set: setBody,
    refuses: () => dictating,
  });
  const blocked = body.trim() ? undefined : "Write the comment first.";
  const send = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      {
        change: change.ref,
        body,
        anchor: { path, line: pick.end, side: pick.side, revision: change.revision },
      },
      { done: `Commented on ${path}, ${lines}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      setBody("");
      onDone();
    }
  };
  return (
    <form
      className="code-finding"
      aria-label={`A comment on ${lines}`}
      onSubmit={(event) => {
        event.preventDefault();
        void send();
      }}
    >
      <p className="muted">A comment on {lines}, posted now as a thread of its own.</p>
      <Composer
        label="Comment"
        placeholder="Leave a comment. Markdown works."
        autoFocus
        value={body}
        rows={3}
        onChange={setBody}
        dictation={dictation}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
            event.preventDefault();
            void send();
          }
        }}
        actions={
          <>
            <Button size="small" variant="ghost" onClick={onDone} disabled={write.busy}>
              Cancel
            </Button>
            <WriteButton
              write={write}
              size="small"
              variant="primary"
              showRefusal={false}
              blocked={blocked}
              onPress={() => void send()}
            >
              Comment
            </WriteButton>
          </>
        }
      />
      <RefusalNote write={write} />
    </form>
  );
}

/** The composer under a pick: a finding drafted on the picked lines. It writes nothing to the
 *  engine: a draft is the tab's until a review sends it. */
function FindingComposer({
  pick,
  initial,
  onAdd,
  onCancel,
}: {
  pick: LinePick;
  /** The draft being edited, its lines fixed. */
  initial?: DraftFinding | undefined;
  onAdd: (draft: Omit<DraftFinding, "id" | "path" | "side" | "line_start" | "line_end">) => void;
  onCancel: () => void;
}) {
  const [severity, setSeverity] = useState(initial?.severity ?? "major");
  const [category, setCategory] = useState(initial?.category ?? "correctness");
  const [title, setTitle] = useDraftState(initial?.title ?? "");
  const [body, setBody] = useDraftState(initial?.body ?? "");
  const [suggesting, setSuggesting] = useState(initial?.suggestion !== undefined);
  const [suggestion, setSuggestion] = useDraftState(initial?.suggestion ?? "");
  const titleDictation = useDictationBox();
  const bodyDictation = useDictationBox();
  const lines = linesOf(pick);
  // THE FINDING'S BOXES AS FIELDS of the change page's one draft, so the
  // person's assistant reads and can fill what is being written here.
  useDraftBox({
    path: "finding.title",
    label: `Finding on ${lines}: title`,
    hint: "One line",
    rank: 10,
    value: title,
    set: setTitle,
    refuses: (next) =>
      titleDictation.refusal ?? (next.includes("\n") ? "A finding's title is one line." : null),
  });
  useDraftBox({
    path: "finding.body",
    label: `Finding on ${lines}: why, and what to do`,
    hint: "Plain text",
    rank: 11,
    value: body,
    set: setBody,
    refuses: () => bodyDictation.refusal,
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
      aria-label={initial ? `Edit the finding on ${lines}` : `A finding on ${lines}`}
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
      <Composer
        singleLine
        label="Title"
        placeholder="What is wrong, in a line"
        value={title}
        onChange={setTitle}
        dictation={titleDictation.dictation}
      />
      <Composer
        label="Body"
        placeholder="Why, and what to do (optional)"
        value={body}
        rows={3}
        onChange={setBody}
        dictation={bodyDictation.dictation}
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
          {initial ? "Save" : "Add to review"}
        </Button>
        <Button size="small" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
