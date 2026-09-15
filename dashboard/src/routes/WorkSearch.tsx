/**
 * Ranking the company's work against a phrase.
 *
 * # The engine has always been able to do this
 *
 * `search_work` is a builtin every seat holds: BM25 over the engine's own
 * inverted list, which exists because Turso has no fts5 and the alternative
 * was refusing knowledge search on the only driver this build ships. An agent
 * looking for "the thing about the billing webhook" gets a ranked list; the
 * operator reading the same company had `q=` — an escaped LIKE over the
 * excerpt, gated to a span of days, that matches a substring or nothing.
 *
 * So the two readers this product is for were looking at the same items
 * through two different instruments, and only one of them could find anything.
 *
 * # Ranked is not filtered, and the screen says which it is
 *
 * The board's filters answer "which items are in this state"; this answers
 * "which items are most about this phrase". What comes back is a PLACE and not
 * a score — the arithmetic that ordered these is finished before a coordinator
 * sees them — so the order is the whole result and this screen never re-sorts
 * it. A grid that let a reader sort by title would throw away the only thing
 * this question produces.
 *
 * The SNIPPET is the index's own excerpt, and it is why a ranked answer is
 * readable without opening every hit — it is the half of a search result that
 * a board row cannot have, because a board row does not know what you asked.
 *
 * # An index still building is not an empty result
 *
 * A node that joined recently has the rows and not the index. Reported as
 * `available: false` with a reason rather than as an error, because nothing is
 * wrong — and drawn as its own state, because a reader told "nothing matches"
 * files the duplicate.
 */

import { useState } from "react";
import { href, useParam } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { KeyCell, TextCell } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Button, Empty } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import type { WorkRanked } from "~/protocol/index.ts";

export function WorkSearch() {
  // THE QUERY IS IN THE URL, which is what makes a search shareable and what
  // makes the back button walk a reader's searches rather than their
  // keystrokes: it is a FILTER, so typing replaces the history entry.
  const [q, setQ] = useParam("q", "", "filter");
  const [typed, setTyped] = useState(q);
  const hits = useQuery("work_search", { q }, { enabled: q.trim() !== "" });
  usePageCoverage(undefined);

  const rows = hits.data?.hits ?? [];

  return (
    <>
      <PageNote>
        The same ranking a seat gets from <code className="inline">search_work</code> — BM25 over
        the engine&rsquo;s own index, not a substring match. The board&rsquo;s filters answer which
        items are in a state; this answers which are most about a phrase.
      </PageNote>

      <form
        className="row gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          setQ(typed.trim());
        }}
      >
        <label className="field" style={{ flex: 1 }}>
          <span className="sr-only">Search the company&rsquo;s work</span>
          <input
            type="search"
            className="input"
            placeholder="A phrase — the words somebody would have written"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
          />
        </label>
        <Button variant="primary" icon="search" type="submit">
          Search
        </Button>
      </form>

      {q.trim() === "" ? (
        <Empty
          icon="search"
          title="Type what you half remember"
          hint="The ranking is over titles, bodies and comments — the words somebody actually wrote, rather than a key or a status."
        />
      ) : hits.data && !hits.data.available ? (
        // NOT AN EMPTY RESULT. See the file head: this node has the items and
        // not yet the index, and a reader told "no matches" acts on it.
        <Empty
          icon="clock"
          title="This node is still indexing"
          hint={
            hits.data.note ||
            "It joined recently, so the company's work is not all findable from here yet. The board's own filters answer in the meantime."
          }
        />
      ) : (
        <QueryState
          error={hits.error}
          loading={hits.loading}
          empty={
            rows.length
              ? undefined
              : {
                  title: "Nothing matched",
                  hint: "The index is over the words in an item, so a key or a status belongs in the board's filters instead.",
                }
          }
        >
          <DataGrid<WorkRanked>
            rows={rows}
            rowKey={(r) => r.id}
            rowHref={(r) => href(["work", r.key || r.id])}
            // NO DEFAULT SORT. The answer's own order IS the result, and a
            // grid that re-sorted it by title would throw away the only
            // thing this question produces.
            columns={[
              {
                key: "rank",
                header: "#",
                shrink: true,
                cell: (r) => <span className="rank-place">{r.rank}</span>,
              },
              {
                key: "title",
                header: "Item",
                cell: (r) => (
                  <div className="col gap-1">
                    <TextCell icon="check">{r.title}</TextCell>
                    {/* THE INDEX'S OWN EXCERPT — the half a board row cannot
                        have, because a board row does not know what you
                        asked. */}
                    {r.snippet && <span className="t-caption faint">{r.snippet}</span>}
                  </div>
                ),
              },
              {
                key: "key",
                header: "Key",
                shrink: true,
                cell: (r) => (r.key ? <KeyCell value={r.key} /> : <span className="faint">—</span>),
              },
              {
                key: "project",
                header: "Project",
                shrink: true,
                cell: (r) =>
                  r.project ? <Badge outline>{r.project}</Badge> : <span className="faint">—</span>,
              },
              {
                key: "status",
                header: "Status",
                shrink: true,
                cell: (r) =>
                  r.status ? <Badge outline>{r.status}</Badge> : <span className="faint">—</span>,
              },
              {
                key: "assignee",
                header: "Assignee",
                shrink: true,
                cell: (r) =>
                  r.assignee ? (
                    <Badge outline>{r.assignee}</Badge>
                  ) : (
                    <span className="faint">unassigned</span>
                  ),
              },
            ]}
            loadedNote={`${rows.length} ranked by relevance`}
          />
        </QueryState>
      )}
    </>
  );
}
