/**
 * One finding as a card, under its review and on its line (Files changed): its head, its lines
 * as its revision's diff marks them, what it says, its suggestion as a small diff, and what a
 * reader does with it.
 */

import { useMemo, useState, type ComponentType, type ReactNode } from "react";
import { Button, Tag } from "@crewlethq/ui";
import {
  BookOpenGlyph,
  CheckGlyph,
  CircleCheckGlyph,
  CircleDashedGlyph,
  CircleDotGlyph,
  CircleQuestionMarkGlyph,
  CircleXGlyph,
  FileCodeGlyph,
  FileDiffGlyph,
  GaugeGlyph,
  LayersGlyph,
  ListChecksGlyph,
  ReplyGlyph,
  ShieldGlyph,
  TargetGlyph,
  TypeGlyph,
  type GlyphProps,
} from "@crewlethq/icons/glyphs";
import { DiffView, type DiffHunk, type DiffLine } from "~/components/DiffView.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import { authoredBy, categoryWord, findingStateWord, severityWord } from "~/lib/code.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtBytes } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useAct } from "~/lib/useAct.ts";
import { useViewer } from "~/lib/viewer.ts";
import type {
  CodeChangeDetail,
  CodeCommentRow,
  CodeExcerpt,
  CodeFindingPlace,
  CodeFindingRow,
} from "~/protocol/index.ts";
import { changeHref } from "../paths.ts";
import { useHunkTokens } from "../syntax.tsx";
import { Byline } from "../ui.tsx";
import { ReplyComposer } from "./Conversation.tsx";
import { FindingAnswer, findingStands } from "./FindingAnswer.tsx";
import { refusedWords } from "./reads.ts";
import "~/styles/gitlet.css";

/** Each category's glyph; a word the dashboard does not know draws none. */
const CATEGORY_GLYPHS: Readonly<Record<string, ComponentType<GlyphProps>>> = {
  correctness: TargetGlyph,
  security: ShieldGlyph,
  performance: GaugeGlyph,
  tests: ListChecksGlyph,
  design: LayersGlyph,
  style: TypeGlyph,
  docs: BookOpenGlyph,
};

/** Each state's glyph: open, answered, cleared, disputed or left behind. */
const STATE_GLYPHS: Readonly<Record<string, ComponentType<GlyphProps>>> = {
  open: CircleDotGlyph,
  resolved: CircleCheckGlyph,
  wont_fix: CircleXGlyph,
  dismissed: CircleXGlyph,
  disputed: CircleQuestionMarkGlyph,
  superseded: CircleDashedGlyph,
};

/** Why Resolve is held for a reader who did not write the change: an author's answer. */
export const NOT_AUTHOR_RESOLVE = "A finding is resolved by an author of the change.";

/**
 * Where an excerpt stands: read, unknown to an older fleet, still out, read
 * without this finding's, or refused (the engine's sentence, "" where none).
 */
export type ExcerptRead = CodeExcerpt | "refused" | "reading" | "missing" | { failed: string };

/** "line 19", or "lines 19 to 21". */
function linesWord(f: Pick<CodeFindingRow, "line_start" | "line_end">): string {
  return f.line_start === f.line_end
    ? `line ${f.line_start}`
    : `lines ${f.line_start} to ${f.line_end}`;
}

/** Whether a line is one of the finding's own, on the side it is about. */
function ownLine(f: CodeFindingRow, line: DiffLine): boolean {
  const n = f.side === "old" ? line.before : line.after;
  if (f.side === "old" && line.kind === "add") return false;
  if (f.side !== "old" && line.kind === "remove") return false;
  return n !== null && n >= f.line_start && n <= f.line_end;
}

/** An excerpt as one hunk of the one DiffView. */
function excerptHunks(excerpt: CodeExcerpt): DiffHunk[] {
  const lines: DiffLine[] = excerpt.lines.map((l) => ({
    kind: l.mark === "+" ? "add" : l.mark === "-" ? "remove" : "same",
    text:
      l.text_bytes !== undefined
        ? `A line of ${fmtBytes(l.text_bytes)}, too long to show here`
        : (l.text ?? ""),
    before: l.old ?? null,
    after: l.new ?? null,
  }));
  return [{ skipped: 0, lines }];
}

