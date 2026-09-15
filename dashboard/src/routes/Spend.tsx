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
import { useNavigator, useParam } from "~/app/router.tsx";
import { QueryState, RecordTable, SeatChip } from "~/components/common.tsx";
import { useOrgBudget, useTokens } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtCount, fmtDateTime, fmtExact, fmtPct, tsKey } from "~/lib/format.ts";
import { phaseColor } from "~/lib/phases.ts";
import {
  BarList,
  Callout,
  Card,
  dataColor,
  EmptyState,
  EmptyValue,
  InlineCode,
  Legend,
  Meter,
  PageHeader,
  RelativeTime,
  SegmentedControl,
  Skeleton,
  StackedBar,
  StatCard,
  StatGroup,
  TabPanel,
  Tag,
  useNow,
} from "@crewlethq/ui";
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
          id: p.phase,
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
          id: m.model,
          label: m.model,
          value: m.total_tokens,
          display: fmtCount(m.total_tokens),
          color: dataColor(i),
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
      <PageHeader
        title="Spend & budgets"
        description="What the company's model calls actually cost, and how much headroom the budget gate has left."
        badges={tokens ? <Tag appearance="outline">{tokens.since_days}-day window</Tag> : undefined}
        actions={
          <SegmentedControl
            label="Window"
            semantics="tabs"
            panelId={panel}
            value={days}
            onValueChange={setDays}
            options={WINDOWS.map((d) => ({ value: d, label: `${d}d` }))}
          />
        }
      />

      <TabPanel id={panel} value={days}>
        <StatGroup columns={4}>
          <StatCard
            icon={<TokenGlyph />}
            label={tokens ? `Tokens · ${tokens.since_days}d` : "Tokens"}
            loading={!tokens}
            loadingLabel="Loading the token total"
            value={tokens ? fmtCount(tokens.totals.total_tokens) : null}
            sub={tokens ? `${fmtExact(tokens.totals.total_tokens)} exactly` : "nothing recorded"}
          />
          <StatCard
            icon={<MemoryGlyph />}
            label="Model calls"
            loading={!tokens}
            loadingLabel="Loading the call count"
            value={tokens ? fmtCount(tokens.totals.calls) : null}
            sub={
              tokens && tokens.totals.calls
                ? `${fmtCount(Math.round(tokens.totals.total_tokens / tokens.totals.calls))} tokens per call`
                : ""
            }
          />
          <StatCard
            icon={<ArrowForwardGlyph />}
            label="Input / output"
            loading={!tokens}
            loadingLabel="Loading the input and output split"
            value={
              tokens
                ? `${fmtCount(tokens.totals.input_tokens)} / ${fmtCount(tokens.totals.output_tokens)}`
                : null
            }
            sub="input includes any cached prefix, as the provider reports it"
          />
          <StatCard
            icon={<ScheduleGlyph />}
            label="Counted through"
            value={
              tokens?.aggregated_through ? (
                <RelativeTime value={tokens.aggregated_through} now={now} />
              ) : (
                <EmptyValue label="Not reported yet" />
              )
            }
            sub={
              tokens?.aggregated_through
                ? fmtDateTime(tokens.aggregated_through)
                : "no high-water mark yet"
            }
          />
        </StatGroup>

        {org && org.max > 0 && (
          <Card as="section">
            <Card.Header
              icon={<TargetGlyph size="sm" />}
              subtitle="process-lifetime, not the window above"
              actions={org.refused_at ? <Tag variant="danger">refusing charges</Tag> : undefined}
            >
              <Card.Title>Company budget meter</Card.Title>
            </Card.Header>
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
          </Card>
        )}

        <div className="grid grid-auto-lg">
          <Card as="section">
            <Card.Header icon={<LayersGlyph size="sm" />} subtitle="where the tokens actually go">
              <Card.Title>By phase</Card.Title>
            </Card.Header>
            <div className="col gap-3">
              <BarList data={phase} emptyLabel="No model calls in this window." />
              {phase.length > 0 && (
                <Legend items={phase.map((p) => ({ id: p.id, label: p.label, color: p.color }))} />
              )}
            </div>
          </Card>
          <Card as="section">
            <Card.Header
              icon={<MemoryGlyph size="sm" />}
              subtitle="from each completion's own reported model"
            >
              <Card.Title>By model</Card.Title>
            </Card.Header>
            <div className="col gap-3">
              <BarList data={models} limit={8} emptyLabel="No model calls in this window." />
              <p className="t-caption">
                Built from what each completion reported, never from a provider's configured name —
                a fallback chain serves several models under one key.
              </p>
            </div>
          </Card>
        </div>

        {(tokens?.by_worker ?? []).length > 0 && (
          <Card as="section">
            <Card.Header
              icon={<AutorenewGlyph size="sm" />}
              subtitle="spend outside any seat's turn"
            >
              <Card.Title>Background workers</Card.Title>
            </Card.Header>
            <BarList
              data={(tokens?.by_worker ?? []).map((w, i) => ({
                id: w.worker,
                label: w.worker,
                value: w.total_tokens,
                display: fmtCount(w.total_tokens),
                color: dataColor(i),
                sub: `${w.calls} calls`,
              }))}
            />
          </Card>
        )}

        <Card as="section" padding="none">
          <Card.Header
            divided
            style={{ paddingInline: "var(--spacing-4)", paddingTop: "var(--spacing-3)" }}
            icon={<GroupGlyph size="sm" />}
            count={tokens?.by_agent?.length ?? 0}
          >
            <Card.Title>By seat</Card.Title>
          </Card.Header>
          <RecordTable
            screen="spend"
            table="seats"
            rows={tokens?.by_agent ?? []}
            getRowKey={(a) => a.agent_id || a.role}
            defaultSort={{ key: "total", direction: "desc" }}
            onRowClick={(a) => nav.to(["seats", a.handle || a.role], { tab: "cost" })}
            emptyMessage={
              <EmptyState
                size="compact"
                title="No seat has spent tokens in this window"
                description="Spend is recorded when a completion returns. Widen the window, or wait for a turn to run."
              />
            }
            columns={[
              {
                key: "seat",
                header: "Seat",
                sortable: true,
                // WHOSE SPEND. Hidden, every number in the row is money nobody
                // is accountable for.
                hideable: false,
                sortValue: (a) => a.role,
                render: (a) => <SeatChip name={a.role} handle={a.handle} />,
              },
              {
                key: "total",
                header: "Tokens",
                align: "right",
                firstDirection: "desc",
                sortable: true,
                sortValue: (a) => a.total_tokens,
                render: (a) => fmtExact(a.total_tokens),
              },
              {
                key: "share",
                header: "Share",
                width: "180px",
                render: (a) => (
                  <StackedBar
                    segments={phaseKeys.map((p) => ({
                      id: p,
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
                firstDirection: "desc",
                sortable: true,
                sortValue: (a) => a.calls,
                render: (a) => fmtExact(a.calls),
              },
              {
                key: "avg",
                header: "Per call",
                align: "right",
                firstDirection: "desc",
                sortable: true,
                sortValue: (a) => (a.calls ? a.total_tokens / a.calls : 0),
                render: (a) =>
                  a.calls ? (
                    fmtCount(Math.round(a.total_tokens / a.calls))
                  ) : (
                    <EmptyValue label="No calls in this window" />
                  ),
              },
            ]}
          />
          {phaseKeys.length > 0 && (
            <Card.Footer
              variant="meta"
              style={{ paddingInline: "var(--spacing-4)", paddingBottom: "var(--spacing-3)" }}
            >
              <Legend items={phaseKeys.map((p) => ({ id: p, label: p, color: phaseColor(p) }))} />
            </Card.Footer>
          )}
        </Card>

        <Card as="section" padding="none">
          <Card.Header
            divided
            style={{ paddingInline: "var(--spacing-4)", paddingTop: "var(--spacing-3)" }}
            icon={<LayersGlyph size="sm" />}
            count={tokens?.by_turn?.length ?? 0}
          >
            <Card.Title>Recent turns</Card.Title>
          </Card.Header>
          <RecordTable
            screen="spend"
            table="turns"
            rows={tokens?.by_turn ?? []}
            getRowKey={(t) => t.turn_id}
            defaultSort={{ key: "started", direction: "desc" }}
            onRowClick={(t) => nav.to(["turns", t.turn_id])}
            emptyMessage={
              <EmptyState
                size="compact"
                title="No turns in this window"
                description="A turn is recorded when it completes. Widen the window to reach older ones."
              />
            }
            columns={[
              {
                key: "started",
                header: "Started",
                shrink: true,
                sortable: true,
                firstDirection: "desc",
                // WHEN is what tells two turns of one seat apart, and the row
                // opens the turn, so it stays.
                hideable: false,
                sortValue: (t) => tsKey(t.started_at),
                render: (t) => <span className="t-caption">{fmtDateTime(t.started_at)}</span>,
              },
              {
                key: "seat",
                header: "Seat",
                sortable: true,
                sortValue: (t) => t.role,
                render: (t) => <SeatChip name={t.role} handle={t.handle} />,
              },
              {
                key: "total",
                header: "Tokens",
                align: "right",
                firstDirection: "desc",
                sortable: true,
                sortValue: (t) => t.total_tokens,
                render: (t) => fmtExact(t.total_tokens),
              },
              {
                key: "calls",
                header: "Calls",
                align: "right",
                firstDirection: "desc",
                sortable: true,
                sortValue: (t) => t.calls,
                render: (t) => t.calls,
              },
              {
                key: "turn",
                header: "Turn",
                shrink: true,
                render: (t) => <InlineCode>{t.turn_id.slice(0, 8)}</InlineCode>,
              },
            ]}
          />
        </Card>

        <Card as="section" padding="none">
          <Card.Header
            divided
            style={{ paddingInline: "var(--spacing-4)", paddingTop: "var(--spacing-3)" }}
            icon={<DatabaseGlyph size="sm" />}
            subtitle="the fleet's shared ledger, not this process's meter"
          >
            <Card.Title>Durable budget counters</Card.Title>
          </Card.Header>
          {budgets.loading && !budgets.data && (
            <Skeleton label="Loading the durable budget counters" variant="text" rows={3} />
          )}
          {budgets.data && budgets.data.durable === false ? (
            <Callout
              variant="neutral"
              style={{ margin: "var(--spacing-3)" }}
              icon={<DatabaseGlyph size="sm" />}
            >
              <span>
                The durable counter could not be READ — which is not the same as it being zero. It
                lives in the fleet's coordination store; this node could not reach it.
              </span>
            </Callout>
          ) : (
            <QueryState error={budgets.error} loading={budgets.loading}>
              <RecordTable
                screen="spend"
                table="budgets"
                rows={budgets.data?.seats ?? []}
                getRowKey={(s) => s.agent_id || s.role}
                defaultSort={{ key: "used", direction: "desc" }}
                emptyMessage={
                  <EmptyState
                    size="compact"
                    title="No per-seat budgets are configured"
                    description="A role takes one from the company configuration. Without its own cap it draws on the organization's."
                  />
                }
                columns={[
                  {
                    key: "seat",
                    header: "Seat",
                    sortable: true,
                    hideable: false,
                    sortValue: (s) => s.role,
                    render: (s) => <SeatChip name={s.role} handle={s.handle} />,
                  },
                  {
                    key: "used",
                    header: "Durable used",
                    align: "right",
                    firstDirection: "desc",
                    sortable: true,
                    sortValue: (s) => s.durable_used,
                    render: (s) => fmtExact(s.durable_used),
                  },
                  {
                    key: "live",
                    header: "This process",
                    align: "right",
                    firstDirection: "desc",
                    // AN ABSENT METER SORTS BELOW EVERY MEASUREMENT, zero
                    // included, in BOTH directions: a seat this node holds no
                    // meter for has not spent nothing, it has not been
                    // measured. The table's own comparator keeps that rule for
                    // an absent value, which is why null is the honest answer
                    // here and the -1 that stood in for it is gone.
                    sortable: true,
                    sortValue: (s) => s.live_used ?? null,
                    render: (s) => fmtExact(s.live_used),
                  },
                  {
                    key: "max",
                    header: "Budget",
                    align: "right",
                    firstDirection: "desc",
                    sortable: true,
                    sortValue: (s) => s.max_tokens,
                    render: (s) =>
                      s.max_tokens ? (
                        fmtExact(s.max_tokens)
                      ) : (
                        <span className="muted">unlimited</span>
                      ),
                  },
                  {
                    key: "headroom",
                    header: "Headroom",
                    width: "160px",
                    render: (s) =>
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
                        <EmptyValue label="No budget set" />
                      ),
                  },
                ]}
              />
            </QueryState>
          )}
        </Card>
      </TabPanel>
    </>
  );
}
