/**
 * Every unit of work the company has done, one row each.
 *
 * # There was no list of turns anywhere
 *
 * A turn is what this engine DOES — a wake, a decision, some tool rounds, a
 * reply — and every other surface is a projection of one: the spend rollup
 * groups them, the seat page shows one seat's, an item's history links to the
 * ones that touched it. None of them is a list of them.
 *
 * This screen used to be the PHASE monitor, which is a level below: sixty rows
 * for one turn on a busy seat, so "what has the company been doing" was
 * answered by scrolling past the rounds of whatever ran last. The phase view
 * is still here, as the other lens, because "which model call is hung right
 * now" is a real question and a turn row cannot answer it.
 *
 * # The fold is the engine's, not the browser's
 *
 * The list was previously assembled client-side by paging the raw event feed
 * sixty-one times and grouping in JavaScript. That is slow, capped at whatever
 * the caller gave up on, and WRONG at the page boundary: a turn whose events
 * straddled two pages appeared twice. The engine folds it now, over promoted
 * columns rather than payloads, with a cursor on the turn's own start.
 *
 * # A turn that has not finished is not a turn that finished instantly
 *
 * `complete` says whether a completion record exists, and the duration is the
 * turn's OWN measurement rather than the span of its events — the span covers
 * the reflection pass that publishes afterwards. Rendering a running turn's
 * zero as a duration would make the busiest turns look like the cheapest.
 */

import { useParam } from "~/app/router.tsx";
import { href, useNavigator } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Badge, Button, Segmented, Skeleton } from "~/ui/primitives.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { fmtCount, fmtDateTime, fmtElapsed, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { ModelActivity } from "../company/Model.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import type { TurnRow } from "~/protocol/index.ts";

export function Turns() {
  // TWO LENSES ON ONE PATH, which is what a `view=` is for: turns are what
  // the company did, and phases are the model calls inside them. They are
  // not two destinations — a reader switching between them is asking the
  // same question at two magnifications.
  const [view, setView] = useParam("view", "turns", "filter");
  if (view === "phases") {
    return (
      <>
        <TurnLens view={view} onChange={setView} />
        <ModelActivity />
      </>
    );
  }
  return <TurnList view={view} onChange={setView} />;
}

function TurnLens({ view, onChange }: { view: string; onChange: (v: string) => void }) {
  return (
    <PageActions>
      {
        <Segmented
          ariaLabel="Turns or the phases inside them"
          value={view}
          onChange={onChange}
          options={[
            { value: "turns", label: "Turns" },
            { value: "phases", label: "Phases" },
          ]}
        />
      }
    </PageActions>
  );
}

