/**
 * What a task's own menu does to it: moved to another project, into the
 * trash, back out of it, and out of the company for good.
 *
 * # Four gestures, two surfaces
 *
 * A move to another project is `move_work_item`, one top-level task and its
 * subtree, re-keyed where it lands: its project's lead's, as the engine decides
 * on the stored row, so a subtask's entry is held (it moves with its parent)
 * and every other refusal is the engine's, drawn in the dialog.
 *
 * Moving a task to the trash and restoring it are tools (`remove_work_item`,
 * `restore_work_item`), made through `useAct` as the reader (ADR-0024): the
 * trash is the task's reporter's, its project lead's or an administrator's,
 * decided by the engine on the stored row, so nothing here guesses who may.
 * Deleting it permanently is no tool at all: it is the human write surface's
 * `POST /worklet/items/{key}/purge`, an administrator's, a person's and a
 * recent proof's, sent as a keyed REST write (`lib/keyedWrite.ts`) whose
 * step-up the REST client asks for and replays.
 *
 * ONE HOOK FOR EVERY PLACE THAT OFFERS THEM ([useTaskActions]), because a task
 * is trashed from its page's header, from each row's menu on every shape and
 * from a selection's bar, and the question, the toast and the refusal must be
 * one wording wherever the press was made. A screen mounts the hook once and
 * draws its [TaskActions.dialog]; each row's menu asks the same hook for its
 * entries ([taskEntries]), so a hundred rows hold no hundred writes.
 *
 * # Subtasks are asked about, never assumed
 *
 * A removal takes the task alone unless it is asked for `subtree`, and a task
 * left behind by its parent stands as the root of what is left on every board
 * it is on. Neither answer is right for every case, so where a task HAS
 * subtasks the reader is asked; where none has, the removal is made at once,
 * since the Undo on its toast is the confirmation a reversible change needs.
 * Whether a row has subtasks is not on the row, so it is asked of the engine
 * when the press is made: one listing of the tasks named, narrowed to those
 * with children ([subtreeProbe]).
 *
 * # Undo is the restore, which brings back exactly what the removal took
 *
 * Each removal's tombstone names the gesture that made it, so restoring the
 * task brings back its subtasks too where the removal took them, and never a
 * subtask that was already in the trash for its own reasons. The toast's Undo
 * is therefore `restore_work_item` on each task the press removed, and no
 * record of what went with it is kept here.
 *
 * # Deleting permanently says what is lost before it is asked for
 *
 * The confirmation names the task and every part of it that goes (the words
 * are `docs/guides/worklet.md`'s account of a purge), asks for the reason the
 * project's lead is told, and is made by typing the task's KEY, which is the
 * engine's own confirmation (`?confirm=`): it is checked against the task the
 * address resolves to, so a confirmation naming another task destroys nothing.
 */

