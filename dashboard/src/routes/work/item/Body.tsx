/**
 * What a task SAYS: its title, its description, its checklists and the tasks
 * under it — and, for a person who can change it, the same text as the thing
 * they edit.
 *
 * # Edited in place, conditional on what was read
 *
 * The title and description are one control each that turns into its own
 * editor, saved through the page's one write ([useItemEdits]) with
 * `if_match: task.version`. There is no per-field merge that makes overwriting
 * prose safe, so a description somebody else changed while this one was open is
 * refused and named rather than silently replaced.
 *
 * # A checklist tick is a gesture
 *
 * `update_work_item{checklist: {op: "set_done"}}`, sent WITHOUT a version: the
 * engine applies it to the checklists as they are when it lands, so a tick
 * never loses to an agent that moved the status meanwhile.
 *
 * # A failed subtree read is not an empty one
 *
 * The detail answer carries links and comments but never the tree below the
 * task, so the sub-task read is the ONLY thing that can see a task's children
 * — and a refusal that rendered as no panel would say "this task has no
 * sub-tasks" about a task that may have twenty. The error is drawn where the
 * rows would have been, and only a read that answered may conclude the task
 * is a leaf.
 */

import { useState, type ReactNode } from "react";
import { Button, IconButton, Input, Textarea } from "@crewlethq/ui";
import { CheckGlyph, PencilGlyph, PlusGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Assignee, StatusMark, type RowChrome } from "~/components/work.tsx";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useAct } from "~/lib/useAct.ts";
import { plural } from "~/lib/format.ts";
import type { SeatRing } from "~/lib/seats.ts";
import type { WorkItem, WorkSummary } from "~/protocol/index.ts";
import { useItemEdits } from "./edit.tsx";

/** The title, drawn as the page's heading and edited in place. */
export function ItemTitle({ item }: { item: WorkItem }) {
  const edits = useItemEdits();
  const [draft, setDraft] = useState<string | null>(null);
  if (draft !== null && edits) {
    const title = draft.trim();
    const save = async () => {
      if (!title || title === item.title) return setDraft(null);
      const result = await edits.edit({ title }, `Renamed ${item.key}`);
      if (result && (result.kind === "applied" || result.kind === "pending")) setDraft(null);
    };
    return (
      <form
        className="task-title-edit"
        onSubmit={(event) => {
          event.preventDefault();
          void save();
        }}
      >
        <Input
          aria-label={`Title of ${item.key}`}
          value={draft}
          autoFocus
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              event.preventDefault();
              setDraft(null);
            }
          }}
        />
        <SaveCancel
          busy={edits.busy}
          blocked={title ? undefined : "A task needs a title."}
          onSave={() => void save()}
          onCancel={() => setDraft(null)}
        />
      </form>
    );
  }
  return (
    <div className="task-title-row">
      <h1 className="task-title">{item.title}</h1>
      {edits?.can && (
        <IconButton
          size="sm"
          variant="ghost"
          className="task-edit"
          label="Edit the title"
          icon={<PencilGlyph size="sm" />}
          onClick={() => setDraft(item.title)}
        />
      )}
    </div>
  );
}

/** The description, as prose — and for a writer, the markdown behind it. */
export function ItemDescription({ item, flush }: { item: WorkItem; flush?: boolean }) {
  const edits = useItemEdits();
  const [draft, setDraft] = useState<string | null>(null);
  if (draft !== null && edits) {
    const save = async () => {
      if (draft === (item.body ?? "")) return setDraft(null);
      const result = await edits.edit({ body: draft }, `Edited the description of ${item.key}`);
      if (result && (result.kind === "applied" || result.kind === "pending")) setDraft(null);
    };
    return (
      <div className="task-body-edit">
        <Textarea
          aria-label={`Description of ${item.key}`}
          value={draft}
          rows={8}
          autoFocus
          onChange={(event) => setDraft(event.target.value)}
        />
        <SaveCancel busy={edits.busy} onSave={() => void save()} onCancel={() => setDraft(null)} />
      </div>
    );
  }
  return (
    <div className="task-body">
      {item.body ? (
        <div className={flush ? "prose md" : "prose md task-prose"}>
          {renderMarkdown(item.body)}
        </div>
      ) : (
        <p className="muted">No description was written.</p>
      )}
      {edits?.can && (
        <Button
          size="small"
          variant="ghost"
          className="task-edit"
          leadingIcon={<PencilGlyph size="sm" />}
          onClick={() => setDraft(item.body ?? "")}
        >
          {item.body ? "Edit description" : "Add a description"}
        </Button>
      )}
    </div>
  );
}

