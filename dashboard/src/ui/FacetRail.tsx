/**
 * One dimension of a list, as chips that narrow it.
 *
 * TWO SCREENS HAD TWO OF THESE. The Inbox rendered `.facet-row` buttons over
 * the reasons on the loaded page; the event log rendered `Chip`s over a closed
 * set of categories. They looked different, behaved differently under the
 * keyboard, and disagreed about the one thing that actually matters here —
 * whether a count is over what is loaded or over what exists.
 *
 * THAT DISTINCTION IS THE COMPONENT'S WHOLE JOB, so it is a required prop
 * rather than a caption a caller remembers to add. A number beside a chip is
 * read as "how many there are", and when it is really "how many of the four
 * hundred rows this tab happens to be holding" the reader is being told
 * something false about their own company. `over` says which, once, where a
 * reader can see it.
 *
 * A VALUE WITH NOTHING IN IT IS STILL SHOWN when the set is closed — it says
 * the dimension exists and is quiet, which is an answer — and is dropped when
 * the set is derived from the rows, where its absence is all there is to say.
 * The caller decides by what it passes.
 */

import { Chip } from "~/ui/primitives.tsx";

/** One value of the dimension. */
export interface Facet {
  /** What the filter is set to. Empty is the "all" chip and never appears here. */
  value: string;
  label: string;
  /** How many rows carry it, or null where the caller genuinely cannot say. */
  count: number | null;
  /** Said on hover — why a value with no rows is still offered, say. */
  title?: string;
}

/** What a count is a count OF. */
export type FacetScope = "loaded" | "window";

const SCOPE_NOTE: Record<FacetScope, string> = {
  // The honest sentence, and it is not decoration: the engine counts nothing
  // server-side for these, so a chip claiming a total would be inventing a
  // number about somebody's company.
  loaded: "counts over the rows loaded",
  window: "counts over the whole window",
};

export function FacetRail({
  name,
  value,
  facets,
  onChange,
  over,
  allLabel = "All",
}: {
  /** The dimension, for the group's label: "Category", "Reason". */
  name: string;
  /** What is selected. Empty means all of them. */
  value: string;
  facets: Facet[];
  onChange: (next: string) => void;
  over: FacetScope;
  allLabel?: string;
}) {
  if (facets.length === 0) return null;
  return (
    <div className="facet-row" role="group" aria-label={name}>
      <Chip on={!value} onClick={() => onChange("")}>
        {allLabel}
      </Chip>
      {facets.map((f) => (
        <Chip
          key={f.value}
          on={value === f.value}
          count={f.count}
          title={f.title}
          // A SECOND PRESS CLEARS IT. A chip that only ever narrows makes
          // the reader hunt for a "clear" button to undo the thing they
          // just did, which is the one gesture every list gets wrong.
          onClick={() => onChange(value === f.value ? "" : f.value)}
        >
          {f.label}
        </Chip>
      ))}
      <span className="t-caption">{SCOPE_NOTE[over]}</span>
    </div>
  );
}
