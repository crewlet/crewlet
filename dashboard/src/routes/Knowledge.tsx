/**
 * Knowledge — what the company knows, and what each seat has learned.
 *
 * Two halves, and the split is the architecture rather than a layout choice:
 *
 *  - **The knowledge base** is searched LIVE, at query time, through the
 *    engine's own `knowledge.Searcher` seam. There is no local copy, no sync
 *    worker and no index to keep fresh — which is exactly why this screen has
 *    a search box and not a browsable tree. Search is BEST EFFORT by contract:
 *    every failure path is an empty result, so this screen has to say when it
 *    got one rather than drawing silence as "nothing found".
 *  - **What a seat learned** is per-agent and private: its diary, its
 *    episodes, the skills it drafted for itself. That lives on the seat.
 */

import { useState } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, Section } from "~/components/common.tsx";
import { Badge, Button, Empty, Panel, SearchInput, Skeleton } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useMemo } from "react";

export function Knowledge() {
  const org = useOrg();
  const [q, setQ] = useParam("q", "");
  const [draft, setDraft] = useState(q);
  const index = useMemo(() => indexOrg(org), [org]);

  // Searching is a real request against a real wiki, so it runs on submit
  // rather than on every keystroke: a per-character search would put one
  // request per letter through the company's own credentials.
  const { data, loading, error } = useQuery("knowledge", { q }, { enabled: q.trim().length > 0 });

  return (
    <>
      <ScreenHead
        title="Knowledge"
        sub="The company knowledge base, searched live the way an agent searches it — there is no local copy, so what you see here is what the backend holds right now."
        badges={data?.backend ? <Badge outline>{data.backend}</Badge> : undefined}
      />

      <form
        className="toolbar"
        onSubmit={(e) => {
          e.preventDefault();
          setQ(draft.trim());
        }}
      >
        <div style={{ flex: 1, maxWidth: 520 }}>
          <SearchInput
            value={draft}
            onChange={setDraft}
            ariaLabel="Search the knowledge base"
            placeholder="Search the knowledge base — plain text, not a query language"
          />
        </div>
        <Button variant="primary" type="submit" icon="search">
          Search
        </Button>
        {q && (
          <Button
            icon="x"
            onClick={() => {
              setDraft("");
              setQ("");
            }}
          >
            Clear
          </Button>
        )}
      </form>

      {!q && (
        <Empty
          icon="book"
          title="Search the company's shared knowledge"
          hint="The engine runs this against the configured knowledge backend at query time — the same live search an agent's Plan phase does before it starts work. Nothing is cached here, so there is no staleness window."
        />
      )}

      {loading && <Skeleton rows={4} />}

      {/* The search DID NOT RUN. `available: false` covers three states — no
          company, no backend, a backend with no org-wide read scope — so the
          engine's `note` is rendered rather than restated, and the REMEDY is
          chosen off `reason`. Neither is guessed from `backend`: it is empty
          for "no backend" and "no company" alike, and telling somebody with
          no company configured to go and wire Confluence is the wrong fix. */}
      {q && data?.available === false && (
        <div className="banner neutral">
          <Icon name="book" size="sm" />
          <span>
            This search could not run: {data.note || "the engine gave no reason"}.
            {data.reason === "no_backend" && (
              <>
                {" "}
                Wire <code className="inline">integrations.confluence</code> and list the spaces to
                read in <code className="inline">knowledge.confluence_spaces</code> to give agents a
                shared place to read from.
              </>
            )}
            {data.reason === "no_scope" && (
              <>
                {" "}
                The integration itself is fine — add the spaces to search to{" "}
                <code className="inline">knowledge.confluence_spaces</code>.
              </>
            )}
          </span>
        </div>
      )}

      {/* The search DID run and came back degraded — a different banner,
          because an empty result that ran is not the same fact as one that
          never started. */}
      {q && data?.available !== false && data?.note && (
        <div className="banner caution">
          <Icon name="alert" size="sm" />
          <span>
            The search did not complete: {data.note}. Knowledge search is best effort by design — a
            turn never dies because a wiki was slow — so an empty result here is not proof that
            nothing matches.
          </span>
        </div>
      )}

      {q && (
        <QueryState
          error={error}
          loading={loading}
          empty={
            data?.hits?.length
              ? undefined
              : data?.available === false || data?.note
                ? undefined
                : {
                    title: `Nothing matched “${q}”`,
                    hint: "This is the backend's own answer, taken just now.",
                  }
          }
        >
          <Panel title="Results" icon="search" count={data?.hits?.length ?? 0} padding="none">
            <div className="list">
              {(data?.hits ?? []).map((hit) => (
                <div key={hit.id} className="hit">
                  <a className="hit-title" href={hit.url} target="_blank" rel="noreferrer">
                    {hit.title} <Icon name="external" size="xs" style={{ display: "inline" }} />
                  </a>
                  <div className="row gap-1">
                    {hit.container && <Badge outline>{hit.container}</Badge>}
                    {hit.updated_at && (
                      <span className="t-caption">updated {fmtDateTime(hit.updated_at)}</span>
                    )}
                  </div>
                  {hit.snippet && <p className="hit-snippet">{hit.snippet}</p>}
                </div>
              ))}
            </div>
            <footer className="panel-foot">
              A snippet is capped by contract — it exists to say WHICH page to read, not to be the
              page.
            </footer>
          </Panel>
        </QueryState>
      )}

      <Section
        title="What each seat has learned for itself"
        hint="private to the seat — its diary, its past turns, the skills it drafted"
      >
        <div className="grid grid-auto">
          {index.seats
            .filter((s) => s.kind === "agent")
            .slice(0, 12)
            .map((seat) => (
              <a
                key={seat.handle}
                className="seat-card"
                href={href(["seats", seat.handle], { tab: "memory" })}
              >
                <div className="row">
                  <span className="attention-icon" data-severity="info">
                    <Icon name="database" size="sm" />
                  </span>
                  <span className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
                    <strong className="truncate t-cell">{seat.name}</strong>
                    <span className="truncate t-caption mono">@{seat.handle}</span>
                  </span>
                  <Icon name="arrowRight" size="sm" />
                </div>
                <span className="t-caption truncate">
                  {seat.goal || "memory, episodes and skills"}
                </span>
              </a>
            ))}
        </div>
      </Section>
    </>
  );
}