function TurnList({ view, onChange }: { view: string; onChange: (v: string) => void }) {
  const now = useNow();
  const nav = useNavigator();
  const org = useOrg();
  // EVERY FILTER IS A FILTER, so it replaces the history entry: a reader
  // narrowing to one seat and then to the failures has walked one screen,
  // not three.
  const [role, setRole] = useParam("role", "", "filter");
  const [failed, setFailed] = useParam("failed", "", "filter");
  const list = useQuery(
    "turns",
    {
      ...(role ? { role } : {}),
      ...(failed ? { failed } : {}),
    },
    { pollMs: 20_000 },
  );
  const turns = list.data?.turns ?? [];
  const seats = (org?.roles ?? []).filter((r) => r.kind !== "human");

  return (
    <>
      <TurnLens view={view} onChange={onChange} />
      <PageNote>
        One row per unit of work — a wake, a decision, its rounds and its reply. The phase lens is
        the same window one level down, where a single turn can be sixty rows.
      </PageNote>

      <div className="row gap-2 wrap">
        <Button
          size="sm"
          variant={role ? "primary" : undefined}
          onClick={() => setRole("")}
          icon="users"
        >
          {role || "every seat"}
        </Button>
        {seats.slice(0, 8).map((seat) => (
          <Button
            key={seat.name}
            size="sm"
            variant={role === seat.name ? "primary" : undefined}
            onClick={() => setRole(role === seat.name ? "" : seat.name)}
          >
            {seat.name}
          </Button>
        ))}
        <span className="spacer" />
        <Segmented
          ariaLabel="Which turns"
          value={failed || "all"}
          onChange={(v) => setFailed(v === "all" ? "" : v)}
          options={[
            { value: "all", label: "All" },
            { value: "true", label: "Carried a failure" },
            { value: "false", label: "Clean" },
          ]}
        />
      </div>

      {list.loading && !turns.length && <Skeleton rows={6} />}
      <QueryState
        error={list.error}
        loading={list.loading}
        empty={
          turns.length
            ? undefined
            : {
                title: "No turns in this window",
                hint: "A turn is recorded when a seat is woken and does something. A company whose seats have not been triggered has none.",
              }
        }
      >
        <DataGrid<TurnRow>
          rows={turns}
          rowKey={(t) => t.turn_id}
          onRowActivate={(t) => nav.to(["activity", "turns", t.turn_id])}
          defaultSort="-started"
          columns={[
            {
              key: "started",
              header: "Started",
              shrink: true,
              sortValue: (t) => tsKey(t.started_at),
              cell: (t) => (
                <span className="t-caption" title={fmtDateTime(t.started_at)}>
                  {relTime(t.started_at, now)}
                </span>
              ),
            },
            {
              key: "seat",
              header: "Seat",
              shrink: true,
              sortValue: (t) => t.role ?? "",
              cell: (t) =>
                t.role ? (
                  <SeatChip name={t.role} handle={t.role} />
                ) : (
                  <span className="faint">the engine</span>
                ),
            },
            {
              key: "summary",
              header: "What it did",
              sortValue: (t) => t.summary ?? "",
              cell: (t) => (
                <span className="row gap-1">
                  <span className="truncate">
                    {t.summary || <span className="faint">no summary recorded</span>}
                  </span>
                  {t.task_id && (
                    <a className="t-link mono t-caption" href={href(["work", t.task_id])}>
                      {t.task_id}
                    </a>
                  )}
                </span>
              ),
            },
            {
              key: "state",
              header: "",
              shrink: true,
              cell: (t) => (
                <span className="row gap-1">
                  {!t.complete && (
                    <Badge
                      tone="info"
                      title="no completion record — running, or it died mid-flight"
                    >
                      running
                    </Badge>
                  )}
                  {t.failed && (
                    <Badge tone="caution" title="at least one event of this turn was a failure">
                      failure
                    </Badge>
                  )}
                </span>
              ),
            },
            {
              key: "rounds",
              header: "Rounds",
              shrink: true,
              align: "right",
              sortValue: (t) => t.rounds,
              cell: (t) => <span className="mono t-caption">{t.rounds || "—"}</span>,
            },
            {
              key: "phases",
              header: "Phases",
              shrink: true,
              align: "right",
              sortValue: (t) => t.phases,
              cell: (t) => <span className="mono t-caption">{t.phases}</span>,
            },
            {
              key: "tokens",
              header: "Tokens",
              shrink: true,
              align: "right",
              sortValue: (t) => t.total_tokens,
              cell: (t) => <span className="mono t-caption">{fmtCount(t.total_tokens)}</span>,
            },
            {
              key: "took",
              header: "Took",
              shrink: true,
              align: "right",
              sortValue: (t) => t.duration_ms,
              cell: (t) =>
                // A RUNNING TURN HAS NO DURATION, and rendering its zero
                // would make the busiest turns look like the cheapest.
                t.complete && t.duration_ms > 0 ? (
                  <span className="t-caption">{fmtElapsed(t.duration_ms)}</span>
                ) : (
                  <span className="faint">—</span>
                ),
            },
          ]}
        />
      </QueryState>
    </>
  );
}
