/**
 * Home's own parts: the page's head, and each of the cards Home has always
 * drawn, by the id the reader's board stores them under.
 *
 * A CARD IS THE SHIPPED CARD, never a copy. Every one renders its module in
 * this folder, fed by the page's one `HomeData`, so a board that places them
 * in any order draws exactly what Home drew before it could be arranged.
 */

import type { ReactNode } from "react";
import { SegmentedControl } from "@crewlethq/ui";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useWorkingNow } from "~/app/Shell.tsx";
import { NewTaskButton } from "~/components/NewTaskButton.tsx";
import type { HomeData } from "./useHomeData.ts";
import { HOME_RANGES, greeting } from "./model.ts";
import { KPI_IDS, Kpis, kpisShown, type KpiId } from "./Kpis.tsx";
import { useServicesRun } from "~/lib/services.ts";
import { Decisions } from "./Decisions.tsx";
import { CodeWaiting } from "./CodeWaiting.tsx";
import { LiveNow } from "./LiveNow.tsx";
import { CompletedChart } from "./CompletedChart.tsx";
import { TokensByTeam } from "./TokensByTeam.tsx";
import { Projects } from "./Projects.tsx";
import { Feed } from "./Feed.tsx";
import { PinnedShelf } from "./PinnedShelf.tsx";

/** Home's cards, by the id a board stores them under. Never renamed: a stored board names them. */
export const SECTION_IDS = [
  "pinned",
  "kpis",
  "decisions",
  "code",
  "live",
  "completed",
  "tokens",
  "projects",
  "feed",
] as const;
export type SectionId = (typeof SECTION_IDS)[number];

/**
 * The page's head: whose morning it is and the engine's sentence, the range
 * control, and the page bar's controls.
 */
export function HomeHead({
  data,
  actions,
  editing = false,
}: {
  data: HomeData;
  /** Controls for the page bar, before New task. */
  actions?: ReactNode;
  /** The page is being arranged: the page bar holds the editor's controls alone. */
  editing?: boolean;
}) {
  // WHO IS WORKING, at the head of this page and no other (`useWorkingNow`).
  useWorkingNow();
  return (
    <>
      <PageActions>
        {actions}
        {/* THE ONE SHEET every "New task" opens: see `app/newTask.ts`. While
            the page is arranged, the bar holds the editor's controls alone. */}
        {editing ? null : <NewTaskButton />}
      </PageActions>
      <header className="home-head">
        <div className="home-head-text">
          <h2 className="home-greeting">{greeting(data.now, data.own)}</h2>
          <p className="home-status">
            {data.sentence.map((run, i) =>
              run.strong ? (
                <strong key={i} className="home-status-figure">
                  {run.text}
                </strong>
              ) : run.href ? (
                <a key={i} className="prose-link" href={run.href}>
                  {run.text}
                </a>
              ) : (
                <span key={i}>{run.text}</span>
              ),
            )}
          </p>
        </div>
        <SegmentedControl
          label="Time range"
          semantics="radio"
          options={HOME_RANGES.map((r) => ({ value: r.value, label: r.label }))}
          value={data.range.value}
          onValueChange={(v) => data.setRange(v)}
        />
      </header>
    </>
  );
}

/** Home's figures, all five or the ones a widget names. */
export function Figures({
  data,
  figures = KPI_IDS,
}: {
  data: HomeData;
  figures?: readonly KpiId[];
}) {
  // A FIGURE A SERVICE THE COMPANY DOES NOT RUN WOULD ANSWER is not drawn,
  // and the row closes up over it (`kpisShown`).
  const shown = kpisShown(figures, useServicesRun());
  if (shown.length === 0) return null;
  return (
    <Kpis
      figures={shown}
      range={data.range}
      crew={data.crew}
      viewer={data.viewer}
      waiting={data.waiting}
      waitingError={data.decisions.error}
      flow={data.flow}
      spend={data.spend}
      now={data.now}
    />
  );
}

/** One of Home's cards, as it has always drawn. */
export function SectionView({ id, data }: { id: SectionId; data: HomeData }) {
  switch (id) {
    case "pinned":
      return <PinnedShelf />;
    case "kpis":
      return <Figures data={data} />;
    case "decisions":
      return (
        <Decisions
          viewer={data.viewer}
          answer={data.decisions.data}
          error={data.decisions.error}
          refusal={data.decisions.refusal}
          loading={data.decisions.loading}
          seatConditions={data.seats.mine}
          now={data.now}
        />
      );
    case "code":
      return <CodeWaiting />;
    case "live":
      return <LiveNow agents={data.agents} now={data.now} />;
    case "completed":
      return (
        <CompletedChart flow={data.flow} locked={!data.viewer.loading && !data.viewer.tracker} />
      );
    case "tokens":
      return <TokensByTeam spend={data.spend} range={data.range} />;
    case "projects":
      return <Projects />;
    case "feed":
      return <Feed now={data.now} />;
  }
}
