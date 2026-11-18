/** A check's mark and the phrases that say where it stands, shared by every Gitlet checks view. */

import { Spinner } from "@crewlethq/ui";
import {
  CircleCheckGlyph,
  CircleDashedGlyph,
  CircleStopGlyph,
  CircleXGlyph,
} from "@crewlethq/icons/glyphs";
import { ago } from "~/lib/ago.ts";
import { checkStateTone, checkStateWord } from "~/lib/checks.ts";
import { useNow } from "~/lib/clock.ts";
import { elapsedMs, fmtDateTime, fmtDuration, fmtElapsed } from "~/lib/format.ts";
import type { CodeCheckRow } from "~/protocol/index.ts";

/** A check's state as a mark: its glyph in its state's ink, a ring while it runs. */
export function CheckMark({ state }: { state: string }) {
  const glyph =
    state === "passed" ? (
      <CircleCheckGlyph size="sm" />
    ) : state === "failed" || state === "errored" ? (
      <CircleXGlyph size="sm" />
    ) : state === "running" ? (
      <Spinner size="sm" />
    ) : state === "queued" ? (
      <CircleDashedGlyph size="sm" />
    ) : (
      <CircleStopGlyph size="sm" />
    );
  return (
    <span className="code-check-mark" data-tone={checkStateTone(state)} aria-hidden="true">
      {glyph}
    </span>
  );
}

/** "passed in 1m 35s", "running for 2m 10s", "queued 2 minutes ago". */
export function CheckPhrase({ row }: { row: CodeCheckRow }) {
  const took = row.duration_ms ?? elapsedMs(row.started_at, row.finished_at);
  // NO DURATION KNOWN (it never started, or its CI sent none): when it ended, never a guess.
  if (
    took === null &&
    (row.state === "passed" || row.state === "failed" || row.state === "errored")
  )
    return row.finished_at ? (
      <EndedAgo word={checkStateWord(row.state)} at={row.finished_at} />
    ) : (
      <span>{checkStateWord(row.state)}</span>
    );
  switch (row.state) {
    case "passed":
      return <span title={fmtDateTime(row.finished_at)}>passed in {fmtDuration(took)}</span>;
    case "failed":
    case "errored":
      return (
        <span title={fmtDateTime(row.finished_at)}>
          {row.state} after {fmtDuration(took)}
        </span>
      );
    case "running":
      return row.started_at ? <RunningFor since={row.started_at} /> : <span>running</span>;
    case "queued":
      return row.queued_at ? <QueuedAgo at={row.queued_at} /> : <span>queued</span>;
  }
  return <span>{checkStateWord(row.state)}</span>;
}

/** A running check's clock, ticking each second only while it runs. */
export function RunningFor({ since }: { since: string }) {
  const now = useNow();
  return <span title={fmtDateTime(since)}>running for {fmtElapsed(now - Date.parse(since))}</span>;
}

export function QueuedAgo({ at }: { at: string }) {
  return <EndedAgo word="queued" at={at} />;
}

/** A state word and how long ago it came to be: "errored 2 minutes ago". */
export function EndedAgo({ word, at }: { word: string; at: string }) {
  const now = useNow();
  return (
    <span>
      {word}{" "}
      <time dateTime={at} title={fmtDateTime(at)}>
        {ago(at, now, { clock: true })}
      </time>
    </span>
  );
}
