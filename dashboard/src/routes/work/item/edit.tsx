/**
 * The task page's one change: `update_work_item`, as the signed-in person.
 *
 * # One write for the page, and its refusal in one place
 *
 * Every field the page offers — the status, the priority, the labels, the due
 * date, the size, the title and description, a checklist tick, following it —
 * is the same tool, so the page holds ONE [useAct] and every control sends
 * through it ([ItemEdits]). A refusal is then one sentence under the header
 * rather than a dozen fields each able to grow an error, and a second press
 * replaces the first one's answer the way a single control's does. The one
 * exception is the hand-off, which has a dialog of its own with its reason
 * and its budget (`components/writes.tsx`).
 *
 * # Conditional on the version the page is drawn from
 *
 * A field is changed looking at this task as it is on screen, so every edit
 * carries `if_match: task.version`. A race lost to somebody else is refused
 * `stale_version`, and [ConflictNote] names who won it from the task's own
 * history — the person to talk to. The refusal has already asked the task
 * again (`protocol/act.ts`), so the page redraws at the version that won and
 * the next press is made against it.
 *
 * A CHECKLIST TICK IS THE EXCEPTION, deliberately: a tick is a GESTURE the
 * engine resolves against the checklists as they are when it lands
 * (`update_work_item{checklist}` — "somebody else's tick in the meantime is
 * kept"), so conditioning it on the whole task's version would refuse a tick
 * because an agent moved the status, which is a race that never was.
 *
 * # A reader who cannot change it is told once, and sees values
 *
 * The same rule the list's inline cells keep: a disabled picker on every row
 * says one sentence eight times. The reason is drawn once, where the
 * refusal would be, and every value renders as the value.
 */

import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { RefusalNote } from "~/components/WriteButton.tsx";
import { ConflictNote } from "../shapes/cells.tsx";
import { useAct, type Act } from "~/lib/useAct.ts";
import type { ActionArgs, ActResult } from "~/protocol/act.ts";

/** The fields a page control changes, beside the item and its version. */
export type ItemPatch = Omit<ActionArgs<"update_work_item">, "item" | "if_match">;

export interface ItemEdits {
  /** Whether this reader may change the task at all. */
  can: boolean;
  /** Why not, in the sentence every write control uses. */
  reason: string;
  busy: boolean;
  /**
   * Change the task. `conditional` false sends no `if_match` — for a gesture
   * the engine resolves as it lands (a checklist tick). A conditional change
   * made while a press is out sends nothing and resolves with null.
   */
  edit: (patch: ItemPatch, done: string, conditional?: boolean) => Promise<ActResult | null>;
  write: Act<"update_work_item">;
  /** The task's key. */
  item: string;
  /** Whether the last press lost a race, which [ItemEditNote] names. */
  lost: boolean;
  forget: () => void;
  seatName?: (handle: string) => string;
}

const Ctx = createContext<ItemEdits | null>(null);

/** The page's edits, or null outside [ItemEditsProvider]. */
export function useItemEdits(): ItemEdits | null {
  return useContext(Ctx);
}

export function ItemEditsProvider({
  item,
  version,
  seatName,
  children,
}: {
  /** The task's key. */
  item: string;
  /** The `task.version` the page is drawn from. */
  version: number;
  seatName?: (handle: string) => string;
  children: ReactNode;
}) {
  const write = useAct("update_work_item");
  const [lost, setLost] = useState(false);
  const edit = useCallback(
    async (patch: ItemPatch, done: string, conditional = true) => {
      // A CONDITIONAL EDIT WHILE A PRESS IS OUT IS NOT SENT: it carries the
      // version on screen, which the press already out is about to move, so
      // the engine could only refuse it — and the page would then report the
      // person's own first press as somebody else's change. The title and a
      // date are forms Enter submits, and a second Enter did exactly that. A
      // gesture sent without a version (a tick, a watch) composes with one in
      // flight and still goes.
      if (conditional && write.busy) return null;
      setLost(false);
      const result = await write.run(
        { item, ...(conditional ? { if_match: version } : {}), ...patch },
        { done },
      );
      if (result?.kind === "refused" && result.code === "stale_version") setLost(true);
      return result;
    },
    [write, item, version],
  );
  const forget = useCallback(() => {
    setLost(false);
    write.dismiss();
  }, [write]);
  const value = useMemo<ItemEdits>(
    () => ({
      can: write.access.can,
      reason: write.access.can ? "" : write.access.reason,
      busy: write.busy,
      edit,
      write,
      item,
      lost,
      forget,
      seatName,
    }),
    [write, edit, item, lost, forget, seatName],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

/**
 * What became of the page's last change, or — for a reader who cannot make
 * one — why, said once.
 */
export function ItemEditNote({ readOnly }: { readOnly?: string }) {
  const edits = useItemEdits();
  if (!edits) return null;
  if (edits.lost && edits.write.refusal) {
    return (
      <ConflictNote
        item={edits.item}
        seatName={edits.seatName}
        fallback={edits.write.refusal.sentence}
        onDismiss={edits.forget}
      />
    );
  }
  if (!edits.can && readOnly) {
    return <p className="t-caption task-readonly">{`${readOnly} ${edits.reason}`}</p>;
  }
  return <RefusalNote write={edits.write} />;
}
