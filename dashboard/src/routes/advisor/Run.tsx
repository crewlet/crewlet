/**
 * The Advisor's run, at the head of its page: when every check last ran and
 * what it moved, Run now, and while a run is going, how long it has run and
 * what it is doing.
 *
 * A RUN IS FOLLOWED, NEVER WAITED ON: the page under it stays in use while it
 * runs (a deep run can take many minutes), its elapsed time ticks, and each
 * step the run reports is listed as it lands, the newest naming the lead it
 * is testing. When it ends, the page reads the findings again.
 *
 * A SOURCE IT COULD NOT READ IS SAID, so a short list never reads as nothing
 * to do.
 *
 * A DEEP RUN IS ITS OWN PRESS (Run deep): it investigates with the Advisor's
 * model and can take many minutes. Its line says it is one, what it spent of
 * its bounds, the bound it stopped at (so a short list is read as one), and,
 * where it could not run, why.
 */

import { useEffect, useState, type ReactNode } from "react";
import { Button, Disclosure, Text, useToast } from "@crewlethq/ui";
import { ago } from "~/lib/ago.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, fmtElapsed, plural } from "~/lib/format.ts";
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
  };
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
    const found = moved.length > 0 ? ` It ${moved.join(" and ")}.` : "";
    return `All checks ran ${ago(run.endedAt ?? run.startedAt, now)}.${found}`;
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
  // one it found and could not stand behind, never as nothing found.
  const did = [
    moved.length > 0
      ? `${moved.join(" and ")}${spent ? `, using ${spent}` : ""}`
      : spent
        ? `used ${spent}`
        : "",
    run.dropped > 0
      ? `left out ${plural(run.dropped, "finding")} it could not ground in what it read`
      : "",
  ].filter(Boolean);
  return did.length > 0 ? `${head} It ${did.join(", and ")}.` : head;
}

/**
 * The run in words, and while it runs the steps it reported so far, the
 * newest (the lead it is testing) on the line itself.
 */
export function RunLine({ run, now }: { run: ViewRun; now: number }) {
  const running = run.status === "running";
  const lead = run.progress.at(-1);
  return (
    // THE PLACEMENT IS ON A BOX OF OUR OWN, never on the kit's Text: its own
    // rule (min-width: 0) ties with ours, and whichever chunk's style loads
    // last won, squeezing the line to a word a line on a phone.
    <div className="advisor-head__run">
      <Text as="p" tone="secondary" aria-live="polite">
        {runWords(run, now)}
        {running && lead ? ` ${lead.text}` : ""}
        {run.unread.length > 0 ? ` Could not read ${unreadNames(run.unread)}.` : ""}
      </Text>
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
      {running && run.progress.length > 1 ? (
        <ol className="advisor-run__steps" aria-label="What the run did so far">
          {run.progress.slice(0, -1).map((step) => (
            <li key={`${step.at}-${step.text}`}>
              <Text as="span" variant="caption" tone="secondary">
                {step.text}
              </Text>
            </li>
          ))}
        </ol>
      ) : null}
    </div>
  );
}

/** When the checks last ran, Run now, and a running run's progress. */
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
  const start = (kind: "rules" | "deep") => {
    setStarted(true);
    void runNow(kind).then(
      () => latest.refetch(),
      () =>
        toast.show({
          variant: "danger",
          title:
            kind === "deep"
              ? "The deep run could not be started"
              : "The checks could not be started",
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
          onClick={() => start("rules")}
        >
          {running ? "Running" : "Run now"}
        </Button>
        <Button
          size="small"
          variant="ghost"
          disabledReason={running ? "A run is going" : undefined}
          onClick={() => start("deep")}
        >
          Run deep
        </Button>
      </div>
    </div>
  );
}
