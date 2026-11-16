/**
 * A repository's history, drawn as a code host draws it: by the day each
 * commit was written, each commit its subject, who wrote it and when, who
 * pushed it and for which work item, and its id with its copy.
 *
 * THE HISTORY is git's (`code_log`): the commits that changed the path, or any
 * path, newest first from the revision, a page at a time, each with the push
 * that introduced it where the answering Gitlet node's index holds it — who
 * pushed it, which the commit's own author line is not — each opening the
 * commit's own page, and each branch but the default offering a comparison
 * with it. A page is what the
 * walk covered, so one can be short with more to come, and each page is joined
 * to the one before it (`lib/usePaged.ts`).
 *
 * A TIMELINE OF THE READER'S DAYS each commit was AUTHORED, in git's order: a
 * day met again after another opens its own heading.
 */

import { useMemo, type ReactNode } from "react";
import { Button, IconButtonLink, Skeleton } from "@crewlethq/ui";
import { BotGlyph, CodeGlyph, GitCommitVerticalGlyph, UploadGlyph } from "@crewlethq/icons/glyphs";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { noDefaultHead, shortOID } from "~/lib/code.ts";
import { ago } from "~/lib/ago.ts";
import { fmtDate, fmtDateTime, plural } from "~/lib/format.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type {
  CodeGitAnswer,
  CodeLogCommit,
  CodePushedBy,
  CodeRepoDetail,
} from "~/protocol/index.ts";
import { CodeList, useDayGroups, type ListDay } from "../CodeList.tsx";
import { commitHref, repoHref } from "../paths.ts";
import { Face, ShaChip, useWhoName } from "../ui.tsx";
import { RefPicker, revisionWord, type Revisions } from "./refs.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import "~/styles/gitlet.css";

/** How a commit reached its branch, as its chip says it: a glyph and its words. */
export interface Provenance {
  glyph: "run" | "push";
  words: ReactNode;
}

/**
 * How the push the index holds brought a commit: a run for its work item, the
 * author with git, or who pushed it. None where the index holds no push.
 */
export function provenanceOf(
  by: CodePushedBy | undefined,
  author: string,
  whoName: (who: string) => string,
): Provenance | null {
  if (!by) return null;
  const pusher = by.principal.seat ?? by.principal.operator_id ?? "";
  const work = by.principal.run?.work_key;
  const theAuthor = pusher !== "" && whoName(pusher) === whoName(author);
  if (work) {
    const key = <span className="code-commit-prov__key mono">{work}</span>;
    return {
      glyph: "run",
      words: theAuthor ? (
        <>pushed by a run for {key}</>
      ) : (
        <>
          pushed by a run of {whoName(pusher)} for {key}
        </>
      ),
    };
  }
  if (theAuthor && by.via === "git") return { glyph: "push", words: "pushed with git" };
  if (pusher === "") return null;
  return { glyph: "push", words: `pushed by ${whoName(pusher)}` };
}

/** One commit's meta line: who wrote it and when, then how it reached its branch. */
function CommitMeta({
  commit,
  by,
  now,
}: {
  commit: CodeLogCommit;
  by: CodePushedBy | undefined;
  now: number;
}) {
  const whoName = useWhoName();
  const author = commit.author ?? `(${commit.author_bytes ?? 0} bytes)`;
  const prov = provenanceOf(by, author, whoName);
  const Glyph = prov?.glyph === "run" ? BotGlyph : UploadGlyph;
  return (
    <>
      <Face who={author} />
      <span className="code-commit-meta__who" title={commit.author_email}>
        {whoName(author)}
      </span>
      <span>
        authored{" "}
        <time dateTime={commit.authored_at} title={fmtDateTime(commit.authored_at)}>
          {ago(commit.authored_at, now)}
        </time>
      </span>
      {prov && (
        <span
          className="code-commit-prov"
          title={by?.principal.operator_id ? `through ${by.principal.operator_id}` : undefined}
        >
          <Glyph size="xs" aria-hidden />
          <span>{prov.words}</span>
        </span>
      )}
    </>
  );
}

/** The commit's files, browsed at it. */
function BrowseAt({ repo, oid }: { repo: string; oid: string }) {
  return (
    <IconButtonLink
      href={repoHref(repo, { ref: oid })}
      icon={<CodeGlyph />}
      label={`Browse the files at ${shortOID(oid)}`}
      size="sm"
      variant="ghost"
    />
  );
}

