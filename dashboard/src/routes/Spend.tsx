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

import { useId, useMemo } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { useNavigator, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Badge, Panel, Segmented, Stat, StatRow, TabPanel } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { BarList, Legend, StackedBar, phaseColor, vizColor } from "~/ui/charts.tsx";
import { useOrgBudget, useTokens } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtCount, fmtDateTime, fmtExact, fmtPct, tsKey } from "~/lib/format.ts";
import { Meter, RelativeTime, Skeleton, useNow } from "@crewlethq/ui";
import {
  ArrowForwardGlyph,
  AutorenewGlyph,
  DatabaseGlyph,
  GroupGlyph,
  LayersGlyph,
  MemoryGlyph,
  ScheduleGlyph,
  TargetGlyph,
  TokenGlyph,
} from "@crewlethq/icons/glyphs";

const WINDOWS = ["1", "7", "30"] as const;

export function Spend() {
  const nav = useNavigator();
  const pushed = useTokens();
  const orgBudget = useOrgBudget();
  const now = useNow();
  const [days, setDays] = useParam("window", "1", "section");
  const panel = useId();

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
      <ScreenHead
        title="Spend & budgets"
        sub="What the company's model calls actually cost, and how much headroom the budget gate has left."
        badges={tokens ? <Badge outline>{tokens.since_days}-day window</Badge> : undefined}
        actions={
          <Segmented
            ariaLabel="Window"
            semantics="tabs"
            panelId={panel}
            value={days}
            onChange={setDays}
            options={WINDOWS.map((d) => ({ value: d, label: `${d}d` }))}
          />
        }
      />

      <TabPanel id={panel} value={days}>
        <Panel padding="none">
          <StatRow cols={4}>
            <Stat
              icon={TokenGlyph}
              label={`Tokens · ${tokens?.since_days ?? "—"}d`}
              value={tokens ? fmtCount(tokens.totals.total_tokens) : "—"}
              sub={tokens ? `${fmtExact(tokens.totals.total_tokens)} exactly` : "nothing recorded"}
            />
            <Stat
              icon={MemoryGlyph}
              label="Model calls"
              value={tokens ? fmtCount(tokens.totals.calls) : "—"}
              sub={
                tokens && tokens.totals.calls
                  ? `${fmtCount(Math.round(tokens.totals.total_tokens / tokens.totals.calls))} tokens per call`
                  : ""
              }
            />
            <Stat
              icon={ArrowForwardGlyph}
              label="Input / output"
              value={
                tokens
                  ? `${fmtCount(tokens.totals.input_tokens)} / ${fmtCount(tokens.totals.output_tokens)}`
                  : "—"
              }
              sub="input includes any cached prefix, as the provider reports it"
            />
            <Stat
              icon={ScheduleGlyph}
              label="Counted through"
              value={
                tokens?.aggregated_through ? (
                  <RelativeTime value={tokens.aggregated_through} now={now} />
                ) : (
                  "—"
                )
              }
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
            icon={TargetGlyph}
            subtitle="process-lifetime — not the window above"
            actions={org.refused_at ? <Badge tone="critical">refusing charges</Badge> : undefined}
          >
            <Meter
              value={org.used}
              max={org.max}
              label={`${fmtPct(org.used, org.max, 1)} of the meter used`}
              valueText={`${fmtExact(org.used)} / ${fmtExact(org.max)}`}
              tone={org.refused_at ? "danger" : undefined}
            />
            {org.refused_at && (
              <p className="t-caption" style={{ marginTop: "var(--spacing-2)" }}>
                Turns are being declined at the budget gate. Last refusal{" "}
                {fmtDateTime(org.refused_at)}.
              </p>
            )}
          </Panel>
        )}

        <div className="grid grid-auto-lg">
          <Panel title="By phase" icon={LayersGlyph} subtitle="where the tokens actually go">
            <div className="col gap-3">
              <BarList data={phase} emptyLabel="No model calls in this window." />
              {phase.length > 0 && (
                <Legend items={phase.map((p) => ({ label: p.label, color: p.color }))} />
              )}
            </div>
          </Panel>
          <Panel
            title="By model"
            icon={MemoryGlyph}
            subtitle="from each completion's own reported model"
          >
            <div className="col gap-3">
              <BarList data={models} limit={8} emptyLabel="No model calls in this window." />
              <p className="t-caption">
                Built from what each completion reported, never from a provider's configured name —
                a fallback chain serves several models under one key.
              </p>
            </div>
          </Panel>
        </div>

        {(tokens?.by_worker ?? []).length > 0 && (
          <Panel
            title="Background workers"
            icon={AutorenewGlyph}
            subtitle="spend outside any seat's turn"
          >
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

        <Panel
          title="By seat"
          icon={GroupGlyph}
          count={tokens?.by_agent?.length ?? 0}
          padding="none"
        >
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

        <Panel
          title="Recent turns"
          icon={LayersGlyph}
          count={tokens?.by_turn?.length ?? 0}
          padding="none"
        >
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

        <Panel
          title="Durable budget counters"
          icon={DatabaseGlyph}
          subtitle="the fleet's shared ledger, not this process's meter"
          padding="none"
        >
          {budgets.loading && !budgets.data && (
            <Skeleton label="Loading the durable budget counters" variant="text" rows={3} />
          )}
          {budgets.data && budgets.data.durable === false ? (
            <div className="banner neutral" style={{ margin: "var(--spacing-3)" }}>
              <DatabaseGlyph size="sm" />
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
                    // AN ABSENT METER SORTS BELOW EVERY MEASUREMENT, zero
                    // included. Handed to the comparator as null it was
                    // compared as the word "null", which put every seat this
                    // node has no meter for above the largest spend.
                    sortValue: (s) => s.live_used ?? -1,
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
                          size="compact"
                          value={s.durable_used}
                          max={s.max_tokens}
                          label={`${s.role} budget`}
                          hideLabel
                          valueText={`${fmtExact(s.durable_used)} / ${fmtExact(s.max_tokens)}`}
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
      </TabPanel>
    </>
  );
}
