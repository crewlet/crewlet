/**
 * Spend › Overview: what the company's model calls spent over a window of
 * company days, and where it went.
 *
 * # One window, read from one place
 *
 * Every figure here is a NAMED WINDOW of whole company days — 7, 30 or 90, or
 * two dates a reader chose — read from the replicated usage domain (ADR-0020):
 * every data node's days, so the answer is the company's whichever node serves
 * it, and it reaches back the domain's 181 days rather than the event log's 30.
 * There is no live rolling window on this screen any more. It opened on one,
 * and a "24 hours" beside a chart of company days put two windows that cannot
 * be compared on one screen; the live window is the live push's, which Home
 * and Live draw.
 *
 * So the hero, the chart, the tables and the export are all ONE window, and
 * each says which: the label is built from the ANSWER's own days, never from
 * the control, so an answer still on screen while the next loads is never
 * captioned with the window it is not.
 *
 * # The budget is a different fact, and says so
 *
 * The monthly tile and the per-seat "today" column are the budget gate's
 * CALENDAR windows (ADR-0019) — this month, today — which are not the window
 * above them. Each says its own window in words.
 *
 * # The unit is TOKENS, and there is no money on this screen
 *
 * Nor on any other: rule 19 in `docs/reference/dashboard-design.md`. A price
 * is reported by a minority of backends, and a currency beside a token count
 * covering every call reads as the company's spend and is a fraction of it.
 * `money.test.tsx` holds the source and the rendered screen to it, and
 * `internal/api/dashboardjs_test.go` the bundle the engine serves.
 */

import { useMemo } from "react";
import { useNavigator, useParam } from "~/app/router.tsx";
import { useAssistantContext } from "~/app/assistant/useAssistantContext.ts";
import { urlAction } from "~/app/assistant/urlAction.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { QueryState } from "~/components/common.tsx";
import { ADMIN_GROUPS, GROUPS } from "~/contract/spend.ts";
import { useNow } from "~/lib/clock.ts";
import { isRange, useTimeRange, windowParam } from "~/lib/range.ts";
import { useAgents, useOrg, useOrgBudget } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useServicesRun } from "~/lib/services.ts";
import { useViewer } from "~/lib/viewer.ts";
import { mayChangeConfig } from "~/lib/useWriteAccess.ts";
import { rangePickerProps } from "~/lib/rangePicker.ts";
import type { BudgetWindow } from "~/protocol/types.ts";
import { ByAgent } from "./ByAgent.tsx";
import { ByModel } from "./ByModel.tsx";
import { ByUnit } from "./ByUnit.tsx";
import { ExpensiveTasksCard } from "./ExpensiveTasks.tsx";
import { Hero } from "./Hero.tsx";
import { PhaseChart } from "./PhaseChart.tsx";
import { Workers } from "./Workers.tsx";
import { agentLines, spendCsv, spendOffer, spendWindowParams, windowWords } from "./model.ts";
import { spendGroupKeys, spendRangeKeys } from "./assistantActions.ts";
import { DownloadButton, TimeRangePicker } from "@crewlethq/ui";

/** The dimensions the chart splits on for a reader: the contract's six, less
 *  the ones only an admin is answered (`ADMIN_GROUPS`). */
function groupsFor(admin: boolean) {
  return GROUPS.filter((g) => admin || !ADMIN_GROUPS.includes(g.value));
}

