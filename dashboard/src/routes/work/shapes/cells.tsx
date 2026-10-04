/**
 * The three fields a writer changes from a row without opening the task:
 * its status, its priority and who holds it.
 *
 * # One write per grid, and its refusal above the grid
 *
 * Every inline cell presses the same tool — `update_work_item`, conditional on
 * the `version` the row was drawn from (`if_match`) — so the grid holds ONE
 * [useAct] and every cell sends through it ([InlineEdits]). The refusal is then
 * one sentence above the rows rather than a hundred cells each able to grow an
 * error beneath them, and a second press replaces the first one's answer the
 * way a single control's does.
 *
 * # A lost race names who won it
 *
 * A conditional edit refused `stale_version` means somebody changed the task
 * between this list's read and the press. The refusal carries the version it
 * lost to and not its author, so the note asks the task's own history for its
 * newest entry (`work_activity{task, limit: 1}`) and says "Changed by Maya
 * since you opened it" — the person to talk to, rather than "somebody else".
 * Until that answer arrives the engine's own sentence stands; the write's own
 * refusal has already asked the list again, so the row redraws at the version
 * that won.
 *
 * # Only a writer gets a control
 *
 * A reader who cannot change the task sees the value the row always drew. A
 * disabled picker in every row of a hundred-row list is a column of controls
 * saying the same "set a token" a hundred times. The reason is drawn ONCE,
 * above the grid, in the sentence every other write control uses — a write is
 * never hidden, and a reader told nothing would take the list for read-only
 * by design.
 */

import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { Button, EmptyValue, Menu } from "@crewlethq/ui";
import { RefusalNote } from "~/components/WriteButton.tsx";
import { Assignee, PriorityMark, StatusBadge, type RowChrome } from "~/components/work.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { humanize } from "~/lib/format.ts";
import { PRIORITIES, STATUSES, statusLabel, itemAddress } from "~/lib/work.ts";
import type { WorkStatusDef, WorkSummary } from "~/protocol/index.ts";

/** One inline change: the field and its new value. */
type Patch = { status: string } | { priority: string } | { assignee: string };

interface Edits {
  /** Whether this reader may change a row at all. */
  can: boolean;
  /** Change one field of one row, conditional on the version it was drawn at. */
  edit: (row: WorkSummary, patch: Patch, done: string) => void;
  busy: boolean;
}

const EditsContext = createContext<Edits | null>(null);

/**
 * The grid's one write, and the note that reports it — mounted around a grid
 * whose cells may edit.
 */
export function InlineEdits({
  children,
  seatName,
}: {
  children: ReactNode;
  seatName?: (handle: string) => string;
}) {
  const write = useAct("update_work_item");
  const [lost, setLost] = useState("");
  const edit = useCallback(
    (row: WorkSummary, patch: Patch, done: string) => {
      setLost("");
      void write
        .run({ item: itemAddress(row), if_match: row.version, ...patch }, { done })
        .then((result) => {
          if (result?.kind === "refused" && result.code === "stale_version") setLost(row.key);
        });
    },
    [write],
  );
  const value = useMemo(
    () => ({ can: write.access.can, edit, busy: write.busy }),
    [write.access.can, edit, write.busy],
  );
  return (
    <EditsContext.Provider value={value}>
      {lost && write.refusal ? (
        <ConflictNote
          item={lost}
          seatName={seatName}
          fallback={write.refusal.sentence}
          onDismiss={() => {
            setLost("");
            write.dismiss();
          }}
        />
      ) : write.access.can ? (
        <RefusalNote write={write} />
      ) : (
        // A READER'S ROWS DRAW VALUES, NOT PICKERS — a disabled picker in
        // every row would say one sentence a hundred times — so the sentence
        // is said once, here, and it is the one every other control uses.
        <p className="t-caption work-write-note">
          Status, priority and assignee are read-only here. {write.access.reason}
        </p>
      )}
      {children}
    </EditsContext.Provider>
  );
}

/**
 * "Changed by Maya since you opened it" — the refusal of a lost race, with the
 * name of whoever won it.
 */
export function ConflictNote({
  item,
  seatName,
  fallback,
  onDismiss,
}: {
  item: string;
  seatName?: (handle: string) => string;
  /** The engine's own sentence, drawn until the history answers. */
  fallback: string;
  onDismiss: () => void;
}) {
  const newest = useQuery("work_activity", { task: item, limit: 1 });
  const record = newest.data?.records?.[0];
  // WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person the directory binds to
  // a seat writes AS that seat, and anybody bound to none under their login,
  // which is a name and drawn as one.
  const actor = record?.actor ?? "";
  const who = actor ? (seatName?.(actor) ?? actor) : "";
  return (
    <span className="write-refusal" role="alert">
      <span>
        {who
          ? `${item} was changed by ${who} since you opened it. Look again, then retry.`
          : fallback}
      </span>
      <Button size="small" variant="ghost" onClick={onDismiss}>
        Dismiss
      </Button>
    </span>
  );
}

