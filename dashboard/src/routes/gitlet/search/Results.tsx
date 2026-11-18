/** Gitlet › Search's results: each repository's files as cards of blocks, a hit with the lines around it. */

import { memo, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Button, IconButton, IconButtonLink } from "@crewlethq/ui";
import {
  ChevronDownGlyph,
  ExternalLinkGlyph,
  FileCodeGlyph,
  SquareCodeGlyph,
  UnfoldVerticalGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { repoPath, shortOID } from "~/lib/code.ts";
import {
  baseOf,
  coverageNotes,
  dirOf,
  fileBlocks,
  hitAt,
  markedSegments,
  matchingLines,
  matchRanges,
  SEARCH_CONTEXT,
  type CodeSearchHit,
  type FileHits,
  type RepoHits,
  type SearchBlock,
} from "~/lib/codeSearch.ts";
import { EXPAND_STEP } from "~/lib/expand.ts";
import { plural } from "~/lib/format.ts";
import { statesOf } from "~/lib/syntax/index.ts";
import type { Grammar, Token } from "~/lib/syntax/types.ts";
import { codeRest } from "~/lib/useCodeQuery.ts";
import { QueryError } from "~/protocol/index.ts";
import { CopyButton } from "../change/CopyButton.tsx";
import { commitHref, repoHref } from "../paths.ts";
import { Painted, SYNTAX_CLASS, tokensOf, useExpandLines, useGrammar } from "../syntax.tsx";

/** The search the page asked, which Show N more asks again of one file, and the page's own ask. */
export interface AskedSearch {
  q: string;
  mode: string;
  ref: string;
  again: () => void;
}

/** "9 matching lines", or "at least 9" where a file's total was not answered. */
function linesWords({ count, exact }: { count: number; exact: boolean }): string {
  return `${exact ? "" : "at least "}${plural(count, "matching line")}`;
}

/** A refusal's own sentence, or none. */
function detailOf(err: unknown): string {
  return err instanceof QueryError ? (err.detail ?? "") : "";
}

export function Results({ groups, asked }: { groups: readonly RepoHits[]; asked: AskedSearch }) {
  return (
    <div className="code-search-results">
      {groups.map((g) => (
        <RepoResult key={g.repo} group={g} asked={asked} />
      ))}
    </div>
  );
}

/** One repository's files, named with the commit they were read at. */
const RepoResult = memo(function RepoResult({
  group,
  asked,
}: {
  group: RepoHits;
  asked: AskedSearch;
}) {
  return (
    <section className="code-search-repo" aria-label={group.repo}>
      <h3 className="code-search-repo__head">
        <SquareCodeGlyph size="sm" className="code-glyph" />
        <a className="mono" href={repoHref(group.repo, group.tip ? { ref: group.tip } : {})}>
          {group.repo}
        </a>
        <span className="t-caption muted">
          {linesWords(matchingLines(group.files))} in {plural(group.files.length, "file")}
        </span>
        {group.tip && (
          <span className="code-search-repo__tip">
            <span className="t-caption muted">at</span>
            <a className="code-search-repo__sha mono" href={commitHref(group.repo, group.tip)}>
              {shortOID(group.tip)}
            </a>
          </span>
        )}
      </h3>
      {group.files.map((f) => (
        <FileResult
          key={`${group.tip}:${f.blob}:${f.pathB64 ?? f.path}`}
          repo={group.repo}
          tip={group.tip}
          file={f}
          asked={asked}
        />
      ))}
    </section>
  );
});

/** A hit's address with no line picked: the file itself. */
function fileOnly(at: Record<string, string>): Record<string, string> {
  return Object.fromEntries(Object.entries(at).filter(([key]) => key !== "lines"));
}

/** Where Show N more stands: not asked, asking, refused, the branch moved, or merged. */
type More =
  | { state: "idle" | "loading" }
  | { state: "failed"; detail: string }
  | { state: "moved"; now: string }
  | { state: "merged"; hits: CodeSearchHit[] };

function FileResult({
  repo,
  tip,
  file,
  asked,
}: {
  repo: string;
  tip?: string;
  file: FileHits;
  asked: AskedSearch;
}) {
  const first = hitAt(tip, file, file.lines[0]?.line ?? 1);
  const grammar = useGrammar(file.path);
  const expand = useExpandLines(repo, file.blob, null);
  const [revealed, setRevealed] = useState<ReadonlyMap<number, string>>(new Map());
  const [refused, setRefused] = useState<ReadonlyMap<number, string>>(new Map());
  const [more, setMore] = useState<More>({ state: "idle" });
  // The line a press revealed, focused once it is drawn.
  const [focus, setFocus] = useState<{ line: number; link: boolean } | null>(null);
  const card = useRef<HTMLElement>(null);
  const extra = more.state === "merged" ? more.hits : null;
  const hits = useMemo(() => (extra ? [...file.lines, ...extra] : file.lines), [file.lines, extra]);
  const blocks = useMemo(() => fileBlocks(hits, revealed), [hits, revealed]);
  const drawn = useMemo(() => new Set(hits.map((h) => h.line)).size, [hits]);
  const fileHref = first ? href(repoPath(repo), fileOnly(first)) : null;
  const at = tip ? ` at ${shortOID(tip)}` : "";

  useEffect(() => {
    if (!focus) return;
    const row = card.current?.querySelector<HTMLElement>(`[data-line="${focus.line}"]`);
    (focus.link ? (row?.querySelector<HTMLElement>("a") ?? row) : row)?.focus();
    setFocus(null);
  }, [focus]);

  const reveal = async (after: number, before: number) => {
    const to = Math.min(after + EXPAND_STEP, before - 1);
    const read = await expand.read(after + 1, to);
    if (read.failed !== null) {
      setRefused((was) => new Map(was).set(after, read.failed));
      return;
    }
    setRefused((was) => {
      const next = new Map(was);
      next.delete(after);
      return next;
    });
    setRevealed((was) => {
      const next = new Map(was);
      for (let n = after + 1; n <= to; n++) next.set(n, expand.line(n) ?? "");
      return next;
    });
    // Where the gap closed, focus the next block's first link instead.
    setFocus(to === before - 1 ? { line: before, link: true } : { line: after + 1, link: false });
  };

  const showMore = async () => {
    setMore({ state: "loading" });
    try {
      const answer = await codeRest.query("code_search", {
        q: asked.q,
        mode: asked.mode,
        repo,
        path: `/${file.path}`,
        ...(asked.ref ? { ref: asked.ref } : {}),
        context: SEARCH_CONTEXT,
      });
      // The engine names a tip only for a repository with lines in the answer: with none, say why.
      const now = answer.tips?.[repo];
      if (now === undefined) {
        const why = coverageNotes(answer.coverage).notes.join(" ");
        setMore({ state: "failed", detail: why.replace(/\.$/, "") });
        return;
      }
      if (now !== tip) {
        setMore({ state: "moved", now });
        return;
      }
      // A repo: or path: in the phrase widens the search, so other files may share the answer.
      const own = (answer.hits ?? []).filter(
        (h) => h.repo === repo && h.path === file.path && !h.path_b64,
      );
      const had = new Set(file.lines.map((h) => h.line));
      const fresh = own.filter((h) => !had.has(h.line)).map((h) => h.line);
      setMore({ state: "merged", hits: own });
      if (fresh.length > 0) setFocus({ line: Math.min(...fresh), link: true });
    } catch (err) {
      setMore({ state: "failed", detail: detailOf(err) });
    }
  };

  const total = file.total;
  const rest = total !== undefined && fileHref ? total - drawn : 0;
  return (
    <article className="code-search-file" aria-label={file.path} ref={card}>
      <div className="code-search-file__head">
        <FileCodeGlyph size="sm" className="code-glyph" />
        <span className="code-search-file__path mono">
          {fileHref ? (
            <a className="code-search-file__link" href={fileHref}>
              <span className="code-search-file__dir">{dirOf(file.path)}</span>
              <strong>{baseOf(file.path)}</strong>
            </a>
          ) : (
            <span title="Its path is not UTF-8, so no address names it">{file.path}</span>
          )}
        </span>
        <span className="t-caption muted">
          {linesWords({ count: total ?? file.lines.length, exact: total !== undefined })}
        </span>
        {fileHref && (
          <span className="code-search-file__acts">
            <CopyButton text={file.path} label={`Copy the path ${file.path}`} />
            <IconButtonLink
              href={fileHref}
              icon={<ExternalLinkGlyph />}
              size="sm"
              variant="ghost"
              label={`Open ${file.path}${at}`}
            />
          </span>
        )}
      </div>
      <div className="code-search-lines">
        {blocks.map((block, i) => {
          const next = blocks[i + 1];
          return (
            <Block
              key={block.first}
              repo={repo}
              tip={tip}
              file={file}
              block={block}
              grammar={grammar}
            >
              {next && (
                <Gap
                  after={block.last}
                  before={next.first}
                  refused={refused.get(block.last)}
                  onExpand={() => void reveal(block.last, next.first)}
                />
              )}
            </Block>
          );
        })}
      </div>
      {rest > 0 && fileHref && (
        <div className="code-search-file__foot">
          {more.state === "merged" ? (
            <p className="t-caption muted">
              Its other {plural(rest, "matching line")} did not fit one search:{" "}
              <a className="prose-link" href={fileHref}>
                open the file to read them
              </a>
              .
            </p>
          ) : more.state === "moved" ? (
            <>
              <p className="t-caption muted">
                {repo} moved since this search (now at {shortOID(more.now)}): search again to see
                this file’s other lines.
              </p>
              <Button variant="secondary" size="small" onClick={asked.again}>
                Search again
              </Button>
            </>
          ) : more.state === "failed" ? (
            <>
              <p className="t-caption muted">
                These lines could not be read{more.detail ? `: ${more.detail}` : ""}.
              </p>
              <Button variant="secondary" size="small" onClick={() => void showMore()}>
                Retry
              </Button>
            </>
          ) : (
            <Button
              variant="ghost"
              size="small"
              leadingIcon={<ChevronDownGlyph size="sm" />}
              loading={more.state === "loading"}
              onClick={() => void showMore()}
            >
              Show {plural(rest, "more matching line", "more matching lines")}
            </Button>
          )}
        </div>
      )}
    </article>
  );
}

/** The hidden lines between two blocks, read from the file's blob by its id, a step a press. */
function Gap({
  after,
  before,
  refused,
  onExpand,
}: {
  after: number;
  before: number;
  refused: string | undefined;
  onExpand: () => void;
}) {
  const hidden = before - after - 1;
  const label =
    hidden <= EXPAND_STEP
      ? `Expand ${plural(hidden, "hidden line")} between ${after} and ${before}`
      : `Expand ${EXPAND_STEP} of ${hidden} hidden lines after ${after}`;
  return (
    <li className="code-search-gap">
      <IconButton
        label={label}
        icon={<UnfoldVerticalGlyph />}
        size="sm"
        variant="ghost"
        onClick={onExpand}
      />
      {refused !== undefined && (
        <span className="t-caption muted">
          These lines are unavailable{refused ? `: ${refused}` : ""}.
        </span>
      )}
    </li>
  );
}

/** A run of consecutive lines, coloured from its first line so a comment opened in it carries on. */
function Block({
  repo,
  tip,
  file,
  block,
  grammar,
  children,
}: {
  repo: string;
  tip?: string;
  file: FileHits;
  block: SearchBlock;
  grammar: Grammar | null;
  children?: ReactNode;
}) {
  const states = useMemo(
    () =>
      grammar
        ? statesOf(
            grammar,
            block.rows.map((r) => r.text),
            "",
          )
        : null,
    [grammar, block],
  );
  return (
    <ol className="code-search-block">
      {block.rows.map((row, i) => {
        const at = row.hit ? hitAt(tip, file, row.line) : null;
        return (
          <li
            key={row.line}
            data-line={row.line}
            tabIndex={-1}
            className={row.hit ? "code-search-line code-search-line--hit" : "code-search-line"}
          >
            {at ? (
              <a className="code-search-line__number mono" href={href(repoPath(repo), at)}>
                {row.line}
              </a>
            ) : (
              <span className="code-search-line__number mono">{row.line}</span>
            )}
            <code className="code-search-line__text">
              <MarkedLine
                text={row.text}
                tokens={tokensOf(grammar, row.text, states?.[i] ?? null)}
                ranges={matchRanges(row.text, row.matches)}
              />
            </code>
          </li>
        );
      })}
      {children}
    </ol>
  );
}

/** A line's tokens with each match wrapped in a mark over them; a line with no match is painted as is. */
function MarkedLine({
  text,
  tokens,
  ranges,
}: {
  text: string;
  tokens: readonly Token[] | null;
  ranges: readonly (readonly [number, number])[];
}): ReactNode {
  if (ranges.length === 0) return <Painted text={text} tokens={tokens} />;
  // Consecutive pieces of one match share a mark, so a match across two tokens is one run.
  const groups: { mark: number; start: number; pieces: ReactNode[] }[] = [];
  let at = 0;
  for (const seg of markedSegments(text, tokens, ranges)) {
    const piece = text.slice(at, seg.end);
    const drawn = seg.kind ? (
      <span key={at} className={SYNTAX_CLASS[seg.kind]}>
        {piece}
      </span>
    ) : (
      piece
    );
    const last = groups[groups.length - 1];
    if (seg.mark >= 0 && last?.mark === seg.mark) last.pieces.push(drawn);
    else groups.push({ mark: seg.mark, start: at, pieces: [drawn] });
    at = seg.end;
  }
  return groups.map((g) =>
    g.mark < 0 ? (
      g.pieces[0]
    ) : (
      <mark key={`m${g.start}`} className="code-search-match">
        {g.pieces}
      </mark>
    ),
  );
}