import { useCallback, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  ConfirmField,
  FormField,
  Menu,
  Select,
  SelectionBar,
  Textarea,
  confirmMatches,
  useToast,
  type MenuEntry,
  type MenuItem,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { EllipsisGlyph, FolderInputGlyph, TrashGlyph, UndoGlyph } from "@crewlethq/icons/glyphs";
import { UNKNOWN_TITLE, useAct, type Act } from "~/lib/useAct.ts";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import { projectOptions } from "~/lib/startingProject.ts";
import { PROJECT_PAGE } from "~/contract/work.ts";
import { accessToAsk } from "~/lib/authorityRemedy.ts";
import { useKeyedGesture, type KeyedRequest } from "~/lib/keyedWrite.ts";
import { WRITE_REASONS, linkOf, type WriteAccess } from "~/lib/useWriteAccess.ts";
import { needsWords } from "~/lib/access.ts";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { itemAddress } from "~/lib/work.ts";
import { pathSegment } from "~/protocol/index.ts";
import { session } from "~/protocol/session.ts";
import type { ActResult } from "~/protocol/act.ts";
import type { WorkSummary } from "~/protocol/index.ts";

/** A task one of the gestures names. */
export interface TaskTarget {
  /** The task's id, which names it and no other. */
  id: string;
  /** Its key, as a person reads it and as a purge is confirmed by. */
  key: string;
  /** Its address ([itemAddress]): the key, or the id where another task claimed that key first. */
  address: string;
  title: string;
  /** The project it is filed in, which a move takes it out of. */
  project: string;
  /** Its parent's id, for a subtask: a subtask moves only with its parent. */
  parent?: string;
}

/** The target a row names. */
export function taskTargetOf(
  row: Pick<WorkSummary, "id" | "key" | "title" | "key_collision" | "project" | "parent">,
): TaskTarget {
  return {
    id: row.id,
    key: row.key,
    address: itemAddress(row),
    title: row.title,
    project: row.project,
    ...(row.parent ? { parent: row.parent } : {}),
  };
}

/** Why a subtask's Move is held: the tool moves a top-level item and its subtree. */
export const SUBTASK_MOVES_WITH_PARENT =
  "A subtask moves with its parent: move the parent instead.";

/** How many tasks a selection says it acts on, in the toast and the bar. */
function named(targets: readonly TaskTarget[]): string {
  return targets.length === 1 ? targets[0]!.key : `${targets.length} tasks`;
}

/**
 * Why a reader may not delete a task permanently, or undefined where the
 * engine may admit them. The purge is admin's and never an agent's
 * (`authz.ActionWorkPurge`); the proof's age is the engine's to ask, which the
 * REST client does by asking the person to confirm who they are.
 */
export function purgeHold(
  viewer: ViewerState,
  link: ReturnType<typeof linkOf>,
): string | undefined {
  if (link !== "open") return WRITE_REASONS[link];
  if (viewer.loading) return WRITE_REASONS.loading;
  if (viewer.anonymous) return WRITE_REASONS.anonymous;
  if (!viewer.admin) return needsWords("Deleting a task permanently", ["admin"]);
  if (viewer.kind === "agent") return "Only a person can delete a task permanently.";
  return undefined;
}

/** The sentence a removal or a restore is held with, where the reader cannot act. */
function heldBy(access: WriteAccess): string | undefined {
  return access.can ? undefined : access.reason;
}

/** A refused press, in the engine's words: the access that admits, where access is the way past. */
function refusalWords(result: Extract<ActResult, { kind: "refused" }>): string {
  const said = accessToAsk(result) ?? result.sentence;
  return result.hint ? `${said} ${result.hint}` : said;
}

/**
 * The purge request: the task by its address, its KEY repeated as the
 * engine's confirmation, and the reason its project's lead is told. No body
 * argument: the route reads both from the query and refuses any argument.
 */
export function purgeRequest(target: TaskTarget, reason: string): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: `/worklet/items/${pathSegment(target.address)}/purge`,
    query: { confirm: target.key, reason: reason.trim() },
    body: {},
  };
}

/**
 * The question that says which of the tasks named have subtasks: one listing
 * of exactly those keys, each task filtered on its own (`subtasks=separate`,
 * so a subtask named is not hidden under its root), whatever its status, and
 * narrowed to the tasks with children. Matched back BY ID, since a key two
 * tasks hold answers both.
 */
export function subtreeProbe(targets: readonly TaskTarget[]): Record<string, unknown> {
  return {
    container: "workspace",
    key: [...new Set(targets.map((t) => t.key))].join(","),
    has_children: true,
    subtasks: "separate",
    show_closed: "true",
    limit: Math.max(targets.length, 1),
  };
}

/** One press of a batch: the task, its arguments, and the key of the press it repeats. */
interface Press {
  target: TaskTarget;
  args: { item: string; subtree?: boolean };
  opId?: string;
}

