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
 *
 * # A hit is opened beside the ranking, not instead of it
 *
 * Searching is a loop: you read the phrase back, open the third hit, decide it
 * is not the one, and open the fifth. Every one of those used to be a
 * navigation and a way back, which is the one journey a RANKED answer cannot
 * afford — the ranking is the whole product of the question, and a reader who
 * has to leave it to check a hit is being asked to remember it. So a plain
 * click opens the item in the frame's rail with the list still behind it, and
 * `[` and `]` step DOWN THE RANKING, because the order this screen publishes is
 * the order the engine returned rather than anything the grid sorted.
 */

import { useMemo, useState } from "react";
import { useParam } from "~/app/router.tsx";
import { peekHref, rowPeekHandler, usePeek, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { Dash, KeyCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { StatusBadge } from "~/components/work.tsx";
import { Button, EmptyState, Input } from "@crewlethq/ui";
import { ScheduleGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import type { WorkRanked } from "~/protocol/index.ts";

/**
 * HOW A HIT IS ADDRESSED, in the one spelling every consumer of it uses.
 *
 * A hit carries both an id and a key, and the key is what the tracker addresses
 * an item by — so the key where there is one and the id where there is not,
 * which is the rule this screen's row link already followed alone. It is a
 * function now because FOUR things have to agree on it: the row's link, the
 * peek a plain click opens, the order `[` and `]` step through, and which row
 * is drawn as the open one. Four spellings of "which item is this" is how the
 * rail comes to highlight a different row from the one it is showing.
 */
function itemId(hit: WorkRanked): string {
  return hit.key || hit.id;
}

export function WorkSearch() {
  // THE QUERY IS IN THE URL, which is what makes a search shareable and what
  // makes the back button walk a reader's searches rather than their
  // keystrokes: it is a FILTER, so typing replaces the history entry.
  const [q, setQ] = useParam("q", "", "filter");
  const [typed, setTyped] = useState(q);
  const hits = useQuery("work_search", { q }, { enabled: q.trim() !== "" });
  usePageCoverage(undefined);

  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);

  // THE PEEK IS THE FRAME'S: the shell mounts one rail for every screen, so
  // this one opens peeks and renders none.
  const peek = usePeek();
  const { open: openPeek } = usePeekControls();

  const rows = useMemo(() => hits.data?.hits ?? [], [hits.data]);
  // WHAT `[` AND `]` WALK: the ranked order, which on this screen is the whole
  // answer — the arithmetic that produced it is finished before this grid sees
  // it and nothing here re-sorts. Stepping the rail is therefore stepping DOWN
  // THE RANKING, which is the gesture a reader working through hits makes.
  usePeekNeighbours(
    useMemo(() => rows.map((r) => ({ kind: "item" as const, id: itemId(r) })), [rows]),
  );

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
        {/* THE NAME MOVES FROM A HIDDEN `<label>` ONTO THE FIELD ITSELF.
            Their `Input` owns the box, the leading slot and the focus ring,
            and it takes no label of its own — so the accessible name is an
            `aria-label` rather than a visually hidden span this screen had to
            remember to write. Same sentence, same reader. */}
        <div style={{ flex: 1, minWidth: 0 }}>
          <Input
            type="search"
            width="full"
            aria-label="Search the company’s work"
            leading={<SearchGlyph size="sm" />}
            placeholder="A phrase — the words somebody would have written"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
          />
        </div>
        <Button variant="primary" leadingIcon={<SearchGlyph size="sm" />} type="submit">
          Search
        </Button>
      </form>

      {q.trim() === "" ? (
        <EmptyState
          icon={<SearchGlyph size="xl" />}
          title="Type what you half remember"
          description="The ranking is over titles, bodies and comments — the words somebody actually wrote, rather than a key or a status."
        />
      ) : hits.data && !hits.data.available ? (
        // NOT AN EMPTY RESULT. See the file head: this node has the items and
        // not yet the index, and a reader told "no matches" acts on it.
        <EmptyState
          icon={<ScheduleGlyph size="xl" />}
          title="This node is still indexing"
          description={
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
            // THE FRAME'S OWN ANSWER to where an item lives, rather than a
            // second copy of the route: the rail's `Open ↗` is built from the
            // same reference, so the link a row carries and the way out of the
            // panel it opens can never name different pages.
            rowHref={(r) => peekHref({ kind: "item", id: itemId(r) })}
            // A PLAIN CLICK PEEKS, because a ranked list is read by working
            // DOWN it: a reader checking whether the third hit is the one they
            // meant should not lose the other nine to find out.
            onRowActivate={(r, e) => {
              const go = () => openPeek({ kind: "item", id: itemId(r) });
              // THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the
              // frame's one copy of "which clicks mean elsewhere" and reads a
              // mouse event; the `enter` chord carries no button at all and is
              // never one of them.
              if (!("button" in e)) {
                go();
                return;
              }
              rowPeekHandler(go)?.(e);
            }}
            isSelected={(r) => peek?.kind === "item" && peek.id === itemId(r)}
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
                    {r.snippet && <span className="t-caption">{r.snippet}</span>}
                  </div>
                ),
              },
              {
                key: "key",
                header: "Key",
                shrink: true,
                // THE CELL OWNS THE ABSENCE. A hit whose key has not landed on
                // this node yet is a dash that says so on hover, rather than a
                // faint em dash of this screen's own that means whatever the
                // next screen's faint em dash means.
                cell: (r) => <KeyCell value={r.key} />,
              },
              {
                key: "project",
                header: "Project",
                shrink: true,
                // A PROJECT KEY IS AN IDENTIFIER, so it wears the mono face
                // every other key in the product wears and links to the
                // project rather than sitting in a chip. Colour is spent on
                // state here, and which project an item is in is identity.
                cell: (r) => <KeyCell value={r.project} path={["work", r.project]} />,
              },
              {
                key: "status",
                header: "Status",
                shrink: true,
                // THE TRACKER'S OWN STATUS BADGE, not a `StatusCell` and not
                // the raw slug this column used to print. Six statuses with a
                // tone each is a VOCABULARY rather than two lifecycle states,
                // and a company may rename any of them — `StatusBadge` is the
                // one place that resolves the label and the tone together, so
                // a status reads the same here as it does on the board.
                //
                // WITHOUT THE PROJECT'S OWN LABELS, deliberately: a ranked
                // answer spans every project and each may name the six
                // differently, so the shipped word is the only one true of the
                // whole list. The board, which is inside one project, passes
                // that project's definitions.
                cell: (r) =>
                  r.status ? <StatusBadge status={r.status} /> : <Dash title="no status" />,
              },
              {
                key: "assignee",
                header: "Assignee",
                shrink: true,
                // THE NAME, not the handle: a handle is the database's word
                // for a person, and every other grid in the tree resolves it
                // through the chart before showing it.
                cell: (r) => {
                  const who = r.assignee ? index.byHandle.get(r.assignee) : undefined;
                  return <SeatCell handle={r.assignee} name={who?.name} kind={who?.kind} />;
                },
              },
            ]}
            loadedNote={`${rows.length} ranked by relevance`}
          />
        </QueryState>
      )}
    </>
  );
}
