/**
 * A wait on the engine, said honestly: what is running, and for how long so
 * far.
 *
 * A HARNESS RUN TAKES SECONDS AND SOMETIMES A MINUTE, and a spinner beside a
 * sentence that never changes reads as a frozen form by the tenth second. The
 * count beside it is the one thing on screen that moves with the wait, so a
 * person can tell a run that is slow from a page that has stopped.
 *
 * THE COUNT IS NOT IN THE STATUS REGION: a live region announces every
 * change, and a screen reader would be read a number once a second. The
 * sentence is announced once; the count is for the eye.
 */

import { useState, type ReactNode } from "react";
import { Spinner, Text } from "@crewlethq/ui";
import { useClock } from "~/lib/clock.ts";

/** "8 s", then "1 min 05 s" past the first minute. */
export function elapsedWords(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  if (seconds < 60) return `${seconds} s`;
  return `${Math.floor(seconds / 60)} min ${String(seconds % 60).padStart(2, "0")} s`;
}

export function Waiting({ children }: { children: ReactNode }) {
  // FROM WHEN THIS WAIT WAS DRAWN: each wait mounts its own line, so the
  // count starts with the request it stands for.
  const [since] = useState(() => Date.now());
  const elapsed = useClock((now) => elapsedWords(now - since));
  return (
    <div className="models-waiting">
      <Spinner size="sm" />
      <span role="status">
        <Text as="span" variant="body">
          {children}
        </Text>
      </span>
      <span className="models-waiting__elapsed" aria-hidden="true">
        {elapsed}
      </span>
    </div>
  );
}
