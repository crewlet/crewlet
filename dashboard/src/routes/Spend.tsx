/**
 * Spend and budgets.
 *
 * Two different facts share this screen and the previous one let them blur:
 *
 *  - the **spend rollup** is a WINDOW (24 hours by default, up to 30 days) over
 *    what was actually billed;
 *  - a **meter** is PROCESS-LIFETIME — it resets when the engine restarts.
 *
 * They are never comparable, and every number here says which it is.
 */

import { useMemo } from "react";
import { useNavigator, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Badge, Meter, Panel, Segmented, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { BarList, Legend, StackedBar, phaseColor, vizColor } from "~/ui/charts.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useOrgBudget, useTokens } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtCount, fmtDateTime, fmtExact, fmtPct, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

const WINDOWS = ["1", "7", "30"] as const;

export function Spend() {
  const nav = useNavigator();
  const pushed = useTokens();
  const orgBudget = useOrgBudget();
  const now = useNow();
  const [days, setDays] = useParam("window", "1", "section");

  // The pushed rollup covers the default window. Any other window is a query,
  // and while it loads the pushed one stays on screen rather than blanking.
  const custom = useQuery(
    "tokens",
    { since_days: Number(days), recent_turns: 100 },
    { enabled: days !== "1" },
  );
  const tokens = days === "1" ? pushed : (custom.data ?? pushed);

  const budgets = useQuery("budgets", undefined, { pollMs: 30_000 });

  const phase = useMemo(
    () =>
      (tokens?.by_phase ?? [])
        .map((p) => ({
          label: p.phase,
          value: p.total_tokens,
          display: fmtCount(p.total_tokens),
          color: phaseColor(p.phase),
          sub: `${p.calls.toLocaleString()} calls · ${fmtCount(Math.round(p.total_tokens / Math.max(1, p.calls)))} per call`,
        }))
        .sort((a, b) => b.value - a.value),
    [tokens],
  );

  const models = useMemo(
    () =>
      (tokens?.by_model ?? [])
        .slice()
        .sort((a, b) => b.total_tokens - a.total_tokens)
        .map((m, i) => ({
          label: m.model,
          value: m.total_tokens,
          display: fmtCount(m.total_tokens),
          color: vizColor(i),
          sub: `${m.calls.toLocaleString()} calls`,
        })),
    [tokens],
  );

  const phaseKeys = useMemo(
    () => [...new Set((tokens?.by_phase ?? []).map((p) => p.phase))],
    [tokens],
  );

  const org = orgBudget?.org;

  return (
    <>
      <PageActions>
        {tokens ? <Badge outline>{tokens.since_days}-day window</Badge> : undefined}
        {
          <Segmented
            ariaLabel="Window"
            value={days}
            onChange={setDays}
            options={WINDOWS.map((d) => ({ value: d, label: `${d}d` }))}
          />
        }
      </PageActions>
      <PageNote>
        What the company's model calls actually cost, and how much headroom the budget gate has
        left.
      </PageNote>

      <Panel padding="none">
        <StatRow cols={4}>
          <Stat
            icon="coin"
            label={`Tokens · ${tokens?.since_days ?? "—"}d`}
            value={tokens ? fmtCount(tokens.totals.total_tokens) : "—"}
            sub={tokens ? `${fmtExact(tokens.totals.total_tokens)} exactly` : "nothing recorded"}
          />
          <Stat
            icon="cpu"
            label="Model calls"
            value={tokens ? fmtCount(tokens.totals.calls) : "—"}
            sub={
              tokens && tokens.totals.calls
                ? `${fmtCount(Math.round(tokens.totals.total_tokens / tokens.totals.calls))} tokens per call`
                : ""
            }
          />
          <Stat
            icon="arrowRight"
            label="Input / output"
            value={
              tokens
                ? `${fmtCount(tokens.totals.input_tokens)} / ${fmtCount(tokens.totals.output_tokens)}`
                : "—"
            }
            sub="input includes any cached prefix, as the provider reports it"
          />
          <Stat
            icon="clock"
            label="Counted through"
            value={tokens?.aggregated_through ? relTime(tokens.aggregated_through, now) : "—"}
            sub={
              tokens?.aggregated_through
                ? fmtDateTime(tokens.aggregated_through)
                : "no high-water mark yet"
            }
          />
        </StatRow>
      </Panel>

      {org && org.max > 0 && (
        <Panel
          title="Company budget meter"
          icon="target"
          subtitle="process-lifetime — not the window above"
          actions={org.used >= org.max ? <Badge tone="critical">spent</Badge> : undefined}
        >
          <Meter
            fullMeans="spent"
            used={org.used}
            max={org.max}
            // A NOUN, not the legend beside it: "94% of the meter used" is
            // the reading, and a name that changes with the value is not a
            // name.
            ariaLabel="Company token budget"
            label={`${fmtPct(org.used, org.max, 1)} of the meter used`}
            right={`${fmtExact(org.used)} / ${fmtExact(org.max)}`}
            tone={org.used >= org.max ? "critical" : undefined}
          />
          {org.used >= org.max && (
            // AT THE CAP IS WHAT THE SHARED COUNTER CAN SAY. It is
            // sufficient and not necessary — a refused charge increments
            // nothing, so a company that is refusing can sit just short of
            // its cap — which is why the attention queue also warns at 90%.
            <p className="t-caption" style={{ marginTop: "var(--space-2)" }}>
              The meter is at its cap, so no further charge can be accepted and turns are being
              declined at the gate.
            </p>
          )}
        </Panel>
      )}

      <div className="grid grid-auto-lg">
        <Panel title="By phase" icon="layers" subtitle="where the tokens actually go">
          <div className="col gap-3">
            <BarList data={phase} emptyLabel="No model calls in this window." />
            {phase.length > 0 && (
              <Legend items={phase.map((p) => ({ label: p.label, color: p.color }))} />
            )}
          </div>
        </Panel>
        <Panel title="By model" icon="cpu" subtitle="from each completion's own reported model">
          <div className="col gap-3">
            <BarList data={models} limit={8} emptyLabel="No model calls in this window." />
            <p className="t-caption">
              Built from what each completion reported, never from a provider's configured name — a
              fallback chain serves several models under one key.
            </p>
          </div>
        </Panel>
      </div>

      {(tokens?.by_worker ?? []).length > 0 && (
        <Panel title="Background workers" icon="refresh" subtitle="spend outside any seat's turn">
          <BarList
            data={(tokens?.by_worker ?? []).map((w, i) => ({
              label: w.worker,
              value: w.total_tokens,
              display: fmtCount(w.total_tokens),
              color: vizColor(i),
              sub: `${w.calls} calls`,
            }))}
          />
        </Panel>
      )}

      <Panel title="By seat" icon="users" count={tokens?.by_agent?.length ?? 0} padding="none">
        <DataTable
          rows={tokens?.by_agent ?? []}
          rowKey={(a) => a.agent_id || a.role}
          defaultSort={{ key: "total", dir: "desc" }}
          onRowClick={(a) => nav.to(["seats", a.handle || a.role], { tab: "cost" })}
          empty={{ title: "No seat has spent tokens in this window" }}
          columns={[
            {
              key: "seat",
              header: "Seat",
              sortValue: (a) => a.role,
              cell: (a) => <SeatChip name={a.role} handle={a.handle} />,
            },
            {
              key: "total",
              header: "Tokens",
              align: "right",
              sortValue: (a) => a.total_tokens,
              cell: (a) => fmtExact(a.total_tokens),
            },
            {
              key: "share",
              header: "Share",
              width: "180px",
              cell: (a) => (
                <StackedBar
                  segments={phaseKeys.map((p) => ({
                    label: p,
                    value: a.by_phase?.[p]?.total_tokens ?? 0,
                    color: phaseColor(p),
                  }))}
                />
              ),
            },
            {
              key: "calls",
              header: "Calls",
              align: "right",
              sortValue: (a) => a.calls,
              cell: (a) => fmtExact(a.calls),
            },
            {
              key: "avg",
              header: "Per call",
              align: "right",
              sortValue: (a) => (a.calls ? a.total_tokens / a.calls : 0),
              cell: (a) => (a.calls ? fmtCount(Math.round(a.total_tokens / a.calls)) : "—"),
            },
          ]}
        />
        {phaseKeys.length > 0 && (
          <footer className="panel-foot">
            <Legend items={phaseKeys.map((p) => ({ label: p, color: phaseColor(p) }))} />
          </footer>
        )}
      </Panel>

      <Panel title="Recent turns" icon="layers" count={tokens?.by_turn?.length ?? 0} padding="none">
        <DataTable
          rows={tokens?.by_turn ?? []}
          rowKey={(t) => t.turn_id}
          defaultSort={{ key: "started", dir: "desc" }}
          onRowClick={(t) => nav.to(["turns", t.turn_id])}
          empty={{ title: "No turns in this window" }}
          columns={[
            {
              key: "started",
              header: "Started",
              shrink: true,
              sortValue: (t) => tsKey(t.started_at),
              cell: (t) => <span className="t-caption">{fmtDateTime(t.started_at)}</span>,
            },
            {
              key: "seat",
              header: "Seat",
              sortValue: (t) => t.role,
              cell: (t) => <SeatChip name={t.role} handle={t.handle} />,
            },
            {
              key: "total",
              header: "Tokens",
              align: "right",
              sortValue: (t) => t.total_tokens,
              cell: (t) => fmtExact(t.total_tokens),
            },
            {
              key: "calls",
              header: "Calls",
              align: "right",
              sortValue: (t) => t.calls,
              cell: (t) => t.calls,
            },
            {
              key: "turn",
              header: "Turn",
              shrink: true,
              cell: (t) => <code className="inline">{t.turn_id.slice(0, 8)}</code>,
            },
          ]}
        />
      </Panel>
    </>
  );
}
