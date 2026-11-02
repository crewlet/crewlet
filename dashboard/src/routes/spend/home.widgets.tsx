/**
 * Spend's widgets: the window's tokens, the monthly budget, the seats near a
 * limit, and where the tokens went, drawn by Spend's own cards.
 *
 * HOME'S WINDOW, NOT SPEND'S. A figure over time follows the range control at
 * the head of Home (Today, 7 days, 30 days), so every widget on the page is
 * read over the same days. The budget widgets are calendar windows of their
 * own (this month, today), as they are on Spend, and say so.
 *
 * READ ONLY. A ceiling is raised on Budgets, never from a widget.
 */

import { useMemo } from "react";
import { List, ListItem, Meter } from "@crewlethq/ui";
import {
  CoinsGlyph,
  CpuGlyph,
  LayersGlyph,
  SlidersVerticalGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { BUDGET_WINDOWS } from "~/contract/config.ts";
import { PERIOD_ADJECTIVE, windowOf } from "~/lib/budget.ts";
import { fmtCount, fmtExact } from "~/lib/format.ts";
import { TODAY } from "~/lib/range.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { useOrgBudget } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { mayChangeConfig } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import { BudgetTile } from "~/routes/spend/BudgetTile.tsx";
import { ByModel } from "~/routes/spend/ByModel.tsx";
import { ExpensiveTasksCard } from "~/routes/spend/ExpensiveTasks.tsx";
import { Hero } from "~/routes/spend/Hero.tsx";
import { windowWords } from "~/routes/spend/model.ts";
import type { BudgetWindow } from "~/protocol/types.ts";
import { WidgetCard, WidgetQuery, defineWidgets, useHomeRange } from "~/routes/home/board/kit.tsx";

/** As Company › Budgets re-reads the counters: the gate reports every 15s. */
const BUDGETS_POLL_MS = 30_000;

/** The window's spend, over Home's range, as Spend's overview asks it. */
function useHomeWindowSpend() {
  const range = useHomeRange();
  const tokens = useQuery("tokens", { days: range.days });
  const rollup = tokens.error === "bad_params" ? null : tokens.data;
  return {
    range,
    tokens,
    rollup,
    words: rollup ? windowWords(rollup, true) : null,
  };
}

function Overview() {
  const { range, tokens, rollup, words } = useHomeWindowSpend();
  const prior = useQuery("token_series", { days: range.days, bucket: "day", previous: true });
  const budget = useOrgBudget();
  const viewer = useViewer();
  if (tokens.error) {
    return (
      <WidgetCard title="Token spend" from="settings-spend">
        <div className="home-widget-pad">
          <WidgetQuery
            error={tokens.error}
            refusal={tokens.refusal}
            detail={tokens.detail}
            loading={false}
            gaps={[]}
          />
        </div>
      </WidgetCard>
    );
  }
  return (
    <Hero
      rollup={rollup}
      loading={tokens.loading}
      words={words}
      named
      prior={prior}
      budget={budget}
      mayConfigure={mayChangeConfig(viewer)}
    />
  );
}

function MonthlyBudget() {
  const budget = useOrgBudget();
  const viewer = useViewer();
  return (
    <WidgetCard title="Monthly budget" from="budgets">
      <div className="home-widget-pad">
        <BudgetTile budget={budget} mayConfigure={mayChangeConfig(viewer)} />
      </div>
    </WidgetCard>
  );
}

/** A seat's tightest window: the one the engine calls refusing, else near. */
function tightest(windows: readonly BudgetWindow[]): BudgetWindow | undefined {
  const capped = BUDGET_WINDOWS.flatMap(({ period }) => {
    const w = windowOf(windows, period);
    return w && w.limit !== undefined && w.state !== "ok" ? [w] : [];
  });
  return capped.find((w) => w.state === "refusing") ?? capped[0];
}

function NearLimit() {
  const budgets = useQuery("budgets", undefined, { pollMs: BUDGETS_POLL_MS });
  const seatBadge = useSeatBadgeOf();
  const answer = budgets.data;
  const rows = useMemo(
    () =>
      (answer?.seats ?? []).flatMap((s) => {
        const w = tightest(s.windows);
        return w ? [{ seat: s, w }] : [];
      }),
    [answer],
  );
  const unread = answer !== null && answer.durable === false;
  return (
    <WidgetCard title="Seats near a limit" from="budgets">
      <WidgetQuery
        error={budgets.error}
        refusal={budgets.refusal}
        loading={budgets.loading && !answer}
        // A COUNTER THAT COULD NOT BE READ IS THE ANSWER'S OWN `durable`,
        // said as the empty state above rather than as a gap line.
        gaps={[]}
        empty={
          unread
            ? { title: "Couldn’t read current spend", hint: "Try again shortly." }
            : answer && rows.length === 0
              ? {
                  title: "Every seat is within its limits",
                  hint: "Seats appear here as they near a daily, weekly or monthly limit.",
                }
              : undefined
        }
      >
        <div className="home-widget-pad">
          <List variant="plain">
            {rows.map(({ seat, w }) => (
              <ListItem key={seat.agent_id || seat.role}>
                <Meter
                  value={w.used}
                  max={w.limit ?? w.used}
                  state={w.state}
                  label={`${seatBadge(seat.role).name}, ${PERIOD_ADJECTIVE[w.period]}`}
                  valueText={`${fmtExact(w.used)} of ${fmtExact(w.limit)} tokens, ${
                    w.state === "refusing" ? "refusing charges" : "nearly spent"
                  }`}
                  hint={`${fmtCount(w.used)} of ${fmtCount(w.limit)}`}
                />
              </ListItem>
            ))}
          </List>
        </div>
      </WidgetQuery>
    </WidgetCard>
  );
}

function SpendByModel() {
  const { tokens, rollup, words } = useHomeWindowSpend();
  return <ByModel rollup={rollup} loading={tokens.loading} words={words} />;
}

function ExpensiveTasks() {
  const { range, rollup } = useHomeWindowSpend();
  return (
    <ExpensiveTasksCard
      since={rollup?.since}
      until={rollup?.until}
      window={range.value === "today" ? TODAY : range.value}
    />
  );
}

export const widgets = defineWidgets([
  {
    key: "settings.spend",
    from: "settings-spend",
    label: "Token spend",
    description: "Tokens used over Home's range",
    icon: CoinsGlyph,
    hue: "amber",
    size: { w: 6, minW: 4, minH: 10, vertical: false },
    once: true,
    Component: Overview,
  },
  {
    key: "budgets.month",
    from: "budgets",
    label: "Monthly budget",
    description: "This month's tokens against the limit",
    icon: SlidersVerticalGlyph,
    hue: "green",
    size: { w: 4, minW: 3, minH: 6, vertical: false },
    once: true,
    Component: MonthlyBudget,
  },
  {
    key: "budgets.near-limit",
    from: "budgets",
    label: "Seats near a limit",
    description: "Seats close to or past a token limit",
    icon: TriangleAlertGlyph,
    hue: "rose",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
    once: true,
    Component: NearLimit,
  },
  {
    key: "settings.spend-by-model",
    from: "settings-spend",
    label: "Spend by model",
    description: "Tokens by model provider",
    icon: CpuGlyph,
    hue: "blue",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: SpendByModel,
  },
  {
    key: "settings.expensive-tasks",
    from: "settings-spend-tasks",
    label: "Most expensive tasks",
    description: "The tasks that used the most tokens",
    icon: LayersGlyph,
    hue: "purple",
    size: { w: 6, minW: 4, minH: 8, vertical: true },
    once: true,
    Component: ExpensiveTasks,
  },
]);
