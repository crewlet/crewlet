/**
 * The engine's warnings about a company, as one callout: each kind said once,
 * with the seats, teams and items it applies to listed under it.
 */

import { useMemo, type ReactNode } from "react";
import { Callout, groupNotices, NoticeList, type NoticeEntry } from "@crewlethq/ui";
import { plural } from "~/lib/format.ts";
import { warningWords, type SeatNamer } from "~/protocol/configAnswer.ts";
import type { ConfigWarning } from "~/protocol/types.ts";
import { marked } from "~/ui/Problems.tsx";
import { useOrgIndex } from "~/lib/seats.ts";

/** An engine sentence as drawn in a notice: sentence case, its keys and values as code. */
export function engineWords(text: string): ReactNode {
  const raised = /^[a-z][a-z']*\s/.test(text) ? text.charAt(0).toUpperCase() + text.slice(1) : text;
  return marked(raised, "inherit");
}

/** A warning's kit entry: equal words merge repeats, its seat or unit is the subject. */
export function configWarningEntries(
  warnings: readonly ConfigWarning[],
  seat: SeatNamer,
  action?: (w: ConfigWarning) => ReactNode,
  operator = false,
): NoticeEntry[] {
  return warnings.map((w) => {
    const words = operator ? w.message : warningWords(w);
    const titled = !operator && Boolean(w.title);
    return {
      // ONE KIND IS ONE WORDING: the engine words every warning of a rule alike, so equal words merge.
      kind: `${w.kind}\u0000${w.title ?? ""}\u0000${words}`,
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
/** A write the engine takes, with warnings. */
export const accepted = (n: number) => `The engine accepts this, with ${plural(n, "warning")}`;
/** What a change would add to what the engine already warns about. */
export const brings = (n: number) => `This change brings ${plural(n, "warning")}`;

/** Seats named off the chart every screen reads. */
export function useSeatNamer(): SeatNamer {
  const index = useOrgIndex();
  return useMemo(
    () => (handle: string) => {
      const s = index.byHandle.get(handle);
      return s === undefined ? undefined : { name: s.name || handle, agent: s.kind === "agent" };
    },
    [index],
  );
}

/** The warnings, named by the caller's seats where it has them and by the chart's otherwise. */
export function ConfigWarnings(props: ConfigWarningsProps) {
  return props.seat ? <Drawn {...props} seat={props.seat} /> : <ChartNamed {...props} />;
}

function ChartNamed(props: ConfigWarningsProps) {
  return <Drawn {...props} seat={useSeatNamer()} />;
}

interface ConfigWarningsProps {
  warnings: readonly ConfigWarning[];
  /** How a seat is named; the chart's names where the screen has no answer of its own. */
  seat?: SeatNamer | undefined;
  /** The one place each warning is changed, where the screen has one. */
  actionFor?: ((w: ConfigWarning) => ReactNode) | undefined;
  /** The callout's one action: a decision about all of them, such as Save anyway. */
  action?: ReactNode;
  title?: ((count: number) => ReactNode) | undefined;
  /** The operator's words (`message`, naming document keys), for a screen that edits the document itself. */
  operator?: boolean;
  /** The group's name for a screen reader. */
  label?: string | undefined;
  className?: string;
}

function Drawn({
  warnings,
  seat,
  actionFor,
  action,
  title = toKnow,
  label = "Warnings",
  operator = false,
  className,
}: ConfigWarningsProps & { seat: SeatNamer }) {
  const notices = groupNotices(configWarningEntries(warnings, seat, actionFor, operator), {
    count: subjectCount(warnings, seat),
  });
  if (notices.length === 0) return null;
  return (
    <Callout
      variant="warning"
      role="group"
      aria-label={label}
      title={title(notices.length)}
      action={action}
      className={className}
    >
      <NoticeList notices={notices} />
    </Callout>
  );
}
