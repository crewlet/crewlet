/**
 * The changes a screen offers today, each as one control.
 *
 * Every one of them is a [WriteButton] over `useAct` — drawn for every reader,
 * disabled with the reason where this browser cannot act, and confirmed by
 * the engine before anything on screen moves (`lib/useAct.ts`). They live
 * together because each is offered by more than one screen (a task is
 * restored from its page and from the trash grid; a view is pinned from its
 * page and, soon, from the strip), and a control written twice is two
 * readings of one argument list.
 *
 * `app/writeGate.test.tsx` renders every control here for an anonymous, an
 * unbound and a bound reader, and fails for a module that makes a change
 * without being one of them.
 */

import { useMemo, useState } from "react";
import { Button, FormField, Input, Modal, Select, type SelectOption } from "@crewlethq/ui";
import { UndoGlyph, CheckGlyph, PinGlyph, UserPlusGlyph } from "@crewlethq/icons/glyphs";
import { RefusalNote, WriteButton } from "./WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { handleLabel, indexOrg } from "~/lib/seats.ts";

/**
 * Hand a task to somebody — or to nobody — with a line saying why.
 *
 * CONDITIONAL ON THE VERSION THE PAGE SHOWS (`if_match`): the person chose an
 * assignee looking at this task as it is on screen, and if somebody else moved
 * it in between, the engine refuses rather than overwriting their change —
 * "Changed by somebody else since you opened it". That refusal asks the
 * task's questions again (`protocol/act.ts`), so the page redraws at the
 * version that won and the next press is made against it. The sentence says
 * "somebody else" rather than naming them: the refusal carries the version it
 * lost to, not who wrote it.
 */
export function AssignButton({
  item,
  version,
  assignee,
}: {
  /** The task's key (ENG-42) or id. */
  item: string;
  /** The `task.version` the page is drawn from. */
  version: number;
  /** Who holds it now, or "". */
  assignee: string;
}) {
  const write = useAct("update_work_item");
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        leadingIcon={<UserPlusGlyph />}
        showRefusal={false}
        onPress={() => setOpen(true)}
      >
        {assignee ? "Reassign" : "Assign"}
      </WriteButton>
      {open && (
        <AssignDialog
          item={item}
          version={version}
          assignee={assignee}
          write={write}
          onClose={() => {
            write.dismiss();
            setOpen(false);
          }}
        />
      )}
    </>
  );
}

/** The value the "nobody" option carries: an empty handle is an unassignment. */
const NOBODY = "";

function AssignDialog({
  item,
  version,
  assignee,
  write,
  onClose,
}: {
  item: string;
  version: number;
  assignee: string;
  write: ReturnType<typeof useAct<"update_work_item">>;
  onClose: () => void;
}) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const [who, setWho] = useState(assignee);
  const [reason, setReason] = useState("");
  const options = useMemo<SelectOption[]>(
    () => [
      { value: NOBODY, label: "Nobody", description: "Take it off everybody's queue" },
      ...index.seats.map((seat) => ({
        value: seat.handle,
        label: seat.name,
        description: handleLabel(seat.handle),
        text: `${seat.name} ${seat.handle}`,
        group: seat.kind === "human" ? "People" : "Agents",
      })),
    ],
    [index],
  );
  const name = index.byHandle.get(who)?.name ?? who;
  const unchanged = who === assignee;
  const submit = async () => {
    if (unchanged) return;
    const result = await write.run(
      {
        item,
        assignee: who,
        if_match: version,
        // A REASON ONLY WITH AN ASSIGNEE, which is the tool's own rule: it
        // is what the new holder is woken with, and nobody is woken by an
        // unassignment.
        ...(who !== NOBODY && reason.trim() ? { reason: reason.trim() } : {}),
      },
      { done: who === NOBODY ? `Unassigned ${item}` : `Assigned ${item} to ${name}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Assign ${item}`}
      icon={<UserPlusGlyph />}
      onClose={onClose}
      onSubmit={() => void submit()}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            onPress={() => void submit()}
            blocked={unchanged ? "Choose somebody other than who holds it now." : undefined}
          >
            {who === NOBODY ? "Unassign" : "Assign"}
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <FormField label="Assignee" htmlFor="assign-who">
          <Select
            id="assign-who"
            ariaLabel="Assignee"
            searchable
            searchPlaceholder="Find a seat"
            value={who}
            options={options}
            onChange={(next) => setWho(String(next))}
          />
        </FormField>
        {who !== NOBODY && (
          <FormField
            label="Why it is theirs now"
            optional
            htmlFor="assign-reason"
            helper="The new assignee is woken with this line, and the task's history shows it beside the hand-off."
          >
            <Input
              id="assign-reason"
              value={reason}
              onChange={(event) => setReason(event.target.value)}
            />
          </FormField>
        )}
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/**
 * Bring a task back from the trash, at any age.
 *
 * NAMED FOR ITS TASK ("Restore ENG-42") while it reads "Restore": the trash
 * grid draws one per row, and a screen reader listing a page's buttons would
 * otherwise announce a column of identical controls with nothing to tell
 * which task each brings back. The visible word leads the name, so a person
 * speaking "Restore" to voice control still reaches it.
 */
export function RestoreButton({ item }: { item: string }) {
  const write = useAct("restore_work_item");
  return (
    <WriteButton
      write={write}
      size="small"
      variant="ghost"
      leadingIcon={<UndoGlyph />}
      aria-label={`Restore ${item}`}
      onPress={() => void write.run({ item }, { done: `Restored ${item}` })}
    >
      Restore
    </WriteButton>
  );
}

/**
 * Pin a saved view to your own strip, or take it off. A pin is per person:
 * it moves nobody else's sidebar.
 */
export function PinButton({ view, name, pinned }: { view: string; name: string; pinned: boolean }) {
  const write = useAct("set_pins");
  return (
    <WriteButton
      write={write}
      size="small"
      variant="secondary"
      leadingIcon={<PinGlyph />}
      pressed={pinned}
      onPress={() =>
        void write.run(
          { views: pinned ? { remove: [view] } : { add: [view] } },
          { done: pinned ? `Unpinned ${name}` : `Pinned ${name}` },
        )
      }
    >
      {pinned ? "Unpin" : "Pin"}
    </WriteButton>
  );
}

/**
 * Mark one notice read — done — and nothing else: the engine marks that
 * record alone and leaves every other mark and the read position as they are.
 *
 * A caller that reuses one slot for different notices keys this on the
 * record, so a refusal drawn for one notice is never shown under the next.
 */
export function MarkReadButton({ recordId }: { recordId: string }) {
  const write = useAct("mark_inbox");
  return (
    <WriteButton
      write={write}
      size="small"
      variant="secondary"
      leadingIcon={<CheckGlyph />}
      onPress={() => void write.run({ read: [recordId] }, { done: "Marked read" })}
    >
      Mark read
    </WriteButton>
  );
}