/** When a commit was authored, the day its heading groups it by. */
const authoredAt = (c: CodeLogCommit) => c.authored_at;

/** A day's heading: the day its commits were authored, or "Commits" where the time does not parse. */
const dayHeading = (day: ListDay | null, c: CodeLogCommit) =>
  day ? `Commits on ${fmtDate(c.authored_at)}` : "Commits";

/** A commit's subject, or what git left of one past its bound. */
export function subjectOf(c: { subject?: string; subject_bytes?: number }): string {
  if (c.subject) return c.subject;
  return c.subject_bytes ? `(a subject of ${c.subject_bytes} bytes)` : "(no message)";
}

/**
 * The history at the revision, of the path the address names or of every
 * path, newest first. Its picker lists the branches and tags its page read
 * once ([useRevisions]).
 */
export function Commits({ repo, revisions }: { repo: CodeRepoDetail; revisions: Revisions }) {
  const now = useNow();
  const [rev, setRev] = useParam("ref", "", "filter");
  const [path, setPath] = useParam("path", "", "filter");
  const params = useMemo(
    () => ({ name: repo.name, ...(rev ? { ref: rev } : {}), ...(path ? { path } : {}) }),
    [repo.name, rev, path],
  );
  const first = useCodeQuery("code_log", params, {
    enabled: Boolean(repo.head) || rev !== "",
  });
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_log", { ...params, cursor }),
    (a: CodeGitAnswer) => a.log?.commits ?? [],
    (c: CodeLogCommit) => c.oid,
    (a: CodeGitAnswer) => a.next ?? "",
  );
  // WHO PUSHED EACH, from the page that named it: a commit is on one page.
  const pushedBy = useMemo(() => {
    const all: Record<string, CodePushedBy> = {};
    for (const a of paged.answers) Object.assign(all, a.pushed_by ?? {});
    return all;
  }, [paged.answers]);
  const rows = paged.items;
  const groups = useDayGroups(rows, authoredAt, dayHeading);
  if (!repo.head && rev === "") {
    // THE DEFAULT BRANCH HOLDS NOTHING: with no branch at all there is no
    // history to read, and with others live the picker reaches theirs.
    return repo.branches === 0 ? (
      <p className="muted">Nobody has pushed a branch yet: {repo.default_branch} has no history.</p>
    ) : (
      <>
        <PageToolbar>
          <RefPicker repo={repo} revisions={revisions} value={rev} onChoose={setRev} />
        </PageToolbar>
        <p className="muted">{noDefaultHead(repo)}</p>
      </>
    );
  }
  return (
    <>
      <PageToolbar>
        <RefPicker repo={repo} revisions={revisions} value={rev} onChoose={setRev} />
        {path && (
          <span className="row gap-1">
            <span className="t-caption">
              History of <span className="mono">{path}</span>
            </span>
            <Button size="small" variant="ghost" onClick={() => setPath("")}>
              Clear
            </Button>
          </span>
        )}
      </PageToolbar>
      {paged.loading && rows.length === 0 && (
        <Skeleton
          variant="text"
          rows={6}
          label={`Loading the history of ${revisionWord(repo, rev)}`}
        />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={
          rows.length > 0 || paged.more
            ? undefined
            : {
                title: path ? `No commit changed ${path}` : "No commit",
                hint: `The history is read from ${revisionWord(repo, rev)}, newest first.`,
              }
        }
      >
        <CodeList<CodeLogCommit>
          groups={groups}
          look="timeline"
          spine={GitCommitVerticalGlyph}
          rowKey={(c) => c.oid}
          rowHref={(c) => commitHref(repo.name, c.oid)}
          aria-label={`History of ${revisionWord(repo, rev)}`}
          title={subjectOf}
          meta={(c) => <CommitMeta commit={c} by={pushedBy[c.oid]} now={now} />}
          facts={(c) => (
            <>
              <ShaChip oid={c.oid} />
              <BrowseAt repo={repo.name} oid={c.oid} />
            </>
          )}
          paged={paged}
          moreLabel="Load older commits"
          foot={
            rows.length > 0 &&
            `${plural(rows.length, "commit")} of ${revisionWord(repo, rev)} loaded, newest first`
          }
        />
      </QueryState>
    </>
  );
}
