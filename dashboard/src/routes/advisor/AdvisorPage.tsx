/**
 * The Advisor's page (`#/advisor`): what its checks found that is worth an
 * operator's attention, each with the change that fixes it, its weekly
 * what was found before.
 *
 * # Anatomy
 *
 * The HEAD says when the checks last ran ("Checked 4 minutes ago"), offers
 * Run now, and names the model its explanations run on (an
 * administrator's is the picker, `components/HelperModel.tsx`).
 *
 * FINDINGS, the first tab: the go-live check while anything blocks the
 * company, then what the owner can change, most costly first (Blocks, Costs,
 * Worth knowing; within each, the largest measured cost, then the newest),
 * then what the engine is handling. Each row says how fresh it is and offers
 * its fix; Apply re-checks the finding before anything is sent (`apply.ts`).
 *
 * HISTORY, the second tab: what was found and closed, and why it closed.
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
import { useAssistantControl } from "~/app/assistant/control.ts";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { ago } from "~/lib/ago.ts";
import { plural, relTime } from "~/lib/format.ts";
import { GoLiveReview } from "./GoLiveReview.tsx";
import { ApplyDialog } from "./ApplyDialog.tsx";
import { decide, runNow, useFindings, useLatestRun } from "./api.ts";
import { AtlasFix, findingItem, type RowActions } from "./rows.tsx";
import type { AdvisorKeptFinding, AdvisorRunSummary } from "~/protocol/advisorFindings.ts";
import type { AdvisorSeverity } from "~/protocol/advisor.ts";
import { CLOSE_REASONS, SEVERITIES, STATE_WORDS, ranked } from "./words.ts";
import { AdvisorModel } from "./AdvisorModel.tsx";

/** The Advisor page's tabs, as its address names them. */
export type AdvisorSection = "findings" | "history";

export function AdvisorPage({ section }: { section: AdvisorSection }) {
  // A RUN THAT ENDED is read by the list under it: its key moves, so it reads again.
  const [runs, setRuns] = useState(0);
  const ended = useCallback(() => setRuns((n) => n + 1), []);
  return (
    <div className="col gap-4">
      <RunHead onEnded={ended} />
      {section === "findings" ? <Findings key={runs} /> : null}
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
      <Text as="p" tone="secondary" className="advisor-head__run">
        {runWords(run, now)}
      </Text>
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
      <AdvisorModel />
    </div>
  );
}

/** The last run, in words. */
export function runWords(run: AdvisorRunSummary | null, now: number): string {
  if (run === null) return "The checks have not run yet.";
  if (run.status === "running") return `Checking now, started ${ago(run.started_at, now)}.`;
  if (run.status === "failed")
    return `The last run did not finish. It started ${ago(run.started_at, now)}.`;
  const found =
    run.opened + run.resolved > 0
      ? ` It found ${plural(run.opened, "new finding")} and closed ${run.resolved}.`
      : "";
  return `Checked ${ago(run.ended_at ?? run.started_at, now)}.${found}`;
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
  const assistant = useAssistantControl();
  const toast = useToast();
  const [applying, setApplying] = useState<AdvisorKeptFinding | null>(null);
  const [reviewing, setReviewing] = useState<AdvisorKeptFinding | null>(null);
  const answer = useCallback(
    (f: AdvisorKeptFinding, press: () => Promise<unknown>, done: string) => {
      void press().then(
        () => {
          toast.show({ variant: "success", title: done, message: f.title });
          reload();
        },
        () =>
          toast.show({
            variant: "danger",
            title: "Nothing was changed",
            message: "The engine did not take that. Try again in a moment.",
          }),
      );
    },
    [toast, reload],
  );
  const actions: RowActions = {
    apply: setApplying,
    review: setReviewing,
    ask: assistant.unavailable ? null : (question) => assistant.ask(question),
    snooze: (f) => answer(f, () => decide(f.id, { decision: "snooze" }), "Snoozed for 7 days"),
    dismiss: (f, reason) =>
      answer(f, () => decide(f.id, { decision: "dismiss", reason }), "Dismissed"),
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
  const owner = all.filter((f) => f.actor === "owner");
  const engine = all.filter((f) => f.actor === "engine");
  const steps = goLiveSteps(owner);
  const blocked = owner.some((f) => f.severity === "blocks");
  return (
    <QueryState
      error={open.error}
      detail={open.detail}
      refusal={open.refusal}
      loading={open.loading && open.data === null}
    >
      {open.data ? (
        <>
          {/* UNTIL NOTHING BLOCKS, the go-live check and the agents' access lead. */}
          {blocked ? (
            <>
              <StepList
                label="Go-live check"
                steps={steps}
                labels={GO_LIVE_LABELS}
                summary={`Go-live check: ${steps.filter((s) => s.state === "done").length} of ${steps.length} done`}
              />
              <GoLiveReview />
            </>
          ) : null}
          {owner.length === 0 ? (
            <EmptyState
              size="compact"
              title="Nothing to change"
              description="The Advisor's checks found nothing worth changing right now."
            />
          ) : (
            <AttentionList
              items={owner.map((f) => findingItem(f, now, actions))}
              labels={{
                title: "What to change",
                tones: Object.fromEntries(SEVERITIES.map((s) => [s.tone, s.label])),
              }}
            />
          )}
          {engine.length > 0 ? (
            <AttentionList
              items={engine.map((f) => findingItem(f, now, actions))}
              labels={{
                title: "What the engine is handling",
                tones: { danger: "Engine", warning: "Engine", info: "Engine" },
              }}
            />
          ) : null}
        </>
      ) : null}
      {dialog}
    </QueryState>
  );
}

function History() {
  const [cursor, setCursor] = useState<string | null>(null);
  const [held, setHeld] = useState<AdvisorKeptFinding[]>([]);
  const closed = useFindings("closed", cursor);
  const now = useNow();
  const rows = [...held, ...(closed.data?.findings ?? [])];
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
