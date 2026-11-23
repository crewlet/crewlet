/**
 * The Advisor's run at the head of its page: two lines of one height whatever
 * its state (where it stands; what it is doing now or how it ended, with its
 * steps and silent checks as toggles), and Run. Steps open as a `StepTimeline`.
 *
 * A RUN IS FOLLOWED, NEVER WAITED ON: the page under it stays in use while it
 * runs (a deep run can take many minutes), its elapsed time ticks, and when it
 * ends the page reads the findings again.
 *
 * A SOURCE IT COULD NOT READ IS SAID, so a short list never reads as nothing
 * to do; a check that could not judge says why, folded.
 */

import { useEffect, useId, useState, type ReactNode } from "react";
import {
  Button,
  Meter,
  Skeleton,
  StatusLine,
  StepTimeline,
  Text,
  useToast,
  type StepTimelineStep,
  type Tone,
} from "@crewlethq/ui";
import { ChevronDownGlyph, ChevronRightGlyph } from "@crewlethq/icons/glyphs";
import { ago } from "~/lib/ago.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, fmtElapsed, fmtTime, plural, relTime } from "~/lib/format.ts";
import { unreadNames } from "~/lib/advisorUnread.ts";
import type { AdvisorRunSummary } from "~/protocol/advisorFindings.ts";
import { runNow, useLatestRun } from "./api.ts";

/** One step a run reported, as the engine says it (`AdvisorRunSummary.progress`). */
export type RunStep = AdvisorRunSummary["progress"][number];

/** A run as its head draws it. */
export interface ViewRun {
  status: "none" | "running" | "done" | "failed" | "interrupted";
  startedAt: string | null;
  endedAt: string | null;
  opened: number;
  resolved: number;
  /** The steps it reported, oldest first; the last is what it is doing now. */
  progress: RunStep[];
  /** The sources it could not read, by their words. */
  unread: string[];
  /** The classes it ran that could not judge, each with why. */
  silent: ClassOutcome[];
  /** The checks, or an investigation on the Advisor's model. */
  kind: "rules" | "deep";
  /** The bound a deep run stopped at, or "done". */
  stoppedBy: string | null;
  /** What a deep run spent of its bounds. */
  budget: AdvisorRunSummary["budget"];
  /** Why its model pass ended as it did, in the engine's words. */
  reason: string | null;
  /** The findings its model submitted that the engine left out, ungrounded. */
  dropped: number;
  /** Those left out, by why. */
  droppedWhy: Record<string, number>;
  /** The findings listed after a repair, by the repair. */
  repaired: Record<string, number>;
  /** Why a press's deep part did not run, and when the one it keeps ran. */
  deepSkipped: string | null;
  keptFrom: string | null;
  /** A deep run's questions and how many it has answered; null where it kept none. */
  agenda: { items: number; answered: number } | null;
}

/** One class a run ran, and why it could not judge where it could not. */
export interface ClassOutcome {
  class: string;
  status: string;
  reason: string | null;
}

/** A run as its head draws it. */
export function runView(run: AdvisorRunSummary | null): ViewRun {
  if (run === null) {
    return {
      status: "none",
      startedAt: null,
      endedAt: null,
      opened: 0,
      resolved: 0,
      progress: [],
      unread: [],
      silent: [],
      kind: "rules",
      stoppedBy: null,
      budget: null,
      reason: null,
      dropped: 0,
      droppedWhy: {},
      repaired: {},
      deepSkipped: null,
      keptFrom: null,
      agenda: null,
    };
  }
  const status: ViewRun["status"] =
    run.status === "running"
      ? "running"
      : run.status === "failed"
        ? "failed"
        : run.status === "interrupted"
          ? "interrupted"
          : "done";
  return {
    status,
    startedAt: run.started_at,
    endedAt: run.ended_at,
    opened: run.opened,
    resolved: run.resolved,
    progress: run.progress,
    unread: run.unread,
    silent: run.classes.filter((c) => c.status === "silent"),
    kind: run.kind === "deep" ? "deep" : "rules",
    stoppedBy: run.stopped_by,
    budget: run.budget,
    reason: run.model?.reason || null,
    dropped: Object.values(run.model?.dropped ?? {}).reduce((n, d) => n + d, 0),
    droppedWhy: run.model?.dropped ?? {},
    repaired: run.model?.repaired ?? {},
    deepSkipped: run.deep_skipped ?? null,
    keptFrom: run.deep_kept_from ?? null,
    agenda: run.agenda && run.agenda.items > 0 ? run.agenda : null,
  };
}