export function Spend() {
  const zone = useOrg()?.timezone;
  const now = useNow();
  const range = useTimeRange(now, spendOffer(zone));
  const named = isRange(range.window);
  const params = spendWindowParams(range.window, zone);

  const viewer = useViewer();
  const groups = groupsFor(viewer.admin);
  const [rawGroup, setGroup] = useParam("group", "phase", "section");
  // A GROUP THE ENGINE WOULD REFUSE IS THE DEFAULT, not a refusal on screen:
  // a hand-edited `group=turn` (the dimension this answer no longer has) is a
  // stale link, and the chart is still worth drawing.
  const group = groups.some((g) => g.value === rawGroup) ? rawGroup : "phase";

  // --- assistant page actions (WP12) ---
  const nav = useNavigator();
  useAssistantContext({
    actions: {
      ...urlAction("spend.range.set", {
        scope: "Spend window (view only)",
        keys: spendRangeKeys(range.offer, () => now),
        nav,
      }),
      ...urlAction("spend.group.set", {
        scope: "Spend chart split (view only)",
        keys: spendGroupKeys(),
        nav,
      }),
    },
  });
  // --- end assistant page actions (WP12) ---

  const tokens = useQuery("tokens", params);
  const series = useQuery("token_series", { ...params, group, bucket: "day" });
  // THE WINDOW BEFORE, cut by the ENGINE on the company's calendar and asked
  // with a fixed grouping — only its total is read, so a change of the
  // chart's split is no reason to ask again.
  const prior = useQuery("token_series", { ...params, bucket: "day", previous: true });

  const budget = useOrgBudget();
  const agents = useAgents();
  const runs = useServicesRun();

  // A REFUSED WINDOW HAS NO FIGURES. The previous answer is still in the
  // hook while the refusal stands, and drawing it under a control that now
  // names a different window put last week's numbers — and the refusal three
  // times over, once from each question asked of the same window — beneath a
  // "Custom" that had answered nothing. The refusal is said once, at the top,
  // and nothing is drawn that is not this window's.
  const refused = tokens.error === "bad_params";
  const rollup = refused ? null : tokens.data;
  // EACH SEAT'S DAY BY ITS AGENT ID, the key the spend row carries too
  // (`agentLines`).
  const dayOf = useMemo(() => {
    const byAgent = new Map<string, BudgetWindow | undefined>();
    for (const a of agents) {
      if (a.agent_id) {
        byAgent.set(
          a.agent_id,
          a.budget?.windows?.find((w) => w.period === "day"),
        );
      }
    }
    return (agentID: string) => byAgent.get(agentID);
  }, [agents]);
  const lines = useMemo(() => agentLines(rollup, dayOf), [rollup, dayOf]);
  const words = rollup ? windowWords(rollup, named) : null;

  return (
    <>
      <PageActions>
        <TimeRangePicker {...rangePickerProps(range, "Window")} />
        <DownloadButton
          label="Export"
          filename={`crewlet-spend-${rollup?.from ?? "window"}-${rollup?.to ?? ""}.csv`}
          mime="text/csv;charset=utf-8"
          text={() => (rollup ? spendCsv(rollup, lines, `${rollup.from}..${rollup.to}`) : "")}
          disabled={!rollup}
          title={rollup ? "Download as CSV" : "Nothing to export until the window has answered"}
        />
      </PageActions>

      {/* THE REFUSAL, ABOVE THE FIGURES IT IS ABOUT — in the engine's own
          words where it has some: a window too long, or starting before the
          history, says which and what to change. */}
      {tokens.error && (
        <QueryState
          error={tokens.error}
          refusal={tokens.refusal}
          detail={tokens.detail}
          loading={false}
        />
      )}

      {!refused && (
        <div className="spend">
          <div className="spend-top">
            <Hero
              rollup={rollup}
              loading={tokens.loading}
              words={words}
              named={named}
              prior={prior}
              budget={budget}
              mayConfigure={mayChangeConfig(viewer)}
            />
            <PhaseChart
              series={series}
              group={group}
              groups={groups}
              onGroup={setGroup}
              zone={zone}
            />
          </div>
          {/* WHO SPENT IT, three ways: the seats' table beside the two ranked
              lists that break the same tokens down by model and by team. */}
          <div className="spend-pair">
            <ByAgent
              lines={lines}
              loading={tokens.loading}
              error={tokens.error}
              words={words}
              range={range.window}
            />
            <div className="spend-side">
              <ByModel rollup={rollup} loading={tokens.loading} words={words} />
              <ByUnit params={params} words={words} />
              {/* WHERE THE COMPANY RUNS NO WORKLET there are no tasks to
                  rank, and the auxiliary model's card joins this column
                  rather than standing alone in a row of its own. */}
              {!runs.tracker && <Workers rollup={rollup} loading={tokens.loading} words={words} />}
            </div>
          </div>
          {/* WHAT IT WAS SPENT ON: the tasks, in the wide track their titles
              need, beside the auxiliary model's purposes. */}
          {runs.tracker && (
            <div className="spend-pair">
              <ExpensiveTasksCard
                since={rollup?.since}
                until={rollup?.until}
                window={windowParam(range.window)}
              />
              <Workers rollup={rollup} loading={tokens.loading} words={words} />
            </div>
          )}
        </div>
      )}
    </>
  );
}
