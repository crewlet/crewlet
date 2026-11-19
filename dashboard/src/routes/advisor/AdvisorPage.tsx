/**
 * The Advisor's page (`#/advisor`): what its checks found that is worth an
 * operator's attention, each with the change that fixes it, its weekly
 * what was found before.
 *
 * # Anatomy
 *
 * The HEAD says when every check last ran ("All checks ran 4 minutes ago"), offers
 * Run now, and names the model its explanations run on, with a link to
 * Settings › Models, the one place it is chosen ([AdvisorModelLine]).
 *
 * FINDINGS, the first tab: the go-live check while anything blocks the
 * company, then what the owner can change, most costly first (Blocks, Costs,
 * Worth knowing; within each, the largest measured cost, then the newest),
 * then what the engine is handling, then what a person snoozed, each saying
 * until when. Each row says how fresh it is and offers its fix; Apply
 * re-checks the finding before anything is sent (`apply.ts`). Under them, while
 * anything blocks, the review of what each agent can do, folded: the go-live
 * check's reading, after what to change rather than between the check and it.
 *
 * REPORTS, the second tab: each week's report, newest first, a page at a
 * time. HISTORY, the third: what was found and closed, and why it closed.
 */

import { useCallback, useEffect, useState } from "react";
import {
  AttentionList,
  Button,
  Callout,
  EmptyState,
  Link,
  StepList,
  Text,
  useToast,
  type StepListStep,
} from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { ago } from "~/lib/ago.ts";
import { plural, relTime } from "~/lib/format.ts";
import { GoLiveReview } from "./GoLiveReview.tsx";
import { ApplyDialog } from "./ApplyDialog.tsx";
import { runNow, useFindings, useLatestRun, useReports } from "./api.ts";
import { useAskAbout, useDecisions } from "./decisions.tsx";
import { AtlasFix, listItems, snoozed, type RowActions } from "./rows.tsx";
import type {
  AdvisorKeptFinding,
  AdvisorReportSummary,
  AdvisorRunSummary,
} from "~/protocol/advisorFindings.ts";
import type { AdvisorSeverity } from "~/protocol/advisor.ts";
import { CLOSE_REASONS, SEVERITIES, STATE_WORDS, ranked, weekMoved, weekWords } from "./words.ts";
import { useHelperModel } from "~/components/HelperModel.tsx";

/** The Advisor page's tabs, as its address names them. */
export type AdvisorSection = "findings" | "reports" | "history";

export function AdvisorPage({ section }: { section: AdvisorSection }) {
  // A RUN THAT ENDED is read by the list under it: its key moves, so it reads again.
  const [runs, setRuns] = useState(0);
  const ended = useCallback(() => setRuns((n) => n + 1), []);
  return (
    <div className="col gap-4">
      <RunHead onEnded={ended} />
      {section === "findings" ? <Findings key={runs} /> : null}
      {section === "reports" ? <Reports /> : null}
      {section === "history" ? <History /> : null}
    </div>
  );
}

/** When the checks last ran, Run now, and the model. */
function RunHead({ onEnded }: { onEnded: () => void }) {
  const [started, setStarted] = useState(false);
  const latest = useLatestRun(started);
  const run = latest.data ?? null;
  const running = run?.status === "running";
  const now = useNow();
  const toast = useToast();
  const model = useHelperModel("advisor").current;
  // A RUN THIS PAGE STARTED is followed until it ends, then the findings are
  // read again for what it found.
  const ended = started && run !== null && !running && run.ended_at !== null;
  useEffect(() => {
    if (!ended) return;
    setStarted(false);
    onEnded();
  }, [ended, onEnded]);
  return (
    <div className="advisor-head">
      {/* THE PLACEMENT IS ON A BOX OF OUR OWN, never on the kit's Text: its
          own rule (min-width: 0) ties with ours, and whichever chunk's style
          loads last won, squeezing the line to a word a line on a phone. */}
      <div className="advisor-head__run">
        <Text as="p" tone="secondary">
          {runWords(run, now)}
        </Text>
      </div>
      <div className="advisor-head__controls">
        <Button
          size="small"
          variant="secondary"
          disabledReason={running ? "The checks are running" : undefined}
          onClick={() => {
            setStarted(true);
            void runNow().then(
              () => latest.refetch(),
              () =>
                toast.show({
                  variant: "danger",
                  title: "The checks could not be started",
                  message: "Try again in a moment.",
                }),
            );
          }}
        >
          {running ? "Running" : "Run now"}
        </Button>
        <AdvisorModelLine current={model} />
      </div>
    </div>
  );
}

/**
 * The model the Advisor explains its findings with, or that it has none, and
 * the way to Settings › Models, where it is chosen as a model's use. Without
 * a model its checks still run; only the explanations and the weekly
 * summary wait for one.
 */
