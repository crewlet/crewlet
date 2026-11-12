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
 * `stale_version`, and [WorkConflictNote] names who won it from the task's own
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
 *
 * # And one draft for every editor open on it
 *
 * The title and the description are each edited in place (`Body.tsx`), and
 * with both open the person's assistant has to see and fill both. The page
 * context carries one draft, so the editors do not each declare their own,
 * which hid the other at every keystroke: each puts itself into the task's
 * boxes (`app/assistant/boxes.ts`), and the page's edits declare ONE draft
 * whose fields are the editors open now, held while the one write is out.
 *
 * # Every inline editor behaves one way ([useInlineEdit])
 *
 * Each had its own draft and its own ways out, and only the title answered
 * Escape. Now Escape cancels and asks first if the draft changed, every close
 * hands focus back to the pencil that opened it, and the rail's field editors
 * share one place, so opening one closes the last instead of stacking a second
 * Save and Cancel. The title and the description keep a place each, because the
 * two open together are the draft described above.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
} from "react";
import { ConfirmModal, isComposing } from "@crewlethq/ui";
import { DraftBoxes, DraftBoxesContext, boxesDraft, useOpenBoxes } from "~/app/assistant/boxes.ts";
import {
  latestWrite,
  useAssistantDraft,
  useDraftState,
  writeVia,
  type DraftSetter,
  type DraftWrite,
} from "~/app/assistant/draft.ts";
import { RefusalNote } from "~/components/WriteButton.tsx";
import { WorkConflictNote } from "../shapes/cells.tsx";
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
  /**
   * The task's ADDRESS ([itemAddress]) — what every change names it by: its
   * key, or its id where another task claimed that key first and the key
   * would change the claimant.
   */
  item: string;
  /** What a sentence calls the task: its key, whichever address it is changed by. */
  label: string;
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
  label,
  version,
  seatName,
  children,
}: {
  /**
   * The task's address ([itemAddress]), never its bare key: on a task whose
   * key another task claimed first, the key would send every field, tick and
   * watch on this page to the claimant.
   */
  item: string;
  /** The task's key, for the sentences that name it. */
  label: string;
  /** The `task.version` the page is drawn from. */
  version: number;
  seatName?: (handle: string) => string;
  children: ReactNode;
}) {
  const write = useAct("update_work_item");
  const [lost, setLost] = useState(false);
  const [boxes] = useState(() => new DraftBoxes());
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
      label,
      lost,
      forget,
      seatName,
    }),
    [write, edit, item, label, lost, forget, seatName],
  );
  return (
    <Ctx.Provider value={value}>
      <DraftBoxesContext.Provider value={boxes}>
        <InlineEdits item={item}>{children}</InlineEdits>
        <ItemDraft boxes={boxes} label={label} version={version} busy={write.busy} />
      </DraftBoxesContext.Provider>
    </Ctx.Provider>
  );
}

/** The one place the rail's field editors open in: see [useInlineEdit]. */
export const RAIL_FIELD = "rail-field";

/** An editor open on the page, and the value it opened on. */
interface OpenEdit {
  id: string;
  /** What the question calls it: "the due date". */
  words: string;
  draft: unknown;
  initial: unknown;
}

const isChanged = (edit: OpenEdit) => JSON.stringify(edit.draft) !== JSON.stringify(edit.initial);

/** Every editor open on the task, by the place it opens in. */
type OpenEdits = Readonly<Record<string, OpenEdit>>;

interface InlineEditStore {
  open: OpenEdits;
  /**
   * What every open editor holds, in branded state: the title and the
   * description are fields of the page's one draft, and a modal handed what
   * writes them is part of the page (`draftGate.test.ts`).
   */
  held: DraftSetter<OpenEdits>;
  /** The editor whose pencil takes focus back, once it has closed. */
  returning: string | null;
  start: (slot: string, edit: OpenEdit) => void;
  close: (slot: string, id: string) => void;
  dismiss: (slot: string) => void;
  returned: () => void;
}

const InlineEditsContext = createContext<InlineEditStore | null>(null);

