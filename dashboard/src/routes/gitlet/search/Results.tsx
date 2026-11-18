/** Gitlet › Search's results: each repository's files as cards of blocks, a hit with the lines around it. */

import { memo, useMemo } from "react";
import { IconButtonLink } from "@crewlethq/ui";
import { ExternalLinkGlyph, FileCodeGlyph, SquareCodeGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { repoPath, shortOID } from "~/lib/code.ts";
import {
  baseOf,
  dirOf,
  fileBlocks,
  hitAt,
  matchingLines,
  type FileHits,
  type RepoHits,
  type SearchBlock,
} from "~/lib/codeSearch.ts";
import { plural } from "~/lib/format.ts";
import { statesOf } from "~/lib/syntax/index.ts";
import type { Grammar } from "~/lib/syntax/types.ts";
import { CopyButton } from "../change/CopyButton.tsx";
import { commitHref, repoHref } from "../paths.ts";
import { Painted, tokensOf, useGrammar } from "../syntax.tsx";

/** "9 matching lines", or "at least 9" where a file's total was not answered. */
function linesWords({ count, exact }: { count: number; exact: boolean }): string {
  return `${exact ? "" : "at least "}${plural(count, "matching line")}`;
}

export function Results({ groups }: { groups: readonly RepoHits[] }) {
  return (
    <div className="code-search-results">
      {groups.map((g) => (
        <RepoResult key={g.repo} group={g} />
      ))}
    </div>
  );
}

/** One repository's files, named with the commit they were read at. */
const RepoResult = memo(function RepoResult({ group }: { group: RepoHits }) {
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
        <FileResult key={f.pathB64 ?? f.path} repo={group.repo} tip={group.tip} file={f} />
      ))}
    </section>
  );
});

/** A hit's address with no line picked: the file itself. */
function fileOnly(at: Record<string, string>): Record<string, string> {
  return Object.fromEntries(Object.entries(at).filter(([key]) => key !== "lines"));
}

function FileResult({ repo, tip, file }: { repo: string; tip?: string; file: FileHits }) {
  const first = hitAt(tip, file, file.lines[0]?.line ?? 1);
  const grammar = useGrammar(file.path);
  const blocks = useMemo(() => fileBlocks(file.lines), [file.lines]);
  const fileHref = first ? href(repoPath(repo), fileOnly(first)) : null;
  const at = tip ? ` at ${shortOID(tip)}` : "";
  return (
    <article className="code-search-file" aria-label={file.path}>
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
          {linesWords({ count: file.total ?? file.lines.length, exact: file.total !== undefined })}
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
        {blocks.map((block) => (
          <Block
            key={block.first}
            repo={repo}
            tip={tip}
            file={file}
            block={block}
            grammar={grammar}
          />
        ))}
      </div>
    </article>
  );
}

/** A run of consecutive lines, coloured from its first line so a comment opened in it carries on. */
function Block({
  repo,
  tip,
  file,
  block,
  grammar,
}: {
  repo: string;
  tip?: string;
  file: FileHits;
  block: SearchBlock;
  grammar: Grammar | null;
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
              <Painted text={row.text} tokens={tokensOf(grammar, row.text, states?.[i] ?? null)} />
            </code>
          </li>
        );
      })}
    </ol>
  );
}