/** What one batch of presses came to. */
interface Outcome {
  done: TaskTarget[];
  unknown: (Press & { opId: string; reason: string })[];
  refused: { target: TaskTarget; sentence: string }[];
}

/** The pending subtree question: the tasks pressed on, and which of them have subtasks. */
interface SubtreeAsk {
  targets: TaskTarget[];
  parents: ReadonlySet<string>;
}

export interface TaskActions {
  /** Moves the tasks to the trash, asking first where any has subtasks. */
  remove: (targets: readonly TaskTarget[], known?: { subtasks: boolean }) => void;
  /** Brings them back, with whatever their removals took. */
  restore: (targets: readonly TaskTarget[]) => void;
  /** Opens the permanent delete's confirmation for one task. */
  purge: (target: TaskTarget) => void;
  /** Opens the move to another project for one top-level task. */
  move: (target: TaskTarget) => void;
  moveHeld: string | undefined;
  /** Why a removal cannot be made from this browser, or undefined. */
  removeHeld: string | undefined;
  restoreHeld: string | undefined;
  purgeHeld: string | undefined;
  /** A press is out. */
  busy: boolean;
  /** The question and the confirmation, mounted once by the screen holding the hook. */
  dialog: ReactNode;
}

/**
 * Every gesture a task's menu offers, as the signed-in reader. See the file's
 * doc for why one hook serves the page, every row and the selection bar.
 */
