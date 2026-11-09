/**
 * Gitlet › Search: the lines of the company's repositories that match a search,
 * by repository, then file, then line — each linking to its file at the
 * commit it was searched at, with the line picked.
 *
 * # One grammar, the engine's
 *
 * The phrase is `code_search`'s `q`, in Gitlet's one grammar
 * (`codeindex.ParseQuery`): `repo:`, `path:` and `ref:` filters first, then
 * the text. The bar's mode (`CODE_SEARCH_MODES`, held to the engine's), and
 * its repository, path and ref beside the phrase, are the question's own
 * parameters — the same filters said as controls, which the engine reads by
 * the grammar's rule (a repository or a path widens the search, a second ref
 * is refused). Every one of them is in the address
 * (`#/gitlet/search?q=&mode=&repo=&path=&ref=`), so a search is a link. A press
 * of Search PUSHES its phrase, path and ref (`nav.submit`), so the back button
 * walks a reader's searches rather than their keystrokes; the mode and the
 * repository are controls that narrow the search on screen, and replace. The
 * boxes are a draft of what the address says (`useDraft`), seeded again
 * whenever it comes to say something else — Back, or ⌘K's "See every match"
 * opening a phrase on this screen — so a box never shows one phrase over the
 * lines of another.
 *
 * # Asked when the reader presses Search, never per keystroke
 *
 * A search across the company is a scatter over every Gitlet node that holds a
 * repository the reader reads: one signed request each, and a second round
 * for what the first left unanswered. So the phrase is asked when it is
 * submitted, as Work's search is, and never as it is typed — and a reconnect
 * asks again only a search that never reached the engine
 * (`refetchOnReconnect: "unreached"`): an answer names the commit each
 * repository was searched at, so asking again only repeats the work, and a
 * rolling restart reconnecting every socket would ask every open tab's at
 * once.
 *
 * # Over REST, at no floor
 *
 * Like every code question it is asked over REST (`lib/useCodeQuery.ts`), and
 * unlike the rest it takes no session floor: it answers at the commit each
 * repository was searched at, named in `tips`, so a write this tab made has
 * nothing to wait for here (`contract/domains.ts`).
 *
 * # What was not searched is said on every answer
 *
 * See `lib/codeSearch.ts`: the coverage is a Callout above the lines, never hidden,
 * and a warning where any repository was left unanswered or the lines cut.
 */

import { memo, useMemo, useRef } from "react";
import {
  Button,
  Callout,
  EmptyState,
  FormField,
  Input,
  SegmentedControl,
  Skeleton,
} from "@crewlethq/ui";
import { SearchGlyph } from "@crewlethq/icons/glyphs";
import { href, useDraft, useNavigator, useParam } from "~/app/router.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { QueryState } from "~/components/common.tsx";
import { CODE_SEARCH_MODES } from "~/contract/code.ts";
import { repoPath, shortOID } from "~/lib/code.ts";
import { plural } from "~/lib/format.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { repoHref } from "./paths.ts";
import { RepoPicker } from "./RepoPicker.tsx";
import { coverageNotes, groupHits, hitAt, type FileHits, type RepoHits } from "~/lib/codeSearch.ts";

/** Each mode, as the bar offers it. */
const MODE_WORDS: Readonly<
  Record<(typeof CODE_SEARCH_MODES)[number], { label: string; hint: string }>
> = {
  literal: { label: "Text", hint: "The text as written" },
  regex: { label: "Regex", hint: "An RE2 expression, matched within a line" },
  word: { label: "Word", hint: "The text as a whole word" },
};

/** The mode an address names, or the engine's default where it names none this build offers. */
function asMode(raw: string): (typeof CODE_SEARCH_MODES)[number] {
  return (CODE_SEARCH_MODES as readonly string[]).includes(raw)
    ? (raw as (typeof CODE_SEARCH_MODES)[number])
    : "literal";
}

/** "Every repository" in the bar's picker: no `repo=` at all. */
const EVERY_REPO = "";

