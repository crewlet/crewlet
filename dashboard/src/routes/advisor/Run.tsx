/**
 * The Advisor's run, at the head of its page: one line of where it stands
 * (its state, how long or how long ago, what it moved), Run, and while a run
 * is going what it is doing now and how far through its questions it is.
 *
 * CONCISE ON THE PAGE, WHOLE ON DEMAND: the line's notes are a word or two
 * each, the full sentence is the state's tooltip, and every step the run
 * reported is folded behind Steps, in the engine's plain words.
 *
 * A RUN IS FOLLOWED, NEVER WAITED ON: the page under it stays in use while it
 * runs (a deep run can take many minutes), its elapsed time ticks, and when it
 * ends the page reads the findings again.
 *
 * A SOURCE IT COULD NOT READ IS SAID, so a short list never reads as nothing
 * to do; a check that could not judge says why, folded.
 */

import { useEffect, useState, type ReactNode } from "react";
import { Button, Disclosure, Meter, StatusLine, Text, useToast, type Tone } from "@crewlethq/ui";
import { ago } from "~/lib/ago.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, fmtElapsed, plural, relTime } from "~/lib/format.ts";
import { unreadNames } from "~/lib/advisorUnread.ts";
import type { AdvisorRunSummary } from "~/protocol/advisorFindings.ts";
import { runNow, useLatestRun } from "./api.ts";

/** One step a run reported. */
export interface RunStep {
  at: string;
  text: string;
}

/** A run as its head draws it. */
export interface ViewRun {
  status: "none" | "running" | "done" | "failed";
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
  const status = run.status === "running" ? "running" : run.status === "failed" ? "failed" : "done";
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

/**
 * The run in one line, and while it runs what it is doing now and how far
 * through its questions it is; every step it reported, folded under Steps.
 */
export function RunLine({ run, now }: { run: ViewRun; now: number }) {
  const running = run.status === "running";
  const state = runState(run, now);
  const doing = running ? run.progress.at(-1)?.text : undefined;
  return (
    // THE PLACEMENT IS ON A BOX OF OUR OWN, never on the kit's Text: its own
    // rule (min-width: 0) ties with ours, and whichever chunk's style loads
    // last won, squeezing the line to a word a line on a phone.
    <div className="advisor-head__run" aria-live="polite">
      <StatusLine
        label={state.label}
        tone={state.tone}
        pulse={state.pulse}
        notes={state.notes}
        detail={runDetail(run, now)}
      />
      {doing ? (
        <Text as="p" variant="caption" tone="secondary" className="advisor-run__now">
          {doing}
        </Text>
      ) : null}
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
      {run.progress.length > 0 || (!running && run.silent.length > 0) ? (
        <div className="advisor-run__folds">
          {run.progress.length > 0 ? (
            <Disclosure
              title="Steps"
              count={run.progress.length}
              size="compact"
              headingLevel="none"
              flush
              lazy
            >
              <ol className="advisor-run__steps" aria-label="What the run did">
                {run.progress.map((step, i) => (
                  <li key={`${i}-${step.at}`}>
                    <Text as="span" variant="caption" tone="secondary">
                      {step.text}
                    </Text>
                  </li>
                ))}
              </ol>
            </Disclosure>
          ) : null}
          {/* A CHECK THAT COULD NOT JUDGE SAYS WHY, folded: a quiet list never
              reads as one that judged and found nothing. */}
          {!running && run.silent.length > 0 ? (
            <Disclosure
              title={`${plural(run.silent.length, "check")} could not judge`}
              size="compact"
              headingLevel="none"
              flush
            >
              <ul className="advisor-run__steps">
                {run.silent.map((c) => (
                  <li key={c.class}>
                    <Text as="span" variant="caption" tone="secondary">
                      {c.reason ?? c.class}
                    </Text>
                  </li>
                ))}
              </ul>
            </Disclosure>
          ) : null}
        </div>
      ) : null}
    </div>
  );
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
  return (
    <div className="advisor-head" aria-busy={running}>
      <RunLine run={run} now={now} />
      <div className="advisor-head__controls">
        {children}
        <Button
          size="small"
          variant="secondary"
          disabledReason={running ? "A run is going" : undefined}
          onClick={start}
        >
          {running ? "Running" : "Run"}
        </Button>
      </div>
    </div>
  );
}