export function AdvisorModelLine({ current }: { current: string | null }) {
  return (
    <Text as="span" variant="caption" tone="secondary">
      {current ? `Explains with ${current}. ` : "No model explains its findings. "}
      <Link href={href(["settings", "models"])} size="caption">
        {current ? "Model settings" : "Choose one in Model settings"}
      </Link>
    </Text>
  );
}

/**
 * The last run, in words.
 *
 * A RUN IS EVERY CHECK, and it is said so: "All checks ran 18 minutes ago".
 * A row's own "checked just now" is that finding's, which an Apply's
 * re-check confirms on its own between runs (the duty runs every check
 * hourly, or when the company's setup changes). Said alike, "Checked 18
 * minutes ago" over a row "checked just now" read as a list 18 minutes old.
 */
export function runWords(run: AdvisorRunSummary | null, now: number): string {
  if (run === null) return "The checks have not run yet.";
  if (run.status === "running") return `Checking now, started ${ago(run.started_at, now)}.`;
  if (run.status === "failed")
    return `The last run did not finish. It started ${ago(run.started_at, now)}.`;
  // WHAT IT MOVED, and only that: "closed 0" said nothing a reader needed.
  const moved = [
    run.opened > 0 ? `found ${plural(run.opened, "new finding")}` : "",
    run.resolved > 0 ? `closed ${plural(run.resolved, "finding")}` : "",
  ].filter(Boolean);
  const found = moved.length > 0 ? ` It ${moved.join(" and ")}.` : "";
  return `All checks ran ${ago(run.ended_at ?? run.started_at, now)}.${found}`;
}

/** The go-live check's own words for a step's state. */
const GO_LIVE_LABELS = { done: "Done", waiting: "To do", pending: "Can't say yet" };

/**
 * THE GO-LIVE CHECK: the owner's open findings counted by what ignoring them
 * costs, as three things to do before the agents start, each done when none
 * is left. Read off the same list as the rows under it, so the two cannot
 * differ.
 */
export function goLiveSteps(owner: readonly AdvisorKeptFinding[]): StepListStep[] {
  const count = (severity: AdvisorSeverity) => owner.filter((f) => f.severity === severity).length;
  const blocks = count("blocks");
  const costs = count("costs");
  const notes = count("note");
  return [
    {
      id: "blocks",
      label: blocks ? `Fix what blocks the company (${blocks})` : "Nothing blocks the company",
      state: blocks ? "waiting" : "done",
    },
    {
      id: "costs",
      label: costs ? `Look at what costs it (${costs})` : "Nothing costs it",
      state: costs ? "waiting" : "done",
    },
    {
      id: "notes",
      label: notes ? `Read what is worth knowing (${notes})` : "Nothing else to read",
      state: notes ? "waiting" : "done",
    },
  ];
}

/** The row actions every list on the page shares, and the dialog Apply opens. */
function useRowActions(reload: () => void) {
  const askAbout = useAskAbout();
  const decisions = useDecisions(reload);
  const [applying, setApplying] = useState<AdvisorKeptFinding | null>(null);
  const [reviewing, setReviewing] = useState<AdvisorKeptFinding | null>(null);
  const actions: RowActions = {
    apply: setApplying,
    review: setReviewing,
    ask: askAbout,
    snooze: decisions.snooze,
    wake: decisions.wake,
    dismiss: decisions.dismiss,
  };
  const dialog = (
    <>
      {applying ? (
        <ApplyDialog finding={applying} onClose={() => setApplying(null)} onSettled={reload} />
      ) : null}
      <AtlasFix
        finding={reviewing}
        onClose={() => {
          setReviewing(null);
          reload();
        }}
      />
    </>
  );
  return { actions, dialog };
}

function Findings() {
  const open = useFindings("open");
  const now = useNow();
  const { refetch } = open;
  const reload = useCallback(() => refetch(), [refetch]);
  const { actions, dialog } = useRowActions(reload);
  const all = ranked(open.data?.findings ?? []);
  // A SNOOZED FINDING LEAVES THE LISTS it would stand in, for one of its own
  // that says until when, and still counts toward the go-live check: the
  // condition holds, the person only asked not to see it for a while.
  const owner = all.filter((f) => f.actor === "owner");
  const shown = all.filter((f) => !snoozed(f, now));
  const asleep = all.filter((f) => snoozed(f, now));
  const engine = shown.filter((f) => f.actor === "engine");
  const steps = goLiveSteps(owner);
  const blocked = owner.some((f) => f.severity === "blocks");
  const change = shown.filter((f) => f.actor === "owner");
  return (
    <QueryState
      error={open.error}
      detail={open.detail}
      refusal={open.refusal}
      loading={open.loading && open.data === null}
    >
      {open.data ? (
        <>
          {/* UNTIL NOTHING BLOCKS, the go-live check leads. */}
          {blocked ? (
            <StepList
              label="Go-live check"
              steps={steps}
              labels={GO_LIVE_LABELS}
              summary={`Go-live check: ${steps.filter((s) => s.state === "done").length} of ${steps.length} done`}
            />
          ) : null}
          {change.length === 0 ? (
            <EmptyState
              size="compact"
              title="Nothing to change"
              description={
                asleep.length > 0
                  ? "Nothing to change right now beyond what you snoozed."
                  : "The Advisor's checks found nothing worth changing right now."
              }
            />
          ) : (
            <AttentionList
              items={listItems(change, now, actions)}
              labels={{
                title: "What to change",
                tones: Object.fromEntries(SEVERITIES.map((s) => [s.tone, s.label])),
              }}
            />
          )}
          {engine.length > 0 ? (
            <AttentionList
              items={listItems(engine, now, actions)}
              labels={{
                title: "What the engine is handling",
                tones: { danger: "Engine", warning: "Engine", info: "Engine" },
              }}
            />
          ) : null}
          {asleep.length > 0 ? (
            <AttentionList
              items={listItems(asleep, now, actions)}
              labels={{
                title: "Snoozed",
                tones: Object.fromEntries(SEVERITIES.map((s) => [s.tone, s.label])),
              }}
            />
          ) : null}
          {blocked ? <GoLiveReview /> : null}
        </>
      ) : null}
      {dialog}
    </QueryState>
  );
}