export function Search() {
  // `/` FOCUSES THIS SCREEN'S PHRASE rather than opening the palette over it.
  const box = useRef<HTMLInputElement>(null);
  useSearchTarget(box);
  const nav = useNavigator();
  const [q] = useParam("q", "", "filter");
  const [modeRaw, setMode] = useParam("mode", "literal", "filter");
  const mode = asMode(modeRaw);
  const [repo, setRepo] = useParam("repo", EVERY_REPO, "filter");
  const [path] = useParam("path", "", "filter");
  const [ref] = useParam("ref", "", "filter");
  // WHAT IS TYPED IS NOT YET ASKED, and is the address's again whenever the
  // address moves: see the file's doc.
  const [typedQ, setTypedQ] = useDraft(q);
  const [typedPath, setTypedPath] = useDraft(path);
  const [typedRef, setTypedRef] = useDraft(ref);
  const asked = q.trim() !== "";
  const answer = useCodeQuery(
    "code_search",
    { q, mode, repo: repo || undefined, path: path || undefined, ref: ref || undefined },
    { enabled: asked, refetchOnReconnect: "unreached" },
  );
  const groups = useMemo(() => (answer.data ? groupHits(answer.data) : []), [answer.data]);

  return (
    <>
      <form
        className="work-search-form"
        onSubmit={(e) => {
          e.preventDefault();
          nav.submit({ q: typedQ.trim(), path: typedPath.trim(), ref: typedRef.trim() });
        }}
      >
        <div className="work-search-phrase">
          <Input
            type="search"
            width="full"
            aria-label="Search the company’s code"
            ref={box}
            leading={<SearchGlyph size="sm" />}
            placeholder="A name, a string or a pattern (repo:, path: and ref: filter it)"
            value={typedQ}
            onChange={(e) => setTypedQ(e.target.value)}
          />
        </div>
        <div className="work-search-how">
          <SegmentedControl
            semantics="radio"
            activate="manual"
            value={mode}
            onValueChange={setMode}
            label="Match"
            options={CODE_SEARCH_MODES.map((m) => ({
              value: m,
              label: MODE_WORDS[m].label,
              title: MODE_WORDS[m].hint,
            }))}
          />
          <Button variant="primary" leadingIcon={<SearchGlyph size="sm" />} type="submit">
            Search
          </Button>
        </div>
        {/* ONE ROW OF LABELLED FILTERS under the phrase, each its own size:
            a placeholder alone was gone once typed, and each field took a row. */}
        <div className="code-search-filters">
          <div className="code-search-filter">
            <RepoPicker repo={repo} onChoose={setRepo} every="Every indexed repository" />
          </div>
          <FormField label="Path" className="code-search-filter">
            {(field) => (
              <Input
                id={field.id}
                width="full"
                spellCheck={false}
                placeholder="internal/"
                value={typedPath}
                onChange={(e) => setTypedPath(e.target.value)}
              />
            )}
          </FormField>
          <FormField label="Branch, tag or commit" className="code-search-filter">
            {(field) => (
              <Input
                id={field.id}
                width="full"
                spellCheck={false}
                placeholder="main"
                value={typedRef}
                onChange={(e) => setTypedRef(e.target.value)}
              />
            )}
          </FormField>
        </div>
      </form>

      {!asked ? (
        <EmptyState
          icon={<SearchGlyph size="xl" />}
          title="Search the company’s code"
          description="Every repository you read that keeps a search index, or the ones a repo: filter names. A line matches within itself; regex is RE2."
        />
      ) : (
        <>
          {answer.loading && !answer.data && (
            <Skeleton variant="text" rows={6} label="Searching the repositories" />
          )}
          <QueryState
            error={answer.error}
            refusal={answer.refusal}
            detail={answer.detail}
            loading={answer.loading}
          >
            {answer.data && (
              <>
                <SearchCoverage coverage={answer.data.coverage} />
                {groups.length === 0 ? (
                  <EmptyState
                    icon={<SearchGlyph size="xl" />}
                    title={`No line matches “${q}”`}
                    description="In what was searched: the note above says what that was."
                  />
                ) : (
                  <div className="code-search-results">
                    {groups.map((g) => (
                      <RepoResult key={g.repo} group={g} />
                    ))}
                  </div>
                )}
              </>
            )}
          </QueryState>
        </>
      )}
    </>
  );
}

/** What the search covered, above the lines it found — on every answer. */
export function SearchCoverage({ coverage }: { coverage: Parameters<typeof coverageNotes>[0] }) {
  const notes = coverageNotes(coverage);
  return (
    <Callout variant={notes.variant} title={notes.lead}>
      {notes.notes.length > 0 && (
        <ul className="code-search-coverage">
          {notes.notes.map((n) => (
            <li key={n}>{n}</li>
          ))}
        </ul>
      )}
    </Callout>
  );
}

/** One repository's files and their lines, named with the commit they were read at. */
const RepoResult = memo(function RepoResult({ group }: { group: RepoHits }) {
  return (
    <section className="code-search-repo" aria-label={group.repo}>
      <h3 className="code-search-repo__head">
        <a className="mono" href={repoHref(group.repo, group.tip ? { ref: group.tip } : {})}>
          {group.repo}
        </a>
        <span className="t-caption muted">
          {plural(group.lines, "line")} in {plural(group.files.length, "file")}
          {group.tip && (
            <>
              {" "}
              at <span className="mono">{shortOID(group.tip)}</span>
            </>
          )}
        </span>
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
  return (
    <div className="code-search-file">
      <div className="code-search-file__path mono">
        {first ? (
          <a className="code-search-file__link" href={href(repoPath(repo), fileOnly(first))}>
            {file.path}
          </a>
        ) : (
          <span title="Its path is not UTF-8, so no address names it">{file.path}</span>
        )}
      </div>
      <ol className="code-search-lines">
        {file.lines.map((hit) => {
          const at = hitAt(tip, file, hit.line);
          return (
            <li key={hit.line} className="code-search-line">
              {at ? (
                <a className="code-search-line__number mono" href={href(repoPath(repo), at)}>
                  {hit.line}
                </a>
              ) : (
                <span className="code-search-line__number mono">{hit.line}</span>
              )}
              <code className="code-search-line__text">{hit.text}</code>
            </li>
          );
        })}
      </ol>
    </div>
  );
}
