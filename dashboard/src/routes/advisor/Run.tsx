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
 */

import { useEffect, useState, type ReactNode } from "react";
import { Button, Text, useToast } from "@crewlethq/ui";
import { ago } from "~/lib/ago.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtElapsed, plural } from "~/lib/format.ts";
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
}

/** The latest run, as the head draws it. Today's run reports no steps of its own. */
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
    };
  }
  const status = run.status === "running" ? "running" : run.status === "failed" ? "failed" : "done";
  return {
    status,
    startedAt: run.started_at,
    endedAt: run.ended_at,
    opened: run.opened,
    resolved: run.resolved,
    progress: [],
    unread: run.unread,
  };
}

/**
 * The last run, in words.
 *
 * A RUN IS EVERY CHECK, and it is said so: "All checks ran 18 minutes ago".
 * A row's own "checked" time is that finding's, which an Apply's re-check
 * confirms on its own between runs.
 */
export function runWords(run: ViewRun, now: number): string {
  switch (run.status) {
    case "none":
      return "The checks have not run yet.";
    case "running":
      return `Running for ${fmtElapsed(now - Date.parse(run.startedAt ?? ""))}.`;
    case "failed":
      return `The last run did not finish. It started ${ago(run.startedAt, now)}.`;
  }
  // WHAT IT MOVED, and only that: "closed 0" said nothing a reader needed.
  const moved = [
    run.opened > 0 ? `found ${plural(run.opened, "new finding")}` : "",
    run.resolved > 0 ? `closed ${plural(run.resolved, "finding")}` : "",
  ].filter(Boolean);
  const found = moved.length > 0 ? ` It ${moved.join(" and ")}.` : "";
  return `All checks ran ${ago(run.endedAt ?? run.startedAt, now)}.${found}`;
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
  return (
    <div className="advisor-head" aria-busy={running}>
      <RunLine run={run} now={now} />
      <div className="advisor-head__controls">
        {children}
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
      </div>
    </div>
  );
}
