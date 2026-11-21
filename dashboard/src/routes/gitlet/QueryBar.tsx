/**
 * Gitlet › Changes' query bar: the address's filters as qualifier chips, and a draft of `q` for
 * the words. Enter submits every key at once (`changeQuery.ts`); a chip's Remove clears its key.
 */

import { useId, useRef, useState, type KeyboardEvent } from "react";
import {
  CircleAlertGlyph,
  CircleQuestionMarkGlyph,
  InfoGlyph,
  SearchGlyph,
  XGlyph,
} from "@crewlethq/icons/glyphs";
import { ChipField, IconButton, Popover } from "@crewlethq/ui";
import { useDraft, useNavigator } from "~/app/router.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { EVERY_STATE } from "~/lib/code.ts";
import {
  clearedPatch,
  filterChips,
  narrowed,
  parseChangeQuery,
  submitPatch,
  unknownNote,
  type FilterKey,
  type Filters,
} from "./changeQuery.ts";

/** How to search changes: each qualifier with what it narrows by. */
const HELP: readonly [term: string, what: string][] = [
  ["is:open", "its state: open, draft, merged or closed; none lists every state"],
  ["author:frontend", "who opened it: a seat's handle, or @me"],
  ["reviewer:jane", "whose review is asked"],
  ["repo:web", "its repository"],
  ["waiting-on:me", "what it waits on: me, review, fix or person"],
  ["readiness:checks", "where its merge stands: ready, approval, checks or blockers"],
  ["updated:7d", "moved in the last 24h, 7d or 30d"],
  ["work:<backend>:<id>", "a task's changes, as its link writes them"],
];

export function QueryBar({ filters, me }: { filters: Filters; me: string }) {
  const nav = useNavigator();
  const box = useRef<HTMLInputElement>(null);
  useSearchTarget(box);
  const problemId = useId();
  const [draft, setDraft] = useDraft(filters.text);
  const [problem, setProblem] = useState<string | undefined>();
  const note = unknownNote(parseChangeQuery(filters.text, me).unknown);

  const submit = () => {
    const parsed = parseChangeQuery(draft, me);
    if (parsed.problem) {
      setProblem(parsed.problem);
      return;
    }
    setProblem(undefined);
    setDraft(parsed.words);
    nav.submit(submitPatch(filters, parsed));
  };
  const remove = (key: string) => {
    setProblem(undefined);
    nav.filter({ [key as FilterKey]: key === "state" ? EVERY_STATE : null });
  };
  const clear = () => {
    setProblem(undefined);
    setDraft("");
    nav.submit(clearedPatch());
    box.current?.focus();
  };
  const keys = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Escape" && draft !== filters.text) {
      event.preventDefault();
      setDraft(filters.text);
      setProblem(undefined);
    } else if (event.key === "Enter" && draft.trim() === "") {
      // AN EMPTY BOX SUBMITS TOO, so a problem is cleared the same way it is said.
      event.preventDefault();
      submit();
    }
  };

  return (
    <form
      className="code-query"
      role="search"
      aria-label="Search changes"
      onSubmit={(event) => {
        event.preventDefault();
        submit();
      }}
    >
      <ChipField
        className="code-query__field"
        variant="boxed"
        prefix={<SearchGlyph size="sm" />}
        items={filterChips(filters)}
        onRemove={remove}
        query={draft}
        onQueryChange={(next) => {
          setDraft(next);
          setProblem(undefined);
        }}
        onCommit={submit}
        onKeyDown={keys}
        inputRef={box}
        label="Search changes"
        placeholder="Search changes"
        labels={{
          chosen: "Filters in the search",
          remove: (chip) => `Remove ${chip}`,
          removed: (chip) => `${chip} removed`,
        }}
        aria-invalid={problem ? true : undefined}
        aria-describedby={problem ? problemId : undefined}
      />
      <span className="code-query__end">
        <Popover
          label="How to search changes"
          align="end"
          height="layer"
          trigger={(open, toggle) => (
            <IconButton
              type="button"
              size="sm"
              variant="ghost"
              label="How to search changes"
              icon={<CircleQuestionMarkGlyph size="sm" />}
              aria-haspopup="dialog"
              aria-expanded={open}
              onClick={toggle}
            />
          )}
        >
          <div className="code-query__help">
            <dl>
              {HELP.map(([term, what]) => (
                <div key={term}>
                  <dt className="mono">{term}</dt>
                  <dd>{what}</dd>
                </div>
              ))}
            </dl>
            <p>
              Other words are searched in each change's title and description: every word must
              appear.
            </p>
          </div>
        </Popover>
        {narrowed(filters) && (
          <IconButton
            type="button"
            size="sm"
            variant="ghost"
            label="Clear the search and its filters"
            icon={<XGlyph size="sm" />}
            onClick={clear}
          />
        )}
      </span>
      {problem ? (
        <p id={problemId} className="code-query__problem" role="status">
          <CircleAlertGlyph size="sm" />
          {problem}
        </p>
      ) : (
        note && (
          <p className="code-query__note">
            <InfoGlyph size="sm" />
            {note}
          </p>
        )
      )}
    </form>
  );
}