export function useTaskActions(): TaskActions {
  const removeWrite = useAct("remove_work_item");
  const restoreWrite = useAct("restore_work_item");
  const moveWrite = useAct("move_work_item");
  const [moving, setMoving] = useState<TaskTarget | null>(null);
  const viewer = useViewer();
  const link = linkOf(useConnection());
  const { socket } = useClient();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [asking, setAsking] = useState<SubtreeAsk | null>(null);
  const [purging, setPurging] = useState<TaskTarget | null>(null);
  // THE LATEST WRITES, read by a toast's Undo or Retry pressed long after the
  // render that made it, as `useAct` reads whether the browser may act.
  const writes = useRef({ removeWrite, restoreWrite });
  useLayoutEffect(() => {
    writes.current = { removeWrite, restoreWrite };
  }, [removeWrite, restoreWrite]);

  /**
   * Sends one press per task, one after the other, each under a fresh
   * operation key or the key of the press it repeats.
   */
  const batch = useCallback(
    async (
      tool: "remove_work_item" | "restore_work_item",
      presses: readonly Press[],
    ): Promise<Outcome> => {
      const out: Outcome = { done: [], unknown: [], refused: [] };
      setBusy(true);
      try {
        for (const press of presses) {
          const write =
            tool === "remove_work_item" ? writes.current.removeWrite : writes.current.restoreWrite;
          const result = await write.run(
            press.args,
            { done: press.target.key, quiet: true },
            press.opId ? { opId: press.opId } : undefined,
          );
          // A REFUSAL IS SAID IN THE TOAST rather than held on the hook for
          // a control to draw: the press came from a menu or a bar, and
          // nothing stays beside it to draw it.
          write.dismiss();
          if (result === null) continue;
          switch (result.kind) {
            case "applied":
            case "pending":
              out.done.push(press.target);
              break;
            case "unknown":
              out.unknown.push({ ...press, opId: result.opId, reason: result.reason });
              break;
            case "refused":
              out.refused.push({ target: press.target, sentence: refusalWords(result) });
              break;
          }
        }
      } finally {
        setBusy(false);
      }
      return out;
    },
    [],
  );

  /**
   * Sends a batch and says what came of it in one toast: what landed, with
   * an Undo where the gesture has an inverse; what nobody can confirm, with
   * a Retry under the same operation keys; and every refusal in the engine's
   * words.
   */
  const send = useCallback(
    async (tool: "remove_work_item" | "restore_work_item", presses: readonly Press[]) => {
      const out = await batch(tool, presses);
      const removing = tool === "remove_work_item";
      const verb = removing ? "Moved" : "Restored";
      const where = removing ? " to the trash" : "";
      const done = out.done.length ? `${verb} ${named(out.done)}${where}.` : "";
      const refused = out.refused.map((r) => `${r.target.key}: ${r.sentence}`).join(" ");
      // THE UNDO OF A REMOVAL IS THE RESTORE OF WHAT IT TOOK, which each
      // removal's tombstone names; see the file's doc.
      const undo =
        removing && out.done.length
          ? {
              action: {
                label: "Undo",
                onClick: () =>
                  void send(
                    "restore_work_item",
                    out.done.map((target) => ({ target, args: { item: target.address } })),
                  ),
              },
            }
          : {};
      if (out.unknown.length) {
        toast.show({
          variant: "warning",
          title: UNKNOWN_TITLE,
          message: [
            done,
            `${named(out.unknown.map((u) => u.target))}: ${out.unknown[0]!.reason}`,
            refused,
          ]
            .filter(Boolean)
            .join(" "),
          duration: 0,
          action: { label: "Retry", onClick: () => void send(tool, out.unknown) },
        });
        return;
      }
      if (!out.refused.length) {
        if (out.done.length) toast.ok(done.slice(0, -1), undo);
        return;
      }
      // REFUSED, IN WHOLE OR IN PART: the toast stays until it is read, and
      // what did land keeps its Undo.
      toast.show({
        variant: out.done.length ? "warning" : "danger",
        title: out.done.length
          ? `${verb} ${out.done.length} of ${out.done.length + out.refused.length}${where}`
          : removing
            ? "Not moved to the trash"
            : "Not restored",
        message: [done, refused].filter(Boolean).join(" "),
        duration: 0,
        ...undo,
      });
    },
    [batch, toast],
  );

  const restore = useCallback(
    (targets: readonly TaskTarget[]) =>
      void send(
        "restore_work_item",
        targets.map((target) => ({ target, args: { item: target.address } })),
      ),
    [send],
  );

  const removeNow = useCallback(
    (targets: readonly TaskTarget[], parents: ReadonlySet<string>, subtree: boolean) =>
      void send(
        "remove_work_item",
        targets.map((target) => ({
          target,
          args: {
            item: target.address,
            // ONLY WHERE THERE IS A SUBTREE TO TAKE: a task with no subtasks
            // removed with `subtree` is the same removal, decided as its
            // lead's rather than as its reporter's.
            ...(subtree && parents.has(target.id) ? { subtree: true } : {}),
          },
        })),
      ),
    [send],
  );

  const remove = useCallback(
    (targets: readonly TaskTarget[], known?: { subtasks: boolean }) => {
      if (targets.length === 0) return;
      const list = [...targets];
      // THE CALLER KNOWS, on a task's own page, which reads its subtasks.
      if (known && list.length === 1) {
        if (known.subtasks) setAsking({ targets: list, parents: new Set([list[0]!.id]) });
        else removeNow(list, new Set(), false);
        return;
      }
      setBusy(true);
      socket
        .query("work_items", withFloor("work_items", JSON.stringify(subtreeProbe(list))))
        .then((answer) => {
          const ids = new Set(list.map((t) => t.id));
          const parents = new Set(
            (answer?.items ?? []).map((row) => row.id).filter((id) => ids.has(id)),
          );
          setBusy(false);
          if (parents.size > 0) setAsking({ targets: list, parents });
          else removeNow(list, parents, false);
        })
        .catch(() => {
          // THE QUESTION WENT UNANSWERED, so whether any has subtasks is
          // not known: the reader is asked rather than a removal guessed.
          setBusy(false);
          setAsking({ targets: list, parents: new Set(list.map((t) => t.id)) });
        });
    },
    [socket, removeNow],
  );

  const dialog = (
    <>
      {asking && (
        <SubtreeDialog
          ask={asking}
          busy={busy}
          onClose={() => setAsking(null)}
          onChoose={(subtree) => {
            setAsking(null);
            removeNow(asking.targets, asking.parents, subtree);
          }}
        />
      )}
      {moving && (
        <MoveDialog
          key={moving.id}
          target={moving}
          write={moveWrite}
          onClose={() => {
            moveWrite.dismiss();
            setMoving(null);
          }}
        />
      )}
      {purging && (
        <PurgeDialog
          key={purging.id}
          target={purging}
          onClose={() => setPurging(null)}
          onPurged={() => {
            toast.ok(`Deleted ${purging.key} permanently`);
            setPurging(null);
          }}
        />
      )}
    </>
  );

  return {
    remove,
    restore,
    purge: setPurging,
    move: setMoving,
    moveHeld: heldBy(moveWrite.access),
    removeHeld: heldBy(removeWrite.access),
    restoreHeld: heldBy(restoreWrite.access),
    purgeHeld: purgeHold(viewer, link),
    busy: busy || removeWrite.busy || restoreWrite.busy,
    dialog,
  };
}