/** Why the model's findings were left out, in words. */
const DROPPED: Readonly<Record<string, string>> = {
  no_evidence: "cited nothing",
  unread_record: "cited a record it had not read",
  wrong_number: "cited a number not where it said",
  repeats_a_rule: "said what a check already says",
  answered: "was already answered",
  tokens_unsplit: "blamed tokens without their cache reads",
  too_many: "was past the ten a run lists",
  words: "broke the wording rule",
  fix_not_built: "had a fix that could not be built",
  explanation_ungrounded: "explained a finding with numbers it did not cite",
};

/** How the model's findings were mended to be listed, each with its count. */
function repairedWords(repaired: Record<string, number>): string[] {
  const n = (k: string) => repaired[k] ?? 0;
  return [
    n("fix_not_built") > 0
      ? `listed ${plural(n("fix_not_built"), "finding")} with no fix it could build`
      : "",
    n("reworded") > 0
      ? `reworded ${plural(n("reworded"), "finding")} the wording rule refused`
      : "",
    n("worded_by_engine") > 0
      ? `worded ${plural(n("worded_by_engine"), "finding")} from its numbers`
      : "",
    n("subject_widened") > 0
      ? `said ${plural(n("subject_widened"), "finding")} about the company, its seat gone`
      : "",
  ].filter(Boolean);
}

/** What the model's findings came to: those mended and those left out, and why. */
function modelWords(run: ViewRun): string[] {
  const why = Object.entries(run.droppedWhy)
    .filter(([, n]) => n > 0)
    .map(([k, n]) => `${n} ${DROPPED[k] ?? "could not be grounded"}`);
  return [
    ...repairedWords(run.repaired),
    run.dropped > 0
      ? `left out ${plural(run.dropped, "finding")} it could not ground in what it read${why.length > 0 ? ` (${why.join("; ")})` : ""}`
      : "",
  ].filter(Boolean);
}

/** The bound a deep run stopped at, in words. */
const BOUNDS: Readonly<Record<string, string>> = {
  tokens: "its token limit",
  minutes: "its time limit",
  rounds: "its round limit",
};

/**
 * The last run, in words.
 *
 * A RUN IS EVERY CHECK, and it is said so: "All checks ran 18 minutes ago".
 * A row's own "checked" time is that finding's, which an Apply's re-check
 * confirms on its own between runs.
 */
