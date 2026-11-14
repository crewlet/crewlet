/**
 * The work on an entry: the open tasks that name it and the ones finished
 * lately, on its page's Work tab and, shorter, on its sheet. A task names the
 * entries it touches on its own record, so this is Worklet's answer read
 * through the atlas, and each app decides its own half.
 *
 * A reader who holds Atlas and not Worklet is shown the place locked, with
 * the access to ask for, and is asked nothing of the tracker. Where the
 * engine read no work for another reason, its own sentence stands in the
 * lists' place.
 */

import { useMemo, type ReactNode } from "react";
import { EmptyState, Link, LockedLabel, Section, Skeleton, Stack } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import type { AtlasEntryWorkAnswer } from "~/contract/atlas.ts";
import { WORKLET, lockReason, needsWords } from "~/lib/access.ts";
import { useQuery, type QueryResult } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { WorkClaimTotal, WorkSummary } from "~/protocol/index.ts";
import { TaskList, countLabel } from "~/routes/worklet/taskRows.tsx";
import { entryPath } from "./entry.tsx";
import { ATLAS_POLL_MS } from "./filters.ts";
import { openTasks } from "./words.ts";

/** The Work tab of an entry's page, as its address names it. */
export const WORK_TAB = "work";

/** What the place is called, on the tab and over the sheet's section. */
export const WORK_LABEL = "Work";

/** At most this many open tasks a sheet lists before pointing at the page. */
export const PEEK_WORK = 5;

/** How to link a task, said where no task names the entry. */
const HOW_TO_LINK = "To link one, open the task and choose Link an atlas entry.";

/** The address of an entry's Work tab. */
export function workHref(id: string): string {
  return href(entryPath(id), { tab: WORK_TAB });
}

/** The work on one entry, as its page and its sheet read it. */
export interface EntryWorkReading {
  /** Why the place is closed to this reader, or nothing where it is open. */
  locked: string | undefined;
  work: QueryResult<AtlasEntryWorkAnswer<WorkSummary>>;
}

/** Ask for the tasks naming an entry, of a reader who uses Worklet, while `enabled`. */
export function useEntryWork(id: string, enabled = true): EntryWorkReading {
  const viewer = useViewer();
  const params = useMemo(() => ({ id }), [id]);
  // THE TRACKER IS WORKLET'S, so it is asked only of a reader holding
  // Worklet: anybody else is told what would open it, never a refusal.
  const work = useQuery("atlas_entry_work", params, {
    enabled: enabled && viewer.worklet,
    pollMs: ATLAS_POLL_MS,
  });
  return {
    locked: !viewer.loading && !viewer.worklet ? lockReason([WORKLET]) : undefined,
    work,
  };
}

/** The place's name, with the key after it for a reader it is closed to. */
export function WorkLabel({ reading }: { reading: EntryWorkReading }) {
  return <LockedLabel reason={reading.locked}>{WORK_LABEL}</LockedLabel>;
}

/** How many open tasks name the entry, once the engine has said. */
export function openTotal(reading: EntryWorkReading): WorkClaimTotal | null {
  const answer = reading.work.data;
  return answer && !answer.unread ? answer.open_total : null;
}

/** What the tab says is happening behind it, while open tasks name the entry. */
export function openWords(total: WorkClaimTotal | null): string | undefined {
  if (!total || total.total === 0) return undefined;
  return `${openTasks(total.total)} ${total.total === 1 ? "names" : "name"} this entry`;
}

/**
 * What stands in the lists' place while there are none to draw: the lock, the
 * wait, the refusal, or the engine's own reason for reading no work. Nothing
 * once the lists can be drawn.
 */
function unread(reading: EntryWorkReading): ReactNode {
  if (reading.locked) {
    return <p className="muted">{needsWords("The work on an entry", [WORKLET])}</p>;
  }
  const { work } = reading;
  if (!work.data) {
    return work.loading ? (
      <Skeleton variant="text" rows={3} label="Loading the work" />
    ) : (
      <QueryState error={work.error} refusal={work.refusal} detail={work.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  if (work.data.unread) return <p className="muted">No work was read: {work.data.unread}.</p>;
  return null;
}

/** A list's count, as how many of them are drawn where the engine cut it. */
function shownOf(shown: number, total: WorkClaimTotal): string {
  return total.total > shown
    ? `${shown.toLocaleString()} of ${countLabel(total.total, total.capped)}`
    : shown.toLocaleString();
}

/** The Work tab: the open tasks naming the entry, then the ones finished lately. */
export function EntryWork({ reading }: { reading: EntryWorkReading }) {
  const held = unread(reading);
  const answer = reading.work.data;
  if (held || !answer) return held;
  // AN ANSWER WITHOUT ITS LISTS IS READ AS NONE, never thrown on.
  const open = answer.open ?? [];
  const recent = answer.recent ?? [];
  if (open.length === 0 && recent.length === 0) {
    return (
      <EmptyState size="compact" title="No task names this entry yet" description={HOW_TO_LINK} />
    );
  }
  return (
    <Stack gap={5}>
      <Section
        spacing="tight"
        headingLevel={2}
        title={`Open tasks (${shownOf(open.length, answer.open_total)})`}
      >
        {open.length > 0 ? (
          <TaskList rows={open} />
        ) : (
          <p className="muted">No open task names this entry. {HOW_TO_LINK}</p>
        )}
      </Section>
      <Section
        spacing="tight"
        headingLevel={2}
        title={`Finished in the last 30 days (${shownOf(recent.length, answer.recent_total)})`}
      >
        {recent.length > 0 ? (
          // A FINISHED TASK'S DUE DATE IS NO LONGER NEWS, so its row ends at its name.
          <TaskList rows={recent} trailing={() => null} />
        ) : (
          <p className="muted">No task naming this entry was finished in the last 30 days.</p>
        )}
      </Section>
    </Stack>
  );
}

/** The sheet's Work section: the first open tasks, and the way to the rest. */
export function PeekWork({ id, reading }: { id: string; reading: EntryWorkReading }) {
  return (
    <Section spacing="tight" headingLevel={3} title={<WorkLabel reading={reading} />}>
      <PeekTasks id={id} reading={reading} />
    </Section>
  );
}

function PeekTasks({ id, reading }: { id: string; reading: EntryWorkReading }) {
  const held = unread(reading);
  const answer = reading.work.data;
  if (held || !answer) return held;
  const open = (answer.open ?? []).slice(0, PEEK_WORK);
  const more = answer.open_total.total - open.length;
  const finished = answer.recent_total.total;
  if (open.length === 0) {
    return (
      <p className="muted">
        No open task names this entry.{" "}
        {finished > 0 ? (
          <Link href={workHref(id)} size="inherit">
            {countLabel(finished, answer.recent_total.capped)} finished in the last 30 days
          </Link>
        ) : (
          HOW_TO_LINK
        )}
      </p>
    );
  }
  return (
    <>
      <TaskList rows={open} />
      {more > 0 && (
        <p className="muted">
          <Link href={workHref(id)} size="inherit">
            And {countLabel(more, answer.open_total.capped)} more
          </Link>
        </p>
      )}
    </>
  );
}