/**
 * One entry of a task's menu: a kit menu item whose description is a sentence,
 * so the same entries fold into a phone's page menu (`PageMenuEntry`).
 */
export type TaskMenuItem = MenuItem & { description?: string };

/**
 * The menu entries for the tasks named: Move to project and Move to trash for
 * live work, and Restore and Delete permanently in the trash. Each is drawn for
 * every reader and held with the sentence that says why it cannot act.
 */
export function taskEntries(
  actions: TaskActions,
  targets: readonly TaskTarget[],
  inTrash: boolean,
  known?: { subtasks: boolean },
): TaskMenuItem[] {
  if (!inTrash) {
    // A MOVE IS ONE TOP-LEVEL TASK, which takes its subtree with it.
    const one = targets.length === 1 ? targets[0]! : null;
    return [
      ...(one
        ? [
            {
              key: "move",
              label: "Move to project…",
              icon: <FolderInputGlyph size="sm" />,
              onSelect: () => actions.move(one),
              ...held(one.parent ? SUBTASK_MOVES_WITH_PARENT : actions.moveHeld),
            },
          ]
        : []),
      {
        key: "trash",
        label: "Move to trash",
        icon: <TrashGlyph size="sm" />,
        onSelect: () => actions.remove(targets, known),
        ...held(actions.removeHeld),
      },
    ];
  }
  const out: TaskMenuItem[] = [
    {
      key: "restore",
      label: "Restore",
      icon: <UndoGlyph size="sm" />,
      onSelect: () => actions.restore(targets),
      ...held(actions.restoreHeld),
    },
  ];
  // ONE TASK AT A TIME, because the confirmation is that task's key typed
  // by the person deleting it.
  if (targets.length === 1) {
    out.push({
      key: "purge",
      label: "Delete permanently",
      icon: <TrashGlyph size="sm" />,
      danger: true,
      onSelect: () => actions.purge(targets[0]!),
      ...held(actions.purgeHeld),
    });
  }
  return out;
}

/** A menu entry held as its inline control is, with the same sentence (`menuHold`'s shape). */
function held(reason: string | undefined): { disabled?: boolean; description?: string } {
  return reason ? { disabled: true, description: reason } : {};
}

/**
 * The ellipsis a row of any shape carries: its own entries (`extra`, such as
 * "Open"), then the trash's. ABOVE THE ROW'S LINK (`data-above-row`), so a
 * press opens the menu rather than the row.
 */