/**
 * The rows already shown, then a page's rows not among them. Until the next
 * page answers, the question still holds the page before, whose rows are
 * already held: drawn twice, they would repeat on screen and share a key.
 */
export function pagedOn<T>(
  held: readonly T[],
  page: readonly T[] | undefined,
  key: (row: T) => string,
): T[] {
  const seen = new Set(held.map(key));
  return [...held, ...(page ?? []).filter((row) => !seen.has(key(row)))];
}

/** The weekly reports, newest first: each opens its week's page. */
function Reports() {
  const [cursor, setCursor] = useState<string | null>(null);
  const [held, setHeld] = useState<AdvisorReportSummary[]>([]);
  const reports = useReports(cursor);
  const now = useNow();
  const rows = pagedOn(held, reports.data?.reports, (r) => r.week);
  return (
    <QueryState
      error={reports.error}
      detail={reports.detail}
      refusal={reports.refusal}
      loading={reports.loading && reports.data === null && held.length === 0}
    >
      {rows.length === 0 && reports.data ? (
        <EmptyState
          size="compact"
          title="No report yet"
          description="The Advisor writes a report every Monday morning: the week's numbers against the week before, and what it found."
        />
      ) : (
        <ul className="advisor-reports" aria-label="Weekly reports">
          {rows.map((r) => (
            <li key={r.week} className="advisor-history__row">
              <Link href={href(["advisor", "reports", r.week])}>{weekWords(r.week)}</Link>
              <Text as="span" variant="caption" tone="secondary">
                {weekMoved(r)}, written {ago(r.written_at, now)}
              </Text>
            </li>
          ))}
        </ul>
      )}
      {reports.data?.next_cursor ? (
        <Button
          size="small"
          variant="ghost"
          onClick={() => {
            setHeld(rows);
            setCursor(reports.data!.next_cursor);
          }}
        >
          Show older reports
        </Button>
      ) : null}
      {reports.error && held.length > 0 ? (
        <Callout variant="warning">Older reports could not be read. Try again in a moment.</Callout>
      ) : null}
    </QueryState>
  );
}

function History() {
  const [cursor, setCursor] = useState<string | null>(null);
  const [held, setHeld] = useState<AdvisorKeptFinding[]>([]);
  const closed = useFindings("closed", cursor);
  const now = useNow();
  const rows = pagedOn(held, closed.data?.findings, (f) => f.id);
  return (
    <QueryState
      error={closed.error}
      detail={closed.detail}
      refusal={closed.refusal}
      loading={closed.loading && closed.data === null && held.length === 0}
    >
      {rows.length === 0 && closed.data ? (
        <EmptyState
          size="compact"
          title="Nothing closed yet"
          description="A finding that clears, is fixed, expires or is dismissed is kept here for 90 days."
        />
      ) : (
        <ul className="advisor-history" aria-label="Closed findings">
          {rows.map((f) => (
            <li key={f.id} className="advisor-history__row">
              <Link href={href(["advisor", "findings", f.id])}>{f.title}</Link>
              <Text as="span" variant="caption" tone="secondary">
                {[
                  f.subject.name,
                  STATE_WORDS[f.state],
                  f.close_reason ? CLOSE_REASONS[f.close_reason] : null,
                  f.closed_at ? relTime(f.closed_at, now) : null,
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </Text>
            </li>
          ))}
        </ul>
      )}
      {closed.data?.next_cursor ? (
        <Button
          size="small"
          variant="ghost"
          onClick={() => {
            setHeld(rows);
            setCursor(closed.data!.next_cursor);
          }}
        >
          Show older findings
        </Button>
      ) : null}
      {closed.error && held.length > 0 ? (
        <Callout variant="warning">
          Older findings could not be read. Try again in a moment.
        </Callout>
      ) : null}
    </QueryState>
  );
}
