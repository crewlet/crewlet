/**
 * Gitlet › Search: Gitlet's one grammar (`codeindex.ParseQuery`) and the bar's filters, all in the
 * address; asked over REST at no floor when Search is pressed, never per keystroke or on a reconnect.
 */

import { useMemo, useRef, type ReactNode } from "react";
import {
  Button,
  Callout,
  Disclosure,
  EmptyState,
  Input,
  NoticeList,
  SegmentedControl,
  Skeleton,
} from "@crewlethq/ui";
import { FolderGlyph, GitBranchGlyph, SearchGlyph, SquareCodeGlyph } from "@crewlethq/icons/glyphs";
import { useDraft, useNavigator, useParam } from "~/app/router.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { QueryState } from "~/components/common.tsx";
import { CODE_SEARCH_MODES } from "~/contract/code.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import type { CodeSearchAnswer } from "~/contract/code.ts";
import { RepoPicker } from "./RepoPicker.tsx";
import { Facets } from "./search/Facets.tsx";
import { Results } from "./search/Results.tsx";
import {
  coverageNotes,
  facetsOf,
  groupHits,
  SEARCH_CONTEXT,
  SEARCH_PER_FILE,
  type RepoHits,
  type SearchLead,
} from "~/lib/codeSearch.ts";
import "~/styles/gitlet.css";

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

/** Below this width of the page the facets fold above the results. */
const FACETS_FOLD = 720;

/** A filter's words inside its field, before its value. */
function Lead({ icon, words }: { icon: ReactNode; words: string }) {
  return (
    <span className="code-search-filter__lead">
      {icon}
      {words}
    </span>
  );
}

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
  // What is typed is not yet asked, and is seeded again whenever the address moves
  // (Back, or ⌘K's "See every match" opening a phrase here).
  const [typedQ, setTypedQ] = useDraft(q);
  const [typedPath, setTypedPath] = useDraft(path);
  const [typedRef, setTypedRef] = useDraft(ref);
  const asked = q.trim() !== "";
  const answer = useCodeQuery(
    "code_search",
    {
      q,
      mode,
      repo: repo || undefined,
      path: path || undefined,
      ref: ref || undefined,
      context: SEARCH_CONTEXT,
      per_file: SEARCH_PER_FILE,
    },
    { enabled: asked, refetchOnReconnect: "unreached" },
  );
  const groups = useMemo(() => (answer.data ? groupHits(answer.data) : []), [answer.data]);
  const again = answer.refetch;
  const search = useMemo(() => ({ q, mode, ref, again }), [q, mode, ref, again]);
  const grid = useRef<HTMLDivElement>(null);
  const narrow = useNarrowerThan(grid, FACETS_FOLD, groups.length > 0);

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
            trailing={
              <span className="code-search-grammar t-caption" aria-hidden="true">
                <span className="mono">repo:</span> <span className="mono">path:</span>{" "}
                <span className="mono">ref:</span> narrow it
              </span>
            }
            placeholder="A name, a string or a pattern"
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
        <div className="code-search-filters">
          <div className="code-search-filter code-search-filter--repo">
            <RepoPicker
              repo={repo}
              onChoose={setRepo}
              every="All"
              size="sm"
              leading={<Lead icon={<SquareCodeGlyph size="xs" />} words="Repository:" />}
            />
          </div>
          <div className="code-search-filter">
            <Input
              inputSize="sm"
              width="full"
              appearance="reference"
              aria-label="Path"
              spellCheck={false}
              leading={<Lead icon={<FolderGlyph size="xs" />} words="Path:" />}
              placeholder="every path"
              value={typedPath}
              onChange={(e) => setTypedPath(e.target.value)}
            />
          </div>
          <div className="code-search-filter">
            <Input
              inputSize="sm"
              width="full"
              appearance="reference"
              aria-label="Branch, tag or commit"
              spellCheck={false}
              leading={<Lead icon={<GitBranchGlyph size="xs" />} words="Ref:" />}
              placeholder="default branches"
              value={typedRef}
              onChange={(e) => setTypedRef(e.target.value)}
            />
          </div>
          <p className="code-search-rule t-caption muted">
            A line matches within itself. Regex is RE2.
          </p>
        </div>
      </form>

      {!asked ? (
        <EmptyState
          icon={<SearchGlyph size="xl" />}
          title="Search the company’s code"
          description="Every repository you read that keeps a search index, or one a repo: filter names."
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
            {answer.data &&
              (groups.length === 0 ? (
                <>
                  <SearchCoverage coverage={answer.data.coverage} groups={groups} search={search} />
                  <EmptyState
                    icon={<SearchGlyph size="xl" />}
                    title={`No line matches “${q}”`}
                    description="In what was searched: the line above says what that was."
                  />
                </>
              ) : (
                <div
                  className={narrow ? "code-search code-search--narrow" : "code-search"}
                  ref={grid}
                >
                  <Found data={answer.data} groups={groups} narrow={narrow} search={search} />
                </div>
              ))}
          </QueryState>
        </>
      )}
    </>
  );
}

/** The facets beside the coverage line and the files, or folded above them on a narrow page. */
function Found({
  data,
  groups,
  narrow,
  search,
}: {
  data: CodeSearchAnswer;
  groups: readonly RepoHits[];
  narrow: boolean;
  search: Parameters<typeof Results>[0]["asked"];
}) {
  const facets = useMemo(() => facetsOf(groups, data.coverage), [groups, data.coverage]);
  return (
    <>
      {narrow ? (
        <Disclosure title="Narrow by repository or path" size="compact" headingLevel="none">
          <Facets facets={facets} />
        </Disclosure>
      ) : (
        <Facets facets={facets} />
      )}
      <div className="code-search-main">
        <SearchCoverage coverage={data.coverage} groups={groups} search={search} />
        <Results groups={groups} asked={search} />
      </div>
    </>
  );
}

/** The lead in its words: the count strong and the ref in mono. */
function LeadWords({ lead }: { lead: SearchLead }) {
  return (
    <>
      {lead.count ? <strong>{lead.count}</strong> : null}
      {lead.rest}
      {lead.ref ? (
        <>
          , at <span className="mono">{lead.ref}</span>
        </>
      ) : lead.ref === "" ? (
        ", on their default branches"
      ) : null}
      .
    </>
  );
}

/** What the search found and covered: one quiet line, or a warning where lines may be missing. */
export function SearchCoverage({
  coverage,
  groups,
  search,
}: {
  coverage: CodeSearchAnswer["coverage"];
  groups: readonly RepoHits[];
  search: { q: string; ref: string };
}) {
  const notes = coverageNotes(coverage, groups, search.ref, search.q);
  if (notes.variant === "warning") {
    return (
      <Callout variant="warning" title={<LeadWords lead={notes.lead} />}>
        <NoticeList notices={notes.notes.map((n) => ({ key: n, title: n }))} />
      </Callout>
    );
  }
  return (
    <p className="code-search-summary t-body">
      <LeadWords lead={notes.lead} />
      {notes.notes.map((n) => ` ${n}`).join("")}
    </p>
  );
}