function SaveCancel({
  busy,
  blocked,
  onSave,
  onCancel,
}: {
  busy: boolean;
  blocked?: string;
  onSave: () => void;
  onCancel: () => void;
}) {
  return (
    <div className="row gap-2">
      <Button
        size="small"
        variant="primary"
        loading={busy}
        disabledReason={blocked}
        onClick={onSave}
      >
        Save
      </Button>
      <Button size="small" variant="ghost" onClick={onCancel} disabled={busy}>
        Cancel
      </Button>
    </div>
  );
}

/**
 * Every named checklist on the task — "Done when", "Rollout" — each item a
 * box a writer ticks.
 */
export function Checklists({ item, chrome }: { item: WorkItem; chrome: RowChrome }) {
  const edits = useItemEdits();
  const lists = (item.checklists ?? []).filter((list) => (list.items ?? []).length > 0);
  if (lists.length === 0) return null;
  return (
    <>
      {lists.map((list) => {
        const items = list.items ?? [];
        const done = items.filter((entry) => entry.done).length;
        return (
          <section key={list.id} className="task-checklist" aria-label={list.name}>
            <div className="task-lbl">
              {list.name}
              <span className="task-num">{`${done} / ${items.length}`}</span>
            </div>
            <ul>
              {items.map((entry) => {
                const tick = (
                  <input
                    type="checkbox"
                    className="task-check-box"
                    checked={Boolean(entry.done)}
                    // A READER SEES THE STATE and cannot move it; the page says
                    // why once, above (`ItemEditNote`).
                    disabled={!edits?.can || edits.busy}
                    onChange={(event) =>
                      void edits?.edit(
                        {
                          checklist: { op: "set_done", item: entry.id, done: event.target.checked },
                        },
                        `${event.target.checked ? "Ticked" : "Unticked"} “${entry.name}” on ${item.key}`,
                        false,
                      )
                    }
                  />
                );
                return (
                  <li
                    key={entry.id}
                    className="task-check"
                    data-done={entry.done ? "true" : undefined}
                  >
                    <label title={edits && !edits.can ? edits.reason : undefined}>
                      {tick}
                      <span className="task-check-mark" aria-hidden="true">
                        {entry.done && <CheckGlyph size="xs" />}
                      </span>
                      <span className="task-check-name">{entry.name}</span>
                    </label>
                    {entry.promoted_to && (
                      <a className="t-link" href={href(["work", entry.promoted_to])}>
                        became a sub-task →
                      </a>
                    )}
                    {entry.assignee && (
                      <Assignee
                        handle={entry.assignee}
                        seatName={chrome.seatName}
                        seatKind={chrome.seatKind}
                      />
                    )}
                  </li>
                );
              })}
            </ul>
          </section>
        );
      })}
    </>
  );
}

/**
 * The tasks under this one: how many are done, each row a way to it, and a
 * "+" that files a new one here.
 */