export function TaskRowMenu({
  row,
  actions,
  inTrash,
  extra = [],
}: {
  row: Pick<WorkSummary, "id" | "key" | "title" | "key_collision" | "project" | "parent">;
  actions: TaskActions;
  inTrash: boolean;
  extra?: readonly MenuEntry[];
}) {
  const items: MenuEntry[] = [
    ...extra,
    ...(extra.length ? [{ kind: "separator" as const, key: "before-trash" }] : []),
    ...taskEntries(actions, [taskTargetOf(row)], inTrash),
  ];
  return (
    <span className="task-row-menu" data-above-row="">
      <Menu
        label={`More for ${row.key}`}
        icon={<EllipsisGlyph size="sm" />}
        triggerVariant="ghost"
        align="end"
        items={items}
      />
    </span>
  );
}

/**
 * What a selection of tasks can have done to it: moved to the trash, or, in
 * the trash, restored. A permanent delete is one task at a time (its key is
 * its confirmation), so it is on each row's menu and not here.
 */
export function TaskSelectionBar({
  selected,
  onClear,
  actions,
  inTrash,
}: {
  selected: readonly TaskTarget[];
  onClear: () => void;
  actions: TaskActions;
  inTrash: boolean;
}) {
  const count = selected.length;
  return (
    <SelectionBar
      size="compact"
      className="task-selection-bar"
      count={count}
      onClear={onClear}
      labels={{ label: "Selected tasks" }}
    >
      {inTrash ? (
        <Button
          tooltip
          size="small"
          variant="secondary"
          leadingIcon={<UndoGlyph size="sm" />}
          loading={actions.busy}
          disabledReason={actions.restoreHeld}
          onClick={() => {
            actions.restore(selected);
            onClear();
          }}
        >
          {`Restore ${count}`}
        </Button>
      ) : (
        <Button
          tooltip
          size="small"
          variant="secondary"
          leadingIcon={<TrashGlyph size="sm" />}
          loading={actions.busy}
          disabledReason={actions.removeHeld}
          onClick={() => {
            actions.remove(selected);
            onClear();
          }}
        >
          {`Move ${count} to trash`}
        </Button>
      )}
    </SelectionBar>
  );
}

/** Whether to take the subtasks with the task, asked before anything moves. */
function SubtreeDialog({
  ask,
  busy,
  onClose,
  onChoose,
}: {
  ask: SubtreeAsk;
  busy: boolean;
  onClose: () => void;
  onChoose: (subtree: boolean) => void;
}) {
  const { targets, parents } = ask;
  const one = targets.length === 1;
  const title = one
    ? `Move ${targets[0]!.key} to the trash?`
    : `Move ${targets.length} tasks to the trash?`;
  const which = one
    ? `${targets[0]!.key} has subtasks.`
    : `${parents.size} of these ${targets.length} tasks ${parents.size === 1 ? "has" : "have"} subtasks.`;
  return (
    <Modal
      open
      onClose={onClose}
      title={title}
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button size="small" variant="secondary" loading={busy} onClick={() => onChoose(false)}>
            {one ? "Only this task" : "Only these tasks"}
          </Button>
          <Button size="small" variant="primary" loading={busy} onClick={() => onChoose(true)}>
            With subtasks
          </Button>
        </>
      }
    >
      <p>
        {which} Taken with {one ? "it" : "them"}, they come back together on a restore. Left behind,
        each subtask stays on every list and board, standing on its own until{" "}
        {one ? "its parent comes" : "their parents come"} back.
      </p>
    </Modal>
  );
}

/**
 * Moving one top-level task to another project: the project chosen from the
 * company's active ones, and what the move does said before it is made.
 *
 * THE ENGINE DECIDES WHO MAY (the lead of the project the task is in, or an
 * administrator), and a refusal is drawn under the choice in its own words,
 * so the reader can pick again or close. The task is re-keyed in the project
 * it moves to, and its subtasks with it; the old keys still open them.
 */