export function runWords(run: ViewRun, now: number): string {
  const deep = run.kind === "deep";
  switch (run.status) {
    case "none":
      return "The checks have not run yet.";
    case "running":
      return `${deep ? "Deep run" : "Running"} for ${fmtElapsed(now - Date.parse(run.startedAt ?? ""))}.`;
    case "interrupted":
      // ITS OWNER STOPPED (the engine restarted, or another node took the
      // duty) before it ended, and the engine closed it as such.
      return `The last ${deep ? "deep run" : "run"} stopped ${ago(run.endedAt ?? run.startedAt, now)}. ${run.reason ?? "It did not finish."}`;
    case "failed":
      // A DEEP RUN THAT ENDED AS IT BEGAN NEVER RAN (no model, for one), and
      // says why in the engine's words.
      if (deep) {
        const ran = run.endedAt !== null && run.endedAt !== run.startedAt;
        return `The last deep run did not ${ran ? "finish" : "run"}.${run.reason ? ` ${run.reason}` : ""}`;
      }
      return `The last run did not finish. It started ${ago(run.startedAt, now)}.`;
  }
  // WHAT IT MOVED, and only that: "closed 0" said nothing a reader needed.
  const moved = [
    run.opened > 0 ? `found ${plural(run.opened, "new finding")}` : "",
    run.resolved > 0 ? `closed ${plural(run.resolved, "finding")}` : "",
  ].filter(Boolean);
  if (!deep) {
    const did = [moved.length > 0 ? moved.join(" and ") : "", ...modelWords(run)].filter(Boolean);
    const found = did.length > 0 ? ` It ${did.join(", and ")}.` : "";
    return `All checks ran ${ago(run.endedAt ?? run.startedAt, now)}.${found}`;
  }
  // A DEEP PART THAT DID NOT RUN is said, with why: the checks ran all the same.
  if (run.deepSkipped) {
    const did = [moved.length > 0 ? moved.join(" and ") : "", ...modelWords(run)].filter(Boolean);
    const found = did.length > 0 ? ` It ${did.join(", and ")}.` : "";
    const part =
      run.deepSkipped === "kept" && run.keptFrom
        ? ` The deep investigation from ${ago(run.keptFrom, now)} is kept.`
        : ` The deep investigation could not run.${run.reason ? ` ${run.reason}` : ""}`;
    return `All checks ran ${ago(run.endedAt ?? run.startedAt, now)}.${found}${part}`;
  }
  const b = run.budget;
  const spent = b
    ? `${fmtCount(b.tokens)} of ${fmtCount(b.tokens_ceiling)} tokens in ${b.minutes} of ${b.minutes_ceiling} minutes`
    : "";
  const bound = run.stoppedBy && run.stoppedBy !== "done" ? (BOUNDS[run.stoppedBy] ?? null) : null;
  const head = bound
    ? `The deep run ended ${ago(run.endedAt ?? run.startedAt, now)} at ${bound}, so its list may be short.`
    : `The deep run ended ${ago(run.endedAt ?? run.startedAt, now)}.`;
  // A FINDING IT COULD NOT GROUND IS COUNTED, so an empty list is read as
  // one it found and could not stand behind, never as nothing found; one it
  // mended to list is counted too.
  const did = [
    moved.length > 0
      ? `${moved.join(" and ")}${spent ? `, using ${spent}` : ""}`
      : spent
        ? `used ${spent}`
        : "",
    ...modelWords(run),
  ].filter(Boolean);
  return did.length > 0 ? `${head} It ${did.join(", and ")}.` : head;
}

/** The bound a deep run stopped at, as a note. */
const BOUND_NOTES: Readonly<Record<string, string>> = {
  tokens: "hit its token limit",
  minutes: "hit its time limit",
  rounds: "hit its round limit",
};

/** The run's one line: its state's word, its tone, and a few short notes. */
export interface RunState {
  label: string;
  tone: Tone | undefined;
  pulse: boolean;
  notes: string[];
}

/** Where the run stands, in a word and a few short notes. */
export function runState(run: ViewRun, now: number): RunState {
  const deep = run.kind === "deep";
  const at = run.endedAt ?? run.startedAt;
  const unread = run.unread.length > 0 ? [`${plural(run.unread.length, "source")} not read`] : [];
  switch (run.status) {
    case "none":
      return { label: "Not run yet", tone: undefined, pulse: false, notes: [] };
    case "running":
      return {
        label: deep ? "Investigating" : "Running checks",
        tone: "info",
        pulse: true,
        notes: [
          fmtElapsed(now - Date.parse(run.startedAt ?? "")),
          ...(run.agenda ? [`${run.agenda.answered} of ${run.agenda.items} answered`] : []),
        ],
      };
    case "interrupted":
      return {
        label: deep ? "Investigation interrupted" : "Last run interrupted",
        tone: "warning",
        pulse: false,
        notes: [relTime(run.endedAt ?? run.startedAt, now), "did not finish"],
      };
    case "failed":
      return {
        label: deep ? "Investigation failed" : "Last run failed",
        tone: "danger",
        pulse: false,
        notes: [relTime(run.startedAt, now)],
      };
  }
  const moved = [
    run.opened > 0 ? `${fmtCount(run.opened)} new` : "",
    run.resolved > 0 ? `${fmtCount(run.resolved)} closed` : "",
  ].filter(Boolean);
  const investigated = deep && !run.deepSkipped;
  const bound =
    investigated && run.stoppedBy && run.stoppedBy !== "done" ? BOUND_NOTES[run.stoppedBy] : "";
  const left = investigated && run.dropped > 0 ? `${fmtCount(run.dropped)} left out` : "";
  const skipped =
    run.deepSkipped === "kept"
      ? "investigation kept"
      : run.deepSkipped
        ? "investigation did not run"
        : "";
  const notes = [
    ...(moved.length > 0 ? moved : ["nothing new"]),
    ...[bound ?? "", left, skipped].filter(Boolean),
    ...unread,
  ];
  const warn = unread.length > 0 || Boolean(bound) || (deep && run.deepSkipped === "no_model");
  return {
    label: investigated ? `Investigated ${ago(at, now)}` : `Checked ${ago(at, now)}`,
    tone: warn ? "warning" : "success",
    pulse: false,
    notes,
  };
}

