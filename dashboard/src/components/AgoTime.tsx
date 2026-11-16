// A relative time inside a sentence, in words, as a time element.
// Cells and chips keep the kit's RelativeTime.
import { EMPTY_VALUE } from "@crewlethq/ui";
import { ago, type AgoOptions } from "~/lib/ago.ts";
import { useClock } from "~/lib/clock.ts";
import { fmtDateTime, parseUTC } from "~/lib/format.ts";

export interface AgoTimeProps extends AgoOptions {
  readonly value: string | null | undefined;
  /** Overrides the one clock, as the kit's `now` does. */
  readonly now?: number | undefined;
}

export function AgoTime({ value, now, ...options }: AgoTimeProps) {
  const words = useClock((t) => ago(value, now ?? t, options));
  if (!parseUTC(value)) return <>{EMPTY_VALUE}</>;
  return (
    <time dateTime={value ?? undefined} title={fmtDateTime(value)}>
      {words}
    </time>
  );
}
