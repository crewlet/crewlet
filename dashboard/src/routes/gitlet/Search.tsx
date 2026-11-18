/**
 * Gitlet › Search: Gitlet's one grammar (`codeindex.ParseQuery`) and the bar's filters, all in the
 * address; asked over REST at no floor when Search is pressed, never per keystroke or on a reconnect.
 */

import { useMemo, useRef } from "react";
import {
  Button,
  Callout,
  EmptyState,
  FormField,
  Input,
  NoticeList,
  SegmentedControl,
  Skeleton,
} from "@crewlethq/ui";
import { SearchGlyph } from "@crewlethq/icons/glyphs";
import { useDraft, useNavigator, useParam } from "~/app/router.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { QueryState } from "~/components/common.tsx";
import { CODE_SEARCH_MODES } from "~/contract/code.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { RepoPicker } from "./RepoPicker.tsx";
import { Results } from "./search/Results.tsx";
import { coverageNotes, groupHits, SEARCH_CONTEXT, SEARCH_PER_FILE } from "~/lib/codeSearch.ts";
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
                  <Results groups={groups} asked={search} />
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
      <NoticeList notices={notes.notes.map((n) => ({ key: n, title: n }))} />
    </Callout>
  );
}