function MoveDialog({
  target,
  write,
  onClose,
}: {
  target: TaskTarget;
  write: Act<"move_work_item">;
  onClose: () => void;
}) {
  const projects = useQuery("work_projects", { limit: PROJECT_PAGE }, { pollMs: 120_000 });
  const rows = useMemo(
    () => (projects.data?.projects ?? []).filter((p) => p.key !== target.project),
    [projects.data, target.project],
  );
  const [to, setTo] = useState("");
  const move = async () => {
    const result = await write.run(
      { item: target.address, project: to },
      {
        done: `Moved ${target.key} to ${to}`,
        // THE NEW KEY, which is how the task is found from now on.
        landed: (receipt) => {
          const key = (receipt as { key?: unknown } | null)?.key;
          return typeof key === "string" && key !== target.key ? `It is ${key} now.` : undefined;
        },
      },
    );
    if (result?.kind === "applied" || result?.kind === "pending") onClose();
  };
  return (
    <Modal
      open
      onClose={onClose}
      title={`Move ${target.key} to another project`}
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            size="small"
            variant="primary"
            showRefusal={false}
            blocked={to ? undefined : "Choose the project it moves to."}
            onPress={() => void move()}
          >
            Move
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <p>
          <strong>{target.title}</strong> and its subtasks are re-keyed in the project they move to.
          Their old keys still open them.
        </p>
        <FormField label="Move to" htmlFor="task-move-project">
          <Select
            id="task-move-project"
            ariaLabel="Move to"
            searchable={rows.length > 8}
            searchPlaceholder="Find a project"
            placeholder={projects.loading ? "Reading the projects…" : "Choose a project"}
            value={to || undefined}
            options={projectOptions(rows, to)}
            onChange={(next) => {
              write.dismiss();
              setTo(String(next));
            }}
          />
        </FormField>
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/**
 * Deleting one task permanently: what goes, the reason the lead is told, and
 * the key typed as the confirmation. See the file's doc.
 */
function PurgeDialog({
  target,
  onClose,
  onPurged,
}: {
  target: TaskTarget;
  onClose: () => void;
  onPurged: () => void;
}) {
  const gesture = useKeyedGesture();
  const [typed, setTyped] = useState("");
  const [reason, setReason] = useState("");
  const ready = confirmMatches(typed, target.key) && reason.trim() !== "";
  const purge = async () => {
    const answer = await gesture.run(purgeRequest(target, reason));
    if (answer?.kind !== "done") return;
    // THE TRACKER MOVED, from a write no tool made: every question that
    // reads it is asked again at the position the purge landed at, so the
    // trash it was pressed in redraws without it.
    const position = typeof answer.body.position === "string" ? answer.body.position : null;
    session.written("tracker", position, ["work_search"]);
    onPurged();
  };
  const failed = gesture.answer && gesture.answer.kind !== "done" ? gesture.answer : null;
  return (
    <Modal
      open
      onClose={onClose}
      title={`Delete ${target.key} permanently?`}
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="danger"
            loading={gesture.busy}
            disabledReason={
              ready
                ? undefined
                : reason.trim() === ""
                  ? "Say why it is deleted."
                  : `Type ${target.key} to confirm.`
            }
            onClick={() => void purge()}
          >
            {failed?.kind === "unknown" ? "Try again" : "Delete permanently"}
          </Button>
        </>
      }
    >
      <div className="col gap-3">
        <p>
          <strong>{target.title}</strong> is deleted on every node, with its description, comments,
          checklists, history and links. Nothing can restore it.
        </p>
        <p>
          Its subtasks stay, moved onto its parent. Every task that waited on it or linked to it
          stops naming it. Its project’s lead is told, with your reason.
        </p>
        <FormField label="Reason" htmlFor="task-purge-reason" required>
          <Textarea
            id="task-purge-reason"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            rows={2}
          />
        </FormField>
        <ConfirmField phrase={target.key} value={typed} onValueChange={setTyped} autoFocus />
        {failed && (
          <Callout variant={failed.kind === "unknown" ? "warning" : "danger"}>
            {failed.text}
          </Callout>
        )}
      </div>
    </Modal>
  );
}