/** Which editors are open on the task, and the one question about discarding. */
function InlineEdits({ item, children }: { item: string; children: ReactNode }) {
  const [open, setOpen] = useDraftState<OpenEdits>({});
  const [returning, setReturning] = useState<string | null>(null);
  const [asking, setAsking] = useState<{ slot: string; next?: OpenEdit } | null>(null);
  // A DRAFT BELONGS TO THE TASK IT WAS TYPED ON: stepping a peek keeps this
  // provider mounted on the next task, and Save would have sent the draft there.
  const [task, setTask] = useState(item);
  if (task !== item) {
    setTask(item);
    setOpen({});
    setReturning(null);
    setAsking(null);
  }
  const latest = useRef(open);
  latest.current = open;
  const store = useMemo<InlineEditStore>(() => {
    const without = (slot: string) => {
      const { [slot]: gone, ...rest } = latest.current;
      return gone ? rest : latest.current;
    };
    return {
      open,
      held: setOpen,
      returning,
      start: (slot, edit) => {
        const current = latest.current[slot];
        if (current && current.id !== edit.id && isChanged(current)) {
          setAsking({ slot, next: edit });
        } else {
          setOpen({ ...latest.current, [slot]: edit });
        }
      },
      // ONLY THE EDITOR STILL OPEN THERE: a save can land after the reader has
      // moved on to another field, and focus must not jump back from it.
      close: (slot, id) => {
        if (latest.current[slot]?.id !== id) return;
        setOpen(without(slot));
        setReturning(id);
      },
      dismiss: (slot) => {
        const current = latest.current[slot];
        if (!current) return;
        if (isChanged(current)) {
          setAsking({ slot });
        } else {
          setOpen(without(slot));
          setReturning(current.id);
        }
      },
      returned: () => setReturning(null),
    };
  }, [open, setOpen, returning]);
  const words = asking ? open[asking.slot]?.words : undefined;
  return (
    <InlineEditsContext.Provider value={store}>
      {children}
      <ConfirmModal
        open={asking !== null}
        destructive
        title="Discard this change?"
        message={words ? `Your change to ${words} is not saved.` : undefined}
        cancelLabel="Keep editing"
        confirmLabel="Discard"
        onClose={() => setAsking(null)}
        onConfirm={() => {
          if (!asking) return;
          if (asking.next) setOpen({ ...latest.current, [asking.slot]: asking.next });
          else store.close(asking.slot, open[asking.slot]?.id ?? "");
          setAsking(null);
        }}
      />
    </InlineEditsContext.Provider>
  );
}

/** One inline editor on the page, from its pencil to its close. */
export interface InlineEdit<T> {
  /** What is being typed, or null while the editor is shut. */
  draft: T | null;
  /** The pencil's press, with the value it opens on. */
  start: (initial: T) => void;
  /** What is typed, written into the page's branded state while this editor is the one open in its place. */
  set: DraftWrite<T>;
  /** Shut it, as a Cancel or a finished save does. */
  close: () => void;
  /** Send a change and shut the editor only once it has landed; a refusal leaves it open. */
  save: (pending: Promise<ActResult | null> | undefined) => Promise<void>;
  /** For the editor's own element: Escape cancels, asking first if the draft changed. */
  keys: (event: KeyboardEvent<HTMLElement>) => void;
  /** The pencil, which takes focus back when the editor shuts. */
  pencil: RefObject<HTMLButtonElement | null>;
}

/**
 * The one way a field of the task is edited in place.
 *
 * `slot` is where it opens: editors sharing one ([RAIL_FIELD]) replace each
 * other, and a replaced draft that changed is asked about first. Outside
 * [ItemEditsProvider] it stays shut, as nothing there draws a pencil.
 */
export function useInlineEdit<T>(id: string, words: string, slot: string = id): InlineEdit<T> {
  const store = useContext(InlineEditsContext);
  const placed = store?.open[slot];
  const mine = placed?.id === id ? placed : undefined;
  const pencil = useRef<HTMLButtonElement>(null);
  const returning = store?.returning === id && !mine;
  // A PASSIVE EFFECT, AFTER THE COMMIT: a Discard closes the kit's dialog in the
  // same commit, and its own focus return runs in a passive cleanup, before this.
  useEffect(() => {
    if (!returning) return;
    store?.returned();
    pencil.current?.focus();
  }, [returning, store]);
  const held = store?.held;
  const set = useMemo(
    () =>
      held
        ? writeVia(held, (next: T, open: OpenEdits) => {
            const edit = open[slot];
            return edit?.id === id ? { ...open, [slot]: { ...edit, draft: next } } : open;
          })
        : // OUTSIDE A PROVIDER nothing is open, so nothing is written.
          latestWrite<T>(() => undefined),
    [held, slot, id],
  );
  const close = useCallback(() => store?.close(slot, id), [store, slot, id]);
  return {
    draft: mine ? (mine.draft as T) : null,
    start: (initial) => store?.start(slot, { id, words, draft: initial, initial }),
    set,
    close,
    save: async (pending) => {
      const result = await pending;
      if (result && (result.kind === "applied" || result.kind === "pending")) close();
    },
    keys: (event) => {
      if (event.key !== "Escape" || event.defaultPrevented || isComposing(event)) return;
      // ANSWERED HERE, so a peek around the editor does not close on it too.
      event.preventDefault();
      store?.dismiss(slot);
    },
    pencil,
  };
}

/**
 * The task's one draft for the person's own assistant: every editor open on
 * it, its title, its description or both, which the editors put themselves
 * into (`Body.tsx`). Held while the page's one write is out, as each
 * editor's Save is.
 */
function ItemDraft({
  boxes,
  label,
  version,
  busy,
}: {
  boxes: DraftBoxes;
  label: string;
  version: number;
  busy: boolean;
}) {
  const open = useOpenBoxes(boxes);
  const names = open.boxes.map((box) => box.label.toLowerCase()).join(" and ");
  useAssistantDraft(
    boxesDraft(open, {
      id: label,
      revision: version,
      scope: `The ${names} of ${label} you are editing (not saved)`,
      help:
        `Every editor open on ${label} is a field: its title, its description or both. ` +
        "You can propose new text for any of them; Save stays the person's. Any other " +
        "field of the task is changed with update_task.",
      blocked: () => (busy ? "The task is being saved." : null),
    }),
  );
  return null;
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
      <WorkConflictNote
        item={edits.item}
        label={edits.label}
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