/** The suggestion as one hunk: the finding's own lines removed, the suggested ones added. */
function suggestionHunks(f: CodeFindingRow, own: readonly DiffLine[]): DiffHunk[] {
  // A TRAILING NEWLINE ends the last line rather than adding an empty one.
  const added = f.suggestion ? f.suggestion.replace(/\n$/, "").split("\n") : [];
  const lines: DiffLine[] = [
    ...own.map((l) => ({
      kind: "remove" as const,
      text: l.text,
      before: (f.side === "old" ? l.before : l.after) ?? null,
      after: null,
    })),
    ...added.map((text, i) => ({
      kind: "add" as const,
      text,
      before: null,
      after: f.line_start + i,
    })),
  ];
  return [{ skipped: 0, lines }];
}

/** The lines around the finding, its own marked, scrolled sideways in their own box. */
function Excerpt({ finding, excerpt }: { finding: CodeFindingRow; excerpt: CodeExcerpt }) {
  const hunks = useMemo(() => excerptHunks(excerpt), [excerpt]);
  const painted = useHunkTokens(finding.path, hunks);
  // AN OVER-LONG LINE'S SENTENCE is words in the tertiary ink, never painted as code.
  const tokens = useMemo(() => {
    const said = { before: new Map<number, number>(), after: new Map<number, number>() };
    hunks[0]!.lines.forEach((l, i) => {
      if (excerpt.lines[i]?.text_bytes === undefined) return;
      if (l.before !== null) said.before.set(l.before, l.text.length);
      if (l.after !== null) said.after.set(l.after, l.text.length);
    });
    if (said.before.size + said.after.size === 0) return painted;
    return (side: "before" | "after", n: number) => {
      const end = said[side].get(n);
      if (end !== undefined) return [{ end, className: "code-excerpt__elided" }];
      return painted ? painted(side, n) : null;
    };
  }, [hunks, excerpt, painted]);
  if (excerpt.binary) return <p className="muted">A binary file: there are no lines to show.</p>;
  const numbers = hunks[0]!.lines
    .map((l) => (finding.side === "old" ? l.before : l.after) ?? l.before ?? l.after)
    .filter((n): n is number => n !== null);
  const from = Math.min(...numbers);
  const to = Math.max(...numbers);
  return (
    <div className="code-excerpt">
      <DiffView
        hunks={hunks}
        hue="code"
        tokens={tokens}
        picked={(line) => ownLine(finding, line)}
        aria-label={`Lines ${from} to ${to} of ${finding.path} at revision ${finding.revision}, ${linesWord(finding)} marked`}
      />
    </div>
  );
}

/** What the finding suggests in place of its lines, as a small diff. */
function Suggestion({ finding, own }: { finding: CodeFindingRow; own: readonly DiffLine[] }) {
  const hunks = useMemo(() => suggestionHunks(finding, own), [finding, own]);
  const tokens = useHunkTokens(finding.path, hunks);
  const removing = finding.suggestion === "";
  const head = removing
    ? `Suggests removing ${linesWord(finding)}`
    : own.length === 0
      ? `In place of ${linesWord(finding)}`
      : linesWord(finding);
  return (
    <section className="code-suggestion" aria-label="Suggested change">
      <div className="code-suggestion__head">
        <FileDiffGlyph size="sm" />
        <span>Suggested change</span>
        <span className="code-suggestion__where">{head}</span>
      </div>
      {hunks[0]!.lines.length > 0 && (
        <div className="code-excerpt">
          <DiffView
            hunks={hunks}
            hue="code"
            tokens={tokens}
            aria-label={`The lines suggested in place of ${linesWord(finding)} of ${finding.path}`}
          />
        </div>
      )}
    </section>
  );
}

