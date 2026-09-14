/**
 * Budgets: the caps, the fleet's own counter behind them, and what is being
 * refused right now.
 *
 * # Its own screen, because it answers a different question from spend
 *
 * Spend is "what has this cost". A budget is "what will be REFUSED", and the
 * two are read at different moments by different people: a founder reads the
 * first at the end of a month and the second the instant a seat stops
 * working. Folded into one screen, the budget table sat under four charts and
 * a turn list — which is where it was.
 *
 * # The counter is the FLEET's, not this process's meter
 *
 * `durable: false` means the coordination store could not be READ, which is
 * not the same as the counter being zero. Rendering it as zero would tell a
 * person their company has spent nothing when what happened is that nobody
 * could ask.
 */

import { PageNote } from "~/app/frame/PageNote.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { Badge, Meter, Panel, Skeleton } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtCount, fmtExact } from "~/lib/format.ts";

export function Budgets() {
  const budgets = useQuery("budgets", undefined, { pollMs: 30_000 });

  return (
    <>
      <PageNote>
        What a seat is allowed to spend, and what the fleet's own durable counter says it has. A
        budget that is exhausted does not slow a seat down — it REFUSES the call, and the seat falls
        through to whatever its chain names next.
      </PageNote>

      <Panel
        title="Durable budget counters"
        icon="database"
        subtitle="the fleet's shared ledger, not this process's meter"
        padding="none"
      >
        {budgets.loading && !budgets.data && <Skeleton rows={3} />}
        {budgets.data && budgets.data.durable === false ? (
          <div className="banner neutral" style={{ margin: "var(--space-3)" }}>
            <Icon name="database" size="sm" />
            <span>
              The durable counter could not be READ — which is not the same as it being zero. It
              lives in the fleet's coordination store; this node could not reach it.
            </span>
          </div>
        ) : (
          <QueryState error={budgets.error} loading={budgets.loading}>
            <DataTable
              rows={budgets.data?.seats ?? []}
              rowKey={(s) => s.agent_id || s.role}
              defaultSort={{ key: "used", dir: "desc" }}
              empty={{ title: "No per-seat budgets are configured" }}
              columns={[
                {
                  key: "seat",
                  header: "Seat",
                  sortValue: (s) => s.role,
                  cell: (s) => <SeatChip name={s.role} handle={s.handle} />,
                },
                {
                  key: "used",
                  header: "Durable used",
                  align: "right",
                  sortValue: (s) => s.durable_used,
                  cell: (s) => fmtExact(s.durable_used),
                },
                {
                  key: "live",
                  header: "This process",
                  align: "right",
                  sortValue: (s) => s.live_used,
                  cell: (s) => fmtExact(s.live_used),
                },
                {
                  key: "max",
                  header: "Budget",
                  align: "right",
                  sortValue: (s) => s.max_tokens,
                  cell: (s) =>
                    s.max_tokens ? (
                      fmtExact(s.max_tokens)
                    ) : (
                      <span className="faint">unlimited</span>
                    ),
                },
                {
                  key: "headroom",
                  header: "Headroom",
                  width: "160px",
                  cell: (s) =>
                    s.max_tokens ? (
                      <Meter
                        fullMeans="spent"
                        used={s.durable_used}
                        max={s.max_tokens}
                        // The column heading names it for a sighted reader;
                        // a screen reader lands on the meter alone.
                        ariaLabel={`${s.role} token budget`}
                      />
                    ) : (
                      <span className="faint">—</span>
                    ),
                },
              ]}
            />
          </QueryState>
        )}
      </Panel>
    </>
  );
}