/** The run in full: the line's tooltip, and what a screen reader reads with it. */
export function runDetail(run: ViewRun, now: number): string {
  const unread = run.unread.length > 0 ? ` Could not read ${unreadNames(run.unread)}.` : "";
  return `${runWords(run, now)}${unread}`;
}

/** The run's steps as the kit's timeline draws them. */
export function timelineSteps(steps: readonly RunStep[]): StepTimelineStep[] {
  return steps.map((step, i) => ({
    key: `${i}-${step.at}`,
    text: step.text,
    state: step.state === "failed" ? "failed" : step.state === "running" ? "running" : "done",
    ...(step.detail ? { detail: step.detail } : {}),
    ...(step.why ? { why: step.why } : {}),
    time: fmtTime(step.at),
    dateTime: step.at,
  }));
}

/** What the run is doing now: the step it is on, or its newest. */
export function nowStep(run: ViewRun): RunStep | null {
  if (run.status !== "running") return null;
  return run.progress.findLast((s) => s.state === "running") ?? run.progress.at(-1) ?? null;
}

type Fold = "steps" | "silent";

/**
 * The run in two lines: where it stands, then what it is doing now (or the
 * failed steps it ended with), with Steps and the checks that could not judge
 * as toggles at the second line's end; the one open is drawn under the head.
 */
