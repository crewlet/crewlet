/**
 * The engine's warnings about a company, as one callout: each kind said once,
 * with the seats, teams and items it applies to listed under it.
 */

import type { ReactNode } from "react";
import { Callout, groupNotices, NoticeList, type NoticeEntry } from "@crewlethq/ui";
import { plural } from "~/lib/format.ts";
import { warningWords, type SeatNamer } from "~/protocol/configAnswer.ts";
import type { ConfigWarning } from "~/protocol/types.ts";
import { marked } from "~/ui/Problems.tsx";

/** An engine sentence as drawn in a notice: sentence case, its keys and values as code. */
export function engineWords(text: string): ReactNode {
  const raised = /^[a-z][a-z']*\s/.test(text) ? text.charAt(0).toUpperCase() + text.slice(1) : text;
  return marked(raised, "inherit");
}

/** A warning's kit entry: its class merges repeats, its seat or unit is the subject. */
export function configWarningEntries(
  warnings: readonly ConfigWarning[],
  seat: SeatNamer,
  action?: (w: ConfigWarning) => ReactNode,
): NoticeEntry[] {
  return warnings.map((w) => {
    const words = warningWords(w);
    const titled = Boolean(w.title);
    return {
      kind: w.class || `${w.kind}\u0000${words}`,
      tone: "warning",
      subject: w.seat ? (seat(w.seat)?.name ?? w.seat) : w.unit || undefined,
      item: w.item || undefined,
      title: engineWords(titled ? w.title! : words),
      detail: titled ? engineWords(words) : undefined,
      action: action?.(w),
    };
  });
}

/** Several subjects named together: "9 agents", "3 seats", "2 teams". */
export function subjectCount(warnings: readonly ConfigWarning[], seat: SeatNamer) {
  const agents = new Set<string>();
  const seats = new Set<string>();
  for (const w of warnings) {
    if (!w.seat) continue;
    const s = seat(w.seat);
    const name = s?.name ?? w.seat;
    seats.add(name);
    if (s?.agent) agents.add(name);
  }
  return (subjects: readonly string[]) => {
    const n = subjects.length;
    if (subjects.every((s) => agents.has(s))) return plural(n, "agent");
    if (subjects.every((s) => seats.has(s))) return plural(n, "seat");
    if (subjects.every((s) => !seats.has(s))) return plural(n, "team");
    return plural(n, "place");
  };
}

/** The default title: how many things there are to know, after repeats merge. */
export const toKnow = (n: number) => `${plural(n, "warning")} to know about`;

export function ConfigWarnings({
  warnings,
  seat,
  action,
  title = toKnow,
  label = "Warnings",
  className,
}: {
  warnings: readonly ConfigWarning[];
  seat: SeatNamer;
  /** The one place each warning is changed, where the screen has one. */
  action?: (w: ConfigWarning) => ReactNode;
  title?: (count: number) => ReactNode;
  /** The group's name for a screen reader. */
  label?: string | undefined;
  className?: string;
}) {
  const notices = groupNotices(configWarningEntries(warnings, seat, action), {
    count: subjectCount(warnings, seat),
  });
  if (notices.length === 0) return null;
  return (
    <Callout
      variant="warning"
      role="group"
      aria-label={label}
      title={title(notices.length)}
      className={className}
    >
      <NoticeList notices={notices} />
    </Callout>
  );
}