export function Subtasks({
  rows,
  error,
  chrome,
  parent,
  ringOf,
  peek,
}: {
  rows: WorkSummary[];
  error?: string | null;
  chrome: RowChrome;
  /** The task a new sub-task is filed under; absent where none may be added. */
  parent?: WorkItem;
  /** The ring round a holder's badge — their seat's state. */
  ringOf?: (handle: string) => SeatRing | undefined;
  /**
   * Open a child in the rail instead of navigating to it. ABSENT IS A REAL
   * SETTING — the row stays the plain link it already is — and the peek
   * passes nothing: a sub-task swapped into the rail in place of its parent
   * would leave `[` and `]` walking a list this task is not in.
   */
  peek?: (row: WorkSummary) => void;
}) {
  const [adding, setAdding] = useState(false);
  if (error) {
    return (
      <section className="task-subtasks" aria-label="Sub-tasks">
        <div className="task-subtasks-head">
          <span className="task-subtasks-title">Sub-tasks</span>
        </div>
        <QueryState error={error} loading={false} />
      </section>
    );
  }
  if (rows.length === 0 && !adding) {
    return parent ? <AddSubtaskLink onAdd={() => setAdding(true)} /> : null;
  }
  const done = rows.filter((row) => row.status_group === "done" || row.status === "done").length;
  return (
    <section className="task-subtasks" aria-label="Sub-tasks">
      <div className="task-subtasks-head">
        <span className="task-subtasks-title">Sub-tasks</span>
        <span className="task-num">{`${done} / ${rows.length}`}</span>
        {rows.length > 0 && (
          <span
            className="task-meter"
            role="img"
            aria-label={`${done} of ${plural(rows.length, "sub-task")} done`}
          >
            <span style={{ width: `${(done / rows.length) * 100}%` }} />
          </span>
        )}
        <span className="spacer" />
        {parent && (
          <IconButton
            size="sm"
            variant="ghost"
            label="Add a sub-task"
            icon={<PlusGlyph size="sm" />}
            onClick={() => setAdding(true)}
          />
        )}
      </div>
      {rows.map((row) => (
        <a
          key={row.id}
          className="task-subtask"
          href={href(["work", row.key])}
          onClick={
            peek
              ? (event) => {
                  if (event.metaKey || event.ctrlKey || event.shiftKey || event.button !== 0)
                    return;
                  event.preventDefault();
                  peek(row);
                }
              : undefined
          }
        >
          <StatusMark status={row.status} />
          <span className="work-key mono">{row.key}</span>
          <span className="truncate task-subtask-title">{row.title}</span>
          <Assignee
            handle={row.assignee}
            seatName={chrome.seatName}
            seatKind={chrome.seatKind}
            ring={row.assignee ? ringOf?.(row.assignee) : undefined}
          />
        </a>
      ))}
      {adding && parent && <AddSubtask parent={parent} onDone={() => setAdding(false)} />}
    </section>
  );
}

/** The "+ Add a sub-task" line a task with no children offers a writer. */
function AddSubtaskLink({ onAdd }: { onAdd: () => void }): ReactNode {
  const edits = useItemEdits();
  if (!edits?.can) return null;
  return (
    <Button
      size="small"
      variant="ghost"
      className="task-add-subtask"
      leadingIcon={<PlusGlyph size="sm" />}
      onClick={onAdd}
    >
      Add a sub-task
    </Button>
  );
}

/** One line that files a sub-task under this task, in its project. */
function AddSubtask({ parent, onDone }: { parent: WorkItem; onDone: () => void }) {
  const write = useAct("create_work_item");
  const [title, setTitle] = useState("");
  const blocked = title.trim() ? undefined : "Give the sub-task a title.";
  const submit = async () => {
    // THE BUTTON'S OWN GATE, because the line is a form Enter submits: a
    // second Enter while the first answer was out filed the sub-task twice.
    if (!pressable(write, blocked)) return;
    const name = title.trim();
    const result = await write.run(
      { title: name, project: parent.project, parent: parent.id },
      { done: `Filed “${name}” under ${parent.key}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      setTitle("");
      onDone();
    }
  };
  return (
    <form
      className="task-subtask-add"
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <Input
        aria-label={`New sub-task of ${parent.key}`}
        placeholder="What needs doing"
        value={title}
        autoFocus
        onChange={(event) => setTitle(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            event.preventDefault();
            onDone();
          }
        }}
      />
      <WriteButton
        write={write}
        size="small"
        variant="primary"
        showRefusal={false}
        blocked={blocked}
        onPress={() => void submit()}
      >
        Add
      </WriteButton>
      <Button size="small" variant="ghost" onClick={onDone} disabled={write.busy}>
        Cancel
      </Button>
      <RefusalNote write={write} />
    </form>
  );
}