/** The writer's hook for a cell, or null outside [InlineEdits]. */
function useEdits(): Edits | null {
  const edits = useContext(EditsContext);
  return edits?.can ? edits : null;
}

/**
 * A writer's picker, drawn AS THE VALUE: the kit's menu, whose trigger is the
 * badge, the mark or the face the row always drew — so a writer's row reads
 * exactly like a reader's, and the choices open under the value they change.
 * A menu of choices rather than a select box, because a box with its own
 * border and chevron in every row of a hundred reads as a form, not a list.
 */
function CellPicker({
  label,
  value,
  options,
  trigger,
  onPick,
  busy,
}: {
  label: string;
  value: string;
  options: readonly { value: string; label: string }[];
  trigger: ReactNode;
  onPick: (value: string) => void;
  busy: boolean;
}) {
  return (
    <span className="cell-edit">
      <Menu
        label={label}
        // THE TRIGGER IS NAMED BY ITS FIELD AND ITS ROW, not by the value alone:
        // a hundred buttons each announced "To do" are a hundred identical
        // controls to a screen reader listing the page's buttons.
        trigger={
          <>
            <span className="sr-only">{label}: </span>
            {trigger}
          </>
        }
        items={options.map((option) => ({
          key: option.value || "-",
          label: option.label,
          checked: option.value === value,
          disabled: busy,
          onSelect: () => {
            if (option.value !== value) onPick(option.value);
          },
        }))}
      />
    </span>
  );
}

/** A status cell: the badge, or for a writer the picker drawn as the badge. */
export function StatusCell({ row, defs }: { row: WorkSummary; defs?: WorkStatusDef[] }) {
  const edits = useEdits();
  const badge = <StatusBadge status={row.status} defs={defs} />;
  if (!edits) return badge;
  return (
    <CellPicker
      label={`Status of ${row.key}`}
      value={row.status}
      options={STATUSES.map((st) => ({ value: st.value, label: statusLabel(st.value, defs) }))}
      trigger={badge}
      busy={edits.busy}
      onPick={(next) =>
        edits.edit(row, { status: next }, `Moved ${row.key} to ${statusLabel(next, defs)}`)
      }
    />
  );
}

/** A priority cell: the mark, or for a writer the picker drawn as the mark. */
export function PriorityCell({ row, word }: { row: WorkSummary; word?: boolean }) {
  const edits = useEdits();
  if (!edits) return <PriorityMark priority={row.priority} word={word} />;
  const current = row.priority ?? "none";
  return (
    <CellPicker
      label={`Priority of ${row.key}`}
      value={current}
      options={PRIORITY_OPTIONS}
      busy={edits.busy}
      trigger={
        current === "none" ? (
          word ? (
            <span className="t-caption">No priority</span>
          ) : (
            <EmptyValue label="No priority" />
          )
        ) : (
          <PriorityMark priority={current} word={word} />
        )
      }
      onPick={(next) => edits.edit(row, { priority: next }, `Set ${row.key} to ${next} priority`)}
    />
  );
}

const PRIORITY_OPTIONS = PRIORITIES.map((p) => ({
  value: p,
  label: p === "none" ? "No priority" : humanize(p),
}));

/** The value the "nobody" option carries: an empty handle is an unassignment. */
const NOBODY = "";

/** An assignee cell: the badge (or the name), or for a writer the picker. */
export function AssigneeCell({
  row,
  chrome,
  seats,
  named,
  readOnly,
}: {
  row: WorkSummary;
  chrome: RowChrome;
  /** Everybody who can hold a task, in the chart's order. */
  seats: readonly { handle: string; name: string; human?: boolean }[];
  /** The table prints the name; the list draws the badge alone. */
  named?: boolean;
  /** What a reader who cannot change it sees. */
  readOnly: ReactNode;
}) {
  const edits = useEdits();
  if (!edits) return <>{readOnly}</>;
  const current = row.assignee ?? NOBODY;
  return (
    <CellPicker
      label={`Assignee of ${row.key}`}
      value={current}
      options={[
        { value: NOBODY, label: "Nobody" },
        ...seats.map((st) => ({ value: st.handle, label: st.name })),
      ]}
      busy={edits.busy}
      trigger={
        <Assignee
          handle={row.assignee}
          seatName={chrome.seatName}
          seatKind={chrome.seatKind}
          name={named}
        />
      }
      onPick={(next) => {
        const name = seats.find((st) => st.handle === next)?.name ?? next;
        edits.edit(
          row,
          { assignee: next },
          next === NOBODY ? `Unassigned ${row.key}` : `Assigned ${row.key} to ${name}`,
        );
      }}
    />
  );
}