export function FindingCard({
  change,
  finding,
  replies,
  applicable,
  place,
  excerpt,
  where,
}: {
  change: CodeChangeDetail;
  finding: CodeFindingRow;
  replies: readonly CodeCommentRow[];
  /** On the head: Apply suggestion may be offered. */
  applicable: boolean;
  /** Files changed: where it sits in the revision shown. */
  place?: CodeFindingPlace | undefined;
  /** Under its review: its excerpt as read; none on a line, whose lines are above it. */
  excerpt?: ExcerptRead | undefined;
  /** Under its review (its row says who and when), or on a line. */
  where: "review" | "line";
}) {
  const now = useNow();
  const viewer = useViewer();
  const apply = useAct("code_apply_suggestions");
  const resolve = useAct("code_resolve_finding");
  const [replying, setReplying] = useState(false);
  const Category = CATEGORY_GLYPHS[finding.category];
  const State = STATE_GLYPHS[finding.state] ?? CircleDotGlyph;
  const read = typeof excerpt === "object" && "lines" in excerpt ? excerpt : null;
  // RESOLVED IS AN AUTHOR'S ANSWER: the engine refuses it to anybody else (`not_author`).
  const resolveHeld = !authoredBy(change, viewer)
    ? NOT_AUTHOR_RESOLVE
    : finding.state === "resolved"
      ? "It is answered this way already."
      : undefined;
  const own = useMemo(
    () => (read ? excerptHunks(read)[0]!.lines.filter((l) => ownLine(finding, l)) : []),
    [read, finding],
  );
  const range = `${finding.line_start}${finding.line_end !== finding.line_start ? `-${finding.line_end}` : ""}`;
  const placeWord = place?.gone
    ? "its file is gone from this revision"
    : place?.outdated
      ? "its lines changed since it was raised"
      : !place
        ? "not placed in this revision"
        : null;
  let lines: ReactNode = null;
  if (where === "review") {
    if (excerpt === "refused")
      lines = <p className="muted">The lines around it are unavailable on this fleet yet.</p>;
    else if (typeof excerpt === "object" && "failed" in excerpt)
      lines = (
        <p className="muted">
          {refusedWords("The lines around it are unavailable", excerpt.failed)}
        </p>
      );
    else if (excerpt === "missing")
      lines = <p className="muted">Its lines are shown once its revision's files are recorded.</p>;
    else if (excerpt === "reading") lines = <div className="code-excerpt code-excerpt--reading" />;
    else if (read) lines = <Excerpt finding={finding} excerpt={read} />;
  }
  const location = changeHref(change.repo, change.number, {
    tab: "files",
    ...(finding.revision !== change.revision ? { rev: finding.revision } : {}),
    file: finding.path,
  });
  return (
    <article className="code-finding" aria-label={`Finding: ${finding.title}`}>
      <div className="code-finding__head">
        {/* COLOUR IS STATE: a blocker holds the change back, the one weight that is a state. */}
        <Tag size="xs" variant={finding.severity === "blocker" ? "danger" : "neutral"}>
          {severityWord(finding.severity)}
        </Tag>
        <Tag
          size="xs"
          appearance="outline"
          leadingIcon={Category ? <Category size="xs" /> : undefined}
        >
          {categoryWord(finding.category)}
        </Tag>
        <h3 className="code-finding__title">{finding.title}</h3>
        <span className="code-finding__state">
          <State size="xs" /> {findingStateWord(finding.state)}
        </span>
      </div>
      {where === "line" && (
        <span className="code-finding__by">
          <Byline
            who={finding.raiser}
            verb={`raised it on revision ${finding.revision}`}
            at={finding.raised_at}
            now={now}
          />
        </span>
      )}
      {where === "review" ? (
        <a className="t-link mono code-finding__where" href={location}>
          <FileCodeGlyph size="xs" /> {finding.path}:{range}
        </a>
      ) : (
        placeWord && (
          <p className="muted mono">
            {finding.path}:{range} · {placeWord}
          </p>
        )
      )}
      {lines}
      {read?.more && (
        <a className="t-link" href={location}>
          Its lines run past what an excerpt shows: read them on Files changed
        </a>
      )}
      {finding.body && <div className="prose md">{renderMarkdown(finding.body)}</div>}
      {finding.suggestion !== undefined && <Suggestion finding={finding} own={own} />}
      {replies.length > 0 && (
        <ol className="code-thread__replies" aria-label="Replies">
          {replies.map((c) => (
            <li key={c.id} className="code-thread__reply">
              <Byline who={c.author} at={c.at} now={now} />
              <div className="prose md">{renderMarkdown(c.body)}</div>
            </li>
          ))}
        </ol>
      )}
      <div className="code-finding__foot">
        {applicable &&
          finding.side === "new" &&
          finding.state === "open" &&
          finding.suggestion !== undefined && (
            <WriteButton
              write={apply}
              size="small"
              variant="secondary"
              leadingIcon={<CheckGlyph size="sm" />}
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
        <FindingAnswer change={change} finding={finding} />
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<ReplyGlyph size="sm" />}
          aria-expanded={replying}
          onClick={() => setReplying((was) => !was)}
        >
          Reply
        </Button>
        {findingStands(finding.state) && (
          <span className="code-finding__end">
            <WriteButton
              write={resolve}
              size="small"
              variant="secondary"
              blocked={resolveHeld}
              onPress={() =>
                void resolve.run(
                  { change: change.ref, findings: [finding.id], resolution: "resolved" },
                  { done: `Marked resolved “${finding.title}”` },
                )
              }
            >
              Resolve
            </WriteButton>
          </span>
        )}
      </div>
      {replying && <ReplyComposer change={change} target={{ finding: finding.id }} focus />}
    </article>
  );
}