export function RunLine({
  run,
  now,
  controls,
  loading = false,
}: {
  run: ViewRun;
  now: number;
  controls?: ReactNode;
  /** The run is not read yet: each line holds its room. */
  loading?: boolean;
}) {
  const running = run.status === "running";
  const state = runState(run, now);
  const doing = nowStep(run);
  const failed = run.progress.filter((s) => s.state === "failed").length;
  const silent = running ? [] : run.silent;
  const [open, setOpen] = useState<Fold | null>(null);
  const panel = useId();
  const toggle = (fold: Fold, label: string) => (
    <Button
      size="small"
      variant="ghost"
      aria-expanded={open === fold}
      aria-controls={panel}
      leadingIcon={open === fold ? <ChevronDownGlyph size="xs" /> : <ChevronRightGlyph size="xs" />}
      onClick={() => setOpen(open === fold ? null : fold)}
    >
      {label}
    </Button>
  );
  return (
    <>
      <div className="advisor-head">
        {/* THE PLACEMENT IS ON A BOX OF OUR OWN, never on the kit's Text: its
            own rule (min-width: 0) ties with ours by load order. */}
        {loading ? (
          <div className="advisor-head__run">
            <div className="advisor-head__state">
              <Skeleton variant="text" rows={1} width="16rem" className="advisor-head__skeleton" />
            </div>
            <div className="advisor-head__trail">
              <Skeleton variant="text" rows={1} width="24rem" className="advisor-head__skeleton" />
            </div>
          </div>
        ) : (
          <div className="advisor-head__run" aria-live="polite">
            <div className="advisor-head__state">
              <StatusLine
                label={state.label}
                tone={state.tone}
                pulse={state.pulse}
                notes={state.notes}
                detail={runDetail(run, now)}
              />
              {running && run.agenda ? (
                <Meter
                  className="advisor-run__meter"
                  size="compact"
                  polarity="progress"
                  tone="quantity"
                  label="Questions answered"
                  hideLabel
                  value={run.agenda.answered}
                  max={run.agenda.items}
                  valueText={`${run.agenda.answered} of ${plural(run.agenda.items, "question")} answered`}
                />
              ) : null}
            </div>
            <div className="advisor-head__trail">
              <Text as="p" variant="caption" tone="secondary" truncate className="advisor-run__now">
                {doing ? doing.text : running ? "Starting" : runTrail(run, now)}
              </Text>
              {run.progress.length > 0
                ? toggle(
                    "steps",
                    `${plural(run.progress.length, "step")}${failed > 0 ? `, ${fmtCount(failed)} failed` : ""}`,
                  )
                : null}
              {silent.length > 0
                ? toggle("silent", `${plural(silent.length, "check")} could not judge`)
                : null}
            </div>
          </div>
        )}
        <div className="advisor-head__controls">{controls}</div>
      </div>
      <div id={panel} className="advisor-run__panel" hidden={open === null}>
        {open === "steps" ? (
          <StepTimeline label="What the run did" steps={timelineSteps(run.progress)} />
        ) : null}
        {/* A CHECK THAT COULD NOT JUDGE SAYS WHY: a quiet list never reads as
            one that judged and found nothing. */}
        {open === "silent" ? (
          <ul className="advisor-run__silent" aria-label="Checks that could not judge">
            {silent.map((c) => (
              <li key={c.class}>
                <Text as="span" variant="caption" tone="secondary">
                  {c.reason ?? c.class}
                </Text>
              </li>
            ))}
          </ul>
        ) : null}
      </div>
    </>
  );
}

/** The second line of a run that is not going: how it ended, in a few words. */
function runTrail(run: ViewRun, now: number): string {
  if (run.status === "none") return "Run reads the company and lists what is worth changing.";
  const failed = run.progress.filter((s) => s.state === "failed");
  if (failed.length > 0) return failed.at(-1)!.text;
  return runWords(run, now);
}

/** When the checks last ran, Run, and a running run's progress. */
export function RunHead({ onEnded, children }: { onEnded: () => void; children?: ReactNode }) {
  const [started, setStarted] = useState(false);
  // FOLLOWED WHILE IT RUNS, whoever started it: a run the schedule began is
  // read every few seconds as one this page started is.
  const [watching, setWatching] = useState(false);
  const latest = useLatestRun(started || watching);
  const run = runView(latest.data ?? null);
  useEffect(() => setWatching(run.status === "running"), [run.status]);
  const running = run.status === "running";
  const now = useNow();
  const toast = useToast();
  // A RUN THIS PAGE STARTED is followed until it ends, then the findings are
  // read again for what it found.
  const ended = started && run.status !== "none" && !running && run.endedAt !== null;
  useEffect(() => {
    if (!ended) return;
    setStarted(false);
    onEnded();
  }, [ended, onEnded]);
  const start = () => {
    setStarted(true);
    void runNow().then(
      () => latest.refetch(),
      () =>
        toast.show({
          variant: "danger",
          title: "The run could not be started",
          message: "Try again in a moment.",
        }),
    );
  };
  const controls = (
    <>
      {children}
      <Button
        size="small"
        variant="secondary"
        disabledReason={running ? "A run is going" : undefined}
        onClick={start}
      >
        {/* ONE WORD IN EVERY STATE, so the controls beside it never move; the head says it runs. */}
        Run
      </Button>
    </>
  );
  // UNREAD, its two lines' room is held (the controls stay where they are), so
  // nothing under the head moves when the run is read.
  const unread = latest.data === null && latest.loading;
  return (
    <div className="advisor-head-wrap" aria-busy={running || unread}>
      <RunLine run={run} now={now} controls={controls} loading={unread} />
    </div>
  );
}
